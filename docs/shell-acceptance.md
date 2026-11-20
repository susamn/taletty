# Shell acceptance audit — #54 / #49 / #12

Scope: accepted main `86b9c32` (including PR #97's `f57c3a4`), plus #54's
production Linux terminal boundary and real-program regressions. #53 is closed
and accepted. This is credential-free **shell** evidence, not an audiobook app,
durable bookmark service, complete J07/J13, live-account test or release claim.
Parents #49 and #12 remain open for maintainer acceptance of this audit.

## Requirement-by-requirement evidence

| Requirement | Implemented evidence | Remaining boundary |
|---|---|---|
| #12 approved geometry and player budgets | [layout tests](../app/internal/ui/layout_test.go), [shell views/cell bounds](../app/internal/ui/shell_test.go), [plain minimum golden](../app/internal/ui/testdata/shell-minimum-plain.golden) | Artwork intentionally deferred by approved R1; no image protocol |
| #12 content focus, selection, compact active pane | Actual-model Tab/numbers/Books navigation; real decoded Tab/Shift+Tab/numbers in [PTY journey](../app/internal/ui/terminal_linux_test.go) | Library destinations, detailed chapter navigation, filter/sort/panning require owning feature integration; not silently advertised as working |
| #12 supporting owner focus/back | [Help journeys](../app/internal/ui/help_test.go), [modal journeys](../app/internal/ui/modal_test.go), PTY Help/confirmation/editor nested Esc | Downloads/Bookmarks/Settings/account destinations not implemented; their transport rules are documented in [UI contract](../app/internal/ui/README.md#supporting-view-transport-contract) |
| #12 contextual hints and disabled actions | Help rows distinguish actual bindings, disabled requests and unavailable features; tested origin/focus restoration | Domain menus/hints arrive with real services; no invented root shortcuts |
| #12 activity/error row | Sticky sanitized failure and explicit dismissal tests; PTY fake shutdown fails, error is observed, Help/Back retains owner, retry quits | Real worker/checkpoint/job persistence and failure reconciliation are not implemented |
| #12 glyph modes / text safety | Unicode/plain model/cell tests, long CJK/combining/ANSI metadata, missing data and unknown duration | Nerd glyph selection, config/theme persistence remain #34; no font switching |
| #12 current-audio miniplayer and browse separation | Injected loaded A versus browsed B; decoded Space emits A/session action; later snapshot does not retarget frozen editor | No mpv/checkpoint/speed policy implementation; not complete J07/J08 |
| #12 no provider calls in rendering | UI uses immutable display projections and typed actions; [terminal.Run](../app/internal/terminal/run_linux.go) knows no providers | Engine/app/domain integration remains separate |
| #12 J13 actual model + PTY resize/key/exit | Model runner executes real Init/Update/Cmd; new terminal tests execute real Bubble Tea v2 and Bubbles textarea inside owned controlling PTYs | Shell portion only; no claim of integrated domain J13/release evidence |
| #49/#52 read-only Help | Actual-model scroll/error/focus and transport whitelist tests; PTY blocks q in Help then restores origin | Feature Help contexts added with their owners |
| #49/#53 modal typing/ownership | PTY real q/Space/digit/Ctrl+C, Alt+Enter (`ESC CR`), bracketed multiline paste; no leaked audio/focus/quit actions | Terminal-emulator-specific alternate key encodings beyond accepted Bubble Tea decoding are not asserted |
| #49/#53 frozen context, correlated authoritative recovery | Typed fake coordinator receives original A and draft after new B snapshot; failed save retains draft; stale reply cannot close second pending attempt; matching success closes | Fake completion is authoritative only for fixture. Nothing is saved durably; #18/#19 provide bookmark services |
| #49/#53 nested Back and bounds | PTY child confirmation consumes q; Esc restores editor and draft. Model tests cover local buttons, confirmation failure/retry, all dialog cell bounds at 80×20 | Destructive domain execution/revalidation belongs to its coordinator |
| #49/#53 zero/minimum/tiny draft/quit | Boundary-grid/zero model tests; real SIGWINCH from wide144×42 to tiny79×19, compact80×20 and 1×1. q/Ctrl+C warns; Esc keeps; repeated q requests quit | No audio continuity claim beyond unchanged injected state/actions |
| #54 terminal startup/restoration/exit | Foreground controlling PTY, real program startup, orderly quit, context cancel, external SIGINT, writer failure. Exact termios, foreground group and file-status flags compared after Run | Abrupt SIGKILL/kernel failure cannot guarantee restoration; caller must exclusively own borrowed terminal |
| #54 timeouts/no orphan test programs | Socket acknowledgments of actual Update transitions, bounded contexts/socket deadlines, joined reader/drain workers and unconditional child kill/wait cleanup | No production domain subprocesses exist in this shell; mpv/media ownership remains #11 |
| #54 mandatory CI and independent coverage | App module race+coverage suite plus named three-repeat PTY step on **both** Go matrix versions; existing Quality gate requires matrix success | No environment switch can skip the parent journey; helper child selection exists only in `_test.go` |

## Production terminal boundary

`terminal.Run(ctx, model, tty, output)` borrows an exclusively owned foreground
controlling terminal. Nil output writes to that terminal. It never discovers user
files, accesses providers or installs signal handlers. The caller translates OS
signals into context cancellation; decoded Ctrl+C remains the active UI owner's
key. The coordinator emits `tea.Quit` only after its workers have stopped/joined.
The shell fixture deliberately injects a fake coordinator, not an alternative UI
state machine. Bubble Tea owns key decoding, raw input, resize and rendering.

Repeated real race tests exposed two upstream cleanup hazards: the old Ultraviolet
reader returned before joining its byte reader, and Bubble Tea v2's context-kill
path skips the input join. The app pins Ultraviolet `20261008173134-6b8d4baf91b4`
(and its MVS-required transitive versions) for the upstream join fix; Bubble Tea and
Bubbles remain v2.0.0. An owned, joined watcher translates external cancellation to
orderly `Program.Quit`, then returns the caller context error after cleanup. This
avoids the unsafe kill path, not the race detector. Model updates and commands must
remain nonblocking/cooperatively canceled; cancellation cannot interrupt arbitrary
caller code. Twenty final race repetitions of all five PTY scenarios passed locally
after those fixes; earlier failing exploratory runs are not counted as success.

The wrapper restores saved termios even on early run failure and reports restoration
or output-write failure (Bubble Tea v2 otherwise discards those errors). It does not
change foreground process group or terminal font. A failed writer is reported after
program shutdown; it is not a transactional screen-output guarantee. A separate
writer retains the borrowed terminal's geometry/resize ownership.

There is no shipped `taletty` application CLI yet. The source-built test host is a
bounded **shell development harness**, not a usable audiobook client. To run its
real terminal evidence from `app/`:

```sh
GOWORK=off go test -count=3 -race -timeout=60s \
  -run '^TestControllingTerminalJourney$' -v ./internal/ui
```

All synthetic state travels through test-only pipe/socket channels; production has
no environment-based test hooks. No credentials, issued tokens, live accounts,
provider calls or protected media are needed. Generated chapter media is a separate
mandatory CI test; a local skip when ffmpeg/ffprobe are absent is not media evidence.

## Deferred domain journeys

J01/J02/J14 onboarding/cache/root app integration, J03/J17 search/favorites storage,
J04–J06 downloads, J07/J08 actual playback, J09 bookmarks, J10/J11 timer/completion,
J12 audio deletion and full J13 integrated app shutdown remain their owning issues.
J15/J16 local import/global search/export remain explicitly deferred in R1. Passing
this shell suite does not close those journeys or imply release readiness. The
accepted #47/#48/#52/#53 slices and this #54 evidence can be reviewed independently
without fabricating domain implementations or closing unaccepted parents.
