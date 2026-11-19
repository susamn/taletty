package bridge

import (
	"context"
	"errors"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

var ErrTerminal = errors.New("login terminal ownership or restoration failed")

// A process may perform only one interactive foreground handoff at a time, even
// across distinct roots. Read-only/noninteractive work never takes this lease.
var terminalLease = make(chan struct{}, 1)

// RunTTY reuses Run's bounded control decoder and complete process-group owner.
// Prompts/input use only terminal (child FD4), never the private JSONL stdout pipe.
// root is child FD3; neither file is closed here. Caller must suspend other terminal
// users, own the controlling foreground TTY and retain both files until return.
// The launcher must consume the request then isolate SDK stdio from control output.
func RunTTY(ctx context.Context, program string, args []string, request []byte, operationID string, root, terminal *os.File, frames chan<- Frame) (frame Frame, err error) {
	select {
	case terminalLease <- struct{}{}:
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	}
	defer func() { <-terminalLease }()
	if err := ctx.Err(); err != nil {
		return Frame{}, err
	}
	if root == nil || terminal == nil {
		return Frame{}, ErrTerminal
	}
	fd := int(terminal.Fd())
	foreground, getErr := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if getErr != nil || foreground != unix.Getpgrp() {
		return Frame{}, ErrTerminal
	}
	saved, getErr := unix.IoctlGetTermios(fd, unix.TCGETS)
	if getErr != nil {
		return Frame{}, ErrTerminal
	}
	// Restore even if Start sets foreground ownership but subsequently exec fails.
	// runConfigured joins/kills/reaps all owned users BEFORE this defer runs.
	defer func() {
		if restoreTTY(fd, foreground, saved) != nil {
			frame, err = Frame{}, errors.Join(err, ErrTerminal)
		}
	}()
	return runConfigured(ctx, program, args, request, operationID, frames,
		processTiming{termGrace, pipeDrain}, observeExit, &processFiles{root, terminal})
}

func restoreTTY(fd, foreground int, saved *unix.Termios) error {
	// tcsetpgrp from the now-background parent must block SIGTTOU on this calling
	// thread. Never alter process-wide signal dispositions or other runtime threads.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var block, previous unix.Sigset_t
	block.Val[0] = 1 << (unix.SIGTTOU - 1)
	if unix.PthreadSigmask(unix.SIG_BLOCK, &block, &previous) != nil {
		return ErrTerminal
	}
	groupErr := unix.IoctlSetPointerInt(fd, unix.TIOCSPGRP, foreground)
	modeErr := unix.IoctlSetTermios(fd, unix.TCSETSF, saved) // Flush residual secret input.
	maskErr := unix.PthreadSigmask(unix.SIG_SETMASK, &previous, nil)
	if groupErr != nil || modeErr != nil || maskErr != nil {
		return ErrTerminal
	}
	return nil
}
