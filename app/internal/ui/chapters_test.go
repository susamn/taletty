package ui_test

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/ui"
)

func TestChapterBrowsingActivationAndTimelineTicks(t *testing.T) {
	data := fixture()
	data.Books[0].Chapters = []string{"Ch 1", "Ch 2", "Ch 3", "Ch 4", "Ch 5"}
	data.Books[0].ChapterStarts = []time.Duration{0, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 75 * time.Minute}
	data.Player.ChapterTicks = []time.Duration{0, 10 * time.Minute, 30 * time.Minute, 60 * time.Minute}

	var actions []ui.Action
	r := start(t, data, func(action ui.Action) tea.Cmd {
		actions = append(actions, action)
		return nil
	})
	view := screen(t, r, 144, 42)
	if !strings.Contains(view, "│") {
		t.Fatalf("expected Unicode chapter ticks on timeline:\n%s", view)
	}
	send(t, r, ui.AppearanceMsg{Glyphs: ui.Plain})
	view = screen(t, r, 80, 20)
	if !strings.Contains(view, "|") {
		t.Fatalf("expected Plain chapter ticks on timeline:\n%s", view)
	}

	// Focus Chapters pane (3) and navigate with j/k/g/G/pgup/pgdown/ctrl+u/ctrl+d.
	send(t, r, key("3"))
	_ = screen(t, r, 144, 42)
	data.Books[0].HasDuration = true
	data.Books[0].Duration = 50 * time.Minute
	send(t, r, data)
	_ = screen(t, r, 144, 42)
	send(t, r, key("j"), key("down"), key("k"), key("up"), key("G"), key("g"), key("pgdown"), key("ctrl+d"), key("ctrl+u"), key("pgup"), key("end"), key("home"), key("j"))
	send(t, r, key("enter"))
	if len(actions) != 1 || actions[0].Kind != ui.ActivateChapter || actions[0].Key != "A" || actions[0].ChapterIndex != 1 || actions[0].ChapterStart != 10*time.Minute {
		t.Fatalf("unexpected ActivateChapter action: %+v", actions)
	}

	// BookChaptersMsg updates chapters for a specific book without changing selection or audio.
	send(t, r, ui.BookChaptersMsg{
		Key:      "B",
		Chapters: []string{"B1", "B2"},
		Starts:   []time.Duration{0, 5 * time.Minute},
	})
	send(t, r, ui.BookChaptersMsg{Key: ""})
	send(t, r, key("2"), key("j")) // Select B; chapterCursor resets to 0.
	send(t, r, key("3"), key("j"), key("enter"))
	if len(actions) != 2 || actions[1].Key != "B" || actions[1].ChapterIndex != 1 || actions[1].ChapterStart != 5*time.Minute {
		t.Fatalf("unexpected B ActivateChapter action: %+v", actions)
	}

	// BookChaptersMsg on currently selected book clamps chapterCursor cleanly.
	send(t, r, ui.BookChaptersMsg{
		Key:      "B",
		Chapters: []string{"Only One"},
		Starts:   []time.Duration{0},
	})
	send(t, r, key("enter"))
	if len(actions) != 3 || actions[2].ChapterIndex != 0 {
		t.Fatalf("expected clamped chapterIndex 0, got %+v", actions)
	}

	// Empty chapters on Enter shows activity without dispatching.
	send(t, r, ui.BookChaptersMsg{Key: "B", Chapters: nil, Starts: nil})
	send(t, r, key("enter"))
	if len(actions) != 3 || !strings.Contains(r.Model.View().Content, "No chapters available") {
		t.Fatalf("empty chapters Enter leaked action or missed message: %+v", actions)
	}

	// Empty library Enter in Chapters is a safe no-op.
	send(t, r, ui.SnapshotMsg{}, key("3"), key("enter"))
	if len(actions) != 3 {
		t.Fatalf("empty library Chapters Enter dispatched action: %+v", actions)
	}
}
