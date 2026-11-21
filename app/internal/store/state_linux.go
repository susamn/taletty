package store

import (
	"context"
	"strings"

	"github.com/ncruces/go-sqlite3"
)

type Completion string

const (
	CompletionUnfinished Completion = "unfinished"
	CompletionFinished   Completion = "finished"
)

// ListeningState is the latest per-book playback checkpoint.
type ListeningState struct {
	Key         BookKey
	PositionNS  int64
	SpeedMilli  int64
	Completion  Completion
	UpdatedAtMS int64
}

// Validate checks ListeningState bounds before opening a transaction.
func (s ListeningState) Validate() error {
	if err := s.Key.Validate(); err != nil {
		return err
	}
	if s.PositionNS < 0 || s.SpeedMilli <= 0 || s.UpdatedAtMS < 0 {
		return ErrInvalidInput
	}
	if s.Completion != CompletionUnfinished && s.Completion != CompletionFinished {
		return ErrInvalidInput
	}
	return nil
}

// PutListeningState ensures book_identity exists and upserts the book's checkpoint.
func (s *Store) PutListeningState(ctx context.Context, state ListeningState) error {
	if err := state.Validate(); err != nil {
		return err
	}
	return s.withTx(ctx, true, func(c *sqlite3.Conn) error {
		bookKey, err := ensureBookIdentity(c, state.Key)
		if err != nil {
			return err
		}
		return execStmt(c,
			`INSERT INTO listening_state(book_key, position_ns, speed_millionths, completion, updated_at_ms)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(book_key) DO UPDATE SET
			     position_ns = excluded.position_ns,
			     speed_millionths = excluded.speed_millionths,
			     completion = excluded.completion,
			     updated_at_ms = excluded.updated_at_ms`,
			bookKey, state.PositionNS, state.SpeedMilli, string(state.Completion), state.UpdatedAtMS,
		)
	})
}

// ListeningState returns the stored checkpoint for key, if any.
func (s *Store) ListeningState(ctx context.Context, key BookKey) (ListeningState, bool, error) {
	if err := key.Validate(); err != nil {
		return ListeningState{}, false, err
	}
	var out ListeningState
	var found bool
	err := s.withTx(ctx, false, func(c *sqlite3.Conn) error {
		return queryStmt(c,
			`SELECT ls.position_ns, ls.speed_millionths, ls.completion, ls.updated_at_ms
			 FROM listening_state ls
			 JOIN book_identity bi ON bi.book_key = ls.book_key
			 WHERE bi.provider = ? AND bi.namespace = ? AND bi.id = ?`,
			func(stmt *sqlite3.Stmt) error {
				out = ListeningState{
					Key:         key,
					PositionNS:  stmt.ColumnInt64(0),
					SpeedMilli:  stmt.ColumnInt64(1),
					Completion:  Completion(stmt.ColumnText(2)),
					UpdatedAtMS: stmt.ColumnInt64(3),
				}
				found = true
				return nil
			},
			key.Provider, key.Namespace, key.ID,
		)
	})
	if err != nil {
		return ListeningState{}, false, err
	}
	return out, found, nil
}

type GlyphMode string

const (
	GlyphUnicode GlyphMode = "unicode"
	GlyphPlain   GlyphMode = "plain"
)

// Settings stores global user preferences persisted in the singleton settings row.
type Settings struct {
	GlyphMode     GlyphMode
	VolumePercent int
}

// Validate checks Settings constraints before opening a transaction.
func (s Settings) Validate() error {
	if s.GlyphMode != GlyphUnicode && s.GlyphMode != GlyphPlain {
		return ErrInvalidInput
	}
	if s.VolumePercent < 0 || s.VolumePercent > 100 {
		return ErrInvalidInput
	}
	return nil
}

// PutSettings persists global settings in the singleton row.
func (s *Store) PutSettings(ctx context.Context, settings Settings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	return s.withTx(ctx, true, func(c *sqlite3.Conn) error {
		return execStmt(c,
			`INSERT INTO settings(singleton, glyph_mode, volume_percent)
			 VALUES (1, ?, ?)
			 ON CONFLICT(singleton) DO UPDATE SET
			     glyph_mode = excluded.glyph_mode,
			     volume_percent = excluded.volume_percent`,
			string(settings.GlyphMode), settings.VolumePercent,
		)
	})
}

// Settings loads global settings or returns defaults when no row has been saved yet.
func (s *Store) Settings(ctx context.Context) (Settings, error) {
	out := Settings{GlyphMode: GlyphUnicode, VolumePercent: 100}
	err := s.withTx(ctx, false, func(c *sqlite3.Conn) error {
		return queryStmt(c,
			`SELECT glyph_mode, volume_percent FROM settings WHERE singleton = 1`,
			func(stmt *sqlite3.Stmt) error {
				out = Settings{
					GlyphMode:     GlyphMode(stmt.ColumnText(0)),
					VolumePercent: stmt.ColumnInt(1),
				}
				return nil
			},
		)
	})
	if err != nil {
		return Settings{}, err
	}
	return out, nil
}

type Destination string

const (
	DestinationAll        Destination = "all"
	DestinationContinue   Destination = "continue"
	DestinationDownloaded Destination = "downloaded"
	DestinationFinished   Destination = "finished"
	DestinationFavorites  Destination = "favorites"
	DestinationDownloads  Destination = "downloads"
	DestinationBookmarks  Destination = "bookmarks"
)

type FocusPane string

const (
	FocusLibrary  FocusPane = "library"
	FocusBooks    FocusPane = "books"
	FocusChapters FocusPane = "chapters"
	FocusDetails  FocusPane = "details"
	FocusPlayer   FocusPane = "player"
)

type SortOrder string

const (
	SortTitle       SortOrder = "title"
	SortAuthor      SortOrder = "author"
	SortDuration    SortOrder = "duration"
	SortRecent      SortOrder = "recent"
	SortAcquisition SortOrder = "acquisition"
)

// ViewState remembers UI navigation, selection, focus, filter and sort order.
type ViewState struct {
	Destination Destination
	Selected    *BookKey
	Focus       FocusPane
	FilterText  string
	SortOrder   SortOrder
}

// Validate checks ViewState enums, text safety and optional Selected BookKey.
func (v ViewState) Validate() error {
	switch v.Destination {
	case DestinationAll, DestinationContinue, DestinationDownloaded,
		DestinationFinished, DestinationFavorites, DestinationDownloads, DestinationBookmarks:
	default:
		return ErrInvalidInput
	}
	switch v.Focus {
	case FocusLibrary, FocusBooks, FocusChapters, FocusDetails, FocusPlayer:
	default:
		return ErrInvalidInput
	}
	switch v.SortOrder {
	case SortTitle, SortAuthor, SortDuration, SortRecent, SortAcquisition:
	default:
		return ErrInvalidInput
	}
	if !safeSingleLine(v.FilterText) {
		return ErrInvalidInput
	}
	if v.Selected != nil {
		if err := v.Selected.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// PutViewState persists UI navigation state and ensures Selected book_identity if set.
func (s *Store) PutViewState(ctx context.Context, state ViewState) error {
	if err := state.Validate(); err != nil {
		return err
	}
	return s.withTx(ctx, true, func(c *sqlite3.Conn) error {
		var selectedKey *int64
		if state.Selected != nil {
			pk, err := ensureBookIdentity(c, *state.Selected)
			if err != nil {
				return err
			}
			selectedKey = &pk
		}
		return execStmt(c,
			`INSERT INTO view_state(singleton, destination, selected_book_key, focus, filter_text, sort_order)
			 VALUES (1, ?, ?, ?, ?, ?)
			 ON CONFLICT(singleton) DO UPDATE SET
			     destination = excluded.destination,
			     selected_book_key = excluded.selected_book_key,
			     focus = excluded.focus,
			     filter_text = excluded.filter_text,
			     sort_order = excluded.sort_order`,
			string(state.Destination), selectedKey, string(state.Focus), state.FilterText, string(state.SortOrder),
		)
	})
}

// ViewState loads persisted UI state or returns defaults when no row has been saved yet.
func (s *Store) ViewState(ctx context.Context) (ViewState, error) {
	out := ViewState{Destination: DestinationAll, Focus: FocusBooks, SortOrder: SortTitle}
	err := s.withTx(ctx, false, func(c *sqlite3.Conn) error {
		return queryStmt(c,
			`SELECT vs.destination, vs.focus, vs.filter_text, vs.sort_order,
			        bi.provider, bi.namespace, bi.id
			 FROM view_state vs
			 LEFT JOIN book_identity bi ON bi.book_key = vs.selected_book_key
			 WHERE vs.singleton = 1`,
			func(stmt *sqlite3.Stmt) error {
				out = ViewState{
					Destination: Destination(stmt.ColumnText(0)),
					Focus:       FocusPane(stmt.ColumnText(1)),
					FilterText:  stmt.ColumnText(2),
					SortOrder:   SortOrder(stmt.ColumnText(3)),
				}
				if stmt.ColumnType(4) != sqlite3.NULL {
					out.Selected = &BookKey{
						Provider:  stmt.ColumnText(4),
						Namespace: stmt.ColumnText(5),
						ID:        stmt.ColumnText(6),
					}
				}
				return nil
			},
		)
	})
	if err != nil {
		return ViewState{}, err
	}
	return out, nil
}

// SetFavorite marks or unmarks key as a favorite while keeping book_identity intact.
func (s *Store) SetFavorite(ctx context.Context, key BookKey, favored bool, createdAtMS int64) error {
	if err := key.Validate(); err != nil {
		return err
	}
	if createdAtMS < 0 {
		return ErrInvalidInput
	}
	return s.withTx(ctx, true, func(c *sqlite3.Conn) error {
		if !favored {
			return execStmt(c,
				`DELETE FROM favorite
				 WHERE book_key = (
				     SELECT book_key FROM book_identity WHERE provider = ? AND namespace = ? AND id = ?
				 )`,
				key.Provider, key.Namespace, key.ID,
			)
		}
		bookKey, err := ensureBookIdentity(c, key)
		if err != nil {
			return err
		}
		return execStmt(c,
			`INSERT INTO favorite(book_key, created_at_ms)
			 VALUES (?, ?)
			 ON CONFLICT(book_key) DO UPDATE SET created_at_ms = excluded.created_at_ms`,
			bookKey, createdAtMS,
		)
	})
}

// Favorites returns all favorited books in creation order.
func (s *Store) Favorites(ctx context.Context) ([]BookKey, error) {
	var keys []BookKey
	err := s.withTx(ctx, false, func(c *sqlite3.Conn) error {
		return queryStmt(c,
			`SELECT bi.provider, bi.namespace, bi.id
			 FROM favorite f
			 JOIN book_identity bi ON bi.book_key = f.book_key
			 ORDER BY f.created_at_ms ASC, bi.provider ASC, bi.namespace ASC, bi.id ASC`,
			func(stmt *sqlite3.Stmt) error {
				keys = append(keys, BookKey{
					Provider:  stmt.ColumnText(0),
					Namespace: stmt.ColumnText(1),
					ID:        stmt.ColumnText(2),
				})
				return nil
			},
		)
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// Bookmark is a frozen jump position and user note attached to a BookKey.
type Bookmark struct {
	ID           string
	Key          BookKey
	PositionNS   int64
	Note         string
	ChapterID    *string
	ChapterTitle *string
	CreatedAtMS  int64
	UpdatedAtMS  int64
}

// Validate checks Bookmark fields, multiline note safety and timestamp bounds.
func (b Bookmark) Validate() error {
	if !safeID(b.ID) {
		return ErrInvalidInput
	}
	if err := b.Key.Validate(); err != nil {
		return err
	}
	if b.PositionNS < 0 || b.CreatedAtMS < 0 || b.UpdatedAtMS < b.CreatedAtMS {
		return ErrInvalidInput
	}
	if strings.TrimSpace(b.Note) == "" || !safeMultiline(b.Note) {
		return ErrInvalidInput
	}
	if b.ChapterID != nil && !safeSingleLine(*b.ChapterID) {
		return ErrInvalidInput
	}
	if b.ChapterTitle != nil && !safeSingleLine(*b.ChapterTitle) {
		return ErrInvalidInput
	}
	return nil
}

// PutBookmark creates or updates a bookmark without reassigning an existing ID across books.
func (s *Store) PutBookmark(ctx context.Context, b Bookmark) error {
	if err := b.Validate(); err != nil {
		return err
	}
	return s.withTx(ctx, true, func(c *sqlite3.Conn) error {
		bookKey, err := ensureBookIdentity(c, b.Key)
		if err != nil {
			return err
		}
		var ownerKey int64
		var exists bool
		if err := queryStmt(c,
			`SELECT book_key FROM bookmark WHERE bookmark_id = ?`,
			func(stmt *sqlite3.Stmt) error {
				ownerKey = stmt.ColumnInt64(0)
				exists = true
				return nil
			},
			b.ID,
		); err != nil {
			return err
		}
		if exists && ownerKey != bookKey {
			return ErrInvalidInput
		}
		return execStmt(c,
			`INSERT INTO bookmark(bookmark_id, book_key, position_ns, note, chapter_id, chapter_title, created_at_ms, updated_at_ms)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(bookmark_id) DO UPDATE SET
			     position_ns = excluded.position_ns,
			     note = excluded.note,
			     chapter_id = excluded.chapter_id,
			     chapter_title = excluded.chapter_title,
			     created_at_ms = excluded.created_at_ms,
			     updated_at_ms = excluded.updated_at_ms`,
			b.ID, bookKey, b.PositionNS, b.Note, b.ChapterID, b.ChapterTitle, b.CreatedAtMS, b.UpdatedAtMS,
		)
	})
}

// DeleteBookmark removes bookmarkID for key without deleting book_identity.
func (s *Store) DeleteBookmark(ctx context.Context, key BookKey, bookmarkID string) error {
	if err := key.Validate(); err != nil {
		return err
	}
	if !safeID(bookmarkID) {
		return ErrInvalidInput
	}
	return s.withTx(ctx, true, func(c *sqlite3.Conn) error {
		return execStmt(c,
			`DELETE FROM bookmark
			 WHERE bookmark_id = ?
			   AND book_key = (
			       SELECT book_key FROM book_identity WHERE provider = ? AND namespace = ? AND id = ?
			   )`,
			bookmarkID, key.Provider, key.Namespace, key.ID,
		)
	})
}

// Bookmarks returns all bookmarks for key ordered by position_ns, then bookmark_id.
func (s *Store) Bookmarks(ctx context.Context, key BookKey) ([]Bookmark, error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	var out []Bookmark
	err := s.withTx(ctx, false, func(c *sqlite3.Conn) error {
		return queryStmt(c,
			`SELECT bm.bookmark_id, bm.position_ns, bm.note, bm.chapter_id, bm.chapter_title, bm.created_at_ms, bm.updated_at_ms
			 FROM bookmark bm
			 JOIN book_identity bi ON bi.book_key = bm.book_key
			 WHERE bi.provider = ? AND bi.namespace = ? AND bi.id = ?
			 ORDER BY bm.position_ns ASC, bm.bookmark_id ASC`,
			func(stmt *sqlite3.Stmt) error {
				out = append(out, Bookmark{
					ID:           stmt.ColumnText(0),
					Key:          key,
					PositionNS:   stmt.ColumnInt64(1),
					Note:         stmt.ColumnText(2),
					ChapterID:    columnOptionalText(stmt, 3),
					ChapterTitle: columnOptionalText(stmt, 4),
					CreatedAtMS:  stmt.ColumnInt64(5),
					UpdatedAtMS:  stmt.ColumnInt64(6),
				})
				return nil
			},
			key.Provider, key.Namespace, key.ID,
		)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
