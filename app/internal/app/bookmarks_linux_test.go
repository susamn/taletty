package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/bookmarks"
	"github.com/susamn/taletty/app/internal/playback"
	"github.com/susamn/taletty/app/internal/profile"
	"github.com/susamn/taletty/app/internal/store"
	"github.com/susamn/taletty/app/internal/testkit"
	"github.com/susamn/taletty/app/internal/ui"
)

type faultBookmarkStore struct {
	*store.Store
	listErr error
	putErr  error
	delErr  error
}

func (f *faultBookmarkStore) Bookmarks(ctx context.Context, key store.BookKey) ([]store.Bookmark, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.Store.Bookmarks(ctx, key)
}

func (f *faultBookmarkStore) PutBookmark(ctx context.Context, b store.Bookmark) error {
	if f.putErr != nil {
		return f.putErr
	}
	return f.Store.PutBookmark(ctx, b)
}

func (f *faultBookmarkStore) DeleteBookmark(ctx context.Context, key store.BookKey, bookmarkID string) error {
	if f.delErr != nil {
		return f.delErr
	}
	return f.Store.DeleteBookmark(ctx, key, bookmarkID)
}

func sendUIKeys(t *testing.T, r *testkit.Runner, names ...string) {
	t.Helper()
	for _, name := range names {
		if name == "ctrl+s" {
			if err := r.Send(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if name == "ctrl+u" {
			if err := r.Send(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		sendUIKey(t, r, name)
	}
}

func setupBookmarkWorkspace(t *testing.T) (context.Context, *profile.Workspace, *store.Store, *fakePlaybackBackend, *playback.Controller, store.BookKey, store.BookKey, store.BookKey) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	ws, err := profile.Open(ctx, home, 1000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close(context.Background()) })

	st := ws.Store()
	kA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B00ORCHARD"}
	kB := store.BookKey{Provider: "audible", Namespace: "us", ID: "B00RIVERS1"}
	kC := store.BookKey{Provider: "audible", Namespace: "us", ID: "B00MISSING"}
	durA := int64((30 * time.Minute).Nanoseconds())
	durB := int64((25 * time.Minute).Nanoseconds())
	durC := int64((20 * time.Minute).Nanoseconds())
	if err := st.ReplaceCatalog(ctx, "audible", "us", []store.CatalogBook{
		{Key: kA, Title: "The Glass Orchard", Authors: []store.Contributor{{Name: "Mara Vale"}}, DurationNS: &durA},
		{Key: kB, Title: "A Map of Quiet Rivers", Authors: []store.Contributor{{Name: "Ilan Reed"}}, DurationNS: &durB},
		{Key: kC, Title: "Missing Audio Book", Authors: []store.Contributor{{Name: "No Media"}}, DurationNS: &durC},
	}, 1000); err != nil {
		t.Fatal(err)
	}

	for _, item := range []struct {
		key  store.BookKey
		name string
	}{
		{kA, "orchard.m4b"},
		{kB, "rivers.m4b"},
	} {
		abs := filepath.Join(ws.DownloadsPath(), item.name)
		if err := os.WriteFile(abs, []byte("fake-m4b"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := st.PutLocalMedia(ctx, store.LocalMedia{
			Key:           item.key,
			RelativePath:  filepath.Join("downloads", item.name),
			Format:        "m4b",
			PublishedAtMS: 1000,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.PutListeningState(ctx, store.ListeningState{
		Key:         kB,
		PositionNS:  int64((5 * time.Minute).Nanoseconds()),
		SpeedMilli:  1_250_000,
		Completion:  store.CompletionUnfinished,
		UpdatedAtMS: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetFavorite(ctx, kB, true, 1000); err != nil {
		t.Fatal(err)
	}

	backend := &fakePlaybackBackend{}
	player, err := playback.NewController(backend, playback.Options{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = player.Close(context.Background()) })

	return ctx, ws, st, backend, player, kA, kB, kC
}

func TestJ09BookmarkCaptureBoundaryDelayedSaveEditDeleteAndOffline(t *testing.T) {
	ctx, ws, st, backend, player, kA, kB, _ := setupBookmarkWorkspace(t)
	now := time.UnixMilli(5000)
	chapters := fakeChapterReader{
		byPath: map[string][]Chapter{
			filepath.Join(ws.DownloadsPath(), "orchard.m4b"): {
				{Title: "Ch 1 Opening", Start: 0, End: 60 * time.Second},
				{Title: "Ch 2 Across the gate", Start: 60 * time.Second, End: 30 * time.Minute},
			},
			filepath.Join(ws.DownloadsPath(), "rivers.m4b"): {
				{Title: "Ch 1 Map", Start: 0, End: 25 * time.Minute},
			},
		},
	}
	faultStore := &faultBookmarkStore{Store: st}
	coord, err := NewCoordinator(ctx, faultStore, Options{
		Player:        player,
		MediaChecker:  NewRootMediaChecker(ws.Owner()),
		ChapterReader: chapters,
		Clock:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer coord.Close(context.Background())

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

	// 1. No loaded audio -> b shows unavailable notice, never falls back to highlighted book.
	sendUIKeys(t, r, "b")
	if !strings.Contains(r.Model.View().Content, "Bookmark capture unavailable: no loaded audio") {
		t.Fatalf("expected unavailable capture notice:\n%s", r.Model.View().Content)
	}

	// 2. Play Orchard (kA), advance to 61s (Ch 2), pause, highlight Rivers (kB), press b.
	// Pre-roll (2s) crosses chapter boundary back to 59s (Ch 1 Opening).
	encA, _ := EncodeBookKey(kA)
	encB, _ := EncodeBookKey(kB)
	sendUIKeys(t, r, "G", "enter") // SortTitle: Rivers first, Orchard last
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	backend.pos = 61 * time.Second
	backend.mu.Unlock()
	snap, ok := coord.PollPlayback(ctx)
	if !ok {
		t.Fatal("expected PollPlayback snapshot")
	}
	if err := r.Send(snap); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, " ", "g") // pause Orchard, highlight Rivers
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if r.Model.(ui.Model).Selection() != encB {
		t.Fatalf("expected Rivers (%s) highlighted, got %q", encB, r.Model.(ui.Model).Selection())
	}
	sendUIKeys(t, r, "b")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	view := r.Model.View().Content
	if !strings.Contains(view, "The Glass Orchard") || !strings.Contains(view, "Cap: Ch 2 Across the gate -> Jump: Ch 1 Opening") || !strings.Contains(view, "Captured 00:01:01 | Jump 00:00:59") {
		t.Fatalf("capture editor missing frozen target or chapter boundary:\n%s", view)
	}

	// 3. Delayed save: advance playback AND switch to Rivers while editor is open!
	backend.mu.Lock()
	backend.pos = 15 * time.Minute
	backend.mu.Unlock()
	if _, err := player.Load(ctx, playback.Track{
		Key: kB, Title: "A Map of Quiet Rivers", Path: filepath.Join(ws.DownloadsPath(), "rivers.m4b"), Duration: 25 * time.Minute,
	}, true); err != nil {
		t.Fatal(err)
	}

	// Type multiline note with Alt+Enter and ANSI paste; inject PutBookmark failure first, then retry.
	faultStore.putErr = store.ErrDatabase
	if err := r.Send(tea.PasteMsg{Content: "Line one\x1b[2J"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Send(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}); err != nil {
		t.Fatal(err)
	}
	if err := r.Send(tea.PasteMsg{Content: "Line two"}); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Line two") || !strings.Contains(r.Model.View().Content, "Operation failed.") {
		t.Fatalf("save failure did not retain draft and error:\n%s", r.Model.View().Content)
	}

	// Retry save -> succeeds on frozen Orchard at 59s (Ch 1 Opening), NOT Rivers at 15m!
	faultStore.putErr = nil
	now = now.Add(time.Second)
	sendUIKeys(t, r, "ctrl+s")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Model.View().Content, "[Focus] Note") {
		t.Fatalf("editor did not close after durable save:\n%s", r.Model.View().Content)
	}
	bmsA, err := st.Bookmarks(ctx, kA)
	if err != nil || len(bmsA) != 1 {
		t.Fatalf("expected 1 bookmark on Orchard, got %+v err=%v", bmsA, err)
	}
	if bmsA[0].PositionNS != int64((59*time.Second).Nanoseconds()) || bmsA[0].Note != "Line one\nLine two" || bmsA[0].ChapterTitle == nil || *bmsA[0].ChapterTitle != "Ch 1 Opening" {
		t.Fatalf("delayed save retargeted or corrupted bookmark: %+v", bmsA[0])
	}
	bmsB, _ := st.Bookmarks(ctx, kB)
	if len(bmsB) != 0 {
		t.Fatalf("delayed save leaked bookmark onto Rivers: %+v", bmsB)
	}

	// Duplicate/late SaveNote for same RequestID must NOT create a second bookmark.
	dupMsg := coord.Dispatch(ui.Action{
		Kind:      ui.SaveNote,
		Key:       encA,
		Target:    ui.NoteTarget{Key: encA},
		Text:      "Duplicate",
		RequestID: "bm-capture-1",
		Attempt:   99,
	})()
	if dupMsg == nil {
		t.Fatal("expected duplicate SaveNote reconciliation message")
	}
	bmsA, _ = st.Bookmarks(ctx, kA)
	if len(bmsA) != 1 {
		t.Fatalf("duplicate SaveNote created second bookmark: %+v", bmsA)
	}

	// 4. Pre-roll zero clamp: capture at 1s -> JumpPosition = 0s.
	backend.mu.Lock()
	backend.pos = 1 * time.Second
	backend.mu.Unlock()
	if snap, ok := coord.PollPlayback(ctx); ok {
		_ = r.Send(snap)
	}
	sendUIKeys(t, r, "b")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Captured 00:00:01 | Jump 00:00:00") {
		t.Fatalf("expected zero-clamped jump position:\n%s", r.Model.View().Content)
	}
	now = now.Add(time.Second)
	if err := r.Send(tea.PasteMsg{Content: "Rivers zero clamp"}); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}

	// 5. Open Bookmarks Manager for Orchard (kA) while Rivers (kB) is playing; test query failure + retry.
	faultStore.listErr = store.ErrDatabase
	sendUIKeys(t, r, "G", "B") // Orchard selected
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Bookmarks - The Glass Orchard") || !strings.Contains(r.Model.View().Content, "Failed to load bookmarks") {
		t.Fatalf("query failure did not keep target header:\n%s", r.Model.View().Content)
	}
	faultStore.listErr = nil
	sendUIKeys(t, r, "r")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "00:00:59 | Ch 1 Opening | Line one Line two") {
		t.Fatalf("retry did not load Orchard bookmark:\n%s", r.Model.View().Content)
	}

	// 6. Capture b inside Orchard's manager while Rivers is playing -> captures Rivers, returns to Orchard manager!
	sendUIKeys(t, r, "b")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "A Map of Quiet Rivers") {
		t.Fatalf("b inside manager did not capture current audio Rivers:\n%s", r.Model.View().Content)
	}
	sendUIKeys(t, r, "esc")
	if !r.Model.(ui.Model).BookmarksOpen() || r.Model.(ui.Model).BookmarksTarget() != encA {
		t.Fatal("Esc from capture did not return to unchanged Orchard manager")
	}

	// 7. Edit note on Orchard bookmark without recapturing position or reapplying pre-roll; Esc cancel leaves untouched.
	sendUIKeys(t, r, "x", "j", "enter")
	if err := r.Send(tea.PasteMsg{Content: " ignored"}); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "esc")
	bmsA, _ = st.Bookmarks(ctx, kA)
	if bmsA[0].Note != "Line one\nLine two" {
		t.Fatalf("canceled edit modified note: %+v", bmsA[0])
	}
	now = now.Add(time.Second)
	sendUIKeys(t, r, "x", "j", "enter")
	if err := r.Send(tea.PasteMsg{Content: " (edited)"}); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "ctrl+s")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	bmsA, _ = st.Bookmarks(ctx, kA)
	if bmsA[0].PositionNS != int64((59*time.Second).Nanoseconds()) || !strings.Contains(bmsA[0].Note, "(edited)") {
		t.Fatalf("edit changed position or failed to update note: %+v", bmsA[0])
	}

	// 8. Delete bookmark: Keep cancels, Delete failure reloads authoritative list, Delete confirm removes only bookmark.
	sendUIKeys(t, r, "x", "j", "j", "enter", "enter") // Keep bookmark
	bmsA, _ = st.Bookmarks(ctx, kA)
	if len(bmsA) != 1 {
		t.Fatal("Keep bookmark deleted bookmark")
	}
	faultStore.delErr = store.ErrDatabase
	sendUIKeys(t, r, "x", "j", "j", "enter", "tab", "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Operation failed.") {
		t.Fatalf("delete failure did not stay recoverable:\n%s", r.Model.View().Content)
	}
	faultStore.delErr = nil
	sendUIKeys(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	bmsA, _ = st.Bookmarks(ctx, kA)
	if len(bmsA) != 0 {
		t.Fatalf("expected Orchard bookmark deleted, got %+v", bmsA)
	}
	if !strings.Contains(r.Model.View().Content, "No bookmarks for this book") {
		t.Fatalf("deleting final row did not show empty state:\n%s", r.Model.View().Content)
	}
	// Audio, favorites, and Rivers bookmarks remain intact.
	if _, found, _ := st.LocalMedia(ctx, kA); !found {
		t.Fatal("deleting bookmark removed local media")
	}
	favs, _ := st.Favorites(ctx)
	if len(favs) != 1 || favs[0] != kB {
		t.Fatalf("deleting bookmark altered favorites: %+v", favs)
	}
}

func TestJ07J08J12BookmarkJumpSwitchStaleAndMissingAudio(t *testing.T) {
	ctx, ws, st, backend, player, kA, kB, kC := setupBookmarkWorkspace(t)
	if err := st.PutBookmark(ctx, store.Bookmark{
		ID: "bm-a1", Key: kA, PositionNS: int64((4 * time.Minute).Nanoseconds()),
		Note: "Orchard 4m", CreatedAtMS: 100, UpdatedAtMS: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutBookmark(ctx, store.Bookmark{
		ID: "bm-b1", Key: kB, PositionNS: int64((7 * time.Minute).Nanoseconds()),
		Note: "Rivers 7m", CreatedAtMS: 100, UpdatedAtMS: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutBookmark(ctx, store.Bookmark{
		ID: "bm-c1", Key: kC, PositionNS: int64((2 * time.Minute).Nanoseconds()),
		Note: "Missing 2m", CreatedAtMS: 100, UpdatedAtMS: 100,
	}); err != nil {
		t.Fatal(err)
	}

	coord, err := NewCoordinator(ctx, st, Options{
		Player:       player,
		MediaChecker: NewRootMediaChecker(ws.Owner()),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer coord.Close(context.Background())

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

	// 1. Play Orchard (kA), pause it, open Orchard bookmarks, jump to bm-a1 -> stays Paused at exact 4m!
	sendUIKeys(t, r, "G", "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, " ", "B")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	snap := player.Snapshot()
	if snap.State != playback.StatePaused || snap.Position != 4*time.Minute || *snap.Book != kA {
		t.Fatalf("same-book paused jump failed: %+v", snap)
	}
	sendUIKeys(t, r, "esc")

	// 2. Missing local audio (kC): offers Queue download, never touches Orchard, stays in manager.
	sendUIKeys(t, r, "g", "j", "B") // SortTitle: Rivers, Missing, Orchard
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Audio not downloaded") || !strings.Contains(r.Model.View().Content, "Queue download") {
		t.Fatalf("missing audio jump did not offer Queue download:\n%s", r.Model.View().Content)
	}
	sendUIKeys(t, r, "esc", "esc")
	if snap := player.Snapshot(); *snap.Book != kA || snap.Position != 4*time.Minute {
		t.Fatalf("missing audio jump altered current audio: %+v", snap)
	}

	// 3. Cross-book jump to Rivers (kB) while Orchard is paused: opens B3 Stay / Switch and play.
	sendUIKeys(t, r, "g", "B")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	view := r.Model.View().Content
	if !strings.Contains(view, "Switch audiobook") || !strings.Contains(view, "[Stay]") || !strings.Contains(view, "Switch and play") {
		t.Fatalf("cross-book jump did not open B3 confirmation:\n%s", view)
	}

	// Stale source invalidation: reload Orchard behind open dialog -> confirm rejects stale session!
	if _, err := player.Load(ctx, playback.Track{
		Key: kA, Title: "The Glass Orchard", Path: filepath.Join(ws.DownloadsPath(), "orchard.m4b"), Duration: 30 * time.Minute,
	}, true); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "tab", "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Playback changed; confirm switch again.") {
		t.Fatalf("stale switch confirmation was not rejected:\n%s", r.Model.View().Content)
	}
	sendUIKeys(t, r, "esc")

	// Re-open switch confirmation, test stop/checkpoint failure blocking switch, then succeed!
	backend.mu.Lock()
	backend.stopErr = errors.New("stop failed")
	backend.mu.Unlock()
	sendUIKeys(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "tab", "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Operation failed.") {
		t.Fatalf("failed switch did not keep confirmation recoverable:\n%s", r.Model.View().Content)
	}
	backend.mu.Lock()
	backend.stopErr = nil
	backend.mu.Unlock()
	if _, err := player.Load(ctx, playback.Track{
		Key: kA, Title: "The Glass Orchard", Path: filepath.Join(ws.DownloadsPath(), "orchard.m4b"), Duration: 30 * time.Minute,
	}, true); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "esc", "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "tab", "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	snap = player.Snapshot()
	if snap.State != playback.StatePlaying || *snap.Book != kB || snap.Position != 7*time.Minute || snap.SpeedMilli != 1_250_000 {
		t.Fatalf("confirmed cross-book switch failed: %+v", snap)
	}
	// Prior Orchard checkpoint preserved at 4m.
	lsA, found, err := st.ListeningState(ctx, kA)
	if err != nil || !found || lsA.PositionNS != int64((4*time.Minute).Nanoseconds()) {
		t.Fatalf("prior Orchard checkpoint lost: found=%v ls=%+v err=%v", found, lsA, err)
	}
}

func TestBookmarkCoordinatorEdgeCasesAndErrors(t *testing.T) {
	ctx, ws, st, _, player, kA, kB, _ := setupBookmarkWorkspace(t)
	neg := -time.Second
	if _, err := NewCoordinator(ctx, st, Options{PreRoll: &neg}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for negative PreRoll, got %v", err)
	}
	zero := time.Duration(0)
	coord, err := NewCoordinator(ctx, st, Options{
		Player:       player,
		MediaChecker: NewRootMediaChecker(ws.Owner()),
		PreRoll:      &zero,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Test draft eviction cap (>64 drafts) and safeJumpTitle / sanitizeBookmarkError.
	for i := range maxTrackedDrafts + 5 {
		coord.mu.Lock()
		coord.storePendingDraftLocked(fmt.Sprintf("d-%d", i), bookmarks.Draft{BookKey: kA})
		coord.storeCommittedDraftLocked(fmt.Sprintf("c-%d", i), store.Bookmark{ID: fmt.Sprintf("b-%d", i), Key: kA})
		coord.mu.Unlock()
	}
	if len(coord.pendingDrafts) != maxTrackedDrafts || len(coord.committedDrafts) != maxTrackedDrafts {
		t.Fatalf("draft tracking exceeded cap: pending=%d committed=%d", len(coord.pendingDrafts), len(coord.committedDrafts))
	}
	if got := safeJumpTitle(" \x00\n "); got != "Audiobook" {
		t.Fatalf("expected Audiobook fallback, got %q", got)
	}
	if got := safeJumpTitle("Bad\xff\x01Title\u200b"); got != "BadTitle" {
		t.Fatalf("expected sanitized title, got %q", got)
	}
	for _, errCase := range []error{
		nil,
		bookmarks.ErrNoLoadedAudio,
		bookmarks.ErrStaleSession,
		bookmarks.ErrBookmarkNotFound,
		bookmarks.ErrMediaMissing,
		bookmarks.ErrExportExists,
		bookmarks.ErrUnsafeExportTarget,
		bookmarks.ErrInvalidInput,
		playback.ErrIPC,
	} {
		_ = sanitizeBookmarkError(errCase)
	}
	if got := coord.bookAuthor(store.BookKey{}); got != "" {
		t.Fatalf("expected empty author for invalid key, got %q", got)
	}
	if got := coord.bookAuthor(store.BookKey{Provider: "audible", Namespace: "us", ID: "B00UNKNOWN"}); got != "" {
		t.Fatalf("expected empty author for unknown key, got %q", got)
	}

	// Coordinator without Player/Bookmarks handles bookmark actions gracefully.
	noPlayerCoord, err := NewCoordinator(ctx, st, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer noPlayerCoord.Close(context.Background())
	encB, _ := EncodeBookKey(kB)
	if msg := noPlayerCoord.Dispatch(ui.Action{Kind: ui.CaptureBookmark})(); !strings.Contains(msg.(ui.NoticeMsg).Activity, "unavailable") {
		t.Fatalf("expected capture unavailable: %#v", msg)
	}
	if msg := noPlayerCoord.Dispatch(ui.Action{Kind: ui.SaveNote, Key: encB, RequestID: "x", Attempt: 1, Text: "hi"})(); !strings.Contains(msg.(ui.ModalResultMsg).Error, "unavailable") {
		t.Fatalf("expected save unavailable: %#v", msg)
	}
	if msg := noPlayerCoord.Dispatch(ui.Action{Kind: ui.OpenBookmarks, Key: encB})(); msg.(ui.OpenBookmarksMsg).Error == "" {
		t.Fatalf("expected open bookmarks unavailable: %#v", msg)
	}
	if msg := noPlayerCoord.Dispatch(ui.Action{Kind: ui.DeleteBookmark, Key: encB, BookmarkID: "x"})(); !strings.Contains(msg.(ui.ModalResultMsg).Error, "unavailable") {
		t.Fatalf("expected delete unavailable: %#v", msg)
	}
	if msg := noPlayerCoord.Dispatch(ui.Action{Kind: ui.JumpBookmark, Key: encB, BookmarkID: "x"})(); !strings.Contains(msg.(ui.NoticeMsg).Error, "unavailable") {
		t.Fatalf("expected jump unavailable: %#v", msg)
	}
	if msg := noPlayerCoord.Dispatch(ui.Action{Kind: ui.ConfirmSwitchBookmark, Key: encB, BookmarkID: "x"})(); !strings.Contains(msg.(ui.ModalResultMsg).Error, "unavailable") {
		t.Fatalf("expected confirm switch unavailable: %#v", msg)
	}
	if msg := noPlayerCoord.handleBookmarkAction(ui.Action{Kind: "unknown"}); !strings.Contains(msg.(ui.NoticeMsg).Activity, "unavailable") {
		t.Fatalf("expected default action notice: %#v", msg)
	}

	// Unloaded player direct jump to downloaded book B starts playing at bookmark position.
	if err := st.PutBookmark(ctx, store.Bookmark{
		ID: "bm-unloaded", Key: kB, PositionNS: int64((3 * time.Minute).Nanoseconds()),
		Note: "Start here", CreatedAtMS: 100, UpdatedAtMS: 100,
	}); err != nil {
		t.Fatal(err)
	}
	msg := coord.Dispatch(ui.Action{Kind: ui.JumpBookmark, Key: encB, BookmarkID: "bm-unloaded"})()
	if snapMsg, ok := msg.(ui.SnapshotMsg); !ok || snapMsg.Player.State != ui.Playing || snapMsg.Player.Position != 3*time.Minute {
		t.Fatalf("unloaded jump failed: %#v", msg)
	}

	// ConfirmSwitchBookmark validation errors: invalid session, invalid key, missing bookmark, missing media.
	curSess := fmt.Sprintf("%d", player.Snapshot().Session)
	if msg := coord.Dispatch(ui.Action{Kind: ui.ConfirmSwitchBookmark, Key: encB, Session: "0", BookmarkID: "bm-unloaded"})(); !strings.Contains(msg.(ui.ModalResultMsg).Error, "Invalid playback session") {
		t.Fatalf("expected invalid session error: %#v", msg)
	}
	if msg := coord.Dispatch(ui.Action{Kind: ui.ConfirmSwitchBookmark, Key: "bad", Session: curSess, BookmarkID: "bm-unloaded"})(); !strings.Contains(msg.(ui.ModalResultMsg).Error, "Invalid") {
		t.Fatalf("expected invalid key error: %#v", msg)
	}
	if msg := coord.Dispatch(ui.Action{Kind: ui.ConfirmSwitchBookmark, Key: encB, Session: curSess, BookmarkID: "missing"})(); !strings.Contains(msg.(ui.ModalResultMsg).Error, "no longer exists") {
		t.Fatalf("expected missing bookmark error: %#v", msg)
	}
	encC, _ := EncodeBookKey(store.BookKey{Provider: "audible", Namespace: "us", ID: "B00MISSING"})
	_ = st.PutBookmark(ctx, store.Bookmark{
		ID: "bm-c-miss", Key: store.BookKey{Provider: "audible", Namespace: "us", ID: "B00MISSING"},
		PositionNS: 1000, Note: "Missing media", CreatedAtMS: 100, UpdatedAtMS: 100,
	})
	if msg := coord.Dispatch(ui.Action{Kind: ui.ConfirmSwitchBookmark, Key: encC, Session: curSess, BookmarkID: "bm-c-miss"})(); !strings.Contains(msg.(ui.ModalResultMsg).Error, "missing or unsafe") {
		t.Fatalf("expected missing media error: %#v", msg)
	}

	// Missing bookmark jump, invalid keys, and expired draft save errors.
	if msg := coord.Dispatch(ui.Action{Kind: ui.JumpBookmark, Key: "bad", BookmarkID: "missing"})(); !strings.Contains(msg.(ui.NoticeMsg).Error, "Invalid") {
		t.Fatalf("expected invalid key jump error: %#v", msg)
	}
	if msg := coord.Dispatch(ui.Action{Kind: ui.JumpBookmark, Key: encB, BookmarkID: "missing"})(); !strings.Contains(msg.(ui.NoticeMsg).Error, "no longer exists") {
		t.Fatalf("expected missing bookmark error: %#v", msg)
	}
	if msg := coord.Dispatch(ui.Action{Kind: ui.SaveNote, Key: encB, Target: ui.NoteTarget{Key: "bad", BookmarkID: "bm-1"}, RequestID: "e1", Attempt: 1, Text: "hi"})(); !strings.Contains(msg.(ui.ModalResultMsg).Error, "Invalid") {
		t.Fatalf("expected invalid edit key error: %#v", msg)
	}
	if msg := coord.Dispatch(ui.Action{Kind: ui.SaveNote, Key: encB, RequestID: "expired", Attempt: 1, Text: "hi"})(); !strings.Contains(msg.(ui.ModalResultMsg).Error, "expired") {
		t.Fatalf("expected expired draft error: %#v", msg)
	}
	if msg := coord.Dispatch(ui.Action{Kind: ui.DeleteBookmark, Key: "bad", BookmarkID: "x"})(); !strings.Contains(msg.(ui.ModalResultMsg).Error, "Invalid") {
		t.Fatalf("expected invalid delete key error: %#v", msg)
	}
	if msg := coord.Dispatch(ui.Action{Kind: ui.OpenBookmarks, Key: "bad"})(); msg.(ui.OpenBookmarksMsg).Error == "" {
		t.Fatalf("expected invalid key error: %#v", msg)
	}
	// Capture without chapters falls back to "No chapter metadata".
	if msg := coord.Dispatch(ui.Action{Kind: ui.CaptureBookmark})(); msg.(ui.OpenNoteEditorMsg).Target.Chapter != "No chapter metadata" {
		t.Fatalf("expected No chapter metadata fallback: %#v", msg)
	}

	// Closed coordinator rejects all bookmark actions cleanly.
	if err := coord.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []ui.ActionKind{ui.CaptureBookmark, ui.SaveNote, ui.OpenBookmarks, ui.OpenGlobalBookmarks, ui.ExportBookmarks, ui.DeleteBookmark, ui.JumpBookmark, ui.ConfirmSwitchBookmark} {
		if msg := coord.Dispatch(ui.Action{Kind: kind, Key: encB})(); msg == nil {
			t.Fatalf("expected closed response for %v", kind)
		}
	}
}

func TestJ16GlobalBookmarkSearchAndSafeMarkdownExport(t *testing.T) {
	ctx, ws, st, _, player, kA, kB, kC := setupBookmarkWorkspace(t)
	// Seed auth.json to verify export NEVER leaks auth/tokens/paths.
	if err := os.WriteFile(filepath.Join(ws.RootPath(), "auth.json"), []byte(`{"access_token":"Atna|SECRET-TOKEN"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Update catalog with malicious title to verify Markdown/HTML escaping.
	dur := int64((30 * time.Minute).Nanoseconds())
	if err := st.ReplaceCatalog(ctx, "audible", "us", []store.CatalogBook{
		{Key: kA, Title: "The Glass Orchard", Authors: []store.Contributor{{Name: "Mara Vale"}}, DurationNS: &dur},
		{Key: kB, Title: "# Malicious <script>Rivers</script>", Authors: []store.Contributor{{Name: "Ilan [x](y)"}}, DurationNS: &dur},
		{Key: kC, Title: "Missing Audio Book", Authors: []store.Contributor{{Name: "No Media"}}, DurationNS: &dur},
	}, 1000); err != nil {
		t.Fatal(err)
	}
	if err := st.PutBookmark(ctx, store.Bookmark{
		ID: "bm-a1", Key: kA, PositionNS: int64(2 * time.Minute),
		Note: "Orchard note", CreatedAtMS: 100, UpdatedAtMS: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutBookmark(ctx, store.Bookmark{
		ID: "bm-b1", Key: kB, PositionNS: int64(5 * time.Minute),
		Note: "# Heading\n<img src=x onerror=alert(1)>\n[click](javascript:alert(1))", CreatedAtMS: 100, UpdatedAtMS: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutBookmark(ctx, store.Bookmark{
		ID: "bm-c1", Key: kC, PositionNS: int64(time.Minute),
		Note: "Missing audio note", CreatedAtMS: 100, UpdatedAtMS: 100,
	}); err != nil {
		t.Fatal(err)
	}

	coord, err := NewCoordinator(ctx, st, Options{
		Player:       player,
		MediaChecker: NewRootMediaChecker(ws.Owner()),
		RootOwner:    ws.Owner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer coord.Close(context.Background())

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

	// 1. Play Orchard (kA), then open All bookmarks (search & export) from Library sidebar (1 -> G -> Enter).
	sendUIKeys(t, r, "G", "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "1", "G", "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !r.Model.(ui.Model).BookmarksGlobal() {
		t.Fatal("expected global bookmark search open")
	}

	// 2. Filter for "Missing" -> jump offers Queue download without touching Orchard.
	sendUIKeys(t, r, "/", "m", "i", "s", "s", "i", "n", "g", "enter", "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Audio not downloaded") {
		t.Fatalf("missing audio jump in global search did not offer Queue download:\n%s", r.Model.View().Content)
	}
	sendUIKeys(t, r, "esc", "esc") // close confirmation, clear filter

	// 3. Filter for "Rivers" -> export filtered Markdown via 'e' -> creates exports/bookmarks-all.md safely!
	sendUIKeys(t, r, "/", "r", "i", "v", "e", "r", "s", "enter", "e")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	exportPath := filepath.Join(ws.RootPath(), "exports", "bookmarks-all.md")
	raw, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatalf("expected exported Markdown file: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, "SECRET-TOKEN") || strings.Contains(text, "<script>") || strings.Contains(text, "<img") {
		t.Fatalf("export leaked secret or unescaped HTML:\n%s", text)
	}
	if !strings.Contains(text, `\# Malicious &lt;script&gt;Rivers&lt;/script&gt;`) || strings.Contains(text, "Orchard note") {
		t.Fatalf("filtered export content mismatch:\n%s", text)
	}

	// 4. Clear filter, press 'e' again -> opens Overwrite confirmation; Keep cancels and preserves file!
	sendUIKeys(t, r, "esc", "e")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Overwrite export") || !strings.Contains(r.Model.View().Content, "[Keep existing file]") {
		t.Fatalf("expected overwrite confirmation:\n%s", r.Model.View().Content)
	}
	sendUIKeys(t, r, "enter") // Keep existing file
	rawAfterKeep, _ := os.ReadFile(exportPath)
	if string(rawAfterKeep) != text {
		t.Fatal("Keep existing file modified export")
	}

	// 5. Write failure during overwrite (unsafe 0755 exports dir) keeps confirmation recoverable; restore 0700 and overwrite!
	sendUIKeys(t, r, "e")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(ws.RootPath(), "exports"), 0o755); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "tab", "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "unsafe") {
		t.Fatalf("expected unsafe export error:\n%s", r.Model.View().Content)
	}
	if err := os.Chmod(filepath.Join(ws.RootPath(), "exports"), 0o700); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "enter") // retry overwrite
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	rawOverwritten, _ := os.ReadFile(exportPath)
	if !strings.Contains(string(rawOverwritten), "Orchard note") || !strings.Contains(string(rawOverwritten), "Missing audio note") {
		t.Fatalf("overwrite did not write all 3 bookmarks:\n%s", string(rawOverwritten))
	}

	// 6. Cross-book switch from global search (Rivers bm-b1 while Orchard is playing).
	sendUIKeys(t, r, "/", "r", "i", "v", "e", "r", "s", "enter", "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Switch audiobook") {
		t.Fatalf("expected cross-book switch confirmation from global search:\n%s", r.Model.View().Content)
	}
	sendUIKeys(t, r, "tab", "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if snap := player.Snapshot(); *snap.Book != kB || snap.Position != 5*time.Minute {
		t.Fatalf("global search cross-book switch failed: %+v", snap)
	}

	// Single-book export & error paths.
	encA, _ := EncodeBookKey(kA)
	if msg := coord.Dispatch(ui.Action{Kind: ui.ExportBookmarks, Key: encA})(); !strings.Contains(msg.(ui.NoticeMsg).Activity, "Exported 1 bookmarks") {
		t.Fatalf("single-book export failed: %#v", msg)
	}
	if msg := coord.Dispatch(ui.Action{Kind: ui.ExportBookmarks, Key: "bad"})(); !strings.Contains(msg.(ui.NoticeMsg).Error, "Invalid") {
		t.Fatalf("expected invalid key export error: %#v", msg)
	}
	noBmCoord, _ := NewCoordinator(ctx, st, Options{})
	defer noBmCoord.Close(context.Background())
	if msg := noBmCoord.Dispatch(ui.Action{Kind: ui.OpenGlobalBookmarks})(); msg.(ui.OpenBookmarksMsg).Error == "" {
		t.Fatalf("expected unavailable global bookmarks error: %#v", msg)
	}
	if msg := noBmCoord.Dispatch(ui.Action{Kind: ui.ExportBookmarks, Overwrite: true})(); !strings.Contains(msg.(ui.ModalResultMsg).Error, "Operation failed") {
		t.Fatalf("expected unavailable overwrite export error: %#v", msg)
	}
}
