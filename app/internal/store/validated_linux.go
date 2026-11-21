package store

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/ncruces/go-sqlite3"
	"golang.org/x/sys/unix"
)

//go:embed schema/001_initial.sql
var initialSQL string

var (
	ErrInvalid  = errors.New("store database is not a valid Taletty v1 database; original files retained")
	ErrVersion  = errors.New("store database version is unsupported; original files retained")
	ErrRecovery = errors.New("store sidecar requires explicit recovery; original files retained")
)

// Initialize compiles the pinned SQLite runtime once per process. Call at startup
// before admitting interactive work. Upstream compilation is synchronous and NOT
// context-interruptible; query/busy cancellation starts after this boundary.
// Open also initializes implicitly. No files in the user's root are accessed here.
func Initialize() error {
	if sqlite3.Initialize() != nil {
		return ErrDatabase
	}
	return nil
}

// Store is the application-facing validated database lifetime. It serializes local
// operations, not authenticated work. It retains no raw connection between calls;
// all statements, VFS registrations and root leases join before an operation ends.
// Typed repositories may use withConn inside this package; UI never receives SQL.
// Do not copy a Store. Close it before releasing its root owner.
type Store struct {
	owner  RootOwner
	token  chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
}

func Open(ctx context.Context, owner RootOwner) (*Store, error) {
	if owner == nil {
		return nil, ErrUnsafeRoot
	}
	lifetime, cancel := context.WithCancel(context.Background())
	s := &Store{owner: owner, token: make(chan struct{}, 1), ctx: lifetime, cancel: cancel}
	s.token <- struct{}{}
	err := s.run(ctx, func(ctx context.Context, root *os.File) error {
		return openValidated(ctx, root, initialSQL)
	})
	if err != nil {
		cancel()
		return nil, err
	}
	return s, nil
}

// Check verifies markers, exact schema, integrity and FK rows without writing.
func (s *Store) Check(ctx context.Context) error {
	return s.run(ctx, func(ctx context.Context, root *os.File) error {
		fresh, err := inspectDatabase(ctx, root)
		if err == nil && fresh {
			return ErrInvalid
		}
		return err
	})
}

// Close stops admission, cancels active/queued local work and joins it. A canceled
// wait can be retried; the Store remains closed. It never closes the external
// owner or waits for unrelated authenticated transfers. No goroutines are owned.
func (s *Store) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.token:
		s.token <- struct{}{}
		return nil
	}
}

func (s *Store) run(ctx context.Context, work func(context.Context, *os.File) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return ErrClosed
	case <-s.token:
	}
	defer func() { s.token <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.ctx.Err() != nil {
		return ErrClosed
	}
	lease, err := s.owner.Lease(ctx)
	if err != nil {
		return err
	}
	defer lease.Close()
	active, cancel := context.WithCancel(lease.Context)
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	return work(active, lease.Root)
}

func (s *Store) withConn(ctx context.Context, work func(*sqlite3.Conn) error) error {
	return s.run(ctx, func(ctx context.Context, root *os.File) error {
		fresh, err := inspectDatabase(ctx, root)
		if err != nil {
			return err
		}
		if fresh {
			return ErrInvalid
		}
		return withSQLite(ctx, root, func(c *sqlite3.Conn) error {
			if work(c) != nil {
				return sqliteFailure(ctx)
			}
			return nil
		})
	})
}

func openValidated(ctx context.Context, root *os.File, script string) error {
	fresh, err := inspectDatabase(ctx, root)
	if err != nil || !fresh {
		return err
	}
	return withSQLite(ctx, root, func(c *sqlite3.Conn) error {
		if c.Exec(script) != nil {
			// Statement failure does not roll back a transaction. Do it explicitly,
			// even after cancellation, before the transport closes the connection.
			c.SetInterrupt(context.Background())
			c.Exec("ROLLBACK")
			return sqliteFailure(ctx)
		}
		return validateConnection(ctx, c, false)
	})
}

// inspectDatabase is the ONLY public-opener path to SQLite. Authorize header and
// refuse every existing sidecar before allowing even readonly SQLite recovery
// probes. No journal can be attributed safely after a crash from markers alone.
func inspectDatabase(ctx context.Context, borrowed *os.File) (fresh bool, err error) {
	err = WithRoot(ctx, borrowed, func(ctx context.Context, root *os.File) error {
		for _, suffix := range []string{"-journal", "-wal", "-shm"} {
			var st unix.Stat_t
			e := unix.Fstatat(int(root.Fd()), "taletty.sqlite3"+suffix, &st, unix.AT_SYMLINK_NOFOLLOW)
			if e == nil {
				return ErrRecovery
			}
			if e != unix.ENOENT {
				return ErrIO
			}
		}
		fd, e := unix.Openat(int(root.Fd()), "taletty.sqlite3", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NOATIME, 0)
		if e == unix.ENOENT {
			fresh = true
			return nil
		}
		if e != nil {
			return ErrIO
		}
		f := os.NewFile(uintptr(fd), "store-header")
		defer f.Close()
		info, e := f.Stat()
		if e != nil {
			return ErrIO
		}
		if info.Size() == 0 {
			fresh = true
			return nil
		}
		var header [100]byte
		if _, e := io.ReadFull(f, header[:]); e != nil {
			return ErrInvalid
		}
		if !validHeader(header[:], info.Size()) {
			return ErrInvalid
		}
		appID := binary.BigEndian.Uint32(header[68:72])
		version := binary.BigEndian.Uint32(header[60:64])
		switch {
		case appID == 0 && version == 0:
			fresh = true
		case appID == 1413565524 && version == 1:
		case appID == 1413565524 && version > 1:
			return ErrVersion
		default:
			return ErrInvalid
		}
		return withSQLiteMode(ctx, root, true, func(c *sqlite3.Conn) error { return validateConnection(ctx, c, fresh) })
	})
	return fresh, err
}

// With no authorized recovery journal, a committed rollback-mode header must
// describe the entire file. Refuse appended/truncated bytes as well as invalid
// sizes, payload fractions, reserved expansion and stale page-count metadata.
func validHeader(h []byte, size int64) bool {
	pageSize := int64(binary.BigEndian.Uint16(h[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	return bytes.Equal(h[:16], []byte("SQLite format 3\x00")) &&
		pageSize >= 512 && pageSize <= 65536 && pageSize&(pageSize-1) == 0 &&
		size%pageSize == 0 && size/pageSize == int64(binary.BigEndian.Uint32(h[28:32])) &&
		h[18] == 1 && h[19] == 1 && h[21] == 64 && h[22] == 32 && h[23] == 32 &&
		bytes.Equal(h[24:28], h[92:96]) && bytes.Equal(h[72:92], make([]byte, 20))
}

func validateConnection(ctx context.Context, c *sqlite3.Conn, fresh bool) error {
	version, err := queryRows(c, "SELECT sqlite_version()")
	if err != nil {
		return sqliteValidationFailure(ctx)
	}
	if len(version) != 1 || !supportedSQLite(version[0][0]) {
		return ErrVersion
	}
	actual, err := schemaRows(c)
	if err != nil {
		return sqliteValidationFailure(ctx)
	}
	if fresh {
		if len(actual) != 0 {
			return ErrInvalid
		}
	} else {
		reference, e := sqlite3.OpenContext(ctx, ":memory:")
		if e != nil {
			return sqliteFailure(ctx)
		}
		defer reference.Close()
		reference.SetInterrupt(ctx)
		if reference.Exec(initialSQL) != nil {
			return sqliteFailure(ctx)
		}
		expected, e := schemaRows(reference)
		if e != nil {
			return sqliteFailure(ctx)
		}
		if !slices.EqualFunc(actual, expected, func(a, b []string) bool { return slices.Equal(a, b) }) {
			return ErrInvalid
		}
	}
	markers, err := queryRows(c, "SELECT application_id, user_version, foreign_keys FROM pragma_application_id, pragma_user_version, pragma_foreign_keys")
	expectedMarkers := []string{"1413565524", "1", "1"}
	if fresh {
		expectedMarkers = []string{"0", "0", "1"}
	}
	if err != nil || len(markers) != 1 || !slices.Equal(markers[0], expectedMarkers) {
		return sqliteValidationFailure(ctx)
	}
	integrity, err := queryRows(c, "PRAGMA integrity_check")
	if err != nil || len(integrity) != 1 || !slices.Equal(integrity[0], []string{"ok"}) {
		return sqliteValidationFailure(ctx)
	}
	foreign, err := queryRows(c, "PRAGMA foreign_key_check")
	if err != nil || len(foreign) != 0 {
		return sqliteValidationFailure(ctx)
	}
	return nil
}

func supportedSQLite(version string) bool {
	var major, minor, patch int
	_, err := fmt.Sscanf(version, "%d.%d.%d", &major, &minor, &patch)
	return err == nil && (major > 3 || major == 3 && minor >= 38)
}

func sqliteValidationFailure(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return ErrInvalid
}

func schemaRows(c *sqlite3.Conn) ([][]string, error) {
	return queryRows(c, "SELECT type,name,tbl_name,coalesce(sql,'') FROM sqlite_schema ORDER BY type,name,tbl_name")
}

func queryRows(c *sqlite3.Conn, sql string) (rows [][]string, err error) {
	stmt, _, err := c.Prepare(sql)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for stmt.Step() {
		row := make([]string, stmt.ColumnCount())
		for i := range row {
			row[i] = stmt.ColumnText(i)
		}
		rows = append(rows, row)
	}
	return rows, stmt.Err()
}
