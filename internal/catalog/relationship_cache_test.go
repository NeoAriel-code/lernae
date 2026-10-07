package catalog

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"lernae/internal/database"
	"lernae/internal/providers"
)

func TestRelationshipMetadataCacheUsesParserVersionAndTTL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relationship-cache.db")
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cache := NewSQLiteRelationshipMetadataCache(db)

	metadata := cacheTestMetadata()
	if err := cache.Put(ctx, metadata, time.Hour); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close database before cache reload: %v", err)
	}
	db, err = database.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen database for durable cache read: %v", err)
	}
	cache = NewSQLiteRelationshipMetadataCache(db)

	got, found, err := cache.Get(ctx, metadata.Provider, metadata.ExternalID, metadata.ParserVersion, metadata.FetchedAt.Add(time.Minute))
	if err != nil || !found {
		t.Fatalf("Get() found=%v error=%v, want cache hit", found, err)
	}
	if !reflect.DeepEqual(got, metadata) {
		t.Fatalf("cached metadata = %#v, want %#v", got, metadata)
	}

	if _, found, err := cache.Get(ctx, metadata.Provider, metadata.ExternalID, "new-parser-version", metadata.FetchedAt.Add(time.Minute)); err != nil || found {
		t.Fatalf("Get() with newer parser found=%v error=%v, want cache miss", found, err)
	}
	if _, found, err := cache.Get(ctx, metadata.Provider, metadata.ExternalID, metadata.ParserVersion, metadata.FetchedAt.Add(2*time.Hour)); err != nil || found {
		t.Fatalf("Get() after expiry found=%v error=%v, want cache miss", found, err)
	}

	var normalizedJSON string
	if err := db.QueryRowContext(ctx, `SELECT normalized_json FROM relationship_provider_cache
		WHERE provider = ? AND external_id = ?`, metadata.Provider, metadata.ExternalID).Scan(&normalizedJSON); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"raw_payload", "unsupported_private_marker", "P155", "OL999M", "private-raw-marker"} {
		if strings.Contains(normalizedJSON, forbidden) {
			t.Errorf("normalized cache JSON contains unsupported/raw field %q: %s", forbidden, normalizedJSON)
		}
	}
	if !strings.Contains(normalizedJSON, `"ordinal":"1b"`) {
		t.Fatalf("normalized cache JSON did not preserve textual ordinal: %s", normalizedJSON)
	}
	if !strings.Contains(normalizedJSON, `"property":"P648","value":"OL123W"`) ||
		!strings.Contains(normalizedJSON, `"property":"P9043","qualifier_property":"P5794","value":"1565"`) {
		t.Fatalf("normalized cache JSON did not preserve only exact parsed provider identifiers: %s", normalizedJSON)
	}
}

func TestRelationshipMetadataCacheRejectsUnsupportedClaimsWithoutWriting(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "relationship-cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cache := NewSQLiteRelationshipMetadataCache(db)
	metadata := cacheTestMetadata()
	metadata.Claims[0].Property = "P155"
	if err := cache.Put(ctx, metadata, time.Hour); err == nil {
		t.Fatal("Put() accepted unsupported P155 claim")
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM relationship_provider_cache`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("cache rows = %d, want no write for unsupported claim", count)
	}
}

func cacheTestMetadata() providers.RelationshipMetadata {
	ordinal := "1b"
	return providers.RelationshipMetadata{
		Provider:       "wikidata",
		ExternalID:     "Q42",
		EntityRevision: "1234",
		ParserVersion:  "wikidata-relationship-v2",
		FetchedAt:      time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC),
		Labels:         []providers.RelationshipLocalizedValue{{Language: "en", Value: "Example Series"}},
		Aliases:        []providers.RelationshipLocalizedValue{{Language: "es", Value: "Serie de ejemplo"}},
		Claims: []providers.RelationshipClaim{{
			Property: "P179", SubjectExternalID: "Q42", ObjectExternalID: "Q10", Ordinal: &ordinal,
		}},
		Identifiers: []providers.RelationshipIdentifier{
			{Property: "P648", Value: "OL123W"},
			{Property: "P9043", QualifierProperty: "P5794", Value: "1565"},
		},
	}
}
