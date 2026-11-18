package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

var fixtureRequest = []byte(`{"protocol":1,"operation_id":"op","action":"fixture"}`)

func childArgs(mode string) []string {
	return []string{"-test.run=^TestBridgeProcessChild$", "--", "taletty-bridge-child", mode}
}

func processContext(t *testing.T) context.Context {
	t.Helper()
	// Race helper subprocesses need no one-second exit delay. Parent stays race-enabled.
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestProcessRoundTripAndReaping(t *testing.T) {
	ctx := processContext(t)
	frames := make(chan Frame, 2)
	terminal, err := Run(ctx, os.Args[0], childArgs("valid"), fixtureRequest, "op", frames)
	if err != nil || terminal.Type != Result || len(frames) != 2 {
		t.Fatal("round trip failed", terminal, err)
	}
	stage := <-frames
	var info struct{ PID, Group, Bytes int }
	if err := json.Unmarshal(stage.Raw, &info); err != nil || info.PID < 1 || info.Group != info.PID || info.Bytes != len(fixtureRequest)+1 {
		t.Fatal("group/request/EOF mismatch", info, err)
	}
	if (<-frames).Type != Progress {
		t.Fatal("progress lost")
	}
	assertReaped(t, info.PID)
}

func TestExactRequestLimitIsDelivered(t *testing.T) {
	ctx := processContext(t)
	prefix := strings.TrimSuffix(string(fixtureRequest), "}") + `,"padding":"`
	request := []byte(prefix + strings.Repeat("x", MaxFrameBytes-len(prefix)-2) + `"}`)
	frames := make(chan Frame, 2)
	terminal, err := Run(ctx, os.Args[0], childArgs("valid"), request, "op", frames)
	if err != nil || terminal.Type != Result {
		t.Fatal("exact request limit rejected", err)
	}
	var info struct{ Bytes int }
	if err := json.Unmarshal((<-frames).Raw, &info); err != nil || info.Bytes != MaxFrameBytes+1 {
		t.Fatal(info, err)
	}
}

func assertReaped(t *testing.T, pid int) {
	t.Helper()
	var status unix.WaitStatus
	if _, err := unix.Wait4(pid, &status, unix.WNOHANG, nil); err != unix.ECHILD {
		t.Fatal("owned leader not reaped", pid, err)
	}
}

func TestProcessFailuresAreSafe(t *testing.T) {
	ctx := processContext(t)
	for mode, want := range map[string]error{"bad": ErrProtocol, "missing": ErrProtocol, "nonzero": ErrProcess} {
		t.Run(mode, func(t *testing.T) {
			frames := make(chan Frame, 2)
			frame, err := Run(ctx, os.Args[0], childArgs(mode), fixtureRequest, "op", frames)
			if err != want || frame.Raw != nil || len(frames) != 1 {
				t.Fatal("unsafe failure/partial terminal", err, frame)
			}
			var info struct{ PID int }
			if err := json.Unmarshal((<-frames).Raw, &info); err != nil || info.PID < 1 {
				t.Fatal(info, err)
			}
			assertReaped(t, info.PID)
		})
	}
	// A valid error envelope is a transport result, NOT public operation success.
	frame, err := Run(ctx, os.Args[0], childArgs("error"), fixtureRequest, "op", nil)
	if err != nil || frame.Type != Failure {
		t.Fatal(frame, err)
	}
	frame, err = Run(ctx, "/nonexistent/taletty-fixture-private-argv", []string{"fixture-private"}, fixtureRequest, "op", nil)
	if err != ErrStart || frame.Raw != nil {
		t.Fatal("unsafe start failure", err)
	}
}

func TestProcessPreflightStartsNothing(t *testing.T) {
	ctx := processContext(t)
	for _, request := range [][]byte{nil, []byte("\xff"), []byte("{}"), []byte(`{"protocol":2,"operation_id":"op"}`), []byte(`{"protocol":1,"operation_id":"other"}`), []byte(strings.Repeat("x", MaxFrameBytes+1))} {
		if _, err := Run(ctx, "/nonexistent", nil, request, "op", nil); err != ErrProtocol {
			t.Fatal(err)
		}
	}
	if _, err := Run(ctx, "/nonexistent", nil, fixtureRequest, "", nil); err != ErrProtocol {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Run(canceled, "/nonexistent", nil, fixtureRequest, "op", nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type cancelAfterValidation struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *cancelAfterValidation) Err() error {
	c.checks++
	if c.checks == 2 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestCancellationDuringPreflightDoesNotStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checked := &cancelAfterValidation{Context: ctx, cancel: cancel}
	if _, err := Run(checked, "/nonexistent", nil, fixtureRequest, "op", nil); err != context.Canceled {
		t.Fatal(err)
	}
}

type processOutcome struct {
	frame Frame
	err   error
}

func startFixture(ctx context.Context, mode string, request []byte, frames chan<- Frame, timing processTiming) <-chan processOutcome {
	done := make(chan processOutcome, 1)
	go func() {
		frame, err := runProcess(ctx, os.Args[0], childArgs(mode), request, "op", frames, timing, observeExit)
		done <- processOutcome{frame, err}
	}()
	return done
}

func readyFrame(t *testing.T, ctx context.Context, frames <-chan Frame) int {
	t.Helper()
	select {
	case frame := <-frames:
		var info struct{ PID int }
		if err := json.Unmarshal(frame.Raw, &info); err != nil || info.PID < 1 {
			t.Fatal("missing ready PID", err)
		}
		return info.PID
	case <-ctx.Done():
		t.Fatal("no ready frame", ctx.Err())
	}
	return 0
}

func awaitOutcome(t *testing.T, ctx context.Context, done <-chan processOutcome) processOutcome {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-ctx.Done():
		t.Fatal("transport did not return", ctx.Err())
	}
	return processOutcome{}
}

func TestProcessCancellationInterruptsIOAndBackpressure(t *testing.T) {
	for _, mode := range []string{"blocked-stdin", "closed-stdout", "blocked-consumer"} {
		t.Run(mode, func(t *testing.T) {
			bound := processContext(t)
			ctx, cancel := context.WithCancel(bound)
			defer cancel()
			frames := make(chan Frame) // Deliberately unbuffered; consume only readiness.
			request := fixtureRequest
			if mode == "blocked-stdin" {
				request = []byte(strings.TrimSuffix(string(fixtureRequest), "}") + `,"padding":"` + strings.Repeat("x", 1<<20) + `"}`)
			}
			done := startFixture(ctx, mode, request, frames, processTiming{20 * time.Millisecond, 100 * time.Millisecond})
			pid := readyFrame(t, bound, frames)
			select {
			case <-done:
				t.Fatal("EOF or provisional frame accepted before exit")
			default:
			}
			cancel()
			result := awaitOutcome(t, bound, done)
			if result.err != context.Canceled || result.frame.Raw != nil {
				t.Fatal(result)
			}
			assertReaped(t, pid)
		})
	}
}

func TestProcessKillsRetainedPipeGroupBeforeReapingLeader(t *testing.T) {
	ctx := processContext(t)
	frames := make(chan Frame, 1)
	done := startFixture(ctx, "exit-on-signal", fixtureRequest, frames, processTiming{20 * time.Millisecond, 100 * time.Millisecond})
	leader := readyFrame(t, ctx, frames)
	// Attach a separately test-owned child to the SAME group and inherited stdout.
	// This models ffmpeg retaining a writer after the bridge exits, while allowing
	// the test to reap that non-Run child itself (no orphan/zombie fixture leaks).
	writer, err := os.OpenFile(fmt.Sprintf("/proc/%d/fd/1", leader), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	ready, ack, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	defer ack.Close()
	child := exec.CommandContext(ctx, os.Args[0], childArgs("group-member")...)
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader}
	child.Stdout, child.ExtraFiles = writer, []*os.File{ack}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.Process.Kill(); child.Wait() })
	ack.Close()
	writer.Close()
	if err := ready.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := io.ReadFull(ready, b[:]); err != nil {
		t.Fatal("member not ready", err)
	}
	if err := unix.Kill(leader, unix.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	result := awaitOutcome(t, ctx, done)
	if result.err != ErrProcess || result.frame.Raw != nil {
		t.Fatal("inherited stdout accepted", result)
	}
	if err := child.Wait(); err == nil || child.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatal("residual group member not killed", err)
	}
	assertReaped(t, leader)
}

type handoffContext struct {
	context.Context
	waiting chan struct{}
}

func (c handoffContext) Done() <-chan struct{} {
	close(c.waiting) // sendFrame evaluates Done once, while entering its select.
	return c.Context.Done()
}

func TestFrameHandoffCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := handoffContext{ctx, make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- sendFrame(observed, make(chan Frame), Frame{}) }()
	<-observed.waiting
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatal(err)
	}
	// Even a ready receiver must not get a frame from an already canceled call.
	ready := make(chan Frame, 1)
	if err := sendFrame(ctx, ready, Frame{}); err != context.Canceled || len(ready) != 0 {
		t.Fatal(err)
	}
}

func TestFailedExitObservationStillJoinsOwnedChild(t *testing.T) {
	ctx := processContext(t)
	ownedPID := make(chan int, 1)
	frame, err := runProcess(ctx, os.Args[0], childArgs("valid"), fixtureRequest, "op", nil,
		processTiming{20 * time.Millisecond, 100 * time.Millisecond}, func(pid int) error {
			ownedPID <- pid // Do not depend on provisional delivery before abort.
			// Preserve the real unreaped PID anchor, then inject an inspection failure.
			if err := observeExit(pid); err != nil {
				return err
			}
			return unix.EIO
		})
	if err != ErrProcess || frame.Raw != nil {
		t.Fatal(frame, err)
	}
	assertReaped(t, <-ownedPID)
}

// The exact argv marker avoids running a helper from an inherited environment.
// No provider/runtime/media fixture, shell, external account or real data root.
func TestBridgeProcessChild(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "taletty-bridge-child" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	if mode == "group-member" {
		signal.Ignore(syscall.SIGTERM)
		f := os.NewFile(3, "ready")
		f.Write([]byte{1})
		f.Close()
		for {
			unix.Pause()
		}
	}
	if mode == "blocked-stdin" {
		signal.Ignore(syscall.SIGTERM)
	} else {
		request, err := io.ReadAll(os.Stdin)
		if err != nil || !json.Valid(request) || len(request) < 1 || request[len(request)-1] != '\n' {
			os.Exit(20)
		}
		var header struct {
			OperationID string `json:"operation_id"`
		}
		if json.Unmarshal(request, &header) != nil || header.OperationID != "op" {
			os.Exit(21)
		}
		// stderr cannot deadlock the process or enter user-facing error text.
		io.WriteString(os.Stderr, strings.Repeat("fixture-private-stderr", 10000))
		if mode == "exit-on-signal" {
			sigs := make(chan os.Signal, 1)
			signal.Notify(sigs, syscall.SIGUSR1)
			emitReady(len(request))
			<-sigs
			fmt.Println(wire(2, Result))
			os.Exit(0)
		}
		emitReady(len(request))
	}
	if mode == "blocked-stdin" {
		emitReady(0)
	}
	switch mode {
	case "blocked-stdin":
		for {
			unix.Pause()
		}
	case "closed-stdout":
		fmt.Println(wire(2, Result))
		os.Stdout.Close()
		for {
			unix.Pause()
		}
	case "blocked-consumer":
		fmt.Println(wire(2, Progress))
		for {
			unix.Pause()
		}
	case "bad":
		fmt.Println("fixture-private-not-json")
		for {
			unix.Pause()
		}
	case "missing":
		os.Exit(0)
	case "nonzero":
		fmt.Println(wire(2, Result))
		os.Exit(9)
	case "error":
		fmt.Println(wire(2, Failure))
		os.Exit(0)
	case "valid":
		fmt.Println(wire(2, Progress))
		fmt.Println(wire(3, Result))
		os.Exit(0)
	default:
		os.Exit(22)
	}
}

func emitReady(n int) {
	prefix := strings.TrimSuffix(wire(1, Stage), "}")
	fmt.Println(prefix + `,"PID":` + strconv.Itoa(os.Getpid()) + `,"Group":` + strconv.Itoa(unix.Getpgrp()) + `,"Bytes":` + strconv.Itoa(n) + `}`)
}
