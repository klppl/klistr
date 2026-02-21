# ─── Build stage ──────────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

# Add certificates for HTTPS
RUN apk add --no-cache ca-certificates git

WORKDIR /app

# Copy dependency files first for better layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build a static binary (no CGO needed thanks to modernc.org/sqlite)
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o klistr ./cmd/klistr

# ─── Runtime stage ────────────────────────────────────────────────────────────
FROM scratch

# Copy CA certificates for HTTPS
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy the binary
COPY --from=builder /app/klistr /klistr

# Data directory for SQLite db and RSA keys
VOLUME /data
WORKDIR /data

EXPOSE 8000

ENTRYPOINT ["/klistr"]
