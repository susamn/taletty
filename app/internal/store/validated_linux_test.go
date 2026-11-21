package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3"
	"golang.org/x/sys/unix"
)

func ownerFixture(t *testing.T) (string, *os.File, *Owner) {
	t.Helper()
	path, root := rootFixture(t)
	owner, err := NewOwner(context.Background(), root, func() error { return nil })
	must(t, err)
	t.Cleanup(func() { must(t, owner.Close(context.Background())) })
	return path, root, owner
}

func storeFixture(t *testing.T, owner RootOwner) *Store {
	t.Helper()
	s, err := Open(context.Background(), owner)
	must(t, err)
	t.Cleanup(func() { must(t, s.Close(context.Background())) })
	return s
}

// Snapshot through O_NOATIME: even atime/ctime/inode/link metadata must remain
// identical on refusals. No metadata restoration that would hide mutation.
type diskState struct {
	data string
	stat unix.Stat_t
}

func snapshot(t *testing.T, path string) map[string]diskState {
	t.Helper()
	result := make(map[string]diskState)
	for _, name := range []string{".", "taletty.sqlite3", "taletty.sqlite3-journal", "taletty.sqlite3-wal", "taletty.sqlite3-shm", "unknown"} {
		full := filepath.Join(path, name)
		var st unix.Stat_t
		err := unix.Lstat(full, &st)
		if err == unix.ENOENT {
			continue
		}
		must(t, err)
		state := diskState{stat: st}
		if st.Mode&unix.S_IFMT == unix.S_IFREG {
			fd, err := unix.Open(full, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOATIME, 0)
			must(t, err)
			f := os.NewFile(uintptr(fd), "snapshot")
			var b bytes.Buffer
			_, err = b.ReadFrom(f)
			must(t, err)
			must(t, f.Close())
			state.data = b.String()
		}
		result[name] = state
	}
	return result
}

func assertPreserved(t *testing.T, before, after map[string]diskState) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatal("files added/removed", before, after)
	}
	for name, want := range before {
		if after[name] != want {
			t.Fatalf("%s bytes/metadata changed: before=%+v after=%+v", name, want.stat, after[name].stat)
		}
	}
}

func TestValidatedFreshReopenAndAnchoring(t *testing.T) {
	must(t, Initialize())
	path, root, owner := ownerFixture(t)
	must(t, os.WriteFile(filepath.Join(path, "unknown"), []byte("untouched"), 0644))
	s := storeFixture(t, owner)
	must(t, s.withConn(context.Background(), func(c *sqlite3.Conn) error {
		if scalar(t, c, "PRAGMA foreign_keys") != "1" {
			t.Fatal("FK disabled")
		}
		return c.Exec("INSERT INTO settings VALUES(1,'plain',37)")
	}))
	moved := path + "-moved"
	t.Cleanup(func() { os.RemoveAll(moved) })
	must(t, os.Rename(path, moved))
	must(t, os.Mkdir(path, 0700))
	must(t, os.WriteFile(filepath.Join(path, "taletty.sqlite3"), []byte("replacement"), 0600))
	must(t, s.Check(context.Background()))
	must(t, s.Close(context.Background()))
	reopened := storeFixture(t, owner)
	must(t, reopened.withConn(context.Background(), func(c *sqlite3.Conn) error {
		if scalar(t, c, "SELECT volume_percent FROM settings") != "37" {
			t.Fatal("data lost")
		}
		if err := c.Exec("INSERT INTO favorite VALUES(999,0)"); !errors.Is(err, sqlite3.CONSTRAINT_FOREIGNKEY) {
			t.Fatal(err)
		}
		return nil
	}))
	must(t, WithRoot(context.Background(), root, func(context.Context, *os.File) error { return nil }))
	before := snapshot(t, moved)
	must(t, reopened.Check(context.Background()))
	assertPreserved(t, before, snapshot(t, moved))
	data, err := os.ReadFile(filepath.Join(path, "taletty.sqlite3"))
	must(t, err)
	if string(data) != "replacement" {
		t.Fatal("replacement root touched")
	}

	// Missing, zero-byte, and empty unversioned SQLite databases fail Check
	// without creating/mutating files, yet initialize cleanly via Open.
	for _, kind := range []string{"missing", "zero-byte", "empty-unversioned"} {
		t.Run("fresh/"+kind, func(t *testing.T) {
			freshPath, freshRoot, freshOwner := ownerFixture(t)
			switch kind {
			case "zero-byte":
				must(t, os.WriteFile(filepath.Join(freshPath, "taletty.sqlite3"), nil, 0600))
			case "empty-unversioned":
				must(t, withSQLite(context.Background(), freshRoot, func(c *sqlite3.Conn) error {
					return c.Exec("CREATE TABLE temp(x); DROP TABLE temp;")
				}))
			}
			beforeFresh := snapshot(t, freshPath)
			// Check via a Store bound to freshOwner without calling Open on freshOwner:
			// construct a Store directly to verify Check refuses uninitialized state.
			checkOnly := &Store{owner: freshOwner, token: make(chan struct{}, 1), ctx: context.Background(), cancel: func() {}}
			checkOnly.token <- struct{}{}
			if err := checkOnly.Check(context.Background()); err != ErrInvalid {
				t.Fatal(kind, err)
			}
			assertPreserved(t, beforeFresh, snapshot(t, freshPath))

			opened := storeFixture(t, freshOwner)
			must(t, opened.Check(context.Background()))
			must(t, opened.withConn(context.Background(), func(c *sqlite3.Conn) error {
				return c.Exec("INSERT INTO settings VALUES(1,'unicode',42)")
			}))
		})
	}

	// Valid v1 database with 65536-byte page size (header pageSize == 1).
	_, bigRoot, bigOwner := ownerFixture(t)
	must(t, withSQLite(context.Background(), bigRoot, func(c *sqlite3.Conn) error {
		return c.Exec("PRAGMA page_size=65536;\n" + initialSQL)
	}))
	bigStore := storeFixture(t, bigOwner)
	must(t, bigStore.Check(context.Background()))
}

func TestValidatedRefusalPreservesEveryByteAndMetadata(t *testing.T) {
	cases := map[string]struct {
		sql  string
		want error
	}{
		"foreign":         {sql: "CREATE TABLE private(value TEXT); INSERT INTO private VALUES('keep'); PRAGMA application_id=123", want: ErrInvalid},
		"unversioned":     {sql: "CREATE TABLE private(value TEXT); INSERT INTO private VALUES('keep')", want: ErrInvalid},
		"sqlite-sequence": {sql: "CREATE TABLE seq(id INTEGER PRIMARY KEY AUTOINCREMENT); DROP TABLE seq", want: ErrInvalid},
		"too-new":         {sql: initialSQL + "PRAGMA user_version=2", want: ErrVersion},
		"wrong-version":   {sql: initialSQL + "PRAGMA user_version=0", want: ErrInvalid},
		"wrong-app":       {sql: initialSQL + "PRAGMA application_id=17", want: ErrInvalid},
		"zero-app-v1":     {sql: initialSQL + "PRAGMA application_id=0", want: ErrInvalid},
		"forged-empty":    {sql: "PRAGMA application_id=1413565524; PRAGMA user_version=1; CREATE TABLE fake(x)", want: ErrInvalid},
		"forged-check":    {sql: initialSQL + "PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql=replace(sql,'volume_percent BETWEEN 0 AND 100','volume_percent BETWEEN 0 AND 200') WHERE name='settings'; PRAGMA writable_schema=OFF", want: ErrInvalid},
		"missing-index":   {sql: initialSQL + "DROP INDEX one_active_download", want: ErrInvalid},
		"extra-trigger":   {sql: initialSQL + "CREATE TRIGGER hidden AFTER INSERT ON settings BEGIN DELETE FROM favorite; END", want: ErrInvalid},
		"fk-violation":    {sql: initialSQL + "PRAGMA foreign_keys=OFF; INSERT INTO favorite VALUES(999,0)", want: ErrInvalid},
		"check-violation": {sql: initialSQL + "PRAGMA ignore_check_constraints=ON; INSERT INTO settings VALUES(1,'plain',200)", want: ErrInvalid},
		"broken-schema":   {sql: initialSQL + "PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql='CREATE TABLE broken(' WHERE name='settings'", want: ErrInvalid},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path, root, owner := ownerFixture(t)
			must(t, withSQLite(context.Background(), root, func(c *sqlite3.Conn) error { return c.Exec(tc.sql) }))
			must(t, os.WriteFile(filepath.Join(path, "unknown"), []byte("private unknown data"), 0600))
			before := snapshot(t, path)
			s, err := Open(context.Background(), owner)
			if s != nil || err != tc.want {
				t.Fatalf("%s: got (%v, %v), want (nil, %v)", name, s, err, tc.want)
			}
			checkOnly := &Store{owner: owner, token: make(chan struct{}, 1), ctx: context.Background(), cancel: func() {}}
			checkOnly.token <- struct{}{}
			if err := checkOnly.Check(context.Background()); err != tc.want {
				t.Fatalf("%s Check: got %v, want %v", name, err, tc.want)
			}
			assertPreserved(t, before, snapshot(t, path))
		})
	}
	for _, kind := range []string{
		"garbage", "short", "corrupt-page", "wal-header", "appended",
		"invalid-page-size", "invalid-65536-size", "stale-change-counter",
		"nonzero-reserved", "bad-payload-fraction", "zero-page-count",
	} {
		t.Run(kind, func(t *testing.T) {
			path, root, owner := ownerFixture(t)
			if kind != "garbage" && kind != "short" {
				initSQL := initialSQL
				if kind == "invalid-65536-size" {
					initSQL = "PRAGMA page_size=65536;\n" + initialSQL
				}
				must(t, withSQLite(context.Background(), root, func(c *sqlite3.Conn) error { return c.Exec(initSQL) }))
				f, err := os.OpenFile(filepath.Join(path, "taletty.sqlite3"), os.O_RDWR, 0)
				must(t, err)
				switch kind {
				case "corrupt-page":
					_, err = f.WriteAt([]byte{255}, 100)
				case "wal-header":
					_, err = f.WriteAt([]byte{2, 2}, 18)
				case "invalid-page-size":
					_, err = f.WriteAt([]byte{0, 3}, 16)
				case "stale-change-counter":
					_, err = f.WriteAt([]byte{0x12, 0x34, 0x56, 0x78}, 92)
				case "nonzero-reserved":
					_, err = f.WriteAt([]byte{1}, 72)
				case "bad-payload-fraction":
					_, err = f.WriteAt([]byte{63}, 21)
				case "zero-page-count":
					_, err = f.WriteAt([]byte{0, 0, 0, 0}, 28)
				case "appended", "invalid-65536-size":
					_, err = f.Seek(0, 2)
					must(t, err)
					_, err = f.Write([]byte("unknown appended data"))
				}
				must(t, err)
				must(t, f.Close())
			} else {
				data := []byte("private unknown bytes")
				if kind == "short" {
					data = []byte("SQLite format 3\x00")
				}
				must(t, os.WriteFile(filepath.Join(path, "taletty.sqlite3"), data, 0600))
			}
			before := snapshot(t, path)
			if _, err := Open(context.Background(), owner); err != ErrInvalid {
				t.Fatal(kind, err)
			}
			checkOnly := &Store{owner: owner, token: make(chan struct{}, 1), ctx: context.Background(), cancel: func() {}}
			checkOnly.token <- struct{}{}
			if err := checkOnly.Check(context.Background()); err != ErrInvalid {
				t.Fatal(kind, err)
			}
			assertPreserved(t, before, snapshot(t, path))
		})
	}

	// Open refuses canceled context, closed owner, unsafe root, and unsafe leaf
	// without creating or modifying files.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	openPath, _, openOwner := ownerFixture(t)
	if _, err := Open(canceled, openOwner); err != context.Canceled {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(openPath)
	must(t, err)
	if len(entries) != 0 {
		t.Fatal("canceled Open created files", entries)
	}
	must(t, os.WriteFile(filepath.Join(openPath, "taletty.sqlite3"), []byte("keep"), 0644))
	beforeUnsafe := snapshot(t, openPath)
	if _, err := Open(context.Background(), openOwner); err != ErrUnsafeFile {
		t.Fatal(err)
	}
	assertPreserved(t, beforeUnsafe, snapshot(t, openPath))
	must(t, os.Chmod(openPath, 0755))
	if _, err := Open(context.Background(), openOwner); err != ErrUnsafeRoot {
		t.Fatal(err)
	}
	must(t, os.Chmod(openPath, 0700))
	must(t, os.Remove(filepath.Join(openPath, "taletty.sqlite3")))
	must(t, openOwner.Close(context.Background()))
	if _, err := Open(context.Background(), openOwner); err != ErrClosed {
		t.Fatal(err)
	}
}

func TestValidatedSidecarsNeverRecoverOrSweep(t *testing.T) {
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		for _, data := range []string{"", "unknown journal bytes"} {
			t.Run(suffix+data, func(t *testing.T) {
				path, _, owner := ownerFixture(t)
				s := storeFixture(t, owner)
				must(t, os.WriteFile(filepath.Join(path, "taletty.sqlite3"+suffix), []byte(data), 0600))
				before := snapshot(t, path)
				if _, err := Open(context.Background(), owner); err != ErrRecovery {
					t.Fatal(err)
				}
				if err := s.Check(context.Background()); err != ErrRecovery {
					t.Fatal(err)
				}
				if err := s.withConn(context.Background(), func(*sqlite3.Conn) error { t.Error("callback reached"); return nil }); err != ErrRecovery {
					t.Fatal(err)
				}
				assertPreserved(t, before, snapshot(t, path))
			})
		}
	}
}

func TestValidatedRealHotJournalRefused(t *testing.T) {
	path, _, owner := ownerFixture(t)
	s := storeFixture(t, owner)
	must(t, s.withConn(context.Background(), func(c *sqlite3.Conn) error { return c.Exec("INSERT INTO settings VALUES(1,'plain',50)") }))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestValidatedHotChild$")
	cmd.Env = append(os.Environ(), "TALETTY_HOT_CHILD="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hot fixture: %v %s", err, output)
	}
	before := snapshot(t, path)
	if len(before["taletty.sqlite3-journal"].data) < 28 {
		t.Fatal("no hot journal")
	}
	if _, err := Open(context.Background(), owner); err != ErrRecovery {
		t.Fatal(err)
	}
	if err := s.Check(context.Background()); err != ErrRecovery {
		t.Fatal(err)
	}
	assertPreserved(t, before, snapshot(t, path))
	// Only the test explicitly authorizes low-level recovery, proving that this
	// fixture was recoverable and that public refusal was meaningful.
	must(t, withSQLite(context.Background(), owner.root, func(c *sqlite3.Conn) error {
		if scalar(t, c, "SELECT volume_percent FROM settings") != "50" {
			t.Fatal("not recovered")
		}
		return nil
	}))
	must(t, s.Check(context.Background()))
}

func TestValidatedHotChild(t *testing.T) {
	path := os.Getenv("TALETTY_HOT_CHILD")
	if path == "" {
		return
	}
	root, err := os.Open(path)
	must(t, err)
	must(t, withSQLite(context.Background(), root, func(c *sqlite3.Conn) error {
		must(t, c.Exec("BEGIN IMMEDIATE; UPDATE settings SET volume_percent=99"))
		must(t, c.CacheFlush())
		os.Exit(0) // abrupt process exit: no connection/journal cleanup
		return nil
	}))
}

func TestValidatedMigrationRollbackRetryAndEmptyDatabase(t *testing.T) {
	must(t, Initialize())
	_, root, owner := ownerFixture(t)
	broken := strings.Replace(initialSQL, "COMMIT;", "INSERT INTO absent VALUES(1); COMMIT;", 1)
	if err := openValidated(context.Background(), root, broken); err != ErrDatabase {
		t.Fatal(err)
	}
	must(t, withSQLite(context.Background(), root, func(c *sqlite3.Conn) error {
		if scalar(t, c, "PRAGMA user_version") != "0" || scalar(t, c, "SELECT count(*) FROM sqlite_schema") != "0" {
			t.Fatal("partial migration survived")
		}
		return nil
	}))
	// Cancellation mid-initialization script must also roll back DDL cleanly.
	cancelScript := "BEGIN IMMEDIATE; CREATE TABLE partial(x); WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n) SELECT sum(x) FROM n; COMMIT;"
	cancelCtx, cancelTimeout := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelTimeout()
	if err := openValidated(cancelCtx, root, cancelScript); err != context.DeadlineExceeded {
		t.Fatal(err)
	}
	must(t, withSQLite(context.Background(), root, func(c *sqlite3.Conn) error {
		if scalar(t, c, "PRAGMA user_version") != "0" || scalar(t, c, "SELECT count(*) FROM sqlite_schema") != "0" {
			t.Fatal("canceled migration survived")
		}
		return nil
	}))
	s := storeFixture(t, owner)
	must(t, s.Check(context.Background()))
	if err := s.withConn(context.Background(), func(c *sqlite3.Conn) error {
		must(t, c.Exec("BEGIN IMMEDIATE; INSERT INTO settings VALUES(1,'plain',9)"))
		return errors.New("private SQLite diagnostics must not escape")
	}); err != ErrDatabase {
		t.Fatal(err)
	}
	if err := s.withConn(context.Background(), func(c *sqlite3.Conn) error {
		_, _, err := c.Prepare("SELECT 1")
		return err
	}); err != ErrDatabase {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() != "conn-panic" {
				t.Error("panic lost")
			}
		}()
		_ = s.withConn(context.Background(), func(c *sqlite3.Conn) error {
			must(t, c.Exec("BEGIN IMMEDIATE; INSERT INTO settings VALUES(1,'plain',9)"))
			panic("conn-panic")
		})
	}()
	must(t, s.withConn(context.Background(), func(c *sqlite3.Conn) error {
		if scalar(t, c, "SELECT count(*) FROM settings") != "0" {
			t.Fatal("failed transaction survived")
		}
		return nil
	}))
}

func TestStoreCloseCancelQueuedPanicAndOwnerRaces(t *testing.T) {
	_, _, owner := ownerFixture(t)
	s := storeFixture(t, owner)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.run(context.Background(), func(ctx context.Context, _ *os.File) error {
			close(entered)
			<-ctx.Done()
			<-release
			return ctx.Err()
		})
	}()
	<-entered
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Close(canceled); err != context.Canceled {
		t.Fatal(err)
	}
	if err := s.Check(canceled); err != context.Canceled {
		t.Fatal(err)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer waitCancel()
	if err := s.Close(waitCtx); err != context.DeadlineExceeded {
		t.Fatal(err)
	}
	if err := s.Check(context.Background()); err != ErrClosed {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != context.Canceled {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := s.Close(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for range 16 {
		if err := s.Check(context.Background()); err != ErrClosed {
			t.Fatal(err)
		}
	}
	s = storeFixture(t, owner)
	func() {
		defer func() {
			if recover() != "panic" {
				t.Error("panic lost")
			}
		}()
		_ = s.run(context.Background(), func(context.Context, *os.File) error { panic("panic") })
	}()
	must(t, s.Check(context.Background()))
	// Owner cancellation also interrupts a real SQL query, not just a fake callback.
	entered = make(chan struct{})
	done = make(chan error, 1)
	go func() {
		done <- s.withConn(context.Background(), func(c *sqlite3.Conn) error {
			close(entered)
			return c.Exec("WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n) SELECT sum(x) FROM n")
		})
	}()
	<-entered
	must(t, owner.Close(context.Background()))
	if err := <-done; err != context.Canceled {
		t.Fatal(err)
	}
	for range 10 {
		if err := s.Check(context.Background()); err != ErrClosed {
			t.Fatal(err)
		}
	}
}

func TestOwnerLeaseIndependentOfAuthenticationGateAndRelease(t *testing.T) {
	path, root := rootFixture(t)
	lock, err := os.OpenFile(filepath.Join(path, ".auth.lock"), os.O_RDWR|os.O_CREATE, 0600)
	must(t, err)
	defer lock.Close()
	must(t, unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB))
	var released atomic.Int32
	owner, err := NewOwner(context.Background(), root, func() error {
		released.Add(1)
		return unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	})
	must(t, err)
	authLease, err := owner.Lease(context.Background())
	must(t, err)
	// The external authenticated gate is occupied for this entire DB lifecycle.
	// Local checkpoints do not enter it, nor release/reacquire .auth.lock.
	authGate := make(chan struct{}, 1)
	authGate <- struct{}{}
	s := storeFixture(t, owner)
	must(t, s.withConn(context.Background(), func(c *sqlite3.Conn) error { return c.Exec("INSERT INTO settings VALUES(1,'plain',60)") }))
	must(t, s.Close(context.Background()))
	other, err := os.OpenFile(filepath.Join(path, ".auth.lock"), os.O_RDWR, 0)
	must(t, err)
	defer other.Close()
	if err := unix.Flock(int(other.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != unix.EWOULDBLOCK {
		t.Fatal("owner unlocked", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := owner.Close(ctx); err != context.DeadlineExceeded {
		t.Fatal(err)
	}
	if authLease.Context.Err() != context.Canceled || released.Load() != 0 {
		t.Fatal("lifetime released before join")
	}
	if _, err := owner.Lease(context.Background()); err != ErrClosed {
		t.Fatal(err)
	}
	<-authGate
	authLease.Close()
	authLease.Close()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := owner.Close(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if released.Load() != 1 {
		t.Fatal("release count", released.Load())
	}
	if _, err := owner.Lease(context.Background()); err != ErrClosed {
		t.Fatal(err)
	}
	must(t, unix.Flock(int(other.Fd()), unix.LOCK_EX|unix.LOCK_NB))

	// Caller cancellation of one lease does not close Owner or affect other leases.
	_, root2 := rootFixture(t)
	owner2, err := NewOwner(context.Background(), root2, func() error { return nil })
	must(t, err)
	defer owner2.Close(context.Background())
	leaseCtx, cancelLease := context.WithCancel(context.Background())
	l1, err := owner2.Lease(leaseCtx)
	must(t, err)
	l2, err := owner2.Lease(context.Background())
	must(t, err)
	cancelLease()
	if l1.Context.Err() != context.Canceled || l2.Context.Err() != nil {
		t.Fatal("lease cancellation leaked across leases")
	}
	l1.Close()
	l2.Close()
}

func TestOwnerAndValidationFailures(t *testing.T) {
	path, root := rootFixture(t)
	if _, err := NewOwner(context.Background(), root, nil); err != ErrUnsafeRoot {
		t.Fatal(err)
	}
	var failedRelease atomic.Int32
	canceledCtx, cancelInit := context.WithCancel(context.Background())
	cancelInit()
	if _, err := NewOwner(canceledCtx, root, func() error { failedRelease.Add(1); return nil }); err != context.Canceled {
		t.Fatal(err)
	}
	must(t, os.Chmod(path, 0755))
	if _, err := NewOwner(context.Background(), root, func() error { failedRelease.Add(1); return nil }); err != ErrUnsafeRoot {
		t.Fatal(err)
	}
	must(t, os.Chmod(path, 0700))
	if failedRelease.Load() != 0 {
		t.Fatal("release called on failed NewOwner")
	}
	if _, err := NewOwner(context.Background(), nil, func() error { return nil }); err != ErrUnsafeRoot {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), nil); err != ErrUnsafeRoot {
		t.Fatal(err)
	}
	owner, err := NewOwner(context.Background(), root, func() error { return errors.New("private error") })
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := owner.Lease(ctx); err != context.Canceled {
		t.Fatal(err)
	}
	if err := owner.Close(ctx); err != context.Canceled {
		t.Fatal(err)
	}
	if err := owner.Close(context.Background()); err != ErrIO {
		t.Fatal(err)
	}
	if err := owner.Close(ctx); err != context.Canceled {
		t.Fatal(err)
	}
	if err := owner.Close(context.Background()); err != ErrIO {
		t.Fatal(err)
	}
	// Even if owner.root fails to close, release still runs once and ErrIO is returned.
	_, rootBad := rootFixture(t)
	var badReleased atomic.Int32
	ownerBad, err := NewOwner(context.Background(), rootBad, func() error { badReleased.Add(1); return nil })
	must(t, err)
	must(t, ownerBad.root.Close())
	if err := ownerBad.Close(context.Background()); err != ErrIO {
		t.Fatal(err)
	}
	if badReleased.Load() != 1 {
		t.Fatal("release skipped on root close error", badReleased.Load())
	}
	for version, want := range map[string]bool{"3.38.0": true, "3.51.3": true, "4.0.0": true, "3.37.9": false, "broken": false} {
		if supportedSQLite(version) != want {
			t.Fatal(version)
		}
	}
	// Direct validHeader boundary checks.
	baseHeader := func() []byte {
		h := make([]byte, 100)
		copy(h[:16], "SQLite format 3\x00")
		binary.BigEndian.PutUint16(h[16:18], 4096)
		h[18], h[19], h[21], h[22], h[23] = 1, 1, 64, 32, 32
		binary.BigEndian.PutUint32(h[24:28], 7)
		binary.BigEndian.PutUint32(h[28:32], 1)
		binary.BigEndian.PutUint32(h[92:96], 7)
		return h
	}
	if !validHeader(baseHeader(), 4096) {
		t.Fatal("valid 4096 header rejected")
	}
	h64k := baseHeader()
	binary.BigEndian.PutUint16(h64k[16:18], 1)
	if !validHeader(h64k, 65536) || validHeader(h64k, 4096) {
		t.Fatal("65536 page size handling failed")
	}
	for _, mutate := range []func([]byte){
		func(h []byte) { binary.BigEndian.PutUint16(h[16:18], 0) },
		func(h []byte) { binary.BigEndian.PutUint16(h[16:18], 256) },
		func(h []byte) { binary.BigEndian.PutUint16(h[16:18], 768) },
		func(h []byte) { h[18] = 2 },
		func(h []byte) { h[19] = 2 },
		func(h []byte) { h[22] = 31 },
		func(h []byte) { h[23] = 31 },
		func(h []byte) { h[92]++ },
		func(h []byte) { h[91] = 1 },
	} {
		h := baseHeader()
		mutate(h)
		if validHeader(h, 4096) {
			t.Fatal("invalid header accepted")
		}
	}
	must(t, os.WriteFile(filepath.Join(path, "taletty.sqlite3"), nil, 0600))
	fresh, err := inspectDatabase(context.Background(), root)
	must(t, err)
	if !fresh {
		t.Fatal("empty file not fresh")
	}
	owner2, err := NewOwner(context.Background(), root, func() error { return nil })
	must(t, err)
	defer owner2.Close(context.Background())
	s := storeFixture(t, owner2)
	must(t, os.Remove(filepath.Join(path, "taletty.sqlite3")))
	if err := s.withConn(context.Background(), func(*sqlite3.Conn) error { return nil }); err != ErrInvalid {
		t.Fatal(err)
	}
}
