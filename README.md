# Duvycrawl

[![Go Report Card](https://goreportcard.com/badge/github.com/DuvyDev/Duvycrawl)](https://goreportcard.com/report/github.com/DuvyDev/Duvycrawl)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

Duvycrawl is a high-performance, personal web crawler and search engine written in Go. It crawls websites, indexes content using SQLite FTS5, extracts schema.org structured data, and exposes a REST API for full-text search and crawler control.

## Features

- **Fast crawling**: Concurrent workers with per-domain rate limiting, Bloom-filter deduplication, and connection pooling.
- **Full-text search**: SQLite FTS5 with hybrid ranking (SQL pre-filter + Go re-ranking), domain diversity, and schema.org boosts.
- **Structured data extraction**: Automatic parsing of JSON-LD (schema.org) for rich results — images, authors, ratings, article types, and keywords.
- **Search API**: Filter by domain (`?domain=`), schema type (`?type=Recipe`), language (`?lang=es`), and paginate results.
- **Image search**: Indexed image metadata with alt-text search.
- **Cloudflare Warp proxy**: Route crawler traffic through a SOCKS5 proxy for privacy.
- **Cloudflare Tunnel**: Expose the search UI securely without opening ports.
- **Docker-ready**: Production docker-compose stack with Duvycrawl + [Suvy](https://github.com/DuvyDev/suvy) (search UI) + Warp + Cloudflare Tunnel.
- **Production security**: Rate limiting, security headers (CSP, HSTS, etc.), and request tracing.

## Quick Start

### Docker Compose (production stack)

The `docker-compose.yml` includes everything you need for a complete search engine deployment:

| Service | Description |
|---------|-------------|
| **duvycrawl** | Web crawler + REST API |
| **suvy** | Search engine frontend (UI) |
| **warp** | Cloudflare Warp SOCKS5 proxy |
| **cloudflared** | Cloudflare Tunnel for secure public access |

```bash
git clone https://github.com/DuvyDev/Duvycrawl.git
cd Duvycrawl

# Configure
cp .env.example .env
# Edit .env — set at least TUNNEL_TOKEN and SITE_URL

# Launch
docker compose up -d
```

The search UI will be available at your `SITE_URL` via Cloudflare Tunnel. The crawler API is accessible internally at `http://duvycrawl:8080`.

> **Note**: The local port `127.0.0.1:${APP_PORT}:8800` is only for debugging. In production, traffic flows through the Cloudflare Tunnel.

### Standalone (crawler only)

```bash
# Build
go build -o duvycrawl .

# Run with defaults
./duvycrawl

# Run with custom config
./duvycrawl -config configs/custom.yaml

# Run with proxy
PROXY_URL=socks5://localhost:1080 ./duvycrawl
```

## Configuration

Duvycrawl uses a YAML config file (`configs/default.yaml`) for crawler settings, and a `.env` file for deployment-specific values (proxy, tunnel token, URLs).

### `.env` file

Copy `.env.example` to `.env` and adjust:

| Variable | Required | Description |
|----------|----------|-------------|
| `TUNNEL_TOKEN` | Yes | Cloudflare Tunnel token |
| `SITE_URL` | Yes | Public URL of the search UI |
| `CRAWLER_API` | Yes | Internal crawler API URL (default: `http://duvycrawl:8080/api/v1`) |
| `PROXY_URL` | No | SOCKS5/HTTP proxy for crawler traffic (default: `socks5://warp:1080`) |
| `TZ` | No | Timezone (default: `UTC`) |
| `APP_PORT` | No | Local debug port for the search UI |
| `DDG_ENABLED` | No | Enable DuckDuckGo fallback results |
| `DDG_RESULTS` | No | Number of DDG fallback results |
| `DDG_CACHE_TTL_MINUTES` | No | DDG cache TTL in minutes |
| `NEWS_MAX_ITEMS` | No | Max items in the news section |
| `WIKIPEDIA_CARD_ENABLED` | No | Show Wikipedia summary card |
| `RESULTS_PER_PAGE` | No | Search results per page |
| `DEFAULT_LANG` | No | Default search language |

### YAML config (`configs/default.yaml`)

```yaml
crawler:
  workers: 400
  max_depth: 1
  seed_domains_only: true
  auto_start: true

storage:
  db_path: "./data"

api:
  host: "0.0.0.0"
  port: 8080
```

See [Configuration](./docs/configuration.md) for all options.

### Proxy

Set `PROXY_URL` in `.env` to route crawler traffic through a proxy:

```env
# Cloudflare Warp (default in docker-compose)
PROXY_URL=socks5://warp:1080

# Custom SOCKS5 with remote DNS
PROXY_URL=socks5h://proxy.example.com:1080

# HTTP proxy
PROXY_URL=http://proxy.example.com:8080

# No proxy (empty or unset)
PROXY_URL=
```

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/health` | Health check |
| `GET` | `/api/v1/search` | Full-text search |
| `GET` | `/api/v1/images/search` | Image search |
| `POST` | `/api/v1/crawl` | Enqueue URLs for crawling |
| `GET` | `/api/v1/queue` | Queue status |
| `POST` | `/api/v1/crawler/start` | Start crawler |
| `POST` | `/api/v1/crawler/stop` | Stop crawler |

### Search Examples

```bash
# Basic search
curl "http://localhost:8080/api/v1/search?q=golang&limit=10"

# Filter by domain
curl "http://localhost:8080/api/v1/search?q=recetas&domain=directoalpaladar.com"

# Filter by schema type (Recipe, NewsArticle, Product, etc.)
curl "http://localhost:8080/api/v1/search?q=pollo&type=Recipe"

# Spanish content with pagination
curl "http://localhost:8080/api/v1/search?q=github&limit=10&page=1&lang=es"
```

### Crawl URLs

```bash
# Crawl a single URL (skips if indexed within 24h)
curl -X POST http://localhost:8080/api/v1/crawl \
  -H "Content-Type: application/json" \
  -d '{"urls": ["https://github.com"]}'

# Force re-crawl
curl -X POST http://localhost:8080/api/v1/crawl \
  -H "Content-Type: application/json" \
  -d '{"urls": ["https://github.com"], "force": true}'
```

## Architecture

```
Duvycrawl/
├── main.go             # Entry point
├── internal/
│   ├── api/            # REST API (handlers, middleware)
│   ├── config/         # YAML configuration
│   ├── crawler/        # Fetcher, Parser, Engine
│   ├── frontier/       # URL queue + Bloom filter dedup
│   ├── ratelimit/      # Per-domain rate limiting
│   ├── scheduler/      # Re-crawl scheduling
│   ├── storage/        # SQLite + FTS5
│   └── seeds/          # Default seed domains
├── configs/
│   └── default.yaml    # Default configuration
├── data/               # SQLite DB + Bloom filter
├── docs/               # Documentation
├── .env.example        # Environment variable template
├── Dockerfile
└── docker-compose.yml  # Production stack
```
Duvycrawl/
├── cmd/duvycrawl/          # Entry point
├── internal/
│   ├── api/                # REST API (handlers, middleware)
│   ├── config/             # YAML configuration
│   ├── crawler/            # Fetcher, Parser, Engine
│   ├── frontier/           # URL queue + Bloom filter dedup
│   ├── ratelimit/          # Per-domain rate limiting
│   ├── scheduler/          # Re-crawl scheduling
│   ├── storage/            # SQLite + FTS5
│   └── seeds/              # Default seed domains
├── configs/
│   └── default.yaml        # Default configuration
├── data/                   # SQLite DB + Bloom filter
├── docs/                   # Documentation
├── Dockerfile
└── docker-compose.yml
```

## Tech Stack

- **Go 1.23** — core language
- **SQLite (modernc.org/sqlite)** — pure-Go, CGO-free, with FTS5 full-text search
- **goquery** — HTML parsing
- **Bloom filter** — fast URL deduplication (bits-and-blooms/bitset)

## License

Apache 2.0 — see [LICENSE](LICENSE).
