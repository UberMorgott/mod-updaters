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
		if r := []rune(fileName); len(r) > maxFileLen {
			fileName = string(r[:maxFileLen-3]) + "..." // cut by runes: names may be Cyrillic
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

// menuView — full-screen red notice after automatic retries are exhausted (or
// the listing was partial): the reason plus the launch / retry / exit keys.
func (m model) menuView() string {
	title, body := "⚠   ОБНОВЛЕНИЕ НЕ УДАЛОСЬ   ⚠", "Моды могут быть устаревшими."
	if m.syncDone {
		title, body = "⚠   ОБНОВЛЕНИЕ НЕПОЛНОЕ   ⚠", "Загруженные моды установлены."
	}
	banner := lipgloss.NewStyle().
		Foreground(lipgloss.Color("231")).
		Background(errorColor).
		Bold(true).
		Padding(1, 6).
		Render(title)

	subtitle := lipgloss.NewStyle().
		Foreground(errorColor).
		Bold(true).
		Align(lipgloss.Center).
		Render(m.menuReason)

	bodyText := lipgloss.NewStyle().
		Foreground(textColor).
		Align(lipgloss.Center).
		Render(body)

	detail := m.menuDetail
	if r := []rune(detail); len(r) > max(m.width-20, 20) {
		detail = string(r[:max(m.width-23, 17)]) + "..."
	}
	detailText := lipgloss.NewStyle().
		Foreground(statsColor).
		Italic(true).
		Render(detail)

	keys := lipgloss.NewStyle().
		Foreground(titleColor).
		Bold(true).
		Render("[Enter] Запустить игру   [R] Повторить   [Esc] Выход")

	content := lipgloss.JoinVertical(lipgloss.Center,
		banner, "", subtitle, "", bodyText, "", detailText, "", keys,
	)

	box := lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(errorColor).
		Padding(2, 6).
		Align(lipgloss.Center).
		Render(content)

	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}
