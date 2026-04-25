# mod-updaters

Self-updating single-file Go TUIs that sync Morgott's mod pack from SFTP and launch the game. One exe per game, all committed at root of this repo.

## How friends use it

Set the Steam launch option for the game once. On every "Play" press, `curl -z` conditionally fetches the latest exe from this repo and runs it; the exe syncs the mods and starts the game.

### Windrose

```
cmd /c "curl -sSfL -z wur.exe -R -o wur.exe https://raw.githubusercontent.com/UberMorgott/mod-updaters/main/wur.exe && wur.exe %command%"
```

Syncs `/tmp/mnt/01DB6F2D5E1A6080/Windrose/` on the Keenetic SFTP into the Windrose game install. Three managed roots (declared in `WUR.go` `syncSpecs`):

- `paks/` → `Windrose\R5\Content\Paks\~mods\` — full mirror, deletes orphans (pak mods).
- `ue4ss/` → `Windrose\R5\Binaries\Win64\ue4ss\` — full mirror, deletes orphans (UE4SS install + Lua mods).
- `win64/` → `Windrose\R5\Binaries\Win64\` — additive only, never deletes (drops `dwmapi.dll` UE4SS proxy alongside game binaries).

### Valheim

```
cmd /c "curl -sSfL -z uar.exe -R -o uar.exe https://raw.githubusercontent.com/UberMorgott/mod-updaters/main/uar.exe && uar.exe %command%"
```

Syncs `/tmp/mnt/01DB6F2D5E1A6080/Valheim/` into the Valheim install directory. Cleanup restricted to `BepInEx/plugins`.

## Adding a new game

1. Copy `WUR.go` → `<X>UR.go`, change the seven constants at the top (`sftpServer`, `remoteDir`, `localSubpath`, `gameExecutable`, `tuiTitle`).
2. Add a line to `build.bat` and `compress.bat`.
3. `build.bat` then `compress.bat`. Commit + push.
4. Add the Steam launch command pattern to this README.

## Local development

- `build.bat` — compile both exes.
- `compress.bat` — UPX-compress both exes (requires `Z:\SOFT\СЖАТИЕ EXE\upx.exe`).
- Go 1.25.4+ required.
