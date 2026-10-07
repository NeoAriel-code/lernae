package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lernae/internal/config"
)

const (
	cliTestIGDBID          = "synthetic-igdb-id-marker"
	cliTestIGDBSecret      = "synthetic-igdb-secret-marker"
	cliTestRomMURL         = "https://romm.example.invalid/synthetic-marker"
	cliTestRomMToken       = "synthetic-romm-token-marker"
	cliTestStoreError      = "synthetic-store-error-marker"
	cliTestWikidataContact = "operator@example.com"
)

type queuedSecretReader struct {
	values  []string
	prompts []string
	err     error
}

type queuedLineReader struct {
	values  []string
	prompts []string
}

type interleavingSettingsStore struct {
	base        runtimeSettingsStore
	beforeWrite func() error
}

func (store *interleavingSettingsStore) Load() (config.RuntimeSettings, error) {
	return store.base.Load()
}

func (store *interleavingSettingsStore) Update(change func(*config.RuntimeSettings)) error {
	if store.beforeWrite != nil {
		beforeWrite := store.beforeWrite
		store.beforeWrite = nil
		if err := beforeWrite(); err != nil {
			return err
		}
	}
	return store.base.Update(change)
}

func (reader *queuedLineReader) ReadLine(prompt string) (string, error) {
	reader.prompts = append(reader.prompts, prompt)
	if len(reader.values) == 0 {
		return "", errors.New("test line reader exhausted")
	}
	value := reader.values[0]
	reader.values = reader.values[1:]
	return value, nil
}

func (reader *queuedSecretReader) ReadSecret(prompt string) (string, error) {
	reader.prompts = append(reader.prompts, prompt)
	if reader.err != nil {
		return "", reader.err
	}
	if len(reader.values) == 0 {
		return "", errors.New("test reader exhausted")
	}
	value := reader.values[0]
	reader.values = reader.values[1:]
	return value, nil
}

type failingProviderStore struct {
	err error
}

func (store failingProviderStore) Load() (config.ProviderConfig, error) {
	return config.ProviderConfig{}, store.err
}

func (store failingProviderStore) SaveIGDB(config.IGDBCredentials) error {
	return store.err
}

func (store failingProviderStore) SaveRomM(config.RomMCredentials) error {
	return store.err
}

func (store failingProviderStore) SaveProwlarr(config.ProwlarrCredentials) error { return store.err }
func (store failingProviderStore) ClearProwlarr() error                          { return store.err }

type failProwlarrSettingsStore struct {
	base   runtimeSettingsStore
	calls  int
	failAt int
}

func (s *failProwlarrSettingsStore) Load() (config.RuntimeSettings, error) { return s.base.Load() }
func (s *failProwlarrSettingsStore) Update(change func(*config.RuntimeSettings)) error {
	s.calls++
	if s.calls == s.failAt {
		return errors.New("PRIVATE_SETTINGS_FAILURE")
	}
	return s.base.Update(change)
}

func TestConfigureQBittorrentTwoStoreFailureNeverEnables(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		providers, xdg, home := newCommandTestStore(t)
		settings, err := config.NewFileRuntimeSettingsStore(xdg, home)
		if err != nil {
			t.Fatal(err)
		}
		failing := &failProwlarrSettingsStore{base: settings, failAt: failAt}
		var output bytes.Buffer
		err = runWithStores([]string{"configure", "qbittorrent"}, providers, failing, &queuedSecretReader{values: []string{"PRIVATE_PASSWORD"}}, &queuedLineReader{values: []string{"true", "http://127.0.0.1:1", "operator"}}, &output)
		if err != errRuntimeSettingsSave {
			t.Fatal("unsafe two-store failure not reported safely")
		}
		loaded, err := settings.Load()
		if err != nil || loaded.QBittorrent.Enabled || strings.Contains(output.String(), "PRIVATE_PASSWORD") {
			t.Fatal("partial configure enabled incomplete state or leaked secret")
		}
	}
}

func TestConfigureQBittorrentPrivateRoundtripAndClear(t *testing.T) {
	providers, xdg, home := newCommandTestStore(t)
	settings, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	endpoint := "http://private-qbit.invalid:8080/base"
	password := "PRIVATE_QBIT_PASSWORD"
	if err := runWithStores([]string{"configure", "qbittorrent"}, providers, settings, &queuedSecretReader{values: []string{password}}, &queuedLineReader{values: []string{"true", endpoint, "private-operator"}}, &output); err != nil {
		t.Fatal("qBittorrent configure command unavailable")
	}
	loaded, err := settings.Load()
	credentials, credentialErr := providers.Load()
	if err != nil || credentialErr != nil || !loaded.QBittorrent.Enabled || loaded.QBittorrent.BaseURL != endpoint || loaded.QBittorrent.Username != "private-operator" || credentials.QBittorrent.Password != password {
		t.Fatal("qBittorrent persistent roundtrip failed")
	}
	if err := runWithStores([]string{"configure", "qbittorrent"}, providers, settings, &queuedSecretReader{values: []string{""}}, &queuedLineReader{values: []string{"", "", ""}}, &output); err != nil {
		t.Fatal(err)
	}
	if err := runWithStores([]string{"status"}, providers, settings, nil, nil, &output); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"qbittorrent_enabled=true", "qbittorrent_endpoint_present=true", "qbittorrent_username_present=true", "qbittorrent_password_present=true", "qbittorrent_configured=true"} {
		if !strings.Contains(output.String(), flag) {
			t.Fatal("qBittorrent boolean status missing")
		}
	}
	for _, private := range []string{endpoint, password, "private-operator"} {
		if strings.Contains(output.String(), private) {
			t.Fatal("qBittorrent CLI leaked private config")
		}
	}
	if err := runWithStores([]string{"configure", "qbittorrent"}, providers, settings, &queuedSecretReader{values: []string{"default"}}, &queuedLineReader{values: []string{"", "default", "default"}}, &output); err != nil {
		t.Fatal(err)
	}
	loaded, _ = settings.Load()
	credentials, _ = providers.Load()
	if loaded.QBittorrent != (config.QBittorrentSettings{}) || credentials.QBittorrent.HasCredentials() {
		t.Fatal("qBittorrent clear not disabled/cleared")
	}
}

func TestConfigureProwlarrPersistentSparseRotationDisableClearAndPrivateStatus(t *testing.T) {
	providers, xdg, home := newCommandTestStore(t)
	settings, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveIGDB(config.IGDBCredentials{ClientID: cliTestIGDBID, ClientSecret: cliTestIGDBSecret}); err != nil {
		t.Fatal(err)
	}
	if err := settings.Update(func(s *config.RuntimeSettings) {
		s.Metadata.Language = config.MetadataLanguageSpanish
		s.Wikidata.ContactEmail = cliTestWikidataContact
		s.LocalAcquisition.SourceRoot = "/private/source"
	}); err != nil {
		t.Fatal(err)
	}
	key := "PRIVATE_PROWLARR_CLI_KEY"
	endpoint := "https://PRIVATE.example.invalid:443/base/"
	var output bytes.Buffer
	lines := &queuedLineReader{values: []string{"true", endpoint}}
	secrets := &queuedSecretReader{values: []string{key}}
	if err := runWithStores([]string{"configure", "prowlarr"}, providers, settings, secrets, lines, &output); err != nil {
		t.Fatal(err)
	}
	fresh, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := fresh.Load()
	if err != nil || !loaded.Prowlarr.Enabled || loaded.Prowlarr.BaseURL != "https://private.example.invalid/base" || loaded.Metadata.Language != config.MetadataLanguageSpanish || loaded.Wikidata.ContactEmail != cliTestWikidataContact || loaded.LocalAcquisition.SourceRoot != "/private/source" {
		t.Fatal("configure did not preserve persistent sparse fields")
	}
	if err := runWithStores([]string{"status"}, providers, fresh, nil, nil, &output); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"prowlarr_enabled=true", "prowlarr_endpoint_present=true", "prowlarr_key_present=true", "prowlarr_configured=true"} {
		if !strings.Contains(output.String(), flag) {
			t.Fatalf("status omitted %s", flag)
		}
	}
	for _, private := range []string{key, endpoint, "private.example.invalid"} {
		if strings.Contains(output.String()+strings.Join(lines.prompts, " ")+strings.Join(secrets.prompts, " "), private) {
			t.Fatal("CLI echoed endpoint/secret")
		}
	}
	// Rotation keeps the prior enabled setting after the temporary safety disable.
	if err := runWithStores([]string{"configure", "prowlarr"}, providers, fresh, &queuedSecretReader{values: []string{"ROTATED_PRIVATE_KEY"}}, &queuedLineReader{values: []string{"", ""}}, &output); err != nil {
		t.Fatal(err)
	}
	loaded, _ = fresh.Load()
	if !loaded.Prowlarr.Enabled {
		t.Fatal("blank enabled answer did not preserve state on rotation")
	}
	if err := runWithStores([]string{"configure", "prowlarr"}, providers, fresh, &queuedSecretReader{values: []string{""}}, &queuedLineReader{values: []string{"false", ""}}, &output); err != nil {
		t.Fatal(err)
	}
	loaded, _ = fresh.Load()
	credentials, _ := providers.Load()
	if loaded.Prowlarr.Enabled || !credentials.Prowlarr.HasCredentials() || loaded.Prowlarr.BaseURL == "" {
		t.Fatal("disable unexpectedly cleared saved config")
	}
	if err := runWithStores([]string{"configure", "prowlarr"}, providers, fresh, &queuedSecretReader{values: []string{"default"}}, &queuedLineReader{values: []string{"", "default"}}, &output); err != nil {
		t.Fatal(err)
	}
	loaded, _ = fresh.Load()
	credentials, _ = providers.Load()
	if loaded.Prowlarr != (config.ProwlarrSettings{}) || credentials.Prowlarr.HasCredentials() || credentials.IGDB.ClientID != cliTestIGDBID {
		t.Fatal("clear changed unrelated credential bundle")
	}
}

func TestConfigureProwlarrTwoStoreFailuresNeverEnableIncompleteState(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			providers, xdg, home := newCommandTestStore(t)
			settings, err := config.NewFileRuntimeSettingsStore(xdg, home)
			if err != nil {
				t.Fatal(err)
			}
			if err := providers.SaveProwlarr(config.ProwlarrCredentials{APIKey: "ORIGINAL_PRIVATE_KEY"}); err != nil {
				t.Fatal(err)
			}
			if err := settings.Update(func(s *config.RuntimeSettings) {
				s.Prowlarr = config.ProwlarrSettings{Enabled: true, BaseURL: "https://host.invalid"}
			}); err != nil {
				t.Fatal(err)
			}
			store := &failProwlarrSettingsStore{base: settings, failAt: failAt}
			var output bytes.Buffer
			err = runWithStores([]string{"configure", "prowlarr"}, providers, store, &queuedSecretReader{values: []string{"NEW_PRIVATE_KEY"}}, &queuedLineReader{values: []string{"true", "https://new.invalid"}}, &output)
			if err != errRuntimeSettingsSave || strings.Contains(err.Error()+output.String(), "PRIVATE") {
				t.Fatal("unsafe two-store error")
			}
			loaded, _ := settings.Load()
			credentials, _ := providers.Load()
			if failAt == 1 {
				if !loaded.Prowlarr.Enabled || credentials.Prowlarr.APIKey != "ORIGINAL_PRIVATE_KEY" || loaded.Prowlarr.BaseURL != "https://host.invalid" {
					t.Fatal("failed safety disable changed original state")
				}
			} else if loaded.Prowlarr.Enabled || credentials.Prowlarr.APIKey != "NEW_PRIVATE_KEY" || loaded.Prowlarr.BaseURL != "https://host.invalid" {
				t.Fatal("failed final settings write left falsely enabled state")
			}
		})
	}
	providers, xdg, home := newCommandTestStore(t)
	settings, _ := config.NewFileRuntimeSettingsStore(xdg, home)
	var output bytes.Buffer
	if err := runWithStores([]string{"configure", "prowlarr"}, providers, settings, &queuedSecretReader{values: []string{""}}, &queuedLineReader{values: []string{"true", "https://host.invalid"}}, &output); err != errRuntimeSettingsSave {
		t.Fatal("keyless provider enabled")
	}
	if err := runWithStores([]string{"configure", "prowlarr"}, providers, settings, &queuedSecretReader{values: []string{"key"}}, &queuedLineReader{values: []string{"true", "http://user:secret@host"}}, &output); err != errRuntimeSettingsSave {
		t.Fatal("credential URL accepted")
	}
}

func TestConfigureLocalReservedStagingRefusalIsPrivateAndAtomic(t *testing.T) {
	providers, xdg, home := newCommandTestStore(t)
	settings, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	source := filepath.Join(base, "PRIVATE_SOURCE")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"assets", "assets/PRIVATE_STAGING"} {
		staging := filepath.Join(base, relative)
		if err := os.MkdirAll(staging, 0700); err != nil {
			t.Fatal(err)
		}
		for _, enabled := range []string{"true", "false"} {
			var output bytes.Buffer
			lines := &queuedLineReader{values: []string{enabled, source, staging}}
			err := runWithStores([]string{"configure", "local"}, providers, settings, nil, lines, &output)
			if err != errRuntimeSettingsSave {
				t.Errorf("reserved staging did not return fixed save error: %v", err)
			}
			if strings.Contains(output.String()+strings.Join(lines.prompts, " "), base) {
				t.Error("refusal leaked private root")
			}
			loaded, err := settings.Load()
			if err != nil || loaded.LocalAcquisition != (config.LocalAcquisitionSettings{}) {
				t.Error("refusal changed settings")
			}
		}
	}
}

func TestConfigureLocalPersistentEnablePartialUpdateDisableAndStatusPrivacy(t *testing.T) {
	providers, xdg, home := newCommandTestStore(t)
	settings, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	source, staging := filepath.Join(base, "PRIVATE_SOURCE"), filepath.Join(base, "PRIVATE_STAGING")
	for _, path := range []string{source, staging} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := settings.Update(func(s *config.RuntimeSettings) { s.Metadata.Language = config.MetadataLanguageSpanish }); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runWithStores([]string{"configure", "local"}, providers, settings, nil, &queuedLineReader{values: []string{"true", source, staging}}, &output); err != nil {
		t.Fatal(err)
	}
	fresh, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := fresh.Load()
	if err != nil || !loaded.LocalAcquisition.Enabled || loaded.LocalAcquisition.SourceRoot != source || loaded.LocalAcquisition.StagingRoot != staging || loaded.Metadata.Language != config.MetadataLanguageSpanish {
		t.Fatal("local settings were not saved persistently")
	}
	output.Reset()
	if err := runWithStores([]string{"status"}, providers, fresh, nil, nil, &output); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"local_acquisition_enabled=true", "local_acquisition_source_configured=true", "local_acquisition_staging_configured=true"} {
		if !strings.Contains(output.String(), flag) {
			t.Fatalf("status omitted %s", flag)
		}
	}
	if strings.Contains(output.String(), source) || strings.Contains(output.String(), staging) {
		t.Fatal("status leaked private roots")
	}
	// Blank root answers preserve roots; disabling needs no healthy filesystem.
	if err := os.Rename(source, source+"-gone"); err != nil {
		t.Fatal(err)
	}
	if err := runWithStores([]string{"configure", "local"}, providers, fresh, nil, &queuedLineReader{values: []string{"false", "", ""}}, &output); err != nil {
		t.Fatal(err)
	}
	disabled, err := settings.Load()
	if err != nil || disabled.LocalAcquisition.Enabled || disabled.LocalAcquisition.SourceRoot != source {
		t.Fatal("disable lost saved roots")
	}
	if err := runWithStores([]string{"configure", "local"}, providers, fresh, nil, &queuedLineReader{values: []string{"true", "", ""}}, &output); !errors.Is(err, errRuntimeSettingsSave) {
		t.Fatal("missing source enabled")
	}
	if strings.Contains(output.String(), source) || strings.Contains(output.String(), staging) {
		t.Fatal("configure echoed private roots")
	}
	if err := runWithStores([]string{"configure", "local"}, providers, fresh, nil, &queuedLineReader{values: []string{"default", "default", "default"}}, &output); err != nil {
		t.Fatal(err)
	}
	cleared, err := settings.Load()
	if err != nil || cleared.LocalAcquisition != (config.LocalAcquisitionSettings{}) || cleared.Metadata.Language != config.MetadataLanguageSpanish {
		t.Fatal("clear failed or erased unrelated settings")
	}
}

func TestConfigureLocalRejectsSymlinkAndOverlappingRootsWithoutChangingSettings(t *testing.T) {
	providers, xdg, home := newCommandTestStore(t)
	settings, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	source, staging := filepath.Join(t.TempDir(), "source"), filepath.Join(t.TempDir(), "staging")
	for _, path := range []string{source, staging} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{source, source}, {link, staging}, {source, "/"}, {"relative", staging}} {
		var output bytes.Buffer
		err := runWithStores([]string{"configure", "local"}, providers, settings, nil, &queuedLineReader{values: []string{"true", pair[0], pair[1]}}, &output)
		if !errors.Is(err, errRuntimeSettingsSave) {
			t.Fatalf("unsafe local configure err=%v", err)
		}
		loaded, err := settings.Load()
		if err != nil || loaded.LocalAcquisition != (config.LocalAcquisitionSettings{}) {
			t.Fatal("invalid configure partially saved")
		}
	}
}

func TestRunConfigureIGDBAndFreshCommandLoadsSavedBundle(t *testing.T) {
	store, xdg, home := newCommandTestStore(t)
	reader := &queuedSecretReader{values: []string{cliTestIGDBID, cliTestIGDBSecret}}
	var output bytes.Buffer

	if err := run([]string{"configure", "igdb"}, store, reader, &output); err != nil {
		t.Fatalf("run(configure igdb) error = %v", err)
	}
	if len(reader.prompts) != 2 {
		t.Fatalf("password prompts = %d, want 2", len(reader.prompts))
	}
	if got, want := output.String(), "Provider configuration saved.\n"; got != want {
		t.Fatalf("configure output = %q, want %q", got, want)
	}
	assertNoCredentialMarkers(t, output.String())

	// A new command/store instance must observe the persisted bundle.
	freshStore, err := config.NewFileProviderConfigStore(xdg, home)
	if err != nil {
		t.Fatalf("new fresh store: %v", err)
	}
	loaded, err := freshStore.Load()
	if err != nil {
		t.Fatalf("fresh store Load() error = %v", err)
	}
	if got := loaded.IGDB; got != (config.IGDBCredentials{ClientID: cliTestIGDBID, ClientSecret: cliTestIGDBSecret}) {
		t.Fatalf("fresh store IGDB bundle = %#v, want configured synthetic bundle", got)
	}

	var status bytes.Buffer
	if err := run([]string{"status"}, freshStore, nil, &status); err != nil {
		t.Fatalf("fresh command status error = %v", err)
	}
	if got, want := status.String(), "igdb_configured=true\nromm_configured=false\n"; got != want {
		t.Fatalf("fresh command status = %q, want %q", got, want)
	}
	assertNoCredentialMarkers(t, status.String())
}

func TestParseOperationAcceptsUnifiedConfigure(t *testing.T) {
	if _, err := parseOperation([]string{"configure"}); err != nil {
		t.Fatalf("parseOperation(configure) error = %v, want unified configure operation", err)
	}
}

func TestUnifiedConfigureEnterPreservesSavedValuesInsteadOfEnvironmentOverrides(t *testing.T) {
	providerStore, xdg, home := newCommandTestStore(t)
	settingsStore, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	persisted := config.RuntimeSettings{
		Server:    config.ServerSettings{DatabasePath: "/tmp/saved/server.db", ListenAddress: "127.0.0.1:8181"},
		Inventory: config.InventorySettings{Source: "manifest", ManifestPath: "/tmp/saved/manifest.json"},
	}
	persisted.Version = config.RuntimeSettingsVersion
	if err := settingsStore.Save(persisted); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvServerListenAddr, "127.0.0.1:9999")
	lines := &queuedLineReader{values: []string{"", "", "", "", "", "", "", "", "n", "n"}}
	var output bytes.Buffer
	if err := runWithStores([]string{"configure"}, providerStore, settingsStore, nil, lines, &output); err != nil {
		t.Fatalf("runWithStores(configure with blank values) error = %v", err)
	}
	loaded, err := settingsStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded != persisted {
		t.Fatalf("Enter changed saved/default settings to %#v, want %#v", loaded, persisted)
	}
	if strings.Contains(output.String(), "127.0.0.1:9999") || !strings.Contains(output.String(), "127.0.0.1:8181") {
		t.Fatalf("configuration displayed an environment override instead of saved settings: %q", output.String())
	}
}

func TestUnifiedConfigureUpdatesSettingsWithoutReplacingProviderStore(t *testing.T) {
	providerStore, xdg, home := newCommandTestStore(t)
	if err := providerStore.SaveRomM(config.RomMCredentials{BaseURL: cliTestRomMURL, ClientAPIToken: cliTestRomMToken}); err != nil {
		t.Fatal(err)
	}
	settingsStore, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	original := config.RuntimeSettings{
		Server:    config.ServerSettings{ListenAddress: "127.0.0.1:8181"},
		Agent:     config.AgentSettings{CachePath: "/tmp/saved/cache"},
		Inventory: config.InventorySettings{Source: "manifest", ManifestPath: "/tmp/saved/manifest.json"},
	}
	original.Version = config.RuntimeSettingsVersion
	if err := settingsStore.Save(original); err != nil {
		t.Fatal(err)
	}
	lines := &queuedLineReader{values: []string{"/tmp/updated/server.db", "", "", "", "", "", "", "", "y", "n"}}
	secrets := &queuedSecretReader{values: []string{cliTestIGDBID, cliTestIGDBSecret}}
	var output bytes.Buffer
	if err := runWithStores([]string{"configure"}, providerStore, settingsStore, secrets, lines, &output); err != nil {
		t.Fatalf("runWithStores(configure) error = %v", err)
	}
	updated, err := settingsStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := original
	want.Server.DatabasePath = "/tmp/updated/server.db"
	if updated != want {
		t.Fatalf("updated settings = %#v, want %#v", updated, want)
	}
	providers, err := providerStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if providers.IGDB != (config.IGDBCredentials{ClientID: cliTestIGDBID, ClientSecret: cliTestIGDBSecret}) ||
		providers.RomM != (config.RomMCredentials{BaseURL: cliTestRomMURL, ClientAPIToken: cliTestRomMToken}) {
		t.Fatal("settings configure lost or failed to update an independent provider bundle")
	}
	for _, marker := range []string{cliTestIGDBID, cliTestIGDBSecret, cliTestRomMURL, cliTestRomMToken} {
		if strings.Contains(output.String(), marker) {
			t.Fatalf("unified configure output leaked credential marker %q", marker)
		}
	}
}

func TestUnifiedConfigureRejectsRomMInventoryWithoutPersistentCredentials(t *testing.T) {
	providerStore, xdg, home := newCommandTestStore(t)
	settingsStore, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	original := config.RuntimeSettings{Inventory: config.InventorySettings{Source: "manifest", ManifestPath: "/tmp/saved/manifest.json"}}
	original.Version = config.RuntimeSettingsVersion
	if err := settingsStore.Save(original); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvRomMBaseURL, cliTestRomMURL)
	t.Setenv(config.EnvRomMClientAPIToken, cliTestRomMToken)
	lines := &queuedLineReader{values: []string{"", "", "", "", "", "romm", "", "", "n", "n"}}
	var output bytes.Buffer
	err = runWithStores([]string{"configure"}, providerStore, settingsStore, nil, lines, &output)
	if err == nil || !strings.Contains(err.Error(), "make configure-romm") {
		t.Fatalf("runWithStores(configure RomM inventory) error = %v, want actionable persistent-credential requirement", err)
	}
	loaded, err := settingsStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded != original {
		t.Fatalf("settings were changed without persistent RomM credentials: %#v, want %#v", loaded, original)
	}
	providers, err := providerStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if providers.RomM.HasCredentials() {
		t.Fatal("environment-only RomM credentials were copied into the provider store")
	}
	if strings.Contains(output.String(), "Configuration saved.") || strings.Contains(output.String(), cliTestRomMToken) {
		t.Fatalf("wizard reported success or exposed credentials: %q", output.String())
	}
}

func TestUnifiedConfigureCanPersistRomMCredentialsBeforeSelectingInventory(t *testing.T) {
	providerStore, xdg, home := newCommandTestStore(t)
	settingsStore, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	lines := &queuedLineReader{values: []string{"", "", "", "", "", "romm", "", "", "n", "y"}}
	secrets := &queuedSecretReader{values: []string{cliTestRomMURL, cliTestRomMToken}}
	var output bytes.Buffer
	if err := runWithStores([]string{"configure"}, providerStore, settingsStore, secrets, lines, &output); err != nil {
		t.Fatalf("runWithStores(configure RomM inventory with credentials) error = %v", err)
	}
	saved, err := settingsStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Inventory.Source != "romm" {
		t.Fatalf("saved inventory source = %q, want romm", saved.Inventory.Source)
	}
	providers, err := providerStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if providers.RomM != (config.RomMCredentials{BaseURL: cliTestRomMURL, ClientAPIToken: cliTestRomMToken}) {
		t.Fatal("persistent RomM bundle was not saved before selecting RomM inventory")
	}
}

func TestUnifiedConfigureRejectsInvalidRuntimeSettingsWithoutSaving(t *testing.T) {
	providerStore, xdg, home := newCommandTestStore(t)
	settingsStore, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	original := config.RuntimeSettings{Server: config.ServerSettings{DatabasePath: "/tmp/saved/server.db"}}
	original.Version = config.RuntimeSettingsVersion
	if err := settingsStore.Save(original); err != nil {
		t.Fatal(err)
	}
	lines := &queuedLineReader{values: []string{"relative/database.db", "", "", "", "", "", "", "", "n", "n"}}
	var output bytes.Buffer
	if err := runWithStores([]string{"configure"}, providerStore, settingsStore, nil, lines, &output); !errors.Is(err, errRuntimeSettingsSave) {
		t.Fatalf("runWithStores(invalid path) error = %v, want safe settings validation failure", err)
	}
	loaded, err := settingsStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded != original {
		t.Fatalf("invalid settings partially replaced saved values: %#v", loaded)
	}
}

func TestUnifiedConfigureRejectsStaleStagingMergeAgainstLatestSettings(t *testing.T) {
	providerStore, xdg, home := newCommandTestStore(t)
	baseSettingsStore, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	initial := config.RuntimeSettings{
		Agent: config.AgentSettings{
			CachePath:   "/tmp/old/cache",
			StagingPath: "/tmp/old/cache/.staging",
		},
	}
	initial.Version = config.RuntimeSettingsVersion
	if err := baseSettingsStore.Save(initial); err != nil {
		t.Fatal(err)
	}
	interleaved := &interleavingSettingsStore{
		base: baseSettingsStore,
		beforeWrite: func() error {
			return baseSettingsStore.Update(func(latest *config.RuntimeSettings) {
				latest.Agent.CachePath = "/tmp/new/cache"
				latest.Agent.StagingPath = "/tmp/new/cache/.staging"
			})
		},
	}
	lines := &queuedLineReader{values: []string{"", "", "", "", "/tmp/old/cache/custom-staging", "", "", "", "n", "n"}}
	var output bytes.Buffer
	err = runWithStores([]string{"configure"}, providerStore, interleaved, nil, lines, &output)
	if !errors.Is(err, errRuntimeSettingsSave) {
		t.Fatalf("configure stale staging merge error = %v, want safe settings validation failure", err)
	}
	latest, err := baseSettingsStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := initial
	want.Agent.CachePath = "/tmp/new/cache"
	want.Agent.StagingPath = "/tmp/new/cache/.staging"
	if latest != want {
		t.Fatalf("stale wizard update corrupted latest settings: got %#v, want %#v", latest, want)
	}
}

func TestUnifiedStatusShowsSavedSettingsAndOnlyProviderConfiguredBooleans(t *testing.T) {
	providerStore, xdg, home := newCommandTestStore(t)
	if err := providerStore.SaveIGDB(config.IGDBCredentials{ClientID: cliTestIGDBID, ClientSecret: cliTestIGDBSecret}); err != nil {
		t.Fatal(err)
	}
	settingsStore, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := settingsStore.Save(config.RuntimeSettings{
		Inventory: config.InventorySettings{ManifestPath: "/tmp/saved/manifest.json"},
		Wikidata:  config.WikidataSettings{ContactEmail: cliTestWikidataContact},
	}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runWithStores([]string{"status"}, providerStore, settingsStore, nil, nil, &output); err != nil {
		t.Fatalf("runWithStores(status) error = %v", err)
	}
	for _, expected := range []string{"igdb_configured=true", "romm_configured=false", "inventory_source=manifest (default)", "inventory_manifest_path=/tmp/saved/manifest.json", "wikidata_configured=true"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("status omitted %q: %q", expected, output.String())
		}
	}
	assertNoCredentialMarkers(t, output.String())
	if strings.Contains(output.String(), cliTestWikidataContact) {
		t.Fatalf("status exposed the configured Wikidata contact: %q", output.String())
	}
}

func TestUnifiedConfigureAddsUpdatesAndClearsWikidataContactWithoutReenteringProviders(t *testing.T) {
	for _, test := range []struct {
		name           string
		initialContact string
		contactInput   string
		wantContact    string
		wantConfigured string
	}{
		{name: "add", contactInput: cliTestWikidataContact, wantContact: cliTestWikidataContact, wantConfigured: "true"},
		{name: "update", initialContact: "invalid-saved-contact-marker", contactInput: cliTestWikidataContact, wantContact: cliTestWikidataContact, wantConfigured: "true"},
		{name: "retain-invalid-as-unconfigured", initialContact: "invalid-saved-contact-marker", wantContact: "invalid-saved-contact-marker", wantConfigured: "false"},
		{name: "clear", initialContact: cliTestWikidataContact, contactInput: "default", wantConfigured: "false"},
	} {
		t.Run(test.name, func(t *testing.T) {
			providerStore, xdg, home := newCommandTestStore(t)
			originalProviders := config.ProviderConfig{
				IGDB: config.IGDBCredentials{ClientID: cliTestIGDBID, ClientSecret: cliTestIGDBSecret},
				RomM: config.RomMCredentials{BaseURL: cliTestRomMURL, ClientAPIToken: cliTestRomMToken},
			}
			if err := providerStore.SaveIGDB(originalProviders.IGDB); err != nil {
				t.Fatal(err)
			}
			if err := providerStore.SaveRomM(originalProviders.RomM); err != nil {
				t.Fatal(err)
			}
			originalProviders, err := providerStore.Load()
			if err != nil {
				t.Fatal(err)
			}
			settingsStore, err := config.NewFileRuntimeSettingsStore(xdg, home)
			if err != nil {
				t.Fatal(err)
			}
			originalSettings := config.RuntimeSettings{
				Server:    config.ServerSettings{ListenAddress: "127.0.0.1:8181"},
				Agent:     config.AgentSettings{CachePath: "/tmp/saved/cache"},
				Inventory: config.InventorySettings{ManifestPath: "/tmp/saved/manifest.json"},
				Wikidata:  config.WikidataSettings{ContactEmail: test.initialContact},
			}
			if err := settingsStore.Save(originalSettings); err != nil {
				t.Fatal(err)
			}
			lines := &queuedLineReader{values: []string{"", "", "", "", "", "", "", test.contactInput, "n", "n"}}
			secrets := &queuedSecretReader{}
			var configureOutput bytes.Buffer
			if err := runWithStores([]string{"configure"}, providerStore, settingsStore, secrets, lines, &configureOutput); err != nil {
				t.Fatalf("configure contact error = %v", err)
			}
			if len(lines.values) != 0 || len(secrets.prompts) != 0 {
				t.Fatalf("configure did not finish as a partial settings update: remaining values=%d secret prompts=%d", len(lines.values), len(secrets.prompts))
			}
			if strings.Contains(configureOutput.String(), cliTestWikidataContact) || strings.Contains(strings.Join(lines.prompts, "\n"), cliTestWikidataContact) {
				t.Fatal("configure output or prompt exposed the Wikidata contact")
			}

			freshSettingsStore, err := config.NewFileRuntimeSettingsStore(xdg, home)
			if err != nil {
				t.Fatal(err)
			}
			gotSettings, err := freshSettingsStore.Load()
			if err != nil {
				t.Fatal(err)
			}
			wantSettings := originalSettings
			wantSettings.Wikidata.ContactEmail = test.wantContact
			wantSettings.Version = config.RuntimeSettingsVersion
			if gotSettings != wantSettings {
				t.Fatalf("reopened runtime settings = %#v, want %#v", gotSettings, wantSettings)
			}
			gotProviders, err := providerStore.Load()
			if err != nil {
				t.Fatal(err)
			}
			if gotProviders != originalProviders {
				t.Fatalf("provider credentials changed during settings update: got %#v, want original configured bundles", gotProviders)
			}

			var status bytes.Buffer
			if err := runWithStores([]string{"status"}, providerStore, freshSettingsStore, nil, nil, &status); err != nil {
				t.Fatalf("status after configure error = %v", err)
			}
			if !strings.Contains(status.String(), "wikidata_configured="+test.wantConfigured+"\n") || strings.Contains(status.String(), cliTestWikidataContact) {
				t.Fatalf("status did not expose only the expected boolean: %q", status.String())
			}
		})
	}
}

func TestUnifiedConfigureAddsWikidataContactToExistingVersionOneSettings(t *testing.T) {
	providerStore, xdg, home := newCommandTestStore(t)
	settingsStore, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(xdg, "lernae", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	legacySettings := `{"version":1,"settings":{"server":{"database_path":"/synthetic/server.db","listen_address":"127.0.0.1:8181"},"agent":{"cache_path":"/synthetic/cache"},"inventory":{"source":"manifest","manifest_path":"/synthetic/manifest.json"},"metadata":{"language":"es"}}}`
	if err := os.WriteFile(settingsPath, []byte(legacySettings), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(settingsPath, 0o600); err != nil {
		t.Fatal(err)
	}

	lines := &queuedLineReader{values: []string{"", "", "", "", "", "", "", cliTestWikidataContact, "n", "n"}}
	var output bytes.Buffer
	err = runWithStores([]string{"configure"}, providerStore, settingsStore, nil, lines, &output)
	if err != nil {
		t.Fatalf("configure first contact from existing version-1 settings error = %v", err)
	}
	freshStore, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	got, err := freshStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := config.RuntimeSettings{
		Version:   config.RuntimeSettingsVersion,
		Server:    config.ServerSettings{DatabasePath: "/synthetic/server.db", ListenAddress: "127.0.0.1:8181"},
		Agent:     config.AgentSettings{CachePath: "/synthetic/cache"},
		Inventory: config.InventorySettings{Source: "manifest", ManifestPath: "/synthetic/manifest.json"},
		Metadata:  config.MetadataSettings{Language: config.MetadataLanguageSpanish},
		Wikidata:  config.WikidataSettings{ContactEmail: cliTestWikidataContact},
	}
	if got != want {
		t.Fatalf("settings after first contact save = %#v, want %#v", got, want)
	}
	if strings.Contains(output.String(), cliTestWikidataContact) || strings.Contains(strings.Join(lines.prompts, "\n"), cliTestWikidataContact) {
		t.Fatal("first contact save exposed the synthetic contact")
	}
}

func TestUnifiedConfigureAddsWikidataContactToExistingVersionZeroSettings(t *testing.T) {
	providerStore, xdg, home := newCommandTestStore(t)
	settingsStore, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(xdg, "lernae", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	legacySettings := `{"version":0,"settings":{"inventory":{"source":"manifest","manifest_path":"/synthetic/manifest.json"},"metadata":{"language":"es"}}}`
	if err := os.WriteFile(settingsPath, []byte(legacySettings), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(settingsPath, 0o600); err != nil {
		t.Fatal(err)
	}

	lines := &queuedLineReader{values: []string{"", "", "", "", "", "", "", cliTestWikidataContact, "n", "n"}}
	var output bytes.Buffer
	err = runWithStores([]string{"configure"}, providerStore, settingsStore, nil, lines, &output)
	if err != nil {
		_, loadErr := settingsStore.Load()
		t.Fatalf("configure first contact from existing version-0 settings error = %v; fixture Load() cause = %v", err, loadErr)
	}
	freshStore, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	got, err := freshStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := config.RuntimeSettings{
		Version: config.RuntimeSettingsVersion,
		Inventory: config.InventorySettings{
			Source:       "manifest",
			ManifestPath: "/synthetic/manifest.json",
		},
		Metadata: config.MetadataSettings{Language: config.MetadataLanguageSpanish},
		Wikidata: config.WikidataSettings{ContactEmail: cliTestWikidataContact},
	}
	if got != want {
		t.Fatalf("settings after first contact save = %#v, want %#v", got, want)
	}
	if strings.Contains(output.String(), cliTestWikidataContact) || strings.Contains(strings.Join(lines.prompts, "\n"), cliTestWikidataContact) {
		t.Fatal("first contact save exposed the synthetic contact")
	}
}

func TestConfigureSettingsErrorRetainsVersionCauseWithoutExposingIt(t *testing.T) {
	providerStore, xdg, home := newCommandTestStore(t)
	settingsStore, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(xdg, "lernae", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	unsupported := `{"version":99,"settings":{"wikidata":{"contact_email":"` + cliTestWikidataContact + `"}}}`
	if err := os.WriteFile(settingsPath, []byte(unsupported), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(settingsPath, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = runWithStores([]string{"configure"}, providerStore, settingsStore, nil, &queuedLineReader{}, &output)
	if !errors.Is(err, errRuntimeSettings) {
		t.Fatalf("configure unsupported settings error = %v, want generic runtime settings sentinel", err)
	}
	if !errors.Is(err, config.ErrRuntimeSettingsVersion) {
		t.Fatalf("configure error = %v, want underlying ErrRuntimeSettingsVersion for internal diagnostics", err)
	}
	if got, want := err.Error(), errRuntimeSettings.Error(); got != want {
		t.Fatalf("operator-facing error = %q, want sanitized message %q", got, want)
	}
	if strings.Contains(err.Error(), cliTestWikidataContact) || strings.Contains(output.String(), cliTestWikidataContact) || strings.Contains(err.Error(), config.ErrRuntimeSettingsVersion.Error()) {
		t.Fatal("operator-facing error or output exposed the contact or internal schema cause")
	}
}

func TestUnifiedConfigureWikidataPromptShowsConfigurationStateWithoutEmail(t *testing.T) {
	for _, test := range []struct {
		name           string
		initialContact string
		wantPhrase     string
	}{
		{name: "never configured", wantPhrase: "not configured"},
		{name: "already configured", initialContact: cliTestWikidataContact, wantPhrase: "configured"},
	} {
		t.Run(test.name, func(t *testing.T) {
			providerStore, xdg, home := newCommandTestStore(t)
			settingsStore, err := config.NewFileRuntimeSettingsStore(xdg, home)
			if err != nil {
				t.Fatal(err)
			}
			if err := settingsStore.Save(config.RuntimeSettings{Wikidata: config.WikidataSettings{ContactEmail: test.initialContact}}); err != nil {
				t.Fatal(err)
			}
			lines := &queuedLineReader{values: []string{"", "", "", "", "", "", "", "", "n", "n"}}
			var output bytes.Buffer
			if err := runWithStores([]string{"configure"}, providerStore, settingsStore, nil, lines, &output); err != nil {
				t.Fatalf("configure prompt state error = %v", err)
			}
			prompt := lines.prompts[7]
			if !strings.Contains(prompt, test.wantPhrase) {
				t.Fatalf("Wikidata prompt = %q, want phrase %q", prompt, test.wantPhrase)
			}
			if strings.Contains(prompt, cliTestWikidataContact) || strings.Contains(output.String(), cliTestWikidataContact) {
				t.Fatal("Wikidata prompt or configure output exposed the saved contact")
			}
		})
	}
}

func TestUnifiedConfigureRejectsInvalidWikidataContactWithoutSavingOrEchoing(t *testing.T) {
	providerStore, xdg, home := newCommandTestStore(t)
	settingsStore, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	original := config.RuntimeSettings{Server: config.ServerSettings{ListenAddress: "127.0.0.1:8181"}}
	if err := settingsStore.Save(original); err != nil {
		t.Fatal(err)
	}
	lines := &queuedLineReader{values: []string{"", "", "", "", "", "", "", "not-an-email", "n", "n"}}
	var output bytes.Buffer
	err = runWithStores([]string{"configure"}, providerStore, settingsStore, nil, lines, &output)
	if !errors.Is(err, errInvalidWikidataContact) {
		t.Fatalf("configure invalid contact error = %v, want sanitized contact validation error", err)
	}
	if strings.Contains(err.Error(), "not-an-email") || strings.Contains(output.String(), "not-an-email") || strings.Contains(strings.Join(lines.prompts, "\n"), "not-an-email") {
		t.Fatal("invalid contact value was exposed in configure error, output, or prompt")
	}
	got, err := settingsStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	original.Version = config.RuntimeSettingsVersion
	if got != original {
		t.Fatalf("invalid contact partially replaced settings: got %#v, want %#v", got, original)
	}
}

func TestProviderOnlyConfigurePreservesRuntimeSettings(t *testing.T) {
	providerStore, xdg, home := newCommandTestStore(t)
	settingsStore, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	original := config.RuntimeSettings{
		Server:    config.ServerSettings{ListenAddress: "127.0.0.1:8181"},
		Inventory: config.InventorySettings{Source: "manifest", ManifestPath: "/tmp/saved/manifest.json"},
	}
	if err := settingsStore.Save(original); err != nil {
		t.Fatal(err)
	}
	if err := runWithStores([]string{"configure", "igdb"}, providerStore, settingsStore,
		&queuedSecretReader{values: []string{cliTestIGDBID, cliTestIGDBSecret}}, nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("provider-only configure error = %v", err)
	}
	updated, err := settingsStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	original.Version = config.RuntimeSettingsVersion
	if updated != original {
		t.Fatalf("provider-only configure replaced runtime settings: %#v, want %#v", updated, original)
	}
}

func TestDevProxyTargetUsesResolvedGoConfiguration(t *testing.T) {
	_, xdg, home := newCommandTestStore(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv(config.EnvServerListenAddr, "")
	settingsStore, err := config.NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runWithStores([]string{"dev-api-proxy-target"}, nil, nil, nil, nil, &output); err != nil {
		t.Fatalf("runWithStores(default target) error = %v", err)
	}
	if got, want := output.String(), "http://127.0.0.1:8081\n"; got != want {
		t.Fatalf("default dev proxy target = %q, want %q", got, want)
	}
	if err := settingsStore.Save(config.RuntimeSettings{Server: config.ServerSettings{ListenAddress: "[::1]:8181"}}); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runWithStores([]string{"dev-api-proxy-target"}, nil, nil, nil, nil, &output); err != nil {
		t.Fatalf("runWithStores(dev-api-proxy-target) error = %v", err)
	}
	if got, want := output.String(), "http://[::1]:8181\n"; got != want {
		t.Fatalf("dev proxy target = %q, want %q", got, want)
	}

	output.Reset()
	t.Setenv(config.EnvServerListenAddr, "127.0.0.1:8282")
	if err := runWithStores([]string{"dev-api-proxy-target"}, nil, nil, nil, nil, &output); err != nil {
		t.Fatalf("runWithStores(environment target) error = %v", err)
	}
	if got, want := output.String(), "http://127.0.0.1:8282\n"; got != want {
		t.Fatalf("environment dev proxy target = %q, want %q", got, want)
	}
	after, err := settingsStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if after.Server.ListenAddress != "[::1]:8181" {
		t.Fatalf("printing effective target mutated persisted settings: %#v", after.Server)
	}
}

func TestRunConfigureRomMAndStatusExposeOnlyConfiguredBooleans(t *testing.T) {
	store, _, _ := newCommandTestStore(t)
	reader := &queuedSecretReader{values: []string{cliTestRomMURL, cliTestRomMToken}}
	var output bytes.Buffer

	if err := run([]string{"configure", "romm"}, store, reader, &output); err != nil {
		t.Fatalf("run(configure romm) error = %v", err)
	}
	if got := len(reader.prompts); got != 2 {
		t.Fatalf("password prompts = %d, want 2", got)
	}
	assertNoCredentialMarkers(t, output.String())

	var status bytes.Buffer
	if err := run([]string{"status"}, store, nil, &status); err != nil {
		t.Fatalf("run(status) error = %v", err)
	}
	if got, want := status.String(), "igdb_configured=false\nromm_configured=true\n"; got != want {
		t.Fatalf("status = %q, want %q", got, want)
	}
	assertNoCredentialMarkers(t, status.String())

	fresh, err := config.NewFileProviderConfigStore(filepath.Join(t.TempDir(), "unused-xdg"), t.TempDir())
	if err != nil {
		t.Fatalf("new unrelated test store: %v", err)
	}
	if err := run([]string{"configure", "romm"}, fresh, &queuedSecretReader{values: []string{cliTestRomMURL, cliTestRomMToken}}, &bytes.Buffer{}); err != nil {
		t.Fatalf("configure RomM on fresh store: %v", err)
	}
	loaded, err := fresh.Load()
	if err != nil {
		t.Fatalf("fresh RomM store Load() error = %v", err)
	}
	if got := loaded.RomM; got != (config.RomMCredentials{BaseURL: cliTestRomMURL, ClientAPIToken: cliTestRomMToken}) {
		t.Fatalf("fresh RomM bundle = %#v, want configured synthetic bundle", got)
	}
}

func TestRunRejectsUnknownAndCredentialLikeArgumentsBeforeReading(t *testing.T) {
	for _, args := range [][]string{
		{"credential-marker"},
		{"configure", "igdb", "credential-marker"},
		{"configure", "--client-secret=credential-marker"},
		{"--config", "/tmp/provider-config-marker"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			reader := &queuedSecretReader{values: []string{"unused-secret-marker"}}
			var output bytes.Buffer
			err := run(args, nil, reader, &output)
			if err == nil {
				t.Fatal("run() succeeded for an unsupported argument list")
			}
			if len(reader.prompts) != 0 {
				t.Fatalf("invalid arguments prompted for credentials: %d prompts", len(reader.prompts))
			}
			assertNoCredentialMarkers(t, err.Error(), output.String())
		})
	}
}

func TestRunSanitizesPasswordReaderErrors(t *testing.T) {
	store, _, _ := newCommandTestStore(t)
	reader := &queuedSecretReader{err: errors.New("synthetic-password-error-marker")}
	var output bytes.Buffer
	err := run([]string{"configure", "igdb"}, store, reader, &output)
	if err == nil {
		t.Fatal("run() succeeded after password input failure")
	}
	assertNoCredentialMarkers(t, err.Error(), output.String())
}

func TestRunSanitizesProviderStoreErrors(t *testing.T) {
	store := failingProviderStore{err: errors.New(cliTestStoreError)}
	var output bytes.Buffer
	if err := run([]string{"status"}, store, nil, &output); err == nil {
		t.Fatal("run(status) succeeded after store load failure")
	} else {
		assertNoCredentialMarkers(t, err.Error(), output.String())
	}

	reader := &queuedSecretReader{values: []string{cliTestIGDBID, cliTestIGDBSecret}}
	if err := run([]string{"configure", "igdb"}, store, reader, &output); err == nil {
		t.Fatal("run(configure igdb) succeeded after store save failure")
	} else {
		assertNoCredentialMarkers(t, err.Error(), output.String())
	}
}

func TestHiddenTerminalReaderDoesNotEchoReadValue(t *testing.T) {
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open null input: %v", err)
	}
	defer input.Close()

	value := []byte("hidden-terminal-secret-marker")
	var promptOutput bytes.Buffer
	reader := hiddenTerminalReader{
		input:        input,
		promptOutput: &promptOutput,
		readPassword: func(fd int) ([]byte, error) {
			if fd != int(input.Fd()) {
				t.Fatalf("ReadPassword fd = %d, want %d", fd, input.Fd())
			}
			return value, nil
		},
	}
	got, err := reader.ReadSecret("IGDB client secret: ")
	if err != nil {
		t.Fatalf("ReadSecret() error = %v", err)
	}
	if got != "hidden-terminal-secret-marker" {
		t.Fatal("ReadSecret() did not return the injected value")
	}
	if strings.Contains(promptOutput.String(), got) {
		t.Fatal("hidden terminal input was echoed to the prompt output")
	}
	if got, want := promptOutput.String(), "IGDB client secret: \n"; got != want {
		t.Fatalf("prompt output = %q, want %q", got, want)
	}
	if strings.Trim(string(value), "\x00") != "" {
		t.Fatal("hidden reader did not clear the temporary password bytes")
	}
}

func newCommandTestStore(t *testing.T) (*config.FileProviderConfigStore, string, string) {
	t.Helper()
	xdg := filepath.Join(t.TempDir(), "xdg")
	home := filepath.Join(t.TempDir(), "home")
	store, err := config.NewFileProviderConfigStore(xdg, home)
	if err != nil {
		t.Fatalf("NewFileProviderConfigStore() error = %v", err)
	}
	return store, xdg, home
}

func assertNoCredentialMarkers(t *testing.T, outputs ...string) {
	t.Helper()
	for _, output := range outputs {
		for _, marker := range []string{cliTestIGDBID, cliTestIGDBSecret, cliTestRomMURL, cliTestRomMToken, cliTestStoreError, "hidden-terminal-secret-marker", "synthetic-password-error-marker", "credential-marker"} {
			if strings.Contains(output, marker) {
				t.Fatalf("output contained credential marker %q", marker)
			}
		}
	}
}
