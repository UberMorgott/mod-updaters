package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// --- Конфигурация ---
const (
	wurVersion     = "0.3.0"
	sftpServer     = "morgott.keenetic.pro:22"
	sftpLogin      = "modman"
	sftpPassword   = "Br2ctG7FGSqPhr4"
	remoteDir      = "/tmp/mnt/01DB6F2D5E1A6080/Windrose/"
	localDir       = "."
	gameExecutable = "Windrose.exe"
	tuiTitle       = "━━━ Загрузчик модов Windrose ━━━"
)

// fullMirrorDirs lists local directories (relative to the game root) where WUR
// applies full-mirror cleanup at the immediate-subdir level: any folder under
// these paths that has NO matching folder on SFTP at the same relative path is
// deleted recursively. Files at this exact level (e.g. mods.txt directly under
// `ue4ss/Mods/`) are NEVER deleted by this pass — only direct subdirectories.
//
// Outside of these two roots WUR is sync-only (file-level if-newer mirror,
// never delete). This keeps `R5/Binaries/Win64/Windrose-Win64-Shipping.exe`,
// the dwmapi/UE4SS dlls, and any random files the friend has under R5/ safe.
var fullMirrorDirs = []string{
	"R5/Binaries/Win64/ue4ss/Mods",
	"R5/Content/Paks/~mods/~mods",
}

// --- Базовые стили (без размеров) ---
var (
	borderColor = lipgloss.Color("214")
	titleColor  = lipgloss.Color("214")
	textColor   = lipgloss.Color("248")
	errorColor  = lipgloss.Color("196")
	statsColor  = lipgloss.Color("245")
)

// --- Типы ---

type sftpEntry struct {
	RemotePath string // absolute SFTP path
	LocalPath  string // absolute local path
	RelSlash   string // path relative to remoteDir / localDir, forward-slash form
	Info       os.FileInfo
}

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

// --- Модель ---
type model struct {
	sshClient                        *ssh.Client
	sftp                             *sftp.Client
	progressBar, fileProgressBar     progress.Model
	progressChan                     chan uint64
	status                           string
	files, allEntries                []sftpEntry
	totalSize, downloaded            uint64
	currentFileSize, currentFileDown uint64
	currentIdx                       int
	width, height                    int
	err                              error
	quitting                         bool
	startTime                        time.Time
}

func newModel() model {
	return model{
		progressBar:     progress.New(progress.WithDefaultGradient()),
		fileProgressBar: progress.New(progress.WithDefaultGradient()),
		status:          fmt.Sprintf("WUR v%s — Подключение к SFTP...", wurVersion),
		width:           80,
		height:          24,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(tea.EnterAltScreen, connectAndListFiles)
}

func tickCmd() tea.Cmd {
	return tea.Tick(50*time.Millisecond, func(time.Time) tea.Msg { return progressTickMsg{} })
}

func percent(current, total uint64) float64 {
	if total == 0 {
		return 0
	}
	p := float64(current) / float64(total)
	if p > 1 {
		return 1
	}
	return p
}

func (m *model) updateBarWidths() {
	barWidth := max(m.width-20, 20)
	m.progressBar.Width = barWidth
	m.fileProgressBar.Width = barWidth
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.updateBarWidths()
		return m, nil

	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC || msg.String() == "q" {
			m.quitting = true
			m.closeConnections()
			return m, tea.Quit
		}

	case errorMsg:
		m.err = msg.err
		m.quitting = true
		return m, tea.Quit

	case filesListedMsg:
		m.sftp = msg.sftpClient
		m.sshClient = msg.sshClient
		m.files = msg.files
		m.allEntries = msg.allEntries
		m.totalSize = msg.totalSize
		m.startTime = time.Now()
		m.updateBarWidths()
		if m.totalSize == 0 {
			m.status = fmt.Sprintf("WUR v%s — Все файлы актуальны. Запуск...", wurVersion)
			return m, tea.Sequence(m.syncAndLaunch(), tea.Quit)
		}
		m.status = fmt.Sprintf("WUR v%s — Найдено %d файлов (%.2f MB)", wurVersion, len(m.files), float64(m.totalSize)/1024/1024)
		if len(m.files) > 0 {
			m.currentFileSize = uint64(m.files[0].Info.Size())
		}
		return m, tea.Batch(m.downloadNext(), tickCmd())

	case progressTickMsg:
		if m.progressChan == nil {
			return m, tickCmd()
		}
		var got uint64
		for {
			select {
			case n, ok := <-m.progressChan:
				if !ok {
					m.progressChan = nil
					return m, tickCmd()
				}
				got += n
			default:
				if got > 0 {
					m.downloaded = min(m.downloaded+got, m.totalSize)
					m.currentFileDown = min(m.currentFileDown+got, m.currentFileSize)
				}
				return m, tickCmd()
			}
		}

	case fileDownloadedMsg:
		m.currentIdx++
		if m.currentIdx >= len(m.files) {
			m.status = fmt.Sprintf("WUR v%s — Загрузка завершена. Запуск...", wurVersion)
			return m, tea.Sequence(m.syncAndLaunch(), tea.Quit)
		}
		m.currentFileDown = 0
		m.currentFileSize = uint64(m.files[m.currentIdx].Info.Size())
		m.updateBarWidths()
		return m, m.downloadNext()
	}
	return m, nil
}

func (m model) View() string {
	if m.quitting {
		return ""
	}

	contentWidth := m.width - 4
	contentHeight := m.height - 4

	appStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Width(contentWidth).
		Height(contentHeight).
		Padding(1, 2)

	centerStyle := lipgloss.NewStyle().
		Width(contentWidth - 4).
		Align(lipgloss.Center)

	if m.err != nil {
		errStyle := centerStyle.Foreground(errorColor).Bold(true)
		return appStyle.Render(errStyle.Render(fmt.Sprintf("Ошибка: %v", m.err)))
	}

	var lines []string

	titleStyle := centerStyle.Foreground(titleColor).Bold(true)
	lines = append(lines, titleStyle.Render(tuiTitle))
	lines = append(lines, "")

	statusStyled := centerStyle.Foreground(textColor)
	lines = append(lines, statusStyled.Render(m.status))
	lines = append(lines, "")

	fileName := "—"
	maxFileLen := max(contentWidth-15, 20)
	if m.currentIdx < len(m.files) {
		fileName = filepath.Base(m.files[m.currentIdx].RemotePath)
		if len(fileName) > maxFileLen {
			fileName = fileName[:maxFileLen-3] + "..."
		}
	}
	lines = append(lines, statusStyled.Render("Файл: "+fileName))
	lines = append(lines, "")

	barLine1 := fmt.Sprintf("Общий:   %s", m.progressBar.ViewAs(percent(m.downloaded, m.totalSize)))
	barLine2 := fmt.Sprintf("Текущий: %s", m.fileProgressBar.ViewAs(percent(m.currentFileDown, m.currentFileSize)))
	lines = append(lines, centerStyle.Render(barLine1))
	lines = append(lines, "")
	lines = append(lines, centerStyle.Render(barLine2))
	lines = append(lines, "")

	elapsed := time.Since(m.startTime).Seconds()
	speed := 0.0
	if elapsed > 0 {
		speed = float64(m.downloaded) / 1024 / 1024 / elapsed
	}
	done := m.currentIdx
	if m.downloaded >= m.totalSize {
		done = len(m.files)
	}

	stats := fmt.Sprintf("Файлы: %d/%d  │  Загружено: %.1f/%.1f MB  │  Скорость: %.2f MB/s",
		done, len(m.files),
		float64(m.downloaded)/1024/1024, float64(m.totalSize)/1024/1024,
		speed)
	statsStyled := centerStyle.Foreground(statsColor)
	lines = append(lines, statsStyled.Render(stats))

	return appStyle.Render(strings.Join(lines, "\n"))
}

func (m *model) closeConnections() {
	if m.sftp != nil {
		m.sftp.Close()
	}
	if m.sshClient != nil {
		m.sshClient.Close()
	}
}

// --- Команды ---

func needsUpdate(local, remote os.FileInfo) bool {
	return local.Size() != remote.Size() ||
		!local.ModTime().Truncate(time.Second).Equal(remote.ModTime().Truncate(time.Second))
}

// connectAndListFiles walks the SFTP root recursively, mirroring `Z:\Windrose\`
// to the local game directory 1:1. Every file under the SFTP root maps to the
// same relative path locally.
func connectAndListFiles() tea.Msg {
	sshCfg := &ssh.ClientConfig{
		User:            sftpLogin,
		Auth:            []ssh.AuthMethod{ssh.Password(sftpPassword)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	sshClient, err := ssh.Dial("tcp", sftpServer, sshCfg)
	if err != nil {
		return errorMsg{fmt.Errorf("SSH: %w", err)}
	}

	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		sshClient.Close()
		return errorMsg{fmt.Errorf("SFTP: %w", err)}
	}

	localRootAbs, _ := filepath.Abs(localDir)
	remoteRoot := strings.TrimRight(remoteDir, "/")

	var all, toDownload []sftpEntry
	var totalSize uint64

	walker := sftpClient.Walk(remoteRoot)
	for walker.Step() {
		if walker.Err() != nil {
			continue
		}
		rPath := walker.Path()
		info := walker.Stat()

		rel := strings.TrimPrefix(rPath, remoteRoot)
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
	}
}

func (m *model) downloadNext() tea.Cmd {
	if m.currentIdx >= len(m.files) {
		return nil
	}

	file := m.files[m.currentIdx]
	client := m.sftp
	ch := make(chan uint64, 1000)
	m.progressChan = ch

	return func() tea.Msg {
		defer close(ch)

		if client == nil {
			return errorMsg{fmt.Errorf("нет SFTP соединения")}
		}

		localPath := file.LocalPath

		if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
			return errorMsg{err}
		}

		src, err := client.Open(file.RemotePath)
		if err != nil {
			return errorMsg{err}
		}
		defer src.Close()

		tmpPath := localPath + ".tmp"
		dst, err := os.Create(tmpPath)
		if err != nil {
			return errorMsg{err}
		}

		buf := make([]byte, 128*1024)
		for {
			n, rErr := src.Read(buf)
			if n > 0 {
				if _, wErr := dst.Write(buf[:n]); wErr != nil {
					dst.Close()
					os.Remove(tmpPath)
					return errorMsg{wErr}
				}
				ch <- uint64(n)
			}
			if rErr == io.EOF {
				break
			}
			if rErr != nil {
				dst.Close()
				os.Remove(tmpPath)
				return errorMsg{rErr}
			}
		}

		if err := dst.Close(); err != nil {
			os.Remove(tmpPath)
			return errorMsg{err}
		}
		if err := os.Rename(tmpPath, localPath); err != nil {
			os.Remove(tmpPath)
			return errorMsg{fmt.Errorf("rename %s: %w", filepath.Base(localPath), err)}
		}
		os.Chtimes(localPath, time.Now(), file.Info.ModTime())
		return fileDownloadedMsg{}
	}
}

func (m *model) syncAndLaunch() tea.Cmd {
	return func() tea.Msg {
		defer m.closeConnections()

		// Create directories that exist on SFTP (so empty dirs survive a sync).
		for _, e := range m.allEntries {
			if e.Info.IsDir() {
				os.MkdirAll(e.LocalPath, 0755)
			}
		}

		// Cleanup pass: for each fullMirrorDirs path, list immediate subdirs of
		// the local copy. Any subdir whose corresponding SFTP path doesn't exist
		// gets recursively deleted (propagates user's "I removed this mod").
		// Files at this exact level are NEVER touched (mods.txt etc.).
		localRootAbs, _ := filepath.Abs(localDir)
		sftpDirs := buildSFTPDirSet(m.allEntries)
		for _, mirror := range fullMirrorDirs {
			cleanupFullMirror(localRootAbs, mirror, sftpDirs)
		}

		// Запуск игры
		cmd := exec.Command("cmd", "/C", "start", "/B", "/high", gameExecutable, "-console")
		cmd.Dir = localDir
		cmd.Start()
		return nil
	}
}

// buildSFTPDirSet returns the set of forward-slash relative paths that are
// directories on SFTP this run. Used to decide which local subdirs survive
// the full-mirror cleanup.
func buildSFTPDirSet(entries []sftpEntry) map[string]struct{} {
	set := make(map[string]struct{})
	for _, e := range entries {
		if e.Info.IsDir() {
			set[e.RelSlash] = struct{}{}
		}
	}
	return set
}

// cleanupFullMirror deletes any immediate subdirectory of `<localRoot>/<mirrorRel>`
// that is NOT present as a directory on SFTP at the same relative path. Files
// directly inside `<localRoot>/<mirrorRel>` (mods.txt, mods.json, etc.) are
// NEVER touched. Recursion stops at the immediate subdir level — once we
// decide a subdir survives, its contents are not pruned (file-level updates
// already happened during the download phase).
func cleanupFullMirror(localRoot, mirrorRel string, sftpDirs map[string]struct{}) {
	mirrorLocal := filepath.Join(localRoot, filepath.FromSlash(mirrorRel))
	st, err := os.Stat(mirrorLocal)
	if err != nil || !st.IsDir() {
		return
	}
	entries, err := os.ReadDir(mirrorLocal)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue // never delete files at this level
		}
		relSlash := mirrorRel + "/" + e.Name()
		if _, ok := sftpDirs[relSlash]; ok {
			continue // matching dir on SFTP — keep
		}
		os.RemoveAll(filepath.Join(mirrorLocal, e.Name()))
	}
}

func sanityCheck() error {
	for _, d := range fullMirrorDirs {
		if d == "" || filepath.IsAbs(d) || strings.Contains(d, "..") {
			return fmt.Errorf("unsafe fullMirrorDirs entry: %q", d)
		}
	}
	if _, err := os.Stat(gameExecutable); err != nil {
		return fmt.Errorf("%s не найден рядом с WUR.exe — запусти из корня папки Windrose", gameExecutable)
	}
	return nil
}

func main() {
	if err := sanityCheck(); err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка:", err)
		os.Exit(1)
	}
	if _, err := tea.NewProgram(newModel()).Run(); err != nil {
		log.Fatal(err)
	}
}
