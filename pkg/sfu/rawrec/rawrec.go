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
// Burada o sorun YOK: Sender Report aynı süreçte (`buff.GetSenderReportData()`),
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
	"fmt"
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
const lookupEvery = 300 * time.Millisecond

// geçAramaAralığı — tampon bırakıldıktan SONRAKİ yoklama sıklığı. Artık acele
// yok (kaçırılan kısım zaten kaçtı), Redis'i boşuna yormayalım.
const geçAramaAralığı = 2 * time.Second

var (
	once    sync.Once
	enabled bool
	rdb     *redis.Client
	waitFor time.Duration
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
	// 60 sn — 10 DEĞİL. Ölçüldü (kayıt 169): track 00:23:26'da yayınlandı ama
	// kayıt satırı DB'ye 00:23:43'te yazıldı (17 sn sonra); webhook o ana
	// kadar "kayıt satırı yok" deyip anahtarı yazamıyor. 10 sn'lik bekleme
	// dolup vazgeçiyorduk. Bellek bedeli: 60 sn × 50 paket/sn × ~90 B ≈ 270 KB.
	waitFor = time.Duration(envInt("SN_RAWREC_WAIT", 60)) * time.Second
	pinHigh = envOr("SN_RAWREC_PIN_HIGH", "1") != "0"
	kfEnÇokKare = envInt("SN_RAWREC_KEYFRAME_MAX_KARE", 24)
	kfEnAzSn = time.Duration(envInt("SN_RAWREC_KEYFRAME_MIN_SN", 2)) * time.Second
	kfEnÇokSn = time.Duration(envInt("SN_RAWREC_KEYFRAME_MAX_SN", 30)) * time.Second
	enabled = true
	logger.Infow("rawrec açık", "redis", envOr("SN_RAWREC_REDIS", "localhost:6379"),
		"bekleme", waitFor, "üst_katman_sabitle", pinHigh,
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
}

// trackEşleme — track anahtarının içeriği (webhook yazıyor).
type trackEşleme struct {
	Room string `json:"room"`
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
func (w *Writer) Write(payload []byte, rtpTS uint32, örnek uint32, seq uint16) {
	if w == nil || w.closed.Load() || len(payload) == 0 {
		return
	}
	an := time.Now()
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
			w.kuyruğaKoy(b.veri, rtpTS-b.ofset, örnek, an, b.ofset != 0, seq)
		}
		return
	}
	w.kuyruğaKoy(payload, rtpTS, örnek, an, false, seq)
}

// kuyruğaKoy — tek bir ses karesini sıraya koyar. ASLA BLOKLAMAZ.
func (w *Writer) kuyruğaKoy(payload []byte, rtpTS, örnek uint32,
	an time.Time, yedek bool, seq uint16) {
	if len(payload) == 0 {
		return
	}
	// KOPYA ŞART: forwardRTP `pktBuf`ı her pakette yeniden kullanıyor.
	cp := make([]byte, len(payload))
	copy(cp, payload)
	select {
	case w.ch <- paket{payload: cp, rtp: rtpTS, örnek: örnek, geliş: an,
		yedek: yedek, seq: seq}:
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
	close(w.ch)
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
		h           *hedef
		bekleyen    []paket
		ogg         *oggYazıcı
		fh          *os.File
		yol         string
		ilkRTP      uint32
		ilkAn       time.Time // dosyadaki İLK paketin anı → yan JSON
		ilkPaketAn  time.Time // track'in ilk paketi → bekleme zaman aşımı
		sonRel      uint32
		kare        int
		yazılan     uint64
		vazgeç      bool // yalnız ONARILAMAZ hata (dosya açılamadı) için
		tamponDurdu bool

		// ── SR'yi AKIŞ SÜRERKEN topla (2026-08-31) ────────────────────────
		// İlk sürüm `GetSenderReportData()`yı dosya KAPANIRKEN okuyordu ve
		// 74,8 DAKİKA BAYAT veri alıyordu (kayıt 172): kapanış anında
		// `forwardRTP` çoktan dönmüş, tampon havuza geri verilmiş oluyor ve
		// içindeki SR başka bir oturuma ait. Artık her N pakette bir
		// örnekleyip SONUNCUSUNU saklıyoruz.
		sonSR    *livekit.RTCPSenderReportState
		srSayaç  int
		srGunluk []srOrnek
		srSonRTP uint32

		// SAĞLIK: yazıcının kendi durumu (bkz. saglik.go). Postprocess
		// artık YALNIZ buna bakıyor, tarayıcı kopyasıyla karşılaştırma yok.
		sagl          = yeniSaglik()
		redKurtarilan uint64 // RED yedeğinden kurtarılan (delik dolduran) kare
	)

	kapat := func() {
		if ogg != nil {
			ogg.finish()
		}
		if fh != nil {
			fh.Close()
		}
		if yol != "" {
			w.yanJSON(yol, ilkAn, kare, ilkRTP, sonRel, sonSR, sagl, redKurtarilan,
				srGunluk)
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
				// ⚠ KALICI VAZGEÇME YOK (2026-09-03, kayıt 177).
				// Eski sürüm `waitFor` dolunca bir daha bakmıyordu. O derste
				// anahtar 77 saniye sonra yazıldı ve yazıcı çoktan pes
				// etmişti — birinci paylaşım hiç kaydedilmedi. Artık yalnız
				// TAMPON duruyor (bellek için); arama track boyunca sürüyor
				// ve anahtar geç gelirse kaydın KALANI kurtarılıyor.
				if !tamponDurdu && time.Since(ilkPaketAn) > waitFor {
					w.log.Infow("rawrec hedef hâlâ yok, tampon bırakıldı "+
						"(arama sürüyor)", "track", w.trackID, "sid", w.sid,
						"bekleme", waitFor, "bırakılan_paket", len(bekleyen))
					tamponDurdu = true
					sagl.tamponDurdu = true
					bekleyen = nil
					tik.Reset(geçAramaAralığı)
				}
				continue
			}
			w.log.Infow("rawrec hedef bulundu", "track", w.trackID,
				"kayit", h.RecordingID, "dizin", h.Dir,
				"gecikme", time.Since(ilkPaketAn).Round(time.Millisecond))
			tik.Stop()

		case p, ok := <-w.ch:
			if !ok {
				return // kanal kapandı → defer kapat() dosyayı bitirir
			}
			if vazgeç {
				continue
			}
			// Kayıp ölçümü ASIL paketler üstünden (yedek blokların sıra
			// numarası taşıyıcı paketinki, kendilerinin değil).
			if !p.yedek {
				sagl.paketGeldi(p.seq)
			}

			// ── 1. Hedef henüz bilinmiyor: beklet ───────────────────────
			if h == nil {
				if ilkPaketAn.IsZero() {
					ilkPaketAn = p.geliş
					w.log.Infow("rawrec ilk paket geldi, hedef aranıyor",
						"track", w.trackID, "sid", w.sid)
				}
				if !tamponDurdu {
					bekleyen = append(bekleyen, p)
				}
				continue
			}

			// ── 2. Hedef var ama dosya henüz açılmadı ───────────────────
			if fh == nil {
				// Tampon bırakılmışsa elde bekleyen kalmadı; dosya bu
				// paketten başlıyor. Çapa da ona göre kuruluyor.
				bekleyen = append(bekleyen, p)
				ilkRTP = bekleyen[0].rtp
				ilkAn = bekleyen[0].geliş
				var err error
				fh, yol, err = w.dosyaAç(h)
				if err != nil {
					w.birKezLogla("rawrec dosya açılamadı", err)
					vazgeç = true
					bekleyen = nil
					continue
				}
				kanal := 1
				if (bekleyen[0].payload[0]>>2)&1 == 1 {
					kanal = 2
				}
				ogg = newOggYazıcı(fh, kanal, seriNo(string(w.trackID)))
				for _, b := range bekleyen {
					rel := b.rtp - ilkRTP
					g := uint64(rel) + uint64(b.örnek)
					if b.yedek && g <= yazılan {
						continue
					}
					if b.yedek {
						redKurtarilan++
					}
					yazılan = g
					ogg.write(b.payload, yazılan)
					sonRel = rel
					kare++
					sagl.kareYazildi(b.geliş)
				}
				bekleyen = nil
				continue
			}

			// ── 3. Normal akış ──────────────────────────────────────────
			rel := p.rtp - ilkRTP
			g := uint64(rel) + uint64(p.örnek)
			// YEDEK BLOK YALNIZ DELİK DOLDURUR. Damgası zaten yazılmış bir
			// yere denk geliyorsa kopyadır, atılır — yoksa dosya şişer ve
			// ses tekrarlar. Delik varsa (asıl paket kaybolmuş) yazılır:
			// SFU dosyası tarayıcı kopyası kadar eksiksiz olur.
			if p.yedek {
				if g <= yazılan {
					continue
				}
				redKurtarilan++
			}
			// Granül ARTAN olmalı; SFU sıralı veriyor ama RED/PLC ile eş
			// damga gelebiliyor. Eşitse bir kare ileri it (bugünkü Python
			// yazıcının monotonluk koruması ile aynı).
			if g <= yazılan {
				yazılan += uint64(p.örnek)
			} else {
				yazılan = g
			}
			ogg.write(p.payload, yazılan)
			sonRel = rel
			kare++
			sagl.kareYazildi(p.geliş)

			// Saniyede ~50 paket → 50'de bir ≈ saniyede bir örnek. SR de
			// zaten saniyede bir geliyor, daha sık bakmanın anlamı yok.
			srSayaç++
			// 50 → 10 (2026-09-05): SR'ın kendisi seste ~5 sn'de bir
			// güncelleniyor, sık bakmak yeni örnek üretmiyor ama
			// güncellemeyi GECİKMEDEN yakalıyor. Maliyeti bir işaretçi
			// okuması.
			if srSayaç%10 == 0 {
				if sr := w.buff.GetSenderReportData(); sr != nil && sr.NtpTimestamp != 0 {
					sonSR = sr
					// SR GÜNLÜĞÜ — seste katman yok, ama yayıncının SAAT
					// KAYMASINI ölçmek için şart: ses ile görüntü 45 dakikada
					// birbirinden ayrılıyorsa kaynağı burada görünür.
					// Gerekçe: `srOrnek` başlığı.
					temel := sr.AtAdjusted
					if temel == 0 {
						temel = sr.At
					}
					if temel != 0 && sr.RtpTimestamp != srSonRTP &&
						len(srGunluk) < srGunluguUstSinir {
						srSonRTP = sr.RtpTimestamp
						srGunluk = append(srGunluk, srOrnek{
							Katman: 0, RTP: sr.RtpTimestamp, AtNs: temel,
							NtpNs: ntpNs(sr.NtpTimestamp)})
					}
				}
			}
		}
	}
}

func (w *Writer) hedefAra() *hedef { return hedefAra(w.sid, w.log) }

// hedefAra — track SID'inden hedefi bul. İKİ ADIM:
//
//	sn:rawrec:track:<sid> → oda adı
//	sn:rawrec:room:<oda>  → {recording_id, dir}
//
// Hangisi geç yazılırsa yazılsın çağıran beklemeye devam ediyor; ikisinin
// yazılma SIRASI önemsiz. Ses ve görüntü yazıcılarının ORTAK yolu.
func hedefAra(sid string, log logger.Logger) *hedef {
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
	return &h
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
	yol := filepath.Join(d, "01_sfu_"+string(w.trackID)+".opus")
	fh, err := os.Create(yol)
	if err != nil {
		return nil, "", err
	}
	w.log.Infow("rawrec ses dosyası açıldı",
		"track", w.trackID, "kaynak", kaynakAdı(w.trackInfo), "yol", yol)
	return fh, yol, nil
}

// yanJSON — postprocess'in okuduğu çapa dosyası.
//
// Alanlar bugünkü tarayıcı yolunun ürettiğiyle AYNI (postprocess değişmiyor).
// Fark: `capture_anchor` TAHMİN değil, Sender Report'tan hesap.
func (w *Writer) yanJSON(yol string, ilkAn time.Time, kare int, ilkRTP, sonRel uint32,
	sr *livekit.RTCPSenderReportState, sagl *saglik, redKurtarilan uint64,
	srGunluk []srOrnek) {
	yanJSONYaz(yanParam{
		yol: yol, ilkAn: ilkAn, kare: kare, ilkRTP: ilkRTP, sonRel: sonRel,
		clockRate: w.clockRate, sr: sr, buff: w.buff, log: w.log,
		trackID: w.trackID, düşen: w.düşen.Load(), etiket: "ses",
		srGunluk: srGunluk, capaKatman: 0,
		errBayrak: &w.errLogged, sagl: sagl, redKurt: redKurtarilan,
		kaynak: kaynakTuru(w.trackInfo),
	})
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
type srOrnek struct {
	Katman int32  `json:"katman"`
	RTP    uint32 `json:"rtp"`
	AtNs   int64  `json:"at_ns"`  // SUNUCU saati (AtAdjusted, yoksa At)
	NtpNs  int64  `json:"ntp_ns"` // YAYINCININ kendi saati, ham SR'dan
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
	if capaNs, ppm, artik, n, ok := capaFit(p.srGunluk, p.capaKatman,
		ilkRTP, p.clockRate); ok {
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
