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

func TestJ08J11CompletionSmartRewindReplayAndRemainingTime(t *testing.T) {
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
		Key: kA, PositionNS: int64((10 * time.Minute).Nanoseconds()), SpeedMilli: 1_500_000,
		Completion: store.CompletionUnfinished, UpdatedAtMS: now.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	backend := &fakePlaybackBackend{}
	policy := playback.DefaultSmartRewindPolicy()
	player, err := playback.NewController(backend, playback.Options{
		Store:       st,
		Clock:       func() time.Time { return now },
		SmartRewind: &policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	chapters := fakeChapterReader{
		byPath: map[string][]Chapter{
			pathA: {
				{Title: "Ch 1", Start: 0, End: 25 * time.Minute},
				{Title: "Ch 2", Start: 25 * time.Minute, End: 60 * time.Minute},
			},
			pathB: {
				{Title: "Beta Ch 1", Start: 0, End: 30 * time.Minute},
			},
		},
	}
	coord, err := NewCoordinator(ctx, st, Options{
		Player:          player,
		MediaChecker:    NewRootMediaChecker(ws.Owner()),
		ChapterReader:   chapters,
		SmartRewind:     &policy,
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
	if err := r.Send(tea.WindowSizeMsg{Width: 144, Height: 42}); err != nil {
		t.Fatal(err)
	}

	// 1. Play Alpha (10m / 60m, Ch 1 ends 25m, speed 1.5x) -> book left 50m/1.5 = 33m20s, chapter left 15m/1.5 = 10m.
	sendUIKeys(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	view := r.Model.View().Content
	if !strings.Contains(view, "Remaining 00:33:20 at speed | Chapter left 00:10:00") {
		t.Fatalf("missing speed-adjusted book/chapter remaining:\n%s", view)
	}

	// 2. Pause, advance 9s (< 10s threshold) -> Space resumes at exact 10m.
	sendUIKeys(t, r, " ")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(9 * time.Second)
	sendUIKeys(t, r, " ")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if player.Snapshot().Position != 10*time.Minute {
		t.Fatalf("expected no rewind below threshold, got %v", player.Snapshot().Position)
	}

	// 3. Pause, advance 15s (>= 10s threshold) -> Space rewinds 5s to 09m55s.
	sendUIKeys(t, r, " ")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Second)
	sendUIKeys(t, r, " ")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if player.Snapshot().Position != 9*time.Minute+55*time.Second {
		t.Fatalf("expected 5s smart rewind to 9m55s, got %v", player.Snapshot().Position)
	}

	// 4. Pause, advance 30s, deliberate seek (+15s) -> Space resumes at exact seeked position (NO smart rewind!).
	sendUIKeys(t, r, " ")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	sendUIKeys(t, r, ".")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	seekedPos := player.Snapshot().Position
	sendUIKeys(t, r, " ")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if player.Snapshot().Position != seekedPos {
		t.Fatalf("deliberate seek triggered smart rewind: got %v want %v", player.Snapshot().Position, seekedPos)
	}

	// 5. Natural EOF via PollPlayback marks Alpha Finished and updates Player + Books.
	backend.mu.Lock()
	backend.dur = 60 * time.Minute
	backend.eof = true
	backend.mu.Unlock()
	snapMsg, ok := coord.PollPlayback(ctx)
	if !ok {
		t.Fatal("expected PollPlayback snapshot on EOF")
	}
	if err := r.Send(snapMsg); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	backend.eof = false
	backend.mu.Unlock()
	if !strings.Contains(r.Model.View().Content, "Player - Finished") {
		t.Fatalf("expected Player - Finished after EOF:\n%s", r.Model.View().Content)
	}
	lsA, found, _ := st.ListeningState(ctx, kA)
	if !found || lsA.Completion != store.CompletionFinished {
		t.Fatalf("EOF did not persist CompletionFinished: found=%v %+v", found, lsA)
	}

	// 6. Manual Mark as unfinished on Beta (inactive) and Alpha (active) without interrupting audio.
	sendUIKeys(t, r, "j", "x", "enter") // Mark Beta finished
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	lsB, foundB, _ := st.ListeningState(ctx, kB)
	if !foundB || lsB.Completion != store.CompletionFinished {
		t.Fatalf("MarkFinished on Beta failed: found=%v %+v", foundB, lsB)
	}
	sendUIKeys(t, r, "x", "enter") // Mark Beta unfinished
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	lsB, _, _ = st.ListeningState(ctx, kB)
	if lsB.Completion != store.CompletionUnfinished {
		t.Fatalf("MarkUnfinished on Beta failed: %+v", lsB)
	}

	// 7. Enter on Finished Alpha opens Replay confirmation; Cancel keeps Finished; Confirm replays from 0 at 1.5x!
	sendUIKeys(t, r, "g", "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Replay finished book") || !strings.Contains(r.Model.View().Content, "[Cancel]") {
		t.Fatalf("Enter on finished book did not open Replay confirmation:\n%s", r.Model.View().Content)
	}
	sendUIKeys(t, r, "enter") // Cancel
	lsA, _, _ = st.ListeningState(ctx, kA)
	if lsA.Completion != store.CompletionFinished {
		t.Fatal("canceled Replay altered completion")
	}
	sendUIKeys(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "tab", "enter") // Replay from start
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	snap := player.Snapshot()
	if snap.State != playback.StatePlaying || snap.Position != 0 || snap.Completion != store.CompletionUnfinished || snap.SpeedMilli != 1_500_000 {
		t.Fatalf("Replay failed: %+v", snap)
	}

	// 8. Seek to 20m, close coordinator, advance clock 60s, reopen workspace -> PlayBook applies smart rewind!
	if _, err := player.SeekAbsolute(ctx, snap.Session, 20*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := coord.Close(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(60 * time.Second)

	ws2, err := profile.Open(ctx, home, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	defer ws2.Close(context.Background())
	backend2 := &fakePlaybackBackend{}
	player2, err := playback.NewController(backend2, playback.Options{
		Store:       ws2.Store(),
		Clock:       func() time.Time { return now },
		SmartRewind: &policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer player2.Close(context.Background())
	coord2, err := NewCoordinator(ctx, ws2.Store(), Options{
		Player:       player2,
		MediaChecker: NewRootMediaChecker(ws2.Owner()),
		SmartRewind:  &policy,
		Clock:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer coord2.Close(context.Background())

	encA, _ := EncodeBookKey(kA)
	_ = coord2.Dispatch(ui.Action{Kind: ui.PlayBook, Key: encA})()
	if got := player2.Snapshot().Position; got != 19*time.Minute+55*time.Second {
		t.Fatalf("restart PlayBook did not apply smart rewind: got %v", got)
	}
}

func TestCompletionCoordinatorEdgeCasesAndErrors(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	ws, err := profile.Open(ctx, home, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close(context.Background())
	st := ws.Store()

	kA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B00ALPHA01"}
	_ = st.ReplaceCatalog(ctx, "audible", "us", []store.CatalogBook{{Key: kA, Title: "Alpha"}}, 100)
	encA, _ := EncodeBookKey(kA)

	// Negative SmartRewind rejected in NewCoordinator and OpenEngineWorkspace.
	neg := playback.SmartRewindPolicy{Threshold: -time.Second}
	if _, err := NewCoordinator(ctx, st, Options{SmartRewind: &neg}); !errors.Is(err, playback.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for negative SmartRewind, got %v", err)
	}
	if _, err := OpenEngineWorkspace(ctx, t.TempDir(), "", Options{SmartRewind: &neg}); !errors.Is(err, playback.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput in OpenEngineWorkspace, got %v", err)
	}

	// Offline coordinator without Player can still MarkFinished / MarkUnfinished via store.
	_ = st.PutListeningState(ctx, store.ListeningState{
		Key: kA, PositionNS: 5000, SpeedMilli: 1_250_000, Completion: store.CompletionUnfinished, UpdatedAtMS: 100,
	})
	_ = st.PutViewState(ctx, store.ViewState{
		Destination: store.DestinationAll, Selected: &kA, Focus: store.FocusBooks, SortOrder: store.SortTitle,
	})
	noPlayerCoord, err := NewCoordinator(ctx, st, Options{})
	if err != nil {
		t.Fatal(err)
	}
	msg := noPlayerCoord.Dispatch(ui.Action{Kind: ui.MarkFinished, Key: encA})()
	if snapMsg, ok := msg.(ui.SnapshotMsg); !ok || !snapMsg.Books[0].Finished || snapMsg.Player.State != ui.Finished {
		t.Fatalf("offline MarkFinished failed: %#v", msg)
	}
	msg = noPlayerCoord.Dispatch(ui.Action{Kind: ui.MarkUnfinished, Key: encA})()
	if snapMsg, ok := msg.(ui.SnapshotMsg); !ok || snapMsg.Books[0].Finished || snapMsg.Player.State != ui.Stopped {
		t.Fatalf("offline MarkUnfinished failed: %#v", msg)
	}
	if msg := noPlayerCoord.Dispatch(ui.Action{Kind: ui.ReplayBook, Key: encA, RequestID: "r1", Attempt: 1})(); !strings.Contains(msg.(ui.ModalResultMsg).Error, "unavailable") {
		t.Fatalf("expected Replay unavailable without player: %#v", msg)
	}
	if msg := noPlayerCoord.Dispatch(ui.Action{Kind: ui.MarkFinished, Key: "bad"})(); !strings.Contains(msg.(ui.NoticeMsg).Error, "Invalid") {
		t.Fatalf("expected invalid key error: %#v", msg)
	}

	// ReplayBook missing media & invalid key & closed coordinator.
	player, _ := playback.NewController(&fakePlaybackBackend{}, playback.Options{Store: st})
	defer player.Close(context.Background())
	coord, err := NewCoordinator(ctx, st, Options{Player: player, MediaChecker: NewRootMediaChecker(ws.Owner())})
	if err != nil {
		t.Fatal(err)
	}
	if msg := coord.Dispatch(ui.Action{Kind: ui.ReplayBook, Key: "bad", RequestID: "r2", Attempt: 1})(); !strings.Contains(msg.(ui.ModalResultMsg).Error, "Invalid") {
		t.Fatalf("expected invalid key on ReplayBook: %#v", msg)
	}
	if msg := coord.Dispatch(ui.Action{Kind: ui.ReplayBook, Key: encA, RequestID: "r3", Attempt: 1})(); !strings.Contains(msg.(ui.ModalResultMsg).Error, "missing or unsafe") {
		t.Fatalf("expected missing media on ReplayBook: %#v", msg)
	}

	// projectActivePlayer chapterEnd fallback to next chapter start and snap.Duration.
	p := projectActivePlayer(playback.Snapshot{
		State: playback.StatePlaying, Session: 1, Book: &kA, Title: "Alpha",
		Position: 5 * time.Minute, Duration: 40 * time.Minute, SpeedMilli: playback.DefaultSpeedMilli,
	}, []Chapter{
		{Title: "Ch 1", Start: 0, End: 0},
		{Title: "Ch 2", Start: 15 * time.Minute, End: 0},
	})
	if p.ChapterEnd != 15*time.Minute {
		t.Fatalf("expected ChapterEnd fallback to next chapter start 15m, got %v", p.ChapterEnd)
	}
	pLast := projectActivePlayer(playback.Snapshot{
		State: playback.StatePlaying, Session: 1, Book: &kA, Title: "Alpha",
		Position: 20 * time.Minute, Duration: 40 * time.Minute, SpeedMilli: playback.DefaultSpeedMilli,
	}, []Chapter{
		{Title: "Ch 1", Start: 0, End: 0},
		{Title: "Ch 2", Start: 15 * time.Minute, End: 0},
	})
	if pLast.ChapterEnd != 40*time.Minute {
		t.Fatalf("expected ChapterEnd fallback to snap.Duration 40m, got %v", pLast.ChapterEnd)
	}

	_ = coord.Close(ctx)
	_ = noPlayerCoord.Close(ctx)
	for _, kind := range []ui.ActionKind{ui.ReplayBook, ui.MarkFinished, ui.MarkUnfinished} {
		if msg := coord.Dispatch(ui.Action{Kind: kind, Key: encA})(); msg == nil {
			t.Fatalf("expected closed response for %v", kind)
		}
	}
}
