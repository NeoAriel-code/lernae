package agentops

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxRcloneLocatorBytes      = 4096
	maxRcloneStatOutputBytes   = 1 << 20
	maxRcloneErrorMessageBytes = 128
	maxRcloneRemoteNameBytes   = 64
	rcloneStatSubcommand       = "lsjson"
	rcloneStatObjectFlag       = "--stat"
)

var safeRcloneRemoteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// RcloneFailureCategory is a bounded, sanitized classification of an
// unexpected rclone source/stat condition. It never contains command output,
// a locator, or environment/configuration data.
type RcloneFailureCategory string

const (
	RcloneFailureInvalidLocator        RcloneFailureCategory = "invalid_locator"
	RcloneFailureExecutableUnavailable RcloneFailureCategory = "executable_unavailable"
	RcloneFailureInvalidStat           RcloneFailureCategory = "invalid_stat"
	RcloneFailureNotRegularFile        RcloneFailureCategory = "not_regular_file"
	RcloneFailureUnknownSize           RcloneFailureCategory = "unknown_size"
	RcloneFailureSizeMismatch          RcloneFailureCategory = "size_mismatch"
	RcloneFailureExpectedSizeInvalid   RcloneFailureCategory = "expected_size_invalid"
)

// RcloneError exposes only a fixed category, keeping paths and process output
// out of internal and user-facing error strings.
type RcloneError struct {
	category RcloneFailureCategory
}

func (failure *RcloneError) Error() string {
	if failure == nil {
		return "rclone operation failed"
	}
	message := "rclone operation failed: " + string(failure.category)
	if len(message) > maxRcloneErrorMessageBytes {
		return "rclone operation failed"
	}
	return message
}

func (failure *RcloneError) Category() RcloneFailureCategory {
	if failure == nil {
		return ""
	}
	return failure.category
}

// RcloneSourceDescriptor is one exact object on one configured rclone remote.
// Its fields are private so callers must obtain one through the validating
// locator parser rather than constructing it from arbitrary components.
type RcloneSourceDescriptor struct {
	remoteName string
	objectPath string
}

func (source RcloneSourceDescriptor) RemoteName() string { return source.remoteName }

func (source RcloneSourceDescriptor) ObjectPath() string { return source.objectPath }

// Locator returns the original remote:path value as a single data string. It
// is not a local path and must never be interpreted as shell input.
func (source RcloneSourceDescriptor) Locator() string {
	return source.remoteName + ":" + source.objectPath
}

// ParseRcloneSourceLocator accepts only an exact regular-object-shaped
// remote-name:path locator. It does not resolve the remote or access storage.
func ParseRcloneSourceLocator(locator string) (RcloneSourceDescriptor, error) {
	if len(locator) == 0 || len(locator) > maxRcloneLocatorBytes || !utf8.ValidString(locator) || strings.TrimSpace(locator) != locator {
		return RcloneSourceDescriptor{}, rcloneFailure(RcloneFailureInvalidLocator)
	}
	for _, character := range locator {
		if unicode.IsControl(character) {
			return RcloneSourceDescriptor{}, rcloneFailure(RcloneFailureInvalidLocator)
		}
	}
	if strings.ContainsRune(locator, '\\') {
		return RcloneSourceDescriptor{}, rcloneFailure(RcloneFailureInvalidLocator)
	}

	separator := strings.IndexByte(locator, ':')
	if separator <= 0 || separator == len(locator)-1 {
		return RcloneSourceDescriptor{}, rcloneFailure(RcloneFailureInvalidLocator)
	}
	remoteName, objectPath := locator[:separator], locator[separator+1:]
	if !validRcloneRemoteName(remoteName) || !validRcloneObjectPath(objectPath) {
		return RcloneSourceDescriptor{}, rcloneFailure(RcloneFailureInvalidLocator)
	}
	return RcloneSourceDescriptor{remoteName: remoteName, objectPath: objectPath}, nil
}

func validRcloneRemoteName(remoteName string) bool {
	return len(remoteName) <= maxRcloneRemoteNameBytes && safeRcloneRemoteName.MatchString(remoteName)
}

func validRcloneObjectPath(objectPath string) bool {
	if objectPath == "" || path.IsAbs(objectPath) || strings.HasSuffix(objectPath, "/") || path.Clean(objectPath) != objectPath {
		return false
	}
	for _, component := range strings.Split(objectPath, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

// ResolveRcloneExecutable resolves only the locally installed "rclone" name
// through the process PATH. Callers cannot provide an executable override.
func ResolveRcloneExecutable() (string, error) {
	executable, err := exec.LookPath("rclone")
	if err != nil || !filepath.IsAbs(executable) {
		return "", rcloneFailure(RcloneFailureExecutableUnavailable)
	}
	return executable, nil
}

// BuildRcloneStatArgs returns the fixed, read-only machine-readable stat
// invocation. The locator is exactly one final argv element, never a flag or
// shell fragment; no caller-supplied executable or flags are accepted.
func BuildRcloneStatArgs(source RcloneSourceDescriptor) ([]string, error) {
	if !validRcloneRemoteName(source.remoteName) || !validRcloneObjectPath(source.objectPath) {
		return nil, rcloneFailure(RcloneFailureInvalidLocator)
	}
	return []string{rcloneStatSubcommand, rcloneStatObjectFlag, source.Locator()}, nil
}

type RcloneObjectStat struct {
	SizeBytes int64
}

// ParseRcloneStatJSON requires a single machine-readable regular-file stat
// whose known positive size equals the AssetPart's expected size.
func ParseRcloneStatJSON(output []byte, expectedPartSize int64) (RcloneObjectStat, error) {
	if expectedPartSize <= 0 {
		return RcloneObjectStat{}, rcloneFailure(RcloneFailureExpectedSizeInvalid)
	}
	if len(output) == 0 || len(output) > maxRcloneStatOutputBytes {
		return RcloneObjectStat{}, rcloneFailure(RcloneFailureInvalidStat)
	}

	decoder := json.NewDecoder(bytes.NewReader(output))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return RcloneObjectStat{}, rcloneFailure(RcloneFailureInvalidStat)
	}
	seen := make(map[string]struct{})
	var (
		directoryKnown bool
		isDirectory    bool
		sizeKnown      bool
		sizeBytes      int64
	)
	for decoder.More() {
		keyToken, tokenErr := decoder.Token()
		key, ok := keyToken.(string)
		if tokenErr != nil || !ok {
			return RcloneObjectStat{}, rcloneFailure(RcloneFailureInvalidStat)
		}
		if _, duplicate := seen[key]; duplicate {
			return RcloneObjectStat{}, rcloneFailure(RcloneFailureInvalidStat)
		}
		seen[key] = struct{}{}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return RcloneObjectStat{}, rcloneFailure(RcloneFailureInvalidStat)
		}
		switch key {
		case "IsDir":
			var parsed *bool
			if err := json.Unmarshal(value, &parsed); err != nil || parsed == nil {
				return RcloneObjectStat{}, rcloneFailure(RcloneFailureInvalidStat)
			}
			isDirectory = *parsed
			directoryKnown = true
		case "Size":
			var parsed *int64
			if err := json.Unmarshal(value, &parsed); err != nil || parsed == nil {
				return RcloneObjectStat{}, rcloneFailure(RcloneFailureUnknownSize)
			}
			sizeBytes = *parsed
			sizeKnown = true
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return RcloneObjectStat{}, rcloneFailure(RcloneFailureInvalidStat)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RcloneObjectStat{}, rcloneFailure(RcloneFailureInvalidStat)
	}
	if !directoryKnown {
		return RcloneObjectStat{}, rcloneFailure(RcloneFailureInvalidStat)
	}
	if isDirectory {
		return RcloneObjectStat{}, rcloneFailure(RcloneFailureNotRegularFile)
	}
	if !sizeKnown || sizeBytes <= 0 {
		return RcloneObjectStat{}, rcloneFailure(RcloneFailureUnknownSize)
	}
	if sizeBytes != expectedPartSize {
		return RcloneObjectStat{}, rcloneFailure(RcloneFailureSizeMismatch)
	}
	return RcloneObjectStat{SizeBytes: sizeBytes}, nil
}

func rcloneFailure(category RcloneFailureCategory) error {
	return &RcloneError{category: category}
}
