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
#49 continues via #53 (modal/draft ownership) and #54 (actual PTY/restoration and
acceptance audit); real playback, persistence and cross-book confirmation remain
feature work.

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

## Release evidence

A release candidate needs an evidence matrix linking each supported journey to
passing tests and relevant manual checks. Unsupported/deferred journeys remain
explicitly marked; do not describe the matrix as mathematical proof of every
possible UI path. 95% coverage and journey tests are complementary gates.
