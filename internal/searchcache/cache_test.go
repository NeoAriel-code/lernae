package searchcache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lernae/internal/database"
	"lernae/internal/domain"
)

func TestFreshResultCacheUsesProviderAndNormalizedQueryKey(t *testing.T) {
	db := openCacheTestDB(t)
	firstSource := &stubSource{results: []domain.MetadataSearchResult{testResult("first")}}
	firstCache := New(firstSource, db, "igdb")

	first, err := firstCache.SearchWorks(context.Background(), "  Soul   Calibur  ")
	if err != nil {
		t.Fatal(err)
	}
	secondSource := &stubSource{results: []domain.MetadataSearchResult{testResult("second")}}
	secondCache := New(secondSource, db, "igdb")
	second, err := secondCache.SearchWorks(context.Background(), "soul calibur")
	if err != nil {
		t.Fatal(err)
	}
	if firstSource.calls != 1 || secondSource.calls != 0 {
		t.Fatalf("source calls = first:%d second:%d, want first:1 second:0", firstSource.calls, secondSource.calls)
	}
	if len(first) != 1 || len(second) != 1 || first[0].Title != "first" || second[0].Title != "first" {
		t.Fatalf("first/second results = %#v / %#v, want cached first result", first, second)
	}

	var provider, query, resultsJSON, fetchedAt, expiresAt string
	if err := db.QueryRow(`SELECT provider, normalized_query, results_json, fetched_at_utc, expires_at_utc
		FROM metadata_search_cache`).Scan(&provider, &query, &resultsJSON, &fetchedAt, &expiresAt); err != nil {
		t.Fatal(err)
	}
	if provider != "igdb" || query != "soul calibur" {
		t.Fatalf("cache key = %q/%q, want igdb/soul calibur", provider, query)
	}
	var stored []domain.MetadataSearchResult
	if err := json.Unmarshal([]byte(resultsJSON), &stored); err != nil {
		t.Fatalf("cache row is not normalized search-result JSON: %v", err)
	}
	if len(stored) != 1 || stored[0].ExternalID != first[0].ExternalID || stored[0].Title != first[0].Title {
		t.Fatalf("stored normalized results = %#v, want %#v", stored, first)
	}
	fetched, err := time.Parse(time.RFC3339Nano, fetchedAt)
	if err != nil {
		t.Fatalf("parse fetched_at_utc %q: %v", fetchedAt, err)
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		t.Fatalf("parse expires_at_utc %q: %v", expiresAt, err)
	}
	if ttl := expires.Sub(fetched); ttl <= 0 || ttl > 15*time.Minute {
		t.Fatalf("cached result TTL = %s, want positive and no more than 15m", ttl)
	}
}

func TestCacheSeparatesProvidersForTheSameNormalizedQuery(t *testing.T) {
	db := openCacheTestDB(t)
	firstSource := &stubSource{results: []domain.MetadataSearchResult{testResult("IGDB result")}}
	otherProviderResult := testResult("other provider result")
	otherProviderResult.Provider = "other"
	secondSource := &stubSource{results: []domain.MetadataSearchResult{otherProviderResult}}
	firstCache := New(firstSource, db, "igdb")
	secondCache := New(secondSource, db, "other")

	if _, err := firstCache.SearchWorks(context.Background(), "Soulcalibur"); err != nil {
		t.Fatal(err)
	}
	got, err := secondCache.SearchWorks(context.Background(), "soulcalibur")
	if err != nil {
		t.Fatal(err)
	}
	if firstSource.calls != 1 || secondSource.calls != 1 || got[0].Title != "other provider result" {
		t.Fatalf("provider caches were not isolated: calls=%d/%d result=%#v", firstSource.calls, secondSource.calls, got)
	}
}

func TestExpiredResultIsRefetchedAndReplaced(t *testing.T) {
	db := openCacheTestDB(t)
	source := &stubSource{results: []domain.MetadataSearchResult{testResult("old result")}}
	cache := New(source, db, "igdb")
	if _, err := cache.SearchWorks(context.Background(), "Soulcalibur"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE metadata_search_cache SET expires_at_utc = '2000-01-01T00:00:00Z'
		WHERE provider = 'igdb' AND normalized_query = 'soulcalibur'`); err != nil {
		t.Fatal(err)
	}
	source.results = []domain.MetadataSearchResult{testResult("fresh result")}
	got, err := cache.SearchWorks(context.Background(), "Soulcalibur")
	if err != nil {
		t.Fatal(err)
	}
	if source.calls != 2 || got[0].Title != "fresh result" {
		t.Fatalf("expired cache was not refreshed: calls=%d result=%#v", source.calls, got)
	}
}

func TestCorruptResultRowIsEvictedAndRefetched(t *testing.T) {
	db := openCacheTestDB(t)
	if _, err := db.Exec(`INSERT INTO metadata_search_cache
		(provider, normalized_query, results_json, fetched_at_utc, expires_at_utc)
		VALUES ('igdb', 'soulcalibur', '{not-json', '2026-01-01T00:00:00Z', '2099-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	source := &stubSource{results: []domain.MetadataSearchResult{testResult("refetched result")}}
	cache := New(source, db, "igdb")
	got, err := cache.SearchWorks(context.Background(), "Soulcalibur")
	if err != nil {
		t.Fatal(err)
	}
	if source.calls != 1 || len(got) != 1 || got[0].Title != "refetched result" {
		t.Fatalf("corrupt cache was not refetched: calls=%d result=%#v", source.calls, got)
	}
	var stored string
	if err := db.QueryRow(`SELECT results_json FROM metadata_search_cache
		WHERE provider = 'igdb' AND normalized_query = 'soulcalibur'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "not-json") {
		t.Fatalf("corrupt cache row remained after refetch: %q", stored)
	}
}

func TestProviderErrorsAreNotCached(t *testing.T) {
	db := openCacheTestDB(t)
	source := &stubSource{err: errors.New("temporary provider failure")}
	cache := New(source, db, "igdb")
	if _, err := cache.SearchWorks(context.Background(), "Soulcalibur"); err == nil {
		t.Fatal("expected provider error")
	}
	source.err = nil
	source.results = []domain.MetadataSearchResult{testResult("recovered")}
	if _, err := cache.SearchWorks(context.Background(), "Soulcalibur"); err != nil {
		t.Fatal(err)
	}
	if source.calls != 2 {
		t.Fatalf("source calls = %d, want 2 because errors are not cached", source.calls)
	}
}

type stubSource struct {
	calls   int
	results []domain.MetadataSearchResult
	err     error
}

func (source *stubSource) SearchWorks(_ context.Context, _ string) ([]domain.MetadataSearchResult, error) {
	source.calls++
	return source.results, source.err
}

func openCacheTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	return db
}

func testResult(title string) domain.MetadataSearchResult {
	return domain.MetadataSearchResult{
		Provider: "igdb", ExternalID: "42", Title: title,
		Medium: domain.MediumGame, WorkType: "game",
	}
}
