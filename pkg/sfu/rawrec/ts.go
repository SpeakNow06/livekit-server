package rawrec

import (
	"os"
)

// ── MPEG-TS KABI (Aşama 4, 2026-09-13) ──────────────────────────────────────
//
// H.264 IVF'e SIĞMAZ (IVF bir VPx/AV1 kabı). Tarayıcı yolu H.264'ü zaten
// MPEG-TS olarak yazıyor (`recording_service/app.py` `_TsWriter`) ve
// postprocess `.ts`i tanıyor; bu yazıcı o Python sınıfının BİREBİR Go
// karşılığı — aynı PID'ler, aynı PES/PCR düzeni, aynı PAT/PMT tekrarı. Aynı
// kareler iki yazıcıdan da bayt bayt aynı dosyayı üretiyor (geliştirme
// sırasında karşılaştırıldı), yani postprocess'in `.ts` için ne yapıyorsa
// (remux, süre, kare tarama, saat kayması düzeltmesi) SFU dosyasına da
// aynen uygulanıyor.
//
// DÜZEN (hepsi 188 baytlık paket):
//   · PAT (PID 0) + PMT (PID 0x1000): ilk karede, her ANAHTAR karede ve en geç
//     her 100 karede bir — dosya kesilse bile son anahtar kareden itibaren
//     okunabilir kalıyor.
//   · Video PID 0x100, stream_type 0x1B (H.264). PES stream_id 0xE0,
//     PES_packet_length 0 (sınırsız — video için geçerli), yalnız PTS.
//   · Her karenin İLK paketinde adaptation field + PCR (= PTS), PUSI=1.
//   · Son paket adaptation field dolgusuyla tamamlanıyor.
//
// SPS/PPS: tarayıcılar her anahtar kareyle gönderiyor; yine de son görülen
// SPS/PPS saklanıyor ve SPS'siz bir anahtar kare gelirse başına ekleniyor
// (çözücü SPS'siz IDR'ı açamaz). Normal akışta bu yol hiç tetiklenmez.

const (
	tsPaketBoyu     = 188
	tsPidPAT        = 0x0000
	tsPidPMT        = 0x1000
	tsPidVideo      = 0x0100
	tsStreamH264    = 0x1B
	tsSIAraligi     = 100 // PAT/PMT en geç her 100 karede bir
	tsPTSMaske      = 0x1FFFFFFFF
	tsPESBaslikBoyu = 9 + 5 // 00 00 01 E0 | len(2) | 80 80 05 | PTS(5)
)

type tsYazıcı struct {
	fh       *os.File
	cc       map[int]byte
	pat, pmt []byte
	sinceSI  int
	sps, pps []byte
	kare     int
}

func newTsYazıcı(fh *os.File) *tsYazıcı {
	t := &tsYazıcı{fh: fh, cc: map[int]byte{tsPidPAT: 0, tsPidPMT: 0, tsPidVideo: 0}}
	t.pat = tsBolum(0x00, []byte{
		0x00, 0x01, // transport_stream_id
		0xC1, 0x00, 0x00, // sürüm / bölüm no
		0x00, 0x01, // program_number
		0xE0 | (tsPidPMT >> 8), tsPidPMT & 0xFF,
	})
	t.pmt = tsBolum(0x02, []byte{
		0x00, 0x01, // program_number
		0xC1, 0x00, 0x00,
		0xE0 | (tsPidVideo >> 8), tsPidVideo & 0xFF, // PCR PID
		0xF0, 0x00, // program_info_length = 0
		tsStreamH264, 0xE0 | (tsPidVideo >> 8), tsPidVideo & 0xFF, 0xF0, 0x00,
	})
	t.sinceSI = tsSIAraligi // ilk karede PAT/PMT yazılsın
	return t
}

// tsCRC32 — MPEG-2 bölüm CRC'si (poly 0x04C11DB7, init 0xFFFFFFFF, final
// xor yok). Go'nun hash/crc32'si yansıtılmış (reflected) IEEE; aynı şey
// DEĞİL, o yüzden elle.
func tsCRC32(b []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, x := range b {
		crc ^= uint32(x) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// tsBolum — pointer_field + tablo başlığı + gövde + CRC32.
func tsBolum(tableID byte, govde []byte) []byte {
	uzunluk := len(govde) + 4
	bas := []byte{tableID, 0xB0 | byte((uzunluk>>8)&0x0F), byte(uzunluk & 0xFF)}
	out := make([]byte, 0, 1+len(bas)+len(govde)+4)
	out = append(out, 0x00)
	out = append(out, bas...)
	out = append(out, govde...)
	crc := tsCRC32(append(append([]byte{}, bas...), govde...))
	return append(out, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}

func (t *tsYazıcı) baslik(pid int, pusi bool, afc byte) []byte {
	cc := t.cc[pid]
	t.cc[pid] = (cc + 1) & 0x0F
	b1 := byte((pid >> 8) & 0x1F)
	if pusi {
		b1 |= 0x40
	}
	return []byte{0x47, b1, byte(pid & 0xFF), (afc << 4) | cc}
}

func (t *tsYazıcı) siPaketi(pid int, bolum []byte) []byte {
	out := t.baslik(pid, true, 1)
	out = append(out, bolum...)
	for i := len(bolum); i < 184; i++ {
		out = append(out, 0xFF)
	}
	return out
}

func tsPTSAlani(pts uint64) []byte {
	pts &= tsPTSMaske
	return []byte{
		0x21 | byte((pts>>29)&0x0E), // '0010' + PTS[32:30] + marker
		byte((pts >> 22) & 0xFF),
		byte((pts>>14)&0xFE) | 0x01,
		byte((pts >> 7) & 0xFF),
		byte((pts<<1)&0xFE) | 0x01,
	}
}

func tsPCRAlani(pcr uint64) []byte {
	base := pcr & tsPTSMaske
	return []byte{
		7, 0x10, // af uzunluğu, PCR_flag
		byte((base >> 25) & 0xFF),
		byte((base >> 17) & 0xFF),
		byte((base >> 9) & 0xFF),
		byte((base >> 1) & 0xFF),
		byte(((base & 1) << 7) | 0x7E),
		0x00,
	}
}

// nalTipleri — Annex-B akışındaki NAL birimlerini (başlangıç kodu hariç
// konum + tip) sırayla verir.
func annexbNALlar(veri []byte, f func(bas, son int, tip byte)) {
	n := len(veri)
	i := 0
	for i+3 <= n {
		// başlangıç kodu ara: 00 00 01 (öncesinde isteğe bağlı 00)
		if !(veri[i] == 0 && veri[i+1] == 0 && veri[i+2] == 1) {
			i++
			continue
		}
		bas := i + 3
		if bas >= n {
			return
		}
		// bir sonraki başlangıç koduna kadar
		j := bas
		for j+3 <= n && !(veri[j] == 0 && veri[j+1] == 0 && veri[j+2] == 1) {
			j++
		}
		son := j
		if j+3 <= n {
			// 00 00 00 01 biçimindeyse öndeki sıfır NAL'a ait değil
			if son > bas && veri[son-1] == 0 {
				son--
			}
		} else {
			son = n
		}
		if son > bas {
			f(bas, son, veri[bas]&0x1F)
		}
		i = j
	}
}

// write — bir erişim birimini (Annex-B, bir kare) yazar. `anahtar` true ise
// PAT/PMT tekrarlanır ve SPS eksikse saklanan SPS/PPS başa eklenir.
func (t *tsYazıcı) write(kare []byte, pts uint64, anahtar bool) {
	if len(kare) == 0 {
		return
	}
	spsVar := false
	annexbNALlar(kare, func(bas, son int, tip byte) {
		switch tip {
		case 7:
			t.sps = append(t.sps[:0], kare[bas:son]...)
			spsVar = true
		case 8:
			t.pps = append(t.pps[:0], kare[bas:son]...)
		case 5:
			anahtar = true // IDR dilimi = anahtar kare, bayrak ne derse desin
		}
	})
	if anahtar && !spsVar && len(t.sps) > 0 {
		on := make([]byte, 0, 8+len(t.sps)+len(t.pps)+len(kare))
		on = append(on, 0, 0, 0, 1)
		on = append(on, t.sps...)
		if len(t.pps) > 0 {
			on = append(on, 0, 0, 0, 1)
			on = append(on, t.pps...)
		}
		kare = append(on, kare...)
	}

	var out []byte
	if anahtar || t.sinceSI >= tsSIAraligi {
		out = append(out, t.siPaketi(tsPidPAT, t.pat)...)
		out = append(out, t.siPaketi(tsPidPMT, t.pmt)...)
		t.sinceSI = 0
	}
	t.sinceSI++

	// PES: uzunluk 0 = sınırsız, yalnız PTS.
	veri := make([]byte, 0, tsPESBaslikBoyu+len(kare))
	veri = append(veri, 0x00, 0x00, 0x01, 0xE0, 0x00, 0x00, 0x80, 0x80, 0x05)
	veri = append(veri, tsPTSAlani(pts)...)
	veri = append(veri, kare...)

	ilk := true
	for len(veri) > 0 {
		if ilk {
			af := tsPCRAlani(pts)
			oda := 184 - len(af)
			n := oda
			if len(veri) < n {
				n = len(veri)
			}
			parca := veri[:n]
			veri = veri[n:]
			if dolgu := oda - n; dolgu > 0 { // tek pakete sığdı → af ile doldur
				af[0] += byte(dolgu)
				for i := 0; i < dolgu; i++ {
					af = append(af, 0xFF)
				}
			}
			out = append(out, t.baslik(tsPidVideo, true, 3)...)
			out = append(out, af...)
			out = append(out, parca...)
			ilk = false
			continue
		}
		n := 184
		if len(veri) < n {
			n = len(veri)
		}
		parca := veri[:n]
		veri = veri[n:]
		dolgu := 184 - n
		switch {
		case dolgu == 0:
			out = append(out, t.baslik(tsPidVideo, false, 1)...)
		case dolgu == 1:
			out = append(out, t.baslik(tsPidVideo, false, 3)...)
			out = append(out, 0x00)
		default:
			out = append(out, t.baslik(tsPidVideo, false, 3)...)
			out = append(out, byte(dolgu-1), 0x00)
			for i := 0; i < dolgu-2; i++ {
				out = append(out, 0xFF)
			}
		}
		out = append(out, parca...)
	}
	_, _ = t.fh.Write(out)
	t.kare++
}

// finish — TS'te kapanış işi yok (salt ekleme; kesilse bile geçerli).
func (t *tsYazıcı) finish() {}
