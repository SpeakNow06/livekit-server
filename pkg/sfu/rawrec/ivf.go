package rawrec

import (
	"encoding/binary"
	"os"
)

// IVF yazıcı — `recording_service/app.py` içindeki `_ivf_file_header` ve kare
// kaydının BİREBİR karşılığı. Çıktı bayt düzeyinde uyumlu olmalı: postprocess
// (`_scan_video_frames`, `_ham_sure`, `_remux_share_mp4`) bugünkü dosyayı
// okuyor ve DEĞİŞMEYECEK.
//
// ZAMAN TABANI 90 kHz — RTP'nin video saati. Kare damgalarını olduğu gibi
// yazıyoruz, dönüştürme yok.
//
// SALT EKLEME: süreç ders ortasında ölse bile dosya geçerli kalıyor; yalnız
// başlıktaki kare sayısı 0 kalır ve ffmpeg onu zaten akıştan sayıyor.

const (
	ivfBaşlıkBoyu = 32
	// Kare sayısı alanının dosya içindeki yeri (başlıkta 24. bayt).
	ivfKareSayısıOfs = 24
)

// ivfFourCC — VP9. Başka bir kodek bu etiketle yazılırsa dosya sözdizimsel
// olarak geçerli görünür ama ÇÖZÜLEMEZ ("Invalid frame marker") ve kare
// taraması sıfır kare bulur — kayıt sessizce görüntüsüz çıkar (kayıt 124 ve
// 158 böyle bozuldu). Yazıcı bu yüzden yalnız VP9 için kuruluyor.
var ivfFourCC = [4]byte{'V', 'P', '9', '0'}

type ivfYazıcı struct {
	fh   *os.File
	kare uint32
}

func newIvfYazıcı(fh *os.File, en, boy uint16) *ivfYazıcı {
	// Genişlik/yükseklik BİLGİ AMAÇLI: gerçek boyut VP9 akışının içinde de
	// var ve ders ortasında değişirse (öğretmen başka pencere paylaşırsa)
	// ffmpeg akıştan okuyor. Sıfır olamaz, en az 2.
	if en < 2 {
		en = 2
	}
	if boy < 2 {
		boy = 2
	}
	b := make([]byte, 0, ivfBaşlıkBoyu)
	b = append(b, 'D', 'K', 'I', 'F')
	b = binary.LittleEndian.AppendUint16(b, 0)             // sürüm
	b = binary.LittleEndian.AppendUint16(b, ivfBaşlıkBoyu) // başlık uzunluğu
	b = append(b, ivfFourCC[:]...)
	b = binary.LittleEndian.AppendUint16(b, en)
	b = binary.LittleEndian.AppendUint16(b, boy)
	b = binary.LittleEndian.AppendUint32(b, 90000) // zaman tabanı payda
	b = binary.LittleEndian.AppendUint32(b, 1)     // zaman tabanı pay
	b = binary.LittleEndian.AppendUint32(b, 0)     // kare sayısı — kapanışta
	b = binary.LittleEndian.AppendUint32(b, 0)     // ayrılmış
	_, _ = fh.Write(b)
	return &ivfYazıcı{fh: fh}
}

// write — bir kareyi kendi kaydıyla yazar: 4 bayt uzunluk + 8 bayt damga.
func (v *ivfYazıcı) write(kare []byte, pts uint64) {
	var h [12]byte
	binary.LittleEndian.PutUint32(h[0:4], uint32(len(kare)))
	binary.LittleEndian.PutUint64(h[4:12], pts)
	_, _ = v.fh.Write(h[:])
	_, _ = v.fh.Write(kare)
	v.kare++
}

// finish — kare sayısını başlığa geri yaz (bazı okuyucular bakıyor).
func (v *ivfYazıcı) finish() {
	if _, err := v.fh.Seek(ivfKareSayısıOfs, 0); err != nil {
		return
	}
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], v.kare)
	_, _ = v.fh.Write(n[:])
}
