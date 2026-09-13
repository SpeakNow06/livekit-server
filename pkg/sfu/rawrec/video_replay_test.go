package rawrec

// ── REPLAY TESTİ (Aşama 1, 2026-09-13) ──────────────────────────────────────
//
// NEDEN VAR: kodek arayüzü refactor'ünün kabul ölçütü "aynı girdi → BYTE
// BİREBİR aynı .ivf". İki canlı kayıt hiçbir zaman aynı girdi değil (paket
// kaybı, katman geçişi, geliş anları), o yüzden girdi BURADA üretiliyor:
// deterministik, ama gerçek akışın bütün kenar hâllerini taşıyan bir paket
// dizisi. Altın sha256 değeri refactor'den ÖNCE bu dosyayla alındı; refactor
// sonrası aynı çıkmak ZORUNDA.
//
// Dizi neleri kapsıyor:
//   · anahtar kare bekleme (dosya ilk anahtar kareyle açılıyor, öncesi atılır)
//   · çok paketli kareler (B … E), marker'la biten kare
//   · E biti kaybolmuş ama marker'ı olan kare
//   · ortasında paket EKSİK kare → bütünlük kapısı atar
//   · sıra bozuk gelen paketler → sıra tamponu düzeltir
//   · kopya paket → sıra tamponu atar
//   · başı (B) gelmemiş kare → toplanmaz
//   · aynı damgada iki kare (eş damga) → monotonluk itmesi (1 ms)
//
// Tek katman: katman geçişi varış SAATİNE bakabiliyor (deterministik değil),
// o yüzden hash'li test tek katmanlı; iki katmanlı ayrı test yalnız sayım
// yapıyor.

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// sahteKaynak — srKaynak'ın test kopyası: sabit bir Sender Report döner.
type sahteKaynak struct {
	sr  *livekit.RTCPSenderReportState
	pli int
}

func (s *sahteKaynak) GetSenderReportData() *livekit.RTCPSenderReportState { return s.sr }
func (s *sahteKaynak) SendPLI(force bool)                                  { s.pli++ }

// paketiEtkinlestir — paketi env'siz açar; Redis YOK (rdb nil → geriDususYaz
// no-op, hedefAra test tarafından veriliyor).
func paketiEtkinlestir(t *testing.T, dir string) {
	t.Helper()
	once.Do(func() {})
	enabled = true
	waitFor = 60 * time.Second
	kfEnÇokKare, kfEnAzSn, kfEnÇokSn = 24, 2*time.Second, 30*time.Second
	eski := hedefAra
	hedefAra = func(sid string, log logger.Logger) *hedef {
		return &hedef{RecordingID: 1, Dir: dir}
	}
	t.Cleanup(func() { hedefAra = eski })
}

// tpaket — üretilen tek bir RTP paketi.
type tpaket struct {
	payload []byte
	rtp     uint32
	marker  bool
	anahtar bool
	seq     uint16
	katman  int32
	kare    int // hangi kareye ait (test süzmesi için)
}

// vp9Yuk — VP9 payload descriptor (1 bayt: I P L F B E V Z) + sahte kare verisi.
// Anahtar karede ilk bayt 0x82 (frame_marker=0b10, key), diğerlerinde 0x86.
func vp9Yuk(b, e, anahtar bool, kare, parca int) []byte {
	var d byte
	if !anahtar {
		d |= 0x40 // P: inter
	}
	if b {
		d |= 0x08
	}
	if e {
		d |= 0x04
	}
	ilk := byte(0x86)
	if anahtar {
		ilk = 0x82
	}
	if !b {
		ilk = byte(0x10 + parca) // kare ortası parçaları — sabit ama farklı
	}
	yuk := []byte{d, ilk}
	for i := 0; i < 40; i++ {
		yuk = append(yuk, byte((kare*7+parca*13+i)&0xff))
	}
	return yuk
}

// replayDizisi — deterministik paket dizisi (bkz. dosya başlığı).
func replayDizisi(katman int32) []tpaket {
	var out []tpaket
	seq := uint16(1000)
	rtp := uint32(90000)
	parcaSayisi := func(k int) int { return 2 + k%4 } // 2..5 paket/kare
	ekle := func(k, i, n int, anahtar bool, eBitiSil bool) {
		b := i == 0
		e := i == n-1 && !eBitiSil
		out = append(out, tpaket{
			payload: vp9Yuk(b, e, anahtar, k, i), rtp: rtp,
			marker: i == n-1, anahtar: anahtar, seq: seq, katman: katman, kare: k})
		seq++
	}
	for k := 0; k < 120; k++ {
		n := parcaSayisi(k)
		anahtar := k%24 == 3 // ilk anahtar kare 3. karede: öncekiler atılmalı
		switch {
		case k == 40: // ortadaki paket EKSİK → kare atılmalı
			for i := 0; i < n; i++ {
				if i == 1 {
					seq++ // paket hiç gelmedi
					continue
				}
				ekle(k, i, n, anahtar, false)
			}
		case k == 60: // sıra bozuk: son iki paket ters GELİŞ sırasında
			for i := 0; i < n; i++ {
				ekle(k, i, n, anahtar, false)
			}
			// sıra numaraları yüklerine bağlı kalıyor, yalnız geliş sırası ters
			out[len(out)-1], out[len(out)-2] = out[len(out)-2], out[len(out)-1]
		case k == 70: // kopya paket
			for i := 0; i < n; i++ {
				ekle(k, i, n, anahtar, false)
				if i == 1 {
					out = append(out, out[len(out)-1]) // aynısı bir daha
				}
			}
		case k == 80: // E biti yok, yalnız marker
			for i := 0; i < n; i++ {
				ekle(k, i, n, anahtar, true)
			}
		case k == 90: // başı (B) hiç gelmedi → toplanmamalı
			seq++ // B'li paket kaybolmuş sayılıyor (sıra da kopuk)
			for i := 1; i < n; i++ {
				ekle(k, i, n, anahtar, false)
			}
		case k == 100: // EŞ DAMGA: bir önceki kareyle aynı rtp
			rtp -= 3000
			for i := 0; i < n; i++ {
				ekle(k, i, n, anahtar, false)
			}
		default:
			for i := 0; i < n; i++ {
				ekle(k, i, n, anahtar, false)
			}
		}
		rtp += 3000 // 30 fps @ 90 kHz
	}
	return out
}

// ivfOku — IVF dosyasını kare listesine ayırır: (pts, uzunluk).
func ivfOku(t *testing.T, yol string) (basliktaKare uint32, kareler [][2]uint64, ham []byte) {
	t.Helper()
	ham, err := os.ReadFile(yol)
	if err != nil {
		t.Fatalf("ivf okunamadı: %v", err)
	}
	if len(ham) < 32 || string(ham[:4]) != "DKIF" {
		t.Fatalf("IVF başlığı yok")
	}
	basliktaKare = binary.LittleEndian.Uint32(ham[24:28])
	i := 32
	for i+12 <= len(ham) {
		boy := binary.LittleEndian.Uint32(ham[i : i+4])
		pts := binary.LittleEndian.Uint64(ham[i+4 : i+12])
		kareler = append(kareler, [2]uint64{pts, uint64(boy)})
		i += 12 + int(boy)
	}
	return
}

func dosyaBekle(t *testing.T, desen string) string {
	t.Helper()
	son := time.Now().Add(5 * time.Second)
	for time.Now().Before(son) {
		if m, _ := filepath.Glob(desen); len(m) > 0 {
			return m[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("dosya açılmadı: %s", desen)
	return ""
}

func yeniTestYazici(t *testing.T, dir string, ustKatman int32) (*VideoWriter, *sahteKaynak) {
	t.Helper()
	paketiEtkinlestir(t, dir)
	kaynak := &sahteKaynak{sr: &livekit.RTCPSenderReportState{
		NtpTimestamp: 0xE7000000_00000000, RtpTimestamp: 90000,
		At: 1_700_000_000_000_000_000, AtAdjusted: 1_700_000_000_000_000_000,
	}}
	ti := &livekit.TrackInfo{Sid: "TR_TEST", Source: livekit.TrackSource_SCREEN_SHARE,
		Width: 640, Height: 480}
	w := NewVideoWriter("cid-test", ti, 90000, mime.MimeTypeVP9, kaynak, ustKatman,
		logger.GetLogger())
	if w == nil {
		t.Fatalf("yazıcı kurulamadı")
	}
	w.KatmanKaydet(0, kaynak)
	return w, kaynak
}

// altinSHA256 — refactor ÖNCESİ (commit 5083f6dd + bu düzenek) alınan değer:
// 18017 bayt, 115 kare; üç tekrarda aynı çıktı. Boşsa test yalnız yazdırır;
// doluysa birebir karşılaştırır. Kasıtlı bir çıktı değişikliğinde (yeni kap,
// yeni damga kuralı) bu değer GEREKÇESİYLE güncellenmeli.
const altinSHA256 = "e3862b35b7684f954bba01f31bdf73df35f37e5e1ff40b643898cedd03d82d2e"

// altinKare — aynı çalıştırmadaki kare sayısı.
const altinKare = 115

func TestReplayTekKatmanBirebir(t *testing.T) {
	dir := t.TempDir()
	w, _ := yeniTestYazici(t, dir, 0)
	for _, p := range replayDizisi(0) {
		w.Write(p.payload, p.rtp, p.marker, p.anahtar, p.seq, p.katman)
	}
	yol := dosyaBekle(t, filepath.Join(dir, "share_1", "*.ivf"))
	w.Close()

	basliktaKare, kareler, ham := ivfOku(t, yol)
	sum := sha256.Sum256(ham)
	hexsum := hex.EncodeToString(sum[:])
	t.Logf("ivf=%s bayt=%d kare=%d baslik_kare=%d sha256=%s",
		filepath.Base(yol), len(ham), len(kareler), basliktaKare, hexsum)
	if int(basliktaKare) != len(kareler) {
		t.Fatalf("başlıktaki kare sayısı (%d) dosyadakiyle (%d) tutmuyor",
			basliktaKare, len(kareler))
	}
	// Beklenen davranış — dizideki kenar hâller:
	//   120 kare − ilk 3 (anahtar kare öncesi) − 1 (k=40 eksik paket)
	//   − 1 (k=90 başsız) = 115
	if len(kareler) != 115 {
		t.Fatalf("kare sayısı %d, beklenen 115", len(kareler))
	}
	for i := 1; i < len(kareler); i++ {
		if kareler[i][0] <= kareler[i-1][0] {
			t.Fatalf("damga artan değil: kare %d pts=%d önceki=%d", i,
				kareler[i][0], kareler[i-1][0])
		}
	}
	if altinSHA256 == "" {
		t.Logf("ALTIN DEĞER HENÜZ YOK — bu değeri altinSHA256'ya yaz: %s (kare %d)",
			hexsum, len(kareler))
		return
	}
	if hexsum != altinSHA256 || len(kareler) != altinKare {
		t.Fatalf("BİREBİR DEĞİL: sha256=%s kare=%d, altın sha256=%s kare=%d",
			hexsum, len(kareler), altinSHA256, altinKare)
	}
}

// TestReplayIkiKatmanSayim — katman geçişi: alt katmandan başlar, üst katmanın
// anahtar karesinde AYNI dosyada yükselir. Hash yok (geçiş varış saatine
// bakabiliyor), yalnız sayım ve monotonluk.
func TestReplayIkiKatmanSayim(t *testing.T) {
	dir := t.TempDir()
	w, _ := yeniTestYazici(t, dir, 1)
	ust := &sahteKaynak{sr: &livekit.RTCPSenderReportState{
		NtpTimestamp: 0xE7000000_00000000, RtpTimestamp: 500000,
		At: 1_700_000_000_000_000_000, AtAdjusted: 1_700_000_000_000_000_000,
	}}
	w.KatmanKaydet(1, ust)

	// alt katman: ilk 60 KARENİN paketleri (paket sayısı kareye göre değişiyor)
	for _, p := range replayDizisi(0) {
		if p.kare >= 60 {
			break
		}
		w.Write(p.payload, p.rtp, p.marker, p.anahtar, p.seq, p.katman)
	}
	dosyaBekle(t, filepath.Join(dir, "share_1", "*.ivf"))
	// Üst katman: bambaşka damga tabanı ve sıra uzayı, anahtar kareyle başlıyor.
	seq := uint16(5000)
	rtp := uint32(500000)
	var ustKare int
	for k := 0; k < 30; k++ {
		n := 3
		anahtar := k == 0 || k == 24
		for i := 0; i < n; i++ {
			w.Write(vp9Yuk(i == 0, i == n-1, anahtar, k, i), rtp, i == n-1, anahtar, seq, 1)
			seq++
		}
		ustKare++
		rtp += 3000
	}
	w.Close()

	yol := dosyaBekle(t, filepath.Join(dir, "share_1", "*.ivf"))
	_, kareler, _ := ivfOku(t, yol)
	t.Logf("iki katman: kare=%d", len(kareler))
	// alt: 60 − 3 (anahtar öncesi) − 1 (k=40 eksik) = 56; üst: 30 → 86
	if len(kareler) != 56+ustKare {
		t.Fatalf("kare sayısı %d, beklenen %d", len(kareler), 56+ustKare)
	}
	for i := 1; i < len(kareler); i++ {
		if kareler[i][0] <= kareler[i-1][0] {
			t.Fatalf("damga artan değil: kare %d", i)
		}
	}
	// Yan JSON yazılmış olmalı.
	if _, err := os.Stat(yol[:len(yol)-4] + ".json"); err != nil {
		t.Fatalf("yan JSON yok: %v", err)
	}
}
