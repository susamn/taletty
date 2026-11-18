# Terminal presentation foundations

First slice of #12: #47 implements shared geometry and safe raw-text helpers.
The approved [R1 contract](../../../docs/design/final-review.md) is authoritative;
SVG pixels do not determine cell budgets.

- Wide requires both 144 columns and 42 rows: 24-column Library, 42-column Chapters,
  remaining middle width; Books uses floor(2/3 content height), Details the remainder.
- Wide player owns six full-width rows, compact five. Activity and hints own one
  row each; all borders/padding must fit inside these allocations.
- Compact starts at 80×20 with one active content pane; below minimum only Screen
  is allocated for the future resize owner. A pure layout result does not change
  focus, discard drafts or start/stop audio.
- Dialog geometry centers inside content with two-column side/one-row vertical
  margins; at 80×20 the maximum dialog is 76×11. Renderers must scroll/reflow inside
  that budget, not grow into the player.
- ClipLine/PadLine strip untrusted ANSI and flatten controls before measuring with
  the existing ANSI/grapheme library. They accept raw metadata, not styled output.
  Callers choose Unicode/plain tails. Borders/colors are renderer-owned and are
  added separately inside the allocated region.

Tests enforce exact W1/C1/R1 budgets, boundary grids including 144×20/80×42, empty/
negative/extreme dimensions, region separation, dialog bounds and Unicode/CJK/
combining/emoji/control-safe text. These are **J13 geometry/text assertions only**.
They do not constitute a complete UI journey, terminal screenshot or PTY test.

Next #48 adds the real shell model/rendering/miniplayer using these primitives and
[the existing command runner](../testkit/README.md). #49 adds actual modal ownership,
minimum-size draft recovery and PTY key/resize/exit evidence before parent #12 closes.
There are no provider/storage/mpv calls here, no executable preview and no fake
artwork/theme loader. Palette injection/file integration remains #34.
