package ui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
)

// SettingsState is a sanitized presentation projection of theme and playback
// preferences. Candidate selection never mutates active state until applied.
type SettingsState struct {
	ActiveTheme string
	ThemeSource string
	Themes      []string
	ThemeError  string
	Glyphs      GlyphMode
	SmartRewind string
	PreRoll     string
}

// SettingsMsg updates SettingsState without stealing focus or closing Settings.
type SettingsMsg struct {
	Settings SettingsState
}

// OpenSettingsMsg opens the Settings view, optionally updating SettingsState.
type OpenSettingsMsg struct {
	Settings *SettingsState
}

// SettingsOpen reports whether the Settings view is currently open.
func (m Model) SettingsOpen() bool { return m.settingsOpen }

func (m Model) effectiveSettings() SettingsState {
	s := m.settings
	if s.ActiveTheme == "" {
		s.ActiveTheme = "colors"
	}
	if s.ThemeSource == "" {
		s.ThemeSource = "colors.toml"
	}
	if len(s.Themes) == 0 {
		s.Themes = []string{"colors", "solarized-dark", "catppuccin"}
	}
	if s.SmartRewind == "" {
		s.SmartRewind = "10s / 5s"
	}
	if s.PreRoll == "" {
		s.PreRoll = "2s"
	}
	return s
}

func (m *Model) openSettings(state *SettingsState) {
	if state != nil {
		m.settings = *state
	}
	if m.ownsModal() || m.help || m.accountOpen || m.sortOpen {
		return
	}
	m.settingsOpen = true
	m.settingsSection = 0
	m.settingsCursor = 0
	m.settingsPrefCursor = 0
}

func (m Model) settingsKey(key string) (tea.Model, tea.Cmd) {
	s := m.effectiveSettings()
	themeCount := len(s.Themes) + 1 // themes + [Reload active]
	const prefCount = 3             // Smart rewind, Bookmark pre-roll, Glyph mode

	switch key {
	case "esc":
		m.settingsOpen = false
		return m, nil
	case "tab", "shift+tab":
		m.settingsSection = (m.settingsSection + 1) % 2
		return m, nil
	case "j", "down":
		if m.settingsSection == 0 {
			m.settingsCursor = (m.settingsCursor + 1) % themeCount
		} else {
			m.settingsPrefCursor = (m.settingsPrefCursor + 1) % prefCount
		}
		return m, nil
	case "k", "up":
		if m.settingsSection == 0 {
			m.settingsCursor = (m.settingsCursor + themeCount - 1) % themeCount
		} else {
			m.settingsPrefCursor = (m.settingsPrefCursor + prefCount - 1) % prefCount
		}
		return m, nil
	case "enter":
		if m.dispatch == nil {
			m.data.Activity = "Settings unavailable: no coordinator"
			return m, nil
		}
		if m.settingsSection == 0 {
			cur := min(themeCount-1, max(0, m.settingsCursor))
			if cur == len(s.Themes) {
				return m.request(Action{Kind: ReloadTheme})
			}
			return m.request(Action{Kind: ApplyTheme, Text: s.Themes[cur]})
		}
		switch min(prefCount-1, max(0, m.settingsPrefCursor)) {
		case 0:
			return m.request(Action{Kind: CycleSmartRewind})
		case 1:
			return m.request(Action{Kind: CyclePreRoll})
		default:
			return m.request(Action{Kind: SetGlyphMode})
		}
	default:
		// Settings owns all keys locally: no transport, quit, or pane passthrough.
		return m, nil
	}
}

func (m Model) settingsLines() []string {
	s := m.effectiveSettings()
	glyphLabel := "Unicode"
	if m.appearance.Glyphs == Plain {
		glyphLabel = "Plain"
	}

	themeRows := []string{"Palette candidates:"}
	themeSelectedRow := -1
	for i, name := range s.Themes {
		status := ""
		if name == s.ActiveTheme {
			status = "  (Active)"
		} else if m.settingsSection == 0 && i == m.settingsCursor {
			status = "  (Candidate, not applied)"
		}
		if m.settingsSection == 0 && i == m.settingsCursor {
			themeSelectedRow = len(themeRows)
		}
		themeRows = append(themeRows, name+status)
	}
	if m.settingsSection == 0 && m.settingsCursor == len(s.Themes) {
		themeSelectedRow = len(themeRows)
	}
	themeRows = append(themeRows, "[Reload active]", "")
	themeRows = append(themeRows, "Active: "+cleanLine(s.ActiveTheme))
	themeRows = append(themeRows, "Source: "+cleanLine(s.ThemeSource))
	if errText := cleanLine(s.ThemeError); errText != "" {
		themeRows = append(themeRows, "Diagnostic: "+errText)
	}
	themeRows = append(themeRows, "Omarchy hook: explicit setup, never automatic.")

	prefRows := []string{"Playback / display preferences:"}
	prefSelectedRow := -1
	prefs := []string{
		"Smart rewind: " + cleanLine(s.SmartRewind),
		"Bookmark pre-roll: " + cleanLine(s.PreRoll),
		"Glyph mode: " + glyphLabel,
	}
	for i, label := range prefs {
		if m.settingsSection == 1 && i == m.settingsPrefCursor {
			prefSelectedRow = len(prefRows)
		}
		prefRows = append(prefRows, label)
	}
	prefRows = append(prefRows,
		"Font: use a terminal profile (e.g. Iosevka Slab)",
		"",
		"Invalid palette: last usable retained.",
		"Esc returns to original focus.",
	)

	c := m.layout.Content
	if m.layout.Mode == Wide {
		leftWidth := c.Width / 2
		rightWidth := c.Width - leftWidth
		leftRect := Rect{X: c.X, Y: c.Y, Width: leftWidth, Height: c.Height}
		rightRect := Rect{X: c.X + leftWidth, Y: c.Y, Width: rightWidth, Height: c.Height}
		leftTitle := " Settings - Theme "
		rightTitle := " Settings - Preferences "
		if !m.ownsModal() && !m.help {
			if m.settingsSection == 0 {
				leftTitle = " [Focus] Settings - Theme "
			} else {
				rightTitle = " [Focus] Settings - Preferences "
			}
		}
		leftBox := m.box(leftRect, leftTitle, themeRows, themeSelectedRow, m.settingsSection == 0 && !m.ownsModal() && !m.help, true)
		rightBox := m.box(rightRect, rightTitle, prefRows, prefSelectedRow, m.settingsSection == 1 && !m.ownsModal() && !m.help, true)
		lines := make([]string, c.Height)
		for y := range c.Height {
			lines[y] = leftBox[y] + rightBox[y]
		}
		return lines
	}

	// Compact mode stacks Theme and Preferences in one scrolling view.
	combined := append([]string{}, themeRows...)
	combined = append(combined, "")
	prefOffset := len(combined)
	combined = append(combined, prefRows...)
	selected := themeSelectedRow
	if m.settingsSection == 1 && prefSelectedRow >= 0 {
		selected = prefOffset + prefSelectedRow
	}
	title := " Settings - Theme & Preferences "
	if !m.ownsModal() && !m.help {
		title = fmt.Sprintf(" [Focus]%s", title)
	}
	return m.box(c, title, combined, selected, !m.ownsModal() && !m.help, true)
}
