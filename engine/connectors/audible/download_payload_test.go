package audible

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/susamn/taletty/engine/connectors/audible/internal/bridge"
	"github.com/susamn/taletty/engine/model"
)

const downloadBook = `{"provider":"audible","namespace":"us","id":"B000000001"}`
const chapterPayload = `{"id":"0","title":"Fixture","start_ms":0,"end_ms":0,"children":[]}`
const outcomePayload = `{"ready":true,"reused":false,"published":true,"media_code":null,"auth_code":null,"chapters":[` + chapterPayload + `]}`

func outcomeFrame(raw string) bridge.Frame { return resultPayload(`{"outcome":` + raw + `}`) }
func outcomeMap(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if json.Unmarshal([]byte(outcomePayload), &v) != nil {
		t.Fatal("fixture")
	}
	return v
}
func TestDownloadPayloadFactsAndStrictTypes(t *testing.T) {
	out, err := downloadTerminal(outcomeFrame(outcomePayload))
	if err != nil || !out.ready || out.reused || out.published == nil || !*out.published || len(out.chapters) != 1 || out.chapters[0].End == nil || *out.chapters[0].End != 0 || out.chapters[0].Children == nil {
		t.Fatal("facts/zero lost", err)
	}
	for _, codes := range [][2]string{{"storage_uncertain", "storage_failed"}, {"internal", "storage_uncertain"}, {"canceled", ""}} {
		v := outcomeMap(t)
		v["chapters"] = nil
		if codes[0] != "" {
			v["media_code"] = codes[0]
		}
		if codes[1] != "" {
			v["auth_code"] = codes[1]
		}
		raw, _ := json.Marshal(v)
		out, err := downloadTerminal(outcomeFrame(string(raw)))
		if err != nil || out.mediaCode == nil || *out.mediaCode != codes[0] {
			t.Fatal("independent failure", err)
		}
	}
	for _, code := range []string{"network", "storage_failed", "storage_uncertain", "conflict", "preparation_failed", "disk_full", "permission_denied", "invalid_input", "protocol_error"} {
		frame := bridge.Frame{Type: bridge.Failure, Raw: []byte(`{"failure":{"code":"` + code + `","message":"fixture-private"}}`)}
		_, err := downloadTerminal(frame)
		if failureCode(t, err) != code || strings.Contains(err.Error(), "fixture-private") {
			t.Fatal(err)
		}
	}
	if f := downloadFailure("unknown_code", "op"); f.Code != "internal" || f.OperationID != "op" {
		t.Fatal("unknown download failure fallback", f)
	}
	if f := downloadFailure("invalid_input", "op"); f.Message != "Invalid download request." {
		t.Fatal("invalid_input message", f)
	}
	if f := downloadFailure("protocol_error", "op"); f.Message != "Audible download response was invalid." {
		t.Fatal("protocol_error message", f)
	}
	// Known empty chapters [] vs unknown null on fresh publication, and auth-only / canceled combinations.
	emptyChapters, err := downloadTerminal(outcomeFrame(`{"ready":true,"reused":false,"published":true,"media_code":null,"auth_code":null,"chapters":[]}`))
	if err != nil || emptyChapters.chapters == nil || len(emptyChapters.chapters) != 0 {
		t.Fatal("known empty chapters lost", err)
	}
	nullChapters, err := downloadTerminal(outcomeFrame(`{"ready":true,"reused":false,"published":true,"media_code":null,"auth_code":null,"chapters":null}`))
	if err != nil || nullChapters.chapters != nil {
		t.Fatal("unknown chapters invented", err)
	}
	authOnly, err := downloadTerminal(outcomeFrame(`{"ready":false,"reused":false,"published":false,"media_code":null,"auth_code":"storage_failed","chapters":null}`))
	if err != nil || authOnly.authCode == nil || *authOnly.authCode != "storage_failed" || authOnly.mediaCode != nil {
		t.Fatal("auth-only failure rejected", err)
	}
	canceledReady, err := downloadTerminal(outcomeFrame(`{"ready":true,"reused":false,"published":true,"media_code":"canceled","auth_code":null,"chapters":null}`))
	if err != nil || !canceledReady.ready || canceledReady.mediaCode == nil || *canceledReady.mediaCode != "canceled" {
		t.Fatal("ready+canceled rejected", err)
	}
	canceledNotReady, err := downloadTerminal(outcomeFrame(`{"ready":false,"reused":false,"published":false,"media_code":"canceled","auth_code":"storage_failed","chapters":null}`))
	if err != nil || canceledNotReady.ready || canceledNotReady.mediaCode == nil || *canceledNotReady.mediaCode != "canceled" || canceledNotReady.authCode == nil {
		t.Fatal("not-ready+canceled+auth rejected", err)
	}
	for _, field := range []string{"ready", "reused", "published", "media_code", "auth_code", "chapters"} {
		for _, bad := range []any{"missing", 123, []any{}, map[string]any{}} {
			if _, array := bad.([]any); array && field == "chapters" {
				continue
			}
			v := outcomeMap(t)
			v[field] = bad
			if marker, ok := bad.(string); ok && marker == "missing" {
				delete(v, field)
			}
			raw, _ := json.Marshal(v)
			if _, err := downloadTerminal(outcomeFrame(string(raw))); err == nil {
				t.Fatalf("bad %s accepted: %s", field, raw)
			}
		}
	}
	for _, patch := range []map[string]any{
		{"ready": nil}, {"reused": nil}, {"media_code": "fixture-private"}, {"auth_code": "internal"},
		{"reused": true}, {"ready": false}, {"published": nil}, {"published": false},
		{"ready": false, "published": false, "chapters": nil},
		{"ready": false, "reused": true, "published": false, "chapters": nil, "media_code": "internal"},
		{"reused": true, "published": false, "chapters": []any{}},
		{"published": false, "chapters": []any{}},
		{"published": nil, "chapters": []any{}},
	} {
		v := outcomeMap(t)
		for k, val := range patch {
			v[k] = val
		}
		raw, _ := json.Marshal(v)
		if _, err := downloadTerminal(outcomeFrame(string(raw))); err == nil {
			t.Fatal("contradiction accepted", string(raw))
		}
	}
	for _, raw := range []string{`null`, `[]`, `{}`, `{"outcome":null}`, `{"outcome":{}}`, `{"outcome":0}`, `{"outcome":` + outcomePayload + `,"future":{"x":0,"\u0078":1}}`, `{"outcome":` + outcomePayload + `,"future":"\ud800"}`, `{"outcome":` + outcomePayload + `,"future":` + strings.Repeat(`{"a":`, 2*model.MaxChapterDepth+9) + `0` + strings.Repeat(`}`, 2*model.MaxChapterDepth+9) + `}`} {
		if _, err := downloadTerminal(resultPayload(raw)); err == nil {
			t.Fatal("invalid schema accepted", raw)
		}
	}
	for _, frame := range []bridge.Frame{{Type: bridge.Stage, Raw: []byte(`{}`)}, {Type: bridge.Failure, Raw: []byte(`{"failure":null}`)}, {Type: bridge.Failure, Raw: []byte(`{"failure":{"Code":"internal"}}`)}, {Type: bridge.Failure, Raw: []byte(`{"failure":{"code":"fixture-private"}}`)}} {
		if _, err := downloadTerminal(frame); err == nil {
			t.Fatal("bad terminal accepted")
		}
	}
}
func TestDownloadChapterSchemaLimitsAndTimeConversion(t *testing.T) {
	var node map[string]any
	json.Unmarshal([]byte(chapterPayload), &node)
	for key, values := range map[string][]any{
		"id": {nil, 0, "", "../bad", strings.Repeat("a", 257)}, "title": {nil, 0, "", "fixture-private\x1b", strings.Repeat("a", 4097)},
		"start_ms": {nil, -1, 0.1, "0", true, int64(9223372036855)},
		"end_ms":   {nil, -1, 0.1, "0", true, int64(9223372036855)},
		"children": {nil, 0, []any{nil}, []any{map[string]any{}}},
	} {
		for _, bad := range values {
			copy := map[string]any{}
			for k, v := range node {
				copy[k] = v
			}
			copy[key] = bad
			raw, _ := json.Marshal(copy)
			if _, err := downloadTerminal(outcomeFrame(strings.Replace(outcomePayload, chapterPayload, string(raw), 1))); err == nil {
				t.Fatalf("chapter %s accepted", raw)
			}
		}
	}
	for _, key := range []string{"id", "title", "start_ms", "children"} {
		copy := map[string]any{}
		for k, v := range node {
			copy[k] = v
		}
		delete(copy, key)
		raw, _ := json.Marshal(copy)
		if _, err := downloadTerminal(outcomeFrame(strings.Replace(outcomePayload, chapterPayload, string(raw), 1))); err == nil {
			t.Fatal("missing node field accepted")
		}
	}
	if _, err := downloadTerminal(outcomeFrame(strings.Replace(outcomePayload, chapterPayload, `{"id":"0","title":"Bad\ud800","start_ms":0,"children":[]}`, 1))); err == nil {
		t.Fatal("lone surrogate chapter title accepted")
	}
	exactNode := fmt.Sprintf(`{"id":%q,"title":%q,"start_ms":10,"end_ms":10,"children":[]}`, strings.Repeat("a", 256), strings.Repeat("b", 4096))
	if out, err := downloadTerminal(outcomeFrame(strings.Replace(outcomePayload, chapterPayload, exactNode, 1))); err != nil || out.chapters[0].Start != 10*time.Millisecond || *out.chapters[0].End != 10*time.Millisecond {
		t.Fatal("exact chapter bounds/zero-length rejected", err)
	}
	crossSubtreeDup := `[{"id":"0","title":"A","start_ms":0,"children":[{"id":"dup","title":"C1","start_ms":0,"children":[]}]},{"id":"1","title":"B","start_ms":0,"children":[{"id":"dup","title":"C2","start_ms":0,"children":[]}]}]`
	if _, err := downloadTerminal(outcomeFrame(strings.Replace(outcomePayload, `[`+chapterPayload+`]`, crossSubtreeDup, 1))); err == nil {
		t.Fatal("cross-subtree duplicate chapter ID accepted")
	}
	sparse := strings.Replace(chapterPayload, `,"end_ms":0`, "", 1)
	out, err := downloadTerminal(outcomeFrame(strings.Replace(outcomePayload, chapterPayload, sparse, 1)))
	if err != nil || out.chapters[0].End != nil {
		t.Fatal("unknown end invented", err)
	}
	max := strings.ReplaceAll(chapterPayload, `"start_ms":0,"end_ms":0`, `"start_ms":9223372036854,"end_ms":9223372036854`)
	out, err = downloadTerminal(outcomeFrame(strings.Replace(outcomePayload, chapterPayload, max, 1)))
	if err != nil || out.chapters[0].Start != 9223372036854*time.Millisecond {
		t.Fatal("time overflow bound", err)
	}
	backward := strings.Replace(chapterPayload, `"start_ms":0`, `"start_ms":1`, 1)
	if _, err := downloadTerminal(outcomeFrame(strings.Replace(outcomePayload, chapterPayload, backward, 1))); err == nil {
		t.Fatal("negative interval accepted")
	}
	duplicate := strings.Replace(outcomePayload, `[`+chapterPayload+`]`, `[`+chapterPayload+`,`+chapterPayload+`]`, 1)
	if _, err := downloadTerminal(outcomeFrame(duplicate)); err == nil {
		t.Fatal("duplicate chapter IDs")
	}
	tree := chapterPayload
	for i := 1; i < model.MaxChapterDepth; i++ {
		tree = fmt.Sprintf(`{"id":"%d","title":"Depth","start_ms":0,"children":[%s]}`, i, tree)
	}
	raw := strings.Replace(outcomePayload, chapterPayload, tree, 1)
	if _, err := downloadTerminal(outcomeFrame(raw)); err != nil {
		t.Fatal("exact provider depth rejected", err)
	}
	tree = fmt.Sprintf(`{"id":"extra","title":"Depth","start_ms":0,"children":[%s]}`, tree)
	if _, err := downloadTerminal(outcomeFrame(strings.Replace(outcomePayload, chapterPayload, tree, 1))); err == nil {
		t.Fatal("provider depth overflow")
	}
	count := model.MaxChapters
	if _, err := downloadChapters([]byte(`[`+chapterPayload+`]`), 1, &count); err == nil {
		t.Fatal("count overflow")
	}
	for _, raw := range []string{`null`, `{}`, `[0]`, `[{"id":"x","title":"x","start_ms":0,"children":{}}]`} {
		count := 0
		if _, err := downloadChapters([]byte(raw), 1, &count); err == nil {
			t.Fatal("malformed children accepted")
		}
	}
	for _, state := range []model.MediaState{model.MediaReady, model.MediaAbsent, model.MediaInvalid, model.MediaUnsafe, model.MediaUnknown} {
		got, err := reconciliationTerminal(resultPayload(`{"state":"` + string(state) + `","future":true}`))
		if err != nil || got != state {
			t.Fatal("state changed", err)
		}
	}
	for _, frame := range []bridge.Frame{
		resultPayload(`{"State":"ready"}`), resultPayload(`{"state":"fixture-private"}`), resultPayload(`{"state":null}`),
		resultPayload(`{"state":"ready","state":"absent"}`), resultPayload(`{}`), resultPayload(`{"state":123}`),
		resultPayload(`[]`), resultPayload(`null`), resultPayload(`{"state":"ready","future":"\ud800"}`),
		resultPayload(`{"state":"ready","future":{"x":0,"\u0078":1}}`),
		resultPayload(`{"state":"ready","future":` + strings.Repeat(`{"a":`, 65) + `0` + strings.Repeat(`}`, 65) + `}`),
		{Type: bridge.Failure, Raw: []byte(`{"state":"ready"}`)},
	} {
		got, err := reconciliationTerminal(frame)
		if err == nil || got != model.MediaUnknown {
			t.Fatal("bad recovery trusted")
		}
	}
}
func FuzzDownloadPayload(f *testing.F) {
	for _, raw := range []string{`{"outcome":` + outcomePayload + `}`, `null`, `{"outcome":{"chapters":[{}]}}`, `{"x":"\ud800"}`} {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > bridge.MaxFrameBytes {
			t.Skip()
		}
		out, err := downloadTerminal(resultPayload(raw))
		if err == nil && model.ValidateChapters(out.chapters) != nil {
			t.Fatal("invalid public chapters")
		}
	})
}
