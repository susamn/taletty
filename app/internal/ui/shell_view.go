package ui

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

var paneNames = [...]string{"Library", "Books", "Chapters", "Details", "Player"}

func (m Model) paneIcon(pane Pane) string {
	if m.appearance.Glyphs == Plain {
		return ""
	}
	switch pane {
	case Library:
		return "◉ "
	case Books, Details:
		return "♪ "
	case Chapters:
		return "○ "
	case PlayerPane:
		switch m.data.Player.State {
		case Playing:
			return "▶ "
		case Paused:
			return "Ⅱ "
		case PlaybackFailed:
			return "! "
		default:
			return "■ "
		}
	default:
		return ""
	}
}

func (m Model) tail() string {
	if m.appearance.Glyphs == Plain {
		return "..."
	}
	return "…"
}

// bookRowMarker returns the podmutt-style leading item glyph for a book row:
// - "+ " when ready locally or queued/downloading (already added/available)
// - "◉ " when finished or ordinary catalog item ("* " in Plain mode)
func (m Model) bookRowMarker(book Book) string {
	switch book.Availability {
	case Ready, Queued, Downloading, Preparing:
		return "+ "
	default:
		if m.appearance.Glyphs == Plain {
			return "* "
		}
		return "◉ "
	}
}

// downloadProgressBar renders an inline progress bar for active downloads:
// - determinate filled bar ("━━━━────" / "[====----]") when BytesTotal is known
// - indeterminate animated spinner/bar when BytesTotal is unknown or preparing
func (m Model) downloadProgressBar(job DownloadJob, width int) string {
	width = max(4, min(16, width))
	if job.State == Downloading && job.Stage != "licensing" && job.BytesTotal != nil && *job.BytesTotal > 0 {
		filled := min(width, max(0, int(job.BytesReceived*int64(width)/(*job.BytesTotal))))
		full, empty := "━", "─"
		if m.appearance.Glyphs == Plain {
			full, empty = "=", "-"
		}
		return "[" + strings.Repeat(full, filled) + strings.Repeat(empty, width-filled) + "]"
	}
	spin := m.spinnerGlyph()
	line := "─"
	if m.appearance.Glyphs == Plain {
		line = "-"
	}
	pos := int(job.BytesReceived/4096) % max(1, width)
	return spin + " [" + strings.Repeat(line, pos) + "•" + strings.Repeat(line, max(0, width-pos-1)) + "]"
}

func (m Model) spinnerGlyph() string {
	if m.appearance.Glyphs == Plain {
		frames := [...]string{"|", "/", "-", "\\"}
		return frames[m.spinTick()%len(frames)]
	}
	frames := [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	return frames[m.spinTick()%len(frames)]
}

func (m Model) spinTick() int {
	total := int(m.data.Player.Position / (250 * time.Millisecond))
	for _, j := range m.downloads {
		total += int(j.BytesReceived / 16384)
		if j.State == Downloading || j.State == Preparing {
			total++
		}
	}
	if m.account.Busy || m.bookmarksLoading || m.ownsModalPending() {
		total++
	}
	return max(0, total)
}

func (m Model) hasActiveLoading() bool {
	if m.account.Busy || m.bookmarksLoading || m.ownsModalPending() {
		return true
	}
	for _, j := range m.downloads {
		if j.State == Downloading || j.State == Preparing {
			return true
		}
	}
	return false
}

func (m Model) paint(text string, tint color.Color) string {
	return m.paintBg(text, tint, m.appearance.Palette.Background)
}

func (m Model) paintBg(text string, tint, background color.Color) string {
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

func (m Model) selectionBackground() color.Color {
	bg := m.appearance.Palette.Background
	if bg == nil {
		return nil
	}
	base := color.NRGBAModel.Convert(bg).(color.NRGBA)
	if sel := m.appearance.Palette.Selection; sel != nil {
		accent := color.NRGBAModel.Convert(sel).(color.NRGBA)
		return color.RGBA{
			R: uint8((int(base.R)*3 + int(accent.R)*2) / 5),
			G: uint8((int(base.G)*3 + int(accent.G)*2) / 5),
			B: uint8((int(base.B)*3 + int(accent.B)*2) / 5),
			A: 0xff,
		}
	}
	return color.RGBA{
		R: uint8(min(255, int(base.R)+24)),
		G: uint8(min(255, int(base.G)+32)),
		B: uint8(min(255, int(base.B)+40)),
		A: 0xff,
	}
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
		sidebar := m.librarySidebarRows()
		rows := make([]string, len(sidebar))
		for i, item := range sidebar {
			label := item.label
			if !item.selectable {
				dash := "─"
				if m.appearance.Glyphs == Plain {
					dash = "-"
				}
				fill := max(0, width-ansi.StringWidth(label)-1)
				if fill > 0 {
					label = label + " " + strings.Repeat(dash, fill)
				}
			} else if item.action == "" && item.view == m.libraryView {
				label = "* " + label
			} else {
				marker := "◉ "
				if m.appearance.Glyphs == Plain {
					marker = "  "
				}
				label = marker + label
			}
			rows[i] = label
		}
		return name, rows, m.clampedLibraryCursor(sidebar)
	}
	if pane == Books {
		visible := m.visibleBooks()
		var tags []string
		if m.libraryView != ViewAll {
			tags = append(tags, m.libraryView.Label())
		}
		if m.sortOrder != SortTitle {
			tags = append(tags, "Sort: "+string(m.sortOrder))
		}
		if m.filterQuery != "" {
			tags = append(tags, fmt.Sprintf("Filter: %q", cleanLine(m.filterQuery)))
		}
		if len(tags) > 0 {
			name += " [" + strings.Join(tags, " | ") + "]"
		}
		rows := make([]string, 0, len(visible)+1)
		selectedIdx := m.visibleBookIndex(visible)
		if len(visible) > 0 && m.layout.Mode == Wide && width >= 48 {
			const stateCol = 15
			const progCol = 8
			const favCol = 5
			titleCol := max(10, width-favCol-progCol-stateCol-9)
			header := PadLine("  TITLE / AUTHOR", titleCol, m.tail()) + " | " + PadLine("FAV", favCol, m.tail()) + " | " + PadLine("PROGRESS", progCol, m.tail()) + " | " + PadLine("LOCAL STATE", stateCol, m.tail())
			rows = append(rows, header)
			if selectedIdx >= 0 {
				selectedIdx++
			}
			for _, book := range visible {
				marker := m.bookRowMarker(book)
				favorite := ""
				favCell := "-"
				if m.appearance.Glyphs != Plain {
					favCell = "·"
				}
				if book.Favorite {
					favorite = "[Fav] "
					favCell = "[Fav]"
					if m.appearance.Glyphs != Plain {
						favorite = "★ "
						favCell = "★"
					}
				}
				playing := ""
				if m.data.Player.Key != "" && m.data.Player.Key == book.Key && m.data.Player.Session != "" {
					switch m.data.Player.State {
					case Playing:
						playing = " [PLAYING]"
					case Paused:
						playing = " [PAUSED]"
					}
				}
				status := m.availability(book.Availability)
				prog := m.bookProgressLabel(book)
				metadata := PadLine(marker+favorite+title(book.Title)+playing+" | "+book.Author, titleCol, m.tail())
				rows = append(rows, metadata+" | "+PadLine(favCell, favCol, m.tail())+" | "+PadLine(prog, progCol, m.tail())+" | "+PadLine(status, stateCol, m.tail()))
			}
		} else {
			for _, book := range visible {
				marker := ""
				if m.appearance.Glyphs != Plain {
					marker = m.bookRowMarker(book)
				}
				favorite := ""
				if book.Favorite {
					favorite = "[Fav] "
					if m.appearance.Glyphs != Plain {
						favorite = "★ "
					}
				}
				status := m.availability(book.Availability)
				metadataWidth := max(0, width-ansi.StringWidth(status)-3)
				metadata := PadLine(marker+favorite+title(book.Title)+" | "+book.Author, metadataWidth, m.tail())
				rows = append(rows, metadata+" | "+status)
			}
		}
		if len(rows) == 0 {
			switch {
			case len(m.data.Books) == 0:
				rows = []string{"No books cached"}
			case m.filterQuery != "":
				rows = []string{"No matches for filter (Esc clears)"}
			default:
				rows = []string{"No books in " + m.libraryView.Label() + " (press 1 for All)"}
			}
		}
		return name, rows, selectedIdx
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
		rows := m.formatChapterRows(book, width)
		sel := m.clampedChapterCursor(book)
		if m.layout.Mode == Wide && width >= 20 && sel >= 0 {
			sel++
		}
		return name, rows, sel
	}
	rows := []string{"  " + title(book.Title)}
	if book.Favorite {
		favLabel := "  [Fav] Favorite"
		if m.appearance.Glyphs != Plain {
			favLabel = "  ★ Favorite"
		}
		rows = append(rows, favLabel)
	}
	if book.Subtitle != "" {
		rows = append(rows, "  "+book.Subtitle)
	}
	contributorLine := "  Author: " + book.Author
	if book.Narrator != "" {
		contributorLine += "  |  Narrator: " + book.Narrator
	}
	rows = append(rows, contributorLine)
	progressLabel := "Not started"
	switch {
	case book.Finished:
		progressLabel = "Finished"
	case book.InProgress:
		progressLabel = "In progress"
	}
	rows = append(rows, "  "+formatDurationLabel(book.Duration, book.HasDuration)+"  |  "+m.availability(book.Availability)+"  |  "+progressLabel)
	rows = append(rows, "  Actions: Enter play/resume  |  f favorite  |  B bookmarks  |  d download  |  x more")
	if book.Description != "" && book.Description != book.Subtitle {
		rows = append(rows, "")
		for _, line := range strings.Split(book.Description, "\n") {
			rows = append(rows, "  "+line)
		}
	}
	return name, rows, -1
}

// frame owns borders, a cursor gutter and one-cell inner padding. It never clips
// renderer styles as untrusted metadata: raw text is fitted before paint is added.
func (m Model) frame(pane Pane, size Rect, name string, rows []string, selected int) []string {
	titlePrefix := m.paneIcon(pane)
	focused := m.paneFocused(pane)
	if focused {
		name = "[Focus] " + name
	}
	tag := fmt.Sprintf("[%d]", pane+1)
	if pane == Chapters {
		if idx := m.bookIndex(); idx >= 0 && len(m.data.Books[idx].Chapters) > 0 && size.Width >= 28 {
			cur := m.clampedChapterCursor(m.data.Books[idx])
			tag = fmt.Sprintf("[%d]──%d/%d", pane+1, cur+1, len(m.data.Books[idx].Chapters))
			if m.appearance.Glyphs == Plain {
				tag = fmt.Sprintf("[%d]--%d/%d", pane+1, cur+1, len(m.data.Books[idx].Chapters))
			}
		}
	}
	return m.boxTagged(size, " "+titlePrefix+name+" ", tag, rows, selected, focused, pane == Library || pane == Books || (pane == Chapters && selected >= 0))
}

func (m Model) paneFocused(pane Pane) bool {
	if m.help || m.ownsModal() || m.accountOpen || m.sortOpen || m.downloadsOpen || m.bookMenu != nil || m.sleepOpen || m.settingsOpen {
		return false
	}
	if m.bookmarksOpen {
		return pane == PlayerPane && m.bookmarkFocus == bmFocusPlayer && !m.bmMenuOpen && !m.bmFilterEditing
	}
	return pane == m.focus
}

// box is shared by root panes and the dedicated Help view. Fitting precedes style.
func (m Model) box(size Rect, name string, rows []string, selected int, focused, list bool) []string {
	return m.boxTagged(size, name, "", rows, selected, focused, list)
}

func (m Model) boxTagged(size Rect, name, tag string, rows []string, selected int, focused, list bool) []string {
	tint := m.appearance.Palette.Border
	if focused {
		tint = m.appearance.Palette.Focus
	}
	topLeft, topRight, bottomLeft, bottomRight, vertical, horizontal := "+", "+", "+", "+", "|", "-"
	prefix := " "
	if m.appearance.Glyphs != Plain {
		topLeft, topRight, bottomLeft, bottomRight, vertical, horizontal = "╭", "╮", "╰", "╯", "│", "─"
		prefix = "──"
	}
	border := func(text string) string { return m.paint(text, tint) }
	lines := make([]string, size.Height)
	innerWidth := max(0, size.Width-2)
	rightFurniture := ""
	if tag != "" && innerWidth >= 12 {
		rightFurniture = tag + horizontal
	}
	availTitle := max(0, innerWidth-ansi.StringWidth(prefix)-ansi.StringWidth(rightFurniture))
	trimmedName := strings.TrimSpace(name)
	label := ClipLine(trimmedName, availTitle, m.tail())
	if label != "" {
		label += " "
		if ansi.StringWidth(label) > availTitle {
			label = ClipLine(trimmedName, availTitle, m.tail())
		}
	}
	fillLen := max(0, innerWidth-ansi.StringWidth(prefix)-ansi.StringWidth(label)-ansi.StringWidth(rightFurniture))
	lines[0] = border(topLeft + prefix + label + strings.Repeat(horizontal, fillLen) + rightFurniture + topRight)
	first := max(0, selected-(size.Height-2)+1)
	for y := 1; y < size.Height-1; y++ {
		index, text, gutter := first+y-1, "", ""
		if list {
			gutter = "  "
		}
		textTint := m.appearance.Palette.Text
		rowBg := m.appearance.Palette.Background
		if index < len(rows) {
			text = rows[index]
			if isMutedHeaderRow(text) {
				textTint = m.appearance.Palette.Muted
			}
		}
		if index == selected {
			gutter, textTint, rowBg = "> ", m.appearance.Palette.Selection, m.selectionBackground()
			if rowBg != nil && m.appearance.Palette.Text != nil {
				textTint = m.appearance.Palette.Text
			}
		}
		fitted := PadLine(text, size.Width-4-len(gutter), m.tail())
		body := m.paintBg(" "+gutter+fitted+" ", textTint, rowBg)
		if m.appearance.Palette.Focus != nil {
			for _, prefix := range []string{"[●] ", "[*] ", "+ ★ ", "+ [Fav] ", "◉ ★ ", "◉ [Fav] ", "* [Fav] ", "+ ", "★ ", "[Fav] ", "  ★ ", "  [Fav] ", "  Active:", "  Source:", "  Diagnostic:", "  Omarchy hook:", "  Font:"} {
				if strings.HasPrefix(fitted, prefix) {
					rest := strings.TrimPrefix(fitted, prefix)
					body = m.paintBg(" "+gutter, textTint, rowBg) + m.paintBg(prefix, m.appearance.Palette.Focus, rowBg) + m.paintBg(rest+" ", textTint, rowBg)
					break
				}
			}
		}
		lines[y] = border(vertical) + body + border(vertical)
	}
	lines[len(lines)-1] = border(bottomLeft + strings.Repeat(horizontal, innerWidth) + bottomRight)
	return lines
}

func (m Model) pane(pane Pane, size Rect) []string {
	gutterLen := 0
	if pane == Library || pane == Books || pane == Chapters {
		gutterLen = 2
	}
	name, rows, selected := m.paneRows(pane, size.Width-4-gutterLen)
	return m.frame(pane, size, name, rows, selected)
}

func (m Model) bookProgressLabel(book Book) string {
	switch {
	case book.Finished:
		return "100%"
	case m.data.Player.Key != "" && m.data.Player.Key == book.Key && m.data.Player.Duration > 0:
		pct := min(100, max(0, int(m.data.Player.Position*100/m.data.Player.Duration)))
		return fmt.Sprintf("%d%%", pct)
	case book.InProgress:
		return "Active"
	case book.Availability == Ready:
		return "0%"
	default:
		if m.appearance.Glyphs == Plain {
			return "-"
		}
		return "—"
	}
}

// formatChapterStart renders a chapter start timestamp like podmutt's chapter view:
// "0:00", "2:19", "19:22", or "1:05:38".
func formatChapterStart(d time.Duration) string {
	seconds := int64(max(0, d) / time.Second)
	if seconds >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", seconds/3600, seconds/60%60, seconds%60)
	}
	return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
}

func (m Model) formatChapterRows(book Book, width int) []string {
	rows := make([]string, 0, len(book.Chapters)+1)
	if m.layout.Mode == Wide && width >= 20 {
		const durCol = 6
		titleCol := max(4, width-8-durCol-2)
		rows = append(rows, PadLine("   Start", 8, m.tail())+"  "+PadLine("Chapter", titleCol, m.tail())+"  "+PadLine("Length", durCol, m.tail()))
	}
	for i, ch := range book.Chapters {
		startStamp := fmt.Sprintf("%02d.", i+1)
		var start time.Duration
		if i < len(book.ChapterStarts) {
			start = max(0, book.ChapterStarts[i])
			startStamp = formatChapterStart(start)
		}
		prefix := fmt.Sprintf("%7s  ", startStamp)
		durLabel := ""
		if i < len(book.ChapterStarts) {
			var end time.Duration
			if i+1 < len(book.ChapterStarts) && book.ChapterStarts[i+1] > start {
				end = book.ChapterStarts[i+1]
			} else if book.HasDuration && book.Duration > start {
				end = book.Duration
			}
			if end > start {
				mins := max(1, int((end-start+30*time.Second)/time.Minute))
				durLabel = fmt.Sprintf("%4dm", mins)
			}
		}
		if durLabel != "" && width >= 20 {
			titleWidth := max(4, width-len(prefix)-len(durLabel)-2)
			rows = append(rows, prefix+PadLine(ch, titleWidth, m.tail())+"  "+durLabel)
		} else {
			rows = append(rows, prefix+ch)
		}
	}
	return rows
}

func isMutedHeaderRow(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.HasPrefix(trimmed, "LIBRARY VIEWS") || strings.HasPrefix(trimmed, "VIEWS / OVERLAYS") ||
		strings.HasPrefix(trimmed, "TITLE / AUTHOR") ||
		strings.HasPrefix(trimmed, "Start ") ||
		strings.HasPrefix(trimmed, "Chapter ") ||
		strings.HasPrefix(trimmed, "BOOK ") ||
		strings.HasPrefix(trimmed, "TIME ") ||
		strings.HasPrefix(trimmed, "Themes (") ||
		trimmed == "Configuration:" ||
		trimmed == "Palette candidates:" || trimmed == "Playback / display preferences:"
}

func (m Model) overlayBox(lines []string, r Rect, box []string) []string {
	for y, row := range box {
		if r.Y+y >= 0 && r.Y+y < len(lines) {
			line := lines[r.Y+y]
			lines[r.Y+y] = ansi.Cut(line, 0, r.X) + row + ansi.Cut(line, r.X+r.Width, m.layout.Content.Width)
		}
	}
	return lines
}

func (m Model) View() tea.View {
	g := m.layout
	if g.Screen.Width == 0 || g.Screen.Height == 0 {
		v := tea.NewView("")
		v.AltScreen = true
		return v
	}
	if g.Mode == ResizeOnly {
		message := fmt.Sprintf("Resize to %dx%d (now %dx%d); q quit", MinWidth, MinHeight, g.Screen.Width, g.Screen.Height)
		tint := m.appearance.Palette.Text
		if m.quitWarning {
			message = "Unsaved draft: q again quits; Esc keeps draft"
			tint = m.appearance.Palette.Error
		}
		v := tea.NewView(m.paint(PadLine(message, g.Screen.Width, m.tail()), tint))
		v.AltScreen = true
		return v
	}
	var lines []string
	if m.downloadsOpen {
		lines = m.downloadsLines()
	} else if g.Mode == Wide {
		left, right := m.pane(Library, g.Library), m.pane(Chapters, g.Chapters)
		middle := append(m.pane(Books, g.Books), m.pane(Details, g.Details)...)
		for y := range g.Content.Height {
			lines = append(lines, left[y]+middle[y]+right[y])
		}
	} else {
		lines = m.pane(m.content, g.Content)
	}
	if m.bookmarksOpen {
		lines = m.bookmarksLines(lines)
	}
	if m.settingsOpen {
		lines = m.settingsLines(lines)
	}
	if m.help {
		lines = m.helpLines(lines)
	}
	if m.ownsModal() {
		lines = m.modalLines(lines)
	} else if m.accountOpen && !m.help {
		lines = m.accountLines(lines)
	} else if m.sortOpen && !m.help {
		lines = m.sortLines(lines)
	} else if m.bookMenu != nil && !m.help && !m.downloadsOpen && !m.bookmarksOpen && !m.settingsOpen {
		lines = m.bookMenuLines(lines)
	} else if m.sleepOpen && !m.help && !m.downloadsOpen && !m.bookmarksOpen && !m.settingsOpen {
		lines = m.sleepLines(lines)
	}
	lines = append(lines, m.playerLines(g.Player)...)
	activity, tint := m.data.Activity, m.appearance.Palette.Muted
	if m.hasActiveLoading() && activity != "" {
		activity = m.spinnerGlyph() + " " + activity
	}
	if m.failure != "" {
		activity, tint = "Error: "+m.failure, m.appearance.Palette.Error
	}
	lines = append(lines, m.paint(PadLine(activity, g.Activity.Width, m.tail()), tint))
	hints := "? Help | q quit | Tab/1-5 panes | "
	switch m.focus {
	case Library:
		hints += "j/k views | Enter open | f/m fav | / filter | s sort"
	case Books:
		hints += "j/k browse | Enter play"
		if g.Hints.Width >= 100 {
			hints += " | f/m favorite | d download | B bookmarks | x actions"
		}
	case Details:
		hints += "Enter request play highlighted book | f/m fav"
	case Chapters:
		hints += "j/k chapters | Enter jump/switch | f/m fav"
	case PlayerPane:
		hints += "Left/Right controls | Enter activate"
	}
	if m.data.Player.transportAvailable() {
		hints += " | Space current audio"
	} else {
		hints += " | Transport disabled: no loaded audio"
	}
	if m.dispatch == nil {
		hints = "? Help | Tab/1-5 panes | Requests disabled: no coordinator"
	}
	if m.filterQuery != "" && !m.filterEditing {
		hints += " | Esc clear filter"
	}
	if m.filterEditing {
		hints = "Filter: " + cleanLine(m.filterDraft) + "_ | Enter apply | Esc cancel"
	}
	if m.bookmarksOpen {
		switch {
		case m.bmFilterEditing:
			hints = "Bookmark filter: " + cleanLine(m.bmFilterDraft) + "_ | Enter apply | Esc cancel"
		case m.bmMenuOpen:
			hints = "j/k choose | Enter act | Esc back | Transport disabled in menu"
		default:
			hints = "j/k move | Enter jump | x actions | e export | b capture | / filter | Esc back | " + m.helpTransport()
		}
	}
	if m.help {
		hints = "Esc Back | j/k scroll | PgUp/PgDown | g/G bounds | " + m.helpTransport()
	}
	if m.accountOpen {
		hints = "j/k/Tab choose | Enter act | Esc back | Transport disabled in Account"
	}
	if m.sortOpen {
		hints = "j/k/Tab choose | Enter sort | Esc cancel | Transport disabled in Sort"
	}
	if m.bookMenu != nil {
		hints = "j/k/Tab choose | Enter act | Esc back | Transport disabled in menu"
	}
	if m.sleepOpen {
		hints = "j/k/Tab choose | Enter arm | Esc back | Transport disabled in Sleep menu"
	}
	if m.settingsOpen {
		hints = "j/k select | Tab section | Enter apply/toggle | Esc back | Transport disabled in Settings"
	}
	if m.downloadsOpen {
		hints = "j/k jobs | Enter/x actions | Esc back | Space/,/. current audio"
		if m.dlMenu != nil {
			hints = "j/k/Tab choose | Enter act | Esc back | Transport disabled in menu"
		}
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
	view.AltScreen = true
	view.Cursor = m.noteCursor()
	return view
}
