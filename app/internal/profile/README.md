# Private profile root bootstrap (#10)

`app/internal/profile` resolves and initializes Taletty's fresh private data root
under `~/.config/taletty/` without importing or touching legacy applications.

## Contract and lifecycle

- `DefaultRoot(home)` resolves `<home>/.config/taletty` read-only, rejecting
  relative or NUL-containing inputs (`ErrInvalidHome`) and ignoring
  `XDG_CONFIG_HOME`.
- `Open(ctx, home, nowMS)` initializes and locks the workspace for a session:
  1. Validates `home` and `nowMS >= 0` (`store.ErrInvalidInput`).
  2. Creates `~/.config` (`0700`) if absent, or validates an existing `~/.config`
     as a real `os.Geteuid()`-owned directory (`O_NOFOLLOW`, without modifying
     its permissions).
  3. Creates/verifies `~/.config/taletty/` (`0700`), `downloads/` (`0700`), and
     `.auth.lock` (`0600`, `Nlink == 1`, `(Dev, Ino)` match, nonblocking
     `LOCK_EX` -> `ErrBusy`) using anchored `O_NOFOLLOW`/`O_CLOEXEC`
     descriptors without `chmod` repair (`ErrUnsafeRoot` on unsafe state).
  4. Transfers root ownership to `store.NewOwner`, opens the validated v1 SQLite
     store (`store.Open`), inspects `local_media` paths under `downloads/`
     (rejecting empty, `.`, `..`, NUL, or backslash segments, symlinks,
     hardlinks, nonregular files, and unsafe modes/owners), and reconciles
     interrupted active download jobs (`JobReady` when a private file exists,
     `JobFailed`/`FailureInterrupted` otherwise).
- Unknown files such as `auth.json` (`0600`) are never inspected, read, or
  modified; bootstrap performs no login or device registration.
- `Workspace.Close(ctx)` drains and closes `Store` before `Owner` (releasing
  `.auth.lock` only after all leases join). Canceled waits preserve ownership
  for retry; repeated and concurrent `Close` calls are safe.

## Evidence and limits

[`paths_test.go`](paths_test.go) and [`bootstrap_linux_test.go`](bootstrap_linux_test.go)
cover fresh launch, untouched seeded `~/.config/audible-tui/` and
`XDG_CONFIG_HOME`, `auth.json` preservation, full repository persistence across
restart, active job reconciliation, real subprocess `ErrBusy` exclusion and
post-`Close` reopen, FD noninheritance across `exec`, `Close` cancel/retry,
adversarial no-repair refusal, and injected `sysOps` I/O faults.

Limits: Linux-only bootstrap (`bootstrap_linux.go`); cooperative same-UID
ownership (not defense against a malicious same-UID mutator); no `cmd/taletty`
CLI/UI startup wiring or Taletty v2 schema migration yet; data is not encrypted
at rest (device/disk encryption is recommended). See
[ADR 002](../../../docs/adr/002-storage-contract.md) and
[the test strategy](../../../docs/testing.md).
