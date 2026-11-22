package ui_test

import (
	"image/color"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/ui"
)

func noteSpec() ui.OpenNoteEditorMsg {
	return ui.OpenNoteEditorMsg{ID: "capture-1", Target: ui.NoteTarget{
		Key: "A", Title: "River", Chapter: "A chapter", Captured: 12 * time.Second, Jump: 10 * time.Second,
	}}
}

func confirmSpec() ui.OpenConfirmationMsg {
	return ui.OpenConfirmationMsg{ID: "confirm-1", Title: "Switch book", Prompt: "Leave River for Stars?",
		Keep: "Stay with River", Confirm: "Switch and play Stars",
		Action: ui.Action{Kind: ui.PlayBook, Key: "B", Session: "session-a"}}
}

func TestNoteTypingFrozenTargetFailureRetryAndDurableResult(t *testing.T) {
	var actions []ui.Action
	r := start(t, fixture(), func(action ui.Action) tea.Cmd {
		actions = append(actions, action)
		return func() tea.Msg {
			return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: "Disk full; retry"}
		}
	})
	screen(t, r, 80, 20)
	send(t, r, key("j"), key("5"), noteSpec())
	for _, name := range []string{" ", "q", "1", "b", "B", "f", "j", "?", "ctrl+c", "ctrl+v"} {
		send(t, r, key(name))
	}
	send(t, r, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}, tea.PasteMsg{Content: "第二行\x1b[2J\x1b]0;ATTACK\x07"})
	data := fixture()
	data.Player.Key, data.Player.Session, data.Player.Position = "C", "session-c", time.Hour
	data.Books[0], data.Books[1] = data.Books[1], data.Books[0]
	send(t, r, data, ui.AppearanceMsg{Glyphs: ui.Plain, Palette: ui.Palette{Focus: color.White}}, key("enter"))
	if len(actions) != 1 {
		t.Fatalf("typing leaked or submit missing: %+v", actions)
	}
	want := ui.Action{Kind: ui.SaveNote, Key: "A", Target: noteSpec().Target, Text: " q1bBfj?\n第二行", RequestID: "capture-1", Attempt: 1}
	if actions[0] != want || r.Model.(ui.Model).Focus() != ui.PlayerPane || r.Model.(ui.Model).Selection() != "B" {
		t.Fatalf("capture retargeted or typing changed root: %+v", actions[0])
	}
	if !strings.Contains(r.Model.View().Content, "Saving; waiting") || r.Model.View().Cursor != nil {
		t.Fatal("submission must stay pending, not close optimistically")
	}
	send(t, r, key("enter"), key("ctrl+s"), key("esc"), key("q"), tea.PasteMsg{Content: "not while pending"}, confirmSpec())
	if len(actions) != 1 || strings.Contains(r.Model.View().Content, "Switch book") {
		t.Fatal("pending action accepted duplicate/input or was hidden by another owner")
	}
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Disk full; retry") || r.Model.View().Cursor == nil {
		t.Fatal("failure did not retain the editor and restore typing")
	}
	send(t, r, key("ctrl+s"))
	if len(actions) != 2 || actions[1].Attempt != 2 || actions[1].Target != want.Target || actions[1].Text != want.Text {
		t.Fatal("retry lost draft or target")
	}
	send(t, r, ui.ModalResultMsg{ID: "capture-1", Attempt: 1}, ui.ModalResultMsg{ID: "other", Attempt: 2})
	if !strings.Contains(r.Model.View().Content, "Saving; waiting") {
		t.Fatal("stale result closed current attempt")
	}
	send(t, r, ui.ModalResultMsg{ID: "capture-1", Attempt: 2})
	if strings.Contains(r.Model.View().Content, "[Focus] Note") || r.Model.View().Cursor != nil || !strings.Contains(r.Model.View().Content, "Player - Playing") {
		t.Fatal("durable result did not close only the editor")
	}
	if err := r.Drain(); err != nil { // late failure cannot reopen a successful editor
		t.Fatal(err)
	}
	if strings.Contains(r.Model.View().Content, "Disk full") {
		t.Fatal("late result resurrected a closed editor")
	}
}

func TestNestedConfirmationOwnsKeysAndUnwindsOneLayer(t *testing.T) {
	for _, pane := range []string{"1", "2", "3", "4", "5"} {
		var actions []ui.Action
		r := start(t, fixture(), func(action ui.Action) tea.Cmd { actions = append(actions, action); return nil })
		screen(t, r, 80, 20)
		send(t, r, key("j"), key(pane))
		origin := r.Model.View().Content
		send(t, r, noteSpec(), tea.PasteMsg{Content: "draft"}, key("tab")) // Save focused
		editor := r.Model.View().Content
		send(t, r, confirmSpec())
		confirm := r.Model.View().Content
		if strings.Count(confirm, "[Focus]") != 1 || r.Model.View().Cursor != nil {
			t.Fatal("confirmation is not the exclusive focus owner")
		}
		for _, name := range []string{" ", "q", "ctrl+c", "1", "5", "b", "B", "f", "?", "/", ",", ".", "[", "]", "-", "="} {
			send(t, r, key(name), tea.PasteMsg{Content: "leak"})
		}
		send(t, r, noteSpec(), confirmSpec()) // cannot replace active targets
		if r.Model.View().Content != confirm || len(actions) != 0 {
			t.Fatal("confirmation leaked input or replaced its fixed context")
		}
		screen(t, r, 79, 20)
		send(t, r, key("q"), key("ctrl+c"), key("esc"))
		if screen(t, r, 80, 20) != confirm || len(actions) != 0 {
			t.Fatal("resize quit recovery lost the nested owner or discarded a draft")
		}
		send(t, r, key("enter")) // default Stay, no mutation
		if r.Model.View().Content != editor || len(actions) != 0 {
			t.Fatal("Keep failed to restore parent focus/draft")
		}
		send(t, r, confirmSpec(), key("esc"))
		if r.Model.View().Content != editor {
			t.Fatal("child Esc did not unwind exactly one layer")
		}
		send(t, r, key("esc"))
		if r.Model.View().Content != origin {
			t.Fatal("editor close changed root focus/selection/audio")
		}
	}
}

func TestConfirmationFrozenSubmissionFailureAndSuccess(t *testing.T) {
	var actions []ui.Action
	r := start(t, fixture(), func(action ui.Action) tea.Cmd {
		actions = append(actions, action)
		return func() tea.Msg {
			return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: "Source changed; reconfirm"}
		}
	})
	screen(t, r, 144, 42)
	send(t, r, key("?"), noteSpec(), confirmSpec(), key("right"), key("left"), key("shift+tab"))
	data := fixture()
	data.Player.Session = "new-session"
	send(t, r, data, key("enter"), key("enter"), key("esc"), key("tab"))
	want := confirmSpec().Action
	want.RequestID, want.Attempt = "confirm-1", 1
	if len(actions) != 1 || actions[0] != want || !strings.Contains(r.Model.View().Content, "Working; waiting") {
		t.Fatalf("confirmation retargeted or duplicated: %+v", actions)
	}
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Source changed; reconfirm") {
		t.Fatal("failed submit lost recoverable dialog")
	}
	send(t, r, key("enter"), ui.ModalResultMsg{ID: "confirm-1", Attempt: 1}, ui.ModalResultMsg{ID: "confirm-1", Attempt: 2})
	if !strings.Contains(r.Model.View().Content, "[Focus] Note") {
		t.Fatal("successful child action closed parent editor")
	}
	send(t, r, key("esc"))
	if !strings.Contains(r.Model.View().Content, "[Focus] Help") {
		t.Fatal("nested editor did not restore supporting parent")
	}
}

func TestNoteGeometryRepaintResizeQuitWarningAndRecovery(t *testing.T) {
	var actions []ui.Action
	r := start(t, fixture(), func(action ui.Action) tea.Cmd {
		actions = append(actions, action)
		return func() tea.Msg { return ui.NoticeMsg{Error: "Shutdown failed; retry"} }
	})
	screen(t, r, 80, 20)
	send(t, r, key("j"), noteSpec(), tea.PasteMsg{Content: strings.Repeat("界e\u0301👩‍💻 long note\n", 20) + "FINAL"})
	for _, mode := range []ui.GlyphMode{ui.Plain, ui.Unicode} {
		send(t, r, ui.AppearanceMsg{Glyphs: mode})
		for _, size := range [][2]int{{80, 20}, {144, 42}, {80, 42}, {144, 20}, {81, 21}, {145, 43}} {
			view := screen(t, r, size[0], size[1])
			cursor := r.Model.View().Cursor
			if !strings.Contains(view, "FINAL") || !strings.Contains(view, "Player - Playing") || strings.Count(view, "[Focus]") != 1 || cursor == nil || cursor.X < 0 || cursor.X >= size[0] || cursor.Y < 0 || cursor.Y >= size[1]-7 {
				t.Fatal("modal overflow, lost focus/draft/cursor/player")
			}
		}
	}
	for _, size := range [][2]int{{79, 20}, {80, 19}, {1, 1}, {0, 0}} {
		screen(t, r, size[0], size[1])
		send(t, r, key(" "), key("tab"), key("1"), key("z"), tea.PasteMsg{Content: "hidden"}, key("ctrl+c"))
		if len(actions) != 0 || r.Model.View().Cursor != nil {
			t.Fatal("hidden editor received input/quit without warning")
		}
		screen(t, r, size[0], size[1]) // prompt itself must be bounded
		send(t, r, key("esc"))
	}
	screen(t, r, 79, 20)
	send(t, r, key("q"))
	if !strings.Contains(r.Model.View().Content, "Unsaved draft") {
		t.Fatal("first q discarded unsaved draft")
	}
	send(t, r, key("q"))
	if len(actions) != 1 || actions[0] != (ui.Action{Kind: ui.RequestQuit}) {
		t.Fatal("repeated q did not request orderly cleanup")
	}
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	view := screen(t, r, 80, 20)
	if !strings.Contains(view, "FINAL") || !strings.Contains(view, "Shutdown failed") || r.Model.(ui.Model).Selection() != "B" {
		t.Fatal("shutdown failure/resize lost draft, error or origin")
	}
	send(t, r, key("ctrl+s"))
	if strings.Contains(actions[1].Text, "hidden") || !strings.HasSuffix(actions[1].Text, "FINAL") {
		t.Fatal("hidden resize input modified draft")
	}
}

func TestModalValidationButtonsAndUnavailableCoordinator(t *testing.T) {
	r := start(t, fixture(), nil)
	screen(t, r, 80, 20)
	send(t, r, ui.OpenNoteEditorMsg{}, ui.OpenNoteEditorMsg{ID: "invalid"}, ui.OpenConfirmationMsg{})
	if strings.Contains(r.Model.View().Content, "[Focus] Note") {
		t.Fatal("invalid opening accepted")
	}
	oversized := noteSpec()
	oversized.Text = strings.Repeat("line\n", 10001)
	send(t, r, oversized)
	if !strings.Contains(r.Model.View().Content, "Note exceeds editor limits") || strings.Contains(r.Model.View().Content, "[Focus] Note") {
		t.Fatal("oversized existing note opened a silently truncated editor")
	}
	send(t, r, ui.DismissFailureMsg{})
	spec := noteSpec()
	spec.Text = " \n "
	send(t, r, spec, key("enter"))
	if !strings.Contains(r.Model.View().Content, "A note is required") {
		t.Fatal("whitespace-only note submitted")
	}
	send(t, r, tea.PasteMsg{Content: "text"}, key("tab"), tea.PasteMsg{Content: "not in button"}, key("q"), key("enter"))
	if !strings.Contains(r.Model.View().Content, "Save unavailable: no coordinator") {
		t.Fatal("unavailable save discarded draft")
	}
	send(t, r, key("shift+tab"), key("tab"), key("tab"), key("enter"))
	if strings.Contains(r.Model.View().Content, "[Focus] Note") {
		t.Fatal("Cancel button did not close editor")
	}
	send(t, r, confirmSpec(), key("tab"), key("enter"))
	if !strings.Contains(r.Model.View().Content, "Action unavailable: no coordinator") {
		t.Fatal("unavailable confirm closed dialog")
	}
	send(t, r, key("esc"), ui.ModalResultMsg{ID: "missing", Attempt: 1})
}

func TestLongConfirmationScrollsAndClampsWithinDialog(t *testing.T) {
	r := start(t, fixture(), nil)
	screen(t, r, 80, 20)
	spec := confirmSpec()
	spec.Prompt = strings.Repeat("長いe\u0301 \x1b[2J", 300) + " FINAL-PROMPT"
	send(t, r, spec, key("up"), key("k"))
	for range 100 {
		send(t, r, key("down"))
	}
	view := screen(t, r, 80, 20)
	if !strings.Contains(view, "FINAL-PROMPT") || strings.Contains(view, "\x1b") {
		t.Fatal("confirmation text inaccessible or unsafe")
	}
	send(t, r, key("j"))
	if r.Model.View().Content != view {
		t.Fatal("confirmation scroll escaped last page")
	}
	send(t, r, key("up"), key("esc"))
}

func TestNoteEditAndChapterBoundaryHeaders(t *testing.T) {
	r := start(t, fixture(), nil)
	screen(t, r, 80, 20)
	send(t, r, ui.OpenNoteEditorMsg{
		ID: "boundary-1",
		Target: ui.NoteTarget{
			Key:         "A",
			Title:       "River",
			Chapter:     "Ch 2",
			JumpChapter: "Ch 1",
			Captured:    61 * time.Second,
			Jump:        59 * time.Second,
		},
	})
	view := screen(t, r, 80, 20)
	if !strings.Contains(view, "Cap: Ch 2 -> Jump: Ch 1") || !strings.Contains(view, "Captured 00:01:01 | Jump 00:00:59") {
		t.Fatalf("missing chapter boundary header:\n%s", view)
	}
	send(t, r, key("esc"))

	send(t, r, ui.OpenNoteEditorMsg{
		ID: "edit-1",
		Target: ui.NoteTarget{
			Key:        "A",
			BookmarkID: "bm-1",
			Title:      "River",
			Chapter:    "Ch 1",
			Captured:   59 * time.Second,
			Jump:       59 * time.Second,
		},
		Text: "Existing note",
	})
	view = screen(t, r, 80, 20)
	if !strings.Contains(view, "Edit bookmark | Jump 00:00:59") || !strings.Contains(view, "Existing note") {
		t.Fatalf("missing edit bookmark header:\n%s", view)
	}
}
