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
Go→child FD/TTY handoff; #73 adds those below. Finally refresh remains #69 and
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

## Release evidence

A release candidate needs an evidence matrix linking each supported journey to
passing tests and relevant manual checks. Unsupported/deferred journeys remain
explicitly marked; do not describe the matrix as mathematical proof of every
possible UI path. 95% coverage and journey tests are complementary gates.
