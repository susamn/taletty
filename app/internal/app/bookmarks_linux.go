package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/bookmarks"
	"github.com/susamn/taletty/app/internal/playback"
	"github.com/susamn/taletty/app/internal/store"
	"github.com/susamn/taletty/app/internal/ui"
)

const maxTrackedDrafts = 64

// BookmarkManager abstracts persistent bookmark capture, editing, deletion,
// offline listing, and explicit same-book / cross-book jump targeting.
// *bookmarks.Service satisfies this interface directly.
type BookmarkManager interface {
	CaptureDraft(ctx context.Context, preRoll time.Duration, chapterAt bookmarks.ChapterLookup) (bookmarks.Draft, error)
	SaveNew(ctx context.Context, draft bookmarks.Draft, note string, nowMS int64) (store.Bookmark, error)
	EditNote(ctx context.Context, key store.BookKey, bookmarkID, note string, nowMS int64) (store.Bookmark, error)
	Delete(ctx context.Context, key store.BookKey, bookmarkID string) error
	List(ctx context.Context, key store.BookKey) ([]store.Bookmark, error)
	Jump(ctx context.Context, bookmark store.Bookmark, expectedSession uint64, targetTitle string, targetDuration time.Duration) (playback.Snapshot, error)
}

var _ BookmarkManager = (*bookmarks.Service)(nil)

func (c *Coordinator) handleBookmarkAction(action ui.Action) tea.Msg {
	switch action.Kind {
	case ui.CaptureBookmark:
		return c.captureBookmark()
	case ui.SaveNote:
		return c.saveNote(action)
	case ui.OpenBookmarks:
		return c.openBookmarks(action.Key)
	case ui.DeleteBookmark:
		return c.deleteBookmark(action)
	case ui.JumpBookmark:
		return c.jumpBookmark(action)
	case ui.ConfirmSwitchBookmark:
		return c.confirmSwitchBookmark(action)
	default:
		return ui.NoticeMsg{Activity: "Action unavailable in this slice"}
	}
}

func (c *Coordinator) chapterLookup() bookmarks.ChapterLookup {
	return func(key store.BookKey, pos time.Duration) (bookmarks.ChapterRef, bool) {
		chs := c.cachedChaptersFor(&key)
		if len(chs) == 0 {
			return bookmarks.ChapterRef{}, false
		}
		idx := -1
		for i, ch := range chs {
			if pos >= ch.Start {
				idx = i
			}
		}
		if idx < 0 {
			idx = 0
		}
		return bookmarks.ChapterRef{
			ID:    fmt.Sprintf("ch-%d", idx+1),
			Title: sanitizeChapterTitle(chs[idx].Title),
		}, true
	}
}

func (c *Coordinator) captureBookmark() tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.NoticeMsg{Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	if c.bookmarks == nil {
		return ui.NoticeMsg{Activity: "Bookmark capture unavailable."}
	}
	if c.player != nil {
		if snap := c.player.Snapshot(); snap.Book != nil && len(c.cachedChaptersFor(snap.Book)) == 0 && c.chapterReader != nil {
			if absPath, _, _, ready := c.verifyBookMedia(ctx, *snap.Book); ready {
				_ = c.ensureChapters(ctx, *snap.Book, absPath)
			}
		}
	}

	draft, err := c.bookmarks.CaptureDraft(ctx, c.preRoll, c.chapterLookup())
	if err != nil {
		if errors.Is(err, bookmarks.ErrNoLoadedAudio) {
			return ui.NoticeMsg{Activity: "Bookmark capture unavailable: no loaded audio"}
		}
		return ui.NoticeMsg{Error: sanitizeBookmarkError(err)}
	}

	encodedKey, err := EncodeBookKey(draft.BookKey)
	if err != nil {
		return ui.NoticeMsg{Error: SanitizeError(ErrInvalidInput)}
	}
	title := draft.Title
	if strings.TrimSpace(title) == "" {
		title = c.bookTitle(draft.BookKey)
	}
	capturedCh := "No chapter metadata"
	if draft.CapturedChapter != nil && strings.TrimSpace(draft.CapturedChapter.Title) != "" {
		capturedCh = draft.CapturedChapter.Title
	}
	jumpCh := ""
	if draft.CrossedChapterBoundary() {
		jumpCh = "No chapter metadata"
		if draft.JumpChapter != nil && strings.TrimSpace(draft.JumpChapter.Title) != "" {
			jumpCh = draft.JumpChapter.Title
		}
	}

	c.mu.Lock()
	c.dialogSeq++
	id := fmt.Sprintf("bm-capture-%d", c.dialogSeq)
	c.storePendingDraftLocked(id, draft)
	c.mu.Unlock()

	return ui.OpenNoteEditorMsg{
		ID: id,
		Target: ui.NoteTarget{
			Key:         encodedKey,
			Title:       title,
			Chapter:     capturedCh,
			JumpChapter: jumpCh,
			Captured:    draft.CapturedPosition,
			Jump:        draft.JumpPosition,
		},
	}
}

func (c *Coordinator) storePendingDraftLocked(id string, draft bookmarks.Draft) {
	if c.pendingDrafts == nil {
		c.pendingDrafts = make(map[string]bookmarks.Draft)
	}
	c.pendingDrafts[id] = draft
	c.draftOrder = append(c.draftOrder, id)
	for len(c.draftOrder) > maxTrackedDrafts {
		oldest := c.draftOrder[0]
		c.draftOrder = c.draftOrder[1:]
		delete(c.pendingDrafts, oldest)
	}
}

func (c *Coordinator) storeCommittedDraftLocked(id string, bm store.Bookmark) {
	if c.committedDrafts == nil {
		c.committedDrafts = make(map[string]store.Bookmark)
	}
	c.committedDrafts[id] = bm
	c.committedOrder = append(c.committedOrder, id)
	for len(c.committedOrder) > maxTrackedDrafts {
		oldest := c.committedOrder[0]
		c.committedOrder = c.committedOrder[1:]
		delete(c.committedDrafts, oldest)
	}
}

func (c *Coordinator) saveNote(action ui.Action) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	if c.bookmarks == nil {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: "Bookmarks unavailable."}
	}
	nowMS := max(int64(0), c.clock().UnixMilli())

	if action.Target.BookmarkID != "" {
		key, err := DecodeBookKey(action.Target.Key)
		if err != nil {
			return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: SanitizeError(ErrInvalidInput)}
		}
		updated, err := c.bookmarks.EditNote(ctx, key, action.Target.BookmarkID, action.Text, nowMS)
		if err != nil {
			return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: sanitizeBookmarkError(err)}
		}
		return c.bookmarkMutationSuccess(ctx, action, key, "Updated bookmark at "+formatClock(time.Duration(updated.PositionNS)))
	}

	c.mu.Lock()
	committed, alreadyCommitted := c.committedDrafts[action.RequestID]
	draft, hasDraft := c.pendingDrafts[action.RequestID]
	c.mu.Unlock()

	if alreadyCommitted {
		return c.bookmarkMutationSuccess(ctx, action, committed.Key, "Saved bookmark at "+formatClock(time.Duration(committed.PositionNS)))
	}
	if !hasDraft {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: "Bookmark draft expired; capture again."}
	}

	saved, err := c.bookmarks.SaveNew(ctx, draft, action.Text, nowMS)
	if err != nil {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: sanitizeBookmarkError(err)}
	}

	c.mu.Lock()
	delete(c.pendingDrafts, action.RequestID)
	c.storeCommittedDraftLocked(action.RequestID, saved)
	c.mu.Unlock()

	return c.bookmarkMutationSuccess(ctx, action, draft.BookKey, "Saved bookmark at "+formatClock(draft.JumpPosition))
}

func (c *Coordinator) bookmarkMutationSuccess(ctx context.Context, action ui.Action, key store.BookKey, activity string) tea.Msg {
	encodedKey, _ := EncodeBookKey(key)
	items, listErr := c.bookmarks.List(ctx, key)
	cmds := []tea.Cmd{
		func() tea.Msg { return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt} },
		func() tea.Msg { return ui.NoticeMsg{Activity: activity} },
	}
	if listErr == nil && encodedKey != "" {
		uiItems := projectBookmarks(items)
		cmds = append(cmds, func() tea.Msg {
			return ui.BookmarksMsg{Key: encodedKey, Bookmarks: uiItems}
		})
	}
	return tea.BatchMsg(cmds)
}

func (c *Coordinator) openBookmarks(encodedKey string) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.OpenBookmarksMsg{Key: encodedKey, Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	key, err := DecodeBookKey(encodedKey)
	if err != nil {
		return ui.OpenBookmarksMsg{Key: encodedKey, Error: SanitizeError(ErrInvalidInput)}
	}
	title := c.bookTitle(key)
	if c.bookmarks == nil {
		return ui.OpenBookmarksMsg{Key: encodedKey, Title: title, Error: "Bookmarks unavailable."}
	}
	items, err := c.bookmarks.List(ctx, key)
	if err != nil {
		return ui.OpenBookmarksMsg{Key: encodedKey, Title: title, Error: sanitizeBookmarkError(err)}
	}
	return ui.OpenBookmarksMsg{
		Key:       encodedKey,
		Title:     title,
		Bookmarks: projectBookmarks(items),
	}
}

func (c *Coordinator) deleteBookmark(action ui.Action) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	if c.bookmarks == nil {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: "Bookmarks unavailable."}
	}
	key, err := DecodeBookKey(action.Key)
	if err != nil {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: SanitizeError(ErrInvalidInput)}
	}
	delErr := c.bookmarks.Delete(ctx, key, action.BookmarkID)
	items, listErr := c.bookmarks.List(ctx, key)
	if delErr != nil {
		cmds := []tea.Cmd{
			func() tea.Msg {
				return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: sanitizeBookmarkError(delErr)}
			},
		}
		if listErr == nil {
			uiItems := projectBookmarks(items)
			cmds = append(cmds, func() tea.Msg {
				return ui.BookmarksMsg{Key: action.Key, Bookmarks: uiItems}
			})
		}
		return tea.BatchMsg(cmds)
	}
	return c.bookmarkMutationSuccess(ctx, action, key, "Deleted bookmark")
}

func (c *Coordinator) findBookmark(ctx context.Context, key store.BookKey, bookmarkID string) (store.Bookmark, bool, error) {
	items, err := c.bookmarks.List(ctx, key)
	if err != nil {
		return store.Bookmark{}, false, err
	}
	for _, item := range items {
		if item.ID == bookmarkID {
			return item, true, nil
		}
	}
	return store.Bookmark{}, false, nil
}

func (c *Coordinator) jumpBookmark(action ui.Action) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.NoticeMsg{Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	if c.bookmarks == nil || c.player == nil {
		return ui.NoticeMsg{Error: "Bookmark jump unavailable."}
	}
	key, err := DecodeBookKey(action.Key)
	if err != nil {
		return ui.NoticeMsg{Error: SanitizeError(ErrInvalidInput)}
	}
	bm, found, err := c.findBookmark(ctx, key, action.BookmarkID)
	if err != nil {
		return ui.NoticeMsg{Error: sanitizeBookmarkError(err)}
	}
	if !found {
		return ui.NoticeMsg{Error: "Bookmark no longer exists."}
	}

	snap := c.player.Snapshot()
	active := snap.State == playback.StatePlaying || snap.State == playback.StatePaused
	if active && snap.Book != nil && *snap.Book == key {
		seeked, err := c.bookmarks.Jump(ctx, bm, snap.Session, safeJumpTitle(c.bookTitle(key)), c.bookDuration(key))
		if err != nil {
			return ui.NoticeMsg{Error: sanitizeBookmarkError(err)}
		}
		c.mu.Lock()
		sleepNotice := c.reconcileSleepAfterSeekLocked(seeked)
		c.mu.Unlock()
		activity := "Jumped to " + formatClock(time.Duration(bm.PositionNS))
		if sleepNotice != "" {
			activity = sleepNotice
		}
		return c.refreshPlaybackSnapshot(seeked, c.cachedChaptersFor(&key), activity)
	}

	absPath, title, dur, ready := c.verifyBookMedia(ctx, key)
	if !ready {
		return c.missingAudioPrompt(action.Key, title)
	}
	if active && snap.Book != nil && *snap.Book != key {
		c.mu.Lock()
		c.dialogSeq++
		id := fmt.Sprintf("bm-switch-%d", c.dialogSeq)
		c.mu.Unlock()
		return ui.OpenConfirmationMsg{
			ID:      id,
			Title:   "Switch audiobook",
			Prompt:  fmt.Sprintf("Switch from %q to %q at %s?", snap.Title, title, formatClock(time.Duration(bm.PositionNS))),
			Keep:    "Stay",
			Confirm: "Switch and play",
			Action: ui.Action{
				Kind:        ui.ConfirmSwitchBookmark,
				Key:         action.Key,
				Session:     strconv.FormatUint(snap.Session, 10),
				BookmarkID:  bm.ID,
				BookmarkPos: time.Duration(bm.PositionNS),
			},
		}
	}

	chapters := c.ensureChapters(ctx, key, absPath)
	seeked, err := c.bookmarks.Jump(ctx, bm, snap.Session, safeJumpTitle(title), dur)
	if err != nil {
		return ui.NoticeMsg{Error: sanitizeBookmarkError(err)}
	}
	c.mu.Lock()
	c.cancelSleepTimerLocked()
	c.mu.Unlock()
	return c.refreshPlaybackSnapshot(seeked, chapters, "Playing "+title+" at "+formatClock(time.Duration(bm.PositionNS)))
}

func (c *Coordinator) confirmSwitchBookmark(action ui.Action) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	if c.bookmarks == nil || c.player == nil {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: "Playback unavailable."}
	}
	expectedSession, err := strconv.ParseUint(action.Session, 10, 64)
	if err != nil || expectedSession == 0 {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: "Invalid playback session."}
	}
	snap := c.player.Snapshot()
	if snap.Session != expectedSession || (snap.State != playback.StatePlaying && snap.State != playback.StatePaused) {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: "Playback changed; confirm switch again."}
	}
	key, err := DecodeBookKey(action.Key)
	if err != nil {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: SanitizeError(ErrInvalidInput)}
	}
	bm, found, err := c.findBookmark(ctx, key, action.BookmarkID)
	if err != nil {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: sanitizeBookmarkError(err)}
	}
	if !found {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: "Bookmark no longer exists."}
	}
	absPath, title, dur, ready := c.verifyBookMedia(ctx, key)
	if !ready {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: "Local audio file is missing or unsafe."}
	}
	chapters := c.ensureChapters(ctx, key, absPath)
	seeked, err := c.bookmarks.Jump(ctx, bm, expectedSession, safeJumpTitle(title), dur)
	if err != nil {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: sanitizeBookmarkError(err)}
	}
	c.mu.Lock()
	c.cancelSleepTimerLocked()
	c.mu.Unlock()
	updated := c.refreshPlaybackSnapshot(seeked, chapters, "Switched to "+title+" at "+formatClock(time.Duration(bm.PositionNS)))
	return tea.BatchMsg{
		func() tea.Msg { return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt} },
		func() tea.Msg { return updated },
	}
}

func projectBookmarks(items []store.Bookmark) []ui.Bookmark {
	if len(items) == 0 {
		return nil
	}
	out := make([]ui.Bookmark, 0, len(items))
	for _, item := range items {
		encoded, err := EncodeBookKey(item.Key)
		if err != nil {
			continue
		}
		chID := ""
		if item.ChapterID != nil {
			chID = *item.ChapterID
		}
		chTitle := ""
		if item.ChapterTitle != nil {
			chTitle = *item.ChapterTitle
		}
		out = append(out, ui.Bookmark{
			ID:           item.ID,
			Key:          encoded,
			Position:     time.Duration(item.PositionNS),
			ChapterID:    chID,
			ChapterTitle: chTitle,
			Note:         item.Note,
			CreatedAtMS:  item.CreatedAtMS,
			UpdatedAtMS:  item.UpdatedAtMS,
		})
	}
	return out
}

func safeJumpTitle(title string) string {
	if !utf8.ValidString(title) {
		title = strings.ToValidUTF8(title, "")
	}
	var b strings.Builder
	for _, r := range title {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		b.WriteRune(r)
	}
	cleaned := strings.TrimSpace(b.String())
	if cleaned == "" {
		return "Audiobook"
	}
	return cleaned
}

func sanitizeBookmarkError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, bookmarks.ErrNoLoadedAudio):
		return "No loaded audio session."
	case errors.Is(err, bookmarks.ErrStaleSession):
		return "Playback session changed; retry action."
	case errors.Is(err, bookmarks.ErrBookmarkNotFound):
		return "Bookmark no longer exists."
	case errors.Is(err, bookmarks.ErrMediaMissing):
		return "Local audio file is missing or unsafe."
	case errors.Is(err, bookmarks.ErrInvalidInput):
		return "Invalid bookmark note or target."
	default:
		return sanitizePlaybackError(err)
	}
}
