# VP9/AV1 Simulcast (`ONE_SPATIAL_LAYER_PER_STREAM`) — implementation notes

End-to-end support for **VP9 and AV1 simulcast** on LiveKit: three independent single-spatial-layer
RTP streams (rids `q`/`h`/`f`, each with its own SSRC) instead of one SVC stream.

For upstream issue [livekit/livekit#4594](https://github.com/livekit/livekit/issues/4594).
Every change below is annotated with `file:line`, the **old → new** code, and **why**, so it can
be reviewed without diffing.

| | Base | Files |
|---|---|---|
| **Server (SFU)** — `livekit/livekit` | `v1.11.0` (`8ccad68`) | 3 core + 6 additional |
| **Client (SDK)** — `livekit/client-sdk-js` | `v2.19.0` (`b09b800`) | 3 |

The two changes are a **coordinated pair**: the client *signals*
`VideoLayer.Mode.ONE_SPATIAL_LAYER_PER_STREAM` and emits standards-compliant per-encoding
simulcast; the SFU *honors* that mode in the forwarding pipeline. **Both are required** — neither
half works alone, and both are backward-compatible: the new path only engages when the publisher
opts in with `simulcast: true` on an SVC-capable codec. With `simulcast` unset, behaviour is
identical to upstream (SVC).

---

## 1. Motivation

LiveKit supports VP9 and AV1 **only as SVC** — for an SVC-capable codec the SFU ignores the
`simulcast` flag and assumes a single SVC stream, so there is no real (rid-based) simulcast for
VP9/AV1. This PR adds it for both. **AV1 needs even less than VP9**: its forwarder branch already
uses the Simulcast selector in stock LiveKit, and the receiver/parser fixes below are
codec-agnostic, so the only AV1-specific addition is one line (`skipReferenceTS`). The client
needs no AV1-specific change at all (`isSVCCodec` already covers it). Both VP9 and AV1 simulcast
are verified in production, including `contentHint='text'`.

Real simulcast matters because, for screen-share, VP9 SVC has two problems:

- **`contentHint:'text'` (screen-content) collapses the layer structure.** VP9 + L3T3 +
  `contentHint=text` degrades to VP8 L1T3 — losing resolution and the spatial layers. (With
  `contentHint=motion`, VP9 SVC L3T3/L3T3h works fine; the breakage is specific to `text`.) With
  simulcast each rid stays a full-resolution VP9 stream, so text stays sharp.
- **Hardware decode on Android.** A multi-layer SVC stream frequently falls back to software
  decode (libvpx) on Android — heavy on low-end phones. With simulcast each rid is a plain
  single-layer VP9 stream that decodes in hardware.

The protocol already defines the signal — `VideoLayer.Mode.ONE_SPATIAL_LAYER_PER_STREAM` — but
the path is not implemented end-to-end: the client SDK always forces SVC for VP9/AV1, and even
when the mode is signalled the SFU's forwarder/receiver/parser mishandle the separate streams and
freeze on the first layer switch (~60–68 s in).

---

## 2. Client SDK changes (`client-sdk-js` v2.19.0)

3 files. All gated on `opts.simulcast === true` for an SVC-capable codec.

### `src/room/participant/LocalParticipant.ts`

**`:17`** — import the enum
`+ VideoLayer_Mode,` (added to the `@livekit/protocol` import).

**`:1137`** — don't apply SVC defaults under simulcast
- old: `if (isSVCCodec(videoCodec)) {`
- new: `if (isSVCCodec(videoCodec) && !opts.simulcast) {`
- why: the SVC default block forces `scalabilityMode`, disables simulcast and sets up a VP8 backup.
  Skipping it lets VP9/AV1 + `simulcast` fall through to the standard rid-based path.

**`:1142`** — respect a caller-set scalability mode (inside the still-SVC screenshare branch)
- old: `opts.scalabilityMode = 'L1T3';`
- new: `opts.scalabilityMode = opts.scalabilityMode ?? 'L1T3';`
- why: don't clobber an explicit mode chosen by the application; only default when unset.

**`:1150`** — respect a caller-set contentHint (don't force `'text'` down to `'motion'`)
- old: `track.mediaStreamTrack.contentHint = 'motion';`
- new: `track.mediaStreamTrack.contentHint = track.mediaStreamTrack.contentHint || 'motion';`
- why: for screenshare with an SVC codec, upstream **unconditionally overwrote** the app's
  contentHint with `'motion'`, so `contentHint='text'` (screen-content / slides / whiteboard) could
  never reach the encoder — text was always treated as motion. Now it only defaults to `'motion'`
  when unset, so a caller-set `'text'` is respected. (Verified: VP9 and AV1 simulcast both work with
  `'text'`.)

**`:1161–1170`** — signal the mode to the SFU *(the key change)*
- old:
  ```ts
  req.simulcastCodecs = [ new SimulcastCodec({ codec: videoCodec, cid: track.mediaStreamTrack.id }) ];
  ```
- new:
  ```ts
  const primaryCodecInfo = new SimulcastCodec({ codec: videoCodec, cid: track.mediaStreamTrack.id });
  if (isSVCCodec(videoCodec) && opts.simulcast) {
    primaryCodecInfo.videoLayerMode = VideoLayer_Mode.ONE_SPATIAL_LAYER_PER_STREAM;
  }
  req.simulcastCodecs = [primaryCodecInfo];
  ```
- why: without an explicit mode, `participant.go` sees `MODE_UNUSED` + an SVC-capable mime type and
  defaults to `MULTIPLE_SPATIAL_LAYERS_PER_STREAM` (SVC). This is what tells the SFU to use the
  rid-based simulcast selector.

**`:1199`** — record the publish mode on the track
- new: `track.svcMode = isSVCCodec(opts.videoCodec) && !opts.simulcast;`
- why: downstream layer/dynacast code reads "SVC vs simulcast" from this explicit flag instead of
  re-deriving it from the codec (see `LocalVideoTrack.ts`).

**`:1213`** — derive layers as simulcast, not SVC
- old (arg to `computeTrackBackupEncodings`/SDK layer setup): `isSVCCodec(opts.videoCodec),`
- new: `isSVCCodec(opts.videoCodec) && !opts.simulcast,`

**`~:1268` + `src/room/PCTransport.ts:26`** — start-bitrate: a local deployment preference, not part of this PR. We chose to start `x-google-start-bitrate` at ~60% of the top layer's bitrate (`startBitrateForSVC` 0.6) for a sharper share start; noted for completeness only.

### `src/room/participant/publishUtils.ts` — `computeVideoEncodings`

**`:140`** — don't take the SVC branch under simulcast
- old: `if (scalabilityMode && isSVCCodec(videoCodec)) {`
- new: `if (scalabilityMode && isSVCCodec(videoCodec) && !useSimulcast) {`

**`:209, :232, :253`** — build a real simulcast encoding ladder
- The function now collects `simulcastPresets` (`[low, mid, original]` / `[low, original]` /
  `[original]`) into `simulcastEncodings` (`:232`) and returns it (`:253`) instead of early-returning
  the SVC single-encoding shape.

**`:248–250`** — give every encoding a `scalabilityMode` *(Chrome M113+ requirement)*
- new:
  ```ts
  if (isSVCCodec(videoCodec)) {
    for (const enc of simulcastEncodings) {
      enc.scalabilityMode = scalabilityMode ?? 'L1T2';
    }
  }
  ```
- why: Chrome M113+ only treats multiple VP9/AV1 encodings as real simulcast when **every**
  encoding carries `scalabilityMode` + `scaleResolutionDownBy`; otherwise it interprets them as
  legacy SVC.

### `src/room/track/LocalVideoTrack.ts`

**`:252`** — new field: `svcMode?: boolean;` (undefined ⇒ legacy codec-derived behaviour).

**`:255–256`** — new getter:
```ts
private get isSvcPublish(): boolean {
  return this.svcMode ?? isSVCCodec(this.codec);
}
```

**`:270`** — `setPublishingQuality` → `this.setPublishingLayers(this.isSvcPublish, qualities);`
(was `isSVCCodec(this.codec)`).

**`:482, :494`** — `setPublishingCodec` paths → `this.svcMode ?? isSVCCodec(codec.codec)`
(was `isSVCCodec(...)`).

**`:576`** — per-layer encoding mode
- old: `scalabilityMode: idx === 0 && isSVCCodec(this.codec) ? 'L1T3' : undefined,`
- new: `scalabilityMode: idx === 0 && isSVCCodec(this.codec) ? (this.isSvcPublish ? 'L1T3' : 'L1T2') : undefined,`

---

## 3. Server / SFU core changes (`livekit` v1.11.0)

The VP9-simulcast core is **3 files** (`receiver_base.go`, `forwarder.go`,
`dependencydescriptorparser.go`).

### `pkg/sfu/receiver_base.go:1020` — rid → spatial-layer mapping  *(root cause)*

- old: `if extPkt.Spatial >= 0 {`
- new: `if extPkt.Spatial >= 0 && layer == 0 {`
- why: in VP9 **simulcast** every rid is a single-spatial-layer stream, so the DD spatial id is
  **always 0** on every rid. Taking the spatial layer from the packet collapses **all** rids to
  spatial 0 → the forwarder can't tell the rids apart → `lastSSRC != SSRC` on essentially every
  packet → switch storm → `extNextTS = extLastTS + 1` → **frozen on the first layer switch**.
  H.264/VP8 never hit this because their `Spatial == -1` (the override never fires). The fix keeps
  the override for `layer == 0` (true SVC, single uptrack) and makes the rid index (`layer`)
  authoritative for simulcast rids (`layer >= 1`).

### `pkg/sfu/forwarder.go` — `skipReferenceTS` + Simulcast selector  *(VP9 branch, `case mime.MimeTypeVP9:` at `:362`)*

**`:372`** — new: `f.skipReferenceTS = true`
- why: VP9 simulcast's per-layer RTCP Sender Reports are unreliable, so the SR-based cross-layer
  timestamp offset (`getRefLayerRTPTimestamp`) comes out wrong/zero → layer switch is rejected with
  "switch point too far behind" → NACK storm / collapse. This reuses LiveKit's existing
  `ONE_SPATIAL_LAYER_PER_STREAM_INCOMPLETE_RTCP_SR` escape hatch: skip the SR offset and use the
  elapsed-time estimate (`extExpectedTS`) instead.

**`:381`** — selector at initial setup
- old: `f.vls = videolayerselector.NewDependencyDescriptor(f.logger)`
- new: `f.vls = videolayerselector.NewSimulcast(f.logger)`
- why: the DD selector mismanaged the separate streams and reported a "switch" on almost every
  frame. The Simulcast selector uses the separate-stream model correctly (switch only on keyframe)
  — the same selector VP8/H.264 simulcast already use. `skipReferenceTS` above is what makes this
  selector viable for VP9.

**AV1 (`case mime.MimeTypeAV1:`)** — one line: `f.skipReferenceTS = true` added to the AV1
simulcast branch (same rationale: unreliable per-layer RTCP SRs). The AV1 branch already used the
Simulcast selector in stock LiveKit, and §3.1 + §3.3 are codec-agnostic, so this single line is the
only AV1-specific change. Verified in production.

### `pkg/sfu/buffer/dependencydescriptorparser.go` — frame-drop threshold

**`:69`** — new field: `structureChangeExtFrameNum uint64`

**`:160`** — drop test
- old: `if extFN < r.structureExtFrameNum {`
- new: `if extFN < r.structureChangeExtFrameNum {`

**`:198`** — advance the new threshold only on a real structure change
- new: `r.structureChangeExtFrameNum = extFN` (inside `if r.structure == nil || ...StructureId != r.structure.StructureId`).

**`:262`** — reset to 0 in `restart()`.

- why: upstream advanced `structureExtFrameNum` on **every** keyframe, even when the structure
  (`StructureId`) was unchanged. With screen-content/text (frequent keyframes), late- or
  retransmit-arriving **valid** frames were then dropped as "earlier than current structure" →
  freezes. The new field only advances when the structure actually changes; `ExtKeyFrameNum` (used
  by the DD selector) still uses the original `structureExtFrameNum` and is untouched.

---

## 4. Additional changes we made

In the same fork but **independent of VP9 simulcast**; could be split into separate PRs (or
dropped) as preferred.

### 4.1 IPv6 TURN URL fix — `pkg/service/roommanager.go:1030`

- old:
  ```go
  for _, ip := range r.config.RTC.NodeIP.ToStringSlice() {
    urls = append(urls, fmt.Sprintf("turn:%s:%d?transport=udp", ip, r.config.TURN.UDPPort))
  }
  ```
- new:
  ```go
  urls = append(urls, fmt.Sprintf("turn:%s:%d?transport=udp", r.config.RTC.NodeIP.V4, r.config.TURN.UDPPort))
  ```
- why: `ToStringSlice()` also returns the IPv6 address, emitted **without brackets**
  (`turn:2a03:...:3478`). The browser rejects the malformed URL and
  `RTCPeerConnection.setConfiguration` throws → **no client connects** on a dual-stack host.
  Restricting the advertised TURN URL to `NodeIP.V4` matches what `turn.go RelayAddress` already
  uses. Genuine upstream bug, unrelated to VP9 (see also livekit#4404).

### 4.2 Exact-match dynacast — `pkg/rtc/dynacast/*` + `pkg/rtc/mediatrack.go`

Make the publisher encode **only the simulcast layers that actually have a subscriber** (Meet
style), instead of every layer ≤ the single `max`.

**Why:** upstream keeps one `max` subscribed quality and enables **every** layer `q <= max`,
keeping lower layers "warm" even when nobody watches them — so unwatched layers are still encoded.

- **`pkg/rtc/dynacast/interfaces.go:125–138`** — new `qualitySet uint32` bitmask type +
  `has(q)` / `add(q)` / `addUpTo(q)`. Represents "which spatial layers currently have a subscriber."
- **`interfaces.go:142`** — listener signature gains the set:
  `OnUpdateMaxQualityForMime(mimeType, maxQuality, qualities qualitySet)` (was max only).
- **`pkg/rtc/dynacast/dynacastqualityvideo.go:197–208`** — while computing `maxSubscribedQuality`,
  also build the set: local subscribers `add(quality)` (exact), remote nodes `addUpTo(quality)`
  (no per-node set available → conservative, keeps all sub-layers on so cross-node subscribers
  aren't starved). **`:237`** passes the set to the listener; the early-return guard now also fires
  on a set change (`subscribedQualities == d.subscribedQualities`), so a set change with unchanged
  `max` still triggers an update.
- **`pkg/rtc/dynacast/dynacastmanagervideo.go:306`** — the core decision
  - old: `Enabled: q <= quality`
  - new: `Enabled: enabled` where (`:294`)
    ```go
    multiLayer := d.params.IsMultiLayer != nil && d.params.IsMultiLayer(mime)
    set := d.committedSubscribedQualities[mime]
    // per quality q:
    if multiLayer && set != 0 { enabled = set.has(q) } else { enabled = q <= quality }
    ```
  - Gaining a layer bit is treated as an upgrade (sent immediately, no debounce); losing one is a
    debounced downgrade.
- **`dynacastmanagervideo.go:41`** + **`pkg/rtc/mediatrack.go:152`** — new
  `DynacastManagerVideoParams.IsMultiLayer` callback, wired to
  `len(buffer.GetVideoLayersForMimeType(...)) > 1`.
  - why (**single-layer guard, important**): a single-encoding track sits in the SDP's `q` (LOW)
    rid slot, but the server computes its subscriber quality as **HIGH**. Stock's "enable
    everything ≤ max" masks this; exact-match would enable only HIGH and disable the `q`/LOW
    encoding → **the track never sends any media**. So exact-match is applied **only to multi-layer
    tracks**; single-layer (e.g. camera) or empty-set falls back to stock (`q <= quality`).

### 4.3 Dockerfile — `Dockerfile:15`

- old: `FROM golang:1.25-alpine AS builder`
- new: `FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder`
- why: native cross-compile (no QEMU emulation). Build-only; no runtime effect.

---

## 5. Validation

- Verified with 1 and 2 simultaneous subscribers; **per-viewer independent adaptation** confirmed
  (one subscriber throttled/frozen while the other keeps streaming).
- Down→up resolution switching works (rare ~1 s glitch on switch).
- Both `contentHint:'motion'` and `contentHint:'text'` work — no freeze, text stays sharp.
- Android subscribers get hardware decode (single-layer rids, not SVC).
- **AV1 simulcast verified in production** (3 rids q/h/f, per-viewer adaptation, no freeze on layer
  switch, `contentHint='text'` works) with only the one-line `skipReferenceTS` addition and no
  client change.
- Server unit tests pass.
- **All encoder/codec testing was done in Chrome** (desktop publisher; Chrome desktop + Android
  subscribers). Other browsers (Firefox/Safari) were not part of this validation.

---

## 6. Suggested PR shape

1. **`client-sdk-js`** — the 3 files in §2, gated on `simulcast === true`, zero behaviour change
   for existing SVC users.
2. **`livekit`** — the 3 core files in §3. The additional changes in §4 split into their own PRs.

The client and server PRs must land together (or behind the same capability), since the SFU fixes
only trigger when the client signals `ONE_SPATIAL_LAYER_PER_STREAM`.
