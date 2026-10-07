// Package sessions persists the lifecycle of a confirmed local launch.
package sessions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"lernae/internal/domain"
)

const sqliteTimestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

var (
	ErrNotFound          = errors.New("session not found")
	ErrInvalidTransition = errors.New("invalid session transition")
	ErrInvalidTimestamp  = errors.New("invalid session timestamp")
	ErrInvalidOutcome    = errors.New("invalid session outcome")
	ErrInvalidIdentity   = errors.New("invalid session identity")
	safeSessionID        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
)

// Repository is the durable lifecycle boundary for Sessions.
type Repository interface {
	Create(context.Context, domain.Session) error
	Get(context.Context, domain.SessionID) (domain.Session, error)
	Finish(context.Context, domain.SessionID, domain.SessionOutcome, time.Time) error
	ReconcileStale(context.Context, time.Time) (int64, error)
}

type Service struct {
	repository Repository
}

func NewService(repository Repository) *Service {
	return &Service{repository: repository}
}

// Start persists a Session after the caller has observed a successful process
// start. Failed launch attempts must not call this method.
func (s *Service) Start(
	ctx context.Context,
	workID domain.WorkID,
	editionID domain.EditionID,
	assetID domain.AssetID,
	startedAt time.Time,
) (domain.Session, error) {
	id, err := s.NewID()
	if err != nil {
		return domain.Session{}, fmt.Errorf("generate Session ID: %w", err)
	}
	return s.StartWithID(ctx, id, workID, editionID, assetID, startedAt)
}

// NewID allocates an opaque correlation identifier without creating Session
// state. The caller can include it in an Agent request before process start.
func (s *Service) NewID() (domain.SessionID, error) {
	id, err := newID()
	if err != nil {
		return "", fmt.Errorf("generate Session ID: %w", err)
	}
	return id, nil
}

// StartWithID persists the active Session associated with a previously
// allocated ID. Call only after the Agent confirms that its process started.
func (s *Service) StartWithID(
	ctx context.Context,
	id domain.SessionID,
	workID domain.WorkID,
	editionID domain.EditionID,
	assetID domain.AssetID,
	startedAt time.Time,
) (domain.Session, error) {
	if !safeSessionID.MatchString(string(id)) {
		return domain.Session{}, ErrInvalidIdentity
	}
	if strings.TrimSpace(string(workID)) == "" || strings.TrimSpace(string(editionID)) == "" || strings.TrimSpace(string(assetID)) == "" {
		return domain.Session{}, ErrInvalidIdentity
	}
	startedAt, _, err := normalizedTimestamp(startedAt)
	if err != nil {
		return domain.Session{}, err
	}
	session := domain.Session{
		ID:        id,
		WorkID:    workID,
		EditionID: editionID,
		AssetID:   assetID,
		StartedAt: startedAt,
	}
	if err := s.repository.Create(ctx, session); err != nil {
		return domain.Session{}, err
	}
	return session, nil
}

func (s *Service) Get(ctx context.Context, id domain.SessionID) (domain.Session, error) {
	return s.repository.Get(ctx, id)
}

// Finish records one terminal result. The repository's conditional update is
// the exactly-once gate when more than one terminal event races to persist.
func (s *Service) Finish(ctx context.Context, id domain.SessionID, outcome domain.SessionOutcome, endedAt time.Time) error {
	if !validOutcome(outcome) {
		return fmt.Errorf("%w: %q", ErrInvalidOutcome, outcome)
	}
	endedAt, _, err := normalizedTimestamp(endedAt)
	if err != nil {
		return err
	}
	return s.repository.Finish(ctx, id, outcome, endedAt)
}

// ReconcileStale marks all active rows interrupted at the caller-supplied
// startup time. No Job or filesystem recovery is performed here.
func (s *Service) ReconcileStale(ctx context.Context, at time.Time) (int64, error) {
	at, _, err := normalizedTimestamp(at)
	if err != nil {
		return 0, err
	}
	count, err := s.repository.ReconcileStale(ctx, at)
	if err != nil {
		return 0, fmt.Errorf("reconcile active Sessions: %w", err)
	}
	return count, nil
}

func normalizedTimestamp(value time.Time) (time.Time, string, error) {
	if value.IsZero() {
		return time.Time{}, "", ErrInvalidTimestamp
	}
	value = value.UTC()
	encoded := value.Format(sqliteTimestampLayout)
	if _, err := time.Parse(time.RFC3339Nano, encoded); err != nil {
		return time.Time{}, "", fmt.Errorf("%w: timestamp is outside RFC3339 range", ErrInvalidTimestamp)
	}
	return value, encoded, nil
}

func validOutcome(outcome domain.SessionOutcome) bool {
	switch outcome {
	case domain.SessionOutcomeNormalExit, domain.SessionOutcomeNonZeroExit, domain.SessionOutcomeInterrupted:
		return true
	default:
		return false
	}
}

func newID() (domain.SessionID, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes[:])
	return domain.SessionID(encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]), nil
}
