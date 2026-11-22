package ui

import (
	"image/color"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"
)

type Pane int

const (
	Library Pane = iota
	Books
	Details
	Chapters
	PlayerPane
)

type GlyphMode int

const (
	Unicode GlyphMode = iota
	Plain
)

// Palette contains semantic colors only. Nil colors use the terminal defaults;
// loading/validating colors.toml and selecting a palette belong to the app layer.
type Palette struct{ Background, Text, Border, Focus, Selection, Muted, Error color.Color }
type AppearanceMsg struct {
	Palette Palette
	Glyphs  GlyphMode
}

type Availability int

const (
	AvailabilityUnknown Availability = iota
	NotDownloaded
	Queued
	Downloading
	Preparing
	Ready
	Failed
	Canceled
)

type PlaybackState int

const (
	Stopped PlaybackState = iota
	Playing
	Paused
	Finished
	PlaybackFailed
)

// Book is a display projection, not a second engine book model. Key is an opaque
// identity supplied by the coordinator, never a title, row number or file path.
type Book struct {
	Key, Title, Author, Description string
	Chapters                        []string
	Availability                    Availability
	Favorite                        bool
}

type Player struct {
	Key, Session, Title, Chapter, Sleep string
	State                               PlaybackState
	Position, Duration                  time.Duration
	Speed                               float64
	Volume                              int
	VolumeKnown                         bool
}

type SnapshotMsg struct {
	Books           []Book
	Player          Player
	Activity, Error string
}

type NoticeMsg struct{ Activity, Error string }
type DismissFailureMsg struct{}

type ActionKind string

const (
	PlayBook               ActionKind = "play-book"
	TogglePlayback         ActionKind = "toggle-playback"
	SeekBackward           ActionKind = "seek-backward"
	SeekForward            ActionKind = "seek-forward"
	VolumeDown             ActionKind = "volume-down"
	VolumeUp               ActionKind = "volume-up"
	SpeedDown              ActionKind = "speed-down"
	SpeedUp                ActionKind = "speed-up"
	RequestQuit            ActionKind = "request-quit"
	SaveNote               ActionKind = "save-note"
	AccountLogin           ActionKind = "account-login"
	AccountSync            ActionKind = "account-sync"
	AccountDoctor          ActionKind = "account-doctor"
	AccountContinueOffline ActionKind = "account-continue-offline"
)

// Current-audio actions carry the source session so the coordinator can reject
// stale commands after replacement, including a reload of the same book.
type Action struct {
	Kind         ActionKind
	Key, Session string
	Target       NoteTarget
	Text         string
	RequestID    string
	Attempt      uint64
}

// Model owns presentation only. Dispatch creates commands; it must not perform
// blocking I/O. Playback is authoritative only when a coordinator sends a snapshot.
type Model struct {
	data          SnapshotMsg
	appearance    AppearanceMsg
	layout        Layout
	focus         Pane
	content       Pane
	selected      string
	control       int
	failure       string
	help          bool
	helpOffset    int
	accountOpen   bool
	accountCursor int
	account       AccountState
	note          *noteEditor
	confirmation  *confirmation
	quitWarning   bool
	dispatch      func(Action) tea.Cmd
}

func NewModel(data SnapshotMsg, dispatch func(Action) tea.Cmd) Model {
	m := Model{focus: Books, content: Books, dispatch: dispatch}
	m.accept(data)
	return m
}

func (m Model) Init() tea.Cmd     { return nil }
func (m Model) Focus() Pane       { return m.focus }
func (m Model) Selection() string { return m.selected }

func (m *Model) accept(data SnapshotMsg) {
	data.Books = slices.Clone(data.Books)
	for i := range data.Books {
		data.Books[i].Chapters = slices.Clone(data.Books[i].Chapters)
	}
	m.data = data
	if m.bookIndex() < 0 {
		m.selected = ""
		if len(data.Books) > 0 {
			m.selected = data.Books[0].Key
		}
	}
	if data.Error != "" {
		m.failure = data.Error
	}
}

func (m Model) bookIndex() int {
	return slices.IndexFunc(m.data.Books, func(book Book) bool { return book.Key == m.selected })
}

func (m Model) request(action Action) (tea.Model, tea.Cmd) {
	if m.dispatch == nil {
		m.data.Activity = "Action unavailable: no coordinator"
		return m, nil
	}
	return m, m.dispatch(action)
}

func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.layout = ComputeLayout(msg.Width, msg.Height)
		m.sizeNote()
		if m.layout.Mode != ResizeOnly {
			m.quitWarning = false
		}
	case OpenNoteEditorMsg:
		m.openNote(msg)
	case OpenConfirmationMsg:
		if msg.ID != "" && msg.Title != "" && msg.Keep != "" && msg.Confirm != "" && msg.Action.Kind != "" && !m.ownsModalPending() && m.confirmation == nil {
			m.confirmation = &confirmation{spec: msg}
		}
	case ModalResultMsg:
		return m.modalResult(msg)
	case tea.PasteMsg:
		if m.layout.Mode != ResizeOnly && m.note != nil && m.confirmation == nil && !m.note.pending && m.note.focus == 0 {
			m.note.field, _ = m.note.field.Update(tea.PasteMsg{Content: safeNote(msg.Content)})
		}
	case OpenAccountMsg:
		m.openAccount(msg.Account)
	case CloseAccountMsg:
		m.accountOpen = false
	case AccountStatusMsg:
		m.account = msg.Account
	case RestoreSessionMsg:
		m.restoreSession(msg)
	case SnapshotMsg:
		m.accept(msg)
	case AppearanceMsg:
		m.appearance = msg
	case NoticeMsg:
		m.data.Activity = msg.Activity
		if msg.Error != "" {
			m.failure = msg.Error
		}
	case DismissFailureMsg:
		m.failure = ""
	case tea.KeyPressMsg:
		key := msg.String()
		// The resize owner supersedes hidden editors: never send hidden typing or
		// transport through. Only a repeated q confirms discarding an unsaved draft.
		if m.layout.Mode == ResizeOnly {
			if key == "esc" {
				m.quitWarning = false
			} else if key == "q" || key == "ctrl+c" {
				if m.note != nil && !(key == "q" && m.quitWarning) {
					m.quitWarning = true
					return m, nil
				}
				m.quitWarning = false
				return m.request(Action{Kind: RequestQuit})
			}
			return m, nil
		}
		if m.ownsModal() {
			return m.modalKey(msg)
		}
		if m.help {
			return m.helpKey(key)
		}
		if m.accountOpen {
			next, action, send := m.accountKey(key)
			if send {
				return next.request(action)
			}
			return next, nil
		}
		if key == "q" || key == "ctrl+c" {
			return m.request(Action{Kind: RequestQuit})
		}
		switch key {
		case "a":
			m.openAccount(nil)
			return m, nil
		case "?":
			m.help, m.helpOffset = true, 0
		case "tab":
			m.focus = (m.focus + 1) % 5
		case "shift+tab":
			m.focus = (m.focus + 4) % 5
		case "1", "2", "3", "4", "5":
			m.focus = Pane(key[0] - '1')
		case "j", "down", "k", "up":
			if m.focus == Books && len(m.data.Books) > 0 {
				delta := 1
				if key == "k" || key == "up" {
					delta = -1
				}
				index := min(len(m.data.Books)-1, max(0, m.bookIndex()+delta))
				m.selected = m.data.Books[index].Key
			}
		case "left", "right":
			if m.focus == PlayerPane {
				delta := 1
				if key == "left" {
					delta = -1
				}
				m.control = (m.control + delta + 3) % 3
			}
		case "enter":
			if (m.focus == Books || m.focus == Details) && m.selected != "" {
				return m.request(Action{Kind: PlayBook, Key: m.selected})
			}
			if m.focus == PlayerPane {
				return m.transport([]ActionKind{TogglePlayback, SeekBackward, SeekForward}[m.control])
			}
		default:
			if action, ok := transportKeys[key]; ok {
				return m.transport(action)
			}
		}
		if m.focus != PlayerPane {
			m.content = m.focus
		}
	}
	return m, nil
}

func (p Player) transportAvailable() bool {
	return p.Key != "" && p.Session != "" && (p.State == Playing || p.State == Paused)
}

func (m Model) transport(action ActionKind) (tea.Model, tea.Cmd) {
	p := m.data.Player
	if !p.transportAvailable() {
		m.data.Activity = "Transport disabled: no loaded audio"
		return m, nil
	}
	return m.request(Action{Kind: action, Key: p.Key, Session: p.Session})
}
