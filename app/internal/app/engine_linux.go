package app

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/susamn/taletty/app/internal/playback"
	"github.com/susamn/taletty/app/internal/profile"
	"github.com/susamn/taletty/app/internal/store"
	"github.com/susamn/taletty/engine/connectors/audible"
	"github.com/susamn/taletty/engine/model"
)

// RootLeaseOwner lends session-lifetime anchored root directory leases.
// *store.Owner satisfies this interface directly.
type RootLeaseOwner interface {
	Lease(ctx context.Context) (*store.RootLease, error)
}

var _ RootLeaseOwner = (*store.Owner)(nil)

type borrowedClient interface {
	Login(ctx context.Context, terminal *os.File) error
	Library(ctx context.Context, operationID string, report func(model.OperationEvent)) (model.LibrarySnapshot, error)
	Close(ctx context.Context) error
}

// EngineAccount adapts the pinned github.com/susamn/taletty/engine Audible
// connector to app.AccountClient using lazy Python discovery and borrowed
// session-lifetime root leases.
type EngineAccount struct {
	owner          RootLeaseOwner
	pythonExplicit string
	discoverPython func(string) (string, error)
	doctorFn       func(context.Context, string) (audible.Diagnostics, error)
	openBorrowed   func(context.Context, *os.File, string) (borrowedClient, error)
}

// NewEngineAccount constructs an AccountClient backed by the pinned engine
// module. Python discovery is deferred until Doctor, Login, or Sync is invoked
// so cached offline browsing works without Python or network access.
func NewEngineAccount(owner RootLeaseOwner, pythonExplicit string) *EngineAccount {
	return &EngineAccount{
		owner:          owner,
		pythonExplicit: pythonExplicit,
		discoverPython: audible.DiscoverPython,
		doctorFn:       audible.Doctor,
		openBorrowed: func(ctx context.Context, root *os.File, python string) (borrowedClient, error) {
			return audible.OpenBorrowed(ctx, root, python)
		},
	}
}

// OpenEngineWorkspace initializes ~/.config/taletty/ for home, wires the pinned
// engine AccountClient, and restores the cached session without autoplay or
// invoking Python during boot.
func OpenEngineWorkspace(ctx context.Context, home, pythonExplicit string, opts Options) (*Coordinator, error) {
	if ctx == nil {
		return nil, ErrInvalidInput
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	nowMS := max(int64(0), clock().UnixMilli())
	ws, err := profile.Open(ctx, home, nowMS)
	if err != nil {
		return nil, err
	}
	settings, err := ws.Store().Settings(ctx)
	if err != nil {
		_ = ws.Close(context.Background())
		return nil, err
	}
	player, err := playback.NewController(playback.NewMPVProcess(playback.MPVConfig{}), playback.Options{
		Store:  ws.Store(),
		Volume: &settings.VolumePercent,
	})
	if err != nil {
		_ = ws.Close(context.Background())
		return nil, err
	}
	opts.Clock = clock
	opts.Account = NewEngineAccount(ws.Owner(), pythonExplicit)
	opts.Player = player
	opts.MediaChecker = NewRootMediaChecker(ws.Owner())
	opts.ChapterReader = FFprobeChapterReader{}
	opts.WorkspaceCloser = ws.Close
	coord, err := NewCoordinator(ctx, ws.Store(), opts)
	if err != nil {
		_ = player.Close(context.Background())
		_ = ws.Close(context.Background())
		return nil, err
	}
	return coord, nil
}

// Doctor runs a read-only runtime readiness probe without acquiring .auth.lock.
func (e *EngineAccount) Doctor(ctx context.Context) (Diagnostics, error) {
	if e == nil || ctx == nil {
		return Diagnostics{}, ErrInvalidInput
	}
	python, err := e.discoverPython(e.pythonExplicit)
	if err != nil {
		return Diagnostics{}, mapEngineError(err)
	}
	diag, err := e.doctorFn(ctx, python)
	if err != nil {
		return Diagnostics{}, mapEngineError(err)
	}
	return Diagnostics{
		Python:  string(diag.Python),
		Audible: string(diag.Audible),
		FFmpeg:  string(diag.FFmpeg),
		FFprobe: string(diag.FFprobe),
	}, nil
}

// Login borrows a root lease and runs interactive browser sign-in on tty.
func (e *EngineAccount) Login(ctx context.Context, tty *os.File) error {
	if e == nil || ctx == nil || e.owner == nil {
		return ErrInvalidInput
	}
	if tty == nil {
		return ErrTerminalRequired
	}
	python, err := e.discoverPython(e.pythonExplicit)
	if err != nil {
		return mapEngineError(err)
	}
	lease, err := e.owner.Lease(ctx)
	if err != nil {
		return mapEngineError(err)
	}
	defer lease.Close()

	client, err := e.openBorrowed(lease.Context, lease.Root, python)
	if err != nil {
		return mapEngineError(err)
	}
	loginErr := client.Login(lease.Context, tty)
	closeErr := client.Close(context.Background())
	if loginErr != nil {
		return mapEngineError(loginErr)
	}
	if closeErr != nil {
		return mapEngineError(closeErr)
	}
	return nil
}

// Sync borrows a root lease and retrieves a complete validated library snapshot.
func (e *EngineAccount) Sync(ctx context.Context, operationID string, report func(SyncProgress)) ([]store.CatalogBook, error) {
	if e == nil || ctx == nil || e.owner == nil || operationID == "" {
		return nil, ErrInvalidInput
	}
	python, err := e.discoverPython(e.pythonExplicit)
	if err != nil {
		return nil, mapEngineError(err)
	}
	lease, err := e.owner.Lease(ctx)
	if err != nil {
		return nil, mapEngineError(err)
	}
	defer lease.Close()

	client, err := e.openBorrowed(lease.Context, lease.Root, python)
	if err != nil {
		return nil, mapEngineError(err)
	}
	var cb func(model.OperationEvent)
	if report != nil {
		cb = func(ev model.OperationEvent) {
			report(SyncProgress{
				OperationID: ev.OperationID,
				Stage:       ev.Stage,
				Count:       ev.Count,
			})
		}
	}
	snap, syncErr := client.Library(lease.Context, operationID, cb)
	closeErr := client.Close(context.Background())
	if syncErr != nil {
		return nil, mapEngineError(syncErr)
	}
	if closeErr != nil {
		return nil, mapEngineError(closeErr)
	}
	return convertCatalogBooks(snap.Books)
}

// Close satisfies AccountClient; per-operation borrowed clients are closed
// before releasing their root leases.
func (e *EngineAccount) Close(context.Context) error { return nil }

func convertCatalogBooks(books []model.Book) ([]store.CatalogBook, error) {
	out := make([]store.CatalogBook, 0, len(books))
	for _, book := range books {
		if err := book.Validate(); err != nil {
			return nil, &AccountFailure{Code: "protocol_error"}
		}
		var subtitle *string
		if book.Subtitle != nil {
			s := *book.Subtitle
			subtitle = &s
		}
		var durationNS *int64
		if book.Duration != nil {
			ns := int64(*book.Duration)
			durationNS = &ns
		}
		converted := store.CatalogBook{
			Key: store.BookKey{
				Provider:  book.ID.Provider,
				Namespace: book.ID.Namespace,
				ID:        book.ID.ID,
			},
			Title:      book.Title,
			Subtitle:   subtitle,
			Authors:    convertContributors(book.Authors),
			Narrators:  convertContributors(book.Narrators),
			DurationNS: durationNS,
		}
		if err := converted.Validate(); err != nil {
			return nil, &AccountFailure{Code: "protocol_error"}
		}
		out = append(out, converted)
	}
	return out, nil
}

func convertContributors(people []model.Contributor) []store.Contributor {
	if people == nil {
		return nil
	}
	out := make([]store.Contributor, len(people))
	for i, person := range people {
		out[i] = store.Contributor{Name: person.Name}
	}
	return out
}

func mapEngineError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, store.ErrClosed) {
		return ErrClosed
	}
	if errors.Is(err, audible.ErrTerminal) {
		return ErrTerminalRequired
	}
	if errors.Is(err, audible.ErrRuntimeMissing) {
		return &AccountFailure{Code: "runtime_missing"}
	}
	if errors.Is(err, audible.ErrRuntimeIncompatible) {
		return &AccountFailure{Code: "runtime_incompatible"}
	}
	if errors.Is(err, audible.ErrAuthBusy) {
		return &AccountFailure{Code: "busy"}
	}
	if errors.Is(err, audible.ErrAuthStorage) {
		return &AccountFailure{Code: "storage_failed"}
	}
	var loginErr *audible.LoginError
	if errors.As(err, &loginErr) {
		return &AccountFailure{Code: loginErr.Code}
	}
	var failure *model.Failure
	if errors.As(err, &failure) {
		return &AccountFailure{Code: failure.Code, Retry: failure.Retry}
	}
	return &AccountFailure{Code: "internal"}
}
