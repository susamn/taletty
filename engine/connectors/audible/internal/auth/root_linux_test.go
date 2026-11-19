package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestRootCapabilityAnchoringAndExplicitExec(t *testing.T) {
	root := filepath.Join(privateRoot(t), "original")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	s := openSession(t, root)
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.WithRoot(context.Background(), func(ctx context.Context, directory *os.File) error {
		var info, lockInfo syscall.Stat_t
		if err := syscall.Fstat(int(directory.Fd()), &info); err != nil {
			return err
		}
		if err := syscall.Fstat(s.fd, &lockInfo); err != nil {
			return err
		}
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, directory.Fd(), syscall.F_GETFD, 0)
		if errno != 0 || flags&syscall.FD_CLOEXEC == 0 {
			return errors.New("root lost CLOEXEC")
		}
		for _, mode := range []string{"ordinary", "explicit"} {
			childCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestRootCapabilityChild$")
			cmd.Env = append(os.Environ(), "TALETTY_ROOT_CHILD="+mode, fmt.Sprintf("TALETTY_ROOT_ID=%d:%d", info.Dev, info.Ino), "TALETTY_LOCK_CHILD_ROOT="+moved, fmt.Sprintf("TALETTY_LOCK_ID=%d:%d", lockInfo.Dev, lockInfo.Ino))
			if mode == "explicit" {
				cmd.ExtraFiles = []*os.File{directory}
			}
			output, err := cmd.CombinedOutput()
			cancel()
			if err != nil || string(output) != "ok\n" {
				return fmt.Errorf("child capability failed: %v %q", err, output)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(moved, "child-marker")); err != nil {
		t.Fatal("child did not use anchored directory", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatal("replacement root touched", entries, err)
	}
}

func TestRootCapabilityChild(t *testing.T) {
	mode := os.Getenv("TALETTY_ROOT_CHILD")
	if mode == "" {
		return
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		os.Exit(1)
	}
	found := false
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			os.Exit(1)
		}
		var info syscall.Stat_t
		if syscall.Fstat(fd, &info) != nil {
			continue
		}
		id := fmt.Sprintf("%d:%d", info.Dev, info.Ino)
		if id == os.Getenv("TALETTY_LOCK_ID") {
			os.Exit(1)
		}
		if id == os.Getenv("TALETTY_ROOT_ID") {
			if mode != "explicit" || fd != 3 {
				os.Exit(1)
			}
			found = true
		}
	}
	if found != (mode == "explicit") {
		os.Exit(1)
	}
	// A root-only inheritance must not inherit the advisory lock or bypass ownership.
	if _, err := Open(context.Background(), os.Getenv("TALETTY_LOCK_CHILD_ROOT")); !errors.Is(err, ErrBusy) {
		os.Exit(1)
	}
	if found {
		fd, err := syscall.Openat(3, "child-marker", syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL, 0600)
		if err != nil {
			os.Exit(1)
		}
		syscall.Close(fd)
	}
	os.Stdout.WriteString("ok\n")
	os.Exit(0)
}

func TestRootBorrowSharesGateAndCloseLifetime(t *testing.T) {
	s := openSession(t, privateRoot(t))
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var borrowed *os.File
	go func() {
		done <- s.WithRoot(context.Background(), func(_ context.Context, root *os.File) error {
			borrowed = root
			close(started)
			<-release
			_, err := root.Stat()
			return err
		})
	}()
	<-started
	for _, method := range []string{"borrow", "close"} {
		ctx, cancel := context.WithCancel(context.Background())
		waiting, outcome := make(chan struct{}), make(chan error, 1)
		observed := &observedContext{Context: ctx, waiting: waiting}
		go func() {
			if method == "close" {
				outcome <- s.Close(observed)
				return
			}
			outcome <- s.WithRoot(observed, func(context.Context, *os.File) error { return errors.New("canceled borrow ran") })
		}()
		<-waiting
		cancel()
		if err := <-outcome; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if _, err := borrowed.Stat(); err != nil {
		t.Fatal("canceled Close revoked live root", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	failure := errors.New("callback failure")
	if err := s.WithRoot(context.Background(), func(context.Context, *os.File) error { return failure }); err != failure {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() != "borrow panic" {
				t.Error("lost caller panic")
			}
		}()
		s.WithRoot(context.Background(), func(context.Context, *os.File) error { panic("borrow panic") })
	}()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := borrowed.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("root descriptor leaked", err)
	}
	if err := s.WithRoot(context.Background(), func(context.Context, *os.File) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestRootCloseFailureStillReleasesLock(t *testing.T) {
	root := privateRoot(t)
	s, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	// Boundary fault: simulate a lost root FD without replacing global syscall hooks.
	if err := s.root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); !errors.Is(err, ErrIO) {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal("repeat close", err)
	}
	openSession(t, root)
}
