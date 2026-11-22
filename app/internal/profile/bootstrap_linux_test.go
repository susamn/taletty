package profile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/susamn/taletty/app/internal/store"
	"golang.org/x/sys/unix"
)

type fileSnapshot struct {
	data string
	stat unix.Stat_t
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func strPtr(v string) *string { return &v }
func int64Ptr(v int64) *int64 { return &v }

func snapshotTree(t *testing.T, root string) map[string]fileSnapshot {
	t.Helper()
	out := make(map[string]fileSnapshot)
	var walk func(absPath, relPath string) error
	walk = func(absPath, relPath string) error {
		var st unix.Stat_t
		if err := unix.Lstat(absPath, &st); err != nil {
			return err
		}
		snap := fileSnapshot{stat: st}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFREG:
			fd, err := unix.Open(absPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOATIME, 0)
			if err != nil {
				return err
			}
			f := os.NewFile(uintptr(fd), "snapshot")
			var buf bytes.Buffer
			_, readErr := buf.ReadFrom(f)
			closeErr := f.Close()
			if readErr != nil {
				return readErr
			}
			if closeErr != nil {
				return closeErr
			}
			snap.data = buf.String()
			out[relPath] = snap
		case unix.S_IFDIR:
			out[relPath] = snap
			fd, err := unix.Open(absPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NOATIME, 0)
			if err != nil {
				return err
			}
			f := os.NewFile(uintptr(fd), "snapshot-dir")
			names, readErr := f.Readdirnames(-1)
			closeErr := f.Close()
			if readErr != nil {
				return readErr
			}
			if closeErr != nil {
				return closeErr
			}
			for _, name := range names {
				childRel := name
				if relPath != "." {
					childRel = filepath.Join(relPath, name)
				}
				if err := walk(filepath.Join(absPath, name), childRel); err != nil {
					return err
				}
			}
		default:
			out[relPath] = snap
		}
		return nil
	}
	must(t, walk(root, "."))
	return out
}

func assertTreePreserved(t *testing.T, before, after map[string]fileSnapshot) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("tree entries changed: before=%d after=%d", len(before), len(after))
	}
	for rel, want := range before {
		got, ok := after[rel]
		if !ok {
			t.Fatalf("missing entry %q", rel)
		}
		if got != want {
			t.Fatalf("%s changed: before=%+v after=%+v", rel, want.stat, got.stat)
		}
	}
}

func statMode(t *testing.T, path string) uint32 {
	t.Helper()
	var st unix.Stat_t
	must(t, unix.Lstat(path, &st))
	return st.Mode & (unix.S_IFMT | 07777)
}

func statInode(t *testing.T, path string) string {
	t.Helper()
	var st unix.Stat_t
	must(t, unix.Lstat(path, &st))
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}

func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	must(t, err)
	return len(entries)
}

func runChild(t *testing.T, mode, home, inodes string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBootstrapSubprocessChild$")
	cmd.Env = append(os.Environ(),
		"TALETTY_PROFILE_CHILD_MODE="+mode,
		"TALETTY_PROFILE_CHILD_HOME="+home,
		"TALETTY_PROFILE_CHILD_INODES="+inodes,
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestBootstrapSubprocessChild(t *testing.T) {
	mode := os.Getenv("TALETTY_PROFILE_CHILD_MODE")
	if mode == "" {
		return
	}
	home := os.Getenv("TALETTY_PROFILE_CHILD_HOME")
	inodes := strings.Split(os.Getenv("TALETTY_PROFILE_CHILD_INODES"), ",")
	forbidden := make(map[string]bool, len(inodes))
	for _, item := range inodes {
		if item != "" {
			forbidden[item] = true
		}
	}
	entries, err := os.ReadDir("/proc/self/fd")
	must(t, err)
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		must(t, err)
		var st unix.Stat_t
		if unix.Fstat(fd, &st) == nil {
			key := fmt.Sprintf("%d:%d", st.Dev, st.Ino)
			if forbidden[key] {
				t.Fatalf("child inherited private descriptor %s", key)
			}
		}
	}

	ctx := context.Background()
	switch mode {
	case "busy-and-fd":
		ws, err := Open(ctx, home, 1000)
		if !errors.Is(err, ErrBusy) || ws != nil {
			t.Fatalf("expected ErrBusy in child, got (%v, %v)", ws, err)
		}
	case "reopen":
		ws, err := Open(ctx, home, 2000)
		must(t, err)
		settings, err := ws.Store().Settings(ctx)
		must(t, err)
		if settings.GlyphMode != store.GlyphPlain || settings.VolumePercent != 42 {
			t.Fatalf("unexpected persisted settings in child: %+v", settings)
		}
		must(t, ws.Close(ctx))
	default:
		t.Fatalf("unknown child mode %q", mode)
	}
}

func TestFreshLaunchIgnoresLegacyAndXDGAndPreservesAuth(t *testing.T) {
	must(t, store.Initialize())
	ctx := context.Background()

	home := t.TempDir()
	xdgDir := filepath.Join(t.TempDir(), "xdg-config")
	must(t, os.MkdirAll(xdgDir, 0700))
	must(t, os.WriteFile(filepath.Join(xdgDir, "sentinel"), []byte("xdg-untouched"), 0600))
	t.Setenv("XDG_CONFIG_HOME", xdgDir)

	legacyDir := filepath.Join(home, ".config", "audible-tui")
	must(t, os.MkdirAll(filepath.Join(legacyDir, "downloads"), 0700))
	must(t, os.WriteFile(filepath.Join(legacyDir, "auth.json"), []byte(`{"legacy":"secret"}`), 0600))
	must(t, os.WriteFile(filepath.Join(legacyDir, "library.json"), []byte(`{"books":[]}`), 0644))
	must(t, os.WriteFile(filepath.Join(legacyDir, "downloads", "old.m4b"), []byte("legacy-audio"), 0600))

	// Existing ~/.config with 0755 must be accepted without chmod repair.
	configDir := filepath.Join(home, ".config")
	must(t, os.Chmod(configDir, 0755))

	beforeLegacy := snapshotTree(t, legacyDir)
	beforeXDG := snapshotTree(t, xdgDir)
	fdsBefore := openFDCount(t)

	ws, err := Open(ctx, home, 1000)
	must(t, err)

	wantRoot := filepath.Join(home, ".config", "taletty")
	wantDownloads := filepath.Join(wantRoot, "downloads")
	if ws.RootPath() != wantRoot || ws.DownloadsPath() != wantDownloads {
		t.Fatalf("unexpected paths: root=%q downloads=%q", ws.RootPath(), ws.DownloadsPath())
	}
	if ws.Owner() == nil || ws.Store() == nil {
		t.Fatal("expected non-nil Owner and Store")
	}
	if got := statMode(t, configDir); got != unix.S_IFDIR|0755 {
		t.Fatalf("existing ~/.config mode modified: %#o", got)
	}
	if got := statMode(t, wantRoot); got != unix.S_IFDIR|0700 {
		t.Fatalf("taletty root mode=%#o, want 0700", got)
	}
	if got := statMode(t, wantDownloads); got != unix.S_IFDIR|0700 {
		t.Fatalf("downloads mode=%#o, want 0700", got)
	}
	for _, leaf := range []string{".auth.lock", "taletty.sqlite3"} {
		var st unix.Stat_t
		must(t, unix.Lstat(filepath.Join(wantRoot, leaf), &st))
		if st.Mode&(unix.S_IFMT|07777) != unix.S_IFREG|0600 || st.Nlink != 1 {
			t.Fatalf("%s unsafe stat: mode=%#o nlink=%d", leaf, st.Mode, st.Nlink)
		}
	}
	if _, err := os.Stat(filepath.Join(wantRoot, "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bootstrap must not create auth.json, got %v", err)
	}
	must(t, ws.Store().Check(ctx))

	// Seed auth.json (0600) and verify it survives Close + restart + child reopen untouched.
	authPath := filepath.Join(wantRoot, "auth.json")
	must(t, os.WriteFile(authPath, []byte(`{"placeholder":"token"}`), 0600))
	beforeAuth := snapshotTree(t, authPath)

	must(t, ws.Close(ctx))
	if fdsAfter := openFDCount(t); fdsAfter != fdsBefore {
		t.Fatalf("fd leak across Open/Close: before=%d after=%d", fdsBefore, fdsAfter)
	}

	ws2, err := Open(ctx, home, 1100)
	must(t, err)
	must(t, ws2.Close(ctx))

	assertTreePreserved(t, beforeAuth, snapshotTree(t, authPath))
	assertTreePreserved(t, beforeLegacy, snapshotTree(t, legacyDir))
	assertTreePreserved(t, beforeXDG, snapshotTree(t, xdgDir))

	// Verify error sanitization never echoes private paths or raw OS diagnostics.
	for _, badHome := range []string{"relative/secret-home", "/nonexistent/secret-home"} {
		_, err := Open(ctx, badHome, 0)
		if err == nil || strings.Contains(err.Error(), "secret-home") {
			t.Fatalf("error leaked private path: %v", err)
		}
	}

	// Also verify fresh launch when ~/.config does not exist yet creates ~/.config at 0700.
	freshHome := t.TempDir()
	wsFresh, err := Open(ctx, freshHome, 1200)
	must(t, err)
	if got := statMode(t, filepath.Join(freshHome, ".config")); got != unix.S_IFDIR|0700 {
		t.Fatalf("created ~/.config mode=%#o, want 0700", got)
	}
	must(t, wsFresh.Close(ctx))
}

func TestRestartPreservesRepositoryDataAndReconcilesJobs(t *testing.T) {
	must(t, store.Initialize())
	ctx := context.Background()
	home := t.TempDir()

	ws, err := Open(ctx, home, 100)
	must(t, err)
	st := ws.Store()

	kReady := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	kNestedReady := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	kMissing := store.BookKey{Provider: "local", Namespace: "user", ID: "book-missing"}
	kUnsafeMode := store.BookKey{Provider: "local", Namespace: "user", ID: "book-unsafe-mode"}
	kUnsafeDir := store.BookKey{Provider: "local", Namespace: "user", ID: "book-unsafe-dir"}
	kWaiting := store.BookKey{Provider: "local", Namespace: "user", ID: "book-waiting"}

	must(t, st.ReplaceCatalog(ctx, "audible", "us", []store.CatalogBook{
		{
			Key:        kReady,
			Title:      "Book One",
			Subtitle:   strPtr("Sub"),
			Authors:    []store.Contributor{{Name: "Author A"}},
			Narrators:  []store.Contributor{{Name: "Narrator A"}},
			DurationNS: int64Ptr(3600_000_000_000),
		},
		{Key: kNestedReady, Title: "Book Two"},
	}, 150))
	must(t, st.ReplaceCatalog(ctx, "local", "user", []store.CatalogBook{
		{Key: kMissing, Title: "Missing Audio"},
		{Key: kUnsafeMode, Title: "Unsafe Mode Audio"},
		{Key: kUnsafeDir, Title: "Unsafe Dir Audio"},
		{Key: kWaiting, Title: "Waiting Audio"},
	}, 160))

	wantLS := store.ListeningState{
		Key:         kReady,
		PositionNS:  42_000_000_000,
		SpeedMilli:  1500000,
		Completion:  store.CompletionUnfinished,
		UpdatedAtMS: 170,
	}
	must(t, st.PutListeningState(ctx, wantLS))
	wantSettings := store.Settings{GlyphMode: store.GlyphPlain, VolumePercent: 65}
	must(t, st.PutSettings(ctx, wantSettings))
	wantView := store.ViewState{
		Destination: store.DestinationFavorites,
		Selected:    &kReady,
		Focus:       store.FocusDetails,
		FilterText:  "book",
		SortOrder:   store.SortRecent,
	}
	must(t, st.PutViewState(ctx, wantView))
	must(t, st.SetFavorite(ctx, kReady, true, 180))
	wantBM := store.Bookmark{
		ID:           "bm-1",
		Key:          kReady,
		PositionNS:   12_000_000_000,
		Note:         "Important\n\tpassage",
		ChapterID:    strPtr("ch-1"),
		ChapterTitle: strPtr("Chapter 1"),
		CreatedAtMS:  190,
		UpdatedAtMS:  195,
	}
	must(t, st.PutBookmark(ctx, wantBM))

	// Seed published media files on disk with varying safety states.
	dlDir := ws.DownloadsPath()
	must(t, os.WriteFile(filepath.Join(dlDir, "B000000001.m4b"), []byte("audio-1"), 0600))
	must(t, os.Mkdir(filepath.Join(dlDir, "nested"), 0700))
	must(t, os.WriteFile(filepath.Join(dlDir, "nested", "B000000002.m4b"), []byte("audio-2"), 0600))
	must(t, os.WriteFile(filepath.Join(dlDir, "unsafe-mode.m4b"), []byte("audio-3"), 0644))
	must(t, os.Mkdir(filepath.Join(dlDir, "unsafe-dir"), 0755))
	must(t, os.WriteFile(filepath.Join(dlDir, "unsafe-dir", "book.m4b"), []byte("audio-4"), 0600))

	must(t, st.PutLocalMedia(ctx, store.LocalMedia{Key: kReady, RelativePath: "downloads/B000000001.m4b", Format: "m4b", PublishedAtMS: 200}))
	must(t, st.PutLocalMedia(ctx, store.LocalMedia{Key: kNestedReady, RelativePath: "downloads/nested/B000000002.m4b", Format: "m4b", PublishedAtMS: 201}))
	must(t, st.PutLocalMedia(ctx, store.LocalMedia{Key: kMissing, RelativePath: "downloads/absent.m4b", Format: "m4b", PublishedAtMS: 202}))
	must(t, st.PutLocalMedia(ctx, store.LocalMedia{Key: kUnsafeMode, RelativePath: "downloads/unsafe-mode.m4b", Format: "m4b", PublishedAtMS: 203}))
	must(t, st.PutLocalMedia(ctx, store.LocalMedia{Key: kUnsafeDir, RelativePath: "downloads/unsafe-dir/book.m4b", Format: "m4b", PublishedAtMS: 204}))

	// Seed waiting + terminal + active jobs across restarts.
	must(t, st.PutDownloadJob(ctx, store.DownloadJob{
		ID: "job-wait", Key: kWaiting, QueueOrder: int64Ptr(0), State: store.JobWaiting, CreatedAtMS: 210, UpdatedAtMS: 210,
	}))
	must(t, st.PutDownloadJob(ctx, store.DownloadJob{
		ID: "job-canceled", Key: kReady, State: store.JobCanceled, CreatedAtMS: 211, UpdatedAtMS: 212,
	}))
	must(t, st.PutDownloadJob(ctx, store.DownloadJob{
		ID: "job-active-ready", Key: kReady, State: store.JobPublishing, BytesReceived: 7, BytesTotal: int64Ptr(7), CreatedAtMS: 220, UpdatedAtMS: 221,
	}))
	must(t, ws.Close(ctx))

	// Restart 1: active job with private file reconciles to JobReady; all data preserved.
	ws, err = Open(ctx, home, 300)
	must(t, err)
	st = ws.Store()

	jobs, err := st.DownloadJobs(ctx)
	must(t, err)
	jobByID := make(map[string]store.DownloadJob, len(jobs))
	for _, j := range jobs {
		jobByID[j.ID] = j
	}
	if j := jobByID["job-active-ready"]; j.State != store.JobReady || j.FailureCode != "" || j.UpdatedAtMS != 300 {
		t.Fatalf("expected JobReady after restart, got %+v", j)
	}
	if j := jobByID["job-wait"]; j.State != store.JobWaiting || j.QueueOrder == nil || *j.QueueOrder != 0 {
		t.Fatalf("waiting job altered: %+v", j)
	}
	if j := jobByID["job-canceled"]; j.State != store.JobCanceled {
		t.Fatalf("terminal job altered: %+v", j)
	}

	// Verify all repository data survived restart.
	usCat, err := st.Catalog(ctx, "audible", "us")
	must(t, err)
	if len(usCat) != 2 || usCat[0].Title != "Book One" {
		t.Fatalf("catalog lost: %+v", usCat)
	}
	ts, found, err := st.SnapshotRefreshedAt(ctx, "audible", "us")
	must(t, err)
	if !found || ts != 150 {
		t.Fatalf("snapshot ts lost: (%d, %v)", ts, found)
	}
	gotLS, found, err := st.ListeningState(ctx, kReady)
	must(t, err)
	if !found || gotLS != wantLS {
		t.Fatalf("listening state lost: (%+v, %v)", gotLS, found)
	}
	gotSettings, err := st.Settings(ctx)
	must(t, err)
	if gotSettings != wantSettings {
		t.Fatalf("settings lost: %+v", gotSettings)
	}
	gotView, err := st.ViewState(ctx)
	must(t, err)
	if gotView.Selected == nil || *gotView.Selected != kReady || gotView.FilterText != "book" {
		t.Fatalf("view state lost: %+v", gotView)
	}
	favs, err := st.Favorites(ctx)
	must(t, err)
	if !reflect.DeepEqual(favs, []store.BookKey{kReady}) {
		t.Fatalf("favorites lost: %+v", favs)
	}
	bms, err := st.Bookmarks(ctx, kReady)
	must(t, err)
	if !reflect.DeepEqual(bms, []store.Bookmark{wantBM}) {
		t.Fatalf("bookmarks lost: %+v", bms)
	}
	mediaList, err := st.LocalMediaList(ctx)
	must(t, err)
	if len(mediaList) != 5 {
		t.Fatalf("local media lost: %+v", mediaList)
	}

	// Test nested private file reconciliation -> JobReady, and missing/unsafe -> JobFailed/FailureInterrupted.
	for _, tc := range []struct {
		id        string
		key       store.BookKey
		wantState store.JobState
		wantFail  store.FailureCode
	}{
		{"job-nested-ready", kNestedReady, store.JobReady, ""},
		{"job-missing", kMissing, store.JobFailed, store.FailureInterrupted},
		{"job-unsafe-mode", kUnsafeMode, store.JobFailed, store.FailureInterrupted},
		{"job-unsafe-dir", kUnsafeDir, store.JobFailed, store.FailureInterrupted},
	} {
		must(t, st.PutDownloadJob(ctx, store.DownloadJob{
			ID: tc.id, Key: tc.key, State: store.JobDownloading, CreatedAtMS: 400, UpdatedAtMS: 400,
		}))
		must(t, ws.Close(ctx))

		ws, err = Open(ctx, home, 500)
		must(t, err)
		st = ws.Store()
		jobs, err = st.DownloadJobs(ctx)
		must(t, err)
		var got store.DownloadJob
		for _, j := range jobs {
			if j.ID == tc.id {
				got = j
			}
		}
		if got.State != tc.wantState || got.FailureCode != tc.wantFail {
			t.Fatalf("%s: got (%s, %s), want (%s, %s)", tc.id, got.State, got.FailureCode, tc.wantState, tc.wantFail)
		}
	}
	must(t, ws.Close(ctx))
}

func TestConcurrentProcessExclusionFDNonInheritanceAndCloseRetry(t *testing.T) {
	must(t, store.Initialize())
	ctx := context.Background()
	home := t.TempDir()

	ws, err := Open(ctx, home, 1000)
	must(t, err)
	must(t, ws.Store().PutSettings(ctx, store.Settings{GlyphMode: store.GlyphPlain, VolumePercent: 42}))

	// In-process concurrent Open must fail with ErrBusy.
	if other, err := Open(ctx, home, 1000); !errors.Is(err, ErrBusy) || other != nil {
		t.Fatalf("expected in-process ErrBusy, got (%v, %v)", other, err)
	}

	// Real subprocess must fail with ErrBusy and must not inherit root or lock FDs.
	inodes := statInode(t, ws.RootPath()) + "," + statInode(t, filepath.Join(ws.RootPath(), ".auth.lock"))
	if out, err := runChild(t, "busy-and-fd", home, inodes); err != nil {
		t.Fatalf("busy-and-fd child failed: %v\n%s", err, out)
	}

	// Canceled Close returns ctx.Err() before closing Store or unlocking Owner.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := ws.Close(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if out, err := runChild(t, "busy-and-fd", home, inodes); err != nil {
		t.Fatalf("lock lost after canceled Close: %v\n%s", err, out)
	}

	// Active Owner.Lease blocks Close until lease joins; timeout must NOT unlock underneath lease.
	lease, err := ws.Owner().Lease(ctx)
	must(t, err)
	shortCtx, shortCancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer shortCancel()
	if err := ws.Close(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded while lease active, got %v", err)
	}
	if lease.Context.Err() != context.Canceled {
		t.Fatal("Close did not cancel active lease")
	}
	if out, err := runChild(t, "busy-and-fd", home, inodes); err != nil {
		t.Fatalf("lock released underneath active lease: %v\n%s", err, out)
	}

	// Join lease, then retry Close concurrently and repeatedly.
	lease.Close()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := ws.Close(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	must(t, ws.Close(ctx))

	// Nil and zero-value Workspace Close are safe no-ops.
	var nilWS *Workspace
	must(t, nilWS.Close(ctx))
	must(t, (&Workspace{}).Close(ctx))

	// Real subprocess can now reopen and read persisted settings.
	if out, err := runChild(t, "reopen", home, inodes); err != nil {
		t.Fatalf("reopen child failed: %v\n%s", err, out)
	}
}

func TestAdversarialNoRepairRefusal(t *testing.T) {
	must(t, store.Initialize())
	ctx := context.Background()

	// 1. Unsafe ~/.config variants.
	for _, kind := range []string{"symlink", "dangling", "file", "fifo"} {
		t.Run("config/"+kind, func(t *testing.T) {
			home := t.TempDir()
			cfg := filepath.Join(home, ".config")
			outside := filepath.Join(t.TempDir(), "outside")
			must(t, os.Mkdir(outside, 0700))
			switch kind {
			case "symlink":
				must(t, os.Symlink(outside, cfg))
			case "dangling":
				must(t, os.Symlink("absent", cfg))
			case "file":
				must(t, os.WriteFile(cfg, []byte("keep"), 0600))
			case "fifo":
				must(t, unix.Mkfifo(cfg, 0600))
			}
			before := snapshotTree(t, home)
			if ws, err := Open(ctx, home, 100); !errors.Is(err, ErrUnsafeRoot) || ws != nil {
				t.Fatalf("config/%s: got (%v, %v), want ErrUnsafeRoot", kind, ws, err)
			}
			assertTreePreserved(t, before, snapshotTree(t, home))
		})
	}

	// 2. Unsafe ~/.config/taletty variants (no chmod repair).
	for _, kind := range []string{"symlink", "dangling", "file", "fifo", "mode-0755", "mode-01700", "mode-02700", "mode-04700"} {
		t.Run("taletty/"+kind, func(t *testing.T) {
			home := t.TempDir()
			cfg := filepath.Join(home, ".config")
			must(t, os.Mkdir(cfg, 0700))
			root := filepath.Join(cfg, "taletty")
			outside := filepath.Join(t.TempDir(), "outside")
			must(t, os.Mkdir(outside, 0700))
			switch kind {
			case "symlink":
				must(t, os.Symlink(outside, root))
			case "dangling":
				must(t, os.Symlink("absent", root))
			case "file":
				must(t, os.WriteFile(root, []byte("keep"), 0600))
			case "fifo":
				must(t, unix.Mkfifo(root, 0600))
			case "mode-0755":
				must(t, os.Mkdir(root, 0755))
			case "mode-01700":
				must(t, os.Mkdir(root, 0700))
				must(t, unix.Chmod(root, 01700))
			case "mode-02700":
				must(t, os.Mkdir(root, 0700))
				must(t, unix.Chmod(root, 02700))
			case "mode-04700":
				must(t, os.Mkdir(root, 0700))
				must(t, unix.Chmod(root, 04700))
			}
			before := snapshotTree(t, home)
			ws, err := Open(ctx, home, 100)
			if !errors.Is(err, ErrUnsafeRoot) || ws != nil {
				t.Fatalf("taletty/%s: got (%v, %v), want ErrUnsafeRoot", kind, ws, err)
			}
			if strings.Contains(err.Error(), home) {
				t.Fatalf("error leaked home path: %v", err)
			}
			assertTreePreserved(t, before, snapshotTree(t, home))
		})
	}

	// 3. Unsafe ~/.config/taletty/downloads variants (no chmod repair).
	for _, kind := range []string{"symlink", "dangling", "file", "fifo", "mode-0755", "mode-01700", "mode-04700"} {
		t.Run("downloads/"+kind, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, ".config", "taletty")
			must(t, os.MkdirAll(root, 0700))
			must(t, os.Chmod(root, 0700))
			dl := filepath.Join(root, "downloads")
			outside := filepath.Join(t.TempDir(), "outside")
			must(t, os.Mkdir(outside, 0700))
			switch kind {
			case "symlink":
				must(t, os.Symlink(outside, dl))
			case "dangling":
				must(t, os.Symlink("absent", dl))
			case "file":
				must(t, os.WriteFile(dl, []byte("keep"), 0600))
			case "fifo":
				must(t, unix.Mkfifo(dl, 0600))
			case "mode-0755":
				must(t, os.Mkdir(dl, 0755))
			case "mode-01700":
				must(t, os.Mkdir(dl, 0700))
				must(t, unix.Chmod(dl, 01700))
			case "mode-04700":
				must(t, os.Mkdir(dl, 0700))
				must(t, unix.Chmod(dl, 04700))
			}
			before := snapshotTree(t, home)
			if ws, err := Open(ctx, home, 100); !errors.Is(err, ErrUnsafeRoot) || ws != nil {
				t.Fatalf("downloads/%s: got (%v, %v), want ErrUnsafeRoot", kind, ws, err)
			}
			assertTreePreserved(t, before, snapshotTree(t, home))
		})
	}

	// 4. Unsafe ~/.config/taletty/.auth.lock variants (no chmod repair).
	for _, kind := range []string{"symlink", "dangling", "hardlink", "fifo", "socket", "dir", "mode-0644", "mode-04600"} {
		t.Run("lock/"+kind, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, ".config", "taletty")
			dl := filepath.Join(root, "downloads")
			must(t, os.MkdirAll(dl, 0700))
			must(t, os.Chmod(filepath.Join(home, ".config"), 0700))
			must(t, os.Chmod(root, 0700))
			must(t, os.Chmod(dl, 0700))
			lock := filepath.Join(root, ".auth.lock")
			outside := filepath.Join(t.TempDir(), "outside")
			must(t, os.WriteFile(outside, []byte("outside"), 0600))
			switch kind {
			case "symlink":
				must(t, os.Symlink(outside, lock))
			case "dangling":
				must(t, os.Symlink("absent", lock))
			case "hardlink":
				must(t, os.Link(outside, lock))
			case "fifo":
				must(t, unix.Mkfifo(lock, 0600))
			case "socket":
				ln, err := net.Listen("unix", lock)
				must(t, err)
				defer ln.Close()
			case "dir":
				must(t, os.Mkdir(lock, 0700))
			case "mode-0644":
				must(t, os.WriteFile(lock, nil, 0644))
			case "mode-04600":
				must(t, os.WriteFile(lock, nil, 0600))
				must(t, unix.Chmod(lock, 04600))
			}
			before := snapshotTree(t, home)
			if ws, err := Open(ctx, home, 100); !errors.Is(err, ErrUnsafeRoot) || ws != nil {
				t.Fatalf("lock/%s: got (%v, %v), want ErrUnsafeRoot", kind, ws, err)
			}
			assertTreePreserved(t, before, snapshotTree(t, home))
			data, err := os.ReadFile(outside)
			must(t, err)
			if string(data) != "outside" {
				t.Fatal("outside file modified")
			}
		})
	}

	// 5. Foreign, corrupt, too-new DBs, unsafe DB mode, and sidecars preserved untouched.
	for _, tc := range []struct {
		name string
		prep func(t *testing.T, root string)
		want error
	}{
		{
			name: "foreign-db",
			prep: func(t *testing.T, root string) {
				c, err := sqlite3.Open(filepath.Join(root, "taletty.sqlite3"))
				must(t, err)
				must(t, c.Exec("PRAGMA application_id=123; CREATE TABLE foreign_tbl(x TEXT); INSERT INTO foreign_tbl VALUES('keep');"))
				must(t, c.Close())
				must(t, os.Chmod(filepath.Join(root, "taletty.sqlite3"), 0600))
			},
			want: store.ErrInvalid,
		},
		{
			name: "corrupt-db",
			prep: func(t *testing.T, root string) {
				must(t, os.WriteFile(filepath.Join(root, "taletty.sqlite3"), []byte("not-sqlite-bytes"), 0600))
			},
			want: store.ErrInvalid,
		},
		{
			name: "too-new-db",
			prep: func(t *testing.T, root string) {
				ws, err := Open(ctx, filepath.Dir(filepath.Dir(root)), 100)
				must(t, err)
				must(t, ws.Close(ctx))
				c, err := sqlite3.Open(filepath.Join(root, "taletty.sqlite3"))
				must(t, err)
				must(t, c.Exec("PRAGMA user_version=2;"))
				must(t, c.Close())
			},
			want: store.ErrVersion,
		},
		{
			name: "unsafe-db-mode",
			prep: func(t *testing.T, root string) {
				must(t, os.WriteFile(filepath.Join(root, "taletty.sqlite3"), []byte("keep"), 0644))
			},
			want: store.ErrUnsafeFile,
		},
		{
			name: "sidecar-journal",
			prep: func(t *testing.T, root string) {
				ws, err := Open(ctx, filepath.Dir(filepath.Dir(root)), 100)
				must(t, err)
				must(t, ws.Close(ctx))
				must(t, os.WriteFile(filepath.Join(root, "taletty.sqlite3-journal"), []byte("journal"), 0600))
			},
			want: store.ErrRecovery,
		},
		{
			name: "sidecar-wal",
			prep: func(t *testing.T, root string) {
				ws, err := Open(ctx, filepath.Dir(filepath.Dir(root)), 100)
				must(t, err)
				must(t, ws.Close(ctx))
				must(t, os.WriteFile(filepath.Join(root, "taletty.sqlite3-wal"), []byte("wal"), 0600))
			},
			want: store.ErrRecovery,
		},
		{
			name: "sidecar-shm",
			prep: func(t *testing.T, root string) {
				ws, err := Open(ctx, filepath.Dir(filepath.Dir(root)), 100)
				must(t, err)
				must(t, ws.Close(ctx))
				must(t, os.WriteFile(filepath.Join(root, "taletty.sqlite3-shm"), []byte("shm"), 0600))
			},
			want: store.ErrRecovery,
		},
	} {
		t.Run("db/"+tc.name, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, ".config", "taletty")
			dl := filepath.Join(root, "downloads")
			must(t, os.MkdirAll(dl, 0700))
			must(t, os.Chmod(filepath.Join(home, ".config"), 0700))
			must(t, os.Chmod(root, 0700))
			must(t, os.Chmod(dl, 0700))
			must(t, os.WriteFile(filepath.Join(root, ".auth.lock"), nil, 0600))
			must(t, os.WriteFile(filepath.Join(root, "auth.json"), []byte(`{"keep":"auth"}`), 0600))
			tc.prep(t, root)
			before := snapshotTree(t, root)
			ws, err := Open(ctx, home, 100)
			if !errors.Is(err, tc.want) || ws != nil {
				t.Fatalf("db/%s: got (%v, %v), want %v", tc.name, ws, err, tc.want)
			}
			if strings.Contains(err.Error(), home) || strings.Contains(err.Error(), "foreign_tbl") {
				t.Fatalf("db/%s leaked private details: %v", tc.name, err)
			}
			assertTreePreserved(t, before, snapshotTree(t, root))
		})
	}
}

func TestBootstrapFaultsValidationAndPathConfinement(t *testing.T) {
	must(t, store.Initialize())
	ctx := context.Background()

	// Invalid home, negative nowMS, canceled context.
	for _, badHome := range []string{"", "relative", "/bad\x00home"} {
		if ws, err := Open(ctx, badHome, 0); !errors.Is(err, ErrInvalidHome) || ws != nil {
			t.Fatalf("bad home %q: got (%v, %v)", badHome, ws, err)
		}
	}
	if ws, err := Open(ctx, t.TempDir(), -1); !errors.Is(err, store.ErrInvalidInput) || ws != nil {
		t.Fatalf("negative nowMS: got (%v, %v)", ws, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if ws, err := Open(canceled, t.TempDir(), 0); !errors.Is(err, context.Canceled) || ws != nil {
		t.Fatalf("canceled ctx: got (%v, %v)", ws, err)
	}

	// ReconcileActiveJobs failure inside Open (nowMS < active job CreatedAtMS) cleans up lock.
	homeReconcile := t.TempDir()
	ws, err := Open(ctx, homeReconcile, 100)
	must(t, err)
	must(t, ws.Store().PutDownloadJob(ctx, store.DownloadJob{
		ID: "job-future", Key: store.BookKey{Provider: "local", Namespace: "user", ID: "b1"},
		State: store.JobDownloading, CreatedAtMS: 500, UpdatedAtMS: 500,
	}))
	must(t, ws.Close(ctx))
	if ws2, err := Open(ctx, homeReconcile, 200); !errors.Is(err, store.ErrInvalidInput) || ws2 != nil {
		t.Fatalf("expected ErrInvalidInput on stale nowMS, got (%v, %v)", ws2, err)
	}
	// Lock must have been released on failure.
	ws2, err := Open(ctx, homeReconcile, 600)
	must(t, err)
	must(t, ws2.Close(ctx))

	// OS permission errors on Mkdir / Open / Mkdirat / Openat.
	homeFile := filepath.Join(t.TempDir(), "not-a-dir")
	must(t, os.WriteFile(homeFile, nil, 0600))
	if _, err := Open(ctx, homeFile, 0); !errors.Is(err, ErrIO) {
		t.Fatalf("expected ErrIO for file home, got %v", err)
	}

	homeNoPermCfg := t.TempDir()
	cfgNoPerm := filepath.Join(homeNoPermCfg, ".config")
	must(t, os.Mkdir(cfgNoPerm, 0000))
	t.Cleanup(func() { _ = os.Chmod(cfgNoPerm, 0700) })
	if _, err := Open(ctx, homeNoPermCfg, 0); !errors.Is(err, ErrIO) {
		t.Fatalf("expected ErrIO for 0000 ~/.config, got %v", err)
	}

	homeROCfg := t.TempDir()
	cfgRO := filepath.Join(homeROCfg, ".config")
	must(t, os.Mkdir(cfgRO, 0500))
	t.Cleanup(func() { _ = os.Chmod(cfgRO, 0700) })
	if _, err := Open(ctx, homeROCfg, 0); !errors.Is(err, ErrIO) {
		t.Fatalf("expected ErrIO for 0500 ~/.config, got %v", err)
	}

	homeNoPermRoot := t.TempDir()
	rootNoPerm := filepath.Join(homeNoPermRoot, ".config", "taletty")
	must(t, os.MkdirAll(rootNoPerm, 0700))
	must(t, os.Chmod(rootNoPerm, 0000))
	t.Cleanup(func() { _ = os.Chmod(rootNoPerm, 0700) })
	if _, err := Open(ctx, homeNoPermRoot, 0); !errors.Is(err, ErrIO) {
		t.Fatalf("expected ErrIO for 0000 taletty, got %v", err)
	}

	homeNoPermDL := t.TempDir()
	dlNoPerm := filepath.Join(homeNoPermDL, ".config", "taletty", "downloads")
	must(t, os.MkdirAll(dlNoPerm, 0700))
	must(t, os.Chmod(filepath.Dir(dlNoPerm), 0700))
	must(t, os.Chmod(dlNoPerm, 0000))
	t.Cleanup(func() { _ = os.Chmod(dlNoPerm, 0700) })
	if _, err := Open(ctx, homeNoPermDL, 0); !errors.Is(err, ErrIO) {
		t.Fatalf("expected ErrIO for 0000 downloads, got %v", err)
	}

	// Direct ensurePrivateDownloads and acquireAuthLock Mkdirat/Openat EACCES paths.
	roDir := t.TempDir()
	must(t, os.Chmod(roDir, 0500))
	t.Cleanup(func() { _ = os.Chmod(roDir, 0700) })
	roFD, err := unix.Open(roDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	must(t, err)
	defer unix.Close(roFD)
	euid := uint32(os.Geteuid())
	if _, err := ensurePrivateDownloads(roFD, euid, defaultSysOps()); !errors.Is(err, ErrIO) {
		t.Fatalf("expected ErrIO for read-only rootFD in ensurePrivateDownloads, got %v", err)
	}
	if _, _, err := acquireAuthLock(roFD, euid, defaultSysOps()); !errors.Is(err, ErrIO) {
		t.Fatalf("expected ErrIO for read-only rootFD in acquireAuthLock, got %v", err)
	}

	// Injected sysOps faults across openWorkspace, openPrivateRoot, ensurePrivateDownloads, acquireAuthLock, releaseFn.
	faultErr := errors.New("injected os fault")
	for _, tc := range []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "cfg-fstat-err",
			run: func(t *testing.T) {
				ops := defaultSysOps()
				ops.fstat = func(int, *unix.Stat_t) error { return faultErr }
				_, err := openWorkspace(ctx, t.TempDir(), 0, ops)
				if !errors.Is(err, ErrIO) || strings.Contains(err.Error(), faultErr.Error()) {
					t.Fatal(err)
				}
			},
		},
		{
			name: "cfg-uid-mismatch",
			run: func(t *testing.T) {
				ops := defaultSysOps()
				ops.fstat = func(fd int, st *unix.Stat_t) error {
					err := unix.Fstat(fd, st)
					st.Uid++
					return err
				}
				if _, err := openWorkspace(ctx, t.TempDir(), 0, ops); !errors.Is(err, ErrUnsafeRoot) {
					t.Fatal(err)
				}
			},
		},
		{
			name: "cfg-fsync-err",
			run: func(t *testing.T) {
				ops := defaultSysOps()
				ops.fsync = func(int) error { return faultErr }
				if _, err := openWorkspace(ctx, t.TempDir(), 0, ops); !errors.Is(err, ErrIO) {
					t.Fatal(err)
				}
			},
		},
		{
			name: "root-fstat-err",
			run: func(t *testing.T) {
				ops := defaultSysOps()
				calls := 0
				ops.fstat = func(fd int, st *unix.Stat_t) error {
					calls++
					if calls == 2 {
						return faultErr
					}
					return unix.Fstat(fd, st)
				}
				if _, err := openWorkspace(ctx, t.TempDir(), 0, ops); !errors.Is(err, ErrIO) {
					t.Fatal(err)
				}
			},
		},
		{
			name: "root-uid-mismatch",
			run: func(t *testing.T) {
				ops := defaultSysOps()
				calls := 0
				ops.fstat = func(fd int, st *unix.Stat_t) error {
					calls++
					err := unix.Fstat(fd, st)
					if calls == 2 {
						st.Uid++
					}
					return err
				}
				if _, err := openWorkspace(ctx, t.TempDir(), 0, ops); !errors.Is(err, ErrUnsafeRoot) {
					t.Fatal(err)
				}
			},
		},
		{
			name: "downloads-fstat-err",
			run: func(t *testing.T) {
				ops := defaultSysOps()
				calls := 0
				ops.fstat = func(fd int, st *unix.Stat_t) error {
					calls++
					if calls == 3 {
						return faultErr
					}
					return unix.Fstat(fd, st)
				}
				if _, err := openWorkspace(ctx, t.TempDir(), 0, ops); !errors.Is(err, ErrIO) {
					t.Fatal(err)
				}
			},
		},
		{
			name: "downloads-uid-mismatch",
			run: func(t *testing.T) {
				ops := defaultSysOps()
				calls := 0
				ops.fstat = func(fd int, st *unix.Stat_t) error {
					calls++
					err := unix.Fstat(fd, st)
					if calls == 3 {
						st.Uid++
					}
					return err
				}
				if _, err := openWorkspace(ctx, t.TempDir(), 0, ops); !errors.Is(err, ErrUnsafeRoot) {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lock-pre-fstatat-err",
			run: func(t *testing.T) {
				ops := defaultSysOps()
				ops.fstatat = func(int, string, *unix.Stat_t, int) error { return faultErr }
				if _, err := openWorkspace(ctx, t.TempDir(), 0, ops); !errors.Is(err, ErrIO) {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lock-fd-fstat-err",
			run: func(t *testing.T) {
				ops := defaultSysOps()
				calls := 0
				ops.fstat = func(fd int, st *unix.Stat_t) error {
					calls++
					if calls == 4 {
						return faultErr
					}
					return unix.Fstat(fd, st)
				}
				if _, err := openWorkspace(ctx, t.TempDir(), 0, ops); !errors.Is(err, ErrIO) {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lock-post-fstatat-err",
			run: func(t *testing.T) {
				ops := defaultSysOps()
				calls := 0
				ops.fstatat = func(dir int, name string, st *unix.Stat_t, flags int) error {
					calls++
					if calls == 2 {
						return faultErr
					}
					return unix.Fstatat(dir, name, st, flags)
				}
				if _, err := openWorkspace(ctx, t.TempDir(), 0, ops); !errors.Is(err, ErrIO) {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lock-inode-mismatch",
			run: func(t *testing.T) {
				ops := defaultSysOps()
				calls := 0
				ops.fstatat = func(dir int, name string, st *unix.Stat_t, flags int) error {
					calls++
					err := unix.Fstatat(dir, name, st, flags)
					if calls == 2 {
						st.Ino++
					}
					return err
				}
				if _, err := openWorkspace(ctx, t.TempDir(), 0, ops); !errors.Is(err, ErrUnsafeRoot) {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lock-flock-io-err",
			run: func(t *testing.T) {
				ops := defaultSysOps()
				ops.flock = func(int, int) error { return faultErr }
				if _, err := openWorkspace(ctx, t.TempDir(), 0, ops); !errors.Is(err, ErrIO) {
					t.Fatal(err)
				}
			},
		},
		{
			name: "root-fsync-err",
			run: func(t *testing.T) {
				ops := defaultSysOps()
				calls := 0
				ops.fsync = func(fd int) error {
					calls++
					if calls == 2 {
						return faultErr
					}
					return unix.Fsync(fd)
				}
				if _, err := openWorkspace(ctx, t.TempDir(), 0, ops); !errors.Is(err, ErrIO) {
					t.Fatal(err)
				}
			},
		},
		{
			name: "cancel-after-fsync",
			run: func(t *testing.T) {
				cctx, ccancel := context.WithCancel(ctx)
				ops := defaultSysOps()
				calls := 0
				ops.fsync = func(fd int) error {
					calls++
					err := unix.Fsync(fd)
					if calls == 2 {
						ccancel()
					}
					return err
				}
				if _, err := openWorkspace(cctx, t.TempDir(), 0, ops); !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			},
		},
		{
			name: "new-owner-unsafe-root",
			run: func(t *testing.T) {
				home := t.TempDir()
				ops := defaultSysOps()
				calls := 0
				ops.fsync = func(fd int) error {
					calls++
					err := unix.Fsync(fd)
					if calls == 2 {
						_ = os.Chmod(filepath.Join(home, ".config", "taletty"), 0755)
					}
					return err
				}
				if _, err := openWorkspace(ctx, home, 0, ops); !errors.Is(err, store.ErrUnsafeRoot) {
					t.Fatal(err)
				}
			},
		},
		{
			name: "release-unlock-err",
			run: func(t *testing.T) {
				ops := defaultSysOps()
				ops.flock = func(fd, how int) error {
					if how == unix.LOCK_UN {
						return faultErr
					}
					return unix.Flock(fd, how)
				}
				ws, err := openWorkspace(ctx, t.TempDir(), 0, ops)
				must(t, err)
				err = ws.Close(ctx)
				if !errors.Is(err, store.ErrIO) || strings.Contains(err.Error(), faultErr.Error()) {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, tc.run)
	}

	// Direct collectPublishedMedia error path (closed store).
	homeClosed := t.TempDir()
	wsClosed, err := Open(ctx, homeClosed, 0)
	must(t, err)
	stClosed := wsClosed.Store()
	must(t, wsClosed.Close(ctx))
	if _, err := collectPublishedMedia(ctx, stClosed, -1, euid, defaultSysOps()); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("expected ErrClosed from collectPublishedMedia, got %v", err)
	}

	// Direct isPublishedFilePrivate defense-in-depth checks.
	homePub := t.TempDir()
	wsPub, err := Open(ctx, homePub, 0)
	must(t, err)
	defer wsPub.Close(ctx)
	rootFD, err := unix.Open(wsPub.RootPath(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	must(t, err)
	defer unix.Close(rootFD)

	dl := wsPub.DownloadsPath()
	must(t, os.Mkdir(filepath.Join(dl, "sub"), 0700))
	must(t, os.WriteFile(filepath.Join(dl, "sub", "ok.m4b"), []byte("ok"), 0600))
	must(t, os.Symlink(filepath.Join(dl, "sub"), filepath.Join(dl, "symdir")))
	must(t, os.WriteFile(filepath.Join(dl, "notdir"), []byte("x"), 0600))
	must(t, os.Symlink(filepath.Join(dl, "sub", "ok.m4b"), filepath.Join(dl, "symfile.m4b")))
	outside := filepath.Join(t.TempDir(), "outside.m4b")
	must(t, os.WriteFile(outside, []byte("x"), 0600))
	must(t, os.Link(outside, filepath.Join(dl, "hardlink.m4b")))
	must(t, unix.Mkfifo(filepath.Join(dl, "fifo.m4b"), 0600))

	if !isPublishedFilePrivate(rootFD, "downloads/sub/ok.m4b", euid, defaultSysOps()) {
		t.Fatal("expected valid nested private file to pass")
	}
	for _, badPath := range []string{
		"",
		"downloads",
		"downloads/",
		"downloads/.",
		"downloads/..",
		"downloads//sub/ok.m4b",
		"downloads/./sub/ok.m4b",
		"downloads/../taletty.sqlite3",
		"downloads/sub\x00/ok.m4b",
		"downloads\\sub\\ok.m4b",
		"other/ok.m4b",
		"downloads/missing.m4b",
		"downloads/symdir/ok.m4b",
		"downloads/notdir/ok.m4b",
		"downloads/symfile.m4b",
		"downloads/hardlink.m4b",
		"downloads/fifo.m4b",
	} {
		if isPublishedFilePrivate(rootFD, badPath, euid, defaultSysOps()) {
			t.Fatalf("expected %q to be rejected", badPath)
		}
	}
	// Injected fstat error and UID mismatch inside isPublishedFilePrivate.
	opsFstatErr := defaultSysOps()
	opsFstatErr.fstat = func(int, *unix.Stat_t) error { return faultErr }
	if isPublishedFilePrivate(rootFD, "downloads/sub/ok.m4b", euid, opsFstatErr) {
		t.Fatal("expected fstat error to reject published file")
	}
	opsUID := defaultSysOps()
	opsUID.fstat = func(fd int, st *unix.Stat_t) error {
		err := unix.Fstat(fd, st)
		st.Uid++
		return err
	}
	if isPublishedFilePrivate(rootFD, "downloads/sub/ok.m4b", euid, opsUID) {
		t.Fatal("expected UID mismatch on downloads dir to reject published file")
	}
}
