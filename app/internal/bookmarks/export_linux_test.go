package bookmarks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/susamn/taletty/app/internal/store"
)

func TestListAllAndSafeMarkdownExport(t *testing.T) {
	ctx := context.Background()
	f := openBookmarkFixture(t)
	bookA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B00ORCHARD"}
	bookB := store.BookKey{Provider: "audible", Namespace: "us", ID: "B00RIVERS1"}

	chTitle := "Ch 1 [Evil](javascript:alert(1)) <script>"
	must(t, f.store.PutBookmark(ctx, store.Bookmark{
		ID: "bm-b1", Key: bookB, PositionNS: int64(2 * time.Minute),
		Note: "Rivers note", CreatedAtMS: 100, UpdatedAtMS: 100,
	}))
	must(t, f.store.PutBookmark(ctx, store.Bookmark{
		ID: "bm-a1", Key: bookA, PositionNS: int64(30 * time.Second),
		ChapterTitle: &chTitle,
		Note:         "# Heading\n<img src=x onerror=alert(1)>\n[link](https://evil.example)\n```fence```",
		CreatedAtMS:  100, UpdatedAtMS: 100,
	}))

	all, err := f.service.ListAll(ctx)
	must(t, err)
	if len(all) != 2 || all[0].Key != bookA || all[1].Key != bookB {
		t.Fatalf("unexpected ListAll order: %+v", all)
	}

	// Filename validation rejects traversal, slashes, dotfiles, controls, and oversized names.
	for _, bad := range []string{
		"", "   ", "../auth.json", "sub/file.md", "a\\b.md", ".hidden.md",
		".md", "bad\x00.md", "bad\n.md", "bad:name.md", string([]byte{0xff, 0xfe}),
		strings.Repeat("a", 129),
	} {
		if _, err := ValidateExportFilename(bad); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for %q, got %v", bad, err)
		}
	}
	if got, err := ValidateExportFilename("my-notes"); err != nil || got != "my-notes.md" {
		t.Fatalf("expected normalized .md suffix, got %q err=%v", got, err)
	}
	if got := SafeExportFilename("A Map of Quiet Rivers! \x1b[2J"); got != "bookmarks-a-map-of-quiet-rivers.md" {
		t.Fatalf("unexpected SafeExportFilename: %q", got)
	}
	if got := SafeExportFilename("!!!"); got != "bookmarks-export.md" {
		t.Fatalf("expected fallback filename, got %q", got)
	}

	// Export with malicious title/chapter/note (including ANSI escapes) escapes Markdown/HTML and strips ANSI.
	all[0].Note += "\x1b[2J"
	entries := []ExportEntry{
		{Bookmark: all[0], BookTitle: "# Malicious <script>Title</script>", Author: "Author [x](y)"},
		{Bookmark: all[1], BookTitle: "", Author: ""},
	}
	res, err := f.service.ExportMarkdown(ctx, ExportOptions{
		Filename: "all-notes",
		Heading:  "All Bookmarks",
		Entries:  entries,
	})
	must(t, err)
	if res.RelativePath != "exports/all-notes.md" || res.Count != 2 {
		t.Fatalf("unexpected ExportResult: %+v", res)
	}
	exportedPath := filepath.Join(f.rootDir, "exports", "all-notes.md")
	info, err := os.Stat(exportedPath)
	must(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 export file, got %o", info.Mode().Perm())
	}
	raw, err := os.ReadFile(exportedPath)
	must(t, err)
	text := string(raw)
	for _, forbidden := range []string{"<script>", "<img", "\x1b", "auth.json", "Atna|"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("export leaked unescaped/unsafe content %q:\n%s", forbidden, text)
		}
	}
	for _, want := range []string{`\# Malicious &lt;script&gt;Title&lt;/script&gt;`, `\[link\]\(https://evil.example\)`, `## Audiobook`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing escaped content %q:\n%s", want, text)
		}
	}

	// Existing file without Overwrite returns ErrExportExists and preserves original bytes.
	if _, err := f.service.ExportMarkdown(ctx, ExportOptions{
		Filename: "all-notes.md",
		Heading:  "New",
		Entries:  nil,
	}); !errors.Is(err, ErrExportExists) {
		t.Fatalf("expected ErrExportExists, got %v", err)
	}
	rawAfterCancel, _ := os.ReadFile(exportedPath)
	if string(rawAfterCancel) != text {
		t.Fatal("ErrExportExists modified existing file")
	}

	// Overwrite=true replaces atomically.
	res2, err := f.service.ExportMarkdown(ctx, ExportOptions{
		Filename:  "all-notes.md",
		Heading:   "Empty Export",
		Entries:   nil,
		Overwrite: true,
	})
	must(t, err)
	if res2.Count != 0 {
		t.Fatalf("expected 0 count, got %+v", res2)
	}
	rawOverwritten, _ := os.ReadFile(exportedPath)
	if !strings.Contains(string(rawOverwritten), "_No bookmarks._") {
		t.Fatalf("overwrite did not update file:\n%s", string(rawOverwritten))
	}

	// Unsafe symlink/hardlink/mode targets fail closed with ErrUnsafeExportTarget.
	symPath := filepath.Join(f.rootDir, "exports", "sym.md")
	must(t, os.Symlink(exportedPath, symPath))
	if _, err := f.service.ExportMarkdown(ctx, ExportOptions{Filename: "sym.md", Overwrite: true}); !errors.Is(err, ErrUnsafeExportTarget) {
		t.Fatalf("expected ErrUnsafeExportTarget for symlink, got %v", err)
	}
	badModePath := filepath.Join(f.rootDir, "exports", "world.md")
	must(t, os.WriteFile(badModePath, []byte("keep"), 0o644))
	if _, err := f.service.ExportMarkdown(ctx, ExportOptions{Filename: "world.md", Overwrite: true}); !errors.Is(err, ErrUnsafeExportTarget) {
		t.Fatalf("expected ErrUnsafeExportTarget for 0644 file, got %v", err)
	}

	// Service without RootOwner fails closed.
	noRootSvc, err := NewService(Options{Store: f.store, Player: f.player})
	must(t, err)
	if _, err := noRootSvc.ExportMarkdown(ctx, ExportOptions{Filename: "x.md"}); !errors.Is(err, ErrUnsafeExportTarget) {
		t.Fatalf("expected ErrUnsafeExportTarget without RootOwner, got %v", err)
	}

	// Canceled context & invalid inputs.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.service.ListAll(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on ListAll, got %v", err)
	}
	if _, err := f.service.ExportMarkdown(canceled, ExportOptions{Filename: "x.md"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on ExportMarkdown, got %v", err)
	}
	if _, err := (*Service)(nil).ListAll(ctx); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("expected ErrInvalidInput on nil Service.ListAll")
	}
	if _, err := (*Service)(nil).ExportMarkdown(ctx, ExportOptions{Filename: "x.md"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("expected ErrInvalidInput on nil Service.ExportMarkdown")
	}

	// ListAll sorting across providers/namespaces/positions/IDs and store without AllBookmarks.
	must(t, f.store.PutBookmark(ctx, store.Bookmark{
		ID: "bm-a0", Key: bookA, PositionNS: int64(30 * time.Second),
		Note: "Tie position", CreatedAtMS: 100, UpdatedAtMS: 100,
	}))
	must(t, f.store.PutBookmark(ctx, store.Bookmark{
		ID: "bm-loc", Key: store.BookKey{Provider: "local", Namespace: "user", ID: "book-1"}, PositionNS: 0,
		Note: "Local book", CreatedAtMS: 100, UpdatedAtMS: 100,
	}))
	allSorted, err := f.service.ListAll(ctx)
	must(t, err)
	if len(allSorted) != 4 || allSorted[0].ID != "bm-a0" || allSorted[3].Key.Provider != "local" {
		t.Fatalf("unexpected ListAll sort order: %+v", allSorted)
	}
	noAllSvc, err := NewService(Options{Store: &faultBookmarkStore{inner: f.store}, Player: f.player})
	must(t, err)
	if _, err := noAllSvc.ListAll(ctx); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput when store lacks AllBookmarks, got %v", err)
	}

	// Invalid UTF-8 and Cf runes stripped cleanly in single-line and multiline helpers.
	if got := stripControlsSingleLine("Bad\xff\u200b\nTitle"); got != "Bad  Title" {
		t.Fatalf("unexpected stripControlsSingleLine: %q", got)
	}
	if got := stripControlsMultiline("Line\xff\u200b\x01\nTwo"); got != "Line\nTwo" {
		t.Fatalf("unexpected stripControlsMultiline: %q", got)
	}
	if _, err := ValidateExportFilename(strings.Repeat("a", 126)); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("expected ErrInvalidInput when .md suffix exceeds 128 bytes")
	}

	// Unsafe exports directory (0755) and unsafe root (0755) fail closed.
	must(t, os.Chmod(filepath.Join(f.rootDir, "exports"), 0o755))
	if _, err := f.service.ExportMarkdown(ctx, ExportOptions{Filename: "new.md"}); !errors.Is(err, ErrUnsafeExportTarget) {
		t.Fatalf("expected ErrUnsafeExportTarget for 0755 exports dir, got %v", err)
	}
	must(t, os.Chmod(filepath.Join(f.rootDir, "exports"), 0o700))
	must(t, os.Chmod(f.rootDir, 0o755))
	if _, err := f.service.ExportMarkdown(ctx, ExportOptions{Filename: "new.md"}); !errors.Is(err, ErrUnsafeExportTarget) {
		t.Fatalf("expected ErrUnsafeExportTarget for 0755 root dir, got %v", err)
	}
	must(t, os.Chmod(f.rootDir, 0o700))
}
