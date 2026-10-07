package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	DefaultAcquisitionLimit = 20
	MaxAcquisitionLimit     = 100
)

var (
	ErrInvalidAcquisitionTarget = errors.New("invalid acquisition Edition target")
	ErrEditionNotFound          = errors.New("acquisition Edition not found")
	ErrInvalidAcquisitionLimit  = errors.New("invalid acquisition list limit")
	ErrInvalidAcquisitionReason = errors.New("invalid acquisition outcome reason")
)

// AcquisitionRepository is an optional capability of the generic Job store.
// Existing PLAY/restore repositories need not implement acquisition operations.
// CreateAcquisition must atomically return the queued/running Job for a target,
// or create it if absent; a read followed by an insert is not sufficient.
type AcquisitionRepository interface {
	CreateAcquisition(context.Context, Job) (Job, error)
	GetAcquisition(context.Context, string) (Job, error)
	ListAcquisitions(context.Context, int) ([]Job, error)
}

func (s *Service) acquisitionRepository() (AcquisitionRepository, error) {
	repository, ok := s.repository.(AcquisitionRepository)
	if !ok {
		return nil, ErrInvalidKind
	}
	return repository, nil
}

// CreateAcquisition accepts durable provider-neutral intent for an existing
// Edition. Work is derived through Edition; neither ownership nor a configured
// provider is a prerequisite. This method does not schedule execution.
func (s *Service) CreateAcquisition(ctx context.Context, editionID string) (Job, error) {
	if !safeSessionReference.MatchString(editionID) {
		return Job{}, ErrInvalidAcquisitionTarget
	}
	repository, err := s.acquisitionRepository()
	if err != nil {
		return Job{}, err
	}
	id, err := newID()
	if err != nil {
		return Job{}, fmt.Errorf("generate acquisition Job ID: %w", err)
	}
	now := time.Now().UTC()
	return repository.CreateAcquisition(ctx, Job{
		ID: id, Kind: KindAcquire, TargetEditionID: editionID,
		Status: StatusQueued, CreatedAt: now, UpdatedAt: now,
	})
}

func (s *Service) GetAcquisition(ctx context.Context, id string) (Job, error) {
	if !safeSessionReference.MatchString(id) {
		return Job{}, ErrNotFound
	}
	repository, err := s.acquisitionRepository()
	if err != nil {
		return Job{}, err
	}
	return repository.GetAcquisition(ctx, id)
}

func (s *Service) ListAcquisitions(ctx context.Context, limit int) ([]Job, error) {
	if limit < 1 || limit > MaxAcquisitionLimit {
		return nil, ErrInvalidAcquisitionLimit
	}
	repository, err := s.acquisitionRepository()
	if err != nil {
		return nil, err
	}
	return repository.ListAcquisitions(ctx, limit)
}

// UpdateAcquisitionProgress reuses generic Job progress persistence with fixed
// safe presentation and monotonic bounded bytes, only after real acceptance.
func (s *Service) UpdateAcquisitionProgress(ctx context.Context, id string, current, total int64) error {
	job, err := s.GetAcquisition(ctx, id)
	if err != nil {
		return err
	}
	if job.Status != StatusRunning || total <= 0 || current < job.ProgressCurrent || current > total ||
		(job.ProgressTotal != 0 && total != job.ProgressTotal) {
		return ErrInvalidProgress
	}
	return s.repository.UpdateProgress(ctx, id, job.ProgressCurrent, job.ProgressTotal,
		current, total, "Acquisition in progress.", time.Now().UTC())
}

// Acquisition terminal reasons are fixed, not truncated infrastructure errors.
func acquisitionOutcomeDetail(status Status) string {
	switch status {
	case StatusFailed:
		return "Acquisition could not complete safely."
	case StatusInterrupted:
		return "Acquisition ended before completion was confirmed."
	case StatusCancelled:
		return "Acquisition was cancelled before execution."
	default:
		return ""
	}
}
