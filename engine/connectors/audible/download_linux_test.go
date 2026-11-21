package audible

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/susamn/taletty/engine/model"
)

var fixtureDownloadBook = model.BookID{Provider: "audible", Namespace: "us", ID: "B000000001"}

func transcriptSource(transcript string, exit int) string {
	return "import json,sys\nrequest=json.load(sys.stdin)\nsys.stdout.write(" + strconv.Quote(transcript) + ".replace('OP_ID',request['operation_id']))\nsys.stdout.flush()\nsys.exit(" + strconv.Itoa(exit) + ")\n"
}
func setDownloadTranscript(c *Client, transcript string, exit int, state string) {
	c.downloadSource = transcriptSource(transcript, exit)
	c.reconcileSource = transcriptSource(wire("result", 1, `"state":"`+state+`"`), 0)
}
func TestDownloadPublicLifecycleIndependentFactsAndCopies(t *testing.T) {
	for _, test := range []struct {
		name, raw, code  string
		state            model.MediaState
		durability, auth model.Persistence
	}{
		{"fresh", outcomePayload, "", model.MediaReady, model.PersistenceConfirmed, model.PersistenceConfirmed},
		{"known empty chapters", `{"ready":true,"reused":false,"published":true,"media_code":null,"auth_code":null,"chapters":[]}`, "", model.MediaReady, model.PersistenceConfirmed, model.PersistenceConfirmed},
		{"reuse", `{"ready":true,"reused":true,"published":false,"media_code":null,"auth_code":null,"chapters":null}`, "", model.MediaReady, model.PersistenceUnknown, model.PersistenceUnknown},
		{"ready canceled", `{"ready":true,"reused":false,"published":true,"media_code":"canceled","auth_code":null,"chapters":null}`, "", model.MediaReady, model.PersistenceUnknown, model.PersistenceUnknown},
		{"ready canceled auth failed", `{"ready":true,"reused":false,"published":true,"media_code":"canceled","auth_code":"storage_failed","chapters":null}`, "storage_failed", model.MediaReady, model.PersistenceUnknown, model.PersistenceFailed},
		{"auth failed", strings.Replace(outcomePayload, `"auth_code":null`, `"auth_code":"storage_failed"`, 1), "storage_failed", model.MediaReady, model.PersistenceConfirmed, model.PersistenceFailed},
		{"auth uncertain", strings.Replace(outcomePayload, `"auth_code":null`, `"auth_code":"storage_uncertain"`, 1), "storage_uncertain", model.MediaReady, model.PersistenceConfirmed, model.PersistenceUncertain},
		{"media uncertain", strings.Replace(outcomePayload, `"media_code":null`, `"media_code":"storage_uncertain"`, 1), "storage_uncertain", model.MediaReady, model.PersistenceUncertain, model.PersistenceUnknown},
		{"close failed", strings.Replace(outcomePayload, `"media_code":null`, `"media_code":"internal"`, 1), "internal", model.MediaReady, model.PersistenceUnknown, model.PersistenceUnknown},
		{"denied", `{"ready":false,"reused":false,"published":false,"media_code":"entitlement_denied","auth_code":null,"chapters":null}`, "entitlement_denied", model.MediaAbsent, model.PersistenceUnknown, model.PersistenceUnknown},
		{"both", `{"ready":false,"reused":false,"published":null,"media_code":"network","auth_code":"storage_failed","chapters":null}`, "storage_failed", model.MediaAbsent, model.PersistenceUnknown, model.PersistenceFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := libraryClient(t)
			setDownloadTranscript(c, wire("result", 1, `"outcome":`+test.raw), 0, "absent")
			var events []model.OperationEvent
			result, err := c.Download(context.Background(), "lifecycle", fixtureDownloadBook, func(e model.OperationEvent) {
				events = append(events, e)
				if e.Failure != nil {
					e.Failure.Message = "fixture-private-mutated"
				}
				if e.Download != nil {
					if e.Download.Published != nil {
						*e.Download.Published = false
					}
					if e.Download.Media != nil {
						e.Download.Media.Path = "fixture-private-mutated"
						if len(e.Download.Media.Chapters) > 0 {
							e.Download.Media.Chapters[0].Title = "mutated"
							*e.Download.Media.Chapters[0].End = 9
						}
					}
					if e.Download.MediaFailure != nil {
						e.Download.MediaFailure.Message = "mutated"
					}
					if e.Download.AuthFailure != nil {
						e.Download.AuthFailure.Message = "mutated"
					}
				}
			})
			if len(events) != 2 || events[0].Type != model.Started || events[0].Download != nil || events[0].Sequence != 1 || events[1].Sequence != 2 || events[1].OperationID != "lifecycle" || events[1].Download == nil {
				t.Fatal("wrong lifecycle")
			}
			if result.OperationID != "lifecycle" || result.BookID != fixtureDownloadBook || result.State != test.state || result.MediaDurability != test.durability || result.AuthPersistence != test.auth {
				t.Fatal("lost facts", result)
			}
			if test.code == "" {
				if err != nil || events[1].Type != model.Completed {
					t.Fatal(err)
				}
			} else if err == nil || failureCode(t, err) != test.code || strings.Contains(err.Error(), "fixture-private") || events[1].Type != model.Failed {
				t.Fatal("wrong failure", err)
			}
			if result.Media != nil {
				if result.Media.Validate() != nil || strings.Contains(result.Media.Path, "mutated") || (len(result.Media.Chapters) > 0 && (result.Media.Chapters[0].Title != "Fixture" || *result.Media.Chapters[0].End != 0)) {
					t.Fatal("event aliased result")
				}
				if test.name == "known empty chapters" && (result.Media.Chapters == nil || len(result.Media.Chapters) != 0) {
					t.Fatal("known empty chapters lost")
				}
			}
			if (test.name == "ready canceled" || test.name == "ready canceled auth failed") && result.MediaFailure != nil {
				t.Fatal("ready media kept media cancellation", result.MediaFailure)
			}
			if result.Published != nil && test.name != "reuse" && test.name != "denied" && !*result.Published {
				t.Fatal("event changed commit")
			}
			if result.MediaFailure != nil && result.MediaFailure.Message == "mutated" || result.AuthFailure != nil && result.AuthFailure.Message == "mutated" {
				t.Fatal("failure aliased")
			}
		})
	}
}
func TestDownloadCanceledOutcomeReconcilesReadyWithAndWithoutAuthFailure(t *testing.T) {
	for _, test := range []struct {
		name, transcript, reconcileState, wantCode string
		wantEvent                                  model.EventType
		wantState                                  model.MediaState
		wantAuth                                   model.Persistence
		wantPublished                              *bool
	}{
		{
			name:           "outcome canceled without auth recovers ready",
			transcript:     wire("result", 1, `"outcome":{"ready":false,"reused":false,"published":false,"media_code":"canceled","auth_code":null,"chapters":null}`),
			reconcileState: "ready",
			wantEvent:      model.Completed,
			wantState:      model.MediaReady,
			wantAuth:       model.PersistenceUnknown,
			wantPublished:  func() *bool { v := false; return &v }(),
		},
		{
			name:           "outcome canceled with auth failed recovers ready",
			transcript:     wire("result", 1, `"outcome":{"ready":false,"reused":false,"published":null,"media_code":"canceled","auth_code":"storage_failed","chapters":null}`),
			reconcileState: "ready",
			wantCode:       "storage_failed",
			wantEvent:      model.Failed,
			wantState:      model.MediaReady,
			wantAuth:       model.PersistenceFailed,
		},
		{
			name:           "outcome canceled with auth uncertain recovers ready",
			transcript:     wire("result", 1, `"outcome":{"ready":false,"reused":false,"published":null,"media_code":"canceled","auth_code":"storage_uncertain","chapters":null}`),
			reconcileState: "ready",
			wantCode:       "storage_uncertain",
			wantEvent:      model.Failed,
			wantState:      model.MediaReady,
			wantAuth:       model.PersistenceUncertain,
		},
		{
			name:           "outcome canceled without auth reconciles absent",
			transcript:     wire("result", 1, `"outcome":{"ready":false,"reused":false,"published":false,"media_code":"canceled","auth_code":null,"chapters":null}`),
			reconcileState: "absent",
			wantCode:       "canceled",
			wantEvent:      model.Canceled,
			wantState:      model.MediaAbsent,
			wantAuth:       model.PersistenceUnknown,
			wantPublished:  func() *bool { v := false; return &v }(),
		},
		{
			name:           "error frame canceled recovers ready",
			transcript:     wire("error", 1, `"failure":{"code":"canceled"}`),
			reconcileState: "ready",
			wantEvent:      model.Completed,
			wantState:      model.MediaReady,
			wantAuth:       model.PersistenceUnknown,
		},
		{
			name:           "error frame canceled reconciles absent",
			transcript:     wire("error", 1, `"failure":{"code":"canceled"}`),
			reconcileState: "absent",
			wantCode:       "canceled",
			wantEvent:      model.Canceled,
			wantState:      model.MediaAbsent,
			wantAuth:       model.PersistenceUnknown,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := libraryClient(t)
			setDownloadTranscript(c, test.transcript, 0, test.reconcileState)
			var events []model.OperationEvent
			result, err := c.Download(context.Background(), "cancel-reconcile", fixtureDownloadBook, func(e model.OperationEvent) {
				events = append(events, e)
			})
			if len(events) != 2 || events[1].Type != test.wantEvent || events[1].Download == nil || events[1].Download.State != test.wantState {
				t.Fatal("wrong lifecycle", events)
			}
			if result.State != test.wantState || result.AuthPersistence != test.wantAuth || result.MediaDurability != model.PersistenceUnknown {
				t.Fatal("wrong state/persistence", result)
			}
			if (result.Published == nil) != (test.wantPublished == nil) || (result.Published != nil && *result.Published != *test.wantPublished) {
				t.Fatal("wrong published fact", result.Published)
			}
			if test.wantState == model.MediaReady {
				if result.Media == nil || result.Media.Chapters != nil || result.MediaFailure != nil {
					t.Fatal("ready recovery kept media cancellation or invented chapters", result)
				}
			} else if result.Media != nil {
				t.Fatal("non-ready returned media", result.Media)
			}
			if test.wantCode == "" {
				if err != nil || result.AuthFailure != nil {
					t.Fatal("unexpected error", err)
				}
			} else {
				if err == nil || failureCode(t, err) != test.wantCode {
					t.Fatal("wrong failure code", err)
				}
				if test.wantCode == "canceled" && !errors.Is(err, context.Canceled) {
					t.Fatal("canceled error lost context identity", err)
				}
				if (test.wantCode == "storage_failed" || test.wantCode == "storage_uncertain") && (result.AuthFailure == nil || result.AuthFailure.Code != test.wantCode) {
					t.Fatal("auth failure lost", result.AuthFailure)
				}
			}
		})
	}
}
func TestDownloadFailedTransportReconcilesWithoutInventingFacts(t *testing.T) {
	for _, test := range []struct {
		name, transcript string
		exit             int
	}{
		{"nonzero", wire("result", 1, `"outcome":`+outcomePayload), 7},
		{"truncated", "", 0},
		{"malformed", wire("result", 1, `"outcome":{}`), 0},
		{"duplicate", wire("result", 1, `"outcome":`+outcomePayload+`,"future":{"x":0,"x":1}`), 0},
		{"unexpected stage", wire("stage", 1, `"stage":"licensing"`) + wire("result", 2, `"outcome":`+outcomePayload), 0},
		{"unsafe error", wire("error", 1, `"failure":{"code":"fixture-private"}`), 0},
	} {
		for _, state := range []string{"ready", "absent", "invalid", "unsafe", "unknown", "bad"} {
			t.Run(test.name+"/"+state, func(t *testing.T) {
				c := libraryClient(t)
				setDownloadTranscript(c, test.transcript, test.exit, state)
				result, err := c.Download(context.Background(), "failed-transport", fixtureDownloadBook, nil)
				want := model.MediaState(state)
				if state == "bad" {
					want = model.MediaUnknown
				}
				if failureCode(t, err) != "protocol_error" || result.State != want || result.Published != nil || result.Reused || result.MediaDurability != model.PersistenceUnknown || result.AuthPersistence != model.PersistenceUnknown {
					t.Fatal("invented recovered facts", result, err)
				}
				if state == "ready" && (result.Media == nil || result.Media.Chapters != nil) {
					t.Fatal("reconstructed hierarchy")
				}
				if state != "ready" && result.Media != nil {
					t.Fatal("unsafe artifact escaped")
				}
			})
		}
	}
	for _, badReconcile := range []struct {
		name, source string
	}{
		{"nonzero", "import sys\nsys.exit(7)\n"},
		{"truncated", transcriptSource("", 0)},
		{"stage", transcriptSource(wire("stage", 1, `"stage":"licensing"`)+wire("result", 2, `"state":"ready"`), 0)},
		{"duplicate", transcriptSource(wire("result", 1, `"state":"ready","future":{"x":0,"x":1}`), 0)},
		{"error", transcriptSource(wire("error", 1, `"failure":{"code":"internal"}`), 0)},
	} {
		t.Run("reconcile/"+badReconcile.name, func(t *testing.T) {
			c := libraryClient(t)
			setDownloadTranscript(c, "", 0, "ready")
			c.reconcileSource = badReconcile.source
			result, err := c.Download(context.Background(), "recovery-failed", fixtureDownloadBook, nil)
			if err == nil || result.State != model.MediaUnknown || result.Media != nil {
				t.Fatal("failed recovery claimed absence or media", result, err)
			}
		})
	}
}
func TestDownloadIDsCancellationReporterPanicAndClose(t *testing.T) {
	c := libraryClient(t)
	setDownloadTranscript(c, wire("result", 1, `"outcome":`+outcomePayload), 0, "absent")
	for _, input := range []struct {
		id   string
		book model.BookID
	}{{"", fixtureDownloadBook}, {"private\n", fixtureDownloadBook}, {strings.Repeat("x", 65), fixtureDownloadBook}, {"book", model.BookID{}}, {"book", model.BookID{Provider: "audible", Namespace: "de", ID: "B000000001"}}, {"book", model.BookID{Provider: "local", Namespace: "library", ID: "uuid-1"}}} {
		called := false
		_, err := c.Download(context.Background(), input.id, input.book, func(model.OperationEvent) { called = true })
		if called || failureCode(t, err) != "invalid_input" {
			t.Fatal("bad input entered")
		}
	}
	for _, libraryFirst := range []bool{true, false} {
		started, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		ctx, stop := context.WithCancel(context.Background())
		callback := func(e model.OperationEvent) {
			if e.Type == model.Started {
				close(started)
				<-release
			}
		}
		go func() {
			if libraryFirst {
				_, err := c.Library(ctx, "shared", callback)
				done <- err
			} else {
				_, err := c.Download(ctx, "shared", fixtureDownloadBook, callback)
				done <- err
			}
		}()
		<-started
		called := false
		var err error
		if libraryFirst {
			_, err = c.Download(context.Background(), "shared", fixtureDownloadBook, func(model.OperationEvent) { called = true })
		} else {
			_, err = c.Library(context.Background(), "shared", func(model.OperationEvent) { called = true })
		}
		if called || failureCode(t, err) != "busy" {
			t.Fatal("cross-operation duplicate entered")
		}
		stop()
		close(release)
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal("queued cancel", err)
		}
	}
	for _, at := range []model.EventType{model.Started, model.Completed, model.Failed} {
		if at == model.Failed {
			setDownloadTranscript(c, "", 0, "absent")
		} else {
			setDownloadTranscript(c, wire("result", 1, `"outcome":`+outcomePayload), 0, "absent")
		}
		func() {
			defer func() {
				if recover() != "caller-owned" {
					t.Fatal("panic swallowed")
				}
			}()
			c.Download(context.Background(), "panic", fixtureDownloadBook, func(e model.OperationEvent) {
				if e.Type == at {
					panic("caller-owned")
				}
			})
			t.Fatal("no panic")
		}()
		setDownloadTranscript(c, wire("result", 1, `"outcome":`+outcomePayload), 0, "absent")
		if _, err := c.Download(context.Background(), "panic", fixtureDownloadBook, nil); err != nil {
			t.Fatal("panic leaked ID/root", err)
		}
	}
	ctx, stop := context.WithCancel(context.Background())
	result, err := c.Download(ctx, "late", fixtureDownloadBook, func(e model.OperationEvent) {
		if e.Type == model.Completed {
			stop()
		}
	})
	if err != nil || result.Media == nil {
		t.Fatal("completed callback undid media", err)
	}
	original := c.python
	c.python = "/nonexistent-taletty-python"
	result, err = c.Download(context.Background(), "runtime", fixtureDownloadBook, nil)
	if failureCode(t, err) != "runtime_missing" || result.State != model.MediaUnknown {
		t.Fatal(err)
	}
	c.python = original
	preCanceled, stopPre := context.WithCancel(context.Background())
	stopPre()
	var preEvents []model.OperationEvent
	if res, err := c.Download(preCanceled, "pre-canceled", fixtureDownloadBook, func(e model.OperationEvent) { preEvents = append(preEvents, e) }); !errors.Is(err, context.Canceled) || res.State != model.MediaUnknown || len(preEvents) != 2 || preEvents[1].Type != model.Canceled {
		t.Fatal("pre-canceled download", res, err, preEvents)
	}
	deadline, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := c.Download(deadline, "deadline", fixtureDownloadBook, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Download(context.Background(), "closed", fixtureDownloadBook, nil); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}
func TestDownloadReadyPathUsesRenamedRootAndRejectsUnsafeDisplayPath(t *testing.T) {
	for _, name := range []string{"renamed", "unsafe\nname", "deleted"} {
		t.Run(name, func(t *testing.T) {
			c := libraryClient(t)
			// Resolve the actual borrowed root without guessing the test TempDir layout.
			var path string
			c.session.WithRoot(context.Background(), func(_ context.Context, root *os.File) error {
				var err error
				path, err = os.Readlink("/proc/self/fd/" + strconv.FormatUint(uint64(root.Fd()), 10))
				return err
			})
			target := filepath.Join(t.TempDir(), name)
			if err := os.Rename(path, target); err != nil {
				t.Fatal(err)
			}
			if name == "deleted" {
				if err := os.Remove(filepath.Join(target, ".auth.lock")); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
			}
			setDownloadTranscript(c, wire("result", 1, `"outcome":`+outcomePayload), 0, "absent")
			result, err := c.Download(context.Background(), "path", fixtureDownloadBook, nil)
			if name == "renamed" {
				if err != nil || result.Media.Path != filepath.Join(target, "downloads", "B000000001.m4b") {
					t.Fatal("root path stale", err)
				}
			} else if err == nil || result.State != model.MediaUnknown || result.Media != nil || result.Published == nil || !*result.Published {
				t.Fatal("unsafe display path/commit facts", err)
			}
		})
	}
}
