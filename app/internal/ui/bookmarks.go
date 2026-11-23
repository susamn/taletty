package ui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Bookmark is a display projection of a persisted timestamped bookmark.
type Bookmark struct {
	ID           string
	Key          string
	BookTitle    string
	Position     time.Duration
	ChapterID    string
	ChapterTitle string
	Note         string
	CreatedAtMS  int64
	UpdatedAtMS  int64
}

// OpenBookmarksMsg opens or populates the Bookmarks Manager for Key (or Global search).
type OpenBookmarksMsg struct {
	Key       string
	Global    bool
	Title     string
	Bookmarks []Bookmark
	Error     string
}

// BookmarksMsg updates the bookmark list for Key (or Global search) only when
// the manager is currently open for that exact fixed target.
type BookmarksMsg struct {
	Key       string
	Global    bool
	Bookmarks []Bookmark
	Error     string
}

type bookmarkArea int

const (
	bmFocusList bookmarkArea = iota
	bmFocusNote
	bmFocusPlayer
)

type bookmarkMenuOption struct {
	label string
	kind  string
}

var bookmarkMenuOptions = []bookmarkMenuOption{
	{label: "Jump to bookmark", kind: "jump"},
	{label: "Edit note", kind: "edit"},
	{label: "Delete bookmark", kind: "delete"},
}

// BookmarksOpen reports whether the Bookmarks Manager view is currently open.
func (m Model) BookmarksOpen() bool { return m.bookmarksOpen }

// BookmarksGlobal reports whether the Bookmarks Manager is in global search mode.
func (m Model) BookmarksGlobal() bool { return m.bookmarksOpen && m.bookmarksGlobal }

// BookmarksTarget returns the fixed BookKey currently targeted by the Bookmarks Manager.
func (m Model) BookmarksTarget() string { return m.bookmarksTarget }

func cloneUIBookmarks(items []Bookmark) []Bookmark {
	if len(items) == 0 {
		return nil
	}
	return slices.Clone(items)
}

func (m Model) bookTitleByKey(key string) string {
	for _, b := range m.data.Books {
		if b.Key == key {
			return b.Title
		}
	}
	if m.data.Player.Key == key && m.data.Player.Title != "" {
		return m.data.Player.Title
	}
	return "Audiobook"
}

func bookmarkChapterLabel(bm Bookmark) string {
	if strings.TrimSpace(cleanLine(bm.ChapterTitle)) == "" {
		return "Chapter unavailable"
	}
	return cleanLine(bm.ChapterTitle)
}

func (m *Model) openBookmarksLoading(targetKey, targetTitle string, global bool) {
	m.bookmarksOpen = true
	m.bookmarksGlobal = global
	m.bookmarksOrigin = m.focus
	m.bookmarksTarget = targetKey
	m.bookmarksTitle = targetTitle
	m.bookmarksLoading = true
	m.bookmarksError = ""
	m.bookmarks = nil
	m.bookmarkCursor = 0
	m.bookmarkFocus = bmFocusList
	m.bookmarkNoteOff = 0
	m.bmFilterQuery = ""
	m.bmFilterDraft = ""
	m.bmFilterPrev = ""
	m.bmFilterEditing = false
	m.bmMenuOpen = false
	m.bmMenuCursor = 0
	m.bmMenuTargetID = ""
	m.bmMenuTargetKey = ""
	m.bmMenuStale = false
}

func (m *Model) applyOpenBookmarks(msg OpenBookmarksMsg) {
	if !msg.Global && msg.Key == "" {
		return
	}
	if m.bookmarksOpen {
		if msg.Global != m.bookmarksGlobal || (!msg.Global && msg.Key != m.bookmarksTarget) {
			return
		}
		if msg.Title != "" {
			m.bookmarksTitle = msg.Title
		}
		m.bookmarksLoading = false
		m.bookmarksError = cleanLine(msg.Error)
		if msg.Error == "" {
			m.bookmarks = cloneUIBookmarks(msg.Bookmarks)
			m.reconcileBookmarkSelection("")
		}
		return
	}
	if m.ownsModal() || m.help || m.accountOpen || m.sortOpen {
		return
	}
	targetTitle := msg.Title
	if msg.Global {
		if targetTitle == "" {
			targetTitle = "All Books (Search)"
		}
		m.openBookmarksLoading("", targetTitle, true)
	} else {
		if targetTitle == "" {
			targetTitle = m.bookTitleByKey(msg.Key)
		}
		m.openBookmarksLoading(msg.Key, targetTitle, false)
	}
	m.bookmarksLoading = false
	m.bookmarksError = cleanLine(msg.Error)
	if msg.Error == "" {
		m.bookmarks = cloneUIBookmarks(msg.Bookmarks)
		m.reconcileBookmarkSelection("")
	}
}

func (m *Model) applyBookmarksUpdate(msg BookmarksMsg) {
	if !m.bookmarksOpen {
		return
	}
	if msg.Global {
		if !m.bookmarksGlobal {
			return
		}
	} else if m.bookmarksGlobal || msg.Key == "" || msg.Key != m.bookmarksTarget {
		return
	}
	m.bookmarksLoading = false
	m.bookmarksError = cleanLine(msg.Error)
	if msg.Error != "" {
		return
	}
	prevID := ""
	if bm, ok := m.selectedBookmark(); ok {
		prevID = bm.ID
	}
	m.bookmarks = cloneUIBookmarks(msg.Bookmarks)
	m.reconcileBookmarkSelection(prevID)
	if m.bmMenuOpen && m.bmMenuTargetID != "" {
		if _, ok := m.bookmarkByKeyAndID(m.bmMenuTargetKey, m.bmMenuTargetID); !ok {
			m.bmMenuStale = true
		}
	}
}

func (m Model) bookmarkByKeyAndID(key, id string) (Bookmark, bool) {
	for _, bm := range m.bookmarks {
		if bm.ID == id && (key == "" || bm.Key == key) {
			return bm, true
		}
	}
	return Bookmark{}, false
}

func (m Model) visibleBookmarks() []Bookmark {
	q := strings.ToLower(strings.TrimSpace(m.bmFilterQuery))
	if q == "" {
		return m.bookmarks
	}
	out := make([]Bookmark, 0, len(m.bookmarks))
	for _, bm := range m.bookmarks {
		if strings.Contains(strings.ToLower(bm.Note), q) ||
			strings.Contains(strings.ToLower(bm.ChapterTitle), q) ||
			strings.Contains(strings.ToLower(bm.BookTitle), q) {
			out = append(out, bm)
		}
	}
	return out
}

func (m Model) selectedBookmark() (Bookmark, bool) {
	visible := m.visibleBookmarks()
	if len(visible) == 0 || m.bookmarkCursor < 0 || m.bookmarkCursor >= len(visible) {
		return Bookmark{}, false
	}
	return visible[m.bookmarkCursor], true
}

func (m *Model) reconcileBookmarkSelection(preferID string) {
	visible := m.visibleBookmarks()
	if len(visible) == 0 {
		m.bookmarkCursor = 0
		m.bookmarkNoteOff = 0
		return
	}
	if preferID != "" {
		for i, bm := range visible {
			if bm.ID == preferID {
				m.bookmarkCursor = i
				return
			}
		}
	}
	m.bookmarkCursor = min(len(visible)-1, max(0, m.bookmarkCursor))
	m.bookmarkNoteOff = 0
}

func (m Model) triggerCaptureBookmark() (tea.Model, tea.Cmd) {
	if m.dispatch == nil {
		m.data.Activity = "Bookmark capture unavailable: no coordinator"
		return m, nil
	}
	if !m.data.Player.transportAvailable() {
		m.data.Activity = "Bookmark capture unavailable: no loaded audio"
		return m, nil
	}
	return m.request(Action{
		Kind:    CaptureBookmark,
		Key:     m.data.Player.Key,
		Session: m.data.Player.Session,
	})
}

func (m Model) triggerOpenBookmarks() (tea.Model, tea.Cmd) {
	var targetKey, targetTitle string
	if m.focus == PlayerPane {
		if !m.data.Player.transportAvailable() {
			m.data.Activity = "Bookmarks unavailable: no loaded audio in Player"
			return m, nil
		}
		targetKey = m.data.Player.Key
		targetTitle = m.data.Player.Title
	} else {
		if m.selected == "" {
			m.data.Activity = "Bookmarks unavailable: no highlighted book"
			return m, nil
		}
		targetKey = m.selected
		targetTitle = m.bookTitleByKey(m.selected)
	}
	if m.dispatch == nil {
		m.data.Activity = "Bookmarks unavailable: no coordinator"
		return m, nil
	}
	m.openBookmarksLoading(targetKey, targetTitle, false)
	return m.request(Action{Kind: OpenBookmarks, Key: targetKey})
}

func (m Model) triggerOpenGlobalBookmarks() (tea.Model, tea.Cmd) {
	if m.dispatch == nil {
		m.data.Activity = "Bookmarks unavailable: no coordinator"
		return m, nil
	}
	m.openBookmarksLoading("", "All Books (Search)", true)
	return m.request(Action{Kind: OpenGlobalBookmarks})
}

func (m Model) bookmarkBookTitle(bm Bookmark) string {
	if strings.TrimSpace(cleanLine(bm.BookTitle)) != "" {
		return bm.BookTitle
	}
	if bm.Key != "" {
		return m.bookTitleByKey(bm.Key)
	}
	return m.bookmarksTitle
}

func (m Model) bookmarksLayout() (Rect, Rect) {
	d := m.layout.Dialog(96, 24)
	if m.layout.Mode == Wide && d.Height >= 14 {
		listHeight := max(8, d.Height*3/5)
		noteHeight := max(4, d.Height-listHeight)
		return Rect{X: d.X, Y: d.Y, Width: d.Width, Height: listHeight},
			Rect{X: d.X, Y: d.Y + listHeight, Width: d.Width, Height: noteHeight}
	}
	listHeight := max(5, d.Height*2/3)
	noteHeight := max(1, d.Height-listHeight)
	return Rect{X: d.X, Y: d.Y, Width: d.Width, Height: listHeight},
		Rect{X: d.X, Y: d.Y + listHeight, Width: d.Width, Height: noteHeight}
}

func (m Model) bookmarkNoteRows(bm Bookmark, width int) []string {
	header := "Jump " + clock(bm.Position) + " | " + bookmarkChapterLabel(bm)
	if m.bookmarksGlobal {
		header = title(m.bookmarkBookTitle(bm)) + " | " + header
	}
	rows := []string{header}
	wrapped := strings.Split(ansi.Wrap(safeNote(bm.Note), max(1, width), ""), "\n")
	return append(rows, wrapped...)
}

func (m Model) bookmarksKey(key, text string) (tea.Model, tea.Cmd) {
	if m.bmMenuOpen {
		return m.bookmarkMenuKey(key)
	}
	if m.bmFilterEditing {
		return m.bookmarkFilterKey(key, text), nil
	}
	switch key {
	case "q", "ctrl+c":
		return m.request(Action{Kind: RequestQuit})
	case "esc":
		if m.bmFilterQuery != "" {
			m.bmFilterQuery = ""
			m.bmFilterDraft = ""
			m.reconcileBookmarkSelection("")
			return m, nil
		}
		m.bookmarksOpen = false
		m.focus = m.bookmarksOrigin
		if m.focus != PlayerPane {
			m.content = m.focus
		}
		return m, nil
	case "1", "2", "3", "4", "5":
		m.bookmarksOpen = false
		m.focus = Pane(key[0] - '1')
		if m.focus != PlayerPane {
			m.content = m.focus
		}
		return m, nil
	case "tab":
		m.bookmarkFocus = (m.bookmarkFocus + 1) % 3
		return m, nil
	case "shift+tab":
		m.bookmarkFocus = (m.bookmarkFocus + 2) % 3
		return m, nil
	case "?":
		m.help, m.helpOffset = true, 0
		return m, nil
	case "/":
		m.bmFilterEditing = true
		m.bmFilterPrev = m.bmFilterQuery
		m.bmFilterDraft = m.bmFilterQuery
		return m, nil
	case "b":
		return m.triggerCaptureBookmark()
	case "B":
		// Never retarget an already-open Bookmarks Manager.
		return m, nil
	case "r":
		if m.dispatch != nil {
			if m.bookmarksGlobal {
				m.bookmarksLoading = true
				m.bookmarksError = ""
				return m.request(Action{Kind: OpenGlobalBookmarks})
			}
			if m.bookmarksTarget != "" {
				m.bookmarksLoading = true
				m.bookmarksError = ""
				return m.request(Action{Kind: OpenBookmarks, Key: m.bookmarksTarget})
			}
		}
		return m, nil
	case "e":
		if m.bookmarksError != "" {
			m.data.Activity = "Reload bookmarks before exporting"
			return m, nil
		}
		if m.dispatch == nil {
			m.data.Activity = "Export unavailable: no coordinator"
			return m, nil
		}
		return m.request(Action{
			Kind: ExportBookmarks,
			Key:  m.bookmarksTarget,
			Text: m.bmFilterQuery,
		})
	case "left", "right":
		if m.bookmarkFocus == bmFocusPlayer {
			delta := 1
			if key == "left" {
				delta = -1
			}
			m.control = (m.control + delta + 3) % 3
		}
		return m, nil
	case "j", "down", "k", "up", "g", "home", "G", "end", "pgdown", "pgup", "ctrl+d", "ctrl+u":
		if m.bookmarkFocus == bmFocusNote {
			if bm, ok := m.selectedBookmark(); ok {
				_, noteRect := m.bookmarksLayout()
				rows := m.bookmarkNoteRows(bm, noteRect.Width-4)
				capacity := max(1, noteRect.Height-2)
				limit := max(0, len(rows)-capacity)
				m.bookmarkNoteOff = moveListCursor(min(m.bookmarkNoteOff, limit), limit+1, key, capacity)
			}
			return m, nil
		}
		if m.bookmarkFocus == bmFocusList {
			visible := m.visibleBookmarks()
			if len(visible) > 0 {
				listRect, _ := m.bookmarksLayout()
				prevID := visible[min(len(visible)-1, max(0, m.bookmarkCursor))].ID
				m.bookmarkCursor = moveListCursor(max(0, m.bookmarkCursor), len(visible), key, max(1, listRect.Height-2))
				if visible[m.bookmarkCursor].ID != prevID {
					m.bookmarkNoteOff = 0
				}
			}
		}
		return m, nil
	case "enter":
		if m.bookmarkFocus == bmFocusPlayer {
			return m.transport([]ActionKind{TogglePlayback, SeekBackward, SeekForward}[m.control])
		}
		if m.bookmarksError != "" && m.dispatch != nil {
			if m.bookmarksGlobal {
				m.bookmarksLoading = true
				m.bookmarksError = ""
				return m.request(Action{Kind: OpenGlobalBookmarks})
			}
			if m.bookmarksTarget != "" {
				m.bookmarksLoading = true
				m.bookmarksError = ""
				return m.request(Action{Kind: OpenBookmarks, Key: m.bookmarksTarget})
			}
		}
		bm, ok := m.selectedBookmark()
		if !ok {
			m.data.Activity = "No bookmark selected"
			return m, nil
		}
		targetKey := bm.Key
		if targetKey == "" {
			targetKey = m.bookmarksTarget
		}
		return m.request(Action{
			Kind:        JumpBookmark,
			Key:         targetKey,
			Session:     m.data.Player.Session,
			BookmarkID:  bm.ID,
			BookmarkPos: bm.Position,
		})
	case "x":
		if m.bookmarksError != "" {
			m.data.Activity = "Reload bookmarks before opening actions"
			return m, nil
		}
		bm, ok := m.selectedBookmark()
		if !ok {
			m.data.Activity = "No bookmark selected"
			return m, nil
		}
		m.bmMenuOpen = true
		m.bmMenuCursor = 0
		m.bmMenuTargetID = bm.ID
		m.bmMenuTargetKey = bm.Key
		m.bmMenuStale = false
		return m, nil
	default:
		if action, ok := transportKeys[key]; ok {
			return m.transport(action)
		}
		return m, nil
	}
}

func (m Model) bookmarkFilterKey(key, text string) Model {
	prevID := ""
	if bm, ok := m.selectedBookmark(); ok {
		prevID = bm.ID
	}
	switch key {
	case "enter":
		m.bmFilterQuery = m.bmFilterDraft
		m.bmFilterEditing = false
		m.reconcileBookmarkSelection(prevID)
	case "esc":
		m.bmFilterQuery = m.bmFilterPrev
		m.bmFilterDraft = m.bmFilterPrev
		m.bmFilterEditing = false
		m.reconcileBookmarkSelection(prevID)
	default:
		if next, changed := stepFilterDraft(m.bmFilterDraft, key, text); changed {
			m.bmFilterDraft = next
			m.bmFilterQuery = next
			m.reconcileBookmarkSelection(prevID)
		}
	}
	return m
}

func (m Model) bookmarkMenuKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc":
		m.bmMenuOpen = false
		m.bmMenuTargetID = ""
		m.bmMenuTargetKey = ""
		m.bmMenuStale = false
		return m, nil
	case "j", "down", "tab":
		if !m.bmMenuStale {
			m.bmMenuCursor = (m.bmMenuCursor + 1) % len(bookmarkMenuOptions)
		}
		return m, nil
	case "k", "up", "shift+tab":
		if !m.bmMenuStale {
			m.bmMenuCursor = (m.bmMenuCursor + len(bookmarkMenuOptions) - 1) % len(bookmarkMenuOptions)
		}
		return m, nil
	case "enter":
		if m.bmMenuStale {
			m.data.Activity = "Target bookmark no longer exists; press Esc"
			return m, nil
		}
		bm, ok := m.bookmarkByKeyAndID(m.bmMenuTargetKey, m.bmMenuTargetID)
		if !ok {
			m.bmMenuStale = true
			m.data.Activity = "Target bookmark no longer exists; press Esc"
			return m, nil
		}
		choice := bookmarkMenuOptions[m.bmMenuCursor]
		m.bmMenuOpen = false
		m.bmMenuTargetID = ""
		m.bmMenuTargetKey = ""
		m.bmMenuStale = false
		targetKey := bm.Key
		if targetKey == "" {
			targetKey = m.bookmarksTarget
		}
		targetTitle := m.bookmarkBookTitle(bm)
		switch choice.kind {
		case "jump":
			return m.request(Action{
				Kind:        JumpBookmark,
				Key:         targetKey,
				Session:     m.data.Player.Session,
				BookmarkID:  bm.ID,
				BookmarkPos: bm.Position,
			})
		case "edit":
			m.bmDialogSeq++
			m.openNote(OpenNoteEditorMsg{
				ID: fmt.Sprintf("bm-edit-%d", m.bmDialogSeq),
				Target: NoteTarget{
					Key:        targetKey,
					BookmarkID: bm.ID,
					Title:      targetTitle,
					Chapter:    bookmarkChapterLabel(bm),
					Captured:   bm.Position,
					Jump:       bm.Position,
				},
				Text: bm.Note,
			})
			return m, nil
		case "delete":
			m.bmDialogSeq++
			preview := cleanLine(bm.Note)
			if len([]rune(preview)) > 48 {
				preview = string([]rune(preview)[:45]) + "..."
			}
			m.confirmation = &confirmation{
				spec: OpenConfirmationMsg{
					ID:      fmt.Sprintf("bm-del-%d", m.bmDialogSeq),
					Title:   "Delete bookmark",
					Prompt:  fmt.Sprintf("Delete bookmark in %q at %s (%q)? Audio, history, and favorites are preserved.", title(targetTitle), clock(bm.Position), preview),
					Keep:    "Keep bookmark",
					Confirm: "Delete bookmark",
					Action: Action{
						Kind:       DeleteBookmark,
						Key:        targetKey,
						BookmarkID: bm.ID,
					},
				},
			}
			return m, nil
		}
	}
	return m, nil
}

func (m Model) bookmarksLines(lines []string) []string {
	listRect, noteRect := m.bookmarksLayout()
	listFocused := m.bookmarkFocus == bmFocusList && !m.ownsModal() && !m.help && !m.bmMenuOpen && !m.bmFilterEditing
	noteFocused := m.bookmarkFocus == bmFocusNote && !m.ownsModal() && !m.help && !m.bmMenuOpen && !m.bmFilterEditing

	listTitle := " Bookmarks - " + title(m.bookmarksTitle)
	if m.bmFilterQuery != "" {
		listTitle += fmt.Sprintf(" [Filter: %q]", cleanLine(m.bmFilterQuery))
	}
	if listFocused {
		listTitle = " [Focus]" + listTitle
	} else {
		listTitle += " "
	}

	var listRows []string
	selectedRow := -1
	tag := ""
	switch {
	case m.bookmarksLoading:
		listRows = []string{m.spinnerGlyph() + " Loading bookmarks..."}
	case m.bookmarksError != "":
		listRows = []string{
			"Failed to load bookmarks: " + m.bookmarksError,
			"Press r or Enter to retry; Esc back",
		}
	case len(m.bookmarks) == 0:
		if m.bookmarksGlobal {
			listRows = []string{"No bookmarks across library. Press b to capture current audio."}
		} else {
			listRows = []string{"No bookmarks for this book. Press b to capture current audio."}
		}
	default:
		visible := m.visibleBookmarks()
		if len(visible) == 0 {
			listRows = []string{"No bookmarks match filter (Esc clears)"}
		} else {
			if m.layout.Mode == Wide {
				header := "TIME     | CHAPTER | NOTE"
				if m.bookmarksGlobal {
					header = "BOOK | " + header
				}
				listRows = append(listRows, header)
			}
			for _, bm := range visible {
				row := clock(bm.Position) + " | " + bookmarkChapterLabel(bm) + " | " + cleanLine(bm.Note)
				if m.bookmarksGlobal {
					row = title(m.bookmarkBookTitle(bm)) + " | " + row
				}
				listRows = append(listRows, row)
			}
			cur := min(len(visible)-1, max(0, m.bookmarkCursor))
			tag = fmt.Sprintf("%d/%d", cur+1, len(visible))
			selectedRow = cur
			if m.layout.Mode == Wide {
				selectedRow++
			}
		}
	}
	listBox := m.boxTagged(listRect, listTitle, tag, listRows, selectedRow, listFocused, selectedRow >= 0)

	noteTitle := " Note "
	var noteRows []string
	if bm, ok := m.selectedBookmark(); ok {
		noteTitle = " Note - " + clock(bm.Position) + " "
		allRows := m.bookmarkNoteRows(bm, noteRect.Width-4)
		capacity := max(1, noteRect.Height-2)
		offset := min(m.bookmarkNoteOff, max(0, len(allRows)-capacity))
		noteRows = allRows[offset:]
	} else {
		noteRows = []string{"No bookmark selected"}
	}
	if noteFocused {
		noteTitle = " [Focus]" + noteTitle
	}
	noteBox := m.box(noteRect, noteTitle, noteRows, -1, noteFocused, false)
	lines = m.overlayBox(lines, listRect, listBox)
	lines = m.overlayBox(lines, noteRect, noteBox)

	if m.bmMenuOpen {
		lines = m.bookmarkMenuLines(lines)
	}
	return lines
}

func (m Model) bookmarkMenuLines(lines []string) []string {
	r := m.layout.Dialog(76, 9)
	innerWidth := max(1, r.Width-4)
	menuTitle := " [Focus] Bookmark Actions "
	var rows []string
	selectedRow := -1
	if m.bmMenuStale {
		menuTitle = " [Focus] Bookmark Actions (Stale) "
		rows = []string{
			"Target bookmark no longer exists.",
			"Press Esc to return to the bookmark list.",
		}
	} else if bm, ok := m.bookmarkByKeyAndID(m.bmMenuTargetKey, m.bmMenuTargetID); ok {
		rows = append(rows, ClipLine(clock(bm.Position)+" | "+bookmarkChapterLabel(bm), innerWidth, m.tail()))
		rows = append(rows, ClipLine(cleanLine(bm.Note), innerWidth, m.tail()))
		rows = append(rows, "")
		selectedRow = len(rows) + m.bmMenuCursor
		for _, opt := range bookmarkMenuOptions {
			rows = append(rows, opt.label)
		}
	}
	box := m.box(r, menuTitle, rows, selectedRow, true, false)
	for y, row := range box {
		line := lines[r.Y+y]
		lines[r.Y+y] = ansi.Cut(line, 0, r.X) + row + ansi.Cut(line, r.X+r.Width, m.layout.Content.Width)
	}
	return lines
}
