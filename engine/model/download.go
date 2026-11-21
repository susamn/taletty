package model

import (
	"errors"
	"path/filepath"
	"strings"
	"time"
)

var ErrInvalidMedia = errors.New("invalid media metadata")

const (
	MaxChapterDepth = 32
	MaxChapters     = 10000
)

// Chapter is a book-local hierarchy, not a playback position. Start/End are
// absolute native durations; nil End is unknown, while a pointer to zero is known.
// IDs are stable only within an unchanged provider tree, not across revisions.
type Chapter struct {
	ID       string         `json:"id"`
	Title    string         `json:"title"`
	Start    time.Duration  `json:"start_ns"`
	End      *time.Duration `json:"end_ns,omitempty"`
	Children []Chapter      `json:"children,omitzero"`
}

// ValidateChapters bounds the entire tree and rejects ambiguous book-local IDs.
// It does not clip overlaps, sort children or invent parent durations.
func ValidateChapters(chapters []Chapter) error {
	count := 0
	seen := make(map[string]bool)
	var visit func([]Chapter, int) error
	visit = func(nodes []Chapter, depth int) error {
		if len(nodes) != 0 && depth > MaxChapterDepth {
			return ErrInvalidMedia
		}
		for _, node := range nodes {
			count++
			if count > MaxChapters || len(node.ID) > 256 || !identifierPattern.MatchString(node.ID) || seen[node.ID] ||
				strings.TrimSpace(node.Title) == "" || len(node.Title) > 4096 || !safeText(node.Title) || node.Start < 0 ||
				(node.End != nil && *node.End < node.Start) {
				return ErrInvalidMedia
			}
			seen[node.ID] = true
			if err := visit(node.Children, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(chapters, 1)
}

// Media describes validated complete local audio, never a partial or content URL.
// Nil Chapters explicitly means unknown provider hierarchy (including disk reuse
// and restart). A known empty hierarchy remains []. No flattened IDs are invented.
type Media struct {
	BookID   BookID    `json:"book_id"`
	Path     string    `json:"path"`
	Format   string    `json:"format"`
	Chapters []Chapter `json:"chapters,omitzero"`
}

func (m Media) Validate() error {
	if m.BookID.Validate() != nil || !safeText(m.Path) || strings.Contains(m.Path, "\\") ||
		!filepath.IsAbs(m.Path) || filepath.Clean(m.Path) != m.Path || m.Format != "m4b" || filepath.Ext(m.Path) != ".m4b" {
		return ErrInvalidMedia
	}
	return ValidateChapters(m.Chapters)
}

type MediaState string

const (
	MediaUnknown MediaState = "unknown"
	MediaReady   MediaState = "ready"
	MediaAbsent  MediaState = "absent"
	MediaInvalid MediaState = "invalid"
	MediaUnsafe  MediaState = "unsafe"
)

type Persistence string

const (
	PersistenceUnknown   Persistence = "unknown"
	PersistenceConfirmed Persistence = "confirmed"
	PersistenceFailed    Persistence = "failed"
	PersistenceUncertain Persistence = "uncertain"
)

// DownloadResult remains authoritative even with a non-nil error. Ready media is
// independent of authentication persistence and this attempt's commit/durability.
// Published=nil is unknown, not false; reconciliation cannot prove a lost commit.
// Reused is true only when the clean producer establishes reuse, not on recovery.
// Consumers must inspect both result and error and must not blindly redownload.
type DownloadResult struct {
	OperationID     string      `json:"operation_id"`
	BookID          BookID      `json:"book_id"`
	State           MediaState  `json:"state"`
	Media           *Media      `json:"media,omitempty"`
	Reused          bool        `json:"reused"`
	Published       *bool       `json:"published"`
	MediaDurability Persistence `json:"media_durability"`
	AuthPersistence Persistence `json:"auth_persistence"`
	MediaFailure    *Failure    `json:"media_failure,omitempty"`
	AuthFailure     *Failure    `json:"auth_failure,omitempty"`
}

// Clone gives a notification consumer independent ownership of mutable facts.
func (r DownloadResult) Clone() DownloadResult {
	if r.Published != nil {
		value := *r.Published
		r.Published = &value
	}
	if r.MediaFailure != nil {
		value := *r.MediaFailure
		r.MediaFailure = &value
	}
	if r.AuthFailure != nil {
		value := *r.AuthFailure
		r.AuthFailure = &value
	}
	if r.Media != nil {
		value := *r.Media
		value.Chapters = cloneChapters(value.Chapters)
		r.Media = &value
	}
	return r
}
func cloneChapters(nodes []Chapter) []Chapter {
	if nodes == nil {
		return nil
	}
	result := make([]Chapter, len(nodes))
	for i, node := range nodes {
		result[i] = node
		if node.End != nil {
			value := *node.End
			result[i].End = &value
		}
		result[i].Children = cloneChapters(node.Children)
	}
	return result
}
