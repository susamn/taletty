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
	defer syscall.Close(dir)
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
	s := &Session{fd: fd, token: make(chan struct{}, 1)}
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

// Close waits context-aware for active work, then closes the sole root-lock fd.
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
	if err := syscall.Close(s.fd); err != nil {
		return ErrIO
	}
	return nil
}
