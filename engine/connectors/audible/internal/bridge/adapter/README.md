# Private Python adapter

## Read-only bootstrap (#63)

`doctor.py` implements bridge-v1 action `doctor`, not login/library/download yet.
One bounded request object with protocol 1, ASCII operation ID (1–64 letters,
digits, `_`/`-`) and action `doctor` produces one seq=1 terminal, flushed as JSONL.
Duplicate keys, non-finite JSON, invalid UTF-8/version/action/ID and oversize input
are rejected before inspection. Unknown optional fields are ignored.

A result has flat `python`, `audible`, `ffmpeg`, `ffprobe` status fields:
`ready`, `runtime_missing`, `runtime_incompatible`, or `not_checked`. CPython 3.12
is the only interpreter evidenced here; incompatible Python skips other probes.
The SDK check pins audible==0.13.1 and imports it without constructing a client or
authenticator. Tool checks locate and execute `-version` with a two-second timeout;
this does not prove codecs/remux, minimum OS/libc/tool versions or release support.

The Go owner runs the embedded source with `-I -B`: no cwd/PYTHONPATH import,
bytecode or persisted script. There is no root input, auth lock/read, provider call,
legacy/runtime discovery or installer. Import banners and tool output go to the
null device; only fixed statuses/messages cross the boundary. Error envelopes exit
cleanly; canceled/closed pipes need not produce a terminal. No account fixtures.

Install dependencies only in an explicit development/test venv. Distribution/runtime
locking, platform support and installation remain #26; first-run system pip is not
implemented. CI independently measures every discovered adapter `.py` (including
future unimported files), checks report/source equality, and requires its named job
in Quality gate. Go contract tests do not substitute for Python source coverage.

For a source-level probe with an explicitly prepared venv (CLI wiring is #59):

```sh
printf '%s\n' '{"protocol":1,"operation_id":"doctor-demo","action":"doctor"}' | \
  /path/to/venv/bin/python -I -B engine/connectors/audible/internal/bridge/adapter/doctor.py
```

The Linux Go entry point is `audible.Doctor(ctx, "/path/to/venv/bin/python")`;
`Diagnostics.Ready()` means these limited dependency probes passed, not signed-in
or playback/download readiness.

## Private atomic authentication snapshots (#67)

`auth_store.load(root_fd)` / `save(root_fd, payload)` operate on bounded (1 byte
through 4 MiB) **opaque immutable bytes**, not public events or a signed-in claim.
SDK serialization/credential validation and FD/TTY ownership are implemented by
#68/#73; #69 adds the ordinary-operation SDK lifecycle below. The store imports no SDK and calls no provider. Doctor
does not import/call the store. No login command is shipped by this primitive.

Caller supplies a borrowed effective-UID-owned **0700 directory descriptor** and
holds #56's authenticated-operation gate throughout. Keep that FD open/unreused
until return; store does not close it or acquire another lock. Directory-relative
operations remain anchored after rename and never discover/create roots. Actual
Go→child directory-FD handoff is owned by #73, not this store. Cooperative same-UID
ownership matches the session threat model; hostile same-UID inode replacement
is not contained.

- `auth.json` must be a same-UID, single-link regular **0600** leaf. Reads use
  no-follow/nonblocking/close-on-exec, including FIFO rejection without hanging.
  Unsafe roots/files are rejected, never chmod-repaired or followed.
- Writes exclusively create a random `.auth-<32 hex>.tmp` at 0600, apply 0600 to
  that **new** file even with restrictive umask, handle short writes, fsync and
  close it, recheck the destination, atomically replace and fsync the directory.
- Pre-commit errors retain the prior file. `StoreError.published=True` means the
  replacement happened but directory durability could not be confirmed; do not
  undo it or assume old auth survived. Error text is fixed and contains no paths.
- Cleanup targets only a successfully created owned temporary. I/O errors or
  SIGKILL may leave a private temporary; no finally-after-kill guarantee or sweep
  of other operations' files. A retry uses a new temp and leaves such files alone.
- `MissingAuth` means absent file; empty/oversize/unsafe files fail safely.
  Tokens remain plaintext/private on disk; use disk encryption where needed.

Tests cover actual filesystem permissions/links/anchoring, partial I/O, publication
and durability failures/recovery, FD cleanup, a bounded real SIGKILL-before-replace
writer and pinned SDK synthetic serialization round-trip with provider I/O forbidden.
This is persistence-boundary evidence, **not** complete J01/J14/login coverage.

## Private US browser login (#72 under #68)

`login.login(root_fd, tty_fd)` is a private callable, **not a CLI entry point**.
It checks root/leaf safety without reading secrets (`auth_store.check`), explicit
TTY ownership and shared CPython/SDK readiness before calling the pinned SDK's
`Authenticator.from_login_external(locale="us", login_url_callback=...)`.
No login/licensing protocol is reimplemented. Only US Amazon HTTPS `/ap/signin`
and `/ap/maplanding` URLs are accepted; redirects require exactly one nonempty
SDK authorization-code query field. Invalid/control/oversize URLs fail safely.

The initial sign-in URL is shown on the owned terminal as browser fallback; it
contains a public challenge, **not** the final redirect. Browser-launch failure
keeps that fallback. Wait for the hidden-input prompt before pasting the final
URL. Input uses the explicit TTY with ECHO/ECHONL off, never an echoed getpass/stdin
fallback; 2048-byte limit stays below Linux canonical-line truncation. Empty/EOF/
interrupt cancels. Exact termios settings are restored on Python exception paths.
The redirect stays in memory, never stdout/stderr/events/argv/auth snapshot.

SDK Python chatter is redirected to the null device. #73's dedicated-child
`interactive.py` launcher also nulls OS-level SDK stdin/stdout/stderr: its private
control descriptor is duplicated before isolation, never mixed with SDK output or
TTY prompts. It loads only embedded sources into a relative-import package, marks
root/TTY noninheritable for further execs, validates the bounded login request with
shared Doctor helpers, and reloads/validates published auth before a seq=1 result
with `status="signed_in"`. Error terminals carry only `failure.code`; unknown
optional fields are ignored by the Go validator, never exposed. Go retains both
FDs inside the session gate, owns foreground handoff/group cleanup, independently
restores terminal state and validates fresh private publication. This callable
neither acquires locks nor claims finally runs after SIGKILL.

Before publication, require US locale and SDK signing/bearer capability on both
returned auth and actual SDK-restored serialized snapshot. Cookie-only/empty auth
is not success; capability is not entitlement/live account proof. Accepted bytes
use #67's private atomic store. Returns no auth data; `LoginError.code` and message
are fixed, including cancellation, missing/incompatible runtime, storage failure
versus published-but-not-durable uncertainty. #69 implements ordinary-operation
refresh-on-failure below; login itself does not use the authenticated-operation context.

Tests use fake external provider/browser callbacks and real pinned SDK restore
with unmistakably synthetic, never-issued placeholders (no account/network).
Real controlling PTY children verify no echo and exact settings after success,
EOF, SIGINT and oversize rejection. These are adapter/TTY boundaries, not complete
app onboarding or live-provider sign-in evidence. CLI wiring remains #59.

## Authenticated SDK lifecycle (#69)

`with sdk_session.authenticated(root_fd) as client:` borrows a verified private
snapshot and yields the pinned synchronous SDK Client. Host holds the existing
Go session gate and root FD throughout, runs a dedicated child with isolated OS
stdio and emits protocol via a separate control FD. No terminal lease is needed;
`RunRoot` lends only FD3 through the same full process owner. The child disables
further root-FD exec inheritance. This module is included in the embedded package;
there is **no public provider action** yet. #79 adds private pagination below;
Go library/framing/sync integration waits for accepted #78/#79 under #80.

Check runtime and US signing/bearer capability before constructing the client.
The SDK still owns request authentication/automatic bearer refresh. Close the
client, then validate/serialize with shared `login.snapshot` and atomically persist
on ordinary success, SDK/provider failure, constructor/close failure and handled
keyboard interrupt. An existing safe `ProviderError` retains identity/code/retry;
SDK exceptions map to fixed not-authenticated/network/throttled/internal messages.
Unknown response text/URLs never become errors. No automatic operation retries.
A close failure cannot mask the primary safe provider failure.

Persistence errors override provider errors: `StoreError.published=False` preserves
the previous snapshot; `True` means new credentials were installed but directory
durability is unconfirmed. Invalid serialization never replaces old bytes. Missing,
unsafe or malformed input starts no client and is never overwritten. Borrowers do
not discover/create roots or retain/close the directory FD. Abrupt process death
may interrupt finally or publication: **no refreshed-token durability guarantee**.
Tests use real SDK bearer flow with a fake refresh response and MockTransport,
actual filesystem faults/recovery, and Go→Python blocked-provider/queued-cancel/
active-kill barriers. No real account or public UI-journey claim.

## Provisional US library pages (#79)

`library.pages(client)` is a private iterator, consumed **inside** the accepted
`sdk_session.authenticated(root_fd)` context. #84 frames it in a private dedicated
child below; embedding/public Go integration remains #85 after maintainer acceptance. Reuses prototype's SDK
`library` request/groups/sort, page size100 and 10,000-page ceiling. Unsupported
marketplace or malformed response/identity/metadata fails with fixed safe codes.
Within/across-page ASIN dedup retains first occurrence; any nonempty no-fresh page
or full-page ceiling exhaustion fails, never silently truncates. Empty first page
is a legitimate empty library; missing/null `items` is not.

Each yielded list is **provisional**. Failed later pages invalidate all candidates.
Only successful exhaustion, SDK close/finally persistence AND future clean transport
completion permit a snapshot; iteration/closing early is not completion. No cache,
app database, automatic retry, Go events or CLI `sync` in this slice.

Books include structured `id` (audible/us/strict ASCII 10-character ASIN), nonempty
title (`Untitled` fallback), and optional subtitle/contributor name objects/integer
`duration_ms`. Unknown/null optional metadata is omitted; known empty lists and
zero duration survive. Finite nonnegative minutes round to milliseconds bounded
by floor(Go int64 nanoseconds /1e6). Complete CSI/OSC are stripped; remaining
controls/format/surrogate code points flatten to spaces before presentation. No
customer IDs, content/signed URLs or invented download/entitlement capabilities.
Compact ASCII JSON `{books: [...]}` is capped at 4MiB minus512 bytes reserved for
#80's fixed v1 header (ASCII ID<=64, uint64 seq/stage). No metadata truncation.
SDK response decoding still belongs to the pinned SDK; this bounds normalized
page payloads, not raw HTTP body memory or remote snapshot consistency.

Tests cover sparse/null/unicode/control/duplicate/invalid/nonfinite/huge/boundary
metadata, request shape, repeated/failed/endless pages, exact frame reservation and
real SDK MockTransport→refresh→later throttling/protocol/keyboard failure→persist,
write-failure preservation and refreshed-snapshot recovery. No live account/HTTP;
consumer cache replacement is demonstrated only after the complete context exits.

## Private streaming library launcher (#84)

`noninteractive.main(sources, load_sources, null_stdio)` uses #73's trusted loader
and OS stdio helper capabilities, not a second bootstrap implementation. The loader
accepts an explicit engine-trusted module order; login's existing default stays
unchanged. Library loads doctor/auth_store/login/sdk_session/library into the same
fileless relative-import package. Not a source/runtime discovery path or CLI.

Only verified root FD3 is borrowed, never a TTY/lock. Duplicate stdout to private
noninheritable control first, disable further root inheritance, null SDK stdout/
stderr, validate the bounded rootless `action=library` request, then null stdin.
Emit compact <=4MiB ASCII JSONL, flush every frame, consecutive seq from1 and exact
operation ID. Stages carry `stage=library_page, books=[...]`; one result has count
only **after** iterator exhaustion and SDK close/atomic persistence. Ordinary
provider/validation/cancel/store failure emits fixed `failure.code` only, never a
result; `storage_uncertain` preserves published-but-not-confirmed semantics. A
closed control pipe cannot manufacture success or print provider tracebacks.

Parent must retain gate/root, validate action payload/count, drain/join the process
owner and check clean EOF/exit before accepting a snapshot. #85 now embeds/runs
this callable through public Client.Library and the same RunRoot/WithRoot owner. It
acquires no lock, writes no cache, retries nothing and guarantees no finally after
hard kill. Tests execute actual -I -B fileless subprocesses and pinned SDK with
synthetic refresh/HTTP MockTransport, null SDK chatter/stdin, further exec FD
noninheritance, malformed requests, later-page failures, close/replace/dir-fsync
faults and handled KeyboardInterrupt. Unit faults measure flushes/frame bounds
and closed-control/loader errors; no live provider/account/protected-media evidence.

## Private chapter preparation (#91, download parent #6)

`chapters.prepare(info)` returns an owned normalized tree and optional ffmetadata.
Reuses the same maintainer's `audible-tui/bridge.py` positive-leaf/stable-start-sort/
escaping algorithm, plus the accepted library text sanitizer and ProviderError;
no third-party source/license is added. The tree retains source hierarchy/order,
structural book-local index-path IDs (stable only for an unchanged tree), safe
titles, absolute start_ms, optional end_ms and children. Unknown fields never pass
through. Legacy missing start defaults to0, missing length stays unknown; null or
empty title becomes Chapter. No positive leaves means None: later remux retains
embedded chapters, never emits a phantom chapter. Parent durations do not become
leaves or clip absolute child offsets; overlaps/ties are preserved, not repaired.

Strict integer nonnegative/bounded times (no bool/fractions/overflow),32 levels,
10,000 nodes,4096 UTF-8 bytes/title and separately <=4MiB minus512 for normalized
ASCII tree JSON and UTF-8 ffmetadata. Cycles/depth/shape/size faults fail with fixed
protocol_error, no mutation/truncation/raw provider prose. Bounds apply to output,
not raw SDK HTTP memory. Leaf ffmetadata keeps TIMEBASE=1/1000, stable start order,
positive lengths and escaped backslash/equals/semicolon/hash; controls flatten
through the existing sanitizer. No provider/credential/filesystem/process access.

Tests include exact depth/count/text/wire/metadata bounds, owned copies, malformed
and sparse/zero/unknown metadata and escaping/overlaps. Mandatory CI's generated
silence→copy-remux→ffprobe/hash roundtrip verifies titles/times and unchanged audio
packet bytes with actual ffmpeg/ffprobe. Local absent tools are explicitly skipped;
CI sets TALETTY_REQUIRE_MEDIA_TOOLS=1 and absence fails, never silently skips.
This callable is not embedded in the current library/login runtime; #93 consumes
it only after acceptance. #92 license planning, #93 private transfer/remux/publication
and #94 public Download/API/CLI remain unimplemented. No live/protected-media,
working download/release/full J04/J06 claim; key-in-ffmpeg-argv limitation remains.
