package engine

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// model is the bubbletea model driving the updater TUI.
type model struct {
	cfg                              Config
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
	failed                           bool // server unreachable → show the fail screen
	startTime                        time.Time
}

func newModel(cfg Config) model {
	status := "Подключение к SFTP..."
	if cfg.Version != "" {
		status = fmt.Sprintf("v%s — Подключение к SFTP...", cfg.Version)
	}
	return model{
		cfg:             cfg,
		progressBar:     progress.New(progress.WithDefaultGradient()),
		fileProgressBar: progress.New(progress.WithDefaultGradient()),
		status:          status,
		width:           80,
		height:          24,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(tea.EnterAltScreen, connectCmd(m.cfg, 1))
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

// connectCmd tries to connect. On failure it returns retryMsg (if attempts
// remain) or connectFailedMsg (if exhausted).
func connectCmd(cfg Config, attempt int) tea.Cmd {
	return func() tea.Msg {
		msg, err := doConnect(cfg)
		if err != nil {
			if attempt < maxConnectAttempts {
				return retryMsg{attempt: attempt + 1, err: err}
			}
			return connectFailedMsg{err: err}
		}
		return msg
	}
}

// launchGameCmd wraps launchGame as a tea.Cmd.
func (m *model) launchGameCmd() tea.Cmd {
	return func() tea.Msg {
		launchGame(m.cfg)
		return nil
	}
}

// delayCmd just waits d (so the user can read the message).
func delayCmd(d time.Duration) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(d)
		return nil
	}
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
		// Update failed mid-way (download / transient error after retries).
		// Priority: let the user into the game. Show the fail screen, then
		// launch WITHOUT cleanup (partial download must not trigger mirror).
		m.err = nil
		m.failed = true
		m.closeConnections()
		return m, tea.Sequence(delayCmd(failNoticeDelay), m.launchGameCmd(), tea.Quit)

	case retryMsg:
		m.status = fmt.Sprintf("Сервер обновлений не отвечает, попытка %d/%d...", msg.attempt, maxConnectAttempts)
		a := msg.attempt
		return m, tea.Tick(retryDelay, func(time.Time) tea.Msg { return startConnectMsg{a} })

	case startConnectMsg:
		m.status = fmt.Sprintf("Подключение к серверу (попытка %d/%d)...", msg.attempt, maxConnectAttempts)
		return m, connectCmd(m.cfg, msg.attempt)

	case connectFailedMsg:
		m.err = nil
		m.failed = true
		return m, tea.Sequence(delayCmd(failNoticeDelay), m.launchGameCmd(), tea.Quit)

	case filesListedMsg:
		m.sftp = msg.sftpClient
		m.sshClient = msg.sshClient
		m.files = msg.files
		m.allEntries = msg.allEntries
		m.totalSize = msg.totalSize
		m.startTime = time.Now()
		m.updateBarWidths()
		if m.totalSize == 0 {
			m.status = "Все файлы актуальны. Запуск..."
			return m, tea.Sequence(m.syncAndLaunch(), tea.Quit)
		}
		m.status = fmt.Sprintf("Найдено %d файлов (%.2f MB)", len(m.files), float64(m.totalSize)/1024/1024)
		if len(m.files) > 0 {
			m.currentFileSize = fileSize(m.files[0].Info)
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
			m.status = "Загрузка завершена. Запуск..."
			return m, tea.Sequence(m.syncAndLaunch(), tea.Quit)
		}
		m.currentFileDown = 0
		m.currentFileSize = fileSize(m.files[m.currentIdx].Info)
		m.updateBarWidths()
		return m, m.downloadNext()
	}
	return m, nil
}

func (m model) View() string {
	if m.quitting {
		return ""
	}
	if m.failed {
		return m.failView()
	}
	return m.progressView()
}

func (m *model) closeConnections() {
	if m.sftp != nil {
		_ = m.sftp.Close()
	}
	if m.sshClient != nil {
		_ = m.sshClient.Close()
	}
}

// Run is the engine entry point: validate config, then drive the TUI.
func Run(cfg Config) {
	if err := SanityCheck(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка:", err)
		os.Exit(1)
	}
	if _, err := tea.NewProgram(newModel(cfg)).Run(); err != nil {
		log.Fatal(err)
	}
}
