package audible

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strconv"

	"github.com/susamn/taletty/engine/connectors/audible/internal/bridge"
	"github.com/susamn/taletty/engine/model"
)

//go:embed internal/bridge/adapter/library.py
var libraryPageSource string

//go:embed internal/bridge/adapter/noninteractive.py
var noninteractiveSource string

var operationPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func libraryScript() string {
	// Only embedded strings: Marshal cannot fail. No credentials/paths in the script.
	sources, _ := json.Marshal(map[string]string{"doctor": doctorSource, "auth_store": authStoreSource, "login": browserLoginSource, "sdk_session": sdkSessionSource, "library": libraryPageSource})
	return "import json\nSOURCES=json.loads(" + strconv.Quote(string(sources)) + ")\n" + interactiveSource + "\n" + noninteractiveSource + "\nmain(SOURCES,load_sources,null_stdio)\n"
}

// Library returns a complete snapshot or its zero value plus a safe *model.Failure.
// Invalid/duplicate live IDs are rejected without events/child work. IDs are scoped
// to this Client until return (including queued work and terminal notification).
// Accepted operations emit Started and exactly one terminal; Stage counts are only
// provisional, never cache updates. Events do not share snapshot books/pointers.
//
// report runs synchronously on the caller goroutine, in order: return promptly,
// never reenter the client, use ctx for cancellation. Arbitrary blocked callbacks
// cannot be interrupted; a panic propagates only after owned process users join.
// Library owns no callback goroutine. Cancellation wins until validated completion,
// not retroactively if requested from the Completed callback. No automatic retry.
func (c *Client) Library(ctx context.Context, id string, report func(model.OperationEvent)) (model.LibrarySnapshot, error) {
	if !operationPattern.MatchString(id) {
		return model.LibrarySnapshot{}, libraryFailure("invalid_input", "")
	}
	c.operationsMu.Lock()
	if c.operations[id] {
		c.operationsMu.Unlock()
		return model.LibrarySnapshot{}, libraryFailure("busy", id)
	}
	if c.operations == nil {
		c.operations = make(map[string]bool)
	}
	c.operations[id] = true
	c.operationsMu.Unlock()
	defer func() { c.operationsMu.Lock(); delete(c.operations, id); c.operationsMu.Unlock() }()
	var sequence uint64
	emit := func(kind model.EventType, stage string, count int, failure *model.Failure) {
		sequence++
		if report != nil {
			event := model.OperationEvent{OperationID: id, Sequence: sequence, Type: kind, Stage: stage, Count: count}
			if failure != nil {
				copy := *failure
				event.Failure = &copy
			}
			report(event)
		}
	}
	emit(model.Started, "", 0, nil)
	candidate := libraryCandidate{books: make([]model.Book, 0)}
	err := c.session.WithRoot(ctx, func(ctx context.Context, root *os.File) error {
		return c.readLibrary(ctx, root, id, &candidate, func() { emit(model.Stage, "library_page", len(candidate.books), nil) })
	})
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		err = libraryError(err, id)
		var failure *model.Failure
		errors.As(err, &failure)
		kind := model.Failed
		if failure.Code == "canceled" {
			kind = model.Canceled
		}
		emit(kind, "", 0, failure)
		return model.LibrarySnapshot{}, err
	}
	emit(model.Completed, "", len(candidate.books), nil)
	return model.LibrarySnapshot{OperationID: id, Books: candidate.books}, nil
}

func (c *Client) readLibrary(ctx context.Context, root *os.File, id string, candidate *libraryCandidate, page func()) error {
	work, cancel := context.WithCancel(ctx)
	frames := make(chan bridge.Frame, 1)
	joined := make(chan struct{})
	var terminal bridge.Frame
	var runErr error
	go func() {
		defer close(joined)
		request := []byte(`{"protocol":1,"operation_id":"` + id + `","action":"library"}`)
		terminal, runErr = bridge.RunRoot(work, c.python, []string{"-I", "-B", "-c", c.librarySource}, request, id, root, frames)
		close(frames) // RunRoot has joined every producer before this close.
	}()
	defer func() { cancel(); <-joined }() // Also cancel/join on payload error or reporter panic.
	for frame := range frames {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := candidate.page(frame); err != nil {
			return err
		}
		page()
	}
	<-joined
	if runErr != nil {
		return runErr
	}
	return candidate.finish(terminal)
}

func libraryError(err error, id string) error {
	code := "protocol_error"
	var failure *model.Failure
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = "canceled"
	case errors.Is(err, ErrClosed):
		code = "internal"
	case errors.Is(err, bridge.ErrStart):
		code = "runtime_missing"
	case errors.As(err, &failure):
		code = failure.Code
	}
	safe := libraryFailure(code, id)
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(safe, context.DeadlineExceeded)
	}
	if code == "canceled" {
		return errors.Join(safe, context.Canceled)
	}
	if errors.Is(err, ErrClosed) {
		return errors.Join(safe, ErrClosed)
	}
	return safe
}
