package ui_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/ui"
)

func downloadsFixture() ui.DownloadsMsg {
	total := int64(100 << 20)
	return ui.DownloadsMsg{
		Jobs: []ui.DownloadJob{
			{
				ID:            "j-active",
				BookKey:       "A",
				Title:         "River",
				State:         ui.Downloading,
				Stage:         "downloading",
				BytesReceived: 40 << 20,
				BytesTotal:    &total,
				SpeedBPS:      2 << 20,
			},
			{
				ID:            "j-wait",
				BookKey:       "B",
				Title:         "Stars",
				State:         ui.Queued,
				QueuePosition: 1,
			},
			{
				ID:      "j-ready",
				BookKey: "A",
				Title:   "River",
				State:   ui.Ready,
			},
			{
				ID:      "j-fail",
				BookKey: "B",
				Title:   "Stars",
				State:   ui.Failed,
				Error:   "Disk full\x1b[2J",
			},
		},
	}
}

func TestDownloadsViewProgressStagesAndKeyOwnership(t *testing.T) {
	var actions []ui.Action
	r := start(t, fixture(), func(a ui.Action) tea.Cmd {
		actions = append(actions, a)
		return nil
	})
	screen(t, r, 144, 42)

	// 'd' in Books dispatches QueueDownload for highlighted book A.
	send(t, r, key("d"))
	if len(actions) != 1 || actions[0].Kind != ui.QueueDownload || actions[0].Key != "A" {
		t.Fatalf("expected QueueDownload for A, got %+v", actions)
	}

	// Open Downloads view via 'D' and feed DownloadsMsg.
	send(t, r, key("D"), downloadsFixture())
	if !r.Model.(ui.Model).DownloadsOpen() {
		t.Fatal("expected DownloadsOpen true")
	}
	view := screen(t, r, 144, 42)
	for _, want := range []string{"[Focus] Downloads", "40.0 MiB / 100.0 MiB (40%)", "2.0 MiB/s", "#1", "Disk full"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q in Downloads view:\n%s", want, view)
		}
	}
	if strings.Contains(r.Model.View().Content, "\x1b[2J") {
		t.Fatal("Downloads view leaked ANSI escape sequence")
	}

	// Unknown total must NOT show percentage; Preparing must show indeterminate stage.
	send(t, r, ui.DownloadsMsg{
		Jobs: []ui.DownloadJob{
			{ID: "j-unk", BookKey: "A", Title: "Unknown Total", State: ui.Downloading, Stage: "downloading", BytesReceived: 5 << 20, BytesTotal: nil, Canceling: true},
			{ID: "j-prep", BookKey: "B", Title: "Prep Book", State: ui.Preparing, Stage: "preparing"},
			{ID: "j-lic", BookKey: "B", Title: "Lic Book", State: ui.Downloading, Stage: "licensing"},
			{ID: "j-pub", BookKey: "B", Title: "Pub Book", State: ui.Preparing, Stage: "publishing"},
		},
	})
	view = screen(t, r, 80, 20)
	for _, want := range []string{"5.0 MiB / unknown total", "Canceling…", "Preparing (indeterminate)", "Licensing", "Publishing"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q in compact Downloads view:\n%s", want, view)
		}
	}
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "Unknown Total") && strings.Contains(line, "%") {
			t.Fatalf("unknown total invented percentage: %q", line)
		}
	}

	// Read-only Downloads list permits Space/,/. transport, blocks Library-only d/s/f.
	before := len(actions)
	send(t, r, key(" "), key(","), key("."), key("d"), key("s"), key("f"), key("-"), key("="))
	if len(actions) != before+3 || actions[before].Kind != ui.TogglePlayback || actions[before+1].Kind != ui.SeekBackward || actions[before+2].Kind != ui.SeekForward {
		t.Fatalf("unexpected transport/blocked actions in Downloads: %+v", actions[before:])
	}

	// Esc closes Downloads and restores originating Books focus and selection.
	send(t, r, key("esc"))
	if r.Model.(ui.Model).DownloadsOpen() || r.Model.(ui.Model).Focus() != ui.Books {
		t.Fatalf("Esc did not restore origin: open=%v focus=%v", r.Model.(ui.Model).DownloadsOpen(), r.Model.(ui.Model).Focus())
	}

	// Numeric focus 3 from Downloads closes Downloads and focuses Details.
	send(t, r, key("D"), key("3"))
	if r.Model.(ui.Model).DownloadsOpen() || r.Model.(ui.Model).Focus() != ui.Details {
		t.Fatalf("numeric focus from Downloads failed: open=%v focus=%v", r.Model.(ui.Model).DownloadsOpen(), r.Model.(ui.Model).Focus())
	}
}

func TestDownloadsRowActionsFrozenTargetAndSafeAudioDeletion(t *testing.T) {
	var actions []ui.Action
	r := start(t, fixture(), func(a ui.Action) tea.Cmd {
		actions = append(actions, a)
		return nil
	})
	screen(t, r, 144, 42)

	// Open Downloads with active, queued, ready, and failed jobs.
	send(t, r, key("D"), downloadsFixture())

	// Active job -> Enter opens menu -> Cancel download opens default-safe confirmation.
	send(t, r, key("enter"))
	if !r.Model.(ui.Model).DownloadMenuOpen() {
		t.Fatal("expected DownloadMenuOpen true")
	}
	// Menu blocks transport keys.
	before := len(actions)
	send(t, r, key(" "), key("q"))
	if len(actions) != before {
		t.Fatal("download action menu leaked transport/quit")
	}
	send(t, r, key("enter")) // opens Cancel download confirmation
	view := screen(t, r, 80, 20)
	if !strings.Contains(view, "Cancel download") || !strings.Contains(view, "[Keep downloading]") {
		t.Fatalf("expected default-safe cancel confirmation:\n%s", view)
	}
	send(t, r, key("tab"), key("enter")) // confirm Cancel download
	if len(actions) != before+1 || actions[before].Kind != ui.DownloadCancel || actions[before].JobID != "j-active" {
		t.Fatalf("expected DownloadCancel for j-active, got %+v", actions[before:])
	}
	send(t, r, ui.ModalResultMsg{ID: actions[before].RequestID, Attempt: actions[before].Attempt})

	// Queued job -> Move up, Move down, Remove from queue.
	send(t, r, key("j"), key("enter"), key("enter")) // Move up
	send(t, r, key("enter"), key("j"), key("enter")) // Move down
	send(t, r, key("enter"), key("k"), key("enter")) // Remove from queue
	if actions[len(actions)-3].Kind != ui.DownloadMoveUp || actions[len(actions)-2].Kind != ui.DownloadMoveDown || actions[len(actions)-1].Kind != ui.DownloadRemove {
		t.Fatalf("unexpected queued actions: %+v", actions)
	}

	// Stale target invalidation: open menu on j-fail, then job disappears before Enter.
	send(t, r, key("G"), key("enter"))
	send(t, r, ui.DownloadsMsg{Jobs: downloadsFixture().Jobs[:3]})
	before = len(actions)
	send(t, r, key("enter"))
	if len(actions) != before || !strings.Contains(r.Model.View().Content, "Download state changed") {
		t.Fatalf("stale download menu acted on wrong job: %+v", actions[before:])
	}

	// Ready job -> Delete downloaded audio for currently playing book A labels "Stop and delete audio".
	send(t, r, key("G"), key("enter"), key("j"), key("enter"))
	view = screen(t, r, 80, 20)
	if !strings.Contains(view, "[Keep audio]") || !strings.Contains(view, "Stop and delete audio") {
		t.Fatalf("expected Stop and delete audio confirmation for playing book:\n%s", view)
	}
	send(t, r, key("esc")) // cancel deletion

	// Ready job -> Show in Library closes Downloads and selects book A.
	send(t, r, key("enter"), key("enter"))
	if r.Model.(ui.Model).DownloadsOpen() || r.Model.(ui.Model).Selection() != "A" {
		t.Fatalf("Show in Library failed: open=%v sel=%q", r.Model.(ui.Model).DownloadsOpen(), r.Model.(ui.Model).Selection())
	}

	// 'x' in Books on Ready book A opens audio deletion confirmation; on Queued book B shows unavailable.
	send(t, r, key("x"))
	if !strings.Contains(r.Model.View().Content, "Delete downloaded audio") {
		t.Fatal("expected 'x' on Ready book to open delete confirmation")
	}
	send(t, r, key("esc"), key("j"), key("x"))
	if !strings.Contains(r.Model.View().Content, "No downloaded audio to delete") {
		t.Fatal("expected unavailable message for non-Ready book")
	}

	// Empty Downloads list and formatBytes coverage.
	send(t, r, key("D"), ui.DownloadsMsg{Jobs: nil})
	view = screen(t, r, 80, 20)
	if !strings.Contains(view, "No download jobs") {
		t.Fatalf("expected empty Downloads message:\n%s", view)
	}
	send(t, r, key("enter")) // safe no-op on empty list
	send(t, r, ui.DownloadsMsg{Jobs: []ui.DownloadJob{
		{ID: "j-b", BookKey: "A", Title: "Bytes", State: ui.Downloading, BytesReceived: 512},
		{ID: "j-k", BookKey: "A", Title: "KiB", State: ui.Downloading, BytesReceived: 2048},
		{ID: "j-g", BookKey: "A", Title: "GiB", State: ui.Downloading, BytesReceived: 2 << 30},
	}})
	view = screen(t, r, 144, 42)
	for _, want := range []string{"512 B", "2.0 KiB", "2.0 GiB"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing formatted size %q:\n%s", want, view)
		}
	}
}
