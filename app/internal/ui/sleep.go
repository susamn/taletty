package ui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	SleepModeDuration = "duration"
	SleepModeChapter  = "chapter"
	SleepModeOff      = "off"
)

type sleepOption struct {
	label    string
	mode     string
	duration time.Duration
}

var sleepOptions = []sleepOption{
	{label: "15 minutes", mode: SleepModeDuration, duration: 15 * time.Minute},
	{label: "30 minutes", mode: SleepModeDuration, duration: 30 * time.Minute},
	{label: "45 minutes", mode: SleepModeDuration, duration: 45 * time.Minute},
	{label: "60 minutes", mode: SleepModeDuration, duration: 60 * time.Minute},
	{label: "End of current chapter", mode: SleepModeChapter},
	{label: "Turn off", mode: SleepModeOff},
}

// SleepOpen reports whether the Sleep Timer menu overlay is currently open.
func (m Model) SleepOpen() bool { return m.sleepOpen }

func (m Model) chapterEndAvailable() bool {
	p := m.data.Player
	return p.transportAvailable() && strings.TrimSpace(cleanLine(p.Chapter)) != "" && p.ChapterEnd > 0
}

func (m *Model) openSleepMenu() {
	if !m.data.Player.transportAvailable() {
		m.data.Activity = "Sleep timer unavailable: no loaded audio"
		return
	}
	if m.dispatch == nil {
		m.data.Activity = "Sleep timer unavailable: no coordinator"
		return
	}
	m.sleepOpen = true
	m.sleepCursor = 0
}

func (m Model) sleepKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc":
		m.sleepOpen = false
		return m, nil
	case "j", "down", "tab":
		m.sleepCursor = (m.sleepCursor + 1) % len(sleepOptions)
		return m, nil
	case "k", "up", "shift+tab":
		m.sleepCursor = (m.sleepCursor + len(sleepOptions) - 1) % len(sleepOptions)
		return m, nil
	case "enter":
		p := m.data.Player
		if !p.transportAvailable() {
			m.sleepOpen = false
			m.data.Activity = "Sleep timer unavailable: no loaded audio"
			return m, nil
		}
		opt := sleepOptions[m.sleepCursor]
		if opt.mode == SleepModeChapter && !m.chapterEndAvailable() {
			m.data.Activity = "Chapter end unavailable: no chapter metadata"
			return m, nil
		}
		m.sleepOpen = false
		return m.request(Action{
			Kind:          SetSleepTimer,
			Key:           p.Key,
			Session:       p.Session,
			SleepMode:     opt.mode,
			SleepDuration: opt.duration,
		})
	default:
		return m, nil
	}
}

func (m Model) sleepLines(lines []string) []string {
	r := m.layout.Dialog(76, 13)
	innerWidth := max(1, r.Width-4)
	p := m.data.Player
	current := cleanLine(p.Sleep)
	if current == "" {
		current = "Off"
	}
	rows := []string{
		ClipLine("Target: "+title(p.Title)+" | Current: Sleep "+current, innerWidth, m.tail()),
		ClipLine("Cancels on book switch or quit; pause freezes duration.", innerWidth, m.tail()),
		"",
	}
	selectedRow := len(rows) + m.sleepCursor
	chapterOK := m.chapterEndAvailable()
	for _, opt := range sleepOptions {
		label := opt.label
		if opt.mode == SleepModeChapter && !chapterOK {
			label += " (unavailable: no chapter metadata)"
		}
		rows = append(rows, label)
	}
	capacity := max(1, r.Height-2)
	if len(rows) > capacity {
		start := max(0, selectedRow-capacity+1)
		if start+capacity > len(rows) {
			start = max(0, len(rows)-capacity)
		}
		rows = rows[start : start+capacity]
		selectedRow -= start
	}
	box := m.box(r, " [Focus] Sleep Timer ", rows, selectedRow, true, false)
	for y, row := range box {
		line := lines[r.Y+y]
		lines[r.Y+y] = ansi.Cut(line, 0, r.X) + row + ansi.Cut(line, r.X+r.Width, m.layout.Content.Width)
	}
	return lines
}
