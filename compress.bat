@echo off
rem Compresses both exes with UPX.
cd /d %~dp0

set UPX="Z:\SOFT\СЖАТИЕ EXE\upx.exe"

if not exist uar.exe (
    echo uar.exe not found. Run build.bat first.
    pause
    exit /b 1
)
if not exist wur.exe (
    echo wur.exe not found. Run build.bat first.
    pause
    exit /b 1
)

%UPX% --best --lzma uar.exe
%UPX% --best --lzma wur.exe

echo Compressed uar.exe and wur.exe.
