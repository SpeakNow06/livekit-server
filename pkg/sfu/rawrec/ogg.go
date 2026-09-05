package rawrec

import (
	"encoding/binary"
	"os"
)

// Ogg/Opus yazıcı — `recording_service/app.py` içindeki `_OggOpusWriter`'ın
// BİREBİR karşılığı. Çıktı bayt düzeyinde uyumlu olmalı: postprocess
// (`_ogg_paketleri_oku`, `_ogg_bosluk_doldur`, `_ham_sure`) bugünkü dosyayı
// okuyor ve DEĞİŞMEYECEK.
//
// ÖN-ATLAMA (pre-skip) = 0 — bilinçli.
// OpusHead'deki pre-skip, çözücünün başta atacağı örnek sayısı. Sıfır
// verildiğinde granül konumu doğrudan "paketin başlangıcının çalma zamanı"
// oluyor ve çapa hesabıyla birebir örtüşüyor. Gerçek bir kodlayıcı 312/3840
// gibi bir değer yazardı; burada dosyayı biz kuruyoruz ve zaman ekseninin
// kaymaması tek önceliğimiz.
//
// GRANÜL = paketin RTP damgasının ilk pakete göre farkı + paketin örnek sayısı.
// Yani mutlak konum; DTX boşlukları granülde BOŞLUK olarak duruyor ve
// postprocess onları `_ogg_bosluk_doldur` ile sessizlikle dolduruyor.

const preSkip = 0

type oggYazıcı struct {
	fh     *os.File
	seq    uint32
	serial uint32
}

func newOggYazıcı(fh *os.File, kanal int, serial uint32) *oggYazıcı {
	if kanal < 1 {
		kanal = 1
	}
	w := &oggYazıcı{fh: fh, serial: serial}
	w.sayfa(w.opusHead(kanal), 0, true, false)
	w.sayfa(w.opusTags(), 0, false, false)
	return w
}

func (w *oggYazıcı) opusHead(kanal int) []byte {
	b := make([]byte, 0, 19)
	b = append(b, []byte("OpusHead")...)
	b = append(b, 1, byte(kanal))
	b = binary.LittleEndian.AppendUint16(b, preSkip)
	b = binary.LittleEndian.AppendUint32(b, 48000)
	b = binary.LittleEndian.AppendUint16(b, 0) // output gain
	b = append(b, 0)                           // kanal eşleme ailesi
	return b
}

func (w *oggYazıcı) opusTags() []byte {
	vendor := []byte("monopol-raw")
	b := make([]byte, 0, 8+4+len(vendor)+4)
	b = append(b, []byte("OpusTags")...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(vendor)))
	b = append(b, vendor...)
	b = binary.LittleEndian.AppendUint32(b, 0) // yorum sayısı
	return b
}

// write — bir Opus paketini kendi sayfasına yazar.
func (w *oggYazıcı) write(payload []byte, granül uint64) {
	w.sayfa(payload, granül, false, false)
}

// finish — akışın sonunu işaretleyen boş sayfa (EOS biti).
func (w *oggYazıcı) finish() {
	w.sayfa(nil, 0, false, true)
}

func (w *oggYazıcı) sayfa(payload []byte, granül uint64, bos, eos bool) {
	// Segment tablosu: 255'lik parçalar + kalan. 255 segmentten uzun paket tek
	// sayfaya sığmaz — ses paketleri en fazla birkaç yüz bayt olduğu için
	// pratikte olmuyor, yine de atlıyoruz (bozuk sayfa yazmaktansa).
	n := len(payload)
	segs := make([]byte, 0, n/255+1)
	for n >= 255 {
		segs = append(segs, 255)
		n -= 255
	}
	segs = append(segs, byte(n))
	if len(segs) > 255 {
		return
	}

	var flags byte
	if bos {
		flags |= 0x02
	}
	if eos {
		flags |= 0x04
	}

	head := make([]byte, 0, 27+len(segs))
	head = append(head, []byte("OggS")...)
	head = append(head, 0, flags)
	head = binary.LittleEndian.AppendUint64(head, granül)
	head = binary.LittleEndian.AppendUint32(head, w.serial)
	head = binary.LittleEndian.AppendUint32(head, w.seq)
	head = append(head, 0, 0, 0, 0) // CRC alanı — hesap için SIFIR olmalı
	head = append(head, byte(len(segs)))
	head = append(head, segs...)

	sayfa := append(head, payload...)
	crc := oggCRC(sayfa)
	binary.LittleEndian.PutUint32(sayfa[22:26], crc)

	_, _ = w.fh.Write(sayfa)
	w.seq++
}

// ── Ogg CRC-32 ──────────────────────────────────────────────────────────────
// Polinom 0x04c11db7, YANSITMASIZ (ne giriş ne çıkış), başlangıç 0, son XOR
// yok. Standart zlib CRC32'den FARKLI — `hash/crc32` kullanılamaz.

var crcTablo [256]uint32

func init() {
	for i := 0; i < 256; i++ {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if r&0x80000000 != 0 {
				r = (r << 1) ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		crcTablo[i] = r
	}
}

func oggCRC(b []byte) uint32 {
	var crc uint32
	for _, x := range b {
		crc = (crc << 8) ^ crcTablo[byte(crc>>24)^x]
	}
	return crc
}
