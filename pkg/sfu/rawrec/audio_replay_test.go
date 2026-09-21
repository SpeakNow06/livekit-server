package rawrec

// ── SES REPLAY TESTLERİ (Adım 0-2, 2026-09-21) ──────────────────────────────
//
// NEDEN VAR: kayıt 911'de öğrenci sesleri dakikalarca yanlış yere yerleşti.
// Kök neden: mikrofon susturulunca yayıncının RTP saati duruyor, ses yazıcısı
// granülü saf RTP'den kurduğu için susturma süresi dosyadan siliniyor
// (plan: speaknow-server docs/split-recording/SES-SAATI-DURAKLAMASI-PLANI.md;
// çözüm: saat.go eşleyicisi).
//
// Bu dosya:
//   1. ALTIN TEST — durmasız, DTX'li, kayıplı sentetik akışta çıktı bayt bayt
//      SABİT (Adım 0'da mevcut kodla alındı; düzeltme sağlam dosyaları
//      DEĞİŞTİRMEMELİ).
//   2. GERÇEK VERİ — 911 öğrenci A: her paketin tarayıcı kopyasındaki (LiveKit
//      forwarder'ının duvar saatiyle düzelttiği) gerçek konumu fikstürde;
//      yazıcının granülü o konuma oturmalı. 911 öğretmen: kontrol (durma yok).
//   3. SENTETİK YAYINCI — DTX / kayıp / durmuş saat (sinyalli-sinyalsiz, kısa-
//      uzun) / kuyruk patlaması (yanlış alarm, SR'la geri alınır) / SR
//      inceliği / RTP sarması / bozuk ilk SR / RED yedeği / dosya adı
//      çakışması.
//
// Fikstür (`testdata/fx911/fixture_911A.json.gz`): prod'da üretildi
// (2026-09-21) — SFU dosyasının paketleri tarayıcı kopyasıyla İÇERİK
// eşleşmesiyle hizalandı (25 614 / 25 617), `paket[i] = [sfu_granül,
// tarayıcı_granül]`; eşleşmeyen paket −1. Yayıncının SR günlüğü de içinde.
// Öğretmen (T) fikstüründe tarayıcı hizalaması GÜVENİLMEZ (sunucu gürültü
// giderici baytları değiştiriyor, DTX paketleri birbirinin aynı); orada
// gerçek konum RTP'nin kendisi (SR açığı 0,0 ölçüldü).

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// ── sahte SR kaynağı ────────────────────────────────────────────────────────
//
// rawrec45'ten beri ses yazıcısı SR'ı YOKLAMIYOR; ham SR kanaldan itiliyor
// (`Writer.SR`). Test, her paketten önce varış anı gelmiş SR'ları sırayla
// iter (`ilet`) — gerçekte RTCP okuyucusunun yaptığı iş. `GetSenderReportData`
// yalnız `srKaynak` arayüzü için duruyor (görüntü yazıcısı yokluyor).
//
// YAYINCI SAATİ: SR'ın NTP'si yayıncının kendi saati; testte sunucudan
// 3 saat geride tutuluyor ki ofsetin sonuca girmediği kanıtlansın (eşleyici
// bölümü yayıncı saatiyle kesinleştiriyor — saat.go srGeldi).

type sahteSesKaynak struct {
	now    atomic.Int64 // simüle "şimdi" (unix ns) — test her paketten önce kurar
	srler  []*livekit.RTCPSenderReportState
	itilen int // kaç SR yazıcıya itildi (sıralı)
}

// yayinciSaatOfseti — yayıncı saati − sunucu saati (testte −3 saat).
const yayinciSaatOfseti = -3 * int64(time.Hour)

// ilet — varış anı `nowNs`'i geçmemiş, henüz itilmemiş SR'ları yazıcıya iter.
func (s *sahteSesKaynak) ilet(w *Writer, nowNs int64) {
	for s.itilen < len(s.srler) && s.srler[s.itilen].At <= nowNs {
		w.SR(s.srler[s.itilen])
		s.itilen++
	}
}

func (s *sahteSesKaynak) GetSenderReportData() *livekit.RTCPSenderReportState {
	now := s.now.Load()
	var son *livekit.RTCPSenderReportState
	for _, sr := range s.srler {
		if sr.At > now {
			break
		}
		son = sr
	}
	return son
}
func (s *sahteSesKaynak) SendPLI(force bool) {}

func ntpTS(unixNs int64) uint64 {
	sn := unixNs / 1e9
	kesir := unixNs % 1e9
	return uint64(sn+ntpUnixOffset)<<32 | uint64(float64(kesir)/1e9*4294967296.0)
}

// srEkle — (rtp, sunucu ns) çifti ekler; liste At'a göre sıralı tutulur.
// NTP yayıncı saatiyle (sunucu − 3 saat), varış sunucu saatiyle.
func (s *sahteSesKaynak) srEkle(rtp uint32, atNs int64) { s.srEkleTitrek(rtp, atNs, 0) }

// srEkleTitrek — SR'ın varışı `titreme` kadar gecikmiş/erken (ağ titremesi);
// NTP/RTP çifti yine gerçek üretim anını taşır. Ham SR + yayıncı saatiyle
// kesinleştirme sayesinde sonuç titremeden etkilenmemeli.
func (s *sahteSesKaynak) srEkleTitrek(rtp uint32, atNs int64, titreme time.Duration) {
	varis := atNs + int64(titreme)
	s.srler = append(s.srler, &livekit.RTCPSenderReportState{
		RtpTimestamp: rtp, NtpTimestamp: ntpTS(atNs + yayinciSaatOfseti), At: varis,
	})
	sort.SliceStable(s.srler, func(i, j int) bool { return s.srler[i].At < s.srler[j].At })
}

// ── Ogg okuyucu ─────────────────────────────────────────────────────────────

type oggPaket struct {
	granul uint64
	yuk    []byte
}

func oggPaketleriOku(t *testing.T, yol string) []oggPaket {
	t.Helper()
	v, err := os.ReadFile(yol)
	if err != nil {
		t.Fatalf("ogg okunamadı: %v", err)
	}
	var out []oggPaket
	i := 0
	for i+27 <= len(v) && string(v[i:i+4]) == "OggS" {
		g := binary.LittleEndian.Uint64(v[i+6 : i+14])
		seg := int(v[i+26])
		tablo := v[i+27 : i+27+seg]
		p := i + 27 + seg
		var b []byte
		for _, uz := range tablo {
			b = append(b, v[p:p+int(uz)]...)
			p += int(uz)
			if uz < 255 {
				if len(b) > 0 && string(b[:min(4, len(b))]) != "Opus" {
					out = append(out, oggPaket{granul: g, yuk: b})
				}
				b = nil
			}
		}
		i = p
	}
	return out
}

// konumSn — paketin BAŞLANGICI (sn): granül − 960 örnek.
func konumSn(p oggPaket) float64 { return (float64(p.granul) - 960) / 48000.0 }

func yanOku(t *testing.T, opusYol string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(opusYol[:len(opusYol)-5] + ".json")
	if err != nil {
		t.Fatalf("yan JSON okunamadı: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("yan JSON bozuk: %v", err)
	}
	return m
}

func ozDenetim(t *testing.T, yan map[string]any) map[string]any {
	t.Helper()
	oz, _ := yan["oz_denetim"].(map[string]any)
	if oz == nil {
		t.Fatalf("yan JSON'da oz_denetim yok")
	}
	return oz
}

func bolumler(t *testing.T, yan map[string]any) []map[string]any {
	t.Helper()
	ham, _ := yan["bolumler"].([]any)
	var out []map[string]any
	for _, b := range ham {
		out = append(out, b.(map[string]any))
	}
	return out
}

// ── yazıcı kurulumu / besleme ───────────────────────────────────────────────

func yeniSesYazici(t *testing.T, dir string, kaynak *sahteSesKaynak, red bool) *Writer {
	t.Helper()
	paketiEtkinlestir(t, dir)
	yazmaGecikme = 10 * time.Second
	ti := &livekit.TrackInfo{Sid: "TR_SES", Source: livekit.TrackSource_MICROPHONE}
	w := NewWriter("cid-ses", ti, 48000, red, kaynak, logger.GetLogger())
	if w == nil {
		t.Fatalf("ses yazıcısı kurulamadı")
	}
	return w
}

// spaket — sentetik ya da fikstürden gelen tek ses paketi.
type spaket struct {
	rtp   uint32
	seq   uint16
	gelis time.Time
	yuk   []byte
	mute  bool  // bu paketten ÖNCE yayıncı mute sinyali (PubMute) verilsin
	yak   int64 // abs-capture-time (yayıncının yakalama saati, unix ns), 0 = yok
}

// sesYaz — paketi yazıcıya verir ve döngünün onu İŞLEMESİNİ bekler
// (deterministik: SR kaynağı simüle saati paket varışında görür).
func sesYaz(w *Writer, k *sahteSesKaynak, p spaket, hedefIslenen uint64) {
	if p.mute {
		w.PubMute(true)
	}
	k.now.Store(p.gelis.UnixNano())
	k.ilet(w, p.gelis.UnixNano()) // paketten önce varmış ham SR'lar, sırayla
	w.Write(p.yuk, p.rtp, 960, p.seq, p.gelis.UnixNano(), p.yak)
	for n := 0; w.islenen.Load() < hedefIslenen; n++ {
		if n < 1000 {
			runtime.Gosched()
		} else {
			time.Sleep(20 * time.Microsecond)
		}
	}
}

// sesBesle — bütün diziyi yazıcıya verir; dosya yolunu döner. Dosya, hedef
// bulunduktan (ticker, ≤200 ms) SONRAKİ pakette açılıyor; görünene kadar
// paketler aralıklı veriliyor. `bloklar` paket başına kaç kanal öğesi
// üretildiği (RED'de 2: yedek + birincil).
func sesBesle(t *testing.T, w *Writer, k *sahteSesKaynak, dir string, dizi []spaket, bloklar func(i int) uint64) string {
	t.Helper()
	desen := filepath.Join(dir, "audioraw_1", "*.opus")
	yol := ""
	var hedef uint64
	for i, p := range dizi {
		hedef += bloklar(i)
		sesYaz(w, k, p, hedef)
		if yol == "" {
			if m, _ := filepath.Glob(desen); len(m) > 0 {
				yol = m[0]
			} else if i < 200 {
				time.Sleep(30 * time.Millisecond)
			} else {
				t.Fatalf("dosya açılmadı: %s", desen)
			}
		}
	}
	if yol == "" {
		t.Fatalf("dosya hiç açılmadı: %s", desen)
	}
	return yol
}

func birBlok(int) uint64 { return 1 }

// opusYuk — deterministik sahte Opus paketi. TOC 0x78 (mono, 20 ms).
func opusYuk(i int) []byte {
	b := make([]byte, 60)
	b[0] = 0x78
	for j := 1; j < len(b); j++ {
		b[j] = byte((i*31 + j*7) & 0xff)
	}
	return b
}

// ── sentetik yayıncı modeli ─────────────────────────────────────────────────
//
// Duvar saati (sunucu varışı) ile RTP sayacını AYRI yürütüyor; olaylar
// ikisini farklı etkiliyor:
//
//	paket()   — 20 ms: ikisi de ilerler, paket üretir
//	dtx(ms)   — ikisi de ilerler, paket ÜRETMEZ (Opus DTX)
//	kayip(n)  — n paket "kaybolur": ikisi de ilerler, seq atlar
//	dur(ms)   — YALNIZ duvar ilerler (track durdu: replaceTrack(null))
//	srEkle()  — o anki (rtp, duvar) çifti kaynağa
type yayinci struct {
	wallNs int64
	rtp    uint32
	seq    uint16
	n      int
	jitter func(i int) time.Duration
	// dogru — üretilen her paketin GERÇEK konumu (örnek, duvar saatine göre)
	dogru []int64
	wall0 int64
	// act — paketlere yakalama saati (abs-capture-time) yazılsın: yakalama =
	// duvar − 40 ms (sabit yakalama→sunucu gecikmesi); yayıncı saati sunucudan
	// 3 saat geride (ofsetin önemi olmadığını gösterir).
	act bool
	// actHer — yakalama saati her N. pakette (0 = her pakette). Chrome ~1/sn
	// yazar (libwebrtc AbsoluteCaptureTimeSender 1 sn aralığı): 50.
	actHer int
}

func yeniYayinci(t0 time.Time, rtp0 uint32) *yayinci {
	return &yayinci{wallNs: t0.UnixNano(), wall0: t0.UnixNano(), rtp: rtp0, seq: 1000}
}

func (y *yayinci) paket() spaket {
	p := spaket{rtp: y.rtp, seq: y.seq, gelis: time.Unix(0, y.wallNs), yuk: opusYuk(y.n)}
	if y.jitter != nil {
		p.gelis = p.gelis.Add(y.jitter(y.n))
	}
	if y.act && (y.actHer == 0 || y.n%y.actHer == 0) {
		p.yak = y.wallNs - 40*int64(time.Millisecond) - 3*int64(time.Hour)
	}
	y.dogru = append(y.dogru, (y.wallNs-y.wall0)*48000/int64(time.Second))
	y.n++
	y.seq++
	y.rtp += 960
	y.wallNs += 20 * int64(time.Millisecond)
	return p
}

func (y *yayinci) dtx(ms int) {
	y.rtp += uint32(ms * 48)
	y.wallNs += int64(ms) * int64(time.Millisecond)
}

func (y *yayinci) kayip(n int) {
	y.seq += uint16(n)
	y.rtp += uint32(n) * 960
	y.wallNs += int64(n) * 20 * int64(time.Millisecond)
}

func (y *yayinci) dur(ms int) {
	y.wallNs += int64(ms) * int64(time.Millisecond)
}

// rtpAtla — RTP sayacı duvar ilerlemeden ileri sıçrar (yayıncı sayacı
// duvar saatine yeniden tabanlıyor; eksik/fazla sıçrama = kare hatası).
func (y *yayinci) rtpAtla(ms int) { y.rtp += uint32(ms * 48) }

func (y *yayinci) srEkle(k *sahteSesKaynak) { k.srEkle(y.rtp, y.wallNs) }

// sesKos — diziyi yeni bir yazıcıya besler, çıktıyı ve yan JSON'u döner.
func sesKos(t *testing.T, k *sahteSesKaynak, dizi []spaket) ([]oggPaket, map[string]any) {
	t.Helper()
	dir := t.TempDir()
	w := yeniSesYazici(t, dir, k, false)
	yol := sesBesle(t, w, k, dir, dizi, birBlok)
	k.ilet(w, 1<<62) // son paketten sonra gelen SR'lar da kapanıştan önce
	w.Close()
	if d := w.düşen.Load(); d != 0 {
		t.Fatalf("test kanalı taşırdı: %d paket düştü", d)
	}
	return oggPaketleriOku(t, yol), yanOku(t, yol)
}

// enBuyukSapma — çıktı konumu ile gerçek konum farkının en büyüğü (sn) ve
// hangi pakette.
func enBuyukSapma(t *testing.T, pk []oggPaket, dogru []int64) (float64, int) {
	t.Helper()
	if len(pk) != len(dogru) {
		t.Fatalf("çıktı paket sayısı %d, beklenen %d", len(pk), len(dogru))
	}
	enB, at := 0.0, -1
	for i, p := range pk {
		d := math.Abs(konumSn(p) - float64(dogru[i])/48000.0)
		if d > enB {
			enB, at = d, i
		}
	}
	return enB, at
}

// ── 1. ALTIN TEST ───────────────────────────────────────────────────────────

// sesAltinSHA256 — Adım 0'da MEVCUT kodla alınan değer (durmasız akış, 3000
// paket, 264 130 bayt). Kasıtlı biçim değişikliğinde gerekçesiyle güncellenir.
const sesAltinSHA256 = "8c97274e84e54477b811414ecabdffc51151c55bf69111494f7650a35eb3b2a6"

func sesAltinDizisi(k *sahteSesKaynak) ([]spaket, *yayinci) {
	y := yeniYayinci(time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC), 1_000_000)
	var out []spaket
	for i := 0; i < 3000; i++ {
		switch {
		case i%400 == 199:
			y.dtx(3000) // 3 sn DTX sessizliği
		case i%700 == 350:
			y.kayip(3) // üç paket kaybı
		}
		out = append(out, y.paket())
		if i%100 == 0 {
			y.srEkle(k) // 2 sn'de bir SR
		}
	}
	return out, y
}

func TestSesAltinDurmasiz(t *testing.T) {
	dir := t.TempDir()
	k := &sahteSesKaynak{}
	dizi, y := sesAltinDizisi(k)
	w := yeniSesYazici(t, dir, k, false)
	yol := sesBesle(t, w, k, dir, dizi, birBlok)
	w.Close()
	if d := w.düşen.Load(); d != 0 {
		t.Fatalf("test kanalı taşırdı: %d paket düştü", d)
	}
	ham, _ := os.ReadFile(yol)
	sum := sha256.Sum256(ham)
	hexsum := hex.EncodeToString(sum[:])
	pk := oggPaketleriOku(t, yol)
	t.Logf("altın: bayt=%d paket=%d sha256=%s", len(ham), len(pk), hexsum)
	if len(pk) != len(dizi) {
		t.Fatalf("paket sayısı %d, beklenen %d", len(pk), len(dizi))
	}
	// Granül = RTP göreli + 960: DTX ve kayıp boşlukları granülde DURMALI.
	for i, p := range pk {
		bek := uint64(dizi[i].rtp-dizi[0].rtp) + 960
		if p.granul != bek {
			t.Fatalf("paket %d granül %d, beklenen %d", i, p.granul, bek)
		}
	}
	if enB, at := enBuyukSapma(t, pk, y.dogru); enB > 0.0005 {
		t.Fatalf("durmasız akışta sapma %.4f sn (paket %d)", enB, at)
	}
	yan := yanOku(t, yol)
	if yan["capa_kaynak"] != "sfu-sr-fit" {
		t.Fatalf("çapa kaynağı %v, beklenen sfu-sr-fit", yan["capa_kaynak"])
	}
	if artik, _ := yan["capa_artik_ms"].(float64); artik > 1 {
		t.Fatalf("durmasız akışta çapa artığı %.1f ms (≈0 olmalı)", artik)
	}
	oz := ozDenetim(t, yan)
	if oz["bolum"].(float64) != 1 || oz["durum"] != "tutarli" {
		t.Fatalf("durmasız akışta bölüm/durum yanlış: %v", oz)
	}
	if hexsum != sesAltinSHA256 {
		t.Fatalf("BİREBİR DEĞİL: %s, altın %s", hexsum, sesAltinSHA256)
	}
}

// ── 2. GERÇEK VERİ — kayıt 911 ──────────────────────────────────────────────

type fikstur struct {
	Etiket       string         `json:"etiket"`
	FirstRTP     uint32         `json:"first_rtp"`
	FirstFrameAt string         `json:"first_frame_at"`
	SR           []srOrnek      `json:"sr_gunlugu"`
	Paket        [][2]int64     `json:"paket"`
	Saglik       map[string]any `json:"saglik"`
}

func fiksturYukle(t *testing.T, etiket string) *fikstur {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "fx911", "fixture_911"+etiket+".json.gz"))
	if err != nil {
		t.Skipf("fikstür yok: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	var fx fikstur
	if err := json.NewDecoder(gz).Decode(&fx); err != nil {
		t.Fatalf("fikstür çözülemedi: %v", err)
	}
	return &fx
}

// fiksturPaketleri — fikstürden paket dizisi: rtp gerçek, varış = gerçek
// konum + ±11 ms deterministik titreme. `rtpGercek`: gerçek konum tarayıcı
// hizalaması yerine RTP'nin kendisi (öğretmen kontrolü). Eşleşmeyen
// paketler için komşunun farkı sürdürülür. EOS boş paketi atlanır.
func fiksturPaketleri(t *testing.T, fx *fikstur, k *sahteSesKaynak, rtpGercek bool) (dizi []spaket, dogru []int64) {
	t.Helper()
	t0, err := time.Parse(time.RFC3339Nano, fx.FirstFrameAt)
	if err != nil {
		t.Fatalf("first_frame_at: %v", err)
	}
	for _, sr := range fx.SR {
		k.srler = append(k.srler, &livekit.RTCPSenderReportState{
			RtpTimestamp: sr.RTP, NtpTimestamp: ntpTS(sr.NtpNs), At: sr.AtNs, AtAdjusted: sr.AtNs})
	}
	sort.SliceStable(k.srler, func(i, j int) bool { return k.srler[i].At < k.srler[j].At })
	var fark int64 // tarayıcı − sfu (son eşleşen)
	seq := uint16(1)
	for i, p := range fx.Paket {
		g, tg := p[0], p[1]
		if g <= 0 {
			continue // EOS
		}
		if tg >= 0 && !rtpGercek {
			fark = tg - g
		}
		konum := g + fark - 960 // gerçek eksendeki başlangıç (örnek)
		rel := uint32(g - 960)
		jitter := time.Duration((i*7919)%23-11) * time.Millisecond
		dizi = append(dizi, spaket{
			rtp: fx.FirstRTP + rel, seq: seq,
			gelis: t0.Add(time.Duration(konum * 1e9 / 48000)).Add(jitter),
			yuk:   opusYuk(i),
		})
		dogru = append(dogru, konum)
		seq++
	}
	return dizi, dogru
}

// sesAyrisma — çıktı ile gerçek konum farkı (sn): son paket, en büyük mutlak
// fark, 60 ms üstündeki paket oranı.
func sesAyrisma(t *testing.T, pk []oggPaket, dogru []int64) (son, enBuyuk, kotuOran float64) {
	t.Helper()
	if len(pk) != len(dogru) {
		t.Fatalf("çıktı paket sayısı %d, beslenen %d", len(pk), len(dogru))
	}
	kotu := 0
	for i, p := range pk {
		d := konumSn(p) - float64(dogru[i])/48000.0
		if math.Abs(d) > enBuyuk {
			enBuyuk = math.Abs(d)
		}
		if math.Abs(d) > 0.060 {
			kotu++
		}
		son = d
	}
	return son, enBuyuk, float64(kotu) / float64(len(pk))
}

func sesFiksturKos(t *testing.T, etiket string, rtpGercek bool) ([]oggPaket, []int64, map[string]any) {
	t.Helper()
	fx := fiksturYukle(t, etiket)
	k := &sahteSesKaynak{}
	dizi, dogru := fiksturPaketleri(t, fx, k, rtpGercek)
	dir := t.TempDir()
	w := yeniSesYazici(t, dir, k, false)
	yol := sesBesle(t, w, k, dir, dizi, birBlok)
	k.ilet(w, 1<<62)
	w.Close()
	if d := w.düşen.Load(); d != 0 {
		t.Fatalf("test kanalı taşırdı: %d paket düştü", d)
	}
	return oggPaketleriOku(t, yol), dogru, yanOku(t, yol)
}

// TestSes911A — öğrenci A: 4 kopuş (12,9 / 73,9 / 24,8 / 3,5 sn), toplam
// 115,0 sn eksik. Adım 0'da mevcut kod sonda −115,031 sn, %98,4 paket 60 ms
// üstü, çapa artığı 22 936 ms verdi. Adım 2 ile: tarayıcı konumuna oturur.
func TestSes911A(t *testing.T) {
	pk, dogru, yan := sesFiksturKos(t, "A", false)
	son, enBuyuk, kotu := sesAyrisma(t, pk, dogru)
	oz := ozDenetim(t, yan)
	t.Logf("911A: paket=%d son_fark=%+.3f sn en_buyuk=%.3f sn 60ms_ustu=%.2f%% capa_artik_ms=%v ppm=%v oz=%v",
		len(pk), son, enBuyuk, kotu*100, yan["capa_artik_ms"], yan["capa_kayma_ppm"], oz)
	for _, b := range bolumler(t, yan) {
		t.Logf("   bölüm %v: kaynak=%v varis_eksik=%v sr_eksik=%v kesin=%v pts0=%.2f sn paket=%v",
			b["sira"], b["kaynak"], b["varis_eksik_sn"], b["sr_eksik_sn"], b["kesin"],
			b["pts0"].(float64)/48000, b["paket"])
	}
	if math.Abs(son) > 0.100 {
		t.Fatalf("911A sonda %.3f sn ayrışıyor (≤0,1 sn olmalı)", son)
	}
	if kotu > 0.01 {
		t.Fatalf("911A paketlerin %%%.2f'i 60 ms'den fazla kaymış (≤%%1 olmalı)", kotu*100)
	}
	if artik, _ := yan["capa_artik_ms"].(float64); artik > 50 {
		t.Fatalf("911A çapa artığı %.0f ms (≤50 olmalı)", artik)
	}
	if oz["durum"] != "tutarli" {
		t.Fatalf("911A öz denetim: %v", oz)
	}
	// rawrec52: rapor basamağı 911A'da iki kez daha böler (~17 ms; gerçek
	// kayma mı Chrome SR salınımı mı bilinmiyor, ölçütler aynı ya da daha
	// iyi: son −0,015, %0,41). 4 kopuş + en çok 2 basamak.
	if n := oz["bolum"].(float64); n < 4 || n > 8 {
		t.Fatalf("911A bölüm sayısı %v (4 kopuş + ≤2 rapor basamağı bekleniyor)", n)
	}
}

// TestSes911T — öğretmen (kontrol): durma yok; çıktı RTP konumuyla birebir.
func TestSes911T(t *testing.T) {
	pk, dogru, yan := sesFiksturKos(t, "T", true)
	son, enBuyuk, kotu := sesAyrisma(t, pk, dogru)
	oz := ozDenetim(t, yan)
	t.Logf("911T: paket=%d son_fark=%+.3f sn en_buyuk=%.3f sn 60ms_ustu=%.2f%% capa_artik_ms=%v ppm=%v oz=%v",
		len(pk), son, enBuyuk, kotu*100, yan["capa_artik_ms"], yan["capa_kayma_ppm"], oz)
	if enBuyuk > 0.0005 {
		t.Fatalf("911T kontrolde sapma: en büyük %.4f sn", enBuyuk)
	}
	if artik, _ := yan["capa_artik_ms"].(float64); artik > 20 {
		t.Fatalf("911T çapa artığı %.1f ms (≤20 olmalı)", artik)
	}
	if oz["bolum"].(float64) != 1 || oz["durum"] != "tutarli" {
		t.Fatalf("911T bölüm/durum: %v", oz)
	}
	if yan["participant"] != "test-kisi" {
		t.Fatalf("ses yan JSON'unda participant yok/yanlış: %v", yan["participant"])
	}
}

// ── 3. SENTETİK SENARYOLAR ──────────────────────────────────────────────────

func t0() time.Time { return time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC) }

// dizi — yayıncı modelinden `n` paketlik dizi; `olay(i)` i. paketten ÖNCE
// çağrılır (dtx/dur/kayıp/mute için). SR her `srHer` pakette.
func dizi(k *sahteSesKaynak, y *yayinci, n, srHer int, olay func(i int, y *yayinci) (mute bool)) []spaket {
	var out []spaket
	for i := 0; i < n; i++ {
		mute := false
		if olay != nil {
			mute = olay(i, y)
		}
		p := y.paket()
		p.mute = mute
		out = append(out, p)
		if srHer > 0 && i%srHer == 0 {
			y.srEkle(k)
		}
	}
	return out
}

// TestSesDurmaSinyalsizUzun — 3 sn durmuş saat, mute sinyali YOK → 2 sn
// eşiğini aşar, bölüm açılır; SR ile taban kesinleşir → konum tam.
func TestSesDurmaSinyalsizUzun(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	d := dizi(k, y, 600, 50, func(i int, y *yayinci) bool {
		if i == 300 {
			y.dur(3000)
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	enB, at := enBuyukSapma(t, pk, y.dogru)
	oz := ozDenetim(t, yan)
	bl := bolumler(t, yan)
	t.Logf("sinyalsiz 3 sn: en büyük sapma %.4f sn (paket %d), oz=%v", enB, at, oz)
	if enB > 0.002 {
		t.Fatalf("sapma %.4f sn (SR ile kesinleşince ≤2 ms olmalı)", enB)
	}
	if len(bl) != 2 || bl[1]["kesin"] != true || bl[1]["mute_sinyali"] != false {
		t.Fatalf("bölümler yanlış: %v", bl)
	}
	if e := bl[1]["sr_eksik_sn"].(float64); math.Abs(e-3.0) > 0.002 {
		t.Fatalf("sr_eksik_sn %.4f, beklenen 3,000", e)
	}
	if oz["durum"] != "tutarli" || oz["kesin_bolum"].(float64) != 2 {
		t.Fatalf("öz denetim: %v", oz)
	}
}

// TestSesDurmaSinyalsizKisa — 1 sn durma, sinyal yok → eşiğin (2 sn) altı:
// DÜZELTİLMEZ (LiveKit ile aynı; kuyruk gecikmesiyle karışmasın diye).
// Belgelenmiş sınırlama — ileride SR ile geriye dönük bölüm (plan §11).
func TestSesDurmaSinyalsizKisa(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	d := dizi(k, y, 400, 50, func(i int, y *yayinci) bool {
		if i == 200 {
			y.dur(1000)
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	enB, at := enBuyukSapma(t, pk, y.dogru)
	oz := ozDenetim(t, yan)
	bl := bolumler(t, yan)
	// rawrec52: varış kuralı 2 sn eşiğinin altını görmez ama RAPOR BASAMAĞI
	// görür — üç SR sonra bölüm hıçkırık paketinden bölünür, kuyruk kayar.
	// (rawrec41-51'de "düzeltilmez, karne 1 sn açık söyler" idi.)
	t.Logf("sinyalsiz 1 sn: sapma %.4f (paket %d) oz=%v bl=%v", enB, at, oz, bl)
	if enB > 0.003 || oz["sr_adim"].(float64) != 1 || len(bl) != 2 || bl[1]["kaynak"] != "sr-adim" {
		t.Fatalf("sinyalsiz 1 sn durma rapor basamağıyla düzelmeliydi: sapma %.4f oz=%v bl=%v", enB, oz, bl)
	}
	if oz["durum"] != "tutarli" {
		t.Fatalf("öz denetim tutarlı olmalı: %v", oz)
	}
}

// TestSesDurmaSinyalli — mute sinyaliyle 0,5 sn durma → 0,2 sn eşiği: bölüm
// açılır ve kesinleşir. 0,1 sn durma → eşiğin altı, açılmaz.
func TestSesDurmaSinyalli(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	d := dizi(k, y, 600, 50, func(i int, y *yayinci) bool {
		switch i {
		case 200:
			y.dur(500)
			return true // mute sinyali bu paketten önce
		case 400:
			y.dur(100)
			return true
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	oz := ozDenetim(t, yan)
	bl := bolumler(t, yan)
	// Paket 200..399: 0,5 sn sinyalli durma → bölüm, SR ile kesin. 400+: 0,1 sn
	// eşik altı; rawrec41-51'de düzeltilmezdi, rawrec52 RAPOR BASAMAĞI üç SR
	// sonra hıçkırık paketinden (400) böler, kuyruk kayar → sapma 0.
	for i, p := range pk {
		d := konumSn(p) - float64(y.dogru[i])/48000.0
		if math.Abs(d) > 0.003 {
			t.Fatalf("paket %d sapma %.4f (sinyalli 0,5 sn ve eşik altı 0,1 sn ikisi de düzelmeli)", i, d)
		}
	}
	if len(bl) != 3 || bl[1]["mute_sinyali"] != true || bl[1]["kesin"] != true || bl[2]["kaynak"] != "sr-adim" {
		t.Fatalf("bölümler: %v", bl)
	}
	t.Logf("sinyalli: bölümler=%d oz=%v", len(bl), oz)
}

// TestSesDTXveKayip — DTX ve paket kaybı bölüm AÇMAZ (RTP duvarla yürüyor).
func TestSesDTXveKayip(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	d := dizi(k, y, 500, 50, func(i int, y *yayinci) bool {
		switch i {
		case 100:
			y.dtx(5000) // 5 sn sessizlik: eşiğin çok üstünde ama RTP de yürüdü
		case 200:
			y.kayip(150) // 3 sn paket kaybı
		case 300:
			y.dtx(30000) // 30 sn sessizlik
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	enB, _ := enBuyukSapma(t, pk, y.dogru)
	oz := ozDenetim(t, yan)
	if enB > 0.0005 || oz["bolum"].(float64) != 1 || oz["durum"] != "tutarli" {
		t.Fatalf("DTX/kayıp bölüm açtı ya da saptı: sapma %.4f oz=%v", enB, oz)
	}
}

// TestSesKuyrukPatlamasi — paketler 2,5 sn kuyrukta bekleyip toplu gelir
// (RTP doğru, varış geç): sinyalsiz eşik (2 sn) aşılır → YANLIŞ ALARM bölümü
// açılır; ama SR o bölümün tabanını GERÇEK yerine çeker → çıktı yine tam.
func TestSesKuyrukPatlamasi(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	y.jitter = func(i int) time.Duration {
		if i >= 300 && i < 360 {
			return 2500 * time.Millisecond
		}
		return 0
	}
	d := dizi(k, y, 700, 50, nil)
	pk, yan := sesKos(t, k, d)
	enB, at := enBuyukSapma(t, pk, y.dogru)
	oz := ozDenetim(t, yan)
	bl := bolumler(t, yan)
	t.Logf("kuyruk patlaması: sapma %.4f (paket %d) bölümler=%d oz=%v", enB, at, len(bl), oz)
	if enB > 0.002 {
		t.Fatalf("yanlış alarm SR ile geri alınmalıydı: sapma %.4f sn", enB)
	}
	if len(bl) < 2 || bl[1]["kesin"] != true {
		t.Fatalf("bölüm açılıp SR ile kesinleşmeliydi: %v", bl)
	}
	if e := bl[1]["sr_eksik_sn"].(float64); math.Abs(e) > 0.002 {
		t.Fatalf("SR eksiği ≈0 olmalı (durma yoktu): %.4f", e)
	}
}

// TestSesSRIncelik — durma sonrası ilk paket 80 ms geç varır (titreme):
// geçici taban 80 ms hatalı, SR gelince kesin. Yan JSON iki ölçüyü de taşır.
func TestSesSRIncelik(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	y.jitter = func(i int) time.Duration {
		if i == 300 {
			return 80 * time.Millisecond
		}
		return 0
	}
	d := dizi(k, y, 600, 50, func(i int, y *yayinci) bool {
		if i == 300 {
			y.dur(3000)
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	enB, _ := enBuyukSapma(t, pk, y.dogru)
	bl := bolumler(t, yan)
	if len(bl) != 2 {
		t.Fatalf("bölümler: %v", bl)
	}
	v, s := bl[1]["varis_eksik_sn"].(float64), bl[1]["sr_eksik_sn"].(float64)
	t.Logf("SR inceliği: varış %.3f sn, SR %.3f sn, sapma %.4f", v, s, enB)
	if math.Abs(v-3.08) > 0.005 || math.Abs(s-3.0) > 0.002 || enB > 0.002 {
		t.Fatalf("varış %.3f (≈3,08), SR %.3f (≈3,00), sapma %.4f (≤2 ms)", v, s, enB)
	}
}

// TestSesRTPSarma — 32 bit RTP sarması, hem akışta hem durmanın içinde.
func TestSesRTPSarma(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 0xFFFFFFFF-150*960)
	d := dizi(k, y, 600, 50, func(i int, y *yayinci) bool {
		if i == 149 {
			y.dur(3000) // sarma bu bölüm başında
		}
		if i == 400 {
			y.dur(2500)
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	enB, at := enBuyukSapma(t, pk, y.dogru)
	oz := ozDenetim(t, yan)
	if enB > 0.002 || oz["bolum"].(float64) != 3 || oz["durum"] != "tutarli" {
		t.Fatalf("sarma: sapma %.4f (paket %d) oz=%v", enB, at, oz)
	}
}

// TestSesBozukIlkSR — ilk SR bozuk (RTP 1 saat ileri): çapa ona kurulmaz,
// iki uyumlu SR beklenir; durma yine düzelir; fit bozuk örneği ayıklar.
func TestSesBozukIlkSR(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	k.srEkle(y.rtp+3600*48000, y.wallNs+10*int64(time.Millisecond)) // bozuk
	d := dizi(k, y, 800, 50, func(i int, y *yayinci) bool {
		if i == 400 {
			y.dur(3000)
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	enB, at := enBuyukSapma(t, pk, y.dogru)
	oz := ozDenetim(t, yan)
	t.Logf("bozuk ilk SR: sapma %.4f (paket %d) artık=%v oz=%v", enB, at, yan["capa_artik_ms"], oz)
	if enB > 0.002 || oz["bolum"].(float64) != 2 || oz["kesin_bolum"].(float64) != 2 {
		t.Fatalf("bozuk ilk SR'la düzeltme bozuldu: sapma %.4f oz=%v", enB, oz)
	}
	if artik, _ := yan["capa_artik_ms"].(float64); artik > 5 {
		t.Fatalf("fit bozuk SR'ı ayıklamalıydı: artık %.1f ms", artik)
	}
}

// TestSesBayatIlkPaket — unmute sonrası ilk paket bayat damgalı (bir önceki
// bölümün RTP'si), hemen ardından doğru damgalar: taban ikinciye kurulur.
func TestSesBayatIlkPaket(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	var d []spaket
	d = append(d, dizi(k, y, 300, 50, nil)...)
	y.dur(3000)
	bayat := y.paket()
	bayat.rtp -= 3000 * 48       // durmadan ÖNCEKİ saatten kalma damga
	y.dogru[len(y.dogru)-1] = -1 // bayat paketin yeri tanımsız, sayılmaz
	d = append(d, bayat)
	d = append(d, dizi(k, y, 300, 50, nil)...)
	pk, yan := sesKos(t, k, d)
	if len(pk) != len(d) {
		t.Fatalf("paket sayısı %d, beklenen %d", len(pk), len(d))
	}
	enB := 0.0
	for i, p := range pk {
		if y.dogru[i] < 0 {
			continue
		}
		if dd := math.Abs(konumSn(p) - float64(y.dogru[i])/48000.0); dd > enB {
			enB = dd
		}
	}
	oz := ozDenetim(t, yan)
	t.Logf("bayat ilk paket: sapma %.4f, bayat=%v oz=%v", enB, oz["bayat_paket"], oz)
	if enB > 0.002 || oz["bayat_paket"].(float64) < 1 {
		t.Fatalf("bayat paket düzeltmesi çalışmadı: sapma %.4f oz=%v", enB, oz)
	}
}

// TestSesDosyaAdiCakismasi — aynı trackID ile ikinci yazıcı (tam yeniden
// bağlanma): ilk dosya EZİLMEZ, `02_sfu_…` açılır.
func TestSesDosyaAdiCakismasi(t *testing.T) {
	dir := t.TempDir()
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	d1 := dizi(k, y, 100, 50, nil)
	w1 := yeniSesYazici(t, dir, k, false)
	yol1 := sesBesle(t, w1, k, dir, d1, birBlok)
	w1.Close()
	st1, _ := os.Stat(yol1)

	y.dur(5000)
	d2 := dizi(k, y, 100, 50, nil)
	w2 := NewWriter("cid-ses", &livekit.TrackInfo{Sid: "TR_SES2", Source: livekit.TrackSource_MICROPHONE},
		48000, false, k, logger.GetLogger())
	var hedef uint64
	for _, p := range d2 {
		hedef++
		sesYaz(w2, k, p, hedef)
		time.Sleep(3 * time.Millisecond)
	}
	w2.Close()
	m, _ := filepath.Glob(filepath.Join(dir, "audioraw_1", "*.opus"))
	if len(m) != 2 {
		t.Fatalf("iki dosya bekleniyordu, %d var: %v", len(m), m)
	}
	st1b, _ := os.Stat(yol1)
	if st1b.Size() != st1.Size() {
		t.Fatalf("ilk dosya ezildi: %d → %d", st1.Size(), st1b.Size())
	}
	if filepath.Base(m[1]) != "02_sfu_cid-ses.opus" && filepath.Base(m[0]) != "02_sfu_cid-ses.opus" {
		t.Fatalf("ikinci dosya adı 02_sfu_ olmalı: %v", m)
	}
	if pk := oggPaketleriOku(t, yol1); len(pk) != 100 {
		t.Fatalf("ilk dosyada %d paket, 100 bekleniyordu", len(pk))
	}
}

// ── RED (RFC 2198) yedek bloğu — durmuş saatle birlikte ────────────────────

// redSar — [yedek başlığı (F=1, ofset 960, uzunluk)] [birincil başlığı (F=0)]
// [yedek veri] [birincil veri]. PT 111.
func redSar(yedek, birincil []byte) []byte {
	var out []byte
	if yedek != nil {
		ofset, uz := 960, len(yedek)
		out = append(out, 0x80|111, byte(ofset>>6), byte((ofset&0x3F)<<2|(uz>>8)), byte(uz&0xFF))
	}
	out = append(out, 111)
	out = append(out, yedek...)
	out = append(out, birincil...)
	return out
}

// TestSesREDYedek — kayıp paket yedekten kurtulur, kopya yedek atılır,
// durmuş saat yedeklerle de doğru; çıktı paket sayısı = üretilen paket
// sayısı (kayıp dahil, kopya hariç).
func TestSesREDYedek(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	var ham []spaket
	for i := 0; i < 500; i++ {
		if i == 250 {
			y.dur(3000)
		}
		ham = append(ham, y.paket())
		if i%50 == 0 {
			y.srEkle(k)
		}
	}
	// RED sarmalı: her paket bir öncekinin yedeğini taşır; paket 100 ve 260
	// KAYIP (gönderilmez), yedekleri 101 ve 261'den gelir.
	var d []spaket
	bloklar := map[int]uint64{}
	for i, p := range ham {
		if i == 100 || i == 260 {
			continue
		}
		q := p
		if i > 0 {
			q.yuk = redSar(ham[i-1].yuk, p.yuk)
			bloklar[len(d)] = 2
		} else {
			q.yuk = redSar(nil, p.yuk)
			bloklar[len(d)] = 1
		}
		d = append(d, q)
	}
	dir := t.TempDir()
	w := yeniSesYazici(t, dir, k, true)
	yol := sesBesle(t, w, k, dir, d, func(i int) uint64 { return bloklar[i] })
	w.Close()
	pk := oggPaketleriOku(t, yol)
	if len(pk) != len(ham) {
		t.Fatalf("çıktı %d paket, beklenen %d (kayıplar yedekten kurtulmalı, kopyalar atılmalı)", len(pk), len(ham))
	}
	enB, at := enBuyukSapma(t, pk, y.dogru)
	yan := yanOku(t, yol)
	sag, _ := yan["saglik"].(map[string]any)
	t.Logf("RED: sapma %.4f (paket %d) red_kurtarilan=%v oz=%v", enB, at, sag["red_kurtarilan"], ozDenetim(t, yan))
	if enB > 0.002 {
		t.Fatalf("RED'li akışta sapma %.4f sn", enB)
	}
	if kur, _ := sag["red_kurtarilan"].(float64); kur != 2 {
		t.Fatalf("red_kurtarilan %v, beklenen 2", sag["red_kurtarilan"])
	}
	for i := 1; i < len(pk); i++ {
		if pk[i].granul <= pk[i-1].granul {
			t.Fatalf("granül artan değil: paket %d", i)
		}
	}
}

// ── YAKALAMA SAATİ (abs-capture-time, Adım 6) ───────────────────────────────

// TestSesYakalamaSaatiKisaDurma — 0,5 sn SİNYALSİZ durma: varış kuralı (2 sn
// eşiği) görmezdi; yakalama saati 100 ms eşiğiyle görür, taban KESİN
// (SR'a gerek yok), kaynak "act".
func TestSesYakalamaSaatiKisaDurma(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	y.act = true
	y.jitter = func(i int) time.Duration { return time.Duration((i*7919)%23-11) * time.Millisecond }
	d := dizi(k, y, 600, 50, func(i int, y *yayinci) bool {
		if i == 300 {
			y.dur(500)
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	enB, at := enBuyukSapma(t, pk, y.dogru)
	oz := ozDenetim(t, yan)
	bl := bolumler(t, yan)
	t.Logf("yakalama 0,5 sn: sapma %.4f (paket %d) oz=%v", enB, at, oz)
	if enB > 0.0005 {
		t.Fatalf("yakalama saatiyle sapma %.4f sn (≈0 olmalı)", enB)
	}
	if len(bl) != 2 || bl[1]["kaynak"] != "act" || bl[1]["kesin"] != true {
		t.Fatalf("bölümler: %v", bl)
	}
	if e := bl[1]["sr_eksik_sn"].(float64); math.Abs(e-0.5) > 0.001 {
		t.Fatalf("act eksik %.4f, beklenen 0,500", e)
	}
	if oz["act_oran"].(float64) != 1 || oz["durum"] != "tutarli" {
		t.Fatalf("öz denetim: %v", oz)
	}
}

// TestSesYakalamaSaatiDTXKayipPatlama — DTX, kayıp ve 2,5 sn kuyruk
// patlaması: yakalama saati hepsini "boşluk değil" diye görür; hiç bölüm
// açılmaz (varış kuralı patlamada yanlış alarm verirdi).
func TestSesYakalamaSaatiDTXKayipPatlama(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	y.act = true
	y.jitter = func(i int) time.Duration {
		if i >= 400 && i < 460 {
			return 2500 * time.Millisecond
		}
		return 0
	}
	d := dizi(k, y, 700, 50, func(i int, y *yayinci) bool {
		switch i {
		case 100:
			y.dtx(5000)
		case 200:
			y.kayip(150)
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	enB, at := enBuyukSapma(t, pk, y.dogru)
	oz := ozDenetim(t, yan)
	bl := bolumler(t, yan)
	// rawrec49: 2,5 sn'lik patlamada ilk paketin damgası "durmadı" diyor,
	// varış "durdu" → damga şüpheli sayılır, varışla geçici bölüm açılır;
	// bir sonraki damga bölümü 2,5 sn geri çekip kesinleştirir (kuyruk
	// kayar). Sonuç: yer yine tam, bölüm "varis+act".
	t.Logf("DTX+kayıp+patlama: sapma %.4f (paket %d) oz=%v bl=%v", enB, at, oz, bl)
	if enB > 0.0005 || len(bl) != 2 || bl[1]["kaynak"] != "varis+act" || bl[1]["kesin"] != true || oz["act_bayat"].(float64) != 1 {
		t.Fatalf("patlama damgayla geri çekilmeliydi: sapma %.4f (paket %d) oz=%v bl=%v", enB, at, oz, bl)
	}
}

// TestSesYakalamaSaatiMuteUzun — mute sinyali + 20 sn durma, yakalama saati
// var: bölüm "act" ve kesin; SR kesinleştirmesi gerekmez, gelse de dokunmaz.
func TestSesYakalamaSaatiMuteUzun(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	y.act = true
	d := dizi(k, y, 600, 50, func(i int, y *yayinci) bool {
		if i == 300 {
			y.dur(20000)
			return true
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	enB, _ := enBuyukSapma(t, pk, y.dogru)
	bl := bolumler(t, yan)
	if enB > 0.0005 || len(bl) != 2 || bl[1]["kaynak"] != "act" || bl[1]["mute_sinyali"] != true {
		t.Fatalf("sapma %.4f bölümler %v", enB, bl)
	}
}

// ── rawrec45: HAM SR + YAYINCI SAATİ ─────────────────────────────────────────

// srGunlugu — yan JSON'daki kabul edilmiş SR sayısı.
func srGunlugu(t *testing.T, yan map[string]any) int {
	t.Helper()
	g, _ := yan["sr_gunlugu"].([]any)
	return len(g)
}

// TestSesSusturmaIciSRAtilir — KAYIT 915 MODELİ. Susturma boyunca Chrome
// RTP'si tahmini (yavaş ilerleyen) SR göndermeyi sürdürür; açılıştan sonra
// gerçek SR'lar gelir. LiveKit'in istatistik katmanı gerçek olanları
// "sırasız" diye atıyordu → bölüm hiç kesinleşmiyordu. Ham SR yolu hepsini
// alır, susturma içindekileri varış penceresiyle eler, gerçek olanla
// tabanı kesinleştirir.
func TestSesSusturmaIciSRAtilir(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	y.jitter = func(i int) time.Duration {
		if i == 200 {
			return 45 * time.Millisecond // açılış sonrası ilk paket geç: varış tahmini 45 ms şaşar
		}
		return 0
	}
	uydurma := 0
	d := dizi(k, y, 700, 150, func(i int, y *yayinci) bool {
		if i == 200 {
			// 60 sn susturma; her 5 sn'de RTP'si ~8 kHz hızında "ilerleyen"
			// uydurma SR (915'te ölçülen: 8–18 kHz).
			for adim := 1; adim <= 12; adim++ {
				y.dur(5000)
				k.srEkle(y.rtp+uint32(adim*5*8000), y.wallNs)
				uydurma++
			}
			return true
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	enB, at := enBuyukSapma(t, pk, y.dogru)
	bl := bolumler(t, yan)
	oz := ozDenetim(t, yan)
	t.Logf("susturma içi SR: sapma %.4f (paket %d) sr_gunlugu=%d oz=%v bl=%v", enB, at, srGunlugu(t, yan), oz, bl)
	if len(bl) != 2 || bl[1]["kesin"] != true || bl[1]["kaynak"] != "mute+varis+sr" {
		t.Fatalf("açılış sonrası bölüm gerçek SR ile kesinleşmeliydi: %v", bl)
	}
	if oz["sr_atlanan"].(float64) != float64(uydurma) {
		t.Fatalf("susturma içi %d uydurma SR'ın hepsi elenmeliydi: oz=%v", uydurma, oz)
	}
	if srGunlugu(t, yan) != 5 {
		t.Fatalf("günlükte yalnız gerçek SR'lar olmalı (5): %d", srGunlugu(t, yan))
	}
	// TEŞHİS (rawrec46): bölümün ilk 16 paketi [varış ms, rtp ms]. Burada
	// ilk paket 45 ms geç geldi (jitter), sonrakiler tam zamanında → 16.
	// paket varışta 300−45 = 255 ms, RTP'de 300 ms. Desen tam bunu gösterir.
	ip, _ := bl[1]["ilk_paketler"].([]any)
	if len(ip) != 16 {
		t.Fatalf("ilk_paketler 16 olmalı: %d", len(ip))
	}
	son, _ := ip[15].([]any)
	if son[0].(float64) != 255 || son[1].(float64) != 300 {
		t.Fatalf("ilk_paketler[15] [255 300] olmalı: %v", son)
	}
	if enB > 0.002 {
		t.Fatalf("sapma %.4f sn (SR kesinleştirince ≤2 ms)", enB)
	}
}

// TestSesTekSRCapa — ilk bölüm kısa, TEK SR almış (çapa doğrulanamıyor);
// sonraki bölüm o tek adayla, dar payla (0,5 sn) yine kesinleşir.
func TestSesTekSRCapa(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	y.jitter = func(i int) time.Duration {
		if i == 100 {
			return 60 * time.Millisecond
		}
		return 0
	}
	d := dizi(k, y, 600, 250, func(i int, y *yayinci) bool { // SR: i=0 (tek), 250, 500
		if i == 100 {
			y.dur(4000)
			return true
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	enB, at := enBuyukSapma(t, pk, y.dogru)
	bl := bolumler(t, yan)
	oz := ozDenetim(t, yan)
	t.Logf("tek SR çapa: sapma %.4f (paket %d) oz=%v bl=%v", enB, at, oz, bl)
	if len(bl) != 2 || bl[1]["kesin"] != true || oz["kesin_bolum"].(float64) != 2 {
		t.Fatalf("tek adayla kesinleşmeliydi: oz=%v bl=%v", oz, bl)
	}
	if enB > 0.002 {
		t.Fatalf("sapma %.4f sn (≤2 ms)", enB)
	}
}

// TestSesTekBozukSRCapa — tek aday BOZUK (crbug 168328: RTP 1 saat ileri):
// dar pay onu eler, bölüm geçici (varış) tabanla kalır, sapma titreme kadar.
func TestSesTekBozukSRCapa(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	y.jitter = func(i int) time.Duration {
		if i == 100 {
			return 60 * time.Millisecond
		}
		return 0
	}
	d := dizi(k, y, 600, 250, func(i int, y *yayinci) bool {
		if i == 100 {
			y.dur(4000)
			return true
		}
		return false
	})
	k.srler[0].RtpTimestamp += 3600 * 48000 // bozuk ilk SR
	pk, yan := sesKos(t, k, d)
	enB, at := enBuyukSapma(t, pk, y.dogru)
	bl := bolumler(t, yan)
	oz := ozDenetim(t, yan)
	t.Logf("tek bozuk SR: sapma %.4f (paket %d) oz=%v bl=%v", enB, at, oz, bl)
	if len(bl) != 2 || bl[1]["kesin"] != false || oz["sr_bozuk"].(float64) < 1 {
		t.Fatalf("bozuk tek aday elenmeli, bölüm geçici kalmalıydı: oz=%v bl=%v", oz, bl)
	}
	if enB > 0.065 {
		t.Fatalf("geçici taban titreme kadar (≤65 ms) şaşmalı: %.4f", enB)
	}
}

// TestSesSRVarisTitremesi — SR'lar ±80 ms titremeyle VARIYOR (ağ), NTP/RTP
// çifti doğru. Yayıncı saatiyle kesinleştirme titremeden etkilenmez:
// eski (sunucu saatli) yol çapayı ve tabanı onlarca ms şaşırtırdı.
func TestSesSRVarisTitremesi(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	d := dizi(k, y, 800, 50, func(i int, y *yayinci) bool {
		if i == 400 {
			y.dur(3000)
			return true
		}
		return false
	})
	for i, sr := range k.srler {
		sr.At += int64((i%2)*2-1) * 80 * int64(time.Millisecond)
	}
	sort.SliceStable(k.srler, func(i, j int) bool { return k.srler[i].At < k.srler[j].At })
	pk, yan := sesKos(t, k, d)
	enB, at := enBuyukSapma(t, pk, y.dogru)
	bl := bolumler(t, yan)
	oz := ozDenetim(t, yan)
	t.Logf("SR varış titremesi: sapma %.4f (paket %d) oz=%v", enB, at, oz)
	if len(bl) != 2 || bl[1]["kesin"] != true {
		t.Fatalf("bölüm kesinleşmeliydi: %v", bl)
	}
	if enB > 0.002 {
		t.Fatalf("titremeli SR'la sapma %.4f sn (yayıncı saatiyle ≤2 ms olmalı)", enB)
	}
}

// ── rawrec47: RED birincil bloğa yakalama saati + erken ilk paket ─────────

// TestSesREDYakalamaSaati — mikrofon hep RED sarmalı gelir; yakalama saati
// yalnız birincil bloğa ulaşmalı (rawrec43-46 hiç ulaştırmıyordu → 914-916'da
// act_oran 0). Durma "act" yoluyla, kesin.
func TestSesREDYakalamaSaati(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	y.act = true
	var ham []spaket
	for i := 0; i < 300; i++ {
		if i == 150 {
			y.dur(3000)
		}
		ham = append(ham, y.paket())
		if i%50 == 0 {
			y.srEkle(k)
		}
	}
	var d []spaket
	bloklar := map[int]uint64{}
	for i, p := range ham {
		q := p
		if i > 0 {
			q.yuk = redSar(ham[i-1].yuk, p.yuk)
			bloklar[i] = 2
		} else {
			q.yuk = redSar(nil, p.yuk)
			bloklar[i] = 1
		}
		d = append(d, q)
	}
	dir := t.TempDir()
	w := yeniSesYazici(t, dir, k, true)
	yol := sesBesle(t, w, k, dir, d, func(i int) uint64 { return bloklar[i] })
	k.ilet(w, 1<<62)
	w.Close()
	pk, yan := oggPaketleriOku(t, yol), yanOku(t, yol)
	oz := ozDenetim(t, yan)
	bl := bolumler(t, yan)
	enB, at := enBuyukSapma(t, pk, y.dogru)
	t.Logf("RED + yakalama saati: sapma %.4f (paket %d) oz=%v", enB, at, oz)
	if oz["act_paket"].(float64) != 300 {
		t.Fatalf("RED birincil bloğun yakalama saati eşleyiciye ulaşmalı (300): %v", oz["act_paket"])
	}
	if len(bl) != 2 || bl[1]["kaynak"] != "act" || bl[1]["kesin"] != true {
		t.Fatalf("durma yakalama saatiyle kesin olmalıydı: %v", bl)
	}
	if enB > 0.002 {
		t.Fatalf("sapma %.4f sn", enB)
	}
}

// TestSesErkenIlkPaket — açılış sonrası İLK paket 200 ms erken gelir, akış
// sonra başlar (Safari, kayıt 917/918). SR gelmese de taban ikinci pakete
// taşınır: ilk paket dışında sapma ≤ 2 ms. Eski kural 200 ms şaşırırdı.
func TestSesErkenIlkPaket(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	y.jitter = func(i int) time.Duration {
		if i == 200 {
			return -200 * time.Millisecond
		}
		return 0
	}
	d := dizi(k, y, 600, 0, func(i int, y *yayinci) bool {
		if i == 0 || i == 150 {
			y.srEkle(k) // SR yalnız bölüm 0'da; bölüm 1 SR'sız kalır
		}
		if i == 200 {
			y.dur(30000)
			return true
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	oz := ozDenetim(t, yan)
	bl := bolumler(t, yan)
	var enB float64
	var ilk float64
	for i, p := range pk {
		fark := konumSn(p) - float64(y.dogru[i])/48000
		if fark < 0 {
			fark = -fark
		}
		if i == 200 {
			ilk = fark
			continue
		}
		if fark > enB {
			enB = fark
		}
	}
	t.Logf("erken ilk paket: ilk paket sapması %.3f, geri kalan en büyük %.4f oz=%v bl=%v", ilk, enB, oz, bl)
	if len(bl) != 2 || bl[1]["kesin"] != false || oz["erken_paket"].(float64) != 1 {
		t.Fatalf("erken ilk paket kuralı bir kez işlemeliydi, bölüm SR'sız kalmalıydı: oz=%v bl=%v", oz, bl)
	}
	if enB > 0.002 {
		t.Fatalf("ilk paket dışında sapma %.4f sn (≤2 ms)", enB)
	}
	if ilk < 0.19 || ilk > 0.21 {
		t.Fatalf("ilk paket varışta kalmalı (~0,2 sn): %.3f", ilk)
	}
}

// TestSesYakalamaSaatiSeyrek — Chrome yakalama saatini ~1/sn yazar (kayıt 919:
// 37/1706). Durma, son yakalama paketinden 0,98 sn sonra başlar; eski formül
// (`sonPts + dYak`) o süreyi bir kez daha ekleyip dosyayı uzatıyordu.
// Açılış sonrası ilk paket yakalama saati taşır (>1 sn boşluk), bölüm "act".
func TestSesYakalamaSaatiSeyrek(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	y.act = true
	y.actHer = 50
	d := dizi(k, y, 800, 100, func(i int, y *yayinci) bool {
		if i == 399 { // son yakalama paketi i=350: durma ondan 49 paket sonra
			y.dur(45000)
			return true
		}
		return false
	})
	// Açılış sonrası ilk paket yakalama saati taşısın (Chrome: 1 sn'den uzun
	// aradan sonraki ilk pakette gönderir).
	d[399].yak = d[399].gelis.UnixNano() - 40*int64(time.Millisecond) - 3*int64(time.Hour)
	pk, yan := sesKos(t, k, d)
	enB, at := enBuyukSapma(t, pk, y.dogru)
	oz := ozDenetim(t, yan)
	bl := bolumler(t, yan)
	t.Logf("seyrek yakalama: sapma %.4f (paket %d) oz=%v", enB, at, oz)
	if len(bl) != 2 || bl[1]["kaynak"] != "act" || bl[1]["kesin"] != true {
		t.Fatalf("durma yakalama saatiyle kesin olmalıydı: %v", bl)
	}
	if enB > 0.002 {
		t.Fatalf("seyrek yakalamada sapma %.4f sn (eski formül ~0,98 sn şaşırırdı)", enB)
	}
	if oz["durum"] != "tutarli" {
		t.Fatalf("öz denetim tutarlı olmalı: %v", oz)
	}
	// DAMGA GÜNLÜĞÜ (rawrec50): damgalı her paket için (pts, yak) — 17 örnek;
	// yak − pts/hz sabit (ofset −3 sa −40 ms), ses↔görüntü hizası buradan ölçülür.
	g, _ := yan["act_gunlugu"].([]any)
	if len(g) != 17 {
		t.Fatalf("act_gunlugu 17 örnek olmalı: %d", len(g))
	}
	ilk, son := g[0].(map[string]any), g[16].(map[string]any)
	c0 := ilk["yak_ns"].(float64) - ilk["pts"].(float64)*1e9/48000
	c1 := son["yak_ns"].(float64) - son["pts"].(float64)*1e9/48000
	if math.Abs(c1-c0) > 1e6 {
		t.Fatalf("damga çapası sabit kalmalı (fark %.1f ms)", (c1-c0)/1e6)
	}
}

// ── rawrec49: BAYAT YAKALAMA SAATİ (kayıt 920) ──────────────────────────────

// bayatDamgaDizisi — seyrek damga (Chrome ~1/sn), 48 sn susturma; açılış
// sonrası İLK paketin damgası BAYAT (susturma öncesi son çerçevenin saati,
// libwebrtc ACM yapışkan damga), sonraki gerçek damga `ikinciDamga`
// numaralı pakette. `kayip` verilirse o paketler gönderilmez.
func bayatDamgaDizisi(k *sahteSesKaynak, y *yayinci, ikinciDamga int, kayip map[int]bool) []spaket {
	y.act = true
	y.actHer = 50
	var d []spaket
	var sonYak int64
	for i := 0; i < 700; i++ {
		if i == 400 {
			y.dur(48000)
		}
		p := y.paket()
		p.mute = i == 400
		if i == 400 {
			// BAYAT: susturma öncesi son damga (i=350) + 50 paket, sanki hiç durulmamış
			p.yak = sonYak + int64(400-350)*20*int64(time.Millisecond)
		} else if i == ikinciDamga {
			p.yak = y.wallNs - 20*int64(time.Millisecond) - 40*int64(time.Millisecond) - 3*int64(time.Hour)
		}
		if p.yak != 0 && i != 400 {
			sonYak = p.yak
		}
		if i%100 == 0 && i != 400 { // 400'de SR olsa damgadan önce o kesinleştirirdi (test damgayı ölçüyor)
			y.srEkle(k)
		}
		if !kayip[i] {
			d = append(d, p)
		}
	}
	return d
}

// TestSesYakalamaSaatiBayatDamga — 920 modeli: ilk paket bayat damgalı →
// damga şüpheli, varışla geçici bölüm; 2. paketin gerçek damgası bölümü
// kesinleştirir. Bütün paketler (ilk dahil) yerinde.
func TestSesYakalamaSaatiBayatDamga(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	d := bayatDamgaDizisi(k, y, 401, nil)
	pk, yan := sesKos(t, k, d)
	enB, at := enBuyukSapma(t, pk, y.dogru)
	oz := ozDenetim(t, yan)
	bl := bolumler(t, yan)
	t.Logf("bayat damga: sapma %.4f (paket %d) kaynak=%v oz=%v", enB, at, bl[len(bl)-1]["kaynak"], oz)
	if len(bl) != 2 || bl[1]["kaynak"] != "mute+varis+act" || bl[1]["kesin"] != true || oz["act_bayat"].(float64) != 1 {
		t.Fatalf("bayat damga elenip bölüm 2. damgayla kesinleşmeliydi: kaynak=%v oz=%v", bl[len(bl)-1]["kaynak"], oz)
	}
	if enB > 0.002 || oz["durum"] != "tutarli" {
		t.Fatalf("sapma %.4f sn / oz=%v", enB, oz)
	}
}

// TestSesYakalamaSaatiBayatDamgaKayip — aynısı, ama 2. paket (gerçek damga)
// KAYIP; sonraki damga 1 sn sonra (450). Geçici bölüm o damgayla kesinleşir,
// aradaki 49 paket kuyrukta kayar. Eski kod: 1 sn boyunca ve sonrasında
// bütün bölüm 48 sn ERKEN kalırdı (bayat damga varış kuralını susturuyordu).
func TestSesYakalamaSaatiBayatDamgaKayip(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	d := bayatDamgaDizisi(k, y, 450, map[int]bool{401: true})
	dir := t.TempDir()
	w := yeniSesYazici(t, dir, k, false)
	yol := sesBesle(t, w, k, dir, d, birBlok)
	k.ilet(w, 1<<62)
	w.Close()
	pk, yan := oggPaketleriOku(t, yol), yanOku(t, yol)
	// dogru dizisi 401'i de içeriyor; çıktıda o paket yok → hizala
	dogru := append(append([]int64{}, y.dogru[:401]...), y.dogru[402:]...)
	enB, at := enBuyukSapma(t, pk, dogru)
	oz := ozDenetim(t, yan)
	bl := bolumler(t, yan)
	t.Logf("bayat damga + kayıp: sapma %.4f (paket %d) oz=%v bl=%v", enB, at, oz, bl)
	if len(bl) != 2 || bl[1]["kesin"] != true || oz["act_bayat"].(float64) != 1 {
		t.Fatalf("bölüm sonraki damgayla kesinleşmeliydi: oz=%v bl=%v", oz, bl)
	}
	if enB > 0.002 || oz["durum"] != "tutarli" {
		t.Fatalf("sapma %.4f sn / oz=%v", enB, oz)
	}
}

// TestSesYakalamaSaatiKucukAdim — iOS uygulaması modeli (kayıt 927): susturmada
// paket yok, açılışta yayıncı RTP'yi duvar saatine yeniden tabanlıyor ama BİR
// KARE (20 ms) eksik → varış "durmadı" der (fark 20 ms, titreme içinde), RTP'ye
// göre yer 20 ms erken kalır. Damga (seyrek, ~1/sn) bu basamağı görür: bölüm
// "act", sapma 0. Eski 100 ms eşiği görmüyordu (Firefox'ta damgasız, açık).
func TestSesYakalamaSaatiKucukAdim(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	y.act = true
	y.actHer = 50
	d := dizi(k, y, 800, 100, func(i int, y *yayinci) bool {
		if i == 400 {
			y.dur(12000)
			y.rtpAtla(12000 - 20) // duvar 12,000 sn ileri, RTP 11,980 sn: bir kare eksik
			return true
		}
		return false
	})
	d[400].yak = d[400].gelis.UnixNano() - 40*int64(time.Millisecond) - 3*int64(time.Hour) // açılış paketi damgalı
	pk, yan := sesKos(t, k, d)
	enB, at := enBuyukSapma(t, pk, y.dogru)
	oz := ozDenetim(t, yan)
	bl := bolumler(t, yan)
	t.Logf("küçük adım: sapma %.4f (paket %d) oz=%v bl=%v", enB, at, oz, bl)
	if len(bl) != 2 || bl[1]["kaynak"] != "act" || bl[1]["kesin"] != true {
		t.Fatalf("20 ms'lik kare hatası damgayla bölüm açmalıydı: %v", bl)
	}
	if e := bl[1]["sr_eksik_sn"].(float64); math.Abs(e-0.020) > 0.002 {
		t.Fatalf("ölçülen eksik %.4f, beklenen 0,020", e)
	}
	if enB > 0.002 || oz["durum"] != "tutarli" {
		t.Fatalf("sapma %.4f / oz=%v", enB, oz)
	}
}

// TestSesSRAdimFirefox — Firefox modeli (kayıt 923): damga YOK; susturmada
// paket yok, açılışta yayıncı RTP'yi duvar saatine yeniden tabanlıyor ama bir
// kare (20 ms) eksik; varış hıçkırığı 20 ms (eşik altı). Tek tanık SR: art
// arda iki SR +20 ms deyince bölüm hıçkırık paketinden bölünür, kuyruk kayar.
func TestSesSRAdimFirefox(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	y.jitter = func(i int) time.Duration { return time.Duration((i*7919)%7-3) * time.Millisecond }
	d := dizi(k, y, 900, 100, func(i int, y *yayinci) bool {
		if i == 400 {
			y.dur(12000)
			y.rtpAtla(12000 - 20)
			return true
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	enB, at := enBuyukSapma(t, pk, y.dogru)
	oz := ozDenetim(t, yan)
	bl := bolumler(t, yan)
	t.Logf("SR basamağı: sapma %.4f (paket %d) oz=%v bl=%v", enB, at, oz, bl)
	if oz["sr_adim"].(float64) != 1 || len(bl) != 2 || bl[1]["kaynak"] != "sr-adim" || bl[1]["kesin"] != true {
		t.Fatalf("rapor basamağı bir kez bölmeliydi: oz=%v bl=%v", oz, bl)
	}
	if e := bl[1]["sr_eksik_sn"].(float64); math.Abs(e-0.020) > 0.004 {
		t.Fatalf("ölçülen basamak %.4f, beklenen 0,020", e)
	}
	if enB > 0.005 || oz["durum"] != "tutarli" {
		t.Fatalf("basamak hıçkırıktan bölünmeliydi: sapma %.4f (paket %d) oz=%v", enB, at, oz)
	}
}
