// SPEAKNOW FORK (AUDIO-NC): SFU-içi mikrofon gürültü temizliği (Google Meet modeli).
//
// forwardRTP döngüsünde (pkg/sfu/receiver_base.go), yayınlanan track başına TEK
// noktada: Opus payload'ı çöz → DFN3 → yeniden kodla → payload'ı yerinde değiştir.
// İkinci katılımcı/track/jitter-buffer YOK → bot mimarisinin ~195ms'ine karşılık
// hedef ek gecikme ~DFN lookahead + ~1-2ms işlem.
//
// GECİKME-ÖNCELİKLİ tasarım kararları:
//   * Sıralama penceresi YOK: kayıp karede Opus PLC ile decoder+DFN durumu
//     ilerletilir; GEÇ gelen paket DÜŞÜRÜLÜR (abone jitter buffer'ı o kareyi
//     zaten kayıp saydı — ham gürültülü kareyi sonradan çalmaktansa hiç çalmamak).
//   * Her hata yolu FAIL-OPEN: işlenemeyen paket HAM geçer; kurulum hatasında
//     track kalıcı bypass'a düşer. Ses hiçbir koşulda kesilmez.
//
// Kapsam: yalnız MİKROFON (screen-share audio =müzik= receiver_base'de elenir),
// yalnız Opus/RED. Env anahtarları:
//   SN_DENOISE=1                    ana anahtar (yoksa bu kod hiç çalışmaz)
//   SN_DENOISE_LIB=/opt/speaknow/libdf.so
//   SN_DENOISE_OPUS=libopus.so.0
//   SN_DENOISE_MODEL=/opt/speaknow/DeepFilterNet3_onnx.tar.gz
//   SN_DENOISE_ATTEN_DB=100        bastırma tavanı (dB)
//   SN_DENOISE_PF=0                DFN post-filter beta (0=kapalı)
package audiodenoise

import (
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/livekit/protocol/logger"
)

// --- global yapılandırma ---

var (
	cfgOnce    sync.Once
	cfgEnabled bool
	cfgLib     string
	cfgOpus    string
	cfgModel   string
	cfgAtten   float32
	cfgPF      float32
)

func loadCfg() {
	cfgOnce.Do(func() {
		if os.Getenv("SN_DENOISE") != "1" {
			return
		}
		cfgLib = envOr("SN_DENOISE_LIB", "/opt/speaknow/libdf.so")
		cfgOpus = envOr("SN_DENOISE_OPUS", "libopus.so.0")
		cfgModel = envOr("SN_DENOISE_MODEL", "/opt/speaknow/DeepFilterNet3_onnx.tar.gz")
		cfgAtten = envFloat("SN_DENOISE_ATTEN_DB", 100)
		cfgPF = envFloat("SN_DENOISE_PF", 0)
		if err := loadNative(cfgLib, cfgOpus); err != nil {
			logger.Warnw("AUDIO-NC devre dışı: native kütüphaneler yüklenemedi", err)
			return
		}
		cfgEnabled = true
		logger.Infow("AUDIO-NC aktif (SFU-içi DFN3)",
			"lib", cfgLib, "model", cfgModel, "attenDB", cfgAtten)
	})
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envFloat(k string, d float32) float32 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 32); err == nil {
			return float32(f)
		}
	}
	return d
}

// Enabled: ana anahtar + native yükleme başarısı.
func Enabled() bool {
	loadCfg()
	return cfgEnabled
}

// --- paket işleme sonucu ---

type Action int

const (
	ActionBypass  Action = iota // payload'a dokunma (ham geçir)
	ActionReplace               // payload değiştirildi
	ActionDrop                  // paketi hiç iletme (geç gelen kare)
)

// --- per-track işlemci ---

// Processor: TEK forwardRTP goroutine'inden çağrılır — kilitsiz.
type Processor struct {
	isRED  bool
	logger logger.Logger

	df        uintptr
	dec       uintptr
	enc       uintptr
	frameLen  int // DFN hop (480 @48k)
	broken    bool

	inited   bool
	lastSeq  uint16
	lastTS   uint32
	prevRED  []byte // önceki İŞLENMİŞ kare (RED yedeği için)
	prevTS   uint32

	pcmIn  []float32
	pcmOut []float32
	encBuf []byte
	redBuf []byte
	plcBuf []float32

	// istatistik
	nProc, nPLC, nLate, nBypass uint64
	procTime                    time.Duration
	lastLog                     time.Time
}

// NewProcessor: kurulum başarısızsa nil döner (çağıran bypass'a düşer).
func NewProcessor(isRED bool, log logger.Logger) *Processor {
	if !Enabled() {
		return nil
	}
	p := &Processor{isRED: isRED, logger: log, lastLog: time.Now()}

	p.df = dfCreate(cstr(cfgModel), cfgAtten, nil)
	if p.df == 0 {
		log.Warnw("AUDIO-NC: df_create başarısız — track bypass", nil)
		return nil
	}
	if cfgPF > 0 {
		dfSetPostFilterB(p.df, cfgPF)
	}
	p.frameLen = int(dfGetFrameLength(p.df))
	if p.frameLen <= 0 || p.frameLen > maxPCMSamples {
		log.Warnw("AUDIO-NC: beklenmedik frame_len — track bypass", nil, "frameLen", p.frameLen)
		dfFree(p.df)
		return nil
	}

	var oe int32
	p.dec = opusDecoderCreate(48000, 1, &oe)
	if oe != 0 || p.dec == 0 {
		log.Warnw("AUDIO-NC: opus decoder açılamadı — track bypass", nil, "err", oe)
		dfFree(p.df)
		return nil
	}
	p.enc = opusEncoderCreate(48000, 1, opusApplicationVoIP, &oe)
	if oe != 0 || p.enc == 0 {
		log.Warnw("AUDIO-NC: opus encoder açılamadı — track bypass", nil, "err", oe)
		opusDecoderDestroy(p.dec)
		dfFree(p.df)
		return nil
	}

	p.pcmIn = make([]float32, maxPCMSamples)
	p.pcmOut = make([]float32, maxPCMSamples)
	p.plcBuf = make([]float32, maxPCMSamples)
	p.encBuf = make([]byte, 1500)
	p.redBuf = make([]byte, 3000)
	p.prevRED = make([]byte, 0, 1500)
	return p
}

func (p *Processor) Close() {
	if p == nil {
		return
	}
	if p.enc != 0 {
		opusEncoderDestroy(p.enc)
	}
	if p.dec != 0 {
		opusDecoderDestroy(p.dec)
	}
	if p.df != 0 {
		dfFree(p.df)
	}
	p.logger.Infow("AUDIO-NC track kapandı",
		"processed", p.nProc, "plc", p.nPLC, "lateDropped", p.nLate, "bypassed", p.nBypass)
}

// ProcessPacket: payload'ı işler. Dönen dilim Processor'ın kendi buffer'ıdır —
// bir sonraki çağrıya kadar geçerli (forwardRTP fan-out'u senkron, sorun değil).
func (p *Processor) ProcessPacket(seq uint16, ts uint32, payload []byte) ([]byte, Action) {
	if p.broken || len(payload) == 0 {
		p.nBypass++
		return nil, ActionBypass
	}

	// sıra takibi: geç kalan kareyi DÜŞÜR, kayıp karede PLC ile durumu ilerlet
	if p.inited {
		delta := int16(seq - p.lastSeq)
		if delta <= 0 { // duplicate bucket'ta elendi → bu GEÇ gelen kare
			p.nLate++
			return nil, ActionDrop
		}
		if gap := int(delta) - 1; gap > 0 && gap <= 5 {
			for i := 0; i < gap; i++ { // kayıp kareler: decoder+DFN durumunu PLC ile sür
				n := opusDecodeFloat(p.dec, nil, 0, &p.plcBuf[0], int32(maxPCMSamples), 0)
				if n > 0 && int(n)%p.frameLen == 0 {
					for off := 0; off+p.frameLen <= int(n); off += p.frameLen {
						dfProcessFrame(p.df, &p.plcBuf[off], &p.pcmOut[0])
					}
				}
				p.nPLC++
			}
		}
		// gap > 5 (uzun DTX sessizliği vb.) → PLC'siz devam; DFN durumu kalıcıdır.
	}
	p.lastSeq, p.inited = seq, true

	// RED ise birincili çıkar
	opusData := payload
	pt := byte(0)
	if p.isRED {
		prim, err := parseREDPrimary(payload)
		if err != nil {
			p.nBypass++
			return nil, ActionBypass
		}
		opusData, pt = prim.payload, prim.pt
	}
	if len(opusData) == 0 { // DTX/boş birincil → dokunma
		p.nBypass++
		return nil, ActionBypass
	}

	t0 := time.Now()

	// çöz (mono'ya; stereo kaynak downmix edilir)
	n := opusDecodeFloat(p.dec, &opusData[0], int32(len(opusData)), &p.pcmIn[0], int32(maxPCMSamples), 0)
	if n <= 0 || int(n)%p.frameLen != 0 {
		// 480'in katı olmayan egzotik kare süresi → bu track'i kalıcı bypass'a al
		if int(n)%p.frameLen != 0 && n > 0 {
			p.logger.Warnw("AUDIO-NC: kare süresi DFN hop'una bölünmüyor — track bypass", nil,
				"samples", n, "hop", p.frameLen)
			p.broken = true
		}
		p.nBypass++
		return nil, ActionBypass
	}

	// DFN3: 480'lik hop'lar halinde
	for off := 0; off+p.frameLen <= int(n); off += p.frameLen {
		dfProcessFrame(p.df, &p.pcmIn[off], &p.pcmOut[off])
	}

	// yeniden kodla (48k mono; encoder default'ları — VBR)
	encLen := opusEncodeFloat(p.enc, &p.pcmOut[0], n, &p.encBuf[0], int32(len(p.encBuf)))
	if encLen <= 0 {
		p.nBypass++
		return nil, ActionBypass
	}
	cur := p.encBuf[:encLen]

	p.procTime += time.Since(t0)
	p.nProc++
	p.maybeLog()

	var out []byte
	if p.isRED {
		out = buildRED(p.redBuf[:0:cap(p.redBuf)], pt, cur, p.prevRED, ts-p.prevTS)
	} else {
		out = cur
	}

	// işlenmiş kareyi RED yedeği olarak sakla (encBuf bir sonraki karede ezilecek → kopyala)
	p.prevRED = append(p.prevRED[:0], cur...)
	p.prevTS = ts
	_ = p.lastTS
	p.lastTS = ts

	return out, ActionReplace
}

func (p *Processor) maybeLog() {
	if p.nProc%1000 != 0 || time.Since(p.lastLog) < 10*time.Second {
		return
	}
	avgUs := int64(0)
	if p.nProc > 0 {
		avgUs = p.procTime.Microseconds() / int64(p.nProc)
	}
	p.logger.Infow("AUDIO-NC istatistik",
		"processed", p.nProc, "avgProcUs", avgUs,
		"plc", p.nPLC, "lateDropped", p.nLate, "bypassed", p.nBypass)
	p.lastLog = time.Now()
}
