package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"lernae/internal/agent"
	"lernae/internal/domain"
)

const (
	localCacheStorageProvider = "filesystem"
	localCacheLocationClass   = "local_cache"
)

// EnsureLocalCacheLocation durably projects a validated Agent restore result
// into relative catalog metadata. The database graph must still match the
// Asset and ROM part carried by the trusted restore request.
func (r *SQLiteRepository) EnsureLocalCacheLocation(
	ctx context.Context,
	restore agent.RestoreAsset,
	result agent.RestoreResult,
) (domain.AssetLocation, error) {
	if err := result.ValidateFor(restore); err != nil {
		return domain.AssetLocation{}, fmt.Errorf("validate restored local-cache evidence: %w", err)
	}
	if !validLocalCacheLocator(result.RelativePath, restore.Asset.ID, restore.Parts[0].Filename) {
		return domain.AssetLocation{}, errors.New("restore result has an unsafe local-cache locator")
	}
	if r == nil || r.db == nil {
		return domain.AssetLocation{}, errors.New("catalog repository is unavailable")
	}
	if err := validatePersistedRestoreAsset(ctx, r.db, restore); err != nil {
		return domain.AssetLocation{}, err
	}

	candidate := domain.AssetLocation{
		ID:                domain.AssetLocationID(stableCatalogID("location", string(restore.Asset.ID), localCacheStorageProvider, result.RelativePath, localCacheLocationClass)),
		AssetID:           restore.Asset.ID,
		StorageProviderID: localCacheStorageProvider,
		Locator:           result.RelativePath,
		LocationClass:     localCacheLocationClass,
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO asset_locations
		(id, asset_id, storage_provider_id, locator, location_class) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(asset_id, storage_provider_id, locator, location_class) DO NOTHING`,
		candidate.ID, candidate.AssetID, candidate.StorageProviderID, candidate.Locator, candidate.LocationClass)
	if err != nil {
		return domain.AssetLocation{}, fmt.Errorf("persist local-cache AssetLocation for Asset %q: %w", candidate.AssetID, err)
	}
	var persisted domain.AssetLocation
	if err := r.db.QueryRowContext(ctx, `SELECT id, asset_id, storage_provider_id, locator, location_class
		FROM asset_locations WHERE asset_id = ? AND storage_provider_id = ? AND locator = ? AND location_class = ?`,
		candidate.AssetID, candidate.StorageProviderID, candidate.Locator, candidate.LocationClass).
		Scan(&persisted.ID, &persisted.AssetID, &persisted.StorageProviderID, &persisted.Locator, &persisted.LocationClass); err != nil {
		return domain.AssetLocation{}, fmt.Errorf("load persisted local-cache AssetLocation for Asset %q: %w", candidate.AssetID, err)
	}
	return persisted, nil
}

// RemoveLocalCacheLocation removes only the exact local-cache metadata row
// observed as invalid by the caller. It never resolves or removes the local
// file named by that location.
//
// Local-cache IDs are deterministic for an Asset/provider/locator tuple, so a
// same-row reprojection is not distinguishable from the observed row without
// generation metadata. Supported PLAY operations serialize through their
// durable active Job in one Server process; multi-process PLAY admission and
// direct external catalog writers are not supported during candidate
// validation or cleanup.
func (r *SQLiteRepository) RemoveLocalCacheLocation(ctx context.Context, expected domain.AssetLocation) (int64, error) {
	if r == nil || r.db == nil {
		return 0, errors.New("catalog repository is unavailable")
	}
	if strings.TrimSpace(string(expected.ID)) == "" || strings.TrimSpace(string(expected.AssetID)) == "" ||
		expected.StorageProviderID == "" || expected.Locator == "" || expected.LocationClass != localCacheLocationClass {
		return 0, errors.New("local-cache cleanup requires the exact observed location identity")
	}
	result, err := r.db.ExecContext(ctx, `DELETE FROM asset_locations WHERE id = ? AND asset_id = ? AND storage_provider_id = ?
		AND locator = ? AND location_class = ?`, expected.ID, expected.AssetID, expected.StorageProviderID, expected.Locator, localCacheLocationClass)
	if err != nil {
		return 0, fmt.Errorf("remove local-cache metadata location %q: %w", expected.ID, err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count removed local-cache metadata location %q: %w", expected.ID, err)
	}
	return removed, nil
}

func validatePersistedRestoreAsset(ctx context.Context, db *sql.DB, restore agent.RestoreAsset) error {
	var editionID domain.EditionID
	var kind string
	var size int64
	if err := db.QueryRowContext(ctx, `SELECT edition_id, kind, total_size_bytes FROM assets WHERE id = ?`, restore.Asset.ID).
		Scan(&editionID, &kind, &size); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("restored Asset %q is not present in the catalog", restore.Asset.ID)
		}
		return fmt.Errorf("load restored Asset %q from catalog: %w", restore.Asset.ID, err)
	}
	if editionID != restore.Asset.EditionID || kind != restore.Asset.Kind || size != restore.Asset.TotalSizeBytes {
		return fmt.Errorf("restore request does not match catalog Asset %q", restore.Asset.ID)
	}
	var partCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_parts WHERE asset_id = ?`, restore.Asset.ID).Scan(&partCount); err != nil {
		return fmt.Errorf("count catalog AssetParts for Asset %q: %w", restore.Asset.ID, err)
	}
	if partCount != 1 {
		return fmt.Errorf("catalog Asset %q is not a single-part restore candidate", restore.Asset.ID)
	}
	part := restore.Parts[0]
	var matchingParts int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_parts
		WHERE id = ? AND asset_id = ? AND role = ? AND filename = ? AND size_bytes = ? AND COALESCE(relative_path, '') = ?`,
		part.ID, part.AssetID, part.Role, part.Filename, part.SizeBytes, part.RelativePath).Scan(&matchingParts); err != nil {
		return fmt.Errorf("verify catalog AssetPart for Asset %q: %w", restore.Asset.ID, err)
	}
	if matchingParts != 1 {
		return fmt.Errorf("restore request does not match the catalog ROM part for Asset %q", restore.Asset.ID)
	}
	return nil
}

func validLocalCacheLocator(locator string, assetID domain.AssetID, filename string) bool {
	if !utf8.ValidString(locator) || strings.TrimSpace(locator) != locator || path.IsAbs(locator) || path.Clean(locator) != locator ||
		strings.ContainsAny(locator, `\\:`) || len(locator) > 4096 {
		return false
	}
	components := strings.Split(locator, "/")
	if len(components) != 3 || components[0] != "assets" || components[1] != string(assetID) || components[2] != filename {
		return false
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." || component == ".staging" {
			return false
		}
		for _, character := range component {
			if character == 0 || character < 0x20 || character == 0x7f {
				return false
			}
		}
	}
	return true
}
