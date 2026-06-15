# client-sdk-js fork — VP9/AV1 simulcast

This branch holds our fork of **`livekit/client-sdk-js` v2.19.0** for VP9/AV1 simulcast.

The SFU (server) fork is on the
**[`vp9-av1-simulcast-server`](https://github.com/SpeakNow06/livekit-server/tree/vp9-av1-simulcast-server)**
branch, and the full line-by-line write-up of both forks is
[`VP9_AV1_SIMULCAST_PR_REPORT.md`](https://github.com/SpeakNow06/livekit-server/blob/vp9-av1-simulcast-server/VP9_AV1_SIMULCAST_PR_REPORT.md)
(§2 covers the client).

- **Base:** `livekit/client-sdk-js` v2.19.0 (`b09b800`)
- **What it does:** publish VP9/AV1 as real rid-based **simulcast** — signals
  `VideoLayer.Mode.ONE_SPATIAL_LAYER_PER_STREAM` and emits standards-compliant per-encoding
  simulcast (Chrome M113+). Gated on `simulcast: true`; with `simulcast` unset, behaviour is
  identical to upstream (SVC).

## Contents

- **`client-sdk-js-2.19.0-vp9simulcast.diff`** — the complete `git diff` vs the v2.19.0 base; apply
  with `git apply` on a clean `client-sdk-js` v2.19.0 checkout.
- **`livekit-client-2.19.0-svc.umd.js`** — prebuilt UMD bundle (drop-in `<script>`, no build step).
- **`src/…`** — the 4 changed source files in their original paths:
  - `src/room/participant/LocalParticipant.ts` — mode signalling, skip SVC defaults under simulcast,
    respect caller `contentHint`/`scalabilityMode`, `svcMode` flag.
  - `src/room/participant/publishUtils.ts` — `computeVideoEncodings`: real simulcast ladder +
    per-encoding `scalabilityMode`.
  - `src/room/track/LocalVideoTrack.ts` — `svcMode` / `isSvcPublish`, per-layer mode.
  - `src/room/PCTransport.ts` — `startBitrateForSVC` (start-bitrate; a local deployment preference,
    not part of the simulcast change — see the report).

Happy to open a formal PR against `livekit/client-sdk-js` (and `livekit/livekit` for the SFU side);
they need to land together since the SFU path only triggers when the client signals the mode.
