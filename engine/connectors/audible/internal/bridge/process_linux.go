package bridge

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

var (
	ErrStart   = errors.New("bridge process could not start")
	ErrProcess = errors.New("bridge process did not complete cleanly")
)

const (
	termGrace = 250 * time.Millisecond
	pipeDrain = 2 * time.Second
)

type processTiming struct{ grace, drain time.Duration }
type decodedStream struct {
	frame Frame
	err   error
}

// Run owns one Linux noninteractive child and its process group. Request must be
// one UTF-8 JSON object (<=4 MiB) with matching protocol/operation_id; action/root
// validation belongs to the typed connector. No credentials belong in argv/request.
// Stderr is discarded, never buffered or exposed. No shell is invoked.
//
// frames, if nonnil, receives provisional nonterminal envelopes, with cancellable
// backpressure. Caller owns the channel, must not close it until Run returns, and
// must discard partial snapshots on failure. No arbitrary callback runs here.
// A returned terminal still needs action-payload validation (including error kind).
// Run never deletes files; publication reconciliation belongs to Download.
func Run(ctx context.Context, program string, args []string, request []byte, operationID string, frames chan<- Frame) (Frame, error) {
	return runProcess(ctx, program, args, request, operationID, frames, processTiming{termGrace, pipeDrain}, observeExit)
}

func runProcess(ctx context.Context, program string, args []string, request []byte, operationID string, frames chan<- Frame, timing processTiming, observe func(int) error) (Frame, error) {
	return runConfigured(ctx, program, args, request, operationID, frames, timing, observe, nil)
}

func runConfigured(ctx context.Context, program string, args []string, request []byte, operationID string, frames chan<- Frame, timing processTiming, observe func(int) error, files *ttyFiles) (terminal Frame, err error) {
	if err := ctx.Err(); err != nil {
		return Frame{}, err
	}
	if len(request) > MaxFrameBytes || !utf8.Valid(request) {
		return Frame{}, ErrProtocol
	}
	header, err := decodeEnvelope(request)
	if err != nil || operationID == "" || header.Protocol != Version || header.OperationID != operationID {
		return Frame{}, ErrProtocol
	}
	// Validation can consume CPU for a bounded large request. Recheck before
	// allocating pipes or starting work if cancellation arrived during it.
	if err := ctx.Err(); err != nil {
		return Frame{}, err
	}
	stdout, writer, err := os.Pipe()
	if err != nil {
		return Frame{}, ErrStart
	}
	defer stdout.Close()
	cmd := exec.Command(program, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if files != nil {
		cmd.ExtraFiles = []*os.File{files.root, files.terminal}
		cmd.SysProcAttr.Foreground = true
		cmd.SysProcAttr.Ctty = int(files.terminal.Fd()) // Parent FD, before exec's FD remapping.
	}
	cmd.Stdin = bytes.NewReader(append(append([]byte(nil), request...), '\n'))
	cmd.Stdout = writer
	cmd.WaitDelay = timing.drain // Bound exec's stdin-copy join after child exit.
	err = cmd.Start()
	writer.Close() // Parent must not keep stdout alive, including on Start failure.
	if err != nil {
		return Frame{}, ErrStart
	}

	work, cancel := context.WithCancel(ctx)
	decoded := make(chan decodedStream, 1)
	exited := make(chan error, 1)
	go func() {
		frame, err := ReadStream(stdout, operationID, func(frame Frame) error {
			return sendFrame(work, frames, frame)
		})
		decoded <- decodedStream{frame, err}
	}()
	go func() { exited <- observe(cmd.Process.Pid) }()

	var sawDecode, sawExit bool
	var observeErr error
	var drain *time.Timer
	defer func() {
		cancel()       // Release a stalled channel send before joining the reader.
		stdout.Close() // Interrupt a blocked pipe read, even with inherited writers.
		if drain != nil {
			drain.Stop()
		}
		if !sawExit {
			unix.Kill(-cmd.Process.Pid, unix.SIGTERM)
			timer := time.NewTimer(timing.grace)
			select {
			case observeErr = <-exited:
				sawExit = true
			case <-timer.C:
			}
			timer.Stop()
		}
		// Always kill residual group members BEFORE reaping the leader. WNOWAIT
		// keeps its PID reserved, so cleanup cannot signal a reused process group.
		unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		if !sawExit {
			observeErr = <-exited
		}
		waitErr := cmd.Wait() // Sole reaper; also joins exec's owned stdin copier.
		if !sawDecode {
			<-decoded
		}
		if ctx.Err() != nil {
			terminal, err = Frame{}, ctx.Err()
		} else if err == nil && (observeErr != nil || waitErr != nil) {
			terminal, err = Frame{}, ErrProcess
		}
	}()

	var drainDone <-chan time.Time
	for !sawDecode || !sawExit {
		select {
		case result := <-decoded:
			sawDecode = true
			if result.err != nil {
				return Frame{}, result.err
			}
			terminal = result.frame
		case observeErr = <-exited:
			sawExit = true
			if observeErr != nil {
				return Frame{}, ErrProcess
			}
			// A dead leader can leave descendants holding stdout or a blocked
			// consumer. Allow bounded draining, never wait forever for their EOF.
			drain = time.NewTimer(timing.drain)
			drainDone = drain.C
		case <-drainDone:
			return Frame{}, ErrProcess
		case <-ctx.Done():
			return Frame{}, ctx.Err()
		}
	}
	return terminal, nil
}

// A bounded handoff is independently testable without timing a subprocess reader.
func sendFrame(ctx context.Context, frames chan<- Frame, frame Frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if frames == nil {
		return nil
	}
	select {
	case frames <- frame:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Observe without reaping: only Run's deferred cleanup may release the PID anchor.
// Callers must not use an external SIGCHLD reaper for children owned by Run.
func observeExit(pid int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err != unix.EINTR {
			return err
		}
	}
}
