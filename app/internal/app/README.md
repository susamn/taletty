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

- **Download manager and safe audio deletion (`#17`)**:
  - Connects `*downloads.Coordinator` (`app/internal/downloads`) and
    `EngineDownloader` (`audible.OpenBorrowed` + `Client.Download` under
    `*store.RootLease`) into `Coordinator` and `RunSession` (`WatchDownloads`
    streaming live `ui.DownloadsMsg` and `ui.SnapshotMsg` updates with measured
    transfer speed when actively downloading).
  - Supports `d` enqueue, `D` Downloads view, `CancelActive`, `Retry`,
    `RemoveWaiting`, `MoveWaiting`, and anchored `DeleteAudio` (stopping/
    checkpointing active playback first if loaded, aborting deletion if `Stop`
    fails, unlinking via `openat`/`O_NOFOLLOW`/`unlinkat` under `downloads/`,
    and preserving `book_identity`, `listening_state`, `favorite`, `bookmark`,
    and `download_job` history).

- **Bookmark editor, manager, and jump targeting (`#19`)**:
  - Auto-wires `*bookmarks.Service` (`app/internal/bookmarks`) behind
    `BookmarkManager` in `NewCoordinator` when `Player != nil` and `Store`
    satisfies `bookmarks.BookmarkStore`.
  - `CaptureBookmark` (`b`) freezes current audio (playing or paused) with
    pre-roll clamping and chapter-boundary distinction; `SaveNote` persists
    new captures from frozen drafts (preventing duplicate creation on retry)
    and edits existing notes without recapturing position or reapplying pre-roll.
  - `OpenBookmarks` (`B` / sidebar entry) lists bookmarks offline; `DeleteBookmark`
    removes only the named bookmark and reloads authoritative state; `JumpBookmark`
    and `ConfirmSwitchBookmark` perform same-book paused seeks, missing-audio
    download prompts, and B3 `Stay` / `Switch and play` cross-book switches with
    stale-session invalidation.

- **Persistent favorite books and Favorites library view (`#42`)**:
  - Maps `store.DestinationFavorites` <-> `ui.ViewFavorites` and handles
    `ToggleFavorite` (`f`) offline via `store.SetFavorite` / `store.Favorites`
    with zero provider calls and zero playback changes, retaining authoritative
    prior state on save failure.

- **Completion state, smart rewind, and speed-adjusted remaining time (`#20`)**:
  - Wires `playback.DefaultSmartRewindPolicy()` in `OpenEngineWorkspace` (and
    optional `Options.SmartRewind`), `ReplayBook` confirmation on `PlayBook` for
    finished books, `MarkFinished`/`MarkUnfinished` (`SetCompletion`), live
    `Finished`/`InProgress` updates on natural EOF in `PollPlayback`, and
    `Player.ChapterEnd` projection for speed-adjusted book and chapter remaining
    time.

- **Global bookmark search and safe Markdown export (`#23`)**:
  - Wires `RootOwner` into `bookmarks.NewService` and handles
    `OpenGlobalBookmarks` (`All bookmarks (search & export)` sidebar entry) and
    `ExportBookmarks` (`e` inside Bookmarks Manager) with overwrite confirmation
    (`Keep existing file` / `Overwrite export`) and sanitized write-failure
    recovery.

- **Sleep timer by duration and chapter end (`#21`)**:
  - Implements in-memory `sleepTimerState` (`15/30/45/60 min`, `End of current
    chapter`, `Turn off`) driven by `Coordinator.clock` in `PollPlayback`:
    duration counts real playing elapsed time and freezes while paused,
    survives same-book seeks, and fires `player.Pause` once on expiry;
    chapter-end captures `[Start, End)` on arm, cancels on explicit seeks into
    another chapter, and fires once at chapter end; book switches, `Stop`, and
    `Close` cancel any active timer with no timer restored across restarts.

- **Semantic themes, Settings, and `SIGUSR1` reload (`#34`)**:
  - Seeds `~/.config/taletty/{colors.toml,config.toml,themes/}` via
    `theme.SeedDefaults` without overwriting user files or desktop-managed
    symlinks, and opens `theme.Manager` on workspace boot.
  - Handles `OpenSettings` (`S`), `ApplyTheme`, `ReloadTheme`, `SetGlyphMode`,
    `CycleSmartRewind`, and `CyclePreRoll`, retaining prior active palettes and
    settings on write/parse failure.
  - Dispatches `SIGUSR1` live reloads in `RunSession` (`theme.WatchSIGUSR1` +
    `HandleThemeSignal`) without interrupting playback, stealing focus, or
    discarding editor drafts, while keeping explicit presets selected during
    Omarchy `colors.toml` updates.

## Scope boundaries

- Local audiobook import (`#22`) and Linux MPRIS (`#30`) remain separate
  downstream issues.
