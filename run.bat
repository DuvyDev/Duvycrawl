@echo off
echo ========================================
echo   Iniciando Duvycrawl (Modo Monolito)
echo ========================================
echo.
echo Este modo inicia tanto la API como el Crawler en el mismo proceso,
echo ideal para pruebas rapidas en Windows.
echo Presiona Ctrl+C para detener.
echo.

if not exist "bin\duvycrawl.exe" (
    echo El binario no existe. Ejecuta build.bat primero.
    pause
    exit /b 1
)

bin\duvycrawl.exe
pause
