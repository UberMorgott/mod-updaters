package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
)

// model is the bubbletea model driving the updater TUI.
type model struct {
	cfg                              Config
	conn                             *conn
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
	listingComplete                  bool  // walk finished without errors → cleanup allowed
	walkErr                          error // listing partly failed → warn instead of silent success
	startTime                        time.Time

	// The menu replaces the progress screen once automatic retries are
	// exhausted (or the listing was partial): the player picks launch / retry /
	// exit — the updater never stands between the player and the game.
	menu       bool
	menuReason string // short Russian reason
	menuDetail string // raw error text, for bug reports
	syncDone   bool   // all downloads finished: launching may create dirs (cleanup still honors listingComplete)
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
func launchGameCmd(cfg Config) tea.Cmd {
	return func() tea.Msg {
		launchGame(cfg)
		return nil
	}
}

// closeCmd closes c off the Update loop: a close on a dead link must never
// freeze the UI.
func closeCmd(c *conn) tea.Cmd {
	if c == nil {
		return nil
	}
	return func() tea.Msg {
		c.Close()
		return nil
	}
}

// failReason maps an error to a short Russian reason for the menu.
func failReason(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, fs.ErrPermission):
		return "Нет доступа к файлу (игра запущена?)"
	case errors.Is(err, fs.ErrNotExist):
		return "Файл пропал с сервера во время загрузки"
	case errors.As(err, &ne):
		return "Соединение с сервером потеряно"
	default:
		return "Загрузка модов прервана"
	}
}

// showMenu switches to the launch/retry/exit menu and drops the connection.
func (m *model) showMenu(reason string, err error) tea.Cmd {
	m.menu = true
	m.menuReason = reason
	m.menuDetail = ""
	if err != nil {
		m.menuDetail = err.Error()
	}
	c := m.conn
	m.conn = nil
	return closeCmd(c)
}

// finishSync runs after the last download (or when nothing needed one): launch
// on a clean listing, otherwise warn via the menu instead of claiming success.
func (m *model) finishSync(status string) tea.Cmd {
	m.syncDone = true
	if m.walkErr != nil {
		return m.showMenu("Список модов получен не полностью — очистка пропущена", m.walkErr)
	}
	m.status = status
	return tea.Sequence(m.syncAndLaunch(), tea.Quit)
}

// menuKey handles a key press while the menu is shown.
func (m model) menuKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.quitting = true
		if m.syncDone {
			// Everything is downloaded: dirs may be created; cleanup still
			// skipped because the listing was incomplete.
			return m, tea.Sequence(m.syncAndLaunch(), tea.Quit)
		}
		// Partial sync: launch as-is, never run cleanup.
		return m, tea.Sequence(launchGameCmd(m.cfg), tea.Quit)
	case "r", "R", "к", "К": // "к" = R on the Russian layout
		// Reconnect and continue: finished files are up to date now and are
		// skipped by the new listing, unfinished ones resume from their .part.
		n := newModel(m.cfg)
		n.width, n.height = m.width, m.height
		n.updateBarWidths()
		n.status = "Повторное подключение..."
		return n, connectCmd(n.cfg, 1)
	case "esc", "ctrl+c", "q":
		m.quitting = true
		return m, tea.Quit
	}
	return m, nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.updateBarWidths()
		return m, nil

	case tea.KeyMsg:
		if m.menu {
			return m.menuKey(msg)
		}
		if msg.Type == tea.KeyCtrlC || msg.String() == "q" {
			m.quitting = true
			c := m.conn
			m.conn = nil
			return m, tea.Batch(closeCmd(c), tea.Quit)
		}

	case errorMsg:
		// Update failed mid-way, automatic retries exhausted (or a permanent
		// error). Let the player choose; a partial sync never runs cleanup.
		m.drainProgress()
		return m, m.showMenu(failReason(msg.err), msg.err)

	case retryMsg:
		m.status = fmt.Sprintf("Сервер обновлений не отвечает, попытка %d/%d...", msg.attempt, maxConnectAttempts)
		a := msg.attempt
		return m, tea.Tick(retryDelay(a-1), func(time.Time) tea.Msg { return startConnectMsg{a} })

	case startConnectMsg:
		m.status = fmt.Sprintf("Подключение к серверу (попытка %d/%d)...", msg.attempt, maxConnectAttempts)
		return m, connectCmd(m.cfg, msg.attempt)

	case connectFailedMsg:
		return m, m.showMenu("Сервер обновлений не отвечает", msg.err)

	case filesListedMsg:
		m.conn = msg.conn
		m.listingComplete = msg.complete
		m.walkErr = msg.walkErr
		m.files = msg.files
		m.allEntries = msg.allEntries
		m.totalSize = msg.totalSize
		m.startTime = time.Now()
		m.updateBarWidths()
		if len(m.files) == 0 {
			return m, m.finishSync("Все файлы актуальны. Запуск...")
		}
		m.status = fmt.Sprintf("Найдено %d файлов (%.2f MB)", len(m.files), float64(m.totalSize)/1024/1024)
		m.currentFileSize = fileSize(m.files[0].Info)
		return m, tea.Batch(m.downloadNext(), tickCmd())

	case progressTickMsg:
		if m.menu {
			return m, nil // download over: stop the tick loop
		}
		m.pollProgress()
		return m, tickCmd()

	case fileDownloadedMsg:
		m.drainProgress()
		var closeOld tea.Cmd
		if msg.conn != nil {
			// The download reconnected: the old connection is dead, adopt the new one.
			closeOld = closeCmd(m.conn)
			m.conn = msg.conn
		}
		m.currentIdx++
		if m.currentIdx >= len(m.files) {
			return m, tea.Batch(closeOld, m.finishSync("Загрузка завершена. Запуск..."))
		}
		m.currentFileDown = 0
		m.currentFileSize = fileSize(m.files[m.currentIdx].Info)
		m.updateBarWidths()
		return m, tea.Batch(closeOld, m.downloadNext())
	}
	return m, nil
}

func (m model) View() string {
	if m.quitting {
		return ""
	}
	if m.menu {
		return m.menuView()
	}
	return m.progressView()
}

// addProgress accounts n freshly downloaded bytes on both bars.
func (m *model) addProgress(n uint64) {
	m.downloaded = min(m.downloaded+n, m.totalSize)
	m.currentFileDown = min(m.currentFileDown+n, m.currentFileSize)
}

// pollProgress consumes whatever the running download has reported so far
// without blocking. A closed channel (download finished) is dropped.
func (m *model) pollProgress() {
	for m.progressChan != nil {
		select {
		case n, ok := <-m.progressChan:
			if !ok {
				m.progressChan = nil
				return
			}
			m.addProgress(n)
		default:
			return
		}
	}
}

// drainProgress counts the bytes still buffered in the finished download's
// progress channel (closed by then), so no progress is lost between ticks.
func (m *model) drainProgress() {
	if m.progressChan == nil {
		return
	}
	for n := range m.progressChan {
		m.addProgress(n)
	}
	m.progressChan = nil
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
