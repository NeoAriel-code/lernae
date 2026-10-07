package config

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestProwlarrPersistentSettingsCredentialsAndDiagnosticPrivacy(t *testing.T) {
	roots := clearIGDBEnv(t)
	before, err := LoadServer()
	if err != nil || before.Prowlarr.Enabled {
		t.Fatal("Prowlarr must default disabled")
	}
	providers, err := NewFileProviderConfigStore(roots.config, roots.home)
	if err != nil {
		t.Fatal(err)
	}
	key := "PRIVATE_PROWLARR_APIKEY_12345"
	if err := providers.SaveProwlarr(ProwlarrCredentials{APIKey: key}); err != nil {
		t.Fatal(err)
	}
	settings, err := NewFileRuntimeSettingsStore(roots.config, roots.home)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "https://private-prowlarr.example.invalid/base"
	if err := settings.Update(func(s *RuntimeSettings) { s.Prowlarr = ProwlarrSettings{Enabled: true, BaseURL: endpoint} }); err != nil {
		t.Fatal(err)
	}
	after, err := LoadServer()
	if err != nil || !after.Prowlarr.Enabled || after.Prowlarr.BaseURL != endpoint || after.ProwlarrCredentials.APIKey != key || string(after.ProwlarrReferenceDirectory) != filepath.Join(roots.config, "lernae", "acquisition", "prowlarr") {
		t.Fatalf("persistent Prowlarr reload failed: %v", err)
	}
	loaded, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{after, after.Prowlarr, after.ProwlarrCredentials, loaded, RuntimeSettings{Prowlarr: after.Prowlarr}} {
		data, _ := json.Marshal(v)
		for _, text := range []string{string(data), fmt.Sprintf("%v", v), fmt.Sprintf("%+v", v), fmt.Sprintf("%#v", v)} {
			if strings.Contains(text, key) || strings.Contains(text, endpoint) || strings.Contains(text, string(after.ProwlarrReferenceDirectory)) {
				t.Fatal("diagnostics leaked Prowlarr config")
			}
		}
	}
}
