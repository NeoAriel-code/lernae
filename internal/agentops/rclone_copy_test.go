package agentops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"lernae/internal/agent"
)

const (
	rcloneTestModeEnv      = "LERNAE_TEST_RCLONE_MODE"
	rcloneTestArgsFileEnv  = "LERNAE_TEST_RCLONE_ARGS_FILE"
	rcloneTestSizeEnv      = "LERNAE_TEST_RCLONE_SIZE"
	rcloneTestPIDFileEnv   = "LERNAE_TEST_RCLONE_PID_FILE"
	rcloneTestPrivateToken = "private-rclone-output-token"
)

// TestMain turns the current Go test binary into a deterministic fake rclone
// or Dolphin executable when invoked through its test-only command name.
func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case "rclone":
		os.Exit(runRcloneTestHelper(os.Args[1:]))
	case "dolphin-emu":
		os.Exit(runDolphinTestHelper(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func TestRcloneCopyRunnerUsesFixedCopytoArgsAndAgentOwnedStaging(t *testing.T) {
	const expectedBytes = int64(2 << 20)
	cache, staged, destination := newRcloneTestStaging(t, "copy-success", "game.iso")
	argsFile, _ := installRcloneTestHelper(t, "success", expectedBytes)
	source, err := ParseRcloneSourceLocator("archive:roms/--config=not-a-flag; echo marker.iso")
	if err != nil {
		t.Fatalf("ParseRcloneSourceLocator() error = %v", err)
	}

	runner := RcloneCopyRunner{pollInterval: 10 * time.Millisecond}
	progress := make([]agent.RestoreProgress, 0)
	err = runner.Copy(context.Background(), source, staged, expectedBytes, func(event agent.RestoreProgress) error {
		progress = append(progress, event)
		return nil
	})
	if err != nil {
		t.Fatalf("RcloneCopyRunner.Copy() error = %v", err)
	}

	args := readRcloneTestArgs(t, argsFile)
	wantArgs := []string{"copyto", "--inplace", source.Locator(), destination}
	if !equalStrings(args, wantArgs) {
		t.Fatalf("copy argv = %#v, want exactly %#v", args, wantArgs)
	}
	stagedInfo, err := staged.File.Stat()
	pathInfo, pathErr := os.Lstat(destination)
	if err != nil || pathErr != nil || !os.SameFile(stagedInfo, pathInfo) {
		t.Fatalf("in-place copy changed the Agent-owned staging inode: fd=%v path=%v errors=(%v, %v)", stagedInfo, pathInfo, err, pathErr)
	}
	if len(progress) == 0 {
		t.Fatal("copy runner returned without reporting byte progress")
	}
	var previous *agent.RestoreProgress
	for _, event := range progress {
		if event.Phase != agent.RestorePhaseCopying || event.TotalBytes != expectedBytes || event.CurrentBytes >= expectedBytes {
			t.Fatalf("copy progress reached an invalid state before P1-05A verification: %#v", event)
		}
		if err := agent.ValidateProgressAfter(previous, event); err != nil {
			t.Fatalf("copy progress is not monotonic and bounded: %v; events=%#v", err, progress)
		}
		copy := event
		previous = &copy
	}
	if progress[len(progress)-1].CurrentBytes <= 0 {
		t.Fatalf("copy progress did not observe transferred bytes: %#v", progress)
	}

	stagedInfo, err = staged.File.Stat()
	if err != nil || stagedInfo.Size() != expectedBytes {
		t.Fatalf("staged file size = %v, error = %v, want %d", sizeOrZero(stagedInfo), err, expectedBytes)
	}
	finalPath := filepath.Join(cache.path, "assets", "asset-1", "game.iso")
	if _, err := os.Lstat(finalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("copy runner created a final cache file before promotion: %v", err)
	}
}

func TestRcloneCopyRunnerSanitizesFailureAndBoundsCapturedOutput(t *testing.T) {
	const expectedBytes = int64(1024)
	_, staged, _ := newRcloneTestStaging(t, "copy-fail", "game.iso")
	_, _ = installRcloneTestHelper(t, "fail", expectedBytes)
	source, err := ParseRcloneSourceLocator("archive:private/rom.iso")
	if err != nil {
		t.Fatal(err)
	}

	err = (RcloneCopyRunner{}).Copy(context.Background(), source, staged, expectedBytes, nil)
	assertRcloneCategory(t, err, RcloneFailureCopyFailed)
	if strings.Contains(err.Error(), rcloneTestPrivateToken) || len(err.Error()) > maxRcloneErrorMessageBytes {
		t.Fatalf("copy error exposed child output or exceeded its bound: %q", err)
	}
}

func TestRcloneCopyRunnerCancellationWaitsForChildReap(t *testing.T) {
	const expectedBytes = int64(4 << 20)
	cache, staged, _ := newRcloneTestStaging(t, "copy-cancel", "game.iso")
	_, pidFile := installRcloneTestHelper(t, "wait", expectedBytes)
	source, err := ParseRcloneSourceLocator("archive:roms/game.iso")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []agent.RestoreProgress
	err = (RcloneCopyRunner{pollInterval: 10 * time.Millisecond}).Copy(ctx, source, staged, expectedBytes, func(event agent.RestoreProgress) error {
		events = append(events, event)
		if event.CurrentBytes > 0 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copy cancellation error = %v, want context.Canceled", err)
	}
	if len(events) == 0 || events[len(events)-1].CurrentBytes <= 0 || events[len(events)-1].CurrentBytes >= expectedBytes {
		t.Fatalf("cancellation progress = %#v, want an in-progress byte count", events)
	}

	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read helper PID: %v", err)
	}
	pid, err := strconv.Atoi(string(pidBytes))
	if err != nil {
		t.Fatalf("parse helper PID: %v", err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("helper PID %d still exists after Copy returned; kill(0) error = %v", pid, err)
	}

	if err := staged.Cleanup(); err != nil {
		t.Fatalf("clean interrupted owned staging: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(cache.path, ".staging", "copy-cancel")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned staging subtree remains after cleanup: %v", err)
	}
}

func TestBoundedRcloneProcessOutputDiscardsOverflow(t *testing.T) {
	var output boundedRcloneOutput
	input := strings.Repeat("x", maxRcloneProcessOutputBytes*2)
	n, err := output.Write([]byte(input))
	if err != nil || n != len(input) {
		t.Fatalf("bounded output write = (%d, %v), want (%d, nil)", n, err, len(input))
	}
	if output.Len() != maxRcloneProcessOutputBytes {
		t.Fatalf("captured output bytes = %d, want at most %d", output.Len(), maxRcloneProcessOutputBytes)
	}
	if !output.Overflowed() {
		t.Fatal("bounded output did not mark discarded overflow")
	}
}

func newRcloneTestStaging(t *testing.T, jobID, filename string) (*Cache, *StagingFile, string) {
	t.Helper()
	cache, err := OpenCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cache.Close(); err != nil {
			t.Errorf("close test cache: %v", err)
		}
	})
	staged, err := cache.CreateStaging(jobID, filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := staged.Cleanup(); err != nil {
			t.Errorf("clean test staging: %v", err)
		}
	})
	destination := filepath.Join(cache.path, ".staging", jobID, filename)
	return cache, staged, destination
}

func installRcloneTestHelper(t *testing.T, mode string, expectedBytes int64) (string, string) {
	t.Helper()
	helperDir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(helperDir, "rclone")); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(t.TempDir(), "argv.json")
	pidFile := filepath.Join(t.TempDir(), "helper.pid")
	t.Setenv("PATH", helperDir)
	t.Setenv(rcloneTestModeEnv, mode)
	t.Setenv(rcloneTestArgsFileEnv, argsFile)
	t.Setenv(rcloneTestSizeEnv, strconv.FormatInt(expectedBytes, 10))
	t.Setenv(rcloneTestPIDFileEnv, pidFile)
	return argsFile, pidFile
}

func readRcloneTestArgs(t *testing.T, filename string) []string {
	t.Helper()
	contents, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read helper argv: %v", err)
	}
	var args []string
	if err := json.Unmarshal(contents, &args); err != nil {
		t.Fatalf("decode helper argv: %v", err)
	}
	return args
}

func runRcloneTestHelper(args []string) int {
	if len(args) < 1 {
		return 90
	}
	if args[0] == "lsjson" {
		if len(args) != 3 {
			return 90
		}
		return runRcloneStatTestHelper(args)
	}
	if args[0] != "copyto" {
		return 90
	}
	destinationIndex := 2
	if len(args) == 4 && args[1] == "--inplace" {
		destinationIndex = 3
	} else if len(args) != 3 {
		return 90
	}
	encoded, err := json.Marshal(args)
	if err != nil || os.WriteFile(os.Getenv(rcloneTestArgsFileEnv), encoded, 0o600) != nil {
		return 91
	}
	size, err := strconv.ParseInt(os.Getenv(rcloneTestSizeEnv), 10, 64)
	if err != nil || size <= 0 {
		return 92
	}
	switch os.Getenv(rcloneTestModeEnv) {
	case "success", "wait", "restore-success", "restore-size-mismatch":
		file, err := os.OpenFile(args[destinationIndex], os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return 93
		}
		chunk := make([]byte, 256*1024)
		var written int64
		for written < size {
			count := int64(len(chunk))
			if left := size - written; left < count {
				count = left
			}
			if _, err := file.Write(chunk[:count]); err != nil {
				_ = file.Close()
				return 94
			}
			written += count
			if os.Getenv(rcloneTestModeEnv) == "wait" && written >= 1<<20 {
				if os.WriteFile(os.Getenv(rcloneTestPIDFileEnv), []byte(strconv.Itoa(os.Getpid())), 0o600) != nil {
					_ = file.Close()
					return 95
				}
				for {
					time.Sleep(time.Hour)
				}
			}
			time.Sleep(30 * time.Millisecond)
		}
		if err := file.Close(); err != nil {
			return 96
		}
		return 0
	case "fail":
		_, _ = fmt.Fprint(os.Stderr, rcloneTestPrivateToken, strings.Repeat("x", maxRcloneProcessOutputBytes*4))
		return 17
	default:
		return 97
	}
}

func runRcloneStatTestHelper(args []string) int {
	if args[0] != "lsjson" || args[1] != "--stat" {
		return 98
	}
	if filename := os.Getenv("LERNAE_TEST_RCLONE_STAT_ARGS_FILE"); filename != "" {
		encoded, err := json.Marshal(args)
		if err != nil || os.WriteFile(filename, encoded, 0o600) != nil {
			return 99
		}
	}
	mode := os.Getenv(rcloneTestModeEnv)
	size, err := strconv.ParseInt(os.Getenv(rcloneTestSizeEnv), 10, 64)
	if err != nil || size <= 0 {
		return 92
	}
	isDirectory := false
	switch mode {
	case "restore-success", "restore-directory", "restore-size-mismatch":
		if mode == "restore-directory" {
			isDirectory = true
		}
		if mode == "restore-size-mismatch" {
			size++
		}
	default:
		return 97
	}
	if _, err := fmt.Fprintf(os.Stdout, `{"IsDir":%t,"Size":%d}`, isDirectory, size); err != nil {
		return 96
	}
	return 0
}

func equalStrings(first, second []string) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func sizeOrZero(info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	return info.Size()
}
