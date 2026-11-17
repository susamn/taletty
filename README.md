# Taletty

A keyboard-first audiobook companion: an offline-first library, chapter browsing,
timestamped notes, and a persistent player. Audible is the first provider; the
application model will also accommodate local audiobooks.

**Status: planning.** No Taletty application or installable release exists yet.
The working `audible-tui` prototype remains untouched outside this repository.

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
- Linux first. Exact visual design remains a separate decision.

## Planning documents

- [Product and scope](docs/product.md)
- [Architecture](docs/architecture.md)
- [Interaction contract](docs/interaction-contract.md)
- [Quality and user journeys](docs/testing.md)
- [Dependency-based delivery plan](docs/roadmap.md)
- [Contribution and PR policy](CONTRIBUTING.md)
- [Security](SECURITY.md)

## Quality commitment

Every production Go module and the Python adapter must meet **95% statement
coverage independently** once executable code lands. Branch coverage is reported
as an additional signal. Automated user-journey tests are required alongside
unit, integration, terminal-layout, and failure/recovery tests. Until executable
code and the coverage gates exist, this repository must not claim this target
has been achieved.

Audible integration is unofficial. Credentials, signed URLs, licenses, and
account-specific data must never enter the repository, CI artifacts, or logs.
Only download content you are entitled to; respect applicable law and provider
terms.
