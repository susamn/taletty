package store

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/ncruces/go-sqlite3"
)

func strPtr(v string) *string { return &v }
func int64Ptr(v int64) *int64 { return &v }

func reopenStoreFixture(t *testing.T, path string) (*Owner, *Store) {
	t.Helper()
	root, err := os.Open(path)
	must(t, err)
	t.Cleanup(func() { root.Close() })
	owner, err := NewOwner(context.Background(), root, func() error { return nil })
	must(t, err)
	t.Cleanup(func() { must(t, owner.Close(context.Background())) })
	s, err := Open(context.Background(), owner)
	must(t, err)
	t.Cleanup(func() { must(t, s.Close(context.Background())) })
	return owner, s
}

func TestBookKeyAndCatalogValidation(t *testing.T) {
	validKeys := []BookKey{
		{Provider: "audible", Namespace: "us", ID: "B000000001"},
		{Provider: "audible", Namespace: "uk", ID: "b00000000A"},
		{Provider: "local", Namespace: "user-1_a", ID: "record.1-a_B"},
	}
	for _, key := range validKeys {
		if err := key.Validate(); err != nil {
			t.Fatalf("expected valid key %+v, got %v", key, err)
		}
	}

	invalidKeys := []BookKey{
		{Provider: "", Namespace: "us", ID: "B000000001"},
		{Provider: "Audible", Namespace: "us", ID: "B000000001"},
		{Provider: "1local", Namespace: "us", ID: "id"},
		{Provider: "local.x", Namespace: "us", ID: "id"},
		{Provider: "local", Namespace: "", ID: "id"},
		{Provider: "local", Namespace: "US", ID: "id"},
		{Provider: "local", Namespace: "us\x00", ID: "id"},
		{Provider: "audible", Namespace: "us", ID: "short"},
		{Provider: "audible", Namespace: "us", ID: "B0000000.1"},
		{Provider: "local", Namespace: "user", ID: ""},
		{Provider: "local", Namespace: "user", ID: "../escape"},
		{Provider: "local", Namespace: "user", ID: "has space"},
		{Provider: "local", Namespace: "user", ID: "id\x00bad"},
	}
	for _, key := range invalidKeys {
		if err := key.Validate(); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for %+v, got %v", key, err)
		}
	}

	validBook := CatalogBook{
		Key:        BookKey{Provider: "local", Namespace: "user", ID: "book-1"},
		Title:      "Title",
		Subtitle:   strPtr(""),
		Authors:    []Contributor{},
		Narrators:  []Contributor{{Name: "Narrator"}},
		DurationNS: int64Ptr(0),
	}
	must(t, validBook.Validate())

	invalidBooks := []CatalogBook{
		{Key: BookKey{}, Title: "Title"},
		{Key: validBook.Key, Title: " \u00a0\t "},
		{Key: validBook.Key, Title: "Bad\x00Title"},
		{Key: validBook.Key, Title: "Bad\u200bTitle"}, // Unicode Cf
		{Key: validBook.Key, Title: string([]byte{0xff, 0xfe})},
		{Key: validBook.Key, Title: "Ok", Subtitle: strPtr("Sub\nLine")},
		{Key: validBook.Key, Title: "Ok", DurationNS: int64Ptr(-1)},
		{Key: validBook.Key, Title: "Ok", Authors: []Contributor{{Name: "   "}}},
		{Key: validBook.Key, Title: "Ok", Narrators: []Contributor{{Name: "Bad\x1b"}}},
	}
	for i, book := range invalidBooks {
		if err := book.Validate(); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("case %d: expected ErrInvalidInput, got %v", i, err)
		}
	}
}

func TestCatalogReplaceQueryPreservationAndReopen(t *testing.T) {
	path, _, owner := ownerFixture(t)
	s := storeFixture(t, owner)
	ctx := context.Background()

	us1 := BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	us2 := BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	uk1 := BookKey{Provider: "audible", Namespace: "uk", ID: "B000000001"}

	// Initial US and UK snapshots: test NULL vs empty/zero values and contributor ordering.
	must(t, s.ReplaceCatalog(ctx, "audible", "us", []CatalogBook{
		{
			Key:        us1,
			Title:      "First Book",
			Subtitle:   strPtr(""),
			Authors:    []Contributor{{Name: "Author Two"}, {Name: "Author One"}},
			Narrators:  []Contributor{},
			DurationNS: int64Ptr(0),
		},
		{
			Key:   us2,
			Title: "Second Book",
		},
	}, 1000))
	must(t, s.ReplaceCatalog(ctx, "audible", "uk", []CatalogBook{
		{Key: uk1, Title: "UK Edition", DurationNS: int64Ptr(9223372036854775807)},
	}, 1001))

	// Record surrogate book_key and attach independent local state to us1.
	var originalPK int64
	must(t, s.withConn(ctx, func(c *sqlite3.Conn) error {
		var err error
		originalPK, err = lookupBookKey(c, us1)
		must(t, err)
		must(t, execStmt(c, `INSERT INTO listening_state VALUES(?, 500, 1000000, 'unfinished', 10)`, originalPK))
		must(t, execStmt(c, `INSERT INTO favorite VALUES(?, 11)`, originalPK))
		return nil
	}))

	// Invalid inputs before transaction must not mutate existing catalog.
	for _, err := range []error{
		s.ReplaceCatalog(ctx, "Audible", "us", nil, 2000),
		s.ReplaceCatalog(ctx, "audible", "us", nil, -1),
		s.ReplaceCatalog(ctx, "audible", "us", []CatalogBook{{Key: uk1, Title: "Wrong Scope"}}, 2000),
		s.ReplaceCatalog(ctx, "audible", "us", []CatalogBook{{Key: us1, Title: "Dup"}, {Key: us1, Title: "Dup"}}, 2000),
		s.ReplaceCatalog(ctx, "audible", "us", []CatalogBook{{Key: us1, Title: "   "}}, 2000),
	} {
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput, got %v", err)
		}
	}
	if _, err := s.Catalog(ctx, "audible", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for partial scope, got %v", err)
	}
	if _, _, err := s.SnapshotRefreshedAt(ctx, "", "us"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for invalid scope, got %v", err)
	}

	// Verify initial catalog preserved after rejected calls.
	usCatalog, err := s.Catalog(ctx, "audible", "us")
	must(t, err)
	if len(usCatalog) != 2 || usCatalog[0].Key != us1 || usCatalog[1].Key != us2 {
		t.Fatalf("unexpected US catalog: %+v", usCatalog)
	}
	if usCatalog[0].Subtitle == nil || *usCatalog[0].Subtitle != "" {
		t.Fatalf("expected empty known subtitle, got %+v", usCatalog[0].Subtitle)
	}
	if !reflect.DeepEqual(usCatalog[0].Authors, []Contributor{{Name: "Author Two"}, {Name: "Author One"}}) {
		t.Fatalf("author order lost: %+v", usCatalog[0].Authors)
	}
	if usCatalog[0].Narrators == nil || len(usCatalog[0].Narrators) != 0 {
		t.Fatalf("expected known empty narrators slice, got %#v", usCatalog[0].Narrators)
	}
	if usCatalog[0].DurationNS == nil || *usCatalog[0].DurationNS != 0 {
		t.Fatalf("expected zero duration, got %+v", usCatalog[0].DurationNS)
	}
	if usCatalog[1].Subtitle != nil || usCatalog[1].Authors != nil || usCatalog[1].Narrators != nil || usCatalog[1].DurationNS != nil {
		t.Fatalf("expected nil unknown fields, got %+v", usCatalog[1])
	}

	// Replace US catalog: remove us1 from catalog, update us2, add us3.
	us3 := BookKey{Provider: "audible", Namespace: "us", ID: "B000000003"}
	must(t, s.ReplaceCatalog(ctx, "audible", "us", []CatalogBook{
		{Key: us2, Title: "Second Book Updated", DurationNS: int64Ptr(42)},
		{Key: us3, Title: "Third Book"},
	}, 2000))

	// us1 identity and local state must remain intact even though us1 left catalog_book.
	must(t, s.withConn(ctx, func(c *sqlite3.Conn) error {
		pk, err := lookupBookKey(c, us1)
		must(t, err)
		if pk != originalPK {
			t.Fatalf("book_key changed: %d vs %d", pk, originalPK)
		}
		if scalar(t, c, "SELECT count(*) FROM listening_state") != "1" || scalar(t, c, "SELECT count(*) FROM favorite") != "1" {
			t.Fatal("independent local state lost")
		}
		return nil
	}))

	// Re-add us1 later: identity key must STILL match originalPK.
	must(t, s.ReplaceCatalog(ctx, "audible", "us", []CatalogBook{
		{Key: us1, Title: "First Book Restored"},
	}, 3000))
	must(t, s.withConn(ctx, func(c *sqlite3.Conn) error {
		pk, err := lookupBookKey(c, us1)
		must(t, err)
		if pk != originalPK {
			t.Fatalf("restored book_key changed: %d vs %d", pk, originalPK)
		}
		return nil
	}))

	// Empty US snapshot removes only US catalog, preserving UK and local state.
	must(t, s.ReplaceCatalog(ctx, "audible", "us", []CatalogBook{}, 4000))
	all, err := s.Catalog(ctx, "", "")
	must(t, err)
	if len(all) != 1 || all[0].Key != uk1 {
		t.Fatalf("expected only UK book remaining, got %+v", all)
	}
	ts, found, err := s.SnapshotRefreshedAt(ctx, "audible", "us")
	must(t, err)
	if !found || ts != 4000 {
		t.Fatalf("unexpected US snapshot ts: (%d, %v)", ts, found)
	}
	_, found, err = s.SnapshotRefreshedAt(ctx, "local", "absent")
	must(t, err)
	if found {
		t.Fatal("unexpected snapshot for absent scope")
	}

	// Reopen durability check.
	must(t, s.Close(ctx))
	must(t, owner.Close(ctx))
	_, reopened := reopenStoreFixture(t, path)
	allAfter, err := reopened.Catalog(ctx, "", "")
	must(t, err)
	if len(allAfter) != 1 || allAfter[0].Key != uk1 || *allAfter[0].DurationNS != 9223372036854775807 {
		t.Fatalf("catalog lost across reopen: %+v", allAfter)
	}
}

func TestRepositoryTransactionRollbackCancelClosedAndHelpers(t *testing.T) {
	_, _, owner := ownerFixture(t)
	s := storeFixture(t, owner)
	ctx := context.Background()

	// Seed initial book.
	k := BookKey{Provider: "local", Namespace: "user", ID: "seed-1"}
	must(t, s.ReplaceCatalog(ctx, "local", "user", []CatalogBook{{Key: k, Title: "Before"}}, 10))

	// Injected failure inside withTx rolls back cleanly and sanitizes raw SQL error.
	err := s.withTx(ctx, true, func(c *sqlite3.Conn) error {
		must(t, execStmt(c, `UPDATE catalog_book SET title = 'Mutated'`))
		return errors.New("raw sqlite diagnostic should be hidden")
	})
	if err != ErrDatabase {
		t.Fatalf("expected ErrDatabase, got %v", err)
	}
	books, err := s.Catalog(ctx, "local", "user")
	must(t, err)
	if len(books) != 1 || books[0].Title != "Before" {
		t.Fatalf("failed transaction was not rolled back: %+v", books)
	}

	// Panic inside withTx rolls back cleanly.
	func() {
		defer func() {
			if recover() != "tx-panic" {
				t.Error("expected tx-panic")
			}
		}()
		_ = s.withTx(ctx, true, func(c *sqlite3.Conn) error {
			must(t, execStmt(c, `UPDATE catalog_book SET title = 'Panicked'`))
			panic("tx-panic")
		})
	}()
	books, err = s.Catalog(ctx, "local", "user")
	must(t, err)
	if len(books) != 1 || books[0].Title != "Before" {
		t.Fatalf("panicked transaction was not rolled back: %+v", books)
	}

	// Canceled context before call.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.ReplaceCatalog(canceled, "local", "user", nil, 20); err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if _, err := s.Catalog(canceled, "", ""); err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if _, _, err := s.SnapshotRefreshedAt(canceled, "local", "user"); err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// Domain errors inside withTx pass through without leaking raw SQLite errors.
	if err := s.withTx(ctx, true, func(*sqlite3.Conn) error {
		return ErrActiveJobExists
	}); err != ErrActiveJobExists {
		t.Fatalf("expected ErrActiveJobExists, got %v", err)
	}

	// Nil and closed store refusals.
	var nilStore *Store
	if err := nilStore.withTx(ctx, false, func(*sqlite3.Conn) error { return nil }); err != ErrClosed {
		t.Fatalf("expected ErrClosed for nil store, got %v", err)
	}
	must(t, s.Close(ctx))
	if err := s.ReplaceCatalog(ctx, "local", "user", nil, 20); err != ErrClosed {
		t.Fatalf("expected ErrClosed, got %v", err)
	}

	// Direct helper boundary checks.
	if _, err := decodeContributors(strPtr("{not-array")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for corrupt JSON, got %v", err)
	}
	if got, err := decodeContributors(strPtr("null")); err != nil || len(got) != 0 || got == nil {
		t.Fatalf("expected empty non-nil slice for null JSON, got (%#v, %v)", got, err)
	}
	if !safeMultiline("line1\n\tline2") || safeMultiline("bad\x00") || safeMultiline("bad\u200b") || safeMultiline(string([]byte{0xff})) {
		t.Fatal("safeMultiline boundary failed")
	}
	if !safeID("id-1") || safeID("") || safeID("  ") || safeID(" id") {
		t.Fatal("safeID boundary failed")
	}

	// Exercise bindParams unsupported type & bad SQL prepare paths on raw in-memory conn.
	mem, err := sqlite3.Open(":memory:")
	must(t, err)
	defer mem.Close()
	if err := execStmt(mem, "SELECT ?", struct{}{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for unsupported bind arg, got %v", err)
	}
	if err := execStmt(mem, "INVALID SQL"); err == nil {
		t.Fatal("expected prepare error")
	}
	if err := queryStmt(mem, "INVALID SQL", nil); err == nil {
		t.Fatal("expected prepare error")
	}
	if err := queryStmt(mem, "SELECT ?", nil, struct{}{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for unsupported query bind arg, got %v", err)
	}
	if _, err := lookupBookKey(mem, BookKey{Provider: "local", Namespace: "user", ID: "missing"}); err == nil {
		t.Fatal("expected error querying missing table")
	}
	must(t, mem.Exec("CREATE TABLE book_identity(book_key INTEGER PRIMARY KEY, provider TEXT, namespace TEXT, id TEXT)"))
	if _, err := lookupBookKey(mem, BookKey{Provider: "local", Namespace: "user", ID: "missing"}); err != ErrDatabase {
		t.Fatalf("expected ErrDatabase for missing book_identity row, got %v", err)
	}
}

func TestListeningStateSettingsViewStateFavoritesAndBookmarks(t *testing.T) {
	path, _, owner := ownerFixture(t)
	s := storeFixture(t, owner)
	ctx := context.Background()

	k1 := BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	k2 := BookKey{Provider: "local", Namespace: "user", ID: "book-2"}

	// Defaults when singleton rows are absent.
	defSettings, err := s.Settings(ctx)
	must(t, err)
	if defSettings != (Settings{GlyphMode: GlyphUnicode, VolumePercent: 100}) {
		t.Fatalf("unexpected default settings: %+v", defSettings)
	}
	defView, err := s.ViewState(ctx)
	must(t, err)
	if defView != (ViewState{Destination: DestinationAll, Focus: FocusBooks, SortOrder: SortTitle}) {
		t.Fatalf("unexpected default view state: %+v", defView)
	}
	if _, found, err := s.ListeningState(ctx, k1); err != nil || found {
		t.Fatalf("expected absent listening state, got (%v, %v)", found, err)
	}

	// ListeningState roundtrip and update.
	wantLS := ListeningState{
		Key:         k1,
		PositionNS:  123456789,
		SpeedMilli:  1250000,
		Completion:  CompletionUnfinished,
		UpdatedAtMS: 500,
	}
	must(t, s.PutListeningState(ctx, wantLS))
	wantLS.Completion = CompletionFinished
	wantLS.UpdatedAtMS = 600
	must(t, s.PutListeningState(ctx, wantLS))
	gotLS, found, err := s.ListeningState(ctx, k1)
	must(t, err)
	if !found || gotLS != wantLS {
		t.Fatalf("unexpected listening state: (%+v, %v)", gotLS, found)
	}

	// Settings roundtrip.
	wantSettings := Settings{GlyphMode: GlyphPlain, VolumePercent: 72}
	must(t, s.PutSettings(ctx, wantSettings))
	gotSettings, err := s.Settings(ctx)
	must(t, err)
	if gotSettings != wantSettings {
		t.Fatalf("unexpected settings: %+v", gotSettings)
	}

	// ViewState roundtrip without and with Selected.
	must(t, s.PutViewState(ctx, ViewState{
		Destination: DestinationContinue,
		Focus:       FocusChapters,
		FilterText:  "chapter",
		SortOrder:   SortRecent,
	}))
	gotView, err := s.ViewState(ctx)
	must(t, err)
	if gotView.Selected != nil || gotView.Destination != DestinationContinue || gotView.Focus != FocusChapters {
		t.Fatalf("unexpected view state without selection: %+v", gotView)
	}
	wantView := ViewState{
		Destination: DestinationFavorites,
		Selected:    &k1,
		Focus:       FocusDetails,
		FilterText:  "query",
		SortOrder:   SortAcquisition,
	}
	must(t, s.PutViewState(ctx, wantView))
	gotView, err = s.ViewState(ctx)
	must(t, err)
	if gotView.Selected == nil || *gotView.Selected != k1 || gotView.SortOrder != SortAcquisition {
		t.Fatalf("unexpected view state with selection: %+v", gotView)
	}

	// Favorites roundtrip, ordering and removal.
	must(t, s.SetFavorite(ctx, k2, false, 0)) // absent un-favorite is a no-op
	must(t, s.SetFavorite(ctx, k2, true, 200))
	must(t, s.SetFavorite(ctx, k1, true, 100))
	favs, err := s.Favorites(ctx)
	must(t, err)
	if !reflect.DeepEqual(favs, []BookKey{k1, k2}) {
		t.Fatalf("unexpected favorites order: %+v", favs)
	}
	must(t, s.SetFavorite(ctx, k1, false, 0))
	favs, err = s.Favorites(ctx)
	must(t, err)
	if !reflect.DeepEqual(favs, []BookKey{k2}) {
		t.Fatalf("unexpected favorites after delete: %+v", favs)
	}

	// Bookmarks CRUD, ordering, multiline note, and cross-book ID protection.
	bLate := Bookmark{
		ID:           "bm-2",
		Key:          k1,
		PositionNS:   900,
		Note:         "Second\n\tnote",
		ChapterID:    strPtr("ch-2"),
		ChapterTitle: strPtr("Chapter Two"),
		CreatedAtMS:  10,
		UpdatedAtMS:  20,
	}
	bEarly := Bookmark{
		ID:           "bm-1",
		Key:          k1,
		PositionNS:   200,
		Note:         "First note",
		ChapterTitle: strPtr(""),
		CreatedAtMS:  5,
		UpdatedAtMS:  5,
	}
	must(t, s.PutBookmark(ctx, bLate))
	must(t, s.PutBookmark(ctx, bEarly))
	bEarly.Note = "First note updated"
	bEarly.UpdatedAtMS = 30
	must(t, s.PutBookmark(ctx, bEarly))

	// Reject stealing an existing bookmark ID for another book.
	if err := s.PutBookmark(ctx, Bookmark{
		ID:          "bm-1",
		Key:         k2,
		PositionNS:  100,
		Note:        "Steal",
		CreatedAtMS: 1,
		UpdatedAtMS: 1,
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for cross-book bookmark ID, got %v", err)
	}

	bms, err := s.Bookmarks(ctx, k1)
	must(t, err)
	if !reflect.DeepEqual(bms, []Bookmark{bEarly, bLate}) {
		t.Fatalf("unexpected bookmarks: %+v", bms)
	}
	must(t, s.DeleteBookmark(ctx, k1, "bm-2"))
	bms, err = s.Bookmarks(ctx, k1)
	must(t, err)
	if !reflect.DeepEqual(bms, []Bookmark{bEarly}) {
		t.Fatalf("unexpected bookmarks after delete: %+v", bms)
	}

	// Input validation matrix before transaction.
	for _, err := range []error{
		s.PutListeningState(ctx, ListeningState{Key: BookKey{}, PositionNS: 0, SpeedMilli: 1000000, Completion: CompletionUnfinished}),
		s.PutListeningState(ctx, ListeningState{Key: k1, PositionNS: -1, SpeedMilli: 1000000, Completion: CompletionUnfinished}),
		s.PutListeningState(ctx, ListeningState{Key: k1, PositionNS: 0, SpeedMilli: 0, Completion: CompletionUnfinished}),
		s.PutListeningState(ctx, ListeningState{Key: k1, PositionNS: 0, SpeedMilli: 1000000, Completion: "done"}),
		s.PutListeningState(ctx, ListeningState{Key: k1, PositionNS: 0, SpeedMilli: 1000000, Completion: CompletionUnfinished, UpdatedAtMS: -1}),
		s.PutSettings(ctx, Settings{GlyphMode: "nerd", VolumePercent: 50}),
		s.PutSettings(ctx, Settings{GlyphMode: GlyphUnicode, VolumePercent: 101}),
		s.PutViewState(ctx, ViewState{Destination: "bad", Focus: FocusBooks, SortOrder: SortTitle}),
		s.PutViewState(ctx, ViewState{Destination: DestinationAll, Focus: "bad", SortOrder: SortTitle}),
		s.PutViewState(ctx, ViewState{Destination: DestinationAll, Focus: FocusBooks, SortOrder: "bad"}),
		s.PutViewState(ctx, ViewState{Destination: DestinationAll, Focus: FocusBooks, SortOrder: SortTitle, FilterText: "bad\n"}),
		s.PutViewState(ctx, ViewState{Destination: DestinationAll, Focus: FocusBooks, SortOrder: SortTitle, Selected: &BookKey{}}),
		s.SetFavorite(ctx, BookKey{}, true, 0),
		s.SetFavorite(ctx, k1, true, -1),
		s.PutBookmark(ctx, Bookmark{ID: "", Key: k1, Note: "x"}),
		s.PutBookmark(ctx, Bookmark{ID: "id", Key: BookKey{}, Note: "x"}),
		s.PutBookmark(ctx, Bookmark{ID: "id", Key: k1, PositionNS: -1, Note: "x"}),
		s.PutBookmark(ctx, Bookmark{ID: "id", Key: k1, Note: " \u00a0 "}),
		s.PutBookmark(ctx, Bookmark{ID: "id", Key: k1, Note: "x", CreatedAtMS: 10, UpdatedAtMS: 9}),
		s.PutBookmark(ctx, Bookmark{ID: "id", Key: k1, Note: "x", ChapterID: strPtr("bad\n")}),
		s.PutBookmark(ctx, Bookmark{ID: "id", Key: k1, Note: "x", ChapterTitle: strPtr("bad\x00")}),
		s.DeleteBookmark(ctx, BookKey{}, "id"),
		s.DeleteBookmark(ctx, k1, ""),
	} {
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput, got %v", err)
		}
	}
	if _, _, err := s.ListeningState(ctx, BookKey{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	if _, err := s.Bookmarks(ctx, BookKey{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}

	// Canceled context before queries.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := s.ListeningState(canceled, k1); err != context.Canceled {
		t.Fatal(err)
	}
	if _, err := s.Settings(canceled); err != context.Canceled {
		t.Fatal(err)
	}
	if _, err := s.ViewState(canceled); err != context.Canceled {
		t.Fatal(err)
	}
	if _, err := s.Favorites(canceled); err != context.Canceled {
		t.Fatal(err)
	}
	if _, err := s.Bookmarks(canceled, k1); err != context.Canceled {
		t.Fatal(err)
	}

	// Reopen durability check.
	must(t, s.Close(ctx))
	must(t, owner.Close(ctx))
	_, reopened := reopenStoreFixture(t, path)

	lsAfter, found, err := reopened.ListeningState(ctx, k1)
	must(t, err)
	if !found || lsAfter != wantLS {
		t.Fatalf("listening state lost across reopen: (%+v, %v)", lsAfter, found)
	}
	settingsAfter, err := reopened.Settings(ctx)
	must(t, err)
	if settingsAfter != wantSettings {
		t.Fatalf("settings lost across reopen: %+v", settingsAfter)
	}
	viewAfter, err := reopened.ViewState(ctx)
	must(t, err)
	if viewAfter.Selected == nil || *viewAfter.Selected != k1 || viewAfter.FilterText != "query" {
		t.Fatalf("view state lost across reopen: %+v", viewAfter)
	}
	favsAfter, err := reopened.Favorites(ctx)
	must(t, err)
	if !reflect.DeepEqual(favsAfter, []BookKey{k2}) {
		t.Fatalf("favorites lost across reopen: %+v", favsAfter)
	}
	bmsAfter, err := reopened.Bookmarks(ctx, k1)
	must(t, err)
	if !reflect.DeepEqual(bmsAfter, []Bookmark{bEarly}) {
		t.Fatalf("bookmarks lost across reopen: %+v", bmsAfter)
	}
}

func TestLocalMediaInventoryAndValidation(t *testing.T) {
	path, _, owner := ownerFixture(t)
	s := storeFixture(t, owner)
	ctx := context.Background()

	k1 := BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	k2 := BookKey{Provider: "local", Namespace: "user", ID: "book-2"}

	if _, found, err := s.LocalMedia(ctx, k1); err != nil || found {
		t.Fatalf("expected absent media, got (%v, %v)", found, err)
	}

	m1 := LocalMedia{Key: k1, RelativePath: "downloads/B000000001.m4b", Format: "m4b", PublishedAtMS: 100}
	m2 := LocalMedia{Key: k2, RelativePath: "downloads/sub/book-2.m4b", Format: "m4b", PublishedAtMS: 200}
	must(t, s.PutLocalMedia(ctx, m1))
	must(t, s.PutLocalMedia(ctx, m2))

	// Unique relative_path conflict across different books returns ErrMediaPathConflict.
	if err := s.PutLocalMedia(ctx, LocalMedia{
		Key: k2, RelativePath: "downloads/B000000001.m4b", Format: "m4b", PublishedAtMS: 300,
	}); err != ErrMediaPathConflict {
		t.Fatalf("expected ErrMediaPathConflict, got %v", err)
	}

	// Same book updating its own path succeeds.
	m1.RelativePath = "downloads/B000000001-v2.m4b"
	m1.PublishedAtMS = 400
	must(t, s.PutLocalMedia(ctx, m1))

	got, found, err := s.LocalMedia(ctx, k1)
	must(t, err)
	if !found || got != m1 {
		t.Fatalf("unexpected local media: (%+v, %v)", got, found)
	}
	list, err := s.LocalMediaList(ctx)
	must(t, err)
	if !reflect.DeepEqual(list, []LocalMedia{m1, m2}) {
		t.Fatalf("unexpected media list: %+v", list)
	}

	// Delete removes only local_media, preserving identity & unrelated local state.
	must(t, s.SetFavorite(ctx, k1, true, 50))
	must(t, s.DeleteLocalMedia(ctx, k1))
	if _, found, err := s.LocalMedia(ctx, k1); err != nil || found {
		t.Fatalf("expected deleted media, got (%v, %v)", found, err)
	}
	favs, err := s.Favorites(ctx)
	must(t, err)
	if !reflect.DeepEqual(favs, []BookKey{k1}) {
		t.Fatalf("favorite lost after DeleteLocalMedia: %+v", favs)
	}

	// Invalid inputs before transaction.
	for _, bad := range []LocalMedia{
		{Key: BookKey{}, RelativePath: "downloads/a.m4b", Format: "m4b"},
		{Key: k1, RelativePath: "downloads/a.m4b", Format: "mp3"},
		{Key: k1, RelativePath: "downloads/a.m4b", Format: "m4b", PublishedAtMS: -1},
		{Key: k1, RelativePath: "other/a.m4b", Format: "m4b"},
		{Key: k1, RelativePath: "downloads/../a.m4b", Format: "m4b"},
		{Key: k1, RelativePath: "downloads/a..b.m4b", Format: "m4b"},
		{Key: k1, RelativePath: "downloads/./a.m4b", Format: "m4b"},
		{Key: k1, RelativePath: "downloads//a.m4b", Format: "m4b"},
		{Key: k1, RelativePath: "downloads\\a.m4b", Format: "m4b"},
		{Key: k1, RelativePath: "downloads/a.mp3", Format: "m4b"},
		{Key: k1, RelativePath: "downloads/.m4b", Format: "m4b"},
		{Key: k1, RelativePath: "downloads/.hidden.m4b", Format: "m4b"},
		{Key: k1, RelativePath: "downloads/a\x00.m4b", Format: "m4b"},
	} {
		if err := s.PutLocalMedia(ctx, bad); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for %+v, got %v", bad, err)
		}
	}
	if _, _, err := s.LocalMedia(ctx, BookKey{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	if err := s.DeleteLocalMedia(ctx, BookKey{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := s.LocalMedia(canceled, k2); err != context.Canceled {
		t.Fatal(err)
	}
	if _, err := s.LocalMediaList(canceled); err != context.Canceled {
		t.Fatal(err)
	}

	// Reopen durability.
	must(t, s.Close(ctx))
	must(t, owner.Close(ctx))
	_, reopened := reopenStoreFixture(t, path)
	listAfter, err := reopened.LocalMediaList(ctx)
	must(t, err)
	if !reflect.DeepEqual(listAfter, []LocalMedia{m2}) {
		t.Fatalf("media inventory lost across reopen: %+v", listAfter)
	}
}

func TestDownloadJobsUniquenessOrderingReconciliationAndReopen(t *testing.T) {
	path, _, owner := ownerFixture(t)
	s := storeFixture(t, owner)
	ctx := context.Background()

	k1 := BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	k2 := BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	k3 := BookKey{Provider: "local", Namespace: "user", ID: "book-3"}

	if code := NormalizeFailureCode("network"); code != FailureNetwork {
		t.Fatalf("unexpected normalized code: %s", code)
	}
	if code := NormalizeFailureCode("raw_engine_prose"); code != FailureInternal {
		t.Fatalf("expected FailureInternal, got %s", code)
	}

	// Terminal history coexists for the same book; only live jobs are unique per book.
	must(t, s.PutDownloadJob(ctx, DownloadJob{
		ID:            "job-done",
		Key:           k1,
		State:         JobReady,
		BytesReceived: 100,
		BytesTotal:    int64Ptr(100),
		CreatedAtMS:   1,
		UpdatedAtMS:   2,
	}))
	must(t, s.PutDownloadJob(ctx, DownloadJob{
		ID:            "job-canceled",
		Key:           k1,
		State:         JobCanceled,
		BytesReceived: 10,
		CreatedAtMS:   3,
		UpdatedAtMS:   4,
	}))

	// One active job globally + waiting queue jobs.
	must(t, s.PutDownloadJob(ctx, DownloadJob{
		ID:            "job-active-1",
		Key:           k1,
		State:         JobDownloading,
		BytesReceived: 50,
		BytesTotal:    int64Ptr(200),
		CreatedAtMS:   5,
		UpdatedAtMS:   6,
	}))
	must(t, s.PutDownloadJob(ctx, DownloadJob{
		ID:            "job-wait-2",
		Key:           k2,
		QueueOrder:    int64Ptr(0),
		State:         JobWaiting,
		BytesReceived: 0,
		CreatedAtMS:   7,
		UpdatedAtMS:   8,
	}))
	must(t, s.PutDownloadJob(ctx, DownloadJob{
		ID:            "job-wait-3",
		Key:           k3,
		QueueOrder:    int64Ptr(1),
		State:         JobWaiting,
		BytesReceived: 0,
		CreatedAtMS:   9,
		UpdatedAtMS:   10,
	}))

	// Duplicate live job for k1 -> ErrActiveJobExists.
	if err := s.PutDownloadJob(ctx, DownloadJob{
		ID:          "job-live-dup",
		Key:         k1,
		QueueOrder:  int64Ptr(2),
		State:       JobWaiting,
		CreatedAtMS: 11,
		UpdatedAtMS: 11,
	}); err != ErrActiveJobExists {
		t.Fatalf("expected ErrActiveJobExists, got %v", err)
	}

	// Second globally active job for k2 -> ErrDownloadInProgress (note: k2 already has waiting job-wait-2, so test with fresh k4).
	k4 := BookKey{Provider: "local", Namespace: "user", ID: "book-4"}
	if err := s.PutDownloadJob(ctx, DownloadJob{
		ID:          "job-active-dup",
		Key:         k4,
		State:       JobPreparing,
		CreatedAtMS: 12,
		UpdatedAtMS: 12,
	}); err != ErrDownloadInProgress {
		t.Fatalf("expected ErrDownloadInProgress, got %v", err)
	}

	// Duplicate queue_order -> ErrQueueOrderConflict.
	if err := s.PutDownloadJob(ctx, DownloadJob{
		ID:          "job-order-dup",
		Key:         k4,
		QueueOrder:  int64Ptr(1),
		State:       JobWaiting,
		CreatedAtMS: 13,
		UpdatedAtMS: 13,
	}); err != ErrQueueOrderConflict {
		t.Fatalf("expected ErrQueueOrderConflict, got %v", err)
	}

	// Reassigning existing job ID to another book -> ErrInvalidInput.
	if err := s.PutDownloadJob(ctx, DownloadJob{
		ID:          "job-wait-2",
		Key:         k4,
		QueueOrder:  int64Ptr(0),
		State:       JobWaiting,
		CreatedAtMS: 7,
		UpdatedAtMS: 14,
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for changing job's book, got %v", err)
	}

	// Reorder waiting queue atomically (swap 0 and 1).
	must(t, s.ReorderDownloadQueue(ctx, []string{"job-wait-3", "job-wait-2"}, 20))

	// Verify ordering: active first, then waiting by queue_order (job-wait-3 then job-wait-2), then terminal.
	jobs, err := s.DownloadJobs(ctx)
	must(t, err)
	wantOrder := []string{"job-active-1", "job-wait-3", "job-wait-2", "job-done", "job-canceled"}
	var gotOrder []string
	for _, j := range jobs {
		gotOrder = append(gotOrder, j.ID)
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("unexpected job order: got %v, want %v", gotOrder, wantOrder)
	}

	// ReorderDownloadQueue validation failures leave queue untouched.
	for _, err := range []error{
		s.ReorderDownloadQueue(ctx, []string{"job-wait-3", "job-wait-2"}, -1),
		s.ReorderDownloadQueue(ctx, []string{""}, 30),
		s.ReorderDownloadQueue(ctx, []string{"job-wait-3", "job-wait-3"}, 30),
		s.ReorderDownloadQueue(ctx, []string{"job-wait-3"}, 30),
		s.ReorderDownloadQueue(ctx, []string{"job-wait-3", "missing"}, 30),
		s.ReorderDownloadQueue(ctx, []string{"job-wait-3", "job-wait-2"}, 1), // < created_at_ms
	} {
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput from ReorderDownloadQueue, got %v", err)
		}
	}

	// Reconcile interrupted active job (not published -> failed/interrupted).
	reconciled, err := s.ReconcileActiveJobs(ctx, 25, nil)
	must(t, err)
	if len(reconciled) != 1 || reconciled[0].ID != "job-active-1" ||
		reconciled[0].State != JobFailed || reconciled[0].FailureCode != FailureInterrupted {
		t.Fatalf("unexpected reconciled job: %+v", reconciled)
	}

	// Reconcile when another active job WAS published -> ready.
	must(t, s.PutDownloadJob(ctx, DownloadJob{
		ID:            "job-pub-4",
		Key:           k4,
		State:         JobPublishing,
		BytesReceived: 200,
		BytesTotal:    int64Ptr(200),
		CreatedAtMS:   30,
		UpdatedAtMS:   31,
	}))
	if _, err := s.ReconcileActiveJobs(ctx, -1, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	if _, err := s.ReconcileActiveJobs(ctx, 10, nil); !errors.Is(err, ErrInvalidInput) { // < CreatedAtMS 30
		t.Fatal(err)
	}
	reconciled, err = s.ReconcileActiveJobs(ctx, 40, map[BookKey]bool{k4: true})
	must(t, err)
	if len(reconciled) != 1 || reconciled[0].ID != "job-pub-4" || reconciled[0].State != JobReady || reconciled[0].FailureCode != "" {
		t.Fatalf("expected published active job reconciled to ready: %+v", reconciled)
	}

	// After transition to failed/ready, k1 can be retried and empty queue reorder succeeds.
	must(t, s.PutDownloadJob(ctx, DownloadJob{
		ID:          "job-retry-k1",
		Key:         k1,
		QueueOrder:  int64Ptr(2),
		State:       JobWaiting,
		CreatedAtMS: 50,
		UpdatedAtMS: 50,
	}))

	// Invalid DownloadJob inputs before transaction.
	for _, bad := range []DownloadJob{
		{ID: "", Key: k4, State: JobReady},
		{ID: "bad", Key: BookKey{}, State: JobReady},
		{ID: "bad", Key: k4, State: "unknown"},
		{ID: "bad", Key: k4, State: JobWaiting},                                           // missing queue_order
		{ID: "bad", Key: k4, State: JobReady, QueueOrder: int64Ptr(0)},                    // queue_order on non-waiting
		{ID: "bad", Key: k4, State: JobWaiting, QueueOrder: int64Ptr(-1)},                 // negative queue_order
		{ID: "bad", Key: k4, State: JobFailed},                                            // missing failure_code
		{ID: "bad", Key: k4, State: JobReady, FailureCode: FailureInternal},               // failure_code on non-failed
		{ID: "bad", Key: k4, State: JobFailed, FailureCode: "raw_prose"},                  // non-allowlisted failure_code
		{ID: "bad", Key: k4, State: JobReady, BytesReceived: -1},                          // negative bytes
		{ID: "bad", Key: k4, State: JobReady, BytesReceived: 10, BytesTotal: int64Ptr(5)}, // total < received
		{ID: "bad", Key: k4, State: JobReady, CreatedAtMS: -1, UpdatedAtMS: 0},            // negative created
		{ID: "bad", Key: k4, State: JobReady, CreatedAtMS: 10, UpdatedAtMS: 9},            // updated < created
	} {
		if err := s.PutDownloadJob(ctx, bad); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for %+v, got %v", bad, err)
		}
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.DownloadJobs(canceled); err != context.Canceled {
		t.Fatal(err)
	}
	if _, err := s.ReconcileActiveJobs(canceled, 60, nil); err != context.Canceled {
		t.Fatal(err)
	}

	// Reopen durability & zero-waiting-queue reorder test on fresh store.
	must(t, s.Close(ctx))
	must(t, owner.Close(ctx))
	_, reopened := reopenStoreFixture(t, path)
	jobsAfter, err := reopened.DownloadJobs(ctx)
	must(t, err)
	if len(jobsAfter) != 7 {
		t.Fatalf("expected 7 jobs after reopen, got %d", len(jobsAfter))
	}

	_, _, emptyOwner := ownerFixture(t)
	emptyStore := storeFixture(t, emptyOwner)
	must(t, emptyStore.ReorderDownloadQueue(ctx, nil, 0))
}
