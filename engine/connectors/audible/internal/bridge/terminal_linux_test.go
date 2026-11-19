package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func terminalPair(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

func TestOwnedForegroundTTYJourneys(t *testing.T) {
	for _, mode := range []string{"success", "cancel", "lost", "nonzero", "start", "background"} {
		t.Run(mode, func(t *testing.T) {
			master, slave := terminalPair(t)
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTTYHostChild$")
			cmd.Env = append(os.Environ(), "TALETTY_TTY_HOST="+mode, "TALETTY_TTY_ROOT="+root, "GORACE=atexit_sleep_ms=0")
			cmd.ExtraFiles = []*os.File{slave}
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 3}
			var output strings.Builder
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// Always reap even if readiness assertions fail.
			defer func() { cmd.Process.Kill(); cmd.Wait() }()
			if mode == "success" {
				ready := []unix.PollFd{{Fd: int32(master.Fd()), Events: unix.POLLIN}}
				if n, err := unix.Poll(ready, 5000); err != nil || n != 1 {
					t.Fatal("no terminal barrier", n, err)
				}
				data := make([]byte, 64)
				if n, err := master.Read(data); err != nil || !strings.Contains(string(data[:n]), "ready") {
					t.Fatal("invalid terminal barrier", err)
				}
				if _, err := master.WriteString("fixture-input\n"); err != nil {
					t.Fatal(err)
				}
			}
			if err := cmd.Wait(); err != nil || strings.TrimSpace(output.String()) != "ok" {
				t.Fatal(mode, err, output.String())
			}
		})
	}
}

func TestTTYHostChild(t *testing.T) {
	mode := os.Getenv("TALETTY_TTY_HOST")
	if mode == "" {
		return
	}
	terminal := os.NewFile(3, "fixture-tty")
	root, err := os.Open(os.Getenv("TALETTY_TTY_ROOT"))
	if err != nil {
		os.Exit(1)
	}
	saved, err := unix.IoctlGetTermios(3, unix.TCGETS)
	if err != nil {
		os.Exit(1)
	}
	foreground, err := unix.IoctlGetInt(3, unix.TIOCGPGRP)
	if err != nil {
		os.Exit(1)
	}
	if mode == "background" {
		busy := exec.Command(os.Args[0], "-test.run=^TestTTYBusyChild$")
		input, err := busy.StdinPipe()
		if err != nil {
			os.Exit(1)
		}
		busy.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if busy.Start() != nil {
			os.Exit(1)
		}
		if unix.IoctlSetPointerInt(3, unix.TIOCSPGRP, busy.Process.Pid) != nil {
			os.Exit(1)
		}
		_, err = RunTTY(context.Background(), os.Args[0], nil, nil, "", root, terminal, nil)
		if !errors.Is(err, ErrTerminal) || restoreTTY(3, foreground, saved) != nil {
			os.Exit(1)
		}
		input.Close()
		busy.Process.Kill()
		busy.Wait()
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		args := []string{"-test.run=^TestTTYFixtureChild$"}
		os.Setenv("TALETTY_TTY_FIXTURE", mode)
		program := os.Args[0]
		if mode == "start" {
			program = filepath.Join(root.Name(), "not-executable")
			if os.WriteFile(program, []byte("fixture"), 0600) != nil {
				os.Exit(1)
			}
		}
		frames := make(chan Frame, 1)
		joined := make(chan struct{})
		spare, dupErr := unix.Dup(3)
		if dupErr != nil {
			os.Exit(1)
		}
		defer unix.Close(spare)
		if mode == "cancel" || mode == "lost" {
			go func() {
				select {
				case <-frames:
					if mode == "lost" {
						terminal.Close()
					}
					cancel()
				case <-ctx.Done():
				}
				close(joined)
			}()
		} else {
			close(joined)
		}
		request := []byte(`{"protocol":1,"operation_id":"tty-fixture"}`)
		frame, runErr := RunTTY(ctx, program, args, request, "tty-fixture", root, terminal, frames)
		<-joined
		want := error(nil)
		switch mode {
		case "cancel", "lost":
			want = context.Canceled
		case "nonzero":
			want = ErrProcess
		case "start":
			want = ErrStart
		}
		if mode == "lost" {
			if !errors.Is(runErr, ErrTerminal) || restoreTTY(spare, foreground, saved) != nil {
				os.Exit(1)
			}
			if err := unix.Dup2(spare, 3); err != nil {
				os.Exit(1)
			}
		}
		if !errors.Is(runErr, want) || (mode == "success" && frame.Type != Result) {
			fmt.Fprintln(os.Stderr, "wrong run outcome", runErr)
			os.Exit(1)
		}
	}
	actual, err := unix.IoctlGetTermios(3, unix.TCGETS)
	group, groupErr := unix.IoctlGetInt(3, unix.TIOCGPGRP)
	if err != nil || groupErr != nil || *actual != *saved || group != foreground {
		os.Exit(1)
	}
	root.Close()
	os.Stdout.WriteString("ok\n")
	os.Exit(0)
}

func TestTTYBusyChild(t *testing.T) {
	if os.Getenv("TALETTY_TTY_HOST") != "background" {
		return
	}
	io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestTTYFixtureChild(t *testing.T) {
	mode := os.Getenv("TALETTY_TTY_FIXTURE")
	if mode == "" {
		return
	}
	io.Copy(io.Discard, os.Stdin)
	group, err := unix.IoctlGetInt(4, unix.TIOCGPGRP)
	if err != nil || group != unix.Getpgrp() {
		os.Exit(1)
	}
	attributes, err := unix.IoctlGetTermios(4, unix.TCGETS)
	if err != nil {
		os.Exit(1)
	}
	attributes.Lflag &^= unix.ECHO | unix.ECHONL
	if unix.IoctlSetTermios(4, unix.TCSETS, attributes) != nil {
		os.Exit(1)
	}
	tty := os.NewFile(4, "fixture-terminal")
	if mode == "cancel" || mode == "lost" {
		signal.Ignore(syscall.SIGTERM)
		os.Stdout.WriteString("{\"protocol\":1,\"operation_id\":\"tty-fixture\",\"seq\":1,\"type\":\"stage\"}\n")
	} else {
		tty.WriteString("ready\n")
	}
	if mode != "nonzero" {
		var input [64]byte
		tty.Read(input[:])
	}
	os.Stdout.WriteString("{\"protocol\":1,\"operation_id\":\"tty-fixture\",\"seq\":1,\"type\":\"result\"}\n")
	if mode == "nonzero" {
		os.Exit(7)
	}
	os.Exit(0) // Deliberately no terminal restoration: the parent owner must do it.
}

func TestTTYRejectedAndLeaseCancellation(t *testing.T) {
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, terminal := range []*os.File{nil, file} {
		if _, err := RunTTY(context.Background(), "unused", nil, nil, "", file, terminal, nil); !errors.Is(err, ErrTerminal) {
			t.Fatal(err)
		}
	}
	terminalLease <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RunTTY(ctx, "unused", nil, nil, "", file, file, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	<-terminalLease
	live, cancelLive := context.WithCancel(context.Background())
	defer cancelLive()
	checked := &cancelAfterValidation{Context: live, cancel: cancelLive, checks: 1}
	if _, err := RunTTY(checked, "unused", nil, nil, "", file, file, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// Even a failed restoration must preserve the calling thread's prior mask,
	// including SIGTTOU already blocked. No process-wide ignore is installed.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var block, original, after unix.Sigset_t
	block.Val[0] = 1 << (unix.SIGTTOU - 1)
	if err := unix.PthreadSigmask(unix.SIG_BLOCK, &block, &original); err != nil {
		t.Fatal(err)
	}
	defer unix.PthreadSigmask(unix.SIG_SETMASK, &original, nil)
	if err := restoreTTY(-1, unix.Getpgrp(), new(unix.Termios)); !errors.Is(err, ErrTerminal) {
		t.Fatal(err)
	}
	if err := unix.PthreadSigmask(unix.SIG_BLOCK, nil, &after); err != nil {
		t.Fatal(err)
	}
	expected := original
	expected.Val[0] |= block.Val[0]
	if after != expected {
		t.Fatal("calling thread signal mask changed")
	}
}
