package searchcache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"lernae/internal/domain"
	"lernae/internal/providers"
)

const (
	CurrentResultSchemaVersion     = "v1"
	OpenLibraryResultSchemaVersion = "v2"
	TVMazeResultSchemaVersion      = "v2"
	defaultMaxProviderCacheKeys    = 500
	defaultStaleGrace              = 24 * time.Hour
)

// ResultSchemaVersion returns the normalized-result cache version for one
// provider. Open Library's nested representative-edition metadata and
// TVMaze's normalized Search channels change their cached SearchResult shapes.
func ResultSchemaVersion(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openlibrary":
		return OpenLibraryResultSchemaVersion
	case "tvmaze":
		return TVMazeResultSchemaVersion
	default:
		return CurrentResultSchemaVersion
	}
}

var providerCacheCapacityMu sync.Mutex

// ProviderCache is a best-effort durable cache for one universal-search
// provider. Cached rows contain only normalized search results.
type ProviderCache struct {
	source        providers.SearchProvider
	db            *sql.DB
	provider      string
	schemaVersion string
	ttl           time.Duration
	staleGrace    time.Duration
	maxEntries    int
	now           func() time.Time
}

var _ providers.SearchProvider = (*ProviderCache)(nil)

type cachedSearchResults struct {
	results []domain.MetadataSearchResult
	stale   bool
}

type searchCacheIdentityProvider interface {
	SearchCacheIdentity() string
}

func NewProviderCache(source providers.SearchProvider, db *sql.DB, provider, schemaVersion string) *ProviderCache {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" && source != nil {
		provider = strings.ToLower(strings.TrimSpace(source.Name()))
	}
	schemaVersion = strings.TrimSpace(schemaVersion)
	if schemaVersion == "" {
		schemaVersion = ResultSchemaVersion(provider)
	}
	return &ProviderCache{
		source: source, db: db, provider: provider, schemaVersion: schemaVersion,
		ttl: defaultTTL, staleGrace: defaultStaleGrace, maxEntries: defaultMaxProviderCacheKeys, now: time.Now,
	}
}

func (cache *ProviderCache) Name() string { return cache.provider }

func (cache *ProviderCache) Search(ctx context.Context, query string, limit int) ([]domain.MetadataSearchResult, error) {
	if err := providers.ValidateSearchRequest(query, limit); err != nil {
		return nil, providers.ErrSearchInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	normalizedQuery := normalizeQuery(query)
	cacheVersion := cache.versionForCurrentIdentity()
	var stale []domain.MetadataSearchResult
	if normalizedQuery != "" && cache.provider != "" && cacheVersion != "" {
		cached, found, err := cache.read(ctx, normalizedQuery, cacheVersion)
		if err == nil && found && cache.versionForCurrentIdentity() == cacheVersion {
			if cached.stale {
				stale = cached.results
			} else {
				return limitResults(cached.results, limit), nil
			}
		}
	}
	if cache.source == nil {
		if stale != nil {
			return limitResults(stale, limit), providers.ErrSearchStale
		}
		return nil, providers.ErrSearchUnavailable
	}
	// Store a request-limit-independent result set so later callers sharing the
	// provider/query/schema key can request a larger page without inheriting a
	// prior caller's truncation.
	results, sourceErr := cache.source.Search(ctx, query, providers.MaxProviderSearchResults)
	if sourceErr != nil {
		if stale != nil && !errors.Is(sourceErr, context.Canceled) && !errors.Is(ctx.Err(), context.Canceled) {
			return limitResults(stale, limit), providers.ErrSearchStale
		}
		return nil, sourceErr
	}
	results = limitResults(results, providers.MaxProviderSearchResults)
	if normalizedQuery != "" && cache.provider != "" && cacheVersion != "" && cache.versionForCurrentIdentity() == cacheVersion && cache.validResults(results) {
		// A cache write failure must not convert a successful provider result
		// into an unsuccessful search.
		_ = cache.write(ctx, normalizedQuery, results, cacheVersion)
	}
	return limitResults(results, limit), nil
}

func (cache *ProviderCache) versionForCurrentIdentity() string {
	version := cache.schemaVersion
	identityProvider, ok := cache.source.(searchCacheIdentityProvider)
	if !ok {
		return version
	}
	identity := strings.TrimSpace(identityProvider.SearchCacheIdentity())
	if identity == "" || len(version)+len(identity)+1 > 64 {
		return version
	}
	return version + "-" + identity
}

func (cache *ProviderCache) read(ctx context.Context, query, version string) (cachedSearchResults, bool, error) {
	if cache.db == nil {
		return cachedSearchResults{}, false, nil
	}
	var encoded, fetchedText, expiresText string
	err := cache.db.QueryRowContext(ctx, `SELECT results_json, fetched_at_utc, expires_at_utc
		FROM universal_search_cache WHERE provider = ? AND normalized_query = ? AND schema_version = ?`,
		cache.provider, query, version).Scan(&encoded, &fetchedText, &expiresText)
	if err == sql.ErrNoRows {
		return cachedSearchResults{}, false, nil
	}
	if err != nil {
		return cachedSearchResults{}, false, err
	}
	fetchedAt, fetchedErr := time.Parse(time.RFC3339Nano, fetchedText)
	expiresAt, expiresErr := time.Parse(time.RFC3339Nano, expiresText)
	if fetchedErr != nil || expiresErr != nil || !expiresAt.After(fetchedAt) {
		cache.evict(ctx, query, version)
		return cachedSearchResults{}, false, nil
	}
	var results []domain.MetadataSearchResult
	if err := json.Unmarshal([]byte(encoded), &results); err != nil || results == nil || !cache.validResults(results) {
		cache.evict(ctx, query, version)
		return cachedSearchResults{}, false, nil
	}
	now := cache.now().UTC()
	if now.Before(expiresAt) {
		return cachedSearchResults{results: results}, true, nil
	}
	if now.Before(expiresAt.Add(cache.staleGrace)) {
		return cachedSearchResults{results: results, stale: true}, true, nil
	}
	cache.evict(ctx, query, version)
	return cachedSearchResults{}, false, nil
}

func (cache *ProviderCache) write(ctx context.Context, query string, results []domain.MetadataSearchResult, version string) error {
	if cache.db == nil {
		return nil
	}
	encoded, err := json.Marshal(results)
	if err != nil {
		return err
	}
	fetchedAt := cache.now().UTC()
	expiresAt := fetchedAt.Add(cache.ttl)
	providerCacheCapacityMu.Lock()
	defer providerCacheCapacityMu.Unlock()
	if _, err := cache.db.ExecContext(ctx, `INSERT INTO universal_search_cache
		(provider, normalized_query, schema_version, results_json, fetched_at_utc, expires_at_utc)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, normalized_query, schema_version) DO UPDATE SET
			results_json = excluded.results_json,
			fetched_at_utc = excluded.fetched_at_utc,
			expires_at_utc = excluded.expires_at_utc`,
		cache.provider, query, version, string(encoded), fetchedAt.Format(time.RFC3339Nano), expiresAt.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	var count int
	if err := cache.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM universal_search_cache`).Scan(&count); err != nil {
		return err
	}
	if excess := count - cache.maxEntries; excess > 0 {
		_, err = cache.db.ExecContext(ctx, `DELETE FROM universal_search_cache
			WHERE rowid IN (
				SELECT rowid FROM universal_search_cache
				ORDER BY fetched_at_utc ASC, provider ASC, normalized_query ASC, schema_version ASC
				LIMIT ?
			)`, excess)
	}
	return err
}

func (cache *ProviderCache) evict(ctx context.Context, query, version string) {
	if cache.db == nil {
		return
	}
	_, _ = cache.db.ExecContext(ctx, `DELETE FROM universal_search_cache
		WHERE provider = ? AND normalized_query = ? AND schema_version = ?`, cache.provider, query, version)
}

func (cache *ProviderCache) validResults(results []domain.MetadataSearchResult) bool {
	for _, result := range results {
		if result.Provider != cache.provider || strings.TrimSpace(result.ExternalID) == "" || strings.TrimSpace(result.Title) == "" {
			return false
		}
	}
	return true
}

func limitResults(results []domain.MetadataSearchResult, limit int) []domain.MetadataSearchResult {
	if results == nil {
		return []domain.MetadataSearchResult{}
	}
	if len(results) > limit {
		return results[:limit]
	}
	return results
}
