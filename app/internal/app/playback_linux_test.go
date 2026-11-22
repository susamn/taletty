package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/playback"
	"github.com/susamn/taletty/app/internal/profile"
	"github.com/susamn/taletty/app/internal/store"
	"github.com/susamn/taletty/app/internal/testkit"
	"github.com/susamn/taletty/app/internal/ui"
)

type fakePlaybackBackend struct {
	mu       sync.Mutex
	started  bool
	path     string
	pos      time.Duration
	dur      time.Duration
	speed    int64
	volume   int
	paused   bool
	eof      bool
	startErr error
	seekErr  error
	posErr   error
	pauseErr error
	speedErr error
	volErr   error
	stopErr  error
	closeErr error
}

func (b *fakePlaybackBackend) Start(_ context.Context, mediaPath string, startPos time.Duration, speedMilli int64, volume int, paused bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.startErr != nil {
		return b.startErr
	}
	b.started = true
	b.path = mediaPath
	b.pos = startPos
	b.dur = 60 * time.Minute
	b.speed = speedMilli
	b.volume = volume
	b.paused = paused
	b.eof = false
	return nil
}

func (b *fakePlaybackBackend) SetPause(_ context.Context, paused bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pauseErr != nil {
		return b.pauseErr
	}
	b.paused = paused
	return nil
}

func (b *fakePlaybackBackend) SeekExact(_ context.Context, target time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seekErr != nil {
		return b.seekErr
	}
	b.pos = target
	return nil
}

func (b *fakePlaybackBackend) SetSpeed(_ context.Context, speedMilli int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.speedErr != nil {
		return b.speedErr
	}
	b.speed = speedMilli
	return nil
}

func (b *fakePlaybackBackend) SetVolume(_ context.Context, volume int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.volErr != nil {
		return b.volErr
	}
	b.volume = volume
	return nil
}

func (b *fakePlaybackBackend) Position(_ context.Context) (time.Duration, time.Duration, bool, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.posErr != nil {
		return 0, 0, false, false, b.posErr
	}
	return b.pos, b.dur, b.paused, b.eof, nil
}

func (b *fakePlaybackBackend) Stop(_ context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.started = false
	return b.stopErr
}

type fakeChapterReader struct {
	byPath map[string][]Chapter
	err    error
}

func (r fakeChapterReader) ReadChapters(_ context.Context, mediaPath string) ([]Chapter, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.byPath[mediaPath], nil
}

type fakeEnqueuer struct {
	keys []store.BookKey
	err  error
}

func (e *fakeEnqueuer) Enqueue(_ context.Context, key store.BookKey) error {
	if e.err != nil {
		return e.err
	}
	e.keys = append(e.keys, key)
	return nil
}

func sendUIKey(t *testing.T, r *testkit.Runner, name string) {
	t.Helper()
	var msg tea.KeyPressMsg
	switch name {
	case "enter":
		msg = tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		msg = tea.KeyPressMsg{Code: tea.KeyEscape}
	case "tab":
		msg = tea.KeyPressMsg{Code: tea.KeyTab}
	case " ":
		msg = tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	default:
		msg = tea.KeyPressMsg{Code: rune(name[0]), Text: name}
	}
	if err := r.Send(msg); err != nil {
		t.Fatal(err)
	}
}

func TestJ07J08PlaybackChapterInspectionAndCrossBookSwitch(t *testing.T) {
	home := t.TempDir()
	ws, err := profile.Open(context.Background(), home, 100)
	if err != nil {
		t.Fatal(err)
	}
	st := ws.Store()

	keyA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	keyB := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	keyC := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000003"}
	dur := int64(60 * time.Minute)
	if err := st.ReplaceCatalog(context.Background(), "audible", "us", []store.CatalogBook{
		{Key: keyA, Title: "Book Alpha", DurationNS: &dur},
		{Key: keyB, Title: "Book Beta", DurationNS: &dur},
		{Key: keyC, Title: "Book Gamma", DurationNS: &dur},
	}, 100); err != nil {
		t.Fatal(err)
	}

	pathA := filepath.Join(ws.DownloadsPath(), "alpha.m4b")
	pathB := filepath.Join(ws.DownloadsPath(), "beta.m4b")
	for _, p := range []string{pathA, pathB} {
		if err := os.WriteFile(p, []byte("synthetic-m4b"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutLocalMedia(context.Background(), store.LocalMedia{Key: keyA, RelativePath: "downloads/alpha.m4b", Format: "m4b", PublishedAtMS: 100}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutLocalMedia(context.Background(), store.LocalMedia{Key: keyB, RelativePath: "downloads/beta.m4b", Format: "m4b", PublishedAtMS: 100}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutListeningState(context.Background(), store.ListeningState{
		Key: keyA, PositionNS: int64(30 * time.Second), SpeedMilli: 1_250_000, Completion: store.CompletionUnfinished, UpdatedAtMS: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutListeningState(context.Background(), store.ListeningState{
		Key: keyB, PositionNS: int64(10 * time.Second), SpeedMilli: 1_500_000, Completion: store.CompletionUnfinished, UpdatedAtMS: 100,
	}); err != nil {
		t.Fatal(err)
	}

	backend := &fakePlaybackBackend{}
	vol := 80
	ctrl, err := playback.NewController(backend, playback.Options{Store: st, Volume: &vol})
	if err != nil {
		t.Fatal(err)
	}
	enq := &fakeEnqueuer{}
	chapters := fakeChapterReader{byPath: map[string][]Chapter{
		pathA: {
			{Title: "Alpha Ch 1", Start: 0, End: 10 * time.Minute},
			{Title: "Alpha Ch 2", Start: 10 * time.Minute, End: 20 * time.Minute},
		},
		pathB: {
			{Title: "Beta Ch 1", Start: 0, End: 15 * time.Minute},
			{Title: "Beta Ch 2", Start: 15 * time.Minute, End: 30 * time.Minute},
		},
	}}

	coord, err := NewCoordinator(context.Background(), st, Options{
		Player:          ctrl,
		MediaChecker:    NewRootMediaChecker(ws.Owner()),
		ChapterReader:   chapters,
		Downloads:       enq,
		WorkspaceCloser: ws.Close,
	})
	if err != nil {
		t.Fatal(err)
	}

	r, err := testkit.New(context.Background(), ui.NewModel(coord.InitialSnapshot(), coord.Dispatch), 300)
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
	for _, k := range coord.DownloadedBookKeys(context.Background()) {
		if msg, ok := coord.LoadChapters(context.Background(), k); ok {
			if err := r.Send(msg); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Play Book Alpha via Enter; verify saved 30s position and 1.25x speed.
	sendUIKey(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	snap := ctrl.Snapshot()
	if snap.State != playback.StatePlaying || *snap.Book != keyA || snap.Position != 30*time.Second || snap.SpeedMilli != 1_250_000 {
		t.Fatalf("unexpected Alpha playback snapshot: %+v", snap)
	}

	// Browse to Book Beta and inspect its chapters without replacing Alpha audio.
	sendUIKey(t, r, "j")
	sendUIKey(t, r, "4")
	view := r.Model.View().Content
	if !strings.Contains(view, "Chapters - Book Beta") || !strings.Contains(view, "Beta Ch 2") || !strings.Contains(view, "Book Alpha | Alpha Ch 1") {
		t.Fatalf("inspecting Beta chapters altered audio or missed chapters:\n%s", view)
	}

	// Pause Alpha, seek -15s/+15s while paused, step speed +/-0.1x, step volume +/-5%.
	sendUIKey(t, r, " ")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if ctrl.Snapshot().State != playback.StatePaused {
		t.Fatalf("expected paused state, got %v", ctrl.Snapshot().State)
	}
	sendUIKey(t, r, ",")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if ctrl.Snapshot().State != playback.StatePaused || ctrl.Snapshot().Position != 15*time.Second {
		t.Fatalf("paused seek -15s failed: %+v", ctrl.Snapshot())
	}
	sendUIKey(t, r, ".")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if ctrl.Snapshot().Position != 30*time.Second {
		t.Fatalf("paused seek +15s failed: %+v", ctrl.Snapshot())
	}
	sendUIKey(t, r, "[")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if ctrl.Snapshot().SpeedMilli != 1_150_000 {
		t.Fatalf("speed -0.1x failed: %+v", ctrl.Snapshot())
	}
	sendUIKey(t, r, "]")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if ctrl.Snapshot().SpeedMilli != 1_250_000 {
		t.Fatalf("speed +0.1x failed: %+v", ctrl.Snapshot())
	}
	sendUIKey(t, r, "-")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	sendUIKey(t, r, "=")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if ctrl.Snapshot().VolumePercent != 80 {
		t.Fatalf("volume step failed: %+v", ctrl.Snapshot())
	}

	// Same-book chapter jump on Alpha preserves paused state without confirmation.
	sendUIKey(t, r, "2")
	sendUIKey(t, r, "k")
	sendUIKey(t, r, "4")
	sendUIKey(t, r, "j")
	sendUIKey(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if ctrl.Snapshot().State != playback.StatePaused || ctrl.Snapshot().Position != 10*time.Minute {
		t.Fatalf("same-book paused chapter jump failed: %+v", ctrl.Snapshot())
	}

	// Cross-book chapter jump on Beta opens B3 confirmation; default Stay keeps Alpha.
	sendUIKey(t, r, "2")
	sendUIKey(t, r, "j")
	sendUIKey(t, r, "4")
	sendUIKey(t, r, "j")
	sendUIKey(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Switch audiobook") || !strings.Contains(r.Model.View().Content, "[Stay]") {
		t.Fatalf("expected B3 switch confirmation:\n%s", r.Model.View().Content)
	}
	sendUIKey(t, r, "enter") // Stay
	if *ctrl.Snapshot().Book != keyA {
		t.Fatal("Stay switched audio unexpectedly")
	}

	// Confirm Switch and play -> checkpoints Alpha, loads Beta at 1.5x, seeks to Beta Ch 2 (15m), plays.
	sendUIKey(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	sendUIKey(t, r, "tab")
	sendUIKey(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	snap = ctrl.Snapshot()
	if snap.State != playback.StatePlaying || *snap.Book != keyB || snap.Position != 15*time.Minute || snap.SpeedMilli != 1_500_000 {
		t.Fatalf("cross-book switch failed: %+v", snap)
	}
	alphaSaved, found, err := st.ListeningState(context.Background(), keyA)
	if err != nil || !found || alphaSaved.PositionNS != int64(10*time.Minute) {
		t.Fatalf("Alpha checkpoint not preserved on switch: %+v found=%v err=%v", alphaSaved, found, err)
	}

	// Missing audio on Book Gamma offers Queue download without touching Beta playback.
	sendUIKey(t, r, "2")
	sendUIKey(t, r, "j")
	sendUIKey(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Audio not downloaded") {
		t.Fatalf("expected missing-audio download prompt:\n%s", r.Model.View().Content)
	}
	sendUIKey(t, r, "tab")
	sendUIKey(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if len(enq.keys) != 1 || enq.keys[0] != keyC || *ctrl.Snapshot().Book != keyB {
		t.Fatalf("download enqueue failed or altered playback: enq=%+v snap=%+v", enq.keys, ctrl.Snapshot())
	}

	if err := coord.Close(context.Background()); err != nil {
		t.Fatalf("Coordinator.Close failed: %v", err)
	}
}

func TestPlaybackStaleSwitchUnsafeMediaAndPoll(t *testing.T) {
	home := t.TempDir()
	ws, err := profile.Open(context.Background(), home, 100)
	if err != nil {
		t.Fatal(err)
	}
	st := ws.Store()
	keyA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	keyB := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	if err := st.ReplaceCatalog(context.Background(), "audible", "us", []store.CatalogBook{
		{Key: keyA, Title: "Alpha"},
		{Key: keyB, Title: "Beta"},
	}, 100); err != nil {
		t.Fatal(err)
	}
	pathA := filepath.Join(ws.DownloadsPath(), "alpha.m4b")
	pathB := filepath.Join(ws.DownloadsPath(), "beta.m4b")
	_ = os.WriteFile(pathA, []byte("a"), 0o600)
	_ = os.WriteFile(pathB, []byte("b"), 0o644) // unsafe 0644 mode!
	_ = st.PutLocalMedia(context.Background(), store.LocalMedia{Key: keyA, RelativePath: "downloads/alpha.m4b", Format: "m4b", PublishedAtMS: 100})
	_ = st.PutLocalMedia(context.Background(), store.LocalMedia{Key: keyB, RelativePath: "downloads/beta.m4b", Format: "m4b", PublishedAtMS: 100})

	backend := &fakePlaybackBackend{}
	ctrl, err := playback.NewController(backend, playback.Options{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	coord, err := NewCoordinator(context.Background(), st, Options{
		Player:          ctrl,
		MediaChecker:    NewRootMediaChecker(ws.Owner()),
		ChapterReader:   fakeChapterReader{byPath: map[string][]Chapter{pathA: {{Title: "Ch 1", Start: 0}, {Title: "Ch 2", Start: 5 * time.Minute}}}},
		WorkspaceCloser: ws.Close,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = coord.Close(context.Background()) }()

	// ActivateChapter when idle loads and seeks directly.
	encA, _ := EncodeBookKey(keyA)
	encB, _ := EncodeBookKey(keyB)
	msg := coord.Dispatch(ui.Action{Kind: ui.ActivateChapter, Key: encA, ChapterIndex: 1, ChapterStart: 5 * time.Minute})()
	if _, ok := msg.(ui.SnapshotMsg); !ok || ctrl.Snapshot().Position != 5*time.Minute {
		t.Fatalf("idle ActivateChapter failed: msg=%T snap=%+v", msg, ctrl.Snapshot())
	}

	// PollPlayback updates position and chapter title.
	backend.mu.Lock()
	backend.pos = 6 * time.Minute
	backend.mu.Unlock()
	polled, ok := coord.PollPlayback(context.Background())
	if !ok || polled.Player.Position != 6*time.Minute || polled.Player.Chapter != "Ch 2" {
		t.Fatalf("PollPlayback failed: ok=%v player=%+v", ok, polled.Player)
	}

	// Unsafe 0644 Beta file is refused and offers download without touching Alpha.
	if msg := coord.Dispatch(ui.Action{Kind: ui.ActivateChapter, Key: encB, ChapterStart: 0})(); msg == nil {
		t.Fatal("expected missing/unsafe download prompt")
	} else if _, ok := msg.(ui.OpenConfirmationMsg); !ok {
		t.Fatalf("expected OpenConfirmationMsg for unsafe file, got %T", msg)
	}

	// Stale ConfirmSwitchPlay session is rejected.
	res := coord.Dispatch(ui.Action{Kind: ui.ConfirmSwitchPlay, Key: encB, Session: "999", RequestID: "sw-1", Attempt: 1})()
	if mr, ok := res.(ui.ModalResultMsg); !ok || !strings.Contains(mr.Error, "Playback changed") {
		t.Fatalf("expected stale switch rejection, got %#v", res)
	}

	// QueueDownload without Downloads coordinator returns safe ModalResultMsg error.
	qRes := coord.Dispatch(ui.Action{Kind: ui.QueueDownload, Key: encB, RequestID: "dl-1", Attempt: 1})()
	if mr, ok := qRes.(ui.ModalResultMsg); !ok || !strings.Contains(mr.Error, "Download manager unavailable") {
		t.Fatalf("expected unavailable download error, got %#v", qRes)
	}

	// Invalid session / nil player / error sanitization checks.
	for _, kind := range []ui.ActionKind{ui.TogglePlayback, ui.SeekBackward, ui.SpeedUp} {
		if n, ok := coord.Dispatch(ui.Action{Kind: kind, Session: "bad"})().(ui.NoticeMsg); !ok || n.Error == "" {
			t.Fatalf("expected error notice for invalid session on %s", kind)
		}
	}
	for _, err := range []error{nil, playback.ErrRuntimeMissing, playback.ErrIPC, playback.ErrStaleSession, playback.ErrNoActiveTrack, errors.New("raw")} {
		_ = sanitizePlaybackError(err)
	}
}

func TestFFprobeChapterParsingMediaCheckerAndSessionModel(t *testing.T) {
	// parseFFprobeChapters valid, blank title fallback, ANSI sanitization, sorting.
	raw := []byte(`{"chapters":[
		{"start_time":"60.0","end_time":"120.0","tags":{"title":"Second\u001b[2J"}},
		{"start_time":"0.0","end_time":"60.0","tags":{"title":""}},
		{"start_time":"-1.0","end_time":"5.0","tags":{"title":"bad"}}
	]}`)
	chs := parseFFprobeChapters(raw)
	if len(chs) != 2 || chs[0].Title != "Chapter 2" || chs[1].Title != "Second[2J" || chs[1].Start != 60*time.Second {
		t.Fatalf("unexpected parsed chapters: %+v", chs)
	}
	if parseFFprobeChapters([]byte("not-json")) != nil {
		t.Fatal("expected nil for invalid JSON")
	}
	var lb limitedBuffer
	lb.limit = 4
	if _, err := lb.Write([]byte("12345")); err == nil {
		t.Fatal("expected limitedBuffer overflow error")
	}

	// Missing ffprobe binary returns nil, nil cleanly.
	missingReader := FFprobeChapterReader{Binary: filepath.Join(t.TempDir(), "missing-ffprobe")}
	if got, err := missingReader.ReadChapters(context.Background(), "/tmp/x.m4b"); err != nil || got != nil {
		t.Fatalf("expected nil, nil for missing ffprobe, got %+v %v", got, err)
	}
	// Fake ffprobe executable exercises ReadChapters success, nonzero exit, and canceled ctx.
	fakeBin := filepath.Join(t.TempDir(), "ffprobe")
	fakeScript := "#!/bin/sh\nif [ \"$TALETTY_FAKE_FFPROBE\" = \"fail\" ]; then exit 1; fi\nprintf '%s\\n' '{\"chapters\":[{\"start_time\":\"0.0\",\"end_time\":\"10.0\",\"tags\":{\"title\":\"Fake Ch 1\"}}]}'\n"
	if err := os.WriteFile(fakeBin, []byte(fakeScript), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeReader := FFprobeChapterReader{Binary: fakeBin}
	if got, err := fakeReader.ReadChapters(context.Background(), "/tmp/x.m4b"); err != nil || len(got) != 1 || got[0].Title != "Fake Ch 1" {
		t.Fatalf("fake ffprobe ReadChapters failed: %+v %v", got, err)
	}
	t.Setenv("TALETTY_FAKE_FFPROBE", "fail")
	if got, err := fakeReader.ReadChapters(context.Background(), "/tmp/x.m4b"); err != nil || got != nil {
		t.Fatalf("expected nil, nil on ffprobe nonzero exit, got %+v %v", got, err)
	}
	canceledCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := fakeReader.ReadChapters(canceledCtx, "/tmp/x.m4b"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if _, err := fakeReader.ReadChapters(nil, "/tmp/x.m4b"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for nil ctx, got %v", err)
	}

	// rootMediaChecker adversarial checks.
	home := t.TempDir()
	ws, err := profile.Open(context.Background(), home, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close(context.Background()) }()
	checker := NewRootMediaChecker(ws.Owner())
	key := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	if _, err := checker.VerifyMedia(nil, store.LocalMedia{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for nil ctx, got %v", err)
	}
	if _, err := checker.VerifyMedia(canceledCtx, store.LocalMedia{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if _, err := checker.VerifyMedia(context.Background(), store.LocalMedia{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for invalid LocalMedia, got %v", err)
	}
	if _, err := checker.VerifyMedia(context.Background(), store.LocalMedia{Key: key, RelativePath: "downloads/missing.m4b", Format: "m4b"}); err == nil {
		t.Fatal("expected error for missing file")
	}
	hardTarget := filepath.Join(ws.DownloadsPath(), "a.m4b")
	hardLink := filepath.Join(ws.DownloadsPath(), "b.m4b")
	_ = os.WriteFile(hardTarget, []byte("x"), 0o600)
	_ = os.Link(hardTarget, hardLink)
	if _, err := checker.VerifyMedia(context.Background(), store.LocalMedia{Key: key, RelativePath: "downloads/b.m4b", Format: "m4b"}); err == nil {
		t.Fatal("expected error for hardlinked file")
	}
	subDir := filepath.Join(ws.DownloadsPath(), "sub")
	_ = os.Mkdir(subDir, 0o755)
	_ = os.WriteFile(filepath.Join(subDir, "c.m4b"), []byte("x"), 0o600)
	if _, err := checker.VerifyMedia(context.Background(), store.LocalMedia{Key: key, RelativePath: "downloads/sub/c.m4b", Format: "m4b"}); err == nil {
		t.Fatal("expected error for 0755 subdir")
	}
	symLink := filepath.Join(ws.DownloadsPath(), "sym.m4b")
	_ = os.Symlink(hardTarget, symLink)
	if _, err := checker.VerifyMedia(context.Background(), store.LocalMedia{Key: key, RelativePath: "downloads/sym.m4b", Format: "m4b"}); err == nil {
		t.Fatal("expected error for symlinked file")
	}
	// VerifyMedia with closed owner returns error cleanly.
	wsClosedHome := t.TempDir()
	wsClosed, _ := profile.Open(context.Background(), wsClosedHome, 100)
	closedChecker := NewRootMediaChecker(wsClosed.Owner())
	_ = wsClosed.Close(context.Background())
	if _, err := closedChecker.VerifyMedia(context.Background(), store.LocalMedia{Key: key, RelativePath: "downloads/a.m4b", Format: "m4b"}); err == nil {
		t.Fatal("expected error for closed owner")
	}

	// sessionModel Init (executing returned batch cmds) and playbackTickMsg polling.
	_ = ws.Store().ReplaceCatalog(context.Background(), "audible", "us", []store.CatalogBook{{Key: key, Title: "A"}}, 100)
	_ = ws.Store().PutLocalMedia(context.Background(), store.LocalMedia{Key: key, RelativePath: "downloads/a.m4b", Format: "m4b", PublishedAtMS: 100})
	backend := &fakePlaybackBackend{}
	ctrl, _ := playback.NewController(backend, playback.Options{Store: ws.Store()})
	coord, err := NewCoordinator(context.Background(), ws.Store(), Options{
		Player:        ctrl,
		MediaChecker:  checker,
		ChapterReader: fakeChapterReader{byPath: map[string][]Chapter{hardTarget: {{Title: "Ch 1", Start: 0}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = coord.Close(context.Background()) }()
	sm := sessionModel{inner: ui.NewModel(coord.Snapshot(), coord.Dispatch), ctx: context.Background(), coord: coord}
	initCmd := sm.Init()
	if initCmd == nil {
		t.Fatal("expected Init batch cmd")
	}
	if batch, ok := initCmd().(tea.BatchMsg); ok {
		for _, sub := range batch {
			if sub != nil {
				sm.Update(sub())
			}
		}
	}
	next, tickCmd := sm.Update(playbackTickMsg{})
	if tickCmd == nil || next.View().Content != "" {
		t.Fatalf("unexpected sessionModel tick result")
	}
}

func TestPlaybackErrorBranchesAndNilGuards(t *testing.T) {
	home := t.TempDir()
	ws, err := profile.Open(context.Background(), home, 100)
	if err != nil {
		t.Fatal(err)
	}
	st := ws.Store()
	keyA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	keyB := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	_ = st.ReplaceCatalog(context.Background(), "audible", "us", []store.CatalogBook{
		{Key: keyA, Title: "Alpha"},
		{Key: keyB, Title: "Beta"},
	}, 100)
	pathA := filepath.Join(ws.DownloadsPath(), "alpha.m4b")
	pathB := filepath.Join(ws.DownloadsPath(), "beta.m4b")
	_ = os.WriteFile(pathA, []byte("a"), 0o600)
	_ = os.WriteFile(pathB, []byte("b"), 0o600)
	_ = st.PutLocalMedia(context.Background(), store.LocalMedia{Key: keyA, RelativePath: "downloads/alpha.m4b", Format: "m4b", PublishedAtMS: 100})
	_ = st.PutLocalMedia(context.Background(), store.LocalMedia{Key: keyB, RelativePath: "downloads/beta.m4b", Format: "m4b", PublishedAtMS: 100})

	// Nil player guards.
	noPlayer, err := NewCoordinator(context.Background(), st, Options{})
	if err != nil {
		t.Fatal(err)
	}
	encA, _ := EncodeBookKey(keyA)
	encB, _ := EncodeBookKey(keyB)
	for _, action := range []ui.Action{
		{Kind: ui.PlayBook, Key: encA},
		{Kind: ui.ActivateChapter, Key: encA},
		{Kind: ui.ConfirmSwitchPlay, Key: encA, Session: "1"},
		{Kind: ui.TogglePlayback, Session: "1"},
		{Kind: ui.SeekBackward, Session: "1"},
		{Kind: ui.SpeedDown, Session: "1"},
		{Kind: ui.VolumeDown},
	} {
		_ = noPlayer.Dispatch(action)()
	}
	_ = noPlayer.Close(context.Background())

	// Backend start/seek errors, PutSettings error, Enqueue error, and projectActivePlayer states.
	backend := &fakePlaybackBackend{}
	ctrl, _ := playback.NewController(backend, playback.Options{Store: st})
	fs := &playbackFaultStore{CatalogStore: st}
	enq := &fakeEnqueuer{err: errors.New("queue full")}
	coord, err := NewCoordinator(context.Background(), fs, Options{
		Player:          ctrl,
		MediaChecker:    NewRootMediaChecker(ws.Owner()),
		ChapterReader:   fakeChapterReader{err: errors.New("probe err")},
		Downloads:       enq,
		WorkspaceCloser: ws.Close,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Invalid keys and backend Start error.
	_ = coord.Dispatch(ui.Action{Kind: ui.PlayBook, Key: "bad"})()
	_ = coord.Dispatch(ui.Action{Kind: ui.ActivateChapter, Key: "bad"})()
	_ = coord.Dispatch(ui.Action{Kind: ui.ConfirmSwitchPlay, Key: "bad", Session: "1"})()
	_ = coord.Dispatch(ui.Action{Kind: ui.QueueDownload, Key: "bad"})()
	_ = coord.Dispatch(ui.Action{Kind: ui.QueueDownload, Key: encA})()

	backend.startErr = playback.ErrIPC
	_ = coord.Dispatch(ui.Action{Kind: ui.PlayBook, Key: encA})()
	_ = coord.Dispatch(ui.Action{Kind: ui.ActivateChapter, Key: encA})()
	backend.startErr = nil

	// Load Alpha, then test SeekExact error on same-book chapter jump and ConfirmSwitchPlay.
	_ = coord.Dispatch(ui.Action{Kind: ui.PlayBook, Key: encA})()
	backend.seekErr = playback.ErrIPC
	_ = coord.Dispatch(ui.Action{Kind: ui.ActivateChapter, Key: encA, ChapterStart: time.Minute})()
	_ = coord.Dispatch(ui.Action{Kind: ui.ConfirmSwitchPlay, Key: encB, Session: "2", ChapterStart: time.Minute})()
	backend.seekErr = nil

	// PutSettings error on VolumeUp, plus backend errors on Toggle/Seek/Speed/Volume/Poll.
	fs.failPutSettings = true
	_ = coord.Dispatch(ui.Action{Kind: ui.VolumeUp})()
	fs.failPutSettings = false

	backend.pauseErr = playback.ErrIPC
	_ = coord.Dispatch(ui.Action{Kind: ui.TogglePlayback, Session: strconv.FormatUint(ctrl.Snapshot().Session, 10)})()
	backend.pauseErr = nil

	_ = coord.Dispatch(ui.Action{Kind: ui.PlayBook, Key: encA})()
	backend.posErr = playback.ErrIPC
	if _, ok := coord.PollPlayback(context.Background()); ok {
		t.Fatal("expected PollPlayback false on backend error")
	}
	backend.posErr = nil
	_ = coord.Dispatch(ui.Action{Kind: ui.PlayBook, Key: encA})()
	backend.posErr = playback.ErrIPC
	_ = coord.Dispatch(ui.Action{Kind: ui.SeekForward, Session: strconv.FormatUint(ctrl.Snapshot().Session, 10)})()
	backend.posErr = nil

	_ = coord.Dispatch(ui.Action{Kind: ui.PlayBook, Key: encA})()
	backend.speedErr = playback.ErrIPC
	_ = coord.Dispatch(ui.Action{Kind: ui.SpeedUp, Session: strconv.FormatUint(ctrl.Snapshot().Session, 10)})()
	backend.speedErr = nil

	_ = coord.Dispatch(ui.Action{Kind: ui.PlayBook, Key: encA})()
	backend.volErr = playback.ErrIPC
	_ = coord.Dispatch(ui.Action{Kind: ui.VolumeDown})()
	backend.volErr = nil

	// ConfirmSwitchPlay with missing media and backend Start error.
	_ = coord.Dispatch(ui.Action{Kind: ui.PlayBook, Key: encA})()
	_ = st.DeleteLocalMedia(context.Background(), keyB)
	if mr, ok := coord.Dispatch(ui.Action{Kind: ui.ConfirmSwitchPlay, Key: encB, Session: strconv.FormatUint(ctrl.Snapshot().Session, 10)})().(ui.ModalResultMsg); !ok || mr.Error == "" {
		t.Fatalf("expected missing media error on ConfirmSwitchPlay, got %#v", mr)
	}
	_ = st.PutLocalMedia(context.Background(), store.LocalMedia{Key: keyB, RelativePath: "downloads/beta.m4b", Format: "m4b", PublishedAtMS: 100})
	backend.startErr = playback.ErrIPC
	if mr, ok := coord.Dispatch(ui.Action{Kind: ui.ConfirmSwitchPlay, Key: encB, Session: strconv.FormatUint(ctrl.Snapshot().Session, 10)})().(ui.ModalResultMsg); !ok || mr.Error == "" {
		t.Fatalf("expected start error on ConfirmSwitchPlay, got %#v", mr)
	}
	backend.startErr = nil

	// LoadChapters, DownloadedBookKeys, splitChapters, handlePlaybackAction default, and PollPlayback nil guards.
	if _, ok := coord.LoadChapters(nil, keyA); ok {
		t.Fatal("expected false for nil ctx")
	}
	if _, ok := coord.LoadChapters(context.Background(), store.BookKey{}); ok {
		t.Fatal("expected false for invalid key")
	}
	if _, ok := coord.LoadChapters(context.Background(), store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000099"}); ok {
		t.Fatal("expected false for missing media")
	}
	if keys := (*Coordinator)(nil).DownloadedBookKeys(context.Background()); keys != nil {
		t.Fatal("expected nil for nil coordinator")
	}
	if t1, s1 := splitChapters(nil); t1 != nil || s1 != nil {
		t.Fatal("expected nil slices for empty chapters")
	}
	_ = coord.handlePlaybackAction(ui.Action{Kind: ui.ActionKind("unknown")})
	if _, ok := (*Coordinator)(nil).PollPlayback(context.Background()); ok {
		t.Fatal("expected false for nil coordinator PollPlayback")
	}

	// projectActivePlayer failed/stopped/nil states.
	_ = projectActivePlayer(playback.Snapshot{}, nil)
	_ = projectActivePlayer(playback.Snapshot{Book: &keyA, State: playback.StateFailed}, nil)
	_ = projectActivePlayer(playback.Snapshot{Book: &keyA, State: playback.StateStopped}, nil)

	// Sync while playing preserves active player and cached chapters.
	_ = coord.Dispatch(ui.Action{Kind: ui.PlayBook, Key: encA})()
	coord.mu.Lock()
	coord.chapters[keyA] = []Chapter{{Title: "Cached Ch", Start: 0}}
	coord.activeSyncID = "sync-keep"
	coord.mu.Unlock()
	_ = coord.ApplySyncResult(context.Background(), "sync-keep", []store.CatalogBook{{Key: keyA, Title: "Alpha"}})
	if len(coord.Snapshot().Books) != 1 || len(coord.Snapshot().Books[0].Chapters) != 1 || coord.Snapshot().Player.State != ui.Playing {
		t.Fatalf("sync did not preserve active player/chapters: %+v", coord.Snapshot())
	}

	// Closed coordinator rejects all playback actions cleanly.
	if err := coord.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, action := range []ui.Action{
		{Kind: ui.PlayBook, Key: encA},
		{Kind: ui.ActivateChapter, Key: encA},
		{Kind: ui.ConfirmSwitchPlay, Key: encA, Session: "1"},
		{Kind: ui.QueueDownload, Key: encA},
		{Kind: ui.TogglePlayback, Session: "1"},
		{Kind: ui.SeekForward, Session: "1"},
		{Kind: ui.SpeedUp, Session: "1"},
		{Kind: ui.VolumeUp},
	} {
		_ = coord.Dispatch(action)()
	}
}

type playbackFaultStore struct {
	CatalogStore
	failPutSettings bool
}

func (f *playbackFaultStore) PutSettings(ctx context.Context, s store.Settings) error {
	if f.failPutSettings {
		return errors.New("put settings failed")
	}
	return f.CatalogStore.PutSettings(ctx, s)
}

func TestRealGeneratedMediaPlaybackAndChapters(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe", "mpv"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("TALETTY_REQUIRE_MEDIA_TOOLS") == "1" {
				t.Fatalf("mandatory media tool %s missing: %v", tool, err)
			}
			t.Skipf("local media tool %s absent; mandatory in CI", tool)
		}
	}

	home := t.TempDir()
	ws, err := profile.Open(context.Background(), home, 100)
	if err != nil {
		t.Fatal(err)
	}
	st := ws.Store()
	key := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	dur := int64(4 * time.Second)
	if err := st.ReplaceCatalog(context.Background(), "audible", "us", []store.CatalogBook{
		{Key: key, Title: "Generated Audio", DurationNS: &dur},
	}, 100); err != nil {
		t.Fatal(err)
	}

	metaPath := filepath.Join(t.TempDir(), "chapters.txt")
	meta := ";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=2000\ntitle=Intro\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=2000\nEND=4000\ntitle=Part Two\n"
	if err := os.WriteFile(metaPath, []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	m4bPath := filepath.Join(ws.DownloadsPath(), "generated.m4b")
	genCtx, genCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer genCancel()
	genCmd := exec.CommandContext(genCtx, "ffmpeg",
		"-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "anullsrc=r=8000:cl=mono",
		"-i", metaPath,
		"-map_metadata", "1",
		"-t", "4",
		"-c:a", "aac",
		"-f", "mp4",
		"-y", m4bPath,
	)
	if out, err := genCmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg generate failed: %v\n%s", err, out)
	}
	if err := os.Chmod(m4bPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.PutLocalMedia(context.Background(), store.LocalMedia{
		Key: key, RelativePath: "downloads/generated.m4b", Format: "m4b", PublishedAtMS: 100,
	}); err != nil {
		t.Fatal(err)
	}

	ctrl, err := playback.NewController(playback.NewMPVProcess(playback.MPVConfig{
		ExtraArgs:    []string{"--ao=null"},
		StartTimeout: 10 * time.Second,
	}), playback.Options{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	coord, err := NewCoordinator(context.Background(), st, Options{
		Player:          ctrl,
		MediaChecker:    NewRootMediaChecker(ws.Owner()),
		ChapterReader:   FFprobeChapterReader{},
		WorkspaceCloser: ws.Close,
	})
	if err != nil {
		t.Fatal(err)
	}

	chMsg, ok := coord.LoadChapters(context.Background(), key)
	if !ok || len(chMsg.Chapters) != 2 || chMsg.Chapters[0] != "Intro" || chMsg.Chapters[1] != "Part Two" {
		t.Fatalf("FFprobeChapterReader failed: ok=%v msg=%+v", ok, chMsg)
	}

	encKey, _ := EncodeBookKey(key)
	if msg := coord.Dispatch(ui.Action{Kind: ui.PlayBook, Key: encKey})(); msg == nil {
		t.Fatal("expected PlayBook snapshot")
	}
	if ctrl.Snapshot().State != playback.StatePlaying {
		t.Fatalf("expected real mpv playing, got %+v", ctrl.Snapshot())
	}
	_ = coord.Dispatch(ui.Action{Kind: ui.TogglePlayback, Session: strconv.FormatUint(ctrl.Snapshot().Session, 10)})()
	if msg := coord.Dispatch(ui.Action{Kind: ui.ActivateChapter, Key: encKey, ChapterIndex: 1, ChapterStart: 2 * time.Second})(); msg == nil {
		t.Fatal("expected chapter seek snapshot")
	}
	if snap := ctrl.Snapshot(); snap.State != playback.StatePaused || snap.Position < 1500*time.Millisecond {
		t.Fatalf("expected paused seek to Part Two, got %+v", snap)
	}
	if _, ok := coord.PollPlayback(context.Background()); !ok {
		t.Fatal("PollPlayback failed on real mpv")
	}
	if err := coord.Close(context.Background()); err != nil {
		t.Fatalf("Coordinator.Close failed: %v", err)
	}
}
