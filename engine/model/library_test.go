package model_test

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/susamn/taletty/engine/model"
)

func pointer[T any](value T) *T { return &value }
func book() model.Book {
	return model.Book{ID: model.BookID{Provider: "audible", Namespace: "us", ID: "T000000001"}, Title: "Fixture"}
}

func TestBookOptionalJSONRoundtrip(t *testing.T) {
	for _, value := range []model.Book{
		book(),
		{ID: book().ID, Title: "CJK中文 e\u0301 🎧", Subtitle: pointer(""), Authors: []model.Contributor{}, Narrators: []model.Contributor{}, Duration: pointer(time.Duration(0))},
		{ID: model.BookID{Provider: "local", Namespace: "library", ID: "uuid-1"}, Title: "Local", Subtitle: pointer("Subtitle"), Authors: []model.Contributor{{Name: "Author"}}, Narrators: []model.Contributor{{Name: "Narrator"}}, Duration: pointer(time.Duration(math.MaxInt64))},
	} {
		if err := value.Validate(); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		for key, known := range map[string]bool{"subtitle": value.Subtitle != nil, "authors": value.Authors != nil, "narrators": value.Narrators != nil, "duration_ns": value.Duration != nil} {
			_, exists := fields[key]
			if exists != known {
				t.Fatalf("optional %s presence=%t want=%t: %s", key, exists, known, data)
			}
		}
		var restored model.Book
		if err := json.Unmarshal(data, &restored); err != nil || !reflect.DeepEqual(value, restored) || restored.Validate() != nil {
			t.Fatal("roundtrip", err, string(data))
		}
		if value.Duration != nil && *value.Duration == 0 && string(fields["duration_ns"]) != "0" {
			t.Fatal("known zero lost")
		}
		if value.Authors != nil && len(value.Authors) == 0 && string(fields["authors"]) != "[]" {
			t.Fatal("known empty lost")
		}
	}
	// Provider-neutral model does not decide which catalogs a connector supports.
	other := book()
	other.ID.Namespace = "de"
	if err := other.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidBookMetadataIsSafeAndUnchanged(t *testing.T) {
	var cases []model.Book
	broken := book()
	broken.ID.ID = "fixture-private-invalid"
	cases = append(cases, broken)
	for _, text := range []string{"", " \u2003 ", "fixture-private\x00", "fixture-private\n", "fixture-private\x1b[31m", "fixture-private\x9b", "fixture-private\u202e", string([]byte{0xff})} {
		value := book()
		value.Title = text
		cases = append(cases, value)
	}
	for _, text := range []string{"fixture-private\t", "fixture-private\u2066", string([]byte{0xfe})} {
		value := book()
		value.Subtitle = pointer(text)
		cases = append(cases, value)
	}
	for _, duration := range []time.Duration{-1, time.Duration(math.MinInt64)} {
		value := book()
		value.Duration = pointer(duration)
		cases = append(cases, value)
	}
	for _, name := range []string{"", " ", "fixture-private\r", "fixture-private\u200b", string([]byte{0xff})} {
		value := book()
		value.Authors = []model.Contributor{{Name: name}}
		cases = append(cases, value)
		value = book()
		value.Narrators = []model.Contributor{{Name: name}}
		cases = append(cases, value)
	}
	for _, value := range cases {
		before := value
		if value.Subtitle != nil {
			before.Subtitle = pointer(*value.Subtitle)
		}
		if value.Duration != nil {
			before.Duration = pointer(*value.Duration)
		}
		before.Authors = append([]model.Contributor(nil), value.Authors...)
		before.Narrators = append([]model.Contributor(nil), value.Narrators...)
		err := value.Validate()
		if !errors.Is(err, model.ErrInvalidBook) || strings.Contains(err.Error(), "fixture-private") {
			t.Fatal("unsafe/absent metadata rejection", err)
		}
		if !reflect.DeepEqual(before, value) {
			t.Fatal("validation rewrote input")
		}
	}
	// U+009B is valid UTF-8 but still a terminal control, unlike malformed byte 0x9b.
	value := book()
	value.Title = "Fixture\u009b31m"
	if value.Validate() == nil {
		t.Fatal("C1 control accepted")
	}
}
