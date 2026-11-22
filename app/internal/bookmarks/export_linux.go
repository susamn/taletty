package bookmarks

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/susamn/taletty/app/internal/store"
	"golang.org/x/sys/unix"
)

var (
	// ErrExportExists reports that the target Markdown export file already exists
	// and Overwrite was not confirmed.
	ErrExportExists = errors.New("export file already exists")
	// ErrUnsafeExportTarget reports that the export directory or target file has
	// unsafe permissions, ownership, or non-regular file type.
	ErrUnsafeExportTarget = errors.New("export directory or target file is unsafe")
)

var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-_]`)

type allBookmarksStore interface {
	AllBookmarks(ctx context.Context) ([]store.Bookmark, error)
}

// ExportEntry pairs a persisted bookmark with safe display metadata for Markdown export.
type ExportEntry struct {
	Bookmark  store.Bookmark
	BookTitle string
	Author    string
}

// ExportOptions configures a safe Markdown export under ~/.config/taletty/exports/.
type ExportOptions struct {
	Filename  string
	Heading   string
	Entries   []ExportEntry
	Overwrite bool
}

// ExportResult describes a completed atomic Markdown export.
type ExportResult struct {
	RelativePath string
	Count        int
}

// ListAll returns all bookmarks across all books offline, ordered by BookKey,
// ascending PositionNS, and bookmark ID.
func (s *Service) ListAll(ctx context.Context) ([]store.Bookmark, error) {
	if s == nil || s.store == nil || ctx == nil {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	allStore, ok := s.store.(allBookmarksStore)
	if !ok {
		return nil, ErrInvalidInput
	}
	items, err := allStore.AllBookmarks(ctx)
	if err != nil {
		return nil, err
	}
	out := cloneBookmarks(items)
	slices.SortFunc(out, func(a, b store.Bookmark) int {
		if c := cmp.Compare(a.Key.Provider, b.Key.Provider); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Key.Namespace, b.Key.Namespace); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Key.ID, b.Key.ID); c != 0 {
			return c
		}
		if c := cmp.Compare(a.PositionNS, b.PositionNS); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out, nil
}

// ValidateExportFilename validates a single safe Markdown basename (no slashes,
// traversal, hidden dotfiles, or control characters) and normalizes a .md suffix.
func ValidateExportFilename(name string) (string, error) {
	if !utf8.ValidString(name) {
		return "", ErrInvalidInput
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || len(trimmed) > 128 {
		return "", ErrInvalidInput
	}
	if strings.ContainsAny(trimmed, "/\\") || strings.Contains(trimmed, "..") || strings.HasPrefix(trimmed, ".") {
		return "", ErrInvalidInput
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", ErrInvalidInput
		}
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' || r == ' ') {
			return "", ErrInvalidInput
		}
	}
	if !strings.HasSuffix(strings.ToLower(trimmed), ".md") {
		trimmed += ".md"
	}
	if len(trimmed) <= len(".md") || len(trimmed) > 128 {
		return "", ErrInvalidInput
	}
	return trimmed, nil
}

// SafeExportFilename derives a safe .md basename from a book title or scope label.
func SafeExportFilename(label string) string {
	cleaned := stripControlsSingleLine(label)
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(cleaned) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
		if b.Len() >= 48 {
			break
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "bookmarks-export.md"
	}
	return "bookmarks-" + slug + ".md"
}

// FormatMarkdown renders entries into escaped, ANSI-free Markdown without
// exposing credentials, signed URLs, or local filesystem paths.
func FormatMarkdown(heading string, entries []ExportEntry) string {
	var b strings.Builder
	title := escapeMarkdownInline(stripControlsSingleLine(heading))
	if title == "" {
		title = "Taletty Bookmarks"
	}
	b.WriteString("# ")
	b.WriteString(title)
	b.WriteString("\n\n")
	if len(entries) == 0 {
		b.WriteString("_No bookmarks._\n")
		return b.String()
	}

	var lastKey *store.BookKey
	for _, entry := range entries {
		if lastKey == nil || *lastKey != entry.Bookmark.Key {
			k := entry.Bookmark.Key
			lastKey = &k
			bookTitle := escapeMarkdownInline(stripControlsSingleLine(entry.BookTitle))
			if bookTitle == "" {
				bookTitle = "Audiobook"
			}
			b.WriteString("## ")
			b.WriteString(bookTitle)
			if author := escapeMarkdownInline(stripControlsSingleLine(entry.Author)); author != "" {
				b.WriteString(" — ")
				b.WriteString(author)
			}
			b.WriteString("\n\n")
		}
		ch := "Chapter unavailable"
		if entry.Bookmark.ChapterTitle != nil {
			if cleaned := escapeMarkdownInline(stripControlsSingleLine(*entry.Bookmark.ChapterTitle)); cleaned != "" {
				ch = cleaned
			}
		}
		b.WriteString("- **")
		b.WriteString(formatExportClock(time.Duration(max(int64(0), entry.Bookmark.PositionNS))))
		b.WriteString("** (")
		b.WriteString(ch)
		b.WriteString(")\n")
		noteLines := strings.Split(stripControlsMultiline(entry.Bookmark.Note), "\n")
		for _, line := range noteLines {
			b.WriteString("  ")
			b.WriteString(escapeMarkdownInline(line))
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// ExportMarkdown writes escaped Markdown atomically under ~/.config/taletty/exports/
// using anchored O_NOFOLLOW descriptors and explicit overwrite confirmation.
func (s *Service) ExportMarkdown(ctx context.Context, opts ExportOptions) (ExportResult, error) {
	if s == nil || ctx == nil {
		return ExportResult{}, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return ExportResult{}, err
	}
	filename, err := ValidateExportFilename(opts.Filename)
	if err != nil {
		return ExportResult{}, err
	}
	if s.rootOwner == nil {
		return ExportResult{}, ErrUnsafeExportTarget
	}

	lease, err := s.rootOwner.Lease(ctx)
	if err != nil {
		return ExportResult{}, err
	}
	if lease == nil || lease.Root == nil {
		return ExportResult{}, ErrUnsafeExportTarget
	}
	defer lease.Close()
	if err := lease.Context.Err(); err != nil {
		return ExportResult{}, err
	}

	euid := uint32(os.Geteuid())
	rootFD := int(lease.Root.Fd())
	var rootStat unix.Stat_t
	if err := unix.Fstat(rootFD, &rootStat); err != nil {
		return ExportResult{}, ErrUnsafeExportTarget
	}
	if rootStat.Mode&(unix.S_IFMT|07777) != unix.S_IFDIR|0700 || rootStat.Uid != euid {
		return ExportResult{}, ErrUnsafeExportTarget
	}

	if err := unix.Mkdirat(rootFD, "exports", 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return ExportResult{}, err
	}
	exportsFD, err := unix.Openat(rootFD, "exports", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return ExportResult{}, ErrUnsafeExportTarget
	}
	defer unix.Close(exportsFD)

	var expStat unix.Stat_t
	if err := unix.Fstat(exportsFD, &expStat); err != nil {
		return ExportResult{}, ErrUnsafeExportTarget
	}
	if expStat.Mode&(unix.S_IFMT|07777) != unix.S_IFDIR|0700 || expStat.Uid != euid {
		return ExportResult{}, ErrUnsafeExportTarget
	}

	var targetStat unix.Stat_t
	err = unix.Fstatat(exportsFD, filename, &targetStat, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil {
		if targetStat.Mode&(unix.S_IFMT|07777) != unix.S_IFREG|0600 || targetStat.Uid != euid || targetStat.Nlink != 1 {
			return ExportResult{}, ErrUnsafeExportTarget
		}
		if !opts.Overwrite {
			return ExportResult{}, ErrExportExists
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return ExportResult{}, ErrUnsafeExportTarget
	}

	content := []byte(FormatMarkdown(opts.Heading, opts.Entries))
	var randBuf [8]byte
	_, _ = rand.Read(randBuf[:])
	tmpName := ".export-" + hex.EncodeToString(randBuf[:]) + ".tmp"
	tmpFD, err := unix.Openat(exportsFD, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return ExportResult{}, err
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = unix.Unlinkat(exportsFD, tmpName, 0)
		}
	}()

	for written := 0; written < len(content); {
		n, writeErr := unix.Write(tmpFD, content[written:])
		if writeErr != nil {
			_ = unix.Close(tmpFD)
			return ExportResult{}, writeErr
		}
		written += n
	}
	if err := unix.Fsync(tmpFD); err != nil {
		_ = unix.Close(tmpFD)
		return ExportResult{}, err
	}
	if err := unix.Close(tmpFD); err != nil {
		return ExportResult{}, err
	}
	if err := unix.Renameat(exportsFD, tmpName, exportsFD, filename); err != nil {
		return ExportResult{}, err
	}
	cleanupTmp = false
	if err := unix.Fsync(exportsFD); err != nil {
		return ExportResult{}, err
	}
	return ExportResult{
		RelativePath: "exports/" + filename,
		Count:        len(opts.Entries),
	}, nil
}

func formatExportClock(d time.Duration) string {
	seconds := int64(max(0, d) / time.Second)
	return fmt.Sprintf("%02d:%02d:%02d", seconds/3600, seconds/60%60, seconds%60)
}

func stripControlsSingleLine(text string) string {
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "")
	}
	text = ansiEscapePattern.ReplaceAllString(text, "")
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, text))
}

func stripControlsMultiline(text string) string {
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "")
	}
	text = ansiEscapePattern.ReplaceAllString(text, "")
	var b strings.Builder
	for _, r := range text {
		if unicode.Is(unicode.Cf, r) {
			continue
		}
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func escapeMarkdownInline(text string) string {
	var b strings.Builder
	for _, r := range text {
		switch r {
		case '\\', '`', '*', '_', '{', '}', '[', ']', '(', ')', '#', '+', '-', '!', '|', '~':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
