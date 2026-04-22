# Getting Started

Guía rápida para poner Duvycrawl en funcionamiento.

---

## Requisitos

- **Go 1.22+** (desarrollado con Go 1.26)
- No requiere bases de datos externas ni servicios adicionales

## Instalación

```bash
# Clonar el repositorio
git clone https://github.com/DuvyDev/Duvycrawl.git
cd Duvycrawl

# Descargar dependencias
go mod tidy

# Compilar el binario
make build
# El binario se genera en bin/duvycrawl.exe
```

## Primer Inicio

```bash
# Opción 1: Ejecutar directamente (modo desarrollo)
make run

# Opción 2: Ejecutar el binario compilado
./bin/duvycrawl.exe -config configs/default.yaml
```

Al iniciar por primera vez, Duvycrawl:

1. **Crea la base de datos** SQLite en `./data/duvycrawl.db`
2. **Registra 18 dominios seed** (Reddit, GitHub, Wikipedia, etc.)
3. **Encola las URLs iniciales** de cada seed (25 URLs en total)
4. **Inicia el crawler** automáticamente (configurable con `-auto-start=false`)
5. **Levanta el API** en `http://localhost:8080`

## Verificar que Funciona

```bash
# Health check
curl http://localhost:8080/api/v1/health

# Ver estadísticas
curl http://localhost:8080/api/v1/stats

# Ver el estado del crawler
curl http://localhost:8080/api/v1/crawler/status
```

> **Nota para Windows (PowerShell):** Usá `Invoke-RestMethod` en lugar de `curl`:
> ```powershell
> Invoke-RestMethod -Uri "http://localhost:8080/api/v1/health"
> ```

## Flags de Línea de Comandos

| Flag | Default | Descripción |
|------|---------|-------------|
| `-config` | `configs/default.yaml` | Ruta al archivo de configuración YAML |
| `-auto-start` | `true` | Iniciar el crawling automáticamente al arrancar |

### Ejemplos

```bash
# Iniciar sin crawlear automáticamente (solo API)
./bin/duvycrawl.exe -auto-start=false

# Usar un archivo de configuración personalizado
./bin/duvycrawl.exe -config ./mi-config.yaml

# Combinados
./bin/duvycrawl.exe -config ./mi-config.yaml -auto-start=false
```

## Detener Duvycrawl

Presioná **Ctrl+C** en la terminal. El shutdown es graceful:

1. Los workers terminan la página que están procesando
2. Se cierra la conexión a la base de datos
3. Se detiene el servidor HTTP

No se pierden datos durante el shutdown.

---

Siguiente: [Configuración](./configuration.md) · [API Reference](./api-reference.md)
