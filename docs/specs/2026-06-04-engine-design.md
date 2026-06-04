# mod-updaters — engine refactor design

Date: 2026-06-04

## Goal

Collapse the copy-paste per-game updaters (`UAR.go`, `WUR.go`) into one shared
`engine` package + tiny per-game config files selected by Go build tags. Add
resumable downloads, connection retries with a fail screen, and refresh the
toolchain/deps. Output exe names stay `uar.exe` / `wur.exe` at repo root
(friends fetch them by name via Steam launch `curl -z`).

## Constraints (hard)

- Output: `go build -tags <game> ... -o <exe>` must still produce `uar.exe`,
  `wur.exe` at repo root. Names must not change.
- SFTP creds stay embedded (verified low-risk: `modman` is read-only, no shell,
  now confined to `Share/modpacks`). No transport change.
- Behaviour parity with current exes for the sync/cleanup of each game — must
  not over-delete. Verify before replacing deployed exes.
- Windows / PowerShell build host. Go via `go-sdk`.

## Architecture

```
mod-updaters/
├─ engine/
│  ├─ config.go    # Config, ServerConfig, CleanupSpec, SyncMode types
│  ├─ engine.go    # Run(cfg) — bubbletea model, Update, View dispatch
│  ├─ connect.go   # connect+retry, SFTP walk, diff (download set + dir set)
│  ├─ download.go  # resumable per-file download (.part + .meta sidecar)
│  ├─ mirror.go    # cleanup: MirrorFiles / MirrorSubdirs
│  ├─ tui.go       # progress view + red fail-screen view
│  └─ launch.go    # launchGame()
├─ server.go       # shared, NO build tag: var Server = engine.ServerConfig{...}
├─ game_valheim.go # //go:build valheim  → var gameConfig = engine.Config{...}
├─ game_windrose.go# //go:build windrose → var gameConfig = engine.Config{...}
├─ main.go         # //go:build valheim || windrose ; func main(){ engine.Run(gameConfig) }
├─ build.bat       # per-tag build (-s -w) + UPX, outputs uar.exe/wur.exe
└─ go.mod
```

Build: `go build -tags valheim -ldflags "-s -w" -o uar.exe .`
(`.` builds the package = main.go + server.go + the one game_*.go whose tag is active).

## Config schema (Go, type-safe)

```go
type ServerConfig struct {
    Host, Login, Password, RemoteBase string // RemoteBase ends with "/"
}

type SyncMode int
const (
    MirrorFiles    SyncMode = iota // delete orphan files AND dirs under path
    MirrorSubdirs                  // delete only orphan immediate subdirs; keep files at that level
)

type CleanupSpec struct {
    Path string   // relative to game root, forward-slash
    Mode SyncMode
}

type Config struct {
    GameName       string   // TUI title
    GameExecutable string   // e.g. "valheim.exe"
    LaunchArgs     []string // e.g. ["-console"]
    Version        string
    Server         ServerConfig
    RemoteSubdir   string        // appended to Server.RemoteBase, ends with "/"
    Cleanup        []CleanupSpec // empty = additive-only, never delete
}
```

Download is ALWAYS whole-tree 1:1: walk `Server.RemoteBase+RemoteSubdir`, map each
remote rel path to the same local rel path under the game root (cwd), download if
missing or `needsUpdate` (size or mtime differ). `Cleanup` only governs deletion.
Outside `Cleanup` paths nothing is ever deleted.

### Shared server (server.go, no tag)
```
Host="morgott.keenetic.pro:22", Login="modman", Password="Br2ctG7FGSqPhr4",
RemoteBase="/tmp/mnt/01DB6F2D5E1A6080/modpacks/"
```

### game_valheim.go (//go:build valheim) → uar.exe
- GameName "Valheim", GameExecutable "valheim.exe", LaunchArgs ["-console"]
- RemoteSubdir "Valheim/"
- Cleanup: [{ "BepInEx/plugins", MirrorFiles }]
  (matches current UAR: 1:1 download, full mirror only inside BepInEx/plugins)

### game_windrose.go (//go:build windrose) → wur.exe
- GameName "Windrose", GameExecutable "Windrose.exe", LaunchArgs ["-console"]
- RemoteSubdir "Windrose/"
- Cleanup (matches current WUR fullMirrorDirs, MirrorSubdirs semantics):
  - { "R5/Binaries/Win64/ue4ss/Mods", MirrorSubdirs }
  - { "R5/Content/Paks/~mods/~mods", MirrorSubdirs }
- Keep WUR's sanityCheck: refuse to run if GameExecutable missing next to exe;
  validate Cleanup paths are relative, no "..", not absolute.

## Download layer (resume)

- Download to `<local>.part`; sidecar `<local>.part.meta` stores remote size+mtime.
- On (re)start of a file: if `.part` exists AND `.meta` matches current remote
  (size+mtime) → `Seek` remote to len(part), open `.part` append, continue.
  Else discard `.part` and start fresh.
- On transient read/connection error mid-file: reconnect + resume, up to N retries
  (e.g. 3) with backoff. Only after exhausting → treat as download failure.
- On completion: verify written size == remote size, `Rename` `.part` → final,
  `Chtimes` to remote mtime, remove `.meta`.
- Keep 128 KiB buffer + progress channel (uint64 bytes) as current.

## Retry + fail screen (from local E:\DEV\UAR4\UAR.go work)

- Connection: up to `maxConnectAttempts` (3), `retryDelay` (3s) between, status
  shows "Сервер обновлений не отвечает, попытка X/3...".
- All attempts fail → full-screen red `failView` (double border, white-on-red
  banner "⚠ ОБНОВЛЕНИЕ НЕ УДАЛОСЬ ⚠", "Сервер обновлений не отвечает", "Игра будет
  запущена БЕЗ обновления модов"), hold `failNoticeDelay` (5s), then launch game,
  quit. Reuse the implementation already written in `E:\DEV\UAR4\UAR.go`
  (`failed` field, `failView()`, `connectFailedMsg`, `retryMsg`, `startConnectMsg`,
  `delayCmd`, `connectAndListFiles(attempt)`).
- Download failure after retries → same failView + launch (priority: let user in).

## Safety guard (already shipped, keep)

If the remote listing is empty (remote dir missing/unreachable), do NOT run any
cleanup — just launch. (`len(allEntries)==0` short-circuit in syncAndLaunch.)
This is independent of, and in addition to, the connection retry/failView.

## Build pipeline (build.bat)

- `build.bat [valheim|windrose|all]` (default all).
- Per game: `go build -tags <tag> -ldflags "-s -w" -o <exe> .` then
  `upx --best --lzma <exe>`. UPX path `Z:\SOFT\СЖАТИЕ EXE\upx.exe`, fallback `upx`
  on PATH; if absent, build uncompressed + warn (don't fail).
- Tag→exe map: valheim→uar.exe, windrose→wur.exe.
- Drop the old single-file `go build UAR.go` / `WUR.go` lines.

## Toolchain / deps update

- Bump `go` directive in go.mod to the latest stable installed; `go get -u ./...`
  for bubbletea, bubbles, lipgloss, pkg/sftp, golang.org/x/crypto; `go mod tidy`.
  Rebuild + re-test after.

## Adding a new game (new README section)

1. Create `game_<x>.go` with `//go:build <x>` and a `var gameConfig = engine.Config{...}`.
2. Add the tag to `main.go` build constraint and the tag→exe map in `build.bat`.
3. `build.bat <x>`, commit + push, add the Steam launch line to README.

## Testing / acceptance (before replacing deployed exes)

- Builds: `-tags valheim` → uar.exe, `-tags windrose` → wur.exe, both compile.
- Parity (headless, against live SFTP `modpacks`):
  - Valheim: fresh dir downloads 331 files (~1258 MB); re-run = "все актуальны",
    no re-download; mirror only touches BepInEx/plugins.
  - A planted orphan dll under BepInEx/plugins gets deleted; a planted file outside
    plugins survives.
  - Windrose: orphan subdir under the two mirror roots deleted; top-level files
    (mods.txt etc.) survive; win64 binaries never deleted.
- Resume: interrupt a large file mid-download, re-run → resumes from `.part`,
  final size correct.
- Retry/failView: block host (hosts → 10.255.255.1) → 3 attempts, red screen,
  game launches.
- Guard: missing remote dir → no wipe, launches.
- Smoke: UPX'd exes run.

## Out of scope

- Transport change (HTTPS/Cloudflare) — not now.
- Obfuscation of the embedded password — rejected as theater; secret already
  de-risked by router confinement.
