package ui

import (
	"fmt"
	"image/color"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

var paneNames = [...]string{"Library", "Books", "Details", "Chapters", "Player"}

func (m Model) tail() string {
	if m.appearance.Glyphs == Plain {
		return "..."
	}
	return "…"
}

func (m Model) paint(text string, tint color.Color) string {
	background := m.appearance.Palette.Background
	if tint == nil && background == nil {
		return text
	}
	style := ansi.Style{}
	if tint != nil {
		style = style.ForegroundColor(tint)
	}
	if background != nil {
		style = style.BackgroundColor(background)
	}
	return style.Styled(text)
}

func (m Model) availability(state Availability) string {
	labels := [...]string{"Availability unknown", "Not downloaded", "Queued", "Downloading", "Preparing", "Ready", "Failed", "Canceled"}
	icons := [...]string{"", "", "… ", "↓ ", "… ", "✓ ", "! ", "× "}
	if state < AvailabilityUnknown || state > Canceled {
		state = AvailabilityUnknown
	}
	if m.appearance.Glyphs == Plain {
		return labels[state]
	}
	return icons[state] + labels[state]
}

func title(text string) string {
	if strings.TrimSpace(cleanLine(text)) == "" {
		return "Untitled"
	}
	return text
}

func (m Model) paneRows(pane Pane, width int) (string, []string, int) {
	name := paneNames[pane]
	if pane == Library {
		return name, []string{"All audiobooks", "Destinations unavailable in shell"}, -1
	}
	if pane == Books {
		rows := make([]string, 0, len(m.data.Books))
		for _, book := range m.data.Books {
			favorite := ""
			if book.Favorite {
				favorite = "[Fav] "
				if m.appearance.Glyphs != Plain {
					favorite = "★ "
				}
			}
			status := m.availability(book.Availability)
			metadataWidth := max(0, width-ansi.StringWidth(status)-3)
			metadata := PadLine(favorite+title(book.Title)+" | "+book.Author, metadataWidth, m.tail())
			rows = append(rows, metadata+" | "+status)
		}
		if len(rows) == 0 {
			rows = []string{"No books cached"}
		}
		return name, rows, m.bookIndex()
	}
	index := m.bookIndex()
	if index < 0 {
		return name, []string{"No book highlighted"}, -1
	}
	book := m.data.Books[index]
	name += " - " + title(book.Title)
	if pane == Chapters {
		if len(book.Chapters) == 0 {
			return name, []string{"No chapters available"}, -1
		}
		return name, book.Chapters, -1
	}
	rows := []string{title(book.Title), "Author: " + book.Author, m.availability(book.Availability)}
	return name, append(rows, strings.Split(book.Description, "\n")...), -1
}

// frame owns borders, a cursor gutter and one-cell inner padding. It never clips
// renderer styles as untrusted metadata: raw text is fitted before paint is added.
func (m Model) frame(pane Pane, size Rect, name string, rows []string, selected int) []string {
	focused := pane == m.focus && !m.help && !m.ownsModal()
	if focused {
		name = "[Focus] " + name
	}
	return m.box(size, fmt.Sprintf(" %d %s ", pane+1, name), rows, selected, focused, pane == Books)
}

// box is shared by root panes and the dedicated Help view. Fitting precedes style.
func (m Model) box(size Rect, name string, rows []string, selected int, focused, list bool) []string {
	tint := m.appearance.Palette.Border
	if focused {
		tint = m.appearance.Palette.Focus
	}
	topLeft, topRight, bottomLeft, bottomRight, vertical, horizontal := "+", "+", "+", "+", "|", "-"
	if m.appearance.Glyphs != Plain {
		topLeft, topRight, bottomLeft, bottomRight, vertical, horizontal = "┌", "┐", "└", "┘", "│", "─"
	}
	border := func(text string) string { return m.paint(text, tint) }
	lines := make([]string, size.Height)
	label := ClipLine(name, size.Width-2, m.tail())
	lines[0] = border(topLeft + label + strings.Repeat(horizontal, size.Width-2-ansi.StringWidth(label)) + topRight)
	first := max(0, selected-(size.Height-2)+1)
	for y := 1; y < size.Height-1; y++ {
		index, text, gutter := first+y-1, "", ""
		if list {
			gutter = " "
		}
		textTint := m.appearance.Palette.Text
		if index < len(rows) {
			text = rows[index]
		}
		if index == selected {
			gutter, textTint = ">", m.appearance.Palette.Selection
		}
		lines[y] = border(vertical) + m.paint(" "+gutter+PadLine(text, size.Width-4-len(gutter), m.tail())+" ", textTint) + border(vertical)
	}
	lines[len(lines)-1] = border(bottomLeft + strings.Repeat(horizontal, size.Width-2) + bottomRight)
	return lines
}

func (m Model) pane(pane Pane, size Rect) []string {
	name, rows, selected := m.paneRows(pane, size.Width-5)
	return m.frame(pane, size, name, rows, selected)
}

func (m Model) View() tea.View {
	g := m.layout
	if g.Screen.Width == 0 || g.Screen.Height == 0 {
		return tea.NewView("")
	}
	if g.Mode == ResizeOnly {
		message := fmt.Sprintf("Resize to %dx%d (now %dx%d); q quit", MinWidth, MinHeight, g.Screen.Width, g.Screen.Height)
		if m.quitWarning {
			message = "Unsaved draft: q again quits; Esc keeps draft"
		}
		return tea.NewView(PadLine(message, g.Screen.Width, m.tail()))
	}
	var lines []string
	if m.help {
		lines = m.helpLines()
	} else if g.Mode == Wide {
		left, right := m.pane(Library, g.Library), m.pane(Chapters, g.Chapters)
		middle := append(m.pane(Books, g.Books), m.pane(Details, g.Details)...)
		for y := range g.Content.Height {
			lines = append(lines, left[y]+middle[y]+right[y])
		}
	} else {
		lines = m.pane(m.content, g.Content)
	}
	if m.ownsModal() {
		lines = m.modalLines(lines)
	}
	lines = append(lines, m.playerLines(g.Player)...)
	activity, tint := m.data.Activity, m.appearance.Palette.Muted
	if m.failure != "" {
		activity, tint = "Error: "+m.failure, m.appearance.Palette.Error
	}
	lines = append(lines, m.paint(PadLine(activity, g.Activity.Width, m.tail()), tint))
	hints := "? Help | q quit | Tab/1-5 panes | "
	switch m.focus {
	case Books:
		hints += "j/k browse | Enter play"
	case Details:
		hints += "Enter request play highlighted book"
	case PlayerPane:
		hints += "Left/Right controls | Enter activate"
	default:
		hints += "Domain actions unavailable in shell"
	}
	if m.data.Player.transportAvailable() {
		hints += " | Space current audio"
	} else {
		hints += " | Transport disabled: no loaded audio"
	}
	if m.dispatch == nil {
		hints = "? Help | Tab/1-5 panes | Requests disabled: no coordinator"
	}
	if m.help {
		hints = "Esc Back | j/k scroll | PgUp/PgDown | g/G bounds | " + m.helpTransport()
	}
	if m.ownsModal() {
		hints = "Enter/Ctrl+S save | Alt+Enter newline | Tab local focus | Esc back"
		if m.confirmation != nil {
			hints = "Tab/Left/Right choose | Enter act | Esc back | Up/Down scroll"
		}
		if m.ownsModalPending() {
			hints = "Action pending; input held until coordinator confirmation"
		}
	}
	lines = append(lines, m.paint(PadLine(hints, g.Hints.Width, m.tail()), m.appearance.Palette.Muted))
	view := tea.NewView(strings.Join(lines, "\n"))
	view.Cursor = m.noteCursor()
	return view
}
