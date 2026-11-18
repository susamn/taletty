# Private bridge framing

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
- EOF is **not** successful child exit. #62 must own startup, pipes, bounded shutdown,
  process-group cleanup/reaping and exit-status checks even after decoder failure.
  Cancelable pipes and bounded handoff belong to that owner, not this synchronous
  codec. An arbitrary blocked Go callback is not interruptible.
- #63 registers actual independently covered Python/runtime diagnostics. Typed
  action payload validators land with actual operations (#58/#59/#7), before any
  public operation API exposes results. No live-provider/login/library/CLI claim.

Tests cover fragmented reads, retained-buffer ownership, exact limits, malformed
header/order/termination cases, escaped/optional values, failed reads after terminal
(including data+error in one Read), consumer rejection/panic and native fuzz seeds.
They are service-boundary evidence, not complete UI/Go↔Python journey evidence.
