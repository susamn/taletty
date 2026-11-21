package audible

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/susamn/taletty/engine/model"
	"golang.org/x/sys/unix"
)

func publicDownloadFixture(c *Client, mode, barrier string, clear []byte) {
	c.downloadSource = producerSource(false, mode, barrier, clear)
	recoveryMode := "fixture"
	if mode == "real" {
		recoveryMode = "real"
	}
	c.reconcileSource = producerSource(true, recoveryMode, "", nil)
}
func TestActualPublicDownloadSDKRefusalCommitPersistenceAndRestart(t *testing.T) {
	for _, mode := range []string{"success", "denied", "throttle", "auth_failed", "auth_uncertain", "media_uncertain", "nonzero", "truncated", "malformed", "missing", "close"} {
		t.Run(mode, func(t *testing.T) {
			c, root, _ := librarySDK(t)
			publicDownloadFixture(c, mode, "", nil)
			switch mode {
			case "nonzero":
				c.downloadSource += "\nraise SystemExit(7)\n"
			case "malformed":
				c.downloadSource = strings.Replace(c.downloadSource, "code = None  # Library", "result = {'outcome': {'ready': True}}\n                code = None  # Library", 1)
			case "missing":
				if err := os.Remove(filepath.Join(root, "auth.json")); err != nil {
					t.Fatal(err)
				}
			case "auth_uncertain":
				c.downloadSource = strings.Replace(c.downloadSource, "return package\nmain(SOURCES,fixture_load", `original_fsync=os.fsync
    def fsync(fd):
        if os.fstat(fd).st_ino==os.fstat(3).st_ino:raise OSError("fixture-private")
        original_fsync(fd)
    original_save=package.auth_store.save
    def save(fd,payload):
        os.fsync=fsync
        return original_save(fd,payload)
    package.auth_store.save=save
    return package
main(SOURCES,fixture_load`, 1)
			case "media_uncertain":
				c.downloadSource = strings.Replace(c.downloadSource, "return package\nmain(SOURCES,fixture_load", `original_rename=package.media_store._rename
    def rename(directory,source,target):
        original_rename(directory,source,target)
        raise OSError("fixture-private-postcommit")
    package.media_store._rename=rename
    return package
main(SOURCES,fixture_load`, 1)
			case "close":
				c.downloadSource = strings.Replace(c.downloadSource, "return original_sdk(**kwargs,transport=httpx.MockTransport(license_response))", `result=original_sdk(**kwargs,transport=httpx.MockTransport(license_response))
        original_close=result.close
        def close():
            original_close()
            raise RuntimeError("fixture-private-close")
        result.close=close
        return result`, 1)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var events []model.OperationEvent
			result, err := c.Download(ctx, "actual-public", fixtureDownloadBook, func(e model.OperationEvent) { events = append(events, e) })
			expected := map[string]string{"denied": "entitlement_denied", "throttle": "throttled", "auth_failed": "storage_failed", "auth_uncertain": "storage_uncertain", "media_uncertain": "storage_uncertain", "nonzero": "protocol_error", "truncated": "protocol_error", "malformed": "protocol_error", "missing": "not_authenticated", "close": "internal"}[mode]
			if expected == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || failureCode(t, err) != expected || strings.Contains(err.Error(), "fixture-private") {
				t.Fatal("unsafe/wrong public failure", err)
			}
			ready := mode != "denied" && mode != "throttle" && mode != "missing"
			if len(events) < 3 {
				t.Fatal("missing lifecycle events", events)
			}
			for i, e := range events {
				if e.Sequence != uint64(i+1) || e.OperationID != "actual-public" {
					t.Fatal("non-monotonic sequence/id", i, e)
				}
			}
			encodedEvents, _ := json.Marshal(events)
			if strings.Contains(string(encodedEvents), "fixture-private") || strings.Contains(string(encodedEvents), "Atna|") || strings.Contains(string(encodedEvents), "https:") {
				t.Fatal("events leaked provider material", string(encodedEvents))
			}
			terminal := events[len(events)-1]
			if (result.Media != nil) != ready || (result.State == model.MediaReady) != ready || terminal.Download == nil || terminal.Download.State != result.State {
				t.Fatal("independent media facts lost", result, err)
			}
			if mode == "denied" || mode == "throttle" || mode == "missing" {
				if len(events) != 3 || events[0].Type != model.Started || events[1].Type != model.Stage || events[1].Stage != "licensing" || terminal.Type != model.Failed {
					t.Fatal("unexpected refusal/missing event sequence", events)
				}
			} else {
				if len(events) != 7 || events[0].Type != model.Started ||
					events[1].Type != model.Stage || events[1].Stage != "licensing" ||
					events[2].Type != model.Stage || events[2].Stage != "downloading" ||
					events[3].Type != model.Progress || events[3].Bytes == nil || *events[3].Bytes != 15 || events[3].Total == nil || *events[3].Total != 15 ||
					events[4].Type != model.Stage || events[4].Stage != "preparing" ||
					events[5].Type != model.Stage || events[5].Stage != "publishing" {
					t.Fatal("unexpected download event sequence", events)
				}
			}
			if ready {
				target := filepath.Join(root, "downloads", "B000000001.m4b")
				before, readErr := os.ReadFile(target)
				if readErr != nil || string(before) != "synthetic-media" || result.Media.Path != target {
					t.Fatal("public media not usable", readErr)
				}
				recovered := mode == "nonzero" || mode == "truncated" || mode == "malformed"
				if recovered {
					if result.Published != nil || result.Media.Chapters != nil || result.MediaDurability != model.PersistenceUnknown || result.AuthPersistence != model.PersistenceUnknown {
						t.Fatal("invented recovered facts")
					}
				} else if result.Published == nil || !*result.Published || (len(result.Media.Chapters) != 1 && mode != "media_uncertain") {
					t.Fatal("fresh facts lost")
				}
				if mode == "auth_failed" && (result.AuthFailure == nil || result.AuthPersistence != model.PersistenceFailed || result.MediaDurability != model.PersistenceConfirmed) {
					t.Fatal("auth failed erased durable media")
				}
				if mode == "auth_uncertain" && (result.AuthFailure == nil || result.AuthPersistence != model.PersistenceUncertain) {
					t.Fatal("auth durability lost")
				}
				if mode == "media_uncertain" && result.MediaDurability != model.PersistenceUncertain {
					t.Fatal("media durability lost")
				}
				// A new Client/root session, no auth file: public ready reuse must not license or emit stages.
				if err := c.Close(ctx); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(root, "auth.json")); err != nil {
					t.Fatal(err)
				}
				next, openErr := Open(ctx, root, c.python)
				if openErr != nil {
					t.Fatal(openErr)
				}
				defer next.Close(context.Background())
				publicDownloadFixture(next, "success", "", nil)
				var reuseEvents []model.OperationEvent
				reused, reuseErr := next.Download(ctx, "restart", fixtureDownloadBook, func(e model.OperationEvent) { reuseEvents = append(reuseEvents, e) })
				if reuseErr != nil || !reused.Reused || reused.Published == nil || *reused.Published || reused.Media == nil || reused.Media.Chapters != nil || reused.MediaDurability != model.PersistenceUnknown || reused.AuthPersistence != model.PersistenceUnknown {
					t.Fatal("restart relicense/invented hierarchy", reuseErr)
				}
				if len(reuseEvents) != 2 || reuseEvents[0].Type != model.Started || reuseEvents[1].Type != model.Completed || reuseEvents[1].Sequence != 2 {
					t.Fatal("reuse emitted unexpected stages", reuseEvents)
				}
				after, _ := os.ReadFile(target)
				if string(after) != string(before) {
					t.Fatal("restart changed completed artifact")
				}
			}
			if mode != "missing" {
				marker, readErr := os.ReadFile(filepath.Join(root, "fixture-client"))
				if readErr != nil || string(marker) != "S" {
					t.Fatal("recovery/reuse constructed SDK", readErr)
				}
			}
			if mode == "denied" || mode == "throttle" {
				saved, readErr := os.ReadFile(filepath.Join(root, "auth.json"))
				if readErr != nil || !strings.Contains(string(saved), "synthetic-refreshed-not-issued") {
					t.Fatal("refusal lost refresh", readErr)
				}
			}
		})
	}
}

func TestActualPublicDownloadCancelJoinsBeforeRecoveryWithSameGate(t *testing.T) {
	for _, mode := range []string{"precommit", "postcommit"} {
		for _, kill := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/kill=%t", mode, kill), func(t *testing.T) {
				c, root, _ := librarySDK(t)
				barrier := filepath.Join(t.TempDir(), "barrier")
				listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: barrier, Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				listener.SetDeadline(time.Now().Add(8 * time.Second))
				publicDownloadFixture(c, mode, barrier, nil)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan struct{})
				var result model.DownloadResult
				var downloadErr error
				var events []model.OperationEvent
				go func() {
					defer close(done)
					result, downloadErr = c.Download(ctx, "active", fixtureDownloadBook, func(e model.OperationEvent) { events = append(events, e) })
				}()
				joined := false
				defer func() {
					cancel()
					if !joined {
						<-done
					}
				}()
				connection, err := listener.AcceptUnix()
				if err != nil {
					t.Fatal(err)
				}
				defer connection.Close()
				connection.SetDeadline(time.Now().Add(8 * time.Second))
				line, err := bufio.NewReader(connection).ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				pid, err := strconv.Atoi(strings.TrimSpace(line))
				if err != nil || pid <= 1 {
					t.Fatal("invalid barrier pid")
				}
				canceled, stop := context.WithCancel(context.Background())
				stop()
				if err := c.Close(canceled); !errors.Is(err, context.Canceled) {
					t.Fatal("Close stole active root", err)
				}
				// Queued public download and library cancellation starts no extra SDK child.
				for _, download := range []bool{true, false} {
					queued, stop := context.WithCancel(context.Background())
					started := make(chan struct{})
					waiting := make(chan error, 1)
					callback := func(e model.OperationEvent) {
						if e.Type == model.Started {
							close(started)
						}
					}
					go func() {
						var err error
						if download {
							_, err = c.Download(queued, "queued", fixtureDownloadBook, callback)
						} else {
							_, err = c.Library(queued, "queued", callback)
						}
						waiting <- err
					}()
					<-started
					stop()
					if err := <-waiting; !errors.Is(err, context.Canceled) {
						t.Fatal("queued cancel", err)
					}
				}
				if _, err := c.Download(context.Background(), "active", fixtureDownloadBook, nil); failureCode(t, err) != "busy" {
					t.Fatal("duplicate active ID")
				}
				if kill {
					if err := unix.Kill(pid, unix.SIGKILL); err != nil {
						t.Fatal(err)
					}
				} else {
					cancel()
				}
				<-done
				joined = true
				if !errors.Is(unix.Kill(pid, 0), unix.ESRCH) {
					t.Fatal("original child not reaped before return")
				}
				wantLen := 3
				if mode == "postcommit" {
					wantLen = 7
				}
				if len(events) != wantLen {
					t.Fatal("unexpected event count", len(events), events)
				}
				for i, e := range events {
					if e.Sequence != uint64(i+1) {
						t.Fatal("non-monotonic sequence", i, events)
					}
				}
				terminal := events[len(events)-1]
				if terminal.Download == nil || terminal.Download.State != result.State {
					t.Fatal("terminal facts/order", events)
				}
				if mode == "postcommit" {
					if result.State != model.MediaReady || result.Media == nil || result.Media.Chapters != nil || result.Published != nil || result.MediaDurability != model.PersistenceUnknown || result.AuthPersistence != model.PersistenceUnknown {
						t.Fatal("committed artifact/facts lost", result)
					}
					if kill {
						if failureCode(t, downloadErr) != "protocol_error" || terminal.Type != model.Failed {
							t.Fatal(downloadErr)
						}
					} else if downloadErr != nil || terminal.Type != model.Completed {
						t.Fatal("media cancellation beat commit", downloadErr)
					}
				} else {
					if result.State != model.MediaAbsent || result.Media != nil {
						t.Fatal("precommit manufactured media")
					}
					if kill {
						if failureCode(t, downloadErr) != "protocol_error" {
							t.Fatal(downloadErr)
						}
					} else if !errors.Is(downloadErr, context.Canceled) || terminal.Type != model.Canceled {
						t.Fatal("cancel identity", downloadErr)
					}
				}
				marker, err := os.ReadFile(filepath.Join(root, "fixture-client"))
				if err != nil || string(marker) != "S" {
					t.Fatal("recovery relicensed", err)
				}
				saved, err := os.ReadFile(filepath.Join(root, "auth.json"))
				if err != nil || string(saved) != initialLibraryAuth {
					t.Fatal("controlled kill crossed pre-auth-save barrier", err)
				}
				if err := c.Close(context.Background()); err != nil {
					t.Fatal("root users not joined", err)
				}
			})
		}
	}
}

func TestDownloadCallerOwnedBlockedCallbacksAndIDs(t *testing.T) {
	for _, at := range []model.EventType{model.Started, model.Completed, model.Failed} {
		t.Run(string(at), func(t *testing.T) {
			c := libraryClient(t)
			setDownloadTranscript(c, wire("result", 1, `"outcome":`+outcomePayload), 0, "absent")
			if at == model.Failed {
				c.downloadSource = transcriptSource("", 0)
			}
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			go func() {
				defer close(done)
				c.Download(context.Background(), "blocked-callback", fixtureDownloadBook, func(e model.OperationEvent) {
					if e.Type == at {
						close(entered)
						<-release
					}
				})
			}()
			defer func() { once.Do(func() { close(release) }); <-done }()
			<-entered
			if _, err := c.Library(context.Background(), "blocked-callback", nil); failureCode(t, err) != "busy" {
				t.Fatal("callback lost ID ownership")
			}
			if err := c.Close(context.Background()); err != nil {
				t.Fatal("callback incorrectly owns root after joined work", err)
			}
			select {
			case <-done:
				t.Fatal("returned through blocked caller code")
			default:
			}
			once.Do(func() { close(release) })
			<-done
		})
	}
}
func TestActualPublicDownloadGeneratedMediaAndRestart(t *testing.T) {
	clear := compositionClearSource(t) // Only actual-media tests may skip locally.
	c, root, _ := librarySDK(t)
	publicDownloadFixture(c, "real", "", clear)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	var events []model.OperationEvent
	result, err := c.Download(ctx, "real-public", fixtureDownloadBook, func(e model.OperationEvent) {
		events = append(events, e)
	})
	if err != nil || result.Media == nil || result.Media.Validate() != nil || result.Published == nil || !*result.Published || len(result.Media.Chapters) != 1 || result.MediaDurability != model.PersistenceConfirmed {
		t.Fatal("real public media failed", err)
	}
	if len(events) < 7 || events[0].Type != model.Started ||
		events[1].Type != model.Stage || events[1].Stage != "licensing" ||
		events[2].Type != model.Stage || events[2].Stage != "downloading" ||
		events[len(events)-3].Type != model.Stage || events[len(events)-3].Stage != "preparing" ||
		events[len(events)-2].Type != model.Stage || events[len(events)-2].Stage != "publishing" ||
		events[len(events)-1].Type != model.Completed {
		t.Fatal("real generated-media stage sequence wrong", events)
	}
	var prevBytes int64 = -1
	for _, e := range events[3 : len(events)-3] {
		if e.Type != model.Progress || e.Bytes == nil || *e.Bytes <= prevBytes || e.Total == nil || *e.Total != int64(len(clear)) {
			t.Fatal("real generated-media progress wrong", e)
		}
		prevBytes = *e.Bytes
	}
	if prevBytes != int64(len(clear)) {
		t.Fatal("real generated-media final bytes mismatch", prevBytes, len(clear))
	}
	for i, e := range events {
		if e.Sequence != uint64(i+1) || e.OperationID != "real-public" {
			t.Fatal("real generated-media sequence/id wrong", i, e)
		}
	}
	if err := os.Remove(filepath.Join(root, "auth.json")); err != nil {
		t.Fatal(err)
	}
	var reuseEvents []model.OperationEvent
	reused, err := c.Download(ctx, "real-reuse", fixtureDownloadBook, func(e model.OperationEvent) {
		reuseEvents = append(reuseEvents, e)
	})
	if err != nil || !reused.Reused || reused.Media == nil || reused.Media.Chapters != nil {
		t.Fatal("real reuse licensed/invented hierarchy", err)
	}
	if len(reuseEvents) != 2 || reuseEvents[0].Type != model.Started || reuseEvents[1].Type != model.Completed {
		t.Fatal("real reuse emitted stages", reuseEvents)
	}
	// Force lost terminal delivery on a second root through the same public API:
	// actual production validator must recover the generated file without auth.
	c.downloadSource = transcriptSource("", 0)
	recovered, err := c.Download(ctx, "real-recovery", fixtureDownloadBook, nil)
	if failureCode(t, err) != "protocol_error" || recovered.Media == nil || recovered.State != model.MediaReady || recovered.Published != nil || recovered.Media.Chapters != nil || recovered.MediaDurability != model.PersistenceUnknown {
		t.Fatal("real public recovery failed", err)
	}
}

func TestActualPublicDownloadSeededRecoveryBoundsAndMalformed(t *testing.T) {
	for _, test := range []struct {
		name     string
		canceled bool
		want     model.MediaState
	}{
		{"ready-truncated", false, model.MediaReady},
		{"ready-canceled", true, model.MediaReady},
		{"absent-truncated", false, model.MediaAbsent},
		{"absent-canceled", true, model.MediaAbsent},
		{"invalid-empty", false, model.MediaInvalid},
		{"invalid-corrupt", false, model.MediaInvalid},
		{"unsafe-mode", false, model.MediaUnsafe},
		{"unsafe-symlink", false, model.MediaUnsafe},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, root, _ := librarySDK(t)
			if err := os.Remove(filepath.Join(root, "auth.json")); err != nil {
				t.Fatal(err)
			}
			downloads := filepath.Join(root, "downloads")
			if err := os.Mkdir(downloads, 0700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(downloads, "B000000001.m4b")
			switch test.name {
			case "ready-truncated", "ready-canceled":
				if err := os.WriteFile(target, []byte("synthetic-media"), 0600); err != nil {
					t.Fatal(err)
				}
			case "invalid-empty":
				if err := os.WriteFile(target, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "invalid-corrupt":
				if err := os.WriteFile(target, []byte("not-media"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unsafe-mode":
				if err := os.WriteFile(target, []byte("synthetic-media"), 0644); err != nil {
					t.Fatal(err)
				}
			case "unsafe-symlink":
				realFile := filepath.Join(t.TempDir(), "real.m4b")
				if err := os.WriteFile(realFile, []byte("synthetic-media"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(realFile, target); err != nil {
					t.Fatal(err)
				}
			}
			publicDownloadFixture(c, "success", "", nil)
			if test.canceled {
				c.downloadSource = transcriptSource(wire("error", 1, `"failure":{"code":"canceled"}`), 0)
			} else {
				c.downloadSource = transcriptSource("", 0)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var events []model.OperationEvent
			result, err := c.Download(ctx, "seeded-recovery", fixtureDownloadBook, func(e model.OperationEvent) {
				events = append(events, e)
			})
			if len(events) != 2 || events[1].Download == nil || events[1].Download.State != test.want {
				t.Fatal("wrong seeded recovery lifecycle", events)
			}
			if result.State != test.want || result.Published != nil || result.Reused || result.MediaDurability != model.PersistenceUnknown || result.AuthPersistence != model.PersistenceUnknown {
				t.Fatal("seeded recovery invented facts", result)
			}
			if test.want == model.MediaReady {
				if result.Media == nil || result.Media.Path != target || result.Media.Chapters != nil {
					t.Fatal("seeded ready media invalid or invented hierarchy", result.Media)
				}
				if test.canceled {
					if err != nil || events[1].Type != model.Completed {
						t.Fatal("ready did not beat canceled error", err)
					}
				} else if failureCode(t, err) != "protocol_error" || events[1].Type != model.Failed {
					t.Fatal("truncated transport lost protocol_error", err)
				}
			} else {
				if result.Media != nil {
					t.Fatal("non-ready returned media", result.Media)
				}
				if test.canceled {
					if failureCode(t, err) != "canceled" || !errors.Is(err, context.Canceled) || events[1].Type != model.Canceled {
						t.Fatal("canceled identity lost", err)
					}
				} else if failureCode(t, err) != "protocol_error" || events[1].Type != model.Failed {
					t.Fatal("truncated transport lost protocol_error", err)
				}
			}
			if _, statErr := os.Stat(filepath.Join(root, "fixture-client")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("seeded recovery constructed SDK", statErr)
			}
		})
	}
}
