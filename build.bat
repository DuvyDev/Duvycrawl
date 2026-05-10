@echo off
echo ========================================
echo   Compilando proyecto Duvycrawl...
echo ========================================
echo.

echo [1/3] Compilando Duvycrawl (Monolito)...
go build -o duvycrawl.exe ./cmd/duvycrawl
if %errorlevel% neq 0 (
    echo Error al compilar Duvycrawl.
    exit /b %errorlevel%
)

echo [2/3] Compilando Search API (Solo Buscador)...
go build -o search-api.exe ./cmd/search-api
if %errorlevel% neq 0 (
    echo Error al compilar Search API.
    exit /b %errorlevel%
)

echo [3/3] Compilando Crawler Engine (Solo Rastreador)...
go build -o crawler.exe ./cmd/crawler
if %errorlevel% neq 0 (
    echo Error al compilar Crawler.
    exit /b %errorlevel%
)

echo.
echo ========================================
echo   Compilacion exitosa.
echo   Los binarios estan en la carpeta raiz.
echo ========================================
pause
