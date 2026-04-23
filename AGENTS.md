# Duvycrawl — Contexto para Agentes

## Qué es este proyecto

Duvycrawl es un web crawler escrito en Go que:
- Crawlea páginas web siguiendo links
- Almacena contenido en SQLite con índice FTS5 para búsqueda full-text
- Expone una API REST para controlar el crawler y hacer búsquedas
- Soporta rate limiting, robots.txt, y re-crawling periódico

## Estructura clave

```
Duvycrawl/
├── cmd/duvycrawl/          # Entry point
├── internal/
│   ├── config/             # Config YAML
│   ├── crawler/            # Engine, Fetcher, Parser, Robots
│   ├── frontier/           # URL queue + Bloom filter
│   ├── ratelimit/          # Per-domain rate limiting (semaphore)
│   ├── storage/            # SQLite + FTS5
│   ├── scheduler/          # Re-crawl scheduling
│   ├── api/                # REST API handlers
│   └── seeds/              # Default seed domains
├── configs/default.yaml    # Configuración por defecto
├── data/                   # SQLite DB + Bloom filter (auto-creado)
└── docs/                   # Documentación
```

## Decisiones arquitectónicas importantes

### 1. SQLite + modernc.org/sqlite

Usamos `modernc.org/sqlite` (implementación pura en Go, sin CGO) con WAL mode habilitado.
- **Ventaja**: compilación simple, sin dependencias de sistema.
- **Trade-off**: `bm25()` de FTS5 devuelve 0 en esta implementación (bug conocido), por eso usamos ranking híbrido con CTE + ROW_NUMBER() + boosts manuales.

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
go build -o bin/duvycrawl.exe ./cmd/duvycrawl

# Linux/macOS
go build -o bin/duvycrawl ./cmd/duvycrawl
```

## Run

```bash
# Con defaults
./bin/duvycrawl

# Con config custom
./bin/duvycrawl -config configs/custom.yaml
```

## Testing

No hay tests unitarios formales todavía. La verificación se hace vía:
1. Build exitoso (`go build`)
2. Iniciar el servidor (`./bin/duvycrawl`)
3. API health check: `GET /api/v1/health`
4. Search: `GET /api/v1/search?q=golang`
5. Verificar que el crawler procesa URLs y las almacena en SQLite.

## Notas para futuros cambios

- Si se modifica `internal/crawler/fetcher.go`, verificar que no se añade `Accept-Encoding` manualmente.
- Si se modifica `internal/storage/sqlite.go`, tener cuidado con `JULIANDAY()` — SQLite de modernc no parsea bien ISO 8601 con microsegundos. Usar `SUBSTR(crawled_at, 1, 10)` para extraer la fecha.
- El Bloom filter usa `bits-and-blooms/bitset` y `tidwall/btree` para persistencia. Si se cambian estas dependencias, actualizar `go.mod`.
- La firma de `/api/v1/search` NO debe cambiar: parámetros `q`, `page`, `limit`, `lang`.
