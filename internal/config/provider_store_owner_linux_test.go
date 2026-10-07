//go:build linux

package config

import (
	"errors"
	"os"
	"testing"
)

func TestProviderConfigStoreRejectsFileOwnedByAnotherUID(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing fixture ownership requires root")
	}
	store, path := newTestProviderStore(t)
	writeProviderConfigFixture(t, path, validIGDBDocument(), 0o600)
	if err := os.Chown(path, 1, -1); err != nil {
		t.Skipf("cannot set a distinct fixture owner: %v", err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrProviderConfigOwner) {
		t.Fatalf("Load() error = %v, want ErrProviderConfigOwner", err)
	}
}
