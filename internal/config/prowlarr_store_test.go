package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProwlarrStoresPreserveUnrelatedBundlesAndSparseSettings(t *testing.T) {
	roots := clearIGDBEnv(t)
	providers, err := NewFileProviderConfigStore(roots.config, roots.home)
	if err != nil {
		t.Fatal(err)
	}
	igdb := IGDBCredentials{ClientID: "test-id", ClientSecret: "test-secret"}
	romm := RomMCredentials{BaseURL: "https://romm.invalid", ClientAPIToken: "test-token"}
	if err := providers.SaveIGDB(igdb); err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveRomM(romm); err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveProwlarr(ProwlarrCredentials{APIKey: "private-prowlarr-key"}); err != nil {
		t.Fatal(err)
	}
	if err := providers.ClearProwlarr(); err != nil {
		t.Fatal(err)
	}
	loaded, err := providers.Load()
	if err != nil || loaded.IGDB != igdb || loaded.RomM != romm || loaded.Prowlarr.HasCredentials() {
		t.Fatal("clearing Prowlarr changed unrelated credentials")
	}
	before, err := os.ReadFile(providers.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", " ", "bad\nkey", strings.Repeat("x", 513)} {
		if err := providers.SaveProwlarr(ProwlarrCredentials{APIKey: key}); err != ErrProviderCredentialsIncomplete {
			t.Fatal("invalid secret bundle saved")
		}
	}
	after, _ := os.ReadFile(providers.path)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid secret write changed durable state")
	}
	for _, path := range []string{providers.path, filepath.Dir(providers.path)} {
		info, err := os.Stat(path)
		want := os.FileMode(0600)
		if path == filepath.Dir(providers.path) {
			want = 0700
		}
		if err != nil || info.Mode().Perm() != want {
			t.Fatal("credential permissions unsafe")
		}
	}
	settings, err := NewFileRuntimeSettingsStore(roots.config, roots.home)
	if err != nil {
		t.Fatal(err)
	}
	baseline := RuntimeSettings{
		Metadata:         MetadataSettings{Language: MetadataLanguageSpanish},
		Wikidata:         WikidataSettings{ContactEmail: "operator@example.invalid"},
		LocalAcquisition: LocalAcquisitionSettings{SourceRoot: "/private/source"},
		Inventory:        InventorySettings{Source: InventorySourceManifest},
	}
	if err := settings.Save(baseline); err != nil {
		t.Fatal(err)
	}
	if err := settings.Update(func(s *RuntimeSettings) {
		s.Prowlarr = ProwlarrSettings{Enabled: true, BaseURL: "https://PRIVATE.EXAMPLE.INVALID:443/base/"}
	}); err != nil {
		t.Fatal(err)
	}
	persisted, err := settings.Load()
	if err != nil || persisted.Prowlarr.BaseURL != "https://private.example.invalid/base" || persisted.Metadata != baseline.Metadata || persisted.Wikidata != baseline.Wikidata || persisted.LocalAcquisition != baseline.LocalAcquisition || persisted.Inventory != baseline.Inventory {
		t.Fatal("sparse settings write or canonicalization failed")
	}
	before, _ = os.ReadFile(settings.path)
	if err := settings.Update(func(s *RuntimeSettings) { s.Prowlarr.BaseURL = "http://user:password@host" }); err == nil {
		t.Fatal("unsafe sparse endpoint update accepted")
	}
	after, _ = os.ReadFile(settings.path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed endpoint update replaced safe settings")
	}
	// Disabled providers do not consume secrets and an enabled keyless provider
	// remains unconfigured rather than creating a startup network dependency.
	server, err := LoadServer()
	if err != nil || server.ProwlarrCredentials.HasCredentials() {
		t.Fatal("enabled keyless settings prevented normal startup")
	}
}

func TestProwlarrRuntimeShapeAndCredentialSchemaAreDefensive(t *testing.T) {
	for _, payload := range []string{
		`{"enabled":null}`, `{"enabled":"true"}`, `{"enabled":1}`,
		`{"base_url":null}`, `{"base_url":1}`, `{"base_url":{}}`,
		`{"enabled":false,"enabled":true}`, `{"enabled":true}`,
		`{"base_url":"https://host?key=secret"}`, `{"api_key":"secret"}`,
	} {
		_, err := decodeRuntimeSettings([]byte(`{"version":1,"settings":{"prowlarr":` + payload + `}}`))
		if err == nil {
			t.Fatal("unsafe or malformed Prowlarr settings accepted")
		}
	}
	for _, payload := range []string{
		`null`, `{}`, `{"api_key":null}`, `{"api_key":1}`,
		`{"api_key":"a","api_key":"b"}`, `{"api_key":"bad\nkey"}`,
	} {
		_, err := decodeProviderConfig([]byte(`{"version":1,"providers":{"prowlarr":` + payload + `}}`))
		if err == nil {
			t.Fatal("malformed secret bundle accepted")
		}
	}
}
