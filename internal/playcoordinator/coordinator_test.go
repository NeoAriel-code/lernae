package playcoordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"lernae/internal/agent"
	"lernae/internal/catalog"
	"lernae/internal/database"
	"lernae/internal/domain"
	"lernae/internal/jobs"
	"lernae/internal/playback"
	"lernae/internal/providers"
	"lernae/internal/sessions"
)

func TestStartRejectsInvalidIntentBeforeCreatingJob(t *testing.T) {
	f := newCoordinatorFixture(t, false)
	for _, test := range []struct {
		name   string
		intent Intent
	}{
		{name: "missing provider", intent: Intent{WorkIdentity: providers.ExternalWorkIdentity{ExternalID: "1565"}, Edition: EditionIntent{Platform: "gamecube", Format: "disc_image"}}},
		{name: "missing external ID", intent: Intent{WorkIdentity: providers.ExternalWorkIdentity{Provider: "igdb"}, Edition: EditionIntent{Platform: "gamecube", Format: "disc_image"}}},
		{name: "unsupported platform", intent: Intent{WorkIdentity: testIdentity(), Edition: EditionIntent{Platform: "playstation-2", Format: "disc_image"}}},
		{name: "unsupported format", intent: Intent{WorkIdentity: testIdentity(), Edition: EditionIntent{Platform: "gamecube", Format: "archive"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := f.coordinator.Start(test.intent); !errors.Is(err, ErrInvalidIntent) {
				t.Fatalf("Start() error = %v, want ErrInvalidIntent", err)
			}
		})
	}
	listed, err := f.jobs.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("invalid intents created Jobs: %#v", listed)
	}
}

func TestWaitDrainsAcceptedWorkerAfterServerContextCancellation(t *testing.T) {
	f := newCoordinatorFixture(t, false)
	gate := &startTransitionGate{entered: make(chan struct{}), release: make(chan struct{})}
	f.jobRepository.mu.Lock()
	f.jobRepository.startGate = gate
	f.jobRepository.mu.Unlock()
	accepted := f.start(t, testIdentity())
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatal("accepted worker did not reach the queued-to-running transition")
	}
	queued, err := f.jobs.Get(context.Background(), accepted.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Status != jobs.StatusQueued {
		t.Fatalf("Job status at blocked startup transition = %q, want queued", queued.Status)
	}
	f.cancel()
	waitDone := make(chan struct{})
	go func() {
		f.coordinator.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		t.Fatal("Wait() returned while an accepted worker still used coordinator dependencies")
	case <-time.After(25 * time.Millisecond):
	}
	close(gate.release)
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("Wait() did not return after the accepted worker drained")
	}
	job, err := f.jobs.Get(context.Background(), accepted.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != jobs.StatusInterrupted {
		t.Fatalf("drained PLAY Job status = %q, want interrupted", job.Status)
	}
}

func TestUnavailableAndAmbiguousInventoryFailClosed(t *testing.T) {
	tests := []struct {
		name    string
		matches []providers.InventoryMatch
	}{
		{name: "unavailable"},
		{name: "two exact Assets", matches: []providers.InventoryMatch{testMatch(), {AssetID: "asset-other", Platform: "gamecube", Format: "disc_image", TotalSizeBytes: 4, Parts: []providers.InventoryPart{{Role: "rom", Filename: "other.iso", SizeBytes: 4}}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newCoordinatorFixture(t, false)
			f.inventory.setMatches(test.matches)
			accepted := f.start(t, testIdentity())
			job := f.waitTerminal(t, accepted.JobID)
			if job.Status != jobs.StatusFailed {
				t.Fatalf("Job status = %s, want failed", job.Status)
			}
			if got := f.restore.count(); got != 0 {
				t.Fatalf("restore calls = %d, want 0", got)
			}
			if got := f.launch.count(); got != 0 {
				t.Fatalf("launch calls = %d, want 0", got)
			}
			if got := f.sessionCount(t); got != 0 {
				t.Fatalf("persisted Sessions = %d, want 0", got)
			}
		})
	}
}

func TestUnboundWorkIdentityFailsWithoutRestoreOrLaunch(t *testing.T) {
	f := newCoordinatorFixture(t, false)
	if _, err := f.db.Exec(`DELETE FROM external_identities WHERE provider = ? AND external_id = ?`, testIdentity().Provider, testIdentity().ExternalID); err != nil {
		t.Fatal(err)
	}
	accepted := f.start(t, testIdentity())
	job := f.waitTerminal(t, accepted.JobID)
	if job.Status != jobs.StatusFailed || f.restore.count() != 0 || f.launch.count() != 0 {
		t.Fatalf("unbound identity outcome: job=%#v restore=%d launch=%d", job, f.restore.count(), f.launch.count())
	}
}

func TestCanonicalPartMismatchAndMultipleLocalCandidatesFailClosed(t *testing.T) {
	tests := []struct {
		name           string
		withCandidate  bool
		mutate         func(*coordinatorFixture)
		seedExtraLocal bool
	}{
		{name: "inventory Part differs from canonical AssetPart", mutate: func(f *coordinatorFixture) {
			match := testMatch()
			match.Parts[0].Filename = "different.iso"
			f.inventory.setMatches([]providers.InventoryMatch{match})
		}},
		{name: "multiple local candidates", withCandidate: true, seedExtraLocal: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newCoordinatorFixture(t, test.withCandidate)
			if test.mutate != nil {
				test.mutate(f)
			}
			if test.seedExtraLocal {
				_, err := f.db.Exec(`INSERT INTO asset_locations (id, asset_id, storage_provider_id, locator, location_class)
					VALUES (?, ?, ?, ?, ?)`, "location-local-extra", testAssetID(), "filesystem", "assets/asset-gc-1/other.iso", "local_cache")
				if err != nil {
					t.Fatal(err)
				}
			}
			accepted := f.start(t, testIdentity())
			job := f.waitTerminal(t, accepted.JobID)
			if job.Status != jobs.StatusFailed || f.restore.count() != 0 || f.launch.count() != 0 {
				t.Fatalf("fail-closed outcome: job=%#v restore=%d launch=%d", job, f.restore.count(), f.launch.count())
			}
		})
	}
}

func TestLocalCacheCandidateLaunchesWithoutRestoreAndPreservesArchive(t *testing.T) {
	f := newCoordinatorFixture(t, true)
	accepted := f.start(t, testIdentity())
	job := f.waitTerminal(t, accepted.JobID)
	if job.Status != jobs.StatusSucceeded || job.Phase != jobs.PhasePlaying || job.SessionID == "" {
		t.Fatalf("completed Job = %#v", job)
	}
	if f.restore.count() != 0 || f.launch.count() != 1 {
		t.Fatalf("restore/launch calls = %d/%d, want 0/1", f.restore.count(), f.launch.count())
	}
	if got := f.session(t, job.SessionID); got.Outcome != domain.SessionOutcomeNormalExit || got.EndedAt == nil {
		t.Fatalf("Session = %#v, want finalized normal exit", got)
	}
	f.assertLocations(t, true)
}

func TestArchiveOnlyRestoresOnceProjectsTrustedLocalCacheThenLaunches(t *testing.T) {
	f := newCoordinatorFixture(t, false)
	accepted := f.start(t, testIdentity())
	job := f.waitTerminal(t, accepted.JobID)
	if job.Status != jobs.StatusSucceeded || job.Phase != jobs.PhasePlaying {
		t.Fatalf("completed Job = %#v", job)
	}
	if f.restore.count() != 1 || f.launch.count() != 1 {
		t.Fatalf("restore/launch calls = %d/%d, want 1/1", f.restore.count(), f.launch.count())
	}
	if f.catalog.ensureCount() != 1 {
		t.Fatalf("local-cache projection calls = %d, want 1", f.catalog.ensureCount())
	}
	f.assertLocations(t, true)
}

func TestRestoreFailureOrInvalidProofNeverProjectsOrLaunches(t *testing.T) {
	tests := []struct {
		name    string
		fail    bool
		invalid bool
		cancel  bool
	}{
		{name: "Agent restore error", fail: true},
		{name: "invalid LOCAL_READY proof", invalid: true},
		{name: "cancellation", cancel: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newCoordinatorFixture(t, false)
			f.restore.fail = test.fail
			f.restore.invalid = test.invalid
			f.restore.waitForCancel = test.cancel
			accepted := f.start(t, testIdentity())
			if test.cancel {
				<-f.restore.entered
				f.cancel()
			}
			job := f.waitTerminal(t, accepted.JobID)
			want := jobs.StatusFailed
			if test.cancel {
				want = jobs.StatusInterrupted
			}
			if job.Status != want {
				t.Fatalf("Job status = %s, want %s (%#v)", job.Status, want, job)
			}
			if f.launch.count() != 0 || f.sessionCount(t) != 0 {
				t.Fatalf("failed restore launched or persisted Session: launches=%d sessions=%d", f.launch.count(), f.sessionCount(t))
			}
			if f.catalog.ensureCount() != 0 {
				t.Fatalf("invalid restore evidence projected %d local-cache locations", f.catalog.ensureCount())
			}
			f.assertLocations(t, false)
		})
	}
}

func TestTypedStaleCandidateIsCleanedAndFallsBackExactlyOnce(t *testing.T) {
	f := newCoordinatorFixture(t, true)
	graph, err := f.base.GetWorkByExternalIdentity(context.Background(), testIdentity().Provider, testIdentity().ExternalID)
	if err != nil || len(graph.Locations) != 2 {
		t.Fatalf("load exact stale-candidate identity before PLAY: locations=%#v err=%v", graph.Locations, err)
	}
	var invalidCandidate domain.AssetLocation
	for _, location := range graph.Locations {
		if location.LocationClass == "local_cache" {
			invalidCandidate = location
		}
	}
	if invalidCandidate.ID == "" {
		t.Fatal("fixture did not contain a local-cache candidate")
	}
	f.launch.setErrors(agent.ErrLaunchLocalCacheInvalid, nil)
	accepted := f.start(t, testIdentity())
	job := f.waitTerminal(t, accepted.JobID)
	if job.Status != jobs.StatusSucceeded || job.SessionID == "" {
		t.Fatalf("fallback Job = %#v", job)
	}
	if f.launch.count() != 2 || f.restore.count() != 1 {
		t.Fatalf("launch/restore calls = %d/%d, want 2/1", f.launch.count(), f.restore.count())
	}
	if f.catalog.removeCount() != 1 || f.catalog.ensureCount() != 1 {
		t.Fatalf("metadata cleanup/projection = %d/%d, want 1/1", f.catalog.removeCount(), f.catalog.ensureCount())
	}
	removed := f.catalog.removedLocationSnapshot()
	if len(removed) != 1 || removed[0] != invalidCandidate {
		t.Fatalf("cleanup removed locations = %#v, want exactly observed invalid candidate %#v", removed, invalidCandidate)
	}
	f.assertLocations(t, true)
}

func TestRepeatedQueuedStartWriteFailureTerminalizesJobAndReleasesBinding(t *testing.T) {
	f := newCoordinatorFixture(t, false)
	f.jobRepository.failQueuedRunning = 2
	accepted := f.start(t, testIdentity())
	f.coordinator.wg.Wait()

	job, err := f.jobs.Get(context.Background(), accepted.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != jobs.StatusCancelled || job.ErrorCode != "job_start_failed" {
		t.Fatalf("accepted Job after repeated start-write failures = %#v, want safely cancelled with truthful category", job)
	}
	if job.ErrorDetail == "" || f.restore.count() != 0 || f.launch.count() != 0 {
		t.Fatalf("startup failure leaked unsafe detail or performed work: job=%#v restore=%d launch=%d", job, f.restore.count(), f.launch.count())
	}

	// The first terminal Job must no longer hold the coordinator's in-memory
	// binding; the distinct intent will be accepted and fail its exact lookup.
	second, err := f.coordinator.Start(Intent{WorkIdentity: providers.ExternalWorkIdentity{Provider: "igdb", ExternalID: "other"}, Edition: testIntent().Edition})
	if err != nil || second.JobID == "" || second.JobID == accepted.JobID {
		t.Fatalf("Start after terminalized startup failure = %#v, %v; want a fresh accepted Job", second, err)
	}
	f.coordinator.wg.Wait()
}

func TestRejectedStartCallbackCannotTurnImmediateNormalExitIntoPlaySuccess(t *testing.T) {
	for _, test := range []struct {
		name        string
		outcome     domain.SessionOutcome
		wantStatus  jobs.Status
		wantErrCode string
	}{
		{name: "normal exit is not clean success", outcome: domain.SessionOutcomeNormalExit, wantStatus: jobs.StatusFailed, wantErrCode: "session_start_rejected"},
		{name: "interruption remains interrupted", outcome: domain.SessionOutcomeInterrupted, wantStatus: jobs.StatusInterrupted, wantErrCode: "play_interrupted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCoordinatorFixture(t, true)
			f.launch.terminalOnRejectedStart = true
			f.launch.outcome = test.outcome
			f.jobRepository.failPlayingPhaseAfterPersist = true
			accepted := f.start(t, testIdentity())
			job := f.waitTerminal(t, accepted.JobID)

			if job.Status != test.wantStatus || job.ErrorCode != test.wantErrCode {
				t.Fatalf("Job after rejected durable start callback = %#v, want status=%s code=%q", job, test.wantStatus, test.wantErrCode)
			}
			if job.SessionID == "" {
				t.Fatal("already persisted Session correlation was not retained on the PLAY Job")
			}
			session := f.session(t, job.SessionID)
			if session.Outcome != test.outcome || session.EndedAt == nil {
				t.Fatalf("Session terminal truth was not preserved after callback rejection: %#v", session)
			}
			if f.restore.count() != 0 || f.launch.count() != 1 {
				t.Fatalf("callback rejection caused restore fallback or duplicate launch: restore=%d launches=%d", f.restore.count(), f.launch.count())
			}
		})
	}
}

func TestNonCacheLaunchFailuresNeverRedownload(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "Agent already running", err: agent.ErrLaunchAlreadyRunning},
		{name: "Dolphin missing or generic start failure", err: agent.ErrLaunchStartFailed},
		{name: "transport failure", err: errors.New("transport unavailable")},
		{name: "Session persistence rejected", err: agent.ErrLaunchNotPersisted},
		{name: "post-dispatch uncertainty", err: agent.ErrLaunchStateUnconfirmed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newCoordinatorFixture(t, true)
			f.launch.setErrors(test.err)
			accepted := f.start(t, testIdentity())
			job := f.waitTerminal(t, accepted.JobID)
			wantStatus := jobs.StatusFailed
			if errors.Is(test.err, agent.ErrLaunchStateUnconfirmed) {
				wantStatus = jobs.StatusInterrupted
			}
			if job.Status != wantStatus {
				t.Fatalf("Job status = %s, want %s", job.Status, wantStatus)
			}
			if f.restore.count() != 0 || f.launch.count() != 1 {
				t.Fatalf("restore/launch calls = %d/%d, want 0/1", f.restore.count(), f.launch.count())
			}
			if f.catalog.removeCount() != 0 || f.sessionCount(t) != 0 {
				t.Fatalf("non-cache error mutated cache metadata or Session state: cleanup=%d sessions=%d", f.catalog.removeCount(), f.sessionCount(t))
			}
		})
	}
}

func TestSecondTypedCacheInvalidAfterRestoreDoesNotRestoreAgain(t *testing.T) {
	f := newCoordinatorFixture(t, true)
	f.launch.setErrors(agent.ErrLaunchLocalCacheInvalid, agent.ErrLaunchLocalCacheInvalid)
	accepted := f.start(t, testIdentity())
	job := f.waitTerminal(t, accepted.JobID)
	if job.Status != jobs.StatusFailed {
		t.Fatalf("Job status = %s, want failed", job.Status)
	}
	if f.restore.count() != 1 || f.launch.count() != 2 {
		t.Fatalf("restore/launch calls = %d/%d, want exactly 1/2", f.restore.count(), f.launch.count())
	}
	if f.catalog.removeCount() != 2 {
		t.Fatalf("stale candidate metadata cleanup calls = %d, want one for each typed invalid candidate", f.catalog.removeCount())
	}
	removed := f.catalog.removedLocationSnapshot()
	if len(removed) != 2 || removed[0] != removed[1] || removed[0].ID == "" {
		t.Fatalf("cleanup identities = %#v, want the exact candidate observed on both Agent attempts", removed)
	}
}

func TestMultipleArchiveSourcesFailClosed(t *testing.T) {
	f := newCoordinatorFixture(t, false)
	f.storage.setLocations([]domain.AssetLocation{testArchiveLocation(), {
		AssetID: testAssetID(), StorageProviderID: "rclone", Locator: "otherremote:other/game.iso", LocationClass: "archive",
	}})
	accepted := f.start(t, testIdentity())
	job := f.waitTerminal(t, accepted.JobID)
	if job.Status != jobs.StatusFailed || f.restore.count() != 0 || f.launch.count() != 0 {
		t.Fatalf("ambiguous source outcome: job=%#v restore=%d launch=%d", job, f.restore.count(), f.launch.count())
	}
}

func TestStartReturnsPromptlyAndOnlyPersistsSessionAfterAgentStart(t *testing.T) {
	f := newCoordinatorFixture(t, true)
	releaseStart := make(chan struct{})
	releaseTerminal := make(chan struct{})
	f.launch.beforeStart = releaseStart
	f.launch.terminalGate = releaseTerminal
	startedAt := time.Now().UTC()
	accepted := f.start(t, testIdentity())
	select {
	case <-f.launch.entered:
	case <-time.After(time.Second):
		t.Fatal("asynchronous launch was not accepted promptly")
	}
	if got := f.sessionCount(t); got != 0 {
		t.Fatalf("Session persisted before Agent process-start confirmation: %d", got)
	}
	close(releaseStart)
	playing := f.waitForJob(t, accepted.JobID, func(job jobs.Job) bool { return job.Phase == jobs.PhasePlaying && job.SessionID != "" })
	if got := f.session(t, playing.SessionID); got.StartedAt.Before(startedAt) || got.EndedAt != nil {
		t.Fatalf("Session after Agent start = %#v, want active", got)
	}
	close(releaseTerminal)
	f.waitTerminal(t, accepted.JobID)
}

func TestPlayJobWaitsForTerminalSessionEvidenceAndPreservesExitOutcome(t *testing.T) {
	tests := []struct {
		name       string
		outcome    domain.SessionOutcome
		exitCode   *int
		wantStatus jobs.Status
	}{
		{name: "normal exit", outcome: domain.SessionOutcomeNormalExit, exitCode: intPtr(0), wantStatus: jobs.StatusSucceeded},
		{name: "nonzero exit", outcome: domain.SessionOutcomeNonZeroExit, exitCode: intPtr(17), wantStatus: jobs.StatusFailed},
		{name: "interrupted", outcome: domain.SessionOutcomeInterrupted, wantStatus: jobs.StatusInterrupted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newCoordinatorFixture(t, true)
			releaseTerminal := make(chan struct{})
			f.launch.outcome, f.launch.exitCode = test.outcome, test.exitCode
			f.launch.terminalGate = releaseTerminal
			accepted := f.start(t, testIdentity())
			playing := f.waitForJob(t, accepted.JobID, func(job jobs.Job) bool { return job.Phase == jobs.PhasePlaying && job.SessionID != "" })
			if playing.Status != jobs.StatusWaitingOnAgent {
				t.Fatalf("Job while Agent waits for exit = %#v", playing)
			}
			if got := f.session(t, playing.SessionID); got.EndedAt != nil {
				t.Fatalf("Session finalized before terminal Agent evidence: %#v", got)
			}
			select {
			case <-f.launch.terminalEntered:
			case <-time.After(time.Second):
				t.Fatal("Agent did not reach terminal wait")
			}
			if current, _ := f.jobs.Get(context.Background(), accepted.JobID); current.Status == jobs.StatusSucceeded || current.Status == jobs.StatusFailed || current.Status == jobs.StatusInterrupted {
				t.Fatalf("Job terminalized before Session terminal evidence: %#v", current)
			}
			close(releaseTerminal)
			finished := f.waitTerminal(t, accepted.JobID)
			if finished.Status != test.wantStatus {
				t.Fatalf("terminal Job status = %s, want %s (%#v)", finished.Status, test.wantStatus, finished)
			}
			if test.outcome == domain.SessionOutcomeNonZeroExit && finished.ErrorCode != "game_exit_nonzero" {
				t.Fatalf("nonzero exit Job error category = %q, want safe game_exit_nonzero", finished.ErrorCode)
			}
			persisted := f.session(t, finished.SessionID)
			if persisted.Outcome != test.outcome || persisted.EndedAt == nil {
				t.Fatalf("terminal Session = %#v, want finalized %s", persisted, test.outcome)
			}
		})
	}
}

func TestDuplicateSameIntentJoinsAndDifferentIntentConflicts(t *testing.T) {
	f := newCoordinatorFixture(t, false)
	f.restore.block = true
	first := f.start(t, testIdentity())
	<-f.restore.entered
	joined, err := f.coordinator.Start(testIntent())
	if err != nil || joined.JobID != first.JobID {
		t.Fatalf("same-intent Start() = %#v, %v; want same Job", joined, err)
	}
	if _, err := f.coordinator.Start(Intent{WorkIdentity: providers.ExternalWorkIdentity{Provider: "igdb", ExternalID: "different"}, Edition: testIntent().Edition}); !errors.Is(err, ErrPlayConflict) {
		t.Fatalf("different simultaneous intent error = %v, want ErrPlayConflict", err)
	}
	if f.restore.count() != 1 || f.launch.count() != 0 {
		t.Fatalf("duplicate started extra work: restore=%d launches=%d", f.restore.count(), f.launch.count())
	}
	f.cancel()
	job := f.waitTerminal(t, first.JobID)
	if job.Status != jobs.StatusInterrupted {
		t.Fatalf("cancelled duplicate operation status = %s, want interrupted", job.Status)
	}
}

func TestConcurrentSameIntentStartCreatesOneJobAndOneRestore(t *testing.T) {
	f := newCoordinatorFixture(t, false)
	f.restore.block = true
	const count = 12
	accepted := make(chan Accepted, count)
	errs := make(chan error, count)
	var wait sync.WaitGroup
	for i := 0; i < count; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			job, err := f.coordinator.Start(testIntent())
			accepted <- job
			errs <- err
		}()
	}
	wait.Wait()
	close(accepted)
	close(errs)
	var jobID string
	for job := range accepted {
		if jobID == "" {
			jobID = job.JobID
		}
		if job.JobID != jobID {
			t.Fatalf("same-intent submissions returned different Job IDs: %q and %q", jobID, job.JobID)
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("same-intent submission error = %v", err)
		}
	}
	<-f.restore.entered
	if f.restore.count() != 1 {
		t.Fatalf("concurrent restore calls = %d, want 1", f.restore.count())
	}
	f.cancel()
	if job := f.waitTerminal(t, jobID); job.Status != jobs.StatusInterrupted {
		t.Fatalf("concurrent operation terminal status = %s, want interrupted", job.Status)
	}
}

func TestStartRejectsWhenAnotherCoordinatorHasAnActivePlayJob(t *testing.T) {
	f := newCoordinatorFixture(t, false)
	f.restore.block = true
	first := f.start(t, testIdentity())
	<-f.restore.entered
	other, err := newCoordinatorForFixture(t, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Start(Intent{WorkIdentity: providers.ExternalWorkIdentity{Provider: "igdb", ExternalID: "other"}, Edition: testIntent().Edition}); !errors.Is(err, ErrPlayConflict) {
		t.Fatalf("second coordinator active-job error = %v, want ErrPlayConflict", err)
	}
	f.cancel()
	f.waitTerminal(t, first.JobID)
}

func TestIntentContainsOnlyWorkIdentityAndEditionIntent(t *testing.T) {
	intentType := reflect.TypeOf(Intent{})
	if intentType.NumField() != 2 {
		t.Fatalf("Intent has %d fields, want only canonical Work identity and Edition intent", intentType.NumField())
	}
	if intentType.Field(0).Name != "WorkIdentity" || intentType.Field(0).Type != reflect.TypeOf(providers.ExternalWorkIdentity{}) ||
		intentType.Field(1).Name != "Edition" || intentType.Field(1).Type != reflect.TypeOf(EditionIntent{}) {
		t.Fatalf("Intent carries fields outside canonical Work identity + Edition intent: %#v", intentType)
	}
	editionType := reflect.TypeOf(EditionIntent{})
	if editionType.NumField() != 2 || editionType.Field(0).Name != "Platform" || editionType.Field(1).Name != "Format" {
		t.Fatalf("EditionIntent carries fields beyond platform + format: %#v", editionType)
	}
}

type coordinatorFixture struct {
	t             *testing.T
	ctx           context.Context
	cancel        context.CancelFunc
	db            *sql.DB
	jobs          *jobs.Service
	jobRepository *faultingJobRepository
	sessions      *sessions.Service
	catalog       *catalogSpy
	base          *catalog.SQLiteRepository
	inventory     *inventoryStub
	storage       *storageStub
	restore       *restoreStub
	launch        *launchStub
	playback      *playback.Service
	coordinator   *PlayCoordinator
}

func newCoordinatorFixture(t *testing.T, withCandidate bool) *coordinatorFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	base := catalog.NewSQLiteRepository(db)
	graph := testGraph()
	if err := base.EnsureGraph(ctx, graph); err != nil {
		cancel()
		_ = db.Close()
		t.Fatal(err)
	}
	if withCandidate {
		request := testRestoreRequest("existing-job")
		result := testRestoreResult(request)
		if _, err := base.EnsureLocalCacheLocation(ctx, request, result); err != nil {
			cancel()
			_ = db.Close()
			t.Fatal(err)
		}
	}
	cat := &catalogSpy{SQLiteRepository: base}
	inv := &inventoryStub{matches: []providers.InventoryMatch{testMatch()}}
	storage := &storageStub{locations: []domain.AssetLocation{testArchiveLocation()}}
	restore := &restoreStub{entered: make(chan struct{}, 8)}
	launch := &launchStub{entered: make(chan struct{}, 16), terminalEntered: make(chan struct{}, 16), outcome: domain.SessionOutcomeNormalExit, exitCode: intPtr(0)}
	jobRepository := &faultingJobRepository{Repository: jobs.NewSQLiteRepository(db)}
	jobService := jobs.NewService(jobRepository)
	sessionService := sessions.NewService(sessions.NewSQLiteRepository(db))
	playbackService, err := playback.NewService(ctx, launch, sessionService)
	if err != nil {
		cancel()
		_ = db.Close()
		t.Fatal(err)
	}
	coordinator, err := NewService(ctx, cat, inv, storage, jobService, restore, playbackService)
	if err != nil {
		cancel()
		_ = db.Close()
		t.Fatal(err)
	}
	f := &coordinatorFixture{t: t, ctx: ctx, cancel: cancel, db: db, jobs: jobService, jobRepository: jobRepository, sessions: sessionService,
		catalog: cat, base: base, inventory: inv, storage: storage, restore: restore, launch: launch,
		playback: playbackService, coordinator: coordinator}
	t.Cleanup(func() {
		cancel()
		_ = playbackService.Close()
		coordinator.wg.Wait()
		_ = db.Close()
	})
	return f
}

type faultingJobRepository struct {
	jobs.Repository
	mu                           sync.Mutex
	failQueuedRunning            int
	startGate                    *startTransitionGate
	failPlayingPhaseAfterPersist bool
}

type startTransitionGate struct {
	entered chan struct{}
	release chan struct{}
}

func (repository *faultingJobRepository) Transition(ctx context.Context, id string, from, to jobs.Status, at time.Time) error {
	repository.mu.Lock()
	if from == jobs.StatusQueued && to == jobs.StatusRunning && repository.startGate != nil {
		gate := repository.startGate
		repository.startGate = nil
		repository.mu.Unlock()
		close(gate.entered)
		<-gate.release
		if err := ctx.Err(); err != nil {
			return err
		}
		return repository.Repository.Transition(ctx, id, from, to, at)
	}
	if from == jobs.StatusQueued && to == jobs.StatusRunning && repository.failQueuedRunning > 0 {
		repository.failQueuedRunning--
		repository.mu.Unlock()
		return errors.New("injected queued-to-running write failure")
	}
	repository.mu.Unlock()
	return repository.Repository.Transition(ctx, id, from, to, at)
}

func (repository *faultingJobRepository) UpdatePlayPhase(ctx context.Context, id string, expected, next jobs.Phase, at time.Time) error {
	if err := repository.Repository.UpdatePlayPhase(ctx, id, expected, next, at); err != nil {
		return err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if next == jobs.PhasePlaying && repository.failPlayingPhaseAfterPersist {
		repository.failPlayingPhaseAfterPersist = false
		return errors.New("injected uncertain playing-phase acknowledgement")
	}
	return nil
}

func newCoordinatorForFixture(t *testing.T, f *coordinatorFixture, ctx context.Context) (*PlayCoordinator, error) {
	t.Helper()
	if ctx == nil {
		ctx = f.ctx
	}
	return NewService(ctx, f.catalog, f.inventory, f.storage, f.jobs, f.restore, f.playback)
}

func (f *coordinatorFixture) start(t *testing.T, identity providers.ExternalWorkIdentity) Accepted {
	t.Helper()
	accepted, err := f.coordinator.Start(Intent{WorkIdentity: identity, Edition: testIntent().Edition})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	return accepted
}

func (f *coordinatorFixture) waitTerminal(t *testing.T, id string) jobs.Job {
	t.Helper()
	return f.waitForJob(t, id, func(job jobs.Job) bool {
		return job.Status == jobs.StatusSucceeded || job.Status == jobs.StatusFailed || job.Status == jobs.StatusInterrupted || job.Status == jobs.StatusCancelled
	})
}

func (f *coordinatorFixture) waitForJob(t *testing.T, id string, predicate func(jobs.Job) bool) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, err := f.jobs.Get(context.Background(), id)
		if err == nil && predicate(job) {
			return job
		}
		time.Sleep(time.Millisecond)
	}
	job, err := f.jobs.Get(context.Background(), id)
	t.Fatalf("timed out waiting for Job %s: current=%#v err=%v", id, job, err)
	return jobs.Job{}
}

func (f *coordinatorFixture) sessionCount(t *testing.T) int {
	t.Helper()
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func (f *coordinatorFixture) session(t *testing.T, id string) domain.Session {
	t.Helper()
	session, err := f.sessions.Get(context.Background(), domain.SessionID(id))
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func (f *coordinatorFixture) assertLocations(t *testing.T, local bool) {
	t.Helper()
	graph, err := f.base.GetWorkByExternalIdentity(context.Background(), testIdentity().Provider, testIdentity().ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	archives, locals := 0, 0
	for _, location := range graph.Locations {
		switch location.LocationClass {
		case "archive":
			archives++
		case "local_cache":
			locals++
		}
	}
	if archives != 1 || (local && locals != 1) || (!local && locals != 0) {
		t.Fatalf("catalog locations archive/local = %d/%d, want 1/%v: %#v", archives, locals, local, graph.Locations)
	}
}

func testIdentity() providers.ExternalWorkIdentity {
	return providers.ExternalWorkIdentity{Provider: "igdb", ExternalID: "1565"}
}

func testIntent() Intent {
	return Intent{WorkIdentity: testIdentity(), Edition: EditionIntent{Platform: "gamecube", Format: "disc_image"}}
}

func testWorkID() domain.WorkID       { return "work-local-1" }
func testEditionID() domain.EditionID { return "edition-gc-1" }
func testAssetID() domain.AssetID     { return "asset-gc-1" }
func testPartID() domain.AssetPartID  { return "part-gc-1" }

func testArchiveLocation() domain.AssetLocation {
	return domain.AssetLocation{ID: "location-archive-1", AssetID: testAssetID(), StorageProviderID: "rclone",
		Locator: "games:gamecube/game.iso", LocationClass: "archive"}
}

func testGraph() catalog.WorkGraph {
	return catalog.WorkGraph{
		Work:               domain.Work{ID: testWorkID(), Medium: domain.MediumGame, WorkType: "game", Title: "Display Only"},
		Editions:           []domain.Edition{{ID: testEditionID(), WorkID: testWorkID(), Platform: "gamecube", Format: "disc_image"}},
		Assets:             []domain.Asset{{ID: testAssetID(), EditionID: testEditionID(), Kind: "disc_image", TotalSizeBytes: 4}},
		Parts:              []domain.AssetPart{{ID: testPartID(), AssetID: testAssetID(), Role: "rom", Filename: "game.iso", SizeBytes: 4}},
		Locations:          []domain.AssetLocation{testArchiveLocation()},
		ExternalIdentities: []domain.ExternalIdentity{{ID: "identity-1", WorkID: testWorkID(), Provider: testIdentity().Provider, ExternalID: testIdentity().ExternalID}},
	}
}

func testMatch() providers.InventoryMatch {
	return providers.InventoryMatch{AssetID: testAssetID(), WorkTitle: "Never Used For Matching", Platform: "gamecube", Format: "disc_image", TotalSizeBytes: 4,
		Parts: []providers.InventoryPart{{Role: "rom", Filename: "game.iso", SizeBytes: 4}}}
}

func testRestoreRequest(jobID string) agent.RestoreAsset {
	asset := testGraph().Assets[0]
	return agent.RestoreAsset{JobID: jobID, Asset: asset, Parts: []domain.AssetPart{testGraph().Parts[0]}, SourceLocation: testArchiveLocation()}
}

func testRestoreResult(request agent.RestoreAsset) agent.RestoreResult {
	return agent.RestoreResult{AssetID: request.Asset.ID, LocationClass: "local_cache", RelativePath: "assets/asset-gc-1/game.iso",
		VerifiedSizeBytes: request.Asset.TotalSizeBytes, VerifiedAt: time.Now().UTC(), LocalReady: true}
}

func intPtr(value int) *int { return &value }

type inventoryStub struct {
	mu      sync.Mutex
	matches []providers.InventoryMatch
	err     error
	calls   int
}

func (source *inventoryStub) AssetsForEdition(context.Context, domain.EditionID) ([]domain.Asset, error) {
	return nil, nil
}
func (source *inventoryStub) FindMatchingAssets(_ context.Context, query providers.InventoryQuery) ([]providers.InventoryMatch, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.calls++
	if query.WorkIdentity != testIdentity() || query.Platform != "gamecube" || query.Format != "disc_image" {
		return nil, fmt.Errorf("unexpected inventory query: %#v", query)
	}
	return append([]providers.InventoryMatch(nil), source.matches...), source.err
}
func (source *inventoryStub) setMatches(matches []providers.InventoryMatch) {
	source.mu.Lock()
	source.matches = matches
	source.mu.Unlock()
}

type storageStub struct {
	mu        sync.Mutex
	locations []domain.AssetLocation
	err       error
}

func (source *storageStub) LocationsForAsset(_ context.Context, id domain.AssetID) ([]domain.AssetLocation, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	if id != testAssetID() {
		return nil, fmt.Errorf("unexpected storage Asset: %s", id)
	}
	return append([]domain.AssetLocation(nil), source.locations...), source.err
}
func (source *storageStub) setLocations(locations []domain.AssetLocation) {
	source.mu.Lock()
	source.locations = locations
	source.mu.Unlock()
}

type catalogSpy struct {
	*catalog.SQLiteRepository
	mu               sync.Mutex
	ensures          int
	removals         int
	removedLocations []domain.AssetLocation
}

func (repository *catalogSpy) EnsureLocalCacheLocation(ctx context.Context, request agent.RestoreAsset, result agent.RestoreResult) (domain.AssetLocation, error) {
	repository.mu.Lock()
	repository.ensures++
	repository.mu.Unlock()
	return repository.SQLiteRepository.EnsureLocalCacheLocation(ctx, request, result)
}
func (repository *catalogSpy) RemoveLocalCacheLocation(ctx context.Context, location domain.AssetLocation) (int64, error) {
	repository.mu.Lock()
	repository.removals++
	repository.removedLocations = append(repository.removedLocations, location)
	repository.mu.Unlock()
	return repository.SQLiteRepository.RemoveLocalCacheLocation(ctx, location)
}
func (repository *catalogSpy) ensureCount() int {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return repository.ensures
}
func (repository *catalogSpy) removeCount() int {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return repository.removals
}

func (repository *catalogSpy) removedLocationSnapshot() []domain.AssetLocation {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return append([]domain.AssetLocation(nil), repository.removedLocations...)
}

type restoreStub struct {
	mu            sync.Mutex
	calls         int
	fail          bool
	invalid       bool
	block         bool
	waitForCancel bool
	entered       chan struct{}
}

func (restore *restoreStub) RestoreAsset(ctx context.Context, request agent.RestoreAsset, report func(agent.RestoreProgress)) (agent.RestoreResult, error) {
	restore.mu.Lock()
	restore.calls++
	fail, invalid, block, waitForCancel := restore.fail, restore.invalid, restore.block, restore.waitForCancel
	restore.mu.Unlock()
	restore.entered <- struct{}{}
	if block {
		select {
		case <-ctx.Done():
			return agent.RestoreResult{}, ctx.Err()
		case <-time.After(2 * time.Second):
			return agent.RestoreResult{}, errors.New("restore test gate timed out")
		}
	}
	if waitForCancel {
		<-ctx.Done()
		return agent.RestoreResult{}, ctx.Err()
	}
	if fail {
		return agent.RestoreResult{}, errors.New("safe fixture restore failure")
	}
	for _, progress := range []agent.RestoreProgress{
		{Phase: agent.RestorePhaseValidating, TotalBytes: 4},
		{Phase: agent.RestorePhaseStaging, TotalBytes: 4},
		{Phase: agent.RestorePhaseCopying, TotalBytes: 4},
		{Phase: agent.RestorePhaseComplete, CurrentBytes: 4, TotalBytes: 4},
	} {
		report(progress)
	}
	result := testRestoreResult(request)
	if invalid {
		result.RelativePath = "../outside.iso"
	}
	return result, nil
}
func (restore *restoreStub) count() int {
	restore.mu.Lock()
	defer restore.mu.Unlock()
	return restore.calls
}

type launchStub struct {
	mu                      sync.Mutex
	calls                   int
	request                 []agent.LaunchAsset
	errors                  []error
	outcome                 domain.SessionOutcome
	exitCode                *int
	terminalOnRejectedStart bool
	beforeStart             <-chan struct{}
	terminalGate            <-chan struct{}
	entered                 chan struct{}
	terminalEntered         chan struct{}
}

func (launch *launchStub) LaunchAsset(ctx context.Context, request agent.LaunchAsset, onStarted func(agent.LaunchStarted) bool) (agent.LaunchEnded, error) {
	launch.mu.Lock()
	launch.calls++
	call := launch.calls
	launch.request = append(launch.request, request)
	var planned error
	if len(launch.errors) > 0 {
		planned = launch.errors[0]
		launch.errors = launch.errors[1:]
	}
	beforeStart, terminalGate := launch.beforeStart, launch.terminalGate
	outcome, code := launch.outcome, launch.exitCode
	terminalOnRejectedStart := launch.terminalOnRejectedStart
	launch.mu.Unlock()
	launch.entered <- struct{}{}
	if planned != nil {
		return agent.LaunchEnded{}, planned
	}
	if beforeStart != nil {
		select {
		case <-ctx.Done():
			return agent.LaunchEnded{}, ctx.Err()
		case <-beforeStart:
		}
	}
	started := agent.LaunchStarted{SessionID: request.SessionID, WorkID: request.WorkID, EditionID: request.EditionID,
		AssetID: request.AssetID, StartedAt: time.Now().UTC()}
	if onStarted == nil || !onStarted(started) {
		if terminalOnRejectedStart {
			terminalExitCode := code
			if outcome == domain.SessionOutcomeInterrupted {
				terminalExitCode = nil
			}
			return agent.LaunchEnded{SessionID: request.SessionID, WorkID: request.WorkID, EditionID: request.EditionID,
				AssetID: request.AssetID, EndedAt: time.Now().UTC(), Outcome: outcome, ExitCode: terminalExitCode}, nil
		}
		return agent.LaunchEnded{}, agent.ErrLaunchNotPersisted
	}
	if terminalGate != nil {
		launch.terminalEntered <- struct{}{}
		select {
		case <-ctx.Done():
			return agent.LaunchEnded{SessionID: request.SessionID, WorkID: request.WorkID, EditionID: request.EditionID,
				AssetID: request.AssetID, EndedAt: time.Now().UTC(), Outcome: domain.SessionOutcomeInterrupted}, ctx.Err()
		case <-terminalGate:
		}
	}
	ended := agent.LaunchEnded{SessionID: request.SessionID, WorkID: request.WorkID, EditionID: request.EditionID,
		AssetID: request.AssetID, EndedAt: time.Now().UTC(), Outcome: outcome, ExitCode: code}
	_ = call
	return ended, nil
}

func (launch *launchStub) setErrors(errs ...error) {
	launch.mu.Lock()
	launch.errors = append([]error(nil), errs...)
	launch.mu.Unlock()
}
func (launch *launchStub) count() int {
	launch.mu.Lock()
	defer launch.mu.Unlock()
	return launch.calls
}
