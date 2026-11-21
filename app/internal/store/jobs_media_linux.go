package store

import (
	"context"
	"path"
	"strings"

	"github.com/ncruces/go-sqlite3"
)

// LocalMedia inventories validated published audio files under downloads/.
// Inventory alone does not prove the file still exists or passes current validation.
type LocalMedia struct {
	Key           BookKey
	RelativePath  string
	Format        string
	PublishedAtMS int64
}

// Validate checks LocalMedia syntax, root-relative path confinement rules and bounds.
func (m LocalMedia) Validate() error {
	if err := m.Key.Validate(); err != nil {
		return err
	}
	if m.Format != "m4b" || m.PublishedAtMS < 0 {
		return ErrInvalidInput
	}
	if !safeSingleLine(m.RelativePath) ||
		strings.Contains(m.RelativePath, "\\") ||
		strings.Contains(m.RelativePath, "..") ||
		!strings.HasPrefix(m.RelativePath, "downloads/") ||
		path.Clean(m.RelativePath) != m.RelativePath ||
		!strings.HasSuffix(m.RelativePath, ".m4b") {
		return ErrInvalidInput
	}
	base := path.Base(m.RelativePath)
	if len(base) <= len(".m4b") || strings.HasPrefix(base, ".") {
		return ErrInvalidInput
	}
	return nil
}

// PutLocalMedia ensures book_identity exists and upserts local_media for media.Key.
func (s *Store) PutLocalMedia(ctx context.Context, media LocalMedia) error {
	if err := media.Validate(); err != nil {
		return err
	}
	return s.withTx(ctx, true, func(c *sqlite3.Conn) error {
		bookKey, err := ensureBookIdentity(c, media.Key)
		if err != nil {
			return err
		}
		var ownerKey int64
		var exists bool
		if err := queryStmt(c,
			`SELECT book_key FROM local_media WHERE relative_path = ?`,
			func(stmt *sqlite3.Stmt) error {
				ownerKey = stmt.ColumnInt64(0)
				exists = true
				return nil
			},
			media.RelativePath,
		); err != nil {
			return err
		}
		if exists && ownerKey != bookKey {
			return ErrMediaPathConflict
		}
		return execStmt(c,
			`INSERT INTO local_media(book_key, relative_path, format, published_at_ms)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(book_key) DO UPDATE SET
			     relative_path = excluded.relative_path,
			     format = excluded.format,
			     published_at_ms = excluded.published_at_ms`,
			bookKey, media.RelativePath, media.Format, media.PublishedAtMS,
		)
	})
}

// DeleteLocalMedia removes only local_media for key, preserving book_identity and state.
func (s *Store) DeleteLocalMedia(ctx context.Context, key BookKey) error {
	if err := key.Validate(); err != nil {
		return err
	}
	return s.withTx(ctx, true, func(c *sqlite3.Conn) error {
		return execStmt(c,
			`DELETE FROM local_media
			 WHERE book_key = (
			     SELECT book_key FROM book_identity WHERE provider = ? AND namespace = ? AND id = ?
			 )`,
			key.Provider, key.Namespace, key.ID,
		)
	})
}

// LocalMedia returns the published media record for key, if present.
func (s *Store) LocalMedia(ctx context.Context, key BookKey) (LocalMedia, bool, error) {
	if err := key.Validate(); err != nil {
		return LocalMedia{}, false, err
	}
	var out LocalMedia
	var found bool
	err := s.withTx(ctx, false, func(c *sqlite3.Conn) error {
		return queryStmt(c,
			`SELECT lm.relative_path, lm.format, lm.published_at_ms
			 FROM local_media lm
			 JOIN book_identity bi ON bi.book_key = lm.book_key
			 WHERE bi.provider = ? AND bi.namespace = ? AND bi.id = ?`,
			func(stmt *sqlite3.Stmt) error {
				out = LocalMedia{
					Key:           key,
					RelativePath:  stmt.ColumnText(0),
					Format:        stmt.ColumnText(1),
					PublishedAtMS: stmt.ColumnInt64(2),
				}
				found = true
				return nil
			},
			key.Provider, key.Namespace, key.ID,
		)
	})
	if err != nil {
		return LocalMedia{}, false, err
	}
	return out, found, nil
}

// LocalMediaList returns all inventoried media records ordered by BookKey.
func (s *Store) LocalMediaList(ctx context.Context) ([]LocalMedia, error) {
	var out []LocalMedia
	err := s.withTx(ctx, false, func(c *sqlite3.Conn) error {
		return queryStmt(c,
			`SELECT bi.provider, bi.namespace, bi.id, lm.relative_path, lm.format, lm.published_at_ms
			 FROM local_media lm
			 JOIN book_identity bi ON bi.book_key = lm.book_key
			 ORDER BY bi.provider ASC, bi.namespace ASC, bi.id ASC`,
			func(stmt *sqlite3.Stmt) error {
				out = append(out, LocalMedia{
					Key: BookKey{
						Provider:  stmt.ColumnText(0),
						Namespace: stmt.ColumnText(1),
						ID:        stmt.ColumnText(2),
					},
					RelativePath:  stmt.ColumnText(3),
					Format:        stmt.ColumnText(4),
					PublishedAtMS: stmt.ColumnInt64(5),
				})
				return nil
			},
		)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type JobState string

const (
	JobWaiting     JobState = "waiting"
	JobLicensing   JobState = "licensing"
	JobDownloading JobState = "downloading"
	JobPreparing   JobState = "preparing"
	JobPublishing  JobState = "publishing"
	JobReady       JobState = "ready"
	JobFailed      JobState = "failed"
	JobCanceled    JobState = "canceled"
)

func (s JobState) valid() bool {
	switch s {
	case JobWaiting, JobLicensing, JobDownloading, JobPreparing, JobPublishing,
		JobReady, JobFailed, JobCanceled:
		return true
	default:
		return false
	}
}

// IsLive reports whether the job is queued or actively executing.
func (s JobState) IsLive() bool {
	return s == JobWaiting || s.IsActive()
}

// IsActive reports whether the job holds the single active download slot.
func (s JobState) IsActive() bool {
	return s == JobLicensing || s == JobDownloading || s == JobPreparing || s == JobPublishing
}

type FailureCode string

const (
	FailureNetwork           FailureCode = "network"
	FailureThrottled         FailureCode = "throttled"
	FailureEntitlementDenied FailureCode = "entitlement_denied"
	FailureDiskFull          FailureCode = "disk_full"
	FailurePermissionDenied  FailureCode = "permission_denied"
	FailurePreparationFailed FailureCode = "preparation_failed"
	FailureInterrupted       FailureCode = "interrupted"
	FailureInternal          FailureCode = "internal"
)

func (c FailureCode) valid() bool {
	switch c {
	case FailureNetwork, FailureThrottled, FailureEntitlementDenied, FailureDiskFull,
		FailurePermissionDenied, FailurePreparationFailed, FailureInterrupted, FailureInternal:
		return true
	default:
		return false
	}
}

// NormalizeFailureCode maps non-allowlisted engine failure codes to FailureInternal.
func NormalizeFailureCode(code string) FailureCode {
	fc := FailureCode(code)
	if fc.valid() {
		return fc
	}
	return FailureInternal
}

// DownloadJob persists queued, active and terminal download attempts.
type DownloadJob struct {
	ID            string
	Key           BookKey
	QueueOrder    *int64
	State         JobState
	BytesReceived int64
	BytesTotal    *int64
	FailureCode   FailureCode
	CreatedAtMS   int64
	UpdatedAtMS   int64
}

// Validate checks DownloadJob state/queue/failure correlations and numeric bounds.
func (j DownloadJob) Validate() error {
	if !safeID(j.ID) {
		return ErrInvalidInput
	}
	if err := j.Key.Validate(); err != nil {
		return err
	}
	if !j.State.valid() {
		return ErrInvalidInput
	}
	if (j.State == JobWaiting) != (j.QueueOrder != nil) {
		return ErrInvalidInput
	}
	if j.QueueOrder != nil && *j.QueueOrder < 0 {
		return ErrInvalidInput
	}
	if (j.State == JobFailed) != (j.FailureCode != "") {
		return ErrInvalidInput
	}
	if j.FailureCode != "" && !j.FailureCode.valid() {
		return ErrInvalidInput
	}
	if j.BytesReceived < 0 || (j.BytesTotal != nil && *j.BytesTotal < j.BytesReceived) {
		return ErrInvalidInput
	}
	if j.CreatedAtMS < 0 || j.UpdatedAtMS < j.CreatedAtMS {
		return ErrInvalidInput
	}
	return nil
}

// PutDownloadJob creates or updates a download job, mapping live/active/queue
// uniqueness conflicts to ErrActiveJobExists, ErrDownloadInProgress or ErrQueueOrderConflict.
func (s *Store) PutDownloadJob(ctx context.Context, job DownloadJob) error {
	if err := job.Validate(); err != nil {
		return err
	}
	var failureCode *string
	if job.FailureCode != "" {
		code := string(job.FailureCode)
		failureCode = &code
	}
	return s.withTx(ctx, true, func(c *sqlite3.Conn) error {
		bookKey, err := ensureBookIdentity(c, job.Key)
		if err != nil {
			return err
		}
		var ownerKey int64
		var exists bool
		if err := queryStmt(c,
			`SELECT book_key FROM download_job WHERE job_id = ?`,
			func(stmt *sqlite3.Stmt) error {
				ownerKey = stmt.ColumnInt64(0)
				exists = true
				return nil
			},
			job.ID,
		); err != nil {
			return err
		}
		if exists && ownerKey != bookKey {
			return ErrInvalidInput
		}
		if job.State.IsLive() {
			var liveConflict bool
			if err := queryStmt(c,
				`SELECT 1 FROM download_job
				 WHERE book_key = ? AND job_id != ?
				   AND state IN ('waiting', 'licensing', 'downloading', 'preparing', 'publishing')
				 LIMIT 1`,
				func(*sqlite3.Stmt) error { liveConflict = true; return nil },
				bookKey, job.ID,
			); err != nil {
				return err
			}
			if liveConflict {
				return ErrActiveJobExists
			}
		}
		if job.State.IsActive() {
			var activeConflict bool
			if err := queryStmt(c,
				`SELECT 1 FROM download_job
				 WHERE job_id != ?
				   AND state IN ('licensing', 'downloading', 'preparing', 'publishing')
				 LIMIT 1`,
				func(*sqlite3.Stmt) error { activeConflict = true; return nil },
				job.ID,
			); err != nil {
				return err
			}
			if activeConflict {
				return ErrDownloadInProgress
			}
		}
		if job.QueueOrder != nil {
			var orderConflict bool
			if err := queryStmt(c,
				`SELECT 1 FROM download_job WHERE queue_order = ? AND job_id != ? LIMIT 1`,
				func(*sqlite3.Stmt) error { orderConflict = true; return nil },
				*job.QueueOrder, job.ID,
			); err != nil {
				return err
			}
			if orderConflict {
				return ErrQueueOrderConflict
			}
		}
		return execStmt(c,
			`INSERT INTO download_job(
			     job_id, book_key, queue_order, state, bytes_received, bytes_total, failure_code, created_at_ms, updated_at_ms
			 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(job_id) DO UPDATE SET
			     queue_order = excluded.queue_order,
			     state = excluded.state,
			     bytes_received = excluded.bytes_received,
			     bytes_total = excluded.bytes_total,
			     failure_code = excluded.failure_code,
			     created_at_ms = excluded.created_at_ms,
			     updated_at_ms = excluded.updated_at_ms`,
			job.ID, bookKey, job.QueueOrder, string(job.State),
			job.BytesReceived, job.BytesTotal, failureCode, job.CreatedAtMS, job.UpdatedAtMS,
		)
	})
}

// DownloadJobs returns all jobs ordered active first, then waiting by queue_order, then terminal.
func (s *Store) DownloadJobs(ctx context.Context) ([]DownloadJob, error) {
	var out []DownloadJob
	err := s.withTx(ctx, false, func(c *sqlite3.Conn) error {
		return queryStmt(c,
			`SELECT dj.job_id, bi.provider, bi.namespace, bi.id,
			        dj.queue_order, dj.state, dj.bytes_received, dj.bytes_total,
			        dj.failure_code, dj.created_at_ms, dj.updated_at_ms
			 FROM download_job dj
			 JOIN book_identity bi ON bi.book_key = dj.book_key
			 ORDER BY
			     CASE
			         WHEN dj.state IN ('licensing', 'downloading', 'preparing', 'publishing') THEN 0
			         WHEN dj.state = 'waiting' THEN 1
			         ELSE 2
			     END ASC,
			     dj.queue_order ASC,
			     dj.created_at_ms ASC,
			     dj.job_id ASC`,
			func(stmt *sqlite3.Stmt) error {
				out = append(out, scanDownloadJob(stmt))
				return nil
			},
		)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ReorderDownloadQueue atomically assigns 0..N-1 queue_order values to all waiting jobs
// using unused nonnegative intermediate values.
func (s *Store) ReorderDownloadQueue(ctx context.Context, orderedJobIDs []string, updatedAtMS int64) error {
	if updatedAtMS < 0 {
		return ErrInvalidInput
	}
	seen := make(map[string]struct{}, len(orderedJobIDs))
	for _, id := range orderedJobIDs {
		if !safeID(id) {
			return ErrInvalidInput
		}
		if _, duplicate := seen[id]; duplicate {
			return ErrInvalidInput
		}
		seen[id] = struct{}{}
	}
	return s.withTx(ctx, true, func(c *sqlite3.Conn) error {
		type waitingMeta struct {
			createdAtMS int64
			updatedAtMS int64
		}
		waiting := make(map[string]waitingMeta)
		if err := queryStmt(c,
			`SELECT job_id, created_at_ms, updated_at_ms FROM download_job WHERE state = 'waiting'`,
			func(stmt *sqlite3.Stmt) error {
				waiting[stmt.ColumnText(0)] = waitingMeta{
					createdAtMS: stmt.ColumnInt64(1),
					updatedAtMS: stmt.ColumnInt64(2),
				}
				return nil
			},
		); err != nil {
			return err
		}
		if len(waiting) != len(orderedJobIDs) {
			return ErrInvalidInput
		}
		for _, id := range orderedJobIDs {
			meta, ok := waiting[id]
			if !ok || updatedAtMS < meta.createdAtMS {
				return ErrInvalidInput
			}
		}
		if len(orderedJobIDs) == 0 {
			return nil
		}
		var base int64
		if err := queryStmt(c,
			`SELECT coalesce(max(queue_order), -1) + 1 FROM download_job`,
			func(stmt *sqlite3.Stmt) error {
				base = stmt.ColumnInt64(0)
				return nil
			},
		); err != nil {
			return err
		}
		for i, id := range orderedJobIDs {
			if err := execStmt(c,
				`UPDATE download_job SET queue_order = ? WHERE job_id = ?`,
				base+int64(i), id,
			); err != nil {
				return err
			}
		}
		for i, id := range orderedJobIDs {
			nextUpdated := max(waiting[id].updatedAtMS, updatedAtMS)
			if err := execStmt(c,
				`UPDATE download_job SET queue_order = ?, updated_at_ms = ? WHERE job_id = ?`,
				int64(i), nextUpdated, id,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReconcileActiveJobs transitions any interrupted active jobs on startup to JobReady
// when confirmed in published, or JobFailed with FailureInterrupted otherwise.
func (s *Store) ReconcileActiveJobs(ctx context.Context, updatedAtMS int64, published map[BookKey]bool) ([]DownloadJob, error) {
	if updatedAtMS < 0 {
		return nil, ErrInvalidInput
	}
	var reconciled []DownloadJob
	err := s.withTx(ctx, true, func(c *sqlite3.Conn) error {
		var active []DownloadJob
		if err := queryStmt(c,
			`SELECT dj.job_id, bi.provider, bi.namespace, bi.id,
			        dj.queue_order, dj.state, dj.bytes_received, dj.bytes_total,
			        dj.failure_code, dj.created_at_ms, dj.updated_at_ms
			 FROM download_job dj
			 JOIN book_identity bi ON bi.book_key = dj.book_key
			 WHERE dj.state IN ('licensing', 'downloading', 'preparing', 'publishing')
			 ORDER BY dj.created_at_ms ASC, dj.job_id ASC`,
			func(stmt *sqlite3.Stmt) error {
				active = append(active, scanDownloadJob(stmt))
				return nil
			},
		); err != nil {
			return err
		}
		for _, job := range active {
			if updatedAtMS < job.CreatedAtMS {
				return ErrInvalidInput
			}
			job.QueueOrder = nil
			job.UpdatedAtMS = max(job.UpdatedAtMS, updatedAtMS)
			var failureCode *string
			if published[job.Key] {
				job.State = JobReady
				job.FailureCode = ""
			} else {
				job.State = JobFailed
				job.FailureCode = FailureInterrupted
				code := string(FailureInterrupted)
				failureCode = &code
			}
			if err := execStmt(c,
				`UPDATE download_job
				 SET state = ?, queue_order = NULL, failure_code = ?, updated_at_ms = ?
				 WHERE job_id = ?`,
				string(job.State), failureCode, job.UpdatedAtMS, job.ID,
			); err != nil {
				return err
			}
			reconciled = append(reconciled, job)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reconciled, nil
}

func scanDownloadJob(stmt *sqlite3.Stmt) DownloadJob {
	var failure FailureCode
	if fc := columnOptionalText(stmt, 8); fc != nil {
		failure = FailureCode(*fc)
	}
	return DownloadJob{
		ID: stmt.ColumnText(0),
		Key: BookKey{
			Provider:  stmt.ColumnText(1),
			Namespace: stmt.ColumnText(2),
			ID:        stmt.ColumnText(3),
		},
		QueueOrder:    columnOptionalInt64(stmt, 4),
		State:         JobState(stmt.ColumnText(5)),
		BytesReceived: stmt.ColumnInt64(6),
		BytesTotal:    columnOptionalInt64(stmt, 7),
		FailureCode:   failure,
		CreatedAtMS:   stmt.ColumnInt64(9),
		UpdatedAtMS:   stmt.ColumnInt64(10),
	}
}
