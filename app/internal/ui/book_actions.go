package ui

import (
	"fmt"
	"slices"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type bookActionOption struct {
	label   string
	action  ActionKind
	confirm string
}

type bookActionMenu struct {
	bookKey      string
	title        string
	finished     bool
	availability Availability
	cursor       int
	options      []bookActionOption
}

func (m *Model) openBookMenu() {
	idx := m.bookIndex()
	if idx < 0 {
		m.data.Activity = "No book highlighted"
		return
	}
	book := m.data.Books[idx]
	var opts []bookActionOption
	if book.Finished {
		opts = append(opts,
			bookActionOption{label: "Mark as unfinished", action: MarkUnfinished},
			bookActionOption{label: "Replay from start", action: ReplayBook, confirm: "replay"},
		)
	} else {
		opts = append(opts, bookActionOption{label: "Mark as finished", action: MarkFinished})
	}
	if book.Availability == Ready {
		delLabel := "Delete downloaded audio"
		if m.data.Player.Key == book.Key && m.data.Player.transportAvailable() {
			delLabel = "Stop and delete audio"
		}
		opts = append(opts, bookActionOption{label: delLabel, action: DownloadDeleteAudio, confirm: "delete"})
	}
	m.bookMenu = &bookActionMenu{
		bookKey:      book.Key,
		title:        title(book.Title),
		finished:     book.Finished,
		availability: book.Availability,
		options:      opts,
	}
}

func (m *Model) replayConfirmation(bookKey, bookTitle string) OpenConfirmationMsg {
	m.dialogSeq++
	return OpenConfirmationMsg{
		ID:      fmt.Sprintf("replay-%d", m.dialogSeq),
		Title:   "Replay finished book",
		Prompt:  fmt.Sprintf("Replay %q from the beginning (00:00:00)? This resets its position and marks it unfinished.", title(bookTitle)),
		Keep:    "Cancel",
		Confirm: "Replay from start",
		Action:  Action{Kind: ReplayBook, Key: bookKey},
	}
}

func (m Model) bookMenuKey(key string) (tea.Model, tea.Cmd) {
	menu := m.bookMenu
	switch key {
	case "esc":
		m.bookMenu = nil
		return m, nil
	case "j", "down", "tab":
		menu.cursor = (menu.cursor + 1) % len(menu.options)
		return m, nil
	case "k", "up", "shift+tab":
		menu.cursor = (menu.cursor + len(menu.options) - 1) % len(menu.options)
		return m, nil
	case "enter":
		opt := menu.options[menu.cursor]
		m.bookMenu = nil
		idx := slices.IndexFunc(m.data.Books, func(b Book) bool { return b.Key == menu.bookKey })
		if idx < 0 || m.data.Books[idx].Finished != menu.finished || m.data.Books[idx].Availability != menu.availability {
			m.data.Activity = "Book state changed; reopen actions"
			return m, nil
		}
		current := m.data.Books[idx]
		switch opt.confirm {
		case "replay":
			return m.Update(m.replayConfirmation(current.Key, current.Title))
		case "delete":
			return m.Update(m.deleteAudioConfirmation(current.Key, current.Title, ""))
		default:
			return m.request(Action{Kind: opt.action, Key: current.Key})
		}
	default:
		return m, nil
	}
}

func (m Model) bookMenuLines(lines []string) []string {
	r := m.layout.Dialog(76, 10)
	status := "Unfinished"
	if m.bookMenu.finished {
		status = "Finished"
	}
	rows := []string{fmt.Sprintf("Actions for %s (%s | %s):", m.bookMenu.title, status, m.availability(m.bookMenu.availability))}
	selectedRow := len(rows) + m.bookMenu.cursor
	for _, opt := range m.bookMenu.options {
		rows = append(rows, opt.label)
	}
	box := m.box(r, " [Focus] Book Actions ", rows, selectedRow, true, false)
	for y, row := range box {
		line := lines[r.Y+y]
		lines[r.Y+y] = ansi.Cut(line, 0, r.X) + row + ansi.Cut(line, r.X+r.Width, m.layout.Content.Width)
	}
	return lines
}
