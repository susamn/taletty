# Application session coordinator (`#131`, `#132` under `#13`)

`app/internal/app` implements Taletty's session coordinator (`Coordinator`),
cached boot (`OpenWorkspace` / `OpenEngineWorkspace` / `NewCoordinator`),
offline session restoration, onboarding/account orchestration, and the concrete
`EngineAccount` adapter over the pinned `github.com/susamn/taletty/engine v0.1.0`
module.

## Responsibilities

- **Cached boot & offline restoration**:
  - Opens the private `~/.config/taletty/` workspace via `profile.Open` (or
    consumes an existing `CatalogStore`), loads cached catalog metadata,
    favorites, local media inventory, download job states, settings, view state,
    and the restored book's `ListeningState`.
  - Restores `Focus`, `Selection` (by encoded `BookKey`), `GlyphMode`, and
    stopped/finished last-book player metadata **without autoplay** (`Session`
    remains empty until explicit playback).
  - Works completely offline without Python, `audible`, or network access
    (`EngineAccount` defers `audible.DiscoverPython` until `Doctor`, `Login`,
    or `Sync` is explicitly invoked).
- **Onboarding, engine integration, and account lifecycle**:
  - Detects first launch (`!hasSnapshot && len(books) == 0`) and opens the
    Account/onboarding overlay (`Login` vs `Continue offline`) without
    collecting passwords in UI widgets or scanning `~/.config/audible-tui/`.
  - `EngineAccount` borrows a session-lifetime `*store.RootLease` from
    `*store.Owner` and opens `audible.OpenBorrowed(lease.Context, lease.Root, python)`
    per `Login`/`Sync` operation, closing the borrowed client before releasing
    the lease.
  - `RunSession` cleanly exits and restores `terminal.Run` around interactive
    `Login` TTY handoffs before resuming the TUI with updated `AccountState`.
  - Serializes `Doctor`, `Login`, and `Sync` operations, rejecting concurrent
    operations with sanitized busy errors.
  - Rejects stale or canceled `Sync` replies by `OperationID` and publishes
    complete snapshots atomically via `store.ReplaceCatalog` only on clean
    success, preserving existing cached books and selection on failure.
  - Persists `ViewState` via `SaveViewState` and cancels/joins active workers
    before closing `AccountClient` and `Workspace` in `Close(ctx)`.

- **Playback, chapter inspection, and miniplayer actions (`#15`)**:
  - Connects `*playback.Controller` and `*playback.MPVProcess` (`app/internal/playback`)
    behind `PlaybackController`, `NewRootMediaChecker` (anchored `openat`/
    `O_NOFOLLOW`/`0700`/`0600`/`Nlink==1` verification), and `FFprobeChapterReader`.
  - `PlayBook` loads verified local `.m4b` audio at saved position/speed;
    missing or unsafe audio opens a download confirmation without touching
    current audio.
  - `LoadChapters` inspects downloaded books without replacing current audio;
    `ActivateChapter` seeks directly (preserving pause) on the same loaded book
    and opens the B3 `Stay` / `Switch and play` confirmation on a different
    loaded book (checkpointing the prior book on switch and rejecting stale
    confirmations).
  - `TogglePlayback` (`Space`), `SeekRelative` (`,`/`.`, `±15s`), `StepSpeed`
    (`[`/`]`, `±0.1x`), `StepVolume` (`-`/`=`, `±5%` persisted to `Settings`),
    and `RunSession` periodic `PollPlayback` keep the miniplayer and chapter
    timeline synchronized, and `Close` saves the final checkpoint and reaps `mpv`.

- **Library views, stable sorting, and contributor filtering (`#14`)**:
  - Projects `Subtitle`, `Narrator`, `Duration`/`HasDuration`, `RecentAtMS`,
    `InProgress`, `Finished`, and `CatalogOrder` from `store.Catalog` and
    `store.ListeningStates` into `ui.Book`.
  - Restores and persists all `store.ViewState` fields (`Destination`,
    `SortOrder`, `FilterText`, `Focus`, `Selected`) across restarts via
    `RestoreSessionMsg` and `SaveFullViewState`.

## Scope boundaries

- Download manager UI (`#17`), bookmark UI (`#19`), completion/smart-rewind
  (`#20`), sleep timer (`#21`), and favorites UI (`#42`) remain separate
  downstream issues.
