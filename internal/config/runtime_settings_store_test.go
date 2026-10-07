package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLocalAcquisitionSettingsDocumentUsesBooleanAndPrivateRoots(t *testing.T) {
	store, path := newTestSettingsStore(t)
	fixture := `{"version":1,"settings":{"local_acquisition":{"enabled":false,"source_root":"/private/source","staging_root":"/private/staging"}}}`
	writeSettingsFixture(t, path, []byte(fixture), 0600)
	if _, err := store.Load(); err != nil {
		t.Fatalf("local settings document rejected: %v", err)
	}
}

func TestLocalSettingsPersistentPartialUpdatesAndStrictShape(t *testing.T) {
	store, path := newTestSettingsStore(t)
	initial := RuntimeSettings{
		LocalAcquisition: LocalAcquisitionSettings{SourceRoot: "/private/source", StagingRoot: "/private/staging"},
		Metadata:         MetadataSettings{Language: MetadataLanguageSpanish},
	}
	if err := store.Save(initial); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewFileRuntimeSettingsStore(filepath.Dir(filepath.Dir(path)), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Update(func(s *RuntimeSettings) { s.Server.ListenAddress = "127.0.0.1:8181" }); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(s *RuntimeSettings) { s.LocalAcquisition.Enabled = true }); err != nil {
		t.Fatal(err)
	}
	loaded, err := fresh.Load()
	if err != nil || loaded.LocalAcquisition != (LocalAcquisitionSettings{Enabled: true, SourceRoot: "/private/source", StagingRoot: "/private/staging"}) || loaded.Metadata != initial.Metadata || loaded.Server.ListenAddress != "127.0.0.1:8181" {
		t.Fatal("local partial update lost persistent settings")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, staging := range []string{initial.LocalAcquisition.SourceRoot, "/private/assets", "/private/assets/staging"} {
		if err := store.Update(func(s *RuntimeSettings) { s.LocalAcquisition.StagingRoot = staging }); err == nil {
			t.Error("unsafe partial staging update saved")
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("invalid update changed settings")
	}
	for _, raw := range []string{
		`{"version":1,"settings":{"local_acquisition":{"enabled":"true"}}}`,
		`{"version":1,"settings":{"local_acquisition":{"enabled":null}}}`,
		`{"version":1,"settings":{"local_acquisition":{"enabled":true,"enabled":false}}}`,
		`{"version":1,"settings":{"local_acquisition":{"source_root":42}}}`,
		`{"version":1,"settings":{"local_acquisition":{"source_path":"/private"}}}`,
		`{"version":1,"settings":{"local_acquisition":{"enabled":true}}}`,
		`{"version":1,"settings":{"local_acquisition":{"source_root":"/"}}}`,
		`{"version":1,"settings":{"local_acquisition":{"staging_root":"/private/assets"}}}`,
		`{"version":1,"settings":{"local_acquisition":{"enabled":false,"staging_root":"/private/assets/staging"}}}`,
	} {
		writeSettingsFixture(t, path, []byte(raw), 0600)
		if _, err := store.Load(); err == nil {
			t.Fatalf("invalid local document accepted: %s", raw)
		}
	}
}

func TestSettingsConfigPathUsesXDGThenHomeFallback(t *testing.T) {
	xdg := filepath.Join(t.TempDir(), "xdg-config")
	home := filepath.Join(t.TempDir(), "home")

	tests := []struct {
		name string
		xdg  string
		home string
		want string
	}{
		{name: "XDG config home", xdg: xdg, home: home, want: filepath.Join(xdg, "lernae", "settings.json")},
		{name: "home fallback", home: home, want: filepath.Join(home, ".config", "lernae", "settings.json")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := SettingsConfigPath(test.xdg, test.home)
			if err != nil {
				t.Fatalf("SettingsConfigPath() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("SettingsConfigPath() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSettingsConfigPathRequiresAbsoluteRoot(t *testing.T) {
	for _, test := range []struct {
		name string
		xdg  string
		home string
	}{
		{name: "relative XDG root", xdg: "relative/config", home: t.TempDir()},
		{name: "relative fallback home", home: "relative/home"},
		{name: "missing roots"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := SettingsConfigPath(test.xdg, test.home); !errors.Is(err, ErrRuntimeSettingsPath) {
				t.Fatalf("SettingsConfigPath() error = %v, want ErrRuntimeSettingsPath", err)
			}
		})
	}
}

func TestSettingsStoreRoundTripsSparseVersionedSettings(t *testing.T) {
	store, path := newTestSettingsStore(t)
	want := RuntimeSettings{
		Server:    ServerSettings{DatabasePath: "/custom/data.db", ListenAddress: "[::1]:8181"},
		Agent:     AgentSettings{CachePath: "/custom/cache"},
		Inventory: InventorySettings{Source: "romm"},
		Metadata:  MetadataSettings{Language: MetadataLanguageSpanish},
		Wikidata:  WikidataSettings{ContactEmail: "operator@example.com"},
	}
	if err := store.Save(want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	freshStore, err := NewFileRuntimeSettingsStore(filepath.Dir(filepath.Dir(path)), t.TempDir())
	if err != nil {
		t.Fatalf("NewFileRuntimeSettingsStore() fresh instance error = %v", err)
	}
	got, err := freshStore.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want.Version = RuntimeSettingsVersion
	if got != want {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"version": 1`) || !strings.Contains(string(data), `"settings"`) {
		t.Fatalf("settings document is not versioned: %s", data)
	}
	if bytes.Contains(data, []byte("client_secret")) || bytes.Contains(data, []byte("client_api_token")) {
		t.Fatalf("settings document contains provider-secret fields: %s", data)
	}
}

func TestSettingsStoreLoadsVersionOneDocumentWithoutWikidataContact(t *testing.T) {
	store, path := newTestSettingsStore(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{"version":1,"settings":{"server":{"database_path":"/synthetic/server.db"},"metadata":{"language":"es"}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() legacy version-1 document error = %v", err)
	}
	want := RuntimeSettings{
		Version:  RuntimeSettingsVersion,
		Server:   ServerSettings{DatabasePath: "/synthetic/server.db"},
		Metadata: MetadataSettings{Language: MetadataLanguageSpanish},
	}
	if got != want {
		t.Fatalf("Load() legacy version-1 document = %#v, want %#v", got, want)
	}
}

func TestSettingsStoreUpgradesVersionZeroSettingsWhenContactIsAdded(t *testing.T) {
	store, path := newTestSettingsStore(t)
	legacy := `{"version":0,"settings":{"inventory":{"source":"manifest","manifest_path":"/synthetic/manifest.json"},"metadata":{"language":"es"}}}`
	writeSettingsFixture(t, path, []byte(legacy), 0o600)

	want := RuntimeSettings{
		Version:   RuntimeSettingsVersion,
		Inventory: InventorySettings{Source: "manifest", ManifestPath: "/synthetic/manifest.json"},
		Metadata:  MetadataSettings{Language: MetadataLanguageSpanish},
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() legacy version-zero settings error = %v", err)
	}
	if loaded != want {
		t.Fatalf("Load() legacy version-zero settings = %#v, want %#v", loaded, want)
	}
	if err := store.Update(func(settings *RuntimeSettings) {
		settings.Wikidata.ContactEmail = "operator@example.com"
	}); err != nil {
		t.Fatalf("Update() legacy version-zero settings error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"version": 1`) {
		t.Fatalf("updated settings were not written using current version one: %s", data)
	}
	want.Wikidata.ContactEmail = "operator@example.com"
	reopened, err := store.Load()
	if err != nil {
		t.Fatalf("Load() upgraded settings error = %v", err)
	}
	if reopened != want {
		t.Fatalf("reopened upgraded settings = %#v, want %#v", reopened, want)
	}
}

func TestSettingsStoreRoundTripsEveryMetadataLanguageChoice(t *testing.T) {
	for _, language := range []MetadataLanguage{
		MetadataLanguageEnglish,
		MetadataLanguageSpanish,
		MetadataLanguageOriginal,
	} {
		t.Run(string(language), func(t *testing.T) {
			store, _ := newTestSettingsStore(t)
			if err := store.Save(RuntimeSettings{Metadata: MetadataSettings{Language: language}}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			got, err := store.Load()
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if got.Metadata.Language != language {
				t.Fatalf("metadata language = %q, want %q", got.Metadata.Language, language)
			}
		})
	}
}

func TestSettingsStoreKeepsUnsetFieldsOmittedAndAbsentFileEmpty(t *testing.T) {
	store, path := newTestSettingsStore(t)
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() for absent file error = %v", err)
	}
	if got.Version != RuntimeSettingsVersion || got != (RuntimeSettings{Version: RuntimeSettingsVersion}) {
		t.Fatalf("absent file settings = %#v, want empty versioned settings", got)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loading absent settings created a file: %v", err)
	}
	if err := store.Save(RuntimeSettings{}); err != nil {
		t.Fatalf("Save(empty) error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"database_path"`)) || bytes.Contains(data, []byte(`"listen_address"`)) || bytes.Contains(data, []byte(`"cache_path"`)) {
		t.Fatalf("empty/unset fields were serialized as defaults: %s", data)
	}
	if bytes.Contains(data, []byte(`"server"`)) || bytes.Contains(data, []byte(`"agent"`)) || bytes.Contains(data, []byte(`"inventory"`)) {
		t.Fatalf("empty settings sections were serialized: %s", data)
	}
}

func TestSettingsStoreRejectsMalformedUnknownAndUnsupportedDocuments(t *testing.T) {
	tests := []struct {
		name string
		data string
		want error
	}{
		{name: "malformed JSON", data: `{`, want: ErrRuntimeSettingsMalformed},
		{name: "unknown field", data: `{"version":1,"settings":{"server":{"database_path":"/tmp/db","unexpected":true}}}`, want: ErrRuntimeSettingsMalformed},
		{name: "secret field", data: `{"version":1,"settings":{"providers":{"igdb":{"client_secret":"synthetic"}}}}`, want: ErrRuntimeSettingsMalformed},
		{name: "null settings section", data: `{"version":1,"settings":{"server":null}}`, want: ErrRuntimeSettingsMalformed},
		{name: "non-string setting", data: `{"version":1,"settings":{"server":{"database_path":42}}}`, want: ErrRuntimeSettingsMalformed},
		{name: "unsupported version", data: `{"version":99,"settings":{}}`, want: ErrRuntimeSettingsVersion},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, path := newTestSettingsStore(t)
			writeSettingsFixture(t, path, []byte(test.data), 0o600)
			if _, err := store.Load(); !errors.Is(err, test.want) {
				t.Fatalf("Load() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestSettingsStoreRejectsDuplicateJSONKeys(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "duplicate root version", data: `{"version":1,"version":1,"settings":{}}`},
		{name: "duplicate root settings", data: `{"version":1,"settings":{},"settings":{}}`},
		{name: "duplicate settings section", data: `{"version":1,"settings":{"server":{},"server":{}}}`},
		{name: "duplicate setting field", data: `{"version":1,"settings":{"server":{"database_path":"/first","database_path":"/second"}}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, path := newTestSettingsStore(t)
			writeSettingsFixture(t, path, []byte(test.data), 0o600)
			if _, err := store.Load(); !errors.Is(err, ErrRuntimeSettingsMalformed) {
				t.Fatalf("Load() error = %v, want ErrRuntimeSettingsMalformed", err)
			}
		})
	}
}

func TestSettingsStoreRejectsPathsInsideGitRepositories(t *testing.T) {
	repository := t.TempDir()
	if err := os.Mkdir(filepath.Join(repository, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileRuntimeSettingsStore(filepath.Join(repository, "config"), t.TempDir())
	if err == nil {
		err = store.Save(RuntimeSettings{Server: ServerSettings{DatabasePath: "/synthetic/db"}})
	}
	if err == nil || !errors.Is(err, ErrRuntimeSettingsRepository) {
		t.Fatalf("settings store error = %v, want ErrRuntimeSettingsRepository", err)
	}
	if _, err := os.Lstat(filepath.Join(repository, "config", "lernae", "settings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settings store created a file under repository: %v", err)
	}
}

func TestSettingsStoreRejectsSymlinkAndUnsafeFile(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		store, path := newTestSettingsStore(t)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "outside.json")
		want := []byte(`{"version":1,"settings":{}}`)
		if err := os.WriteFile(target, want, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := store.Load(); !errors.Is(err, ErrRuntimeSettingsSymlink) {
			t.Fatalf("Load() error = %v, want ErrRuntimeSettingsSymlink", err)
		}
		if err := store.Save(RuntimeSettings{}); !errors.Is(err, ErrRuntimeSettingsSymlink) {
			t.Fatalf("Save() error = %v, want ErrRuntimeSettingsSymlink", err)
		}
		got, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("symlink target changed: data=%q err=%v", got, err)
		}
	})

	t.Run("unsafe permissions", func(t *testing.T) {
		store, path := newTestSettingsStore(t)
		writeSettingsFixture(t, path, []byte(`{"version":1,"settings":{}}`), 0o644)
		if _, err := store.Load(); !errors.Is(err, ErrRuntimeSettingsPermissions) {
			t.Fatalf("Load() error = %v, want ErrRuntimeSettingsPermissions", err)
		}
	})

	t.Run("unsafe directory permissions", func(t *testing.T) {
		store, path := newTestSettingsStore(t)
		writeSettingsFixture(t, path, []byte(`{"version":1,"settings":{}}`), 0o600)
		if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); !errors.Is(err, ErrRuntimeSettingsPermissions) {
			t.Fatalf("Load() error = %v, want ErrRuntimeSettingsPermissions", err)
		}
	})
}

func TestSettingsStorePreservesExistingFileWhenReplaceFails(t *testing.T) {
	store, path := newTestSettingsStore(t)
	if err := store.Save(RuntimeSettings{Server: ServerSettings{DatabasePath: "/old/db"}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store.rename = func(string, string) error { return errors.New("synthetic rename failure") }
	if err := store.Save(RuntimeSettings{Server: ServerSettings{DatabasePath: "/new/db"}}); !errors.Is(err, ErrRuntimeSettingsWrite) {
		t.Fatalf("Save() error = %v, want ErrRuntimeSettingsWrite", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("failed replacement changed existing settings: before=%s after=%s", before, after)
	}
}

func TestConcurrentSettingsUpdatesPreserveDisjointFieldsAcrossStores(t *testing.T) {
	root := t.TempDir()
	xdg := filepath.Join(root, "xdg-config")
	home := filepath.Join(root, "home")
	first, err := NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewFileRuntimeSettingsStore(xdg, home)
	if err != nil {
		t.Fatal(err)
	}
	const writers = 12
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wait sync.WaitGroup
	for i := 0; i < writers; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			store := first
			if index%2 != 0 {
				store = second
			}
			err := store.Update(func(settings *RuntimeSettings) {
				switch index % 3 {
				case 0:
					settings.Server.DatabasePath = "/synthetic/database"
				case 1:
					settings.Agent.CachePath = "/synthetic/cache"
				case 2:
					settings.Inventory.Source = "manifest"
				}
			})
			errs <- err
		}(i)
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
	}

	got, err := first.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Server.DatabasePath != "/synthetic/database" || got.Agent.CachePath != "/synthetic/cache" || got.Inventory.Source != "manifest" {
		t.Fatalf("concurrent updates lost settings: %#v", got)
	}
}

func newTestSettingsStore(t *testing.T) (*FileRuntimeSettingsStore, string) {
	t.Helper()
	root := t.TempDir()
	store, err := NewFileRuntimeSettingsStore(filepath.Join(root, "xdg-config"), filepath.Join(root, "home"))
	if err != nil {
		t.Fatalf("NewFileRuntimeSettingsStore() error = %v", err)
	}
	return store, filepath.Join(root, "xdg-config", "lernae", "settings.json")
}

func writeSettingsFixture(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
