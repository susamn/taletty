package app

import (
	"context"
	"io"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/terminal"
	"github.com/susamn/taletty/app/internal/ui"
)

const playbackPollInterval = 500 * time.Millisecond

type playbackTickMsg struct{}

type sessionModel struct {
	inner tea.Model
	ctx   context.Context
	coord *Coordinator
}

func (m sessionModel) Init() tea.Cmd {
	var cmds []tea.Cmd
	if initCmd := m.inner.Init(); initCmd != nil {
		cmds = append(cmds, initCmd)
	}
	if m.coord != nil {
		for _, key := range m.coord.DownloadedBookKeys(m.ctx) {
			k := key
			cmds = append(cmds, func() tea.Msg {
				if msg, ok := m.coord.LoadChapters(m.ctx, k); ok {
					return msg
				}
				return nil
			})
		}
		if m.coord.player != nil {
			cmds = append(cmds, tea.Tick(playbackPollInterval, func(time.Time) tea.Msg {
				return playbackTickMsg{}
			}))
		}
	}
	return tea.Batch(cmds...)
}

func (m sessionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(playbackTickMsg); ok {
		var cmds []tea.Cmd
		if m.coord != nil && m.coord.player != nil {
			if snap, ok := m.coord.PollPlayback(m.ctx); ok {
				next, cmd := m.inner.Update(snap)
				m.inner = next
				if cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
			cmds = append(cmds, tea.Tick(playbackPollInterval, func(time.Time) tea.Msg {
				return playbackTickMsg{}
			}))
		}
		return m, tea.Batch(cmds...)
	}
	next, cmd := m.inner.Update(msg)
	m.inner = next
	return m, cmd
}

func (m sessionModel) View() tea.View {
	return m.inner.View()
}

// ProgramRunner executes a Bubble Tea model on an owned controlling terminal.
// terminal.Run satisfies this signature directly.
type ProgramRunner func(ctx context.Context, model tea.Model, tty *os.File, output io.Writer) (tea.Model, error)

// RunSession runs the interactive TUI session on tty, cleanly exiting and
// restoring terminal state around interactive Login handoffs before resuming.
func RunSession(ctx context.Context, coord *Coordinator, tty *os.File, output io.Writer, runner ProgramRunner) error {
	if ctx == nil || coord == nil || tty == nil {
		return ErrInvalidInput
	}
	if runner == nil {
		runner = terminal.Run
	}

	restore := coord.InitialRestore()
	appearance := coord.InitialAppearance()
	focus := ui.Books
	if restore.Focus != nil {
		focus = *restore.Focus
	}
	view := ui.ViewAll
	if restore.View != nil {
		view = *restore.View
	}
	sortOrder := ui.SortTitle
	if restore.Sort != nil {
		sortOrder = *restore.Sort
	}
	filterText := ""
	if restore.Filter != nil {
		filterText = *restore.Filter
	}
	selected := restore.Selected
	accountOpen := restore.OpenAccount

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		acct := coord.AccountState()
		model := ui.NewModel(coord.Snapshot(), coord.Dispatch)
		updated, _ := model.Update(appearance)
		updated, _ = updated.Update(ui.RestoreSessionMsg{
			Focus:       &focus,
			View:        &view,
			Sort:        &sortOrder,
			Filter:      &filterText,
			Selected:    selected,
			OpenAccount: accountOpen,
			Account:     &acct,
		})

		wrapped := sessionModel{inner: updated, ctx: ctx, coord: coord}
		finalModel, runErr := runner(ctx, wrapped, tty, output)
		if sm, ok := finalModel.(sessionModel); ok {
			finalModel = sm.inner
		}
		if finalUI, ok := finalModel.(ui.Model); ok {
			focus = finalUI.Focus()
			selected = finalUI.Selection()
			view, sortOrder, filterText = finalUI.ViewState()
			accountOpen = finalUI.AccountOpen()
		}
		if runErr != nil {
			return runErr
		}
		if coord.TakePendingLogin() {
			coord.ExecutePendingLogin(ctx, tty)
			if err := ctx.Err(); err != nil {
				return err
			}
			accountOpen = true
			continue
		}
		return coord.SaveFullViewState(context.Background(), view, sortOrder, filterText, focus, selected)
	}
}
