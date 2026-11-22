package ui

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// LibraryView identifies the active library sidebar destination.
type LibraryView string

const (
	ViewAll        LibraryView = "all"
	ViewContinue   LibraryView = "continue"
	ViewDownloaded LibraryView = "downloaded"
	ViewFinished   LibraryView = "finished"
)

// Label returns the human-readable sidebar label for v.
func (v LibraryView) Label() string {
	switch v {
	case ViewContinue:
		return "Continue Listening"
	case ViewDownloaded:
		return "Downloaded"
	case ViewFinished:
		return "Finished"
	default:
		return "All"
	}
}

// SortOrder identifies the active library sort order.
type SortOrder string

const (
	SortTitle       SortOrder = "title"
	SortAuthor      SortOrder = "author"
	SortDuration    SortOrder = "duration"
	SortRecent      SortOrder = "recent"
	SortAcquisition SortOrder = "acquisition"
)

type librarySidebarRow struct {
	label      string
	view       LibraryView
	selectable bool
}

type sortOption struct {
	order SortOrder
	label string
}

var sortOptions = []sortOption{
	{order: SortTitle, label: "Title (A-Z)"},
	{order: SortAuthor, label: "Author (A-Z)"},
	{order: SortDuration, label: "Duration (shortest first)"},
	{order: SortRecent, label: "Recent listening"},
	{order: SortAcquisition, label: "Acquisition (catalog order if unknown)"},
}

// ViewState returns the active library view, sort order, and applied filter query.
func (m Model) ViewState() (LibraryView, SortOrder, string) {
	return m.libraryView, m.sortOrder, m.filterQuery
}

// SortOpen reports whether the library sort menu overlay is open.
func (m Model) SortOpen() bool { return m.sortOpen }

// FilterEditing reports whether the local filter input owns keyboard input.
func (m Model) FilterEditing() bool { return m.filterEditing }

func (m Model) librarySidebarRows() []librarySidebarRow {
	counts := map[LibraryView]int{
		ViewAll:        len(m.data.Books),
		ViewContinue:   0,
		ViewDownloaded: 0,
		ViewFinished:   0,
	}
	for _, book := range m.data.Books {
		if book.InProgress {
			counts[ViewContinue]++
		}
		if book.Availability == Ready {
			counts[ViewDownloaded]++
		}
		if book.Finished {
			counts[ViewFinished]++
		}
	}
	return []librarySidebarRow{
		{label: "LIBRARY VIEWS", selectable: false},
		{label: fmt.Sprintf("All (%d)", counts[ViewAll]), view: ViewAll, selectable: true},
		{label: fmt.Sprintf("Continue (%d)", counts[ViewContinue]), view: ViewContinue, selectable: true},
		{label: fmt.Sprintf("Downloaded (%d)", counts[ViewDownloaded]), view: ViewDownloaded, selectable: true},
		{label: fmt.Sprintf("Finished (%d)", counts[ViewFinished]), view: ViewFinished, selectable: true},
	}
}

func (m Model) clampedLibraryCursor(rows []librarySidebarRow) int {
	if len(rows) == 0 {
		return -1
	}
	if m.libraryCursor >= 0 && m.libraryCursor < len(rows) && rows[m.libraryCursor].selectable {
		return m.libraryCursor
	}
	for i, row := range rows {
		if row.selectable {
			return i
		}
	}
	return -1
}

func moveSidebarCursor(cursor int, rows []librarySidebarRow, key string, page int) int {
	var selectable []int
	for i, row := range rows {
		if row.selectable {
			selectable = append(selectable, i)
		}
	}
	if len(selectable) == 0 {
		return -1
	}
	pos := slices.Index(selectable, cursor)
	if pos < 0 {
		pos = 0
	}
	nextPos := moveListCursor(pos, len(selectable), key, page)
	return selectable[nextPos]
}

func matchesView(book Book, view LibraryView) bool {
	switch view {
	case ViewContinue:
		return book.InProgress
	case ViewDownloaded:
		return book.Availability == Ready
	case ViewFinished:
		return book.Finished
	default:
		return true
	}
}

func matchesFilter(book Book, query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return true
	}
	for _, field := range []string{book.Title, book.Subtitle, book.Author, book.Narrator} {
		if strings.Contains(strings.ToLower(field), q) {
			return true
		}
	}
	return false
}

func hasKnownAuthor(author string) bool {
	trimmed := strings.TrimSpace(author)
	return trimmed != "" && trimmed != "Unknown author"
}

func compareBooks(a, b Book, order SortOrder) int {
	switch order {
	case SortAuthor:
		aKnown, bKnown := hasKnownAuthor(a.Author), hasKnownAuthor(b.Author)
		if aKnown != bKnown {
			if aKnown {
				return -1
			}
			return 1
		}
		if c := cmp.Compare(strings.ToLower(a.Author), strings.ToLower(b.Author)); c != 0 {
			return c
		}
	case SortDuration:
		if a.HasDuration != b.HasDuration {
			if a.HasDuration {
				return -1
			}
			return 1
		}
		if a.HasDuration {
			if c := cmp.Compare(a.Duration, b.Duration); c != 0 {
				return c
			}
		}
	case SortRecent:
		aHas, bHas := a.RecentAtMS > 0, b.RecentAtMS > 0
		if aHas != bHas {
			if aHas {
				return -1
			}
			return 1
		}
		if c := cmp.Compare(b.RecentAtMS, a.RecentAtMS); c != 0 {
			return c
		}
	case SortAcquisition:
		aHas, bHas := a.AcquisitionAtMS != nil, b.AcquisitionAtMS != nil
		if aHas != bHas {
			if aHas {
				return -1
			}
			return 1
		}
		if aHas {
			if c := cmp.Compare(*b.AcquisitionAtMS, *a.AcquisitionAtMS); c != 0 {
				return c
			}
		}
		if c := cmp.Compare(a.CatalogOrder, b.CatalogOrder); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	}
	if c := cmp.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Title, b.Title); c != 0 {
		return c
	}
	if c := cmp.Compare(a.CatalogOrder, b.CatalogOrder); c != 0 {
		return c
	}
	return cmp.Compare(a.Key, b.Key)
}

func (m Model) visibleBooks() []Book {
	out := make([]Book, 0, len(m.data.Books))
	for _, book := range m.data.Books {
		if matchesView(book, m.libraryView) && matchesFilter(book, m.filterQuery) {
			out = append(out, book)
		}
	}
	slices.SortStableFunc(out, func(a, b Book) int {
		return compareBooks(a, b, m.sortOrder)
	})
	return out
}

func (m Model) visibleBookIndex(visible []Book) int {
	return slices.IndexFunc(visible, func(book Book) bool { return book.Key == m.selected })
}

func (m *Model) reconcileSelection() {
	visible := m.visibleBooks()
	if len(visible) == 0 {
		m.selected = ""
		m.chapterCursor = 0
		return
	}
	if idx := m.visibleBookIndex(visible); idx >= 0 {
		m.chapterCursor = max(0, m.clampedChapterCursor(visible[idx]))
		return
	}
	m.selected = visible[0].Key
	m.chapterCursor = 0
}

func (m *Model) openSortMenu() {
	if m.focus == PlayerPane {
		m.data.Activity = "Sort unavailable in Player"
		return
	}
	m.sortOpen = true
	m.sortCursor = 0
	for i, opt := range sortOptions {
		if opt.order == m.sortOrder {
			m.sortCursor = i
			break
		}
	}
}

func (m Model) sortKey(key string) Model {
	switch key {
	case "esc":
		m.sortOpen = false
	case "j", "down", "tab":
		m.sortCursor = (m.sortCursor + 1) % len(sortOptions)
	case "k", "up", "shift+tab":
		m.sortCursor = (m.sortCursor + len(sortOptions) - 1) % len(sortOptions)
	case "enter":
		m.sortOrder = sortOptions[m.sortCursor].order
		m.sortOpen = false
		m.reconcileSelection()
	}
	return m
}

func (m Model) sortLines(lines []string) []string {
	r := m.layout.Dialog(76, 11)
	rows := []string{"Choose library sort order (preserves selected book):"}
	selectedRow := len(rows) + m.sortCursor
	for _, opt := range sortOptions {
		prefix := "  "
		if opt.order == m.sortOrder {
			prefix = "* "
		}
		rows = append(rows, prefix+opt.label)
	}
	box := m.box(r, " [Focus] Sort Library ", rows, selectedRow, true, false)
	for y, row := range box {
		line := lines[r.Y+y]
		lines[r.Y+y] = ansi.Cut(line, 0, r.X) + row + ansi.Cut(line, r.X+r.Width, m.layout.Content.Width)
	}
	return lines
}

func (m *Model) openFilter() {
	if m.focus == Details || m.focus == PlayerPane {
		m.data.Activity = "Filter unavailable in " + paneNames[m.focus]
		return
	}
	m.filterEditing = true
	m.filterPrev = m.filterQuery
	m.filterDraft = m.filterQuery
}

func (m Model) filterKey(key, text string) Model {
	switch key {
	case "enter":
		m.filterQuery = m.filterDraft
		m.filterEditing = false
		m.reconcileSelection()
	case "esc":
		m.filterQuery = m.filterPrev
		m.filterDraft = m.filterPrev
		m.filterEditing = false
		m.reconcileSelection()
	case "backspace":
		runes := []rune(m.filterDraft)
		if len(runes) > 0 {
			m.filterDraft = string(runes[:len(runes)-1])
			m.filterQuery = m.filterDraft
			m.reconcileSelection()
		}
	case "ctrl+u":
		m.filterDraft = ""
		m.filterQuery = ""
		m.reconcileSelection()
	case "space":
		m.filterDraft += " "
		m.filterQuery = m.filterDraft
		m.reconcileSelection()
	default:
		if clean := cleanLine(text); clean != "" {
			m.filterDraft += clean
			m.filterQuery = m.filterDraft
			m.reconcileSelection()
		}
	}
	return m
}

func cloneBook(book Book) Book {
	book.Chapters = slices.Clone(book.Chapters)
	book.ChapterStarts = slices.Clone(book.ChapterStarts)
	if book.AcquisitionAtMS != nil {
		v := *book.AcquisitionAtMS
		book.AcquisitionAtMS = &v
	}
	return book
}

func formatDurationLabel(d time.Duration, known bool) string {
	if !known {
		return "Duration unknown"
	}
	return "Duration " + clock(d)
}
