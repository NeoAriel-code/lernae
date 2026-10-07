package acquisition

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lernae/internal/database"

	"lernae/internal/jobs"
)

func TestQBittorrentCoreClaimAcceptanceConcurrencyAndRestart(t *testing.T) {
	for _, mode := range []string{"accepted", "running restart", "unconfirmed", "fetch fails", "preclaim protocol", "preclaim locator", "preclaim lost", "preclaim tamper", "preclaim instance", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			var fetches, adds, infos atomic.Int32
			var jobService *jobs.Service
			var db *sql.DB
			var jobID string
			var tagMu sync.Mutex
			tag := ""
			payload, _ := parseTorrent(testTorrent())
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/auth/login":
					http.SetCookie(w, &http.Cookie{Name: "SID", Value: "PRIVATE_COOKIE", Path: "/"})
					_, _ = io.WriteString(w, "Ok.")
				case "/api/v2/app/version":
					_, _ = io.WriteString(w, "v5.0.0")
				case "/api/v2/app/webapiVersion":
					_, _ = io.WriteString(w, "2.11.2")
				case "/api/v2/torrents/add":
					adds.Add(1)
					loaded, err := jobService.GetAcquisition(ctx, jobID)
					if err != nil || loaded.Status != jobs.StatusQueued {
						t.Error("Job running before real acceptance")
					}
					reader, err := r.MultipartReader()
					if err != nil {
						t.Error(err)
						return
					}
					for {
						part, err := reader.NextPart()
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Error(err)
							return
						}
						data, _ := io.ReadAll(part)
						if part.FormName() == "tags" {
							tagMu.Lock()
							tag = string(data)
							tagMu.Unlock()
						}
					}
					if mode == "unconfirmed" {
						_, _ = io.WriteString(w, "Fails.")
					} else {
						_, _ = io.WriteString(w, "Ok.")
					}
				case "/api/v2/torrents/info":
					call := infos.Add(1)
					if call == 1 {
						loaded, err := jobService.GetAcquisition(ctx, jobID)
						if err != nil || loaded.Status != jobs.StatusQueued {
							t.Error("Job running before exact correlation")
						}
					}
					tagMu.Lock()
					ownedTag := tag
					tagMu.Unlock()
					if r.URL.Query().Get("hashes") != payload.hash || r.URL.Query().Get("tag") != ownedTag {
						t.Error("unscoped observation")
					}
					info := qbTorrentInfo{Hash: payload.hash, Tags: ownedTag, State: "downloading", TotalSize: 4, Size: 4, AmountLeft: 4}
					if call > 1 && mode != "running restart" {
						info.State, info.Completed, info.AmountLeft, info.Progress = "stoppedUP", 4, 0, 1
					}
					_ = json.NewEncoder(w).Encode([]qbTorrentInfo{info})
				default:
					t.Error("unexpected remote command/cleanup/retry")
				}
			}))
			defer remote.Close()
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fetches.Add(1)
				if _, exists, err := NewSQLiteRepository(db).GetReservation(ctx, jobID); err != nil || !exists {
					t.Error("Option A fetch occurred before claim")
				}
				if mode == "fetch fails" {
					w.WriteHeader(503)
					return
				}
				_, _ = w.Write(testTorrent())
			}))
			defer proxy.Close()
			p := newTestProwlarr(t, proxy.URL)
			selected := storedProwlarrSelection(t, p, func(r *prowlarrRecord) {
				if mode == "preclaim protocol" {
					r.Protocol = ProwlarrProtocolUsenet
				}
				if mode == "preclaim locator" {
					r.Download.File = ""
				}
			})
			if mode == "preclaim lost" {
				selected.Candidate.Option.ExecutionRef = strings.Repeat("f", 64)
			}
			if mode == "preclaim tamper" {
				if err := os.WriteFile(p.config.ReferenceDirectory+"/"+selected.Candidate.Option.ExecutionRef, []byte("tamper"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			selectionService, service, database, _, job := fixture(t, provider(ProwlarrProviderID, selected.Candidate.Option))
			jobService, db, jobID = service, database, job.ID
			discovery, err := selectionService.Discover(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := selectionService.Select(ctx, job.ID, discovery.Candidates[0].Handle); err != nil {
				t.Fatal(err)
			}
			if fetches.Load() != 0 || adds.Load() != 0 {
				t.Fatal("selection executed")
			}
			if mode == "preclaim instance" {
				p.base.Path = "/other"
				p.instance = prowlarrHash("instance-v1", []byte(p.base.String()))
			}
			q, err := NewQBittorrent(QBittorrentConfig{Enabled: true, BaseURL: remote.URL, Username: "operator", Password: "PRIVATE_PASSWORD"}, p)
			if err != nil {
				t.Fatal(err)
			}
			q.pollInterval = time.Millisecond
			registry, _ := NewExecutionRegistry(NewProwlarrExecutionResolver(p))
			if mode == "disabled" {
				registry = nil // mirrors concrete composition's disabled capability gate
			}
			var executor Executor = q
			progressPersisted := make(chan struct{})
			if mode == "running restart" {
				var observed sync.Once
				executor = &testExecutor{dispatch: func(ctx context.Context, plan ExecutionPlan) (DispatchResult, error) {
					result, err := q.Dispatch(ctx, plan)
					if err == nil && result.Decision == DispatchAccepted && result.Execution != nil {
						remoteExecution := result.Execution
						result.Execution = testExecution{wait: func(ctx context.Context, report func(Progress) error) (Completion, error) {
							return remoteExecution.Wait(ctx, func(progress Progress) error {
								err := report(progress)
								if err == nil {
									observed.Do(func() { close(progressPersisted) })
								}
								return err
							})
						}}
					}
					return result, err
				}}
			}
			core := NewExecutionService(ctx, jobService, NewSQLiteRepository(db), registry, executor)
			defer core.Close()
			if strings.HasPrefix(mode, "preclaim") || mode == "disabled" {
				if _, err := core.Execute(ctx, job.ID); err == nil {
					t.Fatal("invalid preclaim selection accepted")
				}
				if _, exists, err := NewSQLiteRepository(db).GetReservation(ctx, job.ID); err != nil || exists || fetches.Load() != 0 || adds.Load() != 0 {
					t.Fatal("invalid selection reserved or did HTTP")
				}
				return
			}
			var peers sync.WaitGroup
			for i := 0; i < 8; i++ {
				peers.Add(1)
				go func() {
					defer peers.Done()
					if _, err := core.Execute(ctx, job.ID); err != nil {
						t.Error("concurrent execute failed")
					}
				}()
			}
			peers.Wait()
			if mode == "running restart" {
				select {
				case <-progressPersisted:
				case <-time.After(time.Second):
					core.Close()
					active, readErr := jobService.GetAcquisition(ctx, job.ID)
					t.Fatalf("observer handshake: read_error=%t status=%s progress=%d/%d fetches=%d adds=%d infos=%d", readErr != nil, active.Status, active.ProgressCurrent, active.ProgressTotal, fetches.Load(), adds.Load(), infos.Load())
				}
				active, readErr := jobService.GetAcquisition(ctx, job.ID)
				if readErr != nil || active.Status != jobs.StatusRunning || active.ProgressCurrent != 0 || active.ProgressTotal != 4 || infos.Load() < 2 {
					t.Fatalf("active observer: read_error=%t status=%s want=running progress=%d/%d want=0/4 infos=%d want>=2", readErr != nil, active.Status, active.ProgressCurrent, active.ProgressTotal, infos.Load())
				}
				t.Logf("before Close: read_error=%t status=%s progress=%d/%d fetches=%d adds=%d infos=%d", readErr != nil, active.Status, active.ProgressCurrent, active.ProgressTotal, fetches.Load(), adds.Load(), infos.Load())
				// Graceful shutdown, not a crash: Close joins and terminalizes the worker.
				// Persisted running crash recovery is tested separately below.
				core.Close() // stop observation; do not remove external torrent/files
			} else {
				core.Wait()
			}
			stored, exists, err := NewSQLiteRepository(db).GetReservation(ctx, job.ID)
			if err != nil || !exists || fetches.Load() != 1 {
				t.Fatalf("claim/fetch: read_error=%t exists=%t fetches=%d want=1", err != nil, exists, fetches.Load())
			}
			wantOutcome, wantStatus, wantAdds := DispatchAccepted, jobs.StatusSucceeded, int32(1)
			switch mode {
			case "unconfirmed":
				wantOutcome, wantStatus = DispatchUnconfirmed, jobs.StatusFailed
			case "fetch fails":
				wantOutcome, wantStatus, wantAdds = DispatchRejected, jobs.StatusQueued, 0
			case "running restart":
				wantStatus = jobs.StatusInterrupted
			}
			loaded, err := jobService.GetAcquisition(ctx, job.ID)
			if err != nil || stored.Outcome != wantOutcome || loaded.Status != wantStatus || adds.Load() != wantAdds {
				t.Fatalf("terminal: read_error=%t outcome=%s want=%s status=%s want=%s error_code=%s progress=%d/%d fetches=%d adds=%d want_adds=%d infos=%d", err != nil, stored.Outcome, wantOutcome, loaded.Status, wantStatus, loaded.ErrorCode, loaded.ProgressCurrent, loaded.ProgressTotal, fetches.Load(), adds.Load(), wantAdds, infos.Load())
			}
			core.Close()
			if count, err := jobService.ReconcileStartup(ctx); err != nil || count != 0 {
				t.Fatalf("clean startup: reconcile_error=%t count=%d want=0", err != nil, count)
			}
			before := infos.Load()
			restarted := NewExecutionService(ctx, jobService, NewSQLiteRepository(db), registry, q)
			defer restarted.Close()
			retry, err := restarted.Execute(ctx, job.ID)
			if err != nil || retry.ExecutionID != stored.ExecutionID || retry.Outcome != stored.Outcome || adds.Load() != wantAdds || fetches.Load() != 1 || infos.Load() != before {
				t.Fatalf("restart: execute_error=%t same_identity=%t outcome=%s want=%s fetches=%d want=1 adds=%d want=%d infos=%d want=%d", err != nil, retry.ExecutionID == stored.ExecutionID, retry.Outcome, stored.Outcome, fetches.Load(), adds.Load(), wantAdds, infos.Load(), before)
			}
			loaded, err = jobService.GetAcquisition(ctx, job.ID)
			if err != nil || loaded.Status != wantStatus {
				t.Fatalf("restarted Job: read_error=%t status=%s want=%s", err != nil, loaded.Status, wantStatus)
			}
		})
	}
}

type testResolver struct {
	resolve func(context.Context, Selection) (ExecutionPlan, error)
}

func (testResolver) ID() string { return "source" }
func (r testResolver) Resolve(ctx context.Context, selection Selection) (ExecutionPlan, error) {
	if r.resolve != nil {
		return r.resolve(ctx, selection)
	}
	return PlanForSelection(selection), nil
}

type testExecutor struct {
	dispatch func(context.Context, ExecutionPlan) (DispatchResult, error)
	calls    atomic.Int32
}

func (e *testExecutor) Dispatch(ctx context.Context, plan ExecutionPlan) (DispatchResult, error) {
	e.calls.Add(1)
	return e.dispatch(ctx, plan)
}

func TestExecutionExactSelectionAcceptanceAndRetry(t *testing.T) {
	selectionService, jobService, db, _, job := fixture(t, provider("source", option("exact"), option("other")))
	ctx := context.Background()
	discovery, err := selectionService.Discover(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := selectionService.Select(ctx, job.ID, discovery.Candidates[0].Handle)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewExecutionRegistry(testResolver{})
	if err != nil {
		t.Fatal(err)
	}
	executor := &testExecutor{dispatch: func(_ context.Context, plan ExecutionPlan) (DispatchResult, error) {
		want := PlanForSelection(selected)
		want.ExecutionID = plan.ExecutionID
		if plan != want || plan.ExecutionID == "" {
			t.Errorf("dispatch did not preserve exact selection provenance")
		}
		loaded, err := jobService.GetAcquisition(ctx, job.ID)
		if err != nil || loaded.Status != jobs.StatusQueued {
			t.Errorf("Job at acceptance = %s, %v; must still be queued", loaded.Status, err)
		}
		return DispatchResult{Decision: DispatchAccepted, Execution: testExecution{}}, nil
	}}
	core := NewExecutionService(ctx, jobService, NewSQLiteRepository(db), registry, executor)
	defer core.Close()
	first, err := core.Execute(ctx, job.ID)
	if err != nil || first.Outcome != DispatchAccepted {
		t.Fatalf("execute = %#v, %v", first, err)
	}
	core.Wait()
	retry, err := core.Execute(ctx, job.ID)
	if err != nil || retry.ExecutionID != first.ExecutionID || executor.calls.Load() != 1 {
		t.Fatalf("retry = %#v, %v; dispatches %d", retry, err, executor.calls.Load())
	}
	loaded, err := jobService.GetAcquisition(ctx, job.ID)
	if err != nil || loaded.Status != jobs.StatusSucceeded {
		t.Fatalf("accepted completion = %s, %v", loaded.Status, err)
	}
}

type testExecution struct {
	wait func(context.Context, func(Progress) error) (Completion, error)
}

func (e testExecution) Wait(ctx context.Context, report func(Progress) error) (Completion, error) {
	if e.wait != nil {
		return e.wait(ctx, report)
	}
	return CompletionSucceeded, nil
}

func selectedFixture(t *testing.T) (*jobs.Service, *sql.DB, string, jobs.Job, Selection) {
	t.Helper()
	service, jobService, db, path, job := fixture(t, provider("source", option("a"), option("other")))
	discovery, err := service.Discover(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := service.Select(context.Background(), job.ID, discovery.Candidates[0].Handle)
	if err != nil {
		t.Fatal(err)
	}
	return jobService, db, path, job, selection
}

func executionRegistry(t *testing.T, resolver testResolver) *ExecutionRegistry {
	t.Helper()
	registry, err := NewExecutionRegistry(resolver)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestExecutionPreflightNeverClaimsOrSubstitutes(t *testing.T) {
	for _, name := range []string{"provider absent", "executor absent", "unresolved", "different candidate", "different ref", "different provider", "different handle", "different job", "different edition", "invented execution identity"} {
		t.Run(name, func(t *testing.T) {
			jobService, db, _, job, selected := selectedFixture(t)
			resolver := testResolver{resolve: func(_ context.Context, selection Selection) (ExecutionPlan, error) {
				if selection != selected {
					t.Error("resolver did not receive exact durable selection")
				}
				plan := PlanForSelection(selection)
				switch name {
				case "unresolved":
					return ExecutionPlan{}, errors.New("PRIVATE_EXECUTION_SENTINEL_a RAW_SECRET_SENTINEL")
				case "different candidate":
					plan.CandidateID = "other"
				case "different ref":
					plan.ExecutionRef = option("other").ExecutionRef
				case "different provider":
					plan.ProviderID = "other"
				case "different handle":
					plan.Handle = strings.Repeat("a", 64)
				case "different job":
					plan.JobID = "other"
				case "different edition":
					plan.EditionID = "other"
				case "invented execution identity":
					plan.ExecutionID = strings.Repeat("a", 64)
				}
				return plan, nil
			}}
			registry := executionRegistry(t, resolver)
			executor := &testExecutor{dispatch: func(context.Context, ExecutionPlan) (DispatchResult, error) {
				t.Error("preflight dispatched")
				return DispatchResult{}, nil
			}}
			var adapter Executor = executor
			want := ErrUnresolved
			if name == "provider absent" {
				registry = nil
				want = ErrExecutionProvider
			}
			if name == "executor absent" {
				adapter = nil
				want = ErrExecutor
			}
			core := NewExecutionService(context.Background(), jobService, NewSQLiteRepository(db), registry, adapter)
			defer core.Close()
			if _, err := core.Execute(context.Background(), job.ID); !errors.Is(err, want) {
				t.Fatalf("preflight error = %v, want %v", err, want)
			}
			if _, exists, err := NewSQLiteRepository(db).GetReservation(context.Background(), job.ID); err != nil || exists {
				t.Fatalf("preflight claimed: %t, %v", exists, err)
			}
			loaded, _ := jobService.GetAcquisition(context.Background(), job.ID)
			if loaded.Status != jobs.StatusQueued || executor.calls.Load() != 0 {
				t.Fatal("preflight changed Job or called executor")
			}
		})
	}
}

func TestExecutionDurableRejectionAndAmbiguity(t *testing.T) {
	for _, name := range []string{"rejection", "raw dispatch error", "dispatch timeout", "invalid acceptance", "contradictory rejection"} {
		t.Run(name, func(t *testing.T) {
			jobService, db, _, job, _ := selectedFixture(t)
			drained := false
			executor := &testExecutor{dispatch: func(ctx context.Context, _ ExecutionPlan) (DispatchResult, error) {
				switch name {
				case "rejection":
					return DispatchResult{Decision: DispatchRejected}, nil
				case "dispatch timeout":
					<-ctx.Done()
					return DispatchResult{}, ctx.Err()
				case "raw dispatch error":
					return DispatchResult{}, errors.New("PRIVATE_EXECUTION_SENTINEL_a")
				case "contradictory rejection":
					return DispatchResult{Decision: DispatchRejected, Execution: testExecution{wait: func(ctx context.Context, _ func(Progress) error) (Completion, error) {
						drained = ctx.Err() != nil
						return CompletionInterrupted, ctx.Err()
					}}}, nil
				default:
					return DispatchResult{Decision: DispatchAccepted}, nil
				}
			}}
			core := NewExecutionService(context.Background(), jobService, NewSQLiteRepository(db), executionRegistry(t, testResolver{}), executor)
			core.timeout = 10 * time.Millisecond
			defer core.Close()
			first, err := core.Execute(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus, wantOutcome := jobs.StatusFailed, DispatchUnconfirmed
			if name == "rejection" {
				wantStatus, wantOutcome = jobs.StatusQueued, DispatchRejected
			}
			if first.Outcome != wantOutcome || (name == "contradictory rejection" && !drained) {
				t.Fatalf("outcome = %s, drained %t", first.Outcome, drained)
			}
			// Retry without even a resolver/executor: it must be a durable read.
			restarted := NewExecutionService(context.Background(), jobService, NewSQLiteRepository(db), nil, nil)
			defer restarted.Close()
			retry, err := restarted.Execute(context.Background(), job.ID)
			if err != nil || retry != first || executor.calls.Load() != 1 {
				t.Fatalf("durable retry = %#v, %v, calls %d", retry, err, executor.calls.Load())
			}
			loaded, _ := jobService.GetAcquisition(context.Background(), job.ID)
			if loaded.Status != wantStatus || strings.Contains(loaded.ErrorDetail, "SENTINEL") {
				t.Fatalf("Job = %#v", loaded)
			}
		})
	}
}

func TestExecutionConcurrentTwoSQLitePoolsDispatchOnce(t *testing.T) {
	jobService, db, path, job, _ := selectedFixture(t)
	secondDB, err := database.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer secondDB.Close()
	entered, release := make(chan struct{}, 24), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	executor := &testExecutor{dispatch: func(context.Context, ExecutionPlan) (DispatchResult, error) {
		entered <- struct{}{}
		<-release
		return DispatchResult{Decision: DispatchAccepted, Execution: testExecution{}}, nil
	}}
	registry := executionRegistry(t, testResolver{})
	cores := []*ExecutionService{
		NewExecutionService(context.Background(), jobService, NewSQLiteRepository(db), registry, executor),
		NewExecutionService(context.Background(), jobs.NewService(jobs.NewSQLiteRepository(secondDB)), NewSQLiteRepository(secondDB), registry, executor),
	}
	defer func() {
		releaseOnce.Do(func() { close(release) })
		for _, core := range cores {
			core.Close()
		}
	}()
	type response struct {
		reservation Reservation
		err         error
	}
	start, responses := make(chan struct{}), make(chan response, 24)
	for i := range 24 {
		go func(i int) {
			<-start
			result, err := cores[i%2].Execute(context.Background(), job.ID)
			responses <- response{result, err}
		}(i)
	}
	close(start)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch did not start")
	}
	var identity string
	// All losers must return a safe reserved read while the sole call is blocked.
	for range 23 {
		select {
		case got := <-responses:
			if got.err != nil || got.reservation.Outcome != DispatchReserved {
				t.Fatalf("loser response = %#v, %v", got.reservation, got.err)
			}
			if identity != "" && got.reservation.ExecutionID != identity {
				t.Fatal("multiple durable identities")
			}
			identity = got.reservation.ExecutionID
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent retry blocked or attempted a second dispatch")
		}
	}
	releaseOnce.Do(func() { close(release) })
	got := <-responses
	if got.err != nil || got.reservation.Outcome != DispatchAccepted || got.reservation.ExecutionID != identity || executor.calls.Load() != 1 {
		t.Fatalf("winner = %#v, %v; dispatches %d", got.reservation, got.err, executor.calls.Load())
	}
	for _, core := range cores {
		core.Wait()
	}
}

func TestExecutionRestartNeverRedispatchesCrashWindows(t *testing.T) {
	for _, scenario := range []struct {
		outcome DispatchOutcome
		running bool
	}{{DispatchReserved, false}, {DispatchAccepted, false}, {DispatchRejected, false}, {DispatchAccepted, true}} {
		name := string(scenario.outcome)
		if scenario.running {
			name += " running"
		}
		t.Run(name, func(t *testing.T) {
			outcome := scenario.outcome
			jobService, db, _, job, selected := selectedFixture(t)
			repository := NewSQLiteRepository(db)
			reservation, won, err := repository.Reserve(context.Background(), selected, strings.Repeat("a", 64))
			if err != nil || !won {
				t.Fatalf("reserve = %t, %v", won, err)
			}
			if outcome != DispatchReserved {
				if err := repository.Decide(context.Background(), reservation, outcome); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.running {
				if err := jobService.Transition(context.Background(), job.ID, jobs.StatusRunning); err != nil {
					t.Fatal(err)
				}
			}
			// Model durable crash windows directly, without Close: a graceful
			// worker shutdown would already have terminalized the running Job.
			before, err := jobService.GetAcquisition(context.Background(), job.ID)
			wantBefore, wantCount := jobs.StatusQueued, int64(1)
			if outcome == DispatchRejected {
				wantCount = 0 // an explicitly rejected claim remains queued
			}
			if scenario.running {
				wantBefore = jobs.StatusRunning
			}
			if err != nil || before.Status != wantBefore {
				t.Fatalf("crash boundary: read_error=%t status=%s want=%s", err != nil, before.Status, wantBefore)
			}
			if count, err := jobService.ReconcileStartup(context.Background()); err != nil || count != wantCount {
				t.Fatalf("crash reconciliation: reconcile_error=%t count=%d want=%d", err != nil, count, wantCount)
			}
			executor := &testExecutor{dispatch: func(context.Context, ExecutionPlan) (DispatchResult, error) {
				t.Error("startup redispatched")
				return DispatchResult{}, nil
			}}
			core := NewExecutionService(context.Background(), jobService, repository, executionRegistry(t, testResolver{}), executor)
			defer core.Close()
			retry, err := core.Execute(context.Background(), job.ID)
			if err != nil || retry.ExecutionID != reservation.ExecutionID || executor.calls.Load() != 0 {
				t.Fatalf("restart retry = %#v, %v", retry, err)
			}
			wantStatus, wantOutcome := jobs.StatusFailed, outcome
			if outcome == DispatchReserved {
				wantOutcome = DispatchUnconfirmed
			}
			if outcome == DispatchRejected {
				wantStatus = jobs.StatusQueued
			}
			if scenario.running {
				wantStatus = jobs.StatusInterrupted
			}
			loaded, _ := jobService.GetAcquisition(context.Background(), job.ID)
			if loaded.Status != wantStatus || retry.Outcome != wantOutcome {
				t.Fatalf("recovered = %s/%s", loaded.Status, retry.Outcome)
			}
			if count, err := jobService.ReconcileStartup(context.Background()); err != nil || count != 0 {
				t.Fatalf("reconciliation replay = %d, %v", count, err)
			}
		})
	}
}

func TestExecutionCancellationResolutionAndOwnedAcceptance(t *testing.T) {
	for _, mode := range []string{"cancelled before request", "cancelled resolution", "resolution timeout", "cancelled after claim", "shutdown during dispatch"} {
		t.Run(mode, func(t *testing.T) {
			jobService, db, _, job, _ := selectedFixture(t)
			requestCtx, cancelRequest := context.WithCancel(context.Background())
			defer cancelRequest()
			ownerCtx, cancelOwner := context.WithCancel(context.Background())
			defer cancelOwner()
			resolver := testResolver{resolve: func(ctx context.Context, selection Selection) (ExecutionPlan, error) {
				if mode == "cancelled resolution" {
					cancelRequest()
				}
				if mode == "cancelled resolution" || mode == "resolution timeout" {
					<-ctx.Done()
					return ExecutionPlan{}, errors.New("PRIVATE_RESOLVE_ERROR")
				}
				return PlanForSelection(selection), nil
			}}
			executor := &testExecutor{dispatch: func(ctx context.Context, _ ExecutionPlan) (DispatchResult, error) {
				cancelRequest()
				if mode == "shutdown during dispatch" {
					cancelOwner()
					<-ctx.Done()
					return DispatchResult{}, ctx.Err()
				}
				if ctx.Err() != nil {
					t.Error("request cancellation leaked into owned dispatch")
				}
				return DispatchResult{Decision: DispatchAccepted, Execution: testExecution{}}, nil
			}}
			core := NewExecutionService(ownerCtx, jobService, NewSQLiteRepository(db), executionRegistry(t, resolver), executor)
			core.timeout = 10 * time.Millisecond
			defer core.Close()
			if mode == "cancelled before request" {
				cancelRequest()
			}
			result, err := core.Execute(requestCtx, job.ID)
			core.Wait()
			loaded, _ := jobService.GetAcquisition(context.Background(), job.ID)
			switch mode {
			case "cancelled before request", "cancelled resolution", "resolution timeout":
				want := ErrCancelled
				if mode == "resolution timeout" {
					want = ErrTimeout
				}
				if !errors.Is(err, want) || executor.calls.Load() != 0 || loaded.Status != jobs.StatusQueued {
					t.Fatalf("preclaim cancellation = %#v, %v, %s", result, err, loaded.Status)
				}
				if _, exists, _ := NewSQLiteRepository(db).GetReservation(context.Background(), job.ID); exists {
					t.Fatal("preclaim cancellation left reservation")
				}
			case "cancelled after claim":
				if err != nil || result.Outcome != DispatchAccepted || loaded.Status != jobs.StatusSucceeded {
					t.Fatalf("late cancellation lost acceptance = %#v, %v, %s", result, err, loaded.Status)
				}
			case "shutdown during dispatch":
				if err != nil || result.Outcome != DispatchUnconfirmed || loaded.Status != jobs.StatusFailed {
					t.Fatalf("shutdown ambiguity = %#v, %v, %s", result, err, loaded.Status)
				}
			}
		})
	}
}

func TestExecutionAcceptedLifecycleProgressAndShutdown(t *testing.T) {
	for _, mode := range []string{"success", "failure", "interrupted", "bad progress", "shutdown", "shutdown during progress", "bad progress during shutdown"} {
		t.Run(mode, func(t *testing.T) {
			jobService, db, _, job, _ := selectedFixture(t)
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			releaseWorker := func() { once.Do(func() { close(release) }) }
			executor := &testExecutor{dispatch: func(context.Context, ExecutionPlan) (DispatchResult, error) {
				return DispatchResult{Decision: DispatchAccepted, Execution: testExecution{wait: func(ctx context.Context, report func(Progress) error) (Completion, error) {
					loaded, _ := jobService.GetAcquisition(context.Background(), job.ID)
					if loaded.Status != jobs.StatusRunning {
						t.Errorf("accepted worker saw %s", loaded.Status)
					}
					if err := report(Progress{Current: 1, Total: 2}); err != nil {
						t.Errorf("valid progress: %v", err)
					}
					close(started)
					select {
					case <-ctx.Done():
						if mode == "shutdown during progress" {
							// Reproduce shutdown between receiving progress and persisting it.
							err := report(Progress{Current: 2, Total: 2})
							if !errors.Is(err, context.Canceled) {
								t.Errorf("shutdown progress: cancelled=%t", errors.Is(err, context.Canceled))
							}
							return CompletionFailed, ErrExecutor // same report-error contract as qBit
						}
						return CompletionInterrupted, ctx.Err()
					case <-release:
					}
					switch mode {
					case "bad progress", "bad progress during shutdown":
						if err := report(Progress{Current: 0, Total: 2}); !errors.Is(err, jobs.ErrInvalidProgress) {
							t.Errorf("regressive progress: invalid=%t", errors.Is(err, jobs.ErrInvalidProgress))
						}
						if mode == "bad progress during shutdown" {
							<-ctx.Done() // progress failure cancels observation, but must stay failed
							return CompletionInterrupted, ctx.Err()
						}
						return CompletionSucceeded, nil
					case "failure":
						return CompletionFailed, errors.New("PRIVATE_EXECUTION_SENTINEL_a")
					case "interrupted":
						return CompletionInterrupted, nil
					default:
						if err := report(Progress{Current: 2, Total: 2}); err != nil {
							t.Error(err)
						}
						return CompletionSucceeded, nil
					}
				}}}, nil
			}}
			core := NewExecutionService(context.Background(), jobService, NewSQLiteRepository(db), executionRegistry(t, testResolver{}), executor)
			defer func() { releaseWorker(); core.Close() }()
			if _, err := core.Execute(context.Background(), job.ID); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("accepted worker did not start")
			}
			loaded, _ := jobService.GetAcquisition(context.Background(), job.ID)
			if loaded.Status != jobs.StatusRunning || loaded.ProgressCurrent != 1 || loaded.ProgressTotal != 2 {
				t.Fatalf("active progress = %#v", loaded)
			}
			if mode == "shutdown" || mode == "shutdown during progress" {
				core.Close()
				if _, err := core.Execute(context.Background(), job.ID); !errors.Is(err, ErrUnavailable) {
					t.Fatalf("closed service admitted retry: %v", err)
				}
			} else {
				releaseWorker()
				core.Wait()
			}
			want := jobs.StatusFailed
			if mode == "success" {
				want = jobs.StatusSucceeded
			}
			if mode == "interrupted" || mode == "shutdown" || mode == "shutdown during progress" {
				want = jobs.StatusInterrupted
			}
			loaded, _ = jobService.GetAcquisition(context.Background(), job.ID)
			if loaded.Status != want || strings.Contains(loaded.ErrorDetail, "SENTINEL") {
				t.Fatalf("terminal: status=%s want=%s error_code=%s progress=%d/%d", loaded.Status, want, loaded.ErrorCode, loaded.ProgressCurrent, loaded.ProgressTotal)
			}
		})
	}
}

func TestExecutionTerminalJobsAndKindIsolation(t *testing.T) {
	for _, status := range []jobs.Status{jobs.StatusRunning, jobs.StatusCancelled, jobs.StatusSucceeded, jobs.StatusFailed, jobs.StatusInterrupted} {
		t.Run(string(status), func(t *testing.T) {
			jobService, db, _, job, _ := selectedFixture(t)
			ctx := context.Background()
			if status != jobs.StatusCancelled {
				if err := jobService.Transition(ctx, job.ID, jobs.StatusRunning); err != nil {
					t.Fatal(err)
				}
			}
			if status != jobs.StatusRunning {
				if err := jobService.Transition(ctx, job.ID, status); err != nil {
					t.Fatal(err)
				}
			}
			core := NewExecutionService(ctx, jobService, NewSQLiteRepository(db), nil, nil)
			defer core.Close()
			if _, err := core.Execute(ctx, job.ID); !errors.Is(err, ErrNotQueued) {
				t.Fatalf("nonqueued execution = %v", err)
			}
			if _, exists, _ := NewSQLiteRepository(db).GetReservation(ctx, job.ID); exists {
				t.Fatal("nonqueued Job claimed")
			}
			other, err := jobService.Create(ctx, jobs.KindRestoreAsset)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := core.Execute(ctx, other.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("wrong-kind execution = %v", err)
			}
		})
	}
}

func TestExecutionMissingSelectionDoesNotDispatch(t *testing.T) {
	_, jobService, db, _, job := fixture(t)
	executor := &testExecutor{dispatch: func(context.Context, ExecutionPlan) (DispatchResult, error) {
		t.Fatal("dispatched without selection")
		return DispatchResult{}, nil
	}}
	core := NewExecutionService(context.Background(), jobService, NewSQLiteRepository(db), nil, executor)
	defer core.Close()
	if _, err := core.Execute(context.Background(), job.ID); !errors.Is(err, ErrNoSelection) {
		t.Fatalf("missing selection = %v", err)
	}
}
