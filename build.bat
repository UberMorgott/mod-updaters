@echo off
setlocal enabledelayedexpansion
rem build.bat [valheim^|windrose^|all]  (default all)
rem Builds the tagged updater exe(s) into the repo root, then UPX-compresses.
rem Server address + credentials are injected here from the gitignored config.txt
rem via -ldflags -X — they are NOT present in tracked source.
cd /d %~dp0

set "TARGET=%~1"
if "%TARGET%"=="" set "TARGET=all"

rem NOTE: do not name this var "UPX" — upx.exe reads %UPXEXE% as default options.
set "UPXEXE=Z:\SOFT\СЖАТИЕ EXE\upx.exe"

rem --- Load build-time secrets from config.txt (KEY=VALUE, one per line) ---
if not exist "config.txt" (
    echo ERROR: config.txt not found. Copy config.example.txt to config.txt and fill it in.
    exit /b 1
)
set "CFG_SERVER="
set "CFG_LOGIN="
set "CFG_PASSWORD="
set "CFG_REMOTE_BASE="
set "CFG_HOST_KEY="
for /f "usebackq tokens=1* delims==" %%A in ("config.txt") do (
    set "K=%%A"
    set "V=%%B"
    if /i "!K!"=="server"      set "CFG_SERVER=!V!"
    if /i "!K!"=="login"       set "CFG_LOGIN=!V!"
    if /i "!K!"=="password"    set "CFG_PASSWORD=!V!"
    if /i "!K!"=="remote_base" set "CFG_REMOTE_BASE=!V!"
    if /i "!K!"=="host_key"    set "CFG_HOST_KEY=!V!"
)

rem Build the -ldflags string. host_key contains spaces, so its -X value is wrapped
rem in escaped inner quotes (\"...\") so cmd.exe passes it to the linker intact.
set "PKG=main"
set "LDFLAGS=-s -w"
set "LDFLAGS=!LDFLAGS! -X %PKG%.cfgHost=!CFG_SERVER!"
set "LDFLAGS=!LDFLAGS! -X %PKG%.cfgLogin=!CFG_LOGIN!"
set "LDFLAGS=!LDFLAGS! -X %PKG%.cfgPassword=!CFG_PASSWORD!"
set "LDFLAGS=!LDFLAGS! -X %PKG%.cfgRemoteBase=!CFG_REMOTE_BASE!"
set "LDFLAGS=!LDFLAGS! -X \"%PKG%.cfgHostKey=!CFG_HOST_KEY!\""

if /i "%TARGET%"=="all" (
    call :build valheim uar.exe || exit /b 1
    call :build windrose wur.exe || exit /b 1
    goto :done
)
if /i "%TARGET%"=="valheim" (
    call :build valheim uar.exe || exit /b 1
    goto :done
)
if /i "%TARGET%"=="windrose" (
    call :build windrose wur.exe || exit /b 1
    goto :done
)

echo Unknown target "%TARGET%". Use: build.bat [valheim^|windrose^|all]
exit /b 1

:done
echo Done.
exit /b 0

:build
rem %1 = build tag, %2 = output exe
set "TAG=%~1"
set "OUT=%~2"
echo Building %OUT% (tag: %TAG%)...
go build -tags %TAG% -ldflags "!LDFLAGS!" -o %OUT% .
if errorlevel 1 (
    echo %OUT% build failed.
    exit /b 1
)
call :upx %OUT%
exit /b 0

:upx
rem %1 = exe to compress
set "EXE=%~1"
if exist "%UPXEXE%" (
    "%UPXEXE%" --best --lzma "%EXE%"
    exit /b 0
)
where upx >nul 2>nul
if %errorlevel%==0 (
    upx --best --lzma "%EXE%"
    exit /b 0
)
echo WARNING: UPX not found ^(neither "%UPXEXE%" nor upx on PATH^) — skipping compression for %EXE%.
exit /b 0
