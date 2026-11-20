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

- `github.com/susamn/taletty/engine`: public `model` and `connectors/audible`
  packages; Audible-specific bridge/process/auth helpers private to the connector;
  `cmd/taletty-engine`. No terminal widgets, mpv or SQLite imports. Connectors are separate
  packages within the independently versioned engine module, not generic core logic.
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

#83 implements the library subset in `engine/model`: Book/Contributor reuse
BookID.Validate; required safe UTF-8 title/names, optional subtitle and contributor
lists, optional nonnegative native duration. Nil metadata is unknown; known empty
lists/zero duration survive standard JSON via omitzero/pointers. Public JSON names
native time.Duration **duration_ns**; the private DTO's **duration_ms** is distinct
and #85 must validate/bound it before conversion. Validate never normalizes or
rewrites metadata, and safe errors contain no input. No speculative capabilities,
artwork, history, snapshot-completion or new provider support is inferred.

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

- `Doctor(ctx, pythonExecutable)` (implemented for Linux under #63) returns dependency/readiness diagnostics without creating files or
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

The Linux [auth-session primitive](../../engine/connectors/audible/internal/auth/README.md)
implements root ownership and context-aware in-process serialization under #56.
A canceled Close wait retains the root lock; cancel/join the active operation and
retry. This primitive does not itself create/reap bridge processes or persist tokens;
those integrations remain the subsequent #5 extraction slices.

No engine lock guards app playback: downloaded audio keeps playing during provider
work. Cancellation while waiting starts no process. Cancellation during work closes
pipes, terminates the owned process group (including ffmpeg), waits/reaps with a
bounded escalation and removes owned partials. Do not remove another operation's files.

Persist refreshed tokens on normal success/provider failure as the baseline does.
On abrupt kill, token persistence is best effort, never guaranteed; do not claim a
finally block runs after SIGKILL. Atomic writes preserve the prior usable file
before publication. #67's private Python snapshot primitive uses a caller-owned
verified root directory FD and serialized session, no-follow single-link 0600
`auth.json`, random exclusive 0600 staging, file fsync, atomic replace and directory
fsync. It handles opaque bounded bytes only: SDK validation and anchored FD/TTY
handoff are #68/#73; #69 implements the private ordinary-operation SDK lifecycle.
Post-replace directory-fsync error explicitly
marks `published=True`; do not claim the old snapshot survived or undo publication.
Only a successfully created owned temp is cleaned, best effort; kill/I/O failure
may leave private temps, and retries never sweep another operation's files.

#69's embedded `sdk_session.authenticated(root_fd)` borrows the gate/root, loads
bounded validated US SDK auth, yields the real synchronous Client, closes it and
persists validated SDK serialization even after ordinary provider failure. Request
auth/refresh remain SDK-owned. Safe provider failure/code/retry survive successful
persistence and secondary close failure; unknown SDK errors never expose raw text.
Persistence failure takes precedence, with StoreError publication uncertainty kept
explicit. No automatic retries or abrupt-kill finally promise. Missing/malformed/
unsafe input starts no provider client and is not overwritten. `RunRoot` passes
only FD3 via the existing noninteractive process owner, without a TTY lease; child
marks it noninheritable for further exec. SDK stdio is isolated from control output.
The lifecycle now supports #85's typed Library API. Public CLI sync remains #86;
no fabricated provider probe actions or download integration are implied.

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

Go bounds request JSON to 4 MiB before appending its newline and closing stdin.
The process boundary validates the transport header; the typed connector/adapter
must validate action/root/book schema before provider access.

Request envelope: `protocol`, `operation_id`, `action`, `root` and optional `book_id`.
Implemented bootstrap action `doctor` omits root/book; `library` and `download`
remain the provider-operation targets. Protocol integer is exactly `1`. Go checks
runtime/protocol compatibility before accepting a result; no implicit downgrade.
Bridge rejects unknown action/version and invalid inputs without contacting Audible.

Frames: `protocol`, `operation_id`, consecutive `seq` starting at 1, `type`, plus typed
payload. Types: `stage`, `progress`, `result`, `error`. Exactly one terminal result/error
must precede clean exit. Go maps it into public events; it synthesizes a safe failure
for truncation, oversized frame, invalid JSON, mismatched ID, invalid order, child
failure or absent terminal result. Nonzero exit after a result is not trusted as success;
reconcile an already-published file before retry. Never blindly redownload a ready file.

The private [Go envelope reader](../../engine/connectors/audible/internal/bridge/README.md)
lands under #61 (parent #57). Required header names are case-sensitive; duplicate
top-level keys are rejected, including escaped aliases. The 4 MiB bound applies to
ScanLines content after LF/trailing-CR normalization; an EOF-terminated final line
is accepted. Its terminal is only a candidate: action-payload validation and a clean
process exit are still required before any public success. Linux `Run` (#62) owns
process lifetime and joins its reader/stdin/exit workers. It uses a private cancellable
frame channel, discards stderr, bounds post-exit drain and TERM grace, then SIGKILLs
residual group members before Cmd.Wait reaps the leader. Linux waitid(WNOWAIT)
keeps the PID reserved until group cleanup, avoiding PGID reuse. An external SIGCHLD
reaper must not reap its owned child. Only the direct child is waitable by Go;
non-child descendants are killed, with adoption/reaping handled by their parent/init.
This is cooperative process-group ownership, not containment of escaped/privileged
children or a bound on kernel-uninterruptible tasks. No files are deleted by Run;
Download must reconcile committed publication before offering a retry.
#63 registers real independently covered Python/runtime diagnostics. Go embeds the
source, invokes the explicit Python executable with `-I -B` (no cwd/PYTHONPATH,
bytecode or persisted script), and exposes typed fixed-status `Diagnostics` only.
Doctor constructs no client/authenticator, acquires no auth lock and receives no
root. CPython 3.12 and audible==0.13.1 are the evidenced development contract, not
#26's still-open artifact/minimum-platform policy. ffmpeg/ffprobe checks run
`-version` with bounded execution; no codec/media/release readiness is implied.

Doctor requests use an ASCII operation ID (1–64 alphanumeric/underscore/hyphen);
Go generates a fresh random `doctor-` ID. One seq=1 result has required exact-name
flat fields `python`, `audible`, `ffmpeg`, `ffprobe`, each a fixed `ready`,
`runtime_missing`, `runtime_incompatible` or `not_checked` state. Incompatible Python
skips all other probes; compatible Python checks all. Unknown optional fields are
ignored; aliases cannot override required names. Invalid requests emit a fixed safe
error before inspection, with fallback ID `invalid` when no valid ID was supplied.
Error envelopes exit cleanly; arbitrary exceptions/output are never public messages.
Other action-payload validation still precedes public provider-operation results.

Max frame: 4 MiB. Library pagination uses bounded `library_page` stage payloads (books)
followed by a terminal result containing count, not a single unbounded library array.
Accumulate/validate privately; do not publish a partial snapshot on failure. A single
oversized item is a safe protocol failure, not silently truncated metadata. Stage
payload variants are specified with fixtures when declarations land.

#79 implements a private Python candidate-page iterator, not an action/launcher.
Strict US ASIN identity, required list response, first-occurrence dedup, no-fresh-page
failure and 10,000x100 raw-item ceiling avoid silent truncation. Null optional fields
are absent; known empty contributors/zero duration survive. Normalized books carry
id/title, optional subtitle/name objects/duration_ms (bounded before Go conversion),
never customer/content URLs or download entitlement guesses. Terminal controls are
sanitized and compact ASCII `{books: [...]}` reserves512 bytes under 4MiB for fixed
v1 headers. Successful iteration alone is insufficient: SDK finally persistence,
then #80's typed validation/clean process completion are required before a snapshot.

#84's private noninteractive launcher reuses the existing trusted source-loader and
stdio helpers, preserves login defaults and lends only noninheritable FD3. Private
duplicated control output is separate from nulled SDK stdio. Library request/ID
validation precedes client construction, each <=4MiB stage is flushed sequentially,
and count is emitted only after SDK context exit/persistence. Fixed code-only
failure includes storage_failed/storage_uncertain; closed pipes cannot emit success.
Parent #85 still must embed/run it and validate count/typed payloads/EOF/exit before
snapshot publication. #86 exposes sync only after that acceptance; no new public
Go provider operation or executable library action is claimed here.

#85 implements public Client.Library with model.LibrarySnapshot/Failure/
OperationEvent and the real embedded root-only producer. IDs are ASCII1–64,
unique among this client's live Library calls; invalid/duplicate IDs emit no events
or child. Accepted calls report started, provisional count-only library_page stages
and exactly one completed/failed/canceled terminal (except caller reporter panic).
Public sequence includes wrapper lifecycle and differs from private seq. Events
never expose books/raw frames and have independent failure copies. Reporter runs
on caller thread, returns promptly/no reentry; cancellation cannot forcibly
interrupt caller code. Panic cancels/joins child users before releasing root/ID.
Strict exact-key schema, recursive duplicate-key and invalid UTF-8/lone-surrogate
checks precede known-field decoding; optional v1 fields remain ignored. Private
integer milliseconds are range-checked before native duration conversion. Limits:
100 books/page,10,000 stages,64MiB cumulative stage wire bytes,depth64; exceeding
bounds fails rather than truncates. Snapshot requires count agreement, SDK finally
persistence and clean control EOF/process exit. All error returns are zero snapshots.
Context cancellation wins until completion is validated; not retroactively from
completed callback. CLI sync remains #86; no app/cache/live-account/full-journey claim.

Library and download payload schemas are action-specific; fields not applicable to a
frame must not carry invented defaults. Unknown optional fields within protocol v1
are ignored; unknown frame types, invalid required fields or incompatible versions
fail safely. Go drains pipes and reaps even after decoding failure.

Login remains a separate interactive process/TTY path, not a mixed JSON/prompt stream.
The final redirect URL is hidden input owned by the login adapter, never an event.
App suspends/restores its terminal around handoff; Go constructs login outcome from
validated process exit and auth-file state, not parsing printed prose. UI journey
fixtures use fake login. Automatic localhost callback is not promised.

#73's Linux `audible.Open(ctx, root, python)` owns the session through `Close(ctx)`;
`Client.Login(ctx, *os.File)` supports only US, no implicit marketplace fallback or
runtime/root discovery. The caller suspends terminal users and converts its own
shutdown signals to context cancellation; engine installs no global handlers.
`RunTTY` reuses the noninteractive process owner, but hands off foreground TTY
ownership explicitly and restores foreground/termios after all owned users join,
including failed exec/forced kill. Only restoration's calling thread blocks SIGTTOU.
A process-local lease prevents overlapping handoffs even across different roots.
The child gets root FD3 and TTY FD4, never the lock; further exec inheritance is
disabled. A private control FD, duplicated before nulling SDK stdio, carries one
bounded bridge-v1 terminal, separate from prompts/input. The launcher consumes a
rootless login request, reuses embedded relative-import sources with `-I -B`, and
reloads/validates published auth. Result is `status="signed_in"`; error contains only
fixed `failure.code`. Go ignores unknown optional fields, validates required exact
keys/codes and clean EOF/exit, then checks a new same-UID single-link 0600 regular
auth inode. No credential bytes/redirect cross that boundary. `*LoginError` exposes
fixed code/message (including `storage_failed`/`storage_uncertain`), never raw text.
Retry is explicit. Context cancellation may race auth publication, never undo it;
this is not the separately specified completed-media publication policy. A terminal
restoration error stays visible alongside cancellation; callers must not blindly
resume UI. Hard parent death cannot guarantee restoration. CLI wiring remains #59;
no live-account or full onboarding/library journey evidence is claimed.

## Incremental download extraction (#6)

Accepted #5/#80 extraction is complete on merged main628a1b0; download is separate.
Native children: #91 bounded private chapter normalization/leaf ffmetadata, #92
independent license planning/signed-HTTP boundary, #93 anchored transfer/remux/
publication after91/92, #94 typed Go Download/API/CLI after93 (split further if needed).
#7 adds truthful transfer/preparation progress downstream, not a dependency cycle.

#91 implements only chapter preparation, reusing the prototype's positive-leaf,
stable-start-order and escaping algorithm. Private owned DTO retains hierarchy,
book-local structural index-path IDs, safe title, absolute start_ms, optional end_ms
and children; IDs are not provider revision identities. Missing start defaults0 as
before; missing length remains unknown, no positive leaf means retain embedded
chapters. No overlap clipping or parent-duration invention. Bounded32 levels/10,000
nodes/4096 UTF-8 bytes per title and separate4MiB-minus512 tree/metadata payloads;
invalid data fails safely, never truncates. Generated-media CI validates copy-remux
chapter times/titles and unchanged audio packet hashes; no license/transfer/publish
or public Download claim. Inherited key-in-process-args risk remains unchanged.

#92 adds only private `licensing.entitled(root_fd, asin)` and `signed_stream(plan)`.
The former borrows the existing SDK session/gate, preserves the prototype request
body and uses the pinned SDK voucher decryptor, never a licensing rewrite. Explicit
Granted/matching-ASIN responses yield an ephemeral non-serializable, redacted-repr
plan; denied/throttled/malformed responses fail with fixed codes and no retry.
Plan URL/key/IV references are cleared before SDK close/finally persistence, including
consumer failure. This is reference release, not Python immutable-memory zeroization.

Signed media uses a fresh unauthenticated HTTPS client, `trust_env=False`, no shared
cookies/headers and **no redirects** (all 3xx fail closed). There is no guessed CDN
allowlist; an actual redirect-requiring provider response is deliberately unsupported
until a reviewed policy exists. URL checks reject credentials, fragments, non-HTTPS,
non-443 ports, literal IPs, local single-label names and malformed/control characters.
Authenticated provider URL origin plus DNS/TLS are trusted; this is not an arbitrary
user-URL fetcher or DNS-rebinding/SSRF sandbox. Use only inside the dedicated child
with existing OS stdio isolation, never log requests/plans/tracebacks. No provider
URL, response, key or exception text crosses the public event/error boundary.
Response and fresh transport close on success, body/consumer failure or cancellation.
No transfer file, publication, launcher, public Download or CLI is added. #93/#94
remain downstream; inherited ffmpeg key-in-argv exposure remains unsolved.

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

#78 implements only doctor/login in `engine/cmd/taletty-engine`. Required explicit
`--python`, US-only `--marketplace`, doctor-only `--json`; invalid flags/arguments
never echo their values. Doctor creates no root. Login opens the owned controlling
TTY and uses only `$HOME/.config/taletty/`, requesting 0700 for new directories and
rejecting unsafe existing roots/locks without repair; XDG/legacy overrides are not
used. Entry owns Interrupt/TERM cancellation and broken-stdout handling, waits for
connector cleanup and closes the session/TTY before exit. JSON failures have fixed
code/message/retry; readiness results contain Ready plus typed Diagnostics. No
runnable TUI, sync/download placeholder, released artifact or live-account evidence.

#86 extends that same parser/session/entrypoint with sync (no TTY). Explicit Python,
US-only and optional JSON remain; Client.Library uses a fixed per-process cli-sync
ID and nil reporter, emitting no provisional output. Close with uncanceled context
precedes any success output; cleanup failure suppresses snapshot success while
retaining primary safe failure. JSON success is {snapshot: LibrarySnapshot}; empty
books is [], public duration is duration_ns. Plain output is count only. Safe typed
engine Failure code/message/retry is preserved; context/usage/output exit rules
unchanged. No media preflight, retries, app cache/SQLite or download command. Stdout
writes cannot guarantee atomic output bytes; callers replace caches only on exit0.
Actual embedded SDK/fake provider, refresh/store/restart, signal reaping and broken
stdout are tested, not live-account/full-journey/release evidence.

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
{"protocol":1,"operation_id":"op-library","seq":1,"type":"stage","stage":"library_page","books":[{"id":{"provider":"audible","namespace":"us","id":"B000000001"},"title":"Example Book","authors":[],"narrators":[]}]}
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
