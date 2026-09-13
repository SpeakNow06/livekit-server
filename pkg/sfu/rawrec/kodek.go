package rawrec

import (
	"os"
	"strings"

	"github.com/livekit/protocol/codecs/mime"
)

// ── KODEK VE KAP ARAYÜZLERİ (Aşama 1, 2026-09-13) ───────────────────────────
//
// NEDEN VAR: görüntü yazıcısı (video.go) 1500 satırın beşinde VP9'a doğrudan
// bağlıydı — kodek kapısı, kare birleştirme (B/E bitleri), anahtar kare
// başlangıcı, "gerçekten VP9 mi" kontrolü ve IVF etiketi — ve altı yerde de
// somut `*ivfYazıcı` tipine. Başka bir kodek (H.264: mobil uygulama bilerek
// H.264 yayınlıyor; VP8/AV1: ileride ayar değişirse) geldiğinde yazıcı
// sessizce çekiliyor ve kayıt tarayıcı yedeğine düşüyordu.
//
// Kuyruk, sıra tamponu, saat/çapa matematiği, katman politikası, referans
// katman kayması, bütünlük kapısı, sağlık raporu ve Redis kontrolü kodekten
// bağımsız; onlara dokunulmadı. Kodeğe özgü olan her şey bu iki arayüzün
// arkasına alındı. Aşama 1'de davranış DEĞİŞMEDİ — ölçüt
// `video_replay_test.go`: aynı paket dizisi refactor öncesiyle byte birebir
// aynı `.ivf`yi üretiyor (sha256 sabit).
//
// Plan: speaknow-server docs/split-recording/SFU-KODEK-BAGIMSIZ-PLANI.md

// kodekAyiklayici — bir RTP yükünden kare parçası çıkaran, kodeğe özgü yüzey.
//
// Kare toplayıcı (video.go `işle`) kodeğin ne olduğunu bilmiyor: paketi
// `Ayikla`ya verir, dönen parçayı `basla`dan `bitir`e kadar biriktirir.
type kodekAyiklayici interface {
	// Adi — günlük satırları için ("VP9", "H264").
	Adi() string
	// Ayikla — paketin yükünden ham kare verisi + kare sınırı bayrakları.
	// `basla`: bu paket bir karenin İLK parçası; `bitir`: SON parçası.
	// Hata → paket çözülemedi; içinde bulunduğu kare bozuk sayılır.
	Ayikla(p vpaket) (veri []byte, basla, bitir bool, err error)
	// AnahtarBaslangici — bu paket bir ANAHTAR karenin İLK parçası mı.
	// Katman geçişi tam bu noktada yapılıyor (bkz. video.go `hedefBelirle`).
	AnahtarBaslangici(p vpaket) bool
	// Dogrula — birleşmiş ANAHTAR karenin ilk baytları bu kodeğe benziyor mu.
	// Yanlışsa açıklama döner; kare YİNE yazılır, yalnız günlükte iz kalır
	// (bugünkü davranış: RED ayıklayıcısının videoya uygulandığı olay böyle
	// yakalanmıştı, `0a767c2`).
	Dogrula(kare []byte) (ok bool, aciklama string)
	// Uzanti — dosya adı uzantısı (".ivf" / ".ts"). postprocess bu uzantıya
	// göre kap seçiyor (`_ham_video_kabi`), o yüzden kodekle birlikte geliyor.
	Uzanti() string
	// YeniKap — bu kodeğin kareleri hangi kaba yazılacak.
	YeniKap(fh *os.File, en, boy uint16) kapYazici
}

// kapYazici — kareleri bir kaba yazar. IVF (VPx/AV1) ve MPEG-TS (H.264)
// ortak yüzeyi. `anahtar` IVF'te kullanılmıyor; TS'te rastgele erişim
// göstergesi ve PCR yerleşimi için gerekiyor.
type kapYazici interface {
	write(kare []byte, pts uint64, anahtar bool)
	finish()
}

// kodekSec — mime → ayıklayıcı. Desteklenmiyorsa nil; çağıran (NewVideoWriter)
// uyarı basıp geri düşüşü Redis'e yazıyor.
func kodekSec(m mime.MimeType) kodekAyiklayici {
	switch m {
	case mime.MimeTypeVP9:
		return vp9Ayiklayici{}
	}
	return nil
}

// desteklenenKodekler — günlük ve geri düşüş kaydı için liste.
func desteklenenKodekler() string {
	return strings.Join([]string{"VP9"}, ", ")
}
