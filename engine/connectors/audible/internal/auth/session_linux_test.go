package auth

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func privateRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func openSession(t *testing.T, root string) *Session {
	t.Helper()
	s, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestRootOwnershipAndRelease(t *testing.T) {
	root := privateRoot(t)
	s := openSession(t, root)
	info, err := os.Stat(filepath.Join(root, ".auth.lock"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("lock must be private", info, err)
	}
	if _, err := Open(context.Background(), root); !errors.Is(err, ErrBusy) {
		t.Fatal("second session must not modify shared auth", err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(context.Background(), func(context.Context) error {
		t.Fatal("closed session ran work")
		return nil
	}); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	openSession(t, root)
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 || entries[0].Name() != ".auth.lock" {
		t.Fatal("no auth/cache discovery or root cleanup", entries, err)
	}
}

func TestUnsafeRootsAndLockFiles(t *testing.T) {
	for _, kind := range []string{"relative", "missing", "root-symlink", "root-symlink-slash", "root-symlink-dot", "public-root", "public-lock", "lock-symlink", "directory", "fifo", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			root := privateRoot(t)
			lock := filepath.Join(root, ".auth.lock")
			var err error
			switch kind {
			case "relative":
				root = "not-an-absolute-root"
			case "missing":
				root = filepath.Join(root, "missing")
			case "root-symlink", "root-symlink-slash", "root-symlink-dot":
				link := filepath.Join(t.TempDir(), "root")
				err = os.Symlink(root, link)
				root = link
				if kind == "root-symlink-slash" {
					root += "/"
				} else if kind == "root-symlink-dot" {
					root += "/."
				}
			case "public-root":
				err = os.Chmod(root, 0755)
			case "public-lock":
				err = os.WriteFile(lock, nil, 0644)
			case "lock-symlink":
				other := filepath.Join(t.TempDir(), "private")
				if err = os.WriteFile(other, []byte("untouched"), 0600); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(other, lock)
			case "directory":
				err = os.Mkdir(lock, 0700)
			case "fifo":
				err = syscall.Mkfifo(lock, 0600)
			case "hardlink":
				other := filepath.Join(t.TempDir(), "shared")
				if err = os.WriteFile(other, nil, 0600); err != nil {
					t.Fatal(err)
				}
				err = os.Link(other, lock)
			}
			if err != nil {
				t.Fatal(err)
			}
			if s, err := Open(context.Background(), root); err == nil {
				s.Close(context.Background())
				t.Fatal("unsafe path accepted")
			} else if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), lock) {
				t.Fatal("diagnostic disclosed private path", err)
			}
		})
	}
}

// A real cancelable context with an injected cancellation point at Err's second
// check makes the before-work/after-acquire race deterministic, without sleeps or
// relying on Go select randomness. Err/Done retain their context contract.
type cancelAtCheck struct {
	context.Context
	cancel context.CancelFunc
	checks atomic.Int32
}

func (c *cancelAtCheck) Err() error {
	if c.checks.Add(1) == 2 {
		c.cancel()
	}
	return c.Context.Err()
}

func raceContext() *cancelAtCheck {
	ctx, cancel := context.WithCancel(context.Background())
	return &cancelAtCheck{Context: ctx, cancel: cancel}
}

func TestCancellationAtAcquisitionBoundaries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	root := privateRoot(t)
	if _, err := Open(raceContext(), root); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation after file lock must close it", err)
	}
	s := openSession(t, root)
	for _, canceled := range []context.Context{ctx, raceContext()} {
		if err := s.Run(canceled, func(context.Context) error {
			t.Error("canceled work started")
			return nil
		}); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if err := s.Run(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatal("token was not returned after cancellation", err)
	}
}

func TestSerializationCancellationAndClose(t *testing.T) {
	root := privateRoot(t)
	s := openSession(t, root)
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		finished <- s.Run(ctx, func(received context.Context) error {
			if received != ctx {
				return errors.New("callback context changed")
			}
			close(started)
			<-release
			return received.Err()
		})
	}()
	<-started
	waiting, queued := make(chan struct{}), make(chan error, 1)
	waitCtx, cancelWait := context.WithCancel(context.Background())
	go func() {
		queued <- s.Run(&observedContext{Context: waitCtx, waiting: waiting}, func(context.Context) error {
			return errors.New("queued canceled callback ran")
		})
	}()
	<-waiting
	cancelWait()
	if err := <-queued; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	closing, closed := make(chan struct{}), make(chan error, 1)
	closeCtx, cancelClose := context.WithCancel(context.Background())
	go func() { closed <- s.Close(&observedContext{Context: closeCtx, waiting: closing}) }()
	<-closing
	cancelClose()
	if err := <-closed; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), root); !errors.Is(err, ErrBusy) {
		t.Fatal("Close released ownership underneath active work", err)
	}
	cancel()
	close(release)
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	failure := errors.New("operation failed")
	if err := s.Run(context.Background(), func(context.Context) error { return failure }); err != failure {
		t.Fatal("operation result lost", err)
	}
}

type observedContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *observedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestPanicAndConcurrentOperations(t *testing.T) {
	s := openSession(t, privateRoot(t))
	func() {
		defer func() {
			if recover() != "caller panic" {
				t.Error("panic was swallowed")
			}
		}()
		s.Run(context.Background(), func(context.Context) error { panic("caller panic") })
	}()
	var wg sync.WaitGroup
	var active atomic.Int32
	for range 40 {
		wg.Go(func() {
			if err := s.Run(context.Background(), func(context.Context) error {
				if active.Add(1) != 1 {
					t.Error("operations overlapped")
				}
				active.Add(-1)
				return nil
			}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestCrossProcessOwnershipAndExecIsolation(t *testing.T) {
	root := privateRoot(t)
	s := openSession(t, root)
	// Check CLOEXEC on the actual owned descriptor, not merely an open flag in code.
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(s.fd), syscall.F_GETFD, 0)
	if errno != 0 || flags&syscall.FD_CLOEXEC == 0 {
		t.Fatal("lock descriptor may leak into a bridge process", flags, errno)
	}
	child := func(want string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRootLockChild$")
		cmd.Env = append(os.Environ(), "TALETTY_LOCK_CHILD_ROOT="+root)
		data, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(data)) != want {
			t.Fatal("child lock outcome", string(data), err)
		}
	}
	child("busy")
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	child("acquired")
	openSession(t, root)
}

func TestRootLockChild(t *testing.T) {
	root := os.Getenv("TALETTY_LOCK_CHILD_ROOT")
	if root == "" {
		return
	}
	s, err := Open(context.Background(), root)
	if errors.Is(err, ErrBusy) {
		os.Stdout.WriteString("busy\n")
		os.Exit(0)
	}
	if err != nil || s.Close(context.Background()) != nil {
		os.Exit(1)
	}
	os.Stdout.WriteString("acquired\n")
	os.Exit(0)
}

func TestInspectionFailureClosesDescriptors(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		root := privateRoot(t)
		calls, failedFD := 0, -1
		_, err := open(context.Background(), root, func(fd int, stat *syscall.Stat_t) error {
			calls++
			if calls == failAt {
				failedFD = fd
				return syscall.EIO
			}
			return syscall.Fstat(fd, stat)
		})
		if !errors.Is(err, ErrIO) || calls != failAt {
			t.Fatal("inspection failure not rejected", calls, err)
		}
		if err := syscall.Fstat(failedFD, new(syscall.Stat_t)); err != syscall.EBADF {
			t.Fatal("inspection failure leaked descriptor", err)
		}
		openSession(t, root)
	}
}

func TestOwnershipChecksRejectOtherUsers(t *testing.T) {
	for _, otherOwnerAt := range []int{1, 2} {
		calls := 0
		_, err := open(context.Background(), privateRoot(t), func(fd int, stat *syscall.Stat_t) error {
			if err := syscall.Fstat(fd, stat); err != nil {
				return err
			}
			calls++
			if calls == otherOwnerAt {
				stat.Uid++ // Simulate another owner without requiring privileged chown.
			}
			return nil
		})
		want := ErrUnsafeRoot
		if otherOwnerAt == 2 {
			want = ErrUnsafeLock
		}
		if !errors.Is(err, want) {
			t.Fatal("foreign-owned auth path accepted", err)
		}
	}
}

func TestCloseWaitsForSuccessfulOperation(t *testing.T) {
	s := openSession(t, privateRoot(t))
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.Run(context.Background(), func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	waiting, closed := make(chan struct{}), make(chan error, 1)
	go func() {
		closed <- s.Close(&observedContext{Context: context.Background(), waiting: waiting})
	}()
	<-waiting
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := s.Run(context.Background(), func(context.Context) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatal("close did not finish", err)
	}
}

func TestCloseIOFailureIsSafeAndTerminal(t *testing.T) {
	s, err := Open(context.Background(), privateRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	// Inject descriptor loss at the OS boundary without overriding package globals.
	if err := syscall.Close(s.fd); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); !errors.Is(err, ErrIO) {
		t.Fatal("close failure must not be hidden", err)
	}
	if err := s.Run(context.Background(), func(context.Context) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatal("failed close session remained usable", err)
	}
}

func TestOpenBorrowedOwnershipSerializationAndClose(t *testing.T) {
	root := filepath.Join(privateRoot(t), "borrowed")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, ".auth.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	callerRoot, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer callerRoot.Close()

	s, err := OpenBorrowed(context.Background(), callerRoot)
	if err != nil {
		t.Fatal("OpenBorrowed failed on externally locked root:", err)
	}
	if s.fd != -1 || s.root.Fd() == callerRoot.Fd() {
		t.Fatalf("unexpected borrowed session descriptors: fd=%d root=%d caller=%d", s.fd, s.root.Fd(), callerRoot.Fd())
	}
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, s.root.Fd(), syscall.F_GETFD, 0)
	if errno != 0 || flags&syscall.FD_CLOEXEC == 0 {
		t.Fatal("borrowed root descriptor missing FD_CLOEXEC", flags, errno)
	}

	// Anchoring survives directory rename.
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := s.WithRoot(context.Background(), func(_ context.Context, dir *os.File) error {
		fd, err := syscall.Openat(int(dir.Fd()), "borrowed-marker", syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL, 0600)
		if err != nil {
			return err
		}
		return syscall.Close(fd)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(moved, "borrowed-marker")); err != nil {
		t.Fatal("borrowed root did not stay anchored across rename:", err)
	}

	// Concurrent operations serialize via Session.token.
	var wg sync.WaitGroup
	var active atomic.Int32
	for range 20 {
		wg.Go(func() {
			if err := s.WithRoot(context.Background(), func(context.Context, *os.File) error {
				if active.Add(1) != 1 {
					t.Error("borrowed operations overlapped")
				}
				active.Add(-1)
				return nil
			}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	// Canceled Close waits without closing duplicated root; retry succeeds.
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.Run(context.Background(), func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	closing, closed := make(chan struct{}), make(chan error, 1)
	closeCtx, cancelClose := context.WithCancel(context.Background())
	go func() { closed <- s.Close(&observedContext{Context: closeCtx, waiting: closing}) }()
	<-closing
	cancelClose()
	if err := <-closed; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal("repeat Close failed:", err)
	}
	if _, err := s.root.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("duplicated root fd not closed")
	}
	if _, err := callerRoot.Stat(); err != nil {
		t.Fatal("Close closed caller's root fd:", err)
	}
	if err := s.Run(context.Background(), func(context.Context) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatal("closed borrowed session remained usable:", err)
	}

	// Caller's .auth.lock must still be held after borrowed Close.
	checkChild := func(want string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRootLockChild$")
		cmd.Env = append(os.Environ(), "TALETTY_LOCK_CHILD_ROOT="+moved)
		data, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(data)) != want {
			t.Fatal("child lock outcome", string(data), err)
		}
	}
	checkChild("busy")
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	checkChild("acquired")
}

func TestOpenBorrowedRejectsUnsafeAndCanceled(t *testing.T) {
	root := privateRoot(t)
	callerRoot, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer callerRoot.Close()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := OpenBorrowed(canceled, callerRoot); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := OpenBorrowed(context.Background(), nil); !errors.Is(err, ErrUnsafeRoot) {
		t.Fatal(err)
	}

	// Regular file rejected.
	regPath := filepath.Join(root, "regular")
	if err := os.WriteFile(regPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	regFile, err := os.Open(regPath)
	if err != nil {
		t.Fatal(err)
	}
	defer regFile.Close()
	if _, err := OpenBorrowed(context.Background(), regFile); !errors.Is(err, ErrIO) {
		t.Fatal(err)
	}

	// Symlink opened with O_PATH|O_NOFOLLOW rejected.
	linkPath := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, linkPath); err != nil {
		t.Fatal(err)
	}
	linkFD, err := unix.Open(linkPath, unix.O_PATH|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	linkFile := os.NewFile(uintptr(linkFD), "symlink")
	defer linkFile.Close()
	if _, err := OpenBorrowed(context.Background(), linkFile); !errors.Is(err, ErrIO) {
		t.Fatal(err)
	}

	// Closed file rejected.
	closedFile, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	closedFile.Close()
	if _, err := OpenBorrowed(context.Background(), closedFile); !errors.Is(err, ErrIO) {
		t.Fatal(err)
	}

	// Non-0700 modes rejected.
	for _, mode := range []os.FileMode{0755, 0700 | os.ModeSticky} {
		if err := os.Chmod(root, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenBorrowed(context.Background(), callerRoot); !errors.Is(err, ErrUnsafeRoot) {
			t.Fatalf("mode %v accepted: %v", mode, err)
		}
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}

	// Foreign UID, inspect I/O error, and post-inspect cancellation close duplicated fd.
	var inspectedFD int
	if _, err := openBorrowed(context.Background(), callerRoot, func(fd int, stat *syscall.Stat_t) error {
		inspectedFD = fd
		return syscall.EIO
	}); !errors.Is(err, ErrIO) {
		t.Fatal(err)
	}
	if err := syscall.Fstat(inspectedFD, new(syscall.Stat_t)); err != syscall.EBADF {
		t.Fatal("inspect error leaked duplicated fd:", err)
	}

	if _, err := openBorrowed(context.Background(), callerRoot, func(fd int, stat *syscall.Stat_t) error {
		inspectedFD = fd
		if err := syscall.Fstat(fd, stat); err != nil {
			return err
		}
		stat.Uid++
		return nil
	}); !errors.Is(err, ErrUnsafeRoot) {
		t.Fatal(err)
	}
	if err := syscall.Fstat(inspectedFD, new(syscall.Stat_t)); err != syscall.EBADF {
		t.Fatal("foreign UID leaked duplicated fd:", err)
	}

	if _, err := openBorrowed(raceContext(), callerRoot, func(fd int, stat *syscall.Stat_t) error {
		inspectedFD = fd
		return syscall.Fstat(fd, stat)
	}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := syscall.Fstat(inspectedFD, new(syscall.Stat_t)); err != syscall.EBADF {
		t.Fatal("canceled OpenBorrowed leaked duplicated fd:", err)
	}

	// Close I/O failure on borrowed session marks closed and returns ErrIO.
	s, err := OpenBorrowed(context.Background(), callerRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); !errors.Is(err, ErrIO) {
		t.Fatal(err)
	}
	if err := s.Run(context.Background(), func(context.Context) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}
