package rawrec

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/pion/rtp/codecs"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// ── VP8 ve AV1 gerçek akış testleri (2026-09-14, rawrec39) ─────────────────
//
// H.264 testinin (h264_replay_test.go) kalıbı: ffmpeg gerçek bir kodlayıcıyla
// (libvpx / SVT-AV1) IVF üretir, kareler pion'un paketleyicisiyle RTP
// paketlerine bölünür, anahtar kare bayrağı LiveKit tamponunun yaptığı gibi
// hesaplanır, yazıcıya verilir, çıkan IVF ffprobe ile ÇÖZÜLEREK sayılır.
// Ölçüt: kodek adı doğru, fourcc doğru, çözülen kare sayısı girdiyle aynı.

// ivfKareleriOku — ffmpeg'in yazdığı IVF'ten kare verileri (sırayla).
func ivfKareleriOku(t *testing.T, yol string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(yol)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 32 || string(b[:4]) != "DKIF" {
		t.Fatalf("IVF değil: %s", yol)
	}
	var out [][]byte
	i := 32
	for i+12 <= len(b) {
		boy := int(binary.LittleEndian.Uint32(b[i : i+4]))
		if i+12+boy > len(b) {
			break
		}
		out = append(out, b[i+12:i+12+boy])
		i += 12 + boy
	}
	return out
}

// gercekIVF — testsrc'den 2 sn, 15 fps, GOP 12; kodlayıcı yoksa test atlanır.
func gercekIVF(t *testing.T, dir, ad, enc string, ekstra ...string) [][]byte {
	t.Helper()
	ffmpeg := ffmpegYolu(t, "ffmpeg")
	ham := filepath.Join(dir, ad+".ivf")
	args := []string{"-y", "-v", "error", "-f", "lavfi",
		"-i", "testsrc=size=320x240:rate=15", "-t", "2", "-pix_fmt", "yuv420p",
		"-c:v", enc}
	args = append(args, ekstra...)
	args = append(args, "-f", "ivf", ham)
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg %s üretemedi (%s yok?): %v %s", ad, enc, err, out)
	}
	kareler := ivfKareleriOku(t, ham)
	if len(kareler) < 20 {
		t.Fatalf("çok az kare: %d", len(kareler))
	}
	return kareler
}

func yeniKodekYazici(t *testing.T, dir string, m mime.MimeType, sid string) *VideoWriter {
	t.Helper()
	paketiEtkinlestir(t, dir)
	kaynak := &sahteKaynak{sr: &livekit.RTCPSenderReportState{
		NtpTimestamp: 0xE7000000_00000000, RtpTimestamp: 123456,
		At: 1_700_000_000_000_000_000, AtAdjusted: 1_700_000_000_000_000_000,
	}}
	ti := &livekit.TrackInfo{Sid: sid, Source: livekit.TrackSource_CAMERA,
		Width: 320, Height: 240}
	w := NewVideoWriter(livekit.TrackID("cid-"+sid), ti, 90000, m, kaynak, 0, logger.GetLogger())
	if w == nil {
		t.Fatalf("%s yazıcı kurulamadı", m)
	}
	w.KatmanKaydet(0, kaynak)
	return w
}

func ivfFourcc(t *testing.T, yol string) string {
	t.Helper()
	b, err := os.ReadFile(yol)
	if err != nil || len(b) < 12 {
		t.Fatalf("IVF okunamadı: %v", err)
	}
	return string(b[8:12])
}

func vp8AnahtarMi(payload []byte) bool {
	var v buffer.VP8
	if err := v.Unmarshal(payload); err != nil {
		return false
	}
	return v.IsKeyFrame
}

func TestVP8GercekAkisIVF(t *testing.T) {
	dir := t.TempDir()
	kareler := gercekIVF(t, dir, "vp8", "libvpx", "-deadline", "realtime", "-cpu-used", "8", "-g", "12")
	pl := &codecs.VP8Payloader{EnablePictureID: true}
	var paketler []tpaket
	seq, rtp := uint16(100), uint32(123456)
	for k, kare := range kareler {
		yukler := pl.Payload(1200, kare)
		for i, y := range yukler {
			paketler = append(paketler, tpaket{payload: y, rtp: rtp, marker: i == len(yukler)-1,
				anahtar: vp8AnahtarMi(y), seq: seq, katman: 0, kare: k})
			seq++
		}
		rtp += 6000
	}
	if !paketler[0].anahtar {
		t.Fatalf("ilk kare anahtar olmalıydı")
	}
	w := yeniKodekYazici(t, dir, mime.MimeTypeVP8, "TR_VP8")
	for _, p := range paketler {
		w.Write(p.payload, p.rtp, p.marker, p.anahtar, p.seq, p.katman)
	}
	yol := dosyaBekle(t, filepath.Join(dir, "camraw_1", "*.ivf"))
	w.Close()
	kodek, en, boy, kare, pts := tsProbe(t, yol)
	t.Logf("vp8: kare=%d rtp paketi=%d → kodek=%s %dx%d çözülen=%d pts=%d fourcc=%s",
		len(kareler), len(paketler), kodek, en, boy, kare, len(pts), ivfFourcc(t, yol))
	if fcc := ivfFourcc(t, yol); fcc != "VP80" {
		t.Fatalf("fourcc %q, VP80 bekleniyordu", fcc)
	}
	if kodek != "vp8" || en != 320 || boy != 240 {
		t.Fatalf("akış tanınmadı: %s %dx%d", kodek, en, boy)
	}
	if kare != len(kareler) {
		t.Fatalf("çözülen kare %d, beklenen %d", kare, len(kareler))
	}
}

func TestAV1GercekAkisIVF(t *testing.T) {
	dir := t.TempDir()
	// Düşük gecikme: her TU tek gösterilen kare (gizli ARF yok) → sayım birebir.
	kareler := gercekIVF(t, dir, "av1", "libsvtav1", "-preset", "10", "-g", "12",
		"-svtav1-params", "pred-struct=1:lp=1")
	pl := &codecs.AV1Payloader{}
	var paketler []tpaket
	seq, rtp := uint16(100), uint32(123456)
	anahtarSayisi := 0
	for k, tu := range kareler {
		yukler := pl.Payload(1200, tu)
		if len(yukler) == 0 {
			t.Fatalf("TU %d paketlenemedi", k)
		}
		for i, y := range yukler {
			a := buffer.IsAV1KeyFrame(y)
			if a {
				anahtarSayisi++
			}
			paketler = append(paketler, tpaket{payload: y, rtp: rtp, marker: i == len(yukler)-1,
				anahtar: a, seq: seq, katman: 0, kare: k})
			seq++
		}
		rtp += 6000
	}
	if !paketler[0].anahtar || anahtarSayisi < 2 {
		t.Fatalf("anahtar kare bayrağı: ilk=%v sayı=%d", paketler[0].anahtar, anahtarSayisi)
	}
	w := yeniKodekYazici(t, dir, mime.MimeTypeAV1, "TR_AV1")
	for _, p := range paketler {
		w.Write(p.payload, p.rtp, p.marker, p.anahtar, p.seq, p.katman)
	}
	yol := dosyaBekle(t, filepath.Join(dir, "camraw_1", "*.ivf"))
	w.Close()
	kodek, en, boy, kare, pts := tsProbe(t, yol)
	t.Logf("av1: TU=%d rtp paketi=%d anahtar=%d → kodek=%s %dx%d çözülen=%d pts=%d fourcc=%s",
		len(kareler), len(paketler), anahtarSayisi, kodek, en, boy, kare, len(pts), ivfFourcc(t, yol))
	if fcc := ivfFourcc(t, yol); fcc != "AV01" {
		t.Fatalf("fourcc %q, AV01 bekleniyordu", fcc)
	}
	if kodek != "av1" || en != 320 || boy != 240 {
		t.Fatalf("akış tanınmadı: %s %dx%d", kodek, en, boy)
	}
	if kare != len(kareler) {
		t.Fatalf("çözülen kare %d, beklenen %d", kare, len(kareler))
	}
}
