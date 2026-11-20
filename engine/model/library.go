package model

import (
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var ErrInvalidBook = errors.New("invalid book metadata")

// Contributor is display metadata, not a provider/customer identity.
type Contributor struct {
	Name string `json:"name"`
}

// Book contains provider metadata only: no playback/history, auth or content URLs.
// Nil means unknown. Non-nil empty contributors/zero duration are known values.
// Public JSON uses native duration_ns, NOT the private bridge's duration_ms.
// Validate at trust boundaries before storing, displaying or serializing a Book.
type Book struct {
	ID        BookID         `json:"id"`
	Title     string         `json:"title"`
	Subtitle  *string        `json:"subtitle,omitempty"`
	Authors   []Contributor  `json:"authors,omitzero"`
	Narrators []Contributor  `json:"narrators,omitzero"`
	Duration  *time.Duration `json:"duration_ns,omitempty"`
}

// Validate rejects malformed metadata without echoing it or silently changing
// identities/text. Provider adapters sanitize first; this is defense in depth.
// Catalog syntax is not marketplace support, entitlement or snapshot completion.
func (b Book) Validate() error {
	if b.ID.Validate() != nil || strings.TrimSpace(b.Title) == "" || !safeText(b.Title) {
		return ErrInvalidBook
	}
	if b.Subtitle != nil && !safeText(*b.Subtitle) {
		return ErrInvalidBook
	}
	if b.Duration != nil && *b.Duration < 0 {
		return ErrInvalidBook
	}
	for _, people := range [][]Contributor{b.Authors, b.Narrators} {
		for _, person := range people {
			if strings.TrimSpace(person.Name) == "" || !safeText(person.Name) {
				return ErrInvalidBook
			}
		}
	}
	return nil
}

func safeText(text string) bool {
	if !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}
