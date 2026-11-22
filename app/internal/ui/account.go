package ui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// AccountState is a sanitized presentation projection of account/onboarding
// readiness. It never holds credentials, tokens, passwords, or redirect URLs.
type AccountState struct {
	Status     string
	Detail     string
	Error      string
	Busy       bool
	Onboarding bool
	SignedIn   bool
	Offline    bool
}

// AccountStatusMsg updates the Account overlay state without stealing focus.
type AccountStatusMsg struct {
	Account AccountState
}

// OpenAccountMsg opens the Account/onboarding overlay, optionally updating state.
type OpenAccountMsg struct {
	Account *AccountState
}

// CloseAccountMsg closes the Account/onboarding overlay and restores prior focus.
type CloseAccountMsg struct{}

// RestoreSessionMsg restores persisted focus, view, sort, filter, selection
// by BookKey, and optional onboarding/account state without starting playback.
type RestoreSessionMsg struct {
	Focus       *Pane
	View        *LibraryView
	Sort        *SortOrder
	Filter      *string
	Selected    string
	OpenAccount bool
	Account     *AccountState
}

type accountOption struct {
	label  string
	action ActionKind
}

func (m Model) accountOptions() []accountOption {
	closeLabel := "Close"
	if m.account.Onboarding {
		closeLabel = "Continue offline"
	}
	return []accountOption{
		{label: "Login (browser TTY handoff)", action: AccountLogin},
		{label: "Sync library", action: AccountSync},
		{label: "Check runtime (Doctor)", action: AccountDoctor},
		{label: closeLabel, action: AccountContinueOffline},
	}
}

// AccountOpen reports whether the Account/onboarding overlay is currently open.
func (m Model) AccountOpen() bool { return m.accountOpen }

func (m *Model) openAccount(state *AccountState) {
	if state != nil {
		m.account = *state
	}
	if m.ownsModal() || m.help {
		return
	}
	m.accountOpen = true
	m.accountCursor = 0
}

func (m *Model) restoreSession(msg RestoreSessionMsg) {
	if msg.Account != nil {
		m.account = *msg.Account
	}
	if msg.View != nil {
		switch *msg.View {
		case ViewAll, ViewContinue, ViewDownloaded, ViewFinished:
			m.libraryView = *msg.View
			rows := m.librarySidebarRows()
			for i, row := range rows {
				if row.selectable && row.view == m.libraryView {
					m.libraryCursor = i
					break
				}
			}
		}
	}
	if msg.Sort != nil {
		for _, opt := range sortOptions {
			if opt.order == *msg.Sort {
				m.sortOrder = *msg.Sort
				break
			}
		}
	}
	if msg.Filter != nil {
		m.filterQuery = cleanLine(*msg.Filter)
		m.filterDraft = m.filterQuery
	}
	if msg.Focus != nil && *msg.Focus >= Library && *msg.Focus <= PlayerPane {
		m.focus = *msg.Focus
		if m.focus != PlayerPane {
			m.content = m.focus
		}
	}
	if msg.Selected != "" {
		visible := m.visibleBooks()
		for _, book := range visible {
			if book.Key == msg.Selected {
				m.selected = msg.Selected
				break
			}
		}
	}
	m.reconcileSelection()
	if msg.OpenAccount && !m.ownsModal() && !m.help {
		m.accountOpen = true
		m.accountCursor = 0
	}
}

func (m Model) accountKey(key string) (Model, Action, bool) {
	options := m.accountOptions()
	switch key {
	case "esc":
		m.accountOpen = false
		return m, Action{}, false
	case "j", "down", "tab":
		m.accountCursor = (m.accountCursor + 1) % len(options)
		return m, Action{}, false
	case "k", "up", "shift+tab":
		m.accountCursor = (m.accountCursor + len(options) - 1) % len(options)
		return m, Action{}, false
	case "enter":
		choice := options[m.accountCursor]
		if choice.action == AccountContinueOffline {
			m.accountOpen = false
			if m.account.Onboarding && m.dispatch != nil {
				return m, Action{Kind: AccountContinueOffline}, true
			}
			return m, Action{}, false
		}
		if m.account.Busy {
			m.data.Activity = "Account operation already in progress"
			return m, Action{}, false
		}
		if m.dispatch == nil {
			m.data.Activity = "Action unavailable: no coordinator"
			return m, Action{}, false
		}
		return m, Action{Kind: choice.action}, true
	default:
		// Account overlay owns all keys locally: no transport, quit, or pane passthrough.
		return m, Action{}, false
	}
}

func (m Model) accountLines(lines []string) []string {
	r := m.layout.Dialog(76, 15)
	innerWidth := max(1, r.Width-4)
	status := cleanLine(m.account.Status)
	if status == "" {
		status = "Not signed in"
		if m.account.SignedIn {
			status = "Signed in"
		} else if m.account.Offline {
			status = "Offline"
		}
	}
	detail := cleanLine(m.account.Detail)
	if detail == "" {
		detail = "Fresh Taletty root (~/.config/taletty/). No password collected in UI."
	}
	var rows []string
	rows = append(rows, strings.Split(ansi.Wrap("Status: "+status, innerWidth, ""), "\n")...)
	rows = append(rows, strings.Split(ansi.Wrap(detail, innerWidth, ""), "\n")...)
	if errText := cleanLine(m.account.Error); errText != "" {
		rows = append(rows, strings.Split(ansi.Wrap("Error: "+errText, innerWidth, ""), "\n")...)
	}
	rows = append(rows, "")
	selectedRow := len(rows) + m.accountCursor
	for _, opt := range m.accountOptions() {
		rows = append(rows, opt.label)
	}
	capacity := max(1, r.Height-2)
	if len(rows) > capacity {
		// Keep the selected option visible inside compact 80x20 dialogs.
		start := max(0, selectedRow-capacity+1)
		if start+capacity > len(rows) {
			start = max(0, len(rows)-capacity)
		}
		rows = rows[start : start+capacity]
		selectedRow -= start
	}
	box := m.box(r, " [Focus] Account ", rows, selectedRow, true, false)
	for y, row := range box {
		line := lines[r.Y+y]
		lines[r.Y+y] = ansi.Cut(line, 0, r.X) + row + ansi.Cut(line, r.X+r.Width, m.layout.Content.Width)
	}
	return lines
}
