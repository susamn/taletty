# ADR 003: OS/architecture support matrix and Python/runtime packaging

Status: **accepted for #26** (runtime packaging and platform matrix contract;
downloadable release artifacts and native packages remain open under `#27`,
`#28`, `#29`, and `#24`). Depends on accepted [ADR 001](001-engine-api-and-bridge-contract.md)
(`#1`), accepted [ADR 002](002-storage-contract.md) (`#3`), quality/coverage
gates (`#4`), and engine release preparation (`#8`).

Documentation prose in this ADR reports runtime coverage as N/A; reproducible
lockfile and contract verification tests live in
[`tests/tooling/test_packaging_contract.py`](../../tests/tooling/test_packaging_contract.py)
under the `coverage-tooling` gate without adding unmeasured production Python
source.

## Context

Taletty ships two independently versioned Go modules (`github.com/susamn/taletty/engine`
and `github.com/susamn/taletty/app`) plus an embedded Python Audible adapter
(`engine/connectors/audible/internal/bridge/adapter/`) and external media tools
(`ffmpeg`, `ffprobe`, `mpv`). A compiled Go binary alone is not a complete
runtime installation:

- The Audible connector executes embedded Python source via `python -I -B` and
  requires `audible==0.13.1` plus its transitive runtime dependencies (including
  `pillow==13.0.0`, which ships native C-extension wheels tied to CPython 3.12
  and target OS/architecture).
- Media preparation and packet validation require `ffmpeg` and `ffprobe`.
- Audio playback requires `mpv` with `--input-ipc-client=fd://3`.
- Current OS-specific Go implementations (`*_linux.go`) rely on Linux primitives
  (`openat`, `O_NOATIME`, `renameat2`, `waitid(WNOWAIT)`, `/proc/self/fd`, Linux
  `termios`, `socketpair`, and OFD byte-range locks).

Before `#27`, `#28`, and `#29` build distributable artifacts, Taletty must lock
down supported targets, tested minimum versions, runtime packaging boundaries,
offline guarantees, data preservation rules, and release provenance requirements.

## OS and architecture support matrix

Debian packaging uses `amd64`/`arm64` while RPM packaging uses `x86_64`/`aarch64`;
these are naming conventions for the same two Linux architectures, not extra
targets.

| System | Architecture (Go / Debian / RPM) | Delivery track | Current verification status | Release gate |
|---|---|---|---|---|
| Linux | `linux/amd64` / `amd64` / `x86_64` | First release: tar archive + source install (`#27`); native `.deb`/`.rpm` follow (`#28`) | **Runtime-verified in CI** on Ubuntu 24.04 (`glibc 2.39`) across Go `1.26.11` & `1.27.4`, CPython `3.12.16`, `audible==0.13.1`, `ffmpeg 7:6.1.1`, `mpv 0.37.0`, and embedded SQLite `3.51.3` | Clean install, upgrade, `doctor`, generated-media playback/download, and removal/purge tests (`#27`, `#28`, `#24`) |
| Linux | `linux/arm64` / `arm64` / `aarch64` | First release target alongside `amd64` (`#27`, `#28`) | **Source cross-build verified only** (`GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` compiles `engine` and `app`). **Not runtime-verified.** | Native or emulated `linux/arm64` runtime verification (CPython 3.12 + `aarch64` `pillow==13.0.0` wheel, `audible==0.13.1`, `ffmpeg`/`ffprobe`, `mpv --ao=null`, `wazero` SQLite, and `doctor`/media journeys) required in `#27`/`#28` before claiming release support |
| macOS | `darwin/amd64` (Intel) | Staged later: archive + reviewed Homebrew approach (`#29`) | **Not verified** (no Darwin Go implementations or CI runs yet) | Darwin syscall/FD/TTY/playback implementations, native lifecycle/media verification, formula-vs-cask decision, and unsigned disclosure (`#29`) |
| macOS | `darwin/arm64` (Apple Silicon) | Staged later: archive + reviewed Homebrew approach (`#29`) | **Not verified** (no Darwin Go implementations or CI runs yet) | Same native Darwin verification and signing disclosure (`#29`) |
| Windows & all other OS/architectures | `windows/*`, `linux/386`, 32-bit `arm`, `riscv64`, BSDs, etc. | **Explicitly unsupported and unscheduled** | Unsupported | Separate feasibility, security/permission model, and scoped issues required before any future consideration |

**Non-negotiable rule:** Go cross-compilation (`GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...`)
proves only that Go source compiles for `linux/arm64`. It **never** proves
runtime support for `linux/arm64` (which also depends on `wazero` JIT/runtime
behavior, `aarch64` `pillow==13.0.0` wheels, CPython 3.12, `ffmpeg`/`ffprobe`,
and `mpv`). Release notes must distinguish runtime-verified targets from
cross-built-only artifacts until native or emulated runtime tests pass.

## Tested-evidence minimum versions

Minimum versions below come from actual CI and local test runs, not guesses.
Older versions are untested and unsupported unless explicitly verified later.

| Component | Contract floor / minimum | Evidenced CI version | Evidenced local version | Scope and notes |
|---|---|---|---|---|
| Linux OS / libc | Ubuntu 24.04 LTS (`noble`), `glibc 2.39`, Linux kernel `>=3.15`, `/proc` mounted | Ubuntu 24.04 runner (`noble`) | `Ubuntu GLIBC 2.39-0ubuntu8.12` | Kernel `>=3.15` required for OFD locks (`ncruces/go-sqlite3`) and `renameat2` (`RENAME_NOREPLACE`); `/proc/self/fd` required by `store` and `remux.py` |
| Go toolchain | `Go 1.26.0+` (`go 1.26.0` in `app/go.mod` and `engine/go.mod`) | `go1.26.11` and `go1.27.4` (`linux/amd64`) | `go1.27.2` (`linux/amd64`) | Built with `GOWORK=off GOTOOLCHAIN=local`; CI tests run with `CGO_ENABLED=1 -race`, while `CGO_ENABLED=0` is supported for static/cross builds |
| Python runtime | `CPython 3.12` (`PinnedPythonVersion = "3.12"`) | `CPython 3.12.16` | `CPython 3.12.3` | Enforced strictly by `doctor.py` (`cpython` and `(3, 12)`); other Python versions/implementations return `runtime_incompatible` |
| Audible SDK & lockfile | `audible==0.13.1` (`PinnedAudibleVersion = "0.13.1"`) + 14 pinned transitive deps | `audible==0.13.1` (via `requirements.txt`) | `audible==0.13.1` + 14 transitive pins in [`requirements.lock`](../../engine/connectors/audible/internal/bridge/adapter/requirements.lock) | `pillow==13.0.0` is the sole native C-extension wheel (`cp312`); the other 14 packages are `py3-none-any`. Test-only `coverage==7.6.1` ([`tools/requirements.txt`](../../tools/requirements.txt)) and `pip` are excluded |
| SQLite | `>=3.38` (`STRICT` + built-in JSON, enforced in `supportedSQLite`) | Embedded `3.51.3` (`app`); `3.45.1` (schema oracle) | Embedded `3.51.3` (`app`); `3.45.1` (schema oracle) | `app` embeds SQLite `3.51.3` via `ncruces/go-sqlite3 v0.32.0` + `wazero v1.11.0` (CGO-free); `tests/storage/test_schema.py` uses stdlib `sqlite3` (`3.45.1`) as test oracle only |
| `ffmpeg` / `ffprobe` | `7:6.1.1` (`6.1.1`) | `ffmpeg 7:6.1.1-3ubuntu5` | Not installed locally (CI mandatory via `TALETTY_REQUIRE_MEDIA_TOOLS=1`) | Required for `download` copy-remux/chapter embedding/packet validation and full `doctor` readiness; `login` and `sync` do not require media tools |
| `mpv` | `0.37.0` | `mpv 0.37.0-1ubuntu4` | Not installed locally (CI mandatory via `TALETTY_REQUIRE_MEDIA_TOOLS=1`) | Required for `app` audio playback via `--input-ipc-client=fd://3` (`--ao=null` verified in CI) |

Note on transitive pinning: CI currently installs `requirements.txt`
(`audible==0.13.1`), which resolved `certifi==2026.11.20` and `soupsieve==4.1.3`
on 2026-11-22, whereas the tested local CPython 3.12 environment and
[`requirements.lock`](../../engine/connectors/audible/internal/bridge/adapter/requirements.lock)
pin `certifi==2026.10.23` and `soupsieve==4.1.2`. Artifact builds in `#27`,
`#28`, and `#29` must install from `requirements.lock` (`pip install --no-deps -r requirements.lock`)
so transitive dependencies cannot drift silently between builds.

## Runtime packaging decision per artifact

**Decision:** Taletty standardizes on an **isolated managed CPython 3.12 virtual
environment (`venv`)** pinned to [`requirements.lock`](../../engine/connectors/audible/internal/bridge/adapter/requirements.lock)
for source installs, Linux tar archives (`#27`), and native `.deb`/`.rpm`
packages (`#28`). An **optional self-contained bundled runtime staging** layout
is permitted for standalone tarballs (`#27`) or macOS (`#29`) only if built
reproducibly in CI from pinned CPython 3.12 and `requirements.lock` wheels with
full SBOM/license attribution and zero developer-machine state.

| Artifact | Python runtime strategy | Media tools (`ffmpeg`, `ffprobe`, `mpv`) | Notes and constraints |
|---|---|---|---|
| Source install (`go install` / source build) | User/installer creates an explicit CPython 3.12 venv and runs `python3.12 -m pip install --no-deps -r engine/connectors/audible/internal/bridge/adapter/requirements.lock` | System packages on `PATH` | Invoked via `--python`, `TALETTY_PYTHON`, or `PATH` (`python3.12` -> `python3`) |
| Linux tar archive (`#27`) | Ships `taletty`, `taletty-engine`, `requirements.lock`, and an explicit setup helper for an isolated managed CPython 3.12 venv (e.g., under `/opt/taletty/venv` or `~/.local/share/taletty/venv`, never inside `~/.config/taletty/`). Optional self-contained bundled runtime variant must be reproducibly staged in CI from `requirements.lock` | System packages on `PATH` (documented prerequisites; never silently bundled without full license/SBOM review) | Must still honor `--python` and `TALETTY_PYTHON` overrides |
| Linux `.deb` / `.rpm` (`#28`) | Declares distro Python 3.12 + venv dependencies and either ships a pre-built offline-staged isolated venv under `/usr/lib/taletty/venv` (built during package build from `requirements.lock` for the target arch) or pairs with an explicit unprivileged setup step | Declared package dependencies (`ffmpeg`, `mpv`) | **Forbidden:** privileged network `pip` calls in `postinst`/`%post` scripts or installing into system `dist-packages`/`site-packages` |
| macOS archive / Homebrew (`#29`, later) | Isolated Homebrew-managed CPython 3.12 venv from `requirements.lock` (or reproducibly staged bundle) | Homebrew `ffmpeg` and `mpv` dependencies | Explicit formula-vs-cask decision and unsigned/non-notarized disclosure required in `#29` |

### Strict contract and runtime matching

1. **Engine/bridge/adapter pin agreement:** `audible.ContractVersion` (`"v0.1.0"`),
   `audible.BridgeProtocolVersion` (`1`), `audible.PinnedPythonVersion` (`"3.12"`),
   `audible.PinnedAudibleVersion` (`"0.13.1"`), `doctor.py` (`PROTOCOL = 1`,
   `AUDIBLE_VERSION = "0.13.1"`), `requirements.txt`, and `requirements.lock`
   must agree.
2. **App-to-engine pinning:** Once `#13` integrates the engine into `app`,
   `app/go.mod` must pin a released `engine/vX.Y.Z` tag (never a committed
   `replace ../engine`).
3. **Isolated bridge execution:** Go invokes the embedded Python adapter with
   `python -I -B` (isolated mode, ignoring `PYTHONPATH`, cwd, and user
   site-packages, and writing no `.pyc` bytecode).
4. **Strict discovery precedence:** `audible.ResolvePython` / `DiscoverPython`
   resolves `--python` > `TALETTY_PYTHON` > `python3.12` on `PATH` > `python3`
   on `PATH`. Invalid explicit or `TALETTY_PYTHON` values fail closed with
   `ErrRuntimeMissing` without falling back.

### Explicit prohibitions

- **No silent system-Python mutation:** Never run `pip install` against system
  Python (`/usr/lib/python*`) or pass `--break-system-packages`.
- **No unpinned first-run `pip install`:** Neither `taletty` nor `taletty-engine`
  may invoke `pip` or download Python packages at runtime. Missing runtimes
  report `runtime_missing` cleanly via `doctor` and operation errors.
- **No legacy discovery or import:** Never scan, read, import, or modify
  `~/.config/audible-tui/` or honor `XDG_CONFIG_HOME` for the Taletty root.

## Offline operation vs network requirements

| Phase / operation | Network required? | Details |
|---|---|---|
| Initial installation / dependency setup | **Yes** (unless installing from an offline/pre-staged package or bundled archive) | Downloading release archives/packages, system `ffmpeg`/`mpv`/Python 3.12, and `requirements.lock` wheels into the isolated venv |
| `taletty-engine doctor` | **No** | Read-only local probe of Python, `audible`, `ffmpeg`, and `ffprobe`; creates no `~/.config/taletty/` state |
| Cached library browsing, search, filtering, sorting, favorites, bookmarks, settings | **No** | Served completely offline from `~/.config/taletty/taletty.sqlite3` |
| Local `.m4b` playback, chapters, seek, speed, volume, completion, sleep timer | **No** | Served completely offline from `~/.config/taletty/downloads/` via owned `mpv` process |
| Startup media and interrupted-job reconciliation | **No** | Local anchored filesystem and SQLite inspection only |
| Audible `login`, `sync` (`Library`), `download` | **Yes** | Explicit user-initiated provider operations (browser OAuth/MFA, catalog pagination, license request, signed HTTPS media transfer) |

Taletty performs **no** background telemetry, auto-update checks, or network
calls on startup.

## System vs bundled dependencies and executable names

### Executable names

- **`taletty`**: Primary user-facing TUI and application binary (`app/cmd/taletty`).
- **`taletty-engine`**: Headless engine CLI binary (`engine/cmd/taletty-engine`),
  exposing `doctor`, `login`, `sync`, and `download <ASIN>`.

### Dependency classification

- **Compiled into Go binaries:**
  - `app`: Bubble Tea v2 / Lip Gloss v2 UI stack, `github.com/ncruces/go-sqlite3 v0.32.0`
    + `github.com/tetratelabs/wazero v1.11.0` (embedded SQLite `3.51.3`),
    `github.com/susamn/taletty/engine v0.1.0`, and `golang.org/x/sys`.
  - `engine`: `golang.org/x/sys` and embedded Python adapter scripts (`//go:embed`).
- **System dependencies:** `mpv` (`>=0.37.0` evidenced), `ffmpeg` + `ffprobe`
  (`>=6.1.1` evidenced), `glibc` (`>=2.39` evidenced on Ubuntu 24.04), and
  CPython `3.12` (unless a self-contained tarball variant bundles CPython `3.12`).
- **Isolated Python runtime dependencies:** `audible==0.13.1` and its 14 pinned
  transitive dependencies in `requirements.lock` inside the managed CPython 3.12
  venv (never mixed with test-only `coverage==7.6.1`).

## Data preservation under `~/.config/taletty/` on upgrade and removal

All Taletty-owned user state lives exclusively under `~/.config/taletty/` (`0700`):

- `auth.json` (`0600`, engine-owned Audible credentials)
- `.auth.lock` (`0600`, advisory lock)
- `taletty.sqlite3` (`0600`, app-owned SQLite v1 database: catalog cache,
  listening checkpoints, completion, favorites, bookmarks, download job queue,
  local media inventory, view state, settings)
- `downloads/` (`0700`, downloaded `.m4b` audiobooks at `0600`)
- `config.toml`, `colors.toml`, `themes/` (theme and app configuration)

Lifecycle rules:

1. **Upgrades (`tar`, `.deb`, `.rpm`, source rebuild):** Must **never** overwrite,
   reset, or delete `~/.config/taletty/`. Future Taletty-owned SQLite schema
   upgrades (v2+) must follow [ADR 002](002-storage-contract.md) (consistent
   SQLite backup, transactional DDL/data/marker update, rollback on error).
2. **Package removal (`apt remove`, `dnf remove`, binary uninstall):** Removes
   only installed binaries, package metadata, and package-managed venv files.
   **Preserves `~/.config/taletty/` completely** (auth, SQLite DB, downloads,
   config, themes).
3. **Explicit purge separation:** Package purge (`dpkg --purge`) applies only to
   system-wide package configuration (if any) and must never sweep user
   `$HOME/.config/taletty/` directories across user accounts. Any user-data
   deletion requires an explicit, confirmed user action per [SECURITY.md](../../SECURITY.md)
   and ADR 002.
4. **Device registration semantics:** Installing, upgrading, or removing packages
   never registers or revokes an Audible device; explicit `login` owns device
   registration, and removing local `auth.json` does not revoke remote device
   registration.

## Checksums, SBOM, and build provenance requirements (`#27`, `#28`, `#29`)

Every downloadable release artifact produced in `#27`, `#28`, and `#29` must
satisfy:

1. **Checksums:** Publish a `SHA256SUMS` manifest containing SHA-256 digests for
   every archive, package, and source tarball, verified in release CI.
2. **SBOM:** Publish machine-readable SBOMs in **SPDX** (`.spdx.json`) and/or
   **CycloneDX** (`.cdx.json`) covering Go module dependencies (`app/go.mod`/`go.sum`,
   `engine/go.mod`/`go.sum`), pinned Python adapter dependencies (`requirements.lock`),
   and any bundled runtime components, while preserving third-party license
   notices (MIT for Taletty/`ncruces/go-sqlite3`/Bubble Tea, Apache-2.0 for
   `wazero`, and upstream licenses for Python packages and system tools).
3. **Build provenance:** Build reproducibly in CI from tagged commits
   (`engine/vX.Y.Z`, `app/vX.Y.Z`) with `GOWORK=off GOTOOLCHAIN=local`, record
   exact toolchain/OS/dependency versions, and attach SLSA / GitHub Artifact
   Attestations when release workflows publish artifacts.
4. **Artifact hygiene:** Never package developer virtualenvs, `~/.config/taletty/`,
   `auth.json`, OAuth redirect URLs, signed media URLs, vouchers, decryption
   keys, or protected `.m4b` files into any artifact or CI log.
5. **Honest release notes:** Explicitly distinguish runtime-verified targets
   (`linux/amd64`) from cross-built-only targets until native/emulated runtime
   verification passes, and disclose unsigned/non-notarized status on macOS
   (`#29`) unless signing is explicitly implemented.

## Verification and contract enforcement

- [`tests/tooling/test_packaging_contract.py`](../../tests/tooling/test_packaging_contract.py)
  verifies that `requirements.lock` pins exact versions with no unpinned, VCS,
  or local-path entries, includes `audible==0.13.1`, excludes test-only
  `coverage`/`pip`, matches `doctor.py` (`AUDIBLE_VERSION`, `PROTOCOL`, CPython
  `3.12`) and `engine/connectors/audible/version.go` (`ContractVersion`,
  `BridgeProtocolVersion`, `PinnedPythonVersion`, `PinnedAudibleVersion`), and
  keeps ADR 003 and [`docs/distribution.md`](../distribution.md) cross-references
  consistent.
- [`tools/check_inventory.py`](../../tools/check_inventory.py) validates that
  `requirements.lock` and `tests/tooling/test_packaging_contract.py` preserve
  registered module/adapter inventory without adding unmeasured production
  Python source.
- [`engine/connectors/audible/runtime_linux_test.go`](../../engine/connectors/audible/runtime_linux_test.go)
  and [`engine/release_linux_test.go`](../../engine/release_linux_test.go)
  verify runtime discovery precedence, contract pins, clean `go install`, and
  external consumer compilation.
- `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` verifies `engine` and
  `app` source cross-compilation only; native/emulated `linux/arm64` runtime
  verification remains gated on `#27`/`#28`.
