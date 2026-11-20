# Typed Linux library API (#85)

```go
snapshot, err := client.Library(ctx, "refresh-1", func(event model.OperationEvent) {
    // Ordered caller-thread notifications; return promptly, never reenter Client.
    // library_page counts are provisional. Do not replace a cache from a stage.
})
if err == nil {
    // Only here is snapshot authoritative; caller owns persistence and Books.
}
```

Use the same explicit runtime/US-only `Open`/`Close` and private root as login;
login/library share one cancel-aware gate and session lock. Doctor and local
playback remain independent. No TTY, root discovery, media tool requirement,
SQLite/cache write, generic registry, retry or entitlement/download guarantee.
A valid snapshot may be empty (non-nil `Books`); every error returns the zero
snapshot, even after earlier valid pages. Public native duration is `duration_ns`;
private integer `duration_ms` is range-checked before conversion.

Operation IDs: ASCII `[A-Za-z0-9_-]{1,64}`, unique among live Library calls on this
Client, including queued work and terminal notification. Rejected invalid/duplicate
IDs start no child and emit no events. Invalid input is not echoed. IDs become
reusable only after return/panic cleanup. Do not copy a Client after first use.

Accepted calls report Started then zero or more Stage(`library_page`) notifications
then exactly one Completed/Failed/Canceled terminal, except caller reporter panic.
Public Sequence starts1; it is distinct from private bridge seq. Count on Stage is
validated **candidate count**, not committed/cache progress. Failure terminal count
is0; no partial books go to events. Return value is authoritative; reconcile event
and return using operation ID, do not apply completion twice. Callback failures are
independent copies and cannot mutate the returned error or snapshot metadata.

Reporter runs synchronously on the caller goroutine, never an unowned worker. It
must return promptly/no reentry; use context for cancellation. The process owner
can cancel/kill/reap while a caller callback is blocked, but Library cannot return
through arbitrary blocked user code. Reporter panic propagates after root/process
users join, gate and live ID are released. Join your operation callers before
shutdown; Close waits authenticated work, not arbitrary caller-owned callback code.
Cancellation wins until validated completion, not retroactively when requested
from Completed. Hard kills do not guarantee SDK finally persistence.

Exact case-sensitive payloads, all nested duplicate keys, malformed UTF-8/lone
UTF-16 surrogates, identity/optional metadata, integer time/count and repeated books
are checked. Unknown optional v1 fields are ignored after ambiguity/Unicode checks.
Bounds: <=4MiB per frame, <=100 books/page, <=10,000 stages, <=64MiB cumulative
stage wire bytes (not a64MiB heap guarantee), JSON depth<=64. Exceeding any limit
fails safely rather than publishing truncated results. No remotely transactional
snapshot or raw HTTP response memory bound is claimed.

Completion requires count agreement AND clean control EOF/process exit after SDK
close/atomic auth persistence. Use `var failure *model.Failure` and
`errors.As(err, &failure)` to inspect fixed safe Code/Message/Retry/OperationID,
never raw provider/bridge/OS text. Context and
closed-session identities remain available via errors.Is. `storage_uncertain`
means published but durability unconfirmed; do not infer rollback/revoke or retry
automatically. Successful auth refresh may survive an ordinary library failure.

Tests use real embedded Python/pinned SDK with synthetic auth/HTTP MockTransport,
real barriers, process ownership/cancellation and filesystem faults; no live account
or protected media. The [headless sync CLI](../../cmd/taletty-engine/README.md)
now consumes this API under #86; neither implements the TUI, full J01/J14 or a release. Runtime artifacts/locks/minimum platforms remain #26.
