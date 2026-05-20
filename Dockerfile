FROM golang:1.25-bookworm AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
        gcc pkg-config curl unzip ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Install libdave headers + shared lib from prebuilt GitHub release
RUN curl -fsSL \
        https://github.com/discord/libdave/releases/download/v1.1.0/cpp/libdave-Linux-X64-boringssl.zip \
        -o /tmp/libdave.zip \
    && unzip -j /tmp/libdave.zip "include/dave/dave.h" -d /usr/local/include/ \
    && unzip -j /tmp/libdave.zip "lib/libdave.so"      -d /usr/local/lib/ \
    && rm /tmp/libdave.zip

# Minimal pkg-config descriptor so `#cgo pkg-config: dave` resolves
RUN printf 'prefix=/usr/local\nlibdir=${prefix}/lib\nincludedir=${prefix}/include\n\
Name: dave\nVersion: 1.1.0\n\
Libs: -L${libdir} -ldave -Wl,-rpath,${libdir}\nCflags: -I${includedir}\n' \
    > /usr/local/lib/pkgconfig/dave.pc

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -o /bin/ramona ./internal/bot/

# ────────────────────────────────────────────────────────
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/local/lib/libdave.so /usr/local/lib/
RUN ldconfig

COPY --from=builder /bin/ramona /bin/ramona

WORKDIR /app
CMD ["/bin/ramona"]
