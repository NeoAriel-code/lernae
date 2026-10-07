package agentops

import (
	"context"
	"errors"
	"os/exec"

	"lernae/internal/agent"
)

const (
	RcloneFailureStatFailed         RcloneFailureCategory = "stat_failed"
	RcloneFailureStatRequestInvalid RcloneFailureCategory = "stat_request_invalid"
)

// RcloneRestoreExecutor restores one exact archive AssetLocation through the
// same AgentOps preflight, staging, verification and promotion pipeline as the
// injected local fixture executor.
type RcloneRestoreExecutor struct {
	cache      *Cache
	copyRunner RcloneCopyRunner
}

// NewRcloneRestoreExecutor does not resolve or start rclone. An unavailable
// executable therefore fails only a requested archive restore, not Agent
// startup or unrelated status operations.
func NewRcloneRestoreExecutor(cache *Cache) (*RcloneRestoreExecutor, error) {
	if cache == nil {
		return nil, errors.New("rclone restore executor requires an Agent cache")
	}
	return &RcloneRestoreExecutor{cache: cache}, nil
}

func (executor *RcloneRestoreExecutor) Restore(
	ctx context.Context,
	request agent.RestoreAsset,
	report func(agent.RestoreProgress) error,
) (agent.RestoreResult, error) {
	if executor == nil || executor.cache == nil {
		return agent.RestoreResult{}, errors.New("rclone restore executor is not configured")
	}
	return restoreThroughCache(ctx, executor.cache, request, report, func() (restoreTransfer, func() error, error) {
		location := request.SourceLocation
		if location.StorageProviderID != agent.RestoreSourceProviderRclone || location.LocationClass != agent.RestoreSourceClassArchive {
			return nil, nil, rcloneFailure(RcloneFailureInvalidLocator)
		}
		source, err := ParseRcloneSourceLocator(location.Locator)
		if err != nil {
			return nil, nil, err
		}
		if _, err := runRcloneStat(ctx, source, request.Parts[0].SizeBytes); err != nil {
			return nil, nil, err
		}
		transfer := func(ctx context.Context, staged *StagingFile, emitter *progressEmitter) error {
			return executor.copyRunner.Copy(ctx, source, staged, request.Parts[0].SizeBytes, func(event agent.RestoreProgress) error {
				return emitter.emit(event.Phase, event.CurrentBytes, event.TotalBytes, false)
			})
		}
		return transfer, nil, nil
	})
}

func runRcloneStat(ctx context.Context, source RcloneSourceDescriptor, expectedPartSize int64) (RcloneObjectStat, error) {
	if ctx == nil {
		return RcloneObjectStat{}, rcloneFailure(RcloneFailureStatRequestInvalid)
	}
	if expectedPartSize <= 0 {
		return RcloneObjectStat{}, rcloneFailure(RcloneFailureExpectedSizeInvalid)
	}
	if err := ctx.Err(); err != nil {
		return RcloneObjectStat{}, err
	}
	args, err := BuildRcloneStatArgs(source)
	if err != nil {
		return RcloneObjectStat{}, err
	}
	executable, err := ResolveRcloneExecutable()
	if err != nil {
		return RcloneObjectStat{}, err
	}
	command := exec.CommandContext(ctx, executable, args...)
	var stdout, stderr boundedRcloneOutput
	stdout.limit = maxRcloneStatOutputBytes
	stderr.limit = maxRcloneProcessOutputBytes
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return RcloneObjectStat{}, ctx.Err()
		}
		return RcloneObjectStat{}, rcloneFailure(RcloneFailureStatFailed)
	}
	if stdout.Overflowed() {
		return RcloneObjectStat{}, rcloneFailure(RcloneFailureInvalidStat)
	}
	return ParseRcloneStatJSON(stdout.Bytes(), expectedPartSize)
}
