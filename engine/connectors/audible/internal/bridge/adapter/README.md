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
SDK serialization, parsing/credential validation and finally-refresh persistence
belong to #68/#69; the store itself imports no SDK and calls no provider. Doctor
does not import/call the store. No login command is shipped by this primitive.

Caller supplies a borrowed effective-UID-owned **0700 directory descriptor** and
holds #56's authenticated-operation gate throughout. Keep that FD open/unreused
until return; store does not close it or acquire another lock. Directory-relative
operations remain anchored after rename and never discover/create roots. Actual
Go→child directory-FD handoff is #68, not implemented here. Cooperative same-UID
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

SDK Python chatter is redirected to the null device. **Host must run a dedicated
child with stdin/stdout/stderr discarded** (including OS-level library/browser
output), retain the borrowed root/TTY, hold the auth gate, own foreground handoff,
terminate/join processes and independently restore terminal state on kill/TERM.
Those Go/session/process/bootstrap/public-outcome integrations remain **#73**;
this callable neither acquires locks nor claims finally runs after SIGKILL.

Before publication, require US locale and SDK signing/bearer capability on both
returned auth and actual SDK-restored serialized snapshot. Cookie-only/empty auth
is not success; capability is not entitlement/live account proof. Accepted bytes
use #67's private atomic store. Returns no auth data; `LoginError.code` and message
are fixed, including cancellation, missing/incompatible runtime, storage failure
versus published-but-not-durable uncertainty. Refresh-on-failure remains #69.

Tests use fake external provider/browser callbacks and real pinned SDK restore
with unmistakably synthetic, never-issued placeholders (no account/network).
Real controlling PTY children verify no echo and exact settings after success,
EOF, SIGINT and oversize rejection. These are adapter/TTY boundaries, not complete
app onboarding or live-provider sign-in evidence. CLI wiring remains #59.
