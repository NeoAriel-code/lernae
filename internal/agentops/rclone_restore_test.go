package agentops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lernae/internal/agent"
	"lernae/internal/domain"
)

func TestRcloneRestoreExecutorUsesAssetLocationAndVerifiedRestorePipeline(t *testing.T) {
	const expectedBytes = int64(1 << 20)
	const locator = "archive:roms/--data=not-a-flag.iso"
	cachePath := t.TempDir()
	cache, err := OpenCache(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cache.Close(); err != nil {
			t.Errorf("close cache: %v", err)
		}
	})
	copyArgsFile, _ := installRcloneTestHelper(t, "restore-success", expectedBytes)
	statArgsFile := filepath.Join(t.TempDir(), "stat-args.json")
	t.Setenv("LERNAE_TEST_RCLONE_STAT_ARGS_FILE", statArgsFile)
	executor, err := NewRcloneRestoreExecutor(cache)
	if err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(t.TempDir(), "agent.sock")
	server, err := agent.ListenUDS(socketPath, executor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close Agent UDS server: %v", err)
		}
	})
	go func() { _ = server.Serve() }()

	assetID := domain.AssetID("asset-restore")
	request := agent.RestoreAsset{
		JobID: "job-rclone",
		Asset: domain.Asset{ID: assetID, TotalSizeBytes: expectedBytes},
		Parts: []domain.AssetPart{{ID: "part-rclone", AssetID: assetID, Role: "rom", Filename: "game.iso", SizeBytes: expectedBytes}},
		SourceLocation: domain.AssetLocation{
			AssetID: assetID, StorageProviderID: "rclone", Locator: locator, LocationClass: "archive",
		},
	}
	var progress []agent.RestoreProgress
	result, err := (agent.UDSClient{SocketPath: socketPath, Timeout: time.Second}).RestoreAsset(context.Background(), request, func(event agent.RestoreProgress) {
		progress = append(progress, event)
	})
	if err != nil {
		t.Fatalf("UDS RestoreAsset() error = %v", err)
	}
	if err := result.ValidateFor(request); err != nil {
		t.Fatalf("restore result did not prove local readiness: %v", err)
	}
	if len(progress) < 5 || progress[0].Phase != agent.RestorePhaseValidating || progress[len(progress)-1].Phase != agent.RestorePhaseComplete {
		t.Fatalf("restore progress = %#v", progress)
	}
	for index, event := range progress {
		if err := agent.ValidateProgressAfter(progressAt(progress, index-1), event); err != nil {
			t.Fatalf("restore progress is not monotonic: %v; events=%#v", err, progress)
		}
		if event.Phase == agent.RestorePhaseCopying && event.CurrentBytes >= expectedBytes {
			t.Fatalf("copy progress reached total before P1-05A verification: %#v", event)
		}
	}
	wantStatArgs := []string{"lsjson", "--stat", locator}
	if got := readRcloneTestArgs(t, statArgsFile); !equalStrings(got, wantStatArgs) {
		t.Fatalf("stat argv = %#v, want %#v", got, wantStatArgs)
	}
	wantCopyArgs := []string{"copyto", "--inplace", locator, filepath.Join(cachePath, ".staging", request.JobID, "game.iso")}
	if got := readRcloneTestArgs(t, copyArgsFile); !equalStrings(got, wantCopyArgs) {
		t.Fatalf("copy argv = %#v, want %#v", got, wantCopyArgs)
	}
	finalPath := filepath.Join(cachePath, filepath.FromSlash(result.RelativePath))
	finalInfo, err := os.Lstat(finalPath)
	if err != nil || finalInfo.Size() != expectedBytes {
		t.Fatalf("promoted file = %#v, error = %v, want %d bytes", finalInfo, err, expectedBytes)
	}
	if _, err := os.Lstat(filepath.Join(cachePath, ".staging", request.JobID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned staging subtree remains after promotion: %v", err)
	}
}

func TestRcloneRestoreExecutorRejectsStatMismatchBeforeCopyOrStaging(t *testing.T) {
	const expectedBytes = int64(1024)
	cachePath := t.TempDir()
	cache, err := OpenCache(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	copyArgsFile, _ := installRcloneTestHelper(t, "restore-size-mismatch", expectedBytes)
	statArgsFile := filepath.Join(t.TempDir(), "stat-args.json")
	t.Setenv("LERNAE_TEST_RCLONE_STAT_ARGS_FILE", statArgsFile)
	executor, err := NewRcloneRestoreExecutor(cache)
	if err != nil {
		t.Fatal(err)
	}
	request := rcloneRestoreRequest("job-stat-mismatch", expectedBytes)
	_, err = executor.Restore(context.Background(), request, nil)
	assertRcloneCategory(t, err, RcloneFailureSizeMismatch)
	if _, err := os.Lstat(filepath.Join(cachePath, ".staging", request.JobID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat mismatch created staging before copy: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(cachePath, "assets", string(request.Asset.ID), request.Parts[0].Filename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat mismatch created final cache asset: %v", err)
	}
	if _, err := os.Lstat(copyArgsFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat mismatch unexpectedly invoked copyto: %v", err)
	}
	if got := readRcloneTestArgs(t, statArgsFile); !equalStrings(got, []string{"lsjson", "--stat", request.SourceLocation.Locator}) {
		t.Fatalf("stat argv = %#v", got)
	}
}

func rcloneRestoreRequest(jobID string, expectedBytes int64) agent.RestoreAsset {
	assetID := domain.AssetID("asset-rclone")
	return agent.RestoreAsset{
		JobID: jobID,
		Asset: domain.Asset{ID: assetID, TotalSizeBytes: expectedBytes},
		Parts: []domain.AssetPart{{ID: "part-rclone", AssetID: assetID, Role: "rom", Filename: "game.iso", SizeBytes: expectedBytes}},
		SourceLocation: domain.AssetLocation{
			AssetID: assetID, StorageProviderID: "rclone", Locator: "archive:roms/game.iso", LocationClass: "archive",
		},
	}
}

func progressAt(progress []agent.RestoreProgress, index int) *agent.RestoreProgress {
	if index < 0 {
		return nil
	}
	return &progress[index]
}
