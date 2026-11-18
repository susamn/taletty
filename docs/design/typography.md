# Typography — a story-player terminal profile

Status: maintainer approved the recommendation in the #2 / PR #43 design review.
This is presentation guidance, not a font dependency or an implemented Settings option.

## Recommended font: Iosevka Slab

Use **Iosevka Slab**, the monospaced slab-serif family, for a more literary,
bookish appearance while retaining aligned lists and terminal-cell geometry.
Use regular for body/metadata and bold sparingly for book titles, focus and headings.
Keep comfortable line spacing in the terminal profile; no forced pixel size in Taletty.

Official sources:

- [Iosevka project](https://github.com/be5invis/Iosevka)
- [Downloads](https://github.com/be5invis/Iosevka/releases)
- [Preview version v34.9.1](https://github.com/be5invis/Iosevka/releases/tag/v34.9.1),
  `PkgTTF-IosevkaSlab-34.9.1.zip` (regular and bold used for these PNG exports).

Iosevka is separately licensed under **SIL Open Font License 1.1**, not Taletty's
MIT license. Fonts are not copied into the repository or shipped with the app.
The SVG source requests `Iosevka Slab, monospace`; when the font is absent it falls
back. PNG previews already contain the rendered lettering and need no font install.
Do not substitute **Iosevka Etoile**: it is quasi-proportional, not the fixed-cell
slab family recommended for the actual TUI.

## Who controls the font?

The terminal emulator renders the font. A standard Bubble Tea TUI cannot reliably
set its own family, size or line spacing. There is no Taletty `font =` setting.
Never emit terminal-specific font-changing commands, install fonts silently or
rewrite a user's global terminal configuration.

For an optional **Taletty terminal profile**:

1. Install Iosevka Slab through your trusted OS font package or official downloads.
2. Select the family in a separate terminal profile/window configuration.
3. Choose a comfortable size and line spacing there; leave other profiles unchanged.
4. Launch Taletty in that profile. Exact syntax depends on the user's terminal;
   do not publish an untested universal terminal command.

No profile is required. Any correctly functioning monospace font remains supported;
Nerd Fonts are optional, not needed to read state or operate controls. A terminal
profile is separate from Taletty's semantic `colors.toml` and Omarchy palette hook.

## Typography inside Taletty

- Hierarchy through book titles, modest bold emphasis, spacing and muted metadata.
- Use plain book titles in Details/Chapters; do not expose developer-state badges
  such as `SELECTED / INSPECTED`. Keep Playing/Paused state explicit.
- A full-width player distributes identity, timing, slider, controls and status;
  font choice must not be used to conceal wasted layout space or overflow.
- Measure terminal cells, not pixels; avoid dependence on ligatures or proportional
  character widths. Test Unicode/CJK/combining metadata, plain text, long titles and
  missing glyphs independently of these rendered previews.
- Focus, selection, favorite and failure state must remain readable without color,
  italics or a particular font. Preserve `[Fav]` and text-state alternatives.
