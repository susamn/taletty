// Package terminal owns the real Bubble Tea terminal lifecycle, not application
// services. Callers cancel the context for external shutdown and must join their
// own workers before returning tea.Quit from the coordinator.
package terminal

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/sys/unix"
)

var ErrTerminal = errors.New("terminal unavailable or not foreground")

// Run borrows a controlling terminal. Nil output uses that terminal; a separate
// writer supports embedding, but never changes input ownership. No implicit TTY
// discovery, provider access, environment test hooks or global signal handlers.
// The caller must exclusively own this foreground terminal until Run returns.
func Run(ctx context.Context, model tea.Model, tty *os.File, output io.Writer) (final tea.Model, err error) {
	if ctx == nil || model == nil || tty == nil {
		return model, ErrTerminal
	}
	fd := int(tty.Fd())
	state, stateErr := unix.IoctlGetTermios(fd, unix.TCGETS)
	group, groupErr := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if stateErr != nil || groupErr != nil || group != unix.Getpgrp() {
		return model, ErrTerminal
	}
	return runProgram(ctx, model, tty, output, state)
}

func runProgram(ctx context.Context, model tea.Model, tty *os.File, output io.Writer, state *unix.Termios) (final tea.Model, err error) {
	fd := int(tty.Fd())
	// Bubble Tea v2 ignores restoration errors during shutdown and can return
	// early after raw mode initialization. Always restore our borrowed state,
	// reporting failures rather than silently claiming a usable terminal.
	defer func() {
		if restoreErr := unix.IoctlSetTermios(fd, unix.TCSETS, state); restoreErr != nil {
			err = errors.Join(err, ErrTerminal)
		}
	}()
	if output == nil {
		output = tty
	}
	size, sizeErr := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if sizeErr != nil {
		return model, ErrTerminal
	}
	writer := &checkedWriter{File: tty, output: output}
	// Bubble Tea's context-kill path closes its reader without joining input.
	// Translate external cancellation to orderly Quit instead. The model and
	// commands must remain nonblocking/cooperatively canceled by their owner.
	program := tea.NewProgram(model, tea.WithContext(context.WithoutCancel(ctx)), tea.WithInput(tty),
		tea.WithOutput(writer), tea.WithWindowSize(int(size.Col), int(size.Row)), tea.WithoutSignalHandler())
	stop, joined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(joined)
		select {
		case <-ctx.Done():
			program.Quit()
		case <-stop:
		}
	}()
	final, err = program.Run()
	close(stop)
	<-joined
	return final, errors.Join(err, ctx.Err(), writer.failure())
}

// Bubble Tea's renderer does not propagate write errors. Keep that failure
// visible without exposing arbitrary writer diagnostics to the UI.
type checkedWriter struct {
	*os.File
	mu     sync.Mutex
	output io.Writer
	err    error
}

func (w *checkedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.output.Write(p)
	if err != nil || n != len(p) {
		w.err = errors.New("terminal output failed")
	}
	return n, err
}
func (w *checkedWriter) failure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}
