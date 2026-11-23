package theme

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/susamn/taletty/app/internal/ui"
	"golang.org/x/sys/unix"
)

var (
	ErrUnsafeRoot = errors.New("theme root must be a private 0700 owned directory")
	tempSeq       atomic.Uint64
)

func validateRoot(root *os.File) (int, error) {
	if root == nil {
		return -1, ErrUnsafeRoot
	}
	fd := int(root.Fd())
	if fd < 0 {
		return -1, ErrUnsafeRoot
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return -1, ErrUnsafeRoot
	}
	if st.Mode&(unix.S_IFMT|07777) != unix.S_IFDIR|0700 || st.Uid != uint32(os.Geteuid()) {
		return -1, ErrUnsafeRoot
	}
	return fd, nil
}

// SeedDefaults creates default colors.toml (0600) and config.toml (theme = "colors", 0600)
// and an empty themes/ directory (0700) if absent. It never overwrites existing
// user files or desktop-managed symlinks.
func SeedDefaults(root *os.File) error {
	rootFd, err := validateRoot(root)
	if err != nil {
		return err
	}
	if err := unix.Mkdirat(rootFd, "themes", 0700); err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	if err := createExclusive(rootFd, "colors.toml", []byte(SerializePalette(DefaultPalette()))); err != nil {
		return err
	}
	return createExclusive(rootFd, "config.toml", []byte("theme = \"colors\"\n"))
}

func createExclusive(dirFd int, name string, content []byte) error {
	fd, err := unix.Openat(dirFd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if errors.Is(err, unix.EEXIST) {
		return nil
	}
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	if err := writeSyncClose(f, content); err != nil {
		_ = unix.Unlinkat(dirFd, name, 0)
		return err
	}
	return nil
}

func writeSyncClose(f *os.File, content []byte) error {
	for len(content) > 0 {
		n, err := f.Write(content)
		if err != nil {
			_ = f.Close()
			return err
		}
		content = content[n:]
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func readBoundedRegular(dirFd int, name string, followSymlink, requirePrivate bool) ([]byte, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC
	if !followSymlink {
		flags |= unix.O_NOFOLLOW
	}
	fd, err := unix.Openat(dirFd, name, flags, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, ErrUnsafeFile
	}
	if requirePrivate && (st.Mode&(unix.S_IFMT|07777) != unix.S_IFREG|0600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1) {
		return nil, ErrUnsafeFile
	}
	if st.Size > maxConfigBytes {
		return nil, fmt.Errorf("%w: exceeds %d bytes", ErrMalformedFile, maxConfigBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("%w: exceeds %d bytes", ErrMalformedFile, maxConfigBytes)
	}
	return data, nil
}

// LoadConfig reads the active theme name from config.toml under root.
// Missing or invalid configurations fall back safely to DefaultTheme alongside an error.
func LoadConfig(root *os.File) (string, error) {
	rootFd, err := validateRoot(root)
	if err != nil {
		return DefaultTheme, err
	}
	data, err := readBoundedRegular(rootFd, "config.toml", false, true)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return DefaultTheme, fmt.Errorf("config.toml missing: %w", os.ErrNotExist)
		}
		if errors.Is(err, unix.ELOOP) {
			return DefaultTheme, fmt.Errorf("%w: config.toml must not be a symlink", ErrUnsafeFile)
		}
		return DefaultTheme, err
	}
	return parseConfigTheme(data)
}

func parseConfigTheme(data []byte) (string, error) {
	if bytes.IndexByte(data, 0) >= 0 {
		return DefaultTheme, fmt.Errorf("%w: NUL byte in config.toml", ErrMalformedFile)
	}
	themeName := DefaultTheme
	found := false
	lines := strings.Split(string(data), "\n")
	for idx, rawLine := range lines {
		line := strings.TrimSuffix(rawLine, "\r")
		if hasUnsafeControl(line) {
			return DefaultTheme, fmt.Errorf("%w: line %d has control characters", ErrMalformedFile, idx+1)
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, val, ok := parseKeyValue(trimmed)
		if !ok {
			return DefaultTheme, fmt.Errorf("%w: line %d invalid syntax", ErrMalformedFile, idx+1)
		}
		if key == "theme" {
			if !ValidThemeName(val) {
				return DefaultTheme, fmt.Errorf("%w: invalid configured theme", ErrInvalidThemeName)
			}
			themeName = val
			found = true
		}
	}
	if !found {
		return DefaultTheme, nil
	}
	return themeName, nil
}

// Catalog returns built-in themes ("colors", "solarized-dark", "catppuccin")
// followed by sorted, deduplicated safe custom theme names from themes/*.toml.
func Catalog(root *os.File) ([]string, error) {
	themes := BuiltInThemes()
	rootFd, err := validateRoot(root)
	if err != nil {
		return themes, err
	}
	themesFd, err := unix.Openat(rootFd, "themes", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return themes, nil
	}
	if err != nil {
		return themes, err
	}
	dir := os.NewFile(uintptr(themesFd), "themes")
	defer dir.Close()

	var dirStat unix.Stat_t
	if err := unix.Fstat(themesFd, &dirStat); err != nil {
		return themes, err
	}
	if dirStat.Uid != uint32(os.Geteuid()) {
		return themes, ErrUnsafeRoot
	}

	entries, err := dir.ReadDir(-1)
	if err != nil {
		return themes, err
	}

	seen := map[string]bool{
		DefaultTheme:       true,
		SolarizedDarkTheme: true,
		CatppuccinTheme:    true,
	}
	var custom []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".toml") {
			continue
		}
		themeName := strings.TrimSuffix(name, ".toml")
		if !ValidCustomThemeName(themeName) || seen[themeName] {
			continue
		}
		if !safeThemeEntry(themesFd, name) {
			continue
		}
		seen[themeName] = true
		custom = append(custom, themeName)
	}
	slices.Sort(custom)
	return append(themes, custom...), nil
}

func safeThemeEntry(themesFd int, filename string) bool {
	var st unix.Stat_t
	if err := unix.Fstatat(themesFd, filename, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		return st.Size <= maxConfigBytes
	case unix.S_IFLNK:
		target, err := readlinkAt(themesFd, filename)
		if err != nil || target == "" || strings.ContainsRune(target, '/') || strings.Contains(target, "..") {
			return false
		}
		var targetSt unix.Stat_t
		if err := unix.Fstatat(themesFd, filename, &targetSt, 0); err != nil {
			return false
		}
		return targetSt.Mode&unix.S_IFMT == unix.S_IFREG && targetSt.Size <= maxConfigBytes
	default:
		return false
	}
}

func readlinkAt(dirFd int, name string) (string, error) {
	buf := make([]byte, 256)
	n, err := unix.Readlinkat(dirFd, name, buf)
	if err != nil {
		return "", err
	}
	if n >= len(buf) {
		return "", ErrUnsafeFile
	}
	return string(buf[:n]), nil
}

// SaveThemeSelection validates themeName against available Catalog entries and
// atomically updates config.toml (preserving other lines) via temp file + renameat.
func SaveThemeSelection(root *os.File, themeName string) error {
	rootFd, err := validateRoot(root)
	if err != nil {
		return err
	}
	if !ValidThemeName(themeName) {
		return ErrInvalidThemeName
	}
	catalog, err := Catalog(root)
	if err != nil {
		return err
	}
	if !slices.Contains(catalog, themeName) {
		return fmt.Errorf("%w: %s", ErrUnknownTheme, themeName)
	}

	var existing []byte
	data, err := readBoundedRegular(rootFd, "config.toml", false, true)
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return ErrUnsafeFile
	}
	if err == nil {
		existing = data
	}

	updated := renderUpdatedConfig(existing, themeName)
	tmpName := fmt.Sprintf(".config.toml.tmp.%d.%d", os.Getpid(), tempSeq.Add(1))
	fd, err := unix.Openat(rootFd, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), tmpName)
	if err := writeSyncClose(f, updated); err != nil {
		_ = unix.Unlinkat(rootFd, tmpName, 0)
		return err
	}
	if err := unix.Renameat(rootFd, tmpName, rootFd, "config.toml"); err != nil {
		_ = unix.Unlinkat(rootFd, tmpName, 0)
		return err
	}
	_ = unix.Fsync(rootFd)
	return nil
}

func renderUpdatedConfig(existing []byte, themeName string) []byte {
	themeLine := fmt.Sprintf("theme = %q", themeName)
	if len(existing) == 0 {
		return []byte(themeLine + "\n")
	}
	rawLines := strings.Split(strings.TrimSuffix(string(existing), "\n"), "\n")
	out := make([]string, 0, len(rawLines)+1)
	replaced := false
	for _, rawLine := range rawLines {
		line := strings.TrimSuffix(rawLine, "\r")
		trimmed := strings.TrimSpace(line)
		if key, _, ok := parseKeyValue(trimmed); ok && key == "theme" {
			if !replaced {
				out = append(out, themeLine)
				replaced = true
			}
			continue
		}
		out = append(out, line)
	}
	if !replaced {
		out = append(out, themeLine)
	}
	return []byte(strings.Join(out, "\n") + "\n")
}

// Resolve returns an active usable ui.Palette and source description for themeName.
// Missing or malformed theme files fall back safely to DefaultPalette with a non-nil
// diagnostic error so the UI never crashes or renders blank text.
func Resolve(root *os.File, themeName string) (ui.Palette, string, error) {
	if themeName == "" {
		themeName = DefaultTheme
	}
	if themeName == SolarizedDarkTheme {
		return SolarizedDarkPalette(), "built-in solarized-dark", nil
	}
	if themeName == CatppuccinTheme {
		return CatppuccinPalette(), "built-in catppuccin", nil
	}
	if !ValidThemeName(themeName) {
		return DefaultPalette(), "fallback default (invalid theme name)", ErrInvalidThemeName
	}
	rootFd, err := validateRoot(root)
	if err != nil {
		return DefaultPalette(), "fallback default (unavailable root)", err
	}

	if themeName == DefaultTheme {
		data, err := readBoundedRegular(rootFd, "colors.toml", true, false)
		if err != nil {
			return DefaultPalette(), "fallback default (colors.toml unavailable)", fmt.Errorf("colors.toml unavailable: %w", err)
		}
		palette, parseErr := ParsePalette(data)
		if parseErr != nil {
			return palette, "fallback default (colors.toml malformed)", parseErr
		}
		return palette, "colors.toml", nil
	}

	themesFd, err := unix.Openat(rootFd, "themes", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return DefaultPalette(), fmt.Sprintf("fallback default (themes/%s.toml unavailable)", themeName), fmt.Errorf("themes directory unavailable: %w", err)
	}
	dir := os.NewFile(uintptr(themesFd), "themes")
	defer dir.Close()

	filename := themeName + ".toml"
	if !safeThemeEntry(themesFd, filename) {
		return DefaultPalette(), fmt.Sprintf("fallback default (themes/%s.toml unavailable)", themeName), fmt.Errorf("themes/%s.toml missing or unsafe", themeName)
	}
	data, err := readBoundedRegular(themesFd, filename, true, false)
	if err != nil {
		return DefaultPalette(), fmt.Sprintf("fallback default (themes/%s.toml unavailable)", themeName), fmt.Errorf("themes/%s.toml unavailable: %w", themeName, err)
	}
	palette, parseErr := ParsePalette(data)
	if parseErr != nil {
		return palette, fmt.Sprintf("fallback default (themes/%s.toml malformed)", themeName), parseErr
	}
	return palette, fmt.Sprintf("themes/%s.toml", themeName), nil
}

// ReloadOnSignal handles SIGUSR1 live reloads. When currentTheme is "colors" or
// a custom theme, it re-reads that file and reports reloaded=true on valid reload.
// Explicit built-in presets ("solarized-dark", "catppuccin") remain selected and
// report reloaded=false, err=nil. On malformed/missing reload, reloaded=false and
// a non-nil error are returned so the caller can retain its previous usable palette.
func ReloadOnSignal(root *os.File, currentTheme string) (ui.Palette, bool, error) {
	if currentTheme == "" {
		currentTheme = DefaultTheme
	}
	if currentTheme == SolarizedDarkTheme || currentTheme == CatppuccinTheme {
		preset, _ := Preset(currentTheme)
		return preset, false, nil
	}
	if !ValidThemeName(currentTheme) {
		return DefaultPalette(), false, ErrInvalidThemeName
	}
	palette, _, err := Resolve(root, currentTheme)
	if err != nil {
		return palette, false, err
	}
	return palette, true, nil
}

// Manager coordinates active theme selection, atomic persistence, and stale-safe
// live reloads without touching playback or UI focus state.
type Manager struct {
	mu          sync.Mutex
	activeTheme string
	palette     ui.Palette
	source      string
	lastErr     error
	generation  uint64
}

// OpenManager loads config.toml and resolves the initial palette under root.
func OpenManager(root *os.File) *Manager {
	themeName, cfgErr := LoadConfig(root)
	palette, source, resErr := Resolve(root, themeName)
	return &Manager{
		activeTheme: themeName,
		palette:     palette,
		source:      source,
		lastErr:     errors.Join(cfgErr, resErr),
		generation:  1,
	}
}

func (m *Manager) ActiveTheme() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeTheme
}

func (m *Manager) Palette() ui.Palette {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.palette
}

func (m *Manager) Source() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.source
}

func (m *Manager) LastError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

func (m *Manager) Generation() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generation
}

// Select validates, resolves, and persists themeName before committing it live.
// On validation or config write failure, the previous active theme and palette stay intact.
func (m *Manager) Select(root *os.File, themeName string) (ui.Palette, string, error) {
	palette, source, err := Resolve(root, themeName)
	if err != nil {
		m.mu.Lock()
		prevPalette, prevSource := m.palette, m.source
		m.lastErr = err
		m.mu.Unlock()
		return prevPalette, prevSource, err
	}
	if err := SaveThemeSelection(root, themeName); err != nil {
		m.mu.Lock()
		prevPalette, prevSource := m.palette, m.source
		m.lastErr = err
		m.mu.Unlock()
		return prevPalette, prevSource, err
	}
	m.mu.Lock()
	m.activeTheme = themeName
	m.palette = palette
	m.source = source
	m.lastErr = nil
	m.generation++
	m.mu.Unlock()
	return palette, source, nil
}

// HandleSignal applies a live reload if expectedGen matches (or is 0).
// Failed reloads retain the previous usable palette and record the diagnostic error.
func (m *Manager) HandleSignal(root *os.File, expectedGen uint64) (ui.Palette, bool, error) {
	m.mu.Lock()
	if expectedGen != 0 && expectedGen != m.generation {
		prev := m.palette
		m.mu.Unlock()
		return prev, false, nil
	}
	current := m.activeTheme
	prev := m.palette
	m.mu.Unlock()

	palette, reloaded, err := ReloadOnSignal(root, current)
	if err != nil {
		m.mu.Lock()
		m.lastErr = err
		m.mu.Unlock()
		return prev, false, err
	}
	if !reloaded {
		return prev, false, nil
	}
	_, source, _ := Resolve(root, current)
	m.mu.Lock()
	if expectedGen == 0 || expectedGen == m.generation {
		m.palette = palette
		m.source = source
		m.lastErr = nil
		m.mu.Unlock()
		return palette, true, nil
	}
	final := m.palette
	m.mu.Unlock()
	return final, false, nil
}

// WatchSIGUSR1 invokes onSignal whenever SIGUSR1 arrives until ctx is canceled.
func WatchSIGUSR1(ctx context.Context, onSignal func()) {
	WatchSIGUSR1Ready(ctx, onSignal)()
}

// WatchSIGUSR1Ready registers the SIGUSR1 handler synchronously before returning
// a blocking loop function so callers/tests never race signal.Notify startup.
func WatchSIGUSR1Ready(ctx context.Context, onSignal func()) func() {
	if ctx == nil || onSignal == nil {
		return func() {}
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, unix.SIGUSR1)
	return func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				onSignal()
			}
		}
	}
}
