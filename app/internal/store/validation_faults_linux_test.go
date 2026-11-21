package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/vfs"
)

func TestInspectionCannotWriteOrCreate(t *testing.T) {
	path, root, owner := ownerFixture(t)
	s := storeFixture(t, owner)
	before := snapshot(t, path)
	must(t, withSQLiteMode(context.Background(), root, true, func(c *sqlite3.Conn) error {
		for _, sql := range []string{"INSERT INTO settings VALUES(1,'plain',8)", "PRAGMA user_version=9", "CREATE TABLE forbidden(x)"} {
			if err := c.Exec(sql); err == nil {
				t.Fatal("inspection wrote", sql)
			}
		}
		return nil
	}))
	assertPreserved(t, before, snapshot(t, path))
	must(t, s.Check(context.Background()))
	// Exercise the final write barriers directly too: SQLite normally fails at
	// the earlier journal-creation barrier, but cannot bypass these methods.
	f, err := os.Open(filepath.Join(path, "taletty.sqlite3"))
	must(t, err)
	inspection := &inspectionFile{reader: f}
	if _, err := inspection.WriteAt([]byte("no"), 0); err != ErrUnsafeFile {
		t.Fatal(err)
	}
	if err := inspection.Truncate(0); err != ErrUnsafeFile {
		t.Fatal(err)
	}
	must(t, f.Close())
}

func TestValidationConnectionFailuresAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c, err := sqlite3.Open(":memory:")
	must(t, err)
	defer c.Close()
	must(t, c.Exec(initialSQL))
	// Full v1 and the expected markers must agree, even when called without the
	// raw-header guard (defense in depth for initialization).
	must(t, c.Exec("PRAGMA user_version=2"))
	if err := validateConnection(context.Background(), c, false); err != ErrInvalid {
		t.Fatal(err)
	}
	if err := validateConnection(context.Background(), c, true); err != ErrInvalid {
		t.Fatal(err)
	}
	must(t, c.Exec("PRAGMA user_version=1"))
	cancel()
	c.SetInterrupt(ctx)
	if err := validateConnection(ctx, c, false); err != context.Canceled {
		t.Fatal(err)
	}
	if sqliteValidationFailure(ctx) != context.Canceled {
		t.Fatal("lost cancellation")
	}
	c.SetInterrupt(context.Background())
	path, _, owner := ownerFixture(t)
	s := storeFixture(t, owner)
	// The query is actually waiting in the application gate when canceled; it
	// must never acquire a root lease or enter the callback afterward.
	entered := make(chan struct{})
	release := make(chan struct{})
	active := make(chan error, 1)
	go func() {
		active <- s.run(context.Background(), func(context.Context, *os.File) error { close(entered); <-release; return nil })
	}()
	<-entered
	queued, cancelQueued := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelQueued()
	if err := s.withConn(queued, func(*sqlite3.Conn) error { t.Error("canceled work entered"); return nil }); err != context.DeadlineExceeded {
		t.Fatal(err)
	}
	close(release)
	must(t, <-active)
	must(t, s.Check(context.Background()))
	// Close leaves a missing database absent instead of silently reinitializing.
	must(t, os.Remove(filepath.Join(path, "taletty.sqlite3")))
	if err := s.Check(context.Background()); err != ErrInvalid {
		t.Fatal(err)
	}
	// Inspection mode refuses to create a missing database.
	if err := withSQLiteMode(context.Background(), owner.root, true, func(*sqlite3.Conn) error {
		t.Error("inspection opened missing DB")
		return nil
	}); err != ErrDatabase {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(path, "taletty.sqlite3")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inspection created missing DB", err)
	}
	// Replacing DB with a zero-byte file fails withConn without reinitializing.
	must(t, os.WriteFile(filepath.Join(path, "taletty.sqlite3"), nil, 0600))
	if err := s.withConn(context.Background(), func(*sqlite3.Conn) error {
		t.Error("withConn entered on zero-byte DB")
		return nil
	}); err != ErrInvalid {
		t.Fatal(err)
	}
}

func TestValidatedPublicContentionRefusesLiveJournalAndRetries(t *testing.T) {
	_, _, owner := ownerFixture(t)
	first := storeFixture(t, owner)
	second := storeFixture(t, owner)
	must(t, first.withConn(context.Background(), func(c *sqlite3.Conn) error {
		must(t, c.Exec("BEGIN IMMEDIATE; INSERT INTO settings VALUES(1,'plain',11)"))
		start := time.Now()
		// A second Store cannot attribute the first one's journal. Fail closed
		// rather than auto-recovering it; lower transport tests cover real BUSY.
		if err := second.withConn(context.Background(), func(other *sqlite3.Conn) error { return other.Exec("BEGIN IMMEDIATE") }); err != ErrRecovery {
			t.Fatal(err)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatal("unbounded busy")
		}
		return c.Exec("ROLLBACK")
	}))
	must(t, second.withConn(context.Background(), func(c *sqlite3.Conn) error { return c.Exec("INSERT INTO settings VALUES(1,'plain',12)") }))
}

func TestInspectionFileCloseFailures(t *testing.T) {
	// Reuse real OS files for cleanup faults instead of changing global syscalls.
	path, root := rootFixture(t)
	must(t, withSQLite(context.Background(), root, func(c *sqlite3.Conn) error { return c.Exec(initialSQL) }))
	fs := &sqliteVFS{root: root, path: filepath.Join("/proc/self/fd", "unused"), readOnly: true}
	if err := fs.Delete(fs.path+"-journal", false); err != ErrUnsafeFile {
		t.Fatal(err)
	}
	reader, err := os.Open(filepath.Join(path, "taletty.sqlite3"))
	must(t, err)
	must(t, reader.Close())
	// vfs.File's complete method set is already supplied by sqliteFile. Close
	// fault testing only needs a real file close result, not a fake SQLite engine.
	f := &inspectionFile{File: &closeFile{err: errors.New("private")}, reader: reader}
	if err := f.Close(); err != ErrIO {
		t.Fatal(err)
	}
	// Reader succeeds while underlying File.Close fails -> ErrIO.
	reader2, err := os.Open(filepath.Join(path, "taletty.sqlite3"))
	must(t, err)
	f2 := &inspectionFile{File: &closeFile{err: errors.New("private")}, reader: reader2}
	if err := f2.Close(); err != ErrIO {
		t.Fatal(err)
	}
	// Reader fails while underlying File.Close succeeds -> ErrIO.
	f3 := &inspectionFile{File: &closeFile{err: nil}, reader: reader}
	if err := f3.Close(); err != ErrIO {
		t.Fatal(err)
	}
}

type closeFile struct {
	vfs.File
	err error
}

func (f *closeFile) Close() error { return f.err }
