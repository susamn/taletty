package playback

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/susamn/taletty/app/internal/store"
	"golang.org/x/sys/unix"
)

func requireRealMediaTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"mpv", "ffmpeg"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("TALETTY_REQUIRE_MEDIA_TOOLS") == "1" {
				t.Fatalf("mandatory playback media tool %s missing", tool)
			}
			t.Skipf("local media tool %s absent; mandatory in CI", tool)
		}
	}
}

func generateChapteredM4B(t *testing.T, dir, name string, seconds int) string {
	t.Helper()
	metaPath := filepath.Join(dir, name+".ffmeta")
	halfMS := seconds * 500
	totalMS := seconds * 1000
	meta := fmt.Sprintf(";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=%d\ntitle=Opening\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=%d\nEND=%d\ntitle=Chapter Two\n",
		halfMS, halfMS, totalMS)
	must(t, os.WriteFile(metaPath, []byte(meta), 0o600))

	outPath := filepath.Join(dir, name+".m4b")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "anullsrc=r=8000:cl=mono",
		"-i", metaPath,
		"-map_metadata", "1",
		"-t", strconv.Itoa(seconds),
		"-c:a", "aac",
		"-f", "mp4",
		"-y", outPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg generate %s failed: %v: %s", name, err, out)
	}
	return outPath
}

// TestMPVRealParentKillProbe launches real mpv --ao=null and waits to be SIGKILLed.
func TestMPVRealParentKillProbe(t *testing.T) {
	sep := slices.Index(os.Args, "--")
	if sep < 0 || len(os.Args) <= sep+2 || os.Args[sep+1] != "taletty-real-parent-probe" {
		return
	}
	media := os.Args[sep+2]
	proc := NewMPVProcess(MPVConfig{
		ExtraArgs:    []string{"--ao=null"},
		StartTimeout: 10 * time.Second,
		RPCTimeout:   2 * time.Second,
	})
	if err := proc.Start(context.Background(), media, 0, DefaultSpeedMilli, 80, true); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(proc.cmd.Process.Pid)
	select {}
}

func TestRealMPVLifecycleAndControls(t *testing.T) {
	requireRealMediaTools(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s := openTestStore(t)
	dir := t.TempDir()
	mediaA := generateChapteredM4B(t, dir, "bookA", 4)
	mediaB := generateChapteredM4B(t, dir, "bookB", 2)

	bookA := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000001"}
	bookB := store.BookKey{Provider: "audible", Namespace: "us", ID: "B000000002"}

	must(t, s.PutListeningState(ctx, store.ListeningState{
		Key:         bookA,
		PositionNS:  int64(1 * time.Second),
		SpeedMilli:  1_250_000,
		Completion:  store.CompletionUnfinished,
		UpdatedAtMS: 1000,
	}))

	proc := NewMPVProcess(MPVConfig{
		ExtraArgs:    []string{"--ao=null"},
		StartTimeout: 10 * time.Second,
		RPCTimeout:   2 * time.Second,
		StopTimeout:  time.Second,
	})
	ctrl, err := NewController(proc, Options{Store: s})
	must(t, err)
	defer func() { _ = ctrl.Close(context.Background()) }()

	// Load bookA paused at saved 1s position and 1.25x speed.
	snap, err := ctrl.Load(ctx, Track{Key: bookA, Title: "Book A", Path: mediaA}, false)
	must(t, err)
	if snap.State != StatePaused || snap.Session != 1 || snap.SpeedMilli != 1_250_000 {
		t.Fatalf("unexpected initial load snapshot: %+v", snap)
	}

	// Paused exact seek preserves pause state and moves position near 2.5s.
	snap, err = ctrl.SeekAbsolute(ctx, snap.Session, 2500*time.Millisecond)
	must(t, err)
	if snap.State != StatePaused {
		t.Fatalf("expected StatePaused after seek, got %v", snap.State)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		pos, dur, paused, _, err := proc.Position(ctx)
		must(t, err)
		if !paused {
			t.Fatal("paused exact seek unpaused real mpv")
		}
		if dur > 3*time.Second && pos >= 2200*time.Millisecond && pos <= 2800*time.Millisecond {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("real mpv seek did not settle near 2.5s: pos=%v dur=%v", pos, dur)
		}
		time.Sleep(15 * time.Millisecond)
	}

	// Speed and volume updates while paused.
	snap, err = ctrl.SetSpeed(ctx, snap.Session, 1_500_000)
	must(t, err)
	snap, err = ctrl.SetVolume(ctx, 65)
	must(t, err)
	if snap.SpeedMilli != 1_500_000 || snap.VolumePercent != 65 || snap.State != StatePaused {
		t.Fatalf("unexpected speed/volume snapshot: %+v", snap)
	}

	// Play and Pause.
	snap, err = ctrl.Play(ctx, snap.Session)
	must(t, err)
	if snap.State != StatePlaying {
		t.Fatalf("expected StatePlaying, got %v", snap.State)
	}
	snap, err = ctrl.Poll(ctx)
	must(t, err)
	if snap.State != StatePlaying {
		t.Fatalf("expected real mpv playing on Poll, got %+v", snap)
	}
	snap, err = ctrl.Pause(ctx, snap.Session)
	must(t, err)
	if snap.State != StatePaused {
		t.Fatalf("expected StatePaused after Pause, got %v", snap.State)
	}

	// Switch to bookB: saves bookA checkpoint, increments Session, rejects stale session.
	oldSession := snap.Session
	snap, err = ctrl.Load(ctx, Track{Key: bookB, Title: "Book B", Path: mediaB}, true)
	must(t, err)
	if snap.Session != oldSession+1 || snap.State != StatePlaying {
		t.Fatalf("unexpected bookB snapshot: %+v", snap)
	}
	if _, err := ctrl.Pause(ctx, oldSession); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("expected ErrStaleSession for old session, got %v", err)
	}

	savedA, found, err := s.ListeningState(ctx, bookA)
	must(t, err)
	if !found || savedA.SpeedMilli != 1_500_000 || savedA.PositionNS < int64(2*time.Second) {
		t.Fatalf("bookA checkpoint not persisted on switch: found=%v %+v", found, savedA)
	}

	// Poll duration, seek near end, and wait for natural EOF transition to StatePaused.
	snap, err = ctrl.Poll(ctx)
	must(t, err)
	if snap.Duration <= 0 {
		t.Fatalf("expected positive duration from real mpv, got %v", snap.Duration)
	}
	nearEnd := max(0, snap.Duration-300*time.Millisecond)
	snap, err = ctrl.SeekAbsolute(ctx, snap.Session, nearEnd)
	must(t, err)

	eofDeadline := time.Now().Add(5 * time.Second)
	for {
		snap, err = ctrl.Poll(ctx)
		must(t, err)
		if snap.State == StatePaused && snap.Position == snap.Duration {
			break
		}
		if time.Now().After(eofDeadline) {
			t.Fatalf("real mpv did not reach EOF in time: %+v", snap)
		}
		time.Sleep(20 * time.Millisecond)
	}

	savedB, found, err := s.ListeningState(ctx, bookB)
	must(t, err)
	if !found || savedB.PositionNS != int64(snap.Duration) {
		t.Fatalf("expected EOF checkpoint at duration %v, got found=%v %+v", snap.Duration, found, savedB)
	}

	// Orderly Close reaps mpv and is idempotent.
	mpvPID := proc.cmd.Process.Pid
	must(t, ctrl.Close(ctx))
	must(t, ctrl.Close(ctx))
	assertReaped(t, mpvPID)
}

func TestRealMPVKilledChildRecovery(t *testing.T) {
	requireRealMediaTools(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s := openTestStore(t)
	media := generateChapteredM4B(t, t.TempDir(), "killed", 4)
	proc := NewMPVProcess(MPVConfig{
		ExtraArgs:    []string{"--ao=null"},
		StartTimeout: 10 * time.Second,
		RPCTimeout:   2 * time.Second,
		StopTimeout:  time.Second,
	})
	ctrl, err := NewController(proc, Options{Store: s})
	must(t, err)
	defer func() { _ = ctrl.Close(context.Background()) }()

	book := store.BookKey{Provider: "local", Namespace: "user", ID: "killed-child"}
	_, err = ctrl.Load(ctx, Track{Key: book, Title: "Killed Child", Path: media}, false)
	must(t, err)

	mpvPID := proc.cmd.Process.Pid
	must(t, unix.Kill(mpvPID, unix.SIGKILL))

	deadline := time.Now().Add(3 * time.Second)
	for {
		snap, pollErr := ctrl.Poll(ctx)
		if pollErr != nil {
			if !errors.Is(pollErr, ErrIPC) || snap.State != StateFailed || snap.Failure != "mpv playback connection failed" {
				t.Fatalf("unexpected killed-child failure: snap=%+v err=%v", snap, pollErr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("controller did not observe killed mpv child")
		}
		time.Sleep(15 * time.Millisecond)
	}
	assertReaped(t, mpvPID)
	must(t, ctrl.Close(ctx))
}

func TestRealMPVKilledParentNoOrphan(t *testing.T) {
	requireRealMediaTools(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	media := generateChapteredM4B(t, t.TempDir(), "orphan-check", 4)
	probe := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMPVRealParentKillProbe$", "--", "taletty-real-parent-probe", media)
	probe.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0")
	stdout, err := probe.StdoutPipe()
	must(t, err)
	must(t, probe.Start())
	defer func() {
		if probe.Process != nil {
			_ = probe.Process.Kill()
		}
		_ = probe.Wait()
	}()

	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatal("probe did not report real mpv PID")
	}
	mpvPID, err := strconv.Atoi(strings.TrimSpace(scanner.Text()))
	must(t, err)
	defer unix.Kill(mpvPID, unix.SIGKILL)

	if err := unix.Kill(mpvPID, 0); err != nil {
		t.Fatalf("expected real mpv PID %d alive before parent kill: %v", mpvPID, err)
	}

	must(t, probe.Process.Kill())
	_ = probe.Wait()
	assertProcessGone(t, mpvPID)
}
