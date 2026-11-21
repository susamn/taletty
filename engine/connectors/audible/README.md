# Linux Audible connector

Development API; a [headless doctor/login/sync source CLI](../../cmd/taletty-engine/README.md)
now consumes it (not the Taletty TUI or a release). Use an explicit CPython 3.12
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
account sign-in or full J01/J14 is evidenced.

`client.Library(ctx, operationID, report)` now exposes a complete typed snapshot
using the accepted embedded SDK/page/persistence pipeline. See the [library API
contract](library.md) for fixed safe failures, ordered provisional-count events,
strict schema/bounds, cancellation and callback ownership. Every error returns a
zero snapshot; no app cache is written. #86 exposes this API through CLI sync;
#80/#59/#5 remain open for downstream implementation/acceptance.

## Private download producer and reconciliation (#111)

The embedded, fileless download entrypoint consumes accepted `composition.run`
through the same noninteractive loader/stdio and `RunRoot` owner. A separate
root-only `reconcile` entrypoint performs read-only, provider-free packet
validation with a five-second probe bound. Ready proves validated existence, not
this attempt's commit, auth persistence, media durability or provider hierarchy.
See the [private adapter contract](internal/bridge/adapter/README.md#fileless-download-producer-and-read-only-reconciliation-111).

## Typed Linux Download API (#112)

`client.Download(ctx, operationID, bookID, report)` exposes the public US-only
download lifecycle using the shared `Client` session, root lock, cancel-aware
gate and live operation ID namespace shared with `Library`. See the
[download API contract](download.md) and [neutral model contracts](../../model/README.md).

`DownloadResult` keeps `State` (`ready`/`absent`/`invalid`/`unsafe`/`unknown`),
owned `Media` (with rename-safe root-inode path derivation and nil hierarchy on
reuse/restart/recovery), `Reused`, nullable `Published`, `MediaDurability`,
`AuthPersistence`, `MediaFailure` and `AuthFailure` independent even on error.
Auth persistence errors take operation-error precedence without erasing committed
media. Failed, canceled, killed, nonzero or truncated children are joined before
running bounded provider-free `reconcile` under the same `WithRoot` loan; ready
media wins media cancellation, while killed/nonzero/malformed transport retains
`protocol_error` alongside ready media.

Deterministic copy/probe fixtures exercise local failure/recovery without media
tools and are not real-media proof. `TestActualPublicDownloadGeneratedMediaAndRestart`
(and #111's `TestDownloadProducerActualGeneratedMedia`) run the actual production
ffmpeg/ffprobe path, skip locally only when media tools are absent, and fail in
mandatory CI (`TALETTY_REQUIRE_MEDIA_TOOLS=1`) if tools are missing. CLI download
(#113), #110, #94, #6, progress (#7), app queue/storage, UI J04/J05/J06, live
accounts, protected media and a working release remain open.
