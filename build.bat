@echo off
rem Builds both updater exes into the repo root.
cd /d %~dp0

go build -o uar.exe UAR.go
if %errorlevel% neq 0 (
    echo UAR build failed.
    pause
    exit /b 1
)

go build -o wur.exe WUR.go
if %errorlevel% neq 0 (
    echo WUR build failed.
    pause
    exit /b 1
)

echo Built uar.exe and wur.exe successfully.
