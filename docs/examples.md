# Ejemplos de Uso

Ejemplos prácticos para las operaciones más comunes con Duvycrawl.

> En **PowerShell** (Windows), reemplazá `curl` por `Invoke-RestMethod`.

---

## 1. Crawlear un Sitio Específico

```bash
# Una sola URL
curl -X POST http://localhost:8080/api/v1/crawl \
  -H "Content-Type: application/json" \
  -d '{"urls": ["https://go.dev/doc/"]}'

# Múltiples URLs con prioridad alta
curl -X POST http://localhost:8080/api/v1/crawl \
  -H "Content-Type: application/json" \
  -d '{"urls": ["https://go.dev/doc/", "https://go.dev/blog/"], "priority": 90}'
```

**PowerShell:**
```powershell
$body = @{ urls = @("https://go.dev/doc/") } | ConvertTo-Json
Invoke-RestMethod -Uri "http://localhost:8080/api/v1/crawl" -Method POST -ContentType "application/json" -Body $body
```

---

## 2. Buscar en el Índice

```bash
curl "http://localhost:8080/api/v1/search?q=golang+concurrency"
curl "http://localhost:8080/api/v1/search?q=tutorial&page=2&limit=20"
curl "http://localhost:8080/api/v1/search?q=%22machine+learning%22"  # Frase exacta
curl "http://localhost:8080/api/v1/search?q=rust+OR+golang"          # OR
```

### Operadores de Búsqueda FTS5

| Operador | Ejemplo | Descripción |
|----------|---------|-------------|
| Espacio | `web crawler` | AND implícito |
| `OR` | `rust OR golang` | Cualquiera |
| `NOT` | `python NOT django` | Excluir |
| `"..."` | `"machine learning"` | Frase exacta |
| `*` | `program*` | Prefijo (programming, programmer...) |

---

## 3. Gestionar Seeds

```bash
curl http://localhost:8080/api/v1/seeds                              # Listar
curl -X POST http://localhost:8080/api/v1/seeds \
  -H "Content-Type: application/json" \
  -d '{"domain": "lobste.rs", "priority": 85}'                      # Agregar
curl -X DELETE http://localhost:8080/api/v1/seeds/lobste.rs          # Eliminar
```

---

## 4. Controlar el Crawler

```bash
curl http://localhost:8080/api/v1/crawler/status                     # Estado
curl -X POST http://localhost:8080/api/v1/crawler/start              # Iniciar
curl -X POST http://localhost:8080/api/v1/crawler/stop               # Detener
```

---

## 5. Script de Monitoreo (PowerShell)

```powershell
while ($true) {
    $stats = Invoke-RestMethod -Uri "http://localhost:8080/api/v1/stats"
    $s = $stats.data.stats
    $session = $stats.data.session
    Write-Host "[$(Get-Date -Format 'HH:mm:ss')] Pages: $($s.total_pages) | Queue: $($s.queue.pending) | DB: $([math]::Round($s.database_size_mb, 1)) MB"
    Start-Sleep -Seconds 10
}
```

---

## 6. Integración con Frontend (JavaScript)

```typescript
const API = "http://localhost:8080/api/v1";

async function search(query: string, page = 1, limit = 10) {
  const params = new URLSearchParams({ q: query, page: String(page), limit: String(limit) });
  const res = await fetch(`${API}/search?${params}`);
  return res.json();
}

async function crawlURLs(urls: string[], priority = 10) {
  return fetch(`${API}/crawl`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ urls, priority }),
  }).then(r => r.json());
}
```

---

## 7. Integración con Python

```python
import requests

API = "http://localhost:8080/api/v1"

# Buscar
data = requests.get(f"{API}/search", params={"q": "machine learning", "limit": 5}).json()
for r in data["results"]:
    print(f"[{r['domain']}] {r['title']} — {r['url']}")

# Encolar
requests.post(f"{API}/crawl", json={"urls": ["https://arxiv.org/"], "priority": 80})
```

---

## 8. Niveles de Prioridad

| Prioridad | Uso |
|-----------|-----|
| **100** | Seed domains (máxima prioridad) |
| **50** | Dominios nuevos con pocas páginas |
| **10** | URLs descubiertas (default) |
| **5** | Re-crawl automático |

---

Anterior: [API Reference](./api-reference.md) · Siguiente: [Arquitectura](./architecture.md)
