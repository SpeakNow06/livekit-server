package rawrec

import "time"

// ── HEDEF ÖNCESİ KAYAN TAMPON (2026-09-13, kayıt 883 — rawrec36) ────────────
//
// Yazıcı kurulduğunda kayıt anahtarı henüz yazılmamış olabilir (paylaşım /
// kamera kayıttan önce açıldı) ve anahtar görülene kadar gelen paketler
// bekletiliyor. ESKİ düzen: `waitFor` (60 sn) boyunca HER ŞEY biriktiriliyor,
// süre dolunca tampon TÜMDEN atılıyor ve dosya "eksik" damgalanıyordu.
//
// Ölçüldü (kayıt 883): track'ler 18:20:09-17'de yayınlandı, kayıt düğmesine
// 18:22:14.95'te basıldı. Dört yazıcı da 60. saniyede tamponu atıp "eksik"
// dedi; hedef 18:22:15-16'da bulundu ve dosyalar KAYDIN BAŞINDAN İTİBAREN
// TAMDI (atılanların hepsi kayıt öncesiydi). Ama postprocess "eksik"e bakıp
// dört akışı da tarayıcı kopyasına düşürdü; o kopyaların başında 10 sn'lik
// çözülemeyen kare vardı → ses/görüntü 10 sn kaydı. Bekleme süresi tek
// başına arıza DEĞİLDİ.
//
// Kullanıcı kararı: "60 saniyeyi 10'a düşür; kayıt düğmesine bastığım andan
// itibaren yazsın". YENİ düzen:
//
//   - Tampon KAYAN PENCERE: yalnız son `waitFor` (10 sn) tutulur, eskisi paket
//     geldikçe atılır. Bellek sabit, arama süresi sınırsız (kayıt 177 kuralı:
//     kalıcı vazgeçme yok).
//   - Hedef bulununca kesimden (başlangıç − ön pay, rawrec35) eski olanlar
//     atılır; dosya kayıt anıyla başlar. Görüntüde hedef bulunur bulunmaz PLI
//     ile tam kare isteniyor, sonra 4 sn'de bir sürüyor (video.go).
//   - "Eksik" damgası YALNIZ kaydın içinden paket atıldıysa: pencereden taşan
//     EN YENİ paket kayıt başlangıcından SONRA geldiyse dosya kaydın başını
//     kaçırmıştır (`saglik.kayanTamponSonucu`). Bu ancak anahtar, kayıt
//     başladıktan ~8 sn sonra görülürse olur — gerçek bir arıza.
//   - `waitFor` dolunca yalnız yoklama seyreltiliyor (`geçAramaAralığı`) ve
//     Redis'e "hedef-yok" notu düşüyor; hedef sonradan bulunursa not
//     SİLİNİYOR (`geriDususSil`) — 883'te `kaynak_uyari` bu bayat notla "SFU
//     kayıt anahtarını bulamadı" demişti.
//
// Yardımcılar ses (`paket`) ve görüntü (`vpaket`) için ortak; geliş anını
// çağıran veriyor.

// tamponIzi — pencereden taşıp atılanların izi.
type tamponIzi struct {
	atilan    int       // atılan paket sayısı
	atilanSon time.Time // atılan EN YENİ paketin geliş anı
}

func (iz *tamponIzi) kaydet(n int, son time.Time) {
	if n <= 0 {
		return
	}
	iz.atilan += n
	if son.After(iz.atilanSon) {
		iz.atilanSon = son
	}
}

// pencereKirp — `son`dan `pencere` kadar eski paketleri baştan atar. Paketler
// geliş sırasında (kanal FIFO), o yüzden yalnız baştan bakmak yetiyor.
// Dilimin başı kayıyor; `append` kapasite bitince yalnız canlı öğeleri
// kopyalıyor, eski dizi çöpe gidiyor (10 sn'lik pencerede ~10 sn'de bir).
func pencereKirp[T any](b []T, an func(T) time.Time, son time.Time,
	pencere time.Duration, iz *tamponIzi) []T {
	n := 0
	for n < len(b) && son.Sub(an(b[n])) > pencere {
		n++
	}
	if n == 0 {
		return b
	}
	iz.kaydet(n, an(b[n-1]))
	return b[n:]
}

// ustSinirKirp — sayı freni: `ustSinir`ı aşan kadar en eskiyi atar (görüntü,
// bkz. `videoBekleyenÜstSınır`).
func ustSinirKirp[T any](b []T, an func(T) time.Time, ustSinir int,
	iz *tamponIzi) []T {
	n := len(b) - ustSinir
	if n <= 0 {
		return b
	}
	iz.kaydet(n, an(b[n-1]))
	return b[n:]
}

func paketAn(p paket) time.Time   { return p.geliş }
func vpaketAn(p vpaket) time.Time { return p.geliş }
