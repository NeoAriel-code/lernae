package jobs

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"lernae/internal/agent"
	"lernae/internal/database"
	"lernae/internal/domain"
)

type restoreClientFunc func(context.Context, agent.RestoreAsset, func(agent.RestoreProgress)) (agent.RestoreResult, error)

func (client restoreClientFunc) RestoreAsset(ctx context.Context, request agent.RestoreAsset, progress func(agent.RestoreProgress)) (agent.RestoreResult, error) {
	return client(ctx, request, progress)
}

type progressCountingRepository struct {
	Repository
	progressWrites int
}

type cancelAfterRunningRepository struct {
	Repository
	cancel context.CancelFunc
}

func (repository *cancelAfterRunningRepository) Transition(ctx context.Context, id string, from, to Status, at time.Time) error {
	err := repository.Repository.Transition(ctx, id, from, to, at)
	if err == nil && to == StatusRunning {
		repository.cancel()
	}
	return err
}

func (repository *progressCountingRepository) UpdateProgress(ctx context.Context, id string, expectedCurrent, expectedTotal, current, total int64, message string, at time.Time) error {
	repository.progressWrites++
	return repository.Repository.UpdateProgress(ctx, id, expectedCurrent, expectedTotal, current, total, message, at)
}

func newRestoreJob(t *testing.T) (*Service, Job, agent.RestoreAsset) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := NewService(NewSQLiteRepository(db))
	job, err := service.Create(context.Background(), "restore_asset")
	if err != nil {
		t.Fatal(err)
	}
	assetID := domain.AssetID("asset-1")
	request := agent.RestoreAsset{
		JobID: job.ID,
		Asset: domain.Asset{ID: assetID, TotalSizeBytes: 4},
		Parts: []domain.AssetPart{{ID: "part-1", AssetID: assetID, Role: "rom", Filename: "game.iso", SizeBytes: 4}},
		SourceLocation: domain.AssetLocation{
			AssetID: assetID, StorageProviderID: agent.RestoreSourceProviderFixture,
			Locator: "fixtures/game.iso", LocationClass: agent.RestoreSourceClassFixture,
		},
	}
	return service, job, request
}

func restoreProgressEvents() []agent.RestoreProgress {
	return []agent.RestoreProgress{
		{Phase: agent.RestorePhaseValidating, TotalBytes: 4},
		{Phase: agent.RestorePhaseStaging, TotalBytes: 4},
		{Phase: agent.RestorePhaseCopying, TotalBytes: 4},
		{Phase: agent.RestorePhaseCopying, CurrentBytes: 4, TotalBytes: 4},
		{Phase: agent.RestorePhaseVerifying, CurrentBytes: 4, TotalBytes: 4},
		{Phase: agent.RestorePhasePromoting, CurrentBytes: 4, TotalBytes: 4},
		{Phase: agent.RestorePhaseComplete, CurrentBytes: 4, TotalBytes: 4},
	}
}

func localReadyResult(request agent.RestoreAsset) agent.RestoreResult {
	return agent.RestoreResult{
		AssetID:           request.Asset.ID,
		LocationClass:     "local_cache",
		RelativePath:      "assets/asset-1/game.iso",
		VerifiedSizeBytes: request.Asset.TotalSizeBytes,
		VerifiedAt:        time.Now().UTC(),
		LocalReady:        true,
	}
}

func TestRunRestorePersistsBoundedProgressAndReturnsVerifiedLocalReadyResult(t *testing.T) {
	service, job, request := newRestoreJob(t)
	client := restoreClientFunc(func(_ context.Context, got agent.RestoreAsset, report func(agent.RestoreProgress)) (agent.RestoreResult, error) {
		if got.JobID != job.ID {
			t.Errorf("restore Job ID = %q, want %q", got.JobID, job.ID)
		}
		for _, event := range restoreProgressEvents() {
			report(event)
		}
		return localReadyResult(request), nil
	})

	result, err := service.RunRestore(context.Background(), job.ID, request, client)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.ValidateFor(request); err != nil {
		t.Fatalf("RunRestore returned unverified local-ready evidence: %v", err)
	}
	loaded, err := service.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != StatusSucceeded || loaded.ProgressCurrent != 4 || loaded.ProgressTotal != 4 || loaded.Message != string(agent.RestorePhaseComplete) {
		t.Fatalf("completed restore Job = %#v", loaded)
	}
}

func TestPlayRestorePhaseReturnsVerifiedEvidenceWithoutTerminalizingJob(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := NewService(NewSQLiteRepository(db))
	job, err := service.Create(context.Background(), KindPlay)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(context.Background(), job.ID, StatusRunning); err != nil {
		t.Fatal(err)
	}
	assetID := domain.AssetID("asset-1")
	request := agent.RestoreAsset{
		JobID: job.ID,
		Asset: domain.Asset{ID: assetID, TotalSizeBytes: 4},
		Parts: []domain.AssetPart{{ID: "part-1", AssetID: assetID, Role: "rom", Filename: "game.iso", SizeBytes: 4}},
		SourceLocation: domain.AssetLocation{
			AssetID: assetID, StorageProviderID: agent.RestoreSourceProviderFixture,
			Locator: "fixtures/game.iso", LocationClass: agent.RestoreSourceClassFixture,
		},
	}
	client := restoreClientFunc(func(_ context.Context, got agent.RestoreAsset, report func(agent.RestoreProgress)) (agent.RestoreResult, error) {
		if got.JobID != job.ID {
			t.Errorf("restore phase Job ID = %q, want %q", got.JobID, job.ID)
		}
		for _, event := range restoreProgressEvents() {
			report(event)
		}
		return localReadyResult(request), nil
	})

	result, err := service.RunRestorePhase(context.Background(), job.ID, request, client)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.ValidateFor(request); err != nil {
		t.Fatalf("restore phase returned untrusted LOCAL_READY evidence: %v", err)
	}
	loaded, err := service.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Kind != KindPlay || loaded.Status != StatusRunning || loaded.Phase != PhaseRestore ||
		loaded.ProgressCurrent != 4 || loaded.ProgressTotal != 4 {
		t.Fatalf("Job after reusable restore phase = %#v; want active PLAY with verified restore progress", loaded)
	}
}

func TestRunRestoreBoundsProgressPersistenceWrites(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository := &progressCountingRepository{Repository: NewSQLiteRepository(db)}
	service := NewService(repository)
	job, err := service.Create(context.Background(), "restore_asset")
	if err != nil {
		t.Fatal(err)
	}
	assetID := domain.AssetID("asset-1")
	request := agent.RestoreAsset{
		JobID: job.ID,
		Asset: domain.Asset{ID: assetID, TotalSizeBytes: 4},
		Parts: []domain.AssetPart{{ID: "part-1", AssetID: assetID, Role: "rom", Filename: "game.iso", SizeBytes: 4}},
		SourceLocation: domain.AssetLocation{
			AssetID: assetID, StorageProviderID: agent.RestoreSourceProviderFixture,
			Locator: "fixtures/game.iso", LocationClass: agent.RestoreSourceClassFixture,
		},
	}
	client := restoreClientFunc(func(_ context.Context, _ agent.RestoreAsset, report func(agent.RestoreProgress)) (agent.RestoreResult, error) {
		for _, event := range restoreProgressEvents()[:3] {
			report(event)
		}
		for range 100 {
			report(agent.RestoreProgress{Phase: agent.RestorePhaseCopying, TotalBytes: 4})
		}
		for _, event := range restoreProgressEvents()[3:] {
			report(event)
		}
		return localReadyResult(request), nil
	})
	if _, err := service.RunRestore(context.Background(), job.ID, request, client); err != nil {
		t.Fatal(err)
	}
	if repository.progressWrites >= 10 {
		t.Fatalf("persisted %d progress updates for 100 duplicate events; expected bounded writes", repository.progressWrites)
	}
}

func TestRunRestoreRejectsStagingEvidenceAndFailsJob(t *testing.T) {
	service, job, request := newRestoreJob(t)
	client := restoreClientFunc(func(_ context.Context, _ agent.RestoreAsset, report func(agent.RestoreProgress)) (agent.RestoreResult, error) {
		for _, event := range restoreProgressEvents() {
			report(event)
		}
		result := localReadyResult(request)
		result.RelativePath = ".staging/" + job.ID + "/game.iso"
		return result, nil
	})
	result, err := service.RunRestore(context.Background(), job.ID, request, client)
	if err == nil {
		t.Fatal("staging path was accepted as LOCAL_READY evidence")
	}
	if result.LocalReady {
		t.Fatalf("failed restore returned LOCAL_READY result: %#v", result)
	}
	loaded, loadErr := service.Get(context.Background(), job.ID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if loaded.Status != StatusFailed || loaded.ErrorCode == "" {
		t.Fatalf("staging evidence did not fail the Job with a stable error code: %#v", loaded)
	}
}

func TestRunRestoreRequiresTerminalAgentProofBeforeSuccess(t *testing.T) {
	service, job, request := newRestoreJob(t)
	client := restoreClientFunc(func(_ context.Context, _ agent.RestoreAsset, report func(agent.RestoreProgress)) (agent.RestoreResult, error) {
		report(agent.RestoreProgress{Phase: agent.RestorePhaseCopying, CurrentBytes: 4, TotalBytes: 4})
		return localReadyResult(request), nil
	})
	result, err := service.RunRestore(context.Background(), job.ID, request, client)
	if !errors.Is(err, ErrInvalidRestoreProof) || result.LocalReady {
		t.Fatalf("RunRestore without terminal proof = %#v, %v", result, err)
	}
	loaded, loadErr := service.Get(context.Background(), job.ID)
	if loadErr != nil || loaded.Status != StatusFailed {
		t.Fatalf("Job without terminal proof = %#v, error = %v", loaded, loadErr)
	}
}

func TestRunRestoreMarksAgentFailureAndCancellationWithoutLocalReady(t *testing.T) {
	t.Run("agent failure", func(t *testing.T) {
		service, job, request := newRestoreJob(t)
		wantErr := errors.New("fixture read failed")
		client := restoreClientFunc(func(context.Context, agent.RestoreAsset, func(agent.RestoreProgress)) (agent.RestoreResult, error) {
			return agent.RestoreResult{}, wantErr
		})
		result, err := service.RunRestore(context.Background(), job.ID, request, client)
		if !errors.Is(err, wantErr) || result.LocalReady {
			t.Fatalf("RunRestore() = %#v, %v; want source error and no ready result", result, err)
		}
		loaded, loadErr := service.Get(context.Background(), job.ID)
		if loadErr != nil || loaded.Status != StatusFailed {
			t.Fatalf("failed Job = %#v, error = %v", loaded, loadErr)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		service, job, request := newRestoreJob(t)
		started := make(chan struct{})
		client := restoreClientFunc(func(ctx context.Context, _ agent.RestoreAsset, report func(agent.RestoreProgress)) (agent.RestoreResult, error) {
			report(restoreProgressEvents()[0])
			close(started)
			<-ctx.Done()
			return agent.RestoreResult{}, ctx.Err()
		})
		ctx, cancel := context.WithCancel(context.Background())
		resultChannel := make(chan struct {
			result agent.RestoreResult
			err    error
		}, 1)
		go func() {
			result, err := service.RunRestore(ctx, job.ID, request, client)
			resultChannel <- struct {
				result agent.RestoreResult
				err    error
			}{result: result, err: err}
		}()
		<-started
		cancel()
		got := <-resultChannel
		if !errors.Is(got.err, context.Canceled) || got.result.LocalReady {
			t.Fatalf("cancelled RunRestore() = %#v, %v", got.result, got.err)
		}
		loaded, loadErr := service.Get(context.Background(), job.ID)
		if loadErr != nil || loaded.Status != StatusInterrupted {
			t.Fatalf("interrupted Job = %#v, error = %v", loaded, loadErr)
		}
	})

	t.Run("cancelled transport error", func(t *testing.T) {
		service, job, request := newRestoreJob(t)
		ctx, cancel := context.WithCancel(context.Background())
		client := restoreClientFunc(func(_ context.Context, _ agent.RestoreAsset, report func(agent.RestoreProgress)) (agent.RestoreResult, error) {
			report(restoreProgressEvents()[0])
			cancel()
			return agent.RestoreResult{}, errors.New("socket closed after cancellation")
		})
		result, err := service.RunRestore(ctx, job.ID, request, client)
		if !errors.Is(err, context.Canceled) || result.LocalReady {
			t.Fatalf("cancelled transport RunRestore() = %#v, %v", result, err)
		}
		loaded, loadErr := service.Get(context.Background(), job.ID)
		if loadErr != nil || loaded.Status != StatusInterrupted {
			t.Fatalf("cancelled transport Job = %#v, error = %v", loaded, loadErr)
		}
	})

	t.Run("deadline transport error", func(t *testing.T) {
		service, job, request := newRestoreJob(t)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		client := restoreClientFunc(func(ctx context.Context, _ agent.RestoreAsset, report func(agent.RestoreProgress)) (agent.RestoreResult, error) {
			report(restoreProgressEvents()[0])
			<-ctx.Done()
			return agent.RestoreResult{}, errors.New("transport failed after caller deadline")
		})
		result, err := service.RunRestore(ctx, job.ID, request, client)
		if !errors.Is(err, context.DeadlineExceeded) || result.LocalReady {
			t.Fatalf("deadline transport restore = %#v, %v; want interrupted deadline without readiness", result, err)
		}
		loaded, loadErr := service.Get(context.Background(), job.ID)
		if loadErr != nil || loaded.Status != StatusInterrupted || loaded.ErrorCode != "restore_interrupted" {
			t.Fatalf("deadline transport Job = %#v, error = %v; want interrupted outcome", loaded, loadErr)
		}
	})
}

func TestRunRestoreRecordsInterruptionWhenCancelledBeforeRestorePhaseValidation(t *testing.T) {
	service, job, request := newRestoreJob(t)
	ctx, cancel := context.WithCancel(context.Background())
	service.repository = &cancelAfterRunningRepository{Repository: service.repository, cancel: cancel}
	client := restoreClientFunc(func(context.Context, agent.RestoreAsset, func(agent.RestoreProgress)) (agent.RestoreResult, error) {
		t.Fatal("Agent client ran after cancellation between Job start and restore phase validation")
		return agent.RestoreResult{}, nil
	})

	result, err := service.RunRestore(ctx, job.ID, request, client)
	if !errors.Is(err, context.Canceled) || result.LocalReady {
		t.Fatalf("cancelled pre-phase RunRestore() = %#v, %v; want interrupted cancellation without readiness", result, err)
	}
	loaded, loadErr := service.Get(context.Background(), job.ID)
	if loadErr != nil || loaded.Status != StatusInterrupted || loaded.ErrorCode != "restore_interrupted" {
		t.Fatalf("Job after cancellation before restore phase = %#v, error = %v; want interrupted outcome", loaded, loadErr)
	}
}

func TestRunRestoreRejectsRegressingAgentProgress(t *testing.T) {
	service, job, request := newRestoreJob(t)
	client := restoreClientFunc(func(ctx context.Context, _ agent.RestoreAsset, report func(agent.RestoreProgress)) (agent.RestoreResult, error) {
		report(agent.RestoreProgress{Phase: agent.RestorePhaseValidating, TotalBytes: 4})
		report(agent.RestoreProgress{Phase: agent.RestorePhaseCopying, CurrentBytes: 3, TotalBytes: 4})
		report(agent.RestoreProgress{Phase: agent.RestorePhaseCopying, CurrentBytes: 2, TotalBytes: 4})
		<-ctx.Done()
		return agent.RestoreResult{}, ctx.Err()
	})
	result, err := service.RunRestore(context.Background(), job.ID, request, client)
	if err == nil || result.LocalReady {
		t.Fatalf("regressing progress RunRestore() = %#v, %v", result, err)
	}
	loaded, loadErr := service.Get(context.Background(), job.ID)
	if loadErr != nil || loaded.Status != StatusFailed {
		t.Fatalf("invalid-progress Job = %#v, error = %v", loaded, loadErr)
	}
}
