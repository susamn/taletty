package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/playback"
	"github.com/susamn/taletty/app/internal/profile"
	"github.com/susamn/taletty/app/internal/store"
	"github.com/susamn/taletty/app/internal/testkit"
	"github.com/susamn/taletty/app/internal/theme"
	"github.com/susamn/taletty/app/internal/ui"
	"golang.org/x/sys/unix"
)

type faultSettingsStore struct {
	*store.Store
	putSettingsErr error
}

func (f *faultSettingsStore) PutSettings(ctx context.Context, s store.Settings) error {
	if f.putSettingsErr != nil {
		return f.putSettingsErr
	}
	return f.Store.PutSettings(ctx, s)
}

func TestJ08J13ThemeSettingsOmarchySymlinkAndSignalReload(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	now := time.Unix(1_700_000_000, 0)

	// Seed fake Omarchy theme symlink BEFORE opening workspace -> SeedDefaults must preserve symlink!
	omarchyDir := t.TempDir()
	omarchyColors := filepath.Join(omarchyDir, "colors.toml")
	if err := os.WriteFile(omarchyColors, []byte("background = \"#101010\"\nforeground = \"#eeeeee\"\naccent = \"#11aa99\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	talettyDir := filepath.Join(home, ".config", "taletty")
	if err := os.MkdirAll(filepath.Join(home, ".config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(talettyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(omarchyColors, filepath.Join(talettyDir, "colors.toml")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(talettyDir, "themes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(talettyDir, "themes", "nord.toml"), []byte("focus = \"#88c0d0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ws, err := profile.Open(ctx, home, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	st := ws.Store()
	kA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B00ALPHA01"}
	_ = st.ReplaceCatalog(ctx, "audible", "us", []store.CatalogBook{{Key: kA, Title: "Alpha"}}, now.UnixMilli())
	pathA := filepath.Join(ws.DownloadsPath(), "alpha.m4b")
	_ = os.WriteFile(pathA, []byte("fake-m4b"), 0o600)
	_ = st.PutLocalMedia(ctx, store.LocalMedia{Key: kA, RelativePath: "downloads/alpha.m4b", Format: "m4b", PublishedAtMS: now.UnixMilli()})

	backend := &fakePlaybackBackend{}
	player, _ := playback.NewController(backend, playback.Options{Store: st})
	faultSt := &faultSettingsStore{Store: st}
	coord, err := NewCoordinator(ctx, faultSt, Options{
		Player:          player,
		MediaChecker:    NewRootMediaChecker(ws.Owner()),
		RootOwner:       ws.Owner(),
		Clock:           func() time.Time { return now },
		WorkspaceCloser: ws.Close,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Symlink preserved and Omarchy accent loaded!
	if target, err := os.Readlink(filepath.Join(talettyDir, "colors.toml")); err != nil || target != omarchyColors {
		t.Fatalf("workspace open overwrote Omarchy symlink: target=%q err=%v", target, err)
	}
	if theme.FormatHexColor(coord.InitialAppearance().Palette.Focus) != "#11aa99" {
		t.Fatalf("expected Omarchy accent #11aa99, got %s", theme.FormatHexColor(coord.InitialAppearance().Palette.Focus))
	}

	r, err := testkit.New(ctx, ui.NewModel(coord.InitialSnapshot(), coord.Dispatch), 1000)
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

	// 1. Start playback and open a bookmark note draft; Omarchy updates colors.toml and sends signal reload!
	// Reload must NOT interrupt playback, steal focus, or discard the editor draft!
	sendUIKeys(t, r, "enter")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "b")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if err := r.Send(tea.PasteMsg{Content: "Keep this draft"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(omarchyColors, []byte("focus = \"#22bbcc\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sigMsg := coord.HandleThemeSignal(ctx, coord.ThemeGeneration())
	if batch, ok := sigMsg.(tea.BatchMsg); ok {
		for _, fn := range batch {
			if err := r.Send(fn()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !strings.Contains(r.Model.View().Content, "Keep this draft") || player.Snapshot().State != playback.StatePlaying {
		t.Fatalf("signal reload disrupted editor draft or playback:\n%s", r.Model.View().Content)
	}
	if theme.FormatHexColor(coord.InitialAppearance().Palette.Focus) != "#22bbcc" {
		t.Fatalf("signal reload did not update palette: %s", theme.FormatHexColor(coord.InitialAppearance().Palette.Focus))
	}

	// Malformed colors.toml on reload retains previous usable palette (#22bbcc) and surfaces diagnostic!
	if err := os.WriteFile(omarchyColors, []byte("focus = \"bad\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = coord.HandleThemeSignal(ctx, coord.ThemeGeneration())
	if theme.FormatHexColor(coord.InitialAppearance().Palette.Focus) != "#22bbcc" || coord.SettingsState().ThemeError == "" {
		t.Fatalf("malformed reload lost previous palette or diagnostic: %+v", coord.SettingsState())
	}
	sendUIKeys(t, r, "esc") // close note editor

	// 2. Open Settings ('S'), verify catalog order (colors first, presets, nord), apply solarized-dark!
	sendUIKeys(t, r, "S")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	view := r.Model.View().Content
	if !strings.Contains(view, "colors  (Active)") || !strings.Contains(view, "nord") {
		t.Fatalf("Settings missing catalog entries:\n%s", view)
	}
	sendUIKeys(t, r, "j", "enter") // select solarized-dark
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if coord.SettingsState().ActiveTheme != theme.SolarizedDarkTheme {
		t.Fatalf("expected solarized-dark active, got %q", coord.SettingsState().ActiveTheme)
	}

	// Explicit preset stays selected when Omarchy updates colors.toml and sends signal!
	if err := os.WriteFile(omarchyColors, []byte("focus = \"#ff00ff\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = coord.HandleThemeSignal(ctx, coord.ThemeGeneration())
	if coord.SettingsState().ActiveTheme != theme.SolarizedDarkTheme {
		t.Fatalf("signal reload overwrote explicit preset: %q", coord.SettingsState().ActiveTheme)
	}

	// Failed config write (0644 config.toml) retains solarized-dark and surfaces error!
	if err := os.Chmod(filepath.Join(talettyDir, "config.toml"), 0o644); err != nil {
		t.Fatal(err)
	}
	sendUIKeys(t, r, "j", "enter") // try catppuccin
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if coord.SettingsState().ActiveTheme != theme.SolarizedDarkTheme || coord.SettingsState().ThemeError == "" {
		t.Fatalf("failed theme save altered active theme: %+v", coord.SettingsState())
	}
	_ = os.Chmod(filepath.Join(talettyDir, "config.toml"), 0o600)

	// 3. Preferences: cycle Smart rewind, Pre-roll, and Glyph mode (including PutSettings failure recovery).
	sendUIKeys(t, r, "tab", "enter", "j", "enter") // cycle SmartRewind & PreRoll
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if coord.SettingsState().SmartRewind != "15s / 10s" || coord.SettingsState().PreRoll != "5s" {
		t.Fatalf("unexpected cycled preferences: %+v", coord.SettingsState())
	}
	faultSt.putSettingsErr = store.ErrDatabase
	sendUIKeys(t, r, "j", "enter") // Glyph mode fails -> retains Unicode
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if coord.InitialAppearance().Glyphs != ui.Unicode {
		t.Fatal("failed PutSettings altered GlyphMode")
	}
	faultSt.putSettingsErr = nil
	sendUIKeys(t, r, "enter") // Glyph mode succeeds -> Plain
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if coord.InitialAppearance().Glyphs != ui.Plain {
		t.Fatal("GlyphMode did not switch to Plain")
	}
	sendUIKeys(t, r, "esc")

	// Close and reopen workspace -> solarized-dark and Plain glyphs persist across restart!
	_ = player.Close(ctx)
	_ = coord.Close(ctx)

	coord2, err := OpenWorkspace(ctx, home, Options{Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer coord2.Close(context.Background())
	if coord2.SettingsState().ActiveTheme != theme.SolarizedDarkTheme || coord2.InitialAppearance().Glyphs != ui.Plain {
		t.Fatalf("restart lost persisted theme/glyphs: settings=%+v app=%+v", coord2.SettingsState(), coord2.InitialAppearance())
	}
}

func TestRunSessionSIGUSR1AndThemeEdgeCases(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	ws, err := profile.Open(ctx, home, 100)
	if err != nil {
		t.Fatal(err)
	}
	st := ws.Store()
	coord, err := NewCoordinator(ctx, st, Options{
		RootOwner:       ws.Owner(),
		WorkspaceCloser: ws.Close,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer coord.Close(context.Background())

	// Update colors.toml and deliver real SIGUSR1 inside RunSession!
	if err := os.WriteFile(filepath.Join(ws.RootPath(), "colors.toml"), []byte("focus = \"#334455\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tty, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()

	err = RunSession(ctx, coord, tty, nil, func(runCtx context.Context, model tea.Model, _ *os.File, _ io.Writer) (tea.Model, error) {
		r, err := testkit.New(runCtx, model, 100)
		if err != nil {
			return model, err
		}
		if err := unix.Kill(os.Getpid(), unix.SIGUSR1); err != nil {
			return model, err
		}
		if err := r.Step(0); err != nil {
			return model, err
		}
		return r.Model, nil
	})
	if err != nil {
		t.Fatalf("RunSession failed: %v", err)
	}
	if theme.FormatHexColor(coord.InitialAppearance().Palette.Focus) != "#334455" {
		t.Fatalf("RunSession SIGUSR1 did not reload palette: %s", theme.FormatHexColor(coord.InitialAppearance().Palette.Focus))
	}

	// Edge cases: stale generation, no-root coordinator, closed coordinator, format helpers.
	if msg := coord.HandleThemeSignal(ctx, 99999); msg != nil {
		t.Fatalf("expected nil for stale generation, got %#v", msg)
	}
	if msg := (*Coordinator)(nil).HandleThemeSignal(ctx, 0); msg != nil {
		t.Fatal("expected nil for nil coordinator")
	}
	if got := (*Coordinator)(nil).ThemeGeneration(); got != 0 {
		t.Fatal("expected 0 generation for nil coordinator")
	}
	noRootCoord, _ := NewCoordinator(ctx, st, Options{})
	defer noRootCoord.Close(context.Background())
	if msg := noRootCoord.Dispatch(ui.Action{Kind: ui.ApplyTheme, Text: "colors"})(); !strings.Contains(msg.(ui.NoticeMsg).Error, "unavailable") {
		t.Fatalf("expected unavailable without root: %#v", msg)
	}
	if msg := noRootCoord.Dispatch(ui.Action{Kind: ui.ReloadTheme})(); !strings.Contains(msg.(ui.NoticeMsg).Error, "unavailable") {
		t.Fatalf("expected unavailable reload without root: %#v", msg)
	}
	if msg := noRootCoord.handleSettingsAction(ui.Action{Kind: "unknown"}); !strings.Contains(msg.(ui.NoticeMsg).Activity, "unavailable") {
		t.Fatalf("expected default settings notice: %#v", msg)
	}
	if formatSmartRewind(playback.SmartRewindPolicy{}) != "Off" || formatPreRoll(0) != "Off" {
		t.Fatal("expected Off formatting")
	}
	for _, errCase := range []error{
		nil,
		theme.ErrInvalidThemeName,
		theme.ErrUnknownTheme,
		theme.ErrUnsafeFile,
		theme.ErrMalformedFile,
		errors.New("other"),
	} {
		_ = sanitizeThemeError(errCase)
	}

	// Failing RootOwner.Lease in initTheme, applyTheme, and HandleThemeSignal falls back safely.
	failOwner := &failingLeaseOwner{err: errors.New("lease failed")}
	leaseCoord, err := NewCoordinator(ctx, st, Options{RootOwner: failOwner})
	if err != nil {
		t.Fatal(err)
	}
	defer leaseCoord.Close(context.Background())
	if leaseCoord.InitialAppearance().Palette != theme.DefaultPalette() {
		t.Fatal("expected DefaultPalette fallback on Lease failure")
	}
	leaseCoord.themeMgr = theme.OpenManager(nil)
	if msg := leaseCoord.Dispatch(ui.Action{Kind: ui.ApplyTheme, Text: "colors"})(); !strings.Contains(msg.(ui.NoticeMsg).Error, "failed") {
		t.Fatalf("expected Lease error on ApplyTheme: %#v", msg)
	}
	if msg := leaseCoord.HandleThemeSignal(ctx, 0); !strings.Contains(msg.(ui.NoticeMsg).Error, "failed") {
		t.Fatalf("expected Lease error on HandleThemeSignal: %#v", msg)
	}

	_ = coord.Close(ctx)
	for _, kind := range []ui.ActionKind{ui.ApplyTheme, ui.SetGlyphMode, ui.CycleSmartRewind, ui.CyclePreRoll} {
		if msg := coord.Dispatch(ui.Action{Kind: kind, Text: "colors"})(); !strings.Contains(msg.(ui.NoticeMsg).Error, "closed") {
			t.Fatalf("expected closed error for %v: %#v", kind, msg)
		}
	}

	// Malformed colors.toml on startup records ThemeError and falls back to DefaultPalette.
	badHome := t.TempDir()
	badDir := filepath.Join(badHome, ".config", "taletty")
	_ = os.MkdirAll(badDir, 0o700)
	_ = os.WriteFile(filepath.Join(badDir, "colors.toml"), []byte("focus = \"not-hex\"\n"), 0o600)
	badCoord, err := OpenWorkspace(ctx, badHome, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer badCoord.Close(context.Background())
	if badCoord.SettingsState().ThemeError == "" || badCoord.InitialAppearance().Palette != theme.DefaultPalette() {
		t.Fatalf("expected fallback diagnostic on malformed startup colors.toml: %+v", badCoord.SettingsState())
	}

	// sessionModel.applyDispatchedMsg handles single non-batch messages and closed waitThemeSignalCmd.
	sm := sessionModel{inner: ui.NewModel(ui.SnapshotMsg{}, nil)}
	_, _ = sm.applyDispatchedMsg(ui.NoticeMsg{Activity: "ok"})
	closedCh := make(chan uint64)
	close(closedCh)
	if msg := waitThemeSignalCmd(closedCh)(); msg != nil {
		t.Fatalf("expected nil from closed theme signal channel, got %#v", msg)
	}
	if waitThemeSignalCmd(nil) != nil {
		t.Fatal("expected nil cmd for nil theme signal channel")
	}
}

type failingLeaseOwner struct{ err error }

func (f *failingLeaseOwner) Lease(context.Context) (*store.RootLease, error) {
	return nil, f.err
}
