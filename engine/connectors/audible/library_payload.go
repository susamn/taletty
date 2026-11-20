package audible

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/susamn/taletty/engine/connectors/audible/internal/bridge"
	"github.com/susamn/taletty/engine/model"
)

const maxLibraryBytes = 64 << 20

type libraryCandidate struct {
	books        []model.Book
	seen         map[model.BookID]bool
	pages, bytes int
}

func (c *libraryCandidate) page(frame bridge.Frame) error {
	fields, err := strictFields(frame.Raw)
	var stage string
	var items []json.RawMessage
	if err != nil || frame.Type != bridge.Stage || required(fields, "stage", &stage) != nil || stage != "library_page" || required(fields, "books", &items) != nil || len(items) == 0 || len(items) > 100 || c.pages >= 10000 || len(frame.Raw) > maxLibraryBytes-c.bytes {
		return bridge.ErrProtocol
	}
	if c.seen == nil {
		c.seen = make(map[model.BookID]bool)
	}
	for _, raw := range items {
		book, err := libraryBook(raw)
		if err != nil || c.seen[book.ID] {
			return bridge.ErrProtocol
		}
		c.seen[book.ID] = true
		c.books = append(c.books, book)
	}
	c.pages++
	c.bytes += len(frame.Raw)
	return nil
}

func (c *libraryCandidate) finish(frame bridge.Frame) error {
	fields, err := strictFields(frame.Raw)
	if err != nil {
		return err
	}
	if frame.Type == bridge.Failure {
		failure, err := object(fields["failure"])
		var code string
		if err != nil || required(failure, "code", &code) != nil {
			return bridge.ErrProtocol
		}
		if _, known := libraryFailures[code]; !known {
			return bridge.ErrProtocol
		}
		return libraryFailure(code, "")
	}
	var count int
	if frame.Type != bridge.Result || required(fields, "count", &count) != nil || count != len(c.books) {
		return bridge.ErrProtocol
	}
	return nil
}

func libraryBook(raw json.RawMessage) (model.Book, error) {
	var book model.Book
	fields, err := object(raw)
	if err != nil {
		return book, err
	}
	identity, err := object(fields["id"])
	if err != nil || required(identity, "provider", &book.ID.Provider) != nil || required(identity, "namespace", &book.ID.Namespace) != nil || required(identity, "id", &book.ID.ID) != nil || book.ID.Provider != "audible" || book.ID.Namespace != "us" || required(fields, "title", &book.Title) != nil {
		return book, bridge.ErrProtocol
	}
	if _, ok := fields["subtitle"]; ok {
		var subtitle string
		if required(fields, "subtitle", &subtitle) != nil {
			return book, bridge.ErrProtocol
		}
		book.Subtitle = &subtitle
	}
	for key, target := range map[string]*[]model.Contributor{"authors": &book.Authors, "narrators": &book.Narrators} {
		if _, ok := fields[key]; !ok {
			continue
		}
		var people []json.RawMessage
		if required(fields, key, &people) != nil {
			return book, bridge.ErrProtocol
		}
		*target = make([]model.Contributor, 0, len(people))
		for _, raw := range people {
			person, err := object(raw)
			var name string
			if err != nil || required(person, "name", &name) != nil {
				return book, bridge.ErrProtocol
			}
			*target = append(*target, model.Contributor{Name: name})
		}
	}
	if _, ok := fields["duration_ms"]; ok {
		var milliseconds int64
		if required(fields, "duration_ms", &milliseconds) != nil || milliseconds < 0 || milliseconds > int64((1<<63-1)/time.Millisecond) {
			return book, bridge.ErrProtocol
		}
		duration := time.Duration(milliseconds) * time.Millisecond
		book.Duration = &duration
	}
	if book.Validate() != nil {
		return book, bridge.ErrProtocol
	}
	return book, nil
}

// Exact key lookup avoids encoding/json's case-insensitive struct aliases. Null
// is not an optional known value: private DTOs omit unknown fields explicitly.
func required(fields map[string]json.RawMessage, key string, target any) error {
	raw, ok := fields[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, target) != nil {
		return bridge.ErrProtocol
	}
	return nil
}
func object(raw []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, bridge.ErrProtocol
	}
	return fields, nil
}

// Validate even ignored optional subtrees: ambiguity/lossy Unicode must not
// override known schema. Bounded depth keeps attacker-supplied nesting off-stack.
func strictFields(raw []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(raw) || !json.Valid(raw) || !validSurrogates(raw) {
		return nil, bridge.ErrProtocol
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if uniqueValue(decoder, 0) != nil {
		return nil, bridge.ErrProtocol
	}
	return object(raw)
}
func uniqueValue(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return bridge.ErrProtocol
	}
	token, err := decoder.Token()
	if err != nil {
		return bridge.ErrProtocol
	}
	switch token {
	case json.Delim('{'):
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return bridge.ErrProtocol
			}
			name := key.(string)
			if seen[name] {
				return bridge.ErrProtocol
			}
			seen[name] = true
			if uniqueValue(decoder, depth+1) != nil {
				return bridge.ErrProtocol
			}
		}
	case json.Delim('['):
		for decoder.More() {
			if uniqueValue(decoder, depth+1) != nil {
				return bridge.ErrProtocol
			}
		}
	default:
		return nil
	}
	_, err = decoder.Token()
	if err != nil {
		return bridge.ErrProtocol
	}
	return nil
}

// encoding/json replaces lone UTF-16 escapes with U+FFFD. Reject those rather
// than silently changing identity/metadata. json.Valid ran before this scanner.
func validSurrogates(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if raw[i] != 'u' {
			continue
		}
		value, _ := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		i += 4
		if value >= 0xdc00 && value <= 0xdfff {
			return false
		}
		if value < 0xd800 || value > 0xdbff {
			continue
		}
		if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return false
		}
		low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}
