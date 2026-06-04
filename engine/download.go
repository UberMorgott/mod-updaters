package engine

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const (
	downloadBufSize     = 128 * 1024     // 128 KiB copy buffer
	maxDownloadRetries  = 3              // reconnect+resume attempts on transient mid-file error
	downloadRetryBackof = 2 * time.Second
)

// downloadNext returns a tea.Cmd that downloads the current file, reporting
// byte progress over a fresh channel stored on the model.
func (m *model) downloadNext() tea.Cmd {
	if m.currentIdx >= len(m.files) {
		return nil
	}

	file := m.files[m.currentIdx]
	cfg := m.cfg
	client := m.sftp
	ch := make(chan uint64, 1000)
	m.progressChan = ch

	return func() tea.Msg {
		defer close(ch)
		if client == nil {
			return errorMsg{fmt.Errorf("нет SFTP соединения")}
		}
		if err := downloadFile(cfg, client, file, ch); err != nil {
			return errorMsg{err}
		}
		return fileDownloadedMsg{}
	}
}

// metaPath returns the sidecar meta path for a given .part path.
func metaPath(partPath string) string { return partPath + ".meta" }

// writeMeta stores the remote size+mtime alongside the .part file.
func writeMeta(partPath string, info os.FileInfo) error {
	content := fmt.Sprintf("%d\n%d\n", info.Size(), info.ModTime().Unix())
	return os.WriteFile(metaPath(partPath), []byte(content), 0644)
}

// readMeta reads a sidecar meta and reports whether it matches the given remote
// info (size + mtime, second precision).
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
	return size == info.Size() && mtime == info.ModTime().Truncate(time.Second).Unix()
}

// downloadFile performs a resumable download of one entry. It resumes from an
// existing matching .part, retries transient mid-file errors with reconnect +
// resume, then on success verifies size, renames to final, sets mtime, and
// removes the meta sidecar.
func downloadFile(cfg Config, client *sftp.Client, file sftpEntry, ch chan<- uint64) error {
	localPath := file.LocalPath
	partPath := localPath + ".part"

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return err
	}

	// Decide whether we can resume: .part exists AND meta matches current remote.
	var startOffset int64
	if st, err := os.Stat(partPath); err == nil && metaMatches(partPath, file.Info) {
		startOffset = st.Size()
		if startOffset > file.Info.Size() {
			// Stale/oversized part — discard and start fresh.
			startOffset = 0
		}
	}
	if startOffset == 0 {
		// Fresh start: drop any stale part/meta and write a fresh meta.
		os.Remove(partPath)
		os.Remove(metaPath(partPath))
		if err := writeMeta(partPath, file.Info); err != nil {
			return err
		}
	}
	// Report already-present bytes so the overall bar reflects resumed progress.
	if startOffset > 0 {
		ch <- uint64(startOffset)
	}

	// Connections opened for retries are closed before opening the next one
	// (and the last one on return), so at most one extra connection is alive.
	var retrySSH *ssh.Client
	var retrySFTP *sftp.Client
	defer func() {
		if retrySFTP != nil {
			retrySFTP.Close()
		}
		if retrySSH != nil {
			retrySSH.Close()
		}
	}()

	var lastErr error
	for attempt := 0; attempt < maxDownloadRetries; attempt++ {
		if attempt > 0 {
			// Reconnect and re-resume from however much is on disk now.
			time.Sleep(downloadRetryBackof)
			if retrySFTP != nil {
				retrySFTP.Close()
				retrySFTP = nil
			}
			if retrySSH != nil {
				retrySSH.Close()
				retrySSH = nil
			}
			newSSH, newSFTP, derr := dial(cfg.Server)
			if derr != nil {
				lastErr = derr
				continue
			}
			retrySSH, retrySFTP = newSSH, newSFTP
			client = newSFTP
			if st, serr := os.Stat(partPath); serr == nil {
				startOffset = st.Size()
				if startOffset > file.Info.Size() {
					startOffset = 0
				}
			} else {
				startOffset = 0
			}
		}

		err := copyFromOffset(client, file, partPath, startOffset, ch)
		if err == nil {
			break
		}
		lastErr = err
		// On the next loop, we will reconnect and resume.
	}

	// Verify the part is complete; if the loop exhausted with an error, surface it.
	st, err := os.Stat(partPath)
	if err != nil {
		if lastErr != nil {
			return lastErr
		}
		return err
	}
	if st.Size() != file.Info.Size() {
		if lastErr != nil {
			return lastErr
		}
		return fmt.Errorf("неполная загрузка %s: %d/%d байт",
			filepath.Base(localPath), st.Size(), file.Info.Size())
	}

	// Complete: finalize.
	os.Remove(localPath) // ensure rename can overwrite on Windows
	if err := os.Rename(partPath, localPath); err != nil {
		return fmt.Errorf("rename %s: %w", filepath.Base(localPath), err)
	}
	os.Chtimes(localPath, time.Now(), file.Info.ModTime())
	os.Remove(metaPath(partPath))
	return nil
}

// copyFromOffset opens the remote file, seeks to offset, appends to the .part
// file from there, streaming progress. Returns an error on any read/write
// failure (treated as transient by the caller until retries are exhausted).
func copyFromOffset(client *sftp.Client, file sftpEntry, partPath string, offset int64, ch chan<- uint64) error {
	src, err := client.Open(file.RemotePath)
	if err != nil {
		return err
	}
	defer src.Close()

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
	dst, err := os.OpenFile(partPath, flag, 0644)
	if err != nil {
		return err
	}

	buf := make([]byte, downloadBufSize)
	for {
		n, rErr := src.Read(buf)
		if n > 0 {
			if _, wErr := dst.Write(buf[:n]); wErr != nil {
				dst.Close()
				return wErr
			}
			ch <- uint64(n)
		}
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			dst.Close()
			return rErr
		}
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return nil
}
