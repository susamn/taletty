package ui_test

import (
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/ui"
)

func TestSettingsViewThemeSelectionPreferencesAndKeyOwnership(t *testing.T) {
	var actions []ui.Action
	r := start(t, fixture(), func(a ui.Action) tea.Cmd {
		actions = append(actions, a)
		return nil
	})
	screen(t, r, 144, 42)

	// Open Settings via 'S' from Books (with B selected after j).
	send(t, r, key("j"), key("S"))
	if !r.Model.(ui.Model).SettingsOpen() {
		t.Fatal("expected SettingsOpen true")
	}
	if len(actions) != 1 || actions[0].Kind != ui.OpenSettings {
		t.Fatalf("expected OpenSettings action, got %+v", actions)
	}

	// Inject SettingsMsg with custom theme and diagnostic.
	send(t, r, ui.SettingsMsg{
		Settings: ui.SettingsState{
			ActiveTheme: "colors",
			ThemeSource: "colors.toml",
			Themes:      []string{"colors", "solarized-dark", "catppuccin", "nord"},
			ThemeError:  "fallback warning",
			Glyphs:      ui.Unicode,
			SmartRewind: "10s / 5s",
			PreRoll:     "2s",
		},
	})
	view := screen(t, r, 144, 42)
	for _, want := range []string{"[Focus] Settings - Theme", "colors  (Active)", "nord", "Source: colors.toml", "Diagnostic: fallback warning", "Omarchy hook: explicit setup", "Smart rewind: 10s / 5s", "Bookmark pre-roll: 2s", "Glyph mode: Unicode", "Iosevka Slab"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q in wide Settings view:\n%s", want, view)
		}
	}

	// Candidate selection (j) does NOT apply theme until Enter!
	send(t, r, key("j"))
	view = screen(t, r, 144, 42)
	if !strings.Contains(view, "solarized-dark  (Candidate, not applied)") || len(actions) != 1 {
		t.Fatalf("candidate selection applied prematurely or missing label:\n%s", view)
	}

	// Blocked keys stay inside Settings (no transport, quit, or pane switch).
	for _, blocked := range []string{" ", ",", ".", "-", "=", "[", "]", "q", "ctrl+c", "1", "5", "b", "B", "f", "s", "d", "D", "t", "a", "?", "/"} {
		send(t, r, key(blocked))
	}
	if len(actions) != 1 || !r.Model.(ui.Model).SettingsOpen() {
		t.Fatalf("Settings leaked blocked keys: %+v", actions)
	}

	// Enter applies selected theme candidate; moving to [Reload active] dispatches ReloadTheme.
	send(t, r, key("enter"))
	if len(actions) != 2 || actions[1] != (ui.Action{Kind: ui.ApplyTheme, Text: "solarized-dark"}) {
		t.Fatalf("expected ApplyTheme solarized-dark, got %+v", actions)
	}
	send(t, r, key("k"), key("k"), key("enter")) // wraps to [Reload active]
	if len(actions) != 3 || actions[2].Kind != ui.ReloadTheme {
		t.Fatalf("expected ReloadTheme, got %+v", actions)
	}

	// Tab switches to Preferences section; cycle Smart rewind, Pre-roll, and Glyph mode.
	send(t, r, key("tab"), key("enter"), key("j"), key("enter"), key("j"), key("enter"))
	if len(actions) != 6 || actions[3].Kind != ui.CycleSmartRewind || actions[4].Kind != ui.CyclePreRoll || actions[5].Kind != ui.SetGlyphMode {
		t.Fatalf("unexpected preference actions: %+v", actions[3:])
	}
	send(t, r, key("k"), key("k"), key("tab")) // back to Theme section

	// Compact 80x20 stacks Theme and Preferences cleanly without overflow (both sections).
	send(t, r, ui.AppearanceMsg{Glyphs: ui.Plain, Palette: ui.Palette{Background: color.Black, Text: color.White, Focus: color.White, Error: color.White}})
	compact := screen(t, r, 80, 20)
	if !strings.Contains(compact, "[Focus] Settings - Theme & Preferences") {
		t.Fatalf("missing compact Settings header:\n%s", compact)
	}
	send(t, r, key("tab"), key("j"), key("j"))
	compactPref := screen(t, r, 80, 20)
	if !strings.Contains(compactPref, "Glyph mode: Plain") {
		t.Fatalf("compact Settings did not scroll to Preferences:\n%s", compactPref)
	}
	send(t, r, key("tab"))

	// Esc closes Settings and restores exact origin (Books, selection B).
	send(t, r, key("esc"))
	if r.Model.(ui.Model).SettingsOpen() || r.Model.(ui.Model).Focus() != ui.Books || r.Model.(ui.Model).Selection() != "B" {
		t.Fatal("Esc from Settings did not restore origin")
	}

	// OpenSettingsMsg blocked while Help is open; opens directly when clear; nil coordinator shows unavailable notice on Enter.
	send(t, r, key("?"), ui.OpenSettingsMsg{Settings: &ui.SettingsState{ActiveTheme: "colors"}})
	if r.Model.(ui.Model).SettingsOpen() {
		t.Fatal("OpenSettingsMsg should not open over Help")
	}
	send(t, r, key("esc"))
	noCoord := start(t, fixture(), nil)
	screen(t, noCoord, 80, 20)
	send(t, noCoord, ui.OpenSettingsMsg{Settings: &ui.SettingsState{ActiveTheme: "colors"}}, key("enter"))
	if !strings.Contains(noCoord.Model.View().Content, "Settings unavailable: no coordinator") {
		t.Fatal("expected no-coordinator Settings notice")
	}
	// Modal over Settings removes [Focus] from Settings.
	send(t, noCoord, ui.OpenConfirmationMsg{ID: "c1", Title: "Confirm", Prompt: "Test", Keep: "Keep", Confirm: "Ok", Action: ui.Action{Kind: ui.RequestQuit}})
	if strings.Contains(noCoord.Model.View().Content, "[Focus] Settings") {
		t.Fatal("Settings kept [Focus] behind modal")
	}
	send(t, noCoord, key("esc"), key("esc"))
}
