# Serialized Linux mpv playback adapter (`#11`)

`app/internal/playback` implements Taletty's serialized playback authority (`Controller`)
and owned Linux `mpv` JSON-IPC adapter (`MPVProcess`), keeping presentation code
decoupled from media process management and checkpoint persistence.

## Responsibilities

- **`Controller` (`controller_linux.go`)**:
  - Owns one serialized operation mutex (`opMu`) and snapshot state (`stateMu`)
    so UI, future CLI controls and desktop media integrations share a single
    authoritative state machine.
  - Increments `Snapshot.Session` on every `Load` (including reloading the same
    book) and rejects stale transport calls (`session != 0 && session != current`)
    with `ErrStaleSession`.
  - Restores and persists per-book position (`PositionNS`) and speed (`SpeedMilli`,
    clamped `[500000, 3000000]`) through `CheckpointStore` (satisfied directly by
    `*store.Store`), preserving existing `Completion`.
  - Persists final checkpoints on book switch, pause, paused/playing exact seek,
    speed change, EOF, `Stop` and `Close`.
  - Marks `CompletionFinished` on natural EOF in `Poll` (never on deliberate
    `Stop`, book switch, or seek), exposes `SetCompletion` (updating metadata
    without interrupting active audio) and `Replay` (resetting position to `0`
    and `CompletionUnfinished` while preserving saved speed), and enforces
    configurable `SmartRewindPolicy` (`DefaultSmartRewindPolicy()` = 10s pause
    threshold / 5s rewind, clamped at `0`, cleared on deliberate seeks/jumps).
  - Clamps `VolumePercent` to `[0, 100]`.
  - Reaps broken `ErrIPC` backends immediately and exposes sanitized failure
    messages (`ErrRuntimeMissing`, `ErrIPC`, cancellation, timeout).

- **`MPVProcess` (`mpv_linux.go`)**:
  - Implements `Backend` over an owned `mpv` child launched in its own process
    group (`Setpgid: true`) with `--no-config`, `--no-terminal`, `--no-video`,
    `--audio-display=no`, `--load-scripts=no`, `--ytdl=no`, `--idle=yes`,
    `--keep-open=yes`, and `--input-ipc-client=fd://3` over a `CLOEXEC` Unix
    `socketpair` (no temp socket directory, polling sleep, or Go-thread-sensitive
    `Pdeathsig`). Closing the parent socketpair endpoint on crash or `SIGKILL`
    immediately closes `fd://3` so `mpv` exits with zero orphan audio.
  - Loads validated clean absolute `.m4b` paths via the named JSON-IPC `loadfile`
    command (`{"name":"loadfile","url":...,"flags":"replace","options":{"start":...,"pause":...}}`),
    waits for `file-loaded` and initial `time-pos` readiness, and fails fast on
    `end-file` load errors.
  - Uses an allowlisted clean environment (`cleanMPVEnv`), `/dev/null` stdio,
    bounded JSON-IPC line scanning (`256 KiB` cap), `request_id` correlation,
    and per-call context deadlines for `get_property`, `set_property`, and
    `seek <seconds> absolute+exact` (preserving pause state during paused seeks).
  - Eliminates the `Stop` PID-reuse race by observing child exit with
    `waitid(P_PID, pid, WEXITED|WNOWAIT)` before `unix.Kill(-pid, unix.SIGKILL)`
    and sole `cmd.Wait()` reaping (matching `engine/connectors/audible/internal/bridge/process_linux.go`).
  - Enforces speed bounds `[500000, 3000000]` (`ErrInvalidInput`), clamps volume
    `[0, 100]`, and maps missing `mpv` binary to `ErrRuntimeMissing` and
    process/socket/protocol errors to sanitized `ErrIPC`.

## Attribution

Socketpair `fd://3` IPC launch, named `loadfile` command, and bounded JSON-IPC
correlation patterns are adapted from `podmutt` (MIT License, Copyright (c)
podmutt contributors).

## Verification & scope boundaries

- [`playback_linux_test.go`](playback_linux_test.go) exercises fake JSON-IPC
  children over `fd://3`, `net.Pipe` IPC edge cases, and `Controller` + real
  `*store.Store` on temporary SQLite (saved state restore, paused exact seek,
  speed/volume bounds, book switch, EOF, stale sessions, store/backend faults,
  queued cancellation, process-group kill/reap, and killed-parent `fd://3` EOF).
- [`mpv_integration_linux_test.go`](mpv_integration_linux_test.go) generates
  chaptered `.m4b` fixtures via `ffmpeg` and runs real `mpv --ao=null` tests
  (skipped locally only when `mpv`/`ffmpeg` are absent; mandatory fail in CI
  when `TALETTY_REQUIRE_MEDIA_TOOLS=1`, repeated 3x under `-race`).
- Sleep timer policies (`#21`) and desktop media adapters (`#30`) remain
  separate downstream tasks.
