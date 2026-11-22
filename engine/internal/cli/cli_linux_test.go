package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/susamn/taletty/engine/connectors/audible"
	"github.com/susamn/taletty/engine/model"
)

type fakeAccount struct {
	loginErr, closeErr error
	libraryErr         error
	downloadErr        error
	snapshot           model.LibrarySnapshot
	download           model.DownloadResult
	downloadBook       model.BookID
	closed             bool
	cancel             context.CancelFunc
	tty                *os.File
}

func (a *fakeAccount) Login(ctx context.Context, tty *os.File) error {
	a.tty = tty
	if a.cancel != nil {
		a.cancel()
		return ctx.Err()
	}
	return a.loginErr
}
func (a *fakeAccount) Library(ctx context.Context, id string, report func(model.OperationEvent)) (model.LibrarySnapshot, error) {
	if id != "cli-sync" || report != nil {
		panic("CLI must not stream provisional events")
	}
	if a.cancel != nil {
		a.cancel()
		return model.LibrarySnapshot{}, ctx.Err()
	}
	return a.snapshot, a.libraryErr
}
func (a *fakeAccount) Download(ctx context.Context, id string, book model.BookID, report func(model.OperationEvent)) (model.DownloadResult, error) {
	if id != "cli-download" || report != nil {
		panic("CLI download must not stream provisional events")
	}
	a.downloadBook = book
	if a.cancel != nil {
		a.cancel()
		if a.downloadErr == nil && a.download.State == model.MediaReady {
			return a.download, nil
		}
		if a.downloadErr != nil {
			return a.download, a.downloadErr
		}
		return a.download, ctx.Err()
	}
	return a.download, a.downloadErr
}
func (a *fakeAccount) Close(ctx context.Context) error {
	if ctx.Err() != nil {
		panic("cleanup used canceled context")
	}
	a.closed = true
	return a.closeErr
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("fixture-private-output") }
func ready() audible.Diagnostics {
	return audible.Diagnostics{Python: audible.Ready, Audible: audible.Ready, FFmpeg: audible.Ready, FFprobe: audible.Ready}
}
func fixture(t *testing.T, client *fakeAccount) dependencies {
	t.Helper()
	return dependencies{
		resolvePython: audible.DiscoverPython,
		doctor:        func(context.Context, string) (audible.Diagnostics, error) { return ready(), nil },
		open: func(_ context.Context, root, python string) (account, error) {
			if root != filepath.Join(os.Getenv("HOME"), ".config", "taletty") || python != "fixture-python" {
				t.Fatal("wrong explicit root/runtime")
			}
			return client, nil
		},
		terminal: func() (*os.File, error) { return os.Open(os.DevNull) },
	}
}
func invoke(args []string, deps dependencies) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, &out, &errOut, deps)
	return code, out.String(), errOut.String()
}

func TestUsageAndDoctorReadOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	deps := fixture(t, &fakeAccount{})
	for _, args := range [][]string{nil, {"doctor", "--python", ""}, {"doctor", "--python", "   "}, {"doctor", "--python", "bad\x00secret"},
		{"sync", "--python", ""}, {"login", "--python", "   "}, {"login", "--python", "fixture-python", "--json"},
		{"doctor", "--python", "fixture-python", "--marketplace", "de"}, {"doctor", "--python", "fixture-python", "extra"},
		{"doctor", "--fixture-private=secret"}} {
		code, out, errOut := invoke(args, deps)
		if code != 2 || out != "" || strings.Contains(errOut, "secret") {
			t.Fatal(code, out, errOut)
		}
	}
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}, {"doctor", "--help"}} {
		code, out, _ := invoke(args, deps)
		if code != 0 || !strings.Contains(out, "Usage:") {
			t.Fatal(code, out)
		}
	}
	for _, machine := range []bool{false, true} {
		args := []string{"doctor", "--python", "fixture-python"}
		if machine {
			args = append(args, "--json")
		}
		code, out, errOut := invoke(args, deps)
		if code != 0 || errOut != "" {
			t.Fatal(code, errOut)
		}
		if machine {
			var result struct {
				Ready       bool
				Diagnostics audible.Diagnostics
			}
			if json.Unmarshal([]byte(out), &result) != nil || !result.Ready || !result.Diagnostics.Ready() {
				t.Fatal(out)
			}
		} else if !strings.Contains(out, "Python: ready") {
			t.Fatal(out)
		}
	}
	deps.doctor = func(context.Context, string) (audible.Diagnostics, error) {
		d := ready()
		d.FFmpeg = audible.RuntimeMissing
		return d, nil
	}
	if code, _, _ := invoke([]string{"doctor", "--python", "fixture-python"}, deps); code != 1 {
		t.Fatal(code)
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatal("doctor/usage created state", err)
	}
}

func TestLoginFreshRootRestartAndNoLegacy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "must-not-use"))
	legacy := filepath.Join(home, ".config", "audible-tui")
	if err := os.MkdirAll(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(legacy, "auth.json")
	if err := os.WriteFile(marker, []byte("fixture-prior-state"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		account := &fakeAccount{}
		code, out, errOut := invoke([]string{"login", "--python", "fixture-python"}, fixture(t, account))
		if code != 0 || !account.closed || !strings.Contains(out, "Signed in") || errOut != "" {
			t.Fatal(code, out, errOut)
		}
		if _, err := account.tty.Stat(); err == nil {
			t.Fatal("host retained terminal")
		}
		info, err := os.Stat(filepath.Join(home, ".config", "taletty"))
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatal("root privacy", err)
		}
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "fixture-prior-state" {
		t.Fatal("legacy data changed", err)
	}
	if _, err := os.Stat(os.Getenv("XDG_CONFIG_HOME")); !os.IsNotExist(err) {
		t.Fatal("XDG override used")
	}
}

func TestLoginBoundariesAndCleanup(t *testing.T) {
	args := []string{"login", "--python", "fixture-python"}
	t.Setenv("HOME", t.TempDir())
	deps := fixture(t, &fakeAccount{})
	deps.terminal = func() (*os.File, error) { return nil, errors.New("fixture-private-terminal") }
	if code, out, errOut := invoke(args, deps); code != 1 || out != "" || strings.Contains(errOut, "fixture-private") {
		t.Fatal(code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".config")); !os.IsNotExist(err) {
		t.Fatal("nonterminal created root")
	}
	for _, home := range []string{"", "relative"} {
		t.Setenv("HOME", home)
		if code, _, _ := invoke(args, fixture(t, &fakeAccount{})); code != 1 {
			t.Fatal(code)
		}
	}
	t.Setenv("HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".config"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := invoke(args, fixture(t, &fakeAccount{})); code != 1 {
		t.Fatal(code)
	}
	t.Setenv("HOME", t.TempDir())
	deps = fixture(t, &fakeAccount{})
	deps.open = func(context.Context, string, string) (account, error) { return nil, audible.ErrAuthBusy }
	if code, _, _ := invoke(args, deps); code != 1 {
		t.Fatal(code)
	}
	for _, primary := range []error{nil, errors.New("fixture-private-provider"), &audible.LoginError{Code: "storage_uncertain"}} {
		client := &fakeAccount{loginErr: primary, closeErr: errors.New("fixture-private-close")}
		code, out, errOut := invoke(args, fixture(t, client))
		if code != 1 || !client.closed || out != "" || !strings.Contains(errOut, "cleanup failed") || strings.Contains(errOut, "fixture-private") {
			t.Fatal(code, out, errOut)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &fakeAccount{cancel: cancel}
	var out, errOut bytes.Buffer
	if code := run(ctx, args, &out, &errOut, fixture(t, client)); code != 130 || !client.closed {
		t.Fatal(code)
	}
	if code := run(ctx, args, &out, &errOut, fixture(t, &fakeAccount{})); code != 130 {
		t.Fatal(code)
	}
}

func TestSafeFailuresAndOutputErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	failures := []error{context.Canceled, context.DeadlineExceeded, audible.ErrAuthBusy, audible.ErrAuthStorage, audible.ErrRuntimeMissing, audible.ErrRuntimeIncompatible, errors.New("fixture-private")}
	for _, code := range []string{"canceled", "terminal_required", "invalid_input", "runtime_missing", "runtime_incompatible", "login_failed", "storage_failed", "storage_uncertain", "internal", "fixture-private"} {
		failures = append(failures, &audible.LoginError{Code: code})
	}
	failures = append(failures, errors.Join(context.Canceled, audible.ErrTerminal))
	for _, failure := range failures {
		deps := fixture(t, &fakeAccount{})
		deps.doctor = func(context.Context, string) (audible.Diagnostics, error) { return audible.Diagnostics{}, failure }
		for _, machine := range []bool{false, true} {
			args := []string{"doctor", "--python", "fixture-python"}
			if machine {
				args = append(args, "--json")
			}
			code, out, errOut := invoke(args, deps)
			if code == 0 || strings.Contains(out+errOut, "fixture-private") {
				t.Fatal(code, out, errOut)
			}
			if machine {
				var payload struct {
					Failure struct{ Code, Message, Retry string }
				}
				if json.Unmarshal([]byte(out), &payload) != nil || payload.Failure.Code == "" || payload.Failure.Message == "" {
					t.Fatal(out)
				}
			}
		}
	}
	deps := fixture(t, &fakeAccount{})
	sink := io.Discard
	for _, args := range [][]string{{"--help"}, {"doctor", "--help"}, {"doctor", "--python", "fixture-python"}, {"doctor", "--python", "fixture-python", "--json"}, {"login", "--python", "fixture-python"}} {
		if code := run(context.Background(), args, brokenWriter{}, sink, deps); code != 1 {
			t.Fatal(code)
		}
	}
	deps.doctor = func(context.Context, string) (audible.Diagnostics, error) {
		return audible.Diagnostics{}, audible.ErrRuntimeMissing
	}
	if code := run(context.Background(), []string{"doctor", "--python", "fixture-python", "--json"}, brokenWriter{}, sink, deps); code != 1 {
		t.Fatal(code)
	}
}

func TestOptionalPythonDiscoveryAndMissingRuntime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacyBin := filepath.Join(home, ".config", "audible-tui", "venv", "bin")
	if err := os.MkdirAll(legacyBin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyBin, "python3.12"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}

	pathDir := t.TempDir()
	py3 := filepath.Join(pathDir, "python3")
	py312 := filepath.Join(pathDir, "python3.12")
	for _, p := range []string{py3, py312} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}

	var seenPython string
	deps := dependencies{
		resolvePython: audible.DiscoverPython,
		doctor: func(_ context.Context, python string) (audible.Diagnostics, error) {
			seenPython = python
			return ready(), nil
		},
		open: func(_ context.Context, _ string, python string) (account, error) {
			seenPython = python
			return &fakeAccount{}, nil
		},
		terminal: func() (*os.File, error) { return os.Open(os.DevNull) },
	}

	// 1. Explicit --python overrides TALETTY_PYTHON and PATH.
	t.Setenv("TALETTY_PYTHON", "/env/python")
	t.Setenv("PATH", pathDir)
	if code, _, errOut := invoke([]string{"doctor", "--python", "/explicit/python"}, deps); code != 0 || errOut != "" || seenPython != "/explicit/python" {
		t.Fatalf("explicit override failed: code=%d seen=%q err=%q", code, seenPython, errOut)
	}

	// 2. Invalid explicit --python never falls back to TALETTY_PYTHON or PATH and never echoes.
	for _, bad := range []string{"", "   ", "bad\x00secret"} {
		if code, out, errOut := invoke([]string{"doctor", "--python", bad}, deps); code != 2 || out != "" || !strings.Contains(errOut, "Usage:") || strings.Contains(errOut, "secret") {
			t.Fatalf("invalid explicit %q: code=%d out=%q err=%q", bad, code, out, errOut)
		}
	}

	// 3. TALETTY_PYTHON overrides PATH when --python is omitted.
	if code, _, errOut := invoke([]string{"doctor"}, deps); code != 0 || errOut != "" || seenPython != "/env/python" {
		t.Fatalf("TALETTY_PYTHON failed: code=%d seen=%q err=%q", code, seenPython, errOut)
	}

	// 4. PATH python3.12 wins over python3 when TALETTY_PYTHON is unset.
	t.Setenv("TALETTY_PYTHON", "")
	if code, _, errOut := invoke([]string{"doctor"}, deps); code != 0 || errOut != "" || seenPython != py312 {
		t.Fatalf("PATH python3.12 failed: code=%d seen=%q err=%q", code, seenPython, errOut)
	}

	// 5. PATH python3 fallback when python3.12 is absent.
	if err := os.Remove(py312); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := invoke([]string{"sync"}, deps); code != 0 || errOut != "" || seenPython != py3 {
		t.Fatalf("PATH python3 fallback failed: code=%d seen=%q err=%q", code, seenPython, errOut)
	}

	// 6. Missing runtime fails safely with runtime_missing exit 1 across all commands without creating root.
	freshHome := t.TempDir()
	t.Setenv("HOME", freshHome)
	t.Setenv("PATH", t.TempDir())
	for _, args := range [][]string{
		{"doctor"},
		{"doctor", "--json"},
		{"login"},
		{"sync"},
		{"sync", "--json"},
		{"download", "B000000001"},
		{"download", "B000000001", "--json"},
	} {
		machine := len(args) > 1 && args[len(args)-1] == "--json"
		code, out, errOut := invoke(args, deps)
		if code != 1 {
			t.Fatalf("%v: got exit %d, want 1 (out=%q err=%q)", args, code, out, errOut)
		}
		if machine {
			var payload struct {
				Failure failure `json:"failure"`
			}
			if json.Unmarshal([]byte(out), &payload) != nil || payload.Failure.Code != "runtime_missing" {
				t.Fatalf("%v: unexpected JSON failure %q", args, out)
			}
		} else if !strings.Contains(errOut, "Python runtime could not start.") {
			t.Fatalf("%v: unexpected plain failure %q", args, errOut)
		}
	}
	if entries, err := os.ReadDir(freshHome); err != nil || len(entries) != 0 {
		t.Fatalf("missing runtime created state in HOME: %v %v", entries, err)
	}
}
