# livekit-server — VP9/AV1 Simulcast Fork (SpeakNow)

A minimal fork of [`livekit/livekit`](https://github.com/livekit/livekit) **v1.11.0** that makes
**VP9 and AV1 simulcast** work end-to-end. LiveKit treats VP9/AV1 as SVC-only codecs — the
`simulcast` field is ignored for them, and rid-based simulcast (`ONE_SPATIAL_LAYER_PER_STREAM`, a
separate RTP stream per spatial resolution) is unimplemented. This fork adds the missing SFU pieces
for both VP9 and AV1.

✅ **Status:** running in production since 2026-06-13. Single and multiple subscribers, per-viewer
resolution adaptation (a weak viewer drops to a lower-res rid while others stay higher), clean
up/down layer switches, `contentHint` `text` **and** `motion` both fine.

| | |
|---|---|
| This branch | `vp9-av1-simulcast-server` — the SFU (server) fork |
| Client branch | [`vp9-av1-simulcast-client`](https://github.com/SpeakNow06/livekit-server/tree/vp9-av1-simulcast-client) — the `client-sdk-js` fork |
| Base | v1.11.0 (`8ccad68`) |
| Image | `ghcr.io/speaknow06/livekit-server:v1.11.0-vp9simulcast-fix3-dynacast2-av1` |
| Turkish notes | [FORK-CHANGES.md](FORK-CHANGES.md) |

## Everything for review, in one place
For [livekit/livekit#4594](https://github.com/livekit/livekit/issues/4594) — both forks plus the
write-up live in this repo across two branches:

- **[`VP9_AV1_SIMULCAST_PR_REPORT.md`](VP9_AV1_SIMULCAST_PR_REPORT.md)** — full, line-by-line write-up
  of every change (client + server), with `file:line`, old→new, and why.
- **SFU fork — this branch (`vp9-av1-simulcast-server`)** — the server-side changes; Turkish notes in
  [FORK-CHANGES.md](FORK-CHANGES.md).
- **Client SDK fork — branch [`vp9-av1-simulcast-client`](https://github.com/SpeakNow06/livekit-server/tree/vp9-av1-simulcast-client)** —
  our `livekit/client-sdk-js` v2.19.0 changes (full diff + changed files + prebuilt UMD bundle).
- **AV1** works as real simulcast via the same mechanism — one extra line (`skipReferenceTS` in the
  AV1 branch), no client change. Verified in production.

## Why VP9/AV1 simulcast (vs SVC)?
Per-viewer **resolution** adaptation at **constant fps** (weak viewer → lower-res rid; fps stays),
plus Android **hardware decode** of single-spatial rids. VP9 **SVC** doesn't give this
(screen-content/`text` collapses spatial to one; Android falls back to software decode); VP9
**simulcast** does, but it's unsupported upstream.

## The bug
A VP9-simulcast subscriber works ~60 s (stable on its initial layer), then **freezes the instant
the allocator first switches layers**: the forwarder enters a constant `processSourceSwitch` loop
(~60/s), output timestamps freeze (`extNextTS = extLastTS + 1`), NACK/PLI storm follows. H.264/VP8
simulcast is unaffected.

## Root causes & fixes

### 1. `pkg/sfu/receiver_base.go` — all rids collapse to spatial layer 0  *(the main one)*
```go
spatialLayer := layer
if extPkt.Spatial >= 0 {            // SVC: take spatial from the packet
    spatialLayer = extPkt.Spatial
}
```
Each VP9 simulcast rid is single-spatial, so its Dependency-Descriptor `SpatialId == 0`. The
override maps **every** rid to `spatialLayer 0`; the forwarder can no longer tell rids apart and
alternates between their SSRCs on every packet. (H.264/VP8 are unaffected: `extPkt.Spatial == -1`,
so the override never fires — which is why they work and VP9 doesn't.)
**Fix:** apply the override only for SVC (single uptrack → `layer == 0`).

### 2. `pkg/sfu/forwarder.go` — cross-layer timestamp offset never establishes
`getRefLayerRTPTimestamp` derives the inter-layer RTP-timestamp offset from RTCP Sender Reports;
for VP9 simulcast it never becomes valid (`tsOffset` stays 0), so every switch fails with
`switch point too far behind`. **Fix:** treat VP9 simulcast like
`ONE_SPATIAL_LAYER_PER_STREAM_INCOMPLETE_RTCP_SR` (set `skipReferenceTS`) and fall back to the
elapsed-time `extExpectedTS`.

### 3. `pkg/sfu/forwarder.go` — wrong selector
VP9 simulcast was routed to the `DependencyDescriptor` selector (built for one-stream SVC), which
reports a switch on nearly every frame. **Fix:** use the `Simulcast` selector (keyframe-based,
built for separate-stream simulcast — same path as VP8/H.264).

### 4. `pkg/sfu/buffer/dependencydescriptorparser.go` — frame drops with frequent keyframes
`structureExtFrameNum` advances on **every** structure-bearing keyframe, even when `StructureId` is
unchanged, so reordered/retransmitted frames are dropped as "earlier than current structure"
(severe with screen-content / `contentHint=text`). **Fix:** advance the drop threshold (new
`structureChangeExtFrameNum`) only on an actual `StructureId` change; leave `ExtKeyFrameNum` alone.

### Plus (not VP9-specific)
- `pkg/service/roommanager.go` — ipv6 TURN-URL fix: stock v1.11.0 emits IPv6 into the TURN URL
  without brackets → client `setConfiguration` error. Use `NodeIP.V4` only.
- `Dockerfile` — `--platform=$BUILDPLATFORM` for fast native cross-compile.

## Client SDK also needs a fork
VP9 simulcast must be **enabled on the client** too — upstream `livekit-client` forces VP9 into
SVC. A separate `livekit-client` fork (2.19.0, branch `svc-patch`) signals
**`SimulcastCodec.videoLayerMode = ONE_SPATIAL_LAYER_PER_STREAM`** (without which the server assumes
SVC), sets per-encoding `scalabilityMode` (required on Chrome M113+), and skips the SVC defaults in
simulcast mode. App publish config: `videoCodec:'vp9', simulcast:true, scalabilityMode:'L1T2'`.
The server fork (this repo) **and** the client SDK fork are both required.

## Build
```bash
docker build --platform linux/amd64 -t ghcr.io/speaknow06/livekit-server:TAG .
docker push ghcr.io/speaknow06/livekit-server:TAG
```

Every LiveKit upgrade requires re-applying these 5 changes onto the new base. A `spatial-drop-first`
allocator experiment was tried and **reverted** (stock allocator + the `Simulcast` selector are
sufficient). Reported upstream: [livekit/livekit#4594](https://github.com/livekit/livekit/issues/4594) (PR offered).

---

_Below is the upstream LiveKit README._

<!--BEGIN_BANNER_IMAGE-->

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="/.github/banner_dark.png">
  <source media="(prefers-color-scheme: light)" srcset="/.github/banner_light.png">
  <img style="width:100%;" alt="The LiveKit icon, the name of the repository and some sample code in the background." src="https://raw.githubusercontent.com/livekit/livekit/main/.github/banner_light.png">
</picture>

<!--END_BANNER_IMAGE-->

# LiveKit: Real-time video, audio and data for developers

[LiveKit](https://livekit.io) is an open source project that provides scalable, multi-user conferencing based on WebRTC.
It's designed to provide everything you need to build real-time video audio data capabilities in your applications.

LiveKit's server is written in Go, using the awesome [Pion WebRTC](https://github.com/pion/webrtc) implementation.

[![GitHub stars](https://img.shields.io/github/stars/livekit/livekit?style=social&label=Star&maxAge=2592000)](https://github.com/livekit/livekit/stargazers/)
[![Slack community](https://img.shields.io/endpoint?url=https%3A%2F%2Flivekit.io%2Fbadges%2Fslack)](https://livekit.io/join-slack)
[![Twitter Follow](https://img.shields.io/twitter/follow/livekit)](https://twitter.com/livekit)
[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/livekit/livekit)
[![GitHub release (latest SemVer)](https://img.shields.io/github/v/release/livekit/livekit)](https://github.com/livekit/livekit/releases/latest)
[![GitHub Workflow Status](https://img.shields.io/github/actions/workflow/status/livekit/livekit/buildtest.yaml?branch=master)](https://github.com/livekit/livekit/actions/workflows/buildtest.yaml)
[![License](https://img.shields.io/github/license/livekit/livekit)](https://github.com/livekit/livekit/blob/master/LICENSE)

## Features

-   Scalable, distributed WebRTC SFU (Selective Forwarding Unit)
-   Modern, full-featured client SDKs
-   Built for production, supports JWT authentication
-   Robust networking and connectivity, UDP/TCP/TURN
-   Easy to deploy: single binary, Docker or Kubernetes
-   Advanced features including:
    -   [speaker detection](https://docs.livekit.io/home/client/tracks/subscribe/#speaker-detection)
    -   [simulcast](https://docs.livekit.io/home/client/tracks/publish/#video-simulcast)
    -   [end-to-end optimizations](https://blog.livekit.io/livekit-one-dot-zero/)
    -   [selective subscription](https://docs.livekit.io/home/client/tracks/subscribe/#selective-subscription)
    -   [moderation APIs](https://docs.livekit.io/home/server/managing-participants/)
    -   end-to-end encryption
    -   SVC codecs (VP9, AV1)
    -   [webhooks](https://docs.livekit.io/home/server/webhooks/)
    -   [distributed and multi-region](https://docs.livekit.io/home/self-hosting/distributed/)

## Documentation & Guides

https://docs.livekit.io

## Live Demos

-   [LiveKit Meet](https://meet.livekit.io) ([source](https://github.com/livekit-examples/meet))
-   [Spatial Audio](https://spatial-audio-demo.livekit.io/) ([source](https://github.com/livekit-examples/spatial-audio))
-   Livestreaming from OBS Studio ([source](https://github.com/livekit-examples/livestream))
-   [AI voice assistant using ChatGPT](https://livekit.io/kitt) ([source](https://github.com/livekit-examples/kitt))

## Ecosystem

-   [Agents](https://github.com/livekit/agents): build real-time multimodal AI applications with programmable backend participants
-   [Egress](https://github.com/livekit/egress): record or multi-stream rooms and export individual tracks
-   [Ingress](https://github.com/livekit/ingress): ingest streams from external sources like RTMP, WHIP, HLS, or OBS Studio

## SDKs & Tools

### Client SDKs

Client SDKs enable your frontend to include interactive, multi-user experiences.

<table>
  <tr>
    <th>Language</th>
    <th>Repo</th>
    <th>
        <a href="https://docs.livekit.io/home/client/events/#declarative-ui" target="_blank" rel="noopener noreferrer">Declarative UI</a>
    </th>
    <th>Links</th>
  </tr>
  <!-- BEGIN Template
  <tr>
    <td>Language</td>
    <td>
      <a href="" target="_blank" rel="noopener noreferrer"></a>
    </td>
    <td></td>
    <td></td>
  </tr>
  END -->
  <!-- JavaScript -->
  <tr>
    <td>JavaScript (TypeScript)</td>
    <td>
      <a href="https://github.com/livekit/client-sdk-js" target="_blank" rel="noopener noreferrer">client-sdk-js</a>
    </td>
    <td>
      <a href="https://github.com/livekit/livekit-react" target="_blank" rel="noopener noreferrer">React</a>
    </td>
    <td>
      <a href="https://docs.livekit.io/client-sdk-js/" target="_blank" rel="noopener noreferrer">docs</a>
      |
      <a href="https://github.com/livekit/client-sdk-js/tree/main/example" target="_blank" rel="noopener noreferrer">JS example</a>
      |
      <a href="https://github.com/livekit/client-sdk-js/tree/main/example" target="_blank" rel="noopener noreferrer">React example</a>
    </td>
  </tr>
  <!-- Swift -->
  <tr>
    <td>Swift (iOS / MacOS)</td>
    <td>
      <a href="https://github.com/livekit/client-sdk-swift" target="_blank" rel="noopener noreferrer">client-sdk-swift</a>
    </td>
    <td>Swift UI</td>
    <td>
      <a href="https://docs.livekit.io/client-sdk-swift/" target="_blank" rel="noopener noreferrer">docs</a>
      |
      <a href="https://github.com/livekit/client-example-swift" target="_blank" rel="noopener noreferrer">example</a>
    </td>
  </tr>
  <!-- Kotlin -->
  <tr>
    <td>Kotlin (Android)</td>
    <td>
      <a href="https://github.com/livekit/client-sdk-android" target="_blank" rel="noopener noreferrer">client-sdk-android</a>
    </td>
    <td>Compose</td>
    <td>
      <a href="https://docs.livekit.io/client-sdk-android/index.html" target="_blank" rel="noopener noreferrer">docs</a>
      |
      <a href="https://github.com/livekit/client-sdk-android/tree/main/sample-app/src/main/java/io/livekit/android/sample" target="_blank" rel="noopener noreferrer">example</a>
      |
      <a href="https://github.com/livekit/client-sdk-android/tree/main/sample-app-compose/src/main/java/io/livekit/android/composesample" target="_blank" rel="noopener noreferrer">Compose example</a>
    </td>
  </tr>
<!-- Flutter -->
  <tr>
    <td>Flutter (all platforms)</td>
    <td>
      <a href="https://github.com/livekit/client-sdk-flutter" target="_blank" rel="noopener noreferrer">client-sdk-flutter</a>
    </td>
    <td>native</td>
    <td>
      <a href="https://docs.livekit.io/client-sdk-flutter/" target="_blank" rel="noopener noreferrer">docs</a>
      |
      <a href="https://github.com/livekit/client-sdk-flutter/tree/main/example" target="_blank" rel="noopener noreferrer">example</a>
    </td>
  </tr>
  <!-- Unity -->
  <tr>
    <td>Unity WebGL</td>
    <td>
      <a href="https://github.com/livekit/client-sdk-unity-web" target="_blank" rel="noopener noreferrer">client-sdk-unity-web</a>
    </td>
    <td></td>
    <td>
      <a href="https://livekit.github.io/client-sdk-unity-web/" target="_blank" rel="noopener noreferrer">docs</a>
    </td>
  </tr>
  <!-- React Native -->
  <tr>
    <td>React Native (beta)</td>
    <td>
      <a href="https://github.com/livekit/client-sdk-react-native" target="_blank" rel="noopener noreferrer">client-sdk-react-native</a>
    </td>
    <td>native</td>
    <td></td>
  </tr>
  <!-- Rust -->
  <tr>
    <td>Rust</td>
    <td>
      <a href="https://github.com/livekit/client-sdk-rust" target="_blank" rel="noopener noreferrer">client-sdk-rust</a>
    </td>
    <td></td>
    <td></td>
  </tr>
</table>

### Server SDKs

Server SDKs enable your backend to generate [access tokens](https://docs.livekit.io/home/get-started/authentication/),
call [server APIs](https://docs.livekit.io/reference/server/server-apis/), and
receive [webhooks](https://docs.livekit.io/home/server/webhooks/). In addition, the Go SDK includes client capabilities,
enabling you to build automations that behave like end-users.

| Language                | Repo                                                                                    | Docs                                                        |
| :---------------------- | :-------------------------------------------------------------------------------------- | :---------------------------------------------------------- |
| Go                      | [server-sdk-go](https://github.com/livekit/server-sdk-go)                               | [docs](https://pkg.go.dev/github.com/livekit/server-sdk-go) |
| JavaScript (TypeScript) | [server-sdk-js](https://github.com/livekit/server-sdk-js)                               | [docs](https://docs.livekit.io/server-sdk-js/)              |
| Ruby                    | [server-sdk-ruby](https://github.com/livekit/server-sdk-ruby)                           |                                                             |
| Java (Kotlin)           | [server-sdk-kotlin](https://github.com/livekit/server-sdk-kotlin)                       |                                                             |
| Python (community)      | [python-sdks](https://github.com/livekit/python-sdks)                                   |                                                             |
| PHP (community)         | [agence104/livekit-server-sdk-php](https://github.com/agence104/livekit-server-sdk-php) |                                                             |

### Tools

-   [CLI](https://github.com/livekit/livekit-cli) - command line interface & load tester
-   [Docker image](https://hub.docker.com/r/livekit/livekit-server)
-   [Helm charts](https://github.com/livekit/livekit-helm)

## Install

> [!TIP]
> We recommend installing [LiveKit CLI](https://github.com/livekit/livekit-cli) along with the server. It lets you access
> server APIs, create tokens, and generate test traffic.

The following will install LiveKit's media server:

### MacOS

```shell
brew install livekit
```

### Linux

```shell
curl -sSL https://get.livekit.io | bash
```

### Windows

Download the [latest release here](https://github.com/livekit/livekit/releases/latest)

## Getting Started

### Starting LiveKit

Start LiveKit in development mode by running `livekit-server --dev`. It'll use a placeholder API key/secret pair.

```
API Key: devkey
API Secret: secret
```

To customize your setup for production, refer to our [deployment docs](https://docs.livekit.io/deploy/)

### Creating access token

A user connecting to a LiveKit room requires an [access token](https://docs.livekit.io/home/get-started/authentication/#creating-a-token). Access
tokens (JWT) encode the user's identity and the room permissions they've been granted. You can generate a token with our
CLI:

```shell
lk token create \
    --api-key devkey --api-secret secret \
    --join --room my-first-room --identity user1 \
    --valid-for 24h
```

### Test with example app

Head over to our [example app](https://example.livekit.io) and enter a generated token to connect to your LiveKit
server. This app is built with our [React SDK](https://github.com/livekit/livekit-react).

Once connected, your video and audio are now being published to your new LiveKit instance!

### Simulating a test publisher

```shell
lk room join \
    --url ws://localhost:7880 \
    --api-key devkey --api-secret secret \
    --identity bot-user1 \
    --publish-demo \
    my-first-room
```

This command publishes a looped demo video to a room. Due to how the video clip was encoded (keyframes every 3s),
there's a slight delay before the browser has sufficient data to begin rendering frames. This is an artifact of the
simulation.

## Deployment

### Use LiveKit Cloud

LiveKit Cloud is the fastest and most reliable way to run LiveKit. Every project gets free monthly bandwidth and
transcoding credits.

Sign up for [LiveKit Cloud](https://cloud.livekit.io/).

### Self-host

Read our [deployment docs](https://docs.livekit.io/transport/self-hosting/) for more information.

## Building from source

Pre-requisites:

-   Go 1.23+ is installed
-   GOPATH/bin is in your PATH

Then run

```shell
git clone https://github.com/livekit/livekit
cd livekit
./bootstrap.sh
mage
```

## Contributing

We welcome your contributions toward improving LiveKit! Please join us
[on Slack](http://livekit.io/join-slack) to discuss your ideas and/or PRs.

## License

LiveKit server is licensed under Apache License v2.0.

<!--BEGIN_REPO_NAV-->
<br/><table>
<thead><tr><th colspan="2">LiveKit Ecosystem</th></tr></thead>
<tbody>
<tr><td>Agents SDKs</td><td><a href="https://github.com/livekit/agents">Python</a> · <a href="https://github.com/livekit/agents-js">Node.js</a></td></tr><tr></tr>
<tr><td>LiveKit SDKs</td><td><a href="https://github.com/livekit/client-sdk-js">Browser</a> · <a href="https://github.com/livekit/client-sdk-swift">Swift</a> · <a href="https://github.com/livekit/client-sdk-android">Android</a> · <a href="https://github.com/livekit/client-sdk-flutter">Flutter</a> · <a href="https://github.com/livekit/client-sdk-react-native">React Native</a> · <a href="https://github.com/livekit/rust-sdks">Rust</a> · <a href="https://github.com/livekit/node-sdks">Node.js</a> · <a href="https://github.com/livekit/python-sdks">Python</a> · <a href="https://github.com/livekit/client-sdk-unity">Unity</a> · <a href="https://github.com/livekit/client-sdk-unity-web">Unity (WebGL)</a> · <a href="https://github.com/livekit/client-sdk-esp32">ESP32</a> · <a href="https://github.com/livekit/client-sdk-cpp">C++</a></td></tr><tr></tr>
<tr><td>Starter Apps</td><td><a href="https://github.com/livekit-examples/agent-starter-python">Python Agent</a> · <a href="https://github.com/livekit-examples/agent-starter-node">TypeScript Agent</a> · <a href="https://github.com/livekit-examples/agent-starter-react">React App</a> · <a href="https://github.com/livekit-examples/agent-starter-swift">SwiftUI App</a> · <a href="https://github.com/livekit-examples/agent-starter-android">Android App</a> · <a href="https://github.com/livekit-examples/agent-starter-flutter">Flutter App</a> · <a href="https://github.com/livekit-examples/agent-starter-react-native">React Native App</a> · <a href="https://github.com/livekit-examples/agent-starter-embed">Web Embed</a></td></tr><tr></tr>
<tr><td>UI Components</td><td><a href="https://github.com/livekit/components-js">React</a> · <a href="https://github.com/livekit/components-android">Android Compose</a> · <a href="https://github.com/livekit/components-swift">SwiftUI</a> · <a href="https://github.com/livekit/components-flutter">Flutter</a></td></tr><tr></tr>
<tr><td>Server APIs</td><td><a href="https://github.com/livekit/node-sdks">Node.js</a> · <a href="https://github.com/livekit/server-sdk-go">Golang</a> · <a href="https://github.com/livekit/server-sdk-ruby">Ruby</a> · <a href="https://github.com/livekit/server-sdk-kotlin">Java/Kotlin</a> · <a href="https://github.com/livekit/python-sdks">Python</a> · <a href="https://github.com/livekit/rust-sdks">Rust</a> · <a href="https://github.com/agence104/livekit-server-sdk-php">PHP (community)</a> · <a href="https://github.com/pabloFuente/livekit-server-sdk-dotnet">.NET (community)</a></td></tr><tr></tr>
<tr><td>Resources</td><td><a href="https://docs.livekit.io">Docs</a> · <a href="https://docs.livekit.io/mcp">Docs MCP Server</a> · <a href="https://github.com/livekit/livekit-cli">CLI</a> · <a href="https://cloud.livekit.io">LiveKit Cloud</a></td></tr><tr></tr>
<tr><td>LiveKit Server OSS</td><td><b>LiveKit server</b> · <a href="https://github.com/livekit/egress">Egress</a> · <a href="https://github.com/livekit/ingress">Ingress</a> · <a href="https://github.com/livekit/sip">SIP</a></td></tr><tr></tr>
<tr><td>Community</td><td><a href="https://community.livekit.io">Developer Community</a> · <a href="https://livekit.io/join-slack">Slack</a> · <a href="https://x.com/livekit">X</a> · <a href="https://www.youtube.com/@livekit_io">YouTube</a></td></tr>
</tbody>
</table>
<!--END_REPO_NAV-->
