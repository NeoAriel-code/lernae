package agentops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"lernae/internal/agent"
)

const (
	copyBufferBytes      = 64 * 1024
	progressByteInterval = 1 * 1024 * 1024
	progressTimeInterval = 200 * time.Millisecond
)

// LocalFixtureExecutor is a test-injected executor that reads only safe
// relative descriptors beneath its explicitly supplied source root. Production
// Agent startup does not configure this executor.
type LocalFixtureExecutor struct {
	cache      *Cache
	sourceRoot *os.Root
}

// NewLocalFixtureExecutor opens a real directory as the controlled source
// fixture root. It does not discover or use any ambient media directory.
func NewLocalFixtureExecutor(sourcePath string, cache *Cache) (*LocalFixtureExecutor, error) {
	if cache == nil {
		return nil, errors.New("local fixture executor requires a configured Agent cache")
	}
	if sourcePath == "" || !filepath.IsAbs(sourcePath) {
		return nil, errors.New("fixture source root must be an absolute path")
	}
	info, err := os.Lstat(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("inspect fixture source root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("fixture source root must be a real directory")
	}
	root, err := os.OpenRoot(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("open fixture source root: %w", err)
	}
	rootInfo, err := root.Stat(".")
	current, currentErr := os.Lstat(sourcePath)
	if err != nil || currentErr != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, rootInfo) || !os.SameFile(rootInfo, current) {
		_ = root.Close()
		return nil, errors.New("fixture source root changed while opening rooted handle")
	}
	return &LocalFixtureExecutor{cache: cache, sourceRoot: root}, nil
}

// Close releases the explicitly injected fixture root.
func (executor *LocalFixtureExecutor) Close() error {
	if executor.sourceRoot == nil {
		return nil
	}
	return executor.sourceRoot.Close()
}

// Restore executes one one-file ROM fixture and returns local-ready evidence
// only after exact-size verification and no-replace atomic promotion.
func (executor *LocalFixtureExecutor) Restore(ctx context.Context, request agent.RestoreAsset, report func(agent.RestoreProgress) error) (result agent.RestoreResult, returnErr error) {
	return restoreThroughCache(ctx, executor.cache, request, report, func() (restoreTransfer, func() error, error) {
		if request.SourceLocation.StorageProviderID != agent.RestoreSourceProviderFixture ||
			request.SourceLocation.LocationClass != agent.RestoreSourceClassFixture {
			return nil, nil, errors.New("local fixture executor requires an injected fixture source")
		}
		source, sourceInfo, err := executor.openFixture(request.SourceLocation.Locator)
		if err != nil {
			return nil, nil, err
		}
		if sourceInfo.Size() != request.Asset.TotalSizeBytes {
			_ = source.Close()
			return nil, nil, fmt.Errorf("%w: fixture source reports %d bytes, expected %d", ErrSizeMismatch, sourceInfo.Size(), request.Asset.TotalSizeBytes)
		}
		transfer := func(ctx context.Context, staged *StagingFile, emitter *progressEmitter) error {
			_, err := copyContext(ctx, staged.File, source, request.Asset.TotalSizeBytes, emitter)
			return err
		}
		return transfer, source.Close, nil
	})
}

type restoreTransfer func(context.Context, *StagingFile, *progressEmitter) error

func restoreThroughCache(
	ctx context.Context,
	cache *Cache,
	request agent.RestoreAsset,
	report func(agent.RestoreProgress) error,
	prepare func() (restoreTransfer, func() error, error),
) (result agent.RestoreResult, returnErr error) {
	if err := request.Validate(); err != nil {
		return agent.RestoreResult{}, err
	}
	if ctx == nil {
		return agent.RestoreResult{}, errors.New("restore requires a context")
	}
	if err := ctx.Err(); err != nil {
		return agent.RestoreResult{}, err
	}
	emitter := progressEmitter{report: report}
	if err := emitter.emit(agent.RestorePhaseValidating, 0, request.Asset.TotalSizeBytes, true); err != nil {
		return agent.RestoreResult{}, err
	}
	transfer, closeSource, err := prepare()
	if err != nil {
		return agent.RestoreResult{}, err
	}
	if closeSource != nil {
		defer func() {
			if err := closeSource(); err != nil && returnErr == nil {
				returnErr = fmt.Errorf("close restore source: %w", err)
			}
		}()
	}
	if err := ctx.Err(); err != nil {
		return agent.RestoreResult{}, err
	}
	plan, err := cache.Preflight(request.Asset.ID, request.Parts[0].Filename, request.Asset.TotalSizeBytes)
	if err != nil {
		return agent.RestoreResult{}, err
	}
	defer func() {
		if err := plan.Close(); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("close restore destination plan: %w", err)
		}
	}()
	if err := emitter.emit(agent.RestorePhaseStaging, 0, request.Asset.TotalSizeBytes, true); err != nil {
		return agent.RestoreResult{}, err
	}
	staged, err := cache.CreateStaging(request.JobID, request.Parts[0].Filename)
	if err != nil {
		return agent.RestoreResult{}, err
	}
	defer func() {
		if err := staged.Cleanup(); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("clean owned Job staging directory: %w", err)
		}
	}()
	if err := ctx.Err(); err != nil {
		return agent.RestoreResult{}, err
	}
	if err := emitter.emit(agent.RestorePhaseCopying, 0, request.Asset.TotalSizeBytes, true); err != nil {
		return agent.RestoreResult{}, err
	}
	if err := transfer(ctx, staged, &emitter); err != nil {
		return agent.RestoreResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return agent.RestoreResult{}, err
	}
	if err := emitter.emit(agent.RestorePhaseVerifying, request.Asset.TotalSizeBytes, request.Asset.TotalSizeBytes, true); err != nil {
		return agent.RestoreResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return agent.RestoreResult{}, err
	}
	if err := emitter.emit(agent.RestorePhasePromoting, request.Asset.TotalSizeBytes, request.Asset.TotalSizeBytes, true); err != nil {
		return agent.RestoreResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return agent.RestoreResult{}, err
	}
	if _, err := cache.Promote(staged, plan); err != nil {
		return agent.RestoreResult{}, err
	}
	if err := staged.Cleanup(); err != nil {
		return agent.RestoreResult{}, fmt.Errorf("clean promoted Job staging directory: %w", err)
	}
	result = agent.RestoreResult{
		AssetID:           request.Asset.ID,
		LocationClass:     "local_cache",
		RelativePath:      path.Join("assets", string(request.Asset.ID), request.Parts[0].Filename),
		VerifiedSizeBytes: request.Asset.TotalSizeBytes,
		VerifiedAt:        time.Now().UTC(),
		LocalReady:        true,
	}
	if err := emitter.emit(agent.RestorePhaseComplete, request.Asset.TotalSizeBytes, request.Asset.TotalSizeBytes, true); err != nil {
		return agent.RestoreResult{}, err
	}
	return result, nil
}

func (executor *LocalFixtureExecutor) openFixture(descriptor string) (*os.File, os.FileInfo, error) {
	if !safeFixturePath(descriptor) {
		return nil, nil, errors.New("fixture source descriptor is not a safe relative path")
	}
	parts := strings.Split(descriptor, "/")
	current := ""
	var expectedInfo os.FileInfo
	for index, component := range parts {
		if current == "" {
			current = component
		} else {
			current = path.Join(current, component)
		}
		info, err := executor.sourceRoot.Lstat(current)
		if err != nil {
			return nil, nil, fmt.Errorf("inspect fixture source descriptor: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, errors.New("fixture source path must not contain symlinks")
		}
		if index < len(parts)-1 && !info.IsDir() {
			return nil, nil, errors.New("fixture source parent must be a directory")
		}
		if index == len(parts)-1 {
			if !info.Mode().IsRegular() {
				return nil, nil, errors.New("fixture source must be a regular file")
			}
			expectedInfo = info
		}
	}
	file, err := executor.sourceRoot.Open(descriptor)
	if err != nil {
		return nil, nil, fmt.Errorf("open fixture source file: %w", err)
	}
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(expectedInfo, openedInfo) || !openedInfo.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, errors.New("fixture source changed while opening rooted file")
	}
	return file, openedInfo, nil
}

func safeFixturePath(descriptor string) bool {
	if descriptor == "" || len(descriptor) > 4096 || strings.TrimSpace(descriptor) != descriptor || strings.ContainsAny(descriptor, `\\:`) || path.IsAbs(descriptor) || path.Clean(descriptor) != descriptor {
		return false
	}
	for _, component := range strings.Split(descriptor, "/") {
		if component == "" || len(component) > 255 || component == "." || component == ".." || !utf8.ValidString(component) {
			return false
		}
		for _, character := range component {
			if character == 0 || character < 0x20 || character == 0x7f {
				return false
			}
		}
	}
	return true
}

type progressEmitter struct {
	report    func(agent.RestoreProgress) error
	previous  *agent.RestoreProgress
	lastAt    time.Time
	lastBytes int64
}

func (emitter *progressEmitter) emit(phase agent.RestorePhase, current, total int64, force bool) error {
	// Reserve the exact total for successful terminal progress, including tiny transfers.
	if phase != agent.RestorePhaseComplete && current == total {
		current--
	}
	event := agent.RestoreProgress{Phase: phase, CurrentBytes: current, TotalBytes: total}
	if err := event.Validate(); err != nil {
		return err
	}
	if err := agent.ValidateProgressAfter(emitter.previous, event); err != nil {
		return err
	}
	now := time.Now()
	if !force && emitter.previous != nil && phase == emitter.previous.Phase && current != total &&
		current-emitter.lastBytes < progressByteInterval && now.Sub(emitter.lastAt) < progressTimeInterval {
		return nil
	}
	if emitter.report != nil {
		if err := emitter.report(event); err != nil {
			return err
		}
	}
	copy := event
	emitter.previous = &copy
	emitter.lastAt = now
	emitter.lastBytes = current
	return nil
}

func copyContext(ctx context.Context, destination *os.File, source *os.File, expectedBytes int64, emitter *progressEmitter) (int64, error) {
	buffer := make([]byte, copyBufferBytes)
	var current int64
	noProgress := 0
	for {
		if err := ctx.Err(); err != nil {
			return current, err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			noProgress = 0
			if int64(read) > expectedBytes-current {
				return current, fmt.Errorf("%w: fixture source exceeded %d bytes", ErrSizeMismatch, expectedBytes)
			}
			written, writeErr := destination.Write(buffer[:read])
			if writeErr != nil {
				return current, fmt.Errorf("write staged Asset file: %w", writeErr)
			}
			if written != read {
				return current, io.ErrShortWrite
			}
			current += int64(written)
			if err := emitter.emit(agent.RestorePhaseCopying, current, expectedBytes, false); err != nil {
				return current, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			if current != expectedBytes {
				return current, fmt.Errorf("%w: fixture source ended after %d bytes, expected %d", ErrSizeMismatch, current, expectedBytes)
			}
			return current, nil
		}
		if readErr != nil {
			return current, fmt.Errorf("read fixture source file: %w", readErr)
		}
		if read == 0 {
			noProgress++
			if noProgress >= 100 {
				return current, io.ErrNoProgress
			}
		}
	}
}
