package catalog

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"lernae/internal/database"
	"lernae/internal/domain"
	"lernae/internal/providers"
	"lernae/migrations"
)

func TestCreateUniversePersistsMetadataDefaultsAndAggregateTimestamps(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "universe.db")
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := NewSQLiteRepository(db)
	expectedSortTitle := "Universe Metadata"
	expectedDescription := "A plain text description."
	universe := domain.Universe{ID: "metadata-universe", Title: "Metadata Universe", SortTitle: &expectedSortTitle, Description: &expectedDescription}
	if err := repository.CreateUniverse(ctx, universe); err != nil {
		t.Fatalf("create Universe: %v", err)
	}

	var sortTitle, description, artwork sql.NullString
	var createdAt, updatedAt string
	if err := db.QueryRowContext(ctx, `SELECT sort_title, description, artwork, created_at_utc, updated_at_utc
		FROM universes WHERE id = ?`, universe.ID).Scan(&sortTitle, &description, &artwork, &createdAt, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if !sortTitle.Valid || sortTitle.String != expectedSortTitle || !description.Valid || description.String != expectedDescription || artwork.Valid {
		t.Fatalf("new Universe nullable metadata = %#v/%#v/%#v, want stored text and NULL artwork", sortTitle, description, artwork)
	}
	created, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil || created.IsZero() {
		t.Fatalf("new Universe created_at_utc = %q, want valid UTC time: %v", createdAt, err)
	}
	updated, err := time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil || updated.IsZero() || !created.Equal(updated) {
		t.Fatalf("new Universe updated_at_utc = %q, want creation time %q: %v", updatedAt, createdAt, err)
	}

	work, err := repository.MaterializeExternalWork(ctx, domain.WorkMaterialization{
		Provider: "fixture", ExternalID: "metadata-work", Title: "Metadata Work", Medium: domain.MediumVideo, WorkType: "series",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
		UniverseID: universe.ID, WorkID: work.Work.ID, Provenance: domain.UniverseMembershipProvenanceManual,
		Confidence: 1, ConfirmedByUser: true,
	}); err != nil {
		t.Fatalf("confirm Work membership: %v", err)
	}
	var membershipUpdatedAt string
	if err := db.QueryRowContext(ctx, `SELECT updated_at_utc FROM universes WHERE id = ?`, universe.ID).Scan(&membershipUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if membershipUpdatedAt == createdAt {
		t.Fatal("membership mutation did not advance Universe updated_at")
	}
	if _, err := db.ExecContext(ctx, `UPDATE universes SET updated_at_utc = '2000-01-01T00:00:00Z' WHERE id = ?`, universe.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
		UniverseID: universe.ID, WorkID: work.Work.ID, Provenance: domain.UniverseMembershipProvenanceManual,
		Confidence: 1, ConfirmedByUser: true,
	}); err != nil {
		t.Fatalf("repeat identical membership: %v", err)
	}
	var noOpUpdatedAt string
	if err := db.QueryRowContext(ctx, `SELECT updated_at_utc FROM universes WHERE id = ?`, universe.ID).Scan(&noOpUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if noOpUpdatedAt != "2000-01-01T00:00:00Z" {
		t.Fatalf("identical membership changed Universe updated_at to %q; want no-op to preserve sentinel", noOpUpdatedAt)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := database.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen database with Universe metadata: %v", err)
	}
	defer reopened.Close()
	graph, err := NewSQLiteRepository(reopened).GetUniverse(ctx, universe.ID)
	if err != nil || graph.Universe.Title != universe.Title || graph.Universe.SortTitle == nil || *graph.Universe.SortTitle != *universe.SortTitle ||
		graph.Universe.Description == nil || *graph.Universe.Description != *universe.Description || graph.Universe.Artwork != nil ||
		graph.Universe.CreatedAt.IsZero() || graph.Universe.UpdatedAt.IsZero() {
		t.Fatalf("get Universe after database restart = %#v, %v", graph.Universe, err)
	}
}

func TestUniverseAggregateTimestampTracksAliasAndExclusionChanges(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "universe-mutations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := NewSQLiteRepository(db)
	universeID := domain.UniverseID("mutation-universe")
	if err := repository.CreateUniverse(ctx, domain.Universe{ID: universeID, Title: "Mutation Universe"}); err != nil {
		t.Fatal(err)
	}
	setSentinel := func() {
		t.Helper()
		if _, err := db.ExecContext(ctx, `UPDATE universes SET updated_at_utc = '2000-01-01T00:00:00Z' WHERE id = ?`, universeID); err != nil {
			t.Fatal(err)
		}
	}
	assertUpdated := func(wantChanged bool, operation string) {
		t.Helper()
		var updatedAt string
		if err := db.QueryRowContext(ctx, `SELECT updated_at_utc FROM universes WHERE id = ?`, universeID).Scan(&updatedAt); err != nil {
			t.Fatal(err)
		}
		changed := updatedAt != "2000-01-01T00:00:00Z"
		if changed != wantChanged {
			t.Fatalf("%s updated_at = %q; changed=%t, want changed=%t", operation, updatedAt, changed, wantChanged)
		}
	}

	setSentinel()
	alias := domain.UniverseAlias{UniverseID: universeID, Alias: "Alternate", Provenance: "manual"}
	if _, err := repository.AddUniverseAlias(ctx, alias); err != nil {
		t.Fatalf("add alias: %v", err)
	}
	assertUpdated(true, "new alias")

	setSentinel()
	if _, err := repository.AddUniverseAlias(ctx, alias); err != nil {
		t.Fatalf("repeat alias: %v", err)
	}
	assertUpdated(false, "duplicate alias")

	setSentinel()
	if err := repository.RemoveUniverseAlias(ctx, universeID, alias.Alias); err != nil {
		t.Fatalf("remove alias: %v", err)
	}
	assertUpdated(true, "alias removal")

	work, err := repository.MaterializeExternalWork(ctx, domain.WorkMaterialization{
		Provider: "fixture", ExternalID: "excluded-work", Title: "Excluded Work", Medium: domain.MediumVideo, WorkType: "series",
	})
	if err != nil {
		t.Fatal(err)
	}
	setSentinel()
	if _, err := repository.RemoveUniverseMembership(ctx, work.Work.ID, universeID, "manual rejection"); err != nil {
		t.Fatalf("create exclusion: %v", err)
	}
	assertUpdated(true, "new exclusion")

	setSentinel()
	if _, err := repository.RemoveUniverseMembership(ctx, work.Work.ID, universeID, "manual rejection"); err != nil {
		t.Fatalf("repeat exclusion: %v", err)
	}
	assertUpdated(false, "identical exclusion")

	setSentinel()
	if _, err := repository.RemoveUniverseMembership(ctx, work.Work.ID, universeID, "updated rejection"); err != nil {
		t.Fatalf("change exclusion reason: %v", err)
	}
	assertUpdated(true, "changed exclusion")
}

func TestWorkRelationSchemaSupportsTextOrdinalsAndExclusiveTargets(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "relationship-schema.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	repository := NewSQLiteRepository(db)
	work, err := repository.MaterializeExternalWork(ctx, domain.WorkMaterialization{
		Provider: "fixture", ExternalID: "book-one", Title: "Book One", Medium: domain.MediumLiterature, WorkType: "novel",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO work_collections (id, title, collection_type, created_at_utc)
		VALUES ('series-a', 'Series A', 'series', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("create Series collection: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO work_relations
		(id, work_id, relation_type, target_collection_id, ordinal, provenance, confidence, evidence, confirmed_by_user, created_at_utc)
		VALUES ('relation-a', ?, 'part_of_series', 'series-a', '5.0', 'provider', 0.9, 'P1545', 0, '2026-01-01T00:00:00Z')`, work.Work.ID); err != nil {
		t.Fatalf("create ordered Work relation: %v", err)
	}
	var ordinal string
	if err := db.QueryRowContext(ctx, `SELECT ordinal FROM work_relations WHERE id = 'relation-a'`).Scan(&ordinal); err != nil {
		t.Fatal(err)
	}
	if ordinal != "5.0" {
		t.Fatalf("stored ordinal = %q, want textual provider form 5.0", ordinal)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO work_relations
		(id, work_id, relation_type, target_work_id, target_collection_id, provenance, confidence, evidence, confirmed_by_user, created_at_utc)
		VALUES ('invalid-relation', ?, 'adaptation_of', ?, 'series-a', 'provider', 0.5, '', 0, '2026-01-01T00:00:00Z')`, work.Work.ID, work.Work.ID); err == nil {
		t.Fatal("relation with both Work and collection targets unexpectedly succeeded")
	}
}

func TestUniverseAliasSchemaSupportsLocalizationAndAuthority(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "localized-alias-schema.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := NewSQLiteRepository(db).CreateUniverse(ctx, domain.Universe{ID: "universe-a", Title: "Universe A"}); err != nil {
		t.Fatal(err)
	}
	for _, language := range []string{"es", "en"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO universe_aliases
			(universe_id, alias, language_code, provenance, provider_version, confidence, confirmed_by_user, created_at_utc)
			VALUES ('universe-a', 'Juego Ejemplo', ?, 'provider', '2026-01', 0.8, 0, '2026-01-01T00:00:00Z')`, language); err != nil {
			t.Fatalf("store localized alias for %s: %v", language, err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO universe_aliases (universe_id, alias, provenance, created_at_utc)
		VALUES ('universe-a', 'Legacy Direct Alias', 'manual', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("store an alias using the legacy insert shape: %v", err)
	}
	var aliases int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM universe_aliases WHERE universe_id = 'universe-a' AND alias = 'Juego Ejemplo'`).Scan(&aliases); err != nil {
		t.Fatal(err)
	}
	if aliases != 2 {
		t.Fatalf("localized alias count = %d, want the same normalized value in two languages", aliases)
	}
}

func TestLocalizedAliasAndWorkRelationRefreshPreserveManualAuthority(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "relationship-persistence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := NewSQLiteRepository(db)
	universeID := domain.UniverseID("universe-relationships")
	if err := repository.CreateUniverse(ctx, domain.Universe{ID: universeID, Title: "Universe Relationships"}); err != nil {
		t.Fatal(err)
	}

	spanish := "es"
	providerVersion := "2026-01"
	providerAlias, err := repository.AddUniverseAlias(ctx, domain.UniverseAlias{
		UniverseID: universeID, Alias: "  Juego   de Tronos ", Language: &spanish,
		Provenance: "provider", ProviderVersion: &providerVersion, Confidence: 0.8,
	})
	if err != nil {
		t.Fatalf("add localized provider alias: %v", err)
	}
	if providerAlias.Alias != "Juego de Tronos" || providerAlias.Language == nil || *providerAlias.Language != "es" ||
		providerAlias.ProviderVersion == nil || *providerAlias.ProviderVersion != providerVersion || providerAlias.Confidence != 0.8 {
		t.Fatalf("stored localized alias = %#v, want normalized alias and language", providerAlias)
	}
	english := "EN"
	if _, err := repository.AddUniverseAlias(ctx, domain.UniverseAlias{
		UniverseID: universeID, Alias: "Juego de Tronos", Language: &english, Provenance: "provider", Confidence: 0.7,
	}); err != nil {
		t.Fatalf("add English alias with the same normalized value: %v", err)
	}
	manualAlias, err := repository.AddUniverseAlias(ctx, domain.UniverseAlias{
		UniverseID: universeID, Alias: "Juego de Tronos", Language: &spanish, Provenance: "manual",
	})
	if err != nil {
		t.Fatalf("confirm localized alias: %v", err)
	}
	if !manualAlias.ConfirmedByUser || manualAlias.Provenance != "manual" || manualAlias.Confidence != 1 {
		t.Fatalf("manual alias authority = %#v, want confirmed manual alias", manualAlias)
	}
	newProviderVersion := "2026-02"
	refreshedAlias, err := repository.AddUniverseAlias(ctx, domain.UniverseAlias{
		UniverseID: universeID, Alias: "Juego de Tronos", Language: &spanish,
		Provenance: "provider", ProviderVersion: &newProviderVersion, Confidence: 0.5,
	})
	if err != nil {
		t.Fatalf("refresh localized provider alias: %v", err)
	}
	if !refreshedAlias.ConfirmedByUser || refreshedAlias.Provenance != "manual" || refreshedAlias.ProviderVersion != nil {
		t.Fatalf("automatic refresh replaced manually confirmed alias: %#v", refreshedAlias)
	}

	work, err := repository.MaterializeExternalWork(ctx, domain.WorkMaterialization{
		Provider: "fixture", ExternalID: "series-book-1", Title: "Book One", Medium: domain.MediumLiterature, WorkType: "novel",
	})
	if err != nil {
		t.Fatal(err)
	}
	collection := domain.WorkCollection{ID: "series-books", Title: "Book Series", Type: domain.WorkCollectionTypeSeries}
	if err := repository.CreateWorkCollection(ctx, collection); err != nil {
		t.Fatalf("create Work collection: %v", err)
	}
	if got, err := repository.GetWorkCollection(ctx, collection.ID); err != nil || got != collection {
		t.Fatalf("get Work collection = %#v, %v; want %#v", got, err, collection)
	}
	ordinal := "5.0"
	relation := domain.WorkRelation{
		WorkID: work.Work.ID, Type: domain.WorkRelationPartOfSeries, TargetCollectionID: &collection.ID,
		Ordinal: &ordinal, Provenance: domain.WorkRelationProvenanceProvider, ProviderVersion: &providerVersion,
		Confidence: 0.9, Evidence: "P1545",
	}
	providerRelation, err := repository.SetWorkRelation(ctx, relation)
	if err != nil {
		t.Fatalf("set ordered Work relation: %v", err)
	}
	if providerRelation.Ordinal == nil || *providerRelation.Ordinal != ordinal || providerRelation.Type != domain.WorkRelationPartOfSeries ||
		providerRelation.TargetCollectionID == nil || *providerRelation.TargetCollectionID != collection.ID ||
		providerRelation.ProviderVersion == nil || *providerRelation.ProviderVersion != providerVersion ||
		providerRelation.Confidence != 0.9 || providerRelation.Evidence != "P1545" {
		t.Fatalf("stored Work relation = %#v, want textual ordinal %q", providerRelation, ordinal)
	}
	relation.Provenance = domain.WorkRelationProvenanceManual
	relation.ProviderVersion = nil
	relation.Confidence = 1
	relation.ConfirmedByUser = true
	relation.Evidence = "manual order confirmation"
	manualRelation, err := repository.SetWorkRelation(ctx, relation)
	if err != nil {
		t.Fatalf("confirm ordered Work relation: %v", err)
	}
	if !manualRelation.ConfirmedByUser || manualRelation.Provenance != domain.WorkRelationProvenanceManual {
		t.Fatalf("manual relation authority = %#v", manualRelation)
	}
	newOrdinal, newProviderVersion := "6", "2026-02"
	relation.Ordinal = &newOrdinal
	relation.Provenance = domain.WorkRelationProvenanceProvider
	relation.ProviderVersion = &newProviderVersion
	relation.Confidence = 0.7
	relation.ConfirmedByUser = false
	relation.Evidence = "P1545 refresh"
	refreshedRelation, err := repository.SetWorkRelation(ctx, relation)
	if err != nil {
		t.Fatalf("refresh ordered Work relation: %v", err)
	}
	if !refreshedRelation.ConfirmedByUser || refreshedRelation.Provenance != domain.WorkRelationProvenanceManual ||
		refreshedRelation.Ordinal == nil || *refreshedRelation.Ordinal != ordinal || refreshedRelation.ProviderVersion != nil {
		t.Fatalf("automatic refresh replaced manual Work relation: %#v", refreshedRelation)
	}
	relations, err := repository.GetWorkRelations(ctx, work.Work.ID)
	if err != nil || len(relations) != 1 || !relations[0].ConfirmedByUser || relations[0].Ordinal == nil || *relations[0].Ordinal != ordinal {
		t.Fatalf("get Work relations = %#v, %v; want one persisted relation", relations, err)
	}

	graph, err := repository.GetUniverse(ctx, universeID)
	if err != nil || len(graph.Aliases) != 2 || graph.Aliases[1].Language == nil || *graph.Aliases[1].Language != "es" ||
		!graph.Aliases[1].ConfirmedByUser || graph.Aliases[1].Provenance != "manual" {
		t.Fatalf("get localized aliases = %#v, %v; want confirmed Spanish alias", graph.Aliases, err)
	}
	if err := repository.RemoveLocalizedUniverseAlias(ctx, universeID, "Juego de Tronos", &english); err != nil {
		t.Fatalf("remove only the English alias: %v", err)
	}
	graph, err = repository.GetUniverse(ctx, universeID)
	if err != nil || len(graph.Aliases) != 1 || graph.Aliases[0].Language == nil || *graph.Aliases[0].Language != "es" ||
		!graph.Aliases[0].ConfirmedByUser {
		t.Fatalf("localized alias removal changed other languages: %#v, %v", graph.Aliases, err)
	}
}

func TestRelationshipMigrationPreservesLegacyUniverseIdentityAndAlias(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:legacy-relationship-migration?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}

	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || strings.HasPrefix(entry.Name(), "0014_") || strings.HasPrefix(entry.Name(), "0016_") {
			continue
		}
		script, err := migrations.Files.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, string(script)); err != nil {
			t.Fatalf("apply legacy migration %s: %v", entry.Name(), err)
		}
	}

	repository := NewSQLiteRepository(db)
	work, err := repository.MaterializeExternalWork(ctx, domain.WorkMaterialization{
		Provider: "fixture", ExternalID: "legacy-work", Title: "Legacy Work", Medium: domain.MediumVideo, WorkType: "series",
	})
	if err != nil {
		t.Fatal(err)
	}
	universe := domain.Universe{ID: "legacy-universe", Title: "Legacy Universe"}
	if err := repository.CreateUniverse(ctx, universe); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
		UniverseID: universe.ID, WorkID: work.Work.ID, Provenance: domain.UniverseMembershipProvenanceManual,
		Confidence: 1, ConfirmedByUser: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO universe_aliases (universe_id, alias, provenance, created_at_utc)
		VALUES (?, ' Alias   legado ', 'manual', '2020-01-01T00:00:00Z'),
		       (?, 'Alias  legado', 'provider', '2021-01-01T00:00:00Z')`, universe.ID, universe.ID); err != nil {
		t.Fatal(err)
	}

	script, err := migrations.Files.ReadFile("0014_work_relationships_and_aliases.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(script)); err != nil {
		t.Fatalf("apply relationship migration: %v", err)
	}
	graph, err := repository.GetUniverse(ctx, universe.ID)
	if err != nil {
		t.Fatal(err)
	}
	if graph.Universe.ID != universe.ID || graph.Universe.Title != universe.Title || len(graph.Memberships) != 1 ||
		graph.Memberships[0].WorkID != work.Work.ID || len(graph.Aliases) != 1 {
		t.Fatalf("legacy Universe state after migration = %#v, want identity, membership, and alias preserved", graph)
	}
	legacyAlias := graph.Aliases[0]
	if legacyAlias.Alias != "Alias legado" || legacyAlias.Language != nil || !legacyAlias.ConfirmedByUser || legacyAlias.Confidence != 1 ||
		legacyAlias.CreatedAt.Format(time.RFC3339Nano) != "2020-01-01T00:00:00Z" {
		t.Fatalf("migrated legacy alias = %#v, want preserved manual alias with nullable language", legacyAlias)
	}
	var aliases int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM universe_aliases WHERE universe_id = ?`, universe.ID).Scan(&aliases); err != nil {
		t.Fatal(err)
	}
	if aliases != 1 {
		t.Fatalf("migrated alias rows = %d, want whitespace-equivalent aliases merged once", aliases)
	}
	var workCount, membershipCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM works`).Scan(&workCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM universe_memberships`).Scan(&membershipCount); err != nil {
		t.Fatal(err)
	}
	if workCount != 1 || membershipCount != 1 {
		t.Fatalf("legacy catalog rows after migration = Works %d/memberships %d, want 1/1", workCount, membershipCount)
	}
}

func TestCollectionIdentityMigrationPreservesExistingCollectionsAndRelations(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:legacy-collection-identity?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, err := strconv.Atoi(entry.Name()[:4])
		if err != nil || version > 15 {
			continue
		}
		script, err := migrations.Files.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, string(script)); err != nil {
			t.Fatalf("apply existing migration %s: %v", entry.Name(), err)
		}
	}
	repository := NewSQLiteRepository(db)
	work, err := repository.MaterializeExternalWork(ctx, domain.WorkMaterialization{
		Provider: "fixture", ExternalID: "legacy-series-member", Title: "Legacy member", Medium: domain.MediumLiterature, WorkType: "book",
	})
	if err != nil {
		t.Fatal(err)
	}
	collection := domain.WorkCollection{ID: "legacy-series-id", Title: "Legacy series", Type: domain.WorkCollectionTypeSeries}
	if err := repository.CreateWorkCollection(ctx, collection); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.SetWorkRelation(ctx, domain.WorkRelation{
		WorkID: work.Work.ID, Type: domain.WorkRelationPartOfSeries, TargetCollectionID: &collection.ID,
		Provenance: domain.WorkRelationProvenanceManual, Confidence: 1, ConfirmedByUser: true,
	}); err != nil {
		t.Fatal(err)
	}
	script, err := migrations.Files.ReadFile("0016_work_collection_external_identity.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(script)); err != nil {
		t.Fatalf("apply Work collection identity migration: %v", err)
	}
	if got, err := repository.GetWorkCollection(ctx, collection.ID); err != nil || got != collection {
		t.Fatalf("legacy collection after migration = %#v, %v; want %#v", got, err, collection)
	}
	relations, err := repository.GetWorkRelations(ctx, work.Work.ID)
	if err != nil || len(relations) != 1 || relations[0].TargetCollectionID == nil ||
		*relations[0].TargetCollectionID != collection.ID || !relations[0].ConfirmedByUser {
		t.Fatalf("legacy relation after migration = %#v, %v; want preserved manual collection edge", relations, err)
	}
	var identities int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_collection_external_identities`).Scan(&identities); err != nil || identities != 0 {
		t.Fatalf("new collection identity rows after additive migration = %d, %v; want zero", identities, err)
	}
}

func TestResolveWikidataWorkUsesUniqueExactExternalIdentity(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "wikidata-identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := NewSQLiteRepository(db)

	want, err := repository.MaterializeExternalWork(ctx, domain.WorkMaterialization{
		Provider: "openlibrary", ExternalID: "OL123W", Title: "Shared Title", Medium: domain.MediumLiterature, WorkType: "book",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.MaterializeExternalWork(ctx, domain.WorkMaterialization{
		Provider: "openlibrary", ExternalID: "OL999W", Title: "Shared Title", Medium: domain.MediumLiterature, WorkType: "book",
	}); err != nil {
		t.Fatal(err)
	}
	metadata := wikidataIdentityMetadata("Q42", []providers.RelationshipIdentifier{{Property: "P648", Value: "OL123W"}})
	got, err := repository.ResolveWikidataWork(ctx, metadata)
	if err != nil {
		t.Fatalf("ResolveWikidataWork() error = %v", err)
	}
	if got.Work.ID != want.Work.ID {
		t.Fatalf("resolved Work ID = %q, want exact Open Library identity Work %q despite title collision", got.Work.ID, want.Work.ID)
	}

	noMatch, err := repository.ResolveWikidataWork(ctx, wikidataIdentityMetadata("Q43", nil))
	if !errors.Is(err, ErrWikidataWorkIdentityNotFound) || noMatch.Work.ID != "" {
		t.Fatalf("unmatched QID resolution = %#v, %v; want fail-closed not-found", noMatch, err)
	}

	if _, err := repository.MaterializeExternalWork(ctx, domain.WorkMaterialization{
		Provider: "igdb", ExternalID: "1565", Title: "Different Title", Medium: domain.MediumGame, WorkType: "game",
	}); err != nil {
		t.Fatal(err)
	}
	ambiguous := wikidataIdentityMetadata("Q44", []providers.RelationshipIdentifier{
		{Property: "P648", Value: "OL123W"},
		{Property: "P9043", QualifierProperty: "P5794", Value: "1565"},
	})
	if got, err := repository.ResolveWikidataWork(ctx, ambiguous); !errors.Is(err, ErrWikidataWorkIdentityAmbiguous) || got.Work.ID != "" {
		t.Fatalf("multiple exact identity resolution = %#v, %v; want fail-closed ambiguity", got, err)
	}
}

func TestWikidataWorkCollectionIdentityIsIdempotentAndPreservesOpaqueID(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "wikidata-collection.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := NewSQLiteRepository(db)
	first, err := repository.CreateOrGetWorkCollectionByExternalIdentity(ctx, "wikidata", "Q23572", "Original title", domain.WorkCollectionTypeSeries)
	if err != nil {
		t.Fatalf("create QID-backed WorkCollection: %v", err)
	}
	if first.ID == "" || first.ID == "Q23572" {
		t.Fatalf("generated collection ID = %q; want a nonempty opaque local ID", first.ID)
	}
	second, err := repository.CreateOrGetWorkCollectionByExternalIdentity(ctx, "wikidata", "Q23572", "Refreshed provider title", domain.WorkCollectionType("unsupported-refresh-type"))
	if err != nil {
		t.Fatalf("get QID-backed WorkCollection after refresh: %v", err)
	}
	if second.ID != first.ID || second.Title != first.Title || second.Type != first.Type {
		t.Fatalf("provider refresh changed WorkCollection identity/title/type: first=%#v second=%#v", first, second)
	}
	byIdentity, err := repository.GetWorkCollectionByExternalIdentity(ctx, "wikidata", "Q23572")
	if err != nil || byIdentity != first {
		t.Fatalf("lookup by provider identity = %#v, %v; want original collection %#v", byIdentity, err, first)
	}
}

func wikidataIdentityMetadata(qid string, identifiers []providers.RelationshipIdentifier) providers.RelationshipMetadata {
	return providers.RelationshipMetadata{
		Provider: "wikidata", ExternalID: qid, EntityRevision: "1", ParserVersion: "wikidata-relationship-v2",
		FetchedAt: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC), Identifiers: identifiers,
	}
}

func TestMaterializeExternalWorkUsesExactIdentityAndPersistsOnlyWorkDisplayFields(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "universe.db")
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	repository := NewSQLiteRepository(db)
	inputs := []domain.WorkMaterialization{
		{Provider: "open-library", ExternalID: "OL123W", Title: "A Book", Summary: "  A safe\nbook summary.  ", Medium: domain.MediumLiterature, WorkType: "book"},
		{Provider: "tv-catalog", ExternalID: "42", Title: "A Series", Summary: "A safe series summary.", Medium: domain.MediumVideo, WorkType: "series"},
		{Provider: "catalog-a", ExternalID: "id", Title: "Same Display Title", Medium: domain.MediumVideo, WorkType: "series"},
		{Provider: "catalog-a", ExternalID: "ID", Title: "Same Display Title", Medium: domain.MediumVideo, WorkType: "series"},
		{Provider: "catalog-b", ExternalID: "id", Title: "Same Display Title", Medium: domain.MediumVideo, WorkType: "series"},
		{Provider: "untrusted-source", ExternalID: "unsafe-summary", Title: "Safe Work", Summary: "<script>discard me</script>", Medium: domain.MediumVideo, WorkType: "series"},
	}
	wantSummaries := []string{"A safe book summary.", "A safe series summary.", "", "", "", ""}
	graphs := make([]WorkGraph, 0, len(inputs))
	for index, input := range inputs {
		graph, err := repository.MaterializeExternalWork(ctx, input)
		if err != nil {
			t.Fatalf("materialize %s:%s: %v", input.Provider, input.ExternalID, err)
		}
		graphs = append(graphs, graph)
		if graph.Work.Title != input.Title || graph.Work.Summary != wantSummaries[index] || graph.Work.Medium != input.Medium || graph.Work.WorkType != input.WorkType {
			t.Errorf("materialized Work = %#v, want only safe display fields from %#v", graph.Work, input)
		}
		if len(graph.ExternalIdentities) != 1 || graph.ExternalIdentities[0].Provider != input.Provider || graph.ExternalIdentities[0].ExternalID != input.ExternalID {
			t.Errorf("materialized identities = %#v, want exact %s:%s", graph.ExternalIdentities, input.Provider, input.ExternalID)
		}
		if len(graph.Editions)+len(graph.Assets)+len(graph.Parts)+len(graph.Locations) != 0 {
			t.Errorf("provider-only Work fabricated catalog inventory: %#v", graph)
		}
	}
	for first := range graphs {
		for second := first + 1; second < len(graphs); second++ {
			if graphs[first].Work.ID == graphs[second].Work.ID {
				t.Errorf("distinct exact identities %s:%s and %s:%s shared Work %q",
					inputs[first].Provider, inputs[first].ExternalID, inputs[second].Provider, inputs[second].ExternalID, graphs[first].Work.ID)
			}
		}
	}

	repeated, err := repository.MaterializeExternalWork(ctx, inputs[0])
	if err != nil {
		t.Fatalf("repeat exact provider identity: %v", err)
	}
	if repeated.Work.ID != graphs[0].Work.ID || repeated.Work.Title != graphs[0].Work.Title {
		t.Fatalf("repeat materialization = %#v, want original Work %q with unchanged display metadata", repeated.Work, graphs[0].Work.ID)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = database.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen materialized Works: %v", err)
	}
	defer db.Close()
	repository = NewSQLiteRepository(db)
	afterRestart, err := repository.MaterializeExternalWork(ctx, inputs[0])
	if err != nil {
		t.Fatalf("repeat materialization after restart: %v", err)
	}
	if afterRestart.Work.ID != graphs[0].Work.ID || afterRestart.ExternalIdentities[0].ExternalID != "OL123W" {
		t.Fatalf("reopened materialization = %#v, want stable exact identity and Work ID %q", afterRestart, graphs[0].Work.ID)
	}
	var works, identities, editions, assets int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM works").Scan(&works); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM external_identities").Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM editions").Scan(&editions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM assets").Scan(&assets); err != nil {
		t.Fatal(err)
	}
	if works != len(inputs) || identities != len(inputs) || editions != 0 || assets != 0 {
		t.Fatalf("materialized row counts works=%d identities=%d editions=%d assets=%d; want %d/%d/0/0",
			works, identities, editions, assets, len(inputs), len(inputs))
	}
}

func TestMaterializeExternalWorkRejectsUnsafeOrIncompleteDisplayIdentity(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "universe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := NewSQLiteRepository(db)
	tests := []struct {
		name  string
		input domain.WorkMaterialization
	}{
		{name: "empty provider", input: domain.WorkMaterialization{ExternalID: "id", Title: "Title", Medium: domain.MediumVideo, WorkType: "series"}},
		{name: "empty external ID", input: domain.WorkMaterialization{Provider: "provider", Title: "Title", Medium: domain.MediumVideo, WorkType: "series"}},
		{name: "blank title", input: domain.WorkMaterialization{Provider: "provider", ExternalID: "id", Title: "  ", Medium: domain.MediumVideo, WorkType: "series"}},
		{name: "unsupported medium", input: domain.WorkMaterialization{Provider: "provider", ExternalID: "id", Title: "Title", Medium: "unknown", WorkType: "series"}},
		{name: "blank work type", input: domain.WorkMaterialization{Provider: "provider", ExternalID: "id", Title: "Title", Medium: domain.MediumVideo, WorkType: "  "}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := repository.MaterializeExternalWork(ctx, test.input); err == nil {
				t.Fatal("invalid Work materialization unexpectedly succeeded")
			}
		})
	}
	var works, identities int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM works").Scan(&works); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM external_identities").Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if works != 0 || identities != 0 {
		t.Fatalf("invalid materializations persisted works=%d identities=%d, want 0/0", works, identities)
	}
}

func TestUniverseMembershipCorrectionsAliasesExclusionsAndDeletionPersist(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "universe.db")
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	repository := NewSQLiteRepository(db)
	firstUniverse := domain.Universe{ID: "universe-first", Title: "First Universe"}
	secondUniverse := domain.Universe{ID: "universe-second", Title: "Second Universe"}
	for _, universe := range []domain.Universe{firstUniverse, secondUniverse} {
		if err := repository.CreateUniverse(ctx, universe); err != nil {
			t.Fatalf("create Universe %q: %v", universe.ID, err)
		}
	}
	work, err := repository.MaterializeExternalWork(ctx, domain.WorkMaterialization{
		Provider: "tv-catalog", ExternalID: "record-1", Title: "Example Series", Medium: domain.MediumVideo, WorkType: "series",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
		UniverseID: firstUniverse.ID, WorkID: work.Work.ID, Provenance: "manual", Confidence: 1.01,
	}); err == nil {
		t.Fatal("out-of-range membership confidence unexpectedly succeeded")
	}
	membership, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
		UniverseID: firstUniverse.ID, WorkID: work.Work.ID, Provenance: "manual", Confidence: 1, ConfirmedByUser: true,
	})
	if err != nil {
		t.Fatalf("accept Work into first Universe: %v", err)
	}
	if membership.AcceptedAt.IsZero() || membership.Confidence != 1 || membership.Provenance != "manual" {
		t.Fatalf("accepted membership metadata = %#v, want durable time, provenance, and confidence", membership)
	}
	alias := domain.UniverseAlias{UniverseID: firstUniverse.ID, Alias: "Example: The Series", Provenance: "manual"}
	addedAlias, err := repository.AddUniverseAlias(ctx, alias)
	if err != nil {
		t.Fatalf("add alias: %v", err)
	}
	if addedAlias.CreatedAt.IsZero() || addedAlias.Provenance != "manual" {
		t.Fatalf("added alias metadata = %#v, want durable time and provenance", addedAlias)
	}
	if _, err := repository.AddUniverseAlias(ctx, alias); err != nil {
		t.Fatalf("repeat exact alias: %v", err)
	}
	removed, err := repository.RemoveUniverseMembership(ctx, work.Work.ID, firstUniverse.ID, "not part of this universe")
	if err != nil {
		t.Fatalf("remove accepted membership: %v", err)
	}
	if removed.WorkID != work.Work.ID || removed.UniverseID != firstUniverse.ID || removed.Reason == "" || removed.RecordedAt.IsZero() {
		t.Fatalf("removal decision = %#v, want durable exclusion for removed pair", removed)
	}
	firstGraph, err := repository.GetUniverse(ctx, firstUniverse.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstGraph.Memberships) != 0 || len(firstGraph.Exclusions) != 1 || len(firstGraph.Aliases) != 1 {
		t.Fatalf("removed Universe graph = %#v, want alias + exclusion and no active membership", firstGraph)
	}

	if _, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
		UniverseID: firstUniverse.ID, WorkID: work.Work.ID, Provenance: "manual", Confidence: 1, ConfirmedByUser: true,
	}); err != nil {
		t.Fatalf("explicitly reaccept excluded Work: %v", err)
	}
	firstGraph, err = repository.GetUniverse(ctx, firstUniverse.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstGraph.Memberships) != 1 || len(firstGraph.Exclusions) != 0 || len(firstGraph.Aliases) != 1 {
		t.Fatalf("reaccepted Universe graph = %#v, want membership, alias, and no stale exclusion", firstGraph)
	}

	if _, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
		UniverseID: secondUniverse.ID, WorkID: work.Work.ID, Provenance: "manual", Confidence: 1, ConfirmedByUser: true,
	}); err != nil {
		t.Fatalf("correct membership into second Universe: %v", err)
	}
	firstGraph, err = repository.GetUniverse(ctx, firstUniverse.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondGraph, err := repository.GetUniverse(ctx, secondUniverse.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstGraph.Memberships) != 0 || len(firstGraph.Exclusions) != 1 || len(secondGraph.Memberships) != 1 {
		t.Fatalf("corrected membership state first=%#v second=%#v; want old exclusion and one active target", firstGraph, secondGraph)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = database.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen Universe state: %v", err)
	}
	defer db.Close()
	repository = NewSQLiteRepository(db)
	secondGraph, err = repository.GetUniverse(ctx, secondUniverse.ID)
	if err != nil {
		t.Fatalf("load Universe after restart: %v", err)
	}
	firstGraph, err = repository.GetUniverse(ctx, firstUniverse.ID)
	if err != nil {
		t.Fatalf("load prior Universe after restart: %v", err)
	}
	if len(secondGraph.Memberships) != 1 || secondGraph.Memberships[0].WorkID != work.Work.ID ||
		len(firstGraph.Exclusions) != 1 || len(firstGraph.Aliases) != 1 {
		t.Fatalf("reopened Universe state second=%#v first=%#v; want membership, exclusion, and alias", secondGraph, firstGraph)
	}

	if err := repository.DeleteUniverse(ctx, secondUniverse.ID); err != nil {
		t.Fatalf("delete second Universe: %v", err)
	}
	if _, err := repository.GetUniverse(ctx, secondUniverse.ID); err == nil {
		t.Fatal("deleted Universe still loaded")
	}
	if _, err := repository.GetWork(ctx, work.Work.ID); err != nil {
		t.Fatalf("deleting Universe removed its Work: %v", err)
	}
	if _, err := repository.GetWorkUniverseMembership(ctx, work.Work.ID); err == nil {
		t.Fatal("membership survived deletion of its Universe")
	}
}

func TestUniverseExclusionCannotBeClearedByNonConfirmedProviderOrRuleWrite(t *testing.T) {
	for _, source := range []string{"provider", "rule"} {
		t.Run(source, func(t *testing.T) {
			ctx := context.Background()
			db, err := database.Open(ctx, filepath.Join(t.TempDir(), "universe.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			repository := NewSQLiteRepository(db)
			universe := domain.Universe{ID: "excluded-universe", Title: "Excluded Universe"}
			if err := repository.CreateUniverse(ctx, universe); err != nil {
				t.Fatal(err)
			}
			work, err := repository.MaterializeExternalWork(ctx, domain.WorkMaterialization{
				Provider: "test-provider", ExternalID: "record-1", Title: "Excluded Work", Medium: domain.MediumVideo, WorkType: "series",
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
				UniverseID: universe.ID, WorkID: work.Work.ID, Provenance: "provider", Confidence: 0.8,
			}); err != nil {
				t.Fatalf("create initial non-confirmed membership: %v", err)
			}
			if _, err := repository.RemoveUniverseMembership(ctx, work.Work.ID, universe.ID, "user rejected this relation"); err != nil {
				t.Fatalf("record explicit exclusion: %v", err)
			}
			if _, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
				UniverseID: universe.ID, WorkID: work.Work.ID, Provenance: domain.UniverseMembershipProvenance(source), Confidence: 0.95,
			}); err == nil {
				t.Errorf("non-confirmed %s write silently re-added an excluded pair", source)
			}
			graph, err := repository.GetUniverse(ctx, universe.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(graph.Memberships) != 0 || len(graph.Exclusions) != 1 {
				t.Errorf("after non-confirmed %s write, memberships=%d exclusions=%d; want 0/1",
					source, len(graph.Memberships), len(graph.Exclusions))
			}
		})
	}
}

func TestConfirmedMembershipOverridesExclusionAndSurvivesNonConfirmedReruns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "universe.db")
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	repository := NewSQLiteRepository(db)
	firstUniverse := domain.Universe{ID: "confirmed-first", Title: "First Universe"}
	secondUniverse := domain.Universe{ID: "confirmed-second", Title: "Second Universe"}
	for _, universe := range []domain.Universe{firstUniverse, secondUniverse} {
		if err := repository.CreateUniverse(ctx, universe); err != nil {
			t.Fatal(err)
		}
	}
	work, err := repository.MaterializeExternalWork(ctx, domain.WorkMaterialization{
		Provider: "tv-catalog", ExternalID: "confirmed-record", Title: "Confirmed Work", Medium: domain.MediumVideo, WorkType: "series",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.RemoveUniverseMembership(ctx, work.Work.ID, firstUniverse.ID, "initial user exclusion"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
		UniverseID: firstUniverse.ID, WorkID: work.Work.ID, Provenance: "provider", Confidence: 0.8,
	}); err == nil {
		t.Fatal("non-confirmed write unexpectedly bypassed existing exclusion")
	}
	excluded, err := repository.IsUniverseWorkExcluded(ctx, work.Work.ID, firstUniverse.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !excluded {
		t.Fatal("non-confirmed write removed the existing exclusion")
	}

	confirmed, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
		UniverseID: firstUniverse.ID, WorkID: work.Work.ID, Provenance: "provider", Confidence: 0.92, ConfirmedByUser: true,
	})
	if err != nil {
		t.Fatalf("explicitly confirm provider-sourced membership: %v", err)
	}
	if !confirmed.ConfirmedByUser || confirmed.Provenance != "provider" || confirmed.Confidence != 0.92 {
		t.Fatalf("confirmed membership = %#v, want provider provenance plus independent user confirmation", confirmed)
	}

	if _, err := repository.RemoveUniverseMembership(ctx, work.Work.ID, secondUniverse.ID, "second candidate excluded"); err != nil {
		t.Fatal(err)
	}
	sameUniverseRerun, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
		UniverseID: firstUniverse.ID, WorkID: work.Work.ID, Provenance: "rule", Confidence: 0.4,
	})
	if err != nil {
		t.Fatalf("same-Universe non-confirmed rerun: %v", err)
	}
	if sameUniverseRerun.UniverseID != firstUniverse.ID || sameUniverseRerun.Provenance != "provider" ||
		sameUniverseRerun.Confidence != 0.92 || !sameUniverseRerun.ConfirmedByUser {
		t.Fatalf("same-Universe rerun downgraded confirmed membership: %#v", sameUniverseRerun)
	}
	differentUniverseRerun, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
		UniverseID: secondUniverse.ID, WorkID: work.Work.ID, Provenance: "rule", Confidence: 0.99,
	})
	if err != nil {
		t.Fatalf("different-Universe non-confirmed rerun: %v", err)
	}
	if differentUniverseRerun.UniverseID != firstUniverse.ID || differentUniverseRerun.Provenance != "provider" ||
		differentUniverseRerun.Confidence != 0.92 || !differentUniverseRerun.ConfirmedByUser {
		t.Fatalf("different-Universe rerun moved or downgraded confirmed membership: %#v", differentUniverseRerun)
	}
	if excluded, err := repository.IsUniverseWorkExcluded(ctx, work.Work.ID, secondUniverse.ID); err != nil || !excluded {
		t.Fatalf("non-confirmed rerun exclusion state = %t, err=%v; want preserved", excluded, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = database.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen confirmed membership: %v", err)
	}
	defer db.Close()
	repository = NewSQLiteRepository(db)
	reopened, err := repository.GetWorkUniverseMembership(ctx, work.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.UniverseID != firstUniverse.ID || reopened.Provenance != "provider" || reopened.Confidence != 0.92 || !reopened.ConfirmedByUser {
		t.Fatalf("reopened membership = %#v, want original provider-sourced user confirmation", reopened)
	}

	reaccepted, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
		UniverseID: secondUniverse.ID, WorkID: work.Work.ID, Provenance: "rule", Confidence: 0.95, ConfirmedByUser: true,
	})
	if err != nil {
		t.Fatalf("explicitly confirm rule-sourced reacceptance: %v", err)
	}
	if reaccepted.UniverseID != secondUniverse.ID || reaccepted.Provenance != "rule" || !reaccepted.ConfirmedByUser {
		t.Fatalf("confirmed rule-sourced membership = %#v", reaccepted)
	}
	firstGraph, err := repository.GetUniverse(ctx, firstUniverse.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondGraph, err := repository.GetUniverse(ctx, secondUniverse.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstGraph.Memberships) != 0 || len(firstGraph.Exclusions) != 1 ||
		len(secondGraph.Memberships) != 1 || len(secondGraph.Exclusions) != 0 || !secondGraph.Memberships[0].ConfirmedByUser {
		t.Fatalf("explicit reacceptance state first=%#v second=%#v; want old exclusion and confirmed new membership", firstGraph, secondGraph)
	}
}

func TestConfirmWorkPreservesExistingMembershipProvenanceAndConfidence(t *testing.T) {
	tests := []struct {
		name                     string
		provenance               domain.UniverseMembershipProvenance
		confidence               float64
		wantMembershipAfterReset bool
	}{
		{name: "provider evidence", provenance: domain.UniverseMembershipProvenanceProvider, confidence: 0.83, wantMembershipAfterReset: true},
		{name: "rule evidence", provenance: domain.UniverseMembershipProvenanceRule, confidence: 0.71, wantMembershipAfterReset: true},
		{name: "new manual membership", wantMembershipAfterReset: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := database.Open(ctx, filepath.Join(t.TempDir(), "universe.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			repository := NewSQLiteRepository(db)
			universeID := domain.UniverseID("confirmed-universe")
			if err := repository.CreateUniverse(ctx, domain.Universe{ID: universeID, Title: "Confirmed Universe"}); err != nil {
				t.Fatal(err)
			}
			input := domain.WorkMaterialization{
				Provider: "tv-catalog", ExternalID: "confirmed-record", Title: "Confirmed Work",
				Medium: domain.MediumVideo, WorkType: "series",
			}
			work, err := repository.MaterializeExternalWork(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if test.provenance != "" {
				if _, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
					UniverseID: universeID, WorkID: work.Work.ID, Provenance: test.provenance, Confidence: test.confidence,
				}); err != nil {
					t.Fatalf("set existing %s membership: %v", test.provenance, err)
				}
			}

			application := NewUniverseApplication(repository)
			confirmed, err := application.ConfirmWork(ctx, universeID, input)
			if err != nil {
				t.Fatalf("confirm exact Work: %v", err)
			}
			if len(confirmed.Detail.Memberships) != 1 {
				t.Fatalf("confirmed memberships = %#v, want one accepted membership", confirmed.Detail.Memberships)
			}
			membership := confirmed.Detail.Memberships[0]
			wantProvenance, wantConfidence := test.provenance, test.confidence
			if wantProvenance == "" {
				wantProvenance, wantConfidence = domain.UniverseMembershipProvenanceManual, 1
			}
			if membership.Provenance != wantProvenance || membership.Confidence != wantConfidence || !membership.ConfirmedByUser {
				t.Fatalf("confirmed membership = %#v, want provenance %q, confidence %g, and independent user confirmation", membership, wantProvenance, wantConfidence)
			}

			reset, err := application.ResetDecision(ctx, universeID, input.Provider, input.ExternalID, UniverseResetClearConfirmation)
			if err != nil {
				t.Fatalf("reset user confirmation: %v", err)
			}
			if !test.wantMembershipAfterReset {
				if len(reset.Memberships) != 0 {
					t.Fatalf("reset manual-only membership = %#v, want no membership", reset.Memberships)
				}
				return
			}
			if len(reset.Memberships) != 1 {
				t.Fatalf("reset confirmed source-backed membership = %#v, want retained membership", reset.Memberships)
			}
			membership = reset.Memberships[0]
			if membership.Provenance != test.provenance || membership.Confidence != test.confidence || membership.ConfirmedByUser {
				t.Fatalf("reset source-backed membership = %#v, want original %s/%g provenance/confidence and cleared confirmation", membership, test.provenance, test.confidence)
			}
		})
	}
}

func TestConfirmWorkPreservesLegacyMembershipProvenanceAndRejectsNewCustomProvenance(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "legacy-provenance.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := NewSQLiteRepository(db)
	const universeID = domain.UniverseID("legacy-provenance-universe")
	if err := repository.CreateUniverse(ctx, domain.Universe{ID: universeID, Title: "Legacy Provenance Universe"}); err != nil {
		t.Fatal(err)
	}
	input := domain.WorkMaterialization{
		Provider: "tv-catalog", ExternalID: "legacy-provenance-work", Title: "Legacy Provenance Work",
		Medium: domain.MediumVideo, WorkType: "series",
	}
	work, err := repository.MaterializeExternalWork(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	// This models a row preserved by the version-seven-to-eight migration:
	// remove only the INSERT guard to seed legacy data; keep the UPDATE guard.
	if _, err := db.ExecContext(ctx, `DROP TRIGGER universe_memberships_provenance_insert`); err != nil {
		t.Fatal(err)
	}
	const legacyProvenance = domain.UniverseMembershipProvenance("legacy-custom-source")
	const acceptedAt = "2026-09-30T12:00:00Z"
	if _, err := db.ExecContext(ctx, `INSERT INTO universe_memberships
		(work_id, universe_id, provenance, confidence, accepted_at_utc, confirmed_by_user)
		VALUES (?, ?, ?, ?, ?, 0)`, work.Work.ID, universeID, legacyProvenance, 0.75, acceptedAt); err != nil {
		t.Fatalf("seed preserved legacy membership: %v", err)
	}

	application := NewUniverseApplication(repository)
	confirmed, err := application.ConfirmWork(ctx, universeID, input)
	if err != nil {
		t.Fatalf("reconfirm legacy exact Work: %v", err)
	}
	if len(confirmed.Detail.Memberships) != 1 {
		t.Fatalf("confirmed memberships = %#v, want one preserved legacy membership", confirmed.Detail.Memberships)
	}
	membership := confirmed.Detail.Memberships[0]
	var storedAcceptedAt string
	if err := db.QueryRowContext(ctx, `SELECT accepted_at_utc FROM universe_memberships WHERE work_id = ?`, work.Work.ID).Scan(&storedAcceptedAt); err != nil {
		t.Fatal(err)
	}
	if membership.Provenance != legacyProvenance || membership.Confidence != 0.75 || !membership.ConfirmedByUser || storedAcceptedAt != acceptedAt {
		t.Fatalf("reconfirmed legacy membership = %#v, want provenance/confidence/time preserved and explicitly confirmed", membership)
	}

	newInput := domain.WorkMaterialization{
		Provider: "tv-catalog", ExternalID: "new-custom-provenance-work", Title: "New Custom Provenance Work",
		Medium: domain.MediumVideo, WorkType: "series",
	}
	newWork, err := repository.MaterializeExternalWork(ctx, newInput)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.SetUniverseMembership(ctx, domain.UniverseMembership{
		UniverseID: universeID, WorkID: newWork.Work.ID, Provenance: legacyProvenance, Confidence: 0.75,
	}); err == nil {
		t.Fatal("new membership accepted unsupported custom provenance")
	}
}
