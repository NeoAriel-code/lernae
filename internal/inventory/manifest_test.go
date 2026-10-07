package inventory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lernae/internal/domain"
	"lernae/internal/providers"
)

var _ providers.StorageSource = (*ManifestInventorySource)(nil)

func TestManifestInventoryExampleValidatesAndReturnsTypedEntries(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != 1 || len(manifest.Assets) != 1 {
		t.Fatalf("manifest = %#v", manifest)
	}
	source := NewManifestInventorySource(filepath.Join("..", "..", "examples", "manifest.json"))
	entries, err := source.Entries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	entry := entries[0]
	if entry.AssetID != "soulcalibur2-gamecube" || entry.WorkTitle != "Soulcalibur II" || entry.Platform != "gamecube" || entry.Format != "disc_image" {
		t.Fatalf("entry = %#v", entry)
	}
	if entry.TotalSizeBytes != 1450000000 || len(entry.Parts) != 1 || len(entry.Locations) != 1 {
		t.Fatalf("entry details = %#v", entry)
	}

	assets, err := source.AssetsForEdition(context.Background(), EditionKey("gamecube", "disc_image"))
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || assets[0].ID != domain.AssetID("soulcalibur2-gamecube") || assets[0].TotalSizeBytes != 1450000000 {
		t.Fatalf("assets = %#v", assets)
	}
	assets, err = source.AssetsForEdition(context.Background(), EditionKey("playstation2", "disc_image"))
	if err != nil || len(assets) != 0 {
		t.Fatalf("unmatched edition assets=%#v err=%v", assets, err)
	}
}

func TestManifestFindsAssetsByExactWorkIdentityAndEditionAttributes(t *testing.T) {
	baseQuery := providers.InventoryQuery{
		WorkIdentity: providers.ExternalWorkIdentity{Provider: "test-catalog", ExternalID: "record-42"},
		Platform:     "platform-x",
		Format:       "disc-image",
	}
	tests := []struct {
		name  string
		title string
		query providers.InventoryQuery
		want  bool
	}{
		{name: "exact identity and edition match", title: "Synthetic Display Title", query: baseQuery, want: true},
		{name: "same title with a different external id does not match", title: "Synthetic Display Title", query: providers.InventoryQuery{
			WorkIdentity: providers.ExternalWorkIdentity{Provider: "test-catalog", ExternalID: "record-43"},
			Platform:     "platform-x", Format: "disc-image",
		}},
		{name: "different provider identity does not match", title: "Synthetic Display Title", query: providers.InventoryQuery{
			WorkIdentity: providers.ExternalWorkIdentity{Provider: "TEST-CATALOG", ExternalID: "record-42"},
			Platform:     "platform-x", Format: "disc-image",
		}},
		{name: "different platform does not match", title: "Synthetic Display Title", query: providers.InventoryQuery{
			WorkIdentity: baseQuery.WorkIdentity, Platform: "platform-y", Format: "disc-image",
		}},
		{name: "different format does not match", title: "Synthetic Display Title", query: providers.InventoryQuery{
			WorkIdentity: baseQuery.WorkIdentity, Platform: "platform-x", Format: "archive-image",
		}},
		{name: "missing provider identity does not match", title: "Synthetic Display Title", query: providers.InventoryQuery{
			WorkIdentity: providers.ExternalWorkIdentity{ExternalID: "record-42"},
			Platform:     "platform-x", Format: "disc-image",
		}},
		{name: "missing external id does not match", title: "Synthetic Display Title", query: providers.InventoryQuery{
			WorkIdentity: providers.ExternalWorkIdentity{Provider: "test-catalog"},
			Platform:     "platform-x", Format: "disc-image",
		}},
		{name: "title case does not affect an exact identity match", title: "SYNTHETIC DISPLAY TITLE", query: baseQuery, want: true},
		{name: "title punctuation does not affect an exact identity match", title: "Synthetic: Display Title!", query: baseQuery, want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := syntheticMatchSource(t, test.title)
			matches, err := source.FindMatchingAssets(context.Background(), test.query)
			if err != nil {
				t.Fatalf("FindMatchingAssets() error = %v", err)
			}
			if got := len(matches) == 1; got != test.want {
				t.Fatalf("FindMatchingAssets() returned %#v, match = %v; want match = %v", matches, got, test.want)
			}
			if test.want {
				match := matches[0]
				if match.AssetID != "synthetic-asset-42" || match.WorkTitle != test.title ||
					match.Platform != "platform-x" || match.Format != "disc-image" {
					t.Fatalf("match = %#v", match)
				}
			}
		})
	}
}

func syntheticMatchSource(t *testing.T, title string) *ManifestInventorySource {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	asset := manifest.Assets[0]
	asset.ID = "synthetic-asset-42"
	asset.Work = ManifestWork{
		Title:       title,
		ExternalIDs: map[string]string{"test-catalog": "record-42"},
	}
	asset.Edition = ManifestEdition{Platform: "platform-x", Format: "disc-image"}
	for index := range asset.Parts {
		asset.Parts[index].Filename = "synthetic-media.bin"
	}
	asset.Locations = []ManifestLocation{{
		Class: "archive", Provider: "test-storage", Locator: "test://inventory/synthetic-asset-42",
	}}
	manifest.Assets = []ManifestAsset{asset}
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return NewManifestInventorySource(path)
}

func TestManifestRejectsMalformedInput(t *testing.T) {
	valid, err := os.ReadFile(filepath.Join("..", "..", "examples", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	unknownField := strings.Replace(string(valid), `"schema_version": 1`, `"schema_version": 1, "unexpected": true`, 1)
	wrongSize := strings.Replace(string(valid), `"total_size_bytes": 1450000000`, `"total_size_bytes": 1450000001`, 1)
	stagingLocation := strings.Replace(string(valid), `"class": "archive"`, `"class": "local_cache"`, 1)
	stagingLocation = strings.Replace(stagingLocation, `"testremote:games/gamecube/Soulcalibur II.iso"`, `"/cache/.staging/job/file.iso"`, 1)
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{name: "invalid JSON", data: []byte(`{"schema_version":`), want: "decode inventory manifest JSON"},
		{name: "missing required asset id", data: []byte(`{"schema_version":1,"assets":[{}]}`), want: "validation failed"},
		{name: "unknown field", data: []byte(unknownField), want: "validation failed"},
		{name: "incomplete asset size", data: []byte(wrongSize), want: "parts sum"},
		{name: "staging path cannot be local ready inventory", data: []byte(stagingLocation), want: "staging path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseManifest(test.data)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestManifestSourceRejectsOversizedAndMissingFiles(t *testing.T) {
	missing := NewManifestInventorySource(filepath.Join(t.TempDir(), "missing.json"))
	if _, err := missing.Entries(context.Background()); err == nil || !strings.Contains(err.Error(), "open inventory manifest") {
		t.Fatalf("missing file error = %v", err)
	}

	path := filepath.Join(t.TempDir(), "large.json")
	if err := os.WriteFile(path, make([]byte, maxManifestBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManifestInventorySource(path).Entries(context.Background()); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized file error = %v", err)
	}
}

func TestManifestStorageSourceReturnsLocationsForRequestedAsset(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	first := manifest.Assets[0]
	first.Locations = []ManifestLocation{
		{Class: "archive", Provider: "rclone", Locator: "gdrive:games/gamecube/Soulcalibur II.iso"},
		{Class: "local_cache", Provider: "filesystem", Locator: "/cache/games/Soulcalibur II.iso"},
	}
	second := first
	second.ID = "another-game"
	second.Edition = ManifestEdition{Platform: "pc", Format: "disc_image"}
	second.Locations = []ManifestLocation{
		{Class: "library", Provider: "nas", Locator: "/mnt/library/another-game.iso"},
	}
	manifest.Assets = []ManifestAsset{first, second}
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	var storage providers.StorageSource = NewManifestInventorySource(path)
	locations, err := storage.LocationsForAsset(context.Background(), domain.AssetID(first.ID))
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.AssetLocation{
		{
			AssetID: domain.AssetID(first.ID), StorageProviderID: "rclone",
			Locator: "gdrive:games/gamecube/Soulcalibur II.iso", LocationClass: "archive",
		},
		{
			AssetID: domain.AssetID(first.ID), StorageProviderID: "filesystem",
			Locator: "/cache/games/Soulcalibur II.iso", LocationClass: "local_cache",
		},
	}
	if len(locations) != len(want) {
		t.Fatalf("got %d locations, want %d: %#v", len(locations), len(want), locations)
	}
	for index := range want {
		if locations[index] != want[index] {
			t.Errorf("location %d = %#v, want %#v", index, locations[index], want[index])
		}
	}

	unknown, err := storage.LocationsForAsset(context.Background(), "missing-asset")
	if err != nil {
		t.Fatal(err)
	}
	if unknown == nil || len(unknown) != 0 {
		t.Fatalf("unknown asset locations = %#v, want an empty non-nil slice", unknown)
	}
}
