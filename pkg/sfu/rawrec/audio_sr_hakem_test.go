package rawrec

// ── SR HAKEM KANITI (2026-10-13) — kayıt 981 ve 982, 2026-10-07 ─────────────
//
// İki gerçek derste bir Android (uygulama, okhttp / JS 2.20.0) öğrencisinin
// mikrofon SFU dosyası yazıcının öz denetiminde elendi ("SR duvar ≠ yazılan")
// ve tarayıcı yedeğine düşüldü. Buradaki olay tabloları prod yan JSON'larından
// çıkarıldı (audioraw_981/01_sfu_6bd7f620…, audioraw_982/01_sfu_6043e9c4…):
// sr_gunlugu (varış/NTP ↔ pts), act_gunlugu, act_red_gunlugu, bolumler.
//
// YAYINCI MODELİ — sunucu tarafı kayıtların tamamıyla tutarlı tek model:
//  * RTP: 20 ms/paket, kesintisiz sayaç.
//  * SR: libwebrtc GÖNDERME saati ↔ "RTP şimdi", her 5 sn. HAL damgasından
//    bağımsız. Gönderme borusu dururken RTP durur, duvar ilerler.
//  * ACT (abs-capture-time): Android HAL getTimestamp ile, okunan tampona göre
//    DÜZELTİLMEMİŞ (rawrec56 bulgusu): okuyucu HAL'in gerisinde birikmişse damga
//    birikim kadar İLERİDE. Periyodik (~0,5 sn), hata tetiklemeli değil (981'de
//    damgalı paketin kendi varış açığı 0,003 sn: damga duruştan sonraki
//    periyodik pakette geldi).
//  * Olay türleri:
//    - "durma": yakalama durması; SR, varış ve ACT aynı anda +X (981 bölüm 2-3).
//      Bölüm 4: varış 2,14 sn, eşleme 1,84 sn → +0,3 sn kalıcı ağ gecikmesi.
//    - "mute": mute sinyali + durma (981 bölüm 1, "mute+varis+act").
//    - "hal": HAL damgası t0'da +X sıçrar (birikim), gönderme duruşu t0+3 sn'de
//      (birikim tükenince). 981'de reddedilen üç olayın imzası tam bu:
//      damgada kayma = gelecek ≈ 1,83 sn (varış penceresi henüz durmadı),
//      SR adımı 2,7-5,7 sn sonra, art arda 5 red → yeniden tabanlama → boşluk
//      hiç yazılmadı → dosya 3×1,83 = 5,5 sn kısa.
//    - "kesinti": ağ kesintisi; paketler yayıncıda birikir, sonunda patlama
//      halinde (0,1 ms aralıkla) gelir; RTP ve SR kesintisiz. Patlamadaki ilk
//      damga okuma anıyla (bayat/ileri, +3,16 sn) → 982'de varış bölümünü
//      +3,16 sn "kesinleştirdi"; sonraki damgalar ve SR bunu yalanladı ama
//      kesin bölüme SR dokunamadı → dosya 3,1 sn uzun.
//
// DOĞRULUK ÖLÇÜTÜ: yayıncının BEYAN ETTİĞİ yakalama zaman çizgisi (SR/ACT —
// RFC 3550'nin akışlar arası senkron mekanizması). Öz denetim ve tarayıcı
// kopyası da aynı tanımı kullanıyor; test `dogru` dizisini bundan kuruyor.
//
// KANIT DÜZENİ: (1) mevcut kod bu iki senaryoda prod'daki aynı hatayı
// üretir (zaman-tutarsiz, 981 açık ≈ +5,5 / 982 ≈ −3,1); (2) SR hakem
// (saat.go `srHakem`) ile ikisi de "tutarli" olur, eklenen süre beyanla
// örtüşür; (3) hakem meşru bölümlerde ve 937 tipi gelecek damgada SUSAR;
// (4) 911/923/931/937/940 testleri aynen geçer.

import (
	"math"
	"testing"
	"time"
)

// androidYayinci — yukarıdaki modelin üreticisi.
type androidYayinci struct {
	wallNs, wall0 int64
	rtp           uint32
	seq           uint16
	n             int
	yakOfs        int64 // HAL damga ofseti (düzeltilmemiş birikim)
	damgaHer      int   // periyodik damga: her N paket (25 = 0,5 sn)
	srHer         int   // SR: her N paket (250 = 5 sn)
	gecikmeNs     int64 // sabit ağ gecikmesi (varış = gönderme + gecikme)
	gecikmeGecici int64 // yalnız bir sonraki pakete eklenen titreme (981 bölüm 4: varış 2,14 / eşleme 1,84)
	kesintiBitis  int64 // > 0: kesinti sürüyor, paketler bu ana kadar birikir
	patlamaIdx    int
	bogusDamga    int64 // ≠ 0: bir sonraki damgalı pakete eklenecek hata (tek sefer)
	dogru         []int64
	k             *sahteSesKaynak
}

func yeniAndroidYayinci(k *sahteSesKaynak, rtp0 uint32) *androidYayinci {
	return &androidYayinci{wallNs: t0().UnixNano(), wall0: t0().UnixNano(), rtp: rtp0, seq: 1000,
		damgaHer: 25, srHer: 250, gecikmeNs: 30 * int64(time.Millisecond), k: k}
}

func (y *androidYayinci) paket() spaket {
	gonder := y.wallNs
	varis := gonder + y.gecikmeNs + y.gecikmeGecici
	y.gecikmeGecici = 0
	if y.kesintiBitis > 0 {
		if gonder < y.kesintiBitis {
			varis = y.kesintiBitis + y.gecikmeNs + int64(y.patlamaIdx)*100_000
			y.patlamaIdx++
		} else {
			y.kesintiBitis, y.patlamaIdx = 0, 0
		}
	}
	p := spaket{rtp: y.rtp, seq: y.seq, gelis: time.Unix(0, varis), yuk: opusYuk(y.n)}
	if y.damgaHer > 0 && y.n%y.damgaHer == 0 {
		p.yak = y.wallNs - 40*int64(time.Millisecond) + yayinciSaatOfseti + y.yakOfs
		if y.bogusDamga != 0 {
			p.yak += y.bogusDamga
			y.bogusDamga = 0
		}
	}
	// Doğru yer GÖNDERME saatine göre (SR'ın beyanı): HAL damgasının birikim
	// kadar ileride olması (yakOfs) örneğin yerini değiştirmez — örnek,
	// birikim tükenip akış durduğunda boşluğun arkasına düşer.
	y.dogru = append(y.dogru, (y.wallNs-y.wall0)*48000/int64(time.Second))
	y.n++
	y.seq++
	y.rtp += 960
	y.wallNs += 20 * int64(time.Millisecond)
	if y.srHer > 0 && y.n%y.srHer == 0 {
		y.sr()
	}
	return p
}

// sr — gönderme saatiyle: NTP = duvar − 3 sa, RTP = şimdi; varış = duvar +
// gecikme (kesintide patlamayla birlikte).
func (y *androidYayinci) sr() {
	gec := y.gecikmeNs
	if y.kesintiBitis > 0 && y.wallNs < y.kesintiBitis {
		gec += y.kesintiBitis - y.wallNs
	}
	y.k.srEkleTitrek(y.rtp, y.wallNs, time.Duration(gec))
}

func (y *androidYayinci) dur(sn float64) { y.wallNs += int64(sn * float64(time.Second)) }

type sesOlay struct {
	t   float64 // RTP zamanı (sn)
	tur string
	sn  float64
}

// androidDizi — olay tablosunu pakete çevirir. "hal" olayında gönderme duruşu
// t+halGecikme'de gelir.
func androidDizi(y *androidYayinci, sureSn float64, olaylar []sesOlay, halGecikme float64) []spaket {
	n := int(sureSn * 50)
	var dizi []spaket
	uygulandi := make([]bool, len(olaylar))
	bekleyenT, bekleyenSn := -1.0, 0.0
	for i := 0; i < n; i++ {
		t := float64(i) * 0.02
		mute := false
		for j, o := range olaylar {
			if uygulandi[j] || o.t > t {
				continue
			}
			uygulandi[j] = true
			switch o.tur {
			case "mute":
				y.dur(o.sn)
				mute = true
			case "durma":
				y.dur(o.sn)
			case "durma+gecikme":
				y.dur(o.sn)
				y.gecikmeGecici = 300 * int64(time.Millisecond)
			case "hal":
				y.yakOfs += int64(o.sn * float64(time.Second))
				bekleyenT, bekleyenSn = t+halGecikme, o.sn
			case "kesinti":
				y.kesintiBitis = y.wallNs + int64(o.sn*float64(time.Second))
				y.bogusDamga = int64((o.sn - 0.37) * float64(time.Second)) // 982: 3,53 − 0,37 = 3,16
			case "gelecek-damga":
				y.bogusDamga = int64(o.sn * float64(time.Second))
			}
		}
		if bekleyenT >= 0 && t >= bekleyenT {
			y.dur(bekleyenSn)
			y.yakOfs -= int64(bekleyenSn * float64(time.Second))
			bekleyenT = -1
		}
		p := y.paket()
		p.mute = mute
		dizi = append(dizi, p)
	}
	return dizi
}

// sesKosGecikme — sesKos ama üretim yazma gecikmesiyle (25 sn): hakem
// kararı 2 SR (~10 sn) + pencere (≤ 5 sn) ister, 10 sn'lik test varsayılanı
// bölme noktasını yazılmış bırakabilir.
func sesKosGecikme(t *testing.T, k *sahteSesKaynak, dizi []spaket, gecikme time.Duration) ([]oggPaket, map[string]any) {
	t.Helper()
	dir := t.TempDir()
	w := yeniSesYazici(t, dir, k, false)
	yazmaGecikme = gecikme
	yol := sesBesle(t, w, k, dir, dizi, birBlok)
	k.ilet(w, 1<<62)
	w.Close()
	if d := w.düşen.Load(); d != 0 {
		t.Fatalf("test kanalı taşırdı: %d paket düştü", d)
	}
	return oggPaketleriOku(t, yol), yanOku(t, yol)
}

func sonSapmaSn(pk []oggPaket, dogru []int64) float64 {
	i := len(pk) - 1
	return math.Abs(konumSn(pk[i]) - float64(dogru[i])/48000.0)
}

// sapanPaket — beyan edilen yerinden 0,1 sn'den fazla sapan paket sayısı.
// Periyodik damgalı yayıncıda boşluk, duruştan sonraki İLK damgalı pakete
// (≤ 25 paket = 0,5 sn) yazılır; o aradaki paketler eski çizgide kalır. Bu
// prod yazıcısının da davranışı; ölçüt boşluğun BÜYÜKLÜĞÜ değil SÜRESİ.
func sapanPaket(pk []oggPaket, dogru []int64) int {
	n := 0
	for i, p := range pk {
		if math.Abs(konumSn(p)-float64(dogru[i])/48000.0) > 0.1 {
			n++
		}
	}
	return n
}

// hakemDogrula — öz denetim tutarlı, açık ≤ 0,1 sn, eklenen beyanla örtüşür,
// son paket yerinde, en büyük sapma sınırda.
func hakemDogrula(t *testing.T, ad string, o geriOzet, pk []oggPaket, dogru []int64, eklenen, tolEklenen float64, sapanUst, hakem int) {
	t.Helper()
	son := sonSapmaSn(pk, dogru)
	sapan := sapanPaket(pk, dogru)
	t.Logf("[%s] son paket sapması %.3f sn, >0,1 sn sapan paket %d, sr_hakem=%v bos=%v act_gelecek=%v yeniden_taban=%v",
		ad, son, sapan, o.oz["sr_hakem"], o.oz["sr_hakem_bos"], o.oz["act_gelecek"], o.oz["act_yeniden_taban"])
	if o.oz["durum"] != "tutarli" {
		t.Fatalf("%s: öz denetim %v / %v (açık %v sn)", ad, o.oz["durum"], o.oz["neden"], o.oz["acik_sn"])
	}
	if a, _ := o.oz["acik_sn"].(float64); math.Abs(a) > 0.1 {
		t.Fatalf("%s: SR açığı %.3f sn > 0,1", ad, a)
	}
	// Gözlem alanı (rawrec57): yayıncı NTP'sine göre açık da yazılmalı ve
	// varış tabanlı ölçüyle uyuşmalı (bu modelde SR gecikmesi sabit 30 ms).
	n, var_ := o.oz["acik_ntp_sn"].(float64)
	a, _ := o.oz["acik_sn"].(float64)
	if !var_ || math.Abs(n-a) > 0.1 {
		t.Fatalf("%s: acik_ntp_sn %v (var=%v), acik_sn %.3f ile uyuşmuyor", ad, n, var_, a)
	}
	if math.Abs(o.eklenen-eklenen) > tolEklenen {
		t.Fatalf("%s: eklenen %.3f sn, %.3f ± %.2f bekleniyordu: %v", ad, o.eklenen, eklenen, tolEklenen, o.bl)
	}
	if son > 0.05 {
		t.Fatalf("%s: son paket %.3f sn yanlış yerde", ad, son)
	}
	if sapan > sapanUst {
		t.Fatalf("%s: %d paket yerinden > 0,1 sn sapıyor (sınır %d; en büyük %.3f sn, paket %d)", ad, sapan, sapanUst, o.enB, o.at)
	}
	if h := ozSayac(t, o, "sr_hakem"); int(h) != hakem {
		t.Fatalf("%s: sr_hakem %v, %d bekleniyordu", ad, h, hakem)
	}
}

// ── 981 (Yasemin, Android uygulama): 7 × ~1,83 sn — 4'ü yazıldı, 3'ü reddedildi ──
//
// Gerçek tablo (yan JSON): bölüm 1 294,3 sn mute+varis+act 1,811; bölüm 2
// 378,4 act 1,832; bölüm 3 490,3 act 1,846; bölüm 4 708,1 varis+sr 1,8415
// (varış 2,14); reddedilen damga grupları 825,7 / 1157,3 / 1215,1 sn, SR
// basamakları +1,820 / +1,843 / +1,838. Toplam beyan 12,83 sn; prod dosyası
// 7,33 yazdı, öz denetim açık +5,52 sn → "zaman-tutarsiz".
func olaylar981() []sesOlay {
	return []sesOlay{
		{294.3, "mute", 1.811},
		{377.9, "durma", 1.832},
		{489.8, "durma", 1.846},
		{708.1, "durma+gecikme", 1.8415},
		{825.7, "hal", 1.820},
		{1157.3, "hal", 1.843},
		{1215.1, "hal", 1.838},
	}
}

func TestSrHakem981(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniAndroidYayinci(k, 979_996_998)
	d := androidDizi(y, 1288.6, olaylar981(), 3.0)
	pk, yan := sesKosGecikme(t, k, d, 25*time.Second)
	o := geriDamgaOlc(t, "981", pk, yan, y.dogru)
	// Damga yolu üç HAL olayını yine reddeder (15 red, 3 yeniden taban) —
	// hakem SR ile üçünü de yazar. B tipi olaylarda damga duruştan ≤ 0,5 sn
	// sonra geldiği için o kadarlık yerel sapma kalır (prod yazıcısı da öyle).
	// 2 B tipi olayda ≤ 25 paket eski çizgide (0,5 sn damga aralığı); HAL
	// olaylarında hakem boşluğu hıçkırığın tam yerine yazar (0 sapan paket).
	hakemDogrula(t, "981", o, pk, y.dogru, 12.83, 0.25, 2*25+10, 3)
	if r := ozSayac(t, o, "act_gelecek"); r != 15 {
		t.Fatalf("981: damga yolu %v red üretti, 15 bekleniyordu (model prod'u yansıtmıyor)", r)
	}
}

// ── 982 (Dilek, Android uygulama, ilk bağlantı): 3,53 sn ağ kesintisi ─────────
//
// Gerçek tablo: bölüm 5 474,3 sn "varis+act" varış 3,531 / sr_eksik 3,160
// (bayat damga kesinleştirdi); ilk_paketler 15 paketin 1,4 ms içinde geldiğini
// gösteriyor (patlama); sonraki SR'lar −3,24 sn, damgalar −3,22 sn. Beyan
// edilen eklenmesi gereken süre 0; prod dosyası 3,16 yazdı → "zaman-tutarsiz".
func TestSrHakem982(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniAndroidYayinci(k, 4_127_310_894)
	d := androidDizi(y, 682.5, []sesOlay{{474.3, "kesinti", 3.53}}, 0)
	pk, yan := sesKosGecikme(t, k, d, 25*time.Second)
	o := geriDamgaOlc(t, "982", pk, yan, y.dogru)
	hakemDogrula(t, "982", o, pk, y.dogru, 0, 0.06, 5, 1)
}

// ── Hakem SUSMALI: meşru bölümler + 937 tipi gelecek damga ──────────────────
//
// Aynı Android modeli, yalnız B tipi durmalar (SR, varış, ACT aynı anda) ve
// bir kez sayfa yaşı artefaktı (+600 sn tek damga, olay 937). Yazıcı bunları
// zaten doğru yazıyor; hakem devreye GİRMEMELİ (sr_hakem = 0), çift sayım yok.
func TestSrHakemSessiz(t *testing.T) {
	k := &sahteSesKaynak{}
	y := yeniAndroidYayinci(k, 500_000)
	d := androidDizi(y, 300, []sesOlay{
		{60.3, "mute", 1.5},
		{120.7, "durma", 1.83},
		{180.1, "gelecek-damga", 600},
		{240.5, "durma", 0.9},
	}, 0)
	pk, yan := sesKosGecikme(t, k, d, 25*time.Second)
	o := geriDamgaOlc(t, "sessiz", pk, yan, y.dogru)
	hakemDogrula(t, "sessiz", o, pk, y.dogru, 4.23, 0.1, 2*25+10, 0)
}
