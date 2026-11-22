package bookmarks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/susamn/taletty/app/internal/playback"
	"github.com/susamn/taletty/app/internal/store"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if path := os.Getenv("TALETTY_SQLITE_TEST_CACHE"); path != "" {
		cache, err := wazero.NewCompilationCacheWithDir(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		sqlite3.RuntimeConfig = wazero.NewRuntimeConfig().WithMemoryLimitPages(4096).
			WithCoreFeatures(api.CoreFeaturesV2).WithCompilationCache(cache)
	}
	os.Exit(m.Run())
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func strPtr(v string) *string { return &v }

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(ms int64) *fakeClock {
	return &fakeClock{now: time.UnixMilli(ms)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now.UnixMilli()
}

func (c *fakeClock) MS() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now.UnixMilli()
}

type fakeBackend struct {
	mu       sync.Mutex
	pos      time.Duration
	dur      time.Duration
	paused   bool
	speed    int64
	volume   int
	starts   int
	seeks    []time.Duration
	stops    int
	startErr error
	seekErr  error
}

func (b *fakeBackend) Start(_ context.Context, _ string, startPos time.Duration, speedMilli int64, volume int, paused bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.startErr != nil {
		return b.startErr
	}
	b.starts++
	b.pos = startPos
	b.speed = speedMilli
	b.volume = volume
	b.paused = paused
	return nil
}

func (b *fakeBackend) SetPause(_ context.Context, paused bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.paused = paused
	return nil
}

func (b *fakeBackend) SeekExact(_ context.Context, target time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seekErr != nil {
		return b.seekErr
	}
	b.pos = target
	b.seeks = append(b.seeks, target)
	return nil
}

func (b *fakeBackend) SetSpeed(_ context.Context, speedMilli int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.speed = speedMilli
	return nil
}

func (b *fakeBackend) SetVolume(_ context.Context, volume int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.volume = volume
	return nil
}

func (b *fakeBackend) Position(_ context.Context) (time.Duration, time.Duration, bool, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pos, b.dur, b.paused, false, nil
}

func (b *fakeBackend) Stop(_ context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stops++
	return nil
}

func (b *fakeBackend) setPos(pos time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pos = pos
}

func (b *fakeBackend) seekHistory() []time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]time.Duration, len(b.seeks))
	copy(out, b.seeks)
	return out
}

type bookmarkFixture struct {
	rootDir string
	owner   *store.Owner
	store   *store.Store
	backend *fakeBackend
	player  *playback.Controller
	clock   *fakeClock
	service *Service
}

func openBookmarkFixture(t *testing.T) *bookmarkFixture {
	t.Helper()
	rootDir := t.TempDir()
	must(t, os.Chmod(rootDir, 0o700))
	must(t, os.Mkdir(filepath.Join(rootDir, "downloads"), 0o700))

	rootFile, err := os.Open(rootDir)
	must(t, err)
	t.Cleanup(func() { _ = rootFile.Close() })

	owner, err := store.NewOwner(context.Background(), rootFile, func() error { return nil })
	must(t, err)
	t.Cleanup(func() { must(t, owner.Close(context.Background())) })

	st, err := store.Open(context.Background(), owner)
	must(t, err)
	t.Cleanup(func() { must(t, st.Close(context.Background())) })

	clock := newFakeClock(1_700_000_000_000)
	backend := &fakeBackend{dur: 30 * time.Minute}
	ctrl, err := playback.NewController(backend, playback.Options{
		Store: st,
		Clock: clock.Now,
	})
	must(t, err)
	t.Cleanup(func() { must(t, ctrl.Close(context.Background())) })

	svc, err := NewService(Options{
		Store:     st,
		Player:    ctrl,
		RootOwner: owner,
	})
	must(t, err)

	return &bookmarkFixture{
		rootDir: rootDir,
		owner:   owner,
		store:   st,
		backend: backend,
		player:  ctrl,
		clock:   clock,
		service: svc,
	}
}

func (f *bookmarkFixture) writePrivateMedia(t *testing.T, relPath string) string {
	t.Helper()
	abs := filepath.Join(f.rootDir, filepath.FromSlash(relPath))
	must(t, os.MkdirAll(filepath.Dir(abs), 0o700))
	must(t, os.WriteFile(abs, []byte("m4b-fixture"), 0o600))
	return abs
}

type faultBookmarkStore struct {
	inner     BookmarkStore
	listErr   error
	putErr    error
	deleteErr error
	mediaErr  error
	onList    func()
	onDelete  func()
}

func (f *faultBookmarkStore) Bookmarks(ctx context.Context, key store.BookKey) ([]store.Bookmark, error) {
	if f.onList != nil {
		f.onList()
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.inner.Bookmarks(ctx, key)
}

func (f *faultBookmarkStore) PutBookmark(ctx context.Context, b store.Bookmark) error {
	if f.putErr != nil {
		return f.putErr
	}
	return f.inner.PutBookmark(ctx, b)
}

func (f *faultBookmarkStore) DeleteBookmark(ctx context.Context, key store.BookKey, bookmarkID string) error {
	if f.onDelete != nil {
		f.onDelete()
	}
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return f.inner.DeleteBookmark(ctx, key, bookmarkID)
}

func (f *faultBookmarkStore) LocalMedia(ctx context.Context, key store.BookKey) (store.LocalMedia, bool, error) {
	if f.mediaErr != nil {
		return store.LocalMedia{}, false, f.mediaErr
	}
	return f.inner.LocalMedia(ctx, key)
}

type stubPlayer struct {
	snap    playback.Snapshot
	loadFn  func(context.Context, playback.Track, bool) (playback.Snapshot, error)
	seekFn  func(context.Context, uint64, time.Duration) (playback.Snapshot, error)
	loadCnt int
	seekCnt int
}

func (p *stubPlayer) Snapshot() playback.Snapshot { return p.snap }

func (p *stubPlayer) Load(ctx context.Context, track playback.Track, autoPlay bool) (playback.Snapshot, error) {
	p.loadCnt++
	if p.loadFn != nil {
		return p.loadFn(ctx, track, autoPlay)
	}
	return p.snap, nil
}

func (p *stubPlayer) SeekAbsolute(ctx context.Context, session uint64, target time.Duration) (playback.Snapshot, error) {
	p.seekCnt++
	if p.seekFn != nil {
		return p.seekFn(ctx, session, target)
	}
	return p.snap, nil
}

type stubMediaChecker struct {
	fn func(context.Context, store.LocalMedia) (string, error)
}

func (m stubMediaChecker) VerifyMedia(ctx context.Context, media store.LocalMedia) (string, error) {
	return m.fn(ctx, media)
}

type stubRootOwner struct {
	fn func(context.Context) (*store.RootLease, error)
}

func (o stubRootOwner) Lease(ctx context.Context) (*store.RootLease, error) {
	return o.fn(ctx)
}

func TestCaptureDraftPlayingPausedRejectedAndChapterBoundary(t *testing.T) {
	f := openBookmarkFixture(t)
	ctx := context.Background()

	orchard := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	rivers := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	orchardPath := f.writePrivateMedia(t, "downloads/orchard.m4b")

	// Seed catalog with Rivers highlighted while no audio is loaded.
	must(t, f.store.ReplaceCatalog(ctx, "audible", "us", []store.CatalogBook{
		{Key: orchard, Title: "Orchard"},
		{Key: rivers, Title: "Rivers"},
	}, f.clock.MS()))
	must(t, f.store.PutViewState(ctx, store.ViewState{
		Destination: store.DestinationAll,
		Selected:    &rivers,
		Focus:       store.FocusBooks,
		SortOrder:   store.SortTitle,
	}))

	// StateIdle rejects capture with ErrNoLoadedAudio (never falls back to highlighted Rivers).
	if _, err := f.service.CaptureDefaultDraft(ctx, nil); !errors.Is(err, ErrNoLoadedAudio) {
		t.Fatalf("expected ErrNoLoadedAudio when idle, got %v", err)
	}

	// Load Orchard playing at 18m40s while Rivers remains highlighted.
	must(t, f.store.PutListeningState(ctx, store.ListeningState{
		Key:         orchard,
		PositionNS:  int64(18*time.Minute + 40*time.Second),
		SpeedMilli:  playback.DefaultSpeedMilli,
		Completion:  store.CompletionUnfinished,
		UpdatedAtMS: f.clock.MS(),
	}))
	snap, err := f.player.Load(ctx, playback.Track{
		Key:      orchard,
		Title:    "Orchard",
		Path:     orchardPath,
		Duration: 30 * time.Minute,
	}, true)
	must(t, err)

	chapters := func(key store.BookKey, pos time.Duration) (ChapterRef, bool) {
		if key != orchard {
			return ChapterRef{}, false
		}
		switch {
		case pos >= 10*time.Minute:
			return ChapterRef{ID: "ch-2", Title: "Chapter Two"}, true
		case pos >= 5*time.Second:
			return ChapterRef{ID: "ch-1", Title: "Chapter One"}, true
		default:
			return ChapterRef{}, false
		}
	}

	// Capture while playing: freezes Orchard (not highlighted Rivers), keeps playing.
	draftPlaying, err := f.service.CaptureDefaultDraft(ctx, chapters)
	must(t, err)
	if draftPlaying.BookKey != orchard || draftPlaying.Session != snap.Session || draftPlaying.Title != "Orchard" {
		t.Fatalf("unexpected captured target: %+v", draftPlaying)
	}
	if draftPlaying.CapturedPosition != 18*time.Minute+40*time.Second ||
		draftPlaying.PreRoll != DefaultPreRoll ||
		draftPlaying.JumpPosition != 18*time.Minute+38*time.Second {
		t.Fatalf("unexpected captured/jump positions: %+v", draftPlaying)
	}
	if draftPlaying.CrossedChapterBoundary() ||
		draftPlaying.JumpChapter == nil || *draftPlaying.JumpChapter != (ChapterRef{ID: "ch-2", Title: "Chapter Two"}) {
		t.Fatalf("unexpected chapter context: %+v", draftPlaying)
	}
	if cur := f.player.Snapshot(); cur.State != playback.StatePlaying {
		t.Fatalf("capture altered playback state: %v", cur.State)
	}

	// Pause at 10m01s (chapter boundary at 10m00s): pre-roll crosses from ch-2 into ch-1.
	_, err = f.player.SeekAbsolute(ctx, snap.Session, 10*time.Minute+time.Second)
	must(t, err)
	snap, err = f.player.Pause(ctx, snap.Session)
	must(t, err)

	draftBoundary, err := f.service.CaptureDefaultDraft(ctx, chapters)
	must(t, err)
	if draftBoundary.CapturedPosition != 10*time.Minute+time.Second ||
		draftBoundary.JumpPosition != 9*time.Minute+59*time.Second {
		t.Fatalf("unexpected boundary positions: %+v", draftBoundary)
	}
	if !draftBoundary.CrossedChapterBoundary() {
		t.Fatal("expected CrossedChapterBoundary() == true")
	}
	if draftBoundary.CapturedChapter == nil || *draftBoundary.CapturedChapter != (ChapterRef{ID: "ch-2", Title: "Chapter Two"}) {
		t.Fatalf("unexpected CapturedChapter: %+v", draftBoundary.CapturedChapter)
	}
	if draftBoundary.JumpChapter == nil || *draftBoundary.JumpChapter != (ChapterRef{ID: "ch-1", Title: "Chapter One"}) {
		t.Fatalf("unexpected JumpChapter: %+v", draftBoundary.JumpChapter)
	}
	if cur := f.player.Snapshot(); cur.State != playback.StatePaused {
		t.Fatalf("paused capture altered playback state: %v", cur.State)
	}

	// Pre-roll zero clamp and crossing from ch-1 into unchaptered intro.
	_, err = f.player.SeekAbsolute(ctx, snap.Session, 6*time.Second)
	must(t, err)
	draftIntro, err := f.service.CaptureDraft(ctx, 10*time.Second, chapters)
	must(t, err)
	if draftIntro.JumpPosition != 0 || !draftIntro.CrossedChapterBoundary() ||
		draftIntro.CapturedChapter == nil || draftIntro.JumpChapter != nil {
		t.Fatalf("expected zero clamp and nil JumpChapter, got %+v", draftIntro)
	}
	if draftIntro.ChapterID() != nil || draftIntro.ChapterTitle() != nil {
		t.Fatalf("expected nil chapter pointers for unchaptered jump, got %v / %v", draftIntro.ChapterID(), draftIntro.ChapterTitle())
	}

	// Zero pre-roll (off) preserves exact position and clones chapter context.
	draftZeroPreRoll, err := f.service.CaptureDraft(ctx, 0, chapters)
	must(t, err)
	if draftZeroPreRoll.JumpPosition != 6*time.Second || draftZeroPreRoll.CrossedChapterBoundary() {
		t.Fatalf("unexpected zero pre-roll draft: %+v", draftZeroPreRoll)
	}
	if draftZeroPreRoll.CapturedChapter == draftZeroPreRoll.JumpChapter {
		t.Fatal("expected distinct chapter pointers on zero pre-roll")
	}

	// Nil chapterAt and empty ChapterRef produce nil chapter context without crossing.
	draftNoChapter, err := f.service.CaptureDraft(ctx, 0, nil)
	must(t, err)
	if draftNoChapter.CrossedChapterBoundary() || draftNoChapter.CapturedChapter != nil || draftNoChapter.JumpChapter != nil {
		t.Fatalf("expected nil chapters, got %+v", draftNoChapter)
	}
	draftEmptyChapter, err := f.service.CaptureDraft(ctx, 0, func(store.BookKey, time.Duration) (ChapterRef, bool) {
		return ChapterRef{}, true
	})
	must(t, err)
	if draftEmptyChapter.CapturedChapter != nil || draftEmptyChapter.JumpChapter != nil {
		t.Fatalf("expected empty ChapterRef ignored, got %+v", draftEmptyChapter)
	}

	// StateStopped rejects capture with ErrNoLoadedAudio.
	_, err = f.player.Stop(ctx)
	must(t, err)
	if _, err := f.service.CaptureDefaultDraft(ctx, nil); !errors.Is(err, ErrNoLoadedAudio) {
		t.Fatalf("expected ErrNoLoadedAudio when stopped, got %v", err)
	}

	// StateFailed rejects capture with ErrNoLoadedAudio.
	f.backend.startErr = playback.ErrIPC
	_, _ = f.player.Load(ctx, playback.Track{Key: orchard, Title: "Orchard", Path: orchardPath}, true)
	if _, err := f.service.CaptureDefaultDraft(ctx, nil); !errors.Is(err, ErrNoLoadedAudio) {
		t.Fatalf("expected ErrNoLoadedAudio when failed, got %v", err)
	}
	f.backend.startErr = nil

	// Pure ComputeJumpPosition and invalid chapter metadata checks.
	if got, err := ComputeJumpPosition(5*time.Second, 2*time.Second); err != nil || got != 3*time.Second {
		t.Fatalf("ComputeJumpPosition = (%v, %v)", got, err)
	}
	if got, err := ComputeJumpPosition(time.Second, 5*time.Second); err != nil || got != 0 {
		t.Fatalf("ComputeJumpPosition clamp = (%v, %v)", got, err)
	}
	for _, bad := range [][2]time.Duration{{-1, 0}, {0, -1}} {
		if _, err := ComputeJumpPosition(bad[0], bad[1]); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for %v, got %v", bad, err)
		}
	}
	if _, err := f.service.CaptureDraft(ctx, -time.Second, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for negative preRoll, got %v", err)
	}

	// Unsafe chapter metadata from chapterAt is rejected cleanly.
	snapOrchard, err := f.player.Load(ctx, playback.Track{Key: orchard, Title: "Orchard", Path: orchardPath}, true)
	must(t, err)
	_, err = f.player.SeekAbsolute(ctx, snapOrchard.Session, time.Second)
	must(t, err)
	if _, err := f.service.CaptureDraft(ctx, 0, func(store.BookKey, time.Duration) (ChapterRef, bool) {
		return ChapterRef{ID: "ch-1", Title: "Bad\x1bTitle"}, true
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for unsafe captured chapter, got %v", err)
	}
	if _, err := f.service.CaptureDraft(ctx, time.Second, func(_ store.BookKey, pos time.Duration) (ChapterRef, bool) {
		if pos == 0 {
			return ChapterRef{ID: "bad\n", Title: "Ok"}, true
		}
		return ChapterRef{ID: "ch-1", Title: "Ok"}, true
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for unsafe jump chapter, got %v", err)
	}
}

func TestDelayedSaveValidationDuplicatesEditDeleteAndOfflineList(t *testing.T) {
	f := openBookmarkFixture(t)
	ctx := context.Background()

	bookA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	bookB := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	pathA := f.writePrivateMedia(t, "downloads/bookA.m4b")
	pathB := f.writePrivateMedia(t, "downloads/bookB.m4b")

	must(t, f.store.PutListeningState(ctx, store.ListeningState{
		Key:         bookA,
		PositionNS:  int64(18*time.Minute + 40*time.Second),
		SpeedMilli:  playback.DefaultSpeedMilli,
		Completion:  store.CompletionUnfinished,
		UpdatedAtMS: f.clock.MS(),
	}))
	must(t, f.store.PutLocalMedia(ctx, store.LocalMedia{
		Key:           bookA,
		RelativePath:  "downloads/bookA.m4b",
		Format:        "m4b",
		PublishedAtMS: f.clock.MS(),
	}))
	must(t, f.store.SetFavorite(ctx, bookA, true, f.clock.MS()))

	snapA, err := f.player.Load(ctx, playback.Track{
		Key:      bookA,
		Title:    "Orchard",
		Path:     pathA,
		Duration: 30 * time.Minute,
	}, false)
	must(t, err)

	// Capture draft at 18m40s (jump 18m38s) in ch-2 -> ch-1 boundary.
	draft, err := f.service.CaptureDefaultDraft(ctx, func(_ store.BookKey, pos time.Duration) (ChapterRef, bool) {
		if pos >= 18*time.Minute+39*time.Second {
			return ChapterRef{ID: "ch-2", Title: "Chapter Two"}, true
		}
		return ChapterRef{ID: "ch-1", Title: "Chapter One"}, true
	})
	must(t, err)

	// Advance playback, change speed, and switch to Book B BEFORE saving draft.
	_, err = f.player.SeekAbsolute(ctx, snapA.Session, 25*time.Minute)
	must(t, err)
	_, err = f.player.SetSpeed(ctx, snapA.Session, 1_500_000)
	must(t, err)
	_, err = f.player.Load(ctx, playback.Track{
		Key:      bookB,
		Title:    "Rivers",
		Path:     pathB,
		Duration: 20 * time.Minute,
	}, true)
	must(t, err)

	// Delayed save persists frozen Book A target, 18m38s jump position, and jump chapter ch-1.
	saveTS := f.clock.Advance(30 * time.Second)
	bm1, err := f.service.SaveNew(ctx, draft, "Line one\n\tLine two", saveTS)
	must(t, err)
	if bm1.Key != bookA ||
		bm1.PositionNS != int64(18*time.Minute+38*time.Second) ||
		bm1.Note != "Line one\n\tLine two" ||
		bm1.ChapterID == nil || *bm1.ChapterID != "ch-1" ||
		bm1.ChapterTitle == nil || *bm1.ChapterTitle != "Chapter One" ||
		bm1.CreatedAtMS != saveTS || bm1.UpdatedAtMS != saveTS {
		t.Fatalf("delayed save did not preserve frozen draft: %+v", bm1)
	}

	// Duplicate position allowed with distinct stable ID.
	bm2, err := f.service.SaveNew(ctx, draft, "Second note at same position", f.clock.Advance(time.Second))
	must(t, err)
	if bm2.ID == bm1.ID || bm2.PositionNS != bm1.PositionNS {
		t.Fatalf("expected distinct ID at duplicate position: bm1=%+v bm2=%+v", bm1, bm2)
	}

	// Earlier bookmark for ordering check.
	bmEarly, err := f.service.SaveNew(ctx, Draft{
		BookKey:      bookA,
		JumpPosition: 5 * time.Minute,
	}, "Earlier note", f.clock.Advance(time.Second))
	must(t, err)

	// Save validation rejects whitespace-only, control chars, Unicode Cf, and invalid UTF-8.
	for _, badNote := range []string{
		"",
		"   ",
		"\n\t ",
		"\u00a0\u2003",
		"bad\x00note",
		"bad\x1b[31mnote",
		"bad\rnote",
		"bad\u200bnote",
		string([]byte{0xff, 0xfe}),
	} {
		if _, err := f.service.SaveNew(ctx, draft, badNote, f.clock.MS()); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for note %q, got %v", badNote, err)
		}
	}

	// EditNote updates ONLY Note and UpdatedAtMS without recapturing position or reapplying pre-roll.
	editTS := f.clock.Advance(time.Minute)
	edited, err := f.service.EditNote(ctx, bookA, bm1.ID, "Edited\n\tnote", editTS)
	must(t, err)
	if edited.ID != bm1.ID ||
		edited.Key != bookA ||
		edited.PositionNS != bm1.PositionNS ||
		!reflect.DeepEqual(edited.ChapterID, bm1.ChapterID) ||
		!reflect.DeepEqual(edited.ChapterTitle, bm1.ChapterTitle) ||
		edited.CreatedAtMS != bm1.CreatedAtMS ||
		edited.UpdatedAtMS != editTS ||
		edited.Note != "Edited\n\tnote" {
		t.Fatalf("EditNote altered immutable fields: got=%+v orig=%+v", edited, bm1)
	}

	// EditNote rejects missing ID and wrong-book ID cleanly without mutating bm1.
	if _, err := f.service.EditNote(ctx, bookA, "missing-id", "Nope", f.clock.MS()); !errors.Is(err, ErrBookmarkNotFound) {
		t.Fatalf("expected ErrBookmarkNotFound for missing ID, got %v", err)
	}
	if _, err := f.service.EditNote(ctx, bookB, bm1.ID, "Steal", f.clock.MS()); !errors.Is(err, ErrBookmarkNotFound) {
		t.Fatalf("expected ErrBookmarkNotFound for wrong book, got %v", err)
	}
	if _, err := f.service.EditNote(ctx, bookA, bm1.ID, "Backdated", bm1.CreatedAtMS-1); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for UpdatedAtMS < CreatedAtMS, got %v", err)
	}

	// Offline List works with stopped player and returns PositionNS ascending, tie-broken by ID.
	_, err = f.player.Stop(ctx)
	must(t, err)
	listed, err := f.service.List(ctx, bookA)
	must(t, err)
	wantOrder := []store.Bookmark{bmEarly, edited, bm2}
	if bm2.ID < edited.ID {
		wantOrder = []store.Bookmark{bmEarly, bm2, edited}
	}
	if !reflect.DeepEqual(listed, wantOrder) {
		t.Fatalf("unexpected offline list order:\ngot:  %+v\nwant: %+v", listed, wantOrder)
	}
	// Mutating returned slice/pointers does not affect stored data.
	*listed[1].ChapterTitle = "Mutated"
	listedAgain, err := f.service.List(ctx, bookA)
	must(t, err)
	if *listedAgain[1].ChapterTitle != "Chapter One" {
		t.Fatalf("returned bookmark pointers were not isolated: %q", *listedAgain[1].ChapterTitle)
	}

	// Wrong-book Delete does not remove Book A's bookmark.
	must(t, f.service.Delete(ctx, bookB, bmEarly.ID))
	listed, err = f.service.List(ctx, bookA)
	must(t, err)
	if len(listed) != 3 {
		t.Fatalf("wrong-book delete removed bookmark: %+v", listed)
	}

	// Delete single bookmark preserves remaining bookmarks, audio file, LocalMedia, history, and favorites.
	must(t, f.service.Delete(ctx, bookA, bmEarly.ID))
	listed, err = f.service.List(ctx, bookA)
	must(t, err)
	if len(listed) != 2 {
		t.Fatalf("expected 2 remaining bookmarks, got %d", len(listed))
	}
	if _, err := os.Stat(pathA); err != nil {
		t.Fatalf("audio file deleted by bookmark Delete: %v", err)
	}
	if _, found, err := f.store.LocalMedia(ctx, bookA); err != nil || !found {
		t.Fatalf("LocalMedia lost after bookmark Delete: found=%v err=%v", found, err)
	}
	if ls, found, err := f.store.ListeningState(ctx, bookA); err != nil || !found || ls.PositionNS != int64(25*time.Minute) {
		t.Fatalf("ListeningState lost after bookmark Delete: found=%v ls=%+v err=%v", found, ls, err)
	}
	if favs, err := f.store.Favorites(ctx); err != nil || !reflect.DeepEqual(favs, []store.BookKey{bookA}) {
		t.Fatalf("Favorites lost after bookmark Delete: favs=%+v err=%v", favs, err)
	}

	// Empty list returns nil cleanly.
	emptyList, err := f.service.List(ctx, bookB)
	must(t, err)
	if emptyList != nil {
		t.Fatalf("expected nil empty list, got %+v", emptyList)
	}
}

func TestJumpSameBookAndCrossBookTargetingPolicy(t *testing.T) {
	f := openBookmarkFixture(t)
	ctx := context.Background()

	bookA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	bookB := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	bookC := store.BookKey{Provider: "local", Namespace: "user", ID: "book-c"}
	pathA := f.writePrivateMedia(t, "downloads/bookA.m4b")
	_ = f.writePrivateMedia(t, "downloads/nested/bookB.m4b")
	_ = f.writePrivateMedia(t, "downloads/bookC.m4b")

	must(t, f.store.PutLocalMedia(ctx, store.LocalMedia{Key: bookA, RelativePath: "downloads/bookA.m4b", Format: "m4b", PublishedAtMS: 10}))
	must(t, f.store.PutLocalMedia(ctx, store.LocalMedia{Key: bookB, RelativePath: "downloads/nested/bookB.m4b", Format: "m4b", PublishedAtMS: 20}))
	must(t, f.store.PutLocalMedia(ctx, store.LocalMedia{Key: bookC, RelativePath: "downloads/bookC.m4b", Format: "m4b", PublishedAtMS: 30}))

	// Seed Book A finished at 50s, Book B saved at 99s with 1.25x speed.
	must(t, f.store.PutListeningState(ctx, store.ListeningState{
		Key:         bookA,
		PositionNS:  int64(50 * time.Second),
		SpeedMilli:  1_500_000,
		Completion:  store.CompletionFinished,
		UpdatedAtMS: 100,
	}))
	must(t, f.store.PutListeningState(ctx, store.ListeningState{
		Key:         bookB,
		PositionNS:  int64(99 * time.Second),
		SpeedMilli:  1_250_000,
		Completion:  store.CompletionUnfinished,
		UpdatedAtMS: 100,
	}))

	// Load Book A paused.
	snapA, err := f.player.Load(ctx, playback.Track{
		Key:      bookA,
		Title:    "Orchard",
		Path:     pathA,
		Duration: 30 * time.Minute,
	}, false)
	must(t, err)

	// Same-book jump while paused: preserves StatePaused, seeks exact position (no double rewind).
	bmA := store.Bookmark{
		ID:          "bm-a",
		Key:         bookA,
		PositionNS:  int64(12 * time.Minute),
		Note:        "Paused jump",
		CreatedAtMS: 100,
		UpdatedAtMS: 100,
	}
	jumpedA, err := f.service.Jump(ctx, bmA, snapA.Session, "Orchard", 30*time.Minute)
	must(t, err)
	if jumpedA.State != playback.StatePaused || jumpedA.Position != 12*time.Minute || jumpedA.Session != snapA.Session {
		t.Fatalf("same-book paused jump failed: %+v", jumpedA)
	}
	if seeks := f.backend.seekHistory(); !reflect.DeepEqual(seeks, []time.Duration{12 * time.Minute}) {
		t.Fatalf("expected single exact seek to 12m, got %v", seeks)
	}

	// Same-book jump while playing preserves StatePlaying.
	snapA, err = f.player.Play(ctx, snapA.Session)
	must(t, err)
	bmA2 := store.Bookmark{ID: "bm-a2", Key: bookA, PositionNS: int64(45 * time.Second), Note: "Play jump", CreatedAtMS: 100, UpdatedAtMS: 100}
	jumpedA2, err := f.service.Jump(ctx, bmA2, snapA.Session, "", 0)
	must(t, err)
	if jumpedA2.State != playback.StatePlaying || jumpedA2.Position != 45*time.Second {
		t.Fatalf("same-book playing jump failed: %+v", jumpedA2)
	}

	// Same-book stale session rejected without seeking.
	seeksBefore := len(f.backend.seekHistory())
	if _, err := f.service.Jump(ctx, bmA, snapA.Session+99, "Orchard", 30*time.Minute); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("expected ErrStaleSession, got %v", err)
	}
	if _, err := f.service.Jump(ctx, bmA, 0, "Orchard", 30*time.Minute); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("expected ErrStaleSession for zero session, got %v", err)
	}
	if len(f.backend.seekHistory()) != seeksBefore {
		t.Fatal("stale same-book jump triggered seek")
	}

	// Cross-book jump from Book A (at 45s, 1.5x, Finished) to Book B at 0s (overrides Book B's saved 99s!).
	bmBZero := store.Bookmark{
		ID:          "bm-b-zero",
		Key:         bookB,
		PositionNS:  0,
		Note:        "Start of Book B",
		CreatedAtMS: 100,
		UpdatedAtMS: 100,
	}
	jumpedB, err := f.service.Jump(ctx, bmBZero, snapA.Session, "Rivers", 25*time.Minute)
	must(t, err)
	if jumpedB.Book == nil || *jumpedB.Book != bookB ||
		jumpedB.State != playback.StatePlaying ||
		jumpedB.Position != 0 ||
		jumpedB.SpeedMilli != 1_250_000 ||
		jumpedB.Session == snapA.Session {
		t.Fatalf("cross-book jump failed: %+v", jumpedB)
	}
	// Verify Book A's checkpoint was preserved by Controller.Load before switching.
	savedA, found, err := f.store.ListeningState(ctx, bookA)
	must(t, err)
	if !found || savedA.PositionNS != int64(45*time.Second) || savedA.SpeedMilli != 1_500_000 || savedA.Completion != store.CompletionFinished {
		t.Fatalf("old book checkpoint not preserved: found=%v state=%+v", found, savedA)
	}
	// Verify Book B's checkpoint reflects exact bookmark position 0 (not saved 99s).
	savedB, found, err := f.store.ListeningState(ctx, bookB)
	must(t, err)
	if !found || savedB.PositionNS != 0 {
		t.Fatalf("target book checkpoint mismatch: found=%v state=%+v", found, savedB)
	}

	// Cross-book jump to non-zero position after root directory rename (anchored FD path derivation).
	renamedRoot := f.rootDir + "-renamed"
	must(t, os.Rename(f.rootDir, renamedRoot))
	t.Cleanup(func() { _ = os.Rename(renamedRoot, f.rootDir) })

	bmC := store.Bookmark{
		ID:          "bm-c",
		Key:         bookC,
		PositionNS:  int64(7*time.Minute + 15*time.Second),
		Note:        "Renamed root jump",
		CreatedAtMS: 100,
		UpdatedAtMS: 100,
	}
	jumpedC, err := f.service.Jump(ctx, bmC, jumpedB.Session, "Book C", 20*time.Minute)
	must(t, err)
	if jumpedC.Book == nil || *jumpedC.Book != bookC || jumpedC.Position != 7*time.Minute+15*time.Second || jumpedC.State != playback.StatePlaying {
		t.Fatalf("cross-book jump after root rename failed: %+v", jumpedC)
	}

	// Cross-book jump from StateStopped (no loaded book) succeeds with current session.
	stoppedSnap, err := f.player.Stop(ctx)
	must(t, err)
	jumpedFromStopped, err := f.service.Jump(ctx, bmA, stoppedSnap.Session, "Orchard", 30*time.Minute)
	must(t, err)
	if jumpedFromStopped.Book == nil || *jumpedFromStopped.Book != bookA || jumpedFromStopped.Position != 12*time.Minute {
		t.Fatalf("jump from stopped failed: %+v", jumpedFromStopped)
	}

	// Cross-book jump with unknown catalog duration (targetDuration == 0) seeks exact position without clamping.
	f.backend.dur = 0
	bmUnknownDur := store.Bookmark{
		ID:          "bm-b-unknown-dur",
		Key:         bookB,
		PositionNS:  int64(14 * time.Minute),
		Note:        "Unknown duration jump",
		CreatedAtMS: 100,
		UpdatedAtMS: 100,
	}
	jumpedUnknownDur, err := f.service.Jump(ctx, bmUnknownDur, jumpedFromStopped.Session, "Rivers", 0)
	must(t, err)
	if jumpedUnknownDur.Book == nil || *jumpedUnknownDur.Book != bookB || jumpedUnknownDur.Position != 14*time.Minute {
		t.Fatalf("cross-book jump with unknown duration failed: %+v", jumpedUnknownDur)
	}
	f.backend.dur = 30 * time.Minute

	// Cross-book stale confirmation rejected without touching current audio.
	if _, err := f.service.Jump(ctx, bmBZero, snapA.Session, "Rivers", 25*time.Minute); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("expected ErrStaleSession for stale cross-book confirmation, got %v", err)
	}
	if cur := f.player.Snapshot(); cur.Book == nil || *cur.Book != bookB || cur.Position != 14*time.Minute {
		t.Fatalf("stale cross-book jump altered current audio: %+v", cur)
	}
}

func TestJumpMissingUnsafeMediaAndStaleMidCheck(t *testing.T) {
	f := openBookmarkFixture(t)
	ctx := context.Background()

	bookA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	bookMissing := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	pathA := f.writePrivateMedia(t, "downloads/bookA.m4b")
	must(t, f.store.PutLocalMedia(ctx, store.LocalMedia{Key: bookA, RelativePath: "downloads/bookA.m4b", Format: "m4b", PublishedAtMS: 10}))

	snapA, err := f.player.Load(ctx, playback.Track{Key: bookA, Title: "Orchard", Path: pathA, Duration: 30 * time.Minute}, true)
	must(t, err)
	f.backend.setPos(33 * time.Second)

	bmMissing := store.Bookmark{ID: "bm-miss", Key: bookMissing, PositionNS: int64(5 * time.Minute), Note: "Missing", CreatedAtMS: 1, UpdatedAtMS: 1}

	// 1. Absent LocalMedia row -> ErrMediaMissing, current audio untouched.
	if _, err := f.service.Jump(ctx, bmMissing, snapA.Session, "Missing", 10*time.Minute); !errors.Is(err, ErrMediaMissing) {
		t.Fatalf("expected ErrMediaMissing when LocalMedia absent, got %v", err)
	}
	if cur := f.player.Snapshot(); cur.Book == nil || *cur.Book != bookA || cur.Session != snapA.Session {
		t.Fatalf("missing media altered current audio: %+v", cur)
	}

	// 2. LocalMedia row exists, file missing on disk -> ErrMediaMissing.
	must(t, f.store.PutLocalMedia(ctx, store.LocalMedia{Key: bookMissing, RelativePath: "downloads/absent.m4b", Format: "m4b", PublishedAtMS: 20}))
	if _, err := f.service.Jump(ctx, bmMissing, snapA.Session, "Missing", 10*time.Minute); !errors.Is(err, ErrMediaMissing) {
		t.Fatalf("expected ErrMediaMissing when file absent on disk, got %v", err)
	}

	// 3. Adversarial unsafe files under downloads/ -> ErrMediaMissing.
	outside := filepath.Join(t.TempDir(), "outside.m4b")
	must(t, os.WriteFile(outside, []byte("outside"), 0o600))

	unsafeCases := []struct {
		name  string
		rel   string
		setup func(abs string)
	}{
		{
			name: "symlink-file",
			rel:  "downloads/sym.m4b",
			setup: func(abs string) {
				must(t, os.Symlink(outside, abs))
			},
		},
		{
			name: "hardlink-file",
			rel:  "downloads/hard.m4b",
			setup: func(abs string) {
				src := filepath.Join(f.rootDir, "downloads", "hard-src.m4b")
				must(t, os.WriteFile(src, []byte("x"), 0o600))
				must(t, os.Link(src, abs))
			},
		},
		{
			name: "public-mode-file",
			rel:  "downloads/pub.m4b",
			setup: func(abs string) {
				must(t, os.WriteFile(abs, []byte("x"), 0o644))
			},
		},
		{
			name: "special-mode-file",
			rel:  "downloads/setuid.m4b",
			setup: func(abs string) {
				must(t, os.WriteFile(abs, []byte("x"), 0o600))
				must(t, unix.Chmod(abs, 0o4600))
			},
		},
		{
			name: "fifo-file",
			rel:  "downloads/fifo.m4b",
			setup: func(abs string) {
				must(t, unix.Mkfifo(abs, 0o600))
			},
		},
		{
			name: "directory-leaf",
			rel:  "downloads/dir.m4b",
			setup: func(abs string) {
				must(t, os.Mkdir(abs, 0o700))
			},
		},
		{
			name: "unsafe-subdir-mode",
			rel:  "downloads/badsub/book.m4b",
			setup: func(abs string) {
				must(t, os.Mkdir(filepath.Dir(abs), 0o755))
				must(t, os.WriteFile(abs, []byte("x"), 0o600))
			},
		},
		{
			name: "symlink-subdir",
			rel:  "downloads/symsub/book.m4b",
			setup: func(abs string) {
				target := t.TempDir()
				must(t, os.Chmod(target, 0o700))
				must(t, os.WriteFile(filepath.Join(target, "book.m4b"), []byte("x"), 0o600))
				must(t, os.Symlink(target, filepath.Dir(abs)))
			},
		},
	}

	for _, tc := range unsafeCases {
		t.Run(tc.name, func(t *testing.T) {
			abs := filepath.Join(f.rootDir, filepath.FromSlash(tc.rel))
			tc.setup(abs)
			must(t, f.store.PutLocalMedia(ctx, store.LocalMedia{
				Key:           bookMissing,
				RelativePath:  tc.rel,
				Format:        "m4b",
				PublishedAtMS: 30,
			}))
			if _, err := f.service.Jump(ctx, bmMissing, snapA.Session, "Unsafe", 10*time.Minute); !errors.Is(err, ErrMediaMissing) {
				t.Fatalf("%s: expected ErrMediaMissing, got %v", tc.name, err)
			}
		})
	}

	// 4. Unsafe downloads/ directory mode -> ErrMediaMissing.
	validRel := "downloads/valid.m4b"
	f.writePrivateMedia(t, validRel)
	must(t, f.store.PutLocalMedia(ctx, store.LocalMedia{Key: bookMissing, RelativePath: validRel, Format: "m4b", PublishedAtMS: 40}))
	must(t, os.Chmod(filepath.Join(f.rootDir, "downloads"), 0o755))
	if _, err := f.service.Jump(ctx, bmMissing, snapA.Session, "UnsafeDir", 10*time.Minute); !errors.Is(err, ErrMediaMissing) {
		t.Fatalf("expected ErrMediaMissing for 0755 downloads dir, got %v", err)
	}
	must(t, os.Chmod(filepath.Join(f.rootDir, "downloads"), 0o700))

	// 5. Service without RootOwner or MediaChecker fails closed with ErrMediaMissing.
	noCheckerSvc, err := NewService(Options{Store: f.store, Player: f.player})
	must(t, err)
	if _, err := noCheckerSvc.Jump(ctx, bmMissing, snapA.Session, "NoChecker", 10*time.Minute); !errors.Is(err, ErrMediaMissing) {
		t.Fatalf("expected ErrMediaMissing without verifier, got %v", err)
	}

	// 6. Audio changes behind dialog DURING MediaChecker verification -> ErrStaleSession without Load.
	bookC := store.BookKey{Provider: "local", Namespace: "user", ID: "book-c"}
	pathC := f.writePrivateMedia(t, "downloads/bookC.m4b")
	midChangeSvc, err := NewService(Options{
		Store:  f.store,
		Player: f.player,
		MediaChecker: stubMediaChecker{fn: func(ctx context.Context, media store.LocalMedia) (string, error) {
			// Simulate another action loading Book C while confirmation/check is in flight.
			_, loadErr := f.player.Load(ctx, playback.Track{Key: bookC, Title: "Book C", Path: pathC}, true)
			must(t, loadErr)
			return filepath.Join(f.rootDir, filepath.FromSlash(media.RelativePath)), nil
		}},
	})
	must(t, err)
	if _, err := midChangeSvc.Jump(ctx, bmMissing, snapA.Session, "Book B", 10*time.Minute); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("expected ErrStaleSession when audio changed mid-check, got %v", err)
	}
	if cur := f.player.Snapshot(); cur.Book == nil || *cur.Book != bookC {
		t.Fatalf("expected Book C preserved after stale mid-check rejection, got %+v", cur)
	}

	// 7. Audio stops behind dialog DURING MediaChecker verification -> ErrStaleSession without Load.
	curSnap := f.player.Snapshot()
	midStopSvc, err := NewService(Options{
		Store:  f.store,
		Player: f.player,
		MediaChecker: stubMediaChecker{fn: func(ctx context.Context, media store.LocalMedia) (string, error) {
			_, stopErr := f.player.Stop(ctx)
			must(t, stopErr)
			return filepath.Join(f.rootDir, filepath.FromSlash(media.RelativePath)), nil
		}},
	})
	must(t, err)
	if _, err := midStopSvc.Jump(ctx, bmMissing, curSnap.Session, "Book B", 10*time.Minute); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("expected ErrStaleSession when audio stopped mid-check, got %v", err)
	}
}

func TestStoreFaultsDraftRetentionAndEdgeCases(t *testing.T) {
	f := openBookmarkFixture(t)
	ctx := context.Background()

	bookA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	bookB := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	pathA := f.writePrivateMedia(t, "downloads/bookA.m4b")
	pathB := f.writePrivateMedia(t, "downloads/bookB.m4b")
	must(t, f.store.PutLocalMedia(ctx, store.LocalMedia{Key: bookA, RelativePath: "downloads/bookA.m4b", Format: "m4b", PublishedAtMS: 10}))
	must(t, f.store.PutLocalMedia(ctx, store.LocalMedia{Key: bookB, RelativePath: "downloads/bookB.m4b", Format: "m4b", PublishedAtMS: 20}))

	// NewService validation.
	if _, err := NewService(Options{Player: f.player}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("expected ErrInvalidInput for nil Store")
	}
	if _, err := NewService(Options{Store: f.store}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("expected ErrInvalidInput for nil Player")
	}

	// Store transaction error on SaveNew retains draft and allows retry.
	fs := &faultBookmarkStore{inner: f.store, putErr: store.ErrDatabase}
	faultSvc, err := NewService(Options{Store: fs, Player: f.player, RootOwner: f.owner})
	must(t, err)

	snapA, err := f.player.Load(ctx, playback.Track{Key: bookA, Title: "Orchard", Path: pathA, Duration: 20 * time.Minute}, true)
	must(t, err)
	f.backend.setPos(4 * time.Minute)
	_, err = f.player.Poll(ctx)
	must(t, err)

	draft, err := faultSvc.CaptureDefaultDraft(ctx, nil)
	must(t, err)
	if _, err := faultSvc.SaveNew(ctx, draft, "Retryable note", f.clock.MS()); !errors.Is(err, store.ErrDatabase) {
		t.Fatalf("expected store.ErrDatabase, got %v", err)
	}
	if items, err := f.store.Bookmarks(ctx, bookA); err != nil || len(items) != 0 {
		t.Fatalf("failed SaveNew persisted partial state: items=%+v err=%v", items, err)
	}
	// Retry succeeds with retained draft.
	fs.putErr = nil
	saved, err := faultSvc.SaveNew(ctx, draft, "Retryable note", f.clock.Advance(time.Second))
	must(t, err)

	// Store errors on EditNote (Bookmarks error and PutBookmark error), Delete, List, LocalMedia.
	fs.listErr = store.ErrDatabase
	if _, err := faultSvc.EditNote(ctx, bookA, saved.ID, "Updated", f.clock.MS()); !errors.Is(err, store.ErrDatabase) {
		t.Fatalf("expected store.ErrDatabase on EditNote list, got %v", err)
	}
	if _, err := faultSvc.List(ctx, bookA); !errors.Is(err, store.ErrDatabase) {
		t.Fatalf("expected store.ErrDatabase on List, got %v", err)
	}
	fs.listErr = nil

	fs.putErr = store.ErrDatabase
	if _, err := faultSvc.EditNote(ctx, bookA, saved.ID, "Updated", f.clock.Advance(time.Second)); !errors.Is(err, store.ErrDatabase) {
		t.Fatalf("expected store.ErrDatabase on EditNote put, got %v", err)
	}
	fs.putErr = nil
	// Original note preserved after failed EditNote.
	gotList, err := faultSvc.List(ctx, bookA)
	must(t, err)
	if len(gotList) != 1 || gotList[0].Note != "Retryable note" {
		t.Fatalf("failed EditNote mutated bookmark: %+v", gotList)
	}

	fs.deleteErr = store.ErrDatabase
	if err := faultSvc.Delete(ctx, bookA, saved.ID); !errors.Is(err, store.ErrDatabase) {
		t.Fatalf("expected store.ErrDatabase on Delete, got %v", err)
	}
	fs.deleteErr = nil

	fs.mediaErr = store.ErrDatabase
	if _, err := faultSvc.Jump(ctx, store.Bookmark{Key: bookB, PositionNS: 10}, snapA.Session, "Rivers", 10*time.Minute); !errors.Is(err, store.ErrDatabase) {
		t.Fatalf("expected store.ErrDatabase on LocalMedia, got %v", err)
	}
	fs.mediaErr = nil

	// List sorts unsorted mock store output deterministically by PositionNS then ID.
	unsortedStore := &faultBookmarkStore{inner: f.store}
	unsortedSvc, err := NewService(Options{
		Store: &unsortedMockStore{items: []store.Bookmark{
			{ID: "bm-b", Key: bookA, PositionNS: 200, Note: "2", CreatedAtMS: 1, UpdatedAtMS: 1},
			{ID: "bm-c", Key: bookA, PositionNS: 100, Note: "1c", CreatedAtMS: 1, UpdatedAtMS: 1},
			{ID: "bm-a", Key: bookA, PositionNS: 100, Note: "1a", CreatedAtMS: 1, UpdatedAtMS: 1},
		}},
		Player: f.player,
	})
	must(t, err)
	_ = unsortedStore
	sorted, err := unsortedSvc.List(ctx, bookA)
	must(t, err)
	if len(sorted) != 3 || sorted[0].ID != "bm-a" || sorted[1].ID != "bm-c" || sorted[2].ID != "bm-b" {
		t.Fatalf("List did not sort deterministically: %+v", sorted)
	}

	// NewID failure and invalid ID rejection.
	idErr := errors.New("entropy failed")
	badIDSvc, err := NewService(Options{
		Store:  f.store,
		Player: f.player,
		NewID:  func() (string, error) { return "", idErr },
	})
	must(t, err)
	if _, err := badIDSvc.SaveNew(ctx, draft, "Note", f.clock.MS()); !errors.Is(err, idErr) {
		t.Fatalf("expected idErr, got %v", err)
	}
	badIDSvc2, err := NewService(Options{
		Store:  f.store,
		Player: f.player,
		NewID:  func() (string, error) { return " bad\nid ", nil },
	})
	must(t, err)
	if _, err := badIDSvc2.SaveNew(ctx, draft, "Note", f.clock.MS()); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for invalid generated ID, got %v", err)
	}

	// Nil/canceled contexts and invalid inputs across all methods.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, err := range []error{
		func() error { _, e := f.service.CaptureDraft(nil, 0, nil); return e }(),
		func() error { _, e := f.service.CaptureDraft(canceled, 0, nil); return e }(),
		func() error { _, e := f.service.SaveNew(nil, draft, "x", 0); return e }(),
		func() error { _, e := f.service.SaveNew(ctx, draft, "x", -1); return e }(),
		func() error { _, e := f.service.SaveNew(canceled, draft, "x", 0); return e }(),
		func() error { _, e := f.service.SaveNew(ctx, Draft{}, "x", 0); return e }(),
		func() error { _, e := f.service.SaveNew(ctx, Draft{BookKey: bookA, PreRoll: -1}, "x", 0); return e }(),
		func() error {
			_, e := f.service.SaveNew(ctx, Draft{BookKey: bookA, CapturedChapter: &ChapterRef{Title: "bad\n"}}, "x", 0)
			return e
		}(),
		func() error {
			_, e := f.service.SaveNew(ctx, Draft{BookKey: bookA, JumpChapter: &ChapterRef{ID: "bad\x00"}}, "x", 0)
			return e
		}(),
		func() error { _, e := f.service.EditNote(nil, bookA, "id", "x", 0); return e }(),
		func() error { _, e := f.service.EditNote(ctx, bookA, "id", "x", -1); return e }(),
		func() error { _, e := f.service.EditNote(canceled, bookA, "id", "x", 0); return e }(),
		func() error { _, e := f.service.EditNote(ctx, store.BookKey{}, "id", "x", 0); return e }(),
		func() error { _, e := f.service.EditNote(ctx, bookA, "", "x", 0); return e }(),
		func() error { _, e := f.service.EditNote(ctx, bookA, "id", "   ", 0); return e }(),
		f.service.Delete(nil, bookA, "id"),
		f.service.Delete(canceled, bookA, "id"),
		f.service.Delete(ctx, store.BookKey{}, "id"),
		f.service.Delete(ctx, bookA, ""),
		func() error { _, e := f.service.List(nil, bookA); return e }(),
		func() error { _, e := f.service.List(canceled, bookA); return e }(),
		func() error { _, e := f.service.List(ctx, store.BookKey{}); return e }(),
		func() error { _, e := f.service.Jump(nil, saved, snapA.Session, "A", 0); return e }(),
		func() error { _, e := f.service.Jump(canceled, saved, snapA.Session, "A", 0); return e }(),
		func() error { _, e := f.service.Jump(ctx, store.Bookmark{}, snapA.Session, "A", 0); return e }(),
		func() error {
			_, e := f.service.Jump(ctx, store.Bookmark{Key: bookA, PositionNS: -1}, snapA.Session, "A", 0)
			return e
		}(),
		func() error { _, e := f.service.Jump(ctx, saved, snapA.Session, "A", -1); return e }(),
		func() error { _, e := f.service.Jump(ctx, saved, snapA.Session, "Bad\nTitle", 0); return e }(),
		func() error {
			_, e := f.service.Jump(ctx, store.Bookmark{Key: bookB, PositionNS: 0}, snapA.Session, "   ", 0)
			return e
		}(),
	} {
		if err == nil {
			t.Fatal("expected error for invalid/canceled call")
		}
	}

	// Stub player edge cases: StateLoading, nil Book, Session==0, invalid snapshot BookKey, StateFailed same-book jump, Load error, SeekAbsolute errors.
	for _, noAudioSnap := range []playback.Snapshot{
		{State: playback.StateLoading, Session: 1, Book: &bookA},
		{State: playback.StatePlaying, Session: 1, Book: nil},
		{State: playback.StatePlaying, Session: 0, Book: &bookA},
	} {
		noAudioSvc, err := NewService(Options{Store: f.store, Player: &stubPlayer{snap: noAudioSnap}})
		must(t, err)
		if _, err := noAudioSvc.CaptureDraft(ctx, 0, nil); !errors.Is(err, ErrNoLoadedAudio) {
			t.Fatalf("expected ErrNoLoadedAudio for %+v, got %v", noAudioSnap, err)
		}
	}

	badKeyPlayer := &stubPlayer{snap: playback.Snapshot{
		State:   playback.StatePlaying,
		Session: 1,
		Book:    &store.BookKey{Provider: "BAD"},
	}}
	badKeySvc, err := NewService(Options{Store: f.store, Player: badKeyPlayer})
	must(t, err)
	if _, err := badKeySvc.CaptureDraft(ctx, 0, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for invalid snapshot BookKey, got %v", err)
	}

	zeroSessionSameBook := &stubPlayer{snap: playback.Snapshot{
		State:   playback.StatePlaying,
		Session: 0,
		Book:    &bookA,
	}}
	zeroSessionSvc, err := NewService(Options{Store: f.store, Player: zeroSessionSameBook})
	must(t, err)
	if _, err := zeroSessionSvc.Jump(ctx, saved, 0, "Orchard", 0); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("expected ErrStaleSession for zero session same-book jump, got %v", err)
	}

	failedSameBookPlayer := &stubPlayer{snap: playback.Snapshot{
		State:   playback.StateFailed,
		Session: 3,
		Book:    &bookA,
	}}
	failedSameBookSvc, err := NewService(Options{Store: f.store, Player: failedSameBookPlayer})
	must(t, err)
	if _, err := failedSameBookSvc.Jump(ctx, saved, 3, "Orchard", 0); !errors.Is(err, ErrNoLoadedAudio) {
		t.Fatalf("expected ErrNoLoadedAudio for StateFailed same-book jump, got %v", err)
	}

	// Same-book SeekAbsolute non-stale error (e.g. ErrIPC) propagates cleanly.
	seekErrPlayer := &stubPlayer{
		snap: playback.Snapshot{State: playback.StatePlaying, Session: 4, Book: &bookA},
		seekFn: func(context.Context, uint64, time.Duration) (playback.Snapshot, error) {
			return playback.Snapshot{State: playback.StateFailed}, playback.ErrIPC
		},
	}
	seekErrSvc, err := NewService(Options{Store: f.store, Player: seekErrPlayer})
	must(t, err)
	if _, err := seekErrSvc.Jump(ctx, saved, 4, "Orchard", 0); !errors.Is(err, playback.ErrIPC) {
		t.Fatalf("expected playback.ErrIPC, got %v", err)
	}

	// Mid-check book change with unchanged session number triggers sameSessionContext book mismatch.
	bookSwapPlayer := &stubPlayer{snap: playback.Snapshot{State: playback.StatePlaying, Session: 7, Book: &bookA}}
	bookSwapSvc, err := NewService(Options{
		Store:  f.store,
		Player: bookSwapPlayer,
		MediaChecker: stubMediaChecker{fn: func(context.Context, store.LocalMedia) (string, error) {
			bookSwapPlayer.snap.Book = &bookB
			return pathB, nil
		}},
	})
	must(t, err)
	if _, err := bookSwapSvc.Jump(ctx, store.Bookmark{Key: bookB, PositionNS: 10}, 7, "Rivers", 10*time.Minute); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("expected ErrStaleSession on mid-check book swap, got %v", err)
	}

	loadErr := errors.New("load failed")
	loadFailPlayer := &stubPlayer{
		snap: playback.Snapshot{State: playback.StateIdle, Session: 0},
		loadFn: func(context.Context, playback.Track, bool) (playback.Snapshot, error) {
			return playback.Snapshot{State: playback.StateFailed}, loadErr
		},
	}
	loadFailSvc, err := NewService(Options{Store: f.store, Player: loadFailPlayer, RootOwner: f.owner})
	must(t, err)
	if _, err := loadFailSvc.Jump(ctx, store.Bookmark{Key: bookB, PositionNS: 10}, 0, "Rivers", 10*time.Minute); !errors.Is(err, loadErr) {
		t.Fatalf("expected loadErr, got %v", err)
	}

	seekStalePlayer := &stubPlayer{
		snap: playback.Snapshot{State: playback.StateIdle, Session: 0},
		loadFn: func(context.Context, playback.Track, bool) (playback.Snapshot, error) {
			return playback.Snapshot{State: playback.StatePlaying, Session: 5, Book: &bookB}, nil
		},
		seekFn: func(context.Context, uint64, time.Duration) (playback.Snapshot, error) {
			return playback.Snapshot{}, playback.ErrStaleSession
		},
	}
	seekStaleSvc, err := NewService(Options{Store: f.store, Player: seekStalePlayer, RootOwner: f.owner})
	must(t, err)
	if _, err := seekStaleSvc.Jump(ctx, store.Bookmark{Key: bookB, PositionNS: 10}, 0, "Rivers", 10*time.Minute); !errors.Is(err, ErrStaleSession) || !errors.Is(err, playback.ErrStaleSession) {
		t.Fatalf("expected wrapped ErrStaleSession, got %v", err)
	}

	// Custom MediaChecker returning invalid paths fails closed with ErrMediaMissing.
	for _, badResolved := range []string{"relative/path.m4b", "/tmp/book.mp3", "/tmp/.hidden.m4b", "/tmp/" + string([]byte{0xff}) + ".m4b"} {
		badPathSvc, err := NewService(Options{
			Store:  f.store,
			Player: f.player,
			MediaChecker: stubMediaChecker{fn: func(context.Context, store.LocalMedia) (string, error) {
				return badResolved, nil
			}},
		})
		must(t, err)
		if _, err := badPathSvc.Jump(ctx, store.Bookmark{Key: bookB, PositionNS: 10}, f.player.Snapshot().Session, "Rivers", 10*time.Minute); !errors.Is(err, ErrMediaMissing) {
			t.Fatalf("expected ErrMediaMissing for invalid MediaChecker path %q, got %v", badResolved, err)
		}
	}

	// Checkpoint failure on old book blocks cross-book switch and preserves old book.
	_ = pathB
	checkpointFailStore := &faultCheckpointStore{inner: f.store, putErr: errors.New("checkpoint write failed")}
	cpCtrl, err := playback.NewController(&fakeBackend{dur: 20 * time.Minute}, playback.Options{Store: checkpointFailStore})
	must(t, err)
	defer func() { _ = cpCtrl.Close(ctx) }()
	checkpointFailStore.putErr = nil
	cpSnapA, err := cpCtrl.Load(ctx, playback.Track{Key: bookA, Title: "Orchard", Path: pathA, Duration: 20 * time.Minute}, true)
	must(t, err)
	checkpointFailStore.putErr = errors.New("checkpoint write failed")
	cpSvc, err := NewService(Options{Store: f.store, Player: cpCtrl, RootOwner: f.owner})
	must(t, err)
	if _, err := cpSvc.Jump(ctx, store.Bookmark{Key: bookB, PositionNS: 10}, cpSnapA.Session, "Rivers", 20*time.Minute); err == nil {
		t.Fatal("expected checkpoint failure to block cross-book switch")
	}
	if cur := cpCtrl.Snapshot(); cur.Book == nil || *cur.Book != bookA {
		t.Fatalf("expected Book A preserved on checkpoint failure, got %+v", cur)
	}
}

func TestRootMediaCheckerFaultsAndEditTokenCancellation(t *testing.T) {
	f := openBookmarkFixture(t)
	ctx := context.Background()
	bookB := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	f.writePrivateMedia(t, "downloads/bookB.m4b")
	validMedia := store.LocalMedia{Key: bookB, RelativePath: "downloads/bookB.m4b", Format: "m4b", PublishedAtMS: 10}

	// Canceled context and invalid LocalMedia.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	checker := &rootMediaChecker{owner: f.owner, ops: defaultSysOps()}
	if _, err := checker.VerifyMedia(canceled, validMedia); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if _, err := checker.VerifyMedia(ctx, store.LocalMedia{Key: bookB, RelativePath: "downloads/../escape.m4b", Format: "m4b"}); !errors.Is(err, ErrMediaMissing) {
		t.Fatalf("expected ErrMediaMissing for invalid LocalMedia, got %v", err)
	}

	// RootOwner lease errors: canceled lease context, nil lease, closed owner.
	canceledLeaseChecker := &rootMediaChecker{
		owner: stubRootOwner{fn: func(ctx context.Context) (*store.RootLease, error) {
			lctx, lcancel := context.WithCancel(ctx)
			lease, err := f.owner.Lease(lctx)
			lcancel()
			return lease, err
		}},
		ops: defaultSysOps(),
	}
	if _, err := canceledLeaseChecker.VerifyMedia(ctx, validMedia); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled from lease.Context, got %v", err)
	}

	nilLeaseChecker := &rootMediaChecker{
		owner: stubRootOwner{fn: func(context.Context) (*store.RootLease, error) {
			return nil, nil
		}},
		ops: defaultSysOps(),
	}
	if _, err := nilLeaseChecker.VerifyMedia(ctx, validMedia); !errors.Is(err, ErrMediaMissing) {
		t.Fatalf("expected ErrMediaMissing for nil lease, got %v", err)
	}

	leaseErrChecker := &rootMediaChecker{
		owner: stubRootOwner{fn: func(context.Context) (*store.RootLease, error) {
			return nil, store.ErrClosed
		}},
		ops: defaultSysOps(),
	}
	if _, err := leaseErrChecker.VerifyMedia(ctx, validMedia); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("expected store.ErrClosed, got %v", err)
	}
	if _, err := leaseErrChecker.VerifyMedia(canceled, validMedia); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// Also test Lease returning error when ctx cancels inside Lease.
	ctxCancelInLease, cancelInLease := context.WithCancel(ctx)
	cancelInLeaseChecker := &rootMediaChecker{
		owner: stubRootOwner{fn: func(context.Context) (*store.RootLease, error) {
			cancelInLease()
			return nil, errors.New("aborted")
		}},
		ops: defaultSysOps(),
	}
	if _, err := cancelInLeaseChecker.VerifyMedia(ctxCancelInLease, validMedia); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled when canceled during Lease, got %v", err)
	}

	// Injected sysOps faults: root fstat error, root UID mismatch, subdir fstat error, unlinked readlink.
	for _, tc := range []struct {
		name string
		ops  sysOps
	}{
		{
			name: "root-fstat-err",
			ops: sysOps{
				fstat:    func(int, *unix.Stat_t) error { return unix.EIO },
				fstatat:  unix.Fstatat,
				readlink: os.Readlink,
			},
		},
		{
			name: "root-uid-mismatch",
			ops: sysOps{
				fstat: func(fd int, st *unix.Stat_t) error {
					if err := unix.Fstat(fd, st); err != nil {
						return err
					}
					st.Uid++
					return nil
				},
				fstatat:  unix.Fstatat,
				readlink: os.Readlink,
			},
		},
		{
			name: "subdir-fstat-err",
			ops: sysOps{
				fstat: func() func(int, *unix.Stat_t) error {
					calls := 0
					return func(fd int, st *unix.Stat_t) error {
						calls++
						if calls == 2 {
							return unix.EIO
						}
						return unix.Fstat(fd, st)
					}
				}(),
				fstatat:  unix.Fstatat,
				readlink: os.Readlink,
			},
		},
		{
			name: "unlinked-readlink",
			ops: sysOps{
				fstat:   unix.Fstat,
				fstatat: unix.Fstatat,
				readlink: func(string) (string, error) {
					return "/tmp/taletty/downloads (deleted)", nil
				},
			},
		},
		{
			name: "unsafe-readlink-control",
			ops: sysOps{
				fstat:   unix.Fstat,
				fstatat: unix.Fstatat,
				readlink: func(string) (string, error) {
					return "/tmp/taletty\n/downloads", nil
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &rootMediaChecker{owner: f.owner, ops: tc.ops}
			if _, err := c.VerifyMedia(ctx, validMedia); !errors.Is(err, ErrMediaMissing) {
				t.Fatalf("%s: expected ErrMediaMissing, got %v", tc.name, err)
			}
		})
	}

	// EditToken cancellation while another EditNote/Delete holds the token (both Delete and EditNote).
	entered := make(chan struct{})
	release := make(chan struct{})
	blockingStore := &faultBookmarkStore{
		inner: f.store,
		onList: func() {
			close(entered)
			<-release
		},
	}
	blockingSvc, err := NewService(Options{Store: blockingStore, Player: f.player})
	must(t, err)

	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = blockingSvc.EditNote(ctx, bookB, "bm-1", "Note", 10)
	})
	<-entered

	waitCtx, waitCancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer waitCancel()
	if err := blockingSvc.Delete(waitCtx, bookB, "bm-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded waiting on editToken in Delete, got %v", err)
	}
	waitCtx2, waitCancel2 := context.WithTimeout(ctx, 20*time.Millisecond)
	defer waitCancel2()
	if _, err := blockingSvc.EditNote(waitCtx2, bookB, "bm-1", "Note2", 10); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded waiting on editToken in EditNote, got %v", err)
	}
	if err := blockingSvc.acquireEditToken(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled in acquireEditToken, got %v", err)
	}
	close(release)
	wg.Wait()
}

func TestConcurrentBookmarkOperationsRace(t *testing.T) {
	f := openBookmarkFixture(t)
	ctx := context.Background()

	bookA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	bookB := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	pathA := f.writePrivateMedia(t, "downloads/bookA.m4b")
	_ = f.writePrivateMedia(t, "downloads/bookB.m4b")
	must(t, f.store.PutLocalMedia(ctx, store.LocalMedia{Key: bookA, RelativePath: "downloads/bookA.m4b", Format: "m4b", PublishedAtMS: 10}))
	must(t, f.store.PutLocalMedia(ctx, store.LocalMedia{Key: bookB, RelativePath: "downloads/bookB.m4b", Format: "m4b", PublishedAtMS: 20}))

	snapA, err := f.player.Load(ctx, playback.Track{Key: bookA, Title: "Orchard", Path: pathA, Duration: 30 * time.Minute}, true)
	must(t, err)

	seedDraft, err := f.service.CaptureDefaultDraft(ctx, nil)
	must(t, err)
	seedBM, err := f.service.SaveNew(ctx, seedDraft, "Seed", f.clock.MS())
	must(t, err)

	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			d, err := f.service.CaptureDefaultDraft(ctx, nil)
			if err != nil && !errors.Is(err, ErrNoLoadedAudio) {
				t.Errorf("capture %d: %v", i, err)
				return
			}
			if err == nil {
				bm, saveErr := f.service.SaveNew(ctx, d, fmt.Sprintf("Note %d", i), f.clock.Advance(time.Millisecond))
				if saveErr != nil {
					t.Errorf("save %d: %v", i, saveErr)
					return
				}
				_, _ = f.service.EditNote(ctx, bookA, bm.ID, fmt.Sprintf("Edited %d", i), f.clock.Advance(time.Millisecond))
				_ = f.service.Delete(ctx, bookA, bm.ID)
			}
			if _, listErr := f.service.List(ctx, bookA); listErr != nil {
				t.Errorf("list %d: %v", i, listErr)
			}
		})
	}
	wg.Go(func() {
		_, _ = f.service.Jump(ctx, seedBM, snapA.Session, "Orchard", 30*time.Minute)
	})
	wg.Wait()
}

type unsortedMockStore struct {
	items []store.Bookmark
}

func (u *unsortedMockStore) Bookmarks(context.Context, store.BookKey) ([]store.Bookmark, error) {
	return u.items, nil
}

func (u *unsortedMockStore) PutBookmark(context.Context, store.Bookmark) error { return nil }
func (u *unsortedMockStore) DeleteBookmark(context.Context, store.BookKey, string) error {
	return nil
}
func (u *unsortedMockStore) LocalMedia(context.Context, store.BookKey) (store.LocalMedia, bool, error) {
	return store.LocalMedia{}, false, nil
}

type faultCheckpointStore struct {
	inner  playback.CheckpointStore
	putErr error
}

func (f *faultCheckpointStore) ListeningState(ctx context.Context, key store.BookKey) (store.ListeningState, bool, error) {
	return f.inner.ListeningState(ctx, key)
}

func (f *faultCheckpointStore) PutListeningState(ctx context.Context, state store.ListeningState) error {
	if f.putErr != nil {
		return f.putErr
	}
	return f.inner.PutListeningState(ctx, state)
}
