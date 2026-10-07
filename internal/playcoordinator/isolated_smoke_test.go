package playcoordinator

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"lernae/internal/agent"
	"lernae/internal/agentops"
	"lernae/internal/catalog"
	"lernae/internal/database"
	"lernae/internal/domain"
	"lernae/internal/inventory"
	"lernae/internal/jobs"
	"lernae/internal/playback"
	"lernae/internal/providers"
	"lernae/internal/sessions"
)

const (
	playSmokeOptInEnv    = "LERNAE_P107_06_ISOLATED_SMOKE"
	playSmokeDolphinFile = "LERNAE_P107_06_DOLPHIN_RECORD_FILE"
	playSmokeDolphinRoot = "LERNAE_P107_06_DOLPHIN_ROOT"
)

// TestMain turns a private copy of this test binary into a minimal successful
// dolphin-emu helper. It records argv and its PID without running a GUI or
// opening the synthetic image.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "dolphin-emu" {
		os.Exit(runPlaySmokeDolphinHelper(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// validatePlaySmokeRcloneEnvironment rejects inherited rclone overrides that
// the smoke does not replace. It examines only bytes before the first '=' and
// never copies or includes an environment value in the error.
func validatePlaySmokeRcloneEnvironment(environ []string) error {
	for _, entry := range environ {
		separator := strings.IndexByte(entry, '=')
		name := entry
		if separator >= 0 {
			name = entry[:separator]
		}
		if !strings.HasPrefix(name, "RCLONE_") {
			continue
		}
		switch name {
		case "RCLONE_CONFIG", "RCLONE_CONFIG_PASS", "RCLONE_CACHE_DIR":
			continue
		default:
			return fmt.Errorf("isolated smoke refuses unexpected rclone environment variable %s", name)
		}
	}
	return nil
}

func TestPlaySmokeRcloneEnvironmentRejectsUnownedOverridesWithoutExposingValues(t *testing.T) {
	tests := []struct {
		name        string
		environment []string
		wantError   bool
		wantName    string
		wantValue   string
	}{
		{
			name: "smoke-owned rclone variables can be overwritten",
			environment: []string{
				"RCLONE_CONFIG=caller-config",
				"RCLONE_CONFIG_PASS=caller-password",
				"RCLONE_CACHE_DIR=caller-cache",
				"PATH=/synthetic/path",
			},
		},
		{
			name:        "archive remote override is rejected without exposing its value",
			environment: []string{"RCLONE_CONFIG_ARCHIVE_REMOTE=sentinel-private-remote"},
			wantError:   true,
			wantName:    "RCLONE_CONFIG_ARCHIVE_REMOTE",
			wantValue:   "sentinel-private-remote",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePlaySmokeRcloneEnvironment(tt.environment)
			if !tt.wantError {
				if err != nil {
					t.Fatal("smoke-owned RCLONE_* variables were unexpectedly rejected")
				}
				return
			}
			if err == nil {
				t.Fatal("validatePlaySmokeRcloneEnvironment() error = nil, want unexpected RCLONE_* variable rejection")
			}
			message := err.Error()
			if !strings.Contains(message, tt.wantName) {
				t.Fatalf("error did not identify rejected variable %s", tt.wantName)
			}
			if strings.Contains(message, tt.wantValue) {
				t.Fatalf("error for %s exposed the variable value", tt.wantName)
			}
		})
	}
}

func TestPlayIsolatedFirstAndSecondPlaySmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("isolated installed-rclone smoke is skipped in short mode")
	}
	if os.Getenv(playSmokeOptInEnv) != "1" {
		t.Skipf("set %s=1 to run the full isolated first/second-PLAY smoke", playSmokeOptInEnv)
	}
	if err := validatePlaySmokeRcloneEnvironment(os.Environ()); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "linux" {
		t.Skip("the full Agent/rclone smoke currently requires Linux")
	}
	if _, err := os.Stat("/usr/bin/rclone"); err != nil {
		t.Fatalf("the explicitly enabled smoke requires installed /usr/bin/rclone: %v", err)
	}

	root, rootInfo := createOwnedPlaySmokeRoot(t)
	paths := makePlaySmokePaths(root)
	for _, directory := range []string{
		filepath.Join(paths.archive, "fixtures"),
		paths.cache,
		paths.config,
		paths.home,
		paths.xdgConfig,
		paths.xdgCache,
		paths.rcloneCache,
		paths.rcloneBin,
		paths.temp,
		paths.helperBin,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create smoke-owned directory %q: %v", directory, err)
		}
	}
	if err := os.Chmod(paths.helperBin, 0o700); err != nil {
		t.Fatalf("secure helper directory: %v", err)
	}

	// Every process involved in this smoke sees only the smoke-root-owned environment
	// and local alias config. PATH contains only the synthetic Dolphin helper
	// and a private symlink to installed rclone, never system Dolphin or a user
	// configured launcher.
	rcloneAliasPath := filepath.Join(paths.rcloneBin, "rclone")
	if err := os.Symlink("/usr/bin/rclone", rcloneAliasPath); err != nil {
		t.Fatalf("create isolated alias to installed rclone: %v", err)
	}
	t.Setenv("HOME", paths.home)
	t.Setenv("XDG_CONFIG_HOME", paths.xdgConfig)
	t.Setenv("XDG_CACHE_HOME", paths.xdgCache)
	t.Setenv("XDG_RUNTIME_DIR", paths.temp)
	t.Setenv("TMPDIR", paths.temp)
	t.Setenv("RCLONE_CONFIG", paths.rcloneConfig)
	t.Setenv("RCLONE_CACHE_DIR", paths.rcloneCache)
	t.Setenv("RCLONE_CONFIG_PASS", "")
	t.Setenv("PATH", paths.helperBin+string(os.PathListSeparator)+paths.rcloneBin)

	config := fmt.Sprintf("[archive]\ntype = alias\nremote = %s\n", paths.archive)
	if err := os.WriteFile(paths.rcloneConfig, []byte(config), 0o600); err != nil {
		t.Fatalf("write isolated rclone alias config: %v", err)
	}
	rclonePath, err := agentops.ResolveRcloneExecutable()
	if err != nil {
		t.Fatalf("resolve installed rclone from isolated PATH: %v", err)
	}
	installedRclone, err := os.Stat("/usr/bin/rclone")
	if err != nil {
		t.Fatalf("inspect installed /usr/bin/rclone: %v", err)
	}
	resolvedRclone, err := os.Stat(rclonePath)
	if err != nil || !os.SameFile(installedRclone, resolvedRclone) {
		t.Fatalf("rclone resolved to %q (stat=%v err=%v), want an isolated alias to installed /usr/bin/rclone", rclonePath, resolvedRclone, err)
	}

	image := bytes.Repeat([]byte("synthetic isolated disc image bytes\n"), 128)
	if err := os.WriteFile(filepath.Join(paths.archive, "fixtures", playSmokeFilename), image, 0o600); err != nil {
		t.Fatalf("write synthetic archive fixture: %v", err)
	}
	identity, graph := playSmokeCatalogGraph(int64(len(image)))
	manifestPath := filepath.Join(root, "inventory.json")
	if err := writePlaySmokeManifest(manifestPath, identity, graph, int64(len(image))); err != nil {
		t.Fatalf("write isolated synthetic inventory: %v", err)
	}
	manifestSource := inventory.NewManifestInventorySource(manifestPath)

	cache, err := agentops.OpenCache(paths.cache)
	if err != nil {
		t.Fatalf("open isolated Agent cache: %v", err)
	}
	var cacheClose sync.Once
	closeCache := func() {
		cacheClose.Do(func() {
			if err := cache.Close(); err != nil {
				t.Errorf("close isolated Agent cache: %v", err)
			}
		})
	}
	t.Cleanup(closeCache)
	restoreExecutor, err := agentops.NewRcloneRestoreExecutor(cache)
	if err != nil {
		t.Fatalf("construct real-rclone restore executor: %v", err)
	}
	helperPath, err := installPlaySmokeDolphinHelper(t, paths.helperBin)
	if err != nil {
		t.Fatalf("install helper-process Dolphin: %v", err)
	}
	resolvedDolphin, err := exec.LookPath("dolphin-emu")
	if err != nil || resolvedDolphin != helperPath {
		t.Fatalf("Dolphin resolved to %q (err=%v), want only the test-owned helper %q", resolvedDolphin, err, helperPath)
	}
	dolphinRecords := filepath.Join(root, "dolphin-launches.jsonl")
	t.Setenv(playSmokeDolphinFile, dolphinRecords)
	t.Setenv(playSmokeDolphinRoot, root)
	launchExecutor := &playSmokeDolphinLaunchExecutor{cache: cache, runner: agentops.NewDolphinRunner()}
	udsPath := filepath.Join(root, "agent.sock")
	agentServer, err := agent.ListenUDSWithExecutors(udsPath, restoreExecutor, launchExecutor)
	if err != nil {
		t.Fatalf("start isolated Agent UDS: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- agentServer.Serve() }()
	var agentClose sync.Once
	closeAgent := func() {
		agentClose.Do(func() {
			if err := agentServer.Close(); err != nil {
				t.Errorf("close isolated Agent UDS: %v", err)
			}
			if err := <-serveDone; err != nil {
				t.Errorf("Agent UDS Serve() after close: %v", err)
			}
		})
	}
	t.Cleanup(closeAgent)

	client := &playSmokeCountingAgentClient{client: agent.UDSClient{SocketPath: udsPath, Timeout: 15 * time.Second}}
	first := openPlaySmokeServices(t, filepath.Join(root, "lernae.db"), manifestSource, client, graph)
	firstAccepted := startPlaySmoke(t, first, identity)
	firstJob := waitForPlaySmokeJob(t, first.jobs, firstAccepted.JobID)
	if firstJob.Status != jobs.StatusSucceeded {
		t.Fatalf("first PLAY failed: Job=%#v Agent launch error=%v executor start error=%v", firstJob, client.lastLaunchError(), launchExecutor.lastStartError())
	}
	assertSuccessfulPlaySmokeJob(t, first, firstJob)
	assertPlaySmokeSession(t, first.sessions, firstJob.SessionID)
	if got := client.counts(); got.restore != 1 || got.launch != 1 {
		t.Fatalf("first PLAY Agent requests = restore %d / launch %d, want 1 / 1", got.restore, got.launch)
	}
	assertPlaySmokeRestorePipeline(t, client, firstJob.ID, graph, int64(len(image)))
	assertPlaySmokeArchiveAndLocalProjection(t, first.catalog, identity, paths, graph, image)
	assertPlaySmokeHelperProcesses(t, dolphinRecords, 1, filepath.Join(paths.cache, "assets", string(graph.Assets[0].ID), playSmokeFilename))

	// This boundary intentionally closes every PLAY-facing service and SQLite
	// pool while keeping only the Agent daemon/cache alive for the second request.
	if err := first.close(); err != nil {
		t.Fatalf("close first PLAY services and SQLite cleanly: %v", err)
	}

	second := openPlaySmokeServices(t, filepath.Join(root, "lernae.db"), manifestSource, client, catalog.WorkGraph{})
	secondJobAfterReopen, err := second.jobs.Get(context.Background(), firstJob.ID)
	if err != nil || secondJobAfterReopen.Status != jobs.StatusSucceeded {
		t.Fatalf("first PLAY Job after SQLite reopen = %#v, err=%v", secondJobAfterReopen, err)
	}
	assertPlaySmokeSession(t, second.sessions, firstJob.SessionID)
	assertPlaySmokeArchiveAndLocalProjection(t, second.catalog, identity, paths, graph, image)

	secondAccepted := startPlaySmoke(t, second, identity)
	if secondAccepted.JobID == firstAccepted.JobID {
		t.Fatal("second PLAY reused the first durable Job ID")
	}
	secondJob := waitForPlaySmokeJob(t, second.jobs, secondAccepted.JobID)
	assertSuccessfulPlaySmokeJob(t, second, secondJob)
	assertPlaySmokeSession(t, second.sessions, secondJob.SessionID)
	if secondJob.SessionID == firstJob.SessionID {
		t.Fatal("second PLAY reused the first Session ID")
	}
	if got := client.counts(); got.restore != 1 || got.launch != 2 {
		t.Fatalf("Agent requests after second PLAY = restore %d / launch %d, want 1 / 2 (no second restore)", got.restore, got.launch)
	}
	if got := len(client.restoreSnapshot()); got != 1 {
		t.Fatalf("RestoreAsset request count after second PLAY = %d, want no second restore request", got)
	}
	assertPlaySmokeArchiveAndLocalProjection(t, second.catalog, identity, paths, graph, image)
	assertPlaySmokeHelperProcesses(t, dolphinRecords, 2, filepath.Join(paths.cache, "assets", string(graph.Assets[0].ID), playSmokeFilename))
	assertPlaySmokeSessionRows(t, second.db, 2)

	if err := second.close(); err != nil {
		t.Fatalf("close second PLAY services and SQLite cleanly: %v", err)
	}
	closeAgent()
	closeCache()
	assertPlaySmokeRootStillOwned(t, root, rootInfo)
}

const playSmokeFilename = "synthetic-fixture.iso"

type playSmokePaths struct {
	archive      string
	cache        string
	config       string
	home         string
	xdgConfig    string
	xdgCache     string
	rcloneCache  string
	rcloneBin    string
	temp         string
	helperBin    string
	rcloneConfig string
}

func makePlaySmokePaths(root string) playSmokePaths {
	return playSmokePaths{
		archive: filepath.Join(root, "archive"), cache: filepath.Join(root, "agent-cache"),
		config: filepath.Join(root, "config"), home: filepath.Join(root, "home"),
		xdgConfig: filepath.Join(root, "xdg-config"), xdgCache: filepath.Join(root, "xdg-cache"),
		rcloneCache: filepath.Join(root, "rclone-cache"), rcloneBin: filepath.Join(root, "rclone-bin"), temp: filepath.Join(root, "tmp"),
		helperBin: filepath.Join(root, "bin"), rcloneConfig: filepath.Join(root, "config", "rclone.conf"),
	}
}

func createOwnedPlaySmokeRoot(t *testing.T) (string, os.FileInfo) {
	t.Helper()
	root, err := os.MkdirTemp(".", ".p107-06-play-smoke-")
	if err != nil {
		t.Fatalf("create test-owned smoke root: %v", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatalf("resolve test-owned smoke root: %v", err)
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("inspect test-owned smoke root: info=%v err=%v", rootInfo, err)
	}
	t.Logf("SMOKE_ROOT=%s", root)
	t.Cleanup(func() {
		current, err := os.Lstat(root)
		if errors.Is(err, os.ErrNotExist) {
			t.Logf("SMOKE_ROOT_REMOVED=%s", root)
			return
		}
		if err != nil {
			t.Errorf("inspect smoke root before cleanup: %v", err)
			return
		}
		if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(rootInfo, current) {
			t.Errorf("refusing to remove a smoke root no longer owned by this test: %s", root)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove test-owned smoke root: %v", err)
			return
		}
		t.Logf("SMOKE_ROOT_REMOVED=%s", root)
	})
	return root, rootInfo
}

func assertPlaySmokeRootStillOwned(t *testing.T, root string, original os.FileInfo) {
	t.Helper()
	current, err := os.Lstat(root)
	if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(original, current) {
		t.Fatalf("smoke root ownership changed before cleanup: info=%v err=%v", current, err)
	}
}

func playSmokeCatalogGraph(size int64) (providers.ExternalWorkIdentity, catalog.WorkGraph) {
	identity := providers.ExternalWorkIdentity{Provider: "fixture", ExternalID: "synthetic-p107-06"}
	const workID domain.WorkID = "work-p107-06-smoke"
	const editionID domain.EditionID = "edition-p107-06-gc"
	const assetID domain.AssetID = "asset-p107-06-synthetic"
	const partID domain.AssetPartID = "part-p107-06-synthetic"
	partIndex := 1
	return identity, catalog.WorkGraph{
		Work:     domain.Work{ID: workID, Medium: domain.MediumGame, WorkType: "game", Title: "Synthetic PLAY Smoke Fixture"},
		Editions: []domain.Edition{{ID: editionID, WorkID: workID, Platform: "gamecube", Format: "disc_image"}},
		Assets:   []domain.Asset{{ID: assetID, EditionID: editionID, Kind: "disc_image", TotalSizeBytes: size}},
		Parts:    []domain.AssetPart{{ID: partID, AssetID: assetID, PartIndex: &partIndex, Role: "rom", Filename: playSmokeFilename, SizeBytes: size}},
		Locations: []domain.AssetLocation{{ID: "location-p107-06-archive", AssetID: assetID, StorageProviderID: "rclone",
			Locator: "archive:fixtures/" + playSmokeFilename, LocationClass: "archive"}},
		ExternalIdentities: []domain.ExternalIdentity{{ID: "identity-p107-06-smoke", WorkID: workID,
			Provider: identity.Provider, ExternalID: identity.ExternalID}},
	}
}

func writePlaySmokeManifest(filename string, identity providers.ExternalWorkIdentity, graph catalog.WorkGraph, size int64) error {
	partIndex := 1
	manifest := inventory.Manifest{SchemaVersion: 1, Assets: []inventory.ManifestAsset{{
		ID:      string(graph.Assets[0].ID),
		Work:    inventory.ManifestWork{Title: graph.Work.Title, ExternalIDs: map[string]string{identity.Provider: identity.ExternalID}},
		Edition: inventory.ManifestEdition{Platform: "gamecube", Format: "disc_image"}, TotalSizeBytes: size,
		Parts:     []inventory.ManifestAssetPart{{PartIndex: &partIndex, Role: "rom", Filename: playSmokeFilename, SizeBytes: size}},
		Locations: []inventory.ManifestLocation{{Class: "archive", Provider: "rclone", Locator: "archive:fixtures/" + playSmokeFilename}},
	}}}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return os.WriteFile(filename, encoded, 0o600)
}

func installPlaySmokeDolphinHelper(t *testing.T, helperDir string) (string, error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	source, err := os.Open(executable)
	if err != nil {
		return "", err
	}
	defer source.Close()
	helperPath := filepath.Join(helperDir, "dolphin-emu")
	target, err := os.OpenFile(helperPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		return "", err
	}
	if err := target.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(helperPath, 0o700); err != nil {
		return "", err
	}
	return helperPath, nil
}

type playSmokeDolphinRecord struct {
	PID  int      `json:"pid"`
	Args []string `json:"args"`
}

// playSmokeDolphinLaunchExecutor mirrors cmd/agent's private adapter so the
// smoke crosses the real UDS and Cache validation boundaries without importing
// the command's main package or changing production visibility.
type playSmokeDolphinLaunchExecutor struct {
	cache      *agentops.Cache
	runner     *agentops.DolphinRunner
	mu         sync.Mutex
	startError error
}

func (executor *playSmokeDolphinLaunchExecutor) lastStartError() error {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.startError
}

func (executor *playSmokeDolphinLaunchExecutor) rememberStartError(err error) error {
	if err == nil {
		return nil
	}
	executor.mu.Lock()
	executor.startError = err
	executor.mu.Unlock()
	return err
}

func (executor *playSmokeDolphinLaunchExecutor) Start(ctx context.Context, request agent.LaunchAsset) (agent.LaunchProcess, error) {
	if executor == nil {
		return nil, errors.New("invalid GameCube launch request")
	}
	if executor.cache == nil || executor.runner == nil || request.Validate() != nil {
		return nil, executor.rememberStartError(errors.New("invalid GameCube launch request"))
	}
	work, edition, asset, parts := request.DomainRecords()
	image, err := executor.cache.OpenReadyGameCubeImageForSession(string(request.SessionID), work, edition, asset, parts)
	if err != nil {
		if errors.Is(err, agentops.ErrLocalReadyImageInvalid) {
			return nil, executor.rememberStartError(agent.ErrLaunchLocalCacheInvalid)
		}
		return nil, executor.rememberStartError(err)
	}
	defer func() { _ = image.Close() }()
	process, err := executor.runner.Start(ctx, image)
	if err != nil {
		if errors.Is(err, agentops.ErrLocalReadyImageInvalid) {
			return nil, executor.rememberStartError(agent.ErrLaunchLocalCacheInvalid)
		}
		return nil, executor.rememberStartError(err)
	}
	return playSmokeDolphinProcess{process: process}, nil
}

type playSmokeDolphinProcess struct {
	process *agentops.DolphinProcess
}

func (process playSmokeDolphinProcess) Wait() agent.LaunchExit {
	if err := process.process.Wait(); err == nil {
		return agent.LaunchExit{Outcome: domain.SessionOutcomeNormalExit, ExitCode: playSmokeIntPointer(0)}
	} else {
		var exitError *agentops.DolphinExitError
		if errors.As(err, &exitError) {
			return agent.LaunchExit{Outcome: domain.SessionOutcomeNonZeroExit, ExitCode: playSmokeIntPointer(exitError.ExitCode)}
		}
	}
	return agent.LaunchExit{Outcome: domain.SessionOutcomeInterrupted}
}

func playSmokeIntPointer(value int) *int { return &value }

type playSmokeCountingAgentClient struct {
	client          agent.UDSClient
	mu              sync.Mutex
	restore         int
	launch          int
	launchError     error
	restoreRequests []playSmokeRestoreRequest
}

type playSmokeRestoreRequest struct {
	request  agent.RestoreAsset
	progress []agent.RestoreProgress
}

func (client *playSmokeCountingAgentClient) RestoreAsset(ctx context.Context, request agent.RestoreAsset, report func(agent.RestoreProgress)) (agent.RestoreResult, error) {
	client.mu.Lock()
	client.restore++
	client.mu.Unlock()
	var events []agent.RestoreProgress
	wrappedReport := func(event agent.RestoreProgress) {
		events = append(events, event)
		if report != nil {
			report(event)
		}
	}
	result, err := client.client.RestoreAsset(ctx, request, wrappedReport)
	client.mu.Lock()
	client.restoreRequests = append(client.restoreRequests, playSmokeRestoreRequest{request: request, progress: append([]agent.RestoreProgress(nil), events...)})
	client.mu.Unlock()
	return result, err
}

func (client *playSmokeCountingAgentClient) LaunchAsset(ctx context.Context, request agent.LaunchAsset, onStarted func(agent.LaunchStarted) bool) (agent.LaunchEnded, error) {
	client.mu.Lock()
	client.launch++
	client.mu.Unlock()
	ended, err := client.client.LaunchAsset(ctx, request, onStarted)
	client.mu.Lock()
	client.launchError = err
	client.mu.Unlock()
	return ended, err
}

func (client *playSmokeCountingAgentClient) counts() struct{ restore, launch int } {
	client.mu.Lock()
	defer client.mu.Unlock()
	return struct{ restore, launch int }{restore: client.restore, launch: client.launch}
}

func (client *playSmokeCountingAgentClient) lastLaunchError() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.launchError
}

func (client *playSmokeCountingAgentClient) restoreSnapshot() []playSmokeRestoreRequest {
	client.mu.Lock()
	defer client.mu.Unlock()
	requests := make([]playSmokeRestoreRequest, len(client.restoreRequests))
	for index, request := range client.restoreRequests {
		requests[index] = playSmokeRestoreRequest{request: request.request, progress: append([]agent.RestoreProgress(nil), request.progress...)}
	}
	return requests
}

type playSmokeServices struct {
	db          *sql.DB
	catalog     *catalog.SQLiteRepository
	jobs        *jobs.Service
	sessions    *sessions.Service
	playback    *playback.Service
	coordinator *PlayCoordinator
	cancel      context.CancelFunc
	closeOnce   sync.Once
	closeErr    error
}

func openPlaySmokeServices(t *testing.T, databasePath string, source *inventory.ManifestInventorySource, client *playSmokeCountingAgentClient, seedGraph catalog.WorkGraph) *playSmokeServices {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	db, err := database.Open(ctx, databasePath)
	if err != nil {
		cancel()
		t.Fatalf("open isolated PLAY SQLite: %v", err)
	}
	repository := catalog.NewSQLiteRepository(db)
	if seedGraph.Work.ID != "" {
		if err := repository.EnsureGraph(ctx, seedGraph); err != nil {
			cancel()
			_ = db.Close()
			t.Fatalf("persist synthetic canonical catalog graph: %v", err)
		}
	}
	jobService := jobs.NewService(jobs.NewSQLiteRepository(db))
	sessionService := sessions.NewService(sessions.NewSQLiteRepository(db))
	playbackService, err := playback.NewService(ctx, client, sessionService)
	if err != nil {
		cancel()
		_ = db.Close()
		t.Fatalf("construct real Session-backed playback service: %v", err)
	}
	coordinator, err := NewService(ctx, repository, source, source, jobService, client, playbackService)
	if err != nil {
		cancel()
		_ = playbackService.Close()
		_ = db.Close()
		t.Fatalf("construct real catalog/Jobs PLAY coordinator: %v", err)
	}
	services := &playSmokeServices{db: db, catalog: repository, jobs: jobService, sessions: sessionService,
		playback: playbackService, coordinator: coordinator, cancel: cancel}
	t.Cleanup(func() {
		if err := services.close(); err != nil {
			t.Errorf("close isolated PLAY services: %v", err)
		}
	})
	return services
}

func (services *playSmokeServices) close() error {
	services.closeOnce.Do(func() {
		services.cancel()
		services.coordinator.Wait()
		if err := services.playback.Close(); err != nil {
			services.closeErr = errors.Join(services.closeErr, fmt.Errorf("close playback service: %w", err))
		}
		if err := services.db.Close(); err != nil {
			services.closeErr = errors.Join(services.closeErr, fmt.Errorf("close SQLite: %w", err))
		}
	})
	return services.closeErr
}

func startPlaySmoke(t *testing.T, services *playSmokeServices, identity providers.ExternalWorkIdentity) Accepted {
	t.Helper()
	accepted, err := services.coordinator.Start(Intent{WorkIdentity: identity, Edition: EditionIntent{Platform: "gamecube", Format: "disc_image"}})
	if err != nil {
		t.Fatalf("accept synthetic PLAY intent: %v", err)
	}
	return accepted
}

func waitForPlaySmokeJob(t *testing.T, jobsService *jobs.Service, id string) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		job, err := jobsService.Get(context.Background(), id)
		if err == nil && (job.Status == jobs.StatusSucceeded || job.Status == jobs.StatusFailed ||
			job.Status == jobs.StatusInterrupted || job.Status == jobs.StatusCancelled) {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, err := jobsService.Get(context.Background(), id)
	t.Fatalf("timed out waiting for PLAY Job %s: current=%#v err=%v", id, job, err)
	return jobs.Job{}
}

func assertSuccessfulPlaySmokeJob(t *testing.T, services *playSmokeServices, job jobs.Job) {
	t.Helper()
	if job.Status != jobs.StatusSucceeded || job.Phase != jobs.PhasePlaying || job.SessionID == "" {
		t.Fatalf("completed PLAY Job = %#v, want succeeded/playing with a Session", job)
	}
	if _, err := services.sessions.Get(context.Background(), domain.SessionID(job.SessionID)); err != nil {
		t.Fatalf("PLAY Job points to a persisted Session: %v", err)
	}
}

func assertPlaySmokeSession(t *testing.T, service *sessions.Service, id string) {
	t.Helper()
	session, err := service.Get(context.Background(), domain.SessionID(id))
	if err != nil {
		t.Fatalf("load persisted Session %q: %v", id, err)
	}
	if session.ID == "" || session.EndedAt == nil || session.Outcome != domain.SessionOutcomeNormalExit {
		t.Fatalf("Session terminal evidence = %#v, want persisted normal terminal outcome", session)
	}
}

func assertPlaySmokeArchiveAndLocalProjection(t *testing.T, repository *catalog.SQLiteRepository, identity providers.ExternalWorkIdentity, paths playSmokePaths, graph catalog.WorkGraph, image []byte) {
	t.Helper()
	got, err := repository.GetWorkByExternalIdentity(context.Background(), identity.Provider, identity.ExternalID)
	if err != nil {
		t.Fatalf("load canonical graph after PLAY: %v", err)
	}
	var archives, locals int
	for _, location := range got.Locations {
		switch location.LocationClass {
		case "archive":
			archives++
			if location.Locator != graph.Locations[0].Locator || location.StorageProviderID != "rclone" {
				t.Fatalf("archive AssetLocation changed: %#v", location)
			}
		case "local_cache":
			locals++
			want := "assets/" + string(graph.Assets[0].ID) + "/" + playSmokeFilename
			if location.StorageProviderID != "filesystem" || location.Locator != want {
				t.Fatalf("persisted local-cache AssetLocation = %#v, want filesystem:%s", location, want)
			}
		}
	}
	if archives != 1 || locals != 1 {
		t.Fatalf("persisted archive/local AssetLocations = %d/%d, want 1/1: %#v", archives, locals, got.Locations)
	}
	archiveBytes, err := os.ReadFile(filepath.Join(paths.archive, "fixtures", playSmokeFilename))
	if err != nil || !bytes.Equal(archiveBytes, image) {
		t.Fatalf("archive fixture was not preserved: bytes=%d err=%v", len(archiveBytes), err)
	}
	localPath := filepath.Join(paths.cache, "assets", string(graph.Assets[0].ID), playSmokeFilename)
	localBytes, err := os.ReadFile(localPath)
	if err != nil || !bytes.Equal(localBytes, image) {
		t.Fatalf("promoted Agent cache image differs from synthetic archive: bytes=%d err=%v", len(localBytes), err)
	}
}

func assertPlaySmokeHelperProcesses(t *testing.T, filename string, wantCount int, expectedImagePath string) {
	t.Helper()
	file, err := os.Open(filename)
	if err != nil {
		t.Fatalf("read helper Dolphin launch records: %v", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	var records []playSmokeDolphinRecord
	for scanner.Scan() {
		var record playSmokeDolphinRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode synthetic Dolphin helper record: %v", err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan synthetic Dolphin helper records: %v", err)
	}
	if len(records) != wantCount {
		t.Fatalf("helper Dolphin invocation count = %d, want %d", len(records), wantCount)
	}
	for index, record := range records {
		if record.PID <= 0 || len(record.Args) != 3 || record.Args[0] != "-b" || record.Args[1] != "-e" || record.Args[2] != expectedImagePath {
			t.Fatalf("helper Dolphin invocation %d = %#v, want fixed argv for cache candidate %q", index+1, record, expectedImagePath)
		}
		if err := syscall.Kill(record.PID, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("helper Dolphin process %d was not reaped after Session completion: kill(0)=%v", record.PID, err)
		}
	}
}

func assertPlaySmokeSessionRows(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil {
		t.Fatalf("count persisted PLAY Sessions: %v", err)
	}
	if count != want {
		t.Fatalf("persisted Session rows = %d, want %d", count, want)
	}
}

func assertPlaySmokeRestorePipeline(t *testing.T, client *playSmokeCountingAgentClient, jobID string, graph catalog.WorkGraph, expectedBytes int64) {
	t.Helper()
	requests := client.restoreSnapshot()
	if len(requests) != 1 {
		t.Fatalf("RestoreAsset requests = %d, want exactly one archive restore", len(requests))
	}
	record := requests[0]
	if record.request.JobID != jobID || record.request.Asset.ID != graph.Assets[0].ID ||
		record.request.SourceLocation.Locator != graph.Locations[0].Locator || record.request.SourceLocation.LocationClass != "archive" {
		t.Fatalf("first RestoreAsset UDS request = %#v, want the accepted Job's exact archive Asset", record.request)
	}
	wantPhases := []agent.RestorePhase{agent.RestorePhaseValidating, agent.RestorePhaseStaging, agent.RestorePhaseCopying,
		agent.RestorePhaseVerifying, agent.RestorePhasePromoting, agent.RestorePhaseComplete}
	var previous *agent.RestoreProgress
	var phases []agent.RestorePhase
	for index, event := range record.progress {
		if err := agent.ValidateProgressAfter(previous, event); err != nil {
			t.Fatalf("P1-05 progress event %d is invalid: %v; events=%#v", index, err, record.progress)
		}
		if event.TotalBytes != expectedBytes {
			t.Fatalf("P1-05 event %d total = %d, want exact synthetic image size %d", index, event.TotalBytes, expectedBytes)
		}
		if event.Phase != agent.RestorePhaseComplete && event.CurrentBytes >= expectedBytes {
			t.Fatalf("P1-05 phase %q reported completion before promotion: %#v", event.Phase, event)
		}
		if len(phases) == 0 || phases[len(phases)-1] != event.Phase {
			phases = append(phases, event.Phase)
		}
		copy := event
		previous = &copy
	}
	if len(phases) != len(wantPhases) {
		t.Fatalf("P1-05 restore phases = %#v, want full validation/staging/copy/verify/promote/complete pipeline", phases)
	}
	for index, phase := range wantPhases {
		if phases[index] != phase {
			t.Fatalf("P1-05 phase %d = %q, want %q; all phases=%#v", index, phases[index], phase, phases)
		}
	}
	last := record.progress[len(record.progress)-1]
	if last.Phase != agent.RestorePhaseComplete || last.CurrentBytes != expectedBytes {
		t.Fatalf("P1-05 final evidence = %#v, want complete at exact expected size", last)
	}
}

func runPlaySmokeDolphinHelper(args []string) int {
	root := os.Getenv(playSmokeDolphinRoot)
	filename := os.Getenv(playSmokeDolphinFile)
	if root == "" || filename == "" || !filepath.IsAbs(root) || !filepath.IsAbs(filename) {
		return 91
	}
	rel, err := filepath.Rel(root, filename)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return 92
	}
	if len(args) != 3 || args[0] != "-b" || args[1] != "-e" || !filepath.IsAbs(args[2]) {
		return 93
	}
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 94
	}
	record := playSmokeDolphinRecord{PID: os.Getpid(), Args: append([]string(nil), args...)}
	if err := json.NewEncoder(file).Encode(record); err != nil {
		_ = file.Close()
		return 95
	}
	if err := file.Close(); err != nil {
		return 96
	}
	return 0
}
