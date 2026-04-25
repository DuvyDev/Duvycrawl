# Getting Started

Quick guide to get Duvycrawl running.

---

## Requirements

- **Go 1.23+**
- No external databases or services required

## Installation

```bash
# Clone the repository
git clone https://github.com/DuvyDev/Duvycrawl.git
cd Duvycrawl

# Download dependencies
go mod tidy

# Build the binary
go build -o duvycrawl .
```

## Docker (Recommended)

```bash
docker compose up -d
```

The API will be available at `http://localhost:8080`.

## First Start

```bash
# Run with defaults
./duvycrawl

# Run with custom config
./duvycrawl -config configs/custom.yaml

# Don't auto-start the crawler (API only)
./duvycrawl -no-start
```

On first start, Duvycrawl:

1. **Creates the SQLite database** at `./data/duvycrawl.db`
2. **Registers 18 seed domains** (Reddit, GitHub, Wikipedia, etc.)
3. **Enqueues initial URLs** from each seed
4. **Starts the crawler** automatically if `auto_start: true` in config
5. **Starts the API** at `http://localhost:8080`

## Verify It Works

```bash
# Health check
curl http://localhost:8080/api/v1/health

# Check stats
curl http://localhost:8080/api/v1/stats

# Check crawler status
curl http://localhost:8080/api/v1/crawler/status

# Search
curl "http://localhost:8080/api/v1/search?q=golang&limit=10"
```

## CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `configs/default.yaml` | Path to YAML config file |
| `-no-start` | `false` | Override `auto_start` in config; do not start crawler |

## Stop Duvycrawl

Press **Ctrl+C** in the terminal. Shutdown is graceful:

1. Workers finish the current page
2. Database connection is closed
3. HTTP server stops

No data is lost during shutdown.

---

Next: [Configuration](./configuration.md) · [API Reference](./api-reference.md)
