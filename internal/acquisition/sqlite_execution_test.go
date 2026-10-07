package acquisition

import (
	"context"
	"errors"
	"strings"
	"testing"

	"lernae/internal/jobs"
)

func TestReservationExactGuardsAndPermanentProvenance(t *testing.T) {
	jobService, db, _, job, selected := selectedFixture(t)
	ctx := context.Background()
	repository := NewSQLiteRepository(db)
	for _, name := range []string{"ref", "candidate", "provider", "handle", "edition", "metadata", "timestamp"} {
		changed := selected
		switch name {
		case "ref":
			changed.Candidate.Option.ExecutionRef = "different"
		case "candidate":
			changed.Candidate.Option.ID = "different"
		case "provider":
			changed.Candidate.ProviderID = "different"
		case "handle":
			changed.Candidate.Handle = strings.Repeat("b", 64)
		case "edition":
			changed.Candidate.EditionID = "different"
		case "metadata":
			changed.Candidate.Option.Metadata.Title = "Different title"
		case "timestamp":
			changed.SelectedAt = changed.SelectedAt.Add(1)
		}
		if _, won, err := repository.Reserve(ctx, changed, strings.Repeat("a", 64)); won || err == nil {
			t.Fatalf("changed %s granted dispatch: %t, %v", name, won, err)
		}
	}
	reservation, won, err := repository.Reserve(ctx, selected, strings.Repeat("a", 64))
	if err != nil || !won {
		t.Fatalf("exact reserve = %t, %v", won, err)
	}
	for _, query := range []string{
		`UPDATE acquisition_executions SET execution_id = '` + strings.Repeat("b", 64) + `'`,
		`UPDATE acquisition_executions SET reserved_at_utc = '2026-01-01T00:00:00.000000000Z'`,
		`DELETE FROM acquisition_executions`,
		`UPDATE acquisition_executions SET dispatch_outcome = 'accepted'`,
	} {
		if _, err := db.ExecContext(ctx, query); err == nil {
			t.Fatalf("SQL modified permanent/invalid provenance: %s", query)
		}
	}
	if err := repository.Decide(ctx, reservation, DispatchRejected); err != nil {
		t.Fatal(err)
	}
	if err := repository.Decide(ctx, reservation, DispatchAccepted); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("decision rewritten: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE acquisition_executions
		SET dispatch_outcome = 'reserved', decided_at_utc = NULL`); err == nil {
		t.Fatal("SQL released permanent reservation")
	}
	retry, won, err := repository.Reserve(ctx, selected, strings.Repeat("b", 64))
	if err != nil || won || retry.ExecutionID != reservation.ExecutionID || retry.Outcome != DispatchRejected {
		t.Fatalf("reservation retry = %#v, %t, %v", retry, won, err)
	}
	if err := jobService.Transition(ctx, job.ID, jobs.StatusCancelled); err != nil {
		t.Fatal(err)
	}
	if _, won, err := repository.Reserve(ctx, selected, strings.Repeat("c", 64)); err != nil || won {
		t.Fatalf("terminal retry granted authority: %t, %v", won, err)
	}
}

func TestExecutionRestartPreservesUnreservedSelections(t *testing.T) {
	jobService, db, _, job, selected := selectedFixture(t)
	if count, err := jobService.ReconcileStartup(context.Background()); count != 0 || err != nil {
		t.Fatalf("startup changed unreserved selection: %d, %v", count, err)
	}
	loaded, err := NewSQLiteRepository(db).Get(context.Background(), job.ID)
	if err != nil || loaded != selected {
		t.Fatal("startup replaced selection")
	}
	job, err = jobService.GetAcquisition(context.Background(), job.ID)
	if err != nil || job.Status != jobs.StatusQueued {
		t.Fatalf("startup executed unreserved selection: %s, %v", job.Status, err)
	}
}

// Inject persistence faults, not a fake production executor. The real SQLite
// reservation proves that uncertain persistence never enables another call.
type faultyExecutionRepository struct {
	*SQLiteRepository
	loseReserveConfirmation bool
	failDecision            bool
}

func (r faultyExecutionRepository) Reserve(ctx context.Context, selection Selection, identity string) (Reservation, bool, error) {
	reservation, won, err := r.SQLiteRepository.Reserve(ctx, selection, identity)
	if r.loseReserveConfirmation && err == nil && won {
		return Reservation{}, false, ErrUnavailable
	}
	return reservation, won, err
}

func (r faultyExecutionRepository) Decide(ctx context.Context, reservation Reservation, outcome DispatchOutcome) error {
	if r.failDecision {
		return ErrUnavailable
	}
	return r.SQLiteRepository.Decide(ctx, reservation, outcome)
}

func TestExecutionPersistenceWindowsFailClosed(t *testing.T) {
	for _, mode := range []string{"claim confirmation lost", "accepted decision write lost"} {
		t.Run(mode, func(t *testing.T) {
			jobService, db, _, job, _ := selectedFixture(t)
			repository := faultyExecutionRepository{
				SQLiteRepository: NewSQLiteRepository(db), loseReserveConfirmation: mode == "claim confirmation lost",
				failDecision: mode == "accepted decision write lost",
			}
			drained := false
			executor := &testExecutor{dispatch: func(context.Context, ExecutionPlan) (DispatchResult, error) {
				return DispatchResult{Decision: DispatchAccepted, Execution: testExecution{wait: func(ctx context.Context, _ func(Progress) error) (Completion, error) {
					drained = ctx.Err() != nil
					return CompletionInterrupted, ctx.Err()
				}}}, nil
			}}
			core := NewExecutionService(context.Background(), jobService, repository, executionRegistry(t, testResolver{}), executor)
			defer core.Close()
			if _, err := core.Execute(context.Background(), job.ID); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("write uncertainty = %v", err)
			}
			wantCalls := int32(0)
			if mode == "accepted decision write lost" {
				wantCalls = 1
				if !drained {
					t.Fatal("known accepted handle was not cancelled/joined")
				}
			}
			if executor.calls.Load() != wantCalls {
				t.Fatalf("dispatches = %d", executor.calls.Load())
			}
			if _, err := jobService.ReconcileStartup(context.Background()); err != nil {
				t.Fatal(err)
			}
			retry, err := core.Execute(context.Background(), job.ID)
			if err != nil || retry.Outcome != DispatchUnconfirmed || executor.calls.Load() != wantCalls {
				t.Fatalf("uncertainty replay = %#v, %v", retry, err)
			}
			loaded, _ := jobService.GetAcquisition(context.Background(), job.ID)
			if loaded.Status != jobs.StatusFailed {
				t.Fatalf("ambiguous queued Job = %s", loaded.Status)
			}
		})
	}
}
