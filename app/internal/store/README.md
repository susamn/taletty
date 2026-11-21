# Linux SQLite transport: #114, first slice of #107

This is **not an application-facing validated opener**. `withSQLite` is package
private, not used by startup, and must remain behind the future #115 dispatcher.
SQLite may recover a hot journal on access. Consequently this transport alone
cannot promise preservation of foreign/corrupt/unversioned/too-new state. #115
must authorize markers/header/sidecars **before** recovery, embed the actual schema,
validate exact v1 schema/integrity/FK rows, and define public owner/join/Close.
#107 remains open; typed repositories remain #108. No bootstrap, upgrades, backup,
engine integration, UI persistence or full J14 acceptance is claimed.

## Reviewed driver and filesystem boundary

Pinned `github.com/ncruces/go-sqlite3 v0.32.0` is a CGO-free Go/wazero wrapper of
embedded SQLite. The actual artifact reports **SQLite 3.51.3** in tests (its embed
README still says 3.51.2); tests execute the exact merged `schema/001_initial.sql`
via `go:embed`. No v1 DDL was copied into Go. Runtime schema dispatch is deferred.
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
This code does not take `.auth.lock` recursively, discover paths or import engine
internals. That owner is distinct from the authenticated per-operation gate;
never call a DB-lifetime callback from inside that gate, or block checkpoints
behind a licensed transfer. Obtaining a public engine-compatible lifetime lease
without holding its operation gate is **still #115 work**, not solved here.

Each invocation owns its connection and unique VFS registration. All operations
and statements finish synchronously before return; no handle/root escape,
concurrent use of one connection, blob or backup handles. Forgotten statements
are finalized (reported as failure), then Close rolls back unfinished transactions,
releases locks, unregisters the VFS and releases only our root lease, including on
panic. Callback errors retain their meaning; setup/OS errors are sanitized.
Every connection enables and verifies `foreign_keys=ON`, sets temp storage to
memory, installs caller cancellation and a 100ms busy timeout. The future public
API must sanitize operation errors and enforce serialized access. Initial Wasm
compilation is upstream one-time process setup, not context-interruptible query
work; #115 must account for that startup boundary explicitly.

## Evidence and running tests on constrained hosts

Real SQLite tests cover source SQL, create/reopen/root rename, private journal,
WAL refusal, foreign-key enforcement, active transaction rollback on error/panic/
cancel/forgotten statements, partial fresh migration rollback/retry, independent
connections and real-process busy waits, cancellation at the contention barrier,
late journal planting, unknown-file preservation, and external advisory ownership.
The existing `WithRoot` adversarial matrix supplies all fixed-leaf mode/owner/link/
nonregular checks; owner mismatch uses the existing stat-boundary injection.
None of this demonstrates the still-deferred version/refusal dispatcher.

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
