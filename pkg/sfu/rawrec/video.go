package rawrec

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp/codecs"

	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
)

// ── AŞAMA 2: HAM GÖRÜNTÜYÜ SFU'NUN İÇİNDE YAZ ───────────────────────────────
//
// Ses tarafıyla (rawrec.go) aynı gerekçe: dosyanın ders eksenindeki yeri
// TAHMİN değil, yayıncının kendi RTCP Sender Report'undan HESAP. Fark şu ki
// görüntüde bir kare BİRDEN ÇOK RTP paketine bölünüyor, yani burada ayrıca
// KARE TOPLAMA işi var — seste "bir paket = bir kare" olduğu için yoktu.
//
// ÜÇ KURAL:
//
//  1. TEK SEFERDE TEK KATMAN, HEDEF DAİMA ÜST KATMAN. Alt katmanlar da
//     SFU'ya geliyor; ikisini birden yazmak dosyayı bozar (aynı damgada
//     farklı çözünürlükte kareler). Ama üst katman YOKKEN alt katmanı
//     yazmamak da kaydı kaybettiriyor — canlı yolun kuralı burada da
//     geçerli: eldeki katmanı yaz, hedefin anahtar karesinde ona geç.
//     Ayrıntı: `hedefBelirle` / `katmanaGeç` / `Write` başlıkları.
//
//  2. İLK ANAHTAR KAREDEN BAŞLA. Dosyanın ilk karesi anahtar kare değilse
//     çözücü hiçbir şey çözemez ve postprocess'in kare taraması sıfır kare
//     bulur — kayıt sessizce görüntüsüz çıkar.
//
//  3. YALNIZ VP9. IVF dosyasını `VP90` etiketiyle yazıyoruz; başka bir kodek
//     bu etiketle geçerli GÖRÜNÜR ama çözülemez (kayıt 124: H.264, kayıt 158:
//     AV1 — ikisi de görüntüsüz çıktı). H.264 gelirse yazıcı hiç kurulmuyor
//     ve TARAYICI YEDEĞİ devreye giriyor; kayıt kurtuluyor.
//     Proje bugün her iki akışta da VP9 yayınlıyor
//     (`classroom-media-config.js` → `VIDEO_CODEC = 'vp9'`).

// vpaket — kanaldan geçen iş birimi. `ExtPacket`'ı TUTMUYORUZ: dayanağı
// forwardRTP'nin yeniden kullandığı tampon, kopyalamak ŞART.
type vpaket struct {
	payload []byte // RTP yükü — kodek başlığı DAHİL, çözme loop'ta
	rtp     uint32
	marker  bool
	anahtar bool // bu paket anahtar karenin parçası mı
	geliş   time.Time
	seq     uint16 // RTP sıra numarası (kayıp ölçümü — bkz. saglik.go)
	katman  int32  // hangi simulcast katmanından geldi
}

// VideoWriter — bir video track'inin ÜST katmanı için ham IVF yazıcısı.
type VideoWriter struct {
	trackID   livekit.TrackID
	sid       string
	trackInfo *livekit.TrackInfo
	clockRate uint32
	mimeType  mime.MimeType
	en, boy   uint16
	buff      srKaynak // yazıcıyı kuran katmanın kaynağı (varsayılan)
	log       logger.Logger

	// ⚠ PLI ve Sender Report KATMAN BAŞINA. Yazıcı katman değiştirdiğinde
	// anahtar kareyi ve çapayı YENİ katmanın kaynağından istemesi gerekiyor;
	// eski katmana PLI atmak işe yaramaz. Her forwarder kendi katmanını
	// `KatmanKaydet` ile bildiriyor.
	kMu       sync.Mutex
	kaynaklar map[int32]srKaynak
	üstKatman int32

	// KAYNAK TİPİNE GÖRE KATMAN POLİTİKASI (bkz. sabitlerin başlığı).
	// Ekran paylaşımı ile kamera aynı sayılarla yönetilemiyor: paylaşım
	// durgunken 1 fps'e iniyor ve yukarı çıkmakta acele etmenin karşılığı
	// var (metin okunuyor); kamera sürekli akıyor ve orada çırpınma riski
	// gerçek. LiveKit de tam bu ayrımı yapıyor.
	ölüEşiği  time.Duration
	yukarıTik int

	// ⚠ SÜZME KAYNAKTA (Write içinde), döngüde DEĞİL. Bütün katmanların
	// paketi kuyruğa girerse iki şey bozulur: (1) kuyruk (2048) kat kat
	// hızlı dolar ve yazılan katmanın paketleri düşer, (2) hedef aranırken
	// biriken tampon karışık katman içerir ve tampon boşaltılırken kare
	// toplayıcı farklı çözünürlükteki kareleri birleştirmeye çalışır.
	//
	// ⚠ İKİ KATMAN GEÇER, BİR DEĞİL (2026-09-04, kayıt 186). `suAnki`
	// yazdığımız katman, `hedef` geçmek İSTEDİĞİMİZ katman. Geçiş yalnız
	// hedefin ANAHTAR KARESİNDE yapılıyor; o ana kadar `suAnki` yazılmaya
	// devam ediyor. Hedefin paketleri de kuyruğa alınıyor ki anahtar
	// karesini görebilelim — ama YAZILMIYOR, yalnız yoklanıyor.
	// Gerekçe: `hedefBelirle` başlığı.
	suAnki      atomic.Int32
	hedef       atomic.Int32
	katmanSonNs [maxKatman]atomic.Int64 // katman -> son paketin anı (unix ns)

	// KATMAN BAŞINA KARE SAYACI (2026-09-05, kayıt 192). Aşağı düşme kararı
	// artık SÜRE değil KARE karşılaştırıyor; gerekçe `katmanÖndeKare`.
	// Sayaç `Write` içinde, RTP damgası değişimiyle tutuluyor: bir karenin
	// bütün paketleri aynı damgayı taşır, damga değişimi = yeni kare.
	katmanSonRTP [maxKatman]atomic.Uint32
	katmanKare   [maxKatman]atomic.Uint64

	ch        chan vpaket
	done      chan struct{}
	closed    atomic.Bool
	düşen     atomic.Uint64
	errLogged atomic.Bool
}

// NewVideoWriter — yazıcıyı kurar. Kapalıysa ya da kodek desteklenmiyorsa
// nil döner; çağıran nil kontrolü yapıp hiçbir şey yapmamalı (fail-open).
func NewVideoWriter(
	trackID livekit.TrackID,
	trackInfo *livekit.TrackInfo,
	clockRate uint32,
	mimeType mime.MimeType,
	buff srKaynak,
	üstKatman int32,
	log logger.Logger,
) *VideoWriter {
	if !Enabled() {
		return nil
	}
	// ⚠ SESSİZ NİL YOK — Aşama 1'de sessiz `return nil` bir saat kör aramaya
	// mal oldu. Her çıkış yolu loglanıyor.
	if mimeType != mime.MimeTypeVP9 {
		log.Infow("rawrec görüntü: VP9 değil, SFU yazmıyor (tarayıcı yedeği devrede)",
			"track", trackID, "mime", mimeType.String())
		return nil
	}
	if clockRate == 0 {
		clockRate = 90000 // video için tek makul değer
	}
	sid := string(trackID)
	if trackInfo != nil && trackInfo.Sid != "" {
		sid = trackInfo.Sid
	}
	en, boy := üstKatmanBoyutu(trackInfo)
	w := &VideoWriter{
		trackID:   trackID,
		sid:       sid,
		trackInfo: trackInfo,
		clockRate: clockRate,
		mimeType:  mimeType,
		en:        en,
		boy:       boy,
		buff:      buff,
		log:       log,
		kaynaklar: map[int32]srKaynak{},
		üstKatman: üstKatman,
		ch:        make(chan vpaket, chanBuf),
		done:      make(chan struct{}),
	}
	// ⚠ `katmanBelirsiz` = "hangi katmanı yazacağıma daha karar vermedim".
	// İlk gelen paketin katmanı yazılan katman oluyor, HEDEF ise baştan üst
	// katman: alttan başlasak bile üst katman anahtar kare verir vermez
	// AYNI DOSYADA yükseliyoruz (bkz. Write'taki not, kayıt 188).
	w.suAnki.Store(katmanBelirsiz)
	w.hedef.Store(üstKatman)
	if trackInfo != nil && trackInfo.Source == livekit.TrackSource_SCREEN_SHARE {
		w.ölüEşiği, w.yukarıTik = ölüEşiğiPaylaşım, yukarıTikPaylaşım
	} else {
		w.ölüEşiği, w.yukarıTik = ölüEşiğiKamera, yukarıTikKamera
	}
	go w.loop()
	log.Infow("rawrec görüntü yazıcı kuruldu", "sid", sid,
		"üst_katman", üstKatman,
		"ölü_eşiği", w.ölüEşiği, "yukarı_tik", w.yukarıTik,
		"kaynak", kaynakAdı(trackInfo), "hz", clockRate,
		"en", en, "boy", boy)
	return w
}

// üstKatmanBoyutu — IVF başlığındaki bilgi amaçlı çözünürlük. Bulunamazsa
// (0,0) döner ve yazıcı 2×2 yazar; ffmpeg gerçek boyutu akıştan okuyor.
func üstKatmanBoyutu(ti *livekit.TrackInfo) (uint16, uint16) {
	if ti == nil {
		return 0, 0
	}
	var en, boy uint32
	for _, l := range ti.Layers {
		if l == nil {
			continue
		}
		if l.Width*l.Height > en*boy {
			en, boy = l.Width, l.Height
		}
	}
	if en == 0 {
		en, boy = ti.Width, ti.Height
	}
	if en > 65535 || boy > 65535 {
		return 0, 0
	}
	return uint16(en), uint16(boy)
}

// Write — paketi sıraya koyar. ASLA BLOKLAMAZ.
//
// Çözme (VP9 başlığını soyma) burada DEĞİL, loop'ta yapılıyor: bu fonksiyon
// medya yolundan, forwardRTP goroutine'inden çağrılıyor ve orada yapılacak
// her iş canlı derse gecikme olarak yansır.
func (w *VideoWriter) Write(payload []byte, rtpTS uint32, marker, anahtarKare bool,
	seq uint16, katman int32) {
	if w == nil || w.closed.Load() || len(payload) == 0 {
		return
	}
	an := time.Now()
	// CANLILIK KAYDI — her katman için, aktif olmasa bile. Döngü katman
	// seçimini bunlara bakarak yapıyor.
	if katman >= 0 && int(katman) < maxKatman {
		w.katmanSonNs[katman].Store(an.UnixNano())
		// KARE SAYACI — VP9 başlığı ÇÖZÜLMÜYOR (burası medya yolu, bkz.
		// fonksiyon başlığı). Bir karenin bütün paketleri aynı RTP
		// damgasını taşıdığı için damganın değişmesi yeni kare demek.
		// Ortak hâlde yalnız bir Load, RMW yok.
		if w.katmanSonRTP[katman].Load() != rtpTS {
			w.katmanSonRTP[katman].Store(rtpTS)
			w.katmanKare[katman].Add(1)
		}
	}
	// YAZILAN ve HEDEFLENEN dışındaki katman kuyruğa GİRMEZ (gerekçe:
	// struct'taki not). İkisi eşitken (normal hâl) tek katman geçiyor.
	//
	// ⚠ HENÜZ KARAR YOKKEN HEPSİ GEÇER (2026-09-04, kayıt 188). Yayıncı
	// paylaşımı açtığında Chrome önce YALNIZ alt katmanı gönderiyor, üst
	// katmanı kodlayıcı ısınınca açıyor — 188'de bu 6,29 sn sürdü ve üst
	// katmana kilitli yazıcı o 6 saniyeyi hiç yazmadı (tarayıcı kopyası
	// 960x539 ile baştan kaydetmişti). Hangi katmanın önce geleceğini
	// bilemeyiz; ilk paket hangi katmandansa döngü onu `suAnki` yapıyor ve
	// süzme aynı paketle geri geliyor.
	if su := w.suAnki.Load(); su >= 0 &&
		katman != su && katman != w.hedef.Load() {
		return
	}
	// KOPYA ŞART: forwardRTP `pktBuf`ı her pakette yeniden kullanıyor.
	cp := make([]byte, len(payload))
	copy(cp, payload)
	select {
	case w.ch <- vpaket{payload: cp, rtp: rtpTS, marker: marker,
		anahtar: anahtarKare, geliş: an, seq: seq, katman: katman}:
	default:
		if n := w.düşen.Add(1); n == 1 || n%1000 == 0 {
			// Görüntüde düşen paket = BOZUK KARE. Seste bir paket kaybı 20 ms
			// sessizlik; burada karenin tamamı çözülemez hale geliyor.
			w.log.Warnw("rawrec görüntü sırası dolu, paket düşürüldü", nil,
				"toplam", n)
		}
	}
}

// KatmanKaydet — bir katmanın forwarder'ı kendi kaynağını bildiriyor.
// Yazıcı o katmana geçtiğinde PLI'yı ve Sender Report'u buradan alıyor.
func (w *VideoWriter) KatmanKaydet(katman int32, buff srKaynak) {
	if w == nil || buff == nil {
		return
	}
	w.kMu.Lock()
	w.kaynaklar[katman] = buff
	w.kMu.Unlock()
}

// kaynak — verilen katmanın kaynağı; yoksa yazıcıyı kuran katmanınki.
func (w *VideoWriter) kaynak(katman int32) srKaynak {
	w.kMu.Lock()
	b := w.kaynaklar[katman]
	w.kMu.Unlock()
	if b == nil {
		return w.buff
	}
	return b
}

// bölümAraÜstSınır — katman geçişindeki boşluk için akıl sağlığı tavanı.
// SR bozuk ya da bayatsa hesap saçmalayabilir; bu sınırın üstünde çıkan
// sonuç kullanılmıyor, varış ölçüsüne düşülüyor. 60 sn: gerçek bir geçiş
// boşluğu (anahtar kare beklemesi + kuyruk) hiçbir ölçümde 5 sn'yi aşmadı.
const bölümAraÜstSınır = 60 * time.Second

// katmanDuvarSaati — bir katmanın RTP damgasını SUNUCU saatine (unix ns)
// çevirir. Katmanın KENDİ Sender Report'unu kullanıyor; katmanların RTP
// tabanı ayrı olduğu için başka katmanın SR'ı işe yaramaz.
//
// Hesap dosya çapasıyla aynı (bkz. rawrec.go, "ÇAPA: YAYINCININ KENDİ SAATİ"):
//
//	duvar = AtAdjusted − (sr.RtpTimestamp − rtp) / clockRate
//
// `AtAdjusted` yayıncının saat kaymasını yayılım gecikmesi tahminiyle
// soğurulmuş hâli, yani SUNUCU saatinde — iki katmanınki karşılaştırılabilir.
// Fark İŞARETLİ: RTP damgası 32 bit ve sarıyor.
//
// SR yoksa 0 döner; çağıran bunu "hesaplayamadım" diye okuyup yedek yola
// düşüyor.
func (w *VideoWriter) katmanDuvarSaati(katman int32, rtp uint32) int64 {
	sr, ok := w.srDurumu(katman)
	if !ok {
		return 0
	}
	geri := int64(int32(sr.rtp - rtp)) // SR'den rtp'ye kaç tik geride
	return sr.temel - geri*int64(time.Second)/int64(w.clockRate)
}

// katmanKaymasi — `katman`ın RTP damgasını REFERANS katmanın damga uzayına
// taşıyan sabit kayma. Bulunamıyorsa ikinci dönüş false.
//
// ⚠ NEDEN REFERANS KATMAN (2026-09-05). Katman geçişindeki boşluğu her
// seferinde "eski katman ↔ yeni katman" diye ikili hesaplarsak, her geçiş
// KENDİ SR tahmin hatasını taşıyor ve hatalar zincirleniyor. LiveKit'in canlı
// yolu bunu yapmıyor: TEK bir referans katman seçip (`referenceLayerSpatial`)
// her katmanı hep ona çeviriyor.
//
// Aynısını yapınca kayma bir SABİT oluyor. Aşağı geçişte +B hatası varsa
// yukarı geçişte AYNI sabitin −B'si giriyor ve TAM olarak götürüyor —
// "yaklaşık götürür" değil, birebir. Birikme yapısal olarak imkânsız.
//
// Türetme: iki katmanın SR'ı aynı duvar anını farklı RTP damgalarıyla
// gösteriyor. `duvar_k(rtp) = temel_k − (srRtp_k − rtp)/rate` eşitliğini
// referans için de yazıp ikisini eşitleyince:
//
//	kayma_k = (temel_k − temel_r)·rate + srRtp_r − srRtp_k
func (w *VideoWriter) katmanKaymasi(katman, referans int32) (int64, bool) {
	if katman == referans {
		return 0, true
	}
	srK, okK := w.srDurumu(katman)
	srR, okR := w.srDurumu(referans)
	if !okK || !okR {
		return 0, false
	}
	// ns farkını tik farkına çevir; fark saniyeler mertebesinde, taşma yok.
	d := (srK.temel - srR.temel) * int64(w.clockRate) / int64(time.Second)
	return d + int64(srR.rtp) - int64(srK.rtp), true
}

// srDurumu — bir katmanın Sender Report'undan (RTP, sunucu saati) çifti.
func (w *VideoWriter) srDurumu(katman int32) (struct {
	rtp   uint32
	temel int64
	ntp   int64
}, bool) {
	var out struct {
		rtp   uint32
		temel int64
		ntp   int64
	}
	k := w.kaynak(katman)
	if k == nil {
		return out, false
	}
	sr := k.GetSenderReportData()
	if sr == nil {
		return out, false
	}
	temel := sr.AtAdjusted
	if temel == 0 {
		temel = sr.At
	}
	if temel == 0 || w.clockRate == 0 {
		return out, false
	}
	out.rtp, out.temel, out.ntp = sr.RtpTimestamp, temel, ntpNs(sr.NtpTimestamp)
	return out, true
}

// Close — sırayı kapatır ve yazıcının bitmesini bekler.
func (w *VideoWriter) Close() {
	if w == nil || !w.closed.CompareAndSwap(false, true) {
		return
	}
	close(w.ch)
	<-w.done
}

// videoKlasörAdı — bugünkü tarayıcı yolunun ürettiği isimlendirmenin AYNISI.
// postprocess bu isimlere göre arıyor; değiştirmek kaydı bozar.
func videoKlasörAdı(ti *livekit.TrackInfo, recID int) string {
	if ti != nil && ti.Source == livekit.TrackSource_SCREEN_SHARE {
		return fmt.Sprintf("share_%d", recID)
	}
	return fmt.Sprintf("camraw_%d", recID)
}

func (w *VideoWriter) loop() {
	defer close(w.done)

	var (
		h           *hedef
		bekleyen    []vpaket
		ivf         *ivfYazıcı
		fh          *os.File
		yol         string
		ilkRTP      uint32
		ilkAn       time.Time // ilk YAZILAN karenin anı → yan JSON
		ilkPaketAn  time.Time // ilk paketin anı → hedef bekleme zaman aşımı
		sonPTS      int64     = -1
		kare        int
		vazgeç      bool // yalnız ONARILAMAZ hata (dosya açılamadı) için
		tamponDurdu bool

		// Kare toplama durumu.
		parça        []byte
		topluyor     bool
		parçaRTP     uint32
		parçaAnahtar bool
		parçaBozuk   bool // toplanan karenin ortasında eksik paket var
		başladı      bool // ilk anahtar kare görüldü mü

		// SIRA TAMPONU (2026-09-04, kayıt 185). Paketler kare toplayıcıya
		// GELİŞ sırasıyla değil, SIRA NUMARASI düzeniyle giriyor. Gerekçe
		// ve ölçüm: sira.go dosya başlığı.
		sira siraTampon

		sonSR   *livekit.RTCPSenderReportState
		srSayaç int

		// sonAnahtar — dosyaya en son ne zaman ANAHTAR KARE yazıldı.
		// Düzenli anahtar kare isteği bunun üstünden karar veriyor
		// (bkz. anahtarKareAralığı).
		sonAnahtar time.Time
		// sonAnahtarKare — son anahtar kare yazıldığındaki `kare` değeri.
		// Bütçe KARE üstünden işliyor (gerekçe: rawrec.go, kfEnÇokKare).
		sonAnahtarKare int

		// ÜST KATMAN SUSTU İZLEME (2026-09-03, kayıt 182).
		// Yazıcı yalnız üst simulcast katmanına bağlı ve o katman ders
		// ortasında SUSABİLİYOR — kayıt 182'de kamera üst katmanı 0,56 sn
		// sonra kesildi (10 kare) ve 25 saniye boyunca tek paket gelmedi;
		// paylaşımda da ilk ~11 saniye üst katman neredeyse boştu. Dosyada
		// bunun izi YOKTU: düşen paket 0, hata yok, sadece kare yok.
		// Artık susma ve dönüş loglanıyor — teşhis dosyaya bakmadan
		// günlükten yapılabilsin.
		sonPaket   time.Time
		susmaLogla bool

		// SAĞLIK: yazıcının kendi durumu (bkz. saglik.go). Postprocess
		// artık YALNIZ buna bakıyor. Dosya başına yenileniyor.
		sagl = yeniSaglik()

		// ── KATMAN TAKİBİ (2026-09-04) ────────────────────────────────
		// Yazıcı artık sabit bir katmana bağlı değil; o an CANLI olan en
		// üst katmanı takip ediyor. Gerekçe: kayıt 184'te kameranın üst
		// katmanı 196 saniyenin 134'ünde ölmüştü ve alt katman akmasına
		// rağmen kayda hiçbir şey girmiyordu.
		katmanlar          = map[int32]*katmanDurum{}
		suAnkiKatman int32 = -1 // YAZDIĞIMIZ katman
		hedefKatman  int32 = -1 // geçmek İSTEDİĞİMİZ katman
		dosyaSayacı        = 1
		// yazılan katmanın bir önceki yoklamadaki kare sayacı; değiştiyse
		// bütün katmanların öndelik işareti tazeleniyor (bkz. yoklama).
		yazSonKare uint64

		// ── TEK DOSYA + BÖLÜM DAMGASI (2026-09-04, kayıt 186) ─────────
		// Katman değişimi artık YENİ DOSYA AÇMIYOR. Ama simulcast
		// katmanlarının RTP damga tabanı ortak olmak zorunda değil
		// (ölçüldü, kayıt 182: paylaşımda iki katmanın tabanı arasında
		// saatlerce fark vardı), o yüzden her geçişte damga YENİDEN
		// TABANLANIYOR: yeni bölümün ilk karesi, önceki karenin hemen
		// ardına konuyor ve aradaki gerçek boşluk VARIŞ SAATİNDEN
		// ölçülüyor. Geçiş anahtar karede yapıldığı için boşluk zaten
		// bir kare aralığı kadar.
		bölümTaban  int64  // bu bölümün ilk karesinin dosya içindeki PTS'i
		bölümİlkRTP uint32 // bu bölümün ilk karesinin RTP damgası
		bölümBekler bool   // sıradaki kare yeni bölümü başlatacak
		sonGeliş    time.Time
		// EN SON YAZILAN karenin RTP damgası ve ait olduğu katman. Katman
		// değişiminde bölüm tabanını ÇEKİM saatinden hesaplamak için şart
		// (bkz. `bölümBekler` bloğu, kayıt 194).
		sonYazRTP    uint32
		sonYazKatman int32 = -1

		// REFERANS KATMAN — bütün katmanlar bunun damga uzayına çevriliyor
		// (bkz. `katmanKaymasi`). İlk yazılan katman referans oluyor.
		referansKatman int32 = -1
		kaymaOnbellek  [maxKatman]int64
		kaymaVar       [maxKatman]bool

		// SR GÜNLÜĞÜ — yan JSON'a yazılıyor (bkz. `srOrnek`).
		srGunluk []srOrnek
		srSonRTP [maxKatman]uint32

		// KARE GÜNLÜĞÜ — tanı amaçlı, varsayılan KAPALI (bkz. `kareOrnek`).
		kareGunluk []kareOrnek

		// dosyaKatmanı — dosyayı AÇAN katman. `first_rtp` ondan geldiği
		// için Sender Report da ondan toplanmak ZORUNDA: başka katmanın
		// SR'si ile çapa hesabı saçmalıyor (ölçüldü, kayıt 185 `cam_2`
		// 4 saat 20 dk, kayıt 186 `share_04` 2,5 saat sapma — ikisi de
		// katman değişiminden hemen sonra açılan dosyalardı).
		dosyaKatmanı int32 = -1
	)

	// dosyaKapat — açık dosyayı bitirir ve yan JSON'unu yazar. Katman
	// değişiminde de, akış sonunda da BU çağrılıyor; bu yüzden birden çok
	// kez çağrılabilir olması şart.
	dosyaKapat := func() {
		if ivf != nil {
			ivf.finish()
			ivf = nil
		}
		if fh != nil {
			fh.Close()
			fh = nil
		}
		if yol != "" {
			sagl.siraDurumu(sira.bosluk, sira.gecPaket)
			yanJSONYaz(yanParam{
				yol: yol, ilkAn: ilkAn, kare: kare, ilkRTP: ilkRTP,
				sonRel: uint32(max64(sonPTS, 0)), clockRate: w.clockRate,
				sr: sonSR, buff: w.kaynak(dosyaKatmanı), log: w.log, trackID: w.trackID,
				düşen: w.düşen.Load(), etiket: "görüntü",
				sagl: sagl, kaynak: kaynakTuru(w.trackInfo),
				srGunluk: srGunluk, kareGunluk: kareGunluk,
				// `ilkRTP` ilk yazılan karenin damgası, o da referans
				// katmandan geliyor — fit yalnız o katmanın SR'larını
				// kullanmalı (katmanların RTP tabanı ayrı).
				capaKatman: referansKatman,
			})
			yol = ""
		}
	}
	defer dosyaKapat()

	// hedefBelirle — "şu katmana geçmek istiyorum" der ve BİTER. Geçişin
	// kendisi burada YAPILMAZ.
	//
	// ⚠ CANLI YOLUN TASARIMI (2026-09-04, kayıt 186). LiveKit'in abone
	// tarafı `videolayerselector/simulcast.go`da tam olarak bunu yapıyor:
	//
	//	if s.currentLayer.Spatial != s.targetLayer.Spatial {
	//	    //   1. Opportunistic layer upgrade - needs a key frame
	//	    //   2. Need to downgrade - needs a key frame
	//	    if extPkt.IsKeyFrame { currentLayer.Spatial = layer }
	//	}
	//	result.IsSelected = layer == s.currentLayer.Spatial
	//
	// "İstenen" ile "gönderilen" AYRI iki değişken; hedefin anahtar karesi
	// gelene kadar ESKİ katman gönderilmeye devam ediyor. Öğrencinin kalite
	// değişiminde hiç boşluk görmemesinin sebebi bu.
	//
	// rawrec bunu tek değişkenle yapıyordu ve bedeli ölçüldü (kayıt 186):
	// t=27,8'de üst katmana geçme kararı verildi, üst katman 38 saniye
	// anahtar kare vermedi, o sırada ALT katman kesintisiz 15 fps akıyordu
	// ama `Write` onu artık kabul etmediği için 38 saniye HİÇBİR ŞEY
	// yazılmadı. Oynatıcıda 53 saniyelik beyaz tahta olarak göründü.
	hedefBelirle := func(yeni int32, an time.Time) {
		if yeni == hedefKatman {
			return
		}
		hedefKatman = yeni
		w.hedef.Store(yeni) // Write hedefin paketlerini de kuyruğa alsın
		w.log.Infow("rawrec görüntü: KATMAN HEDEFİ DEĞİŞTİ (anahtar kare bekleniyor)",
			"track", w.trackID, "yazilan_katman", suAnkiKatman,
			"hedef_katman", yeni, "kaynak", kaynakAdı(w.trackInfo))
		w.kaynak(yeni).SendPLI(true) // hedefte anahtar kareyi bekletmeyelim
	}

	// katmanaGeç — geçişi GERÇEKTEN yap. Yalnız hedefin anahtar karesi
	// görüldüğünde çağrılıyor.
	//
	// ⚠ DOSYA KAPANMIYOR (2026-09-04, kayıt 186). Eskiden her geçiş yeni
	// dosya açıyordu; kayıt 186'da paylaşım 5, kamera 7 parçaya bölündü ve
	// postprocess bir paylaşım aralığına tek dosya koyabildiği için
	// parçaların çoğu üretilip HİÇ KULLANILMADI. Artık tek dosya: damga
	// `bölümTaban`/`bölümİlkRTP` ile kaldığı yerden sürüyor.
	katmanaGeç := func(yeni int32, an time.Time) {
		eskiK := suAnkiKatman
		// Kare toplama durumu sıfırlanmalı: yarım kalan kare eski katmana ait.
		// Sıra tamponunda bekleyenler de eski katmanın paketleri — üstelik
		// katmanların sıra numarası uzayları AYRI, karıştırılamaz.
		sira.sifirla()
		parça, topluyor, parçaAnahtar, parçaBozuk = nil, false, false, false
		// Katmanların sıra numarası uzayları ayrı — sağlık sayacı devretsin.
		sagl.katmanDegisti()
		bölümBekler = true
		suAnkiKatman = yeni
		w.suAnki.Store(yeni)
		w.log.Infow("rawrec görüntü: KATMAN DEĞİŞTİ (anahtar karede, dosya aynı)",
			"track", w.trackID, "eski_katman", eskiK, "yeni_katman", yeni,
			"kaynak", kaynakAdı(w.trackInfo), "o_ana_kadar_kare", kare)
	}

	// kayma — bir katmanın referans uzayına kayması, ÖNBELLEKLİ.
	//
	// ⚠ BİR KEZ HESAPLANIP DONDURULUYOR. Değeri her geçişte yeniden
	// hesaplasaydık her seferinde o anki SR tahminini alırdık ve hatalar
	// bağımsız olurdu — sabitleyince aşağı/yukarı geçişlerin hataları AYNI
	// sayıdan geldiği için birebir götürüyor. Gerekçe: `katmanKaymasi`.
	kayma := func(k int32) (int64, bool) {
		if k < 0 || int(k) >= maxKatman || referansKatman < 0 {
			return 0, false
		}
		if kaymaVar[k] {
			return kaymaOnbellek[k], true
		}
		v, ok := w.katmanKaymasi(k, referansKatman)
		if !ok {
			return 0, false
		}
		kaymaOnbellek[k], kaymaVar[k] = v, true
		w.log.Infow("rawrec görüntü: KATMAN KAYMASI sabitlendi",
			"track", w.trackID, "katman", k, "referans", referansKatman,
			"kayma_tik", v,
			"kayma_sn", float64(v)/float64(w.clockRate))
		return v, true
	}

	// kareYaz — toplanmış kareyi dosyaya yaz. Dosya henüz açılmadıysa ve kare
	// anahtar kare ise dosyayı burada açar.
	kareYaz := func(veri []byte, rtpTS uint32, anahtarKare bool, geliş time.Time) {
		if len(veri) == 0 {
			return
		}
		if fh == nil {
			if !anahtarKare {
				return // anahtar kare beklemedeyiz
			}
			// ⚠ VP9 MI GERÇEKTEN — SESSİZ BOZULMAYA KARŞI KORUMA.
			// VP9 kare başlığının ilk 2 biti frame_marker = 0b10. Ölçüldü:
			// VP9 → 2, AV1 → 0. Yanlışsa dosya çözülemez ve kayıt görüntüsüz
			// çıkar; en azından günlükte iz kalsın.
			if (veri[0] >> 6) != 0b10 {
				w.log.Errorw("rawrec görüntü: VP9 GİBİ GÖRÜNMÜYOR, IVF yine de "+
					"'VP90' etiketiyle yazılıyor ve muhtemelen ÇÖZÜLEMEYECEK",
					nil, "ilk_bayt", fmt.Sprintf("0x%02x", veri[0]),
					"frame_marker", veri[0]>>6)
			}
			var err error
			fh, yol, err = w.dosyaAç(h, dosyaSayacı)
			if err != nil {
				birKezLogla(&w.errLogged, w.log,
					"rawrec görüntü dosyası açılamadı", err)
				vazgeç = true
				return
			}
			ivf = newIvfYazıcı(fh, w.en, w.boy)
			ilkRTP = rtpTS
			// Sender Report ARTIK BU KATMANDAN toplanacak: `first_rtp`
			// buradan geldi (bkz. `dosyaKatmanı`).
			dosyaKatmanı = suAnkiKatman
			bölümTaban, bölümİlkRTP, bölümBekler = 0, rtpTS, false
			// İLK YAZILAN KATMAN REFERANS. Sonradan değişmiyor: bütün
			// kaymalar buna göre sabitleniyor.
			if referansKatman < 0 {
				referansKatman = suAnkiKatman
			}
			// `first_frame_at` = dosyadaki İLK KARENİN VARIŞ anı. Ne
			// track'in ilk paketi (anahtar kare beklenirken gelenler
			// atılıyor — kayıt 176'da fark 877 ms) ne de `time.Now()`
			// (tampon boşaltılırken bu, VARIŞ değil İŞLENME anı olurdu —
			// kayıt 179'da fark 16,6 sn).
			ilkAn = geliş
			başladı = true
		}
		// YENİ BÖLÜM (katman değişti): damgayı yeniden tabanla. Katmanların
		// RTP tabanı bambaşka (ayrı SSRC, ayrı rastgele offset), o yüzden
		// aradaki boşluğu AYRICA ölçmek gerekiyor.
		//
		// ⚠ VARIŞ SAATİ DEĞİL, ÇEKİM SAATİ (2026-09-05, kayıt 194).
		// Eskiden boşluk `geliş - sonGeliş` ile, yani paketlerin SFU'ya
		// VARDIĞI anlarla ölçülüyordu. Bu SİSTEMATİK OLARAK KISA çıkıyor:
		// bant daralınca ölmekte olan üst katmanın son kareleri kuyrukta
		// bekliyor (varışları çekimlerinden çok geç), yeni katmanın ilk
		// anahtar karesi ise anında geliyor. Aradaki fark, ölen katmanın
		// kuyruk gecikmesi kadar eksik hesaplanıyordu.
		//
		// ÖLÇÜLDÜ — her katman geçişinde ~1 saniye kayboluyordu:
		//
		//	kayıt 194 kamera  : gerçek 138,84 sn → dosya 136,81 sn (2 geçiş)
		//	kayıt 193 paylaşım: gerçek 206,51 sn → dosya 200,02 sn (7 geçiş)
		//	ses (tek katman, hiç geçiş yok)     : kayıp SIFIR
		//
		// 194'te tek bir geçişin hesabı: üst katmanın son karesi 10:07:14,90'da
		// vardı ama o an kuyruk gecikmesi 2,4 sn'ydi → çekimi 10:07:12,5.
		// Alt katmanın ilk karesi 10:07:16,05'te vardı, gecikmesiz →
		// çekimi 10:07:15,9. Gerçek boşluk 3,4 sn, yazdığımız 1,15 sn.
		//
		// Sonuç: video sesten kısa kalıyor ve oynatıcıda görüntü giderek
		// ÖNE kaçıyor. Ham SFU yoluna geçmenin bütün amacı ses-görüntü
		// farkını sıfırlamaktı; bu hata onu tek başına götürüyordu.
		//
		// ÇÖZÜM: her katmanın KENDİ Sender Report'u var (`GetSenderReportData`,
		// dosya çapası zaten onunla hesaplanıyor). İki damgayı da kendi
		// katmanının SR'ıyla sunucu saatine çevirip farkı alıyoruz.
		// SR yoksa ya da sonuç mantıksızsa eski davranışa düşülüyor —
		// kayıt hiçbir koşulda bozulmuyor.
		if bölümBekler {
			ara := int64(-1)
			araKaynak := "referans"
			// ── 1. YOL: REFERANS KATMAN UZAYI (tercih edilen) ──────────
			// İki damgayı da referansın uzayına taşıyıp farkı alıyoruz.
			// Kaymalar SABİT olduğu için aşağı/yukarı geçişlerin hataları
			// birebir götürüyor (gerekçe: `katmanKaymasi`).
			if sonYazKatman >= 0 && referansKatman >= 0 {
				ke, oke := kayma(sonYazKatman)
				ky, oky := kayma(suAnkiKatman)
				if oke && oky {
					d := int64(int32((rtpTS + uint32(ky)) - (sonYazRTP + uint32(ke))))
					if d > 0 && d < int64(bölümAraÜstSınır)*int64(w.clockRate)/int64(time.Second) {
						ara = d
					}
				}
			}
			// ── 2. YOL: KATMAN BAŞINA DUVAR SAATİ ──────────────────────
			// Referans kayması henüz hesaplanamadıysa (SR gelmemiş).
			if ara < 0 && sonYazKatman >= 0 {
				araKaynak = "çekim"
				eskiNs := w.katmanDuvarSaati(sonYazKatman, sonYazRTP)
				yeniNs := w.katmanDuvarSaati(suAnkiKatman, rtpTS)
				if eskiNs != 0 && yeniNs != 0 {
					if d := yeniNs - eskiNs; d > 0 && d < int64(bölümAraÜstSınır) {
						ara = d * int64(w.clockRate) / int64(time.Second)
					}
				}
			}
			if ara < 0 {
				// YEDEK: SR yok ya da sonuç mantıksız → eski varış ölçüsü.
				araKaynak = "varış"
				ara = 1
				if !sonGeliş.IsZero() {
					if d := geliş.Sub(sonGeliş); d > 0 {
						ara = int64(d.Seconds() * float64(w.clockRate))
					}
				}
			}
			if ara < 1 {
				ara = 1
			}
			// TANI: iki ölçüyü de yaz ki düzeltmenin etkisi kayıttan
			// doğrulanabilsin. `varis_sn` eski davranış, `ara_sn` yeni.
			varış := 0.0
			if !sonGeliş.IsZero() {
				varış = geliş.Sub(sonGeliş).Seconds()
			}
			w.log.Infow("rawrec görüntü: BÖLÜM TABANI",
				"track", w.trackID, "katman", suAnkiKatman,
				"ara_sn", float64(ara)/float64(w.clockRate),
				"varis_sn", varış, "kaynak", araKaynak)
			bölümTaban = sonPTS + ara
			bölümİlkRTP = rtpTS
			bölümBekler = false
		}
		// Damga bölümün ilk karesine göre göreceli, üstüne bölümün tabanı.
		// Dosya daima 0'dan başlıyor ve oynatıcının "aralık başından oynat"
		// mantığıyla örtüşüyor.
		rel := int64(int32(rtpTS - bölümİlkRTP)) // sarma-güvenli işaretli fark
		if rel < 0 {
			rel = 0
		}
		pts := rel + bölümTaban
		// ARTAN OLMAYAN DAMGA — IVF'te sıra dosya sırasıdır; geri giden damga
		// muxer'ı bozuyor ("non monotonically increasing dts"). Kareyi
		// ATMIYORUZ (görüntü kaybı olurdu), damgayı bir tık ileri alıyoruz.
		// Tarayıcı yolundaki koruma ile aynı (app.py, `backwards`).
		if pts <= sonPTS {
			pts = sonPTS + 1
		}
		sonPTS = pts
		sonGeliş = geliş
		sonYazRTP, sonYazKatman = rtpTS, suAnkiKatman
		if kareGunluguAcik {
			kareGunluk = append(kareGunluk, kareOrnek{
				K: suAnkiKatman, R: rtpTS, P: pts})
		}
		ivf.write(veri, uint64(pts))
		kare++
		sagl.kareYazildi(geliş)
		if anahtarKare {
			sonAnahtar = geliş
			sonAnahtarKare = kare
		}
	}

	// boşalt — sıra tamponunda hazır olan paketleri SIRA NUMARASI düzeninde
	// kare toplayıcıya ver. `hepsi` true iken bekleme süresi gözetilmiyor
	// (akış sonu / hedef bulunduğunda biriken tamponun boşaltılması).
	boşalt := func(an time.Time, hepsi bool) {
		for {
			p, kopuk, ok := sira.al(an, hepsi)
			if !ok {
				return
			}
			w.işle(p, kopuk, &parça, &topluyor, &parçaRTP, &parçaAnahtar,
				&parçaBozuk, sagl, kareYaz)
			if vazgeç {
				return
			}
		}
	}

	// ⚠ HEDEF ARAMASI ZAMANLAYICIDA, PAKETTE DEĞİL (2026-09-03, kayıt 179).
	// Eski sürüm yalnız paket geldiğinde yokluyordu ve GÖRÜNTÜDE akış
	// durabiliyor: ekran sabitken VP9 hiç kare göndermiyor, döngü
	// `range w.ch` üstünde bloke kalıyor, arama da duruyor. Ölçüldü: anahtar
	// 10:55:47.9'da hazırdı, ses yazıcısı 34 ms'de buldu, görüntü ancak
	// 13,3 SANİYE sonra bulabildi ve dosyanın başı o kadar geç açıldı.
	tik := time.NewTicker(lookupEvery)
	defer tik.Stop()

	bitti := false
	for !bitti {
		select {
		case <-tik.C:
			if h != nil {
				// ── DÜZENLİ ANAHTAR KARE ─────────────────────────────
				// Yalnız GEREKİYORSA istiyoruz: aralık boyunca zaten bir
				// anahtar kare yazıldıysa (yayıncı kendi gönderdi, ya da
				// başka bir abone PLI attı) yeni istek göndermiyoruz.
				// `force=false` → LiveKit'in kendi 500 ms PLI kısıtı da
				// devrede, yani hiçbir koşulda sel olmuyor.
				if !vazgeç && fh != nil {
					geçen := time.Since(sonAnahtar)
					kareFarkı := kare - sonAnahtarKare
					gerek := (kfEnÇokKare > 0 && kareFarkı >= kfEnÇokKare) ||
						(kfEnÇokSn > 0 && geçen >= kfEnÇokSn)
					// kfEnAzSn = maliyet tavanı; tam hareketli içerikte kare
					// bütçesi saniyede bir anahtar kare isteyebilir.
					if gerek && geçen >= kfEnAzSn {
						w.kaynak(suAnkiKatman).SendPLI(false)
					}
				}
				// Sessizlikte tamponda kalan paketler burada boşalıyor:
				// paket akışı durunca `boşalt` bir daha çağrılmazdı.
				boşalt(time.Now(), false)
				if !sonPaket.IsZero() && !susmaLogla &&
					time.Since(sonPaket) >= üstKatmanSusmaEşiği {
					susmaLogla = true
					w.log.Warnw("rawrec görüntü: AKTİF KATMAN SUSTU, paket gelmiyor",
						nil, "track", w.trackID, "sid", w.sid,
						"kaynak", kaynakAdı(w.trackInfo),
						"yazilan_katman", suAnkiKatman,
						"hedef_katman", hedefKatman,
						"sessizlik", time.Since(sonPaket).Round(time.Millisecond),
						"o_ana_kadar_kare", kare)
				}
				// ── KATMAN SEÇİMİ ────────────────────────────────────
				// Canlılık damgaları Write tarafından atomik yazılıyor
				// (aktif olmayan katmanın paketi kuyruğa girmiyor).
				an := time.Now()
				for k := int32(0); k < maxKatman; k++ {
					ns := w.katmanSonNs[k].Load()
					if ns == 0 {
						continue
					}
					d := katmanlar[k]
					if d == nil {
						d = &katmanDurum{}
						katmanlar[k] = d
					}
					d.sonPaket = time.Unix(0, ns)
					d.kare = w.katmanKare[k].Load()
					// KARE ARALIĞI: pencerede geçen süre ÷ pencerede üretilen
					// kare. Yoklama 500 ms'de bir ama 24 fps'te aralığa
					// 12 kare sığıyor — bölmezsek 500 ms ölçerdik.
					if d.kare != d.öncekiKare {
						if !d.öncekiSonPaket.IsZero() {
							if geçen := d.sonPaket.Sub(d.öncekiSonPaket); geçen > 0 {
								d.aralık = geçen / time.Duration(d.kare-d.öncekiKare)
							}
						}
						d.öncekiKare, d.öncekiSonPaket = d.kare, d.sonPaket
					}
				}
				// ── SR ÖRNEKLEME (2026-09-05) ────────────────────────
				// Her katmanın Sender Report'u değiştiyse günlüğe ekle.
				// Gerekçe: `srOrnek` başlığı.
				if len(srGunluk) < srGunluguUstSinir {
					for k := int32(0); k < maxKatman; k++ {
						if katmanlar[k] == nil {
							continue
						}
						if sr, ok := w.srDurumu(k); ok && sr.rtp != srSonRTP[k] {
							srSonRTP[k] = sr.rtp
							srGunluk = append(srGunluk, srOrnek{
								Katman: k, RTP: sr.rtp, AtNs: sr.temel,
								NtpNs: sr.ntp})
						}
					}
				}
				// ── ÖNDELİK İŞARETİ (2026-09-05, kayıt 192) ──────────
				// Yazılan katman bu yoklamada yeni kare ürettiyse bütün
				// katmanlar "başa baş" kabul edilip işaretleniyor. Sonraki
				// yoklamalarda `kare - işaret`, yazılan katman sustuğundan
				// beri o katmanın KAÇ KARE öne geçtiğini veriyor.
				//
				// Bütün sayaçlar yukarıdaki döngüde AYNI ANDA okunduğu için
				// işaret tutarlı: aynı yakalama tikinden çıkıp da bu
				// yoklamadan önce varmış bir alt katman karesi işarete
				// dahil olur, yani öndelik sayılmaz. Tam istediğimiz bu.
				//
				// Katman değişiminde ayrıca sıfırlamaya gerek yok: yeni
				// katmanın sayacı farklı olduğu için ilk yoklamada zaten
				// tazeleniyor.
				if yaz := katmanlar[suAnkiKatman]; yaz != nil &&
					yaz.kare != yazSonKare {
					yazSonKare = yaz.kare
					for _, d := range katmanlar {
						d.işaret = d.kare
					}
				}
				// ── YUKARI UYGUNLUK SAYACI (2026-09-05) ──────────────
				// Ölçüt MUTLAK HIZ DEĞİL, KARŞILAŞTIRMA: "üst katman,
				// yazdığımdan geride değil". İki simulcast katmanı aynı
				// yakalama tikinden beslendiği için 1 fps'lik durgun
				// ekranda bile ikisi başa baş kalıyor ve şart sağlanıyor.
				// Eski kural (`canlıdan` + `katmanCanlıEşiği`) mutlak 1
				// fps istiyordu; 1 fps'lik paylaşımda sayaç sürekli
				// sıfırlanıyor ve alt katmana bir düşünce BİR DAHA
				// ÇIKAMIYORDUK (kullanıcı bunu sordu, kodda doğrulandı).
				// LiveKit'in kendi tabanı da bunu kabul ediyor:
				// `DefaultStreamTrackerFrameConfigScreenshare.MinFPS = 0.5`.
				if yaz := katmanlar[suAnkiKatman]; yaz != nil {
					// TOLERANS = BİR KARE ARALIĞI (2026-09-05, kayıt 192).
					// Sabit 500 ms, aşağı yöndekiyle AYNI çarpıklığa
					// düşüyordu — sadece ters işaretle: 1080p karesi 540p
					// karesinden kat kat büyük olduğu için aynı yakalama
					// tikinden çıksalar bile üst katman ~300-500 ms SONRA
					// varıyor. Yoklama, alt katmanın karesi gelmiş üst
					// katmanınki gelmemişken denk gelirse üst katman
					// "geride" görünüp sayacı sıfırlıyordu. 1 fps'te sayaç
					// 1-0-1-0 salınıp 2'ye HİÇ ulaşamıyor:
					//
					//	yok. t=0,5: L0=0,0  L1=0,3 → −0,3 ✓ tik=1
					//	yok. t=1,0: L0=1,0  L1=0,3 → +0,7 ✗ tik=0
					//	yok. t=1,5: L0=1,0  L1=1,3 → −0,3 ✓ tik=1
					//
					// Ölçüldü: kayıt 192'de canlı yol üst katmanı t=76,15'te
					// aldı, biz t=78,66'da — kuralın vaat ettiği ~1 sn'nin
					// (2 yoklama) iki buçuk katı.
					//
					// Bir kare aralığı kadar geride olmak ÖLÜLÜK DEĞİL, boyut
					// farkı. 24 fps'te aralık ~42 ms → 500 ms tabanı geçerli,
					// yani KAMERA DAVRANIŞI DEĞİŞMİYOR (histerezis 10 sn).
					// 1 fps'te ~1 sn → sahte sıfırlama kapanıyor.
					tolerans := katmanTazelikFarkı
					if yaz.aralık > tolerans {
						tolerans = yaz.aralık
					}
					if tolerans > katmanGeriKalmaTavanı {
						tolerans = katmanGeriKalmaTavanı
					}
					for k, d := range katmanlar {
						if k <= suAnkiKatman {
							d.uygunTik = 0
							continue
						}
						if yaz.sonPaket.Sub(d.sonPaket) <= tolerans {
							d.uygunTik++
						} else {
							d.uygunTik = 0
						}
					}
				}
				// ── AŞAĞI, HIZLI YOL — CANLI YOLUN TEPKİSİ ───────────
				// (2026-09-04, kayıt 191) Ölçüldü: üst katman sustuğunda
				// canlı yol ALTA ANINDA geçiyor — tarayıcı kopyasında son
				// 1920 karesi ile ilk 960 karesi AYNI DAMGADA, boşluk sıfır,
				// hemen ardından 15 fps sürüyor. Bizde ise `katmanDüşmeEşiği`
				// (3 sn) + anahtar kare beklemesi = 3,5-4 saniyelik DELİK
				// (ölçülen üç düşüşte 4,01 / 3,50 / 4,07 sn). Kullanıcı bunu
				// "katman düşüşlerinde video dondu" diye bildirdi.
				//
				// 3 saniye keyfi değildi: durgun ekranda VP9 1 fps'e kadar
				// iniyor (kayıt 182) ve kısa bir eşik onu "ölmüş" sanardı.
				// O yüzden eşiği KISALTMIYORUZ — ÖLÇÜTÜ değiştiriyoruz.
				//
				// AYIRICI: iki katman da AYNI yakalama tikinde üretiyor,
				// yani ÜRETİM sayıları başa baş gitmek zorunda.
				//   · Durgun ekran (1 fps): ikisi de seyrek ama aynı tikte
				//     → alt katman öne GEÇEMEZ.
				//   · Bant daralması: üst katman büsbütün susar, alt katman
				//     üretmeye devam eder → öndelik birikir.
				// Yani "alt katman, biz son kareyi yazdığımızdan beri iki
				// kare üretti" yanlış alarm üretmeyen bir kanıt.
				//
				// ⚠ ÖLÇÜT SÜREDEN KAREYE ÇEVRİLDİ (2026-09-05, kayıt 192).
				// Süreyle ölçerken 1 fps'lik durgun paylaşımda 9,4 saniyede
				// dört kez katman değişti; gerekçe `katmanÖndeKare`.
				//
				// Çırpınma freni YOK: burası
				// tahmin değil olgu, ve elde akan veri varken delik açmak
				// çırpınmadan her zaman kötü. Fren YUKARI yönde duruyor.
				düştü := false
				if suAnkiKatman > 0 {
					if d := katmanlar[suAnkiKatman]; d != nil &&
						an.Sub(d.sonPaket) >= katmanHızlıDüşmeEşiği {
						if aday := enTazeAltKatman(katmanlar, an,
							suAnkiKatman); aday >= 0 && aday != hedefKatman {
							w.log.Infow("rawrec görüntü: ÜST KATMAN SUSTU, ALT "+
								"KATMAN AKIYOR — beklemeden düşülüyor",
								"track", w.trackID, "sid", w.sid,
								"yazilan_katman", suAnkiKatman, "aday", aday,
								"yazilan_sessizlik",
								an.Sub(d.sonPaket).Round(time.Millisecond),
								"kaynak", kaynakAdı(w.trackInfo))
							hedefBelirle(aday, an)
							düştü = true
						}
					}
				}
				// ── AŞAĞI, YAVAŞ YOL ─────────────────────────────────
				// Hızlı yol "alt katman ŞU AN üretiyor" istiyor. Alt katman
				// da seyrekleşmişse ama yazılan katman büsbütün ölmüşse
				// (`ölüEşiği`) yine düşmek gerekiyor. Ölçüt burada da
				// KARŞILAŞTIRMALI: alt katman yazılanı geçmiş olmalı.
				// İkisi AYNI ANDA sustuysa (durgun ekran / paylaşım
				// duraklatıldı) hiçbir şey yapılmıyor — yazacak görüntü
				// zaten yok, katman değiştirmenin anlamı olmazdı.
				if !düştü && suAnkiKatman > 0 {
					if yaz := katmanlar[suAnkiKatman]; yaz != nil &&
						an.Sub(yaz.sonPaket) >= w.ölüEşiği {
						if aday := enGeridenTazeAltKatman(katmanlar,
							suAnkiKatman); aday >= 0 &&
							aday != hedefKatman {
							hedefBelirle(aday, an)
							düştü = true
						}
					}
				}
				// ── YUKARI ───────────────────────────────────────────
				// Şart: aday katman `w.yukarıTik` yoklamadır kesintisiz
				// "geride değil". Histerezis LiveKit'ten: ekran paylaşımı
				// için kısa (1 pencere ≈ 1 sn), kamera için uzun (10 sn) —
				// kamera sürekli akan bir kaynak, çırpınma riski orada
				// gerçek ve her geçiş yeni anahtar kare demek, yani zaten
				// daralmış uplink'e ek yük.
				if !düştü {
					aday := int32(-1)
					for k, d := range katmanlar {
						if k > suAnkiKatman && d.uygunTik >= w.yukarıTik &&
							k > aday {
							aday = k
						}
					}
					if aday >= 0 && aday != hedefKatman {
						hedefBelirle(aday, an)
					}
				}
				continue
			}
			if vazgeç || ilkPaketAn.IsZero() {
				continue
			}
			h = hedefAra(w.sid, w.log)
			if h == nil {
				// ⚠ KALICI VAZGEÇME YOK — gerekçe rawrec.go'da (kayıt 177).
				if !tamponDurdu && time.Since(ilkPaketAn) > waitFor {
					w.log.Infow("rawrec görüntü hedef hâlâ yok, tampon "+
						"bırakıldı (arama sürüyor)", "track", w.trackID,
						"sid", w.sid, "bekleme", waitFor,
						"bırakılan_paket", len(bekleyen))
					tamponDurdu = true
					sagl.tamponDurdu = true
					bekleyen = nil
					tik.Reset(geçAramaAralığı)
				}
				continue
			}
			w.log.Infow("rawrec görüntü hedef bulundu", "track", w.trackID,
				"kayit", h.RecordingID, "dizin", h.Dir,
				"gecikme", time.Since(ilkPaketAn).Round(time.Millisecond))
			// ⚠ TICKER DURMUYOR — DÜZENLİ ANAHTAR KAREYE geçiyor.
			// Eskiden burada `tik.Stop()` vardı; ilk anahtar kareden sonra
			// bir daha PLI istenmiyordu ve dosyada anahtar kare yalnız
			// yayıncı kendiliğinden gönderirse oluşuyordu. Ölçüldü (kayıt
			// 182): `cam_1.webm` 24,8 sn / 2 anahtar kare (ikisi de t≈0),
			// `share_1.webm` 72,8 sn / 9 anahtar kare — hepsi ilk 21 sn'de,
			// sonra 51 saniye HİÇ. Sonuç: kayıt ARANAMIYOR — tarayıcı
			// uzaktaki anahtar kareden itibaren çözmek zorunda kalıyor,
			// kullanıcı "seek yapınca görüntü donuyor / siyah kalıyor,
			// ses hemen geliyor" diye bildirdi (ses Opus, her paket anahtar).
			// Yoklama sık, İSTEK seyrek: karar kare bütçesine bakıyor ve
			// bütçe hareketli içerikte saniyeler değil kareler içinde
			// dolabiliyor. Ticker'ı bütçe periyoduna bağlamak isteği
			// geciktirirdi.
			if kfEnÇokKare > 0 || kfEnÇokSn > 0 {
				tik.Reset(anahtarKareYoklama)
			} else {
				tik.Stop()
			}
			// ANAHTAR KARE İSTE. Dosya ancak anahtar kareyle açılabiliyor ve
			// ekran paylaşımında anahtar kare çok seyrek — sabit ekranda
			// dakikalarca gelmeyebilir. Hedefi yeni öğrendiysek beklemeyelim.
			w.kaynak(suAnkiKatman).SendPLI(true)
			// Bekleyeni sırayla işle, sonra normal akışa geç.
			kuyruk := bekleyen
			bekleyen = nil
			for _, b := range kuyruk {
				if b.katman != suAnkiKatman {
					continue // katman değişmişse eski paketler atılır
				}
				sira.ekle(b)
			}
			// Biriken tampon TAMAMEN boşaltılıyor: hepsi elimizde, beklemeye
			// gerek yok — ama sıra numarası düzeninde veriliyor.
			boşalt(time.Now(), true)

		case p, ok := <-w.ch:
			if !ok {
				// Akış bitti: sıra tamponunda ne kaldıysa yazılsın.
				boşalt(time.Now(), true)
				bitti = true
				continue
			}
			if vazgeç {
				continue
			}
			// ── İLK PAKET: NE GELDİYSE ONDAN BAŞLA ─────────────────
			// Hedef her hâlükârda ÜST katman; yazılan katman ise ilk gelen.
			// İkisi farklıysa aşağıdaki normal geçiş yolu üst katmanın ilk
			// anahtar karesinde devreye giriyor — dosya kapanmıyor, damga
			// `bölümTaban` ile sürüyor. Böylece paylaşımın başı 540p da olsa
			// KAYDEDİLİYOR; eskiden üst katman gelene kadar hiçbir şey
			// yazılmıyordu (kayıt 188: 6,29 sn).
			if suAnkiKatman < 0 {
				suAnkiKatman = p.katman
				w.suAnki.Store(p.katman)
				hedefKatman = w.üstKatman
				w.hedef.Store(w.üstKatman)
				if p.katman != w.üstKatman {
					w.log.Infow("rawrec görüntü: ÜST KATMAN HENÜZ YOK, ALT "+
						"KATMANDAN BAŞLANIYOR (üst katman gelince aynı "+
						"dosyada yükselinecek)",
						"track", w.trackID, "sid", w.sid,
						"baslangic_katman", p.katman,
						"hedef_katman", w.üstKatman,
						"kaynak", kaynakAdı(w.trackInfo))
					// Üst katmanın anahtar karesini bekletmeyelim: yükselme
					// ancak onunla olabiliyor.
					w.kaynak(w.üstKatman).SendPLI(true)
				}
			}
			// ── HEDEF KATMANIN PAKETİ ──────────────────────────────
			// Yazılmıyor, yalnız ANAHTAR KARE BAŞLANGICI aranıyor.
			// Bulunduğunda geçiş tam o karede yapılıyor ve paket normal
			// yoldan devam ediyor (canlı yolun tasarımı, bkz.
			// `hedefBelirle`). Bulunamazsa atılıyor — yazılan katman
			// kesintisiz akmaya devam ediyor.
			if p.katman != suAnkiKatman {
				if p.katman != hedefKatman || !anahtarKareBaşlangıcı(p) {
					continue
				}
				katmanaGeç(hedefKatman, p.geliş)
			}

			if susmaLogla {
				w.log.Infow("rawrec görüntü: yazılan katman geri geldi",
					"track", w.trackID, "sid", w.sid, "katman", suAnkiKatman,
					"sessizlik", time.Since(sonPaket).Round(time.Millisecond))
				susmaLogla = false
			}
			sonPaket = p.geliş
			sagl.paketGeldi(p.seq)

			// ── 1. Hedef henüz bilinmiyor: beklet ───────────────────────
			if h == nil {
				if ilkPaketAn.IsZero() {
					ilkPaketAn = p.geliş
					w.log.Infow("rawrec görüntü ilk paket geldi, hedef aranıyor",
						"track", w.trackID, "sid", w.sid)
				}
				if !tamponDurdu {
					bekleyen = append(bekleyen, p)
					// Bellek freni: görüntüde saniyede ~2 Mbit akıyor, ses
					// gibi 60 saniye biriktirilemez. Üst sınırı koyup en
					// eskiyi atıyoruz — nasıl olsa dosya ilk ANAHTAR
					// KAREDEN başlayacak.
					if len(bekleyen) > videoBekleyenÜstSınır {
						bekleyen = bekleyen[len(bekleyen)-videoBekleyenÜstSınır:]
					}
				}
				continue
			}

			// ── 2. Normal akış ──────────────────────────────────────────
			// ⚠ DOĞRUDAN `işle`YE VERİLMİYOR. Paketler önce sıra tamponuna
			// giriyor ve oradan SIRA NUMARASI düzeninde çıkıyor; geç gelen
			// tekrar gönderim (RTX) yerine oturuyor. Gerekçe: sira.go.
			sira.ekle(p)
			boşalt(p.geliş, false)

			// SR'yi AKIŞ SÜRERKEN topla (gerekçe: rawrec.go, kayıt 172).
			// Video ~24 fps × ~40 paket = saniyede ~1000 paket → 500'de bir
			// ≈ yarım saniyede bir örnek.
			srSayaç++
			if srSayaç%500 == 0 {
				if sr := w.kaynak(dosyaKatmanı).GetSenderReportData(); sr != nil && sr.NtpTimestamp != 0 {
					sonSR = sr
				}
			}
		}
	}

	// Akış bitti: elde yarım kare kaldıysa ATILIYOR. Yarım kare çözülemez ve
	// dosyanın sonuna bozuk bir kayıt eklemektense eksik bitirmek doğru.
	if başladı && topluyor && len(parça) > 0 {
		w.log.Debugw("rawrec görüntü: akış sonunda yarım kare atıldı",
			"bayt", len(parça))
	}
}

// anahtarKareYoklama — anahtar kare bütçesinin yoklanma sıklığı. İSTEK
// sıklığı bu değil (bkz. kfEnAzSn); bu yalnız kararın ne kadar çabuk
// verildiğini belirliyor.
const anahtarKareYoklama = 500 * time.Millisecond

// ── KATMAN TAKİBİ (2026-09-04) ─────────────────────────────────────────────
//
// Yazıcı artık sabit bir simulcast katmanına bağlı değil. Sebep ölçüldü:
// kayıt 184'te kameranın ÜST katmanı, tam olarak ekran paylaşımı yayındayken
// 196 saniyenin 134'ünde öldü (92 sn + 43 sn iki delik) — alt katman kesintisiz
// akıyor olmasına rağmen kayda o süre boyunca kamera girmedi.
//
// Kural: o an CANLI olan EN ÜST katman yazılır. Katman değişince YENİ DOSYA
// açılır (damga tabanları ortak olmak zorunda değil — bkz. `katmanaGeç`).

// maxKatman — LiveKit'te en fazla spatial katman sayısı; dizi boyutu.
const maxKatman = 4

// katmanBelirsiz — "yazılacak katmana daha karar verilmedi". Yazıcı bu
// değerle kuruluyor; `Write` bu hâldeyken HİÇBİR katmanı süzmüyor ve ilk
// gelen paket kararı veriyor (kayıt 188).
const katmanBelirsiz int32 = -1

type katmanDurum struct {
	sonPaket time.Time // bu katmandan en son ne zaman paket geldi
	kare     uint64    // `Write`ın saydığı toplam kare (yoklamada okunuyor)
	işaret   uint64    // yazılan katman en son kare ürettiğinde `kare` neydi
	uygunTik int       // YUKARI çıkmaya ardışık kaç yoklamadır uygun

	// ÖLÇÜLEN KARE ARALIĞI — yukarı yönün toleransı buradan geliyor
	// (bkz. `katmanGeriKalmaTavanı`). Yoklama penceresinde geçen süre ÷ o
	// pencerede üretilen kare sayısı; yani yoklama aralığından (500 ms)
	// bağımsız, gerçek kare aralığı. 24 fps'te ~42 ms, 1 fps'te ~1 sn.
	aralık         time.Duration
	öncekiKare     uint64
	öncekiSonPaket time.Time
}

// ── KATMAN POLİTİKASI (2026-09-05'te yeniden yazıldı) ──────────────────────
//
// ESKİDEN üç mutlak süre vardı: `katmanCanlıEşiği` (1 sn içinde paket geldiyse
// canlı), `katmanDüşmeEşiği` (3 sn sustuysa düş), `katmanÇıkmaEşiği` (2 sn
// KESİNTİSİZ canlıysa çık). Üçü de KARE HIZINA bağlıydı ve 1 fps'lik durgun
// ekran paylaşımında yanlış cevap veriyordu: alt katmana bir düşünce bir daha
// çıkılamıyordu, çünkü 1 fps'te kareler arası mesafe 1 sn eşiğinin etrafında
// salınıp `canlıdan` sayacını sürekli sıfırlıyordu.
//
// LiveKit'in canlı yolu (`pkg/sfu/streamtracker`) bu işi HIZ ile değil
// PENCERE ile ölçüyor — ekran paylaşımı için "2 saniyede en az 1 paket",
// yani kabul edilen taban 0,5 fps (`DefaultStreamTrackerFrameConfigScreenshare
// .MinFPS = 0.5`). Bizim 1 sn'lik eşiğimiz onun İKİ KATI sertti.
//
// YENİ ÖLÇÜT — KARŞILAŞTIRMA. LiveKit katmanları BAĞIMSIZ izliyor (genel amaçlı
// yazılmışlar). Bizim elimizde onların kullanmadığı bir bilgi var: iki
// simulcast katmanı AYNI yakalama tikinden besleniyor, yani birbirine bağlı.
// Kural artık mutlak hız değil, katmanlar arası fark:
//
//	AŞAĞI  : yazılan katman sustu VE alt katman onu KARE OLARAK GEÇTİ
//	YUKARI : aday katman yazılandan BİR KARE ARALIĞINDAN fazla geride değil,
//	         üstelik N yoklamadır
//
// Aşağı yönde ölçüt SÜRE değil KARE (2026-09-05, kayıt 192) — süreyle ölçmek
// 1 fps'lik durgun paylaşımda çırpınma üretti, gerekçe `katmanÖndeKare`.
//
// Bu ölçüt kare hızından bağımsız — 15 fps'te de, 1 fps'te de, 0,2 fps'te de
// aynı çalışıyor. LiveKit'in penceresine gömülü 0,5 fps tabanı bizde yok.
//
// HİSTEREZİS ise LiveKit'ten alındı ve KAYNAK TİPİNE göre ayrıldı (bizde hiç
// yoktu): yukarı çıkmak anahtar kare istemek demek, anahtar kare de zaten
// daralmış uplink'e ek yük. Ekran paylaşımında acele etmenin karşılığı var
// (metin okunuyor), kamerada yok.

// katmanTazelikFarkı, katmanMeşgulEşiği, katmanHızlıDüşmeEşiği aşağıda.

// ölüEşiğiPaylaşım / ölüEşiğiKamera — yazılan katman bu kadar susarsa YAVAŞ
// aşağı yol devreye giriyor. Paylaşım için 3 sn ≈ LiveKit'in 2-4 sn'si;
// kamera sürekli akan bir kaynak, orada 1 sn yeterli (LiveKit 500 ms'lik
// pencere kullanıyor).
const ölüEşiğiPaylaşım = 3 * time.Second
const ölüEşiğiKamera = 1 * time.Second

// yukarıTikPaylaşım / yukarıTikKamera — yukarı çıkmak için gereken ARDIŞIK
// uygun yoklama sayısı. Yoklama `anahtarKareYoklama` (500 ms) aralıklı.
//
//	· paylaşım: 2 tik ≈ 1 sn — LiveKit'in `CyclesRequired: 1` × 2 sn'lik
//	  penceresinin karşılığı, biraz daha atik.
//	· kamera: 20 tik = 10 sn — LiveKit'in üst katman için verdiği sayının
//	  (`CyclesRequired: 20` × 500 ms) birebir aynısı.
const yukarıTikPaylaşım = 2
const yukarıTikKamera = 20

// katmanHızlıDüşmeEşiği — hızlı düşüş yolunun ilk şartı: yazılan katman en
// az bu kadar susmuş olmalı. Yoklama zaten 500 ms'de bir (`anahtarKareYoklama`),
// yani karar en geç ~1,2 saniyede veriliyor — canlı yola yakın.
const katmanHızlıDüşmeEşiği = 700 * time.Millisecond

// katmanTazelikFarkı — YALNIZ YUKARI yönde kullanılıyor: aday üst katman,
// yazılandan bu kadar geride değilse "uygun" sayılıyor ve `uygunTik` artıyor.
//
// ⚠ AŞAĞI yönde ARTIK KULLANILMIYOR — orada yerini `katmanÖndeKare` aldı,
// gerekçesi onun başlığında.
//
// ⚠ ARTIK TEK BAŞINA DEĞİL, TABAN. Gerçek tolerans yazılan katmanın ÖLÇÜLEN
// kare aralığı; bu sabit onun altına inmemesini sağlıyor (24 fps'te aralık
// ~42 ms, o kadar dar bir pencere sıradan varış jitter'ıyla bile sıfırlanırdı).
// Tavanı `katmanGeriKalmaTavanı`. Gerekçe: yoklama döngüsündeki "TOLERANS =
// BİR KARE ARALIĞI" başlığı.
const katmanTazelikFarkı = 500 * time.Millisecond

// katmanÖndeKare — hızlı ve yavaş düşüşün ASIL şartı: yazılan katmanın son
// karesinden bu yana alt katman kaç KARE öne geçmiş olmalı.
//
// ⚠ BU ESKİDEN SÜREYDİ (`katmanTazelikFarkı`, 500 ms) VE YANLIŞ ALARM VERDİ
// (2026-09-05, kayıt 192). Eski gerekçe şuydu: iki simulcast katmanı aynı
// yakalama tikinden beslendiği için durgun ekranda son paketleri milisaniye
// arayla gelir, fark 500 ms'e çıkmaz. ÖLÇÜM bunu çürüttü: 1080p karesi 540p
// karesinden kat kat büyük, dar bantta AYNI TİKTEN çıkan iki kare SFU'ya
// yarım saniyeden fazla arayla varıyor. Sonuç: durgun, 1 fps'lik bir
// paylaşımda üç şart birden sağlandı ve yazıcı 9,4 saniyede DÖRT kez katman
// değiştirdi (t = 95,9 / 99,1 / 104,1 / 105,3 sn). Bedeli 3,02 saniyelik
// delik — dosyanın en büyüğü — ve dört gereksiz 1080p anahtar karesi
// (59-75 KB), ki PLI bütün abonelere gidiyor.
//
// Kare saymak kare hızından bağımsız. "Alt katman iki kare üretti, biz hiç
// üretmedik" ancak katmanlar GERÇEKTEN ayrıştığında olur:
//
//	· durgun ekran → ikisi de aynı tikte üretir, alt katman öne geçemez,
//	  varış sırası ne olursa olsun → yanlış alarm YOK;
//	· bant daralması → üst katman büsbütün susar, alt katman üretmeye devam
//	  eder → iki kare farkı 15 fps'te ~130 ms'te, 1 fps'te 2 sn'de açılır.
//
// Yani eşik kendiliğinden ölçekleniyor; `katmanHızlıDüşmeEşiği` (700 ms)
// mutlak taban olarak duruyor ki yüksek fps'te titremeyle tetiklenmesin.
//
// 2, 1 değil: tek kare öndelik, tik sınırına denk gelen sıradan bir varış
// sırası farkından da doğabilir.
const katmanÖndeKare = 2

// katmanGeriKalmaTavanı — yukarı yöndeki toleransın üst sınırı. Tolerans
// yazılan katmanın ÖLÇÜLEN kare aralığından geliyor; sınırsız bırakılırsa
// yazılan katman çok seyrekleştiğinde (0,05 fps) tolerans 20 saniyeye çıkar
// ve neredeyse ölü bir üst katman "geride değil" sayılırdı.
//
// 4 sn: 0,25 fps'e kadar iner. Ondan seyrek bir kaynakta zaten yukarı
// çıkmakta acele etmenin karşılığı yok.
const katmanGeriKalmaTavanı = 4 * time.Second

// katmanMeşgulEşiği — hızlı düşüşün ÜÇÜNCÜ şartı: alt katman ŞU AN üretiyor
// olmalı (son paketi bu kadar yakın).
//
// ⚠ NEDEN ÜÇÜNCÜ ŞART GEREKLİ. Tek başına `katmanTazelikFarkı` bir deliğe
// açık: durgun ekranda alt katman tek seferlik bir tazeleme karesi
// gönderir de üst katman göndermezse fark bir anda 500 ms'i aşar ve öylece
// DURUR — iki katman da susmaya devam ettiği için bir daha kapanmaz. Sonuç:
// hiçbir sorun yokken 540p'ye düşer ve durgunluk sürdükçe orada KALIRDI
// (yukarı dönüş, üst katmanın `katmanÇıkmaEşiği` boyunca KESİNTİSİZ canlı
// olmasını istiyor; 1 fps'lik ekranda bu şart zor sağlanıyor).
//
// Üçüncü şart bunu kapatıyor.
//
// ⚠ BURADA ESKİDEN "1. ile 3. şart durgun ekranda BİRBİRİNİ DIŞLAR" yazıyordu
// (iki katman aynı tikte ürettiği için biri sağlanınca öteki düşer sanılmıştı).
// Kayıt 192 bunu çürüttü: dar bantta 1080p karesi 540p karesinden yarım
// saniyeden fazla geç varıyor, yani ikisi de sağlanabiliyor. Durgun ekranı
// koruyan şey artık 2. şart (`katmanÖndeKare`); bu üçüncü şart yalnız kendi
// işini yapıyor — tek seferlik tazeleme karesini elemek.
const katmanMeşgulEşiği = 500 * time.Millisecond

// enTazeAltKatman — `suAnki`nin ALTINDA olup İKİ şartı birden sağlayan en üst
// katman. Yoksa -1.
//
//	① ŞU AN üretiyor  : an - sonPaket <= katmanMeşgulEşiği
//	② yazılanı geçmiş : kare - işaret >= katmanÖndeKare
//
// Gerekçe: `katmanÖndeKare` ve `katmanMeşgulEşiği` başlıkları.
func enTazeAltKatman(m map[int32]*katmanDurum, an time.Time, suAnki int32) int32 {
	en := int32(-1)
	for k, d := range m {
		if k >= suAnki || d == nil {
			continue
		}
		// ① ŞU AN üretiyor mu — tek seferlik tazeleme karesini eler.
		if an.Sub(d.sonPaket) > katmanMeşgulEşiği {
			continue
		}
		// ② yazılan katmanı KARE olarak geçmiş mi.
		if d.kare-d.işaret >= katmanÖndeKare && k > en {
			en = k
		}
	}
	return en
}

// enGeridenTazeAltKatman — YAVAŞ aşağı yol için aday: `suAnki`nin altında
// olup ondan `katmanÖndeKare` kadar öne geçmiş en üst katman. Yoksa -1.
//
// Hızlı yoldan (`enTazeAltKatman`) tek farkı, adayın ŞU AN üretiyor olmasını
// istememesi: yazılan katman `ölüEşiği` kadar susmuşsa, altta bir süre önce
// üretmiş bir katman da yeterli kanıttır.
func enGeridenTazeAltKatman(m map[int32]*katmanDurum, suAnki int32) int32 {
	en := int32(-1)
	for k, d := range m {
		if k >= suAnki || d == nil {
			continue
		}
		if d.kare-d.işaret >= katmanÖndeKare && k > en {
			en = k
		}
	}
	return en
}

// üstKatmanSusmaEşiği — üst simulcast katmanından bu kadar süre paket
// gelmezse günlüğe bir kez UYARI düşüyor. 3 sn: durgun ekranda VP9 zaten
// 1 fps'e kadar inebiliyor (ölçüldü, kayıt 182: 45-72 sn arası tam 1 fps),
// o yüzden eşik ondan rahat büyük.
const üstKatmanSusmaEşiği = 3 * time.Second

// videoBekleyenÜstSınır — hedef gelene kadar tutulacak en fazla paket.
// 24 fps × ~40 paket/kare × ~10 sn ≈ 10k. Paket ~1200 bayt → ~12 MB tavan.
const videoBekleyenÜstSınır = 10000

// anahtarKareBaşlangıcı — bu paket bir ANAHTAR KARENİN İLK parçası mı?
//
// Geçiş noktası tam olarak burasıdır: karenin ortasından geçilirse çözücü
// görmediği bir görüntüye atıf yapan delta kareler alır. Kare TOPLANMIYOR,
// yalnız VP9 başlığındaki B (kare başlangıcı) biti ile paketin anahtar kare
// bayrağına bakılıyor — hedef katman için ayrı bir toplayıcı tutmaya gerek
// kalmıyor (katmanların sıra numarası uzayları da ayrı, tek sıra tamponu
// ikisini birden düzenleyemezdi).
func anahtarKareBaşlangıcı(p vpaket) bool {
	if !p.anahtar {
		return false
	}
	var vp9 codecs.VP9Packet
	if _, err := vp9.Unmarshal(p.payload); err != nil {
		return false
	}
	return vp9.B
}

// işle — bir RTP paketini kare toplayıcıya ver; kare tamamlanınca yaz.
//
// VP9 payload descriptor'ı B (kare başlangıcı) ve E (kare sonu) bitlerini
// taşıyor. Kare = B'den E'ye kadar olan paketlerin yüklerinin, başlıkları
// soyulmuş hâlde ardışık birleşimi.
func (w *VideoWriter) işle(p vpaket, kopuk bool, parça *[]byte, topluyor *bool,
	parçaRTP *uint32, parçaAnahtar *bool, parçaBozuk *bool, sagl *saglik,
	kareYaz func([]byte, uint32, bool, time.Time)) {

	// atla — yarım kareyi bırak. `bozuk` true ise kare eksik parçayla
	// kapanmak üzereydi, sayaca yazılıyor (sağlık raporuna giriyor).
	atla := func(bozuk bool) {
		if bozuk && *topluyor {
			sagl.kareAtildi()
		}
		*topluyor = false
		*parçaBozuk = false
		*parça = (*parça)[:0]
	}

	var vp9 codecs.VP9Packet
	veri, err := vp9.Unmarshal(p.payload)
	if err != nil || len(veri) == 0 {
		// Çözülemeyen paket: içinde bulunduğu kareyi de bozar, toplamayı
		// iptal et. Bir sonraki B bitinde temiz başlarız.
		atla(true)
		return
	}

	// Damga değiştiyse önceki kare bitmemiş demektir (E biti kaybolmuş ya da
	// paket düşmüş) — yarım kareyi at, yenisine geç.
	if *topluyor && p.rtp != *parçaRTP {
		atla(true)
	}

	if vp9.B {
		*topluyor = true
		*parçaBozuk = false
		*parça = (*parça)[:0]
		*parçaRTP = p.rtp
		*parçaAnahtar = p.anahtar
	} else if kopuk && *topluyor {
		// ⚠ BÜTÜNLÜK KAPISI (2026-09-04, kayıt 185). Bu paketten hemen önce
		// bir paket eksik → karenin ortasında delik var. Eskiden yazılıyordu
		// ve Chrome'un çözücüsü o kareyi görünce KALICI olarak ölüyordu
		// (PIPELINE_ERROR_DECODE) — kalan bütün kareler dosyada dursa bile
		// ekranda donuk kare kalıyordu. Bozuk kare yazmaktansa kareyi hiç
		// yazmamak doğru: bir kare eksilir, kayıt izlenebilir kalır.
		*parçaBozuk = true
	}
	if !*topluyor {
		return // kare ortasından geldik, B bekliyoruz
	}
	*parça = append(*parça, veri...)
	if p.anahtar {
		*parçaAnahtar = true
	}

	// Kare sonu: E biti ya da marker. İkisi de aynı şeyi söylüyor ama
	// yayıncılar birini boş bırakabiliyor; ikisine de bakıyoruz.
	if vp9.E || p.marker {
		if *parçaBozuk {
			atla(true)
			return
		}
		kareYaz(*parça, *parçaRTP, *parçaAnahtar, p.geliş)
		atla(false)
	}
}

func (w *VideoWriter) dosyaAç(h *hedef, n int) (*os.File, string, error) {
	d := filepath.Join(h.Dir, videoKlasörAdı(w.trackInfo, h.RecordingID))
	if err := os.MkdirAll(d, 0o755); err != nil {
		return nil, "", err
	}
	// "sfu_" öneki — gerekçe rawrec.go `dosyaAç`ta.
	yol := filepath.Join(d, fmt.Sprintf("%02d_sfu_%s.ivf", n, string(w.trackID)))
	fh, err := os.Create(yol)
	if err != nil {
		return nil, "", err
	}
	w.log.Infow("rawrec görüntü dosyası açıldı",
		"track", w.trackID, "kaynak", kaynakAdı(w.trackInfo), "yol", yol,
		"açılış_ns", mono.UnixNano())
	return fh, yol, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
