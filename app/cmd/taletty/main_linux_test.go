package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/ncruces/go-sqlite3"
	"github.com/susamn/taletty/app/internal/app"
	"github.com/susamn/taletty/app/internal/profile"
	"github.com/susamn/taletty/app/internal/store"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	path := os.Getenv("TALETTY_SQLITE_TEST_CACHE")
	owned := path == ""
	if owned {
		var err error
		path, err = os.MkdirTemp("", "taletty-cmd-sqlite-compiler-")
		if err != nil {
			os.Exit(1)
		}
		os.Setenv("TALETTY_SQLITE_TEST_CACHE", path)
	}
	cache, err := wazero.NewCompilationCacheWithDir(path)
	if err != nil {
		os.Exit(1)
	}
	sqlite3.RuntimeConfig = wazero.NewRuntimeConfig().WithMemoryLimitPages(4096).
		WithCoreFeatures(api.CoreFeaturesV2).WithCompilationCache(cache)
	if raw := os.Getenv("TALETTY_APP_CLI_TEST_ARGS"); raw != "" {
		var args []string
		if json.Unmarshal([]byte(raw), &args) != nil {
			os.Exit(2)
		}
		os.Args = append([]string{"taletty"}, args...)
		main()
	}
	code := m.Run()
	if owned {
		os.RemoveAll(path)
	}
	os.Exit(code)
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestCLIUsageValidationAndErrorPaths(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, &out, &errOut, defaultDeps()); code != 0 || !strings.Contains(out.String(), "Usage: taletty") {
		t.Fatalf("help failed: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"-h"}, &out, &errOut, defaultDeps()); code != 0 {
		t.Fatalf("-h failed: code=%d", code)
	}
	if code := run(context.Background(), []string{"--help"}, failingWriter{}, &errOut, defaultDeps()); code != 1 {
		t.Fatalf("expected exit 1 on broken stdout for --help, got %d", code)
	}
	if code := run(context.Background(), []string{"-h"}, failingWriter{}, &errOut, defaultDeps()); code != 1 {
		t.Fatalf("expected exit 1 on broken stdout for -h, got %d", code)
	}

	for _, args := range [][]string{
		{"--unknown-secret-flag"},
		{"extra-arg"},
		{"--python", ""},
		{"--python", "   "},
		{"--python", "bad\x00python"},
	} {
		out.Reset()
		errOut.Reset()
		if code := run(context.Background(), args, &out, &errOut, defaultDeps()); code != 2 {
			t.Fatalf("args %v: got code %d, want 2", args, code)
		}
		if strings.Contains(errOut.String(), "unknown-secret-flag") || strings.Contains(errOut.String(), "extra-arg") {
			t.Fatalf("usage error echoed supplied argument: %q", errOut.String())
		}
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	errOut.Reset()
	if code := run(canceled, nil, &out, &errOut, defaultDeps()); code != 130 {
		t.Fatalf("canceled ctx: got code %d, want 130", code)
	}

	// Invalid home directory.
	errOut.Reset()
	deps := defaultDeps()
	deps.userHomeDir = func() (string, error) { return "relative", nil }
	if code := run(context.Background(), nil, &out, &errOut, deps); code != 1 {
		t.Fatalf("relative home: got code %d, want 1", code)
	}

	// Missing controlling TTY.
	deps = defaultDeps()
	deps.userHomeDir = func() (string, error) { return t.TempDir(), nil }
	deps.openTTY = func() (*os.File, error) { return nil, errors.New("no tty") }
	errOut.Reset()
	if code := run(context.Background(), nil, &out, &errOut, deps); code != 1 || !strings.Contains(errOut.String(), "interactive terminal") {
		t.Fatalf("missing tty: code=%d err=%q", code, errOut.String())
	}

	// Workspace open failure (busy and canceled).
	deps.openTTY = func() (*os.File, error) { return os.Open(os.DevNull) }
	deps.openWorkspace = func(context.Context, string, string) (*app.Coordinator, error) {
		return nil, profile.ErrBusy
	}
	errOut.Reset()
	if code := run(context.Background(), nil, &out, &errOut, deps); code != 1 {
		t.Fatalf("busy workspace: got code %d, want 1", code)
	}
	deps.openWorkspace = func(context.Context, string, string) (*app.Coordinator, error) {
		return nil, context.Canceled
	}
	if code := run(context.Background(), nil, &out, &errOut, deps); code != 130 {
		t.Fatalf("canceled workspace: got code %d, want 130", code)
	}

	// Session run error, cancel, and close error.
	home := t.TempDir()
	deps = defaultDeps()
	deps.userHomeDir = func() (string, error) { return home, nil }
	deps.openTTY = func() (*os.File, error) { return os.Open(os.DevNull) }
	deps.runSession = func(context.Context, *app.Coordinator, *os.File, io.Writer) error {
		return errors.New("terminal failed")
	}
	if code := run(context.Background(), nil, &out, &errOut, deps); code != 1 {
		t.Fatalf("runSession error: got code %d, want 1", code)
	}
	deps.runSession = func(context.Context, *app.Coordinator, *os.File, io.Writer) error {
		return context.Canceled
	}
	if code := run(context.Background(), nil, &out, &errOut, deps); code != 130 {
		t.Fatalf("runSession cancel: got code %d, want 130", code)
	}
	deps.runSession = func(context.Context, *app.Coordinator, *os.File, io.Writer) error {
		return nil
	}
	if code := run(context.Background(), []string{"--python", "/custom/python"}, &out, &errOut, deps); code != 0 {
		t.Fatalf("clean run failed: code=%d err=%q", code, errOut.String())
	}

	// Close error after clean runSession returns exit 1.
	deps.openWorkspace = func(ctx context.Context, h, _ string) (*app.Coordinator, error) {
		ws, err := profile.Open(ctx, h, 100)
		if err != nil {
			return nil, err
		}
		return app.NewCoordinator(ctx, ws.Store(), app.Options{
			WorkspaceCloser: func(c context.Context) error {
				_ = ws.Close(c)
				return errors.New("close failed")
			},
		})
	}
	errOut.Reset()
	if code := run(context.Background(), nil, &out, &errOut, deps); code != 1 {
		t.Fatalf("close error: got code %d, want 1", code)
	}
}

func TestMainSubprocessHelpInvalidAndRealPTYSession(t *testing.T) {
	home := t.TempDir()
	legacyDir := filepath.Join(home, ".config", "audible-tui")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyFile := filepath.Join(legacyDir, "auth.json")
	if err := os.WriteFile(legacyFile, []byte(`{"legacy":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Seed cached book so offline launch opens directly into cached library without Python.
	ws, err := profile.Open(context.Background(), home, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Store().ReplaceCatalog(context.Background(), "audible", "us", []store.CatalogBook{{
		Key:   store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"},
		Title: "Cached PTY Book",
	}}, 100); err != nil {
		t.Fatal(err)
	}
	if err := ws.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Subprocess --help and invalid flag tests exercising main().
	runSub := func(args ...string) (int, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		raw, _ := json.Marshal(args)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMain$")
		cmd.Env = append(os.Environ(), "TALETTY_APP_CLI_TEST_ARGS="+string(raw), "HOME="+home, "GORACE=atexit_sleep_ms=0")
		out, err := cmd.CombinedOutput()
		if err == nil {
			return 0, string(out)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), string(out)
		}
		t.Fatalf("unexpected exec error: %v (%s)", err, out)
		return -1, ""
	}
	if code, out := runSub("--help"); code != 0 || !strings.Contains(out, "Usage: taletty") {
		t.Fatalf("main --help failed: code=%d out=%q", code, out)
	}
	if code, _ := runSub("--bad-flag"); code != 2 {
		t.Fatalf("main --bad-flag: got code %d, want 2", code)
	}

	// Real controlling PTY launch -> render cached book -> quit with 'q' -> verify termios & ViewState.
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(filepath.Join("/dev/pts", itoa(n)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: 80, Row: 20}); err != nil {
		t.Fatal(err)
	}
	beforeTermios, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	raw, _ := json.Marshal([]string{})
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMain$")
	cmd.Env = append(os.Environ(),
		"TALETTY_APP_CLI_TEST_ARGS="+string(raw),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "ignored-xdg"),
		"PATH="+t.TempDir(),
		"TALETTY_PYTHON=",
		"TERM=xterm-256color",
		"GORACE=atexit_sleep_ms=0",
	)
	cmd.ExtraFiles = []*os.File{slave}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 3}
	stderrFile, err := os.CreateTemp(t.TempDir(), "taletty-stderr-")
	if err != nil {
		t.Fatal(err)
	}
	defer stderrFile.Close()
	cmd.Stderr = stderrFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Wait until the TUI renders the cached book title on the PTY master, then send 'q'.
	rendered := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		var buf bytes.Buffer
		tmp := make([]byte, 4096)
		sentQuit := false
		for {
			nr, rerr := master.Read(tmp)
			if nr > 0 {
				buf.Write(tmp[:nr])
				if !sentQuit && strings.Contains(ansi.Strip(buf.String()), "Cached PTY Book") {
					sentQuit = true
					close(rendered)
					_, _ = master.Write([]byte("q"))
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	select {
	case <-rendered:
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = slave.Close()
		_ = master.Close()
		<-drained
		errBytes, _ := os.ReadFile(stderrFile.Name())
		t.Fatalf("timed out waiting for PTY render (stderr=%q)", string(errBytes))
	}
	if err := cmd.Wait(); err != nil {
		_ = master.Close()
		<-drained
		errBytes, _ := os.ReadFile(stderrFile.Name())
		t.Fatalf("taletty PTY session failed: %v (stderr=%q)", err, string(errBytes))
	}
	afterTermios, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	_ = slave.Close()
	_ = master.Close()
	<-drained
	if err != nil || *afterTermios != *beforeTermios {
		t.Fatalf("termios not restored cleanly: err=%v before=%+v after=%+v", err, beforeTermios, afterTermios)
	}
	legacyBytes, err := os.ReadFile(legacyFile)
	if err != nil || string(legacyBytes) != `{"legacy":true}` {
		t.Fatalf("legacy file modified: %v %q", err, legacyBytes)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
