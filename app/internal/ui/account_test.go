package ui_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/ui"
)

func TestAccountOverlayOwnershipNavigationAndActions(t *testing.T) {
	var actions []ui.Action
	r := start(t, fixture(), func(action ui.Action) tea.Cmd {
		actions = append(actions, action)
		return nil
	})
	screen(t, r, 80, 20)

	// Open Account overlay via 'a' from root.
	send(t, r, key("a"))
	model := r.Model.(ui.Model)
	if !model.AccountOpen() {
		t.Fatal("expected 'a' to open Account overlay")
	}
	view := screen(t, r, 80, 20)
	for _, want := range []string{"[Focus] Account", "Status: Not signed in", "Login (browser TTY handoff)", "Sync library", "Check runtime (Doctor)", "Transport disabled in Account"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q in Account view:\n%s", want, view)
		}
	}

	// Account overlay must block transport, quit, pane focus, and help keys.
	for _, blocked := range []string{" ", ",", ".", "-", "=", "[", "]", "q", "ctrl+c", "1", "2", "3", "4", "5", "?", "b", "B", "f", "s", "d", "D", "S", "x", "t"} {
		send(t, r, key(blocked))
	}
	if len(actions) != 0 || !r.Model.(ui.Model).AccountOpen() {
		t.Fatalf("Account overlay leaked blocked keys: actions=%+v open=%v", actions, r.Model.(ui.Model).AccountOpen())
	}

	// Navigate options and dispatch Login, Sync, Doctor.
	send(t, r, key("enter"))              // Login
	send(t, r, key("j"), key("enter"))    // Sync
	send(t, r, key("down"), key("enter")) // Doctor
	send(t, r, key("tab"), key("shift+tab"), key("k"), key("up"))
	if len(actions) != 3 ||
		actions[0].Kind != ui.AccountLogin ||
		actions[1].Kind != ui.AccountSync ||
		actions[2].Kind != ui.AccountDoctor {
		t.Fatalf("unexpected Account actions: %+v", actions)
	}

	// Busy state blocks duplicate account operations without dispatching.
	send(t, r, ui.AccountStatusMsg{Account: ui.AccountState{
		Status: "Syncing",
		Detail: "Syncing library...",
		Busy:   true,
	}})
	send(t, r, key("enter"))
	if len(actions) != 3 || !strings.Contains(r.Model.View().Content, "Account operation already in progress") {
		t.Fatalf("busy guard failed: actions=%+v view=%s", actions, r.Model.View().Content)
	}

	// Resize below minimum hides overlay without losing it, then restores cleanly.
	screen(t, r, 79, 19)
	if !r.Model.(ui.Model).AccountOpen() {
		t.Fatal("resize below minimum closed Account overlay")
	}
	view = screen(t, r, 144, 42)
	if !r.Model.(ui.Model).AccountOpen() || !strings.Contains(view, "[Focus] Account") {
		t.Fatal("resize recovery lost Account overlay")
	}

	// Esc closes Account overlay and restores prior focus.
	send(t, r, key("esc"))
	if r.Model.(ui.Model).AccountOpen() || r.Model.(ui.Model).Focus() != ui.Books {
		t.Fatalf("Esc did not close Account overlay cleanly: open=%v focus=%v", r.Model.(ui.Model).AccountOpen(), r.Model.(ui.Model).Focus())
	}
}

func TestAccountOnboardingContinueOfflineAndSanitization(t *testing.T) {
	var actions []ui.Action
	r := start(t, ui.SnapshotMsg{}, func(action ui.Action) tea.Cmd {
		actions = append(actions, action)
		return nil
	})
	screen(t, r, 80, 20)

	focus := ui.Details
	send(t, r, ui.RestoreSessionMsg{
		Focus:       &focus,
		OpenAccount: true,
		Account: &ui.AccountState{
			Status:     "Not signed in\x1b[2J",
			Detail:     "Fresh Taletty root (~/.config/taletty/).\x1b]0;BAD\x07 Sign in or continue offline.",
			Error:      "Auth busy\x1b[31m",
			Onboarding: true,
		},
	})
	view := screen(t, r, 80, 20)
	if strings.Contains(r.Model.View().Content, "\x1b[2J") || strings.Contains(r.Model.View().Content, "\x1b]0;BAD") {
		t.Fatal("Account overlay leaked ANSI escape sequences")
	}
	if !strings.Contains(view, "Continue offline") {
		t.Fatalf("expected Continue offline option in onboarding view:\n%s", view)
	}

	// Move to Continue offline (4th option) and press Enter.
	send(t, r, key("k"), key("enter"))
	if r.Model.(ui.Model).AccountOpen() {
		t.Fatal("Continue offline did not close Account overlay")
	}
	if len(actions) != 1 || actions[0].Kind != ui.AccountContinueOffline {
		t.Fatalf("expected AccountContinueOffline action, got %+v", actions)
	}
	if r.Model.(ui.Model).Focus() != ui.Details {
		t.Fatalf("expected restored focus Details, got %v", r.Model.(ui.Model).Focus())
	}

	// CloseAccountMsg and OpenAccountMsg with nil/non-nil state, plus nil dispatcher guard.
	rNil := start(t, ui.SnapshotMsg{}, nil)
	screen(t, rNil, 80, 20)
	send(t, rNil, ui.OpenAccountMsg{Account: &ui.AccountState{SignedIn: true}})
	view = screen(t, rNil, 80, 20)
	if !strings.Contains(view, "Status: Signed in") {
		t.Fatalf("expected Signed in status:\n%s", view)
	}
	send(t, rNil, key("enter"))
	if !strings.Contains(rNil.Model.View().Content, "Action unavailable: no coordinator") {
		t.Fatal("nil dispatcher did not report unavailable action")
	}
	send(t, rNil, key("k"), key("enter")) // Close without onboarding and nil dispatch
	if rNil.Model.(ui.Model).AccountOpen() {
		t.Fatal("Close option did not close Account overlay")
	}
	send(t, rNil, ui.OpenAccountMsg{Account: &ui.AccountState{Offline: true}})
	view = screen(t, rNil, 80, 20)
	if !strings.Contains(view, "Status: Offline") {
		t.Fatalf("expected Offline status:\n%s", view)
	}
	send(t, rNil, ui.CloseAccountMsg{})
	if rNil.Model.(ui.Model).AccountOpen() {
		t.Fatal("CloseAccountMsg did not close Account overlay")
	}
}

func TestRestoreSessionSelectionAndModalPrecedence(t *testing.T) {
	r := start(t, fixture(), nil)
	screen(t, r, 144, 42)

	// Restore selection by BookKey and focus to Chapters.
	focus := ui.Chapters
	send(t, r, ui.RestoreSessionMsg{
		Focus:    &focus,
		Selected: "B",
	})
	model := r.Model.(ui.Model)
	if model.Focus() != ui.Chapters || model.Selection() != "B" {
		t.Fatalf("RestoreSessionMsg failed: focus=%v selection=%q", model.Focus(), model.Selection())
	}

	// Unknown selection keeps existing valid selection.
	send(t, r, ui.RestoreSessionMsg{Selected: "missing"})
	if r.Model.(ui.Model).Selection() != "B" {
		t.Fatalf("unknown selection overwrote valid selection: %q", r.Model.(ui.Model).Selection())
	}

	// OpenAccountMsg must not steal focus from an active modal or Help.
	send(t, r, ui.OpenNoteEditorMsg{ID: "n1", Target: ui.NoteTarget{Key: "B", Title: "Stars"}})
	send(t, r, ui.OpenAccountMsg{Account: &ui.AccountState{Status: "Updated"}})
	if r.Model.(ui.Model).AccountOpen() {
		t.Fatal("OpenAccountMsg stole focus from active note modal")
	}
	send(t, r, key("esc"))
	send(t, r, key("?"))
	send(t, r, ui.OpenAccountMsg{})
	if r.Model.(ui.Model).AccountOpen() {
		t.Fatal("OpenAccountMsg stole focus from Help")
	}
}
