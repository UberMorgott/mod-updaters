package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const (
	maxConnectAttempts = 3               // how many times we try to reach the server
	retryDelay         = 3 * time.Second // pause between attempts
	failNoticeDelay    = 5 * time.Second // how long the fail screen is shown before launching
)

// sftpEntry is one walked SFTP entry mapped 1:1 to a local path.
type sftpEntry struct {
	RemotePath string      // absolute SFTP path
	LocalPath  string      // absolute local path
	RelSlash   string      // path relative to the remote root, forward-slash form
	Info       os.FileInfo // remote file info
}

// conn is one SSH connection plus the SFTP session running over it.
type conn struct {
	ssh  *ssh.Client
	sftp *sftp.Client
}

// Close closes the SFTP session and the SSH connection. Safe on a nil conn.
func (c *conn) Close() {
	if c == nil {
		return
	}
	_ = c.sftp.Close() // best-effort: the SSH close below tears the session down anyway
	_ = c.ssh.Close()
}

// --- bubbletea messages ---

type errorMsg struct{ err error }

type filesListedMsg struct {
	conn       *conn
	files      []sftpEntry // files needing download
	allEntries []sftpEntry // every SFTP entry seen (files + dirs) — used for cleanup keep-set
	totalSize  uint64
	// complete is false when the walk hit an error or rejected an unsafe entry:
	// the keep-set may then be missing files, so cleanup must not run.
	complete bool
}

type progressTickMsg struct{}

// fileDownloadedMsg — the current file is done. conn is non-nil when the
// download had to reconnect: the model adopts it in place of the old (broken)
// connection so the next files don't each pay for a reconnect.
type fileDownloadedMsg struct{ conn *conn }

// retryMsg — a connection attempt failed but spare attempts remain.
type retryMsg struct {
	attempt int // number of the NEXT attempt
	err     error
}

// startConnectMsg — time to start the next connection attempt (after the pause).
type startConnectMsg struct{ attempt int }

// connectFailedMsg — all attempts exhausted, server unreachable.
type connectFailedMsg struct{ err error }

// sshConfig builds the SSH client config for the given server, pinning the
// server's host key from s.HostKey (a known_hosts-format line injected at build
// time). Returns an error if the host key is missing or unparsable — we never
// fall back to an insecure callback.
func sshConfig(s ServerConfig) (*ssh.ClientConfig, error) {
	if s.HostKey == "" {
		return nil, errors.New("no pinned host key configured")
	}
	_, _, key, _, _, err := ssh.ParseKnownHosts([]byte(s.HostKey))
	if err != nil {
		return nil, fmt.Errorf("parse host key: %w", err)
	}
	return &ssh.ClientConfig{
		User:              s.Login,
		Auth:              []ssh.AuthMethod{ssh.Password(s.Password)},
		HostKeyCallback:   ssh.FixedHostKey(key),
		HostKeyAlgorithms: []string{key.Type()},
		Timeout:           10 * time.Second,
	}, nil
}

// dial opens an SSH + SFTP connection to the configured server.
func dial(s ServerConfig) (*conn, error) {
	cfg, err := sshConfig(s)
	if err != nil {
		return nil, fmt.Errorf("SSH: %w", err)
	}
	sshClient, err := ssh.Dial("tcp", s.Host, cfg)
	if err != nil {
		return nil, fmt.Errorf("SSH: %w", err)
	}
	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		_ = sshClient.Close()
		return nil, fmt.Errorf("SFTP: %w", err)
	}
	return &conn{ssh: sshClient, sftp: sftpClient}, nil
}

// remoteRoot returns the absolute SFTP root for this config (no trailing slash).
func remoteRoot(cfg Config) string {
	return strings.TrimRight(cfg.Server.RemoteBase+cfg.RemoteSubdir, "/")
}

// localRel maps a walked remote path to a local path relative to the game root.
// The file names come from the server, so anything that could escape the game
// root (".." elements, absolute/drive paths, backslashes, ':' streams, reserved
// Windows names like NUL) is rejected — filepath.Localize does all of that.
// ok is false for the root itself and for rejected paths.
func localRel(root, remotePath string) (relSlash, rel string, ok bool) {
	relSlash, found := strings.CutPrefix(remotePath, root+"/")
	if !found {
		return "", "", false
	}
	rel, err := filepath.Localize(relSlash)
	if err != nil || rel == "." {
		return "", "", false
	}
	return relSlash, rel, true
}

// doConnect performs a single connection attempt and builds the file lists.
// Whole-tree 1:1: walk remoteRoot, map each remote rel path to the same local
// rel path under the game root (cwd), download if missing or needsUpdate.
func doConnect(cfg Config) (filesListedMsg, error) {
	localRootAbs, err := filepath.Abs(".")
	if err != nil {
		return filesListedMsg{}, err
	}
	c, err := dial(cfg.Server)
	if err != nil {
		return filesListedMsg{}, err
	}

	root := remoteRoot(cfg)
	var all, toDownload []sftpEntry
	var totalSize uint64
	complete := true

	walker := c.sftp.Walk(root)
	for walker.Step() {
		if walker.Err() != nil {
			// Unreadable entry/dir: keep going (download what we can) but the
			// listing is partial, so cleanup is disabled for this run.
			complete = false
			continue
		}
		rPath := walker.Path()
		if rPath == root {
			continue
		}
		info := walker.Stat()

		relSlash, rel, ok := localRel(root, rPath)
		if !ok {
			// Unsafe name from the server — never map it to a local path.
			complete = false
			if info.IsDir() {
				walker.SkipDir()
			}
			continue
		}

		entry := sftpEntry{
			RemotePath: rPath,
			LocalPath:  filepath.Join(localRootAbs, rel),
			RelSlash:   relSlash,
			Info:       info,
		}
		all = append(all, entry)

		if info.IsDir() {
			continue
		}

		local, lerr := os.Stat(entry.LocalPath)
		if errors.Is(lerr, fs.ErrNotExist) || (lerr == nil && needsUpdate(local, info)) {
			toDownload = append(toDownload, entry)
			totalSize += fileSize(info)
		}
	}

	return filesListedMsg{
		conn:       c,
		files:      toDownload,
		allEntries: all,
		totalSize:  totalSize,
		complete:   complete,
	}, nil
}

// fileSize returns info.Size() as uint64, clamping a (never expected) negative
// size to 0.
func fileSize(info os.FileInfo) uint64 {
	if s := info.Size(); s > 0 {
		return uint64(s)
	}
	return 0
}

// needsUpdate reports whether the local file differs from remote by size or mtime.
func needsUpdate(local, remote os.FileInfo) bool {
	return local.Size() != remote.Size() ||
		!local.ModTime().Truncate(time.Second).Equal(remote.ModTime().Truncate(time.Second))
}
