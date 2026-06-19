# syntax=docker/dockerfile:1

# ─── Build stage ─────────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /server ./cmd/server

# ─── Runtime stage ───────────────────────────────────────────────────────────
# Use debian-slim (not distroless) because tesseract is a runtime dependency.
FROM debian:bookworm-slim AS runtime

# Install tesseract with Traditional Chinese + English language packs.
# Versions pinned for supply-chain integrity: this sidecar processes KYC ID
# images and must not silently pull new tesseract behaviour on rebuild.
# Versions sourced from debian:bookworm-slim (bookworm/main) on 2026-06-20.
# To update: run `apt-cache policy tesseract-ocr tesseract-ocr-chi-tra tesseract-ocr-eng`
# in debian:bookworm-slim and update all three versions together.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       tesseract-ocr=5.3.0-2 \
       tesseract-ocr-chi-tra=1:4.1.0-2 \
       tesseract-ocr-eng=1:4.1.0-2 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /server /server

# Run as a non-root user for principle of least privilege.
RUN useradd -r -s /bin/false -u 10001 ocr
USER ocr:ocr

EXPOSE 8085

# Liveness probe — process is serving.
HEALTHCHECK --interval=10s --timeout=5s --start-period=30s --retries=3 \
    CMD ["/server", "-healthcheck"]

ENTRYPOINT ["/server"]
