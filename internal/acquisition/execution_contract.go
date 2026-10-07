package acquisition

import (
	"context"
	"reflect"

	"lernae/internal/domain"
)

const (
	ErrExecutionProvider Error = "execution_provider_unavailable"
	ErrExecutor          Error = "executor_unavailable"
	ErrUnresolved        Error = "selection_unresolvable"
)

// ExecutionPlan is exact identity/provenance, not a universal download protocol.
// The private token still addresses provider-owned state. No arbitrary payload,
// URL, credentials, or replacement candidate is accepted by this boundary.
type ExecutionPlan struct {
	ExecutionID  string
	JobID        string
	EditionID    domain.EditionID
	ProviderID   string
	CandidateID  string
	Handle       string
	ExecutionRef string `json:"-"`
}

func PlanForSelection(selection Selection) ExecutionPlan {
	candidate := selection.Candidate
	return ExecutionPlan{
		JobID: candidate.JobID, EditionID: candidate.EditionID,
		ProviderID: candidate.ProviderID, CandidateID: candidate.Option.ID,
		Handle: candidate.Handle, ExecutionRef: candidate.Option.ExecutionRef,
	}
}

// ExecutionResolver is deliberately separate from discovery. Resolve must be
// side-effect-free, honor context, and resolve only the supplied durable choice.
// It may reject expired/missing provider state, never rediscover or substitute.
type ExecutionResolver interface {
	ID() string
	Resolve(context.Context, Selection) (ExecutionPlan, error)
}

// ExecutionRegistry freezes typed resolution capabilities at composition.
// Discovery providers cannot implicitly gain execution authority.
type ExecutionRegistry struct {
	resolvers map[string]ExecutionResolver
}

func NewExecutionRegistry(resolvers ...ExecutionResolver) (*ExecutionRegistry, error) {
	if len(resolvers) > MaxProviders {
		return nil, ErrRegistry
	}
	registry := &ExecutionRegistry{resolvers: make(map[string]ExecutionResolver)}
	for _, resolver := range resolvers {
		if nilCapability(resolver) {
			return nil, ErrRegistry
		}
		id := resolver.ID()
		if !providerIDPattern.MatchString(id) || registry.resolvers[id] != nil {
			return nil, ErrRegistry
		}
		registry.resolvers[id] = resolver
	}
	return registry, nil
}

func nilCapability(value interface{}) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

type DispatchOutcome string

const (
	DispatchReserved    DispatchOutcome = "reserved"
	DispatchAccepted    DispatchOutcome = "accepted"
	DispatchRejected    DispatchOutcome = "rejected"
	DispatchUnconfirmed DispatchOutcome = "unconfirmed"
)

// Executor gets one call per durable reservation. Accepted means a REAL
// execution boundary accepted responsibility, not successful preflight. Only a
// rejected result with nil error guarantees no acceptance/effects. Any error,
// timeout, or malformed result is ambiguous and permanently consumes the claim.
// Implementations must honor context and join dispatch work before returning.
// The Dispatch context bounds only acceptance, not the lifetime of accepted
// work: its Execution handle is subsequently owned/cancelled through Wait.
// Future effectful adapters must use ExecutionID for executor-side idempotency;
// the core alone promises at-most-one dispatch, NOT exactly-once effects.
type Executor interface {
	Dispatch(context.Context, ExecutionPlan) (DispatchResult, error)
}

type DispatchResult struct {
	Decision  DispatchOutcome
	Execution Execution
}

// Execution owns accepted work. Wait must join it, honor cancellation, invoke
// progress serially, and return success only with adapter-verified completion.
// No productive adapter is supplied here; this seam does not invent Agent RPCs.
type Execution interface {
	Wait(context.Context, func(Progress) error) (Completion, error)
}

type Progress struct {
	Current int64
	Total   int64
}

type Completion string

const (
	CompletionSucceeded   Completion = "succeeded"
	CompletionFailed      Completion = "failed"
	CompletionInterrupted Completion = "interrupted"
)

// Reservation is dispatch provenance only; Jobs remains the sole lifecycle.
// It contains no duplicate private reference or provider payload.
type Reservation struct {
	JobID       string
	ExecutionID string
	Outcome     DispatchOutcome
}

type ExecutionRepository interface {
	Get(context.Context, string) (Selection, error)
	GetReservation(context.Context, string) (Reservation, bool, error)
	Reserve(context.Context, Selection, string) (Reservation, bool, error)
	Decide(context.Context, Reservation, DispatchOutcome) error
}
