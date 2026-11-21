package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync/atomic"
	"time"

	"github.com/ncruces/go-sqlite3"
	_ "github.com/ncruces/go-sqlite3/embed"
	"github.com/ncruces/go-sqlite3/vfs"
	"golang.org/x/sys/unix"
)

var ErrDatabase = errors.New("store database operation failed")

var sqliteLeaseID atomic.Uint64

// withSQLite is transport ONLY, not a validated Taletty opener. The future
// schema dispatcher must authorize existing state BEFORE calling it: SQLite may
// recover a hot journal during access. Never wire this directly into app startup.
//
// The caller retains session-lifetime cooperative root ownership until return,
// NOT an authenticated operation gate. No .auth.lock acquisition occurs here.
// Work and all derived statements must finish synchronously; neither connection
// nor root may escape. Each invocation owns its connection and VFS registration.
func withSQLite(ctx context.Context, borrowed *os.File, work func(*sqlite3.Conn) error) error {
	return WithRoot(ctx, borrowed, func(ctx context.Context, root *os.File) (err error) {
		fs := &sqliteVFS{root: root, path: fmt.Sprintf("/proc/self/fd/%d/taletty.sqlite3", root.Fd())}
		// This transport intentionally has no WAL recovery/checkpoint contract.
		for _, suffix := range []string{"-wal", "-shm"} {
			var stat unix.Stat_t
			if e := unix.Fstatat(int(root.Fd()), "taletty.sqlite3"+suffix, &stat, unix.AT_SYMLINK_NOFOLLOW); e != unix.ENOENT {
				return ErrUnsafeFile
			}
		}
		name := fmt.Sprintf("taletty-%d", sqliteLeaseID.Add(1))
		vfs.Register(name, fs)
		defer vfs.Unregister(name)
		conn, e := sqlite3.OpenContext(ctx, "file:"+fs.path+"?vfs="+name)
		if e != nil {
			return sqliteFailure(ctx)
		}
		defer func() {
			// Finalize forgotten statements even on panic, before Close releases
			// locks and before the VFS/root lease disappear.
			for _, stmt := range slices.Collect(conn.Stmts()) {
				stmt.Close()
				if err == nil {
					err = ErrDatabase
				}
			}
			if e := conn.Close(); e != nil && err == nil {
				err = ErrDatabase
			}
		}()
		conn.SetInterrupt(ctx)
		if conn.BusyTimeout(100*time.Millisecond) != nil ||
			conn.Exec("PRAGMA temp_store=MEMORY; PRAGMA foreign_keys=ON;") != nil {
			return sqliteFailure(ctx)
		}
		stmt, _, e := conn.Prepare("PRAGMA foreign_keys")
		if e != nil {
			return sqliteFailure(ctx)
		}
		valid := stmt.Step() && stmt.ColumnInt(0) == 1 && stmt.Err() == nil
		e = stmt.Close()
		if !valid || e != nil {
			return sqliteFailure(ctx)
		}
		return work(conn)
	})
}

func sqliteFailure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrDatabase
}

// Do not delegate FullPathname: upstream canonicalizes symlinks and would lose
// the directory anchor. Every pathname here has a real fixed leaf, never a DB
// descriptor pseudo-path. All driver reopen/delete calls retain the same root.
// External cooperative ownership excludes name mutation between preflight and
// driver reopen; malicious same-UID mutation is outside that ownership contract.
type sqliteVFS struct {
	root *os.File
	path string
}

func (s *sqliteVFS) leaf(path string) (string, error) {
	switch path {
	case s.path:
		return "taletty.sqlite3", nil
	case s.path + "-journal":
		return "taletty.sqlite3-journal", nil
	default:
		return "", ErrUnsafeFile
	}
}

func (s *sqliteVFS) FullPathname(path string) (string, error) {
	_, err := s.leaf(path)
	return path, err
}

func (s *sqliteVFS) Open(string, vfs.OpenFlag) (vfs.File, vfs.OpenFlag, error) {
	// SQLite must supply a named main DB or journal, never an anonymous temp.
	return nil, 0, ErrUnsafeFile
}

func (s *sqliteVFS) OpenFilename(path *vfs.Filename, flags vfs.OpenFlag) (vfs.File, vfs.OpenFlag, error) {
	leaf, err := s.leaf(path.String())
	if err != nil {
		return nil, 0, err
	}
	if path.URIParameter("modeof") != "" {
		return nil, 0, ErrUnsafeFile
	}
	if flags&vfs.OPEN_CREATE != 0 {
		fd, e := unix.Openat(int(s.root.Fd()), leaf, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if e != nil && e != unix.EEXIST {
			return nil, 0, ErrIO
		}
		if e == nil {
			err = unix.Close(fd)
			if err == nil {
				err = s.root.Sync()
			}
			if err != nil {
				return nil, 0, ErrIO
			}
		}
	}
	if exists, err := s.Access(path.String(), vfs.ACCESS_EXISTS); err != nil || !exists {
		return nil, 0, ErrUnsafeFile
	}
	// Upstream now only reopens an already private file with O_NOFOLLOW. It
	// cannot create at 0666, chmod via modeof, or create SHM behind our back.
	file, out, err := vfs.Find("os").(vfs.VFSFilename).OpenFilename(path, flags&^(vfs.OPEN_CREATE|vfs.OPEN_EXCLUSIVE))
	if err != nil {
		return nil, 0, err
	}
	return &sqliteFile{File: file, root: s.root, journal: leaf == "taletty.sqlite3-journal"}, out, nil
}

func (s *sqliteVFS) Access(path string, _ vfs.AccessFlag) (bool, error) {
	leaf, err := s.leaf(path)
	if err != nil {
		// SQLite probes WAL even in rollback mode; absent is safe, existing is
		// never opened or deleted. withSQLite rejects it before connecting.
		if path != s.path+"-wal" && path != s.path+"-shm" {
			return false, err
		}
		leaf = "taletty.sqlite3" + path[len(s.path):]
	}
	var stat unix.Stat_t
	err = unix.Fstatat(int(s.root.Fd()), leaf, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err == unix.ENOENT {
		return false, nil
	}
	if err != nil {
		return false, ErrIO
	}
	if !privateFile(&stat) || leaf == "taletty.sqlite3-wal" || leaf == "taletty.sqlite3-shm" {
		return false, ErrUnsafeFile
	}
	return true, nil
}

func (s *sqliteVFS) Delete(path string, syncDir bool) error {
	if path != s.path+"-journal" {
		return ErrUnsafeFile
	}
	if exists, err := s.Access(path, vfs.ACCESS_EXISTS); err != nil || !exists {
		return ErrUnsafeFile
	}
	if err := unix.Unlinkat(int(s.root.Fd()), "taletty.sqlite3-journal", 0); err != nil {
		return ErrIO
	}
	if syncDir {
		return s.root.Sync()
	}
	return nil
}

// Expose only v1 file IO: deliberately omit SharedMemory and Unwrap, so SQLite
// cannot enable WAL or reach the upstream SHM path. Keep upstream OFD locking.
type sqliteFile struct {
	vfs.File
	root    *os.File
	journal bool
}

func (s *sqliteFile) Sync(flags vfs.SyncFlag) error {
	if err := s.File.Sync(flags); err != nil {
		return err
	}
	if s.journal {
		return s.root.Sync()
	}
	return nil
}
