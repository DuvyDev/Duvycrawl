# Duvycrawl — Contexto para Agentes

## Qué es este proyecto

Duvycrawl es un web crawler escrito en Go que:
- Crawlea páginas web siguiendo links
- Almacena contenido en SQLite con índice FTS5 para búsqueda full-text
- Extrae schema.org (JSON-LD) para enriquecer resultados con imágenes, autor, tipo de contenido, rating, etc.
- Expone una API REST para controlar el crawler y hacer búsquedas con filtrado por dominio, tipo, e idioma
- Soporta rate limiting, robots.txt, re-crawling periódico, y proxy SOCKS5 (Cloudflare Warp)

## Estructura clave

```
Duvycrawl/
├── main.go                 # Entry point
├── internal/
│   ├── config/             # Config YAML
│   ├── crawler/            # Engine, Fetcher, Parser, Robots
│   ├── frontier/           # URL queue + Bloom filter
│   ├── ratelimit/          # Per-domain rate limiting (semaphore)
│   ├── storage/            # SQLite + FTS5
│   ├── scheduler/          # Re-crawl scheduling
│   ├── api/                # REST API handlers + middleware
│   └── seeds/              # Default seed domains
├── configs/default.yaml    # Configuración por defecto
├── data/                   # SQLite DB + Bloom filter (auto-creado)
├── docs/                   # Documentación
├── Dockerfile
├── docker-compose.yml
└── README.md
```

## Decisiones arquitectónicas importantes

### 1. Arquitectura Multi-DB (SQLite + modernc.org/sqlite)

Usamos `modernc.org/sqlite` (implementación pura en Go, sin CGO) con WAL mode habilitado.
Para evitar bloqueos de concurrencia (`SQLITE_BUSY`) entre las lecturas rápidas del buscador y las escrituras masivas del crawler, el almacenamiento está segmentado en 3 archivos independientes:
- `content.db`: Almacena `pages` y el índice FTS5. Optimizado para búsquedas.
- `crawler.db`: Almacena el estado asíncrono (`crawl_queue`, `domains`).
- `graph.db`: Dedicado exclusivamente a la topología (`links`) con sincronización relajada para máxima velocidad de inserción.

**Trade-off conocido**: `bm25()` de FTS5 devuelve 0 en esta implementación (bug conocido), por eso usamos ranking híbrido con CTE + ROW_NUMBER() + boosts manuales. Además, para calcular el PageRank cruzando datos, usamos `ATTACH DATABASE 'graph.db' AS graph` temporalmente sobre la conexión de `content.db`.

### 2. Bloom Filter para deduplicación

La queue usa un Bloom filter para evitar encolar URLs ya vistas.
- **Ventaja**: memoria constante (~1MB para 1M URLs) vs. set en memoria o query a DB.
- **Trade-off**: permite falsos positivos (~1%), por eso siempre hay un fallback `SELECT id FROM pages WHERE url = ?`.
- Se persiste en `data/bloom.bf` para sobrevivir reinicios.

### 3. Rate Limiter con semáforos

Inspirado en Colly: usa `chan struct{}` bufferizado por dominio en lugar de un simple `time.Sleep`.
- Permite configurar paralelismo concurrente por dominio (`parallelism_per_domain`).
- Añade `random_delay` como jitter para evitar patrones predecibles.

### 4. Charset detection

El parser implementa 3 capas de detección de encoding:
1. HTTP `Content-Type` header
2. HTML `<meta charset="...">`
3. Heurística de contenido vía `github.com/saintfish/chardet`

Esto resuelve el problema de caracteres corruptos (acentos, eñes) en sitios que no usan UTF-8.

### 5. Fetcher con cookie jar y retry

- Cookie jar habilitado por defecto (`net/http/cookiejar`) para sitios con sesión.
- Retry con backoff exponencial para errores transientes (incluye 429).
- Proxy HTTP/SOCKS5 soportado.
- **Importante**: NO añadir manualmente `Accept-Encoding: gzip` en los headers del request. Go's `http.Transport` maneja la compresión automáticamente; si se añade manualmente, el body llega comprimido y el parser no puede leerlo.

## Build

```bash
# Windows PowerShell
go build -o bin/duvycrawl.exe .

# Linux/macOS
go build -o bin/duvycrawl .
```

## Run

```bash
# Con defaults
./bin/duvycrawl

# Con config custom
./bin/duvycrawl -config configs/custom.yaml

# Docker
docker compose up -d
```

## Testing

No hay tests unitarios formales todavía. La verificación se hace vía:
1. Build exitoso (`go build`)
2. Iniciar el servidor (`./bin/duvycrawl`)
3. API health check: `GET /api/v1/health`
4. Search: `GET /api/v1/search?q=golang`
5. Verificar que el crawler procesa URLs y las almacena en SQLite.

## Semantic Search (Embeddings)

Desde la integración con `internal/embedder`, el crawler genera embeddings vectoriales para cada página usando la API de Ollama (`/api/embeddings`). Esto habilita re-ranking semántico en las búsquedas.

### Configuración

Variables de entorno:
- `OLLAMA_EMBED_URL`: URL base de Ollama (default: `http://localhost:11434`)
- `OLLAMA_EMBED_MODEL`: Modelo de embeddings (default: `qwen3-embedding:0.6b`)

### Cómo funciona

1. **Crawl time**: `engine.go` genera un embedding por página concatenando `title + description + content` (hasta 6000 runes ≈ 2000 tokens) y enviándolo en una sola llamada a Ollama. Sin chunking ni average pooling — un vector limpio por página.
2. **Storage**: Los vectores se guardan en la tabla `page_embeddings` como BLOB little-endian de `[]float32`.
3. **Search time**: `sqlite_search.go` genera el embedding de la query, calcula similitud coseno con los top 100 candidatos FTS5, y blendea: `final_score = 0.40 * lexical_score + 0.60 * semantic_score`.

### Consideraciones

- Si Ollama no está disponible, el crawler y el buscador funcionan normalmente en modo léxico puro.
- Páginas sin embedding simplemente no reciben boost semántico.
- `qwen3-embedding:0.6b` produce vectores de 1024 dimensiones (soporta Matryoshka: 32-1024). Multilingüe (100+ idiomas).
- El embedding se computa sobre `title + description + content` (hasta 8000 chars capturados, 6000 runes max al modelo).

## Notas para futuros cambios

- Si se modifica `internal/crawler/fetcher.go`, verificar que no se añade `Accept-Encoding` manualmente.
- Si se modifica `internal/storage/sqlite.go`, tener cuidado con `JULIANDAY()` — SQLite de modernc no parsea bien ISO 8601 con microsegundos. Usar `SUBSTR(crawled_at, 1, 10)` para extraer la fecha.
- El Bloom filter usa `bits-and-blooms/bitset` y `tidwall/btree` para persistencia. Si se cambian estas dependencias, actualizar `go.mod`.
- La firma de `/api/v1/search` NO debe cambiar: parámetros `q`, `page`, `limit`, `lang`.
- `modernc.org/sqlite` devuelve `published_at` como string, no como `time.Time`. Escanear siempre como `sql.NullString` y parsear con `parseFlexibleTime`.
- El proxy SOCKS5 usa `golang.org/x/net/proxy`. `socks5h://` fuerce resolución DNS remota a través del proxy.
- Los campos schema.org (`schema_type`, `schema_image`, etc.) se agregaron con `ALTER TABLE`. Para DBs existentes, las filas viejas quedarán vacías.
