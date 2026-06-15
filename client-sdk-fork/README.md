# client-sdk-js fork — VP9/AV1 simulcast

This folder mirrors our **`livekit/client-sdk-js`** fork changes so the client and server forks for
VP9/AV1 simulcast live behind a single link, alongside the SFU fork in this repo and the full
write-up in [`../VP9_SIMULCAST_PR_REPORT.md`](../VP9_SIMULCAST_PR_REPORT.md).

- **Base:** `livekit/client-sdk-js` v2.19.0 (`b09b800`)
- **What it does:** lets VP9/AV1 publish as real rid-based **simulcast** (signals
  `VideoLayer.Mode.ONE_SPATIAL_LAYER_PER_STREAM` + emits standards-compliant per-encoding simulcast,
  Chrome M113+), gated on `simulcast: true`. With `simulcast` unset, behaviour is identical to
  upstream (SVC).

## Contents

- **`client-sdk-js-2.19.0-vp9simulcast.diff`** — the complete `git diff` vs the v2.19.0 base. Apply
  with `git apply` on a clean `client-sdk-js` v2.19.0 checkout.
- **`src/…`** — the 4 changed source files in their original paths, for direct viewing:
  - `src/room/participant/LocalParticipant.ts` — mode signalling, skip SVC defaults under simulcast,
    respect caller `contentHint`/`scalabilityMode`, `svcMode` flag.
  - `src/room/participant/publishUtils.ts` — `computeVideoEncodings`: real simulcast ladder +
    per-encoding `scalabilityMode`.
  - `src/room/track/LocalVideoTrack.ts` — `svcMode` / `isSvcPublish`, per-layer mode.
  - `src/room/PCTransport.ts` — `startBitrateForSVC` (start-bitrate tuning; a local deployment
    preference, not part of the simulcast change — see the report).

See [`../VP9_SIMULCAST_PR_REPORT.md`](../VP9_SIMULCAST_PR_REPORT.md) §2 for the line-by-line
explanation of each change.

> Note: this is a convenience mirror for review. The changes are small and self-contained; we're
> happy to open a formal PR against `livekit/client-sdk-js` (and `livekit/livekit` for the SFU side).
