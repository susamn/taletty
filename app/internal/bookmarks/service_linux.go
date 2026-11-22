// Package bookmarks implements Taletty's persistent timestamped bookmark
// service and explicit targeting policy without terminal UI dependencies.
package bookmarks

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/susamn/taletty/app/internal/playback"
	"github.com/susamn/taletty/app/internal/store"
	"golang.org/x/sys/unix"
)

// DefaultPreRoll is the default capture pre-roll applied once at capture time.
const DefaultPreRoll = 2 * time.Second

var (
	// ErrInvalidInput reports malformed bookmark arguments, notes, or metadata.
	ErrInvalidInput = errors.New("invalid bookmark input")
	// ErrNoLoadedAudio reports that no active playing or paused audio session is loaded.
	ErrNoLoadedAudio = errors.New("no loaded audio session")
	// ErrStaleSession reports that playback changed or stopped since capture or confirmation.
	ErrStaleSession = errors.New("stale playback session")
	// ErrBookmarkNotFound reports that the target bookmark does not exist for the named book.
	ErrBookmarkNotFound = errors.New("bookmark not found")
	// ErrMediaMissing reports that local audio for a cross-book jump is absent or unsafe.
	ErrMediaMissing = errors.New("local audio file missing or unsafe")
)

// BookmarkStore persists bookmarks and local media inventory.
// *store.Store satisfies this interface directly.
type BookmarkStore interface {
	Bookmarks(ctx context.Context, key store.BookKey) ([]store.Bookmark, error)
	PutBookmark(ctx context.Context, b store.Bookmark) error
	DeleteBookmark(ctx context.Context, key store.BookKey, bookmarkID string) error
	LocalMedia(ctx context.Context, key store.BookKey) (store.LocalMedia, bool, error)
}

// PlaybackController exposes authoritative playback state and seek/load operations.
// *playback.Controller satisfies this interface directly.
type PlaybackController interface {
	Snapshot() playback.Snapshot
	Load(ctx context.Context, track playback.Track, autoPlay bool) (playback.Snapshot, error)
	SeekAbsolute(ctx context.Context, session uint64, target time.Duration) (playback.Snapshot, error)
}

// RootOwner lends an anchored session-lifetime root directory lease.
// *store.Owner and store.RootOwner satisfy this interface directly.
type RootOwner interface {
	Lease(ctx context.Context) (*store.RootLease, error)
}

// MediaChecker verifies that a stored LocalMedia record refers to an existing
// private regular audio file under downloads/ and returns its clean absolute path.
type MediaChecker interface {
	VerifyMedia(ctx context.Context, media store.LocalMedia) (string, error)
}

var (
	_ BookmarkStore      = (*store.Store)(nil)
	_ PlaybackController = (*playback.Controller)(nil)
	_ RootOwner          = (*store.Owner)(nil)
	_ RootOwner          = (store.RootOwner)(nil)
)

// ChapterRef identifies optional chapter metadata at a playback position.
type ChapterRef struct {
	ID    string
	Title string
}

func (c ChapterRef) empty() bool {
	return c.ID == "" && c.Title == ""
}

func (c ChapterRef) validate() error {
	if !safeSingleLine(c.ID) || !safeSingleLine(c.Title) {
		return ErrInvalidInput
	}
	return nil
}

// ChapterLookup resolves optional chapter metadata for key at position.
type ChapterLookup func(key store.BookKey, position time.Duration) (ChapterRef, bool)

// Draft holds a frozen bookmark capture created at action invocation time.
// Delayed saves, playback advances, speed changes, or book switches never
// retarget or recompute a Draft.
type Draft struct {
	BookKey          store.BookKey
	Session          uint64
	Title            string
	CapturedPosition time.Duration
	PreRoll          time.Duration
	JumpPosition     time.Duration
	CapturedChapter  *ChapterRef
	JumpChapter      *ChapterRef
}

// CrossedChapterBoundary reports whether pre-roll moved JumpPosition into a
// different chapter context than CapturedPosition.
func (d Draft) CrossedChapterBoundary() bool {
	if d.CapturedChapter == nil && d.JumpChapter == nil {
		return false
	}
	if d.CapturedChapter == nil || d.JumpChapter == nil {
		return true
	}
	return *d.CapturedChapter != *d.JumpChapter
}

// ChapterID returns a copy of the jump chapter ID, if present.
func (d Draft) ChapterID() *string {
	if d.JumpChapter == nil || d.JumpChapter.ID == "" {
		return nil
	}
	v := d.JumpChapter.ID
	return &v
}

// ChapterTitle returns a copy of the jump chapter title, if present.
func (d Draft) ChapterTitle() *string {
	if d.JumpChapter == nil || d.JumpChapter.Title == "" {
		return nil
	}
	v := d.JumpChapter.Title
	return &v
}

// ComputeJumpPosition applies preRoll once to captured, clamping at zero.
func ComputeJumpPosition(captured, preRoll time.Duration) (time.Duration, error) {
	if captured < 0 || preRoll < 0 {
		return 0, ErrInvalidInput
	}
	return clampJumpPosition(captured, preRoll), nil
}

func clampJumpPosition(captured, preRoll time.Duration) time.Duration {
	if preRoll >= captured {
		return 0
	}
	return captured - preRoll
}

// Options configures a bookmark Service.
type Options struct {
	Store        BookmarkStore
	Player       PlaybackController
	RootOwner    RootOwner
	MediaChecker MediaChecker
	NewID        func() (string, error)
}

// Service coordinates bookmark capture, persistence, offline listing, editing,
// deletion, and explicit same-book or cross-book jump targeting.
type Service struct {
	store        BookmarkStore
	player       PlaybackController
	mediaChecker MediaChecker
	newID        func() (string, error)
	editToken    chan struct{}
}

// NewService constructs a bookmark Service around opts.Store and opts.Player.
func NewService(opts Options) (*Service, error) {
	if opts.Store == nil || opts.Player == nil {
		return nil, ErrInvalidInput
	}
	checker := opts.MediaChecker
	if checker == nil && opts.RootOwner != nil {
		checker = &rootMediaChecker{owner: opts.RootOwner, ops: defaultSysOps()}
	}
	newID := opts.NewID
	if newID == nil {
		newID = defaultBookmarkID
	}
	token := make(chan struct{}, 1)
	token <- struct{}{}
	return &Service{
		store:        opts.Store,
		player:       opts.Player,
		mediaChecker: checker,
		newID:        newID,
		editToken:    token,
	}, nil
}

// CaptureDefaultDraft captures the loaded current audio session using DefaultPreRoll (2s).
func (s *Service) CaptureDefaultDraft(ctx context.Context, chapterAt ChapterLookup) (Draft, error) {
	return s.CaptureDraft(ctx, DefaultPreRoll, chapterAt)
}

// CaptureDraft freezes the loaded current audio session (playing or paused) at
// invocation time, applying preRoll once clamped at zero. It never falls back
// to a highlighted library book when no audio is loaded.
func (s *Service) CaptureDraft(ctx context.Context, preRoll time.Duration, chapterAt ChapterLookup) (Draft, error) {
	if s == nil || s.player == nil || ctx == nil || preRoll < 0 {
		return Draft{}, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return Draft{}, err
	}

	snap := s.player.Snapshot()
	if (snap.State != playback.StatePlaying && snap.State != playback.StatePaused) ||
		snap.Book == nil || snap.Session == 0 {
		return Draft{}, ErrNoLoadedAudio
	}
	key := *snap.Book
	if err := key.Validate(); err != nil {
		return Draft{}, ErrInvalidInput
	}

	capturedPos := max(0, snap.Position)
	jumpPos := clampJumpPosition(capturedPos, preRoll)

	var capturedChapter, jumpChapter *ChapterRef
	if chapterAt != nil {
		if ref, ok := chapterAt(key, capturedPos); ok && !ref.empty() {
			if err := ref.validate(); err != nil {
				return Draft{}, err
			}
			capturedChapter = &ChapterRef{ID: ref.ID, Title: ref.Title}
		}
		if jumpPos == capturedPos {
			if capturedChapter != nil {
				jumpChapter = &ChapterRef{ID: capturedChapter.ID, Title: capturedChapter.Title}
			}
		} else if ref, ok := chapterAt(key, jumpPos); ok && !ref.empty() {
			if err := ref.validate(); err != nil {
				return Draft{}, err
			}
			jumpChapter = &ChapterRef{ID: ref.ID, Title: ref.Title}
		}
	}

	return Draft{
		BookKey:          key,
		Session:          snap.Session,
		Title:            snap.Title,
		CapturedPosition: capturedPos,
		PreRoll:          preRoll,
		JumpPosition:     jumpPos,
		CapturedChapter:  capturedChapter,
		JumpChapter:      jumpChapter,
	}, nil
}

// SaveNew validates note and persists a new bookmark using draft's frozen
// BookKey, JumpPosition, and jump chapter context without re-reading playback.
// On store failure, nothing is persisted and the caller retains draft intact.
func (s *Service) SaveNew(ctx context.Context, draft Draft, note string, nowMS int64) (store.Bookmark, error) {
	if s == nil || s.store == nil || ctx == nil || nowMS < 0 {
		return store.Bookmark{}, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return store.Bookmark{}, err
	}
	if err := validateDraft(draft); err != nil {
		return store.Bookmark{}, err
	}
	if err := validateNote(note); err != nil {
		return store.Bookmark{}, err
	}

	id, err := s.newID()
	if err != nil {
		return store.Bookmark{}, err
	}
	if !safeID(id) {
		return store.Bookmark{}, ErrInvalidInput
	}

	bm := store.Bookmark{
		ID:           id,
		Key:          draft.BookKey,
		PositionNS:   draft.JumpPosition.Nanoseconds(),
		Note:         note,
		ChapterID:    draft.ChapterID(),
		ChapterTitle: draft.ChapterTitle(),
		CreatedAtMS:  nowMS,
		UpdatedAtMS:  nowMS,
	}
	if err := s.store.PutBookmark(ctx, bm); err != nil {
		return store.Bookmark{}, err
	}
	return bm, nil
}

// EditNote updates only Note and UpdatedAtMS on an existing bookmark for key.
// It never recaptures playback position or reapplies pre-roll, and rejects
// missing or wrong-book bookmark IDs with ErrBookmarkNotFound.
func (s *Service) EditNote(ctx context.Context, key store.BookKey, bookmarkID, note string, nowMS int64) (store.Bookmark, error) {
	if s == nil || s.store == nil || ctx == nil || nowMS < 0 {
		return store.Bookmark{}, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return store.Bookmark{}, err
	}
	if key.Validate() != nil || !safeID(bookmarkID) {
		return store.Bookmark{}, ErrInvalidInput
	}
	if err := validateNote(note); err != nil {
		return store.Bookmark{}, err
	}

	if err := s.acquireEditToken(ctx); err != nil {
		return store.Bookmark{}, err
	}
	defer s.releaseEditToken()

	items, err := s.store.Bookmarks(ctx, key)
	if err != nil {
		return store.Bookmark{}, err
	}
	idx := slices.IndexFunc(items, func(b store.Bookmark) bool { return b.ID == bookmarkID })
	if idx < 0 {
		return store.Bookmark{}, ErrBookmarkNotFound
	}

	updated := items[idx]
	if nowMS < updated.CreatedAtMS {
		return store.Bookmark{}, ErrInvalidInput
	}
	updated.Note = note
	updated.UpdatedAtMS = nowMS
	if err := s.store.PutBookmark(ctx, updated); err != nil {
		return store.Bookmark{}, err
	}
	return updated, nil
}

// Delete removes only bookmarkID for key, preserving audio, history, favorites,
// and bookmarks belonging to other books.
func (s *Service) Delete(ctx context.Context, key store.BookKey, bookmarkID string) error {
	if s == nil || s.store == nil || ctx == nil {
		return ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if key.Validate() != nil || !safeID(bookmarkID) {
		return ErrInvalidInput
	}

	if err := s.acquireEditToken(ctx); err != nil {
		return err
	}
	defer s.releaseEditToken()

	return s.store.DeleteBookmark(ctx, key, bookmarkID)
}

// List returns all bookmarks for key offline in ascending PositionNS order,
// tie-broken by bookmark ID.
func (s *Service) List(ctx context.Context, key store.BookKey) ([]store.Bookmark, error) {
	if s == nil || s.store == nil || ctx == nil {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key.Validate() != nil {
		return nil, ErrInvalidInput
	}

	items, err := s.store.Bookmarks(ctx, key)
	if err != nil {
		return nil, err
	}
	out := cloneBookmarks(items)
	slices.SortFunc(out, func(a, b store.Bookmark) int {
		if c := cmp.Compare(a.PositionNS, b.PositionNS); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out, nil
}

// Jump activates bookmark at its stored absolute PositionNS without applying
// pre-roll or smart rewind again.
//
// For the same loaded book, Jump verifies active playback and expectedSession,
// then calls SeekAbsolute (preserving playing or paused state).
//
// For a different book (or when no book is loaded), Jump verifies LocalMedia
// and private regular file existence under downloads/, re-verifies that the
// playback session has not changed behind the confirmation, then calls Load
// (autoPlay=true, preserving the prior book's checkpoint via Controller) and
// SeekAbsolute to the exact bookmark position.
func (s *Service) Jump(ctx context.Context, bookmark store.Bookmark, expectedSession uint64, targetTitle string, targetDuration time.Duration) (playback.Snapshot, error) {
	if s == nil || s.store == nil || s.player == nil || ctx == nil {
		return playback.Snapshot{}, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return s.player.Snapshot(), err
	}
	if bookmark.Key.Validate() != nil || bookmark.PositionNS < 0 || targetDuration < 0 {
		return s.player.Snapshot(), ErrInvalidInput
	}
	if targetTitle != "" && !safeSingleLine(targetTitle) {
		return s.player.Snapshot(), ErrInvalidInput
	}

	jumpPos := time.Duration(bookmark.PositionNS)
	snap := s.player.Snapshot()
	if snap.Session != expectedSession {
		return snap, ErrStaleSession
	}

	if snap.Book != nil && *snap.Book == bookmark.Key {
		if expectedSession == 0 {
			return snap, ErrStaleSession
		}
		if snap.State != playback.StatePlaying && snap.State != playback.StatePaused {
			return snap, ErrNoLoadedAudio
		}
		out, err := s.player.SeekAbsolute(ctx, expectedSession, jumpPos)
		return out, mapPlaybackErr(err)
	}

	media, found, err := s.store.LocalMedia(ctx, bookmark.Key)
	if err != nil {
		return snap, err
	}
	if !found || s.mediaChecker == nil {
		return snap, ErrMediaMissing
	}
	absPath, err := s.mediaChecker.VerifyMedia(ctx, media)
	if err != nil {
		return snap, err
	}
	if !validResolvedMediaPath(absPath) {
		return snap, ErrMediaMissing
	}
	if strings.TrimSpace(targetTitle) == "" {
		return snap, ErrInvalidInput
	}

	snapAfter := s.player.Snapshot()
	if !sameSessionContext(snap, snapAfter, expectedSession) {
		return snapAfter, ErrStaleSession
	}

	track := playback.Track{
		Key:      bookmark.Key,
		Title:    targetTitle,
		Path:     absPath,
		Duration: targetDuration,
	}
	loadedSnap, err := s.player.Load(ctx, track, true)
	if err != nil {
		return loadedSnap, err
	}
	seekSnap, err := s.player.SeekAbsolute(ctx, loadedSnap.Session, jumpPos)
	return seekSnap, mapPlaybackErr(err)
}

func (s *Service) acquireEditToken(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.editToken:
		return nil
	}
}

func (s *Service) releaseEditToken() {
	s.editToken <- struct{}{}
}

func defaultBookmarkID() (string, error) {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return "bm-" + hex.EncodeToString(buf[:]), nil
}

func validateDraft(draft Draft) error {
	if err := draft.BookKey.Validate(); err != nil {
		return ErrInvalidInput
	}
	if draft.CapturedPosition < 0 || draft.PreRoll < 0 || draft.JumpPosition < 0 {
		return ErrInvalidInput
	}
	if draft.CapturedChapter != nil {
		if err := draft.CapturedChapter.validate(); err != nil {
			return err
		}
	}
	if draft.JumpChapter != nil {
		if err := draft.JumpChapter.validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateNote(note string) error {
	if strings.TrimSpace(note) == "" || !safeMultiline(note) {
		return ErrInvalidInput
	}
	return nil
}

func cloneBookmarks(items []store.Bookmark) []store.Bookmark {
	if len(items) == 0 {
		return nil
	}
	out := make([]store.Bookmark, len(items))
	for i, item := range items {
		out[i] = item
		if item.ChapterID != nil {
			v := *item.ChapterID
			out[i].ChapterID = &v
		}
		if item.ChapterTitle != nil {
			v := *item.ChapterTitle
			out[i].ChapterTitle = &v
		}
	}
	return out
}

func sameSessionContext(before, after playback.Snapshot, expectedSession uint64) bool {
	if after.Session != expectedSession {
		return false
	}
	if (before.Book == nil) != (after.Book == nil) {
		return false
	}
	if before.Book != nil && *before.Book != *after.Book {
		return false
	}
	return true
}

func mapPlaybackErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, playback.ErrStaleSession) || errors.Is(err, playback.ErrNoActiveTrack) {
		return fmt.Errorf("%w: %w", ErrStaleSession, err)
	}
	return err
}

func validResolvedMediaPath(mediaPath string) bool {
	if !safeSingleLine(mediaPath) || !filepath.IsAbs(mediaPath) || filepath.Clean(mediaPath) != mediaPath {
		return false
	}
	if !strings.EqualFold(filepath.Ext(mediaPath), ".m4b") {
		return false
	}
	base := filepath.Base(mediaPath)
	return len(base) > len(".m4b") && !strings.HasPrefix(base, ".")
}

type sysOps struct {
	fstat    func(int, *unix.Stat_t) error
	fstatat  func(int, string, *unix.Stat_t, int) error
	readlink func(string) (string, error)
}

func defaultSysOps() sysOps {
	return sysOps{
		fstat:    unix.Fstat,
		fstatat:  unix.Fstatat,
		readlink: os.Readlink,
	}
}

type rootMediaChecker struct {
	owner RootOwner
	ops   sysOps
}

func (c *rootMediaChecker) VerifyMedia(ctx context.Context, media store.LocalMedia) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := media.Validate(); err != nil {
		return "", ErrMediaMissing
	}

	lease, err := c.owner.Lease(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	if lease == nil || lease.Root == nil {
		return "", ErrMediaMissing
	}
	defer lease.Close()
	if err := lease.Context.Err(); err != nil {
		return "", err
	}

	euid := uint32(os.Geteuid())
	var rootStat unix.Stat_t
	if err := c.ops.fstat(int(lease.Root.Fd()), &rootStat); err != nil {
		return "", ErrMediaMissing
	}
	if rootStat.Mode&(unix.S_IFMT|07777) != unix.S_IFDIR|0700 || rootStat.Uid != euid {
		return "", ErrMediaMissing
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
			return "", ErrMediaMissing
		}
		dirFD = nextFD
		closeDir = true
		var st unix.Stat_t
		if err := c.ops.fstat(dirFD, &st); err != nil {
			return "", ErrMediaMissing
		}
		if st.Mode&(unix.S_IFMT|07777) != unix.S_IFDIR|0700 || st.Uid != euid {
			return "", ErrMediaMissing
		}
	}

	leaf := parts[len(parts)-1]
	var st unix.Stat_t
	if err := c.ops.fstatat(dirFD, leaf, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return "", ErrMediaMissing
	}
	if st.Mode&(unix.S_IFMT|07777) != unix.S_IFREG|0600 || st.Uid != euid || st.Nlink != 1 {
		return "", ErrMediaMissing
	}

	dirPath, err := c.ops.readlink("/proc/self/fd/" + strconv.FormatUint(uint64(dirFD), 10))
	if err != nil || strings.HasSuffix(dirPath, " (deleted)") || !filepath.IsAbs(dirPath) {
		return "", ErrMediaMissing
	}
	absPath := filepath.Join(dirPath, leaf)
	if !validResolvedMediaPath(absPath) {
		return "", ErrMediaMissing
	}
	return absPath, nil
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

func safeMultiline(text string) bool {
	if !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		if unicode.Is(unicode.Cf, r) {
			return false
		}
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return false
		}
	}
	return true
}

func safeID(id string) bool {
	return id != "" && strings.TrimSpace(id) == id && safeSingleLine(id)
}
