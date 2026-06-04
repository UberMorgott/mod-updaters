@echo off
setlocal enabledelayedexpansion
rem build.bat [valheim^|windrose^|all]  (default all)
rem Builds the tagged updater exe(s) into the repo root, then UPX-compresses.
cd /d %~dp0

set "TARGET=%~1"
if "%TARGET%"=="" set "TARGET=all"

rem NOTE: do not name this var "UPX" — upx.exe reads %UPXEXE% as default options.
set "UPXEXE=Z:\SOFT\СЖАТИЕ EXE\upx.exe"

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
go build -tags %TAG% -ldflags "-s -w" -o %OUT% .
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
