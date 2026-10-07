package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"lernae/internal/providers"
)

const maxRelationshipCacheBytes = 1 << 20

var ErrInvalidRelationshipCacheEntry = errors.New("invalid relationship metadata cache entry")

type SQLiteRelationshipMetadataCache struct {
	db *sql.DB
}

type relationshipMetadataPayload struct {
	Labels      []providers.RelationshipLocalizedValue `json:"labels,omitempty"`
	Aliases     []providers.RelationshipLocalizedValue `json:"aliases,omitempty"`
	Claims      []providers.RelationshipClaim          `json:"claims,omitempty"`
	Identifiers []providers.RelationshipIdentifier     `json:"identifiers,omitempty"`
}

func NewSQLiteRelationshipMetadataCache(db *sql.DB) *SQLiteRelationshipMetadataCache {
	return &SQLiteRelationshipMetadataCache{db: db}
}

// Put replaces one provider snapshot only. Manual relations and aliases are
// stored in separate catalog tables and are never rewritten through this cache.
func (cache *SQLiteRelationshipMetadataCache) Put(ctx context.Context, metadata providers.RelationshipMetadata, ttl time.Duration) error {
	if cache == nil || cache.db == nil || ttl <= 0 {
		return ErrInvalidRelationshipCacheEntry
	}
	if err := metadata.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRelationshipCacheEntry, err)
	}
	expiresAt := metadata.FetchedAt.UTC().Add(ttl)
	if !expiresAt.After(metadata.FetchedAt.UTC()) || expiresAt.Year() < 1 || expiresAt.Year() > 9999 {
		return ErrInvalidRelationshipCacheEntry
	}
	payload, err := json.Marshal(relationshipMetadataPayload{
		Labels: metadata.Labels, Aliases: metadata.Aliases, Claims: metadata.Claims, Identifiers: metadata.Identifiers,
	})
	if err != nil {
		return fmt.Errorf("encode normalized relationship metadata: %w", err)
	}
	if len(payload) > maxRelationshipCacheBytes {
		return ErrInvalidRelationshipCacheEntry
	}
	_, err = cache.db.ExecContext(ctx, `INSERT INTO relationship_provider_cache
		(provider, external_id, entity_revision, parser_version, fetched_at_utc, expires_at_utc, normalized_json)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, external_id) DO UPDATE SET
		entity_revision = excluded.entity_revision,
		parser_version = excluded.parser_version,
		fetched_at_utc = excluded.fetched_at_utc,
		expires_at_utc = excluded.expires_at_utc,
		normalized_json = excluded.normalized_json`,
		metadata.Provider, metadata.ExternalID, metadata.EntityRevision, metadata.ParserVersion,
		formatRelationshipCacheTime(metadata.FetchedAt), formatRelationshipCacheTime(expiresAt), string(payload))
	if err != nil {
		return fmt.Errorf("store parsed relationship metadata for %s:%s: %w", metadata.Provider, metadata.ExternalID, err)
	}
	return nil
}

// Get returns a hit only while both the caller's parser version and the
// explicit TTL timestamp still match the cached snapshot.
func (cache *SQLiteRelationshipMetadataCache) Get(ctx context.Context, provider, externalID, parserVersion string, now time.Time) (providers.RelationshipMetadata, bool, error) {
	if cache == nil || cache.db == nil || provider == "" || externalID == "" || parserVersion == "" || now.IsZero() {
		return providers.RelationshipMetadata{}, false, ErrInvalidRelationshipCacheEntry
	}
	var revision, storedParserVersion, fetchedAtText, expiresAtText, normalizedJSON string
	err := cache.db.QueryRowContext(ctx, `SELECT entity_revision, parser_version, fetched_at_utc, expires_at_utc, normalized_json
		FROM relationship_provider_cache WHERE provider = ? AND external_id = ?`, provider, externalID).
		Scan(&revision, &storedParserVersion, &fetchedAtText, &expiresAtText, &normalizedJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return providers.RelationshipMetadata{}, false, nil
	}
	if err != nil {
		return providers.RelationshipMetadata{}, false, fmt.Errorf("read parsed relationship metadata: %w", err)
	}
	if storedParserVersion != parserVersion {
		return providers.RelationshipMetadata{}, false, nil
	}
	fetchedAt, err := time.Parse(time.RFC3339Nano, fetchedAtText)
	if err != nil {
		return providers.RelationshipMetadata{}, false, fmt.Errorf("decode relationship cache fetch time: %w", err)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expiresAtText)
	if err != nil {
		return providers.RelationshipMetadata{}, false, fmt.Errorf("decode relationship cache expiration time: %w", err)
	}
	if !now.UTC().Before(expiresAt) {
		return providers.RelationshipMetadata{}, false, nil
	}
	var payload relationshipMetadataPayload
	if len(normalizedJSON) > maxRelationshipCacheBytes || json.Unmarshal([]byte(normalizedJSON), &payload) != nil {
		return providers.RelationshipMetadata{}, false, ErrInvalidRelationshipCacheEntry
	}
	metadata := providers.RelationshipMetadata{
		Provider: provider, ExternalID: externalID, EntityRevision: revision, ParserVersion: storedParserVersion,
		FetchedAt: fetchedAt.UTC(), Labels: payload.Labels, Aliases: payload.Aliases, Claims: payload.Claims,
		Identifiers: payload.Identifiers,
	}
	if err := metadata.Validate(); err != nil {
		return providers.RelationshipMetadata{}, false, fmt.Errorf("validate parsed relationship cache data: %w", err)
	}
	return metadata, true, nil
}

func formatRelationshipCacheTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
