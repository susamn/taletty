package app

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/profile"
	"github.com/susamn/taletty/app/internal/store"
	"github.com/susamn/taletty/app/internal/ui"
	"github.com/susamn/taletty/engine/connectors/audible"
	"github.com/susamn/taletty/engine/model"
	"golang.org/x/sys/unix"
)

func bridgeHelperExecutable(t *testing.T, mode string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "python3.12")
	script := "#!/bin/sh\nexport TALETTY_ENGINE_BRIDGE_HELPER=1\nexport TALETTY_BRIDGE_MODE=" + mode + "\nexec \"" + exe + "\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return wrapper
}

func openTestPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile("/dev/pts/"+itoa(n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
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

func TestEngineAccountDoctorLoginSyncAndOfflineReopen(t *testing.T) {
	home := t.TempDir()
	python := bridgeHelperExecutable(t, "ok")

	// OpenEngineWorkspace boots offline without invoking Python.
	coord, err := OpenEngineWorkspace(context.Background(), home, "/nonexistent-python-not-called-at-boot", Options{})
	if err != nil {
		t.Fatalf("OpenEngineWorkspace failed: %v", err)
	}
	if !coord.InitialRestore().OpenAccount {
		t.Fatal("expected onboarding on fresh workspace")
	}
	_ = coord.Close(context.Background())

	// Reopen with working bridge helper executable and run Doctor, Login, Sync.
	coord, err = OpenEngineWorkspace(context.Background(), home, python, Options{
		Handoff: func(ctx context.Context, fn func(*os.File) error) error {
			// Run Login in a foreground session child or directly if controlling TTY matches.
			// Here test EngineAccount.Login directly via a foreground PTY child or fake TTY error,
			// and test real RunTTY in a setsid child below.
			return fn(nil)
		},
	})
	if err != nil {
		t.Fatalf("OpenEngineWorkspace failed: %v", err)
	}
	defer func() { _ = coord.Close(context.Background()) }()

	// Doctor succeeds via real audible.Doctor.
	_ = coord.Dispatch(ui.Action{Kind: ui.AccountDoctor})()
	if coord.AccountState().Status != "Runtime ready" {
		t.Fatalf("unexpected Doctor status: %+v", coord.AccountState())
	}

	// Login with nil TTY fails safely with ErrTerminalRequired.
	_ = coord.Dispatch(ui.Action{Kind: ui.AccountLogin})()
	if coord.AccountState().Error != "Sign-in requires an owned terminal." {
		t.Fatalf("unexpected nil-TTY login error: %+v", coord.AccountState())
	}

	// Sync succeeds via real audible.OpenBorrowed + Client.Library and persists to SQLite.
	var progressCount int
	ws, err := profile.Open(context.Background(), home, 100)
	if err == nil {
		_ = ws.Close(context.Background())
		t.Fatal("expected profile.ErrBusy while Coordinator holds workspace")
	}
	_ = coord.Dispatch(ui.Action{Kind: ui.AccountSync})()
	if !coord.AccountState().SignedIn || len(coord.Snapshot().Books) != 1 || coord.Snapshot().Books[0].Title != "Engine Book" {
		t.Fatalf("unexpected Sync result: state=%+v snap=%+v", coord.AccountState(), coord.Snapshot())
	}
	_ = progressCount

	// Direct EngineAccount.Sync with progress callback.
	ws2Home := t.TempDir()
	ws2, err := profile.Open(context.Background(), ws2Home, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws2.Close(context.Background()) }()
	acc := NewEngineAccount(ws2.Owner(), python)
	var stages []string
	books, err := acc.Sync(context.Background(), "op-sync-1", func(p SyncProgress) {
		stages = append(stages, p.Stage)
	})
	if err != nil || len(books) != 1 || len(stages) == 0 {
		t.Fatalf("EngineAccount.Sync failed: books=%+v stages=%v err=%v", books, stages, err)
	}
	if err := acc.Close(context.Background()); err != nil {
		t.Fatalf("EngineAccount.Close failed: %v", err)
	}

	// Close first coordinator and reopen completely offline without Python.
	if err := coord.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("TALETTY_PYTHON", "")
	offline, err := OpenEngineWorkspace(context.Background(), home, "", Options{})
	if err != nil {
		t.Fatalf("offline OpenEngineWorkspace failed: %v", err)
	}
	defer func() { _ = offline.Close(context.Background()) }()
	if len(offline.InitialSnapshot().Books) != 1 || offline.InitialSnapshot().Books[0].Title != "Engine Book" {
		t.Fatalf("offline reopen lost synced book: %+v", offline.InitialSnapshot())
	}
	// Doctor fails cleanly with runtime_missing when Python is absent.
	_ = offline.Dispatch(ui.Action{Kind: ui.AccountDoctor})()
	if offline.AccountState().Error != "Python 3.12 or Audible runtime is missing." {
		t.Fatalf("unexpected missing-runtime error: %+v", offline.AccountState())
	}
}

func TestEngineAccountRealPTYLoginAndFailures(t *testing.T) {
	if os.Getenv("TALETTY_PTY_LOGIN_CHILD") == "1" {
		home := os.Getenv("TALETTY_PTY_HOME")
		python := os.Getenv("TALETTY_PTY_PYTHON")
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			os.Exit(11)
		}
		defer tty.Close()
		ws, err := profile.Open(context.Background(), home, 100)
		if err != nil {
			os.Exit(12)
		}
		defer func() { _ = ws.Close(context.Background()) }()
		acc := NewEngineAccount(ws.Owner(), python)
		if err := acc.Login(context.Background(), tty); err != nil {
			os.Exit(13)
		}
		os.Exit(0)
	}

	// Run real audible.OpenBorrowed + Client.Login inside a controlling PTY session.
	master, slave := openTestPTY(t)
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, master)
		close(drained)
	}()
	t.Cleanup(func() {
		_ = slave.Close()
		_ = master.Close()
		<-drained
	})
	home := t.TempDir()
	python := bridgeHelperExecutable(t, "ok")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestEngineAccountRealPTYLoginAndFailures$")
	cmd.Env = append(os.Environ(),
		"TALETTY_PTY_LOGIN_CHILD=1",
		"TALETTY_PTY_HOME="+home,
		"TALETTY_PTY_PYTHON="+python,
		"GORACE=atexit_sleep_ms=0",
	)
	cmd.ExtraFiles = []*os.File{slave}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 3}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("real PTY Login child failed: %v\n%s", err, out)
	}
	info, err := os.Stat(filepath.Join(home, ".config", "taletty", "auth.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("expected private 0600 auth.json after Login, got err=%v info=%v", err, info)
	}

	// Test throttled Sync and incompatible Doctor via real bridge helper.
	ws, err := profile.Open(context.Background(), home, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close(context.Background()) }()

	throttledAcc := NewEngineAccount(ws.Owner(), bridgeHelperExecutable(t, "library-throttled"))
	_, err = throttledAcc.Sync(context.Background(), "op-throttle", nil)
	var af *AccountFailure
	if !errors.As(err, &af) || af.Code != "throttled" {
		t.Fatalf("expected throttled AccountFailure, got %v", err)
	}

	// Missing Python and closed owner lease errors on Login and Sync.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("TALETTY_PYTHON", "")
	missingAcc := NewEngineAccount(ws.Owner(), "")
	if err := missingAcc.Login(context.Background(), slave); !errors.As(err, &af) || af.Code != "runtime_missing" {
		t.Fatalf("expected runtime_missing on Login, got %v", err)
	}
	if _, err := missingAcc.Sync(context.Background(), "op-missing", nil); !errors.As(err, &af) || af.Code != "runtime_missing" {
		t.Fatalf("expected runtime_missing on Sync, got %v", err)
	}

	// Injected openBorrowed / client.Close / lease errors on Login and Sync.
	fakeBorrowed := &fakeBorrowedClient{closeErr: errors.New("close fail")}
	errAcc := NewEngineAccount(ws.Owner(), python)
	errAcc.openBorrowed = func(context.Context, *os.File, string) (borrowedClient, error) {
		return fakeBorrowed, nil
	}
	if err := errAcc.Login(context.Background(), slave); !errors.As(err, &af) || af.Code != "internal" {
		t.Fatalf("expected close error on Login, got %v", err)
	}
	if _, err := errAcc.Sync(context.Background(), "op-close-err", nil); !errors.As(err, &af) || af.Code != "internal" {
		t.Fatalf("expected close error on Sync, got %v", err)
	}
	errAcc.openBorrowed = func(context.Context, *os.File, string) (borrowedClient, error) {
		return nil, audible.ErrAuthStorage
	}
	if err := errAcc.Login(context.Background(), slave); !errors.As(err, &af) || af.Code != "storage_failed" {
		t.Fatalf("expected openBorrowed error on Login, got %v", err)
	}
	if _, err := errAcc.Sync(context.Background(), "op-open-err", nil); !errors.As(err, &af) || af.Code != "storage_failed" {
		t.Fatalf("expected openBorrowed error on Sync, got %v", err)
	}
	canceledCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if err := errAcc.Login(canceledCtx, slave); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on Login lease, got %v", err)
	}
	if _, err := errAcc.Sync(canceledCtx, "op-cancel", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on Sync lease, got %v", err)
	}

	incompatAcc := NewEngineAccount(ws.Owner(), bridgeHelperExecutable(t, "doctor-incompatible"))
	diag, err := incompatAcc.Doctor(context.Background())
	if err != nil || diag.Python != "runtime_incompatible" || diag.Ready() || diag.LoginReady() {
		t.Fatalf("expected runtime_incompatible Diagnostics, got diag=%+v err=%v", diag, err)
	}

	malformedAcc := NewEngineAccount(ws.Owner(), bridgeHelperExecutable(t, "doctor-malformed"))
	if _, err := malformedAcc.Doctor(context.Background()); !errors.As(err, &af) || af.Code != "runtime_incompatible" {
		t.Fatalf("expected runtime_incompatible AccountFailure, got %v", err)
	}

	// Test real TALETTY_TEST_PYTHON Doctor if configured in environment.
	if realPy := os.Getenv("TALETTY_TEST_PYTHON"); realPy != "" {
		realAcc := NewEngineAccount(ws.Owner(), realPy)
		diag, err := realAcc.Doctor(context.Background())
		if err != nil || !diag.LoginReady() {
			t.Fatalf("real TALETTY_TEST_PYTHON Doctor failed: diag=%+v err=%v", diag, err)
		}
	}
}

func TestRunSessionSuspendResumeLoginAndQuit(t *testing.T) {
	home := t.TempDir()
	ws, err := profile.Open(context.Background(), home, 100)
	if err != nil {
		t.Fatal(err)
	}
	var loginCalls int
	account := &fakeAccountForSession{
		loginFn: func(ctx context.Context, tty *os.File) error {
			loginCalls++
			return nil
		},
		syncFn: func(ctx context.Context, opID string, report func(SyncProgress)) ([]store.CatalogBook, error) {
			return []store.CatalogBook{{
				Key:   store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"},
				Title: "Session Book",
			}}, nil
		},
	}
	coord, err := NewCoordinator(context.Background(), ws.Store(), Options{
		Account:         account,
		SuspendForLogin: true,
		WorkspaceCloser: ws.Close,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = coord.Close(context.Background()) }()

	tty, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()

	runCount := 0
	err = RunSession(context.Background(), coord, tty, io.Discard, func(ctx context.Context, m tea.Model, _ *os.File, _ io.Writer) (tea.Model, error) {
		runCount++
		next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
		if runCount == 1 {
			// First run: onboarding Account overlay is open; press Enter to trigger Login suspension.
			var cmd tea.Cmd
			next, cmd = next.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			if cmd == nil {
				t.Fatal("expected tea.Quit cmd for Login suspension")
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Fatalf("expected tea.QuitMsg, got %T", cmd())
			}
			return next, nil
		}
		// Second run: resumed after Login; verify SignedIn, run Sync, close Account, quit.
		if !coord.AccountState().SignedIn {
			t.Fatal("expected SignedIn after suspended Login")
		}
		var cmd tea.Cmd
		next, cmd = next.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
		next, cmd = next.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if cmd != nil {
			if batch, ok := cmd().(tea.BatchMsg); ok {
				for _, sub := range batch {
					next, _ = next.Update(sub())
				}
			}
		}
		next, _ = next.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
		next, _ = next.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
		return next, nil
	})
	if err != nil {
		t.Fatalf("RunSession failed: %v", err)
	}
	if runCount != 2 || loginCalls != 1 {
		t.Fatalf("unexpected runCount=%d loginCalls=%d", runCount, loginCalls)
	}
	if err := coord.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Verify ViewState persisted on clean quit.
	ws2, err := profile.Open(context.Background(), home, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws2.Close(context.Background()) }()
	vs, err := ws2.Store().ViewState(context.Background())
	if err != nil || vs.Selected == nil || vs.Selected.ID != "B000000001" {
		t.Fatalf("expected persisted ViewState selection, got %+v err=%v", vs, err)
	}
}

func TestEngineConversionsErrorMappingAndSessionGuards(t *testing.T) {
	// convertCatalogBooks valid and invalid cases.
	dur := 5 * time.Minute
	sub := "Subtitle"
	converted, err := convertCatalogBooks([]model.Book{{
		ID:        model.BookID{Provider: "audible", Namespace: "us", ID: "B000000001"},
		Title:     "Valid",
		Subtitle:  &sub,
		Authors:   []model.Contributor{{Name: "Author"}},
		Narrators: []model.Contributor{},
		Duration:  &dur,
	}})
	if err != nil || len(converted) != 1 || *converted[0].Subtitle != "Subtitle" || *converted[0].DurationNS != int64(dur) || converted[0].Narrators == nil {
		t.Fatalf("convertCatalogBooks failed: %+v err=%v", converted, err)
	}
	if _, err := convertCatalogBooks([]model.Book{{Title: "Missing ID"}}); err == nil {
		t.Fatal("expected error for invalid model.Book")
	}
	if convertContributors(nil) != nil {
		t.Fatal("expected nil contributors for nil input")
	}
	// Valid model.Book with nil Subtitle/Authors/Narrators/Duration.
	minimal, err := convertCatalogBooks([]model.Book{{
		ID:    model.BookID{Provider: "audible", Namespace: "us", ID: "B000000002"},
		Title: "Minimal",
	}})
	if err != nil || len(minimal) != 1 || minimal[0].Authors != nil {
		t.Fatalf("minimal convertCatalogBooks failed: %+v err=%v", minimal, err)
	}

	// mapEngineError covers all engine error types.
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{context.Canceled, "Operation canceled."},
		{context.DeadlineExceeded, "Operation timed out."},
		{store.ErrClosed, "Application session is closed."},
		{audible.ErrTerminal, "Sign-in requires an owned terminal."},
		{audible.ErrRuntimeMissing, "Python 3.12 or Audible runtime is missing."},
		{audible.ErrRuntimeIncompatible, "Python or Audible runtime is incompatible."},
		{audible.ErrAuthBusy, "Another account operation is already active."},
		{audible.ErrAuthStorage, "Credentials could not be saved."},
		{&audible.LoginError{Code: "storage_uncertain"}, "Credentials published; durability not confirmed."},
		{&model.Failure{Code: "network", Retry: "user_retry"}, "Network error while contacting Audible."},
		{errors.New("unexpected"), "Account operation failed."},
	} {
		got := SanitizeError(mapEngineError(tc.err))
		if got != tc.want {
			t.Fatalf("mapEngineError(%v) -> %q, want %q", tc.err, got, tc.want)
		}
	}

	// Nil/invalid guards on EngineAccount, OpenEngineWorkspace, RunSession, ExecutePendingLogin.
	var nilAcc *EngineAccount
	if _, err := nilAcc.Doctor(context.Background()); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
	if err := nilAcc.Login(context.Background(), nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
	if _, err := nilAcc.Sync(context.Background(), "op", nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
	if _, err := OpenEngineWorkspace(nil, t.TempDir(), "", Options{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
	if _, err := OpenEngineWorkspace(context.Background(), "relative", "", Options{}); err == nil {
		t.Fatal("expected error for relative home")
	}
	if err := RunSession(nil, nil, nil, nil, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}

	// ExecutePendingLogin error paths and RunSession error/cancel paths.
	home := t.TempDir()
	ws, err := profile.Open(context.Background(), home, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close(context.Background()) }()
	coord, err := NewCoordinator(context.Background(), ws.Store(), Options{SuspendForLogin: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = coord.Close(context.Background()) }()
	coord.ExecutePendingLogin(nil, nil)
	if !strings.Contains(coord.AccountState().Error, "missing") {
		t.Fatalf("expected missing runtime error, got %+v", coord.AccountState())
	}
	// ExecutePendingLogin with nil tty and login failure, plus SuspendForLogin busy guard.
	failAcc := &fakeAccountForSession{
		loginFn: func(context.Context, *os.File) error { return &AccountFailure{Code: "canceled"} },
	}
	coord2, err := NewCoordinator(context.Background(), ws.Store(), Options{Account: failAcc, SuspendForLogin: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = coord2.Close(context.Background()) }()
	coord2.ExecutePendingLogin(context.Background(), nil)
	if coord2.AccountState().Error != "Sign-in requires an owned terminal." {
		t.Fatalf("expected terminal required error, got %+v", coord2.AccountState())
	}
	tty, _ := os.Open(os.DevNull)
	defer tty.Close()
	coord2.ExecutePendingLogin(context.Background(), tty)
	if coord2.AccountState().Error != "Operation canceled." {
		t.Fatalf("expected canceled error, got %+v", coord2.AccountState())
	}
	_ = coord2.Close(context.Background())
	coord2.ExecutePendingLogin(context.Background(), tty)
	if msg := coord2.Dispatch(ui.Action{Kind: ui.AccountLogin})(); msg == nil {
		t.Fatal("expected closed notice on SuspendForLogin Dispatch after Close")
	}
	runErr := errors.New("runner failed")
	if err := RunSession(context.Background(), coord, tty, io.Discard, func(context.Context, tea.Model, *os.File, io.Writer) (tea.Model, error) {
		return nil, runErr
	}); !errors.Is(err, runErr) {
		t.Fatalf("expected runner error, got %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunSession(canceled, coord, tty, io.Discard, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

type fakeAccountForSession struct {
	loginFn func(context.Context, *os.File) error
	syncFn  func(context.Context, string, func(SyncProgress)) ([]store.CatalogBook, error)
}

func (f *fakeAccountForSession) Doctor(context.Context) (Diagnostics, error) {
	return Diagnostics{Python: "ready", Audible: "ready", FFmpeg: "ready", FFprobe: "ready"}, nil
}
func (f *fakeAccountForSession) Login(ctx context.Context, tty *os.File) error {
	return f.loginFn(ctx, tty)
}
func (f *fakeAccountForSession) Sync(ctx context.Context, opID string, report func(SyncProgress)) ([]store.CatalogBook, error) {
	return f.syncFn(ctx, opID, report)
}
func (f *fakeAccountForSession) Close(context.Context) error { return nil }

type fakeBorrowedClient struct {
	loginErr error
	syncErr  error
	closeErr error
}

func (f *fakeBorrowedClient) Login(context.Context, *os.File) error { return f.loginErr }
func (f *fakeBorrowedClient) Library(context.Context, string, func(model.OperationEvent)) (model.LibrarySnapshot, error) {
	return model.LibrarySnapshot{}, f.syncErr
}
func (f *fakeBorrowedClient) Close(context.Context) error { return f.closeErr }
