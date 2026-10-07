package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"lernae/internal/config"
	"lernae/internal/providers/openlibrary"
)

type metadataSettingsTestLimiter struct{}

func (metadataSettingsTestLimiter) Wait(context.Context) error { return nil }

func TestRuntimeMetadataLanguageSettingUpdatesProviderAndSurvivesRestart(t *testing.T) {
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/search.json" || request.URL.Query().Has("lang") {
			t.Errorf("provider request = %s?%s, want unfiltered Search", request.URL.Path, request.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"docs":[{"key":"/works/OL123W","title":"El ingenioso hidalgo Don Quijote","editions":{"docs":[{"key":"/books/OL321M","title":"Don Quijote","language":["eng"]},{"key":"/books/OL322M","title":"Don Quijote","language":["spa"]}]}}]}`)
	}))
	defer providerServer.Close()

	root := t.TempDir()
	store, err := config.NewFileRuntimeSettingsStore(filepath.Join(root, "config"), filepath.Join(root, "home"))
	if err != nil {
		t.Fatal(err)
	}
	originalSettings := config.RuntimeSettings{Server: config.ServerSettings{DatabasePath: filepath.Join(root, "database.db")}}
	if err := store.Save(originalSettings); err != nil {
		t.Fatalf("Save unrelated runtime settings: %v", err)
	}
	provider := openlibrary.New(openlibrary.ClientConfig{Endpoint: providerServer.URL, HTTPClient: providerServer.Client(), Limiter: metadataSettingsTestLimiter{}})
	settings := &runtimeMetadataLanguageSettings{store: store, provider: provider}
	if language, err := settings.GetMetadataLanguage(); err != nil || language != config.MetadataLanguageEnglish {
		t.Fatalf("default metadata language = %q, %v; want en", language, err)
	}
	if err := settings.SetMetadataLanguage(config.MetadataLanguageSpanish); err != nil {
		t.Fatalf("SetMetadataLanguage() error = %v", err)
	}
	results, err := provider.Search(context.Background(), "Don Quijote", 1)
	if err != nil {
		t.Fatalf("Search() after preference update: %v", err)
	}
	if len(results) != 1 || results[0].RepresentativeEdition == nil || results[0].RepresentativeEdition.ExternalID != "OL322M" {
		t.Fatalf("updated provider preference selected %#v, want Spanish edition OL322M", results)
	}

	persisted, err := store.Load()
	if err != nil || persisted.Metadata.Language != config.MetadataLanguageSpanish || persisted.Server != originalSettings.Server {
		t.Fatalf("persisted language = %q, %v; want es", persisted.Metadata.Language, err)
	}
	restartedProvider := openlibrary.New(openlibrary.ClientConfig{
		Endpoint: providerServer.URL, HTTPClient: providerServer.Client(), Limiter: metadataSettingsTestLimiter{},
		MetadataLanguage: string(config.EffectiveMetadataLanguage(persisted.Metadata.Language)),
	})
	restartedSettings := &runtimeMetadataLanguageSettings{store: store, provider: restartedProvider}
	if language, err := restartedSettings.GetMetadataLanguage(); err != nil || language != config.MetadataLanguageSpanish {
		t.Fatalf("restarted metadata language = %q, %v; want es", language, err)
	}
	restartedResults, err := restartedProvider.Search(context.Background(), "Don Quijote", 1)
	if err != nil || len(restartedResults) != 1 || restartedResults[0].RepresentativeEdition == nil || restartedResults[0].RepresentativeEdition.ExternalID != "OL322M" {
		t.Fatalf("restarted provider results = %#v, %v; want Spanish edition OL322M", restartedResults, err)
	}
}
