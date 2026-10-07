package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type SQLiteRepository struct {
	db *sql.DB
}

func NewSQLiteRepository(db *sql.DB) *SQLiteRepository {
	return &SQLiteRepository{db: db}
}

func (r *SQLiteRepository) Create(ctx context.Context, job Job) error {
	createdAt := formatTime(job.CreatedAt)
	if job.Kind == KindAcquire {
		createdAt = formatAcquisitionTime(job.CreatedAt)
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO jobs (id, kind, status, target_edition_id, created_at_utc, updated_at_utc)
		VALUES (?, ?, ?, ?, ?, ?)`, job.ID, job.Kind, job.Status, nullableEditionTarget(job.TargetEditionID), createdAt, formatTime(job.UpdatedAt))
	if err != nil {
		return fmt.Errorf("persist job %s: %w", job.ID, err)
	}
	return nil
}

func (r *SQLiteRepository) Get(ctx context.Context, id string) (Job, error) {
	job, err := scanJob(r.db.QueryRowContext(ctx, `SELECT id, kind, status, phase, session_id, progress_current, progress_total,
		message, error_code, error_detail, target_edition_id, created_at_utc, updated_at_utc FROM jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Job{}, fmt.Errorf("load job %s: %w", id, err)
	}
	return job, nil
}

func (r *SQLiteRepository) List(ctx context.Context) ([]Job, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, kind, status, phase, session_id, progress_current, progress_total,
		message, error_code, error_detail, target_edition_id, created_at_utc, updated_at_utc FROM jobs ORDER BY created_at_utc, id`)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	jobs := make([]Job, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("decode job: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read jobs: %w", err)
	}
	return jobs, nil
}

func (r *SQLiteRepository) Transition(ctx context.Context, id string, from, to Status, at time.Time) error {
	if to == StatusWaitingOnAgent {
		job, err := r.Get(ctx, id)
		if err != nil {
			return err
		}
		if job.Kind == KindAcquire {
			return ErrInvalidTransition
		}
	}
	query := `UPDATE jobs SET status = ?, updated_at_utc = ? WHERE id = ? AND status = ?`
	args := []any{to, formatTime(at), id, from}
	if to == StatusSucceeded {
		query = `UPDATE jobs SET status = ?, updated_at_utc = ? WHERE id = ? AND status = ?
			AND (kind <> ? OR (phase = ? AND session_id IS NOT NULL AND EXISTS (
				SELECT 1 FROM sessions WHERE sessions.id = jobs.session_id
				AND sessions.ended_at_utc IS NOT NULL AND sessions.outcome IS NOT NULL
			)))`
		args = append(args, KindPlay, PhasePlaying)
	}
	result, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update job %s status: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm job %s status update: %w", id, err)
	}
	if rows == 0 {
		current, err := r.Get(ctx, id)
		if err != nil {
			return err
		}
		if current.Status == from && current.Kind == KindPlay && to == StatusSucceeded {
			return fmt.Errorf("%w: PLAY Job %s requires a playing phase and persisted terminal Session evidence", ErrInvalidTransition, id)
		}
		return fmt.Errorf("%w: job %s is already %s, expected %s", ErrInvalidTransition, id, current.Status, from)
	}
	return nil
}

func (r *SQLiteRepository) TransitionWithError(ctx context.Context, id string, from, to Status, errorCode, errorDetail string, at time.Time) error {
	result, err := r.db.ExecContext(ctx, `UPDATE jobs SET status = ?, error_code = ?, error_detail = ?, updated_at_utc = ?
		WHERE id = ? AND status = ?`, to, errorCode, errorDetail, formatTime(at), id, from)
	if err != nil {
		return fmt.Errorf("update Job %s failure state: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm Job %s failure state: %w", id, err)
	}
	if rows == 0 {
		current, err := r.Get(ctx, id)
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: job %s is already %s, expected %s", ErrInvalidTransition, id, current.Status, from)
	}
	return nil
}

func (r *SQLiteRepository) UpdateProgress(ctx context.Context, id string, expectedCurrent, expectedTotal, current, total int64, message string, at time.Time) error {
	result, err := r.db.ExecContext(ctx, `UPDATE jobs SET progress_current = ?, progress_total = ?, message = ?, updated_at_utc = ?
		WHERE id = ? AND status IN (?, ?) AND progress_current = ? AND progress_total = ?`,
		current, total, message, formatTime(at), id, StatusRunning, StatusWaitingOnAgent, expectedCurrent, expectedTotal)
	if err != nil {
		return fmt.Errorf("update Job %s progress: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm Job %s progress update: %w", id, err)
	}
	if rows == 0 {
		job, err := r.Get(ctx, id)
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: Job %s changed while progress was persisted (status %s, progress %d/%d)",
			ErrInvalidProgress, id, job.Status, job.ProgressCurrent, job.ProgressTotal)
	}
	return nil
}

func (r *SQLiteRepository) UpdatePlayPhase(ctx context.Context, id string, expected, next Phase, at time.Time) error {
	if !validPhaseAdvance(expected, next) {
		return fmt.Errorf("%w: %q -> %q", ErrInvalidPhase, expected, next)
	}
	result, err := r.db.ExecContext(ctx, `UPDATE jobs SET phase = ?, updated_at_utc = ?
		WHERE id = ? AND kind = ? AND status IN (?, ?) AND phase = ?
		AND (? != ? OR EXISTS (SELECT 1 FROM sessions WHERE sessions.id = jobs.session_id))
		AND (phase <> ? OR ? <> ? OR status = ?)`,
		next, formatTime(at), id, KindPlay, StatusRunning, StatusWaitingOnAgent, expected, next, PhasePlaying,
		PhaseRestore, next, PhaseLaunch, StatusRunning)
	if err != nil {
		return fmt.Errorf("update PLAY Job %s phase: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm PLAY Job %s phase update: %w", id, err)
	}
	if rows == 0 {
		job, err := r.Get(ctx, id)
		if err != nil {
			return err
		}
		if job.Kind != KindPlay {
			return fmt.Errorf("%w: Job %s has kind %q", ErrInvalidKind, id, job.Kind)
		}
		if job.Status != StatusRunning && job.Status != StatusWaitingOnAgent {
			return fmt.Errorf("%w: PLAY Job %s is %s", ErrInvalidTransition, id, job.Status)
		}
		if job.Phase == expected && expected == PhaseRestore && next == PhaseLaunch && job.Status == StatusWaitingOnAgent {
			return fmt.Errorf("%w: PLAY restore must finish before launch", ErrInvalidTransition)
		}
		return fmt.Errorf("%w: PLAY Job %s phase changed concurrently", ErrInvalidPhase, id)
	}
	return nil
}

func (r *SQLiteRepository) UpdatePlayProgress(ctx context.Context, id string, expectedCurrent, expectedTotal, current, total int64, message string, at time.Time) error {
	if total <= 0 || current < expectedCurrent || current > total ||
		(expectedTotal != 0 && expectedTotal != total) {
		return ErrInvalidProgress
	}
	result, err := r.db.ExecContext(ctx, `UPDATE jobs SET progress_current = ?, progress_total = ?, message = ?, updated_at_utc = ?
		WHERE id = ? AND kind = ? AND status = ? AND phase = ? AND progress_current = ? AND progress_total = ?`,
		current, total, message, formatTime(at), id, KindPlay, StatusWaitingOnAgent, PhaseRestore, expectedCurrent, expectedTotal)
	if err != nil {
		return fmt.Errorf("update PLAY Job %s restore progress: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm PLAY Job %s restore progress update: %w", id, err)
	}
	if rows == 0 {
		return r.playUpdateConflict(ctx, id, ErrInvalidProgress)
	}
	return nil
}

func (r *SQLiteRepository) AttachPlaySession(ctx context.Context, id, sessionID string, at time.Time) error {
	if !safeSessionReference.MatchString(sessionID) {
		return fmt.Errorf("%w: malformed Session ID", ErrInvalidSessionReference)
	}
	result, err := r.db.ExecContext(ctx, `UPDATE jobs SET session_id = ?, updated_at_utc = ?
		WHERE id = ? AND kind = ? AND status IN (?, ?) AND phase IN (?, ?) AND session_id IS NULL
		AND EXISTS (SELECT 1 FROM sessions WHERE sessions.id = ?)`,
		sessionID, formatTime(at), id, KindPlay, StatusRunning, StatusWaitingOnAgent, PhaseLaunch, PhasePlaying, sessionID)
	if err != nil {
		return fmt.Errorf("attach Session to PLAY Job %s: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm Session attachment to PLAY Job %s: %w", id, err)
	}
	if rows == 0 {
		return r.playUpdateConflict(ctx, id, ErrInvalidSessionReference)
	}
	return nil
}

func (r *SQLiteRepository) playUpdateConflict(ctx context.Context, id string, fallback error) error {
	job, err := r.Get(ctx, id)
	if err != nil {
		return err
	}
	if job.Kind != KindPlay {
		return fmt.Errorf("%w: Job %s has kind %q", ErrInvalidKind, id, job.Kind)
	}
	if job.Status != StatusRunning && job.Status != StatusWaitingOnAgent {
		return fmt.Errorf("%w: PLAY Job %s is %s", ErrInvalidTransition, id, job.Status)
	}
	return fmt.Errorf("%w: PLAY Job %s changed concurrently", fallback, id)
}

func (r *SQLiteRepository) ReconcileTransient(ctx context.Context, at time.Time) (int64, error) {
	// Startup remains single-owning-Server recovery, not a peer-safe lease.
	// A queued reservation may have crossed the external boundary before a
	// crash. Fail conservatively without pretending acceptance or replaying it.
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	queued, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'failed',
		error_code = 'acquisition_failed', error_detail = ?, updated_at_utc = ?
		WHERE kind = 'acquire' AND status = 'queued' AND EXISTS (
			SELECT 1 FROM acquisition_executions e WHERE e.job_id = jobs.id
			AND e.dispatch_outcome IN ('reserved', 'accepted', 'unconfirmed')
		)`, acquisitionOutcomeDetail(StatusFailed), formatTime(at))
	if err != nil {
		return 0, err
	}
	queuedCount, err := queued.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE acquisition_executions
		SET dispatch_outcome = 'unconfirmed', decided_at_utc = ?
		WHERE dispatch_outcome = 'reserved'`, at.UTC().Format("2006-01-02T15:04:05.000000000Z")); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET
		status = CASE WHEN kind = ? AND status = ? THEN ? ELSE ? END,
		error_code = CASE WHEN kind = ? AND status = ? THEN ? WHEN kind = ? THEN ? ELSE error_code END,
		error_detail = CASE WHEN kind = ? AND status = ? THEN ? WHEN kind = ? THEN ? ELSE error_detail END,
		updated_at_utc = ?
		WHERE status IN (?, ?) OR (kind = ? AND status = ?)`,
		KindPlay, StatusQueued, StatusCancelled, StatusInterrupted,
		KindPlay, StatusQueued, "play_not_started", KindAcquire, "acquisition_interrupted",
		KindPlay, StatusQueued, "PLAY was accepted but did not start before Server restart.",
		KindAcquire, acquisitionOutcomeDetail(StatusInterrupted),
		formatTime(at), StatusRunning, StatusWaitingOnAgent, KindPlay, StatusQueued)
	if err != nil {
		return 0, fmt.Errorf("reconcile orphaned jobs: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count reconciled jobs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count + queuedCount, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (Job, error) {
	var job Job
	var sessionID, targetEditionID sql.NullString
	var createdAt, updatedAt string
	if err := row.Scan(&job.ID, &job.Kind, &job.Status, &job.Phase, &sessionID, &job.ProgressCurrent, &job.ProgressTotal,
		&job.Message, &job.ErrorCode, &job.ErrorDetail, &targetEditionID, &createdAt, &updatedAt); err != nil {
		return Job{}, err
	}
	if sessionID.Valid {
		job.SessionID = sessionID.String
	}
	if targetEditionID.Valid {
		job.TargetEditionID = targetEditionID.String
	}
	var err error
	if job.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Job{}, fmt.Errorf("parse created timestamp: %w", err)
	}
	if job.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return Job{}, fmt.Errorf("parse updated timestamp: %w", err)
	}
	return job, nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
