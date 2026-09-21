package rawrec

import (
	"errors"
	"fmt"
	"os"

	"github.com/pion/rtp/codecs"
)

// ── H.264 (Aşama 4, 2026-09-13) ─────────────────────────────────────────────
//
// NEDEN VAR: öğrenci mobil uygulaması kamerayı ve ekran paylaşımını BİLEREK
// H.264 yayınlıyor (`mobil/src/lib/classroom/config.ts:42,104` — telefon
// H.264'ü donanımla kodluyor). Yazıcı yalnız VP9 bildiği için mobil
// öğrencinin her paylaşımı tarayıcı yedeğine düşüyordu (kayıt 876). Karar
// (2026-09-13): mobil aynen kalır, SFU H.264 yazar.
//
// RFC 6184: bir RTP paketi ya tek NAL (tip 1-23), ya STAP-A (24: birkaç
// NAL bir arada — tipik olarak SPS+PPS), ya FU-A (28: bir NAL'ın parçası).
// pion `codecs.H264Packet` üçünü de Annex-B'ye (00 00 00 01 + NAL) çeviriyor;
// FU-A parçalarını KENDİ İÇİNDE biriktiriyor ve ara parçalarda boş veri
// döndürüyor. Bu yüzden:
//   · akış başına TEK örnek tutuluyor (paket başına yeni örnek olsaydı FU-A
//     hiç birleşmezdi),
//   · boş veri + nil hata "biriktiriliyor" demek, bozuk paket değil,
//   · FU-B (29) ve NAL 25-27 desteklenmiyor → hata → kare atılır (sessiz
//     kalmaz: `işle` bütünlük sayacına yazıyor).
//
// KARE SINIRI: VP9'daki B/E bitlerinin karşılığı yok. Bir erişim birimi
// (kare) = aynı RTP damgasını taşıyan paketler; sonuncusu marker'lı.
//   basla = damga bir önceki paketten farklı
//   bitir = RTP marker
// Damga değiştiğinde pion örneği sıfırlanıyor: bir NAL iki kareye yayılamaz,
// kaybolan FU-A sonu yüzünden tamponda kalan çöp yeni kareye bulaşmasın.
//
// ANAHTAR KARE: tampon kodek başına hesaplıyor (`buffer.IsH264KeyFrame`):
// paket SPS (NAL 7) taşıyorsa — tek NAL ya da STAP-A içinde. Tarayıcılar
// her anahtar kareyi SPS+PPS(+IDR) ile başlattığı için bu paket erişim
// biriminin de ilki oluyor; `AnahtarBaslangici` bu yüzden yalnız bayrağa
// bakıyor (hedef katmanın paketleri için durumsuz olması ŞART — o paketler
// bu örneğin damga durumundan geçmiyor).

type h264Ayiklayici struct {
	pkt    codecs.H264Packet
	sonRTP uint32
	sonVar bool
}

var errH264BosParca = errors.New("h264: boş parça")

// errH264BassizParca — karenin İLK paketi FU-A ama S (başlangıç) biti yok:
// NAL'ın başı, yani dilim başlığı kaybolmuş (rawrec53, kayıt 929). pion böyle
// parçaları yine de birleştirip başına ÜRETİLMİŞ bir NAL başlığı koyuyor;
// çıkan "dilim"in başlığı çöp (929'da `first_mb=4, pps_id=3`) ve Chrome'un
// çözücüsü bunu görünce öğeyi KALICI kapatıyor (PIPELINE_ERROR_DECODE).
// Bütünlük kapısı (`kopuk`, video.go) yalnız kare ORTASINDAKİ deliği
// görüyordu; karenin BAŞI yeni damgayla geldiği için "yeni kare" sanılıyor,
// kaybolan baş fark edilmiyordu. Tarayıcı kopyası (Chrome'un kendi
// depaketleyicisi) aynı akışta bu kareyi yazmamıştı.
var errH264BassizParca = errors.New("h264: FU-A başlangıç parçası yok — karenin başı kayıp")

func (h *h264Ayiklayici) Adi() string { return "H264" }

func (h *h264Ayiklayici) Ayikla(p vpaket) ([]byte, bool, bool, error) {
	basla := !h.sonVar || p.rtp != h.sonRTP
	if basla {
		h.pkt = codecs.H264Packet{}
		h.sonRTP, h.sonVar = p.rtp, true
		// FU-A (28) ve S biti (0x80) yok → bkz. errH264BassizParca.
		if len(p.payload) >= 2 && p.payload[0]&0x1f == 28 && p.payload[1]&0x80 == 0 {
			return nil, basla, false, errH264BassizParca
		}
	}
	veri, err := h.pkt.Unmarshal(p.payload)
	if err != nil {
		return nil, basla, false, err
	}
	// Boş veri: FU-A ara parçası, biriktiriliyor — hata DEĞİL. Ama kare
	// tek paketse ve boşsa (marker'lı boş yük) yazacak bir şey yok.
	if len(veri) == 0 && basla && p.marker {
		return nil, basla, true, errH264BosParca
	}
	return veri, basla, p.marker, nil
}

// AnahtarBaslangici — bkz. dosya başlığı "ANAHTAR KARE".
func (h *h264Ayiklayici) AnahtarBaslangici(p vpaket) bool { return p.anahtar }

// Dogrula — Annex-B başlangıç kodu (pion `IsAVC=false` her NAL'ın önüne
// 00 00 00 01 koyuyor). Yoksa bu H.264 değil ya da ayıklayıcı yanlış.
func (h *h264Ayiklayici) Dogrula(kare []byte) (bool, string) {
	if len(kare) >= 4 && kare[0] == 0 && kare[1] == 0 && kare[2] == 0 && kare[3] == 1 {
		return true, ""
	}
	if len(kare) >= 3 && kare[0] == 0 && kare[1] == 0 && kare[2] == 1 {
		return true, ""
	}
	n := 4
	if len(kare) < n {
		n = len(kare)
	}
	return false, fmt.Sprintf("Annex-B başlangıç kodu yok, ilk baytlar % x", kare[:n])
}

func (h *h264Ayiklayici) Uzanti() string { return ".ts" }

func (h *h264Ayiklayici) YeniKap(fh *os.File, en, boy uint16) kapYazici {
	return newTsYazıcı(fh) // TS başlığında boyut yok; SPS taşıyor
}
