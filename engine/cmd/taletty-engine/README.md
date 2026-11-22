# Headless account/library/download CLI (`taletty-engine`)

This is the engine CLI (`doctor`, `login`, `sync`, and `download <ASIN>`), **not
the Taletty TUI**. Playback and app queue persistence remain in the `app` module.
See [the engine module README](../../README.md) for prerequisites, source
installation (`go install ./cmd/taletty-engine`), and post-merge `engine/v0.1.0`
release tagging.

From `engine/`, with Go `1.26.0+` and an explicitly prepared CPython 3.12 venv:

```sh
/path/to/venv/bin/python -m pip install -r connectors/audible/internal/bridge/adapter/requirements.txt
go install ./cmd/taletty-engine
taletty-engine doctor --python /path/to/venv/bin/python --json
TALETTY_PYTHON=/path/to/venv/bin/python taletty-engine doctor --json
taletty-engine login --python /path/to/venv/bin/python
taletty-engine sync --python /path/to/venv/bin/python
taletty-engine sync --python /path/to/venv/bin/python --json
taletty-engine download B000000001 --python /path/to/venv/bin/python
taletty-engine download B000000001 --python /path/to/venv/bin/python --json
```

Use `--help`. Flags follow the command (`download` accepts `<ASIN>` before or
after flags). `--python` is optional and resolved via `audible.ResolvePython`
with strict precedence: `--python` flag -> `TALETTY_PYTHON` -> `python3.12` on
`PATH` -> `python3` on `PATH`. Explicit empty, whitespace-only, or NUL-containing
`--python` values fail with usage exit `2` without falling back or echoing
supplied values; missing runtimes fail safely with `runtime_missing` (exit `1`)
across `doctor`, `login`, `sync`, and `download`. There is no first-run `pip`,
system Python mutation, legacy `~/.config/audible-tui` scanning, or migration.
Marketplace defaults to `us`; any other `--marketplace` fails before work. JSON
is available for `doctor`, `sync`, and `download`, never `login`. Usage errors
never echo supplied values.

Doctor is read-only and takes no auth lock: it never creates the Taletty root.
JSON is `{ "ready": bool, "diagnostics": { "python": ..., "audible": ...,
"ffmpeg": ..., "ffprobe": ... } }`. Missing media tools make Ready false/exit 1,
but do not block login's Python/SDK-only preflight. These limited version probes
are not codec, authentication, entitlement or playback proof. Operation errors in
JSON mode are `{ "failure": { "code": ..., "message": ..., "retry": ... } }`.
All fields are typed/sanitized; no raw bridge output or SDK diagnostics.

Login must run in its owning foreground terminal, not a pipe/CI. It creates only
the fresh `$HOME/.config/taletty/` root (0700 requested), verified by the existing
connector; unsafe existing roots/locks are rejected, never chmod-repaired. No XDG
root override or legacy import. Existing valid private roots can be reused. Auth
is atomic private 0600 plaintext, not encrypted. A failed local persistence step
can follow remote device registration; local deletion does not revoke a device.

**Manual live-account sign-in is opt-in, never required by automated tests.**
Use a private terminal: do not record/share screenshots, logs, auth files or the
final redirect URL. Follow the public-challenge browser fallback; wait for the
hidden-input prompt before pasting the entire final URL. Empty/EOF/Ctrl+C cancels.
No password goes into Taletty widgets/arguments. Browser refusal/account restrictions
remain authoritative; successful login does not prove library/media access.

Exit codes: **0** success/help, **1** operation/readiness/output failure, **2** invalid
usage/unsupported marketplace, **130** handled interrupt/TERM/cancellation. The
command owns shutdown signals; the connector joins/cleans its child group and
restores terminal state before return, then CLI closes session/TTY before exit.
Hard kill/parent death cannot guarantee restoration/finally persistence. Cancellation
may race auth publication; `storage_uncertain` explicitly means published but not
confirmed durable. Retry is manual. A restoration warning must not be ignored.

Sync is noninteractive (pipes/no controlling TTY are supported) and uses the same
private root/session and explicit Python/SDK contract. It does not require media
tools. Sign in first; no implicit login, retry, cache or SQLite writes occur. Plain
success is only `Library synced: N books.` JSON success is
`{"snapshot":{"operation_id":"cli-sync","books":[...]}}`, preserving native
`duration_ns` and unknown-vs-empty/zero metadata. A complete empty library has
`books:[]`. No candidate pages/events reach stdout: success is emitted only after
Library completion **and session Close**. Failure uses the existing typed failure
envelope/exit codes, including sign_in/wait retry hints and storage uncertainty.
Do not redirect over your last good cache: check exit0 before replacing caller data.
Output failures can leave incomplete bytes on the output stream; stdout is not an
atomic file publication interface. Session cleanup has already run before output.

Tests use injected fake accounts, synthetic diagnostic children and actual command
entrypoint/controlling PTYs/signals, plus the accepted SDK/TTY connector contracts.
Sync and download command tests additionally run the real embedded SDK with synthetic auth,
fake HTTP/refresh, atomic persistence faults/restart and actual SIGINT/TERM reaping.
Download (`#113`) validates the 10-character ASCII alphanumeric `<ASIN>` before touching
the root, emits output only after `Client.Close(context.Background())`, preserves
committed-media `download` facts alongside `failure` in `--json` mode (`{"download":...,"failure":...}`)
and plain error output (`Media ready: <path>`) when auth persistence, durability, or
cleanup fails after commit, and exits `0` when committed/reconciled ready media wins
late cancellation (`130` on precommit cancellation, `1` on operation/output failure, `2` on usage error).
No live account evidence, full J01/J04/J14, app cache or schema-upgrade claim.
