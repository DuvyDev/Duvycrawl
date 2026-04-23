# API Reference

Duvycrawl expone una API REST en `http://localhost:8080` (configurable).
Todos los endpoints devuelven JSON con `Content-Type: application/json`.

---

## Tabla de Endpoints

| Método | Path | Descripción |
|--------|------|-------------|
| `GET` | `/api/v1/health` | Health check |
| `GET` | `/api/v1/search` | Búsqueda full-text |
| `GET` | `/api/v1/pages/{id}` | Detalle de una página |
| `GET` | `/api/v1/stats` | Estadísticas generales |
| `POST` | `/api/v1/crawl` | Encolar URLs para crawlear |
| `GET` | `/api/v1/pages/lookup` | Buscar página por URL exacta |
| `GET` | `/api/v1/queue` | Estado de la cola |
| `GET` | `/api/v1/seeds` | Listar dominios seed |
| `POST` | `/api/v1/seeds` | Agregar dominio seed |
| `DELETE` | `/api/v1/seeds/{domain}` | Eliminar dominio seed |
| `POST` | `/api/v1/crawler/start` | Iniciar el crawler |
| `POST` | `/api/v1/crawler/stop` | Detener el crawler |
| `GET` | `/api/v1/crawler/status` | Estado del crawler |

---

## Endpoints en Detalle

### `GET /api/v1/health`

Health check simple. Útil para monitoreo.

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

Búsqueda full-text sobre las páginas indexadas usando SQLite FTS5.

**Query Parameters**

| Param | Tipo | Default | Descripción |
|-------|------|---------|-------------|
| `q` | string | *requerido* | Texto a buscar |
| `page` | int | `1` | Número de página |
| `limit` | int | `10` | Resultados por página (máx: 100) |

**Response** `200 OK`
```json
{
  "query": "golang tutorial",
  "total": 42,
  "page": 1,
  "limit": 10,
  "results": [
    {
      "id": 1234,
      "url": "https://go.dev/doc/tutorial/",
      "title": "Tutorial - The Go Programming Language",
      "description": "A tutorial for the Go programming language.",
      "snippet": "...learn <mark>Golang</mark> with this comprehensive <mark>tutorial</mark>...",
      "domain": "go.dev",
      "crawled_at": "2026-04-21T20:00:00Z",
      "rank": -4.02
    }
  ]
}
```

**Notas sobre la búsqueda:**
- Los `snippet` contienen tags `<mark>` alrededor de las palabras encontradas
- El `rank` es el score de relevancia de FTS5 (más negativo = más relevante)
- Se busca en título, descripción y contenido de la página
- Soporta operadores FTS5: `"frase exacta"`, `word1 AND word2`, `word1 OR word2`, `word1 NOT word2`

**Error** `400 Bad Request` — Si falta el parámetro `q`
```json
{
  "error": "missing required query parameter 'q'"
}
```

---

### `GET /api/v1/pages/lookup`

Busca una página por su URL exacta. Útil para verificar si una URL ya fue crawleada.

**Query Parameters**

| Param | Tipo | Descripción |
|-------|------|-------------|
| `url` | string | URL exacta a buscar (requerido) |

**Response** `200 OK`
```json
{
  "data": {
    "id": 43,
    "url": "https://learn.microsoft.com/en-us/dotnet/csharp/",
    "domain": "learn.microsoft.com",
    "title": "C# Guide - .NET managed language",
    "description": "The C# guide has everything you need...",
    "content": "Get started Tour of C# Concept Fundamentals...",
    "status_code": 200,
    "content_hash": "a1b2c3d4...",
    "crawled_at": "2026-04-21T23:36:14Z",
    "created_at": "2026-04-21T23:36:14Z",
    "updated_at": "2026-04-21T23:36:14Z"
  }
}
```

**Error** `404 Not Found` — Si la URL no existe en la base de datos.
```json
{
  "error": "page not found"
}
```

**Error** `400 Bad Request` — Si falta el parámetro `url`.
```json
{
  "error": "missing required query parameter 'url'"
}
```

---

### `GET /api/v1/pages/{id}`

Retorna el detalle completo de una página por su ID.

**Path Parameters**

| Param | Tipo | Descripción |
|-------|------|-------------|
| `id` | int | ID numérico de la página |

**Response** `200 OK`
```json
{
  "data": {
    "id": 43,
    "url": "https://learn.microsoft.com/en-us/dotnet/csharp/",
    "domain": "learn.microsoft.com",
    "title": "C# Guide - .NET managed language",
    "description": "The C# guide has everything you need...",
    "content": "Get started Tour of C# Concept Fundamentals...",
    "status_code": 200,
    "content_hash": "a1b2c3d4...",
    "crawled_at": "2026-04-21T23:36:14Z",
    "created_at": "2026-04-21T23:36:14Z",
    "updated_at": "2026-04-21T23:36:14Z"
  }
}
```

**Error** `404 Not Found`
```json
{
  "error": "page not found"
}
```

---

### `GET /api/v1/stats`

Estadísticas generales del crawler.

**Response** `200 OK`
```json
{
  "data": {
    "stats": {
      "total_pages": 125,
      "total_domains": 23,
      "seed_domains": 18,
      "queue": {
        "pending": 3691,
        "in_progress": 10,
        "done": 0,
        "failed": 2,
        "total": 3703
      },
      "database_size_mb": 3.37
    },
    "engine_status": "running",
    "session": {
      "pages_crawled": 127,
      "pages_errored": 0
    }
  }
}
```

---

### `POST /api/v1/crawl`

Encola una o más URLs para ser crawleadas.

**Request Body**
```json
{
  "urls": [
    "https://example.com",
    "https://another-site.org/page"
  ],
  "priority": 50
}
```

| Campo | Tipo | Default | Descripción |
|-------|------|---------|-------------|
| `urls` | string[] | *requerido* | Lista de URLs a crawlear (máx: 1000) |
| `priority` | int | `10` | Prioridad en la cola (mayor = se procesa antes) |

**Response** `202 Accepted`
```json
{
  "message": "URLs enqueued for crawling",
  "count": 2
}
```

---

### `GET /api/v1/queue`

Estado actual de la cola de crawling.

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

### `GET /api/v1/seeds`

Lista todos los dominios seed registrados.

**Response** `200 OK`
```json
{
  "data": [
    {
      "id": 1,
      "domain": "reddit.com",
      "is_seed": true,
      "pages_count": 5,
      "avg_response_ms": 230,
      "created_at": "2026-04-21T23:34:35Z"
    }
  ]
}
```

---

### `POST /api/v1/seeds`

Agrega un nuevo dominio seed. Automáticamente encola la página principal del dominio.

**Request Body**
```json
{
  "domain": "news.ycombinator.com",
  "priority": 90
}
```

**Response** `201 Created`
```json
{
  "message": "seed domain added",
  "domain": "news.ycombinator.com"
}
```

---

### `DELETE /api/v1/seeds/{domain}`

Elimina un dominio de la lista de seeds. Las páginas ya crawleadas se mantienen.

**Response** `200 OK`
```json
{
  "message": "seed domain removed",
  "domain": "news.ycombinator.com"
}
```

---

### `POST /api/v1/crawler/start`

Inicia el motor de crawling.

**Response** `200 OK`
```json
{
  "message": "crawler started"
}
```

**Error** `409 Conflict` — Si ya está corriendo
```json
{
  "error": "crawler is already running"
}
```

---

### `POST /api/v1/crawler/stop`

Detiene el crawler de forma graceful (los workers terminan la página actual).

**Response** `200 OK`
```json
{
  "message": "crawler stop initiated"
}
```

---

### `GET /api/v1/crawler/status`

Estado actual del motor de crawling.

**Response** `200 OK`
```json
{
  "status": "running",
  "pages_crawled": 262,
  "pages_errored": 10
}
```

Valores posibles de `status`: `idle`, `running`, `stopping`.

---

## Headers

### CORS

Todos los endpoints incluyen headers CORS permisivos para desarrollo:
```
Access-Control-Allow-Origin: *
Access-Control-Allow-Methods: GET, POST, PUT, DELETE, OPTIONS
Access-Control-Allow-Headers: Content-Type, Authorization, X-Request-ID
```

### Request ID

Cada response incluye un `X-Request-ID` header para trazabilidad.
Podés enviar tu propio `X-Request-ID` en la request para correlacionar logs.

---

Siguiente: [Ejemplos de Uso](./examples.md) · [Arquitectura](./architecture.md)
