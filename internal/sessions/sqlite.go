package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"lernae/internal/domain"
)

type SQLiteRepository struct {
	db *sql.DB
}

func NewSQLiteRepository(db *sql.DB) *SQLiteRepository {
	return &SQLiteRepository{db: db}
}

func (r *SQLiteRepository) Create(ctx context.Context, session domain.Session) error {
	if session.ID == "" || session.WorkID == "" || session.EditionID == "" || session.AssetID == "" {
		return ErrInvalidIdentity
	}
	if session.EndedAt != nil || session.Outcome != "" {
		return fmt.Errorf("%w: a new Session must be active", ErrInvalidTransition)
	}
	startedAt, startedAtText, err := normalizedTimestamp(session.StartedAt)
	if err != nil {
		return err
	}
	session.StartedAt = startedAt
	var associated bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM assets a JOIN editions e ON e.id = a.edition_id
		WHERE a.id = ? AND a.edition_id = ? AND e.work_id = ?
	)`, session.AssetID, session.EditionID, session.WorkID).Scan(&associated); err != nil {
		return fmt.Errorf("validate Session catalog association: %w", err)
	}
	if !associated {
		return fmt.Errorf("%w: Work, Edition, and Asset do not form one catalog path", ErrInvalidIdentity)
	}
	if _, err := r.db.ExecContext(ctx, `INSERT INTO sessions
		(id, work_id, edition_id, asset_id, started_at_utc, ended_at_utc, outcome)
		VALUES (?, ?, ?, ?, ?, NULL, NULL)`, session.ID, session.WorkID, session.EditionID, session.AssetID, startedAtText); err != nil {
		return fmt.Errorf("persist Session: %w", err)
	}
	return nil
}

func (r *SQLiteRepository) Get(ctx context.Context, id domain.SessionID) (domain.Session, error) {
	session, err := scanSession(r.db.QueryRowContext(ctx, `SELECT id, work_id, edition_id, asset_id,
		started_at_utc, ended_at_utc, outcome FROM sessions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Session{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return domain.Session{}, fmt.Errorf("load Session %s: %w", id, err)
	}
	return session, nil
}

func (r *SQLiteRepository) Finish(ctx context.Context, id domain.SessionID, outcome domain.SessionOutcome, endedAt time.Time) error {
	if !validOutcome(outcome) {
		return fmt.Errorf("%w: %q", ErrInvalidOutcome, outcome)
	}
	endedAt, endedAtText, err := normalizedTimestamp(endedAt)
	if err != nil {
		return err
	}
	result, err := r.db.ExecContext(ctx, `UPDATE sessions SET ended_at_utc = ?, outcome = ?
		WHERE id = ? AND ended_at_utc IS NULL AND started_at_utc <= ?`,
		endedAtText, outcome, id, endedAtText)
	if err != nil {
		return fmt.Errorf("finalize Session %s: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm Session %s finalization: %w", id, err)
	}
	if rows != 0 {
		return nil
	}
	current, err := r.Get(ctx, id)
	if err != nil {
		return err
	}
	if current.EndedAt != nil {
		return fmt.Errorf("%w: Session %s is already terminal", ErrInvalidTransition, id)
	}
	if endedAt.Before(current.StartedAt) {
		return fmt.Errorf("%w: end precedes start", ErrInvalidTimestamp)
	}
	return fmt.Errorf("%w: Session %s was not finalized", ErrInvalidTransition, id)
}

func (r *SQLiteRepository) ReconcileStale(ctx context.Context, at time.Time) (int64, error) {
	_, atText, err := normalizedTimestamp(at)
	if err != nil {
		return 0, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin Session reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var futureSession bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM sessions WHERE ended_at_utc IS NULL AND started_at_utc > ?
	)`, atText).Scan(&futureSession); err != nil {
		return 0, fmt.Errorf("check active Session timestamps: %w", err)
	}
	if futureSession {
		return 0, fmt.Errorf("%w: reconciliation time precedes an active Session start", ErrInvalidTimestamp)
	}
	result, err := tx.ExecContext(ctx, `UPDATE sessions SET ended_at_utc = ?, outcome = ?
		WHERE ended_at_utc IS NULL`, atText, domain.SessionOutcomeInterrupted)
	if err != nil {
		return 0, fmt.Errorf("mark stale Sessions interrupted: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count reconciled Sessions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit Session reconciliation: %w", err)
	}
	return count, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSession(row rowScanner) (domain.Session, error) {
	var session domain.Session
	var startedAt string
	var endedAt, outcome sql.NullString
	if err := row.Scan(&session.ID, &session.WorkID, &session.EditionID, &session.AssetID,
		&startedAt, &endedAt, &outcome); err != nil {
		return domain.Session{}, err
	}
	parsedStart, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return domain.Session{}, fmt.Errorf("parse Session start timestamp: %w", err)
	}
	session.StartedAt = parsedStart.UTC()
	if endedAt.Valid {
		parsedEnd, err := time.Parse(time.RFC3339Nano, endedAt.String)
		if err != nil {
			return domain.Session{}, fmt.Errorf("parse Session end timestamp: %w", err)
		}
		parsedEnd = parsedEnd.UTC()
		session.EndedAt = &parsedEnd
	}
	if outcome.Valid {
		session.Outcome = domain.SessionOutcome(outcome.String)
	}
	return session, nil
}
