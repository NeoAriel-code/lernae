package searchcache

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"lernae/internal/domain"
	"lernae/internal/providers"
	"lernae/migrations"
)

func TestResultSchemaVersionBumpsOpenLibraryAndTVMazeForNormalizedMetadata(t *testing.T) {
	for _, test := range []struct {
		provider string
		want     string
	}{
		{provider: "openlibrary", want: OpenLibraryResultSchemaVersion},
		{provider: " OpenLibrary ", want: OpenLibraryResultSchemaVersion},
		{provider: "tvmaze", want: "v2"},
		{provider: "igdb", want: CurrentResultSchemaVersion},
	} {
		t.Run(test.provider, func(t *testing.T) {
			if got := ResultSchemaVersion(test.provider); got != test.want {
				t.Fatalf("ResultSchemaVersion(%q) = %q, want %q", test.provider, got, test.want)
			}
		})
	}
}

func TestProviderCacheDefaultSchemaVersionUsesProviderShape(t *testing.T) {
	for _, test := range []struct {
		provider string
		want     string
	}{
		{provider: "openlibrary", want: OpenLibraryResultSchemaVersion},
		{provider: "tvmaze", want: "v2"},
	} {
		t.Run(test.provider, func(t *testing.T) {
			cache := NewProviderCache(&providerSearchStub{name: test.provider}, nil, test.provider, "")
			if cache.schemaVersion != test.want {
				t.Fatalf("default schema version = %q, want %q", cache.schemaVersion, test.want)
			}
		})
	}
}

func TestDefaultTVMazeCacheDoesNotServeResultsWithoutSearchChannels(t *testing.T) {
	db := openCacheTestDB(t)
	oldResult := searchResult("tvmaze", "Channel-aware show")
	oldSource := &providerSearchStub{name: "tvmaze", results: []domain.MetadataSearchResult{oldResult}}
	oldCache := NewProviderCache(oldSource, db, "tvmaze", CurrentResultSchemaVersion)
	if _, err := oldCache.Search(context.Background(), "channel cache", 10); err != nil {
		t.Fatal(err)
	}

	newResult := searchResult("tvmaze", "Channel-aware show")
	newResult.Network = "Prime Video"
	newResult.WebChannel = "Amazon"
	newSource := &providerSearchStub{name: "tvmaze", results: []domain.MetadataSearchResult{newResult}}
	newCache := NewProviderCache(newSource, db, "tvmaze", "")
	got, err := newCache.Search(context.Background(), "channel cache", 10)
	if err != nil || len(got) != 1 || got[0].Network != "Prime Video" || got[0].WebChannel != "Amazon" {
		t.Fatalf("default cache result = %#v, error=%v", got, err)
	}
	cached, err := newCache.Search(context.Background(), "channel cache", 10)
	if err != nil || len(cached) != 1 || cached[0].Network != "Prime Video" || cached[0].WebChannel != "Amazon" {
		t.Fatalf("TVMaze cached channel fields = %#v, error=%v", cached, err)
	}
	if oldSource.calls != 1 || newSource.calls != 1 || newCache.schemaVersion != "v2" {
		t.Fatalf("cache calls/version = %d/%d/%q, want old v1 then new TVMaze v2 fetch", oldSource.calls, newSource.calls, newCache.schemaVersion)
	}
}

func TestDefaultOpenLibraryCacheDoesNotServeThePreviousResultShape(t *testing.T) {
	db := openCacheTestDB(t)
	oldSource := &providerSearchStub{name: "openlibrary", results: []domain.MetadataSearchResult{searchResult("openlibrary", "old shape")}}
	oldCache := NewProviderCache(oldSource, db, "openlibrary", CurrentResultSchemaVersion)
	if _, err := oldCache.Search(context.Background(), "language cache", 10); err != nil {
		t.Fatal(err)
	}
	newResult := searchResult("openlibrary", "edition-aware shape")
	newResult.RepresentativeEdition = &domain.RepresentativeEdition{ExternalID: "OL99M", Language: "en"}
	newSource := &providerSearchStub{name: "openlibrary", results: []domain.MetadataSearchResult{newResult}}
	newCache := NewProviderCache(newSource, db, "openlibrary", "")
	got, err := newCache.Search(context.Background(), "language cache", 10)
	if err != nil || len(got) != 1 || got[0].RepresentativeEdition == nil || got[0].RepresentativeEdition.ExternalID != "OL99M" {
		t.Fatalf("default cache result = %#v, error=%v", got, err)
	}
	if oldSource.calls != 1 || newSource.calls != 1 || newCache.schemaVersion != OpenLibraryResultSchemaVersion {
		t.Fatalf("cache calls/version = %d/%d/%q, want old v1 then new Open Library v2 fetch", oldSource.calls, newSource.calls, newCache.schemaVersion)
	}
}

type providerSearchStub struct {
	name    string
	calls   int
	limits  []int
	results []domain.MetadataSearchResult
	err     error
}

func (source *providerSearchStub) Name() string { return source.name }

func (source *providerSearchStub) Search(_ context.Context, _ string, limit int) ([]domain.MetadataSearchResult, error) {
	source.calls++
	source.limits = append(source.limits, limit)
	return source.results, source.err
}

func TestProviderCacheUsesNormalizedProviderAndSchemaVersionKeys(t *testing.T) {
	db := openCacheTestDB(t)
	firstSource := &providerSearchStub{name: "openlibrary", results: []domain.MetadataSearchResult{searchResult("openlibrary", "first")}}
	firstCache := NewProviderCache(firstSource, db, "openlibrary", "v1")
	first, err := firstCache.Search(context.Background(), "  Wheel   of Time  ", 10)
	if err != nil {
		t.Fatal(err)
	}
	secondSource := &providerSearchStub{name: "openlibrary", results: []domain.MetadataSearchResult{searchResult("openlibrary", "second")}}
	secondCache := NewProviderCache(secondSource, db, "openlibrary", "v1")
	second, err := secondCache.Search(context.Background(), "wheel of time", 10)
	if err != nil {
		t.Fatal(err)
	}
	if firstSource.calls != 1 || secondSource.calls != 0 || first[0].Title != "first" || second[0].Title != "first" {
		t.Fatalf("fresh cache calls/results = %d/%d %#v/%#v", firstSource.calls, secondSource.calls, first, second)
	}
	versionSource := &providerSearchStub{name: "openlibrary", results: []domain.MetadataSearchResult{searchResult("openlibrary", "new schema")}}
	versionCache := NewProviderCache(versionSource, db, "openlibrary", "v2")
	if _, err := versionCache.Search(context.Background(), "wheel of time", 10); err != nil {
		t.Fatal(err)
	}
	otherSource := &providerSearchStub{name: "tvmaze", results: []domain.MetadataSearchResult{searchResult("tvmaze", "other provider")}}
	otherCache := NewProviderCache(otherSource, db, "tvmaze", "v1")
	if _, err := otherCache.Search(context.Background(), "wheel of time", 10); err != nil {
		t.Fatal(err)
	}
	if versionSource.calls != 1 || otherSource.calls != 1 {
		t.Fatalf("schema/provider key isolation calls = %d/%d, want 1/1", versionSource.calls, otherSource.calls)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM universal_search_cache`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 3 {
		t.Fatalf("stored cache variants = %d, want 3", rows)
	}
}

func TestOpenLibraryCacheSeparatesMetadataLanguageAndHitsWithinLanguage(t *testing.T) {
	db := openCacheTestDB(t)
	source := &languageIdentitySearchProvider{language: "en"}
	cache := NewProviderCache(source, db, "openlibrary", "")

	for _, language := range []string{"en", "es", "original", "es", "en"} {
		source.language = language
		results, err := cache.Search(context.Background(), "the wheel of time", 10)
		if err != nil || len(results) != 1 || results[0].Title != language {
			t.Fatalf("language %q cache result = %#v, error=%v; want language-specific result", language, results, err)
		}
	}
	if source.calls != 3 {
		t.Fatalf("provider calls across en/es/original and repeated languages = %d, want 3", source.calls)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM universal_search_cache WHERE provider = 'openlibrary' AND normalized_query = 'the wheel of time'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 3 {
		t.Fatalf("durable Open Library language variants = %d, want 3", rows)
	}
}

type languageIdentitySearchProvider struct {
	language string
	calls    int
}

func (*languageIdentitySearchProvider) Name() string { return "openlibrary" }

func (source *languageIdentitySearchProvider) SearchCacheIdentity() string {
	return "metadata-language-" + source.language
}

func (source *languageIdentitySearchProvider) Search(_ context.Context, _ string, _ int) ([]domain.MetadataSearchResult, error) {
	source.calls++
	return []domain.MetadataSearchResult{searchResult("openlibrary", source.language)}, nil
}

func TestProviderCacheDoesNotLetRequestLimitContaminateCachedResults(t *testing.T) {
	for _, order := range []string{"small then large", "large then small"} {
		t.Run(order, func(t *testing.T) {
			db := openCacheTestDB(t)
			results := []domain.MetadataSearchResult{
				searchResult("openlibrary", "first"),
				searchResult("openlibrary", "second"),
				searchResult("openlibrary", "third"),
			}
			for index := range results {
				results[index].ExternalID = fmt.Sprintf("work-%d", index+1)
			}
			source := &providerSearchStub{name: "openlibrary", results: results}
			cache := NewProviderCache(source, db, "openlibrary", "v1")

			firstLimit, secondLimit := 1, 2
			if order == "large then small" {
				firstLimit, secondLimit = secondLimit, firstLimit
			}
			first, err := cache.Search(context.Background(), "same query", firstLimit)
			if err != nil {
				t.Fatal(err)
			}
			second, err := cache.Search(context.Background(), "same query", secondLimit)
			if err != nil {
				t.Fatal(err)
			}
			wantFirst, wantSecond := firstLimit, secondLimit
			if wantFirst > len(results) {
				wantFirst = len(results)
			}
			if wantSecond > len(results) {
				wantSecond = len(results)
			}
			if len(first) != wantFirst || len(second) != wantSecond {
				t.Fatalf("result counts = %d then %d, want %d then %d", len(first), len(second), wantFirst, wantSecond)
			}
			if source.calls != 1 {
				t.Fatalf("provider calls = %d, want one call shared by both limits", source.calls)
			}
			if len(source.limits) != 1 || source.limits[0] != providers.MaxProviderSearchResults {
				t.Fatalf("provider limits = %v, want canonical sufficient limit %d", source.limits, providers.MaxProviderSearchResults)
			}
		})
	}
}

func TestProviderCacheFreshExpiryAndBoundedStaleIfError(t *testing.T) {
	db := openCacheTestDB(t)
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	source := &providerSearchStub{name: "openlibrary", results: []domain.MetadataSearchResult{searchResult("openlibrary", "cached")}}
	cache := NewProviderCache(source, db, "openlibrary", "v1")
	cache.now = func() time.Time { return now }
	if _, err := cache.Search(context.Background(), "Wheel of Time", 10); err != nil {
		t.Fatal(err)
	}
	if cache.ttl != 15*time.Minute || cache.staleGrace != 24*time.Hour {
		t.Fatalf("fresh/stale policy = %s/%s, want 15m/24h", cache.ttl, cache.staleGrace)
	}
	now = now.Add(16 * time.Minute)
	source.err = providers.ErrSearchThrottled
	stale, err := cache.Search(context.Background(), "Wheel of Time", 10)
	if !errors.Is(err, providers.ErrSearchStale) || len(stale) != 1 || stale[0].Title != "cached" {
		t.Fatalf("stale fallback = %#v, %v, want cached data plus ErrSearchStale", stale, err)
	}

	now = now.Add(24*time.Hour + time.Second)
	tooOld, err := cache.Search(context.Background(), "Wheel of Time", 10)
	if !errors.Is(err, providers.ErrSearchThrottled) || len(tooOld) != 0 {
		t.Fatalf("expired stale fallback = %#v, %v, want no stale results and throttle", tooOld, err)
	}
	if source.calls != 3 {
		t.Fatalf("provider calls = %d, want 3", source.calls)
	}
}

func TestProviderCacheEvictsOldestKeysAtGlobalCapacity(t *testing.T) {
	db := openCacheTestDB(t)
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for index := 0; index < defaultMaxProviderCacheKeys; index++ {
		fetched := base.Add(time.Duration(index) * time.Minute)
		if _, err := db.Exec(`INSERT INTO universal_search_cache
			(provider, normalized_query, schema_version, results_json, fetched_at_utc, expires_at_utc)
			VALUES ('openlibrary', ?, 'v1', '[]', ?, ?)`, fmt.Sprintf("query-%03d", index),
			fetched.Format(time.RFC3339Nano), fetched.Add(defaultTTL).Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	now := base.Add(time.Duration(defaultMaxProviderCacheKeys+1) * time.Minute)
	source := &providerSearchStub{name: "openlibrary", results: []domain.MetadataSearchResult{searchResult("openlibrary", "new query")}}
	cache := NewProviderCache(source, db, "openlibrary", "v1")
	cache.now = func() time.Time { return now }
	if _, err := cache.Search(context.Background(), "query-new", 10); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM universal_search_cache`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != defaultMaxProviderCacheKeys {
		t.Fatalf("cache size = %d, want bounded %d", rows, defaultMaxProviderCacheKeys)
	}
	var oldest int
	if err := db.QueryRow(`SELECT COUNT(*) FROM universal_search_cache WHERE normalized_query = 'query-000'`).Scan(&oldest); err != nil {
		t.Fatal(err)
	}
	if oldest != 0 {
		t.Fatal("oldest cache key was not evicted")
	}
	var newest int
	if err := db.QueryRow(`SELECT COUNT(*) FROM universal_search_cache WHERE normalized_query = 'query-new'`).Scan(&newest); err != nil {
		t.Fatal(err)
	}
	if newest != 1 {
		t.Fatal("new cache key was evicted instead of the oldest")
	}
}

func TestProviderCacheSurvivesReadWriteFailures(t *testing.T) {
	db := openCacheTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	source := &providerSearchStub{name: "openlibrary", results: []domain.MetadataSearchResult{searchResult("openlibrary", "provider result")}}
	cache := NewProviderCache(source, db, "openlibrary", "v1")
	results, err := cache.Search(context.Background(), "title", 10)
	if err != nil || len(results) != 1 || results[0].Title != "provider result" {
		t.Fatalf("search with closed cache DB = %#v, %v", results, err)
	}
	cache = NewProviderCache(source, nil, "openlibrary", "v1")
	results, err = cache.Search(context.Background(), "title", 10)
	if err != nil || len(results) != 1 {
		t.Fatalf("search with absent cache DB = %#v, %v", results, err)
	}
}

func TestDatabaseMigrationCreatesVersionedUniversalSearchCache(t *testing.T) {
	db := openCacheTestDB(t)
	columns := make(map[string]bool)
	rows, err := db.Query(`PRAGMA table_info(universal_search_cache)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&ordinal, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"provider", "normalized_query", "schema_version", "results_json", "fetched_at_utc", "expires_at_utc"} {
		if !columns[required] {
			t.Errorf("cache table missing %q column", required)
		}
	}
}

func TestDatabaseMigrationPreservesLegacyNormalizedResults(t *testing.T) {
	db := openCacheTestDB(t)
	_, err := db.Exec(`INSERT INTO metadata_search_cache
		(provider, normalized_query, results_json, fetched_at_utc, expires_at_utc)
		VALUES ('igdb', 'legacy query', '[{"provider":"igdb","external_id":"42","title":"migrated result","medium":"game","work_type":"game"}]',
		'2026-01-01T00:00:00Z', '2099-01-01T00:15:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	script, err := migrations.Files.ReadFile("0006_universal_search_cache.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(script)); err != nil {
		t.Fatalf("reapply versioned cache migration: %v", err)
	}
	source := &providerSearchStub{name: "igdb", results: []domain.MetadataSearchResult{searchResult("igdb", "live result")}}
	cache := NewProviderCache(source, db, "igdb", "v1")
	got, err := cache.Search(context.Background(), " Legacy   Query ", 10)
	if err != nil || len(got) != 1 || got[0].Title != "migrated result" || source.calls != 0 {
		t.Fatalf("migrated cache result = %#v, error=%v calls=%d", got, err, source.calls)
	}
	var legacyRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM metadata_search_cache`).Scan(&legacyRows); err != nil {
		t.Fatal(err)
	}
	if legacyRows != 0 {
		t.Fatalf("legacy cache rows left behind = %d, want 0", legacyRows)
	}
}

func searchResult(provider, title string) domain.MetadataSearchResult {
	return domain.MetadataSearchResult{Provider: provider, ExternalID: "external-1", Title: title, MediaType: "book", Medium: domain.MediumLiterature, WorkType: "book"}
}
