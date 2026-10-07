package acquisition

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (r *SQLiteRepository) GetReservation(ctx context.Context, jobID string) (Reservation, bool, error) {
	var reservation Reservation
	err := r.db.QueryRowContext(ctx, `SELECT job_id, execution_id, dispatch_outcome
		FROM acquisition_executions WHERE job_id = ?`, jobID).
		Scan(&reservation.JobID, &reservation.ExecutionID, &reservation.Outcome)
	if errors.Is(err, sql.ErrNoRows) {
		return Reservation{}, false, nil
	}
	if err != nil {
		return Reservation{}, false, safeRepositoryError(ctx)
	}
	return reservation, true, nil
}

// Reserve arbitrates with one guarded SQLite write across independent pools.
// Only RowsAffected == 1 grants authority to dispatch; reads never do. Compare
// the FULL exact immutable selection as defense against trusted caller mistakes.
func (r *SQLiteRepository) Reserve(ctx context.Context, selection Selection, executionID string) (Reservation, bool, error) {
	candidate := selection.Candidate
	if !validCandidate(candidate) || !handlePattern.MatchString(executionID) {
		return Reservation{}, false, ErrInvalidResponse
	}
	result, err := r.db.ExecContext(ctx, `INSERT INTO acquisition_executions
		(job_id, execution_id, reserved_at_utc)
		SELECT j.id, ?, ? FROM jobs j JOIN acquisition_selections s ON s.job_id = j.id
		WHERE j.id = ? AND j.kind = 'acquire' AND j.status = 'queued' AND j.target_edition_id = ?
		AND s.provider_id = ? AND s.candidate_id = ? AND s.candidate_handle = ?
		AND s.execution_ref = ? AND s.title = ? AND s.label = ? AND s.language = ? AND s.selected_at_utc = ?
		ON CONFLICT(job_id) DO NOTHING`, executionID, executionTime(time.Now()),
		candidate.JobID, candidate.EditionID, candidate.ProviderID, candidate.Option.ID, candidate.Handle,
		candidate.Option.ExecutionRef, candidate.Option.Metadata.Title, candidate.Option.Metadata.Label,
		candidate.Option.Metadata.Language, executionTime(selection.SelectedAt))
	if err != nil {
		return Reservation{}, false, safeRepositoryError(ctx)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Reservation{}, false, ErrUnavailable
	}
	if count == 1 {
		// Never rely on a subsequent read to recover dispatch authority after an
		// uncertain write. A caller that cannot confirm this write must not call.
		return Reservation{JobID: candidate.JobID, ExecutionID: executionID, Outcome: DispatchReserved}, true, nil
	}
	stored, exists, err := r.GetReservation(ctx, candidate.JobID)
	if err != nil || exists {
		return stored, false, err
	}
	return Reservation{}, false, ErrNotQueued
}

func (r *SQLiteRepository) Decide(ctx context.Context, reservation Reservation, outcome DispatchOutcome) error {
	if outcome != DispatchAccepted && outcome != DispatchRejected && outcome != DispatchUnconfirmed {
		return ErrInvalidResponse
	}
	result, err := r.db.ExecContext(ctx, `UPDATE acquisition_executions
		SET dispatch_outcome = ?, decided_at_utc = ?
		WHERE job_id = ? AND execution_id = ? AND dispatch_outcome = 'reserved'`,
		outcome, executionTime(time.Now()), reservation.JobID, reservation.ExecutionID)
	if err != nil {
		return safeRepositoryError(ctx)
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return ErrUnavailable
	}
	return nil
}

func executionTime(at time.Time) string {
	return at.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
