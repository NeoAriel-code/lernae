// Package catalog persists the narrow Work-to-Asset graph used by the first
// product slice.
package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"lernae/internal/domain"
)

var ErrNotFound = errors.New("catalog Work not found")

// WorkGraph contains a Work and its persisted catalog children. Child slices
// are returned in stable order: editions, assets, locations, and identities by
// ID/provider; parts by explicit index (unindexed parts last), then ID.
type WorkGraph struct {
	Work               domain.Work
	Editions           []domain.Edition
	Assets             []domain.Asset
	Parts              []domain.AssetPart
	Locations          []domain.AssetLocation
	ExternalIdentities []domain.ExternalIdentity
}

type SQLiteRepository struct {
	db *sql.DB
}

func NewSQLiteRepository(db *sql.DB) *SQLiteRepository {
	return &SQLiteRepository{db: db}
}

// CreateGraph atomically persists one Work and all of its Editions, Assets,
// AssetParts, AssetLocations, and Work ExternalIdentities.
func (r *SQLiteRepository) CreateGraph(ctx context.Context, graph WorkGraph) error {
	return r.persistGraph(ctx, graph, false)
}

// EnsureGraph atomically adds a graph while reusing rows with the same IDs.
// It leaves existing catalog metadata and locations intact.
func (r *SQLiteRepository) EnsureGraph(ctx context.Context, graph WorkGraph) error {
	return r.persistGraph(ctx, graph, true)
}

func (r *SQLiteRepository) persistGraph(ctx context.Context, graph WorkGraph, idempotent bool) error {
	if err := validateGraph(graph); err != nil {
		return err
	}
	conflictClause := ""
	if idempotent {
		conflictClause = " ON CONFLICT(id) DO NOTHING"
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin catalog graph: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if idempotent {
		if err := ensureAssetsCompatible(ctx, tx, graph.Assets); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO works (id, medium, work_type, title, summary) VALUES (?, ?, ?, ?, ?)`+conflictClause,
		graph.Work.ID, graph.Work.Medium, graph.Work.WorkType, graph.Work.Title, graph.Work.Summary); err != nil {
		return fmt.Errorf("create catalog Work %q: %w", graph.Work.ID, err)
	}
	for _, edition := range graph.Editions {
		if _, err := tx.ExecContext(ctx, `INSERT INTO editions (id, work_id, label, platform, region, format)
			VALUES (?, ?, ?, ?, ?, ?)`+conflictClause, edition.ID, edition.WorkID, nullableString(edition.Label), nullableString(edition.Platform),
			nullableString(edition.Region), edition.Format); err != nil {
			return fmt.Errorf("create catalog Edition %q: %w", edition.ID, err)
		}
	}
	for _, asset := range graph.Assets {
		if _, err := tx.ExecContext(ctx, `INSERT INTO assets (id, edition_id, kind, total_size_bytes)
			VALUES (?, ?, ?, ?)`+conflictClause, asset.ID, asset.EditionID, asset.Kind, asset.TotalSizeBytes); err != nil {
			return fmt.Errorf("create catalog Asset %q: %w", asset.ID, err)
		}
	}
	for _, part := range graph.Parts {
		var partIndex any
		if part.PartIndex != nil {
			partIndex = *part.PartIndex
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO asset_parts
			(id, asset_id, part_index, role, filename, relative_path, size_bytes) VALUES (?, ?, ?, ?, ?, ?, ?)`+conflictClause,
			part.ID, part.AssetID, partIndex, part.Role, part.Filename, nullableString(part.RelativePath), part.SizeBytes); err != nil {
			return fmt.Errorf("create AssetPart %q: %w", part.ID, err)
		}
	}
	for _, location := range graph.Locations {
		if _, err := tx.ExecContext(ctx, `INSERT INTO asset_locations
			(id, asset_id, storage_provider_id, locator, location_class) VALUES (?, ?, ?, ?, ?)`+conflictClause,
			location.ID, location.AssetID, location.StorageProviderID, location.Locator, location.LocationClass); err != nil {
			return fmt.Errorf("create AssetLocation %q: %w", location.ID, err)
		}
	}
	for _, identity := range graph.ExternalIdentities {
		if _, err := tx.ExecContext(ctx, `INSERT INTO external_identities (id, work_id, provider, external_id)
			VALUES (?, ?, ?, ?)`+conflictClause, identity.ID, identity.WorkID, identity.Provider, identity.ExternalID); err != nil {
			return fmt.Errorf("create Work ExternalIdentity %q: %w", identity.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit catalog graph %q: %w", graph.Work.ID, err)
	}
	return nil
}

func ensureAssetsCompatible(ctx context.Context, tx *sql.Tx, assets []domain.Asset) error {
	for _, asset := range assets {
		var editionID domain.EditionID
		var kind string
		var totalSizeBytes int64
		err := tx.QueryRowContext(ctx, `SELECT edition_id, kind, total_size_bytes FROM assets WHERE id = ?`, asset.ID).
			Scan(&editionID, &kind, &totalSizeBytes)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("check existing catalog Asset %q: %w", asset.ID, err)
		}
		if editionID != asset.EditionID || kind != asset.Kind || totalSizeBytes != asset.TotalSizeBytes {
			return fmt.Errorf("catalog Asset %q is already bound to incompatible inventory data", asset.ID)
		}
	}
	return nil
}

// GetWork loads a Work and all of its persisted catalog children.
func (r *SQLiteRepository) GetWork(ctx context.Context, id domain.WorkID) (WorkGraph, error) {
	graph := WorkGraph{Work: domain.Work{ID: id}}
	var medium string
	if err := r.db.QueryRowContext(ctx, `SELECT medium, work_type, title, summary FROM works WHERE id = ?`, id).
		Scan(&medium, &graph.Work.WorkType, &graph.Work.Title, &graph.Work.Summary); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkGraph{}, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return WorkGraph{}, fmt.Errorf("load catalog Work %q: %w", id, err)
	}
	graph.Work.Medium = domain.Medium(medium)

	rows, err := r.db.QueryContext(ctx, `SELECT id, label, platform, region, format FROM editions WHERE work_id = ? ORDER BY id`, id)
	if err != nil {
		return WorkGraph{}, fmt.Errorf("load Editions for Work %q: %w", id, err)
	}
	for rows.Next() {
		var edition domain.Edition
		var label, platform, region sql.NullString
		edition.WorkID = id
		if err := rows.Scan(&edition.ID, &label, &platform, &region, &edition.Format); err != nil {
			_ = rows.Close()
			return WorkGraph{}, fmt.Errorf("decode Edition for Work %q: %w", id, err)
		}
		edition.Label, edition.Platform, edition.Region = nullString(label), nullString(platform), nullString(region)
		graph.Editions = append(graph.Editions, edition)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return WorkGraph{}, fmt.Errorf("read Editions for Work %q: %w", id, err)
	}
	if err := rows.Close(); err != nil {
		return WorkGraph{}, fmt.Errorf("close Editions for Work %q: %w", id, err)
	}

	rows, err = r.db.QueryContext(ctx, `SELECT a.id, a.edition_id, a.kind, a.total_size_bytes
		FROM assets a JOIN editions e ON e.id = a.edition_id WHERE e.work_id = ? ORDER BY a.id`, id)
	if err != nil {
		return WorkGraph{}, fmt.Errorf("load Assets for Work %q: %w", id, err)
	}
	for rows.Next() {
		var asset domain.Asset
		if err := rows.Scan(&asset.ID, &asset.EditionID, &asset.Kind, &asset.TotalSizeBytes); err != nil {
			_ = rows.Close()
			return WorkGraph{}, fmt.Errorf("decode Asset for Work %q: %w", id, err)
		}
		graph.Assets = append(graph.Assets, asset)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return WorkGraph{}, fmt.Errorf("read Assets for Work %q: %w", id, err)
	}
	if err := rows.Close(); err != nil {
		return WorkGraph{}, fmt.Errorf("close Assets for Work %q: %w", id, err)
	}

	rows, err = r.db.QueryContext(ctx, `SELECT p.id, p.asset_id, p.part_index, p.role, p.filename,
		p.relative_path, p.size_bytes FROM asset_parts p JOIN assets a ON a.id = p.asset_id
		JOIN editions e ON e.id = a.edition_id WHERE e.work_id = ?
		ORDER BY a.id, CASE WHEN p.part_index IS NULL THEN 1 ELSE 0 END, p.part_index, p.id`, id)
	if err != nil {
		return WorkGraph{}, fmt.Errorf("load AssetParts for Work %q: %w", id, err)
	}
	for rows.Next() {
		var part domain.AssetPart
		var partIndex sql.NullInt64
		var relativePath sql.NullString
		if err := rows.Scan(&part.ID, &part.AssetID, &partIndex, &part.Role, &part.Filename, &relativePath, &part.SizeBytes); err != nil {
			_ = rows.Close()
			return WorkGraph{}, fmt.Errorf("decode AssetPart for Work %q: %w", id, err)
		}
		if partIndex.Valid {
			index := int(partIndex.Int64)
			part.PartIndex = &index
		}
		part.RelativePath = nullString(relativePath)
		graph.Parts = append(graph.Parts, part)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return WorkGraph{}, fmt.Errorf("read AssetParts for Work %q: %w", id, err)
	}
	if err := rows.Close(); err != nil {
		return WorkGraph{}, fmt.Errorf("close AssetParts for Work %q: %w", id, err)
	}

	rows, err = r.db.QueryContext(ctx, `SELECT l.id, l.asset_id, l.storage_provider_id, l.locator, l.location_class
		FROM asset_locations l JOIN assets a ON a.id = l.asset_id JOIN editions e ON e.id = a.edition_id
		WHERE e.work_id = ? ORDER BY l.id`, id)
	if err != nil {
		return WorkGraph{}, fmt.Errorf("load AssetLocations for Work %q: %w", id, err)
	}
	for rows.Next() {
		var location domain.AssetLocation
		if err := rows.Scan(&location.ID, &location.AssetID, &location.StorageProviderID, &location.Locator, &location.LocationClass); err != nil {
			_ = rows.Close()
			return WorkGraph{}, fmt.Errorf("decode AssetLocation for Work %q: %w", id, err)
		}
		graph.Locations = append(graph.Locations, location)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return WorkGraph{}, fmt.Errorf("read AssetLocations for Work %q: %w", id, err)
	}
	if err := rows.Close(); err != nil {
		return WorkGraph{}, fmt.Errorf("close AssetLocations for Work %q: %w", id, err)
	}

	rows, err = r.db.QueryContext(ctx, `SELECT id, work_id, provider, external_id FROM external_identities
		WHERE work_id = ? ORDER BY provider, external_id, id`, id)
	if err != nil {
		return WorkGraph{}, fmt.Errorf("load ExternalIdentities for Work %q: %w", id, err)
	}
	for rows.Next() {
		var identity domain.ExternalIdentity
		if err := rows.Scan(&identity.ID, &identity.WorkID, &identity.Provider, &identity.ExternalID); err != nil {
			_ = rows.Close()
			return WorkGraph{}, fmt.Errorf("decode ExternalIdentity for Work %q: %w", id, err)
		}
		graph.ExternalIdentities = append(graph.ExternalIdentities, identity)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return WorkGraph{}, fmt.Errorf("read ExternalIdentities for Work %q: %w", id, err)
	}
	if err := rows.Close(); err != nil {
		return WorkGraph{}, fmt.Errorf("close ExternalIdentities for Work %q: %w", id, err)
	}

	return graph, nil
}

// GetWorkByExternalIdentity resolves a provider record and loads the same
// complete graph returned by GetWork.
func (r *SQLiteRepository) GetWorkByExternalIdentity(ctx context.Context, provider, externalID string) (WorkGraph, error) {
	var workID domain.WorkID
	if err := r.db.QueryRowContext(ctx, `SELECT work_id FROM external_identities WHERE provider = ? AND external_id = ?`,
		provider, externalID).Scan(&workID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkGraph{}, fmt.Errorf("%w: provider %s external ID %s", ErrNotFound, provider, externalID)
		}
		return WorkGraph{}, fmt.Errorf("resolve Work ExternalIdentity %s:%s: %w", provider, externalID, err)
	}
	return r.GetWork(ctx, workID)
}

func validateGraph(graph WorkGraph) error {
	if graph.Work.ID == "" {
		return errors.New("catalog Work ID must not be empty")
	}
	if graph.Work.Summary != normalizeWorkSummary(graph.Work.Summary) {
		return fmt.Errorf("catalog Work %q has an unsafe or oversized summary", graph.Work.ID)
	}
	editionIDs := make(map[domain.EditionID]struct{}, len(graph.Editions))
	for _, edition := range graph.Editions {
		if edition.WorkID != graph.Work.ID {
			return fmt.Errorf("Edition %q does not belong to Work %q", edition.ID, graph.Work.ID)
		}
		editionIDs[edition.ID] = struct{}{}
	}
	assetIDs := make(map[domain.AssetID]struct{}, len(graph.Assets))
	for _, asset := range graph.Assets {
		if _, exists := editionIDs[asset.EditionID]; !exists {
			return fmt.Errorf("Asset %q references Edition %q outside Work graph", asset.ID, asset.EditionID)
		}
		assetIDs[asset.ID] = struct{}{}
	}
	for _, part := range graph.Parts {
		if _, exists := assetIDs[part.AssetID]; !exists {
			return fmt.Errorf("AssetPart %q references Asset %q outside Work graph", part.ID, part.AssetID)
		}
	}
	for _, location := range graph.Locations {
		if _, exists := assetIDs[location.AssetID]; !exists {
			return fmt.Errorf("AssetLocation %q references Asset %q outside Work graph", location.ID, location.AssetID)
		}
	}
	for _, identity := range graph.ExternalIdentities {
		if identity.WorkID != graph.Work.ID {
			return fmt.Errorf("ExternalIdentity %q does not belong to Work %q", identity.ID, graph.Work.ID)
		}
	}
	return nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullString(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return value.String
}
