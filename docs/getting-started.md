# Getting Started

Quick guide to get Duvycrawl running.

---

## Requirements

- **Docker** (recommended for production)
- **Go 1.23+** (for building from source)
- No external databases or services required

---

## Production Deployment (Docker Compose)

The `docker-compose.yml` provides a complete, ready-to-deploy search engine stack:

| Service | Image | Description |
|---------|-------|-------------|
| **cloudflared** | `cloudflare/cloudflared:latest` | Cloudflare Tunnel — exposes Suvy to the internet securely |
| **warp** | `caomingjun/warp:latest` | SOCKS5 proxy — routes crawler traffic through Cloudflare Warp |
| **duvycrawl** | `ghcr.io/duvydev/duvycrawl:latest` | Web crawler + REST API |
| **suvy** | `ghcr.io/duvydev/suvy:latest` | Search engine frontend (UI) |

### Network topology

```
Internet ──▶ cloudflared ──▶ suvy ──▶ duvycrawl (API)
                                      │
                                 warp (SOCKS5)
                                      │
                                 Internet (crawled sites)
```

- **cloudflared** → **suvy**: on `cloudflared-proxy` network
- **suvy** → **duvycrawl**: on `suvynet` network (internal API calls)
- **duvycrawl** → **warp**: on `duvycrawl` network (SOCKS5 proxy)
- The API is **not** exposed to the internet — only Suvy can reach it

### Steps

```bash
# 1. Clone the repository
git clone https://github.com/DuvyDev/Duvycrawl.git
cd Duvycrawl

# 2. Create your .env file
cp .env.example .env
```

Edit `.env` and set the required values:

```env
# Required
TUNNEL_TOKEN=your_cloudflare_tunnel_token
SITE_URL=https://search.yourdomain.com
CRAWLER_API=http://duvycrawl:8080/api/v1

# Proxy (enabled by default via Warp)
PROXY_URL=socks5://warp:1080
```

```bash
# 3. Launch the stack
docker compose up -d

# 4. Verify
docker compose ps          # all services should be "healthy"
docker compose logs -f     # watch logs
```

Your search engine will be available at `SITE_URL` (e.g. `https://search.yourdomain.com`).

> **Local debugging**: Suvy is also exposed at `127.0.0.1:${APP_PORT}` (default 8800). This is for development only — in production, use the Cloudflare Tunnel.

### .env variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `TUNNEL_TOKEN` | Yes | — | Cloudflare Tunnel token |
| `SITE_URL` | Yes | — | Public URL of the search UI |
| `CRAWLER_API` | Yes | — | Internal crawler API URL |
| `PROXY_URL` | No | `socks5://warp:1080` | Proxy for crawler traffic |
| `TZ` | No | `UTC` | Timezone |
| `APP_PORT` | No | `8800` | Local debug port for Suvy |
| `DDG_ENABLED` | No | `true` | Enable DuckDuckGo fallback |
| `DDG_RESULTS` | No | `3` | Number of DDG fallback results |
| `DDG_CACHE_TTL_MINUTES` | No | `120` | DDG cache TTL |
| `NEWS_MAX_ITEMS` | No | `5` | Max news section items |
| `WIKIPEDIA_CARD_ENABLED` | No | `true` | Show Wikipedia card |
| `RESULTS_PER_PAGE` | No | `10` | Results per page |
| `DEFAULT_LANG` | No | `en` | Default search language |

---

## Standalone (Binary)

If you only need the crawler (no Suvy, no Docker):

```bash
# Build
go build -o duvycrawl .

# Run with defaults
./duvycrawl

# Run with custom config
./duvycrawl -config configs/custom.yaml

# Run with SOCKS5 proxy
PROXY_URL=socks5://localhost:1080 ./duvycrawl

# Don't auto-start the crawler (API only)
./duvycrawl -no-start
```

## First Start

On first start, Duvycrawl:

1. **Creates the SQLite database** at `./data/duvycrawl.db`
2. **Registers seed domains** (Reddit, GitHub, Wikipedia, etc.)
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

For Docker: `docker compose down`

---

Next: [Configuration](./configuration.md) · [API Reference](./api-reference.md)
