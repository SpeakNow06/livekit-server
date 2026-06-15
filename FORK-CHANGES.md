# SpeakNow LiveKit Server Fork — VP9/AV1 Simulcast Desteği

`livekit/livekit` **v1.11.0** üzerine **minimal** fork (5 dosya, ~35 satır).
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

### 6. `pkg/rtc/dynacast/*` — dynacast EXACT-MATCH (VP9 simulcast'ten BAĞIMSIZ özellik)
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
