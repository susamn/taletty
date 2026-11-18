package ui_test

import (
	"strings"
	"testing"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/susamn/taletty/app/internal/ui"
)

func TestRawMetadataCannotInjectTerminalControls(t *testing.T) {
	for _, sample := range []struct{ input, want string }{
		{"plain book", "plain book"},
		{"\x1b[31mBook\x1b[0m", "Book"},
		{"\x1b]0;unsafe window title\x07Book", "Book"},
		{"\x1b]8;;https://example.invalid\x1b\\Book\x1b]8;;\x1b\\", "Book"},
		{"Line\nBreak\rTab\tEnd", "Line Break Tab End"},
		{"\x00Book\x7f", " Book "},
	} {
		if got := ui.ClipLine(sample.input, 100, "…"); got != sample.want {
			t.Fatalf("sanitize %q: got %q, want %q", sample.input, got, sample.want)
		}
	}
	if got := ui.ClipLine("untrusted book", 4, "\x1b[31m...\x1b[0m"); got != "u..." {
		t.Fatalf("tail was not sanitized: %q", got)
	}
}

func TestClippingAndPaddingUseCellsAcrossGlyphModes(t *testing.T) {
	texts := []string{
		"", "Book", strings.Repeat("Long title ", 50), "河流與星辰", "Cafe\u0301",
		"👩‍💻 travels", "\xffinvalid UTF-8", "\x1b[2J\x1b[31m界\nTitle\x1b[0m",
	}
	for _, text := range texts {
		for _, tail := range []string{"", "…", "...", "界界"} {
			for width := -1; width <= 16; width++ {
				clipped := ui.ClipLine(text, width, tail)
				padded := ui.PadLine(text, width, tail)
				if ansi.StringWidth(clipped) > max(0, width) || ansi.StringWidth(padded) != max(0, width) {
					t.Fatalf("cell overflow text=%q tail=%q width=%d: clip=%q pad=%q", text, tail, width, clipped, padded)
				}
				if strings.ContainsAny(clipped, "\x1b\n\r\t") || strings.IndexFunc(padded, unicode.IsControl) >= 0 {
					t.Fatalf("unsafe output: %q / %q", clipped, padded)
				}
			}
		}
	}
	if got := ui.ClipLine("A界B", 3, ""); got != "A界" {
		t.Fatalf("split a wide character: %q", got)
	}
	if got := ui.PadLine("界", 4, "..."); got != "界  " {
		t.Fatalf("incorrect padding: %q", got)
	}
	if got := ui.ClipLine("Cafe\u0301", 4, "…"); got != "Cafe\u0301" {
		t.Fatalf("split a combining grapheme: %q", got)
	}
	if got := ui.ClipLine("Long title", 6, "…"); got != "Long …" {
		t.Fatalf("Unicode ellipsis: %q", got)
	}
}
