// Package playcoordinator accepts provider-neutral PLAY intent and owns the
// single durable Job across inventory resolution, optional restore, Agent
// launch, and Session completion.
package playcoordinator

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"lernae/internal/agent"
	"lernae/internal/catalog"
	"lernae/internal/domain"
	"lernae/internal/jobs"
	"lernae/internal/playback"
	"lernae/internal/providers"
)

const (
	playPlatform = "gamecube"
	playFormat   = "disc_image"

	acceptanceTimeout    = 2 * time.Second
	terminalWriteTimeout = 3 * time.Second
	jobErrorDetail       = "PLAY could not complete safely"
)

var (
	ErrInvalidIntent        = errors.New("PLAY requires a supported Work identity and Edition intent")
	ErrInventoryUnavailable = errors.New("no exact owned Asset is available for this Edition")
	ErrAmbiguousAsset       = errors.New("multiple exact owned Assets match the PLAY intent")
	ErrAmbiguousSource      = errors.New("PLAY source metadata is ambiguous or inconsistent")
	ErrNoLaunchSource       = errors.New("PLAY has neither a local candidate nor one exact archive source")
	ErrPlayConflict         = errors.New("another PLAY operation is active")
	ErrServiceStopping      = errors.New("PLAY service is stopping")
)

// Intent contains only the canonical external Work identity and Edition
// selection. Asset authority and launch/restore details are always resolved
// internally from the owned catalog graph.
type Intent struct {
	WorkIdentity providers.ExternalWorkIdentity
	Edition      EditionIntent
}

type EditionIntent struct {
	Platform string
	Format   string
}

// Accepted contains the opaque Job reference returned after durable acceptance.
type Accepted struct {
	JobID string
}

// Catalog is the bounded catalog surface required by PLAY. Local-cache
// locations are projected only from validated Agent restore evidence.
type Catalog interface {
	GetWorkByExternalIdentity(context.Context, string, string) (catalog.WorkGraph, error)
	EnsureLocalCacheLocation(context.Context, agent.RestoreAsset, agent.RestoreResult) (domain.AssetLocation, error)
	RemoveLocalCacheLocation(context.Context, domain.AssetLocation) (int64, error)
}

// Playback is the P1-06 start/Session boundary. LaunchWithStarted notifies the
// coordinator only after the active Session row has been persisted.
type Playback interface {
	LaunchWithStarted(agent.LaunchAsset, func(domain.Session) bool) (agent.LaunchEnded, error)
}

type intentKey struct {
	provider   string
	externalID string
	platform   string
	format     string
}

type activeOperation struct {
	key   intentKey
	jobID string
}

// PlayCoordinator owns PLAY work under a Server-lifetime context. Start does
// not accept an HTTP/request context, so request disconnect cannot cancel work
// after the Job is durably accepted.
type PlayCoordinator struct {
	ctx       context.Context
	catalog   Catalog
	inventory providers.InventorySource
	storage   providers.StorageSource
	jobs      *jobs.Service
	restore   jobs.RestoreClient
	playback  Playback

	mu     sync.Mutex
	active *activeOperation
	wg     sync.WaitGroup
}

// Admission is serialized across coordinator instances in one Server process;
// the persisted active-PLAY Job then owns the only supported PLAY worker. This
// does not coordinate multiple Server processes sharing one Jobs database.
var coordinatorStartMu sync.Mutex

// NewService constructs a PLAY service whose operation lifetime is bounded by
// the injected Server context.
func NewService(
	serverContext context.Context,
	catalogRepository Catalog,
	inventorySource providers.InventorySource,
	storageSource providers.StorageSource,
	jobService *jobs.Service,
	restoreClient jobs.RestoreClient,
	playbackService Playback,
) (*PlayCoordinator, error) {
	if serverContext == nil || catalogRepository == nil || inventorySource == nil || storageSource == nil ||
		jobService == nil || restoreClient == nil || playbackService == nil {
		return nil, errors.New("PLAY service requires Server context, catalog, inventory, storage, Jobs, Agent restore, and playback")
	}
	return &PlayCoordinator{
		ctx: serverContext, catalog: catalogRepository, inventory: inventorySource, storage: storageSource,
		jobs: jobService, restore: restoreClient, playback: playbackService,
	}, nil
}

// Start validates the intent, durably creates one PLAY Job, and hands the
// accepted operation to the Server-lifetime worker. A repeated active intent
// returns the existing Job reference; a different PLAY conflicts.
func (coordinator *PlayCoordinator) Start(intent Intent) (Accepted, error) {
	if err := validateIntent(intent); err != nil {
		return Accepted{}, err
	}
	if err := coordinator.ctx.Err(); err != nil {
		return Accepted{}, ErrServiceStopping
	}
	acceptCtx, cancelAccept := context.WithTimeout(coordinator.ctx, acceptanceTimeout)
	defer cancelAccept()
	key := keyFor(intent)

	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	coordinatorStartMu.Lock()
	defer coordinatorStartMu.Unlock()

	if coordinator.active != nil {
		if coordinator.active.key == key {
			return Accepted{JobID: coordinator.active.jobID}, nil
		}
		return Accepted{}, ErrPlayConflict
	}
	if err := coordinator.ctx.Err(); err != nil {
		return Accepted{}, ErrServiceStopping
	}
	activeJobs, err := coordinator.jobs.List(acceptCtx)
	if err != nil {
		return Accepted{}, fmt.Errorf("check active PLAY operations: %w", err)
	}
	for _, job := range activeJobs {
		if job.Kind == jobs.KindPlay && activeStatus(job.Status) {
			// Intent is deliberately not stored in Job messages or details. If the
			// current service cannot prove the binding, fail closed with conflict.
			return Accepted{}, ErrPlayConflict
		}
	}
	job, err := coordinator.jobs.Create(acceptCtx, jobs.KindPlay)
	if err != nil {
		return Accepted{}, fmt.Errorf("create PLAY Job: %w", err)
	}
	coordinator.active = &activeOperation{key: key, jobID: job.ID}
	coordinator.wg.Add(1)
	go coordinator.run(job.ID, intent)
	return Accepted{JobID: job.ID}, nil
}

// Wait drains all accepted PLAY workers after the owner cancels the injected
// Server context and stops admitting HTTP requests. The mutex barrier waits for
// any Start already admitting an operation before waiting on worker completion.
func (coordinator *PlayCoordinator) Wait() {
	coordinator.mu.Lock()
	coordinator.mu.Unlock()
	coordinator.wg.Wait()
}

func (coordinator *PlayCoordinator) run(jobID string, intent Intent) {
	defer coordinator.wg.Done()
	defer coordinator.releaseActiveIfTerminal(jobID)

	if startErr := coordinator.jobs.Transition(coordinator.ctx, jobID, jobs.StatusRunning); startErr != nil {
		// An ambiguous first write may already have moved the Job to running. A
		// retry that succeeds also enters the ordinary worker path; two failed
		// writes terminalize from the state actually persisted.
		if reconcileErr := coordinator.reconcileStartFailure(jobID, startErr); reconcileErr != nil {
			return
		}
	}
	if err := coordinator.ctx.Err(); err != nil {
		coordinator.finishError(jobID, jobs.StatusInterrupted, "play_interrupted")
		return
	}

	resolved, err := coordinator.resolve(coordinator.ctx, intent)
	if err != nil {
		if coordinator.ctx.Err() != nil {
			coordinator.finishError(jobID, jobs.StatusInterrupted, "play_interrupted")
			return
		}
		code := "inventory_resolution_failed"
		switch {
		case errors.Is(err, ErrInventoryUnavailable):
			code = "inventory_unavailable"
		case errors.Is(err, ErrAmbiguousAsset), errors.Is(err, ErrAmbiguousSource):
			code = "inventory_conflict"
		case errors.Is(err, ErrNoLaunchSource):
			code = "launch_source_unavailable"
		}
		coordinator.finishError(jobID, jobs.StatusFailed, code)
		return
	}

	restored := false
	if !resolved.localCandidate {
		if resolved.archive == nil {
			coordinator.finishError(jobID, jobs.StatusFailed, "launch_source_unavailable")
			return
		}
		location, err := coordinator.restoreAndProject(coordinator.ctx, jobID, resolved)
		if err != nil {
			coordinator.finishPhaseError(jobID, err, "restore_failed")
			return
		}
		resolved.localCandidate = true
		resolved.localLocation = &location
		restored = true
	}

	attempt, err := coordinator.launch(coordinator.ctx, jobID, resolved)
	if err != nil {
		coordinator.finishPhaseError(jobID, err, "launch_failed")
		return
	}
	if errors.Is(attempt.err, agent.ErrLaunchLocalCacheInvalid) && attempt.sessionID == "" && coordinator.ctx.Err() == nil {
		if err := coordinator.removeInvalidCandidate(resolved.localLocation); err != nil {
			coordinator.finishError(jobID, jobs.StatusFailed, "cache_metadata_cleanup_failed")
			return
		}
		if restored || !resolved.localCandidate || resolved.archive == nil {
			// A restored candidate is already the one allowed archive attempt.
			// Clear stale metadata but never repeat the transfer.
			coordinator.finishError(jobID, jobs.StatusFailed, "local_cache_invalid")
			return
		}
		if err := coordinator.jobs.Transition(coordinator.ctx, jobID, jobs.StatusRunning); err != nil {
			coordinator.finishPhaseError(jobID, err, "play_interrupted")
			return
		}
		location, err := coordinator.restoreAndProject(coordinator.ctx, jobID, resolved)
		if err != nil {
			coordinator.finishPhaseError(jobID, err, "restore_failed")
			return
		}
		resolved.localCandidate = true
		resolved.localLocation = &location
		restored = true
		attempt, err = coordinator.launch(coordinator.ctx, jobID, resolved)
		if err != nil {
			coordinator.finishPhaseError(jobID, err, "launch_failed")
			return
		}
		if errors.Is(attempt.err, agent.ErrLaunchLocalCacheInvalid) && attempt.sessionID == "" {
			// The newly projected row is stale too; remove only its metadata and
			// stop after the single archive restore.
			if err := coordinator.removeInvalidCandidate(resolved.localLocation); err != nil {
				coordinator.finishError(jobID, jobs.StatusFailed, "cache_metadata_cleanup_failed")
				return
			}
		}
	}
	coordinator.finishLaunch(jobID, attempt)
}

type resolvedAsset struct {
	work           domain.Work
	edition        domain.Edition
	asset          domain.Asset
	part           domain.AssetPart
	localCandidate bool
	localLocation  *domain.AssetLocation
	archive        *domain.AssetLocation
}

func (coordinator *PlayCoordinator) resolve(ctx context.Context, intent Intent) (resolvedAsset, error) {
	matches, err := coordinator.inventory.FindMatchingAssets(ctx, providers.InventoryQuery{
		WorkIdentity: intent.WorkIdentity, Platform: intent.Edition.Platform, Format: intent.Edition.Format,
	})
	if err != nil {
		return resolvedAsset{}, fmt.Errorf("resolve exact owned inventory: %w", err)
	}
	if len(matches) == 0 {
		return resolvedAsset{}, ErrInventoryUnavailable
	}
	if len(matches) != 1 {
		return resolvedAsset{}, ErrAmbiguousAsset
	}
	match := matches[0]
	if match.AssetID == "" || match.Platform != intent.Edition.Platform || match.Format != intent.Edition.Format {
		return resolvedAsset{}, ErrAmbiguousAsset
	}

	graph, err := coordinator.catalog.GetWorkByExternalIdentity(ctx, intent.WorkIdentity.Provider, intent.WorkIdentity.ExternalID)
	if err != nil {
		return resolvedAsset{}, fmt.Errorf("load canonical Work graph: %w", err)
	}
	if graph.Work.ID == "" || graph.Work.Medium != domain.MediumGame {
		return resolvedAsset{}, errors.New("canonical Work is not a supported game")
	}
	identityCount := 0
	for _, identity := range graph.ExternalIdentities {
		if identity.Provider == intent.WorkIdentity.Provider && identity.ExternalID == intent.WorkIdentity.ExternalID && identity.WorkID == graph.Work.ID {
			identityCount++
		}
	}
	if identityCount != 1 {
		return resolvedAsset{}, errors.New("canonical Work identity is missing or ambiguous")
	}

	var edition domain.Edition
	editionCount := 0
	for _, candidate := range graph.Editions {
		if candidate.WorkID == graph.Work.ID && candidate.Platform == intent.Edition.Platform && candidate.Format == intent.Edition.Format {
			edition, editionCount = candidate, editionCount+1
		}
	}
	if editionCount != 1 {
		return resolvedAsset{}, errors.New("canonical Edition is missing or ambiguous")
	}

	var asset domain.Asset
	assetCount := 0
	for _, candidate := range graph.Assets {
		if candidate.ID == match.AssetID {
			asset, assetCount = candidate, assetCount+1
		}
	}
	if assetCount != 1 || asset.EditionID != edition.ID || asset.Kind != playFormat || asset.TotalSizeBytes <= 0 ||
		match.TotalSizeBytes != asset.TotalSizeBytes {
		return resolvedAsset{}, ErrAmbiguousAsset
	}

	var part domain.AssetPart
	partCount := 0
	for _, candidate := range graph.Parts {
		if candidate.AssetID == asset.ID {
			part, partCount = candidate, partCount+1
		}
	}
	if len(match.Parts) != 1 || partCount != 1 || !samePart(part, match.Parts[0]) || part.Role != "rom" ||
		part.SizeBytes != asset.TotalSizeBytes || part.RelativePath != "" || part.Filename == "" {
		return resolvedAsset{}, ErrAmbiguousAsset
	}

	localCandidates := make([]domain.AssetLocation, 0, 1)
	graphArchives := make([]domain.AssetLocation, 0, 1)
	for _, location := range graph.Locations {
		if location.AssetID != asset.ID {
			continue
		}
		switch location.LocationClass {
		case "local_cache":
			localCandidates = append(localCandidates, location)
		case "archive":
			graphArchives = append(graphArchives, location)
		default:
			return resolvedAsset{}, ErrAmbiguousSource
		}
	}
	if len(localCandidates) > 1 || len(graphArchives) > 1 {
		return resolvedAsset{}, ErrAmbiguousSource
	}
	localCandidate := len(localCandidates) == 1
	var localLocation *domain.AssetLocation
	if localCandidate {
		candidate := localCandidates[0]
		localLocation = &candidate
	}
	if localCandidate && !validLocalCandidate(*localLocation, asset, part) {
		return resolvedAsset{}, ErrAmbiguousSource
	}

	storageLocations, err := coordinator.storage.LocationsForAsset(ctx, asset.ID)
	if err != nil {
		return resolvedAsset{}, fmt.Errorf("resolve exact Asset source: %w", err)
	}
	storageArchives := make([]domain.AssetLocation, 0, 1)
	for _, location := range storageLocations {
		if location.AssetID != asset.ID {
			return resolvedAsset{}, ErrAmbiguousSource
		}
		switch location.LocationClass {
		case "archive":
			storageArchives = append(storageArchives, location)
		case "local_cache":
			// A source adapter cannot prove local readiness. Only catalog metadata
			// is considered as a candidate, and Agent revalidates it on launch.
		default:
			return resolvedAsset{}, ErrAmbiguousSource
		}
	}
	if len(storageArchives) > 1 || len(storageArchives) != len(graphArchives) {
		return resolvedAsset{}, ErrAmbiguousSource
	}
	var archive *domain.AssetLocation
	if len(graphArchives) == 1 {
		if !sameLocationSource(graphArchives[0], storageArchives[0]) {
			return resolvedAsset{}, ErrAmbiguousSource
		}
		candidate := graphArchives[0]
		archive = &candidate
	}
	if !localCandidate && archive == nil {
		return resolvedAsset{}, ErrNoLaunchSource
	}
	return resolvedAsset{work: graph.Work, edition: edition, asset: asset, part: part,
		localCandidate: localCandidate, localLocation: localLocation, archive: archive}, nil
}

func (coordinator *PlayCoordinator) restoreAndProject(ctx context.Context, jobID string, resolved resolvedAsset) (domain.AssetLocation, error) {
	if resolved.archive == nil {
		return domain.AssetLocation{}, ErrNoLaunchSource
	}
	request := agent.RestoreAsset{JobID: jobID, Asset: resolved.asset, Parts: []domain.AssetPart{resolved.part}, SourceLocation: *resolved.archive}
	result, err := coordinator.jobs.RunRestorePhase(ctx, jobID, request, coordinator.restore)
	if err != nil {
		return domain.AssetLocation{}, err
	}
	location, err := coordinator.catalog.EnsureLocalCacheLocation(ctx, request, result)
	if err != nil {
		return domain.AssetLocation{}, fmt.Errorf("persist validated local-cache location: %w", err)
	}
	return location, nil
}

type launchAttempt struct {
	request   agent.LaunchAsset
	ended     agent.LaunchEnded
	err       error
	sessionID string
}

func (coordinator *PlayCoordinator) launch(ctx context.Context, jobID string, resolved resolvedAsset) (launchAttempt, error) {
	job, err := coordinator.jobs.Get(ctx, jobID)
	if err != nil {
		return launchAttempt{}, err
	}
	if job.Status != jobs.StatusRunning && job.Status != jobs.StatusWaitingOnAgent {
		return launchAttempt{}, fmt.Errorf("PLAY Job is not active before launch")
	}
	if job.Phase == jobs.PhaseRestore {
		if err := coordinator.jobs.UpdatePlayPhase(ctx, jobID, jobs.PhaseLaunch); err != nil {
			return launchAttempt{}, err
		}
	}
	if job.Status == jobs.StatusRunning {
		if err := coordinator.jobs.Transition(ctx, jobID, jobs.StatusWaitingOnAgent); err != nil {
			return launchAttempt{}, err
		}
	}
	request := agent.LaunchAsset{
		WorkID: resolved.work.ID, EditionID: resolved.edition.ID, AssetID: resolved.asset.ID, PartID: resolved.part.ID,
		Medium: resolved.work.Medium, Platform: resolved.edition.Platform, Format: resolved.edition.Format,
		Role: resolved.part.Role, Filename: resolved.part.Filename, ExpectedBytes: resolved.part.SizeBytes,
	}
	var sessionID domain.SessionID
	ended, launchErr := coordinator.playback.LaunchWithStarted(request, func(session domain.Session) bool {
		// Keep correlation even if the Job-side acknowledgement fails. Playback
		// will reject the Agent ACK and return terminal interrupted evidence for
		// the Session it already persisted.
		sessionID = session.ID
		if session.ID == "" || session.WorkID != resolved.work.ID || session.EditionID != resolved.edition.ID ||
			session.AssetID != resolved.asset.ID || session.StartedAt.IsZero() || session.EndedAt != nil || session.Outcome != "" {
			return false
		}
		current, err := coordinator.jobs.Get(ctx, jobID)
		if err != nil {
			return false
		}
		if current.Phase == "" {
			if err := coordinator.jobs.UpdatePlayPhase(ctx, jobID, jobs.PhaseLaunch); err != nil {
				return false
			}
		} else if current.Phase != jobs.PhaseLaunch {
			return false
		}
		if err := coordinator.jobs.AttachPlaySession(ctx, jobID, string(session.ID)); err != nil {
			return false
		}
		if err := coordinator.jobs.UpdatePlayPhase(ctx, jobID, jobs.PhasePlaying); err != nil {
			return false
		}
		sessionID = session.ID
		return true
	})
	request.SessionID = sessionID
	return launchAttempt{request: request, ended: ended, err: launchErr, sessionID: string(sessionID)}, nil
}

func (coordinator *PlayCoordinator) finishLaunch(jobID string, attempt launchAttempt) {
	if err := attempt.ended.ValidateFor(attempt.request); err != nil {
		code := "launch_failed"
		if coordinator.ctx.Err() != nil || errors.Is(attempt.err, agent.ErrLaunchStateUnconfirmed) || errors.Is(attempt.err, context.Canceled) ||
			errors.Is(attempt.err, context.DeadlineExceeded) {
			coordinator.finishError(jobID, jobs.StatusInterrupted, "launch_unconfirmed")
			return
		}
		coordinator.finishError(jobID, jobs.StatusFailed, code)
		return
	}
	if errors.Is(attempt.err, playback.ErrSessionStartNotificationRejected) {
		// LaunchWithStarted can receive a normal terminal event immediately after
		// the Job-side start notification was rejected. Playback has still
		// finalized its persisted Session, but PLAY never earned a successful
		// durable Playing transition and must not report clean gameplay success.
		switch attempt.ended.Outcome {
		case domain.SessionOutcomeInterrupted:
			coordinator.finishError(jobID, jobs.StatusInterrupted, "play_interrupted")
		case domain.SessionOutcomeNonZeroExit:
			coordinator.finishError(jobID, jobs.StatusFailed, "game_exit_nonzero")
		default:
			coordinator.finishError(jobID, jobs.StatusFailed, "session_start_rejected")
		}
		return
	}

	switch attempt.ended.Outcome {
	case domain.SessionOutcomeNormalExit:
		if attempt.sessionID == "" {
			coordinator.finishError(jobID, jobs.StatusFailed, "session_start_unconfirmed")
			return
		}
		if err := coordinator.finishSuccess(jobID); err != nil {
			coordinator.finishError(jobID, jobs.StatusFailed, "session_terminal_unconfirmed")
		}
	case domain.SessionOutcomeNonZeroExit:
		coordinator.finishError(jobID, jobs.StatusFailed, "game_exit_nonzero")
	case domain.SessionOutcomeInterrupted:
		coordinator.finishError(jobID, jobs.StatusInterrupted, "play_interrupted")
	default:
		coordinator.finishError(jobID, jobs.StatusFailed, "launch_failed")
	}
}

func (coordinator *PlayCoordinator) finishSuccess(jobID string) error {
	ctx, cancel := coordinator.writeContext()
	defer cancel()
	if err := coordinator.jobs.Transition(ctx, jobID, jobs.StatusSucceeded); err != nil {
		current, getErr := coordinator.jobs.Get(ctx, jobID)
		if getErr == nil && current.Status == jobs.StatusSucceeded {
			return nil
		}
		return err
	}
	return nil
}

func (coordinator *PlayCoordinator) finishPhaseError(jobID string, err error, defaultCode string) {
	if coordinator.ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		coordinator.finishError(jobID, jobs.StatusInterrupted, "play_interrupted")
		return
	}
	coordinator.finishError(jobID, jobs.StatusFailed, defaultCode)
}

func (coordinator *PlayCoordinator) finishError(jobID string, status jobs.Status, code string) {
	ctx, cancel := coordinator.writeContext()
	defer cancel()
	if err := coordinator.jobs.TransitionWithError(ctx, jobID, status, code, safeDetail(code)); err != nil {
		current, getErr := coordinator.jobs.Get(ctx, jobID)
		if getErr == nil && terminalStatus(current.Status) {
			return
		}
	}
}

func (coordinator *PlayCoordinator) reconcileStartFailure(jobID string, startErr error) error {
	writeCtx, cancel := coordinator.writeContext()
	defer cancel()
	causes := []error{fmt.Errorf("transition accepted PLAY Job to running: %w", startErr)}
	current, err := coordinator.jobs.Get(writeCtx, jobID)
	if err != nil {
		return errors.Join(append(causes, fmt.Errorf("read PLAY Job after start failure: %w", err))...)
	}
	if current.Status == jobs.StatusQueued {
		retryErr := coordinator.jobs.Transition(writeCtx, jobID, jobs.StatusRunning)
		if retryErr == nil {
			return nil
		}
		causes = append(causes, fmt.Errorf("retry PLAY Job start transition: %w", retryErr))
		current, err = coordinator.jobs.Get(writeCtx, jobID)
		if err != nil {
			causes = append(causes, fmt.Errorf("read PLAY Job after retry failure: %w", err))
			return errors.Join(causes...)
		}
	}
	if current.Status == jobs.StatusRunning {
		return nil
	}
	if activeStatus(current.Status) {
		status, code := jobs.StatusFailed, "job_start_failed"
		if current.Status == jobs.StatusQueued {
			// queued -> cancelled is the only truthful terminal transition allowed
			// before work has entered running.
			status = jobs.StatusCancelled
		}
		if coordinator.ctx.Err() != nil && status != jobs.StatusCancelled {
			status, code = jobs.StatusInterrupted, "play_interrupted"
		}
		if err := coordinator.jobs.TransitionWithError(writeCtx, jobID, status, code, safeDetail(code)); err != nil {
			causes = append(causes, fmt.Errorf("terminalize PLAY Job after start failure: %w", err))
			return errors.Join(causes...)
		}
		return errors.Join(causes...)
	}
	return errors.Join(append(causes, fmt.Errorf("PLAY Job became terminal during start reconciliation: %s", current.Status))...)
}

func (coordinator *PlayCoordinator) removeInvalidCandidate(location *domain.AssetLocation) error {
	if location == nil {
		return errors.New("Agent rejected a local-cache candidate that was not retained by the coordinator")
	}
	if _, err := coordinator.catalog.RemoveLocalCacheLocation(coordinator.ctx, *location); err != nil {
		return err
	}
	return nil
}

func (coordinator *PlayCoordinator) releaseActiveIfTerminal(jobID string) {
	ctx, cancel := coordinator.writeContext()
	defer cancel()
	job, err := coordinator.jobs.Get(ctx, jobID)
	if err != nil || !terminalStatus(job.Status) {
		return
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.active != nil && coordinator.active.jobID == jobID {
		coordinator.active = nil
	}
}

func (coordinator *PlayCoordinator) writeContext() (context.Context, context.CancelFunc) {
	// Detached final persistence keeps accepted work truthful when Server
	// shutdown cancels its operation context, while the deadline bounds shutdown.
	return context.WithTimeout(context.WithoutCancel(coordinator.ctx), terminalWriteTimeout)
}

func validateIntent(intent Intent) error {
	if !validIdentityPart(intent.WorkIdentity.Provider, 64) || !validIdentityPart(intent.WorkIdentity.ExternalID, 128) ||
		intent.Edition.Platform != playPlatform || intent.Edition.Format != playFormat {
		return ErrInvalidIntent
	}
	return nil
}

func validIdentityPart(value string, max int) bool {
	if value == "" || len(value) > max || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character == 0 || character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func keyFor(intent Intent) intentKey {
	return intentKey{provider: intent.WorkIdentity.Provider, externalID: intent.WorkIdentity.ExternalID,
		platform: intent.Edition.Platform, format: intent.Edition.Format}
}

func activeStatus(status jobs.Status) bool {
	return status == jobs.StatusQueued || status == jobs.StatusRunning || status == jobs.StatusWaitingOnAgent
}

func terminalStatus(status jobs.Status) bool {
	return status == jobs.StatusSucceeded || status == jobs.StatusFailed || status == jobs.StatusInterrupted || status == jobs.StatusCancelled
}

func samePart(part domain.AssetPart, match providers.InventoryPart) bool {
	if part.Role != match.Role || part.Filename != match.Filename || part.RelativePath != match.RelativePath || part.SizeBytes != match.SizeBytes {
		return false
	}
	if part.PartIndex == nil || match.PartIndex == nil {
		return part.PartIndex == nil && match.PartIndex == nil
	}
	return *part.PartIndex == *match.PartIndex
}

func sameLocationSource(first, second domain.AssetLocation) bool {
	return first.AssetID == second.AssetID && first.StorageProviderID == second.StorageProviderID &&
		first.Locator == second.Locator && first.LocationClass == second.LocationClass
}

func validLocalCandidate(location domain.AssetLocation, asset domain.Asset, part domain.AssetPart) bool {
	want := "assets/" + string(asset.ID) + "/" + part.Filename
	return location.AssetID == asset.ID && location.StorageProviderID == "filesystem" && location.LocationClass == "local_cache" &&
		location.Locator == want && path.Clean(location.Locator) == want && !path.IsAbs(location.Locator)
}

func safeDetail(code string) string {
	switch code {
	case "game_exit_nonzero":
		return "game process exited unsuccessfully"
	case "play_interrupted", "launch_unconfirmed":
		return "PLAY ended without confirmed normal completion"
	case "job_start_failed":
		return "PLAY could not start safely"
	case "session_start_rejected":
		return "PLAY Session start could not be confirmed by the Job"
	default:
		return jobErrorDetail
	}
}
