package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"lernae/internal/database"
	"lernae/internal/domain"
)

func TestSoulcaliburWorkGraphRoundTripsByIDAndExternalIdentity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lernae.db")
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	repository := NewSQLiteRepository(db)
	want := soulcaliburFixture()
	if err := repository.CreateGraph(ctx, want); err != nil {
		t.Fatalf("create Soulcalibur graph: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = database.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen catalog database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository = NewSQLiteRepository(db)

	byID, err := repository.GetWork(ctx, want.Work.ID)
	if err != nil {
		t.Fatalf("get graph by Work ID: %v", err)
	}
	if !reflect.DeepEqual(byID, want) {
		t.Fatalf("reopened graph by Work ID = %#v, want %#v", byID, want)
	}
	byIdentity, err := repository.GetWorkByExternalIdentity(ctx, "igdb", "TEST_IGDB_ID")
	if err != nil {
		t.Fatalf("get graph by ExternalIdentity: %v", err)
	}
	if !reflect.DeepEqual(byIdentity, want) {
		t.Fatalf("reopened graph by ExternalIdentity = %#v, want %#v", byIdentity, want)
	}

	if got := byID.Editions[0]; got.Platform != "gamecube" || got.Format != "disc_image" {
		t.Fatalf("Edition platform/format = %q/%q, want gamecube/disc_image", got.Platform, got.Format)
	}
}

func TestSoulcaliburFixtureMatchesRequestedFixtureIdentifiers(t *testing.T) {
	graph := soulcaliburFixture()
	if got, want := graph.Assets[0].ID, domain.AssetID("soulcalibur2-gamecube"); got != want {
		t.Fatalf("fixture Asset ID = %q, want %q", got, want)
	}
	if got, want := graph.ExternalIdentities[0].ExternalID, "TEST_IGDB_ID"; got != want {
		t.Fatalf("fixture ExternalIdentity ID = %q, want %q", got, want)
	}
	if got, want := graph.Locations[0].Locator, "testremote:games/gamecube/Soulcalibur II.iso"; got != want {
		t.Fatalf("fixture archive locator = %q, want %q", got, want)
	}
}

func TestCatalogGraphSupportsMultipleIndexedPartsAndLocations(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository := NewSQLiteRepository(db)
	graph := soulcaliburFixture()
	firstIndex, secondIndex := 1, 2
	graph.Parts = []domain.AssetPart{
		{ID: "part-soulcalibur-cue", AssetID: graph.Assets[0].ID, PartIndex: &firstIndex, Role: "cue", Filename: "Soulcalibur II.cue", SizeBytes: 54},
		{ID: "part-soulcalibur-bin", AssetID: graph.Assets[0].ID, PartIndex: &secondIndex, Role: "bin", Filename: "Soulcalibur II.bin", SizeBytes: 1449999946},
	}
	graph.Locations = append(graph.Locations, domain.AssetLocation{
		ID: "location-soulcalibur-cache", AssetID: graph.Assets[0].ID,
		StorageProviderID: "filesystem", Locator: "/cache/games/Soulcalibur II.iso", LocationClass: "local_cache",
	})

	if err := repository.CreateGraph(ctx, graph); err != nil {
		t.Fatalf("create multi-part, multi-location graph: %v", err)
	}
	got, err := repository.GetWork(ctx, graph.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Parts) != 2 || got.Parts[0].PartIndex == nil || *got.Parts[0].PartIndex != 1 || got.Parts[0].Role != "cue" || got.Parts[1].PartIndex == nil || *got.Parts[1].PartIndex != 2 || got.Parts[1].Role != "bin" {
		t.Fatalf("parts are not explicitly ordered and role-addressable: %#v", got.Parts)
	}
	if len(got.Locations) != 2 {
		t.Fatalf("got %d AssetLocations, want both archive and local cache: %#v", len(got.Locations), got.Locations)
	}
}

func TestSQLiteConstraintsRejectInvalidForeignKeys(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cases := []struct {
		name string
		sql  string
	}{
		{name: "Edition without Work", sql: `INSERT INTO editions (id, work_id, format) VALUES ('bad-edition', 'missing-work', 'disc_image')`},
		{name: "Asset without Edition", sql: `INSERT INTO assets (id, edition_id, kind, total_size_bytes) VALUES ('bad-asset', 'missing-edition', 'disc_image', 1)`},
		{name: "AssetPart without Asset", sql: `INSERT INTO asset_parts (id, asset_id, role, filename, size_bytes) VALUES ('bad-part', 'missing-asset', 'rom', 'bad.iso', 1)`},
		{name: "AssetLocation without Asset", sql: `INSERT INTO asset_locations (id, asset_id, storage_provider_id, locator, location_class) VALUES ('bad-location', 'missing-asset', 'filesystem', '/cache/bad.iso', 'local_cache')`},
		{name: "ExternalIdentity without Work", sql: `INSERT INTO external_identities (id, work_id, provider, external_id) VALUES ('bad-identity', 'missing-work', 'igdb', '1')`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, test.sql); err == nil {
				t.Fatal("insert with a missing parent unexpectedly succeeded")
			}
		})
	}
}

func TestCatalogRejectsDuplicateMappingsAndPartAddresses(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository := NewSQLiteRepository(db)
	graph := soulcaliburFixture()
	if err := repository.CreateGraph(ctx, graph); err != nil {
		t.Fatal(err)
	}

	duplicateEdition := `INSERT INTO editions (id, work_id, label, platform, format) VALUES ('duplicate-edition', 'work-soulcalibur-ii', 'GameCube', 'gamecube', 'disc_image')`
	if _, err := db.ExecContext(ctx, duplicateEdition); err == nil {
		t.Fatal("duplicate Edition attributes unexpectedly succeeded")
	}
	duplicatePartIndex := `INSERT INTO asset_parts (id, asset_id, part_index, role, filename, size_bytes) VALUES ('duplicate-index', 'soulcalibur2-gamecube', 1, 'second-rom', 'another.iso', 1)`
	if _, err := db.ExecContext(ctx, duplicatePartIndex); err == nil {
		t.Fatal("duplicate AssetPart index unexpectedly succeeded")
	}
	duplicateLocation := `INSERT INTO asset_locations (id, asset_id, storage_provider_id, locator, location_class) VALUES ('duplicate-location', 'soulcalibur2-gamecube', 'rclone', 'testremote:games/gamecube/Soulcalibur II.iso', 'archive')`
	if _, err := db.ExecContext(ctx, duplicateLocation); err == nil {
		t.Fatal("duplicate AssetLocation unexpectedly succeeded")
	}

	duplicateIdentityGraph := soulcaliburFixture()
	duplicateIdentityGraph.Work.ID = "other-work"
	duplicateIdentityGraph.Editions[0].ID = "other-edition"
	duplicateIdentityGraph.Editions[0].WorkID = duplicateIdentityGraph.Work.ID
	duplicateIdentityGraph.Assets[0].ID = "other-asset"
	duplicateIdentityGraph.Assets[0].EditionID = duplicateIdentityGraph.Editions[0].ID
	duplicateIdentityGraph.Parts[0].ID = "other-part"
	duplicateIdentityGraph.Parts[0].AssetID = duplicateIdentityGraph.Assets[0].ID
	duplicateIdentityGraph.Locations[0].ID = "other-location"
	duplicateIdentityGraph.Locations[0].AssetID = duplicateIdentityGraph.Assets[0].ID
	duplicateIdentityGraph.ExternalIdentities[0].ID = "other-identity"
	duplicateIdentityGraph.ExternalIdentities[0].WorkID = duplicateIdentityGraph.Work.ID
	if err := repository.CreateGraph(ctx, duplicateIdentityGraph); err == nil {
		t.Fatal("duplicate provider/external ID mapping unexpectedly succeeded")
	}
	if _, err := repository.GetWork(ctx, duplicateIdentityGraph.Work.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed graph transaction left Work behind: err = %v", err)
	}
}

func TestStagingPathCannotBecomeReadyLocalCacheLocation(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository := NewSQLiteRepository(db)
	graph := soulcaliburFixture()
	graph.Locations[0].LocationClass = "local_cache"
	graph.Locations[0].Locator = "/cache/.staging/job-123/Soulcalibur II.iso"
	if err := repository.CreateGraph(ctx, graph); err == nil {
		t.Fatal(".staging AssetLocation was accepted as ready local cache")
	}
	if _, err := repository.GetWork(ctx, graph.Work.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("staging rejection did not roll back graph: err = %v", err)
	}
}

func soulcaliburFixture() WorkGraph {
	partIndex := 1
	workID := domain.WorkID("work-soulcalibur-ii")
	editionID := domain.EditionID("edition-soulcalibur-ii-gamecube")
	assetID := domain.AssetID("soulcalibur2-gamecube")
	return WorkGraph{
		Work: domain.Work{ID: workID, Title: "Soulcalibur II", Medium: domain.MediumGame, WorkType: "game"},
		Editions: []domain.Edition{{
			ID: editionID, WorkID: workID, Label: "GameCube", Platform: "gamecube", Format: "disc_image",
		}},
		Assets: []domain.Asset{{
			ID: assetID, EditionID: editionID, Kind: "disc_image", TotalSizeBytes: 1450000000,
		}},
		Parts: []domain.AssetPart{{
			ID: "part-soulcalibur2-rom", AssetID: assetID, PartIndex: &partIndex, Role: "rom",
			Filename: "Soulcalibur II.iso", SizeBytes: 1450000000,
		}},
		Locations: []domain.AssetLocation{{
			ID: "location-soulcalibur2-archive", AssetID: assetID, StorageProviderID: "rclone",
			Locator: "testremote:games/gamecube/Soulcalibur II.iso", LocationClass: "archive",
		}},
		ExternalIdentities: []domain.ExternalIdentity{{
			ID: "identity-soulcalibur2-igdb", WorkID: workID, Provider: "igdb", ExternalID: "TEST_IGDB_ID",
		}},
	}
}
