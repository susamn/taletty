# Application session coordinator (`#131` under `#13`)

`app/internal/app` implements Taletty's session coordinator (`Coordinator`),
cached boot (`OpenWorkspace` / `NewCoordinator`), offline session restoration,
and onboarding/account orchestration over narrow consumer-owned interfaces
(`CatalogStore`, `AccountClient`, `TerminalHandoff`) without importing the
unpublished `engine` module.

## Responsibilities

- **Cached boot & offline restoration**:
  - Opens the private `~/.config/taletty/` workspace via `profile.Open` (or
    consumes an existing `CatalogStore`), loads cached catalog metadata,
    favorites, local media inventory, download job states, settings, view state,
    and the restored book's `ListeningState`.
  - Restores `Focus`, `Selection` (by encoded `BookKey`), `GlyphMode`, and
    stopped/finished last-book player metadata **without autoplay** (`Session`
    remains empty until explicit playback).
  - Works completely offline without Python, `audible`, or network access.
- **Onboarding & account lifecycle**:
  - Detects first launch (`!hasSnapshot && len(books) == 0`) and opens the
    Account/onboarding overlay (`Login` vs `Continue offline`) without
    collecting passwords in UI widgets or scanning `~/.config/audible-tui/`.
  - Serializes `Doctor`, `Login` (via injected `TerminalHandoff`), and `Sync`
    operations, rejecting concurrent operations with sanitized busy errors.
  - Rejects stale or canceled `Sync` replies by `OperationID` and publishes
    complete snapshots atomically via `store.ReplaceCatalog` only on clean
    success, preserving existing cached books and selection on failure.
  - Persists `ViewState` via `SaveViewState` and cancels/joins active workers
    before closing `AccountClient` and `Workspace` in `Close(ctx)`.

## Scope boundaries

- Concrete `engine/v0.1.0` pinning (`app/go.mod`), `audible.OpenBorrowed`
  adapter wiring, and `app/cmd/taletty` entrypoint remain `#132` (blocked by
  `#131` and maintainer tagging `engine/v0.1.0`).
- Library filtering/sorting (`#14`), playback wiring (`#15`), download manager
  UI (`#17`), and bookmark UI (`#19`) remain separate downstream issues.
