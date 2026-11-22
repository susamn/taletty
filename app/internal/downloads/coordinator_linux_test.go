package downloads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/susamn/taletty/app/internal/store"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

func TestMain(m *testing.M) {
	if path := os.Getenv("TALETTY_SQLITE_TEST_CACHE"); path != "" {
		cache, err := wazero.NewCompilationCacheWithDir(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		sqlite3.RuntimeConfig = wazero.NewRuntimeConfig().WithMemoryLimitPages(4096).
			WithCoreFeatures(api.CoreFeaturesV2).WithCompilationCache(cache)
	}
	os.Exit(m.Run())
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func int64Ptr(v int64) *int64 { return &v }

func openTestStore(t *testing.T) (string, *store.Owner, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	must(t, os.Chmod(dir, 0o700))
	must(t, os.Mkdir(filepath.Join(dir, "downloads"), 0o700))

	root, err := os.Open(dir)
	must(t, err)
	t.Cleanup(func() { _ = root.Close() })

	owner, err := store.NewOwner(context.Background(), root, func() error { return nil })
	must(t, err)
	t.Cleanup(func() { must(t, owner.Close(context.Background())) })

	st, err := store.Open(context.Background(), owner)
	must(t, err)
	t.Cleanup(func() { must(t, st.Close(context.Background())) })

	return dir, owner, st
}

func writePrivateMedia(t *testing.T, rootDir, relPath string) {
	t.Helper()
	full := filepath.Join(rootDir, filepath.FromSlash(relPath))
	parent := filepath.Dir(full)
	for cur := parent; cur != rootDir && cur != "."; cur = filepath.Dir(cur) {
		if err := os.MkdirAll(cur, 0o700); err != nil {
			t.Fatal(err)
		}
		must(t, os.Chmod(cur, 0o700))
	}
	must(t, os.WriteFile(full, []byte("m4b-fixture"), 0o600))
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(ms int64) *fakeClock {
	return &fakeClock{now: time.UnixMilli(ms)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fakeDownloader struct {
	fn func(ctx context.Context, opID string, key store.BookKey, report func(DownloadEvent)) (DownloadOutcome, error)

	mu            sync.Mutex
	calls         []string
	activeCount   int
	maxConcurrent int
}

func (d *fakeDownloader) Download(ctx context.Context, opID string, key store.BookKey, report func(DownloadEvent)) (DownloadOutcome, error) {
	d.mu.Lock()
	d.calls = append(d.calls, key.ID)
	d.activeCount++
	if d.activeCount > d.maxConcurrent {
		d.maxConcurrent = d.activeCount
	}
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		d.activeCount--
		d.mu.Unlock()
	}()

	return d.fn(ctx, opID, key, report)
}

func (d *fakeDownloader) snapshotCalls() ([]string, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls), d.maxConcurrent
}

type faultStore struct {
	JobStore
	downloadJobsErr        error
	putDownloadJobErr      error
	putDownloadJobOnCall   int
	putDownloadJobCalls    int
	reorderErr             error
	reconcileErr           error
	localMediaErr          error
	localMediaListErr      error
	putLocalMediaErr       error
	onPutDownloadJobBefore func(store.DownloadJob)
}

func (f *faultStore) DownloadJobs(ctx context.Context) ([]store.DownloadJob, error) {
	if f.downloadJobsErr != nil {
		return nil, f.downloadJobsErr
	}
	return f.JobStore.DownloadJobs(ctx)
}

func (f *faultStore) PutDownloadJob(ctx context.Context, job store.DownloadJob) error {
	f.putDownloadJobCalls++
	if f.onPutDownloadJobBefore != nil {
		f.onPutDownloadJobBefore(job)
	}
	if f.putDownloadJobErr != nil && (f.putDownloadJobOnCall == 0 || f.putDownloadJobCalls == f.putDownloadJobOnCall) {
		return f.putDownloadJobErr
	}
	return f.JobStore.PutDownloadJob(ctx, job)
}

func (f *faultStore) ReorderDownloadQueue(ctx context.Context, orderedJobIDs []string, updatedAtMS int64) error {
	if f.reorderErr != nil {
		return f.reorderErr
	}
	return f.JobStore.ReorderDownloadQueue(ctx, orderedJobIDs, updatedAtMS)
}

func (f *faultStore) ReconcileActiveJobs(ctx context.Context, updatedAtMS int64, published map[store.BookKey]bool) ([]store.DownloadJob, error) {
	if f.reconcileErr != nil {
		return nil, f.reconcileErr
	}
	return f.JobStore.ReconcileActiveJobs(ctx, updatedAtMS, published)
}

func (f *faultStore) LocalMedia(ctx context.Context, key store.BookKey) (store.LocalMedia, bool, error) {
	if f.localMediaErr != nil {
		return store.LocalMedia{}, false, f.localMediaErr
	}
	return f.JobStore.LocalMedia(ctx, key)
}

func (f *faultStore) LocalMediaList(ctx context.Context) ([]store.LocalMedia, error) {
	if f.localMediaListErr != nil {
		return nil, f.localMediaListErr
	}
	return f.JobStore.LocalMediaList(ctx)
}

func (f *faultStore) PutLocalMedia(ctx context.Context, media store.LocalMedia) error {
	if f.putLocalMediaErr != nil {
		return f.putLocalMediaErr
	}
	return f.JobStore.PutLocalMedia(ctx, media)
}

type fakeChecker struct {
	fn func(ctx context.Context, media store.LocalMedia) (bool, error)
}

func (c fakeChecker) VerifyMedia(ctx context.Context, media store.LocalMedia) (bool, error) {
	return c.fn(ctx, media)
}

func waitForSnapshot(t *testing.T, ch <-chan Snapshot, pred func(Snapshot) bool) Snapshot {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case snap, ok := <-ch:
			if !ok {
				t.Fatal("watch channel closed before predicate matched")
			}
			if pred(snap) {
				return snap
			}
		case <-timer.C:
			t.Fatal("timed out waiting for snapshot predicate")
		}
	}
}

func TestNewValidationAndStartupErrors(t *testing.T) {
	_, owner, st := openTestStore(t)
	ctx := context.Background()
	dl := &fakeDownloader{fn: func(context.Context, string, store.BookKey, func(DownloadEvent)) (DownloadOutcome, error) {
		return DownloadOutcome{}, nil
	}}

	if _, err := New(nil, Options{Store: st, Downloader: dl, RootOwner: owner}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for nil ctx, got %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := New(canceled, Options{Store: st, Downloader: dl, RootOwner: owner}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	for _, opts := range []Options{
		{Downloader: dl, RootOwner: owner},
		{Store: st, RootOwner: owner},
		{Store: st, Downloader: dl},
	} {
		if _, err := New(ctx, opts); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for %+v, got %v", opts, err)
		}
	}

	k1 := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	must(t, st.PutLocalMedia(ctx, store.LocalMedia{
		Key: k1, RelativePath: "downloads/B000000001.m4b", Format: "m4b", PublishedAtMS: 10,
	}))

	boom := errors.New("boom")
	if _, err := New(ctx, Options{
		Store:      &faultStore{JobStore: st, localMediaListErr: boom},
		Downloader: dl,
		RootOwner:  owner,
	}); !errors.Is(err, boom) {
		t.Fatalf("expected LocalMediaList error, got %v", err)
	}
	if _, err := New(ctx, Options{
		Store:        st,
		Downloader:   dl,
		MediaChecker: fakeChecker{fn: func(context.Context, store.LocalMedia) (bool, error) { return false, boom }},
	}); !errors.Is(err, boom) {
		t.Fatalf("expected MediaChecker error, got %v", err)
	}
	if _, err := New(ctx, Options{
		Store:      &faultStore{JobStore: st, reconcileErr: boom},
		Downloader: dl,
		RootOwner:  owner,
	}); !errors.Is(err, boom) {
		t.Fatalf("expected ReconcileActiveJobs error, got %v", err)
	}
	if _, err := New(ctx, Options{
		Store:      &faultStore{JobStore: st, downloadJobsErr: boom},
		Downloader: dl,
		RootOwner:  owner,
	}); !errors.Is(err, boom) {
		t.Fatalf("expected DownloadJobs error, got %v", err)
	}

	// If a broken store still returns an active job after ReconcileActiveJobs, fail closed.
	brokenStore := &stubActiveStore{JobStore: st, activeKey: k1}
	if _, err := New(ctx, Options{
		Store:      brokenStore,
		Downloader: dl,
		RootOwner:  owner,
	}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState, got %v", err)
	}
}

type stubActiveStore struct {
	JobStore
	activeKey store.BookKey
}

func (s *stubActiveStore) DownloadJobs(context.Context) ([]store.DownloadJob, error) {
	return []store.DownloadJob{{
		ID: "still-active", Key: s.activeKey, State: store.JobDownloading, CreatedAtMS: 1, UpdatedAtMS: 1,
	}}, nil
}

func TestRootMediaVerifierSecurityChecks(t *testing.T) {
	rootDir, owner, _ := openTestStore(t)
	ctx := context.Background()
	v := &rootMediaVerifier{owner: owner, euid: uint32(os.Geteuid())}

	k := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	valid := store.LocalMedia{Key: k, RelativePath: "downloads/sub/B000000001.m4b", Format: "m4b", PublishedAtMS: 1}

	if ok, err := v.VerifyMedia(nil, valid); ok || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got (%v, %v)", ok, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if ok, err := v.VerifyMedia(canceled, valid); ok || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got (%v, %v)", ok, err)
	}
	if ok, err := v.VerifyMedia(ctx, store.LocalMedia{Key: k, RelativePath: "../bad.m4b", Format: "m4b"}); ok || err != nil {
		t.Fatalf("expected (false, nil) for invalid media, got (%v, %v)", ok, err)
	}

	// Missing file -> false, nil.
	if ok, err := v.VerifyMedia(ctx, valid); ok || err != nil {
		t.Fatalf("expected (false, nil) for missing file, got (%v, %v)", ok, err)
	}

	// Valid nested private file -> true, nil.
	writePrivateMedia(t, rootDir, valid.RelativePath)
	if ok, err := v.VerifyMedia(ctx, valid); !ok || err != nil {
		t.Fatalf("expected (true, nil) for valid private file, got (%v, %v)", ok, err)
	}

	// Unsafe file mode (0644) -> false, nil.
	full := filepath.Join(rootDir, filepath.FromSlash(valid.RelativePath))
	must(t, os.Chmod(full, 0o644))
	if ok, err := v.VerifyMedia(ctx, valid); ok || err != nil {
		t.Fatalf("expected (false, nil) for 0644 file, got (%v, %v)", ok, err)
	}
	must(t, os.Chmod(full, 0o600))

	// Hardlink (Nlink == 2) -> false, nil.
	hardlink := filepath.Join(rootDir, "downloads", "hardlink.m4b")
	must(t, os.Link(full, hardlink))
	if ok, err := v.VerifyMedia(ctx, valid); ok || err != nil {
		t.Fatalf("expected (false, nil) for hardlinked file, got (%v, %v)", ok, err)
	}
	must(t, os.Remove(hardlink))

	// Unsafe directory mode (0755) -> false, nil.
	subDir := filepath.Join(rootDir, "downloads", "sub")
	must(t, os.Chmod(subDir, 0o755))
	if ok, err := v.VerifyMedia(ctx, valid); ok || err != nil {
		t.Fatalf("expected (false, nil) for 0755 subdir, got (%v, %v)", ok, err)
	}
	must(t, os.Chmod(subDir, 0o700))

	// Symlink leaf -> false, nil.
	symMedia := store.LocalMedia{Key: k, RelativePath: "downloads/sym.m4b", Format: "m4b", PublishedAtMS: 1}
	must(t, os.Symlink(full, filepath.Join(rootDir, "downloads", "sym.m4b")))
	if ok, err := v.VerifyMedia(ctx, symMedia); ok || err != nil {
		t.Fatalf("expected (false, nil) for symlink leaf, got (%v, %v)", ok, err)
	}

	// Symlink directory -> false, nil.
	must(t, os.Symlink(subDir, filepath.Join(rootDir, "downloads", "symdir")))
	symDirMedia := store.LocalMedia{Key: k, RelativePath: "downloads/symdir/B000000001.m4b", Format: "m4b", PublishedAtMS: 1}
	if ok, err := v.VerifyMedia(ctx, symDirMedia); ok || err != nil {
		t.Fatalf("expected (false, nil) for symlink dir, got (%v, %v)", ok, err)
	}

	// Directory at leaf path -> false, nil.
	must(t, os.Mkdir(filepath.Join(rootDir, "downloads", "dirleaf.m4b"), 0o700))
	dirLeafMedia := store.LocalMedia{Key: k, RelativePath: "downloads/dirleaf.m4b", Format: "m4b", PublishedAtMS: 1}
	if ok, err := v.VerifyMedia(ctx, dirLeafMedia); ok || err != nil {
		t.Fatalf("expected (false, nil) for directory leaf, got (%v, %v)", ok, err)
	}

	// Unsafe root directory -> store.ErrUnsafeRoot.
	must(t, os.Chmod(rootDir, 0o755))
	if ok, err := v.VerifyMedia(ctx, valid); ok || !errors.Is(err, store.ErrUnsafeRoot) {
		t.Fatalf("expected ErrUnsafeRoot, got (%v, %v)", ok, err)
	}
	must(t, os.Chmod(rootDir, 0o700))
}

func TestStartupReconciliationAndQueuePreservation(t *testing.T) {
	rootDir, owner, st := openTestStore(t)
	ctx := context.Background()
	clock := newFakeClock(1000)

	kReady := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	kMissing := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	kUnsafe := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000003"}
	kWait1 := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000004"}
	kWait2 := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000005"}
	kHist := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000006"}

	// Seed valid private file for kReady, missing file for kMissing, unsafe 0644 file for kUnsafe.
	writePrivateMedia(t, rootDir, "downloads/sub/B000000001.m4b")
	must(t, st.PutLocalMedia(ctx, store.LocalMedia{
		Key: kReady, RelativePath: "downloads/sub/B000000001.m4b", Format: "m4b", PublishedAtMS: 100,
	}))
	must(t, st.PutLocalMedia(ctx, store.LocalMedia{
		Key: kMissing, RelativePath: "downloads/B000000002.m4b", Format: "m4b", PublishedAtMS: 100,
	}))
	writePrivateMedia(t, rootDir, "downloads/B000000003.m4b")
	must(t, os.Chmod(filepath.Join(rootDir, "downloads", "B000000003.m4b"), 0o644))
	must(t, st.PutLocalMedia(ctx, store.LocalMedia{
		Key: kUnsafe, RelativePath: "downloads/B000000003.m4b", Format: "m4b", PublishedAtMS: 100,
	}))

	// Seed interrupted active job for kReady (note: SQLite allows only one active job at a time,
	// so we test kReady first, then restart with kMissing, then kUnsafe, plus waiting queue).
	must(t, st.PutDownloadJob(ctx, store.DownloadJob{
		ID: "active-ready", Key: kReady, State: store.JobPublishing, BytesReceived: 500, BytesTotal: int64Ptr(500), CreatedAtMS: 10, UpdatedAtMS: 20,
	}))
	must(t, st.PutDownloadJob(ctx, store.DownloadJob{
		ID: "wait-2", Key: kWait2, QueueOrder: int64Ptr(5), State: store.JobWaiting, CreatedAtMS: 30, UpdatedAtMS: 30,
	}))
	must(t, st.PutDownloadJob(ctx, store.DownloadJob{
		ID: "wait-1", Key: kWait1, QueueOrder: int64Ptr(2), State: store.JobWaiting, CreatedAtMS: 40, UpdatedAtMS: 40,
	}))
	must(t, st.PutDownloadJob(ctx, store.DownloadJob{
		ID: "hist-fail", Key: kHist, State: store.JobFailed, FailureCode: store.FailureNetwork, CreatedAtMS: 5, UpdatedAtMS: 6,
	}))

	started := make(chan string, 4)
	release := make(chan struct{})
	dl := &fakeDownloader{
		fn: func(ctx context.Context, opID string, key store.BookKey, report func(DownloadEvent)) (DownloadOutcome, error) {
			started <- key.ID
			<-release
			return DownloadOutcome{Media: &PublishedMedia{RelativePath: "downloads/" + key.ID + ".m4b", Format: "m4b"}}, nil
		},
	}

	coord, err := New(ctx, Options{
		Store:      st,
		Downloader: dl,
		RootOwner:  owner,
		Clock:      clock.Now,
	})
	must(t, err)

	// First waiting job (lowest queue_order = 2 -> kWait1) must start first; kWait2 stays waiting.
	if got := <-started; got != kWait1.ID {
		t.Fatalf("expected %s to start first, got %s", kWait1.ID, got)
	}
	snap := coord.Snapshot()
	if snap.Active == nil || snap.Active.Key != kWait1 {
		t.Fatalf("expected active %v, got %+v", kWait1, snap.Active)
	}
	if len(snap.Waiting) != 1 || snap.Waiting[0].Key != kWait2 {
		t.Fatalf("expected waiting [%v], got %+v", kWait2, snap.Waiting)
	}
	byID := make(map[string]Job)
	for _, h := range snap.History {
		byID[h.ID] = h
	}
	if byID["active-ready"].State != store.JobReady || byID["active-ready"].FailureCode != "" {
		t.Fatalf("expected active-ready reconciled to JobReady, got %+v", byID["active-ready"])
	}
	if byID["hist-fail"].FailureMessage == "" || byID["hist-fail"].RetryDisposition != "user_retry" {
		t.Fatalf("expected populated failure metadata on restored history, got %+v", byID["hist-fail"])
	}

	close(release)
	if got := <-started; got != kWait2.ID {
		t.Fatalf("expected %s to start second, got %s", kWait2.ID, got)
	}
	must(t, coord.Close(ctx))

	// Now test interrupted active jobs with missing and unsafe files across restarts.
	for _, tc := range []struct {
		id  string
		key store.BookKey
	}{
		{"active-missing", kMissing},
		{"active-unsafe", kUnsafe},
	} {
		must(t, st.PutDownloadJob(ctx, store.DownloadJob{
			ID: tc.id, Key: tc.key, State: store.JobDownloading, BytesReceived: 123, CreatedAtMS: 500, UpdatedAtMS: 500,
		}))
		c2, err := New(ctx, Options{Store: st, Downloader: dl, RootOwner: owner, Clock: clock.Now})
		must(t, err)
		s2 := c2.Snapshot()
		var found *Job
		for i := range s2.History {
			if s2.History[i].ID == tc.id {
				found = &s2.History[i]
			}
		}
		if found == nil || found.State != store.JobFailed || found.FailureCode != store.FailureInterrupted {
			t.Fatalf("%s: expected JobFailed/FailureInterrupted, got %+v", tc.id, found)
		}
		must(t, c2.Close(ctx))
	}
}

func TestConcurrentEnqueueDeduplicationAndSequentialExecution(t *testing.T) {
	rootDir, owner, st := openTestStore(t)
	ctx := context.Background()
	clock := newFakeClock(2000)

	kReady := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	kStale := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	k1 := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000003"}
	k2 := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000004"}

	writePrivateMedia(t, rootDir, "downloads/B000000001.m4b")
	must(t, st.PutLocalMedia(ctx, store.LocalMedia{
		Key: kReady, RelativePath: "downloads/B000000001.m4b", Format: "m4b", PublishedAtMS: 100,
	}))
	// Stale local_media row with missing file on disk must allow re-download.
	must(t, st.PutLocalMedia(ctx, store.LocalMedia{
		Key: kStale, RelativePath: "downloads/B000000002.m4b", Format: "m4b", PublishedAtMS: 100,
	}))

	gate := make(chan struct{})
	entered := make(chan string, 4)
	dl := &fakeDownloader{
		fn: func(ctx context.Context, opID string, key store.BookKey, report func(DownloadEvent)) (DownloadOutcome, error) {
			entered <- key.ID
			if key == k1 {
				<-gate
			}
			rel := "downloads/" + key.ID + ".m4b"
			writePrivateMedia(t, rootDir, rel)
			return DownloadOutcome{Media: &PublishedMedia{RelativePath: rel, Format: "m4b"}}, nil
		},
	}

	coord, err := New(ctx, Options{Store: st, Downloader: dl, RootOwner: owner, Clock: clock.Now})
	must(t, err)
	defer func() { must(t, coord.Close(ctx)) }()

	// Input validation & already-downloaded check.
	if _, err := coord.Enqueue(nil, k1); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for nil ctx, got %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := coord.Enqueue(canceled, k1); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if _, err := coord.Enqueue(ctx, store.BookKey{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for invalid key, got %v", err)
	}
	if _, err := coord.Enqueue(ctx, kReady); !errors.Is(err, ErrAlreadyDownloaded) {
		t.Fatalf("expected ErrAlreadyDownloaded, got %v", err)
	}

	// Concurrent enqueue for k1: exactly 1 succeeds, others get ErrAlreadyQueued with existing job.
	const workers = 12
	var wg sync.WaitGroup
	var okCount atomic.Int32
	var dupCount atomic.Int32
	var createdID string
	var idMu sync.Mutex
	for range workers {
		wg.Go(func() {
			job, err := coord.Enqueue(ctx, k1)
			switch {
			case err == nil:
				okCount.Add(1)
				idMu.Lock()
				createdID = job.ID
				idMu.Unlock()
			case errors.Is(err, ErrAlreadyQueued):
				dupCount.Add(1)
				if job.Key != k1 || job.ID == "" {
					t.Errorf("expected existing job on ErrAlreadyQueued, got %+v", job)
				}
			default:
				t.Errorf("unexpected enqueue error: %v", err)
			}
		})
	}
	wg.Wait()
	if okCount.Load() != 1 || dupCount.Load() != workers-1 {
		t.Fatalf("unexpected concurrent enqueue counts: ok=%d dup=%d", okCount.Load(), dupCount.Load())
	}

	if got := <-entered; got != k1.ID {
		t.Fatalf("expected %s active, got %s", k1.ID, got)
	}

	// Enqueue k2 and kStale while k1 is active; duplicate enqueue for waiting k2 returns ErrAlreadyQueued.
	j2, err := coord.Enqueue(ctx, k2)
	must(t, err)
	if j2.QueueOrder == nil || *j2.QueueOrder != 0 {
		t.Fatalf("expected queue_order 0, got %+v", j2.QueueOrder)
	}
	if dup, err := coord.Enqueue(ctx, k2); !errors.Is(err, ErrAlreadyQueued) || dup.ID != j2.ID {
		t.Fatalf("expected ErrAlreadyQueued for waiting job %s, got (%+v, %v)", j2.ID, dup, err)
	}
	jStale, err := coord.Enqueue(ctx, kStale)
	must(t, err)
	if jStale.QueueOrder == nil || *jStale.QueueOrder != 1 {
		t.Fatalf("expected queue_order 1, got %+v", jStale.QueueOrder)
	}

	// Snapshot immutability check: mutating returned snapshot must not affect coordinator.
	snap := coord.Snapshot()
	snap.Active.ID = "mutated"
	*snap.Waiting[0].QueueOrder = 999
	snap2 := coord.Snapshot()
	if snap2.Active.ID != createdID || *snap2.Waiting[0].QueueOrder != 0 {
		t.Fatalf("snapshot mutation leaked into coordinator: %+v", snap2)
	}

	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()
	ch := coord.Watch(watchCtx)

	close(gate)
	if got := <-entered; got != k2.ID {
		t.Fatalf("expected %s second, got %s", k2.ID, got)
	}
	if got := <-entered; got != kStale.ID {
		t.Fatalf("expected %s third, got %s", kStale.ID, got)
	}

	finalSnap := waitForSnapshot(t, ch, func(s Snapshot) bool {
		return s.Active == nil && len(s.Waiting) == 0 && len(s.History) == 3
	})
	for _, h := range finalSnap.History {
		if h.State != store.JobReady {
			t.Fatalf("expected JobReady, got %+v", h)
		}
	}

	calls, maxConc := dl.snapshotCalls()
	if maxConc != 1 {
		t.Fatalf("expected single sequential worker (maxConcurrent=1), got %d", maxConc)
	}
	if !reflect.DeepEqual(calls, []string{k1.ID, k2.ID, kStale.ID}) {
		t.Fatalf("unexpected execution order: %v", calls)
	}

	// Now k1 is downloaded and verified on disk -> Enqueue returns ErrAlreadyDownloaded.
	if _, err := coord.Enqueue(ctx, k1); !errors.Is(err, ErrAlreadyDownloaded) {
		t.Fatalf("expected ErrAlreadyDownloaded after completion, got %v", err)
	}
}

func TestProgressStagesStaleEventsAndWatchDelivery(t *testing.T) {
	rootDir, owner, st := openTestStore(t)
	ctx := context.Background()
	clock := newFakeClock(3000)

	kKnown := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	kUnknown := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}

	var lateReport func(DownloadEvent)
	var lateOpID string
	var reportMu sync.Mutex

	dl := &fakeDownloader{
		fn: func(ctx context.Context, opID string, key store.BookKey, report func(DownloadEvent)) (DownloadOutcome, error) {
			reportMu.Lock()
			lateReport = report
			lateOpID = opID
			reportMu.Unlock()

			// Ignored lifecycle events & stale/invalid events before downloading.
			report(DownloadEvent{OperationID: opID, Sequence: 1, Kind: EventStarted})
			report(DownloadEvent{OperationID: "wrong-op", Sequence: 2, Kind: EventStage, Stage: StageDownloading})
			report(DownloadEvent{OperationID: opID, Sequence: 2, Kind: EventStage, Stage: Stage("bogus")})
			report(DownloadEvent{OperationID: opID, Sequence: 2, Kind: EventStage, Stage: StageLicensing})  // same rank
			report(DownloadEvent{OperationID: opID, Sequence: 2, Kind: EventProgress, Bytes: int64Ptr(10)}) // outside downloading

			report(DownloadEvent{OperationID: opID, Sequence: 3, Kind: EventStage, Stage: StageDownloading})
			// Invalid progress samples rejected.
			report(DownloadEvent{OperationID: opID, Sequence: 2, Kind: EventProgress, Bytes: int64Ptr(10)}) // stale seq
			report(DownloadEvent{OperationID: opID, Sequence: 4, Kind: EventProgress, Bytes: nil})
			report(DownloadEvent{OperationID: opID, Sequence: 4, Kind: EventProgress, Bytes: int64Ptr(-1)})
			report(DownloadEvent{OperationID: opID, Sequence: 4, Kind: EventProgress, Bytes: int64Ptr(50), Total: int64Ptr(40)})

			if key == kKnown {
				report(DownloadEvent{OperationID: opID, Sequence: 4, Kind: EventProgress, Bytes: int64Ptr(100), Total: int64Ptr(400)})
				report(DownloadEvent{OperationID: opID, Sequence: 5, Kind: EventProgress, Bytes: int64Ptr(50), Total: int64Ptr(400)})  // backwards bytes
				report(DownloadEvent{OperationID: opID, Sequence: 5, Kind: EventProgress, Bytes: int64Ptr(200), Total: int64Ptr(500)}) // inconsistent total
				report(DownloadEvent{OperationID: opID, Sequence: 5, Kind: EventProgress, Bytes: int64Ptr(250), Total: int64Ptr(400)})
				report(DownloadEvent{OperationID: opID, Sequence: 6, Kind: EventProgress, Bytes: int64Ptr(400), Total: int64Ptr(400)})
			} else {
				// Unknown total stays nil.
				report(DownloadEvent{OperationID: opID, Sequence: 4, Kind: EventProgress, Bytes: int64Ptr(128)})
				report(DownloadEvent{OperationID: opID, Sequence: 5, Kind: EventProgress, Bytes: int64Ptr(256)})
			}

			report(DownloadEvent{OperationID: opID, Sequence: 7, Kind: EventStage, Stage: StagePreparing})
			// Backwards stage & progress during preparing rejected.
			report(DownloadEvent{OperationID: opID, Sequence: 8, Kind: EventStage, Stage: StageDownloading})
			report(DownloadEvent{OperationID: opID, Sequence: 8, Kind: EventProgress, Bytes: int64Ptr(999)})

			report(DownloadEvent{OperationID: opID, Sequence: 9, Kind: EventStage, Stage: StagePublishing})
			report(DownloadEvent{OperationID: opID, Sequence: 10, Kind: EventCompleted})

			rel := "downloads/" + key.ID + ".m4b"
			writePrivateMedia(t, rootDir, rel)
			return DownloadOutcome{Media: &PublishedMedia{RelativePath: rel, Format: "m4b"}}, nil
		},
	}

	coord, err := New(ctx, Options{Store: st, Downloader: dl, RootOwner: owner, Clock: clock.Now})
	must(t, err)

	// Watch edge cases: nil ctx, canceled ctx, stalled watcher, active watcher.
	if _, ok := <-coord.Watch(nil); ok {
		t.Fatal("expected closed channel for Watch(nil)")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, ok := <-coord.Watch(canceled); ok {
		t.Fatal("expected closed channel for Watch(canceled)")
	}

	stalledCtx, stalledCancel := context.WithCancel(ctx)
	defer stalledCancel()
	_ = coord.Watch(stalledCtx) // intentionally never read

	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()
	ch := coord.Watch(watchCtx)

	jKnown, err := coord.Enqueue(ctx, kKnown)
	must(t, err)

	var observed []Snapshot
	doneKnown := waitForSnapshot(t, ch, func(s Snapshot) bool {
		observed = append(observed, s)
		return s.Active == nil && len(s.History) == 1
	})
	if doneKnown.History[0].State != store.JobReady ||
		doneKnown.History[0].BytesReceived != 400 ||
		doneKnown.History[0].BytesTotal == nil || *doneKnown.History[0].BytesTotal != 400 {
		t.Fatalf("unexpected known-total final job: %+v", doneKnown.History[0])
	}

	// Verify every stage transition was delivered without dropping any stage.
	var stages []store.JobState
	for _, s := range observed {
		if s.Active != nil && s.Active.ID == jKnown.ID {
			if len(stages) == 0 || stages[len(stages)-1] != s.Active.State {
				stages = append(stages, s.Active.State)
			}
		}
	}
	wantStages := []store.JobState{store.JobLicensing, store.JobDownloading, store.JobPreparing, store.JobPublishing}
	if !reflect.DeepEqual(stages, wantStages) {
		t.Fatalf("dropped or misordered stages: got %v, want %v", stages, wantStages)
	}

	// Late event after Download returned must be ignored cleanly.
	reportMu.Lock()
	lateReport(DownloadEvent{OperationID: lateOpID, Sequence: 99, Kind: EventStage, Stage: StageDownloading})
	reportMu.Unlock()

	// Unknown total job.
	_, err = coord.Enqueue(ctx, kUnknown)
	must(t, err)
	doneUnknown := waitForSnapshot(t, ch, func(s Snapshot) bool {
		return s.Active == nil && len(s.History) == 2
	})
	var unknownJob Job
	for _, h := range doneUnknown.History {
		if h.Key == kUnknown {
			unknownJob = h
		}
	}
	if unknownJob.BytesReceived != 256 || unknownJob.BytesTotal != nil {
		t.Fatalf("expected unknown total nil and bytes 256, got %+v", unknownJob)
	}

	// Cancel watchCtx and verify channel closes cleanly.
	watchCancel()
	for range ch {
	}

	// Close must not block on stalled watcher.
	must(t, coord.Close(ctx))
	if _, ok := <-coord.Watch(ctx); ok {
		t.Fatal("expected closed channel for Watch after Close")
	}
}

func TestCancelActiveAndCommitPointRace(t *testing.T) {
	rootDir, owner, st := openTestStore(t)
	ctx := context.Background()
	clock := newFakeClock(4000)

	kPreCommit := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	kPostCommit := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	kPostWarn := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000003"}
	kNext := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000004"}

	inDownload := make(chan store.BookKey, 4)
	releasePre := make(chan struct{})
	releasePost := make(chan struct{})
	releaseWarn := make(chan struct{})

	dl := &fakeDownloader{
		fn: func(opCtx context.Context, opID string, key store.BookKey, report func(DownloadEvent)) (DownloadOutcome, error) {
			report(DownloadEvent{OperationID: opID, Sequence: 1, Kind: EventStage, Stage: StageDownloading})
			report(DownloadEvent{OperationID: opID, Sequence: 2, Kind: EventProgress, Bytes: int64Ptr(50), Total: int64Ptr(100)})
			inDownload <- key

			switch key {
			case kPreCommit:
				<-opCtx.Done()
				<-releasePre
				return DownloadOutcome{}, opCtx.Err()
			case kPostCommit:
				<-opCtx.Done()
				<-releasePost
				rel := "downloads/" + key.ID + ".m4b"
				writePrivateMedia(t, rootDir, rel)
				return DownloadOutcome{Media: &PublishedMedia{RelativePath: rel, Format: "m4b"}}, context.Canceled
			case kPostWarn:
				<-opCtx.Done()
				<-releaseWarn
				rel := "downloads/" + key.ID + ".m4b"
				writePrivateMedia(t, rootDir, rel)
				return DownloadOutcome{Media: &PublishedMedia{RelativePath: rel, Format: "m4b"}}, Failure{Code: "storage_uncertain"}
			default:
				rel := "downloads/" + key.ID + ".m4b"
				writePrivateMedia(t, rootDir, rel)
				return DownloadOutcome{Media: &PublishedMedia{RelativePath: rel, Format: "m4b"}}, nil
			}
		},
	}

	coord, err := New(ctx, Options{Store: st, Downloader: dl, RootOwner: owner, Clock: clock.Now})
	must(t, err)
	defer func() { must(t, coord.Close(ctx)) }()

	// CancelActive validation when idle.
	if err := coord.CancelActive(nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := coord.CancelActive(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if err := coord.CancelActive(ctx); !errors.Is(err, ErrNoActiveJob) {
		t.Fatalf("expected ErrNoActiveJob, got %v", err)
	}

	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()
	ch := coord.Watch(watchCtx)

	// 1. Pre-commit cancel: active slot stays occupied with Canceling=true until worker joins.
	_, err = coord.Enqueue(ctx, kPreCommit)
	must(t, err)
	_, err = coord.Enqueue(ctx, kNext)
	must(t, err)

	if got := <-inDownload; got != kPreCommit {
		t.Fatalf("expected %v, got %v", kPreCommit, got)
	}
	must(t, coord.CancelActive(ctx))
	must(t, coord.CancelActive(ctx)) // idempotent while canceling

	snap := coord.Snapshot()
	if snap.Active == nil || !snap.Active.Canceling || snap.Active.State != store.JobDownloading {
		t.Fatalf("expected active Canceling in JobDownloading, got %+v", snap.Active)
	}
	if len(snap.Waiting) != 1 || snap.Waiting[0].Key != kNext {
		t.Fatalf("expected kNext still waiting while active slot occupied, got %+v", snap.Waiting)
	}

	// Release pre-commit cancel -> JobCanceled, then kNext runs and completes.
	close(releasePre)

	if got := <-inDownload; got != kNext {
		t.Fatalf("expected %v after cancel, got %v", kNext, got)
	}
	afterPre := waitForSnapshot(t, ch, func(s Snapshot) bool {
		return s.Active == nil && len(s.History) == 2
	})
	if afterPre.History[0].State != store.JobCanceled {
		t.Fatalf("expected JobCanceled for pre-commit cancel, got %+v", afterPre.History[0])
	}

	// 2. Post-commit cancel race: committed media wins JobReady and persists LocalMedia!
	_, err = coord.Enqueue(ctx, kPostCommit)
	must(t, err)
	if got := <-inDownload; got != kPostCommit {
		t.Fatalf("expected %v, got %v", kPostCommit, got)
	}
	must(t, coord.CancelActive(ctx))
	close(releasePost)

	afterPost := waitForSnapshot(t, ch, func(s Snapshot) bool {
		return s.Active == nil && len(s.History) == 3
	})
	if afterPost.History[2].State != store.JobReady || afterPost.LastError != "" {
		t.Fatalf("expected JobReady without warning on late cancel commit, got %+v (lastErr=%q)", afterPost.History[2], afterPost.LastError)
	}
	if _, found, err := st.LocalMedia(ctx, kPostCommit); err != nil || !found {
		t.Fatalf("expected LocalMedia persisted on commit-point race, got (%v, %v)", found, err)
	}
	if _, err := os.Stat(filepath.Join(rootDir, "downloads", kPostCommit.ID+".m4b")); err != nil {
		t.Fatalf("committed media file was unlinked on late cancel: %v", err)
	}

	// 3. Post-commit with non-cancel warning (e.g. storage_uncertain): still JobReady + LastError notice.
	_, err = coord.Enqueue(ctx, kPostWarn)
	must(t, err)
	if got := <-inDownload; got != kPostWarn {
		t.Fatalf("expected %v, got %v", kPostWarn, got)
	}
	must(t, coord.CancelActive(ctx))
	close(releaseWarn)

	afterWarn := waitForSnapshot(t, ch, func(s Snapshot) bool {
		return s.Active == nil && len(s.History) == 4
	})
	if afterWarn.History[3].State != store.JobReady || afterWarn.LastError == "" {
		t.Fatalf("expected JobReady with post-commit warning, got %+v (lastErr=%q)", afterWarn.History[3], afterWarn.LastError)
	}
}

func TestWaitingQueueRemoveReorderAndMove(t *testing.T) {
	rootDir, owner, st := openTestStore(t)
	ctx := context.Background()
	clock := newFakeClock(5000)

	k1 := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	k2 := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	k3 := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000003"}
	k4 := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000004"}
	kReady := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000005"}

	// Seed independent local state that RemoveWaiting must never touch.
	writePrivateMedia(t, rootDir, "downloads/B000000005.m4b")
	must(t, st.PutLocalMedia(ctx, store.LocalMedia{
		Key: kReady, RelativePath: "downloads/B000000005.m4b", Format: "m4b", PublishedAtMS: 10,
	}))
	must(t, st.SetFavorite(ctx, k3, true, 10))
	must(t, st.PutListeningState(ctx, store.ListeningState{
		Key: k3, PositionNS: 500, SpeedMilli: 1000000, Completion: store.CompletionUnfinished, UpdatedAtMS: 10,
	}))
	must(t, st.PutBookmark(ctx, store.Bookmark{
		ID: "bm-1", Key: k3, PositionNS: 100, Note: "keep me", CreatedAtMS: 10, UpdatedAtMS: 10,
	}))
	must(t, st.PutDownloadJob(ctx, store.DownloadJob{
		ID: "hist-1", Key: kReady, State: store.JobReady, CreatedAtMS: 1, UpdatedAtMS: 2,
	}))

	gate := make(chan struct{})
	entered := make(chan string, 4)
	dl := &fakeDownloader{
		fn: func(ctx context.Context, opID string, key store.BookKey, report func(DownloadEvent)) (DownloadOutcome, error) {
			entered <- key.ID
			if key == k1 {
				<-gate
			}
			rel := "downloads/" + key.ID + ".m4b"
			writePrivateMedia(t, rootDir, rel)
			return DownloadOutcome{Media: &PublishedMedia{RelativePath: rel, Format: "m4b"}}, nil
		},
	}

	fs := &faultStore{JobStore: st}
	coord, err := New(ctx, Options{Store: fs, Downloader: dl, RootOwner: owner, Clock: clock.Now})
	must(t, err)

	j1, err := coord.Enqueue(ctx, k1)
	must(t, err)
	if got := <-entered; got != k1.ID {
		t.Fatalf("expected %s active, got %s", k1.ID, got)
	}
	j2, err := coord.Enqueue(ctx, k2)
	must(t, err)
	j3, err := coord.Enqueue(ctx, k3)
	must(t, err)
	j4, err := coord.Enqueue(ctx, k4)
	must(t, err)

	// Validation errors for RemoveWaiting, ReorderWaiting, MoveWaiting.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, err := range []error{
		coord.RemoveWaiting(nil, j2.ID),
		coord.RemoveWaiting(ctx, " "),
		coord.ReorderWaiting(nil, nil),
		coord.MoveWaiting(nil, j2.ID, 1),
		coord.MoveWaiting(ctx, " ", 1),
	} {
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput, got %v", err)
		}
	}
	for _, err := range []error{
		coord.RemoveWaiting(canceled, j2.ID),
		coord.ReorderWaiting(canceled, nil),
		coord.MoveWaiting(canceled, j2.ID, 1),
	} {
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	}
	if err := coord.RemoveWaiting(ctx, j1.ID); !errors.Is(err, ErrJobNotWaiting) {
		t.Fatalf("expected ErrJobNotWaiting for active job, got %v", err)
	}
	if err := coord.RemoveWaiting(ctx, "hist-1"); !errors.Is(err, ErrJobNotWaiting) {
		t.Fatalf("expected ErrJobNotWaiting for history job, got %v", err)
	}
	if err := coord.RemoveWaiting(ctx, "missing"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("expected ErrJobNotFound, got %v", err)
	}
	if err := coord.MoveWaiting(ctx, j1.ID, 1); !errors.Is(err, ErrJobNotWaiting) {
		t.Fatalf("expected ErrJobNotWaiting for active MoveWaiting, got %v", err)
	}
	if err := coord.MoveWaiting(ctx, "hist-1", 1); !errors.Is(err, ErrJobNotWaiting) {
		t.Fatalf("expected ErrJobNotWaiting for history MoveWaiting, got %v", err)
	}
	if err := coord.MoveWaiting(ctx, "missing", 1); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("expected ErrJobNotFound for MoveWaiting, got %v", err)
	}

	// RemoveWaiting persistence failure preserves waiting queue.
	fs.putDownloadJobErr = errors.New("disk error")
	if err := coord.RemoveWaiting(ctx, j3.ID); err == nil {
		t.Fatal("expected RemoveWaiting error on store failure")
	}
	fs.putDownloadJobErr = nil
	if len(coord.Snapshot().Waiting) != 3 {
		t.Fatalf("expected 3 waiting jobs preserved, got %d", len(coord.Snapshot().Waiting))
	}

	// RemoveWaiting succeeds for j3 without touching active j1 or local state.
	must(t, coord.RemoveWaiting(ctx, j3.ID))
	snap := coord.Snapshot()
	if snap.Active == nil || snap.Active.ID != j1.ID {
		t.Fatalf("active job disturbed by RemoveWaiting: %+v", snap.Active)
	}
	if len(snap.Waiting) != 2 || snap.Waiting[0].ID != j2.ID || snap.Waiting[1].ID != j4.ID {
		t.Fatalf("unexpected waiting queue after RemoveWaiting: %+v", snap.Waiting)
	}
	if favs, err := st.Favorites(ctx); err != nil || len(favs) != 1 || favs[0] != k3 {
		t.Fatalf("favorites disturbed: (%+v, %v)", favs, err)
	}
	if bms, err := st.Bookmarks(ctx, k3); err != nil || len(bms) != 1 {
		t.Fatalf("bookmarks disturbed: (%+v, %v)", bms, err)
	}
	if _, found, err := st.ListeningState(ctx, k3); err != nil || !found {
		t.Fatalf("listening state disturbed: (%v, %v)", found, err)
	}
	if _, found, err := st.LocalMedia(ctx, kReady); err != nil || !found {
		t.Fatalf("local media disturbed: (%v, %v)", found, err)
	}

	// ReorderWaiting invalid inputs.
	for _, bad := range [][]string{
		{j2.ID},
		{j2.ID, j2.ID},
		{j2.ID, "missing"},
	} {
		if err := coord.ReorderWaiting(ctx, bad); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for %v, got %v", bad, err)
		}
	}

	// ReorderWaiting & MoveWaiting persistence failure preserves authoritative order [j2, j4].
	fs.reorderErr = errors.New("reorder failed")
	if err := coord.ReorderWaiting(ctx, []string{j4.ID, j2.ID}); err == nil {
		t.Fatal("expected ReorderWaiting error")
	}
	if err := coord.MoveWaiting(ctx, j4.ID, -1); err == nil {
		t.Fatal("expected MoveWaiting error")
	}
	fs.reorderErr = nil
	snap = coord.Snapshot()
	if snap.Waiting[0].ID != j2.ID || snap.Waiting[1].ID != j4.ID {
		t.Fatalf("authoritative order lost on failure: %+v", snap.Waiting)
	}

	// MoveWaiting boundary no-op and valid move.
	must(t, coord.MoveWaiting(ctx, j2.ID, -1)) // already at top
	must(t, coord.MoveWaiting(ctx, j4.ID, -1)) // moves j4 ahead of j2
	snap = coord.Snapshot()
	if snap.Waiting[0].ID != j4.ID || snap.Waiting[1].ID != j2.ID {
		t.Fatalf("expected [j4, j2], got %+v", snap.Waiting)
	}

	// ReorderWaiting back to [j2, j4], then swap to [j4, j2] and verify execution order.
	must(t, coord.ReorderWaiting(ctx, []string{j2.ID, j4.ID}))
	must(t, coord.ReorderWaiting(ctx, []string{j4.ID, j2.ID}))

	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()
	ch := coord.Watch(watchCtx)

	close(gate)
	if got := <-entered; got != k4.ID {
		t.Fatalf("expected %s next after reorder, got %s", k4.ID, got)
	}
	if got := <-entered; got != k2.ID {
		t.Fatalf("expected %s last after reorder, got %s", k2.ID, got)
	}
	waitForSnapshot(t, ch, func(s Snapshot) bool {
		return s.Active == nil && len(s.Waiting) == 0
	})
	must(t, coord.Close(ctx))

	// Empty queue ReorderWaiting succeeds cleanly.
	cEmpty, err := New(ctx, Options{Store: st, Downloader: dl, RootOwner: owner, Clock: clock.Now})
	must(t, err)
	must(t, cEmpty.ReorderWaiting(ctx, nil))
	must(t, cEmpty.Close(ctx))
}

func TestExplicitRetryAndNoAutoRetry(t *testing.T) {
	rootDir, owner, st := openTestStore(t)
	ctx := context.Background()
	clock := newFakeClock(6000)

	kDenied := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	kThrottled := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	kSecret := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000003"}
	kOther := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000004"}

	var attempt atomic.Int32
	dl := &fakeDownloader{
		fn: func(ctx context.Context, opID string, key store.BookKey, report func(DownloadEvent)) (DownloadOutcome, error) {
			n := attempt.Add(1)
			switch key {
			case kDenied:
				if n == 1 {
					return DownloadOutcome{}, Failure{Code: "entitlement_denied", Retry: "check_entitlement"}
				}
			case kThrottled:
				return DownloadOutcome{}, Failure{Code: "throttled", Retry: "wait"}
			case kSecret:
				return DownloadOutcome{}, errors.New("https://secret.example.com/signed?token=TOPSECRET")
			case kOther:
				return DownloadOutcome{}, Failure{Code: "not_authenticated"}
			}
			rel := "downloads/" + key.ID + ".m4b"
			writePrivateMedia(t, rootDir, rel)
			return DownloadOutcome{Media: &PublishedMedia{RelativePath: rel, Format: "m4b"}}, nil
		},
	}

	coord, err := New(ctx, Options{Store: st, Downloader: dl, RootOwner: owner, Clock: clock.Now})
	must(t, err)
	defer func() { must(t, coord.Close(ctx)) }()

	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()
	ch := coord.Watch(watchCtx)

	jDenied, err := coord.Enqueue(ctx, kDenied)
	must(t, err)
	jThrottled, err := coord.Enqueue(ctx, kThrottled)
	must(t, err)
	jSecret, err := coord.Enqueue(ctx, kSecret)
	must(t, err)
	jOther, err := coord.Enqueue(ctx, kOther)
	must(t, err)

	snap := waitForSnapshot(t, ch, func(s Snapshot) bool {
		return s.Active == nil && len(s.Waiting) == 0 && len(s.History) == 4
	})

	// Verify NO automatic retries occurred (exactly 4 calls total).
	calls, _ := dl.snapshotCalls()
	if len(calls) != 4 {
		t.Fatalf("expected exactly 4 calls (no auto-retry), got %d (%v)", len(calls), calls)
	}

	byID := make(map[string]Job)
	for _, h := range snap.History {
		byID[h.ID] = h
	}
	if byID[jDenied.ID].FailureCode != store.FailureEntitlementDenied || byID[jDenied.ID].RetryDisposition != "check_entitlement" {
		t.Fatalf("unexpected entitlement_denied job: %+v", byID[jDenied.ID])
	}
	if byID[jThrottled.ID].FailureCode != store.FailureThrottled || byID[jThrottled.ID].RetryDisposition != "wait" {
		t.Fatalf("unexpected throttled job: %+v", byID[jThrottled.ID])
	}
	if byID[jSecret.ID].FailureCode != store.FailureInternal || strings.Contains(byID[jSecret.ID].FailureMessage, "TOPSECRET") || strings.Contains(byID[jSecret.ID].FailureMessage, "secret.example.com") {
		t.Fatalf("secret leaked or wrong code: %+v", byID[jSecret.ID])
	}
	if byID[jOther.ID].FailureCode != store.FailureInternal || !strings.Contains(byID[jOther.ID].FailureMessage, "Sign in") {
		t.Fatalf("unexpected not_authenticated mapping: %+v", byID[jOther.ID])
	}

	// Retry validation errors.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := coord.Retry(nil, jDenied.ID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
	if _, err := coord.Retry(canceled, jDenied.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if _, err := coord.Retry(ctx, " "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for blank ID, got %v", err)
	}
	if _, err := coord.Retry(ctx, "missing"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("expected ErrJobNotFound, got %v", err)
	}

	// Explicit Retry on jDenied creates a new queued attempt and succeeds.
	retried, err := coord.Retry(ctx, jDenied.ID)
	must(t, err)
	if retried.ID == jDenied.ID || retried.State != store.JobWaiting {
		t.Fatalf("expected new waiting job on Retry, got %+v", retried)
	}
	snap = waitForSnapshot(t, ch, func(s Snapshot) bool {
		return s.Active == nil && len(s.History) == 5
	})
	var retriedDone Job
	for _, h := range snap.History {
		if h.ID == retried.ID {
			retriedDone = h
		}
	}
	if retriedDone.State != store.JobReady {
		t.Fatalf("expected retried job ready, got %+v", retriedDone)
	}

	// Retrying a JobReady job fails with ErrJobNotRetryable.
	if _, err := coord.Retry(ctx, retried.ID); !errors.Is(err, ErrJobNotRetryable) {
		t.Fatalf("expected ErrJobNotRetryable for JobReady, got %v", err)
	}
	// Retrying the old failed jDenied when media is now ready returns ErrAlreadyDownloaded.
	if _, err := coord.Retry(ctx, jDenied.ID); !errors.Is(err, ErrAlreadyDownloaded) {
		t.Fatalf("expected ErrAlreadyDownloaded, got %v", err)
	}

	// Test Retry when job is active, waiting, or book already has a live job.
	gate := make(chan struct{})
	entered := make(chan string, 2)
	dl.fn = func(ctx context.Context, opID string, key store.BookKey, report func(DownloadEvent)) (DownloadOutcome, error) {
		entered <- key.ID
		<-gate
		return DownloadOutcome{}, Failure{Code: "network"}
	}
	jActive, err := coord.Retry(ctx, jThrottled.ID)
	must(t, err)
	if got := <-entered; got != kThrottled.ID {
		t.Fatalf("expected %s active, got %s", kThrottled.ID, got)
	}
	jWait, err := coord.Retry(ctx, jSecret.ID)
	must(t, err)
	if _, err := coord.Retry(ctx, jActive.ID); !errors.Is(err, ErrJobNotRetryable) {
		t.Fatalf("expected ErrJobNotRetryable for active job, got %v", err)
	}
	if _, err := coord.Retry(ctx, jWait.ID); !errors.Is(err, ErrJobNotRetryable) {
		t.Fatalf("expected ErrJobNotRetryable for waiting job, got %v", err)
	}
	if _, err := coord.Retry(ctx, jThrottled.ID); !errors.Is(err, ErrAlreadyQueued) {
		t.Fatalf("expected ErrAlreadyQueued when book already live, got %v", err)
	}
	close(gate)
	waitForSnapshot(t, ch, func(s Snapshot) bool {
		return s.Active == nil && len(s.Waiting) == 0 && len(s.History) == 7
	})

	// Verify safeFailureMessage & retryDispositionFor coverage across all codes.
	for _, code := range []string{
		"busy", "runtime_missing", "runtime_incompatible", "protocol_error", "unsupported",
		"disk_full", "permission_denied", "preparation_failed", "interrupted",
	} {
		msg := safeFailureMessage(store.NormalizeFailureCode(code), code)
		if msg == "" {
			t.Fatalf("empty message for %s", code)
		}
	}
	if got := retryDispositionFor(store.FailureInternal, "sign_in"); got != "sign_in" {
		t.Fatalf("expected sign_in, got %s", got)
	}
	if got := retryDispositionFor(store.FailureEntitlementDenied, ""); got != "check_entitlement" {
		t.Fatalf("expected check_entitlement, got %s", got)
	}
	if got := retryDispositionFor(store.FailureThrottled, ""); got != "wait" {
		t.Fatalf("expected wait, got %s", got)
	}
	if msg := (Failure{Code: "entitlement_denied"}).Error(); msg == "" {
		t.Fatal("expected non-empty Failure.Error()")
	}
}

func TestStoreFaultsAndInvalidOutcomes(t *testing.T) {
	_, owner, st := openTestStore(t)
	ctx := context.Background()
	clock := newFakeClock(7000)

	k1 := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	k2 := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	k3 := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000003"}

	fs := &faultStore{JobStore: st}
	dl := &fakeDownloader{
		fn: func(ctx context.Context, opID string, key store.BookKey, report func(DownloadEvent)) (DownloadOutcome, error) {
			switch key {
			case k1:
				// Invalid relative path returned by Downloader -> fail closed.
				return DownloadOutcome{Media: &PublishedMedia{RelativePath: "../escape.m4b", Format: "m4b"}}, nil
			case k2:
				// Valid media, but PutLocalMedia fails -> fail closed.
				return DownloadOutcome{Media: &PublishedMedia{RelativePath: "downloads/B000000002.m4b", Format: "m4b"}}, nil
			default:
				// Neither media nor error -> fail closed, plus stage persistence error.
				report(DownloadEvent{OperationID: opID, Sequence: 1, Kind: EventStage, Stage: StageDownloading})
				return DownloadOutcome{}, nil
			}
		},
	}

	coord, err := New(ctx, Options{Store: fs, Downloader: dl, RootOwner: owner, Clock: clock.Now})
	must(t, err)
	defer func() { must(t, coord.Close(ctx)) }()

	// Enqueue store errors.
	fs.localMediaErr = errors.New("read err")
	if _, err := coord.Enqueue(ctx, k1); err == nil {
		t.Fatal("expected LocalMedia error")
	}
	fs.localMediaErr = nil

	fs.putDownloadJobErr = errors.New("write err")
	if _, err := coord.Enqueue(ctx, k1); err == nil {
		t.Fatal("expected PutDownloadJob error")
	}
	fs.putDownloadJobErr = nil

	// Enqueue with MediaChecker error.
	cErr, err := New(ctx, Options{
		Store:      st,
		Downloader: dl,
		MediaChecker: fakeChecker{fn: func(context.Context, store.LocalMedia) (bool, error) {
			return false, errors.New("verify err")
		}},
	})
	must(t, err)
	must(t, st.PutLocalMedia(ctx, store.LocalMedia{
		Key: k3, RelativePath: "downloads/B000000003.m4b", Format: "m4b", PublishedAtMS: 1,
	}))
	if _, err := cErr.Enqueue(ctx, k3); err == nil {
		t.Fatal("expected MediaChecker error on Enqueue")
	}
	must(t, cErr.Close(ctx))

	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()
	ch := coord.Watch(watchCtx)

	// k1: invalid PublishedMedia -> JobFailed/FailureInternal.
	_, err = coord.Enqueue(ctx, k1)
	must(t, err)
	snap := waitForSnapshot(t, ch, func(s Snapshot) bool {
		return s.Active == nil && len(s.History) == 1
	})
	if snap.History[0].State != store.JobFailed || snap.History[0].FailureCode != store.FailureInternal {
		t.Fatalf("expected JobFailed/FailureInternal for invalid media, got %+v", snap.History[0])
	}

	// k2: PutLocalMedia failure -> JobFailed/FailureInternal + LastError.
	fs.putLocalMediaErr = errors.New("media write err")
	_, err = coord.Enqueue(ctx, k2)
	must(t, err)
	snap = waitForSnapshot(t, ch, func(s Snapshot) bool {
		return s.Active == nil && len(s.History) == 2
	})
	fs.putLocalMediaErr = nil
	if snap.History[1].State != store.JobFailed || snap.LastError == "" {
		t.Fatalf("expected JobFailed with LastError on PutLocalMedia failure, got %+v (lastErr=%q)", snap.History[1], snap.LastError)
	}

	// k3: stage PutDownloadJob failure + terminal PutDownloadJob failure.
	fs.putDownloadJobErr = errors.New("stage/terminal write err")
	fs.putDownloadJobOnCall = fs.putDownloadJobCalls + 3 // allow Enqueue + claimNextJob, fail stage PutDownloadJob
	_, err = coord.Enqueue(ctx, k3)
	must(t, err)
	snap = waitForSnapshot(t, ch, func(s Snapshot) bool {
		return s.Active == nil && len(s.History) == 3
	})
	fs.putDownloadJobErr = nil
	fs.putDownloadJobOnCall = 0
	if snap.History[2].State != store.JobFailed || snap.LastError == "" {
		t.Fatalf("expected JobFailed with LastError, got %+v (lastErr=%q)", snap.History[2], snap.LastError)
	}

	// Terminal PutDownloadJob failure during Close propagates error to Close.
	kTerm := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000005"}
	inTerm := make(chan struct{})
	dlTerm := &fakeDownloader{
		fn: func(opCtx context.Context, opID string, key store.BookKey, report func(DownloadEvent)) (DownloadOutcome, error) {
			close(inTerm)
			<-opCtx.Done()
			return DownloadOutcome{}, opCtx.Err()
		},
	}
	fsTerm := &faultStore{JobStore: st}
	cTerm, err := New(ctx, Options{Store: fsTerm, Downloader: dlTerm, RootOwner: owner, Clock: clock.Now})
	must(t, err)
	_, err = cTerm.Enqueue(ctx, kTerm)
	must(t, err)
	<-inTerm
	fsTerm.putDownloadJobErr = errors.New("terminal write err")
	if err := cTerm.Close(ctx); err == nil {
		t.Fatal("expected Close to return terminal persistence error")
	}

	// claimNextJob PutDownloadJob failure sets LastError without spinning.
	k4 := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000004"}
	fs.putDownloadJobErr = errors.New("claim write err")
	fs.putDownloadJobOnCall = fs.putDownloadJobCalls + 2 // allow Enqueue, fail claimNextJob
	_, err = coord.Enqueue(ctx, k4)
	must(t, err)
	snap = waitForSnapshot(t, ch, func(s Snapshot) bool {
		return s.LastError == "failed to persist download start"
	})
	fs.putDownloadJobErr = nil
	fs.putDownloadJobOnCall = 0
	if len(snap.Waiting) != 1 || snap.Waiting[0].Key != k4 {
		t.Fatalf("expected k4 still waiting after claim failure, got %+v", snap.Waiting)
	}
}

func TestCloseCancellationJoinIdempotencyAndRestart(t *testing.T) {
	rootDir, owner, st := openTestStore(t)
	ctx := context.Background()
	clock := newFakeClock(8000)

	k1 := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	k2 := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}

	inDownload := make(chan struct{})
	release := make(chan struct{})
	dl := &fakeDownloader{
		fn: func(opCtx context.Context, opID string, key store.BookKey, report func(DownloadEvent)) (DownloadOutcome, error) {
			if key == k1 {
				close(inDownload)
				<-opCtx.Done()
				<-release
				return DownloadOutcome{}, opCtx.Err()
			}
			rel := "downloads/" + key.ID + ".m4b"
			writePrivateMedia(t, rootDir, rel)
			return DownloadOutcome{Media: &PublishedMedia{RelativePath: rel, Format: "m4b"}}, nil
		},
	}

	coord, err := New(ctx, Options{Store: st, Downloader: dl, RootOwner: owner, Clock: clock.Now})
	must(t, err)

	_, err = coord.Enqueue(ctx, k1)
	must(t, err)
	_, err = coord.Enqueue(ctx, k2)
	must(t, err)
	<-inDownload

	// Canceled Close returns context.Canceled without closing.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := coord.Close(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// Short-timeout Close initiates shutdown/cancel and returns DeadlineExceeded while worker is blocked.
	shortCtx, shortCancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer shortCancel()
	if err := coord.Close(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}

	// Operations while closing return ErrClosed.
	if _, err := coord.Enqueue(ctx, k2); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed on Enqueue, got %v", err)
	}
	if _, err := coord.Retry(ctx, "any"); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed on Retry, got %v", err)
	}
	if err := coord.CancelActive(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed on CancelActive, got %v", err)
	}
	if err := coord.RemoveWaiting(ctx, "any"); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed on RemoveWaiting, got %v", err)
	}
	if err := coord.ReorderWaiting(ctx, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed on ReorderWaiting, got %v", err)
	}
	if err := coord.MoveWaiting(ctx, "any", 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed on MoveWaiting, got %v", err)
	}

	// Release worker and retry Close concurrently & repeatedly.
	close(release)
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if err := coord.Close(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	must(t, coord.Close(nil)) // idempotent even with nil ctx

	// Restart coordinator with the SAME frozen clock: k1 is JobCanceled, k2 resumes and completes without ID collision!
	coord2, err := New(ctx, Options{Store: st, Downloader: dl, RootOwner: owner, Clock: clock.Now})
	must(t, err)
	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()
	ch2 := coord2.Watch(watchCtx)
	snap2 := coord2.Snapshot()
	if snap2.Active == nil && len(snap2.Waiting) == 0 && len(snap2.History) == 2 {
		// Already finished before Watch subscribed.
	} else {
		snap2 = waitForSnapshot(t, ch2, func(s Snapshot) bool {
			return s.Active == nil && len(s.Waiting) == 0 && len(s.History) == 2
		})
	}
	if snap2.History[0].State != store.JobCanceled || snap2.History[1].State != store.JobReady {
		t.Fatalf("unexpected post-restart history: %+v", snap2.History)
	}
	// Enqueue a new book with the same frozen clock -> unique ID generated cleanly.
	k3 := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000003"}
	j3, err := coord2.Enqueue(ctx, k3)
	must(t, err)
	if j3.ID == "" {
		t.Fatal("expected non-empty job ID")
	}
	waitForSnapshot(t, ch2, func(s Snapshot) bool {
		return s.Active == nil && len(s.History) == 3
	})
	must(t, coord2.Close(ctx))
}

func TestCloseDuringClaimNextJobRestoresWaiting(t *testing.T) {
	_, owner, st := openTestStore(t)
	ctx := context.Background()
	clock := newFakeClock(9000)

	k1 := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	claiming := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once

	fs := &faultStore{
		JobStore: st,
		onPutDownloadJobBefore: func(job store.DownloadJob) {
			if job.State == store.JobLicensing {
				once.Do(func() {
					close(claiming)
					<-proceed
				})
			}
		},
	}
	dl := &fakeDownloader{
		fn: func(context.Context, string, store.BookKey, func(DownloadEvent)) (DownloadOutcome, error) {
			t.Error("Downloader should not be invoked when Close races claimNextJob")
			return DownloadOutcome{}, nil
		},
	}

	coord, err := New(ctx, Options{Store: fs, Downloader: dl, RootOwner: owner, Clock: clock.Now})
	must(t, err)

	_, err = coord.Enqueue(ctx, k1)
	must(t, err)
	<-claiming

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- coord.Close(ctx)
	}()
	// Wait until Close sets closing=true before letting claimNextJob finish PutDownloadJob.
	for {
		coord.stateMu.Lock()
		closing := coord.closing
		coord.stateMu.Unlock()
		if closing {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(proceed)
	must(t, <-closeDone)

	// Verify waiting job was restored to JobWaiting in SQLite.
	jobs, err := st.DownloadJobs(ctx)
	must(t, err)
	if len(jobs) != 1 || jobs[0].State != store.JobWaiting || jobs[0].QueueOrder == nil {
		t.Fatalf("expected waiting job restored on Close race, got %+v", jobs)
	}
}

func TestStalledWatcherOverflowAndClose(t *testing.T) {
	rootDir, owner, st := openTestStore(t)
	ctx := context.Background()
	clock := newFakeClock(10000)

	dl := &fakeDownloader{
		fn: func(ctx context.Context, opID string, key store.BookKey, report func(DownloadEvent)) (DownloadOutcome, error) {
			report(DownloadEvent{OperationID: opID, Sequence: 1, Kind: EventStage, Stage: StageDownloading})
			report(DownloadEvent{OperationID: opID, Sequence: 2, Kind: EventProgress, Bytes: int64Ptr(10), Total: int64Ptr(20)})
			report(DownloadEvent{OperationID: opID, Sequence: 3, Kind: EventStage, Stage: StagePreparing})
			report(DownloadEvent{OperationID: opID, Sequence: 4, Kind: EventStage, Stage: StagePublishing})
			rel := "downloads/" + key.ID + ".m4b"
			writePrivateMedia(t, rootDir, rel)
			return DownloadOutcome{Media: &PublishedMedia{RelativePath: rel, Format: "m4b"}}, nil
		},
	}

	coord, err := New(ctx, Options{Store: st, Downloader: dl, RootOwner: owner, Clock: clock.Now})
	must(t, err)

	// Stalled watcher never reads from ch; >64 important updates overflow the 64-item buffer.
	stalledCtx, stalledCancel := context.WithCancel(ctx)
	defer stalledCancel()
	_ = coord.Watch(stalledCtx)

	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()
	activeCh := coord.Watch(watchCtx)

	for i := 1; i <= 12; i++ {
		key := store.BookKey{Provider: "audible", Namespace: "us", ID: fmt.Sprintf("B0000000%02d", i)}
		_, err := coord.Enqueue(ctx, key)
		must(t, err)
	}
	waitForSnapshot(t, activeCh, func(s Snapshot) bool {
		return s.Active == nil && len(s.Waiting) == 0 && len(s.History) == 12
	})

	// Close must flush/drop stalled watcher cleanly without blocking.
	must(t, coord.Close(ctx))
}
