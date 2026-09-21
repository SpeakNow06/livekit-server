// Package rawrec — ham medyayı SFU'nun İÇİNDE diske yazar (Aşama 1: SES).
//
// NEDEN
// -----
// Ders kaydı bugün ham medyayı botun tarayıcısında yakalıyor
// (`RTCRtpScriptTransform`). O kanca jitter tamponunun ÖNÜNDE, yani WebRTC'nin
// kendi A/V hizalamasının da öncesinde duruyor: elimize geçen RTP damgasının
// başlangıcı akış başına rastgele ve duvar saatiyle bağı yok. Bu yüzden
// dosyanın ders eksenindeki yeri TAHMİN edilmek zorunda kalıyor ve tahminin
// kalitesi o anki ağa bağlı — ölçüldü (2026-08-30, üç kayıt): paylaşım
// görüntüsünde gecikme yayılımı 148 ms ile 4501 ms arasında oynadı, 4501 ms'lik
// kayıtta paylaşımın sesi görüntüden ~500 ms kaydı.
//
// Burada o sorun YOK: Sender Report aynı süreçte (ham SR kancası, `Writer.SR`),
// yani çapa hesap değil VERİ. Üstelik `ExtPacket` NACK onarımlı ve sıralı
// geliyor, damga da 32 bit sarmadan arınmış.
//
// KAPSAM (Aşama 1)
// ----------------
// YALNIZ SES. Opus'ta bir paket = bir kare olduğu için birleştirme sorunu yok;
// yazıcı bugünkü Python `_OggOpusWriter`'ın birebir karşılığı. Görüntü Aşama 2.
// Plan: monopol/docs/split-recording/SFU-HAM-YAKALAMA-PLANI.md
//
// KONTROL — NEDEN ODA ADI DEĞİL TRACK SID
// ---------------------------------------
// Kanca noktasında (`receiver_base.go` → `forwardRTP`) oda adı YOK; eklemek
// `MediaTrackParams` ve `ReceiverBaseParams`'ı birden değiştirmek demekti.
// Gerek kalmadı: uygulama `track-published` webhook'unda hem odayı hem track
// SID'ini zaten alıyor, dolayısıyla anahtarı SID'le yazabiliyor. Böylece
// LiveKit yapılarında HİÇBİR yeni alan yok.
//
//	anahtar  sn:rawrec:<track_sid>
//	değer    {"recording_id": 165, "dir": "/abs/Recording/videos/2026-08-30"}
//
// YARIŞ: SFU paketleri iletmeye hemen başlıyor, webhook + Redis yazımı birkaç
// ms sonra bitiyor. Bu yüzden yazıcı, anahtar gelene kadar ilk paketleri
// BEKLETİYOR (tarayıcı kancasındaki `HOLD_MAX_FRAMES` ile aynı gerekçe —
// orada da şart olduğu ölçülerek öğrenilmişti). Ses paketi ~90 bayt, saniyede
// 50 tane: 10 saniyelik bekletme ~45 KB, bedeli yok.
//
// MEDYA YOLU KURALI (pazarlıksız)
// -------------------------------
// `Write` ASLA bloklamaz: tamponlu kanala non-blocking yazar, kanal doluysa
// paketi DÜŞÜRÜR. Disk I/O ayrı goroutine'de. Her hata yutulur, ilki bir kez
// loglanır. Kayıt kaybı, dersin kendisinden sonsuz kere ucuzdur.
//
// ORTAM DEĞİŞKENLERİ
//
//	SN_RAWREC=1                ana anahtar (yoksa kod yoluna hiç girilmez)
//	SN_RAWREC_REDIS=localhost:6379
//	SN_RAWREC_REDIS_DB=0
//	SN_RAWREC_REDIS_PASSWORD=
//	SN_RAWREC_WAIT=60          kontrol anahtarı için bekleme (sn)
package rawrec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/redis/go-redis/v9"
)

// İKİ PARÇALI ANAHTAR (2026-09-03, kayıt 177). Tek anahtar `track-published`
// webhook'unda yazılıyordu ve webhook DB'de kayıt satırı arıyordu; öğretmen
// paylaşımı kayıttan 10 sn ÖNCE başlatınca satır yoktu, anahtar hiç yazılmadı
// ve SFU birinci paylaşımı kaçırdı. Artık iki taraf da kendi bildiğini
// yazıyor ve SIRA ÖNEMSİZ:
//
//	sn:rawrec:track:<sid>  {"room": "<oda>"}            ← webhook
//	sn:rawrec:room:<oda>   {"recording_id":…, "dir":…}  ← kayıt mikroservisi
const trackPrefix = "sn:rawrec:track:"
const roomPrefix = "sn:rawrec:room:"

// NTP çağı (1900-01-01) ile Unix çağı (1970-01-01) arasındaki saniye farkı.
const ntpUnixOffset = 2208988800

// Kanal tamponu. Ses saniyede ~50 paket; 2048 tıkanma anında ~40 saniye pay
// bırakıyor. Dolarsa düşürüyoruz — bkz. paket başlığı.
const chanBuf = 2048

// Kontrol anahtarı yoklama aralığı. İlk paketten sonra bu sıklıkta bakılıyor.
// 300 → 200 ms (2026-09-14, rawrec40, kullanıcı isteği): düğme ile yazıcının
// anahtarı görmesi arasındaki gecikme en çok 200 ms; Redis bedeli track başına
// 5 GET/sn. Kayan pencere (`waitFor`, 2 sn) bunun 10 katı.
const lookupEvery = 200 * time.Millisecond

// hedefYokNotuSonra — ilk paketten bu kadar sonra hedef hâlâ yoksa Redis'e
// "hedef-yok" notu düşülür (tanı; hedef sonradan bulunursa silinir).
// YOKLAMA SIKLIĞI DEĞİŞMİYOR (rawrec37): `lookupEvery` track boyunca sürüyor.
// Eskiden burada yoklama 2 sn'ye yavaşlatılıyordu; o zaman tampon penceresi de
// o gecikmeyi karşılayacak kadar büyük tutulmak zorundaydı. Redis bedeli
// track başına ~3 GET/sn — önemsiz.
const hedefYokNotuSonra = 10 * time.Second

var (
	once         sync.Once
	enabled      bool
	rdb          *redis.Client
	waitFor      time.Duration
	yazmaGecikme time.Duration
	// ⚠ errLogged BURADA DEĞİL — yazıcı başına (kayıt 178'de öğrenildi).
	// Paket düzeyinde tekken SÜREÇ BOYUNCA tek satır yazılıyordu: ses
	// yazıcısının "dosya açılamadı" hatası logu tüketti ve VİDEO yazıcısının
	// aynı hatası hiç görünmedi. Tanı bir tur gecikti.
	pinHigh bool

	// anahtarKareAralığı — ham GÖRÜNTÜ dosyasına en fazla bu aralıkla bir
	// anahtar kare düşsün diye yayıncıdan PLI isteniyor. 0 = kapalı.
	//
	// NEDEN VAR (2026-09-03, kayıt 182): canlı yayında anahtar kare gerekmez
	// — yeni abone geldiğinde SFU zaten PLI atıyor, gerisi fark karesiyle
	// yürüyor. Ama KAYIT ARANABİLİR bir dosya ve arama ancak anahtar kareden
	// başlayabiliyor. Ölçüldü: `cam_1.webm` 24,8 sn / 2 anahtar kare (ikisi
	// de t≈0), `share_1.webm` 72,8 sn / 9 anahtar kare — hepsi ilk 21 sn'de,
	// sonra 51 saniye HİÇ. Kullanıcı bildirdi: "seek yapınca görüntü donuk
	// kalıyor / siyah ekran, ses hemen geliyor" (ses Opus: her paket anahtar).
	//
	// ⚠ ÖLÇÜ SANİYE DEĞİL, KARE (2026-09-03 — kullanıcı: "BBB'de seek anlık,
	// bizimki değil; asıl memnun olması gereken öğrenci").
	//
	// Aramanın süresi = son anahtar kareden beri geçen KARE SAYISI × kare
	// başına çözme maliyeti. Saniye bunun kötü bir vekili, çünkü paylaşım
	// akışının iki rejimi var (kayıt 182'de ölçüldü):
	//     durgun ekran  →  1 fps  → 8 sn geri = 8 kare   (zaten anlık)
	//     hareketli     → 14 fps  → 8 sn geri = 112 kare (yavaş olan tek durum)
	// Sabit saniye durgun ekranda boşuna anahtar kare basıyor, hareketlide
	// yetersiz kalıyor — tam ters. Hesaplandı (45 dk, %30 hareketli):
	//
	//     sabit  8 sn : +19,1 MB, en kötü 112 kare (~0,36 sn)
	//     sabit  4 sn : +38,2 MB, en kötü  56 kare (~0,18 sn)
	//     24 kare     : +27   MB, en kötü  28 kare (~0,09 sn)   ← seçilen
	//
	// Üç sınır birlikte çalışıyor:
	//   kfEnÇokKare — ARAMA GECİKMESİ tavanı. Asıl knob.
	//   kfEnAzSn    — MALİYET tavanı. Tam hareketli içerikte (24 fps video
	//                 paylaşımı) kare bütçesi saniyede bir anahtar kare
	//                 isterdi; bu kapı onu 2 sn'ye sınırlıyor (+94 MB tavan).
	//   kfEnÇokSn   — SEYREK KAPSAMA tabanı. Ekran tamamen donduğunda bile
	//                 dosyada ara ara çapa kalsın.
	kfEnÇokKare int
	kfEnAzSn    time.Duration
	kfEnÇokSn   time.Duration
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func initOnce() {
	if os.Getenv("SN_RAWREC") != "1" {
		return
	}
	rdb = redis.NewClient(&redis.Options{
		Addr:     envOr("SN_RAWREC_REDIS", "localhost:6379"),
		DB:       envInt("SN_RAWREC_REDIS_DB", 0),
		Password: os.Getenv("SN_RAWREC_REDIS_PASSWORD"),
	})
	// KAYAN TAMPON PENCERESİ (2026-09-13, kayıt 883 — rawrec36, bkz. tampon.go).
	// Hedef bulunana kadar YALNIZ son bu kadar saniyenin paketleri tutulur;
	// eskisi paket geldikçe atılır. Eskiden 60 sn BİRİKTİRİLİP dolunca HEPSİ
	// atılıyor ve dosya "eksik" damgalanıyordu; kayıt 883'te track'ler
	// kayıttan 2 dk önce yayınlandığı için dört yazıcı da "eksik" dedi,
	// postprocess tarayıcı kopyasına düştü ve ses/görüntü 10 sn kaydı — oysa
	// dosyalar kaydın başından itibaren tamdı.
	//
	// 60'ın gerekçesi de kalmadı: kayıt 169'daki 17 sn'lik "DB satırı geç
	// yazıldı" gecikmesi, oda anahtarının satır oluşur oluşmaz (robot
	// girmeden, `baslangic_ms` ile) yazılmasıyla kapandı.
	//
	// PENCERE NEDEN 2 SN (rawrec37, kullanıcı: "10 saniyelik biriktirmeye
	// neden ihtiyaç kaldı?"): tamponun TEK işi, düğmeye basılmasıyla
	// yazıcının anahtarı GÖRMESİ arasındaki paketleri kaybetmemek. Yoklama
	// 300 ms'de bir ve hiç yavaşlamıyor; 2 sn bunun 6-7 katı. Kesim düğmenin
	// kendisi olduğu için bu pencereden dosyaya kayıt öncesi HİÇBİR ŞEY
	// girmez (ses ve görüntüde aynı kural, bkz. hedef.baslangic). Bellek:
	// ses 2 sn × 50 paket × ~90 B ≈ 9 KB; görüntü ≈ 0,5 MB.
	waitFor = time.Duration(envInt("SN_RAWREC_WAIT", 2)) * time.Second
	// YAZMA GECİKMESİ (Adım 2, saat.go): ses paketleri diske bu kadar
	// geriden yazılır ki susturma sonrası bölümün tabanı ilk Sender Report
	// ile kesinleştikten SONRA yazılsın. Kayıt canlı değil, bedeli yok:
	// 20 sn × 50 paket × ~90 B ≈ 90 KB. 20 sn neden: 911'de unmute sonrası
	// ilk geçerli SR 2,2 / 7,0 / 8,4 / 13,9 sn sonra geldi (Chrome ~5 sn'de
	// bir yolluyor, susturma içindekiler atılıyor, iki aralık geçebiliyor);
	// 10 sn birini kaçırıyordu. Geç kalırsa da kayıt bozulmaz: kesin değer
	// yan JSON'a düşer, postprocess uygular (`_bolum_kaydirmalari`).
	yazmaGecikme = time.Duration(envInt("SN_RAWREC_YAZMA_GECIKME", 20)) * time.Second
	pinHigh = envOr("SN_RAWREC_PIN_HIGH", "1") != "0"
	kfEnÇokKare = envInt("SN_RAWREC_KEYFRAME_MAX_KARE", 24)
	kfEnAzSn = time.Duration(envInt("SN_RAWREC_KEYFRAME_MIN_SN", 2)) * time.Second
	kfEnÇokSn = time.Duration(envInt("SN_RAWREC_KEYFRAME_MAX_SN", 30)) * time.Second
	enabled = true
	logger.Infow("rawrec açık", "redis", envOr("SN_RAWREC_REDIS", "localhost:6379"),
		"tampon_penceresi", waitFor, "üst_katman_sabitle", pinHigh,
		"anahtar_kare_en_çok_kare", kfEnÇokKare,
		"anahtar_kare_en_az", kfEnAzSn, "anahtar_kare_en_çok", kfEnÇokSn)
}

// Enabled — ham yazım açık mı. Kanca bunu çağırıp erken dönüyor.
func Enabled() bool {
	once.Do(initOnce)
	return enabled
}

// SanalAboneID — dynacast'e "bu track'i ben de izliyorum" diyen sahte abone.
// Gerçek bir katılımcı değil; yalnız `maxSubscriberQuality` haritasında bir
// satır. Kimseye paket gitmiyor.
const SanalAboneID livekit.ParticipantID = "PA_SN_RAWREC"

// PinHigh — RAWREC yazarken üst simulcast katmanı UYANIK TUTULSUN mu.
//
// NEDEN: dynacast izlenmeyen katmanın encoder'ını durduruyor; üst katman
// ancak bir abone HIGH isteyince uyanıyor. Bot bunu istiyor ama abone olup
// isteği iletene kadar zaman geçiyor ve o sürede üst katman HİÇ GÖNDERİLMİYOR
// — SFU de göndermediği kareyi yazamıyor. Ölçüldü:
//
//	kayıt 114  ilk 20,4 sn  960×539  (sonra 1920×1078)
//	kayıt 176  ilk  8,3 sn  üst katmanda tek kare (donuk), sonra normal
//
// Çözüm: kayıt sunucuda başladığına göre isteği de sunucu yapsın. Track
// alıcısı kurulur kurulmaz sahte bir abone HIGH istiyor; bota abone olup
// istemesini beklemeye gerek kalmıyor.
//
// KAPATMA: SN_RAWREC_PIN_HIGH=0. Bedeli, kayıt olmayan derste de üst katmanın
// açık kalması (yayıncı boşuna encode eder).
func PinHigh() bool {
	once.Do(initOnce)
	return enabled && pinHigh
}

// hedef — oda anahtarının içeriği (kayıt mikroservisi yazıyor).
type hedef struct {
	RecordingID int    `json:"recording_id"`
	Dir         string `json:"dir"`
	// Kaydın başlangıç anı (unix ms) — kayıt servisi yazıyor (2026-09-13,
	// kayıt 882). Yoksa/0 ise eski davranış: tampondaki her şey dosyaya girer.
	BaslangicMs int64 `json:"baslangic_ms,omitempty"`
	// Identity — track'in sahibi (webhook `sn:rawrec:track:<sid>` değerinden,
	// Adım 4). Yan JSON'a `participant` olarak gidiyor: postprocess kamera ↔
	// mikrofon eşlemesini klasör sırasına değil KİŞİYE göre yapsın.
	Identity string `json:"-"`
}

// baslangic — kayıt düğmesine basıldığı an (anahtar taşıyorsa), yoksa sıfır
// zaman (= kesme yok, eski anahtar). Bundan ESKİ hiçbir paket dosyaya girmez;
// ses de görüntü de buradan başlar.
//
// NEDEN (kayıt 882): yazıcı hedefi beklerken biriktirdiği HER paketi dosyaya
// yazıyordu. Paylaşım kayıttan 57 sn önce açılınca dosyanın başına 57 sn'lik,
// anahtar karesiz tek bir parça (10,8 MB) oturdu; kaydın ilk saniyesi o parçanın
// sonuna düştüğü için tarayıcı başa her dönüşte 10,8 MB indirip 550 kare çözdü
// (68 aralık isteği, hızlı hatta 18 sn, kullanıcıda ~60 sn).
//
// ÖN PAY YOK (rawrec37; rawrec35-36'da 2 sn vardı, `SN_RAWREC_ONROL_SN`).
// Kullanıcı: "ses için 2 saniye ön payı neden tutalım ki?" — gerekçesi
// yoktu: anahtar düğmeyle aynı anda, aynı makinede yazılıyor; düğme ile
// yazıcının anahtarı görmesi arasındaki paketleri zaten kayan tampon tutuyor
// (`waitFor`). Kayıt düğmenin anından başlar, bu kadar.
func (h *hedef) baslangic() time.Time {
	if h == nil || h.BaslangicMs <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(h.BaslangicMs)
}

// trackEşleme — track anahtarının içeriği (webhook yazıyor).
type trackEşleme struct {
	Room     string `json:"room"`
	Identity string `json:"identity,omitempty"` // yayıncının kimliği (Adım 4)
}

// paket — kanaldan geçen iş birimi. `ExtPacket`'ı TUTMUYORUZ: dayanağı
// forwardRTP'nin yeniden kullandığı tampon (`pktBuf`), kopyalamak ŞART.
type paket struct {
	payload []byte
	rtp     uint32
	örnek   uint32 // 48 kHz örnek sayısı (granül ilerlemesi)
	geliş   time.Time
	yedek   bool   // RED yedek bloğu mu (delik doldurmak için, yoksa atılır)
	seq     uint16 // RTP sıra numarası (kayıp ölçümü; yedekte anlamsız)
	// yakalamaNs — paketin ilk örneğinin YAKALANMA anı (abs-capture-time
	// RTP başlık uzantısı, yayıncının saati, unix ns); 0 = uzantı yok.
	// Varsa eşleyici bölüm kararını ağ titremesiz, sinyalsiz ve kesin verir
	// (saat.go "0. YOL").
	yakalamaNs int64
	// sr — Sender Report öğesi (rawrec45): payload boş, yalnız bu dolu.
	// Kanala paketlerle aynı sıradan girer, döngü eşleyiciye verir.
	sr *livekit.RTCPSenderReportState
}

// Writer — bir ses track'i için ham Ogg/Opus yazıcısı.
//
// Ömrü `forwardRTP` çağrısı kadar: orada kurulur, `defer Close()` ile kapanır.
// `Write` forwardRTP goroutine'inden, gerisi kendi goroutine'inden çalışır.
type Writer struct {
	trackID   livekit.TrackID
	sid       string // LiveKit track SID'i (TR_…), tarayıcı UUID'si DEĞİL
	isRED     bool
	trackInfo *livekit.TrackInfo
	clockRate uint32
	buff      srKaynak
	log       logger.Logger

	ch     chan paket
	done   chan struct{}
	closed atomic.Bool

	düşen     atomic.Uint64
	errLogged atomic.Bool

	// islenen — döngünün kanaldan aldığı paket sayısı (tanı + testlerin
	// deterministik beslemesi: test, paketin işlendiğini bununla bekliyor).
	islenen atomic.Uint64
	// muteSayac — yayıncı mute sinyali kaç kez geldi (PubMute). Döngü
	// değişimi görünce eşleyiciye "mute bekliyor" der (saat.go: eşik 0,2 sn).
	muteSayac atomic.Int64
	// kapatKilit — `SR` başka goroutine'den (RTCP okuyucu) geliyor; kanal
	// kapanırken gönderim olmasın diye Close yazma, SR okuma kilidi alır.
	kapatKilit sync.RWMutex
}

// PubMute — yayıncı track'i susturdu/açtı (ReceiverBase.UpdateTrackInfo →
// trackInfo.Muted). Yalnız SUSTURMA sayılıyor: eşleyici bir sonraki bölüm
// açılışında sıkı eşiğe geçer. Medya yolundan çağrılabilir, bloklamaz.
func (w *Writer) PubMute(muted bool) {
	if w == nil || !muted {
		return
	}
	w.muteSayac.Add(1)
}

// SR — HAM Sender Report (rawrec45). LiveKit'in istatistik katmanından
// (rtpstats) DEĞİL, telden geldiği gibi: o katman susturma sırasında
// Chrome'un RTP'si tahmini SR'larını kabul edip açılıştan sonraki gerçek
// SR'ları "sırasız" diye atıyordu (kayıt 915: 157 sn susturma → 6 SR düştü,
// bölüm hiç kesinleşmedi). Eleme burada: eşleyici SR'ı VARIŞ anına göre
// bir bölümün penceresine oturtuyor, susturma içindekiler açıkta kalıyor
// (saat.go bolumZamanla). Kanala paketlerle aynı sıradan girer; RTCP
// okuyucu goroutine'inden çağrılır, ASLA BLOKLAMAZ.
func (w *Writer) SR(sr *livekit.RTCPSenderReportState) {
	if w == nil || sr == nil || sr.NtpTimestamp == 0 {
		return
	}
	w.kapatKilit.RLock()
	defer w.kapatKilit.RUnlock()
	if w.closed.Load() {
		return
	}
	select {
	case w.ch <- paket{sr: sr}:
	default:
		if n := w.düşen.Add(1); n == 1 || n%1000 == 0 {
			w.log.Warnw("rawrec sırası dolu, SR düşürüldü", nil, "toplam", n)
		}
	}
}

// srKaynak — yalnız ihtiyacımız olan kısım. Arayüzü dar tutuyoruz ki test
// edilebilsin ve buffer paketine bağımlılık minimum kalsın.
type srKaynak interface {
	GetSenderReportData() *livekit.RTCPSenderReportState
	// SendPLI — anahtar kare iste. Görüntü yazıcısı dosyayı ancak anahtar
	// kareyle açabiliyor ve ekran paylaşımında anahtar kare ÇOK seyrek
	// (sabit ekranda dakikalarca gelmeyebilir).
	SendPLI(force bool)
}

// NewWriter — yazıcıyı kurar. Kapalıysa ya da track ses değilse nil döner;
// çağıran nil kontrolü yapıp hiçbir şey yapmamalı (fail-open).
func NewWriter(
	trackID livekit.TrackID,
	trackInfo *livekit.TrackInfo,
	clockRate uint32,
	isRED bool,
	buff srKaynak,
	log logger.Logger,
) *Writer {
	if !Enabled() {
		return nil
	}
	// ⚠ SESSİZ NİL YOK. İlk sürüm `trackInfo == nil || clockRate == 0` deyip
	// sessizce nil dönüyordu; yazıcı hiç kurulmadı ve GÜNLÜKTE İZ KALMADI
	// (kayıt 168'de tam bir saat bunu aradık). Artık eksik alan varsa
	// varsayılana düşüyoruz ve her hâlükârda kuruluyoruz.
	if clockRate == 0 {
		clockRate = 48000 // ses için tek makul değer
	}
	// ⚠ ANAHTAR `trackInfo.Sid` — `params.TrackID` DEĞİL.
	// Ölçüldü (kayıt 169): `ReceiverBaseParams.TrackID` tarayıcının
	// MediaStreamTrack UUID'sini taşıyor (b9049109-…), LiveKit SID'ini
	// (TR_AMPAvFsdbHKHLd) değil. Webhook SID'le yazıyor, biz UUID'yle
	// arıyorduk → hiç bulunamadı.
	sid := string(trackID)
	if trackInfo != nil && trackInfo.Sid != "" {
		sid = trackInfo.Sid
	}
	w := &Writer{
		trackID:   trackID,
		sid:       sid,
		isRED:     isRED,
		trackInfo: trackInfo,
		clockRate: clockRate,
		buff:      buff,
		log:       log,
		ch:        make(chan paket, chanBuf),
		done:      make(chan struct{}),
	}
	go w.loop()
	log.Infow("rawrec yazıcı kuruldu", "sid", w.sid,
		"kaynak", kaynakAdı(trackInfo), "hz", clockRate, "red", isRED)
	return w
}

// kaynakAdı — trackInfo nil olabilir; klasör seçimi ona bakıyor.
func kaynakAdı(ti *livekit.TrackInfo) string {
	if ti == nil {
		return "BİLİNMİYOR"
	}
	return ti.Source.String()
}

// Write — paketi sıraya koyar. ASLA BLOKLAMAZ.
//
// `gelişNs`: paketin tampona VARIŞ anı (`extPkt.Arrival`, unix ns). Eşleyici
// (saat.go) durmuş saati bununla ölçüyor; SFU kuyruğunun gecikmesi hesaba
// karışmasın diye `time.Now()` değil. 0 verilirse şimdi.
//
// `yakalamaNs`: abs-capture-time uzantısından yakalanma anı (unix ns), yoksa 0.
func (w *Writer) Write(payload []byte, rtpTS uint32, örnek uint32, seq uint16, gelişNs, yakalamaNs int64) {
	if w == nil || w.closed.Load() || len(payload) == 0 {
		return
	}
	an := time.Now()
	if gelişNs > 0 {
		an = time.Unix(0, gelişNs)
	}
	// RED SARMALINI AÇ (RFC 2198). Mikrofon `audio/red` ile yayınlanıyor
	// (ölçüldü, kayıt 169: mime=audio/red) — yani yük saf Opus DEĞİL, yedekli
	// bir kap. Ogg/Opus'a olduğu gibi yazılsaydı dosya çözülemezdi.
	//
	// YEDEKLER DE KUYRUĞA GİRİYOR (2026-09-03, kayıt 183 — bkz. redBloklar).
	// Döngü onları yalnız DELİK varsa yazıyor; delik yoksa atıyor.
	if w.isRED {
		bloklar := redBloklar(payload)
		if len(bloklar) == 0 {
			return
		}
		for _, b := range bloklar {
			// Yakalanma anı yalnız BİRİNCİL bloğa: yedek blok bir önceki
			// paketin sesi, taşıyıcının saati ona ait değil.
			// ⚠ rawrec43-46 burada İKİSİNE DE 0 veriyordu → mikrofon (hep RED)
			// için yakalama saati eşleyiciye HİÇ ulaşmadı: 914-916'da
			// act_oran 0 çıktı, Chrome aslında her pakete yazıyordu (rawrec46
			// tanısı: 1. pakette id 5, ~1/sn). Kayıt 47'de düzeltildi.
			yak := yakalamaNs
			if b.ofset != 0 {
				yak = 0
			}
			w.kuyruğaKoy(b.veri, rtpTS-b.ofset, örnek, an, b.ofset != 0, seq, yak)
		}
		return
	}
	w.kuyruğaKoy(payload, rtpTS, örnek, an, false, seq, yakalamaNs)
}

// kuyruğaKoy — tek bir ses karesini sıraya koyar. ASLA BLOKLAMAZ.
func (w *Writer) kuyruğaKoy(payload []byte, rtpTS, örnek uint32,
	an time.Time, yedek bool, seq uint16, yakalamaNs int64) {
	if len(payload) == 0 {
		return
	}
	// KOPYA ŞART: forwardRTP `pktBuf`ı her pakette yeniden kullanıyor.
	cp := make([]byte, len(payload))
	copy(cp, payload)
	select {
	case w.ch <- paket{payload: cp, rtp: rtpTS, örnek: örnek, geliş: an,
		yedek: yedek, seq: seq, yakalamaNs: yakalamaNs}:
	default:
		if n := w.düşen.Add(1); n == 1 || n%1000 == 0 {
			w.log.Warnw("rawrec sırası dolu, paket düşürüldü", nil, "toplam", n)
		}
	}
}

// Close — sırayı kapatır ve yazıcının bitmesini bekler (dosya + yan JSON
// kapansın diye). forwardRTP'nin `defer`inden çağrılıyor, oradaki gecikme
// medya yolunu etkilemiyor: akış zaten bitmiş oluyor.
func (w *Writer) Close() {
	if w == nil || !w.closed.CompareAndSwap(false, true) {
		return
	}
	w.kapatKilit.Lock()
	close(w.ch)
	w.kapatKilit.Unlock()
	<-w.done
}

// klasörAdı — bugünkü tarayıcı yolunun ürettiği isimlendirmenin AYNISI.
// postprocess bu isimlere göre arama yapıyor; değiştirmek kaydı bozar.
func klasörAdı(ti *livekit.TrackInfo, recID int) string {
	if ti != nil && ti.Source == livekit.TrackSource_SCREEN_SHARE_AUDIO {
		return fmt.Sprintf("shareaudio_%d", recID)
	}
	return fmt.Sprintf("audioraw_%d", recID)
}

func (w *Writer) loop() {
	defer close(w.done)

	var (
		h          *hedef
		bekleyen   []paket
		ogg        *oggYazıcı
		fh         *os.File
		yol        string
		ilkRTP     uint32
		ilkAn      time.Time // dosyadaki İLK paketin anı → yan JSON
		ilkPaketAn time.Time // track'in ilk paketi → bekleme zaman aşımı
		sonRel     uint32
		kare       int
		yazılan    uint64
		vazgeç     bool // yalnız ONARILAMAZ hata (dosya açılamadı) için
		// Hedef öncesi kayan tamponun izi (bkz. tampon.go): kaç paket
		// pencereden taştı, en yenisi ne zaman geldi.
		iz              tamponIzi
		pencereUyarildi bool // `waitFor` doldu: yoklama seyreltildi, Redis'e "hedef-yok" düştü

		// ── SR AKIŞLA GELİYOR ─────────────────────────────────────────────
		// İlk sürüm `GetSenderReportData()`yı dosya KAPANIRKEN okuyordu ve
		// 74,8 DAKİKA BAYAT veri alıyordu (kayıt 172); sonra her 10 pakette
		// bir yokluyordu. rawrec45'ten beri SR'lar HAM olarak kanaldan
		// geliyor (`Writer.SR`), paketlerle aynı sırada — yoklama yok, LiveKit
		// süzgeci yok (kayıt 915 gerekçesi `SR` başlığında). Dosya açılmadan
		// gelenler bekletilir, açılışta eşleyiciye verilir.
		sonSR      *livekit.RTCPSenderReportState
		bekleyenSR []*livekit.RTCPSenderReportState
		// SR günlüğü artık EŞLEYİCİDE (`saat.gunluk`, pts'li).

		// SAĞLIK: yazıcının kendi durumu (bkz. saglik.go). Postprocess
		// artık YALNIZ buna bakıyor, tarayıcı kopyasıyla karşılaştırma yok.
		sagl          = yeniSaglik()
		redKurtarilan uint64 // RED yedeğinden kurtarılan (delik dolduran) kare

		// ── SAAT EŞLEYİCİ + YAZMA GECİKMESİ KUYRUĞU (Adım 2, saat.go) ──
		// Paketin dosya içindeki yeri (pts) eşleyiciden; dosyaya
		// `yazmaGecikme` geriden yazılıyor ki bölüm tabanı SR ile
		// kesinleşebilsin. Kuyruk akış saatiyle (son varış) boşalıyor,
		// kapanışta tamamen.
		saat        = yeniSesSaat(w.clockRate)
		kuyruk      []kuyrukOgesi
		gorulenMute int64
	)

	// yazPaket — kuyruktan çıkan paketi dosyaya yaz. Granül = pts + örnek
	// (paketin BİTİŞİ). RED yedeği yalnız delik doldurur; eş damga ileri
	// itilir (eski davranış, yalnız kaynağı RTP yerine pts).
	yazPaket := func(e kuyrukOgesi) {
		if ogg == nil {
			return
		}
		g := uint64(e.pts) + uint64(e.p.örnek)
		if e.p.yedek {
			if g <= yazılan {
				return
			}
			redKurtarilan++
		}
		if g <= yazılan {
			yazılan += uint64(e.p.örnek)
		} else {
			yazılan = g
		}
		ogg.write(e.p.payload, yazılan)
		sonRel = uint32(e.pts)
		kare++
		sagl.kareYazildi(e.p.geliş)
		if e.bolum != nil {
			e.bolum.yazildi = true
		}
	}
	// bosalt — akış saatine göre gecikmesi dolan paketleri yaz.
	bosalt := func(hepsi bool) {
		n := 0
		for n < len(kuyruk) {
			if !hepsi && saat.sonGelis.Sub(kuyruk[n].p.geliş) < yazmaGecikme {
				break
			}
			yazPaket(kuyruk[n])
			n++
		}
		if n > 0 {
			kuyruk = append(kuyruk[:0], kuyruk[n:]...)
		}
	}
	// kuyrugaAl — paketi eşleyiciden geçirip kuyruğa koy.
	kuyrugaAl := func(p paket) {
		if n := w.muteSayac.Load(); n != gorulenMute {
			gorulenMute = n
			saat.muteSinyali()
		}
		pts, yeni, kb, kayma := saat.yerlestir(p.rtp, p.geliş, p.seq, p.yedek, p.yakalamaNs)
		if kb != nil && kayma != 0 {
			// Yakalama saati geçici bölümü kesinleştirdi (SR'daki gibi):
			// kuyruktaki o bölümün paketleri kayar.
			for i := range kuyruk {
				if kuyruk[i].bolum != nil && kuyruk[i].bolum.Sira >= kb.Sira {
					kuyruk[i].pts += kayma
				}
			}
			w.log.Infow("rawrec ses: bölüm tabanı yakalama saatiyle kesinleşti",
				"track", w.trackID, "sid", w.sid, "bolum", kb.Sira,
				"kayma_ms", yuvarla(saat.ornekSn(kayma)*1000),
				"varis_eksik_sn", yuvarla(kb.VarisEksikSn), "sr_eksik_sn", yuvarla(*kb.SrEksikSn))
		}
		if p.yedek {
			// Kopya mı delik mi: atanmış en büyük pts'nin gerisindeyse ya da
			// eşitse zaten var (kuyrukta ya da dosyada) → at.
			if pts <= saat.enSonPts {
				return
			}
			saat.enSonPts = pts
		}
		if yeni != nil {
			w.log.Infow("rawrec ses: BÖLÜM TABANI (durmuş saat)",
				"track", w.trackID, "sid", w.sid, "bolum", yeni.Sira,
				"eksik_sn", yuvarla(yeni.VarisEksikSn), "kaynak", yeni.Kaynak,
				"pts0_sn", yuvarla(saat.ornekSn(yeni.Pts0)))
		}
		kuyruk = append(kuyruk, kuyrukOgesi{p: p, pts: pts, bolum: saat.simdiki()})
	}
	// srIsle — Sender Report eşleyiciye; bir bölümün tabanı kesinleştiyse
	// kuyruktaki o ve sonraki bölümlerin paketleri kayar.
	srIsle := func(sr *livekit.RTCPSenderReportState) {
		b, kayma := saat.srGeldi(sr)
		if kayma == 0 {
			return
		}
		for i := range kuyruk {
			if kuyruk[i].bolum != nil && kuyruk[i].bolum.Sira >= b.Sira {
				kuyruk[i].pts += kayma
			}
		}
		w.log.Infow("rawrec ses: bölüm tabanı SR ile kesinleşti",
			"track", w.trackID, "sid", w.sid, "bolum", b.Sira,
			"kayma_ms", yuvarla(saat.ornekSn(kayma)*1000),
			"varis_eksik_sn", yuvarla(b.VarisEksikSn), "sr_eksik_sn", yuvarla(*b.SrEksikSn))
	}

	kapat := func() {
		if ogg != nil {
			bosalt(true)
			ogg.finish()
		}
		if fh != nil {
			fh.Close()
		}
		if yol != "" {
			w.yanJSON(yol, ilkAn, kare, ilkRTP, sonRel, sonSR, sagl, redKurtarilan, saat, h)
		}
	}
	defer kapat()

	// ⚠ HEDEF ARAMASI ZAMANLAYICIDA, PAKETTE DEĞİL (2026-09-03, kayıt 179).
	// Eski sürüm yalnız paket geldiğinde yokluyordu. Ses için sorun değildi
	// (saniyede 50 paket) ama GÖRÜNTÜDE akış durabiliyor: ekran sabitken VP9
	// hiç kare göndermiyor, döngü `range w.ch` üstünde bloke kalıyor ve
	// arama durmuş oluyor. Ölçüldü: anahtar 10:55:47.9'da hazırdı, ses
	// yazıcısı 34 ms'de buldu, GÖRÜNTÜ 13,3 SANİYE sonra bulabildi.
	tik := time.NewTicker(lookupEvery)
	defer tik.Stop()

	for {
		select {
		case <-tik.C:
			if h != nil || vazgeç {
				continue
			}
			if ilkPaketAn.IsZero() {
				continue // henüz tek paket bile gelmedi
			}
			h = w.hedefAra()
			if h == nil {
				// ⚠ KALICI VAZGEÇME YOK (2026-09-03, kayıt 177): eski sürüm
				// `waitFor` dolunca bir daha bakmıyordu, o derste anahtar 77
				// saniye sonra yazıldı ve birinci paylaşım hiç kaydedilmedi.
				// Arama track boyunca sürüyor.
				// ⚠ TAMPON DA BIRAKILMIYOR (2026-09-13, kayıt 883 — rawrec36):
				// tampon zaten kayan pencere (bkz. tampon.go); yoklama hiç
				// yavaşlamıyor (rawrec37). `hedefYokNotuSonra` dolunca yalnız
				// Redis'e "hedef-yok" notu düşüyor (hedef sonradan bulunursa
				// siliniyor). Eskiden burada tampon atılıp dosya "eksik"
				// damgalanıyordu — gerekçe `waitFor` başlığında.
				if !pencereUyarildi && time.Since(ilkPaketAn) > hedefYokNotuSonra {
					pencereUyarildi = true
					w.log.Infow("rawrec hedef henüz yok, kayan tampon sürüyor",
						"track", w.trackID, "sid", w.sid,
						"pencere", waitFor, "bekleyen_paket", len(bekleyen),
						"kaynak", kaynakAdı(w.trackInfo))
					geriDususYaz(w.sid, geriDusus{Neden: "hedef-yok",
						Kaynak: kaynakAdı(w.trackInfo), Track: string(w.trackID),
						Ayrinti: fmt.Sprintf("%s içinde kayıt anahtarı bulunamadı, "+
							"arama sürüyor (son %s tamponda)", hedefYokNotuSonra, waitFor)}, w.log)
				}
				continue
			}
			w.log.Infow("rawrec hedef bulundu", "track", w.trackID,
				"kayit", h.RecordingID, "dizin", h.Dir,
				"gecikme", time.Since(ilkPaketAn).Round(time.Millisecond),
				"bekleyen_paket", len(bekleyen), "pencereden_atilan", iz.atilan)
			if pencereUyarildi {
				geriDususSil(w.sid, w.log) // "hedef-yok" notu artık yanlış
			}
			tik.Stop()

		case p, ok := <-w.ch:
			if !ok {
				return // kanal kapandı → defer kapat() dosyayı bitirir
			}
			if p.sr != nil {
				// HAM SR öğesi (rawrec45). `islenen`e SAYILMAZ: o sayaç
				// paketleri izliyor (testlerin beslemesi ona bakıyor).
				if vazgeç {
					continue
				}
				sonSR = p.sr
				if fh == nil {
					// Dosya (ve eşleyici) yok: beklet, açılışta ver. Kayan
					// tampon en çok `waitFor` tutuyor; SR ~5 sn'de bir → 16 yeter.
					bekleyenSR = append(bekleyenSR, p.sr)
					if len(bekleyenSR) > 16 {
						bekleyenSR = bekleyenSR[1:]
					}
				} else {
					srIsle(p.sr)
				}
				continue
			}
			w.islenen.Add(1)
			if vazgeç {
				continue
			}
			// Kayıp ölçümü ASIL paketler üstünden (yedek blokların sıra
			// numarası taşıyıcı paketinki, kendilerinin değil).
			if !p.yedek {
				sagl.paketGeldi(p.seq, p.geliş)
			}

			// ── 1. Hedef henüz bilinmiyor: beklet ───────────────────────
			if h == nil {
				if ilkPaketAn.IsZero() {
					ilkPaketAn = p.geliş
					w.log.Infow("rawrec ilk paket geldi, hedef aranıyor",
						"track", w.trackID, "sid", w.sid)
				}
				// KAYAN PENCERE: yalnız son `waitFor` tutulur (tampon.go).
				bekleyen = pencereKirp(append(bekleyen, p), paketAn, p.geliş,
					waitFor, &iz)
				continue
			}

			// ── 2. Hedef var ama dosya henüz açılmadı ───────────────────
			if fh == nil {
				// Elde en çok son `waitFor`ın paketleri var (kayan tampon);
				// dosya kesimden sonraki ilk paketten başlıyor, çapa da ona
				// göre kuruluyor.
				bekleyen = append(bekleyen, p)
				// KAYIT ÖNCESİNİ ATMA (bkz. hedef.baslangic): düğmeden önceki
				// paketler dosyaya girmez. Sağlık tabanı da kesime çekilir ki
				// "akış ömrünün yarısından azı yazıldı" kararı bozulmasın.
				if kesim := h.baslangic(); !kesim.IsZero() {
					tutulan := bekleyen[:0:0]
					for _, b := range bekleyen {
						if !b.geliş.Before(kesim) {
							tutulan = append(tutulan, b)
						}
					}
					if atilan := len(bekleyen) - len(tutulan); atilan > 0 {
						w.log.Infow("rawrec kayıt öncesi paketler atıldı",
							"track", w.trackID, "atilan", atilan, "tutulan", len(tutulan),
							"kesim", kesim.Format(time.RFC3339Nano),
							"kaynak", kaynakAdı(w.trackInfo))
					}
					if len(tutulan) == 0 {
						tutulan = append(tutulan, p) // en yeni paket her zaman kalır
					}
					bekleyen = tutulan
					sagl.kesimUygulandi(kesim)
				}
				// Pencereden taşan paket KAYDA ait miydi? (bkz. tampon.go)
				sagl.kayanTamponSonucu(waitFor, iz, h.baslangic())
				ilkRTP = bekleyen[0].rtp
				ilkAn = bekleyen[0].geliş
				var err error
				fh, yol, err = w.dosyaAç(h)
				if err != nil {
					w.birKezLogla("rawrec dosya açılamadı", err)
					vazgeç = true
					bekleyen = nil
					geriDususYaz(w.sid, geriDusus{Neden: "dosya-acilamadi",
						Kaynak: kaynakAdı(w.trackInfo), Track: string(w.trackID),
						Ayrinti: err.Error()}, w.log)
					continue
				}
				kanal := 1
				if (bekleyen[0].payload[0]>>2)&1 == 1 {
					kanal = 2
				}
				ogg = newOggYazıcı(fh, kanal, seriNo(string(w.trackID)))
				for _, b := range bekleyen {
					kuyrugaAl(b)
				}
				bekleyen = nil
				// Dosya açılmadan gelen ham SR'lar: dosyadaki ilk paketten
				// öncekiler hiçbir bölüme düşmez (boşuna "atlanan" sayılır),
				// kalanlar eşleyiciye — ilk bölümün çapası burada kurulur.
				for _, sr := range bekleyenSR {
					if sr.At >= ilkAn.UnixNano() {
						srIsle(sr)
					}
				}
				bekleyenSR = nil
				bosalt(false)
				continue
			}

			// ── 3. Normal akış: EŞLEYİCİ → KUYRUK → (gecikmeli) DOSYA ────
			// Eski kod burada `rel = rtp − ilkRTP` ile doğrudan yazıyordu;
			// susturmada duran RTP saati dosyadan süre siliyordu (kayıt 911).
			// Artık yer eşleyiciden (saat.go), yazım kuyruktan (yazPaket).
			kuyrugaAl(p)
			// SR artık kanaldan, paketlerle aynı sırada (yukarıda `p.sr`).
			bosalt(false)
		}
	}
}

// kuyrukOgesi — yazma gecikmesi kuyruğundaki paket: yeri (pts) ve bölümü.
type kuyrukOgesi struct {
	p     paket
	pts   int64
	bolum *sesBolum
}

func (w *Writer) hedefAra() *hedef { return hedefAra(w.sid, w.log) }

// hedefAra — track SID'inden hedefi bul. İKİ ADIM:
//
//	sn:rawrec:track:<sid> → oda adı
//	sn:rawrec:room:<oda>  → {recording_id, dir}
//
// Hangisi geç yazılırsa yazılsın çağıran beklemeye devam ediyor; ikisinin
// yazılma SIRASI önemsiz. Ses ve görüntü yazıcılarının ORTAK yolu.
//
// DEĞİŞKEN, FONKSİYON DEĞİL (Aşama 1, 2026-09-13): replay testi
// (`video_replay_test.go`) Redis'e gitmeden hedefi verebilsin diye.
var hedefAra = hedefAraRedis

func hedefAraRedis(sid string, log logger.Logger) *hedef {
	ctx, iptal := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer iptal()

	oku := func(anahtar string) []byte {
		v, err := rdb.Get(ctx, anahtar).Bytes()
		if err != nil {
			if err != redis.Nil {
				// Bir kez DEĞİL: Redis erişilemezse bunu görmek zorundayız.
				log.Warnw("rawrec kontrol anahtarı okunamadı", err,
					"anahtar", anahtar)
			}
			return nil
		}
		return v
	}

	tv := oku(trackPrefix + sid)
	if tv == nil {
		return nil
	}
	var te trackEşleme
	if json.Unmarshal(tv, &te) != nil || te.Room == "" {
		return nil
	}
	rv := oku(roomPrefix + te.Room)
	if rv == nil {
		return nil
	}
	var h hedef
	if json.Unmarshal(rv, &h) != nil || h.Dir == "" || h.RecordingID == 0 {
		return nil
	}
	h.Identity = te.Identity
	return &h
}

// ── GERİ DÜŞÜŞ KAYDI (Aşama 0, 2026-09-13) ─────────────────────────────────
//
// NEDEN VAR: SFU bir track'i yazmaktan vazgeçtiğinde (kodek tanınmıyor, kayıt
// anahtarı bulunamadı, dosya açılamadı) tek iz konteyner logundaki bir
// satırdı. Kayıt sessizce tarayıcı yedeğinden geliyor ve kimse fark
// etmiyordu — ölçüldü: kayıt 876'da mobil öğrencinin H.264 paylaşımı,
// 870-872'de öğretmen kamerası (webhook CAMERA anahtarı yazmamıştı) böyle
// kaçtı; ikisi de ancak dosyalar elle incelenince görüldü.
//
// Artık vazgeçiş Redis'e düşüyor; postprocess kayıt bitince okuyup satıra
// `kaynak_uyari` yazıyor ve admin panelinde rozet çıkıyor
// (`recording_service/postprocess.py` → `_geri_dusus_raporla`).
//
//	anahtar  sn:rawrec:fallback:<track_sid>
//	değer    {"neden":"kodek","mime":"video/H264","kaynak":"SCREEN_SHARE",…}
//	ömür     12 saat — postprocess kayıt bitince okuyor, ders en çok birkaç saat
//
// Plan: docs/split-recording/SFU-KODEK-BAGIMSIZ-PLANI.md (Aşama 0)
const fallbackPrefix = "sn:rawrec:fallback:"
const fallbackTTL = 12 * time.Hour

// geriDusus — Redis'e düşen kaydın içeriği. `Neden` üç değerden biri:
// "kodek" (yazıcı hiç kurulmadı), "hedef-yok" (kayıt anahtarı `waitFor`
// içinde bulunamadı; arama sürüyor, sonradan bulunursa dosya yine yazılır ve
// bu not SİLİNİR — `geriDususSil`), "dosya-acilamadi" (onarılamaz disk hatası).
type geriDusus struct {
	Neden   string `json:"neden"`
	Mime    string `json:"mime,omitempty"`
	Kaynak  string `json:"kaynak"`
	Track   string `json:"track"`
	Ayrinti string `json:"ayrinti,omitempty"`
	An      string `json:"an"`
}

// geriDususYaz — MEDYA YOLUNDAN ÇAĞRILABİLİR: kendi goroutine'inde çalışır,
// bloklamaz; Redis yoksa yalnız loglar. Aynı track için son yazan kazanır.
func geriDususYaz(sid string, g geriDusus, log logger.Logger) {
	if rdb == nil || sid == "" {
		return
	}
	g.An = time.Now().Format(time.RFC3339Nano)
	go func() {
		b, err := json.Marshal(g)
		if err != nil {
			return
		}
		ctx, iptal := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer iptal()
		if err := rdb.Set(ctx, fallbackPrefix+sid, b, fallbackTTL).Err(); err != nil {
			log.Warnw("rawrec geri düşüş kaydı Redis'e yazılamadı", err,
				"sid", sid, "neden", g.Neden)
		}
	}()
}

// geriDususSil — hedef SONRADAN bulundu: `waitFor` dolunca düşülen "hedef-yok"
// notu artık yanlış, postprocess'i yanıltmasın. Kayıt 883'te `kaynak_uyari`
// bu bayat notla "SFU kayıt anahtarını bulamadı" dedi; dosya tamdı.
func geriDususSil(sid string, log logger.Logger) {
	if rdb == nil || sid == "" {
		return
	}
	go func() {
		ctx, iptal := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer iptal()
		if err := rdb.Del(ctx, fallbackPrefix+sid).Err(); err != nil {
			log.Warnw("rawrec geri düşüş notu silinemedi", err, "sid", sid)
		}
	}()
}

func (w *Writer) dosyaAç(h *hedef) (*os.File, string, error) {
	d := filepath.Join(h.Dir, klasörAdı(w.trackInfo, h.RecordingID))
	if err := os.MkdirAll(d, 0o755); err != nil {
		return nil, "", err
	}
	// ⚠ "sfu_" ÖNEKİ ŞART. Tarayıcı yolu AYNI KLASÖRE yazıyor ve dosya adını
	// kendi track kimliğinden kuruyor (`app.py`: `{n:02d}_{sid}`); iki taraf
	// aynı ada denk gelirse biri diğerini EZER ve iki yazıcı tek dosyayı
	// bozar. Önek bunu imkânsız kılıyor. postprocess dosyayı ADINDAN değil
	// yan JSON'daki `capa_kaynak`tan tanıyor (`_sfu_asil_tarayici_yedek`),
	// yani ad serbest.
	fh, yol, err := dosyaAcExcl(d, func(n int) string {
		return fmt.Sprintf("%02d_sfu_%s.opus", n, string(w.trackID))
	}, 1)
	if err != nil {
		return nil, "", err
	}
	w.log.Infow("rawrec ses dosyası açıldı",
		"track", w.trackID, "kaynak", kaynakAdı(w.trackInfo), "yol", yol)
	return fh, yol, nil
}

// dosyaAcExcl — dosyayı YALNIZ YOKSA açar; varsa numarayı artırır.
//
// NEDEN (Adım 1, 2026-09-21): tam yeniden bağlanmada livekit-client o an
// susturulmuş mikrofonu ve ekran paylaşımını AYNI MediaStreamTrack (aynı
// UUID = bizim trackID) ile yeniden yayınlıyor (`republishAllTracks`:
// `!track.isMuted && source !== ScreenShare` şartı). SFU'da yeni receiver →
// yeni yazıcı → aynı ad → `os.Create` (O_TRUNC) kopukluktan ÖNCEKİ parçayı
// sıfırlıyordu. Artık `02_sfu_…`, `03_sfu_…` açılır; postprocess dosya adına
// değil yan JSON'daki `sid`e bakıyor, desenler (`*_sfu_*`, uzantı) aynı.
func dosyaAcExcl(d string, ad func(n int) string, n int) (*os.File, string, error) {
	for ; n <= 99; n++ {
		yol := filepath.Join(d, ad(n))
		fh, err := os.OpenFile(yol, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return fh, yol, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("rawrec: %s içinde 99 dosya adı da dolu", d)
}

// yanJSON — postprocess'in okuduğu çapa dosyası.
//
// Alanlar bugünkü tarayıcı yolunun ürettiğiyle AYNI (postprocess değişmiyor).
// Fark: `capture_anchor` TAHMİN değil, Sender Report'tan hesap.
func (w *Writer) yanJSON(yol string, ilkAn time.Time, kare int, ilkRTP, sonRel uint32,
	sr *livekit.RTCPSenderReportState, sagl *saglik, redKurtarilan uint64,
	saat *sesSaat, h *hedef) {
	ek := katilimciEk(h)
	ek["bolumler"] = saat.bolumler
	ek["oz_denetim"] = saat.ozDenetim()
	if len(saat.actG.orn) > 0 {
		ek["act_gunlugu"] = saat.actG.orn // pts@hz ↔ yayıncı yakalama anı (rawrec50)
	}
	ek["yazma_gecikme_sn"] = yazmaGecikme.Seconds()
	yanJSONYaz(yanParam{
		yol: yol, sid: w.sid, ilkAn: ilkAn, kare: kare, ilkRTP: ilkRTP, sonRel: sonRel,
		clockRate: w.clockRate, sr: sr, buff: w.buff, log: w.log,
		trackID: w.trackID, düşen: w.düşen.Load(), etiket: "ses",
		srGunluk: saat.gunluk, capaKatman: 0,
		errBayrak: &w.errLogged, sagl: sagl, redKurt: redKurtarilan,
		kaynak:   kaynakTuru(w.trackInfo),
		ptsUzayi: true, anchorNs: saat.anchorNs, ek: ek,
	})
}

// katilimciEk — yan JSON'a `participant` (webhook `identity`). Ses VE
// görüntü yazıcısı: postprocess kamera ↔ mikrofon eşlemesini bununla yapıyor
// (kayıt 913'te görüntü yan dosyasında eksikti, kamera ilk mikrofona
// eşlenirdi).
func katilimciEk(h *hedef) map[string]any {
	ek := map[string]any{}
	if h != nil && h.Identity != "" {
		ek["participant"] = h.Identity
	}
	return ek
}

// srOrnek — akış boyunca görülen bir Sender Report'un (RTP, duvar saati)
// çifti. Yan JSON'a listeleniyor.
//
// ⚠ NEDEN GÜNLÜK TUTUYORUZ (2026-09-05). Çapa TEK bir SR'dan hesaplanıyor
// ve dosya kapandıktan sonra düzeltme şansı yok. Canlı Chrome ise her SR'da
// (saniyede bir) kendini yeniden ayarlıyor — 45 dakikada ~2700 düzeltme.
// Dosyada anlık düzeltme yapamayız ama SONRADAN düzeltebiliriz: bu günlükle
//
//	· yayıncının saat kayması ölçülebiliyor (kristal kayması 10-50 ppm,
//	  45 dakikada 25-135 ms eder — bugün göremiyoruz bile),
//	· yazdığımız zaman çizgisinin SR'ların söylediğiyle tutup tutmadığı
//	  postprocess'te denetlenebiliyor.
//
// Maliyeti birkaç yüz KB JSON.
// actOrnek — yakalama damgası günlüğü öğesi: dosyadaki yer (pts, saat
// biriminde) ↔ yayıncının yakalama anı (unix ns, YAYINCI saati). Ses ve
// görüntü aynı cihaz saatini taşıdığı için ikisinin `yak − pts/hz` çapaları
// arasındaki fark, oynatıcıdaki ses↔görüntü kaymasının kesin ölçüsü
// (rawrec50; el çırpma/göz gerekmez). Adım 7'nin de girdisi.
type actOrnek struct {
	Pts   int64 `json:"pts"`
	YakNs int64 `json:"yak_ns"`
}

// actGunlukUst — günlükte en çok bu kadar örnek; dolunca her ikinci örnek
// atılır ve kayıt adımı iki katına çıkar (uzun derste ~200-400 örnek kalır).
const actGunlukUst = 400

type actGunluk struct {
	orn   []actOrnek
	adim  int // 0/1 = her damgalı paket; 2, 4, 8… seyreltme adımı
	sayac int
}

func (g *actGunluk) ekle(pts, yakNs int64) {
	g.sayac++
	if g.adim > 1 && g.sayac%g.adim != 0 {
		return
	}
	g.orn = append(g.orn, actOrnek{Pts: pts, YakNs: yakNs})
	if len(g.orn) >= actGunlukUst {
		k := g.orn[:0]
		for i, o := range g.orn {
			if i%2 == 0 {
				k = append(k, o)
			}
		}
		g.orn = k
		if g.adim < 2 {
			g.adim = 2
		} else {
			g.adim *= 2
		}
	}
}

type srOrnek struct {
	Katman int32  `json:"katman"`
	RTP    uint32 `json:"rtp"`
	AtNs   int64  `json:"at_ns"`  // SUNUCU saati (AtAdjusted, yoksa At)
	NtpNs  int64  `json:"ntp_ns"` // YAYINCININ kendi saati, ham SR'dan
	// Pts — SR'ın damgasının DOSYA İÇİNDEKİ yeri (48 kHz örnek), ses
	// eşleyicisinden (saat.go). Susturmalı akışta ham RTP merdiven, pts
	// düz: çapa fit'i ve postprocess'in kayma ölçümü bunu okumalı.
	Pts *int64 `json:"pts,omitempty"`
}

// ntpNs — SR'ın NTP damgasını unix nanosaniyeye çevirir.
//
// ⚠ NEDEN AYRICA KAYDEDİYORUZ (2026-09-05). `AtNs` yayılım gecikmesi
// TAHMİNİYLE düzeltilmiş sunucu saati — içinde ağ gürültüsü var (kayıt
// 197'de ses tarafında artık 20,6 ms çıktı, saat kayması ölçülemedi).
// `NtpNs` ise yayıncının KENDİ saati, ham telden, hiçbir tahmin karışmadan.
//
// RTP ile NTP'yi karşılaştırmak tamamen YAYINCI İÇİ bir ölçüm: ses örnekleme
// saatinin (48 kHz kristal) yayıncının sistem saatine göre kayması. Ağ hiç
// işin içinde değil, yani gürültüsüz. Video için aynısını yapıp ikisinin
// farkını alınca ses-görüntü kaymasının GERÇEK kaynağı çıkıyor.
func ntpNs(ntp uint64) int64 {
	if ntp == 0 {
		return 0
	}
	sn := float64(ntp>>32) - ntpUnixOffset
	kesir := float64(ntp&0xFFFFFFFF) / 4294967296.0
	return int64((sn + kesir) * 1e9)
}

// kareOrnek — TEK BİR KARENİN ham kimliği: hangi katmandan geldi, ham RTP
// damgası neydi, dosyaya hangi PTS ile yazıldı.
//
// ⚠ TANI AMAÇLI, VARSAYILAN KAPALI (`SN_RAWREC_KARE_GUNLUGU=1` ile açılır).
// Bununla + `sr_gunlugu` ile bir kaydın zaman çizgisi ÇIRPMA OLMADAN, tamamen
// sayısal doğrulanabiliyor: her kare için "SR'lara göre nerede olmalıydı" ile
// "dosyada nerede" karşılaştırılıyor. Ölçüm bitince bayrak kapatılmalı —
// 45 dakikalık ders 24 fps'te ~2 MB JSON eder.
//
// Anahtarlar tek harf: dosya boyutunun yarısı anahtar adı olurdu.
type kareOrnek struct {
	K int32  `json:"k"` // katman
	R uint32 `json:"r"` // ham RTP damgası
	P int64  `json:"p"` // dosyaya yazılan PTS
}

// kareGunluguAcik — `kareOrnek` toplansın mı. Varsayılan KAPALI.
var kareGunluguAcik = envInt("SN_RAWREC_KARE_GUNLUGU", 0) != 0

// srGunluguUstSinir — günlükteki en fazla örnek. Saniyede bir SR, iki katman,
// 45 dakika ≈ 5400. Tavan bunun üstünde ki uzun ders de sığsın.
const srGunluguUstSinir = 20000

// yanParam — `yanJSONYaz`ın girdisi. Ses ve görüntü yazıcıları aynı yan
// dosyayı üretiyor; tek fark `etiket` (log satırı) ve `clockRate`.
type yanParam struct {
	yol        string
	sid        string // LiveKit track SID (TR_…) — postprocess SFU ↔ tarayıcı eşleşmesi için
	ilkAn      time.Time
	kare       int
	ilkRTP     uint32
	sonRel     uint32
	clockRate  uint32
	sr         *livekit.RTCPSenderReportState
	buff       srKaynak
	log        logger.Logger
	trackID    livekit.TrackID
	düşen      uint64
	etiket     string
	errBayrak  *atomic.Bool
	sagl       *saglik
	redKurt    uint64
	kaynak     string
	srGunluk   []srOrnek
	kareGunluk []kareOrnek
	capaKatman int32 // `ilkRTP` hangi katmanın damga uzayında (ses: 0)
	// ptsUzayi — SR örnekleri `Pts` taşıyor (ses eşleyicisi): çapa fit'i
	// ham RTP yerine dosya PTS'i üstünden yapılır (susturmada RTP merdiven).
	ptsUzayi bool
	// anchorNs — eşleyicinin ilk bölümden bildiği pts 0 saati; fit için
	// yeterli örnek yoksa tek-SR yolu yerine bu kullanılır.
	anchorNs int64
	// ek — yazıcıya özgü fazladan alanlar (bolumler, oz_denetim, participant).
	ek map[string]any
}

// capaFit — SR günlüğündeki BÜTÜN örneklerden çapa ve saat kayması.
//
// ⚠ NEDEN TEK SR YETMİYOR (2026-09-05). Çapa bugüne kadar dosya kapanırken
// görülen TEK bir Sender Report'tan hesaplanıyordu. Oysa 10 dakikalık bir
// kayıtta 600 SR görüyoruz — 599'unu çöpe atıp birine güveniyorduk. O tek
// örneğin içindeki gürültü doğrudan çapaya geçiyor ("kalan hata: onlarca
// milisaniye").
//
// Her örnek kendi başına bir çapa veriyor:
//
//	çapa_i = at_i − (rtp_i − ilkRTP) / clockRate
//
// Saatler birebir aynı hızda tıklasaydı hepsi AYNI çıkardı. Çıkmıyorlar:
//
//	· saçılma  → ölçüm gürültüsü, ortalamayla siliniyor
//	· eğilim   → saat kayması (yayıncı kristali ile sunucununki arasındaki
//	             ppm farkı)
//
// O yüzden ortalama DEĞİL, doğru uyduruyoruz ve **başlangıca** bakıyoruz:
// eğilim varsa ortalama kaydın ORTASINI verirdi, biz başını istiyoruz.
//
// Dönenler: çapa (unix ns), kayma (ppm), artık (RMS ms), kullanılan örnek.
// Yeterli örnek yoksa ok=false — çağıran eski tek-SR yoluna düşüyor.
func capaFit(orn []srOrnek, katman int32, ilkRTP uint32,
	clockRate uint32) (capa int64, ppm float64, artikMs float64, n int, ok bool) {
	if clockRate == 0 {
		return
	}
	type nokta struct{ x, y float64 } // x: zaman (sn), y: o örneğin çapası (sn)
	var pts []nokta
	for _, o := range orn {
		if o.Katman != katman || o.AtNs == 0 {
			continue
		}
		// İŞARETLİ fark: RTP 32 bit ve sarıyor.
		d := float64(int32(o.RTP-ilkRTP)) / float64(clockRate)
		pts = append(pts, nokta{x: float64(o.AtNs) / 1e9, y: float64(o.AtNs)/1e9 - d})
	}
	if len(pts) < capaEnAzOrnek {
		return
	}
	// İki geçiş: uydur, 3×RMS dışını at, yeniden uydur. Tek bozuk SR
	// (kaydın başında oturmamış tahminci) sonucu bozmasın.
	uydur := func(v []nokta) (a, b, rms float64) {
		n := float64(len(v))
		var mx, my float64
		for _, q := range v {
			mx += q.x
			my += q.y
		}
		mx /= n
		my /= n
		var num, den float64
		for _, q := range v {
			num += (q.x - mx) * (q.y - my)
			den += (q.x - mx) * (q.x - mx)
		}
		if den == 0 {
			return my, 0, 0
		}
		b = num / den
		a = my - b*mx
		var ss float64
		for _, q := range v {
			e := q.y - (a + b*q.x)
			ss += e * e
		}
		return a, b, math.Sqrt(ss / n)
	}
	a, b, rms := uydur(pts)
	if rms > 0 {
		var temiz []nokta
		for _, q := range pts {
			if math.Abs(q.y-(a+b*q.x)) <= 3*rms {
				temiz = append(temiz, q)
			}
		}
		if len(temiz) >= capaEnAzOrnek {
			pts = temiz
			a, b, rms = uydur(pts)
		}
	}
	// Çapa: doğrunun İLK örnekteki değeri (kaydın başı).
	x0 := pts[0].x
	capaSn := a + b*x0
	// Eğim, çapanın zamanla kayması = saatler arası ppm farkı.
	// (çapa_i = at_i − ölçülen_süre, yani eğim doğrudan kayma oranı.)
	return int64(capaSn * 1e9), b * 1e6, rms * 1000, len(pts), true
}

// capaEnAzOrnek — `capaFit`in çalışması için gereken en az SR örneği.
// Altındaysa tek-SR yoluna düşülüyor: az örnekle uydurulan doğru, tek
// örnekten daha kötü olabilir (kayıt 198: 18 örnekle hata payı ±66 ppm).
const capaEnAzOrnek = 12

// yanJSONYaz — postprocess'in okuduğu çapa dosyası (ORTAK).
func yanJSONYaz(p yanParam) {
	yol, ilkAn, kare := p.yol, p.ilkAn, p.kare
	ilkRTP, sonRel, sr := p.ilkRTP, p.sonRel, p.sr
	veri := map[string]any{
		"first_frame_at": ilkAn.Format(time.RFC3339Nano),
		"frames":         kare,
		"first_rtp":      ilkRTP,
		"last_rtp_rel":   sonRel,
		"rtp_hz":         p.clockRate,
		"capa_kaynak":    "sfu-sr",
		// AKIŞ KİMLİĞİ (2026-09-13, kayıt 877). Tarayıcı kopyası dosya adını
		// track SID'inden kuruyor (`NN_<TR_…>`), SFU dosyası ise istemci
		// kimliğinden (`NN_sfu_<cid>`); ikisi ad üstünden eşleşemiyordu ve
		// postprocess "SFU mu tarayıcı mı" kararını KLASÖR başına veriyordu.
		// Aynı derste bir paylaşımın SFU dosyası elenip başka bir paylaşımın
		// SFU dosyası sağlam çıkınca elenen paylaşımın tarayıcı kopyası da
		// atıldı, 30 saniyelik paylaşım kayda hiç girmedi. SID ile karar
		// akış başına veriliyor (`_sfu_asil_tarayici_yedek`).
		"sid": p.sid,
		"cid": string(p.trackID),
	}
	if len(p.srGunluk) > 0 {
		veri["sr_gunlugu"] = p.srGunluk
	}
	if len(p.kareGunluk) > 0 {
		veri["kare_gunlugu"] = p.kareGunluk
	}
	// SAĞLIK RAPORU — postprocess'in TEK ölçütü (bkz. saglik.go).
	// Karşılaştırma yerine yazıcının kendi beyanı.
	if r := p.sagl.rapor(); r != nil {
		if p.redKurt > 0 {
			r["red_kurtarilan"] = p.redKurt
		}
		if p.kaynak != "" {
			r["kaynak"] = p.kaynak
		}
		veri["saglik"] = r
	}
	// ── ÇAPA: YAYINCININ KENDİ SAATİ ─────────────────────────────────────
	// SR (NTP, RTP) çifti yayıncının saatini taşıyor ve SFU dışında hiçbir
	// yerden görülemiyor: LiveKit RTCP'yi sonlandırıp abonelere KENDİ
	// saatiyle SR üretiyor, tarayıcı API'leri de ham çifti vermiyor.
	//   ntp_unix = (ntp>>32) − 2208988800 + (ntp & 0xFFFFFFFF)/2^32
	//   anchor   = ntp_unix − (first_rtp − sr_rtp) / clock_rate
	// Fark İŞARETLİ hesaplanmalı: RTP damgası 32 bit ve sarıyor, ilk paket
	// SR'den önce de sonra da olabilir.
	// ── ÖNCE ÇOK ÖRNEKLİ FIT (2026-09-05) ───────────────────────────────
	// Bütün SR günlüğüne doğru uydurup çapayı KAYDIN BAŞINDA okuyoruz.
	// Yeterli örnek yoksa aşağıdaki tek-SR yoluna düşülüyor. Gerekçe:
	// `capaFit` başlığı.
	for k, v := range p.ek {
		veri[k] = v
	}
	// PTS UZAYI (Adım 2): ses yazıcısında SR örneklerinin `pts`i var; fit
	// `at − pts/hz` üstünden. Ham RTP ile yapılsaydı susturmalı dosyada
	// merdiven veriye doğru uydurulur, çapa saçmalardı (911: 50,7 sn).
	fitOrn, fitIlk := p.srGunluk, ilkRTP
	if p.ptsUzayi {
		fitOrn = make([]srOrnek, 0, len(p.srGunluk))
		for _, o := range p.srGunluk {
			if o.Pts != nil && *o.Pts >= 0 {
				fitOrn = append(fitOrn, srOrnek{Katman: o.Katman, RTP: uint32(*o.Pts),
					AtNs: o.AtNs, NtpNs: o.NtpNs})
			}
		}
		fitIlk = 0
	}
	if capaNs, ppm, artik, n, ok := capaFit(fitOrn, p.capaKatman,
		fitIlk, p.clockRate); ok {
		veri["capture_anchor"] = time.Unix(capaNs/1e9, capaNs%1e9).
			Format(time.RFC3339Nano)
		veri["capa_kaynak"] = "sfu-sr-fit"
		veri["capa_ornek"] = n
		veri["capa_artik_ms"] = yuvarla(artik)
		// Eğim = yayıncı saatinin sunucu saatine göre kayması. Düzeltmede
		// KULLANILMIYOR, yalnız kaydediliyor: farklı donanımlarda dağılımı
		// görüp gerekirse sonra düzeltmeye karar vereceğiz.
		veri["capa_kayma_ppm"] = yuvarla(ppm)
		p.log.Infow("rawrec çapa fit", "track", p.trackID, "ornek", n,
			"artik_ms", artik, "kayma_ppm", ppm, "kaynak", p.etiket)
	}
	if _, varmi := veri["capture_anchor"]; !varmi && p.ptsUzayi && p.anchorNs != 0 {
		// Fit için örnek yetmedi ama eşleyici ilk bölümün SR'ından pts 0'ın
		// saatini biliyor — tek-SR yolundan (ham RTP) daha doğru.
		veri["capture_anchor"] = time.Unix(p.anchorNs/1e9, p.anchorNs%1e9).
			Format(time.RFC3339Nano)
		veri["capa_kaynak"] = "sfu-sr-bolum"
	}
	if sr == nil {
		// Akış boyunca hiç örnekleyemediysek (çok kısa akış) son bir şans.
		sr = p.buff.GetSenderReportData()
	}
	if _, varmi := veri["capture_anchor"]; !varmi && sr != nil {
		// ── HANGİ ZAMAN ALANI VE NİYE (2026-08-31, ölçümle) ─────────────
		//
		// `NtpTimestamp` YAYINCININ KENDİ MAKİNE SAATİ ve ham telden geliyor;
		// LiveKit ona DOKUNMUYOR (`getExtendedSenderReport` yalnız
		// `RtpTimestampExt` ekliyor). Ölçüldü, iki ardışık kayıtta:
		//     kayıt 172  SR: 21:23:16Z   dosya açılışı: 22:38:06Z  → 4491 sn
		//     kayıt 173  SR: 21:29:15Z   dosya açılışı: 22:44:34Z  → 4519 sn
		// Öğretmenin bilgisayar saati ~75 dakika geride. Bu alana dayanmak,
		// çapayı o makinenin saatine emanet etmek olurdu.
		//
		// LiveKit'in çözümü yanı başında (rtpstats_receiver.go:759):
		//     senderClockTime := NtpTime(NtpTimestamp).UnixNano()
		//     delay := propagationDelayEstimator.Update(senderClockTime, At)
		//     AtAdjusted = senderClockTime + delay
		// `At` = SFU'nun SR'yi ALDIĞI an, SUNUCU saatinde (mono.UnixNano()).
		// `AtAdjusted` = yayıncının saat kayması yayılım gecikmesi tahminiyle
		// soğurulmuş hâli. Tercih: AtAdjusted → At.
		//
		// KALAN HATA: yayılım gecikmesi kadar (onlarca ms) — tarayıcı
		// sondasının jitter tamponu gecikmesiyle aynı mertebede, ama ölçüm
		// penceresine ve ağ dalgalanmasına bağlı DEĞİL.
		temel := sr.AtAdjusted
		kaynak := "AtAdjusted"
		if temel == 0 {
			temel = sr.At
			kaynak = "At"
		}
		if temel != 0 {
			fark := float64(int32(sr.RtpTimestamp-ilkRTP)) / float64(p.clockRate)
			an := float64(temel)/1e9 - fark
			sec := int64(an)
			nsec := int64((an - float64(sec)) * 1e9)
			veri["capture_anchor"] = time.Unix(sec, nsec).Format(time.RFC3339Nano)
			veri["capa_kaynak"] = "sfu-sr-" + kaynak

			// TANI: yayıncının saatiyle sunucununki arasındaki fark. Çapayı
			// etkilemiyor ama bozuk istemci saatini görünür kılıyor.
			ntpUnix := float64(sr.NtpTimestamp>>32) - ntpUnixOffset +
				float64(sr.NtpTimestamp&0xFFFFFFFF)/4294967296.0
			veri["yayinci_saat_farki_sn"] = float64(temel)/1e9 - ntpUnix

			p.log.Infow("rawrec çapa hesabı", "kaynak", kaynak,
				"sr_rtp", sr.RtpTimestamp, "ilk_rtp", ilkRTP, "fark_sn", fark,
				"yayinci_saat_farki_sn", veri["yayinci_saat_farki_sn"])
		}
	}
	if _, ok := veri["capture_anchor"]; !ok {
		// Çapa yazılmıyor → postprocess tarayıcı sondasına devam ediyor.
		// Kayıt BOZULMUYOR.
		p.log.Warnw("rawrec: kullanılabilir Sender Report yok, çapa yazılmadı",
			nil, "track", p.trackID)
	}

	b, err := json.Marshal(veri)
	if err != nil {
		return
	}
	jyol := yol[:len(yol)-len(filepath.Ext(yol))] + ".json"
	if err := os.WriteFile(jyol, b, 0o644); err != nil {
		birKezLogla(p.errBayrak, p.log, "rawrec yan JSON yazılamadı", err)
		return
	}
	p.log.Infow("rawrec "+p.etiket+" dosyası kapandı",
		"track", p.trackID, "kare", kare, "son_rtp_rel", sonRel,
		"çapa", veri["capture_anchor"], "düşen", p.düşen)
}

func (w *Writer) birKezLogla(msg string, err error) {
	birKezLogla(&w.errLogged, w.log, msg, err)
}

// birKezLogla — aynı sınıf hatayı YAZICI başına BİR KEZ logla. Disk
// dolduğunda saniyede 50 satır yazmasın diye; ama her yazıcı kendi hatasını
// bir kez söyleyebilsin diye bayrak yazıcıya ait.
func birKezLogla(bayrak *atomic.Bool, log logger.Logger, msg string, err error) {
	if bayrak.CompareAndSwap(false, true) {
		log.Warnw(msg+" (bu yazıcı için bir kez loglanır)", err)
	}
}

// seriNo — Ogg akış seri numarası. Track SID'inden türetiliyor ki aynı
// kayıttaki iki dosya çakışmasın.
func seriNo(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	if h == 0 {
		h = 1
	}
	return h
}

// redBirincil — RFC 2198 RED yükünden BİRİNCİL (en yeni) bloğu çıkarır.
//
// Biçim: art arda blok başlıkları, sonra veriler.
//
//	yedek başlık (F=1): 1 bayt PT | 14 bit zaman farkı | 10 bit uzunluk  = 4 bayt
//	birincil başlık (F=0): 1 bayt PT                                      = 1 bayt
//
// Birincil bloğun verisi EN SONDA ve uzunluğu başlıkta yazmıyor — kalan
// her şey odur.
// redBlok — RED yükünün içindeki TEK bir ses karesi.
// `ofset` = birincil karenin damgasından KAÇ ÖRNEK GERİDE olduğu (0 = birincil).
type redBlok struct {
	ofset uint32
	veri  []byte
}

// redBloklar — RFC 2198 RED yükünü PARÇALARINA ayırır: önce yedek bloklar
// (eskiden yeniye), en sonda birincil.
//
// ⚠ ESKİ SÜRÜM YALNIZ BİRİNCİLİ ALIYORDU ve yedekleri ATLIYORDU
// (`i += u`). Bedeli 2026-09-03'te ölçüldü (kayıt 183): SFU ham ses dosyası
// 5.789 paket, bot tarayıcısının kopyası 6.446 paket — SFU %11 DAHA AZ.
// Sebep tam buydu: öğretmen→SFU yolunda kaybolan paketin KOPYASI bir
// sonraki paketin içinde geliyor; SFU onu abonelere iletiyor, botun
// tarayıcısı kopyadan kurtarıp yazıyor, bizim yazıcımız ise çöpe atıyordu.
// Yani veri SFU'DA VARDI, kullanmıyorduk. Artık kullanıyoruz: delik kalan
// damgaya denk gelen yedek blok dosyaya yazılıyor.
//
// Başlık düzeni (F=1 olan her blok 4 bayt):
//
//	 0                   1                   2                   3
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|F|  blok PT   |   damga ofseti (14 bit)   |  blok uzunluğu (10) |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//
// Birincil başlık F=0 ve tek bayt; ofseti 0, uzunluğu "kalan her şey".
func redBloklar(p []byte) []redBlok {
	type basl struct {
		ofset   uint32
		uzunluk int
	}
	i := 0
	basliklar := make([]basl, 0, 4)
	for {
		if i >= len(p) {
			return nil
		}
		if p[i]&0x80 == 0 { // birincil başlık — blok tablosu bitti
			i++
			break
		}
		if i+4 > len(p) {
			return nil
		}
		basliklar = append(basliklar, basl{
			ofset:   uint32(p[i+1])<<6 | uint32(p[i+2])>>2,
			uzunluk: int(p[i+2]&0x03)<<8 | int(p[i+3]),
		})
		i += 4
	}
	out := make([]redBlok, 0, len(basliklar)+1)
	for _, b := range basliklar {
		if i+b.uzunluk > len(p) {
			return nil
		}
		if b.uzunluk > 0 {
			out = append(out, redBlok{ofset: b.ofset, veri: p[i : i+b.uzunluk]})
		}
		i += b.uzunluk
	}
	if i < len(p) {
		out = append(out, redBlok{ofset: 0, veri: p[i:]})
	}
	return out
}
