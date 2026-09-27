package rawrec

// rawrec56 — TEK PAKETLİK YAKALAMA DAMGASI TİTREMESİ (kayıt 940, Android).
// Bkz. saat.go "rawrec56" bloğu: Android libwebrtc bazen bir paketi bir HAL
// periyodu bayat damgalar (çukur, −18…−20 ms) ya da okuma gecikince ilk pakete
// yeni damgayı verir (tepe, +20 ms); sonraki paket hep eski çizgiye döner.
// Eski yazıcı çukurdaki paketi referans yapıp dönüşü boşluk sayıyordu.
//
// Gönderici modeli libwebrtc AbsoluteCaptureTimeSender kuralını birebir
// uygular: damga, son gönderilenden enterpolasyon hatası > 1 ms (kesme →
// fiilen ≥ 2 ms) ya da 1 sn geçtiyse pakete yazılır. Sapan paket ve dönüş
// paketi bu kuralla HER ZAMAN damgalı gider — yazıcının dayandığı garanti.

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
)

type actGonderici struct {
	var_    bool
	sonWall int64
	sonRtp  int64
	sonYak  int64
}

func (g *actGonderici) gonder(wallNs int64, rtp uint32, yak int64) bool {
	ok := false
	if !g.var_ || wallNs-g.sonWall > int64(time.Second) {
		ok = true
	} else {
		interp := g.sonYak + (int64(rtp)-g.sonRtp)*int64(time.Second)/48000
		err := interp - yak
		if err < 0 {
			err = -err
		}
		ok = err/int64(time.Millisecond) > 1
	}
	if ok {
		g.var_, g.sonWall, g.sonRtp, g.sonYak = true, wallNs, int64(rtp), yak
	}
	return ok
}

// geriDamgaDizisi — n paket; sapma(i) paketin GERÇEK yakalama damgasına
// eklenen hata (çukur −18, tepe +20); olay(i, y) duraklama vb. (mute sinyali
// döner). Damgalar gönderici modelinden geçer (seyrek). SR her srHer pakette.
func geriDamgaDizisi(k *sahteSesKaynak, y *yayinci, n, srHer int, sapma func(i int) time.Duration, olay func(i int, y *yayinci) bool) []spaket {
	g := &actGonderici{}
	var out []spaket
	for i := 0; i < n; i++ {
		mute := false
		if olay != nil {
			mute = olay(i, y)
		}
		p := y.paket()
		p.mute = mute
		yak := p.gelis.UnixNano() - 40*int64(time.Millisecond) - 3*int64(time.Hour)
		if sapma != nil {
			yak += int64(sapma(i))
		}
		if g.gonder(p.gelis.UnixNano(), p.rtp, yak) {
			p.yak = yak
		}
		out = append(out, p)
		if srHer > 0 && i%srHer == 0 {
			y.srEkle(k)
		}
	}
	return out
}

func sapmaHaritasi(m map[int]time.Duration) func(i int) time.Duration {
	return func(i int) time.Duration { return m[i] }
}

// geriOzet — bir koşunun ölçüleri.
type geriOzet struct {
	bolum   int
	eklenen float64 // bölümlerin sr_eksik toplamı (yazıcının eklediği süre)
	enB     float64 // çıktı konumu − gerçek konum, en büyük (sn)
	at      int
	oz      map[string]any
	bl      []map[string]any
}

func geriDamgaOlc(t *testing.T, ad string, pk []oggPaket, yan map[string]any, dogru []int64) geriOzet {
	t.Helper()
	o := geriOzet{oz: ozDenetim(t, yan), bl: bolumler(t, yan)}
	o.enB, o.at = enBuyukSapma(t, pk, dogru)
	o.bolum = len(o.bl)
	var sb strings.Builder
	for _, b := range o.bl {
		if e, ok := b["sr_eksik_sn"].(float64); ok {
			o.eklenen += e
			sb.WriteString(fmt.Sprintf(" s%v:%+.3f", b["sira"], e))
		}
	}
	t.Logf("[%s] bölüm=%d eklenen=%.3f sn en_büyük_sapma=%.4f (paket %d) oz=%v/%v acik=%v geri_atlanan=%v geri_taban=%v gecici_duzelt=%v",
		ad, o.bolum, o.eklenen, o.enB, o.at, o.oz["durum"], o.oz["neden"], o.oz["acik_sn"],
		o.oz["act_geri_atlanan"], o.oz["act_geri_taban"], o.oz["act_gecici_duzelt"])
	t.Logf("[%s] bölümler:%s", ad, sb.String())
	return o
}

// geriDamgaDogrula — bölüm sayısı, eklenen süre (±tolEklenen), en büyük
// sapma (≤ tolSapma), öz denetim "tutarlı", bütün bölümler kesin.
func geriDamgaDogrula(t *testing.T, ad string, o geriOzet, bolum int, eklenen, tolEklenen, tolSapma float64) {
	t.Helper()
	if o.bolum != bolum {
		t.Fatalf("%s: %d bölüm bekleniyordu, %d var: %v", ad, bolum, o.bolum, o.bl)
	}
	if math.Abs(o.eklenen-eklenen) > tolEklenen {
		t.Fatalf("%s: eklenen %.3f sn, %.3f bekleniyordu: %v", ad, o.eklenen, eklenen, o.bl)
	}
	if o.enB > tolSapma {
		t.Fatalf("%s: en büyük sapma %.4f sn (paket %d), sınır %.3f", ad, o.enB, o.at, tolSapma)
	}
	if o.oz["durum"] != "tutarli" {
		t.Fatalf("%s: öz denetim %v", ad, o.oz)
	}
	for _, b := range o.bl[1:] {
		if b["kesin"] != true {
			t.Fatalf("%s: bölüm kesinleşmedi: %v", ad, b)
		}
	}
}

func ozSayac(t *testing.T, o geriOzet, ad string) float64 {
	t.Helper()
	v, _ := o.oz[ad].(float64)
	return v
}

// ── 1. yerlestir izi: t, t+2, t+40 üçlüsü, tepe, alternans — paket paket ─────
func TestGeriDamgaYerlestirIzi(t *testing.T) {
	s := yeniSesSaat(48000)
	t0 := t0()
	g := &actGonderici{}
	sapma := map[int]time.Duration{5: -18 * time.Millisecond, 12: 20 * time.Millisecond, 20: -18 * time.Millisecond, 22: -18 * time.Millisecond}
	var tepe *sesBolum
	for i := 0; i < 30; i++ {
		rtp := uint32(1000 + 960*i)
		gelis := t0.Add(time.Duration(i) * 20 * time.Millisecond).Add(40 * time.Millisecond)
		yak := gelis.UnixNano() - 40*int64(time.Millisecond) + int64(sapma[i])
		var yakNs int64
		if g.gonder(gelis.UnixNano(), rtp, yak) {
			yakNs = yak
		}
		refOnce := s.sonYakPts
		pts, yeni, kb, kayma := s.yerlestir(rtp, gelis, uint16(i), false, yakNs)
		dogru := int64(i) * 960
		t.Logf("i=%2d damga=%-5v hata=%+4dms pts−doğru=%+3dms yeni=%v düzeltme=%v(%d)", i, yakNs != 0, sapma[i].Milliseconds(), (pts-dogru)*1000/48000, yeni != nil, kb != nil, kayma)
		switch i {
		case 5, 20, 22: // çukur: paket RTP çizgisinde kalır, bölüm açılmaz, REFERANS KORUNUR
			if pts != dogru || yeni != nil || s.sonYakPts != refOnce {
				t.Fatalf("i=%d çukur: pts %d (doğru %d) yeni=%v ref %d→%d", i, pts, dogru, yeni != nil, refOnce, s.sonYakPts)
			}
		case 6, 21, 23: // dönüş: eski çizgi, boşluk yok
			if pts != dogru || yeni != nil {
				t.Fatalf("i=%d dönüş: pts %d (doğru %d) yeni=%v", i, pts, dogru, yeni != nil)
			}
		case 12: // tepe: küçük adım → GEÇİCİ bölüm (+20 ms)
			if yeni == nil || !yeni.actGecici || math.Abs(*yeni.SrEksikSn-0.020) > 0.001 {
				t.Fatalf("i=12 tepe: geçici bölüm bekleniyordu: %+v", yeni)
			}
			tepe = yeni
		case 13: // dönüş: geçici bölüm eski çizgiye çekilir (kuyruk kayar), boşluk ~0
			// (i=14 damgasız gelince bölüm kesinleşir; i=20'deki çukur düzeltme sanılmaz)
			if kb != tepe || kayma >= 0 || pts != dogru || math.Abs(*tepe.SrEksikSn) > 0.003 {
				t.Fatalf("i=13: düzeltme bekleniyordu: kb=%v kayma=%d pts=%d doğru=%d sr_eksik=%v", kb == tepe, kayma, pts, dogru, *tepe.SrEksikSn)
			}
		}
	}
	if len(s.bolumler) != 2 {
		t.Fatalf("2 bölüm (ilk + geri alınmış tepe) bekleniyordu: %d", len(s.bolumler))
	}
	if s.actGeriAtlan != 3 || s.actGeciciDuz != 1 || s.actGeriTaban != 0 {
		t.Fatalf("sayaçlar: atlanan %d (3) düzeltme %d (1) taban %d (0)", s.actGeriAtlan, s.actGeciciDuz, s.actGeriTaban)
	}
}

// ── 2. sentetik 940: 60 sn, 40 çukur + 6 tepe + alternans + 2 gerçek susturma ─
// Eski kod: 51 bölüm, +0,876 sn, "zaman-tutarsiz". Yeni: yalnız 2 gerçek
// susturma (3 + 1 sn); tepe bölümleri listede sıfır boşlukla kalır.
func sentetik940(k *sahteSesKaynak, y *yayinci) []spaket {
	sapma := map[int]time.Duration{}
	for j := 0; j < 40; j++ {
		sapma[97+j*61+(j*j)%17] = -18 * time.Millisecond
	}
	for j := 0; j < 6; j++ {
		sapma[311+j*401] = 20 * time.Millisecond
	}
	sapma[1500], sapma[1502], sapma[1504] = -18*time.Millisecond, -18*time.Millisecond, -18*time.Millisecond
	return geriDamgaDizisi(k, y, 3000, 100, sapmaHaritasi(sapma), func(i int, y *yayinci) bool {
		switch i {
		case 1000:
			y.dur(3000)
			return true
		case 2200:
			y.dur(1000)
			return true
		}
		return false
	})
}

func TestGeriDamgaSentetik940(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	d := sentetik940(k, y)
	pk, yan := sesKos(t, k, d)
	o := geriDamgaOlc(t, "sentetik 940", pk, yan, y.dogru)
	geriDamgaDogrula(t, "sentetik 940", o, 9, 4.000, 0.003, 0.002)
	if a := ozSayac(t, o, "act_geri_atlanan"); a < 40 {
		t.Fatalf("çukurlar referansı korumalıydı: act_geri_atlanan %v", a)
	}
	if d := ozSayac(t, o, "act_gecici_duzelt"); d < 6 {
		t.Fatalf("tepeler geri alınmalıydı: act_gecici_duzelt %v", d)
	}
	if tb := ozSayac(t, o, "act_geri_taban"); tb != 0 {
		t.Fatalf("kalıcı geri adım yoktu: act_geri_taban %v", tb)
	}
}

// ── 3. gerçek kalıcı +20 adım (iOS 927, rawrec51) çukur ve tepeyle birlikte ─
// Adım korunmalı (tek bölüm +0,020), çukur/tepe boşluk eklememeli.
func TestGeriDamgaKaliciAdimVeCukur(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	sapma := map[int]time.Duration{200: -18 * time.Millisecond, 600: -18 * time.Millisecond, 650: 20 * time.Millisecond}
	d := geriDamgaDizisi(k, y, 800, 100, sapmaHaritasi(sapma), func(i int, y *yayinci) bool {
		if i == 400 {
			y.dur(12000)
			y.rtpAtla(12000 - 20)
			return true
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	o := geriDamgaOlc(t, "kalıcı adım + çukur", pk, yan, y.dogru)
	geriDamgaDogrula(t, "kalıcı adım + çukur", o, 3, 0.020, 0.002, 0.002)
	if e := o.bl[1]["sr_eksik_sn"].(float64); math.Abs(e-0.020) > 0.001 {
		t.Fatalf("gerçek adım korunmalıydı: %v", o.bl[1])
	}
}

// ── 4. çukur + gelecek damga (olay 937) ──────────────────────────────────────
func TestGeriDamgaCukurVeGelecek(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	sapma := map[int]time.Duration{398: -18 * time.Millisecond}
	d := geriDamgaDizisi(k, y, 800, 100, sapmaHaritasi(sapma), func(i int, y *yayinci) bool {
		if i == 400 {
			y.dur(260)
			return true
		}
		return false
	})
	d[400].yak = d[400].gelis.UnixNano() - 40*int64(time.Millisecond) - 3*int64(time.Hour) + 1240*int64(time.Second)
	d[401].yak = d[401].gelis.UnixNano() - 40*int64(time.Millisecond) - 3*int64(time.Hour)
	pk, yan := sesKos(t, k, d)
	o := geriDamgaOlc(t, "çukur + gelecek", pk, yan, y.dogru)
	geriDamgaDogrula(t, "çukur + gelecek", o, 2, 0.260, 0.003, 0.002)
	if g := ozSayac(t, o, "act_gelecek"); g != 1 {
		t.Fatalf("gelecek damga reddedilmeliydi: act_gelecek %v", g)
	}
}

// ── 5. çukurlar + SR (gerçek çizgide, ±15 ms titrek) ─────────────────────────
func TestGeriDamgaCukurVeSR(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	sapma := map[int]time.Duration{}
	for j := 0; j < 30; j++ {
		sapma[50+j*37] = -18 * time.Millisecond
	}
	g := &actGonderici{}
	var d []spaket
	for i := 0; i < 1500; i++ {
		p := y.paket()
		yak := p.gelis.UnixNano() - 40*int64(time.Millisecond) - 3*int64(time.Hour) + int64(sapma[i])
		if g.gonder(p.gelis.UnixNano(), p.rtp, yak) {
			p.yak = yak
		}
		d = append(d, p)
		if i%100 == 0 {
			k.srEkleTitrek(y.rtp, y.wallNs, time.Duration((i/100%3)-1)*15*time.Millisecond)
		}
	}
	pk, yan := sesKos(t, k, d)
	o := geriDamgaOlc(t, "çukur + SR", pk, yan, y.dogru)
	geriDamgaDogrula(t, "çukur + SR", o, 1, 0, 0.001, 0.002)
}

// ── 6. gerçek +20 adım, doğrulayıcı damga gelmeden 3 sn susturma ─────────────
func TestGeriDamgaAdimSonraMute(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	d := geriDamgaDizisi(k, y, 800, 100, nil, func(i int, y *yayinci) bool {
		switch i {
		case 400:
			y.dur(12000)
			y.rtpAtla(12000 - 20)
			return true
		case 410:
			y.dur(3000)
			return true
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	o := geriDamgaOlc(t, "adım sonra mute", pk, yan, y.dogru)
	geriDamgaDogrula(t, "adım sonra mute", o, 3, 3.020, 0.003, 0.002)
}

// ── 7. susturma sonrası İLK damga çukur: toplam doğru, o tek paket ≤ 20 ms ───
func TestGeriDamgaMuteIlkCukur(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	sapma := map[int]time.Duration{400: -18 * time.Millisecond}
	d := geriDamgaDizisi(k, y, 800, 100, sapmaHaritasi(sapma), func(i int, y *yayinci) bool {
		if i == 400 {
			y.dur(3000)
			return true
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	o := geriDamgaOlc(t, "mute ilk çukur", pk, yan, y.dogru)
	if o.bolum > 3 || math.Abs(o.eklenen-3.000) > 0.003 || o.enB > 0.020 || o.oz["durum"] != "tutarli" {
		t.Fatalf("mute ilk çukur: bölüm %d eklenen %.3f sapma %.4f oz %v", o.bolum, o.eklenen, o.enB, o.oz)
	}
}

// ── 8. tepe (+20) ve hemen ardından 3 sn susturma ────────────────────────────
func TestGeriDamgaTepeSonraMute(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	sapma := map[int]time.Duration{399: 20 * time.Millisecond}
	d := geriDamgaDizisi(k, y, 800, 100, sapmaHaritasi(sapma), func(i int, y *yayinci) bool {
		if i == 400 {
			y.dur(3000)
			return true
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	o := geriDamgaOlc(t, "tepe sonra mute", pk, yan, y.dogru)
	if o.bolum > 3 || math.Abs(o.eklenen-3.000) > 0.003 || o.enB > 0.020 || o.oz["durum"] != "tutarli" {
		t.Fatalf("tepe sonra mute: bölüm %d eklenen %.3f sapma %.4f oz %v", o.bolum, o.eklenen, o.enB, o.oz)
	}
}

// ── 9. KALICI geri adım: RTP duvar ilerlemeden +20 ms sıçrar ─────────────────
// Dosya geri alınamaz (sapma sabit 20 ms); referans 3 uyumlu damgada yeniden
// tabanlanmalı ki sonraki gerçek susturma (2 sn) doğru açılsın.
func TestGeriDamgaKaliciGeri(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	d := geriDamgaDizisi(k, y, 1200, 100, nil, func(i int, y *yayinci) bool {
		if i == 400 {
			y.rtpAtla(20)
		}
		if i == 900 {
			y.dur(2000)
			return true
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	o := geriDamgaOlc(t, "kalıcı geri adım", pk, yan, y.dogru)
	geriDamgaDogrula(t, "kalıcı geri adım", o, 2, 2.000, 0.003, 0.021)
	if tb, a := ozSayac(t, o, "act_geri_taban"), ozSayac(t, o, "act_geri_atlanan"); tb != 1 || a != 2 {
		t.Fatalf("3 uyumlu geri damgada yeniden taban bekleniyordu: taban %v atlanan %v", tb, a)
	}
}

// ── 10. iki paketlik tepe ve iki paketlik çukur ──────────────────────────────
func TestGeriDamgaCiftTepeCukur(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	sapma := map[int]time.Duration{300: 20 * time.Millisecond, 301: 20 * time.Millisecond, 500: -18 * time.Millisecond, 501: -18 * time.Millisecond}
	d := geriDamgaDizisi(k, y, 800, 100, sapmaHaritasi(sapma), nil)
	pk, yan := sesKos(t, k, d)
	o := geriDamgaOlc(t, "çift tepe/çukur", pk, yan, y.dogru)
	geriDamgaDogrula(t, "çift tepe/çukur", o, 2, 0, 0.003, 0.002)
}

// ── 11. Chrome seyrek damga (1/sn) + tepe + çukur + gerçek 5 sn ──────────────
func TestGeriDamgaSeyrekTepe(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	sapma := map[int]time.Duration{300: 20 * time.Millisecond, 600: -18 * time.Millisecond}
	d := geriDamgaDizisi(k, y, 1500, 100, sapmaHaritasi(sapma), func(i int, y *yayinci) bool {
		if i == 1000 {
			y.dur(5000)
			return true
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	o := geriDamgaOlc(t, "seyrek + tepe + 5 sn", pk, yan, y.dogru)
	geriDamgaDogrula(t, "seyrek + tepe + 5 sn", o, 3, 5.000, 0.003, 0.002)
}

// ── 12. MERDİVEN: okuma iş parçacığı 100 ms takılır (+100, sonra −20×5) ───────
// net 0; ayrıca gerçek 80 ms xrun (kalıcı) korunmalı.
func TestGeriDamgaMerdiven(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	sapma := map[int]time.Duration{300: 100 * time.Millisecond, 301: 80 * time.Millisecond, 302: 60 * time.Millisecond, 303: 40 * time.Millisecond, 304: 20 * time.Millisecond}
	d := geriDamgaDizisi(k, y, 1200, 100, sapmaHaritasi(sapma), func(i int, y *yayinci) bool {
		if i == 700 {
			y.dur(80)
		}
		return false
	})
	pk, yan := sesKos(t, k, d)
	o := geriDamgaOlc(t, "merdiven + 80 ms", pk, yan, y.dogru)
	geriDamgaDogrula(t, "merdiven + 80 ms", o, 3, 0.080, 0.003, 0.002)
}

// ── 13. geçici bölüm doğrulanmadan YAZILDIYSA düzeltme atlanır (eski davranış) ─
// Yazma gecikmesi 0: tepe bölümünün paketi hemen dosyaya iner; dönüş damgası
// tabanı kaydıramaz (yazildi). Dosya +20 ms uzar, çökmez, monoton kalır.
func TestGeriDamgaYazildiktanSonraDuzeltmeYok(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	sapma := map[int]time.Duration{300: 20 * time.Millisecond}
	d := geriDamgaDizisi(k, y, 600, 100, sapmaHaritasi(sapma), nil)
	dir := t.TempDir()
	w := yeniSesYazici(t, dir, k, false)
	yazmaGecikme = 0
	defer func() { yazmaGecikme = 10 * time.Second }()
	yol := sesBesle(t, w, k, dir, d, birBlok)
	k.ilet(w, 1<<62)
	w.Close()
	pk, yan := oggPaketleriOku(t, yol), yanOku(t, yol)
	o := geriDamgaOlc(t, "yazıldıktan sonra", pk, yan, y.dogru)
	if o.bolum != 2 || math.Abs(o.eklenen-0.020) > 0.002 || o.enB > 0.021 || ozSayac(t, o, "act_gecici_duzelt") != 0 {
		t.Fatalf("yazılmış geçici bölüm düzeltilmemeliydi: bölüm %d eklenen %.3f sapma %.4f oz %v", o.bolum, o.eklenen, o.enB, o.oz)
	}
	var son uint64
	for _, p := range pk {
		if p.granul < son {
			t.Fatalf("granül geri gitti: %d < %d", p.granul, son)
		}
		son = p.granul
	}
}

// ── 14. geçici bölüm sürerken SR gelirse SR dokunmaz (bölüm kesin) ───────────
func TestGeriDamgaGeciciBolumdeSR(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniYayinci(t0(), 500_000)
	g := &actGonderici{}
	var d []spaket
	for i := 0; i < 800; i++ {
		p := y.paket()
		yak := p.gelis.UnixNano() - 40*int64(time.Millisecond) - 3*int64(time.Hour)
		if i == 300 {
			yak += 20 * int64(time.Millisecond)
		}
		if g.gonder(p.gelis.UnixNano(), p.rtp, yak) {
			p.yak = yak
		}
		d = append(d, p)
		if i%100 == 0 {
			k.srEkle(y.rtp, y.wallNs)
		}
		if i == 300 {
			// SR tepe paketiyle dönüş paketi ARASINDA varır (+10 ms titreme)
			k.srEkleTitrek(y.rtp, y.wallNs, 10*time.Millisecond)
		}
	}
	pk, yan := sesKos(t, k, d)
	o := geriDamgaOlc(t, "geçici bölümde SR", pk, yan, y.dogru)
	geriDamgaDogrula(t, "geçici bölümde SR", o, 2, 0, 0.003, 0.002)
	if s := ozSayac(t, o, "act_gecici_duzelt"); s != 1 {
		t.Fatalf("tepe damgayla geri alınmalıydı (SR değil): act_gecici_duzelt %v", s)
	}
}

// ── 15. GERÇEK KAYIT 940 (kayit940_verisi_test.go) ───────────────────────────
// Damgalar ve SR'lar gerçek; RTP bölüm tablosundan geri türetilmiş; varışlar
// gerçek duraklamalarla (RTP ilerlemez) ve ±4 ms titremeyle üretilir. Eski kod
// bu dosyada 57 bölüm açıp −0,57 sn ile "zaman-tutarsiz" veriyordu (SFU
// kopyası elendi). Yeni: yalnız 11 gerçek duraklama, sahte 18 ms bölümü yok.
func TestGeriDamgaGercek940(t *testing.T) {
	k := &sahteSesKaynak{}
	sonRtp := kayit940Damgalar[len(kayit940Damgalar)-1][0]
	n := int(sonRtp/960) + 50
	damga := map[int64]int64{}
	for _, e := range kayit940Damgalar {
		damga[e[0]] = e[1]
	}
	durak := map[int64]int{}
	muteler := map[int64]bool{}
	for _, x := range kayit940Duraklar {
		durak[x.rtp] = x.ms
		muteler[x.rtp] = x.mute
	}
	for _, sr := range kayit940SR {
		k.srler = append(k.srler, &livekit.RTCPSenderReportState{
			RtpTimestamp: kayit940IlkRTP + uint32(sr[0]), NtpTimestamp: ntpTS(sr[2]), At: sr[1]})
	}
	sort.SliceStable(k.srler, func(i, j int) bool { return k.srler[i].At < k.srler[j].At })
	yak0 := kayit940IlkGelisNs - 40*int64(time.Millisecond) - 3*int64(time.Hour)
	wall := kayit940IlkGelisNs
	var d []spaket
	var dogru []int64
	for i := 0; i < n; i++ {
		rel := int64(i) * 960
		if ms, ok := durak[rel]; ok {
			wall += int64(ms) * int64(time.Millisecond)
		}
		titreme := int64((i*7)%9-4) * int64(time.Millisecond)
		p := spaket{rtp: kayit940IlkRTP + uint32(rel), seq: uint16(1000 + i), gelis: time.Unix(0, wall+titreme), yuk: opusYuk(i), mute: muteler[rel]}
		if y, ok := damga[rel]; ok {
			p.yak = yak0 + y
		}
		d = append(d, p)
		dogru = append(dogru, (wall-kayit940IlkGelisNs)*48000/int64(time.Second))
		wall += 20 * int64(time.Millisecond)
	}
	pk, yan := sesKos(t, k, d)
	o := geriDamgaOlc(t, "gerçek 940", pk, yan, dogru)
	// 11 gerçek duraklama (≥ 0,1 sn); geri alınmış tepe bölümleri (~0) ve
	// duraklama sonrası hizalayıcı yerleşmesinden gelen ≤ 20 ms'lik en çok 2
	// bölüm listede kalabilir. Eski kodun 41 adet 18 ms'lik bölümü OLMAMALI.
	buyuk, kucukToplam, kucukSayi := 0, 0.0, 0
	for _, b := range o.bl[1:] {
		e, _ := b["sr_eksik_sn"].(float64)
		switch {
		case e >= 0.1:
			buyuk++
		case math.Abs(e) > 0.005:
			kucukSayi++
			kucukToplam += math.Abs(e)
		}
	}
	if buyuk != len(kayit940Duraklar) || kucukSayi > 2 || kucukToplam > 0.06 {
		t.Fatalf("gerçek 940: %d büyük bölüm (11), %d küçük (≤2, toplam %.3f ≤ 0,06): %v", buyuk, kucukSayi, kucukToplam, o.bl)
	}
	// yazıcı duraklamayı damgayla ölçer (varıştan en çok ~0,1 sn farklı olabilir)
	if math.Abs(o.eklenen-24.27) > 0.3 || o.enB > 0.35 {
		t.Fatalf("gerçek 940: eklenen %.3f (≈24,27) sapma %.3f", o.eklenen, o.enB)
	}
	if o.oz["durum"] != "tutarli" {
		t.Fatalf("gerçek 940: öz denetim %v", o.oz)
	}
	if a, dz := ozSayac(t, o, "act_geri_atlanan"), ozSayac(t, o, "act_gecici_duzelt"); a < 35 || dz < 4 {
		t.Fatalf("gerçek 940: çukur/tepe sayaçları beklenenden az: atlanan %v düzeltme %v", a, dz)
	}
}
