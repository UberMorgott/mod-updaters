# mod-updaters

Self-updating Go TUIs that sync Morgott's mod pack from SFTP and launch the
game. One shared `engine` package; one exe per game, built with a build tag and
committed at the repo root.

## How friends use it

Set the Steam launch option for the game once. On every "Play" press, `curl -z`
conditionally fetches the latest exe from this repo and runs it; the exe syncs
the mods and starts the game. If the update server is unreachable the exe retries
a few times, then shows a notice and launches the game anyway (no update).

### Windrose

```
cmd /c "curl -sSfL -z wur.exe -R -o wur.exe https://raw.githubusercontent.com/UberMorgott/mod-updaters/main/wur.exe && wur.exe %command%"
```

Syncs `…/modpacks/Windrose/` on the SFTP server 1:1 into the Windrose install.
Cleanup (delete orphan mods) restricted to immediate subdirs of:
- `R5\Binaries\Win64\ue4ss\Mods`
- `R5\Content\Paks\~mods\~mods`

Everything else is additive (never deleted) — game binaries, `dwmapi.dll`,
`mods.txt`, etc. survive.

### Valheim

```
cmd /c "curl -sSfL -z uar.exe -R -o uar.exe https://raw.githubusercontent.com/UberMorgott/mod-updaters/main/uar.exe && uar.exe %command%"
```

Syncs `…/modpacks/Valheim/` 1:1 into the Valheim install. Cleanup restricted to
`BepInEx/plugins` (full mirror — orphan files and dirs there are deleted).

## Architecture

- `engine/` — shared logic: SFTP connect+retry, walk/diff, resumable downloads
  (`.part` + `.meta` sidecar), mirror cleanup, TUI (progress + fail screen), launch.
- `server.go` — wires the SFTP connection settings (server, login, password,
  `RemoteBase`, pinned `HostKey`) into `engine.ServerConfig`. The values are
  empty in source and **injected at build time** via `-ldflags -X` from a local,
  gitignored `config.txt` — they never live in the repo, only in the built exe.
- `game_<name>.go` — per-game `engine.Config` behind `//go:build <name>`
  (game subdir, executable, launch args, cleanup specs).
- `main.go` — `engine.Run(gameConfig)`.

The SFTP account is read-only and confined to its mod-pack directory, so the
embedded credentials grant nothing beyond reading the mod files.

## Configuration

1. Copy `config.example.txt` → `config.txt` (gitignored) and fill in `server`,
   `login`, `password`, `remote_base`, and `host_key`
   (`ssh-keyscan -t ed25519 <host>` — used for SSH host-key pinning).
2. `build.bat` reads `config.txt` and bakes the values into the exes. To build a
   single game by hand you must pass the same `-X main.cfg*` ldflags, otherwise
   the exe is built with empty connection settings.

## Adding a new game

1. Create `game_<x>.go` with `//go:build <x>` and a `var gameConfig = engine.Config{...}`
   (set `Server: Server`, `RemoteSubdir`, `GameExecutable`, `LaunchArgs`, `Cleanup`).
2. Add the tag to the build constraint in `main.go` and the tag→exe map in `build.bat`.
3. `build.bat <x>`, commit + push.
4. Add the Steam launch line to this README.

## Local development

- `build.bat [valheim|windrose|all]` — build (`-ldflags "-s -w"`) + UPX-compress
  (`Z:\SOFT\СЖАТИЕ EXE\upx.exe`, falls back to `upx` on PATH, else uncompressed).
- Build a single game manually: `go build -tags valheim -ldflags "-s -w" -o uar.exe .`
- Go 1.26+ required.
