package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Shared with root input: Help permits transport only, never bookmark capture,
// quit, focus or context shortcuts. No consumed key is dispatched a second time.
var transportKeys = map[string]ActionKind{
	"space": TogglePlayback, ",": SeekBackward, ".": SeekForward,
	"-": VolumeDown, "=": VolumeUp, "[": SpeedDown, "]": SpeedUp,
}

func (m Model) helpTransport() string {
	if m.dispatch == nil {
		return "Transport disabled: no coordinator"
	}
	if !m.data.Player.transportAvailable() {
		return "Transport disabled: no loaded audio"
	}
	return "Space current audio"
}

func (m Model) helpRows() []string {
	enter := "Enter: unavailable in this shell for " + paneNames[m.focus]
	switch m.focus {
	case Books, Details:
		enter = "Enter: request play highlighted book"
		if m.selected == "" {
			enter = "Enter disabled: no highlighted book"
		}
	case PlayerPane:
		enter = "Enter: activate selected player control"
		if !m.data.Player.transportAvailable() {
			enter = "Enter disabled: no loaded audio"
		}
	}
	if m.dispatch == nil {
		enter = "Enter disabled: no coordinator"
	}
	audio := "No current audio"
	if m.data.Player.Key != "" {
		audio = title(m.data.Player.Title) + " - " + playbackLabel(m.data.Player.State)
	}
	rows := []string{
		"HELP - Esc returns to " + paneNames[m.focus],
		"Origin keys below apply only after closing Help, not while reading it.",
		"HELP NAVIGATION",
		"j/k or Up/Down: scroll; g/G or Home/End: bounds",
		"PageUp/PageDown: page; Ctrl+u/d: half page",
		"Tab/Shift+Tab stay in Help. Numbers, q/Ctrl+C, b/B and context keys are blocked.",
		"CURRENT AUDIO: " + audio,
		m.helpTransport(),
		"Space: pause/resume; comma/period: seek -15s/+15s",
		"minus/equals: volume -5/+5%; brackets: speed -0.1/+0.1x",
		"These requests target current audio, never the highlighted book.",
		"ORIGIN: " + paneNames[m.focus], enter,
		"Tab/Shift+Tab or 1-5: root pane focus; q/Ctrl+C: request orderly shutdown",
		"Books j/k or Up/Down: browse without switching audio",
		"Player Left/Right: select control; Enter: activate it",
		"UNAVAILABLE IN THIS SHELL",
		"Filter/sort/favorites, destinations, chapter jumps and text panning await their feature slices.",
		"Bookmarks/downloads, account/Settings, modal editors and real playback are not implemented.",
		"ERROR DETAILS - sticky until explicitly acknowledged",
	}
	if m.failure != "" {
		rows = append(rows, m.failure)
	} else {
		rows = append(rows, "No recorded failure")
	}
	var wrapped []string
	for _, row := range rows {
		// Strip metadata controls before wrapping, not after measuring its cells.
		wrapped = append(wrapped, strings.Split(ansi.Wrap(cleanLine(row), max(1, m.layout.Content.Width-4), ""), "\n")...)
	}
	return wrapped
}

func (m Model) helpKey(key string) (tea.Model, tea.Cmd) {
	capacity := m.layout.Content.Height - 2
	limit := max(0, len(m.helpRows())-capacity)
	offset := min(m.helpOffset, limit)
	switch key {
	case "esc":
		m.help = false
	case "j", "down":
		offset++
	case "k", "up":
		offset--
	case "g", "home":
		offset = 0
	case "G", "end":
		offset = limit
	case "pgdown":
		offset += capacity
	case "pgup":
		offset -= capacity
	case "ctrl+d":
		offset += max(1, capacity/2)
	case "ctrl+u":
		offset -= max(1, capacity/2)
	default:
		if action, ok := transportKeys[key]; ok {
			return m.transport(action)
		}
		return m, nil
	}
	m.helpOffset = min(limit, max(0, offset))
	return m, nil
}

func (m Model) helpLines() []string {
	rows := m.helpRows()
	capacity := m.layout.Content.Height - 2
	offset := min(m.helpOffset, max(0, len(rows)-capacity))
	label := fmt.Sprintf(" [Focus] Help %d-%d/%d ", offset+1, min(len(rows), offset+capacity), len(rows))
	return m.box(m.layout.Content, label, rows[offset:], -1, true, false)
}
