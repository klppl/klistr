# ─── Build stage ──────────────────────────────────────────────────────────────
# $BUILDPLATFORM is always the runner's native platform (amd64).
# Go cross-compiles to $TARGETARCH, so no QEMU emulation is needed.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY go.mod go.sum ./

# Module cache persists across builds — no re-downloading on unchanged dependencies.
RUN --mount=type=cache,target=/root/go/pkg/mod \
    go mod download

COPY . .

# Build cache persists across builds — only changed packages are recompiled.
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-w -s" -o klistr ./cmd/klistr

# ─── Runtime stage ────────────────────────────────────────────────────────────
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/klistr /klistr

VOLUME /data
WORKDIR /data

EXPOSE 8000

ENTRYPOINT ["/klistr"]
