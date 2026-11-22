package ui_test

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/ui"
)

func TestSleepTimerMenuOwnershipChapterAvailabilityAndNarrowPlayer(t *testing.T) {
	var actions []ui.Action
	data := fixture()
	data.Player.ChapterEnd = 20 * time.Minute
	r := start(t, data, func(a ui.Action) tea.Cmd {
		actions = append(actions, a)
		return nil
	})
	screen(t, r, 80, 20)

	// Open Sleep Timer menu via 't'; verify target, notice, and key ownership.
	send(t, r, key("t"))
	if !r.Model.(ui.Model).SleepOpen() {
		t.Fatal("expected SleepOpen true")
	}
	view := screen(t, r, 80, 20)
	if !strings.Contains(view, "[Focus] Sleep Timer") || !strings.Contains(view, "Target: River") || !strings.Contains(view, "Cancels on book switch or quit") {
		t.Fatalf("missing Sleep Timer header/notice:\n%s", view)
	}
	for _, blocked := range []string{" ", "q", "ctrl+c", "1", "5", "b", "B", "f", "?"} {
		send(t, r, key(blocked))
	}
	if len(actions) != 0 || !r.Model.(ui.Model).SleepOpen() {
		t.Fatalf("Sleep Timer menu leaked blocked keys: %+v", actions)
	}
	send(t, r, key("esc"))
	if r.Model.(ui.Model).SleepOpen() {
		t.Fatal("Esc did not close Sleep Timer menu")
	}

	// Arm 30 minutes (index 1).
	send(t, r, key("t"), key("j"), key("enter"))
	if len(actions) != 1 || actions[0] != (ui.Action{
		Kind:          ui.SetSleepTimer,
		Key:           "A",
		Session:       "session-a",
		SleepMode:     ui.SleepModeDuration,
		SleepDuration: 30 * time.Minute,
	}) {
		t.Fatalf("unexpected 30m sleep action: %+v", actions)
	}

	// Arm End of current chapter (index 4) when chapter metadata is present.
	send(t, r, key("t"), key("k"), key("k"), key("enter"))
	if len(actions) != 2 || actions[1].SleepMode != ui.SleepModeChapter {
		t.Fatalf("unexpected chapter-end sleep action: %+v", actions)
	}

	// Turn off (index 5).
	send(t, r, key("t"), key("k"), key("enter"))
	if len(actions) != 3 || actions[2].SleepMode != ui.SleepModeOff {
		t.Fatalf("unexpected sleep-off action: %+v", actions)
	}

	// Chapter end unavailable without chapter metadata.
	noCh := fixture()
	noCh.Player.Chapter = ""
	noCh.Player.ChapterEnd = 0
	send(t, r, noCh, key("t"), key("k"), key("k"))
	view = screen(t, r, 80, 20)
	if !strings.Contains(view, "unavailable: no chapter metadata") {
		t.Fatalf("expected unavailable chapter-end label:\n%s", view)
	}
	send(t, r, key("enter"))
	if len(actions) != 3 || !strings.Contains(r.Model.View().Content, "Chapter end unavailable") {
		t.Fatalf("unavailable chapter-end dispatched action: %+v", actions)
	}
	send(t, r, key("esc"))

	// Narrow 80x20 player clips long sleep chapter label cleanly without overflow.
	longSleep := fixture()
	longSleep.Player.Sleep = "Ch 02 Very Long Chapter Name That Must Clip Cleanly"
	send(t, r, longSleep)
	view = screen(t, r, 80, 20)
	if !strings.Contains(view, "Sleep Ch 02") {
		t.Fatalf("narrow player lost Sleep indicator:\n%s", view)
	}

	// Unavailable when no loaded audio or no coordinator.
	stopped := fixture()
	stopped.Player.State = ui.Stopped
	send(t, r, stopped, key("t"))
	if r.Model.(ui.Model).SleepOpen() || !strings.Contains(r.Model.View().Content, "Sleep timer unavailable: no loaded audio") {
		t.Fatal("t should be unavailable when stopped")
	}
	noCoord := start(t, fixture(), nil)
	screen(t, noCoord, 80, 20)
	send(t, noCoord, key("t"))
	if noCoord.Model.(ui.Model).SleepOpen() || !strings.Contains(noCoord.Model.View().Content, "Sleep timer unavailable: no coordinator") {
		t.Fatal("t should be unavailable without coordinator")
	}
}
