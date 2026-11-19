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
