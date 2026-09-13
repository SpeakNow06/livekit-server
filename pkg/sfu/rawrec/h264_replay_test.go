package rawrec

// ── H.264 UÇTAN UCA TEST (Aşama 4, 2026-09-13) ──────────────────────────────
//
// Sentetik NAL değil, GERÇEK H.264: ffmpeg/libx264 ile üretilen akış pion'un
// RTP paketleyicisinden (STAP-A + FU-A, tarayıcıların yaptığı gibi) geçirilip
// yazıcıya veriliyor; çıkan .ts ffprobe ile ÇÖZÜLÜYOR — kare sayısı, boyut,
// kodek ve monoton PTS denetleniyor. ffmpeg yoksa test atlanıyor.
//
// İkinci senaryo bir FU-A parçası düşürüyor: o kare bütünlük kapısında
// atılmalı, dosya yine hatasız çözülmeli (kare sayısı bir eksik).
//
// Üçüncü test Go TS yazıcısını tarayıcı yolunun Python `_TsWriter`'ıyla
// (recording_service/app.py) BAYT BAYT karşılaştırıyor: aynı kareler → aynı
// dosya. speaknow-server yerelde yoksa atlanıyor.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp/codecs"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

func ffmpegYolu(t *testing.T, ad string) string {
	t.Helper()
	if p, err := exec.LookPath(ad); err == nil {
		return p
	}
	if _, err := os.Stat("/opt/homebrew/bin/" + ad); err == nil {
		return "/opt/homebrew/bin/" + ad
	}
	t.Skipf("%s yok, test atlandı", ad)
	return ""
}

// h264Erisim — bir erişim birimi (Annex-B) + anahtar bayrağı.
type h264Erisim struct {
	veri    []byte
	anahtar bool
}

// gercekH264 — libx264 ile 2 sn, 15 fps, B karesiz akış; erişim birimlerine
// ffprobe'un paket sınırlarıyla bölünüyor.
func gercekH264(t *testing.T, dir string) []h264Erisim {
	t.Helper()
	ffmpeg := ffmpegYolu(t, "ffmpeg")
	ffprobe := ffmpegYolu(t, "ffprobe")
	ham := filepath.Join(dir, "kaynak.h264")
	cmd := exec.Command(ffmpeg, "-y", "-v", "error", "-f", "lavfi",
		"-i", "testsrc=size=320x240:rate=15", "-t", "2",
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-g", "12", "-bf", "0", "-pix_fmt", "yuv420p", "-f", "h264", ham)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg H.264 üretemedi (libx264 yok?): %v %s", err, out)
	}
	probe := exec.Command(ffprobe, "-v", "error", "-show_entries", "packet=size,flags",
		"-of", "csv=p=0", ham)
	out, err := probe.Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	b, err := os.ReadFile(ham)
	if err != nil {
		t.Fatal(err)
	}
	var aus []h264Erisim
	ofs := 0
	for _, satir := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		alan := strings.Split(strings.TrimSpace(satir), ",")
		if len(alan) < 2 {
			continue
		}
		boy, err := strconv.Atoi(alan[0])
		if err != nil || ofs+boy > len(b) {
			t.Fatalf("paket boyu okunamadı: %q", satir)
		}
		aus = append(aus, h264Erisim{veri: b[ofs : ofs+boy],
			anahtar: strings.Contains(alan[1], "K")})
		ofs += boy
	}
	if len(aus) < 20 {
		t.Fatalf("çok az erişim birimi: %d", len(aus))
	}
	return aus
}

// h264Paketle — erişim birimlerini RTP paketlerine böler (pion payloader:
// SPS+PPS → STAP-A, büyük NAL → FU-A). Anahtar bayrağı tamponun kuralıyla.
func h264Paketle(aus []h264Erisim) []tpaket {
	var out []tpaket
	seq := uint16(100)
	rtp := uint32(123456)
	pl := &codecs.H264Payloader{}
	for k, au := range aus {
		yukler := pl.Payload(1200, au.veri)
		for i, y := range yukler {
			out = append(out, tpaket{payload: y, rtp: rtp, marker: i == len(yukler)-1,
				anahtar: buffer.IsH264KeyFrame(y), seq: seq, katman: 0, kare: k})
			seq++
		}
		rtp += 6000 // 15 fps @ 90 kHz
	}
	return out
}

func yeniH264Yazici(t *testing.T, dir string) *VideoWriter {
	t.Helper()
	paketiEtkinlestir(t, dir)
	kaynak := &sahteKaynak{sr: &livekit.RTCPSenderReportState{
		NtpTimestamp: 0xE7000000_00000000, RtpTimestamp: 123456,
		At: 1_700_000_000_000_000_000, AtAdjusted: 1_700_000_000_000_000_000,
	}}
	ti := &livekit.TrackInfo{Sid: "TR_H264", Source: livekit.TrackSource_CAMERA,
		Width: 320, Height: 240}
	w := NewVideoWriter("cid-h264", ti, 90000, mime.MimeTypeH264, kaynak, 0,
		logger.GetLogger())
	if w == nil {
		t.Fatalf("H.264 yazıcı kurulamadı")
	}
	w.KatmanKaydet(0, kaynak)
	return w
}

// tsProbe — ffprobe ile çöz: (kodek, en, boy, çözülen kare, pts listesi).
func tsProbe(t *testing.T, yol string) (kodek string, en, boy, kare int, pts []int64) {
	t.Helper()
	ffprobe := ffmpegYolu(t, "ffprobe")
	cmd := exec.Command(ffprobe, "-v", "error", "-count_frames", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name,width,height,nb_read_frames",
		"-of", "csv=p=0", yol)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffprobe: %v %s", err, stderr.String())
	}
	if s := strings.TrimSpace(stderr.String()); s != "" {
		t.Fatalf("ffprobe HATA verdi (çözülemeyen kare?): %s", s)
	}
	// TS'te akış hem "program" hem "stream" bölümünde yazılıyor → satır iki
	// kez geliyor; ilkini al.
	satir := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	alan := strings.Split(strings.TrimSpace(satir), ",")
	if len(alan) < 4 {
		t.Fatalf("ffprobe çıktısı beklenmedik: %q", out)
	}
	kodek = alan[0]
	en, _ = strconv.Atoi(alan[1])
	boy, _ = strconv.Atoi(alan[2])
	kare, _ = strconv.Atoi(alan[3])
	cmd2 := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "packet=pts", "-of", "csv=p=0", yol)
	out2, err := cmd2.Output()
	if err != nil {
		t.Fatalf("ffprobe paketler: %v", err)
	}
	for _, s := range strings.Split(strings.TrimSpace(string(out2)), "\n") {
		s = strings.TrimSuffix(strings.TrimSpace(s), ",")
		if s == "" {
			continue
		}
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("pts okunamadı: %q", s)
		}
		pts = append(pts, v)
	}
	return
}

func TestH264GercekAkisTS(t *testing.T) {
	dir := t.TempDir()
	aus := gercekH264(t, dir)
	paketler := h264Paketle(aus)
	t.Logf("erişim birimi=%d rtp paketi=%d", len(aus), len(paketler))

	w := yeniH264Yazici(t, dir)
	for _, p := range paketler {
		w.Write(p.payload, p.rtp, p.marker, p.anahtar, p.seq, p.katman)
	}
	yol := dosyaBekle(t, filepath.Join(dir, "camraw_1", "*.ts"))
	w.Close()

	kodek, en, boy, kare, pts := tsProbe(t, yol)
	t.Logf("ts=%s kodek=%s %dx%d çözülen_kare=%d pts_paket=%d",
		filepath.Base(yol), kodek, en, boy, kare, len(pts))
	if kodek != "h264" || en != 320 || boy != 240 {
		t.Fatalf("akış tanınmadı: %s %dx%d", kodek, en, boy)
	}
	if kare != len(aus) {
		t.Fatalf("çözülen kare %d, beklenen %d", kare, len(aus))
	}
	for i := 1; i < len(pts); i++ {
		if pts[i] <= pts[i-1] {
			t.Fatalf("pts artan değil: %d → %d", pts[i-1], pts[i])
		}
	}
	// 15 fps → ardışık PTS farkı 6000 tık
	if len(pts) >= 2 && pts[1]-pts[0] != 6000 {
		t.Fatalf("pts adımı %d, beklenen 6000", pts[1]-pts[0])
	}
	// Yan JSON
	if _, err := os.Stat(yol[:len(yol)-3] + ".json"); err != nil {
		t.Fatalf("yan JSON yok: %v", err)
	}
}

func TestH264ParcaKaybiKareAtilir(t *testing.T) {
	dir := t.TempDir()
	aus := gercekH264(t, dir)
	paketler := h264Paketle(aus)
	// Anahtar olmayan, çok parçalı bir karenin ORTA parçasını düşür.
	hedef := -1
	for k := 5; k < len(aus) && hedef < 0; k++ {
		var idx []int
		for i, p := range paketler {
			if p.kare == k {
				idx = append(idx, i)
			}
		}
		if !aus[k].anahtar && len(idx) >= 3 {
			hedef = idx[1]
		}
	}
	if hedef < 0 {
		t.Skip("çok parçalı anahtar olmayan kare bulunamadı")
	}
	dusen := paketler[hedef].kare
	w := yeniH264Yazici(t, dir)
	for i, p := range paketler {
		if i == hedef {
			continue
		}
		w.Write(p.payload, p.rtp, p.marker, p.anahtar, p.seq, p.katman)
	}
	yol := dosyaBekle(t, filepath.Join(dir, "camraw_1", "*.ts"))
	w.Close()
	_, _, _, kare, _ := tsProbe(t, yol)
	t.Logf("düşürülen kare=%d çözülen=%d/%d", dusen, kare, len(aus))
	if kare != len(aus)-1 {
		t.Fatalf("çözülen kare %d, beklenen %d (bozuk kare atılmalıydı)", kare, len(aus)-1)
	}
}

// TestTsPythonBirebir — Go TS yazıcısı == tarayıcı yolunun Python `_TsWriter`'ı.
func TestTsPythonBirebir(t *testing.T) {
	appPy := filepath.Join("..", "..", "..", "..", "speaknow-server", "recording_service", "app.py")
	if _, err := os.Stat(appPy); err != nil {
		t.Skip("speaknow-server/recording_service/app.py yerelde yok")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 yok")
	}
	dir := t.TempDir()
	aus := gercekH264(t, dir)

	// Go
	goYol := filepath.Join(dir, "go.ts")
	fh, err := os.Create(goYol)
	if err != nil {
		t.Fatal(err)
	}
	ts := newTsYazıcı(fh)
	var giris bytes.Buffer // Python'a: [pts u64][key u8][len u32][veri]
	pts := uint64(0)
	for _, au := range aus {
		ts.write(au.veri, pts, au.anahtar)
		_ = binary.Write(&giris, binary.LittleEndian, pts)
		key := byte(0)
		if au.anahtar {
			key = 1
		}
		giris.WriteByte(key)
		_ = binary.Write(&giris, binary.LittleEndian, uint32(len(au.veri)))
		giris.Write(au.veri)
		pts += 6000
	}
	ts.finish()
	fh.Close()

	// Python: app.py'den yalnız TS parçalarını çekip çalıştır.
	pyYol := filepath.Join(dir, "py.ts")
	script := fmt.Sprintf(`
import re, struct, sys
src = open(%q, encoding='utf-8').read()
parca = []
for m in re.finditer(r'^_TS_[A-Z0-9_]+ = .*$', src, re.M):
    parca.append(m.group(0))
def blok(bas):
    i = src.index(bas)
    j = src.find('\n\n\n', i)
    return src[i:j]
parca.append(blok('def _crc32_mpeg'))
parca.append(blok('class _TsWriter'))
ns = {'struct': struct}
exec('\n'.join(parca), ns)
out = open(%q, 'wb')
w = ns['_TsWriter'](out)
data = sys.stdin.buffer.read()
i = 0
while i < len(data):
    pts, key, n = struct.unpack_from('<QBI', data, i); i += 13
    w.write_frame(data[i:i+n], pts, bool(key)); i += n
out.close()
`, appPy, pyYol)
	cmd := exec.Command(py, "-c", script)
	cmd.Stdin = &giris
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("python _TsWriter: %v\n%s", err, out)
	}
	g, _ := os.ReadFile(goYol)
	p, _ := os.ReadFile(pyYol)
	sg, sp := sha256.Sum256(g), sha256.Sum256(p)
	t.Logf("go=%d bayt %s  python=%d bayt %s", len(g), hex.EncodeToString(sg[:8]),
		len(p), hex.EncodeToString(sp[:8]))
	if !bytes.Equal(g, p) {
		t.Fatalf("Go TS yazıcısı Python _TsWriter ile BİREBİR DEĞİL (go %d bayt, python %d bayt)",
			len(g), len(p))
	}
	_ = time.Second
}
