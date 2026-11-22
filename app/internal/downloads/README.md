# Persistent sequential download-job coordinator (`#16`)

`app/internal/downloads` implements Taletty's persistent sequential download-job
coordinator (`Coordinator`) over `app/internal/store` without importing the
unpublished `engine` module or terminal UI widgets (`#17`).

## Consumer-owned boundaries

- **`JobStore`**: satisfied directly by `*store.Store` (`DownloadJobs`,
  `PutDownloadJob`, `ReorderDownloadQueue`, `ReconcileActiveJobs`, `LocalMedia`,
  `LocalMediaList`, `PutLocalMedia`).
- **`MediaChecker`**: verifies whether persisted `store.LocalMedia` entries
  currently point to private `0700` directories and `0600` single-link regular
  files under `downloads/`. The default implementation uses `store.RootOwner`
  and `store.WithRoot` with `O_NOFOLLOW`/`O_CLOEXEC` descriptor anchoring.
- **`Downloader`**: executes one download operation synchronously
  (`Download(ctx, opID, key, report) -> (DownloadOutcome, error)`), emitting
  ordered `Stage` (`licensing`, `downloading`, `preparing`, `publishing`) and
  `Progress` (`Bytes`, optional `Total`) events.

## Lifecycle and invariants

- **Startup reconciliation**: `New` inspects `LocalMediaList`, verifies private
  files under `downloads/`, calls `store.ReconcileActiveJobs` (marking
  interrupted active jobs `JobReady` when confirmed published or
  `JobFailed`/`FailureInterrupted` otherwise), and restores waiting jobs in
  `queue_order` order.
- **Deduplication & sequential execution**: `Enqueue` validates `store.BookKey`,
  rejects duplicate live jobs (`ErrAlreadyQueued`) and verified ready media
  (`ErrAlreadyDownloaded`), assigns the next `queue_order`, persists
  `JobWaiting`, and wakes the single sequential worker.
- **Progress & stage validation**: rejects stale events whose `OperationID`
  does not match the active job, out-of-order sequences, backwards stage
  transitions, progress outside `downloading`, decreasing `BytesReceived`, and
  inconsistent `BytesTotal`.
- **Cancellation & commit-point race**: `CancelActive` cancels `opCtx` and marks
  `Canceling` in `Snapshot` while keeping the active slot occupied until the
  worker joins and `Downloader` returns. If `Downloader` returns committed ready
  media, `JobReady` and `LocalMedia` win (never unlinking committed media on
  late cancel); otherwise the job transitions to `JobCanceled`.
- **Waiting queue & explicit retry**: `RemoveWaiting` cancels only waiting jobs
  without touching active jobs, local media, history, bookmarks, or favorites.
  `ReorderWaiting` and `MoveWaiting` use `store.ReorderDownloadQueue` atomically
  and preserve authoritative order on persistence failure. `Retry` creates a new
  queued attempt only for terminal `JobFailed`/`JobCanceled` jobs; failures such
  as `entitlement_denied` and `throttled` are never retried automatically.
- **Snapshots, watch, and shutdown**: `Snapshot` returns deep-copied immutable
  state with monotonic `Revision`; `Watch(ctx)` delivers updates without dropping
  stage or terminal transitions; `Close(ctx)` cancels active work, waits for
  worker join and final persistence, and is idempotent.

## Verification & scope limits

[`coordinator_linux_test.go`](coordinator_linux_test.go) exercises real
`*store.Store` on temporary SQLite and deterministic channel/barrier fake
`Downloader` implementations (no real sleeps). Terminal UI views/actions
(`#17`, J04–J06) and `engine` integration (`#13`) remain separate downstream
issues.
