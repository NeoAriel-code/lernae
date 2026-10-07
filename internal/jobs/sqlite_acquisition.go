package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const acquisitionColumns = `id, kind, status, phase, session_id, progress_current, progress_total,
	message, error_code, error_detail, target_edition_id, created_at_utc, updated_at_utc`

func (r *SQLiteRepository) CreateAcquisition(ctx context.Context, job Job) (Job, error) {
	if job.Kind != KindAcquire || job.Status != StatusQueued || !safeSessionReference.MatchString(job.TargetEditionID) {
		return Job{}, ErrInvalidAcquisitionTarget
	}
	// The partial unique index is the arbiter. The no-op conflict update returns
	// the existing row in the same statement, with its identity/state unchanged.
	// INSERT ... SELECT checks Edition existence atomically with the write.
	stored, err := scanJob(r.db.QueryRowContext(ctx, `INSERT INTO jobs
		(id, kind, status, target_edition_id, created_at_utc, updated_at_utc)
		SELECT ?, ?, ?, id, ?, ? FROM editions WHERE id = ?
		ON CONFLICT(target_edition_id) WHERE kind = 'acquire' AND status IN ('queued', 'running')
		DO UPDATE SET id = jobs.id
		RETURNING `+acquisitionColumns,
		job.ID, KindAcquire, StatusQueued, formatAcquisitionTime(job.CreatedAt), formatTime(job.UpdatedAt), job.TargetEditionID))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrEditionNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("persist acquisition Job: %w", err)
	}
	return stored, nil
}

func (r *SQLiteRepository) GetAcquisition(ctx context.Context, id string) (Job, error) {
	job, err := scanJob(r.db.QueryRowContext(ctx, `SELECT `+acquisitionColumns+` FROM jobs WHERE id = ? AND kind = ?`, id, KindAcquire))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("load acquisition Job: %w", err)
	}
	return job, nil
}

func (r *SQLiteRepository) ListAcquisitions(ctx context.Context, limit int) ([]Job, error) {
	if limit < 1 || limit > MaxAcquisitionLimit {
		return nil, ErrInvalidAcquisitionLimit
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+acquisitionColumns+` FROM jobs
		WHERE kind = 'acquire' ORDER BY created_at_utc DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list acquisition Jobs: %w", err)
	}
	defer rows.Close()
	result := make([]Job, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("decode acquisition Job: %w", err)
		}
		result = append(result, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read acquisition Jobs: %w", err)
	}
	return result, nil
}

func nullableEditionTarget(id string) any {
	if id == "" {
		return nil
	}
	return id
}

// Fixed fractional precision keeps textual SQLite ordering chronological even
// for whole seconds versus adjacent nanoseconds. Existing Job formats stay put.
func formatAcquisitionTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
