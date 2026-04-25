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
- **Optional Cloudflare Warp**: Route crawler traffic through a SOCKS5 proxy for privacy.
- **Docker-ready**: Multi-stage Dockerfile with health checks and non-root user.
- **Production security**: Rate limiting, security headers (CSP, HSTS, etc.), and request tracing.

## Quick Start

### Docker (recommended)

```bash
git clone https://github.com/DuvyDev/Duvycrawl.git
cd Duvycrawl
docker compose up -d
```

The API will be available at `http://localhost:8080`.

### Binary

```bash
# Build
go build -o duvycrawl .

# Run with defaults
./duvycrawl

# Run with custom config
./duvycrawl -config configs/custom.yaml
```

## Configuration

Copy `configs/default.yaml` and adjust to your needs:

```yaml
crawler:
  workers: 400
  max_depth: 1
  seed_domains_only: true
  auto_start: true
  proxy_url: ""               # "socks5h://warp:1080" for Cloudflare Warp

storage:
  db_path: "./data/duvycrawl.db"

api:
  host: "0.0.0.0"
  port: 8080
```

### Optional: Cloudflare Warp

Uncomment the `warp` service in `docker-compose.yml` and set:

```yaml
crawler:
  proxy_url: "socks5h://warp:1080"
```

All crawler traffic will be routed through Warp while the API remains directly accessible.

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
