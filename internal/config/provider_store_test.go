package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testProviderClientID     = "synthetic-client-id"
	testProviderClientSecret = "synthetic-client-secret"
	testProviderRomMURL      = "https://romm.example.test"
	testProviderRomMToken    = "synthetic-romm-token"
)

func TestProviderConfigPathUsesXDGThenHomeFallback(t *testing.T) {
	xdg := filepath.Join(t.TempDir(), "xdg-config")
	home := filepath.Join(t.TempDir(), "home")

	tests := []struct {
		name string
		xdg  string
		home string
		want string
	}{
		{name: "XDG config home", xdg: xdg, home: home, want: filepath.Join(xdg, "lernae", "providers.json")},
		{name: "home fallback", home: home, want: filepath.Join(home, ".config", "lernae", "providers.json")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ProviderConfigPath(test.xdg, test.home)
			if err != nil {
				t.Fatalf("ProviderConfigPath() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("ProviderConfigPath() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestProviderConfigPathRequiresAbsoluteRoot(t *testing.T) {
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
			if _, err := ProviderConfigPath(test.xdg, test.home); !errors.Is(err, ErrProviderConfigPath) {
				t.Fatalf("ProviderConfigPath() error = %v, want ErrProviderConfigPath", err)
			}
		})
	}
}

func TestProviderConfigStoreRejectsXDGRootInsideGitRepository(t *testing.T) {
	repository := t.TempDir()
	if err := os.Mkdir(filepath.Join(repository, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	xdg := filepath.Join(repository, "xdg-config")
	store, err := NewFileProviderConfigStore(xdg, filepath.Join(t.TempDir(), "home"))
	if err == nil {
		err = store.SaveIGDB(testIGDBCredentials())
	}
	if err == nil || !errors.Is(err, ErrProviderConfigRepository) || err.Error() != "provider config path must be outside a Git repository" {
		t.Errorf("provider config creation error = %v, want fixed repository-path error", err)
	}
	for _, path := range []string{
		filepath.Join(repository, "xdg-config", "lernae"),
		filepath.Join(repository, "xdg-config", "lernae", "providers.json"),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("repository path %q exists or cannot be checked: %v", path, err)
		}
	}
}

func TestProviderConfigStoreRejectsSymlinkedXDGRootIntoGitRepository(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".git"), []byte("gitdir: synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	xdg := filepath.Join(root, "linked-xdg")
	if err := os.Symlink(repository, xdg); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	store, err := NewFileProviderConfigStore(xdg, filepath.Join(root, "home"))
	if err == nil {
		err = store.SaveRomM(testRomMCredentials())
	}
	if err == nil || !errors.Is(err, ErrProviderConfigRepository) || err.Error() != "provider config path must be outside a Git repository" {
		t.Errorf("provider config creation error = %v, want fixed repository-path error", err)
	}
	for _, path := range []string{
		filepath.Join(repository, "lernae"),
		filepath.Join(repository, "lernae", "providers.json"),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("repository path %q exists or cannot be checked: %v", path, err)
		}
	}
}

func TestProviderConfigStoreCreatesPrivateParentAndFile(t *testing.T) {
	store, path := newTestProviderStore(t)
	if err := store.SaveIGDB(testIGDBCredentials()); err != nil {
		t.Fatalf("SaveIGDB() error = %v", err)
	}

	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat provider config parent: %v", err)
	}
	if got := parent.Mode().Perm(); got != 0o700 {
		t.Fatalf("provider config parent permissions = %04o, want 0700", got)
	}
	file, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat provider config file: %v", err)
	}
	if !file.Mode().IsRegular() {
		t.Fatalf("provider config file mode = %v, want regular file", file.Mode())
	}
	if got := file.Mode().Perm(); got != 0o600 {
		t.Fatalf("provider config file permissions = %04o, want 0600", got)
	}
}

func TestProviderConfigStoreLoadsAbsentFileAsEmptyVersionOne(t *testing.T) {
	store, _ := newTestProviderStore(t)
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Version != ProviderConfigVersion || got.IGDB.HasCredentials() || got.RomM.HasCredentials() {
		t.Fatalf("Load() returned unexpected empty config: %#v", got)
	}
}

func TestProviderConfigStoreRoundTripsAndPreservesOtherProvider(t *testing.T) {
	store, _ := newTestProviderStore(t)
	igdb := testIGDBCredentials()
	romm := testRomMCredentials()

	if err := store.SaveIGDB(igdb); err != nil {
		t.Fatalf("SaveIGDB() error = %v", err)
	}
	if err := store.SaveRomM(romm); err != nil {
		t.Fatalf("SaveRomM() error = %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.IGDB != igdb || got.RomM != romm {
		t.Fatalf("Load() did not return both complete provider bundles: %#v", got)
	}

	updatedIGDB := IGDBCredentials{ClientID: "updated-client-id", ClientSecret: "updated-client-secret"}
	if err := store.SaveIGDB(updatedIGDB); err != nil {
		t.Fatalf("update SaveIGDB() error = %v", err)
	}
	got, err = store.Load()
	if err != nil {
		t.Fatalf("Load() after update error = %v", err)
	}
	if got.IGDB != updatedIGDB || got.RomM != romm {
		t.Fatal("updating IGDB did not preserve the RomM bundle")
	}
}

func TestProviderConfigStoreRejectsIncompleteBundlesWithoutWriting(t *testing.T) {
	tests := []struct {
		name string
		save func(*FileProviderConfigStore) error
	}{
		{name: "IGDB", save: func(store *FileProviderConfigStore) error {
			return store.SaveIGDB(IGDBCredentials{ClientID: testProviderClientID})
		}},
		{name: "RomM", save: func(store *FileProviderConfigStore) error {
			return store.SaveRomM(RomMCredentials{BaseURL: testProviderRomMURL})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, path := newTestProviderStore(t)
			if err := test.save(store); !errors.Is(err, ErrProviderCredentialsIncomplete) {
				t.Fatalf("save incomplete credentials error = %v, want ErrProviderCredentialsIncomplete", err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("incomplete credentials created a file: stat error = %v", err)
			}
		})
	}
}

func TestProviderConfigStoreRejectsSymlinkAtFinalPath(t *testing.T) {
	store, path := newTestProviderStore(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.json")
	want := []byte(`{"version":1,"providers":{"igdb":{"client_id":"outside-id","client_secret":"outside-secret"}}}`)
	if err := os.WriteFile(target, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := store.Load(); !errors.Is(err, ErrProviderConfigSymlink) {
		t.Fatalf("Load() error = %v, want ErrProviderConfigSymlink", err)
	}
	if err := store.SaveRomM(testRomMCredentials()); !errors.Is(err, ErrProviderConfigSymlink) {
		t.Fatalf("SaveRomM() error = %v, want ErrProviderConfigSymlink", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("symlink target was modified")
	}
}

func TestProviderConfigStoreRejectsNonRegularFinalPath(t *testing.T) {
	store, path := newTestProviderStore(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrProviderConfigNotRegular) {
		t.Fatalf("Load() error = %v, want ErrProviderConfigNotRegular", err)
	}
}

func TestProviderConfigStoreRejectsUnsafeFileModes(t *testing.T) {
	tests := []struct {
		name string
		mode os.FileMode
	}{
		{name: "group read", mode: 0o640},
		{name: "group write", mode: 0o620},
		{name: "world read", mode: 0o604},
		{name: "world write", mode: 0o602},
		{name: "owner write missing", mode: 0o400},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, path := newTestProviderStore(t)
			writeProviderConfigFixture(t, path, validIGDBDocument(), test.mode)
			if _, err := store.Load(); !errors.Is(err, ErrProviderConfigPermissions) {
				t.Fatalf("Load() error = %v, want ErrProviderConfigPermissions", err)
			}
		})
	}
}

func TestProviderConfigStoreRejectsUnsafeParentMode(t *testing.T) {
	store, path := newTestProviderStore(t)
	writeProviderConfigFixture(t, path, validIGDBDocument(), 0o600)
	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrProviderConfigPermissions) {
		t.Fatalf("Load() error = %v, want ErrProviderConfigPermissions", err)
	}
}

func TestEnsurePrivateProviderConfigDirectoryRejectsExistingUnsafeModeWithoutChangingIt(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "lernae")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateProviderConfigDirectory(directory); !errors.Is(err, ErrProviderConfigPermissions) {
		t.Fatalf("ensurePrivateProviderConfigDirectory() error = %v, want ErrProviderConfigPermissions", err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("existing directory permissions changed to %04o, want unchanged 0755", got)
	}
}

func TestConcurrentProviderConfigSavesPreserveBothBundlesAcrossInstances(t *testing.T) {
	const writers = 16
	root := t.TempDir()
	xdg := filepath.Join(root, "xdg-config")
	home := filepath.Join(root, "home")
	stores := make([]*FileProviderConfigStore, writers)
	for i := range stores {
		store, err := NewFileProviderConfigStore(xdg, home)
		if err != nil {
			t.Fatalf("create store %d: %v", i, err)
		}
		stores[i] = store
	}

	// Hold every replacement until each independent store has loaded its
	// snapshot. Without same-path serialization all writers then replace from
	// stale snapshots, deterministically dropping at least one provider.
	arrivals := atomic.Int32{}
	release := make(chan struct{})
	var releaseOnce sync.Once
	for _, store := range stores {
		store.rename = func(oldPath, newPath string) error {
			if arrivals.Add(1) == writers {
				releaseOnce.Do(func() { close(release) })
			}
			select {
			case <-release:
			case <-time.After(time.Second):
				releaseOnce.Do(func() { close(release) })
			}
			return os.Rename(oldPath, newPath)
		}
	}

	start := make(chan struct{})
	errs := make(chan error, writers)
	var done sync.WaitGroup
	for i, store := range stores {
		i, store := i, store
		done.Add(1)
		go func() {
			defer done.Done()
			<-start
			if i%2 == 0 {
				errs <- store.SaveIGDB(IGDBCredentials{
					ClientID:     "synthetic-client-id-" + string(rune('a'+i)),
					ClientSecret: "synthetic-client-secret-" + string(rune('a'+i)),
				})
				return
			}
			errs <- store.SaveRomM(RomMCredentials{
				BaseURL:        testProviderRomMURL,
				ClientAPIToken: "synthetic-romm-token-" + string(rune('a'+i)),
			})
		}()
	}
	close(start)
	done.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent provider save failed: %v", err)
		}
	}

	loaded, err := stores[0].Load()
	if err != nil {
		t.Fatalf("Load() after concurrent saves: %v", err)
	}
	if !loaded.IGDB.HasCredentials() || !loaded.RomM.HasCredentials() {
		t.Fatal("concurrent IGDB and RomM saves did not preserve both provider bundles")
	}
}

func TestProviderConfigStoreRejectsMalformedAndUnsupportedSchemaWithoutValues(t *testing.T) {
	secret := "synthetic-schema-secret"
	tests := []struct {
		name string
		data string
		want error
	}{
		{name: "malformed JSON", data: `{"version":1,"providers":{"igdb":{"client_secret":"` + secret, want: ErrProviderConfigMalformed},
		{name: "top-level array", data: `[]`, want: ErrProviderConfigMalformed},
		{name: "missing providers", data: `{"version":1}`, want: ErrProviderConfigMalformed},
		{name: "providers is not an object", data: `{"version":1,"providers":[]}`, want: ErrProviderConfigMalformed},
		{name: "unsupported version", data: `{"version":99,"providers":{"igdb":{"client_id":"` + secret + `","client_secret":"` + secret + `"}}}`, want: ErrProviderConfigVersion},
		{name: "incomplete IGDB schema", data: `{"version":1,"providers":{"igdb":{"client_id":"` + secret + `"}}}`, want: ErrProviderConfigMalformed},
		{name: "non-string RomM field", data: `{"version":1,"providers":{"romm":{"base_url":23,"client_api_token":"` + secret + `"}}}`, want: ErrProviderConfigMalformed},
		{name: "provider is not an object", data: `{"version":1,"providers":{"igdb":null}}`, want: ErrProviderConfigMalformed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, path := newTestProviderStore(t)
			writeProviderConfigFixture(t, path, []byte(test.data), 0o600)
			_, err := store.Load()
			if !errors.Is(err, test.want) {
				t.Fatalf("Load() error = %v, want %v", err, test.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("schema error exposed credential data")
			}
		})
	}
}

func TestProviderConfigStoreIgnoresUnknownProviderFieldsWithoutExposingThem(t *testing.T) {
	store, path := newTestProviderStore(t)
	unknownSecret := "synthetic-unknown-provider-secret"
	document := []byte(`{"version":1,"providers":{"igdb":{"client_id":"` + testProviderClientID + `","client_secret":"` + testProviderClientSecret + `","future_field":"` + unknownSecret + `"},"future_provider":{"token":"` + unknownSecret + `"}}}`)
	writeProviderConfigFixture(t, path, document, 0o600)

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	for _, rendered := range []string{fmt.Sprintf("%v", loaded), fmt.Sprintf("%+v", loaded), fmt.Sprintf("%#v", loaded)} {
		if strings.Contains(rendered, unknownSecret) || strings.Contains(rendered, testProviderClientSecret) {
			t.Fatal("provider config formatting exposed credential data")
		}
	}
	encoded, err := json.Marshal(loaded)
	if err != nil {
		t.Fatalf("marshal provider config: %v", err)
	}
	if strings.Contains(string(encoded), unknownSecret) || strings.Contains(string(encoded), testProviderClientSecret) {
		t.Fatal("provider config JSON exposed credential data")
	}

	if err := store.SaveRomM(testRomMCredentials()); err != nil {
		t.Fatalf("SaveRomM() error = %v", err)
	}
	rewritten, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rewritten), unknownSecret) {
		t.Fatal("rewriting config preserved an unknown provider/field value")
	}
	updated, err := store.Load()
	if err != nil {
		t.Fatalf("Load() after rewrite error = %v", err)
	}
	if updated.IGDB.ClientID != testProviderClientID || updated.IGDB.ClientSecret != testProviderClientSecret || updated.RomM != testRomMCredentials() {
		t.Fatal("known provider bundles were not preserved while dropping unknown fields")
	}
}

func TestProviderConfigStoreReplacementFailurePreservesPreviousFileAndCleansOwnTemp(t *testing.T) {
	store, path := newTestProviderStore(t)
	if err := store.SaveIGDB(testIGDBCredentials()); err != nil {
		t.Fatalf("initial SaveIGDB() error = %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store.rename = func(string, string) error { return errors.New("synthetic replacement failure") }

	if err := store.SaveRomM(testRomMCredentials()); err == nil {
		t.Fatal("SaveRomM() succeeded despite injected replacement failure")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("failed replacement changed the previous valid configuration")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != ".providers.lock" || entries[1].Name() != filepath.Base(path) {
		t.Fatalf("temporary files were not cleaned up: %v", entries)
	}
	loaded, err := store.Load()
	if err != nil || loaded.IGDB != testIGDBCredentials() || loaded.RomM.HasCredentials() {
		t.Fatalf("previous valid config after failed replacement = %#v, error = %v", loaded, err)
	}
}

func TestProviderConfigStoreErrorsNeverExposeCredentialValues(t *testing.T) {
	store, path := newTestProviderStore(t)
	writeProviderConfigFixture(t, path, []byte(`{"version":3,"providers":{"igdb":{"client_secret":"`+testProviderClientSecret+`"}}}`), 0o600)
	_, err := store.Load()
	if err == nil {
		t.Fatal("Load() accepted unsupported schema")
	}
	for _, secret := range []string{testProviderClientID, testProviderClientSecret, testProviderRomMToken} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("provider config error exposed credential value %q", secret)
		}
	}
}

func newTestProviderStore(t *testing.T) (*FileProviderConfigStore, string) {
	t.Helper()
	root := t.TempDir()
	xdg := filepath.Join(root, "xdg-config")
	home := filepath.Join(root, "home")
	store, err := NewFileProviderConfigStore(xdg, home)
	if err != nil {
		t.Fatalf("NewFileProviderConfigStore() error = %v", err)
	}
	return store, filepath.Join(xdg, "lernae", "providers.json")
}

func testIGDBCredentials() IGDBCredentials {
	return IGDBCredentials{ClientID: testProviderClientID, ClientSecret: testProviderClientSecret}
}

func testRomMCredentials() RomMCredentials {
	return RomMCredentials{BaseURL: testProviderRomMURL, ClientAPIToken: testProviderRomMToken}
}

func validIGDBDocument() []byte {
	return []byte(`{"version":1,"providers":{"igdb":{"client_id":"` + testProviderClientID + `","client_secret":"` + testProviderClientSecret + `"}}}`)
}

func writeProviderConfigFixture(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create fixture parent: %v", err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod config fixture: %v", err)
	}
}
