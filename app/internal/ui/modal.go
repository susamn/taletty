package ui

import (
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// NoteTarget is frozen by the coordinator at capture/edit time. Jump already
// includes pre-roll; editing an existing BookmarkID must not recapture position.
// This component neither calculates playback policy nor persists bookmarks.
type NoteTarget struct {
	Key, BookmarkID, Title, Chapter, JumpChapter string
	Captured, Jump                               time.Duration
}

// OpenNoteEditorMsg uses the same unique-opening-ID contract as confirmations.
type OpenNoteEditorMsg struct {
	ID     string
	Target NoteTarget
	Text   string
}

// OpenConfirmationMsg names an explicit action, never a cursor-relative target.
// The coordinator must revalidate source sessions/removed targets on submission.
// IDs must be nonempty and unique for each opening, including canceled openings.
type OpenConfirmationMsg struct {
	ID, Title, Prompt, Keep, Confirm string
	Action                           Action
}

// ModalResultMsg is a sanitized coordinator result, not an optimistic UI save.
// Both opening ID and attempt must match; success closes only the submitted layer.
type ModalResultMsg struct {
	ID      string
	Attempt uint64
	Error   string
}

type noteEditor struct {
	spec    OpenNoteEditorMsg
	field   textarea.Model
	focus   int // field, Save, Cancel
	pending bool
	attempt uint64
	error   string
}

type confirmation struct {
	spec    OpenConfirmationMsg
	confirm bool // default-safe Keep/Stay
	pending bool
	attempt uint64
	error   string
	offset  int
}

func safeNote(text string) string {
	rows := strings.Split(text, "\n")
	for i := range rows {
		rows[i] = cleanLine(rows[i])
	}
	return strings.Join(rows, "\n")
}

func (m *Model) openNote(msg OpenNoteEditorMsg) {
	if msg.ID == "" || msg.Target.Key == "" || m.note != nil || m.confirmation != nil {
		return
	}
	field := textarea.New()
	field.SetStyles(textarea.Styles{}) // shell semantic palette paints the fitted rows
	field.SetVirtualCursor(false)      // use the actual terminal cursor, not blink commands
	field.MaxHeight = 0                // logical note lines are not the viewport height
	field.ShowLineNumbers, field.Prompt, field.Placeholder = false, "", "Note"
	field.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter"))
	field.KeyMap.Paste.SetEnabled(false) // bracketed paste only; no clipboard subprocess
	text := safeNote(msg.Text)
	field.SetValue(text)
	if field.Value() != text {
		m.failure = "Note exceeds editor limits; cannot edit safely"
		return // never allow a silently truncated existing note to be saved
	}
	field.Focus()
	m.note = &noteEditor{spec: msg, field: field}
	m.sizeNote()
}

func (m *Model) sizeNote() {
	if m.note != nil && m.layout.Mode != ResizeOnly {
		m.note.field.SetWidth(m.layout.Dialog(76, 11).Width - 4)
		m.note.field.SetHeight(4)
	}
}

func (m Model) ownsModal() bool { return m.note != nil || m.confirmation != nil }

func (m Model) ownsModalPending() bool {
	return m.note != nil && m.note.pending || m.confirmation != nil && m.confirmation.pending
}

func (m Model) modalResult(msg ModalResultMsg) (tea.Model, tea.Cmd) {
	if c := m.confirmation; c != nil && c.pending && msg.ID == c.spec.ID && msg.Attempt == c.attempt {
		c.pending, c.error = false, msg.Error
		if msg.Error == "" {
			m.confirmation = nil
		}
	} else if n := m.note; n != nil && n.pending && msg.ID == n.spec.ID && msg.Attempt == n.attempt {
		n.pending, n.error = false, msg.Error
		if msg.Error == "" {
			m.note = nil
		}
	}
	return m, nil
}

func (m Model) submitNote() (tea.Model, tea.Cmd) {
	n := m.note
	if strings.TrimSpace(n.field.Value()) == "" {
		n.error = "A note is required"
		return m, nil
	}
	if m.dispatch == nil {
		n.error = "Save unavailable: no coordinator"
		return m, nil
	}
	n.pending, n.error = true, ""
	n.attempt++
	return m.request(Action{Kind: SaveNote, Key: n.spec.Target.Key, Target: n.spec.Target,
		Text: n.field.Value(), RequestID: n.spec.ID, Attempt: n.attempt})
}

func (m Model) modalKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	name := msg.String()
	if c := m.confirmation; c != nil {
		if c.pending {
			return m, nil // cannot promise Esc rolls back an already committed action
		}
		switch name {
		case "esc":
			m.confirmation = nil
		case "tab", "shift+tab", "left", "right":
			c.confirm = !c.confirm
		case "up", "k":
			c.offset = max(0, c.offset-1)
		case "down", "j":
			c.offset = min(max(0, len(m.confirmRows())-7), c.offset+1)
		case "enter":
			if !c.confirm {
				m.confirmation = nil
			} else if m.dispatch == nil {
				c.error = "Action unavailable: no coordinator"
			} else {
				c.pending, c.error = true, ""
				c.attempt++
				action := c.spec.Action
				action.RequestID, action.Attempt = c.spec.ID, c.attempt
				return m.request(action)
			}
		}
		return m, nil
	}
	n := m.note
	if n.pending {
		return m, nil
	}
	switch name {
	case "esc":
		m.note = nil
	case "tab", "shift+tab":
		delta := 1
		if name == "shift+tab" {
			delta = 2
		}
		n.focus = (n.focus + delta) % 3
		if n.focus == 0 {
			n.field.Focus()
		} else {
			n.field.Blur()
		}
	case "ctrl+s", "enter":
		if name == "enter" && n.focus == 2 {
			m.note = nil
			return m, nil
		}
		return m.submitNote()
	default:
		if n.focus == 0 {
			// All text/edit keys belong to this field, including q/Space/digits.
			msg.Text = safeNote(msg.Text)
			n.field, _ = n.field.Update(msg)
		}
	}
	return m, nil
}

func (m Model) confirmRows() []string {
	c := m.confirmation
	text := c.spec.Prompt + "\nKeep: " + c.spec.Keep + "\nConfirm: " + c.spec.Confirm
	return strings.Split(ansi.Wrap(safeNote(text), m.layout.Dialog(76, 11).Width-4, ""), "\n")
}

// modalLines shares the shell box, geometry and semantic palette. Background
// cells outside the dialog remain visible; the full-width player is untouched.
func (m Model) modalLines(lines []string) []string {
	r := m.layout.Dialog(76, 11)
	var rows []string
	label := "[Focus] Note"
	if c := m.confirmation; c != nil {
		label = "[Focus] " + c.spec.Title
		body := m.confirmRows()
		offset := min(c.offset, max(0, len(body)-7))
		rows = append(rows, body[offset:min(len(body), offset+7)]...)
		for len(rows) < 7 {
			rows = append(rows, "")
		}
		status := c.error
		if c.pending {
			status = m.spinnerGlyph() + " Working; waiting for confirmation"
		}
		keep, confirm := c.spec.Keep, c.spec.Confirm
		if c.confirm {
			confirm = "[" + confirm + "]"
		} else {
			keep = "[" + keep + "]"
		}
		buttonWidth := (r.Width - 7) / 2
		buttons := PadLine(keep, buttonWidth, m.tail()) + " | " + ClipLine(confirm, buttonWidth, m.tail())
		rows = append(rows, status, buttons)
	} else {
		n := m.note
		chapterLine := n.spec.Target.Chapter
		if n.spec.Target.JumpChapter != "" && n.spec.Target.JumpChapter != n.spec.Target.Chapter {
			chapterLine = "Cap: " + n.spec.Target.Chapter + " -> Jump: " + n.spec.Target.JumpChapter
		}
		timingLine := "Captured " + clock(n.spec.Target.Captured) + " | Jump " + clock(n.spec.Target.Jump)
		if n.spec.Target.BookmarkID != "" {
			timingLine = "Edit bookmark | Jump " + clock(n.spec.Target.Jump)
		}
		rows = []string{title(n.spec.Target.Title) + " | " + chapterLine, timingLine}
		rows = append(rows, strings.Split(n.field.View(), "\n")...)
		status := n.error
		if n.pending {
			status = m.spinnerGlyph() + " Saving; waiting for durable confirmation"
		}
		buttons := []string{"[Note]  Save  Cancel", "Note  [Save]  Cancel", "Note  Save  [Cancel]"}
		rows = append(rows, status, buttons[n.focus], "Enter/Ctrl+S save | Alt+Enter newline | Tab | Esc back")
	}
	box := m.box(r, " "+label+" ", rows, -1, true, false)
	for y, row := range box {
		line := lines[r.Y+y]
		lines[r.Y+y] = ansi.Cut(line, 0, r.X) + row + ansi.Cut(line, r.X+r.Width, m.layout.Content.Width)
	}
	return lines
}

func (m Model) noteCursor() *tea.Cursor {
	if m.note == nil || m.confirmation != nil || m.note.pending || m.note.focus != 0 {
		return nil
	}
	cursor := m.note.field.Cursor()
	r := m.layout.Dialog(76, 11)
	cursor.X += r.X + 2 // box border and inner padding
	cursor.Y += r.Y + 3 // border and two frozen-target rows
	return cursor
}
