package model_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/susamn/taletty/engine/model"
)

func TestBookIDValidation(t *testing.T) {
	for _, id := range []model.BookID{
		{Provider: "audible", Namespace: "us", ID: "B000000001"},
		{Provider: "audible", Namespace: "de", ID: "a123456789"},
		{Provider: "local", Namespace: "library", ID: "uuid-123_456.7"},
		{Provider: "future_provider", Namespace: "catalog-2", ID: "123"},
	} {
		t.Run(id.Provider+"/"+id.Namespace, func(t *testing.T) {
			if err := id.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, id := range []model.BookID{
		{},
		{Provider: "Audible", Namespace: "us", ID: "B000000001"},
		{Provider: "audible", Namespace: "", ID: "B000000001"},
		{Provider: "audible", Namespace: "../us", ID: "B000000001"},
		{Provider: "audible", Namespace: "us", ID: "short"},
		{Provider: "audible", Namespace: "us", ID: "B00000000-"},
		{Provider: "audible", Namespace: "us", ID: "B0000000001"},
		{Provider: "local", Namespace: "library", ID: ""},
		{Provider: "local", Namespace: "library", ID: "../file"},
		{Provider: "local", Namespace: "library", ID: "/home/private/book"},
		{Provider: "local", Namespace: "library", ID: " note"},
		{Provider: "local", Namespace: "library", ID: "book\x1b[31m"},
	} {
		if !errors.Is(id.Validate(), model.ErrInvalidBookID) {
			t.Fatalf("expected safe invalid-identity error for %#v", id)
		}
	}
}

func TestBookIdentityRoundtripAndCatalogSeparation(t *testing.T) {
	original := model.BookID{Provider: "audible", Namespace: "us", ID: "B000000001"}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded model.BookID
	if err := json.Unmarshal(data, &decoded); err != nil || decoded != original {
		t.Fatalf("identity roundtrip: %v, %#v", err, decoded)
	}
	otherMarket, otherProvider := original, original
	otherMarket.Namespace, otherProvider.Provider = "de", "local"
	identities := map[model.BookID]bool{original: true, otherMarket: true, otherProvider: true}
	if len(identities) != 3 {
		t.Fatal("different catalogs/providers collided")
	}
}
