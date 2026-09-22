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
	downloadBufSize    = 128 * 1024 // 128 KiB copy buffer
	maxDownloadRetries = 8          // reconnect+resume attempts in a row without progress
)

// retryBudget limits the automatic retries of one download. Any progress (the
// .part grew since the previous attempt) refills the budget, so a flaky but
// working link finishes the file while a dead one gives up.
type retryBudget struct {
	failures int   // failed attempts since the last progress
	lastSize int64 // .part size at the previous attempt
}

// next records a failed attempt that left the .part at size. It returns the
// pause before the next attempt, or false once the budget is exhausted.
func (b *retryBudget) next(size int64) (time.Duration, bool) {
	if size > b.lastSize {
		b.failures = 0
	}
	b.lastSize = size
	b.failures++
	if b.failures > maxDownloadRetries {
		return 0, false
	}
	return retryDelay(b.failures), true
}

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

// resetPart drops any .part/.meta and writes a fresh meta for info, so the
// download starts over from byte 0.
func resetPart(partPath string, info os.FileInfo) error {
	_ = os.Remove(partPath)           // may not exist
	_ = os.Remove(metaPath(partPath)) // may not exist
	return writeMeta(partPath, info)
}

// checkPartSize fails unless the .part holds exactly want bytes.
func checkPartSize(partPath string, want int64) error {
	st, err := os.Stat(partPath)
	if err != nil {
		return err
	}
	if st.Size() != want {
		return fmt.Errorf("неполная загрузка %s: %d/%d байт",
			filepath.Base(partPath), st.Size(), want)
	}
	return nil
}

// downloadFile performs a resumable download of one entry. It resumes from an
// existing matching .part and retries transient errors with reconnect +
// resume (re-checking the remote file first: if it changed, the .part is
// dropped and the file restarts). Permanent errors fail at once. Only a copy
// whose local writes all succeeded AND whose size matches is finalized.
//
// If a retry had to open a new connection and the download then succeeded, that
// connection is returned (the caller owns it and should use it from now on);
// otherwise the returned conn is nil.
func downloadFile(cfg Config, c *conn, file sftpEntry, ch chan<- uint64) (*conn, error) {
	localPath := file.LocalPath
	partPath := localPath + ".part"
	info := file.Info

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return nil, err
	}

	// Decide whether we can resume: .part exists AND meta matches current remote.
	var startOffset int64
	if metaMatches(partPath, info) {
		startOffset = partOffset(partPath, info.Size())
	}
	if startOffset == 0 {
		if err := resetPart(partPath, info); err != nil {
			return nil, err
		}
	} else {
		// Report already-present bytes so the overall bar reflects resumed progress.
		ch <- uint64(startOffset)
	}

	// A connection opened for a retry replaces the previous retry connection
	// (closed first), so at most one extra connection is alive at a time.
	var retryConn *conn
	budget := retryBudget{lastSize: startOffset}

	var err error
	for {
		if err != nil {
			if isPermanent(err) {
				break
			}
			delay, ok := budget.next(partOffset(partPath, info.Size()))
			if !ok {
				break
			}
			time.Sleep(delay)
			retryConn.Close()
			retryConn = nil
			var nc *conn
			if nc, err = dial(cfg.Server); err != nil {
				continue
			}
			retryConn, c = nc, nc
			if info, err = refreshRemote(c, file.RemotePath, partPath, info); err != nil {
				continue
			}
			startOffset = partOffset(partPath, info.Size())
		}

		err = copyFromOffset(c, file.RemotePath, partPath, startOffset, ch)
		if err == nil {
			err = checkPartSize(partPath, info.Size())
		}
		if err == nil {
			err = finalize(partPath, localPath, info.ModTime())
		}
		if err == nil {
			return retryConn, nil
		}
	}
	retryConn.Close()
	return nil, err
}

// refreshRemote re-stats the remote file before a retry. If it changed since
// the .part was started (meta mismatch), the .part is dropped so the retry
// starts over from 0 with a fresh meta. Returns the current remote info.
func refreshRemote(c *conn, remotePath, partPath string, old os.FileInfo) (os.FileInfo, error) {
	info, err := c.sftp.Stat(remotePath)
	if err != nil {
		return old, err
	}
	if err := reconcilePart(partPath, info); err != nil {
		return old, err
	}
	return info, nil
}

// reconcilePart keeps the .part only if its meta still matches the remote
// info; otherwise it is dropped and a fresh meta written (restart from 0).
func reconcilePart(partPath string, info os.FileInfo) error {
	if metaMatches(partPath, info) {
		return nil
	}
	return resetPart(partPath, info)
}

// finalize stamps the remote mtime on the closed .part, moves it into place,
// then drops the meta. os.Rename replaces an existing file on Windows too, so
// the old version stays intact if anything before the rename fails.
func finalize(partPath, localPath string, mtime time.Time) error {
	// mtime must match remote, otherwise needsUpdate re-downloads next run.
	if err := os.Chtimes(partPath, time.Now(), mtime); err != nil {
		return fmt.Errorf("chtimes %s: %w", filepath.Base(localPath), err)
	}
	if err := os.Rename(partPath, localPath); err != nil {
		return fmt.Errorf("rename %s: %w", filepath.Base(localPath), err)
	}
	_ = os.Remove(metaPath(partPath)) // best-effort: orphan meta is ignored without its .part
	return nil
}

// copyFromOffset opens the remote file, seeks to offset, appends to the .part
// file from there, streaming progress, then syncs and closes the .part.
// Returns the first read/write/sync/close error.
func copyFromOffset(c *conn, remotePath, partPath string, offset int64, ch chan<- uint64) error {
	src, err := c.sftp.Open(remotePath)
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

	err = copyProgress(dst, src, ch)
	if err == nil {
		err = dst.Sync()
	}
	if cErr := dst.Close(); err == nil {
		err = cErr
	}
	return err
}

// copyProgress copies src to dst until EOF, reporting each chunk on ch.
func copyProgress(dst io.Writer, src io.Reader, ch chan<- uint64) error {
	buf := make([]byte, downloadBufSize)
	for {
		n, rErr := src.Read(buf)
		if n > 0 {
			if _, wErr := dst.Write(buf[:n]); wErr != nil {
				return wErr
			}
			ch <- uint64(n)
		}
		if errors.Is(rErr, io.EOF) {
			return nil
		}
		if rErr != nil {
			return rErr
		}
	}
}
