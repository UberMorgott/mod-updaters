package engine

// ServerConfig describes the SFTP server holding the modpacks.
// RemoteBase must end with "/".
type ServerConfig struct {
	Host, Login, Password, RemoteBase string
	// HostKey is a known_hosts-format line used to pin the server's SSH host
	// key (ssh.ParseKnownHosts). Injected at build time; empty in source.
	HostKey string
}

// SyncMode selects how a Cleanup path prunes orphans.
type SyncMode int

const (
	// MirrorFiles deletes orphan files AND dirs under the path (full mirror).
	MirrorFiles SyncMode = iota
	// MirrorSubdirs deletes only orphan immediate subdirs; keeps files at that level.
	MirrorSubdirs
)

// CleanupSpec governs deletion under one path. Path is relative to the game
// root, forward-slash form.
type CleanupSpec struct {
	Path string
	Mode SyncMode
}

// Config is the per-game configuration passed to Run.
type Config struct {
	GameName       string   // TUI title
	GameExecutable string   // e.g. "valheim.exe"
	LaunchArgs     []string // e.g. ["-console"]
	Version        string
	Server         ServerConfig
	RemoteSubdir   string        // appended to Server.RemoteBase, ends with "/"
	Cleanup        []CleanupSpec // empty = additive-only, never delete
}
