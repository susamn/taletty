package ui

import (
	"slices"
	"time"
)

// BookChaptersMsg updates chapter titles and start offsets for a specific BookKey
// without changing selection, focus, or current playback.
type BookChaptersMsg struct {
	Key      string
	Chapters []string
	Starts   []time.Duration
}

func (m *Model) applyBookChapters(msg BookChaptersMsg) {
	if msg.Key == "" {
		return
	}
	for i := range m.data.Books {
		if m.data.Books[i].Key == msg.Key {
			m.data.Books[i].Chapters = slices.Clone(msg.Chapters)
			m.data.Books[i].ChapterStarts = slices.Clone(msg.Starts)
			if m.selected == msg.Key {
				m.chapterCursor = m.clampedChapterCursor(m.data.Books[i])
			}
			return
		}
	}
}

func (m Model) clampedChapterCursor(book Book) int {
	if len(book.Chapters) == 0 {
		return -1
	}
	return min(len(book.Chapters)-1, max(0, m.chapterCursor))
}

func (m Model) listPageSize() int {
	if m.layout.Mode == Wide {
		if m.focus == Chapters {
			return max(1, m.layout.Chapters.Height-2)
		}
		return max(1, m.layout.Books.Height-2)
	}
	return max(1, m.layout.Content.Height-2)
}

func moveListCursor(cursor, count int, key string, page int) int {
	if count <= 0 {
		return 0
	}
	last := count - 1
	half := max(1, page/2)
	switch key {
	case "j", "down":
		return min(last, cursor+1)
	case "k", "up":
		return max(0, cursor-1)
	case "g", "home":
		return 0
	case "G", "end":
		return last
	case "pgdown":
		return min(last, cursor+page)
	case "pgup":
		return max(0, cursor-page)
	case "ctrl+d":
		return min(last, cursor+half)
	case "ctrl+u":
		return max(0, cursor-half)
	default:
		return min(last, max(0, cursor))
	}
}

func (m Model) activateSelectedChapter() (Model, Action, bool) {
	index := m.bookIndex()
	if index < 0 {
		return m, Action{}, false
	}
	book := m.data.Books[index]
	if len(book.Chapters) == 0 {
		m.data.Activity = "No chapters available for highlighted book"
		return m, Action{}, false
	}
	chIdx := m.clampedChapterCursor(book)
	var start time.Duration
	if chIdx >= 0 && chIdx < len(book.ChapterStarts) {
		start = max(0, book.ChapterStarts[chIdx])
	}
	return m, Action{
		Kind:         ActivateChapter,
		Key:          book.Key,
		ChapterIndex: chIdx,
		ChapterStart: start,
	}, true
}
