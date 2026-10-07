package agentops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"lernae/internal/agent"
	"lernae/internal/domain"
)

func fixtureRestoreRequest(jobID, sourceDescriptor string, size int64) agent.RestoreAsset {
	assetID := domain.AssetID("asset-1")
	return agent.RestoreAsset{
		JobID: jobID,
		Asset: domain.Asset{ID: assetID, TotalSizeBytes: size},
		Parts: []domain.AssetPart{{ID: "part-1", AssetID: assetID, Role: "rom", Filename: "game.iso", SizeBytes: size}},
		SourceLocation: domain.AssetLocation{
			AssetID: assetID, StorageProviderID: agent.RestoreSourceProviderFixture,
			Locator: sourceDescriptor, LocationClass: agent.RestoreSourceClassFixture,
		},
	}
}

func TestLocalFixtureRestoreCopiesVerifiesAndPromotesOneROM(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "fixtures"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "fixtures", "game.iso"), []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	cache, err := OpenCache(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	progress := make([]agent.RestoreProgress, 0)
	executor, err := NewLocalFixtureExecutor(root, cache)
	if err != nil {
		t.Fatal(err)
	}
	request := fixtureRestoreRequest("job-1", "fixtures/game.iso", 4)
	result, err := executor.Restore(context.Background(), request, func(event agent.RestoreProgress) error {
		progress = append(progress, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := result.ValidateFor(request); err != nil {
		t.Fatalf("restore result did not prove local readiness: %v", err)
	}
	if len(progress) < 4 || progress[0].Phase != agent.RestorePhaseValidating || progress[len(progress)-1].Phase != agent.RestorePhaseComplete {
		t.Fatalf("restore progress = %#v", progress)
	}
	for index := 1; index < len(progress); index++ {
		if progress[index].CurrentBytes < progress[index-1].CurrentBytes || progress[index].TotalBytes != 4 {
			t.Fatalf("restore progress is not monotonic/bounded: %#v", progress)
		}
	}
	finalPath := filepath.Join(cacheDir, filepath.FromSlash(result.RelativePath))
	data, err := os.ReadFile(finalPath)
	if err != nil || string(data) != "safe" {
		t.Fatalf("final content = %q, error = %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(cacheDir, ".staging", request.JobID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Job staging subtree remains: %v", err)
	}
}

func TestRestoreProgressReachesTotalOnlyAfterPromotion(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "game.iso"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	cache, err := OpenCache(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	executor, err := NewLocalFixtureExecutor(root, cache)
	if err != nil {
		t.Fatal(err)
	}
	request := fixtureRestoreRequest("job-progress-total-one", "game.iso", 1)
	var events []agent.RestoreProgress
	result, err := executor.Restore(context.Background(), request, func(event agent.RestoreProgress) error {
		events = append(events, event)
		if event.Phase == agent.RestorePhaseComplete {
			finalPath := filepath.Join(cacheDir, "assets", string(request.Asset.ID), request.Parts[0].Filename)
			data, readErr := os.ReadFile(finalPath)
			if readErr != nil || string(data) != "x" {
				return errors.New("complete progress preceded successful promotion")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := result.ValidateFor(request); err != nil {
		t.Fatalf("restore result did not prove local readiness: %v", err)
	}
	wantPhases := []agent.RestorePhase{
		agent.RestorePhaseValidating,
		agent.RestorePhaseStaging,
		agent.RestorePhaseCopying,
		agent.RestorePhaseVerifying,
		agent.RestorePhasePromoting,
		agent.RestorePhaseComplete,
	}
	phaseSequence := make([]agent.RestorePhase, 0, len(wantPhases))
	terminalBeforeComplete := make([]agent.RestoreProgress, 0)
	for index, event := range events {
		if index == 0 || event.Phase != events[index-1].Phase {
			phaseSequence = append(phaseSequence, event.Phase)
		}
		if event.TotalBytes != 1 {
			t.Fatalf("restore phase %q total = %d, want 1", event.Phase, event.TotalBytes)
		}
		if index > 0 && event.CurrentBytes < events[index-1].CurrentBytes {
			t.Fatalf("restore progress regressed: %#v", events)
		}
		if event.Phase == agent.RestorePhaseComplete {
			if event.CurrentBytes != event.TotalBytes {
				t.Fatalf("complete progress = %#v, want exact total", event)
			}
		} else if event.CurrentBytes >= event.TotalBytes {
			terminalBeforeComplete = append(terminalBeforeComplete, event)
		}
	}
	if len(terminalBeforeComplete) > 0 {
		t.Fatalf("nonterminal progress reached total before promotion: %#v", terminalBeforeComplete)
	}
	if len(phaseSequence) != len(wantPhases) {
		t.Fatalf("restore phase sequence = %#v, want %#v; events: %#v", phaseSequence, wantPhases, events)
	}
	for index, phase := range phaseSequence {
		if phase != wantPhases[index] {
			t.Fatalf("restore phase sequence = %#v, want %#v", phaseSequence, wantPhases)
		}
	}
}

func TestRestoreFailureAndCancellationNeverReportTotalOrLocalReady(t *testing.T) {
	tests := []struct {
		name                 string
		cancelPhase          agent.RestorePhase
		createFinalOnPromote bool
	}{
		{name: "promotion failure preserves raced final", createFinalOnPromote: true},
		{name: "cancellation during verification", cancelPhase: agent.RestorePhaseVerifying},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "game.iso"), []byte("safe"), 0o600); err != nil {
				t.Fatal(err)
			}
			cacheDir := t.TempDir()
			cache, err := OpenCache(cacheDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cache.Close() })
			executor, err := NewLocalFixtureExecutor(root, cache)
			if err != nil {
				t.Fatal(err)
			}
			request := fixtureRestoreRequest("job-progress-failure", "game.iso", 4)
			finalPath := filepath.Join(cacheDir, "assets", string(request.Asset.ID), request.Parts[0].Filename)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var events []agent.RestoreProgress
			result, restoreErr := executor.Restore(ctx, request, func(event agent.RestoreProgress) error {
				events = append(events, event)
				if test.cancelPhase != "" && event.Phase == test.cancelPhase {
					cancel()
				}
				if test.createFinalOnPromote && event.Phase == agent.RestorePhasePromoting {
					file, openErr := os.OpenFile(finalPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
					if openErr != nil {
						return openErr
					}
					if _, writeErr := file.Write([]byte("keep")); writeErr != nil {
						_ = file.Close()
						return writeErr
					}
					if closeErr := file.Close(); closeErr != nil {
						return closeErr
					}
				}
				return nil
			})
			if restoreErr == nil {
				t.Fatal("restore unexpectedly succeeded")
			}
			if result.LocalReady || result.AssetID != "" {
				t.Fatalf("failed restore returned local-ready evidence: %#v", result)
			}
			terminalEvents := make([]agent.RestoreProgress, 0)
			for _, event := range events {
				if event.Phase == agent.RestorePhaseComplete || event.CurrentBytes == event.TotalBytes {
					terminalEvents = append(terminalEvents, event)
				}
			}
			if len(terminalEvents) > 0 {
				t.Fatalf("failed restore reported terminal total: %#v", terminalEvents)
			}
			if test.cancelPhase != "" && !errors.Is(restoreErr, context.Canceled) {
				t.Fatalf("cancelled restore error = %v, want context.Canceled", restoreErr)
			}
			if test.createFinalOnPromote {
				data, readErr := os.ReadFile(finalPath)
				if readErr != nil || string(data) != "keep" {
					t.Fatalf("promotion failure changed raced final: %q, %v", data, readErr)
				}
			} else if _, statErr := os.Lstat(finalPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("cancelled restore left a final ready file: %v", statErr)
			}
			if _, statErr := os.Lstat(filepath.Join(cacheDir, ".staging", request.JobID)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed restore left staging bytes: %v", statErr)
			}
		})
	}
}

func TestLocalFixtureRestoreRejectsUnsupportedAndUnsafeRequests(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "game.iso"), []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache, err := OpenCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	executor, err := NewLocalFixtureExecutor(root, cache)
	if err != nil {
		t.Fatal(err)
	}
	request := fixtureRestoreRequest("job-1", "game.iso", 4)
	request.Parts = append(request.Parts, request.Parts[0])
	if _, err := executor.Restore(context.Background(), request, nil); !errors.Is(err, agent.ErrUnsupportedAssetShape) {
		t.Fatalf("multi-part restore error = %v", err)
	}
	request = fixtureRestoreRequest("job-2", "../outside.iso", 4)
	if _, err := executor.Restore(context.Background(), request, nil); err == nil {
		t.Fatal("path-traversing fixture descriptor unexpectedly restored")
	}
	if _, err := os.Lstat(filepath.Join(cache.path, "assets", "asset-1", "game.iso")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid request created a final file: %v", err)
	}
}

func TestLocalFixtureRestoreRejectsSymlinkedSourceFile(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.iso")
	if err := os.WriteFile(outside, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "game.iso")); err != nil {
		t.Fatal(err)
	}
	cache, err := OpenCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	executor, err := NewLocalFixtureExecutor(root, cache)
	if err != nil {
		t.Fatal(err)
	}
	request := fixtureRestoreRequest("job-1", "game.iso", 4)
	if _, err := executor.Restore(context.Background(), request, nil); err == nil {
		t.Fatal("symlinked source fixture unexpectedly restored")
	}
	if _, err := os.Lstat(filepath.Join(cache.path, "assets", "asset-1", "game.iso")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlinked source created a final file: %v", err)
	}
}

func TestLocalFixtureRestoreCancellationCleansPartialStagingWithoutReadyFile(t *testing.T) {
	root := t.TempDir()
	source := make([]byte, 4*1024*1024)
	for index := range source {
		source[index] = byte(index % 251)
	}
	if err := os.WriteFile(filepath.Join(root, "game.iso"), source, 0o600); err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	cache, err := OpenCache(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	executor, err := NewLocalFixtureExecutor(root, cache)
	if err != nil {
		t.Fatal(err)
	}
	request := fixtureRestoreRequest("job-cancel", "game.iso", int64(len(source)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []agent.RestoreProgress
	_, err = executor.Restore(ctx, request, func(event agent.RestoreProgress) error {
		events = append(events, event)
		if event.Phase == agent.RestorePhaseCopying && event.CurrentBytes > 0 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled restore error = %v, want context.Canceled", err)
	}
	if events[len(events)-1].Phase != agent.RestorePhaseCopying {
		t.Fatalf("last progress = %#v, want interrupted copy", events[len(events)-1])
	}
	if events[len(events)-1].CurrentBytes >= events[len(events)-1].TotalBytes {
		t.Fatalf("cancellation occurred only after the full source was copied: %#v", events[len(events)-1])
	}
	if _, err := os.Lstat(filepath.Join(cacheDir, "assets", "asset-1", "game.iso")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled restore left a final ready file: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(cacheDir, ".staging", request.JobID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled restore left staging bytes: %v", err)
	}
}

func TestLocalFixtureRestoreRejectsSizeMismatchAndProgressCallbackFailure(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "game.iso")
	if err := os.WriteFile(sourcePath, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	cache, err := OpenCache(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	executor, err := NewLocalFixtureExecutor(root, cache)
	if err != nil {
		t.Fatal(err)
	}
	request := fixtureRestoreRequest("job-size", "game.iso", 5)
	if _, err := executor.Restore(context.Background(), request, nil); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("source size mismatch error = %v", err)
	}
	request = fixtureRestoreRequest("job-progress", "game.iso", 4)
	wantProgressErr := errors.New("progress transport failed")
	if _, err := executor.Restore(context.Background(), request, func(agent.RestoreProgress) error { return wantProgressErr }); !errors.Is(err, wantProgressErr) {
		t.Fatalf("progress callback error = %v, want %v", err, wantProgressErr)
	}
	if _, err := os.Lstat(filepath.Join(cacheDir, "assets", "asset-1", "game.iso")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed restore created a final file: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(cacheDir, ".staging", request.JobID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed restore left staging bytes: %v", err)
	}
}

func TestLocalFixtureRootMustBeRealDirectory(t *testing.T) {
	actual := t.TempDir()
	configured := filepath.Join(t.TempDir(), "fixture-root")
	if err := os.Symlink(actual, configured); err != nil {
		t.Fatal(err)
	}
	cache, err := OpenCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	if _, err := NewLocalFixtureExecutor(configured, cache); err == nil {
		t.Fatal("symlinked fixture source root unexpectedly accepted")
	}
}
