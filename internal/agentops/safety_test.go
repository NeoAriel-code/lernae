package agentops

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"lernae/internal/domain"
)

func TestFinalPathIsDerivedFromValidatedAssetIDAndFilename(t *testing.T) {
	cacheDir := t.TempDir()
	cache, err := OpenCache(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })

	got, err := cache.FinalPath(domain.AssetID("asset-123"), "game.iso")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cacheDir, "assets", "asset-123", "game.iso")
	if got != want {
		t.Fatalf("FinalPath() = %q, want %q", got, want)
	}
}

func TestRestorePathValidationRejectsTraversalAndSeparators(t *testing.T) {
	tests := []struct {
		name     string
		assetID  string
		jobID    string
		filename string
	}{
		{name: "asset traversal", assetID: "../outside", jobID: "job-1", filename: "game.iso"},
		{name: "absolute asset ID", assetID: "/tmp/asset", jobID: "job-1", filename: "game.iso"},
		{name: "job traversal", assetID: "asset-1", jobID: "../outside", filename: "game.iso"},
		{name: "absolute job ID", assetID: "asset-1", jobID: "/tmp/job", filename: "game.iso"},
		{name: "filename traversal", assetID: "asset-1", jobID: "job-1", filename: "../game.iso"},
		{name: "filename separator", assetID: "asset-1", jobID: "job-1", filename: "nested/game.iso"},
		{name: "windows separator", assetID: "asset-1", jobID: "job-1", filename: `nested\game.iso`},
		{name: "absolute filename", assetID: "asset-1", jobID: "job-1", filename: "/tmp/game.iso"},
		{name: "dot filename", assetID: "asset-1", jobID: "job-1", filename: "."},
		{name: "empty filename", assetID: "asset-1", jobID: "job-1", filename: ""},
		{name: "drive injection", assetID: "asset-1", jobID: "job-1", filename: "C:game.iso"},
		{name: "nul filename", assetID: "asset-1", jobID: "job-1", filename: "game\x00.iso"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateRestorePaths(domain.AssetID(test.assetID), test.jobID, test.filename); err == nil {
				t.Fatalf("ValidateRestorePaths(%q, %q, %q) unexpectedly succeeded", test.assetID, test.jobID, test.filename)
			}
		})
	}
}

func TestOpenCacheRejectsSymlinkedStagingRoot(t *testing.T) {
	cacheDir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(cacheDir, ".staging")); err != nil {
		t.Fatal(err)
	}

	if _, err := OpenCache(cacheDir); err == nil || !strings.Contains(strings.ToLower(err.Error()), "staging") {
		t.Fatalf("OpenCache with symlinked staging root error = %v, want staging rejection", err)
	}
}

func TestOpenCacheWithStagingPathUsesNestedPrivateDirectory(t *testing.T) {
	cachePath := t.TempDir()
	stagingPath := filepath.Join(cachePath, "private", "restore-staging")
	cache, err := OpenCacheWithStagingPath(cachePath, stagingPath)
	if err != nil {
		t.Fatalf("OpenCacheWithStagingPath: %v", err)
	}
	defer cache.Close()

	staged, err := cache.CreateStaging("job-custom-staging", "game.iso")
	if err != nil {
		t.Fatalf("CreateStaging: %v", err)
	}
	if _, err := staged.File.Write([]byte("safe")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(stagingPath, "job-custom-staging", "game.iso")); err != nil {
		t.Fatalf("staged file missing from configured staging path: %v", err)
	}
	if err := staged.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(cachePath, ".staging")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default staging directory was created despite a configured path: %v", err)
	}
}

func TestOpenCacheWithStagingPathRejectsEscapeAndSymlinkComponents(t *testing.T) {
	cachePath := t.TempDir()
	outside := t.TempDir()
	if _, err := OpenCacheWithStagingPath(cachePath, filepath.Join(outside, "stage")); err == nil {
		t.Fatal("OpenCacheWithStagingPath accepted a staging directory outside the cache")
	}
	link := filepath.Join(cachePath, "stage-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenCacheWithStagingPath(cachePath, filepath.Join(link, "stage")); err == nil {
		t.Fatal("OpenCacheWithStagingPath followed a symlinked staging path component")
	}
	if _, err := os.Lstat(filepath.Join(outside, "stage")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink target was mutated during rejected staging open: %v", err)
	}
}

func TestOpenCacheWithStagingPathRejectsFinalAssetNamespaceAndPreservesFinalAsset(t *testing.T) {
	cachePath := t.TempDir()
	assetsPath := filepath.Join(cachePath, "assets")
	finalPath := filepath.Join(assetsPath, "asset-final", "game.iso")
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalPath, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, stagingPath := range []string{assetsPath, filepath.Join(assetsPath, "restore-staging")} {
		t.Run(filepath.Base(stagingPath), func(t *testing.T) {
			if cache, err := OpenCacheWithStagingPath(cachePath, stagingPath); err == nil {
				_ = cache.Close()
				t.Fatalf("OpenCacheWithStagingPath accepted final Asset namespace %q", stagingPath)
			}
			if data, err := os.ReadFile(finalPath); err != nil || string(data) != "safe" {
				t.Fatalf("existing final Asset = %q, error = %v; conflicting staging must be rejected without mutation", data, err)
			}
		})
	}
}

func TestOpenCacheRejectsSymlinkedQuarantineRoot(t *testing.T) {
	cacheDir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(cacheDir, ".quarantine")); err != nil {
		t.Fatal(err)
	}

	cache, err := OpenCache(cacheDir)
	if cache != nil {
		_ = cache.Close()
	}
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "quarantine") {
		t.Fatalf("OpenCache with symlinked quarantine root error = %v, want quarantine rejection", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlinked quarantine target was modified: entries=%v err=%v", entries, err)
	}
}

func TestOpenCacheRejectsSymlinkedConfiguredRoot(t *testing.T) {
	actualRoot := t.TempDir()
	configuredRoot := filepath.Join(t.TempDir(), "cache")
	if err := os.Symlink(actualRoot, configuredRoot); err != nil {
		t.Fatal(err)
	}

	if _, err := OpenCache(configuredRoot); err == nil {
		t.Fatal("OpenCache unexpectedly accepted a symlinked configured root")
	}
}

func TestOpenCacheRejectsGroupOrOtherWritableNonStickyAncestor(t *testing.T) {
	workspace := t.TempDir()
	shared := filepath.Join(workspace, "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(shared, "cache")

	cache, err := OpenCache(cachePath)
	if cache != nil {
		_ = cache.Close()
	}
	if err == nil {
		t.Fatal("OpenCache unexpectedly accepted a cache beneath a group/other-writable non-sticky ancestor")
	}
	if _, statErr := os.Lstat(cachePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected cache path was created beneath the unsafe ancestor: %v", statErr)
	}
}

func TestOpenCacheRejectsSymlinkedAncestor(t *testing.T) {
	workspace := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(workspace, "linked-parent")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	cache, err := OpenCache(filepath.Join(link, "cache"))
	if cache != nil {
		_ = cache.Close()
	}
	if err == nil {
		t.Fatal("OpenCache unexpectedly followed a symlinked ancestor")
	}
	if _, statErr := os.Lstat(filepath.Join(target, "cache")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected cache path was created through a symlinked ancestor: %v", statErr)
	}
}

func TestOpenCacheRejectsNonDirectoryAncestor(t *testing.T) {
	workspace := t.TempDir()
	filePath := filepath.Join(workspace, "not-a-directory")
	if err := os.WriteFile(filePath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}

	if cache, err := OpenCache(filepath.Join(filePath, "cache")); err == nil {
		_ = cache.Close()
		t.Fatal("OpenCache unexpectedly accepted a non-directory ancestor")
	}
}

func TestOpenCacheAcceptsRootOwnedStickyTmpAncestor(t *testing.T) {
	tmpInfo, err := os.Lstat("/tmp")
	if err != nil {
		t.Skipf("/tmp is unavailable: %v", err)
	}
	tmpStat, ok := tmpInfo.Sys().(*syscall.Stat_t)
	if !ok || tmpStat.Uid != 0 || tmpInfo.Mode()&os.ModeSticky == 0 {
		t.Skip("/tmp is not root-owned and sticky on this host")
	}

	cachePath := filepath.Join(t.TempDir(), "cache")
	relative, err := filepath.Rel("/tmp", cachePath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Skipf("t.TempDir() is not beneath /tmp: %q", cachePath)
	}
	cache, err := OpenCache(cachePath)
	if err != nil {
		t.Fatalf("OpenCache rejected a private cache beneath root-owned sticky /tmp: %v", err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCacheRejectsGroupOrOtherWritableTrustedDirectories(t *testing.T) {
	tests := []struct {
		name            string
		prepare         func(t *testing.T, cacheDir string)
		expectOpenError bool
		verifyAfterOpen func(t *testing.T, cache *Cache, cacheDir string)
	}{
		{
			name: "cache root",
			prepare: func(t *testing.T, cacheDir string) {
				t.Helper()
				if err := os.Chmod(cacheDir, 0o722); err != nil {
					t.Fatal(err)
				}
			},
			expectOpenError: true,
		},
		{
			name: "staging root",
			prepare: func(t *testing.T, cacheDir string) {
				t.Helper()
				staging := filepath.Join(cacheDir, ".staging")
				if err := os.Mkdir(staging, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(staging, 0o722); err != nil {
					t.Fatal(err)
				}
			},
			expectOpenError: true,
		},
		{
			name: "assets root",
			prepare: func(t *testing.T, cacheDir string) {
				t.Helper()
				assets := filepath.Join(cacheDir, "assets")
				if err := os.Mkdir(assets, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(assets, 0o722); err != nil {
					t.Fatal(err)
				}
			},
			expectOpenError: true,
		},
		{
			name: "asset final directory",
			prepare: func(t *testing.T, cacheDir string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(cacheDir, "assets", "asset-1"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Join(cacheDir, "assets", "asset-1"), 0o722); err != nil {
					t.Fatal(err)
				}
			},
			verifyAfterOpen: func(t *testing.T, cache *Cache, _ string) {
				t.Helper()
				cache.availableBytes = func(*os.File) (uint64, error) { return 1 << 20, nil }
				if _, err := cache.Preflight(domain.AssetID("asset-1"), "game.iso", 4); err == nil {
					t.Fatal("Preflight unexpectedly accepted a group/other-writable Asset parent")
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cacheDir := t.TempDir()
			test.prepare(t, cacheDir)
			if test.expectOpenError {
				cache, err := OpenCache(cacheDir)
				if cache != nil {
					_ = cache.Close()
				}
				if err == nil {
					t.Fatal("OpenCache unexpectedly accepted a group/other-writable directory")
				}
				return
			}

			cache, err := OpenCache(cacheDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cache.Close() })
			if test.verifyAfterOpen != nil {
				test.verifyAfterOpen(t, cache, cacheDir)
			}
		})
	}
}

func TestPromoteRejectsGroupOrOtherWritableOwnedStagingDirectory(t *testing.T) {
	cacheDir := t.TempDir()
	cache, err := OpenCache(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	cache.availableBytes = func(*os.File) (uint64, error) { return 1 << 20, nil }
	plan, err := cache.Preflight(domain.AssetID("asset-1"), "game.iso", 4)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plan.Close() }()
	staged, err := cache.CreateStaging("job-unsafe", "game.iso")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = staged.Cleanup() }()
	if _, err := staged.File.Write([]byte("safe")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(cacheDir, ".staging", "job-unsafe"), 0o722); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.Promote(staged, plan); err == nil {
		t.Fatal("Promote unexpectedly published through a group/other-writable staging directory")
	}
	final := filepath.Join(cacheDir, "assets", "asset-1", "game.iso")
	if _, err := os.Lstat(final); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe staging directory produced a final path: %v", err)
	}
}

func TestValidateDirectoryMetadataRejectsWrongOwnerAndWritableModes(t *testing.T) {
	currentUID := uint32(os.Geteuid())
	foreignUID := currentUID ^ 1
	tests := []struct {
		name     string
		ownerUID uint32
		mode     os.FileMode
		private  bool
		wantErr  bool
	}{
		{name: "owned cache directory", ownerUID: currentUID, mode: os.ModeDir | 0o755},
		{name: "foreign owner", ownerUID: foreignUID, mode: os.ModeDir | 0o700, wantErr: true},
		{name: "group writable", ownerUID: currentUID, mode: os.ModeDir | 0o772, wantErr: true},
		{name: "other writable", ownerUID: currentUID, mode: os.ModeDir | 0o707, wantErr: true},
		{name: "private staging directory", ownerUID: currentUID, mode: os.ModeDir | 0o700, private: true},
		{name: "group-readable staging directory", ownerUID: currentUID, mode: os.ModeDir | 0o750, private: true, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateDirectoryMetadata("synthetic directory", test.ownerUID, test.mode, test.private)
			if test.wantErr && err == nil {
				t.Fatal("validateDirectoryMetadata unexpectedly accepted unsafe owner/mode metadata")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateDirectoryMetadata() error = %v", err)
			}
		})
	}
}

func syntheticForeignUID(currentUID uint32) uint32 {
	if currentUID == 1 {
		return 2
	}
	return 1
}

func TestSyntheticForeignUIDIsNonzeroAndDistinctFromCurrentUID(t *testing.T) {
	tests := []struct {
		name      string
		currentID uint32
	}{
		{name: "root", currentID: 0},
		{name: "uid one", currentID: 1},
		{name: "ordinary uid", currentID: 1000},
		{name: "max uint32", currentID: ^uint32(0)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			foreignUID := syntheticForeignUID(test.currentID)
			if foreignUID == 0 {
				t.Fatalf("synthetic foreign UID for current UID %d is zero", test.currentID)
			}
			if foreignUID == test.currentID {
				t.Fatalf("synthetic foreign UID for current UID %d is not distinct", test.currentID)
			}
		})
	}
}

func TestValidateCacheAncestorMetadataRejectsForeignReplacementAuthority(t *testing.T) {
	currentUID := uint32(os.Geteuid())
	foreignUID := syntheticForeignUID(currentUID)
	tests := []struct {
		name     string
		ownerUID uint32
		mode     os.FileMode
		wantErr  bool
	}{
		{name: "effective user parent", ownerUID: currentUID, mode: os.ModeDir | 0o755},
		{name: "root read-only parent", ownerUID: 0, mode: os.ModeDir | 0o555},
		{name: "root-owned sticky shared parent", ownerUID: 0, mode: os.ModeDir | os.ModeSticky | 0o777},
		{name: "effective user writable without sticky", ownerUID: currentUID, mode: os.ModeDir | 0o777, wantErr: true},
		{name: "foreign owner can replace child", ownerUID: foreignUID, mode: os.ModeDir | 0o755, wantErr: true},
		{name: "foreign owner read-only", ownerUID: foreignUID, mode: os.ModeDir | 0o555, wantErr: true},
		{name: "foreign owner sticky", ownerUID: foreignUID, mode: os.ModeDir | os.ModeSticky | 0o777, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateCacheAncestorMetadata("synthetic cache ancestor", test.ownerUID, test.mode)
			if test.wantErr && err == nil {
				t.Fatal("validateCacheAncestorMetadata unexpectedly accepted unsafe owner/mode metadata")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateCacheAncestorMetadata() error = %v", err)
			}
		})
	}
}

func TestValidateDirectoryHandleReadsMetadataFromOpenedDescriptor(t *testing.T) {
	directory := t.TempDir()
	file, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	if err := validateDirectoryHandle("opened directory", file, false); err != nil {
		t.Fatalf("validateDirectoryHandle() rejected owned private descriptor: %v", err)
	}
	if err := os.Chmod(directory, 0o722); err != nil {
		t.Fatal(err)
	}
	if err := validateDirectoryHandle("opened directory", file, false); err == nil {
		t.Fatal("validateDirectoryHandle() did not observe writable metadata on the opened descriptor")
	}
}

func TestStagingCleanupIsScopedToOwnedJobDirectory(t *testing.T) {
	cacheDir := t.TempDir()
	cache, err := OpenCache(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })

	staged, err := cache.CreateStaging("job-123", "game.iso")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := staged.File.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	ownedFile := filepath.Join(cacheDir, ".staging", "job-123", "game.iso")
	otherFile := filepath.Join(cacheDir, ".staging", "other-job", "keep.txt")
	if err := os.MkdirAll(filepath.Dir(otherFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := staged.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(ownedFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned staging file remains after cleanup: %v", err)
	}
	if data, err := os.ReadFile(otherFile); err != nil || string(data) != "keep" {
		t.Fatalf("cleanup touched another Job: data=%q error=%v", data, err)
	}
}

func TestCreateStagingRejectsExistingJobSubtree(t *testing.T) {
	cache, err := OpenCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })

	first, err := cache.CreateStaging("job-123", "game.iso")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Cleanup() }()
	if _, err := cache.CreateStaging("job-123", "game.iso"); err == nil {
		t.Fatal("CreateStaging unexpectedly reused an existing Job subtree")
	}
}

func TestPreflightRequiresKnownPositiveExpectedSize(t *testing.T) {
	cache, err := OpenCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	checks := 0
	cache.availableBytes = func(*os.File) (uint64, error) {
		checks++
		return 1 << 20, nil
	}

	for _, size := range []int64{0, -1} {
		if _, err := cache.Preflight(domain.AssetID("asset-1"), "game.iso", size); !errors.Is(err, ErrExpectedSizeRequired) {
			t.Errorf("Preflight(%d) error = %v, want ErrExpectedSizeRequired", size, err)
		}
	}
	if checks != 0 {
		t.Fatalf("free-space checks = %d, want none for unknown sizes", checks)
	}
}

func TestPreflightRejectsExistingExactSizeFinalWithoutChangingIt(t *testing.T) {
	cacheDir := t.TempDir()
	cache, err := OpenCache(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	assetID := domain.AssetID("asset-1")
	final, err := cache.FinalPath(assetID, "game.iso")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		t.Fatal(err)
	}
	existing := []byte("valid")
	if err := os.WriteFile(final, existing, 0o600); err != nil {
		t.Fatal(err)
	}

	plan, err := cache.Preflight(assetID, "game.iso", int64(len(existing)))
	if plan != nil {
		_ = plan.Close()
	}
	if !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("Preflight() error = %v, want occupied exact-size destination conflict", err)
	}
	if got, err := os.ReadFile(final); err != nil || !bytes.Equal(got, existing) {
		t.Fatalf("exact-size existing destination changed: bytes=%q err=%v", got, err)
	}
}

func TestPreflightUsesExactExpectedBytesForFreeSpacePolicy(t *testing.T) {
	tests := []struct {
		name      string
		available uint64
		wantErr   bool
	}{
		{name: "exact expected bytes available", available: 100},
		{name: "one byte short", available: 99, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cache, err := OpenCache(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cache.Close() })
			var gotPath string
			cache.availableBytes = func(directory *os.File) (uint64, error) {
				if directory == nil {
					t.Fatal("free-space preflight received no anchored directory handle")
				}
				gotPath = directory.Name()
				return test.available, nil
			}

			plan, err := cache.Preflight(domain.AssetID("asset-1"), "game.iso", 100)
			if test.wantErr && !errors.Is(err, ErrInsufficientSpace) {
				t.Fatalf("Preflight error = %v, want ErrInsufficientSpace", err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("Preflight() error = %v", err)
			}
			if plan != nil {
				defer func() { _ = plan.Close() }()
			}
			if gotPath == "" {
				t.Fatal("Preflight did not check available bytes")
			}
		})
	}
}

func TestPromoteRequiresExactRegularFileSize(t *testing.T) {
	cache, err := OpenCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	cache.availableBytes = func(*os.File) (uint64, error) { return 1 << 20, nil }
	plan, err := cache.Preflight(domain.AssetID("asset-1"), "game.iso", 6)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plan.Close() }()
	staged, err := cache.CreateStaging("job-1", "game.iso")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = staged.Cleanup() }()
	if _, err := staged.File.Write([]byte("short")); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.Promote(staged, plan); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("Promote() error = %v, want ErrSizeMismatch", err)
	}
	final, err := cache.FinalPath(domain.AssetID("asset-1"), "game.iso")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(final); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("size mismatch created final path, Lstat error = %v", err)
	}
}

func TestPromotionUsesAtomicNoReplaceAndPreservesExistingContent(t *testing.T) {
	cacheDir := t.TempDir()
	cache, err := OpenCache(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	cache.availableBytes = func(*os.File) (uint64, error) { return 1 << 20, nil }
	assetID := domain.AssetID("asset-1")
	plan, err := cache.Preflight(assetID, "game.iso", 4)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plan.Close() }()
	staged, err := cache.CreateStaging("job-1", "game.iso")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = staged.Cleanup() }()
	if _, err := staged.File.Write([]byte("safe")); err != nil {
		t.Fatal(err)
	}
	final, err := cache.FinalPath(assetID, "game.iso")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(final, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.Promote(staged, plan); !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("Promote() error = %v, want ErrDestinationExists", err)
	}
	if data, err := os.ReadFile(final); err != nil || string(data) != "keep" {
		t.Fatalf("existing destination was changed: data=%q error=%v", data, err)
	}
}

func TestPromoteVerifiesAndReturnsOnlyFinalCacheLocation(t *testing.T) {
	cacheDir := t.TempDir()
	cache, err := OpenCache(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	cache.availableBytes = func(*os.File) (uint64, error) { return 4, nil }
	assetID := domain.AssetID("asset-1")
	plan, err := cache.Preflight(assetID, "game.iso", 4)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plan.Close() }()
	staged, err := cache.CreateStaging("job-2", "game.iso")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := staged.File.Write([]byte("safe")); err != nil {
		t.Fatal(err)
	}

	final, err := cache.Promote(staged, plan)
	if err != nil {
		t.Fatal(err)
	}
	if final != filepath.Join(cacheDir, "assets", string(assetID), "game.iso") {
		t.Fatalf("Promote() location = %q, want final cache path", final)
	}
	if err := staged.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(final); err != nil || string(data) != "safe" {
		t.Fatalf("promoted content = %q, error = %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(cacheDir, ".staging", "job-2")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging Job subtree remains after cleanup: %v", err)
	}
}

func TestPreflightRejectsSymlinkedFinalParent(t *testing.T) {
	cacheDir := t.TempDir()
	outside := t.TempDir()
	cache, err := OpenCache(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	if err := os.Remove(filepath.Join(cacheDir, "assets")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cacheDir, "assets")); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Preflight(domain.AssetID("asset-1"), "game.iso", 1); err == nil {
		t.Fatal("Preflight unexpectedly followed a symlinked final parent")
	}
	if _, err := os.Lstat(filepath.Join(outside, "asset-1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight created a path outside the cache: %v", err)
	}
}

func TestPromoteRejectsReplacedFinalParentAfterPreflight(t *testing.T) {
	cacheDir := t.TempDir()
	outside := t.TempDir()
	cache, err := OpenCache(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	cache.availableBytes = func(*os.File) (uint64, error) { return 1 << 20, nil }
	assetID := domain.AssetID("asset-1")
	plan, err := cache.Preflight(assetID, "game.iso", 4)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plan.Close() }()
	staged, err := cache.CreateStaging("job-3", "game.iso")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = staged.Cleanup() }()
	if _, err := staged.File.Write([]byte("safe")); err != nil {
		t.Fatal(err)
	}

	finalParent := filepath.Join(cacheDir, "assets", string(assetID))
	if err := os.Remove(finalParent); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, finalParent); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Promote(staged, plan); err == nil {
		t.Fatal("Promote unexpectedly accepted a replaced final parent")
	}
	if _, err := os.Lstat(filepath.Join(outside, "game.iso")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("parent replacement caused promotion outside the cache: %v", err)
	}
}
