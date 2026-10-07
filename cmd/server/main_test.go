package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"lernae/internal/acquisition"
	"lernae/internal/agent"
	"lernae/internal/api"
	"lernae/internal/catalog"
	"lernae/internal/config"
	"lernae/internal/database"
	"lernae/internal/domain"
	"lernae/internal/jobs"
	"lernae/internal/playcoordinator"
	"lernae/internal/providers"
	"lernae/internal/providers/openlibrary"
	"lernae/internal/providers/tvmaze"
	universalsearch "lernae/internal/search"
	"lernae/internal/sessions"
)

func TestServerProwlarrCompositionIsDiscoveryOnlyAndNeverProbesAtStartup(t *testing.T) {
	var calls, discoveryCalls, otherCalls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Safe attribution evidence only: no raw URLs, headers, queries or bodies.
		if r.Method == http.MethodGet && r.URL.Path == "/prefix/api/v1/search" {
			discoveryCalls.Add(1)
		} else {
			otherCalls.Add(1)
		}
	}))
	defer remote.Close()
	base := t.TempDir()
	settings := config.Server{
		Prowlarr:                   config.ProwlarrSettings{Enabled: true, BaseURL: remote.URL + "/prefix"},
		ProwlarrCredentials:        config.ProwlarrCredentials{APIKey: "PRIVATE_SERVER_PROWLARR_KEY"},
		ProwlarrReferenceDirectory: config.ProwlarrReferenceDirectory(filepath.Join(base, "config", "lernae", "acquisition", "prowlarr")),
	}
	p, err := newServerProwlarrAcquisition(settings)
	if err != nil || p == nil || calls.Load() != 0 {
		t.Fatal("composition probed remote service")
	}
	discovery, execution, executor, err := serverAcquisitionCapabilities(nil, p)
	if err != nil || executor != nil || execution != nil {
		t.Fatal("remote discovery acquired execution capabilities")
	}
	registered, ok := discovery.Lookup("prowlarr")
	if !ok || registered != p {
		t.Fatal("productive discovery provider not registered")
	}
	if _, err := os.Stat(string(settings.ProwlarrReferenceDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("startup created private reference state")
	}
	for _, settings := range []config.Server{{}, {Prowlarr: config.ProwlarrSettings{Enabled: true, BaseURL: remote.URL}}, {Prowlarr: config.ProwlarrSettings{BaseURL: remote.URL}, ProwlarrCredentials: config.ProwlarrCredentials{APIKey: "saved-key"}}} {
		p, err := newServerProwlarrAcquisition(settings)
		if err != nil || p != nil {
			t.Fatal("disabled or unconfigured remote prevented normal startup")
		}
	}
	// Exercise actual startup to its occupied-listener stop. A configured remote
	// service must never be contacted before an explicit candidates request.
	xdg := filepath.Join(base, "config")
	home := filepath.Join(base, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("XDG_RUNTIME_DIR", base)
	t.Setenv(config.EnvInventorySource, config.InventorySourceManifest)
	t.Setenv(config.EnvInventoryManifestPath, "")
	for _, name := range []string{config.EnvIGDBClientID, config.EnvIGDBClientSecret, config.EnvRomMBaseURL, config.EnvRomMClientAPIToken} {
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	providers, err := config.NewFileProviderConfigStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveProwlarr(settings.ProwlarrCredentials); err != nil {
		t.Fatal(err)
	}
	runtime, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Update(func(s *config.RuntimeSettings) { s.Prowlarr = settings.Prowlarr }); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv(config.EnvServerListenAddr, listener.Addr().String())
	t.Setenv(config.EnvServerDatabasePath, filepath.Join(base, "startup.db"))
	t.Setenv(config.EnvAgentSocketPath, filepath.Join(base, "missing-agent.sock"))
	if err := run(); err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatal("startup did not reach normal listener composition")
	}
	if calls.Load() != 0 {
		t.Fatalf("Server startup depends on remote Prowlarr: requests=%d discovery=%d other=%d", calls.Load(), discoveryCalls.Load(), otherCalls.Load())
	}
	if _, err := os.Stat(string(settings.ProwlarrReferenceDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Server startup opened reference store")
	}
}

func TestServerQBittorrentCompositionAndActualStartupStayLocal(t *testing.T) {
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer remote.Close()
	base := t.TempDir()
	xdg, home := filepath.Join(base, "config"), filepath.Join(base, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("XDG_RUNTIME_DIR", base)
	t.Setenv(config.EnvInventorySource, config.InventorySourceManifest)
	t.Setenv(config.EnvInventoryManifestPath, "")
	settings := config.Server{
		Prowlarr:                   config.ProwlarrSettings{Enabled: true, BaseURL: remote.URL},
		ProwlarrCredentials:        config.ProwlarrCredentials{APIKey: "PRIVATE_PROWLARR_KEY"},
		ProwlarrReferenceDirectory: config.ProwlarrReferenceDirectory(filepath.Join(xdg, "lernae", "acquisition", "prowlarr")),
		QBittorrent:                config.QBittorrentSettings{Enabled: true, BaseURL: remote.URL, Username: "operator"},
		QBittorrentCredentials:     config.QBittorrentCredentials{Password: "PRIVATE_PASSWORD"},
	}
	p, err := newServerProwlarrAcquisition(settings)
	if err != nil {
		t.Fatal(err)
	}
	q, err := newServerQBittorrentAcquisition(settings, p)
	if err != nil || q == nil || !q.Configured() {
		t.Fatal("configured qBit not composed")
	}
	_, execution, executor, err := serverAcquisitionWithQBittorrent(nil, p, q)
	if err != nil || execution == nil || executor == nil || calls.Load() != 0 {
		t.Fatal("explicit capabilities absent or composition did HTTP")
	}
	for _, settings := range []config.Server{{}, {QBittorrent: settings.QBittorrent}, {QBittorrent: config.QBittorrentSettings{BaseURL: remote.URL, Username: "operator"}, QBittorrentCredentials: settings.QBittorrentCredentials}} {
		q, err := newServerQBittorrentAcquisition(settings, p)
		if err != nil || q != nil {
			t.Fatal("disabled/unconfigured qBit has authority")
		}
		_, registry, executor, err := serverAcquisitionWithQBittorrent(nil, p, q)
		if err != nil || registry != nil || executor != nil {
			t.Fatal("disabled/unconfigured qBit gained preclaim capability")
		}
	}
	providers, err := config.NewFileProviderConfigStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveProwlarr(settings.ProwlarrCredentials); err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveQBittorrent(settings.QBittorrentCredentials); err != nil {
		t.Fatal(err)
	}
	runtime, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Update(func(s *config.RuntimeSettings) { s.Prowlarr, s.QBittorrent = settings.Prowlarr, settings.QBittorrent }); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv(config.EnvServerListenAddr, listener.Addr().String())
	t.Setenv(config.EnvServerDatabasePath, filepath.Join(base, "startup.db"))
	t.Setenv(config.EnvAgentSocketPath, filepath.Join(base, "missing-agent.sock"))
	if err := run(); err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatal("startup did not reach occupied listener")
	}
	if calls.Load() != 0 {
		t.Fatal("configured qBit/Prowlarr startup did HTTP")
	}
}

func TestRunRejectsReservedStagingBeforeDatabaseOrListener(t *testing.T) {
	base := t.TempDir()
	xdg := filepath.Join(base, "config")
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("XDG_RUNTIME_DIR", base)
	t.Setenv(config.EnvInventorySource, config.InventorySourceManifest)
	t.Setenv(config.EnvInventoryManifestPath, "")
	for _, name := range []string{config.EnvIGDBClientID, config.EnvIGDBClientSecret, config.EnvRomMBaseURL, config.EnvRomMClientAPIToken} {
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(base, "PRIVATE_SOURCE")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	path, err := config.SettingsConfigPath(xdg, filepath.Join(base, "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	// An occupied address forces any incorrect startup to stop, rather than serve.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv(config.EnvServerListenAddr, listener.Addr().String())
	t.Setenv(config.EnvAgentSocketPath, filepath.Join(base, "missing-agent.sock"))
	for _, relative := range []string{"assets", "assets/PRIVATE_STAGING"} {
		staging := filepath.Join(base, relative)
		if err := os.MkdirAll(staging, 0700); err != nil {
			t.Fatal(err)
		}
		databasePath := filepath.Join(base, strings.ReplaceAll(relative, "/", "-")+".db")
		t.Setenv(config.EnvServerDatabasePath, databasePath)
		data, err := json.Marshal(map[string]any{"version": 1, "settings": map[string]any{"local_acquisition": map[string]any{"enabled": true, "source_root": source, "staging_root": staging}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := run(); err != config.ErrInvalidRuntimeSettings {
			t.Errorf("reserved startup did not fail before binding with fixed config error: %v", err)
		}
		if _, err := os.Stat(databasePath); !errors.Is(err, os.ErrNotExist) {
			t.Error("reserved startup opened database")
		}
		if _, err := os.Stat(filepath.Join(staging, ".local-references")); !errors.Is(err, os.ErrNotExist) {
			t.Error("reserved startup created staging state")
		}
	}
}

func TestServerLocalCapabilitiesUseOneRealAdapterAndInvalidRootsFailSafely(t *testing.T) {
	disabled, err := newServerLocalAcquisition(config.Server{LocalAcquisition: config.LocalAcquisitionSettings{SourceRoot: "/missing", StagingRoot: "/missing"}})
	if err != nil || disabled != nil {
		t.Fatal("disabled adapter opened roots")
	}
	base := t.TempDir()
	source, staging := filepath.Join(base, "PRIVATE_SOURCE"), filepath.Join(base, "PRIVATE_STAGING")
	for _, path := range []string{source, staging, filepath.Join(source, "edition-1")} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	settings := config.Server{LocalAcquisition: config.LocalAcquisitionSettings{Enabled: true, SourceRoot: source, StagingRoot: staging}}
	local, err := newServerLocalAcquisition(settings)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	discovery, execution, executor, err := localAcquisitionCapabilities(local)
	if err != nil || executor != local {
		t.Fatal("executor composition mismatch")
	}
	provider, ok := discovery.Lookup(acquisition.LocalProviderID)
	if !ok || provider != local || execution == nil {
		t.Fatal("discovery and execution not composed together")
	}
	// Exercise the production two-adapter switch with both adapters available:
	// exact Local selection must still copy locally, never dispatch to qBit.
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/search" {
			t.Error("Local execution reached qBittorrent")
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer remote.Close()
	p, err := acquisition.NewProwlarr(acquisition.ProwlarrConfig{Enabled: true, BaseURL: remote.URL, APIKey: "PRIVATE_PROWLARR_KEY", ReferenceDirectory: filepath.Join(base, "config", "lernae", "acquisition", "prowlarr")})
	if err != nil {
		t.Fatal(err)
	}
	q, err := acquisition.NewQBittorrent(acquisition.QBittorrentConfig{Enabled: true, BaseURL: remote.URL, Username: "operator", Password: "PRIVATE_PASSWORD"}, p)
	if err != nil {
		t.Fatal(err)
	}
	discovery, execution, executor, err = serverAcquisitionWithQBittorrent(local, p, q)
	if err != nil || execution == nil || executor == nil {
		t.Fatal("two-adapter production composition failed")
	}
	if err := os.WriteFile(filepath.Join(source, "edition-1", "PRIVATE_FILENAME"), []byte("Server production seam bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(context.Background(), filepath.Join(base, "server-local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		`INSERT INTO works (id,medium,work_type,title) VALUES ('work-1','literature','novel','Fixture')`,
		`INSERT INTO editions (id,work_id,format) VALUES ('edition-1','work-1','epub')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	core := newServerAcquisitionExecution(context.Background(), db, execution, executor)
	defer core.Close()
	handler, err := newServerHandlerWithAcquisitionCapabilities(settings, db, nil, nil, nil, serverInventorySources{}, nil, core, discovery, serverUnavailableSearchProvider("openlibrary"), serverUnavailableSearchProvider("tvmaze"), nil)
	if err != nil {
		t.Fatal(err)
	}
	job, err := jobs.NewService(jobs.NewSQLiteRepository(db)).CreateAcquisition(context.Background(), "edition-1")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/acquisitions/"+job.ID+"/candidates", nil))
	var candidates api.AcquisitionCandidatesResponse
	if err := json.Unmarshal(response.Body.Bytes(), &candidates); err != nil || response.Code != 200 || len(candidates.Candidates) != 1 {
		t.Fatalf("productive discovery=%d/%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/acquisitions/"+job.ID+"/selection", strings.NewReader(`{"candidate_handle":"`+candidates.Candidates[0].CandidateHandle+`"}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal("production selection failed")
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/acquisitions/"+job.ID+"/execute", nil))
	if response.Code != 202 {
		t.Fatalf("productive execute=%d/%s", response.Code, response.Body.String())
	}
	core.Wait()
	loaded, err := jobs.NewService(jobs.NewSQLiteRepository(db)).GetAcquisition(context.Background(), job.ID)
	if err != nil || loaded.Status != jobs.StatusSucceeded {
		t.Fatal("productive composition did not finish real copy")
	}
	settings.LocalAcquisition.SourceRoot = filepath.Join(base, "missing")
	if opened, err := newServerLocalAcquisition(settings); err == nil {
		opened.Close()
		t.Fatal("invalid enabled root did not fail startup composition")
	} else if strings.Contains(err.Error(), base) {
		t.Fatal("startup error leaked root")
	}
}

func TestServerAcquisitionRegistryDefaultsToEmpty(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "acquisition-default.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		`INSERT INTO works (id, medium, work_type, title) VALUES ('work-1', 'literature', 'novel', 'Fixture')`,
		`INSERT INTO editions (id, work_id, format) VALUES ('edition-1', 'work-1', 'epub')`,
	} {
		if _, err := db.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	job, err := jobs.NewService(jobs.NewSQLiteRepository(db)).CreateAcquisition(ctx, "edition-1")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newServerHandler(config.Server{}, db, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/acquisitions/"+job.ID+"/candidates", nil))
	var body api.AcquisitionCandidatesResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || body.Outcome != "no_providers_configured" || body.Candidates == nil || len(body.Candidates) != 0 || body.Failures == nil {
		t.Fatalf("default registry response = %d/%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/acquisitions/"+job.ID+"/execute", nil))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "accepted selection") {
		t.Fatalf("default execution response = %d/%s", response.Code, response.Body.String())
	}
	selected := serverSelection(job.ID)
	if _, err := acquisition.NewSQLiteRepository(db).Accept(ctx, selected); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/acquisitions/"+job.ID+"/execute", nil))
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "PRIVATE") {
		t.Fatalf("unwired execution response = %d/%s", response.Code, response.Body.String())
	}
	var reservations int
	if err := db.QueryRow(`SELECT COUNT(*) FROM acquisition_executions`).Scan(&reservations); err != nil || reservations != 0 {
		t.Fatalf("production unavailable path claimed execution: %d, %v", reservations, err)
	}
}

func serverSelection(jobID string) acquisition.Selection {
	return acquisition.Selection{Candidate: acquisition.Candidate{
		JobID: jobID, EditionID: "edition-1", ProviderID: "source", Handle: strings.Repeat("b", 64),
		Option: acquisition.Option{ID: "exact", ExecutionRef: "PRIVATE_SERVER_REFERENCE", Metadata: acquisition.Metadata{Title: "Exact choice"}},
	}, SelectedAt: time.Now().UTC()}
}

type serverExecutionResolver struct{}

func (serverExecutionResolver) ID() string { return "source" }
func (serverExecutionResolver) Resolve(_ context.Context, selected acquisition.Selection) (acquisition.ExecutionPlan, error) {
	return acquisition.PlanForSelection(selected), nil
}

type serverAcquisitionExecutor struct {
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
}

func (e serverAcquisitionExecutor) Dispatch(context.Context, acquisition.ExecutionPlan) (acquisition.DispatchResult, error) {
	return acquisition.DispatchResult{Decision: acquisition.DispatchAccepted, Execution: e}, nil
}

func (e serverAcquisitionExecutor) Wait(ctx context.Context, _ func(acquisition.Progress) error) (acquisition.Completion, error) {
	close(e.started)
	<-ctx.Done()
	close(e.canceled)
	<-e.release
	return acquisition.CompletionInterrupted, ctx.Err()
}

func TestServerAcquisitionShutdownJoinsAcceptedWorkBeforeDatabaseClose(t *testing.T) {
	ctx, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "acquire-shutdown.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		`INSERT INTO works (id, medium, work_type, title) VALUES ('work-1', 'literature', 'novel', 'Fixture')`,
		`INSERT INTO editions (id, work_id, format) VALUES ('edition-1', 'work-1', 'epub')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	jobService := jobs.NewService(jobs.NewSQLiteRepository(db))
	job, err := jobService.CreateAcquisition(ctx, "edition-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquisition.NewSQLiteRepository(db).Accept(ctx, serverSelection(job.ID)); err != nil {
		t.Fatal(err)
	}
	registry, err := acquisition.NewExecutionRegistry(serverExecutionResolver{})
	if err != nil {
		t.Fatal(err)
	}
	executor := serverAcquisitionExecutor{started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	core := newServerAcquisitionExecution(ctx, db, registry, executor)
	var once sync.Once
	release := func() { once.Do(func() { close(executor.release) }) }
	defer func() { release(); core.Close() }()
	handler, err := newServerHandlerWithSearchProvidersAndExecution(config.Server{}, db, nil, nil, nil, serverInventorySources{}, nil, core,
		serverUnavailableSearchProvider("openlibrary"), serverUnavailableSearchProvider("tvmaze"), nil)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/acquisitions/"+job.ID+"/execute", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("injected execute = %d/%s", response.Code, response.Body.String())
	}
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("accepted worker never started")
	}
	shutdownDone := make(chan error, 1)
	closed := make(chan struct{})
	go func() {
		shutdownDone <- shutdownServerAndServices(context.Background(), nil, cancelOwner, nil,
			func() error { core.Close(); return nil },
			func() error {
				loaded, err := jobService.GetAcquisition(context.Background(), job.ID)
				if err != nil || loaded.Status != jobs.StatusInterrupted {
					return errors.New("database close preceded terminal persistence")
				}
				close(closed)
				return nil
			})
	}()
	select {
	case <-executor.canceled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel accepted acquisition")
	}
	select {
	case <-closed:
		t.Fatal("database closed before accepted acquisition joined")
	default:
	}
	release()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown failed to drain acquisition")
	}
}

type serverImmediateLimiter struct{}

func (serverImmediateLimiter) Wait(context.Context) error { return nil }

func TestServerHandlerRoutesBookAndTVDetailsByExactProvider(t *testing.T) {
	var providerRequests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		providerRequests = append(providerRequests, request.Method+" "+request.URL.Path)
		switch request.URL.Path {
		case "/works/OL123W.json":
			_, _ = w.Write([]byte(`{"key":"/works/OL123W","title":"A Book","covers":[12]}`))
		case "/works/OL123W/editions.json":
			_, _ = w.Write([]byte(`{"entries":[]}`))
		case "/shows/42":
			_, _ = w.Write([]byte(`{"id":42,"name":"A Show","url":"https://www.tvmaze.com/shows/42/a-show","language":"English","status":"Running","genres":["Drama"],"premiered":"2021-01-02","network":{"name":"Example Network"}}`))
		case "/shows/42/seasons":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected provider request %s %s", request.Method, request.URL.Path)
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "details.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	books := openlibrary.New(openlibrary.ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: serverImmediateLimiter{}})
	shows := tvmaze.New(tvmaze.ClientConfig{Endpoint: server.URL, HTTPClient: server.Client(), Limiter: serverImmediateLimiter{}})
	handler, err := newServerHandlerWithSearchProviders(config.Server{}, db, nil, nil, nil, serverInventorySources{}, nil, books, shows, nil)
	if err != nil {
		t.Fatalf("newServerHandlerWithSearchProviders() error = %v", err)
	}
	for _, test := range []struct {
		provider   string
		externalID string
		wantTitle  string
	}{
		{provider: "openlibrary", externalID: "OL123W", wantTitle: "A Book"},
		{provider: "tvmaze", externalID: "42", wantTitle: "A Show"},
	} {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/work-details?provider="+test.provider+"&external_id="+test.externalID, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s Details response = %d/%q, want 200", test.provider, response.Code, response.Body.String())
		}
		var details domain.WorkDetails
		if err := json.NewDecoder(response.Body).Decode(&details); err != nil {
			t.Fatal(err)
		}
		if details.Provider != test.provider || details.ExternalID != test.externalID || details.Title != test.wantTitle {
			t.Fatalf("%s Details identity = %#v", test.provider, details)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/work-details?provider=TVMaze&external_id=42", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("noncanonical provider response = %d/%q, want safe 400", response.Code, response.Body.String())
	}
	wantProviderRequests := []string{
		"GET /works/OL123W.json",
		"GET /works/OL123W/editions.json",
		"GET /shows/42",
		"GET /shows/42/seasons",
	}
	if strings.Join(providerRequests, "\n") != strings.Join(wantProviderRequests, "\n") {
		t.Fatalf("provider request sequence = %q, want exactly %q", providerRequests, wantProviderRequests)
	}
}

func TestServerWikidataRelationshipClientIsOptionalAndConfiguredWithoutCallingProvider(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "wikidata-optional.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	missing, err := newServerWikidataRelationshipResolver(db, "   ")
	if err != nil || missing != nil {
		t.Fatalf("blank contact resolver = %#v, error %v; want disabled without startup error", missing, err)
	}
	configured, err := newServerWikidataRelationshipResolver(db, "resolver@example.invalid")
	if err != nil || configured == nil {
		t.Fatalf("configured contact resolver = %#v, error %v; want injected client and parsed cache", configured, err)
	}
}

func TestServerHandlerDefersMissingIGDBCredentialsUntilSearch(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	sources, err := newServerInventorySources(config.Server{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newServerHandlerWithSearchProviders(config.Server{}, db, nil, nil, nil, sources, nil,
		serverUnavailableSearchProvider("openlibrary"),
		serverUnavailableSearchProvider("tvmaze"),
		nil,
	)
	if err != nil {
		t.Fatalf("newServerHandler() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=Soulcalibur", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var searchResponse domain.SearchResponse
	if err := json.NewDecoder(response.Body).Decode(&searchResponse); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(searchResponse.Sources) != 3 {
		t.Fatalf("search response = %d/%#v, want three configured source states", response.Code, searchResponse)
	}
	for index, provider := range []string{"igdb", "openlibrary", "tvmaze"} {
		if searchResponse.Sources[index].Provider != provider || searchResponse.Sources[index].State != domain.SearchSourceUnavailable {
			t.Fatalf("search source[%d] = %#v, want unavailable %s", index, searchResponse.Sources[index], provider)
		}
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/work-details?provider=igdb&external_id=1565", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "not configured") {
		t.Fatalf("details response = %d/%q, want safe missing-configuration response", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/inventory/resolve", strings.NewReader(syntheticInventoryResolveRequest))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "not configured") {
		t.Fatalf("inventory response = %d/%q, want safe missing-configuration response", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/play", strings.NewReader(`{"work_identity":{"provider":"igdb","external_id":"1565"},"edition":{"platform":"gamecube","format":"disc_image"}}`))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "not configured") {
		t.Fatalf("PLAY response = %d/%q, want safe missing-configuration response", response.Code, response.Body.String())
	}
}

func TestServerSearchCompositionPreservesOrderCacheAndProviderStatus(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "search-composition.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	game := serverSearchProviderStub{name: "igdb", results: []domain.MetadataSearchResult{serverSearchResult("igdb")}}
	books := serverSearchProviderStub{name: "openlibrary", err: providers.ErrSearchThrottled}
	tv := serverSearchProviderStub{name: "tvmaze", results: []domain.MetadataSearchResult{serverSearchResult("tvmaze")}}
	search := newServerSearchSource(db, universalsearch.Config{
		OverallTimeout:  500 * time.Millisecond,
		ProviderTimeout: 200 * time.Millisecond,
	}, &game, &books, &tv)

	for attempt := 0; attempt < 2; attempt++ {
		response, err := search.Search(context.Background(), "Composition Test", 5)
		if err != nil {
			t.Fatalf("Search() attempt %d error = %v", attempt+1, err)
		}
		if len(response.Sources) != 3 {
			t.Fatalf("source count = %d, want the three configured providers", len(response.Sources))
		}
		wantProviders := []string{"igdb", "openlibrary", "tvmaze"}
		wantStates := []domain.SearchSourceState{domain.SearchSourceAvailable, domain.SearchSourceThrottled, domain.SearchSourceAvailable}
		for index, want := range wantProviders {
			if response.Sources[index].Provider != want || response.Sources[index].State != wantStates[index] {
				t.Fatalf("source[%d] = %#v, want %s/%s", index, response.Sources[index], want, wantStates[index])
			}
		}
		if len(response.Results) != 2 || response.Results[0].Provider != "igdb" || response.Results[1].Provider != "tvmaze" {
			t.Fatalf("partial results = %#v, want IGDB then TVMaze around throttled Open Library", response.Results)
		}
	}
	if game.calls != 1 || books.calls != 2 || tv.calls != 1 {
		t.Fatalf("provider calls game/books/tv = %d/%d/%d, want cache hits for successes and retry for throttle", game.calls, books.calls, tv.calls)
	}
	for _, source := range []*serverSearchProviderStub{&game, &books, &tv} {
		if len(source.limits) != source.calls {
			t.Fatalf("%s recorded limits = %v for %d calls", source.name, source.limits, source.calls)
		}
		for _, limit := range source.limits {
			if limit != providers.MaxProviderSearchResults {
				t.Errorf("%s received limit %d, want cache-sufficient limit %d", source.name, limit, providers.MaxProviderSearchResults)
			}
		}
		for _, deadline := range source.deadlines {
			remaining := time.Until(deadline)
			if remaining <= 0 || remaining > 200*time.Millisecond {
				t.Errorf("%s provider deadline remaining = %s, want a live deadline within the 200ms provider timeout", source.name, remaining)
			}
		}
	}
}

func TestServerSearchProductionBudgetCoversTVMaze429BackoffAndHTTP(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls++
		timer := time.NewTimer(600 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-request.Context().Done():
			return
		case <-timer.C:
		}
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"show":{"id":99,"name":"Budgeted TVMaze result"}}]`))
	}))
	defer server.Close()

	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "search-budget.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	tvProvider := tvmaze.New(tvmaze.ClientConfig{
		Endpoint:       server.URL,
		HTTPClient:     server.Client(),
		RequestTimeout: 5 * time.Second,
		RetryBackoff:   2 * time.Second,
		Limiter:        &serverPacedLimiter{interval: 500 * time.Millisecond},
	})
	search := newServerSearchSource(db, universalsearch.Config{},
		&serverSearchProviderStub{name: "igdb", results: []domain.MetadataSearchResult{serverSearchResult("igdb")}},
		&serverSearchProviderStub{name: "openlibrary", results: []domain.MetadataSearchResult{serverSearchResult("openlibrary")}},
		tvProvider,
	)
	started := time.Now()
	response, err := search.Search(context.Background(), "The Wheel of Time", 5)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(response.Sources) != 3 || response.Sources[2].Provider != "tvmaze" || response.Sources[2].State != domain.SearchSourceAvailable {
		t.Fatalf("TVMaze source = %#v after %s, want available within production provider budget", response.Sources, elapsed)
	}
	if calls != 2 || !hasSearchProvider(response.Results, "tvmaze") {
		t.Fatalf("retry calls/results = %d/%#v, want exactly one retry and a TVMaze result", calls, response.Results)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("request elapsed = %s, want to complete before 5s overall timeout", elapsed)
	}
}

type serverPacedLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func (limiter *serverPacedLimiter) Wait(ctx context.Context) error {
	limiter.mu.Lock()
	delay := time.Until(limiter.next)
	limiter.next = time.Now().Add(limiter.interval)
	limiter.mu.Unlock()
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func hasSearchProvider(results []domain.MetadataSearchResult, provider string) bool {
	for _, result := range results {
		if result.Provider == provider {
			return true
		}
	}
	return false
}

type serverSearchProviderStub struct {
	name      string
	results   []domain.MetadataSearchResult
	err       error
	calls     int
	limits    []int
	deadlines []time.Time
}

func (source *serverSearchProviderStub) Name() string { return source.name }

func (source *serverSearchProviderStub) Search(ctx context.Context, _ string, limit int) ([]domain.MetadataSearchResult, error) {
	source.calls++
	source.limits = append(source.limits, limit)
	if deadline, ok := ctx.Deadline(); ok {
		source.deadlines = append(source.deadlines, deadline)
	}
	return source.results, source.err
}

func serverSearchResult(provider string) domain.MetadataSearchResult {
	return domain.MetadataSearchResult{
		Provider: provider, ExternalID: provider + "-result-1", Title: provider + " result",
		MediaType: "game", Medium: domain.MediumGame, WorkType: "game",
	}
}

func newServerHandlerWithTestWorkDetails(settings config.Server, db *sql.DB, agentClient agent.StatusClient, playService api.PlayStarter, playJobs api.PlayJobReader) (http.Handler, error) {
	sources, err := newServerInventorySources(settings)
	if err != nil {
		return nil, err
	}
	return newServerHandlerWithSearchProviders(settings, db, agentClient, playService, playJobs, sources, serverTestWorkDetailsSource{},
		serverUnavailableSearchProvider("openlibrary"),
		serverUnavailableSearchProvider("tvmaze"),
		nil,
	)
}

func serverUnavailableSearchProvider(name string) *serverSearchProviderStub {
	return &serverSearchProviderStub{name: name, err: providers.ErrSearchUnavailable}
}

type serverTestWorkDetailsSource struct{}

func (serverTestWorkDetailsSource) GetWorkDetails(_ context.Context, provider, externalID string) (domain.WorkDetails, error) {
	return domain.WorkDetails{
		Provider: provider, ExternalID: externalID, Title: "Trusted Test Work",
		Medium: domain.MediumGame, WorkType: "game",
	}, nil
}

type serverPlayStarterStub struct{}

func (serverPlayStarterStub) Start(playcoordinator.Intent) (playcoordinator.Accepted, error) {
	return playcoordinator.Accepted{JobID: "server-wired-operation"}, nil
}

func TestServerHandlerWiresPlayCoordinatorAndJobsReader(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "server-play.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	jobReader := jobs.NewService(jobs.NewSQLiteRepository(db))
	handler, err := newServerHandler(config.Server{}, db, nil, serverPlayStarterStub{}, jobReader)
	if err != nil {
		t.Fatalf("newServerHandler() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/play", strings.NewReader(`{"work_identity":{"provider":"igdb","external_id":"1565"},"edition":{"platform":"gamecube","format":"disc_image"}}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), "server-wired-operation") {
		t.Fatalf("wired PLAY response = %d/%q, want 202 with injected operation ID", response.Code, response.Body.String())
	}
}

type shutdownPlayStub struct {
	ctx       context.Context
	started   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
	finished  chan struct{}
	events    chan string
	startOnce sync.Once
}

func (stub *shutdownPlayStub) Start(playcoordinator.Intent) (playcoordinator.Accepted, error) {
	stub.startOnce.Do(func() {
		go func() {
			close(stub.started)
			<-stub.ctx.Done()
			stub.events <- "service-cancelled"
			close(stub.cancelled)
			<-stub.release
			stub.events <- "worker-drained"
			close(stub.finished)
		}()
	})
	return playcoordinator.Accepted{JobID: "accepted-shutdown-operation"}, nil
}

func (stub *shutdownPlayStub) Wait() {
	<-stub.finished
}

func TestServerShutdownCancelsAndDrainsAcceptedPlayBeforeClosingDependencies(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "server-shutdown.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	serviceCtx, cancelService := context.WithCancel(context.Background())
	defer cancelService()
	var releaseOnce sync.Once
	play := &shutdownPlayStub{
		ctx: serviceCtx, started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{}),
		finished: make(chan struct{}), events: make(chan string, 4),
	}
	releaseWorker := func() { releaseOnce.Do(func() { close(play.release) }) }
	handler, err := newServerHandler(config.Server{}, db, nil, play, nil)
	if err != nil {
		t.Fatalf("newServerHandler() error = %v", err)
	}
	server := &http.Server{Handler: handler}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	defer func() {
		releaseWorker()
		_ = server.Close()
	}()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	request, err := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/api/v1/play", strings.NewReader(`{"work_identity":{"provider":"igdb","external_id":"1565"},"edition":{"platform":"gamecube","format":"disc_image"}}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("accepted PLAY response = %d, want 202", response.StatusCode)
	}
	select {
	case <-play.started:
	case <-time.After(time.Second):
		t.Fatal("accepted PLAY worker did not start")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelShutdown()
	shutdownDone := make(chan error, 1)
	playbackClosed := make(chan struct{})
	databaseClosed := make(chan struct{})
	go func() {
		shutdownDone <- shutdownServerAndServices(shutdownCtx, server, cancelService, play,
			func() error {
				select {
				case <-play.finished:
					play.events <- "playback-closed"
					close(playbackClosed)
					return nil
				default:
					return errors.New("playback closed before accepted worker drained")
				}
			},
			func() error {
				select {
				case <-playbackClosed:
					play.events <- "database-closed"
					close(databaseClosed)
					return nil
				default:
					return errors.New("database closed before playback")
				}
			})
	}()
	select {
	case <-play.cancelled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel the Server-owned PLAY context")
	}
	if _, err := client.Get("http://" + listener.Addr().String() + "/api/v1/health"); err == nil {
		t.Fatal("Server accepted a new HTTP request after service cancellation")
	}
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown completed before accepted worker drained: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	select {
	case <-playbackClosed:
		t.Fatal("playback closed while accepted worker was still active")
	default:
	}
	select {
	case <-databaseClosed:
		t.Fatal("database closed while accepted worker was still active")
	default:
	}
	releaseWorker()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("orderly server shutdown error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Server shutdown did not finish after accepted worker drained")
	}
	var got []string
	for len(play.events) > 0 {
		got = append(got, <-play.events)
	}
	want := []string{"service-cancelled", "worker-drained", "playback-closed", "database-closed"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("shutdown order = %v, want %v", got, want)
	}
	if err := <-serveResult; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve result = %v, want http.ErrServerClosed", err)
	}
}

func TestServerSessionLauncherPersistsTheConfirmedAgentLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "server-launch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	graph := catalog.WorkGraph{
		Work:     domain.Work{ID: "work-1", Medium: domain.MediumGame, WorkType: "game", Title: "Test Game"},
		Editions: []domain.Edition{{ID: "edition-1", WorkID: "work-1", Platform: "gamecube", Format: "disc_image"}},
		Assets:   []domain.Asset{{ID: "asset-1", EditionID: "edition-1", Kind: "disc_image"}},
	}
	if err := catalog.NewSQLiteRepository(db).CreateGraph(ctx, graph); err != nil {
		t.Fatal(err)
	}
	sessionStore := sessions.NewService(sessions.NewSQLiteRepository(db))
	executor := &serverLifecycleExecutor{sessions: sessionStore, observed: make(chan bool, 1)}
	socketPath := filepath.Join(t.TempDir(), "agent.sock")
	agentServer, err := agent.ListenUDSWithExecutors(socketPath, nil, executor)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- agentServer.Serve() }()
	t.Cleanup(func() {
		if err := agentServer.Close(); err != nil {
			t.Errorf("close fake Agent: %v", err)
		}
		if err := <-served; err != nil {
			t.Errorf("fake Agent Serve() = %v", err)
		}
	})

	launchService, err := newSessionLaunchService(ctx, db, agent.UDSClient{SocketPath: socketPath, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := launchService.Close(); err != nil {
			t.Errorf("close Server launch service: %v", err)
		}
	})
	ended, err := launchService.Launch(agent.LaunchAsset{
		WorkID: "work-1", EditionID: "edition-1", AssetID: "asset-1", PartID: "part-1",
		Medium: domain.MediumGame, Platform: "gamecube", Format: "disc_image", Role: "rom",
		Filename: "game.iso", ExpectedBytes: 4,
	})
	if err != nil {
		t.Fatalf("Server-owned Launch() error = %v", err)
	}
	if ended.Outcome != domain.SessionOutcomeNormalExit {
		t.Fatalf("Server-owned launch outcome = %q, want normal exit", ended.Outcome)
	}
	select {
	case persisted := <-executor.observed:
		if !persisted {
			t.Fatal("Agent process Wait ran before the production Server lifecycle persisted its Session")
		}
	case <-time.After(time.Second):
		t.Fatal("fake Agent did not observe the persisted lifecycle")
	}
	loaded, err := sessionStore.Get(ctx, ended.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.EndedAt == nil || loaded.Outcome != domain.SessionOutcomeNormalExit {
		t.Fatalf("Server-persisted Session = %#v", loaded)
	}
}

func TestRunReconcilesSessionsBeforeBindingHTTPListener(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "server.db")
	db, err := database.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	graph := catalog.WorkGraph{
		Work:     domain.Work{ID: "work-1", Medium: domain.MediumGame, WorkType: "game", Title: "Test Game"},
		Editions: []domain.Edition{{ID: "edition-1", WorkID: "work-1", Platform: "gamecube", Format: "disc_image"}},
		Assets:   []domain.Asset{{ID: "asset-1", EditionID: "edition-1", Kind: "disc_image"}},
	}
	if err := catalog.NewSQLiteRepository(db).CreateGraph(ctx, graph); err != nil {
		t.Fatal(err)
	}
	sessionStore := sessions.NewService(sessions.NewSQLiteRepository(db))
	base := time.Now().UTC().Add(-10 * time.Minute)
	active, err := sessionStore.StartWithID(ctx, "active-session", "work-1", "edition-1", "asset-1", base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	completed, err := sessionStore.StartWithID(ctx, "completed-session", "work-1", "edition-1", "asset-1", base)
	if err != nil {
		t.Fatal(err)
	}
	completedAt := base.Add(time.Minute)
	if err := sessionStore.Finish(ctx, completed.ID, domain.SessionOutcomeNormalExit, completedAt); err != nil {
		t.Fatal(err)
	}
	jobService := jobs.NewService(jobs.NewSQLiteRepository(db))
	reservedJob, err := jobService.CreateAcquisition(ctx, "edition-1")
	if err != nil {
		t.Fatal(err)
	}
	repository := acquisition.NewSQLiteRepository(db)
	selected, err := repository.Accept(ctx, serverSelection(reservedJob.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, won, err := repository.Reserve(ctx, selected, strings.Repeat("a", 64)); err != nil || !won {
		t.Fatalf("startup reservation = %t, %v", won, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	occupiedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupiedListener.Close()
	for _, name := range []string{config.EnvIGDBClientID, config.EnvIGDBClientSecret, config.EnvRomMBaseURL, config.EnvRomMClientAPIToken} {
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("clear %s: %v", name, err)
		}
	}
	t.Setenv("HOME", filepath.Join(t.TempDir(), "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv(config.EnvServerDatabasePath, databasePath)
	t.Setenv(config.EnvServerListenAddr, occupiedListener.Addr().String())
	t.Setenv(config.EnvAgentSocketPath, filepath.Join(t.TempDir(), "missing-agent.sock"))
	t.Setenv(config.EnvInventorySource, config.InventorySourceManifest)
	t.Setenv(config.EnvInventoryManifestPath, "")
	// Force run() to stop at listener binding; reconciled state then proves
	// startup recovery ran before HTTP could begin serving.
	if err := run(); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("run() error = %v, want listener address conflict", err)
	}

	checkDB, err := database.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = checkDB.Close() })
	checkJobs := jobs.NewService(jobs.NewSQLiteRepository(checkDB))
	failedReservation, err := checkJobs.GetAcquisition(ctx, reservedJob.ID)
	if err != nil || failedReservation.Status != jobs.StatusFailed {
		t.Fatalf("claimed queued acquisition before listener bind = %s, %v", failedReservation.Status, err)
	}
	recoveredReservation, exists, err := acquisition.NewSQLiteRepository(checkDB).GetReservation(ctx, reservedJob.ID)
	if err != nil || !exists || recoveredReservation.Outcome != acquisition.DispatchUnconfirmed {
		t.Fatalf("dispatch provenance before listener bind = %#v, %v", recoveredReservation, err)
	}
	checkSessions := sessions.NewService(sessions.NewSQLiteRepository(checkDB))
	reconciled, err := checkSessions.Get(ctx, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.EndedAt == nil || reconciled.Outcome != domain.SessionOutcomeInterrupted {
		t.Fatalf("active Session after startup before listener bind = %#v, want interrupted", reconciled)
	}
	unchanged, err := checkSessions.Get(ctx, completed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.EndedAt == nil || !unchanged.EndedAt.Equal(completedAt) || unchanged.Outcome != domain.SessionOutcomeNormalExit {
		t.Fatalf("completed Session after startup = %#v, want unchanged normal exit at %s", unchanged, completedAt)
	}
}

type serverLifecycleExecutor struct {
	sessions *sessions.Service
	observed chan bool
}

func (executor *serverLifecycleExecutor) Start(_ context.Context, request agent.LaunchAsset) (agent.LaunchProcess, error) {
	return serverLifecycleProcess{sessions: executor.sessions, request: request, observed: executor.observed}, nil
}

type serverLifecycleProcess struct {
	sessions *sessions.Service
	request  agent.LaunchAsset
	observed chan bool
}

func (process serverLifecycleProcess) Wait() agent.LaunchExit {
	loaded, err := process.sessions.Get(context.Background(), process.request.SessionID)
	process.observed <- err == nil && loaded.EndedAt == nil && loaded.Outcome == ""
	code := 0
	return agent.LaunchExit{Outcome: domain.SessionOutcomeNormalExit, ExitCode: &code}
}

func TestServerHandlerWiresConfiguredInventoryManifest(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	const manifest = `{"schema_version":1,"assets":[{"id":"synthetic-asset-42","work":{"title":"Manifest Display","external_ids":{"test-catalog":"record-42"}},"edition":{"platform":"platform-x","format":"disc-image"},"total_size_bytes":4,"parts":[{"role":"data","filename":"synthetic.bin","size_bytes":4}],"locations":[{"class":"archive","provider":"synthetic-storage","locator":"synthetic-private-locator"}]}]}`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	const workID = domain.WorkID("server-test-work")
	const editionID = domain.EditionID("server-test-edition")
	const assetID = domain.AssetID("synthetic-asset-42")
	graph := catalog.WorkGraph{
		Work:     domain.Work{ID: workID, Title: "Trusted Server Work", Medium: domain.MediumGame, WorkType: "game"},
		Editions: []domain.Edition{{ID: editionID, WorkID: workID, Platform: "platform-x", Format: "disc-image"}},
		Assets:   []domain.Asset{{ID: assetID, EditionID: editionID, Kind: "disc-image", TotalSizeBytes: 4}},
		Parts:    []domain.AssetPart{{ID: "server-test-part", AssetID: assetID, Role: "data", Filename: "synthetic.bin", SizeBytes: 4}},
		Locations: []domain.AssetLocation{{
			ID: "server-test-location", AssetID: assetID, StorageProviderID: "synthetic-storage",
			Locator: "synthetic-private-locator", LocationClass: "archive",
		}},
		ExternalIdentities: []domain.ExternalIdentity{{
			ID: "server-test-identity", WorkID: workID, Provider: "test-catalog", ExternalID: "record-42",
		}},
	}
	if err := catalog.NewSQLiteRepository(db).CreateGraph(context.Background(), graph); err != nil {
		t.Fatal(err)
	}
	settings := config.Server{InventoryManifestPath: manifestPath}
	handler, err := newServerHandler(settings, db, nil, nil, nil)
	if err != nil {
		t.Fatalf("newServerHandler() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/inventory/resolve", strings.NewReader(syntheticInventoryResolveRequest))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"owned":true`) || strings.Contains(response.Body.String(), "synthetic-private-locator") {
		t.Fatalf("wired inventory response = %d/%q, want safe successful resolution", response.Code, response.Body.String())
	}
	persisted, err := catalog.NewSQLiteRepository(db).GetWorkByExternalIdentity(context.Background(), "test-catalog", "record-42")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Work.Title != "Trusted Server Work" || persisted.Work.Medium != domain.MediumGame || persisted.Work.WorkType != "game" {
		t.Fatalf("existing canonical Work changed during inventory reuse: %#v", persisted.Work)
	}
}

const syntheticInventoryResolveRequest = `{"work_identity":{"provider":"test-catalog","external_id":"record-42"},"edition":{"platform":"platform-x","format":"disc-image"}}`
