package rawrec

import (
	"errors"
	"fmt"
	"os"

	"github.com/pion/rtp/codecs"
)

// ── VP9 (Aşama 1, 2026-09-13) ───────────────────────────────────────────────
//
// video.go'daki VP9'a özgü beş noktanın taşındığı yer; mantık birebir aynı
// (bkz. `video_replay_test.go` altın değeri).
//
// VP9 RTP payload descriptor (RFC draft-ietf-payload-vp9) B (kare başlangıcı)
// ve E (kare sonu) bitlerini taşıyor. Kare = B'den E'ye kadar olan paketlerin
// yüklerinin, başlıkları soyulmuş hâlde ardışık birleşimi. Yayıncılar E'yi
// boş bırakabiliyor, o yüzden RTP marker'ı da kare sonu sayılıyor.

type vp9Ayiklayici struct{}

func (vp9Ayiklayici) Adi() string { return "VP9" }

var errVP9BosYuk = errors.New("vp9: başlık var, yük yok")

func (vp9Ayiklayici) Ayikla(p vpaket) ([]byte, bool, bool, error) {
	var h codecs.VP9Packet
	veri, err := h.Unmarshal(p.payload)
	if err != nil {
		return nil, false, false, err
	}
	// Boş yük VP9'da çözülemeyen paket sayılıyor (refactor öncesi davranış:
	// `err != nil || len(veri) == 0` → kare atılır). H.264'te boş parça
	// normal (FU-A biriktirme), o yüzden karar kodekte veriliyor.
	if len(veri) == 0 {
		return nil, false, false, errVP9BosYuk
	}
	return veri, h.B, h.E || p.marker, nil
}

// AnahtarBaslangici — paketin anahtar kare bayrağı (tampon kodek başına
// hesaplıyor, `buffer.IsVP9KeyFrame`) VE VP9 başlığındaki B biti. Kare
// TOPLANMIYOR; hedef katman için ayrı bir toplayıcı tutmaya gerek kalmıyor.
func (vp9Ayiklayici) AnahtarBaslangici(p vpaket) bool {
	if !p.anahtar {
		return false
	}
	var h codecs.VP9Packet
	if _, err := h.Unmarshal(p.payload); err != nil {
		return false
	}
	return h.B
}

// Dogrula — VP9 kare başlığının ilk 2 biti frame_marker = 0b10. Ölçüldü:
// VP9 → 2, AV1 → 0. Yanlışsa dosya çözülemez ve kayıt görüntüsüz çıkar; en
// azından günlükte iz kalsın.
func (vp9Ayiklayici) Dogrula(kare []byte) (bool, string) {
	if len(kare) == 0 {
		return false, "boş kare"
	}
	if (kare[0] >> 6) != 0b10 {
		return false, fmt.Sprintf("frame_marker %d (0b10 bekleniyordu), ilk bayt 0x%02x",
			kare[0]>>6, kare[0])
	}
	return true, ""
}

func (vp9Ayiklayici) Uzanti() string { return ".ivf" }

func (vp9Ayiklayici) YeniKap(fh *os.File, en, boy uint16) kapYazici {
	return newIvfYazıcı(fh, en, boy, ivfFourCCVP9)
}
