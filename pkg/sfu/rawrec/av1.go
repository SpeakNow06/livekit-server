package rawrec

import (
	"fmt"
	"os"

	"github.com/pion/rtp/codecs"
)

// ── AV1 (2026-09-14, rawrec39) ──────────────────────────────────────────────
//
// Kullanıcı kararı: "dört codec de aynı şekilde tanınsın". AV1 daha önce
// kamerada denenmiş ve kaydı bozmuştu (kayıt 158: tarayıcı kopyası VP90
// etiketiyle yazıldı, çözülemedi); yazıcı da tanımıyordu.
//
// RTP yük biçimi (AOM "RTP Payload Format For AV1"): 1 baytlık toplama
// başlığı |Z|Y|W W|N|-|-|-| + OBU elemanları (LEB128 uzunluk + OBU). RTP'de
// temporal delimiter TAŞINMAZ ve OBU'ların boyut alanı genelde YOKTUR;
// dosyaya (IVF, "low overhead bitstream") yazarken her OBU'ya boyut alanı
// eklenmeli ve TU'nun başına temporal delimiter konmalı. Parçalanmış OBU'lar
// (Z/Y) paketler arasında birleştirilir. Bu işi pion'un `AV1Depacketizer`ı
// yapıyor (boyut alanlarını ekliyor, Z/Y parçalarını tamponluyor); burada
// yalnız TU sınırı (RTP damgası değişimi) ve TD ekleniyor.
//
// Anahtar kare: toplama başlığında N=1 (yeni kodlanmış video dizisi) ve Z=0;
// LiveKit tamponu bunu `buffer.IsAV1KeyFrame` ile hesaplıyor (`p.anahtar`).
// Kap IVF, fourcc AV01.

type av1Ayiklayici struct {
	dep    codecs.AV1Depacketizer
	sonRTP uint32
	sonVar bool
}

func (a *av1Ayiklayici) Adi() string { return "AV1" }

// av1TD — temporal delimiter OBU: tip 2, boyut alanı var, boyut 0.
var av1TD = []byte{0x12, 0x00}

func (a *av1Ayiklayici) Ayikla(p vpaket) ([]byte, bool, bool, error) {
	// TU sınırı = RTP damgası değişimi (H.264'teki gibi). Yeni TU'da parça
	// tamponu sıfırlanıyor: önceki TU'nun yarım kalmış OBU'su (paket kaybı)
	// yeni TU'ya sızmasın.
	basla := !a.sonVar || p.rtp != a.sonRTP
	if basla {
		a.dep = codecs.AV1Depacketizer{}
		a.sonRTP, a.sonVar = p.rtp, true
	}
	veri, err := a.dep.Unmarshal(p.payload)
	if err != nil {
		return nil, basla, false, err
	}
	if basla {
		out := make([]byte, 0, len(av1TD)+len(veri))
		out = append(out, av1TD...)
		veri = append(out, veri...)
	}
	return veri, basla, p.marker, nil
}

// AnahtarBaslangici — tampon bayrağı zaten Z=0 ve N=1 istiyor
// (`buffer.IsAV1KeyFrame`); Z=0 aynı zamanda TU'nun ilk paketi demek.
func (a *av1Ayiklayici) AnahtarBaslangici(p vpaket) bool {
	return p.anahtar && len(p.payload) > 0 && p.payload[0]&0x80 == 0
}

// Dogrula — birleşmiş anahtar kare TD ile başlar (biz ekledik), ardından
// SEQUENCE_HEADER OBU (tip 1) gelmeli: WebRTC her anahtar kareyle dizi
// başlığını yollar.
func (a *av1Ayiklayici) Dogrula(kare []byte) (bool, string) {
	if len(kare) < 3 {
		return false, "kare çok kısa"
	}
	if kare[0] != av1TD[0] || kare[1] != av1TD[1] {
		return false, fmt.Sprintf("temporal delimiter yok, ilk baytlar % x", kare[:2])
	}
	h := kare[2]
	if h&0x80 != 0 {
		return false, fmt.Sprintf("OBU yasak biti 1 (0x%02x)", h)
	}
	if tip := (h >> 3) & 0x0F; tip != 1 {
		return false, fmt.Sprintf("ilk OBU tipi %d (sequence header = 1 bekleniyordu)", tip)
	}
	return true, ""
}

func (a *av1Ayiklayici) Uzanti() string { return ".ivf" }

func (a *av1Ayiklayici) YeniKap(fh *os.File, en, boy uint16) kapYazici {
	return newIvfYazıcı(fh, en, boy, ivfFourCCAV1)
}
