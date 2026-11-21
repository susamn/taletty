package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/vfs"
	"golang.org/x/sys/unix"
)

//go:embed schema/001_initial.sql
var initialSQL string

func scalar(t *testing.T, conn *sqlite3.Conn, query string) string {
	t.Helper()
	stmt, _, err := conn.Prepare(query)
	must(t, err)
	defer stmt.Close()
	if !stmt.Step() {
		t.Fatal("no row", stmt.Err())
	}
	return stmt.ColumnText(0)
}

func TestSQLitePrivateCreateRenameReopen(t *testing.T) {
	root, borrowed := rootFixture(t)
	moved := root + "-moved"
	t.Cleanup(func() { os.RemoveAll(moved) })
	must(t, os.WriteFile(filepath.Join(root, "unknown"), []byte("preserve"), 0644))
	must(t, withSQLite(context.Background(), borrowed, func(c *sqlite3.Conn) error {
		t.Log("production SQLite", scalar(t, c, "SELECT sqlite_version()"))
		must(t, c.Exec(initialSQL)) // exact merged schema, not another DDL definition
		if scalar(t, c, "PRAGMA foreign_keys") != "1" || scalar(t, c, "PRAGMA temp_store") != "2" {
			t.Fatal("connection pragmas not applied")
		}
		if err := c.Exec("INSERT INTO favorite VALUES(999, 1)"); !errors.Is(err, sqlite3.CONSTRAINT_FOREIGNKEY) {
			t.Fatal("foreign keys not enforced", err)
		}
		if mode := scalar(t, c, "PRAGMA journal_mode=WAL"); mode != "delete" {
			t.Fatal("WAL enabled", mode)
		}
		must(t, c.Exec("INSERT INTO settings VALUES(1, 'plain', 80); BEGIN IMMEDIATE; UPDATE settings SET volume_percent=20;"))
		for _, leaf := range []string{"taletty.sqlite3", "taletty.sqlite3-journal"} {
			var stat unix.Stat_t
			must(t, unix.Lstat(filepath.Join(root, leaf), &stat))
			if !privateFile(&stat) {
				t.Fatal("SQLite exposed nonprivate file", leaf, stat)
			}
		}
		must(t, os.Rename(root, moved))
		must(t, os.Mkdir(root, 0700))
		must(t, os.Symlink("absent", filepath.Join(root, "taletty.sqlite3")))
		must(t, c.Exec("ROLLBACK; BEGIN IMMEDIATE; UPDATE settings SET volume_percent=70; COMMIT;"))
		// No ATTACH, external temp DB or arbitrary file access through this VFS.
		if err := c.Exec("ATTACH 'outside.sqlite3' AS outside"); err == nil {
			t.Fatal("arbitrary path opened")
		}
		return nil
	}))
	must(t, withSQLite(context.Background(), borrowed, func(c *sqlite3.Conn) error {
		if scalar(t, c, "SELECT volume_percent FROM settings") != "70" || scalar(t, c, "PRAGMA integrity_check") != "ok" {
			t.Fatal("anchored data not durable")
		}
		if scalar(t, c, "PRAGMA application_id") != "1413565524" || scalar(t, c, "PRAGMA user_version") != "1" {
			t.Fatal("source SQL markers not retained")
		}
		return nil
	}))
	entries, err := os.ReadDir(moved)
	must(t, err)
	if len(entries) != 2 {
		t.Fatal("sidecar leaked or unknown file removed", entries)
	}
	data, err := os.ReadFile(filepath.Join(moved, "unknown"))
	must(t, err)
	if string(data) != "preserve" {
		t.Fatal("unknown data changed")
	}
}

func TestSQLiteRollbackPanicCancelAndStatementRecovery(t *testing.T) {
	_, root := rootFixture(t)
	must(t, withSQLite(context.Background(), root, func(c *sqlite3.Conn) error { return c.Exec(initialSQL) }))
	failure := errors.New("operation failed")
	for _, outcome := range []string{"error", "panic", "cancel", "statement"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			func() {
				if outcome == "panic" {
					defer func() {
						if recover() != failure {
							t.Error("panic changed")
						}
					}()
				}
				err := withSQLite(ctx, root, func(c *sqlite3.Conn) error {
					must(t, c.Exec("BEGIN IMMEDIATE; INSERT INTO settings VALUES(1, 'plain', 50)"))
					switch outcome {
					case "panic":
						panic(failure)
					case "cancel":
						cancel()
						if err := c.Exec("SELECT 1"); !errors.Is(err, sqlite3.INTERRUPT) {
							t.Fatal("cancellation ignored", err)
						}
						return ctx.Err()
					case "statement":
						_, _, err := c.Prepare("SELECT 1") // forgotten statement must be finalized
						return err
					}
					return failure
				})
				want := map[string]error{"error": failure, "cancel": context.Canceled, "statement": ErrDatabase}[outcome]
				if err != want {
					t.Fatal("wrong outcome", err, want)
				}
			}()
			must(t, withSQLite(context.Background(), root, func(c *sqlite3.Conn) error {
				if scalar(t, c, "SELECT count(*) FROM settings") != "0" {
					t.Fatal("transaction was not rolled back on close")
				}
				return c.Exec("BEGIN IMMEDIATE; ROLLBACK")
			}))
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := withSQLite(ctx, root, nil); err != context.Canceled {
		t.Fatal(err)
	}
	if sqliteFailure(ctx) != context.Canceled || sqliteFailure(context.Background()) != ErrDatabase {
		t.Fatal("unsafe error mapping")
	}
}

func TestSQLitePreflightPreservesUnsafeLeaves(t *testing.T) {
	// Reuse the complete WithRoot fixture matrix; test the transport boundary too.
	for _, leaf := range []string{"taletty.sqlite3", "taletty.sqlite3-journal", "taletty.sqlite3-wal", "taletty.sqlite3-shm"} {
		for _, kind := range []string{"symlink", "hardlink", "fifo", "public", "private-wal"} {
			if kind == "private-wal" && leaf != "taletty.sqlite3-wal" && leaf != "taletty.sqlite3-shm" {
				continue
			}
			t.Run(leaf+"/"+kind, func(t *testing.T) {
				path, root := rootFixture(t)
				outside := filepath.Join(t.TempDir(), "outside")
				must(t, os.WriteFile(outside, []byte("unchanged"), 0600))
				name := filepath.Join(path, leaf)
				switch kind {
				case "symlink":
					must(t, os.Symlink(outside, name))
				case "hardlink":
					must(t, os.Link(outside, name))
				case "fifo":
					must(t, unix.Mkfifo(name, 0600))
				default:
					must(t, os.WriteFile(name, []byte("preserve"), 0600))
					if kind == "public" {
						must(t, os.Chmod(name, 0644))
					}
				}
				var before, after unix.Stat_t
				must(t, unix.Lstat(name, &before))
				if err := withSQLite(context.Background(), root, func(*sqlite3.Conn) error { t.Error("unsafe callback"); return nil }); err != ErrUnsafeFile {
					t.Fatal(err)
				}
				must(t, unix.Lstat(name, &after))
				if before != after {
					t.Fatal("unsafe leaf metadata changed")
				}
				data, err := os.ReadFile(outside)
				must(t, err)
				if string(data) != "unchanged" {
					t.Fatal("target changed")
				}
			})
		}
	}
}

func TestSQLiteBusyAcrossConnectionsAndProcess(t *testing.T) {
	path, root := rootFixture(t)
	must(t, withSQLite(context.Background(), root, func(c *sqlite3.Conn) error {
		must(t, c.Exec(initialSQL+"\nBEGIN IMMEDIATE; INSERT INTO settings VALUES(1, 'plain', 20);"))
		// Separate real connection, no sleeps or scheduler assumptions: parent
		// already holds the writer lock before the contender is opened.
		must(t, withSQLite(context.Background(), root, func(other *sqlite3.Conn) error {
			start := time.Now()
			if err := other.Exec("BEGIN IMMEDIATE"); !errors.Is(err, sqlite3.BUSY) {
				t.Fatal("missing lock contention", err)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("busy timeout unbounded")
			}
			return nil
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSQLiteBusyChild$")
		cmd.Env = append(os.Environ(), "TALETTY_SQLITE_CHILD="+path)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("process contender: %v\n%s", err, output)
		}
		ctx, cancelWait := context.WithCancel(context.Background())
		defer cancelWait()
		must(t, withSQLite(ctx, root, func(other *sqlite3.Conn) error {
			must(t, other.BusyHandler(func(_ context.Context, _ int) bool {
				cancelWait() // actual lock contention is the cancellation barrier
				return true
			}))
			if err := other.Exec("BEGIN IMMEDIATE"); !errors.Is(err, sqlite3.INTERRUPT) && !errors.Is(err, sqlite3.BUSY) {
				t.Fatal("canceled wait", err)
			}
			if ctx.Err() != context.Canceled {
				t.Fatal("busy handler did not observe contention")
			}
			return nil
		}))
		return c.Exec("ROLLBACK")
	}))
	must(t, withSQLite(context.Background(), root, func(c *sqlite3.Conn) error {
		return c.Exec("BEGIN IMMEDIATE; INSERT INTO settings VALUES(1, 'plain', 30); COMMIT")
	}))
}

func TestSQLiteFreshMigrationRollbackAndLateSidecarRefusal(t *testing.T) {
	path, root := rootFixture(t)
	must(t, withSQLite(context.Background(), root, func(c *sqlite3.Conn) error {
		broken := strings.Replace(initialSQL, "COMMIT;", "INSERT INTO missing_table VALUES(1); COMMIT;", 1)
		if err := c.Exec(broken); err == nil || c.GetAutocommit() {
			t.Fatal("expected partial migration transaction")
		}
		return nil // Close must roll back DDL and markers too.
	}))
	must(t, withSQLite(context.Background(), root, func(c *sqlite3.Conn) error {
		if scalar(t, c, "PRAGMA user_version") != "0" || scalar(t, c, "SELECT count(*) FROM sqlite_schema") != "0" {
			t.Fatal("partial migration escaped rollback")
		}
		must(t, c.Exec(initialSQL))
		outside := filepath.Join(t.TempDir(), "outside")
		must(t, os.WriteFile(outside, []byte("preserve"), 0600))
		journal := filepath.Join(path, "taletty.sqlite3-journal")
		must(t, os.Symlink(outside, journal)) // after initial root preflight
		if err := c.Exec("INSERT INTO settings VALUES(1, 'plain', 20)"); err == nil {
			t.Fatal("late planted journal accepted")
		}
		data, err := os.ReadFile(outside)
		must(t, err)
		if string(data) != "preserve" {
			t.Fatal("journal target changed")
		}
		must(t, os.Remove(journal)) // test owns fixture; production never sweeps
		return c.Exec("INSERT INTO settings VALUES(1, 'plain', 30)")
	}))
}

func TestSQLiteBorrowsAdvisoryOwnerWithoutRecursion(t *testing.T) {
	path, root := rootFixture(t)
	owner, err := os.OpenFile(filepath.Join(path, ".auth.lock"), os.O_CREATE|os.O_RDWR, 0600)
	must(t, err)
	defer owner.Close()
	must(t, unix.Flock(int(owner.Fd()), unix.LOCK_EX|unix.LOCK_NB))
	// SQLite receives only an externally owned root capability, no auth gate.
	// This is not evidence of the still-deferred public engine lease API.
	must(t, withSQLite(context.Background(), root, func(c *sqlite3.Conn) error {
		return c.Exec(initialSQL + "\nINSERT INTO settings VALUES(1, 'plain', 40)")
	}))
	other, err := os.OpenFile(filepath.Join(path, ".auth.lock"), os.O_RDWR, 0)
	must(t, err)
	defer other.Close()
	if err := unix.Flock(int(other.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatal("store released external advisory ownership", err)
	}
}

func TestSQLiteBusyChild(t *testing.T) {
	path := os.Getenv("TALETTY_SQLITE_CHILD")
	if path == "" {
		return
	}
	root, err := os.Open(path)
	must(t, err)
	defer root.Close()
	must(t, withSQLite(context.Background(), root, func(c *sqlite3.Conn) error {
		if err := c.Exec("BEGIN IMMEDIATE"); !errors.Is(err, sqlite3.BUSY) {
			return fmt.Errorf("process missed parent lock: %w", err)
		}
		return nil
	}))
}

func TestSQLiteVFSRejectsUnexpectedPaths(t *testing.T) {
	path, root := rootFixture(t)
	fs := &sqliteVFS{root: root, path: fmt.Sprintf("/proc/self/fd/%d/taletty.sqlite3", root.Fd())}
	for _, name := range []string{"", "../escape", fs.path + "-wal", fs.path + "-shm", fs.path + "-mj123"} {
		if _, err := fs.FullPathname(name); err != ErrUnsafeFile {
			t.Fatal("unexpected full path", name, err)
		}
		if err := fs.Delete(name, true); err != ErrUnsafeFile {
			t.Fatal("unexpected deletion", name, err)
		}
	}
	if _, _, err := fs.Open("", vfs.OPEN_CREATE); err != ErrUnsafeFile {
		t.Fatal(err)
	}
	if _, _, err := fs.OpenFilename(nil, vfs.OPEN_CREATE); err != ErrUnsafeFile {
		t.Fatal(err)
	}
	if _, err := fs.Access("../escape", vfs.ACCESS_EXISTS); err != ErrUnsafeFile {
		t.Fatal(err)
	}
	for _, option := range []string{"modeof=outside", "mode=rw"} {
		vfs.Register("taletty-probe", fs)
		conn, err := sqlite3.Open("file:" + fs.path + "?vfs=taletty-probe&" + option)
		vfs.Unregister("taletty-probe")
		if conn != nil {
			conn.Close()
		}
		if err == nil {
			t.Fatal("unsafe mode or missing file accepted", option)
		}
	}
	if err := fs.Delete(fs.path, true); err != ErrUnsafeFile {
		t.Fatal(err)
	}
	if err := fs.Delete(fs.path+"-journal", true); err != ErrUnsafeFile {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		name := filepath.Join(path, "taletty.sqlite3"+suffix)
		must(t, os.WriteFile(name, []byte("private"), 0600))
		_, err := fs.Access(fs.path+suffix, vfs.ACCESS_EXISTS)
		if suffix == "-wal" || suffix == "-shm" {
			if err != ErrUnsafeFile {
				t.Fatal(err)
			}
		} else {
			must(t, err)
		}
	}
	must(t, fs.Delete(fs.path+"-journal", false))
	must(t, os.Chmod(filepath.Join(path, "taletty.sqlite3"), 0644))
	if _, err := fs.Access(fs.path, vfs.ACCESS_EXISTS); err != ErrUnsafeFile {
		t.Fatal(err)
	}
	must(t, root.Close())
	if _, err := fs.Access(fs.path, vfs.ACCESS_EXISTS); err != ErrIO {
		t.Fatal(err)
	}
}
