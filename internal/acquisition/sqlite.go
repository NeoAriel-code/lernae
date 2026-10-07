package acquisition

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"lernae/internal/domain"
)

// Repository is acquisition's own optional persistence capability. The generic
// jobs.Repository interface and existing PLAY/restore mocks remain unchanged.
type Repository interface {
	Target(context.Context, domain.EditionID) (Target, error)
	Get(context.Context, string) (Selection, error)
	Accept(context.Context, Selection) (Selection, error)
}

type SQLiteRepository struct {
	db *sql.DB
}

func NewSQLiteRepository(db *sql.DB) *SQLiteRepository {
	return &SQLiteRepository{db: db}
}

// Target follows catalog's exact Edition/Work read shape, but reads only the
// context needed by providers rather than loading Assets or an entire graph.
func (r *SQLiteRepository) Target(ctx context.Context, editionID domain.EditionID) (Target, error) {
	var target Target
	var label, platform, region sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT e.id, e.work_id, e.label, e.platform, e.region, e.format,
		w.id, w.medium, w.work_type, w.title, w.summary
		FROM editions e JOIN works w ON w.id = e.work_id WHERE e.id = ?`, editionID).
		Scan(&target.Edition.ID, &target.Edition.WorkID, &label, &platform, &region, &target.Edition.Format,
			&target.Work.ID, &target.Work.Medium, &target.Work.WorkType, &target.Work.Title, &target.Work.Summary)
	if errors.Is(err, sql.ErrNoRows) {
		return Target{}, ErrNotFound
	}
	if err != nil {
		return Target{}, safeRepositoryError(ctx)
	}
	target.Edition.Label, target.Edition.Platform, target.Edition.Region = label.String, platform.String, region.String
	return target, nil
}

func (r *SQLiteRepository) Get(ctx context.Context, jobID string) (Selection, error) {
	var selection Selection
	var selectedAt string
	candidate := &selection.Candidate
	err := r.db.QueryRowContext(ctx, `SELECT s.job_id, j.target_edition_id, s.provider_id,
		s.candidate_id, s.candidate_handle, s.execution_ref, s.title, s.label, s.language, s.selected_at_utc
		FROM acquisition_selections s JOIN jobs j ON j.id = s.job_id
		WHERE s.job_id = ? AND j.kind = 'acquire'`, jobID).
		Scan(&candidate.JobID, &candidate.EditionID, &candidate.ProviderID, &candidate.Option.ID,
			&candidate.Handle, &candidate.Option.ExecutionRef, &candidate.Option.Metadata.Title,
			&candidate.Option.Metadata.Label, &candidate.Option.Metadata.Language, &selectedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Selection{}, ErrNoSelection
	}
	if err != nil {
		return Selection{}, safeRepositoryError(ctx)
	}
	selection.SelectedAt, err = time.Parse(time.RFC3339Nano, selectedAt)
	if err != nil || !validCandidate(*candidate) || selection.SelectedAt.IsZero() {
		return Selection{}, ErrUnavailable
	}
	return selection, nil
}

// Accept uses a single INSERT ... SELECT with the queued guard in the write
// statement, not a process-local read/insert check. SQLite serializes that guard
// against Job transitions across pools. The PK chooses the immutable winner.
func (r *SQLiteRepository) Accept(ctx context.Context, selection Selection) (Selection, error) {
	candidate := selection.Candidate
	if !validCandidate(candidate) || selection.SelectedAt.IsZero() {
		return Selection{}, ErrInvalidResponse
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO acquisition_selections
		(job_id, provider_id, candidate_id, candidate_handle, execution_ref, title, label, language, selected_at_utc)
		SELECT id, ?, ?, ?, ?, ?, ?, ?, ? FROM jobs
		WHERE id = ? AND kind = 'acquire' AND status = 'queued' AND target_edition_id = ?
		ON CONFLICT(job_id) DO NOTHING`,
		candidate.ProviderID, candidate.Option.ID, candidate.Handle, candidate.Option.ExecutionRef,
		candidate.Option.Metadata.Title, candidate.Option.Metadata.Label, candidate.Option.Metadata.Language,
		selection.SelectedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"), candidate.JobID, candidate.EditionID)
	if err != nil {
		return Selection{}, safeRepositoryError(ctx)
	}
	stored, err := r.Get(ctx, candidate.JobID)
	if err == nil {
		if stored.Candidate != candidate {
			return Selection{}, ErrConflict
		}
		return stored, nil
	}
	if !errors.Is(err, ErrNoSelection) {
		return Selection{}, err
	}
	// No row was written: distinguish a missing/wrong-kind Job from a state
	// race or target mismatch without exposing SQL details.
	var status string
	var editionID domain.EditionID
	err = r.db.QueryRowContext(ctx, `SELECT status, target_edition_id FROM jobs WHERE id = ? AND kind = 'acquire'`, candidate.JobID).
		Scan(&status, &editionID)
	if errors.Is(err, sql.ErrNoRows) {
		return Selection{}, ErrNotFound
	}
	if err != nil {
		return Selection{}, safeRepositoryError(ctx)
	}
	if editionID != candidate.EditionID {
		return Selection{}, ErrInvalidResponse
	}
	return Selection{}, ErrNotQueued
}

func validCandidate(candidate Candidate) bool {
	return localIDPattern.MatchString(candidate.JobID) && localIDPattern.MatchString(string(candidate.EditionID)) &&
		providerIDPattern.MatchString(candidate.ProviderID) && handlePattern.MatchString(candidate.Handle) && validOption(candidate.Option)
}

func safeRepositoryError(ctx context.Context) error {
	if ctx.Err() != nil {
		return ErrCancelled
	}
	return ErrUnavailable
}
