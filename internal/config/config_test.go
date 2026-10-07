package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServerLocalConfigurationReloadAndDiagnosticPrivacy(t *testing.T) {
	roots := clearIGDBEnv(t)
	store, err := NewFileRuntimeSettingsStore(roots.config, roots.home)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := LoadServer()
	if err != nil || baseline.LocalAcquisition.Enabled {
		t.Fatal("local provider not disabled by default")
	}
	settings := LocalAcquisitionSettings{Enabled: true, SourceRoot: "/PRIVATE_SOURCE", StagingRoot: "/PRIVATE_STAGING"}
	if err := store.Update(func(s *RuntimeSettings) { s.LocalAcquisition = settings }); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadServer()
	if err != nil || loaded.LocalAcquisition != settings {
		t.Fatal("persistent local config not reloaded")
	}
	for _, value := range []any{loaded, settings, RuntimeSettings{LocalAcquisition: settings}} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{fmt.Sprintf("%v", value), fmt.Sprintf("%+v", value), fmt.Sprintf("%#v", value), string(encoded)} {
			if strings.Contains(text, settings.SourceRoot) || strings.Contains(text, settings.StagingRoot) {
				t.Fatal("configuration diagnostics leaked private roots")
			}
		}
	}
	for _, settings := range []LocalAcquisitionSettings{
		{},
		{SourceRoot: "/private/assets/source"},
		{StagingRoot: "/private/Assets"},
		{SourceRoot: "/private/assets", StagingRoot: "/private/staging"},
		{Enabled: true, SourceRoot: "/private/assets", StagingRoot: "/private/staging"},
	} {
		if err := ValidateLocalAcquisitionSettings(settings); err != nil {
			t.Fatal("safe disabled/partial/source settings rejected", err)
		}
	}
	for _, settings := range []LocalAcquisitionSettings{
		{Enabled: true},
		{SourceRoot: "relative"},
		{StagingRoot: "/private/assets"},
		{StagingRoot: "/private/assets/staging"},
		{SourceRoot: "/private/source", StagingRoot: "/private/cache/assets"},
		{Enabled: true, SourceRoot: "/private/source", StagingRoot: "/private/assets/staging"},
		{SourceRoot: "/"},
		{SourceRoot: "/private/../source"},
		{SourceRoot: "/private/source", StagingRoot: "/private/source/staging"},
		{SourceRoot: "/private/source", StagingRoot: "/private"},
	} {
		if err := ValidateLocalAcquisitionSettings(settings); err == nil {
			t.Fatal("unsafe local config accepted")
		}
	}
}

func TestLoadDefaultsUseXDGDirectories(t *testing.T) {
	clearIGDBEnv(t)
	t.Setenv("HOME", "/tmp/lernae-home")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "xdg-config"))
	t.Setenv("XDG_DATA_HOME", "/tmp/lernae-data")
	t.Setenv("XDG_CACHE_HOME", "/tmp/lernae-cache")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/lernae-runtime")
	for _, name := range []string{EnvServerDatabasePath, EnvServerListenAddr, EnvAgentSocketPath, EnvAgentCachePath, EnvAgentStagingPath, EnvInventoryManifestPath, EnvInventorySource} {
		t.Setenv(name, "")
	}

	server, err := LoadServer()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := LoadAgent()
	if err != nil {
		t.Fatal(err)
	}

	if want := "/tmp/lernae-data/lernae/lernae.db"; server.DatabasePath != want {
		t.Fatalf("database path = %q, want %q", server.DatabasePath, want)
	}
	if want := "/tmp/lernae-runtime/lernae/agent.sock"; server.AgentSocketPath != want || agent.SocketPath != want {
		t.Fatalf("socket paths = %q and %q, want %q", server.AgentSocketPath, agent.SocketPath, want)
	}
	if server.ListenAddress != "127.0.0.1:8081" {
		t.Fatalf("listen address = %q", server.ListenAddress)
	}
	if server.MetadataLanguage != MetadataLanguageEnglish {
		t.Fatalf("metadata language = %q, want English default", server.MetadataLanguage)
	}
	if agent.CachePath != "/tmp/lernae-cache/lernae/cache" || agent.StagingPath != filepath.Join(agent.CachePath, ".staging") {
		t.Fatalf("unexpected cache/staging defaults: %#v", agent)
	}
}

func TestLoadServerListenAddressRequiresLiteralLoopback(t *testing.T) {
	tests := []struct {
		name        string
		listenAddr  string
		wantAddress string
		wantError   bool
	}{
		{name: "default remains loopback", wantAddress: "127.0.0.1:8081"},
		{name: "IPv4 loopback", listenAddr: "127.0.0.1:8082", wantAddress: "127.0.0.1:8082"},
		{name: "IPv6 loopback", listenAddr: "[::1]:8083", wantAddress: "[::1]:8083"},
		{name: "empty host", listenAddr: ":8081", wantError: true},
		{name: "IPv4 wildcard", listenAddr: "0.0.0.0:8081", wantError: true},
		{name: "IPv6 wildcard", listenAddr: "[::]:8081", wantError: true},
		{name: "DNS host", listenAddr: "localhost:8081", wantError: true},
		{name: "non-loopback IP", listenAddr: "192.0.2.10:8081", wantError: true},
		{name: "other 127 slash 8 address", listenAddr: "127.0.0.2:8081", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearIGDBEnv(t)
			t.Setenv("HOME", "/tmp/lernae-home")
			t.Setenv("XDG_RUNTIME_DIR", "/tmp/lernae-runtime")
			t.Setenv(EnvServerListenAddr, test.listenAddr)

			settings, err := LoadServer()
			if test.wantError {
				if err == nil {
					t.Fatalf("LoadServer() accepted unsafe listen address %q", test.listenAddr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadServer() error = %v", err)
			}
			if settings.ListenAddress != test.wantAddress {
				t.Fatalf("listen address = %q, want %q", settings.ListenAddress, test.wantAddress)
			}
		})
	}
}

func TestLoadAgentRejectsUnsafeOrUnrelatedPaths(t *testing.T) {
	clearIGDBEnv(t)
	t.Setenv("HOME", "/tmp/lernae-home")
	t.Setenv("XDG_CACHE_HOME", "/tmp/lernae-cache")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/lernae-runtime")
	t.Setenv(EnvAgentCachePath, "/tmp/cache")
	t.Setenv(EnvAgentStagingPath, "/tmp/outside")
	if _, err := LoadAgent(); err == nil {
		t.Fatal("expected staging outside cache to be rejected")
	}

	t.Setenv(EnvAgentStagingPath, "/tmp/cache/../outside")
	if _, err := LoadAgent(); err == nil {
		t.Fatal("expected path traversal to be rejected")
	}
}

func TestLoadServerRejectsRelativeDatabasePath(t *testing.T) {
	clearIGDBEnv(t)
	t.Setenv("HOME", "/tmp/lernae-home")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/lernae-runtime")
	t.Setenv(EnvServerDatabasePath, "relative.db")
	if _, err := LoadServer(); err == nil {
		t.Fatal("expected relative database path to be rejected")
	}
}

func TestLoadServerAllowsOptionalExplicitInventoryManifestPath(t *testing.T) {
	clearIGDBEnv(t)
	t.Setenv("HOME", "/tmp/lernae-home")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/lernae-runtime")
	t.Setenv(EnvInventoryManifestPath, "")

	settings, err := LoadServer()
	if err != nil {
		t.Fatalf("missing optional inventory manifest prevented startup: %v", err)
	}
	if settings.InventoryManifestPath != "" {
		t.Fatalf("unset inventory manifest path = %q, want empty", settings.InventoryManifestPath)
	}

	t.Setenv(EnvInventoryManifestPath, "  /tmp/synthetic-inventory.json  ")
	settings, err = LoadServer()
	if err != nil {
		t.Fatalf("explicit absolute inventory manifest path rejected: %v", err)
	}
	if settings.InventoryManifestPath != "/tmp/synthetic-inventory.json" {
		t.Fatalf("inventory manifest path = %q, want trimmed absolute path", settings.InventoryManifestPath)
	}

	t.Setenv(EnvInventoryManifestPath, "relative/manifest.json")
	if _, err := LoadServer(); err == nil {
		t.Fatal("relative inventory manifest path should be rejected")
	}
}

func TestLoadServerInventorySourceSelectionIsExplicitAndOptional(t *testing.T) {
	tests := []struct {
		name       string
		selector   string
		rommURL    string
		rommToken  string
		wantSource string
		wantError  bool
	}{
		{name: "default manifest without RomM configuration", wantSource: InventorySourceManifest},
		{name: "manifest ignores optional RomM configuration", selector: InventorySourceManifest, rommURL: "http://127.0.0.1:32400", rommToken: "synthetic-token", wantSource: InventorySourceManifest},
		{name: "RomM requires a base URL", selector: InventorySourceRomM, rommToken: "synthetic-token", wantError: true},
		{name: "RomM requires a client API token", selector: InventorySourceRomM, rommURL: "http://127.0.0.1:32400", wantError: true},
		{name: "RomM source is available only when selected", selector: InventorySourceRomM, rommURL: "http://127.0.0.1:32400", rommToken: "synthetic-token", wantSource: InventorySourceRomM},
		{name: "unknown selector fails safely", selector: "unknown", wantError: true},
		{name: "selector matching is case-sensitive", selector: "RomM", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearIGDBEnv(t)
			t.Setenv("HOME", "/tmp/lernae-home")
			t.Setenv("XDG_RUNTIME_DIR", "/tmp/lernae-runtime")
			t.Setenv(EnvInventorySource, test.selector)
			if test.rommURL != "" {
				t.Setenv(EnvRomMBaseURL, test.rommURL)
			}
			if test.rommToken != "" {
				t.Setenv(EnvRomMClientAPIToken, test.rommToken)
			}

			settings, err := LoadServer()
			if test.wantError {
				if err == nil {
					t.Fatal("LoadServer() succeeded; want selected-source configuration error")
				}
				if (test.rommToken != "" && strings.Contains(err.Error(), test.rommToken)) ||
					(test.rommURL != "" && strings.Contains(err.Error(), test.rommURL)) {
					t.Fatal("LoadServer() error exposed RomM configuration")
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadServer() error = %v", err)
			}
			if settings.InventorySource != test.wantSource {
				t.Fatalf("inventory source = %q, want %q", settings.InventorySource, test.wantSource)
			}
			if (settings.RomM.HasCredentials()) != (test.rommURL != "" && test.rommToken != "") {
				t.Fatal("RomM credentials were not preserved as an optional configuration pair")
			}
		})
	}
}

func TestLoadUsesCacheFallbackAndExplicitEnvironmentPaths(t *testing.T) {
	clearIGDBEnv(t)
	t.Setenv("HOME", "/tmp/lernae-home")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "/tmp/lernae-cache")
	t.Setenv("XDG_RUNTIME_DIR", "")
	for _, name := range []string{EnvServerDatabasePath, EnvServerListenAddr, EnvAgentSocketPath, EnvAgentCachePath, EnvAgentStagingPath} {
		t.Setenv(name, "")
	}
	server, err := LoadServer()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := LoadAgent()
	if err != nil {
		t.Fatal(err)
	}
	if server.DatabasePath != "/tmp/lernae-home/.local/share/lernae/lernae.db" {
		t.Fatalf("database XDG fallback = %q", server.DatabasePath)
	}
	if server.AgentSocketPath != "/tmp/lernae-cache/lernae/run/agent.sock" {
		t.Fatalf("socket XDG fallback = %q", server.AgentSocketPath)
	}
	if agent.CachePath != "/tmp/lernae-cache/lernae/cache" {
		t.Fatalf("cache XDG fallback = %q", agent.CachePath)
	}

	t.Setenv(EnvServerDatabasePath, "/tmp/custom/db.sqlite")
	t.Setenv(EnvServerListenAddr, "127.0.0.1:9999")
	t.Setenv(EnvAgentSocketPath, "/tmp/custom/run/agent.sock")
	t.Setenv(EnvAgentCachePath, "/tmp/custom/cache")
	t.Setenv(EnvAgentStagingPath, "/tmp/custom/cache/staging")
	server, err = LoadServer()
	if err != nil {
		t.Fatal(err)
	}
	agent, err = LoadAgent()
	if err != nil {
		t.Fatal(err)
	}
	if server.DatabasePath != "/tmp/custom/db.sqlite" || server.ListenAddress != "127.0.0.1:9999" || server.AgentSocketPath != agent.SocketPath {
		t.Fatalf("server environment configuration = %#v", server)
	}
	if agent.CachePath != "/tmp/custom/cache" || agent.StagingPath != "/tmp/custom/cache/staging" {
		t.Fatalf("agent environment configuration = %#v", agent)
	}
}

func TestLoadServerAndAgentResolvePersistedSettingsBelowEnvironment(t *testing.T) {
	roots := clearIGDBEnv(t)
	store, err := NewFileRuntimeSettingsStore(roots.config, roots.home)
	if err != nil {
		t.Fatal(err)
	}
	settings := RuntimeSettings{
		Server:   ServerSettings{DatabasePath: "/tmp/persisted/server.db", ListenAddress: "127.0.0.1:8181"},
		Agent:    AgentSettings{SocketPath: "/tmp/persisted/agent.sock", CachePath: "/tmp/persisted/cache"},
		Metadata: MetadataSettings{Language: MetadataLanguageSpanish},
	}
	if err := store.Save(settings); err != nil {
		t.Fatalf("save runtime settings: %v", err)
	}

	server, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer persisted settings: %v", err)
	}
	agent, err := LoadAgent()
	if err != nil {
		t.Fatalf("LoadAgent persisted settings: %v", err)
	}
	if server.DatabasePath != settings.Server.DatabasePath || server.ListenAddress != settings.Server.ListenAddress || server.AgentSocketPath != settings.Agent.SocketPath || server.MetadataLanguage != MetadataLanguageSpanish {
		t.Fatalf("server settings = %#v, want persisted values", server)
	}
	if agent.SocketPath != settings.Agent.SocketPath || agent.CachePath != settings.Agent.CachePath || agent.StagingPath != filepath.Join(settings.Agent.CachePath, ".staging") {
		t.Fatalf("agent settings = %#v, want persisted paths and derived staging", agent)
	}

	t.Setenv(EnvServerDatabasePath, "/tmp/temporary/server.db")
	t.Setenv(EnvServerListenAddr, "[::1]:8182")
	t.Setenv(EnvAgentSocketPath, "/tmp/temporary/agent.sock")
	t.Setenv(EnvAgentCachePath, "/tmp/temporary/cache")
	server, err = LoadServer()
	if err != nil {
		t.Fatalf("LoadServer environment overrides: %v", err)
	}
	agent, err = LoadAgent()
	if err != nil {
		t.Fatalf("LoadAgent environment overrides: %v", err)
	}
	if server.DatabasePath != "/tmp/temporary/server.db" || server.ListenAddress != "[::1]:8182" || server.AgentSocketPath != "/tmp/temporary/agent.sock" || server.MetadataLanguage != MetadataLanguageSpanish {
		t.Fatalf("environment did not override server settings: %#v", server)
	}
	if agent.SocketPath != "/tmp/temporary/agent.sock" || agent.CachePath != "/tmp/temporary/cache" || agent.StagingPath != filepath.Join("/tmp/temporary/cache", ".staging") {
		t.Fatalf("environment did not override agent settings and re-derive staging: %#v", agent)
	}
	after, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if after.Server != settings.Server || after.Agent != settings.Agent {
		t.Fatalf("temporary overrides mutated saved settings: %#v", after)
	}
}

func TestValidateRuntimeSettingsRejectsUnsupportedMetadataLanguage(t *testing.T) {
	if err := ValidateRuntimeSettings(RuntimeSettings{Metadata: MetadataSettings{Language: "fr"}}); !errors.Is(err, ErrInvalidRuntimeSettings) {
		t.Fatalf("ValidateRuntimeSettings() error = %v, want ErrInvalidRuntimeSettings", err)
	}
}

func TestLoadServerRejectsUnsupportedPersistedMetadataLanguage(t *testing.T) {
	roots := clearIGDBEnv(t)
	store, err := NewFileRuntimeSettingsStore(roots.config, roots.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(RuntimeSettings{Metadata: MetadataSettings{Language: "fr"}}); err != nil {
		t.Fatalf("save invalid metadata preference fixture: %v", err)
	}
	if _, err := LoadServer(); !errors.Is(err, ErrInvalidRuntimeSettings) {
		t.Fatalf("LoadServer() error = %v, want ErrInvalidRuntimeSettings", err)
	}
}

func TestLoadServerResolvesPersistedInventoryBelowEnvironment(t *testing.T) {
	roots := clearIGDBEnv(t)
	store, err := NewFileRuntimeSettingsStore(roots.config, roots.home)
	if err != nil {
		t.Fatal(err)
	}
	persisted := RuntimeSettings{Inventory: InventorySettings{
		Source:       InventorySourceRomM,
		ManifestPath: "/tmp/persisted/manifest.json",
	}}
	if err := store.Save(persisted); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvRomMBaseURL, "https://romm.example.invalid/synthetic")
	t.Setenv(EnvRomMClientAPIToken, "synthetic-romm-token")

	server, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer persisted inventory: %v", err)
	}
	if server.InventorySource != InventorySourceRomM || server.InventoryManifestPath != persisted.Inventory.ManifestPath {
		t.Fatalf("persisted inventory = %q/%q, want %q/%q", server.InventorySource, server.InventoryManifestPath, InventorySourceRomM, persisted.Inventory.ManifestPath)
	}

	t.Setenv(EnvInventorySource, InventorySourceManifest)
	t.Setenv(EnvInventoryManifestPath, "/tmp/temporary/manifest.json")
	server, err = LoadServer()
	if err != nil {
		t.Fatalf("LoadServer inventory environment overrides: %v", err)
	}
	if server.InventorySource != InventorySourceManifest || server.InventoryManifestPath != "/tmp/temporary/manifest.json" {
		t.Fatalf("environment inventory = %q/%q, want Manifest and temporary path", server.InventorySource, server.InventoryManifestPath)
	}
	after, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if after.Inventory != persisted.Inventory {
		t.Fatalf("environment overrides mutated saved inventory: %#v", after.Inventory)
	}
}

func TestLoadServerValidatesPersistedInventorySettings(t *testing.T) {
	for _, test := range []struct {
		name          string
		inventory     InventorySettings
		wantSourceErr bool
		wantPathErr   bool
	}{
		{name: "unknown source", inventory: InventorySettings{Source: "federated"}, wantSourceErr: true},
		{name: "relative manifest path", inventory: InventorySettings{ManifestPath: "manifest.json"}, wantPathErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			roots := clearIGDBEnv(t)
			store, err := NewFileRuntimeSettingsStore(roots.config, roots.home)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(RuntimeSettings{Inventory: test.inventory}); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadServer(); err == nil {
				t.Fatal("LoadServer() accepted invalid persisted inventory settings")
			} else if test.wantSourceErr && !errors.Is(err, ErrInvalidInventorySource) {
				t.Fatalf("LoadServer() error = %v, want ErrInvalidInventorySource", err)
			} else if test.wantPathErr && !strings.Contains(err.Error(), EnvInventoryManifestPath+" must be an absolute path") {
				t.Fatalf("LoadServer() error = %v, want absolute Manifest path validation", err)
			}
		})
	}
}

func TestLoadServerKeepsMissingPersistedManifestPathAsSafeConfiguration(t *testing.T) {
	roots := clearIGDBEnv(t)
	manifestPath := filepath.Join(t.TempDir(), "missing-manifest.json")
	store, err := NewFileRuntimeSettingsStore(roots.config, roots.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(RuntimeSettings{Inventory: InventorySettings{
		Source:       InventorySourceManifest,
		ManifestPath: manifestPath,
	}}); err != nil {
		t.Fatal(err)
	}

	server, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer rejected an absent but configured Manifest path: %v", err)
	}
	if server.InventorySource != InventorySourceManifest || server.InventoryManifestPath != manifestPath {
		t.Fatalf("Server inventory = %q/%q, want Manifest with the configured missing path retained", server.InventorySource, server.InventoryManifestPath)
	}
}

func TestLoadAgentPersistedStagingCanBeExplicitAndMustStayWithinCache(t *testing.T) {
	roots := clearIGDBEnv(t)
	store, err := NewFileRuntimeSettingsStore(roots.config, roots.home)
	if err != nil {
		t.Fatal(err)
	}
	settings := RuntimeSettings{Agent: AgentSettings{
		CachePath:   "/tmp/persisted/cache",
		StagingPath: "/tmp/persisted/cache/custom-staging",
	}}
	if err := store.Save(settings); err != nil {
		t.Fatal(err)
	}
	agent, err := LoadAgent()
	if err != nil {
		t.Fatalf("LoadAgent explicit persisted staging: %v", err)
	}
	if agent.StagingPath != settings.Agent.StagingPath {
		t.Fatalf("staging path = %q, want explicit persisted path %q", agent.StagingPath, settings.Agent.StagingPath)
	}

	settings.Agent.StagingPath = "/tmp/outside/staging"
	if err := store.Save(settings); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAgent(); err == nil {
		t.Fatal("LoadAgent accepted persisted staging outside the cache")
	}
}

func TestLoadAgentRejectsStagingInsideFinalAssetNamespace(t *testing.T) {
	for _, source := range []string{"persisted setting", "environment override"} {
		t.Run(source, func(t *testing.T) {
			roots := clearIGDBEnv(t)
			cachePath := "/tmp/synthetic-agent-cache"
			stagingPath := filepath.Join(cachePath, "assets", "restore-staging")
			if source == "persisted setting" {
				store, err := NewFileRuntimeSettingsStore(roots.config, roots.home)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Save(RuntimeSettings{Agent: AgentSettings{CachePath: cachePath, StagingPath: stagingPath}}); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Setenv(EnvAgentCachePath, cachePath)
				t.Setenv(EnvAgentStagingPath, stagingPath)
			}
			if _, err := LoadAgent(); !errors.Is(err, ErrAgentStagingPathConflict) {
				t.Fatalf("LoadAgent error = %v, want ErrAgentStagingPathConflict", err)
			}
		})
	}
}

func TestLoadServerValidatesPersistedListenAddressAndSettingsFile(t *testing.T) {
	roots := clearIGDBEnv(t)
	store, err := NewFileRuntimeSettingsStore(roots.config, roots.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(RuntimeSettings{Server: ServerSettings{ListenAddress: "0.0.0.0:8081"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadServer(); !errors.Is(err, ErrServerListenAddressNotLoopback) {
		t.Fatalf("persisted wildcard listener error = %v, want loopback validation error", err)
	}

	path, err := SettingsConfigPath(roots.config, roots.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":99,"settings":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadServer(); !errors.Is(err, ErrRuntimeSettingsVersion) {
		t.Fatalf("unsupported settings version error = %v, want ErrRuntimeSettingsVersion", err)
	}
}

func TestLoadServerReadsIGDBCredentialsWithoutRequiringThem(t *testing.T) {
	clearIGDBEnv(t)
	t.Setenv(EnvIGDBClientID, "synthetic-client-id")
	t.Setenv(EnvIGDBClientSecret, "synthetic-client-secret")
	t.Setenv("HOME", "/tmp/lernae-home")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/lernae-runtime")

	configured, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer rejected configured IGDB credentials: %v", err)
	}
	if got := configured.IGDB.HasCredentials(); !got {
		t.Fatal("IGDB credentials were not loaded")
	}
	if configured.IGDB.ClientID != "synthetic-client-id" || configured.IGDB.ClientSecret != "synthetic-client-secret" {
		t.Fatal("IGDB credentials were not preserved exactly")
	}

	unsetProviderEnv(t, EnvIGDBClientID, EnvIGDBClientSecret)
	missing, err := LoadServer()
	if err != nil {
		t.Fatalf("missing IGDB credentials prevented unrelated server startup: %v", err)
	}
	if missing.IGDB.HasCredentials() {
		t.Fatal("missing IGDB credentials were reported configured")
	}
	if err := missing.IGDB.Validate(); err == nil {
		t.Fatal("missing credentials passed IGDB validation")
	}
	t.Setenv(EnvIGDBClientID, "synthetic-client-id")
	if _, err := LoadServer(); !errors.Is(err, ErrIGDBCredentialsIncomplete) {
		t.Fatalf("partial environment credentials error = %v, want ErrIGDBCredentialsIncomplete", err)
	}
}

func TestLoadServerKeepsWikidataContactEmailOptionalAndTrimsConfiguredValue(t *testing.T) {
	roots := clearIGDBEnv(t)
	store, err := NewFileRuntimeSettingsStore(roots.config, roots.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(RuntimeSettings{Wikidata: WikidataSettings{ContactEmail: "persisted-contact-marker"}}); err != nil {
		t.Fatalf("save Wikidata runtime setting: %v", err)
	}

	t.Run("persisted setting is used when environment is absent", func(t *testing.T) {
		unsetProviderEnv(t, EnvWikidataContactEmail)
		if err := store.Update(func(settings *RuntimeSettings) {
			settings.Wikidata.ContactEmail = "operator@example.com"
		}); err != nil {
			t.Fatalf("save valid persisted contact: %v", err)
		}
		server, err := LoadServer()
		if err != nil {
			t.Fatalf("LoadServer() error = %v", err)
		}
		if server.WikidataContactEmail != "operator@example.com" {
			t.Fatalf("Wikidata contact = %q, want persisted contact", server.WikidataContactEmail)
		}
	})

	t.Run("nonblank environment overrides persisted value", func(t *testing.T) {
		if err := store.Update(func(settings *RuntimeSettings) {
			settings.Wikidata.ContactEmail = "persisted-contact-marker"
		}); err != nil {
			t.Fatalf("save distinct persisted value: %v", err)
		}
		t.Setenv(EnvWikidataContactEmail, "  operator@example.com  ")
		server, err := LoadServer()
		if err != nil {
			t.Fatalf("LoadServer() error = %v", err)
		}
		if server.WikidataContactEmail != "operator@example.com" {
			t.Fatalf("Wikidata contact = %q, want trimmed environment override", server.WikidataContactEmail)
		}
	})

	t.Run("blank environment falls back to persisted value", func(t *testing.T) {
		if err := store.Update(func(settings *RuntimeSettings) {
			settings.Wikidata.ContactEmail = "operator@example.com"
		}); err != nil {
			t.Fatalf("restore valid persisted contact: %v", err)
		}
		t.Setenv(EnvWikidataContactEmail, "   ")
		server, err := LoadServer()
		if err != nil {
			t.Fatalf("LoadServer() error = %v", err)
		}
		if server.WikidataContactEmail != "operator@example.com" {
			t.Fatalf("Wikidata contact = %q, want persisted fallback", server.WikidataContactEmail)
		}
	})

	t.Run("invalid nonblank environment remains authoritative", func(t *testing.T) {
		if err := store.Update(func(settings *RuntimeSettings) {
			settings.Wikidata.ContactEmail = "operator@example.com"
		}); err != nil {
			t.Fatalf("restore valid persisted contact: %v", err)
		}
		t.Setenv(EnvWikidataContactEmail, "not-an-email")
		server, err := LoadServer()
		if err != nil {
			t.Fatalf("invalid optional contact prevented server startup: %v", err)
		}
		if server.WikidataContactEmail != "not-an-email" {
			t.Fatalf("Wikidata contact = %q, want authoritative invalid override", server.WikidataContactEmail)
		}
	})

	t.Run("unset config remains optional", func(t *testing.T) {
		unsetProviderEnv(t, EnvWikidataContactEmail)
		if err := store.Save(RuntimeSettings{}); err != nil {
			t.Fatal(err)
		}
		server, err := LoadServer()
		if err != nil || server.WikidataContactEmail != "" {
			t.Fatalf("unset optional Wikidata contact = %q, error %v", server.WikidataContactEmail, err)
		}
	})
}

func TestLoadServerReadsPersistedProviderBundlesOnFreshLoad(t *testing.T) {
	roots := clearIGDBEnv(t)
	store, err := NewFileProviderConfigStore(roots.config, roots.home)
	if err != nil {
		t.Fatal(err)
	}
	const (
		igdbID     = "synthetic-stored-igdb-id"
		igdbSecret = "synthetic-stored-igdb-secret"
		rommURL    = "https://romm.example.invalid/stored"
		rommToken  = "synthetic-stored-romm-token"
	)
	if err := store.SaveIGDB(IGDBCredentials{ClientID: igdbID, ClientSecret: igdbSecret}); err != nil {
		t.Fatalf("save IGDB fixture: %v", err)
	}
	if err := store.SaveRomM(RomMCredentials{BaseURL: rommURL, ClientAPIToken: rommToken}); err != nil {
		t.Fatalf("save RomM fixture: %v", err)
	}
	t.Setenv(EnvInventorySource, InventorySourceRomM)

	server, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer() with persisted bundles: %v", err)
	}
	if server.IGDB != (IGDBCredentials{ClientID: igdbID, ClientSecret: igdbSecret}) {
		t.Fatal("fresh Server load did not resolve the persisted IGDB bundle")
	}
	if server.RomM != (RomMCredentials{BaseURL: rommURL, ClientAPIToken: rommToken}) {
		t.Fatal("fresh Server load did not resolve the persisted RomM bundle")
	}
}

func TestLoadServerEnvironmentBundlesOverridePersistedBundles(t *testing.T) {
	roots := clearIGDBEnv(t)
	store, err := NewFileProviderConfigStore(roots.config, roots.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveIGDB(IGDBCredentials{ClientID: "synthetic-disk-id", ClientSecret: "synthetic-disk-secret"}); err != nil {
		t.Fatalf("save IGDB fixture: %v", err)
	}
	if err := store.SaveRomM(RomMCredentials{BaseURL: "https://romm.example.invalid/disk", ClientAPIToken: "synthetic-disk-token"}); err != nil {
		t.Fatalf("save RomM fixture: %v", err)
	}
	t.Setenv(EnvIGDBClientID, "synthetic-env-id")
	t.Setenv(EnvIGDBClientSecret, "synthetic-env-secret")
	t.Setenv(EnvRomMBaseURL, "https://romm.example.invalid/environment")
	t.Setenv(EnvRomMClientAPIToken, "synthetic-env-romm-token")
	t.Setenv(EnvInventorySource, InventorySourceRomM)

	server, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer() with complete environment bundles: %v", err)
	}
	if server.IGDB != (IGDBCredentials{ClientID: "synthetic-env-id", ClientSecret: "synthetic-env-secret"}) {
		t.Fatal("complete IGDB environment bundle did not override persisted credentials")
	}
	if server.RomM != (RomMCredentials{BaseURL: "https://romm.example.invalid/environment", ClientAPIToken: "synthetic-env-romm-token"}) {
		t.Fatal("complete RomM environment bundle did not override persisted credentials")
	}
}

func TestLoadServerRejectsPartialEnvironmentBundlesInsteadOfMixingSources(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		set      map[string]string
		want     error
	}{
		{name: "IGDB client id only", set: map[string]string{EnvIGDBClientID: "synthetic-partial-id"}, want: ErrIGDBCredentialsIncomplete},
		{name: "IGDB secret only", set: map[string]string{EnvIGDBClientSecret: "synthetic-partial-secret"}, want: ErrIGDBCredentialsIncomplete},
		{name: "IGDB explicit empty pair", set: map[string]string{EnvIGDBClientID: "", EnvIGDBClientSecret: ""}, want: ErrIGDBCredentialsIncomplete},
		{name: "selected RomM base URL only", selector: InventorySourceRomM, set: map[string]string{EnvRomMBaseURL: "https://romm.example.invalid/partial"}, want: ErrRomMCredentialsIncomplete},
		{name: "selected RomM token only", selector: InventorySourceRomM, set: map[string]string{EnvRomMClientAPIToken: "synthetic-partial-romm-token"}, want: ErrRomMCredentialsIncomplete},
		{name: "selected RomM explicit empty pair", selector: InventorySourceRomM, set: map[string]string{EnvRomMBaseURL: "", EnvRomMClientAPIToken: ""}, want: ErrRomMCredentialsIncomplete},
		{name: "unselected RomM partial environment is still invalid", selector: InventorySourceManifest, set: map[string]string{EnvRomMBaseURL: "https://romm.example.invalid/partial"}, want: ErrRomMCredentialsIncomplete},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			roots := clearIGDBEnv(t)
			store, err := NewFileProviderConfigStore(roots.config, roots.home)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SaveIGDB(IGDBCredentials{ClientID: "synthetic-stored-id", ClientSecret: "synthetic-stored-secret"}); err != nil {
				t.Fatalf("save IGDB fixture: %v", err)
			}
			if err := store.SaveRomM(RomMCredentials{BaseURL: "https://romm.example.invalid/stored", ClientAPIToken: "synthetic-stored-token"}); err != nil {
				t.Fatalf("save RomM fixture: %v", err)
			}
			t.Setenv(EnvInventorySource, test.selector)
			for name, value := range test.set {
				t.Setenv(name, value)
			}

			_, err = LoadServer()
			if !errors.Is(err, test.want) {
				t.Fatalf("LoadServer() error = %v, want %v", err, test.want)
			}
			if err != nil {
				for _, marker := range test.set {
					if marker != "" && strings.Contains(err.Error(), marker) {
						t.Fatal("LoadServer() error exposed an environment credential")
					}
				}
			}
		})
	}
}

func TestLoadServerMissingProviderConfigKeepsManifestStartupOptional(t *testing.T) {
	clearIGDBEnv(t)

	server, err := LoadServer()
	if err != nil {
		t.Fatalf("missing optional provider config prevented Manifest startup: %v", err)
	}
	if server.InventorySource != InventorySourceManifest || server.IGDB.HasCredentials() || server.RomM.HasCredentials() {
		t.Fatal("missing provider config did not leave optional providers unconfigured under Manifest")
	}
}

func TestPersistedRomMCredentialsDoNotChangeManifestSelection(t *testing.T) {
	roots := clearIGDBEnv(t)
	store, err := NewFileProviderConfigStore(roots.config, roots.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveIGDB(IGDBCredentials{ClientID: "synthetic-stored-id", ClientSecret: "synthetic-stored-secret"}); err != nil {
		t.Fatalf("save IGDB fixture: %v", err)
	}
	if err := store.SaveRomM(RomMCredentials{BaseURL: "https://romm.example.invalid/stored", ClientAPIToken: "synthetic-stored-token"}); err != nil {
		t.Fatalf("save RomM fixture: %v", err)
	}
	t.Setenv(EnvInventorySource, InventorySourceManifest)

	server, err := LoadServer()
	if err != nil {
		t.Fatalf("Manifest LoadServer() with optional persisted RomM: %v", err)
	}
	if server.InventorySource != InventorySourceManifest {
		t.Fatalf("persisted RomM credentials changed the selected source to %q", server.InventorySource)
	}
}

func TestLoadServerMapsUnsafeOrMalformedProviderFilesToFixedActionableError(t *testing.T) {
	const persistedMarker = "synthetic-corrupt-provider-secret"
	var expected string
	for _, unsafeDirectory := range []bool{false, true} {
		name := "malformed file"
		if unsafeDirectory {
			name = "unsafe directory"
		}
		t.Run(name, func(t *testing.T) {
			roots := clearIGDBEnv(t)
			store, err := NewFileProviderConfigStore(roots.config, roots.home)
			if err != nil {
				t.Fatal(err)
			}
			directory := filepath.Dir(store.path)
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(store.path, []byte(`{"version":1,"secret":"`+persistedMarker), 0o600); err != nil {
				t.Fatal(err)
			}
			if unsafeDirectory {
				if err := os.Chmod(directory, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			_, err = LoadServer()
			if err == nil {
				t.Fatal("LoadServer() accepted an unsafe or malformed persisted provider file")
			}
			if strings.Contains(err.Error(), persistedMarker) {
				t.Fatal("LoadServer() error exposed persisted secret data")
			}
			if !strings.Contains(err.Error(), "provider") || !strings.Contains(err.Error(), "repair") {
				t.Fatalf("provider config error is not fixed/actionable: %v", err)
			}
			if expected == "" {
				expected = err.Error()
			} else if err.Error() != expected {
				t.Fatalf("provider config error = %q, want fixed error %q", err.Error(), expected)
			}
		})
	}
}

func TestLoadServerSkipsPersistedFileWhenSelectedBundlesUseCompleteEnvironment(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		rommEnv  bool
	}{
		{name: "Manifest uses IGDB environment", selector: InventorySourceManifest},
		{name: "RomM uses both environment bundles", selector: InventorySourceRomM, rommEnv: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			roots := clearIGDBEnv(t)
			store, err := NewFileProviderConfigStore(roots.config, roots.home)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(store.path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(store.path, []byte(`{"broken":"synthetic-skipped-file-marker`), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(EnvInventorySource, test.selector)
			t.Setenv(EnvIGDBClientID, "synthetic-env-id")
			t.Setenv(EnvIGDBClientSecret, "synthetic-env-secret")
			if test.rommEnv {
				t.Setenv(EnvRomMBaseURL, "https://romm.example.invalid/environment")
				t.Setenv(EnvRomMClientAPIToken, "synthetic-env-romm-token")
			}

			server, err := LoadServer()
			if err != nil {
				t.Fatalf("unneeded malformed provider file prevented complete environment resolution: %v", err)
			}
			if !server.IGDB.HasCredentials() || server.InventorySource != test.selector {
				t.Fatal("complete environment bundle was not resolved without disturbing source selection")
			}
			if test.rommEnv && !server.RomM.HasCredentials() {
				t.Fatal("complete RomM environment bundle was not resolved")
			}
		})
	}
}

func TestServerDiagnosticsNeverRevealProviderValues(t *testing.T) {
	server := Server{
		IGDB: IGDBCredentials{ClientID: "synthetic-server-igdb-id", ClientSecret: "synthetic-server-igdb-secret"},
		RomM: RomMCredentials{BaseURL: "https://romm.example.invalid/synthetic-server-path", ClientAPIToken: "synthetic-server-romm-token"},
	}
	markers := []string{server.IGDB.ClientID, server.IGDB.ClientSecret, server.RomM.BaseURL, server.RomM.ClientAPIToken}
	for _, formatted := range []string{fmt.Sprintf("%v", server), fmt.Sprintf("%+v", server), fmt.Sprintf("%#v", server)} {
		for _, marker := range markers {
			if strings.Contains(formatted, marker) {
				t.Fatal("Server formatting exposed provider values")
			}
		}
	}
	encoded, err := json.Marshal(server)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range markers {
		if strings.Contains(string(encoded), marker) {
			t.Fatal("Server JSON diagnostics exposed provider values")
		}
	}
}

func TestIGDBCredentialsFormattingNeverRevealsValues(t *testing.T) {
	credentials := IGDBCredentials{ClientID: "synthetic-client-id", ClientSecret: "synthetic-secret-value"}
	for _, formatted := range []string{fmt.Sprintf("%v", credentials), fmt.Sprintf("%+v", credentials), fmt.Sprintf("%#v", credentials)} {
		if strings.Contains(formatted, credentials.ClientID) || strings.Contains(formatted, credentials.ClientSecret) {
			t.Fatal("credential formatting exposed a value")
		}
	}
	encoded, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "{}" {
		t.Fatalf("credential JSON representation was not redacted")
	}
}

func TestRomMCredentialsFormattingNeverRevealsValues(t *testing.T) {
	credentials := RomMCredentials{BaseURL: "http://127.0.0.1:32400", ClientAPIToken: "synthetic-romm-token"}
	for _, formatted := range []string{fmt.Sprintf("%v", credentials), fmt.Sprintf("%+v", credentials), fmt.Sprintf("%#v", credentials)} {
		if strings.Contains(formatted, credentials.BaseURL) || strings.Contains(formatted, credentials.ClientAPIToken) {
			t.Fatal("RomM credential formatting exposed a configured value")
		}
	}
	encoded, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "{}" {
		t.Fatalf("RomM credential JSON representation was not redacted")
	}
}

type testProviderConfigRoots struct {
	home   string
	config string
}

func clearIGDBEnv(t *testing.T) testProviderConfigRoots {
	t.Helper()
	unsetProviderEnv(t, EnvIGDBClientID, EnvIGDBClientSecret, EnvRomMBaseURL, EnvRomMClientAPIToken)
	unsetProviderEnv(t, EnvWikidataContactEmail)
	root := t.TempDir()
	roots := testProviderConfigRoots{home: filepath.Join(root, "home"), config: filepath.Join(root, "config")}
	t.Setenv("HOME", roots.home)
	t.Setenv("XDG_CONFIG_HOME", roots.config)
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	for _, name := range []string{
		EnvServerDatabasePath, EnvServerListenAddr, EnvAgentSocketPath,
		EnvAgentCachePath, EnvAgentStagingPath, EnvInventoryManifestPath, EnvInventorySource,
	} {
		t.Setenv(name, "")
	}
	return roots
}

func unsetProviderEnv(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("clear %s: %v", name, err)
		}
	}
}
