package agentops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"lernae/internal/agent"
	"lernae/internal/domain"
)

const rcloneSmokeOptInEnv = "LERNAE_P105B_RCLONE_SMOKE"

func TestRcloneRestoreIsolatedBinarySmoke(t *testing.T) {
	if os.Getenv(rcloneSmokeOptInEnv) != "1" {
		t.Skipf("set %s=1 to run the isolated installed-rclone smoke", rcloneSmokeOptInEnv)
	}
	if _, err := ResolveRcloneExecutable(); err != nil {
		t.Fatalf("installed rclone is required for the explicitly enabled smoke: %v", err)
	}

	smokeRoot, err := os.MkdirTemp("/tmp", "lernae-p1-05b-rclone-smoke-")
	if err != nil {
		t.Fatalf("create owned smoke root: %v", err)
	}
	smokeRootInfo, err := os.Lstat(smokeRoot)
	if err != nil || !smokeRootInfo.IsDir() || smokeRootInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("inspect newly created smoke root: info=%v err=%v", smokeRootInfo, err)
	}
	t.Logf("SMOKE_ROOT=%s", smokeRoot)
	t.Cleanup(func() {
		currentRoot, err := os.Lstat(smokeRoot)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				t.Logf("SMOKE_ROOT_REMOVED=%s", smokeRoot)
				return
			}
			t.Errorf("inspect owned smoke root before cleanup: %v", err)
			return
		}
		if currentRoot.Mode()&os.ModeSymlink != 0 || !os.SameFile(smokeRootInfo, currentRoot) {
			t.Errorf("refusing to remove smoke root that is no longer the exact directory created by this test: %s", smokeRoot)
			return
		}
		if err := os.RemoveAll(smokeRoot); err != nil {
			t.Errorf("remove owned smoke root: %v", err)
			return
		}
		t.Logf("SMOKE_ROOT_REMOVED=%s", smokeRoot)
	})

	sourceRoot := filepath.Join(smokeRoot, "source")
	configRoot := filepath.Join(smokeRoot, "config")
	cacheRoot := filepath.Join(smokeRoot, "cache")
	for _, directory := range []string{sourceRoot, configRoot, filepath.Join(smokeRoot, "home"), filepath.Join(smokeRoot, "xdg-config"), filepath.Join(smokeRoot, "xdg-cache"), filepath.Join(smokeRoot, "rclone-cache")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create isolated smoke directory: %v", err)
		}
	}

	configPath := filepath.Join(configRoot, "rclone.conf")
	config := fmt.Sprintf("[archive]\ntype = alias\nremote = %s\n", sourceRoot)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write isolated rclone alias config: %v", err)
	}
	// Keep every possible rclone default/config/cache lookup under the owned root.
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", filepath.Join(smokeRoot, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(smokeRoot, "xdg-config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(smokeRoot, "xdg-cache"))
	t.Setenv("RCLONE_CONFIG", configPath)
	t.Setenv("RCLONE_CACHE_DIR", filepath.Join(smokeRoot, "rclone-cache"))

	cache, err := OpenCache(cacheRoot)
	if err != nil {
		t.Fatalf("open isolated Agent cache: %v", err)
	}
	t.Cleanup(func() {
		if err := cache.Close(); err != nil {
			t.Errorf("close isolated Agent cache: %v", err)
		}
	})
	executor, err := NewRcloneRestoreExecutor(cache)
	if err != nil {
		t.Fatalf("create rclone restore executor: %v", err)
	}

	t.Run("stat copy verify promote and local ready", func(t *testing.T) {
		content := bytes.Repeat([]byte("isolated-rclone-smoke\n"), 128)
		if err := os.WriteFile(filepath.Join(sourceRoot, "ready.bin"), content, 0o600); err != nil {
			t.Fatal(err)
		}
		request := rcloneSmokeRequest("job-smoke-ready", "asset-smoke-ready", "ready.bin", int64(len(content)))
		var progress []agent.RestoreProgress
		result, err := executor.Restore(context.Background(), request, func(event agent.RestoreProgress) error {
			progress = append(progress, event)
			return nil
		})
		if err != nil {
			t.Fatalf("isolated real-rclone restore failed: %v", err)
		}
		if err := result.ValidateFor(request); err != nil {
			t.Fatalf("restore result lacks LOCAL_READY proof: %v", err)
		}
		if len(progress) < 5 || progress[0].Phase != agent.RestorePhaseValidating || progress[len(progress)-1].Phase != agent.RestorePhaseComplete {
			t.Fatalf("restore progress did not cover the full P1-05A pipeline: %#v", progress)
		}
		for index, event := range progress {
			if err := agent.ValidateProgressAfter(progressAt(progress, index-1), event); err != nil {
				t.Fatalf("restore progress regressed: %v; events=%#v", err, progress)
			}
			if event.Phase == agent.RestorePhaseCopying && event.CurrentBytes >= request.Asset.TotalSizeBytes {
				t.Fatalf("copy progress reached total before verification and promotion: %#v", event)
			}
		}
		finalPath := filepath.Join(cacheRoot, filepath.FromSlash(result.RelativePath))
		actual, err := os.ReadFile(finalPath)
		if err != nil || !bytes.Equal(actual, content) {
			t.Fatalf("promoted file content mismatch: bytes=%d err=%v", len(actual), err)
		}
		if _, err := os.Lstat(filepath.Join(cacheRoot, ".staging", request.JobID)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("successful restore left owned staging: %v", err)
		}
	})

	t.Run("missing object fails before staging or final", func(t *testing.T) {
		request := rcloneSmokeRequest("job-smoke-missing", "asset-smoke-missing", "missing.bin", 8)
		_, err := executor.Restore(context.Background(), request, nil)
		assertRcloneCategory(t, err, RcloneFailureStatFailed)
		assertRcloneSmokeNoOutput(t, request, cacheRoot)
	})

	t.Run("stat size mismatch fails before staging or final", func(t *testing.T) {
		content := []byte("known-size")
		if err := os.WriteFile(filepath.Join(sourceRoot, "mismatch.bin"), content, 0o600); err != nil {
			t.Fatal(err)
		}
		request := rcloneSmokeRequest("job-smoke-mismatch", "asset-smoke-mismatch", "mismatch.bin", int64(len(content)+1))
		_, err := executor.Restore(context.Background(), request, nil)
		assertRcloneCategory(t, err, RcloneFailureSizeMismatch)
		assertRcloneSmokeNoOutput(t, request, cacheRoot)
	})

	t.Run("cancellation reaps rclone and cleans owned staging", func(t *testing.T) {
		const sourceBytes = int64(16 << 20)
		content := bytes.Repeat([]byte{0x5a}, int(sourceBytes))
		if err := os.WriteFile(filepath.Join(sourceRoot, "cancel.bin"), content, 0o600); err != nil {
			t.Fatal(err)
		}
		// This test-owned limit ensures cancellation happens while copyto is live.
		t.Setenv("RCLONE_BWLIMIT", "32k")
		request := rcloneSmokeRequest("job-smoke-cancel", "asset-smoke-cancel", "cancel.bin", sourceBytes)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var childPID int
		var copiedBytes int64
		partialStageObserved := false
		finalAbsentDuringCopy := false
		result, err := executor.Restore(ctx, request, func(event agent.RestoreProgress) error {
			if event.Phase == agent.RestorePhaseCopying && event.CurrentBytes > 0 && childPID == 0 {
				childPID = findRcloneChildProcess()
				copiedBytes = event.CurrentBytes
				stagePath := filepath.Join(cacheRoot, ".staging", request.JobID, request.Parts[0].Filename)
				stageInfo, stageErr := os.Lstat(stagePath)
				partialStageObserved = stageErr == nil && stageInfo.Mode().IsRegular() && stageInfo.Size() > 0 && stageInfo.Size() < sourceBytes
				finalPath := filepath.Join(cacheRoot, "assets", string(request.Asset.ID), request.Parts[0].Filename)
				_, finalErr := os.Lstat(finalPath)
				finalAbsentDuringCopy = errors.Is(finalErr, os.ErrNotExist)
				cancel()
			}
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled restore error = %v, want context.Canceled", err)
		}
		if result.LocalReady {
			t.Fatal("cancelled restore returned LOCAL_READY evidence")
		}
		if childPID <= 0 || copiedBytes <= 0 || copiedBytes >= sourceBytes {
			t.Fatalf("cancellation did not capture a live rclone child with partial progress: pid=%d bytes=%d/%d", childPID, copiedBytes, sourceBytes)
		}
		if !partialStageObserved || !finalAbsentDuringCopy {
			t.Fatalf("partial transfer state was unsafe: partial_staging=%t final_absent=%t", partialStageObserved, finalAbsentDuringCopy)
		}
		if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("rclone child %d was not reaped before restore returned; kill(0) error=%v", childPID, err)
		}
		assertRcloneSmokeNoOutput(t, request, cacheRoot)
	})

	t.Run("existing final file is unchanged", func(t *testing.T) {
		content := []byte("remote replacement candidate")
		sentinel := []byte("existing local file must survive")
		if err := os.WriteFile(filepath.Join(sourceRoot, "existing.bin"), content, 0o600); err != nil {
			t.Fatal(err)
		}
		request := rcloneSmokeRequest("job-smoke-existing", "asset-smoke-existing", "existing.bin", int64(len(content)))
		finalPath := filepath.Join(cacheRoot, "assets", string(request.Asset.ID), request.Parts[0].Filename)
		if err := os.MkdirAll(filepath.Dir(finalPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(finalPath, sentinel, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := executor.Restore(context.Background(), request, nil)
		if !errors.Is(err, ErrDestinationExists) {
			t.Fatalf("restore over an existing final = %v, want ErrDestinationExists", err)
		}
		actual, readErr := os.ReadFile(finalPath)
		if readErr != nil || !bytes.Equal(actual, sentinel) {
			t.Fatalf("existing final changed: bytes=%q err=%v", actual, readErr)
		}
		if _, err := os.Lstat(filepath.Join(cacheRoot, ".staging", request.JobID)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("existing-final conflict created staging: %v", err)
		}
	})
}

func rcloneSmokeRequest(jobID, assetID, filename string, size int64) agent.RestoreAsset {
	asset := domain.AssetID(assetID)
	return agent.RestoreAsset{
		JobID: jobID,
		Asset: domain.Asset{ID: asset, TotalSizeBytes: size},
		Parts: []domain.AssetPart{{ID: domain.AssetPartID("part-" + assetID), AssetID: asset, Role: "rom", Filename: filename, SizeBytes: size}},
		SourceLocation: domain.AssetLocation{
			AssetID: asset, StorageProviderID: agent.RestoreSourceProviderRclone,
			Locator: "archive:" + filename, LocationClass: agent.RestoreSourceClassArchive,
		},
	}
}

func assertRcloneSmokeNoOutput(t *testing.T, request agent.RestoreAsset, cacheRoot string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(cacheRoot, ".staging", request.JobID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed restore created or retained owned staging: %v", err)
	}
	finalPath := filepath.Join(cacheRoot, "assets", string(request.Asset.ID), request.Parts[0].Filename)
	if _, err := os.Lstat(finalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed restore created a final file: %v", err)
	}
}

func findRcloneChildProcess() int {
	taskRoot := filepath.Join("/proc", strconv.Itoa(os.Getpid()), "task")
	tasks, err := os.ReadDir(taskRoot)
	if err != nil {
		return 0
	}
	for _, task := range tasks {
		contents, err := os.ReadFile(filepath.Join(taskRoot, task.Name(), "children"))
		if err != nil {
			continue
		}
		for _, field := range strings.Fields(string(contents)) {
			pid, err := strconv.Atoi(field)
			if err != nil || pid <= 0 {
				continue
			}
			executable, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
			if err == nil && filepath.Base(executable) == "rclone" {
				return pid
			}
		}
	}
	return 0
}
