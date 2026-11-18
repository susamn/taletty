# Audible authentication session (Linux)

Implemented foundation for #56 / parent #5. No provider calls, token persistence,
Python bridge, login UI or headless command is implemented by this package.

- `Open(ctx, root)` takes exclusive nonblocking process ownership of an **existing**
  absolute, lexically cleaned, effective-user-owned 0700 directory. Root initialization is a separate
  explicit operation; there is no discovery or import of old app data.
- Descriptor-relative `.auth.lock` creation/open uses no-follow and close-on-exec.
  Require an effective-user-owned, single-link regular 0600 file. Reject unsafe
  roots/files; never silently chmod existing user paths or follow a final symlink
  (including paths spelled with a trailing slash or dot component).
  The lock is not unlinked on close: unlinking could let another process lock a
  different inode while the original session still owns the first.
- Hold one Session for the engine lifetime. `Run(ctx, operation)` serializes auth
  writers inside it; do not create a new session or reacquire the root lock per job.
  A different process/session gets ErrBusy, not an unbounded wait.
- Run checks cancellation both before and after acquiring the token. The operation
  receives its caller's context and must honor it. Error/panic releases the token;
  provider-error sanitization is the connector's later responsibility.
- `Close(ctx)` waits for current work. A canceled close leaves ownership intact;
  cancel/join work then retry Close. It never unlocks beneath an active operation.
  Repeated Close with a live context succeeds. Do not reenter Run/Close from a Run
  callback; they deliberately serialize. Callbacks and sessions are not copied.
- No goroutines/processes are spawned by this primitive. It cannot interrupt arbitrary
  blocking callbacks/kernel I/O. Bridge process-group termination/reaping belongs to
  #57. Playback does not use this gate. Read-only Doctor must not call Open.
- Advisory locking protects cooperative clients. Same-UID hostile directory/lock
  replacement is outside its threat model. Parent path components may be symlinks;
  the opened directory descriptor anchors the lock lookup. macOS is not supported.

Tests use private temporary roots, injected cancellation/descriptor inspection,
channels and bounded real Go subprocesses. They verify exclusion/release, private
permissions, unsafe symlink/FIFO/hardlink/ownership rejection, failed-inspection
cleanup, queued cancellation, panic recovery, descriptor inheritance flags and close
races/failures. No credential/provider/media fixtures or sleeps. This is a real
service boundary, **not** completed login/library or UI-journey evidence.
