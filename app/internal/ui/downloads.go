package ui

import (
	"fmt"
	"slices"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// DownloadJob is a sanitized presentation projection of a persistent download job.
type DownloadJob struct {
	ID            string
	BookKey       string
	Title         string
	State         Availability
	Stage         string
	Canceling     bool
	QueuePosition int
	BytesReceived int64
	BytesTotal    *int64
	SpeedBPS      float64
	Error         string
}

// DownloadsMsg updates the Downloads view job list without altering playback or
// Library selection.
type DownloadsMsg struct {
	Jobs     []DownloadJob
	Activity string
	Error    string
}

type downloadMenuOption struct {
	label  string
	action ActionKind
	show   bool
}

type downloadActionMenu struct {
	jobID   string
	bookKey string
	title   string
	state   Availability
	options []downloadMenuOption
	cursor  int
}

// DownloadsOpen reports whether the dedicated Downloads view is currently open.
func (m Model) DownloadsOpen() bool { return m.downloadsOpen }

// DownloadMenuOpen reports whether a Downloads row action menu is currently open.
func (m Model) DownloadMenuOpen() bool { return m.dlMenu != nil }

func cloneDownloadJobs(jobs []DownloadJob) []DownloadJob {
	out := make([]DownloadJob, len(jobs))
	for i, job := range jobs {
		out[i] = job
		if job.BytesTotal != nil {
			v := *job.BytesTotal
			out[i].BytesTotal = &v
		}
	}
	return out
}

func (m *Model) acceptDownloads(msg DownloadsMsg) {
	m.downloads = cloneDownloadJobs(msg.Jobs)
	if len(m.downloads) == 0 {
		m.downloadsCursor = 0
	} else {
		m.downloadsCursor = min(len(m.downloads)-1, max(0, m.downloadsCursor))
	}
	if msg.Activity != "" {
		m.data.Activity = msg.Activity
	}
	if msg.Error != "" {
		m.failure = msg.Error
	}
}

func (m *Model) openDownloads() {
	if m.ownsModal() || m.help || m.accountOpen || m.sortOpen || m.filterEditing {
		return
	}
	m.downloadsOpen = true
	if len(m.downloads) == 0 {
		m.downloadsCursor = 0
	} else {
		m.downloadsCursor = min(len(m.downloads)-1, max(0, m.downloadsCursor))
	}
}

func (m *Model) openDownloadMenu() {
	if len(m.downloads) == 0 {
		m.data.Activity = "No download jobs selected"
		return
	}
	idx := min(len(m.downloads)-1, max(0, m.downloadsCursor))
	job := m.downloads[idx]
	var opts []downloadMenuOption
	switch job.State {
	case Queued:
		opts = []downloadMenuOption{
			{label: "Move up", action: DownloadMoveUp},
			{label: "Move down", action: DownloadMoveDown},
			{label: "Remove from queue", action: DownloadRemove},
		}
	case Downloading, Preparing:
		opts = []downloadMenuOption{
			{label: "Cancel download", action: DownloadCancel},
		}
	case Ready:
		opts = []downloadMenuOption{
			{label: "Show in Library", show: true},
			{label: "Delete downloaded audio", action: DownloadDeleteAudio},
		}
	case Failed, Canceled:
		opts = []downloadMenuOption{
			{label: "Retry download", action: DownloadRetry},
		}
	}
	if len(opts) == 0 {
		return
	}
	m.dlMenu = &downloadActionMenu{
		jobID:   job.ID,
		bookKey: job.BookKey,
		title:   title(job.Title),
		state:   job.State,
		options: opts,
	}
}

func (m Model) deleteAudioConfirmation(bookKey, bookTitle, jobID string) OpenConfirmationMsg {
	m.dialogSeq++
	confirmLabel := "Delete audio"
	prompt := fmt.Sprintf("Delete downloaded audio for %q? Notes, favorites, and history are preserved.", title(bookTitle))
	p := m.data.Player
	if p.Key == bookKey && (p.State == Playing || p.State == Paused) {
		confirmLabel = "Stop and delete audio"
		prompt = fmt.Sprintf("Stop playback and delete downloaded audio for %q? Checkpoint, notes, favorites, and history are preserved.", title(bookTitle))
	}
	return OpenConfirmationMsg{
		ID:      fmt.Sprintf("delete-audio-%d", m.dialogSeq),
		Title:   "Delete downloaded audio",
		Prompt:  prompt,
		Keep:    "Keep audio",
		Confirm: confirmLabel,
		Action:  Action{Kind: DownloadDeleteAudio, Key: bookKey, JobID: jobID},
	}
}

func (m Model) cancelDownloadConfirmation(jobID, bookKey, bookTitle, stage string) OpenConfirmationMsg {
	m.dialogSeq++
	stageLabel := "downloading"
	if stage == "preparing" {
		stageLabel = "preparing"
	}
	return OpenConfirmationMsg{
		ID:      fmt.Sprintf("cancel-dl-%d", m.dialogSeq),
		Title:   "Cancel download",
		Prompt:  fmt.Sprintf("Cancel %s for %q? Partial files will be cleaned up unless publication already committed.", stageLabel, title(bookTitle)),
		Keep:    "Keep " + stageLabel,
		Confirm: "Cancel download",
		Action:  Action{Kind: DownloadCancel, Key: bookKey, JobID: jobID},
	}
}

func (m Model) downloadMenuKey(key string) (Model, tea.Cmd) {
	menu := m.dlMenu
	switch key {
	case "esc":
		m.dlMenu = nil
		return m, nil
	case "j", "down", "tab":
		menu.cursor = (menu.cursor + 1) % len(menu.options)
		return m, nil
	case "k", "up", "shift+tab":
		menu.cursor = (menu.cursor + len(menu.options) - 1) % len(menu.options)
		return m, nil
	case "enter":
		opt := menu.options[menu.cursor]
		m.dlMenu = nil
		if opt.show {
			m.downloadsOpen = false
			m.libraryView = ViewAll
			m.filterQuery = ""
			m.filterDraft = ""
			if menu.bookKey != "" {
				m.selected = menu.bookKey
			}
			m.reconcileSelection()
			m.focus = Books
			m.content = Books
			return m, nil
		}
		// Revalidate frozen target job before executing state-dependent actions.
		idx := slices.IndexFunc(m.downloads, func(j DownloadJob) bool { return j.ID == menu.jobID })
		if idx < 0 || m.downloads[idx].State != menu.state {
			m.data.Activity = "Download state changed; reopen actions"
			return m, nil
		}
		current := m.downloads[idx]
		if opt.action == DownloadCancel {
			msg := m.cancelDownloadConfirmation(current.ID, current.BookKey, current.Title, current.Stage)
			next, cmd := m.Update(msg)
			return next.(Model), cmd
		}
		if opt.action == DownloadDeleteAudio {
			msg := m.deleteAudioConfirmation(current.BookKey, current.Title, current.ID)
			next, cmd := m.Update(msg)
			return next.(Model), cmd
		}
		if m.dispatch == nil {
			m.data.Activity = "Action unavailable: no coordinator"
			return m, nil
		}
		return m, m.dispatch(Action{Kind: opt.action, Key: current.BookKey, JobID: current.ID})
	default:
		return m, nil
	}
}

func (m Model) downloadsKey(key string) (tea.Model, tea.Cmd) {
	if m.dlMenu != nil {
		return m.downloadMenuKey(key)
	}
	switch key {
	case "esc":
		m.downloadsOpen = false
		return m, nil
	case "q", "ctrl+c":
		return m.request(Action{Kind: RequestQuit})
	case "?":
		m.help, m.helpOffset = true, 0
		return m, nil
	case "1", "2", "3", "4", "5":
		m.downloadsOpen = false
		m.focus = Pane(key[0] - '1')
		if m.focus != PlayerPane {
			m.content = m.focus
		}
		return m, nil
	case "j", "down", "k", "up", "g", "home", "G", "end", "pgdown", "pgup", "ctrl+d", "ctrl+u":
		if len(m.downloads) > 0 {
			m.downloadsCursor = moveListCursor(max(0, m.downloadsCursor), len(m.downloads), key, max(1, m.layout.Content.Height-2))
		}
		return m, nil
	case "enter", "x":
		m.openDownloadMenu()
		return m, nil
	case "b":
		return m.triggerCaptureBookmark()
	case "space", ",", ".":
		if action, ok := transportKeys[key]; ok {
			return m.transport(action)
		}
	}
	return m, nil
}

func (m Model) downloadsLines() []string {
	c := m.layout.Content
	selected := -1
	if len(m.downloads) > 0 {
		selected = min(len(m.downloads)-1, max(0, m.downloadsCursor))
	}
	focused := m.dlMenu == nil && !m.ownsModal()
	var lines []string
	if m.layout.Mode == Wide && len(m.downloads) > 0 {
		leftWidth := max(48, c.Width*2/3)
		rightWidth := c.Width - leftWidth
		leftRect := Rect{X: c.X, Y: c.Y, Width: leftWidth, Height: c.Height}
		rightRect := Rect{X: c.X + leftWidth, Y: c.Y, Width: rightWidth, Height: c.Height}
		innerWidth := max(1, leftRect.Width-6)
		rows := make([]string, 0, len(m.downloads)+1)
		const stateCol = 16
		titleCol := max(12, (innerWidth-stateCol-6)/2)
		detailCol := max(12, innerWidth-titleCol-stateCol-6)
		rows = append(rows, PadLine("BOOK", titleCol, m.tail())+" | "+PadLine("STATE", stateCol, m.tail())+" | "+PadLine("TRANSFER / QUEUE", detailCol, m.tail()))
		for _, job := range m.downloads {
			rows = append(rows, m.formatDownloadRow(job, innerWidth))
		}
		boxSelected := selected
		if boxSelected >= 0 {
			boxSelected++
		}
		leftTitle := " 1 Downloads "
		if focused {
			leftTitle = " 1 [Focus] Downloads "
		}
		leftBox := m.box(leftRect, leftTitle, rows, boxSelected, focused, true)
		job := m.downloads[selected]
		rightRows := m.downloadDetailRows(job)
		rightBox := m.box(rightRect, " Details - "+title(job.Title)+" ", rightRows, -1, false, false)
		lines = make([]string, c.Height)
		for y := range c.Height {
			lines[y] = leftBox[y] + rightBox[y]
		}
	} else {
		rows := make([]string, 0, len(m.downloads))
		innerWidth := max(1, c.Width-6)
		for _, job := range m.downloads {
			rows = append(rows, m.formatDownloadRow(job, innerWidth))
		}
		if len(rows) == 0 {
			rows = []string{"No download jobs. Press d on a book in Library to queue a download."}
		}
		lines = m.box(c, " 1 [Focus] Downloads ", rows, selected, focused, selected >= 0)
	}
	if m.dlMenu != nil {
		lines = m.downloadMenuLines(lines)
	}
	return lines
}

func (m Model) downloadDetailRows(job DownloadJob) []string {
	status := m.availability(job.State)
	if job.Canceling {
		status += " (Canceling)"
	}
	rows := []string{
		title(job.Title),
		"State: " + status,
	}
	switch job.State {
	case Queued:
		rows = append(rows, fmt.Sprintf("Queue position: #%d", max(1, job.QueuePosition)), "Actions: Enter / x to reorder or remove")
	case Downloading:
		if job.Stage == "licensing" {
			rows = append(rows, "Stage: Licensing entitlement")
		} else if job.BytesTotal != nil && *job.BytesTotal > 0 {
			pct := min(100, max(0, int(job.BytesReceived*100 / *job.BytesTotal)))
			rows = append(rows, fmt.Sprintf("Transfer: %s / %s (%d%%)", formatBytes(job.BytesReceived), formatBytes(*job.BytesTotal), pct))
		} else {
			rows = append(rows, fmt.Sprintf("Transfer: %s / unknown total", formatBytes(job.BytesReceived)))
		}
		if job.SpeedBPS > 0 && job.Stage != "licensing" {
			rows = append(rows, fmt.Sprintf("Speed: %s/s", formatBytes(int64(job.SpeedBPS))))
		}
		rows = append(rows, "Actions: Enter / x to cancel download")
	case Preparing:
		if job.Stage == "publishing" {
			rows = append(rows, "Stage: Publishing local audio")
		} else {
			rows = append(rows, "Stage: Preparing playable audio (indeterminate)")
		}
		rows = append(rows, "Actions: Enter / x to cancel preparation")
	case Ready:
		rows = append(rows, "Completed local audio ready.", "Actions: Enter / x to show in Library or delete audio")
	case Failed:
		if errText := cleanLine(job.Error); errText != "" {
			rows = append(rows, "Failure: "+errText)
		}
		rows = append(rows, "No automatic retry.", "Actions: Enter / x to retry download")
	case Canceled:
		rows = append(rows, "Canceled before publication.", "Actions: Enter / x to retry from scratch")
	}
	return rows
}

func (m Model) downloadMenuLines(lines []string) []string {
	r := m.layout.Dialog(76, 11)
	rows := []string{fmt.Sprintf("Actions for %s (%s):", m.dlMenu.title, m.availability(m.dlMenu.state))}
	selectedRow := len(rows) + m.dlMenu.cursor
	for _, opt := range m.dlMenu.options {
		rows = append(rows, opt.label)
	}
	box := m.box(r, " [Focus] Download Actions ", rows, selectedRow, true, false)
	for y, row := range box {
		line := lines[r.Y+y]
		lines[r.Y+y] = ansi.Cut(line, 0, r.X) + row + ansi.Cut(line, r.X+r.Width, m.layout.Content.Width)
	}
	return lines
}

func (m Model) formatDownloadRow(job DownloadJob, width int) string {
	status := m.availability(job.State)
	if job.Canceling {
		status += " (Canceling…)"
		if m.appearance.Glyphs == Plain {
			status = m.availability(job.State) + " (Canceling...)"
		}
	}
	var detail string
	switch job.State {
	case Queued:
		detail = fmt.Sprintf("Queued (#%d)", max(1, job.QueuePosition))
	case Downloading:
		if job.Stage == "licensing" {
			detail = "Licensing"
		} else if job.BytesTotal != nil && *job.BytesTotal > 0 {
			pct := min(100, max(0, int(job.BytesReceived*100 / *job.BytesTotal)))
			detail = fmt.Sprintf("%s / %s (%d%%)", formatBytes(job.BytesReceived), formatBytes(*job.BytesTotal), pct)
		} else {
			detail = fmt.Sprintf("%s / unknown total", formatBytes(job.BytesReceived))
		}
		if job.SpeedBPS > 0 && job.Stage != "licensing" {
			detail += fmt.Sprintf(" | %s/s", formatBytes(int64(job.SpeedBPS)))
		}
	case Preparing:
		if job.Stage == "publishing" {
			detail = "Publishing"
		} else {
			detail = "Preparing (indeterminate)"
		}
	case Ready:
		detail = "Completed local audio"
	case Canceled:
		detail = "Retry from scratch"
	case Failed:
		if errText := cleanLine(job.Error); errText != "" {
			detail = errText
		}
	}
	if m.layout.Mode == Wide && width >= 48 {
		const stateCol = 16
		titleCol := max(12, (width-stateCol-6)/2)
		detailCol := max(12, width-titleCol-stateCol-6)
		return PadLine(title(job.Title), titleCol, m.tail()) + " | " + PadLine(status, stateCol, m.tail()) + " | " + PadLine(detail, detailCol, m.tail())
	}
	right := status
	if detail != "" {
		right = detail + " | " + status
	}
	leftWidth := max(0, width-ansi.StringWidth(right)-3)
	return PadLine(title(job.Title), leftWidth, m.tail()) + " | " + right
}

func formatBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(gib))
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(mib))
	case n >= kib:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(kib))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
