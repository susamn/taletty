package audible

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/susamn/taletty/engine/model"
)

func libraryClient(t *testing.T) *Client {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	client, err := Open(context.Background(), root, pythonFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	return client
}
func wire(kind string, seq int, payload string) string {
	return fmt.Sprintf("{\"protocol\":1,\"operation_id\":\"OP_ID\",\"seq\":%d,\"type\":%q,%s}\n", seq, kind, payload)
}
func setTranscript(client *Client, transcript string, exit int, block bool) {
	client.librarySource = "import json,sys\nrequest=json.load(sys.stdin)\nsys.stdout.write(" + strconv.Quote(transcript) + ".replace('OP_ID',request['operation_id']))\nsys.stdout.flush()\n"
	if block {
		client.librarySource += "import signal\nsignal.pause()\n"
	} else {
		client.librarySource += fmt.Sprintf("sys.exit(%d)\n", exit)
	}
}
func goodTranscript() string {
	return wire("stage", 1, `"stage":"library_page","books":[`+bookPayload+`]`) + wire("result", 2, `"count":1`)
}

func TestLibraryPublicEventsAndNoPartialSnapshots(t *testing.T) {
	client := libraryClient(t)
	for _, test := range []struct {
		name, transcript, code string
		exit                   int
	}{
		{name: "success", transcript: goodTranscript()},
		{name: "empty", transcript: wire("result", 1, `"count":0`)},
		{name: "later failure", transcript: wire("stage", 1, `"stage":"library_page","books":[`+bookPayload+`]`) + wire("error", 2, `"failure":{"code":"throttled","message":"fixture-private"}`), code: "throttled"},
		{name: "count", transcript: wire("result", 1, `"count":1`), code: "protocol_error"},
		{name: "nonzero", transcript: goodTranscript(), exit: 3, code: "protocol_error"},
		{name: "truncated", transcript: wire("stage", 1, `"stage":"library_page","books":[`+bookPayload+`]`), code: "protocol_error"},
		{name: "invalid payload", transcript: wire("stage", 1, `"stage":"library_page","books":[{}]`) + wire("result", 2, `"count":1`), code: "protocol_error"},
		{name: "unsupported", transcript: wire("error", 1, `"failure":{"code":"unsupported"}`), code: "unsupported"},
		{name: "SDK cancel", transcript: wire("error", 1, `"failure":{"code":"canceled"}`), code: "canceled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			setTranscript(client, test.transcript, test.exit, false)
			var events []model.OperationEvent
			result, err := client.Library(context.Background(), "public-test", func(event model.OperationEvent) {
				events = append(events, event)
				if event.Failure != nil {
					event.Failure.Message = "fixture-private-mutated"
				}
			})
			if len(events) < 2 || events[0].Type != model.Started {
				t.Fatal("missing lifecycle")
			}
			for index, event := range events {
				if event.Sequence != uint64(index+1) || event.OperationID != "public-test" {
					t.Fatal("event ordering")
				}
			}
			final := events[len(events)-1]
			if test.code == "" {
				if err != nil || final.Type != model.Completed || result.OperationID != "public-test" || result.Books == nil || final.Count != len(result.Books) {
					t.Fatal("invalid complete result", err)
				}
			} else {
				if err == nil || failureCode(t, err) != test.code || strings.Contains(err.Error(), "fixture-private") || result.Books != nil || result.OperationID != "" || final.Count != 0 {
					t.Fatal("partial/unsafe result", err)
				}
				expected := model.Failed
				if test.code == "canceled" {
					expected = model.Canceled
					if !errors.Is(err, context.Canceled) {
						t.Fatal("cancel identity lost")
					}
				}
				if final.Type != expected {
					t.Fatal("wrong terminal")
				}
			}
		})
	}
	setTranscript(client, goodTranscript(), 0, false)
	if _, err := client.Library(context.Background(), "nil-reporter", nil); err != nil {
		t.Fatal(err)
	}
	// Cancellation requested from the completed notification is after the commit.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if result, err := client.Library(ctx, "committed", func(e model.OperationEvent) {
		if e.Type == model.Completed {
			cancel()
		}
	}); err != nil || len(result.Books) != 1 {
		t.Fatal("completion undone", err)
	}
}

func TestLibraryIdentityCancellationAndClosedPreflight(t *testing.T) {
	client := libraryClient(t)
	setTranscript(client, goodTranscript(), 0, false)
	for _, id := range []string{"", "fixture-private\n", strings.Repeat("a", 65), "中文"} {
		called := false
		_, err := client.Library(context.Background(), id, func(model.OperationEvent) { called = true })
		var failure *model.Failure
		if called || !errors.As(err, &failure) || failure.Code != "invalid_input" || failure.OperationID != "" || strings.Contains(err.Error(), "fixture-private") {
			t.Fatal("invalid ID accepted/echoed", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := client.Library(ctx, "reserved", func(e model.OperationEvent) {
			if e.Type == model.Started {
				close(started)
				<-release
			}
		})
		done <- err
	}()
	<-started
	called := false
	if _, err := client.Library(context.Background(), "reserved", func(model.OperationEvent) { called = true }); failureCode(t, err) != "busy" || called {
		t.Fatal("duplicate ID entered")
	}
	cancel()
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := client.Library(context.Background(), "reserved", nil); err != nil {
		t.Fatal("ID not released", err)
	}
	deadline, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if _, err := client.Library(deadline, "deadline", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	original := client.python
	client.python = "/nonexistent-taletty-python"
	if _, err := client.Library(context.Background(), "runtime", nil); failureCode(t, err) != "runtime_missing" {
		t.Fatal(err)
	}
	client.python = original
	if err := client.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Library(context.Background(), "closed", nil); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestLibraryCallbackPanicAndActiveCancelJoinOwnedChild(t *testing.T) {
	client := libraryClient(t)
	for _, at := range []model.EventType{model.Started, model.Stage} {
		setTranscript(client, wire("stage", 1, `"stage":"library_page","books":[`+bookPayload+`]`), 0, true)
		func() {
			defer func() {
				if recovered := recover(); recovered != "caller-owned-panic" {
					t.Fatal("panic was sanitized", recovered)
				}
			}()
			client.Library(context.Background(), "panic", func(event model.OperationEvent) {
				if event.Type == at {
					panic("caller-owned-panic")
				}
			})
			t.Fatal("callback did not panic")
		}()
		setTranscript(client, goodTranscript(), 0, false)
		if _, err := client.Library(context.Background(), "panic", nil); err != nil {
			t.Fatal("root/ID/worker not recovered", err)
		}
	}
	setTranscript(client, wire("stage", 1, `"stage":"library_page","books":[`+bookPayload+`]`), 0, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := client.Library(ctx, "active-cancel", func(event model.OperationEvent) {
		if event.Type == model.Stage {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || result.Books != nil {
		t.Fatal("active cancel returned partial", err)
	}
}
