//go:build !linux

package config

import (
	"os"
)

// Non-Linux platforms lack the Linux O_NOFOLLOW and UID-specific contract.
// The pre/open/post identity checks still reject ordinary final-path swaps.
func openProviderConfigFile(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, ErrProviderConfigSymlink
	}
	if !before.Mode().IsRegular() {
		return nil, ErrProviderConfigNotRegular
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(after, opened) {
		_ = file.Close()
		if err == nil && after.Mode()&os.ModeSymlink != 0 {
			return nil, ErrProviderConfigSymlink
		}
		return nil, ErrProviderConfigRead
	}
	return file, nil
}

func providerConfigOwnedByCurrentUser(os.FileInfo) bool {
	return true
}
