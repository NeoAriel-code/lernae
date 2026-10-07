package playcoordinator

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"lernae/internal/agent"
	"lernae/internal/agentops"
	"lernae/internal/domain"
	"lernae/internal/inventory"
	"lernae/internal/jobs"
)

func TestIsolatedPlayQuarantinesStaleCacheAndRestoresOnceFromFixture(t *testing.T) {
	root := t.TempDir()
	fixtureRoot := filepath.Join(root, "archive-fixture")
	fixturePath := filepath.Join(fixtureRoot, "fixtures", playSmokeFilename)
	if err := os.MkdirAll(filepath.Dir(fixturePath), 0o700); err != nil {
		t.Fatal(err)
	}
	archived := []byte("deterministic archived fixture bytes")
	if err := os.WriteFile(fixturePath, archived, 0o600); err != nil {
		t.Fatal(err)
	}

	identity, graph := playSmokeCatalogGraph(int64(len(archived)))
	graph.Locations = append(graph.Locations, domain.AssetLocation{
		ID: "location-p107-stale-cache", AssetID: graph.Assets[0].ID,
		StorageProviderID: "filesystem", Locator: "assets/" + string(graph.Assets[0].ID) + "/" + playSmokeFilename,
		LocationClass: "local_cache",
	})
	manifestPath := filepath.Join(root, "inventory.json")
	if err := writePlaySmokeManifest(manifestPath, identity, graph, int64(len(archived))); err != nil {
		t.Fatal(err)
	}
	manifestSource := inventory.NewManifestInventorySource(manifestPath)

	cachePath := filepath.Join(root, "agent-cache")
	cache, err := agentops.OpenCache(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cache.Close(); err != nil {
			t.Errorf("close isolated test cache: %v", err)
		}
	})
	stale := []byte("stale wrong-sized local cache bytes")
	finalPath := filepath.Join(cachePath, "assets", string(graph.Assets[0].ID), playSmokeFilename)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalPath, stale, 0o600); err != nil {
		t.Fatal(err)
	}

	fixtureRestorer, err := agentops.NewLocalFixtureExecutor(fixtureRoot, cache)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := fixtureRestorer.Close(); err != nil {
			t.Errorf("close isolated fixture restore root: %v", err)
		}
	})
	launchExecutor := &quarantineTestLaunchExecutor{cache: cache}
	server, err := agent.ListenUDSWithExecutors(filepath.Join(root, "agent.sock"),
		fixtureBackedArchiveRestorer{fixture: fixtureRestorer, descriptor: "fixtures/" + playSmokeFilename},
		launchExecutor)
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close isolated Agent UDS: %v", err)
		}
		if err := <-serveDone; err != nil {
			t.Errorf("Agent UDS Serve() after close: %v", err)
		}
	})
	client := &playSmokeCountingAgentClient{client: agent.UDSClient{SocketPath: filepath.Join(root, "agent.sock")}}
	services := openPlaySmokeServices(t, filepath.Join(root, "lernae.db"), manifestSource, client, graph)
	accepted := startPlaySmoke(t, services, identity)
	job := waitForPlaySmokeJob(t, services.jobs, accepted.JobID)
	if job.Status != jobs.StatusSucceeded || job.Phase != jobs.PhasePlaying || job.SessionID == "" {
		t.Fatalf("stale-cache fallback Job = %#v, Agent launch error=%v", job, client.lastLaunchError())
	}
	if got := client.counts(); got.launch != 2 || got.restore != 1 {
		t.Fatalf("Agent PLAY requests = launch %d / restore %d, want 2 / 1", got.launch, got.restore)
	}
	if got := len(client.restoreSnapshot()); got != 1 {
		t.Fatalf("restore request history = %d, want one bounded fallback restore", got)
	}
	if launchExecutor.successfulLaunches() != 1 || launchExecutor.rejectedSessionID() == "" {
		t.Fatalf("launch outcomes = successful %d / rejected session %q, want one success after a correlated stale rejection",
			launchExecutor.successfulLaunches(), launchExecutor.rejectedSessionID())
	}
	quarantinedPath := filepath.Join(cachePath, ".quarantine", launchExecutor.rejectedSessionID(), string(graph.Assets[0].ID), playSmokeFilename)
	if got, err := os.ReadFile(quarantinedPath); err != nil || !bytes.Equal(got, stale) {
		t.Fatalf("quarantined cache bytes = %q, err=%v; want original stale bytes", got, err)
	}
	if got, err := os.ReadFile(finalPath); err != nil || !bytes.Equal(got, archived) {
		t.Fatalf("restored final bytes = %q, err=%v; want exact verified fixture", got, err)
	}
	if got, err := os.ReadFile(fixturePath); err != nil || !bytes.Equal(got, archived) {
		t.Fatalf("archived source changed during restore: bytes=%q err=%v", got, err)
	}
	ready, err := cache.OpenReadyGameCubeImageForSession("session-after-restore", graph.Work, graph.Editions[0], graph.Assets[0], graph.Parts)
	if err != nil {
		t.Fatalf("Agent did not accept the promoted exact-size image: %v", err)
	}
	_ = ready.Close()
	if err := services.close(); err != nil {
		t.Fatal(err)
	}
}

type fixtureBackedArchiveRestorer struct {
	fixture    *agentops.LocalFixtureExecutor
	descriptor string
}

func (executor fixtureBackedArchiveRestorer) Restore(ctx context.Context, request agent.RestoreAsset,
	report func(agent.RestoreProgress) error,
) (agent.RestoreResult, error) {
	if request.SourceLocation.StorageProviderID != "rclone" || request.SourceLocation.LocationClass != "archive" {
		return agent.RestoreResult{}, errors.New("test restore expected a catalog archive source")
	}
	fixtureRequest := request
	fixtureRequest.SourceLocation = domain.AssetLocation{
		AssetID: request.Asset.ID, StorageProviderID: agent.RestoreSourceProviderFixture,
		Locator: executor.descriptor, LocationClass: agent.RestoreSourceClassFixture,
	}
	return executor.fixture.Restore(ctx, fixtureRequest, report)
}

type quarantineTestLaunchExecutor struct {
	cache           *agentops.Cache
	mu              sync.Mutex
	rejectedSession string
	successes       int
}

func (executor *quarantineTestLaunchExecutor) Start(_ context.Context, request agent.LaunchAsset) (agent.LaunchProcess, error) {
	if executor == nil || executor.cache == nil || request.Validate() != nil {
		return nil, errors.New("invalid GameCube launch request")
	}
	work, edition, asset, parts := request.DomainRecords()
	image, err := executor.cache.OpenReadyGameCubeImageForSession(string(request.SessionID), work, edition, asset, parts)
	if errors.Is(err, agentops.ErrLocalReadyImageInvalid) {
		executor.mu.Lock()
		executor.rejectedSession = string(request.SessionID)
		executor.mu.Unlock()
		return nil, agent.ErrLaunchLocalCacheInvalid
	}
	if err != nil {
		return nil, err
	}
	_ = image.Close()
	executor.mu.Lock()
	executor.successes++
	executor.mu.Unlock()
	return quarantineTestLaunchProcess{}, nil
}

func (executor *quarantineTestLaunchExecutor) rejectedSessionID() string {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.rejectedSession
}

func (executor *quarantineTestLaunchExecutor) successfulLaunches() int {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.successes
}

type quarantineTestLaunchProcess struct{}

func (quarantineTestLaunchProcess) Wait() agent.LaunchExit {
	zero := 0
	return agent.LaunchExit{Outcome: domain.SessionOutcomeNormalExit, ExitCode: &zero}
}
