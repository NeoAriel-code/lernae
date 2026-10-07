package acquisition

import (
	"context"
	"errors"
	"sync"
	"time"

	"lernae/internal/jobs"
)

// ExecutionService owns dispatch and accepted workers independently of HTTP
// cancellation. Close stops admission, cancels owned work and joins it before
// SQLite can close. Resolution alone uses the caller's context.
type ExecutionService struct {
	jobs       *jobs.Service
	repository ExecutionRepository
	registry   *ExecutionRegistry
	executor   Executor
	ctx        context.Context
	cancel     context.CancelFunc
	timeout    time.Duration
	mu         sync.Mutex
	closed     bool
	workers    sync.WaitGroup
}

func NewExecutionService(parent context.Context, jobService *jobs.Service, repository ExecutionRepository, registry *ExecutionRegistry, executor Executor) *ExecutionService {
	ctx, cancel := context.WithCancel(parent)
	return &ExecutionService{
		jobs: jobService, repository: repository, registry: registry, executor: executor,
		ctx: ctx, cancel: cancel, timeout: providerTimeout,
	}
}

func (s *ExecutionService) Close() {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	s.workers.Wait()
}

// Wait is for callers that have already stopped admission; Close also gates it.
func (s *ExecutionService) Wait() { s.workers.Wait() }

func (s *ExecutionService) Execute(ctx context.Context, jobID string) (Reservation, error) {
	s.mu.Lock()
	if s.closed || s.ctx.Err() != nil {
		s.mu.Unlock()
		return Reservation{}, ErrUnavailable
	}
	s.workers.Add(1)
	s.mu.Unlock()
	defer s.workers.Done()
	if ctx.Err() != nil {
		return Reservation{}, ErrCancelled
	}
	if !localIDPattern.MatchString(jobID) {
		return Reservation{}, ErrInvalidID
	}
	if s.jobs == nil || nilCapability(s.repository) {
		return Reservation{}, ErrUnavailable
	}
	job, err := s.jobs.GetAcquisition(ctx, jobID)
	if errors.Is(err, jobs.ErrNotFound) {
		return Reservation{}, ErrNotFound
	}
	if err != nil {
		return Reservation{}, safeRepositoryError(ctx)
	}
	// Retrying is a durable read, including terminal Jobs and missing adapters.
	stored, exists, err := s.repository.GetReservation(ctx, jobID)
	if err != nil || exists {
		return stored, err
	}
	if job.Status != jobs.StatusQueued {
		return Reservation{}, ErrNotQueued
	}
	selection, err := s.repository.Get(ctx, jobID)
	if err != nil {
		return Reservation{}, err
	}
	if s.registry == nil || s.registry.resolvers[selection.Candidate.ProviderID] == nil {
		return Reservation{}, ErrExecutionProvider
	}
	if nilCapability(s.executor) {
		return Reservation{}, ErrExecutor
	}
	resolver := s.registry.resolvers[selection.Candidate.ProviderID]
	resolveBudget := contentVerificationPolicy(resolver, s.timeout).Resolution
	resolveCtx, cancelResolve := context.WithTimeout(ctx, resolveBudget)
	// Shutdown also cancels in-flight preflight; no claim is needed to stop it.
	stop := context.AfterFunc(s.ctx, cancelResolve)
	plan, resolveErr := resolver.Resolve(resolveCtx, selection)
	resolveCancelled := resolveCtx.Err() != nil
	stop()
	cancelResolve()
	if ctx.Err() != nil {
		return Reservation{}, ErrCancelled
	}
	if s.ctx.Err() != nil {
		return Reservation{}, ErrUnavailable
	}
	if resolveCancelled {
		return Reservation{}, ErrTimeout
	}
	if resolveErr != nil || plan != PlanForSelection(selection) {
		return Reservation{}, ErrUnresolved
	}
	identity, err := newHandle()
	if err != nil {
		return Reservation{}, ErrUnavailable
	}
	reservation, won, err := s.repository.Reserve(ctx, selection, identity)
	if err != nil || !won {
		return reservation, err
	}
	plan.ExecutionID = reservation.ExecutionID
	// From this point caller cancellation cannot release the claim or suppress
	// persistence. Even claimed-before-call crashes consume this identity.
	dispatchBudget := contentVerificationPolicy(s.executor, s.timeout).Dispatch
	dispatchCtx, cancelDispatch := context.WithTimeout(s.ctx, dispatchBudget)
	result, dispatchErr := s.executor.Dispatch(dispatchCtx, plan)
	cancelDispatch()
	switch {
	case dispatchErr == nil && result.Decision == DispatchRejected && nilCapability(result.Execution):
		reservation.Outcome = DispatchRejected
		if err := s.decide(reservation); err != nil {
			return reservation, err
		}
		return reservation, nil // explicit rejection remains queued, permanently claimed
	case dispatchErr == nil && result.Decision == DispatchAccepted && !nilCapability(result.Execution):
		reservation.Outcome = DispatchAccepted
		if err := s.decide(reservation); err != nil {
			s.drain(result.Execution)
			_ = s.terminalize(jobID, jobs.StatusFailed)
			return reservation, err
		}
		persistCtx, cancel := persistenceContext()
		err := s.jobs.Transition(persistCtx, jobID, jobs.StatusRunning)
		cancel()
		if err != nil {
			s.drain(result.Execution)
			_ = s.terminalize(jobID, jobs.StatusFailed)
			return reservation, ErrUnavailable
		}
		s.workers.Add(1) // Execute is still counted; Close cannot race a zero count.
		go func() {
			defer s.workers.Done()
			s.run(jobID, result.Execution)
		}()
		return reservation, nil
	default:
		reservation.Outcome = DispatchUnconfirmed
		decisionErr := s.decide(reservation)
		if !nilCapability(result.Execution) {
			s.drain(result.Execution)
		}
		terminalErr := s.terminalize(jobID, jobs.StatusFailed)
		if decisionErr != nil || terminalErr != nil {
			return reservation, ErrUnavailable
		}
		return reservation, nil
	}
}

func persistenceContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Second)
}

func (s *ExecutionService) decide(reservation Reservation) error {
	ctx, cancel := persistenceContext()
	defer cancel()
	return s.repository.Decide(ctx, reservation, reservation.Outcome)
}

// Drain a contradictory or unpersistable accepted handle without using the
// canceled HTTP context. Cooperative adapter joining is part of the contract.
func (s *ExecutionService) drain(execution Execution) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = execution.Wait(ctx, func(Progress) error { return ErrCancelled })
}

func (s *ExecutionService) run(jobID string, execution Execution) {
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	var progressErr error
	completion, err := execution.Wait(ctx, func(progress Progress) error {
		if progressErr != nil {
			return progressErr
		}
		progressErr = s.jobs.UpdateAcquisitionProgress(ctx, jobID, progress.Current, progress.Total)
		if progressErr != nil {
			cancel()
		}
		return progressErr
	})
	next := jobs.StatusFailed
	switch {
	case progressErr != nil:
		// Shutdown can cancel a valid progress write. Other persistence or
		// validation errors still fail, even though they also cancel observation.
		if errors.Is(progressErr, context.Canceled) || errors.Is(progressErr, context.DeadlineExceeded) {
			next = jobs.StatusInterrupted
		}
	case err == nil && completion == CompletionSucceeded:
		next = jobs.StatusSucceeded // confirmed completion wins a late cancellation
	case ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || completion == CompletionInterrupted:
		next = jobs.StatusInterrupted
	}
	// Persistence errors leave a permanent reservation and are recoverable only
	// by startup reconciliation; they never enable replay of external effects.
	_ = s.terminalize(jobID, next)
}

func (s *ExecutionService) terminalize(jobID string, next jobs.Status) error {
	ctx, cancel := persistenceContext()
	defer cancel()
	if next == jobs.StatusSucceeded {
		return s.jobs.Transition(ctx, jobID, next)
	}
	return s.jobs.TransitionWithError(ctx, jobID, next, "acquisition_"+string(next), "")
}
