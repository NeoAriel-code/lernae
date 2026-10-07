package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lernae/internal/agent"
	"lernae/internal/catalog"
	"lernae/internal/config"
	"lernae/internal/database"
	"lernae/internal/domain"
	"lernae/internal/inventory"
	"lernae/internal/jobs"
	"lernae/internal/playcoordinator"
	"lernae/internal/providers"
)

const serverSourceSelectionRomMToken = "synthetic-romm-token"

func TestServerInventorySourceDefaultsToManifestAndDoesNotContactOptionalRomM(t *testing.T) {
	manifestPath := writeServerSourceSelectionManifest(t)
	var rommRequests atomic.Int32
	rommServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rommRequests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer rommServer.Close()
	settings := config.Server{
		InventoryManifestPath: manifestPath,
		RomM:                  config.RomMCredentials{BaseURL: rommServer.URL, ClientAPIToken: serverSourceSelectionRomMToken},
	}

	sources, err := newServerInventorySources(settings)
	if err != nil {
		t.Fatalf("newServerInventorySources() error = %v", err)
	}
	manifestSource, ok := sources.inventory.(*inventory.ManifestInventorySource)
	if !ok || sources.storage != manifestSource {
		t.Fatalf("default source pair = %T/%T, want one shared Manifest inventory/storage source", sources.inventory, sources.storage)
	}

	db := openServerSourceSelectionDB(t)
	handler, err := newServerHandlerWithTestWorkDetails(settings, db, nil, nil, nil)
	if err != nil {
		t.Fatalf("newServerHandler() error = %v", err)
	}
	response := resolveServerSourceSelectionInventory(t, handler)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"owned":true`) {
		t.Fatalf("Manifest resolution = %d/%q, want owned inventory match", response.Code, response.Body.String())
	}
	if got := rommRequests.Load(); got != 0 {
		t.Fatalf("optional RomM requests = %d, want zero while Manifest is selected", got)
	}
}

func TestSelectedRomMInventoryNeverCreatesTrustedStorageOrPLAYWork(t *testing.T) {
	rommServer, rommRequests := newServerSourceSelectionRomM(t)
	settings := config.Server{
		InventorySource: config.InventorySourceRomM,
		RomM:            config.RomMCredentials{BaseURL: rommServer.URL, ClientAPIToken: serverSourceSelectionRomMToken},
	}
	sources, err := newServerInventorySources(settings)
	if err != nil {
		t.Fatalf("newServerInventorySources() error = %v", err)
	}
	if _, ok := sources.inventory.(*inventory.RomMInventorySource); !ok {
		t.Fatalf("selected inventory source = %T, want RomMInventorySource", sources.inventory)
	}
	if sources.storage == nil {
		t.Fatal("selected RomM source has nil storage source; binder requires explicit no-location behavior")
	}
	locations, err := sources.storage.LocationsForAsset(context.Background(), "romm-501")
	if err != nil || len(locations) != 0 {
		t.Fatalf("RomM Asset locations = %#v, err=%v; want no trusted locations", locations, err)
	}

	db := openServerSourceSelectionDB(t)
	handler, err := newServerHandlerWithTestWorkDetails(settings, db, nil, nil, nil)
	if err != nil {
		t.Fatalf("newServerHandler() error = %v", err)
	}
	response := resolveServerSourceSelectionInventory(t, handler)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"owned":true`) ||
		!strings.Contains(response.Body.String(), `"availability":"unavailable"`) ||
		!strings.Contains(response.Body.String(), `"romm-501"`) || strings.Contains(response.Body.String(), "/private/romm/") {
		t.Fatalf("RomM inventory result = %d/%q; want exact owned metadata, unavailable, and no path", response.Code, response.Body.String())
	}

	graph, err := catalog.NewSQLiteRepository(db).GetWorkByExternalIdentity(context.Background(), "igdb", "1565")
	if err != nil {
		t.Fatalf("load exact RomM catalog graph: %v", err)
	}
	if len(graph.Assets) != 1 || graph.Assets[0].ID != "romm-501" || len(graph.Locations) != 0 {
		t.Fatalf("RomM graph = %#v; want one exact Asset and no trusted locations", graph)
	}
	for _, part := range graph.Parts {
		if strings.Contains(part.Filename, "/private/romm/") || strings.Contains(part.RelativePath, "/private/romm/") {
			t.Fatalf("RomM path appeared in catalog part: %#v", part)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	restoreExecutor := &serverSourceSelectionRestoreExecutor{}
	launchExecutor := &serverSourceSelectionLaunchExecutor{}
	agentClient := startServerSourceSelectionAgent(t, restoreExecutor, launchExecutor)
	launchService, err := newSessionLaunchService(ctx, db, agentClient)
	if err != nil {
		t.Fatalf("newSessionLaunchService() error = %v", err)
	}
	t.Cleanup(func() {
		if err := launchService.Close(); err != nil {
			t.Errorf("close PLAY launch service: %v", err)
		}
	})
	jobService := jobs.NewService(jobs.NewSQLiteRepository(db))
	coordinator, err := newServerPlayCoordinator(ctx, settings, db, jobService, agentClient, launchService)
	if err != nil {
		t.Fatalf("newServerPlayCoordinator() error = %v", err)
	}
	if coordinator == nil {
		t.Fatal("selected RomM source did not construct a PLAY coordinator")
	}
	accepted, err := coordinator.Start(playcoordinator.Intent{
		WorkIdentity: providers.ExternalWorkIdentity{Provider: "igdb", ExternalID: "1565"},
		Edition:      playcoordinator.EditionIntent{Platform: "gamecube", Format: "disc_image"},
	})
	if err != nil {
		t.Fatalf("RomM-only PLAY admission error = %v", err)
	}
	coordinator.Wait()
	job, err := jobService.Get(context.Background(), accepted.JobID)
	if err != nil {
		t.Fatalf("load RomM-only PLAY Job: %v", err)
	}
	if job.Status != jobs.StatusFailed || job.ErrorCode != "launch_source_unavailable" {
		t.Fatalf("RomM-only PLAY Job = %#v; want unavailable launch source", job)
	}
	if restoreExecutor.calls.Load() != 0 || launchExecutor.calls.Load() != 0 {
		t.Fatalf("RomM-only Agent restore/launch calls = %d/%d, want 0/0", restoreExecutor.calls.Load(), launchExecutor.calls.Load())
	}
	var sessionCount int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM sessions").Scan(&sessionCount); err != nil {
		t.Fatalf("count PLAY Sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Fatalf("RomM-only PLAY Sessions = %d, want zero", sessionCount)
	}
	if got := rommRequests.Load(); got < 4 {
		t.Fatalf("selected RomM inventory requests = %d, want handler and PLAY lookup requests", got)
	}
}

func TestDefaultManifestSelectionPreservesFirstRestoreSecondLocalPLAY(t *testing.T) {
	manifestPath := writeServerSourceSelectionManifest(t)
	var rommRequests atomic.Int32
	rommServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rommRequests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer rommServer.Close()
	settings := config.Server{
		InventoryManifestPath: manifestPath,
		RomM:                  config.RomMCredentials{BaseURL: rommServer.URL, ClientAPIToken: serverSourceSelectionRomMToken},
	}
	db := openServerSourceSelectionDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	restoreExecutor := &serverSourceSelectionRestoreExecutor{}
	launchExecutor := &serverSourceSelectionLaunchExecutor{}
	agentClient := startServerSourceSelectionAgent(t, restoreExecutor, launchExecutor)
	launchService, err := newSessionLaunchService(ctx, db, agentClient)
	if err != nil {
		t.Fatalf("newSessionLaunchService() error = %v", err)
	}
	t.Cleanup(func() {
		if err := launchService.Close(); err != nil {
			t.Errorf("close PLAY launch service: %v", err)
		}
	})
	jobService := jobs.NewService(jobs.NewSQLiteRepository(db))
	coordinator, err := newServerPlayCoordinator(ctx, settings, db, jobService, agentClient, launchService)
	if err != nil || coordinator == nil {
		t.Fatalf("newServerPlayCoordinator() = %v, %v; want Manifest PLAY service", coordinator, err)
	}
	handler, err := newServerHandlerWithTestWorkDetails(settings, db, nil, coordinator, jobService)
	if err != nil {
		t.Fatalf("newServerHandler() error = %v", err)
	}
	if response := resolveServerSourceSelectionInventory(t, handler); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"availability":"archived"`) {
		t.Fatalf("Manifest archive inventory = %d/%q; want archived", response.Code, response.Body.String())
	}

	const playIntent = `{"work_identity":{"provider":"igdb","external_id":"1565"},"edition":{"platform":"gamecube","format":"disc_image"}}`
	first := postServerSourceSelectionPlay(t, handler, playIntent)
	coordinator.Wait()
	firstJob, err := jobService.Get(context.Background(), first.JobID)
	if err != nil || firstJob.Status != jobs.StatusSucceeded {
		t.Fatalf("first Manifest PLAY Job = %#v, err=%v; want successful archive restore and launch", firstJob, err)
	}
	if restoreExecutor.calls.Load() != 1 || launchExecutor.calls.Load() != 1 {
		t.Fatalf("first PLAY Agent calls = restore %d / launch %d; want 1 / 1", restoreExecutor.calls.Load(), launchExecutor.calls.Load())
	}

	second := postServerSourceSelectionPlay(t, handler, playIntent)
	coordinator.Wait()
	secondJob, err := jobService.Get(context.Background(), second.JobID)
	if err != nil || secondJob.Status != jobs.StatusSucceeded || secondJob.SessionID == firstJob.SessionID {
		t.Fatalf("second Manifest PLAY Job = %#v, err=%v; want a distinct successful local launch", secondJob, err)
	}
	if restoreExecutor.calls.Load() != 1 || launchExecutor.calls.Load() != 2 {
		t.Fatalf("two PLAY Agent calls = restore %d / launch %d; want one restore and two launches", restoreExecutor.calls.Load(), launchExecutor.calls.Load())
	}
	if got := rommRequests.Load(); got != 0 {
		t.Fatalf("optional RomM requests under default Manifest selection = %d, want zero", got)
	}
	graph, err := catalog.NewSQLiteRepository(db).GetWorkByExternalIdentity(context.Background(), "igdb", "1565")
	if err != nil || len(graph.Assets) != 1 || len(graph.Locations) != 2 {
		t.Fatalf("Manifest graph after two PLAY operations = %#v, err=%v; want one Asset with archive and projected local locations", graph, err)
	}
}

func writeServerSourceSelectionManifest(t *testing.T) string {
	t.Helper()
	const manifest = `{"schema_version":1,"assets":[{"id":"manifest-asset-42","work":{"title":"Soulcalibur II","external_ids":{"igdb":"1565"}},"edition":{"platform":"gamecube","format":"disc_image"},"total_size_bytes":4,"parts":[{"role":"rom","filename":"synthetic.iso","size_bytes":4}],"locations":[{"class":"archive","provider":"rclone","locator":"syntheticremote:games/synthetic.iso"}]}]}`
	path := filepath.Join(t.TempDir(), "inventory.json")
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatalf("write synthetic Manifest fixture: %v", err)
	}
	return path
}

func newServerSourceSelectionRomM(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+serverSourceSelectionRomMToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/platforms":
			_, _ = w.Write([]byte(`[{"id":1,"slug":"ngc"}]`))
		case "/api/roms":
			_, _ = w.Write([]byte(`{"items":[{"id":501,"igdb_id":1565,"platform_id":1,"platform_slug":"ngc","fs_extension":"iso","fs_size_bytes":4,"has_simple_single_file":true,"has_nested_single_file":false,"has_multiple_files":false,"fs_path":"/private/romm/synthetic.iso"}],"total":1,"limit":100,"offset":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func openServerSourceSelectionDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "source-selection.db"))
	if err != nil {
		t.Fatalf("open source-selection database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close source-selection database: %v", err)
		}
	})
	return db
}

func resolveServerSourceSelectionInventory(t *testing.T, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/inventory/resolve", strings.NewReader(`{"work_identity":{"provider":"igdb","external_id":"1565"},"edition":{"platform":"gamecube","format":"disc_image"}}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type serverSourceSelectionPlayAccepted struct {
	JobID string `json:"operation_id"`
}

func postServerSourceSelectionPlay(t *testing.T, handler http.Handler, body string) serverSourceSelectionPlayAccepted {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/play", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("PLAY request = %d/%q; want accepted identity/edition intent", response.Code, response.Body.String())
	}
	var accepted serverSourceSelectionPlayAccepted
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil || accepted.JobID == "" {
		t.Fatalf("decode accepted PLAY response %#v: %v", accepted, err)
	}
	return accepted
}

type serverSourceSelectionRestoreExecutor struct{ calls atomic.Int32 }

func (executor *serverSourceSelectionRestoreExecutor) Restore(_ context.Context, request agent.RestoreAsset, progress func(agent.RestoreProgress) error) (agent.RestoreResult, error) {
	executor.calls.Add(1)
	total := request.Asset.TotalSizeBytes
	for _, phase := range []agent.RestorePhase{agent.RestorePhaseValidating, agent.RestorePhaseCopying, agent.RestorePhaseVerifying, agent.RestorePhasePromoting, agent.RestorePhaseComplete} {
		current := total
		if phase == agent.RestorePhaseValidating {
			current = 0
		}
		if err := progress(agent.RestoreProgress{Phase: phase, CurrentBytes: current, TotalBytes: total}); err != nil {
			return agent.RestoreResult{}, err
		}
	}
	return agent.RestoreResult{
		AssetID: request.Asset.ID, LocationClass: "local_cache", RelativePath: path.Join("assets", string(request.Asset.ID), request.Parts[0].Filename),
		VerifiedSizeBytes: total, VerifiedAt: time.Now().UTC(), LocalReady: true,
	}, nil
}

type serverSourceSelectionLaunchExecutor struct{ calls atomic.Int32 }

func (executor *serverSourceSelectionLaunchExecutor) Start(_ context.Context, _ agent.LaunchAsset) (agent.LaunchProcess, error) {
	executor.calls.Add(1)
	return serverSourceSelectionLaunchProcess{}, nil
}

type serverSourceSelectionLaunchProcess struct{}

func (serverSourceSelectionLaunchProcess) Wait() agent.LaunchExit {
	code := 0
	return agent.LaunchExit{Outcome: domain.SessionOutcomeNormalExit, ExitCode: &code}
}

func startServerSourceSelectionAgent(t *testing.T, restore *serverSourceSelectionRestoreExecutor, launch *serverSourceSelectionLaunchExecutor) agent.UDSClient {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "agent.sock")
	server, err := agent.ListenUDSWithExecutors(socketPath, restore, launch)
	if err != nil {
		t.Fatalf("listen on fake Agent UDS: %v", err)
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve() }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close fake Agent UDS: %v", err)
		}
		if err := <-serveResult; err != nil {
			t.Errorf("fake Agent Serve() after close: %v", err)
		}
	})
	return agent.UDSClient{SocketPath: socketPath, Timeout: time.Second}
}
