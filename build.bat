@echo off
echo ========================================
echo   Compilando proyecto Duvycrawl...
echo ========================================
echo.

echo [1/2] Compilando Search API...
go build -o search-api.exe ./cmd/search-api
if %errorlevel% neq 0 (
    echo Error al compilar Search API.
    exit /b %errorlevel%
)

echo [2/2] Compilando Crawler Engine...
go build -o crawler.exe ./cmd/crawler
if %errorlevel% neq 0 (
    echo Error al compilar Crawler.
    exit /b %errorlevel%
)

echo.
echo ========================================
echo   Compilacion exitosa.
echo   Binarios: search-api.exe, crawler.exe
echo ========================================
pause
