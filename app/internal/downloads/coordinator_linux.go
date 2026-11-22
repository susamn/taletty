// Package downloads implements Taletty's persistent sequential download-job
// coordinator without importing the unpublished engine module or terminal UI.
package downloads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/susamn/taletty/app/internal/store"
	"golang.org/x/sys/unix"
)

var (
	// ErrInvalidInput indicates malformed coordinator options, keys, or arguments.
	ErrInvalidInput = errors.New("invalid download input")
	// ErrClosed indicates the download coordinator has been closed or is closing.
	ErrClosed = errors.New("download coordinator closed")
	// ErrAlreadyQueued indicates the book already has a waiting or active job.
	ErrAlreadyQueued = errors.New("book already has a live download job")
	// ErrAlreadyDownloaded indicates verified private local media already exists.
	ErrAlreadyDownloaded = errors.New("book media is already downloaded")
	// ErrNoActiveJob indicates no active download job is currently running.
	ErrNoActiveJob = errors.New("no active download job")
	// ErrJobNotFound indicates the requested job ID does not exist.
	ErrJobNotFound = errors.New("download job not found")
	// ErrJobNotWaiting indicates the target job is not in the waiting queue.
	ErrJobNotWaiting = errors.New("download job is not waiting")
	// ErrJobNotRetryable indicates only terminal failed or canceled jobs can be retried.
	ErrJobNotRetryable = errors.New("download job is not retryable")
	// ErrInvalidState indicates persisted store state violated coordinator invariants.
	ErrInvalidState = errors.New("invalid persisted download state")
)

// JobStore persists download jobs and local media inventory.
// *store.Store satisfies this interface directly.
type JobStore interface {
	DownloadJobs(ctx context.Context) ([]store.DownloadJob, error)
	PutDownloadJob(ctx context.Context, job store.DownloadJob) error
	ReorderDownloadQueue(ctx context.Context, orderedJobIDs []string, updatedAtMS int64) error
	ReconcileActiveJobs(ctx context.Context, updatedAtMS int64, published map[store.BookKey]bool) ([]store.DownloadJob, error)
	LocalMedia(ctx context.Context, key store.BookKey) (store.LocalMedia, bool, error)
	LocalMediaList(ctx context.Context) ([]store.LocalMedia, error)
	PutLocalMedia(ctx context.Context, media store.LocalMedia) error
}

var _ JobStore = (*store.Store)(nil)

// MediaChecker verifies whether a persisted LocalMedia entry currently points
// to a valid private regular file under the Taletty downloads root.
type MediaChecker interface {
	VerifyMedia(ctx context.Context, media store.LocalMedia) (bool, error)
}

// EventKind identifies nonterminal progress/stage notifications from a Downloader.
type EventKind string

const (
	EventStarted   EventKind = "started"
	EventStage     EventKind = "stage"
	EventProgress  EventKind = "progress"
	EventCompleted EventKind = "completed"
	EventFailed    EventKind = "failed"
	EventCanceled  EventKind = "canceled"
)

// Stage identifies an active download execution stage.
type Stage string

const (
	StageLicensing   Stage = "licensing"
	StageDownloading Stage = "downloading"
	StagePreparing   Stage = "preparing"
	StagePublishing  Stage = "publishing"
)

// DownloadEvent reports ordered stage and byte-progress updates for one operation.
type DownloadEvent struct {
	OperationID string
	Sequence    uint64
	Kind        EventKind
	Stage       Stage
	Bytes       *int64
	Total       *int64
}

// PublishedMedia describes validated complete local audio produced by a Downloader.
type PublishedMedia struct {
	RelativePath string
	Format       string
}

// DownloadOutcome is the authoritative result returned by Downloader.Download.
// Media != nil indicates validated committed local media, even if an error
// (such as post-commit auth persistence uncertainty) is also returned.
type DownloadOutcome struct {
	Media *PublishedMedia
}

// Failure describes a sanitized failure code and retry disposition returned by a Downloader.
type Failure struct {
	Code  string
	Retry string
}

func (f Failure) Error() string {
	return safeFailureMessage(store.NormalizeFailureCode(f.Code), f.Code)
}

// Downloader executes a single download operation synchronously until completion,
// failure, or context cancellation.
type Downloader interface {
	Download(ctx context.Context, opID string, key store.BookKey, report func(DownloadEvent)) (DownloadOutcome, error)
}

// Options configures a Coordinator. Store, Downloader, and either MediaChecker
// or RootOwner are required.
type Options struct {
	Store        JobStore
	Downloader   Downloader
	RootOwner    store.RootOwner
	MediaChecker MediaChecker
	Clock        func() time.Time
}

// Job represents a persistent download job in an immutable Coordinator snapshot.
type Job struct {
	ID               string
	Key              store.BookKey
	QueueOrder       *int64
	State            store.JobState
	Canceling        bool
	BytesReceived    int64
	BytesTotal       *int64
	FailureCode      store.FailureCode
	FailureMessage   string
	RetryDisposition string
	CreatedAtMS      int64
	UpdatedAtMS      int64
}

func (j Job) clone() Job {
	if j.QueueOrder != nil {
		v := *j.QueueOrder
		j.QueueOrder = &v
	}
	if j.BytesTotal != nil {
		v := *j.BytesTotal
		j.BytesTotal = &v
	}
	return j
}

// Snapshot is an immutable point-in-time view of active, waiting, and terminal jobs.
type Snapshot struct {
	Active    *Job
	Waiting   []Job
	History   []Job
	Revision  uint64
	LastError string
}

type activeJobState struct {
	job       Job
	opCancel  context.CancelFunc
	canceling bool
	finished  bool
	lastSeq   uint64
	stageRank int
}

type queuedSnapshot struct {
	snap      Snapshot
	important bool
}

type watcher struct {
	ch       chan Snapshot
	sig      chan struct{}
	ctx      context.Context
	closedCh <-chan struct{}

	mu    sync.Mutex
	queue []queuedSnapshot
}

// Coordinator owns persistent sequential download execution, startup
// reconciliation, queue management, cancellation/commit-point resolution, and
// immutable snapshot publication.
type Coordinator struct {
	store      JobStore
	downloader Downloader
	verifier   MediaChecker
	clock      func() time.Time

	opMu    sync.Mutex
	stateMu sync.Mutex

	active    *activeJobState
	waiting   []Job
	history   []Job
	usedIDs   map[string]bool
	nextSeq   uint64
	revision  uint64
	lastError string

	closing      bool
	closed       bool
	stopSignaled bool
	closeErr     error

	wakeCh       chan struct{}
	stopWorkerCh chan struct{}
	workerDone   chan struct{}
	closedCh     chan struct{}
	closeOnce    sync.Once

	watchers map[uint64]*watcher
	watchSeq uint64
	watchWG  sync.WaitGroup
}

// New restores persisted download state, verifies private ready media under
// downloads/, reconciles interrupted active jobs, and starts the sequential worker.
func New(ctx context.Context, opts Options) (*Coordinator, error) {
	if ctx == nil {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Store == nil || opts.Downloader == nil || (opts.MediaChecker == nil && opts.RootOwner == nil) {
		return nil, ErrInvalidInput
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	verifier := opts.MediaChecker
	if verifier == nil {
		verifier = &rootMediaVerifier{
			owner: opts.RootOwner,
			euid:  uint32(os.Geteuid()),
		}
	}

	mediaList, err := opts.Store.LocalMediaList(ctx)
	if err != nil {
		return nil, err
	}
	var published map[store.BookKey]bool
	if len(mediaList) > 0 {
		published = make(map[store.BookKey]bool, len(mediaList))
		for _, item := range mediaList {
			ok, err := verifier.VerifyMedia(ctx, item)
			if err != nil {
				return nil, err
			}
			if ok {
				published[item.Key] = true
			}
		}
	}

	nowMS := max(clock().UnixMilli(), 0)
	if _, err := opts.Store.ReconcileActiveJobs(ctx, nowMS, published); err != nil {
		return nil, err
	}

	persisted, err := opts.Store.DownloadJobs(ctx)
	if err != nil {
		return nil, err
	}

	waiting := make([]Job, 0)
	history := make([]Job, 0)
	usedIDs := make(map[string]bool, len(persisted))
	for _, sj := range persisted {
		usedIDs[sj.ID] = true
		if sj.State.IsActive() {
			return nil, ErrInvalidState
		}
		job := jobFromStore(sj)
		if sj.State == store.JobWaiting {
			waiting = append(waiting, job)
		} else {
			history = append(history, job)
		}
	}
	sortHistory(history)

	c := &Coordinator{
		store:        opts.Store,
		downloader:   opts.Downloader,
		verifier:     verifier,
		clock:        clock,
		waiting:      waiting,
		history:      history,
		usedIDs:      usedIDs,
		revision:     1,
		wakeCh:       make(chan struct{}, 1),
		stopWorkerCh: make(chan struct{}),
		workerDone:   make(chan struct{}),
		closedCh:     make(chan struct{}),
		watchers:     make(map[uint64]*watcher),
	}
	if len(c.waiting) > 0 {
		c.wakeWorkerLocked()
	}
	go c.workerLoop()
	return c, nil
}

// Snapshot returns an isolated deep copy of the current coordinator state.
func (c *Coordinator) Snapshot() Snapshot {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.snapshotLocked()
}

func (c *Coordinator) snapshotLocked() Snapshot {
	snap := Snapshot{
		Waiting:   make([]Job, len(c.waiting)),
		History:   make([]Job, len(c.history)),
		Revision:  c.revision,
		LastError: c.lastError,
	}
	if c.active != nil {
		active := c.active.job.clone()
		active.Canceling = c.active.canceling
		snap.Active = &active
	}
	for i, j := range c.waiting {
		snap.Waiting[i] = j.clone()
	}
	for i, j := range c.history {
		snap.History[i] = j.clone()
	}
	return snap
}

// Watch returns a channel receiving immutable snapshot updates until ctx is
// canceled or the coordinator closes. Stage and terminal transitions are never dropped.
func (c *Coordinator) Watch(ctx context.Context) <-chan Snapshot {
	ch := make(chan Snapshot, 64)
	if ctx == nil || ctx.Err() != nil {
		close(ch)
		return ch
	}
	c.stateMu.Lock()
	if c.closed || c.closing {
		c.stateMu.Unlock()
		close(ch)
		return ch
	}
	c.watchSeq++
	id := c.watchSeq
	w := &watcher{
		ch:       ch,
		sig:      make(chan struct{}, 1),
		ctx:      ctx,
		closedCh: c.closedCh,
	}
	c.watchers[id] = w
	c.watchWG.Add(1)
	c.stateMu.Unlock()

	go c.runWatcher(id, w)
	return ch
}

func (c *Coordinator) runWatcher(id uint64, w *watcher) {
	defer c.watchWG.Done()
	defer close(w.ch)
	defer c.removeWatcher(id)

	for {
		item, ok := w.pop()
		if !ok {
			select {
			case <-w.sig:
				continue
			case <-w.ctx.Done():
				return
			case <-w.closedCh:
				w.drainNonBlocking()
				return
			}
		}
		select {
		case w.ch <- item.snap:
		case <-w.ctx.Done():
			return
		case <-w.closedCh:
			select {
			case w.ch <- item.snap:
				w.drainNonBlocking()
			default:
			}
			return
		}
	}
}

func (c *Coordinator) removeWatcher(id uint64) {
	c.stateMu.Lock()
	delete(c.watchers, id)
	c.stateMu.Unlock()
}

func (w *watcher) enqueue(snap Snapshot, important bool) {
	w.mu.Lock()
	if !important && len(w.queue) > 0 && !w.queue[len(w.queue)-1].important {
		w.queue[len(w.queue)-1] = queuedSnapshot{snap: snap, important: false}
	} else {
		w.queue = append(w.queue, queuedSnapshot{snap: snap, important: important})
	}
	w.mu.Unlock()
	select {
	case w.sig <- struct{}{}:
	default:
	}
}

func (w *watcher) pop() (queuedSnapshot, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.queue) == 0 {
		return queuedSnapshot{}, false
	}
	item := w.queue[0]
	w.queue = w.queue[1:]
	return item, true
}

func (w *watcher) drainNonBlocking() {
	for {
		item, ok := w.pop()
		if !ok {
			return
		}
		select {
		case w.ch <- item.snap:
		default:
			return
		}
	}
}

func (c *Coordinator) notifyLocked(important bool) {
	if len(c.watchers) == 0 {
		return
	}
	snap := c.snapshotLocked()
	for _, w := range c.watchers {
		w.enqueue(snap, important)
	}
}

func (c *Coordinator) wakeWorkerLocked() {
	select {
	case c.wakeCh <- struct{}{}:
	default:
	}
}

// Enqueue validates key, deduplicates against live jobs and verified local media,
// persists a new waiting job, and wakes the sequential worker.
func (c *Coordinator) Enqueue(ctx context.Context, key store.BookKey) (Job, error) {
	if ctx == nil {
		return Job{}, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	if err := key.Validate(); err != nil {
		return Job{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}

	c.opMu.Lock()
	defer c.opMu.Unlock()

	c.stateMu.Lock()
	if c.closed || c.closing {
		c.stateMu.Unlock()
		return Job{}, ErrClosed
	}
	if existing, ok := c.findLiveLocked(key); ok {
		c.stateMu.Unlock()
		return existing, ErrAlreadyQueued
	}
	c.stateMu.Unlock()

	return c.enqueueNewLocked(ctx, key)
}

// Retry creates a new queued attempt for a terminal JobFailed or JobCanceled job.
// It never runs automatically and rejects active, waiting, or ready jobs.
func (c *Coordinator) Retry(ctx context.Context, jobID string) (Job, error) {
	if ctx == nil {
		return Job{}, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	if strings.TrimSpace(jobID) == "" {
		return Job{}, ErrInvalidInput
	}

	c.opMu.Lock()
	defer c.opMu.Unlock()

	c.stateMu.Lock()
	if c.closed || c.closing {
		c.stateMu.Unlock()
		return Job{}, ErrClosed
	}
	if c.active != nil && c.active.job.ID == jobID {
		c.stateMu.Unlock()
		return Job{}, ErrJobNotRetryable
	}
	for _, j := range c.waiting {
		if j.ID == jobID {
			c.stateMu.Unlock()
			return Job{}, ErrJobNotRetryable
		}
	}
	var target *Job
	for i := range c.history {
		if c.history[i].ID == jobID {
			j := c.history[i].clone()
			target = &j
			break
		}
	}
	if target == nil {
		c.stateMu.Unlock()
		return Job{}, ErrJobNotFound
	}
	if target.State != store.JobFailed && target.State != store.JobCanceled {
		c.stateMu.Unlock()
		return Job{}, ErrJobNotRetryable
	}
	if existing, ok := c.findLiveLocked(target.Key); ok {
		c.stateMu.Unlock()
		return existing, ErrAlreadyQueued
	}
	c.stateMu.Unlock()

	return c.enqueueNewLocked(ctx, target.Key)
}

func (c *Coordinator) findLiveLocked(key store.BookKey) (Job, bool) {
	if c.active != nil && c.active.job.Key == key {
		active := c.active.job.clone()
		active.Canceling = c.active.canceling
		return active, true
	}
	for _, j := range c.waiting {
		if j.Key == key {
			return j.clone(), true
		}
	}
	return Job{}, false
}

func (c *Coordinator) enqueueNewLocked(ctx context.Context, key store.BookKey) (Job, error) {
	media, found, err := c.store.LocalMedia(ctx, key)
	if err != nil {
		return Job{}, err
	}
	if found {
		ok, err := c.verifier.VerifyMedia(ctx, media)
		if err != nil {
			return Job{}, err
		}
		if ok {
			return Job{}, ErrAlreadyDownloaded
		}
	}

	c.stateMu.Lock()
	if c.closed || c.closing {
		c.stateMu.Unlock()
		return Job{}, ErrClosed
	}
	nowMS := max(c.clock().UnixMilli(), 0)
	jobID := c.nextJobIDLocked(nowMS)
	var nextOrder int64
	if len(c.waiting) > 0 {
		nextOrder = *c.waiting[len(c.waiting)-1].QueueOrder + 1
	}
	c.stateMu.Unlock()

	order := nextOrder
	storeJob := store.DownloadJob{
		ID:          jobID,
		Key:         key,
		QueueOrder:  &order,
		State:       store.JobWaiting,
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}
	if err := c.store.PutDownloadJob(ctx, storeJob); err != nil {
		return Job{}, err
	}

	job := jobFromStore(storeJob)
	c.stateMu.Lock()
	c.usedIDs[job.ID] = true
	c.waiting = append(c.waiting, job)
	c.revision++
	c.notifyLocked(true)
	if !c.closing && !c.closed {
		c.wakeWorkerLocked()
	}
	c.stateMu.Unlock()

	return job.clone(), nil
}

func (c *Coordinator) nextJobIDLocked(nowMS int64) string {
	for {
		c.nextSeq++
		id := fmt.Sprintf("dl-%d-%d", nowMS, c.nextSeq)
		if !c.usedIDs[id] {
			return id
		}
	}
}

// CancelActive requests cancellation of the currently active download job while
// keeping the active slot occupied and Canceling marked until the worker joins.
func (c *Coordinator) CancelActive(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	c.stateMu.Lock()
	if c.closed || c.closing {
		c.stateMu.Unlock()
		return ErrClosed
	}
	if c.active == nil || c.active.finished {
		c.stateMu.Unlock()
		return ErrNoActiveJob
	}
	alreadyCanceling := c.active.canceling
	c.active.canceling = true
	opCancel := c.active.opCancel
	if !alreadyCanceling {
		c.revision++
		c.notifyLocked(true)
	}
	c.stateMu.Unlock()

	opCancel()
	return nil
}

// RemoveWaiting removes a waiting job from the queue without touching any active
// job, local media, history, bookmarks, or favorites.
func (c *Coordinator) RemoveWaiting(ctx context.Context, jobID string) error {
	if ctx == nil {
		return ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(jobID) == "" {
		return ErrInvalidInput
	}

	c.opMu.Lock()
	defer c.opMu.Unlock()

	c.stateMu.Lock()
	if c.closed || c.closing {
		c.stateMu.Unlock()
		return ErrClosed
	}
	if c.active != nil && c.active.job.ID == jobID {
		c.stateMu.Unlock()
		return ErrJobNotWaiting
	}
	idx := slices.IndexFunc(c.waiting, func(j Job) bool { return j.ID == jobID })
	if idx < 0 {
		inHistory := slices.ContainsFunc(c.history, func(j Job) bool { return j.ID == jobID })
		c.stateMu.Unlock()
		if inHistory {
			return ErrJobNotWaiting
		}
		return ErrJobNotFound
	}
	target := c.waiting[idx].clone()
	c.stateMu.Unlock()

	nowMS := max(target.CreatedAtMS, max(c.clock().UnixMilli(), 0))
	canceledStore := store.DownloadJob{
		ID:            target.ID,
		Key:           target.Key,
		QueueOrder:    nil,
		State:         store.JobCanceled,
		BytesReceived: target.BytesReceived,
		BytesTotal:    cloneInt64Ptr(target.BytesTotal),
		CreatedAtMS:   target.CreatedAtMS,
		UpdatedAtMS:   nowMS,
	}
	if err := c.store.PutDownloadJob(ctx, canceledStore); err != nil {
		return err
	}

	canceledJob := jobFromStore(canceledStore)
	c.stateMu.Lock()
	c.waiting = append(c.waiting[:idx], c.waiting[idx+1:]...)
	c.history = append(c.history, canceledJob)
	sortHistory(c.history)
	c.revision++
	c.notifyLocked(true)
	c.stateMu.Unlock()
	return nil
}

// ReorderWaiting atomically persists a new waiting queue order and preserves the
// authoritative order if persistence fails.
func (c *Coordinator) ReorderWaiting(ctx context.Context, orderedIDs []string) error {
	if ctx == nil {
		return ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	c.opMu.Lock()
	defer c.opMu.Unlock()

	c.stateMu.Lock()
	if c.closed || c.closing {
		c.stateMu.Unlock()
		return ErrClosed
	}
	waiting := slices.Clone(c.waiting)
	c.stateMu.Unlock()

	if len(orderedIDs) != len(waiting) {
		return ErrInvalidInput
	}
	byID := make(map[string]Job, len(waiting))
	var maxCreated int64
	for _, j := range waiting {
		byID[j.ID] = j
		maxCreated = max(maxCreated, j.CreatedAtMS)
	}
	seen := make(map[string]struct{}, len(orderedIDs))
	for _, id := range orderedIDs {
		if _, ok := byID[id]; !ok {
			return ErrInvalidInput
		}
		if _, dup := seen[id]; dup {
			return ErrInvalidInput
		}
		seen[id] = struct{}{}
	}
	if len(orderedIDs) == 0 {
		return nil
	}

	nowMS := max(maxCreated, max(c.clock().UnixMilli(), 0))
	if err := c.store.ReorderDownloadQueue(ctx, orderedIDs, nowMS); err != nil {
		return err
	}

	reordered := make([]Job, len(orderedIDs))
	for i, id := range orderedIDs {
		j := byID[id].clone()
		order := int64(i)
		j.QueueOrder = &order
		j.UpdatedAtMS = max(j.UpdatedAtMS, nowMS)
		reordered[i] = j
	}

	c.stateMu.Lock()
	c.waiting = reordered
	c.revision++
	c.notifyLocked(true)
	c.stateMu.Unlock()
	return nil
}

// MoveWaiting shifts a waiting job by delta positions and persists atomically,
// preserving the authoritative order if persistence fails.
func (c *Coordinator) MoveWaiting(ctx context.Context, jobID string, delta int) error {
	if ctx == nil {
		return ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(jobID) == "" {
		return ErrInvalidInput
	}

	c.opMu.Lock()
	defer c.opMu.Unlock()

	c.stateMu.Lock()
	if c.closed || c.closing {
		c.stateMu.Unlock()
		return ErrClosed
	}
	if c.active != nil && c.active.job.ID == jobID {
		c.stateMu.Unlock()
		return ErrJobNotWaiting
	}
	idx := slices.IndexFunc(c.waiting, func(j Job) bool { return j.ID == jobID })
	if idx < 0 {
		inHistory := slices.ContainsFunc(c.history, func(j Job) bool { return j.ID == jobID })
		c.stateMu.Unlock()
		if inHistory {
			return ErrJobNotWaiting
		}
		return ErrJobNotFound
	}
	waiting := slices.Clone(c.waiting)
	c.stateMu.Unlock()

	targetIdx := min(len(waiting)-1, max(0, idx+delta))
	if targetIdx == idx {
		return nil
	}

	reordered := make([]Job, 0, len(waiting))
	moved := waiting[idx]
	without := append(slices.Clone(waiting[:idx]), waiting[idx+1:]...)
	reordered = append(reordered, without[:targetIdx]...)
	reordered = append(reordered, moved)
	reordered = append(reordered, without[targetIdx:]...)

	orderedIDs := make([]string, len(reordered))
	var maxCreated int64
	for i, j := range reordered {
		orderedIDs[i] = j.ID
		maxCreated = max(maxCreated, j.CreatedAtMS)
	}

	nowMS := max(maxCreated, max(c.clock().UnixMilli(), 0))
	if err := c.store.ReorderDownloadQueue(ctx, orderedIDs, nowMS); err != nil {
		return err
	}

	for i := range reordered {
		order := int64(i)
		reordered[i] = reordered[i].clone()
		reordered[i].QueueOrder = &order
		reordered[i].UpdatedAtMS = max(reordered[i].UpdatedAtMS, nowMS)
	}

	c.stateMu.Lock()
	c.waiting = reordered
	c.revision++
	c.notifyLocked(true)
	c.stateMu.Unlock()
	return nil
}

// Close cancels any active download, waits for the worker to join and persist
// final state, and closes watchers. Repeated and concurrent Close calls are safe.
func (c *Coordinator) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	c.stateMu.Lock()
	if c.closed {
		err := c.closeErr
		c.stateMu.Unlock()
		return err
	}
	if err := ctx.Err(); err != nil {
		c.stateMu.Unlock()
		return err
	}
	c.closing = true
	if !c.stopSignaled {
		close(c.stopWorkerCh)
		c.stopSignaled = true
	}
	var opCancel context.CancelFunc
	if c.active != nil && !c.active.finished {
		alreadyCanceling := c.active.canceling
		c.active.canceling = true
		opCancel = c.active.opCancel
		if !alreadyCanceling {
			c.revision++
			c.notifyLocked(true)
		}
	}
	c.stateMu.Unlock()

	if opCancel != nil {
		opCancel()
	}

	select {
	case <-c.workerDone:
	case <-ctx.Done():
		return ctx.Err()
	}

	c.closeOnce.Do(func() {
		close(c.closedCh)
	})
	c.watchWG.Wait()

	c.stateMu.Lock()
	c.closed = true
	err := c.closeErr
	c.stateMu.Unlock()
	return err
}

func (c *Coordinator) workerLoop() {
	defer close(c.workerDone)
	for {
		select {
		case <-c.stopWorkerCh:
			return
		default:
		}

		job, opCtx, opCancel, ok := c.claimNextJob()
		if !ok {
			select {
			case <-c.stopWorkerCh:
				return
			case <-c.wakeCh:
				continue
			}
		}
		c.executeJob(job, opCtx, opCancel)
	}
}

func (c *Coordinator) claimNextJob() (Job, context.Context, context.CancelFunc, bool) {
	c.opMu.Lock()
	defer c.opMu.Unlock()

	c.stateMu.Lock()
	if c.closing || c.closed || c.active != nil || len(c.waiting) == 0 {
		c.stateMu.Unlock()
		return Job{}, nil, nil, false
	}
	next := c.waiting[0].clone()
	c.stateMu.Unlock()

	nowMS := max(next.CreatedAtMS, max(c.clock().UnixMilli(), 0))
	activeStore := store.DownloadJob{
		ID:          next.ID,
		Key:         next.Key,
		QueueOrder:  nil,
		State:       store.JobLicensing,
		CreatedAtMS: next.CreatedAtMS,
		UpdatedAtMS: nowMS,
	}
	if err := c.store.PutDownloadJob(context.Background(), activeStore); err != nil {
		c.stateMu.Lock()
		c.lastError = "failed to persist download start"
		c.revision++
		c.notifyLocked(true)
		c.stateMu.Unlock()
		return Job{}, nil, nil, false
	}

	c.stateMu.Lock()
	if c.closing || c.closed {
		c.stateMu.Unlock()
		// Restore waiting state in store so shutdown preserves the waiting queue.
		restoreStore := store.DownloadJob{
			ID:            next.ID,
			Key:           next.Key,
			QueueOrder:    cloneInt64Ptr(next.QueueOrder),
			State:         store.JobWaiting,
			BytesReceived: next.BytesReceived,
			BytesTotal:    cloneInt64Ptr(next.BytesTotal),
			CreatedAtMS:   next.CreatedAtMS,
			UpdatedAtMS:   nowMS,
		}
		if err := c.store.PutDownloadJob(context.Background(), restoreStore); err != nil {
			c.stateMu.Lock()
			c.closeErr = errors.Join(c.closeErr, err)
			c.stateMu.Unlock()
		}
		return Job{}, nil, nil, false
	}

	c.waiting = append([]Job(nil), c.waiting[1:]...)
	opCtx, opCancel := context.WithCancel(context.Background())
	activeJob := jobFromStore(activeStore)
	c.active = &activeJobState{
		job:       activeJob,
		opCancel:  opCancel,
		stageRank: 1,
	}
	c.lastError = ""
	c.revision++
	c.notifyLocked(true)
	c.stateMu.Unlock()

	return activeJob.clone(), opCtx, opCancel, true
}

func (c *Coordinator) executeJob(job Job, opCtx context.Context, opCancel context.CancelFunc) {
	defer opCancel()

	outcome, err := c.downloader.Download(opCtx, job.ID, job.Key, func(ev DownloadEvent) {
		c.handleEvent(job.ID, ev)
	})

	c.stateMu.Lock()
	c.active.finished = true
	cancelRequested := c.active.canceling
	current := c.active.job.clone()
	c.stateMu.Unlock()

	c.opMu.Lock()
	defer c.opMu.Unlock()

	nowMS := max(current.CreatedAtMS, max(c.clock().UnixMilli(), 0))
	finalJob, persistNotice := c.resolveOutcome(current, outcome, err, cancelRequested, nowMS)

	if putErr := c.store.PutDownloadJob(context.Background(), toStoreJob(finalJob)); putErr != nil {
		persistNotice = "failed to persist download outcome"
		c.stateMu.Lock()
		if c.closing {
			c.closeErr = errors.Join(c.closeErr, putErr)
		}
		c.stateMu.Unlock()
	}

	c.stateMu.Lock()
	c.active = nil
	c.history = append(c.history, finalJob)
	sortHistory(c.history)
	if persistNotice != "" {
		c.lastError = persistNotice
	}
	c.revision++
	c.notifyLocked(true)
	if !c.closing && !c.closed && len(c.waiting) > 0 {
		c.wakeWorkerLocked()
	}
	c.stateMu.Unlock()
}

func (c *Coordinator) resolveOutcome(current Job, outcome DownloadOutcome, err error, cancelRequested bool, nowMS int64) (Job, string) {
	finalJob := current.clone()
	finalJob.QueueOrder = nil
	finalJob.Canceling = false
	finalJob.UpdatedAtMS = nowMS

	if outcome.Media != nil {
		media := store.LocalMedia{
			Key:           current.Key,
			RelativePath:  outcome.Media.RelativePath,
			Format:        outcome.Media.Format,
			PublishedAtMS: nowMS,
		}
		if media.Validate() != nil {
			finalJob.State = store.JobFailed
			finalJob.FailureCode = store.FailureInternal
			finalJob.FailureMessage = safeFailureMessage(store.FailureInternal, "")
			finalJob.RetryDisposition = retryDispositionFor(store.FailureInternal, "")
			return finalJob, ""
		}
		if putErr := c.store.PutLocalMedia(context.Background(), media); putErr != nil {
			finalJob.State = store.JobFailed
			finalJob.FailureCode = store.FailureInternal
			finalJob.FailureMessage = safeFailureMessage(store.FailureInternal, "")
			finalJob.RetryDisposition = retryDispositionFor(store.FailureInternal, "")
			return finalJob, "failed to persist local media"
		}
		finalJob.State = store.JobReady
		finalJob.FailureCode = ""
		finalJob.FailureMessage = ""
		finalJob.RetryDisposition = ""
		if err != nil && !isCanceledError(err) {
			return finalJob, "media ready; post-commit warning"
		}
		return finalJob, ""
	}

	if cancelRequested || isCanceledError(err) {
		finalJob.State = store.JobCanceled
		finalJob.FailureCode = ""
		finalJob.FailureMessage = ""
		finalJob.RetryDisposition = ""
		return finalJob, ""
	}

	rawCode, rawRetry := extractFailure(err)
	normCode := store.NormalizeFailureCode(rawCode)
	finalJob.State = store.JobFailed
	finalJob.FailureCode = normCode
	finalJob.FailureMessage = safeFailureMessage(normCode, rawCode)
	finalJob.RetryDisposition = retryDispositionFor(normCode, rawRetry)
	return finalJob, ""
}

func (c *Coordinator) handleEvent(expectedJobID string, ev DownloadEvent) {
	if ev.OperationID != expectedJobID {
		return
	}
	switch ev.Kind {
	case EventStage:
		targetState, rank, ok := stageToJobState(ev.Stage)
		if !ok {
			return
		}
		c.opMu.Lock()
		defer c.opMu.Unlock()

		c.stateMu.Lock()
		if c.active == nil || c.active.finished || c.active.job.ID != expectedJobID {
			c.stateMu.Unlock()
			return
		}
		if ev.Sequence != 0 && ev.Sequence <= c.active.lastSeq {
			c.stateMu.Unlock()
			return
		}
		if rank < c.active.stageRank {
			c.stateMu.Unlock()
			return
		}
		if ev.Sequence != 0 {
			c.active.lastSeq = ev.Sequence
		}
		if rank == c.active.stageRank {
			c.stateMu.Unlock()
			return
		}
		nowMS := max(c.active.job.CreatedAtMS, max(c.clock().UnixMilli(), 0))
		stageStore := store.DownloadJob{
			ID:            c.active.job.ID,
			Key:           c.active.job.Key,
			QueueOrder:    nil,
			State:         targetState,
			BytesReceived: c.active.job.BytesReceived,
			BytesTotal:    cloneInt64Ptr(c.active.job.BytesTotal),
			CreatedAtMS:   c.active.job.CreatedAtMS,
			UpdatedAtMS:   nowMS,
		}
		c.stateMu.Unlock()

		putErr := c.store.PutDownloadJob(context.Background(), stageStore)

		c.stateMu.Lock()
		if c.active != nil && !c.active.finished && c.active.job.ID == expectedJobID {
			c.active.stageRank = rank
			c.active.job.State = targetState
			c.active.job.UpdatedAtMS = nowMS
			if putErr != nil {
				c.lastError = "failed to persist download stage"
			}
			c.revision++
			c.notifyLocked(true)
		}
		c.stateMu.Unlock()

	case EventProgress:
		if ev.Bytes == nil || *ev.Bytes < 0 {
			return
		}
		if ev.Total != nil && (*ev.Total < 0 || *ev.Total < *ev.Bytes) {
			return
		}

		c.stateMu.Lock()
		defer c.stateMu.Unlock()
		if c.active == nil || c.active.finished || c.active.job.ID != expectedJobID {
			return
		}
		if c.active.job.State != store.JobDownloading {
			return
		}
		if ev.Sequence != 0 && ev.Sequence <= c.active.lastSeq {
			return
		}
		if *ev.Bytes < c.active.job.BytesReceived {
			return
		}
		if ev.Total != nil && c.active.job.BytesTotal != nil && *ev.Total != *c.active.job.BytesTotal {
			return
		}
		if ev.Sequence != 0 {
			c.active.lastSeq = ev.Sequence
		}
		c.active.job.BytesReceived = *ev.Bytes
		if ev.Total != nil && c.active.job.BytesTotal == nil {
			total := *ev.Total
			c.active.job.BytesTotal = &total
		}
		c.active.job.UpdatedAtMS = max(c.active.job.UpdatedAtMS, max(c.clock().UnixMilli(), 0))
		c.revision++
		c.notifyLocked(false)
	}
}

type rootMediaVerifier struct {
	owner store.RootOwner
	euid  uint32
}

func (v *rootMediaVerifier) VerifyMedia(ctx context.Context, media store.LocalMedia) (bool, error) {
	if ctx == nil {
		return false, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if media.Validate() != nil {
		return false, nil
	}
	lease, err := v.owner.Lease(ctx)
	if err != nil {
		return false, err
	}
	defer lease.Close()

	var verified bool
	err = store.WithRoot(lease.Context, lease.Root, func(_ context.Context, root *os.File) error {
		ok, checkErr := verifyPrivateMediaFile(int(root.Fd()), media.RelativePath, v.euid)
		if checkErr != nil {
			return checkErr
		}
		verified = ok
		return nil
	})
	if err != nil {
		return false, err
	}
	return verified, nil
}

func verifyPrivateMediaFile(rootFD int, relPath string, euid uint32) (bool, error) {
	parts := strings.Split(relPath, "/")
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
			if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
				return false, nil
			}
			return false, store.ErrIO
		}
		dirFD = nextFD
		closeDir = true

		var st unix.Stat_t
		if err := unix.Fstat(dirFD, &st); err != nil {
			return false, store.ErrIO
		}
		if st.Mode&(unix.S_IFMT|07777) != unix.S_IFDIR|0700 || st.Uid != euid {
			return false, nil
		}
	}

	var st unix.Stat_t
	if err := unix.Fstatat(dirFD, parts[len(parts)-1], &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			return false, nil
		}
		return false, store.ErrIO
	}
	return st.Mode&(unix.S_IFMT|07777) == unix.S_IFREG|0600 && st.Uid == euid && st.Nlink == 1, nil
}

func stageToJobState(stage Stage) (store.JobState, int, bool) {
	switch stage {
	case StageLicensing:
		return store.JobLicensing, 1, true
	case StageDownloading:
		return store.JobDownloading, 2, true
	case StagePreparing:
		return store.JobPreparing, 3, true
	case StagePublishing:
		return store.JobPublishing, 4, true
	default:
		return "", 0, false
	}
}

func isCanceledError(err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	var f Failure
	return errors.As(err, &f) && f.Code == "canceled"
}

func extractFailure(err error) (string, string) {
	var f Failure
	if errors.As(err, &f) {
		return f.Code, f.Retry
	}
	return "", ""
}

func safeFailureMessage(norm store.FailureCode, rawCode string) string {
	switch rawCode {
	case "not_authenticated":
		return "Sign in to Audible before downloading."
	case "busy":
		return "Another Audible operation is already running."
	case "runtime_missing", "runtime_incompatible":
		return "Required download runtime dependency is unavailable."
	case "protocol_error":
		return "Download response was invalid."
	case "unsupported":
		return "Download is unsupported for this item."
	}
	switch norm {
	case store.FailureNetwork:
		return "Network error during download."
	case store.FailureThrottled:
		return "Audible rate-limited the download request; retry later."
	case store.FailureEntitlementDenied:
		return "Audible did not grant a download license."
	case store.FailureDiskFull:
		return "Insufficient disk space for download."
	case store.FailurePermissionDenied:
		return "Permission denied accessing private download storage."
	case store.FailurePreparationFailed:
		return "Media preparation failed."
	case store.FailureInterrupted:
		return "Previous download attempt was interrupted; retry explicitly."
	default:
		return "Download failed."
	}
}

func retryDispositionFor(norm store.FailureCode, rawRetry string) string {
	switch rawRetry {
	case "user_retry", "sign_in", "check_entitlement", "wait", "none":
		return rawRetry
	}
	switch norm {
	case store.FailureEntitlementDenied:
		return "check_entitlement"
	case store.FailureThrottled:
		return "wait"
	default:
		return "user_retry"
	}
}

func jobFromStore(sj store.DownloadJob) Job {
	j := Job{
		ID:            sj.ID,
		Key:           sj.Key,
		QueueOrder:    cloneInt64Ptr(sj.QueueOrder),
		State:         sj.State,
		BytesReceived: sj.BytesReceived,
		BytesTotal:    cloneInt64Ptr(sj.BytesTotal),
		FailureCode:   sj.FailureCode,
		CreatedAtMS:   sj.CreatedAtMS,
		UpdatedAtMS:   sj.UpdatedAtMS,
	}
	if sj.State == store.JobFailed {
		j.FailureMessage = safeFailureMessage(sj.FailureCode, string(sj.FailureCode))
		j.RetryDisposition = retryDispositionFor(sj.FailureCode, "")
	}
	return j
}

func toStoreJob(j Job) store.DownloadJob {
	return store.DownloadJob{
		ID:            j.ID,
		Key:           j.Key,
		QueueOrder:    cloneInt64Ptr(j.QueueOrder),
		State:         j.State,
		BytesReceived: j.BytesReceived,
		BytesTotal:    cloneInt64Ptr(j.BytesTotal),
		FailureCode:   j.FailureCode,
		CreatedAtMS:   j.CreatedAtMS,
		UpdatedAtMS:   j.UpdatedAtMS,
	}
}

func sortHistory(history []Job) {
	sort.Slice(history, func(i, j int) bool {
		if history[i].CreatedAtMS != history[j].CreatedAtMS {
			return history[i].CreatedAtMS < history[j].CreatedAtMS
		}
		return history[i].ID < history[j].ID
	})
}

func cloneInt64Ptr(v *int64) *int64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
