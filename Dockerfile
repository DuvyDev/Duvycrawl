# ─── Build stage ───
FROM golang:1.26.2-alpine AS builder

WORKDIR /app

# Install git for fetching dependencies.
RUN apk add --no-cache git ca-certificates tzdata

# Copy dependency files first for better layer caching.
COPY go.mod go.sum ./
RUN go mod download

# Copy source code.
COPY . .

# Build the binary (CGO disabled for pure Go binary).
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o duvycrawl .

# ─── Runtime stage ───
FROM alpine:latest

WORKDIR /app

# Install ca-certificates for HTTPS and tzdata for timezone support.
RUN apk add --no-cache ca-certificates tzdata

# Copy binary from builder.
COPY --from=builder /app/duvycrawl /app/duvycrawl

# Copy default config.
COPY --from=builder /app/configs/default.yaml /app/configs/default.yaml

# Create data directory for SQLite.
RUN mkdir -p /app/data

EXPOSE 8080

# Health check.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/api/v1/health || exit 1

ENTRYPOINT ["/app/duvycrawl"]
CMD ["-config", "/app/configs/default.yaml"]
