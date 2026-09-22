package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const (
	maxConnectAttempts = 3                // how many times we try to reach the server
	dialTimeout        = 10 * time.Second // TCP connect
	handshakeTimeout   = 20 * time.Second // SSH handshake + SFTP session start
	ioTimeout          = 30 * time.Second // no traffic for this long = dead link
	keepaliveInterval  = 15 * time.Second // SSH keepalive period (keeps idle links under ioTimeout)
	retryBaseDelay     = 2 * time.Second  // first retry pause, doubled per failure
	retryMaxDelay      = 30 * time.Second // cap for the doubled pause
)

// retryDelay returns the backoff before retry number n (1-based):
// 2s, 4s, 8s, … capped at retryMaxDelay.
func retryDelay(n int) time.Duration {
	d := retryBaseDelay
	for i := 1; i < n && d < retryMaxDelay; i++ {
		d *= 2
	}
	return min(d, retryMaxDelay)
}

// isPermanent reports errors that a reconnect cannot fix (missing file,
// permission denied — remote or local): they fail at once instead of retrying.
func isPermanent(err error) bool {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return true
	}
	var se *sftp.StatusError
	return errors.As(err, &se) &&
		(se.FxCode() == sftp.ErrSSHFxNoSuchFile || se.FxCode() == sftp.ErrSSHFxPermissionDenied)
}

// isConnError reports whether an error during the walk means the connection
// itself failed (anything that is not an SFTP status reply about one entry).
func isConnError(err error) bool {
	var se *sftp.StatusError
	return !errors.As(err, &se) && !isPermanent(err)
}

// deadlineConn refreshes an inactivity deadline before every Read/Write, so a
// stalled link errors out (into the retry path) instead of hanging forever.
type deadlineConn struct {
	net.Conn
	timeout time.Duration
}

func (c *deadlineConn) Read(p []byte) (int, error) {
	if err := c.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func (c *deadlineConn) Write(p []byte) (int, error) {
	if err := c.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

// keepalive pings the server every keepaliveInterval. A failed or unanswered
// ping closes the client, which fails any in-flight SFTP call. It exits once
// the client is closed (the next ping errors).
func keepalive(client *ssh.Client) {
	t := time.NewTicker(keepaliveInterval)
	defer t.Stop()
	for range t.C {
		timer := time.AfterFunc(ioTimeout, func() { _ = client.Close() })
		_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
		timer.Stop()
		if err != nil {
			_ = client.Close()
			return
		}
	}
}

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

// Close closes the SSH connection (TCP) first, so the SFTP close after it can
// never block on a dead link. May still take a moment: callers in the TUI run
// it off the Update loop (closeCmd). Safe on a nil conn.
func (c *conn) Close() {
	if c == nil {
		return
	}
	_ = c.ssh.Close()  // best-effort: may already be closed by keepalive
	_ = c.sftp.Close() // best-effort: the session died with the SSH connection
}

// --- bubbletea messages ---

type errorMsg struct{ err error }

type filesListedMsg struct {
	conn       *conn
	files      []sftpEntry // files needing download
	allEntries []sftpEntry // every SFTP entry seen (files + dirs) — used for cleanup keep-set
	totalSize  uint64
	// complete is false when the walk hit an error or skipped an entry (unsafe
	// name, dir symlink, special file): the keep-set may then be missing files,
	// so cleanup must not run.
	complete bool
	// walkErr is the first per-entry walk error (not a skipped entry): the user
	// is warned via the menu instead of a silent "success".
	walkErr error
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
	}, nil
}

// dial opens an SSH + SFTP connection to the configured server. Every stage is
// bounded: TCP connect by dialTimeout, handshake + SFTP start by
// handshakeTimeout, and afterwards any ioTimeout without traffic (keepalive
// keeps a healthy idle link busy) kills the connection.
func dial(s ServerConfig) (*conn, error) {
	cfg, err := sshConfig(s)
	if err != nil {
		return nil, fmt.Errorf("SSH: %w", err)
	}
	d := net.Dialer{Timeout: dialTimeout}
	raw, err := d.DialContext(context.Background(), "tcp", s.Host)
	if err != nil {
		return nil, fmt.Errorf("SSH: %w", err)
	}
	nc := &deadlineConn{Conn: raw, timeout: ioTimeout}
	// The per-Read deadline alone would let a trickling handshake run forever.
	timer := time.AfterFunc(handshakeTimeout, func() { _ = raw.Close() })
	defer timer.Stop()

	sc, chans, reqs, err := ssh.NewClientConn(nc, s.Host, cfg)
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("SSH: %w", err)
	}
	sshClient := ssh.NewClient(sc, chans, reqs)
	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		_ = sshClient.Close()
		return nil, fmt.Errorf("SFTP: %w", err)
	}
	if !timer.Stop() {
		// The handshake timer fired just as setup finished: the link is closed.
		_ = sshClient.Close()
		return nil, errors.New("SSH: handshake timeout")
	}
	go keepalive(sshClient)
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
	var walkErr error

	// walkFailed handles a per-entry walk error. A dead connection aborts the
	// attempt (it is retried like a failed connect); anything else leaves a
	// partial listing: download what we can, but no cleanup and warn the user.
	walkFailed := func(err error) error {
		if isConnError(err) {
			c.Close()
			return fmt.Errorf("SFTP walk: %w", err)
		}
		complete = false
		if walkErr == nil {
			walkErr = err
		}
		return nil
	}

	walker := c.sftp.Walk(root)
	for walker.Step() {
		if err := walker.Err(); err != nil {
			if ferr := walkFailed(err); ferr != nil {
				return filesListedMsg{}, ferr
			}
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

		// The walk uses Lstat. A symlink to a file is followed (Stat) and synced
		// as that file: Open follows it too, so sizes agree. A dir symlink or a
		// special file can't be synced 1:1 (the walk doesn't descend into it,
		// its Lstat size never matches the download) — skip it, no cleanup.
		if info.Mode()&fs.ModeSymlink != 0 {
			target, serr := c.sftp.Stat(rPath)
			if serr != nil {
				if ferr := walkFailed(serr); ferr != nil {
					return filesListedMsg{}, ferr
				}
				continue
			}
			info = target
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			complete = false
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
		walkErr:    walkErr,
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
