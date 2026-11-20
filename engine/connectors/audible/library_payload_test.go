package audible

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/susamn/taletty/engine/connectors/audible/internal/bridge"
	"github.com/susamn/taletty/engine/model"
)

const bookPayload = `{"id":{"provider":"audible","namespace":"us","id":"T000000001"},"title":"Fixture"}`

func pagePayload(books string) bridge.Frame {
	return bridge.Frame{Type: bridge.Stage, Raw: json.RawMessage(`{"stage":"library_page","books":[` + books + `]}`)}
}
func resultPayload(raw string) bridge.Frame {
	return bridge.Frame{Type: bridge.Result, Raw: json.RawMessage(raw)}
}
func failureCode(t *testing.T, err error) string {
	t.Helper()
	var failure *model.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("missing safe failure: %v", err)
	}
	return failure.Code
}

func TestLibraryPayloadOptionalityAndStrictBoundaries(t *testing.T) {
	rich := strings.TrimSuffix(bookPayload, "}") + `,"subtitle":"","authors":[],"narrators":[{"name":"CJK中文 e\u0301"}],"duration_ms":0,"future":{"number":1e9999,"text":"\ud83c\udfa7"}}`
	candidate := libraryCandidate{}
	if err := candidate.page(pagePayload(rich)); err != nil {
		t.Fatal(err)
	}
	value := candidate.books[0]
	if value.Subtitle == nil || *value.Subtitle != "" || value.Authors == nil || len(value.Authors) != 0 || len(value.Narrators) != 1 || value.Duration == nil || *value.Duration != 0 {
		t.Fatal("optionality lost")
	}
	if candidate.finish(resultPayload(`{"count":1,"future":true}`)) != nil {
		t.Fatal("completion rejected")
	}
	sparse, err := libraryBook(json.RawMessage(bookPayload))
	if err != nil || sparse.Authors != nil || sparse.Narrators != nil || sparse.Duration != nil || sparse.Subtitle != nil {
		t.Fatal("unknown invented", err)
	}
	maximum := strings.TrimSuffix(bookPayload, "}") + `,"duration_ms":9223372036854}`
	bounded, err := libraryBook(json.RawMessage(maximum))
	if err != nil || *bounded.Duration != 9223372036854*time.Millisecond {
		t.Fatal("conversion boundary", err)
	}
	if err := (&libraryCandidate{}).finish(resultPayload(`{"count":0}`)); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"not_authenticated", "runtime_missing", "runtime_incompatible", "network", "throttled", "entitlement_denied", "unsupported", "canceled", "internal", "storage_failed", "storage_uncertain", "busy", "invalid_input", "protocol_error"} {
		frame := bridge.Frame{Type: bridge.Failure, Raw: json.RawMessage(`{"failure":{"code":"` + code + `","message":"fixture-private-ignore"}}`)}
		err := candidate.finish(frame)
		if failureCode(t, err) != code || strings.Contains(err.Error(), "fixture-private") {
			t.Fatal("unsafe failure", err)
		}
	}
	if failure := libraryFailure("fixture-private", ""); failure.Code != "internal" || failure.Error() != libraryFailures["internal"].message {
		t.Fatal("unknown code echoed")
	}
}

func TestLibraryRejectsMalformedPagesBooksAndCompletion(t *testing.T) {
	fields := map[string]any{}
	if err := json.Unmarshal([]byte(bookPayload), &fields); err != nil {
		t.Fatal(err)
	}
	var books []string
	for key, values := range map[string][]any{
		"id":    {nil, []any{}, map[string]any{"Provider": "audible", "namespace": "us", "id": "T000000001"}, map[string]any{"provider": "audible", "namespace": "de", "id": "T000000001"}, map[string]any{"provider": "local", "namespace": "us", "id": "T000000001"}},
		"title": {nil, 0, "", "fixture-private\n", "\u202e"}, "subtitle": {nil, 0, "bad\t"},
		"authors":     {nil, 0, []any{nil}, []any{map[string]any{"Name": "bad"}}, []any{map[string]any{"name": ""}}},
		"narrators":   {nil, []any{map[string]any{"name": "bad\u2066"}}},
		"duration_ms": {nil, -1, 0.5, "10", true, 9223372036855, int64(9223372036854775807)},
	} {
		for _, value := range values {
			copy := map[string]any{}
			for k, v := range fields {
				copy[k] = v
			}
			copy[key] = value
			raw, err := json.Marshal(copy)
			if err != nil {
				t.Fatal(err)
			}
			books = append(books, string(raw))
		}
	}
	books = append(books, `null`, `[]`, `{}`, strings.Replace(bookPayload, `"title"`, `"Title"`, 1), strings.Replace(bookPayload, `"Fixture"`, `"\ud800"`, 1), strings.Replace(bookPayload, `"Fixture"`, `"\udc00"`, 1), strings.Replace(bookPayload, `"Fixture"`, `"\ud800\u0041"`, 1), strings.TrimSuffix(bookPayload, "}")+`,"future":{"k":0,"\u006b":1}}`)
	for _, book := range books {
		if err := (&libraryCandidate{}).page(pagePayload(book)); !errors.Is(err, bridge.ErrProtocol) {
			t.Fatal("bad book accepted")
		}
	}
	for _, raw := range []string{`null`, `[]`, `{}`, `{"Stage":"library_page","books":[]}`, `{"stage":"other","books":[]}`, `{"stage":"library_page","books":null}`, `{"stage":"library_page","books":[]}`, `{"stage":"library_page","books":{}}`, `{"stage":"library_page","books":[],"books":[]}`, "{\"x\":\"\xff\"}", `{"x":` + strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66) + `}`} {
		if err := (&libraryCandidate{}).page(bridge.Frame{Type: bridge.Stage, Raw: []byte(raw)}); err == nil {
			t.Fatal("malformed frame accepted")
		}
	}
	frame := pagePayload(bookPayload)
	for _, candidate := range []libraryCandidate{{pages: 10000}, {bytes: maxLibraryBytes}, {seen: map[model.BookID]bool{{Provider: "audible", Namespace: "us", ID: "T000000001"}: true}}} {
		if candidate.page(frame) == nil {
			t.Fatal("limit/duplicate accepted")
		}
	}
	repeated := libraryCandidate{}
	if repeated.page(frame) != nil || repeated.page(frame) == nil {
		t.Fatal("repeated book accepted")
	}
	many := strings.TrimSuffix(strings.Repeat(bookPayload+",", 101), ",")
	if (&libraryCandidate{}).page(pagePayload(many)) == nil {
		t.Fatal("oversize page accepted")
	}
	frame.Type = bridge.Progress
	if (&libraryCandidate{}).page(frame) == nil {
		t.Fatal("progress accepted")
	}
	for _, frame := range []bridge.Frame{resultPayload(`{"count":-1}`), resultPayload(`{"Count":0}`), resultPayload(`{"count":null}`), resultPayload(`{"count":0.0}`), resultPayload(`{"count":1}`), resultPayload(`{"count":0,"count":0}`), {Type: bridge.Failure, Raw: []byte(`{"failure":null}`)}, {Type: bridge.Failure, Raw: []byte(`{"failure":{"code":"fixture-private"}}`)}, {Type: bridge.Stage, Raw: []byte(`{"count":0}`)}} {
		if (&libraryCandidate{}).finish(frame) == nil {
			t.Fatal("invalid terminal accepted")
		}
	}
	// Exactly at the cumulative byte cap is accepted, then the next page fails.
	exact := libraryCandidate{bytes: maxLibraryBytes - len(pagePayload(bookPayload).Raw)}
	if exact.page(pagePayload(bookPayload)) != nil || exact.bytes != maxLibraryBytes {
		t.Fatal("exact wire bound")
	}
	for _, raw := range []string{"", `{"key"`, `{"key":`, `[1`, `{`} {
		decoder := json.NewDecoder(strings.NewReader(raw))
		if uniqueValue(decoder, 0) == nil {
			t.Fatal("decoder fault accepted")
		}
	}
}

func FuzzLibraryPayload(f *testing.F) {
	for _, seed := range []string{string(pagePayload(bookPayload).Raw), `null`, `{"future":"\ud800"}`, `{"x":{"k":1,"k":2}}`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > bridge.MaxFrameBytes {
			t.Skip()
		}
		frame := bridge.Frame{Type: bridge.Stage, Raw: []byte(raw)}
		candidate := libraryCandidate{}
		if candidate.page(frame) == nil {
			for _, book := range candidate.books {
				if book.Validate() != nil {
					t.Fatal("invalid public metadata")
				}
			}
		}
	})
}
