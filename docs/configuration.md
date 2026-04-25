# Configuration

Duvycrawl is configured via a YAML file and environment variables:
- **YAML** (`configs/default.yaml`): crawler behavior (workers, depth, delays, etc.)
- **`.env` file**: deployment-specific values (proxy, tunnel token, URLs)

---

## Full Config with Defaults

```yaml
crawler:
  workers: 400
  max_depth: 3
  request_timeout: 10s
  politeness_delay: 1s
  random_delay: 500ms
  max_retries: 3
  user_agent: "Mozilla/5.0 ..."
  fallback_user_agent: "Mozilla/5.0 ...bingbot..."
  max_fallback_retries: 1
  max_page_size_kb: 512
  respect_robots: false
  seed_domains_only: false
  parallelism_per_domain: 4
  disable_cookies: false
  max_idle_conns_per_host: 150
  domain_stats_flush_interval: 30s
  auto_start: true

storage:
  db_path: "./data/duvycrawl.db"

api:
  host: "0.0.0.0"
  port: 8080

logging:
  level: "info"
  format: "text"
```

---

## Sections

### `crawler` — Crawling Engine

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `workers` | int | `400` | Concurrent crawl goroutines. Range: 1–10000. |
| `max_depth` | int | `3` | Max link depth from seed pages. `0` = seeds only. |
| `request_timeout` | duration | `10s` | HTTP request timeout. |
| `politeness_delay` | duration | `1s` | Min delay between requests to the same domain. |
| `random_delay` | duration | `500ms` | Random jitter added to politeness delay. |
| `max_retries` | int | `3` | Max retry attempts for failed requests. |
| `user_agent` | string | (see above) | Primary User-Agent. |
| `fallback_user_agent` | string | (see above) | Fallback UA for bot-blocked sites. |
| `max_fallback_retries` | int | `1` | Max fallback attempts per URL. |
| `max_page_size_kb` | int | `512` | Max page size in KB. Larger pages are truncated, not discarded. |
| `respect_robots` | bool | `false` | Whether to honor robots.txt. |
| `seed_domains_only` | bool | `false` | If `true`, only crawl within seed domains. |
| `parallelism_per_domain` | int | `4` | Max concurrent requests to the same domain. |
| `disable_cookies` | bool | `false` | Disable cookie jar. |
| `max_idle_conns_per_host` | int | `150` | Max idle keep-alive connections per host. |
| `domain_stats_flush_interval` | duration | `30s` | How often domain stats are flushed from memory to SQLite. |
| `auto_start` | bool | `true` | Automatically start crawling on launch. Set to `false` to start via API. |

> **Note**: Proxy is configured via `PROXY_URL` environment variable, not YAML. Supports `http://`, `https://`, `socks5://`, `socks5h://`. |

### `storage` — Persistence

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `db_path` | string | `./data/duvycrawl.db` | SQLite database path. |

### `api` — REST Server

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `host` | string | `0.0.0.0` | Bind address. `127.0.0.1` for localhost only. |
| `port` | int | `8080` | TCP port. |

### `logging` — Logging

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `level` | string | `info` | Min log level: `debug`, `info`, `warn`, `error`. |
| `format` | string | `text` | Output format: `text` or `json`. |

---

## Cloudflare Warp (SOCKS5)

Set `PROXY_URL` in your `.env` file:

```env
PROXY_URL=socks5://warp:1080
```

The `warp` service is already configured in `docker-compose.yml`. The crawler routes all traffic through it while the API remains directly accessible on the internal network.

Other proxy formats:

```env
# SOCKS5 with remote DNS resolution (recommended for Warp)
PROXY_URL=socks5h://warp:1080

# HTTP proxy
PROXY_URL=http://proxy.example.com:8080

# Disable proxy
PROXY_URL=
```

---

## Environment Variables

All environment variables are set in the `.env` file (see `.env.example`):

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `TUNNEL_TOKEN` | Yes | — | Cloudflare Tunnel token for public access |
| `SITE_URL` | Yes | — | Public URL of the search UI (e.g. `https://search.example.com`) |
| `CRAWLER_API` | Yes | — | Internal crawler API URL (e.g. `http://duvycrawl:8080/api/v1`) |
| `PROXY_URL` | No | `socks5://warp:1080` | Proxy URL for crawler traffic |
| `TZ` | No | `UTC` | Timezone |
| `APP_PORT` | No | `8800` | Local debug port for the search UI |
| `DDG_ENABLED` | No | `true` | Enable DuckDuckGo fallback results |
| `DDG_RESULTS` | No | `3` | Number of DDG fallback results |
| `DDG_CACHE_TTL_MINUTES` | No | `120` | DDG cache TTL in minutes |
| `NEWS_MAX_ITEMS` | No | `5` | Max items in the news section |
| `WIKIPEDIA_CARD_ENABLED` | No | `true` | Show Wikipedia summary card |
| `RESULTS_PER_PAGE` | No | `10` | Search results per page |
| `DEFAULT_LANG` | No | `en` | Default search language |

> **Note**: Proxy is configured exclusively via `PROXY_URL` environment variable — not in YAML. This separates deployment config (proxy, tunnel) from crawler behavior (workers, depth).

---

## Validation

Configuration is validated on startup. Invalid values produce a clear error:

```
fatal: loading configuration: invalid configuration: crawler.workers must be >= 1, got 0
```

---

Next: [API Reference](./api-reference.md) · [Examples](./examples.md)
