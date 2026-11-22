# Typed Linux Download API (#112)

```go
result, err := client.Download(ctx, "download-1", bookID, report) // nil report is valid
// Inspect BOTH even when err != nil. A ready result may accompany an auth,
// durability, or child/protocol failure; never blindly redownload from err alone.
if result.Media != nil {
    // Validated complete local media, not a partial. Persist/use as app policy allows.
}
```

Uses the existing explicit Python runtime, US-only Client session, root lock and
cancel-aware gate shared with Login/Library. No queue, app storage, automatic
retry, or competing process owner. CLI `download` passes a `nil` reporter so
CLI stdout stays clean.
Invalid requests and duplicate live IDs emit no events/start no child. IDs are
ASCII `[A-Za-z0-9_-]{1,64}`, shared across **Library and Download** on this Client,
including queued work and callbacks until return/panic cleanup.

Accepted calls emit `Started`, ordered `Stage`/`Progress` notifications (#7),
and exactly one `Completed`/`Failed`/`Canceled` with an independently owned
`OperationEvent.Download`, except a caller reporter panic. Verified ready reuse
skips all stages and progress (`Started` -> `Completed`). Fresh downloads emit
`Stage` (`licensing`, `downloading`, `preparing`, `publishing`) in strict order
without duplicates or backwards transitions, and monotonic `Progress` (`Bytes`,
optional `Total` omitted when unknown or `< Bytes`) during `downloading` only.
Preparation emits `Stage("preparing")` with no fake byte progress. Every
nonterminal frame is strictly validated (exact keys, duplicate keys, UTF-8/lone
surrogates, stage enum/order, monotonic nonnegative `bytes`, consistent optional
`total >= bytes`); `reconcile` continues rejecting any nonterminal frame.
Event sequence starts at 1 and is separate from private bridge sequence.
Callbacks receive independent pointer copies for `Bytes`, `Total`, `Failure`,
and `Download`. Bounded channel backpressure selects on cancellation; all owned
child/ffmpeg process-group users join on cancel, panic, or error before
reconciliation.
Return is authoritative: correlate event and return by operation ID, don't apply
it twice. Completion does not imply authentication was attempted or persisted.

Reporter runs synchronously on the caller goroutine; return promptly and never
reenter Client. Nil is allowed. Panic propagates unchanged after owned users join.
There is no callback goroutine and no way to interrupt arbitrary caller code.
Started runs before acquiring the root; terminal runs after all process/recovery
users release it. Close waits root work (including bounded reconciliation), not
caller-owned notification code. Join operation callers before shutdown. A canceled
Close retains the session lock and can be retried after cancel/join of operations.

## Independent facts, not a single success bit

- `State`: ready / absent / invalid / unsafe / unknown. Error before acquiring the
  root leaves unknown; unknown is never absence. Preflight rejection returns zero.
- `Media`: only for validated ready output, fixed `m4b` format and a safe absolute
  final path derived from the borrowed root inode, not provider text or the stale
  Open pathname. Rename is supported; deleted/unsafe-to-display root fails closed
  without erasing the known commit. All paths remain cooperative same-UID paths,
  not lifetime capabilities against later deletion/rename/malicious mutation.
- `Published`: this attempt's exact false / true / null commit fact. Reconciliation
  cannot reconstruct it. A recovered ready file has null, never guessed true.
- `Reused`: only a clean producer's verified prior/concurrent reuse. Reconciliation
  does not guess whether this operation created the file.
- `MediaDurability`: confirmed only for clean known fresh publication with no media
  failure; storage_uncertain remains uncertain. Existing/recovered ready is unknown,
  never fsynced by recovery. Other media errors conservatively leave unknown.
- `AuthPersistence`: confirmed only when a clean fresh publication has no media or
  auth failure (the SDK context has then exited successfully); failed/uncertain
  when explicitly reported. Otherwise unknown. Nil auth code means **no reported
  auth fault**, not proof of auth execution/persistence; reuse can avoid auth or
  occur concurrently after licensing, so it cannot establish either fact.
- `MediaFailure` and `AuthFailure`: independent safe code/message/retry records;
  auth persistence failure takes precedence in the operation error, not in media
  readiness. Child transport failure remains an operation error even if recovery
  finds ready media. No provider exception, signed URL, key or response escapes.

`Chapter` retains the owned provider hierarchy/order and book-local structural IDs
only on fresh validated publication. IDs are stable for an unchanged tree, not
provider revisions. Start/end are absolute native durations, JSON `start_ns` /
`end_ns`; omitted end is unknown, zero pointer is known zero. Private integer
milliseconds are bounded before shared conversion. No overlap clipping, parent
end invention, or reconstruction from flattened ffprobe chapters. Nil hierarchy
is unknown on reuse/restart/recovery; known empty is distinct from unknown.
Bounds: 32 levels, 10,000 nodes, unique safe IDs <=256 bytes, safe nonempty titles
<=4096 UTF-8 bytes, nonnegative times/end>=start. Max bridge frame remains4MiB.
Recursive duplicate/Unicode validation also covers ignored optional fields; the
download JSON-depth limit accounts for both object/array per chapter level.

## Cancellation and recovery

After a canceled/killed/nonzero/truncated/malformed child or a failure without ready
media, the original RunRoot users are **joined first**. The same WithRoot loan/gate
is retained for separate provider-free `reconcile`, using a fresh independent
8-second whole-child context (production probe is bounded to5 seconds), then joined
before release. No recursive gate acquisition, auth/licensing, mkdir/fsync, sweep,
partial/final deletion, or retry. Recovery failure/timeout is unknown, not absent.
Ordinary refused downloads also reconcile conservatively without another license.

A verified ready artifact wins media cancellation, including after actual commit
and before auth save. Return/terminal are completed with unknown lost commit/auth/
durability facts after canceled-child recovery. A killed/nonzero/malformed child
instead retains a safe protocol error alongside ready media; auth/publication
uncertainty is never hidden. Context identity is retained on non-ready cancellation,
but ctx.Err is **not** blindly substituted after completion/recovery. Cancellation
from a completed callback is too late to undo media. No hard-kill cleanup/finally,
kill-time auth durability, or final-file deletion promise is made.

Tests use actual embedded Python/pinned SDK with synthetic auth and fake HTTP,
real root/store/syscalls and barriers. Deterministic copy/probe fixtures exercise
local failure/recovery without tools; they are not actual-media evidence. The
separate generated clear-media public API test uses production ffmpeg/ffprobe and
runs automatically where installed; mandatory CI fails if tools are missing.
No live account, protected media, UI J04/J05/J06, or working-release claim. The
inherited local key-in-ffmpeg-argv exposure remains documented and unsolved.
