package store

import (
	"context"
	"errors"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

var ErrClosed = errors.New("store owner or connection is closed")

// RootOwner lends session-lifetime ownership, NOT the authenticated-operation
// gate. An engine integration may implement this interface without importing app
// internals. Close of that owner must cancel/join leases before unlocking root.
// The current engine public Client does not yet expose this capability.
type RootOwner interface {
	Lease(context.Context) (*RootLease, error)
}

// Owner retains an externally acquired cooperative root owner. Do not copy it.
// It never takes .auth.lock. Leases may coexist with authenticated work; they do
// not serialize it. Close cancels all leases and joins them before release.
type Owner struct {
	mu      sync.Mutex
	root    *os.File
	ctx     context.Context
	cancel  context.CancelFunc
	release func() error
	refs    int
	closing bool
	drained chan struct{}
	once    sync.Once
	err     error
}

// NewOwner borrows an already exclusively owned root and takes responsibility
// for calling release once after all users join. Transfer occurs only on success.
// The caller must not otherwise unlock/close the external owner until release.
// ALL root users, including authenticated operations, must hold leases. Their
// separate auth gate is caller-owned. release must be nonblocking cleanup only
// (users have already joined), and must not reenter Owner.
// Neither the original descriptor nor its lock is closed by this package.
// No root discovery/creation or engine integration is performed here.
func NewOwner(ctx context.Context, borrowed *os.File, release func() error) (*Owner, error) {
	if release == nil {
		return nil, ErrUnsafeRoot
	}
	var root *os.File
	err := WithRoot(ctx, borrowed, func(_ context.Context, dir *os.File) error {
		fd, err := unix.Openat(int(dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return ErrIO
		}
		root = os.NewFile(uintptr(fd), "store-owner")
		return nil
	})
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &Owner{root: root, ctx: lifetime, cancel: cancel, release: release, drained: make(chan struct{})}, nil
}

// RootLease is a synchronous capability. Root must not be closed or retained
// after Close, nor inherited by ordinary exec. All derived users must join first.
// Context is canceled by caller cancellation or Owner.Close. Close is idempotent.
type RootLease struct {
	Root    *os.File
	Context context.Context
	once    sync.Once
	done    func()
}

func (l *RootLease) Close() { l.once.Do(l.done) }

func (o *Owner) Lease(ctx context.Context) (*RootLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closing {
		return nil, ErrClosed
	}
	o.refs++
	work, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(o.ctx, cancel)
	return &RootLease{Root: o.root, Context: work, done: func() {
		stop()
		cancel()
		o.mu.Lock()
		defer o.mu.Unlock()
		o.refs--
		if o.closing && o.refs == 0 {
			close(o.drained)
		}
	}}, nil
}

// Close permanently stops admission and cancels work, then waits context-aware
// for lease users to join. A canceled wait does not unlock underneath users;
// retry Close to finish release. Never call from inside a lease. Concurrent and
// repeated Close calls share the same final result. No background worker is owned.
func (o *Owner) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o.mu.Lock()
	if !o.closing {
		o.closing = true
		o.cancel()
		if o.refs == 0 {
			close(o.drained)
		}
	}
	o.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.drained:
	}
	o.once.Do(func() {
		rootErr := o.root.Close()
		releaseErr := o.release()
		if rootErr != nil || releaseErr != nil {
			o.err = ErrIO
		}
	})
	return o.err
}
