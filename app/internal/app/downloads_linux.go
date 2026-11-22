package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/downloads"
	"github.com/susamn/taletty/app/internal/playback"
	"github.com/susamn/taletty/app/internal/store"
	"github.com/susamn/taletty/app/internal/ui"
	"github.com/susamn/taletty/engine/connectors/audible"
	"github.com/susamn/taletty/engine/model"
	"golang.org/x/sys/unix"
)

// DownloadManager abstracts the persistent sequential download-job coordinator.
// *downloads.Coordinator satisfies this interface directly.
type DownloadManager interface {
	Snapshot() downloads.Snapshot
	Watch(ctx context.Context) <-chan downloads.Snapshot
	Enqueue(ctx context.Context, key store.BookKey) (downloads.Job, error)
	CancelActive(ctx context.Context) error
	Retry(ctx context.Context, jobID string) (downloads.Job, error)
	RemoveWaiting(ctx context.Context, jobID string) error
	MoveWaiting(ctx context.Context, jobID string, delta int) error
	Close(ctx context.Context) error
}

var _ DownloadManager = (*downloads.Coordinator)(nil)

type borrowedDownloaderClient interface {
	Download(ctx context.Context, operationID string, book model.BookID, report func(model.OperationEvent)) (model.DownloadResult, error)
	Close(ctx context.Context) error
}

// EngineDownloader adapts the pinned engine Client.Download to downloads.Downloader
// using lazy Python discovery and borrowed session-lifetime root leases.
type EngineDownloader struct {
	owner          RootLeaseOwner
	pythonExplicit string
	discoverPython func(string) (string, error)
	openBorrowed   func(context.Context, *os.File, string) (borrowedDownloaderClient, error)
}

// NewEngineDownloader constructs a downloads.Downloader backed by the pinned engine.
func NewEngineDownloader(owner RootLeaseOwner, pythonExplicit string) *EngineDownloader {
	return &EngineDownloader{
		owner:          owner,
		pythonExplicit: pythonExplicit,
		discoverPython: audible.DiscoverPython,
		openBorrowed: func(ctx context.Context, root *os.File, python string) (borrowedDownloaderClient, error) {
			return audible.OpenBorrowed(ctx, root, python)
		},
	}
}

// Download executes one Audible download operation under a borrowed root lease.
func (d *EngineDownloader) Download(ctx context.Context, opID string, key store.BookKey, report func(downloads.DownloadEvent)) (downloads.DownloadOutcome, error) {
	if d == nil || d.owner == nil || ctx == nil || opID == "" {
		return downloads.DownloadOutcome{}, downloads.Failure{Code: "invalid_input"}
	}
	if err := key.Validate(); err != nil || key.Provider != "audible" || key.Namespace != "us" {
		return downloads.DownloadOutcome{}, downloads.Failure{Code: "invalid_input"}
	}
	python, err := d.discoverPython(d.pythonExplicit)
	if err != nil {
		return downloads.DownloadOutcome{}, mapDownloadEngineError(err)
	}
	lease, err := d.owner.Lease(ctx)
	if err != nil {
		return downloads.DownloadOutcome{}, mapDownloadEngineError(err)
	}
	defer lease.Close()

	client, err := d.openBorrowed(lease.Context, lease.Root, python)
	if err != nil {
		return downloads.DownloadOutcome{}, mapDownloadEngineError(err)
	}

	var cb func(model.OperationEvent)
	if report != nil {
		cb = func(ev model.OperationEvent) {
			report(mapDownloadEvent(ev))
		}
	}
	bookID := model.BookID{Provider: key.Provider, Namespace: key.Namespace, ID: key.ID}
	result, dlErr := client.Download(lease.Context, opID, bookID, cb)
	closeErr := client.Close(context.Background())

	var outcome downloads.DownloadOutcome
	if result.State == model.MediaReady && result.Media != nil {
		outcome.Media = &downloads.PublishedMedia{
			RelativePath: "downloads/" + key.ID + ".m4b",
			Format:       "m4b",
		}
	}
	if dlErr != nil {
		return outcome, mapDownloadEngineError(dlErr)
	}
	if closeErr != nil {
		return outcome, mapDownloadEngineError(closeErr)
	}
	return outcome, nil
}

func mapDownloadEvent(ev model.OperationEvent) downloads.DownloadEvent {
	out := downloads.DownloadEvent{
		OperationID: ev.OperationID,
		Sequence:    ev.Sequence,
		Kind:        downloads.EventKind(ev.Type),
		Stage:       downloads.Stage(ev.Stage),
	}
	if ev.Bytes != nil {
		v := *ev.Bytes
		out.Bytes = &v
	}
	if ev.Total != nil {
		v := *ev.Total
		out.Total = &v
	}
	return out
}

func mapDownloadEngineError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, audible.ErrRuntimeMissing) {
		return downloads.Failure{Code: "runtime_missing"}
	}
	if errors.Is(err, audible.ErrRuntimeIncompatible) {
		return downloads.Failure{Code: "runtime_incompatible"}
	}
	if errors.Is(err, audible.ErrAuthBusy) {
		return downloads.Failure{Code: "busy", Retry: "user_retry"}
	}
	if errors.Is(err, audible.ErrAuthStorage) {
		return downloads.Failure{Code: "storage_failed"}
	}
	var failure *model.Failure
	if errors.As(err, &failure) {
		return downloads.Failure{Code: failure.Code, Retry: failure.Retry}
	}
	return downloads.Failure{Code: "internal"}
}

func (c *Coordinator) handleDownloadAction(action ui.Action) tea.Msg {
	switch action.Kind {
	case ui.QueueDownload:
		return c.queueDownload(action)
	case ui.DownloadCancel:
		return c.cancelDownload(action)
	case ui.DownloadRetry:
		return c.retryDownload(action)
	case ui.DownloadRemove:
		return c.removeDownload(action)
	case ui.DownloadMoveUp:
		return c.moveDownload(action, -1)
	case ui.DownloadMoveDown:
		return c.moveDownload(action, 1)
	case ui.DownloadDeleteAudio:
		return c.deleteAudio(action)
	default:
		return ui.NoticeMsg{Activity: "Action unavailable in this slice"}
	}
}

func (c *Coordinator) queueDownload(action ui.Action) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return actionResultOrNotice(action, SanitizeError(ErrClosed), "")
	}
	defer c.wg.Done()

	key, err := DecodeBookKey(action.Key)
	if err != nil {
		return actionResultOrNotice(action, SanitizeError(ErrInvalidInput), "")
	}
	if c.downloadsMgr != nil {
		if _, err := c.downloadsMgr.Enqueue(ctx, key); err != nil {
			return actionResultOrNotice(action, sanitizeDownloadError(err), "")
		}
		dlMsg := c.ProjectDownloads(c.downloadsMgr.Snapshot())
		snapMsg := c.RefreshSnapshotFromDownloads(ctx, c.downloadsMgr.Snapshot())
		if action.RequestID != "" {
			return tea.BatchMsg{
				func() tea.Msg { return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt} },
				func() tea.Msg { return dlMsg },
				func() tea.Msg { return snapMsg },
			}
		}
		return tea.BatchMsg{
			func() tea.Msg { return dlMsg },
			func() tea.Msg { return snapMsg },
			func() tea.Msg { return ui.NoticeMsg{Activity: "Queued download"} },
		}
	}
	if c.downloads == nil {
		return actionResultOrNotice(action, "Download manager unavailable in this slice; use taletty-engine download.", "")
	}
	if err := c.downloads.Enqueue(ctx, key); err != nil {
		return actionResultOrNotice(action, SanitizeError(err), "")
	}
	return actionResultOrNotice(action, "", "Queued download")
}

func (c *Coordinator) cancelDownload(action ui.Action) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	if c.downloadsMgr == nil {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: "Download manager unavailable."}
	}
	if err := c.downloadsMgr.CancelActive(ctx); err != nil {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: sanitizeDownloadError(err)}
	}
	dlMsg := c.ProjectDownloads(c.downloadsMgr.Snapshot())
	return tea.BatchMsg{
		func() tea.Msg { return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt} },
		func() tea.Msg { return dlMsg },
		func() tea.Msg { return ui.NoticeMsg{Activity: "Canceling active download..."} },
	}
}

func (c *Coordinator) retryDownload(action ui.Action) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.NoticeMsg{Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	if c.downloadsMgr == nil {
		return ui.NoticeMsg{Error: "Download manager unavailable."}
	}
	if _, err := c.downloadsMgr.Retry(ctx, action.JobID); err != nil {
		return ui.NoticeMsg{Error: sanitizeDownloadError(err)}
	}
	dlMsg := c.ProjectDownloads(c.downloadsMgr.Snapshot())
	snapMsg := c.RefreshSnapshotFromDownloads(ctx, c.downloadsMgr.Snapshot())
	return tea.BatchMsg{
		func() tea.Msg { return dlMsg },
		func() tea.Msg { return snapMsg },
		func() tea.Msg { return ui.NoticeMsg{Activity: "Retrying download"} },
	}
}

func (c *Coordinator) removeDownload(action ui.Action) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.NoticeMsg{Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	if c.downloadsMgr == nil {
		return ui.NoticeMsg{Error: "Download manager unavailable."}
	}
	if err := c.downloadsMgr.RemoveWaiting(ctx, action.JobID); err != nil {
		return ui.NoticeMsg{Error: sanitizeDownloadError(err)}
	}
	dlMsg := c.ProjectDownloads(c.downloadsMgr.Snapshot())
	snapMsg := c.RefreshSnapshotFromDownloads(ctx, c.downloadsMgr.Snapshot())
	return tea.BatchMsg{
		func() tea.Msg { return dlMsg },
		func() tea.Msg { return snapMsg },
		func() tea.Msg { return ui.NoticeMsg{Activity: "Removed waiting download"} },
	}
}

func (c *Coordinator) moveDownload(action ui.Action, delta int) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.NoticeMsg{Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	if c.downloadsMgr == nil {
		return ui.NoticeMsg{Error: "Download manager unavailable."}
	}
	if err := c.downloadsMgr.MoveWaiting(ctx, action.JobID, delta); err != nil {
		return ui.NoticeMsg{Error: sanitizeDownloadError(err)}
	}
	dlMsg := c.ProjectDownloads(c.downloadsMgr.Snapshot())
	return tea.BatchMsg{
		func() tea.Msg { return dlMsg },
		func() tea.Msg { return ui.NoticeMsg{Activity: "Updated download queue order"} },
	}
}

func (c *Coordinator) deleteAudio(action ui.Action) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	key, err := DecodeBookKey(action.Key)
	if err != nil {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: SanitizeError(ErrInvalidInput)}
	}

	// Stop and checkpoint active playback first if deleting the currently loaded book.
	var stoppedPlayer bool
	if c.player != nil {
		snap := c.player.Snapshot()
		if snap.Book != nil && *snap.Book == key && (snap.State == playback.StatePlaying || snap.State == playback.StatePaused) {
			if _, stopErr := c.player.Stop(ctx); stopErr != nil {
				return ui.ModalResultMsg{
					ID:      action.RequestID,
					Attempt: action.Attempt,
					Error:   "Could not stop playback; audio preserved.",
				}
			}
			stoppedPlayer = true
		}
	}

	media, found, err := c.store.LocalMedia(ctx, key)
	if err != nil {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: SanitizeError(err)}
	}
	if found {
		if err := c.unlinkPrivateMedia(ctx, media); err != nil {
			return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: "Could not delete local audio file safely."}
		}
		if err := c.store.DeleteLocalMedia(ctx, key); err != nil {
			return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: SanitizeError(err)}
		}
	}

	c.mu.Lock()
	delete(c.chapters, key)
	if stoppedPlayer {
		c.snapshot.Player = ui.Player{}
	}
	c.mu.Unlock()

	var dlSnap downloads.Snapshot
	if c.downloadsMgr != nil {
		dlSnap = c.downloadsMgr.Snapshot()
	}
	snapMsg := c.RefreshSnapshotFromDownloads(ctx, dlSnap)
	snapMsg.Activity = "Deleted downloaded audio; notes and history preserved"
	dlMsg := c.ProjectDownloads(dlSnap)
	return tea.BatchMsg{
		func() tea.Msg { return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt} },
		func() tea.Msg { return snapMsg },
		func() tea.Msg { return dlMsg },
	}
}

func (c *Coordinator) unlinkPrivateMedia(ctx context.Context, media store.LocalMedia) error {
	if err := media.Validate(); err != nil {
		return ErrInvalidInput
	}
	if c.rootOwner == nil {
		return ErrInvalidInput
	}
	lease, err := c.rootOwner.Lease(ctx)
	if err != nil {
		return err
	}
	if lease == nil || lease.Root == nil {
		return ErrInvalidInput
	}
	defer lease.Close()
	if err := lease.Context.Err(); err != nil {
		return err
	}

	euid := uint32(os.Geteuid())
	var rootStat unix.Stat_t
	if err := unix.Fstat(int(lease.Root.Fd()), &rootStat); err != nil {
		return err
	}
	if rootStat.Mode&(unix.S_IFMT|07777) != unix.S_IFDIR|0700 || rootStat.Uid != euid {
		return ErrInvalidInput
	}

	parts := strings.Split(media.RelativePath, "/")
	dirFD := int(lease.Root.Fd())
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
			if errors.Is(err, unix.ENOENT) {
				return nil
			}
			return err
		}
		dirFD = nextFD
		closeDir = true
		var st unix.Stat_t
		if err := unix.Fstat(dirFD, &st); err != nil {
			return err
		}
		if st.Mode&(unix.S_IFMT|07777) != unix.S_IFDIR|0700 || st.Uid != euid {
			return ErrInvalidInput
		}
	}

	leaf := parts[len(parts)-1]
	var st unix.Stat_t
	if err := unix.Fstatat(dirFD, leaf, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if st.Mode&(unix.S_IFMT|07777) != unix.S_IFREG|0600 || st.Uid != euid || st.Nlink != 1 {
		return ErrInvalidInput
	}
	if err := unix.Unlinkat(dirFD, leaf, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	_ = unix.Fsync(dirFD)
	return nil
}

// DownloadsSnapshot returns the current projected DownloadsMsg.
func (c *Coordinator) DownloadsSnapshot() ui.DownloadsMsg {
	if c == nil || c.downloadsMgr == nil {
		return ui.DownloadsMsg{}
	}
	return c.ProjectDownloads(c.downloadsMgr.Snapshot())
}

// WatchDownloads returns the underlying download snapshot stream, or nil if absent.
func (c *Coordinator) WatchDownloads(ctx context.Context) <-chan downloads.Snapshot {
	if c == nil || c.downloadsMgr == nil {
		return nil
	}
	return c.downloadsMgr.Watch(ctx)
}

// ProjectDownloads converts a downloads.Snapshot into a sanitized ui.DownloadsMsg
// with measured transfer speed when actively downloading.
func (c *Coordinator) ProjectDownloads(snap downloads.Snapshot) ui.DownloadsMsg {
	now := c.clock()
	var speedBPS float64
	c.mu.Lock()
	if snap.Active != nil && snap.Active.State == store.JobDownloading {
		if c.speedJobID == snap.Active.ID && snap.Active.BytesReceived > c.speedBytes && !c.speedAt.IsZero() {
			if elapsed := now.Sub(c.speedAt).Seconds(); elapsed > 0 {
				speedBPS = float64(snap.Active.BytesReceived-c.speedBytes) / elapsed
			}
		}
		c.speedJobID = snap.Active.ID
		c.speedBytes = snap.Active.BytesReceived
		c.speedAt = now
	} else {
		c.speedJobID = ""
		c.speedBytes = 0
		c.speedAt = time.Time{}
	}
	c.mu.Unlock()

	var jobs []ui.DownloadJob
	if snap.Active != nil {
		jobs = append(jobs, c.projectDownloadJob(*snap.Active, 0, speedBPS))
	}
	for i, j := range snap.Waiting {
		jobs = append(jobs, c.projectDownloadJob(j, i+1, 0))
	}
	for _, j := range snap.History {
		jobs = append(jobs, c.projectDownloadJob(j, 0, 0))
	}
	return ui.DownloadsMsg{
		Jobs:  jobs,
		Error: snap.LastError,
	}
}

func (c *Coordinator) projectDownloadJob(job downloads.Job, queuePos int, speedBPS float64) ui.DownloadJob {
	encoded, _ := EncodeBookKey(job.Key)
	var total *int64
	if job.BytesTotal != nil {
		v := *job.BytesTotal
		total = &v
	}
	return ui.DownloadJob{
		ID:            job.ID,
		BookKey:       encoded,
		Title:         c.bookTitle(job.Key),
		State:         mapJobAvailability(job.State),
		Stage:         string(job.State),
		Canceling:     job.Canceling,
		QueuePosition: queuePos,
		BytesReceived: job.BytesReceived,
		BytesTotal:    total,
		SpeedBPS:      speedBPS,
		Error:         job.FailureMessage,
	}
}

// RefreshSnapshotFromDownloads updates book availability in the UI snapshot from
// persisted LocalMedia and live/terminal download jobs.
func (c *Coordinator) RefreshSnapshotFromDownloads(ctx context.Context, dlSnap downloads.Snapshot) ui.SnapshotMsg {
	media, err := c.store.LocalMediaList(ctx)
	if err != nil {
		c.mu.Lock()
		out := c.snapshot
		c.mu.Unlock()
		return out
	}
	mediaSet := make(map[string]struct{}, len(media))
	for _, item := range media {
		if enc, encErr := EncodeBookKey(item.Key); encErr == nil {
			mediaSet[enc] = struct{}{}
		}
	}
	jobAvail := make(map[string]ui.Availability)
	if dlSnap.Active != nil {
		if enc, encErr := EncodeBookKey(dlSnap.Active.Key); encErr == nil {
			jobAvail[enc] = mapJobAvailability(dlSnap.Active.State)
		}
	}
	for _, j := range dlSnap.Waiting {
		if enc, encErr := EncodeBookKey(j.Key); encErr == nil {
			if _, exists := jobAvail[enc]; !exists {
				jobAvail[enc] = mapJobAvailability(j.State)
			}
		}
	}
	for _, j := range dlSnap.History {
		if enc, encErr := EncodeBookKey(j.Key); encErr == nil {
			if _, exists := jobAvail[enc]; !exists {
				jobAvail[enc] = mapJobAvailability(j.State)
			}
		}
	}

	c.mu.Lock()
	for i := range c.snapshot.Books {
		key := c.snapshot.Books[i].Key
		if _, ok := mediaSet[key]; ok {
			c.snapshot.Books[i].Availability = ui.Ready
		} else if avail, ok := jobAvail[key]; ok {
			c.snapshot.Books[i].Availability = avail
		} else {
			c.snapshot.Books[i].Availability = ui.NotDownloaded
		}
	}
	out := c.snapshot
	c.mu.Unlock()
	return out
}

func actionResultOrNotice(action ui.Action, errText, activity string) tea.Msg {
	if action.RequestID != "" {
		if errText != "" {
			return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: errText}
		}
		return tea.BatchMsg{
			func() tea.Msg { return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt} },
			func() tea.Msg { return ui.NoticeMsg{Activity: activity} },
		}
	}
	if errText != "" {
		return ui.NoticeMsg{Error: errText}
	}
	return ui.NoticeMsg{Activity: activity}
}

func sanitizeDownloadError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, downloads.ErrAlreadyQueued):
		return "Book is already queued or downloading."
	case errors.Is(err, downloads.ErrAlreadyDownloaded):
		return "Book audio is already downloaded."
	case errors.Is(err, downloads.ErrNoActiveJob):
		return "No active download job to cancel."
	case errors.Is(err, downloads.ErrJobNotFound):
		return "Download job not found."
	case errors.Is(err, downloads.ErrJobNotWaiting):
		return "Download job is no longer waiting."
	case errors.Is(err, downloads.ErrJobNotRetryable):
		return "Download job cannot be retried."
	case errors.Is(err, downloads.ErrClosed):
		return "Download manager is closed."
	default:
		return SanitizeError(err)
	}
}
