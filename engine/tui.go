package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// --- base styles (no sizes) ---
var (
	borderColor = lipgloss.Color("214")
	titleColor  = lipgloss.Color("214")
	textColor   = lipgloss.Color("248")
	errorColor  = lipgloss.Color("196")
	statsColor  = lipgloss.Color("245")
)

// progressView renders the normal updater screen (bordered box, title, status,
// current file, two progress bars, stats line).
func (m model) progressView() string {
	contentWidth := m.width - 4   // border + padding
	contentHeight := m.height - 4 // border + padding

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
	lines = append(lines, titleStyle.Render(fmt.Sprintf("━━━ Загрузчик модов %s ━━━", m.cfg.GameName)))
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

// failView — full-screen red notice: the update failed.
func (m model) failView() string {
	banner := lipgloss.NewStyle().
		Foreground(lipgloss.Color("231")).
		Background(errorColor).
		Bold(true).
		Padding(1, 6).
		Render("⚠   ОБНОВЛЕНИЕ НЕ УДАЛОСЬ   ⚠")

	subtitle := lipgloss.NewStyle().
		Foreground(errorColor).
		Bold(true).
		Align(lipgloss.Center).
		Render("Сервер обновлений не отвечает")

	body := lipgloss.NewStyle().
		Foreground(textColor).
		Align(lipgloss.Center).
		Render("Игра будет запущена\nБЕЗ обновления модов.")

	hint := lipgloss.NewStyle().
		Foreground(statsColor).
		Italic(true).
		Render("запуск через несколько секунд…")

	content := lipgloss.JoinVertical(lipgloss.Center,
		banner, "", subtitle, "", body, "", hint,
	)

	box := lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(errorColor).
		Padding(2, 6).
		Align(lipgloss.Center).
		Render(content)

	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}
