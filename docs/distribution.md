# Distribution and platform support plan

Status: runtime/platform contract accepted in [ADR 003](adr/003-runtime-and-platform-matrix.md)
(#26); downloadable artifacts and native packages remain planned, not shipped
(#27, #28, #29, #24).

## Target matrix

| System | Architecture | Planned delivery | Current status & support gate |
|---|---|---|---|
| Linux | amd64 / x86_64 | First release: tar archive + source install (#27); native deb/rpm follow (#28) | Runtime-verified in CI (Ubuntu 24.04 / glibc 2.39); clean install/upgrade/removal tests required in #27/#28/#24 |
| Linux | arm64 / aarch64 | Same artifacts/staging as amd64 (#27, #28) | Source cross-build verified only (`GOOS=linux GOARCH=arm64 CGO_ENABLED=0`); native/emulated runtime evidence required in #27/#28 before release support claim |
| macOS | amd64 / Intel | Later archive + reviewed Homebrew approach (#29) | Staged later; Darwin implementations and native lifecycle/install/media verification required (#29) |
| macOS | arm64 / Apple Silicon | Later archive + reviewed Homebrew approach (#29) | Staged later; Darwin implementations and native lifecycle/install/media verification required (#29) |
| Windows / other OS/architectures | — | Explicitly unsupported and unscheduled | Separate feasibility, security model, and scoped work required |

No downloadable artifact or supported release exists yet. Debian package names use
amd64/arm64; RPM uses x86_64/aarch64. Do not mistake naming differences for extra targets.
Tested-evidence minimum versions recorded in [ADR 003](adr/003-runtime-and-platform-matrix.md):
Go `1.26.0+` (CI verified `1.26.11` & `1.27.4`), CPython `3.12` (CI `3.12.16`, local `3.12.3`),
Ubuntu 24.04 / `glibc 2.39`, SQLite `>=3.38` (embedded `3.51.3` in `app`, `3.45.1` in schema oracle),
`ffmpeg 7:6.1.1` / `ffprobe`, `mpv 0.37.0`, and `audible==0.13.1`. Cross-compiled artifacts
must be distinguished from runtime-verified targets in release notes; Go cross-builds
never prove runtime support.

## Runtime and engine delivery

A Go binary alone is not the full install: the Audible adapter needs pinned Python
dependencies, playback needs `mpv`, and preparation needs a compatible `ffmpeg`/`ffprobe` build.
Per [ADR 003](adr/003-runtime-and-platform-matrix.md), Taletty standardizes on an
**isolated managed CPython 3.12 virtual environment** pinned to
[`requirements.lock`](../engine/connectors/audible/internal/bridge/adapter/requirements.lock)
for source installs, Linux tar archives (#27), and `.deb`/`.rpm` packages (#28),
with optional reproducibly staged bundled runtime packaging permitted for standalone
tarballs (#27) or macOS (#29). No silent system-Python mutation, unpinned first-run
installs, or privileged network `pip` calls in package lifecycle scripts are permitted.
Network access is required only for initial dependency/package installation and
explicit Audible `login`/`sync`/`download` operations; cached library browsing and
local `mpv` playback work completely offline after setup.

Executable names are `taletty` (primary TUI/app binary, `app/cmd/taletty`) and
`taletty-engine` (headless engine CLI). The app pins the released engine module
(`github.com/susamn/taletty/engine v0.1.0`); engine module/CLI/bridge versions
and protocol must agree. Engine release #8
prepares the first independently consumable engine release (`engine/v0.1.0`,
`ContractVersion = "v0.1.0"`, `BridgeProtocolVersion = 1`,
`PinnedPythonVersion = "3.12"`, `PinnedAudibleVersion = "0.13.1"`), adds
`ResolvePython`/`DiscoverPython` (`--python` > `TALETTY_PYTHON` > `python3.12` >
`python3`, never scanning `~/.config/audible-tui` or mutating system Python), adds
`OpenBorrowed` for caller-owned cooperative root handoff, and verifies clean
temp-dir `go install ./cmd/taletty-engine` plus an ephemeral external consumer
module outside the workspace. See [the engine README](../engine/README.md) for
prerequisites (Go `1.26.0+`, CPython `3.12` +
[`requirements.txt`](../engine/connectors/audible/internal/bridge/adapter/requirements.txt) /
[`requirements.lock`](../engine/connectors/audible/internal/bridge/adapter/requirements.lock),
`ffmpeg`/`ffprobe`), source install steps, and post-merge maintainer tagging
(`git tag -a engine/v0.1.0 ...`). Keep runtime discovery overrideable, report
missing/incompatible tools in doctor and never expose credentials in diagnostics.

## Packaging and upgrades

- #27: versioned Linux archives/source installer, SHA-256 `SHA256SUMS` checksums,
  SPDX/CycloneDX SBOMs, and build provenance attestations.
- #28: deb/rpm packages with declared dependencies, architecture checks and install/
  upgrade/remove/purge tests. Keep user auth/history/notes/media under `~/.config/taletty/`
  on upgrade and removal; explicit purge remains separate and never sweeps user home directories.
- #29: macOS audit, archives and explicit formula-versus-cask/signing decisions.
  A CLI normally uses a formula; an app bundle must be intentional. Disclose unsigned
  status rather than promising notarization that does not exist.
- Signed apt/rpm repositories, AUR, Nix, auto-update and Windows are separate future
  distribution stories, not implied by producing standalone packages.
- Fresh private data-root setup is #10. All configuration, credentials, cache,
  metadata/progress and downloads live under `~/.config/taletty/`. No old app paths
  are scanned/imported/reused. Taletty-owned data survives upgrades/uninstall.
  Installing the app must not register a device; explicit sign-in owns registration.
- Never package credentials, protected media, local caches or virtual environments
  copied from a developer machine. Build the selected runtime reproducibly.

## Release evidence

First core release #24 is blocked by Linux artifacts #27 and capability audit #31.
Native packages and macOS are later, explicitly gated deliveries, not requirements
silently applied to the Linux-first release. Each claimed target needs install,
upgrade, doctor, generated-media playback/chapter/seek and cleanup evidence.
Linux container/emulation checks must document limits, especially real audio/device
integration. Desktop media keys (#30) are app features, not prerequisites for archives.

All implementation retains independent >=95% coverage and small PR policy. Broad
platform stories are decomposed into dependent child tasks before coding.
