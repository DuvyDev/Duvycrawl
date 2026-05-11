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

# Build the binaries (CGO disabled for pure Go binary).
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o bin/search-api ./cmd/search-api
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o bin/crawler ./cmd/crawler

# ─── Runtime stage ───
FROM alpine:latest

WORKDIR /app

# Install ca-certificates for HTTPS and tzdata for timezone support.
RUN apk add --no-cache ca-certificates tzdata

# Copy binaries from builder.
COPY --from=builder /app/bin/* /app/bin/

# Copy default config.
COPY --from=builder /app/configs/default.yaml /app/configs/default.yaml

# Create data directory for SQLite.
RUN mkdir -p /app/data
ENV PATH="/app/bin:${PATH}"

EXPOSE 8080
EXPOSE 8081

ENTRYPOINT []
CMD ["search-api", "-config", "/app/configs/default.yaml"]
