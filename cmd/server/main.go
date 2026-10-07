package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"lernae/internal/acquisition"
	"lernae/internal/agent"
	"lernae/internal/api"
	"lernae/internal/catalog"
	"lernae/internal/config"
	"lernae/internal/database"
	"lernae/internal/domain"
	"lernae/internal/inventory"
	"lernae/internal/jobs"
	"lernae/internal/playback"
	"lernae/internal/playcoordinator"
	"lernae/internal/providers"
	"lernae/internal/providers/igdb"
	"lernae/internal/providers/openlibrary"
	"lernae/internal/providers/tvmaze"
	"lernae/internal/providers/wikidata"
	universalsearch "lernae/internal/search"
	"lernae/internal/searchcache"
	"lernae/internal/sessions"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("Lernae Server stopped with an error", "error", err)
		os.Exit(1)
	}
}

func run() (runErr error) {
	settings, err := config.LoadServer()
	if err != nil {
		return err
	}
	inventorySources, err := newServerInventorySources(settings)
	if err != nil {
		return err
	}
	localAdapter, err := newServerLocalAcquisition(settings)
	if err != nil {
		return err
	}
	if localAdapter != nil {
		defer localAdapter.Close()
	}
	prowlarrAdapter, err := newServerProwlarrAcquisition(settings)
	if err != nil {
		return err
	}
	qbittorrentAdapter, err := newServerQBittorrentAcquisition(settings, prowlarrAdapter)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Open(ctx, settings.DatabasePath)
	if err != nil {
		return err
	}
	slog.Info("SQLite database initialized", "path", settings.DatabasePath)
	var (
		server          *http.Server
		serviceCancel   context.CancelFunc
		launchService   *playback.Service
		playService     *playcoordinator.PlayCoordinator
		acquireService  *acquisition.ExecutionService
		resourcesClosed bool
	)
	closeResources := func(shutdownContext context.Context) error {
		if resourcesClosed {
			return nil
		}
		resourcesClosed = true
		var playWaiter playOperationWaiter
		if playService != nil {
			playWaiter = playService
		}
		return shutdownServerAndServices(shutdownContext, server, serviceCancel, playWaiter,
			func() error {
				if acquireService != nil {
					acquireService.Close()
				}
				if launchService == nil {
					return nil
				}
				if err := launchService.Close(); err != nil {
					return fmt.Errorf("close Server launch service: %w", err)
				}
				return nil
			}, db.Close)
	}
	defer func() {
		if err := closeResources(context.Background()); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}()

	jobService := jobs.NewService(jobs.NewSQLiteRepository(db))
	reconciledJobs, err := jobService.ReconcileStartup(ctx)
	if err != nil {
		return err
	}
	slog.Info("startup job reconciliation complete", "reconciled_jobs", reconciledJobs)
	interruptedSessions, err := reconcileSessionsOnStartup(ctx, db)
	if err != nil {
		return err
	}
	slog.Info("startup Session reconciliation complete", "interrupted_sessions", interruptedSessions)

	agentClient := agent.UDSClient{SocketPath: settings.AgentSocketPath, Timeout: api.AgentStatusTimeout}
	serviceContext, cancelService := context.WithCancel(context.Background())
	serviceCancel = cancelService
	discoveryRegistry, executionRegistry, executor, err := serverAcquisitionWithQBittorrent(localAdapter, prowlarrAdapter, qbittorrentAdapter)
	if err != nil {
		return err
	}
	acquireService = newServerAcquisitionExecution(serviceContext, db, executionRegistry, executor)
	launchService, err = newSessionLaunchService(serviceContext, db, agentClient)
	if err != nil {
		return err
	}
	playService, err = newServerPlayCoordinatorWithSources(serviceContext, db, jobService, agentClient, launchService, inventorySources)
	if err != nil {
		return err
	}
	statusCtx, statusCancel := context.WithTimeout(ctx, api.AgentStatusTimeout)
	_, agentErr := agentClient.GetStatus(statusCtx)
	statusCancel()
	if agentErr != nil {
		slog.Warn("Lernae Agent is offline", "error", agentErr)
	} else {
		slog.Info("Lernae Agent connected over Unix Domain Socket", "socket_path", settings.AgentSocketPath)
	}

	settingsStore, err := config.NewDefaultFileRuntimeSettingsStore()
	if err != nil {
		return err
	}
	openLibraryProvider := openlibrary.New(openlibrary.ClientConfig{MetadataLanguage: string(settings.MetadataLanguage)})
	metadataLanguageSettings := runtimeMetadataLanguageSettings{store: settingsStore, provider: openLibraryProvider}
	handler, err := newServerHandlerWithAcquisitionCapabilities(settings, db, agentClient, playService, jobService, inventorySources, nil, acquireService, discoveryRegistry,
		openLibraryProvider,
		tvmaze.New(tvmaze.ClientConfig{}),
		&metadataLanguageSettings,
	)
	if err != nil {
		return err
	}
	server = &http.Server{
		Addr:              settings.ListenAddress,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// Remote discovery has a bounded 30s budget; permit its safe response
		// to be written without raising other providers' core call deadlines.
		WriteTimeout: 40 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	listener, err := net.Listen("tcp", settings.ListenAddress)
	if err != nil {
		return err
	}
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- server.Serve(listener)
	}()
	slog.Info("Lernae Server started", "listen_address", settings.ListenAddress)

	select {
	case <-ctx.Done():
		slog.Info("Lernae Server shutdown requested")
	case err := <-serveResult:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("serve Server HTTP: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return errors.Join(runErr, closeResources(shutdownCtx))
}

type playOperationWaiter interface {
	Wait()
}

// shutdownServerAndServices rejects new HTTP work, cancels the Server-owned
// operation context, drains accepted PLAY workers, then closes their playback
// and persistence dependencies. Worker draining is intentionally unbounded:
// an HTTP shutdown deadline must never close SQLite beneath accepted work.
func shutdownServerAndServices(
	shutdownContext context.Context,
	server *http.Server,
	cancelService context.CancelFunc,
	playService playOperationWaiter,
	closePlayback func() error,
	closeDatabase func() error,
) error {
	var shutdownErr error
	if server != nil {
		if err := server.Shutdown(shutdownContext); err != nil {
			shutdownErr = err
			if closeErr := server.Close(); closeErr != nil {
				shutdownErr = errors.Join(shutdownErr, closeErr)
			}
		}
	}
	if cancelService != nil {
		cancelService()
	}
	if playService != nil {
		playService.Wait()
	}
	if closePlayback != nil {
		if err := closePlayback(); err != nil {
			shutdownErr = errors.Join(shutdownErr, err)
		}
	}
	if closeDatabase != nil {
		if err := closeDatabase(); err != nil {
			shutdownErr = errors.Join(shutdownErr, err)
		}
	}
	return shutdownErr
}

func newSessionLaunchService(parent context.Context, db *sql.DB, client agent.UDSClient) (*playback.Service, error) {
	if db == nil {
		return nil, errors.New("Server Session database is required")
	}
	return playback.NewService(parent, client, sessions.NewService(sessions.NewSQLiteRepository(db)))
}

// reconcileSessionsOnStartup assumes this Server is the sole owner of the
// SQLite database, matching Jobs startup reconciliation. Multiple Servers
// sharing one database could otherwise interrupt a Session owned by a peer.
func reconcileSessionsOnStartup(ctx context.Context, db *sql.DB) (int64, error) {
	if db == nil {
		return 0, errors.New("Server Session database is required")
	}
	interrupted, err := sessions.NewService(sessions.NewSQLiteRepository(db)).ReconcileStale(ctx, time.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("reconcile startup Sessions: %w", err)
	}
	return interrupted, nil
}

func newServerInventorySources(settings config.Server) (serverInventorySources, error) {
	switch settings.InventorySource {
	case "", config.InventorySourceManifest:
		if settings.InventoryManifestPath == "" {
			return serverInventorySources{}, nil
		}
		manifestSource := inventory.NewManifestInventorySource(settings.InventoryManifestPath)
		return serverInventorySources{inventory: manifestSource, storage: manifestSource}, nil
	case config.InventorySourceRomM:
		rommSource, err := inventory.NewRomMInventorySource(inventory.RomMInventorySourceOptions{
			BaseURL: settings.RomM.BaseURL, Token: settings.RomM.ClientAPIToken,
		})
		if err != nil {
			return serverInventorySources{}, fmt.Errorf("configure RomM inventory source: %w", err)
		}
		return serverInventorySources{inventory: rommSource, storage: noTrustedStorageSource{}}, nil
	default:
		return serverInventorySources{}, config.ErrInvalidInventorySource
	}
}

type serverInventorySources struct {
	inventory providers.InventorySource
	storage   providers.StorageSource
}

// noTrustedStorageSource makes the RomM pairing explicit without deriving
// restore locations from RomM's local filesystem paths.
type noTrustedStorageSource struct{}

func (noTrustedStorageSource) LocationsForAsset(context.Context, domain.AssetID) ([]domain.AssetLocation, error) {
	return []domain.AssetLocation{}, nil
}

func newServerHandler(settings config.Server, db *sql.DB, agentClient agent.StatusClient, playService api.PlayStarter, playJobs api.PlayJobReader) (http.Handler, error) {
	sources, err := newServerInventorySources(settings)
	if err != nil {
		return nil, err
	}
	return newServerHandlerWithSources(settings, db, agentClient, playService, playJobs, sources, nil)
}

func newServerHandlerWithSources(settings config.Server, db *sql.DB, agentClient agent.StatusClient, playService api.PlayStarter, playJobs api.PlayJobReader, sources serverInventorySources, detailsSource providers.WorkDetailsSource) (http.Handler, error) {
	return newServerHandlerWithSearchProviders(settings, db, agentClient, playService, playJobs, sources, detailsSource,
		openlibrary.New(openlibrary.ClientConfig{MetadataLanguage: string(config.EffectiveMetadataLanguage(settings.MetadataLanguage))}),
		tvmaze.New(tvmaze.ClientConfig{}),
		nil,
	)
}

func newServerHandlerWithSearchProviders(settings config.Server, db *sql.DB, agentClient agent.StatusClient, playService api.PlayStarter, playJobs api.PlayJobReader, sources serverInventorySources, detailsSource providers.WorkDetailsSource, openLibraryProvider, tvMazeProvider providers.SearchProvider, metadataLanguageSettings api.MetadataLanguagePreference) (http.Handler, error) {
	return newServerHandlerWithSearchProvidersAndExecution(settings, db, agentClient, playService, playJobs, sources, detailsSource,
		newServerAcquisitionExecution(context.Background(), db, nil, nil), openLibraryProvider, tvMazeProvider, metadataLanguageSettings)
}

func newServerHandlerWithSearchProvidersAndExecution(settings config.Server, db *sql.DB, agentClient agent.StatusClient, playService api.PlayStarter, playJobs api.PlayJobReader, sources serverInventorySources, detailsSource providers.WorkDetailsSource, execution api.AcquisitionExecutionService, openLibraryProvider, tvMazeProvider providers.SearchProvider, metadataLanguageSettings api.MetadataLanguagePreference) (http.Handler, error) {
	return newServerHandlerWithAcquisitionCapabilities(settings, db, agentClient, playService, playJobs, sources, detailsSource, execution, nil, openLibraryProvider, tvMazeProvider, metadataLanguageSettings)
}

// Production supplies explicit discovery and execution capabilities. Prowlarr
// discovery alone never implicitly gains execution authority.
// Convenience handlers remain explicitly unconfigured and own no fs handles.
func newServerHandlerWithAcquisitionCapabilities(settings config.Server, db *sql.DB, agentClient agent.StatusClient, playService api.PlayStarter, playJobs api.PlayJobReader, sources serverInventorySources, detailsSource providers.WorkDetailsSource, execution api.AcquisitionExecutionService, acquisitionRegistry *acquisition.Registry, openLibraryProvider, tvMazeProvider providers.SearchProvider, metadataLanguageSettings api.MetadataLanguagePreference) (http.Handler, error) {
	metadataSource := igdb.New(igdb.ClientConfig{Credentials: settings.IGDB})
	if detailsSource == nil {
		detailsByProvider := map[string]providers.WorkDetailsSource{"igdb": metadataSource}
		if source, ok := openLibraryProvider.(providers.WorkDetailsSource); ok {
			detailsByProvider["openlibrary"] = source
		}
		if source, ok := tvMazeProvider.(providers.WorkDetailsSource); ok {
			detailsByProvider["tvmaze"] = source
		}
		detailsSource = providers.NewWorkDetailsRouter(detailsByProvider)
	}
	igdbSearchProvider := universalsearch.MetadataAdapter{Provider: "igdb", Source: metadataSource}
	searchSource := newServerSearchSource(db, universalsearch.Config{}, igdbSearchProvider, openLibraryProvider, tvMazeProvider)
	var inventoryBinder *catalog.InventoryBinder
	if sources.inventory != nil || sources.storage != nil {
		if sources.inventory == nil || sources.storage == nil {
			return nil, errors.New("Server inventory source pair is incomplete")
		}
		inventoryBinder = catalog.NewInventoryBinder(catalog.NewSQLiteRepository(db), sources.inventory, sources.storage)
	}
	// Acquisition configuration never derives from metadata or inventory.
	if acquisitionRegistry == nil {
		acquisitionRegistry, _ = acquisition.NewRegistry()
	}
	universeService := newServerUniverseApplication(db, settings.WikidataContactEmail)
	handler := api.StatusHandler{
		Database: db, Agent: agentClient, Metadata: metadataSource, Search: searchSource, Details: detailsSource,
		Inventory: inventoryBinder, Universes: universeService,
		Play: playService, PlayJobs: playJobs,
		Acquisitions:             jobs.NewService(jobs.NewSQLiteRepository(db)),
		AcquisitionCandidates:    newServerAcquisitionCandidates(db, acquisitionRegistry),
		MetadataLanguageSettings: metadataLanguageSettings,
	}.Routes()
	return api.WithAcquisitionExecution(handler, execution), nil
}

func newServerLocalAcquisition(settings config.Server) (*acquisition.Local, error) {
	if !settings.LocalAcquisition.Enabled {
		return nil, nil
	}
	return acquisition.OpenLocal(settings.LocalAcquisition.SourceRoot, settings.LocalAcquisition.StagingRoot)
}

func newServerProwlarrAcquisition(settings config.Server) (*acquisition.Prowlarr, error) {
	if !settings.Prowlarr.Enabled || settings.Prowlarr.BaseURL == "" || !settings.ProwlarrCredentials.HasCredentials() {
		return nil, nil
	}
	return acquisition.NewProwlarr(acquisition.ProwlarrConfig{
		Enabled: true, BaseURL: settings.Prowlarr.BaseURL, APIKey: settings.ProwlarrCredentials.APIKey,
		ReferenceDirectory: string(settings.ProwlarrReferenceDirectory),
	})
}

func newServerQBittorrentAcquisition(settings config.Server, prowlarr *acquisition.Prowlarr) (*acquisition.QBittorrent, error) {
	if prowlarr == nil || !settings.QBittorrent.Enabled || settings.QBittorrent.BaseURL == "" || settings.QBittorrent.Username == "" || !settings.QBittorrentCredentials.HasCredentials() {
		return nil, nil
	}
	return acquisition.NewQBittorrent(acquisition.QBittorrentConfig{
		Enabled: true, BaseURL: settings.QBittorrent.BaseURL,
		Username: settings.QBittorrent.Username, Password: settings.QBittorrentCredentials.Password,
	}, prowlarr)
}

// Discovery-only composition remains explicit when no qBittorrent is supplied.
func serverAcquisitionCapabilities(local *acquisition.Local, prowlarr *acquisition.Prowlarr) (*acquisition.Registry, *acquisition.ExecutionRegistry, acquisition.Executor, error) {
	return serverAcquisitionWithQBittorrent(local, prowlarr, nil)
}

func serverAcquisitionWithQBittorrent(local *acquisition.Local, prowlarr *acquisition.Prowlarr, qbittorrent *acquisition.QBittorrent) (*acquisition.Registry, *acquisition.ExecutionRegistry, acquisition.Executor, error) {
	var resolvers []acquisition.ExecutionResolver
	if local != nil {
		resolvers = append(resolvers, local)
	}
	if qbittorrent != nil && qbittorrent.Configured() && prowlarr != nil {
		resolvers = append(resolvers, acquisition.NewProwlarrExecutionResolver(prowlarr))
	} else {
		qbittorrent = nil
	}
	var execution *acquisition.ExecutionRegistry
	var executor acquisition.Executor
	if len(resolvers) > 0 {
		var err error
		execution, err = acquisition.NewExecutionRegistry(resolvers...)
		if err != nil {
			return nil, nil, nil, err
		}
		executor = serverTwoAcquisitionAdapters{local: local, qbittorrent: qbittorrent}
	}
	var providers []acquisition.Provider
	if local != nil {
		providers = append(providers, local)
	}
	if prowlarr != nil {
		providers = append(providers, prowlarr)
	}
	discovery, err := acquisition.NewRegistry(providers...)
	return discovery, execution, executor, err
}

// Only these two concrete adapters are supported; no plugin/router registry.
type serverTwoAcquisitionAdapters struct {
	local       *acquisition.Local
	qbittorrent *acquisition.QBittorrent
}

func (e serverTwoAcquisitionAdapters) ContentVerificationTimeouts() acquisition.ContentVerificationPolicy {
	if e.local != nil {
		return e.local.ContentVerificationTimeouts()
	}
	return e.qbittorrent.ContentVerificationTimeouts()
}

func (e serverTwoAcquisitionAdapters) Dispatch(ctx context.Context, plan acquisition.ExecutionPlan) (acquisition.DispatchResult, error) {
	switch plan.ProviderID {
	case acquisition.LocalProviderID:
		if e.local != nil {
			return e.local.Dispatch(ctx, plan)
		}
	case acquisition.ProwlarrProviderID:
		if e.qbittorrent != nil {
			return e.qbittorrent.Dispatch(ctx, plan)
		}
	}
	return acquisition.DispatchResult{Decision: acquisition.DispatchRejected}, nil
}

func localAcquisitionCapabilities(local *acquisition.Local) (*acquisition.Registry, *acquisition.ExecutionRegistry, acquisition.Executor, error) {
	if local == nil {
		discovery, err := acquisition.NewRegistry()
		return discovery, nil, nil, err
	}
	discovery, err := acquisition.NewRegistry(local)
	if err != nil {
		return nil, nil, nil, err
	}
	execution, err := acquisition.NewExecutionRegistry(local)
	return discovery, execution, local, err
}

// newServerAcquisitionExecution is the explicit typed execution injection seam.
// Its default has no adapters; it can only return safe preaccept errors.
func newServerAcquisitionExecution(parent context.Context, db *sql.DB, registry *acquisition.ExecutionRegistry, executor acquisition.Executor) *acquisition.ExecutionService {
	return acquisition.NewExecutionService(parent, jobs.NewService(jobs.NewSQLiteRepository(db)), acquisition.NewSQLiteRepository(db), registry, executor)
}

// newServerAcquisitionCandidates is the explicit acquisition registry injection
// seam. Metadata/inventory configuration cannot implicitly populate this registry.
func newServerAcquisitionCandidates(db *sql.DB, registry *acquisition.Registry) *acquisition.Service {
	return acquisition.NewService(jobs.NewService(jobs.NewSQLiteRepository(db)), acquisition.NewSQLiteRepository(db), registry)
}

func newServerUniverseApplication(db *sql.DB, contactEmail string) *catalog.UniverseApplication {
	repository := catalog.NewSQLiteRepository(db)
	resolver, err := newServerWikidataRelationshipResolver(db, contactEmail)
	if err != nil {
		slog.Warn("Wikidata relationship resolution is disabled", "reason", "invalid_contact_identity")
	}
	return catalog.NewUniverseApplicationWithRelationshipResolver(repository, resolver)
}

func newServerWikidataRelationshipResolver(db *sql.DB, contactEmail string) (*catalog.UniverseRelationshipResolver, error) {
	contactEmail = strings.TrimSpace(contactEmail)
	if contactEmail == "" {
		return nil, nil
	}
	client, err := wikidata.NewClient(wikidata.ClientConfig{ContactEmail: contactEmail})
	if err != nil {
		return nil, err
	}
	return catalog.NewUniverseRelationshipResolver(catalog.NewSQLiteRepository(db), client, catalog.NewSQLiteRelationshipMetadataCache(db)), nil
}

// newServerSearchSource is the shared production/test composition seam. Keep
// provider order explicit: IGDB, Open Library, then TVMaze. Each source gets
// the same durable provider cache before aggregation.
func newServerSearchSource(db *sql.DB, config universalsearch.Config, igdbProvider, openLibraryProvider, tvMazeProvider providers.SearchProvider) *universalsearch.Aggregator {
	providerSources := []providers.SearchProvider{igdbProvider, openLibraryProvider, tvMazeProvider}
	cachedProviders := make([]providers.SearchProvider, 0, len(providerSources))
	for _, provider := range providerSources {
		name := ""
		if provider != nil {
			name = provider.Name()
		}
		cachedProviders = append(cachedProviders, searchcache.NewProviderCache(provider, db, name, searchcache.ResultSchemaVersion(name)))
	}
	return universalsearch.NewAggregator(config, cachedProviders...)
}

func newServerPlayCoordinator(
	serverContext context.Context,
	settings config.Server,
	db *sql.DB,
	jobService *jobs.Service,
	agentClient agent.UDSClient,
	launchService *playback.Service,
) (*playcoordinator.PlayCoordinator, error) {
	sources, err := newServerInventorySources(settings)
	if err != nil {
		return nil, err
	}
	return newServerPlayCoordinatorWithSources(serverContext, db, jobService, agentClient, launchService, sources)
}

func newServerPlayCoordinatorWithSources(
	serverContext context.Context,
	db *sql.DB,
	jobService *jobs.Service,
	agentClient agent.UDSClient,
	launchService *playback.Service,
	sources serverInventorySources,
) (*playcoordinator.PlayCoordinator, error) {
	if sources.inventory == nil && sources.storage == nil {
		return nil, nil
	}
	return playcoordinator.NewService(
		serverContext,
		catalog.NewSQLiteRepository(db),
		sources.inventory,
		sources.storage,
		jobService,
		agentClient,
		launchService,
	)
}
