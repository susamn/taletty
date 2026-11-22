package app

import (
	"context"
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

type faultFavoriteStore struct {
	*store.Store
	favErr    error
	setFavErr error
}

func (f *faultFavoriteStore) Favorites(ctx context.Context) ([]store.BookKey, error) {
	if f.favErr != nil {
		return nil, f.favErr
	}
	return f.Store.Favorites(ctx)
}

func (f *faultFavoriteStore) SetFavorite(ctx context.Context, key store.BookKey, favored bool, createdAtMS int64) error {
	if f.setFavErr != nil {
		return f.setFavErr
	}
	return f.Store.SetFavorite(ctx, key, favored, createdAtMS)
}

type countingAccountClient struct {
	calls int
	books []store.CatalogBook
}

func (a *countingAccountClient) Doctor(context.Context) (Diagnostics, error) {
	a.calls++
	return Diagnostics{}, nil
}
func (a *countingAccountClient) Login(context.Context, *os.File) error {
	a.calls++
	return nil
}
func (a *countingAccountClient) Sync(context.Context, string, func(SyncProgress)) ([]store.CatalogBook, error) {
	a.calls++
	return a.books, nil
}
func (a *countingAccountClient) Close(context.Context) error { return nil }

func TestJ17FavoritesJourneyPersistenceFailureAndOfflineRestart(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	ws, err := profile.Open(ctx, home, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	st := ws.Store()

	kA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B00ORCHARD"}
	kB := store.BookKey{Provider: "audible", Namespace: "us", ID: "B00RIVERS1"}
	kC := store.BookKey{Provider: "audible", Namespace: "us", ID: "B00UNDOWNL"}
	dur := int64((30 * time.Minute).Nanoseconds())
	catalog := []store.CatalogBook{
		{Key: kA, Title: "The Glass Orchard", Authors: []store.Contributor{{Name: "Mara Vale"}}, DurationNS: &dur},
		{Key: kB, Title: "A Map of Quiet Rivers", Authors: []store.Contributor{{Name: "Ilan Reed"}}, DurationNS: &dur},
		{Key: kC, Title: "Completed Undownloaded", Authors: []store.Contributor{{Name: "Noor Bell"}}, DurationNS: &dur},
	}
	if err := st.ReplaceCatalog(ctx, "audible", "us", catalog, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		key  store.BookKey
		name string
	}{
		{kA, "orchard.m4b"},
		{kB, "rivers.m4b"},
	} {
		if err := os.WriteFile(filepath.Join(ws.DownloadsPath(), item.name), []byte("fake-m4b"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := st.PutLocalMedia(ctx, store.LocalMedia{
			Key: item.key, RelativePath: filepath.Join("downloads", item.name), Format: "m4b", PublishedAtMS: now.UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutListeningState(ctx, store.ListeningState{
		Key: kC, PositionNS: dur, SpeedMilli: playback.DefaultSpeedMilli, Completion: store.CompletionFinished, UpdatedAtMS: now.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutBookmark(ctx, store.Bookmark{
		ID: "bm-b1", Key: kB, PositionNS: int64(time.Minute), Note: "Keep bookmark", CreatedAtMS: 100, UpdatedAtMS: 100,
	}); err != nil {
		t.Fatal(err)
	}

	backend := &fakePlaybackBackend{}
	player, err := playback.NewController(backend, playback.Options{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	acct := &countingAccountClient{books: catalog}
	faultSt := &faultFavoriteStore{Store: st}
	coord, err := NewCoordinator(ctx, faultSt, Options{
		Account:         acct,
		Player:          player,
		MediaChecker:    NewRootMediaChecker(ws.Owner()),
		RootOwner:       ws.Owner(),
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

	// 1. Play Orchard (kA), highlight Rivers (kB), mark Rivers and Completed Undownloaded (kC) favorite.
	// SortTitle order: A Map of Quiet Rivers (kB), Completed Undownloaded (kC), The Glass Orchard (kA).
	sendUIKeys(t, r, "G", "enter") // play Orchard
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "g", "f") // highlight Rivers, favorite Rivers while Orchard plays!
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if acct.calls != 0 {
		t.Fatalf("favorite toggle invoked provider: calls=%d", acct.calls)
	}
	if snap := player.Snapshot(); *snap.Book != kA || snap.State != playback.StatePlaying {
		t.Fatalf("favorite toggle altered current audio: %+v", snap)
	}
	sendUIKeys(t, r, "j", "f") // favorite Completed Undownloaded (kC)
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}

	// 2. Save failure retains authoritative prior state and displays recoverable error.
	faultSt.setFavErr = store.ErrDatabase
	sendUIKeys(t, r, "f")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "Error: Operation failed.") {
		t.Fatalf("expected recoverable error on SetFavorite failure:\n%s", r.Model.View().Content)
	}
	favs, _ := st.Favorites(ctx)
	if len(favs) != 2 {
		t.Fatalf("failed unfavorite mutated store: %+v", favs)
	}
	faultSt.setFavErr = nil
	if err := r.Send(ui.DismissFailureMsg{}); err != nil {
		t.Fatal(err)
	}

	// 3. Modal typing (/ filter and note editor) must NOT trigger f toggle.
	sendUIKeys(t, r, "/", "f", "esc")
	sendUIKeys(t, r, "b")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "f", "esc")
	favs, _ = st.Favorites(ctx)
	if len(favs) != 2 {
		t.Fatalf("modal typing triggered favorite toggle: %+v", favs)
	}

	// 4. Open Favorites view, filter/sort, verify background Sync preserves favorites, and delete local audio preserves favorite.
	sendUIKeys(t, r, "1", "g", "j", "j", "j", "enter") // Favorites
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	view, sortOrder, filter := r.Model.(ui.Model).ViewState()
	if view != ui.ViewFavorites {
		t.Fatalf("expected ViewFavorites, got %v", view)
	}
	// Background Sync preserves favorites.
	sendUIKeys(t, r, "a", "j", "enter") // Sync library
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "esc")
	favs, _ = st.Favorites(ctx)
	if len(favs) != 2 {
		t.Fatalf("background Sync lost favorites: %+v", favs)
	}
	// Delete local audio for Rivers (kB) preserves favorite state.
	encB, _ := EncodeBookKey(kB)
	_ = coord.Dispatch(ui.Action{Kind: ui.DownloadDeleteAudio, Key: encB, RequestID: "del-b", Attempt: 1})()
	favs, _ = st.Favorites(ctx)
	if len(favs) != 2 {
		t.Fatalf("audio deletion removed favorite: %+v", favs)
	}

	// Save ViewFavorites state and close coordinator cleanly.
	if err := coord.SaveFullViewState(ctx, view, sortOrder, filter, r.Model.(ui.Model).Focus(), r.Model.(ui.Model).Selection()); err != nil {
		t.Fatal(err)
	}
	_ = player.Close(ctx)
	_ = coord.Close(ctx)

	// 5. Restart completely offline (Account == nil): Favorites view and favorites restored!
	coord2, err := OpenWorkspace(ctx, home, Options{Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer coord2.Close(context.Background())

	r2, err := testkit.New(ctx, ui.NewModel(coord2.InitialSnapshot(), coord2.Dispatch), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range coord2.InitialMessages() {
		if err := r2.Send(msg); err != nil {
			t.Fatal(err)
		}
	}
	if err := r2.Send(tea.WindowSizeMsg{Width: 144, Height: 42}); err != nil {
		t.Fatal(err)
	}
	v2, _, _ := r2.Model.(ui.Model).ViewState()
	if v2 != ui.ViewFavorites {
		t.Fatalf("offline restart did not restore ViewFavorites: %v", v2)
	}
	// Unfavorite both rows inside Favorites -> selection stays valid, final row shows empty Favorites guidance,
	// and bookmarks/history remain intact!
	sendUIKeys(t, r2, "f")
	if err := r2.Drain(); err != nil {
		t.Fatal(err)
	}
	if r2.Model.(ui.Model).Selection() == "" {
		t.Fatal("unfavoriting first of two rows left empty selection")
	}
	sendUIKeys(t, r2, "f")
	if err := r2.Drain(); err != nil {
		t.Fatal(err)
	}
	if r2.Model.(ui.Model).Selection() != "" || !strings.Contains(r2.Model.View().Content, "No books in Favorites (press 1 for All)") {
		t.Fatalf("unfavoriting final row did not show empty Favorites guidance:\n%s", r2.Model.View().Content)
	}
	// Bookmarks and listening history preserved after unfavoriting.
	bms, _ := coord2.store.(*store.Store).Bookmarks(ctx, kB)
	if len(bms) != 1 {
		t.Fatalf("unfavoriting deleted bookmark: %+v", bms)
	}
	if _, found, _ := coord2.store.ListeningState(ctx, kC); !found {
		t.Fatal("unfavoriting deleted listening history")
	}
}

type readOnlyCatalogStore struct {
	CatalogStore
}

func TestFavoriteCoordinatorEdgeCases(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	ws, err := profile.Open(ctx, home, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close(context.Background())
	st := ws.Store()
	kA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B00ORCHARD"}
	_ = st.ReplaceCatalog(ctx, "audible", "us", []store.CatalogBook{{Key: kA, Title: "Orchard"}}, 100)
	encA, _ := EncodeBookKey(kA)

	faultSt := &faultFavoriteStore{Store: st}
	coord, err := NewCoordinator(ctx, faultSt, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if msg := coord.Dispatch(ui.Action{Kind: ui.ToggleFavorite, Key: "bad"})(); !strings.Contains(msg.(ui.NoticeMsg).Error, "Invalid") {
		t.Fatalf("expected invalid key error: %#v", msg)
	}
	faultSt.favErr = store.ErrDatabase
	if msg := coord.Dispatch(ui.Action{Kind: ui.ToggleFavorite, Key: encA})(); !strings.Contains(msg.(ui.NoticeMsg).Error, "Operation failed") {
		t.Fatalf("expected Favorites read error: %#v", msg)
	}
	faultSt.favErr = nil

	// Store without SetFavorite returns unavailable notice.
	noWriterCoord, err := NewCoordinator(ctx, readOnlyCatalogStore{CatalogStore: st}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer noWriterCoord.Close(context.Background())
	if msg := noWriterCoord.Dispatch(ui.Action{Kind: ui.ToggleFavorite, Key: encA})(); !strings.Contains(msg.(ui.NoticeMsg).Error, "unavailable") {
		t.Fatalf("expected unavailable error without favoriteWriter: %#v", msg)
	}

	_ = coord.Close(ctx)
	if msg := coord.Dispatch(ui.Action{Kind: ui.ToggleFavorite, Key: encA})(); !strings.Contains(msg.(ui.NoticeMsg).Error, "closed") {
		t.Fatalf("expected closed error: %#v", msg)
	}
}
