package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/ncruces/go-sqlite3"
	"github.com/susamn/taletty/app/internal/app"
	"github.com/susamn/taletty/app/internal/profile"
	"github.com/susamn/taletty/app/internal/store"
	"github.com/susamn/taletty/app/internal/testkit"
	"github.com/susamn/taletty/app/internal/ui"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if os.Getenv("TALETTY_ENGINE_BRIDGE_HELPER") == "1" {
		runBridgeHelper()
		os.Exit(0)
	}
	path := os.Getenv("TALETTY_SQLITE_TEST_CACHE")
	owned := path == ""
	if owned {
		var err error
		path, err = os.MkdirTemp("", "taletty-app-sqlite-compiler-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Setenv("TALETTY_SQLITE_TEST_CACHE", path)
	}
	code := runAppTests(m, path)
	if owned {
		os.RemoveAll(path)
	}
	os.Exit(code)
}

func runAppTests(m *testing.M, path string) int {
	cache, err := wazero.NewCompilationCacheWithDir(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	sqlite3.RuntimeConfig = wazero.NewRuntimeConfig().WithMemoryLimitPages(4096).
		WithCoreFeatures(api.CoreFeaturesV2).WithCompilationCache(cache)
	return m.Run()
}

func runBridgeHelper() {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(1)
	}
	var req struct {
		Protocol    int    `json:"protocol"`
		OperationID string `json:"operation_id"`
		Action      string `json:"action"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		os.Exit(1)
	}
	mode := os.Getenv("TALETTY_BRIDGE_MODE")
	switch req.Action {
	case "doctor":
		if mode == "doctor-incompatible" {
			fmt.Printf(`{"protocol":1,"operation_id":%q,"seq":1,"type":"result","python":"runtime_incompatible","audible":"not_checked","ffmpeg":"not_checked","ffprobe":"not_checked"}`+"\n", req.OperationID)
			return
		}
		if mode == "doctor-malformed" {
			fmt.Println("not-json")
			return
		}
		fmt.Printf(`{"protocol":1,"operation_id":%q,"seq":1,"type":"result","python":"ready","audible":"ready","ffmpeg":"ready","ffprobe":"ready"}`+"\n", req.OperationID)
	case "login":
		if mode == "login-fail" {
			fmt.Printf(`{"protocol":1,"operation_id":%q,"seq":1,"type":"error","failure":{"code":"login_failed"}}`+"\n", req.OperationID)
			return
		}
		// Write a fresh 0600 auth.json inside borrowed root FD 3 via atomic renameat.
		tmpName := ".auth-helper.tmp"
		fd, err := unix.Openat(3, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			os.Exit(1)
		}
		_, _ = unix.Write(fd, []byte(`{"synthetic":"fixture"}`))
		_ = unix.Close(fd)
		if err := unix.Renameat(3, tmpName, 3, "auth.json"); err != nil {
			os.Exit(1)
		}
		fmt.Printf(`{"protocol":1,"operation_id":%q,"seq":1,"type":"result","status":"signed_in"}`+"\n", req.OperationID)
	case "library":
		if mode == "library-throttled" {
			fmt.Printf(`{"protocol":1,"operation_id":%q,"seq":1,"type":"error","failure":{"code":"throttled","message":"Rate limited.","retry":"wait"}}`+"\n", req.OperationID)
			return
		}
		fmt.Printf(`{"protocol":1,"operation_id":%q,"seq":1,"type":"stage","stage":"library_page","books":[{"id":{"provider":"audible","namespace":"us","id":"B000000001"},"title":"Engine Book","subtitle":"Sub","authors":[{"name":"Engine Author"}],"narrators":[{"name":"Engine Narrator"}],"duration_ms":3600000}]}`+"\n", req.OperationID)
		fmt.Printf(`{"protocol":1,"operation_id":%q,"seq":2,"type":"result","count":1}`+"\n", req.OperationID)
	default:
		os.Exit(1)
	}
}

type fakeAccount struct {
	mu         sync.Mutex
	doctorFn   func(context.Context) (app.Diagnostics, error)
	loginFn    func(context.Context, *os.File) error
	syncFn     func(context.Context, string, func(app.SyncProgress)) ([]store.CatalogBook, error)
	closeFn    func(context.Context) error
	closeCalls int
}

func (f *fakeAccount) Doctor(ctx context.Context) (app.Diagnostics, error) {
	if f.doctorFn != nil {
		return f.doctorFn(ctx)
	}
	return app.Diagnostics{Python: "ready", Audible: "ready", FFmpeg: "ready", FFprobe: "ready"}, nil
}

func (f *fakeAccount) Login(ctx context.Context, tty *os.File) error {
	if f.loginFn != nil {
		return f.loginFn(ctx, tty)
	}
	return nil
}

func (f *fakeAccount) Sync(ctx context.Context, opID string, report func(app.SyncProgress)) ([]store.CatalogBook, error) {
	if f.syncFn != nil {
		return f.syncFn(ctx, opID, report)
	}
	return nil, nil
}

func (f *fakeAccount) Close(ctx context.Context) error {
	f.mu.Lock()
	f.closeCalls++
	fn := f.closeFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return nil
}

func sampleBooks() []store.CatalogBook {
	sub1 := "First subtitle"
	sub2 := "Second subtitle"
	dur1 := int64(3600 * time.Second)
	dur2 := int64(7200 * time.Second)
	return []store.CatalogBook{
		{
			Key:        store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"},
			Title:      "First Book",
			Subtitle:   &sub1,
			Authors:    []store.Contributor{{Name: "Author One"}},
			Narrators:  []store.Contributor{{Name: "Narrator One"}},
			DurationNS: &dur1,
		},
		{
			Key:        store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"},
			Title:      "Second Book",
			Subtitle:   &sub2,
			Authors:    []store.Contributor{{Name: "Author Two"}, {Name: "Coauthor"}},
			DurationNS: &dur2,
		},
	}
}

func runKey(t *testing.T, r *testkit.Runner, name string) {
	t.Helper()
	var msg tea.KeyPressMsg
	switch name {
	case "enter":
		msg = tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		msg = tea.KeyPressMsg{Code: tea.KeyEscape}
	case "down":
		msg = tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		msg = tea.KeyPressMsg{Code: tea.KeyUp}
	default:
		msg = tea.KeyPressMsg{Code: rune(name[0]), Text: name}
	}
	if err := r.Send(msg); err != nil {
		t.Fatal(err)
	}
}

func TestJ01FirstLaunchOnboardingLoginSyncAndJ02OfflineRestore(t *testing.T) {
	home := t.TempDir()
	legacyDir := filepath.Join(home, ".config", "audible-tui")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyFile := filepath.Join(legacyDir, "auth.json")
	if err := os.WriteFile(legacyFile, []byte(`{"legacy":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "ignored-xdg"))

	now := time.Unix(1700000000, 0)
	clock := func() time.Time { return now }

	loginAttempt := 0
	account := &fakeAccount{
		loginFn: func(ctx context.Context, tty *os.File) error {
			loginAttempt++
			switch loginAttempt {
			case 1:
				return context.Canceled
			case 2:
				return &app.AccountFailure{Code: "storage_uncertain"}
			default:
				return nil
			}
		},
		syncFn: func(ctx context.Context, opID string, report func(app.SyncProgress)) ([]store.CatalogBook, error) {
			return sampleBooks(), nil
		},
	}

	coord, err := app.OpenWorkspace(context.Background(), home, app.Options{
		Account: account,
		Handoff: func(ctx context.Context, fn func(*os.File) error) error {
			return fn(nil)
		},
		Clock: clock,
	})
	if err != nil {
		t.Fatalf("OpenWorkspace failed: %v", err)
	}

	if !coord.InitialRestore().OpenAccount || !coord.AccountState().Onboarding {
		t.Fatalf("expected first-launch onboarding state, got restore=%+v account=%+v", coord.InitialRestore(), coord.AccountState())
	}
	if len(coord.InitialSnapshot().Books) != 0 || coord.InitialSnapshot().Player.State != ui.Stopped {
		t.Fatalf("unexpected initial snapshot: %+v", coord.InitialSnapshot())
	}

	r, err := testkit.New(context.Background(), ui.NewModel(coord.InitialSnapshot(), coord.Dispatch), 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range coord.InitialMessages() {
		if err := r.Send(msg); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Send(tea.WindowSizeMsg{Width: 144, Height: 42}); err != nil {
		t.Fatal(err)
	}
	if !r.Model.(ui.Model).AccountOpen() {
		t.Fatal("expected onboarding Account overlay open on first launch")
	}

	// Attempt 1: canceled login.
	runKey(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if coord.AccountState().SignedIn || coord.AccountState().Error != "Operation canceled." {
		t.Fatalf("unexpected state after canceled login: %+v", coord.AccountState())
	}

	// Attempt 2: storage_uncertain login failure.
	runKey(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if coord.AccountState().Error != "Credentials published; durability not confirmed." {
		t.Fatalf("unexpected storage_uncertain error: %+v", coord.AccountState())
	}

	// Attempt 3: login succeeds, then sync populates cached library without autoplay.
	runKey(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !coord.AccountState().SignedIn || coord.AccountState().Onboarding {
		t.Fatalf("expected signed-in state after login: %+v", coord.AccountState())
	}

	runKey(t, r, "j")
	runKey(t, r, "enter") // Sync library
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	runKey(t, r, "esc") // Close Account overlay

	// Browse to second book and focus Details; verify no autoplay.
	runKey(t, r, "j")
	runKey(t, r, "3")
	model := r.Model.(ui.Model)
	wantKey, _ := app.EncodeBookKey(sampleBooks()[1].Key)
	if model.Selection() != wantKey || model.Focus() != ui.Details {
		t.Fatalf("unexpected selection/focus: %q %v", model.Selection(), model.Focus())
	}
	if strings.Contains(model.View().Content, "Player - Playing") {
		t.Fatal("unexpected autoplay after sync")
	}

	// Persist ViewState and close cleanly.
	if err := coord.SaveViewState(context.Background(), model.Focus(), model.Selection()); err != nil {
		t.Fatal(err)
	}
	if err := coord.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Seed local state (settings, favorite, listening checkpoint, local media) before J02 offline reopen.
	ws, err := profile.Open(context.Background(), home, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	st := ws.Store()
	if err := st.PutSettings(context.Background(), store.Settings{GlyphMode: store.GlyphPlain, VolumePercent: 65}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetFavorite(context.Background(), sampleBooks()[1].Key, true, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := st.PutListeningState(context.Background(), store.ListeningState{
		Key:         sampleBooks()[1].Key,
		PositionNS:  int64(42 * time.Second),
		SpeedMilli:  1_250_000,
		Completion:  store.CompletionUnfinished,
		UpdatedAtMS: now.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutLocalMedia(context.Background(), store.LocalMedia{
		Key:           sampleBooks()[1].Key,
		RelativePath:  "downloads/B000000002.m4b",
		Format:        "m4b",
		PublishedAtMS: now.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := ws.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// J02: Reopen completely offline (Account == nil, no Python/network).
	offlineCoord, err := app.OpenWorkspace(context.Background(), home, app.Options{Clock: clock})
	if err != nil {
		t.Fatalf("offline OpenWorkspace failed: %v", err)
	}
	defer func() { _ = offlineCoord.Close(context.Background()) }()

	restore := offlineCoord.InitialRestore()
	snap := offlineCoord.InitialSnapshot()
	if restore.OpenAccount || restore.Selected != wantKey || restore.Focus == nil || *restore.Focus != ui.Details {
		t.Fatalf("unexpected offline restore: %+v", restore)
	}
	if offlineCoord.InitialAppearance().Glyphs != ui.Plain {
		t.Fatalf("expected restored Plain glyph mode, got %+v", offlineCoord.InitialAppearance())
	}
	if len(snap.Books) != 2 || !snap.Books[1].Favorite || snap.Books[1].Availability != ui.Ready {
		t.Fatalf("unexpected restored books: %+v", snap.Books)
	}
	if snap.Player.Key != wantKey || snap.Player.State != ui.Stopped || snap.Player.Session != "" || snap.Player.Position != 42*time.Second || snap.Player.Volume != 65 {
		t.Fatalf("unexpected restored player (must not autoplay): %+v", snap.Player)
	}

	// Verify legacy ~/.config/audible-tui/ was never modified.
	legacyBytes, err := os.ReadFile(legacyFile)
	if err != nil || string(legacyBytes) != `{"legacy":true}` {
		t.Fatalf("legacy file touched: %v %q", err, legacyBytes)
	}
}

func TestSyncFailureStaleReplyBusyGuardAndCloseJoin(t *testing.T) {
	home := t.TempDir()
	ws, err := profile.Open(context.Background(), home, 100)
	if err != nil {
		t.Fatal(err)
	}
	st := ws.Store()
	books := sampleBooks()
	if err := st.ReplaceCatalog(context.Background(), "audible", "us", books, 100); err != nil {
		t.Fatal(err)
	}
	if err := st.PutDownloadJob(context.Background(), store.DownloadJob{
		ID: "job-1", Key: books[0].Key, QueueOrder: ptrInt64(0), State: store.JobWaiting, CreatedAtMS: 100, UpdatedAtMS: 100,
	}); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var syncCalls int
	account := &fakeAccount{
		syncFn: func(ctx context.Context, opID string, report func(app.SyncProgress)) ([]store.CatalogBook, error) {
			syncCalls++
			if syncCalls == 1 {
				return nil, &app.AccountFailure{Code: "throttled"}
			}
			close(started)
			<-release
			return books[:1], ctx.Err()
		},
	}

	coord, err := app.NewCoordinator(context.Background(), st, app.Options{
		Account:         account,
		WorkspaceCloser: ws.Close,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Failed sync preserves existing cached catalog.
	msg := coord.Dispatch(ui.Action{Kind: ui.AccountSync})()
	if _, ok := msg.(tea.BatchMsg); !ok {
		t.Fatalf("expected BatchMsg on sync failure, got %T", msg)
	}
	cached, err := st.Catalog(context.Background(), "audible", "us")
	if err != nil || len(cached) != 2 {
		t.Fatalf("failed sync altered cached catalog: %v %d", err, len(cached))
	}
	if coord.AccountState().Error != "Audible rate limit reached; wait before retrying." {
		t.Fatalf("unexpected throttled error: %+v", coord.AccountState())
	}

	// Stale sync reply is ignored without replacing catalog.
	staleMsg := coord.ApplySyncResult(context.Background(), "sync-stale", books[:1])
	if notice, ok := staleMsg.(ui.NoticeMsg); !ok || !strings.Contains(notice.Activity, "stale") {
		t.Fatalf("expected stale notice, got %#v", staleMsg)
	}
	cached, _ = st.Catalog(context.Background(), "audible", "us")
	if len(cached) != 2 {
		t.Fatalf("stale sync replaced catalog: %d", len(cached))
	}

	// Concurrent busy guard + Close waiting for active worker join.
	done := make(chan tea.Msg, 1)
	go func() {
		done <- coord.Dispatch(ui.Action{Kind: ui.AccountSync})()
	}()
	<-started

	busyMsg := coord.Dispatch(ui.Action{Kind: ui.AccountLogin})()
	if _, ok := busyMsg.(tea.BatchMsg); !ok {
		t.Fatalf("expected busy BatchMsg, got %T", busyMsg)
	}
	if coord.AccountState().Error != "Another Taletty session or account operation is already active." {
		t.Fatalf("unexpected busy error: %+v", coord.AccountState())
	}

	shortCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := coord.Close(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected Close timeout while worker active, got %v", err)
	}
	close(release)
	<-done

	if err := coord.Close(context.Background()); err != nil {
		t.Fatalf("retry Close failed: %v", err)
	}
	// Idempotent second Close.
	if err := coord.Close(context.Background()); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}
	// Reopen succeeds because Workspace was closed cleanly.
	reopened, err := app.OpenWorkspace(context.Background(), home, app.Options{})
	if err != nil {
		t.Fatalf("reopen after Close failed: %v", err)
	}
	_ = reopened.Close(context.Background())
}

func TestDoctorOfflineActionsAndErrorSanitization(t *testing.T) {
	home := t.TempDir()
	ws, err := profile.Open(context.Background(), home, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close(context.Background()) }()

	diagState := app.Diagnostics{Python: "ready", Audible: "ready", FFmpeg: "runtime_missing", FFprobe: "runtime_missing"}
	var doctorErr error
	account := &fakeAccount{
		doctorFn: func(context.Context) (app.Diagnostics, error) {
			return diagState, doctorErr
		},
	}
	coord, err := app.NewCoordinator(context.Background(), ws.Store(), app.Options{Account: account})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = coord.Close(context.Background()) }()

	// Doctor login-ready (missing media tools).
	_ = coord.Dispatch(ui.Action{Kind: ui.AccountDoctor})()
	if coord.AccountState().Status != "Login/sync ready (media tools missing)" {
		t.Fatalf("unexpected doctor status: %+v", coord.AccountState())
	}
	// Doctor fully ready.
	diagState = app.Diagnostics{Python: "ready", Audible: "ready", FFmpeg: "ready", FFprobe: "ready"}
	_ = coord.Dispatch(ui.Action{Kind: ui.AccountDoctor})()
	if coord.AccountState().Status != "Runtime ready" {
		t.Fatalf("unexpected ready doctor status: %+v", coord.AccountState())
	}
	// Doctor error.
	doctorErr = &app.AccountFailure{Code: "runtime_incompatible"}
	_ = coord.Dispatch(ui.Action{Kind: ui.AccountDoctor})()
	if coord.AccountState().Error != "Python or Audible runtime is incompatible." {
		t.Fatalf("unexpected doctor error: %+v", coord.AccountState())
	}

	// Continue offline and unsupported action.
	_ = coord.Dispatch(ui.Action{Kind: ui.AccountContinueOffline})()
	if coord.AccountState().Status != "Offline" {
		t.Fatalf("unexpected continue-offline status: %+v", coord.AccountState())
	}
	if msg := coord.Dispatch(ui.Action{Kind: ui.PlayBook})(); msg == nil {
		t.Fatal("expected notice for unsupported action")
	}
	if msg := coord.Dispatch(ui.Action{Kind: ui.RequestQuit})(); msg == nil {
		t.Fatal("expected tea.Quit cmd")
	}

	// Login without TerminalHandoff fails safely.
	_ = coord.Dispatch(ui.Action{Kind: ui.AccountLogin})()
	if coord.AccountState().Error != "Sign-in requires an owned terminal." {
		t.Fatalf("unexpected missing-handoff error: %+v", coord.AccountState())
	}

	// Offline coordinator (Account == nil) handles Doctor/Login/Sync safely.
	offline, err := app.NewCoordinator(context.Background(), ws.Store(), app.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = offline.Close(context.Background()) }()
	for _, kind := range []ui.ActionKind{ui.AccountDoctor, ui.AccountLogin, ui.AccountSync} {
		_ = offline.Dispatch(ui.Action{Kind: kind})()
		if offline.AccountState().Error != "Python 3.12 or Audible runtime is missing." {
			t.Fatalf("offline %s error mismatch: %+v", kind, offline.AccountState())
		}
	}

	// SanitizeError covers all codes without leaking raw text.
	var nilFailure *app.AccountFailure
	if got := nilFailure.Error(); got != "Account operation failed." {
		t.Fatalf("nil AccountFailure.Error() = %q", got)
	}
	for _, code := range []string{"busy", "not_authenticated", "runtime_missing", "runtime_incompatible", "network", "throttled", "entitlement_denied", "storage_failed", "storage_uncertain", "protocol_error", "canceled", "unknown"} {
		f := &app.AccountFailure{Code: code, Message: "SECRET_RAW_TEXT"}
		if got := f.Error(); got == "" || strings.Contains(got, "SECRET_RAW_TEXT") {
			t.Fatalf("AccountFailure.Error() leaked or emptied %q: %q", code, got)
		}
		if got := app.SanitizeError(f); got == "" || strings.Contains(got, "SECRET_RAW_TEXT") {
			t.Fatalf("SanitizeError leaked or emptied %q: %q", code, got)
		}
	}
	// Doctor incomplete (Python incompatible).
	diagState = app.Diagnostics{Python: "runtime_incompatible", Audible: "not_checked", FFmpeg: "not_checked", FFprobe: "not_checked"}
	doctorErr = nil
	_ = coord.Dispatch(ui.Action{Kind: ui.AccountDoctor})()
	if coord.AccountState().Status != "Runtime incomplete" {
		t.Fatalf("unexpected incomplete doctor status: %+v", coord.AccountState())
	}
	// Nil coordinator Dispatch and Close edge cases.
	var nilCoord *app.Coordinator
	if nilCoord.Dispatch(ui.Action{Kind: ui.AccountSync}) != nil {
		t.Fatal("nil Coordinator.Dispatch should return nil")
	}
	if err := nilCoord.Close(context.Background()); err != nil {
		t.Fatalf("nil Coordinator.Close failed: %v", err)
	}
	if err := coord.Close(nil); !errors.Is(err, app.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for nil Close ctx, got %v", err)
	}
	for _, err := range []error{
		nil,
		context.Canceled,
		context.DeadlineExceeded,
		app.ErrAccountBusy,
		profile.ErrBusy,
		profile.ErrUnsafeRoot,
		profile.ErrInvalidHome,
		profile.ErrIO,
		app.ErrTerminalRequired,
		app.ErrClosed,
		store.ErrClosed,
		app.ErrInvalidInput,
		store.ErrInvalidInput,
		errors.New("raw secret path /home/user/token"),
	} {
		got := app.SanitizeError(err)
		if strings.Contains(got, "/home/user/token") {
			t.Fatalf("SanitizeError leaked raw error: %q", got)
		}
	}
}

type faultStore struct {
	CatalogStore app.CatalogStore
	failStep     string
}

func (f *faultStore) Catalog(ctx context.Context, p, n string) ([]store.CatalogBook, error) {
	if f.failStep == "Catalog" {
		return nil, errors.New("db fail")
	}
	return f.CatalogStore.Catalog(ctx, p, n)
}
func (f *faultStore) ReplaceCatalog(ctx context.Context, p, n string, b []store.CatalogBook, ts int64) error {
	if f.failStep == "ReplaceCatalog" {
		return errors.New("db fail")
	}
	return f.CatalogStore.ReplaceCatalog(ctx, p, n, b, ts)
}
func (f *faultStore) SnapshotRefreshedAt(ctx context.Context, p, n string) (int64, bool, error) {
	if f.failStep == "SnapshotRefreshedAt" {
		return 0, false, errors.New("db fail")
	}
	return f.CatalogStore.SnapshotRefreshedAt(ctx, p, n)
}
func (f *faultStore) ListeningState(ctx context.Context, k store.BookKey) (store.ListeningState, bool, error) {
	if f.failStep == "ListeningState" {
		return store.ListeningState{}, false, errors.New("db fail")
	}
	return f.CatalogStore.ListeningState(ctx, k)
}
func (f *faultStore) ListeningStates(ctx context.Context) ([]store.ListeningState, error) {
	if f.failStep == "ListeningStates" {
		return nil, errors.New("db fail")
	}
	return f.CatalogStore.ListeningStates(ctx)
}
func (f *faultStore) Settings(ctx context.Context) (store.Settings, error) {
	if f.failStep == "Settings" {
		return store.Settings{}, errors.New("db fail")
	}
	return f.CatalogStore.Settings(ctx)
}
func (f *faultStore) PutSettings(ctx context.Context, s store.Settings) error {
	if f.failStep == "PutSettings" {
		return errors.New("db fail")
	}
	return f.CatalogStore.PutSettings(ctx, s)
}
func (f *faultStore) ViewState(ctx context.Context) (store.ViewState, error) {
	if f.failStep == "ViewState" {
		return store.ViewState{}, errors.New("db fail")
	}
	return f.CatalogStore.ViewState(ctx)
}
func (f *faultStore) PutViewState(ctx context.Context, s store.ViewState) error {
	if f.failStep == "PutViewState" {
		return errors.New("db fail")
	}
	return f.CatalogStore.PutViewState(ctx, s)
}
func (f *faultStore) Favorites(ctx context.Context) ([]store.BookKey, error) {
	if f.failStep == "Favorites" {
		return nil, errors.New("db fail")
	}
	return f.CatalogStore.Favorites(ctx)
}
func (f *faultStore) LocalMedia(ctx context.Context, k store.BookKey) (store.LocalMedia, bool, error) {
	if f.failStep == "LocalMedia" {
		return store.LocalMedia{}, false, errors.New("db fail")
	}
	return f.CatalogStore.LocalMedia(ctx, k)
}
func (f *faultStore) LocalMediaList(ctx context.Context) ([]store.LocalMedia, error) {
	if f.failStep == "LocalMediaList" {
		return nil, errors.New("db fail")
	}
	return f.CatalogStore.LocalMediaList(ctx)
}
func (f *faultStore) DownloadJobs(ctx context.Context) ([]store.DownloadJob, error) {
	if f.failStep == "DownloadJobs" {
		return nil, errors.New("db fail")
	}
	return f.CatalogStore.DownloadJobs(ctx)
}

func TestValidationFaultsAndKeyEncoding(t *testing.T) {
	if _, err := app.EncodeBookKey(store.BookKey{}); !errors.Is(err, app.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for empty BookKey, got %v", err)
	}
	for _, bad := range []string{"", "bad", "a:b", "audible:us:bad"} {
		if _, err := app.DecodeBookKey(bad); !errors.Is(err, app.ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for %q, got %v", bad, err)
		}
	}
	if _, err := app.OpenWorkspace(nil, t.TempDir(), app.Options{}); !errors.Is(err, app.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for nil ctx, got %v", err)
	}
	if _, err := app.OpenWorkspace(context.Background(), "relative", app.Options{}); !errors.Is(err, profile.ErrInvalidHome) {
		t.Fatalf("expected ErrInvalidHome, got %v", err)
	}
	if _, err := app.NewCoordinator(context.Background(), nil, app.Options{}); !errors.Is(err, app.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for nil store, got %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := app.NewCoordinator(canceled, &faultStore{}, app.Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	home := t.TempDir()
	ws, err := profile.Open(context.Background(), home, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close(context.Background()) }()

	// Busy profile lock returns profile.ErrBusy.
	if _, err := app.OpenWorkspace(context.Background(), home, app.Options{}); !errors.Is(err, profile.ErrBusy) {
		t.Fatalf("expected profile.ErrBusy, got %v", err)
	}

	// Seed selected book + listening state so ListeningState fault path is exercised.
	books := sampleBooks()
	if err := ws.Store().ReplaceCatalog(context.Background(), "audible", "us", books, 100); err != nil {
		t.Fatal(err)
	}
	if err := ws.Store().PutViewState(context.Background(), store.ViewState{
		Destination: store.DestinationAll,
		Selected:    &books[0].Key,
		Focus:       store.FocusLibrary,
		SortOrder:   store.SortTitle,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ws.Store().PutListeningState(context.Background(), store.ListeningState{
		Key: books[0].Key, PositionNS: 10, SpeedMilli: 1_000_000, Completion: store.CompletionFinished, UpdatedAtMS: 100,
	}); err != nil {
		t.Fatal(err)
	}

	for _, step := range []string{"Catalog", "SnapshotRefreshedAt", "Favorites", "LocalMediaList", "DownloadJobs", "Settings", "ViewState", "ListeningStates", "ListeningState"} {
		if _, err := app.NewCoordinator(context.Background(), &faultStore{CatalogStore: ws.Store(), failStep: step}, app.Options{}); err == nil {
			t.Fatalf("expected error at step %s", step)
		}
	}

	// SaveViewState validation and fault handling.
	fs := &faultStore{CatalogStore: ws.Store()}
	coord, err := app.NewCoordinator(context.Background(), fs, app.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, pane := range []ui.Pane{ui.Library, ui.Books, ui.Details, ui.Chapters, ui.PlayerPane} {
		if err := coord.SaveViewState(context.Background(), pane, ""); err != nil {
			t.Fatalf("SaveViewState(%v) failed: %v", pane, err)
		}
	}
	if err := coord.SaveViewState(context.Background(), ui.Books, "bad-key"); !errors.Is(err, app.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for bad key, got %v", err)
	}
	fs.failStep = "PutViewState"
	if err := coord.SaveViewState(context.Background(), ui.Books, ""); err == nil {
		t.Fatal("expected PutViewState error")
	}
	fs.failStep = "ReplaceCatalog"
	if msg := coord.ApplySyncResult(context.Background(), "sync-1", books); msg == nil {
		t.Fatal("expected error message on ReplaceCatalog failure")
	}
	canceledSync, cancelSync := context.WithCancel(context.Background())
	cancelSync()
	if msg := coord.ApplySyncResult(canceledSync, "sync-1", books); msg == nil {
		t.Fatal("expected error message on canceled ApplySyncResult")
	}
	if err := coord.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if msg := coord.Dispatch(ui.Action{Kind: ui.AccountSync})(); msg == nil {
		t.Fatal("expected closed notice on Dispatch after Close")
	}
	if err := coord.SaveViewState(context.Background(), ui.Books, ""); !errors.Is(err, app.ErrClosed) {
		t.Fatalf("expected ErrClosed after Close, got %v", err)
	}
	if err := coord.SaveFullViewState(context.Background(), ui.ViewAll, ui.SortTitle, "", ui.Books, ""); !errors.Is(err, app.ErrClosed) {
		t.Fatalf("expected ErrClosed after Close on SaveFullViewState, got %v", err)
	}
	if err := (*app.Coordinator)(nil).SaveFullViewState(context.Background(), ui.ViewAll, ui.SortTitle, "", ui.Books, ""); !errors.Is(err, app.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for nil SaveFullViewState, got %v", err)
	}
}

func TestJ03CoordinatorViewStatePersistenceAndMetadataProjection(t *testing.T) {
	home := t.TempDir()
	ws, err := profile.Open(context.Background(), home, 100)
	if err != nil {
		t.Fatal(err)
	}
	st := ws.Store()
	books := sampleBooks()
	if err := st.ReplaceCatalog(context.Background(), "audible", "us", books, 100); err != nil {
		t.Fatal(err)
	}
	if err := st.PutListeningState(context.Background(), store.ListeningState{
		Key: books[0].Key, PositionNS: int64(15 * time.Second), SpeedMilli: 1_000_000, Completion: store.CompletionUnfinished, UpdatedAtMS: 500,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutListeningState(context.Background(), store.ListeningState{
		Key: books[1].Key, PositionNS: int64(7200 * time.Second), SpeedMilli: 1_000_000, Completion: store.CompletionFinished, UpdatedAtMS: 900,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ws.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	coord, err := app.OpenWorkspace(context.Background(), home, app.Options{})
	if err != nil {
		t.Fatal(err)
	}
	snap := coord.InitialSnapshot()
	if len(snap.Books) != 2 || !snap.Books[0].InProgress || !snap.Books[1].Finished || snap.Books[0].Narrator != "Narrator One" || !snap.Books[0].HasDuration {
		t.Fatalf("unexpected projected metadata: %+v", snap.Books)
	}

	enc1, _ := app.EncodeBookKey(books[1].Key)
	for _, tc := range []struct {
		view ui.LibraryView
		sort ui.SortOrder
	}{
		{ui.ViewContinue, ui.SortAuthor},
		{ui.ViewDownloaded, ui.SortDuration},
		{ui.ViewFinished, ui.SortRecent},
		{ui.ViewAll, ui.SortAcquisition},
	} {
		if err := coord.SaveFullViewState(context.Background(), tc.view, tc.sort, "narrator", ui.Books, enc1); err != nil {
			t.Fatalf("SaveFullViewState(%v, %v) failed: %v", tc.view, tc.sort, err)
		}
	}
	if err := coord.SaveFullViewState(context.Background(), ui.ViewContinue, ui.SortRecent, "author", ui.Library, enc1); err != nil {
		t.Fatal(err)
	}
	if err := coord.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	reopened, err := app.OpenWorkspace(context.Background(), home, app.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close(context.Background()) }()
	rest := reopened.InitialRestore()
	if rest.View == nil || *rest.View != ui.ViewContinue || rest.Sort == nil || *rest.Sort != ui.SortRecent || rest.Filter == nil || *rest.Filter != "author" || rest.Selected != enc1 {
		t.Fatalf("unexpected restored ViewState: %+v", rest)
	}
}

func TestProjectionsAllJobStatesFocusPanesAndCloseErrors(t *testing.T) {
	home := t.TempDir()
	ws, err := profile.Open(context.Background(), home, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close(context.Background()) }()

	st := ws.Store()
	var books []store.CatalogBook
	states := []store.JobState{
		store.JobWaiting, store.JobDownloading,
		store.JobReady, store.JobFailed, store.JobCanceled,
	}
	for i, state := range states {
		id := "B00000001" + string(rune('0'+i))
		key := store.BookKey{Provider: "audible", Namespace: "us", ID: id}
		books = append(books, store.CatalogBook{Key: key, Title: "Book " + id})
		var qOrder *int64
		var failCode store.FailureCode
		if state == store.JobWaiting {
			qOrder = ptrInt64(int64(i))
		}
		if state == store.JobFailed {
			failCode = store.FailureNetwork
		}
		if err := st.PutDownloadJob(context.Background(), store.DownloadJob{
			ID: "job-" + id, Key: key, QueueOrder: qOrder, State: state, FailureCode: failCode, CreatedAtMS: 100, UpdatedAtMS: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.ReplaceCatalog(context.Background(), "audible", "us", books, 100); err != nil {
		t.Fatal(err)
	}

	for _, focus := range []store.FocusPane{store.FocusLibrary, store.FocusBooks, store.FocusDetails, store.FocusChapters, store.FocusPlayer} {
		if err := st.PutViewState(context.Background(), store.ViewState{
			Destination: store.DestinationAll,
			Selected:    &books[0].Key,
			Focus:       focus,
			SortOrder:   store.SortTitle,
		}); err != nil {
			t.Fatal(err)
		}
		coord, err := app.NewCoordinator(context.Background(), st, app.Options{})
		if err != nil {
			t.Fatal(err)
		}
		snap := coord.InitialSnapshot()
		if len(snap.Books) != len(states) || snap.Books[0].Author != "Unknown author" {
			t.Fatalf("unexpected projected books: %+v", snap.Books)
		}
		wantAvail := []ui.Availability{ui.Queued, ui.Downloading, ui.Ready, ui.Failed, ui.Canceled}
		for i, want := range wantAvail {
			if snap.Books[i].Availability != want {
				t.Fatalf("book %d availability = %v, want %v", i, snap.Books[i].Availability, want)
			}
		}
		_ = coord.Close(context.Background())
	}
	// Also verify JobPreparing projection by advancing the single active job slot.
	if err := st.PutDownloadJob(context.Background(), store.DownloadJob{
		ID: "job-B000000011", Key: books[1].Key, State: store.JobPreparing, CreatedAtMS: 100, UpdatedAtMS: 200,
	}); err != nil {
		t.Fatal(err)
	}
	prepCoord, err := app.NewCoordinator(context.Background(), st, app.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if prepCoord.InitialSnapshot().Books[1].Availability != ui.Preparing {
		t.Fatalf("expected Preparing availability, got %v", prepCoord.InitialSnapshot().Books[1].Availability)
	}
	_ = prepCoord.Close(context.Background())

	// Selected book without ListeningState leaves Player empty (no fabricated audio).
	if err := st.PutViewState(context.Background(), store.ViewState{
		Destination: store.DestinationAll,
		Selected:    &books[2].Key,
		Focus:       store.FocusBooks,
		SortOrder:   store.SortTitle,
	}); err != nil {
		t.Fatal(err)
	}
	noListenCoord, err := app.NewCoordinator(context.Background(), st, app.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if noListenCoord.InitialSnapshot().Player.Key != "" {
		t.Fatalf("expected empty player without ListeningState, got %+v", noListenCoord.InitialSnapshot().Player)
	}
	_ = noListenCoord.Close(context.Background())

	// Selected book with ListeningState but absent from catalog uses Unavailable title fallback.
	orphanKey := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000099"}
	if err := st.PutListeningState(context.Background(), store.ListeningState{
		Key: orphanKey, PositionNS: 5, SpeedMilli: 1_000_000, Completion: store.CompletionUnfinished, UpdatedAtMS: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutViewState(context.Background(), store.ViewState{
		Destination: store.DestinationAll,
		Selected:    &orphanKey,
		Focus:       store.FocusBooks,
		SortOrder:   store.SortTitle,
	}); err != nil {
		t.Fatal(err)
	}
	orphanCoord, err := app.NewCoordinator(context.Background(), st, app.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if orphanCoord.InitialSnapshot().Player.Title != "Unavailable title" {
		t.Fatalf("expected Unavailable title fallback, got %+v", orphanCoord.InitialSnapshot().Player)
	}
	_ = orphanCoord.Close(context.Background())

	// Account.Close and WorkspaceCloser error propagation.
	closeErr := errors.New("close failed")
	acc := &fakeAccount{closeFn: func(context.Context) error { return closeErr }}
	coord, err := app.NewCoordinator(context.Background(), st, app.Options{
		Account:         acc,
		WorkspaceCloser: func(context.Context) error { return closeErr },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coord.Close(context.Background()); !errors.Is(err, closeErr) {
		t.Fatalf("expected account close error, got %v", err)
	}
	acc.closeFn = nil
	if err := coord.Close(context.Background()); !errors.Is(err, closeErr) {
		t.Fatalf("expected workspace close error, got %v", err)
	}
}

func ptrInt64(v int64) *int64 { return &v }
