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
