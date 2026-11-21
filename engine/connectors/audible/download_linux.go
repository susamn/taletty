package audible

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/susamn/taletty/engine/connectors/audible/internal/bridge"
	"github.com/susamn/taletty/engine/model"
)

const reconciliationTimeout = 8 * time.Second

// Download returns independent media/auth facts even on error; inspect BOTH.
// It never retries or deletes final media. IDs share Library's live namespace.
// Accepted calls emit Started, ordered Stage/Progress notifications (skipping
// stages on verified ready reuse), and exactly one terminal with an owned
// result copy, unless the caller reporter panics. The authoritative return must
// not be applied twice with its terminal notification.
//
// Reporter ownership matches Library: synchronous caller code, prompt/no reentry,
// nil allowed. Panic propagates after owned users join. Close waits root/process
// work (including recovery), not caller-owned started/terminal callback code.
// Join operation callers before shutting down. Ready media wins cancellation,
// never auth persistence or media durability errors. Unknown is not absent/durable.
func (c *Client) Download(ctx context.Context, id string, book model.BookID, report func(model.OperationEvent)) (model.DownloadResult, error) {
	if !operationPattern.MatchString(id) || book.Validate() != nil || book.Provider != "audible" || book.Namespace != "us" {
		return model.DownloadResult{}, downloadFailure("invalid_input", "")
	}
	if !c.reserveOperation(id) {
		return model.DownloadResult{}, downloadFailure("busy", id)
	}
	defer c.releaseOperation(id)
	result := model.DownloadResult{OperationID: id, BookID: book, State: model.MediaUnknown, MediaDurability: model.PersistenceUnknown, AuthPersistence: model.PersistenceUnknown}
	var sequence uint64
	emit := func(event model.OperationEvent) {
		sequence++
		if report == nil {
			return
		}
		event.OperationID = id
		event.Sequence = sequence
		if event.Bytes != nil {
			value := *event.Bytes
			event.Bytes = &value
		}
		if event.Total != nil {
			value := *event.Total
			event.Total = &value
		}
		if event.Failure != nil {
			value := *event.Failure
			event.Failure = &value
		}
		if event.Download != nil {
			value := event.Download.Clone()
			event.Download = &value
		}
		report(event)
	}
	emit(model.OperationEvent{Type: model.Started})
	err := c.session.WithRoot(ctx, func(work context.Context, root *os.File) error {
		var tracker downloadTracker
		frame, runErr := c.downloadChild(work, root, id, book, "download", c.downloadSource, func(frame bridge.Frame) error {
			event, err := tracker.next(frame)
			if err != nil {
				return err
			}
			emit(event)
			return nil
		})
		var out downloadOutcome
		if runErr == nil {
			out, runErr = downloadTerminal(frame)
		}
		if runErr == nil {
			result.Reused, result.Published = out.reused, out.published
			if out.mediaCode != nil {
				result.MediaFailure = downloadFailure(*out.mediaCode, id)
			}
			if out.authCode != nil {
				result.AuthFailure = libraryFailure(*out.authCode, id)
				result.AuthPersistence = model.PersistenceFailed
				if *out.authCode == "storage_uncertain" {
					result.AuthPersistence = model.PersistenceUncertain
				}
			}
			if out.ready {
				result.State = model.MediaReady
				result.Media = &model.Media{BookID: book, Format: "m4b", Chapters: out.chapters}
			}
			if out.published != nil && *out.published && out.mediaCode == nil {
				result.MediaDurability = model.PersistenceConfirmed
				if out.authCode == nil {
					result.AuthPersistence = model.PersistenceConfirmed
				}
			}
			if out.mediaCode != nil && *out.mediaCode == "storage_uncertain" {
				result.MediaDurability = model.PersistenceUncertain
			}
			if out.ready && result.MediaFailure != nil && result.MediaFailure.Code == "canceled" {
				result.MediaFailure = nil // validated publication beats media cancellation
			}
			if result.AuthFailure != nil {
				runErr = result.AuthFailure
			} else if result.MediaFailure != nil {
				runErr = result.MediaFailure
			}
		}
		// downloadChild has joined ALL original users, including on cancellation or
		// schema failure. Retain THIS loan/gate; never call WithRoot recursively or
		// reuse the canceled operation context. Reconciliation is provider-free.
		if runErr != nil && (result.State != model.MediaReady || errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded)) {
			recovery, cancel := context.WithTimeout(context.Background(), reconciliationTimeout)
			recovered, recoveryErr := c.downloadChild(recovery, root, id, book, "reconcile", c.reconcileSource, nil)
			cancel() // RunRoot has already joined; no recovery users outlive this loan.
			if recoveryErr == nil {
				state, decodeErr := reconciliationTerminal(recovered)
				if decodeErr == nil {
					result.State = state
				}
			}
			if result.State == model.MediaReady {
				result.Media = &model.Media{BookID: book, Format: "m4b"} // hierarchy unknown after recovery
				if result.MediaFailure != nil && result.MediaFailure.Code == "canceled" {
					result.MediaFailure = nil
				}
				var failure *model.Failure
				if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) || (errors.As(runErr, &failure) && failure.Code == "canceled") {
					result.MediaFailure = nil
					runErr = result.AuthFailure
					// An interface containing a nil *Failure is not a nil error.
					if result.AuthFailure == nil {
						runErr = nil
					}
				}
			}
		}
		if result.Media != nil {
			// Derive the path from the borrowed inode, never provider output or a stale
			// Open pathname. Renamed roots remain anchored; unlinked roots fail closed.
			path, pathErr := os.Readlink("/proc/self/fd/" + strconv.FormatUint(uint64(root.Fd()), 10))
			result.Media.Path = filepath.Join(path, "downloads", book.ID+".m4b")
			if pathErr != nil || strings.HasSuffix(path, " (deleted)") || result.Media.Validate() != nil {
				result.Media = nil
				result.State = model.MediaUnknown
				if runErr == nil {
					runErr = bridge.ErrProtocol
				}
			}
		}
		return runErr
	})
	// Do NOT substitute ctx.Err here: a joined/reconciled ready artifact wins a
	// late cancellation; auth/publication uncertainty must remain independently true.
	if err != nil {
		failureFor := downloadFailure
		if result.AuthFailure != nil {
			failureFor = libraryFailure
		}
		err = operationError(err, id, failureFor)
		var failure *model.Failure
		errors.As(err, &failure)
		kind := model.Failed
		if failure.Code == "canceled" {
			kind = model.Canceled
		}
		emit(model.OperationEvent{Type: kind, Failure: failure, Download: &result})
		return result, err
	}
	emit(model.OperationEvent{Type: model.Completed, Download: &result})
	return result, nil
}

// downloadChild streams validated nonterminal frames when onFrame is non-nil
// (action="download") and rejects any nonterminal frame when onFrame is nil
// (action="reconcile"). All owned process-group users join before return/panic.
func (c *Client) downloadChild(ctx context.Context, root *os.File, id string, book model.BookID, action, source string, onFrame func(bridge.Frame) error) (bridge.Frame, error) {
	request, _ := json.Marshal(struct {
		Protocol    int          `json:"protocol"`
		OperationID string       `json:"operation_id"`
		Action      string       `json:"action"`
		BookID      model.BookID `json:"book_id"`
	}{1, id, action, book})
	work, cancel := context.WithCancel(ctx)
	frames := make(chan bridge.Frame, 1)
	joined := make(chan struct{})
	var terminal bridge.Frame
	var err error
	go func() {
		defer close(joined)
		terminal, err = bridge.RunRoot(work, c.python, []string{"-I", "-B", "-c", source}, request, id, root, frames)
		close(frames)
	}()
	defer func() { cancel(); <-joined }()
	for frame := range frames {
		if onFrame == nil {
			return bridge.Frame{}, bridge.ErrProtocol
		}
		if err := ctx.Err(); err != nil {
			return bridge.Frame{}, err
		}
		if err := onFrame(frame); err != nil {
			return bridge.Frame{}, err
		}
	}
	<-joined
	return terminal, err
}
