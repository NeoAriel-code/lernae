package acquisition

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lernae/internal/database"
	"lernae/internal/domain"
	"lernae/internal/jobs"
)

type testProvider struct {
	id       string
	eligible bool
	discover func(context.Context, Target) ([]Option, error)
}

func (p testProvider) ID() string           { return p.id }
func (p testProvider) Eligible(Target) bool { return p.eligible }
func (p testProvider) Discover(ctx context.Context, target Target) ([]Option, error) {
	return p.discover(ctx, target)
}

func option(id string) Option {
	return Option{
		ID: id, Metadata: Metadata{Title: "Human title", Label: "Standard edition", Language: "en"},
		ExecutionRef: "PRIVATE_EXECUTION_SENTINEL_" + id,
	}
}

func provider(id string, options ...Option) testProvider {
	return testProvider{
		id: id, eligible: true,
		discover: func(context.Context, Target) ([]Option, error) { return options, nil },
	}
}

func fixture(t *testing.T, providers ...Provider) (*Service, *jobs.Service, *sql.DB, string, jobs.Job) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "acquisition.db")
	db, err := database.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, query := range []string{
		`INSERT INTO works (id, medium, work_type, title) VALUES ('work-1', 'literature', 'novel', 'Fixture')`,
		`INSERT INTO editions (id, work_id, format) VALUES ('edition-1', 'work-1', 'epub')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	jobService := jobs.NewService(jobs.NewSQLiteRepository(db))
	job, err := jobService.CreateAcquisition(context.Background(), "edition-1")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(providers...)
	if err != nil {
		t.Fatal(err)
	}
	return NewService(jobService, NewSQLiteRepository(db), registry), jobService, db, path, job
}

func TestDiscoveryEligibilityIsolationOrderAndContext(t *testing.T) {
	never := testProvider{id: "ineligible", discover: func(context.Context, Target) ([]Option, error) {
		t.Error("ineligible provider called")
		return nil, nil
	}}
	bad := testProvider{id: "failure", eligible: true, discover: func(context.Context, Target) ([]Option, error) {
		return nil, errors.New("RAW_SECRET_SENTINEL")
	}}
	good := provider("alpha", option("z"), option("a"))
	good.discover = func(ctx context.Context, target Target) ([]Option, error) {
		if target.Edition.ID != domain.EditionID("edition-1") || target.Work.ID != target.Edition.WorkID || target.Work.Title != "Fixture" {
			t.Errorf("derived target = %#v", target)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("missing bounded deadline")
		}
		return []Option{option("z"), option("a")}, nil
	}
	service, jobService, _, _, job := fixture(t, provider("zeta", option("a")), never, bad, good)
	result, err := service.Discover(context.Background(), job.ID)
	if err != nil || result.Outcome != OutcomePartial || len(result.Candidates) != 3 || len(result.Failures) != 1 {
		t.Fatalf("discovery = %#v, %v", result, err)
	}
	if result.Failures[0] != (Failure{ProviderID: "failure", Category: ErrProvider}) {
		t.Fatalf("unsafe failure = %#v", result.Failures)
	}
	for i, want := range []string{"alpha/a", "alpha/z", "zeta/a"} {
		candidate := result.Candidates[i]
		if candidate.ProviderID+"/"+candidate.Option.ID != want || candidate.JobID != job.ID || candidate.EditionID != "edition-1" || !handlePattern.MatchString(candidate.Handle) {
			t.Errorf("candidate[%d] = %#v", i, candidate)
		}
	}
	stored, err := jobService.GetAcquisition(context.Background(), job.ID)
	if err != nil || stored.Status != jobs.StatusQueued {
		t.Fatalf("discovery changed Job: %#v, %v", stored, err)
	}
}

func TestDiscoveryPreservesOnlyDirectSafeProviderErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want Error
	}{
		{"invalid response", ErrInvalidResponse, ErrInvalidResponse},
		{"timeout", ErrTimeout, ErrTimeout},
		{"unavailable", ErrProvider, ErrProvider},
		{"raw", errors.New("PRIVATE_UPSTREAM_ERROR"), ErrProvider},
		{"wrapped", errors.Join(ErrInvalidResponse, errors.New("PRIVATE_UPSTREAM_ERROR")), ErrProvider},
		{"other typed", ErrConflict, ErrProvider},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := testProvider{id: "source", eligible: true, discover: func(context.Context, Target) ([]Option, error) { return nil, test.err }}
			s, _, _, _, job := fixture(t, p)
			result, err := s.Discover(context.Background(), job.ID)
			if err != nil || len(result.Failures) != 1 || result.Failures[0].Category != test.want {
				t.Fatalf("safe failure mapping = %v, %v; want %s", result.Failures, err, test.want)
			}
		})
	}
}

func TestDiscoveryExplicitEmptyOutcomes(t *testing.T) {
	failed := testProvider{id: "bad", eligible: true, discover: func(context.Context, Target) ([]Option, error) {
		return nil, ErrProvider
	}}
	for _, test := range []struct {
		name      string
		providers []Provider
		outcome   Outcome
	}{
		{"unconfigured", nil, OutcomeUnconfigured},
		{"ineligible", []Provider{testProvider{id: "no"}}, OutcomeIneligible},
		{"empty", []Provider{provider("empty")}, OutcomeEmpty},
		{"failed", []Provider{failed}, OutcomeFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, _, _, _, job := fixture(t, test.providers...)
			result, err := service.Discover(context.Background(), job.ID)
			if err != nil || result.Outcome != test.outcome || len(result.Candidates) != 0 || result.Candidates == nil || result.Failures == nil {
				t.Fatalf("empty outcome = %#v, %v", result, err)
			}
		})
	}
}

func TestRegistryRejectsInvalidAndDuplicateIDs(t *testing.T) {
	for _, id := range []string{"", "UPPER", "https://host", "bad\n", strings.Repeat("x", 65)} {
		if _, err := NewRegistry(provider(id)); !errors.Is(err, ErrRegistry) {
			t.Errorf("registry %q: %v", id, err)
		}
	}
	for _, providers := range [][]Provider{
		{provider("same"), provider("same")},
		{nil},
		{(*testProvider)(nil)},
		make([]Provider, MaxProviders+1),
	} {
		if _, err := NewRegistry(providers...); !errors.Is(err, ErrRegistry) {
			t.Fatalf("invalid registry accepted: %v", err)
		}
	}
	registry, err := NewRegistry(provider("source"))
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := registry.Lookup("source"); !ok || p.ID() != "source" {
		t.Fatal("exact provider lookup failed")
	}
	if _, ok := registry.Lookup("unknown"); ok {
		t.Fatal("registry inferred unknown provider")
	}
}

func TestInvalidProviderResultsFailClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Option)
	}{
		{"empty id", func(o *Option) { o.ID = "" }},
		{"URL id", func(o *Option) { o.ID = "https://private" }},
		{"blank title", func(o *Option) { o.Metadata.Title = " " }},
		{"meaningless title", func(o *Option) { o.Metadata.Title = "..." }},
		{"URL title", func(o *Option) { o.Metadata.Title = "See https://private.example/token" }},
		{"URL label", func(o *Option) { o.Metadata.Label = "www.private.example" }},
		{"control", func(o *Option) { o.Metadata.Title = "Title\nPRIVATE" }},
		{"format control", func(o *Option) { o.Metadata.Title = "Title\u202eSECRET" }},
		{"long title", func(o *Option) { o.Metadata.Title = strings.Repeat("x", 257) }},
		{"long label", func(o *Option) { o.Metadata.Label = strings.Repeat("x", 129) }},
		{"invalid utf8", func(o *Option) { o.Metadata.Title = string([]byte{0xff}) }},
		{"language", func(o *Option) { o.Metadata.Language = "https://private" }},
		{"URL ref", func(o *Option) { o.ExecutionRef = "https://private/token" }},
		{"raw ref", func(o *Option) { o.ExecutionRef = `{"secret":"value"}` }},
		{"long ref", func(o *Option) { o.ExecutionRef = strings.Repeat("x", 513) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := option("invalid")
			test.mutate(&invalid)
			service, _, _, _, job := fixture(t, provider("bad", option("valid"), invalid), provider("good", option("valid")))
			result, err := service.Discover(context.Background(), job.ID)
			if err != nil || len(result.Candidates) != 1 || len(result.Failures) != 1 || result.Failures[0].Category != ErrInvalidResponse {
				t.Fatalf("invalid response was not isolated: %#v, %v", result, err)
			}
		})
	}
	for _, options := range [][]Option{{option("same"), option("same")}, make([]Option, MaxProviderCandidates+1)} {
		service, _, _, _, job := fixture(t, provider("bad", options...))
		result, err := service.Discover(context.Background(), job.ID)
		if err != nil || result.Outcome != OutcomeFailed || result.Failures[0].Category != ErrInvalidResponse {
			t.Fatalf("duplicate/unbounded response = %#v, %v", result, err)
		}
	}
}

func TestPresentationTextAllowsHumanPunctuationAndUnicode(t *testing.T) {
	for _, title := range []string{"Title: subtitle", "日本語のタイトル", "Édition française", "1984"} {
		candidate := option("a")
		candidate.Metadata.Title = title
		if !validOption(candidate) {
			t.Errorf("valid presentation title rejected: %q", title)
		}
	}
}

func TestCancellationAndTimeoutJoinProviderWork(t *testing.T) {
	finished := make(chan struct{})
	slow := testProvider{id: "slow", eligible: true, discover: func(ctx context.Context, _ Target) ([]Option, error) {
		defer close(finished)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	service, _, _, _, job := fixture(t, slow, provider("fast", option("a")))
	service.timeout = 10 * time.Millisecond
	result, err := service.Discover(context.Background(), job.ID)
	if err != nil || len(result.Candidates) != 1 || result.Failures[0].Category != ErrTimeout {
		t.Fatalf("timeout isolation = %#v, %v", result, err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("provider work was abandoned")
	}
	service.timeout = time.Second
	for _, deadline := range []bool{false, true} {
		finished = make(chan struct{})
		var ctx context.Context
		var cancel context.CancelFunc
		if deadline {
			ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
		} else {
			ctx, cancel = context.WithCancel(context.Background())
			cancel()
		}
		_, err := service.Discover(ctx, job.ID)
		cancel()
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("caller cancellation = %v", err)
		}
		if deadline {
			select {
			case <-finished:
			default:
				t.Fatal("caller deadline abandoned work")
			}
		}
	}
}

func TestProviderCallsStartIndependentlyAndKeepCallerContext(t *testing.T) {
	type contextKey struct{}
	fastStarted := make(chan struct{})
	slow := testProvider{id: "alpha-slow", eligible: true, discover: func(ctx context.Context, _ Target) ([]Option, error) {
		select {
		case <-fastStarted:
			return []Option{option("slow-option")}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	fast := testProvider{id: "zeta-fast", eligible: true, discover: func(ctx context.Context, _ Target) ([]Option, error) {
		if ctx.Value(contextKey{}) != "caller-value" {
			t.Error("caller context was replaced")
		}
		close(fastStarted)
		return []Option{option("fast-option")}, nil
	}}
	service, _, _, _, job := fixture(t, slow, fast)
	result, err := service.Discover(context.WithValue(context.Background(), contextKey{}, "caller-value"), job.ID)
	if err != nil || result.Outcome != OutcomeAvailable || len(result.Candidates) != 2 {
		t.Fatalf("provider calls blocked each other: %#v, %v", result, err)
	}
}

func TestSelectionExactSnapshotIdempotencyAndRestart(t *testing.T) {
	service, jobService, _, path, job := fixture(t, provider("source", option("a"), option("b")))
	ctx := context.Background()
	result, err := service.Discover(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	chosen := result.Candidates[0]
	// Mutating the returned projection must not change the retained snapshot.
	result.Candidates[0].Option.Metadata.Title = "Caller mutation"
	selection, err := service.Select(ctx, job.ID, chosen.Handle)
	if err != nil || selection.Candidate != chosen || selection.SelectedAt.IsZero() {
		t.Fatalf("selection = %#v, %v", selection, err)
	}
	otherDB, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer otherDB.Close()
	registry, _ := NewRegistry()
	restarted := NewService(jobs.NewService(jobs.NewSQLiteRepository(otherDB)), NewSQLiteRepository(otherDB), registry)
	loaded, err := restarted.GetSelection(ctx, job.ID)
	if err != nil || loaded != selection {
		t.Fatalf("restart lost selection: %#v, %v", loaded, err)
	}
	retry, err := restarted.Select(ctx, job.ID, chosen.Handle)
	if err != nil || retry != selection {
		t.Fatalf("retry without cache/provider = %#v, %v", retry, err)
	}
	if _, err := restarted.Select(ctx, job.ID, result.Candidates[1].Handle); !errors.Is(err, ErrConflict) {
		t.Fatalf("different selection = %v", err)
	}
	stored, err := jobService.GetAcquisition(ctx, job.ID)
	if err != nil || stored.Status != jobs.StatusQueued {
		t.Fatalf("selection transitioned Job: %#v, %v", stored, err)
	}
}

func TestMissingUnknownChangedExpiredAndJobScopedSnapshot(t *testing.T) {
	service, _, db, _, job := fixture(t, provider("source", option("a")))
	ctx := context.Background()
	if _, err := service.GetSelection(ctx, job.ID); !errors.Is(err, ErrNoSelection) {
		t.Fatalf("missing selection = %v", err)
	}
	if _, err := service.Select(ctx, job.ID, "bad"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("invalid handle = %v", err)
	}
	if _, err := service.Select(ctx, job.ID, strings.Repeat("a", 64)); !errors.Is(err, ErrSnapshotExpired) {
		t.Fatalf("missing snapshot = %v", err)
	}
	first, err := service.Discover(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Select(ctx, job.ID, strings.Repeat("a", 64)); !errors.Is(err, ErrUnknownCandidate) {
		t.Fatalf("unknown handle = %v", err)
	}
	changed := option("a")
	changed.ExecutionRef = "CHANGED_PRIVATE_REF"
	service.registry, _ = NewRegistry(provider("source", changed))
	second, err := service.Discover(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Select(ctx, job.ID, first.Candidates[0].Handle); !errors.Is(err, ErrUnknownCandidate) {
		t.Fatalf("stale snapshot accepted changed ID = %v", err)
	}
	if second.Candidates[0].Handle == first.Candidates[0].Handle {
		t.Fatal("snapshot handle reused")
	}
	now := time.Now().Add(snapshotTTL + time.Second)
	service.now = func() time.Time { return now }
	if _, err := service.Select(ctx, job.ID, second.Candidates[0].Handle); !errors.Is(err, ErrSnapshotExpired) {
		t.Fatalf("expired = %v", err)
	}
	if _, err := db.Exec(`INSERT INTO editions (id, work_id, format) VALUES ('edition-2', 'work-1', 'audiobook')`); err != nil {
		t.Fatal(err)
	}
	jobsService := jobs.NewService(jobs.NewSQLiteRepository(db))
	other, err := jobsService.CreateAcquisition(ctx, "edition-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Discover(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Select(ctx, other.ID, second.Candidates[0].Handle); !errors.Is(err, ErrUnknownCandidate) {
		t.Fatalf("cross-job selection = %v", err)
	}
	for _, id := range []string{"", "bad:id", "unknown"} {
		if _, err := service.Discover(ctx, id); err == nil {
			t.Fatalf("invalid/missing Job %q accepted", id)
		}
	}
}

func TestSnapshotCapacityEvictsOldest(t *testing.T) {
	service, _, db, _, job := fixture(t, provider("source", option("a")))
	service.capacity = 1
	first, err := service.Discover(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO editions (id, work_id, format) VALUES ('edition-2', 'work-1', 'audiobook')`); err != nil {
		t.Fatal(err)
	}
	other, err := jobs.NewService(jobs.NewSQLiteRepository(db)).CreateAcquisition(context.Background(), "edition-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Discover(context.Background(), other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Select(context.Background(), job.ID, first.Candidates[0].Handle); !errors.Is(err, ErrSnapshotExpired) {
		t.Fatalf("evicted snapshot remained selectable: %v", err)
	}
	if len(service.snapshots) != 1 {
		t.Fatalf("cache exceeded bound: %d", len(service.snapshots))
	}
}

func TestConcurrentSelectionsAcrossPools(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(map[bool]string{true: "same", false: "different"}[same], func(t *testing.T) {
			service, _, _, path, job := fixture(t, provider("source", option("a"), option("b")))
			ctx := context.Background()
			result, err := service.Discover(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			otherDB, err := database.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer otherDB.Close()
			repositories := []*SQLiteRepository{NewSQLiteRepository(otherDB), service.repository.(*SQLiteRepository)}
			start := make(chan struct{})
			var wait sync.WaitGroup
			errorsOut := make(chan error, 24)
			selections := make(chan Selection, 24)
			for i := 0; i < 24; i++ {
				wait.Add(1)
				go func(i int) {
					defer wait.Done()
					<-start
					index := 0
					if !same {
						index = i % 2
					}
					selection, err := repositories[i%2].Accept(ctx, Selection{
						Candidate: result.Candidates[index], SelectedAt: time.Now().UTC(),
					})
					errorsOut <- err
					selections <- selection
				}(i)
			}
			close(start)
			wait.Wait()
			close(errorsOut)
			close(selections)
			var successes, conflicts int
			for err := range errorsOut {
				if err == nil {
					successes++
				} else if errors.Is(err, ErrConflict) {
					conflicts++
				} else {
					t.Errorf("unexpected concurrency error: %v", err)
				}
			}
			if (same && successes != 24) || (!same && (successes != 12 || conflicts != 12)) {
				t.Fatalf("success/conflict = %d/%d", successes, conflicts)
			}
			stored, err := service.GetSelection(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			for selection := range selections {
				if selection.Candidate.Handle != "" && selection != stored {
					t.Errorf("immutable winner changed: %#v != %#v", selection, stored)
				}
			}
		})
	}
}

func TestQueuedGuardAndExecutionRace(t *testing.T) {
	for i := 0; i < 12; i++ {
		service, jobService, _, path, job := fixture(t, provider("source", option("a")))
		ctx := context.Background()
		result, err := service.Discover(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		otherDB, err := database.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		repo := NewSQLiteRepository(otherDB)
		start := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(2)
		var accepted Selection
		var acceptErr, transitionErr error
		go func() {
			defer wait.Done()
			<-start
			accepted, acceptErr = repo.Accept(ctx, Selection{Candidate: result.Candidates[0], SelectedAt: time.Now().UTC()})
		}()
		go func() {
			defer wait.Done()
			<-start
			transitionErr = jobService.Transition(ctx, job.ID, jobs.StatusRunning)
		}()
		close(start)
		wait.Wait()
		if transitionErr != nil || (acceptErr != nil && !errors.Is(acceptErr, ErrNotQueued)) {
			t.Fatalf("race = %v/%v", acceptErr, transitionErr)
		}
		if acceptErr == nil && accepted.Candidate != result.Candidates[0] {
			t.Fatal("wrong accepted option")
		}
		if _, err := service.Discover(ctx, job.ID); !errors.Is(err, ErrNotQueued) {
			t.Fatalf("running discovery = %v", err)
		}
		fresh := result.Candidates[0]
		fresh.Handle = strings.Repeat("b", 64)
		_, err = repo.Accept(ctx, Selection{Candidate: fresh, SelectedAt: time.Now().UTC()})
		_ = otherDB.Close()
		if !errors.Is(err, ErrNotQueued) && !errors.Is(err, ErrConflict) {
			t.Fatalf("running write guard returned unexpected result: %v", err)
		}
	}
}
