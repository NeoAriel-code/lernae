package agentops

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"lernae/internal/agent"
)

const (
	maxRcloneProcessOutputBytes = 8 * 1024
	rcloneCopySubcommand        = "copyto"
	rcloneCopyInPlaceFlag       = "--inplace"
	rcloneCopyPollInterval      = 100 * time.Millisecond
)

const (
	RcloneFailureCopyFailed          RcloneFailureCategory = "copy_failed"
	RcloneFailureCopyProgressInvalid RcloneFailureCategory = "copy_progress_invalid"
	RcloneFailureCopyRequestInvalid  RcloneFailureCategory = "copy_request_invalid"
	RcloneFailureCopyStageInvalid    RcloneFailureCategory = "copy_stage_invalid"
)

// RcloneCopyRunner executes only one read-only-source to local-staging
// copyto. It does not verify or publish the staged file; the Agent restore
// pipeline owns those barriers.
type RcloneCopyRunner struct {
	pollInterval time.Duration
}

// Copy writes one parsed remote object to a file selected by Cache.CreateStaging.
// Progress is monotonic and remains below the expected total until the caller's
// P1-05A verification and atomic promotion have succeeded.
func (runner RcloneCopyRunner) Copy(
	ctx context.Context,
	source RcloneSourceDescriptor,
	staged *StagingFile,
	expectedBytes int64,
	report func(agent.RestoreProgress) error,
) error {
	if ctx == nil {
		return rcloneFailure(RcloneFailureCopyRequestInvalid)
	}
	if expectedBytes <= 0 {
		return rcloneFailure(RcloneFailureExpectedSizeInvalid)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	args, err := buildRcloneCopyArgs(source, staged)
	if err != nil {
		return err
	}
	executable, err := ResolveRcloneExecutable()
	if err != nil {
		return err
	}

	progress := newRcloneCopyProgress(report, expectedBytes)

	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(commandCtx, executable, args...)
	command.Stdout = io.Discard
	var stderr boundedRcloneOutput
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return rcloneFailure(RcloneFailureCopyFailed)
	}
	finished := make(chan error, 1)
	go func() {
		finished <- command.Wait()
	}()
	stopAndWait := func(cause error) error {
		cancel()
		<-finished
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return cause
	}

	interval := runner.pollInterval
	if interval <= 0 {
		interval = rcloneCopyPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case waitErr := <-finished:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if waitErr != nil {
				return rcloneFailure(RcloneFailureCopyFailed)
			}
			current, err := stagedRcloneCopySize(staged)
			if err != nil {
				return err
			}
			if current < progress.lastObserved || current > expectedBytes {
				return rcloneFailure(RcloneFailureCopyProgressInvalid)
			}
			if err := progress.emit(current, true); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			return nil
		case <-ticker.C:
			current, err := stagedRcloneCopySize(staged)
			if err != nil {
				return stopAndWait(err)
			}
			if current < progress.lastObserved || current > expectedBytes {
				return stopAndWait(rcloneFailure(RcloneFailureCopyProgressInvalid))
			}
			if err := progress.emit(current, false); err != nil {
				return stopAndWait(err)
			}
		case <-ctx.Done():
			cancel()
			<-finished
			return ctx.Err()
		}
	}
}

func buildRcloneCopyArgs(source RcloneSourceDescriptor, staged *StagingFile) ([]string, error) {
	if !validRcloneRemoteName(source.remoteName) || !validRcloneObjectPath(source.objectPath) {
		return nil, rcloneFailure(RcloneFailureInvalidLocator)
	}
	destination, err := stagedRcloneCopyDestination(staged)
	if err != nil {
		return nil, err
	}
	// Preserve the Cache-created staging inode: rclone's default local
	// copyto path writes a temporary file and atomically renames it over the
	// destination, which would invalidate P1-05A's open-file identity checks.
	// In-place writes remain confined to the disposable, Agent-owned staging
	// file; exact size verification and no-replace promotion still gate readiness.
	return []string{rcloneCopySubcommand, rcloneCopyInPlaceFlag, source.Locator(), destination}, nil
}

func stagedRcloneCopyDestination(staged *StagingFile) (string, error) {
	current, err := inspectRcloneStagingFile(staged)
	if err != nil || current != 0 {
		return "", rcloneFailure(RcloneFailureCopyStageInvalid)
	}
	return filepath.Join(staged.cache.stagingPath, staged.jobID, staged.filename), nil
}

func stagedRcloneCopySize(staged *StagingFile) (int64, error) {
	size, err := inspectRcloneStagingFile(staged)
	if err != nil {
		return 0, err
	}
	if size < 0 {
		return 0, rcloneFailure(RcloneFailureCopyProgressInvalid)
	}
	return size, nil
}

func inspectRcloneStagingFile(staged *StagingFile) (int64, error) {
	if staged == nil || staged.cache == nil || staged.File == nil || staged.fileClosed ||
		staged.jobRoot == nil || staged.jobDir == nil || staged.jobInfo == nil {
		return 0, rcloneFailure(RcloneFailureCopyStageInvalid)
	}
	if err := validateJobFilename(staged.jobID, staged.filename); err != nil {
		return 0, rcloneFailure(RcloneFailureCopyStageInvalid)
	}
	if err := staged.cache.ensureStagingRoot(); err != nil {
		return 0, rcloneFailure(RcloneFailureCopyStageInvalid)
	}
	currentJob, err := staged.cache.stagingRoot.Lstat(staged.jobID)
	if err != nil || !os.SameFile(staged.jobInfo, currentJob) {
		return 0, rcloneFailure(RcloneFailureCopyStageInvalid)
	}
	fileInfo, err := staged.File.Stat()
	if err != nil || !fileInfo.Mode().IsRegular() {
		return 0, rcloneFailure(RcloneFailureCopyStageInvalid)
	}
	pathInfo, err := staged.jobRoot.Lstat(staged.filename)
	if err != nil || !pathInfo.Mode().IsRegular() || !os.SameFile(fileInfo, pathInfo) {
		return 0, rcloneFailure(RcloneFailureCopyStageInvalid)
	}
	return fileInfo.Size(), nil
}

type rcloneCopyProgress struct {
	report       func(agent.RestoreProgress) error
	totalBytes   int64
	lastObserved int64
	lastReported int64
	lastAt       time.Time
	previous     *agent.RestoreProgress
}

func newRcloneCopyProgress(report func(agent.RestoreProgress) error, totalBytes int64) *rcloneCopyProgress {
	return &rcloneCopyProgress{report: report, totalBytes: totalBytes, lastAt: time.Now()}
}

func (progress *rcloneCopyProgress) emit(currentBytes int64, force bool) error {
	if currentBytes < progress.lastObserved || currentBytes < 0 || currentBytes > progress.totalBytes {
		return rcloneFailure(RcloneFailureCopyProgressInvalid)
	}
	progress.lastObserved = currentBytes
	if progress.report == nil || currentBytes == 0 {
		return nil
	}
	visibleBytes := currentBytes
	if visibleBytes >= progress.totalBytes {
		visibleBytes = progress.totalBytes - 1
	}
	if visibleBytes <= progress.lastReported {
		return nil
	}
	now := time.Now()
	if !force && visibleBytes-progress.lastReported < progressByteInterval && now.Sub(progress.lastAt) < progressTimeInterval {
		return nil
	}
	event := agent.RestoreProgress{
		Phase:        agent.RestorePhaseCopying,
		CurrentBytes: visibleBytes,
		TotalBytes:   progress.totalBytes,
	}
	if err := agent.ValidateProgressAfter(progress.previous, event); err != nil {
		return rcloneFailure(RcloneFailureCopyProgressInvalid)
	}
	if err := progress.report(event); err != nil {
		return err
	}
	copy := event
	progress.previous = &copy
	progress.lastReported = visibleBytes
	progress.lastAt = now
	return nil
}

type boundedRcloneOutput struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (output *boundedRcloneOutput) Write(data []byte) (int, error) {
	limit := output.limit
	if limit <= 0 {
		limit = maxRcloneProcessOutputBytes
	}
	remaining := limit - output.buffer.Len()
	if remaining > 0 {
		if len(data) < remaining {
			remaining = len(data)
		}
		_, _ = output.buffer.Write(data[:remaining])
	}
	if len(data) > remaining {
		output.overflow = true
	}
	return len(data), nil
}

func (output *boundedRcloneOutput) Len() int {
	return output.buffer.Len()
}

func (output *boundedRcloneOutput) Bytes() []byte {
	return output.buffer.Bytes()
}

func (output *boundedRcloneOutput) Overflowed() bool {
	return output.overflow
}
