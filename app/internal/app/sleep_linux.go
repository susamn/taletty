package app

import (
	"context"
	"fmt"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/playback"
	"github.com/susamn/taletty/app/internal/store"
	"github.com/susamn/taletty/app/internal/ui"
)

type sleepKind int

const (
	sleepOff sleepKind = iota
	sleepDuration
	sleepChapterEnd
)

type sleepTimerState struct {
	kind         sleepKind
	gen          uint64
	session      uint64
	book         store.BookKey
	remaining    time.Duration
	lastTick     time.Time
	chapterStart time.Duration
	chapterEnd   time.Duration
	chapterTitle string
}

func (c *Coordinator) setSleepTimer(action ui.Action) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.NoticeMsg{Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	if c.player == nil {
		return ui.NoticeMsg{Error: "Sleep timer unavailable."}
	}
	session, err := strconv.ParseUint(action.Session, 10, 64)
	if err != nil || session == 0 {
		return ui.NoticeMsg{Error: "No active playback session."}
	}
	snap := c.player.Snapshot()
	active := snap.State == playback.StatePlaying || snap.State == playback.StatePaused
	if !active || snap.Book == nil || snap.Session != session {
		return ui.NoticeMsg{Error: "Playback session changed; reopen sleep timer."}
	}

	switch action.SleepMode {
	case ui.SleepModeOff:
		c.mu.Lock()
		c.cancelSleepTimerLocked()
		c.mu.Unlock()
		return c.refreshPlaybackSnapshot(snap, c.cachedChaptersFor(snap.Book), "Sleep timer turned off")

	case ui.SleepModeDuration:
		if action.SleepDuration <= 0 {
			return ui.NoticeMsg{Error: SanitizeError(ErrInvalidInput)}
		}
		now := c.clock()
		c.mu.Lock()
		c.sleepGen++
		c.sleep = sleepTimerState{
			kind:      sleepDuration,
			gen:       c.sleepGen,
			session:   snap.Session,
			book:      *snap.Book,
			remaining: action.SleepDuration,
			lastTick:  now,
		}
		c.mu.Unlock()
		return c.refreshPlaybackSnapshot(snap, c.cachedChaptersFor(snap.Book), "Sleep timer set for "+formatSleepDuration(action.SleepDuration))

	case ui.SleepModeChapter:
		chapters := c.cachedChaptersFor(snap.Book)
		if len(chapters) == 0 && c.chapterReader != nil {
			if absPath, _, _, ready := c.verifyBookMedia(ctx, *snap.Book); ready {
				chapters = c.ensureChapters(ctx, *snap.Book, absPath)
			}
		}
		chStart, chEnd, chTitle, ok := activeChapterBounds(chapters, snap.Position, snap.Duration)
		if !ok {
			return ui.NoticeMsg{Activity: "Chapter end unavailable: no chapter metadata"}
		}
		c.mu.Lock()
		c.sleepGen++
		c.sleep = sleepTimerState{
			kind:         sleepChapterEnd,
			gen:          c.sleepGen,
			session:      snap.Session,
			book:         *snap.Book,
			chapterStart: chStart,
			chapterEnd:   chEnd,
			chapterTitle: chTitle,
		}
		c.mu.Unlock()
		return c.refreshPlaybackSnapshot(snap, chapters, "Sleep timer set for end of "+chTitle)

	default:
		return ui.NoticeMsg{Error: SanitizeError(ErrInvalidInput)}
	}
}

func activeChapterBounds(chapters []Chapter, pos, bookDur time.Duration) (time.Duration, time.Duration, string, bool) {
	if len(chapters) == 0 {
		return 0, 0, "", false
	}
	idx := -1
	for i, ch := range chapters {
		if pos >= ch.Start {
			idx = i
		}
	}
	if idx < 0 {
		return 0, 0, "", false
	}
	ch := chapters[idx]
	var end time.Duration
	switch {
	case ch.End > ch.Start:
		end = ch.End
	case idx+1 < len(chapters) && chapters[idx+1].Start > ch.Start:
		end = chapters[idx+1].Start
	case bookDur > ch.Start:
		end = bookDur
	}
	if end <= ch.Start || pos >= end {
		return 0, 0, "", false
	}
	return ch.Start, end, ch.Title, true
}

func (c *Coordinator) cancelSleepTimerLocked() {
	if c.sleep.kind != sleepOff {
		c.sleep = sleepTimerState{}
		c.sleepGen++
	}
}

func (c *Coordinator) reconcileSleepAfterSeekLocked(snap playback.Snapshot) string {
	if c.sleep.kind == sleepOff {
		return ""
	}
	if snap.Book == nil || *snap.Book != c.sleep.book || snap.Session != c.sleep.session {
		c.cancelSleepTimerLocked()
		return "Sleep timer canceled"
	}
	if c.sleep.kind == sleepChapterEnd && (snap.Position < c.sleep.chapterStart || snap.Position >= c.sleep.chapterEnd) {
		c.cancelSleepTimerLocked()
		return "Sleep timer canceled: seeked to another chapter"
	}
	return ""
}

func (c *Coordinator) advanceSleepTimer(ctx context.Context, snap playback.Snapshot) (playback.Snapshot, string) {
	now := c.clock()
	var shouldPause bool
	var armedSession uint64
	var activity string

	c.mu.Lock()
	switch c.sleep.kind {
	case sleepOff:
		c.mu.Unlock()
		return snap, ""
	case sleepDuration:
		if snap.Book == nil || *snap.Book != c.sleep.book || snap.Session != c.sleep.session ||
			(snap.State != playback.StatePlaying && snap.State != playback.StatePaused) {
			c.cancelSleepTimerLocked()
			c.mu.Unlock()
			return snap, "Sleep timer canceled"
		}
		if snap.State == playback.StatePlaying {
			elapsed := max(time.Duration(0), now.Sub(c.sleep.lastTick))
			c.sleep.remaining -= elapsed
		}
		c.sleep.lastTick = now
		if c.sleep.remaining <= 0 {
			armedSession = c.sleep.session
			c.cancelSleepTimerLocked()
			shouldPause = true
			activity = "Sleep timer expired; playback paused"
		}
	case sleepChapterEnd:
		if snap.Book == nil || *snap.Book != c.sleep.book || snap.Session != c.sleep.session ||
			(snap.State != playback.StatePlaying && snap.State != playback.StatePaused) {
			c.cancelSleepTimerLocked()
			c.mu.Unlock()
			return snap, "Sleep timer canceled"
		}
		if snap.State == playback.StatePlaying && snap.Position >= c.sleep.chapterEnd {
			armedSession = c.sleep.session
			activity = "Sleep timer expired at end of " + c.sleep.chapterTitle
			c.cancelSleepTimerLocked()
			shouldPause = true
		} else if snap.State == playback.StatePaused && snap.Completion == store.CompletionFinished && snap.Duration > 0 && snap.Position >= snap.Duration {
			c.cancelSleepTimerLocked()
		}
	}
	c.mu.Unlock()

	if shouldPause && c.player != nil {
		pausedSnap, err := c.player.Pause(ctx, armedSession)
		if err != nil {
			return snap, sanitizePlaybackError(err)
		}
		return pausedSnap, activity
	}
	return snap, activity
}

func (c *Coordinator) sleepLabelLocked() string {
	switch c.sleep.kind {
	case sleepDuration:
		return formatSleepDuration(c.sleep.remaining)
	case sleepChapterEnd:
		return c.sleep.chapterTitle
	default:
		return ""
	}
}

func formatSleepDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d >= time.Minute {
		mins := (d + time.Minute - 1) / time.Minute
		return fmt.Sprintf("%dm", mins)
	}
	secs := max(int64(1), int64(d/time.Second))
	return fmt.Sprintf("%ds", secs)
}
