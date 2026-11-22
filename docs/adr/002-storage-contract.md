# ADR 002: App-owned SQLite v1 contract

Status: **accepted for #3 via merged PR #104; not runtime storage acceptance**.
Depends on accepted ADR 001 / #1. Executable artifact:
[`001_initial.sql`](../../app/internal/store/schema/001_initial.sql).

## Scope, reuse and tradeoffs

The app owns local state; the engine owns auth/provider operations and never opens
this database. Reuse `profile.DefaultRoot` and `model.BookID.Validate` / `Book.Validate`
at the future typed repository boundary. The prototype `storage.go` source was
reviewed (no runtime files accessed): retain per-book resume/speed, remembered
selection, offline cache and atomic publication intent; do not copy its ASIN-only
keys, float seconds, JSON store or broad partial-file cleanup. No source extracted.

This slice approves executable SQL, not #9's opener, CRUD, driver, upgrade runner,
UI integration or release linkage. Python sqlite3 is a **test oracle only**, not a
production adapter/module. #9 must choose a production Go driver separately. SQLite
>=3.38 is the accepted contract floor (STRICT + built-in JSON); runtime builds must
verify it. SQL statement-coverage tooling is unavailable/N/A: execute the actual
artifact and relational/transaction tests instead; existing independent >=95%
Go/adapter/tooling statement gates remain unchanged.

## Identity and metadata

`book_identity` assigns an internal integer key to a unique, case-sensitive
(provider, namespace, id) tuple. The integer is never an external BookID. SQL checks
ASCII syntax including Audible ASIN shape, not entitlement or provider support.
Local imports may use e.g. (`local`, `user`, `record-1`), never a fabricated ASIN.
Retain identity rows when removing catalog/media; RESTRICT prevents accidental
history/favorite/bookmark/job/selection deletion. Explicit user-data erasure is a
separate confirmed operation, not refresh cleanup. Surrogate keys reduce repeated
composite FKs; all repository APIs must still accept/return structured BookID.

`catalog_book` is the current cached provider metadata only. NULL means unknown;
empty subtitle, JSON `[]` contributors and duration 0 are known values. JSON arrays
store ordered `{"name":"..."}` contributors from the accepted model; SQL checks
array shape, **typed code must validate members, UTF-8/control safety and required
nonblank names/title**. No raw provider bodies, credentials, customer IDs, licenses,
URLs (including artwork), keys or arbitrary error strings enter SQL. No speculative
provider/capability/chapter cache tables: future accepted model additions require
an explicit migration. Retained identity/history can outlive display metadata;
UI must support an unavailable-title fallback, not silently delete local records.

## Units and independent local state

All duration/position fields are signed SQLite INTEGER **nanoseconds**, 0 through
9223372036854775807 (Go time.Duration range), not bridge milliseconds or floating
seconds. Position may exceed unknown/stale catalog duration; playback clamps only
against validated loaded media, not refresh metadata. UTC Unix timestamps are
integer **milliseconds**, same nonnegative integer range; typed conversions must
check their destination range. `speed_millionths` is a positive integer fixed-point
multiplier (1000000 = 1x), avoiding NaN/infinity. Typed playback rejects unsupported
speeds without rounding overflow. Completion is an explicit unfinished/finished
enum, not inferred from position/EOF alone; policies belong to playback services.

Listening state is the latest per-book checkpoint, not a speculative session-event
log. Favorites are row-presence local state (#42), independent of history, bookmarks,
completion and downloaded media; no cloud sync. Bookmarks have stable app-generated
IDs, frozen absolute jump position, optional chapter context, nonblank note, creation
and update timestamps; same-position notes are allowed. Apply configured pre-roll
once at capture (`max(0, captured - pre_roll)`), never again on jump, per B1.
SQL rejects ASCII-whitespace-only notes; typed code must reject all Unicode
whitespace-only notes with strings.TrimSpace and sanitize multiline text. Chapter context is a hint
that survives provider chapter changes, not a foreign key into a mutable catalog.

`local_media` inventories validated published media only, using a unique root-relative
`downloads/...` path; SQL's coarse path check is **not path confinement**. Runtime
must anchor/no-follow/validate path components and actual files. Deletion removes
only media inventory/file, never identity/local state. Job Ready is historical,
not proof media still exists. Jobs retain queue order, fixed state, nonnegative
byte samples and optional total (NULL unknown, 0 known). One live job per book;
terminal attempts can coexist. Only waiting rows have a queue order; active and
terminal rows have NULL. A unique partial index permits only one globally active
job across licensing/transfer/preparation/publication, per D1. Queue reorder needs
a transaction with unused intermediate order values. Failure codes are a deliberately small persistent
allowlist, not engine free-text errors. Typed code maps other failures to `internal`
and handles retry policy explicitly. Restart reconciles active jobs with validated
published media; otherwise mark failed/interrupted and require explicit retry.
A stale engine event must match the current attempt/operation before persistence.
No SQL trigger pretends to implement the job state machine or media publication.

`view_state` remembers destination, selected identity (not audio), focus, filter and
sort. No autoplay on restore. `settings` contains only glyph mode and volume.
Singleton rows are optional until first save, not fabricated user preferences.
Sort enum title/author/duration/recent/acquisition follows the accepted key map;
acquisition sorting remains unavailable without the corresponding model metadata.
Glyph unicode/plain maps to the actual shell's modes; no automatic font detection
or currently unimplemented Nerd mode is inferred. Theme selection/palettes stay in
config.toml/colors.toml per the accepted theming contract, not duplicated in SQL.
**Accepted choice (PR #104):** nanoseconds/fixed-point speed representation. Volume
0–100 (no boost) follows the accepted key map. Incompatible changes to these
representations require reviewed contract, SQL and test updates together.
More SQL settings need reviewed typed columns/migrations, never a
credential-capable opaque key/value bucket.

## Transactions, initialization and upgrades

Only a complete validated `LibrarySnapshot` after clean engine success may be
published. In one transaction: upsert identities/metadata for that provider and
namespace, delete its absent catalog rows, update `library_snapshot.refreshed_at_ms`,
commit. Validate/deduplicate the entire snapshot before the transaction. An empty
successful snapshot removes that scope's catalog only. Provider/page/auth failure
or stale operation does **not start publication**; DB failure rolls back all changes.
Local state and other providers/marketplaces are never replaced. Snapshot completeness
and marker/row agreement are service invariants, not magically enforced by DDL.

The source artifact owns BEGIN IMMEDIATE/COMMIT and applies v0→v1 to an empty
Taletty database only. It enables FKs outside the transaction. Its fresh-database
guard rejects nonzero application_id/user_version or existing user objects, before
DDL/markers. Caller **must rollback on any script error** (SQLite statement failure
does not automatically roll back the transaction). It is intentionally not a
rerunnable migration; no IF NOT EXISTS that hides malformed schemas. Idempotent
runtime initialization means inspect and validate an existing v1 DB, then do
nothing. Tests prove repeat application refuses without altering existing data,
and prove rollback of a partially executed migration. They do not implement that
runtime dispatch.

Markers: application_id 1413565524 (`TALT`), user_version 1. #9 must inspect both
before executing SQL, refuse foreign, too-new, unversioned nonempty or corrupt
state without overwriting, deletion, downgrade or automatic reset. Validate expected
schema/integrity/FKs with sanitized recoverable diagnostics. Do not claim the SQL
guard detects a forged v1 schema. For future Taletty-only upgrades, require explicit
ordered version steps and fixtures, a private consistent SQLite backup (not a live
DB-file copy), transactional DDL/data/marker publication and rollback on error.
Retain original/backup and report uncertainty after I/O failure; no claim all
migrations, arbitrary crashes or disk-full are solved by v1 DDL. Restore/recovery
must never sweep unknown files. No prototype discovery/import/reuse, even though
#9's old issue text mentions a separate legacy import: that is not authorization.

## Runtime ownership/security obligations (#9/#10)

All config/cache/database/history/auth and downloads stay in fresh
`~/.config/taletty/` independent of XDG; database proposed name `taletty.sqlite3`,
audio in `downloads/`. Installation performs no login/registration. Engine auth
writes remain behind its existing root/session gate; SQL stores no auth references.

Runtime must create/verify same-UID 0700 root/directories and 0600 single-link regular
DB/backup/sidecar files; reject unsafe existing ownership, symlinks, hardlinks and
nonregular leaves without chmod-based repair or clobber. Anchor root access across
renames, coordinate app/root ownership with the engine's existing advisory gate
(no recursive acquisition/deadlock), and account for SQLite path-based reopening.
Use private creation/umask before connecting; checking DB mode afterward is too late.
#9 must decide and test its driver's locking, busy timeout/cancellation and journal
mode. WAL, if selected, requires equally private owned `-wal`/`-shm`, safe reopen,
checkpoint/backup handling and no deletion of another live connection's sidecars.
Never assume DB-file chmod protects sidecars. No journal mode is enabled by this
artifact. Enable/verify foreign_keys on **every** runtime connection. Serialize
writes, bound waits, sanitize errors and retain recoverable drafts on failure.
Permissions are OS/runtime obligations, **not properties enforced/tested by SQL**.
Data is not encrypted at rest; recommend device/disk encryption.

## Evidence and deferred journeys

`python3 tests/storage/test_schema.py` executes source SQL in real temporary SQLite
files, prints SQLite version, refuses zero tests/skips, checks constraints, identity
collisions, optional values, independent state, complete snapshot rollback, reopen,
markers and migration failure rollback. Mandatory Coverage tooling CI step and
workflow regression protect execution. No coverage percentage is manufactured for
SQL or this test-only oracle. This is partial service-contract evidence toward
J02/J09/J14/J17, **not** completed UI journeys, app installation, OS permission tests,
disk-full/crash proof or production upgrades. #9/#10/#11/#13/#42 keep their native
blockers and runtime acceptance tests; #3 approval does not accept runtime storage.

## Borrowed Linux root foundation (#106 under #9)

`store.WithRoot` consumes an already-owned directory descriptor, verifies exact
same-UID 0700 directory/0600 single-link regular database and journal/WAL/SHM
leaves using no-follow metadata inspection, and lends a separate CLOEXEC directory
lease. Missing leaves are allowed; no files are created, repaired or removed.
Unknown files are not inspected. Descriptor-relative access stays anchored across
root rename. The caller must retain exclusive cooperative ownership throughout
and join all users before return. Engine `Session.WithRoot` already lends its root
inside the operation gate while holding `.auth.lock` for the session lifetime;
store must not recursively acquire that lock or reenter the engine gate. This
primitive does not import or integrate the independently unpublished engine.
The session-lifetime root owner is distinct from the authenticated operation gate:
#107 must resolve public lease/connection ownership without monopolizing that gate
for a database lifetime or blocking local checkpoints behind licensed transfers.

This is preflight, not connection safety or schema validation. #107 must consume
the anchored lease and resolve the driver's SQLite path-reopening, journal/sidecar
creation and locking semantics before opening anything. A successful metadata
check alone does not authorize a path-based connection. #108 owns subsequent
typed repositories; #9 and downstream runtime blockers remain open.

## Private Linux SQLite transport (#114 under #107)

#107 was split before implementation into native dependency-linked children #114
(transport) → #115 (validated opener and public owner lifetime). This first slice
pins `github.com/ncruces/go-sqlite3 v0.32.0`, a supported CGO-free SQLite/wazero
wrapper with public writable Go VFS hooks and default Linux OFD locks. The cached
`modernc.org/sqlite v1.56.0` was also inspected: its public fs.FS VFS is read-only;
its ordinary Unix VFS canonicalizes paths. The ncruces default VFS also cannot be
used unwrapped: it canonicalizes symlinks, creates at 0666 and separately opens SHM.
See [the transport review](../../app/internal/store/README.md) for the inspected
source behavior, licensing, build/filesystem constraints and real-test evidence.

The package-private adapter retains the borrowed directory lease, preserves a
fixed-leaf path through `/proc/self/fd/<directory-fd>/` (never a DB-file fd path),
precreates at 0600 with openat/exclusive/no-follow, validates before driver reopen,
removes driver CREATE flags and restricts every path to the DB/rollback journal.
It reuses upstream IO/OFD locks and root validation; directory sync covers private
creation/journal publication/deletion. Only version-1 file IO is exposed: WAL is
not supported and any existing WAL/SHM is refused/preserved. Temp storage is memory
only. Every connection enables/verifies foreign_keys, uses caller cancellation and
a 100ms busy timeout, then joins synchronous work, finalizes statements and closes
before unregistering the VFS/releasing the directory. No unknown-file sweep,
chmod repair, recursive .auth.lock, engine import or authenticated-gate ownership.
Cooperative ownership excludes name mutation, not a hostile same-UID attacker.

**Transport alone is not a validated app opener:** SQLite can recover a hot
journal on access. It remains unexported and unused by app startup. #115 adds the
following dispatcher; transport evidence is not public engine integration,
#107/#9 acceptance or a full J14.

## Validated Linux v1 opener and external owner (#115)

`store.Open(ctx, RootOwner)` validates before opening writable SQLite. Missing or
zero-length DBs, or valid unversioned SQLite files with no schema objects, may
initialize using the exact embedded artifact. Every initialization script error
explicitly rolls back, including canceled work. Existing v1 requires exact full
sqlite_schema agreement with an in-memory execution of that same artifact,
markers, SQLite >=3.38, full integrity_check and zero foreign_key_check rows.
Rewritten-but-equivalent schema SQL is deliberately refused; no permissive forged
schema matching. Every connection enables/verifies foreign_keys. Check is a
non-writing health operation; typed SQL remains private to the store package.

Before ANY SQLite access, inspect private leaves and raw header/markers through
the anchored directory with O_NOATIME. Refuse malformed file/page counts, foreign,
nonempty unversioned, corrupt and too-new state. Refuse/preserve **every existing
rollback journal, WAL and SHM**, including zero/cold journals: markers cannot prove
a journal's provenance. No automatic recovery, deletion/reset, backup, upgrade,
unknown-file cleanup or import. A real abrupt-exit hot-journal test proves public
refusal/preservation; recovery requires future explicit tooling. Crash availability
is intentionally limited rather than recovering unknown files speculatively.

The inspection VFS forbids all write/truncate/create/delete/sidecar opens, reads
through O_NOATIME and retains upstream OFD locking. It uses SQLite `mode=rw` with
no CREATE, not `mode=ro`: real SQLite 3.51.3 regression tests demonstrate that
readonly integrity_check skips CHECK constraints. The VFS is the write barrier;
full schema matching precedes integrity expression evaluation. Refusal tests
compare all bytes and stat metadata, including atime/ctime. Failed no-atime access
fails closed. This remains cooperative ownership, not malicious same-UID defense.

`NewOwner` takes an already externally owned root and a nonblocking cleanup
callback, duplicates the anchored descriptor, and provides public RootOwner/lease
capabilities independently of the caller's authenticated-operation gate. Transfer
occurs on success only. ALL external/auth users must retain leases and honor their
cancellation. Close stops admission, cancels/joins leases before closing its own
descriptor and invoking external cleanup exactly once. Canceled waits preserve
ownership until retry; cleanup must not block/reenter. Store independently
serializes/cancels/joins local connection work and Close does not wait on an auth
transfer. No raw DB handle escapes, no recursive .auth.lock, no engine imports.
The current engine public Client has no public root-lifetime lease: this contract
is an external handoff foundation, not engine wiring. Future public integration
must arrange all engine users under lifetime leases or equivalent capability,
never retain the private gated WithRoot callback or monopolize auth for DB lifetime.

`store.Initialize()` explicitly exposes upstream one-time synchronous Wasm startup
before interactive admission; compilation itself is not context-interruptible.
Open initializes implicitly if necessary. Query/busy waits and local/owner shutdown
are cancelable after that boundary. See the store README for tested limits,
permissions, supported build/filesystem and constrained-host cache instructions.
No startup/bootstrap/UI persistence, repositories (#108), release linkage, general
crash/disk-full proof, full J14 or parent closure is claimed by this slice.

## Typed local-state repositories (#108 under #9)

`app/internal/store` implements the typed repository boundary over `Store.withConn`
and the accepted `001_initial.sql` schema (`catalog_linux.go`, `state_linux.go`,
`jobs_media_linux.go`) without leaking `*sqlite3.Conn` or raw SQLite diagnostics.
Inputs are validated before starting transactions (`ErrInvalidInput` and fixed
conflict sentinels `ErrActiveJobExists`, `ErrDownloadInProgress`,
`ErrQueueOrderConflict`, `ErrMediaPathConflict`), and `withTx` rolls back on any
error, cancellation or panic.

- `ReplaceCatalog` atomically replaces `catalog_book` for a single validated
  `(provider, namespace)` and updates `library_snapshot.refreshed_at_ms`,
  preserving `book_identity` keys and all local state (`listening_state`,
  `favorite`, `bookmark`, `local_media`, `download_job`, `view_state`).
- `ListeningState`, `Settings`, `ViewState`, `SetFavorite`/`Favorites`, and
  `Bookmark` CRUD preserve nullable unknown vs known empty/zero semantics,
  singleton defaults (`unicode`/`100`, `all`/`books`/`title`), and multiline note
  safety.
- `LocalMedia` enforces `downloads/...` `.m4b` relative path syntax and path
  uniqueness; `DownloadJob` enforces live-per-book, single-active-slot and
  queue-order uniqueness, `NormalizeFailureCode` fallback to `internal`,
  two-phase `ReorderDownloadQueue` with nonnegative intermediate orders, and
  startup `ReconcileActiveJobs` (marking active jobs `ready` only when confirmed
  published, otherwise `failed`/`interrupted`). No app startup wiring, engine
  integration or UI persistence wiring is claimed by `#108`.

## Fresh private profile root bootstrap (#10)

`app/internal/profile` (`paths.go`, `bootstrap_linux.go`) implements `Open(ctx, home, nowMS)`
and `Workspace` over `DefaultRoot`, `store.NewOwner`, `store.Open`, and
`Store.LocalMediaList`/`Store.ReconcileActiveJobs` without importing `engine`.

- Validates existing `~/.config` as a real `os.Geteuid()`-owned directory
  (`O_NOFOLLOW`, without modifying its permissions) or creates it at `0700`,
  then creates/verifies `~/.config/taletty/` (`0700`), `downloads/` (`0700`),
  and `.auth.lock` (`0600`, `Nlink == 1`, `(Dev, Ino)` match, nonblocking
  `LOCK_EX` -> `ErrBusy`) via anchored `O_NOFOLLOW`/`O_CLOEXEC` descriptors
  without `chmod` repair.
- Ignores `XDG_CONFIG_HOME`, never scans/reads/modifies `~/.config/audible-tui/`,
  performs no login or device registration, and preserves existing `auth.json`
  (`0600`) and all Taletty-owned repository data across restarts.
- Reconciles interrupted active download jobs against validated private files
  under `downloads/` (explicitly rejecting empty, `.`, `..`, NUL, or backslash
  segments, symlinks, hardlinks, nonregular files, and unsafe modes/owners).
- `Workspace.Close(ctx)` closes `Store` before `Owner` without unlocking
  `.auth.lock` underneath active operations or leases if `ctx` expires.
- Limits: Linux-only bootstrap foundation; no `cmd/taletty` CLI/UI startup
  wiring, engine `Client` integration, or Taletty v2 schema migration yet. See
  [the profile README](../../app/internal/profile/README.md).
