package playback

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/susamn/taletty/app/internal/store"
	"golang.org/x/sys/unix"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	must(t, os.Chmod(dir, 0o700))
	root, err := os.Open(dir)
	must(t, err)
	t.Cleanup(func() { _ = root.Close() })

	owner, err := store.NewOwner(context.Background(), root, func() error { return nil })
	must(t, err)
	t.Cleanup(func() { must(t, owner.Close(context.Background())) })

	s, err := store.Open(context.Background(), owner)
	must(t, err)
	t.Cleanup(func() { must(t, s.Close(context.Background())) })
	return s
}

func fakeMPVBinary(t *testing.T, mode string, extra ...string) string {
	t.Helper()
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-mpv")
	quotedBin := "'" + strings.ReplaceAll(os.Args[0], "'", "'\\''") + "'"
	var quotedExtra []string
	for _, arg := range extra {
		quotedExtra = append(quotedExtra, "'"+strings.ReplaceAll(arg, "'", "'\\''")+"'")
	}
	body := fmt.Sprintf("#!/bin/sh\nexec %s -test.run=^TestMPVFakeChild$ -- taletty-mpv-child %s %s \"$@\"\n",
		quotedBin, mode, strings.Join(quotedExtra, " "))
	must(t, os.WriteFile(script, []byte(body), 0o700))
	return script
}

func assertReaped(t *testing.T, pid int) {
	t.Helper()
	var status unix.WaitStatus
	if _, err := unix.Wait4(pid, &status, unix.WNOHANG, nil); !errors.Is(err, unix.ECHILD) {
		t.Fatalf("expected child %d to be reaped (ECHILD), got %v", pid, err)
	}
}

func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := unix.Kill(pid, 0); errors.Is(err, unix.ESRCH) {
			return
		}
		if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			fields := strings.Fields(string(stat))
			if len(fields) > 2 && fields[2] == "Z" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d survived after parent/group termination", pid)
}

// TestMPVFakeChild is a disposable fake JSON-IPC child process over fd://3.
func TestMPVFakeChild(t *testing.T) {
	sep := slices.Index(os.Args, "--")
	if sep < 0 || len(os.Args) <= sep+2 || os.Args[sep+1] != "taletty-mpv-child" {
		return
	}
	mode := os.Args[sep+2]
	rest := os.Args[sep+3:]

	if mode == "group-sleep" {
		signal.Ignore(syscall.SIGTERM)
		for {
			unix.Pause()
		}
	}
	if mode == "exit-early" {
		os.Exit(1)
	}

	var groupPIDFile string
	var mpvArgs []string
	for _, arg := range rest {
		if strings.HasPrefix(arg, "--group-pid-file=") {
			groupPIDFile = strings.TrimPrefix(arg, "--group-pid-file=")
			continue
		}
		mpvArgs = append(mpvArgs, arg)
	}

	for _, required := range []string{
		"--no-config",
		"--no-terminal",
		"--no-video",
		"--audio-display=no",
		"--load-scripts=no",
		"--ytdl=no",
		"--idle=yes",
		"--keep-open=yes",
		"--input-ipc-client=fd://3",
	} {
		if !slices.Contains(mpvArgs, required) {
			os.Exit(11)
		}
	}
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		switch key {
		case "TALETTY_TEST_SECRET", "AWS_SECRET_ACCESS_KEY", "HTTP_PROXY", "LD_PRELOAD":
			os.Exit(12)
		}
	}

	if mode == "hang-quit" {
		signal.Ignore(syscall.SIGTERM)
	}
	if groupPIDFile != "" {
		sub := exec.Command(os.Args[0], "-test.run=^TestMPVFakeChild$", "--", "taletty-mpv-child", "group-sleep")
		if err := sub.Start(); err != nil {
			os.Exit(13)
		}
		if err := os.WriteFile(groupPIDFile, []byte(strconv.Itoa(sub.Process.Pid)), 0o600); err != nil {
			os.Exit(14)
		}
	}

	ipcFile := os.NewFile(3, "ipc")
	if ipcFile == nil {
		os.Exit(15)
	}
	defer ipcFile.Close()

	scanner := bufio.NewScanner(ipcFile)
	scanner.Buffer(make([]byte, 4096), maxIPCLineBytes)
	encoder := json.NewEncoder(ipcFile)

	pos := 0.0
	dur := 120.0
	paused := false
	speed := 1.0
	volume := 100.0
	eof := mode == "eof"
	posCalls := 0

	for _, arg := range mpvArgs {
		switch {
		case arg == "--pause=yes":
			paused = true
		case strings.HasPrefix(arg, "--speed="):
			speed, _ = strconv.ParseFloat(strings.TrimPrefix(arg, "--speed="), 64)
		case strings.HasPrefix(arg, "--volume="):
			volume, _ = strconv.ParseFloat(strings.TrimPrefix(arg, "--volume="), 64)
		}
	}

	for scanner.Scan() {
		var req struct {
			Command   json.RawMessage `json:"command"`
			RequestID uint64          `json:"request_id"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			os.Exit(16)
		}

		reply := map[string]any{
			"request_id": req.RequestID,
			"error":      "success",
		}

		var arr []any
		if err := json.Unmarshal(req.Command, &arr); err == nil && len(arr) > 0 {
			cmdName, _ := arr[0].(string)
			switch cmdName {
			case "get_property":
				prop, _ := arr[1].(string)
				switch prop {
				case "mpv-version":
					reply["data"] = "mpv 0.37.0-fake"
				case "time-pos":
					posCalls++
					switch mode {
					case "delayed-pos":
						if posCalls == 1 {
							reply["error"] = "property unavailable"
						} else if posCalls == 2 {
							reply["data"] = nil
						} else {
							reply["data"] = pos
						}
					case "post-load-error":
						reply["error"] = "property unavailable"
					case "bad-initial-pos":
						reply["data"] = "not-a-number"
					case "bad-pos-prop":
						if posCalls == 1 {
							reply["data"] = pos
						} else {
							reply["data"] = "not-a-number"
						}
					case "overflow-pos":
						if posCalls == 1 {
							reply["data"] = pos
						} else {
							reply["data"] = 1e30
						}
					case "malformed-ipc":
						if posCalls == 1 {
							reply["data"] = pos
						} else {
							_, _ = ipcFile.WriteString("{not-json\n")
							continue
						}
					case "oversized-ipc":
						if posCalls == 1 {
							reply["data"] = pos
						} else {
							_, _ = ipcFile.WriteString(`{"data":"` + strings.Repeat("x", maxIPCLineBytes+1024) + `"}` + "\n")
							continue
						}
					case "cmd-error":
						if posCalls == 1 {
							reply["data"] = pos
						} else {
							reply["error"] = "property unavailable"
						}
					case "eof":
						reply["data"] = dur
					default:
						reply["data"] = pos
					}
				case "duration":
					switch mode {
					case "null-duration":
						reply["data"] = nil
					case "bad-dur-prop":
						reply["data"] = "bad"
					case "cmd-error-dur":
						reply["error"] = "property unavailable"
					default:
						reply["data"] = dur
					}
				case "pause":
					switch mode {
					case "bad-pause-prop":
						reply["data"] = "not-bool"
					case "cmd-error-pause":
						reply["error"] = "property unavailable"
					default:
						reply["data"] = paused
					}
				case "eof-reached":
					switch mode {
					case "bad-eof-prop":
						reply["data"] = "not-bool"
					case "cmd-error-eof":
						reply["error"] = "property unavailable"
					default:
						reply["data"] = eof
					}
				}
			case "set_property":
				if mode == "cmd-error" {
					reply["error"] = "property unavailable"
					break
				}
				prop, _ := arr[1].(string)
				switch prop {
				case "pause":
					paused, _ = arr[2].(bool)
				case "speed":
					speed, _ = arr[2].(float64)
				case "volume":
					volume, _ = arr[2].(float64)
				}
				_ = speed
				_ = volume
			case "seek":
				if mode == "cmd-error" {
					reply["error"] = "property unavailable"
					break
				}
				if len(arr) != 3 || arr[2] != "absolute+exact" {
					reply["error"] = "invalid seek"
					break
				}
				target, _ := arr[1].(float64)
				pos = target
				if pos < dur {
					eof = false
				}
			case "quit":
				if mode == "hang-quit" {
					for {
						unix.Pause()
					}
				}
				_ = encoder.Encode(reply)
				os.Exit(0)
			}
			_ = encoder.Encode(reply)
			continue
		}

		var named map[string]any
		if err := json.Unmarshal(req.Command, &named); err == nil && named["name"] == "loadfile" {
			if mode == "loadfile-cmd-error" {
				reply["error"] = "loadfile rejected"
				_ = encoder.Encode(reply)
				continue
			}
			if named["flags"] != "replace" {
				reply["error"] = "invalid flags"
				_ = encoder.Encode(reply)
				continue
			}
			opts, _ := named["options"].(map[string]any)
			if startStr, ok := opts["start"].(string); ok {
				pos, _ = strconv.ParseFloat(startStr, 64)
			}
			if pauseStr, ok := opts["pause"].(string); ok {
				paused = pauseStr == "yes"
			}
			_ = encoder.Encode(reply)
			switch mode {
			case "hang-start":
				continue
			case "load-error":
				_ = encoder.Encode(map[string]any{"event": "start-file"})
				_ = encoder.Encode(map[string]any{"event": "end-file", "reason": "error", "file_error": "open failed"})
			case "post-load-error":
				_ = encoder.Encode(map[string]any{"event": "start-file"})
				_ = encoder.Encode(map[string]any{"event": "file-loaded"})
				_ = encoder.Encode(map[string]any{"event": "end-file", "reason": "error"})
			default:
				_ = encoder.Encode(map[string]any{"event": "start-file"})
				_ = encoder.Encode(map[string]any{"event": "file-loaded"})
			}
			continue
		}

		reply["error"] = "unknown command"
		_ = encoder.Encode(reply)
	}

	if mode == "hang-quit" {
		for {
			unix.Pause()
		}
	}
}

// TestMPVFakeParentKillProbe starts a fake mpv child and waits to be SIGKILLed.
func TestMPVFakeParentKillProbe(t *testing.T) {
	sep := slices.Index(os.Args, "--")
	if sep < 0 || len(os.Args) <= sep+2 || os.Args[sep+1] != "taletty-fake-parent-probe" {
		return
	}
	bin := os.Args[sep+2]
	media := os.Args[sep+3]
	proc := NewMPVProcess(MPVConfig{Binary: bin, StartTimeout: 3 * time.Second, RPCTimeout: time.Second})
	if err := proc.Start(context.Background(), media, 0, DefaultSpeedMilli, 80, true); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(proc.cmd.Process.Pid)
	select {}
}

func TestMPVProcessValidationAndCleanEnv(t *testing.T) {
	proc := NewMPVProcess(MPVConfig{})
	if proc.cfg.Binary != "mpv" || proc.cfg.StartTimeout != defaultStartTimeout || proc.cfg.RPCTimeout != defaultRPCTimeout || proc.cfg.StopTimeout != defaultStopTimeout {
		t.Fatalf("unexpected defaults: %+v", proc.cfg)
	}

	for _, badPath := range []string{
		"",
		"relative.m4b",
		"/tmp/../book.m4b",
		"/tmp/book.mp3",
		"/tmp/.m4b",
		"/tmp/.hidden.m4b",
		"/tmp/bad\x00.m4b",
		"/tmp/bad\u200b.m4b",
		"/tmp/" + string([]byte{0xff, 0xfe}) + ".m4b",
	} {
		if err := proc.Start(context.Background(), badPath, 0, DefaultSpeedMilli, 50, false); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for %q, got %v", badPath, err)
		}
	}

	if err := proc.Start(nil, "/tmp/book.m4b", 0, DefaultSpeedMilli, 50, false); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for nil ctx, got %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := proc.Start(canceled, "/tmp/book.m4b", 0, DefaultSpeedMilli, 50, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	for _, badSpeed := range []int64{0, -1, MinSpeedMilli - 1, MaxSpeedMilli + 1} {
		if err := proc.Start(context.Background(), "/tmp/book.m4b", 0, badSpeed, 50, false); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for speed %d, got %v", badSpeed, err)
		}
		if err := proc.SetSpeed(context.Background(), badSpeed); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for SetSpeed %d, got %v", badSpeed, err)
		}
	}

	missing := NewMPVProcess(MPVConfig{Binary: "/nonexistent/taletty-missing-mpv"})
	if err := missing.Start(context.Background(), "/tmp/book.m4b", 0, DefaultSpeedMilli, 50, false); !errors.Is(err, ErrRuntimeMissing) {
		t.Fatalf("expected ErrRuntimeMissing, got %v", err)
	}
	dirBin := NewMPVProcess(MPVConfig{Binary: t.TempDir()})
	if err := dirBin.Start(context.Background(), "/tmp/book.m4b", 0, DefaultSpeedMilli, 50, false); !errors.Is(err, ErrIPC) {
		t.Fatalf("expected ErrIPC for directory binary, got %v", err)
	}

	for _, call := range []func() error{
		func() error { return proc.SetPause(context.Background(), true) },
		func() error { return proc.SeekExact(context.Background(), time.Second) },
		func() error { return proc.SetSpeed(context.Background(), DefaultSpeedMilli) },
		func() error { return proc.SetVolume(context.Background(), 50) },
		func() error { _, _, _, _, err := proc.Position(context.Background()); return err },
	} {
		if err := call(); !errors.Is(err, ErrIPC) {
			t.Fatalf("expected ErrIPC when not running, got %v", err)
		}
	}
	if err := proc.SetPause(nil, true); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for nil ctx, got %v", err)
	}
	if err := proc.Stop(nil); err != nil {
		t.Fatalf("expected nil Stop on idle process, got %v", err)
	}

	cleaned := cleanMPVEnv([]string{
		"MALFORMED",
		"=emptykey",
		"PATH=/usr/bin",
		"HOME=/home/user",
		"LC_ALL=C.UTF-8",
		"PULSE_SERVER=unix:/tmp/pulse",
		"PIPEWIRE_RUNTIME_DIR=/run/user/1000",
		"ALSA_CONFIG_PATH=/etc/alsa.conf",
		"GORACE=atexit_sleep_ms=0",
		"TALETTY_SECRET=topsecret",
		"AWS_SECRET_ACCESS_KEY=secret",
		"HTTP_PROXY=http://proxy.local",
		"LD_PRELOAD=/tmp/evil.so",
	})
	want := []string{
		"PATH=/usr/bin",
		"HOME=/home/user",
		"LC_ALL=C.UTF-8",
		"PULSE_SERVER=unix:/tmp/pulse",
		"PIPEWIRE_RUNTIME_DIR=/run/user/1000",
		"ALSA_CONFIG_PATH=/etc/alsa.conf",
		"GORACE=atexit_sleep_ms=0",
	}
	if !slices.Equal(cleaned, want) {
		t.Fatalf("cleanMPVEnv = %v, want %v", cleaned, want)
	}
	if empty := cleanMPVEnv(nil); empty == nil || len(empty) != 0 {
		t.Fatalf("expected non-nil empty slice, got %#v", empty)
	}
}

func TestMPVProcessFakeIPCLifecycleAndFailures(t *testing.T) {
	t.Setenv("TALETTY_TEST_SECRET", "must-not-leak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "must-not-leak")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9999")
	t.Setenv("LD_PRELOAD", "/nonexistent/preload.so")

	media := filepath.Join(t.TempDir(), "book.M4B")
	ctx := context.Background()

	t.Run("normal-and-restart", func(t *testing.T) {
		proc := NewMPVProcess(MPVConfig{
			Binary:       fakeMPVBinary(t, "normal"),
			StartTimeout: 3 * time.Second,
			RPCTimeout:   time.Second,
			StopTimeout:  500 * time.Millisecond,
		})
		must(t, proc.Start(ctx, media, -5*time.Second, 1_250_000, 150, true))
		pid := proc.cmd.Process.Pid

		pos, dur, paused, eof, err := proc.Position(ctx)
		must(t, err)
		if pos != 0 || dur != 120*time.Second || !paused || eof {
			t.Fatalf("unexpected initial position: pos=%v dur=%v paused=%v eof=%v", pos, dur, paused, eof)
		}

		must(t, proc.SeekExact(ctx, -2*time.Second))
		must(t, proc.SeekExact(ctx, 45*time.Second))
		must(t, proc.SetSpeed(ctx, 1_500_000))
		must(t, proc.SetVolume(ctx, -10))
		pos, _, paused, _, err = proc.Position(ctx)
		must(t, err)
		if pos != 45*time.Second || !paused {
			t.Fatalf("paused seek altered pause or pos: pos=%v paused=%v", pos, paused)
		}

		must(t, proc.SetPause(ctx, false))
		_, _, paused, _, err = proc.Position(ctx)
		must(t, err)
		if paused {
			t.Fatal("expected unpaused")
		}

		// Restarting directly stops and reaps the previous child.
		must(t, proc.Start(ctx, media, 10*time.Second, DefaultSpeedMilli, 80, false))
		assertReaped(t, pid)
		pid2 := proc.cmd.Process.Pid
		must(t, proc.Stop(ctx))
		assertReaped(t, pid2)
		must(t, proc.Stop(ctx))
	})

	t.Run("delayed-pos-and-null-duration", func(t *testing.T) {
		proc := NewMPVProcess(MPVConfig{Binary: fakeMPVBinary(t, "delayed-pos"), StartTimeout: 3 * time.Second, RPCTimeout: time.Second})
		must(t, proc.Start(ctx, media, 5*time.Second, DefaultSpeedMilli, 80, true))
		pos, dur, _, _, err := proc.Position(ctx)
		must(t, err)
		if pos != 5*time.Second || dur != 120*time.Second {
			t.Fatalf("unexpected pos/dur: %v / %v", pos, dur)
		}
		must(t, proc.Stop(ctx))

		procNull := NewMPVProcess(MPVConfig{Binary: fakeMPVBinary(t, "null-duration"), StartTimeout: 3 * time.Second, RPCTimeout: time.Second})
		must(t, procNull.Start(ctx, media, 0, DefaultSpeedMilli, 80, false))
		_, dur, _, _, err = procNull.Position(ctx)
		must(t, err)
		if dur != 0 {
			t.Fatalf("expected 0 duration for null, got %v", dur)
		}
		must(t, procNull.Stop(ctx))
	})

	t.Run("startup-failures", func(t *testing.T) {
		for _, tc := range []struct {
			mode    string
			timeout time.Duration
		}{
			{mode: "exit-early", timeout: 2 * time.Second},
			{mode: "loadfile-cmd-error", timeout: 2 * time.Second},
			{mode: "load-error", timeout: 2 * time.Second},
			{mode: "post-load-error", timeout: 2 * time.Second},
			{mode: "bad-initial-pos", timeout: 2 * time.Second},
			{mode: "hang-start", timeout: 80 * time.Millisecond},
		} {
			t.Run(tc.mode, func(t *testing.T) {
				proc := NewMPVProcess(MPVConfig{
					Binary:       fakeMPVBinary(t, tc.mode),
					StartTimeout: tc.timeout,
					RPCTimeout:   tc.timeout,
					StopTimeout:  100 * time.Millisecond,
				})
				if err := proc.Start(ctx, media, 0, DefaultSpeedMilli, 80, false); !errors.Is(err, ErrIPC) {
					t.Fatalf("%s: expected ErrIPC, got %v", tc.mode, err)
				}
			})
		}

		// Caller context canceled while waiting for file-loaded returns context.Canceled.
		proc := NewMPVProcess(MPVConfig{
			Binary:       fakeMPVBinary(t, "hang-start"),
			StartTimeout: 5 * time.Second,
			RPCTimeout:   time.Second,
			StopTimeout:  100 * time.Millisecond,
		})
		cancelCtx, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
		defer cancel()
		if err := proc.Start(cancelCtx, media, 0, DefaultSpeedMilli, 80, false); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected DeadlineExceeded on caller timeout, got %v", err)
		}

		// Concurrent Stop while Start is waiting on loadCh unblocks Start cleanly.
		proc2 := NewMPVProcess(MPVConfig{
			Binary:       fakeMPVBinary(t, "hang-start"),
			StartTimeout: 5 * time.Second,
			RPCTimeout:   time.Second,
			StopTimeout:  100 * time.Millisecond,
		})
		startErr := make(chan error, 1)
		go func() {
			startErr <- proc2.Start(ctx, media, 0, DefaultSpeedMilli, 80, false)
		}()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			proc2.mu.Lock()
			ready := proc2.client != nil
			proc2.mu.Unlock()
			if ready {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		must(t, proc2.Stop(ctx))
		if err := <-startErr; !errors.Is(err, ErrIPC) {
			t.Fatalf("expected ErrIPC when Stop interrupts Start, got %v", err)
		}

		// Restarting with a canceled context fails in p.Stop(ctx) while still reaping old child.
		proc3 := NewMPVProcess(MPVConfig{
			Binary:       fakeMPVBinary(t, "normal"),
			StartTimeout: 3 * time.Second,
			RPCTimeout:   time.Second,
			StopTimeout:  100 * time.Millisecond,
		})
		must(t, proc3.Start(ctx, media, 0, DefaultSpeedMilli, 80, false))
		pid3 := proc3.cmd.Process.Pid
		cctx, ccancel := context.WithCancel(ctx)
		ccancel()
		if err := proc3.Start(cctx, media, 0, DefaultSpeedMilli, 80, false); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
		_ = proc3.Stop(ctx)
		assertReaped(t, pid3)
	})

	t.Run("position-and-ipc-failures", func(t *testing.T) {
		for _, mode := range []string{"bad-pos-prop", "overflow-pos", "bad-dur-prop", "bad-pause-prop", "bad-eof-prop", "malformed-ipc", "oversized-ipc", "cmd-error", "cmd-error-dur", "cmd-error-pause", "cmd-error-eof"} {
			t.Run(mode, func(t *testing.T) {
				proc := NewMPVProcess(MPVConfig{Binary: fakeMPVBinary(t, mode), StartTimeout: 3 * time.Second, RPCTimeout: time.Second})
				must(t, proc.Start(ctx, media, 0, DefaultSpeedMilli, 80, false))
				defer proc.Stop(ctx)
				if _, _, _, _, err := proc.Position(ctx); !errors.Is(err, ErrIPC) {
					t.Fatalf("%s: expected ErrIPC, got %v", mode, err)
				}
				if mode == "cmd-error" {
					err := proc.SetPause(ctx, true)
					if !errors.Is(err, ErrIPC) || err.Error() != "mpv ipc failure: command failed" {
						t.Fatalf("expected sanitized ErrIPC on SetPause, got %v", err)
					}
				}
			})
		}
	})

	t.Run("hang-quit-kills-process-group-and-reaps", func(t *testing.T) {
		groupPIDFile := filepath.Join(t.TempDir(), "group.pid")
		proc := NewMPVProcess(MPVConfig{
			Binary:       fakeMPVBinary(t, "hang-quit", "--group-pid-file="+groupPIDFile),
			StartTimeout: 3 * time.Second,
			RPCTimeout:   time.Second,
			StopTimeout:  80 * time.Millisecond,
		})
		must(t, proc.Start(ctx, media, 0, DefaultSpeedMilli, 80, false))
		leaderPID := proc.cmd.Process.Pid
		rawPID, err := os.ReadFile(groupPIDFile)
		must(t, err)
		groupPID, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
		must(t, err)
		defer unix.Kill(groupPID, unix.SIGKILL)

		must(t, proc.Stop(ctx))
		assertReaped(t, leaderPID)
		assertProcessGone(t, groupPID)
	})

	t.Run("stop-with-canceled-context-still-kills-and-reaps", func(t *testing.T) {
		proc := NewMPVProcess(MPVConfig{
			Binary:       fakeMPVBinary(t, "hang-quit"),
			StartTimeout: 3 * time.Second,
			RPCTimeout:   time.Second,
			StopTimeout:  5 * time.Second,
		})
		must(t, proc.Start(ctx, media, 0, DefaultSpeedMilli, 80, false))
		pid := proc.cmd.Process.Pid
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := proc.Stop(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
		assertReaped(t, pid)
	})

	t.Run("killed-parent-closes-fd3-and-child-exits", func(t *testing.T) {
		bin := fakeMPVBinary(t, "normal")
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		probe := exec.CommandContext(probeCtx, os.Args[0], "-test.run=^TestMPVFakeParentKillProbe$", "--", "taletty-fake-parent-probe", bin, media)
		probe.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0")
		stdout, err := probe.StdoutPipe()
		must(t, err)
		must(t, probe.Start())
		defer func() {
			if probe.Process != nil {
				_ = probe.Process.Kill()
			}
			_ = probe.Wait()
		}()

		scanner := bufio.NewScanner(stdout)
		if !scanner.Scan() {
			t.Fatal("probe did not report child PID")
		}
		childPID, err := strconv.Atoi(strings.TrimSpace(scanner.Text()))
		must(t, err)
		defer unix.Kill(childPID, unix.SIGKILL)

		must(t, probe.Process.Kill())
		_ = probe.Wait()
		assertProcessGone(t, childPID)
	})
}

func TestIPCClientEdgeCases(t *testing.T) {
	t.Run("out-of-order-and-unsolicited-events", func(t *testing.T) {
		local, remote := net.Pipe()
		client := newIPCClient(local)
		defer func() {
			_ = remote.Close()
			client.close()
			client.close() // idempotent
		}()

		const count = 8
		go func() {
			dec := json.NewDecoder(remote)
			enc := json.NewEncoder(remote)
			var reqs []map[string]any
			for range count {
				var req map[string]any
				if dec.Decode(&req) != nil {
					return
				}
				reqs = append(reqs, req)
			}
			_ = enc.Encode(map[string]any{"event": "property-change", "name": "time-pos"})
			for i := len(reqs) - 1; i >= 0; i-- {
				_ = enc.Encode(map[string]any{
					"request_id": reqs[i]["request_id"],
					"error":      "success",
					"data":       reqs[i]["command"],
				})
			}
		}()

		var wg sync.WaitGroup
		for i := range count {
			wg.Go(func() {
				data, err := client.call(context.Background(), time.Second, i)
				if err != nil || string(data) != fmt.Sprint(i) {
					t.Errorf("call %d = %s, %v", i, data, err)
				}
			})
		}
		wg.Wait()
	})

	t.Run("timeout-cancel-write-fail-and-decode", func(t *testing.T) {
		local, remote := net.Pipe()
		client := newIPCClient(local)
		defer func() {
			_ = remote.Close()
			client.close()
		}()

		// Blocked write times out cleanly, and concurrent caller waiting on writeGate times out too.
		var wg sync.WaitGroup
		wg.Go(func() {
			_, _ = client.call(context.Background(), 80*time.Millisecond, "blocked-holder")
		})
		time.Sleep(10 * time.Millisecond)
		if _, err := client.call(context.Background(), 20*time.Millisecond, "blocked-waiter"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected DeadlineExceeded waiting on writeGate, got %v", err)
		}
		wg.Wait()

		// Unmarshalable command fails encoding.
		if _, err := client.call(context.Background(), time.Second, make(chan int)); !errors.Is(err, ErrIPC) {
			t.Fatalf("expected ErrIPC on encode failure, got %v", err)
		}

		// Canceled context fails immediately.
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := client.call(canceled, time.Second, "canceled"); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}

		// Armed loadCh receives error when remote closes, and loadErr is reported on subsequent armLoadWait.
		loadCh := client.armLoadWait()
		_ = remote.Close()
		if err := <-loadCh; !errors.Is(err, ErrIPC) {
			t.Fatalf("expected ErrIPC on armed loadCh after remote close, got %v", err)
		}
		if _, err := client.call(context.Background(), time.Second, "after-close"); !errors.Is(err, ErrIPC) {
			t.Fatalf("expected ErrIPC after remote close, got %v", err)
		}
		if ch := client.armLoadWait(); !errors.Is(<-ch, ErrIPC) {
			t.Fatal("expected armLoadWait to report existing client error")
		}

		// Direct write failure on closed net.Pipe without waiting for readLoop.
		local2, remote2 := net.Pipe()
		client2 := newIPCClient(local2)
		_ = local2.Close()
		_ = remote2.Close()
		if _, err := client2.call(context.Background(), time.Second, "write-fail"); !errors.Is(err, ErrIPC) {
			t.Fatalf("expected ErrIPC on closed local write, got %v", err)
		}
		client2.close()

		// handleEvent end-file error after file-loaded sets loadErr.
		local3, remote3 := net.Pipe()
		client3 := newIPCClient(local3)
		client3.handleEvent("end-file", "error")
		if ch := client3.armLoadWait(); !errors.Is(<-ch, ErrIPC) {
			t.Fatal("expected armLoadWait to report loadErr")
		}
		_ = remote3.Close()
		client3.close()
	})

	t.Run("decode-seconds-bounds", func(t *testing.T) {
		for _, raw := range []string{`"bad"`, `-1`, `1e30`} {
			if _, err := decodeSeconds(json.RawMessage(raw)); !errors.Is(err, ErrIPC) {
				t.Fatalf("expected ErrIPC for %s, got %v", raw, err)
			}
		}
		if _, err := decodeSeconds(json.RawMessage(fmt.Sprint(math.MaxFloat64))); !errors.Is(err, ErrIPC) {
			t.Fatal("expected overflow error")
		}
		if d, err := decodeOptionalSeconds(json.RawMessage("null")); err != nil || d != 0 {
			t.Fatalf("expected 0 for null, got %v, %v", d, err)
		}
	})
}

type faultStore struct {
	inner  CheckpointStore
	getErr error
	putErr error
}

func (f *faultStore) ListeningState(ctx context.Context, key store.BookKey) (store.ListeningState, bool, error) {
	if f.getErr != nil {
		return store.ListeningState{}, false, f.getErr
	}
	return f.inner.ListeningState(ctx, key)
}

func (f *faultStore) PutListeningState(ctx context.Context, state store.ListeningState) error {
	if f.putErr != nil {
		return f.putErr
	}
	return f.inner.PutListeningState(ctx, state)
}

type mockBackend struct {
	mu       sync.Mutex
	startErr error
	pauseErr error
	seekErr  error
	speedErr error
	volErr   error
	posErr   error
	stopErr  error

	pos    time.Duration
	dur    time.Duration
	paused bool
	eof    bool
	stops  int
}

func (m *mockBackend) Start(_ context.Context, _ string, startPos time.Duration, _ int64, _ int, paused bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.startErr != nil {
		return m.startErr
	}
	m.pos = startPos
	m.paused = paused
	return nil
}

func (m *mockBackend) SetPause(_ context.Context, paused bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pauseErr != nil {
		return m.pauseErr
	}
	m.paused = paused
	return nil
}

func (m *mockBackend) SeekExact(_ context.Context, target time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seekErr != nil {
		return m.seekErr
	}
	m.pos = target
	return nil
}

func (m *mockBackend) SetSpeed(_ context.Context, _ int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.speedErr
}

func (m *mockBackend) SetVolume(_ context.Context, _ int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.volErr
}

func (m *mockBackend) Position(_ context.Context) (time.Duration, time.Duration, bool, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.posErr != nil {
		return 0, 0, false, false, m.posErr
	}
	return m.pos, m.dur, m.paused, m.eof, nil
}

func (m *mockBackend) Stop(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stops++
	return m.stopErr
}

func TestControllerWithRealStoreAndFakeIPC(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1_700_000_000_000)
	clock := func() time.Time {
		now = now.Add(time.Second)
		return now
	}

	bookA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	bookB := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	mediaA := filepath.Join(t.TempDir(), "bookA.m4b")
	mediaB := filepath.Join(t.TempDir(), "bookB.m4b")

	// Seed saved ListeningState for bookA (including Finished completion and out-of-range speed).
	must(t, s.PutListeningState(ctx, store.ListeningState{
		Key:         bookA,
		PositionNS:  int64(30 * time.Second),
		SpeedMilli:  4_500_000, // clamped to MaxSpeedMilli (3_000_000)
		Completion:  store.CompletionFinished,
		UpdatedAtMS: 1000,
	}))

	var snapshots []Snapshot
	var snapMu sync.Mutex
	initVol := 120 // clamped to 100
	proc := NewMPVProcess(MPVConfig{
		Binary:       fakeMPVBinary(t, "normal"),
		StartTimeout: 3 * time.Second,
		RPCTimeout:   time.Second,
		StopTimeout:  500 * time.Millisecond,
	})
	ctrl, err := NewController(proc, Options{
		Store:  s,
		Clock:  clock,
		Volume: &initVol,
		OnChange: func(snap Snapshot) {
			snapMu.Lock()
			snapshots = append(snapshots, snap)
			snapMu.Unlock()
		},
	})
	must(t, err)
	defer func() { _ = ctrl.Close(context.Background()) }()

	if snap := ctrl.Snapshot(); snap.State != StateIdle || snap.VolumePercent != 100 || snap.SpeedMilli != DefaultSpeedMilli {
		t.Fatalf("unexpected initial snapshot: %+v", snap)
	}

	// Idle volume adjustment persists in controller and applies on Load.
	snap, err := ctrl.StepVolume(ctx, -25)
	must(t, err)
	if snap.VolumePercent != 75 {
		t.Fatalf("expected volume 75, got %d", snap.VolumePercent)
	}

	// Load bookA paused: restores 30s position, clamped 3.0x speed, preserves CompletionFinished.
	snap, err = ctrl.Load(ctx, Track{Key: bookA, Title: "Book A", Path: mediaA, Duration: 120 * time.Second}, false)
	must(t, err)
	if snap.State != StatePaused || snap.Session != 1 || snap.Position != 30*time.Second || snap.SpeedMilli != MaxSpeedMilli || snap.VolumePercent != 75 {
		t.Fatalf("unexpected loaded snapshot: %+v", snap)
	}

	// Paused exact seek preserves StatePaused and persists checkpoint.
	snap, err = ctrl.SeekRelative(ctx, snap.Session, 15*time.Second)
	must(t, err)
	if snap.State != StatePaused || snap.Position != 45*time.Second {
		t.Fatalf("paused relative seek failed: %+v", snap)
	}
	snap, err = ctrl.SeekAbsolute(ctx, snap.Session, 200*time.Second)
	must(t, err)
	if snap.State != StatePaused || snap.Position != 120*time.Second {
		t.Fatalf("paused absolute seek clamp failed: %+v", snap)
	}
	snap, err = ctrl.SeekAbsolute(ctx, snap.Session, 50*time.Second)
	must(t, err)

	// StepSpeed / SetSpeed clamp and persist.
	snap, err = ctrl.StepSpeed(ctx, snap.Session, -1_500_000)
	must(t, err)
	if snap.SpeedMilli != 1_500_000 {
		t.Fatalf("expected speed 1_500_000, got %d", snap.SpeedMilli)
	}
	snap, err = ctrl.SetSpeed(ctx, snap.Session, 100_000)
	must(t, err)
	if snap.SpeedMilli != MinSpeedMilli {
		t.Fatalf("expected clamped MinSpeedMilli, got %d", snap.SpeedMilli)
	}

	// Toggle / Play / Pause.
	snap, err = ctrl.Toggle(ctx, snap.Session)
	must(t, err)
	if snap.State != StatePlaying {
		t.Fatalf("expected StatePlaying after Toggle, got %v", snap.State)
	}
	snap, err = ctrl.Toggle(ctx, snap.Session)
	must(t, err)
	if snap.State != StatePaused {
		t.Fatalf("expected StatePaused after second Toggle, got %v", snap.State)
	}
	snap, err = ctrl.Play(ctx, snap.Session)
	must(t, err)
	if snap.State != StatePlaying {
		t.Fatalf("expected StatePlaying after Play, got %v", snap.State)
	}
	snap, err = ctrl.SetVolume(ctx, 60)
	must(t, err)
	if snap.VolumePercent != 60 {
		t.Fatalf("expected volume 60, got %d", snap.VolumePercent)
	}
	snap, err = ctrl.Poll(ctx)
	must(t, err)
	if snap.State != StatePlaying || snap.Position != 50*time.Second {
		t.Fatalf("unexpected Poll snapshot: %+v", snap)
	}

	// Switch to bookB: saves bookA final checkpoint (preserving CompletionFinished), increments Session.
	oldSession := snap.Session
	snap, err = ctrl.Load(ctx, Track{Key: bookB, Title: "Book B", Path: mediaB, Duration: 90 * time.Second}, true)
	must(t, err)
	if snap.Session != oldSession+1 || snap.Book == nil || *snap.Book != bookB || snap.State != StatePlaying || snap.Position != 0 || snap.SpeedMilli != DefaultSpeedMilli {
		t.Fatalf("unexpected bookB snapshot: %+v", snap)
	}

	savedA, found, err := s.ListeningState(ctx, bookA)
	must(t, err)
	if !found || savedA.PositionNS != int64(50*time.Second) || savedA.SpeedMilli != MinSpeedMilli || savedA.Completion != store.CompletionFinished {
		t.Fatalf("bookA checkpoint mismatch: found=%v state=%+v", found, savedA)
	}

	// Stale session rejected across all transport methods.
	for _, call := range []func() error{
		func() error { _, e := ctrl.Toggle(ctx, oldSession); return e },
		func() error { _, e := ctrl.Pause(ctx, oldSession); return e },
		func() error { _, e := ctrl.Play(ctx, oldSession); return e },
		func() error { _, e := ctrl.SeekRelative(ctx, oldSession, time.Second); return e },
		func() error { _, e := ctrl.SeekAbsolute(ctx, oldSession, time.Second); return e },
		func() error { _, e := ctrl.StepSpeed(ctx, oldSession, SpeedStepMilli); return e },
		func() error { _, e := ctrl.SetSpeed(ctx, oldSession, DefaultSpeedMilli); return e },
	} {
		if err := call(); !errors.Is(err, ErrStaleSession) {
			t.Fatalf("expected ErrStaleSession, got %v", err)
		}
	}

	// Pause and Stop persist final checkpoint and reset state.
	snap, err = ctrl.Pause(ctx, snap.Session)
	must(t, err)
	snap, err = ctrl.Stop(ctx)
	must(t, err)
	if snap.State != StateStopped || snap.Book != nil {
		t.Fatalf("expected StateStopped with nil book, got %+v", snap)
	}
	if _, err := ctrl.Play(ctx, 0); !errors.Is(err, ErrNoActiveTrack) {
		t.Fatalf("expected ErrNoActiveTrack when stopped, got %v", err)
	}

	// Reload bookB and Close cleanly (idempotent).
	snap, err = ctrl.Load(ctx, Track{Key: bookB, Title: "Book B", Path: mediaB, Duration: 90 * time.Second}, false)
	must(t, err)
	must(t, ctrl.Close(ctx))
	must(t, ctrl.Close(ctx))

	for _, call := range []func() error{
		func() error {
			_, e := ctrl.Load(ctx, Track{Key: bookB, Title: "Book B", Path: mediaB}, false)
			return e
		},
		func() error { _, e := ctrl.Toggle(ctx, 0); return e },
		func() error { _, e := ctrl.Pause(ctx, 0); return e },
		func() error { _, e := ctrl.Play(ctx, 0); return e },
		func() error { _, e := ctrl.SeekRelative(ctx, 0, time.Second); return e },
		func() error { _, e := ctrl.SeekAbsolute(ctx, 0, time.Second); return e },
		func() error { _, e := ctrl.StepSpeed(ctx, 0, SpeedStepMilli); return e },
		func() error { _, e := ctrl.SetSpeed(ctx, 0, DefaultSpeedMilli); return e },
		func() error { _, e := ctrl.StepVolume(ctx, 5); return e },
		func() error { _, e := ctrl.SetVolume(ctx, 50); return e },
		func() error { _, e := ctrl.Poll(ctx); return e },
		func() error { _, e := ctrl.Stop(ctx); return e },
	} {
		if err := call(); !errors.Is(err, ErrClosed) {
			t.Fatalf("expected ErrClosed after Close, got %v", err)
		}
	}

	snapMu.Lock()
	if len(snapshots) == 0 {
		t.Fatal("expected OnChange snapshots")
	}
	snapMu.Unlock()
}

func TestControllerEOFAndEdgeCases(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	book := store.BookKey{Provider: "local", Namespace: "user", ID: "book-1"}
	media := filepath.Join(t.TempDir(), "book.m4b")

	if _, err := NewController(nil, Options{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("expected ErrInvalidInput for nil backend")
	}

	for i, sVal := range []State{StateIdle, StateLoading, StatePlaying, StatePaused, StateStopped, StateFailed, State(99)} {
		if sVal.String() == "" || (i == 6 && sVal.String() != "unknown") {
			t.Fatalf("unexpected State.String() for %d: %q", sVal, sVal.String())
		}
	}

	for _, badTrack := range []Track{
		{Key: store.BookKey{}, Title: "Ok", Path: media},
		{Key: book, Title: "   ", Path: media},
		{Key: book, Title: "Bad\nTitle", Path: media},
		{Key: book, Title: string([]byte{0xff, 0xfe}), Path: media},
		{Key: book, Title: "Ok", Path: "relative.m4b"},
		{Key: book, Title: "Ok", Path: media, Duration: -1},
	} {
		if err := badTrack.Validate(); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for %+v, got %v", badTrack, err)
		}
	}

	// Controller with nil Store operates cleanly without persistence.
	nilStoreCtrl, err := NewController(&mockBackend{dur: 30 * time.Second}, Options{})
	must(t, err)
	snapNil, err := nilStoreCtrl.Load(ctx, Track{Key: book, Title: "NoStore", Path: media, Duration: 30 * time.Second}, false)
	must(t, err)
	snapNil, err = nilStoreCtrl.SeekAbsolute(ctx, snapNil.Session, -5*time.Second)
	must(t, err)
	if snapNil.Position != 0 {
		t.Fatalf("expected clamped 0 position, got %v", snapNil.Position)
	}
	must(t, nilStoreCtrl.Close(ctx))

	// EOF via fake IPC child transitions to StatePaused and persists checkpoint at duration.
	proc := NewMPVProcess(MPVConfig{Binary: fakeMPVBinary(t, "eof"), StartTimeout: 3 * time.Second, RPCTimeout: time.Second})
	ctrl, err := NewController(proc, Options{Store: s})
	must(t, err)
	defer func() { _ = ctrl.Close(ctx) }()

	// Poll while idle is a no-op.
	snap, err := ctrl.Poll(ctx)
	must(t, err)
	if snap.State != StateIdle {
		t.Fatalf("expected StateIdle, got %v", snap.State)
	}

	_, err = ctrl.Load(ctx, Track{Key: book, Title: "Book", Path: media, Duration: 120 * time.Second}, true)
	must(t, err)
	snap, err = ctrl.Poll(ctx)
	must(t, err)
	if snap.State != StatePaused || snap.Position != 120*time.Second {
		t.Fatalf("expected paused at EOF duration, got %+v", snap)
	}
	saved, found, err := s.ListeningState(ctx, book)
	must(t, err)
	if !found || saved.PositionNS != int64(120*time.Second) {
		t.Fatalf("expected EOF checkpoint saved, got found=%v %+v", found, saved)
	}
}

func TestControllerErrorPathsAndStoreFaults(t *testing.T) {
	ctx := context.Background()
	realStore := openTestStore(t)
	bookA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	bookB := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	mediaA := filepath.Join(t.TempDir(), "a.m4b")
	mediaB := filepath.Join(t.TempDir(), "b.m4b")

	// Seed negative/oversized saved position to verify clamping on Load.
	must(t, realStore.PutListeningState(ctx, store.ListeningState{
		Key:         bookA,
		PositionNS:  int64(500 * time.Second),
		SpeedMilli:  DefaultSpeedMilli,
		Completion:  store.CompletionUnfinished,
		UpdatedAtMS: 100,
	}))

	fs := &faultStore{inner: realStore}
	mb := &mockBackend{dur: 60 * time.Second}
	ctrl, err := NewController(mb, Options{Store: fs})
	must(t, err)
	defer func() { _ = ctrl.Close(ctx) }()

	// Nil/canceled context and invalid inputs.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, call := range []func() error{
		func() error { _, e := ctrl.Load(nil, Track{Key: bookA, Title: "A", Path: mediaA}, true); return e },
		func() error { _, e := ctrl.Load(ctx, Track{}, true); return e },
		func() error { _, e := ctrl.Load(canceled, Track{Key: bookA, Title: "A", Path: mediaA}, true); return e },
		func() error { _, e := ctrl.Toggle(nil, 0); return e },
		func() error { _, e := ctrl.Toggle(canceled, 0); return e },
		func() error { _, e := ctrl.Pause(nil, 0); return e },
		func() error { _, e := ctrl.Pause(canceled, 0); return e },
		func() error { _, e := ctrl.Play(nil, 0); return e },
		func() error { _, e := ctrl.Play(canceled, 0); return e },
		func() error { _, e := ctrl.SeekRelative(nil, 0, time.Second); return e },
		func() error { _, e := ctrl.SeekRelative(canceled, 0, time.Second); return e },
		func() error { _, e := ctrl.SeekAbsolute(nil, 0, time.Second); return e },
		func() error { _, e := ctrl.SeekAbsolute(canceled, 0, time.Second); return e },
		func() error { _, e := ctrl.StepSpeed(nil, 0, SpeedStepMilli); return e },
		func() error { _, e := ctrl.StepSpeed(canceled, 0, SpeedStepMilli); return e },
		func() error { _, e := ctrl.SetSpeed(nil, 0, DefaultSpeedMilli); return e },
		func() error { _, e := ctrl.SetSpeed(ctx, 0, 0); return e },
		func() error { _, e := ctrl.SetSpeed(canceled, 0, DefaultSpeedMilli); return e },
		func() error { _, e := ctrl.StepVolume(nil, 5); return e },
		func() error { _, e := ctrl.StepVolume(canceled, 5); return e },
		func() error { _, e := ctrl.SetVolume(nil, 50); return e },
		func() error { _, e := ctrl.SetVolume(canceled, 50); return e },
		func() error { _, e := ctrl.Poll(nil); return e },
		func() error { _, e := ctrl.Poll(canceled); return e },
		func() error { _, e := ctrl.Stop(nil); return e },
	} {
		if err := call(); err == nil {
			t.Fatal("expected error for invalid/canceled call")
		}
	}

	// Store read error on Load.
	fs.getErr = errors.New("db read failed")
	snap, err := ctrl.Load(ctx, Track{Key: bookA, Title: "A", Path: mediaA, Duration: 60 * time.Second}, true)
	if err == nil || snap.State != StateFailed || snap.Failure != "playback failed" {
		t.Fatalf("expected StateFailed on store read error, got %+v, %v", snap, err)
	}
	fs.getErr = nil

	// Store write error on Load stops backend so no orphan audio plays.
	fs.putErr = errors.New("db write failed")
	stopsBefore := mb.stops
	snap, err = ctrl.Load(ctx, Track{Key: bookA, Title: "A", Path: mediaA, Duration: 60 * time.Second}, true)
	if err == nil || snap.State != StateFailed || mb.stops != stopsBefore+1 {
		t.Fatalf("expected backend Stop on Load checkpoint failure, got %+v, %v, stops=%d", snap, err, mb.stops)
	}
	fs.putErr = nil

	// Successful Load clamps saved 500s position to 60s duration.
	snap, err = ctrl.Load(ctx, Track{Key: bookA, Title: "A", Path: mediaA, Duration: 60 * time.Second}, true)
	must(t, err)
	if snap.Position != 60*time.Second {
		t.Fatalf("expected clamped position 60s, got %v", snap.Position)
	}

	// Store write error during Pause/Seek/SetSpeed/Poll-EOF keeps active state and sets Failure.
	fs.putErr = errors.New("disk full")
	if snap, err = ctrl.Pause(ctx, snap.Session); err == nil || snap.State != StatePaused || snap.Failure != "playback failed" {
		t.Fatalf("unexpected Pause store error state: %+v, %v", snap, err)
	}
	if snap, err = ctrl.SeekAbsolute(ctx, snap.Session, 10*time.Second); err == nil || snap.State != StatePaused || snap.Failure != "playback failed" {
		t.Fatalf("unexpected Seek store error state: %+v, %v", snap, err)
	}
	if snap, err = ctrl.SetSpeed(ctx, snap.Session, 1_250_000); err == nil || snap.State != StatePaused || snap.Failure != "playback failed" {
		t.Fatalf("unexpected SetSpeed store error state: %+v, %v", snap, err)
	}
	mb.eof = true
	if snap, err = ctrl.Poll(ctx); err == nil || snap.State != StatePaused || snap.Failure != "playback failed" {
		t.Fatalf("unexpected Poll EOF store error state: %+v, %v", snap, err)
	}
	mb.eof = false

	// Store write error during Load switch aborts switch and keeps bookA active.
	if snap, err = ctrl.Load(ctx, Track{Key: bookB, Title: "B", Path: mediaB}, true); err == nil || snap.Book == nil || *snap.Book != bookA {
		t.Fatalf("expected bookA retained on switch checkpoint failure, got %+v, %v", snap, err)
	}
	fs.putErr = nil

	// Poll paused vs playing transitions.
	mb.paused = false
	snap, err = ctrl.Poll(ctx)
	must(t, err)
	if snap.State != StatePlaying {
		t.Fatalf("expected StatePlaying, got %v", snap.State)
	}
	mb.paused = true
	snap, err = ctrl.Poll(ctx)
	must(t, err)
	if snap.State != StatePaused {
		t.Fatalf("expected StatePaused, got %v", snap.State)
	}

	// Backend errors sanitize cleanly and ErrIPC triggers Stop reaping.
	mb.pauseErr = ErrRuntimeMissing
	if snap, err = ctrl.Play(ctx, snap.Session); err == nil || snap.Failure != "mpv runtime missing" {
		t.Fatalf("unexpected runtime missing failure: %+v, %v", snap, err)
	}
	mb.pauseErr = nil

	snap, err = ctrl.Load(ctx, Track{Key: bookA, Title: "A", Path: mediaA, Duration: 60 * time.Second}, true)
	must(t, err)
	mb.pauseErr = ErrIPC
	if snap, err = ctrl.Pause(ctx, snap.Session); err == nil || snap.Failure != "mpv playback connection failed" {
		t.Fatalf("unexpected Pause failure: %+v, %v", snap, err)
	}
	mb.pauseErr = nil

	snap, err = ctrl.Load(ctx, Track{Key: bookA, Title: "A", Path: mediaA, Duration: 60 * time.Second}, true)
	must(t, err)
	mb.posErr = ErrIPC
	if snap, err = ctrl.SeekRelative(ctx, snap.Session, time.Second); err == nil || snap.Failure != "mpv playback connection failed" {
		t.Fatalf("unexpected SeekRelative Position failure: %+v, %v", snap, err)
	}
	mb.posErr = nil

	// SetVolume while StateFailed preserves Failure message.
	if snap, err = ctrl.SetVolume(ctx, 40); err != nil || snap.Failure != "mpv playback connection failed" {
		t.Fatalf("expected failure preserved on SetVolume while failed, got %+v, %v", snap, err)
	}

	// Reload and test ErrIPC reaping + timeout/cancel sanitization.
	snap, err = ctrl.Load(ctx, Track{Key: bookA, Title: "A", Path: mediaA, Duration: 60 * time.Second}, true)
	must(t, err)
	stopsBefore = mb.stops
	mb.posErr = ErrIPC
	if snap, err = ctrl.Poll(ctx); err == nil || snap.Failure != "mpv playback connection failed" || mb.stops != stopsBefore+1 {
		t.Fatalf("expected ErrIPC reap and sanitized failure, got %+v, %v, stops=%d", snap, err, mb.stops)
	}
	mb.posErr = nil

	snap, err = ctrl.Load(ctx, Track{Key: bookA, Title: "A", Path: mediaA, Duration: 60 * time.Second}, true)
	must(t, err)
	mb.seekErr = context.DeadlineExceeded
	if snap, err = ctrl.SeekAbsolute(ctx, snap.Session, 5*time.Second); err == nil || snap.Failure != "playback timed out" {
		t.Fatalf("expected timeout failure, got %+v, %v", snap, err)
	}
	mb.seekErr = nil

	snap, err = ctrl.Load(ctx, Track{Key: bookA, Title: "A", Path: mediaA, Duration: 60 * time.Second}, true)
	must(t, err)
	mb.speedErr = context.Canceled
	if snap, err = ctrl.SetSpeed(ctx, snap.Session, DefaultSpeedMilli); err == nil || snap.Failure != "playback canceled" {
		t.Fatalf("expected canceled failure, got %+v, %v", snap, err)
	}
	mb.speedErr = nil

	snap, err = ctrl.Load(ctx, Track{Key: bookA, Title: "A", Path: mediaA, Duration: 60 * time.Second}, true)
	must(t, err)
	mb.volErr = errors.New("vol err")
	if snap, err = ctrl.SetVolume(ctx, 50); err == nil || snap.State != StateFailed {
		t.Fatalf("expected StateFailed on SetVolume error, got %+v, %v", snap, err)
	}
	mb.volErr = nil

	// Stop failure during Load switch.
	snap, err = ctrl.Load(ctx, Track{Key: bookA, Title: "A", Path: mediaA, Duration: 60 * time.Second}, true)
	must(t, err)
	mb.stopErr = ErrIPC
	if snap, err = ctrl.Load(ctx, Track{Key: bookB, Title: "B", Path: mediaB}, true); err == nil || snap.State != StateFailed {
		t.Fatalf("expected StateFailed on Stop error during Load, got %+v, %v", snap, err)
	}
	mb.stopErr = nil

	// Start failure during Load.
	mb.startErr = ErrRuntimeMissing
	if snap, err = ctrl.Load(ctx, Track{Key: bookB, Title: "B", Path: mediaB}, true); err == nil || snap.State != StateFailed || snap.Failure != "mpv runtime missing" {
		t.Fatalf("expected StateFailed on Start error, got %+v, %v", snap, err)
	}
	mb.startErr = nil

	// Stop with store error + backend stop error joins errors and sets Failure.
	snap, err = ctrl.Load(ctx, Track{Key: bookA, Title: "A", Path: mediaA, Duration: 60 * time.Second}, true)
	must(t, err)
	fs.putErr = errors.New("stop save err")
	mb.stopErr = ErrIPC
	if snap, err = ctrl.Stop(ctx); err == nil || snap.State != StateStopped || snap.Failure == "" {
		t.Fatalf("expected joined error and failure on Stop, got %+v, %v", snap, err)
	}
	fs.putErr = nil
	mb.stopErr = nil

	// Close with nil ctx and active track saves checkpoint and stops cleanly.
	_, err = ctrl.Load(ctx, Track{Key: bookA, Title: "A", Path: mediaA, Duration: 60 * time.Second}, true)
	must(t, err)
	must(t, ctrl.Close(nil))

	// Close propagates checkpoint and backend Stop errors while still closing.
	ctrlErr, err := NewController(&mockBackend{dur: 60 * time.Second, stopErr: ErrIPC}, Options{Store: &faultStore{inner: realStore, putErr: errors.New("close save err")}})
	must(t, err)
	ctrlErr.state = StatePlaying
	ctrlErr.book = &bookA
	if err := ctrlErr.Close(ctx); err == nil {
		t.Fatal("expected joined error on Close")
	}

	// Negative saved PositionNS from store is clamped to 0 on Load.
	negStore := &negativePosStore{}
	ctrlNeg, err := NewController(&mockBackend{dur: 60 * time.Second}, Options{Store: negStore})
	must(t, err)
	snapNeg, err := ctrlNeg.Load(ctx, Track{Key: bookA, Title: "Neg", Path: mediaA, Duration: 60 * time.Second}, false)
	must(t, err)
	if snapNeg.Position != 0 {
		t.Fatalf("expected negative saved position clamped to 0, got %v", snapNeg.Position)
	}
	must(t, ctrlNeg.Close(ctx))
}

type negativePosStore struct{}

func (negativePosStore) ListeningState(_ context.Context, key store.BookKey) (store.ListeningState, bool, error) {
	return store.ListeningState{Key: key, PositionNS: -500, SpeedMilli: DefaultSpeedMilli, Completion: store.CompletionUnfinished}, true, nil
}

func (negativePosStore) PutListeningState(context.Context, store.ListeningState) error { return nil }

type blockingBackend struct {
	mockBackend
	entered chan struct{}
	release chan struct{}
}

func (b *blockingBackend) Start(ctx context.Context, mediaPath string, startPos time.Duration, speedMilli int64, volume int, paused bool) error {
	close(b.entered)
	<-b.release
	return b.mockBackend.Start(ctx, mediaPath, startPos, speedMilli, volume, paused)
}

func TestControllerQueuedCancellationWhileOpMuHeld(t *testing.T) {
	ctx := context.Background()
	bb := &blockingBackend{
		mockBackend: mockBackend{dur: 60 * time.Second},
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	ctrl, err := NewController(bb, Options{})
	must(t, err)
	defer func() { _ = ctrl.Close(ctx) }()

	book := store.BookKey{Provider: "local", Namespace: "user", ID: "queued-1"}
	media := filepath.Join(t.TempDir(), "queued.m4b")

	// First load an active track so transport calls pass checkTransportLocked.
	bb.release = make(chan struct{})
	bb.entered = make(chan struct{})
	go func() {
		<-bb.entered
		close(bb.release)
	}()
	snap, err := ctrl.Load(ctx, Track{Key: book, Title: "Q", Path: media, Duration: 60 * time.Second}, true)
	must(t, err)

	// Now block opMu inside a second Load.
	bb.entered = make(chan struct{})
	bb.release = make(chan struct{})
	loadDone := make(chan error, 1)
	go func() {
		_, e := ctrl.Load(ctx, Track{Key: book, Title: "Q2", Path: media, Duration: 60 * time.Second}, true)
		loadDone <- e
	}()
	<-bb.entered

	// Queue calls with contexts that cancel while waiting on opMu.
	var wg sync.WaitGroup
	errs := make(chan error, 11)
	var cancels []context.CancelFunc
	queue := func(fn func(context.Context) error) {
		qctx, qcancel := context.WithCancel(ctx)
		cancels = append(cancels, qcancel)
		wg.Go(func() {
			errs <- fn(qctx)
		})
	}
	queue(func(c context.Context) error {
		_, e := ctrl.Load(c, Track{Key: book, Title: "Q3", Path: media}, true)
		return e
	})
	queue(func(c context.Context) error { _, e := ctrl.Toggle(c, snap.Session); return e })
	queue(func(c context.Context) error { _, e := ctrl.Pause(c, snap.Session); return e })
	queue(func(c context.Context) error { _, e := ctrl.Play(c, snap.Session); return e })
	queue(func(c context.Context) error { _, e := ctrl.SeekRelative(c, snap.Session, time.Second); return e })
	queue(func(c context.Context) error { _, e := ctrl.SeekAbsolute(c, snap.Session, time.Second); return e })
	queue(func(c context.Context) error { _, e := ctrl.StepSpeed(c, snap.Session, SpeedStepMilli); return e })
	queue(func(c context.Context) error { _, e := ctrl.SetSpeed(c, snap.Session, DefaultSpeedMilli); return e })
	queue(func(c context.Context) error { _, e := ctrl.StepVolume(c, 5); return e })
	queue(func(c context.Context) error { _, e := ctrl.SetVolume(c, 50); return e })
	queue(func(c context.Context) error { _, e := ctrl.Poll(c); return e })

	time.Sleep(15 * time.Millisecond)
	for _, qcancel := range cancels {
		qcancel()
	}
	close(bb.release)
	must(t, <-loadDone)
	wg.Wait()
	close(errs)
	for e := range errs {
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("expected context.Canceled for queued op, got %v", e)
		}
	}
}

func TestControllerConcurrentSerialization(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	proc := NewMPVProcess(MPVConfig{Binary: fakeMPVBinary(t, "normal"), StartTimeout: 3 * time.Second, RPCTimeout: time.Second})
	ctrl, err := NewController(proc, Options{Store: s})
	must(t, err)
	defer func() { _ = ctrl.Close(ctx) }()

	book := store.BookKey{Provider: "local", Namespace: "user", ID: "concurrent-1"}
	media := filepath.Join(t.TempDir(), "book.m4b")
	snap, err := ctrl.Load(ctx, Track{Key: book, Title: "Concurrent", Path: media, Duration: 120 * time.Second}, true)
	must(t, err)

	var wg sync.WaitGroup
	var ops atomic.Int32
	for i := range 6 {
		wg.Go(func() {
			for j := range 5 {
				switch (i + j) % 4 {
				case 0:
					_, _ = ctrl.Poll(ctx)
				case 1:
					_, _ = ctrl.SeekAbsolute(ctx, snap.Session, time.Duration(j+1)*time.Second)
				case 2:
					_, _ = ctrl.SetVolume(ctx, 50+j)
				case 3:
					_ = ctrl.Snapshot()
				}
				ops.Add(1)
			}
		})
	}
	wg.Wait()
	if ops.Load() != 30 {
		t.Fatalf("expected 30 ops, got %d", ops.Load())
	}
	must(t, ctrl.Close(ctx))
}

func TestControllerSmartRewindCompletionAndReplay(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	bookA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	bookB := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	mediaA := filepath.Join(t.TempDir(), "a.m4b")
	mediaB := filepath.Join(t.TempDir(), "b.m4b")

	// Invalid SmartRewindPolicy rejected.
	for _, bad := range []SmartRewindPolicy{
		{Threshold: -time.Second, Rewind: time.Second},
		{Threshold: time.Second, Rewind: -time.Second},
	} {
		if _, err := NewController(&mockBackend{}, Options{SmartRewind: &bad}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for %+v, got %v", bad, err)
		}
	}

	now := time.Unix(1_700_000_000, 0)
	policy := DefaultSmartRewindPolicy()
	mb := &mockBackend{dur: 120 * time.Second}
	ctrl, err := NewController(mb, Options{
		Store:       s,
		Clock:       func() time.Time { return now },
		SmartRewind: &policy,
	})
	must(t, err)
	defer func() { _ = ctrl.Close(ctx) }()

	if err := ctrl.SetSmartRewind(SmartRewindPolicy{Threshold: -1}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput on SetSmartRewind, got %v", err)
	}

	// Load bookA at 0, advance to 30s, pause, resume after 9s (< 10s threshold) -> NO rewind.
	snap, err := ctrl.Load(ctx, Track{Key: bookA, Title: "A", Path: mediaA, Duration: 120 * time.Second}, true)
	must(t, err)
	mb.mu.Lock()
	mb.pos = 30 * time.Second
	mb.mu.Unlock()
	snap, err = ctrl.Pause(ctx, snap.Session)
	must(t, err)
	now = now.Add(9 * time.Second)
	snap, err = ctrl.Play(ctx, snap.Session)
	must(t, err)
	if snap.Position != 30*time.Second {
		t.Fatalf("expected no rewind below threshold, got %v", snap.Position)
	}

	// Pause at 30s, advance 10s (>= threshold) -> Play rewinds 5s to 25s and persists checkpoint.
	snap, err = ctrl.Pause(ctx, snap.Session)
	must(t, err)
	now = now.Add(10 * time.Second)
	snap, err = ctrl.Play(ctx, snap.Session)
	must(t, err)
	if snap.Position != 25*time.Second {
		t.Fatalf("expected 5s smart rewind to 25s, got %v", snap.Position)
	}
	saved, found, err := s.ListeningState(ctx, bookA)
	must(t, err)
	if !found || saved.PositionNS != int64(25*time.Second) {
		t.Fatalf("expected rewound checkpoint 25s, got %+v", saved)
	}

	// Deliberate seek while paused clears pause timer -> NO smart rewind on subsequent Play!
	snap, err = ctrl.Pause(ctx, snap.Session)
	must(t, err)
	now = now.Add(30 * time.Second)
	snap, err = ctrl.SeekAbsolute(ctx, snap.Session, 40*time.Second)
	must(t, err)
	snap, err = ctrl.Play(ctx, snap.Session)
	must(t, err)
	if snap.Position != 40*time.Second {
		t.Fatalf("deliberate seek while paused triggered smart rewind: %v", snap.Position)
	}

	// Clamp at 0 when position < Rewind.
	snap, err = ctrl.SeekAbsolute(ctx, snap.Session, 2*time.Second)
	must(t, err)
	snap, err = ctrl.Pause(ctx, snap.Session)
	must(t, err)
	now = now.Add(20 * time.Second)
	snap, err = ctrl.Play(ctx, snap.Session)
	must(t, err)
	if snap.Position != 0 {
		t.Fatalf("expected smart rewind clamped at 0, got %v", snap.Position)
	}

	// Restart restoration: Stop does NOT mark finished; Load after >= threshold rewinds 5s.
	snap, err = ctrl.SeekAbsolute(ctx, snap.Session, 50*time.Second)
	must(t, err)
	snap, err = ctrl.Stop(ctx)
	must(t, err)
	saved, _, _ = s.ListeningState(ctx, bookA)
	if saved.Completion != store.CompletionUnfinished {
		t.Fatalf("deliberate Stop marked book finished: %+v", saved)
	}
	now = now.Add(60 * time.Second)
	snap, err = ctrl.Load(ctx, Track{Key: bookA, Title: "A", Path: mediaA, Duration: 120 * time.Second}, true)
	must(t, err)
	if snap.Position != 45*time.Second {
		t.Fatalf("expected restart smart rewind to 45s, got %v", snap.Position)
	}

	// Poll detects external pause -> smart rewind applies on Play; Poll EOF marks CompletionFinished.
	mb.mu.Lock()
	mb.paused = true
	mb.pos = 45 * time.Second
	mb.mu.Unlock()
	snap, err = ctrl.Poll(ctx)
	must(t, err)
	now = now.Add(15 * time.Second)
	snap, err = ctrl.Play(ctx, snap.Session)
	must(t, err)
	if snap.Position != 40*time.Second {
		t.Fatalf("expected smart rewind after Poll-detected pause, got %v", snap.Position)
	}

	// Natural EOF marks CompletionFinished and persists it.
	mb.mu.Lock()
	mb.eof = true
	mb.mu.Unlock()
	snap, err = ctrl.Poll(ctx)
	must(t, err)
	if snap.Completion != store.CompletionFinished || snap.Position != 120*time.Second {
		t.Fatalf("expected CompletionFinished at EOF, got %+v", snap)
	}
	mb.mu.Lock()
	mb.eof = false
	mb.mu.Unlock()

	// SetCompletion marks unfinished/finished without interrupting playback or changing position/speed.
	snap, err = ctrl.SetSpeed(ctx, snap.Session, 1_500_000)
	must(t, err)
	snap, err = ctrl.SetCompletion(ctx, bookA, store.CompletionUnfinished)
	must(t, err)
	if snap.Completion != store.CompletionUnfinished || snap.Position != 120*time.Second || snap.SpeedMilli != 1_500_000 {
		t.Fatalf("SetCompletion altered position/speed: %+v", snap)
	}
	// SetCompletion on inactive bookB preserves its saved state or defaults.
	_, err = ctrl.SetCompletion(ctx, bookB, store.CompletionFinished)
	must(t, err)
	savedB, foundB, _ := s.ListeningState(ctx, bookB)
	if !foundB || savedB.Completion != store.CompletionFinished || savedB.PositionNS != 0 {
		t.Fatalf("inactive SetCompletion failed: found=%v %+v", foundB, savedB)
	}

	// Replay resets position to 0 and CompletionUnfinished while preserving saved speed (1.5x).
	snap, err = ctrl.Replay(ctx, Track{Key: bookA, Title: "A", Path: mediaA, Duration: 120 * time.Second})
	must(t, err)
	if snap.State != StatePlaying || snap.Position != 0 || snap.Completion != store.CompletionUnfinished || snap.SpeedMilli != 1_500_000 {
		t.Fatalf("Replay failed: %+v", snap)
	}

	// Disable smart rewind via SetSmartRewind(SmartRewindPolicy{}).
	must(t, ctrl.SetSmartRewind(SmartRewindPolicy{}))
	snap, err = ctrl.SeekAbsolute(ctx, snap.Session, 30*time.Second)
	must(t, err)
	snap, err = ctrl.Pause(ctx, snap.Session)
	must(t, err)
	now = now.Add(time.Hour)
	snap, err = ctrl.Play(ctx, snap.Session)
	must(t, err)
	if snap.Position != 30*time.Second {
		t.Fatalf("disabled smart rewind still rewound: %v", snap.Position)
	}

	// Error paths for SetCompletion and Replay.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, call := range []func() error{
		func() error { _, e := ctrl.SetCompletion(nil, bookA, store.CompletionFinished); return e },
		func() error { _, e := ctrl.SetCompletion(ctx, store.BookKey{}, store.CompletionFinished); return e },
		func() error { _, e := ctrl.SetCompletion(ctx, bookA, "bad"); return e },
		func() error { _, e := ctrl.SetCompletion(canceled, bookA, store.CompletionFinished); return e },
		func() error { _, e := ctrl.Replay(nil, Track{Key: bookA, Title: "A", Path: mediaA}); return e },
		func() error { _, e := ctrl.Replay(ctx, Track{}); return e },
		func() error { _, e := ctrl.Replay(canceled, Track{Key: bookA, Title: "A", Path: mediaA}); return e },
	} {
		if err := call(); err == nil {
			t.Fatal("expected error for invalid SetCompletion/Replay call")
		}
	}

	// Backend/store error paths on smart rewind seek, Replay, and SetCompletion.
	fs := &faultStore{inner: s}
	mbErr := &mockBackend{dur: 60 * time.Second}
	pOn := DefaultSmartRewindPolicy()
	ctrlFault, err := NewController(mbErr, Options{Store: fs, Clock: func() time.Time { return now }, SmartRewind: &pOn})
	must(t, err)
	snapF, err := ctrlFault.Load(ctx, Track{Key: bookA, Title: "A", Path: mediaA, Duration: 60 * time.Second}, true)
	must(t, err)
	snapF, err = ctrlFault.SeekAbsolute(ctx, snapF.Session, 20*time.Second)
	must(t, err)
	snapF, err = ctrlFault.Pause(ctx, snapF.Session)
	must(t, err)
	now = now.Add(20 * time.Second)
	mbErr.seekErr = ErrIPC
	if _, err := ctrlFault.Play(ctx, snapF.Session); err == nil {
		t.Fatal("expected SeekExact error during smart rewind")
	}
	mbErr.seekErr = nil
	fs.putErr = errors.New("disk full")
	if _, err := ctrlFault.SetCompletion(ctx, bookB, store.CompletionUnfinished); err == nil {
		t.Fatal("expected store error on SetCompletion")
	}
	fs.getErr = errors.New("read err")
	if _, err := ctrlFault.Replay(ctx, Track{Key: bookB, Title: "B", Path: mediaB}); err == nil {
		t.Fatal("expected store read error on Replay")
	}
	fs.getErr = nil
	fs.putErr = nil
	must(t, ctrlFault.Close(ctx))
	if _, err := ctrlFault.SetCompletion(ctx, bookA, store.CompletionFinished); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed on SetCompletion, got %v", err)
	}
	if _, err := ctrlFault.Replay(ctx, Track{Key: bookA, Title: "A", Path: mediaA}); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed on Replay, got %v", err)
	}
}
