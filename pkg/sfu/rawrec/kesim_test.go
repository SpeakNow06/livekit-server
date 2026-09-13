package rawrec

import (
	"testing"
	"time"
)

// Kayıt öncesi kesim: anahtar başlangıcı taşıyorsa kesim = başlangıç − ön pay;
// taşımıyorsa sıfır (kesme yok). Sağlık tabanı kesime çekilir, atılan süre raporlanır.
func TestHedefKesim(t *testing.T) {
	eski := onRol
	onRol = 2 * time.Second
	defer func() { onRol = eski }()

	var yok *hedef
	if !yok.kesim().IsZero() {
		t.Fatal("nil hedefte kesim sıfır olmalı")
	}
	if !(&hedef{RecordingID: 1, Dir: "/x"}).kesim().IsZero() {
		t.Fatal("baslangic_ms yoksa kesim sıfır olmalı (eski davranış)")
	}
	bas := time.Date(2026, 9, 13, 13, 34, 37, 0, time.UTC)
	h := &hedef{RecordingID: 882, Dir: "/x", BaslangicMs: bas.UnixMilli()}
	if k := h.kesim(); !k.Equal(bas.Add(-2 * time.Second)) {
		t.Fatalf("kesim = %v, beklenen %v", k, bas.Add(-2*time.Second))
	}

	s := yeniSaglik()
	s.baslangic = bas.Add(-57 * time.Second) // yazıcı 57 sn önce kuruldu (paylaşım önce açıldı)
	s.kesimUygulandi(h.kesim())
	if !s.baslangic.Equal(bas.Add(-2 * time.Second)) {
		t.Fatalf("sağlık tabanı kesime çekilmedi: %v", s.baslangic)
	}
	r := s.rapor()
	if v, _ := r["kayit_oncesi_atilan_sn"].(float64); v < 54.9 || v > 55.1 {
		t.Fatalf("kayit_oncesi_atilan_sn = %v, beklenen ~55", r["kayit_oncesi_atilan_sn"])
	}
	// Kesim tabandan eskiyse (kayıt tampondan önce başladı) taban değişmez.
	s2 := yeniSaglik()
	s2.baslangic = bas.Add(10 * time.Second)
	s2.kesimUygulandi(h.kesim())
	if !s2.baslangic.Equal(bas.Add(10 * time.Second)) {
		t.Fatal("kesim tabandan eskiyken taban değişmemeli")
	}
	if _, var_ := s2.rapor()["kayit_oncesi_atilan_sn"]; var_ {
		t.Fatal("atılan süre yokken alan yazılmamalı")
	}
}
