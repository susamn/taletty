# Theme direction

Status: maintainer-approved direction; implementation pending. The Settings layout
and recovery behavior are consolidated in the [final #2 review](final-review.md).

## Podmutt-compatible palettes

Use semantic palette fields rather than hard-coded widget colors. Follow podmutt's
flat `key = "value"` `colors.toml` vocabulary: background/foreground variants,
accent, selection, muted, categorical colors and brighter variants. Preserve MIT
notices when reusing source. Do not import podmutt's app/profile dependencies.

```text
~/.config/taletty/
  config.toml                 theme = "colors" by default
  colors.toml                 active desktop/custom palette
  themes/
    solarized-dark.toml
    catppuccin.toml
```

`colors` means Taletty's own `colors.toml`, not an unnamed hard-coded preset.
In Omarchy, integrate by generating/updating this file from the desktop theme and
requesting a reload. Omarchy hook installation must be explicit, documented and
idempotent; selecting `colors` alone does not magically install a desktop hook.
Never read the old Audible app or borrow podmutt's runtime config directory.
No Omarchy dependency is required outside that desktop environment.

## Explicit Omarchy hook

To sync Omarchy theme changes into Taletty without touching Taletty's `config.toml`
or interrupting playback, install an explicit idempotent hook (for example in
`~/.config/omarchy/hooks/theme-set.d/50-taletty.sh` or an explicit user script):

```sh
#!/bin/sh
set -eu

src="${1:-$HOME/.config/omarchy/current/theme/colors.toml}"
dst_dir="$HOME/.config/taletty"
dst="$dst_dir/colors.toml"

[ -r "$src" ] || exit 0
mkdir -p -m 0700 "$dst_dir"
tmp="$(mktemp "$dst_dir/.colors.toml.tmp.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
chmod 0600 "$tmp"
cp "$src" "$tmp"
mv -f "$tmp" "$dst"
trap - EXIT

pkill -USR1 -x taletty >/dev/null 2>&1 || true
```

Alternatively, users who prefer desktop-managed symlinks may link
`~/.config/taletty/colors.toml` to `~/.config/omarchy/current/theme/colors.toml`;
Taletty's initial seeding (`O_CREAT|O_EXCL|O_NOFOLLOW`) never overwrites an existing
file or symlink. Sending `SIGUSR1` reloads `colors.toml` live when `theme = "colors"`
is active and leaves explicit presets (`solarized-dark`, `catppuccin`) selected.

## Settings

Offer `colors`, built-in `solarized-dark` and `catppuccin`, plus valid custom
`themes/<name>.toml` palettes. Keep `colors` first; sort/deduplicate other names.
Selecting a theme applies it live and persists only after a successful config write;
report failure without losing the previous selection. Settings should display the
active palette source and explain missing/invalid-file fallback.

Initial file seeding must not overwrite existing files or replace desktop-managed
symlinks. Use safe theme-name validation (no path traversal), validated colors and
private Taletty files. Missing fields inherit a documented fallback palette;
malformed values never inject terminal escapes. Failed reload must retain a usable
palette and expose a nonblocking diagnostic, not crash or disrupt audio.

Support explicit `SIGUSR1` reload on Linux, following podmutt. Repaint all views,
modals, hints and focus/selection styling consistently. Signal handling belongs in
the app layer; palette parsing/loading remains independently testable. Do not add
a filesystem watcher unless separately justified. Reload active `colors` after an
Omarchy update; an explicitly selected preset stays selected.

## Required tests

Parser/serializer/defaults; safe names/colors; built-in/custom discovery; missing/
invalid/partial files; seed without overwrite; symlink preservation; settings
apply/save/failure; signal dispatch and stale reload protection; all widget styles;
reload while playing, paused, filtering and editing. Test using temporary homes,
not the user's live Omarchy setup. Independent >=95% module coverage and relevant
UI journeys remain required. Exact palette-independent geometry/key layout is #2.
