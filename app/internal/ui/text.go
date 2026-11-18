package ui

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// cleanLine removes untrusted terminal sequences and flattens control characters.
// These helpers accept raw metadata, not trusted renderer-owned ANSI styles.
func cleanLine(text string) string {
	return strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, ansi.Strip(text))
}

// ClipLine fits raw text to a terminal-cell budget. Callers choose a Unicode or
// plain tail (for example, "…" or "..."); even an oversized tail stays bounded.
func ClipLine(text string, width int, tail string) string {
	if width <= 0 {
		return ""
	}
	tail = ansi.Truncate(cleanLine(tail), width, "")
	return ansi.Truncate(cleanLine(text), width, tail)
}

// PadLine clips first, then pads to exactly width cells, never bytes or runes.
func PadLine(text string, width int, tail string) string {
	line := ClipLine(text, width, tail)
	return line + strings.Repeat(" ", max(0, width-ansi.StringWidth(line)))
}
