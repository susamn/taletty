# Read-only Python bootstrap (#63)

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
