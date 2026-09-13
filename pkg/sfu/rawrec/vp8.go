package rawrec

import (
	"errors"
	"fmt"
	"os"

	"github.com/pion/rtp/codecs"
)

// ── VP8 (2026-09-14, rawrec39) ──────────────────────────────────────────────
//
// Kullanıcı kararı: "dört codec de aynı şekilde tanınsın". VP8 bugün hiçbir
// SpeakNow istemcisinin varsayılanı değil ama LiveKit'in sessiz yedek codec'i
// ve Safari gibi istemcilerde ortaya çıkabiliyor; yazıcı tanımayınca kayıt
// tarayıcı yedeğine düşüyor, o kopya da yanlış fourcc'yle (VP90) yazıldığı
// için görüntü ÇÖZÜLEMİYORDU (duman testi 2026-09-14: 0/33 kare).
//
// RTP yük biçimi RFC 7741: 1-4 baytlık payload descriptor (X/N/S/PID +
// isteğe bağlı PictureID/TL0PICIDX/TID/KEYIDX). Kare = S=1,PID=0 ile başlayan
// paketten RTP marker'lı pakete kadar yüklerin birleşimi. Anahtar kare:
// kare etiketinin (payload'ın ilk baytı) P biti 0. Kap IVF, fourcc VP80.

type vp8Ayiklayici struct{}

func (vp8Ayiklayici) Adi() string { return "VP8" }

var errVP8BosYuk = errors.New("vp8: başlık var, yük yok")

func (vp8Ayiklayici) Ayikla(p vpaket) ([]byte, bool, bool, error) {
	var h codecs.VP8Packet
	veri, err := h.Unmarshal(p.payload)
	if err != nil {
		return nil, false, false, err
	}
	if len(veri) == 0 {
		return nil, false, false, errVP8BosYuk
	}
	return veri, h.S == 1 && h.PID == 0, p.marker, nil
}

// AnahtarBaslangici — tamponun bayrağı (LiveKit `buffer.VP8.IsKeyFrame`:
// S=1, PID=0, P biti 0) yeterli; yine de yükten doğrulanıyor ki katman geçişi
// karenin ortasında yapılmasın.
func (vp8Ayiklayici) AnahtarBaslangici(p vpaket) bool {
	if !p.anahtar {
		return false
	}
	var h codecs.VP8Packet
	veri, err := h.Unmarshal(p.payload)
	if err != nil || len(veri) == 0 || h.S != 1 || h.PID != 0 {
		return false
	}
	return veri[0]&0x01 == 0
}

// Dogrula — anahtar karede 3 baytlık kare etiketinden sonra başlangıç kodu
// 9d 01 2a (RFC 6386 §9.1) ve etiketin P biti 0.
func (vp8Ayiklayici) Dogrula(kare []byte) (bool, string) {
	if len(kare) < 6 {
		return false, "kare çok kısa"
	}
	if kare[0]&0x01 != 0 {
		return false, fmt.Sprintf("kare etiketi P biti 1 (ara kare), ilk bayt 0x%02x", kare[0])
	}
	if kare[3] != 0x9d || kare[4] != 0x01 || kare[5] != 0x2a {
		return false, fmt.Sprintf("VP8 başlangıç kodu yok: % x", kare[3:6])
	}
	return true, ""
}

func (vp8Ayiklayici) Uzanti() string { return ".ivf" }

func (vp8Ayiklayici) YeniKap(fh *os.File, en, boy uint16) kapYazici {
	return newIvfYazıcı(fh, en, boy, ivfFourCCVP8)
}
