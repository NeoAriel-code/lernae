// Package config loads explicit process configuration with XDG-based defaults.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"lernae/internal/acquisition"
)

const (
	EnvServerDatabasePath    = "LERNAE_SERVER_DB_PATH"
	EnvServerListenAddr      = "LERNAE_SERVER_LISTEN_ADDR"
	EnvAgentSocketPath       = "LERNAE_AGENT_SOCKET_PATH"
	EnvAgentCachePath        = "LERNAE_AGENT_CACHE_PATH"
	EnvAgentStagingPath      = "LERNAE_AGENT_STAGING_PATH"
	EnvIGDBClientID          = "LERNAE_IGDB_CLIENT_ID"
	EnvIGDBClientSecret      = "LERNAE_IGDB_CLIENT_SECRET"
	EnvWikidataContactEmail  = "LERNAE_WIKIDATA_CONTACT_EMAIL"
	EnvInventoryManifestPath = "LERNAE_INVENTORY_MANIFEST_PATH"
	EnvInventorySource       = "LERNAE_INVENTORY_SOURCE"
	EnvRomMBaseURL           = "LERNAE_ROMM_BASE_URL"
	EnvRomMClientAPIToken    = "LERNAE_ROMM_CLIENT_API_TOKEN"

	InventorySourceManifest    = "manifest"
	InventorySourceRomM        = "romm"
	DefaultServerListenAddress = "127.0.0.1:8081"
)

var (
	ErrIGDBCredentialsMissing         = errors.New("IGDB credentials are not configured")
	ErrIGDBCredentialsIncomplete      = errors.New("IGDB credentials are incomplete")
	ErrProviderConfigUnavailable      = errors.New("provider config is unsafe or invalid; repair the local provider config and retry")
	ErrInvalidInventorySource         = errors.New("inventory source must be manifest or romm")
	ErrRomMCredentialsMissing         = errors.New("RomM base URL and Client API Token are required when RomM inventory is selected")
	ErrRomMCredentialsIncomplete      = errors.New("RomM credentials are incomplete")
	ErrServerListenAddressNotLoopback = errors.New("server listen address must use the literal loopback IP 127.0.0.1 or ::1")
	ErrAgentStagingPathConflict       = errors.New("agent staging path must not overlap the final Asset cache namespace")
	ErrInvalidRuntimeSettings         = errors.New("runtime settings are invalid")
)

type Server struct {
	DatabasePath               string
	AgentSocketPath            string
	ListenAddress              string
	MetadataLanguage           MetadataLanguage
	InventoryManifestPath      string
	InventorySource            string
	RomM                       RomMCredentials
	IGDB                       IGDBCredentials
	WikidataContactEmail       string                     `json:"-"`
	LocalAcquisition           LocalAcquisitionSettings   `json:"-"`
	Prowlarr                   ProwlarrSettings           `json:"-"`
	ProwlarrCredentials        ProwlarrCredentials        `json:"-"`
	ProwlarrReferenceDirectory ProwlarrReferenceDirectory `json:"-"`
	QBittorrent                QBittorrentSettings        `json:"-"`
	QBittorrentCredentials     QBittorrentCredentials     `json:"-"`
}

// RomMCredentials are read only when RomM is explicitly selected as the
// Server inventory source. Their formatted and JSON representations are
// deliberately redacted.
type RomMCredentials struct {
	BaseURL        string `json:"-"`
	ClientAPIToken string `json:"-"`
}

func (credentials RomMCredentials) HasCredentials() bool {
	return strings.TrimSpace(credentials.BaseURL) != "" && strings.TrimSpace(credentials.ClientAPIToken) != ""
}

func (RomMCredentials) String() string {
	return "RomMCredentials{configured:<redacted>}"
}

func (credentials RomMCredentials) GoString() string {
	return credentials.String()
}

// IGDBCredentials are loaded for the Server integration only. Formatting is
// deliberately redacted so accidental diagnostic formatting cannot reveal a
// configured value.
type IGDBCredentials struct {
	ClientID     string `json:"-"`
	ClientSecret string `json:"-"`
}

func (credentials IGDBCredentials) HasCredentials() bool {
	return strings.TrimSpace(credentials.ClientID) != "" && strings.TrimSpace(credentials.ClientSecret) != ""
}

// Validate reports presence errors without including either credential value.
// Credentials remain optional at server startup and are validated when IGDB is
// used.
func (credentials IGDBCredentials) Validate() error {
	hasClientID := strings.TrimSpace(credentials.ClientID) != ""
	hasClientSecret := strings.TrimSpace(credentials.ClientSecret) != ""
	switch {
	case !hasClientID && !hasClientSecret:
		return ErrIGDBCredentialsMissing
	case !hasClientID || !hasClientSecret:
		return ErrIGDBCredentialsIncomplete
	default:
		return nil
	}
}

func (IGDBCredentials) String() string {
	return "IGDBCredentials{configured:<redacted>}"
}

func (credentials IGDBCredentials) GoString() string {
	return credentials.String()
}

type Agent struct {
	SocketPath  string
	CachePath   string
	StagingPath string
}

func LoadServer() (Server, error) {
	runtimeSettings, err := loadRuntimeSettings()
	if err != nil {
		return Server{}, err
	}
	if err := ValidateLocalAcquisitionSettings(runtimeSettings.LocalAcquisition); err != nil {
		return Server{}, err
	}
	if runtimeSettings.Metadata.Language != "" && !runtimeSettings.Metadata.Language.Valid() {
		return Server{}, ErrInvalidRuntimeSettings
	}
	prowlarr, prowlarrCredentials, prowlarrDirectory, err := loadProwlarrSettings(runtimeSettings.Prowlarr)
	if err != nil {
		return Server{}, err
	}
	qbittorrent, qbCredentials, err := loadQBittorrentSettings(runtimeSettings.QBittorrent)
	if err != nil {
		return Server{}, err
	}
	dataDir, err := xdgDataDir()
	if err != nil {
		return Server{}, err
	}
	socketPath, err := defaultSocketPath()
	if err != nil {
		return Server{}, err
	}

	databasePath, err := configuredAbsolutePath(EnvServerDatabasePath, runtimeSettings.Server.DatabasePath, filepath.Join(dataDir, "lernae", "lernae.db"))
	if err != nil {
		return Server{}, err
	}
	socketPath, err = configuredAbsolutePath(EnvAgentSocketPath, runtimeSettings.Agent.SocketPath, socketPath)
	if err != nil {
		return Server{}, err
	}
	manifestPath, err := configuredOptionalAbsolutePath(EnvInventoryManifestPath, runtimeSettings.Inventory.ManifestPath)
	if err != nil {
		return Server{}, err
	}
	inventorySource, err := selectedInventorySource(resolveRuntimeValue(EnvInventorySource, runtimeSettings.Inventory.Source, InventorySourceManifest))
	if err != nil {
		return Server{}, err
	}
	igdb, romm, err := resolveServerProviderCredentials(inventorySource)
	if err != nil {
		return Server{}, err
	}
	listenAddress := resolveRuntimeValue(EnvServerListenAddr, runtimeSettings.Server.ListenAddress, DefaultServerListenAddress)
	if err := validateServerListenAddress(listenAddress); err != nil {
		return Server{}, err
	}
	return Server{
		DatabasePath:               databasePath,
		AgentSocketPath:            socketPath,
		ListenAddress:              listenAddress,
		MetadataLanguage:           EffectiveMetadataLanguage(runtimeSettings.Metadata.Language),
		InventoryManifestPath:      manifestPath,
		InventorySource:            inventorySource,
		RomM:                       romm,
		IGDB:                       igdb,
		WikidataContactEmail:       resolveRuntimeValue(EnvWikidataContactEmail, runtimeSettings.Wikidata.ContactEmail, ""),
		LocalAcquisition:           runtimeSettings.LocalAcquisition,
		Prowlarr:                   prowlarr,
		ProwlarrCredentials:        prowlarrCredentials,
		ProwlarrReferenceDirectory: prowlarrDirectory,
		QBittorrent:                qbittorrent,
		QBittorrentCredentials:     qbCredentials,
	}, nil
}

func resolveServerProviderCredentials(inventorySource string) (IGDBCredentials, RomMCredentials, error) {
	igdbID, igdbSecret, igdbFromEnvironment := environmentBundle(EnvIGDBClientID, EnvIGDBClientSecret)
	rommURL, rommToken, rommFromEnvironment := environmentBundle(EnvRomMBaseURL, EnvRomMClientAPIToken)

	igdbEnvironment := IGDBCredentials{ClientID: igdbID, ClientSecret: igdbSecret}
	if igdbFromEnvironment && !igdbEnvironment.HasCredentials() {
		return IGDBCredentials{}, RomMCredentials{}, ErrIGDBCredentialsIncomplete
	}
	rommEnvironment := RomMCredentials{BaseURL: rommURL, ClientAPIToken: rommToken}
	if rommFromEnvironment && !rommEnvironment.HasCredentials() {
		return IGDBCredentials{}, RomMCredentials{}, ErrRomMCredentialsIncomplete
	}

	needsPersistedIGDB := !igdbFromEnvironment
	needsPersistedRomM := inventorySource == InventorySourceRomM && !rommFromEnvironment
	var persisted ProviderConfig
	if needsPersistedIGDB || needsPersistedRomM {
		store, err := NewDefaultFileProviderConfigStore()
		if err != nil {
			return IGDBCredentials{}, RomMCredentials{}, ErrProviderConfigUnavailable
		}
		persisted, err = store.Load()
		if err != nil {
			return IGDBCredentials{}, RomMCredentials{}, ErrProviderConfigUnavailable
		}
	}

	igdb := igdbEnvironment
	if !igdbFromEnvironment {
		igdb = persisted.IGDB
		if err := igdb.Validate(); err != nil && !errors.Is(err, ErrIGDBCredentialsMissing) {
			return IGDBCredentials{}, RomMCredentials{}, ErrIGDBCredentialsIncomplete
		}
	}

	romm := RomMCredentials{}
	if rommFromEnvironment {
		romm = rommEnvironment
	} else if needsPersistedRomM {
		romm = persisted.RomM
		if !romm.HasCredentials() {
			if strings.TrimSpace(romm.BaseURL) != "" || strings.TrimSpace(romm.ClientAPIToken) != "" {
				return IGDBCredentials{}, RomMCredentials{}, ErrRomMCredentialsIncomplete
			}
			return IGDBCredentials{}, RomMCredentials{}, ErrRomMCredentialsMissing
		}
	}
	return igdb, romm, nil
}

func environmentBundle(firstName, secondName string) (first, second string, present bool) {
	first, firstPresent := os.LookupEnv(firstName)
	second, secondPresent := os.LookupEnv(secondName)
	return first, second, firstPresent || secondPresent
}

func loadRuntimeSettings() (RuntimeSettings, error) {
	store, err := NewDefaultFileRuntimeSettingsStore()
	if err != nil {
		return RuntimeSettings{}, err
	}
	return store.Load()
}

// ValidateLocalAcquisitionSettings validates the persisted shape without I/O.
// Enabled roots are opened and checked at Server composition before serving.
// This permits operators to disable a provider whose directories disappeared.
func ValidateLocalAcquisitionSettings(settings LocalAcquisitionSettings) error {
	if settings.StagingRoot != "" && acquisition.ValidateLocalStagingRoot(settings.StagingRoot) != nil {
		return ErrInvalidRuntimeSettings
	}
	for _, path := range []string{settings.SourceRoot, settings.StagingRoot} {
		if path != "" && (!filepath.IsAbs(path) || path != filepath.Clean(path) || path == "/" || strings.ContainsAny(path, "\x00\\")) {
			return ErrInvalidRuntimeSettings
		}
	}
	if settings.Enabled || (settings.SourceRoot != "" && settings.StagingRoot != "") {
		if acquisition.ValidateLocalRoots(settings.SourceRoot, settings.StagingRoot) != nil {
			return ErrInvalidRuntimeSettings
		}
	}
	return nil
}

// ValidateRuntimeSettings checks explicitly persisted values without applying
// Lernae-owned environment overrides. Empty fields remain unset for runtime
// default resolution.
func ValidateRuntimeSettings(settings RuntimeSettings) error {
	if err := ValidateQBittorrentSettings(settings.QBittorrent); err != nil {
		return err
	}
	if err := ValidateProwlarrSettings(settings.Prowlarr); err != nil {
		return err
	}
	if err := ValidateLocalAcquisitionSettings(settings.LocalAcquisition); err != nil {
		return err
	}
	if settings.Metadata.Language != "" && !settings.Metadata.Language.Valid() {
		return ErrInvalidRuntimeSettings
	}
	for _, value := range []struct{ name, path string }{
		{"server database path", settings.Server.DatabasePath},
		{"agent socket path", settings.Agent.SocketPath},
		{"agent cache path", settings.Agent.CachePath},
		{"agent staging path", settings.Agent.StagingPath},
		{"inventory Manifest path", settings.Inventory.ManifestPath},
	} {
		if value.path != "" {
			if _, err := requireAbsolute(value.name, value.path); err != nil {
				return ErrInvalidRuntimeSettings
			}
		}
	}
	if settings.Server.ListenAddress != "" && validateServerListenAddress(settings.Server.ListenAddress) != nil {
		return ErrInvalidRuntimeSettings
	}
	if settings.Inventory.Source != "" {
		if _, err := selectedInventorySource(settings.Inventory.Source); err != nil {
			return ErrInvalidRuntimeSettings
		}
	}
	if settings.Agent.StagingPath != "" {
		cachePath := settings.Agent.CachePath
		if cachePath == "" {
			cacheDir, err := xdgCacheDir()
			if err != nil {
				return ErrInvalidRuntimeSettings
			}
			cachePath = filepath.Join(cacheDir, "lernae", "cache")
		}
		rel, err := filepath.Rel(filepath.Clean(cachePath), filepath.Clean(settings.Agent.StagingPath))
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ErrInvalidRuntimeSettings
		}
		if components := strings.Split(filepath.ToSlash(rel), "/"); len(components) > 0 && strings.EqualFold(components[0], "assets") {
			return ErrAgentStagingPathConflict
		}
	}
	return nil
}

func resolveRuntimeValue(environmentName, persisted, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(environmentName)); value != "" {
		return value
	}
	if value := strings.TrimSpace(persisted); value != "" {
		return value
	}
	return fallback
}

func configuredAbsolutePath(environmentName, persisted, fallback string) (string, error) {
	return requireAbsolute(environmentName, resolveRuntimeValue(environmentName, persisted, fallback))
}

func validateServerListenAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "" || (host != "127.0.0.1" && host != "::1") {
		return ErrServerListenAddressNotLoopback
	}
	return nil
}

// ResolvedServerAPIProxyTarget returns the effective, validated loopback
// Server address as an HTTP URL for local development tools such as Vite.
func ResolvedServerAPIProxyTarget() (string, error) {
	runtimeSettings, err := loadRuntimeSettings()
	if err != nil {
		return "", err
	}
	address := resolveRuntimeValue(EnvServerListenAddr, runtimeSettings.Server.ListenAddress, DefaultServerListenAddress)
	if err := validateServerListenAddress(address); err != nil {
		return "", err
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", ErrServerListenAddressNotLoopback
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

func selectedInventorySource(value string) (string, error) {
	selector := strings.TrimSpace(value)
	if selector == "" {
		return InventorySourceManifest, nil
	}
	switch selector {
	case InventorySourceManifest, InventorySourceRomM:
		return selector, nil
	default:
		return "", ErrInvalidInventorySource
	}
}

func LoadAgent() (Agent, error) {
	runtimeSettings, err := loadRuntimeSettings()
	if err != nil {
		return Agent{}, err
	}
	cacheDir, err := xdgCacheDir()
	if err != nil {
		return Agent{}, err
	}
	socketPath, err := defaultSocketPath()
	if err != nil {
		return Agent{}, err
	}
	cachePath, err := configuredAbsolutePath(EnvAgentCachePath, runtimeSettings.Agent.CachePath, filepath.Join(cacheDir, "lernae", "cache"))
	if err != nil {
		return Agent{}, err
	}
	stagingFallback := filepath.Join(cachePath, ".staging")
	stagingPath, err := configuredAbsolutePath(EnvAgentStagingPath, runtimeSettings.Agent.StagingPath, stagingFallback)
	if err != nil {
		return Agent{}, err
	}
	socketPath, err = configuredAbsolutePath(EnvAgentSocketPath, runtimeSettings.Agent.SocketPath, socketPath)
	if err != nil {
		return Agent{}, err
	}

	rel, err := filepath.Rel(cachePath, stagingPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return Agent{}, fmt.Errorf("%s must be a child directory of %s", EnvAgentStagingPath, EnvAgentCachePath)
	}
	if components := strings.Split(filepath.ToSlash(rel), "/"); len(components) > 0 && strings.EqualFold(components[0], "assets") {
		return Agent{}, ErrAgentStagingPathConflict
	}
	return Agent{SocketPath: socketPath, CachePath: cachePath, StagingPath: stagingPath}, nil
}

func defaultSocketPath() (string, error) {
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		path, err := requireAbsolute("XDG_RUNTIME_DIR", runtimeDir)
		if err != nil {
			return "", err
		}
		return filepath.Join(path, "lernae", "agent.sock"), nil
	}
	cacheDir, err := xdgCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, "lernae", "run", "agent.sock"), nil
}

func xdgDataDir() (string, error) {
	if path := os.Getenv("XDG_DATA_HOME"); path != "" {
		return requireAbsolute("XDG_DATA_HOME", path)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for XDG data path: %w", err)
	}
	return filepath.Join(home, ".local", "share"), nil
}

func xdgCacheDir() (string, error) {
	if path := os.Getenv("XDG_CACHE_HOME"); path != "" {
		return requireAbsolute("XDG_CACHE_HOME", path)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for XDG cache path: %w", err)
	}
	return filepath.Join(home, ".cache"), nil
}

func absolutePath(name, fallback string) (string, error) {
	value := envOr(name, fallback)
	return requireAbsolute(name, value)
}

func configuredOptionalAbsolutePath(environmentName, persisted string) (string, error) {
	value := resolveRuntimeValue(environmentName, persisted, "")
	if value == "" {
		return "", nil
	}
	return requireAbsolute(environmentName, value)
}

func requireAbsolute(name, value string) (string, error) {
	if value == "" || !filepath.IsAbs(value) {
		return "", fmt.Errorf("%s must be an absolute path", name)
	}
	clean := filepath.Clean(value)
	for _, segment := range strings.Split(filepath.ToSlash(value), "/") {
		if segment == ".." {
			return "", fmt.Errorf("%s must not contain path traversal", name)
		}
	}
	return clean, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
