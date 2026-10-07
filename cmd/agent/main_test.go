package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lernae/internal/agent"
	"lernae/internal/config"
	"lernae/internal/domain"
)

func TestProductionAgentStartsWithoutRcloneAndFailsRestoreWithoutLeakingSource(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", filepath.Join(root, "empty-path"))
	settings := config.Agent{
		SocketPath:  filepath.Join(root, "run", "agent.sock"),
		CachePath:   filepath.Join(root, "cache"),
		StagingPath: filepath.Join(root, "cache", ".staging"),
	}
	server, cache, err := newAgentServer(settings)
	if err != nil {
		t.Fatalf("construct production Agent without rclone installed: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close Agent server: %v", err)
		}
		if err := cache.Close(); err != nil {
			t.Errorf("close Agent cache: %v", err)
		}
	})
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve() }()

	client := agent.UDSClient{SocketPath: settings.SocketPath, Timeout: time.Second}
	if _, err := client.GetStatus(context.Background()); err != nil {
		t.Fatalf("Agent without rclone failed GetStatus: %v", err)
	}
	launch := agent.LaunchAsset{
		SessionID: "session-no-image", WorkID: "work-1", EditionID: "edition-1", AssetID: "asset-no-image", PartID: "part-1",
		Medium: domain.MediumGame, Platform: "gamecube", Format: "disc_image", Role: "rom", Filename: "game.iso", ExpectedBytes: 4,
	}
	started := false
	if _, err := client.LaunchAsset(context.Background(), launch, func(agent.LaunchStarted) bool {
		started = true
		return true
	}); !errors.Is(err, agent.ErrLaunchLocalCacheInvalid) {
		t.Fatalf("missing local-ready image launch error = %v, want typed local-cache invalid failure", err)
	}
	if started {
		t.Fatal("Agent emitted started before validating the local-ready cache file")
	}
	validAssetID := domain.AssetID("asset-valid-no-dolphin")
	validImagePath := filepath.Join(settings.CachePath, "assets", string(validAssetID), "game.iso")
	if err := os.MkdirAll(filepath.Dir(validImagePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(validImagePath, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	missingDolphin := launch
	missingDolphin.SessionID = "session-no-dolphin"
	missingDolphin.AssetID = validAssetID
	started = false
	if _, err := client.LaunchAsset(context.Background(), missingDolphin, func(agent.LaunchStarted) bool {
		started = true
		return true
	}); !errors.Is(err, agent.ErrLaunchStartFailed) || errors.Is(err, agent.ErrLaunchLocalCacheInvalid) {
		t.Fatalf("valid local image with missing Dolphin error = %v, want distinct safe start failure", err)
	}
	if started {
		t.Fatal("Agent emitted started when Dolphin executable was unavailable")
	}
	const locator = "private-remote:private/path/game.iso"
	assetID := domain.AssetID("asset-missing-rclone")
	request := agent.RestoreAsset{
		JobID: "job-missing-rclone",
		Asset: domain.Asset{ID: assetID, TotalSizeBytes: 4},
		Parts: []domain.AssetPart{{ID: "part-missing-rclone", AssetID: assetID, Role: "rom", Filename: "game.iso", SizeBytes: 4}},
		SourceLocation: domain.AssetLocation{
			AssetID: assetID, StorageProviderID: "rclone", Locator: locator, LocationClass: "archive",
		},
	}
	if _, err := client.RestoreAsset(context.Background(), request, nil); err == nil {
		t.Fatal("archive restore unexpectedly succeeded with no rclone executable")
	} else if strings.Contains(err.Error(), locator) || strings.Contains(err.Error(), "executable_unavailable") || strings.Contains(err.Error(), "rclone operation failed") {
		t.Fatalf("restore error leaked locator or executable details: %q", err)
	}
	if _, err := os.Lstat(filepath.Join(settings.CachePath, "assets", string(assetID), "game.iso")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing executable created a final cache asset: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(settings.CachePath, ".staging", request.JobID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing executable created Agent staging: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("close production Agent after missing-rclone request: %v", err)
	}
	if err := <-serveResult; err != nil {
		t.Fatalf("Agent Serve() after close: %v", err)
	}
}

func TestProductionAgentRestoreWritesToConfiguredStagingPath(t *testing.T) {
	root := t.TempDir()
	rcloneDir := filepath.Join(root, "synthetic-bin")
	if err := os.MkdirAll(rcloneDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rcloneStub := "#!/bin/sh\nif [ \"$1\" = \"lsjson\" ]; then\n printf '{\\\"IsDir\\\":false,\\\"Size\\\":4}'\n exit 0\nfi\nexit 1\n"
	if err := os.WriteFile(filepath.Join(rcloneDir, "rclone"), []byte(rcloneStub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", rcloneDir)
	for _, test := range []struct {
		name        string
		cachePath   string
		stagingPath string
	}{
		{
			name:        "explicit staging path",
			cachePath:   filepath.Join(root, "explicit-cache"),
			stagingPath: filepath.Join(root, "explicit-cache", "restore-staging"),
		},
		{
			name:        "derived staging follows effective cache",
			cachePath:   filepath.Join(root, "new-cache"),
			stagingPath: filepath.Join(root, "new-cache", ".staging"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := config.Agent{
				SocketPath:  filepath.Join(root, filepath.Base(test.cachePath)+"-run", "agent.sock"),
				CachePath:   test.cachePath,
				StagingPath: test.stagingPath,
			}
			server, cache, err := newAgentServer(settings)
			if err != nil {
				t.Fatalf("newAgentServer: %v", err)
			}
			serveResult := make(chan error, 1)
			go func() { serveResult <- server.Serve() }()
			client := agent.UDSClient{SocketPath: settings.SocketPath, Timeout: time.Second}
			request := agent.RestoreAsset{
				JobID: "job-configured-staging",
				Asset: domain.Asset{ID: "asset-configured-staging", TotalSizeBytes: 4},
				Parts: []domain.AssetPart{{ID: "part-configured-staging", AssetID: "asset-configured-staging", Role: "rom", Filename: "game.iso", SizeBytes: 4}},
				SourceLocation: domain.AssetLocation{
					AssetID: "asset-configured-staging", StorageProviderID: "rclone", Locator: "synthetic-remote:game.iso", LocationClass: "archive",
				},
			}
			var observedStagingFile bool
			var stagingObservationErr error
			_, restoreErr := client.RestoreAsset(context.Background(), request, func(event agent.RestoreProgress) {
				if event.Phase != agent.RestorePhaseCopying {
					return
				}
				stageFile := filepath.Join(settings.StagingPath, request.JobID, request.Parts[0].Filename)
				info, statErr := os.Lstat(stageFile)
				if statErr != nil || !info.Mode().IsRegular() {
					stagingObservationErr = errors.New("restore did not create its private staging file at the configured path")
					return
				}
				observedStagingFile = true
			})
			if restoreErr == nil {
				t.Fatal("restore succeeded without the intentionally unavailable rclone executable")
			}
			if !observedStagingFile || stagingObservationErr != nil {
				t.Fatalf("restore failed before creating staging data at %q: %v", settings.StagingPath, restoreErr)
			}
			if err := server.Close(); err != nil {
				t.Errorf("close Agent server: %v", err)
			}
			if err := <-serveResult; err != nil {
				t.Errorf("Agent Serve after close: %v", err)
			}
			if err := cache.Close(); err != nil {
				t.Errorf("close Agent cache: %v", err)
			}
		})
	}
}
