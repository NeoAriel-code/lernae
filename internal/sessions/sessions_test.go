package sessions_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lernae/internal/catalog"
	"lernae/internal/database"
	"lernae/internal/domain"
	"lernae/internal/sessions"
)

func TestSessionPersistsAcrossDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lernae.db")
	db, service := openSessionService(t, path)
	startedAt := time.Date(2026, 9, 26, 12, 0, 0, 0, time.FixedZone("UTC-4", -4*60*60))
	session, err := service.Start(ctx, domain.WorkID("work-1"), domain.EditionID("edition-1"), domain.AssetID("asset-1"), startedAt)
	if err != nil {
		t.Fatal(err)
	}
	endedAt := startedAt.Add(37 * time.Second)
	if err := service.Finish(ctx, session.ID, domain.SessionOutcomeNormalExit, endedAt); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = database.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	loaded, err := sessions.NewService(sessions.NewSQLiteRepository(db)).Get(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != session.ID || loaded.WorkID != domain.WorkID("work-1") ||
		loaded.EditionID != domain.EditionID("edition-1") || loaded.AssetID != domain.AssetID("asset-1") {
		t.Fatalf("reopened Session identity = %#v", loaded)
	}
	if loaded.StartedAt != startedAt.UTC() || loaded.EndedAt == nil || !loaded.EndedAt.Equal(endedAt) ||
		loaded.Outcome != domain.SessionOutcomeNormalExit {
		t.Fatalf("reopened Session lifecycle = %#v", loaded)
	}
}

func TestStartCreatesActiveSessionWithOpaqueIDAndAssociations(t *testing.T) {
	ctx := context.Background()
	_, service := openSessionService(t, filepath.Join(t.TempDir(), "lernae.db"))
	startedAt := time.Date(2026, 9, 26, 16, 0, 0, 123, time.UTC)

	created, err := service.Start(ctx, "work-1", "edition-1", "asset-1", startedAt)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := service.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID == "" || loaded.ID == domain.SessionID("work-1") {
		t.Fatalf("Session ID is not opaque: %q", loaded.ID)
	}
	if loaded.WorkID != "work-1" || loaded.EditionID != "edition-1" || loaded.AssetID != "asset-1" {
		t.Fatalf("Session associations = work:%q edition:%q asset:%q", loaded.WorkID, loaded.EditionID, loaded.AssetID)
	}
	if loaded.StartedAt != startedAt || loaded.EndedAt != nil || loaded.Outcome != "" {
		t.Fatalf("new Session is not active: %#v", loaded)
	}
}

func TestPreallocatedSessionIDIsNotPersistedUntilConfirmedStart(t *testing.T) {
	ctx := context.Background()
	_, service := openSessionService(t, filepath.Join(t.TempDir(), "lernae.db"))
	id, err := service.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(ctx, id); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("preallocated Session ID lookup error = %v, want ErrNotFound", err)
	}
	startedAt := time.Date(2026, 9, 26, 16, 30, 0, 0, time.UTC)
	created, err := service.StartWithID(ctx, id, "work-1", "edition-1", "asset-1", startedAt)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != id || created.EndedAt != nil || created.Outcome != "" {
		t.Fatalf("Session started with preallocated ID = %#v", created)
	}
}

func TestSessionFinalizesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	_, service := openSessionService(t, filepath.Join(t.TempDir(), "lernae.db"))
	startedAt := time.Date(2026, 9, 26, 16, 0, 0, 0, time.UTC)
	session, err := service.Start(ctx, "work-1", "edition-1", "asset-1", startedAt)
	if err != nil {
		t.Fatal(err)
	}
	endedAt := startedAt.Add(time.Second)
	if err := service.Finish(ctx, session.ID, domain.SessionOutcomeNonZeroExit, endedAt); err != nil {
		t.Fatal(err)
	}
	if err := service.Finish(ctx, session.ID, domain.SessionOutcomeInterrupted, endedAt.Add(time.Second)); !errors.Is(err, sessions.ErrInvalidTransition) {
		t.Fatalf("repeated Session finalization error = %v, want ErrInvalidTransition", err)
	}
	loaded, err := service.Get(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.EndedAt == nil || !loaded.EndedAt.Equal(endedAt) || loaded.Outcome != domain.SessionOutcomeNonZeroExit {
		t.Fatalf("repeated finalization changed terminal Session: %#v", loaded)
	}
}

func TestSessionSupportsEveryTerminalOutcome(t *testing.T) {
	tests := []struct {
		name    string
		outcome domain.SessionOutcome
	}{
		{name: "normal exit", outcome: domain.SessionOutcomeNormalExit},
		{name: "non-zero exit", outcome: domain.SessionOutcomeNonZeroExit},
		{name: "interruption", outcome: domain.SessionOutcomeInterrupted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			_, service := openSessionService(t, filepath.Join(t.TempDir(), "lernae.db"))
			startedAt := time.Date(2026, 9, 26, 16, 0, 0, 0, time.UTC)
			session, err := service.Start(ctx, "work-1", "edition-1", "asset-1", startedAt)
			if err != nil {
				t.Fatal(err)
			}
			endedAt := startedAt.Add(time.Second)
			if err := service.Finish(ctx, session.ID, tt.outcome, endedAt); err != nil {
				t.Fatal(err)
			}
			loaded, err := service.Get(ctx, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Outcome != tt.outcome || loaded.EndedAt == nil || !loaded.EndedAt.Equal(endedAt) {
				t.Fatalf("terminal Session = %#v, want outcome %q at %s", loaded, tt.outcome, endedAt)
			}
		})
	}
}

func TestSessionPersistsAndComparesTimestampPrecisionChronologically(t *testing.T) {
	base := time.Date(2026, 9, 26, 16, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		startedAt time.Time
		endedAt   time.Time
	}{
		{name: "whole seconds", startedAt: base, endedAt: base.Add(time.Second)},
		{name: "subsecond fractions", startedAt: base.Add(100 * time.Millisecond), endedAt: base.Add(110 * time.Millisecond)},
		{name: "nanosecond fractions", startedAt: base.Add(time.Nanosecond), endedAt: base.Add(2 * time.Nanosecond)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.endedAt.After(tt.startedAt) {
				t.Fatal("test end time must be chronologically after its start")
			}
			ctx := context.Background()
			_, service := openSessionService(t, filepath.Join(t.TempDir(), "lernae.db"))
			session, err := service.Start(ctx, "work-1", "edition-1", "asset-1", tt.startedAt)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Finish(ctx, session.ID, domain.SessionOutcomeNormalExit, tt.endedAt); err != nil {
				t.Fatalf("Finish(%s, %s): %v", tt.startedAt.Format(time.RFC3339Nano), tt.endedAt.Format(time.RFC3339Nano), err)
			}
			loaded, err := service.Get(ctx, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !loaded.StartedAt.Equal(tt.startedAt) || loaded.EndedAt == nil || !loaded.EndedAt.Equal(tt.endedAt) {
				t.Fatalf("loaded Session times = %s to %v, want %s to %s", loaded.StartedAt, loaded.EndedAt,
					tt.startedAt, tt.endedAt)
			}
		})
	}
}

func TestSessionReconciliationOrdersSubsecondTimestampsChronologically(t *testing.T) {
	ctx := context.Background()
	_, service := openSessionService(t, filepath.Join(t.TempDir(), "lernae.db"))
	startedAt := time.Date(2026, 9, 26, 16, 0, 0, 100_000_000, time.UTC)
	reconciledAt := startedAt.Add(10 * time.Millisecond)
	if !reconciledAt.After(startedAt) {
		t.Fatal("reconciliation time must be chronologically after Session start")
	}
	session, err := service.Start(ctx, "work-1", "edition-1", "asset-1", startedAt)
	if err != nil {
		t.Fatal(err)
	}
	count, err := service.ReconcileStale(ctx, reconciledAt)
	if err != nil {
		t.Fatalf("ReconcileStale(%s) for start %s: %v", reconciledAt.Format(time.RFC3339Nano),
			startedAt.Format(time.RFC3339Nano), err)
	}
	if count != 1 {
		t.Fatalf("reconciled %d Sessions, want 1", count)
	}
	loaded, err := service.Get(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Outcome != domain.SessionOutcomeInterrupted || loaded.EndedAt == nil || !loaded.EndedAt.Equal(reconciledAt) {
		t.Fatalf("reconciled Session = %#v", loaded)
	}
}

func TestSessionSchemaRejectsEndBeforeStart(t *testing.T) {
	ctx := context.Background()
	db, _ := openSessionService(t, filepath.Join(t.TempDir(), "lernae.db"))
	_, err := db.ExecContext(ctx, `INSERT INTO sessions
		(id, work_id, edition_id, asset_id, started_at_utc, ended_at_utc, outcome)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"session-invalid-order", "work-1", "edition-1", "asset-1",
		"2026-09-26T16:00:00.110000000Z", "2026-09-26T16:00:00.100000000Z", domain.SessionOutcomeNormalExit)
	if err == nil {
		t.Fatal("Session schema accepted an end timestamp before start")
	}
	if !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("end-before-start was rejected for a reason other than the schema CHECK: %v", err)
	}
}

func TestSessionSchemaRejectsNoncanonicalFractionWidth(t *testing.T) {
	ctx := context.Background()
	db, _ := openSessionService(t, filepath.Join(t.TempDir(), "lernae.db"))
	_, err := db.ExecContext(ctx, `INSERT INTO sessions
		(id, work_id, edition_id, asset_id, started_at_utc, ended_at_utc, outcome)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"session-noncanonical-order", "work-1", "edition-1", "asset-1",
		"2026-09-26T16:00:00.11Z", "2026-09-26T16:00:00.1Z", domain.SessionOutcomeNormalExit)
	if err == nil {
		t.Fatal("Session schema accepted noncanonical timestamps whose lexical order hides end-before-start")
	}
	if !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("noncanonical timestamps were rejected for a reason other than the schema CHECK: %v", err)
	}
}

func TestSessionRejectsInvalidTimestampsWithoutChangingState(t *testing.T) {
	ctx := context.Background()
	db, service := openSessionService(t, filepath.Join(t.TempDir(), "lernae.db"))
	if _, err := service.Start(ctx, "work-1", "edition-1", "asset-1", time.Time{}); !errors.Is(err, sessions.ErrInvalidTimestamp) {
		t.Fatalf("zero Session start time error = %v, want ErrInvalidTimestamp", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalid start created %d Session rows", count)
	}

	startedAt := time.Date(2026, 9, 26, 16, 0, 0, 0, time.UTC)
	session, err := service.Start(ctx, "work-1", "edition-1", "asset-1", startedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Finish(ctx, session.ID, domain.SessionOutcomeNormalExit, startedAt.Add(-time.Nanosecond)); !errors.Is(err, sessions.ErrInvalidTimestamp) {
		t.Fatalf("end-before-start error = %v, want ErrInvalidTimestamp", err)
	}
	loaded, err := service.Get(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.EndedAt != nil || loaded.Outcome != "" {
		t.Fatalf("invalid finalization changed Session: %#v", loaded)
	}
}

func TestSessionReconciliationInterruptsOnlyStaleActiveRows(t *testing.T) {
	ctx := context.Background()
	_, service := openSessionService(t, filepath.Join(t.TempDir(), "lernae.db"))
	startedAt := time.Date(2026, 9, 26, 16, 0, 0, 0, time.UTC)
	first, err := service.Start(ctx, "work-1", "edition-1", "asset-1", startedAt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Start(ctx, "work-1", "edition-1", "asset-1", startedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	completed, err := service.Start(ctx, "work-1", "edition-1", "asset-1", startedAt.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	completedAt := startedAt.Add(3 * time.Second)
	if err := service.Finish(ctx, completed.ID, domain.SessionOutcomeNormalExit, completedAt); err != nil {
		t.Fatal(err)
	}
	reconciledAt := startedAt.Add(4 * time.Second)
	count, err := service.ReconcileStale(ctx, reconciledAt)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("reconciled %d Sessions, want 2", count)
	}
	for _, id := range []domain.SessionID{first.ID, second.ID} {
		loaded, err := service.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Outcome != domain.SessionOutcomeInterrupted || loaded.EndedAt == nil || !loaded.EndedAt.Equal(reconciledAt) {
			t.Errorf("reconciled Session %s = %#v", id, loaded)
		}
	}
	loaded, err := service.Get(ctx, completed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Outcome != domain.SessionOutcomeNormalExit || loaded.EndedAt == nil || !loaded.EndedAt.Equal(completedAt) {
		t.Fatalf("reconciliation changed completed Session: %#v", loaded)
	}
	count, err = service.ReconcileStale(ctx, reconciledAt.Add(time.Second))
	if err != nil || count != 0 {
		t.Fatalf("second reconciliation = %d, %v; want 0, nil", count, err)
	}
}

func TestSessionReconciliationRejectsTimestampBeforeAnyActiveStart(t *testing.T) {
	ctx := context.Background()
	_, service := openSessionService(t, filepath.Join(t.TempDir(), "lernae.db"))
	startedAt := time.Date(2026, 9, 26, 16, 0, 0, 0, time.UTC)
	session, err := service.Start(ctx, "work-1", "edition-1", "asset-1", startedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReconcileStale(ctx, startedAt.Add(-time.Second)); !errors.Is(err, sessions.ErrInvalidTimestamp) {
		t.Fatalf("reconciliation with earlier timestamp error = %v, want ErrInvalidTimestamp", err)
	}
	loaded, err := service.Get(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.EndedAt != nil || loaded.Outcome != "" {
		t.Fatalf("invalid reconciliation changed Session: %#v", loaded)
	}
}

func openSessionService(t *testing.T, path string) (*sql.DB, *sessions.Service) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	graph := catalog.WorkGraph{
		Work:     domain.Work{ID: "work-1", Medium: domain.MediumGame, WorkType: "game", Title: "Test Game"},
		Editions: []domain.Edition{{ID: "edition-1", WorkID: "work-1", Platform: "gamecube", Format: "disc_image"}},
		Assets:   []domain.Asset{{ID: "asset-1", EditionID: "edition-1", Kind: "disc_image"}},
	}
	if err := catalog.NewSQLiteRepository(db).CreateGraph(ctx, graph); err != nil {
		t.Fatal(err)
	}
	return db, sessions.NewService(sessions.NewSQLiteRepository(db))
}

func TestSessionServiceSupportsConcurrentFinalization(t *testing.T) {
	ctx := context.Background()
	_, service := openSessionService(t, filepath.Join(t.TempDir(), "lernae.db"))
	startedAt := time.Date(2026, 9, 26, 16, 0, 0, 0, time.UTC)
	session, err := service.Start(ctx, "work-1", "edition-1", "asset-1", startedAt)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	var mu sync.Mutex
	successes, invalidTransitions := 0, 0
	for _, outcome := range []domain.SessionOutcome{domain.SessionOutcomeNormalExit, domain.SessionOutcomeInterrupted} {
		wait.Add(1)
		go func(outcome domain.SessionOutcome) {
			defer wait.Done()
			err := service.Finish(ctx, session.ID, outcome, startedAt.Add(time.Second))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, sessions.ErrInvalidTransition):
				invalidTransitions++
			default:
				t.Errorf("concurrent finalization error = %v", err)
			}
		}(outcome)
	}
	wait.Wait()
	if successes != 1 || invalidTransitions != 1 {
		t.Fatalf("concurrent finalizations = %d success, %d invalid, want 1 each", successes, invalidTransitions)
	}
}
