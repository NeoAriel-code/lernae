// Package jobs contains the persistent Job model and orchestration boundary.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

type Status string

const (
	StatusQueued         Status = "queued"
	StatusRunning        Status = "running"
	StatusWaitingOnAgent Status = "waiting_on_agent"
	StatusSucceeded      Status = "succeeded"
	StatusFailed         Status = "failed"
	StatusInterrupted    Status = "interrupted"
	StatusCancelled      Status = "cancelled"
)

var (
	ErrNotFound                = errors.New("job not found")
	ErrInvalidKind             = errors.New("job kind does not support this operation")
	ErrInvalidTransition       = errors.New("invalid job status transition")
	ErrInvalidProgress         = errors.New("invalid job progress update")
	ErrInvalidPhase            = errors.New("invalid job phase update")
	ErrInvalidSessionReference = errors.New("invalid job Session reference")
)

const (
	KindRestoreAsset = "restore_asset"
	KindPlay         = "play"
	KindAcquire      = "acquire"
)

type Phase string

const (
	PhaseRestore Phase = "restore"
	PhaseLaunch  Phase = "launch"
	PhasePlaying Phase = "playing"
)

var safeSessionReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

type Job struct {
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	Status          Status    `json:"status"`
	Phase           Phase     `json:"phase,omitempty"`
	SessionID       string    `json:"session_id,omitempty"`
	TargetEditionID string    `json:"target_edition_id,omitempty"`
	ProgressCurrent int64     `json:"progress_current"`
	ProgressTotal   int64     `json:"progress_total"`
	Message         string    `json:"message,omitempty"`
	ErrorCode       string    `json:"error_code,omitempty"`
	ErrorDetail     string    `json:"error_detail,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type Repository interface {
	Create(context.Context, Job) error
	Get(context.Context, string) (Job, error)
	List(context.Context) ([]Job, error)
	Transition(context.Context, string, Status, Status, time.Time) error
	TransitionWithError(context.Context, string, Status, Status, string, string, time.Time) error
	UpdateProgress(context.Context, string, int64, int64, int64, int64, string, time.Time) error
	UpdatePlayPhase(context.Context, string, Phase, Phase, time.Time) error
	UpdatePlayProgress(context.Context, string, int64, int64, int64, int64, string, time.Time) error
	AttachPlaySession(context.Context, string, string, time.Time) error
	ReconcileTransient(context.Context, time.Time) (int64, error)
}

type Service struct {
	repository Repository
}

func NewService(repository Repository) *Service {
	return &Service{repository: repository}
}

func (s *Service) Create(ctx context.Context, kind string) (Job, error) {
	if kind == KindAcquire {
		return Job{}, ErrInvalidAcquisitionTarget
	}
	if kind == "" {
		return Job{}, errors.New("job kind must not be empty")
	}
	id, err := newID()
	if err != nil {
		return Job{}, fmt.Errorf("generate job ID: %w", err)
	}
	now := time.Now().UTC()
	job := Job{ID: id, Kind: kind, Status: StatusQueued, CreatedAt: now, UpdatedAt: now}
	if err := s.repository.Create(ctx, job); err != nil {
		return Job{}, err
	}
	return job, nil
}

func (s *Service) Get(ctx context.Context, id string) (Job, error) {
	return s.repository.Get(ctx, id)
}

func (s *Service) List(ctx context.Context) ([]Job, error) {
	return s.repository.List(ctx)
}

// UpdatePlayPhase advances an active PLAY Job through its bounded operation phases.
func (s *Service) UpdatePlayPhase(ctx context.Context, id string, next Phase) error {
	job, err := s.repository.Get(ctx, id)
	if err != nil {
		return err
	}
	if job.Kind != KindPlay {
		return fmt.Errorf("%w: Job %s has kind %q", ErrInvalidKind, id, job.Kind)
	}
	if job.Status != StatusRunning && job.Status != StatusWaitingOnAgent {
		return fmt.Errorf("%w: PLAY Job %s is %s", ErrInvalidTransition, id, job.Status)
	}
	if job.Phase == PhaseRestore && next == PhaseLaunch && job.Status != StatusRunning {
		return fmt.Errorf("%w: PLAY restore must finish before launch", ErrInvalidTransition)
	}
	if next == PhasePlaying && job.SessionID == "" {
		return fmt.Errorf("%w: PLAY Job %s has no confirmed Session", ErrInvalidSessionReference, id)
	}
	if !validPhaseAdvance(job.Phase, next) {
		return fmt.Errorf("%w: %q -> %q", ErrInvalidPhase, job.Phase, next)
	}
	if job.Phase == next {
		return nil
	}
	if err := s.repository.UpdatePlayPhase(ctx, id, job.Phase, next, time.Now().UTC()); err != nil {
		return err
	}
	return nil
}

// UpdatePlayProgress persists restore bytes only during the active restore phase.
// Later phases deliberately retain the verified restore byte totals unchanged.
func (s *Service) UpdatePlayProgress(ctx context.Context, id string, current, total int64) error {
	job, err := s.repository.Get(ctx, id)
	if err != nil {
		return err
	}
	if job.Kind != KindPlay {
		return fmt.Errorf("%w: Job %s has kind %q", ErrInvalidKind, id, job.Kind)
	}
	if job.Status != StatusWaitingOnAgent {
		return fmt.Errorf("%w: PLAY restore progress requires waiting_on_agent, got %s", ErrInvalidProgress, job.Status)
	}
	if job.Phase != PhaseRestore || total <= 0 || current < job.ProgressCurrent || current > total ||
		(job.ProgressTotal != 0 && job.ProgressTotal != total) {
		return fmt.Errorf("%w: phase %q progress %d/%d after %d/%d", ErrInvalidProgress,
			job.Phase, current, total, job.ProgressCurrent, job.ProgressTotal)
	}
	return s.repository.UpdatePlayProgress(ctx, id, job.ProgressCurrent, job.ProgressTotal,
		current, total, string(PhaseRestore), time.Now().UTC())
}

// AttachPlaySession records the first confirmed Session reference for an active PLAY Job.
func (s *Service) AttachPlaySession(ctx context.Context, id, sessionID string) error {
	job, err := s.repository.Get(ctx, id)
	if err != nil {
		return err
	}
	if job.Kind != KindPlay {
		return fmt.Errorf("%w: Job %s has kind %q", ErrInvalidKind, id, job.Kind)
	}
	if job.Status != StatusRunning && job.Status != StatusWaitingOnAgent {
		return fmt.Errorf("%w: PLAY Job %s is %s", ErrInvalidTransition, id, job.Status)
	}
	if job.Phase != PhaseLaunch && job.Phase != PhasePlaying {
		return fmt.Errorf("%w: Session cannot be attached during phase %q", ErrInvalidPhase, job.Phase)
	}
	if !safeSessionReference.MatchString(sessionID) {
		return fmt.Errorf("%w: malformed Session ID", ErrInvalidSessionReference)
	}
	if job.SessionID == sessionID {
		return nil
	}
	if job.SessionID != "" {
		return fmt.Errorf("%w: PLAY Job %s already references Session %s", ErrInvalidSessionReference, id, job.SessionID)
	}
	return s.repository.AttachPlaySession(ctx, id, sessionID, time.Now().UTC())
}

func validPhaseAdvance(current, next Phase) bool {
	switch next {
	case PhaseRestore:
		return current == "" || current == PhaseRestore
	case PhaseLaunch:
		return current == "" || current == PhaseRestore || current == PhaseLaunch
	case PhasePlaying:
		return current == PhaseLaunch || current == PhasePlaying
	default:
		return false
	}
}

func (s *Service) Transition(ctx context.Context, id string, next Status) error {
	job, err := s.repository.Get(ctx, id)
	if err != nil {
		return err
	}
	if !allowedTransition(job.Status, next) || (job.Kind == KindAcquire && next == StatusWaitingOnAgent) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, job.Status, next)
	}
	if job.Kind == KindPlay && next == StatusSucceeded && (job.Phase != PhasePlaying || job.SessionID == "") {
		return fmt.Errorf("%w: PLAY Job requires a playing phase and attached Session", ErrInvalidTransition)
	}
	if err := s.repository.Transition(ctx, id, job.Status, next, time.Now().UTC()); err != nil {
		return err
	}
	return nil
}

// TransitionWithError records a bounded failed, interrupted, or cancelled Job
// outcome. Cancellation is also accepted directly from queued so a durably
// accepted operation can be terminalized safely if it never enters running.
// Callers provide an allowlisted category and safe detail rather than raw
// infrastructure errors that may contain paths or transport data.
func (s *Service) TransitionWithError(ctx context.Context, id string, next Status, errorCode, errorDetail string) error {
	if next != StatusFailed && next != StatusInterrupted && next != StatusCancelled {
		return fmt.Errorf("%w: error outcome must be failed, interrupted, or cancelled", ErrInvalidTransition)
	}
	job, err := s.repository.Get(ctx, id)
	if err != nil {
		return err
	}
	// An unconfirmed acquisition reservation can fail while still queued;
	// running would falsely claim acceptance, and cancelled would falsely
	// claim that no execution occurred. Keep the generic transition table intact.
	queuedAcquisitionFailure := job.Kind == KindAcquire && job.Status == StatusQueued && next == StatusFailed
	if !allowedTransition(job.Status, next) && !queuedAcquisitionFailure {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, job.Status, next)
	}
	if job.Kind == KindAcquire {
		detail := acquisitionOutcomeDetail(next)
		if errorCode != "acquisition_"+string(next) || (errorDetail != "" && errorDetail != detail) {
			return ErrInvalidAcquisitionReason
		}
		errorDetail = detail
	}
	if err := s.repository.TransitionWithError(ctx, id, job.Status, next, errorCode, errorDetail, time.Now().UTC()); err != nil {
		return err
	}
	return nil
}

func (s *Service) ReconcileStartup(ctx context.Context) (int64, error) {
	// The repository also cancels queued PLAY Jobs that were accepted but never
	// entered running; unrelated queued Job kinds retain their existing state.
	count, err := s.repository.ReconcileTransient(ctx, time.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("reconcile transient jobs: %w", err)
	}
	return count, nil
}

func allowedTransition(current, next Status) bool {
	switch current {
	case StatusQueued:
		return next == StatusRunning || next == StatusCancelled
	case StatusRunning:
		return next == StatusWaitingOnAgent || next == StatusSucceeded || next == StatusFailed || next == StatusInterrupted
	case StatusWaitingOnAgent:
		return next == StatusRunning || next == StatusSucceeded || next == StatusFailed || next == StatusInterrupted
	default:
		return false
	}
}

func newID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}
