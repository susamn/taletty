// Package playback implements Taletty's serialized playback authority and owned Linux mpv adapter.
//
// Socketpair fd://3 IPC launch, named loadfile command, and bounded JSON-IPC
// correlation patterns are adapted from podmutt (MIT License, Copyright (c) podmutt contributors).
package playback

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	defaultStartTimeout = 5 * time.Second
	defaultRPCTimeout   = 2 * time.Second
	defaultStopTimeout  = 500 * time.Millisecond
	maxIPCLineBytes     = 256 * 1024
)

var (
	// ErrRuntimeMissing reports that the mpv executable could not be found.
	ErrRuntimeMissing = errors.New("mpv runtime missing")
	// ErrIPC reports a sanitized mpv process or JSON-IPC failure.
	ErrIPC = errors.New("mpv ipc failure")
)

var allowedMPVEnv = map[string]bool{
	"PATH":                     true,
	"HOME":                     true,
	"USER":                     true,
	"LOGNAME":                  true,
	"TMPDIR":                   true,
	"LANG":                     true,
	"TZ":                       true,
	"TERM":                     true,
	"XDG_RUNTIME_DIR":          true,
	"XDG_DATA_DIRS":            true,
	"XDG_CONFIG_DIRS":          true,
	"DBUS_SESSION_BUS_ADDRESS": true,
	"DISPLAY":                  true,
	"WAYLAND_DISPLAY":          true,
	"SDL_AUDIODRIVER":          true,
	"GORACE":                   true,
	"ASAN_OPTIONS":             true,
	"TSAN_OPTIONS":             true,
}

var allowedMPVEnvPrefixes = []string{
	"LC_",
	"PULSE_",
	"PIPEWIRE_",
	"ALSA_",
}

// Backend abstracts the underlying media playback engine for serialized playback control.
type Backend interface {
	Start(ctx context.Context, mediaPath string, startPos time.Duration, speedMilli int64, volume int, paused bool) error
	SetPause(ctx context.Context, paused bool) error
	SeekExact(ctx context.Context, target time.Duration) error
	SetSpeed(ctx context.Context, speedMilli int64) error
	SetVolume(ctx context.Context, volume int) error
	Position(ctx context.Context) (pos, dur time.Duration, paused, eof bool, err error)
	Stop(ctx context.Context) error
}

// MPVConfig configures an owned Linux mpv subprocess backend.
type MPVConfig struct {
	Binary       string
	ExtraArgs    []string
	StartTimeout time.Duration
	RPCTimeout   time.Duration
	StopTimeout  time.Duration
}

// MPVProcess manages a single owned mpv subprocess over a private inherited socketpair.
type MPVProcess struct {
	cfg MPVConfig

	mu     sync.Mutex
	cmd    *exec.Cmd
	exited <-chan error
	client *ipcClient
}

// NewMPVProcess creates an MPVProcess configured with cfg.
func NewMPVProcess(cfg MPVConfig) *MPVProcess {
	if cfg.Binary == "" {
		cfg.Binary = "mpv"
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = defaultStartTimeout
	}
	if cfg.RPCTimeout <= 0 {
		cfg.RPCTimeout = defaultRPCTimeout
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = defaultStopTimeout
	}
	return &MPVProcess{cfg: cfg}
}

func validateMediaPath(mediaPath string) error {
	if !utf8.ValidString(mediaPath) || !filepath.IsAbs(mediaPath) || filepath.Clean(mediaPath) != mediaPath {
		return ErrInvalidInput
	}
	for _, r := range mediaPath {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ErrInvalidInput
		}
	}
	if !strings.EqualFold(filepath.Ext(mediaPath), ".m4b") {
		return ErrInvalidInput
	}
	base := filepath.Base(mediaPath)
	if len(base) <= len(".m4b") || strings.HasPrefix(base, ".") {
		return ErrInvalidInput
	}
	return nil
}

func cleanMPVEnv(environ []string) []string {
	cleaned := make([]string, 0, len(environ))
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		if allowedMPVEnv[key] {
			cleaned = append(cleaned, entry)
			continue
		}
		for _, prefix := range allowedMPVEnvPrefixes {
			if strings.HasPrefix(key, prefix) {
				cleaned = append(cleaned, entry)
				break
			}
		}
	}
	return cleaned
}

// Start launches mpv in its own process group with an inherited CLOEXEC socketpair
// on fd://3 (--idle=yes --keep-open=yes) and loads mediaPath via named loadfile IPC.
// Closing the parent socketpair endpoint on crash or SIGKILL closes fd://3 so mpv exits.
func (p *MPVProcess) Start(ctx context.Context, mediaPath string, startPos time.Duration, speedMilli int64, volume int, paused bool) error {
	if ctx == nil {
		return ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateMediaPath(mediaPath); err != nil {
		return err
	}
	if speedMilli < MinSpeedMilli || speedMilli > MaxSpeedMilli {
		return ErrInvalidInput
	}
	if err := p.Stop(ctx); err != nil {
		return err
	}

	binary, err := exec.LookPath(p.cfg.Binary)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s not found", ErrRuntimeMissing, filepath.Base(p.cfg.Binary))
		}
		return fmt.Errorf("%w: unable to resolve mpv binary", ErrIPC)
	}

	// Adapted from podmutt (MIT License, Copyright (c) podmutt contributors):
	// prevent concurrent Go exec calls from inheriting either endpoint before CLOEXEC.
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return fmt.Errorf("%w: create ipc socketpair", ErrIPC)
	}

	parent := os.NewFile(uintptr(fds[0]), "mpv-parent")
	child := os.NewFile(uintptr(fds[1]), "mpv-child")
	defer parent.Close()
	defer child.Close()

	conn, err := net.FileConn(parent)
	if err != nil {
		return fmt.Errorf("%w: wrap ipc socket", ErrIPC)
	}

	if startPos < 0 {
		startPos = 0
	}
	volume = clampVolume(volume)
	pauseFlag := "--pause=no"
	if paused {
		pauseFlag = "--pause=yes"
	}

	args := []string{
		"--no-config",
		"--no-terminal",
		"--no-video",
		"--audio-display=no",
		"--load-scripts=no",
		"--ytdl=no",
		"--idle=yes",
		"--keep-open=yes",
		"--input-ipc-client=fd://3",
		"--speed=" + strconv.FormatFloat(float64(speedMilli)/float64(DefaultSpeedMilli), 'f', 3, 64),
		"--volume=" + strconv.Itoa(volume),
		pauseFlag,
	}
	args = append(args, p.cfg.ExtraArgs...)

	cmd := exec.Command(binary, args...)
	cmd.Env = cleanMPVEnv(os.Environ())
	cmd.ExtraFiles = []*os.File{child} // Child receives fd 3; no filesystem socket.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Leave Stdin/Stdout/Stderr nil so os/exec attaches /dev/null directly.

	if err := cmd.Start(); err != nil {
		_ = conn.Close()
		return fmt.Errorf("%w: start mpv process", ErrIPC)
	}
	// Close local originals immediately so only conn and the child hold endpoints.
	_ = parent.Close()
	_ = child.Close()

	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() {
		exited <- observeExit(pid)
	}()

	client := newIPCClient(conn)
	p.mu.Lock()
	p.cmd = cmd
	p.exited = exited
	p.client = client
	p.mu.Unlock()

	startCtx, cancel := context.WithTimeout(ctx, p.cfg.StartTimeout)
	defer cancel()

	if err := p.startMedia(ctx, startCtx, client, mediaPath, startPos, paused); err != nil {
		_ = p.Stop(context.Background())
		return err
	}
	return nil
}

func (p *MPVProcess) startMedia(ctx, startCtx context.Context, client *ipcClient, mediaPath string, startPos time.Duration, paused bool) error {
	if _, err := client.call(startCtx, p.cfg.StartTimeout, []any{"get_property", "mpv-version"}); err != nil {
		return startupError(ctx, startCtx, err)
	}

	loadCh := client.armLoadWait()
	pauseOpt := "no"
	if paused {
		pauseOpt = "yes"
	}
	// Named arguments avoid the loadfile positional-argument change in mpv 0.38.
	loadCmd := map[string]any{
		"name":  "loadfile",
		"url":   mediaPath,
		"flags": "replace",
		"options": map[string]string{
			"start": strconv.FormatFloat(startPos.Seconds(), 'f', 3, 64),
			"pause": pauseOpt,
		},
	}
	if _, err := client.call(startCtx, p.cfg.StartTimeout, loadCmd); err != nil {
		return startupError(ctx, startCtx, err)
	}

	select {
	case err := <-loadCh:
		if err != nil {
			return startupError(ctx, startCtx, err)
		}
	case <-startCtx.Done():
		return startupError(ctx, startCtx, startCtx.Err())
	}

	if err := waitPositionReady(startCtx, client, p.cfg.RPCTimeout); err != nil {
		return startupError(ctx, startCtx, err)
	}
	return nil
}

func startupError(ctx, startCtx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) && startCtx.Err() != nil {
		return fmt.Errorf("%w: mpv startup timed out", ErrIPC)
	}
	return err
}

func waitPositionReady(ctx context.Context, client *ipcClient, rpcTimeout time.Duration) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := client.loadFailure(); err != nil {
			return err
		}
		raw, err := client.call(ctx, rpcTimeout, []any{"get_property", "time-pos"})
		if err == nil {
			if string(raw) != "null" {
				if _, decErr := decodeSeconds(raw); decErr != nil {
					return fmt.Errorf("%w: invalid initial position", ErrIPC)
				}
				return nil
			}
		} else {
			if loadErr := client.loadFailure(); loadErr != nil {
				return loadErr
			}
			var cmdErr *ipcCommandError
			if !errors.As(err, &cmdErr) || cmdErr.msg != "property unavailable" {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Observe without reaping: only Stop may release the PID anchor via cmd.Wait.
func observeExit(pid int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err != unix.EINTR {
			return err
		}
	}
}

func (p *MPVProcess) currentClient() (*ipcClient, error) {
	p.mu.Lock()
	client := p.client
	p.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("%w: not running", ErrIPC)
	}
	return client, nil
}

func (p *MPVProcess) call(ctx context.Context, command ...any) (json.RawMessage, error) {
	if ctx == nil {
		return nil, ErrInvalidInput
	}
	client, err := p.currentClient()
	if err != nil {
		return nil, err
	}
	return client.call(ctx, p.cfg.RPCTimeout, command)
}

// SetPause updates mpv's pause state.
func (p *MPVProcess) SetPause(ctx context.Context, paused bool) error {
	_, err := p.call(ctx, "set_property", "pause", paused)
	return err
}

// SeekExact seeks to target using absolute+exact mode without altering pause state.
func (p *MPVProcess) SeekExact(ctx context.Context, target time.Duration) error {
	if target < 0 {
		target = 0
	}
	_, err := p.call(ctx, "seek", target.Seconds(), "absolute+exact")
	return err
}

// SetSpeed updates mpv's playback speed from milli-rate units within [MinSpeedMilli, MaxSpeedMilli].
func (p *MPVProcess) SetSpeed(ctx context.Context, speedMilli int64) error {
	if speedMilli < MinSpeedMilli || speedMilli > MaxSpeedMilli {
		return ErrInvalidInput
	}
	_, err := p.call(ctx, "set_property", "speed", float64(speedMilli)/float64(DefaultSpeedMilli))
	return err
}

// SetVolume updates mpv's volume percentage (0..100).
func (p *MPVProcess) SetVolume(ctx context.Context, volume int) error {
	_, err := p.call(ctx, "set_property", "volume", clampVolume(volume))
	return err
}

// Position queries current playback position, duration, pause state and EOF status.
func (p *MPVProcess) Position(ctx context.Context) (time.Duration, time.Duration, bool, bool, error) {
	posRaw, err := p.call(ctx, "get_property", "time-pos")
	if err != nil {
		return 0, 0, false, false, err
	}
	pos, err := decodeSeconds(posRaw)
	if err != nil {
		return 0, 0, false, false, err
	}

	durRaw, err := p.call(ctx, "get_property", "duration")
	if err != nil {
		return 0, 0, false, false, err
	}
	dur, err := decodeOptionalSeconds(durRaw)
	if err != nil {
		return 0, 0, false, false, err
	}

	pauseRaw, err := p.call(ctx, "get_property", "pause")
	if err != nil {
		return 0, 0, false, false, err
	}
	var paused bool
	if err := json.Unmarshal(pauseRaw, &paused); err != nil {
		return 0, 0, false, false, fmt.Errorf("%w: invalid pause property", ErrIPC)
	}

	eofRaw, err := p.call(ctx, "get_property", "eof-reached")
	if err != nil {
		return 0, 0, false, false, err
	}
	var eof bool
	if err := json.Unmarshal(eofRaw, &eof); err != nil {
		return 0, 0, false, false, fmt.Errorf("%w: invalid eof property", ErrIPC)
	}

	return pos, dur, paused, eof, nil
}

// Stop sends quit over IPC, closes the socketpair, kills residual process-group
// members while waitid(WNOWAIT) keeps the PID reserved, and reaps the child via cmd.Wait.
func (p *MPVProcess) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.Lock()
	cmd := p.cmd
	exited := p.exited
	client := p.client
	p.cmd = nil
	p.exited = nil
	p.client = nil
	p.mu.Unlock()

	if cmd == nil && client == nil {
		return nil
	}

	if client != nil {
		if ctx.Err() == nil {
			quitTimeout := min(200*time.Millisecond, p.cfg.StopTimeout)
			quitCtx, cancel := context.WithTimeout(ctx, quitTimeout)
			_, _ = client.call(quitCtx, quitTimeout, []any{"quit"})
			cancel()
		}
		client.close()
	}

	if cmd != nil && cmd.Process != nil && exited != nil {
		pid := cmd.Process.Pid
		sawExit := false
		if ctx.Err() == nil {
			timer := time.NewTimer(p.cfg.StopTimeout)
			select {
			case <-exited:
				sawExit = true
				timer.Stop()
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
			}
		}
		// Always kill residual group members BEFORE reaping the leader. WNOWAIT
		// keeps its PID reserved, so cleanup cannot signal a reused process group.
		_ = unix.Kill(-pid, unix.SIGKILL)
		if !sawExit {
			<-exited
		}
		_ = cmd.Wait()
	}

	return ctx.Err()
}

func decodeSeconds(raw json.RawMessage) (time.Duration, error) {
	var secs float64
	if err := json.Unmarshal(raw, &secs); err != nil || math.IsNaN(secs) || math.IsInf(secs, 0) || secs < 0 {
		return 0, fmt.Errorf("%w: invalid seconds value", ErrIPC)
	}
	ns := secs * float64(time.Second)
	if ns > float64(math.MaxInt64) {
		return 0, fmt.Errorf("%w: seconds overflow", ErrIPC)
	}
	return time.Duration(math.Round(ns)), nil
}

func decodeOptionalSeconds(raw json.RawMessage) (time.Duration, error) {
	if string(raw) == "null" {
		return 0, nil
	}
	return decodeSeconds(raw)
}

type ipcCommandError struct {
	msg string
}

func (e *ipcCommandError) Error() string { return ErrIPC.Error() + ": command failed" }
func (e *ipcCommandError) Unwrap() error { return ErrIPC }

type ipcReply struct {
	data   json.RawMessage
	errStr string
	err    error
}

type ipcFrame struct {
	RequestID uint64          `json:"request_id"`
	Error     string          `json:"error"`
	Data      json.RawMessage `json:"data"`
	Event     string          `json:"event"`
	Reason    string          `json:"reason"`
}

type ipcClient struct {
	conn      net.Conn
	writeGate chan struct{}

	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan ipcReply
	loadCh  chan error
	loadErr error
	err     error
	closed  bool
	done    chan struct{}
}

func newIPCClient(conn net.Conn) *ipcClient {
	c := &ipcClient{
		conn:      conn,
		writeGate: make(chan struct{}, 1),
		pending:   make(map[uint64]chan ipcReply),
		done:      make(chan struct{}),
	}
	go c.readLoop()
	return c
}

func (c *ipcClient) armLoadWait() <-chan error {
	ch := make(chan error, 1)
	c.mu.Lock()
	if c.err != nil {
		ch <- c.err
	} else if c.loadErr != nil {
		ch <- c.loadErr
	} else {
		c.loadCh = ch
	}
	c.mu.Unlock()
	return ch
}

func (c *ipcClient) loadFailure() error {
	c.mu.Lock()
	err := c.loadErr
	c.mu.Unlock()
	return err
}

func (c *ipcClient) handleEvent(event, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch event {
	case "file-loaded":
		if c.loadCh != nil {
			c.loadCh <- nil
			c.loadCh = nil
		}
	case "end-file":
		if c.loadCh != nil {
			c.loadCh <- fmt.Errorf("%w: media load failed", ErrIPC)
			c.loadCh = nil
		} else if reason == "error" && c.loadErr == nil {
			c.loadErr = fmt.Errorf("%w: media playback failed", ErrIPC)
		}
	}
}

func (c *ipcClient) readLoop() {
	defer close(c.done)
	scanner := bufio.NewScanner(c.conn)
	scanner.Buffer(make([]byte, 4096), maxIPCLineBytes)
	for scanner.Scan() {
		var frame ipcFrame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			c.fail(fmt.Errorf("%w: malformed ipc line", ErrIPC))
			return
		}
		if frame.RequestID == 0 {
			c.handleEvent(frame.Event, frame.Reason)
			continue
		}
		c.mu.Lock()
		if ch, ok := c.pending[frame.RequestID]; ok {
			delete(c.pending, frame.RequestID)
			ch <- ipcReply{data: frame.Data, errStr: frame.Error}
		}
		c.mu.Unlock()
	}
	if scanner.Err() != nil {
		c.fail(fmt.Errorf("%w: ipc stream error", ErrIPC))
		return
	}
	c.fail(fmt.Errorf("%w: ipc connection closed", ErrIPC))
}

func (c *ipcClient) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	pending := c.pending
	c.pending = make(map[uint64]chan ipcReply)
	loadCh := c.loadCh
	c.loadCh = nil
	c.mu.Unlock()

	if loadCh != nil {
		loadCh <- err
	}
	for _, ch := range pending {
		ch <- ipcReply{err: err}
	}
	_ = c.conn.Close()
}

func (c *ipcClient) close() {
	c.mu.Lock()
	already := c.closed
	c.closed = true
	if c.err == nil {
		c.err = fmt.Errorf("%w: ipc closed", ErrIPC)
	}
	err := c.err
	pending := c.pending
	c.pending = make(map[uint64]chan ipcReply)
	loadCh := c.loadCh
	c.loadCh = nil
	c.mu.Unlock()

	if !already {
		if loadCh != nil {
			loadCh <- err
		}
		for _, ch := range pending {
			ch <- ipcReply{err: err}
		}
		_ = c.conn.Close()
	}
	<-c.done
}

func (c *ipcClient) call(ctx context.Context, timeout time.Duration, command any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	c.nextID++
	id := c.nextID
	ch := make(chan ipcReply, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	payload, err := json.Marshal(map[string]any{
		"command":    command,
		"request_id": id,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: encode command", ErrIPC)
	}
	payload = append(payload, '\n')

	select {
	case c.writeGate <- struct{}{}:
	case <-callCtx.Done():
		return nil, callCtx.Err()
	}
	if err := callCtx.Err(); err != nil {
		<-c.writeGate
		return nil, err
	}
	if deadline, ok := callCtx.Deadline(); ok {
		_ = c.conn.SetWriteDeadline(deadline)
	}
	_, writeErr := c.conn.Write(payload)
	<-c.writeGate
	if writeErr != nil {
		err := fmt.Errorf("%w: write ipc command", ErrIPC)
		c.fail(err)
		if callCtx.Err() != nil {
			return nil, callCtx.Err()
		}
		return nil, err
	}

	select {
	case rep := <-ch:
		if rep.err != nil {
			return nil, rep.err
		}
		if rep.errStr != "success" {
			return nil, &ipcCommandError{msg: rep.errStr}
		}
		return rep.data, nil
	case <-callCtx.Done():
		return nil, callCtx.Err()
	}
}
