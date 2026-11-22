package app

import (
	"context"
	"io"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/terminal"
	"github.com/susamn/taletty/app/internal/ui"
)

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
			Selected:    selected,
			OpenAccount: accountOpen,
			Account:     &acct,
		})

		finalModel, runErr := runner(ctx, updated, tty, output)
		if finalUI, ok := finalModel.(ui.Model); ok {
			focus = finalUI.Focus()
			selected = finalUI.Selection()
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
		return coord.SaveViewState(context.Background(), focus, selected)
	}
}
