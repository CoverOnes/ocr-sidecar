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
# Pinning versions is not required for an internal dev sidecar; apt security
# updates are applied on image rebuild (intended).
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       tesseract-ocr \
       tesseract-ocr-chi-tra \
       tesseract-ocr-eng \
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
