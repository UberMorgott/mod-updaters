package engine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	downloadBufSize      = 128 * 1024 // 128 KiB copy buffer
	maxDownloadRetries   = 3          // reconnect+resume attempts on transient mid-file error
	downloadRetryBackoff = 2 * time.Second
)

// downloadNext returns a tea.Cmd that downloads the current file, reporting
// byte progress over a fresh channel stored on the model.
func (m *model) downloadNext() tea.Cmd {
	if m.currentIdx >= len(m.files) {
		return nil
	}

	file := m.files[m.currentIdx]
	cfg := m.cfg
	c := m.conn
	ch := make(chan uint64, 1000)
	m.progressChan = ch

	return func() tea.Msg {
		defer close(ch)
		if c == nil {
			return errorMsg{errors.New("нет SFTP соединения")}
		}
		newConn, err := downloadFile(cfg, c, file, ch)
		if err != nil {
			return errorMsg{err}
		}
		return fileDownloadedMsg{conn: newConn}
	}
}

// metaPath returns the sidecar meta path for a given .part path.
func metaPath(partPath string) string { return partPath + ".meta" }

// writeMeta stores the remote size+mtime alongside the .part file.
func writeMeta(partPath string, info os.FileInfo) error {
	content := fmt.Sprintf("%d\n%d\n", info.Size(), info.ModTime().Unix())
	return os.WriteFile(metaPath(partPath), []byte(content), 0644)
}

// metaMatches reads a sidecar meta and reports whether it matches the given
// remote info (size + mtime, second precision).
func metaMatches(partPath string, info os.FileInfo) bool {
	data, err := os.ReadFile(metaPath(partPath))
	if err != nil {
		return false
	}
	parts := strings.Fields(string(data))
	if len(parts) != 2 {
		return false
	}
	size, err1 := strconv.ParseInt(parts[0], 10, 64)
	mtime, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return false
	}
	return size == info.Size() && mtime == info.ModTime().Unix()
}

// partOffset returns how many bytes of the .part can be resumed from: its
// size, or 0 if it is missing or larger than the remote file (stale).
func partOffset(partPath string, remoteSize int64) int64 {
	st, err := os.Stat(partPath)
	if err != nil || st.Size() > remoteSize {
		return 0
	}
	return st.Size()
}

// downloadFile performs a resumable download of one entry. It resumes from an
// existing matching .part, retries transient mid-file errors with reconnect +
// resume, then on success verifies size, renames to final, sets mtime, and
// removes the meta sidecar.
//
// If a retry had to open a new connection and the download then succeeded, that
// connection is returned (the caller owns it and should use it from now on);
// otherwise the returned conn is nil.
func downloadFile(cfg Config, c *conn, file sftpEntry, ch chan<- uint64) (*conn, error) {
	localPath := file.LocalPath
	partPath := localPath + ".part"

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return nil, err
	}

	// Decide whether we can resume: .part exists AND meta matches current remote.
	var startOffset int64
	if metaMatches(partPath, file.Info) {
		startOffset = partOffset(partPath, file.Info.Size())
	}
	if startOffset == 0 {
		// Fresh start: drop any stale part/meta and write a fresh meta.
		_ = os.Remove(partPath)           // may not exist
		_ = os.Remove(metaPath(partPath)) // may not exist
		if err := writeMeta(partPath, file.Info); err != nil {
			return nil, err
		}
	} else {
		// Report already-present bytes so the overall bar reflects resumed progress.
		ch <- uint64(startOffset)
	}

	// A connection opened for a retry replaces the previous retry connection
	// (closed first), so at most one extra connection is alive at a time.
	var retryConn *conn

	var lastErr error
	for attempt := range maxDownloadRetries {
		if attempt > 0 {
			// Reconnect and re-resume from however much is on disk now.
			time.Sleep(downloadRetryBackoff)
			retryConn.Close()
			retryConn = nil
			nc, derr := dial(cfg.Server)
			if derr != nil {
				lastErr = derr
				continue
			}
			retryConn, c = nc, nc
			startOffset = partOffset(partPath, file.Info.Size())
		}

		lastErr = copyFromOffset(c, file, partPath, startOffset, ch)
		if lastErr == nil {
			break
		}
		// On the next loop, we will reconnect and resume.
	}

	// Verify the part is complete; if not and the loop exhausted with an error,
	// surface that error.
	st, err := os.Stat(partPath)
	switch {
	case err == nil && st.Size() == file.Info.Size():
		err = finalize(partPath, localPath, file.Info.ModTime())
	case lastErr != nil:
		err = lastErr
	case err == nil:
		err = fmt.Errorf("неполная загрузка %s: %d/%d байт",
			filepath.Base(localPath), st.Size(), file.Info.Size())
	}
	if err != nil {
		retryConn.Close()
		return nil, err
	}
	return retryConn, nil
}

// finalize moves a complete .part into place and stamps the remote mtime.
// os.Rename replaces an existing file on Windows too, so the old version stays
// intact if the rename fails.
func finalize(partPath, localPath string, mtime time.Time) error {
	if err := os.Rename(partPath, localPath); err != nil {
		return fmt.Errorf("rename %s: %w", filepath.Base(localPath), err)
	}
	_ = os.Remove(metaPath(partPath)) // best-effort: orphan meta is ignored without its .part
	// mtime must match remote, otherwise needsUpdate re-downloads next run.
	if err := os.Chtimes(localPath, time.Now(), mtime); err != nil {
		return fmt.Errorf("chtimes %s: %w", filepath.Base(localPath), err)
	}
	return nil
}

// copyFromOffset opens the remote file, seeks to offset, appends to the .part
// file from there, streaming progress. Returns an error on any read/write
// failure (treated as transient by the caller until retries are exhausted).
func copyFromOffset(c *conn, file sftpEntry, partPath string, offset int64, ch chan<- uint64) error {
	src, err := c.sftp.Open(file.RemotePath)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	if offset > 0 {
		if _, err := src.Seek(offset, io.SeekStart); err != nil {
			return err
		}
	}

	// Open .part for append (create if missing).
	flag := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	dst, err := os.OpenFile(partPath, flag, 0644) //nolint:gosec // G304: partPath is derived from the local game root, not external input
	if err != nil {
		return err
	}

	buf := make([]byte, downloadBufSize)
	for {
		n, rErr := src.Read(buf)
		if n > 0 {
			if _, wErr := dst.Write(buf[:n]); wErr != nil {
				_ = dst.Close()
				return wErr
			}
			ch <- uint64(n)
		}
		if errors.Is(rErr, io.EOF) {
			break
		}
		if rErr != nil {
			_ = dst.Close()
			return rErr
		}
	}
	return dst.Close()
}
