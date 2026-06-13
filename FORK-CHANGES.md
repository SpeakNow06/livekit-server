# SpeakNow LiveKit Server Fork — VP9 Simulcast Desteği

`livekit/livekit` **v1.11.0** üzerine **minimal** fork (5 dosya, ~35 satır).
Amaç: **VP9 simulcast** desteği (upstream'de bilerek KAPALI — LiveKit VP9'u SVC sayar,
`simulcast` alanını yok sayar) + ipv6 TURN fix.

- **Branch:** `speaknow-spatialfirst`
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

### 4. `pkg/sfu/receiver_base.go` — VP9 simulcast rid→spatial layer eşlemesi  (ASIL KÖK)
`if extPkt.Spatial >= 0 { spatialLayer = extPkt.Spatial }` — SVC için spatial'ı paketten (DD SID)
alıyordu. VP9 simulcast'te her rid tek-spatial → DD SID **HEP 0** → TÜM rid'ler spatial 0'a çöker →
forwarder rid'leri ayırt edemez, `lastSSRC != SSRC` her pakette → switch storm → `extNextTS =
extLastTS+1` frozen → donma (ilk layer-switch'te, ~68. sn). H.264/VP8'de `Spatial = -1` olduğu için
bu override hiç fırmıyordu (onlar bu yüzden çalışıyor). **Fix:** override yalnız `layer == 0` (SVC,
tek uptrack); simulcast rid≥1'de rid index (`layer`) authoritative.

### 5. `Dockerfile` — hızlı cross-compile
`FROM --platform=$BUILDPLATFORM` → native cross-compile (amd64), QEMU emülasyon yok (~1 dk build).

## Rebuild
```bash
cd livekit-server-source            # bu repo, branch speaknow-spatialfirst
docker build --platform linux/amd64 -t ghcr.io/speaknow06/livekit-server:TAG .
echo "$GHCR_PAT" | docker login ghcr.io -u SpeakNow06 --password-stdin   # PAT: Speaknow-Prod.md
docker push ghcr.io/speaknow06/livekit-server:TAG
```
Deploy (prod): `~/Livekit-server/docker-compose.yml` image tag güncelle → `docker compose pull livekit && docker compose up -d livekit`.

## Client tarafı (speaknow-server repo, branch server)
Classroom ekran: `static/js/classroom-media-config.js` → `videoCodec:'vp9', simulcast:true,
scalabilityMode:'L1T2'`, ladder 1080/720/540. `contentHint` text VE motion ikisi de çalışır (bu
fork'larla). Webinar ayrı: H.264 simulcast + text (dokunulmadı).

## Notlar
- Spatial-drop-first allocator fork'u (a5a9a29) DENENDİ sonra GERİ ALINDI (1a05678) — gereksiz; stock
  allocator + Simulcast selector yeterli. Bu fork'ta YOK.
- Bu fork upstream'in DESTEKLEMEDİĞİ bir yolu (VP9 simulcast) tamamlıyor → her LiveKit upgrade'inde
  5 değişikliğin yeni base'e taşınması gerekir.
