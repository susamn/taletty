// Package app implements Taletty's application session coordinator, cached
// boot, offline session restoration, and onboarding/account orchestration over
// narrow consumer-owned interfaces without importing the engine module.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/bookmarks"
	"github.com/susamn/taletty/app/internal/profile"
	"github.com/susamn/taletty/app/internal/store"
	"github.com/susamn/taletty/app/internal/ui"
)

const (
	defaultProvider  = "audible"
	defaultNamespace = "us"
)

var (
	// ErrInvalidInput indicates malformed coordinator arguments or book keys.
	ErrInvalidInput = errors.New("invalid application input")
	// ErrClosed indicates the coordinator has been closed or is closing.
	ErrClosed = errors.New("application coordinator closed")
	// ErrAccountBusy indicates another account operation is already running.
	ErrAccountBusy = errors.New("account operation already in progress")
	// ErrTerminalRequired indicates interactive login requires a TTY handoff.
	ErrTerminalRequired = errors.New("sign-in requires an owned terminal")
)

// CatalogStore persists cached catalog metadata, local state, and view state.
// *store.Store satisfies this interface directly.
type CatalogStore interface {
	Catalog(ctx context.Context, provider, namespace string) ([]store.CatalogBook, error)
	ReplaceCatalog(ctx context.Context, provider, namespace string, books []store.CatalogBook, refreshedAtMS int64) error
	SnapshotRefreshedAt(ctx context.Context, provider, namespace string) (int64, bool, error)
	ListeningState(ctx context.Context, key store.BookKey) (store.ListeningState, bool, error)
	ListeningStates(ctx context.Context) ([]store.ListeningState, error)
	Settings(ctx context.Context) (store.Settings, error)
	PutSettings(ctx context.Context, settings store.Settings) error
	ViewState(ctx context.Context) (store.ViewState, error)
	PutViewState(ctx context.Context, state store.ViewState) error
	Favorites(ctx context.Context) ([]store.BookKey, error)
	LocalMedia(ctx context.Context, key store.BookKey) (store.LocalMedia, bool, error)
	LocalMediaList(ctx context.Context) ([]store.LocalMedia, error)
	DeleteLocalMedia(ctx context.Context, key store.BookKey) error
	DownloadJobs(ctx context.Context) ([]store.DownloadJob, error)
}

var _ CatalogStore = (*store.Store)(nil)

// Diagnostics reports read-only runtime readiness without exposing paths or secrets.
type Diagnostics struct {
	Python  string
	Audible string
	FFmpeg  string
	FFprobe string
}

// Ready reports whether Python, Audible SDK, ffmpeg, and ffprobe are all ready.
func (d Diagnostics) Ready() bool {
	return d.Python == "ready" && d.Audible == "ready" && d.FFmpeg == "ready" && d.FFprobe == "ready"
}

// LoginReady reports whether Python and the Audible SDK are ready for login/sync.
func (d Diagnostics) LoginReady() bool {
	return d.Python == "ready" && d.Audible == "ready"
}

// AccountFailure carries a sanitized failure code and retry disposition.
type AccountFailure struct {
	Code    string
	Retry   string
	Message string
}

func (f *AccountFailure) Error() string {
	return sanitizeAccountFailure(f)
}

// SyncProgress reports provisional library sync progress.
type SyncProgress struct {
	OperationID string
	Stage       string
	Count       int
}

// AccountClient abstracts provider readiness, interactive login, and library sync.
type AccountClient interface {
	Doctor(ctx context.Context) (Diagnostics, error)
	Login(ctx context.Context, tty *os.File) error
	Sync(ctx context.Context, operationID string, report func(SyncProgress)) ([]store.CatalogBook, error)
	Close(ctx context.Context) error
}

// TerminalHandoff suspends any active TUI terminal user, lends the controlling
// foreground TTY to fn, and restores the TUI before returning.
type TerminalHandoff func(ctx context.Context, fn func(tty *os.File) error) error

// Options configures a Coordinator.
type Options struct {
	Provider        string
	Namespace       string
	Account         AccountClient
	Handoff         TerminalHandoff
	SuspendForLogin bool
	Player          PlaybackController
	MediaChecker    MediaChecker
	ChapterReader   ChapterReader
	Downloads       DownloadEnqueuer
	DownloadsMgr    DownloadManager
	RootOwner       RootLeaseOwner
	Bookmarks       BookmarkManager
	PreRoll         *time.Duration
	Clock           func() time.Time
	WorkspaceCloser func(context.Context) error
}

// EncodeBookKey encodes a validated BookKey into a stable UI selection key.
func EncodeBookKey(key store.BookKey) (string, error) {
	if err := key.Validate(); err != nil {
		return "", ErrInvalidInput
	}
	return key.Provider + ":" + key.Namespace + ":" + key.ID, nil
}

// DecodeBookKey decodes a UI selection key back into a validated BookKey.
func DecodeBookKey(encoded string) (store.BookKey, error) {
	parts := strings.Split(encoded, ":")
	if len(parts) != 3 {
		return store.BookKey{}, ErrInvalidInput
	}
	key := store.BookKey{Provider: parts[0], Namespace: parts[1], ID: parts[2]}
	if err := key.Validate(); err != nil {
		return store.BookKey{}, ErrInvalidInput
	}
	return key, nil
}

// Coordinator owns cached boot, offline session restoration, onboarding/account
// orchestration, and ViewState persistence for a Taletty session.
type Coordinator struct {
	store           CatalogStore
	provider        string
	namespace       string
	account         AccountClient
	handoff         TerminalHandoff
	suspendForLogin bool
	player          PlaybackController
	mediaChecker    MediaChecker
	chapterReader   ChapterReader
	downloads       DownloadEnqueuer
	downloadsMgr    DownloadManager
	rootOwner       RootLeaseOwner
	bookmarks       BookmarkManager
	preRoll         time.Duration
	clock           func() time.Time
	workspaceCloser func(context.Context) error

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu              sync.Mutex
	closed          bool
	downloadsDone   bool
	playerClosed    bool
	accountClosed   bool
	workspaceDone   bool
	accountBusy     bool
	pendingLogin    bool
	syncSeq         uint64
	dialogSeq       uint64
	activeSyncID    string
	speedJobID      string
	speedBytes      int64
	speedAt         time.Time
	viewState       store.ViewState
	settings        store.Settings
	durations       map[store.BookKey]time.Duration
	chapters        map[store.BookKey][]Chapter
	pendingDrafts   map[string]bookmarks.Draft
	draftOrder      []string
	committedDrafts map[string]store.Bookmark
	committedOrder  []string
	accountState    ui.AccountState
	snapshot        ui.SnapshotMsg
	appearance      ui.AppearanceMsg
	restore         ui.RestoreSessionMsg
}

// OpenWorkspace initializes the private ~/.config/taletty/ workspace for home,
// opens the validated SQLite store, and restores the cached session without
// starting playback or requiring Python/network access.
func OpenWorkspace(ctx context.Context, home string, opts Options) (*Coordinator, error) {
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
	opts.Clock = clock
	opts.WorkspaceCloser = ws.Close
	coord, err := NewCoordinator(ctx, ws.Store(), opts)
	if err != nil {
		_ = ws.Close(context.Background())
		return nil, err
	}
	return coord, nil
}

// NewCoordinator loads cached catalog, local state, settings, and view state
// from st without invoking AccountClient or starting playback.
func NewCoordinator(ctx context.Context, st CatalogStore, opts Options) (*Coordinator, error) {
	if ctx == nil || st == nil {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	provider := opts.Provider
	if provider == "" {
		provider = defaultProvider
	}
	namespace := opts.Namespace
	if namespace == "" {
		namespace = defaultNamespace
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}

	snapshot, appearance, restore, viewState, settings, accountState, durations, err := loadInitialState(ctx, st, provider, namespace, opts.Account != nil)
	if err != nil {
		return nil, err
	}

	preRoll := bookmarks.DefaultPreRoll
	if opts.PreRoll != nil {
		if *opts.PreRoll < 0 {
			return nil, ErrInvalidInput
		}
		preRoll = *opts.PreRoll
	}
	bmSvc := opts.Bookmarks
	if bmSvc == nil && opts.Player != nil {
		if bmStore, ok := st.(bookmarks.BookmarkStore); ok {
			svc, svcErr := bookmarks.NewService(bookmarks.Options{
				Store:        bmStore,
				Player:       opts.Player,
				MediaChecker: opts.MediaChecker,
			})
			if svcErr != nil {
				return nil, svcErr
			}
			bmSvc = svc
		}
	}

	lifeCtx, cancel := context.WithCancel(context.Background())
	return &Coordinator{
		store:           st,
		provider:        provider,
		namespace:       namespace,
		account:         opts.Account,
		handoff:         opts.Handoff,
		suspendForLogin: opts.SuspendForLogin,
		player:          opts.Player,
		mediaChecker:    opts.MediaChecker,
		chapterReader:   opts.ChapterReader,
		downloads:       opts.Downloads,
		downloadsMgr:    opts.DownloadsMgr,
		rootOwner:       opts.RootOwner,
		bookmarks:       bmSvc,
		preRoll:         preRoll,
		clock:           clock,
		workspaceCloser: opts.WorkspaceCloser,
		ctx:             lifeCtx,
		cancel:          cancel,
		viewState:       viewState,
		settings:        settings,
		durations:       durations,
		chapters:        make(map[store.BookKey][]Chapter),
		accountState:    accountState,
		snapshot:        snapshot,
		appearance:      appearance,
		restore:         restore,
	}, nil
}

func loadInitialState(ctx context.Context, st CatalogStore, provider, namespace string, hasAccount bool) (
	ui.SnapshotMsg, ui.AppearanceMsg, ui.RestoreSessionMsg, store.ViewState, store.Settings, ui.AccountState, map[store.BookKey]time.Duration, error,
) {
	catalog, err := st.Catalog(ctx, "", "")
	if err != nil {
		return ui.SnapshotMsg{}, ui.AppearanceMsg{}, ui.RestoreSessionMsg{}, store.ViewState{}, store.Settings{}, ui.AccountState{}, nil, err
	}
	_, hasSnapshot, err := st.SnapshotRefreshedAt(ctx, provider, namespace)
	if err != nil {
		return ui.SnapshotMsg{}, ui.AppearanceMsg{}, ui.RestoreSessionMsg{}, store.ViewState{}, store.Settings{}, ui.AccountState{}, nil, err
	}
	favorites, err := st.Favorites(ctx)
	if err != nil {
		return ui.SnapshotMsg{}, ui.AppearanceMsg{}, ui.RestoreSessionMsg{}, store.ViewState{}, store.Settings{}, ui.AccountState{}, nil, err
	}
	media, err := st.LocalMediaList(ctx)
	if err != nil {
		return ui.SnapshotMsg{}, ui.AppearanceMsg{}, ui.RestoreSessionMsg{}, store.ViewState{}, store.Settings{}, ui.AccountState{}, nil, err
	}
	jobs, err := st.DownloadJobs(ctx)
	if err != nil {
		return ui.SnapshotMsg{}, ui.AppearanceMsg{}, ui.RestoreSessionMsg{}, store.ViewState{}, store.Settings{}, ui.AccountState{}, nil, err
	}
	settings, err := st.Settings(ctx)
	if err != nil {
		return ui.SnapshotMsg{}, ui.AppearanceMsg{}, ui.RestoreSessionMsg{}, store.ViewState{}, store.Settings{}, ui.AccountState{}, nil, err
	}
	viewState, err := st.ViewState(ctx)
	if err != nil {
		return ui.SnapshotMsg{}, ui.AppearanceMsg{}, ui.RestoreSessionMsg{}, store.ViewState{}, store.Settings{}, ui.AccountState{}, nil, err
	}
	listeningStates, err := st.ListeningStates(ctx)
	if err != nil {
		return ui.SnapshotMsg{}, ui.AppearanceMsg{}, ui.RestoreSessionMsg{}, store.ViewState{}, store.Settings{}, ui.AccountState{}, nil, err
	}

	durations := make(map[store.BookKey]time.Duration, len(catalog))
	for _, item := range catalog {
		if item.DurationNS != nil && *item.DurationNS > 0 {
			durations[item.Key] = time.Duration(*item.DurationNS)
		}
	}
	books := projectBooks(catalog, favorites, media, jobs, listeningStates)
	player, err := projectRestoredPlayer(ctx, st, viewState, books, settings)
	if err != nil {
		return ui.SnapshotMsg{}, ui.AppearanceMsg{}, ui.RestoreSessionMsg{}, store.ViewState{}, store.Settings{}, ui.AccountState{}, nil, err
	}

	firstLaunch := !hasSnapshot && len(books) == 0
	accountState := initialAccountState(firstLaunch, hasAccount)
	activity := "Cached library loaded offline"
	if firstLaunch {
		activity = "Fresh Taletty root (~/.config/taletty/): sign in or continue offline"
	} else if !hasAccount {
		activity = "Offline mode: cached library ready"
	}

	snapshot := ui.SnapshotMsg{
		Books:    books,
		Player:   player,
		Activity: activity,
	}
	appearance := ui.AppearanceMsg{
		Glyphs: mapGlyphMode(settings.GlyphMode),
	}
	focus := mapFocusPane(viewState.Focus)
	view := mapLibraryView(viewState.Destination)
	sortOrder := mapSortOrder(viewState.SortOrder)
	filterText := viewState.FilterText
	selected := ""
	if viewState.Selected != nil {
		if encoded, encErr := EncodeBookKey(*viewState.Selected); encErr == nil {
			selected = encoded
		}
	}
	accountCopy := accountState
	restore := ui.RestoreSessionMsg{
		Focus:       &focus,
		View:        &view,
		Sort:        &sortOrder,
		Filter:      &filterText,
		Selected:    selected,
		OpenAccount: firstLaunch,
		Account:     &accountCopy,
	}
	return snapshot, appearance, restore, viewState, settings, accountState, durations, nil
}

func initialAccountState(firstLaunch, hasAccount bool) ui.AccountState {
	if firstLaunch {
		return ui.AccountState{
			Status:     "Not signed in",
			Detail:     "Fresh Taletty root (~/.config/taletty/). Sign in or continue offline.",
			Onboarding: true,
			Offline:    !hasAccount,
		}
	}
	if !hasAccount {
		return ui.AccountState{
			Status:  "Offline",
			Detail:  "Cached library ready; account runtime unavailable.",
			Offline: true,
		}
	}
	return ui.AccountState{
		Status: "Cached",
		Detail: "Cached library loaded offline; press a for account sync.",
	}
}

func projectBooks(catalog []store.CatalogBook, favorites []store.BookKey, media []store.LocalMedia, jobs []store.DownloadJob, listening []store.ListeningState) []ui.Book {
	favSet := make(map[store.BookKey]struct{}, len(favorites))
	for _, key := range favorites {
		favSet[key] = struct{}{}
	}
	mediaSet := make(map[store.BookKey]struct{}, len(media))
	for _, item := range media {
		mediaSet[item.Key] = struct{}{}
	}
	jobState := make(map[store.BookKey]store.JobState, len(jobs))
	for _, job := range jobs {
		// DownloadJobs returns newest/live jobs; keep live state over historical terminal rows.
		prev, exists := jobState[job.Key]
		if !exists || (!prev.IsLive() && job.State.IsLive()) {
			jobState[job.Key] = job.State
		}
	}
	listenMap := make(map[store.BookKey]store.ListeningState, len(listening))
	for _, ls := range listening {
		listenMap[ls.Key] = ls
	}

	out := make([]ui.Book, 0, len(catalog))
	for idx, item := range catalog {
		encoded, err := EncodeBookKey(item.Key)
		if err != nil {
			continue
		}
		author := "Unknown author"
		if len(item.Authors) > 0 {
			names := make([]string, 0, len(item.Authors))
			for _, person := range item.Authors {
				names = append(names, person.Name)
			}
			author = strings.Join(names, ", ")
		}
		narrator := ""
		if len(item.Narrators) > 0 {
			names := make([]string, 0, len(item.Narrators))
			for _, person := range item.Narrators {
				names = append(names, person.Name)
			}
			narrator = strings.Join(names, ", ")
		}
		subtitle := ""
		if item.Subtitle != nil {
			subtitle = *item.Subtitle
		}
		var dur time.Duration
		hasDur := false
		if item.DurationNS != nil {
			dur = time.Duration(*item.DurationNS)
			hasDur = true
		}
		avail := ui.NotDownloaded
		if _, ok := mediaSet[item.Key]; ok {
			avail = ui.Ready
		} else if state, ok := jobState[item.Key]; ok {
			avail = mapJobAvailability(state)
		}
		_, fav := favSet[item.Key]
		ls, hasLS := listenMap[item.Key]
		out = append(out, ui.Book{
			Key:          encoded,
			Title:        item.Title,
			Author:       author,
			Subtitle:     subtitle,
			Narrator:     narrator,
			Description:  subtitle,
			Duration:     dur,
			HasDuration:  hasDur,
			RecentAtMS:   ls.UpdatedAtMS,
			InProgress:   hasLS && ls.PositionNS > 0 && ls.Completion != store.CompletionFinished,
			Finished:     hasLS && ls.Completion == store.CompletionFinished,
			CatalogOrder: idx,
			Availability: avail,
			Favorite:     fav,
		})
	}
	return out
}

func projectRestoredPlayer(ctx context.Context, st CatalogStore, viewState store.ViewState, books []ui.Book, settings store.Settings) (ui.Player, error) {
	if viewState.Selected == nil {
		return ui.Player{}, nil
	}
	ls, found, err := st.ListeningState(ctx, *viewState.Selected)
	if err != nil {
		return ui.Player{}, err
	}
	if !found {
		return ui.Player{}, nil
	}
	encoded, err := EncodeBookKey(*viewState.Selected)
	if err != nil {
		return ui.Player{}, nil
	}
	title := "Unavailable title"
	for _, book := range books {
		if book.Key == encoded {
			title = book.Title
			break
		}
	}
	state := ui.Stopped
	if ls.Completion == store.CompletionFinished {
		state = ui.Finished
	}
	// Never autoplay on restore: Session stays empty until explicit Play/Resume.
	return ui.Player{
		Key:         encoded,
		Session:     "",
		Title:       title,
		State:       state,
		Position:    time.Duration(ls.PositionNS),
		Speed:       float64(ls.SpeedMilli) / 1_000_000.0,
		Volume:      settings.VolumePercent,
		VolumeKnown: true,
	}, nil
}

func mapJobAvailability(state store.JobState) ui.Availability {
	switch state {
	case store.JobWaiting:
		return ui.Queued
	case store.JobLicensing, store.JobDownloading:
		return ui.Downloading
	case store.JobPreparing, store.JobPublishing:
		return ui.Preparing
	case store.JobReady:
		return ui.Ready
	case store.JobFailed:
		return ui.Failed
	case store.JobCanceled:
		return ui.Canceled
	default:
		return ui.AvailabilityUnknown
	}
}

func mapGlyphMode(mode store.GlyphMode) ui.GlyphMode {
	if mode == store.GlyphPlain {
		return ui.Plain
	}
	return ui.Unicode
}

func mapFocusPane(focus store.FocusPane) ui.Pane {
	switch focus {
	case store.FocusLibrary:
		return ui.Library
	case store.FocusDetails:
		return ui.Details
	case store.FocusChapters:
		return ui.Chapters
	case store.FocusPlayer:
		return ui.PlayerPane
	default:
		return ui.Books
	}
}

func toStoreFocus(focus ui.Pane) store.FocusPane {
	switch focus {
	case ui.Library:
		return store.FocusLibrary
	case ui.Details:
		return store.FocusDetails
	case ui.Chapters:
		return store.FocusChapters
	case ui.PlayerPane:
		return store.FocusPlayer
	default:
		return store.FocusBooks
	}
}

func mapLibraryView(dest store.Destination) ui.LibraryView {
	switch dest {
	case store.DestinationContinue:
		return ui.ViewContinue
	case store.DestinationDownloaded:
		return ui.ViewDownloaded
	case store.DestinationFavorites:
		return ui.ViewFavorites
	case store.DestinationFinished:
		return ui.ViewFinished
	default:
		return ui.ViewAll
	}
}

func toStoreDestination(view ui.LibraryView) store.Destination {
	switch view {
	case ui.ViewContinue:
		return store.DestinationContinue
	case ui.ViewDownloaded:
		return store.DestinationDownloaded
	case ui.ViewFavorites:
		return store.DestinationFavorites
	case ui.ViewFinished:
		return store.DestinationFinished
	default:
		return store.DestinationAll
	}
}

func mapSortOrder(order store.SortOrder) ui.SortOrder {
	switch order {
	case store.SortAuthor:
		return ui.SortAuthor
	case store.SortDuration:
		return ui.SortDuration
	case store.SortRecent:
		return ui.SortRecent
	case store.SortAcquisition:
		return ui.SortAcquisition
	default:
		return ui.SortTitle
	}
}

func toStoreSortOrder(order ui.SortOrder) store.SortOrder {
	switch order {
	case ui.SortAuthor:
		return store.SortAuthor
	case ui.SortDuration:
		return store.SortDuration
	case ui.SortRecent:
		return store.SortRecent
	case ui.SortAcquisition:
		return store.SortAcquisition
	default:
		return store.SortTitle
	}
}

// Snapshot returns the latest projected UI snapshot.
func (c *Coordinator) Snapshot() ui.SnapshotMsg {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshot
}

// InitialSnapshot returns the initial cached UI snapshot (never autoplaying).
func (c *Coordinator) InitialSnapshot() ui.SnapshotMsg {
	return c.Snapshot()
}

// InitialAppearance returns the initial persisted glyph/palette appearance.
func (c *Coordinator) InitialAppearance() ui.AppearanceMsg {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.appearance
}

// InitialRestore returns the initial session restoration message.
func (c *Coordinator) InitialRestore() ui.RestoreSessionMsg {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.restore
}

// InitialMessages returns the startup messages to apply after ui.NewModel.
func (c *Coordinator) InitialMessages() []tea.Msg {
	return []tea.Msg{c.InitialAppearance(), c.InitialRestore()}
}

// AccountState returns the current sanitized account state.
func (c *Coordinator) AccountState() ui.AccountState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.accountState
}

// Dispatch translates UI actions into asynchronous Bubble Tea commands.
func (c *Coordinator) Dispatch(action ui.Action) tea.Cmd {
	if c == nil {
		return nil
	}
	switch action.Kind {
	case ui.RequestQuit:
		return tea.Quit
	case ui.AccountContinueOffline:
		return func() tea.Msg {
			return c.continueOffline()
		}
	case ui.AccountDoctor:
		return func() tea.Msg {
			return c.runDoctor()
		}
	case ui.AccountLogin:
		if c.suspendForLogin {
			c.mu.Lock()
			canSuspend := !c.closed && !c.accountBusy
			if canSuspend {
				c.pendingLogin = true
			}
			c.mu.Unlock()
			if canSuspend {
				return tea.Quit
			}
			return func() tea.Msg {
				return c.busyOrClosedMsg()
			}
		}
		return func() tea.Msg {
			return c.runLogin()
		}
	case ui.AccountSync:
		return func() tea.Msg {
			return c.runSync()
		}
	case ui.PlayBook, ui.ActivateChapter, ui.ConfirmSwitchPlay,
		ui.TogglePlayback, ui.SeekBackward, ui.SeekForward,
		ui.SpeedDown, ui.SpeedUp, ui.VolumeDown, ui.VolumeUp:
		return func() tea.Msg {
			return c.handlePlaybackAction(action)
		}
	case ui.QueueDownload, ui.DownloadCancel, ui.DownloadRetry, ui.DownloadRemove,
		ui.DownloadMoveUp, ui.DownloadMoveDown, ui.DownloadDeleteAudio:
		return func() tea.Msg {
			return c.handleDownloadAction(action)
		}
	case ui.CaptureBookmark, ui.SaveNote, ui.OpenBookmarks,
		ui.DeleteBookmark, ui.JumpBookmark, ui.ConfirmSwitchBookmark:
		return func() tea.Msg {
			return c.handleBookmarkAction(action)
		}
	case ui.ToggleFavorite:
		return func() tea.Msg {
			return c.toggleFavorite(action.Key)
		}
	default:
		return func() tea.Msg {
			return ui.NoticeMsg{Activity: "Action unavailable in this slice"}
		}
	}
}

type favoriteWriter interface {
	SetFavorite(ctx context.Context, key store.BookKey, favored bool, createdAtMS int64) error
}

func (c *Coordinator) toggleFavorite(encodedKey string) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.NoticeMsg{Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	key, err := DecodeBookKey(encodedKey)
	if err != nil {
		return ui.NoticeMsg{Error: SanitizeError(ErrInvalidInput)}
	}
	writer, ok := c.store.(favoriteWriter)
	if !ok {
		return ui.NoticeMsg{Error: "Favorites persistence unavailable."}
	}
	favs, err := c.store.Favorites(ctx)
	if err != nil {
		return ui.NoticeMsg{Error: SanitizeError(err)}
	}
	nextFav := !slices.Contains(favs, key)
	nowMS := max(int64(0), c.clock().UnixMilli())
	if err := writer.SetFavorite(ctx, key, nextFav, nowMS); err != nil {
		return ui.NoticeMsg{Error: SanitizeError(err)}
	}

	activity := "Removed from Favorites"
	if nextFav {
		activity = "Added to Favorites"
	}
	c.mu.Lock()
	for i := range c.snapshot.Books {
		if c.snapshot.Books[i].Key == encodedKey {
			c.snapshot.Books[i].Favorite = nextFav
			break
		}
	}
	c.snapshot.Activity = activity
	out := c.snapshot
	c.mu.Unlock()
	return out
}

func (c *Coordinator) continueOffline() tea.Msg {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accountState.Onboarding = false
	if !c.accountState.SignedIn {
		c.accountState.Offline = true
		c.accountState.Status = "Offline"
		c.accountState.Detail = "Browsing cached library offline."
	}
	c.accountState.Error = ""
	state := c.accountState
	return tea.BatchMsg{
		func() tea.Msg { return ui.AccountStatusMsg{Account: state} },
		func() tea.Msg { return ui.NoticeMsg{Activity: "Continuing offline with cached library"} },
	}
}

func (c *Coordinator) beginAccountOp(isSync bool) (context.Context, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.accountBusy {
		return nil, "", false
	}
	c.accountBusy = true
	c.accountState.Busy = true
	opID := ""
	if isSync {
		c.syncSeq++
		opID = fmt.Sprintf("sync-%d", c.syncSeq)
		c.activeSyncID = opID
	}
	c.wg.Add(1)
	return c.ctx, opID, true
}

func (c *Coordinator) finishAccountState(update func(*ui.AccountState)) ui.AccountState {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accountBusy = false
	c.accountState.Busy = false
	if update != nil {
		update(&c.accountState)
	}
	return c.accountState
}

func (c *Coordinator) runDoctor() tea.Msg {
	ctx, _, ok := c.beginAccountOp(false)
	if !ok {
		return c.busyOrClosedMsg()
	}
	defer c.wg.Done()

	if c.account == nil {
		return c.accountErrorMsg("Offline", "Account runtime unavailable.", errors.New("runtime_missing"))
	}
	diag, err := c.account.Doctor(ctx)
	if err != nil {
		return c.accountErrorMsg("", "Runtime check failed.", err)
	}
	detail := fmt.Sprintf("Python: %s | Audible: %s | ffmpeg: %s | ffprobe: %s",
		diag.Python, diag.Audible, diag.FFmpeg, diag.FFprobe)
	status := "Runtime incomplete"
	if diag.Ready() {
		status = "Runtime ready"
	} else if diag.LoginReady() {
		status = "Login/sync ready (media tools missing)"
	}
	state := c.finishAccountState(func(s *ui.AccountState) {
		s.Status = status
		s.Detail = detail
		s.Error = ""
	})
	return tea.BatchMsg{
		func() tea.Msg { return ui.AccountStatusMsg{Account: state} },
		func() tea.Msg { return ui.NoticeMsg{Activity: status} },
	}
}

// TakePendingLogin reports and clears whether the UI requested a terminal
// suspension for interactive login.
func (c *Coordinator) TakePendingLogin() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	pending := c.pendingLogin
	c.pendingLogin = false
	return pending
}

// ExecutePendingLogin runs interactive login on an already-suspended controlling
// TTY and updates AccountState with the sanitized outcome.
func (c *Coordinator) ExecutePendingLogin(ctx context.Context, tty *os.File) {
	if ctx == nil {
		ctx = context.Background()
	}
	opCtx, _, ok := c.beginAccountOp(false)
	if !ok {
		_ = c.busyOrClosedMsg()
		return
	}
	defer c.wg.Done()

	if c.account == nil {
		_ = c.accountErrorMsg("Offline", "Account runtime unavailable.", errors.New("runtime_missing"))
		return
	}
	if tty == nil {
		_ = c.accountErrorMsg("", "Sign-in requires an owned terminal.", ErrTerminalRequired)
		return
	}
	runCtx, cancel := context.WithCancel(opCtx)
	stop := context.AfterFunc(ctx, cancel)
	defer func() {
		stop()
		cancel()
	}()
	if err := c.account.Login(runCtx, tty); err != nil {
		_ = c.accountErrorMsg("", "Sign-in did not complete.", err)
		return
	}
	c.finishAccountState(func(s *ui.AccountState) {
		s.SignedIn = true
		s.Onboarding = false
		s.Offline = false
		s.Status = "Signed in"
		s.Detail = "Signed in; select Sync library to refresh catalog."
		s.Error = ""
	})
}

func (c *Coordinator) runLogin() tea.Msg {
	ctx, _, ok := c.beginAccountOp(false)
	if !ok {
		return c.busyOrClosedMsg()
	}
	defer c.wg.Done()

	if c.account == nil {
		return c.accountErrorMsg("Offline", "Account runtime unavailable.", errors.New("runtime_missing"))
	}
	if c.handoff == nil {
		return c.accountErrorMsg("", "Sign-in requires an owned terminal.", ErrTerminalRequired)
	}
	err := c.handoff(ctx, func(tty *os.File) error {
		return c.account.Login(ctx, tty)
	})
	if err != nil {
		return c.accountErrorMsg("", "Sign-in did not complete.", err)
	}
	state := c.finishAccountState(func(s *ui.AccountState) {
		s.SignedIn = true
		s.Onboarding = false
		s.Offline = false
		s.Status = "Signed in"
		s.Detail = "Signed in; select Sync library to refresh catalog."
		s.Error = ""
	})
	return tea.BatchMsg{
		func() tea.Msg { return ui.AccountStatusMsg{Account: state} },
		func() tea.Msg { return ui.NoticeMsg{Activity: "Signed in to Audible"} },
	}
}

func (c *Coordinator) runSync() tea.Msg {
	ctx, opID, ok := c.beginAccountOp(true)
	if !ok {
		return c.busyOrClosedMsg()
	}
	defer c.wg.Done()

	if c.account == nil {
		return c.accountErrorMsg("Offline", "Account runtime unavailable; cached library preserved.", errors.New("runtime_missing"))
	}
	books, err := c.account.Sync(ctx, opID, nil)
	if err != nil {
		return c.accountErrorMsg("", "Library sync failed; cached library preserved.", err)
	}
	return c.commitSync(ctx, opID, books)
}

// ApplySyncResult validates opID against the active sync operation and commits
// books atomically only when opID is current and ctx is uncanceled.
func (c *Coordinator) ApplySyncResult(ctx context.Context, opID string, books []store.CatalogBook) tea.Msg {
	return c.commitSync(ctx, opID, books)
}

func (c *Coordinator) commitSync(ctx context.Context, opID string, books []store.CatalogBook) tea.Msg {
	if ctx == nil || ctx.Err() != nil {
		return c.accountErrorMsg("", "Library sync canceled; cached library preserved.", context.Canceled)
	}
	c.mu.Lock()
	stale := c.closed || opID == "" || opID != c.activeSyncID
	provider, namespace := c.provider, c.namespace
	c.mu.Unlock()
	if stale {
		c.finishAccountState(nil)
		return ui.NoticeMsg{Activity: "Ignored stale library sync reply"}
	}

	nowMS := max(int64(0), c.clock().UnixMilli())
	if err := c.store.ReplaceCatalog(ctx, provider, namespace, books, nowMS); err != nil {
		return c.accountErrorMsg("", "Failed to save synced library; previous cache preserved.", err)
	}

	snapshot, _, _, viewState, settings, _, durations, err := loadInitialState(ctx, c.store, provider, namespace, true)
	if err != nil {
		return c.accountErrorMsg("", "Failed to reload synced library.", err)
	}
	snapshot.Activity = fmt.Sprintf("Library synced: %d books", len(snapshot.Books))

	c.mu.Lock()
	c.accountBusy = false
	c.viewState = viewState
	c.settings = settings
	c.durations = durations
	if c.player != nil {
		if active := c.player.Snapshot(); active.Book != nil {
			snapshot.Player = projectActivePlayer(active, c.chapters[*active.Book])
		}
	}
	for i := range snapshot.Books {
		if key, decErr := DecodeBookKey(snapshot.Books[i].Key); decErr == nil {
			if chs, ok := c.chapters[key]; ok && len(chs) > 0 {
				titles, starts := splitChapters(chs)
				snapshot.Books[i].Chapters = titles
				snapshot.Books[i].ChapterStarts = starts
			}
		}
	}
	c.snapshot = snapshot
	c.accountState.Busy = false
	c.accountState.SignedIn = true
	c.accountState.Onboarding = false
	c.accountState.Offline = false
	c.accountState.Status = "Signed in"
	c.accountState.Detail = snapshot.Activity
	c.accountState.Error = ""
	state := c.accountState
	c.mu.Unlock()

	return tea.BatchMsg{
		func() tea.Msg { return snapshot },
		func() tea.Msg { return ui.AccountStatusMsg{Account: state} },
	}
}

func (c *Coordinator) busyOrClosedMsg() tea.Msg {
	c.mu.Lock()
	closed := c.closed
	if !closed {
		c.accountState.Error = SanitizeError(ErrAccountBusy)
	}
	state := c.accountState
	c.mu.Unlock()
	if closed {
		return ui.NoticeMsg{Error: SanitizeError(ErrClosed)}
	}
	return tea.BatchMsg{
		func() tea.Msg { return ui.AccountStatusMsg{Account: state} },
		func() tea.Msg { return ui.NoticeMsg{Error: state.Error} },
	}
}

func (c *Coordinator) accountErrorMsg(statusOverride, detail string, err error) tea.Msg {
	msg := SanitizeError(err)
	state := c.finishAccountState(func(s *ui.AccountState) {
		if statusOverride != "" {
			s.Status = statusOverride
		}
		if detail != "" {
			s.Detail = detail
		}
		s.Error = msg
	})
	return tea.BatchMsg{
		func() tea.Msg { return ui.AccountStatusMsg{Account: state} },
		func() tea.Msg { return ui.NoticeMsg{Error: msg} },
	}
}

// SaveViewState persists the current UI focus and selected BookKey without
// altering destination, filter, or sort order.
func (c *Coordinator) SaveViewState(ctx context.Context, focus ui.Pane, selectedKey string) error {
	if c == nil || ctx == nil {
		return ErrInvalidInput
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	next := c.viewState
	c.mu.Unlock()
	return c.SaveFullViewState(ctx, mapLibraryView(next.Destination), mapSortOrder(next.SortOrder), next.FilterText, focus, selectedKey)
}

// SaveFullViewState persists destination view, sort order, filter text, focus,
// and selected BookKey.
func (c *Coordinator) SaveFullViewState(ctx context.Context, view ui.LibraryView, sort ui.SortOrder, filter string, focus ui.Pane, selectedKey string) error {
	if c == nil || ctx == nil {
		return ErrInvalidInput
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	next := c.viewState
	c.mu.Unlock()

	next.Destination = toStoreDestination(view)
	next.SortOrder = toStoreSortOrder(sort)
	next.FilterText = filter
	next.Focus = toStoreFocus(focus)
	if selectedKey == "" {
		next.Selected = nil
	} else if key, err := DecodeBookKey(selectedKey); err == nil {
		next.Selected = &key
	} else {
		return ErrInvalidInput
	}
	if err := c.store.PutViewState(ctx, next); err != nil {
		return err
	}
	c.mu.Lock()
	c.viewState = next
	c.mu.Unlock()
	return nil
}

// Close cancels active account/sync workers, waits for them to join, and closes
// AccountClient and the owned Workspace. If ctx expires while waiting, Close
// returns ctx.Err() without closing underlying resources underneath active work.
func (c *Coordinator) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		return ErrInvalidInput
	}
	c.mu.Lock()
	c.closed = true
	c.cancel()
	c.mu.Unlock()

	joined := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(joined)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-joined:
	}

	c.mu.Lock()
	needDownloadsClose := c.downloadsMgr != nil && !c.downloadsDone
	c.mu.Unlock()
	if needDownloadsClose {
		if err := c.downloadsMgr.Close(ctx); err != nil {
			return err
		}
		c.mu.Lock()
		c.downloadsDone = true
		c.mu.Unlock()
	}

	c.mu.Lock()
	needPlayerClose := c.player != nil && !c.playerClosed
	c.mu.Unlock()
	if needPlayerClose {
		if err := c.player.Close(ctx); err != nil {
			return err
		}
		c.mu.Lock()
		c.playerClosed = true
		c.mu.Unlock()
	}

	c.mu.Lock()
	needAccountClose := c.account != nil && !c.accountClosed
	c.mu.Unlock()
	if needAccountClose {
		if err := c.account.Close(ctx); err != nil {
			return err
		}
		c.mu.Lock()
		c.accountClosed = true
		c.mu.Unlock()
	}

	c.mu.Lock()
	needWorkspaceClose := c.workspaceCloser != nil && !c.workspaceDone
	c.mu.Unlock()
	if needWorkspaceClose {
		if err := c.workspaceCloser(ctx); err != nil {
			return err
		}
		c.mu.Lock()
		c.workspaceDone = true
		c.mu.Unlock()
	}
	return nil
}

// SanitizeError maps internal/provider errors to fixed safe user-facing text.
func SanitizeError(err error) string {
	if err == nil {
		return ""
	}
	var failure *AccountFailure
	if errors.As(err, &failure) {
		return sanitizeAccountFailure(failure)
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "Operation canceled."
	case errors.Is(err, context.DeadlineExceeded):
		return "Operation timed out."
	case errors.Is(err, ErrAccountBusy), errors.Is(err, profile.ErrBusy):
		return "Another Taletty session or account operation is already active."
	case errors.Is(err, profile.ErrUnsafeRoot):
		return "Profile directory ~/.config/taletty/ has unsafe permissions or ownership."
	case errors.Is(err, profile.ErrInvalidHome):
		return "Home directory is invalid."
	case errors.Is(err, profile.ErrIO):
		return "Profile root initialization failed."
	case errors.Is(err, ErrTerminalRequired):
		return "Sign-in requires an owned terminal."
	case errors.Is(err, ErrClosed), errors.Is(err, store.ErrClosed):
		return "Application session is closed."
	case errors.Is(err, ErrInvalidInput), errors.Is(err, store.ErrInvalidInput):
		return "Invalid application or library data."
	case strings.Contains(err.Error(), "runtime_missing"):
		return "Python 3.12 or Audible runtime is missing."
	default:
		return "Operation failed."
	}
}

func sanitizeAccountFailure(f *AccountFailure) string {
	if f == nil {
		return "Account operation failed."
	}
	switch f.Code {
	case "busy":
		return "Another account operation is already active."
	case "not_authenticated":
		return "Not signed in to Audible."
	case "runtime_missing":
		return "Python 3.12 or Audible runtime is missing."
	case "runtime_incompatible":
		return "Python or Audible runtime is incompatible."
	case "network":
		return "Network error while contacting Audible."
	case "throttled":
		return "Audible rate limit reached; wait before retrying."
	case "entitlement_denied":
		return "Audible denied access for this request."
	case "storage_failed":
		return "Credentials could not be saved."
	case "storage_uncertain":
		return "Credentials published; durability not confirmed."
	case "protocol_error":
		return "Unexpected response from Audible bridge."
	case "canceled":
		return "Operation canceled."
	default:
		return "Account operation failed."
	}
}
