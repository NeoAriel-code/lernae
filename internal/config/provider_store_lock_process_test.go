//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	providerLockHelperEnv       = "LERNAE_TEST_PROVIDER_LOCK_HELPER"
	providerLockPathEnv         = "LERNAE_TEST_PROVIDER_LOCK_PATH"
	providerLockStartedFileEnv  = "LERNAE_TEST_PROVIDER_LOCK_STARTED_FILE"
	providerLockAcquiredFileEnv = "LERNAE_TEST_PROVIDER_LOCK_ACQUIRED_FILE"
)

func TestProviderConfigFileLockBlocksAcrossProcesses(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "settings.json")
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	parentUnlock, err := acquireProviderConfigFileLock(configPath)
	if err != nil {
		t.Fatalf("acquire parent lock: %v", err)
	}
	parentLocked := true
	defer func() {
		if parentLocked {
			parentUnlock()
		}
	}()

	startedPath := filepath.Join(root, "child-started")
	acquiredPath := filepath.Join(root, "child-acquired")
	command := exec.Command(os.Args[0], "-test.run=^TestProviderConfigFileLockProcessHelper$")
	command.Env = []string{
		providerLockHelperEnv + "=1",
		providerLockPathEnv + "=" + configPath,
		providerLockStartedFileEnv + "=" + startedPath,
		providerLockAcquiredFileEnv + "=" + acquiredPath,
	}
	if err := command.Start(); err != nil {
		t.Fatalf("start lock helper: %v", err)
	}
	if !waitForLockMarker(startedPath, 5*time.Second) {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal("lock helper did not report that it started")
	}
	if waitForLockMarker(acquiredPath, 200*time.Millisecond) {
		parentUnlock()
		parentLocked = false
		_ = command.Wait()
		t.Fatal("second process acquired the advisory lock before the parent released it")
	}
	parentUnlock()
	parentLocked = false
	if err := command.Wait(); err != nil {
		t.Fatalf("wait for lock helper: %v", err)
	}
	if !waitForLockMarker(acquiredPath, time.Second) {
		t.Fatal("lock helper did not acquire the advisory lock after release")
	}
}

func TestProviderConfigFileLockProcessHelper(t *testing.T) {
	if os.Getenv(providerLockHelperEnv) != "1" {
		return
	}
	startedPath := os.Getenv(providerLockStartedFileEnv)
	acquiredPath := os.Getenv(providerLockAcquiredFileEnv)
	if startedPath == "" || acquiredPath == "" {
		os.Exit(2)
	}
	if err := os.WriteFile(startedPath, []byte("started"), 0o600); err != nil {
		os.Exit(3)
	}
	unlock, err := acquireProviderConfigFileLock(os.Getenv(providerLockPathEnv))
	if err != nil {
		os.Exit(4)
	}
	if err := os.WriteFile(acquiredPath, []byte("acquired"), 0o600); err != nil {
		unlock()
		os.Exit(5)
	}
	unlock()
	os.Exit(0)
}

func waitForLockMarker(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
