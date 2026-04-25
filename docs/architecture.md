# Architecture

Technical overview of Duvycrawl's internal design and production deployment.

---

## Production Deployment

```
                     Internet
                        │
                        ▼
               ┌─────────────────┐
               │  Cloudflare      │
               │  Tunnel          │
               │  (cloudflared)   │
               └────────┬────────┘
                        │  cloudflared-proxy
                        ▼
               ┌─────────────────┐
               │  Suvy           │
               │  (Search UI)    │
               │  :8800          │
               └────────┬────────┘
                        │  suvynet
                        ▼
               ┌─────────────────┐
               │  Duvycrawl      │
               │  (Crawler+API)  │
               │  :8080          │
               └────────┬────────┘
                        │  duvycrawl
                        ▼
               ┌─────────────────┐
               │  Warp           │
               │  (SOCKS5 proxy) │
               │  :1080          │
               └────────┬────────┘
                        │
                        ▼
                     Internet
                 (crawled sites)
```

Networks:
- **cloudflared-proxy**: cloudflared ↔ suvy
- **suvynet**: suvy ↔ duvycrawl (API calls)
- **duvycrawl**: duvycrawl ↔ warp (SOCKS5 proxy)

The crawler API is **not** exposed to the internet — only Suvy can reach it internally.

---

## Internal Architecture

```
┌──────────────────────────────┐
│ REST API Server              │
│ (net/http ServeMux, :8080)   │
└──────────┬───────────────────┘
           │
┌────────────────────┼────────────────────┐
│                    │                    │
▼                    ▼                    ▼
┌─────────────┐ ┌──────────────┐ ┌──────────────┐
│ Scheduler   │ │ Frontier     │ │ Storage      │
│ (re-crawl)  │──▶│ (URL queue) │ │ (SQLite)     │
└─────────────┘ └──────┬───────┘ └──────────────┘
                       │ ▲
                       ▼ │
              ┌──────────────────┐ │
              │ Worker Pool      │ │
              │ (N goroutines)   │ │
              └───┬────┬────┬───┘ │
                  │    │    │     │
                  ▼    ▼    ▼     │
              ┌──────────────────┐ │
              │ Fetcher          │ │
              │ (HTTP client)    │ │
              └───────┬──────────┘ │
                      │            │
                      ▼            │
              ┌──────────────────┐ │
              │ Parser           │─┘
              │ (HTML → data)    │
              └──────────────────┘
```
                    ┌──────────────────────────────┐
                    │       REST API Server         │
                    │   (net/http ServeMux, :8080)   │
                    └──────────┬───────────────────┘
                               │
          ┌────────────────────┼────────────────────┐
          │                    │                    │
          ▼                    ▼                    ▼
   ┌─────────────┐    ┌──────────────┐    ┌──────────────┐
   │  Scheduler   │    │   Frontier    │    │   Storage    │
   │  (re-crawl)  │───▶│ (URL queue)  │    │  (SQLite)    │
   └─────────────┘    └──────┬───────┘    └──────────────┘
                              │                    ▲
                              ▼                    │
                    ┌──────────────────┐           │
                    │   Worker Pool    │           │
                    │  (N goroutines)  │           │
                    └───┬────┬────┬───┘           │
                        │    │    │                │
                        ▼    ▼    ▼                │
                    ┌──────────────────┐           │
                    │     Fetcher      │           │
                    │   (HTTP client)  │           │
                    └───────┬──────────┘           │
                            │                      │
                            ▼                      │
                    ┌──────────────────┐           │
                    │     Parser       │───────────┘
                    │  (HTML → data)   │
                    └──────────────────┘
```

## Flujo de un Crawl

1. **Scheduler** o **API** insertan URLs en el **Frontier**
2. **Workers** toman URLs del Frontier (ordenadas por prioridad)
3. Cada worker consulta el **Rate Limiter** (delay por dominio) y **Robots.txt Cache**
4. El **Fetcher** descarga la página (HTTP GET con timeouts)
5. El **Parser** extrae título, descripción, texto y links del HTML
6. Los datos se guardan en **SQLite** (tabla `pages` + índice FTS5)
7. Los links descubiertos se envían de vuelta al **Frontier**

## Componentes Clave

### Fetcher (HTTP Client)

- **Cookie jar** (`net/http/cookiejar`) habilitado por defecto: esencial para
  sitios con protecciones por sesión, CSRF, o login walls.
- **Transport optimizado**:
  - `ForceAttemptHTTP2: true` para HTTP/2.
  - `MaxIdleConnsPerHost: 100` (antes 10) para aprovechar keep-alive.
  - `MaxConnsPerHost` igualado a idle conns.
  - Gzip automático habilitado.
- **Retry con backoff**: reintentos automáticos con espera creciente;
  no reintenta errores 4xx (excepto 429 Too Many Requests).
- **Proxy HTTP/SOCKS5** soportado vía `PROXY_URL` environment variable.
- **Headers realistas**: `Accept-Encoding`, `Sec-Fetch-*`, `Upgrade-Insecure-Requests`,
  simulando un navegador real.

### Queue (Frontier)

- **Bloom filter** integrado para deduplicación eficiente de URLs:
  - Filtra URLs duplicadas con bajo uso de memoria (~1MB para 1M URLs).
  - Tasa de falsos positivos configurable (~1% por defecto).
  - Persistido en disco (`data/bloom.bf`) para sobrevivir reinicios.
  - Consultado antes de encolar cada URL nueva.
- **Wake channel**: los workers se bloquean eficientemente cuando no hay trabajo
  y se despiertan inmediatamente al llegar nuevas URLs.

### Parser (HTML → datos)

- **Detección de charset en 3 capas** (como Colly):
  1. `Content-Type` HTTP header (`charset=...`).
  2. Meta tags HTML (`<meta charset="...">`).
  3. **Heurística de contenido** vía `github.com/saintfish/chardet` cuando las
     dos anteriores fallan. Detecta encodings como ISO-8859-1, Windows-1252,
     GB2312, EUC-JP, etc. desde el contenido binario crudo.
- Conversión automática a UTF-8 antes de parsear con GoQuery.

### Storage (SQLite + FTS5)

- **WAL mode**: Permite lecturas concurrentes durante escrituras
- **FTS5 triggers**: Mantienen el índice full-text sincronizado automáticamente
- **Una sola conexión** de escritura (SQLite limitation) con busy timeout de 5s
- Tablas: `pages`, `pages_fts`, `crawl_queue`, `domains`

### Worker Pool

- N goroutines (default: 100) ejecutan un loop eficiente.
- Cada worker opera independientemente: dequeue → fetch → parse → store.
- **Wake channel** en la queue: los workers duermen eficientemente cuando no
  hay trabajo y se despiertan inmediatamente al llegar nuevas URLs.
  Elimina el polling con `time.Sleep(500ms)` anterior.
- `context.WithCancel` permite shutdown graceful.
- `sync.WaitGroup` asegura que todos los workers terminen.

### Rate Limiter (inspirado en Colly)

- **Semáforos por dominio** (`chan struct{}` bufferizado) permiten configurar
  paralelismo concurrente por dominio (no solo 1 como antes).
- **Delay fijo + RandomDelay**: añade jitter aleatorio para evitar patrones
  predecibles de crawling.
- `sync.Mutex` para thread-safety.
- Cleanup automático cada 5 minutos.

### Robots.txt Cache

- Cache in-memory con TTL de 24 horas
- **Fail-open**: si no se puede obtener robots.txt, permite el crawl
- Double-checked locking para evitar fetches duplicados

## Estructura de Directorios

```
Duvycrawl/
├── main.go             # Entry point
├── internal/
│   ├── config/         # YAML config loading
│   ├── crawler/        # Engine, Fetcher, Parser, Robots
│   ├── frontier/       # URL priority queue
│   ├── ratelimit/      # Per-domain rate limiting
│   ├── storage/        # SQLite + FTS5 implementation
│   ├── scheduler/      # Re-crawl scheduling
│   ├── api/            # REST API (handlers, middleware, server)
│   └── seeds/          # Default seed domains
├── configs/            # YAML configuration files
├── data/               # SQLite database (auto-created)
├── docs/               # Documentation
├── .env.example        # Environment variable template
├── Dockerfile
└── docker-compose.yml  # Production deployment stack
```

---

Previous: [Examples](./examples.md) · Next: [Getting Started](./getting-started.md)
