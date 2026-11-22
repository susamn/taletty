package playback

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/susamn/taletty/app/internal/store"
)

const (
	MinSpeedMilli     int64 = 500_000
	DefaultSpeedMilli int64 = 1_000_000
	MaxSpeedMilli     int64 = 3_000_000
	SpeedStepMilli    int64 = 250_000

	MinVolumePercent     int = 0
	DefaultVolumePercent int = 100
	MaxVolumePercent     int = 100
)

var (
	// ErrInvalidInput indicates malformed playback arguments or track metadata.
	ErrInvalidInput = errors.New("invalid playback input")
	// ErrClosed indicates the playback controller has been closed.
	ErrClosed = errors.New("playback controller closed")
	// ErrNoActiveTrack indicates a transport command was issued with no loaded track.
	ErrNoActiveTrack = errors.New("no active playback track")
	// ErrStaleSession indicates a transport command targeted a superseded playback session.
	ErrStaleSession = errors.New("stale playback session")
)

// State represents the authoritative lifecycle state of local playback.
type State int

const (
	StateIdle State = iota
	StateLoading
	StatePlaying
	StatePaused
	StateStopped
	StateFailed
)

// String returns a human-readable lowercase label for s.
func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateLoading:
		return "loading"
	case StatePlaying:
		return "playing"
	case StatePaused:
		return "paused"
	case StateStopped:
		return "stopped"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// Track describes a validated local audiobook track ready for playback.
type Track struct {
	Key      store.BookKey
	Title    string
	Path     string
	Duration time.Duration
}

// Validate checks identity, safe title, clean absolute .m4b path and nonnegative duration.
func (t Track) Validate() error {
	if err := t.Key.Validate(); err != nil {
		return ErrInvalidInput
	}
	if strings.TrimSpace(t.Title) == "" || !safeSingleLine(t.Title) {
		return ErrInvalidInput
	}
	if err := validateMediaPath(t.Path); err != nil {
		return ErrInvalidInput
	}
	if t.Duration < 0 {
		return ErrInvalidInput
	}
	return nil
}

// Snapshot is an immutable point-in-time view of playback state.
type Snapshot struct {
	State         State
	Session       uint64
	Book          *store.BookKey
	Title         string
	Position      time.Duration
	Duration      time.Duration
	SpeedMilli    int64
	VolumePercent int
	Failure       string
}

// CheckpointStore persists per-book listening positions and playback speeds.
// *store.Store satisfies this interface directly.
type CheckpointStore interface {
	ListeningState(ctx context.Context, key store.BookKey) (store.ListeningState, bool, error)
	PutListeningState(ctx context.Context, state store.ListeningState) error
}

// Options configures a Controller.
type Options struct {
	Store    CheckpointStore
	Clock    func() time.Time
	OnChange func(Snapshot)
	Volume   *int
}

// Controller is the single serialized authority for playback state, backend calls and checkpoints.
type Controller struct {
	backend  Backend
	store    CheckpointStore
	clock    func() time.Time
	onChange func(Snapshot)

	opMu    sync.Mutex
	stateMu sync.Mutex

	closed     bool
	state      State
	session    uint64
	book       *store.BookKey
	title      string
	position   time.Duration
	duration   time.Duration
	speedMilli int64
	volume     int
	completion store.Completion
	failure    string
}

// NewController constructs a serialized playback Controller around backend.
func NewController(backend Backend, opts Options) (*Controller, error) {
	if backend == nil {
		return nil, ErrInvalidInput
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	volume := DefaultVolumePercent
	if opts.Volume != nil {
		volume = clampVolume(*opts.Volume)
	}
	return &Controller{
		backend:    backend,
		store:      opts.Store,
		clock:      clock,
		onChange:   opts.OnChange,
		state:      StateIdle,
		speedMilli: DefaultSpeedMilli,
		volume:     volume,
		completion: store.CompletionUnfinished,
	}, nil
}

// Snapshot returns an isolated copy of the current playback snapshot.
func (c *Controller) Snapshot() Snapshot {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.snapshotLocked()
}

func (c *Controller) snapshotLocked() Snapshot {
	snap := Snapshot{
		State:         c.state,
		Session:       c.session,
		Title:         c.title,
		Position:      c.position,
		Duration:      c.duration,
		SpeedMilli:    c.speedMilli,
		VolumePercent: c.volume,
		Failure:       c.failure,
	}
	if c.book != nil {
		key := *c.book
		snap.Book = &key
	}
	return snap
}

func (c *Controller) emit() Snapshot {
	c.stateMu.Lock()
	snap := c.snapshotLocked()
	c.stateMu.Unlock()
	if c.onChange != nil {
		c.onChange(snap)
	}
	return snap
}

// Load switches to track (saving any active previous track's final checkpoint first),
// restores saved position/speed if available, increments Session and starts playback.
func (c *Controller) Load(ctx context.Context, track Track, autoPlay bool) (Snapshot, error) {
	if ctx == nil {
		return c.Snapshot(), ErrInvalidInput
	}
	if err := track.Validate(); err != nil {
		return c.Snapshot(), err
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}

	c.opMu.Lock()
	defer c.opMu.Unlock()

	if c.closed {
		return c.Snapshot(), ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}

	if c.isActiveLocked() {
		if pos, dur, _, _, err := c.backend.Position(ctx); err == nil {
			c.updatePositionLocked(pos, dur)
		}
		if err := c.saveCheckpointLocked(ctx); err != nil {
			return c.Snapshot(), err
		}
		if err := c.backend.Stop(ctx); err != nil {
			return c.failLocked(err), err
		}
	}

	c.stateMu.Lock()
	c.session++
	c.stateMu.Unlock()

	key := track.Key
	pos := time.Duration(0)
	speed := DefaultSpeedMilli
	completion := store.CompletionUnfinished

	if c.store != nil {
		saved, found, err := c.store.ListeningState(ctx, key)
		if err != nil {
			c.stateMu.Lock()
			c.book = &key
			c.title = track.Title
			c.position = 0
			c.duration = track.Duration
			c.speedMilli = speed
			c.completion = completion
			c.stateMu.Unlock()
			return c.failLocked(err), err
		}
		if found {
			pos = time.Duration(saved.PositionNS)
			if pos < 0 {
				pos = 0
			}
			if track.Duration > 0 && pos > track.Duration {
				pos = track.Duration
			}
			if saved.SpeedMilli > 0 {
				speed = clampSpeed(saved.SpeedMilli)
			}
			if saved.Completion == store.CompletionFinished || saved.Completion == store.CompletionUnfinished {
				completion = saved.Completion
			}
		}
	}

	c.stateMu.Lock()
	c.state = StateLoading
	c.book = &key
	c.title = track.Title
	c.position = pos
	c.duration = track.Duration
	c.speedMilli = speed
	c.completion = completion
	c.failure = ""
	volume := c.volume
	c.stateMu.Unlock()
	c.emit()

	if err := c.backend.Start(ctx, track.Path, pos, speed, volume, !autoPlay); err != nil {
		return c.failLocked(err), err
	}

	c.stateMu.Lock()
	if autoPlay {
		c.state = StatePlaying
	} else {
		c.state = StatePaused
	}
	c.failure = ""
	c.stateMu.Unlock()

	if err := c.saveCheckpointLocked(ctx); err != nil {
		_ = c.backend.Stop(context.Background())
		return c.failLocked(err), err
	}

	return c.emit(), nil
}

// Toggle switches between playing and paused for the matching session.
func (c *Controller) Toggle(ctx context.Context, session uint64) (Snapshot, error) {
	if ctx == nil {
		return c.Snapshot(), ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if c.closed {
		return c.Snapshot(), ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	if err := c.checkTransportLocked(session); err != nil {
		return c.Snapshot(), err
	}
	if c.state == StatePlaying {
		return c.pauseLocked(ctx)
	}
	return c.playLocked(ctx)
}

// Pause pauses active playback and persists a checkpoint.
func (c *Controller) Pause(ctx context.Context, session uint64) (Snapshot, error) {
	if ctx == nil {
		return c.Snapshot(), ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if c.closed {
		return c.Snapshot(), ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	if err := c.checkTransportLocked(session); err != nil {
		return c.Snapshot(), err
	}
	return c.pauseLocked(ctx)
}

// Play resumes paused playback for the matching session.
func (c *Controller) Play(ctx context.Context, session uint64) (Snapshot, error) {
	if ctx == nil {
		return c.Snapshot(), ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if c.closed {
		return c.Snapshot(), ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	if err := c.checkTransportLocked(session); err != nil {
		return c.Snapshot(), err
	}
	return c.playLocked(ctx)
}

func (c *Controller) pauseLocked(ctx context.Context) (Snapshot, error) {
	if err := c.backend.SetPause(ctx, true); err != nil {
		return c.failBackendLocked(err), err
	}
	if pos, dur, _, _, err := c.backend.Position(ctx); err == nil {
		c.updatePositionLocked(pos, dur)
	}
	c.stateMu.Lock()
	c.state = StatePaused
	c.failure = ""
	c.stateMu.Unlock()

	if err := c.saveCheckpointLocked(ctx); err != nil {
		c.setFailureLocked(err)
		return c.emit(), err
	}
	return c.emit(), nil
}

func (c *Controller) playLocked(ctx context.Context) (Snapshot, error) {
	if err := c.backend.SetPause(ctx, false); err != nil {
		return c.failBackendLocked(err), err
	}
	c.stateMu.Lock()
	c.state = StatePlaying
	c.failure = ""
	c.stateMu.Unlock()
	return c.emit(), nil
}

// SeekRelative adjusts position by delta while preserving pause state and persists a checkpoint.
func (c *Controller) SeekRelative(ctx context.Context, session uint64, delta time.Duration) (Snapshot, error) {
	if ctx == nil {
		return c.Snapshot(), ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if c.closed {
		return c.Snapshot(), ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	if err := c.checkTransportLocked(session); err != nil {
		return c.Snapshot(), err
	}
	pos, dur, _, _, err := c.backend.Position(ctx)
	if err != nil {
		return c.failBackendLocked(err), err
	}
	c.updatePositionLocked(pos, dur)
	c.stateMu.Lock()
	target := clampPosition(c.position+delta, c.duration)
	c.stateMu.Unlock()
	return c.seekLocked(ctx, target)
}

// SeekAbsolute seeks to target while preserving pause state and persists a checkpoint.
func (c *Controller) SeekAbsolute(ctx context.Context, session uint64, target time.Duration) (Snapshot, error) {
	if ctx == nil {
		return c.Snapshot(), ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if c.closed {
		return c.Snapshot(), ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	if err := c.checkTransportLocked(session); err != nil {
		return c.Snapshot(), err
	}
	c.stateMu.Lock()
	target = clampPosition(target, c.duration)
	c.stateMu.Unlock()
	return c.seekLocked(ctx, target)
}

func (c *Controller) seekLocked(ctx context.Context, target time.Duration) (Snapshot, error) {
	if err := c.backend.SeekExact(ctx, target); err != nil {
		return c.failBackendLocked(err), err
	}
	c.stateMu.Lock()
	c.position = target
	c.failure = ""
	c.stateMu.Unlock()

	if err := c.saveCheckpointLocked(ctx); err != nil {
		c.setFailureLocked(err)
		return c.emit(), err
	}
	return c.emit(), nil
}

// StepSpeed adjusts playback speed by deltaMilli, clamps to [MinSpeedMilli, MaxSpeedMilli],
// updates the backend and persists a checkpoint.
func (c *Controller) StepSpeed(ctx context.Context, session uint64, deltaMilli int64) (Snapshot, error) {
	if ctx == nil {
		return c.Snapshot(), ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if c.closed {
		return c.Snapshot(), ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	if err := c.checkTransportLocked(session); err != nil {
		return c.Snapshot(), err
	}
	return c.setSpeedLocked(ctx, clampSpeed(c.speedMilli+deltaMilli))
}

// SetSpeed sets playback speed (clamped to [MinSpeedMilli, MaxSpeedMilli]),
// updates the backend and persists a checkpoint.
func (c *Controller) SetSpeed(ctx context.Context, session uint64, speedMilli int64) (Snapshot, error) {
	if ctx == nil || speedMilli <= 0 {
		return c.Snapshot(), ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if c.closed {
		return c.Snapshot(), ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	if err := c.checkTransportLocked(session); err != nil {
		return c.Snapshot(), err
	}
	return c.setSpeedLocked(ctx, clampSpeed(speedMilli))
}

func (c *Controller) setSpeedLocked(ctx context.Context, speedMilli int64) (Snapshot, error) {
	if err := c.backend.SetSpeed(ctx, speedMilli); err != nil {
		return c.failBackendLocked(err), err
	}
	c.stateMu.Lock()
	c.speedMilli = speedMilli
	c.failure = ""
	c.stateMu.Unlock()

	if err := c.saveCheckpointLocked(ctx); err != nil {
		c.setFailureLocked(err)
		return c.emit(), err
	}
	return c.emit(), nil
}

// StepVolume adjusts volume by deltaPercent (clamped 0..100) and applies it if active.
func (c *Controller) StepVolume(ctx context.Context, deltaPercent int) (Snapshot, error) {
	if ctx == nil {
		return c.Snapshot(), ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if c.closed {
		return c.Snapshot(), ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	return c.setVolumeLocked(ctx, clampVolume(c.volume+deltaPercent))
}

// SetVolume clamps volumePercent to 0..100 and applies it to the backend if active.
func (c *Controller) SetVolume(ctx context.Context, volumePercent int) (Snapshot, error) {
	if ctx == nil {
		return c.Snapshot(), ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if c.closed {
		return c.Snapshot(), ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	return c.setVolumeLocked(ctx, clampVolume(volumePercent))
}

func (c *Controller) setVolumeLocked(ctx context.Context, volume int) (Snapshot, error) {
	if c.isActiveLocked() {
		if err := c.backend.SetVolume(ctx, volume); err != nil {
			return c.failBackendLocked(err), err
		}
	}
	c.stateMu.Lock()
	c.volume = volume
	if c.state != StateFailed {
		c.failure = ""
	}
	c.stateMu.Unlock()
	return c.emit(), nil
}

// Poll refreshes position/state from the backend and transitions to StatePaused with a
// persisted checkpoint when EOF is reached.
func (c *Controller) Poll(ctx context.Context) (Snapshot, error) {
	if ctx == nil {
		return c.Snapshot(), ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if c.closed {
		return c.Snapshot(), ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	if !c.isActiveLocked() {
		return c.Snapshot(), nil
	}

	pos, dur, paused, eof, err := c.backend.Position(ctx)
	if err != nil {
		return c.failBackendLocked(err), err
	}
	c.updatePositionLocked(pos, dur)

	if eof {
		c.stateMu.Lock()
		if c.duration > 0 {
			c.position = c.duration
		}
		c.state = StatePaused
		c.failure = ""
		c.stateMu.Unlock()

		if err := c.saveCheckpointLocked(ctx); err != nil {
			c.setFailureLocked(err)
			return c.emit(), err
		}
		return c.emit(), nil
	}

	c.stateMu.Lock()
	if paused {
		c.state = StatePaused
	} else {
		c.state = StatePlaying
	}
	c.failure = ""
	c.stateMu.Unlock()

	return c.emit(), nil
}

// Stop persists a final checkpoint for any active track, stops/reaps the backend,
// and resets playback state to StateStopped.
func (c *Controller) Stop(ctx context.Context) (Snapshot, error) {
	if ctx == nil {
		return c.Snapshot(), ErrInvalidInput
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if c.closed {
		return c.Snapshot(), ErrClosed
	}

	var errs []error
	if c.isActiveLocked() {
		if pos, dur, _, _, err := c.backend.Position(ctx); err == nil {
			c.updatePositionLocked(pos, dur)
		}
		if err := c.saveCheckpointLocked(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if err := c.backend.Stop(ctx); err != nil {
		errs = append(errs, err)
	}

	joined := errors.Join(errs...)
	c.stateMu.Lock()
	c.state = StateStopped
	c.book = nil
	c.title = ""
	c.position = 0
	c.duration = 0
	c.failure = sanitizeFailure(joined)
	c.stateMu.Unlock()

	return c.emit(), joined
}

// Close persists a final checkpoint if active, stops/reaps the backend, and closes the controller.
func (c *Controller) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if c.closed {
		return nil
	}
	c.closed = true

	var errs []error
	if c.isActiveLocked() {
		if pos, dur, _, _, err := c.backend.Position(ctx); err == nil {
			c.updatePositionLocked(pos, dur)
		}
		if err := c.saveCheckpointLocked(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if err := c.backend.Stop(ctx); err != nil {
		errs = append(errs, err)
	}

	c.stateMu.Lock()
	c.state = StateStopped
	c.book = nil
	c.title = ""
	c.position = 0
	c.duration = 0
	c.failure = ""
	c.stateMu.Unlock()

	return errors.Join(errs...)
}

func (c *Controller) isActiveLocked() bool {
	return c.state == StatePlaying || c.state == StatePaused
}

func (c *Controller) checkTransportLocked(session uint64) error {
	if session != 0 && session != c.session {
		return ErrStaleSession
	}
	if !c.isActiveLocked() || c.book == nil {
		return ErrNoActiveTrack
	}
	return nil
}

func (c *Controller) updatePositionLocked(pos, dur time.Duration) {
	c.stateMu.Lock()
	if dur > 0 {
		c.duration = dur
	}
	c.position = clampPosition(pos, c.duration)
	c.stateMu.Unlock()
}

func clampPosition(pos, dur time.Duration) time.Duration {
	if pos < 0 {
		return 0
	}
	if dur > 0 && pos > dur {
		return dur
	}
	return pos
}

func (c *Controller) saveCheckpointLocked(ctx context.Context) error {
	if c.store == nil || c.book == nil {
		return nil
	}
	updatedAtMS := max(c.clock().UnixMilli(), 0)
	posNS := max(c.position.Nanoseconds(), 0)
	return c.store.PutListeningState(ctx, store.ListeningState{
		Key:         *c.book,
		PositionNS:  posNS,
		SpeedMilli:  c.speedMilli,
		Completion:  c.completion,
		UpdatedAtMS: updatedAtMS,
	})
}

func (c *Controller) setFailureLocked(err error) {
	c.stateMu.Lock()
	c.failure = sanitizeFailure(err)
	c.stateMu.Unlock()
}

func (c *Controller) failBackendLocked(err error) Snapshot {
	if errors.Is(err, ErrIPC) {
		_ = c.backend.Stop(context.Background())
	}
	return c.failLocked(err)
}

func (c *Controller) failLocked(err error) Snapshot {
	c.stateMu.Lock()
	c.state = StateFailed
	c.failure = sanitizeFailure(err)
	c.stateMu.Unlock()
	return c.emit()
}

func sanitizeFailure(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrRuntimeMissing):
		return "mpv runtime missing"
	case errors.Is(err, ErrIPC):
		return "mpv playback connection failed"
	case errors.Is(err, context.Canceled):
		return "playback canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "playback timed out"
	default:
		return "playback failed"
	}
}

func clampSpeed(speedMilli int64) int64 {
	return min(MaxSpeedMilli, max(MinSpeedMilli, speedMilli))
}

func clampVolume(volume int) int {
	return min(MaxVolumePercent, max(MinVolumePercent, volume))
}

func safeSingleLine(text string) bool {
	if !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}
