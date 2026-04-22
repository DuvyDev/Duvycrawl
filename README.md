# Duvycrawl 🕷️

A fast, efficient web crawler written in Go, designed for powering a personal search engine.

## Features

- **Concurrent crawling** with configurable worker pool
- **Full-text search** powered by SQLite FTS5
- **REST API** for frontend integration and remote control
- **Priority-based URL frontier** with domain fairness
- **robots.txt** compliance
- **Per-domain rate limiting** for polite crawling
- **Automatic re-crawling** with configurable freshness policies
- **Seed domains** list for bootstrapping with priority sites
- **Graceful shutdown** — never loses data mid-crawl
- **Zero external infrastructure** — everything runs in a single binary with SQLite

## Quick Start

### Prerequisites

- Go 1.22+ (developed with Go 1.26)

### Build & Run

```bash
# Clone the repository
git clone https://github.com/DuvyDev/Duvycrawl.git
cd Duvycrawl

# Build
make build

# Run (uses configs/default.yaml by default)
make run
```

### Configuration

Edit `configs/default.yaml` to customize:

```yaml
crawler:
  workers: 10              # Concurrent workers
  max_depth: 3             # Link depth from seeds
  politeness_delay: 1s     # Delay between same-domain requests
  respect_robots: true     # Honor robots.txt

storage:
  db_path: "./data/duvycrawl.db"

api:
  port: 8080
```

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/health` | Health check |
| `GET` | `/api/v1/search?q=...&page=1&limit=10` | Full-text search |
| `GET` | `/api/v1/pages/{id}` | Get page details |
| `GET` | `/api/v1/stats` | Crawler statistics |
| `POST` | `/api/v1/crawl` | Queue URL(s) for crawling |
| `GET` | `/api/v1/queue` | Queue status |
| `GET` | `/api/v1/seeds` | List seed domains |
| `POST` | `/api/v1/seeds` | Add seed domain |
| `DELETE` | `/api/v1/seeds/{domain}` | Remove seed domain |
| `POST` | `/api/v1/crawler/start` | Start the crawler |
| `POST` | `/api/v1/crawler/stop` | Stop the crawler |
| `GET` | `/api/v1/crawler/status` | Crawler status |

### Example: Search

```bash
curl "http://localhost:8080/api/v1/search?q=golang+tutorial&limit=10"
```

### Example: Queue a URL

```bash
curl -X POST http://localhost:8080/api/v1/crawl \
  -H "Content-Type: application/json" \
  -d '{"urls": ["https://example.com"]}'
```

## Architecture

```
cmd/duvycrawl/         → Entry point
internal/
  ├── config/          → YAML configuration
  ├── crawler/         → Engine, fetcher, parser, robots.txt
  ├── frontier/        → Priority URL queue
  ├── ratelimit/       → Per-domain rate limiting
  ├── storage/         → SQLite + FTS5 persistence
  ├── scheduler/       → Re-crawl scheduling
  ├── api/             → REST API server
  └── seeds/           → Default seed domains
```

## License

MIT
