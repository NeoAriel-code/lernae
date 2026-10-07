//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package config

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// acquireProviderConfigFileLock uses the same advisory lock file on Unix
// platforms as the Linux implementation, so provider and settings updates
// remain serialized across processes and across both stores.
func acquireProviderConfigFileLock(configPath string) (func(), error) {
	lockPath := filepath.Join(filepath.Dir(configPath), ".providers.lock")
	fd, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, ErrProviderConfigSymlink
		}
		return nil, ErrProviderConfigLock
	}
	file := os.NewFile(uintptr(fd), lockPath)
	if err := validateProviderConfigLockFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	for {
		err = unix.Flock(fd, unix.LOCK_EX)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, ErrProviderConfigLock
	}
	if err := validateProviderConfigLockFile(file); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = file.Close()
	}, nil
}
