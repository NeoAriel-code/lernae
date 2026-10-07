//go:build linux

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProviderConfigFileLockSerializesSeparateDescriptors(t *testing.T) {
	_, path := newTestProviderStore(t)
	if err := ensurePrivateProviderConfigDirectory(filepath.Dir(path)); err != nil {
		t.Fatalf("ensure config directory: %v", err)
	}

	unlockFirst, err := acquireProviderConfigFileLock(path)
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	defer func() {
		if unlockFirst != nil {
			unlockFirst()
		}
	}()
	lockInfo, err := os.Stat(filepath.Join(filepath.Dir(path), ".providers.lock"))
	if err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	if got := lockInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("lock file permissions = %04o, want 0600", got)
	}

	started := make(chan struct{})
	type lockResult struct {
		unlock func()
		err    error
	}
	acquired := make(chan lockResult, 1)
	go func() {
		close(started)
		unlock, err := acquireProviderConfigFileLock(path)
		acquired <- lockResult{unlock: unlock, err: err}
	}()
	<-started
	select {
	case result := <-acquired:
		if result.unlock != nil {
			result.unlock()
		}
		if result.err != nil {
			t.Fatalf("acquire second lock: %v", result.err)
		}
		t.Fatal("second descriptor acquired the advisory lock while the first held it")
	case <-time.After(100 * time.Millisecond):
	}

	unlockFirst()
	unlockFirst = nil
	select {
	case result := <-acquired:
		if result.err != nil {
			t.Fatalf("acquire second lock after release: %v", result.err)
		}
		result.unlock()
	case <-time.After(time.Second):
		t.Fatal("second descriptor did not acquire the advisory lock after release")
	}
}
