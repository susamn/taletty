package profile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/susamn/taletty/app/internal/store"
	"golang.org/x/sys/unix"
)

var (
	// ErrUnsafeRoot reports an existing profile directory, downloads directory,
	// or .auth.lock with unsafe type, ownership, link count, or permissions.
	ErrUnsafeRoot = errors.New("profile root must be a private owned directory")
	// ErrBusy reports that another live session holds the profile .auth.lock.
	ErrBusy = errors.New("another session owns the profile root")
	// ErrIO reports an operating-system I/O failure during root bootstrap or close.
	ErrIO = errors.New("profile root initialization failed")
)

// Workspace owns an initialized, locked Taletty data root (~/.config/taletty/)
// and its validated application store for a session lifetime.
type Workspace struct {
	rootPath      string
	downloadsPath string
	owner         *store.Owner
	store         *store.Store
}

// RootPath returns the resolved ~/.config/taletty path.
func (w *Workspace) RootPath() string { return w.rootPath }

// DownloadsPath returns the resolved ~/.config/taletty/downloads path.
func (w *Workspace) DownloadsPath() string { return w.downloadsPath }

// Owner returns the session-lifetime root owner for anchored root leases.
func (w *Workspace) Owner() *store.Owner { return w.owner }

// Store returns the validated Taletty SQLite persistence store.
func (w *Workspace) Store() *store.Store { return w.store }

// Close drains and closes Store first, then Owner (which releases .auth.lock
// after all leases join). Repeated and concurrent Close calls are safe. If ctx
// expires while waiting, Close returns ctx.Err() without unlocking underneath
// active operations or leases; callers may retry Close.
func (w *Workspace) Close(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if w.store != nil {
		if err := w.store.Close(ctx); err != nil {
			return err
		}
	}
	if w.owner != nil {
		return w.owner.Close(ctx)
	}
	return nil
}

type sysOps struct {
	fstat   func(int, *unix.Stat_t) error
	fstatat func(int, string, *unix.Stat_t, int) error
	flock   func(int, int) error
	fsync   func(int) error
}

func defaultSysOps() sysOps {
	return sysOps{
		fstat:   unix.Fstat,
		fstatat: unix.Fstatat,
		flock:   unix.Flock,
		fsync:   unix.Fsync,
	}
}

// Open initializes (idempotently) and locks the fresh private ~/.config/taletty/
// data root for home, opens the validated v1 SQLite store, and reconciles any
// interrupted active download jobs using nowMS. It ignores XDG_CONFIG_HOME and
// never scans, reads, or modifies ~/.config/audible-tui or other applications.
func Open(ctx context.Context, home string, nowMS int64) (*Workspace, error) {
	return openWorkspace(ctx, home, nowMS, defaultSysOps())
}

func openWorkspace(ctx context.Context, home string, nowMS int64, ops sysOps) (*Workspace, error) {
	rootPath, err := DefaultRoot(home)
	if err != nil {
		return nil, err
	}
	if nowMS < 0 {
		return nil, store.ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	euid := uint32(os.Geteuid())
	rootFD, createdRoot, err := openPrivateRoot(rootPath, euid, ops)
	if err != nil {
		return nil, err
	}
	rootFile := os.NewFile(uintptr(rootFD), "profile-root")

	createdDownloads, err := ensurePrivateDownloads(rootFD, euid, ops)
	if err != nil {
		_ = rootFile.Close()
		return nil, err
	}

	lockFD, createdLock, err := acquireAuthLock(rootFD, euid, ops)
	if err != nil {
		_ = rootFile.Close()
		return nil, err
	}

	releaseFn := func() error {
		rootCloseErr := rootFile.Close()
		unlockErr := ops.flock(lockFD, unix.LOCK_UN)
		lockCloseErr := unix.Close(lockFD)
		if rootCloseErr != nil || unlockErr != nil || lockCloseErr != nil {
			return ErrIO
		}
		return nil
	}

	if createdRoot || createdDownloads || createdLock {
		if err := ops.fsync(rootFD); err != nil {
			_ = releaseFn()
			return nil, ErrIO
		}
	}
	if err := ctx.Err(); err != nil {
		_ = releaseFn()
		return nil, err
	}

	owner, err := store.NewOwner(ctx, rootFile, releaseFn)
	if err != nil {
		_ = releaseFn()
		return nil, err
	}

	st, err := store.Open(ctx, owner)
	if err != nil {
		_ = owner.Close(context.Background())
		return nil, err
	}

	published, err := collectPublishedMedia(ctx, st, rootFD, euid, ops)
	if err != nil {
		_ = st.Close(context.Background())
		_ = owner.Close(context.Background())
		return nil, err
	}
	if _, err := st.ReconcileActiveJobs(ctx, nowMS, published); err != nil {
		_ = st.Close(context.Background())
		_ = owner.Close(context.Background())
		return nil, err
	}

	return &Workspace{
		rootPath:      rootPath,
		downloadsPath: filepath.Join(rootPath, "downloads"),
		owner:         owner,
		store:         st,
	}, nil
}

func openPrivateRoot(rootPath string, euid uint32, ops sysOps) (int, bool, error) {
	configDir := filepath.Dir(rootPath)
	if err := unix.Mkdir(configDir, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, false, ErrIO
	}
	cfgFD, err := unix.Open(configDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return -1, false, ErrUnsafeRoot
		}
		return -1, false, ErrIO
	}
	var cfgStat unix.Stat_t
	if err := ops.fstat(cfgFD, &cfgStat); err != nil {
		_ = unix.Close(cfgFD)
		return -1, false, ErrIO
	}
	if cfgStat.Mode&unix.S_IFMT != unix.S_IFDIR || cfgStat.Uid != euid {
		_ = unix.Close(cfgFD)
		return -1, false, ErrUnsafeRoot
	}

	err = unix.Mkdirat(cfgFD, "taletty", 0700)
	createdRoot := err == nil
	if err != nil && !errors.Is(err, unix.EEXIST) {
		_ = unix.Close(cfgFD)
		return -1, false, ErrIO
	}
	rootFD, err := unix.Openat(cfgFD, "taletty", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Close(cfgFD)
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return -1, false, ErrUnsafeRoot
		}
		return -1, false, ErrIO
	}
	if createdRoot {
		if err := ops.fsync(cfgFD); err != nil {
			_ = unix.Close(cfgFD)
			_ = unix.Close(rootFD)
			return -1, false, ErrIO
		}
	}
	if err := unix.Close(cfgFD); err != nil {
		_ = unix.Close(rootFD)
		return -1, false, ErrIO
	}

	var rootStat unix.Stat_t
	if err := ops.fstat(rootFD, &rootStat); err != nil {
		_ = unix.Close(rootFD)
		return -1, false, ErrIO
	}
	if rootStat.Mode&(unix.S_IFMT|07777) != unix.S_IFDIR|0700 || rootStat.Uid != euid {
		_ = unix.Close(rootFD)
		return -1, false, ErrUnsafeRoot
	}
	return rootFD, createdRoot, nil
}

func ensurePrivateDownloads(rootFD int, euid uint32, ops sysOps) (bool, error) {
	err := unix.Mkdirat(rootFD, "downloads", 0700)
	created := err == nil
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return false, ErrIO
	}
	dlFD, err := unix.Openat(rootFD, "downloads", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return false, ErrUnsafeRoot
		}
		return false, ErrIO
	}
	var dlStat unix.Stat_t
	fstatErr := ops.fstat(dlFD, &dlStat)
	closeErr := unix.Close(dlFD)
	if fstatErr != nil || closeErr != nil {
		return false, ErrIO
	}
	if dlStat.Mode&(unix.S_IFMT|07777) != unix.S_IFDIR|0700 || dlStat.Uid != euid {
		return false, ErrUnsafeRoot
	}
	return created, nil
}

func acquireAuthLock(rootFD int, euid uint32, ops sysOps) (int, bool, error) {
	var preStat unix.Stat_t
	preErr := ops.fstatat(rootFD, ".auth.lock", &preStat, unix.AT_SYMLINK_NOFOLLOW)
	created := false
	switch {
	case errors.Is(preErr, unix.ENOENT):
		created = true
	case preErr != nil:
		return -1, false, ErrIO
	default:
		if !isPrivateFile(&preStat, euid) {
			return -1, false, ErrUnsafeRoot
		}
	}

	lockFD, err := unix.Openat(rootFD, ".auth.lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EISDIR) {
			return -1, false, ErrUnsafeRoot
		}
		return -1, false, ErrIO
	}

	var fdStat, dirStat unix.Stat_t
	if err := ops.fstat(lockFD, &fdStat); err != nil {
		_ = unix.Close(lockFD)
		return -1, false, ErrIO
	}
	if err := ops.fstatat(rootFD, ".auth.lock", &dirStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		_ = unix.Close(lockFD)
		return -1, false, ErrIO
	}
	if !isPrivateFile(&fdStat, euid) || !isPrivateFile(&dirStat, euid) ||
		fdStat.Dev != dirStat.Dev || fdStat.Ino != dirStat.Ino {
		_ = unix.Close(lockFD)
		return -1, false, ErrUnsafeRoot
	}

	if err := ops.flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = unix.Close(lockFD)
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return -1, false, ErrBusy
		}
		return -1, false, ErrIO
	}
	return lockFD, created, nil
}

func collectPublishedMedia(ctx context.Context, st *store.Store, rootFD int, euid uint32, ops sysOps) (map[store.BookKey]bool, error) {
	media, err := st.LocalMediaList(ctx)
	if err != nil {
		return nil, err
	}
	if len(media) == 0 {
		return nil, nil
	}
	published := make(map[store.BookKey]bool, len(media))
	for _, item := range media {
		if isPublishedFilePrivate(rootFD, item.RelativePath, euid, ops) {
			published[item.Key] = true
		}
	}
	return published, nil
}

func isPublishedFilePrivate(rootFD int, relPath string, euid uint32, ops sysOps) bool {
	if strings.ContainsAny(relPath, "\x00\\") {
		return false
	}
	parts := strings.Split(relPath, "/")
	if len(parts) < 2 || parts[0] != "downloads" {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	dirFD := rootFD
	closeDir := false
	defer func() {
		if closeDir {
			_ = unix.Close(dirFD)
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		nextFD, err := unix.Openat(dirFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if closeDir {
			_ = unix.Close(dirFD)
			closeDir = false
		}
		if err != nil {
			return false
		}
		dirFD = nextFD
		closeDir = true
		var st unix.Stat_t
		if err := ops.fstat(dirFD, &st); err != nil {
			return false
		}
		if st.Mode&(unix.S_IFMT|07777) != unix.S_IFDIR|0700 || st.Uid != euid {
			return false
		}
	}
	var st unix.Stat_t
	if err := ops.fstatat(dirFD, parts[len(parts)-1], &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false
	}
	return isPrivateFile(&st, euid)
}

func isPrivateFile(st *unix.Stat_t, euid uint32) bool {
	return st.Mode&(unix.S_IFMT|07777) == unix.S_IFREG|0600 && st.Uid == euid && st.Nlink == 1
}
