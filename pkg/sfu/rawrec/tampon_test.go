package rawrec

import (
	"testing"
	"time"
)

// Kayan pencere: son `pencere`den eski paketler baştan atılır, iz tutulur.
func TestPencereKirp(t *testing.T) {
	t0 := time.Date(2026, 9, 13, 18, 20, 0, 0, time.UTC)
	var b []paket
	var iz tamponIzi
	for i := 0; i <= 14; i++ {
		p := paket{geliş: t0.Add(time.Duration(i) * time.Second), seq: uint16(i)}
		b = pencereKirp(append(b, p), paketAn, p.geliş, 10*time.Second, &iz)
	}
	// son = 14 sn; 10 sn pencere → 4..14 kalır (11 paket), 0..3 atılır.
	if len(b) != 11 || b[0].seq != 4 || b[len(b)-1].seq != 14 {
		t.Fatalf("pencere yanlış: len=%d ilk=%d son=%d", len(b), b[0].seq, b[len(b)-1].seq)
	}
	if iz.atilan != 4 || !iz.atilanSon.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("iz yanlış: %+v", iz)
	}
	// Sayı freni: 3'e indir → 12..14 kalır.
	b = ustSinirKirp(b, paketAn, 3, &iz)
	if len(b) != 3 || b[0].seq != 12 {
		t.Fatalf("üst sınır yanlış: len=%d ilk=%d", len(b), b[0].seq)
	}
	if iz.atilan != 12 || !iz.atilanSon.Equal(t0.Add(11*time.Second)) {
		t.Fatalf("iz (üst sınır) yanlış: %+v", iz)
	}
	// Sınırın altında dokunulmaz.
	if b2 := ustSinirKirp(b, paketAn, 3, &iz); len(b2) != 3 || iz.atilan != 12 {
		t.Fatal("sınır aşılmamışken atılmamalı")
	}
}

// "Eksik" YALNIZ kaydın içinden paket atıldıysa; kayıt öncesi taşma "tam".
func TestKayanTamponSonucu(t *testing.T) {
	t0 := time.Date(2026, 9, 13, 18, 20, 0, 0, time.UTC)
	iz := tamponIzi{atilan: 100, atilanSon: t0.Add(3 * time.Second)}
	saglam := func(s *saglik) { // diğer kuralları tetiklemesin
		s.baslangic = t0
		s.kareYazildi(t0.Add(4 * time.Second))
		s.kareYazildi(t0.Add(60 * time.Second))
		s.paketGeldi(1, t0.Add(60*time.Second))
	}
	// Kayıt, atılanlardan SONRA başladı → yalnız kayıt öncesi taştı → tam.
	s := yeniSaglik()
	saglam(s)
	s.kayanTamponSonucu(10*time.Second, iz, t0.Add(5*time.Second))
	r := s.rapor()
	if r["durum"] != "tam" {
		t.Fatalf("kayıt öncesi taşma 'tam' olmalı: %v", r)
	}
	if r["tampon_atilan_paket"] != 100 || r["tampon_penceresi_sn"] != 10.0 {
		t.Fatalf("alanlar yanlış: %v", r)
	}
	if _, var_ := r["kayit_basi_kayip_sn"]; var_ {
		t.Fatal("kayıp yokken alan yazılmamalı")
	}
	// Kayıt, atılan paketlerden ÖNCE başladı → kaydın 1 sn'si atıldı → eksik.
	s = yeniSaglik()
	saglam(s)
	s.kayanTamponSonucu(10*time.Second, iz, t0.Add(2*time.Second))
	r = s.rapor()
	if r["durum"] != "eksik" || r["kayit_basi_kayip_sn"] != 1.0 {
		t.Fatalf("kayıt içi taşma 'eksik' olmalı: %v", r)
	}
	// Eski anahtar (başlangıç yok) → karar verilemez → tam.
	s = yeniSaglik()
	saglam(s)
	s.kayanTamponSonucu(10*time.Second, iz, time.Time{})
	if s.rapor()["durum"] != "tam" {
		t.Fatal("başlangıçsız anahtarda 'tam' kalmalı")
	}
	// Hiç taşma yok → alan yok.
	s = yeniSaglik()
	saglam(s)
	s.kayanTamponSonucu(10*time.Second, tamponIzi{}, t0)
	if _, var_ := s.rapor()["tampon_atilan_paket"]; var_ {
		t.Fatal("taşma yokken alan yazılmamalı")
	}
}
