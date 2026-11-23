package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/playback"
	"github.com/susamn/taletty/app/internal/profile"
	"github.com/susamn/taletty/app/internal/store"
	"github.com/susamn/taletty/app/internal/testkit"
	"github.com/susamn/taletty/app/internal/ui"
)

func TestJ10SleepTimerDurationChapterEndPauseSeekSwitchAndRestart(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	ws, err := profile.Open(ctx, home, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	st := ws.Store()

	kA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B00ALPHA01"}
	kB := store.BookKey{Provider: "audible", Namespace: "us", ID: "B00BETA002"}
	durA := int64((60 * time.Minute).Nanoseconds())
	durB := int64((30 * time.Minute).Nanoseconds())
	if err := st.ReplaceCatalog(ctx, "audible", "us", []store.CatalogBook{
		{Key: kA, Title: "Alpha Book", DurationNS: &durA},
		{Key: kB, Title: "Beta Book", DurationNS: &durB},
	}, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	pathA := filepath.Join(ws.DownloadsPath(), "alpha.m4b")
	pathB := filepath.Join(ws.DownloadsPath(), "beta.m4b")
	for _, p := range []string{pathA, pathB} {
		if err := os.WriteFile(p, []byte("fake-m4b"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutLocalMedia(ctx, store.LocalMedia{Key: kA, RelativePath: "downloads/alpha.m4b", Format: "m4b", PublishedAtMS: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutLocalMedia(ctx, store.LocalMedia{Key: kB, RelativePath: "downloads/beta.m4b", Format: "m4b", PublishedAtMS: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutListeningState(ctx, store.ListeningState{
		Key: kA, PositionNS: int64((5 * time.Minute).Nanoseconds()), SpeedMilli: 1_500_000,
		Completion: store.CompletionUnfinished, UpdatedAtMS: now.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	backend := &fakePlaybackBackend{}
	player, err := playback.NewController(backend, playback.Options{
		Store: st,
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	chapters := fakeChapterReader{
		byPath: map[string][]Chapter{
			pathA: {
				{Title: "Ch 01", Start: 0, End: 25 * time.Minute},
				{Title: "Ch 02", Start: 25 * time.Minute, End: 60 * time.Minute},
			},
			pathB: {
				{Title: "Beta Ch 01", Start: 0, End: 30 * time.Minute},
			},
		},
	}
	coord, err := NewCoordinator(ctx, st, Options{
		Player:          player,
		MediaChecker:    NewRootMediaChecker(ws.Owner()),
		ChapterReader:   chapters,
		Clock:           func() time.Time { return now },
		WorkspaceCloser: ws.Close,
	})
	if err != nil {
		t.Fatal(err)
	}

	r, err := testkit.New(ctx, ui.NewModel(coord.InitialSnapshot(), coord.Dispatch), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range coord.InitialMessages() {
		if err := r.Send(msg); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Send(tea.WindowSizeMsg{Width: 80, Height: 20}); err != nil {
		t.Fatal(err)
	}

	// 1. Play Alpha at 1.5x speed, arm 15m sleep timer via 't' -> Enter.
	sendUIKeys(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "t", "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Sleep 15m") {
		t.Fatalf("expected Sleep 15m in narrow player:\n%s", r.Model.View().Content)
	}

	// 2. Advance fake clock 5m while playing -> 10m remaining (real wall time, not 1.5x speed-scaled).
	now = now.Add(5 * time.Minute)
	snapMsg, ok := coord.PollPlayback(ctx)
	if !ok || snapMsg.Player.Sleep != "10m" {
		t.Fatalf("expected 10m remaining after 5m playing, got ok=%v sleep=%q", ok, snapMsg.Player.Sleep)
	}
	if err := r.Send(snapMsg); err != nil {
		t.Fatal(err)
	}

	// 3. Pause freezes duration timer even across 20m elapsed!
	sendUIKeys(t, r, " ")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(20 * time.Minute)
	snapMsg, _ = coord.PollPlayback(ctx)
	if snapMsg.Player.Sleep != "10m" {
		t.Fatalf("pause did not freeze duration timer: sleep=%q", snapMsg.Player.Sleep)
	}

	// 4. Resume, same-book seek survives duration timer, advance 9m30s -> 30s, then 30s -> expires ONCE!
	sendUIKeys(t, r, " ", ".")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(9*time.Minute + 30*time.Second)
	snapMsg, _ = coord.PollPlayback(ctx)
	if snapMsg.Player.Sleep != "30s" {
		t.Fatalf("expected 30s remaining after same-book seek, got %q", snapMsg.Player.Sleep)
	}
	now = now.Add(30 * time.Second)
	snapMsg, _ = coord.PollPlayback(ctx)
	if snapMsg.Player.State != ui.Paused || snapMsg.Player.Sleep != "" || !strings.Contains(snapMsg.Activity, "Sleep timer expired") {
		t.Fatalf("duration timer did not pause once on expiry: %+v", snapMsg)
	}
	// Subsequent PollPlayback does not re-fire (even after resuming playback).
	now = now.Add(time.Minute)
	snapMsg2, _ := coord.PollPlayback(ctx)
	if snapMsg2.Player.Sleep != "" || snapMsg2.Player.State != ui.Paused {
		t.Fatalf("expired timer altered state on second poll: %+v", snapMsg2)
	}

	// 5. Resume, arm Chapter end (Ch 01), seek inside Ch 01 stays armed, seek into Ch 02 cancels!
	sendUIKeys(t, r, " ")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if err := r.Send(snapMsg); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "t", "k", "k", "enter") // End of current chapter
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Sleep Ch 01") {
		t.Fatalf("expected Sleep Ch 01 in player:\n%s", r.Model.View().Content)
	}
	// Seek +15s inside Ch 01 -> stays armed.
	sendUIKeys(t, r, ".")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Sleep Ch 01") {
		t.Fatalf("same-chapter seek canceled chapter-end timer:\n%s", r.Model.View().Content)
	}
	// Explicit chapter jump to Ch 02 -> cancels chapter-end timer!
	sendUIKeys(t, r, "3", "j", "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Model.View().Content, "Sleep Ch 01") || !strings.Contains(r.Model.View().Content, "Sleep Off") {
		t.Fatalf("cross-chapter seek did not cancel chapter-end timer:\n%s", r.Model.View().Content)
	}

	// 6. Arm Chapter end on Ch 02, advance playback to 60m -> expires once and pauses!
	sendUIKeys(t, r, "t", "k", "k", "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	backend.pos = 60 * time.Minute
	backend.mu.Unlock()
	snapMsg, _ = coord.PollPlayback(ctx)
	if snapMsg.Player.State != ui.Paused || snapMsg.Player.Sleep != "" || !strings.Contains(snapMsg.Activity, "Ch 02") {
		t.Fatalf("chapter-end timer did not pause at chapter end: %+v", snapMsg)
	}

	// 7. Arm 15m, switch to Beta -> cancels timer; re-arm on Beta, Turn off -> cancels; re-arm and Close -> restart has Sleep Off!
	sendUIKeys(t, r, " ")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "t", "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "2", "j", "enter") // Play Beta -> cancels Alpha timer
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Sleep Off") {
		t.Fatalf("book switch did not cancel sleep timer:\n%s", r.Model.View().Content)
	}
	sendUIKeys(t, r, "t", "enter") // arm 15m on Beta
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "t", "k", "enter") // Turn off
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Sleep Off") {
		t.Fatalf("Turn off did not clear sleep timer:\n%s", r.Model.View().Content)
	}
	sendUIKeys(t, r, "t", "enter") // arm 15m before Close
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if err := coord.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// Restart workspace -> no restored timer on launch!
	ws2, err := profile.Open(ctx, home, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	defer ws2.Close(context.Background())
	coord2, err := NewCoordinator(ctx, ws2.Store(), Options{Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer coord2.Close(context.Background())
	if coord2.InitialSnapshot().Player.Sleep != "" {
		t.Fatalf("sleep timer persisted across restart: %+v", coord2.InitialSnapshot().Player)
	}
}

func TestSleepTimerEdgeCasesStaleAndErrors(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	ws, err := profile.Open(ctx, home, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close(context.Background())
	st := ws.Store()

	kA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B00ALPHA01"}
	_ = st.ReplaceCatalog(ctx, "audible", "us", []store.CatalogBook{{Key: kA, Title: "Alpha"}}, now.UnixMilli())
	pathA := filepath.Join(ws.DownloadsPath(), "alpha.m4b")
	_ = os.WriteFile(pathA, []byte("fake-m4b"), 0o600)
	_ = st.PutLocalMedia(ctx, store.LocalMedia{Key: kA, RelativePath: "downloads/alpha.m4b", Format: "m4b", PublishedAtMS: now.UnixMilli()})
	encA, _ := EncodeBookKey(kA)

	// No player -> unavailable.
	noPlayerCoord, err := NewCoordinator(ctx, st, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer noPlayerCoord.Close(context.Background())
	if msg := noPlayerCoord.Dispatch(ui.Action{Kind: ui.SetSleepTimer, Key: encA, Session: "1", SleepMode: ui.SleepModeOff})(); !strings.Contains(msg.(ui.NoticeMsg).Error, "unavailable") {
		t.Fatalf("expected unavailable without player: %#v", msg)
	}

	backend := &fakePlaybackBackend{}
	player, _ := playback.NewController(backend, playback.Options{Store: st, Clock: func() time.Time { return now }})
	defer player.Close(context.Background())
	coord, err := NewCoordinator(ctx, st, Options{
		Player:       player,
		MediaChecker: NewRootMediaChecker(ws.Owner()),
		Clock:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	// Invalid session / stale session / invalid mode / invalid duration / missing chapters.
	if msg := coord.Dispatch(ui.Action{Kind: ui.SetSleepTimer, Key: encA, Session: "0", SleepMode: ui.SleepModeOff})(); !strings.Contains(msg.(ui.NoticeMsg).Error, "No active") {
		t.Fatalf("expected zero session error: %#v", msg)
	}
	if msg := coord.Dispatch(ui.Action{Kind: ui.SetSleepTimer, Key: encA, Session: "99", SleepMode: ui.SleepModeOff})(); !strings.Contains(msg.(ui.NoticeMsg).Error, "changed") {
		t.Fatalf("expected stale session error: %#v", msg)
	}
	_ = coord.Dispatch(ui.Action{Kind: ui.PlayBook, Key: encA})()
	sess := player.Snapshot().Session
	sessStr := "1"
	if sess != 1 {
		t.Fatalf("expected session 1, got %d", sess)
	}
	if msg := coord.Dispatch(ui.Action{Kind: ui.SetSleepTimer, Key: encA, Session: sessStr, SleepMode: ui.SleepModeDuration, SleepDuration: 0})(); !strings.Contains(msg.(ui.NoticeMsg).Error, "Invalid") {
		t.Fatalf("expected invalid duration error: %#v", msg)
	}
	if msg := coord.Dispatch(ui.Action{Kind: ui.SetSleepTimer, Key: encA, Session: sessStr, SleepMode: "bad"})(); !strings.Contains(msg.(ui.NoticeMsg).Error, "Invalid") {
		t.Fatalf("expected invalid mode error: %#v", msg)
	}
	if msg := coord.Dispatch(ui.Action{Kind: ui.SetSleepTimer, Key: encA, Session: sessStr, SleepMode: ui.SleepModeChapter})(); !strings.Contains(msg.(ui.NoticeMsg).Activity, "no chapter metadata") {
		t.Fatalf("expected missing chapter metadata notice: %#v", msg)
	}

	// Pause error on expiry clears timer cleanly and surfaces sanitized error.
	_ = coord.Dispatch(ui.Action{Kind: ui.SetSleepTimer, Key: encA, Session: sessStr, SleepMode: ui.SleepModeDuration, SleepDuration: time.Minute})()
	now = now.Add(2 * time.Minute)
	backend.mu.Lock()
	backend.pauseErr = errors.New("pause failed")
	backend.mu.Unlock()
	snapMsg, _ := coord.PollPlayback(ctx)
	if snapMsg.Player.Sleep != "" || snapMsg.Activity == "" {
		t.Fatalf("expected cleared timer and error activity on Pause failure: %+v", snapMsg)
	}
	backend.mu.Lock()
	backend.pauseErr = nil
	backend.mu.Unlock()

	// activeChapterBounds edge cases & formatSleepDuration.
	if _, _, _, ok := activeChapterBounds(nil, 0, 0); ok {
		t.Fatal("expected false for empty chapters")
	}
	if _, _, _, ok := activeChapterBounds([]Chapter{{Title: "C1", Start: 10 * time.Second}}, 5*time.Second, 0); ok {
		t.Fatal("expected false before first chapter")
	}
	if _, end, _, ok := activeChapterBounds([]Chapter{{Title: "C1", Start: 0}, {Title: "C2", Start: 10 * time.Minute}}, 2*time.Minute, 0); !ok || end != 10*time.Minute {
		t.Fatalf("expected next chapter start fallback, got ok=%v end=%v", ok, end)
	}
	if _, end, _, ok := activeChapterBounds([]Chapter{{Title: "C1", Start: 0}}, 2*time.Minute, 20*time.Minute); !ok || end != 20*time.Minute {
		t.Fatalf("expected book duration fallback, got ok=%v end=%v", ok, end)
	}
	if formatSleepDuration(0) != "0s" {
		t.Fatal("expected 0s")
	}

	// Closed coordinator rejects SetSleepTimer.
	_ = coord.Close(ctx)
	if msg := coord.Dispatch(ui.Action{Kind: ui.SetSleepTimer, Key: encA, Session: sessStr, SleepMode: ui.SleepModeOff})(); !strings.Contains(msg.(ui.NoticeMsg).Error, "closed") {
		t.Fatalf("expected closed error: %#v", msg)
	}
}
