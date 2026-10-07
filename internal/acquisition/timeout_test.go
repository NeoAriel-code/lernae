package acquisition

import (
	"context"
	"errors"
	"testing"
	"time"
)

type verificationProvider struct {
	testProvider
	policy ContentVerificationPolicy
}

func (p verificationProvider) ContentVerificationTimeouts() ContentVerificationPolicy {
	return p.policy
}

type verificationResolver struct {
	testResolver
	policy ContentVerificationPolicy
}

func (r verificationResolver) ContentVerificationTimeouts() ContentVerificationPolicy {
	return r.policy
}

type verificationExecutor struct {
	*testExecutor
	policy ContentVerificationPolicy
}

func (e verificationExecutor) ContentVerificationTimeouts() ContentVerificationPolicy {
	return e.policy
}

func assertVerificationBudget(t *testing.T, ctx context.Context, want time.Duration) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining <= want-time.Second || remaining > want {
		t.Errorf("deadline remaining=%s, want bounded %s", remaining, want)
	}
}

func TestContentVerificationPolicyBoundsAndDefaults(t *testing.T) {
	defaults := ContentVerificationPolicy{Discovery: providerTimeout, Resolution: providerTimeout, Dispatch: providerTimeout}
	if got := contentVerificationPolicy(testResolver{}, providerTimeout); got != defaults {
		t.Fatal("generic capability defaults changed", got)
	}
	for _, policy := range []ContentVerificationPolicy{
		{},
		{Discovery: 2 * time.Second},
		{Resolution: 10 * time.Minute},
		{Discovery: 3 * time.Second, Resolution: 4 * time.Second, Dispatch: 5 * time.Second},
	} {
		got := contentVerificationPolicy(verificationResolver{policy: policy}, providerTimeout)
		want := policy
		if want.Discovery == 0 {
			want.Discovery = providerTimeout
		}
		if want.Resolution == 0 {
			want.Resolution = providerTimeout
		}
		if want.Dispatch == 0 {
			want.Dispatch = providerTimeout
		}
		if got != want {
			t.Errorf("valid/zero policy=%v, got=%v want=%v", policy, got, want)
		}
	}
	for _, invalid := range []time.Duration{-time.Second, time.Nanosecond, providerTimeout - 1, 10*time.Minute + 1, time.Duration(1<<63 - 1)} {
		for _, field := range []string{"discovery", "resolution", "dispatch"} {
			policy := ContentVerificationPolicy{Discovery: 3 * time.Second, Resolution: 4 * time.Second, Dispatch: 5 * time.Second}
			switch field {
			case "discovery":
				policy.Discovery = invalid
			case "resolution":
				policy.Resolution = invalid
			case "dispatch":
				policy.Dispatch = invalid
			}
			if got := contentVerificationPolicy(verificationResolver{policy: policy}, providerTimeout); got != defaults {
				t.Errorf("invalid %s budget=%s did not fall back: %v", field, invalid, got)
			}
		}
	}
}

func TestDiscoveryVerificationPolicyAndCallerDeadline(t *testing.T) {
	for _, mode := range []string{"generic", "zero", "typed", "invalid", "caller deadline", "caller cancellation"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callerDeadline time.Time
			if mode == "caller deadline" {
				callerDeadline = time.Now().Add(time.Second)
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(ctx, callerDeadline)
				defer stop()
			}
			plain := provider("source", option("exact"))
			plain.discover = func(callCtx context.Context, _ Target) ([]Option, error) {
				want := providerTimeout
				if mode == "typed" || mode == "caller cancellation" {
					want = 5 * time.Minute
				}
				if mode == "caller deadline" {
					deadline, ok := callCtx.Deadline()
					if !ok || deadline != callerDeadline {
						t.Error("typed policy extended caller deadline")
					}
				} else {
					assertVerificationBudget(t, callCtx, want)
				}
				if mode == "caller cancellation" {
					cancel()
					<-callCtx.Done()
					return nil, callCtx.Err()
				}
				return []Option{option("exact")}, nil
			}
			var capability Provider = plain
			policy := ContentVerificationPolicy{Discovery: 5 * time.Minute}
			if mode == "zero" {
				policy = ContentVerificationPolicy{}
			}
			if mode == "invalid" {
				policy.Discovery = 11 * time.Minute
			}
			if mode != "generic" {
				capability = verificationProvider{testProvider: plain, policy: policy}
			}
			service, _, _, _, job := fixture(t, capability)
			found, err := service.Discover(ctx, job.ID)
			if mode == "caller cancellation" {
				if !errors.Is(err, ErrCancelled) {
					t.Fatal("caller cancellation lost", err)
				}
			} else if err != nil || len(found.Candidates) != 1 {
				t.Fatal("budgeted discovery failed", err)
			}
		})
	}
}

func TestExecutionVerificationPolicyPerCapabilityAndCancellation(t *testing.T) {
	for _, mode := range []string{"generic", "typed", "invalid", "caller deadline", "caller cancellation", "shutdown resolution", "shutdown dispatch", "caller cancellation after claim"} {
		t.Run(mode, func(t *testing.T) {
			jobService, db, _, job, _ := selectedFixture(t)
			ctx, cancelCaller := context.WithCancel(context.Background())
			defer cancelCaller()
			owner, cancelOwner := context.WithCancel(context.Background())
			defer cancelOwner()
			var callerDeadline time.Time
			if mode == "caller deadline" {
				callerDeadline = time.Now().Add(time.Second)
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(ctx, callerDeadline)
				defer stop()
			}
			policy := ContentVerificationPolicy{Discovery: 3 * time.Second, Resolution: 4 * time.Second, Dispatch: 5 * time.Second}
			if mode == "invalid" {
				policy.Dispatch = -time.Second
			}
			resolutionBudget, dispatchBudget := 4*time.Second, 5*time.Second
			if mode == "generic" || mode == "invalid" {
				resolutionBudget, dispatchBudget = providerTimeout, providerTimeout
			}
			resolver := testResolver{resolve: func(callCtx context.Context, selection Selection) (ExecutionPlan, error) {
				if mode == "caller deadline" {
					deadline, ok := callCtx.Deadline()
					if !ok || deadline != callerDeadline {
						t.Error("resolution extended caller deadline")
					}
				} else {
					assertVerificationBudget(t, callCtx, resolutionBudget)
				}
				if mode == "caller cancellation" || mode == "shutdown resolution" {
					if mode == "caller cancellation" {
						cancelCaller()
					} else {
						cancelOwner()
					}
					<-callCtx.Done()
					return ExecutionPlan{}, callCtx.Err()
				}
				return PlanForSelection(selection), nil
			}}
			executor := &testExecutor{dispatch: func(callCtx context.Context, _ ExecutionPlan) (DispatchResult, error) {
				assertVerificationBudget(t, callCtx, dispatchBudget)
				if mode == "shutdown dispatch" {
					cancelOwner()
					<-callCtx.Done()
					return DispatchResult{}, callCtx.Err()
				}
				if mode == "caller cancellation after claim" {
					cancelCaller()
					if callCtx.Err() != nil {
						t.Error("caller cancellation leaked into owned dispatch")
					}
				}
				return DispatchResult{Decision: DispatchAccepted, Execution: testExecution{}}, nil
			}}
			var resolveCapability ExecutionResolver = resolver
			var dispatchCapability Executor = executor
			if mode != "generic" {
				resolveCapability = verificationResolver{testResolver: resolver, policy: policy}
				dispatchCapability = verificationExecutor{testExecutor: executor, policy: policy}
			}
			registry, err := NewExecutionRegistry(resolveCapability)
			if err != nil {
				t.Fatal(err)
			}
			core := NewExecutionService(owner, jobService, NewSQLiteRepository(db), registry, dispatchCapability)
			defer core.Close()
			reservation, err := core.Execute(ctx, job.ID)
			core.Wait()
			switch mode {
			case "caller cancellation", "shutdown resolution":
				want := ErrCancelled
				if mode == "shutdown resolution" {
					want = ErrUnavailable
				}
				if !errors.Is(err, want) || executor.calls.Load() != 0 {
					t.Fatal("preflight cancellation lost", err)
				}
				if _, exists, err := NewSQLiteRepository(db).GetReservation(context.Background(), job.ID); err != nil || exists {
					t.Fatal("cancelled preflight claimed execution", err)
				}
			case "shutdown dispatch":
				if err != nil || reservation.Outcome != DispatchUnconfirmed {
					t.Fatal("shutdown dispatch lost permanent receipt", err)
				}
			default:
				if err != nil || reservation.Outcome != DispatchAccepted {
					t.Fatal("execution failed", err)
				}
			}
		})
	}
}
