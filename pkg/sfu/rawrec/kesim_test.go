package rawrec

import (
	"testing"
	"time"
)

// Kayıt öncesi kesim = düğmenin anı (ön pay yok, rawrec37): anahtar başlangıcı
// taşıyorsa o an, taşımıyorsa sıfır (kesme yok). Sağlık tabanı kesime çekilir,
// atılan süre raporlanır.
func TestHedefBaslangic(t *testing.T) {
	var yok *hedef
	if !yok.baslangic().IsZero() {
		t.Fatal("nil hedefte başlangıç sıfır olmalı")
	}
	if !(&hedef{RecordingID: 1, Dir: "/x"}).baslangic().IsZero() {
		t.Fatal("baslangic_ms yoksa sıfır olmalı (eski anahtar: kesme yok)")
	}
	bas := time.Date(2026, 9, 13, 13, 34, 37, 0, time.UTC)
	h := &hedef{RecordingID: 882, Dir: "/x", BaslangicMs: bas.UnixMilli()}
	if k := h.baslangic(); !k.Equal(bas) {
		t.Fatalf("başlangıç = %v, beklenen %v (ön pay olmamalı)", k, bas)
	}

	s := yeniSaglik()
	s.baslangic = bas.Add(-57 * time.Second) // yazıcı 57 sn önce kuruldu (paylaşım önce açıldı)
	s.kesimUygulandi(h.baslangic())
	if !s.baslangic.Equal(bas) {
		t.Fatalf("sağlık tabanı kesime çekilmedi: %v", s.baslangic)
	}
	r := s.rapor()
	if v, _ := r["kayit_oncesi_atilan_sn"].(float64); v < 56.9 || v > 57.1 {
		t.Fatalf("kayit_oncesi_atilan_sn = %v, beklenen ~57", r["kayit_oncesi_atilan_sn"])
	}
	// Kesim tabandan eskiyse (kayıt tampondan önce başladı) taban değişmez.
	s2 := yeniSaglik()
	s2.baslangic = bas.Add(10 * time.Second)
	s2.kesimUygulandi(h.baslangic())
	if !s2.baslangic.Equal(bas.Add(10 * time.Second)) {
		t.Fatal("kesim tabandan eskiyken taban değişmemeli")
	}
	if _, var_ := s2.rapor()["kayit_oncesi_atilan_sn"]; var_ {
		t.Fatal("atılan süre yokken alan yazılmamalı")
	}
}
