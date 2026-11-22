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

## Scope boundaries

- Library filtering/sorting (`#14`), playback wiring (`#15`), download manager
  UI (`#17`), and bookmark UI (`#19`) remain separate downstream issues.
