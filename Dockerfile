FROM ubuntu:24.04 AS builder

# amd64 / arm64 — set automatically by BuildKit for the target platform
ARG TARGETARCH

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        g++-14 pkg-config curl unzip ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Install Go 1.25.1
RUN curl -fsSL "https://go.dev/dl/go1.25.1.linux-${TARGETARCH}.tar.gz" | tar -C /usr/local -xz
ENV PATH=/usr/local/go/bin:$PATH
ENV GOPATH=/go

# Install libdave headers + shared lib from prebuilt GitHub release.
# Release asset naming: amd64 → Linux-X64, arm64 → Linux-ARM64.
RUN case "${TARGETARCH}" in \
        amd64) DAVE_ARCH=X64 ;; \
        arm64) DAVE_ARCH=ARM64 ;; \
        *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && curl -fsSL \
        "https://github.com/discord/libdave/releases/download/v1.1.0/cpp/libdave-Linux-${DAVE_ARCH}-boringssl.zip" \
        -o /tmp/libdave.zip \
    && unzip -j /tmp/libdave.zip "include/dave/dave.h" -d /usr/local/include/ \
    && unzip -j /tmp/libdave.zip "lib/libdave.so"      -d /usr/local/lib/ \
    && rm /tmp/libdave.zip

# Minimal pkg-config descriptor so `#cgo pkg-config: dave` resolves
RUN mkdir -p /usr/share/pkgconfig \
    && printf 'Name: dave\nDescription: libdave E2EE\nVersion: 1.1.0\nCflags: -I/usr/local/include\nLibs: -L/usr/local/lib -ldave -Wl,-rpath,/usr/local/lib\n' \
        > /usr/share/pkgconfig/dave.pc \
    && pkg-config --cflags dave

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 CC=gcc-14 go build -o /bin/ramona ./internal/bot/

# ────────────────────────────────────────────────────────
FROM ubuntu:24.04

ARG TARGETARCH

ENV DEBIAN_FRONTEND=noninteractive

# g++-14 is installed solely to pull in libstdc++6 >= 14 (GLIBCXX_3.4.32)
# which libdave.so requires at runtime
RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg ca-certificates g++-14 curl unzip \
    && rm -rf /var/lib/apt/lists/*

# deno: JS runtime required by modern yt-dlp to solve YouTube's n-parameter
# challenge — without it audio formats are missing entirely.
RUN case "${TARGETARCH}" in \
        amd64) DENO_ARCH=x86_64 ;; \
        arm64) DENO_ARCH=aarch64 ;; \
        *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && curl -fsSL "https://github.com/denoland/deno/releases/latest/download/deno-${DENO_ARCH}-unknown-linux-gnu.zip" \
        -o /tmp/deno.zip \
    && unzip -j /tmp/deno.zip deno -d /usr/local/bin/ \
    && chmod +x /usr/local/bin/deno \
    && rm /tmp/deno.zip

COPY --from=builder /usr/local/lib/libdave.so /usr/local/lib/
RUN ldconfig

COPY --from=builder /bin/ramona /bin/ramona

WORKDIR /app
CMD ["/bin/ramona"]
