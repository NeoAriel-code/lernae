package agentops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"lernae/internal/domain"
)

const (
	dolphinExecutableName = "dolphin-emu"
	dolphinBatchFlag      = "-b"
	dolphinExecFlag       = "-e"
)

var (
	ErrUnsupportedGameCubeImage = errors.New("Dolphin supports only one GameCube disc-image ROM")
	ErrLocalReadyImageInvalid   = errors.New("local-ready GameCube image is unavailable or invalid")
	ErrDolphinExecutableMissing = errors.New("Dolphin executable is unavailable")
	ErrDolphinExecutableUnsafe  = errors.New("Dolphin executable is not trusted")
	ErrDolphinStartFailed       = errors.New("Dolphin process could not be started")
	ErrDolphinProcessFailed     = errors.New("Dolphin process failed")
	ErrDolphinNonZeroExit       = errors.New("Dolphin exited unsuccessfully")
)

// ReadyGameCubeImage is an opaque cache-issued capability for one validated
// local GameCube disc image. Its pathname and opened file are intentionally
// private so callers cannot turn a request-supplied path or readiness boolean
// into launch authority.
//
// Dolphin consumes the derived pathname rather than this open descriptor.
// The file and rooted cache identity are re-opened and checked immediately
// before process start, but a same-user or privileged actor can still replace
// the pathname after that check and before Dolphin opens it (residual TOCTOU).
type ReadyGameCubeImage struct {
	cache         *Cache
	assetID       domain.AssetID
	filename      string
	path          string
	expectedBytes int64
	parentInfo    os.FileInfo
	fileInfo      os.FileInfo
	file          *os.File
	closed        bool
	mu            sync.Mutex
}

// OpenReadyGameCubeImage derives and opens the only supported local launch
// image from trusted domain identity and an exact one-ROM AssetPart. It never
// accepts a cache path or trusts RestoreResult.LocalReady.
func (cache *Cache) OpenReadyGameCubeImage(
	work domain.Work,
	edition domain.Edition,
	asset domain.Asset,
	parts []domain.AssetPart,
) (*ReadyGameCubeImage, error) {
	if cache == nil || !supportedGameCubeShape(work, edition, asset, parts) {
		return nil, ErrUnsupportedGameCubeImage
	}
	part := parts[0]
	path, err := cache.FinalPath(asset.ID, part.Filename)
	if err != nil {
		return nil, ErrUnsupportedGameCubeImage
	}
	parent, err := cache.openFinalDirectory(asset.ID, false)
	if err != nil {
		return nil, ErrLocalReadyImageInvalid
	}
	defer func() { _ = parent.Close() }()

	if err := cache.ensureFinalDirectoryStillSame(&RestorePlan{cache: cache, assetID: asset.ID, parent: parent}); err != nil {
		return nil, ErrLocalReadyImageInvalid
	}
	file, fileInfo, err := openVerifiedReadyFile(parent, part.Filename, asset.TotalSizeBytes)
	if err != nil {
		return nil, ErrLocalReadyImageInvalid
	}
	if err := cache.ensureFinalDirectoryStillSame(&RestorePlan{cache: cache, assetID: asset.ID, parent: parent}); err != nil {
		_ = file.Close()
		return nil, ErrLocalReadyImageInvalid
	}
	return &ReadyGameCubeImage{
		cache: cache, assetID: asset.ID, filename: part.Filename, path: path,
		expectedBytes: asset.TotalSizeBytes, parentInfo: parent.info, fileInfo: fileInfo, file: file,
	}, nil
}

// OpenReadyGameCubeImageForSession applies the bounded stale-cache recovery rule
// for one validated Agent launch. Only a wrong-size regular file at the exact
// Asset/Part-derived final path can be moved; the launch SessionID is used
// solely as private attempt-scoped quarantine correlation, never as a path.
func (cache *Cache) OpenReadyGameCubeImageForSession(
	sessionID string,
	work domain.Work,
	edition domain.Edition,
	asset domain.Asset,
	parts []domain.AssetPart,
) (*ReadyGameCubeImage, error) {
	image, err := cache.OpenReadyGameCubeImage(work, edition, asset, parts)
	if !errors.Is(err, ErrLocalReadyImageInvalid) {
		return image, err
	}
	if cache == nil || !supportedGameCubeShape(work, edition, asset, parts) {
		return nil, err
	}
	moved, existed, quarantineErr := cache.quarantineWrongSizeReadyFile(sessionID, asset.ID, parts[0].Filename, asset.TotalSizeBytes)
	if quarantineErr != nil {
		return nil, fmt.Errorf("fail-closed stale local-cache quarantine: %w", quarantineErr)
	}
	if existed && !moved {
		return nil, errors.New("existing local cache entry was not proven safe to quarantine")
	}
	return nil, ErrLocalReadyImageInvalid
}

func supportedGameCubeShape(work domain.Work, edition domain.Edition, asset domain.Asset, parts []domain.AssetPart) bool {
	if work.ID == "" || work.Medium != domain.MediumGame || edition.ID == "" || edition.WorkID != work.ID ||
		edition.Platform != "gamecube" || edition.Format != "disc_image" || asset.ID == "" ||
		asset.EditionID != edition.ID || asset.Kind != "disc_image" || asset.TotalSizeBytes <= 0 || len(parts) != 1 {
		return false
	}
	part := parts[0]
	return part.ID != "" && part.AssetID == asset.ID && part.Role == "rom" && part.RelativePath == "" &&
		part.SizeBytes > 0 && part.SizeBytes == asset.TotalSizeBytes
}

func openVerifiedReadyFile(parent *finalDirectory, filename string, expectedBytes int64) (*os.File, os.FileInfo, error) {
	if parent == nil || parent.root == nil || expectedBytes <= 0 {
		return nil, nil, ErrLocalReadyImageInvalid
	}
	info, err := parent.root.Lstat(filename)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != expectedBytes {
		return nil, nil, ErrLocalReadyImageInvalid
	}
	file, err := parent.root.Open(filename)
	if err != nil {
		return nil, nil, ErrLocalReadyImageInvalid
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() != expectedBytes || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, nil, ErrLocalReadyImageInvalid
	}
	return file, opened, nil
}

// Close releases the verified image descriptor. It is safe to call repeatedly.
func (image *ReadyGameCubeImage) Close() error {
	if image == nil {
		return nil
	}
	image.mu.Lock()
	defer image.mu.Unlock()
	if image.closed {
		return nil
	}
	image.closed = true
	if image.file == nil {
		return nil
	}
	err := image.file.Close()
	image.file = nil
	return err
}

func (image *ReadyGameCubeImage) revalidateForLaunch() (string, error) {
	if image == nil {
		return "", ErrLocalReadyImageInvalid
	}
	image.mu.Lock()
	defer image.mu.Unlock()
	if image.closed || image.cache == nil || image.file == nil || image.expectedBytes <= 0 || image.path == "" {
		return "", ErrLocalReadyImageInvalid
	}
	path, err := image.cache.FinalPath(image.assetID, image.filename)
	if err != nil || path != image.path {
		return "", ErrLocalReadyImageInvalid
	}
	parent, err := image.cache.openFinalDirectory(image.assetID, false)
	if err != nil {
		return "", ErrLocalReadyImageInvalid
	}
	defer func() { _ = parent.Close() }()
	if !os.SameFile(image.parentInfo, parent.info) {
		return "", ErrLocalReadyImageInvalid
	}
	if err := image.cache.ensureFinalDirectoryStillSame(&RestorePlan{cache: image.cache, assetID: image.assetID, parent: parent}); err != nil {
		return "", ErrLocalReadyImageInvalid
	}
	file, info, err := openVerifiedReadyFile(parent, image.filename, image.expectedBytes)
	if err != nil || !os.SameFile(image.fileInfo, info) {
		if file != nil {
			_ = file.Close()
		}
		return "", ErrLocalReadyImageInvalid
	}
	if err := image.cache.ensureFinalDirectoryStillSame(&RestorePlan{cache: image.cache, assetID: image.assetID, parent: parent}); err != nil {
		_ = file.Close()
		return "", ErrLocalReadyImageInvalid
	}
	oldFile := image.file
	image.file = file
	image.fileInfo = info
	if err := oldFile.Close(); err != nil {
		_ = file.Close()
		image.file = nil
		image.closed = true
		return "", ErrLocalReadyImageInvalid
	}
	return path, nil
}

// DolphinRunner starts only the installed, allowlisted dolphin-emu command.
// It has no request-controlled executable, arguments, cwd, or environment.
type DolphinRunner struct {
	lookPath func(string) (string, error)
	start    func(context.Context, string, ...string) (*exec.Cmd, error)
}

// NewDolphinRunner is deliberately lazy: it does not resolve Dolphin during
// Agent construction or status requests. Resolution occurs only on Start.
func NewDolphinRunner() *DolphinRunner {
	return &DolphinRunner{lookPath: exec.LookPath, start: startDolphinCommand}
}

// Start revalidates the cache-issued image immediately before starting Dolphin
// with the fixed -b -e <pathname> argv. Stdout and stderr are discarded.
func (runner *DolphinRunner) Start(ctx context.Context, image *ReadyGameCubeImage) (*DolphinProcess, error) {
	if runner == nil || ctx == nil {
		return nil, ErrDolphinStartFailed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if runner.lookPath == nil || runner.start == nil {
		return nil, ErrDolphinExecutableUnsafe
	}
	executable, err := resolveDolphinExecutable(runner.lookPath)
	if err != nil {
		return nil, err
	}
	romPath, err := image.revalidateForLaunch()
	if err != nil {
		return nil, err
	}
	command, err := runner.start(ctx, executable, dolphinBatchFlag, dolphinExecFlag, romPath)
	if err != nil || command == nil || command.Process == nil {
		return nil, ErrDolphinStartFailed
	}
	return &DolphinProcess{ctx: ctx, command: command}, nil
}

func resolveDolphinExecutable(lookPath func(string) (string, error)) (string, error) {
	if lookPath == nil {
		return "", ErrDolphinExecutableMissing
	}
	resolved, err := lookPath(dolphinExecutableName)
	if err != nil {
		return "", ErrDolphinExecutableMissing
	}
	if !filepath.IsAbs(resolved) || filepath.Base(resolved) != dolphinExecutableName {
		return "", ErrDolphinExecutableUnsafe
	}
	canonical, err := filepath.EvalSymlinks(resolved)
	if err != nil || !filepath.IsAbs(canonical) || filepath.Clean(canonical) != canonical || filepath.Base(canonical) != dolphinExecutableName {
		return "", ErrDolphinExecutableUnsafe
	}
	if err := validateDolphinExecutableAncestors(canonical); err != nil {
		return "", ErrDolphinExecutableUnsafe
	}
	info, err := os.Lstat(canonical)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return "", ErrDolphinExecutableUnsafe
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != uint32(os.Geteuid()) && stat.Uid != 0) {
		return "", ErrDolphinExecutableUnsafe
	}
	return canonical, nil
}

// validateDolphinExecutableAncestors requires every canonical parent entry to
// be a real directory owned by root or the Agent and not writable by group or
// other users. Owner-writable Agent directories are trusted in this local-user
// model; without descriptor-based execveat, the same UID can still replace a
// file after these checks and before process start.
func validateDolphinExecutableAncestors(executable string) error {
	if !filepath.IsAbs(executable) {
		return ErrDolphinExecutableUnsafe
	}
	for directory := filepath.Dir(executable); ; directory = filepath.Dir(directory) {
		info, err := os.Lstat(directory)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
			return ErrDolphinExecutableUnsafe
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != uint32(os.Geteuid()) && stat.Uid != 0) {
			return ErrDolphinExecutableUnsafe
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
	}
	return nil
}

func startDolphinCommand(ctx context.Context, executable string, args ...string) (*exec.Cmd, error) {
	command := exec.CommandContext(ctx, executable, args...)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return nil, err
	}
	return command, nil
}

// DolphinProcess exposes only waiting on the child owned by DolphinRunner.
// Context cancellation stops the child; Wait must be called to reap it.
type DolphinProcess struct {
	ctx      context.Context
	command  *exec.Cmd
	waitOnce sync.Once
	waitErr  error
}

// Wait reaps the owned child once and returns bounded exit information only.
func (process *DolphinProcess) Wait() error {
	if process == nil || process.command == nil {
		return ErrDolphinProcessFailed
	}
	process.waitOnce.Do(func() {
		err := process.command.Wait()
		state := process.command.ProcessState
		processExited := state != nil && state.Exited()
		exitCode := -1
		if state != nil {
			exitCode = state.ExitCode()
		}
		var contextErr error
		if process.ctx != nil {
			contextErr = process.ctx.Err()
		}
		process.waitErr = classifyDolphinWaitResult(contextErr, err, processExited, exitCode)
	})
	return process.waitErr
}

func classifyDolphinWaitResult(contextErr, waitErr error, processExited bool, exitCode int) error {
	if waitErr == nil {
		return nil
	}
	if processExited {
		if exitCode == 0 {
			return nil
		}
		if exitCode > 0 {
			return &DolphinExitError{ExitCode: exitCode}
		}
	}
	if contextErr != nil {
		return contextErr
	}
	return ErrDolphinProcessFailed
}

// DolphinExitError contains only the child exit code, never arbitrary process output.
type DolphinExitError struct {
	ExitCode int
}

func (err *DolphinExitError) Error() string {
	if err == nil {
		return ErrDolphinNonZeroExit.Error()
	}
	return fmt.Sprintf("Dolphin exited with status %d", err.ExitCode)
}

func (err *DolphinExitError) Unwrap() error { return ErrDolphinNonZeroExit }
