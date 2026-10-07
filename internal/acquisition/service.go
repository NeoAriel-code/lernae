package acquisition

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"

	"lernae/internal/domain"
	"lernae/internal/jobs"
)

type JobReader interface {
	GetAcquisition(context.Context, string) (jobs.Job, error)
}

type snapshot struct {
	expiresAt  time.Time
	sequence   uint64
	candidates []Candidate
}

type Service struct {
	jobs       JobReader
	repository Repository
	registry   *Registry
	timeout    time.Duration
	capacity   int
	now        func() time.Time
	mu         sync.Mutex
	sequence   uint64
	snapshots  map[string]snapshot
}

func NewService(jobReader JobReader, repository Repository, registry *Registry) *Service {
	return &Service{
		jobs: jobReader, repository: repository, registry: registry,
		timeout: providerTimeout, capacity: maxSnapshots, now: time.Now,
		snapshots: make(map[string]snapshot),
	}
}

func (s *Service) acquisition(ctx context.Context, jobID string) (jobs.Job, error) {
	if ctx.Err() != nil {
		return jobs.Job{}, ErrCancelled
	}
	if !localIDPattern.MatchString(jobID) {
		return jobs.Job{}, ErrInvalidID
	}
	if s.jobs == nil || s.repository == nil {
		return jobs.Job{}, ErrUnavailable
	}
	job, err := s.jobs.GetAcquisition(ctx, jobID)
	if errors.Is(err, jobs.ErrNotFound) {
		return jobs.Job{}, ErrNotFound
	}
	if err != nil {
		return jobs.Job{}, safeRepositoryError(ctx)
	}
	if job.ID != jobID || job.Kind != jobs.KindAcquire {
		return jobs.Job{}, ErrNotFound
	}
	return job, nil
}

func (s *Service) Discover(ctx context.Context, jobID string) (Discovery, error) {
	job, err := s.acquisition(ctx, jobID)
	if err != nil {
		return Discovery{}, err
	}
	if job.Status != jobs.StatusQueued {
		return Discovery{}, ErrNotQueued
	}
	target, err := s.repository.Target(ctx, domain.EditionID(job.TargetEditionID))
	if err != nil {
		return Discovery{}, err
	}
	result := Discovery{
		JobID: job.ID, EditionID: target.Edition.ID, Outcome: OutcomeUnconfigured,
		Candidates: make([]Candidate, 0), Failures: make([]Failure, 0),
	}
	var eligible []registeredProvider
	if s.registry != nil && len(s.registry.entries) > 0 {
		result.Outcome = OutcomeIneligible
		for _, entry := range s.registry.entries {
			if entry.provider.Eligible(target) {
				eligible = append(eligible, entry)
			}
		}
	}
	// Each eligible provider gets an independent timeout starting with its call.
	// Join all workers: no timeout wrapper returns leaving orphan goroutines.
	// Providers must honor cancellation as required by the contract.
	type providerResult struct {
		options []Option
		failure Error
	}
	results := make([]providerResult, len(eligible))
	var wait sync.WaitGroup
	for i, entry := range eligible {
		wait.Add(1)
		go func(i int, entry registeredProvider) {
			defer wait.Done()
			budget := contentVerificationPolicy(entry.provider, s.timeout).Discovery
			providerCtx, cancel := context.WithTimeout(ctx, budget)
			defer cancel()
			options, providerErr := entry.provider.Discover(providerCtx, target)
			switch {
			case providerCtx.Err() != nil:
				results[i].failure = ErrTimeout
			case providerErr != nil:
				// Preserve only direct allowlisted categories. Do not unwrap raw
				// provider errors: wrappers may carry sensitive upstream details.
				results[i].failure = ErrProvider
				if category, ok := providerErr.(Error); ok {
					switch category {
					case ErrInvalidResponse, ErrTimeout, ErrProvider:
						results[i].failure = category
					}
				}
			case !validOptions(options):
				results[i].failure = ErrInvalidResponse
			default:
				// Copy provider-owned results before sorting or retaining them.
				results[i].options = append([]Option(nil), options...)
			}
		}(i, entry)
	}
	wait.Wait()
	if ctx.Err() != nil {
		return Discovery{}, ErrCancelled
	}
	for i, entry := range eligible {
		if results[i].failure != "" {
			result.Failures = append(result.Failures, Failure{ProviderID: entry.id, Category: results[i].failure})
			continue
		}
		options := results[i].options
		sort.Slice(options, func(i, j int) bool { return options[i].ID < options[j].ID })
		for _, option := range options {
			handle, err := newHandle()
			if err != nil {
				return Discovery{}, ErrUnavailable
			}
			result.Candidates = append(result.Candidates, Candidate{
				JobID: job.ID, EditionID: target.Edition.ID, ProviderID: entry.id,
				Handle: handle, Option: option,
			})
		}
	}
	if len(eligible) > 0 {
		switch {
		case len(result.Failures) == len(eligible):
			result.Outcome = OutcomeFailed
		case len(result.Failures) > 0:
			result.Outcome = OutcomePartial
		case len(result.Candidates) == 0:
			result.Outcome = OutcomeEmpty
		default:
			result.Outcome = OutcomeAvailable
		}
	}
	if ctx.Err() != nil {
		return Discovery{}, ErrCancelled
	}
	s.storeSnapshot(job.ID, result.Candidates)
	return result, nil
}

func validOptions(options []Option) bool {
	if len(options) > MaxProviderCandidates {
		return false
	}
	seen := make(map[string]bool)
	for _, option := range options {
		if !validOption(option) || seen[option.ID] {
			return false
		}
		seen[option.ID] = true
	}
	return true
}

func newHandle() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (s *Service) storeSnapshot(jobID string, candidates []Candidate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for id, entry := range s.snapshots {
		if !now.Before(entry.expiresAt) {
			delete(s.snapshots, id)
		}
	}
	if _, exists := s.snapshots[jobID]; !exists && len(s.snapshots) >= s.capacity {
		var oldestID string
		var oldestSequence uint64
		for id, entry := range s.snapshots {
			if oldestID == "" || entry.sequence < oldestSequence {
				oldestID, oldestSequence = id, entry.sequence
			}
		}
		delete(s.snapshots, oldestID)
	}
	s.sequence++
	s.snapshots[jobID] = snapshot{
		expiresAt: now.Add(snapshotTTL), sequence: s.sequence,
		candidates: append([]Candidate(nil), candidates...),
	}
}

func (s *Service) Select(ctx context.Context, jobID, handle string) (Selection, error) {
	job, err := s.acquisition(ctx, jobID)
	if err != nil {
		return Selection{}, err
	}
	if !handlePattern.MatchString(handle) {
		return Selection{}, ErrInvalidID
	}
	stored, err := s.repository.Get(ctx, jobID)
	if err == nil {
		if stored.Candidate.Handle != handle {
			return Selection{}, ErrConflict
		}
		return stored, nil // read-only retry works after restart or execution
	}
	if !errors.Is(err, ErrNoSelection) {
		return Selection{}, err
	}
	if job.Status != jobs.StatusQueued {
		return Selection{}, ErrNotQueued
	}
	s.mu.Lock()
	entry, exists := s.snapshots[jobID]
	if !exists || !s.now().Before(entry.expiresAt) {
		delete(s.snapshots, jobID)
		s.mu.Unlock()
		return Selection{}, ErrSnapshotExpired
	}
	var chosen Candidate
	for _, candidate := range entry.candidates {
		if candidate.Handle == handle {
			chosen = candidate
			break
		}
	}
	s.mu.Unlock()
	if chosen.Handle == "" {
		return Selection{}, ErrUnknownCandidate
	}
	if ctx.Err() != nil {
		return Selection{}, ErrCancelled
	}
	return s.repository.Accept(ctx, Selection{Candidate: chosen, SelectedAt: s.now().UTC()})
}

func (s *Service) GetSelection(ctx context.Context, jobID string) (Selection, error) {
	if _, err := s.acquisition(ctx, jobID); err != nil {
		return Selection{}, err
	}
	return s.repository.Get(ctx, jobID)
}
