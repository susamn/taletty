package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/playback"
	"github.com/susamn/taletty/app/internal/store"
	"github.com/susamn/taletty/app/internal/theme"
	"github.com/susamn/taletty/app/internal/ui"
)

var smartRewindPresets = []playback.SmartRewindPolicy{
	playback.DefaultSmartRewindPolicy(),
	{Threshold: 15 * time.Second, Rewind: 10 * time.Second},
	{Threshold: 30 * time.Second, Rewind: 15 * time.Second},
	{},
}

var preRollPresets = []time.Duration{
	2 * time.Second,
	5 * time.Second,
	0,
}

func (c *Coordinator) initTheme(ctx context.Context) {
	if c.rootOwner == nil {
		c.appearance.Palette = theme.DefaultPalette()
		return
	}
	lease, err := c.rootOwner.Lease(ctx)
	if err != nil || lease == nil || lease.Root == nil {
		c.appearance.Palette = theme.DefaultPalette()
		return
	}
	defer lease.Close()
	_ = theme.SeedDefaults(lease.Root)
	mgr := theme.OpenManager(lease.Root)
	c.themeMgr = mgr
	c.appearance.Palette = mgr.Palette()
	if mgr.LastError() != nil {
		c.themeError = sanitizeThemeError(mgr.LastError())
	}
}

// ThemeGeneration returns the current theme manager generation (0 if unconfigured).
func (c *Coordinator) ThemeGeneration() uint64 {
	if c == nil || c.themeMgr == nil {
		return 0
	}
	return c.themeMgr.Generation()
}

// SettingsState returns the current sanitized Settings projection.
func (c *Coordinator) SettingsState() ui.SettingsState {
	c.mu.Lock()
	mgr := c.themeMgr
	rootOwner := c.rootOwner
	glyphs := c.appearance.Glyphs
	smartRewind := c.smartRewind
	preRoll := c.preRoll
	themeErr := c.themeError
	c.mu.Unlock()

	active := theme.DefaultTheme
	source := "default fallback"
	themes := theme.BuiltInThemes()
	if mgr != nil {
		active = mgr.ActiveTheme()
		source = mgr.Source()
	}
	if rootOwner != nil {
		if lease, err := rootOwner.Lease(context.Background()); err == nil && lease != nil && lease.Root != nil {
			if cat, catErr := theme.Catalog(lease.Root); catErr == nil && len(cat) > 0 {
				themes = cat
			}
			lease.Close()
		}
	}
	return ui.SettingsState{
		ActiveTheme: active,
		ThemeSource: source,
		Themes:      themes,
		ThemeError:  themeErr,
		Glyphs:      glyphs,
		SmartRewind: formatSmartRewind(smartRewind),
		PreRoll:     formatPreRoll(preRoll),
	}
}

// InitialSettings returns the initial SettingsMsg for startup.
func (c *Coordinator) InitialSettings() ui.SettingsMsg {
	return ui.SettingsMsg{Settings: c.SettingsState()}
}

func (c *Coordinator) handleSettingsAction(action ui.Action) tea.Msg {
	switch action.Kind {
	case ui.OpenSettings:
		return ui.SettingsMsg{Settings: c.SettingsState()}
	case ui.ApplyTheme:
		return c.applyTheme(action.Text)
	case ui.ReloadTheme:
		return c.HandleThemeSignal(c.ctx, 0)
	case ui.SetGlyphMode:
		return c.toggleGlyphMode()
	case ui.CycleSmartRewind:
		return c.cycleSmartRewind()
	case ui.CyclePreRoll:
		return c.cyclePreRoll()
	default:
		return ui.NoticeMsg{Activity: "Action unavailable in this slice"}
	}
}

func (c *Coordinator) applyTheme(name string) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.NoticeMsg{Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	if c.themeMgr == nil || c.rootOwner == nil {
		return ui.NoticeMsg{Error: "Theme configuration root unavailable."}
	}
	lease, err := c.rootOwner.Lease(ctx)
	if err != nil || lease == nil || lease.Root == nil {
		return ui.NoticeMsg{Error: SanitizeError(err)}
	}
	defer lease.Close()

	palette, _, selErr := c.themeMgr.Select(lease.Root, name)
	if selErr != nil {
		msg := sanitizeThemeError(selErr)
		c.mu.Lock()
		c.themeError = msg
		c.mu.Unlock()
		settingsMsg := ui.SettingsMsg{Settings: c.SettingsState()}
		return tea.BatchMsg{
			func() tea.Msg { return settingsMsg },
			func() tea.Msg { return ui.NoticeMsg{Error: msg} },
		}
	}

	c.mu.Lock()
	c.appearance.Palette = palette
	c.themeError = ""
	appMsg := c.appearance
	c.mu.Unlock()
	settingsMsg := ui.SettingsMsg{Settings: c.SettingsState()}
	return tea.BatchMsg{
		func() tea.Msg { return appMsg },
		func() tea.Msg { return settingsMsg },
		func() tea.Msg { return ui.NoticeMsg{Activity: "Applied theme " + name} },
	}
}

// HandleThemeSignal reloads colors.toml or an active custom theme when expectedGen
// matches (or is 0), retaining the previous usable palette on malformed reload.
func (c *Coordinator) HandleThemeSignal(ctx context.Context, expectedGen uint64) tea.Msg {
	if c == nil || ctx == nil {
		return nil
	}
	opCtx, ok := c.beginWorker()
	if !ok {
		return nil
	}
	defer c.wg.Done()

	if c.themeMgr == nil || c.rootOwner == nil {
		return ui.NoticeMsg{Error: "Theme configuration root unavailable."}
	}
	lease, err := c.rootOwner.Lease(opCtx)
	if err != nil || lease == nil || lease.Root == nil {
		return ui.NoticeMsg{Error: SanitizeError(err)}
	}
	defer lease.Close()

	palette, reloaded, sigErr := c.themeMgr.HandleSignal(lease.Root, expectedGen)
	if sigErr != nil {
		msg := sanitizeThemeError(sigErr)
		c.mu.Lock()
		c.themeError = msg
		c.mu.Unlock()
		settingsMsg := ui.SettingsMsg{Settings: c.SettingsState()}
		return tea.BatchMsg{
			func() tea.Msg { return settingsMsg },
			func() tea.Msg { return ui.NoticeMsg{Error: msg} },
		}
	}
	if !reloaded {
		if expectedGen != 0 && expectedGen != c.themeMgr.Generation() {
			return nil
		}
		settingsMsg := ui.SettingsMsg{Settings: c.SettingsState()}
		return tea.BatchMsg{
			func() tea.Msg { return settingsMsg },
			func() tea.Msg { return ui.NoticeMsg{Activity: "Preset theme active; colors.toml reload skipped"} },
		}
	}

	c.mu.Lock()
	c.appearance.Palette = palette
	c.themeError = ""
	appMsg := c.appearance
	c.mu.Unlock()
	settingsMsg := ui.SettingsMsg{Settings: c.SettingsState()}
	return tea.BatchMsg{
		func() tea.Msg { return appMsg },
		func() tea.Msg { return settingsMsg },
		func() tea.Msg { return ui.NoticeMsg{Activity: "Reloaded theme"} },
	}
}

func (c *Coordinator) toggleGlyphMode() tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.NoticeMsg{Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	c.mu.Lock()
	nextSettings := c.settings
	if nextSettings.GlyphMode == store.GlyphPlain {
		nextSettings.GlyphMode = store.GlyphUnicode
	} else {
		nextSettings.GlyphMode = store.GlyphPlain
	}
	c.mu.Unlock()

	if err := c.store.PutSettings(ctx, nextSettings); err != nil {
		return ui.NoticeMsg{Error: SanitizeError(err)}
	}

	c.mu.Lock()
	c.settings = nextSettings
	c.appearance.Glyphs = mapGlyphMode(nextSettings.GlyphMode)
	appMsg := c.appearance
	c.mu.Unlock()
	settingsMsg := ui.SettingsMsg{Settings: c.SettingsState()}
	return tea.BatchMsg{
		func() tea.Msg { return appMsg },
		func() tea.Msg { return settingsMsg },
		func() tea.Msg { return ui.NoticeMsg{Activity: "Glyph mode: " + string(nextSettings.GlyphMode)} },
	}
}

func (c *Coordinator) cycleSmartRewind() tea.Msg {
	_, ok := c.beginWorker()
	if !ok {
		return ui.NoticeMsg{Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	c.mu.Lock()
	cur := c.smartRewind
	next := smartRewindPresets[0]
	for i, p := range smartRewindPresets {
		if p == cur {
			next = smartRewindPresets[(i+1)%len(smartRewindPresets)]
			break
		}
	}
	c.mu.Unlock()

	if c.player != nil {
		if err := c.player.SetSmartRewind(next); err != nil {
			return ui.NoticeMsg{Error: sanitizePlaybackError(err)}
		}
	}
	c.mu.Lock()
	c.smartRewind = next
	c.mu.Unlock()
	settingsMsg := ui.SettingsMsg{Settings: c.SettingsState()}
	return tea.BatchMsg{
		func() tea.Msg { return settingsMsg },
		func() tea.Msg { return ui.NoticeMsg{Activity: "Smart rewind: " + formatSmartRewind(next)} },
	}
}

func (c *Coordinator) cyclePreRoll() tea.Msg {
	_, ok := c.beginWorker()
	if !ok {
		return ui.NoticeMsg{Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	c.mu.Lock()
	cur := c.preRoll
	next := preRollPresets[0]
	for i, d := range preRollPresets {
		if d == cur {
			next = preRollPresets[(i+1)%len(preRollPresets)]
			break
		}
	}
	c.preRoll = next
	c.mu.Unlock()
	settingsMsg := ui.SettingsMsg{Settings: c.SettingsState()}
	return tea.BatchMsg{
		func() tea.Msg { return settingsMsg },
		func() tea.Msg { return ui.NoticeMsg{Activity: "Bookmark pre-roll: " + formatPreRoll(next)} },
	}
}

func formatSmartRewind(p playback.SmartRewindPolicy) string {
	if !p.Enabled() {
		return "Off"
	}
	return fmt.Sprintf("%ds / %ds", int(p.Threshold/time.Second), int(p.Rewind/time.Second))
}

func formatPreRoll(d time.Duration) string {
	if d <= 0 {
		return "Off"
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}

func sanitizeThemeError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, theme.ErrInvalidThemeName):
		return "Invalid theme name."
	case errors.Is(err, theme.ErrUnknownTheme):
		return "Selected theme is not available."
	case errors.Is(err, theme.ErrUnsafeFile), errors.Is(err, theme.ErrUnsafeRoot):
		return "Theme or config file has unsafe permissions or type."
	case errors.Is(err, theme.ErrMalformedFile):
		return "Theme or config file is malformed; fallback palette retained."
	default:
		return "Theme operation failed; previous palette retained."
	}
}
