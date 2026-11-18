# ADR 001: Independent engine API and versioned Audible bridge

Status: **accepted via merged PR #35**. Addresses #1; the contract is implemented
incrementally. Model identity and independent-module foundations land under #37.

## Context and reuse

The prototype embeds Python and serializes authenticated operations to protect token
refresh writes. Its Go UI directly manages persistence, downloads and mpv. Preserve
working Audible library pagination, license requests, remux/chapter logic and safe
error handling, but extract provider work behind a typed boundary. Do not rewrite
licensing, copy runtime data or carry the UI's mutable model into the engine.

## Module and dependency direction

- `github.com/susamn/taletty/engine`: public `model` and `audible` packages; internal
  bridge/process/auth helpers; `cmd/taletty-engine`. No terminal, mpv or SQLite imports.
- `github.com/susamn/taletty/app`: `cmd/taletty` plus internal app/UI/store/playback
  packages. Import engine types/API, never its internal helpers or Python modules.
- App services depend on narrow consumer-owned interfaces; concrete engine satisfies
  them. Inject fakes in journeys. Do not invent a universal provider registry now.
- Provider-neutral models belong to `engine/model`. Local import can construct these
  models without logging into Audible. Engine does not own listening history/notes.
- All runtime data is fresh under `~/.config/taletty/`; audio under `downloads/`.
  Engine owns auth writes, app owns local database/views/jobs. No old-app import.

## Public model contract

Final Go declarations land with tests in #4/#5; these are normative semantics.

| Type | Required semantics |
|---|---|
| BookID | Comparable structured key: provider, namespace, provider ID. Audible namespace is marketplace (initially `us`); ID is validated ASIN. Do not key by title, queue row or file path. |
| Book | ID, title, optional subtitle, author/narrator lists, optional duration/acquisition timestamp/description/artwork reference and capabilities. Unknown is absent, not zero or invented metadata. |
| Chapter | Stable book-local ID, title, absolute nonnegative start, optional end and child chapters. Preserve provider hierarchy; document leaf mapping for remux. |
| Capabilities | Library retrieval, download, chapter retrieval and interactive login support. Capability is not proof of entitlement or a download license. |
| Media | Book ID, final local path, media format and available chapters. Ready only after validated atomic publication. Never expose voucher/key/signed URL. |
| Failure | Safe code, safe message, retry disposition and optional operation/book ID. No provider response body or arbitrary exception string. |
| OperationEvent | Operation ID, sequence, stage/type, optional bytes/total and safe failure or result. No account secrets. |

Keep provider metadata separate from local playback position/speed/completion/notes.
Use seconds in finite Go `time.Duration` values internally; bridge times are integer
milliseconds, byte counts nonnegative integers. Validate bounds before conversion.
Book duration and byte total may be absent; absence is not completion or zero length.
Account tokens, customer IDs and content URLs are never public model fields. Artwork
references must be safe for display/fetch policy, not arbitrary signed response URLs.

## Go API shape and ownership

The `audible` client is configured with private Taletty root, runtime/process seams
and supported marketplace. Reject unsupported marketplaces explicitly. Proposed
operations (names may be made idiomatic when declarations land):

- `Doctor(ctx)` returns dependency/readiness diagnostics without creating files or
  making provider calls. Separate read-only dependency inspection from session init.
- `Login(ctx, terminal)` is an explicit interactive handoff. Terminal is a narrow
  injected I/O capability, not a Bubble Tea dependency. Never accept/store an Amazon
  password in app widgets. Login starts only on user request.
- `Library(ctx, operationID, report)` returns a complete validated library snapshot;
  pagination failure never replaces an existing app cache with partial results.
- `Download(ctx, operationID, bookID, report)` returns final `Media`; app handles the
  persistent waiting queue. No listening queue or automatic retry in the engine.

Operations are synchronous/context-aware; app invokes them in asynchronous commands.
The optional reporter is a synchronous, ordered notification callback: return quickly,
never reenter engine operations, marshal to the app's message queue, never mutate UI
state from a worker. Nil reporter is valid. Reporter is not a cancellation mechanism;
use context. A reporter panic is caller misuse, not a sanitized provider error.

The Go wrapper generates started/terminal events and returns the authoritative result
or error exactly once. Callers must not apply terminal state twice because both an
event and return value describe it; identify updates by operation ID. IDs must be
unique among live operations. Reject invalid/duplicate live IDs before subprocess work.

## Serialization and lifetime

One authenticated operation at a time per data root across login/library/download.
Use a cancellation-aware in-process gate plus root advisory lock for cross-process
ownership; no blocking mutex acquisition that ignores canceled contexts. Session
owns the root lock. Standalone CLI commands acquire it; UI session and its engine
operations share ownership rather than recursively acquiring the lock. Busy external
clients fail with a safe busy error instead of modifying auth concurrently.

No engine lock guards app playback: downloaded audio keeps playing during provider
work. Cancellation while waiting starts no process. Cancellation during work closes
pipes, terminates the owned process group (including ffmpeg), waits/reaps with a
bounded escalation and removes owned partials. Do not remove another operation's files.

Persist refreshed tokens on normal success/provider failure as the baseline does.
On abrupt kill, token persistence is best effort, never guaranteed; do not claim a
finally block runs after SIGKILL. Atomic writes preserve the prior usable file.
Cancellation racing with final media publication has a defined commit point: if the
atomic publication occurred, return/report completed Media; otherwise cancel and
clean partials. A late cancel does not delete an already-published complete book.

## Python bridge v1

One child per noninteractive operation; one request object on stdin, then EOF. Output
is UTF-8 JSON lines on stdout, flushed after each frame. No banners/debug prints.
Stderr contains allowlisted sanitized diagnostics only; Go does not display arbitrary
stderr/tracebacks/process arguments. Never put auth/keys/signed URLs on request payloads
or command lines. The private root may be passed; the bridge reads engine-owned auth.
The inherited ffmpeg-key argument limitation is separately documented and retained
until a safer tested interface exists; never log its invocation.

Request envelope: `protocol`, `operation_id`, `action`, `root` and optional `book_id`.
Actions initially `library` and `download`. Protocol integer is exactly `1`. Go checks
runtime/protocol compatibility before accepting a result; no implicit downgrade.
Bridge rejects unknown action/version and invalid inputs without contacting Audible.

Frames: `protocol`, `operation_id`, consecutive `seq` starting at 1, `type`, plus typed
payload. Types: `stage`, `progress`, `result`, `error`. Exactly one terminal result/error
must precede clean exit. Go maps it into public events; it synthesizes a safe failure
for truncation, oversized frame, invalid JSON, mismatched ID, invalid order, child
failure or absent terminal result. Nonzero exit after a result is not trusted as success;
reconcile an already-published file before retry. Never blindly redownload a ready file.

Max frame: 4 MiB. Library pagination uses bounded `library_page` stage payloads (books)
followed by a terminal result containing count, not a single unbounded library array.
Accumulate/validate privately; do not publish a partial snapshot on failure. A single
oversized item is a safe protocol failure, not silently truncated metadata. Stage
payload variants are specified with fixtures when declarations land.

Library and download payload schemas are action-specific; fields not applicable to a
frame must not carry invented defaults. Unknown optional fields within protocol v1
are ignored; unknown frame types, invalid required fields or incompatible versions
fail safely. Go drains pipes and reaps even after decoding failure.

Login remains a separate interactive process/TTY path, not a mixed JSON/prompt stream.
The final redirect URL is hidden input owned by the login adapter, never an event.
App suspends/restores its terminal around handoff; Go constructs login outcome from
validated process exit and auth-file state, not parsing printed prose. UI journey
fixtures use fake login. Automatic localhost callback is not promised.

## Progress and failures

Stages: `licensing`, `downloading`, `preparing`, `publishing`. Download byte samples
are monotonic within that transfer; unknown total is omitted. Do not derive preparation
percentage from bytes. If ffmpeg offers no verified progress, preparation is indeterminate.
Emit stage before its samples. Sequence numbers remain monotonic even if UI coalesces
samples; gaps are permitted in public presentation, never in raw bridge validation.
Terminal outcomes/stage boundaries are never silently dropped. Bound transport queues;
backpressure must select on cancellation. A stalled consumer cannot prevent shutdown.

Safe failure codes: `not_authenticated`, `busy`, `invalid_input`, `runtime_missing`,
`runtime_incompatible`, `network`, `throttled`, `entitlement_denied`, `unsupported`,
`disk_full`, `permission_denied`, `preparation_failed`, `protocol_error`, `canceled`,
`internal`. Unknown provider exceptions become `internal` with safe generic wording.
Retry dispositions: `user_retry`, `sign_in`, `check_entitlement`, `wait`, `none`.
No automatic retries for license denial/throttling. Final redirect URLs, customer IDs,
license bodies, voucher keys and signed URLs must not appear in errors or JSON.

## CLI and independent releases

`taletty-engine` exposes `doctor`, `login`, `sync`, `download <ASIN>`;
machine-readable output via explicit `--json` for noninteractive
commands. No fabricated progress while awaiting auth gate. CLI/Go API use the same engine.
CLI must not write the app's SQLite database; a library result goes to caller/stdout,
which the app may persist. Login is not JSON mode. Exit codes: 0 success, 1 operation
failure, 2 invalid CLI usage, 130 user interrupt. Typed JSON failure code is authoritative
for machine recovery, not a growing taxonomy of OS exit codes.

Tag modules `engine/v0.x.y` and `app/v0.x.y`. Bridge protocol version is independent
of Go semantic version; changing required framing requires a protocol bump and
compatibility plan. Pin Python dependencies with the engine release. App updates its
engine dependency through explicit review; changing engine alone must not rebuild UI
contracts accidentally. Root remains a docs repository, not a third production module.

Bootstrap #4 builds/tests modules independently; app initially contains only app-owned
foundations and has no engine dependency until the first engine release #8. For local
cross-module work use a disposable ignored `go.work`, never committed `replace ../engine`.
At #13 integration, pin a released engine tag and run `GOWORK=off` builds/tests outside
the developer workspace. CI checks both development wiring and pinned-release mode
once integration exists; it cannot require an unpublished engine tag in the bootstrap.

## Synthetic protocol examples (not credentials or real provider data)

```json
{"protocol":1,"operation_id":"op-demo","action":"download","root":"/tmp/taletty-fixture","book_id":{"provider":"audible","namespace":"us","id":"B000000001"}}
```

Successful download; unknown total omitted until known:

```jsonl
{"protocol":1,"operation_id":"op-demo","seq":1,"type":"stage","stage":"licensing"}
{"protocol":1,"operation_id":"op-demo","seq":2,"type":"stage","stage":"downloading"}
{"protocol":1,"operation_id":"op-demo","seq":3,"type":"progress","bytes":1024}
{"protocol":1,"operation_id":"op-demo","seq":4,"type":"progress","bytes":2048,"total":2048}
{"protocol":1,"operation_id":"op-demo","seq":5,"type":"stage","stage":"preparing"}
{"protocol":1,"operation_id":"op-demo","seq":6,"type":"stage","stage":"publishing"}
{"protocol":1,"operation_id":"op-demo","seq":7,"type":"result","media":{"book_id":{"provider":"audible","namespace":"us","id":"B000000001"},"path":"/tmp/taletty-fixture/downloads/B000000001.m4b","format":"m4b","chapters":[]}}
```

Alternative terminal error after licensing, not appended to the success transcript:

```jsonl
{"protocol":1,"operation_id":"op-demo","seq":2,"type":"error","failure":{"code":"entitlement_denied","message":"Audible did not grant a download license.","retry":"check_entitlement"}}
```

Library success (a separate operation/transcript):

```jsonl
{"protocol":1,"operation_id":"op-library","seq":1,"type":"stage","stage":"library_page","books":[{"id":{"provider":"audible","namespace":"us","id":"B000000001"},"title":"Example Book","authors":[],"narrators":[],"capabilities":{"download":true}}]}
{"protocol":1,"operation_id":"op-library","seq":2,"type":"result","count":1}
```

Compatibility failures: a v2 request must be rejected before provider access; a v2
response to a v1 request is `protocol_error`; an unknown optional v1 result field
is ignored. A failed second library page discards the first page from the proposed
snapshot, preserving the caller's last successful cache.

Cancellation: after progress seq 3, cancel ctx → terminate/reap group → cleanup
unpublished partials → Go emits one canceled terminal at its public sequence and
returns context cancellation. Killed Python need not emit an error frame. If result
publication already committed, completion wins as described above.

## Acceptance and implementation tests

#4/#5/#7 must turn these semantics into tests, not rely on this prose as execution:

- Identity collisions across providers/marketplaces, optional fields and invalid times.
- Pagination duplicate/repeated/failed pages with no partial library replacement.
- Auth serialization/canceled wait, external owner busy, token-refresh write failures.
- Framing split lines, escaped newlines, invalid UTF-8/JSON, oversized frames, wrong
  versions/IDs/sequences, duplicate terminals, missing terminal and nonzero exit.
- Unknown total, monotonic bytes, progress backpressure, terminal ordering and stale
  app updates rejected by operation identity.
- Missing Python/audible/ffmpeg, provider denial, disk/permission/preparation errors,
  process-group cancellation, killed child, committed-file/cancel race and cleanup.
- Login TTY handoff/restore without capturing secrets; doctor read-only.
- Pinned release consumer and generated-media chapter/remux evidence; no live accounts.

Coverage remains >=95% per production module/adapter; this ADR itself is docs-only N/A.
Layout/theme code and storage schema remain their own issues, not engine concerns.
