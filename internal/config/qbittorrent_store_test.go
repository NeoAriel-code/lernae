package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestQBittorrentStoredSchemaAcceptedPrivately(t *testing.T) {
	data := []byte(`{"version":1,"settings":{"qbittorrent":{"enabled":true,"base_url":"http://127.0.0.1:8080","username":"private-operator"}}}`)
	settings, err := decodeRuntimeSettings(data)
	if err != nil {
		t.Fatal("valid qBittorrent runtime schema not supported")
	}
	public, _ := json.Marshal(settings)
	if strings.Contains(string(public), "private-operator") || strings.Contains(fmt.Sprintf("%#v", settings), "private-operator") {
		t.Fatal("qBittorrent runtime settings leaked")
	}
}

func TestQBittorrentSeparatePrivateStoreRoundtrip(t *testing.T) {
	root := t.TempDir()
	providers, err := NewFileProviderConfigStore(root, root)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := NewFileRuntimeSettingsStore(root, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveProwlarr(ProwlarrCredentials{APIKey: "OTHER_PROVIDER_KEY"}); err != nil {
		t.Fatal(err)
	}
	// Exercise allowlisted private JSON without requiring the future exported API.
	bundle, err := decodeProviderConfig([]byte(`{"version":1,"providers":{"qbittorrent":{"password":"PRIVATE_PASSWORD"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeProviderConfig(bundle)
	if err != nil || !strings.Contains(string(encoded), "PRIVATE_PASSWORD") {
		t.Fatal("qBittorrent password bundle dropped")
	}
	if err := providers.update(func(c *ProviderConfig) { *c = bundle }); err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(RuntimeSettings{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(settings.path)
	if err != nil || strings.Contains(string(data), "PRIVATE_PASSWORD") {
		t.Fatal("password entered runtime settings")
	}
	loaded, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	public, _ := json.Marshal(loaded)
	if strings.Contains(string(public)+fmt.Sprintf("%#v", loaded), "PRIVATE_PASSWORD") {
		t.Fatal("private password leaked")
	}
}
