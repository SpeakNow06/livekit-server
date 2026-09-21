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

	srSonNTP  uint64
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
	sonYak      int64  // son (yedek olmayan) paketin yakalanma anı, 0 = yok
	sonYakRTP   uint32 // o paketin RTP'si
	actPaket    int    // yakalama saatli paket sayısı
	paketSayisi int    // yedek dışı toplam paket
}

// actEsik — yakalama saatiyle ölçülen eksik bunu aşarsa bölüm açılır.
// Yakalama saatinde titreme yok; 5 paketlik pay, ölçüm gürültüsüne karşı.
const actEsik = 100 * time.Millisecond

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
func (s *sesSaat) yerlestir(rtp uint32, gelis time.Time, seq uint16, yedek bool, yakNs int64) (int64, *sesBolum) {
	if !s.var_ {
		b := &sesBolum{Sira: 0, RTP0: rtp, Pts0: 0, Kaynak: "ilk", IlkGelisNs: gelis.UnixNano()}
		s.bolumler = append(s.bolumler, b)
		s.var_ = true
		s.sonRTP, s.sonGelis, s.sonSeq, s.sonPts = rtp, gelis, seq, 0
		b.sonRTP, b.sonPts, b.Paket, b.sonGelisNs = rtp, 0, 1, gelis.UnixNano()
		s.paketSayisi = 1
		if yakNs > 0 {
			s.actPaket, s.sonYak, s.sonYakRTP = 1, yakNs, rtp
		}
		return 0, nil
	}
	b := s.simdiki()
	if yedek {
		return b.Pts0 + sdelta(rtp, b.RTP0), nil
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
	if yakNs > 0 && s.sonYak > 0 {
		// ── 0. YOL: YAKALAMA SAATİ ── yayıncının kendi saatiyle ölçülen
		// eksik; kesin, titremesiz. Bölüm hemen "kesin" (SR düzeltmesi
		// gerekmez), varış tahmini karşılaştırma için kaydedilir.
		dYak := time.Duration(yakNs - s.sonYak)
		eksikYak := dYak - time.Duration(sdelta(rtp, s.sonYakRTP)*int64(time.Second)/s.hz)
		switch {
		case eksikYak > actEsik:
			// YER = RTP'ye göre yer + ölçülen durma. ⚠ rawrec43-47 burada
			// `sonPts + dYak` yazıyordu; yakalama saati SEYREKSE (Chrome
			// ~1/sn, DTX'te daha seyrek) son yakalama paketi ile son paket
			// arasındaki süre bir kez daha ekleniyordu (kayıt 919: dosya
			// 2,0 sn uzun, öz denetim "tutarsız", postprocess tarayıcı
			// kopyasına düştü). Testler her pakette yakalama kullandığı
			// için görünmedi; `TestSesYakalamaSaatiSeyrek` bunu tutuyor.
			pts += s.sureOrnek(eksikYak)
			e := eksikYak.Seconds()
			yeni = &sesBolum{Sira: len(s.bolumler), RTP0: rtp, Pts0: pts, Kaynak: "act",
				VarisEksikSn: eksik.Seconds(), SrEksikSn: &e, Kesin: true,
				MuteSinyali: s.muteBekliyor, IlkGelisNs: gelis.UnixNano()}
			s.bolumler = append(s.bolumler, yeni)
			s.muteBekliyor = false
			b = yeni
		case eksikYak < -actEsik && !b.Kesin && b.Paket <= 3 && b.Sira > 0:
			// Bayat ilk paket (bkz. aşağıdaki varış kuralı) — yakalama
			// saatiyle kesin. Seyrek yakalamada da doğru: RTP'ye göre yer +
			// ölçülen (negatif) fark.
			duz := pts + s.sureOrnek(eksikYak)
			b.Pts0 += duz - pts
			pts = duz
			s.bayat++
		}
		s.sonYak, s.sonYakRTP = yakNs, rtp
		return s.bitir(b, rtp, gelis, seq, pts, dRtp, yeni)
	}
	if yakNs > 0 {
		s.sonYak, s.sonYakRTP = yakNs, rtp
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
	return s.bitir(b, rtp, gelis, seq, pts, dRtp, yeni)
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
		// Sonraki bölümler bu bölümün sonuna göre kurulmuştu: onlar da kayar.
		for i := b.Sira + 1; i < len(s.bolumler); i++ {
			s.bolumler[i].Pts0 += kayma
			s.bolumler[i].sonPts += kayma
		}
		s.sonPts += kayma
		s.enSonPts += kayma
		ptsSR += kayma
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
		"bolum":       len(s.bolumler),
		"kesin_bolum": 0,
		"sr_bozuk":    s.srBozuk,
		"sr_atlanan":  s.srAtlanan,
		"bayat_paket": s.bayat,
		"erken_paket": s.erken,
		"act_paket":   s.actPaket,
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
