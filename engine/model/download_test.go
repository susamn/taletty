package model_test

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/susamn/taletty/engine/model"
)

func media() model.Media {
	return model.Media{BookID: book().ID, Path: "/private/downloads/T000000001.m4b", Format: "m4b"}
}
func chapter() model.Chapter {
	return model.Chapter{ID: "0", Title: "Chapter", Children: []model.Chapter{}}
}
func TestMediaChapterOptionalityAndOwnership(t *testing.T) {
	for _, chapters := range [][]model.Chapter{nil, {}, {chapter()}, {{ID: "0", Title: "CJK中文 🎧", End: pointer(time.Duration(0)), Children: []model.Chapter{{ID: "0.0", Title: "Child", Start: time.Millisecond}}}}} {
		value := media()
		value.Chapters = chapters
		if value.Validate() != nil {
			t.Fatal("valid media rejected")
		}
		data, err := json.Marshal(value)
		var restored model.Media
		if err != nil || json.Unmarshal(data, &restored) != nil || !reflect.DeepEqual(value, restored) {
			t.Fatal("optionality lost", string(data), err)
		}
		var fields map[string]json.RawMessage
		json.Unmarshal(data, &fields)
		if _, ok := fields["chapters"]; ok != (chapters != nil) {
			t.Fatal("unknown hierarchy invented")
		}
	}
	value := media()
	node := chapter()
	node.End = pointer(time.Duration(0))
	node.Children = []model.Chapter{{ID: "0.0", Title: "Child"}}
	value.Chapters = []model.Chapter{node}
	original := model.DownloadResult{Media: &value, Published: pointer(true), MediaFailure: &model.Failure{Code: "storage_uncertain", Message: "media"}, AuthFailure: &model.Failure{Code: "storage_failed", Message: "auth"}}
	copy := original.Clone()
	*copy.Published = false
	copy.Media.Path = "mutated"
	copy.Media.Chapters[0].Title = "mutated"
	*copy.Media.Chapters[0].End = 1
	copy.Media.Chapters[0].Children[0].Title = "mutated"
	copy.MediaFailure.Message = "mutated"
	copy.AuthFailure.Message = "mutated"
	if !*original.Published || original.Media.Path != media().Path || original.Media.Chapters[0].Title != "Chapter" || *original.Media.Chapters[0].End != 0 || original.Media.Chapters[0].Children[0].Title != "Child" || original.MediaFailure.Message != "media" || original.AuthFailure.Message != "auth" {
		t.Fatal("shared result pointers")
	}
	if !reflect.DeepEqual(model.DownloadResult{}, (model.DownloadResult{}).Clone()) {
		t.Fatal("zero clone changed")
	}
	// Unknown vs known-empty hierarchy and nil vs false commit survive Clone and JSON.
	for _, chapters := range [][]model.Chapter{nil, {}} {
		item := media()
		item.Chapters = chapters
		res := model.DownloadResult{
			OperationID: "op-1", BookID: item.BookID, State: model.MediaReady,
			Media: &item, Published: pointer(false),
			MediaDurability: model.PersistenceUnknown, AuthPersistence: model.PersistenceUnknown,
		}
		cloned := res.Clone()
		if (cloned.Media.Chapters == nil) != (chapters == nil) || cloned.Published == nil || *cloned.Published {
			t.Fatal("clone lost chapter/commit distinction")
		}
		*cloned.Published = true
		if *res.Published {
			t.Fatal("clone aliased false commit")
		}
	}
	unknownCommit := model.DownloadResult{OperationID: "op-2", BookID: media().BookID, State: model.MediaAbsent, MediaDurability: model.PersistenceUnknown, AuthPersistence: model.PersistenceUnknown}
	encoded, err := json.Marshal(unknownCommit)
	var rawFields map[string]json.RawMessage
	if err != nil || json.Unmarshal(encoded, &rawFields) != nil || string(rawFields["published"]) != "null" {
		t.Fatal("nullable published lost", string(encoded), err)
	}
	// Chapter JSON preserves start_ns=0, omits nil end_ns/children, keeps &0/[] when known.
	leaf := model.Chapter{ID: "0", Title: "Leaf"}
	leafJSON, err := json.Marshal(leaf)
	var leafFields map[string]json.RawMessage
	if err != nil || json.Unmarshal(leafJSON, &leafFields) != nil || string(leafFields["start_ns"]) != "0" {
		t.Fatal("chapter start_ns lost", string(leafJSON), err)
	}
	if _, ok := leafFields["end_ns"]; ok {
		t.Fatal("unknown end_ns invented")
	}
	if _, ok := leafFields["children"]; ok {
		t.Fatal("unknown children invented")
	}
}
func TestMediaAndChapterValidationBounds(t *testing.T) {
	for _, path := range []string{"", "relative.m4b", "/private/../book.m4b", "/private//book.m4b", "/private/book.m4b/", "/private/book.mp3", "/private/book.M4B", "/private/fixture-private\n.m4b", "/private/fixture-private\u202e.m4b", "/private/book\\.m4b", string([]byte{'/', 0xff})} {
		value := media()
		value.Path = path
		if value.Validate() == nil {
			t.Fatal("unsafe path accepted")
		}
	}
	local := model.Media{BookID: model.BookID{Provider: "local", Namespace: "library", ID: "uuid-1"}, Path: "/private/downloads/uuid-1.m4b", Format: "m4b"}
	if local.Validate() != nil {
		t.Fatal("provider-neutral media rejected")
	}
	value := media()
	value.BookID = model.BookID{}
	if value.Validate() == nil {
		t.Fatal("bad identity")
	}
	value = media()
	value.Format = "fixture-private"
	if value.Validate() == nil {
		t.Fatal("bad format")
	}
	value = media()
	value.Chapters = []model.Chapter{{ID: "dup", Title: "A"}, {ID: "dup", Title: "B"}}
	if value.Validate() == nil {
		t.Fatal("invalid chapters accepted by Media.Validate")
	}
	for _, mutate := range []func(*model.Chapter){
		func(c *model.Chapter) { c.ID = "" }, func(c *model.Chapter) { c.ID = "../bad" }, func(c *model.Chapter) { c.ID = strings.Repeat("a", 257) },
		func(c *model.Chapter) { c.Title = "" }, func(c *model.Chapter) { c.Title = " " }, func(c *model.Chapter) { c.Title = "fixture-private\x1b" },
		func(c *model.Chapter) { c.Title = "fixture-private\u202e" }, func(c *model.Chapter) { c.Title = string([]byte{0xff}) },
		func(c *model.Chapter) { c.Title = strings.Repeat("a", 4097) },
		func(c *model.Chapter) { c.Start = -1 }, func(c *model.Chapter) { c.End = pointer(time.Duration(-1)) },
		func(c *model.Chapter) { c.Start = 2; c.End = pointer(time.Duration(1)) },
		func(c *model.Chapter) { c.Children = []model.Chapter{*c} },
	} {
		node := chapter()
		mutate(&node)
		if err := model.ValidateChapters([]model.Chapter{node}); err == nil || strings.Contains(err.Error(), "fixture-private") {
			t.Fatal("unsafe metadata", err)
		}
	}
	if model.ValidateChapters([]model.Chapter{
		{ID: "0", Title: "A", Children: []model.Chapter{{ID: "dup", Title: "C1"}}},
		{ID: "1", Title: "B", Children: []model.Chapter{{ID: "dup", Title: "C2"}}},
	}) == nil {
		t.Fatal("cross-subtree duplicate chapter ID accepted")
	}
	exact := model.Chapter{
		ID: strings.Repeat("a", 256), Title: strings.Repeat("b", 4096),
		Start: time.Duration(math.MaxInt64), End: pointer(time.Duration(math.MaxInt64)),
	}
	if model.ValidateChapters([]model.Chapter{exact}) != nil {
		t.Fatal("exact chapter bounds rejected")
	}
	nodes := make([]model.Chapter, model.MaxChapters+1)
	for i := range nodes {
		nodes[i] = chapter()
		nodes[i].ID = fmt.Sprint(i)
	}
	if model.ValidateChapters(nodes[:model.MaxChapters]) != nil || model.ValidateChapters(nodes) == nil {
		t.Fatal("node count bound")
	}
	deep := chapter()
	for i := model.MaxChapterDepth - 1; i > 0; i-- {
		deep = model.Chapter{ID: fmt.Sprint(i), Title: "Depth", Children: []model.Chapter{deep}}
	}
	if model.ValidateChapters([]model.Chapter{deep}) != nil {
		t.Fatal("exact depth rejected")
	}
	deep = model.Chapter{ID: "extra", Title: "Depth", Children: []model.Chapter{deep}}
	if model.ValidateChapters([]model.Chapter{deep}) == nil {
		t.Fatal("depth overflow")
	}
}

func TestOperationEventProgressOptionality(t *testing.T) {
	for _, total := range []*int64{nil, pointer(int64(0)), pointer(int64(2048))} {
		event := model.OperationEvent{
			OperationID: "op-progress",
			Sequence:    3,
			Type:        model.Progress,
			Bytes:       pointer(int64(0)),
			Total:       total,
		}
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["bytes"]) != "0" {
			t.Fatal("known zero bytes lost", string(data))
		}
		_, hasTotal := fields["total"]
		if hasTotal != (total != nil) {
			t.Fatal("total optionality lost", string(data))
		}
		if total != nil && *total == 0 && string(fields["total"]) != "0" {
			t.Fatal("known zero total lost", string(data))
		}
		var restored model.OperationEvent
		if err := json.Unmarshal(data, &restored); err != nil || !reflect.DeepEqual(event, restored) {
			t.Fatal("progress event roundtrip failed", err, string(data))
		}
	}
	started := model.OperationEvent{OperationID: "op-progress", Sequence: 1, Type: model.Started}
	data, err := json.Marshal(started)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["bytes"]; ok {
		t.Fatal("unexpected bytes on started event", string(data))
	}
	if _, ok := fields["total"]; ok {
		t.Fatal("unexpected total on started event", string(data))
	}
}
