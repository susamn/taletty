package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/susamn/taletty/engine/model"
)

func readyDownloadResult(reused, publishedVal bool) model.DownloadResult {
	book := model.BookID{Provider: "audible", Namespace: "us", ID: "B000000001"}
	dur := model.PersistenceConfirmed
	if reused {
		dur = model.PersistenceUnknown
	}
	return model.DownloadResult{
		OperationID:     "cli-download",
		BookID:          book,
		State:           model.MediaReady,
		Media:           &model.Media{BookID: book, Path: "/tmp/taletty/downloads/B000000001.m4b", Format: "m4b"},
		Reused:          reused,
		Published:       &publishedVal,
		MediaDurability: dur,
		AuthPersistence: dur,
	}
}

func TestDownloadUsageAndRedaction(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	deps := fixture(t, &fakeAccount{})
	for _, args := range [][]string{
		{"download"},
		{"download", "--python", "fixture-python"},
		{"download", "B000000001", "--python", ""},
		{"download", "B000000001", "--python", "   "},
		{"download", "B000000001", "--python", "bad\x00secret"},
		{"download", "SHORT", "--python", "fixture-python"},
		{"download", "bad_asin12", "--python", "fixture-python"},
		{"download", "../secret1", "--python", "fixture-python"},
		{"download", "B000000001", "B000000002", "--python", "fixture-python"},
		{"download", "--python", "fixture-python", "B000000001", "B000000002"},
		{"download", "B000000001", "--python", "fixture-python", "--marketplace", "uk"},
	} {
		code, out, errOut := invoke(args, deps)
		if code != 2 || out != "" || !strings.Contains(errOut, "Usage:") || strings.Contains(errOut, "secret1") || strings.Contains(errOut, "bad_asin12") {
			t.Fatal(args, code, out, errOut)
		}
	}
	for _, args := range [][]string{
		{"download", "--help"},
		{"download", "B000000001", "--help"},
	} {
		code, out, _ := invoke(args, deps)
		if code != 0 || !strings.Contains(out, "download <ASIN>") {
			t.Fatal(args, code, out)
		}
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid download invocation created root state", entries, err)
	}

	// Omitted --python resolves via TALETTY_PYTHON.
	t.Setenv("TALETTY_PYTHON", "fixture-python")
	client := &fakeAccount{download: readyDownloadResult(false, true)}
	code, out, errOut := invoke([]string{"download", "B000000001", "--json"}, fixture(t, client))
	if code != 0 || errOut != "" || !strings.Contains(out, `"state":"ready"`) {
		t.Fatalf("download without --python failed: code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestDownloadCompleteOutputAfterCloseAndArgumentPositions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, machine := range []bool{false, true} {
		for _, reused := range []bool{false, true} {
			for _, asinFirst := range []bool{true, false} {
				client := &fakeAccount{download: readyDownloadResult(reused, !reused)}
				deps := fixture(t, client)
				deps.terminal = func() (*os.File, error) { panic("download touched terminal") }
				var args []string
				if asinFirst {
					args = []string{"download", "B000000001", "--python", "fixture-python"}
					if machine {
						args = append(args, "--json")
					}
				} else {
					args = []string{"download", "--python", "fixture-python"}
					if machine {
						args = append(args, "--json")
					}
					args = append(args, "B000000001")
				}
				out := &afterCloseWriter{client: client}
				var diagnostics bytes.Buffer
				if code := run(context.Background(), args, out, &diagnostics, deps); code != 0 || diagnostics.Len() != 0 || !client.closed {
					t.Fatal(code, diagnostics.String())
				}
				if client.downloadBook != (model.BookID{Provider: "audible", Namespace: "us", ID: "B000000001"}) {
					t.Fatal("wrong bookID", client.downloadBook)
				}
				if machine {
					var payload struct {
						Download model.DownloadResult `json:"download"`
						Failure  *failure             `json:"failure"`
					}
					if json.Unmarshal(out.Bytes(), &payload) != nil || payload.Failure != nil || payload.Download.State != model.MediaReady || payload.Download.Reused != reused || payload.Download.Media == nil {
						t.Fatal(out.String())
					}
				} else {
					want := "Media downloaded: /tmp/taletty/downloads/B000000001.m4b\n"
					if reused {
						want = "Media ready (reused): /tmp/taletty/downloads/B000000001.m4b\n"
					}
					if out.String() != want {
						t.Fatal("unexpected plain output:", out.String())
					}
				}
			}
		}
	}
}

func TestDownloadPreservesCommittedMediaOnAuthDurabilityAndCloseFailures(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ready := readyDownloadResult(false, true)
	ready.AuthPersistence = model.PersistenceFailed
	ready.AuthFailure = &model.Failure{Code: "storage_failed", Message: "Authentication storage failed.", Retry: "none", OperationID: "cli-download"}

	uncertain := readyDownloadResult(false, true)
	uncertain.MediaDurability = model.PersistenceUncertain
	uncertain.MediaFailure = &model.Failure{Code: "storage_uncertain", Message: "Media published; durability not confirmed.", Retry: "none", OperationID: "cli-download"}

	denied := model.DownloadResult{
		OperationID:     "cli-download",
		BookID:          ready.BookID,
		State:           model.MediaAbsent,
		MediaDurability: model.PersistenceUnknown,
		AuthPersistence: model.PersistenceConfirmed,
		MediaFailure:    &model.Failure{Code: "entitlement_denied", Message: "Audible denied access to this title.", Retry: "none", OperationID: "cli-download"},
	}

	cases := []struct {
		name     string
		result   model.DownloadResult
		err      error
		closeErr error
		wantCode string
		hasMedia bool
	}{
		{"auth-failed-after-commit", ready, ready.AuthFailure, nil, "storage_failed", true},
		{"media-uncertain-after-commit", uncertain, uncertain.MediaFailure, nil, "storage_uncertain", true},
		{"close-failed-after-commit", readyDownloadResult(false, true), nil, errors.New("fixture-private-close"), "storage_failed", true},
		{"entitlement-denied", denied, denied.MediaFailure, nil, "entitlement_denied", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, machine := range []bool{false, true} {
				client := &fakeAccount{download: tc.result, downloadErr: tc.err, closeErr: tc.closeErr}
				args := []string{"download", "B000000001", "--python", "fixture-python"}
				if machine {
					args = append(args, "--json")
				}
				code, out, errOut := invoke(args, fixture(t, client))
				if code != 1 || !client.closed || strings.Contains(out+errOut, "fixture-private") {
					t.Fatal(code, out, errOut)
				}
				if machine {
					var payload struct {
						Download *model.DownloadResult `json:"download"`
						Failure  failure               `json:"failure"`
					}
					if json.Unmarshal([]byte(out), &payload) != nil || payload.Failure.Code != tc.wantCode || payload.Download == nil {
						t.Fatal(out)
					}
					if (payload.Download.Media != nil) != tc.hasMedia {
						t.Fatal("committed media presence mismatch", out)
					}
				} else {
					if tc.hasMedia != strings.Contains(errOut, "Media ready: /tmp/taletty/downloads/B000000001.m4b") {
						t.Fatal("plain error output lost ready media notice:", errOut)
					}
				}
			}
		})
	}
}

func TestDownloadPublicationVsCancellationAndOutputErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// 1. Committed/reconciled ready media wins late cancellation -> exit 0.
	ctx, cancel := context.WithCancel(context.Background())
	client := &fakeAccount{download: readyDownloadResult(false, true), cancel: cancel}
	var out, errOut bytes.Buffer
	if code := run(ctx, []string{"download", "B000000001", "--python", "fixture-python", "--json"}, &out, &errOut, fixture(t, client)); code != 0 || !client.closed {
		t.Fatal("publication did not win interrupt", code, out.String(), errOut.String())
	}
	// 2. Precommit cancellation -> exit 130 with download result preserved.
	ctx, cancel = context.WithCancel(context.Background())
	absent := model.DownloadResult{
		OperationID:     "cli-download",
		BookID:          model.BookID{Provider: "audible", Namespace: "us", ID: "B000000001"},
		State:           model.MediaAbsent,
		MediaDurability: model.PersistenceUnknown,
		AuthPersistence: model.PersistenceUnknown,
		MediaFailure:    &model.Failure{Code: "canceled", Message: "Operation canceled.", Retry: "user_retry", OperationID: "cli-download"},
	}
	client = &fakeAccount{download: absent, downloadErr: absent.MediaFailure, cancel: cancel}
	out.Reset()
	errOut.Reset()
	if code := run(ctx, []string{"download", "B000000001", "--python", "fixture-python", "--json"}, &out, &errOut, fixture(t, client)); code != 130 || !client.closed || !strings.Contains(out.String(), `"state":"absent"`) {
		t.Fatal("precommit cancel outcome wrong", code, out.String())
	}
	// 3. Broken output writer on success and failure returns exit 1 after closing session.
	for _, failErr := range []error{nil, absent.MediaFailure} {
		client = &fakeAccount{download: readyDownloadResult(false, true), downloadErr: failErr}
		if code := run(context.Background(), []string{"download", "B000000001", "--python", "fixture-python", "--json"}, brokenWriter{}, io.Discard, fixture(t, client)); code != 1 || !client.closed {
			t.Fatal("broken output writer did not fail cleanly", code)
		}
	}
}
