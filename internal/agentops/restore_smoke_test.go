package agentops

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"lernae/internal/agent"
	"lernae/internal/database"
	"lernae/internal/domain"
	"lernae/internal/jobs"
)

const restoreSmokeEnv = "LERNAE_P105A_SMOKE"

type cancelOnCopyClient struct {
	client jobs.RestoreClient
	cancel context.CancelFunc
	bytes  int64
}

func (client *cancelOnCopyClient) RestoreAsset(ctx context.Context, request agent.RestoreAsset, onProgress func(agent.RestoreProgress)) (agent.RestoreResult, error) {
	return client.client.RestoreAsset(ctx, request, func(progress agent.RestoreProgress) {
		onProgress(progress)
		if progress.Phase == agent.RestorePhaseCopying && progress.CurrentBytes > 0 && progress.CurrentBytes < progress.TotalBytes {
			client.bytes = progress.CurrentBytes
			client.cancel()
		}
	})
}

func TestP105AAgentRestoreSmoke(t *testing.T) {
	if os.Getenv(restoreSmokeEnv) != "1" {
		t.Skipf("set %s=1 to run the isolated /tmp restore smoke", restoreSmokeEnv)
	}
	smokeRoot, err := os.MkdirTemp("/tmp", "lernae-p1-05a-smoke-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(smokeRoot); err != nil {
			t.Errorf("remove only owned smoke directory %s: %v", smokeRoot, err)
		}
	}()

	sourceRoot := filepath.Join(smokeRoot, "source")
	fixtureDir := filepath.Join(sourceRoot, "fixtures")
	cacheDir := filepath.Join(smokeRoot, "cache")
	socketDir := filepath.Join(smokeRoot, "agent")
	if err := os.MkdirAll(fixtureDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		t.Fatal(err)
	}

	successBytes := bytes.Repeat([]byte("local-smoke\n"), 64)
	if err := os.WriteFile(filepath.Join(fixtureDir, "tiny.iso"), successBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	cancelBytes := make([]byte, 8*1024*1024)
	for index := range cancelBytes {
		cancelBytes[index] = byte(index % 251)
	}
	if err := os.WriteFile(filepath.Join(fixtureDir, "cancel.bin"), cancelBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	cache, err := OpenCache(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cache.Close(); err != nil {
			t.Errorf("close smoke cache: %v", err)
		}
	}()
	executor, err := NewLocalFixtureExecutor(sourceRoot, cache)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := executor.Close(); err != nil {
			t.Errorf("close smoke fixture root: %v", err)
		}
	}()

	socketPath := filepath.Join(socketDir, "agent.sock")
	server, err := agent.ListenUDS(socketPath, executor)
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	defer func() {
		if err := server.Close(); err != nil {
			t.Errorf("close smoke Agent: %v", err)
		}
		if err := <-serveDone; err != nil {
			t.Errorf("smoke Agent Serve(): %v", err)
		}
	}()

	db, err := database.Open(context.Background(), filepath.Join(smokeRoot, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close smoke Job database: %v", err)
		}
	}()
	jobService := jobs.NewService(jobs.NewSQLiteRepository(db))
	client := agent.UDSClient{SocketPath: socketPath}

	readyJob, err := jobService.Create(context.Background(), "restore_asset")
	if err != nil {
		t.Fatal(err)
	}
	readyRequest := smokeRestoreRequest(readyJob.ID, "asset-smoke-ready", "tiny.iso", "fixtures/tiny.iso", int64(len(successBytes)))
	ready, err := jobService.RunRestore(context.Background(), readyJob.ID, readyRequest, client)
	if err != nil {
		t.Fatal(err)
	}
	if err := ready.ValidateFor(readyRequest); err != nil {
		t.Fatalf("smoke result does not prove local readiness: %v", err)
	}
	readyPath := filepath.Join(cacheDir, filepath.FromSlash(ready.RelativePath))
	readyInfo, err := os.Lstat(readyPath)
	if err != nil || !readyInfo.Mode().IsRegular() {
		t.Fatalf("promoted smoke target must be a regular file: info=%v err=%v", readyInfo, err)
	}
	contents, err := os.ReadFile(readyPath)
	if err != nil || !bytes.Equal(contents, successBytes) {
		t.Fatalf("promoted smoke content mismatch: bytes=%d err=%v", len(contents), err)
	}
	readyJobResult, err := jobService.Get(context.Background(), readyJob.ID)
	if err != nil || readyJobResult.Status != jobs.StatusSucceeded {
		t.Fatalf("successful smoke Job = %#v, err=%v", readyJobResult, err)
	}

	cancelJob, err := jobService.Create(context.Background(), "restore_asset")
	if err != nil {
		t.Fatal(err)
	}
	cancelRequest := smokeRestoreRequest(cancelJob.ID, "asset-smoke-cancel", "cancel.bin", "fixtures/cancel.bin", int64(len(cancelBytes)))
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancelingClient := &cancelOnCopyClient{client: client, cancel: cancel}
	cancelResult, cancelErr := jobService.RunRestore(cancelCtx, cancelJob.ID, cancelRequest, cancelingClient)
	cancel()
	if !errors.Is(cancelErr, context.Canceled) || cancelResult.LocalReady {
		t.Fatalf("cancelled smoke restore = %#v, err=%v; want context cancellation without readiness", cancelResult, cancelErr)
	}
	if cancelingClient.bytes <= 0 || cancelingClient.bytes >= int64(len(cancelBytes)) {
		t.Fatalf("smoke cancellation did not interrupt partial copy: %d/%d", cancelingClient.bytes, len(cancelBytes))
	}
	cancelFinal := filepath.Join(cacheDir, "assets", "asset-smoke-cancel", "cancel.bin")
	if _, err := os.Lstat(cancelFinal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled smoke left a final target %s: %v", cancelFinal, err)
	}
	if _, err := os.Lstat(filepath.Join(cacheDir, ".staging", cancelJob.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled smoke left its owned staging subtree: %v", err)
	}
	cancelJobResult, err := jobService.Get(context.Background(), cancelJob.ID)
	if err != nil || cancelJobResult.Status != jobs.StatusInterrupted {
		t.Fatalf("cancelled smoke Job = %#v, err=%v", cancelJobResult, err)
	}

	t.Logf("SMOKE_ROOT=%s\nSMOKE_SOURCE=%s\nSMOKE_CACHE=%s\nSMOKE_SOCKET=%s\nSMOKE_DATABASE=%s\nSMOKE_READY_FILE=%s\nSMOKE_CANCEL_SOURCE=%s\nSMOKE_CANCEL_BYTES=%d/%d\nSMOKE_CANCEL_FINAL_ABSENT=%s\nSMOKE_CANCEL_STAGING_ABSENT=%s",
		smokeRoot, filepath.Join(fixtureDir, "tiny.iso"), cacheDir, socketPath,
		filepath.Join(smokeRoot, "jobs.db"), readyPath, filepath.Join(fixtureDir, "cancel.bin"),
		cancelingClient.bytes, len(cancelBytes), cancelFinal, filepath.Join(cacheDir, ".staging", cancelJob.ID))
}

func smokeRestoreRequest(jobID string, assetID domain.AssetID, filename, descriptor string, size int64) agent.RestoreAsset {
	return agent.RestoreAsset{
		JobID: jobID,
		Asset: domain.Asset{ID: assetID, TotalSizeBytes: size},
		Parts: []domain.AssetPart{{ID: domain.AssetPartID("part-" + string(assetID)), AssetID: assetID, Role: "rom", Filename: filename, SizeBytes: size}},
		SourceLocation: domain.AssetLocation{
			AssetID: assetID, StorageProviderID: agent.RestoreSourceProviderFixture,
			Locator: descriptor, LocationClass: agent.RestoreSourceClassFixture,
		},
	}
}
