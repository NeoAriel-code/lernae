package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"lernae/internal/database"
	"lernae/internal/domain"
	"lernae/internal/inventory"
	"lernae/internal/providers"
)

func TestInventoryBinderAssignsOpaqueLocalWorkIDAndReusesExternalIdentity(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository := NewSQLiteRepository(db)
	source := syntheticBinderSource(t, "work-record-42", []inventory.ManifestLocation{
		{Class: "archive", Provider: "test-storage", Locator: "test://archive/synthetic.bin"},
	})
	identity := syntheticWorkIdentity()
	details := syntheticWorkDetails(identity)
	binder := NewInventoryBinder(repository, source, source)

	first, err := binder.Resolve(ctx, identity, "platform-x", "disc-image", details)
	if err != nil {
		t.Fatalf("first inventory resolution: %v", err)
	}
	if details.calls != 1 || details.provider != identity.Provider || details.externalID != identity.ExternalID {
		t.Fatalf("details lookup = calls:%d identity:%s:%s, want exactly %s:%s", details.calls, details.provider, details.externalID, identity.Provider, identity.ExternalID)
	}
	if first.Graph.Work.Title != "Trusted Synthetic Work" || first.Graph.Work.Medium != domain.MediumGame || first.Graph.Work.WorkType != "canonical-game" {
		t.Fatalf("first Work = %#v, want trusted normalized provider details", first.Graph.Work)
	}
	providerDerivedID := domain.WorkID(stableCatalogID("work", identity.Provider, identity.ExternalID))
	if first.Graph.Work.ID == "" || first.Graph.Work.ID == providerDerivedID {
		t.Fatalf("new Work.ID = %q, want an opaque local ID independent of provider identity", first.Graph.Work.ID)
	}

	persisted, err := repository.GetWorkByExternalIdentity(ctx, identity.Provider, identity.ExternalID)
	if err != nil {
		t.Fatalf("lookup newly persisted external identity: %v", err)
	}
	if persisted.Work.ID != first.Graph.Work.ID {
		t.Fatalf("persisted Work.ID = %q, first resolved Work.ID = %q", persisted.Work.ID, first.Graph.Work.ID)
	}

	second, err := binder.Resolve(ctx, identity, "platform-x", "disc-image", details)
	if err != nil {
		t.Fatalf("repeated inventory resolution: %v", err)
	}
	if details.calls != 1 {
		t.Fatalf("existing canonical Work reloaded trusted details %d times, want no second lookup", details.calls-1)
	}
	if second.Graph.Work.ID != first.Graph.Work.ID {
		t.Fatalf("repeated Work.ID = %q, want reused local ID %q", second.Graph.Work.ID, first.Graph.Work.ID)
	}
	persisted, err = repository.GetWorkByExternalIdentity(ctx, identity.Provider, identity.ExternalID)
	if err != nil {
		t.Fatalf("lookup reused external identity: %v", err)
	}
	if persisted.Work.ID != first.Graph.Work.ID {
		t.Fatalf("external identity now points to Work.ID %q, want %q", persisted.Work.ID, first.Graph.Work.ID)
	}
	assertCatalogCounts(t, db, map[string]int{"works": 1, "external_identities": 1})
}

func TestInventoryBinderCreatesAndReusesCanonicalGraphAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "catalog.db")
	source := syntheticBinderSource(t, "work-record-42", []inventory.ManifestLocation{
		{Class: "archive", Provider: "test-storage", Locator: "test://archive/synthetic.bin"},
		{Class: "local_cache", Provider: "test-cache", Locator: "/cache/synthetic.bin"},
	})
	identity := syntheticWorkIdentity()
	details := syntheticWorkDetails(identity)

	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository := NewSQLiteRepository(db)
	binder := NewInventoryBinder(repository, source, source)
	first, err := binder.Resolve(ctx, identity, "platform-x", "disc-image", details)
	if err != nil {
		t.Fatalf("first inventory resolution: %v", err)
	}
	assertResolvedSyntheticGraph(t, first)
	if first.Availability != AvailabilityArchived {
		t.Fatalf("availability = %q, want archived because local-cache declaration is unverified", first.Availability)
	}

	second, err := binder.Resolve(ctx, identity, "platform-x", "disc-image", details)
	if err != nil {
		t.Fatalf("repeated inventory resolution: %v", err)
	}
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("repeated resolution = %#v, want identical graph/result %#v", second, first)
	}
	assertCatalogCounts(t, db, map[string]int{"works": 1, "external_identities": 1, "editions": 1, "assets": 1, "asset_parts": 1, "asset_locations": 2})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = database.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen catalog database: %v", err)
	}
	repository = NewSQLiteRepository(db)
	binder = NewInventoryBinder(repository, source, source)
	third, err := binder.Resolve(ctx, identity, "platform-x", "disc-image", details)
	if err != nil {
		t.Fatalf("resolution after reopen: %v", err)
	}
	if !reflect.DeepEqual(third, first) {
		t.Fatalf("resolution after reopen = %#v, want identical graph/result %#v", third, first)
	}
	assertCatalogCounts(t, db, map[string]int{"works": 1, "external_identities": 1, "editions": 1, "assets": 1, "asset_parts": 1, "asset_locations": 2})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInventoryBinderResolvesCheckedInSoulcaliburFixtureByCanonicalWorkIdentity(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	source := inventory.NewManifestInventorySource(filepath.Join("..", "..", "examples", "manifest.json"))
	repository := NewSQLiteRepository(db)
	binder := NewInventoryBinder(repository, source, source)
	identity := providers.ExternalWorkIdentity{Provider: "igdb", ExternalID: "1565"}
	details := &binderWorkDetailsStub{details: domain.WorkDetails{
		Provider: "igdb", ExternalID: "1565", Title: "Soulcalibur II",
		Medium: domain.MediumGame, WorkType: "game",
	}}

	first, err := binder.Resolve(ctx, identity, "gamecube", "disc_image", details)
	if err != nil {
		t.Fatalf("resolve checked-in Soulcalibur fixture: %v", err)
	}
	if first.Availability != AvailabilityArchived {
		t.Fatalf("availability = %q, want archived for exact checked-in fixture match", first.Availability)
	}
	graph := first.Graph
	if graph.Work.ID == "" || graph.Work.ID == domain.WorkID("1565") || graph.Work.Title != "Soulcalibur II" ||
		len(graph.ExternalIdentities) != 1 || len(graph.Editions) != 1 || len(graph.Assets) != 1 ||
		len(graph.Parts) != 1 || len(graph.Locations) != 1 {
		t.Fatalf("checked-in fixture did not resolve a complete canonical P1-01 graph: %#v", graph)
	}
	externalIdentity := graph.ExternalIdentities[0]
	edition := graph.Editions[0]
	asset := graph.Assets[0]
	part := graph.Parts[0]
	location := graph.Locations[0]
	if externalIdentity.Provider != "igdb" || externalIdentity.ExternalID != "1565" || externalIdentity.WorkID != graph.Work.ID {
		t.Fatalf("external identity = %#v, want canonical igdb:1565 bound to local Work %q", externalIdentity, graph.Work.ID)
	}
	if edition.Platform != "gamecube" || edition.Format != "disc_image" || edition.WorkID != graph.Work.ID || edition.ID == domain.EditionID("gamecube:disc_image") {
		t.Fatalf("edition = %#v, want canonical GameCube disc image linked to Work", edition)
	}
	if asset.ID != domain.AssetID("soulcalibur2-gamecube") || asset.EditionID != edition.ID || asset.Kind != "disc_image" || asset.TotalSizeBytes != 1450000000 {
		t.Fatalf("asset = %#v, want checked-in Soulcalibur II GameCube asset", asset)
	}
	if part.AssetID != asset.ID || part.PartIndex == nil || *part.PartIndex != 1 || part.Role != "rom" || part.Filename != "Soulcalibur II.iso" || part.SizeBytes != 1450000000 {
		t.Fatalf("part = %#v, want checked-in Soulcalibur II ISO part", part)
	}
	if location.AssetID != asset.ID || location.LocationClass != "archive" || location.StorageProviderID != "rclone" ||
		location.Locator != "testremote:games/gamecube/Soulcalibur II.iso" {
		t.Fatalf("location = %#v, want existing neutral archive locator", location)
	}

	second, err := binder.Resolve(ctx, identity, "gamecube", "disc_image", details)
	if err != nil {
		t.Fatalf("repeat checked-in fixture resolution: %v", err)
	}
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("repeated resolution = %#v, want identical result %#v", second, first)
	}
	assertCatalogCounts(t, db, map[string]int{
		"works": 1, "external_identities": 1, "editions": 1, "assets": 1, "asset_parts": 1, "asset_locations": 1,
	})
}

func TestInventoryBinderFailsClosedForCheckedInSoulcaliburFixtureMismatches(t *testing.T) {
	tests := []struct {
		name     string
		external string
		platform string
		format   string
	}{
		{name: "same title with wrong IGDB identity", external: "1566", platform: "gamecube", format: "disc_image"},
		{name: "canonical identity with wrong platform", external: "1565", platform: "playstation2", format: "disc_image"},
		{name: "canonical identity with wrong format", external: "1565", platform: "gamecube", format: "dvd_image"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := database.Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			source := inventory.NewManifestInventorySource(filepath.Join("..", "..", "examples", "manifest.json"))
			identity := providers.ExternalWorkIdentity{Provider: "igdb", ExternalID: test.external}
			details := &binderWorkDetailsStub{details: domain.WorkDetails{
				Provider: "igdb", ExternalID: test.external, Title: "Soulcalibur II",
				Medium: domain.MediumGame, WorkType: "game",
			}}
			result, err := NewInventoryBinder(NewSQLiteRepository(db), source, source).Resolve(ctx, identity, test.platform, test.format, details)
			if err != nil {
				t.Fatalf("resolve mismatched fixture: %v", err)
			}
			if result.Availability != AvailabilityUnavailable || result.Graph.Work.ID != "" {
				t.Fatalf("mismatched resolution = %#v, want unavailable with no graph", result)
			}
			assertCatalogCounts(t, db, map[string]int{
				"works": 0, "external_identities": 0, "editions": 0, "assets": 0, "asset_parts": 0, "asset_locations": 0,
			})
		})
	}
}

func TestInventoryBinderFailsClosedWhenTrustedDetailsCannotBindExactIdentity(t *testing.T) {
	identity := syntheticWorkIdentity()
	tests := []struct {
		name    string
		details domain.WorkDetails
		err     error
	}{
		{
			name: "details provider error",
			err:  errors.New("synthetic WorkDetails failure"),
		},
		{
			name: "different provider",
			details: domain.WorkDetails{
				Provider: "other-catalog", ExternalID: identity.ExternalID, Title: "Forged Match",
				Medium: domain.MediumGame, WorkType: "game",
			},
		},
		{
			name: "different external ID",
			details: domain.WorkDetails{
				Provider: identity.Provider, ExternalID: "different-record", Title: "Forged Match",
				Medium: domain.MediumGame, WorkType: "game",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := database.Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			source := syntheticBinderSource(t, identity.ExternalID, []inventory.ManifestLocation{
				{Class: "archive", Provider: "test-storage", Locator: "test://archive/synthetic.bin"},
			})
			details := &binderWorkDetailsStub{details: test.details, err: test.err}
			_, err = NewInventoryBinder(NewSQLiteRepository(db), source, source).Resolve(ctx, identity, "platform-x", "disc-image", details)
			if err == nil {
				t.Fatal("invalid trusted Work details unexpectedly bound a catalog graph")
			}
			if details.calls != 1 || details.provider != identity.Provider || details.externalID != identity.ExternalID {
				t.Fatalf("details lookup = calls:%d identity:%s:%s, want exactly %s:%s", details.calls, details.provider, details.externalID, identity.Provider, identity.ExternalID)
			}
			assertCatalogCounts(t, db, map[string]int{
				"works": 0, "external_identities": 0, "editions": 0, "assets": 0, "asset_parts": 0, "asset_locations": 0,
			})
		})
	}
}

func TestInventoryBinderReusesPreexistingCanonicalGraphWithoutDuplicatingChildren(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository := NewSQLiteRepository(db)
	partIndex := 1
	existing := WorkGraph{
		Work:     domain.Work{ID: "work-existing-record", Title: "Synthetic Work", Medium: domain.MediumGame, WorkType: "game"},
		Editions: []domain.Edition{{ID: "edition-existing-record", WorkID: "work-existing-record", Platform: "platform-x", Format: "disc-image"}},
		Assets:   []domain.Asset{{ID: "asset-record-42", EditionID: "edition-existing-record", Kind: "disc-image", TotalSizeBytes: 7}},
		Parts:    []domain.AssetPart{{ID: "part-existing-record", AssetID: "asset-record-42", PartIndex: &partIndex, Role: "rom", Filename: "synthetic.bin", SizeBytes: 7}},
		Locations: []domain.AssetLocation{{
			ID: "location-existing-record", AssetID: "asset-record-42", StorageProviderID: "test-storage",
			Locator: "test://archive/synthetic.bin", LocationClass: "archive",
		}},
		ExternalIdentities: []domain.ExternalIdentity{{
			ID: "identity-existing-record", WorkID: "work-existing-record", Provider: "test-catalog", ExternalID: "work-record-42",
		}},
	}
	if err := repository.CreateGraph(ctx, existing); err != nil {
		t.Fatal(err)
	}
	source := syntheticBinderSource(t, "work-record-42", []inventory.ManifestLocation{
		{Class: "archive", Provider: "test-storage", Locator: "test://archive/synthetic.bin"},
	})
	identity := syntheticWorkIdentity()
	details := &binderWorkDetailsStub{details: domain.WorkDetails{
		Provider: identity.Provider, ExternalID: identity.ExternalID, Title: "Unexpected Details",
		Medium: domain.MediumAudio, WorkType: "audio-recording",
	}}
	result, err := NewInventoryBinder(repository, source, source).Resolve(ctx, identity, "platform-x", "disc-image", details)
	if err != nil {
		t.Fatalf("resolve against an existing canonical graph: %v", err)
	}
	if result.Availability != AvailabilityArchived || !reflect.DeepEqual(result.Graph, existing) {
		t.Fatalf("reused resolution = %#v, want archived existing graph %#v", result, existing)
	}
	if details.calls != 0 {
		t.Fatalf("existing canonical Work called WorkDetails %d times, want no mutation-dependent lookup", details.calls)
	}
	assertCatalogCounts(t, db, map[string]int{"works": 1, "external_identities": 1, "editions": 1, "assets": 1, "asset_parts": 1, "asset_locations": 1})
}

func TestInventoryBinderReturnsUnavailableWithoutAnExactMatch(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	source := syntheticBinderSource(t, "different-record-42", []inventory.ManifestLocation{
		{Class: "archive", Provider: "test-storage", Locator: "test://archive/synthetic.bin"},
	})
	identity := syntheticWorkIdentity()
	details := syntheticWorkDetails(identity)
	result, err := NewInventoryBinder(NewSQLiteRepository(db), source, source).Resolve(ctx, identity, "platform-x", "disc-image", details)
	if err != nil {
		t.Fatalf("resolve non-matching inventory: %v", err)
	}
	if result.Availability != AvailabilityUnavailable {
		t.Fatalf("availability = %q, want unavailable", result.Availability)
	}
	if result.Graph.Work.ID != "" || len(result.Graph.Editions)+len(result.Graph.Assets)+len(result.Graph.ExternalIdentities) != 0 {
		t.Fatalf("non-match created catalog graph: %#v", result.Graph)
	}
	assertCatalogCounts(t, db, map[string]int{"works": 0, "external_identities": 0, "editions": 0, "assets": 0, "asset_parts": 0, "asset_locations": 0})
}

func TestInventoryBinderDoesNotLeavePartialGraphWhenAssetBelongsToAnotherEdition(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository := NewSQLiteRepository(db)
	conflict := WorkGraph{
		Work:     domain.Work{ID: "work-other-record", Title: "Other Synthetic Work", Medium: domain.MediumGame, WorkType: "game"},
		Editions: []domain.Edition{{ID: "edition-other-platform", WorkID: "work-other-record", Platform: "platform-y", Format: "disc-image"}},
		Assets:   []domain.Asset{{ID: "asset-record-42", EditionID: "edition-other-platform", Kind: "disc-image", TotalSizeBytes: 7}},
		Parts:    []domain.AssetPart{{ID: "part-other-record", AssetID: "asset-record-42", Role: "rom", Filename: "other.bin", SizeBytes: 7}},
		Locations: []domain.AssetLocation{{
			ID: "location-other-record", AssetID: "asset-record-42", StorageProviderID: "test-storage",
			Locator: "test://archive/other.bin", LocationClass: "archive",
		}},
		ExternalIdentities: []domain.ExternalIdentity{{
			ID: "identity-other-record", WorkID: "work-other-record", Provider: "test-catalog", ExternalID: "other-record-42",
		}},
	}
	if err := repository.CreateGraph(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	source := syntheticBinderSource(t, "work-record-42", []inventory.ManifestLocation{
		{Class: "archive", Provider: "test-storage", Locator: "test://archive/synthetic.bin"},
	})
	identity := syntheticWorkIdentity()
	details := syntheticWorkDetails(identity)
	_, err = NewInventoryBinder(repository, source, source).Resolve(ctx, identity, "platform-x", "disc-image", details)
	if err == nil {
		t.Fatal("inventory Asset ID already owned by another Edition unexpectedly resolved")
	}
	assertCatalogCounts(t, db, map[string]int{"works": 1, "external_identities": 1, "editions": 1, "assets": 1, "asset_parts": 1, "asset_locations": 1})
}

func TestAvailabilityRequiresVerifiedLocalCacheAndPreservesArchive(t *testing.T) {
	tests := []struct {
		name      string
		locations []domain.AssetLocation
		want      Availability
	}{
		{
			name:      "archive only is archived",
			locations: []domain.AssetLocation{{LocationClass: "archive", Locator: "test://archive/item.bin"}},
			want:      AvailabilityArchived,
		},
		{
			name:      "unverified local cache is not ready",
			locations: []domain.AssetLocation{{LocationClass: "local_cache", Locator: "/cache/item.bin"}},
			want:      AvailabilityUnavailable,
		},
		{
			name:      "staging cache is not ready",
			locations: []domain.AssetLocation{{LocationClass: "local_cache", Locator: "/cache/.staging/job/item.bin"}},
			want:      AvailabilityUnavailable,
		},
		{
			name: "local cache declaration does not discard archive",
			locations: []domain.AssetLocation{
				{LocationClass: "local_cache", Locator: "/cache/item.bin"},
				{LocationClass: "archive", Locator: "test://archive/item.bin"},
			},
			want: AvailabilityArchived,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := deriveAvailability(test.locations); got != test.want {
				t.Fatalf("deriveAvailability() = %q, want %q", got, test.want)
			}
		})
	}
}

func syntheticWorkIdentity() providers.ExternalWorkIdentity {
	return providers.ExternalWorkIdentity{Provider: "test-catalog", ExternalID: "work-record-42"}
}

func syntheticWorkDetails(identity providers.ExternalWorkIdentity) *binderWorkDetailsStub {
	return &binderWorkDetailsStub{details: domain.WorkDetails{
		Provider: identity.Provider, ExternalID: identity.ExternalID, Title: "Trusted Synthetic Work",
		Medium: domain.MediumGame, WorkType: "canonical-game",
	}}
}

type binderWorkDetailsStub struct {
	calls      int
	provider   string
	externalID string
	details    domain.WorkDetails
	err        error
}

func (source *binderWorkDetailsStub) GetWorkDetails(_ context.Context, provider, externalID string) (domain.WorkDetails, error) {
	source.calls++
	source.provider = provider
	source.externalID = externalID
	return source.details, source.err
}

func intPointer(value int) *int { return &value }

func syntheticBinderSource(t *testing.T, externalID string, locations []inventory.ManifestLocation) *inventory.ManifestInventorySource {
	t.Helper()
	partIndex := 1
	manifest := inventory.Manifest{
		SchemaVersion: 1,
		Assets: []inventory.ManifestAsset{{
			ID: "asset-record-42",
			Work: inventory.ManifestWork{
				Title:       "Synthetic Inventory Work",
				ExternalIDs: map[string]string{"test-catalog": externalID},
			},
			Edition:        inventory.ManifestEdition{Platform: "platform-x", Format: "disc-image"},
			TotalSizeBytes: 7,
			Parts:          []inventory.ManifestAssetPart{{PartIndex: &partIndex, Role: "rom", Filename: "synthetic.bin", SizeBytes: 7}},
			Locations:      locations,
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return inventory.NewManifestInventorySource(path)
}

func assertResolvedSyntheticGraph(t *testing.T, result BindingResult) {
	t.Helper()
	graph := result.Graph
	if result.Availability == "" || graph.Work.ID == "" || len(graph.Editions) != 1 || len(graph.Assets) != 1 ||
		len(graph.Parts) != 1 || len(graph.Locations) != 2 || len(graph.ExternalIdentities) != 1 {
		t.Fatalf("resolution did not include complete canonical graph: %#v", result)
	}
	if graph.Editions[0].ID == domain.EditionID("platform-x:disc-image") {
		t.Fatalf("Edition ID reused Manifest-local platform:format key %q", graph.Editions[0].ID)
	}
	if graph.Editions[0].ID == "" || graph.Assets[0].ID == "" || graph.Parts[0].ID == "" ||
		graph.Locations[0].ID == "" || graph.ExternalIdentities[0].ID == "" {
		t.Fatalf("catalog entity IDs must be stable and non-empty: %#v", graph)
	}
	if graph.Editions[0].WorkID != graph.Work.ID || graph.Assets[0].EditionID != graph.Editions[0].ID ||
		graph.Parts[0].AssetID != graph.Assets[0].ID || graph.Locations[0].AssetID != graph.Assets[0].ID ||
		graph.ExternalIdentities[0].WorkID != graph.Work.ID {
		t.Fatalf("canonical graph relationships are inconsistent: %#v", graph)
	}
	if graph.ExternalIdentities[0].Provider != "test-catalog" || graph.ExternalIdentities[0].ExternalID != "work-record-42" {
		t.Fatalf("ExternalIdentity = %#v, want exact synthetic identity", graph.ExternalIdentities[0])
	}
	locationClasses := make(map[string]struct{}, len(graph.Locations))
	for _, location := range graph.Locations {
		locationClasses[location.LocationClass] = struct{}{}
	}
	if _, hasArchive := locationClasses["archive"]; !hasArchive {
		t.Fatalf("archive location was not retained: %#v", graph.Locations)
	}
	if _, hasLocalCache := locationClasses["local_cache"]; !hasLocalCache {
		t.Fatalf("local-cache location was not retained: %#v", graph.Locations)
	}
}

func assertCatalogCounts(t *testing.T, db *sql.DB, want map[string]int) {
	t.Helper()
	for table, expected := range want {
		var got int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if got != expected {
			t.Errorf("%s rows = %d, want %d", table, got, expected)
		}
	}
}
