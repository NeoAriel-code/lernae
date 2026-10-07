//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package config

// Platforms without a verified cross-process advisory-lock implementation
// fail closed instead of silently allowing concurrent config writes.
func acquireProviderConfigFileLock(string) (func(), error) {
	return nil, ErrProviderConfigLock
}
