package catalog

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lernae/internal/agent"
	"lernae/internal/database"
	"lernae/internal/domain"
)

func TestEnsureLocalCacheLocationFromRestoreIsRelativeAndIdempotentAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	db, err := database.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	repository := NewSQLiteRepository(db)
	graph := soulcaliburFixture()
	if err := repository.CreateGraph(ctx, graph); err != nil {
		t.Fatal(err)
	}
	restore, result := validatedLocalCacheRestore(graph.Assets[0], graph.Parts[0], graph.Locations[0])

	first, err := repository.EnsureLocalCacheLocation(ctx, restore, result)
	if err != nil {
		t.Fatalf("persist validated local-cache metadata: %v", err)
	}
	second, err := repository.EnsureLocalCacheLocation(ctx, restore, result)
	if err != nil {
		t.Fatalf("repeat local-cache projection: %v", err)
	}
	if first != second {
		t.Fatalf("duplicate projection = %#v, want the same durable location %#v", second, first)
	}
	if first.AssetID != graph.Assets[0].ID || first.StorageProviderID != "filesystem" || first.LocationClass != "local_cache" ||
		first.Locator != "assets/soulcalibur2-gamecube/Soulcalibur II.iso" || filepath.IsAbs(first.Locator) {
		t.Fatalf("projected local-cache location = %#v; want trusted relative locator", first)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = database.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen catalog database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository = NewSQLiteRepository(db)
	loaded, err := repository.GetWork(ctx, graph.Work.ID)
	if err != nil {
		t.Fatalf("load graph after reopen: %v", err)
	}
	if len(loaded.Locations) != 2 {
		t.Fatalf("reopened locations = %#v; want archive plus one local-cache row", loaded.Locations)
	}
	if !containsLocation(loaded.Locations, graph.Locations[0]) || !containsLocation(loaded.Locations, first) {
		t.Fatalf("reopened graph did not preserve archive and projected local-cache locations: %#v", loaded.Locations)
	}
}

func TestEnsureLocalCacheLocationRejectsUntrustedOrUnsafeRestoreEvidence(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository := NewSQLiteRepository(db)
	graph := soulcaliburFixture()
	if err := repository.CreateGraph(ctx, graph); err != nil {
		t.Fatal(err)
	}
	restore, result := validatedLocalCacheRestore(graph.Assets[0], graph.Parts[0], graph.Locations[0])
	tests := []struct {
		name   string
		mutate func(*agent.RestoreAsset, *agent.RestoreResult)
	}{
		{name: "absolute path", mutate: func(_ *agent.RestoreAsset, result *agent.RestoreResult) { result.RelativePath = "/tmp/game.iso" }},
		{name: "traversal", mutate: func(_ *agent.RestoreAsset, result *agent.RestoreResult) {
			result.RelativePath = "assets/../outside.iso"
		}},
		{name: "staging path", mutate: func(_ *agent.RestoreAsset, result *agent.RestoreResult) {
			result.RelativePath = ".staging/job-1/game.iso"
		}},
		{name: "wrong Asset identity", mutate: func(_ *agent.RestoreAsset, result *agent.RestoreResult) { result.AssetID = "other-asset" }},
		{name: "not ready", mutate: func(_ *agent.RestoreAsset, result *agent.RestoreResult) { result.LocalReady = false }},
		{name: "Asset absent from catalog shape", mutate: func(restore *agent.RestoreAsset, _ *agent.RestoreResult) { restore.Asset.Kind = "other-format" }},
		{name: "part absent from catalog", mutate: func(restore *agent.RestoreAsset, _ *agent.RestoreResult) { restore.Parts[0].ID = "other-part" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidateRestore, candidateResult := restore, result
			test.mutate(&candidateRestore, &candidateResult)
			if _, err := repository.EnsureLocalCacheLocation(ctx, candidateRestore, candidateResult); err == nil {
				t.Fatal("untrusted or unsafe restore evidence unexpectedly created local-cache metadata")
			}
		})
	}
	loaded, err := repository.GetWork(ctx, graph.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Locations) != 1 || loaded.Locations[0] != graph.Locations[0] {
		t.Fatalf("invalid evidence changed persisted locations: %#v", loaded.Locations)
	}
}

func TestRemoveLocalCacheLocationDeletesOnlyObservedRowAndNeverRemovesFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := database.Open(ctx, filepath.Join(root, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository := NewSQLiteRepository(db)
	graph := soulcaliburFixture()
	sibling := domain.Asset{ID: "other-gamecube", EditionID: graph.Editions[0].ID, Kind: "disc_image", TotalSizeBytes: 4}
	graph.Assets = append(graph.Assets, sibling)
	graph.Parts = append(graph.Parts, domain.AssetPart{ID: "other-gamecube-rom", AssetID: sibling.ID, PartIndex: localCacheTestIntPointer(1), Role: "rom", Filename: "other.iso", SizeBytes: 4})
	archive := domain.AssetLocation{ID: "other-gamecube-archive", AssetID: sibling.ID, StorageProviderID: "rclone", Locator: "remote:other.iso", LocationClass: "archive"}
	graph.Locations = append(graph.Locations, archive)
	if err := repository.CreateGraph(ctx, graph); err != nil {
		t.Fatal(err)
	}
	selectedRestore, selectedResult := validatedLocalCacheRestore(graph.Assets[0], graph.Parts[0], graph.Locations[0])
	otherRestore, otherResult := validatedLocalCacheRestore(sibling, graph.Parts[1], archive)
	selectedLocation, err := repository.EnsureLocalCacheLocation(ctx, selectedRestore, selectedResult)
	if err != nil {
		t.Fatal(err)
	}
	otherLocation, err := repository.EnsureLocalCacheLocation(ctx, otherRestore, otherResult)
	if err != nil {
		t.Fatal(err)
	}
	stale := domain.AssetLocation{ID: "legacy-selected-cache", AssetID: graph.Assets[0].ID, StorageProviderID: "legacy-filesystem", Locator: "assets/soulcalibur2-gamecube/old.iso", LocationClass: "local_cache"}
	siblingCandidate := domain.AssetLocation{ID: "selected-valid-sibling-cache", AssetID: graph.Assets[0].ID, StorageProviderID: "filesystem", Locator: "assets/soulcalibur2-gamecube/alternate.iso", LocationClass: "local_cache"}
	for _, candidate := range []domain.AssetLocation{stale, siblingCandidate} {
		if _, err := db.ExecContext(ctx, `INSERT INTO asset_locations (id, asset_id, storage_provider_id, locator, location_class) VALUES (?, ?, ?, ?, ?)`,
			candidate.ID, candidate.AssetID, candidate.StorageProviderID, candidate.Locator, candidate.LocationClass); err != nil {
			t.Fatalf("insert legacy local-cache metadata: %v", err)
		}
	}
	cacheFile := filepath.Join(root, filepath.FromSlash(selectedLocation.Locator))
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheFile, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := repository.RemoveLocalCacheLocation(ctx, selectedLocation)
	if err != nil {
		t.Fatalf("remove selected Asset local-cache metadata: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed %d local-cache rows, want only the invalid candidate", removed)
	}
	loaded, err := repository.GetWork(ctx, graph.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if containsLocation(loaded.Locations, selectedLocation) {
		t.Fatalf("invalid candidate metadata remains: %#v", loaded.Locations)
	}
	if !containsLocation(loaded.Locations, stale) || !containsLocation(loaded.Locations, siblingCandidate) ||
		!containsLocation(loaded.Locations, graph.Locations[0]) || !containsLocation(loaded.Locations, archive) || !containsLocation(loaded.Locations, otherLocation) {
		t.Fatalf("cleanup removed a sibling candidate or archive metadata: %#v", loaded.Locations)
	}
	if contents, err := os.ReadFile(cacheFile); err != nil || string(contents) != "data" {
		t.Fatalf("metadata cleanup touched the local file: contents=%q err=%v", contents, err)
	}
}

func validatedLocalCacheRestore(asset domain.Asset, part domain.AssetPart, archive domain.AssetLocation) (agent.RestoreAsset, agent.RestoreResult) {
	request := agent.RestoreAsset{
		JobID: "restore-local-cache-test", Asset: asset, Parts: []domain.AssetPart{part}, SourceLocation: archive,
	}
	result := agent.RestoreResult{
		AssetID: asset.ID, LocationClass: "local_cache", RelativePath: "assets/" + string(asset.ID) + "/" + part.Filename,
		VerifiedSizeBytes: asset.TotalSizeBytes, VerifiedAt: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC), LocalReady: true,
	}
	return request, result
}

func containsLocation(locations []domain.AssetLocation, want domain.AssetLocation) bool {
	for _, location := range locations {
		if location == want {
			return true
		}
	}
	return false
}

func localCacheTestIntPointer(value int) *int { return &value }
