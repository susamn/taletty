# Taletty TUI entrypoint (`taletty`)

`app/cmd/taletty` builds the interactive `taletty` terminal application binary
over `app/internal/app`, `app/internal/profile`, `app/internal/terminal`,
`app/internal/ui`, and the pinned `github.com/susamn/taletty/engine v0.1.0`
module.

## Usage

```sh
make build
./bin/taletty --help
./bin/taletty --python /path/to/venv/bin/python
```

- `--python` is optional and resolved lazily via `audible.DiscoverPython`
  (`--python` > `TALETTY_PYTHON` > `python3.12` > `python3`) only when
  running `Doctor`, `Login`, or `Sync`. Cached browsing works completely offline
  without Python or network access.
- Invalid explicit `--python` values or unknown flags fail with usage exit `2`
  without echoing supplied values.
- All user state lives exclusively under `~/.config/taletty/` (`XDG_CONFIG_HOME`
  is ignored and `~/.config/audible-tui/` is never scanned or imported).
- Interactive `Login` cleanly exits and restores `terminal.Run` before handing
  the controlling TTY to `audible.Client.Login`, then resumes the TUI with the
  updated account state and preserved focus/selection.
