// SPEAKNOW FORK (AUDIO-NC): RFC 2198 (audio/red) payload ayrıştırma + yeniden kurma.
//
// Yayıncı RED gönderiyorsa payload çok-bloklu: [yedek blok başlıkları...][birincil
// başlık][yedek veriler...][birincil veri]. Biz birincili çıkarıp işler, payload'ı
// İŞLENMİŞ birincil + İŞLENMİŞ önceki kare (yedek) ile yeniden kurarız — böylece
// hem RED-abonesi hem primary-çıkaran (RedPrimaryReceiver) temiz ses alır ve kayıp
// kurtarması da temiz kareyle olur.
//
// Blok başlığı (yedek): F(1)=1 | PT(7) | TS-offset(14) | uzunluk(10)  → 4 bayt
// Son başlık (birincil): F(1)=0 | PT(7)                               → 1 bayt
package audiodenoise

import "errors"

var errMalformedRED = errors.New("bozuk RED payload")

type redPrimary struct {
	pt      byte   // birincil bloğun payload type'ı (opus PT'si)
	payload []byte // birincil opus verisi (orijinal buffer'a dilim)
}

// parseREDPrimary birincil opus bloğunu bulur (yedekleri atlar).
func parseREDPrimary(payload []byte) (redPrimary, error) {
	var out redPrimary
	idx := 0
	var blockLens []int
	for {
		if idx >= len(payload) {
			return out, errMalformedRED
		}
		hdr := payload[idx]
		if hdr&0x80 == 0 { // F=0 → birincil başlık (1 bayt)
			out.pt = hdr & 0x7f
			idx++
			break
		}
		// F=1 → 4 baytlık yedek başlığı: PT(7) | TS-off(14) | len(10)
		if idx+4 > len(payload) {
			return out, errMalformedRED
		}
		blockLen := (int(payload[idx+2]&0x03) << 8) | int(payload[idx+3])
		blockLens = append(blockLens, blockLen)
		idx += 4
	}
	// yedek verilerini atla
	for _, bl := range blockLens {
		if idx+bl > len(payload) {
			return out, errMalformedRED
		}
		idx += bl
	}
	if idx > len(payload) {
		return out, errMalformedRED
	}
	out.payload = payload[idx:]
	return out, nil
}

// buildRED işlenmiş kareden RED payload üretir.
// prev boş DEĞİLSE ve tsDelta 14 bit'e sığıyorsa: [yedek=prev][birincil=cur].
// Aksi halde tek-bloklu (yalnız birincil, F=0) — RFC'ye uygun, alıcılar destekler.
func buildRED(dst []byte, pt byte, cur []byte, prev []byte, tsDelta uint32) []byte {
	dst = dst[:0]
	if len(prev) > 0 && tsDelta > 0 && tsDelta <= 0x3FFF && len(prev) <= 0x3FF &&
		5+len(prev)+len(cur) <= cap(dst) {
		// yedek başlık: F=1|PT, TS-off(14)|len(10)
		dst = append(dst,
			0x80|pt,
			byte(tsDelta>>6),
			byte((tsDelta&0x3F)<<2)|byte(len(prev)>>8),
			byte(len(prev)&0xFF),
		)
		dst = append(dst, pt) // birincil başlık F=0
		dst = append(dst, prev...)
		dst = append(dst, cur...)
		return dst
	}
	dst = append(dst, pt) // tek blok
	dst = append(dst, cur...)
	return dst
}
