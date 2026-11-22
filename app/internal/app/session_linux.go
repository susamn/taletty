package app

import (
	"context"
	"io"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/downloads"
	"github.com/susamn/taletty/app/internal/terminal"
	"github.com/susamn/taletty/app/internal/theme"
	"github.com/susamn/taletty/app/internal/ui"
)

const playbackPollInterval = 500 * time.Millisecond

type playbackTickMsg struct{}

type downloadWatchMsg struct {
	snap downloads.Snapshot
	ch   <-chan downloads.Snapshot
}

type themeSignalMsg struct {
	gen uint64
	ch  <-chan uint64
}

type sessionModel struct {
	inner      tea.Model
	ctx        context.Context
	coord      *Coordinator
	themeSigCh <-chan uint64
}

func waitThemeSignalCmd(ch <-chan uint64) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		gen, ok := <-ch
		if !ok {
			return nil
		}
		return themeSignalMsg{gen: gen, ch: ch}
	}
}

func waitDownloadCmd(ch <-chan downloads.Snapshot) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		snap, ok := <-ch
		if !ok {
			return nil
		}
		return downloadWatchMsg{snap: snap, ch: ch}
	}
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
		if m.coord.downloadsMgr != nil {
			cmds = append(cmds, func() tea.Msg {
				return m.coord.DownloadsSnapshot()
			})
			cmds = append(cmds, waitDownloadCmd(m.coord.WatchDownloads(m.ctx)))
		}
		if m.coord.player != nil {
			cmds = append(cmds, tea.Tick(playbackPollInterval, func(time.Time) tea.Msg {
				return playbackTickMsg{}
			}))
		}
		if m.themeSigCh != nil {
			cmds = append(cmds, waitThemeSignalCmd(m.themeSigCh))
		}
	}
	return tea.Batch(cmds...)
}

func (m sessionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ts, ok := msg.(themeSignalMsg); ok {
		var cmds []tea.Cmd
		if m.coord != nil {
			if res := m.coord.HandleThemeSignal(m.ctx, ts.gen); res != nil {
				next, cmd := m.applyDispatchedMsg(res)
				m.inner = next
				if cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		}
		if nextSig := waitThemeSignalCmd(ts.ch); nextSig != nil {
			cmds = append(cmds, nextSig)
		}
		return m, tea.Batch(cmds...)
	}
	if dw, ok := msg.(downloadWatchMsg); ok {
		var cmds []tea.Cmd
		if m.coord != nil {
			dlMsg := m.coord.ProjectDownloads(dw.snap)
			next, cmd := m.inner.Update(dlMsg)
			m.inner = next
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
			snapMsg := m.coord.RefreshSnapshotFromDownloads(m.ctx, dw.snap)
			next, cmd = m.inner.Update(snapMsg)
			m.inner = next
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
			if dw.snap.Active == nil {
				for _, h := range dw.snap.History {
					if h.State == "ready" && len(m.coord.cachedChaptersFor(&h.Key)) == 0 {
						k := h.Key
						cmds = append(cmds, func() tea.Msg {
							if chMsg, ok := m.coord.LoadChapters(m.ctx, k); ok {
								return chMsg
							}
							return nil
						})
					}
				}
			}
		}
		if nextWatch := waitDownloadCmd(dw.ch); nextWatch != nil {
			cmds = append(cmds, nextWatch)
		}
		return m, tea.Batch(cmds...)
	}
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

func (m sessionModel) applyDispatchedMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	if batch, ok := msg.(tea.BatchMsg); ok {
		var cmds []tea.Cmd
		cur := m.inner
		for _, fn := range batch {
			if fn == nil {
				continue
			}
			sub := fn()
			if sub == nil {
				continue
			}
			next, cmd := cur.Update(sub)
			cur = next
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		return cur, tea.Batch(cmds...)
	}
	return m.inner.Update(msg)
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
		updated, _ := model.Update(coord.InitialAppearance())
		updated, _ = updated.Update(coord.InitialSettings())
		updated, _ = updated.Update(ui.RestoreSessionMsg{
			Focus:       &focus,
			View:        &view,
			Sort:        &sortOrder,
			Filter:      &filterText,
			Selected:    selected,
			OpenAccount: accountOpen,
			Account:     &acct,
		})

		sigCtx, sigCancel := context.WithCancel(ctx)
		var sigCh chan uint64
		sigDone := make(chan struct{})
		if coord.themeMgr != nil {
			sigCh = make(chan uint64, 1)
			go func() {
				defer close(sigDone)
				theme.WatchSIGUSR1(sigCtx, func() {
					gen := coord.ThemeGeneration()
					select {
					case sigCh <- gen:
					default:
					}
				})
			}()
		} else {
			close(sigDone)
		}

		wrapped := sessionModel{inner: updated, ctx: ctx, coord: coord, themeSigCh: sigCh}
		finalModel, runErr := runner(ctx, wrapped, tty, output)
		sigCancel()
		<-sigDone
		if sigCh != nil {
			close(sigCh)
		}
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
