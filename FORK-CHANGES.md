# SpeakNow LiveKit Server Fork — VP9/AV1 Simulcast + Ses Gürültü Temizliği + Ham Kayıt

`livekit/livekit` **v1.11.0** üzerine fork. Üç özellik + bir tarihi not:
1. **VP9/AV1 simulcast** (upstream'de bilerek KAPALI) + ipv6 TURN fix — aşağıdaki 6 madde. *(env'siz, hep açık)*
2. **AUDIO-NC**: SFU-içi mikrofon gürültü temizliği (DeepFilterNet3). `SN_DENOISE=1`
3. **RAWREC**: ham medyayı (ses + görüntü) SFU'nun İÇİNDE diske yazar. `SN_RAWREC=1`
4. ~~**SR-EXPORT**~~: aynı gün geri alındı — aşağıdaki nota bak.

## RAWREC (ham medya → disk, SFU'nun içinde) — 2026-08-31 / 2026-09-03

**Durum:**
- **Aşama 1 (SES) ✅ CANLI VE DOĞRULANDI** (kayıt 175, 2026-09-03):
  `capa_kaynak=sfu-sr-AtAdjusted`, çapa dosya açılışından 5,9 ms önce,
  0 düşen paket, `first_rtp` tarayıcı yolununkiyle BİREBİR aynı.
- **Aşama 2 (GÖRÜNTÜ) ✅ CANLI VE DOĞRULANDI** (kayıt 176, 2026-09-03):
  1049 kare, 85,1 sn, 0 düşen paket, ffmpeg hatasız çözüyor,
  `capa_kaynak=sfu-sr-AtAdjusted`. postprocess her iki akışta da SFU
  dosyasını seçti, tarayıcı kopyalarını eledi.
  **Kazanç:** SFU 1920×1018 yazdı, bot 1280×679 yazıyordu — bot orta
  katmanı kaydediyormuş.
- **Dynacast sabitlemesi + iki parçalı anahtar + kalıcı vazgeçmenin
  kaldırılması (`v1.11.0-rawrec11`) — TEST BEKLİYOR.**
  Kayıt 178'de iki parçalı anahtar doğrulandı (paylaşım kayıttan önce
  başlatıldı, SFU 9 sn bekleyip hedefi buldu). Aynı turda iki kusur çıktı ve
  kapatıldı: oda anahtarına GÖRELİ yol yazılıyordu (SFU ayrı container'da,
  `MkdirAll` patlıyordu) ve "bir kez logla" bayrağı SÜREÇ genelindeydi
  (ses yazıcısının hatası video yazıcısının hatasını gizledi).

Plan: `monopol/docs/split-recording/SFU-HAM-YAKALAMA-PLANI.md`

**Mimari karar (2026-09-03, kullanıcı):** SFU **asıl yol**, bot tarayıcısı
**yedek**. Tarayıcı yakalaması SİLİNMİYOR — SFU bir sebeple yazamazsa
(kodek VP9 değil, Sender Report gelmedi, Redis erişilemedi) tarayıcının
dosyası olduğu gibi devrede kalıyor ve kayıt kurtuluyor. Seçimi postprocess
yapıyor: `_sfu_asil_tarayici_yedek` (recording_service/postprocess.py) yan
JSON'daki `capa_kaynak` alanına bakıp aynı akışın tarayıcı kopyasını eliyor.

### Sorun

Monopol'ün ders kaydı ham medyayı **botun tarayıcısında** yakalıyor
(`RTCRtpScriptTransform` / `createEncodedStreams`). O kanca jitter tamponunun
ÖNÜNDE, yani WebRTC'nin kendi A/V hizalamasının da öncesinde: elimize geçen RTP
damgasının başlangıcı akış başına rastgele ve duvar saatiyle bağı yok. Dosyanın
ders eksenindeki yeri bu yüzden **tahmin** edilmek zorunda kalıyor —
`min(varış − üretim)` süzgeci — ve kalitesi o anki ağa bağlı.

Ölçüldü (2026-08-30, üç ardışık kayıt), paylaşım görüntüsünde gecikme yayılımı:

| kayıt | yayılım | sonuç |
|---|---|---|
| 20:32 | **4501 ms** | paylaşımın sesi görüntüden ~500 ms kaydı, kullanıcı duydu |
| 21:58 | 148 ms | senkron iyi |
| 22:59 | 3266 ms | — |

Aynı kod, farklı ağ, farklı sonuç.

### Neden SFU

Yayıncının RTCP Sender Report'u (RTP damgası ↔ NTP saati) **SFU dışında hiçbir
yerden görülemiyor**: LiveKit RTCP'yi sonlandırıp abonelere KENDİ saatiyle SR
üretiyor, tarayıcı API'leri de ham çifti vermiyor (`getStats` NTP yarısını,
`getSynchronizationSources` RTP yarısını veriyor; birleştirilemiyorlar).
`abs-capture-time` uzantısı da bu kurulumda pazarlık edilmiyor — ölçüldü,
`captureTime` iki ayrı API'de de `undefined`.

SFU'nun içinde ise çift zaten elimizde (`buff.GetSenderReportData()`) ve
`ExtPacket` NACK onarımlı, sıralı, 32 bit sarmadan arınmış damgayla geliyor.
Çapa **hesap değil, veri**.

### Değişiklikler

- **Yeni paket:** `pkg/sfu/rawrec/`
  - `rawrec.go` — oturum/kontrol/yazıcı ömrü
  - `ogg.go` — Ogg/Opus yazıcı, `recording_service/app.py` içindeki Python
    `_OggOpusWriter`'ın **bayt düzeyinde uyumlu** karşılığı. İki incelik:
    **pre-skip 0** (granül doğrudan paketin çalma zamanı olsun diye) ve
    **Ogg CRC yansıtmasız** (polinom 0x04c11db7; `hash/crc32` ile yazılsaydı
    sessizce bozuk dosya çıkardı).
  - `video.go` — **Aşama 2**: RTP paketlerinden KARE TOPLAMA + IVF yazıcı
    kurulumu. Seste "bir paket = bir kare" olduğu için bu iş yoktu.
  - `ivf.go` — IVF yazıcı, `app.py._ivf_file_header` + kare kaydının bayt
    düzeyinde uyumlu karşılığı (32 baytlık başlık, kare kaydı `<IQ`,
    kapanışta kare sayısı 24. bayta geri yazılır).
- **Kanca:** `pkg/sfu/receiver_base.go` → `forwardRTP`, AUDIO-NC'nin hemen
  yanında, `SN_RAWREC=1` iken.
  - ses: `layer == 0` + ses mime'ı.
  - görüntü: `layer == r.rawrecÜstKatman()` + video mime'ı. Paket yazımı
    payload-tipi kontrolünün **altında** (eşleşmeyen paket kareyi bozar);
    seste üstünde.
- **Yazılan:**
  - ses → `<dir>/audioraw_<recid>/01_sfu_<track>.opus` (MICROPHONE) ya da
    `shareaudio_<recid>/…` (SCREEN_SHARE_AUDIO)
  - görüntü → `<dir>/camraw_<recid>/01_sfu_<track>.ivf` (CAMERA) ya da
    `share_<recid>/…` (SCREEN_SHARE)
  - her birinin yanında aynı kökle `.json` çapa dosyası.

  Klasör adları bugünkü tarayıcı yolunun ürettiğiyle AYNI — postprocess
  DEĞİŞMİYOR. Dosya adındaki **`sfu_` öneki ŞART**: tarayıcı aynı klasöre
  kendi track kimliğiyle yazıyor, önek olmasa iki yazıcı aynı dosyaya
  denk gelip birbirini ezebilirdi.

### Aşama 2'nin üç kuralı (görüntü)

1. **Yalnız üst simulcast katmanı.** Alt katmanlar da SFU'ya geliyor; hepsini
   tek dosyaya yazmak dosyayı bozar (aynı damgada farklı çözünürlükte
   kareler). Katmanı `rawrecÜstKatman()` seçiyor:
   `buffer.GetSpatialLayerForVideoQuality(..., VideoQuality_HIGH, trackInfo)`,
   SVC'de 0. **Yan fayda:** bot bugün SFU'nun kendisine ilettiği katmanı
   yazıyor ve tıkanıklıkta katman değişince damgalar geri gidebiliyor
   (ölçüldü, kayıt 114: 1079 karede 7 kez). Tek katmana sabitlemek o sınıf
   bozulmayı kaldırıyor.
2. **İlk anahtar kareden başla.** Dosyanın ilk karesi anahtar kare değilse
   çözücü hiçbir şey çözemez ve postprocess'in kare taraması sıfır kare
   bulur — kayıt sessizce görüntüsüz çıkar.
3. **Yalnız VP9.** IVF `VP90` etiketiyle yazılıyor; başka kodek bu etiketle
   geçerli GÖRÜNÜR ama çözülemez (kayıt 124: H.264, kayıt 158: AV1 — ikisi de
   görüntüsüz çıktı). VP9 değilse yazıcı hiç kurulmuyor, tarayıcı yedeği
   devrede kalıyor. Proje bugün her iki akışta da VP9 yayınlıyor
   (`classroom-media-config.js` → `VIDEO_CODEC = 'vp9'`).

### Üst katmanı uyanık tutma (dynacast pin) — 2026-09-03

Kayıt 176'da SFU'nun üst-katman dosyasında **ilk 8,27 saniye tek donuk kare**
çıktı. Kodda hata yok: dynacast izlenmeyen simulcast katmanının encoder'ını
durduruyor, üst katman ancak bir abone HIGH isteyince uyanıyor ve SFU
gönderilmeyen kareyi yazamıyor. Aynı sınıf ölçüm daha önce de vardı —
kayıt 114'te ilk **20,4 saniye** 960×539 gelmişti
(`classroom-livekit-integration.js:799`).

Bot HIGH istiyor, ama abone olup isteği iletene kadar zaman geçiyor.
**Kayıt sunucuda başladığına göre isteği de sunucu yapsın:**

```go
// pkg/rtc/mediatrack.go → OnSetupReceiver
if ti.Type == livekit.TrackType_VIDEO && rawrec.PinHigh() {
    t.dynacastManager.NotifySubscriberMaxQuality(
        rawrec.SanalAboneID, mime, livekit.VideoQuality_HIGH)
}
```

`rawrec.SanalAboneID` (`PA_SN_RAWREC`) gerçek bir katılımcı değil — yalnız
`maxSubscriberQuality` haritasında bir satır; kimseye paket gitmiyor.
Track alıcısı kurulur kurulmaz konuyor, yani bota abone olmasını beklemek yok.

Kapatma: `SN_RAWREC_PIN_HIGH=0`. Bedeli, kayıt olmayan derste de üst katmanın
açık kalması (yayıncı boşuna encode eder).

**Kare toplama:** VP9 payload descriptor'ındaki `B` (kare başı) ve `E` (kare
sonu) bitleri. Kare = B'den E'ye kadarki paketlerin, descriptor'ları
`codecs.VP9Packet.Unmarshal` ile soyulmuş yüklerinin ardışık birleşimi.
Damga ortada değişirse yarım kare ATILIR (çözülemez kare yazmaktansa eksik
bitirmek doğru). `ExtPacket.Payload` KULLANILAMIYOR: DependencyDescriptor
varken LiveKit onu doldurmuyor (`buffer_base.go`, `case mime.MimeTypeVP9`).

### Kontrol: neden oda adı değil track SID

Kanca noktasında oda adı YOK; eklemek `MediaTrackParams` ile
`ReceiverBaseParams`'ı birlikte değiştirmek demekti. Gerek kalmadı — uygulama
`track-published` webhook'unda hem odayı hem SID'i alıyor ve anahtarı SID'le
yazıyor. **LiveKit yapılarında tek bir yeni alan yok.**

```
sn:rawrec:track:<sid>   {"room": "<oda>"}
                        yazan: monopol/livekit_recording_webhooks.py (track-published)
sn:rawrec:room:<oda>    {"recording_id": 165, "dir": "/abs/…/videos/2026-08-30"}
                        yazan: recording_service/recording_engine.py (kayıt başlarken)
```

**Neden iki parça (2026-09-03, kayıt 177).** Tek anahtar webhook'ta yazılıyor
ve webhook DB'de kayıt satırı arıyordu. Öğretmen paylaşımı kayıttan 10 sn
ÖNCE başlatınca satır henüz yoktu → anahtar hiç yazılmadı, bir daha da
denenmedi → SFU 60 sn bekleyip vazgeçti ve birinci paylaşımı kaçırdı.
Artık iki taraf da kendi bildiğini yazıyor ve SIRA ÖNEMSİZ. Webhook DB'ye
hiç bakmadığı için "kayıt satırı 17 sn sonra doğuyor" sorunu da düştü.

**Yarış ve çözümü:** yazıcı anahtar gelene kadar ilk paketleri BEKLETİYOR
(`SN_RAWREC_WAIT`, vars. 60 sn). ⚠ Süre dolunca **VAZGEÇMİYOR** — yalnız
tamponu bırakıyor (bellek için) ve 2 saniyede bir aramaya devam ediyor.
Kalıcı vazgeçme kayıt 177'de bir paylaşımın tamamını kaybettirdi: warm
transceiver yüzünden video track'i ders boyunca aynı kaldığı için o yazıcı
bir daha hiç uyanmadı. Video tarafında hedef bulunur bulunmaz `SendPLI(true)`
ile anahtar kare isteniyor (ekran paylaşımında anahtar kare kendiliğinden
dakikalarca gelmeyebilir). Tarayıcı kancasındaki `HOLD_MAX_FRAMES` ile aynı gerekçe —
orada da şart olduğu ölçülerek öğrenilmişti.

### Medya yolu güvenliği (pazarlıksız)

`Write` ASLA bloklamaz: tamponlu kanala (2048) non-blocking yazar, doluysa
paketi DÜŞÜRÜR. Disk I/O ayrı goroutine'de. Her hata yutulur, ilki bir kez
loglanır. `Close` yalnız akış bittikten sonra bekliyor, yani medya yolunu
tutmuyor. AUDIO-NC ile aynı disiplin.

### Env

    SN_RAWREC=1                ana anahtar (yoksa kod yoluna hiç girilmez)
    SN_RAWREC_REDIS=localhost:6379
    SN_RAWREC_REDIS_DB=0
    SN_RAWREC_REDIS_PASSWORD=
    SN_RAWREC_PIN_HIGH=1       üst simulcast katmanını uyanık tut (vars. açık)
    SN_RAWREC_WAIT=60          kontrol anahtarı için bekleme (sn)
                               ⚠ 10 DEĞİL: ölçüldü ki kayıt satırı
                               track yayınlandıktan ~17 sn sonra doğuyor.
    SN_RAWREC_KEYFRAME_MAX_KARE=24   iki anahtar kare arası EN ÇOK kare
                               (arama gecikmesi tavanı — asıl knob)
    SN_RAWREC_KEYFRAME_MIN_SN=2      iki anahtar kare arası EN AZ süre
                               (maliyet tavanı; tam hareketli içerikte
                                kare bütçesi saniyede bir isteyebilir)
    SN_RAWREC_KEYFRAME_MAX_SN=30     seyrek kapsama tabanı (ekran donsa bile)

### Bilinen açık uçlar

**Görüntü tampon tavanı:** hedef (kontrol anahtarı) gelene kadar paketler
bellekte bekliyor. Seste 60 sn ≈ 270 KB, görüntüde saniyede ~2 Mbit —
aynı süre tutulamaz. Üst sınır 10 000 paket (~12 MB) ve aşılınca en eski
paketler atılıyor; dosya nasılsa ilk ANAHTAR KAREDEN başlıyor.

**Ses paket süresi** şimdilik sabit **960 örnek (20 ms)** varsayılıyor. Ölçüldü ki bu
akışlarda Opus config 13/15/31'in üçü de 20 ms — ama kodlayıcı 10/40/60 ms'e
geçerse granül kayar. TOC baytındaki `config` alanı süreyi doğrudan veriyor;
Aşama 4'te oradan okunacak.

## SR-EXPORT — DENENDİ VE GERİ ALINDI (2026-08-31, aynı gün)

Kısa ömürlü bir yamaydı: yayıncının RTCP Sender Report'larını
(`pkg/rtc/mediatrack.go` → `case *rtcp.SenderReport`) Redis'e aktarıyordu, ki
Monopol'ün ders kaydı ses/görüntü hizalamasını tahmin yerine ölçümle yapsın.
Çalıştı, imajı da derlendi (`v1.11.0-srexport1`).

**Neden geri alındı:** aynı gün karar değişti — ham medya yakalaması botun
tarayıcısından **SFU'nun içine** taşınacak. O olunca Sender Report zaten aynı
süreçte olacak (`buff.GetSenderReportData()`) ve Redis turu anlamsız kalıyor.
Kenarda çalışmayan kod bırakmamak için söküldü.

**Kaybolmaması gereken bilgi** (SFU ham yazıcısında birebir kullanılacak):

- SR çifti burada ayrıştırılıyor: `pkg/rtc/mediatrack.go`, `rtcpReader.OnPacket`
  içindeki `case *rtcp.SenderReport` — `pkt.RTPTime` (32 bit) + `pkt.NTPTime`
  (64 bit NTP: üst 32 saniye, alt 32 kesir).
- NTP → Unix saniye:
  `(ntp>>32) − 2208988800 + (ntp & 0xFFFFFFFF) / 2^32`
- Dosyanın ders eksenindeki çapası:
  `anchor_unix = ntp_unix − (first_rtp − sr_rtp) / clock_rate`
  (`first_rtp` = ham dosyanın ilk paketinin RTP damgası; fark 32 bit **işaretli**
  hesaplanmalı, sarma var.)
- `RTCPSenderReportState.At` (SFU'nun alış anı) farklı katılımcıların saatleri
  tutmadığında ORTAK eksen veriyor; `NtpTimestamp` ise aynı yayıncının tüm
  track'leri için kesin.
- Medya yolu kuralı (yeni yazıcı için de geçerli): `forwardRTP` / RTCP okuyucusu
  ASLA bloklanmaz — tamponlu kanal + doluysa düşür, her hata yutulur.

Plan: `monopol/docs/split-recording/SFU-HAM-YAKALAMA-PLANI.md`

## AUDIO-NC (SFU-içi ses gürültü temizliği) — 2026-07-16

Mikrofon Opus/RED paketini SFU'nun içinde, yayıncı-track başına TEK noktada
(forwardRTP, abonelere dağıtımdan önce) çöz→DFN3→yeniden-kodla→payload'ı yerinde
değiştir. Ayrı bot mimarisinin ~195ms ek gecikmesine karşılık **ölçülen ~23ms**
(prod, izole test: baz 231ms → denoise 254ms). Gürültü bastırma ~64dB (debug).

- **Yeni paket:** `pkg/sfu/audiodenoise/` — `native.go` (purego ile libdf+libopus
  dlopen, cgo YOK → CGO_ENABLED=0 statik build korunur), `processor.go` (per-track
  decode/DFN/encode, gecikme-öncelikli: sıra penceresi yok+PLC+geç-kare-düşür, her
  yol fail-open), `red.go` (RFC2198 ayrıştır/yeniden-kur).
- **Hook:** `pkg/sfu/receiver_base.go` `forwardRTP` — yalnız MİKROFON opus/red (ekran
  sesi hariç), `SN_DENOISE=1` iken; processor goroutine-local (kilitsiz).
- **Dockerfile:** 3 aşama (rust→libdf.so, go→binary, debian-slim+libopus+model);
  final Alpine→debian-slim (glibc libdf için). `go.mod`: purego v0.8.2.
- **Env:** `SN_DENOISE=1` (ana anahtar; yoksa kod path'i hiç girilmez, düz fork gibi),
  `SN_DENOISE_ATTEN_DB` (bastırma tavanı, 100=tam; daha doğal için 30-50).
- **Image:** `ghcr.io/speaknow06/livekit-server:v1.11.0-audionc1`.
- **Doğrulama (prod izole test):** native yükleme OK, mikrofon audio/opus+red
  işleniyor, DFN3 konuşma korur+gürültü siler, ~23ms ek gecikme, çift-ses riski YOK
  (ayrı track yok). AÇIK: gerçek insan sesiyle canlı test + atten_lim ayarı.

---

## VP9/AV1 Simulcast — minimal fork (5 dosya, ~35 satır)
Amaç: **VP9 simulcast** desteği (upstream'de bilerek KAPALI — LiveKit VP9'u SVC sayar,
`simulcast` alanını yok sayar) + ipv6 TURN fix.

- **Branch:** `speaknow-vp9-simulcast`
- **Base:** v1.11.0 (`8ccad68`, Release v1.11.0 #4459)
- **Image:** `ghcr.io/speaknow06/livekit-server:v1.11.0-vp9simulcast-fix3` — PROD'DA CANLI, ÇALIŞIYOR (2026-06-13 doğrulandı)

## Neden?
İhtiyaç: classroom ekran paylaşımında **text/screen-content + per-viewer çözünürlük adaptasyonu
(fps sabit)**. VP9 SVC bunu vermiyor (text spatial'ı çökertir + Android software decode). VP9
**simulcast** (her rid bağımsız stream) veriyor ama upstream desteklemiyor → 5 yer yamandı.
"Per-viewer bağımsız": bir öğrenci takılırken diğeri akmaya devam eder.

## Değişiklikler (net diff: `git diff 8ccad68 HEAD`)

### 1. `pkg/service/roommanager.go` — ipv6 TURN fix  (HER rebuild'de ŞART)
Düz v1.11.0 TURN URL'sine IPv6'yı köşeli-ayraçsız basıyor (`ToStringSlice()`) → client
`setConfiguration` syntax error → hiçbir client bağlanamaz. Fix: TURN URL yalnız `NodeIP.V4`.

### 2. `pkg/sfu/buffer/dependencydescriptorparser.go` — DD parser drop eşiği
`structureExtFrameNum` HER keyframe'de ilerliyordu (yapı/StructureId değişmese bile). text/
screen-content (sık keyframe) akışında geç/retransmit gelen GEÇERLİ kareler "earlier than current
structure" diye atılıyordu → donma. **Fix:** yeni alan `structureChangeExtFrameNum`; drop eşiği
YALNIZ StructureId gerçekten değişince ilerler. `ExtKeyFrameNum` (DD selector kullanır) dokunulmadı.

### 3. `pkg/sfu/forwarder.go` — VP9 simulcast: skipReferenceTS + Simulcast selector
- **(a) `skipReferenceTS = true`** (VP9 simulcast dalında): VP9 simulcast'in per-katman RTCP SR'ları
  güvenilmez → cross-layer timestamp offset (`getRefLayerRTPTimestamp`) hatalı/0 → layer switch
  "switch point too far behind" → NACK storm/çöküş. LiveKit'in `ONE_SPATIAL_LAYER_PER_STREAM_INCOMPLETE_RTCP_SR`
  için tasarladığı kaçış kapısı: SR-offset atlanır, elapsed-time (`extExpectedTS`) kullanılır.
- **(b) Simulcast selector** (DD selector yerine, ilk-kurulumda): DD selector VP9 simulcast'i (ayrı
  stream'ler) yanlış yönetip neredeyse her frame'de "switch" raporluyordu. Simulcast selector ayrı-stream
  modelini doğru kullanır (keyframe'de switch).
- **(c) AV1 simulcast (2026-06-15):** Aynı dosyada `case mime.MimeTypeAV1` simulcast dalına da
  `f.skipReferenceTS = true` eklendi (VP9 ile birebir gerekçe — per-katman RTCP SR güvenilmez). AV1
  case'i Simulcast selector'u zaten stock'ta kullanıyordu; #4 (rid→spatial) ve #2 (DD parser drop
  eşiği) codec-agnostik olduğu için AV1'e otomatik uygulanıyor → **tek eksik buydu.** Prod'da AV1
  simulcast (contentHint='text' dahil) test edildi, çalışıyor. (Client AV1'i `isSVCCodec` ile zaten
  destekliyor; ayrı client değişikliği gerekmedi.)

### 4. `pkg/sfu/receiver_base.go` — VP9 simulcast rid→spatial layer eşlemesi  (ASIL KÖK)
`if extPkt.Spatial >= 0 { spatialLayer = extPkt.Spatial }` — SVC için spatial'ı paketten (DD SID)
alıyordu. VP9 simulcast'te her rid tek-spatial → DD SID **HEP 0** → TÜM rid'ler spatial 0'a çöker →
forwarder rid'leri ayırt edemez, `lastSSRC != SSRC` her pakette → switch storm → `extNextTS =
extLastTS+1` frozen → donma (ilk layer-switch'te, ~68. sn). H.264/VP8'de `Spatial = -1` olduğu için
bu override hiç fırmıyordu (onlar bu yüzden çalışıyor). **Fix:** override yalnız `layer == 0` (SVC,
tek uptrack); simulcast rid≥1'de rid index (`layer`) authoritative.

### 5. `Dockerfile` — hızlı cross-compile
`FROM --platform=$BUILDPLATFORM` → native cross-compile (amd64), QEMU emülasyon yok (~1 dk build).

### 6. `pkg/rtc/dynacast/*` — dynacast EXACT-MATCH · ⛔ **GERİ ALINDI (2026-09-04)**

> **BU ÖZELLİK ARTIK YOK.** `dynacastmanagervideo.go` upstream davranışına
> döndürüldü: `Enabled: q <= max`. Aşağıdaki anlatım TARİHSEL kayıt olarak
> duruyor — neden yapıldığını ve neden geri alındığını birlikte görmek için.
> Geri alma gerekçesi: **§7 Neden geri alındı**.
>
> **TEMİZLİK TAMAM (2026-09-04):** `pkg/rtc/dynacast/` tamamen upstream'e
> döndürüldü (`git checkout e6f143ba^ -- pkg/rtc/dynacast/`) ve
> `mediatrack.go`'daki `IsMultiLayer` kapaması kaldırıldı. Ölü kod
> (`qualitySet`, `subscribedQualities`, `IsMultiLayer`) KALMADI.
> `dynacastmanager_test.go` da upstream hâline döndü ve **geçiyor**
> (`go test ./pkg/rtc/dynacast/...` → ok, 1,5 sn).

**İstek:** Google Meet gibi — tek izleyici 1080 (HIGH) izliyorsa yayıncı (öğretmen) **yalnız
1080** encode etsin; 720/540 **dynacast ile paused**. Upstream dynacast bunu yapmaz: tek bir
**max** abone kalitesi tutar ve **max'in ALTINDAki her katmanı da** yayınlatır (`Enabled: q <= max`,
`dynacastmanagervideo.go`) — alt katmanı "sıcak" tutup anında düşüş içindir, ama kimse izlemese de
encode edilir. Sonuç: 1 öğrenci 1080 izlerken hoca 3 katman (≈6.3M) encode ediyordu.

**Fix (set-based):** Upstream'in tek `max`'i yerine "hangi katmanların O AN abonesi var" **kümesi**
(`qualitySet` bitmask) taşınır ve **yalnız o katmanlar** yayınlatılır.
- `interfaces.go` — `qualitySet` tipi (LOW/MEDIUM/HIGH bitmask) + `dynacastQualityListener.OnUpdateMaxQualityForMime` imzasına küme eklenir.
- `dynacastqualityvideo.go` — `updateQualityChange` her abonenin **tam** kalitesini küme'ye ekler; küme değişimi (max sabit kalsa bile) bildirim tetikler.
- `dynacastmanagervideo.go` — `Enabled: q <= max` **→** `Enabled: küme.has(q)`. Debounce: küme **bit kaybı** (katman boşaldı) = downgrade (5 sn debounce); **bit kazancı** (katman gerekti) = anında (respin). Guard: küme boş ama max≠OFF (ForceQuality/regress yarışı) → upstream `q<=max`'a düşer.
- `dynacastmanager_test.go` — beklenen değerler exact-match'e güncellendi.

**Çok-izleyici güvenli:** yerel aboneler **exact** (540 izleyen + 1080 izleyen → orta katman paused,
ikisi de doğru beslenir). **Cross-node** yalnız max taşıdığından (uzak node'un kümesi gelmez)
o node'un katkısı `addUpTo(max)` ile **conservative** (tüm alt katmanlar açık) tutulur → multi-node
bozulmaz. `maxSubscribedQualities` (receiver max-expected-layer, `mediatrack.go`) **değişmedi** = tek max.

**Trade-off:** izleyici bant düşüşüyle **o an paused** alt katmana geçerken ~1-2 sn respin (keyframe).
Boş/stabil odada tetiklenmez; dolu odada alt katmanlar zaten açık. VP9 simulcast'te paused rid'i
**resume** yolu (#2 DD parser, #4 rid→spatial ile aynı makine) → canlı testte özellikle izlenmeli.

> Not: Bu değişiklik VP9 simulcast'ten bağımsızdır; H.264 webinar simulcast'i de etkiler (orada da
> izlenmeyen katman durur — webinar'ın zaten beklediği davranış, bkz `webinar-livekit.js` dynacast notu).

**DÜZELTME v2 (2026-06-14 — KAMERA REGRESYONU, ŞART):** İlk `dynacast1` image'i **tek-katmanlı
kamerayı bozdu.** Kamera tek encoding'i SDP'de rid `q`(=LOW) slotundayken, server abonenin quality'sini
**HIGH** hesaplıyor (`GetVideoQualityForSpatialLayer` lone spatial-0 layer → HIGH). Stock "max'in
altındaki HER katmanı aç" bu tutarsızlığı örtüyordu (LOW da açık → kamera yayılır). Exact-match yalnız
HIGH'ı açıp `q`/LOW encoding'i kapatınca → **kamera hiç gitmedi** (`active=false`, öğretmende sürekli
"internet kötü"/POOR göstergesi). **Fix:** exact-match YALNIZ **çok-katmanlı** track'te uygulanır —
`DynacastManagerVideoParams.IsMultiLayer` (mediatrack.go'da `len(buffer.GetVideoLayersForMimeType) > 1`
ile geçilir); tek-katman (kamera) ya da kume-bos → **stock** (`q<=max`, tek encoding asla kapanmaz).
Ekran paylaşımı (q/h/f = 3 katman) optimizasyonu aynen korunur. Regresyon testi:
`TestSubscribedMaxQualitySingleLayer`. → düzeltilmiş image **`v1.11.0-vp9simulcast-fix3-dynacast2`**
(buggy `dynacast1`'i KULLANMA).

### 7. Neden EXACT-MATCH geri alındı (2026-09-04) — **ÖLÇÜMLE**

Fikir sağlamdı: kimse alt katmanı izlemiyorsa öğretmen onu üretmesin.
Ama **ölçülmemiş bir varsayıma** dayanıyordu:

> *"Öğretmen HIGH'a gücü yetmezse kendiliğinden LOW'a düşer."*

**DÜŞEMİYOR.** Tarayıcı yalnız `active: true` olan encoding'leri üretir;
kapalı bir katmanı kendi kararıyla açamaz — o karar sunucudan gelir. Bant
genişliği HIGH'ın alt sınırının altına inince Chrome o katmanı **tamamen
duraklatır** ve düşülecek basamak kalmaz.

Ve geri bildirim yolu da yok: sorun **öğretmenin YÜKLEME** hattında olduğu
için öğrencilerde tıkanıklık oluşmuyor, kimse alt katman istemiyor,
dolayısıyla alt katman hiç açılmıyor. Durum kendiliğinden düzelmiyor.

**Ölçümler (Monopol demo sunucusu, kayıt 183 ve 184):**

| bulgu | ölçüm |
|---|---|
| paylaşım track'i ders boyunca aldığı dynacast kararı | **tek karar: `LOW- MED- HIGH+`** — üst katmana rakip yoktu |
| buna rağmen üst katmanın sustuğu süreler | **11,3 sn** ve **7,8 sn** → öğrenci DONUK EKRAN |
| kameranın üst katmanı, ekran paylaşımı yayındayken | **196 saniyenin 134'ünde ölü** (92 sn + 43 sn iki delik) |
| kameranın alt katmanı (50 kbps) aynı sürede | **kesintisiz akıyordu** — ama kapalı olduğu için kimse alamadı |
| öğretmenden istenen toplam / ölçülen kapasite | **6.672 kbps / ~2.000 kbps** (3,3 kat) |

Kamera deliklerinin başlangıç-bitişi paylaşım oturumlarıyla **birebir**
örtüştü (delik paylaşımdan 0,4-1,6 sn sonra başlıyor, 1,7-3,6 sn sonra
bitiyor) — yani öğrencinin ağıyla ilgisi yok, öğretmenin kendi bütçesinde
kamera sıranın sonundaydı.

**Upstream davranışının üstünlüğü:** alt katman ZATEN kodlanıyor. Üst katman
susar susmaz SFU elindeki hazır kareleri iletmeye başlıyor — açma + kodlama
+ anahtar kare beklemesi yok, geçiş **anlık**. Bedeli, herkesin üst katmanı
izlediği durumda alt katmanın boşa gitmesi; bu bilinçli bir **sigorta primi**.

**Birlikte giden istemci değişikliği** (`monopol/static/js/classroom-media-config.js`):
kamera `priority: 'low' → 'medium'` (paylaşımla eşit; WebRTC eşit öncelikte
her akışın asgarisini önce garanti ediyor, kamera artık sıfıra düşmüyor) ve
kamera alt katmanı `240×180/50k → 320×240/100k` (artık yalnız telefon
sığınağı değil, kameranın düşeceği basamak).

### 8. `pkg/sfu/rawrec` — ham yazıcı KATMAN TAKİBİ (2026-09-04)

Kayıt yazıcısı **sabit** bir simulcast katmanına (`rawrecÜstKatman()`)
bağlıydı. O katman ders ortasında susunca dosyaya hiçbir şey yazılmıyordu —
alt katman akmasına rağmen (kayıt 184: kamera 196 sn'nin 134'ünde kayda
girmedi).

**Yeni yapı:**

- Yazıcı **track başına tek** (`ReceiverBase.rawVideo`), katman başına değil.
  Bütün katmanların `forwardRTP`'si aynı yazıcıya yazıyor.
- Yazıcı o an **canlı olan en üst** katmanı takip ediyor
  (`enİyiCanlıKatman`). Eşikler: canlı sayılma 1 sn, alta düşme 3 sn, üste
  geri çıkma 2 sn kesintisiz canlılık (çırpınma önlemi).
- Katman değişince **YENİ DOSYA** açılıyor (`02_sfu_…`, `03_sfu_…`).
  Aynı dosyaya devam EDİLMİYOR: simulcast katmanlarının RTP damga tabanı
  ortak olmak zorunda değil (ölçüldü, kayıt 182: paylaşımda iki katmanın
  tabanı arasında saatlerce fark vardı). Ayrı dosya sorunsuz — her dosya
  ders eksenindeki yerini KENDİ RTCP çapasından alıyor.
- **Süzme kaynakta:** aktif olmayan katmanın paketi kuyruğa hiç girmiyor
  (`Write` içinde atomik `aktif` karşılaştırması). Yoksa 2048'lik kuyruk üç
  kat hızlı dolar ve hedef aranırken biriken tampon karışık katman içerirdi.
  Canlılık damgaları (`katmanSonNs`) her katman için atomik yazılıyor.
- **PLI ve Sender Report katman başına** (`KatmanKaydet` / `kaynak(katman)`):
  yeni katmanda anahtar kareyi ondan istiyoruz, çapayı da ondan alıyoruz.

### 9. `pkg/sfu/rawrec` — RED yedeklerinden kayıp kurtarma (2026-09-04)

`redBirincil()` RFC 2198 yükünden yalnız **birincil** bloğu alıp yedekleri
atlıyordu. Sonuç ölçüldü (kayıt 183): SFU ham ses dosyası 5.789 paket, bot
tarayıcısının kopyası 6.446 — **SFU %11 daha az**. Sebep: öğretmen→SFU
yolunda kaybolan paketin kopyası bir sonraki paketin içinde geliyor; SFU onu
abonelere iletiyor, botun tarayıcısı kurtarıp yazıyor, bizim yazıcımız çöpe
atıyordu. **Veri SFU'daydı, kullanmıyorduk.**

`redBloklar()` yükü parçalarına ayırıyor (yedekler eskiden yeniye, sonra
birincil); yazıcı yedek bloğu **yalnız delik varsa** yazıyor. Doğrulandı
(kayıt 184): kayıp 9 paketin 8'i kurtarıldı, SFU artık tarayıcıdan eksik
değil (mikrofonda SFU 2186 / tarayıcı 2082).

### 10. `pkg/sfu/rawrec/saglik.go` — yazıcının kendi sağlık raporu (2026-09-04)

Postprocess, SFU dosyasını **bot tarayıcısının kopyasıyla karşılaştırıp**
"kare sayısının %90'ını tutuyor mu" diye bakıyordu. İki sorunu vardı:
kaynağı türeviyle ölçüyordu (görüntüde zaten farklı katmanlar), ve bıçak
sırtıydı — kayıt 183'te SFU %89,8 çıkıp eşiği 0,2 puanla kaçırdı, bozuk
tarayıcı dosyasına düşüldü ve 2. paylaşımın sesi kayda hiç girmedi.

Artık yazıcı kendi durumunu yan JSON'a yazıyor (`saglik` bloğu): bağlı
kaldığı süre, yazdığı aralık, kareler arası en büyük boşluk, tampon
düşürüldü mü, RTP sıra numarasından kayıp paket sayısı, RED'den kurtarılan.
Postprocess yalnız buna bakıyor; karşılaştırma tamamen kaldırıldı.

### 11. `pkg/sfu/rawrec/sira.go` — paketleri SIRA NUMARASINA göre birleştir (2026-09-04)

**Ölçülen arıza (kayıt 185).** Ekran paylaşımının SFU dosyasında 1556 karenin
**3'ü çözülemez** çıktı. Chrome bozuk kareyi atlamıyor, çözücüyü komple
kapatıyor:

```
error → PIPELINE_ERROR_DECODE: video decode error!
sonra: seeking=true, readyState=1 — KALICI
```

Kullanıcı bunu *"ikinci paylaşımın görüntüsü tamamen donuk"* diye bildirdi.
Oysa donma **birinci** paylaşımın 42,77. saniyesinde başlıyordu ve ikinci
paylaşımın 915 karesi dosyada sapasağlam duruyordu — sadece çözücü ölmüştü.
Gerçek izleme sayfasında Playwright ile birebir tekrarlandı.

| dosya | kare | çözülemeyen |
|---|---|---|
| **SFU paylaşım** | 1556 | **3** |
| tarayıcı kopyası (AYNI track) | 1568 | 0 |
| SFU kamera | 2778 | 0 |
| tarayıcı kamera | 3264 | 0 |

**Kök neden.** 1080p bir paylaşım karesi ~40 RTP paketine bölünüyor.
Yayıncı→SFU arasında bir paket düşünce LiveKit NACK atıyor, yayıncı **tekrar
gönderiyor** — ama bir gidiş-dönüş sonra, kare çoktan kapanmış oluyor. `işle`
parçaları **geliş sırasına** göre yapıştırıyordu: ortasında delik olan,
başlığı geçerli görünen bir kare yazılıyordu.

Tarayıcıda olmuyor çünkü WebRTC'nin jitter tamponu geç geleni yerine koyuyor.
Bizim kanca (`forwardRTP`) jitter tamponundan **önce** duruyor. Yani kaynak
akış hiç bozuk değildi; **birleştirme** hatalıydı.

Sağlık raporu da göremedi: paketlerin hepsi geldiği için `paket_kayip: 0`
yazıyordu. Ölçtüğü şey **kayıp**tı, **kare bütünlüğü** değil.
(15260 paket, 0 kayıp, buna karşılık 3 bozuk + 12 eksik kare.)

**Düzeltme — iki kapı:**

1. **Sıralama penceresi** (`sira.go`). Paketler kare toplayıcıya geliş
   sırasıyla değil, kısa bir pencerede (`videoSiraBekleme = 300 ms`,
   `videoSiraKapasite = 512`) sıra numarası düzeninde giriyor. Geç gelen RTX
   yerine oturuyor. ⚠ Jitter buffer DEĞİL: oynatma saati, gecikme kestirimi,
   kayıp onarımı yok. Gecikme yalnız kayıt yazıcısında; canlı yayına etkisi
   sıfır (rawrec pasif bir dinleyici).
2. **Bütünlük kapısı** (`video.go` → `işle`). Pencere kapandıktan sonra hâlâ
   eksik paket varsa, o kare dosyaya **hiç yazılmıyor** — atılıyor ve
   sayılıyor. Bozuk kare üretmek artık yapısal olarak mümkün değil. Maliyeti:
   bir kare eksilir, bir sonraki anahtar kareye kadar (bütçe gereği ~1-2 sn)
   görüntü hafif bozulur; donma olmaz.

**Sağlık raporuna eklenenler** (`saglik.go`): `kare_yazilan`, `kare_atilan`,
`kare_atilan_oran`, `sira_boslugu`, `gec_paket`. Atılan kare oranı
`kareButunlukEsigi = 0,02`'yi aşarsa dosya `durum: "eksik"` bildiriyor.

**Monopol tarafı** (`recording_service/postprocess.py`): `_ivf_onar` —
çözülemeyen kare taşıyan bir IVF **elenmiyor, onarılıyor**; yalnız o kareler
çıkarılıp remux ediliyor. Eski kayıtlar da böylece kurtarılabiliyor. Tarama
her dosyada yapılmıyor: yeni yazıcı `saglik.kare_atilan` alanını yazdığı için
o alan varsa ve sıfırsa atlanıyor (tam çözme ~21× gerçek zaman).

**Doğrulama (kayıt 185, yeniden işlendi):** 3 kare çıkarıldı (1556 → 1553),
dosya `ffmpeg` ile temiz çözülüyor, ve gerçek tarayıcıda `error: null` —
video 42,77'yi geçiyor, 2. paylaşıma 31 ms'de atlıyor, donma yok.

### 12. `pkg/sfu/rawrec` — katman geçişi CANLI YOL gibi, tek dosya (2026-09-04)

**Ölçülen arıza (kayıt 186).** Ekran paylaşımı 5, kamera 7 parçaya bölündü;
oynatıcıda paylaşımın ilk 5 saniyesi dışında **53 saniye beyaz tahta** çıktı.

Tarayıcı kopyası (= SFU'nun ilettiği) ile karşılaştırma:

| ders sn | tarayıcı kopyası | rawrec |
|---|---|---|
| 15,5–20,5 | 1080p 68 kare | 70 kare ✅ |
| 20,5–25,5 | 540p'ye düştü (gerçek dip) | alt katmana geçti ✅ |
| **25,5–65,5** | **1080p ~15 fps, ~570 kare** | **3 kare** ❌ |

**Kök neden.** rawrec'te "istenen katman" ile "yazılan katman" TEK değişkendi
(`aktif`). Karar verilir verilmez değişiyordu ve `Write` şunu yapıyordu:

```go
if katman != w.aktif.Load() { return }   // eski katman ARTIK KABUL EDİLMİYOR
```

t=23,8'de üst katman 3 sn sustu → alt katmana geçildi. t=27,8'de üst katman
canlandı → üste geçme kararı verildi, PLI atıldı, **anahtar kare 38 saniye
gelmedi**. O 38 saniye boyunca alt katman kesintisiz 15 fps akıyordu ama
artık kabul edilmiyordu. Sonuç: kaynak da vardı, kayıt da boştu.

**Canlı yol bu tuzağa düşmüyor** — `videolayerselector/simulcast.go`:

```go
if s.currentLayer.Spatial != s.targetLayer.Spatial {
    //   1. Opportunistic layer upgrade - needs a key frame
    //   2. Need to downgrade - needs a key frame
    if extPkt.IsKeyFrame { currentLayer.Spatial = layer }
}
result.IsSelected = layer == s.currentLayer.Spatial
```

`targetLayer` = istenen, `currentLayer` = gönderilen. Hedefin anahtar karesi
gelene kadar ESKİ katman gönderilmeye devam ediyor; öğrenci hiç boşluk görmez.

**Düzeltme — aynı tasarım rawrec'e taşındı:**

1. `aktif atomic.Int32` → **`suAnki`** (yazılan) + **`hedef`** (istenen).
   `Write` ikisini de kuyruğa alıyor.
2. `hedefBelirle(katman)` yalnız hedefi kurar ve PLI atar. Geçişin kendisi
   `katmanaGeç`te, **yalnız hedefin anahtar karesinin İLK paketinde**
   (`anahtarKareBaşlangıcı`: VP9 B biti + anahtar kare bayrağı).
   Hedefin paketleri toplanmıyor, yalnız yoklanıyor — katmanların sıra
   numarası uzayları ayrı olduğu için tek sıra tamponu ikisini düzenleyemezdi.
3. **Geçiş artık dosya kapatmıyor.** Damga `bölümTaban`/`bölümİlkRTP` ile
   yeniden tabanlanıyor: yeni bölümün ilk karesi öncekinin hemen ardına
   konuyor, aradaki boşluk VARIŞ SAATİNDEN ölçülüyor (geçiş anahtar karede
   olduğu için zaten bir kare aralığı kadar).
4. **Sender Report dosyayı AÇAN katmandan** (`dosyaKatmanı`). Eskiden geçişte
   `sonSR` sıfırlanmıyordu ve yeni dosya eski katmanın SR'siyle çapa
   hesaplıyordu — kayıt 185 `cam_2` 4 sa 20 dk, kayıt 186 `share_04` 2,5 sa
   sapma. Tek dosyada bu sınıf hata kökten kalkıyor.
5. `saglik`: paket sayaçları katman başına tutulup dosya başına toplanıyor
   (`katmanDegisti` → `beklenenBirikmis`/`alinanBirikmis`). Katmanların sıra
   numarası uzayları ayrı olduğu için tek sayaçta toplamak kayıp ölçümünü
   saçmalatırdı.

**Kapsam dışı bırakılanlar (kullanıcı kararı):** ısrarlı PLI, düşme eşiğinin
3 sn → 10 sn yükseltilmesi, postprocess'te sağlığın track başına
değerlendirilmesi. Madde 2 ile "veri akarken kayıt boş kalıyor" durumu zaten
yapısal olarak imkânsız hale geldiği için bunlar kalite iyileştirmesi.

### 13. `pkg/sfu/rawrec/video.go` — ELDEKİ KATMANDAN BAŞLA (2026-09-04, kayıt 188)

§12 katman geçişini dosyanın ORTASINDA düzeltti; dosyanın BAŞI hâlâ üst
katmana kilitliydi. Kayıt 188 bunun bedelini gösterdi:

```
tarayıcı kopyası   0,000 sn   960x539     ← alt katman baştan akıyor
                   6,294 sn   960 -> 1920 ← üst katman ancak burada açılıyor
rawrec ilk paket   6,34 sn                ← üst katmana kilitli, o ana kadar HİÇBİR ŞEY
```

Yayıncı (Chrome) paylaşımı açtığında önce yalnız alt katmanı gönderiyor;
üst katmanı kodlayıcı/BWE ısınınca açıyor. `Write` yalnız `suAnki`/`hedef`
katmanını kuyruğa aldığı ve ikisi de baştan üst katman olduğu için
paylaşımın ilk 6,29 saniyesi kayda hiç girmedi. `saglik` bunu göremiyor:
"yazılmayan katman" diye bir kavramı yok, raporu `durum: tam` diyordu.

**Değişiklik (üç nokta):**

1. `NewVideoWriter`: `suAnki = katmanBelirsiz (-1)`, `hedef = üstKatman`.
   Artık "hangi katmanı yazacağıma karar vermedim" diye bir başlangıç hâli
   var.
2. `Write`: `su < 0` iken HİÇBİR katman süzülmüyor — hangisi önce gelirse
   gelsin kuyruğa giriyor.
3. Döngü, ilk pakette `suAnki = p.katman` yapıyor (üst katman DEĞİL, ne
   geldiyse o) ve hedefi üst katmanda bırakıyor. İkisi farklıysa üst
   katmana bir PLI atılıyor ve §12'nin geçiş yolu üst katmanın ilk anahtar
   karesinde devreye giriyor — dosya kapanmadan, damga `bölümTaban` ile
   sürerek.

Sonuç: paylaşımın başı 540p da olsa KAYDEDİLİYOR, birkaç saniye sonra
1080p'ye yükseliyor. Eskiden o saniyeler yoktu.

Yan not: `gofmt` `rawrec.go`da eski bir girinti sapmasını buldu, düzeltildi
(yalnız boşluk).

### 14. `pkg/sfu/rawrec/video.go` — AŞAĞI GEÇİŞ CANLI YOL GİBİ (2026-09-04, kayıt 191)

§12 ve §13 yukarı yönü ve dosya başını çözdü. Kayıt 191 (macOS Network Link
Conditioner ile uplink 1500/1000/700 kbps, kayıpsız) **yedi katman geçişi**
üretti ve aşağı yönde bir maliyet ortaya çıktı:

```
BİZİM KAYIT                      TARAYICI KOPYASI (canlı yol)
27.98 -> 32.00  4,01 sn DELİK    27.909  1920 -> 960  boşluk YOK
58.17 -> 61.66  3,50 sn DELİK    54.009  1920 -> 960  boşluk YOK
88.41 -> 92.49  4,07 sn DELİK    81.107  1920 -> 960  boşluk YOK
```

⚠ **BU OKUMA YANLIŞTI, sonradan düzeltildi.** Tarayıcı kopyasındaki
"boşluksuz" geçiş bir yanılsama: LiveKit forwarder'ı katman değişiminde RTP
damgalarını sürekli kalacak şekilde yeniden yazıyor, yani duran akış boşluk
olarak GÖRÜNMÜYOR. Üç ölçüm çürüttü: (1) kare sayıları 1449 / 1469 — 4
saniyelik üç kayıp ~180 kare fark gerektirirdi; (2) tarayıcı kopyasının damga
ekseni duvar saatinden 11,45 sn kısa, bizim deliklerin toplamı 11,58 sn;
(3) botun kendi çözdüğü kare hızı düşüş pencerelerinde 14,5 → 8,9 ve 10,0
fps'e iniyor. Gerçek sebep: Chrome katman düşürürken kodlayıcıyı yeniden
yapılandırıyor ve HER İKİ katmanda da ~3-4 sn kare üretmiyor. Kayıt bu
noktada sadık; canlı derste öğrenci de aynı donmayı yaşıyor.

⚠⚠ Bu düzeltmenin de bir kısmı YANLIŞTI (bkz. plan §25.7). "Yayıncı her iki
katmanı da durduruyor" çıkarımı dayanaksızdı: bot'un `çözülen kare` sayacı
SFU'nun İLETTİĞİ akışı ölçüyor, yayıncının ürettiğini değil. Kayıt 191'in
logu tersini kanıtlıyor — aşağı yön kararı `enİyiCanlıKatman`'dan geçtiği
için, karar anında alt katmanın son paketi 1 sn'den yeni olmak ZORUNDA:
katman 0, katman 1 sustuktan **2,19 sn sonra** hâlâ üretiyordu. O saniyelerde
yazılacak görüntü vardı. Delik bizim.

Doğru olan kısım: canlı yol da donuyor (SFU eski katmanın ölümünü 2-4 sn'de
fark edip yeni katmanda anahtar kare bekliyor) — simulcast düşüşünde birkaç
saniyelik donma bilinen WebRTC davranışı. Fark: canlıda donma affediliyor,
dosyada delik kalıcı. Bu yüzden aşağıdaki hızlı düşüş yolu küçük bir
iyileştirme değil, asıl kazanç.

**Eşiği kısaltmak çözüm değil:** 3 saniye keyfi seçilmemişti — durgun ekranda
VP9 1 fps'e kadar iniyor (kayıt 182) ve kısa bir "sustu = öldü" kuralı onu
yanlış okurdu.

**Yapılan: ölçüt değişti, süre değil.** İki simulcast katmanı aynı yakalama
tikinden besleniyor:

| durum | üst katman | alt katman | son paketler arası fark |
|---|---|---|---|
| durgun ekran (1 fps) | seyrek | seyrek | milisaniyeler |
| bant daralması | **sustu** | 15 fps | yarım saniyede açılır |

Yeni hızlı yol (`enTazeAltKatman`): yazılan katman `katmanHızlıDüşmeEşiği`
(700 ms) susmuşsa VE altta son paketi ondan `katmanTazelikFarkı` (500 ms)
daha YENİ bir katman varsa, beklemeden hedef oraya konuyor. Yoklama 500
ms'de bir, yani karar en geç ~1,2 saniyede. Durgun ekranda fark hiç açılmadığı
için yanlış alarm üretmiyor.

Hızlı yol `sonGeçiş` çırpınma frenine takılmıyor (burası tahmin değil olgu;
elde akan veri varken delik açmak çırpınmadan her zaman kötü). Fren YUKARI
yönde aynen duruyor. Alt katman da suskunsa eski ihtiyatlı 3 sn kuralı
devrede kalıyor.

Yeni log satırı: `ÜST KATMAN SUSTU, ALT KATMAN AKIYOR — beklemeden düşülüyor`.

### 15. `pkg/sfu/rawrec/video.go` — KATMAN POLİTİKASI YENİDEN (2026-09-05)

Üç mutlak süre (`katmanCanlıEşiği` 1s, `katmanDüşmeEşiği` 3s,
`katmanÇıkmaEşiği` 2s) de KARE HIZINA bağlıydı ve 1 fps'lik durgun ekran
paylaşımında yanlış cevap veriyordu: alt katmana bir düşünce **bir daha
çıkılamıyordu** (kareler arası mesafe 1 sn eşiğinin etrafında salınıp
`canlıdan` sayacını sürekli sıfırlıyor). LiveKit'in kendi tabanı
`MinFPS: 0.5` — bizimki iki katı sertti.

**LiveKit'ten alınan:** ölçünün pencere/sayaç şekli ve histerezisin KAYNAK
TİPİNE göre ayrılması (`streamtracker`: paylaşım `CyclesRequired: 1` × 2 sn,
kamera `20` × 500 ms = 10 sn).

**Bizde kalan üstünlük:** LiveKit katmanları BAĞIMSIZ izliyor; biz
KARŞILAŞTIRIYORUZ. İki simulcast katmanı aynı yakalama tikinden beslendiği
için "alt katman üstü geçti mi / aday geride mi" ölçütü kare hızından
tamamen bağımsız — LiveKit'in penceresine gömülü 0,5 fps tabanı bizde yok.
Gerekçe: canlı yolda delik affediliyor, dosyada kalıcı.

| yön | kural |
|---|---|
| AŞAĞI hızlı | yazılan 700 ms sustu + alt ŞU AN üretiyor (≤500 ms) + yazılanı ≥500 ms geçmiş |
| AŞAĞI yavaş | yazılan `ölüEşiği` sustu + alt yazılanı ≥500 ms geçmiş |
| YUKARI | aday yazılandan geride değil (≤500 ms), `yukarıTik` yoklamadır kesintisiz |

```go
ölüEşiğiPaylaşım = 3s   yukarıTikPaylaşım = 2    // ≈1 sn  (yoklama 500 ms)
ölüEşiğiKamera   = 1s   yukarıTikKamera   = 20   // 10 sn  (LiveKit'in sayısı)
```

Kaldırıldı: `katmanCanlıEşiği`, `katmanDüşmeEşiği`, `katmanÇıkmaEşiği`,
`katmanDurum.canlıdan`, `enİyiCanlıKatman`, `sonGeçiş`.
Eklendi: `katmanDurum.uygunTik`, `enGeridenTazeAltKatman`,
`VideoWriter.ölüEşiği`, `VideoWriter.yukarıTik`.

**Image:** `v1.11.0-rawrec24` — kayıt 192'de ÖLÇÜLDÜ, iki eksiği çıktı; bkz. §16.

### 16. `rawrec/{video,saglik}.go` — AŞAĞI ÖLÇÜTÜ KAREYE, SAĞLIĞA MUTLAK TABAN (2026-09-05, kayıt 192)

Kayıt 192 (hareketli video → 700 kbps kısıt → tamamen durgun ekran → kısıt
kalkar) rawrec24'ün **asıl hedefini doğruladı**: alt katmanda 44 saniye
kaldıktan sonra, ekran hiç hareket etmeden üst katmana **kendiliğinden geri
çıktı** (t=34,45 → 78,66). Eski kodda bu imkânsızdı. Düşüş delikleri de
3,50/4,01/4,07 sn'den **1,67 sn**'ye indi, karar gecikmesi ~3 sn'den
997 ms – 1,478 sn'ye.

İki eksik çıktı:

**(a) 1 fps'lik durgun ekranda ÇIRPINMA — `katmanTazelikFarkı` yanlış alarm.**
9,4 saniyede dört geçiş (t = 95,9 / 99,1 / 104,1 / 105,3). §15'in gerekçesi
"iki katman aynı yakalama tikinden beslendiği için son paketleri milisaniye
arayla gelir, fark 500 ms'e çıkmaz" idi. **Ölçüm bunu çürüttü:** 1080p karesi
540p karesinden kat kat büyük, dar bantta AYNI TİKTEN çıkan iki kare SFU'ya
yarım saniyeden fazla arayla varıyor. Üç şart da sağlıklı içerikte sağlanıyor.
Bedeli: 3,02 sn'lik delik (dosyanın en büyüğü) + dört gereksiz 1080p anahtar
kare (59-75 KB) — PLI bütün abonelere gidiyor.

*Çözüm:* aşağı yönde ölçüt **SÜRE değil KARE**. `Write` katman başına kare
sayıyor (RTP damgası değişimi = yeni kare, VP9 başlığı çözülmüyor); yazılan
katman kare ürettiği her yoklamada bütün katmanlar "başa baş" işaretleniyor.
Şart: `kare - işaret >= katmanÖndeKare` (2). Kendiliğinden ölçekleniyor —
15 fps'te ~130 ms, 1 fps'te 2 sn. Durgun ekranda iki katman aynı tikte
ürettiği için alt katman **öne geçemez**, varış sırası ne olursa olsun.

**(b) SFU dosyası 0,22 puanla reddedildi — `kareButunlukEsigi` bir ORAN.**
485 kare yazıldı, 11 atıldı → %2,22 > %2 eşik → postprocess tarayıcı
kopyasına düştü. Oysa iki dosya yan yana: SFU 485 kare / 127,0 sn kapsama,
tarayıcı 480 / 123,0; **her ikisinde sıfır çözücü hatası**, aynı andan
çıkarılan kareler gözle ayırt edilemez. Eşiğin kendi gerekçesi 24 fps
varsayıyor ("%2 ≈ dakikada yarım saniye" = 29 kare/dk); bu kayıt ortalama
3,7 fps'ti ve 5 kare/dk ile **altı kat daha iyiydi**. Oran, kare hızı 10 kat
oynayan bir kaynakta yanlış ölçüt.

*Çözüm:* ret artık İKİ ölçütü birden istiyor — oran > %2 **VE** mutlak hız
> `kareButunlukTabanı` (0,25 kare/sn, yani 24 fps'teki %2'nin yarısı).
Sağlık raporuna `kare_atilan_hiz` da yazılıyor.

| yön | ESKİ kural (rawrec24) | YENİ kural (rawrec25) |
|---|---|---|
| AŞAĞI hızlı | yazılan 700 ms sustu + alt ŞU AN üretiyor + **yazılanı ≥500 ms geçmiş** | … + **alt katman ≥2 KARE öne geçmiş** |
| AŞAĞI yavaş | yazılan `ölüEşiği` sustu + **alt ≥500 ms geçmiş** | … + **alt ≥2 KARE geçmiş** |
| YUKARI | değişmedi (aday ≤500 ms geride, `yukarıTik` yoklamadır) | aynı |

Eklendi: `VideoWriter.katmanSonRTP/katmanKare`, `katmanDurum.kare/işaret`,
`katmanÖndeKare`, `kareButunlukTabanı`. `katmanTazelikFarkı` artık YALNIZ
yukarı yönde.

**Image:** `v1.11.0-rawrec25` — sınanmadan §17 geldi, canlı olan `rawrec26`.

### 17. `rawrec/video.go` — YUKARI TOLERANSI KARE ARALIĞI KADAR (2026-09-05)

§16 aşağı yönü kareye çevirdi ama yukarı yön sabit 500 ms ölçmeye devam
ediyordu — ve **aynı çarpıklığı ters işaretle taşıyordu**. 1080p karesi 540p
karesinden kat kat büyük olduğu için aynı yakalama tikinden çıksalar bile üst
katman ~300-500 ms SONRA varıyor. Yoklama, alt katmanın karesi gelmiş üst
katmanınki gelmemişken denk gelirse üst katman "geride" görünüp `uygunTik`
sayacını sıfırlıyor. 1 fps'te sayaç **1-0-1-0 salınıp 2'ye hiç ulaşmıyor**:

```
t=0,5: L0=0,0  L1=0,3 → −0,3 ✓ tik=1
t=1,0: L0=1,0  L1=0,3 → +0,7 ✗ tik=0     ← sahte sıfırlama
t=1,5: L0=1,0  L1=1,3 → −0,3 ✓ tik=1
```

Yukarı çıkış, yoklama fazının kare fazıyla şans eseri denk gelmesine kalıyor.
⚠ **DÜZELTME (kayıt 193).** Bu değişikliği ilk gerekçelendirirken kayıt
192'de "canlı yol üst katmanı t=76,15'te aldı, yazıcı t=78,66'da → 2,51 sn
gecikme" diye bir ölçü verdim. **O ölçü geçersizdi.** İki dosyanın zaman
ekseni her katman geçişinde ayrı ayrı yeniden tabanlanıyor; kayıt 193'te
yedi geçişle bakınca fark tek yönde büyüyor (0,21 → 3,79 → 5,32 → 6,23 →
7,72 → 9,90 → 12,99 sn) ve toplam süre farkına (200,02 − 187,03 = 12,99 sn)
birebir oturuyor. Yani sürüklenme, karar gecikmesi değil. Dosya zaman
eksenleri karşılaştırılarak gecikme ÖLÇÜLEMEZ; VARIŞ saatine bakmak gerekir.

Değişikliğin gerekçesi yukarıdaki aritmetik olarak duruyor: sabit 500 ms
toleransla sayaç, yoklama fazı kare fazıyla şans eseri denk gelmedikçe 2'ye
ulaşamıyor. Kayıt 193'te varış saatiyle ölçülen gerçek durum: üç yukarı
geçişin üçünde de yazıcı, canlı yolun katman değiştirdiği 10 saniyelik örnek
penceresinin İÇİNDE, ikisinde pencerenin başında geçti — ölçülebilir gecikme
yok.

*Çözüm:* tolerans sabit değil, yazılan katmanın **ölçülen kare aralığı**
kadar — tabanı `katmanTazelikFarkı` (500 ms), tavanı `katmanGeriKalmaTavanı`
(4 sn, yani 0,25 fps'e kadar). Aralık yoklama penceresinde geçen süre ÷ o
pencerede üretilen kare sayısı, yani yoklama aralığından (500 ms) bağımsız.

- **24 fps kamera:** aralık ~42 ms → 500 ms tabanı geçerli →
  **davranış değişmiyor**, LiveKit'ten alınan 10 sn histerezis aynen duruyor
- **1 fps durgun paylaşım:** aralık ~1 sn → sahte sıfırlama kapanıyor,
  2 yoklamada (1 sn) çıkılıyor

Böylece iki yön de aynı fiziksel gerçeği (boyut farkından doğan varış
gecikmesi) hesaba katıyor ve ikisi de kare hızıyla ölçekleniyor:

| yön | ölçüt |
|---|---|
| AŞAĞI | alt katman ≥`katmanÖndeKare` (2) **KARE** öne geçti mi |
| YUKARI | aday, bir **KARE ARALIĞINDAN** fazla geride değil, `yukarıTik` yoklamadır |

Eklendi: `katmanDurum.aralık/öncekiKare/öncekiSonPaket`, `katmanGeriKalmaTavanı`.
`katmanTazelikFarkı` artık tek başına kural değil, toleransın TABANI.

**Image:** `v1.11.0-rawrec26` — kamerada da doğrulandı (kayıt 194).

### 18. `rawrec/video.go` — BÖLÜM TABANI ÇEKİM SAATİNDEN (2026-09-05, kayıt 194)

**Ses–görüntü senkronu bozuktu.** Katman geçişinde yeni bölümün damga tabanı
`geliş - sonGeliş` ile, yani paketlerin SFU'ya VARDIĞI anlarla hesaplanıyordu.
Varış ≠ çekim: bant daralınca ölmekte olan üst katmanın son kareleri kuyrukta
bekliyor (varışları çok geç), yeni katmanın ilk anahtar karesi anında geliyor.
Varış farkı, gerçek çekim farkından **ölen katmanın kuyruk gecikmesi kadar
eksik** çıkıyor.

Ölçüldü — her katman geçişinde **~1 saniye** kayboluyordu:

| kayıt | akış | gerçek (varış) | dosya (PTS) | kayıp | geçiş |
|---|---|---|---|---|---|
| 194 kamera | video | 138,84 sn | 136,81 sn | **−2,03** | 2 |
| 194 kamera | ses | 138,46 sn | 138,46 sn | 0 | 0 |
| 193 paylaşım | video | 206,51 sn | 200,02 sn | **−6,49** | 7 |
| 193 paylaşım | ses | 206,62 sn | 206,64 sn | 0 | 0 |

Ses tek katmanlı, hiç rebase etmiyor, kaybı yok. Video kısaldığı için
oynatıcıda görüntü sese göre giderek ÖNE kaçıyor — ham SFU yoluna geçmenin
bütün amacı ses-görüntü farkını sıfırlamaktı, bu hata onu tek başına
götürüyordu.

*Çözüm:* her katmanın KENDİ Sender Report'undan çekim saati. Yeni yardımcı
`katmanDuvarSaati(katman, rtp)`:

```
duvar = AtAdjusted − (sr.RtpTimestamp − rtp) / clockRate
```

`AtAdjusted` sunucu saatinde olduğu için iki farklı katmanınki
karşılaştırılabilir (dosya çapası zaten aynı hesabı kullanıyor). SR yoksa ya
da sonuç mantıksızsa (≤0 veya `bölümAraÜstSınır` = 60 sn üstü) eski varış
ölçüsüne düşülüyor — kayıt hiçbir koşulda bozulmuyor.

Tanı için her bölüm başında log: `BÖLÜM TABANI  ara_sn=… varis_sn=… kaynak=çekim`
(yeni ölçü / eski ölçü / hangi yol). Düzeltmenin etkisi kayıttan doğrulanabilir.

Eklendi: `VideoWriter.katmanDuvarSaati`, `bölümAraÜstSınır`, döngü yerelleri
`sonYazRTP` / `sonYazKatman`.

**Image:** `v1.11.0-rawrec27` — kayıt 195'te doğrulandı (2,03 sn → 28 ms).

### 19. `rawrec/{video,rawrec,saglik}.go` — REFERANS KATMAN + SR GÜNLÜĞÜ (2026-09-05)

Üç değişiklik birlikte (`rawrec28`).

**(a) Referans katman — birikme yapısal olarak kalktı.** §18 boşluğu çekim
saatinden hesaplıyordu ama her geçişte "eski katman ↔ yeni katman" diye İKİLİ
çeviriyordu; her geçiş kendi SR tahmin hatasını taşıyor, hatalar zincirleniyor.

LiveKit'in canlı yolu bunu yapmıyor: tek bir referans katman seçip
(`referenceLayerSpatial`) her katmanı hep ona çeviriyor. Aynısını yaptık —
ilk yazılan katman referans, her katmanın kayması BİR KEZ hesaplanıp
donduruluyor:

```
kayma_k = (temel_k − temel_r)·rate + srRtp_r − srRtp_k
```

Kaymalar SABİT olduğu için aşağı geçişte giren +B hatası, yukarı geçişte AYNI
sabitten gelen −B ile **birebir** götürüyor. Üç kademeli yol: referans uzayı →
katman başına duvar saati → varış farkı.

*Ölçüldü (kayıt 197/198/199):* 5 645 / 2 022 / 14 379 karede sapma ±2,6 ms
içinde, **birikme −0,5 / −0,4 / −0,1 ms**. Kayıt 197'de 8 bölüm tabanının
sekizi de `kaynak=referans`, yedek yola hiç düşülmedi.

**(b) SR günlüğü — Chrome'un güvenlik ağının dosya karşılığı.** Canlı Chrome
her Sender Report'ta kendini yeniden ayarlıyor (45 dk'da ~2700 düzeltme).
Dosyada anlık düzeltme yapamayız ama SONRADAN düzeltebiliriz — yeter ki veri
kayıtlı olsun. Her SR değişiminde `(katman, rtp, at_ns)` yan JSON'a
`sr_gunlugu` olarak yazılıyor; hem görüntü hem ses yazıcısında.

**(c) Ret eşiği senkron lehine.** `kareButunlukEsigi` 0,02 → **0,05**. Eski
gerekçe reddin bedelini saymıyordu: reddedince tarayıcı kopyasına düşülüyor ve
o kopyanın zaman çizgisi ölçüldü — kayıt 196'da 506,5 sn yerine 488,9 sn
(%3,5 kayıp). "%2-5 kare eksik" ile "saniyelerce senkron kayması" aynı kefeye
konamaz.

*Ayrıca `rawrec29`:* tanı amaçlı `kare_gunlugu` (her karenin katman + ham RTP
+ PTS'i), `SN_RAWREC_KARE_GUNLUGU=1` ile açılıyor, varsayılan KAPALI. Ölçüm
bitince kapatıldı; 45 dakikalık ders başına ~2 MB JSON yazıyor.

### 20. `rawrec/rawrec.go` — SR GÜNLÜĞÜNE HAM NTP (2026-09-05)

Kayıt 197'de ses tarafında saat kayması ÖLÇÜLEMEDİ: artık 20,6 ms, hata payı
±33 ppm. Sebep örnek azlığı değil, **ölçtüğümüz saatti**.

`AtAdjusted` yayılım gecikmesi TAHMİNİYLE düzeltilmiş sunucu saati — içinde ağ
gürültüsü var. SR'ın içindeki ham `NtpTimestamp` ise yayıncının kendi saati.
Ses ve video AYNI makinede AYNI saatten damgalandığı için RTP'yi NTP ile
karşılaştırmak tamamen **yayıncı içi** bir ölçüm; ağ hiç işin içinde değil.

`srOrnek`e `ntp_ns` eklendi (`ntpNs()` ile RFC 868 NTP → unix ns). Ses
tarafında örnekleme 50 pakette birden 10'a indirildi (SR ~5 sn'de bir
güncelleniyor; sık bakmak yeni örnek üretmiyor ama gecikmeden yakalıyor).

*Sonuç:* artık **20,6 ms → 0,3 ms**. Kayıt 199 (613 sn) ile ölçülen:

| akış | kayma (NTP) | 45 dakikada |
|---|---|---|
| kamera k0 | −0,05 ± 0,1 ppm | −0,1 ms |
| kamera k1 | −0,08 ± 0,1 ppm | −0,2 ms |
| mikrofon | −5,09 ± 3,4 ppm | −13,7 ms |

Göreli ses–görüntü kayması **−5,0 ± 3,4 ppm**, en kötü hâlde 45 dakikada
−23 ms — fark edilebilirlik eşiğinin (45 ms önde / 125 ms geride) altında.

### 21. `rawrec/rawrec.go` — ÇAPA TEK SR'DAN DEĞİL, BÜTÜN GÜNLÜKTEN (2026-09-05)

Çapa dosya kapanırken görülen **tek** SR'dan hesaplanıyordu. 10 dakikalık
kayıtta 600 SR görüyoruz — 599'unu atıp birine güveniyorduk ve o örneğin
gürültüsü doğrudan çapaya geçiyordu ("kalan hata: onlarca milisaniye").

Her örnek kendi başına bir çapa veriyor (`çapa_i = at_i − (rtp_i − ilkRTP)/rate`).
Saatler birebir aynı hızda olsa hepsi aynı çıkardı; çıkmıyor:
**saçılma** = ölçüm gürültüsü, **eğilim** = saat kayması.

`capaFit` bu noktalara doğru uyduruyor ve **kaydın BAŞINDAKİ** değeri alıyor —
ortalama alsaydık eğim yüzünden kaydın ORTASINI verirdi (10 dk / 5 ppm'de
1,5 ms sapma). İki geçiş: uydur → 3×RMS dışını at → yeniden uydur. 12'den az
örnekte eski tek-SR yoluna düşülüyor (az noktayla uydurulan doğru tek
noktadan kötü olabilir — kayıt 198: 18 örnekle ±66 ppm).

⚠ Kesişim çapayı, **eğim kaymayı** veriyor — yani düzeltme için gereken sayı
bu hesabın yan ürünü. `capa_kayma_ppm` olarak yazılıyor ama fork'ta
KULLANILMIYOR; düzeltmeyi postprocess yapıyor (göreli kayma iki akışın
karşılaştırılmasını gerektiriyor, SFU'da ses ve video ayrı yazıcılar).

Yan JSON'a eklenenler: `capa_kaynak: "sfu-sr-fit"`, `capa_ornek`,
`capa_artik_ms`, `capa_kayma_ppm`.

**Image:** `ghcr.io/speaknow06/livekit-server:v1.11.0-rawrec31` (demo'da canlı).

⚠ **Türev zinciri sıfırlandı.** rawrec19'dan beri her sürüm bir öncekinden
`FROM` aldığı için eski binary'ler katmanlarda birikiyordu: 192 → 263 → 335 →
407 → 479 → **550 MB** (rawrec24). rawrec25 son TAM build olan **rawrec18
(192 MB)** tabanından yapıldı → **263 MB**. Bundan sonraki türevler de
rawrec18'den alınmalı; prod'da yine de TAM build yapılacak.

## Rebuild
```bash
cd livekit-server-source            # bu repo, branch speaknow-vp9-simulcast
docker build --platform linux/amd64 -t ghcr.io/speaknow06/livekit-server:TAG .
echo "$GHCR_PAT" | docker login ghcr.io -u SpeakNow06 --password-stdin   # PAT: Speaknow-Prod.md
docker push ghcr.io/speaknow06/livekit-server:TAG
```
Deploy (prod): `~/Livekit-server/docker-compose.yml` image tag güncelle → `docker compose pull livekit && docker compose up -d livekit`.

## Client SDK fork (livekit-client — VP9 simulcast'i AÇMAK için ŞART)
Upstream `livekit-client` VP9'u her koşulda SVC'ye zorlar; VP9 simulcast'i client'tan açmak için
AYRI fork ŞART: **`~/Desktop/web/livekit-client-fork`** (livekit-client 2.19.0, branch `svc-patch`).
Ana sinyal: **`SimulcastCodec.videoLayerMode = ONE_SPATIAL_LAYER_PER_STREAM`** (yoksa server SVC
varsayar) + per-encoding `scalabilityMode` (Chrome M113+ şartı) + simulcast'te SVC default'larını
atlama. Bundle: `livekit-client-2.19.0-svc.umd.js` (speaknow-server vendor'da). **Server fork (bu
repo) + client SDK fork BİRLİKTE gerekir.**

## Client config (speaknow-server repo, branch server)
Classroom ekran: `static/js/classroom-media-config.js` → `videoCodec:'vp9', simulcast:true,
scalabilityMode:'L1T2'`, ladder 1080/720/540. `contentHint` text VE motion ikisi de çalışır (bu
fork'larla). Webinar ayrı: H.264 simulcast + text (dokunulmadı).

## Notlar
- Spatial-drop-first allocator fork'u (a5a9a29) DENENDİ sonra GERİ ALINDI (1a05678) — gereksiz; stock
  allocator + Simulcast selector yeterli. Bu fork'ta YOK.
- Bu fork upstream'in DESTEKLEMEDİĞİ bir yolu (VP9 simulcast) tamamlıyor → her LiveKit upgrade'inde
  5 değişikliğin yeni base'e taşınması gerekir.
