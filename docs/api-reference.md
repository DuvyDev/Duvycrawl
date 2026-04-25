# API Reference

Duvycrawl exposes a REST API at `http://localhost:8080` (configurable).
All endpoints return JSON with `Content-Type: application/json`.

---

## Endpoint Table

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/health` | Health check |
| `GET` | `/api/v1/search` | Full-text search |
| `GET` | `/api/v1/images/search` | Image search |
| `GET` | `/api/v1/pages/{id}` | Page detail by ID |
| `GET` | `/api/v1/pages/lookup` | Lookup page by exact URL |
| `GET` | `/api/v1/stats` | General statistics |
| `POST` | `/api/v1/crawl` | Enqueue URLs for crawling |
| `GET` | `/api/v1/queue` | Queue status |
| `GET` | `/api/v1/seeds` | List seed domains |
| `POST` | `/api/v1/seeds` | Add seed domain |
| `DELETE` | `/api/v1/seeds/{domain}` | Remove seed domain |
| `POST` | `/api/v1/crawler/start` | Start crawler |
| `POST` | `/api/v1/crawler/stop` | Stop crawler |
| `GET` | `/api/v1/crawler/status` | Crawler status |

---

## Endpoints in Detail

### `GET /api/v1/health`

Simple health check. Useful for monitoring.

**Response** `200 OK`
```json
{
  "status": "ok",
  "timestamp": "2026-04-21T23:35:42Z",
  "version": "1.0.0"
}
```

---

### `GET /api/v1/search`

Full-text search over indexed pages using SQLite FTS5.

**Query Parameters**

| Param | Type | Default | Description |
|-------|------|---------|-------------|
| `q` | string | *required* | Search text |
| `page` | int | `1` | Page number |
| `limit` | int | `10` | Results per page (max: 100) |
| `lang` | string | `""` | Boost results in this language (e.g. `es`, `en`) |
| `domain` | string | `""` | Filter by domain (e.g. `github.com`) |
| `type` | string | `""` | Filter by schema.org type (e.g. `Recipe`, `NewsArticle`) |

**Response** `200 OK`
```json
{
  "query": "golang tutorial",
  "total": 42,
  "page": 1,
  "limit": 10,
  "domain": "",
  "type": "",
  "results": [
    {
      "id": 1234,
      "url": "https://go.dev/doc/tutorial/",
      "title": "Tutorial - The Go Programming Language",
      "description": "A tutorial for the Go programming language.",
      "snippet": "...learn <mark>Golang</mark> with this comprehensive <mark>tutorial</mark>...",
      "domain": "go.dev",
      "language": "en",
      "crawled_at": "2026-04-21T20:00:00Z",
      "updated_at": "2026-04-21T20:00:00Z",
      "published_at": "2026-04-20T10:00:00Z",
      "rank": 1450.5,
      "schema_type": "TechArticle",
      "schema_image": "https://go.dev/images/gophers.png",
      "schema_author": "Go Team",
      "schema_keywords": "go, golang, tutorial",
      "schema_rating": 0
    }
  ]
}
```

**Search notes:**
- `snippet` contains `<mark>` tags around matched words
- `rank` is the composite relevance score (higher = better)
- Supports FTS5 operators: `"exact phrase"`, `word1 AND word2`, `word1 OR word2`

---

### `GET /api/v1/images/search`

Search indexed images by alt text, title, and context.

**Query Parameters**

| Param | Type | Default | Description |
|-------|------|---------|-------------|
| `q` | string | *required* | Search text |
| `page` | int | `1` | Page number |
| `limit` | int | `10` | Results per page |

---

### `POST /api/v1/crawl`

Enqueue one or more URLs for crawling. By default, URLs crawled within the last 24 hours are skipped.

**Request Body**
```json
{
  "urls": [
    "https://example.com",
    "https://another-site.org/page"
  ],
  "priority": 50,
  "force": false
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `urls` | string[] | *required* | URLs to crawl (max: 1000) |
| `priority` | int | `10` | Queue priority (higher = processed sooner) |
| `force` | bool | `false` | Re-crawl even if recently indexed |

**Response** `202 Accepted`
```json
{
  "queued": 2,
  "skipped": 0
}
```

If some URLs were skipped:
```json
{
  "queued": 1,
  "skipped": 1,
  "reason": "already_indexed"
}
```

---

### `GET /api/v1/queue`

Current crawl queue status.

**Response** `200 OK`
```json
{
  "data": {
    "pending": 3691,
    "in_progress": 10,
    "done": 0,
    "failed": 2,
    "total": 3703
  }
}
```

---

### `POST /api/v1/crawler/start`

Start the crawler engine.

**Response** `200 OK`
```json
{
  "message": "crawler started"
}
```

**Error** `409 Conflict` — If already running
```json
{
  "error": "crawler is already running"
}
```

---

### `POST /api/v1/crawler/stop`

Gracefully stop the crawler.

**Response** `200 OK`
```json
{
  "message": "crawler stop initiated"
}
```

---

### `GET /api/v1/crawler/status`

Current crawler engine status.

**Response** `200 OK`
```json
{
  "status": "running",
  "pages_crawled": 262,
  "pages_errored": 10
}
```

Possible `status` values: `idle`, `running`, `stopping`.

---

## Headers

### Security

Production-grade security headers are included on every response:

```
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
X-XSS-Protection: 1; mode=block
Referrer-Policy: strict-origin-when-cross-origin
Strict-Transport-Security: max-age=63072000; includeSubDomains; preload
Content-Security-Policy: default-src 'self'; ...
```

### CORS

CORS headers are included for frontend access:
```
Access-Control-Allow-Origin: *
Access-Control-Allow-Methods: GET, POST, PUT, DELETE, OPTIONS
Access-Control-Allow-Headers: Content-Type, Authorization, X-Request-ID
```

### Rate Limiting

API requests are limited to **120 requests per minute per IP**. Exceeded requests receive `429 Too Many Requests` with a `Retry-After: 60` header.

### Request ID

Every response includes an `X-Request-ID` header for traceability. You can send your own `X-Request-ID` to correlate logs.

---

Next: [Examples](./examples.md) · [Architecture](./architecture.md)
