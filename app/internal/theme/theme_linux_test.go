package theme

import (
	"context"
	"errors"
	"image/color"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/susamn/taletty/app/internal/ui"
	"golang.org/x/sys/unix"
)

func openPrivateRoot(t *testing.T) (string, *os.File) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return dir, f
}

func TestThemeNamesPresetsAndPaletteRoundtrip(t *testing.T) {
	if got := BuiltInThemes(); !reflect.DeepEqual(got, []string{"colors", "solarized-dark", "catppuccin"}) {
		t.Fatalf("unexpected BuiltInThemes: %+v", got)
	}
	for _, valid := range []string{"a", "nord", "gruvbox-dark_1", strings.Repeat("a", 63)} {
		if !ValidThemeName(valid) || !ValidCustomThemeName(valid) {
			t.Fatalf("expected valid custom theme name %q", valid)
		}
	}
	for _, reserved := range []string{"colors", "solarized-dark", "catppuccin"} {
		if !ValidThemeName(reserved) || ValidCustomThemeName(reserved) {
			t.Fatalf("reserved theme %q misclassified", reserved)
		}
	}
	for _, invalid := range []string{"", "-bad", "_bad", "Bad", "a/b", "../x", "a.b", strings.Repeat("a", 64)} {
		if ValidThemeName(invalid) || ValidCustomThemeName(invalid) {
			t.Fatalf("expected invalid theme name %q", invalid)
		}
	}

	for _, name := range []string{DefaultTheme, SolarizedDarkTheme, CatppuccinTheme} {
		p, ok := Preset(name)
		if !ok || p.Focus == nil || p.Background == nil {
			t.Fatalf("Preset(%q) failed: ok=%v %+v", name, ok, p)
		}
		serialized := SerializePalette(p)
		parsed, err := ParsePalette([]byte(serialized))
		if err != nil {
			t.Fatalf("roundtrip ParsePalette(%q) failed: %v", name, err)
		}
		if parsed != p {
			t.Fatalf("roundtrip mismatch for %q:\ngot:  %+v\nwant: %+v", name, parsed, p)
		}
	}
	if p, ok := Preset("unknown"); ok || p != DefaultPalette() {
		t.Fatalf("expected DefaultPalette fallback for unknown preset")
	}
	if got := FormatHexColor(nil); got != "#000000" {
		t.Fatalf("expected #000000 for nil color, got %q", got)
	}
	if got := SerializePalette(ui.Palette{}); !strings.Contains(got, "background = \"#000000\"") {
		t.Fatalf("SerializePalette nil fallback failed:\n%s", got)
	}
}

func TestParsePaletteOmarchyPartialAndMalformedInputs(t *testing.T) {
	// Omarchy / Alacritty / base16 keys + comments + single quotes + mode ignored.
	omarchy := `
# Omarchy theme colors
mode = "dark"
unknown_key = "ignored"
background = '#112233' # inline comment
foreground = "#ddeeff"
accent = "#44aa88"
selection_foreground = "#ffffff"
color8 = "#556677"
red = "#cc2233"
`
	p, err := ParsePalette([]byte(omarchy))
	if err != nil {
		t.Fatalf("unexpected Omarchy parse error: %v", err)
	}
	if FormatHexColor(p.Background) != "#112233" || FormatHexColor(p.Text) != "#ddeeff" ||
		FormatHexColor(p.Focus) != "#44aa88" || FormatHexColor(p.Selection) != "#ffffff" ||
		FormatHexColor(p.Border) != "#556677" || FormatHexColor(p.Error) != "#cc2233" {
		t.Fatalf("unexpected Omarchy mapped palette: %+v", p)
	}

	// Partial file inherits missing slots from DefaultPalette.
	partial, err := ParsePalette([]byte("focus = #123456\t# tab comment\n"))
	if err != nil {
		t.Fatalf("unexpected partial parse error: %v", err)
	}
	if FormatHexColor(partial.Focus) != "#123456" || partial.Background != DefaultPalette().Background {
		t.Fatalf("partial fallback failed: %+v", partial)
	}

	// Malformed / injection / oversized / NUL inputs return readable fallback + error.
	for _, bad := range [][]byte{
		make([]byte, maxConfigBytes+1),
		[]byte("focus = \"#112233\"\x00"),
		[]byte("focus = \"#112233\x1b[2J\""),
		[]byte("bad line without equals"),
		[]byte("= \"#112233\""),
		[]byte("bad-key = \"#112233\""),
		[]byte("focus = "),
		[]byte("focus = \"unterminated"),
		[]byte("focus = \"#112233\" trailing"),
		[]byte("focus = \"#12345\""),
		[]byte("focus = \"#zzzzzz\""),
		[]byte("# only comments\nmode = \"dark\"\n"),
	} {
		got, err := ParsePalette(bad)
		if err == nil || !errors.Is(err, ErrMalformedFile) {
			t.Fatalf("expected ErrMalformedFile for %q, got err=%v", string(bad), err)
		}
		if got.Text == nil || got.Background == nil {
			t.Fatalf("malformed parse returned unusable palette: %+v", got)
		}
	}
}

func TestSeedDefaultsSymlinkPreservationCatalogAndPersistence(t *testing.T) {
	dir, root := openPrivateRoot(t)

	// Unsafe root rejected.
	if _, err := validateRoot(nil); !errors.Is(err, ErrUnsafeRoot) {
		t.Fatal("expected ErrUnsafeRoot for nil root")
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SeedDefaults(root); !errors.Is(err, ErrUnsafeRoot) {
		t.Fatalf("expected ErrUnsafeRoot for 0755 root, got %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// SeedDefaults creates colors.toml, config.toml, and themes/ without overwriting existing files or symlinks.
	omarchyDir := t.TempDir()
	omarchyColors := filepath.Join(omarchyDir, "colors.toml")
	if err := os.WriteFile(omarchyColors, []byte("focus = \"#abcdef\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(omarchyColors, filepath.Join(dir, "colors.toml")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("# user comment\ntheme = \"colors\"\nother = \"keep\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SeedDefaults(root); err != nil {
		t.Fatalf("SeedDefaults failed: %v", err)
	}
	// Symlink preserved!
	if target, err := os.Readlink(filepath.Join(dir, "colors.toml")); err != nil || target != omarchyColors {
		t.Fatalf("SeedDefaults overwrote desktop symlink: target=%q err=%v", target, err)
	}
	// Fresh root seeds default files cleanly.
	freshDir, freshRoot := openPrivateRoot(t)
	if err := SeedDefaults(freshRoot); err != nil {
		t.Fatalf("fresh SeedDefaults failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(freshDir, "themes")); err != nil {
		t.Fatalf("missing themes dir: %v", err)
	}

	// Catalog discovers sorted valid custom themes and ignores invalid/traversal/hidden/reserved entries.
	themesDir := filepath.Join(dir, "themes")
	for _, item := range []struct {
		name, body string
	}{
		{"zeta.toml", "focus = \"#111111\"\n"},
		{"alpha.toml", "focus = \"#222222\"\n"},
		{"colors.toml", "focus = \"#333333\"\n"}, // reserved name ignored
		{".hidden.toml", "focus = \"#444444\"\n"},
		{"BadName.toml", "focus = \"#555555\"\n"},
		{"notes.txt", "ignored"},
	} {
		if err := os.WriteFile(filepath.Join(themesDir, item.name), []byte(item.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Safe relative symlink inside themes/ allowed; traversal symlink rejected.
	if err := os.Symlink("alpha.toml", filepath.Join(themesDir, "beta.toml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../colors.toml", filepath.Join(themesDir, "escape.toml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(themesDir, "subdir.toml"), 0o700); err != nil {
		t.Fatal(err)
	}

	cat, err := Catalog(root)
	if err != nil {
		t.Fatalf("Catalog failed: %v", err)
	}
	wantCat := []string{"colors", "solarized-dark", "catppuccin", "alpha", "beta", "zeta"}
	if !reflect.DeepEqual(cat, wantCat) {
		t.Fatalf("unexpected Catalog: got=%+v want=%+v", cat, wantCat)
	}

	// SaveThemeSelection preserves comments/other keys and rejects invalid/unknown names.
	if err := SaveThemeSelection(root, "alpha"); err != nil {
		t.Fatalf("SaveThemeSelection failed: %v", err)
	}
	cfgBytes, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	if !strings.Contains(string(cfgBytes), "# user comment") || !strings.Contains(string(cfgBytes), "other = \"keep\"") || !strings.Contains(string(cfgBytes), `theme = "alpha"`) {
		t.Fatalf("SaveThemeSelection lost config lines:\n%s", string(cfgBytes))
	}
	if gotTheme, err := LoadConfig(root); err != nil || gotTheme != "alpha" {
		t.Fatalf("LoadConfig mismatch: theme=%q err=%v", gotTheme, err)
	}
	if err := SaveThemeSelection(root, "../bad"); !errors.Is(err, ErrInvalidThemeName) {
		t.Fatalf("expected ErrInvalidThemeName, got %v", err)
	}
	if err := SaveThemeSelection(root, "missing"); !errors.Is(err, ErrUnknownTheme) {
		t.Fatalf("expected ErrUnknownTheme, got %v", err)
	}

	// Resolve built-in, colors.toml symlink, custom theme, and malformed/missing fallbacks.
	if p, src, err := Resolve(root, ""); err != nil || src != "colors.toml" || FormatHexColor(p.Focus) != "#abcdef" {
		t.Fatalf("Resolve colors.toml symlink failed: src=%q err=%v p=%+v", src, err, p)
	}
	if _, src, err := Resolve(root, SolarizedDarkTheme); err != nil || src != "built-in solarized-dark" {
		t.Fatalf("Resolve solarized-dark failed: src=%q err=%v", src, err)
	}
	if _, src, err := Resolve(root, CatppuccinTheme); err != nil || src != "built-in catppuccin" {
		t.Fatalf("Resolve catppuccin failed: src=%q err=%v", src, err)
	}
	if p, src, err := Resolve(root, "alpha"); err != nil || src != "themes/alpha.toml" || FormatHexColor(p.Focus) != "#222222" {
		t.Fatalf("Resolve custom alpha failed: src=%q err=%v p=%+v", src, err, p)
	}
	if _, _, err := Resolve(root, "missing"); err == nil {
		t.Fatal("expected error resolving missing custom theme")
	}
	if _, _, err := Resolve(root, "Bad!"); !errors.Is(err, ErrInvalidThemeName) {
		t.Fatalf("expected ErrInvalidThemeName, got %v", err)
	}

	// Malformed config.toml falls back safely.
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("theme = \"Bad!\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadConfig(root); !errors.Is(err, ErrInvalidThemeName) || got != DefaultTheme {
		t.Fatalf("expected DefaultTheme fallback on invalid config theme, got %q err=%v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("bad syntax\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(root); !errors.Is(err, ErrMalformedFile) {
		t.Fatalf("expected ErrMalformedFile, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("other = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadConfig(root); err != nil || got != DefaultTheme {
		t.Fatalf("expected DefaultTheme when theme key absent, got %q err=%v", got, err)
	}
	// Symlink config.toml rejected.
	_ = os.Remove(filepath.Join(dir, "config.toml"))
	_ = os.Symlink(omarchyColors, filepath.Join(dir, "config.toml"))
	if _, err := LoadConfig(root); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("expected ErrUnsafeFile for symlink config.toml, got %v", err)
	}
	_ = os.Remove(filepath.Join(dir, "config.toml"))
	if _, err := LoadConfig(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected ErrNotExist for missing config.toml, got %v", err)
	}
}

func TestManagerSignalReloadAndWatchSIGUSR1(t *testing.T) {
	dir, root := openPrivateRoot(t)
	if err := SeedDefaults(root); err != nil {
		t.Fatal(err)
	}
	mgr := OpenManager(root)
	if mgr.ActiveTheme() != DefaultTheme || mgr.Source() != "colors.toml" || mgr.LastError() != nil || mgr.Generation() != 1 {
		t.Fatalf("unexpected initial Manager state: theme=%q src=%q err=%v gen=%d", mgr.ActiveTheme(), mgr.Source(), mgr.LastError(), mgr.Generation())
	}

	// Update colors.toml (simulating Omarchy hook) and reload via HandleSignal.
	if err := os.WriteFile(filepath.Join(dir, "colors.toml"), []byte("focus = \"#102030\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, reloaded, err := mgr.HandleSignal(root, mgr.Generation())
	if err != nil || !reloaded || FormatHexColor(p.Focus) != "#102030" {
		t.Fatalf("HandleSignal reload failed: reloaded=%v err=%v p=%+v", reloaded, err, p)
	}

	// Malformed colors.toml on reload retains previous usable palette (#102030)!
	if err := os.WriteFile(filepath.Join(dir, "colors.toml"), []byte("focus = \"not-hex\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pBad, reloadedBad, errBad := mgr.HandleSignal(root, mgr.Generation())
	if errBad == nil || reloadedBad || FormatHexColor(pBad.Focus) != "#102030" || mgr.LastError() == nil {
		t.Fatalf("malformed reload did not retain previous palette: reloaded=%v err=%v p=%+v", reloadedBad, errBad, pBad)
	}

	// Select explicit preset solarized-dark -> persists and increments generation; stale signal ignored;
	// subsequent signal reload keeps solarized-dark selected even when colors.toml changes!
	oldGen := mgr.Generation()
	pSol, srcSol, err := mgr.Select(root, SolarizedDarkTheme)
	if err != nil || srcSol != "built-in solarized-dark" || pSol != SolarizedDarkPalette() || mgr.Generation() != oldGen+1 {
		t.Fatalf("Select solarized-dark failed: src=%q err=%v", srcSol, err)
	}
	if _, reloadedStale, errStale := mgr.HandleSignal(root, oldGen); errStale != nil || reloadedStale {
		t.Fatalf("stale generation signal was not ignored: reloaded=%v err=%v", reloadedStale, errStale)
	}
	if err := os.WriteFile(filepath.Join(dir, "colors.toml"), []byte("focus = \"#998877\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pKeep, reloadedKeep, errKeep := mgr.HandleSignal(root, mgr.Generation())
	if errKeep != nil || reloadedKeep || pKeep != SolarizedDarkPalette() || mgr.ActiveTheme() != SolarizedDarkTheme {
		t.Fatalf("signal reload overwrote explicit preset: reloaded=%v err=%v theme=%q", reloadedKeep, errKeep, mgr.ActiveTheme())
	}

	// Failed Select (unknown theme or unsafe config.toml) retains previous active theme and palette.
	if pPrev, _, err := mgr.Select(root, "nonexistent"); err == nil || pPrev != SolarizedDarkPalette() || mgr.ActiveTheme() != SolarizedDarkTheme {
		t.Fatalf("failed Select altered active theme: err=%v theme=%q", err, mgr.ActiveTheme())
	}
	if err := os.Chmod(filepath.Join(dir, "config.toml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if pPrev, _, err := mgr.Select(root, CatppuccinTheme); err == nil || pPrev != SolarizedDarkPalette() || mgr.ActiveTheme() != SolarizedDarkTheme {
		t.Fatalf("failed config write altered active theme: err=%v theme=%q", err, mgr.ActiveTheme())
	}
	if mgr.Palette() != SolarizedDarkPalette() {
		t.Fatal("Manager.Palette changed after failed Select")
	}

	// WatchSIGUSR1 receives real SIGUSR1 and exits cleanly on cancel.
	ctx, cancel := context.WithCancel(context.Background())
	gotSig := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		WatchSIGUSR1(ctx, func() {
			select {
			case gotSig <- struct{}{}:
			default:
			}
		})
		close(done)
	}()
	time.Sleep(15 * time.Millisecond)
	if err := unix.Kill(os.Getpid(), unix.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gotSig:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for WatchSIGUSR1 callback")
	}
	cancel()
	<-done
	WatchSIGUSR1(nil, nil)
}

func TestThemeEdgeCasesAndConfigRendering(t *testing.T) {
	// renderUpdatedConfig appends theme when absent.
	if got := string(renderUpdatedConfig([]byte("other = \"1\"\n"), "catppuccin")); !strings.Contains(got, "other = \"1\"\ntheme = \"catppuccin\"\n") {
		t.Fatalf("unexpected renderUpdatedConfig append:\n%s", got)
	}
	if got := string(renderUpdatedConfig(nil, "colors")); got != "theme = \"colors\"\n" {
		t.Fatalf("unexpected renderUpdatedConfig empty: %q", got)
	}
	// ReloadOnSignal invalid theme name.
	if _, reloaded, err := ReloadOnSignal(nil, "Bad!"); !errors.Is(err, ErrInvalidThemeName) || reloaded {
		t.Fatalf("expected ErrInvalidThemeName, got reloaded=%v err=%v", reloaded, err)
	}
	// Resolve with nil root.
	if _, _, err := Resolve(nil, DefaultTheme); !errors.Is(err, ErrUnsafeRoot) {
		t.Fatalf("expected ErrUnsafeRoot, got %v", err)
	}
	// ParsePalette with color.NRGBA non-RGBA color and uppercase hex.
	if got := FormatHexColor(color.NRGBA{R: 0x12, G: 0x34, B: 0x56, A: 0xff}); got != "#123456" {
		t.Fatalf("unexpected FormatHexColor: %q", got)
	}
	if _, err := ParseHexColor("#ABCDEF"); err != nil {
		t.Fatalf("expected uppercase hex valid: %v", err)
	}
	if validTOMLKey("") {
		t.Fatal("expected empty TOML key invalid")
	}

	// parseConfigTheme NUL and control char rejection.
	if _, err := parseConfigTheme([]byte("theme = \"colors\"\x00")); !errors.Is(err, ErrMalformedFile) {
		t.Fatalf("expected ErrMalformedFile for NUL in config, got %v", err)
	}
	if _, err := parseConfigTheme([]byte("theme = \"colors\x1b[2J\"")); !errors.Is(err, ErrMalformedFile) {
		t.Fatalf("expected ErrMalformedFile for control char in config, got %v", err)
	}

	// Oversized / non-regular / missing files in Resolve, Catalog, SaveThemeSelection.
	dir, root := openPrivateRoot(t)
	if _, err := Catalog(nil); !errors.Is(err, ErrUnsafeRoot) {
		t.Fatal("expected ErrUnsafeRoot on nil Catalog")
	}
	if err := SaveThemeSelection(nil, "colors"); !errors.Is(err, ErrUnsafeRoot) {
		t.Fatal("expected ErrUnsafeRoot on nil SaveThemeSelection")
	}
	// SaveThemeSelection creates config.toml when absent.
	if err := SaveThemeSelection(root, "catppuccin"); err != nil {
		t.Fatalf("SaveThemeSelection on missing config.toml failed: %v", err)
	}
	// Missing colors.toml and missing themes/ dir fallbacks.
	if _, src, err := Resolve(root, DefaultTheme); err == nil || !strings.Contains(src, "unavailable") {
		t.Fatalf("expected unavailable colors.toml fallback, got src=%q err=%v", src, err)
	}
	if _, src, err := Resolve(root, "custom"); err == nil || !strings.Contains(src, "unavailable") {
		t.Fatalf("expected unavailable themes dir fallback, got src=%q err=%v", src, err)
	}
	// Malformed colors.toml and malformed custom theme fallbacks.
	if err := os.WriteFile(filepath.Join(dir, "colors.toml"), []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, src, err := Resolve(root, DefaultTheme); err == nil || !strings.Contains(src, "malformed") {
		t.Fatalf("expected malformed colors.toml fallback, got src=%q err=%v", src, err)
	}
	if err := os.Mkdir(filepath.Join(dir, "themes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "themes", "bad.toml"), []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, src, err := Resolve(root, "bad"); err == nil || !strings.Contains(src, "malformed") {
		t.Fatalf("expected malformed custom theme fallback, got src=%q err=%v", src, err)
	}
	// Oversized colors.toml rejected cleanly.
	if err := os.WriteFile(filepath.Join(dir, "colors.toml"), make([]byte, maxConfigBytes+10), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Resolve(root, DefaultTheme); !errors.Is(err, ErrMalformedFile) {
		t.Fatalf("expected ErrMalformedFile for oversized colors.toml, got %v", err)
	}
	// Non-regular config.toml (directory) rejected cleanly.
	_ = os.Remove(filepath.Join(dir, "config.toml"))
	if err := os.Mkdir(filepath.Join(dir, "config.toml"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(root); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("expected ErrUnsafeFile for directory config.toml, got %v", err)
	}
}
