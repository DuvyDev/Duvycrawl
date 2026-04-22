# Arquitectura

Visión técnica de cómo Duvycrawl está diseñado internamente.

---

## Diagrama General

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

### Storage (SQLite + FTS5)

- **WAL mode**: Permite lecturas concurrentes durante escrituras
- **FTS5 triggers**: Mantienen el índice full-text sincronizado automáticamente
- **Una sola conexión** de escritura (SQLite limitation) con busy timeout de 5s
- Tablas: `pages`, `pages_fts`, `crawl_queue`, `domains`

### Worker Pool

- N goroutines (default: 10) ejecutan un loop infinito
- Cada worker opera independientemente: dequeue → fetch → parse → store
- `context.WithCancel` permite shutdown graceful
- `sync.WaitGroup` asegura que todos los workers terminen

### Rate Limiter

- Map de `domain → último timestamp de request`
- `sync.Mutex` para thread-safety
- Cleanup automático cada 5 minutos (evita memory leaks)

### Robots.txt Cache

- Cache in-memory con TTL de 24 horas
- **Fail-open**: si no se puede obtener robots.txt, permite el crawl
- Double-checked locking para evitar fetches duplicados

## Estructura de Directorios

```
Duvycrawl/
├── cmd/duvycrawl/          # Entry point
├── internal/
│   ├── config/             # YAML config loading
│   ├── crawler/            # Engine, Fetcher, Parser, Robots
│   ├── frontier/           # URL priority queue
│   ├── ratelimit/          # Per-domain rate limiting
│   ├── storage/            # SQLite + FTS5 implementation
│   ├── scheduler/          # Re-crawl scheduling
│   ├── api/                # REST API (handlers, middleware, server)
│   └── seeds/              # Default seed domains
├── configs/                # YAML configuration files
├── data/                   # SQLite database (auto-created)
└── docs/                   # Documentation
```

---

Anterior: [Ejemplos de Uso](./examples.md) · Inicio: [Getting Started](./getting-started.md)
