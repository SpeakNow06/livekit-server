// SPEAKNOW FORK (AUDIO-NC): SFU-içi gürültü temizliği — native kütüphane köprüsü.
//
// libdf (DeepFilterNet3, Rust/tract) ve libopus'u purego ile ÇALIŞMA ANINDA
// yükler (dlopen). cgo YOK → CGO_ENABLED=0 statik build ve cross-arch imaj
// düzeni aynen korunur; imaja yalnız iki .so + model tar'ı girer (Dockerfile
// dfn-builder stage'i). Kütüphaneler yüklenemezse denoise sessizce devre dışı
// kalır (fail-open: ses ASLA kesilmez, ham geçer).
//
// libdf C yüzeyi (DeepFilterNet libDF/src/capi.rs, v0.5.6):
//   DFState* df_create(char* model_tar_path, float atten_lim_db, char* log_level /*nullable*/);
//   size_t   df_get_frame_length(DFState*);            // 48 kHz'de 480 (10 ms hop)
//   float    df_process_frame(DFState*, float* in, float* out);
//   void     df_set_post_filter_beta(DFState*, float);
//   void     df_free(DFState*);
package audiodenoise

import (
	"fmt"
	"sync"

	"github.com/ebitengine/purego"
)

const (
	opusApplicationVoIP = 2048 // OPUS_APPLICATION_VOIP (opus_defines.h)
	maxPCMSamples       = 5760 // 120ms @ 48k — opus_decode üst sınırı
)

var (
	nativeOnce sync.Once
	nativeErr  error

	// libdf
	dfCreate          func(modelPath *byte, attenLim float32, logLevel *byte) uintptr
	dfGetFrameLength  func(st uintptr) uintptr
	dfProcessFrame    func(st uintptr, in *float32, out *float32) float32
	dfSetPostFilterB  func(st uintptr, beta float32)
	dfFree            func(st uintptr)

	// libopus
	opusDecoderCreate  func(fs int32, ch int32, errPtr *int32) uintptr
	opusDecodeFloat    func(dec uintptr, data *byte, length int32, pcm *float32, frameSize int32, decodeFec int32) int32
	opusDecoderDestroy func(dec uintptr)
	opusEncoderCreate  func(fs int32, ch int32, app int32, errPtr *int32) uintptr
	opusEncodeFloat    func(enc uintptr, pcm *float32, frameSize int32, out *byte, maxBytes int32) int32
	opusEncoderDestroy func(enc uintptr)
)

// cstr: Go string → NUL-sonlu C string (purego'ya *byte olarak geçer; nil = NULL).
func cstr(s string) *byte {
	if s == "" {
		return nil
	}
	b := append([]byte(s), 0)
	return &b[0]
}

func loadNative(libdfPath, libopusPath string) error {
	nativeOnce.Do(func() {
		df, err := purego.Dlopen(libdfPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			nativeErr = fmt.Errorf("libdf yüklenemedi (%s): %w", libdfPath, err)
			return
		}
		op, err := purego.Dlopen(libopusPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			nativeErr = fmt.Errorf("libopus yüklenemedi (%s): %w", libopusPath, err)
			return
		}

		defer func() {
			// RegisterLibFunc eksik sembolde panic'ler — fail-open kalmak için yakala.
			if r := recover(); r != nil {
				nativeErr = fmt.Errorf("native sembol bağlama hatası: %v", r)
			}
		}()

		purego.RegisterLibFunc(&dfCreate, df, "df_create")
		purego.RegisterLibFunc(&dfGetFrameLength, df, "df_get_frame_length")
		purego.RegisterLibFunc(&dfProcessFrame, df, "df_process_frame")
		purego.RegisterLibFunc(&dfSetPostFilterB, df, "df_set_post_filter_beta")
		purego.RegisterLibFunc(&dfFree, df, "df_free")

		purego.RegisterLibFunc(&opusDecoderCreate, op, "opus_decoder_create")
		purego.RegisterLibFunc(&opusDecodeFloat, op, "opus_decode_float")
		purego.RegisterLibFunc(&opusDecoderDestroy, op, "opus_decoder_destroy")
		purego.RegisterLibFunc(&opusEncoderCreate, op, "opus_encoder_create")
		purego.RegisterLibFunc(&opusEncodeFloat, op, "opus_encode_float")
		purego.RegisterLibFunc(&opusEncoderDestroy, op, "opus_encoder_destroy")
	})
	return nativeErr
}
