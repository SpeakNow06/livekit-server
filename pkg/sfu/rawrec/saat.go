package rawrec

// ── SES SAAT EŞLEYİCİ (Adım 2, 2026-09-21) ──────────────────────────────────
//
// NEDEN VAR — kayıt 911: mikrofon susturulunca (`stopMicTrackOnMute`, web ve
// mobilde açık) tarayıcı track'i durduruyor, kodlayıcıya örnek girmiyor ve
// yayıncının RTP SAATİ DURUYOR; açınca kaldığı yerden sürüyor. Ses yazıcısı
// granülü saf RTP'den kurduğu için her susturma süresi dosyadan siliniyordu:
// öğrenci A 8 susturmada 115,0 sn, öğrenci B 135,7 sn kaybetti; sonrası o
// kadar erkene kaydı. Öğretmen hiç susturmadığı için doğruydu. (Video
// etkilenmiyor: görüntü damgası yakalama duvar saatinden türüyor.)
//
// LiveKit'in kendi abone yolu bunu çözüyor (`forwarder.go processSourceSwitch`,
// "mute valley" yorumu): beklenen damga duvar saatinden, gelen damga RTP'den;
// fark eşiği aşarsa duvar saati kazanır (mute sinyali varsa 0,2 sn, yoksa
// 2 sn). Bot tarayıcı kopyası bu yüzden doğruydu. Bu dosya aynı kuralı ses
// yazıcısına taşıyor — video yazıcının "bölüm tabanı"nın (video.go) ses
// karşılığı. Plan ve kanıt: speaknow-server
// docs/split-recording/SES-SAATI-DURAKLAMASI-PLANI.md.
//
// MODEL
//   · BÖLÜM = kesintisiz koşu. Bölüm içinde pts = taban + (rtp − rtp0): RTP
//     sayacı ağ titremesinden bağımsız, hassas.
//   · Yeni bölüm: varış farkı − RTP farkı > eşik (durmuş saat). DTX'te ve
//     paket kaybında RTP duvarla birlikte yürür → fark ≈ 0 → bölüm açılmaz
//     (Janus'un üçlü ayrımı: kayıp / DTX / durmuş saat).
//   · Taban önce VARIŞ farkından (geçici), bölümün ilk Sender Report'u gelince
//     SR'dan (kesin). Yazıcı ~6 sn geriden yazdığı için (rawrec.go, yazma
//     gecikmesi) kesinleşme çoğu zaman dosyaya yazılmadan önce oluyor;
//     olmadıysa değer yan JSON'a `sr_eksik_sn` olarak düşüyor, dosya
//     içinde tutarlılık bozulmuyor.
//   · Çapa (pts 0'ın sunucu saati) ilk bölümün ilk SR'ından; sonraki bölümler
//     aynı çapaya göre SR ile kesinleşiyor.
//
// ⚠ Eşleyici yazıcının döngü goroutine'inde çalışır; kilit yok.

import (
	"fmt"
	"time"

	"github.com/livekit/protocol/livekit"
)

// Eşikler — LiveKit forwarder.go ile aynı (ResumeBehindThresholdSeconds 0,2 /
// ResumeBehindHighThresholdSeconds 2,0).
const (
	bolumEsikSinyalli  = 200 * time.Millisecond // yayıncı mute sinyali görüldüyse
	bolumEsikSinyalsiz = 2 * time.Second        // görülmediyse (kuyruk gecikmesiyle karışmasın)
	// bayatEsik — geçici bölümün ilk paketlerinde RTP duvardan bu kadar ÖNE
	// geçerse ilk paket bayattı (LiveKit `isPacketTooOld`): taban ona değil
	// bu pakete kurulur.
	bayatEsik = 500 * time.Millisecond
	// erkenBoslukEsik — geçici bölümün ilk paketlerinde varış RTP'den bu
	// kadar GERİ kalırsa (ilk paket erken/tek gelmiş, akış sonra başlamış)
	// taban o pakete taşınır (bayat kuralının aynası). Safari açılışta böyle:
	// kayıt 917'de SR tabanı 203 ms düzeltti, 918 başlangıcında 2 paket +
	// 203 ms boşluk + akış görüldü. Ağ titremesi ≤ 40 ms, DTX boşlukları
	// RTP'yle tutarlı → tetiklemez. SR gelmese de taban doğru kalır.
	erkenBoslukEsik = 100 * time.Millisecond
	// srToleransSn — SR'dan hesaplanan taban geçici tabandan bu kadar
	// uzaksa SR'a güvenilmez (bozuk SR: crbug 168328), geçici kalır.
	srToleransSn = 10.0
	// srTekToleransSn — çapa TEK SR'a dayanıyorsa (doğrulanmamış) izin
	// verilen düzeltme payı: varış tahmini ≤ 40 ms şaşar, bozuk SR
	// saniyelerce — dar pay bozuğu eler (rawrec45, kayıt 915 dersi).
	srTekToleransSn = 0.5
	// RAPOR BASAMAĞI (rawrec52, Firefox 923): damgasız akışta tek tanık SR.
	// Kesin bir bölümde SR'ın söylediği taban ile RTP'ye göre yer arasındaki
	// fark izlenir; SON ÜÇ SR'ın ortalaması ÖNCEKİ ÜÇÜN ortalamasından
	// srAdimEsikSn'den fazla ayrılır ve iki grup kendi içinde srAdimYayilimSn
	// içinde tutarlıysa yayıncının sayacı basamak atlamıştır (açılışta bir
	// karelik yeniden tabanlama). ⚠ "Art arda iki SR 10 ms" DENENDİ: Chrome'un
	// SR'ları kendiliğinden ±15 ms salınıyor (911 öğretmeni: −16…+3 ms, std
	// 6,5) → 72 yanlış tetik. Grup kuralı 911'de 0, Firefox 923'te 1 (−20,7 ms).
	// Damgalı akışta hiç çalışmaz (`actPaket > 0`): damga 10 ms'de görür.
	srAdimEsikSn    = 0.015
	srAdimYayilimSn = 0.010
	srAdimGrup      = 3
	// hicEsik — basamağın YERİ: son uyumlu SR'dan beri varışın RTP'den en
	// çok geri kaldığı paket (açılış hıçkırığı) bunu aşıyorsa bölme oraya,
	// yoksa "şimdi"ye (arada en çok bir SR aralığı 20 ms şaşar).
	hicEsik = 10 * time.Millisecond
	// ozDenetimEsikSn — SR duvar süresi ile yazılan süre farkı bunu aşarsa
	// dosya "zaman-tutarsız" (Janus'un 0,5 sn uyarısı).
	ozDenetimEsikSn = 0.5
)

// sesBolum — kesintisiz bir koşunun dosya içindeki yeri. Yan JSON'a
// `bolumler[]` olarak yazılıyor.
type sesBolum struct {
	Sira         int      `json:"sira"`
	RTP0         uint32   `json:"rtp0"`                  // bölümün ilk paketinin RTP damgası
	Pts0         int64    `json:"pts0"`                  // o paketin dosya içindeki yeri (48 kHz örnek)
	Kaynak       string   `json:"kaynak"`                // ilk | varis | mute+varis | …+sr
	VarisEksikSn float64  `json:"varis_eksik_sn"`        // varış farkından ölçülen eksik (sn)
	SrEksikSn    *float64 `json:"sr_eksik_sn,omitempty"` // SR ile kesinleşen eksik (sn)
	Kesin        bool     `json:"kesin"`                 // taban SR ile kesinleşti mi
	MuteSinyali  bool     `json:"mute_sinyali"`          // yayıncı mute sinyaliyle mi açıldı
	IlkGelisNs   int64    `json:"ilk_gelis_ns"`          // ilk paketin sunucuya varışı
	Paket        int      `json:"paket"`                 // bölümdeki paket sayısı
	// IlkPaketler — bölümün ilk 16 paketi: [varış ms (ilk pakete göre),
	// RTP ms (RTP0'a göre)]. TEŞHİS (rawrec46, kayıt 917): Safari'de SR
	// düzeltmesi 203 ms çıktı (Chrome 9,5 ms); açılış sonrası ilk paketler
	// patlama mı, gecikmeli mi geliyor, buradan okunur.
	IlkPaketler [][2]float64 `json:"ilk_paketler,omitempty"`

	sonRTP     uint32
	sonPts     int64
	sonGelisNs int64 // bölümün son paketinin varışı (SR'ın hangi bölüme ait olduğu bununla)
	yazildi    bool  // bölümden en az bir paket dosyaya yazıldı (artık taban değişemez)
	actGecici  bool  // damgayla açıldı, adımı küçük: bir sonraki damga doğrulayana kadar GEÇİCİ (rawrec56)
	actDuz     int   // geçici act bölümünde yapılan taban düzeltmesi sayısı (rawrec56)
}

// srPencereTolerans — SR, bölümün son paketinden en çok bu kadar sonra
// geldiyse o bölüme aittir. DTX sessizliğinde paket 400 ms'de bir geliyor
// (ölçüldü: en büyük boşluk 0,43 sn), o aralıkta gelen SR tutarlı (sayaç
// yürüyor). Daha geç gelen SR susturma İÇİNDEN geliyordur → atılır.
const srPencereTolerans = 500 * time.Millisecond

// sesSaat — bkz. dosya başlığı.
type sesSaat struct {
	hz       int64
	bolumler []*sesBolum

	var_     bool
	sonRTP   uint32
	sonGelis time.Time
	sonSeq   uint16
	sonPts   int64
	enSonPts int64 // atanmış en büyük pts (RED yedek kopyası ayıklaması)

	muteBekliyor   bool    // PubMute(true) görüldü, henüz bir bölüm açmadı
	anchorNs       int64   // pts 0'ın SUNUCU saati (unix ns); 0 = bilinmiyor. Yan JSON çapası.
	anchorKaynakNs int64   // pts 0'ın YAYINCI saati (SR NTP'si, unix ns). Bölüm kesinleştirme.
	anchorKesin    bool    // iki uyumlu SR'la doğrulandı
	anchorAday     []int64 // ilk bölümün SR'larından sunucu-saati adayları (bkz. srGeldi)
	anchorAdayK    []int64 // aynı SR'ların yayıncı-saati adayları (aynı sıra)

	srSonNTP uint64
	// Rapor basamağı (rawrec52): son SR sapmaları (örnek, en çok 2×grup) ve
	// her SR aralığının en büyük varış hıçkırığı (bölme noktası adayı).
	srSapma   []int64
	hicAra    []hicKaydi
	hicSu     hicKaydi
	srAdim    int
	gunluk    []srOrnek // SR günlüğü, pts'li
	srBozuk   int       // toleransı aşan SR sayısı
	srAtlanan int       // hiçbir bölümün penceresine düşmeyen SR (susturma içi)
	bayat     int       // bayat ilk paket düzeltmesi sayısı
	erken     int       // erken ilk paket düzeltmesi sayısı (rawrec47)

	// ── YAKALAMA SAATİ (abs-capture-time, Adım 6) ────────────────────────
	// Paketin başlığındaki yakalanma anı yayıncının kendi saati: ağ titremesi
	// yok, SR beklemek yok, sinyal gerekmez. Varsa "0. YOL": Δyakalama −
	// Δrtp > 100 ms → durmuş saat, taban kesin. Chrome dolduruyor; Safari ve
	// Firefox libwebrtc'ye yakalama zamanı vermiyor (uzantı pazarlansa da
	// pakete girmez) → orada varış/SR yolu sürer. Mobil (react-native)
	// zaten durmuyor.
	sonYak       int64         // son (yedek olmayan) paketin yakalanma anı, 0 = yok
	sonYakRTP    uint32        // o paketin RTP'si
	sonYakPts    int64         // o paketin dosyadaki yeri: yakalama saatiyle yer = sonYakPts + Δyakalama
	sonYakBolum  int           // o paketin bölümü (SR kayması olursa sonYakPts de kayar; referans bölümden önceyse geçici bölüm kesinleşir)
	actPaket     int           // yakalama saatli paket sayısı
	actG         actGunluk     // damga günlüğü (pts ↔ yakalama anı), yan JSON `act_gunlugu`
	actBayat     int           // bayat yakalama saati: varış "durdu" dedi, damga onaylamadı (rawrec49, kayıt 920)
	sonYakGelis  time.Time     // referans damgalı paketin VARIŞ anı (gelecek damga kuralı, olay 937)
	actGelecek   int           // gelecek damga: damga, varışın görmediği bir duraklama iddia etti → reddedildi
	actRedArd    int           // art arda reddedilen gelecek damga; actRedTaban'da referans yeniden tabanlanır
	actYeniTaban int           // yayıncı saati sıçradı sayılıp referansın yeniden tabanlandığı sayı
	actRed       []actRedOrnek // reddedilen gelecek damgalar (ilk actRedGunlukUst tanesi), yan JSON `act_red_gunlugu`
	actSupheli   int           // referansın çok gerisinde kalan damga → referans bozuk sayılıp yeniden tabanlandı
	paketSayisi  int           // yedek dışı toplam paket
	actGeriArd   int           // art arda uygulanmayan GERİ damga (referans korundu) — rawrec56
	actGeriSon   int64         // son uygulanmayan geri damganın kayması (örnek)
	actGeriTaban int           // kalıcı geri adım: art arda uyumlu geri damgayla referans yeniden tabanlandı
	actGeriAtlan int           // tek paketlik geri damga: uygulanmadı, referans korundu (yan JSON act_geri_atlanan)
	actGeciciDuz int           // geçici act bölümünün tabanı sonraki damgayla düzeltildi/geri alındı
}

// actEsik — damgayla ölçülen fark bunu aşarsa bölüm açılır (ya da geçici
// bölüm kesinleşir). Damgada ağ titremesi yok; ölçülen gürültü ±1 ms (Chrome
// enterpolasyon hatası > 1 ms olunca yeni damga yollar, damga çözünürlüğü
// 1 ms). 100 ms → 10 ms (rawrec51, kayıt 927): iOS uygulaması açılışta RTP'yi
// duvar saatine yeniden tabanlıyor ama bir kare (20 ms) eksik — damga serisi
// +20 ms basamak gösterdi, 100 ms eşiği bunu görmüyordu. Firefox'ta (923)
// aynı basamak var ama damga yok → orada yakalanamıyor.
const actEsik = 10 * time.Millisecond

// actGelecekEsik — GELECEK DAMGA kuralı (olay 937; kayıtlar 932/933/937 bozuldu).
// Chromium'un breakout box'ı (MediaStreamTrackProcessor → Worker → Generator)
// zincir her kurulduğunda ilk 1-3 parçayı SAYFA YAŞI kadar ileri damgalıyor
// (sayfa-göreli damga, Worker realm'inde okunuyor); libwebrtc ACM ilk 10 ms
// çerçevenin damgasını bütün pakete yapıştırdığı için açılışın İLK paketi
// dakikalarca "gelecekte yakalanmış" görünüyor (937: +1240 sn, 933: +196-496 sn).
// Yazıcı buna güvenip dosyaya o kadar sessizlik ekliyordu.
//
// Ölçüt: damganın referanstan beri iddia ettiği ilerleme (kayma) ile varışın
// aynı aralıkta gördüğü duraklama (Δvarış − Δrtp) karşılaştırılır. Gerçek
// susturmada ikisi eşit (± ağ titremesi); ağ kesintisinde ikisi de ~0; gelecek
// damgada kayma = susturma + sayfa yaşı. Fark bu eşiği aşarsa damga reddedilir,
// referans GÜNCELLENMEZ, paket varış yoluna düşer; 20-60 ms sonra gelen doğru
// damga geçici bölümü kesinleştirir (bayat damga kuralının aynası, rawrec49).
// 500 ms: titremenin çok üstü, sayfa yaşının çok altı (zincir sayfa yüklendikten
// en az birkaç saniye sonra kurulabilir).
const actGelecekEsik = 500 * time.Millisecond

// actRedTaban — art arda bu kadar damga "gelecek" diye reddedilirse yayıncının
// saati SIÇRAMIŞ sayılır (NTP düzeltmesi vb.) ve referans bu paketin varış
// yoluyla bulunan yerine yeniden tabanlanır; aksi hâlde yakalama saati yolu
// kaydın sonuna kadar ölü kalırdı. Gelecek damga olayı tek pakettir: gönderici
// (libwebrtc AbsoluteCaptureTimeSender) enterpolasyon hatası > 1 ms görünce
// doğru damgayı hemen yollar, sayaç sıfırlanır.
const actRedTaban = 5

// actGeriEsik — damga, referansa göre bundan fazla GERİDEYSE referans bozuktur
// (dosyanın ilk damgası gelecekteydi ve referans oldu; bir sonraki damgadan
// önce susturma gelirse geçici bölüm bu damgayla dakikalarca GERİ kayar, pts
// negatife düşer, granül taşar — denetim T1). Damga uygulanmaz, referans bu
// pakete taşınır (`act_supheli`). Meşru geri düzeltmeler (bayat ilk paket
// ≤ 500 ms, titreme) bunun çok altında.
const actGeriEsik = 10 * time.Second

// ── rawrec56: TEK PAKETLİK DAMGA TİTREMESİ (kayıt 940, Android) ──────────────
// Android libwebrtc (WebRtcAudioRecord) her 10 ms okumada `AudioRecord.
// getTimestamp().nanoTime`'ı okunan tampona göre düzeltmeden damga yapar;
// AudioFlinger veriyi damgadan önce yayımladığı için okuma bazen bir HAL
// periyodu BAYAT damga alır (tek paket ~18-20 ms ERKEN: "çukur"), okuma iş
// parçacığı gecikince de ilk paket yeni damgayı alır (tek paket +20 ms: "tepe").
// Sonraki paket hep eski çizgiye döner; gönderici (AbsoluteCaptureTimeSender,
// enterpolasyon hatası > 1 ms) sapan paketi de dönüşü de damgalı yollar.
// Kullanıcı eylemiyle ilgisi yok; bütün Android cihazlarda olur (940'ta 86 sn'de
// 41 çukur + 5 tepe). Eski kod çukurdaki paketi referans yapıp dönüşü "durmuş
// saat" sayıyor, her çukur için 18 ms kalıcı boşluk ekliyordu (57 bölüm,
// −0,57 sn, oz "zaman-tutarsiz" → SFU kopyası elenip tarayıcı yedeği seçildi).
// Fiziksel gerekçe: gerçek yakalama çizgisi basamak ATLAYABİLİR (duraklama,
// xrun) ama asla inip geri dönemez; o yüzden geri damga kalıcı olmadıkça yok
// sayılır, küçük ileri adım ise bir sonraki damga onaylayana kadar geçicidir.
// Testler: audio_geri_damga_test.go (gerçek 940 dizisi dahil).

// actGeriArdTaban — referansa göre GERİ (negatif, eşik üstü) damga UYGULANMAZ ve
// referans korunur; bir sonraki paket eski çizgide → kayma 0. Art arda bu kadar
// damga aynı geri kaymayı (±actGeriUyum) gösterirse kalıcı geri adımdır
// (yayıncı saati geri sıçradı): referans o pakete taşınır (`act_geri_taban`).
const actGeriArdTaban = 3
const actGeriUyum = 3 * time.Millisecond

// actGeciciUst — damgayla açılan bölümün adımı bundan küçükse bölüm GEÇİCİDİR:
// bir sonraki damgalı paket adımı doğrular (kayma ≈ 0), düzeltir (küçük
// kayma: taban kaydırılır, kuyruk onunla kayar) ya da geri alır (tek paketlik
// tepe: kayma ≈ −adım → bölüm eski çizgiye çekilir, boşluk ~0). Daha büyük
// adımlar (gerçek duraklama) eskisi gibi hemen kesindir. Bölüm `Kesin: true`
// kalır: SR dokunamaz; yalnız yakalama damgası düzeltir.
const actGeciciUst = 250 * time.Millisecond

// actGeciciDuzUst — geçici bölümde en çok bu kadar düzeltme (merdiven: okuma iş
// parçacığı takılınca +100 sonra −20×5; sonsuz salınım kilidi).
const actGeciciDuzUst = 8

// actRedOrnek — reddedilen bir damganın kaydı: paketin RTP'ye göre yeri, ham
// damga, damganın duvar saatini ne kadar aştığı ve RTP'ye göre iddia ettiği
// kayma. Yayın sonrası izleme için (denetim önerisi).
type actRedOrnek struct {
	Pts       int64 `json:"pts"`
	YakNs     int64 `json:"yak_ns"`
	GelecekMs int64 `json:"gelecek_ms"`
	KaymaMs   int64 `json:"kayma_ms"`
}

const actRedGunlukUst = 20

func (s *sesSaat) actRedKaydet(pts, yakNs int64, gelecek time.Duration, kayma int64) {
	if len(s.actRed) >= actRedGunlukUst {
		return
	}
	s.actRed = append(s.actRed, actRedOrnek{Pts: pts, YakNs: yakNs,
		GelecekMs: gelecek.Milliseconds(), KaymaMs: kayma * 1000 / s.hz})
}

func yeniSesSaat(hz uint32) *sesSaat {
	return &sesSaat{hz: int64(hz)}
}

// sdelta — 32 bit RTP farkı, İŞARETLİ (sarma güvenli).
func sdelta(a, b uint32) int64 { return int64(int32(a - b)) }

func (s *sesSaat) sureOrnek(d time.Duration) int64 {
	return d.Nanoseconds() * s.hz / int64(time.Second)
}

func (s *sesSaat) ornekSn(n int64) float64 { return float64(n) / float64(s.hz) }

func (s *sesSaat) simdiki() *sesBolum {
	if len(s.bolumler) == 0 {
		return nil
	}
	return s.bolumler[len(s.bolumler)-1]
}

// muteSinyali — yayıncı track'i susturdu (ReceiverBase.UpdateTrackInfo →
// Writer.PubMute). Bir sonraki bölüm açılışında eşik 0,2 sn'ye iner.
func (s *sesSaat) muteSinyali() { s.muteBekliyor = true }

// yerlestir — paketin dosya içindeki yerini (pts) verir; yeni bölüm
// açıldıysa onu döner. `yedek` (RED yedek bloğu) durumu değiştirmez.
// Son iki dönüş: yakalama saati geçici bir bölümü kesinleştirdiyse o bölüm ve
// kayma (örnek) — yazıcı kuyruktaki paketleri SR'daki gibi kaydırır.
func (s *sesSaat) yerlestir(rtp uint32, gelis time.Time, seq uint16, yedek bool, yakNs int64) (int64, *sesBolum, *sesBolum, int64) {
	if !s.var_ {
		b := &sesBolum{Sira: 0, RTP0: rtp, Pts0: 0, Kaynak: "ilk", IlkGelisNs: gelis.UnixNano()}
		s.bolumler = append(s.bolumler, b)
		s.var_ = true
		s.sonRTP, s.sonGelis, s.sonSeq, s.sonPts = rtp, gelis, seq, 0
		b.sonRTP, b.sonPts, b.Paket, b.sonGelisNs = rtp, 0, 1, gelis.UnixNano()
		s.paketSayisi = 1
		if yakNs > 0 {
			s.actPaket, s.sonYak, s.sonYakRTP, s.sonYakPts, s.sonYakBolum = 1, yakNs, rtp, 0, 0
			s.sonYakGelis = gelis
			s.actG.ekle(0, yakNs)
		}
		return 0, nil, nil, 0
	}
	b := s.simdiki()
	if yedek {
		return b.Pts0 + sdelta(rtp, b.RTP0), nil, nil, 0
	}
	s.paketSayisi++
	dRtp := sdelta(rtp, s.sonRTP)
	dGelis := gelis.Sub(s.sonGelis)
	eksik := dGelis - time.Duration(dRtp*int64(time.Second)/s.hz)
	esik := bolumEsikSinyalsiz
	if s.muteBekliyor {
		esik = bolumEsikSinyalli
	}
	var yeni *sesBolum
	pts := b.Pts0 + sdelta(rtp, b.RTP0)
	if yakNs > 0 {
		s.actPaket++
	}
	// ── 0. YOL: YAKALAMA SAATİ ── yayıncının kendi saatiyle ölçülen yer;
	// kesin, titremesiz. `dogru` = son yakalama paketinin yeri + iki damga
	// arası; `kayma` = bununla RTP'ye göre yer arasındaki fark.
	actVar := yakNs > 0 && s.sonYak > 0
	var dogru, kayma int64
	var kaymaBolum *sesBolum
	yeniTaban := false
	if actVar {
		dogru = s.sonYakPts + s.sureOrnek(time.Duration(yakNs-s.sonYak))
		kayma = dogru - pts
		// Damganın referanstan beri İLERLEMESİ ile duvar saatinin (varış)
		// ilerlemesi DOĞRUDAN karşılaştırılır: yakalama, duvar saatinden hızlı
		// akamaz. Aradaki bölümler hesaba girmez — rawrec54 taslağı `kayma`
		// üzerinden karşılaştırıyordu ve referans önceki bölümdeyken varış
		// yoluyla açılmış geçici bölümün boşluğunu iki kez düşüyordu (denetim
		// bulgusu: açılışın ilk paketi damgasız/kayıp, ikincisi gelecek damgalı
		// → 2,2 sn hayalet boşluk; rawrec55). Referans damgalı paket en çok ~1 sn
		// geride (Chrome damgayı 1/sn yollar), titreme yalnız iki uç paketinki.
		gelecek := time.Duration(yakNs-s.sonYak) - gelis.Sub(s.sonYakGelis)
		if kayma > s.sureOrnek(actEsik) && !s.sonYakGelis.IsZero() && gelecek > actGelecekEsik {
			// GELECEK DAMGA (olay 937): damga, duvar saatinin geçmediği bir süreyi
			// "yakalandı" diyor → yakalama varıştan SONRA olamaz. Damga güvenilmez:
			// varış yoluna düş, referansı KORU (bkz. actGelecekEsik).
			actVar = false
			s.actGelecek++
			s.actRedArd++
			s.actRedKaydet(pts, yakNs, gelecek, kayma)
			if s.actRedArd >= actRedTaban {
				yeniTaban = true
			}
		} else if kayma < -s.sureOrnek(actGeriEsik) {
			// REFERANS BOZUK (bkz. actGeriEsik): damga uygulanmaz, referans buraya taşınır.
			actVar = false
			s.actSupheli++
			yeniTaban = true
		} else if eksik > esik && kayma <= s.sureOrnek(actEsik) {
			// BAYAT YAKALAMA SAATİ (rawrec49, kayıt 920): varış "akış durdu"
			// diyor, bu paketin damgası "hiç durmadı" diyor. Chrome'da
			// susturma öncesi yarım kalan 10 ms'lik çerçevenin damgası
			// (libwebrtc ACM `absolute_capture_timestamp_ms_` yapışkan) açılış
			// sonrası İLK pakete yapışıyor; damga susturma ÖNCESİNİN saati.
			// Güvenilmez: varış yoluna düş, referans (sonYak) GÜNCELLENMEZ;
			// bir sonraki gerçek damga geçici bölümü kesinleştirir. (Gerçek
			// bir 3 sn+ ağ kesintisi de aynı görünür; o zaman da bir sonraki
			// damga bölümü geri çeker — sonuç yine doğru.)
			actVar = false
			s.actBayat++
		}
	}
	if actVar {
		refKoru := false
		if b.actGecici && s.sonYakBolum == b.Sira && !b.yazildi && b.Sira > 0 {
			// GEÇİCİ ACT BÖLÜMÜ (rawrec56): sonraki damgalar hükmü verir. kayma ≈ 0
			// → doğrulandı; küçük kayma → taban düzeltilir, bölüm geçici kalır
			// (merdiven); büyük kayma → olduğu gibi kabul (gerçek duraklama). En
			// çok actGeciciDuzUst düzeltme. Bölümden paket yazıldıysa (yazildi)
			// taban artık kaydırılamaz: düzeltme atlanır, eski davranış.
			if kayma >= -s.sureOrnek(actEsik) && kayma <= s.sureOrnek(actEsik) || b.actDuz >= actGeciciDuzUst || kayma < -s.sureOrnek(actGeciciUst) || kayma > s.sureOrnek(actGeciciUst) {
				b.actGecici = false
			}
			// Düzeltme bölümün adımını EKSİYE çeviremez (bölüm önceki çizginin
			// altına inemez). Tepe geri alınıp adım ~0 olduktan sonra geçici
			// pencerede gelen tek paketlik çukur düzeltme DEĞİL geri damgadır:
			// aşağıdaki kurala düşer (uygulanmaz, referans korunur). Damgasız
			// paket bölümü kesinleştiremez: iki paketlik tepede ikinci paket
			// damgasız gelir (gönderici ilkine göre sapma görmez), dönüş üçüncüde.
			cukur := b.actGecici && kayma < 0 && b.SrEksikSn != nil && *b.SrEksikSn+s.ornekSn(kayma) < -s.ornekSn(s.sureOrnek(actEsik))/2
			if b.actGecici && kayma != 0 && !cukur {
				b.actDuz++
				if alt := s.bolumler[b.Sira-1].sonPts + 1; b.Pts0+kayma < alt {
					kayma = alt - b.Pts0
				}
				if kayma != 0 {
					b.Pts0 += kayma
					b.sonPts += kayma
					s.sonPts += kayma
					s.enSonPts += kayma
					s.sonYakPts += kayma
					kaymaBolum = b
					e := *b.SrEksikSn + s.ornekSn(kayma)
					b.SrEksikSn = &e
					s.actGeciciDuz++
				}
				pts = dogru
				s.actGeriArd = 0
				p, y := s.bitir(b, rtp, gelis, seq, pts, dRtp, nil)
				s.hicKaydet(eksik, p, rtp, gelis, nil)
				s.sonYak, s.sonYakRTP, s.sonYakPts, s.sonYakBolum = yakNs, rtp, p, b.Sira
				s.sonYakGelis = gelis
				s.actRedArd = 0
				s.actG.ekle(p, yakNs)
				return p, y, kaymaBolum, kayma
			}
		}
		switch {
		case !b.Kesin && b.Sira > 0 && s.sonYakBolum < b.Sira:
			// GEÇİCİ BÖLÜMÜ KESİNLEŞTİR (SR gibi, yayıncının damgasıyla):
			// referans damga bu bölümden ÖNCE → ölçülen fark bölümün varışla
			// kurulan tabanının hatası (bayat/erken ilk paket, kuyruk
			// patlaması, bayat damga). Taban kayar, kuyruk yazıcıda kayar.
			if b.Sira > 0 {
				// MONOTON KORUMA: bölüm tabanı önceki bölümün son paketinin
				// altına inemez (negatif/örtüşen pts hiç oluşmasın; denetim T1).
				if alt := s.bolumler[b.Sira-1].sonPts + 1; b.Pts0+kayma < alt {
					kayma = alt - b.Pts0
				}
			}
			if kayma != 0 {
				b.Pts0 += kayma
				b.sonPts += kayma
				s.sonPts += kayma
				s.enSonPts += kayma
				kaymaBolum = b
			}
			pts = dogru
			b.Kesin = true
			b.Kaynak += "+act"
			e := b.VarisEksikSn + s.ornekSn(kayma)
			b.SrEksikSn = &e
		case kayma > s.sureOrnek(actEsik):
			// DURMUŞ SAAT, damgayla kesin: yeni bölüm. ⚠ rawrec43-47 yeri
			// `sonPts + dYak` diye kuruyordu; damga SEYREKSE (Chrome ~1/sn)
			// son damgalı paket ile son paket arası bir kez daha ekleniyordu
			// (kayıt 919: dosya 2,0 sn uzun). `TestSesYakalamaSaatiSeyrek`.
			e := s.ornekSn(kayma)
			pts = dogru
			yeni = &sesBolum{Sira: len(s.bolumler), RTP0: rtp, Pts0: pts, Kaynak: "act",
				VarisEksikSn: eksik.Seconds(), SrEksikSn: &e, Kesin: true,
				MuteSinyali: s.muteBekliyor, IlkGelisNs: gelis.UnixNano()}
			yeni.actGecici = kayma <= s.sureOrnek(actGeciciUst)
			s.bolumler = append(s.bolumler, yeni)
			s.muteBekliyor = false
			b = yeni
			s.actGeriArd = 0
		case kayma < -s.sureOrnek(actEsik) && !b.Kesin && b.Paket <= 3 && b.Sira > 0:
			// Bayat ilk paket (referans aynı bölümde) — damgayla kesin.
			b.Pts0 += kayma
			pts = dogru
			s.bayat++
		case kayma < -s.sureOrnek(actEsik):
			// GERİ DAMGA (rawrec56, kayıt 940): damga referansın gerisinde, dosya
			// geri alınamaz. Tek paketlikse (Android bayat HAL damgası: "çukur")
			// uygulanmaz ve REFERANS KORUNUR: bir sonraki paket eski çizgide → kayma 0.
			// Eski kod referansı buraya taşıyordu; sonraki paket +18 ms "durmuş saat"
			// sayılıp kalıcı boşluk oluyordu. Art arda actGeriArdTaban damga aynı
			// geri kaymayı gösterirse kalıcı adımdır: referans taşınır.
			if s.actGeriArd > 0 && kayma-s.actGeriSon <= s.sureOrnek(actGeriUyum) && s.actGeriSon-kayma <= s.sureOrnek(actGeriUyum) {
				s.actGeriArd++
			} else {
				s.actGeriArd = 1
			}
			s.actGeriSon = kayma
			if s.actGeriArd >= actGeriArdTaban {
				s.actGeriTaban++
				s.actGeriArd = 0
			} else {
				refKoru = true
				s.actGeriAtlan++
			}
		default:
			s.actGeriArd = 0
		}
		p, y := s.bitir(b, rtp, gelis, seq, pts, dRtp, yeni)
		s.hicKaydet(eksik, p, rtp, gelis, yeni)
		if !refKoru {
			s.sonYak, s.sonYakRTP, s.sonYakPts, s.sonYakBolum = yakNs, rtp, p, b.Sira
			s.sonYakGelis = gelis
		}
		s.actRedArd = 0
		s.actG.ekle(p, yakNs)
		return p, y, kaymaBolum, kayma
	}
	switch {
	case eksik > esik:
		// DURMUŞ SAAT: duvar, RTP'den eşikten fazla ilerledi. Paketi bir
		// önceki paketin `dGelis` kadar sonrasına koy (geçici, varış).
		pts = s.sonPts + s.sureOrnek(dGelis)
		kaynak := "varis"
		if s.muteBekliyor {
			kaynak = "mute+varis"
		}
		yeni = &sesBolum{Sira: len(s.bolumler), RTP0: rtp, Pts0: pts, Kaynak: kaynak,
			VarisEksikSn: eksik.Seconds(), MuteSinyali: s.muteBekliyor,
			IlkGelisNs: gelis.UnixNano()}
		s.bolumler = append(s.bolumler, yeni)
		s.muteBekliyor = false
		b = yeni
	case !b.Kesin && b.Paket <= 3 && b.Sira > 0 && -eksik > bayatEsik:
		// BAYAT İLK PAKET: geçici bölümün başında RTP duvardan öne geçti →
		// taban ilk pakete değil buna kurulmalıydı. Kaydır.
		duz := s.sonPts + s.sureOrnek(dGelis)
		b.Pts0 += duz - pts
		pts = duz
		s.bayat++
	case !b.Kesin && b.Paket <= 3 && b.Sira > 0 && eksik > erkenBoslukEsik:
		// ERKEN İLK PAKET (rawrec47): bölümün başında akış varıştan geride
		// kaldı — ilk paket(ler) erken gelmiş, gerçek akış boşluktan sonra
		// başlamış. Taban buna taşınır; kuyruktaki ilk paket(ler) olduğu
		// yerde kalır (≤ 60 ms ses, önemsiz). SR sonra yine kesinleştirir.
		duz := s.sonPts + s.sureOrnek(dGelis)
		b.Pts0 += duz - pts
		pts = duz
		s.erken++
	}
	p, y := s.bitir(b, rtp, gelis, seq, pts, dRtp, yeni)
	s.hicKaydet(eksik, p, rtp, gelis, yeni)
	if yakNs > 0 && (s.sonYak == 0 || yeniTaban) {
		// İlk damga: referans (bayat/gelecek şüphelisi buraya düşmez, referansı
		// korur). yeniTaban: yayıncı saati sıçradı, referans varış yoluyla
		// bulunan yere yeniden kurulur — dosya kaymaz, act yolu yeniden çalışır.
		s.sonYak, s.sonYakRTP, s.sonYakPts, s.sonYakBolum = yakNs, rtp, p, s.simdiki().Sira
		s.sonYakGelis = gelis
		s.actG.ekle(p, yakNs)
		if yeniTaban {
			s.actYeniTaban++
			s.actRedArd = 0
		}
	}
	return p, y, nil, 0
}

// bitir — paketin durumunu işle (ortak kuyruk).
func (s *sesSaat) bitir(b *sesBolum, rtp uint32, gelis time.Time, seq uint16,
	pts, dRtp int64, yeni *sesBolum) (int64, *sesBolum) {
	if pts <= s.sonPts && dRtp > 0 {
		// Aynı bölümde geri gitmez; eş damga/kopya paketleri yazıcı ayıklıyor.
		pts = s.sonPts + 1
	}
	s.sonRTP, s.sonGelis, s.sonSeq, s.sonPts = rtp, gelis, seq, pts
	b.sonRTP, b.sonPts, b.sonGelisNs = rtp, pts, gelis.UnixNano()
	if b.Paket < 16 {
		b.IlkPaketler = append(b.IlkPaketler, [2]float64{
			yuvarla(float64(gelis.UnixNano()-b.IlkGelisNs) / 1e6),
			yuvarla(float64(sdelta(rtp, b.RTP0)) * 1000 / float64(s.hz)),
		})
	}
	b.Paket++
	if pts > s.enSonPts {
		s.enSonPts = pts
	}
	return pts, yeni
}

// hicKaydi — bir SR aralığındaki en büyük varış hıçkırığı (varış − RTP,
// pozitif) ve yeri. Rapor basamağının bölme noktası adayı.
type hicKaydi struct {
	eksik   time.Duration
	pts     int64
	rtp     uint32
	gelisNs int64
}

// hicKaydet — süren SR aralığının en büyük hıçkırığını tutar; yeni bölüm
// açılınca basamak izleme sıfırlanır (bölüm başı zaten bir sıçrama, sapmalar
// yeni tabana göre ölçülür).
func (s *sesSaat) hicKaydet(eksik time.Duration, pts int64, rtp uint32, gelis time.Time, yeni *sesBolum) {
	if yeni != nil {
		s.srSapma, s.hicAra, s.hicSu = s.srSapma[:0], s.hicAra[:0], hicKaydi{}
		return
	}
	if s.hicSu.pts == 0 || eksik > s.hicSu.eksik {
		if s.hicSu.pts == 0 || eksik > s.hicSu.eksik {
			s.hicSu = hicKaydi{eksik: eksik, pts: pts, rtp: rtp, gelisNs: gelis.UnixNano()}
		}
	}
}

func ortalama(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	var t int64
	for _, x := range v {
		t += x
	}
	return t / int64(len(v))
}

func yayilim(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	lo, hi := v[0], v[0]
	for _, x := range v {
		if x < lo {
			lo = x
		}
		if x > hi {
			hi = x
		}
	}
	return hi - lo
}

// bolumZamanla — SR hangi bölüme ait: VARIŞ ANI (At) hangi bölümün paket
// penceresine düşüyorsa (ilk varış … son varış + tolerans).
//
// ⚠ NEDEN RTP DEĞİL VARIŞ (kayıt 911 fikstürüyle öğrenildi). Chrome
// susturma SIRASINDA da Sender Report yolluyor ve o raporların RTP damgası
// gerçek sayaç değil, son kareden duvar saatiyle TAHMİN (libwebrtc
// RTCPSender: last_rtp + (now − last_capture) × hz). Sayaç ise durmuş;
// açılınca paketler tahminin GERİSİNDEN gelir. Bu raporlar RTP'ye göre
// "ileride" görünür, sonradan o damgaya ulaşan paketlere eşlenirse bölüm
// tabanı susturma süresi kadar yanlış kesinleşir (911A: bölüm 1'de 12,9 sn
// yerine 8,5 sn). Varış penceresi bunları dışarıda bırakıyor.
func (s *sesSaat) bolumZamanla(atNs int64) *sesBolum {
	for i := len(s.bolumler) - 1; i >= 0; i-- {
		b := s.bolumler[i]
		if atNs >= b.IlkGelisNs && atNs <= b.sonGelisNs+int64(srPencereTolerans) {
			return b
		}
		if atNs > b.sonGelisNs {
			return nil // iki bölümün arasında (susturma içi)
		}
	}
	return nil
}

// srGeldi — yeni Sender Report. Döner: tabanı kesinleşen bölüm ve kayma
// (örnek); kayma 0 ise kuyruk dokunulmaz.
func (s *sesSaat) srGeldi(sr *livekit.RTCPSenderReportState) (*sesBolum, int64) {
	if sr == nil || sr.NtpTimestamp == 0 || !s.var_ {
		return nil, 0
	}
	temel := sr.AtAdjusted
	if temel == 0 {
		temel = sr.At
	}
	if temel == 0 || sr.NtpTimestamp == s.srSonNTP {
		return nil, 0
	}
	s.srSonNTP = sr.NtpTimestamp
	b := s.bolumZamanla(temel)
	if b == nil || sdelta(sr.RtpTimestamp, b.RTP0) < 0 {
		s.srAtlanan++
		return nil, 0
	}
	ptsSR := b.Pts0 + sdelta(sr.RtpTimestamp, b.RTP0)
	// YAYINCI SAATİ (rawrec45): SR'ın NTP'si yayıncının kendi saati, RTP'si
	// de kendi sayacı — ikisi aynı makinede, aynı anda alınıyor. Bölüm
	// tabanı bu çiftle kesinleşir: ağ titremesi de, sunucu saati de, SR'ın
	// yolda geçirdiği süre de karışmaz. Sunucu saati (`temel` = varış)
	// yalnız pencere seçiminde ve yan JSON çapasında (anchorNs) kullanılır.
	// Eskiden düzeltme de `temel` üstündendi: SR'ın varış titremesi kadar
	// (1–40 ms) hata taşıyordu.
	kaynak := ntpNs(sr.NtpTimestamp)
	var kayma int64
	switch {
	case b.Sira == 0 && !s.anchorKesin:
		// ÇAPA: ilk bölümün SR'ları pts 0'ın saatini veriyor (yayıncı ve
		// sunucu saatinde ayrı ayrı). TEK SR'a tam güvenilmiyor (ilk ses
		// SR'ı bozuk olabiliyor, crbug 168328): birbirine 200 ms içinde iki
		// örnek gelince ortalaması çapa olur, uyumsuz örnek atılır.
		adayS := temel - ptsSR*int64(time.Second)/s.hz
		adayK := kaynak - ptsSR*int64(time.Second)/s.hz
		for i, eski := range s.anchorAdayK {
			if d := adayK - eski; d < 200*int64(time.Millisecond) && d > -200*int64(time.Millisecond) {
				s.anchorKaynakNs = (adayK + eski) / 2
				s.anchorNs = (adayS + s.anchorAday[i]) / 2
				s.anchorKesin = true
				// Rapor basamağı dizisi (rawrec52): iki çapa SR'ının çapaya
				// göre sapması (±d/2) "önce" grubunun ilk örnekleri.
				s.srSapma = append(s.srSapma, (eski-s.anchorKaynakNs)*s.hz/int64(time.Second),
					(adayK-s.anchorKaynakNs)*s.hz/int64(time.Second))
				s.hicAra = append(s.hicAra, hicKaydi{}, s.hicSu)
				s.hicSu = hicKaydi{}
				break
			}
		}
		if !s.anchorKesin {
			s.anchorAday = append(s.anchorAday, adayS)
			s.anchorAdayK = append(s.anchorAdayK, adayK)
			if len(s.anchorAday) > 5 {
				s.anchorAday = s.anchorAday[1:]
				s.anchorAdayK = s.anchorAdayK[1:]
			}
		}
	case !b.Kesin && b.Sira > 0 && (s.anchorKesin || len(s.anchorAdayK) == 1):
		// TEK ADAYLA DA KESİNLEŞTİRME (rawrec45): ilk bölüm kısa sürüp tek
		// SR almışsa çapa doğrulanamıyor; yine de kullanılır ama düzeltme
		// payı dar (srTekToleransSn). Doğrulanmış çapada pay geniş: kuyruk
		// patlamasında varış tahmini saniyelerce şaşabiliyor, SR düzeltir.
		capa, tol := s.anchorKaynakNs, srToleransSn
		if !s.anchorKesin {
			capa, tol = s.anchorAdayK[0], srTekToleransSn
		}
		ptsKesin := (kaynak-capa)*s.hz/int64(time.Second) - sdelta(sr.RtpTimestamp, b.RTP0)
		kayma = ptsKesin - b.Pts0
		if k := s.ornekSn(kayma); k > tol || k < -tol {
			s.srBozuk++
			kayma = 0
			break
		}
		eksik := b.VarisEksikSn + s.ornekSn(kayma)
		b.SrEksikSn = &eksik
		if b.yazildi {
			// Dosyaya girdi: taban artık değişemez, değer yan JSON'da kalır.
			kayma = 0
			break
		}
		b.Pts0 = ptsKesin
		b.Kesin = true
		b.Kaynak += "+sr"
		b.sonPts += kayma
		// Rapor basamağı dizisi (rawrec52): kesinleştiren SR'ın sapması tanım
		// gereği 0 — "önce" grubunun ilk örneği. Tabanı tek SR kurduğu için
		// onun salınımı tabanda; sonraki SR'lar buna göre ölçülür.
		s.srSapma = append(s.srSapma[:0], 0)
		s.hicAra = append(s.hicAra[:0], s.hicSu)
		s.hicSu = hicKaydi{}
		// Sonraki bölümler bu bölümün sonuna göre kurulmuştu: onlar da kayar.
		for i := b.Sira + 1; i < len(s.bolumler); i++ {
			s.bolumler[i].Pts0 += kayma
			s.bolumler[i].sonPts += kayma
		}
		s.sonPts += kayma
		s.enSonPts += kayma
		if s.sonYakBolum >= b.Sira {
			s.sonYakPts += kayma
		}
		ptsSR += kayma
	case s.anchorKesin && s.actPaket == 0 && (b.Kesin || b.Sira == 0):
		// RAPOR BASAMAĞI (rawrec52): bölüm kesin, SR yine de tabanı başka
		// yerde görüyor. Firefox (923) açılışta RTP'yi duvar saatine yeniden
		// tabanlıyor ama bir kare (20 ms) eksik: varış kuralı görmez (titreme
		// payı), damga yok → tek tanık SR. Grup kuralı (sabitlerin yorumu).
		ptsKesin := (kaynak-s.anchorKaynakNs)*s.hz/int64(time.Second) - sdelta(sr.RtpTimestamp, b.RTP0)
		s.srSapma = append(s.srSapma, ptsKesin-b.Pts0)
		s.hicAra = append(s.hicAra, s.hicSu)
		s.hicSu = hicKaydi{}
		if len(s.srSapma) > 2*srAdimGrup {
			s.srSapma = s.srSapma[1:]
			s.hicAra = s.hicAra[1:]
		}
		n := len(s.srSapma)
		if n < 2*srAdimGrup {
			// İKİ TAM GRUP ŞART. Tabanı tek SR kurmuş olabilir ve o SR'ın
			// kendi salınımını (Chrome ±15 ms) taşır; "önce" grubunu sıfır
			// saymak 911A'da 17 ms'lik üç yanlış basamak verdi.
			break
		}
		sonra := s.srSapma[n-srAdimGrup:]
		once := s.srSapma[n-2*srAdimGrup : n-srAdimGrup]
		adim := ortalama(sonra) - ortalama(once)
		if s.ornekSn(yayilim(sonra)) > srAdimYayilimSn || s.ornekSn(yayilim(once)) > srAdimYayilimSn ||
			(s.ornekSn(adim) < srAdimEsikSn && s.ornekSn(adim) > -srAdimEsikSn) {
			break
		}
		{
			// Bölme noktası: "sonra" grubunun aralıklarındaki en büyük
			// hıçkırık (≥ hicEsik); yoksa ilk "sonra" aralığının kaydı;
			// o da yoksa şimdi.
			bolP, bolRTP, bolGelis := s.sonPts, s.sonRTP, s.sonGelis.UnixNano()
			var enB hicKaydi
			for _, h := range s.hicAra[n-srAdimGrup:] {
				if h.eksik > enB.eksik {
					enB = h
				}
			}
			ilk := s.hicAra[n-srAdimGrup]
			switch {
			case enB.eksik >= hicEsik:
				bolP, bolRTP, bolGelis = enB.pts, enB.rtp, enB.gelisNs
			case ilk.pts > 0:
				bolP, bolRTP, bolGelis = ilk.pts, ilk.rtp, ilk.gelisNs
			}
			e := s.ornekSn(adim)
			yeni := &sesBolum{Sira: len(s.bolumler), RTP0: bolRTP, Pts0: bolP + adim,
				Kaynak: "sr-adim", SrEksikSn: &e, Kesin: true, IlkGelisNs: bolGelis,
				sonRTP: s.sonRTP, sonPts: s.sonPts + adim, sonGelisNs: s.sonGelis.UnixNano()}
			b.sonGelisNs = bolGelis // eski bölümün penceresi bölmede biter
			s.bolumler = append(s.bolumler, yeni)
			s.sonPts += adim
			s.enSonPts += adim
			if s.sonYakBolum == b.Sira && s.sonYakPts >= bolP {
				s.sonYakPts += adim
				s.sonYakBolum = yeni.Sira
			}
			if sdelta(sr.RtpTimestamp, bolRTP) >= 0 {
				ptsSR += adim
			}
			s.srSapma, s.hicAra, s.hicSu = s.srSapma[:0], s.hicAra[:0], hicKaydi{}
			s.srAdim++
			kayma = adim
			b = yeni // yazıcı: pts ≥ Pts0−kayma olan kuyruk öğeleri kayar (srIsle)
		}
	}
	if len(s.gunluk) < srGunluguUstSinir {
		s.gunluk = append(s.gunluk, srOrnek{Katman: 0, RTP: sr.RtpTimestamp, AtNs: temel,
			NtpNs: ntpNs(sr.NtpTimestamp), Pts: &ptsSR})
	}
	return b, kayma
}

// ozDenetim — yazıcının kendi karnesi: SR'ların söylediği duvar süresi ile
// dosyaya yazılan süre tutuyor mu. `ham_acik_sn` eski (saf RTP) ölçü —
// düzeltmenin ne kadar iş yaptığını gösterir.
func (s *sesSaat) ozDenetim() map[string]any {
	r := map[string]any{
		"bolum":             len(s.bolumler),
		"kesin_bolum":       0,
		"sr_bozuk":          s.srBozuk,
		"sr_atlanan":        s.srAtlanan,
		"bayat_paket":       s.bayat,
		"erken_paket":       s.erken,
		"act_bayat":         s.actBayat,
		"act_gelecek":       s.actGelecek, // reddedilen gelecek damga (olay 937)
		"act_supheli":       s.actSupheli, // referansın çok gerisinde damga → referans yeniden tabanlandı
		"act_yeniden_taban": s.actYeniTaban,
		"sr_adim":           s.srAdim,
		"act_paket":         s.actPaket,
		"act_geri_atlanan":  s.actGeriAtlan,
		"act_geri_taban":    s.actGeriTaban,
		"act_gecici_duzelt": s.actGeciciDuz,
	}
	if s.paketSayisi > 0 {
		r["act_oran"] = yuvarla(float64(s.actPaket) / float64(s.paketSayisi))
	}
	kesin := 0
	for _, b := range s.bolumler {
		if b.Kesin || b.Sira == 0 {
			kesin++
		}
	}
	r["kesin_bolum"] = kesin
	if len(s.gunluk) < 2 {
		r["durum"] = "sr-yetersiz"
		return r
	}
	ilk, son := s.gunluk[0], s.gunluk[len(s.gunluk)-1]
	duvar := float64(son.AtNs-ilk.AtNs) / 1e9
	yazilan := s.ornekSn(*son.Pts - *ilk.Pts)
	ham := s.ornekSn(sdelta(son.RTP, ilk.RTP))
	acik := duvar - yazilan
	r["duvar_sn"] = yuvarla(duvar)
	r["yazilan_sn"] = yuvarla(yazilan)
	r["acik_sn"] = yuvarla(acik)
	r["ham_acik_sn"] = yuvarla(duvar - ham)
	if acik > ozDenetimEsikSn || acik < -ozDenetimEsikSn {
		r["durum"] = "zaman-tutarsiz"
		r["neden"] = fmt.Sprintf("SR duvar %.1f sn, yazılan %.1f sn (fark %.2f sn)", duvar, yazilan, acik)
	} else {
		r["durum"] = "tutarli"
	}
	return r
}
