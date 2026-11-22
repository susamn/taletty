package ui_test

import (
	"math"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/ui"
)

func TestSpeedAdjustedBookAndChapterRemainingTime(t *testing.T) {
	data := fixture()
	data.Player = ui.Player{
		Key:         "A",
		Session:     "s1",
		Title:       "River",
		Chapter:     "Ch 1",
		State:       ui.Playing,
		Position:    10 * time.Minute,
		Duration:    70 * time.Minute,
		ChapterEnd:  25 * time.Minute,
		Speed:       1.5,
		Volume:      80,
		VolumeKnown: true,
	}
	r := start(t, data, nil)

	// Wide 144x42 shows both speed-adjusted book remaining (60m/1.5 = 40m) and chapter remaining (15m/1.5 = 10m).
	view := screen(t, r, 144, 42)
	if !strings.Contains(view, "Remaining 00:40:00 at speed | Chapter left 00:10:00") {
		t.Fatalf("missing speed-adjusted book/chapter remaining:\n%s", view)
	}

	// Compact 80x20 hides secondary remaining time while keeping core identity and controls.
	compact := screen(t, r, 80, 20)
	if strings.Contains(compact, "Remaining") || !strings.Contains(compact, "River | Ch 1") {
		t.Fatalf("compact player should hide secondary remaining time:\n%s", compact)
	}

	// Unknown book duration with known chapter end, and unknown chapter end with known book duration.
	data.Player.Duration = 0
	send(t, r, data)
	view = screen(t, r, 144, 42)
	if !strings.Contains(view, "Remaining unknown | Chapter left 00:10:00") {
		t.Fatalf("expected unknown book + known chapter remaining:\n%s", view)
	}

	data.Player.Duration = 70 * time.Minute
	data.Player.ChapterEnd = 0
	send(t, r, data)
	view = screen(t, r, 144, 42)
	if !strings.Contains(view, "Remaining 00:40:00 at speed | Chapter left unknown") {
		t.Fatalf("expected known book + unknown chapter remaining:\n%s", view)
	}

	// Invalid/infinite/tiny speed never overflows or panics.
	data.Player.Speed = math.Inf(1)
	send(t, r, data)
	view = screen(t, r, 144, 42)
	if !strings.Contains(view, "Remaining unknown | Chapter left unknown") {
		t.Fatalf("expected unknown remaining for Inf speed:\n%s", view)
	}
	data.Player.Speed = 1e-15
	send(t, r, data)
	view = screen(t, r, 144, 42)
	if !strings.Contains(view, "Remaining unknown") {
		t.Fatalf("expected overflow-safe unknown remaining:\n%s", view)
	}
}

func TestBookContextActionMenuCompletionReplayAndStaleInvalidation(t *testing.T) {
	var actions []ui.Action
	r := start(t, fixture(), func(a ui.Action) tea.Cmd {
		actions = append(actions, a)
		return nil
	})
	screen(t, r, 144, 42)

	// Unfinished book A -> x opens Book Actions with Mark as finished + Stop and delete audio.
	send(t, r, key("x"))
	view := screen(t, r, 144, 42)
	if !strings.Contains(view, "[Focus] Book Actions") || !strings.Contains(view, "Mark as finished") || !strings.Contains(view, "Stop and delete audio") {
		t.Fatalf("unexpected Book Actions menu:\n%s", view)
	}
	// Blocked keys stay inside menu.
	for _, blocked := range []string{" ", "q", "ctrl+c", "1", "5", "b", "B", "?"} {
		send(t, r, key(blocked))
	}
	if len(actions) != 0 {
		t.Fatalf("Book Actions menu leaked blocked keys: %+v", actions)
	}
	send(t, r, key("j"), key("k"), key("tab"), key("shift+tab"), key("enter"))
	if len(actions) != 1 || actions[0] != (ui.Action{Kind: ui.MarkFinished, Key: "A"}) {
		t.Fatalf("expected MarkFinished for A, got %+v", actions)
	}

	// Finished book A -> x offers Mark as unfinished and Replay from start confirmation.
	fin := fixture()
	fin.Books[0].Finished = true
	send(t, r, fin, key("x"))
	view = screen(t, r, 80, 20)
	if !strings.Contains(view, "Mark as unfinished") || !strings.Contains(view, "Replay from start") {
		t.Fatalf("missing finished book actions:\n%s", view)
	}
	send(t, r, key("j"), key("enter")) // Replay from start -> opens default-safe Cancel confirmation
	view = screen(t, r, 80, 20)
	if !strings.Contains(view, "[Focus] Replay finished book") || !strings.Contains(view, "[Cancel]") {
		t.Fatalf("missing Replay confirmation:\n%s", view)
	}
	send(t, r, key("tab"), key("enter"))
	if len(actions) != 2 || actions[1].Kind != ui.ReplayBook || actions[1].Key != "A" {
		t.Fatalf("expected ReplayBook for A, got %+v", actions)
	}
	send(t, r, ui.ModalResultMsg{ID: actions[1].RequestID, Attempt: actions[1].Attempt})

	// Stale-target invalidation: book state changes behind open Book Actions menu.
	send(t, r, key("x"))
	unfin := fixture()
	unfin.Books[0].Finished = false
	send(t, r, unfin, key("enter"))
	if len(actions) != 2 || !strings.Contains(r.Model.View().Content, "Book state changed; reopen actions") {
		t.Fatalf("stale Book Actions menu executed on changed state: %+v", actions)
	}

	// Mark as unfinished action.
	send(t, r, fin, key("x"), key("enter"))
	if len(actions) != 3 || actions[2] != (ui.Action{Kind: ui.MarkUnfinished, Key: "A"}) {
		t.Fatalf("expected MarkUnfinished for A, got %+v", actions)
	}
}
