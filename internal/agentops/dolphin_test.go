package agentops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lernae/internal/domain"
)

const (
	dolphinTestModeEnv     = "LERNAE_TEST_DOLPHIN_MODE"
	dolphinTestArgsFileEnv = "LERNAE_TEST_DOLPHIN_ARGS_FILE"
	dolphinTestSecret      = "private-dolphin-child-output"
)

func TestCacheOpensOnlySupportedLocalReadyGameCubeImage(t *testing.T) {
	const filename = "GameCube ; $(echo literal) & image.iso"
	cache, ready := newReadyGameCubeImage(t, filename, []byte("image"))
	defer func() { _ = ready.Close() }()

	want := filepath.Join(cache.path, "assets", "asset-1", filename)
	if ready.path != want {
		t.Fatalf("ready image path = %q, want cache-derived %q", ready.path, want)
	}
	info, err := ready.file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len("image")) {
		t.Fatalf("ready image handle stat = (%v, %v), want regular exact-size file", info, err)
	}
}

func TestOpenReadyGameCubeImageForSessionQuarantinesWrongSizeFinalAndLeavesSameSizeAlone(t *testing.T) {
	work, edition, asset, parts := validGameCubeImage()
	cache := openTestCache(t)
	stale := []byte("stale bytes that do not match the trusted Asset size")
	writeFinalAsset(t, cache, parts[0].Filename, stale)

	image, err := cache.OpenReadyGameCubeImageForSession("session-stale-1", work, edition, asset, parts)
	if image != nil || !errors.Is(err, ErrLocalReadyImageInvalid) {
		t.Fatalf("OpenReadyGameCubeImageForSession() = (%v, %v), want invalid cache result after quarantine", image, err)
	}
	final := filepath.Join(cache.path, "assets", string(asset.ID), parts[0].Filename)
	if _, err := os.Lstat(final); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale final entry still occupies restore path: %v", err)
	}
	quarantined := filepath.Join(cache.path, ".quarantine", "session-stale-1", string(asset.ID), parts[0].Filename)
	if got, err := os.ReadFile(quarantined); err != nil || !bytes.Equal(got, stale) {
		t.Fatalf("quarantined stale bytes = %q, err=%v; want exact old bytes", got, err)
	}
	if _, err := cache.OpenReadyGameCubeImageForSession("session-stale-2", work, edition, asset, parts); !errors.Is(err, ErrLocalReadyImageInvalid) {
		t.Fatalf("missing final entry error = %v, want invalid-cache evidence for the bounded restore fallback", err)
	}

	if err := os.WriteFile(final, []byte("valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	ready, err := cache.OpenReadyGameCubeImageForSession("session-good", work, edition, asset, parts)
	if err != nil {
		t.Fatalf("same-size trusted cache entry was not accepted: %v", err)
	}
	defer func() { _ = ready.Close() }()
	if _, err := os.Lstat(filepath.Join(cache.path, ".quarantine", "session-good")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("same-size cache entry created quarantine state: %v", err)
	}
}

func TestOpenReadyGameCubeImageForSessionFailsClosedOnUnsafeAndOccupiedQuarantineEntries(t *testing.T) {
	t.Run("symlink final is not moved", func(t *testing.T) {
		work, edition, asset, parts := validGameCubeImage()
		cache := openTestCache(t)
		mkdirFinalAssetDir(t, cache)
		outside := filepath.Join(t.TempDir(), "outside.iso")
		if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		final := filepath.Join(cache.path, "assets", string(asset.ID), parts[0].Filename)
		if err := os.Symlink(outside, final); err != nil {
			t.Fatal(err)
		}

		image, err := cache.OpenReadyGameCubeImageForSession("session-symlink", work, edition, asset, parts)
		if image != nil || err == nil || errors.Is(err, ErrLocalReadyImageInvalid) {
			t.Fatalf("unsafe symlink result = (%v, %v), want fail-closed non-fallback error", image, err)
		}
		if info, err := os.Lstat(final); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("symlink final was changed: info=%v err=%v", info, err)
		}
		if got, err := os.ReadFile(outside); err != nil || string(got) != "outside" {
			t.Fatalf("symlink target changed: bytes=%q err=%v", got, err)
		}
	})

	t.Run("occupied quarantine destination is preserved", func(t *testing.T) {
		work, edition, asset, parts := validGameCubeImage()
		cache := openTestCache(t)
		stale := []byte("stale wrong-size bytes")
		writeFinalAsset(t, cache, parts[0].Filename, stale)
		quarantine := filepath.Join(cache.path, ".quarantine", "session-conflict", string(asset.ID))
		if err := os.MkdirAll(quarantine, 0o700); err != nil {
			t.Fatal(err)
		}
		conflicting := filepath.Join(quarantine, parts[0].Filename)
		if err := os.WriteFile(conflicting, []byte("preserve this"), 0o600); err != nil {
			t.Fatal(err)
		}

		image, err := cache.OpenReadyGameCubeImageForSession("session-conflict", work, edition, asset, parts)
		if image != nil || err == nil || errors.Is(err, ErrLocalReadyImageInvalid) {
			t.Fatalf("occupied quarantine result = (%v, %v), want fail-closed non-fallback error", image, err)
		}
		final := filepath.Join(cache.path, "assets", string(asset.ID), parts[0].Filename)
		if got, err := os.ReadFile(final); err != nil || !bytes.Equal(got, stale) {
			t.Fatalf("stale final changed after quarantine conflict: bytes=%q err=%v", got, err)
		}
		if got, err := os.ReadFile(conflicting); err != nil || string(got) != "preserve this" {
			t.Fatalf("quarantine conflict was overwritten: bytes=%q err=%v", got, err)
		}
	})
}

func TestCacheRejectsUnsupportedGameCubeLaunchShapes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*domain.Work, *domain.Edition, *domain.Asset, *[]domain.AssetPart)
	}{
		{name: "wrong medium", mutate: func(work *domain.Work, _ *domain.Edition, _ *domain.Asset, _ *[]domain.AssetPart) {
			work.Medium = domain.MediumVideo
		}},
		{name: "wrong platform", mutate: func(_ *domain.Work, edition *domain.Edition, _ *domain.Asset, _ *[]domain.AssetPart) {
			edition.Platform = "playstation2"
		}},
		{name: "wrong edition format", mutate: func(_ *domain.Work, edition *domain.Edition, _ *domain.Asset, _ *[]domain.AssetPart) {
			edition.Format = "archive"
		}},
		{name: "wrong asset kind", mutate: func(_ *domain.Work, _ *domain.Edition, asset *domain.Asset, _ *[]domain.AssetPart) {
			asset.Kind = "archive"
		}},
		{name: "asset edition mismatch", mutate: func(_ *domain.Work, _ *domain.Edition, asset *domain.Asset, _ *[]domain.AssetPart) {
			asset.EditionID = "other-edition"
		}},
		{name: "asset ID traversal", mutate: func(_ *domain.Work, _ *domain.Edition, asset *domain.Asset, parts *[]domain.AssetPart) {
			asset.ID = "../outside"
			(*parts)[0].AssetID = asset.ID
		}},
		{name: "edition work mismatch", mutate: func(work *domain.Work, edition *domain.Edition, _ *domain.Asset, _ *[]domain.AssetPart) {
			edition.WorkID = "other-work"
			_ = work
		}},
		{name: "missing part", mutate: func(_ *domain.Work, _ *domain.Edition, _ *domain.Asset, parts *[]domain.AssetPart) { *parts = nil }},
		{name: "multipart asset", mutate: func(_ *domain.Work, _ *domain.Edition, _ *domain.Asset, parts *[]domain.AssetPart) {
			*parts = append(*parts, (*parts)[0])
		}},
		{name: "non-ROM part", mutate: func(_ *domain.Work, _ *domain.Edition, _ *domain.Asset, parts *[]domain.AssetPart) {
			(*parts)[0].Role = "cover"
		}},
		{name: "part belongs to another asset", mutate: func(_ *domain.Work, _ *domain.Edition, _ *domain.Asset, parts *[]domain.AssetPart) {
			(*parts)[0].AssetID = "other-asset"
		}},
		{name: "relative path override", mutate: func(_ *domain.Work, _ *domain.Edition, _ *domain.Asset, parts *[]domain.AssetPart) {
			(*parts)[0].RelativePath = "assets/other/game.iso"
		}},
		{name: "unknown part size", mutate: func(_ *domain.Work, _ *domain.Edition, _ *domain.Asset, parts *[]domain.AssetPart) {
			(*parts)[0].SizeBytes = 0
		}},
		{name: "mismatched part size", mutate: func(_ *domain.Work, _ *domain.Edition, _ *domain.Asset, parts *[]domain.AssetPart) {
			(*parts)[0].SizeBytes++
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cache := newGameCubeCache(t, "game.iso", []byte("image"))
			work, edition, asset, parts := validGameCubeImage()
			test.mutate(&work, &edition, &asset, &parts)
			if image, err := cache.OpenReadyGameCubeImage(work, edition, asset, parts); err == nil {
				_ = image.Close()
				t.Fatal("unsupported launch shape unexpectedly produced a ready-image handle")
			}
		})
	}
}

func TestCacheRejectsUnsafeOrUnreadyGameCubeFiles(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, cache *Cache, filename string)
		parts func([]domain.AssetPart) []domain.AssetPart
	}{
		{name: "missing final file", setup: func(t *testing.T, _ *Cache, _ string) {}},
		{name: "directory instead of file", setup: func(t *testing.T, cache *Cache, filename string) {
			mkdirFinalAssetDir(t, cache)
			if err := os.Mkdir(filepath.Join(cache.path, "assets", "asset-1", filename), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink to outside file", setup: func(t *testing.T, cache *Cache, filename string) {
			mkdirFinalAssetDir(t, cache)
			outside := filepath.Join(t.TempDir(), "outside.iso")
			if err := os.WriteFile(outside, []byte("image"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(cache.path, "assets", "asset-1", filename)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlinked Asset directory", setup: func(t *testing.T, cache *Cache, filename string) {
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, filename), []byte("image"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(cache.path, "assets", "asset-1")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wrong byte count", setup: func(t *testing.T, cache *Cache, filename string) {
			writeFinalAsset(t, cache, filename, []byte("four"))
		}},
		{name: "staging path is not a ready asset", setup: func(t *testing.T, cache *Cache, filename string) {
			staging := filepath.Join(cache.path, ".staging", "job-1")
			if err := os.Mkdir(staging, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(staging, filename), []byte("image"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "filename traversal", setup: func(t *testing.T, _ *Cache, _ string) {}, parts: func(parts []domain.AssetPart) []domain.AssetPart { parts[0].Filename = "../outside.iso"; return parts }},
		{name: "absolute filename", setup: func(t *testing.T, _ *Cache, _ string) {}, parts: func(parts []domain.AssetPart) []domain.AssetPart {
			parts[0].Filename = filepath.Join(t.TempDir(), "outside.iso")
			return parts
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cache := openTestCache(t)
			test.setup(t, cache, "game.iso")
			work, edition, asset, parts := validGameCubeImage()
			if test.parts != nil {
				parts = test.parts(parts)
			}
			if image, err := cache.OpenReadyGameCubeImage(work, edition, asset, parts); err == nil {
				_ = image.Close()
				t.Fatal("unready or unsafe cache file unexpectedly produced a ready-image handle")
			}
		})
	}
}

func TestDolphinExecutableResolverAcceptsOnlyTrustedFixedLookup(t *testing.T) {
	helperPath, _ := installDolphinTestHelper(t, "success")
	called := ""
	resolved, err := resolveDolphinExecutable(func(name string) (string, error) {
		called = name
		return helperPath, nil
	})
	if err != nil {
		t.Fatalf("resolveDolphinExecutable() error = %v", err)
	}
	if called != "dolphin-emu" || resolved != helperPath {
		t.Fatalf("resolver lookup = %q, path = %q; want fixed dolphin-emu and %q", called, resolved, helperPath)
	}
}

func TestDolphinExecutableResolverRejectsMissingMalformedAndUntrustedResults(t *testing.T) {
	helperPath, _ := installDolphinTestHelper(t, "success")
	testDir := dolphinTestTempDir(t)
	noExec := filepath.Join(testDir, "not-executable")
	if err := os.WriteFile(noExec, []byte("not executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	writable := filepath.Join(testDir, "writable-executable")
	if err := os.WriteFile(writable, []byte("not launched"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o702); err != nil {
		t.Fatal(err)
	}
	wrongName := filepath.Join(filepath.Dir(helperPath), "other-executable")
	if err := os.Link(helperPath, wrongName); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(testDir, "missing-dolphin-emu")
	nonregular := filepath.Join(testDir, "directory")
	if err := os.Mkdir(nonregular, 0o700); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		fail bool
	}{
		{name: "lookup failed", fail: true},
		{name: "empty path", path: ""},
		{name: "relative path", path: "bin/dolphin-emu"},
		{name: "path traversal", path: filepath.Join(testDir, "sub") + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "not-clean"},
		{name: "wrong executable name", path: wrongName},
		{name: "missing resolved file", path: missing},
		{name: "nonregular executable", path: nonregular},
		{name: "nonexecutable file", path: noExec},
		{name: "group-or-other-writable executable", path: writable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := ""
			lookup := func(name string) (string, error) {
				called = name
				if test.fail {
					return "", errors.New("private lookup detail")
				}
				return test.path, nil
			}
			if _, err := resolveDolphinExecutable(lookup); err == nil {
				t.Fatal("untrusted executable lookup unexpectedly succeeded")
			}
			if called != "dolphin-emu" {
				t.Fatalf("resolver queried %q, want fixed dolphin-emu", called)
			}
		})
	}
}

func TestDolphinExecutableResolverReturnsCanonicalSymlinkTarget(t *testing.T) {
	helperPath, _ := installDolphinTestHelper(t, "success")
	linkDir := dolphinTestTempDir(t)
	lookupPath := filepath.Join(linkDir, dolphinExecutableName)
	if err := os.Symlink(helperPath, lookupPath); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveDolphinExecutable(func(name string) (string, error) {
		if name != dolphinExecutableName {
			t.Fatalf("lookup name = %q, want fixed %q", name, dolphinExecutableName)
		}
		return lookupPath, nil
	})
	if err != nil {
		t.Fatalf("resolveDolphinExecutable() error = %v", err)
	}
	if resolved != helperPath {
		t.Fatalf("resolved executable = %q, want canonical path %q", resolved, helperPath)
	}
}

func TestDolphinExecutableResolverRejectsWritableAncestorAndSymlinkedAncestor(t *testing.T) {
	root := dolphinTestTempDir(t)
	unsafeDir := filepath.Join(root, "writable-bin")
	if err := os.Mkdir(unsafeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	helperPath := copyDolphinTestExecutable(t, unsafeDir)
	if err := os.Chmod(unsafeDir, 0o777); err != nil {
		t.Fatal(err)
	}

	t.Run("writable PATH directory", func(t *testing.T) {
		t.Setenv("PATH", unsafeDir)
		if _, err := resolveDolphinExecutable(exec.LookPath); err == nil {
			t.Fatal("executable in a group/other-writable PATH ancestor was accepted")
		}
	})

	t.Run("trusted PATH symlink resolves into writable directory", func(t *testing.T) {
		linkedPath := filepath.Join(root, "trusted-looking-bin")
		if err := os.Symlink(unsafeDir, linkedPath); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", linkedPath)
		resolved, err := exec.LookPath(dolphinExecutableName)
		if err != nil {
			t.Fatalf("LookPath(%q) error = %v; helper = %q", dolphinExecutableName, err, helperPath)
		}
		if filepath.Dir(resolved) != linkedPath {
			t.Fatalf("LookPath path = %q, want symlink ancestor %q", resolved, linkedPath)
		}
		if _, err := resolveDolphinExecutable(exec.LookPath); err == nil {
			t.Fatalf("symlinked PATH ancestor resolving to %q was accepted", unsafeDir)
		}
	})
}

func TestNewDolphinRunnerDoesNotResolveExecutableUntilStart(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	runner := NewDolphinRunner()
	if runner == nil {
		t.Fatal("NewDolphinRunner() returned nil")
	}
	_, image := newReadyGameCubeImage(t, "game.iso", []byte("image"))
	defer func() { _ = image.Close() }()
	if _, err := runner.Start(context.Background(), image); !errors.Is(err, ErrDolphinExecutableMissing) {
		t.Fatalf("Start() with no Dolphin = %v, want lazy ErrDolphinExecutableMissing", err)
	}
}

func TestDolphinRunnerUsesFixedArgvWithoutShell(t *testing.T) {
	const filename = "GameCube ; $(echo literal) & image.iso"
	_, image := newReadyGameCubeImage(t, filename, []byte("image"))
	defer func() { _ = image.Close() }()
	_, argsFile := installDolphinTestHelper(t, "success")

	process, err := NewDolphinRunner().Start(context.Background(), image)
	if err != nil {
		t.Fatalf("DolphinRunner.Start() error = %v", err)
	}
	if err := process.Wait(); err != nil {
		t.Fatalf("DolphinProcess.Wait() error = %v", err)
	}

	got := readDolphinTestArgs(t, argsFile)
	want := []string{"-b", "-e", filepath.Join(image.cache.path, "assets", "asset-1", filename)}
	if !equalStrings(got, want) {
		t.Fatalf("Dolphin argv = %#v, want exactly %#v", got, want)
	}
}

func TestDolphinRunnerRejectsForgedOrChangedReadyImage(t *testing.T) {
	if _, err := NewDolphinRunner().Start(context.Background(), &ReadyGameCubeImage{}); err == nil {
		t.Fatal("zero-value ready-image handle unexpectedly launched")
	}

	_, image := newReadyGameCubeImage(t, "game.iso", []byte("image"))
	defer func() { _ = image.Close() }()
	argsFile := filepath.Join(t.TempDir(), "argv.json")
	helperPath := installDolphinTestHelperOnPath(t, "success", argsFile)
	original := image.path
	backup := original + ".original"
	if err := os.Rename(original, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := NewDolphinRunner()
	runner.lookPath = func(name string) (string, error) { return helperPath, nil }
	if _, err := runner.Start(context.Background(), image); err == nil {
		t.Fatal("changed final-file identity unexpectedly launched")
	}
	if _, err := os.Stat(argsFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("helper was started before identity failure: stat args file error = %v", err)
	}
}

func TestDolphinRunnerSanitizesChildStartAndExitFailures(t *testing.T) {
	_, image := newReadyGameCubeImage(t, "game.iso", []byte("image"))
	defer func() { _ = image.Close() }()
	helperPath, argsFile := installDolphinTestHelper(t, "nonzero")

	startFailure := NewDolphinRunner()
	startFailure.lookPath = func(string) (string, error) { return helperPath, nil }
	startFailure.start = func(context.Context, string, ...string) (*exec.Cmd, error) {
		return nil, errors.New("private executable path and stderr")
	}
	if _, err := startFailure.Start(context.Background(), image); !errors.Is(err, ErrDolphinStartFailed) || strings.Contains(err.Error(), "private") {
		t.Fatalf("child start error = %v, want sanitized ErrDolphinStartFailed", err)
	}

	runner := NewDolphinRunner()
	runner.lookPath = func(string) (string, error) { return helperPath, nil }
	process, err := runner.Start(context.Background(), image)
	if err != nil {
		t.Fatalf("DolphinRunner.Start() error = %v", err)
	}
	err = process.Wait()
	var exitErr *DolphinExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode != 23 || strings.Contains(err.Error(), dolphinTestSecret) {
		t.Fatalf("DolphinProcess.Wait() error = %v, want sanitized exit status 23", err)
	}
	if _, err := os.Stat(argsFile); err != nil {
		t.Fatalf("test helper did not execute: %v", err)
	}
}

func TestDolphinRunnerCancellationWaitsForChildReap(t *testing.T) {
	_, image := newReadyGameCubeImage(t, "game.iso", []byte("image"))
	defer func() { _ = image.Close() }()
	_, argsFile := installDolphinTestHelper(t, "wait")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	process, err := NewDolphinRunner().Start(ctx, image)
	if err != nil {
		cancel()
		t.Fatalf("DolphinRunner.Start() error = %v", err)
	}
	waitForFile(t, argsFile)
	cancel()
	if err := process.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("DolphinProcess.Wait() after cancellation = %v, want context.Canceled", err)
	}
	if process.command == nil || process.command.ProcessState == nil {
		t.Fatal("Dolphin child was not waited/reaped after cancellation")
	}
}

func TestClassifyDolphinWaitResultPreservesObservedExitAcrossCancellation(t *testing.T) {
	tests := []struct {
		name          string
		contextErr    error
		waitErr       error
		processExited bool
		exitCode      int
		wantCancel    bool
		wantExitCode  int
		wantExit      bool
	}{
		{
			name: "positive exit observed before later cancellation", contextErr: context.Canceled,
			waitErr: errors.New("exit status 23"), processExited: true, exitCode: 23,
			wantExitCode: 23, wantExit: true,
		},
		{
			name: "context-cancelled signal termination", contextErr: context.Canceled,
			waitErr: errors.New("signal: killed"), processExited: false, exitCode: -1,
			wantCancel: true,
		},
		{
			name: "normal zero exit remains normal", contextErr: context.Canceled,
			processExited: true, exitCode: 0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := classifyDolphinWaitResult(test.contextErr, test.waitErr, test.processExited, test.exitCode)
			if test.wantCancel {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("classification error = %v, want context.Canceled", err)
				}
				return
			}
			if test.wantExit {
				var exitErr *DolphinExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode != test.wantExitCode {
					t.Fatalf("classification error = %v, want Dolphin exit code %d", err, test.wantExitCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("classification error = %v, want normal exit", err)
			}
		})
	}
}

func newGameCubeCache(t *testing.T, filename string, contents []byte) *Cache {
	t.Helper()
	cache := openTestCache(t)
	writeFinalAsset(t, cache, filename, contents)
	return cache
}

func newReadyGameCubeImage(t *testing.T, filename string, contents []byte) (*Cache, *ReadyGameCubeImage) {
	t.Helper()
	cache := newGameCubeCache(t, filename, contents)
	work, edition, asset, parts := validGameCubeImage()
	asset.TotalSizeBytes = int64(len(contents))
	parts[0].SizeBytes = int64(len(contents))
	parts[0].Filename = filename
	image, err := cache.OpenReadyGameCubeImage(work, edition, asset, parts)
	if err != nil {
		t.Fatalf("Cache.OpenReadyGameCubeImage() error = %v", err)
	}
	return cache, image
}

func openTestCache(t *testing.T) *Cache {
	t.Helper()
	cache, err := OpenCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	return cache
}

func validGameCubeImage() (domain.Work, domain.Edition, domain.Asset, []domain.AssetPart) {
	work := domain.Work{ID: "work-1", Medium: domain.MediumGame, WorkType: "game"}
	edition := domain.Edition{ID: "edition-1", WorkID: work.ID, Platform: "gamecube", Format: "disc_image"}
	asset := domain.Asset{ID: "asset-1", EditionID: edition.ID, Kind: "disc_image", TotalSizeBytes: 5}
	parts := []domain.AssetPart{{ID: "part-1", AssetID: asset.ID, Role: "rom", Filename: "game.iso", SizeBytes: 5}}
	return work, edition, asset, parts
}

func mkdirFinalAssetDir(t *testing.T, cache *Cache) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(cache.path, "assets", "asset-1"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeFinalAsset(t *testing.T, cache *Cache, filename string, contents []byte) {
	t.Helper()
	mkdirFinalAssetDir(t, cache)
	if err := os.WriteFile(filepath.Join(cache.path, "assets", "asset-1", filename), contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func installDolphinTestHelper(t *testing.T, mode string) (string, string) {
	t.Helper()
	helperDir := dolphinTestTempDir(t)
	helperPath := copyDolphinTestExecutable(t, helperDir)
	argsFile := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv("PATH", helperDir)
	t.Setenv(dolphinTestModeEnv, mode)
	t.Setenv(dolphinTestArgsFileEnv, argsFile)
	return helperPath, argsFile
}

func dolphinTestTempDir(t *testing.T) string {
	t.Helper()
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(workingDir, ".dolphin-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func copyDolphinTestExecutable(t *testing.T, helperDir string) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	helperPath := filepath.Join(helperDir, dolphinExecutableName)
	target, err := os.OpenFile(helperPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(helperPath, 0o700); err != nil {
		t.Fatal(err)
	}
	return helperPath
}

func installDolphinTestHelperOnPath(t *testing.T, mode, argsFile string) string {
	t.Helper()
	helperPath, _ := installDolphinTestHelper(t, mode)
	t.Setenv(dolphinTestArgsFileEnv, argsFile)
	return helperPath
}

func readDolphinTestArgs(t *testing.T, filename string) []string {
	t.Helper()
	contents, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read fake Dolphin argv: %v", err)
	}
	var args []string
	if err := json.Unmarshal(contents, &args); err != nil {
		t.Fatalf("decode fake Dolphin argv: %v", err)
	}
	return args
}

func waitForFile(t *testing.T, filename string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filename); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for fake Dolphin helper file %q", filename)
}

func runDolphinTestHelper(args []string) int {
	encoded, err := json.Marshal(args)
	if err != nil || os.WriteFile(os.Getenv(dolphinTestArgsFileEnv), encoded, 0o600) != nil {
		return 91
	}
	switch os.Getenv(dolphinTestModeEnv) {
	case "success":
		return 0
	case "nonzero":
		_, _ = fmt.Fprint(os.Stdout, dolphinTestSecret)
		_, _ = fmt.Fprint(os.Stderr, dolphinTestSecret)
		return 23
	case "wait":
		for {
			time.Sleep(time.Hour)
		}
	default:
		return 92
	}
}
