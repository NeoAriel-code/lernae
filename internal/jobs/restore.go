package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"lernae/internal/agent"
)

var (
	ErrRestoreRequestJobMismatch = errors.New("restore request does not match its Job")
	ErrRestoreClientRequired     = errors.New("restore requires an Agent client")
	ErrInvalidRestoreProgress    = errors.New("Agent returned invalid restore progress")
	ErrInvalidRestoreProof       = errors.New("Agent did not prove a verified final local-cache Asset")
)

const restoreJobKind = KindRestoreAsset

const (
	jobProgressByteInterval = 1 << 20
	jobProgressTimeInterval = 200 * time.Millisecond
)

// RestoreClient is the typed Agent restore boundary consumed by Job orchestration.
type RestoreClient interface {
	RestoreAsset(context.Context, agent.RestoreAsset, func(agent.RestoreProgress)) (agent.RestoreResult, error)
}

type restorePhaseFailure struct {
	status Status
	code   string
	cause  error
}

func (failure *restorePhaseFailure) Error() string { return failure.cause.Error() }
func (failure *restorePhaseFailure) Unwrap() error { return failure.cause }

// RunRestore preserves the standalone restore_asset Job lifecycle: it owns the
// running, succeeded, failed, or interrupted terminal state transitions.
func (service *Service) RunRestore(ctx context.Context, jobID string, request agent.RestoreAsset, client RestoreClient) (agent.RestoreResult, error) {
	job, err := service.validateRestoreCall(ctx, jobID, request, client)
	if err != nil {
		return agent.RestoreResult{}, err
	}
	if job.Kind != restoreJobKind {
		return agent.RestoreResult{}, fmt.Errorf("%w: Job %s has kind %q, want %q", ErrInvalidKind, jobID, job.Kind, restoreJobKind)
	}
	if err := service.Transition(ctx, jobID, StatusRunning); err != nil {
		return agent.RestoreResult{}, err
	}

	result, err := service.RunRestorePhase(ctx, jobID, request, client)
	if err != nil {
		var failure *restorePhaseFailure
		if errors.As(err, &failure) {
			return agent.RestoreResult{}, service.failRestore(jobID, failure.status, failure.code, failure.cause)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return agent.RestoreResult{}, service.failRestore(jobID, StatusInterrupted, "restore_interrupted", ctxErr)
		}
		return agent.RestoreResult{}, err
	}

	// A validated terminal Agent result is already committed; persist that fact
	// even if the caller cancels immediately after receiving it.
	transitionCtx, cancelTransition := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelTransition()
	if err := service.Transition(transitionCtx, jobID, StatusSucceeded); err != nil {
		return agent.RestoreResult{}, err
	}
	return result, nil
}

// RunRestorePhase runs the Agent restore work without owning the outer Job's
// terminal result. The caller must already have moved the Job to running. PLAY
// Jobs return to running so the outer operation can continue; standalone restore
// Jobs remain waiting_on_agent for RunRestore to preserve their lifecycle.
func (service *Service) RunRestorePhase(ctx context.Context, jobID string, request agent.RestoreAsset, client RestoreClient) (agent.RestoreResult, error) {
	job, err := service.validateRestoreCall(ctx, jobID, request, client)
	if err != nil {
		return agent.RestoreResult{}, err
	}
	if job.Kind != restoreJobKind && job.Kind != KindPlay {
		return agent.RestoreResult{}, fmt.Errorf("%w: Job %s has kind %q", ErrInvalidKind, jobID, job.Kind)
	}
	if job.Status != StatusRunning {
		return agent.RestoreResult{}, fmt.Errorf("%w: restore phase requires a running Job, got %s", ErrInvalidTransition, job.Status)
	}
	if job.Kind == KindPlay {
		if err := service.UpdatePlayPhase(ctx, jobID, PhaseRestore); err != nil {
			return agent.RestoreResult{}, err
		}
	}
	if err := service.Transition(ctx, jobID, StatusWaitingOnAgent); err != nil {
		if ctx.Err() != nil {
			return agent.RestoreResult{}, service.restorePhaseError(jobID, StatusInterrupted, "restore_interrupted", ctx.Err())
		}
		return agent.RestoreResult{}, service.restorePhaseError(jobID, StatusFailed, "restore_start_failed", err)
	}

	operationCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var previous *agent.RestoreProgress
	var lastPersisted *agent.RestoreProgress
	var lastPersistedAt time.Time
	var lastPersistedBytes int64
	var progressErr error
	result, restoreErr := client.RestoreAsset(operationCtx, request, func(next agent.RestoreProgress) {
		if progressErr != nil || operationCtx.Err() != nil {
			return
		}
		if err := agent.ValidateProgressAfter(previous, next); err != nil {
			progressErr = fmt.Errorf("%w: %v", ErrInvalidRestoreProgress, err)
			cancel()
			return
		}
		now := time.Now()
		if shouldPersistRestoreProgress(lastPersisted, lastPersistedAt, lastPersistedBytes, next, now) {
			if err := service.persistRestoreProgress(operationCtx, jobID, next); err != nil {
				progressErr = fmt.Errorf("persist Agent restore progress: %w", err)
				cancel()
				return
			}
			copy := next
			lastPersisted = &copy
			lastPersistedAt = now
			lastPersistedBytes = next.CurrentBytes
		}
		copy := next
		previous = &copy
	})

	if progressErr != nil {
		return agent.RestoreResult{}, service.restorePhaseError(jobID, StatusFailed, "restore_progress_failed", progressErr)
	}
	if restoreErr != nil {
		if ctx.Err() != nil || errors.Is(restoreErr, context.Canceled) || errors.Is(restoreErr, context.DeadlineExceeded) {
			cause := restoreErr
			if ctxErr := ctx.Err(); ctxErr != nil {
				cause = ctxErr
			}
			return agent.RestoreResult{}, service.restorePhaseError(jobID, StatusInterrupted, "restore_interrupted", cause)
		}
		return agent.RestoreResult{}, service.restorePhaseError(jobID, StatusFailed, "restore_failed", restoreErr)
	}
	if previous == nil || previous.Phase != agent.RestorePhaseComplete || previous.CurrentBytes != previous.TotalBytes {
		return agent.RestoreResult{}, service.restorePhaseError(jobID, StatusFailed, "restore_proof_missing", ErrInvalidRestoreProof)
	}
	if err := result.ValidateFor(request); err != nil {
		return agent.RestoreResult{}, service.restorePhaseError(jobID, StatusFailed, "restore_proof_invalid", fmt.Errorf("%w: %v", ErrInvalidRestoreProof, err))
	}
	if job.Kind == KindPlay {
		if err := service.returnRestoreJobToRunning(jobID); err != nil {
			return agent.RestoreResult{}, err
		}
	}
	return result, nil
}

func (service *Service) validateRestoreCall(ctx context.Context, jobID string, request agent.RestoreAsset, client RestoreClient) (Job, error) {
	if ctx == nil {
		return Job{}, errors.New("restore requires a context")
	}
	if client == nil {
		return Job{}, ErrRestoreClientRequired
	}
	if err := request.Validate(); err != nil {
		return Job{}, err
	}
	if request.JobID != jobID {
		return Job{}, ErrRestoreRequestJobMismatch
	}
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	return service.repository.Get(ctx, jobID)
}

func shouldPersistRestoreProgress(last *agent.RestoreProgress, lastAt time.Time, lastBytes int64, next agent.RestoreProgress, now time.Time) bool {
	if last == nil || next.Phase != last.Phase || next.Phase == agent.RestorePhaseComplete {
		return true
	}
	return next.CurrentBytes-lastBytes >= jobProgressByteInterval || now.Sub(lastAt) >= jobProgressTimeInterval
}

func (service *Service) persistRestoreProgress(ctx context.Context, jobID string, progress agent.RestoreProgress) error {
	job, err := service.repository.Get(ctx, jobID)
	if err != nil {
		return err
	}
	if job.Kind == KindPlay {
		if job.Status != StatusWaitingOnAgent || job.Phase != PhaseRestore {
			return fmt.Errorf("%w: PLAY restore progress cannot update phase %q in %s status", ErrInvalidProgress, job.Phase, job.Status)
		}
		if progress.TotalBytes <= 0 || progress.CurrentBytes < job.ProgressCurrent || progress.CurrentBytes > progress.TotalBytes ||
			(job.ProgressTotal != 0 && job.ProgressTotal != progress.TotalBytes) {
			return ErrInvalidProgress
		}
		return service.repository.UpdatePlayProgress(ctx, jobID, job.ProgressCurrent, job.ProgressTotal,
			progress.CurrentBytes, progress.TotalBytes, string(progress.Phase), time.Now().UTC())
	}
	if job.Kind != restoreJobKind {
		return fmt.Errorf("%w: Job %s has kind %q", ErrInvalidKind, jobID, job.Kind)
	}
	if job.Status != StatusRunning && job.Status != StatusWaitingOnAgent {
		return fmt.Errorf("restore progress cannot update Job in %s status", job.Status)
	}
	if progress.TotalBytes <= 0 || progress.CurrentBytes < job.ProgressCurrent || progress.CurrentBytes > progress.TotalBytes ||
		(job.ProgressTotal != 0 && job.ProgressTotal != progress.TotalBytes) {
		return ErrInvalidProgress
	}
	return service.repository.UpdateProgress(ctx, jobID, job.ProgressCurrent, job.ProgressTotal,
		progress.CurrentBytes, progress.TotalBytes, string(progress.Phase), time.Now().UTC())
}

func (service *Service) restorePhaseError(jobID string, status Status, code string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	job, err := service.repository.Get(ctx, jobID)
	cancel()
	if err != nil {
		cause = errors.Join(cause, fmt.Errorf("load Job while recording restore phase result: %w", err))
	} else if job.Kind == KindPlay {
		if err := service.returnRestoreJobToRunning(jobID); err != nil {
			cause = errors.Join(cause, fmt.Errorf("restore phase could not return PLAY Job to running: %w", err))
		}
	}
	return &restorePhaseFailure{status: status, code: code, cause: cause}
}

func (service *Service) returnRestoreJobToRunning(jobID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	job, err := service.repository.Get(ctx, jobID)
	if err != nil {
		return err
	}
	if job.Status == StatusRunning {
		return nil
	}
	if job.Status != StatusWaitingOnAgent {
		return fmt.Errorf("%w: cannot return restore phase Job from %s to running", ErrInvalidTransition, job.Status)
	}
	return service.repository.Transition(ctx, jobID, StatusWaitingOnAgent, StatusRunning, time.Now().UTC())
}

func (service *Service) failRestore(jobID string, next Status, code string, cause error) error {
	// Record the terminal result even when the restore's caller context was canceled.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	job, err := service.repository.Get(ctx, jobID)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("load Job while recording restore outcome: %w", err))
	}
	if !allowedTransition(job.Status, next) {
		return errors.Join(cause, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, job.Status, next))
	}
	if err := service.repository.TransitionWithError(ctx, jobID, job.Status, next, code,
		"restore stopped before verified final-cache readiness", time.Now().UTC()); err != nil {
		return errors.Join(cause, fmt.Errorf("record restore Job outcome: %w", err))
	}
	return cause
}
