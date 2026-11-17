# Distribution and platform support plan

Status: planned, not shipped. Exact minimum versions/runtime strategy require #26.

## Target matrix

| System | Architecture | Planned delivery | Support gate |
|---|---|---|---|
| Linux | amd64 / x86_64 | First release: tar archive + source install (#27); native deb/rpm follow (#28) | Clean install and runtime tests on declared baseline |
| Linux | arm64 / aarch64 | Same artifacts/staging as amd64 | Native/emulated runtime evidence, not just cross-build |
| macOS | amd64 / Intel | Later archive + reviewed Homebrew approach (#29) | Native lifecycle/install/media verification |
| macOS | arm64 / Apple Silicon | Later archive + reviewed Homebrew approach (#29) | Native lifecycle/install/media verification |
| Windows / other OS/architectures | — | Not currently supported or scheduled | Separate feasibility and scoped work required |

No downloadable artifact or supported platform exists yet. Debian package names use
amd64/arm64; RPM uses x86_64/aarch64. Do not mistake naming differences for extra targets.
Minimum distro/libc/macOS/Go/Python/mpv/ffmpeg versions will come from tests in #26,
not guesses in documentation. Cross-compiled artifacts must be distinguished from
runtime-verified targets in release notes.

## Runtime and engine delivery

A Go binary alone is not the full install: the Audible adapter needs pinned Python
dependencies, playback needs mpv, and preparation needs a compatible ffmpeg build.
#26 decides an isolated managed environment versus bundled runtime per artifact.
No silent system-Python mutation, unpinned first-run installs or privileged network
pip calls in package lifecycle scripts. Explain first-install network requirements
separately from offline library/playback after setup.

The app pins a released engine module; engine module/CLI/bridge versions and protocol
must agree. Engine release #8 and clean consumer builds verify independent release
rather than relying on a developer workspace. Keep runtime discovery overrideable,
report missing/incompatible tools in doctor and never expose credentials in diagnostics.

## Packaging and upgrades

- #27: versioned Linux archives/source installer, checksums and dependency manifest/SBOM.
- #28: deb/rpm packages with declared dependencies, architecture checks and install/
  upgrade/remove/purge tests. Keep user auth/history/notes/media on removal.
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
