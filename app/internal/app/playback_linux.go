package app

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/susamn/taletty/app/internal/playback"
	"github.com/susamn/taletty/app/internal/store"
	"github.com/susamn/taletty/app/internal/ui"
	"golang.org/x/sys/unix"
)

const (
	seekStepDuration = 15 * time.Second
	volumeStepPct    = 5
	speedStepMilli   = int64(100_000) // 0.1x per docs/design/keys.md
	ffprobeTimeout   = 5 * time.Second
	maxProbeBytes    = 4 << 20
)

// PlaybackController abstracts the serialized playback authority.
// *playback.Controller satisfies this interface directly.
type PlaybackController interface {
	Snapshot() playback.Snapshot
	Load(ctx context.Context, track playback.Track, autoPlay bool) (playback.Snapshot, error)
	Replay(ctx context.Context, track playback.Track) (playback.Snapshot, error)
	SetCompletion(ctx context.Context, key store.BookKey, completion store.Completion) (playback.Snapshot, error)
	SetSmartRewind(policy playback.SmartRewindPolicy) error
	Toggle(ctx context.Context, session uint64) (playback.Snapshot, error)
	SeekRelative(ctx context.Context, session uint64, delta time.Duration) (playback.Snapshot, error)
	SeekAbsolute(ctx context.Context, session uint64, target time.Duration) (playback.Snapshot, error)
	StepSpeed(ctx context.Context, session uint64, deltaMilli int64) (playback.Snapshot, error)
	StepVolume(ctx context.Context, deltaPercent int) (playback.Snapshot, error)
	Poll(ctx context.Context) (playback.Snapshot, error)
	Stop(ctx context.Context) (playback.Snapshot, error)
	Close(ctx context.Context) error
}

var _ PlaybackController = (*playback.Controller)(nil)

// MediaChecker verifies that a persisted LocalMedia record points to a private
// regular .m4b file under ~/.config/taletty/downloads/ and returns its clean path.
type MediaChecker interface {
	VerifyMedia(ctx context.Context, media store.LocalMedia) (string, error)
}

// DownloadEnqueuer queues a missing book for download.
type DownloadEnqueuer interface {
	Enqueue(ctx context.Context, key store.BookKey) error
}

// Chapter describes a single normalized audiobook chapter.
type Chapter struct {
	Title string
	Start time.Duration
	End   time.Duration
}

// ChapterReader extracts chapter metadata from a verified local .m4b file.
type ChapterReader interface {
	ReadChapters(ctx context.Context, mediaPath string) ([]Chapter, error)
}

// FFprobeChapterReader reads chapter metadata using ffprobe with bounded execution.
type FFprobeChapterReader struct {
	Binary  string
	Timeout time.Duration
}

// ReadChapters runs ffprobe on mediaPath and returns sanitized chapters sorted by Start.
// Missing ffprobe returns nil, nil so playback still works without chapter metadata.
func (r FFprobeChapterReader) ReadChapters(ctx context.Context, mediaPath string) ([]Chapter, error) {
	if ctx == nil {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bin := r.Binary
	if bin == "" {
		bin = "ffprobe"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = ffprobeTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, resolved,
		"-v", "error",
		"-show_entries", "chapter=start_time,end_time:chapter_tags=title",
		"-of", "json",
		mediaPath,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 500 * time.Millisecond
	var stdout limitedBuffer
	stdout.limit = maxProbeBytes
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil
	}
	return parseFFprobeChapters(stdout.buf.Bytes()), nil
}

type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.limit {
		return 0, errors.New("ffprobe output exceeded limit")
	}
	return b.buf.Write(p)
}

func parseFFprobeChapters(raw []byte) []Chapter {
	var payload struct {
		Chapters []struct {
			StartTime string `json:"start_time"`
			EndTime   string `json:"end_time"`
			Tags      struct {
				Title string `json:"title"`
			} `json:"tags"`
		} `json:"chapters"`
	}
	if json.Unmarshal(raw, &payload) != nil || len(payload.Chapters) == 0 {
		return nil
	}
	out := make([]Chapter, 0, len(payload.Chapters))
	for i, ch := range payload.Chapters {
		startSec, err := strconv.ParseFloat(ch.StartTime, 64)
		if err != nil || startSec < 0 {
			continue
		}
		endSec, err := strconv.ParseFloat(ch.EndTime, 64)
		if err != nil || endSec < startSec {
			endSec = startSec
		}
		title := sanitizeChapterTitle(ch.Tags.Title)
		if strings.TrimSpace(title) == "" {
			title = fmt.Sprintf("Chapter %d", i+1)
		}
		out = append(out, Chapter{
			Title: title,
			Start: time.Duration(startSec * float64(time.Second)),
			End:   time.Duration(endSec * float64(time.Second)),
		})
	}
	slices.SortStableFunc(out, func(a, b Chapter) int {
		return cmp.Compare(a.Start, b.Start)
	})
	return out
}

func sanitizeChapterTitle(text string) string {
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "")
	}
	var b strings.Builder
	for _, r := range text {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

type rootMediaChecker struct {
	owner RootLeaseOwner
}

// NewRootMediaChecker constructs an anchored private-file verifier over owner.
func NewRootMediaChecker(owner RootLeaseOwner) MediaChecker {
	return &rootMediaChecker{owner: owner}
}

func (c *rootMediaChecker) VerifyMedia(ctx context.Context, media store.LocalMedia) (string, error) {
	if c == nil || c.owner == nil || ctx == nil {
		return "", ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := media.Validate(); err != nil {
		return "", ErrInvalidInput
	}
	lease, err := c.owner.Lease(ctx)
	if err != nil {
		return "", err
	}
	if lease == nil || lease.Root == nil {
		return "", ErrInvalidInput
	}
	defer lease.Close()
	if err := lease.Context.Err(); err != nil {
		return "", err
	}

	euid := uint32(os.Geteuid())
	var rootStat unix.Stat_t
	if err := unix.Fstat(int(lease.Root.Fd()), &rootStat); err != nil {
		return "", ErrInvalidInput
	}
	if rootStat.Mode&(unix.S_IFMT|07777) != unix.S_IFDIR|0700 || rootStat.Uid != euid {
		return "", ErrInvalidInput
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
			return "", ErrInvalidInput
		}
		dirFD = nextFD
		closeDir = true
		var st unix.Stat_t
		if err := unix.Fstat(dirFD, &st); err != nil {
			return "", ErrInvalidInput
		}
		if st.Mode&(unix.S_IFMT|07777) != unix.S_IFDIR|0700 || st.Uid != euid {
			return "", ErrInvalidInput
		}
	}

	leaf := parts[len(parts)-1]
	var st unix.Stat_t
	if err := unix.Fstatat(dirFD, leaf, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return "", ErrInvalidInput
	}
	if st.Mode&(unix.S_IFMT|07777) != unix.S_IFREG|0600 || st.Uid != euid || st.Nlink != 1 {
		return "", ErrInvalidInput
	}

	dirPath, err := os.Readlink("/proc/self/fd/" + strconv.FormatUint(uint64(dirFD), 10))
	if err != nil || strings.HasSuffix(dirPath, " (deleted)") || !filepath.IsAbs(dirPath) {
		return "", ErrInvalidInput
	}
	absPath := filepath.Join(dirPath, leaf)
	if !strings.EqualFold(filepath.Ext(absPath), ".m4b") {
		return "", ErrInvalidInput
	}
	return absPath, nil
}

func (c *Coordinator) handlePlaybackAction(action ui.Action) tea.Msg {
	switch action.Kind {
	case ui.PlayBook:
		return c.playBook(action.Key)
	case ui.ReplayBook:
		return c.replayBook(action)
	case ui.MarkFinished:
		return c.setBookCompletion(action.Key, store.CompletionFinished)
	case ui.MarkUnfinished:
		return c.setBookCompletion(action.Key, store.CompletionUnfinished)
	case ui.ActivateChapter:
		return c.activateChapter(action)
	case ui.ConfirmSwitchPlay:
		return c.confirmSwitchPlay(action)
	case ui.TogglePlayback:
		return c.togglePlayback(action.Session)
	case ui.SeekBackward:
		return c.seekRelative(action.Session, -seekStepDuration)
	case ui.SeekForward:
		return c.seekRelative(action.Session, seekStepDuration)
	case ui.SpeedDown:
		return c.stepSpeed(action.Session, -speedStepMilli)
	case ui.SpeedUp:
		return c.stepSpeed(action.Session, speedStepMilli)
	case ui.VolumeDown:
		return c.stepVolume(-volumeStepPct)
	case ui.VolumeUp:
		return c.stepVolume(volumeStepPct)
	default:
		return ui.NoticeMsg{Activity: "Action unavailable in this slice"}
	}
}

func (c *Coordinator) beginWorker() (context.Context, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, false
	}
	c.wg.Add(1)
	return c.ctx, true
}

func (c *Coordinator) verifyBookMedia(ctx context.Context, key store.BookKey) (string, string, time.Duration, bool) {
	media, found, err := c.store.LocalMedia(ctx, key)
	if err != nil || !found || c.mediaChecker == nil {
		return "", c.bookTitle(key), c.bookDuration(key), false
	}
	absPath, err := c.mediaChecker.VerifyMedia(ctx, media)
	if err != nil || absPath == "" {
		return "", c.bookTitle(key), c.bookDuration(key), false
	}
	return absPath, c.bookTitle(key), c.bookDuration(key), true
}

func (c *Coordinator) bookTitle(key store.BookKey) string {
	encoded, err := EncodeBookKey(key)
	if err != nil {
		return "Audiobook"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range c.snapshot.Books {
		if b.Key == encoded {
			return b.Title
		}
	}
	return "Audiobook"
}

func (c *Coordinator) bookDuration(key store.BookKey) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if dur, ok := c.durations[key]; ok && dur > 0 {
		return dur
	}
	return 0
}

func (c *Coordinator) ensureChapters(ctx context.Context, key store.BookKey, absPath string) []Chapter {
	c.mu.Lock()
	cached, ok := c.chapters[key]
	c.mu.Unlock()
	if ok {
		return cached
	}
	if c.chapterReader == nil || absPath == "" {
		return nil
	}
	chapters, err := c.chapterReader.ReadChapters(ctx, absPath)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	if c.chapters == nil {
		c.chapters = make(map[store.BookKey][]Chapter)
	}
	c.chapters[key] = slices.Clone(chapters)
	c.mu.Unlock()
	return chapters
}

// LoadChapters verifies local media for key, reads chapters via ChapterReader,
// caches them, and returns a ui.BookChaptersMsg for the UI.
func (c *Coordinator) LoadChapters(ctx context.Context, key store.BookKey) (ui.BookChaptersMsg, bool) {
	if c == nil || ctx == nil || c.chapterReader == nil {
		return ui.BookChaptersMsg{}, false
	}
	encoded, err := EncodeBookKey(key)
	if err != nil {
		return ui.BookChaptersMsg{}, false
	}
	absPath, _, _, ok := c.verifyBookMedia(ctx, key)
	if !ok {
		return ui.BookChaptersMsg{}, false
	}
	chapters := c.ensureChapters(ctx, key, absPath)
	if len(chapters) == 0 {
		return ui.BookChaptersMsg{}, false
	}
	titles, starts := splitChapters(chapters)
	c.mu.Lock()
	for i := range c.snapshot.Books {
		if c.snapshot.Books[i].Key == encoded {
			c.snapshot.Books[i].Chapters = slices.Clone(titles)
			c.snapshot.Books[i].ChapterStarts = slices.Clone(starts)
			break
		}
	}
	c.mu.Unlock()
	return ui.BookChaptersMsg{Key: encoded, Chapters: titles, Starts: starts}, true
}

// DownloadedBookKeys returns all BookKeys that currently have LocalMedia records.
func (c *Coordinator) DownloadedBookKeys(ctx context.Context) []store.BookKey {
	if c == nil || ctx == nil {
		return nil
	}
	media, err := c.store.LocalMediaList(ctx)
	if err != nil {
		return nil
	}
	keys := make([]store.BookKey, 0, len(media))
	for _, item := range media {
		keys = append(keys, item.Key)
	}
	return keys
}

func splitChapters(chapters []Chapter) ([]string, []time.Duration) {
	if len(chapters) == 0 {
		return nil, nil
	}
	titles := make([]string, len(chapters))
	starts := make([]time.Duration, len(chapters))
	for i, ch := range chapters {
		titles[i] = ch.Title
		starts[i] = ch.Start
	}
	return titles, starts
}

func (c *Coordinator) missingAudioPrompt(encodedKey, title string) tea.Msg {
	c.mu.Lock()
	c.dialogSeq++
	id := fmt.Sprintf("download-%d", c.dialogSeq)
	c.mu.Unlock()
	return ui.OpenConfirmationMsg{
		ID:      id,
		Title:   "Audio not downloaded",
		Prompt:  fmt.Sprintf("%q is not downloaded locally. Queue download?", title),
		Keep:    "Cancel",
		Confirm: "Queue download",
		Action:  ui.Action{Kind: ui.QueueDownload, Key: encodedKey},
	}
}

func (c *Coordinator) replayConfirmation(encodedKey, title string) tea.Msg {
	c.mu.Lock()
	c.dialogSeq++
	id := fmt.Sprintf("replay-%d", c.dialogSeq)
	c.mu.Unlock()
	return ui.OpenConfirmationMsg{
		ID:      id,
		Title:   "Replay finished book",
		Prompt:  fmt.Sprintf("Replay %q from the beginning (00:00:00)? This resets its position and marks it unfinished.", title),
		Keep:    "Cancel",
		Confirm: "Replay from start",
		Action:  ui.Action{Kind: ui.ReplayBook, Key: encodedKey},
	}
}

func (c *Coordinator) playBook(encodedKey string) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.NoticeMsg{Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	if c.player == nil {
		return ui.NoticeMsg{Error: "Playback unavailable."}
	}
	key, err := DecodeBookKey(encodedKey)
	if err != nil {
		return ui.NoticeMsg{Error: SanitizeError(ErrInvalidInput)}
	}
	absPath, title, dur, ready := c.verifyBookMedia(ctx, key)
	if !ready {
		return c.missingAudioPrompt(encodedKey, title)
	}
	if ls, found, err := c.store.ListeningState(ctx, key); err == nil && found && ls.Completion == store.CompletionFinished {
		return c.replayConfirmation(encodedKey, title)
	}
	chapters := c.ensureChapters(ctx, key, absPath)
	snap, err := c.player.Load(ctx, playback.Track{
		Key:      key,
		Title:    title,
		Path:     absPath,
		Duration: dur,
	}, true)
	if err != nil {
		return ui.NoticeMsg{Error: sanitizePlaybackError(err)}
	}
	return c.refreshPlaybackSnapshot(snap, chapters, "Playing "+title)
}

func (c *Coordinator) replayBook(action ui.Action) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	if c.player == nil {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: "Playback unavailable."}
	}
	key, err := DecodeBookKey(action.Key)
	if err != nil {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: SanitizeError(ErrInvalidInput)}
	}
	absPath, title, dur, ready := c.verifyBookMedia(ctx, key)
	if !ready {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: "Local audio file is missing or unsafe."}
	}
	chapters := c.ensureChapters(ctx, key, absPath)
	snap, err := c.player.Replay(ctx, playback.Track{
		Key:      key,
		Title:    title,
		Path:     absPath,
		Duration: dur,
	})
	if err != nil {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: sanitizePlaybackError(err)}
	}
	updated := c.refreshPlaybackSnapshot(snap, chapters, "Replaying "+title)
	return tea.BatchMsg{
		func() tea.Msg { return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt} },
		func() tea.Msg { return updated },
	}
}

type listeningStateWriter interface {
	PutListeningState(ctx context.Context, state store.ListeningState) error
}

func (c *Coordinator) setBookCompletion(encodedKey string, completion store.Completion) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.NoticeMsg{Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	key, err := DecodeBookKey(encodedKey)
	if err != nil {
		return ui.NoticeMsg{Error: SanitizeError(ErrInvalidInput)}
	}
	activity := "Marked unfinished"
	if completion == store.CompletionFinished {
		activity = "Marked finished"
	}

	if c.player != nil {
		snap, err := c.player.SetCompletion(ctx, key, completion)
		if err != nil {
			return ui.NoticeMsg{Error: sanitizePlaybackError(err)}
		}
		if snap.Book != nil && *snap.Book == key {
			return c.refreshPlaybackSnapshot(snap, c.cachedChaptersFor(snap.Book), activity)
		}
	} else if writer, ok := c.store.(listeningStateWriter); ok {
		posNS := int64(0)
		speedMilli := playback.DefaultSpeedMilli
		if saved, found, err := c.store.ListeningState(ctx, key); err != nil {
			return ui.NoticeMsg{Error: SanitizeError(err)}
		} else if found {
			posNS = max(saved.PositionNS, 0)
			if saved.SpeedMilli > 0 {
				speedMilli = saved.SpeedMilli
			}
		}
		if err := writer.PutListeningState(ctx, store.ListeningState{
			Key:         key,
			PositionNS:  posNS,
			SpeedMilli:  speedMilli,
			Completion:  completion,
			UpdatedAtMS: max(c.clock().UnixMilli(), 0),
		}); err != nil {
			return ui.NoticeMsg{Error: SanitizeError(err)}
		}
	} else {
		return ui.NoticeMsg{Error: "Completion persistence unavailable."}
	}

	c.mu.Lock()
	for i := range c.snapshot.Books {
		if c.snapshot.Books[i].Key == encodedKey {
			c.snapshot.Books[i].Finished = completion == store.CompletionFinished
			c.snapshot.Books[i].InProgress = c.snapshot.Books[i].InProgress && completion != store.CompletionFinished
			break
		}
	}
	if c.snapshot.Player.Key == encodedKey && c.snapshot.Player.Session == "" {
		if completion == store.CompletionFinished {
			c.snapshot.Player.State = ui.Finished
		} else {
			c.snapshot.Player.State = ui.Stopped
		}
	}
	c.snapshot.Activity = activity
	out := c.snapshot
	c.mu.Unlock()
	return out
}

func (c *Coordinator) activateChapter(action ui.Action) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.NoticeMsg{Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	if c.player == nil {
		return ui.NoticeMsg{Error: "Playback unavailable."}
	}
	key, err := DecodeBookKey(action.Key)
	if err != nil {
		return ui.NoticeMsg{Error: SanitizeError(ErrInvalidInput)}
	}
	absPath, title, dur, ready := c.verifyBookMedia(ctx, key)
	if !ready {
		return c.missingAudioPrompt(action.Key, title)
	}

	snap := c.player.Snapshot()
	active := snap.State == playback.StatePlaying || snap.State == playback.StatePaused
	if active && snap.Book != nil && *snap.Book == key {
		seeked, err := c.player.SeekAbsolute(ctx, snap.Session, action.ChapterStart)
		if err != nil {
			return ui.NoticeMsg{Error: sanitizePlaybackError(err)}
		}
		chapters := c.ensureChapters(ctx, key, absPath)
		return c.refreshPlaybackSnapshot(seeked, chapters, "Jumped to "+formatClock(action.ChapterStart))
	}

	if active && snap.Book != nil && *snap.Book != key {
		c.mu.Lock()
		c.dialogSeq++
		id := fmt.Sprintf("switch-%d", c.dialogSeq)
		c.mu.Unlock()
		return ui.OpenConfirmationMsg{
			ID:      id,
			Title:   "Switch audiobook",
			Prompt:  fmt.Sprintf("Switch from %q to %q at %s?", snap.Title, title, formatClock(action.ChapterStart)),
			Keep:    "Stay",
			Confirm: "Switch and play",
			Action: ui.Action{
				Kind:         ui.ConfirmSwitchPlay,
				Key:          action.Key,
				Session:      strconv.FormatUint(snap.Session, 10),
				ChapterIndex: action.ChapterIndex,
				ChapterStart: action.ChapterStart,
			},
		}
	}

	chapters := c.ensureChapters(ctx, key, absPath)
	loaded, err := c.player.Load(ctx, playback.Track{
		Key:      key,
		Title:    title,
		Path:     absPath,
		Duration: dur,
	}, true)
	if err != nil {
		return ui.NoticeMsg{Error: sanitizePlaybackError(err)}
	}
	seeked, err := c.player.SeekAbsolute(ctx, loaded.Session, action.ChapterStart)
	if err != nil {
		return ui.NoticeMsg{Error: sanitizePlaybackError(err)}
	}
	return c.refreshPlaybackSnapshot(seeked, chapters, "Playing "+title)
}

func (c *Coordinator) confirmSwitchPlay(action ui.Action) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	if c.player == nil {
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
	absPath, title, dur, ready := c.verifyBookMedia(ctx, key)
	if !ready {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: "Local audio file is missing or unsafe."}
	}
	chapters := c.ensureChapters(ctx, key, absPath)
	loaded, err := c.player.Load(ctx, playback.Track{
		Key:      key,
		Title:    title,
		Path:     absPath,
		Duration: dur,
	}, true)
	if err != nil {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: sanitizePlaybackError(err)}
	}
	seeked, err := c.player.SeekAbsolute(ctx, loaded.Session, action.ChapterStart)
	if err != nil {
		return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt, Error: sanitizePlaybackError(err)}
	}
	updated := c.refreshPlaybackSnapshot(seeked, chapters, "Switched to "+title)
	return tea.BatchMsg{
		func() tea.Msg { return ui.ModalResultMsg{ID: action.RequestID, Attempt: action.Attempt} },
		func() tea.Msg { return updated },
	}
}

func (c *Coordinator) togglePlayback(sessionStr string) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.NoticeMsg{Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	if c.player == nil {
		return ui.NoticeMsg{Error: "Playback unavailable."}
	}
	session, err := strconv.ParseUint(sessionStr, 10, 64)
	if err != nil || session == 0 {
		return ui.NoticeMsg{Error: "No active playback session."}
	}
	snap, err := c.player.Toggle(ctx, session)
	if err != nil {
		return ui.NoticeMsg{Error: sanitizePlaybackError(err)}
	}
	return c.refreshPlaybackSnapshot(snap, c.cachedChaptersFor(snap.Book), "")
}

func (c *Coordinator) seekRelative(sessionStr string, delta time.Duration) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.NoticeMsg{Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	if c.player == nil {
		return ui.NoticeMsg{Error: "Playback unavailable."}
	}
	session, err := strconv.ParseUint(sessionStr, 10, 64)
	if err != nil || session == 0 {
		return ui.NoticeMsg{Error: "No active playback session."}
	}
	snap, err := c.player.SeekRelative(ctx, session, delta)
	if err != nil {
		return ui.NoticeMsg{Error: sanitizePlaybackError(err)}
	}
	return c.refreshPlaybackSnapshot(snap, c.cachedChaptersFor(snap.Book), "")
}

func (c *Coordinator) stepSpeed(sessionStr string, deltaMilli int64) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.NoticeMsg{Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	if c.player == nil {
		return ui.NoticeMsg{Error: "Playback unavailable."}
	}
	session, err := strconv.ParseUint(sessionStr, 10, 64)
	if err != nil || session == 0 {
		return ui.NoticeMsg{Error: "No active playback session."}
	}
	snap, err := c.player.StepSpeed(ctx, session, deltaMilli)
	if err != nil {
		return ui.NoticeMsg{Error: sanitizePlaybackError(err)}
	}
	return c.refreshPlaybackSnapshot(snap, c.cachedChaptersFor(snap.Book), "")
}

func (c *Coordinator) stepVolume(delta int) tea.Msg {
	ctx, ok := c.beginWorker()
	if !ok {
		return ui.NoticeMsg{Error: SanitizeError(ErrClosed)}
	}
	defer c.wg.Done()

	if c.player == nil {
		return ui.NoticeMsg{Error: "Playback unavailable."}
	}
	snap, err := c.player.StepVolume(ctx, delta)
	if err != nil {
		return ui.NoticeMsg{Error: sanitizePlaybackError(err)}
	}
	c.mu.Lock()
	nextSettings := c.settings
	nextSettings.VolumePercent = snap.VolumePercent
	c.mu.Unlock()
	if err := c.store.PutSettings(ctx, nextSettings); err != nil {
		return ui.NoticeMsg{Error: SanitizeError(err)}
	}
	c.mu.Lock()
	c.settings = nextSettings
	c.mu.Unlock()
	return c.refreshPlaybackSnapshot(snap, c.cachedChaptersFor(snap.Book), "")
}

// PollPlayback refreshes active playback position and chapter state.
func (c *Coordinator) PollPlayback(ctx context.Context) (ui.SnapshotMsg, bool) {
	if c == nil || c.player == nil || ctx == nil {
		return ui.SnapshotMsg{}, false
	}
	snap, err := c.player.Poll(ctx)
	if err != nil {
		return ui.SnapshotMsg{}, false
	}
	return c.refreshPlaybackSnapshot(snap, c.cachedChaptersFor(snap.Book), ""), true
}

func (c *Coordinator) cachedChaptersFor(book *store.BookKey) []Chapter {
	if book == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chapters[*book]
}

func (c *Coordinator) refreshPlaybackSnapshot(snap playback.Snapshot, chapters []Chapter, activity string) ui.SnapshotMsg {
	player := projectActivePlayer(snap, chapters)
	c.mu.Lock()
	c.snapshot.Player = player
	if activity != "" {
		c.snapshot.Activity = activity
	}
	if snap.Book != nil {
		if encoded, err := EncodeBookKey(*snap.Book); err == nil {
			titles, starts := splitChapters(chapters)
			for i := range c.snapshot.Books {
				if c.snapshot.Books[i].Key == encoded {
					c.snapshot.Books[i].Finished = snap.Completion == store.CompletionFinished
					c.snapshot.Books[i].InProgress = snap.Position > 0 && snap.Completion != store.CompletionFinished
					if snap.State == playback.StatePlaying || snap.State == playback.StatePaused {
						c.snapshot.Books[i].RecentAtMS = max(c.snapshot.Books[i].RecentAtMS, c.clock().UnixMilli())
					}
					if len(chapters) > 0 {
						c.snapshot.Books[i].Chapters = slices.Clone(titles)
						c.snapshot.Books[i].ChapterStarts = slices.Clone(starts)
					}
					break
				}
			}
		}
	}
	out := c.snapshot
	c.mu.Unlock()
	return out
}

func projectActivePlayer(snap playback.Snapshot, chapters []Chapter) ui.Player {
	if snap.Book == nil {
		return ui.Player{}
	}
	encoded, err := EncodeBookKey(*snap.Book)
	if err != nil {
		return ui.Player{}
	}
	var state ui.PlaybackState
	switch snap.State {
	case playback.StatePlaying:
		state = ui.Playing
	case playback.StatePaused:
		if snap.Completion == store.CompletionFinished && snap.Duration > 0 && snap.Position >= snap.Duration {
			state = ui.Finished
		} else {
			state = ui.Paused
		}
	case playback.StateFailed:
		state = ui.PlaybackFailed
	default:
		state = ui.Stopped
	}
	session := ""
	if snap.Session > 0 && (state == ui.Playing || state == ui.Paused) {
		session = strconv.FormatUint(snap.Session, 10)
	}
	chapterTitle := ""
	var chapterEnd time.Duration
	var ticks []time.Duration
	if len(chapters) > 0 {
		ticks = make([]time.Duration, len(chapters))
		activeIdx := -1
		for i, ch := range chapters {
			ticks[i] = ch.Start
			if snap.Position >= ch.Start {
				activeIdx = i
			}
		}
		if activeIdx >= 0 {
			ch := chapters[activeIdx]
			chapterTitle = ch.Title
			switch {
			case ch.End > ch.Start:
				chapterEnd = ch.End
			case activeIdx+1 < len(chapters) && chapters[activeIdx+1].Start > ch.Start:
				chapterEnd = chapters[activeIdx+1].Start
			case snap.Duration > ch.Start:
				chapterEnd = snap.Duration
			}
		}
	}
	return ui.Player{
		Key:          encoded,
		Session:      session,
		Title:        snap.Title,
		Chapter:      chapterTitle,
		State:        state,
		Position:     snap.Position,
		Duration:     snap.Duration,
		ChapterEnd:   chapterEnd,
		ChapterTicks: ticks,
		Speed:        float64(snap.SpeedMilli) / float64(playback.DefaultSpeedMilli),
		Volume:       snap.VolumePercent,
		VolumeKnown:  true,
	}
}

func formatClock(d time.Duration) string {
	seconds := int64(max(0, d) / time.Second)
	return fmt.Sprintf("%02d:%02d:%02d", seconds/3600, seconds/60%60, seconds%60)
}

func sanitizePlaybackError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, playback.ErrRuntimeMissing):
		return "mpv runtime is missing."
	case errors.Is(err, playback.ErrIPC):
		return "Playback process failed."
	case errors.Is(err, playback.ErrStaleSession):
		return "Playback session changed; retry action."
	case errors.Is(err, playback.ErrNoActiveTrack):
		return "No active playback track."
	default:
		return SanitizeError(err)
	}
}
