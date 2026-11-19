# Linux Audible connector

Development API, not a released/runnable Taletty CLI. Use an explicit CPython 3.12
venv with `internal/bridge/adapter/requirements.txt`; installation/platform policy
remains #26. `Doctor(ctx, pythonExecutable)` is separate and read-only.

`Open(ctx, absoluteRoot, pythonExecutable)` returns a `*Client`. Supply the fresh
Taletty-owned root (eventually `~/.config/taletty/`), already created as effective-
UID-owned 0700. No root/runtime/legacy discovery or directory creation occurs.
One client owns the private advisory lock until `Close(ctx)`; another owner gets
`ErrAuthBusy`. Close waits for authenticated work; cancel/join that work before
shutdown. A canceled Close retains ownership and is retryable; closed clients
reject login with `ErrClosed`. Playback and Doctor do not acquire this gate.

`client.Login(ctx, terminal *os.File)` is explicit, synchronous and US-only.
Suspend the UI/other terminal users first. Borrow the controlling foreground TTY
until return, never close/reuse it during the call. The engine serializes foreground
handoffs process-wide, lends only its anchored root and TTY to a dedicated child,
and terminates the group, reaps its direct child and joins owned workers before
restoring foreground ownership and exact terminal settings. SDK/browser execs do not inherit the borrowed descriptors.
Callers translate their own shutdown signals into context cancellation; the library
installs no process-wide signal handlers. Hard parent death cannot guarantee
terminal restoration. Job-control suspension is not implemented as a UI journey.

The initial browser URL is a public-challenge fallback. Paste the final URL **only
after the hidden-input prompt**; never record/share it. Ctrl+C/empty input/EOF cancel.
SDK stdin/stdout/stderr are null; private bounded control JSON never carries auth
or the redirect and is not a CLI JSON mode. No password collection in app widgets.

Nil error requires clean process exit/control EOF, fixed validated completion,
SDK-validated reloaded snapshot and a fresh private auth inode. It is not proof of
entitlement/library access. `*LoginError` exposes only a fixed `Code`/message;
`storage_uncertain` means credentials were published but durability is unconfirmed.
Cancellation does **not** roll back a racing publication. Retry is always explicit;
never assume every failure preserved old credentials. `ErrTerminal` can accompany
cancellation if restoration failed: do not resume terminal rendering as if restored.
Unsafe storage yields `ErrAuthStorage`; unavailable/broken executables/protocols
use the existing runtime errors. Tokens remain private plaintext, not encrypted;
local removal is not remote device revocation. A failed local persistence step
may still follow remote device registration; no automatic remote revocation occurs.

The real PTY/Go→Python tests use a fake external provider and real SDK restore,
with synthetic never-issued placeholders and provider HTTP forbidden. No live
account sign-in or full J01/J14 is evidenced. CLI/library sync is #59; #69 adds private SDK lifecycle/finally persistence to the
embedded adapter, not a public provider operation. No executable login command is
shipped here.
