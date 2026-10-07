package jobs

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"lernae/internal/database"
)

func acquisitionFixture(t *testing.T) (*sql.DB, *Service, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jobs.db")
	db, err := database.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`INSERT INTO works (id, medium, work_type, title) VALUES ('work-1', 'literature', 'novel', 'Fixture')`,
		`INSERT INTO editions (id, work_id, format) VALUES ('edition-1', 'work-1', 'epub'), ('edition-2', 'work-1', 'audiobook')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return db, NewService(NewSQLiteRepository(db)), path
}

func TestAcquisitionRequiresEditionTarget(t *testing.T) {
	_, service, _ := acquisitionFixture(t)
	ctx := context.Background()
	if job, err := service.Create(ctx, "acquire"); err == nil {
		t.Fatalf("targetless acquisition accepted: %#v", job)
	}
	for _, target := range []string{"", " ", "edition-1 ", "../edition-1", "edition\n1", strings.Repeat("e", 129)} {
		t.Run(target, func(t *testing.T) {
			if _, err := service.CreateAcquisition(ctx, target); !errors.Is(err, ErrInvalidAcquisitionTarget) {
				t.Fatalf("invalid target error = %v", err)
			}
		})
	}
	if _, err := service.CreateAcquisition(ctx, "missing"); !errors.Is(err, ErrEditionNotFound) {
		t.Fatalf("unknown Edition error = %v", err)
	}
	list, err := service.ListAcquisitions(ctx, 20)
	if err != nil || len(list) != 0 || list == nil {
		t.Fatalf("invalid targets left jobs: %#v, %v", list, err)
	}
}

func TestAcquisitionAcceptsOwnedEditionWithoutChangingInventory(t *testing.T) {
	db, service, _ := acquisitionFixture(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO assets (id, edition_id, kind, total_size_bytes)
		VALUES ('owned-asset', 'edition-1', 'ebook', 42)`); err != nil {
		t.Fatal(err)
	}
	job, err := service.CreateAcquisition(ctx, "edition-1")
	if err != nil || job.Status != StatusQueued || job.TargetEditionID != "edition-1" {
		t.Fatalf("owned Edition acquisition = %#v, %v", job, err)
	}
	var count int
	var size int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*), total_size_bytes FROM assets WHERE id = 'owned-asset'`).Scan(&count, &size); err != nil {
		t.Fatal(err)
	}
	if count != 1 || size != 42 {
		t.Fatalf("inventory changed during acquisition acceptance: count=%d size=%d", count, size)
	}
}

func TestAcquisitionDurableIdentityAndKindIsolation(t *testing.T) {
	db, service, path := acquisitionFixture(t)
	ctx := context.Background()
	created, err := service.CreateAcquisition(ctx, "edition-1")
	if err != nil {
		t.Fatal(err)
	}
	if created.Kind != KindAcquire || created.Status != StatusQueued || created.TargetEditionID != "edition-1" ||
		!regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(created.ID) ||
		created.CreatedAt.IsZero() || !created.CreatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("created acquisition = %#v", created)
	}
	play, err := service.Create(ctx, KindPlay)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetAcquisition(ctx, play.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PLAY visible through acquisition reader: %v", err)
	}
	if _, err := service.GetAcquisition(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing acquisition error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	reloadedService := NewService(NewSQLiteRepository(reopened))
	loaded, err := reloadedService.GetAcquisition(ctx, created.ID)
	if err != nil || loaded != created {
		t.Fatalf("reloaded = %#v, %v; want %#v", loaded, err, created)
	}
	duplicate, err := reloadedService.CreateAcquisition(ctx, "edition-1")
	if err != nil || duplicate != created {
		t.Fatalf("persistent duplicate = %#v, %v", duplicate, err)
	}
}

func TestAcquisitionConcurrentActiveDeduplication(t *testing.T) {
	_, service, path := acquisitionFixture(t)
	ctx := context.Background()
	// A second pool proves correctness is not a mutex on one Service instance.
	peer, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	services := []*Service{service, NewService(NewSQLiteRepository(peer))}
	const count = 24
	results := make(chan Job, count)
	errorsCh := make(chan error, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(s *Service) {
			defer wg.Done()
			<-start
			job, err := s.CreateAcquisition(ctx, "edition-1")
			results <- job
			errorsCh <- err
		}(services[i%len(services)])
	}
	close(start)
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first Job
	for job := range results {
		if first.ID == "" {
			first = job
		}
		if job != first {
			t.Fatalf("concurrent requests returned different jobs: %#v / %#v", first, job)
		}
	}
	if err := service.Transition(ctx, first.ID, StatusRunning); err != nil {
		t.Fatal(err)
	}
	duplicate, err := service.CreateAcquisition(ctx, "edition-1")
	if err != nil || duplicate.ID != first.ID || duplicate.Status != StatusRunning {
		t.Fatalf("running duplicate = %#v, %v", duplicate, err)
	}
	other, err := service.CreateAcquisition(ctx, "edition-2")
	if err != nil || other.ID == first.ID {
		t.Fatalf("separate Edition request = %#v, %v", other, err)
	}
	list, err := service.ListAcquisitions(ctx, 100)
	if err != nil || len(list) != 2 {
		t.Fatalf("deduplicated list = %#v, %v", list, err)
	}
}

func TestAcquisitionTransitionsAndTerminalRetry(t *testing.T) {
	for _, terminal := range []Status{StatusSucceeded, StatusFailed, StatusInterrupted, StatusCancelled} {
		t.Run(string(terminal), func(t *testing.T) {
			_, service, _ := acquisitionFixture(t)
			ctx := context.Background()
			job, err := service.CreateAcquisition(ctx, "edition-1")
			if err != nil {
				t.Fatal(err)
			}
			for _, next := range []Status{StatusSucceeded, StatusFailed, StatusInterrupted, StatusWaitingOnAgent, Status("unknown")} {
				if err := service.Transition(ctx, job.ID, next); !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("queued -> %s error = %v", next, err)
				}
			}
			if terminal != StatusCancelled {
				if err := service.Transition(ctx, job.ID, StatusRunning); err != nil {
					t.Fatal(err)
				}
				for _, next := range []Status{StatusQueued, StatusRunning, StatusWaitingOnAgent, StatusCancelled} {
					if err := service.Transition(ctx, job.ID, next); !errors.Is(err, ErrInvalidTransition) {
						t.Fatalf("running -> %s error = %v", next, err)
					}
				}
			}
			if err := service.Transition(ctx, job.ID, terminal); err != nil {
				t.Fatal(err)
			}
			loaded, err := service.GetAcquisition(ctx, job.ID)
			if err != nil || loaded.Status != terminal || loaded.UpdatedAt.Before(job.CreatedAt) {
				t.Fatalf("terminal Job = %#v, %v", loaded, err)
			}
			if err := service.Transition(ctx, job.ID, StatusRunning); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("terminal resurrection error = %v", err)
			}
			retry, err := service.CreateAcquisition(ctx, "edition-1")
			if err != nil || retry.ID == job.ID || retry.Status != StatusQueued {
				t.Fatalf("terminal retry = %#v, %v", retry, err)
			}
		})
	}
}

func TestAcquisitionBoundedFailureReasons(t *testing.T) {
	for _, terminal := range []Status{StatusFailed, StatusInterrupted, StatusCancelled} {
		t.Run(string(terminal), func(t *testing.T) {
			_, service, _ := acquisitionFixture(t)
			ctx := context.Background()
			job, err := service.CreateAcquisition(ctx, "edition-1")
			if err != nil {
				t.Fatal(err)
			}
			if terminal != StatusCancelled {
				if err := service.Transition(ctx, job.ID, StatusRunning); err != nil {
					t.Fatal(err)
				}
			}
			code := "acquisition_" + string(terminal)
			for _, reason := range []struct{ code, detail string }{
				{"raw_internal_error", "private infrastructure detail"},
				{code, strings.Repeat("x", 257)},
				{code, "raw infrastructure detail"},
			} {
				if err := service.TransitionWithError(ctx, job.ID, terminal, reason.code, reason.detail); !errors.Is(err, ErrInvalidAcquisitionReason) {
					t.Fatalf("unbounded reason accepted: %v", err)
				}
			}
			if err := service.TransitionWithError(ctx, job.ID, terminal, code, ""); err != nil {
				t.Fatal(err)
			}
			loaded, err := service.GetAcquisition(ctx, job.ID)
			if err != nil || loaded.ErrorCode != code || loaded.ErrorDetail == "" || len(loaded.ErrorDetail) > 256 {
				t.Fatalf("bounded failure = %#v, %v", loaded, err)
			}
		})
	}
}

func TestAcquisitionRecentOrderingAndBounds(t *testing.T) {
	db, service, _ := acquisitionFixture(t)
	ctx := context.Background()
	repository := NewSQLiteRepository(db)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, entry := range []struct {
		id string
		at time.Time
	}{
		{"job-old", base.Add(-time.Second)},
		{"job-a", base},
		{"job-b", base},
		{"job-new", base.Add(time.Nanosecond)},
	} {
		if err := repository.Create(ctx, Job{ID: entry.id, Kind: KindAcquire, TargetEditionID: "edition-1", Status: StatusSucceeded, CreatedAt: entry.at, UpdatedAt: entry.at}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.Create(ctx, KindRestoreAsset); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{-1, 0, 101} {
		if _, err := service.ListAcquisitions(ctx, limit); !errors.Is(err, ErrInvalidAcquisitionLimit) {
			t.Fatalf("invalid limit %d error = %v", limit, err)
		}
	}
	list, err := service.ListAcquisitions(ctx, 3)
	if err != nil || len(list) != 3 {
		t.Fatalf("recent list = %#v, %v", list, err)
	}
	for i, want := range []string{"job-new", "job-b", "job-a"} {
		if list[i].ID != want || list[i].Kind != KindAcquire {
			t.Fatalf("recent[%d] = %#v, want %s", i, list[i], want)
		}
	}
}

func TestAcquisitionStartupReconciliation(t *testing.T) {
	_, service, _ := acquisitionFixture(t)
	ctx := context.Background()
	queued, err := service.CreateAcquisition(ctx, "edition-1")
	if err != nil {
		t.Fatal(err)
	}
	running, err := service.CreateAcquisition(ctx, "edition-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, running.ID, StatusRunning); err != nil {
		t.Fatal(err)
	}
	play, err := service.Create(ctx, KindPlay)
	if err != nil {
		t.Fatal(err)
	}
	restore, err := service.Create(ctx, KindRestoreAsset)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, restore.ID, StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, restore.ID, StatusWaitingOnAgent); err != nil {
		t.Fatal(err)
	}
	count, err := service.ReconcileStartup(ctx)
	if err != nil || count != 3 {
		t.Fatalf("reconciliation count/error = %d/%v", count, err)
	}
	for id, want := range map[string]Status{queued.ID: StatusQueued, running.ID: StatusInterrupted, play.ID: StatusCancelled, restore.ID: StatusInterrupted} {
		job, err := service.Get(ctx, id)
		if err != nil || job.Status != want {
			t.Fatalf("reconciled %s = %#v, %v", id, job, err)
		}
	}
	interrupted, err := service.GetAcquisition(ctx, running.ID)
	if err != nil || interrupted.ErrorCode != "acquisition_interrupted" || interrupted.ErrorDetail == "" {
		t.Fatalf("startup interruption reason = %#v, %v", interrupted, err)
	}
	retry, err := service.CreateAcquisition(ctx, "edition-2")
	if err != nil || retry.ID == running.ID {
		t.Fatalf("startup retry = %#v, %v", retry, err)
	}
	if count, err := service.ReconcileStartup(ctx); err != nil || count != 0 {
		t.Fatalf("second reconciliation = %d, %v", count, err)
	}
}

func TestAcquisitionUnconfirmedQueuedFailureDoesNotChangeGenericTransitions(t *testing.T) {
	_, service, _ := acquisitionFixture(t)
	ctx := context.Background()
	job, err := service.CreateAcquisition(ctx, "edition-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, job.ID, StatusFailed); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("generic queued failure = %v", err)
	}
	if err := service.TransitionWithError(ctx, job.ID, StatusFailed, "acquisition_failed", "private detail"); !errors.Is(err, ErrInvalidAcquisitionReason) {
		t.Fatalf("queued raw reason = %v", err)
	}
	if err := service.TransitionWithError(ctx, job.ID, StatusFailed, "acquisition_failed", ""); err != nil {
		t.Fatal(err)
	}
	loaded, _ := service.GetAcquisition(ctx, job.ID)
	if loaded.Status != StatusFailed || loaded.ErrorDetail != acquisitionOutcomeDetail(StatusFailed) {
		t.Fatalf("unconfirmed failure = %#v", loaded)
	}
	other, err := service.Create(ctx, KindRestoreAsset)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.TransitionWithError(ctx, other.ID, StatusFailed, "restore_failed", "safe"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("generic queued transition expanded: %v", err)
	}
}

func TestAcquisitionProgressUsesExistingJobGuards(t *testing.T) {
	_, service, _ := acquisitionFixture(t)
	ctx := context.Background()
	job, err := service.CreateAcquisition(ctx, "edition-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.UpdateAcquisitionProgress(ctx, job.ID, 1, 2); !errors.Is(err, ErrInvalidProgress) {
		t.Fatalf("progress before acceptance = %v", err)
	}
	if err := service.Transition(ctx, job.ID, StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdateAcquisitionProgress(ctx, job.ID, 1, 2); err != nil {
		t.Fatal(err)
	}
	for _, update := range [][2]int64{{0, 2}, {1, 3}, {3, 2}, {1, 0}, {-1, 2}} {
		if err := service.UpdateAcquisitionProgress(ctx, job.ID, update[0], update[1]); !errors.Is(err, ErrInvalidProgress) {
			t.Fatalf("invalid progress %v = %v", update, err)
		}
	}
	if err := service.UpdateAcquisitionProgress(ctx, job.ID, 2, 2); err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, job.ID, StatusSucceeded); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdateAcquisitionProgress(ctx, job.ID, 2, 2); !errors.Is(err, ErrInvalidProgress) {
		t.Fatalf("terminal progress = %v", err)
	}
}

func TestAcquisitionSQLiteEnforcesIdentityAndActiveUniqueness(t *testing.T) {
	db, service, _ := acquisitionFixture(t)
	ctx := context.Background()
	job, err := service.CreateAcquisition(ctx, "edition-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE jobs SET target_edition_id = NULL`,
		`UPDATE jobs SET target_edition_id = 'missing'`,
		`UPDATE jobs SET status = 'waiting_on_agent'`,
		`INSERT INTO jobs (id, kind, status, created_at_utc, updated_at_utc) VALUES ('targetless', 'acquire', 'queued', 'now', 'now')`,
		`INSERT INTO jobs (id, kind, status, target_edition_id, created_at_utc, updated_at_utc) VALUES ('wrong-kind', 'play', 'queued', 'edition-1', 'now', 'now')`,
		`INSERT INTO jobs (id, kind, status, target_edition_id, created_at_utc, updated_at_utc) VALUES ('duplicate', 'acquire', 'running', 'edition-1', 'now', 'now')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("SQLite accepted invalid acquisition: %s", statement)
		}
	}
	if err := NewSQLiteRepository(db).Transition(ctx, job.ID, StatusQueued, StatusWaitingOnAgent, time.Now()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("repository waiting_on_agent error = %v", err)
	}
}
