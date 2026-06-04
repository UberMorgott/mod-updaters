package engine

import (
	"fmt"
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

// --- bubbletea messages ---

type errorMsg struct{ err error }

type filesListedMsg struct {
	sftpClient *sftp.Client
	sshClient  *ssh.Client
	files      []sftpEntry // files needing download
	allEntries []sftpEntry // every SFTP entry seen (files + dirs) — used for cleanup keep-set
	totalSize  uint64
}

type progressTickMsg struct{}
type fileDownloadedMsg struct{}

// retryMsg — a connection attempt failed but spare attempts remain.
type retryMsg struct {
	attempt int // number of the NEXT attempt
	err     error
}

// startConnectMsg — time to start the next connection attempt (after the pause).
type startConnectMsg struct{ attempt int }

// connectFailedMsg — all attempts exhausted, server unreachable.
type connectFailedMsg struct{ err error }

// sshConfig builds the SSH client config for the given server.
func sshConfig(s ServerConfig) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:            s.Login,
		Auth:            []ssh.AuthMethod{ssh.Password(s.Password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
}

// dial opens an SSH + SFTP connection to the configured server.
func dial(s ServerConfig) (*ssh.Client, *sftp.Client, error) {
	sshClient, err := ssh.Dial("tcp", s.Host, sshConfig(s))
	if err != nil {
		return nil, nil, fmt.Errorf("SSH: %w", err)
	}
	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		sshClient.Close()
		return nil, nil, fmt.Errorf("SFTP: %w", err)
	}
	return sshClient, sftpClient, nil
}

// remoteRoot returns the absolute SFTP root for this config (no trailing slash).
func remoteRoot(cfg Config) string {
	return strings.TrimRight(cfg.Server.RemoteBase+cfg.RemoteSubdir, "/")
}

// doConnect performs a single connection attempt and builds the file lists.
// Whole-tree 1:1: walk remoteRoot, map each remote rel path to the same local
// rel path under the game root (cwd), download if missing or needsUpdate.
func doConnect(cfg Config) (filesListedMsg, error) {
	sshClient, sftpClient, err := dial(cfg.Server)
	if err != nil {
		return filesListedMsg{}, err
	}

	localRootAbs, _ := filepath.Abs(".")
	root := remoteRoot(cfg)

	var all, toDownload []sftpEntry
	var totalSize uint64

	walker := sftpClient.Walk(root)
	for walker.Step() {
		if walker.Err() != nil {
			continue
		}
		rPath := walker.Path()
		info := walker.Stat()

		rel := strings.TrimPrefix(rPath, root)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			continue
		}
		relSlash := filepath.ToSlash(rel)
		localPath := filepath.Join(localRootAbs, filepath.FromSlash(rel))

		entry := sftpEntry{RemotePath: rPath, LocalPath: localPath, RelSlash: relSlash, Info: info}
		all = append(all, entry)

		if info.IsDir() {
			continue
		}

		local, lerr := os.Stat(localPath)
		if os.IsNotExist(lerr) || (lerr == nil && needsUpdate(local, info)) {
			toDownload = append(toDownload, entry)
			totalSize += uint64(info.Size())
		}
	}

	return filesListedMsg{
		sftpClient: sftpClient,
		sshClient:  sshClient,
		files:      toDownload,
		allEntries: all,
		totalSize:  totalSize,
	}, nil
}

// needsUpdate reports whether the local file differs from remote by size or mtime.
func needsUpdate(local, remote os.FileInfo) bool {
	return local.Size() != remote.Size() ||
		!local.ModTime().Truncate(time.Second).Equal(remote.ModTime().Truncate(time.Second))
}
