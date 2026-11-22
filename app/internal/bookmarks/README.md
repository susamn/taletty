# Persistent timestamped bookmarks and targeting policy (`#18`)

`app/internal/bookmarks` implements Taletty's bookmark capture, persistence,
offline listing, note editing, deletion, and explicit same-book / cross-book
jump targeting without terminal UI dependencies (`#19`).

## Consumer-owned boundaries

- **`BookmarkStore`**: satisfied directly by `*store.Store` (`Bookmarks`,
  `PutBookmark`, `DeleteBookmark`, `LocalMedia`).
- **`PlaybackController`**: satisfied directly by `*playback.Controller`
  (`Snapshot`, `Load`, `SeekAbsolute`).
- **`RootOwner` / `MediaChecker`**: optional anchored verification of private
  `0600` regular `.m4b` files under `0700` `downloads/` directories (`*store.Owner`
  satisfies `RootOwner` directly via `Lease`). Cross-book jumps fail closed with
  `ErrMediaMissing` when no verifier is configured or when local media is absent
  or unsafe.

## Targeting and lifecycle invariants

- **Capture (`CaptureDraft` / `CaptureDefaultDraft`)**: captures only the loaded
  current audio session (`StatePlaying` or `StatePaused` with non-nil `Book` and
  nonzero `Session`), rejecting `StateIdle`, `StateLoading`, `StateStopped`, and
  `StateFailed` with `ErrNoLoadedAudio` and never falling back to a highlighted
  library book. Freezes `BookKey`, `Session`, `Title`, `CapturedPosition`,
  `PreRoll >= 0` (`DefaultPreRoll = 2s`), `JumpPosition = max(0, captured - preRoll)`,
  and distinct `CapturedChapter` / `JumpChapter` context when `chapterAt` crosses
  a chapter boundary.
- **Save (`SaveNew`)**: validates nonblank safe multiline notes (rejecting
  whitespace-only text, control characters other than `\n`/`\t`, Unicode `Cf`,
  and invalid UTF-8 with `ErrInvalidInput`), generates a unique stable bookmark
  ID, and persists `store.Bookmark` with the frozen `JumpPosition` and jump
  chapter context without re-reading playback on delayed saves. Store errors
  leave the caller's `Draft` intact for retry.
- **Edit (`EditNote`)**: updates only `Note` and `UpdatedAtMS` on the fixed
  bookmark identity for `key`, never recapturing playback position or reapplying
  pre-roll, and rejecting missing or wrong-book IDs with `ErrBookmarkNotFound`.
- **Delete (`Delete`)**: removes only the named bookmark for `key`, preserving
  audio files, `LocalMedia`, `ListeningState`, `Favorite`, and other books'
  bookmarks.
- **List (`List`)**: returns bookmarks offline in ascending `PositionNS` order,
  tie-broken by bookmark `ID`.
- **Jump (`Jump`)**:
  - *Same loaded book*: verifies active playback, matching `BookKey`, and
    `expectedSession` (`ErrStaleSession` on mismatch), then calls `SeekAbsolute`
    to the stored position while preserving playing/paused state with no second
    pre-roll or smart rewind.
  - *Different book / unloaded player*: verifies `LocalMedia` and private
    regular file existence under `downloads/` (`ErrMediaMissing` if absent or
    unsafe, never seeking unrelated current audio), re-verifies that the
    playback session and prior book did not change behind confirmation
    (`ErrStaleSession` on mismatch), then calls `Load(ctx, track, true)`
    (preserving the prior book's checkpoint via `Controller`) and `SeekAbsolute`
    to the exact bookmark position.

- **Global listing & safe Markdown export (`export_linux.go`, `#23`)**:
  - `ListAll` returns all bookmarks across books offline ordered by `BookKey`,
    `PositionNS`, and `ID`.
  - `ExportMarkdown` writes escaped, ANSI-free Markdown atomically under
    `~/.config/taletty/exports/` (`0700` dir, `0600` regular `.md` file,
    `O_NOFOLLOW`), validating safe basenames, rejecting symlinks/hardlinks/unsafe
    modes (`ErrUnsafeExportTarget`), returning `ErrExportExists` when the target
    exists without `Overwrite: true`, and never exporting auth, signed URLs, or
    local paths.

## Verification & scope boundaries

[`service_linux_test.go`](service_linux_test.go) and
[`export_linux_test.go`](export_linux_test.go) exercise `Service` against real
`*store.Store` on temporary SQLite, real `*playback.Controller` (with a fake
`playback.Backend`), real `*store.Owner` private roots, and a deterministic fake
clock: playing/paused capture, idle/stopped/failed rejection, highlighted-vs-loaded
separation, pre-roll zero clamp and chapter-boundary distinction, delayed save
after playback advances/switches, note validation, duplicate positions, edit
without recapture/re-rewind, single-bookmark delete preserving audio/history/favorites,
offline ordering, same-book paused jump and stale session rejection, cross-book
jump (including root rename and unknown catalog duration), missing/unsafe/symlink/
hardlink/mode-mismatch local audio, mid-check stale confirmation, store/checkpoint
faults, `-race` concurrency, global `ListAll`, filename/path confinement,
Markdown/HTML escaping, overwrite confirmation, and unsafe export target refusal.

Limits: Linux-only service (`service_linux.go`, `export_linux.go`). See
[ADR 002](../../../docs/adr/002-storage-contract.md),
[the bookmarks design](../../../docs/design/bookmarks.md), and
[the test strategy](../../../docs/testing.md).
