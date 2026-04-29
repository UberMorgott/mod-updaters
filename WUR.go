package main

import (
	"bufio"
	"bytes"
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
	wurVersion     = "0.2.0"
	sftpServer     = "morgott.keenetic.pro:22"
	sftpLogin      = "modman"
	sftpPassword   = "Br2ctG7FGSqPhr4"
	remoteDir      = "/tmp/mnt/01DB6F2D5E1A6080/Windrose/"
	localDir       = "."
	gameExecutable = "Windrose.exe"
	tuiTitle       = "━━━ Загрузчик модов Windrose ━━━"

	// mods.txt merge — special file at SFTP `ue4ss-mods/mods-additions.txt`,
	// listing UE4SS mod-name lines (`<Name> : <0|1>`) WUR ensures registered
	// in friend's local `R5/Binaries/Win64/ue4ss/Mods/mods.txt`.
	modsAdditionsRemoteName = "mods-additions.txt"
	modsTxtLocalSubpath     = "R5/Binaries/Win64/ue4ss/Mods/mods.txt"
)

// managedMods enumerates the mod folder names WUR is allowed to touch.
//
// Policy:
//   - Folders in this list are FULL-MIRRORED from SFTP. If the folder
//     disappears from `Z:\Windrose\paks\~mods\<X>` or `Z:\Windrose\ue4ss-mods\<X>`,
//     it is DELETED from friend's local install on the next launch (propagating
//     deletes — user's intent).
//   - Folders NOT in this list are NEVER touched, regardless of SFTP state.
//     This protects friend's other mods (Caites Map Tweaks, ConsoleEnabler,
//     Keybinds, etc.) from being clobbered.
//   - mods.txt entries: if a name in this list is in friend's mods.txt but no
//     longer on SFTP, the entry is removed. Names NOT in this list are
//     preserved verbatim.
//
// To start managing a new mod, append its folder name here, rebuild, push.
// To stop managing a mod (leave it alone forever), remove from this list.
var managedMods = []string{
	"ShareMap",
	"ShareMap-CPP",
	"ShareShip",
}

// subtree maps an SFTP source subpath to a local target subpath.
//
// Each subtree's top-level folders that match a name in `managedMods` are
// owned by WUR. Inside an owned folder, full-mirror semantics apply (any file
// not on SFTP is deleted). Folders not in `managedMods` are left alone.
var subtrees = []struct {
	Name                    string
	RemoteSubpath           string // relative to remoteDir, no trailing slash
	LocalSubpath            string // relative to localDir, no trailing slash
	HandleModsAdditionsFile bool   // if true, file `mods-additions.txt` at the subpath root drives the mods.txt merge
}{
	{
		Name:          "paks",
		RemoteSubpath: "paks/~mods",
		LocalSubpath:  "R5/Content/Paks/~mods/~mods",
	},
	{
		Name:                    "ue4ss-mods",
		RemoteSubpath:           "ue4ss-mods",
		LocalSubpath:            "R5/Binaries/Win64/ue4ss/Mods",
		HandleModsAdditionsFile: true,
	},
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
	RemotePath string
	LocalPath  string
	Info       os.FileInfo
}

// subtreePresence records, per subtree, which managed mod-name folders exist on SFTP this run.
type subtreePresence struct {
	LocalSubpath string              // absolute path (resolved at sync time)
	OnSFTP       map[string]struct{} // managed mod names that are present on SFTP for this subtree
}

type errorMsg struct{ err error }
type filesListedMsg struct {
	sftpClient   *sftp.Client
	sshClient    *ssh.Client
	files        []sftpEntry
	allEntries   []sftpEntry
	presence     []subtreePresence
	modsAddLines []string
	totalSize    uint64
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
	presence                         []subtreePresence
	modsAddLines                     []string
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
		m.presence = msg.presence
		m.modsAddLines = msg.modsAddLines
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

func remoteDirExists(c *sftp.Client, path string) bool {
	st, err := c.Stat(path)
	if err != nil {
		return false
	}
	return st.IsDir()
}

func isManagedMod(name string) bool {
	for _, m := range managedMods {
		if strings.EqualFold(m, name) {
			return true
		}
	}
	return false
}

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

	var all, toDownload []sftpEntry
	var presence []subtreePresence
	var modsAddLines []string
	var totalSize uint64

	for _, st := range subtrees {
		remoteRoot := strings.TrimRight(remoteDir, "/") + "/" + st.RemoteSubpath
		localRootAbs, _ := filepath.Abs(filepath.Join(localDir, filepath.FromSlash(st.LocalSubpath)))

		pres := subtreePresence{LocalSubpath: localRootAbs, OnSFTP: make(map[string]struct{})}

		if !remoteDirExists(sftpClient, remoteRoot) {
			presence = append(presence, pres) // empty OnSFTP → all managed mods absent → all get cleaned
			continue
		}

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

			// Special file at the subtree root: drives mods.txt merge.
			if st.HandleModsAdditionsFile && !info.IsDir() && rel == modsAdditionsRemoteName {
				lines, ferr := readRemoteTextFile(sftpClient, rPath)
				if ferr == nil {
					modsAddLines = lines
				}
				continue
			}

			// Top-level segment under the subtree root = mod-name directory.
			topSeg := rel
			if i := strings.IndexByte(rel, '/'); i >= 0 {
				topSeg = rel[:i]
			}
			if topSeg == "" {
				continue
			}
			// Only sync managed mods. Anything else on SFTP is ignored — keeps the
			// `managedMods` list as the single source of truth on the friend side.
			if !isManagedMod(topSeg) {
				continue
			}
			pres.OnSFTP[topSeg] = struct{}{}

			localPath := filepath.Join(localRootAbs, filepath.FromSlash(rel))
			entry := sftpEntry{RemotePath: rPath, LocalPath: localPath, Info: info}
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

		presence = append(presence, pres)
	}

	return filesListedMsg{
		sftpClient:   sftpClient,
		sshClient:    sshClient,
		files:        toDownload,
		allEntries:   all,
		presence:     presence,
		modsAddLines: modsAddLines,
		totalSize:    totalSize,
	}
}

func readRemoteTextFile(c *sftp.Client, path string) ([]string, error) {
	f, err := c.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	var out []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r\n")
		out = append(out, line)
	}
	return out, scanner.Err()
}

func needsUpdate(local, remote os.FileInfo) bool {
	return local.Size() != remote.Size() ||
		!local.ModTime().Truncate(time.Second).Equal(remote.ModTime().Truncate(time.Second))
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

		// Create directories under managed roots that exist on SFTP.
		for _, e := range m.allEntries {
			if e.Info.IsDir() {
				os.MkdirAll(e.LocalPath, 0755)
			}
		}

		// Per-managed-mod cleanup (managed-list pattern).
		//
		// For each subtree:
		//   1. For every directory entry directly under the local subtree root
		//      whose name is in `managedMods`:
		//        - If that mod is present on SFTP → full-mirror inside the
		//          folder (delete files not on SFTP).
		//        - If absent from SFTP → DELETE the whole folder (propagating
		//          deletes — user removed the mod from the source).
		//   2. Folders not in `managedMods` are NEVER touched.
		keep := make(map[string]struct{})
		for _, e := range m.allEntries {
			abs, err := filepath.Abs(e.LocalPath)
			if err != nil {
				continue
			}
			keep[filepath.Clean(abs)] = struct{}{}
		}

		for _, pres := range m.presence {
			cleanSubtreeManaged(pres.LocalSubpath, pres.OnSFTP, keep)
		}

		// mods.txt managed merge: ensure entries for every managed mod present
		// on SFTP (the ue4ss-mods subtree); remove entries for managed mods
		// absent from SFTP; preserve all unmanaged entries verbatim.
		var ue4ssPresent map[string]struct{}
		for _, p := range m.presence {
			if strings.HasSuffix(filepath.ToSlash(p.LocalSubpath), "Win64/ue4ss/Mods") {
				ue4ssPresent = p.OnSFTP
				break
			}
		}
		mergeModsTxt(m.modsAddLines, ue4ssPresent)

		// Запуск игры
		cmd := exec.Command("cmd", "/C", "start", "/B", "/high", gameExecutable, "-console")
		cmd.Dir = localDir
		cmd.Start()
		return nil
	}
}

// cleanSubtreeManaged enforces the managed-list policy on a subtree root.
//
// `localSubtree` is the absolute path of e.g. R5/Content/Paks/~mods/~mods.
// `onSFTP` is the set of managed mod names present on SFTP for this subtree.
// `keep` is the global set of absolute paths that must survive cleanup.
func cleanSubtreeManaged(localSubtree string, onSFTP map[string]struct{}, keep map[string]struct{}) {
	entries, err := os.ReadDir(localSubtree)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !isManagedMod(name) {
			continue // friend's other mods — leave alone
		}
		full := filepath.Join(localSubtree, name)
		if _, present := onSFTP[name]; !present {
			// Managed mod absent from SFTP → propagate delete.
			os.RemoveAll(full)
			continue
		}
		// Managed mod present on SFTP → full-mirror inside the folder.
		fullMirrorWithin(full, keep)
	}
}

// fullMirrorWithin walks `root` (absolute) and deletes any path not in `keep`.
// Used inside a managed mod folder where everything is owned by WUR.
func fullMirrorWithin(root string, keep map[string]struct{}) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		full := filepath.Join(root, e.Name())
		abs, err := filepath.Abs(full)
		if err != nil {
			continue
		}
		clean := filepath.Clean(abs)
		if _, ok := keep[clean]; !ok {
			os.RemoveAll(full)
			continue
		}
		if e.IsDir() {
			fullMirrorWithin(full, keep)
		}
	}
}

// mergeModsTxt enforces the managed-list policy on friend's mods.txt.
//
//   - For every entry whose mod name is in `managedMods`:
//   - present on SFTP → keep entry (with whatever flag user already has)
//   - absent from SFTP → REMOVE entry (cleanup propagates)
//   - For every entry whose mod name is NOT in `managedMods` → preserve verbatim.
//   - For every managed mod on SFTP not yet listed → APPEND `<Name> : 1`.
//
// `additions` is the raw line list from `mods-additions.txt` and is treated as
// a hint for the desired flag (`: 0` vs `: 1`) of new entries; if no hint,
// default to `: 1`.
func mergeModsTxt(additions []string, ue4ssOnSFTP map[string]struct{}) {
	target := filepath.Join(localDir, filepath.FromSlash(modsTxtLocalSubpath))
	existing, err := os.ReadFile(target)
	if err != nil {
		return // mods.txt not present — friend hasn't installed UE4SS yet
	}

	// Index hints from mods-additions.txt: managed mod name -> full line text.
	hints := make(map[string]string)
	for _, line := range additions {
		if name := parseModName(line); name != "" && isManagedMod(name) {
			hints[strings.ToLower(name)] = line
		}
	}

	// Walk existing lines, drop managed mods missing from SFTP, keep everything else.
	var out []string
	have := make(map[string]struct{})
	scanner := bufio.NewScanner(bytes.NewReader(existing))
	for scanner.Scan() {
		line := scanner.Text()
		name := parseModName(line)
		if name != "" && isManagedMod(name) {
			if _, present := ue4ssOnSFTP[name]; !present {
				continue // drop — managed mod removed from SFTP
			}
			have[strings.ToLower(name)] = struct{}{}
		}
		out = append(out, line)
	}

	// Append any managed mod present on SFTP but not yet in mods.txt.
	var toAppend []string
	for name := range ue4ssOnSFTP {
		if !isManagedMod(name) {
			continue
		}
		if _, ok := have[strings.ToLower(name)]; ok {
			continue
		}
		if hint, hasHint := hints[strings.ToLower(name)]; hasHint {
			toAppend = append(toAppend, hint)
		} else {
			toAppend = append(toAppend, name+" : 1")
		}
		have[strings.ToLower(name)] = struct{}{}
	}

	// Compose output. Insertion strategy: if a "; Built-in keybinds" sentinel
	// comment exists, insert appendees before it (preserves Keybinds-last
	// convention); else append at end.
	body := strings.Join(out, "\r\n")
	if !strings.HasSuffix(body, "\r\n") && len(body) > 0 {
		body += "\r\n"
	}
	if len(toAppend) > 0 {
		insertion := strings.Join(toAppend, "\r\n") + "\r\n"
		sentinel := "; Built-in keybinds"
		if idx := strings.Index(body, sentinel); idx >= 0 {
			lineStart := strings.LastIndex(body[:idx], "\n")
			if lineStart < 0 {
				lineStart = 0
			} else {
				lineStart++
			}
			prefix := body[:lineStart]
			if !strings.HasSuffix(prefix, "\r\n\r\n") && !strings.HasSuffix(prefix, "\n\n") && !strings.HasSuffix(prefix, "\r\n") {
				prefix += "\r\n"
			}
			body = prefix + insertion + body[lineStart:]
		} else {
			body += insertion
		}
	}

	if bytes.Equal([]byte(body), existing) {
		return // nothing to do
	}

	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0644); err != nil {
		return
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
	}
}

// parseModName extracts the mod name from a `<Name> : <0|1>` line.
// Returns "" for blank lines, comments, or malformed entries.
func parseModName(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
		return ""
	}
	colon := strings.IndexByte(line, ':')
	if colon < 0 {
		return ""
	}
	name := strings.TrimSpace(line[:colon])
	return name
}

func sanityCheck() error {
	for _, st := range subtrees {
		if st.LocalSubpath == "" || st.LocalSubpath == "." || filepath.IsAbs(st.LocalSubpath) || strings.Contains(st.LocalSubpath, "..") {
			return fmt.Errorf("unsafe subtree LocalSubpath: %q", st.LocalSubpath)
		}
		if st.RemoteSubpath == "" || strings.Contains(st.RemoteSubpath, "..") {
			return fmt.Errorf("unsafe subtree RemoteSubpath: %q", st.RemoteSubpath)
		}
	}
	if len(managedMods) == 0 {
		return fmt.Errorf("managedMods is empty — refusing to run")
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
