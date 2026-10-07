// Package agent defines the typed commands and local UDS transport used by the
// Server/Agent boundary. No generic process or shell operation exists here.
package agent

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"lernae/internal/domain"
)

// Command can only be implemented inside this package, keeping the protocol
// explicit rather than exposing a generic command or shell string.
type Command interface {
	isAgentCommand()
}

type RestoreAsset struct {
	JobID          string               `json:"job_id"`
	Asset          domain.Asset         `json:"asset"`
	Parts          []domain.AssetPart   `json:"parts"`
	SourceLocation domain.AssetLocation `json:"source_location"`
}

func (RestoreAsset) isAgentCommand() {}

type LaunchAsset struct {
	SessionID     domain.SessionID   `json:"session_id"`
	WorkID        domain.WorkID      `json:"work_id"`
	EditionID     domain.EditionID   `json:"edition_id"`
	AssetID       domain.AssetID     `json:"asset_id"`
	PartID        domain.AssetPartID `json:"part_id"`
	Medium        domain.Medium      `json:"medium"`
	Platform      string             `json:"platform"`
	Format        string             `json:"format"`
	Role          string             `json:"role"`
	Filename      string             `json:"filename"`
	ExpectedBytes int64              `json:"expected_bytes"`
}

func (LaunchAsset) isAgentCommand() {}

func (launch LaunchAsset) Validate() error {
	if !safeRestoreID.MatchString(string(launch.SessionID)) || !safeRestoreID.MatchString(string(launch.WorkID)) ||
		!safeRestoreID.MatchString(string(launch.EditionID)) || !safeRestoreID.MatchString(string(launch.AssetID)) ||
		!safeRestoreID.MatchString(string(launch.PartID)) {
		return ErrUnsafeLaunchRequest
	}
	if launch.ExpectedBytes <= 0 {
		return ErrLaunchExpectedSizeRequired
	}
	if launch.Medium != domain.MediumGame || launch.Platform != "gamecube" || launch.Format != "disc_image" || launch.Role != "rom" {
		return ErrUnsupportedLaunchShape
	}
	if !safeRestoreFilename(launch.Filename) {
		return ErrUnsafeLaunchRequest
	}
	return nil
}

// DomainRecords materializes only the fixed GameCube shape represented by a
// validated request. The cache still reopens and verifies the actual file.
func (launch LaunchAsset) DomainRecords() (domain.Work, domain.Edition, domain.Asset, []domain.AssetPart) {
	work := domain.Work{ID: launch.WorkID, Medium: launch.Medium}
	edition := domain.Edition{ID: launch.EditionID, WorkID: launch.WorkID, Platform: launch.Platform, Format: launch.Format}
	asset := domain.Asset{ID: launch.AssetID, EditionID: launch.EditionID, Kind: launch.Format, TotalSizeBytes: launch.ExpectedBytes}
	part := domain.AssetPart{
		ID: launch.PartID, AssetID: launch.AssetID, Role: launch.Role,
		Filename: launch.Filename, SizeBytes: launch.ExpectedBytes,
	}
	return work, edition, asset, []domain.AssetPart{part}
}

type LaunchProcess interface {
	Wait() LaunchExit
}

type LaunchExecutor interface {
	Start(context.Context, LaunchAsset) (LaunchProcess, error)
}

type LaunchExit struct {
	Outcome  domain.SessionOutcome
	ExitCode *int
}

func (exit LaunchExit) Validate() error {
	switch exit.Outcome {
	case domain.SessionOutcomeNormalExit:
		if exit.ExitCode == nil || *exit.ExitCode != 0 {
			return errors.New("normal launch exit requires exit code zero")
		}
	case domain.SessionOutcomeNonZeroExit:
		if exit.ExitCode == nil || *exit.ExitCode == 0 {
			return errors.New("non-zero launch exit requires a non-zero exit code")
		}
	case domain.SessionOutcomeInterrupted:
		if exit.ExitCode != nil {
			return errors.New("interrupted launch exit must not expose an exit code")
		}
	default:
		return errors.New("unknown launch outcome")
	}
	return nil
}

type LaunchStarted struct {
	SessionID domain.SessionID `json:"session_id"`
	WorkID    domain.WorkID    `json:"work_id"`
	EditionID domain.EditionID `json:"edition_id"`
	AssetID   domain.AssetID   `json:"asset_id"`
	StartedAt time.Time        `json:"started_at"`
}

func (event LaunchStarted) ValidateFor(request LaunchAsset) error {
	if event.SessionID != request.SessionID || event.WorkID != request.WorkID || event.EditionID != request.EditionID ||
		event.AssetID != request.AssetID || event.StartedAt.IsZero() {
		return errors.New("Agent returned invalid launch-start evidence")
	}
	return nil
}

type LaunchEnded struct {
	SessionID domain.SessionID      `json:"session_id"`
	WorkID    domain.WorkID         `json:"work_id"`
	EditionID domain.EditionID      `json:"edition_id"`
	AssetID   domain.AssetID        `json:"asset_id"`
	EndedAt   time.Time             `json:"ended_at"`
	Outcome   domain.SessionOutcome `json:"outcome"`
	ExitCode  *int                  `json:"exit_code,omitempty"`
}

func (event LaunchEnded) ValidateFor(request LaunchAsset) error {
	if event.SessionID != request.SessionID || event.WorkID != request.WorkID || event.EditionID != request.EditionID ||
		event.AssetID != request.AssetID || event.EndedAt.IsZero() {
		return errors.New("Agent returned invalid launch-end evidence")
	}
	return (LaunchExit{Outcome: event.Outcome, ExitCode: event.ExitCode}).Validate()
}

type LaunchFailureCode string

const (
	LaunchFailureAlreadyRunning    LaunchFailureCode = "already_running"
	LaunchFailureLocalCacheInvalid LaunchFailureCode = "local_cache_invalid"
	LaunchFailureStartFailed       LaunchFailureCode = "start_failed"
)

type LaunchFailed struct {
	SessionID domain.SessionID  `json:"session_id"`
	Code      LaunchFailureCode `json:"code"`
}

func (event LaunchFailed) ValidateFor(request LaunchAsset) error {
	if event.SessionID != request.SessionID || (event.Code != LaunchFailureAlreadyRunning &&
		event.Code != LaunchFailureLocalCacheInvalid && event.Code != LaunchFailureStartFailed) {
		return errors.New("Agent returned invalid launch failure")
	}
	return nil
}

type LaunchAcknowledgement struct {
	SessionID domain.SessionID `json:"session_id"`
	Persisted bool             `json:"persisted"`
}

type GetStatus struct{}

func (GetStatus) isAgentCommand() {}

type Operation string

const (
	OperationGetStatus    Operation = "get_status"
	OperationRestoreAsset Operation = "restore_asset"
	OperationLaunchAsset  Operation = "launch_asset"
)

const (
	RestoreSourceProviderFixture = "fixture"
	RestoreSourceClassFixture    = "fixture"
	RestoreSourceProviderRclone  = "rclone"
	RestoreSourceClassArchive    = "archive"
)

var (
	ErrUnsupportedAssetShape      = errors.New("restore supports exactly one regular ROM file")
	ErrExpectedSizeRequired       = errors.New("restore requires a known positive expected size")
	ErrUnsafeRestorePath          = errors.New("restore path is unsafe")
	ErrUnsupportedLaunchShape     = errors.New("launch supports exactly one GameCube disc-image ROM")
	ErrUnsafeLaunchRequest        = errors.New("launch request identity or filename is unsafe")
	ErrLaunchExpectedSizeRequired = errors.New("launch requires a known positive expected size")
	ErrLaunchStartFailed          = errors.New("Agent could not start the requested local game")
	ErrLaunchAlreadyRunning       = errors.New("a Lernae-owned game is already running")
	ErrLaunchLocalCacheInvalid    = errors.New("Agent local-cache image is unavailable or invalid before launch")
	ErrLaunchNotPersisted         = errors.New("Session was not persisted; the launched process was stopped")
	ErrLaunchStateUnconfirmed     = errors.New("Agent launch state is unconfirmed after request dispatch")
	safeRestoreID                 = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
)

func (restore RestoreAsset) Validate() error {
	if !safeRestoreID.MatchString(restore.JobID) || !safeRestoreID.MatchString(string(restore.Asset.ID)) {
		return ErrUnsafeRestorePath
	}
	if restore.Asset.TotalSizeBytes <= 0 {
		return ErrExpectedSizeRequired
	}
	if len(restore.Parts) != 1 {
		return ErrUnsupportedAssetShape
	}
	part := restore.Parts[0]
	if part.AssetID != restore.Asset.ID || part.Role != "rom" || part.RelativePath != "" {
		return ErrUnsupportedAssetShape
	}
	if part.SizeBytes <= 0 || part.SizeBytes != restore.Asset.TotalSizeBytes {
		return ErrExpectedSizeRequired
	}
	if part.ID == "" {
		return ErrUnsupportedAssetShape
	}
	if !safeRestoreFilename(part.Filename) {
		return ErrUnsafeRestorePath
	}
	if !validRestoreSourceLocation(restore.SourceLocation, restore.Asset.ID) {
		return ErrUnsafeRestorePath
	}
	return nil
}

func validRestoreSourceLocation(location domain.AssetLocation, assetID domain.AssetID) bool {
	if location.AssetID != assetID || location.Locator == "" || len(location.Locator) > 4096 ||
		!utf8.ValidString(location.Locator) || strings.TrimSpace(location.Locator) != location.Locator {
		return false
	}
	for _, character := range location.Locator {
		if character == 0 || character < 0x20 || character == 0x7f {
			return false
		}
	}

	switch location.StorageProviderID {
	case RestoreSourceProviderFixture:
		return location.LocationClass == RestoreSourceClassFixture && safeFixtureDescriptor(location.Locator)
	case RestoreSourceProviderRclone:
		return location.LocationClass == RestoreSourceClassArchive
	default:
		return false
	}
}

func safeRestoreFilename(filename string) bool {
	if filename == "" || len(filename) > 255 || !utf8.ValidString(filename) || filename == "." || filename == ".." || strings.TrimSpace(filename) == "" || strings.ContainsAny(filename, `/\\:`) {
		return false
	}
	for _, character := range filename {
		if character == 0 || character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func safeFixtureDescriptor(descriptor string) bool {
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

type RestorePhase string

const (
	RestorePhaseValidating RestorePhase = "validating"
	RestorePhaseStaging    RestorePhase = "staging"
	RestorePhaseCopying    RestorePhase = "copying"
	RestorePhaseVerifying  RestorePhase = "verifying"
	RestorePhasePromoting  RestorePhase = "promoting"
	RestorePhaseComplete   RestorePhase = "complete"
)

type RestoreProgress struct {
	Phase        RestorePhase `json:"phase"`
	CurrentBytes int64        `json:"current_bytes"`
	TotalBytes   int64        `json:"total_bytes"`
}

func (progress RestoreProgress) Validate() error {
	if progress.CurrentBytes < 0 || progress.TotalBytes <= 0 || progress.CurrentBytes > progress.TotalBytes {
		return errors.New("restore progress is outside its byte bounds")
	}
	if progress.Phase == RestorePhaseComplete && progress.CurrentBytes != progress.TotalBytes {
		return errors.New("completed restore progress must equal the expected byte total")
	}
	switch progress.Phase {
	case RestorePhaseValidating, RestorePhaseStaging, RestorePhaseCopying,
		RestorePhaseVerifying, RestorePhasePromoting, RestorePhaseComplete:
		return nil
	default:
		return fmt.Errorf("unknown restore progress phase %q", progress.Phase)
	}
}

func ValidateProgressAfter(previous *RestoreProgress, next RestoreProgress) error {
	if err := next.Validate(); err != nil {
		return err
	}
	if previous == nil {
		return nil
	}
	if next.TotalBytes != previous.TotalBytes || next.CurrentBytes < previous.CurrentBytes || restorePhaseOrder(next.Phase) < restorePhaseOrder(previous.Phase) {
		return errors.New("restore progress regressed or changed its byte total")
	}
	return nil
}

func restorePhaseOrder(phase RestorePhase) int {
	switch phase {
	case RestorePhaseValidating:
		return 0
	case RestorePhaseStaging:
		return 1
	case RestorePhaseCopying:
		return 2
	case RestorePhaseVerifying:
		return 3
	case RestorePhasePromoting:
		return 4
	case RestorePhaseComplete:
		return 5
	default:
		return -1
	}
}

type RestoreResult struct {
	AssetID           domain.AssetID `json:"asset_id"`
	LocationClass     string         `json:"location_class"`
	RelativePath      string         `json:"relative_path"`
	VerifiedSizeBytes int64          `json:"verified_size_bytes"`
	VerifiedAt        time.Time      `json:"verified_at"`
	LocalReady        bool           `json:"local_ready"`
}

func (result RestoreResult) ValidateFor(restore RestoreAsset) error {
	if err := restore.Validate(); err != nil {
		return err
	}
	part := restore.Parts[0]
	wantPath := path.Join("assets", string(restore.Asset.ID), part.Filename)
	if result.AssetID != restore.Asset.ID || result.LocationClass != "local_cache" || result.RelativePath != wantPath ||
		result.VerifiedSizeBytes != restore.Asset.TotalSizeBytes || result.VerifiedAt.IsZero() || !result.LocalReady {
		return errors.New("restore result does not prove a verified final local-cache Asset")
	}
	return nil
}

type RestoreExecutor interface {
	Restore(context.Context, RestoreAsset, func(RestoreProgress) error) (RestoreResult, error)
}

type Request struct {
	Operation    Operation     `json:"operation"`
	RestoreAsset *RestoreAsset `json:"restore_asset,omitempty"`
	LaunchAsset  *LaunchAsset  `json:"launch_asset,omitempty"`
}

func (request Request) Validate() error {
	switch request.Operation {
	case OperationGetStatus:
		if request.RestoreAsset != nil || request.LaunchAsset != nil {
			return errors.New("GetStatus request must not include an operation payload")
		}
		return nil
	case OperationRestoreAsset:
		if request.RestoreAsset == nil || request.LaunchAsset != nil {
			return errors.New("restore operation requires a typed RestoreAsset payload")
		}
		return request.RestoreAsset.Validate()
	case OperationLaunchAsset:
		if request.LaunchAsset == nil || request.RestoreAsset != nil {
			return errors.New("launch operation requires a typed LaunchAsset payload")
		}
		return request.LaunchAsset.Validate()
	default:
		return ErrUnsupportedOperation
	}
}

type State string

const StateOnline State = "online"

type AgentStatus struct {
	State     State     `json:"state"`
	StartedAt time.Time `json:"started_at"`
}

type Response struct {
	Status        *AgentStatus     `json:"status,omitempty"`
	Progress      *RestoreProgress `json:"progress,omitempty"`
	RestoreResult *RestoreResult   `json:"restore_result,omitempty"`
	LaunchStarted *LaunchStarted   `json:"launch_started,omitempty"`
	LaunchEnded   *LaunchEnded     `json:"launch_ended,omitempty"`
	LaunchFailed  *LaunchFailed    `json:"launch_failed,omitempty"`
	Error         string           `json:"error,omitempty"`
}

type StatusClient interface {
	GetStatus(context.Context) (AgentStatus, error)
}
