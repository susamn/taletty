// Package theme resolves Taletty's semantic palettes, presets, and colors.toml
// configuration.
//
// Palette semantics, built-in presets (Default, Solarized Dark, Catppuccin Mocha),
// and flat key = "value" colors.toml conventions are adapted from podmutt
// (MIT License). Do not import podmutt packages or discover external app configs.
package theme

import (
	"bytes"
	"errors"
	"fmt"
	"image/color"
	"strings"

	"github.com/susamn/taletty/app/internal/ui"
)

const (
	// DefaultTheme loads Taletty-owned ~/.config/taletty/colors.toml.
	DefaultTheme = "colors"
	// SolarizedDarkTheme selects the built-in Solarized Dark preset.
	SolarizedDarkTheme = "solarized-dark"
	// CatppuccinTheme selects the built-in Catppuccin Mocha preset.
	CatppuccinTheme = "catppuccin"
)

const maxConfigBytes = 64 * 1024

var (
	ErrInvalidThemeName = errors.New("invalid theme name")
	ErrUnknownTheme     = errors.New("theme is not available")
	ErrUnsafeFile       = errors.New("unsafe theme or configuration file")
	ErrMalformedFile    = errors.New("malformed theme or configuration file")
)

var recognizedColorKeys = map[string]struct{}{
	"background": {}, "text": {}, "border": {}, "focus": {}, "selection": {}, "muted": {}, "error": {},
	"foreground": {}, "dark_foreground": {}, "light_foreground": {}, "bright_foreground": {},
	"dark_background": {}, "darker_background": {}, "lighter_background": {},
	"accent": {}, "cursor": {}, "selection_background": {}, "selection_foreground": {},
	"red": {}, "yellow": {}, "orange": {}, "green": {}, "cyan": {}, "blue": {}, "magenta": {}, "brown": {},
	"bright_red": {}, "bright_yellow": {}, "bright_green": {}, "bright_cyan": {}, "bright_blue": {}, "bright_magenta": {},
	"color0": {}, "color1": {}, "color2": {}, "color3": {}, "color4": {}, "color5": {}, "color6": {}, "color7": {},
	"color8": {}, "color9": {}, "color10": {}, "color11": {}, "color12": {}, "color13": {}, "color14": {}, "color15": {},
}

// BuiltInThemes returns the built-in theme choices in Settings display order.
func BuiltInThemes() []string {
	return []string{DefaultTheme, SolarizedDarkTheme, CatppuccinTheme}
}

// ValidThemeName reports whether name matches ^[a-z0-9][a-z0-9_-]{0,62}$.
func ValidThemeName(name string) bool {
	if len(name) < 1 || len(name) > 63 {
		return false
	}
	if !isLowerAlnum(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !isLowerAlnum(c) && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// IsReservedTheme reports whether name is one of the built-in reserved names.
func IsReservedTheme(name string) bool {
	return name == DefaultTheme || name == SolarizedDarkTheme || name == CatppuccinTheme
}

// ValidCustomThemeName reports whether name is a safe custom theme identifier
// that does not collide with reserved built-in names.
func ValidCustomThemeName(name string) bool {
	return ValidThemeName(name) && !IsReservedTheme(name)
}

func isLowerAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// DefaultPalette returns Taletty's readable default dark fallback palette.
func DefaultPalette() ui.Palette {
	return ui.Palette{
		Background: color.RGBA{R: 0x0c, G: 0x10, B: 0x17, A: 0xff},
		Text:       color.RGBA{R: 0xe7, G: 0xe9, B: 0xee, A: 0xff},
		Border:     color.RGBA{R: 0x58, G: 0x62, B: 0x72, A: 0xff},
		Focus:      color.RGBA{R: 0x00, G: 0xff, B: 0x00, A: 0xff},
		Selection:  color.RGBA{R: 0x26, G: 0x64, B: 0xff, A: 0xff},
		Muted:      color.RGBA{R: 0x7c, G: 0x88, B: 0x9b, A: 0xff},
		Error:      color.RGBA{R: 0xf3, G: 0x8b, B: 0xa8, A: 0xff},
	}
}

// SolarizedDarkPalette returns the built-in Solarized Dark semantic palette.
func SolarizedDarkPalette() ui.Palette {
	return ui.Palette{
		Background: color.RGBA{R: 0x00, G: 0x2b, B: 0x36, A: 0xff},
		Text:       color.RGBA{R: 0x83, G: 0x94, B: 0x96, A: 0xff},
		Border:     color.RGBA{R: 0x58, G: 0x6e, B: 0x75, A: 0xff},
		Focus:      color.RGBA{R: 0x26, G: 0x8b, B: 0xd2, A: 0xff},
		Selection:  color.RGBA{R: 0xfd, G: 0xf6, B: 0xe3, A: 0xff},
		Muted:      color.RGBA{R: 0x65, G: 0x7b, B: 0x83, A: 0xff},
		Error:      color.RGBA{R: 0xdc, G: 0x32, B: 0x2f, A: 0xff},
	}
}

// CatppuccinPalette returns the built-in Catppuccin Mocha semantic palette.
func CatppuccinPalette() ui.Palette {
	return ui.Palette{
		Background: color.RGBA{R: 0x1e, G: 0x1e, B: 0x2e, A: 0xff},
		Text:       color.RGBA{R: 0xcd, G: 0xd6, B: 0xf4, A: 0xff},
		Border:     color.RGBA{R: 0x58, G: 0x5b, B: 0x70, A: 0xff},
		Focus:      color.RGBA{R: 0xa6, G: 0xe3, B: 0xa1, A: 0xff},
		Selection:  color.RGBA{R: 0x89, G: 0xb4, B: 0xfa, A: 0xff},
		Muted:      color.RGBA{R: 0xa6, G: 0xad, B: 0xc8, A: 0xff},
		Error:      color.RGBA{R: 0xf3, G: 0x8b, B: 0xa8, A: 0xff},
	}
}

// Preset returns a built-in palette by name. Unrecognized names return DefaultPalette and false.
func Preset(name string) (ui.Palette, bool) {
	switch name {
	case DefaultTheme:
		return DefaultPalette(), true
	case SolarizedDarkTheme:
		return SolarizedDarkPalette(), true
	case CatppuccinTheme:
		return CatppuccinPalette(), true
	default:
		return DefaultPalette(), false
	}
}

// ParseHexColor strictly validates an ASCII #RRGGBB color string.
func ParseHexColor(raw string) (color.RGBA, error) {
	if len(raw) != 7 || raw[0] != '#' {
		return color.RGBA{}, errors.New("color must be #RRGGBB hex")
	}
	r, okR := parseHexByte(raw[1], raw[2])
	g, okG := parseHexByte(raw[3], raw[4])
	b, okB := parseHexByte(raw[5], raw[6])
	if !okR || !okG || !okB {
		return color.RGBA{}, errors.New("color must contain ASCII hex digits")
	}
	return color.RGBA{R: r, G: g, B: b, A: 0xff}, nil
}

func parseHexByte(hi, lo byte) (byte, bool) {
	a, okA := hexNibble(hi)
	b, okB := hexNibble(lo)
	return (a << 4) | b, okA && okB
}

func hexNibble(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	default:
		return 0, false
	}
}

// FormatHexColor renders a color as lowercase #rrggbb hex.
func FormatHexColor(c color.Color) string {
	if c == nil {
		return "#000000"
	}
	nrgba := color.NRGBAModel.Convert(c).(color.NRGBA)
	return fmt.Sprintf("#%02x%02x%02x", nrgba.R, nrgba.G, nrgba.B)
}

// SerializePalette encodes Taletty's 7 semantic slots as flat key = "value" TOML.
func SerializePalette(p ui.Palette) string {
	base := DefaultPalette()
	fill := func(c, fallback color.Color) color.Color {
		if c == nil {
			return fallback
		}
		return c
	}
	var b strings.Builder
	b.WriteString("# Taletty semantic palette (flat key = \"#RRGGBB\" TOML)\n\n")
	write := func(comment, key string, c color.Color) {
		fmt.Fprintf(&b, "# %s\n%s = %q\n\n", comment, key, FormatHexColor(c))
	}
	write("Base background color.", "background", fill(p.Background, base.Background))
	write("Primary text color.", "text", fill(p.Text, base.Text))
	write("Unfocused pane and dialog border color.", "border", fill(p.Border, base.Border))
	write("Focused pane border and active highlight color.", "focus", fill(p.Focus, base.Focus))
	write("Selected row foreground color.", "selection", fill(p.Selection, base.Selection))
	write("Secondary metadata, activity, and hint color.", "muted", fill(p.Muted, base.Muted))
	write("Sticky error and alert color.", "error", fill(p.Error, base.Error))
	return b.String()
}

// ParsePalette parses flat TOML key = "value" entries (supporting both Taletty
// semantic keys and Omarchy/Alacritty/base16/podmutt color keys) onto a readable
// default fallback palette. Invalid values or syntax never inject control
// characters and report a sanitized error alongside the safe fallback palette.
func ParsePalette(data []byte) (ui.Palette, error) {
	p := DefaultPalette()
	if len(data) > maxConfigBytes {
		return p, fmt.Errorf("%w: file exceeds %d bytes", ErrMalformedFile, maxConfigBytes)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return p, fmt.Errorf("%w: NUL byte not permitted", ErrMalformedFile)
	}

	parsed := make(map[string]color.RGBA)
	var problems []string
	validColorKeys := 0

	lines := strings.Split(string(data), "\n")
	for idx, rawLine := range lines {
		lineNum := idx + 1
		line := strings.TrimSuffix(rawLine, "\r")
		if hasUnsafeControl(line) {
			problems = append(problems, fmt.Sprintf("line %d: control characters rejected", lineNum))
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, val, ok := parseKeyValue(trimmed)
		if !ok {
			problems = append(problems, fmt.Sprintf("line %d: invalid key-value syntax", lineNum))
			continue
		}
		if key == "mode" {
			continue
		}
		if _, known := recognizedColorKeys[key]; !known {
			continue
		}
		rgba, err := ParseHexColor(val)
		if err != nil {
			problems = append(problems, fmt.Sprintf("line %d (%s): %v", lineNum, key, err))
			continue
		}
		parsed[key] = rgba
		validColorKeys++
	}

	pick := func(dst *color.Color, keys ...string) {
		for _, k := range keys {
			if c, ok := parsed[k]; ok {
				*dst = c
				return
			}
		}
	}
	pick(&p.Background, "background", "darker_background", "dark_background", "color0")
	pick(&p.Text, "text", "foreground", "light_foreground", "color7", "color15")
	pick(&p.Border, "border", "muted", "dark_foreground", "lighter_background", "color8")
	pick(&p.Focus, "focus", "accent", "cursor", "color4", "blue", "color2", "green", "color6", "cyan")
	pick(&p.Selection, "selection", "selection_foreground", "selection_background", "accent", "color6", "color12", "lighter_background")
	pick(&p.Muted, "muted", "dark_foreground", "color8", "color7")
	pick(&p.Error, "error", "red", "bright_red", "color1", "color9")

	if len(problems) > 0 {
		return p, fmt.Errorf("%w: %s", ErrMalformedFile, strings.Join(problems, "; "))
	}
	if validColorKeys == 0 {
		return p, fmt.Errorf("%w: no valid color entries", ErrMalformedFile)
	}
	return p, nil
}

func hasUnsafeControl(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if (b < 0x20 && b != '\t') || b == 0x7f {
			return true
		}
	}
	return false
}

func parseKeyValue(line string) (string, string, bool) {
	eq := strings.IndexByte(line, '=')
	if eq <= 0 {
		return "", "", false
	}
	key := strings.ToLower(strings.TrimSpace(line[:eq]))
	if !validTOMLKey(key) {
		return "", "", false
	}
	rawVal := strings.TrimSpace(line[eq+1:])
	if rawVal == "" {
		return "", "", false
	}
	if rawVal[0] == '"' || rawVal[0] == '\'' {
		quote := rawVal[0]
		end := strings.IndexByte(rawVal[1:], quote)
		if end < 0 {
			return "", "", false
		}
		rest := strings.TrimSpace(rawVal[end+2:])
		if rest != "" && !strings.HasPrefix(rest, "#") {
			return "", "", false
		}
		return key, rawVal[1 : end+1], true
	}
	if idx := strings.Index(rawVal, " #"); idx >= 0 {
		rawVal = strings.TrimSpace(rawVal[:idx])
	} else if idx := strings.Index(rawVal, "\t#"); idx >= 0 {
		rawVal = strings.TrimSpace(rawVal[:idx])
	}
	return key, rawVal, rawVal != ""
}

func validTOMLKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if !isLowerAlnum(c) && c != '_' {
			return false
		}
	}
	return true
}
