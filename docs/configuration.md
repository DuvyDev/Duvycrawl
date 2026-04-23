# Configuración

Duvycrawl se configura a través de un archivo YAML. Por defecto usa `configs/default.yaml`.

---

## Archivo Completo con Defaults

```yaml
crawler:
  workers: 10
  max_depth: 3
  request_timeout: 15s
  politeness_delay: 1s
  random_delay: 500ms
  parallelism_per_domain: 2
  max_retries: 3
  user_agent: "Duvycrawl/1.0 (+https://github.com/DuvyDev/Duvycrawl)"
  max_page_size_kb: 5120
  respect_robots: true
  disable_cookies: false
  max_idle_conns_per_host: 100
  proxy_url: ""

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

## Secciones

### `crawler` — Motor de Crawling

| Campo | Tipo | Default | Descripción |
|-------|------|---------|-------------|
| `workers` | int | `10` | Número de goroutines concurrentes que crawlean páginas. Más workers = más velocidad, pero más carga en tu red y los servidores destino. Rango: 1–100. |
| `max_depth` | int | `3` | Profundidad máxima de links a seguir desde las páginas seed. `0` = solo las páginas seed, `1` = seeds + links directos, etc. |
| `request_timeout` | duration | `15s` | Timeout máximo para una request HTTP individual. Incluye conexión, TLS y descarga del body. |
| `politeness_delay` | duration | `1s` | Delay mínimo entre requests al **mismo dominio**. Evita saturar servidores. Mínimo permitido: `100ms`. |
| `random_delay` | duration | `500ms` | Jitter aleatorio añadido al delay entre requests. Hace el crawling menos predecible. |
| `parallelism_per_domain` | int | `2` | Número de requests concurrentes permitidas al mismo dominio. Valores altos = más rápido pero más agresivo. |
| `max_retries` | int | `3` | Intentos máximos para una URL que falla. Después del último intento, la URL se marca como failed. |
| `user_agent` | string | (ver arriba) | User-Agent enviado en cada request. Algunos sitios bloquean crawlers sin User-Agent válido. |
| `max_page_size_kb` | int | `5120` | Tamaño máximo de página a descargar (en KB). Páginas más grandes se ignoran. Default: 5 MB. |
| `respect_robots` | bool | `true` | Si es `true`, respeta las directivas de `robots.txt`. **Recomendado dejarlo en `true`**. |
| `disable_cookies` | bool | `false` | Si es `true`, desactiva el cookie jar. Útil para sitios que no requieren sesión. |
| `max_idle_conns_per_host` | int | `100` | Conexiones idle máximas por host. Aumentar mejora reutilización de conexiones HTTP keep-alive. |
| `proxy_url` | string | `""` | URL de proxy HTTP/SOCKS5. Formato: `http://proxy:8080` o `socks5://proxy:1080`. Vacío = sin proxy. |

### `storage` — Persistencia

| Campo | Tipo | Default | Descripción |
|-------|------|---------|-------------|
| `db_path` | string | `./data/duvycrawl.db` | Ruta al archivo SQLite. El directorio se crea automáticamente si no existe. |

### `api` — Servidor REST

| Campo | Tipo | Default | Descripción |
|-------|------|---------|-------------|
| `host` | string | `0.0.0.0` | Dirección IP donde escucha el server. `0.0.0.0` acepta conexiones de cualquier interfaz; `127.0.0.1` solo localhost. |
| `port` | int | `8080` | Puerto TCP del API. Rango: 1–65535. |

### `logging` — Logs

| Campo | Tipo | Default | Descripción |
|-------|------|---------|-------------|
| `level` | string | `info` | Nivel mínimo de log. Opciones: `debug`, `info`, `warn`, `error`. |
| `format` | string | `text` | Formato de salida. `text` = legible para humanos, `json` = para herramientas de log (ELK, etc). |

---

## Ejemplos de Configuración

### Crawling Agresivo (red local / testing)

```yaml
crawler:
  workers: 50
  max_depth: 5
  request_timeout: 30s
  politeness_delay: 200ms
  max_retries: 5

logging:
  level: "debug"
```

### Crawling Conservador (uso liviano)

```yaml
crawler:
  workers: 3
  max_depth: 2
  politeness_delay: 3s
  max_retries: 1

logging:
  level: "warn"
```

### Producción con Logs JSON

```yaml
crawler:
  workers: 15
  max_depth: 3
  politeness_delay: 1s

api:
  host: "127.0.0.1"
  port: 9090

logging:
  level: "info"
  format: "json"
```

---

## Validación

La configuración se valida al iniciar. Si hay valores inválidos, Duvycrawl muestra un error claro y no arranca:

```
fatal: loading configuration: invalid configuration: crawler.workers must be >= 1, got 0
```

---

Siguiente: [API Reference](./api-reference.md) · [Ejemplos de Uso](./examples.md)
