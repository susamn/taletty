# Quality gates and user-journey testing

## Coverage policy

Required at the first executable-code PR, not as a post-release cleanup:

- Each production Go module: >=95.00% statement coverage independently, using
  race-enabled tests and `-coverpkg=./...` so unimported packages count.
- Python adapter: >=95.00% statement coverage independently. Report branch coverage
  too; add a branch threshold only as an explicit reviewed strengthening of policy.
- Report changed-code coverage and every uncovered critical branch. A global
  percentage does not excuse missing credential handling or destructive flows.
- Do not exclude CLI wiring, errors, process lifecycle or migrations for convenience.
  Any technically unavoidable exclusions need a reviewed rationale and compensating
  tests; changing exclusions is a policy change, not a feature-PR shortcut.
- Fail on missing/empty/malformed reports, missing expected modules, and threshold
  violations. Compare unrounded ratios. Report exact command/tool versions.
- Public CI uses deterministic fixtures and generated audio only; no Audible login,
  external network access or protected audiobook downloads are required.

Issue #4 is split into #36 (coverage validator/tooling CI), #37 (real module gates)
and #38 (command harness/check policy). See [tooling instructions](../tools/README.md).
Tooling and real Go foundations are measured independently. Inventory validation feeds
the module CI matrix and rejects unregistered module/adapter source. No adapter or full
UI journey coverage is claimed before code exists. Required checks/branch protection
follow actual verified check names; see [check policy and acceptance audit](quality-checks.md).
The [model command runner](../app/internal/testkit/README.md) is a tested seam, not
full UI-journey coverage. Feature/UI/media gates arrive with production features,
never no-op green jobs.

## Test layers

1. **Pure unit/property tests:** filtering, ordering, identity, time formatting,
   progress math, completion, chapter mapping, sleep policy, bookmark pre-roll.
2. **Service/contract tests:** Go↔Python process framing, engine API compatibility,
   SQLite migrations, atomic files, permissions, token-write serialization,
   process cancellation and sanitized error mapping with fake providers.
3. **UI journeys:** run actual model Init/Update commands and returned messages
   through the app coordinator with injected services, clock and persistence.
   Assert actions, focus, selected identity, resulting view and durable state.
   Helper-only tests are not journey coverage.
4. **Layout/input tests:** table-driven terminal-cell assertions, reviewed golden
   fixtures and realistic metadata in Unicode/Nerd/plain modes. Avoid brittle
   full-screen snapshots as the only oracle.
5. **Terminal integration:** PTY smoke journeys for startup, resize, actual key
   decoding, modal typing, exit and terminal restoration. Keep bounded timeouts.
6. **Real media integration:** generated chaptered `.m4b`, real mpv with null audio,
   ffmpeg/ffprobe where needed; resume, paused seek, speed, EOF, shutdown, killed
   child/parent and no orphan process. Run as a named CI job with dependencies
   installed, not permanently skipped behind a local environment variable.
7. **Manual live-provider validation:** opt-in, documented and redacted. Required
   before claiming working Audible release downloads; never part of public CI.

Use fake time, scripted providers, temp roots and deterministic event scheduling.
No real sleep loops in policy/UI tests. Exercise out-of-order/stale updates and
late completion after cancellation. Run `go vet`, formatting and Python checks.

#79's [private library tests](../tests/adapter/test_library.py) cover bounded SDK
request pagination, within/across-page dedup, repeated/nonterminating-page failure,
strict response/identity/metadata, absent-vs-empty/zero values, Unicode/control safety,
exact page/envelope byte boundary and invalid/nonfinite/overflow durations. Actual
pinned SDK/MockTransport pages test refresh→later throttle/protocol/cancel→persist,
closed clients, failed persistence preserving caller cache and next-operation reuse.
All page candidates remain provisional until exhaustion AND successful SDK context
exit; later Go clean EOF/exit/typed snapshot/event/CLI integration is #80. These are
private provider contracts, not public sync, app cache or full J01/J02/J14 evidence.

#84's [streaming launcher tests](../tests/adapter/test_noninteractive.py) execute
actual isolated fileless Python/SDK children: stage pages followed by count only
after persistence, later throttle/repeated/malformed/cancel/close/storage failures
never emit success, precommit preservation vs published uncertainty, hidden OS
chatter/stdin EOF and further-exec root/control noninheritance. Malformed requests
never construct a client; bounded frame/flush/closed-control/loader faults are
independently covered. Provider HTTP is forbidden. These are private child contracts;
public Go embedding/count validation/snapshot/event/cancel ownership is #85, CLI
sync #86. No full UI/real account/cache/release evidence is claimed.

#85 adds [strict library payload tests](../engine/connectors/audible/library_payload_test.go),
[public lifecycle/process tests](../engine/connectors/audible/library_linux_test.go),
[incoming-page cancellation](../engine/connectors/audible/library_cancel_linux_test.go)
and [real embedded SDK boundaries](../engine/connectors/audible/library_sdk_linux_test.go).
Exact required keys, nested duplicates, malformed Unicode/surrogates/time/count,
optional absence/empty/zero, repeat/size/depth limits and fuzz seeds protect typed
results. Public Go→Python→SDK success/empty/denied/repeated/malformed/unsupported/
cancel/close/storage failures never expose partial snapshots or SDK text. Refreshed
snapshots survive ordinary failure and are reused; canceled queued operations never
construct a client. Socket/PIDFD barriers prove active child exit while caller's
callback is blocked, callback remains caller-owned, then root/process users join;
controlled pre-persistence kill is not a general durability guarantee. Panic and
failure release IDs/gate; nil reporters, preflight refusal, error-copy isolation,
late cancel after completion and nonzero/truncated processes are covered. No CLI
sync/UI/cache/live-account or full J01/J14 claim.

## Required journey matrix

Each row must link to executable tests when implemented. Every feature PR adds its
own rows/tests rather than deferring all UI verification to a final testing issue.

| ID | Journey | Required assertions |
|---|---|---|
| J01 | First launch → onboarding/account setup → cached library | Empty state, cancel/login error/success, no password collected by UI, no autoplay |
| J02 | Launch offline → cached browse → downloaded playback | No network requirement, restore view/selection and per-book position/speed |
| J03 | Search/filter → sort → background refresh → clear/back | Case-insensitive contributor search, stable identity, modal ownership, correct bounds |
| J04 | Queue downloads → transfer → preparation → ready | Deduplication, truthful progress/stages, glyph+text, completed file only |
| J05 | Cancel/retry/reorder → quit/restart with waiting jobs | Process cleanup, terminal event delivery, persistent queue, no phantom Ready |
| J06 | License denial/network/disk-full/conversion failure → recovery | Sanitized errors, no blind entitlement retries, original state preserved |
| J07 | Play A → browse/inspect B → chapter switch → return A | No accidental audio switch, correct target, checkpoint and speed preserved |
| J08 | Pause → chapter seek → seek/speed/volume → resume | Pause preserved, bounds, accurate adjusted remaining time, no fake clock advance |
| J09 | Bookmark while playing/paused → type → save/edit/delete/jump | Captured timestamp, pre-roll clamp, typing safety, confirmation, correct book |
| J10 | Sleep by time/chapter → pause/resume/switch/exit | Fake-clock policy, exactly-once stop/pause, persistence policy, no stale timer |
| J11 | Natural end → Finished → mark unfinished/replay | Completion independent of deliberate stop/seek, correct replay position |
| J12 | Delete downloaded audio → restart | Confirmation, file removal only, notes/history retained, playback safety |
| J13 | Resize wide/compact/minimum → overlay → back → quit | Player visibility, no overflow, focus restored, errors readable, terminal restored |
| J14 | Fresh Taletty root → initialize → sign in/save → restart/upgrade | Only `~/.config/taletty/` used, private permissions, idempotent setup, old app files untouched, Taletty-owned upgrades preserve data |
| J15 | Local import → chapters/resume/bookmarks | Provider-neutral identity, duplicate policy, no Audible auth required |
| J16 | Global bookmark search → jump/export | Correct book target, escaping, safe filename, no credential leakage |
| J17 | Favorite → Favorites view → restart offline → unfavorite (#42) | Stable target distinct from playing book; persistent local state; save failure recovery; safe final-row removal; audio/history retained |

Partial executable J13 evidence now exists in the [shell model journeys](../app/internal/ui/shell_test.go)
and [geometry/text assertions](../app/internal/ui/README.md): real Init/Update/Cmd,
focus/resize recovery, cell-bounded views, Unicode/plain modes, sticky errors and
injected snapshots. The same suite covers J07's browse-target separation only.
[Help owner journeys](../app/internal/ui/help_test.go) additionally exercise real
read-only key ownership, origin restoration, transport/session whitelisting, scrollable
errors and resize/failed-shutdown recovery. These do **not** complete J13/J07:
#53's [modal component journeys](../app/internal/ui/modal_test.go) cover real
textarea ownership, frozen note/confirmation submissions, failed-submit/retry,
stale replies, nested Back/focus and bounded resize/unsaved-quit recovery. These
are coordinator-opened presentation components, not bookmark persistence or actual
cross-book playback. #54 adds [actual controlling-PTY journeys](../app/internal/ui/terminal_linux_test.go)
and a [production terminal lifecycle boundary](../app/internal/terminal/run_linux.go):
real key/Alt+Enter/bracketed-paste decoding, focus/audio separation, frozen editor
failure/retry, nested Back, wide/compact/tiny SIGWINCH, draft-safe quit/recovery,
context cancel/external SIGINT, writer failure and exact termios/foreground/file-status
restoration. Socket acknowledgments observe actual Update transitions, not screen
output timing; bounded contexts and unconditional kill/wait cleanup own children.
The app matrix runs the suite plus a mandatory named three-repeat PTY step on both
Go versions; Quality gate requires it. [The shell acceptance audit](shell-acceptance.md)
records every #12/#49 requirement and deferred domain journey. Real playback,
persistence and feature entry-point integration remain work; parents stay open for
maintainer acceptance. The source-built test host is not a full audiobook app.

For J13 include long CJK/combining text, embedded terminal escapes, missing titles,
no chapters, unknown duration, unknown byte total, many list rows and help/errors
at the minimum size. Golden updates require deliberate review.

The [Audible auth-session boundary tests](../engine/connectors/audible/internal/auth/session_linux_test.go)
exercise real private-directory/flock ownership across Go processes, cancellation-aware
serialization, failed inspection cleanup, panic-safe release and close races. These
are #56's service foundations only, not complete J01/J02 login/library evidence.
The [private bridge stream tests](../engine/connectors/audible/internal/bridge/stream_test.go)
add #61's bounded envelope/framing evidence: fragmented readers, exact line limits,
malformed headers/JSON/UTF-8/order, duplicate terminals/keys, failed reads after
terminal, owned buffers and fuzz seeds. A terminal is provisional until action
validation and process exit checks. The [Linux process tests](../engine/connectors/audible/internal/bridge/process_linux_test.go)
add #62's real request/EOF/exit and cleanup journeys: nonzero exit after result,
malformed/missing output, stderr flood, queued-frame cancellation, blocked pipes,
TERM-ignore escalation, inherited stdout, group-member kill before leader reaping,
and failed exit inspection. Barriers/FD acknowledgments avoid timing guesses;
fixture group members are separately test-owned and reaped, never orphaned.
The [Python adapter tests](../tests/adapter/test_doctor.py) and
[Go↔Python doctor contracts](../engine/connectors/audible/doctor_linux_test.go)
add #63's real bootstrap/SDK and fake-provider/dependency/error evidence. SDK import
and private-root sentinels prove no client construction/user-state writes; CI installs
and requires actual ffmpeg/ffprobe probes. Named Python coverage measures every
production adapter source independently; inventory rejects omitted reports/files.
These are read-only diagnostics, not complete J01/J02 login/library evidence.
The [private auth snapshot tests](../tests/adapter/test_auth_store.py) add #67's
real bounded filesystem boundary: descriptor anchoring/borrowed ownership, private
root/leaf/unique-temp permissions, symlink/hardlink/FIFO/socket rejection, short I/O,
pre-publication rollback versus published-but-not-durable errors, cleanup failure,
retry and real SIGKILL before replace. A synthetic pinned-SDK dict round-trip
forbids provider I/O. The store alone provides neither credential validation nor
Go→child FD/TTY handoff; #73 adds those below. #69 adds finally persistence below;
complete J01/J14 awaits CLI/app integration.

#73 adds [owned terminal journeys](../engine/connectors/audible/internal/bridge/terminal_linux_test.go),
[fileless launcher tests](../tests/adapter/test_interactive.py) and the
[public Go login boundary](../engine/connectors/audible/login_linux_test.go).
Real controlling PTYs cover foreground ownership, success, failed exec, nonzero
exit, background rejection, TERM-ignore→KILL cancellation and explicit lost-FD
restoration failure/recovery. Public Go→embedded Python→fake external provider→real
SDK restore covers hidden input, Ctrl+C/context cancel, anchored-root publication,
old-file preservation on provider failure, stale/malformed completion rejection,
unsafe publication and missing runtime. A real SDK-spawned exec checks root/TTY/
control descriptors are not inherited; OS-level SDK chatter cannot corrupt control
output. Tests use only synthetic never-issued placeholders and forbid provider HTTP.
These are engine/terminal boundary journeys, not completed J01/J14 or live sign-in.
CLI/root initialization, library and app integration remain #59 and later work.

#69's [SDK lifecycle tests](../tests/adapter/test_sdk_session.py) exercise actual
pinned-SDK bearer refresh with fake refresh/HTTP responses: success, 401/403/429,
network/unknown failure, primary-error/close-error precedence, constructor failure,
malformed/missing/unsafe auth, serialization/replace/directory-fsync faults and
recovery. Refreshed snapshots survive ordinary failure without raw provider text.
[Owned Go→Python contracts](../engine/connectors/audible/sdk_session_linux_test.go)
run that embedded lifecycle under WithRoot with a real blocked-provider socket
barrier: queued cancellation never enters/starts a child, throttled completion
persists refreshed auth for the next operation, and active pre-persistence kill
retains old bytes at the controlled barrier (not a general kill-durability promise).
These are private SDK/process boundaries, not library pagination or full UI journeys.

#78 adds [CLI policy tests](../engine/internal/cli/cli_linux_test.go) and
[real entrypoint/process journeys](../engine/cmd/taletty-engine/main_linux_test.go):
actual exit 0/1/2/130, read-only doctor, private root/restart/no-XDG/no-legacy paths,
unsafe-root rejection, explicit runtime/marketplace/JSON restrictions, safe errors,
output/session-close faults, SIGINT/TERM joining a real diagnostic child and broken
stdout without abrupt SIGPIPE. Owned controlling PTYs exercise the real account
client's startup/cleanup without provider network; fake account success/error/cancel
covers CLI policy. Main is measured, not excluded. No live-account/full J01/J14 or
library-sync claim; existing connector PTYs cover hidden input and restoration.

#86 extends the same CLI with complete-only sync (not app-cache/UI evidence).
[Policy tests](../engine/internal/cli/sync_linux_test.go) verify no TTY access,
empty/complete snapshots only after Close, typed retry/failure, no candidate on
provider/close failure, private HOME faults, cancellation and output failure.
[Actual sync entrypoint tests](../engine/cmd/taletty-engine/sync_linux_test.go) run
embedded Python/pinned SDK with synthetic auth and fake HTTP/refresh:100+1 dedup,
empty, later throttle, precommit storage failure, published uncertainty, restart
without refresh, unsafe-root/no-repair/busy/missing-auth and OS broken stdout.
Socket barriers at a real later provider page synchronize SIGINT/TERM: exit130,
no candidate JSON, child reaped, root immediately reusable. Controlled cancellation
before persistence is not a general kill-durability guarantee. Explicit test runtime
wrapper changes trusted transports only; production has no env-based test hooks.
No live account, app-cache/SQLite, download/playback/release or full J01/J14 claim.

#91's [private chapter tests](../tests/adapter/test_chapters.py) cover normalized
hierarchy/structural IDs and owned copies, optional end vs known zero, inherited
missing-start0 policy, safe title fallback, positive stable leaf sorting, escaping,
overlap preservation and exact depth/node/title/tree/ffmetadata limits. A generated
silence/copy-remux/ffprobe/audio-packet-hash test runs with actual mandatory CI media
tools; local absence is explicitly skipped, required CI absence fails. This tests
chapter preparation only, not licensed transfer, private staging/publication, Go
Download/CLI, protected media/live accounts or complete J04/J06. Those remain #92–#94.

#92's [private license and signed-media tests](../tests/adapter/test_licensing.py)
use the actual pinned SDK with fake HTTP and a generated never-issued AES voucher.
They verify the inherited request body, SDK voucher handling, refresh/persist after
success/refusal/throttle/malformed plan and signed transport failure, then recovery.
Invalid ASIN/URL/voucher, fixed errors/redacted plan repr, cleared references, no
blind retries, response/client cleanup, no Amazon headers/cookies and fail-closed
redirect refusal are covered. No external provider HTTP, protected media, transfer
files/publication or complete J04/J06/live-account/release claim is made.

#98's [private media store tests](../tests/adapter/test_media_store.py) cover actual
Linux no-clobber publication, root/download-directory rename anchoring, strict
no-follow ownership/permissions, short/failed writes, validators and name mutation,
inode-owned cleanup, precommit failures and postcommit fsync/close uncertainty.
Repeated barrier-synchronized rename races distinguish identical validated reuse
from conflict. Real synchronous SIGINT before/after syscall commit tests inode
reconciliation; a pipe-acknowledged SIGKILL leaves a private partial and retry
never sweeps it. Mandatory CI publishes/ffprobes generated silence through a test
validator; local missing tools are reported as skipped, required CI absence fails.
This is a production filesystem primitive, not yet production remux/validation,
licensed composition, embedded download, public Go/CLI or complete J04/J05/J06.
Those remain dependency-linked #99/#100/#94 after maintainer acceptance.

#100's [private composition tests](../tests/adapter/test_composition.py) combine
real pinned SDK fake HTTP/refresh, a never-issued synthetic voucher, fresh signed
stream header isolation, real owned store writes/rename and production composition.
Fault tests use an explicit copy/validator seam (not media validation evidence) to
exercise ordinary refusals/throttling/redirect/body/write/remux failures, optional
chapter metadata fallback, unsafe-ready refusal, identical reuse/conflict, exact
precommit/postcommit/unknown publication, independent auth pre-replace/post-replace
uncertainty, close failure, cancellation and recovery. A separate mandatory actual
ffmpeg/ffprobe generated-clear-audio test uses the production remux and validator,
checks audio packet hashes, then proves verified ready reuse without authentication.

[Actual composition process tests](../engine/connectors/audible/composition_process_linux_test.go)
run the real private consumer under existing WithRoot/RunRoot, not a public action.
Socket barriers prove queued cancellation never starts another child and active
controlled pre-persistence cancellation reaps the direct child and releases ownership.
A separate real post-media-commit/pre-auth-save barrier proves cancel/join retains
the final valid artifact and prior auth at that controlled window; the next isolated
operation verifies and reuses it without another license or transfer. The engine
matrix repeats these actual composition journeys three times with named verbose
logs, required media tools and bounded deadlines on both Go versions.
Required-media execution runs real copy/probe children with a further-exec inspector
proving same process group and only intended media descriptor inheritance (no root,
auth lock or control). SDK refresh/refusal/recovery and ready-before-license reuse
cross the actual isolated process boundary. Neither test promises cleanup/finally
or durability after hard kill, containment of escaped children, real provider media,
public Download/CLI, persistent queue/UI journeys or a working release. Media and
auth uncertainties remain separate for downstream #94 reconciliation.

#111's [private download producer and reconciliation tests](../tests/adapter/test_download_producer.py)
and [Go producer process tests](../engine/connectors/audible/download_producer_linux_test.go)
exercise the embedded fileless `download` and read-only `reconcile` entrypoints
under `RunRoot`/`WithRoot`: structured Audible/US request validation, independent
media/auth outcome facts, ready reuse without auth, joined pre/postcommit cancel
followed by provider-free reconciliation under the same root loan, seeded
`ready`/`absent`/`invalid`/`unsafe`/`unknown` states and nonzero/truncated child
recovery. Deterministic copy/probe fixtures are not real-media proof;
`TestDownloadProducerActualGeneratedMedia` runs the actual production ffmpeg/ffprobe
path, skips locally only when media tools are absent, and fails in mandatory CI
(`TALETTY_REQUIRE_MEDIA_TOOLS=1`) if tools are missing.

#112 adds [neutral download model tests](../engine/model/download_test.go),
[strict download payload tests](../engine/connectors/audible/download_payload_test.go),
[public Download lifecycle tests](../engine/connectors/audible/download_linux_test.go)
and [embedded SDK/process download tests](../engine/connectors/audible/download_sdk_linux_test.go).
Neutral tests cover bounded `Chapter` hierarchy/IDs/text/native times, optional
unknown vs known-empty/zero JSON, safe rooted `Media` paths and independent
`DownloadResult.Clone` ownership. Strict decoder tests and `FuzzDownloadPayload`
cover exact keys, nested duplicates, lone surrogates, depth bounds, integer-ms
conversion, outcome contradictions and `reconcile` states. Public lifecycle and
embedded SDK/fake-HTTP tests run unconditionally locally: shared live IDs across
`Library` and `Download`, nil/panic/blocked caller callbacks, `Close` joining,
rename-safe root-inode path derivation, refusal/refresh persistence, auth/media
durability uncertainty, ready reuse across `Client` restart without auth, seeded
recovery without auth, canceled-outcome recovery with/without auth failure, and
pre/postcommit cancel/SIGKILL socket barriers joining original users before
bounded provider-free reconciliation under the same gate. Deterministic copy/probe
fixtures do not prove real-media readiness; `TestActualPublicDownloadGeneratedMediaAndRestart`
exercises the actual production ffmpeg/ffprobe path, skips locally only when media
tools are absent, and fails in mandatory CI (`TALETTY_REQUIRE_MEDIA_TOOLS=1`) if
tools are missing.

#7 adds structured download stage/progress streaming tests across the Python
adapter ([`test_licensing.py`](../tests/adapter/test_licensing.py),
[`test_composition.py`](../tests/adapter/test_composition.py),
[`test_noninteractive.py`](../tests/adapter/test_noninteractive.py),
[`test_download_producer.py`](../tests/adapter/test_download_producer.py)) and
Go engine ([`download_test.go`](../engine/model/download_test.go),
[`download_payload_test.go`](../engine/connectors/audible/download_payload_test.go),
[`download_linux_test.go`](../engine/connectors/audible/download_linux_test.go),
[`download_sdk_linux_test.go`](../engine/connectors/audible/download_sdk_linux_test.go),
[`composition_process_linux_test.go`](../engine/connectors/audible/composition_process_linux_test.go)):
strict `Content-Length` parsing (known, zero, absent, malformed, negative,
smaller, overflow), stage ordering, verified ready reuse skipping stages, closed
control pipe during progress without manufacturing success, `SIGTERM`/`SIGINT`
killing the `ffmpeg` process group and cleaning partials, strict Go
stage/progress validation and `FuzzDownloadPayload`, unknown vs known-zero
`Total`, monotonic public `Sequence`, independent pointer copies, stalled
consumer backpressure + cancellation, killed `ffmpeg` process group +
reconciliation, and mandatory CI generated-media verification of
`licensing -> downloading -> progress -> preparing -> publishing -> completed`.
App storage/queue (#16), UI J04/J05/J06 (#17), live
accounts, protected media and full app release remain open.

## Engine release preparation (#8)

[`runtime_linux_test.go`](../engine/connectors/audible/runtime_linux_test.go),
[`session_linux_test.go`](../engine/connectors/audible/internal/auth/session_linux_test.go),
[`login_linux_test.go`](../engine/connectors/audible/login_linux_test.go),
[`cli_linux_test.go`](../engine/internal/cli/cli_linux_test.go),
[`main_linux_test.go`](../engine/cmd/taletty-engine/main_linux_test.go), and
[`release_linux_test.go`](../engine/release_linux_test.go) verify:

- Contract and runtime pinning constants (`ContractVersion = "v0.1.0"`,
  `BridgeProtocolVersion = 1`, `PinnedPythonVersion = "3.12"`,
  `PinnedAudibleVersion = "0.13.1"`) matching `requirements.txt`,
  `requirements.lock`, and `doctor.py`.
- `ResolvePython` / `DiscoverPython` strict precedence (`explicit` >
  `TALETTY_PYTHON` > `python3.12` > `python3`), clean rejection of invalid
  explicit/env overrides without fallback, `ErrRuntimeMissing` when absent, and
  no scanning of `~/.config/audible-tui` or legacy paths.
- Optional `--python` CLI wiring across `doctor`, `login`, `sync`, and
  `download` (flag override, `TALETTY_PYTHON`, `PATH` fallback, exit `2` without
  echoing invalid explicit `--python`, and `runtime_missing` exit `1` without
  creating `~/.config/taletty`).
- `auth.OpenBorrowed` and `audible.OpenBorrowed`: borrowing an externally locked
  `0700` root without `ErrAuthBusy`, `Session.token` serialization across
  operations, `Close` closing only the duplicated root descriptor without
  unlocking `.auth.lock`, rejection of nil/closed/regular/symlink/non-0700/
  foreign-UID roots, and real `Doctor`/`Login`/`Library`/`Download` fixtures.
- Clean temp-dir `go install ./cmd/taletty-engine` executing `taletty-engine
  doctor --json` against prepared and missing runtimes, plus an ephemeral
  external consumer Go module created dynamically in `t.TempDir()` (`GOWORK=off`,
  local `replace` only inside the temp `go.mod`, never committed in the repo)
  compiling and running against `engine/model` and `engine/connectors/audible`.

## Executable storage contract (#3)

`python tests/storage/test_schema.py` uses stdlib sqlite3 as a test-only oracle
against the app-owned v1 SQL artifact, printing the actual SQLite/Python versions.
Coverage tooling runs it as a mandatory standalone step; workflow regression also
executes that entry point and rejects missing execution, zero tests or skips. Real
temporary databases exercise identity/optionality, FKs/checks, independent local
state, complete-snapshot commit/rollback, migration rollback, markers and reopen.
SQL statement coverage is N/A (no SQL coverage instrument); execution is not a
substitute for the existing independent Go/adapter/tooling statement gates. See
[ADR 002](adr/002-storage-contract.md) for runtime #9/#10 boundaries: no production
opener/driver, OS permission proof, upgrade runner or completed J14 is claimed.

## Linux borrowed store-root foundation (#106)

[`root_linux_test.go`](../app/internal/store/root_linux_test.go) exercises production
`store.WithRoot`: real private roots, rename/replacement isolation, all four fixed
DB/sidecar names with private/public/special-mode/symlink/hardlink/FIFO/socket/directory
fixtures, unchanged unknown data, cancellation, callback error/panic cleanup and
retry, independent concurrent read leases and ordinary-exec noninheritance. A real
child proves an externally held `.auth.lock` remains busy; this models the exact
engine lock contract, not engine integration. Owner mismatch and inspection I/O
failures are injected at the stat boundary (no privileged chown). Existing profile
path tests retain no-XDG/no-old-app evidence. This is partial J14 service evidence,
not SQLite connection/migration, bootstrap, persistence or a complete UI journey.

## Linux SQLite transport and validated v1 opener (#114, #115)

[`sqlite_linux_test.go`](../app/internal/store/sqlite_linux_test.go),
[`validated_linux_test.go`](../app/internal/store/validated_linux_test.go) and
[`validation_faults_linux_test.go`](../app/internal/store/validation_faults_linux_test.go)
exercise the pinned `ncruces/go-sqlite3` transport, `store.Open`/`Check`/`Close`
and `NewOwner`/`RootLease` against real temporary SQLite databases and the exact
embedded v1 SQL artifact. Real regression tests discovered that SQLite 3.51.3's
`PRAGMA integrity_check` skips `CHECK` constraints under `mode=ro`; inspection
therefore opens with `mode=rw` (no create) behind an inspection VFS write/truncate/
sidecar/delete ban and `O_NOATIME` reads while retaining upstream OFD locks.
Refusal tests verify byte-for-byte and `O_NOATIME` stat metadata (including
atime/ctime) preservation across foreign, nonempty unversioned, corrupt, too-new,
forged-schema, FK-violating and CHECK-violating databases, plus all `-journal`/
`-wal`/`-shm` sidecars. An abruptly exited child produces a real hot journal that
`Open`/`Check` refuse and preserve without auto-recovery. `Owner`/`RootLease`
tests prove session-lifetime cancellation/join and retryable `Close` remain
independent of `.auth.lock` and any external auth gate. See
[the store README](../app/internal/store/README.md) and
[ADR 002](adr/002-storage-contract.md) for constrained-host cache notes and
explicit limits: no #108 typed repositories, #10 root bootstrap, engine
integration, general crash/disk-full proof or full J14 acceptance is claimed.

## Serialized mpv playback adapter (#11)

[`playback_linux_test.go`](../app/internal/playback/playback_linux_test.go) and
[`mpv_integration_linux_test.go`](../app/internal/playback/mpv_integration_linux_test.go)
exercise `Controller` and `MPVProcess` over a `CLOEXEC` Unix `socketpair`
(`--input-ipc-client=fd://3`, `--idle=yes --keep-open=yes`, named `loadfile`)
and `waitid(WNOWAIT)` + `SIGKILL` process-group reaping against real `*store.Store`
SQLite checkpoints. Fake JSON-IPC child tests cover saved `ListeningState`
restore, `Play`/`Pause`/`Toggle`, paused exact seek, speed `[500000, 3000000]`
and volume `[0, 100]` bounds, book switching, `Poll` + EOF checkpointing, stale
`Session` rejection, store/backend faults, queued cancellation, malformed/oversized
IPC lines, process-group reaping, and killed-parent `fd://3` EOF exit. Real
generated chaptered `.m4b` (`ffmpeg`) + `mpv --ao=null` integration tests cover
load, pause, paused exact seek, speed, volume, book switch, EOF, orderly `Close`,
killed child recovery, and `SIGKILL` parent subprocess non-orphan verification;
they skip locally only when `mpv`/`ffmpeg` are absent, fail when
`TALETTY_REQUIRE_MEDIA_TOOLS=1` in CI, and repeat three times under `-race` in
the `app` matrix. See [the playback README](../app/internal/playback/README.md);
`#15` coordinator/UI wiring, `#18` bookmarks, `#20` completion/smart-rewind, and
`#21` sleep timer remain open.
## Fresh private profile root bootstrap (#10)

[`paths_test.go`](../app/internal/profile/paths_test.go) and
[`bootstrap_linux_test.go`](../app/internal/profile/bootstrap_linux_test.go)
exercise `profile.DefaultRoot`, `profile.Open`, and `Workspace.Close` against
real temporary home directories and subprocesses: fresh first launch creating
`~/.config/taletty/` (`0700`), `downloads/` (`0700`), `.auth.lock` (`0600`),
and `taletty.sqlite3` (`0600`); untouched seeded `~/.config/audible-tui/` and
`XDG_CONFIG_HOME` (`O_NOATIME` stat/byte preservation); untouched `auth.json`
(`0600`); full Taletty-owned repository persistence across restart and startup
reconciliation of interrupted active download jobs (`JobReady` vs
`JobFailed`/`FailureInterrupted`); real subprocess `ErrBusy` exclusion,
post-`Close` reopen, `Close` cancel/retry without unlocking underneath active
leases, and `/proc/self/fd` noninheritance across `exec`; adversarial no-repair
refusal of unsafe `~/.config`, `taletty`, `downloads`, `.auth.lock`, foreign/
corrupt/too-new DBs, and sidecars; and injected `sysOps` I/O faults and path
confinement checks. See [the profile README](../app/internal/profile/README.md)
and [ADR 002](adr/002-storage-contract.md). Full `cmd/taletty` CLI/UI startup
wiring, live sign-in, and Taletty v2 schema upgrades remain future work.

## Persistent sequential download-job coordinator (#16)

[`coordinator_linux_test.go`](../app/internal/downloads/coordinator_linux_test.go)
exercises `downloads.Coordinator` against real `*store.Store` temporary SQLite
databases, anchored `store.RootOwner` private-file verification under
`downloads/`, and deterministic channel/barrier fake `Downloader` fixtures (no
real sleeps): startup reconciliation of interrupted active jobs (`JobReady` for
verified private files vs `JobFailed`/`FailureInterrupted` for missing/unsafe
files) with preserved waiting queue order; concurrent `Enqueue` deduplication
(`ErrAlreadyQueued`, `ErrAlreadyDownloaded`) and sequential single-worker
execution; stage transitions (`licensing`, `downloading`, `preparing`,
`publishing`), monotonic `BytesReceived` with known vs unknown `BytesTotal`,
and rejection of stale `OperationID`, out-of-order sequence, backwards stage,
and invalid progress events; active cancellation keeping `Canceling` visible
and the active slot occupied until worker join, plus commit-point race
resolution (`JobReady` + `LocalMedia` winning over late cancel without
unlinking committed media vs `JobCanceled` pre-commit); waiting queue
`RemoveWaiting` (preserving active job, local media, history, bookmarks, and
favorites) and atomic `ReorderWaiting`/`MoveWaiting` preserving authoritative
order on persistence failure; explicit `Retry` for terminal `JobFailed`/
`JobCanceled` only with zero automatic retries on `entitlement_denied` or
`throttled` and sanitized failure messages; store fault injection; non-dropped
stage/terminal `Watch(ctx)` delivery and stalled-watcher overflow safety; and
idempotent `Close(ctx)` cancellation/join. See
[the downloads README](../app/internal/downloads/README.md); terminal UI
views/actions (`#17`, J04–J06) and `engine` integration (`#13`) remain open.
## Persistent timestamped bookmarks and targeting policy (#18)

[`service_linux_test.go`](../app/internal/bookmarks/service_linux_test.go)
exercises `bookmarks.Service` against real `*store.Store` on temporary SQLite,
real `*playback.Controller` (with a fake `playback.Backend`), real `*store.Owner`
private roots, and a deterministic fake clock: playing and paused capture,
`ErrNoLoadedAudio` rejection when idle/stopped/failed/nil-book, highlighted-vs-loaded
book separation, pre-roll zero clamping and chapter-boundary distinction, delayed
save after playback advances or switches books, multiline note validation
(rejecting whitespace-only, control characters, Unicode `Cf`, and invalid UTF-8),
duplicate positions with distinct stable IDs, `EditNote` without recapturing
position or reapplying pre-roll (plus missing/wrong-book rejection), single-bookmark
`Delete` preserving audio/history/favorites, offline `List` ordering, same-book
paused jump without double rewind, stale session rejection, cross-book jump
(preserving prior checkpoint via `Controller`, loading target, seeking exact
position, including root rename and unknown catalog duration), missing/unsafe/
symlink/hardlink/mode-mismatch local audio returning `ErrMediaMissing` without
touching current audio, mid-check stale confirmation rejection, store/checkpoint
faults retaining drafts/state, and `-race` concurrency. See
[the bookmarks README](../app/internal/bookmarks/README.md); UI modal/key wiring
(`#19`), full J09 UI journeys, and global bookmark search/export (`#23`) remain
open.

## Session coordinator, cached boot, and onboarding UI (#131 under #13)

[`coordinator_linux_test.go`](../app/internal/app/coordinator_linux_test.go)
and [`account_test.go`](../app/internal/ui/account_test.go) exercise
`app.Coordinator` and `ui.Model` against real `*profile.Workspace` and
`*store.Store` temporary SQLite databases, fake `AccountClient` /
`TerminalHandoff` seams, and `testkit.Runner`:

- **J01 onboarding & account lifecycle**: fresh `~/.config/taletty/` boot
  opening the Account/onboarding overlay without autoplay or legacy
  `~/.config/audible-tui/` discovery, login cancel/error (`storage_uncertain`)/
  success via TTY handoff without collecting passwords in UI widgets, explicit
  `Sync` publishing atomically via `store.ReplaceCatalog`, and `ViewState`
  persistence on `Close`.
- **J02 offline restoration**: reopening with `Account == nil` (no Python or
  network) restores `Focus`, `Selection` by encoded `BookKey`, `GlyphMode`,
  favorites, local media/job availability, and stopped/finished last-book
  player metadata without autoplay.
- **Failure, concurrency, and recovery**: failed/canceled/stale `Sync` replies
  preserving existing cached catalog and selection, concurrent busy guard,
  `Doctor` readiness states, `Close(ctx)` timeout/retry joining active workers,
  Account overlay key ownership (blocking transport/quit/pane shortcuts),
  resize preservation across `79x19`/`80x20`/`144x42`, and ANSI sanitization.

See [the app coordinator README](../app/internal/app/README.md); concrete
`engine/v0.1.0` pinning and `app/cmd/taletty` entrypoint wiring remain `#132`
(blocked by `#131` and maintainer tagging `engine/v0.1.0`).

## Runtime packaging and platform matrix (#26)

[ADR 003](adr/003-runtime-and-platform-matrix.md) defines the OS/architecture
support matrix, tested-evidence minimum versions, isolated managed CPython 3.12
venv packaging strategy, offline vs network boundaries, `~/.config/taletty/`
preservation rules, and checksum/SBOM/provenance requirements for `#27`–`#29`.

[`tests/tooling/test_packaging_contract.py`](../tests/tooling/test_packaging_contract.py)
runs under the `coverage-tooling` gate (without adding unmeasured production
Python source) and verifies:

- [`requirements.lock`](../engine/connectors/audible/internal/bridge/adapter/requirements.lock)
  pins exact `==` versions for `audible==0.13.1` and its 14 runtime transitive
  dependencies with no unpinned, VCS, or local-path entries, and excludes
  test-only `coverage` and `pip`.
- [`tools/requirements.txt`](../tools/requirements.txt) pins `coverage==7.6.1`.
- `doctor.py` (`PROTOCOL`, `AUDIBLE_VERSION`, CPython `3.12`) and
  `engine/connectors/audible/version.go` (`ContractVersion`,
  `BridgeProtocolVersion`, `PinnedPythonVersion`, `PinnedAudibleVersion`) agree
  with `requirements.txt` and `requirements.lock`.
- Cross-references and relative links across ADR 003 and
  [the distribution plan](distribution.md) remain consistent.

`GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` verifies `engine` and
`app` source cross-compilation only; it does **not** prove `linux/arm64` runtime
support, which remains gated on native or emulated runtime verification in
`#27`/`#28`.

## Release evidence

A release candidate needs an evidence matrix linking each supported journey to
passing tests and relevant manual checks. Unsupported/deferred journeys remain
explicitly marked; do not describe the matrix as mathematical proof of every
possible UI path. 95% coverage and journey tests are complementary gates.
