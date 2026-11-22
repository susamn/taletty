// Package auth owns Linux authentication-session exclusion for the Audible connector.
package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

var (
	ErrUnsafeRoot = errors.New("authentication root must be an existing private directory")
	ErrUnsafeLock = errors.New("authentication lock must be a private regular file")
	ErrBusy       = errors.New("another session owns the authentication root")
	ErrIO         = errors.New("authentication lock I/O failed")
	ErrClosed     = errors.New("authentication session is closed")
)

// Session holds the root lock for an engine session, not per operation. Run
// serializes login, library and download token writes. Do not copy a Session.
// Playback does not use this gate. The lock file is never unlinked (inode identity
// must remain stable across cooperative processes). No credentials are stored here.
// All fields, except the immutable token channel, are protected by that token.
type Session struct {
	token  chan struct{}
	fd     int
	root   *os.File
	closed bool
}

// Open locks an existing absolute 0700 root owned by the effective user. It creates
// only .auth.lock (0600); it does not create/discover roots, read auth or contact a
// provider. Final-component symlinks and nonregular/shared lock files are rejected.
// Parent path components may be symlinks; the opened directory descriptor anchors
// the lock lookup. This protects cooperative clients, not a malicious same-UID owner
// who can rename their own directory/lock. No descriptor is inherited by exec.
func Open(ctx context.Context, root string) (*Session, error) {
	return open(ctx, root, syscall.Fstat)
}

// OpenBorrowed duplicates a caller-owned effective-UID-owned 0700 directory
// descriptor via openat(".", O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC) and
// verifies 0700 + euid without acquiring or unlocking .auth.lock (fd: -1).
// The caller must hold exclusive cooperative root ownership (for example,
// store.Owner / RootLease) until Close returns. Close closes only the
// duplicated root descriptor.
func OpenBorrowed(ctx context.Context, root *os.File) (*Session, error) {
	return openBorrowed(ctx, root, syscall.Fstat)
}

func openBorrowed(ctx context.Context, root *os.File, inspect func(int, *syscall.Stat_t) error) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, ErrUnsafeRoot
	}
	dir, err := syscall.Openat(int(root.Fd()), ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrIO
	}
	keepDir := false
	defer func() {
		if !keepDir {
			syscall.Close(dir)
		}
	}()
	var stat syscall.Stat_t
	if err = inspect(dir, &stat); err != nil {
		return nil, ErrIO
	}
	if stat.Mode&(syscall.S_IFMT|07777) != syscall.S_IFDIR|0700 || stat.Uid != uint32(os.Geteuid()) {
		return nil, ErrUnsafeRoot
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	s := &Session{fd: -1, root: os.NewFile(uintptr(dir), "authentication-root"), token: make(chan struct{}, 1)}
	keepDir = true
	s.token <- struct{}{}
	return s, nil
}

// Inject descriptor inspection at the filesystem boundary so failed security
// checks/cleanup can be exercised without mutable process-wide syscall hooks.
func open(ctx context.Context, root string, inspect func(int, *syscall.Stat_t) error) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(root) {
		return nil, ErrUnsafeRoot
	}
	// Remove trailing slashes/dot components before the final no-follow check.
	root = filepath.Clean(root)
	dir, err := syscall.Open(root, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrIO
	}
	keepDir := false
	defer func() {
		if !keepDir {
			syscall.Close(dir)
		}
	}()
	var stat syscall.Stat_t
	if err = inspect(dir, &stat); err != nil {
		return nil, ErrIO
	}
	if stat.Mode&0777 != 0700 || stat.Uid != uint32(os.Geteuid()) {
		return nil, ErrUnsafeRoot
	}
	fd, err := syscall.Openat(dir, ".auth.lock", syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, ErrIO
	}
	if err = inspect(fd, &stat); err != nil {
		syscall.Close(fd)
		return nil, ErrIO
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Mode&0777 != 0600 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		syscall.Close(fd)
		return nil, ErrUnsafeLock
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		syscall.Close(fd)
		if err == syscall.EWOULDBLOCK {
			return nil, ErrBusy
		}
		return nil, ErrIO
	}
	if err = ctx.Err(); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	s := &Session{fd: fd, root: os.NewFile(uintptr(dir), "authentication-root"), token: make(chan struct{}, 1)}
	keepDir = true
	s.token <- struct{}{}
	return s, nil
}

func (s *Session) enter(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.token:
		// Cancellation must win before starting work, even if select chose a token.
		if err := ctx.Err(); err != nil {
			s.token <- struct{}{}
			return err
		}
		return nil
	}
}

// Run invokes operation synchronously with the caller's context after acquiring
// the gate. Operations must honor context and must not reenter Run/Close on the
// same session. This is not an arbitrary-function interrupt mechanism. Callback
// errors/panics retain their meaning; the gate is released even on panic. Provider
// error sanitization belongs to the connector, not this private lock boundary.
func (s *Session) Run(ctx context.Context, operation func(context.Context) error) error {
	if err := s.enter(ctx); err != nil {
		return err
	}
	defer func() { s.token <- struct{}{} }()
	if s.closed {
		return ErrClosed
	}
	return operation(ctx)
}

// WithRoot lends the anchored directory inside the existing authenticated-operation
// gate. The callback must not close/retain the file or reenter Run/WithRoot/Close.
// Only explicit exec.Cmd.ExtraFiles may inherit it; never pass the lock descriptor.
// Join all child users before returning, including on failure/cancellation. The
// borrowed capability is not a snapshot or an arbitrary-callback interrupt API.
func (s *Session) WithRoot(ctx context.Context, operation func(context.Context, *os.File) error) error {
	return s.Run(ctx, func(ctx context.Context) error { return operation(ctx, s.root) })
}

// Close waits context-aware for active work, then closes root and root-lock fds.
// Canceling this wait leaves the session open and externally locked; cancel/join
// the operation first and retry Close. It never unlocks underneath active work.
// Repeated Close with a live context succeeds. The session is unusable afterward,
// including if descriptor close reports an I/O error. No goroutines are spawned.
func (s *Session) Close(ctx context.Context) error {
	if err := s.enter(ctx); err != nil {
		return err
	}
	defer func() { s.token <- struct{}{} }()
	if s.closed {
		return nil
	}
	s.closed = true
	rootErr := s.root.Close()
	var lockErr error
	if s.fd >= 0 {
		lockErr = syscall.Close(s.fd) // Release ownership even if the root close failed.
	}
	if rootErr != nil || lockErr != nil {
		return ErrIO
	}
	return nil
}
