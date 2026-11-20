package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"testing"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/sys/unix"
)

type exitModel struct{}

func (exitModel) Init() tea.Cmd                         { return tea.Quit }
func (m exitModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return m, nil }
func (exitModel) View() tea.View                        { return tea.NewView("Terminal development boundary") }

type shortWriter struct{}

func (shortWriter) Write([]byte) (int, error) { return 0, nil }

func TestRunValidation(t *testing.T) {
	for _, ctx := range []context.Context{nil, context.Background()} {
		if _, err := Run(ctx, exitModel{}, nil, nil); !errors.Is(err, ErrTerminal) {
			t.Fatal(err)
		}
	}
	file, err := os.CreateTemp(t.TempDir(), "not-tty")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := Run(context.Background(), nil, file, nil); !errors.Is(err, ErrTerminal) {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), exitModel{}, file, nil); !errors.Is(err, ErrTerminal) {
		t.Fatal(err)
	}
}

// Direct lifecycle tests measure all production error/restore paths in the
// ordinary module profile. UI subprocess journeys separately prove controlling
// foreground ownership and actual terminal-decoded input.
func TestProgramLifecycle(t *testing.T) {
	for _, mode := range []string{"quit", "cancel", "writer", "closed"} {
		t.Run(mode, func(t *testing.T) {
			master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			if err = unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
				t.Fatal(err)
			}
			number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
			if err != nil {
				t.Fatal(err)
			}
			tty, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer tty.Close()
			state, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var output io.Writer
			switch mode {
			case "cancel":
				cancel()
			case "writer":
				output = shortWriter{}
			case "closed":
				tty.Close()
			}
			_, err = runProgram(ctx, exitModel{}, tty, output, state)
			switch mode {
			case "quit":
				if err != nil {
					t.Fatal(err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			default:
				if err == nil {
					t.Fatal("missing terminal failure")
				}
			}
			if mode != "closed" {
				actual, e := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
				if e != nil || *actual != *state {
					t.Fatal("termios not restored", e)
				}
			}
		})
	}
}
