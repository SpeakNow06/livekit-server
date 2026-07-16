# Copyright 2023 LiveKit, Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# SPEAKNOW FORK (AUDIO-NC): 3 aşamalı build. Final imaj Alpine→debian-slim'e
# taşındı çünkü libdf (DeepFilterNet3, glibc) musl'da yüklenmez. libdf imaj
# içinde derlenir + libopus + DFN3 modeli gömülür → imaj kendi kendine yeterli,
# harici .so taşımaya gerek yok. Denoise ENV ile açılır (SN_DENOISE=1); env yoksa
# imaj düz v1.11.0-fork gibi davranır (kod path'i hiç girilmez, native yüklenmez).

# --- Aşama 1: libdf.so (DeepFilterNet3 C-API, glibc) ---
FROM rust:1.75-bookworm AS dfn-builder
RUN apt-get update && apt-get install -y --no-install-recommends git && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 --branch v0.5.6 https://github.com/Rikorose/DeepFilterNet /src && \
    cd /src && \
    (cargo build --release -p deep_filter --features capi || true) && \
    if ! ls target/release/libdf*.so >/dev/null 2>&1; then \
      cd /src/libDF && cargo rustc --release --features capi --crate-type cdylib; \
    fi && \
    cp "$(ls /src/target/release/libdf*.so | head -1)" /libdf.so && \
    strip /libdf.so && ls -la /libdf.so

# --- Aşama 2: livekit-server binary (CGO_ENABLED=0, statik) ---
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder

ARG TARGETPLATFORM
ARG TARGETARCH
RUN echo building for "$TARGETPLATFORM"

WORKDIR /workspace

COPY go.mod go.mod
COPY go.sum go.sum
COPY cmd/ cmd/
COPY pkg/ pkg/
COPY test/ test/
COPY tools/ tools/
COPY version/ version/

# purego yeni bağımlılık → go.sum'ı source'a göre güncelle (internet build'de var)
RUN go mod tidy

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GO111MODULE=on go build -a -o livekit-server ./cmd/server

# --- Aşama 3: final (debian-slim + libopus + libdf + model) ---
FROM debian:trixie-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends libopus0 ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /workspace/livekit-server /livekit-server
COPY --from=dfn-builder /libdf.so /opt/speaknow/libdf.so
# DFN3 modeli (build-time indir; ~8MB)
ADD https://github.com/Rikorose/DeepFilterNet/raw/main/models/DeepFilterNet3_onnx.tar.gz /opt/speaknow/DeepFilterNet3_onnx.tar.gz

# AUDIO-NC varsayılan env yolları (SN_DENOISE=1 compose'da verilir)
ENV SN_DENOISE_LIB=/opt/speaknow/libdf.so \
    SN_DENOISE_OPUS=libopus.so.0 \
    SN_DENOISE_MODEL=/opt/speaknow/DeepFilterNet3_onnx.tar.gz

ENTRYPOINT ["/livekit-server"]
