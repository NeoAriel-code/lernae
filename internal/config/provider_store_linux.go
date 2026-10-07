//go:build linux

package config

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func openProviderConfigFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, ErrProviderConfigSymlink
		}
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func providerConfigOwnedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
