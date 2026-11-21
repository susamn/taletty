# Linux validated SQLite store: #114 transport + #115 opener/owner

`Open(ctx, RootOwner)` is the application-facing v1 dispatcher; `Store.Check`
validates without writing, and `Store.Close` cancels/joins local work. Raw SQLite
handles remain package-private for future typed repositories (#108), never UI.
Startup is not wired here. #107 remains open for maintainer acceptance. No root
bootstrap, upgrades, backup/restore, engine integration, UI persistence or full
J14 acceptance is claimed.

Before SQLite access the dispatcher preflights private anchored files, refuses
**every existing journal/WAL/SHM**, then reads the DB header with O_NOATIME. It
checks rollback-mode header layout, complete file/page counts and markers. Missing
and zero-length databases may initialize; unversioned SQLite databases must have
no schema objects. Foreign/nonempty unversioned/corrupt/too-new/forged state is
refused without byte, inode, permission, link, atime, mtime or ctime repair.
Unknown files are untouched. No automatic cleanup/reset or old-app discovery.

An inspection VFS denies every write/truncate, creation, sidecar open and delete;
all byte reads use O_NOATIME while upstream OFD locks remain intact. SQLite is
opened with `mode=rw` (no create), NOT `mode=ro`: real regression tests found
SQLite 3.51.3's integrity_check skips CHECK constraints on a readonly connection.
The VFS enforces the write ban, rather than relying on that SQLite flag. Failed
O_NOATIME access fails closed; there is no fallback that changes access times.
Full sqlite_schema rows (type/name/table/SQL, including autoindexes) must exactly
match an in-memory execution of the embedded source artifact. Semantically similar
but rewritten schemas, extra triggers/indexes/tables and changed FKs/CHECKs are
rejected. Full integrity_check and foreign_key_check follow schema comparison,
so unknown schema expressions are not evaluated. Runtime SQLite >=3.38 and both
markers are verified; foreign_keys is enabled/verified on every connection.
Fresh initialization executes the exact embedded SQL and explicitly rolls back
any script failure, including after cancellation. No generic migration runner.

## Recovery policy

Markers cannot authenticate a crash journal's provenance. v1 therefore refuses
all journals, including empty/cold ones, **before SQLite can recover, delete or
mutate anything**. A real abruptly exited child produces a hot journal in tests;
the public opener preserves it byte-for-byte and metadata-for-metadata. Only the
test explicitly authorizes the private transport to prove rollback recovery of
that fixture. Production offers no recovery/reset command yet. Preserve root and
seek explicit recovery tooling; do not manually delete journals to force startup.
This trades crash availability for preservation, not a claim of general crash,
disk-full, backup or durability recovery. Low-level `withSQLite` alone is unsafe
for untrusted existing state and must never bypass the public dispatcher.

## Reviewed driver and filesystem boundary

Pinned `github.com/ncruces/go-sqlite3 v0.32.0` is a CGO-free Go/wazero wrapper of
embedded SQLite. The actual artifact reports **SQLite 3.51.3** in tests (its embed
README still says 3.51.2); production embeds the exact merged
`schema/001_initial.sql` via `go:embed`. No v1 DDL was copied into Go.
The dependency is MIT (Nuno Cruces); wazero v1.11.0 is Apache-2.0. Dependencies
remain modules with their upstream licenses, not copied/relicensed source.

Both this version and cached `modernc.org/sqlite v1.56.0` support Linux without
CGO. Source review found modernc's public `vfs.New(fs.FS)` is read-only; its normal
Unix VFS canonicalizes pathnames and uses POSIX locks. A writable anchored adapter
would require lower-level translated-C hooks. ncruces exposes Go writable VFS
interfaces and default Linux OFD byte-range locking (Linux >=3.15), compatible
with standard Unix SQLite locks. We reuse its file IO/locking rather than copy a
locking algorithm. Alternate `sqlite3_flock`/`sqlite3_dotlk` tags are **unsupported**:
they change locking compatibility; the supported/tested build uses neither.
Use a local filesystem supporting those locks and directory fsync, not network FS.

The unwrapped ncruces VFS is also unsuitable: `FullPathname` resolves symlinks,
file creation requests 0666, and SHM opens bypass ordinary VFS `OpenFilename`.
The adapter therefore:

- Retains a CLOEXEC directory lease from `WithRoot` until all connection work ends.
  `FullPathname` preserves `/proc/self/fd/<DIRECTORY-fd>/taletty.sqlite3`; the final
  component is always a real fixed leaf, **never a database-file fd pseudo-path**.
  Journals and driver reopens resolve through the same anchored directory, even
  after root rename/replacement. Linux `/proc` availability is required.
- Allows only the fixed main DB and rollback journal; no ATTACH path, unnamed
  disk temp or super-journal. Creates with descriptor-relative O_EXCL/O_NOFOLLOW
  and 0600, verifies ownership/type/link count/exact mode before driver reopen,
  and removes CREATE from driver flags. No global umask change, post-open
  permission repair, or permission check after handing the file to SQLite.
- Reuses no-follow inspection at every access/open/delete boundary. Only SQLite's
  fixed validated rollback journal can be deleted, with requested directory
  sync. New-file and journal sync include root-directory sync. Unknown files are
  neither inspected nor swept. Under cooperative ownership, no other writer
  mutates names between inspection and driver reopen. This is **not** containment
  against a malicious same-UID process.
- Exposes version-1 file IO only, without upstream SharedMemory or Unwrap. WAL
  cannot be enabled. Existing WAL/SHM leaves, even private ones, are refused and
  preserved, not checkpointed or deleted. DELETE journaling and memory-only temp
  storage are the supported mode, not an unreviewed WAL implementation.

## Ownership, cancellation and recovery

The caller must already retain the **session-lifetime advisory root owner**.
`NewOwner(ctx, borrowedRoot, release)` transfers cleanup responsibility only on
success, retains its own anchored CLOEXEC directory, and implements `RootOwner`.
ALL external/auth users must take `Owner.Lease` and honor its Context, independently
of the caller's separate auth-operation gate. RootLease.Close is idempotent;
join every derived user first. Owner.Close permanently stops admission, cancels
leases and waits context-aware for them to join, then closes its directory and
calls the nonblocking external release exactly once. A canceled wait retains
ownership; retry Close. Release must not block/reenter: joining belongs to leases.
Store.Close cancels/joins only local work and never waits for an unrelated auth
lease. Close Store before Owner; no reentrant Close inside its own work/lease.

No `.auth.lock` acquisition, engine import or root discovery occurs. Integration
can wrap its lifetime owner with this capability; the currently merged engine
Client does NOT expose a public root lifetime lease and its private WithRoot is
inside the auth gate. Thus this is a usable external owner/handoff contract,
**not current engine integration**. Never retain a root from that private callback
or hold the auth gate for DB lifetime. Future public engine wiring must arrange
all auth users under lifetime leases, or provide an equivalent RootOwner adapter.

Each invocation owns its connection and unique VFS registration. All operations
and statements finish synchronously before return; no handle/root escape,
concurrent use of one connection, blob or backup handles. Forgotten statements
are finalized (reported as failure), then Close rolls back unfinished transactions,
releases locks, unregisters the VFS and releases only our root lease, including on
panic. Callback errors retain their meaning; setup/OS errors are sanitized.
Every connection enables and verifies `foreign_keys=ON`, sets temp storage to
memory, installs caller cancellation and a 100ms busy timeout. The public Store sanitizes
operation errors and serializes access with a cancellation-aware local gate.
`Initialize()` is explicit one-time synchronous process startup, before interactive
work: upstream Wasm compilation cannot be canceled. Open initializes implicitly
if needed; do not promise a cold Open deadline interrupts the compiler. SQLite
queries/busy waits and lease/store shutdown are cancelable after that boundary.

## Evidence and running tests on constrained hosts

Real SQLite tests cover source SQL, create/reopen/root rename, private journal,
WAL refusal, foreign-key enforcement, active transaction rollback on error/panic/
cancel/forgotten statements, partial/canceled fresh migration rollback/retry,
independent connections and real-process busy waits, cancellation at the
contention barrier, late journal planting, unknown-file preservation, and
external advisory ownership. #115 adds [`validated_linux_test.go`](validated_linux_test.go)
and [`validation_faults_linux_test.go`](validation_faults_linux_test.go): fresh/zero-byte/
empty-unversioned initialization, 65536-byte page headers, byte-and-metadata
(`O_NOATIME` atime/ctime/inode/link) refusal of foreign/nonempty-unversioned/
corrupt/too-new/forged-schema/FK/CHECK-violating databases, SQLite 3.51.3
`mode=rw` + VFS write-ban enforcement, abrupt-exit hot-journal refusal and
preservation, `Store.Check`/`Close` races, and `Owner`/`RootLease` independence
from `.auth.lock`. The existing `WithRoot` adversarial matrix supplies all
fixed-leaf mode/owner/link/nonregular checks; owner mismatch uses the existing
stat-boundary injection. No #108 repositories, #10 bootstrap, engine integration,
or full J14 acceptance is claimed.

Tests use the real default JIT runtime, including under `-race`, and a private
compilation cache shared with owned child fixtures. On a one-core ~1GiB host,
cold race-instrumented **compiler** setup exceeded 60s (interpreter was slower).
No tests or production sources are excluded and initialization is **inside** the
Go test deadline. Before the normal race command on such hosts, prewarm the same
explicit private cache with ordinary tests:

```sh
cache=$(mktemp -d)
export TALETTY_SQLITE_TEST_CACHE="$cache"
(cd app && GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=1 go test -count=1 -timeout=60s ./internal/store)
# Now run the normal all-source race/coverage command with this environment.
# Remove the owned cache after all tests/child processes finish:
rm -rf -- "$cache"
```

Without that environment, tests create/remove their own private temporary cache.
A warmed compiler cache does not cache query results, bypass VFS work or disable
race instrumentation of Go file/locking/lifecycle code. Cold race startup on
this constrained host has **not** passed the 60s budget; do not imply otherwise.

## Typed local-state repositories (#108)

All repository methods run synchronously over `Store.withConn` inside `withTx`,
rolling back explicitly (`c.SetInterrupt(context.Background()); c.Exec("ROLLBACK")`)
on any error, panic or cancellation without exposing `*sqlite3.Conn` or raw SQLite
error text outside `package store`. All inputs are validated before starting a
transaction (`ErrInvalidInput` and explicit conflict sentinels):

- **Identity & scoped catalog (`catalog_linux.go`)**: `BookKey` syntax checks
  match `001_initial.sql` and `model.BookID`. `ReplaceCatalog` atomically
  replaces `catalog_book` for a single `(provider, namespace)` and updates
  `library_snapshot.refreshed_at_ms` while preserving `book_identity` rows and
  all independent local state; `Catalog` and `SnapshotRefreshedAt` query cached
  rows with unknown (`nil`) vs known empty/zero fields preserved.
- **Playback & UI state (`state_linux.go`)**: `PutListeningState`/`ListeningState`,
  `PutSettings`/`Settings` (defaults `unicode`, `100`), `PutViewState`/`ViewState`
  (defaults `all`, `books`, `title`, optional `Selected`), `SetFavorite`/`Favorites`,
  and `PutBookmark`/`DeleteBookmark`/`Bookmarks` (multiline safe notes, cross-book
  ID guard).
- **Local media & download jobs (`jobs_media_linux.go`)**: `LocalMedia` path-shape
  confinement (`downloads/...` `.m4b` only, `ErrMediaPathConflict` on duplicate
  path across books), `DownloadJob` uniqueness/state checks (`ErrActiveJobExists`,
  `ErrDownloadInProgress`, `ErrQueueOrderConflict`), `NormalizeFailureCode`,
  two-phase `ReorderDownloadQueue` using unused nonnegative intermediate order
  values, and `ReconcileActiveJobs` for startup recovery of interrupted jobs.
