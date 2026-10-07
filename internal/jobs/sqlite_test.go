package jobs

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"lernae/internal/database"
	"lernae/internal/domain"
	"lernae/internal/sessions"
)

func startPersistedTestSession(t *testing.T, db *sql.DB, id string) *sessions.Service {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`INSERT OR IGNORE INTO works (id, medium, work_type, title) VALUES ('work-1', 'game', 'video_game', 'Fixture')`,
		`INSERT OR IGNORE INTO editions (id, work_id, format) VALUES ('edition-1', 'work-1', 'disc_image')`,
		`INSERT OR IGNORE INTO assets (id, edition_id, kind, total_size_bytes) VALUES ('asset-1', 'edition-1', 'disc_image', 4)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	service := sessions.NewService(sessions.NewSQLiteRepository(db))
	if _, err := service.StartWithID(ctx, domain.SessionID(id), "work-1", "edition-1", "asset-1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return service
}

func TestJobsPersistAcrossDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lernae.db")
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(NewSQLiteRepository(db))
	created, err := service.Create(ctx, "foundation-check")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	loaded, err := NewService(NewSQLiteRepository(db)).Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != created.ID || loaded.Kind != "foundation-check" || loaded.Status != StatusQueued {
		t.Fatalf("persisted job = %#v", loaded)
	}
}

func TestQueuedJobCanBeCancelledWithSafeError(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := NewService(NewSQLiteRepository(db))
	created, err := service.Create(ctx, KindPlay)
	if err != nil {
		t.Fatal(err)
	}

	if err := service.TransitionWithError(ctx, created.ID, StatusCancelled, "job_start_failed", "PLAY could not start safely"); err != nil {
		t.Fatalf("cancel queued Job with a safe startup error: %v", err)
	}
	loaded, err := service.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != StatusCancelled || loaded.ErrorCode != "job_start_failed" || loaded.ErrorDetail != "PLAY could not start safely" {
		t.Fatalf("cancelled queued Job = %#v, want bounded cancellation error", loaded)
	}
}

func TestPlayJobPhaseProgressAndSessionPersistAcrossDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lernae.db")
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(NewSQLiteRepository(db))
	created, err := service.Create(ctx, KindPlay)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, created.ID, StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePlayPhase(ctx, created.ID, PhaseRestore); err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, created.ID, StatusWaitingOnAgent); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePlayProgress(ctx, created.ID, 3, 4); err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, created.ID, StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePlayPhase(ctx, created.ID, PhaseLaunch); err != nil {
		t.Fatal(err)
	}
	startPersistedTestSession(t, db, "session-1")
	if err := service.AttachPlaySession(ctx, created.ID, "session-1"); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePlayPhase(ctx, created.ID, PhasePlaying); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = database.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopening database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	loaded, err := NewService(NewSQLiteRepository(db)).Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Kind != KindPlay || loaded.Phase != PhasePlaying || loaded.SessionID != "session-1" ||
		loaded.ProgressCurrent != 3 || loaded.ProgressTotal != 4 || loaded.Status != StatusRunning {
		t.Fatalf("reopened PLAY Job = %#v", loaded)
	}
}

func TestPlayJobUpdatesRemainNonterminalAndFreezeRestoreProgress(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := NewService(NewSQLiteRepository(db))
	job, err := service.Create(ctx, KindPlay)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, job.ID, StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePlayPhase(ctx, job.ID, PhaseRestore); err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, job.ID, StatusWaitingOnAgent); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePlayProgress(ctx, job.ID, 4, 4); err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, job.ID, StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePlayPhase(ctx, job.ID, PhaseLaunch); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePlayPhase(ctx, job.ID, PhasePlaying); !errors.Is(err, ErrInvalidSessionReference) {
		t.Fatalf("playing before attaching a Session error = %v, want ErrInvalidSessionReference", err)
	}
	startPersistedTestSession(t, db, "session-1")
	if err := service.AttachPlaySession(ctx, job.ID, "session-1"); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePlayPhase(ctx, job.ID, PhasePlaying); err != nil {
		t.Fatal(err)
	}
	loaded, err := service.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != StatusRunning || loaded.Phase != PhasePlaying || loaded.ProgressCurrent != 4 || loaded.ProgressTotal != 4 {
		t.Fatalf("PLAY Job after restore, launch, and playing updates = %#v; want active with frozen 4/4 restore progress", loaded)
	}
	if err := service.UpdatePlayPhase(ctx, job.ID, PhaseRestore); !errors.Is(err, ErrInvalidPhase) {
		t.Errorf("phase regression error = %v, want ErrInvalidPhase", err)
	}
	if err := service.UpdatePlayProgress(ctx, job.ID, 4, 4); !errors.Is(err, ErrInvalidProgress) {
		t.Errorf("post-restore progress update error = %v, want ErrInvalidProgress", err)
	}
	if err := service.AttachPlaySession(ctx, job.ID, "session-2"); !errors.Is(err, ErrInvalidSessionReference) {
		t.Errorf("second Session attachment error = %v, want ErrInvalidSessionReference", err)
	}
	if err := service.UpdatePlayPhase(ctx, job.ID, Phase("complete")); !errors.Is(err, ErrInvalidPhase) {
		t.Errorf("unknown phase error = %v, want ErrInvalidPhase", err)
	}
}

func TestPlayJobCannotSucceedBeforePlayingAndTerminalSessionEvidence(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := NewService(NewSQLiteRepository(db))

	beforeLaunch, err := service.Create(ctx, KindPlay)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, beforeLaunch.ID, StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, beforeLaunch.ID, StatusSucceeded); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("PLAY success before launch = %v, want ErrInvalidTransition", err)
	}

	job, err := service.Create(ctx, KindPlay)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, job.ID, StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePlayPhase(ctx, job.ID, PhaseLaunch); err != nil {
		t.Fatal(err)
	}
	sessionService := startPersistedTestSession(t, db, "session-terminal-proof")
	if err := service.AttachPlaySession(ctx, job.ID, "session-terminal-proof"); err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, job.ID, StatusSucceeded); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("PLAY success before playing phase = %v, want ErrInvalidTransition", err)
	}
	if err := service.UpdatePlayPhase(ctx, job.ID, PhasePlaying); err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, job.ID, StatusSucceeded); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("PLAY success while Session is active = %v, want ErrInvalidTransition", err)
	}
	if err := sessionService.Finish(ctx, domain.SessionID("session-terminal-proof"), domain.SessionOutcomeNormalExit, time.Now().UTC().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, job.ID, StatusSucceeded); err != nil {
		t.Fatalf("PLAY success after persisted terminal Session = %v", err)
	}
}

func TestPlayJobAttachesOnlyPersistedStartedSessions(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := NewService(NewSQLiteRepository(db))
	job, err := service.Create(ctx, KindPlay)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, job.ID, StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePlayPhase(ctx, job.ID, PhaseLaunch); err != nil {
		t.Fatal(err)
	}
	sessionService := sessions.NewService(sessions.NewSQLiteRepository(db))
	unconfirmedID, err := sessionService.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if err := service.AttachPlaySession(ctx, job.ID, string(unconfirmedID)); !errors.Is(err, ErrInvalidSessionReference) {
		t.Errorf("preallocated but unpersisted Session attachment = %v, want ErrInvalidSessionReference", err)
	}
	if err := service.AttachPlaySession(ctx, job.ID, "session/not-valid"); !errors.Is(err, ErrInvalidSessionReference) {
		t.Errorf("malformed Session attachment = %v, want ErrInvalidSessionReference", err)
	}
	startPersistedTestSession(t, db, "session-started")
	if err := service.AttachPlaySession(ctx, job.ID, "session-started"); err != nil {
		t.Fatalf("persisted process-started Session attachment = %v", err)
	}
}

func TestPlayRestoreCannotAdvanceToLaunchWhileWaitingOnAgent(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := NewService(NewSQLiteRepository(db))
	job, err := service.Create(ctx, KindPlay)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, job.ID, StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePlayPhase(ctx, job.ID, PhaseRestore); err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, job.ID, StatusWaitingOnAgent); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePlayPhase(ctx, job.ID, PhaseLaunch); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("restore -> launch while waiting_on_agent = %v, want ErrInvalidTransition", err)
	}
	if err := service.Transition(ctx, job.ID, StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePlayPhase(ctx, job.ID, PhaseLaunch); err != nil {
		t.Fatalf("restore -> launch after returning to running = %v", err)
	}
}

func TestPlayJobUpdatesRejectWrongKindsAndInactiveStates(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := NewService(NewSQLiteRepository(db))

	wrongKind, err := service.Create(ctx, KindRestoreAsset)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, wrongKind.ID, StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePlayPhase(ctx, wrongKind.ID, PhaseRestore); !errors.Is(err, ErrInvalidKind) {
		t.Errorf("non-PLAY phase update error = %v, want ErrInvalidKind", err)
	}
	if err := service.AttachPlaySession(ctx, wrongKind.ID, "session-1"); !errors.Is(err, ErrInvalidKind) {
		t.Errorf("non-PLAY Session update error = %v, want ErrInvalidKind", err)
	}
	if err := service.UpdatePlayProgress(ctx, wrongKind.ID, 1, 1); !errors.Is(err, ErrInvalidKind) {
		t.Errorf("non-PLAY progress update error = %v, want ErrInvalidKind", err)
	}

	queued, err := service.Create(ctx, KindPlay)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePlayPhase(ctx, queued.ID, PhaseRestore); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("queued PLAY phase update error = %v, want ErrInvalidTransition", err)
	}
	if err := service.AttachPlaySession(ctx, queued.ID, "session-1"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("queued PLAY Session update error = %v, want ErrInvalidTransition", err)
	}

	terminal, err := service.Create(ctx, KindPlay)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, terminal.ID, StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, terminal.ID, StatusFailed); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePlayPhase(ctx, terminal.ID, PhaseLaunch); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("terminal PLAY phase update error = %v, want ErrInvalidTransition", err)
	}
	if err := service.AttachPlaySession(ctx, terminal.ID, "session-1"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("terminal PLAY Session update error = %v, want ErrInvalidTransition", err)
	}
}

func TestStartupReconciliationInterruptsTransientJobs(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := NewService(NewSQLiteRepository(db))

	queued, err := service.Create(ctx, "queued-job")
	if err != nil {
		t.Fatal(err)
	}
	running, err := service.Create(ctx, "running-job")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, running.ID, StatusRunning); err != nil {
		t.Fatal(err)
	}
	waiting, err := service.Create(ctx, "waiting-job")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, waiting.ID, StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, waiting.ID, StatusWaitingOnAgent); err != nil {
		t.Fatal(err)
	}

	count, err := service.ReconcileStartup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("reconciled %d jobs, want 2", count)
	}
	for id, want := range map[string]Status{queued.ID: StatusQueued, running.ID: StatusInterrupted, waiting.ID: StatusInterrupted} {
		got, err := service.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != want {
			t.Errorf("job %s status = %q, want %q", id, got.Status, want)
		}
	}
}

func TestStartupReconciliationCancelsOrphanedQueuedPlayButPreservesQueuedRestore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "queued-play-restart.db")
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	initial := NewService(NewSQLiteRepository(db))
	play, err := initial.Create(ctx, KindPlay)
	if err != nil {
		t.Fatal(err)
	}
	restore, err := initial.Create(ctx, KindRestoreAsset)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	restarted := NewService(NewSQLiteRepository(db))
	count, err := restarted.ReconcileStartup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("startup reconciled %d Jobs, want only orphaned queued PLAY", count)
	}
	recoveredPlay, err := restarted.Get(ctx, play.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredPlay.Status != StatusCancelled || recoveredPlay.ErrorCode != "play_not_started" ||
		recoveredPlay.ErrorDetail != "PLAY was accepted but did not start before Server restart." {
		t.Fatalf("orphaned queued PLAY = %#v, want safely cancelled with bounded detail", recoveredPlay)
	}
	recoveredRestore, err := restarted.Get(ctx, restore.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredRestore.Status != StatusQueued {
		t.Fatalf("queued restore_asset Job status = %q, want unchanged queued semantics", recoveredRestore.Status)
	}
}

func TestJobTransitionsRejectTerminalMutation(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := NewService(NewSQLiteRepository(db))
	job, err := service.Create(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Transition(ctx, job.ID, StatusSucceeded); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("transition queued->succeeded error = %v", err)
	}
}
