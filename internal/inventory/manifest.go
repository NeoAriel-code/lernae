// Package inventory implements Foundation's local inventory-manifest baseline.
package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"lernae/internal/domain"
	"lernae/internal/providers"
	"lernae/schemas"
)

const maxManifestBytes = 4 << 20

type Manifest struct {
	SchemaVersion int             `json:"schema_version"`
	Assets        []ManifestAsset `json:"assets"`
}

type ManifestAsset struct {
	ID             string              `json:"id"`
	Work           ManifestWork        `json:"work"`
	Edition        ManifestEdition     `json:"edition"`
	TotalSizeBytes int64               `json:"total_size_bytes"`
	Parts          []ManifestAssetPart `json:"parts"`
	Locations      []ManifestLocation  `json:"locations"`
}

type ManifestWork struct {
	Title       string            `json:"title"`
	ExternalIDs map[string]string `json:"external_ids,omitempty"`
}

type ManifestEdition struct {
	Platform string `json:"platform"`
	Format   string `json:"format"`
}

type ManifestAssetPart struct {
	PartIndex *int   `json:"part_index,omitempty"`
	Role      string `json:"role"`
	Filename  string `json:"filename"`
	SizeBytes int64  `json:"size_bytes"`
}

type ManifestLocation struct {
	Class    string `json:"class"`
	Provider string `json:"provider"`
	Locator  string `json:"locator"`
}

// InventoryEntry retains manifest details that are not canonical catalog
// entities yet. EditionID uses the adapter-local platform:format key because
// Manifest v1 intentionally does not assign Lernae catalog IDs.
type InventoryEntry struct {
	AssetID         domain.AssetID
	EditionID       domain.EditionID
	WorkTitle       string
	WorkExternalIDs map[string]string
	Platform        string
	Format          string
	TotalSizeBytes  int64
	Parts           []ManifestAssetPart
	Locations       []ManifestLocation
}

type ManifestInventorySource struct {
	Path string
}

var _ providers.InventorySource = (*ManifestInventorySource)(nil)
var _ providers.StorageSource = (*ManifestInventorySource)(nil)

func NewManifestInventorySource(path string) *ManifestInventorySource {
	return &ManifestInventorySource{Path: path}
}

func (source *ManifestInventorySource) Load(ctx context.Context) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	file, err := os.Open(source.Path)
	if err != nil {
		return Manifest{}, fmt.Errorf("open inventory manifest %q: %w", source.Path, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("read inventory manifest %q: %w", source.Path, err)
	}
	if len(data) > maxManifestBytes {
		return Manifest{}, fmt.Errorf("inventory manifest %q exceeds the %d-byte limit", source.Path, maxManifestBytes)
	}
	return ParseManifest(data)
}

func ParseManifest(data []byte) (Manifest, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Manifest{}, errors.New("inventory manifest is empty")
	}
	compiled, err := manifestSchema()
	if err != nil {
		return Manifest{}, fmt.Errorf("load inventory manifest schema: %w", err)
	}
	var instance any
	if err := json.Unmarshal(data, &instance); err != nil {
		return Manifest{}, fmt.Errorf("decode inventory manifest JSON: %w", err)
	}
	if err := compiled.Validate(instance); err != nil {
		return Manifest{}, fmt.Errorf("inventory manifest validation failed: %w", err)
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode validated inventory manifest: %w", err)
	}
	if err := validateManifestDomain(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (source *ManifestInventorySource) Entries(ctx context.Context) ([]InventoryEntry, error) {
	manifest, err := source.Load(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]InventoryEntry, 0, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		entry := InventoryEntry{
			AssetID:         domain.AssetID(asset.ID),
			EditionID:       EditionKey(asset.Edition.Platform, asset.Edition.Format),
			WorkTitle:       asset.Work.Title,
			WorkExternalIDs: cloneMap(asset.Work.ExternalIDs),
			Platform:        asset.Edition.Platform,
			Format:          asset.Edition.Format,
			TotalSizeBytes:  asset.TotalSizeBytes,
			Parts:           append([]ManifestAssetPart(nil), asset.Parts...),
			Locations:       append([]ManifestLocation(nil), asset.Locations...),
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// AssetsForEdition implements the shared provider capability. Edition keys are
// derived from the v1 manifest's concrete platform and format fields.
func (source *ManifestInventorySource) AssetsForEdition(ctx context.Context, editionID domain.EditionID) ([]domain.Asset, error) {
	entries, err := source.Entries(ctx)
	if err != nil {
		return nil, err
	}
	assets := make([]domain.Asset, 0)
	for _, entry := range entries {
		if entry.EditionID != editionID {
			continue
		}
		assets = append(assets, domain.Asset{
			ID: entry.AssetID, EditionID: entry.EditionID,
			Kind: entry.Format, TotalSizeBytes: entry.TotalSizeBytes,
		})
	}
	return assets, nil
}

// FindMatchingAssets resolves an exact external work identity and concrete
// edition attributes against the manifest. The manifest's platform:format
// EditionID remains an adapter-local implementation detail.
func (source *ManifestInventorySource) FindMatchingAssets(ctx context.Context, query providers.InventoryQuery) ([]providers.InventoryMatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(query.WorkIdentity.Provider) == "" ||
		strings.TrimSpace(query.WorkIdentity.ExternalID) == "" ||
		strings.TrimSpace(query.Platform) == "" ||
		strings.TrimSpace(query.Format) == "" {
		return []providers.InventoryMatch{}, nil
	}

	entries, err := source.Entries(ctx)
	if err != nil {
		return nil, err
	}
	matches := make([]providers.InventoryMatch, 0)
	for _, entry := range entries {
		if entry.WorkExternalIDs[query.WorkIdentity.Provider] != query.WorkIdentity.ExternalID ||
			entry.Platform != query.Platform || entry.Format != query.Format {
			continue
		}
		parts := make([]providers.InventoryPart, 0, len(entry.Parts))
		for _, part := range entry.Parts {
			parts = append(parts, providers.InventoryPart{
				PartIndex: cloneInt(part.PartIndex),
				Role:      part.Role,
				Filename:  part.Filename,
				SizeBytes: part.SizeBytes,
			})
		}
		matches = append(matches, providers.InventoryMatch{
			AssetID:        entry.AssetID,
			WorkTitle:      entry.WorkTitle,
			Platform:       entry.Platform,
			Format:         entry.Format,
			TotalSizeBytes: entry.TotalSizeBytes,
			Parts:          parts,
		})
	}
	return matches, nil
}

// LocationsForAsset implements the shared storage capability using the
// locations declared for the requested Asset in the manifest.
func (source *ManifestInventorySource) LocationsForAsset(ctx context.Context, assetID domain.AssetID) ([]domain.AssetLocation, error) {
	entries, err := source.Entries(ctx)
	if err != nil {
		return nil, err
	}
	locations := make([]domain.AssetLocation, 0)
	for _, entry := range entries {
		if entry.AssetID != assetID {
			continue
		}
		for _, location := range entry.Locations {
			locations = append(locations, domain.AssetLocation{
				AssetID:           entry.AssetID,
				StorageProviderID: location.Provider,
				Locator:           location.Locator,
				LocationClass:     location.Class,
			})
		}
	}
	return locations, nil
}

func EditionKey(platform, format string) domain.EditionID {
	return domain.EditionID(strings.ToLower(strings.TrimSpace(platform)) + ":" + strings.ToLower(strings.TrimSpace(format)))
}

func validateManifestDomain(manifest Manifest) error {
	seen := make(map[string]struct{}, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		if _, exists := seen[asset.ID]; exists {
			return fmt.Errorf("inventory manifest contains duplicate asset id %q", asset.ID)
		}
		seen[asset.ID] = struct{}{}

		var partTotal int64
		for partIndex, part := range asset.Parts {
			if partTotal > math.MaxInt64-part.SizeBytes {
				return fmt.Errorf("asset %q part sizes overflow", asset.ID)
			}
			partTotal += part.SizeBytes
			if part.PartIndex != nil && *part.PartIndex < 1 {
				return fmt.Errorf("asset %q part %d has an invalid part_index", asset.ID, partIndex+1)
			}
		}
		if partTotal != asset.TotalSizeBytes {
			return fmt.Errorf("asset %q total_size_bytes is %d but its parts sum to %d", asset.ID, asset.TotalSizeBytes, partTotal)
		}
		for _, location := range asset.Locations {
			if location.Class == "local_cache" && hasStagingPathComponent(location.Locator) {
				return fmt.Errorf("asset %q declares a staging path as local_cache inventory", asset.ID)
			}
		}
	}
	return nil
}

func hasStagingPathComponent(locator string) bool {
	clean := filepath.ToSlash(filepath.Clean(locator))
	for _, part := range strings.Split(clean, "/") {
		if part == ".staging" {
			return true
		}
	}
	return false
}

func cloneMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func cloneInt(source *int) *int {
	if source == nil {
		return nil
	}
	cloned := *source
	return &cloned
}

var schemaOnce sync.Once
var compiledManifestSchema *jsonschema.Schema
var schemaErr error

func manifestSchema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		data, err := schemas.Files.ReadFile("inventory-manifest.schema.json")
		if err != nil {
			schemaErr = err
			return
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			schemaErr = err
			return
		}
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource("inventory-manifest.schema.json", document); err != nil {
			schemaErr = err
			return
		}
		compiledManifestSchema, schemaErr = compiler.Compile("inventory-manifest.schema.json")
	})
	return compiledManifestSchema, schemaErr
}
