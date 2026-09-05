package rawrec

import (
	"time"

	"github.com/livekit/protocol/livekit"
)

// ── SAĞLIK RAPORU (2026-09-04) ──────────────────────────────────────────────
//
// NEDEN VAR — kullanıcının sorusu ve haklı itirazı:
//
//	"SFU kaynak değil mi? Tarayıcıya ulaşandan nasıl daha az veri olabilir?
//	 Ve SFU'nun düzgün çalışmadığına nasıl karar vereceğiz? Bu %75'lik
//	 karşılaştırma doğru bir yöntem mi?"
//
// Değildi. Postprocess, SFU dosyasını BOT TARAYICISININ kopyasıyla
// karşılaştırıp "kare sayısının %90'ını (sonra %75) tutuyor mu" diye
// bakıyordu. İki sorunu vardı:
//
//  1. KAYNAĞI TÜREVİYLE ÖLÇÜYORDU. Bot, SFU'nun ilettiğini alıyor; ölçüt
//     olamaz. Görüntüde zaten FARKLI KATMANLAR karşılaştırılıyordu.
//  2. BIÇAK SIRTIYDI. Kayıt 183'te SFU %89,8 çıktı — iki paylaşımın da sesi
//     SFU'da tamdı — ve %90 eşiğini 0,2 puanla kaçırıp bozuk tarayıcı
//     dosyasına düşüldü. Sonuç: 2. paylaşımın sesi kayda hiç girmedi.
//
// Doğrusu: yazıcı KENDİ DURUMUNU bildirsin. Yazıcı zaten biliyor —
// track'in ömrü boyunca bağlı mıydı, tamponu düşürdü mü, kaç paket kayıp
// gördü, yazdığı aralık ömrün ne kadarını kaplıyor. Postprocess yalnız bu
// rapora bakar; karşılaştırma diye bir şey kalmaz.
//
// ⚠ Bu rapor SUNUCU TARAFI GERÇEĞİDİR: hepsi SFU'nun kendi gözlemi, hiçbir
// yerde bot tarayıcısına bakılmıyor.

// saglikEsigi — yazılan aralık, yazıcının bağlı kaldığı sürenin bu oranından
// azsa dosya "eksik" sayılır.
//
// 0,5 neden: iki gerçek arıza ölçüldü ve ikisi de bunun ÇOK altında —
// kayıt 182'nin kamerası 25,8 sn bağlı kalıp yalnız 0,56 sn yazdı (%2).
// Sağlam dosyalar ise %90'ın üstünde (kayıt 183 paylaşım videosu: 208 sn
// bağlı, 188 sn yazılı = %90). Aradaki açıklık büyük, eşik ortada.
const saglikEsigi = 0.5

// kareButunlukEsigi — sıra kopukluğu yüzünden ATILAN karelerin toplam kareye
// oranı bunu aşarsa dosya "eksik" sayılıyor ve postprocess tarayıcı
// kopyasına düşüyor.
//
// 0,02 neden: atılan kare bozuk kare DEĞİL, sadece eksik kare — çözücü
// ölmüyor, bir sonraki anahtar kareye kadar (bütçe gereği ~1-2,5 sn)
// görüntü bozulup toparlıyor. 24 fps'te %2 ≈ dakikada yarım saniye; bunun
// üstü artık gözle görülür ve tarayıcı kopyası daha iyi bir seçenek olur.
// ⚠ 0,02 → 0,05'E ÇIKARILDI (2026-09-05). Yukarıdaki gerekçe reddin
// BEDELİNİ hesaba katmıyordu. Reddedince postprocess tarayıcı kopyasına
// düşüyor ve o kopyanın zaman çizgisi ÖLÇÜLDÜ ki kayıp:
//
//	kayıt 196 paylaşım : SFU 506,5 sn  ·  tarayıcı kopyası 488,9 sn
//	                     → 17,6 saniye eksik (%3,5)
//	kayıt 193 paylaşım : SFU 200,0 sn  ·  tarayıcı kopyası 187,0 sn
//
// İki maliyet aynı kefeye konamaz:
//
//	· dosyayı kabul etmek (%2-5 kare eksik) → bir sonraki anahtar kareye
//	  kadar süren, gözle zor seçilen takılmalar
//	· yedeğe düşmek → SANİYELERCE ses-görüntü kayması, kaydı bozar
//
// Tarayıcı kopyası ayrıca Sender Report göremiyor (tarayıcı API'leri ham
// NTP/RTP çiftini vermiyor), yani yapısal olarak asla SFU kadar doğru
// olamaz. Eşik bu yüzden SFU lehine ayarlandı.
const kareButunlukEsigi = 0.05

// kareButunlukTabanı — bütünlük reddi için oranın YANINDA aranan MUTLAK hız:
// saniyede bu kadardan az kare atıldıysa dosya reddedilmiyor (kare/sn).
//
// ⚠ NEDEN GEREKLİ (2026-09-05, kayıt 192). `kareButunlukEsigi` bir ORAN ve
// gerekçesi 24 fps varsayıyordu ("%2 ≈ dakikada yarım saniye"). Ekran
// paylaşımının kare hızı ise 10 kata kadar oynuyor: hareketli içerikte
// 13 fps, durgun ekranda 1 fps. Aynı oran düşük fps'te kendiliğinden
// sertleşiyor ve dosya azıcık kayıpla reddediliyor.
//
// Kayıt 192 ölçüldü: 130 sn, ortalama 3,7 fps, 485 kare yazıldı, 11 atıldı
// → oran %2,22, eşik %2. Yani 0,22 puanla reddedildi ve postprocess tarayıcı
// kopyasına düştü. Oysa iki dosya yan yana koyulunca SFU kopyası DAHA İYİYDİ:
// 485 kare / 127,0 sn kapsama (tarayıcı: 480 / 123,0), her ikisinde de sıfır
// çözücü hatası, aynı anda çıkarılan kareler gözle ayırt edilemez.
// Dakikaya vurulunca 5 kare/dk — eşiğin kendi gerekçesindeki 29 kare/dk'nın
// altı kat altında.
//
// 0,25 kare/sn nereden: eşiğin kendi gerekçesi 24 fps'te %2, yani 0,48
// kare/sn. Onun yarısını taban aldık — gerçek bir bozulma (0,48) yine
// reddediliyor, kare hızının düşmesinden doğan sahte ret ise kapanıyor.
const kareButunlukTabanı = 0.25

// saglik — tek bir yazıcının kendi durumu.
type saglik struct {
	baslangic  time.Time // yazıcı kuruldu
	ilkKare    time.Time // dosyaya ilk kare yazıldı
	sonKare    time.Time // dosyaya son kare yazıldı
	enBuyukAra time.Duration

	tamponDurdu bool   // hedef geç bulundu, bekleyen paketler atıldı
	ilkSeq      uint16 // ilk RTP sıra numarası
	sonSeq      uint16
	seqVar      bool
	alinanPaket uint64 // gerçekten gelen ASIL paket (yedekler hariç)

	// ⚠ KATMAN BAŞINA SAYILIR, DOSYA BAŞINA TOPLANIR (2026-09-04).
	// Tek dosyada birden çok katman olabiliyor (bkz. video.go
	// `katmanaGeç`) ve katmanların RTP SIRA NUMARASI UZAYLARI AYRI —
	// ikisini tek sayaçta toplamak kayıp ölçümünü saçmalatırdı. Geçişte
	// biten katmanın sayıları buraya devrediliyor, sayaç sıfırlanıyor.
	beklenenBirikmis uint64
	alinanBirikmis   uint64

	// ── KARE BÜTÜNLÜĞÜ (2026-09-04, kayıt 185) ────────────────────────
	// `paket_kayip` bu arızayı GÖREMİYORDU: paketlerin hepsi geliyordu
	// (kaybolanlar NACK ile tekrar gönderiliyordu), yalnız sıraları
	// bozuktu ve kare ortasında delikle yazılıyordu. Chrome böyle bir
	// kareyi görünce çözücüyü kalıcı olarak kapatıyor. Artık böyle kare
	// YAZILMIYOR, atılıyor — ve kaç tane atıldığı buradan raporlanıyor
	// ki postprocess gerekirse tarayıcı kopyasına düşebilsin.
	yazilanKare uint64
	atilanKare  uint64
	siraBosluk  uint64 // sıra tamponunda hiç kapanmayan boşluk
	gecPaket    uint64 // pencere kapandıktan sonra gelip atılan paket
}

func yeniSaglik() *saglik { return &saglik{baslangic: simdi()} }

// simdi — test edilebilirlik için tek yerden saat.
// (Adı `mono` OLAMAZ: paket zaten `utils/mono`yu import ediyor.)
func simdi() time.Time { return time.Now() }

// paketGeldi — ASIL (yedek olmayan) bir paket geldiğinde çağrılır; kayıp
// ölçümü RTP sıra numarası üstünden yapılıyor.
func (s *saglik) paketGeldi(seq uint16) {
	if s == nil {
		return
	}
	if !s.seqVar {
		s.ilkSeq, s.sonSeq, s.seqVar = seq, seq, true
	} else if int16(seq-s.sonSeq) > 0 { // sarma-güvenli "daha yeni mi"
		s.sonSeq = seq
	}
	s.alinanPaket++
}

// katmanDegisti — yazılan katman değişti: biten katmanın paket sayıları
// birikime devrediliyor ve sıra numarası izleme sıfırlanıyor.
func (s *saglik) katmanDegisti() {
	if s == nil || !s.seqVar {
		return
	}
	s.beklenenBirikmis += uint64(s.sonSeq-s.ilkSeq) + 1
	s.alinanBirikmis += s.alinanPaket
	s.seqVar, s.alinanPaket = false, 0
}

// kareYazildi — dosyaya bir kare yazıldığında çağrılır; en büyük boşluk
// buradan çıkıyor (görüntüde "üst katman sustu" süresi, seste DTX).
func (s *saglik) kareYazildi(an time.Time) {
	if s == nil || an.IsZero() {
		return
	}
	if s.ilkKare.IsZero() {
		s.ilkKare = an
	} else if d := an.Sub(s.sonKare); d > s.enBuyukAra {
		s.enBuyukAra = d
	}
	s.sonKare = an
	s.yazilanKare++
}

// kareAtildi — bütünlüğü bozuk olduğu için dosyaya YAZILMAYAN kare.
func (s *saglik) kareAtildi() {
	if s == nil {
		return
	}
	s.atilanKare++
}

// siraDurumu — sıra tamponunun sayaçları (dosya kapanırken bir kez).
func (s *saglik) siraDurumu(bosluk, gec uint64) {
	if s == nil {
		return
	}
	s.siraBosluk, s.gecPaket = bosluk, gec
}

// rapor — yan JSON'a yazılacak sözlük.
func (s *saglik) rapor() map[string]any {
	if s == nil {
		return nil
	}
	bagli := simdi().Sub(s.baslangic).Seconds()
	yazilan := 0.0
	if !s.ilkKare.IsZero() {
		yazilan = s.sonKare.Sub(s.ilkKare).Seconds()
	}
	beklenen := s.beklenenBirikmis
	if s.seqVar {
		beklenen += uint64(s.sonSeq-s.ilkSeq) + 1
	}
	alinan := s.alinanBirikmis + s.alinanPaket
	kayip := int64(beklenen) - int64(alinan)
	if kayip < 0 {
		kayip = 0
	}

	toplamKare := s.yazilanKare + s.atilanKare
	bozukOran := 0.0
	if toplamKare > 0 {
		bozukOran = float64(s.atilanKare) / float64(toplamKare)
	}

	durum, neden := "tam", ""
	switch {
	case s.tamponDurdu:
		durum, neden = "eksik", "hedef geç bulundu, bekleyen paketler atıldı"
	case s.ilkKare.IsZero():
		durum, neden = "eksik", "dosyaya hiç kare yazılmadı"
	case bagli > 1 && yazilan < bagli*saglikEsigi:
		durum, neden = "eksik", "akış ömrünün yarısından azı yazıldı"
	// Bütünlük reddi İKİ ölçütü birden istiyor: oran YÜKSEK ve atılan kare
	// hızı MUTLAK olarak da yüksek. Gerekçe: `kareButunlukTabanı`.
	case toplamKare > 0 && bozukOran > kareButunlukEsigi &&
		(yazilan <= 0 || float64(s.atilanKare)/yazilan > kareButunlukTabanı):
		durum, neden = "eksik", "kare bütünlüğü: kareler eksik paketle geldi"
	}

	r := map[string]any{
		"durum":              durum,
		"neden":              neden,
		"bagli_sn":           yuvarla(bagli),
		"yazilan_sn":         yuvarla(yazilan),
		"en_buyuk_bosluk_sn": yuvarla(s.enBuyukAra.Seconds()),
		"tampon_dusuruldu":   s.tamponDurdu,
		"paket_beklenen":     beklenen,
		"paket_kayip":        kayip,
	}
	// Seste de yazılıyor ama orada `kare_atilan` daima 0: bir Opus paketi
	// zaten bir kare, parçalanma yok, dolayısıyla bütünlük sorunu da yok.
	if toplamKare > 0 {
		r["kare_yazilan"] = s.yazilanKare
		r["kare_atilan"] = s.atilanKare
		r["kare_atilan_oran"] = yuvarla(bozukOran)
		if yazilan > 0 {
			r["kare_atilan_hiz"] = yuvarla(float64(s.atilanKare) / yazilan)
		}
	}
	if s.siraBosluk > 0 {
		r["sira_boslugu"] = s.siraBosluk
	}
	if s.gecPaket > 0 {
		r["gec_paket"] = s.gecPaket
	}
	return r
}

func yuvarla(f float64) float64 {
	return float64(int64(f*1000+0.5)) / 1000
}

// kaynakTuru — rapora bilgi olarak giriyor (kamera / paylaşım / mikrofon).
func kaynakTuru(ti *livekit.TrackInfo) string { return kaynakAdı(ti) }
