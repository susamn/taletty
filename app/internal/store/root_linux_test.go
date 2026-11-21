package store

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func rootFixture(t *testing.T) (string, *os.File) {
	t.Helper()
	path, err := os.MkdirTemp("", "taletty-store-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(path) })
	must(t, os.Chmod(path, 0700))
	dir, err := os.Open(path)
	must(t, err)
	t.Cleanup(func() { dir.Close() })
	return path, dir
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func unused(context.Context, *os.File) error { return errors.New("unexpected callback") }

func TestRootAnchoringLifetimeAndRecovery(t *testing.T) {
	path, borrowed := rootFixture(t)
	must(t, os.WriteFile(filepath.Join(path, "unknown"), []byte("preserve"), 0644))
	var lease *os.File
	failure := errors.New("callback failure")
	for _, outcome := range []string{"success", "error", "panic"} {
		func() {
			if outcome == "panic" {
				defer func() {
					if recover() != failure {
						t.Error("caller panic changed")
					}
				}()
			}
			err := WithRoot(context.Background(), borrowed, func(_ context.Context, dir *os.File) error {
				lease = dir
				flags, err := unix.FcntlInt(dir.Fd(), unix.F_GETFD, 0)
				must(t, err)
				if flags&unix.FD_CLOEXEC == 0 {
					t.Fatal("lease is inheritable")
				}
				if outcome == "panic" {
					panic(failure)
				}
				if outcome == "error" {
					return failure
				}
				return nil
			})
			if (outcome == "error" && err != failure) || (outcome == "success" && err != nil) {
				t.Fatal(err)
			}
		}()
		if _, err := lease.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatal("lease leaked", err)
		}
		_, err := borrowed.Stat()
		must(t, err)
	}
	moved := path + "-moved"
	must(t, os.Rename(path, moved))
	t.Cleanup(func() { os.RemoveAll(moved) })
	must(t, os.Mkdir(path, 0700))
	// An unsafe planted replacement must neither be used nor repaired.
	must(t, os.Symlink("missing", filepath.Join(path, "taletty.sqlite3")))
	must(t, WithRoot(context.Background(), borrowed, func(_ context.Context, dir *os.File) error {
		fd, err := unix.Openat(int(dir.Fd()), "unknown", unix.O_RDONLY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		return unix.Close(fd)
	}))
	entries, err := os.ReadDir(moved)
	must(t, err)
	if len(entries) != 1 || entries[0].Name() != "unknown" {
		t.Fatal("preflight created or removed files", entries)
	}
	data, err := os.ReadFile(filepath.Join(moved, "unknown"))
	must(t, err)
	if string(data) != "preserve" {
		t.Fatal("unknown data changed")
	}
	// Also detect callback misuse without replacing its primary error.
	for _, primary := range []error{nil, failure} {
		err := WithRoot(context.Background(), borrowed, func(_ context.Context, dir *os.File) error {
			must(t, dir.Close())
			return primary
		})
		if (primary == nil && err != ErrIO) || (primary != nil && err != primary) {
			t.Fatal(err)
		}
	}
}

func TestUnsafeDatabaseLeavesAreNotRepaired(t *testing.T) {
	for _, name := range []string{"taletty.sqlite3", "taletty.sqlite3-journal", "taletty.sqlite3-wal", "taletty.sqlite3-shm"} {
		for _, kind := range []string{"private", "public", "special-mode", "symlink", "dangling", "hardlink", "fifo", "socket", "directory"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				root, dir := rootFixture(t)
				leaf := filepath.Join(root, name)
				outside := filepath.Join(t.TempDir(), "untouched")
				must(t, os.WriteFile(outside, []byte("outside"), 0600))
				switch kind {
				case "private", "public", "special-mode":
					must(t, os.WriteFile(leaf, []byte("unchanged"), 0600))
					if kind == "public" {
						must(t, os.Chmod(leaf, 0644))
					} else if kind == "special-mode" {
						must(t, unix.Chmod(leaf, 04600))
					}
				case "symlink":
					must(t, os.Symlink(outside, leaf))
				case "dangling":
					must(t, os.Symlink("absent", leaf))
				case "hardlink":
					must(t, os.Link(outside, leaf))
				case "fifo":
					must(t, unix.Mkfifo(leaf, 0600))
				case "socket":
					listener, err := net.Listen("unix", leaf)
					must(t, err)
					defer listener.Close()
				case "directory":
					must(t, os.Mkdir(leaf, 0700))
				}
				var before, after unix.Stat_t
				must(t, unix.Lstat(leaf, &before))
				err := WithRoot(context.Background(), dir, func(context.Context, *os.File) error { return nil })
				if (kind == "private" && err != nil) || (kind != "private" && err != ErrUnsafeFile) {
					t.Fatal(err)
				}
				must(t, unix.Lstat(leaf, &after))
				if before != after {
					t.Fatal("leaf metadata changed")
				}
				data, err := os.ReadFile(outside)
				must(t, err)
				if string(data) != "outside" {
					t.Fatal("outside data changed")
				}
			})
		}
	}
}

func TestRootFailuresAndCancellation(t *testing.T) {
	root, borrowed := rootFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WithRoot(ctx, borrowed, unused); err != context.Canceled {
		t.Fatal(err)
	}
	if err := WithRoot(context.Background(), nil, unused); err != ErrUnsafeRoot {
		t.Fatal(err)
	}
	for _, mode := range []uint32{0755, 01700} {
		must(t, unix.Chmod(root, mode))
		if err := WithRoot(context.Background(), borrowed, unused); err != ErrUnsafeRoot {
			t.Fatal(err)
		}
	}
	must(t, unix.Chmod(root, 0700))
	must(t, os.WriteFile(filepath.Join(root, "taletty.sqlite3"), nil, 0600))
	for _, fault := range []string{"root-io", "leaf-io", "root-owner", "leaf-owner", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var fd int
			inspect := func(dir int, name string, stat *unix.Stat_t, flags int) error {
				fd = dir
				err := unix.Fstatat(dir, name, stat, flags)
				switch {
				case fault == "root-io" && name == ".", fault == "leaf-io" && name == "taletty.sqlite3":
					return errors.New("private diagnostic must not escape")
				case fault == "root-owner" && name == ".", fault == "leaf-owner" && name == "taletty.sqlite3":
					stat.Uid++ // Real ownership mutation needs privileges; inject at the stat boundary.
				case fault == "cancel" && name == "taletty.sqlite3-shm":
					cancel()
				}
				return err
			}
			want := map[string]error{"root-io": ErrIO, "leaf-io": ErrIO, "root-owner": ErrUnsafeRoot, "leaf-owner": ErrUnsafeFile, "cancel": context.Canceled}[fault]
			if err := withRoot(ctx, borrowed, unused, inspect); err != want {
				t.Fatal(err)
			}
			var stat unix.Stat_t
			if err := unix.Fstat(fd, &stat); err != unix.EBADF {
				t.Fatal("failed preflight leaked descriptor", err)
			}
		})
	}
	file, err := os.Open(filepath.Join(root, "taletty.sqlite3"))
	must(t, err)
	if err := WithRoot(context.Background(), file, unused); err != ErrIO {
		t.Fatal(err)
	}
	must(t, file.Close())
	if err := WithRoot(context.Background(), file, unused); err != ErrIO {
		t.Fatal(err)
	}
	active, cancelActive := context.WithCancel(context.Background())
	defer cancelActive()
	var activeLease *os.File
	if err := WithRoot(active, borrowed, func(ctx context.Context, dir *os.File) error {
		activeLease = dir
		cancelActive()
		return ctx.Err()
	}); err != context.Canceled {
		t.Fatal("callback did not receive caller cancellation", err)
	}
	if _, err := activeLease.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("active cancellation leaked lease", err)
	}
	must(t, WithRoot(context.Background(), borrowed, func(context.Context, *os.File) error { return nil }))
}

func TestBorrowedOwnershipProcessAndConcurrentReaders(t *testing.T) {
	path, borrowed := rootFixture(t)
	lock, err := os.OpenFile(filepath.Join(path, ".auth.lock"), os.O_CREATE|os.O_RDWR, 0600)
	must(t, err)
	defer lock.Close()
	must(t, unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB))
	must(t, WithRoot(context.Background(), borrowed, func(ctx context.Context, dir *os.File) error {
		// Model the actual external auth lock contract; production must not acquire it.
		childCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestStoreRootChild$")
		var stat unix.Stat_t
		must(t, unix.Fstat(int(dir.Fd()), &stat))
		cmd.Env = append(os.Environ(), "TALETTY_STORE_CHILD="+path, fmt.Sprintf("TALETTY_STORE_INODE=%d:%d", stat.Dev, stat.Ino))
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("child: %w: %s", err, output)
		}
		return nil
	}))
	var readers sync.WaitGroup
	for range 8 {
		readers.Go(func() {
			if err := WithRoot(context.Background(), borrowed, func(context.Context, *os.File) error { return nil }); err != nil {
				t.Error(err)
			}
		})
	}
	readers.Wait() // Caller owns the gate; leases do not claim write serialization.
	must(t, unix.Flock(int(lock.Fd()), unix.LOCK_UN))
	other, err := os.OpenFile(filepath.Join(path, ".auth.lock"), os.O_RDWR, 0)
	must(t, err)
	defer other.Close()
	must(t, unix.Flock(int(other.Fd()), unix.LOCK_EX|unix.LOCK_NB))
}

func TestStoreRootChild(t *testing.T) {
	path := os.Getenv("TALETTY_STORE_CHILD")
	if path == "" {
		return
	}
	entries, err := os.ReadDir("/proc/self/fd")
	must(t, err)
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		must(t, err)
		var stat unix.Stat_t
		if unix.Fstat(fd, &stat) == nil && fmt.Sprintf("%d:%d", stat.Dev, stat.Ino) == os.Getenv("TALETTY_STORE_INODE") {
			t.Fatal("ordinary exec inherited root")
		}
	}
	lock, err := os.OpenFile(filepath.Join(path, ".auth.lock"), os.O_RDWR, 0)
	must(t, err)
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatal("external ownership lost", err)
	}
}
