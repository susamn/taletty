// Package store owns application-local persistence, never provider authentication.
package store

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

var (
	ErrUnsafeRoot = errors.New("store root must be a private owned directory")
	ErrUnsafeFile = errors.New("store database files must be private owned single-link regular files")
	ErrIO         = errors.New("store root inspection failed")
)

// WithRoot lends an anchored CLOEXEC directory descriptor for synchronous store
// work. The caller MUST already hold exclusive cooperative root ownership (the
// engine's Session.WithRoot gate or its eventual app equivalent) until return.
// This function never acquires .auth.lock: recursively acquiring it would conflict
// with the existing owner. No engine dependency or integration is implied here.
//
// Existing database, rollback journal and WAL/SHM leaves are inspected without
// following links or opening devices. Missing leaves are allowed; nothing is
// created, repaired or removed. Unknown files, including auth, are not inspected.
// This is a preflight capability, NOT a SQLite opener: a future driver must also
// solve private creation, path reopening and sidecar anchoring before connecting.
// Cooperative ownership cannot protect against a malicious same-UID mutator.
//
// The callback must honor ctx, join all users before returning, and never close or
// retain the descriptor. Error/panic preserves caller meaning and closes only our
// lease, not borrowed. Cancellation is checked before inspection and callback;
// arbitrary callback/kernel work cannot be interrupted by this function.
func WithRoot(ctx context.Context, borrowed *os.File, operation func(context.Context, *os.File) error) error {
	return withRoot(ctx, borrowed, operation, unix.Fstatat)
}

// Inspect is injected only at the descriptor security boundary for fault tests.
func withRoot(ctx context.Context, borrowed *os.File, operation func(context.Context, *os.File) error,
	inspect func(int, string, *unix.Stat_t, int) error) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	if borrowed == nil {
		return ErrUnsafeRoot
	}
	fd, err := unix.Openat(int(borrowed.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return ErrIO
	}
	lease := os.NewFile(uintptr(fd), "store-root")
	defer func() {
		if closeErr := lease.Close(); closeErr != nil && err == nil {
			err = ErrIO
		}
	}()
	var stat unix.Stat_t
	if err = inspect(fd, ".", &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return ErrIO
	}
	if stat.Mode&(unix.S_IFMT|07777) != unix.S_IFDIR|0700 || stat.Uid != uint32(os.Geteuid()) {
		return ErrUnsafeRoot
	}
	for _, name := range []string{"taletty.sqlite3", "taletty.sqlite3-journal", "taletty.sqlite3-wal", "taletty.sqlite3-shm"} {
		if err = inspect(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return ErrIO
		}
		if stat.Mode&(unix.S_IFMT|07777) != unix.S_IFREG|0600 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
			return ErrUnsafeFile
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return operation(ctx, lease)
}
