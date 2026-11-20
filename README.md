# Taletty

A keyboard-first audiobook companion: an offline-first library, chapter browsing,
timestamped notes, and a persistent player. Audible is the first provider; the
application model will also accommodate local audiobooks.

**Status: foundations.** Independent Go modules and their coverage gates exist;
the shell/Help model, private auth/bridge lifetime, SDK auth persistence and typed
runtime Doctor are implemented, with a [typed Go library API](engine/connectors/audible/library.md).
A [source-built headless doctor/login/sync CLI](engine/cmd/taletty-engine/README.md)
is runnable; the Taletty TUI, download/playback integration and installable release are not ready. The working
`audible-tui` prototype remains untouched outside this repository.

## Direction

- Go terminal application using Bubble Tea and Lip Gloss.
- Independently versioned, UI-independent Audible engine and application modules
  in one repository, with the existing Python Audible adapter preserved behind
  a typed boundary.
- Download-first playback through an owned mpv process.
- Bordered panes, contextual hints, predictable vim-style navigation, Unicode
  glyphs with Nerd Font and plain-text alternatives.
- A fixed podmutt-inspired miniplayer, including compact-terminal layouts.
- Local resume, chapter navigation, bookmarks, sleep timer, and download management.
- Linux first. Podmutt-style `colors` theme with Omarchy integration and alternative
  palettes in Settings. Exact layout remains a separate design decision.

## Development policy

**Before starting any issue, read [DEVELOPMENT.md](DEVELOPMENT.md).** Mandatory:
95% independent module/adapter coverage, reuse and clean readable architecture,
and small focused commits with a sensible count per PR.

## Planning documents

- [Product and scope](docs/product.md)
- [Architecture](docs/architecture.md)
- [Interaction contract](docs/interaction-contract.md)
- [Theme direction](docs/design/theming.md)
- [Quality and user journeys](docs/testing.md)
- [Dependency-based delivery plan](docs/roadmap.md)
- [OS/architecture and packaging plan](docs/distribution.md)
- [Audible/player feature stories](docs/player-features.md)
- [Contribution and PR policy](CONTRIBUTING.md)
- [Security](SECURITY.md)

## Quality commitment

Every production Go module and the Python adapter must meet **95% statement
coverage independently** once executable code lands. Branch coverage is reported
as an additional signal. Automated user-journey tests are required alongside
unit, integration, terminal-layout, and failure/recovery tests. Current module/tooling results cover only the implemented foundations, not future
player and provider-operation features. See [local verification and CI](tools/README.md).

Audible integration is unofficial. Credentials, signed URLs, licenses, and
account-specific data must never enter the repository, CI artifacts, or logs.
Only download content you are entitled to; respect applicable law and provider
terms.

## License

[MIT](LICENSE). Preserve third-party copyright/license notices when reusing code;
the project license does not relicense dependencies or grant rights to Audible media.
