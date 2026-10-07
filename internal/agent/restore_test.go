package agent

import (
	"errors"
	"strings"
	"testing"
	"time"

	"lernae/internal/domain"
)

func validRestoreRequest() RestoreAsset {
	assetID := domain.AssetID("asset-1")
	return RestoreAsset{
		JobID: "job-1",
		Asset: domain.Asset{ID: assetID, TotalSizeBytes: 4},
		Parts: []domain.AssetPart{{ID: "part-1", AssetID: assetID, Role: "rom", Filename: "game.iso", SizeBytes: 4}},
		SourceLocation: domain.AssetLocation{
			AssetID: assetID, StorageProviderID: RestoreSourceProviderFixture,
			Locator: "fixtures/game.iso", LocationClass: RestoreSourceClassFixture,
		},
	}
}

func TestRestoreAssetValidatesSingleROMPartAndKnownSize(t *testing.T) {
	request := validRestoreRequest()
	if err := request.Validate(); err != nil {
		t.Fatalf("valid restore request rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*RestoreAsset)
		want   error
	}{
		{name: "no part", mutate: func(request *RestoreAsset) { request.Parts = nil }, want: ErrUnsupportedAssetShape},
		{name: "multiple parts", mutate: func(request *RestoreAsset) {
			request.Parts = append(request.Parts, request.Parts[0])
		}, want: ErrUnsupportedAssetShape},
		{name: "non ROM role", mutate: func(request *RestoreAsset) { request.Parts[0].Role = "data" }, want: ErrUnsupportedAssetShape},
		{name: "unknown asset size", mutate: func(request *RestoreAsset) { request.Asset.TotalSizeBytes = 0 }, want: ErrExpectedSizeRequired},
		{name: "part size mismatch", mutate: func(request *RestoreAsset) { request.Parts[0].SizeBytes = 3 }, want: ErrExpectedSizeRequired},
		{name: "part asset mismatch", mutate: func(request *RestoreAsset) { request.Parts[0].AssetID = "other" }, want: ErrUnsupportedAssetShape},
		{name: "nested logical part path", mutate: func(request *RestoreAsset) { request.Parts[0].RelativePath = "nested/game.iso" }, want: ErrUnsupportedAssetShape},
		{name: "invalid UTF-8 filename", mutate: func(request *RestoreAsset) { request.Parts[0].Filename = string([]byte{0xff, '.', 'i', 's', 'o'}) }, want: ErrUnsafeRestorePath},
		{name: "oversized filename", mutate: func(request *RestoreAsset) { request.Parts[0].Filename = strings.Repeat("a", 256) + ".iso" }, want: ErrUnsafeRestorePath},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := validRestoreRequest()
			test.mutate(&candidate)
			if err := candidate.Validate(); !errors.Is(err, test.want) {
				t.Fatalf("Validate() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestRestoreAssetRejectsUnsafeFixtureDescriptors(t *testing.T) {
	for _, descriptor := range []string{"", "../outside.iso", "/tmp/game.iso", `nested\game.iso`, "fixtures/../outside.iso", "C:game.iso", strings.Repeat("a", 4097)} {
		t.Run(descriptor, func(t *testing.T) {
			request := validRestoreRequest()
			request.SourceLocation.Locator = descriptor
			if err := request.Validate(); err == nil {
				t.Fatalf("unsafe source descriptor %q unexpectedly passed", descriptor)
			}
		})
	}
}

func TestRestoreAssetValidatesStructuredSourceLocations(t *testing.T) {
	request := validRestoreRequest()
	tests := []struct {
		name   string
		source domain.AssetLocation
		valid  bool
	}{
		{
			name: "rclone archive",
			source: domain.AssetLocation{
				AssetID: request.Asset.ID, StorageProviderID: "rclone", Locator: "archive:roms/game.iso", LocationClass: "archive",
			},
			valid: true,
		},
		{
			name: "injected fixture source",
			source: domain.AssetLocation{
				AssetID: request.Asset.ID, StorageProviderID: "fixture", Locator: "fixtures/game.iso", LocationClass: "fixture",
			},
			valid: true,
		},
		{
			name: "provider and class mismatch",
			source: domain.AssetLocation{
				AssetID: request.Asset.ID, StorageProviderID: "rclone", Locator: "archive:roms/game.iso", LocationClass: "local_cache",
			},
		},
		{
			name: "location belongs to another asset",
			source: domain.AssetLocation{
				AssetID: "other-asset", StorageProviderID: "rclone", Locator: "archive:roms/game.iso", LocationClass: "archive",
			},
		},
		{
			name: "unsupported provider",
			source: domain.AssetLocation{
				AssetID: request.Asset.ID, StorageProviderID: "arbitrary", Locator: "archive:roms/game.iso", LocationClass: "archive",
			},
		},
		{
			name: "unsafe fixture locator",
			source: domain.AssetLocation{
				AssetID: request.Asset.ID, StorageProviderID: "fixture", Locator: "../outside.iso", LocationClass: "fixture",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := request
			candidate.SourceLocation = test.source
			err := candidate.Validate()
			if test.valid && err != nil {
				t.Fatalf("valid source location rejected: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("invalid source location unexpectedly passed")
			}
		})
	}
}

func TestRestoreRequestIsTheOnlyRestoreWireCommand(t *testing.T) {
	request := Request{Operation: OperationRestoreAsset, RestoreAsset: pointer(validRestoreRequest())}
	if err := request.Validate(); err != nil {
		t.Fatalf("typed restore request rejected: %v", err)
	}
	request.RestoreAsset = nil
	if err := request.Validate(); err == nil {
		t.Fatal("restore operation without its typed payload unexpectedly passed")
	}
	if err := (Request{Operation: Operation("run_command")}).Validate(); !errors.Is(err, ErrUnsupportedOperation) {
		t.Fatalf("generic command validation error = %v, want ErrUnsupportedOperation", err)
	}
}

func TestRestoreProgressRejectsInvalidBoundsAndPhases(t *testing.T) {
	tests := []RestoreProgress{
		{Phase: RestorePhaseCopying, CurrentBytes: -1, TotalBytes: 4},
		{Phase: RestorePhaseCopying, CurrentBytes: 5, TotalBytes: 4},
		{Phase: RestorePhaseCopying, CurrentBytes: 0, TotalBytes: 0},
		{Phase: RestorePhaseComplete, CurrentBytes: 3, TotalBytes: 4},
		{Phase: RestorePhase("shell"), CurrentBytes: 0, TotalBytes: 4},
	}
	for _, progress := range tests {
		if err := progress.Validate(); err == nil {
			t.Errorf("invalid progress %#v unexpectedly passed", progress)
		}
	}
	if err := (RestoreProgress{Phase: RestorePhaseCopying, CurrentBytes: 4, TotalBytes: 4}).Validate(); err != nil {
		t.Fatalf("valid progress rejected: %v", err)
	}
}

func TestRestoreResultRequiresVerifiedDerivedLocalCacheEvidence(t *testing.T) {
	request := validRestoreRequest()
	result := RestoreResult{
		AssetID:           request.Asset.ID,
		LocationClass:     "local_cache",
		RelativePath:      "assets/asset-1/game.iso",
		VerifiedSizeBytes: 4,
		VerifiedAt:        time.Now().UTC(),
		LocalReady:        true,
	}
	if err := result.ValidateFor(request); err != nil {
		t.Fatalf("valid promoted result rejected: %v", err)
	}
	for _, mutate := range []func(*RestoreResult){
		func(result *RestoreResult) { result.LocalReady = false },
		func(result *RestoreResult) { result.LocationClass = "archive" },
		func(result *RestoreResult) { result.RelativePath = ".staging/job-1/game.iso" },
		func(result *RestoreResult) { result.RelativePath = "assets/other/game.iso" },
		func(result *RestoreResult) { result.VerifiedSizeBytes = 3 },
		func(result *RestoreResult) { result.VerifiedAt = time.Time{} },
	} {
		candidate := result
		mutate(&candidate)
		if err := candidate.ValidateFor(request); err == nil {
			t.Errorf("invalid restore result %#v unexpectedly passed", candidate)
		}
	}
}

func pointer[T any](value T) *T { return &value }
