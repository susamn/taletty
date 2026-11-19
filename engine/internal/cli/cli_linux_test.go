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
)

type fakeAccount struct {
	loginErr, closeErr error
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
		doctor: func(context.Context, string) (audible.Diagnostics, error) { return ready(), nil },
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
	for _, args := range [][]string{nil, {"sync"}, {"doctor"}, {"login", "--python", "fixture-python", "--json"},
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
