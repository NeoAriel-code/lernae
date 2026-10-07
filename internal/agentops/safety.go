// Package agentops owns the Agent's bounded local filesystem operations.
package agentops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"lernae/internal/domain"
)

var (
	safeIdentifier          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	ErrExpectedSizeRequired = errors.New("restore requires a known positive expected size")
	ErrInsufficientSpace    = errors.New("insufficient available cache space for expected Asset bytes")
	ErrSizeMismatch         = errors.New("staged Asset size does not match expected bytes")
	ErrDestinationExists    = errors.New("final cache destination already exists")
	ErrDifferentFilesystem  = errors.New("staging and final cache directories are not on the same filesystem")
)

// Cache anchors restore filesystem access at one configured cache root and a
// private staging directory beneath that root.
type Cache struct {
	path            string
	stagingPath     string
	stagingRelative string
	root            *os.Root
	rootFD          *os.File
	rootInfo        os.FileInfo
	ancestorRoots   []*os.Root
	ancestorFDs     []*os.File
	ancestorNames   []string
	stagingRoot     *os.Root
	stagingFD       *os.File
	stagingInfo     os.FileInfo
	assetsRoot      *os.Root
	assetsFD        *os.File
	assetsInfo      os.FileInfo
	quarantineRoot  *os.Root
	quarantineFD    *os.File
	quarantineInfo  os.FileInfo
	availableBytes  func(*os.File) (uint64, error)
	closeOnce       sync.Once
	closeErr        error
}

// OpenCache opens or creates the cache root by walking from / through trusted
// directory handles. Symlink and non-directory path components are rejected.
func OpenCache(cachePath string) (*Cache, error) {
	return OpenCacheWithStagingPath(cachePath, filepath.Join(cachePath, ".staging"))
}

// OpenCacheWithStagingPath opens or creates the cache root and its configured
// staging directory using descriptor-rooted, no-follow path traversal.
func OpenCacheWithStagingPath(cachePath, stagingPath string) (*Cache, error) {
	if err := validateConfiguredRoot(cachePath); err != nil {
		return nil, err
	}
	cachePath = filepath.Clean(cachePath)
	if err := validateConfiguredRoot(stagingPath); err != nil {
		return nil, errors.New("configured staging root must be an absolute path without traversal")
	}
	stagingPath = filepath.Clean(stagingPath)
	stagingRelative, err := filepath.Rel(cachePath, stagingPath)
	if err != nil || stagingRelative == "." || stagingRelative == ".." ||
		strings.HasPrefix(stagingRelative, ".."+string(filepath.Separator)) || filepath.IsAbs(stagingRelative) {
		return nil, errors.New("configured staging root must be a child directory of the cache root")
	}
	if components := strings.Split(filepath.ToSlash(stagingRelative), "/"); len(components) > 0 && strings.EqualFold(components[0], "assets") {
		return nil, errors.New("configured staging root must not overlap the final Asset namespace")
	}
	roots, directories, names, rootInfo, err := openCachePath(cachePath)
	if err != nil {
		return nil, err
	}
	root := roots[len(roots)-1]
	rootFD := directories[len(directories)-1]
	cache := &Cache{
		path: cachePath, stagingPath: stagingPath, stagingRelative: stagingRelative,
		root: root, rootFD: rootFD, rootInfo: rootInfo,
		ancestorRoots: roots, ancestorFDs: directories, ancestorNames: names,
		availableBytes: availableSpace,
	}
	if err := cache.openStagingRoot(); err != nil {
		_ = cache.Close()
		return nil, err
	}
	if err := cache.openAssetsRoot(); err != nil {
		_ = cache.Close()
		return nil, err
	}
	if err := cache.openQuarantineRoot(); err != nil {
		_ = cache.Close()
		return nil, err
	}
	return cache, nil
}

// openCachePath walks from the filesystem root one component at a time. Each
// parent is validated before it can create or open its child; the child is
// opened with O_NOFOLLOW and compared to an os.Root handle rooted at its
// already-open parent.
func openCachePath(cachePath string) ([]*os.Root, []*os.File, []string, os.FileInfo, error) {
	root, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("open filesystem root for cache walk: %w", err)
	}
	directory, err := os.Open(string(filepath.Separator))
	if err != nil {
		_ = root.Close()
		return nil, nil, nil, nil, fmt.Errorf("open filesystem root descriptor for cache walk: %w", err)
	}
	roots := []*os.Root{root}
	directories := []*os.File{directory}
	names := make([]string, 0)
	parts := strings.Split(strings.TrimPrefix(filepath.ToSlash(cachePath), "/"), "/")
	for index, name := range parts {
		if name == "" || name == "." {
			continue
		}
		parentRoot := roots[len(roots)-1]
		parentDirectory := directories[len(directories)-1]
		if err := validateCacheAncestorHandle("cache path ancestor", parentDirectory); err != nil {
			closeCachePathHandles(roots, directories)
			return nil, nil, nil, nil, err
		}
		info, err := parentRoot.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			if mkdirErr := parentRoot.Mkdir(name, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
				closeCachePathHandles(roots, directories)
				return nil, nil, nil, nil, fmt.Errorf("create configured cache path component: %w", mkdirErr)
			}
			info, err = parentRoot.Lstat(name)
		}
		if err != nil {
			closeCachePathHandles(roots, directories)
			return nil, nil, nil, nil, fmt.Errorf("inspect configured cache path component: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			closeCachePathHandles(roots, directories)
			return nil, nil, nil, nil, errors.New("configured cache path must not contain symlink components")
		}
		if !info.IsDir() {
			closeCachePathHandles(roots, directories)
			return nil, nil, nil, nil, errors.New("configured cache path components must be directories")
		}
		childDirectory, err := openDirectoryAt(parentDirectory, name)
		if err != nil {
			closeCachePathHandles(roots, directories)
			return nil, nil, nil, nil, fmt.Errorf("open configured cache path component without following symlinks: %w", err)
		}
		childRoot, err := parentRoot.OpenRoot(name)
		if err != nil {
			_ = childDirectory.Close()
			closeCachePathHandles(roots, directories)
			return nil, nil, nil, nil, fmt.Errorf("open rooted configured cache path component: %w", err)
		}
		fdInfo, fdErr := childDirectory.Stat()
		rootInfo, rootErr := childRoot.Stat(".")
		if fdErr != nil || rootErr != nil || !os.SameFile(info, fdInfo) || !os.SameFile(fdInfo, rootInfo) {
			_ = childDirectory.Close()
			_ = childRoot.Close()
			closeCachePathHandles(roots, directories)
			return nil, nil, nil, nil, errors.New("configured cache path component changed while opening rooted handles")
		}
		isCacheRoot := index == len(parts)-1
		if isCacheRoot {
			if err := validateDirectoryHandle("configured cache root", childDirectory, false); err != nil {
				_ = childDirectory.Close()
				_ = childRoot.Close()
				closeCachePathHandles(roots, directories)
				return nil, nil, nil, nil, err
			}
		} else if err := validateCacheAncestorHandle("cache path ancestor", childDirectory); err != nil {
			_ = childDirectory.Close()
			_ = childRoot.Close()
			closeCachePathHandles(roots, directories)
			return nil, nil, nil, nil, err
		}
		roots = append(roots, childRoot)
		directories = append(directories, childDirectory)
		names = append(names, name)
	}
	if len(names) == 0 {
		closeCachePathHandles(roots, directories)
		return nil, nil, nil, nil, errors.New("filesystem root cannot be used as the configured cache root")
	}
	rootInfo, err := roots[len(roots)-1].Stat(".")
	if err != nil {
		closeCachePathHandles(roots, directories)
		return nil, nil, nil, nil, fmt.Errorf("inspect configured cache root descriptor: %w", err)
	}
	return roots, directories, names, rootInfo, nil
}

func closeCachePathHandles(roots []*os.Root, directories []*os.File) {
	for index := len(directories) - 1; index >= 0; index-- {
		_ = directories[index].Close()
	}
	for index := len(roots) - 1; index >= 0; index-- {
		_ = roots[index].Close()
	}
}

func (cache *Cache) openStagingRoot() error {
	root, directory, info, err := cache.openStagingPath(true)
	if err != nil {
		return fmt.Errorf("open configured cache staging root: %w", err)
	}
	cache.stagingRoot, cache.stagingFD, cache.stagingInfo = root, directory, info
	return nil
}

func (cache *Cache) openStagingPath(create bool) (*os.Root, *os.File, os.FileInfo, error) {
	parts := strings.Split(filepath.ToSlash(cache.stagingRelative), "/")
	parentRoot := cache.root
	parentDirectory := cache.rootFD
	var ownedRoot *os.Root
	var ownedDirectory *os.File
	closeOwned := func() {
		if ownedDirectory != nil {
			_ = ownedDirectory.Close()
		}
		if ownedRoot != nil {
			_ = ownedRoot.Close()
		}
	}
	for _, name := range parts {
		if name == "" || name == "." || name == ".." {
			closeOwned()
			return nil, nil, nil, errors.New("configured staging root contains an unsafe path component")
		}
		info, err := parentRoot.Lstat(name)
		if errors.Is(err, os.ErrNotExist) && create {
			if mkdirErr := parentRoot.Mkdir(name, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
				closeOwned()
				return nil, nil, nil, fmt.Errorf("create staging directory component: %w", mkdirErr)
			}
			info, err = parentRoot.Lstat(name)
		}
		if err != nil {
			closeOwned()
			return nil, nil, nil, fmt.Errorf("inspect staging directory component: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			closeOwned()
			return nil, nil, nil, errors.New("configured staging path components must be real directories")
		}
		directory, err := openDirectoryAt(parentDirectory, name)
		if err != nil {
			closeOwned()
			return nil, nil, nil, fmt.Errorf("open staging directory component without following symlinks: %w", err)
		}
		root, err := parentRoot.OpenRoot(name)
		if err != nil {
			_ = directory.Close()
			closeOwned()
			return nil, nil, nil, fmt.Errorf("open rooted staging directory component: %w", err)
		}
		fdInfo, fdErr := directory.Stat()
		rootInfo, rootErr := root.Stat(".")
		if fdErr != nil || rootErr != nil || !os.SameFile(info, fdInfo) || !os.SameFile(fdInfo, rootInfo) {
			_ = directory.Close()
			_ = root.Close()
			closeOwned()
			return nil, nil, nil, errors.New("configured staging path changed while opening rooted handles")
		}
		if err := validateDirectoryHandle("cache staging path component", directory, true); err != nil {
			_ = directory.Close()
			_ = root.Close()
			closeOwned()
			return nil, nil, nil, err
		}
		closeOwned()
		ownedRoot, ownedDirectory = root, directory
		parentRoot, parentDirectory = root, directory
	}
	if ownedRoot == nil || ownedDirectory == nil {
		return nil, nil, nil, errors.New("configured staging root must identify a child directory")
	}
	finalInfo, err := ownedRoot.Stat(".")
	if err != nil {
		closeOwned()
		return nil, nil, nil, fmt.Errorf("inspect configured staging root: %w", err)
	}
	return ownedRoot, ownedDirectory, finalInfo, nil
}

func (cache *Cache) openAssetsRoot() error {
	if err := cache.root.Mkdir("assets", 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create cache Asset root: %w", err)
	}
	info, err := cache.root.Lstat("assets")
	if err != nil {
		return fmt.Errorf("inspect cache Asset root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("cache Asset root must be a real directory")
	}
	cache.assetsRoot, err = cache.root.OpenRoot("assets")
	if err != nil {
		return fmt.Errorf("open cache Asset root: %w", err)
	}
	cache.assetsInfo = info
	cache.assetsFD, err = openDirectoryAt(cache.rootFD, "assets")
	if err != nil {
		return fmt.Errorf("open cache Asset directory handle: %w", err)
	}
	rootInfo, err := cache.assetsRoot.Stat(".")
	fdInfo, fdErr := cache.assetsFD.Stat()
	if err != nil || fdErr != nil || !os.SameFile(rootInfo, fdInfo) || !os.SameFile(info, fdInfo) {
		return errors.New("cache Asset root changed while opening rooted handles")
	}
	if err := validateDirectoryHandle("cache Asset root", cache.assetsFD, false); err != nil {
		return err
	}
	return nil
}

func (cache *Cache) openQuarantineRoot() error {
	if err := cache.root.Mkdir(".quarantine", 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create Agent cache quarantine root: %w", err)
	}
	info, err := cache.root.Lstat(".quarantine")
	if err != nil {
		return fmt.Errorf("inspect Agent cache quarantine root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("Agent cache quarantine root must be a real directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("Agent cache quarantine root must not be accessible by group or other users")
	}
	cache.quarantineRoot, err = cache.root.OpenRoot(".quarantine")
	if err != nil {
		return fmt.Errorf("open Agent cache quarantine root: %w", err)
	}
	cache.quarantineInfo = info
	cache.quarantineFD, err = openDirectoryAt(cache.rootFD, ".quarantine")
	if err != nil {
		return fmt.Errorf("open Agent cache quarantine directory handle: %w", err)
	}
	rootInfo, err := cache.quarantineRoot.Stat(".")
	fdInfo, fdErr := cache.quarantineFD.Stat()
	if err != nil || fdErr != nil || !os.SameFile(rootInfo, fdInfo) || !os.SameFile(info, fdInfo) {
		return errors.New("Agent cache quarantine root changed while opening rooted handles")
	}
	if err := validateDirectoryHandle("Agent cache quarantine root", cache.quarantineFD, true); err != nil {
		return err
	}
	return nil
}

// ValidateRestorePaths accepts only opaque path-safe IDs and a single safe
// filename. It rejects both Unix and Windows path separators on every host.
func ValidateRestorePaths(assetID domain.AssetID, jobID string, filename string) error {
	if !safeIdentifier.MatchString(string(assetID)) {
		return errors.New("Asset ID must be a path-safe identifier")
	}
	return validateJobFilename(jobID, filename)
}

func validateJobFilename(jobID string, filename string) error {
	if !safeIdentifier.MatchString(jobID) {
		return errors.New("Job ID must be a path-safe identifier")
	}
	if filename == "" || !utf8.ValidString(filename) || filename == "." || filename == ".." ||
		strings.TrimSpace(filename) == "" || filepath.IsAbs(filename) || filepath.VolumeName(filename) != "" ||
		strings.ContainsAny(filename, `/\:`) {
		return errors.New("Asset filename must be one safe path component")
	}
	for _, character := range filename {
		if character == 0 || character < 0x20 || character == 0x7f {
			return errors.New("Asset filename must not contain control characters")
		}
	}
	return nil
}

func validateConfiguredRoot(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("configured cache root must be an absolute path")
	}
	for _, segment := range strings.Split(filepath.ToSlash(path), "/") {
		if segment == ".." {
			return errors.New("configured cache root must not contain path traversal")
		}
	}
	return nil
}

// FinalPath returns the display/diagnostic path derived from the validated
// Asset ID and safe filename. Filesystem mutations use Cache's rooted handle.
func (cache *Cache) FinalPath(assetID domain.AssetID, filename string) (string, error) {
	if !safeIdentifier.MatchString(string(assetID)) {
		return "", errors.New("Asset ID must be a path-safe identifier")
	}
	if err := validateJobFilename("job-validation", filename); err != nil {
		return "", err
	}
	relative := filepath.Join("assets", string(assetID), filename)
	path := filepath.Join(cache.path, relative)
	resolved, err := filepath.Rel(cache.path, path)
	if err != nil || resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) || filepath.IsAbs(resolved) {
		return "", errors.New("derived Asset cache path escapes configured root")
	}
	return path, nil
}

func (cache *Cache) ensureRootStillSame() error {
	return cache.ensureAncestorChain()
}

func (cache *Cache) ensureAncestorChain() error {
	if len(cache.ancestorRoots) < 2 || len(cache.ancestorFDs) != len(cache.ancestorRoots) ||
		len(cache.ancestorNames)+1 != len(cache.ancestorRoots) {
		return errors.New("configured cache ancestor handles are incomplete")
	}
	for index, directory := range cache.ancestorFDs {
		if index == len(cache.ancestorFDs)-1 {
			if err := validateDirectoryHandle("configured cache root", directory, false); err != nil {
				return err
			}
		} else if err := validateCacheAncestorHandle("cache path ancestor", directory); err != nil {
			return err
		}
	}
	for index, name := range cache.ancestorNames {
		current, err := cache.ancestorRoots[index].Lstat(name)
		if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() {
			return errors.New("configured cache ancestor path changed after opening rooted handles")
		}
		descriptorInfo, err := cache.ancestorFDs[index+1].Stat()
		if err != nil || !os.SameFile(descriptorInfo, current) {
			return errors.New("configured cache ancestor path changed after opening rooted handles")
		}
		if index == len(cache.ancestorNames)-1 && !os.SameFile(cache.rootInfo, descriptorInfo) {
			return errors.New("configured cache root changed after opening rooted handles")
		}
	}
	return nil
}

func (cache *Cache) ensureStagingRoot() error {
	if err := cache.ensureRootStillSame(); err != nil {
		return err
	}
	currentRoot, currentDirectory, currentInfo, err := cache.openStagingPath(false)
	if err != nil {
		return errors.New("cache staging root changed after opening rooted handles")
	}
	_ = currentDirectory.Close()
	_ = currentRoot.Close()
	if !os.SameFile(cache.stagingInfo, currentInfo) {
		return errors.New("cache staging root changed after opening rooted handles")
	}
	return validateDirectoryHandle("cache staging root", cache.stagingFD, true)
}

func (cache *Cache) ensureAssetsRoot() error {
	if err := cache.ensureRootStillSame(); err != nil {
		return err
	}
	current, err := cache.root.Lstat("assets")
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(cache.assetsInfo, current) {
		return errors.New("cache Asset root changed after opening rooted handles")
	}
	return validateDirectoryHandle("cache Asset root", cache.assetsFD, false)
}

func (cache *Cache) ensureQuarantineRoot() error {
	if err := cache.ensureRootStillSame(); err != nil {
		return err
	}
	current, err := cache.root.Lstat(".quarantine")
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(cache.quarantineInfo, current) {
		return errors.New("Agent cache quarantine root changed after opening rooted handles")
	}
	return validateDirectoryHandle("Agent cache quarantine root", cache.quarantineFD, true)
}

// Preflight opens and retains the final parent directory, verifies that
// staging and final paths share a filesystem, checks free space against the
// exact expected bytes, and rejects an existing target before transfer.
func (cache *Cache) Preflight(assetID domain.AssetID, filename string, expectedBytes int64) (*RestorePlan, error) {
	if expectedBytes <= 0 {
		return nil, ErrExpectedSizeRequired
	}
	if _, err := cache.FinalPath(assetID, filename); err != nil {
		return nil, err
	}
	if err := cache.ensureStagingRoot(); err != nil {
		return nil, err
	}
	parent, err := cache.openFinalDirectory(assetID, true)
	if err != nil {
		return nil, err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = parent.Close()
		}
	}()
	if err := cache.ensureAssetsRoot(); err != nil {
		return nil, err
	}
	if _, err := parent.root.Lstat(filename); err == nil {
		return nil, ErrDestinationExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect final cache destination: %w", err)
	}
	if !sameDevice(cache.stagingFD, parent.file) {
		return nil, ErrDifferentFilesystem
	}
	available, err := cache.availableBytes(parent.file)
	if err != nil {
		return nil, fmt.Errorf("check available cache space: %w", err)
	}
	if available < uint64(expectedBytes) {
		return nil, fmt.Errorf("%w: available %d bytes, expected %d", ErrInsufficientSpace, available, expectedBytes)
	}
	closeOnError = false
	return &RestorePlan{cache: cache, assetID: assetID, filename: filename, expectedBytes: expectedBytes, parent: parent}, nil
}

// RestorePlan binds one validated Asset filename and expected size to the
// opened final parent used for preflight. It exposes no caller-chosen path.
type RestorePlan struct {
	cache         *Cache
	assetID       domain.AssetID
	filename      string
	expectedBytes int64
	parent        *finalDirectory
	closeOnce     sync.Once
	closeErr      error
}

// Close releases the retained destination parent handle.
func (plan *RestorePlan) Close() error {
	plan.closeOnce.Do(func() {
		if plan.parent != nil {
			plan.closeErr = plan.parent.Close()
		}
	})
	return plan.closeErr
}

type finalDirectory struct {
	root *os.Root
	file *os.File
	info os.FileInfo
}

func (directory *finalDirectory) Close() error {
	var closeErr error
	if directory.file != nil {
		closeErr = directory.file.Close()
	}
	if directory.root != nil {
		if err := directory.root.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}

func (cache *Cache) openFinalDirectory(assetID domain.AssetID, create bool) (*finalDirectory, error) {
	if !safeIdentifier.MatchString(string(assetID)) {
		return nil, errors.New("Asset ID must be a path-safe identifier")
	}
	if err := cache.ensureAssetsRoot(); err != nil {
		return nil, err
	}
	if create {
		if err := cache.assetsRoot.Mkdir(string(assetID), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create final Asset cache directory: %w", err)
		}
	}
	info, err := cache.assetsRoot.Lstat(string(assetID))
	if err != nil {
		return nil, fmt.Errorf("inspect final Asset cache directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("final Asset cache parent must be a real directory")
	}
	root, err := cache.assetsRoot.OpenRoot(string(assetID))
	if err != nil {
		return nil, fmt.Errorf("open final Asset cache directory: %w", err)
	}
	file, err := openDirectoryAt(cache.assetsFD, string(assetID))
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("open final Asset cache directory handle: %w", err)
	}
	fdInfo, err := file.Stat()
	rootInfo, rootErr := root.Stat(".")
	if err != nil || rootErr != nil || !os.SameFile(info, fdInfo) || !os.SameFile(fdInfo, rootInfo) {
		_ = file.Close()
		_ = root.Close()
		return nil, errors.New("final Asset cache parent changed while opening rooted handles")
	}
	if err := validateDirectoryHandle("final Asset cache parent", file, false); err != nil {
		_ = file.Close()
		_ = root.Close()
		return nil, err
	}
	return &finalDirectory{root: root, file: file, info: info}, nil
}

func (cache *Cache) quarantineWrongSizeReadyFile(sessionID string, assetID domain.AssetID, filename string, expectedBytes int64) (moved bool, existed bool, returnErr error) {
	if !safeIdentifier.MatchString(string(assetID)) {
		return false, false, errors.New("Asset ID must be a path-safe identifier")
	}
	if !safeIdentifier.MatchString(sessionID) {
		return false, false, errors.New("launch Session ID must be a path-safe identifier")
	}
	if err := validateJobFilename("quarantine", filename); err != nil {
		return false, false, err
	}
	if expectedBytes <= 0 {
		return false, false, ErrExpectedSizeRequired
	}
	if _, err := cache.FinalPath(assetID, filename); err != nil {
		return false, false, err
	}
	if err := cache.ensureQuarantineRoot(); err != nil {
		return false, false, err
	}
	parent, err := cache.openFinalDirectory(assetID, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	defer func() { returnErr = errors.Join(returnErr, parent.Close()) }()
	if err := cache.ensureFinalDirectoryStillSame(&RestorePlan{cache: cache, assetID: assetID, parent: parent}); err != nil {
		return false, false, err
	}
	info, err := parent.root.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("inspect exact final cache entry before quarantine: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, true, errors.New("only a regular final cache file can be quarantined")
	}
	if info.Size() == expectedBytes {
		return false, true, nil
	}
	file, err := openRegularFileAt(parent.file, filename)
	if err != nil {
		return false, true, fmt.Errorf("open exact final cache entry without following symlinks: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() == expectedBytes || !os.SameFile(info, opened) {
		return false, true, errors.New("final cache entry changed before stale-size quarantine")
	}
	if err := cache.ensureFinalDirectoryStillSame(&RestorePlan{cache: cache, assetID: assetID, parent: parent}); err != nil {
		return false, true, err
	}
	current, err := parent.root.Lstat(filename)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return false, true, errors.New("exact final cache entry changed before quarantine rename")
	}

	sessionDirectory, err := cache.createOwnedQuarantineDirectory(sessionID)
	if err != nil {
		return false, true, err
	}
	defer func() { returnErr = errors.Join(returnErr, sessionDirectory.Close()) }()
	assetDirectory, err := cache.createOwnedQuarantineChild(sessionDirectory, string(assetID))
	if err != nil {
		return false, true, err
	}
	defer func() { returnErr = errors.Join(returnErr, assetDirectory.Close()) }()
	if !sameDevice(parent.file, assetDirectory.file) {
		return false, true, ErrDifferentFilesystem
	}
	if err := cache.ensureAssetsRoot(); err != nil {
		return false, true, err
	}
	if err := cache.ensureQuarantineRoot(); err != nil {
		return false, true, err
	}
	if err := cache.ensureOwnedQuarantineDirectoryStillSame(&finalDirectory{root: cache.quarantineRoot, file: cache.quarantineFD}, sessionID, sessionDirectory); err != nil {
		return false, true, err
	}
	if err := cache.ensureOwnedQuarantineDirectoryStillSame(sessionDirectory, string(assetID), assetDirectory); err != nil {
		return false, true, err
	}
	if err := cache.ensureFinalDirectoryStillSame(&RestorePlan{cache: cache, assetID: assetID, parent: parent}); err != nil {
		return false, true, err
	}
	current, err = parent.root.Lstat(filename)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return false, true, errors.New("exact final cache entry changed before no-replace quarantine")
	}
	if err := unix.Renameat2(int(parent.file.Fd()), filename, int(assetDirectory.file.Fd()), filename, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return false, true, ErrDestinationExists
		}
		return false, true, fmt.Errorf("move stale final cache entry into Agent quarantine without replacement: %w", err)
	}
	if err := unix.Fsync(int(parent.file.Fd())); err != nil {
		return false, true, fmt.Errorf("sync final Asset directory after quarantine: %w", err)
	}
	if err := unix.Fsync(int(assetDirectory.file.Fd())); err != nil {
		return false, true, fmt.Errorf("sync Agent quarantine directory after move: %w", err)
	}
	quarantined, err := assetDirectory.root.Lstat(filename)
	if err != nil || !quarantined.Mode().IsRegular() || quarantined.Size() == expectedBytes || !os.SameFile(opened, quarantined) {
		return false, true, errors.New("quarantined final cache entry failed identity and size verification")
	}
	if err := cache.ensureFinalDirectoryStillSame(&RestorePlan{cache: cache, assetID: assetID, parent: parent}); err != nil {
		return false, true, err
	}
	if err := cache.ensureQuarantineRoot(); err != nil {
		return false, true, err
	}
	if err := cache.ensureOwnedQuarantineDirectoryStillSame(&finalDirectory{root: cache.quarantineRoot, file: cache.quarantineFD}, sessionID, sessionDirectory); err != nil {
		return false, true, err
	}
	if err := cache.ensureOwnedQuarantineDirectoryStillSame(sessionDirectory, string(assetID), assetDirectory); err != nil {
		return false, true, err
	}
	return true, true, nil
}

func (cache *Cache) createOwnedQuarantineDirectory(sessionID string) (*finalDirectory, error) {
	return cache.createOwnedQuarantineChild(&finalDirectory{root: cache.quarantineRoot, file: cache.quarantineFD}, sessionID)
}

func (cache *Cache) createOwnedQuarantineChild(parent *finalDirectory, name string) (*finalDirectory, error) {
	if parent == nil || parent.root == nil || parent.file == nil || !safeIdentifier.MatchString(name) {
		return nil, errors.New("Agent quarantine directory requires a validated parent and identifier")
	}
	if err := validateDirectoryHandle("Agent-owned quarantine parent", parent.file, true); err != nil {
		return nil, err
	}
	if err := parent.root.Mkdir(name, 0o700); err != nil {
		return nil, fmt.Errorf("create unique Agent quarantine directory: %w", err)
	}
	info, err := parent.root.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("new Agent quarantine directory is not a private real directory")
	}
	root, err := parent.root.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("open unique Agent quarantine directory: %w", err)
	}
	file, err := openDirectoryAt(parent.file, name)
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("open unique Agent quarantine directory handle: %w", err)
	}
	fdInfo, err := file.Stat()
	rootInfo, rootErr := root.Stat(".")
	if err != nil || rootErr != nil || !os.SameFile(info, fdInfo) || !os.SameFile(fdInfo, rootInfo) {
		_ = file.Close()
		_ = root.Close()
		return nil, errors.New("Agent quarantine directory changed while opening rooted handles")
	}
	if err := validateDirectoryHandle("Agent-owned quarantine directory", file, true); err != nil {
		_ = file.Close()
		_ = root.Close()
		return nil, err
	}
	return &finalDirectory{root: root, file: file, info: info}, nil
}

func (cache *Cache) ensureOwnedQuarantineDirectoryStillSame(parent *finalDirectory, name string, child *finalDirectory) error {
	if parent == nil || parent.root == nil || child == nil || child.file == nil || child.info == nil {
		return errors.New("Agent quarantine directory identity is incomplete")
	}
	if err := validateDirectoryHandle("Agent-owned quarantine parent", parent.file, true); err != nil {
		return err
	}
	current, err := parent.root.Lstat(name)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(child.info, current) {
		return errors.New("Agent quarantine directory changed after opening rooted handles")
	}
	fdInfo, err := child.file.Stat()
	if err != nil || !os.SameFile(child.info, fdInfo) {
		return errors.New("Agent quarantine directory descriptor changed after opening rooted handles")
	}
	return validateDirectoryHandle("Agent-owned quarantine directory", child.file, true)
}

func openRegularFileAt(parent *os.File, filename string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), filename, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filename), nil
}

// Promote verifies and flushes the staged regular file, then atomically moves
// it into the preflight-bound final directory with Linux RENAME_NOREPLACE.
func (cache *Cache) Promote(staged *StagingFile, plan *RestorePlan) (string, error) {
	if staged == nil || plan == nil || staged.cache != cache || plan.cache != cache || plan.parent == nil {
		return "", errors.New("promotion requires staging and preflight handles from the same cache")
	}
	if staged.filename != plan.filename {
		return "", errors.New("staged filename does not match preflight target")
	}
	if plan.expectedBytes <= 0 {
		return "", ErrExpectedSizeRequired
	}
	if staged.File == nil || staged.fileClosed || staged.jobDir == nil {
		return "", errors.New("staged Asset file is not open for promotion")
	}
	if err := cache.ensureStagingRoot(); err != nil {
		return "", err
	}
	if err := cache.ensureFinalDirectoryStillSame(plan); err != nil {
		return "", err
	}
	if err := validateDirectoryHandle("owned Job staging directory", staged.jobDir, true); err != nil {
		return "", err
	}
	jobInfo, err := cache.stagingRoot.Lstat(staged.jobID)
	if err != nil || !os.SameFile(staged.jobInfo, jobInfo) {
		return "", errors.New("owned Job staging directory changed before promotion")
	}
	fileInfo, err := staged.File.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect staged Asset file: %w", err)
	}
	if !fileInfo.Mode().IsRegular() {
		return "", errors.New("staged Asset must be a regular file")
	}
	if fileInfo.Size() != plan.expectedBytes {
		return "", fmt.Errorf("%w: got %d, expected %d", ErrSizeMismatch, fileInfo.Size(), plan.expectedBytes)
	}
	stagedInfo, err := staged.jobRoot.Lstat(staged.filename)
	if err != nil || !os.SameFile(fileInfo, stagedInfo) {
		return "", errors.New("staged Asset path changed before promotion")
	}
	if err := staged.File.Sync(); err != nil {
		return "", fmt.Errorf("flush staged Asset file: %w", err)
	}
	if err := staged.File.Close(); err != nil {
		staged.fileClosed = true
		return "", fmt.Errorf("close staged Asset file before promotion: %w", err)
	}
	staged.fileClosed = true
	if !sameDevice(staged.jobDir, plan.parent.file) {
		return "", ErrDifferentFilesystem
	}
	if err := cache.ensureFinalDirectoryStillSame(plan); err != nil {
		return "", err
	}
	if err := unix.Renameat2(int(staged.jobDir.Fd()), staged.filename, int(plan.parent.file.Fd()), plan.filename, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return "", ErrDestinationExists
		}
		if errors.Is(err, unix.EXDEV) {
			return "", ErrDifferentFilesystem
		}
		return "", fmt.Errorf("atomically promote staged Asset without replacement: %w", err)
	}
	if err := unix.Fsync(int(plan.parent.file.Fd())); err != nil {
		cache.removePromotedFileIfOwned(plan, fileInfo)
		return "", fmt.Errorf("sync final cache directory after promotion: %w", err)
	}
	finalInfo, err := plan.parent.root.Lstat(plan.filename)
	if err != nil || !finalInfo.Mode().IsRegular() || finalInfo.Size() != plan.expectedBytes || !os.SameFile(fileInfo, finalInfo) {
		cache.removePromotedFileIfOwned(plan, fileInfo)
		return "", errors.New("promoted Asset failed final regular-file verification")
	}
	if err := cache.ensureFinalDirectoryStillSame(plan); err != nil {
		cache.removePromotedFileIfOwned(plan, fileInfo)
		return "", err
	}
	return filepath.Join(cache.path, "assets", string(plan.assetID), plan.filename), nil
}

func (cache *Cache) ensureFinalDirectoryStillSame(plan *RestorePlan) error {
	if err := cache.ensureAssetsRoot(); err != nil {
		return err
	}
	if err := validateDirectoryHandle("final Asset cache parent", plan.parent.file, false); err != nil {
		return err
	}
	current, err := cache.assetsRoot.Lstat(string(plan.assetID))
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(plan.parent.info, current) {
		return errors.New("final Asset cache parent changed during restore")
	}
	return nil
}

func (cache *Cache) removePromotedFileIfOwned(plan *RestorePlan, promoted os.FileInfo) {
	current, err := plan.parent.root.Lstat(plan.filename)
	if err == nil && os.SameFile(promoted, current) {
		_ = plan.parent.root.Remove(plan.filename)
	}
}

func openDirectoryAt(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func validateDirectoryHandle(name string, directory *os.File, private bool) error {
	if directory == nil {
		return fmt.Errorf("%s has no opened directory handle", name)
	}
	info, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened %s metadata: %w", name, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect opened %s owner metadata: unsupported filesystem metadata", name)
	}
	return validateDirectoryMetadata(name, stat.Uid, info.Mode(), private)
}

func validateDirectoryMetadata(name string, ownerUID uint32, mode os.FileMode, private bool) error {
	if !mode.IsDir() {
		return fmt.Errorf("%s must be a directory", name)
	}
	if ownerUID != uint32(os.Geteuid()) {
		return fmt.Errorf("%s must be owned by the Agent effective user", name)
	}
	unsafePermissions := os.FileMode(0o022)
	if private {
		unsafePermissions = 0o077
	}
	if mode.Perm()&unsafePermissions != 0 {
		if private {
			return fmt.Errorf("%s must not be accessible by group or other users", name)
		}
		return fmt.Errorf("%s must not be writable by group or other users", name)
	}
	return nil
}

func validateCacheAncestorHandle(name string, directory *os.File) error {
	if directory == nil {
		return fmt.Errorf("%s has no opened directory handle", name)
	}
	info, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened %s metadata: %w", name, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect opened %s owner metadata: unsupported filesystem metadata", name)
	}
	return validateCacheAncestorMetadata(name, stat.Uid, info.Mode())
}

// validateCacheAncestorMetadata trusts only ancestors owned by the Agent or
// root. Group/other-writable ancestors are rejected except root-owned sticky
// directories, where another UID cannot replace an entry owned by the Agent.
func validateCacheAncestorMetadata(name string, ownerUID uint32, mode os.FileMode) error {
	if !mode.IsDir() {
		return fmt.Errorf("%s must be a directory", name)
	}
	agentUID := uint32(os.Geteuid())
	if ownerUID != agentUID && ownerUID != 0 {
		return fmt.Errorf("%s must be owned by the Agent effective user or root", name)
	}
	if mode.Perm()&0o022 != 0 && !(ownerUID == 0 && mode&os.ModeSticky != 0) {
		return fmt.Errorf("%s must not be writable by group or other users unless it is root-owned and sticky", name)
	}
	return nil
}

func sameDevice(first *os.File, second *os.File) bool {
	firstInfo, firstErr := first.Stat()
	secondInfo, secondErr := second.Stat()
	if firstErr != nil || secondErr != nil {
		return false
	}
	firstStat, firstOK := firstInfo.Sys().(*syscall.Stat_t)
	secondStat, secondOK := secondInfo.Sys().(*syscall.Stat_t)
	return firstOK && secondOK && firstStat.Dev == secondStat.Dev
}

func availableSpace(directory *os.File) (uint64, error) {
	var stats unix.Statfs_t
	if err := unix.Fstatfs(int(directory.Fd()), &stats); err != nil {
		return 0, err
	}
	if stats.Bsize <= 0 {
		return 0, errors.New("filesystem reported an invalid block size")
	}
	blockSize := uint64(stats.Bsize)
	if stats.Bavail > ^uint64(0)/blockSize {
		return ^uint64(0), nil
	}
	return stats.Bavail * blockSize, nil
}

// CreateStaging creates a private, exclusive per-Job staging subtree and one
// new file inside it. Existing Job subtrees are never reused or cleaned.
func (cache *Cache) CreateStaging(jobID string, filename string) (*StagingFile, error) {
	if err := validateJobFilename(jobID, filename); err != nil {
		return nil, err
	}
	if err := cache.ensureStagingRoot(); err != nil {
		return nil, err
	}
	if err := cache.stagingRoot.Mkdir(jobID, 0o700); err != nil {
		return nil, fmt.Errorf("create owned Job staging directory: %w", err)
	}
	jobInfo, err := cache.stagingRoot.Lstat(jobID)
	if err != nil {
		_ = cache.stagingRoot.RemoveAll(jobID)
		return nil, fmt.Errorf("inspect owned Job staging directory: %w", err)
	}
	if jobInfo.Mode()&os.ModeSymlink != 0 || !jobInfo.IsDir() || jobInfo.Mode().Perm()&0o077 != 0 {
		_ = cache.stagingRoot.RemoveAll(jobID)
		return nil, errors.New("owned Job staging directory is not private and regular")
	}
	jobRoot, err := cache.stagingRoot.OpenRoot(jobID)
	if err != nil {
		_ = cache.removeOwnedJobDirectory(jobID, jobInfo)
		return nil, fmt.Errorf("open owned Job staging directory: %w", err)
	}
	jobDir, err := openDirectoryAt(cache.stagingFD, jobID)
	if err != nil {
		_ = jobRoot.Close()
		_ = cache.removeOwnedJobDirectory(jobID, jobInfo)
		return nil, fmt.Errorf("open owned Job staging directory handle: %w", err)
	}
	jobFDInfo, err := jobDir.Stat()
	jobRootInfo, rootErr := jobRoot.Stat(".")
	if err != nil || rootErr != nil || !os.SameFile(jobInfo, jobFDInfo) || !os.SameFile(jobFDInfo, jobRootInfo) {
		_ = jobDir.Close()
		_ = jobRoot.Close()
		_ = cache.removeOwnedJobDirectory(jobID, jobInfo)
		return nil, errors.New("owned Job staging directory changed while opening rooted handles")
	}
	if err := validateDirectoryHandle("owned Job staging directory", jobDir, true); err != nil {
		_ = jobDir.Close()
		_ = jobRoot.Close()
		_ = cache.removeOwnedJobDirectory(jobID, jobInfo)
		return nil, err
	}
	file, err := jobRoot.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = jobDir.Close()
		_ = jobRoot.Close()
		_ = cache.removeOwnedJobDirectory(jobID, jobInfo)
		return nil, fmt.Errorf("create staged Asset file: %w", err)
	}
	return &StagingFile{File: file, cache: cache, jobRoot: jobRoot, jobDir: jobDir, jobID: jobID, jobInfo: jobInfo, filename: filename}, nil
}

// StagingFile holds the only writable handle exposed for a per-Job staged
// file. Cleanup can remove only the directory this call created.
type StagingFile struct {
	File       *os.File
	cache      *Cache
	jobRoot    *os.Root
	jobDir     *os.File
	jobID      string
	jobInfo    os.FileInfo
	filename   string
	fileClosed bool
	cleanOnce  sync.Once
	cleanErr   error
}

// Cleanup closes the staging file and removes only its owned Job subtree.
func (staged *StagingFile) Cleanup() error {
	staged.cleanOnce.Do(func() {
		if staged.File != nil && !staged.fileClosed {
			if err := staged.File.Close(); err != nil {
				staged.cleanErr = fmt.Errorf("close staged Asset file: %w", err)
			}
			staged.fileClosed = true
		}
		if staged.jobDir != nil {
			if err := staged.jobDir.Close(); err != nil && staged.cleanErr == nil {
				staged.cleanErr = fmt.Errorf("close owned Job staging directory handle: %w", err)
			}
		}
		if staged.jobRoot != nil {
			if err := staged.jobRoot.Close(); err != nil && staged.cleanErr == nil {
				staged.cleanErr = fmt.Errorf("close owned Job staging root: %w", err)
			}
		}
		if err := staged.cache.removeOwnedJobDirectory(staged.jobID, staged.jobInfo); err != nil && staged.cleanErr == nil {
			staged.cleanErr = err
		}
	})
	return staged.cleanErr
}

func (cache *Cache) removeOwnedJobDirectory(jobID string, owned os.FileInfo) error {
	current, err := cache.stagingRoot.Lstat(jobID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect owned Job staging directory before cleanup: %w", err)
	}
	if !os.SameFile(owned, current) {
		return errors.New("owned Job staging directory changed before cleanup")
	}
	if err := cache.stagingRoot.RemoveAll(jobID); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove owned Job staging directory: %w", err)
	}
	return nil
}

// Close releases the rooted cache handles.
func (cache *Cache) Close() error {
	cache.closeOnce.Do(func() {
		cache.closeFile(cache.assetsFD, "cache Asset directory handle")
		cache.closeFile(cache.quarantineFD, "Agent cache quarantine directory handle")
		cache.closeFile(cache.stagingFD, "cache staging directory handle")
		cache.closeRoot(cache.assetsRoot, "cache Asset root")
		cache.closeRoot(cache.quarantineRoot, "Agent cache quarantine root")
		cache.closeRoot(cache.stagingRoot, "cache staging root")
		for index := len(cache.ancestorFDs) - 1; index >= 0; index-- {
			cache.closeFile(cache.ancestorFDs[index], "cache ancestor directory handle")
		}
		for index := len(cache.ancestorRoots) - 1; index >= 0; index-- {
			cache.closeRoot(cache.ancestorRoots[index], "cache ancestor root")
		}
	})
	return cache.closeErr
}

func (cache *Cache) closeFile(file *os.File, name string) {
	if file == nil {
		return
	}
	if err := file.Close(); err != nil && cache.closeErr == nil {
		cache.closeErr = fmt.Errorf("close %s: %w", name, err)
	}
}

func (cache *Cache) closeRoot(root *os.Root, name string) {
	if root == nil {
		return
	}
	if err := root.Close(); err != nil && cache.closeErr == nil {
		cache.closeErr = fmt.Errorf("close %s: %w", name, err)
	}
}
