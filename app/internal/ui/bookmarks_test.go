package ui_test

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/ui"
)

func bookmarksFixture() []ui.Bookmark {
	return []ui.Bookmark{
		{
			ID:           "bm-1",
			Key:          "B",
			Position:     10 * time.Second,
			ChapterID:    "ch-1",
			ChapterTitle: "Ch 1 Opening",
			Note:         "First line\nLine two\nThird line\nFourth line\nFifth line\nSixth line\nSeventh line\nEighth line\nFINAL-NOTE-LINE",
			CreatedAtMS:  100,
			UpdatedAtMS:  100,
		},
		{
			ID:           "bm-2",
			Key:          "B",
			Position:     125 * time.Second,
			ChapterTitle: "",
			Note:         "Second bookmark\x1b[2J\x1b]0;ATTACK\x07",
			CreatedAtMS:  200,
			UpdatedAtMS:  200,
		},
	}
}

func TestBookmarkCaptureAndExplicitBTargeting(t *testing.T) {
	var actions []ui.Action
	r := start(t, fixture(), func(a ui.Action) tea.Cmd {
		actions = append(actions, a)
		return nil
	})
	screen(t, r, 144, 42)

	// Highlight B (Stars) while A (River) is playing; b captures current audio A.
	send(t, r, key("j"), key("b"))
	if len(actions) != 1 || actions[0] != (ui.Action{Kind: ui.CaptureBookmark, Key: "A", Session: "session-a"}) {
		t.Fatalf("b did not capture current audio A: %+v", actions)
	}

	// B from Books/Details/Chapters targets highlighted book B.
	for _, paneKey := range []string{"2", "3", "4"} {
		send(t, r, key(paneKey), key("B"))
		model := r.Model.(ui.Model)
		if !model.BookmarksOpen() || model.BookmarksTarget() != "B" {
			t.Fatalf("B from pane %s did not target B: open=%v target=%q", paneKey, model.BookmarksOpen(), model.BookmarksTarget())
		}
		// Pressing B again inside open manager never retargets.
		send(t, r, key("B"))
		if r.Model.(ui.Model).BookmarksTarget() != "B" {
			t.Fatal("B retargeted open manager")
		}
		send(t, r, key("esc"))
		if r.Model.(ui.Model).BookmarksOpen() {
			t.Fatal("Esc did not close manager")
		}
	}

	// Library sidebar Bookmarks entry names highlighted book and opens manager for B.
	send(t, r, key("1"), key("G"), key("k"))
	view := screen(t, r, 144, 42)
	if !strings.Contains(view, "Bookmarks: Stars") {
		t.Fatalf("missing named Bookmarks sidebar entry:\n%s", view)
	}
	send(t, r, key("enter"))
	if !r.Model.(ui.Model).BookmarksOpen() || r.Model.(ui.Model).BookmarksTarget() != "B" {
		t.Fatal("Library sidebar Bookmarks entry did not open B")
	}
	send(t, r, key("esc"))

	// B from PlayerPane targets loaded current audio A (even when paused).
	paused := fixture()
	paused.Player.State = ui.Paused
	send(t, r, paused, key("5"), key("B"))
	if !r.Model.(ui.Model).BookmarksOpen() || r.Model.(ui.Model).BookmarksTarget() != "A" {
		t.Fatalf("B from paused PlayerPane did not target A: open=%v target=%q", r.Model.(ui.Model).BookmarksOpen(), r.Model.(ui.Model).BookmarksTarget())
	}
	send(t, r, key("esc"))

	// Unavailable cases: no loaded audio for b or Player B; empty library for B; nil coordinator.
	noAudio := fixture()
	noAudio.Player = ui.Player{Key: "A", State: ui.Stopped}
	send(t, r, noAudio, key("2"), key("b"))
	if !strings.Contains(r.Model.View().Content, "Bookmark capture unavailable: no loaded audio") {
		t.Fatal("b should be unavailable when stopped")
	}
	send(t, r, key("5"), key("B"))
	if r.Model.(ui.Model).BookmarksOpen() || !strings.Contains(r.Model.View().Content, "Bookmarks unavailable: no loaded audio in Player") {
		t.Fatal("B in stopped Player should be unavailable")
	}
	send(t, r, ui.SnapshotMsg{}, key("2"), key("B"))
	if r.Model.(ui.Model).BookmarksOpen() || !strings.Contains(r.Model.View().Content, "Bookmarks unavailable: no highlighted book") {
		t.Fatal("B with no highlighted book should be unavailable")
	}

	noCoord := start(t, fixture(), nil)
	screen(t, noCoord, 80, 20)
	send(t, noCoord, key("b"))
	if !strings.Contains(noCoord.Model.View().Content, "Bookmark capture unavailable: no coordinator") {
		t.Fatal("expected no-coordinator capture message")
	}
	send(t, noCoord, key("B"))
	if !strings.Contains(noCoord.Model.View().Content, "Bookmarks unavailable: no coordinator") {
		t.Fatal("expected no-coordinator manager message")
	}
}

func TestBookmarksManagerNavigationFilterNoteScrollAndActions(t *testing.T) {
	var actions []ui.Action
	r := start(t, fixture(), func(a ui.Action) tea.Cmd {
		actions = append(actions, a)
		return nil
	})
	screen(t, r, 80, 20)

	// Highlight B, open Bookmarks Manager, deliver query error first, then retry and populate.
	send(t, r, key("j"), key("B"))
	send(t, r, ui.OpenBookmarksMsg{Key: "B", Title: "Stars", Error: "Database read failed"})
	view := screen(t, r, 80, 20)
	if !strings.Contains(view, "Bookmarks - Stars") || !strings.Contains(view, "Failed to load bookmarks: Database read failed") {
		t.Fatalf("query failure did not preserve target header and retry prompt:\n%s", view)
	}
	send(t, r, key("x"))
	if !strings.Contains(r.Model.View().Content, "Reload bookmarks before opening actions") {
		t.Fatal("x should refuse while query error is active")
	}
	send(t, r, key("r"))
	if actions[len(actions)-1] != (ui.Action{Kind: ui.OpenBookmarks, Key: "B"}) {
		t.Fatalf("r did not retry OpenBookmarks: %+v", actions)
	}
	// Stale reply for another book is ignored.
	send(t, r, ui.OpenBookmarksMsg{Key: "A", Title: "River", Bookmarks: bookmarksFixture()})
	if r.Model.(ui.Model).BookmarksTarget() != "B" {
		t.Fatal("stale OpenBookmarksMsg retargeted manager")
	}
	send(t, r, ui.OpenBookmarksMsg{Key: "B", Title: "Stars", Bookmarks: bookmarksFixture()})

	// Background snapshot changing selection/audio cannot retarget open manager.
	bg := fixture()
	bg.Player.Key, bg.Player.Session, bg.Player.Title = "C", "session-c", "Cloud"
	send(t, r, bg)
	if r.Model.(ui.Model).BookmarksTarget() != "B" {
		t.Fatal("background snapshot retargeted manager")
	}

	// Tab cycles List -> Note scroller -> Player -> List; Note scroller scrolls text without moving cursor.
	send(t, r, key("tab"))
	for range 10 {
		send(t, r, key("j"))
	}
	view = screen(t, r, 80, 20)
	if !strings.Contains(view, "[Focus] Note") || !strings.Contains(view, "FINAL-NOTE-LINE") {
		t.Fatalf("note scroller did not scroll to bottom line:\n%s", view)
	}
	send(t, r, key("tab"), key("right"), key("enter"))
	if actions[len(actions)-1] != (ui.Action{Kind: ui.SeekBackward, Key: "C", Session: "session-c"}) {
		t.Fatalf("Player focus inside manager did not activate control: %+v", actions)
	}
	send(t, r, key("shift+tab"), key("shift+tab")) // back to List

	// Read-only list permits transport and b capture (targeting current audio C, not B).
	send(t, r, key(" "), key("b"))
	if actions[len(actions)-2].Kind != ui.TogglePlayback || actions[len(actions)-1] != (ui.Action{Kind: ui.CaptureBookmark, Key: "C", Session: "session-c"}) {
		t.Fatalf("read-only manager transport/b failed: %+v", actions)
	}

	// Local filter inside Bookmarks Manager owns typing, filters notes/chapters, Esc restores/clears.
	send(t, r, key("/"), key("q"), key("ctrl+u"), tea.PasteMsg{Content: "second\x1b[2J"}, key("enter"))
	view = screen(t, r, 80, 20)
	if !strings.Contains(view, `Filter: "second"`) || strings.Contains(view, "\x1b") || strings.Contains(view, "ATTACK") {
		t.Fatalf("bookmark filter or ANSI sanitization failed:\n%s", view)
	}
	send(t, r, key("j"), key("k"))
	// Enter jumps to selected bookmark (bm-2).
	send(t, r, key("enter"))
	last := actions[len(actions)-1]
	if last.Kind != ui.JumpBookmark || last.Key != "B" || last.BookmarkID != "bm-2" || last.BookmarkPos != 125*time.Second {
		t.Fatalf("Enter did not dispatch JumpBookmark for bm-2: %+v", last)
	}

	// Esc clears applied filter first before closing manager.
	send(t, r, key("esc"))
	if !r.Model.(ui.Model).BookmarksOpen() {
		t.Fatal("first Esc should clear filter, not close manager")
	}

	// Row action menu (x): frozen target ID + stale-target invalidation.
	send(t, r, key("g"), key("x"))
	view = screen(t, r, 80, 20)
	if !strings.Contains(view, "[Focus] Bookmark Actions") || !strings.Contains(view, "Edit note") {
		t.Fatalf("missing row action menu:\n%s", view)
	}
	// Remove bm-1 via BookmarksMsg while menu is open -> menu becomes stale and refuses Enter.
	send(t, r, ui.BookmarksMsg{Key: "B", Bookmarks: bookmarksFixture()[1:]})
	send(t, r, key("enter"))
	if !strings.Contains(r.Model.View().Content, "Target bookmark no longer exists") {
		t.Fatal("stale row action menu executed on wrong row")
	}
	send(t, r, key("esc"))

	// Open menu on bm-2 -> Edit note opens NoteEditor with BookmarkID frozen.
	send(t, r, key("x"), key("j"), key("enter"))
	view = screen(t, r, 80, 20)
	if !strings.Contains(view, "[Focus] Note") || !strings.Contains(view, "Edit bookmark | Jump 00:02:05") {
		t.Fatalf("Edit note did not open editor with frozen BookmarkID:\n%s", view)
	}
	send(t, r, key("esc"))
	if !r.Model.(ui.Model).BookmarksOpen() {
		t.Fatal("Esc from nested edit did not return to Bookmarks Manager")
	}

	// Open menu on bm-2 -> Delete bookmark opens default-safe Keep bookmark confirmation.
	send(t, r, key("x"), key("j"), key("j"), key("enter"))
	view = screen(t, r, 80, 20)
	if !strings.Contains(view, "[Focus] Delete bookmark") || !strings.Contains(view, "[Keep bookmark]") {
		t.Fatalf("Delete bookmark did not open default-safe confirmation:\n%s", view)
	}
	send(t, r, key("tab"), key("enter"))
	del := actions[len(actions)-1]
	if del.Kind != ui.DeleteBookmark || del.Key != "B" || del.BookmarkID != "bm-2" {
		t.Fatalf("Delete confirmation did not dispatch DeleteBookmark: %+v", del)
	}
	send(t, r, ui.ModalResultMsg{ID: del.RequestID, Attempt: del.Attempt})

	// Empty state after deleting final bookmark.
	send(t, r, ui.BookmarksMsg{Key: "B", Bookmarks: nil})
	view = screen(t, r, 144, 42)
	if !strings.Contains(view, "No bookmarks for this book. Press b to capture current audio.") {
		t.Fatalf("missing empty bookmarks guidance:\n%s", view)
	}

	// Numeric focus 4 closes manager and focuses Details; Esc from manager restores origin.
	send(t, r, key("4"))
	if r.Model.(ui.Model).BookmarksOpen() || r.Model.(ui.Model).Focus() != ui.Details {
		t.Fatal("numeric focus did not close manager and focus Details")
	}
}

func TestBookmarksEdgeCasesDirectOpenFilterCancelAndMenuJump(t *testing.T) {
	var actions []ui.Action
	r := start(t, fixture(), func(a ui.Action) tea.Cmd {
		actions = append(actions, a)
		return nil
	})
	screen(t, r, 80, 20)

	// Direct OpenBookmarksMsg with empty key ignored; valid key opens manager directly.
	send(t, r, ui.OpenBookmarksMsg{})
	if r.Model.(ui.Model).BookmarksOpen() {
		t.Fatal("empty OpenBookmarksMsg should be ignored")
	}
	longNote := strings.Repeat("Long note preview ", 5)
	bms := []ui.Bookmark{
		{ID: "bm-long", Key: "Z", Position: 42 * time.Second, ChapterTitle: "Ch 3", Note: longNote},
	}
	// Book Z is not in Books or Player -> falls back to "Audiobook".
	send(t, r, ui.OpenBookmarksMsg{Key: "Z", Bookmarks: bms})
	if !r.Model.(ui.Model).BookmarksOpen() || !strings.Contains(r.Model.View().Content, "Bookmarks - Audiobook") {
		t.Fatalf("direct OpenBookmarksMsg failed:\n%s", r.Model.View().Content)
	}

	// BookmarksMsg for wrong key or with error does not overwrite list.
	send(t, r, ui.BookmarksMsg{Key: "OTHER", Bookmarks: nil})
	send(t, r, ui.BookmarksMsg{Key: "Z", Error: "Transient error"})
	if !strings.Contains(r.Model.View().Content, "Failed to load bookmarks: Transient error") {
		t.Fatal("expected error state on BookmarksMsg error")
	}
	// Enter on error retries OpenBookmarks.
	send(t, r, key("enter"))
	if actions[len(actions)-1] != (ui.Action{Kind: ui.OpenBookmarks, Key: "Z"}) {
		t.Fatalf("Enter on error did not retry: %+v", actions)
	}
	send(t, r, ui.OpenBookmarksMsg{Key: "Z", Title: "Custom Z", Bookmarks: bms})

	// Filter typing, paste, no-match message, and Esc restoring prior filter.
	send(t, r, key("/"), key("l"), key("o"), key("n"), key("g"), key("enter"))
	send(t, r, key("/"), tea.PasteMsg{Content: "nomatch"}, key("enter"))
	if !strings.Contains(r.Model.View().Content, "No bookmarks match filter") {
		t.Fatal("expected no-match filter message")
	}
	send(t, r, key("/"), key("x"), key("esc")) // Esc restores "nomatch"
	send(t, r, key("esc"))                     // Esc clears "nomatch"

	// Help inside manager returns to manager on Esc; q requests quit.
	send(t, r, key("?"), key("esc"))
	if !r.Model.(ui.Model).BookmarksOpen() {
		t.Fatal("Esc from Help did not return to Bookmarks Manager")
	}

	// Menu navigation up/down, Jump to bookmark, and long-note Delete preview truncation.
	send(t, r, key("x"), key("k"), key("j"), key("enter"))
	if actions[len(actions)-1].Kind != ui.JumpBookmark || actions[len(actions)-1].BookmarkID != "bm-long" {
		t.Fatalf("menu Jump failed: %+v", actions)
	}
	send(t, r, key("x"), key("k"), key("enter")) // k wraps to Delete bookmark
	if !strings.Contains(r.Model.View().Content, "...") {
		t.Fatal("expected truncated note preview in Delete confirmation")
	}
	send(t, r, key("esc"))

	// Player focus left/right inside manager, then q quit.
	send(t, r, key("tab"), key("tab"), key("left"), key("right"), key("q"))
	if actions[len(actions)-1].Kind != ui.RequestQuit {
		t.Fatalf("q inside manager did not request quit: %+v", actions)
	}

	// Empty list Enter/x notices.
	send(t, r, ui.BookmarksMsg{Key: "Z", Bookmarks: nil}, key("shift+tab"), key("shift+tab"), key("enter"), key("x"))
	if !strings.Contains(r.Model.View().Content, "No bookmark selected") {
		t.Fatal("expected No bookmark selected notice")
	}
	// Player title fallback when book is only in Player.
	send(t, r, key("esc"))
	onlyPlayer := ui.SnapshotMsg{
		Player: ui.Player{Key: "P", Session: "sp", Title: "Player Only", State: ui.Paused},
	}
	send(t, r, onlyPlayer, key("5"), key("B"))
	if !strings.Contains(r.Model.View().Content, "Bookmarks - Player Only") {
		t.Fatalf("expected Player title fallback:\n%s", r.Model.View().Content)
	}
}

func TestGlobalBookmarkSearchAndExportTrigger(t *testing.T) {
	var actions []ui.Action
	r := start(t, fixture(), func(a ui.Action) tea.Cmd {
		actions = append(actions, a)
		return nil
	})
	screen(t, r, 144, 42)

	// Open All bookmarks (search & export) from Library sidebar (G).
	send(t, r, key("1"), key("G"), key("enter"))
	if !r.Model.(ui.Model).BookmarksOpen() || !r.Model.(ui.Model).BookmarksGlobal() {
		t.Fatal("expected global bookmark search open")
	}
	if actions[len(actions)-1].Kind != ui.OpenGlobalBookmarks {
		t.Fatalf("expected OpenGlobalBookmarks action, got %+v", actions)
	}

	// Error + retry in global mode (both 'r' and 'enter'), then populate across books A and B.
	send(t, r, ui.OpenBookmarksMsg{Global: true, Error: "Query failed"})
	send(t, r, key("e"))
	if !strings.Contains(r.Model.View().Content, "Reload bookmarks before exporting") {
		t.Fatal("e should refuse while query error is active")
	}
	send(t, r, key("r"))
	if actions[len(actions)-1].Kind != ui.OpenGlobalBookmarks {
		t.Fatalf("r did not retry OpenGlobalBookmarks: %+v", actions)
	}
	send(t, r, key("enter"))
	if actions[len(actions)-1].Kind != ui.OpenGlobalBookmarks {
		t.Fatalf("enter on error did not retry OpenGlobalBookmarks: %+v", actions)
	}
	// Mismatched single-book OpenBookmarksMsg / BookmarksMsg ignored while in global mode.
	send(t, r, ui.OpenBookmarksMsg{Key: "A", Title: "River"}, ui.BookmarksMsg{Key: "A"})
	if !r.Model.(ui.Model).BookmarksGlobal() {
		t.Fatal("single-book message altered global mode")
	}
	send(t, r, ui.OpenBookmarksMsg{
		Global: true,
		Bookmarks: []ui.Bookmark{
			{ID: "bm-a", Key: "A", Position: 15 * time.Second, ChapterTitle: "Ch 1", Note: "River note"},
			{ID: "bm-b", Key: "B", BookTitle: "Stars", Position: 45 * time.Second, ChapterTitle: "Ch 2", Note: "Stars note"},
		},
	})
	view := screen(t, r, 144, 42)
	if !strings.Contains(view, "River | 00:00:15") || !strings.Contains(view, "Stars | 00:00:45") {
		t.Fatalf("global search missing book titles:\n%s", view)
	}

	// Filter by book title "stars", jump targets B, export carries filter query.
	send(t, r, key("/"), key("s"), key("t"), key("a"), key("r"), key("s"), key("enter"))
	send(t, r, key("enter"))
	last := actions[len(actions)-1]
	if last.Kind != ui.JumpBookmark || last.Key != "B" || last.BookmarkID != "bm-b" {
		t.Fatalf("global jump did not target B/bm-b: %+v", last)
	}
	send(t, r, key("e"))
	exp := actions[len(actions)-1]
	if exp.Kind != ui.ExportBookmarks || exp.Key != "" || exp.Text != "stars" {
		t.Fatalf("global export action mismatch: %+v", exp)
	}

	// Row action menu in global mode freezes (bm.Key, bm.ID) and updates via BookmarksMsg{Global: true}.
	send(t, r, key("x"), key("j"), key("enter")) // Edit note for B/bm-b
	if !strings.Contains(r.Model.View().Content, "Stars | Ch 2") {
		t.Fatalf("global edit did not target Stars:\n%s", r.Model.View().Content)
	}
	send(t, r, key("esc"))
	send(t, r, ui.BookmarksMsg{Global: true, Bookmarks: nil})
	if !strings.Contains(r.Model.View().Content, "No bookmarks across library") {
		t.Fatalf("missing empty global bookmarks message:\n%s", r.Model.View().Content)
	}

	// Nil coordinator guards for global open and export.
	noCoord := start(t, fixture(), nil)
	screen(t, noCoord, 80, 20)
	send(t, noCoord, key("1"), key("G"), key("enter"))
	if noCoord.Model.(ui.Model).BookmarksOpen() || !strings.Contains(noCoord.Model.View().Content, "Bookmarks unavailable: no coordinator") {
		t.Fatal("expected no-coordinator global bookmarks message")
	}
	send(t, noCoord, ui.OpenBookmarksMsg{Global: true}, key("e"))
	if !strings.Contains(noCoord.Model.View().Content, "Export unavailable: no coordinator") {
		t.Fatal("expected no-coordinator export message")
	}
}
