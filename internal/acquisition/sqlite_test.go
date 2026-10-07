package acquisition

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"lernae/internal/jobs"
)

func TestDiscoveryIsNotPersistedAndSchemaIsMinimal(t *testing.T) {
	service, _, db, _, job := fixture(t, provider("source", option("accepted"), option("not-selected")))
	result, err := service.Discover(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var rows, tables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM acquisition_selections`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("discovery was persisted: rows=%d, %v", rows, err)
	}
	// P4-03 adds only permanent dispatch provenance beside the selection.
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name LIKE 'acquisition_%'`).Scan(&tables); err != nil || tables != 2 {
		t.Fatalf("excess acquisition persistence: tables=%d, %v", tables, err)
	}
	if _, err := service.Select(context.Background(), job.ID, result.Candidates[0].Handle); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM acquisition_selections`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("accepted selection rows=%d, %v", rows, err)
	}
	var privateRef string
	if err := db.QueryRow(`SELECT execution_ref FROM acquisition_selections WHERE job_id = ?`, job.ID).Scan(&privateRef); err != nil || privateRef != option("accepted").ExecutionRef {
		t.Fatalf("stored only accepted reference = %q, %v", privateRef, err)
	}
}

func TestSelectionDatabaseConstraintsAndImmutability(t *testing.T) {
	service, _, db, _, job := fixture(t, provider("source", option("a")))
	ctx := context.Background()
	result, err := service.Discover(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct {
		name   string
		change func(*Selection)
	}{
		{"wrong edition", func(s *Selection) { s.Candidate.EditionID = "other-edition" }},
		{"invalid provider", func(s *Selection) { s.Candidate.ProviderID = "UPPER" }},
		{"private URL", func(s *Selection) { s.Candidate.Option.ExecutionRef = "https://private" }},
		{"no timestamp", func(s *Selection) { s.SelectedAt = time.Time{} }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			selection := Selection{Candidate: result.Candidates[0], SelectedAt: time.Now().UTC()}
			mutation.change(&selection)
			if _, err := service.repository.Accept(ctx, selection); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("repository validation = %v", err)
			}
		})
	}
	// Exercise SQL constraints independently of Go validation.
	insert := `INSERT INTO acquisition_selections
		(job_id, provider_id, candidate_id, candidate_handle, execution_ref, title, label, language, selected_at_utc)
		VALUES (?, ?, ?, ?, ?, ?, '', '', '2026-01-01T00:00:00.000000000Z')`
	for _, test := range []struct {
		name        string
		jobID       string
		providerID  string
		candidateID string
		handle      string
		ref         string
		title       string
	}{
		{"missing FK", "missing", "source", "a", strings.Repeat("a", 64), "opaque", "Title"},
		{"provider shape", job.ID, "https://private", "a", strings.Repeat("a", 64), "opaque", "Title"},
		{"candidate shape", job.ID, "source", "bad:id", strings.Repeat("a", 64), "opaque", "Title"},
		{"handle shape", job.ID, "source", "a", "bad", "opaque", "Title"},
		{"reference bound", job.ID, "source", "a", strings.Repeat("a", 64), strings.Repeat("x", 513), "Title"},
		{"title bound", job.ID, "source", "a", strings.Repeat("a", 64), "opaque", strings.Repeat("x", 257)},
		{"empty title", job.ID, "source", "a", strings.Repeat("a", 64), "opaque", " "},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := db.Exec(insert, test.jobID, test.providerID, test.candidateID, test.handle, test.ref, test.title); err == nil {
				t.Fatal("SQL accepted invalid selection")
			}
		})
	}
	accepted, err := service.Select(ctx, job.ID, result.Candidates[0].Handle)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE acquisition_selections SET title = 'Changed' WHERE job_id = ?`,
		`DELETE FROM acquisition_selections WHERE job_id = ?`,
		`DELETE FROM jobs WHERE id = ?`,
	} {
		if _, err := db.Exec(query, job.ID); err == nil {
			t.Fatalf("immutable selection/association was modified: %s", query)
		}
	}
	altered := accepted
	altered.Candidate.Option.ExecutionRef = "DIFFERENT_PRIVATE_REF"
	if _, err := service.repository.Accept(ctx, altered); !errors.Is(err, ErrConflict) {
		t.Fatalf("same handle with different content = %v", err)
	}
	loaded, err := service.GetSelection(ctx, job.ID)
	if err != nil || loaded != accepted {
		t.Fatalf("constraint attempts changed snapshot: %#v, %v", loaded, err)
	}
}

func TestSelectionOnlyQueuedButReadOnlyRetrySurvivesTransition(t *testing.T) {
	for _, selected := range []bool{false, true} {
		for _, status := range []jobs.Status{jobs.StatusRunning, jobs.StatusCancelled} {
			service, jobService, db, _, job := fixture(t, provider("source", option("a")))
			ctx := context.Background()
			result, err := service.Discover(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			var accepted Selection
			if selected {
				accepted, err = service.Select(ctx, job.ID, result.Candidates[0].Handle)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := jobService.Transition(ctx, job.ID, status); err != nil {
				t.Fatal(err)
			}
			retry, err := service.Select(ctx, job.ID, result.Candidates[0].Handle)
			if selected {
				if err != nil || retry != accepted {
					t.Fatalf("read-only retry after %s = %#v, %v", status, retry, err)
				}
			} else if !errors.Is(err, ErrNotQueued) {
				t.Fatalf("new selection after %s = %v", status, err)
			}
			if !selected {
				// Direct SQL is guarded too, not only the service/INSERT SELECT.
				if _, err := db.Exec(`INSERT INTO acquisition_selections
					(job_id, provider_id, candidate_id, candidate_handle, execution_ref, title, label, language, selected_at_utc)
					VALUES (?, 'source', 'a', ?, 'opaque', 'Title', '', '', '2026-01-01T00:00:00.000000000Z')`, job.ID, result.Candidates[0].Handle); err == nil {
					t.Fatal("SQL bypassed queued guard")
				}
			}
		}
	}
}
