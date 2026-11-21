package store

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ncruces/go-sqlite3"
)

var (
	ErrInvalidInput       = errors.New("invalid store input")
	ErrActiveJobExists    = errors.New("live download job already exists for book")
	ErrDownloadInProgress = errors.New("another download job is already active")
	ErrQueueOrderConflict = errors.New("download queue order is already assigned")
	ErrMediaPathConflict  = errors.New("local media path is already assigned to another book")
)

var (
	scopePattern      = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	asinPattern       = regexp.MustCompile(`^[A-Za-z0-9]{10}$`)
)

// BookKey is a comparable provider-neutral book identity. Fields are case-sensitive
// and never normalized by the store.
type BookKey struct {
	Provider  string
	Namespace string
	ID        string
}

func validateScope(provider, namespace string) error {
	if !scopePattern.MatchString(provider) || !scopePattern.MatchString(namespace) {
		return ErrInvalidInput
	}
	return nil
}

// Validate checks BookKey syntax against 001_initial.sql and model.BookID rules.
func (k BookKey) Validate() error {
	if err := validateScope(k.Provider, k.Namespace); err != nil {
		return err
	}
	if !identifierPattern.MatchString(k.ID) {
		return ErrInvalidInput
	}
	if k.Provider == "audible" && !asinPattern.MatchString(k.ID) {
		return ErrInvalidInput
	}
	return nil
}

// Contributor is ordered display metadata stored as JSON objects in catalog_book.
type Contributor struct {
	Name string `json:"name"`
}

// CatalogBook is current cached provider metadata for one BookKey.
// Nil Subtitle, Authors, Narrators or DurationNS represent SQL NULL (unknown).
// Non-nil empty slices/strings or zero duration represent known empty/zero values.
type CatalogBook struct {
	Key        BookKey
	Title      string
	Subtitle   *string
	Authors    []Contributor
	Narrators  []Contributor
	DurationNS *int64
}

// Validate checks metadata syntax and bounds before opening a transaction.
func (b CatalogBook) Validate() error {
	if err := b.Key.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(b.Title) == "" || !safeSingleLine(b.Title) {
		return ErrInvalidInput
	}
	if b.Subtitle != nil && !safeSingleLine(*b.Subtitle) {
		return ErrInvalidInput
	}
	if b.DurationNS != nil && *b.DurationNS < 0 {
		return ErrInvalidInput
	}
	for _, group := range [][]Contributor{b.Authors, b.Narrators} {
		for _, person := range group {
			if strings.TrimSpace(person.Name) == "" || !safeSingleLine(person.Name) {
				return ErrInvalidInput
			}
		}
	}
	return nil
}

// ReplaceCatalog atomically replaces cached catalog metadata for (provider, namespace)
// while preserving book_identity keys and all independent local state.
func (s *Store) ReplaceCatalog(ctx context.Context, provider, namespace string, books []CatalogBook, refreshedAtMS int64) error {
	if err := validateScope(provider, namespace); err != nil {
		return err
	}
	if refreshedAtMS < 0 {
		return ErrInvalidInput
	}
	type preparedBook struct {
		book          CatalogBook
		authorsJSON   *string
		narratorsJSON *string
	}
	seen := make(map[BookKey]struct{}, len(books))
	prepared := make([]preparedBook, len(books))
	for i, book := range books {
		if err := book.Validate(); err != nil {
			return err
		}
		if book.Key.Provider != provider || book.Key.Namespace != namespace {
			return ErrInvalidInput
		}
		if _, duplicate := seen[book.Key]; duplicate {
			return ErrInvalidInput
		}
		seen[book.Key] = struct{}{}
		prepared[i] = preparedBook{
			book:          book,
			authorsJSON:   encodeContributors(book.Authors),
			narratorsJSON: encodeContributors(book.Narrators),
		}
	}

	return s.withTx(ctx, true, func(c *sqlite3.Conn) error {
		if err := execStmt(c,
			`DELETE FROM catalog_book
			 WHERE book_key IN (
			     SELECT book_key FROM book_identity WHERE provider = ? AND namespace = ?
			 )`,
			provider, namespace,
		); err != nil {
			return err
		}
		for _, item := range prepared {
			bookKey, err := ensureBookIdentity(c, item.book.Key)
			if err != nil {
				return err
			}
			if err := execStmt(c,
				`INSERT INTO catalog_book(book_key, title, subtitle, authors_json, narrators_json, duration_ns)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				bookKey, item.book.Title, item.book.Subtitle, item.authorsJSON, item.narratorsJSON, item.book.DurationNS,
			); err != nil {
				return err
			}
		}
		return execStmt(c,
			`INSERT INTO library_snapshot(provider, namespace, refreshed_at_ms)
			 VALUES (?, ?, ?)
			 ON CONFLICT(provider, namespace) DO UPDATE SET refreshed_at_ms = excluded.refreshed_at_ms`,
			provider, namespace, refreshedAtMS,
		)
	})
}

// Catalog returns cached books for (provider, namespace), or all scopes when both are empty.
func (s *Store) Catalog(ctx context.Context, provider, namespace string) ([]CatalogBook, error) {
	scoped := provider != "" || namespace != ""
	if scoped {
		if err := validateScope(provider, namespace); err != nil {
			return nil, err
		}
	}
	var books []CatalogBook
	err := s.withTx(ctx, false, func(c *sqlite3.Conn) error {
		query := `SELECT bi.provider, bi.namespace, bi.id, cb.title, cb.subtitle, cb.authors_json, cb.narrators_json, cb.duration_ns
		          FROM catalog_book cb
		          JOIN book_identity bi ON bi.book_key = cb.book_key`
		var args []any
		if scoped {
			query += ` WHERE bi.provider = ? AND bi.namespace = ? ORDER BY bi.id ASC`
			args = []any{provider, namespace}
		} else {
			query += ` ORDER BY bi.provider ASC, bi.namespace ASC, bi.id ASC`
		}
		return queryStmt(c, query, func(stmt *sqlite3.Stmt) error {
			authors, err := decodeContributors(columnOptionalText(stmt, 5))
			if err != nil {
				return err
			}
			narrators, err := decodeContributors(columnOptionalText(stmt, 6))
			if err != nil {
				return err
			}
			books = append(books, CatalogBook{
				Key: BookKey{
					Provider:  stmt.ColumnText(0),
					Namespace: stmt.ColumnText(1),
					ID:        stmt.ColumnText(2),
				},
				Title:      stmt.ColumnText(3),
				Subtitle:   columnOptionalText(stmt, 4),
				Authors:    authors,
				Narrators:  narrators,
				DurationNS: columnOptionalInt64(stmt, 7),
			})
			return nil
		}, args...)
	})
	if err != nil {
		return nil, err
	}
	return books, nil
}

// SnapshotRefreshedAt returns the last successful refresh timestamp for (provider, namespace).
func (s *Store) SnapshotRefreshedAt(ctx context.Context, provider, namespace string) (int64, bool, error) {
	if err := validateScope(provider, namespace); err != nil {
		return 0, false, err
	}
	var refreshedAt int64
	var found bool
	err := s.withTx(ctx, false, func(c *sqlite3.Conn) error {
		return queryStmt(c,
			`SELECT refreshed_at_ms FROM library_snapshot WHERE provider = ? AND namespace = ?`,
			func(stmt *sqlite3.Stmt) error {
				refreshedAt = stmt.ColumnInt64(0)
				found = true
				return nil
			},
			provider, namespace,
		)
	})
	if err != nil {
		return 0, false, err
	}
	return refreshedAt, found, nil
}

func (s *Store) withTx(ctx context.Context, immediate bool, work func(*sqlite3.Conn) error) error {
	if s == nil {
		return ErrClosed
	}
	var domainErr error
	err := s.withConn(ctx, func(c *sqlite3.Conn) error {
		begin := "BEGIN"
		if immediate {
			begin = "BEGIN IMMEDIATE"
		}
		if e := c.Exec(begin); e != nil {
			return e
		}
		if e := work(c); e != nil {
			domainErr = e
			c.SetInterrupt(context.Background())
			_ = c.Exec("ROLLBACK")
			return e
		}
		if e := c.Exec("COMMIT"); e != nil {
			c.SetInterrupt(context.Background())
			_ = c.Exec("ROLLBACK")
			return e
		}
		return nil
	})
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.ctx.Err() != nil {
		return err
	}
	if isDomainError(domainErr) {
		return domainErr
	}
	return err
}

func isDomainError(err error) bool {
	return errors.Is(err, ErrInvalidInput) ||
		errors.Is(err, ErrActiveJobExists) ||
		errors.Is(err, ErrDownloadInProgress) ||
		errors.Is(err, ErrQueueOrderConflict) ||
		errors.Is(err, ErrMediaPathConflict)
}

func ensureBookIdentity(c *sqlite3.Conn, key BookKey) (int64, error) {
	if err := execStmt(c,
		`INSERT INTO book_identity(provider, namespace, id) VALUES (?, ?, ?)
		 ON CONFLICT(provider, namespace, id) DO NOTHING`,
		key.Provider, key.Namespace, key.ID,
	); err != nil {
		return 0, err
	}
	return lookupBookKey(c, key)
}

func lookupBookKey(c *sqlite3.Conn, key BookKey) (int64, error) {
	var bookKey int64
	var found bool
	err := queryStmt(c,
		`SELECT book_key FROM book_identity WHERE provider = ? AND namespace = ? AND id = ?`,
		func(stmt *sqlite3.Stmt) error {
			bookKey = stmt.ColumnInt64(0)
			found = true
			return nil
		},
		key.Provider, key.Namespace, key.ID,
	)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, ErrDatabase
	}
	return bookKey, nil
}

func execStmt(c *sqlite3.Conn, query string, args ...any) error {
	stmt, _, err := c.Prepare(query)
	if err != nil {
		return err
	}
	defer stmt.Close()
	if err := bindParams(stmt, args...); err != nil {
		return err
	}
	return stmt.Exec()
}

func queryStmt(c *sqlite3.Conn, query string, row func(*sqlite3.Stmt) error, args ...any) error {
	stmt, _, err := c.Prepare(query)
	if err != nil {
		return err
	}
	defer stmt.Close()
	if err := bindParams(stmt, args...); err != nil {
		return err
	}
	for stmt.Step() {
		if err := row(stmt); err != nil {
			return err
		}
	}
	return stmt.Err()
}

func bindParams(stmt *sqlite3.Stmt, args ...any) error {
	for i, arg := range args {
		idx := i + 1
		var err error
		switch v := arg.(type) {
		case string:
			err = stmt.BindText(idx, v)
		case *string:
			if v == nil {
				err = stmt.BindNull(idx)
			} else {
				err = stmt.BindText(idx, *v)
			}
		case int64:
			err = stmt.BindInt64(idx, v)
		case *int64:
			if v == nil {
				err = stmt.BindNull(idx)
			} else {
				err = stmt.BindInt64(idx, *v)
			}
		case int:
			err = stmt.BindInt(idx, v)
		default:
			return ErrInvalidInput
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func columnOptionalText(stmt *sqlite3.Stmt, col int) *string {
	if stmt.ColumnType(col) == sqlite3.NULL {
		return nil
	}
	v := stmt.ColumnText(col)
	return &v
}

func columnOptionalInt64(stmt *sqlite3.Stmt, col int) *int64 {
	if stmt.ColumnType(col) == sqlite3.NULL {
		return nil
	}
	v := stmt.ColumnInt64(col)
	return &v
}

func encodeContributors(people []Contributor) *string {
	if people == nil {
		return nil
	}
	raw, _ := json.Marshal(people)
	s := string(raw)
	return &s
}

func decodeContributors(raw *string) ([]Contributor, error) {
	if raw == nil {
		return nil, nil
	}
	var people []Contributor
	if err := json.Unmarshal([]byte(*raw), &people); err != nil {
		return nil, ErrInvalidInput
	}
	if people == nil {
		people = []Contributor{}
	}
	return people, nil
}

func safeSingleLine(text string) bool {
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

func safeMultiline(text string) bool {
	if !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		if unicode.Is(unicode.Cf, r) {
			return false
		}
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return false
		}
	}
	return true
}

func safeID(id string) bool {
	return id != "" && strings.TrimSpace(id) == id && safeSingleLine(id)
}
