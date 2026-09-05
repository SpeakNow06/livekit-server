package rawrec

import "time"

// ── SIRA TAMPONU (2026-09-04, kayıt 185) ────────────────────────────────────
//
// NEDEN VAR — ölçülen arıza:
//
//	Kayıt 185'te ekran paylaşımının SFU dosyasında 1556 karenin 3'ü ÇÖZÜLEMEZ
//	çıktı. Chrome bozuk kareyi atlamıyor, çözücüyü komple kapatıyor:
//	  error → PIPELINE_ERROR_DECODE, sonra seeking=true / readyState=1 kalıcı.
//	Kullanıcı bunu "ikinci paylaşımın görüntüsü tamamen donuk" diye bildirdi;
//	oysa donma BİRİNCİ paylaşımın 42,8. saniyesinde başlıyordu ve ikinci
//	paylaşımın 915 karesi dosyada sapasağlam duruyordu — sadece çözücü ölmüştü.
//	(ffmpeg bozuk kareyi sessizce atlayıp devam ediyor; postprocess ve elle
//	 yapılan kare çıkarma bu yüzden hiçbir şey göremedi. Yalnız tarayıcı ölüyor.)
//
// KÖK NEDEN:
//
//	1080p bir paylaşım karesi ~40 RTP paketine bölünüyor. Yayıncı→SFU arasında
//	bir paket düşünce LiveKit NACK atıyor ve yayıncı onu TEKRAR gönderiyor —
//	ama bir gidiş-dönüş sonra, kare çoktan kapanmış oluyor. Kare toplayıcı
//	parçaları GELİŞ SIRASINA göre ekliyordu, sıra numarasına bakmıyordu:
//	ortasında delik olan, başlığı geçerli görünen bir kare yazılıyordu.
//
//	Tarayıcı kopyasında bu olmuyor çünkü WebRTC'nin jitter tamponu geç gelen
//	paketi bekleyip yerine koyuyor. Bizim kanca (`forwardRTP`) jitter
//	tamponundan ÖNCE, ham iletim yolunda duruyor.
//
//	Sağlık raporu da göremiyordu: paketlerin HEPSİ geldiği için
//	`paket_kayip: 0` yazıyordu. Ölçtüğü şey kayıptı, kare bütünlüğü değil.
//
// ⚠ BU BİR JITTER BUFFER DEĞİL. Oynatma saati, gecikme kestirimi, kayıp
// onarımı yok — WebRTC'nin işini yeniden yapmıyoruz. Bu yalnız bir DOSYA
// YAZICISININ "sırayla oku" görevi: paketleri kısa bir pencerede sıra
// numarasına göre dizip öyle veriyor. Gecikme sadece kayıt yazıcısında,
// canlı yayına etkisi SIFIR (rawrec pasif bir dinleyici).

// videoSiraBekleme — bir paket, kendisinden önceki eksikler gelsin diye en
// fazla bu kadar bekletiliyor. Tekrar gönderim (RTX) bir gidiş-dönüşte
// geliyor; 300 ms yerel ağda da internette de rahat pay bırakıyor.
const videoSiraBekleme = 300 * time.Millisecond

// videoSiraKapasite — tamponda tutulacak en fazla paket. Dolarsa en küçük
// sıra numaralısı beklemeden salınır (bellek freni). 1080p'de ~40 paket/kare
// olduğuna göre 512 ≈ 12 kare ≈ yarım saniye.
const videoSiraKapasite = 512

// siraTampon — paketleri sıra numarası düzeninde salan kısa pencere.
//
// Sıfır değeri kullanıma hazır.
type siraTampon struct {
	ögeler []vpaket // sıra numarasına göre ARTAN

	sonSeq uint16 // en son SALINAN paketin sıra numarası
	sonVar bool

	gecPaket uint64 // pencere kapandıktan sonra gelen (atılan) paket
	bosluk   uint64 // hiç gelmeyen paket yüzünden oluşan kopukluk sayısı
}

// ekle — paketi sıra numarasına göre yerine sokar.
//
// Ekleme sondan geriye tarıyor: paketler zaten büyük ölçüde sıralı geliyor,
// yani neredeyse her zaman ilk karşılaştırmada yer bulunuyor.
func (t *siraTampon) ekle(p vpaket) {
	i := len(t.ögeler)
	for i > 0 && int16(p.seq-t.ögeler[i-1].seq) < 0 {
		i--
	}
	t.ögeler = append(t.ögeler, vpaket{})
	copy(t.ögeler[i+1:], t.ögeler[i:])
	t.ögeler[i] = p
}

// al — salınmaya hazır bir sonraki paketi verir.
//
// `kopuk` = bu paketten HEMEN ÖNCE bir paket eksik. Kare toplayıcı bunu
// görünce içinde bulunduğu kareyi bozuk sayıp ATIYOR — yarım kare yazmaktansa
// kareyi hiç yazmamak doğru (bkz. dosya başlığı).
//
// `hepsi` true iken bekleme süresi gözetilmiyor: akış sonu ve katman
// değişiminde elde kalanı boşaltmak için.
func (t *siraTampon) al(an time.Time, hepsi bool) (p vpaket, kopuk bool, ok bool) {
	for len(t.ögeler) > 0 {
		baş := t.ögeler[0]
		if !hepsi && len(t.ögeler) < videoSiraKapasite &&
			an.Sub(baş.geliş) < videoSiraBekleme {
			return vpaket{}, false, false // daha bekleyebilir
		}
		t.ögeler = t.ögeler[1:]

		if !t.sonVar {
			t.sonSeq, t.sonVar = baş.seq, true
			return baş, false, true
		}
		d := int16(baş.seq - t.sonSeq)
		if d <= 0 {
			// Pencere kapandıktan SONRA geldi (ya da kopya). Yerine koymak
			// artık mümkün değil; eklemek kareyi bozar. Say ve at.
			t.gecPaket++
			continue
		}
		t.sonSeq = baş.seq
		if d > 1 {
			t.bosluk++
			return baş, true, true
		}
		return baş, false, true
	}
	return vpaket{}, false, false
}

// sifirla — katman değişiminde çağrılıyor: elde kalan paketler ESKİ katmana
// ait, yeni dosyaya girmemeleri gerekiyor.
func (t *siraTampon) sifirla() {
	t.ögeler = t.ögeler[:0]
	t.sonVar = false
	// Sayaçlar DOSYA BAŞINA raporlanıyor; katman değişince yeni dosya
	// açıldığı için burada da sıfırlanıyorlar.
	t.gecPaket, t.bosluk = 0, 0
}
