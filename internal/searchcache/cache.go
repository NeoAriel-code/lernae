// Package searchcache decorates metadata search sources with a small SQLite
// cache of normalized results.
package searchcache

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"lernae/internal/domain"
	"lernae/internal/providers"
)

const defaultTTL = 15 * time.Minute

type Cache struct {
	source   providers.MetadataSource
	db       *sql.DB
	provider string
	ttl      time.Duration
	now      func() time.Time
}

var _ providers.MetadataSource = (*Cache)(nil)

// New wraps source with a provider-specific normalized-result cache. The TTL
// is intentionally short and cache failures never turn a successful provider
// search into a failed search.
func New(source providers.MetadataSource, db *sql.DB, provider string) *Cache {
	return &Cache{
		source: source, db: db, provider: strings.ToLower(strings.TrimSpace(provider)),
		ttl: defaultTTL, now: time.Now,
	}
}

func (cache *Cache) SearchWorks(ctx context.Context, query string) ([]domain.MetadataSearchResult, error) {
	key := normalizeQuery(query)
	if key != "" && cache.provider != "" {
		if results, found, err := cache.read(ctx, key); err == nil && found {
			return results, nil
		}
	}

	results, err := cache.source.SearchWorks(ctx, query)
	if err != nil {
		return nil, err
	}
	if key != "" && cache.provider != "" && cache.validResults(results) {
		// Cache persistence is best-effort: the provider result remains usable if
		// SQLite is temporarily unavailable or the cache file is read-only.
		_ = cache.write(ctx, key, results)
	}
	return results, nil
}

func (cache *Cache) read(ctx context.Context, query string) ([]domain.MetadataSearchResult, bool, error) {
	var encoded, fetchedText, expiresText string
	err := cache.db.QueryRowContext(ctx, `SELECT results_json, fetched_at_utc, expires_at_utc
		FROM metadata_search_cache WHERE provider = ? AND normalized_query = ?`, cache.provider, query).
		Scan(&encoded, &fetchedText, &expiresText)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	fetchedAt, fetchedErr := time.Parse(time.RFC3339Nano, fetchedText)
	expiresAt, expiresErr := time.Parse(time.RFC3339Nano, expiresText)
	if fetchedErr != nil || expiresErr != nil || !expiresAt.After(fetchedAt) {
		cache.evict(ctx, query)
		return nil, false, nil
	}
	if !cache.now().Before(expiresAt) {
		cache.evict(ctx, query)
		return nil, false, nil
	}
	var results []domain.MetadataSearchResult
	if err := json.Unmarshal([]byte(encoded), &results); err != nil || !cache.validResults(results) {
		cache.evict(ctx, query)
		return nil, false, nil
	}
	return results, true, nil
}

func (cache *Cache) write(ctx context.Context, query string, results []domain.MetadataSearchResult) error {
	encoded, err := json.Marshal(results)
	if err != nil {
		return err
	}
	fetchedAt := cache.now().UTC()
	expiresAt := fetchedAt.Add(cache.ttl)
	_, err = cache.db.ExecContext(ctx, `INSERT INTO metadata_search_cache
		(provider, normalized_query, results_json, fetched_at_utc, expires_at_utc)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(provider, normalized_query) DO UPDATE SET
			results_json = excluded.results_json,
			fetched_at_utc = excluded.fetched_at_utc,
			expires_at_utc = excluded.expires_at_utc`,
		cache.provider, query, string(encoded), fetchedAt.Format(time.RFC3339Nano), expiresAt.Format(time.RFC3339Nano))
	return err
}

func (cache *Cache) evict(ctx context.Context, query string) {
	_, _ = cache.db.ExecContext(ctx, `DELETE FROM metadata_search_cache
		WHERE provider = ? AND normalized_query = ?`, cache.provider, query)
}

func (cache *Cache) validResults(results []domain.MetadataSearchResult) bool {
	for _, result := range results {
		if result.Provider != cache.provider || strings.TrimSpace(result.ExternalID) == "" || strings.TrimSpace(result.Title) == "" {
			return false
		}
	}
	return true
}

func normalizeQuery(query string) string {
	if !utf8.ValidString(query) {
		return ""
	}
	for _, character := range query {
		if unicode.IsControl(character) || unicode.In(character, unicode.Zl, unicode.Zp) {
			// Do not allow newline-like query controls to collapse onto the same
			// key as a valid search string.
			return ""
		}
	}
	return strings.ToLower(strings.Join(strings.Fields(query), " "))
}
