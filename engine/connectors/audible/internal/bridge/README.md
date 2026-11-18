# Private bridge framing and process ownership

`ReadStream` implements #61, the envelope-reader slice of #57 / ADR 001. It uses
bounded standard-library JSON-lines scanning, not the prototype's all-output buffer.

- Require exact protocol 1/header names, matching nonempty operation ID, positive
  consecutive uint64 sequence and stage/progress/result/error kind. Reject malformed
  JSON/UTF-8, duplicate top-level keys (including escaped aliases), missing headers,
  unknown kinds, missing/duplicate terminals and anything following a terminal.
- Bound line content to 4 MiB, excluding ScanLines' LF/trailing-CR normalization.
  An EOF-terminated final line is accepted. Whitespace inside the content counts.
  Unknown optional values are ignored, not exposed as public defaults.
- Frame.Raw is an owned copy. Stage/progress callback delivery is **provisional**:
  it is a private payload validator/consumer, not the public event reporter. Errors
  and panics propagate. A failure after pages invalidates the proposed snapshot.
- Return a **candidate terminal envelope** only after clean EOF, including error
  envelopes. The action payload is still unvalidated. No callback receives a terminal.
  Never log/forward raw frames, provider messages or decoder/read errors to the app.
- EOF is **not** successful child exit. Linux `Run` (#62) owns startup, pipes,
  cancellable channel handoff, process-group shutdown and exit checks. It reuses
  ReadStream without calling arbitrary consumer code on its reader goroutine.
- #63 registers actual independently covered Python/runtime diagnostics. Typed
  action payload validators land with actual operations (#58/#59/#7), before any
  public operation API exposes results. No live-provider/login/library/CLI claim.

Tests cover fragmented reads, retained-buffer ownership, exact limits, malformed
header/order/termination cases, escaped/optional values, failed reads after terminal
(including data+error in one Read), consumer rejection/panic and native fuzz seeds.
They are service-boundary evidence, not complete UI/Go↔Python journey evidence.

## Linux process lifetime (#62)

`Run(ctx, program, args, request, operationID, frames)` sends one bounded UTF-8 JSON
object plus newline then stdin EOF. It validates the transport header, not the typed
action/root schema. Request input is bounded to 4 MiB before the appended newline.
No shell or old-runtime discovery. Authenticated callers must hold the #56 Session;
read-only Doctor must not initialize/acquire it.

The caller owns the optional private frame channel: keep it open until Run returns,
consume/discard provisional frames, and use context for cancellation. Backpressure
selects on cancellation; there is no arbitrary consumer callback to interrupt.
Stderr goes to the null device, not a growing buffer or public diagnostic.

Success requires both validated stream EOF and a clean child exit. After leader
exit, allow at most 2s for stream draining. On failure/cancel close stdout, unblock
the handoff, TERM the group, allow 250ms grace, then KILL residual members. Linux
waitid(WNOWAIT) observes exit without releasing the PID; signal the group before
Cmd.Wait reaps that anchor. Join the decoder/observer/exec stdin copier before return.
Do not ignore SIGCHLD or install an external reaper for these children. Only the leader is directly
waitable; a descendant's parent/init owns its eventual reaping. Escaped process
groups, privilege changes and kernel-uninterruptible waits are not containment or
absolute deadline guarantees. No runtime/auth/media files are removed by Run;
Download must reconcile an already-published file before retrying a failed transport.
