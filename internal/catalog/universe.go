package catalog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"lernae/internal/domain"
	"lernae/internal/providers"
)

var (
	ErrUniverseNotFound              = errors.New("catalog Universe not found")
	ErrUniverseMembershipNotFound    = errors.New("catalog Work has no accepted Universe membership")
	ErrUniverseMembershipExcluded    = errors.New("catalog Work/Universe pair is explicitly excluded")
	ErrUniverseAliasNotFound         = errors.New("catalog Universe alias not found")
	ErrWorkCollectionNotFound        = errors.New("catalog Work collection not found")
	ErrInvalidWorkCollectionIdentity = errors.New("invalid Work collection external identity")
	ErrWikidataWorkIdentityNotFound  = errors.New("no local Work matches Wikidata provider identifiers")
	ErrWikidataWorkIdentityAmbiguous = errors.New("Wikidata provider identifiers match multiple local Works")
)

// UniverseGraph contains accepted relationships, review-needed suggestions,
// aliases, and explicitly rejected Work candidates for one Universe.
type UniverseGraph struct {
	Universe    domain.Universe
	Memberships []domain.UniverseMembership
	Suggestions []domain.UniverseMembership
	Aliases     []domain.UniverseAlias
	Exclusions  []domain.UniverseExclusion
}

// MaterializeExternalWork creates or reuses a canonical Work using only the
// exact provider identity and bounded provider-neutral display metadata. It
// never creates inventory, edition, ownership, or playback records.
func (r *SQLiteRepository) MaterializeExternalWork(ctx context.Context, input domain.WorkMaterialization) (WorkGraph, error) {
	if err := ctx.Err(); err != nil {
		return WorkGraph{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkGraph{}, fmt.Errorf("begin external Work materialization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	workID, err := materializeExternalWorkTx(ctx, tx, input)
	if err != nil {
		return WorkGraph{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkGraph{}, fmt.Errorf("commit external Work materialization: %w", err)
	}
	return r.GetWork(ctx, workID)
}

// CreateUniverse persists a new Universe. Universe IDs are local opaque IDs;
// external provider IDs belong only to external_identities.
func (r *SQLiteRepository) CreateUniverse(ctx context.Context, universe domain.Universe) error {
	if err := validateBoundedText("Universe ID", string(universe.ID), 128); err != nil {
		return err
	}
	var err error
	if universe, err = normalizeCreateUniverseMetadata(universe); err != nil {
		return err
	}
	now := time.Now().UTC()
	if universe.CreatedAt.IsZero() {
		universe.CreatedAt = now
	} else {
		universe.CreatedAt = universe.CreatedAt.UTC()
	}
	if universe.UpdatedAt.IsZero() {
		universe.UpdatedAt = universe.CreatedAt
	} else {
		universe.UpdatedAt = universe.UpdatedAt.UTC()
	}
	if _, err := r.db.ExecContext(ctx, `INSERT INTO universes
		(id, title, sort_title, description, artwork, created_at_utc, updated_at_utc, existence_confidence,
		naming_confidence, naming_confidence_level, naming_evidence, provenance, confirmed_by_user)
		VALUES (?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?)`,
		universe.ID, universe.Title, universe.SortTitle, universe.Description,
		formatUniverseTime(universe.CreatedAt), formatUniverseTime(universe.UpdatedAt), universe.ExistenceConfidence,
		universe.NamingConfidence, universe.NamingConfidenceLevel, universe.NamingEvidence, universe.Provenance, universe.ConfirmedByUser); err != nil {
		return fmt.Errorf("create Universe %q: %w", universe.ID, err)
	}
	return nil
}

// GetUniverse loads the complete durable Universe grouping state in stable
// relation order.
func (r *SQLiteRepository) GetUniverse(ctx context.Context, id domain.UniverseID) (UniverseGraph, error) {
	graph := UniverseGraph{Universe: domain.Universe{ID: id}}
	var sortTitle, description, artwork sql.NullString
	var createdAt, updatedAt string
	if err := r.db.QueryRowContext(ctx, `SELECT title, sort_title, description, artwork, created_at_utc, updated_at_utc,
		existence_confidence, naming_confidence, naming_confidence_level, naming_evidence, provenance, confirmed_by_user FROM universes WHERE id = ?`, id).Scan(
		&graph.Universe.Title, &sortTitle, &description, &artwork, &createdAt, &updatedAt,
		&graph.Universe.ExistenceConfidence, &graph.Universe.NamingConfidence, &graph.Universe.NamingConfidenceLevel,
		&graph.Universe.NamingEvidence, &graph.Universe.Provenance, &graph.Universe.ConfirmedByUser); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UniverseGraph{}, fmt.Errorf("%w: %s", ErrUniverseNotFound, id)
		}
		return UniverseGraph{}, fmt.Errorf("load Universe %q: %w", id, err)
	}
	graph.Universe.SortTitle = nullStringPointer(sortTitle)
	graph.Universe.Description = nullStringPointer(description)
	graph.Universe.Artwork = nullStringPointer(artwork)
	var err error
	graph.Universe.CreatedAt, err = parseUniverseTime(createdAt)
	if err != nil {
		return UniverseGraph{}, fmt.Errorf("decode Universe creation timestamp %q: %w", id, err)
	}
	graph.Universe.UpdatedAt, err = parseUniverseTime(updatedAt)
	if err != nil {
		return UniverseGraph{}, fmt.Errorf("decode Universe update timestamp %q: %w", id, err)
	}

	rows, err := r.db.QueryContext(ctx, `SELECT work_id, provenance, confidence, evidence, reason, confirmed_by_user, accepted_at_utc, status
		FROM universe_memberships WHERE universe_id = ? ORDER BY work_id`, id)
	if err != nil {
		return UniverseGraph{}, fmt.Errorf("load accepted memberships for Universe %q: %w", id, err)
	}
	for rows.Next() {
		var membership domain.UniverseMembership
		var provenance, status string
		var acceptedAt string
		membership.UniverseID = id
		if err := rows.Scan(&membership.WorkID, &provenance, &membership.Confidence, &membership.Evidence, &membership.Reason, &membership.ConfirmedByUser, &acceptedAt, &status); err != nil {
			_ = rows.Close()
			return UniverseGraph{}, fmt.Errorf("decode membership for Universe %q: %w", id, err)
		}
		membership.Provenance = domain.UniverseMembershipProvenance(provenance)
		membership.Status = domain.UniverseMembershipStatus(status)
		membership.AcceptedAt, err = parseUniverseTime(acceptedAt)
		if err != nil {
			_ = rows.Close()
			return UniverseGraph{}, fmt.Errorf("decode membership timestamp for Universe %q: %w", id, err)
		}
		if membership.Status == domain.UniverseMembershipStatusReviewNeeded {
			graph.Suggestions = append(graph.Suggestions, membership)
		} else {
			graph.Memberships = append(graph.Memberships, membership)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return UniverseGraph{}, fmt.Errorf("read memberships for Universe %q: %w", id, err)
	}
	if err := rows.Close(); err != nil {
		return UniverseGraph{}, fmt.Errorf("close memberships for Universe %q: %w", id, err)
	}

	rows, err = r.db.QueryContext(ctx, `SELECT alias, language_code, provenance, provider_version,
		confidence, confirmed_by_user, created_at_utc FROM universe_aliases
		WHERE universe_id = ? ORDER BY alias, language_code`, id)
	if err != nil {
		return UniverseGraph{}, fmt.Errorf("load aliases for Universe %q: %w", id, err)
	}
	for rows.Next() {
		var alias domain.UniverseAlias
		var language, providerVersion sql.NullString
		var createdAt string
		alias.UniverseID = id
		if err := rows.Scan(&alias.Alias, &language, &alias.Provenance, &providerVersion,
			&alias.Confidence, &alias.ConfirmedByUser, &createdAt); err != nil {
			_ = rows.Close()
			return UniverseGraph{}, fmt.Errorf("decode alias for Universe %q: %w", id, err)
		}
		alias.Language = nullStringPointer(language)
		alias.ProviderVersion = nullStringPointer(providerVersion)
		alias.CreatedAt, err = parseUniverseTime(createdAt)
		if err != nil {
			_ = rows.Close()
			return UniverseGraph{}, fmt.Errorf("decode alias timestamp for Universe %q: %w", id, err)
		}
		graph.Aliases = append(graph.Aliases, alias)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return UniverseGraph{}, fmt.Errorf("read aliases for Universe %q: %w", id, err)
	}
	if err := rows.Close(); err != nil {
		return UniverseGraph{}, fmt.Errorf("close aliases for Universe %q: %w", id, err)
	}

	rows, err = r.db.QueryContext(ctx, `SELECT work_id, reason, recorded_at_utc FROM universe_exclusions
		WHERE universe_id = ? ORDER BY work_id`, id)
	if err != nil {
		return UniverseGraph{}, fmt.Errorf("load exclusions for Universe %q: %w", id, err)
	}
	for rows.Next() {
		var exclusion domain.UniverseExclusion
		var recordedAt string
		exclusion.UniverseID = id
		if err := rows.Scan(&exclusion.WorkID, &exclusion.Reason, &recordedAt); err != nil {
			_ = rows.Close()
			return UniverseGraph{}, fmt.Errorf("decode exclusion for Universe %q: %w", id, err)
		}
		exclusion.RecordedAt, err = parseUniverseTime(recordedAt)
		if err != nil {
			_ = rows.Close()
			return UniverseGraph{}, fmt.Errorf("decode exclusion timestamp for Universe %q: %w", id, err)
		}
		graph.Exclusions = append(graph.Exclusions, exclusion)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return UniverseGraph{}, fmt.Errorf("read exclusions for Universe %q: %w", id, err)
	}
	if err := rows.Close(); err != nil {
		return UniverseGraph{}, fmt.Errorf("close exclusions for Universe %q: %w", id, err)
	}
	return graph, nil
}

// SetUniverseMembership accepts a Work in one Universe. Reassignment and
// exclusion reversal happen in the same transaction; the previous relation is
// recorded as an exclusion so later proposals cannot silently undo correction.
func (r *SQLiteRepository) SetUniverseMembership(ctx context.Context, membership domain.UniverseMembership) (domain.UniverseMembership, error) {
	if err := ctx.Err(); err != nil {
		return domain.UniverseMembership{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.UniverseMembership{}, fmt.Errorf("begin Universe membership change: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	membership, err = setUniverseMembershipTx(ctx, tx, membership, time.Now().UTC())
	if err != nil {
		return domain.UniverseMembership{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.UniverseMembership{}, fmt.Errorf("commit Universe membership change: %w", err)
	}
	return membership, nil
}

// GetWorkUniverseMembership returns the one accepted membership for a Work.
func (r *SQLiteRepository) GetWorkUniverseMembership(ctx context.Context, workID domain.WorkID) (domain.UniverseMembership, error) {
	var membership domain.UniverseMembership
	var acceptedAt string
	var provenance string
	if err := r.db.QueryRowContext(ctx, `SELECT universe_id, provenance, confidence, evidence, reason, confirmed_by_user, accepted_at_utc, status
		FROM universe_memberships WHERE work_id = ? AND status = 'accepted'`, workID).Scan(
		&membership.UniverseID, &provenance, &membership.Confidence, &membership.Evidence, &membership.Reason, &membership.ConfirmedByUser, &acceptedAt, &membership.Status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.UniverseMembership{}, fmt.Errorf("%w: %s", ErrUniverseMembershipNotFound, workID)
		}
		return domain.UniverseMembership{}, fmt.Errorf("load accepted membership for Work %q: %w", workID, err)
	}
	membership.WorkID = workID
	membership.Provenance = domain.UniverseMembershipProvenance(provenance)
	var err error
	membership.AcceptedAt, err = parseUniverseTime(acceptedAt)
	if err != nil {
		return domain.UniverseMembership{}, fmt.Errorf("decode membership timestamp for Work %q: %w", workID, err)
	}
	return membership, nil
}

// RemoveUniverseMembership removes only the named accepted pair and records a
// durable exclusion even when the pair was only a candidate.
func (r *SQLiteRepository) RemoveUniverseMembership(ctx context.Context, workID domain.WorkID, universeID domain.UniverseID, reason string) (domain.UniverseExclusion, error) {
	if err := validateBoundedText("Work ID", string(workID), 128); err != nil {
		return domain.UniverseExclusion{}, err
	}
	if err := validateBoundedText("Universe ID", string(universeID), 128); err != nil {
		return domain.UniverseExclusion{}, err
	}
	if err := validateBoundedText("exclusion reason", reason, 512); err != nil {
		return domain.UniverseExclusion{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.UniverseExclusion{}, err
	}
	now := time.Now().UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.UniverseExclusion{}, fmt.Errorf("begin Universe membership removal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	deleted, err := tx.ExecContext(ctx, `DELETE FROM universe_memberships WHERE work_id = ? AND universe_id = ?`, workID, universeID)
	if err != nil {
		return domain.UniverseExclusion{}, fmt.Errorf("remove accepted Universe membership: %w", err)
	}
	deletedCount, err := deleted.RowsAffected()
	if err != nil {
		return domain.UniverseExclusion{}, fmt.Errorf("read removed Universe membership result: %w", err)
	}
	var existingReason, existingRecordedAt string
	existingErr := tx.QueryRowContext(ctx, `SELECT reason, recorded_at_utc FROM universe_exclusions WHERE work_id = ? AND universe_id = ?`, workID, universeID).Scan(&existingReason, &existingRecordedAt)
	if existingErr != nil && !errors.Is(existingErr, sql.ErrNoRows) {
		return domain.UniverseExclusion{}, fmt.Errorf("load existing Universe exclusion: %w", existingErr)
	}
	changed := deletedCount > 0 || errors.Is(existingErr, sql.ErrNoRows) || existingReason != reason
	recordedAt := now
	if changed {
		if _, err := tx.ExecContext(ctx, `INSERT INTO universe_exclusions
			(work_id, universe_id, reason, recorded_at_utc) VALUES (?, ?, ?, ?)
			ON CONFLICT(work_id, universe_id) DO UPDATE SET reason = excluded.reason, recorded_at_utc = excluded.recorded_at_utc`,
			workID, universeID, reason, formatUniverseTime(now)); err != nil {
			return domain.UniverseExclusion{}, fmt.Errorf("record Universe membership exclusion: %w", err)
		}
		if err := touchUniverseTx(ctx, tx, universeID, now); err != nil {
			return domain.UniverseExclusion{}, err
		}
	} else {
		recordedAt, err = parseUniverseTime(existingRecordedAt)
		if err != nil {
			return domain.UniverseExclusion{}, fmt.Errorf("decode existing Universe exclusion timestamp: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.UniverseExclusion{}, fmt.Errorf("commit Universe membership removal: %w", err)
	}
	return domain.UniverseExclusion{UniverseID: universeID, WorkID: workID, Reason: reason, RecordedAt: recordedAt}, nil
}

// IsUniverseWorkExcluded reports whether a prior explicit decision blocks the
// Work/Universe pair from silent re-addition.
func (r *SQLiteRepository) IsUniverseWorkExcluded(ctx context.Context, workID domain.WorkID, universeID domain.UniverseID) (bool, error) {
	var excluded bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM universe_exclusions WHERE work_id = ? AND universe_id = ?)`,
		workID, universeID).Scan(&excluded); err != nil {
		return false, fmt.Errorf("check Universe exclusion for Work %q: %w", workID, err)
	}
	return excluded, nil
}

// AddUniverseAlias stores a normalized localized alternate name. Automatic
// refreshes may update automatic aliases but cannot replace manual authority.
func (r *SQLiteRepository) AddUniverseAlias(ctx context.Context, alias domain.UniverseAlias) (domain.UniverseAlias, error) {
	if err := validateBoundedText("Universe ID", string(alias.UniverseID), 128); err != nil {
		return domain.UniverseAlias{}, err
	}
	normalizedAlias, err := normalizeUniverseAlias(alias.Alias)
	if err != nil {
		return domain.UniverseAlias{}, err
	}
	if err := validateBoundedText("alias provenance", alias.Provenance, 64); err != nil {
		return domain.UniverseAlias{}, err
	}
	alias.Alias = normalizedAlias
	if alias.Language != nil {
		language, err := normalizeAliasLanguage(*alias.Language)
		if err != nil {
			return domain.UniverseAlias{}, err
		}
		alias.Language = &language
	}
	if alias.ProviderVersion != nil {
		if err := validateBoundedText("alias provider version", *alias.ProviderVersion, 128); err != nil {
			return domain.UniverseAlias{}, err
		}
	}
	if alias.Provenance == string(domain.UniverseMembershipProvenanceManual) {
		alias.ConfirmedByUser = true
		if alias.Confidence == 0 {
			alias.Confidence = 1
		}
	}
	if math.IsNaN(alias.Confidence) || math.IsInf(alias.Confidence, 0) || alias.Confidence < 0 || alias.Confidence > 1 {
		return domain.UniverseAlias{}, fmt.Errorf("alias confidence must be between 0 and 1")
	}
	now := time.Now().UTC()
	if alias.CreatedAt.IsZero() {
		alias.CreatedAt = now
	} else {
		alias.CreatedAt = alias.CreatedAt.UTC()
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.UniverseAlias{}, fmt.Errorf("begin Universe alias addition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var language any
	if alias.Language != nil {
		language = *alias.Language
	}
	inserted, err := tx.ExecContext(ctx, `INSERT INTO universe_aliases
		(universe_id, alias, language_code, provenance, provider_version, confidence, confirmed_by_user, created_at_utc)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO UPDATE SET
			provenance = excluded.provenance,
			provider_version = excluded.provider_version,
			confidence = excluded.confidence,
			confirmed_by_user = excluded.confirmed_by_user
		WHERE ((universe_aliases.confirmed_by_user = 0 AND universe_aliases.provenance <> 'manual')
			OR excluded.confirmed_by_user = 1 OR excluded.provenance = 'manual')
		AND (universe_aliases.provenance IS NOT excluded.provenance
			OR universe_aliases.provider_version IS NOT excluded.provider_version
			OR universe_aliases.confidence IS NOT excluded.confidence
			OR universe_aliases.confirmed_by_user IS NOT excluded.confirmed_by_user)`,
		alias.UniverseID, alias.Alias, language, alias.Provenance, nullableStringPointer(alias.ProviderVersion),
		alias.Confidence, alias.ConfirmedByUser, formatUniverseTime(alias.CreatedAt))
	if err != nil {
		return domain.UniverseAlias{}, fmt.Errorf("add alias to Universe %q: %w", alias.UniverseID, err)
	}
	insertedCount, err := inserted.RowsAffected()
	if err != nil {
		return domain.UniverseAlias{}, fmt.Errorf("read Universe alias addition result: %w", err)
	}
	if insertedCount > 0 {
		if err := touchUniverseTx(ctx, tx, alias.UniverseID, now); err != nil {
			return domain.UniverseAlias{}, err
		}
	}
	var storedLanguage, providerVersion sql.NullString
	var createdAt string
	if err := tx.QueryRowContext(ctx, `SELECT language_code, provenance, provider_version, confidence, confirmed_by_user, created_at_utc
		FROM universe_aliases WHERE universe_id = ? AND alias = ? AND language_code IS ?`,
		alias.UniverseID, alias.Alias, language).Scan(&storedLanguage, &alias.Provenance, &providerVersion,
		&alias.Confidence, &alias.ConfirmedByUser, &createdAt); err != nil {
		return domain.UniverseAlias{}, fmt.Errorf("load alias for Universe %q: %w", alias.UniverseID, err)
	}
	alias.Language = nullStringPointer(storedLanguage)
	alias.ProviderVersion = nullStringPointer(providerVersion)
	alias.CreatedAt, err = parseUniverseTime(createdAt)
	if err != nil {
		return domain.UniverseAlias{}, fmt.Errorf("decode alias timestamp for Universe %q: %w", alias.UniverseID, err)
	}
	if err := tx.Commit(); err != nil {
		return domain.UniverseAlias{}, fmt.Errorf("commit Universe alias addition: %w", err)
	}
	return alias, nil
}

// RemoveUniverseAlias removes every language variant of one normalized alias
// and reports missing aliases. Use RemoveLocalizedUniverseAlias to remove only
// one locale.
func (r *SQLiteRepository) RemoveUniverseAlias(ctx context.Context, universeID domain.UniverseID, alias string) error {
	normalizedAlias, err := normalizeUniverseAlias(alias)
	if err != nil {
		return err
	}
	return r.removeUniverseAlias(ctx, universeID, normalizedAlias, nil, false)
}

// RemoveLocalizedUniverseAlias removes one exact normalized alias/language
// identity. A nil language selects only aliases whose language is unknown.
func (r *SQLiteRepository) RemoveLocalizedUniverseAlias(ctx context.Context, universeID domain.UniverseID, alias string, language *string) error {
	normalizedAlias, err := normalizeUniverseAlias(alias)
	if err != nil {
		return err
	}
	if language != nil {
		normalizedLanguage, err := normalizeAliasLanguage(*language)
		if err != nil {
			return err
		}
		language = &normalizedLanguage
	}
	return r.removeUniverseAlias(ctx, universeID, normalizedAlias, language, true)
}

func (r *SQLiteRepository) removeUniverseAlias(ctx context.Context, universeID domain.UniverseID, alias string, language *string, localized bool) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Universe alias removal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	query := `DELETE FROM universe_aliases WHERE universe_id = ? AND alias = ?`
	arguments := []any{universeID, alias}
	if localized {
		query += ` AND language_code IS ?`
		arguments = append(arguments, nullableStringPointer(language))
	}
	result, err := tx.ExecContext(ctx, query, arguments...)
	if err != nil {
		return fmt.Errorf("remove alias from Universe %q: %w", universeID, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read alias removal result: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("%w: %q", ErrUniverseAliasNotFound, alias)
	}
	if err := touchUniverseTx(ctx, tx, universeID, time.Now().UTC()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Universe alias removal: %w", err)
	}
	return nil
}

// CreateWorkCollection creates a durable typed grouping such as a Series.
func (r *SQLiteRepository) CreateWorkCollection(ctx context.Context, collection domain.WorkCollection) error {
	if err := validateBoundedText("Work collection ID", string(collection.ID), 128); err != nil {
		return err
	}
	if err := validateBoundedText("Work collection title", collection.Title, 512); err != nil {
		return err
	}
	if !collection.Type.Valid() {
		return fmt.Errorf("unsupported Work collection type %q", collection.Type)
	}
	if _, err := r.db.ExecContext(ctx, `INSERT INTO work_collections (id, title, collection_type, created_at_utc)
		VALUES (?, ?, ?, ?)`, collection.ID, strings.TrimSpace(collection.Title), collection.Type, formatUniverseTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("create Work collection %q: %w", collection.ID, err)
	}
	return nil
}

// CreateOrGetWorkCollectionByExternalIdentity binds an exact Wikidata QID to
// one stable opaque local collection ID. Refreshes return existing metadata
// unchanged so provider title/type updates cannot overwrite local identity.
func (r *SQLiteRepository) CreateOrGetWorkCollectionByExternalIdentity(
	ctx context.Context, provider, externalID, title string, collectionType domain.WorkCollectionType,
) (domain.WorkCollection, error) {
	if err := validateWorkCollectionExternalIdentity(provider, externalID); err != nil {
		return domain.WorkCollection{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.WorkCollection{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.WorkCollection{}, fmt.Errorf("begin Work collection identity binding: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	collection, err := getWorkCollectionByExternalIdentity(ctx, tx, provider, externalID)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return domain.WorkCollection{}, fmt.Errorf("commit existing Work collection identity lookup: %w", err)
		}
		return collection, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.WorkCollection{}, err
	}
	if err := validateBoundedText("Work collection title", title, 512); err != nil {
		return domain.WorkCollection{}, err
	}
	if !collectionType.Valid() {
		return domain.WorkCollection{}, fmt.Errorf("unsupported Work collection type %q", collectionType)
	}
	localID := domain.WorkCollectionID(stableCatalogID("collection", provider, externalID))
	if _, err := tx.ExecContext(ctx, `INSERT INTO work_collections (id, title, collection_type, created_at_utc)
		VALUES (?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`, localID, strings.TrimSpace(title), collectionType,
		formatUniverseTime(time.Now().UTC())); err != nil {
		return domain.WorkCollection{}, fmt.Errorf("create QID-backed Work collection %q: %w", externalID, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO work_collection_external_identities (provider, external_id, collection_id)
		VALUES (?, ?, ?) ON CONFLICT(provider, external_id) DO NOTHING`, provider, externalID, localID); err != nil {
		return domain.WorkCollection{}, fmt.Errorf("bind Work collection external identity %s:%s: %w", provider, externalID, err)
	}
	collection, err = getWorkCollectionByExternalIdentity(ctx, tx, provider, externalID)
	if err != nil {
		return domain.WorkCollection{}, fmt.Errorf("read QID-backed Work collection %q: %w", externalID, err)
	}
	if err := tx.Commit(); err != nil {
		return domain.WorkCollection{}, fmt.Errorf("commit QID-backed Work collection %q: %w", externalID, err)
	}
	return collection, nil
}

// GetWorkCollectionByExternalIdentity resolves one exact provider identity;
// it never falls back to title or local-ID matching.
func (r *SQLiteRepository) GetWorkCollectionByExternalIdentity(ctx context.Context, provider, externalID string) (domain.WorkCollection, error) {
	if err := validateWorkCollectionExternalIdentity(provider, externalID); err != nil {
		return domain.WorkCollection{}, err
	}
	collection, err := getWorkCollectionByExternalIdentity(ctx, r.db, provider, externalID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WorkCollection{}, fmt.Errorf("%w: provider %s external ID %s", ErrWorkCollectionNotFound, provider, externalID)
	}
	if err != nil {
		return domain.WorkCollection{}, fmt.Errorf("load Work collection by external identity %s:%s: %w", provider, externalID, err)
	}
	return collection, nil
}

func getWorkCollectionByExternalIdentity(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, provider, externalID string) (domain.WorkCollection, error) {
	var collection domain.WorkCollection
	err := queryer.QueryRowContext(ctx, `SELECT c.id, c.title, c.collection_type
		FROM work_collection_external_identities i JOIN work_collections c ON c.id = i.collection_id
		WHERE i.provider = ? AND i.external_id = ?`, provider, externalID).
		Scan(&collection.ID, &collection.Title, &collection.Type)
	return collection, err
}

func validateWorkCollectionExternalIdentity(provider, externalID string) error {
	if provider != "wikidata" || !validCatalogWikidataEntityID(externalID) {
		return ErrInvalidWorkCollectionIdentity
	}
	return nil
}

func validCatalogWikidataEntityID(value string) bool {
	if len(value) < 2 || len(value) > 20 || value[0] != 'Q' || value[1] == '0' {
		return false
	}
	for _, character := range value[1:] {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

// ResolveWikidataWork returns a local Work only when allowlisted parsed
// identifiers point to exactly one distinct Work. Search title candidates
// are intentionally absent from this identity boundary.
func (r *SQLiteRepository) ResolveWikidataWork(ctx context.Context, metadata providers.RelationshipMetadata) (WorkGraph, error) {
	if metadata.Provider != "wikidata" {
		return WorkGraph{}, providers.ErrInvalidRelationshipMetadata
	}
	if err := metadata.Validate(); err != nil {
		return WorkGraph{}, err
	}
	matches := make(map[domain.WorkID]struct{})
	for _, identifier := range metadata.Identifiers {
		provider, externalID, ok := identifier.LocalProviderIdentity()
		if !ok {
			continue
		}
		var workID domain.WorkID
		err := r.db.QueryRowContext(ctx, `SELECT work_id FROM external_identities WHERE provider = ? AND external_id = ?`, provider, externalID).Scan(&workID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return WorkGraph{}, fmt.Errorf("resolve exact provider identity %s:%s: %w", provider, externalID, err)
		}
		matches[workID] = struct{}{}
		if len(matches) > 1 {
			return WorkGraph{}, ErrWikidataWorkIdentityAmbiguous
		}
	}
	if len(matches) == 0 {
		return WorkGraph{}, ErrWikidataWorkIdentityNotFound
	}
	for workID := range matches {
		return r.GetWork(ctx, workID)
	}
	return WorkGraph{}, ErrWikidataWorkIdentityNotFound
}

// GetWorkCollection loads a stable Series or collection identity by local ID.
func (r *SQLiteRepository) GetWorkCollection(ctx context.Context, id domain.WorkCollectionID) (domain.WorkCollection, error) {
	var collection domain.WorkCollection
	collection.ID = id
	if err := r.db.QueryRowContext(ctx, `SELECT title, collection_type FROM work_collections WHERE id = ?`, id).
		Scan(&collection.Title, &collection.Type); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.WorkCollection{}, fmt.Errorf("%w: %s", ErrWorkCollectionNotFound, id)
		}
		return domain.WorkCollection{}, fmt.Errorf("load Work collection %q: %w", id, err)
	}
	return collection, nil
}

// SetWorkRelation stores one explicit edge. A repeated provider refresh is
// idempotent, and it cannot replace a manually confirmed edge.
func (r *SQLiteRepository) SetWorkRelation(ctx context.Context, relation domain.WorkRelation) (domain.WorkRelation, error) {
	if !relation.Valid() {
		return domain.WorkRelation{}, fmt.Errorf("invalid Work relation")
	}
	if err := validateBoundedText("Work ID", string(relation.WorkID), 128); err != nil {
		return domain.WorkRelation{}, err
	}
	if relation.TargetWorkID != nil {
		if err := validateBoundedText("relation target Work ID", string(*relation.TargetWorkID), 128); err != nil {
			return domain.WorkRelation{}, err
		}
	}
	if relation.TargetCollectionID != nil {
		if err := validateBoundedText("relation target collection ID", string(*relation.TargetCollectionID), 128); err != nil {
			return domain.WorkRelation{}, err
		}
	}
	if relation.ProviderVersion != nil {
		if err := validateBoundedText("relation provider version", *relation.ProviderVersion, 128); err != nil {
			return domain.WorkRelation{}, err
		}
	}
	if err := validateOptionalBoundedText("relation ordinal", relation.Ordinal, 64); err != nil {
		return domain.WorkRelation{}, err
	}
	if err := validateOptionalText("relation evidence", relation.Evidence, 2048); err != nil {
		return domain.WorkRelation{}, err
	}
	if math.IsNaN(relation.Confidence) || math.IsInf(relation.Confidence, 0) {
		return domain.WorkRelation{}, fmt.Errorf("relation confidence must be between 0 and 1")
	}

	var targetWork, targetCollection, ordinal any
	if relation.TargetWorkID != nil {
		targetWork = *relation.TargetWorkID
	}
	if relation.TargetCollectionID != nil {
		targetCollection = *relation.TargetCollectionID
	}
	if relation.Ordinal != nil {
		ordinal = *relation.Ordinal
	}
	if relation.Provenance == domain.WorkRelationProvenanceManual {
		relation.ConfirmedByUser = true
		if relation.Confidence == 0 {
			relation.Confidence = 1
		}
	}
	relationID := workRelationStorageID(relation)
	now := formatUniverseTime(time.Now().UTC())
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.WorkRelation{}, fmt.Errorf("begin Work relation change: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO work_relations
		(id, work_id, relation_type, target_work_id, target_collection_id, ordinal, provenance, provider_version,
		confidence, evidence, confirmed_by_user, created_at_utc)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			ordinal = excluded.ordinal,
			provenance = excluded.provenance,
			provider_version = excluded.provider_version,
			confidence = excluded.confidence,
			evidence = excluded.evidence,
			confirmed_by_user = excluded.confirmed_by_user
		WHERE ((work_relations.confirmed_by_user = 0 AND work_relations.provenance <> 'manual')
			OR excluded.confirmed_by_user = 1 OR excluded.provenance = 'manual')
			AND (work_relations.ordinal IS NOT excluded.ordinal
				OR work_relations.provenance IS NOT excluded.provenance
				OR work_relations.provider_version IS NOT excluded.provider_version
				OR work_relations.confidence IS NOT excluded.confidence
				OR work_relations.evidence IS NOT excluded.evidence
				OR work_relations.confirmed_by_user IS NOT excluded.confirmed_by_user)`,
		relationID, relation.WorkID, relation.Type, targetWork, targetCollection, ordinal,
		relation.Provenance, nullableStringPointer(relation.ProviderVersion), relation.Confidence,
		relation.Evidence, relation.ConfirmedByUser, now)
	if err != nil {
		return domain.WorkRelation{}, fmt.Errorf("set Work relation for %q: %w", relation.WorkID, err)
	}
	stored, err := scanWorkRelation(tx.QueryRowContext(ctx, `SELECT work_id, relation_type, target_work_id,
		target_collection_id, ordinal, provenance, provider_version, confidence, evidence, confirmed_by_user
		FROM work_relations WHERE id = ?`, relationID))
	if err != nil {
		return domain.WorkRelation{}, fmt.Errorf("load stored Work relation for %q: %w", relation.WorkID, err)
	}
	if err := tx.Commit(); err != nil {
		return domain.WorkRelation{}, fmt.Errorf("commit Work relation change: %w", err)
	}
	return stored, nil
}

// GetWorkRelations returns explicit outgoing Work relations in stable order.
func (r *SQLiteRepository) GetWorkRelations(ctx context.Context, workID domain.WorkID) ([]domain.WorkRelation, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT work_id, relation_type, target_work_id, target_collection_id,
		ordinal, provenance, provider_version, confidence, evidence, confirmed_by_user
		FROM work_relations WHERE work_id = ? ORDER BY relation_type, COALESCE(target_work_id, target_collection_id)`, workID)
	if err != nil {
		return nil, fmt.Errorf("load Work relations for %q: %w", workID, err)
	}
	defer rows.Close()
	var relations []domain.WorkRelation
	for rows.Next() {
		relation, err := scanWorkRelation(rows)
		if err != nil {
			return nil, fmt.Errorf("decode Work relation for %q: %w", workID, err)
		}
		relations = append(relations, relation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Work relations for %q: %w", workID, err)
	}
	return relations, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanWorkRelation(row rowScanner) (domain.WorkRelation, error) {
	var relation domain.WorkRelation
	var targetWork, targetCollection, ordinal, providerVersion sql.NullString
	var provenance string
	if err := row.Scan(&relation.WorkID, &relation.Type, &targetWork, &targetCollection, &ordinal,
		&provenance, &providerVersion, &relation.Confidence, &relation.Evidence, &relation.ConfirmedByUser); err != nil {
		return domain.WorkRelation{}, err
	}
	relation.Provenance = domain.WorkRelationProvenance(provenance)
	relation.TargetWorkID = workIDPointer(targetWork)
	relation.TargetCollectionID = workCollectionIDPointer(targetCollection)
	relation.Ordinal = nullStringPointer(ordinal)
	relation.ProviderVersion = nullStringPointer(providerVersion)
	return relation, nil
}

func workIDPointer(value sql.NullString) *domain.WorkID {
	if !value.Valid {
		return nil
	}
	id := domain.WorkID(value.String)
	return &id
}

func workCollectionIDPointer(value sql.NullString) *domain.WorkCollectionID {
	if !value.Valid {
		return nil
	}
	id := domain.WorkCollectionID(value.String)
	return &id
}

func workRelationStorageID(relation domain.WorkRelation) string {
	var targetKind, targetID string
	if relation.TargetWorkID != nil {
		targetKind, targetID = "work", string(*relation.TargetWorkID)
	} else {
		targetKind, targetID = "collection", string(*relation.TargetCollectionID)
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%d:%s%d:%s%d:%s", len(relation.WorkID), relation.WorkID,
		len(relation.Type), relation.Type, len(targetKind), targetKind, len(targetID), targetID)))
	return "workrel-" + hex.EncodeToString(hash[:])
}

// DeleteUniverse removes a Universe and its relationships, aliases, and
// exclusions through migration-defined foreign-key cascades. Works survive.
func (r *SQLiteRepository) DeleteUniverse(ctx context.Context, id domain.UniverseID) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM universes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete Universe %q: %w", id, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read Universe deletion result: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("%w: %s", ErrUniverseNotFound, id)
	}
	return nil
}

func validateBoundedText(label, value string, maximum int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", label)
	}
	if strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > maximum {
		return fmt.Errorf("%s must contain 1 to %d characters", label, maximum)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s must not contain control characters", label)
		}
	}
	return nil
}

func normalizeUniverseAlias(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("Universe alias must be valid UTF-8")
	}
	for _, character := range value {
		if unicode.IsControl(character) && !unicode.IsSpace(character) {
			return "", fmt.Errorf("Universe alias must not contain control characters")
		}
	}
	normalized := strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	if normalized == "" || utf8.RuneCountInString(normalized) > 256 {
		return "", fmt.Errorf("Universe alias must contain 1 to 256 characters")
	}
	return normalized, nil
}

func normalizeAliasLanguage(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" || len(normalized) > 35 {
		return "", fmt.Errorf("alias language must contain 1 to 35 characters")
	}
	for _, character := range normalized {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return "", fmt.Errorf("alias language must be a BCP 47 language tag")
		}
	}
	return normalized, nil
}

func validateOptionalBoundedText(label string, value *string, maximum int) error {
	if value == nil {
		return nil
	}
	if !utf8.ValidString(*value) || strings.TrimSpace(*value) == "" || utf8.RuneCountInString(*value) > maximum {
		return fmt.Errorf("%s must contain 1 to %d valid UTF-8 characters", label, maximum)
	}
	for _, character := range *value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s must not contain control characters", label)
		}
	}
	return nil
}

func validateOptionalText(label, value string, maximum int) error {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maximum {
		return fmt.Errorf("%s must be valid UTF-8 and contain at most %d characters", label, maximum)
	}
	for _, character := range value {
		if unicode.IsControl(character) && !unicode.IsSpace(character) {
			return fmt.Errorf("%s must not contain control characters", label)
		}
	}
	return nil
}

func nullableStringPointer(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func normalizeUniverseText(label string, value *string, maximum int) (*string, error) {
	if value == nil {
		return nil, nil
	}
	if !utf8.ValidString(*value) {
		return nil, fmt.Errorf("%s must be valid UTF-8", label)
	}
	for _, character := range *value {
		if unicode.IsControl(character) && !unicode.IsSpace(character) {
			return nil, fmt.Errorf("%s must not contain control characters", label)
		}
	}
	normalized := strings.Join(strings.Fields(strings.TrimSpace(*value)), " ")
	if normalized == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(normalized) > maximum {
		return nil, fmt.Errorf("%s must contain at most %d characters", label, maximum)
	}
	return &normalized, nil
}

func nullStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	result := value.String
	return &result
}

func normalizeWorkSummary(summary string) string {
	if !utf8.ValidString(summary) || utf8.RuneCountInString(summary) > 2048 {
		return ""
	}
	for _, character := range summary {
		if unicode.IsControl(character) && !unicode.IsSpace(character) || character == '<' || character == '>' {
			return ""
		}
	}
	return strings.Join(strings.Fields(strings.TrimSpace(summary)), " ")
}

func formatUniverseTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func touchUniverseTx(ctx context.Context, tx *sql.Tx, id domain.UniverseID, at time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE universes SET updated_at_utc = ? WHERE id = ?`, formatUniverseTime(at), id)
	if err != nil {
		return fmt.Errorf("update Universe %q timestamp: %w", id, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read Universe %q timestamp update result: %w", id, err)
	}
	if count == 0 {
		return fmt.Errorf("%w: %s", ErrUniverseNotFound, id)
	}
	return nil
}

func parseUniverseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}
