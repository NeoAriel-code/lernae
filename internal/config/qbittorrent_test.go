package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestQBittorrentValidationAndServerLoadingWithoutHTTP(t *testing.T) {
	for _, settings := range []QBittorrentSettings{
		{Enabled: true},
		{Enabled: true, BaseURL: "http://127.0.0.1:1"},
		{BaseURL: "http://user:secret@host"},
		{BaseURL: "https://host?key=secret"},
		{BaseURL: "http://host/a/../b"},
		{Username: "bad\nuser"},
	} {
		if ValidateQBittorrentSettings(settings) != ErrInvalidRuntimeSettings {
			t.Fatal("unsafe configuration accepted")
		}
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", root)
	settings, err := NewFileRuntimeSettingsStore(root, root)
	if err != nil {
		t.Fatal(err)
	}
	providers, err := NewFileProviderConfigStore(root, root)
	if err != nil {
		t.Fatal(err)
	}
	// No server exists at this fixture endpoint; all loads must remain local.
	if err := settings.Update(func(s *RuntimeSettings) {
		s.QBittorrent = QBittorrentSettings{Enabled: true, BaseURL: "HTTP://127.0.0.1:1/base/", Username: "operator"}
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadServer()
	if err != nil || loaded.QBittorrent.BaseURL != "http://127.0.0.1:1/base" || loaded.QBittorrentCredentials.HasCredentials() {
		t.Fatal("unconfigured qBit prevented normal local configuration load")
	}
	if err := providers.SaveQBittorrent(QBittorrentCredentials{Password: "PRIVATE_PASSWORD"}); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadServer()
	if err != nil || !loaded.QBittorrentCredentials.HasCredentials() {
		t.Fatal("persistent qBit password not loaded")
	}
	public, _ := json.Marshal(loaded)
	for _, private := range []string{"PRIVATE_PASSWORD", "operator", "http://127.0.0.1:1/base"} {
		if strings.Contains(string(public)+fmt.Sprintf("%#v", loaded), private) {
			t.Fatal("Server configuration leaked qBit values")
		}
	}
	if err := settings.Update(func(s *RuntimeSettings) { s.QBittorrent.Enabled = false }); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadServer()
	if err != nil || loaded.QBittorrentCredentials.HasCredentials() {
		t.Fatal("disabled qBit consumed password")
	}
}

func TestQBittorrentStorePreservesOtherBundlesAndRejectsMalformed(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileProviderConfigStore(root, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProwlarr(ProwlarrCredentials{APIKey: "OTHER_PROVIDER_KEY"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveQBittorrent(QBittorrentCredentials{Password: "PRIVATE_PASSWORD"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil || loaded.Prowlarr.APIKey != "OTHER_PROVIDER_KEY" || loaded.QBittorrent.Password != "PRIVATE_PASSWORD" {
		t.Fatal("credential replacement changed unrelated bundle")
	}
	if err := store.ClearQBittorrent(); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.Load()
	if err != nil || loaded.QBittorrent.HasCredentials() || !loaded.Prowlarr.HasCredentials() {
		t.Fatal("credential clear changed unrelated bundle")
	}
	for _, raw := range []string{`{}`, `{"password":null}`, `{"password":""}`, `{"password":"x","password":"y"}`} {
		if _, err := decodeProviderConfig([]byte(`{"version":1,"providers":{"qbittorrent":` + raw + `}}`)); err == nil {
			t.Fatal("malformed password schema accepted")
		}
	}
	for _, raw := range []string{`{"enabled":null}`, `{"enabled":"true"}`, `{"password":"secret"}`, `{"enabled":true}`, `{"enabled":false,"username":null}`, `{"enabled":false,"base_url":"http://user:secret@host"}`} {
		if _, err := decodeRuntimeSettings([]byte(`{"version":1,"settings":{"qbittorrent":` + raw + `}}`)); err == nil {
			t.Fatal("malformed settings schema accepted")
		}
	}
}
