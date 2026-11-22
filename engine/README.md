# Taletty Engine (`github.com/susamn/taletty/engine`)

Independently versioned Go engine module and headless CLI (`taletty-engine`) for
provider-neutral audiobook models and the Linux Audible connector. It contains no
Bubble Tea, Lip Gloss, SQLite, or mpv dependencies.

## Contract and runtime pins

Exposed in [`connectors/audible`](connectors/audible/README.md):

| Constant | Value | Meaning |
|---|---|---|
| `ContractVersion` | `"v0.1.0"` | Engine module/API contract version |
| `BridgeProtocolVersion` | `1` | Required JSON-lines bridge protocol version |
| `PinnedPythonVersion` | `"3.12"` | Evidenced CPython minor version |
| `PinnedAudibleVersion` | `"0.13.1"` | Pinned `audible` Python package version |

## Prerequisites

- **OS**: Linux (`amd64` runtime-verified in CI on Ubuntu 24.04 / `glibc 2.39`; `arm64` source cross-build verified only — native/emulated runtime verification required in `#27`/`#28`). See [ADR 003](../docs/adr/003-runtime-and-platform-matrix.md).
- **Go**: `1.26.0+` (CI verified on Go `1.26.11` and `1.27.4`).
- **Python**: CPython `3.12` (CI `3.12.16`, local `3.12.3`) with [`connectors/audible/internal/bridge/adapter/requirements.lock`](connectors/audible/internal/bridge/adapter/requirements.lock) (reproducible transitive pins for [`requirements.txt`](connectors/audible/internal/bridge/adapter/requirements.txt), `audible==0.13.1`) installed in an explicit isolated virtual environment.
- **Media tools**: `ffmpeg` (`7:6.1.1` evidenced in CI) and `ffprobe` on `PATH` for `download` preparation/validation and full `doctor` readiness (`login` and `sync` require only Python + `audible`).

Taletty never runs `pip`, never mutates system Python, and never scans `~/.config/audible-tui` or legacy paths.

## Source installation

1. Prepare an explicit isolated CPython 3.12 virtual environment using the pinned lockfile:

```sh
python3.12 -m venv ~/.local/share/taletty-venv
~/.local/share/taletty-venv/bin/python -m pip install --no-deps -r engine/connectors/audible/internal/bridge/adapter/requirements.lock
```

2. Install `taletty-engine` from source (or from the module-scoped tag once published):

```sh
cd engine
GOWORK=off go install ./cmd/taletty-engine
```

Once `engine/v0.1.0` is tagged on `main`:

```sh
GOWORK=off go install github.com/susamn/taletty/engine/cmd/taletty-engine@engine/v0.1.0
```

3. Verify runtime readiness (read-only; creates no `~/.config/taletty` state):

```sh
TALETTY_PYTHON=~/.local/share/taletty-venv/bin/python taletty-engine doctor --json
```

## Runtime discovery precedence

`audible.ResolvePython` / `audible.DiscoverPython` and `taletty-engine` resolve the Python executable in strict order:

1. Explicit override (`explicit` argument or `--python` flag)
2. `TALETTY_PYTHON` environment variable
3. `python3.12` on `PATH`
4. `python3` on `PATH`

Empty, whitespace-only, or NUL-containing explicit overrides (and invalid `TALETTY_PYTHON` values) are rejected cleanly without falling back. Missing runtimes return `audible.ErrRuntimeMissing` (CLI `runtime_missing`, exit `1`).

## Go library API

- [`model`](model/README.md): `BookID`, `Book`, `Contributor`, `Chapter`, `Media`, `LibrarySnapshot`, `DownloadResult`, `OperationEvent`, `Failure`.
- [`connectors/audible`](connectors/audible/README.md):
  - `Doctor(ctx, python)` — read-only dependency probe.
  - `ResolvePython` / `DiscoverPython` — pinned runtime discovery.
  - `Open(ctx, root, python)` — acquires exclusive `.auth.lock` on an existing `0700` same-UID root.
  - `OpenBorrowed(ctx, root *os.File, python)` — duplicates a caller-owned `0700` same-UID directory descriptor via `openat(".", O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC)` and serializes `Login`/`Library`/`Download` without acquiring or unlocking `.auth.lock`. Callers (such as `app/internal/store.Owner` / `RootLease`) must retain exclusive cooperative root ownership until `Client.Close` returns.
  - `Client.Login`, [`Client.Library`](connectors/audible/library.md), [`Client.Download`](connectors/audible/download.md), `Client.Close`.

See [the CLI guide](cmd/taletty-engine/README.md), [ADR 001](../docs/adr/001-engine-api-and-bridge-contract.md), [ADR 003](../docs/adr/003-runtime-and-platform-matrix.md), [distribution plan](../docs/distribution.md), [testing strategy](../docs/testing.md), and [security policy](../SECURITY.md).

## Post-merge maintainer release tagging

Do **not** push tags before PR review and merge to `main`. After the PR merges:

```sh
git checkout main
git pull --ff-only origin main
git tag -a engine/v0.1.0 -m "Release github.com/susamn/taletty/engine v0.1.0"
git push origin engine/v0.1.0
```
