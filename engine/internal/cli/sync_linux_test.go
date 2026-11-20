package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/susamn/taletty/engine/model"
)

type afterCloseWriter struct {
	client *fakeAccount
	bytes.Buffer
}

func (w *afterCloseWriter) Write(b []byte) (int, error) {
	if !w.client.closed {
		panic("snapshot before session cleanup")
	}
	return w.Buffer.Write(b)
}
func TestSyncCompleteOutputAndCleanup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, machine := range []bool{false, true} {
		for _, count := range []int{0, 1} {
			client := &fakeAccount{snapshot: model.LibrarySnapshot{OperationID: "cli-sync", Books: make([]model.Book, count)}}
			if count > 0 {
				client.snapshot.Books[0] = model.Book{ID: model.BookID{Provider: "audible", Namespace: "us", ID: "T000000001"}, Title: "Fixture"}
			}
			deps := fixture(t, client)
			deps.terminal = func() (*os.File, error) { panic("sync touched terminal") }
			args := []string{"sync", "--python", "fixture-python"}
			if machine {
				args = append(args, "--json")
			}
			out := &afterCloseWriter{client: client}
			var diagnostics bytes.Buffer
			if code := run(context.Background(), args, out, &diagnostics, deps); code != 0 || diagnostics.Len() != 0 || !client.closed {
				t.Fatal(code, diagnostics.String())
			}
			if machine {
				var payload struct{ Snapshot model.LibrarySnapshot }
				if json.Unmarshal(out.Bytes(), &payload) != nil || payload.Snapshot.Books == nil || len(payload.Snapshot.Books) != count {
					t.Fatal(out.String())
				}
			} else if !strings.Contains(out.String(), "Library synced:") {
				t.Fatal(out.String())
			}
		}
	}
}
func TestSyncFailuresNeverPublishCandidate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, primary := range []error{nil, errors.New("fixture-private"), &model.Failure{Code: "throttled", Message: "Audible requested a pause before retrying.", Retry: "wait"}, &model.Failure{Code: "storage_uncertain", Message: "Credentials published; durability not confirmed.", Retry: "none"}} {
		for _, closeError := range []error{nil, errors.New("fixture-private-close")} {
			if primary == nil && closeError == nil {
				continue
			}
			client := &fakeAccount{libraryErr: primary, closeErr: closeError, snapshot: model.LibrarySnapshot{Books: []model.Book{{Title: "candidate-must-not-publish"}}}}
			code, out, diagnostics := invoke([]string{"sync", "--python", "fixture-python", "--json"}, fixture(t, client))
			var payload struct{ Failure failure }
			if code != 1 || !client.closed || json.Unmarshal([]byte(out), &payload) != nil || payload.Failure.Code == "" || strings.Contains(out+diagnostics, "fixture-private") || strings.Contains(out, "candidate-must-not-publish") {
				t.Fatal(code, out, diagnostics)
			}
			if primary != nil {
				var typed *model.Failure
				if errors.As(primary, &typed) && (payload.Failure.Code != typed.Code || payload.Failure.Retry != typed.Retry) {
					t.Fatal("typed failure lost", out)
				}
			}
		}
	}
	for _, machine := range []bool{false, true} {
		args := []string{"sync", "--python", "fixture-python"}
		if machine {
			args = append(args, "--json")
		}
		client := &fakeAccount{}
		if code := run(context.Background(), args, brokenWriter{}, io.Discard, fixture(t, client)); code != 1 || !client.closed {
			t.Fatal("output leaked session", code)
		}
		ctx, cancel := context.WithCancel(context.Background())
		client = &fakeAccount{cancel: cancel}
		var out, diagnostics bytes.Buffer
		if code := run(ctx, args, &out, &diagnostics, fixture(t, client)); code != 130 || !client.closed {
			t.Fatal("cancel leaked session", code)
		}
		cancel()
	}
}
func TestSyncRootAndPreflightFailures(t *testing.T) {
	args := []string{"sync", "--python", "fixture-python", "--json"}
	for _, home := range []string{"", "relative", t.TempDir()} {
		t.Setenv("HOME", home)
		if filepath.IsAbs(home) {
			if err := os.WriteFile(filepath.Join(home, ".config"), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		code, out, _ := invoke(args, fixture(t, &fakeAccount{}))
		if code != 1 || !strings.Contains(out, `"code":"storage_failed"`) {
			t.Fatal(code, out)
		}
	}
	t.Setenv("HOME", t.TempDir())
	deps := fixture(t, &fakeAccount{})
	deps.open = func(context.Context, string, string) (account, error) { return nil, errors.New("fixture-private-open") }
	if code, out, _ := invoke(args, deps); code != 1 || !strings.Contains(out, `"code":"internal"`) {
		t.Fatal(code, out)
	}
	if code, _, _ := invoke([]string{"sync", "--python", "fixture-python", "--marketplace", "de"}, deps); code != 2 {
		t.Fatal(code)
	}
}
