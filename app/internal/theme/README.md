# Semantic themes and `colors.toml` loader

`app/internal/theme` implements Taletty's semantic color palettes, preset themes,
descriptor-anchored `config.toml` / `colors.toml` loading and persistence,
safe custom theme discovery, and `SIGUSR1` live reload coordination.

Palette semantics, built-in presets, and flat `key = "value"` `colors.toml`
conventions are adapted from podmutt (`internal/theme/theme.go`, MIT License)
without importing podmutt packages or reading external app configurations.

## Supported keys and fallback behavior

- **Taletty semantic keys** (highest precedence): `background`, `text`, `border`,
  `focus`, `selection`, `muted`, `error`.
- **Omarchy / Alacritty / base16 / podmutt keys**: `foreground`, `accent`,
  `cursor`, `selection_background`, `selection_foreground`, `dark_foreground`,
  `light_foreground`, `lighter_background`, `dark_background`,
  `darker_background`, `red`, `bright_red`, `blue`, `green`, `cyan`, and
  `color0`..`color15`.
- **Strict validation**: only `#RRGGBB` ASCII hex colors are accepted. Control
  characters, ANSI escape sequences, named colors, NUL bytes, and malformed lines
  are rejected cleanly and fall back to the readable default palette without
  crashing or injecting terminal escapes.
- **Partial files**: missing slots inherit from `DefaultPalette()`.

## Built-in and custom catalog

1. `colors` (default; reads `~/.config/taletty/colors.toml`, including desktop-managed symlinks)
2. `solarized-dark` (built-in Solarized Dark)
3. `catppuccin` (built-in Catppuccin Mocha)
4. Custom `themes/<name>.toml` entries where `<name>` matches
   `^[a-z0-9][a-z0-9_-]{0,62}$` and does not collide with reserved built-in
   names, sorted lexicographically and deduplicated. Symlinks with path
   traversal (`/`, `..`) and hidden files are ignored.

## Anchored root operations

All filesystem operations operate relative to a borrowed `0700` `*os.File`
directory descriptor (`openat`, `fstatat`, `renameat`, `O_NOFOLLOW`, `O_CLOEXEC`):

- `SeedDefaults(root)` creates `themes/` (`0700`), `colors.toml` (`0600`), and
  `config.toml` (`theme = "colors"`, `0600`) with `O_CREAT|O_EXCL|O_NOFOLLOW`,
  never overwriting existing files or desktop-managed symlinks.
- `LoadConfig(root)` reads `config.toml` (`0600` regular file, `<= 64 KiB`, no symlinks).
- `SaveThemeSelection(root, name)` validates `name` against `Catalog(root)` and
  atomically updates `theme = "<name>"` via temp file + `renameat` while
  preserving other lines and comments in `config.toml`.
- `ReloadOnSignal(root, currentTheme)` reloads `colors.toml` (or an active
  custom theme) on `SIGUSR1`, leaves explicit presets (`solarized-dark`,
  `catppuccin`) selected, and lets callers retain the previous usable palette
  if the reloaded file is malformed.
- `Manager` coordinates candidate validation, save-before-commit persistence,
  and generation-checked (`stale`-safe) signal reloads.

## Explicit Omarchy hook

Selecting `colors` never installs desktop hooks automatically. Users on Omarchy
can install an explicit, idempotent hook that atomically writes
`~/.config/taletty/colors.toml` and sends `SIGUSR1`:

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
