package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/downloads"
	"github.com/susamn/taletty/app/internal/playback"
	"github.com/susamn/taletty/app/internal/profile"
	"github.com/susamn/taletty/app/internal/store"
	"github.com/susamn/taletty/app/internal/testkit"
	"github.com/susamn/taletty/app/internal/ui"
	"github.com/susamn/taletty/engine/connectors/audible"
	"github.com/susamn/taletty/engine/model"
)

type scriptedDownloader struct {
	mu      sync.Mutex
	handler func(ctx context.Context, opID string, key store.BookKey, report func(downloads.DownloadEvent)) (downloads.DownloadOutcome, error)
}

func (d *scriptedDownloader) Download(ctx context.Context, opID string, key store.BookKey, report func(downloads.DownloadEvent)) (downloads.DownloadOutcome, error) {
	d.mu.Lock()
	fn := d.handler
	d.mu.Unlock()
	return fn(ctx, opID, key, report)
}

func TestJ04J05J06DownloadManagerProgressCancelRetryReorder(t *testing.T) {
	home := t.TempDir()
	ws, err := profile.Open(context.Background(), home, 100)
	if err != nil {
		t.Fatal(err)
	}
	st := ws.Store()

	keyA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	keyB := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}
	keyC := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000003"}
	_ = st.ReplaceCatalog(context.Background(), "audible", "us", []store.CatalogBook{
		{Key: keyA, Title: "Alpha Book"},
		{Key: keyB, Title: "Beta Book"},
		{Key: keyC, Title: "Gamma Book"},
	}, 100)

	var clockMu sync.Mutex
	now := time.Unix(1700000000, 0)
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		now = now.Add(time.Second)
		return now
	}

	betaStarted := make(chan struct{})
	betaRelease := make(chan struct{})
	var failGammaOnce sync.Once
	dl := &scriptedDownloader{
		handler: func(ctx context.Context, opID string, key store.BookKey, report func(downloads.DownloadEvent)) (downloads.DownloadOutcome, error) {
			switch key {
			case keyA:
				total := int64(100 << 20)
				b1 := int64(40 << 20)
				b2 := int64(100 << 20)
				report(downloads.DownloadEvent{OperationID: opID, Sequence: 1, Kind: downloads.EventStarted})
				report(downloads.DownloadEvent{OperationID: opID, Sequence: 2, Kind: downloads.EventStage, Stage: downloads.StageLicensing})
				report(downloads.DownloadEvent{OperationID: opID, Sequence: 3, Kind: downloads.EventStage, Stage: downloads.StageDownloading})
				report(downloads.DownloadEvent{OperationID: opID, Sequence: 4, Kind: downloads.EventProgress, Bytes: &b1, Total: &total})
				report(downloads.DownloadEvent{OperationID: opID, Sequence: 5, Kind: downloads.EventProgress, Bytes: &b2, Total: &total})
				report(downloads.DownloadEvent{OperationID: opID, Sequence: 6, Kind: downloads.EventStage, Stage: downloads.StagePreparing})
				report(downloads.DownloadEvent{OperationID: opID, Sequence: 7, Kind: downloads.EventStage, Stage: downloads.StagePublishing})
				outPath := filepath.Join(ws.DownloadsPath(), key.ID+".m4b")
				_ = os.WriteFile(outPath, []byte("m4b-a"), 0o600)
				return downloads.DownloadOutcome{Media: &downloads.PublishedMedia{RelativePath: "downloads/" + key.ID + ".m4b", Format: "m4b"}}, nil
			case keyB:
				b1 := int64(5 << 20)
				report(downloads.DownloadEvent{OperationID: opID, Sequence: 1, Kind: downloads.EventStarted})
				report(downloads.DownloadEvent{OperationID: opID, Sequence: 2, Kind: downloads.EventStage, Stage: downloads.StageDownloading})
				report(downloads.DownloadEvent{OperationID: opID, Sequence: 3, Kind: downloads.EventProgress, Bytes: &b1})
				select {
				case <-betaStarted:
				default:
					close(betaStarted)
				}
				select {
				case <-betaRelease:
					outPath := filepath.Join(ws.DownloadsPath(), key.ID+".m4b")
					_ = os.WriteFile(outPath, []byte("m4b-b"), 0o600)
					return downloads.DownloadOutcome{Media: &downloads.PublishedMedia{RelativePath: "downloads/" + key.ID + ".m4b", Format: "m4b"}}, nil
				case <-ctx.Done():
					return downloads.DownloadOutcome{}, ctx.Err()
				}
			default:
				var fail bool
				failGammaOnce.Do(func() { fail = true })
				if fail {
					return downloads.DownloadOutcome{}, downloads.Failure{Code: "disk_full"}
				}
				outPath := filepath.Join(ws.DownloadsPath(), key.ID+".m4b")
				_ = os.WriteFile(outPath, []byte("m4b-c"), 0o600)
				return downloads.DownloadOutcome{Media: &downloads.PublishedMedia{RelativePath: "downloads/" + key.ID + ".m4b", Format: "m4b"}}, nil
			}
		},
	}

	dlMgr, err := downloads.New(context.Background(), downloads.Options{
		Store:      st,
		Downloader: dl,
		RootOwner:  ws.Owner(),
		Clock:      clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	coord, err := NewCoordinator(context.Background(), st, Options{
		DownloadsMgr:    dlMgr,
		RootOwner:       ws.Owner(),
		Clock:           clock,
		WorkspaceCloser: ws.Close,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = coord.Close(context.Background()) }()

	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	watchCh := coord.WatchDownloads(watchCtx)

	r, err := testkit.New(context.Background(), ui.NewModel(coord.InitialSnapshot(), coord.Dispatch), 300)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range coord.InitialMessages() {
		if err := r.Send(msg); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Send(tea.WindowSizeMsg{Width: 144, Height: 42}); err != nil {
		t.Fatal(err)
	}

	// J04: Press 'd' on Alpha Book -> completes Ready.
	sendUIKey(t, r, "d")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		snap := dlMgr.Snapshot()
		if snap.Active == nil && len(snap.History) == 1 && snap.History[0].State == store.JobReady {
			break
		}
		select {
		case <-watchCh:
		case <-deadline:
			t.Fatalf("timed out waiting for Alpha download Ready: %+v", dlMgr.Snapshot())
		}
	}
	// Duplicate 'd' on Ready Alpha returns already downloaded notice.
	sendUIKey(t, r, "d")
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Model.View().Content, "already downloaded") {
		t.Fatalf("expected already downloaded notice:\n%s", r.Model.View().Content)
	}

	// J05/J06: Enqueue Beta (blocks with unknown total) and Gamma (fails disk_full once).
	sendUIKey(t, r, "j")
	sendUIKey(t, r, "d") // Beta
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}
	<-betaStarted
	sendUIKey(t, r, "j")
	sendUIKey(t, r, "d") // Gamma queued behind Beta
	if err := r.Drain(); err != nil {
		t.Fatal(err)
	}

	// Open Downloads view, feed live snapshot, verify unknown total and queued Gamma.
	if err := r.Send(coord.DownloadsSnapshot()); err != nil {
		t.Fatal(err)
	}
	sendUIKey(t, r, "D")
	view := r.Model.View().Content
	if !strings.Contains(view, "unknown total") || !strings.Contains(view, "Gamma Book") {
		t.Fatalf("expected Beta unknown total and queued Gamma in Downloads view:\n%s", view)
	}

	// MoveWaiting / RemoveWaiting / CancelActive / Retry via Coordinator actions.
	encB, _ := EncodeBookKey(keyB)
	encC, _ := EncodeBookKey(keyC)
	gammaID := dlMgr.Snapshot().Waiting[0].ID
	_ = coord.Dispatch(ui.Action{Kind: ui.DownloadMoveUp, JobID: gammaID, Key: encC})()
	_ = coord.Dispatch(ui.Action{Kind: ui.DownloadMoveDown, JobID: gammaID, Key: encC})()
	_ = coord.Dispatch(ui.Action{Kind: ui.DownloadRemove, JobID: gammaID, Key: encC})()

	// Cancel active Beta via confirmation action.
	betaID := dlMgr.Snapshot().Active.ID
	_ = coord.Dispatch(ui.Action{Kind: ui.DownloadCancel, JobID: betaID, Key: encB, RequestID: "c1", Attempt: 1})()
	for dlMgr.Snapshot().Active != nil {
		select {
		case <-watchCh:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for Beta cancel")
		}
	}

	// Enqueue Gamma -> fails disk_full -> explicit Retry succeeds.
	_ = coord.Dispatch(ui.Action{Kind: ui.QueueDownload, Key: encC})()
	var failedGammaID string
	for failedGammaID == "" {
		snap := dlMgr.Snapshot()
		for _, h := range snap.History {
			if h.Key == keyC && h.State == store.JobFailed {
				failedGammaID = h.ID
				break
			}
		}
		if failedGammaID != "" {
			break
		}
		select {
		case <-watchCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for Gamma failure: %+v", dlMgr.Snapshot())
		}
	}
	_ = coord.Dispatch(ui.Action{Kind: ui.DownloadRetry, JobID: failedGammaID, Key: encC})()
	for {
		snap := dlMgr.Snapshot()
		ready := false
		for _, h := range snap.History {
			if h.Key == keyC && h.State == store.JobReady {
				ready = true
				break
			}
		}
		if snap.Active == nil && ready {
			break
		}
		select {
		case <-watchCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for Gamma retry Ready: %+v", dlMgr.Snapshot())
		}
	}
	close(betaRelease)
}

func TestJ12SafeAudioDeletionPreservesMetadataAndStopsPlayback(t *testing.T) {
	home := t.TempDir()
	ws, err := profile.Open(context.Background(), home, 100)
	if err != nil {
		t.Fatal(err)
	}
	st := ws.Store()

	keyA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	_ = st.ReplaceCatalog(context.Background(), "audible", "us", []store.CatalogBook{{Key: keyA, Title: "Alpha"}}, 100)
	pathA := filepath.Join(ws.DownloadsPath(), "B000000001.m4b")
	_ = os.WriteFile(pathA, []byte("audio-bytes"), 0o600)
	_ = st.PutLocalMedia(context.Background(), store.LocalMedia{Key: keyA, RelativePath: "downloads/B000000001.m4b", Format: "m4b", PublishedAtMS: 100})
	_ = st.PutListeningState(context.Background(), store.ListeningState{Key: keyA, PositionNS: int64(25 * time.Second), SpeedMilli: 1_250_000, Completion: store.CompletionUnfinished, UpdatedAtMS: 100})
	_ = st.SetFavorite(context.Background(), keyA, true, 100)
	_ = st.PutBookmark(context.Background(), store.Bookmark{ID: "bm-1", Key: keyA, PositionNS: int64(20 * time.Second), Note: "Keep this note", CreatedAtMS: 100, UpdatedAtMS: 100})

	backend := &fakePlaybackBackend{}
	ctrl, _ := playback.NewController(backend, playback.Options{Store: st})
	coord, err := NewCoordinator(context.Background(), st, Options{
		Player:          ctrl,
		MediaChecker:    NewRootMediaChecker(ws.Owner()),
		RootOwner:       ws.Owner(),
		WorkspaceCloser: ws.Close,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = coord.Close(context.Background()) }()

	encA, _ := EncodeBookKey(keyA)
	_ = coord.Dispatch(ui.Action{Kind: ui.PlayBook, Key: encA})()
	if ctrl.Snapshot().State != playback.StatePlaying {
		t.Fatalf("expected playing state, got %+v", ctrl.Snapshot())
	}

	// Stop failure must abort deletion and preserve audio file + LocalMedia.
	backend.stopErr = playback.ErrIPC
	res := coord.Dispatch(ui.Action{Kind: ui.DownloadDeleteAudio, Key: encA, RequestID: "del-1", Attempt: 1})()
	if mr, ok := res.(ui.ModalResultMsg); !ok || !strings.Contains(mr.Error, "Could not stop playback") {
		t.Fatalf("expected stop failure error, got %#v", res)
	}
	if _, err := os.Stat(pathA); err != nil {
		t.Fatalf("audio file deleted despite Stop failure: %v", err)
	}
	backend.stopErr = nil

	// Successful deletion stops playback, unlinks file, removes LocalMedia, and preserves metadata/notes/favorites/history.
	res = coord.Dispatch(ui.Action{Kind: ui.DownloadDeleteAudio, Key: encA, RequestID: "del-2", Attempt: 1})()
	if _, ok := res.(tea.BatchMsg); !ok {
		t.Fatalf("expected BatchMsg on deleteAudio success, got %T", res)
	}
	if _, err := os.Stat(pathA); !os.IsNotExist(err) {
		t.Fatalf("expected audio file unlinked, got err=%v", err)
	}
	if _, found, err := st.LocalMedia(context.Background(), keyA); err != nil || found {
		t.Fatalf("expected LocalMedia deleted, got found=%v err=%v", found, err)
	}
	if ls, found, err := st.ListeningState(context.Background(), keyA); err != nil || !found || ls.PositionNS != int64(25*time.Second) {
		t.Fatalf("ListeningState lost on audio deletion: %+v found=%v err=%v", ls, found, err)
	}
	if favs, err := st.Favorites(context.Background()); err != nil || len(favs) != 1 || favs[0] != keyA {
		t.Fatalf("Favorites lost on audio deletion: %+v err=%v", favs, err)
	}
	if bms, err := st.Bookmarks(context.Background(), keyA); err != nil || len(bms) != 1 || bms[0].Note != "Keep this note" {
		t.Fatalf("Bookmarks lost on audio deletion: %+v err=%v", bms, err)
	}

	// Adversarial unlinkPrivateMedia checks: 0644 file refused, ENOENT succeeds cleanly.
	unsafePath := filepath.Join(ws.DownloadsPath(), "unsafe.m4b")
	_ = os.WriteFile(unsafePath, []byte("x"), 0o644)
	if err := coord.unlinkPrivateMedia(context.Background(), store.LocalMedia{Key: keyA, RelativePath: "downloads/unsafe.m4b", Format: "m4b"}); err == nil {
		t.Fatal("expected refusal for 0644 file")
	}
	if err := coord.unlinkPrivateMedia(context.Background(), store.LocalMedia{Key: keyA, RelativePath: "downloads/absent.m4b", Format: "m4b"}); err != nil {
		t.Fatalf("expected nil for absent file, got %v", err)
	}
}

type fakeBorrowedDownloader struct {
	result model.DownloadResult
	err    error
	close  error
}

func (f *fakeBorrowedDownloader) Download(_ context.Context, opID string, book model.BookID, report func(model.OperationEvent)) (model.DownloadResult, error) {
	if report != nil {
		b := int64(1024)
		tot := int64(2048)
		report(model.OperationEvent{OperationID: opID, Sequence: 1, Type: model.Started})
		report(model.OperationEvent{OperationID: opID, Sequence: 2, Type: model.Stage, Stage: "downloading"})
		report(model.OperationEvent{OperationID: opID, Sequence: 3, Type: model.Progress, Bytes: &b, Total: &tot})
	}
	return f.result, f.err
}
func (f *fakeBorrowedDownloader) Close(context.Context) error { return f.close }

func TestEngineDownloaderAndSessionDownloadWatch(t *testing.T) {
	home := t.TempDir()
	ws, err := profile.Open(context.Background(), home, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close(context.Background()) }()

	key := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	bookID := model.BookID{Provider: "audible", Namespace: "us", ID: "B000000001"}

	ed := NewEngineDownloader(ws.Owner(), "/fake/python")
	ed.discoverPython = func(s string) (string, error) { return s, nil }
	ed.openBorrowed = func(context.Context, *os.File, string) (borrowedDownloaderClient, error) {
		return &fakeBorrowedDownloader{
			result: model.DownloadResult{
				OperationID: "op-1",
				BookID:      bookID,
				State:       model.MediaReady,
				Media:       &model.Media{BookID: bookID, Path: "/tmp/downloads/B000000001.m4b", Format: "m4b"},
			},
		}, nil
	}
	var events []downloads.DownloadEvent
	out, err := ed.Download(context.Background(), "op-1", key, func(ev downloads.DownloadEvent) {
		events = append(events, ev)
	})
	if err != nil || out.Media == nil || out.Media.RelativePath != "downloads/B000000001.m4b" || len(events) != 3 {
		t.Fatalf("EngineDownloader.Download failed: out=%+v events=%+v err=%v", out, events, err)
	}

	// Post-commit auth failure preserves PublishedMedia alongside Failure.
	ed.openBorrowed = func(context.Context, *os.File, string) (borrowedDownloaderClient, error) {
		return &fakeBorrowedDownloader{
			result: model.DownloadResult{
				OperationID: "op-2",
				BookID:      bookID,
				State:       model.MediaReady,
				Media:       &model.Media{BookID: bookID, Path: "/tmp/downloads/B000000001.m4b", Format: "m4b"},
			},
			err: &model.Failure{Code: "storage_uncertain", Retry: "none"},
		}, nil
	}
	out, err = ed.Download(context.Background(), "op-2", key, nil)
	if out.Media == nil || err == nil {
		t.Fatalf("expected both Media and Failure on post-commit auth error: out=%+v err=%v", out, err)
	}

	// Error mapping and nil/invalid guards.
	for _, e := range []error{
		nil,
		context.Canceled,
		context.DeadlineExceeded,
		audible.ErrRuntimeMissing,
		audible.ErrRuntimeIncompatible,
		audible.ErrAuthBusy,
		audible.ErrAuthStorage,
		&model.Failure{Code: "throttled", Retry: "wait"},
		errors.New("unknown"),
	} {
		_ = mapDownloadEngineError(e)
	}
	for _, e := range []error{
		nil,
		downloads.ErrAlreadyQueued,
		downloads.ErrAlreadyDownloaded,
		downloads.ErrNoActiveJob,
		downloads.ErrJobNotFound,
		downloads.ErrJobNotWaiting,
		downloads.ErrJobNotRetryable,
		downloads.ErrClosed,
		errors.New("other"),
	} {
		_ = sanitizeDownloadError(e)
	}
	if _, err := (*EngineDownloader)(nil).Download(context.Background(), "op", key, nil); err == nil {
		t.Fatal("expected error for nil EngineDownloader")
	}
	if _, err := ed.Download(context.Background(), "op", store.BookKey{Provider: "local", Namespace: "user", ID: "x"}, nil); err == nil {
		t.Fatal("expected error for non-audible key")
	}

	// sessionModel downloadWatchMsg handling.
	dlMgr, err := downloads.New(context.Background(), downloads.Options{
		Store: ws.Store(),
		Downloader: &scriptedDownloader{handler: func(context.Context, string, store.BookKey, func(downloads.DownloadEvent)) (downloads.DownloadOutcome, error) {
			return downloads.DownloadOutcome{}, nil
		}},
		RootOwner: ws.Owner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	coord, err := NewCoordinator(context.Background(), ws.Store(), Options{
		DownloadsMgr: dlMgr,
		RootOwner:    ws.Owner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = coord.Close(context.Background()) }()
	sm := sessionModel{inner: ui.NewModel(coord.Snapshot(), coord.Dispatch), ctx: context.Background(), coord: coord}
	_ = sm.Init()
	ch := make(chan downloads.Snapshot, 1)
	ch <- downloads.Snapshot{History: []downloads.Job{{ID: "j1", Key: key, State: store.JobReady}}}
	close(ch)
	msg := waitDownloadCmd(ch)()
	_, _ = sm.Update(msg)
}

func TestDownloadManagerErrorBranchesAndGuards(t *testing.T) {
	home := t.TempDir()
	ws, err := profile.Open(context.Background(), home, 100)
	if err != nil {
		t.Fatal(err)
	}
	st := ws.Store()
	key := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	encKey, _ := EncodeBookKey(key)

	// EngineDownloader error branches: missing python, canceled lease, openBorrowed error, close error.
	ed := NewEngineDownloader(ws.Owner(), "")
	ed.discoverPython = func(string) (string, error) { return "", audible.ErrRuntimeMissing }
	if _, err := ed.Download(context.Background(), "op", key, nil); err == nil {
		t.Fatal("expected runtime_missing error")
	}
	ed.discoverPython = func(string) (string, error) { return "/fake/python", nil }
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ed.Download(canceled, "op", key, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	ed.openBorrowed = func(context.Context, *os.File, string) (borrowedDownloaderClient, error) {
		return nil, audible.ErrAuthStorage
	}
	if _, err := ed.Download(context.Background(), "op", key, nil); err == nil {
		t.Fatal("expected openBorrowed error")
	}
	ed.openBorrowed = func(context.Context, *os.File, string) (borrowedDownloaderClient, error) {
		return &fakeBorrowedDownloader{close: errors.New("close fail")}, nil
	}
	if _, err := ed.Download(context.Background(), "op", key, nil); err == nil {
		t.Fatal("expected close error")
	}

	// Coordinator without downloadsMgr / rootOwner handles all download actions safely.
	noMgr, err := NewCoordinator(context.Background(), st, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(noMgr.DownloadsSnapshot().Jobs) != 0 || noMgr.WatchDownloads(context.Background()) != nil {
		t.Fatal("expected empty snapshot and nil watch without downloadsMgr")
	}
	for _, action := range []ui.Action{
		{Kind: ui.DownloadCancel, RequestID: "r1", Attempt: 1},
		{Kind: ui.DownloadRetry, JobID: "j1"},
		{Kind: ui.DownloadRemove, JobID: "j1"},
		{Kind: ui.DownloadMoveUp, JobID: "j1"},
		{Kind: ui.DownloadMoveDown, JobID: "j1"},
		{Kind: ui.DownloadDeleteAudio, Key: "bad", RequestID: "r2", Attempt: 1},
	} {
		_ = noMgr.Dispatch(action)()
	}
	_ = noMgr.handleDownloadAction(ui.Action{Kind: ui.ActionKind("unknown")})
	_ = noMgr.Close(context.Background())

	// Coordinator with downloadsMgr error paths (no active job, missing job ID) and closed guards.
	dlMgr, err := downloads.New(context.Background(), downloads.Options{
		Store: st,
		Downloader: &scriptedDownloader{handler: func(context.Context, string, store.BookKey, func(downloads.DownloadEvent)) (downloads.DownloadOutcome, error) {
			return downloads.DownloadOutcome{}, nil
		}},
		RootOwner: ws.Owner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fs := &downloadFaultStore{CatalogStore: st}
	coord, err := NewCoordinator(context.Background(), fs, Options{
		DownloadsMgr:    dlMgr,
		RootOwner:       ws.Owner(),
		WorkspaceCloser: ws.Close,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []ui.Action{
		{Kind: ui.DownloadCancel, RequestID: "r1", Attempt: 1},
		{Kind: ui.DownloadRetry, JobID: "missing"},
		{Kind: ui.DownloadRemove, JobID: "missing"},
		{Kind: ui.DownloadMoveUp, JobID: "missing"},
		{Kind: ui.DownloadMoveDown, JobID: "missing"},
	} {
		_ = coord.Dispatch(action)()
	}

	// unlinkPrivateMedia adversarial checks: invalid media, hardlink, symlink, 0755 subdir, DeleteLocalMedia fault.
	if err := coord.unlinkPrivateMedia(context.Background(), store.LocalMedia{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for invalid media, got %v", err)
	}
	hardA := filepath.Join(ws.DownloadsPath(), "h1.m4b")
	hardB := filepath.Join(ws.DownloadsPath(), "h2.m4b")
	_ = os.WriteFile(hardA, []byte("x"), 0o600)
	_ = os.Link(hardA, hardB)
	if err := coord.unlinkPrivateMedia(context.Background(), store.LocalMedia{Key: key, RelativePath: "downloads/h2.m4b", Format: "m4b"}); err == nil {
		t.Fatal("expected error for hardlinked file")
	}
	sym := filepath.Join(ws.DownloadsPath(), "sym.m4b")
	_ = os.Symlink(hardA, sym)
	if err := coord.unlinkPrivateMedia(context.Background(), store.LocalMedia{Key: key, RelativePath: "downloads/sym.m4b", Format: "m4b"}); err == nil {
		t.Fatal("expected error for symlinked file")
	}
	sub := filepath.Join(ws.DownloadsPath(), "badsub")
	_ = os.Mkdir(sub, 0o755)
	_ = os.WriteFile(filepath.Join(sub, "x.m4b"), []byte("x"), 0o600)
	if err := coord.unlinkPrivateMedia(context.Background(), store.LocalMedia{Key: key, RelativePath: "downloads/badsub/x.m4b", Format: "m4b"}); err == nil {
		t.Fatal("expected error for 0755 subdir")
	}

	_ = st.PutLocalMedia(context.Background(), store.LocalMedia{Key: key, RelativePath: "downloads/h1.m4b", Format: "m4b", PublishedAtMS: 100})
	fs.failDelete = true
	if mr, ok := coord.Dispatch(ui.Action{Kind: ui.DownloadDeleteAudio, Key: encKey, RequestID: "d1", Attempt: 1})().(ui.ModalResultMsg); !ok || mr.Error == "" {
		t.Fatalf("expected DeleteLocalMedia error, got %#v", mr)
	}
	fs.failDelete = false

	// Closed coordinator rejects all download actions cleanly.
	if err := coord.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, action := range []ui.Action{
		{Kind: ui.QueueDownload, Key: encKey, RequestID: "q1", Attempt: 1},
		{Kind: ui.DownloadCancel, RequestID: "c1", Attempt: 1},
		{Kind: ui.DownloadRetry, JobID: "j1"},
		{Kind: ui.DownloadRemove, JobID: "j1"},
		{Kind: ui.DownloadMoveUp, JobID: "j1"},
		{Kind: ui.DownloadMoveDown, JobID: "j1"},
		{Kind: ui.DownloadDeleteAudio, Key: encKey, RequestID: "d2", Attempt: 1},
	} {
		_ = coord.Dispatch(action)()
	}
}

type downloadFaultStore struct {
	CatalogStore
	failDelete bool
}

func (f *downloadFaultStore) DeleteLocalMedia(ctx context.Context, key store.BookKey) error {
	if f.failDelete {
		return errors.New("delete local media failed")
	}
	return f.CatalogStore.DeleteLocalMedia(ctx, key)
}
