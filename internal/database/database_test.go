package database

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"lernae/migrations"
)

func TestOpenConfiguresRequiredPragmasOnEveryConnection(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "lernae.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	connections := make([]*sql.Conn, 0, 4)
	for range 4 {
		connection, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, connection)
	}
	defer func() {
		for _, connection := range connections {
			_ = connection.Close()
		}
	}()

	var wait sync.WaitGroup
	for index, connection := range connections {
		wait.Add(1)
		go func(index int, connection *sql.Conn) {
			defer wait.Done()
			var journal string
			var busyTimeout, foreignKeys int
			if err := connection.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
				t.Errorf("connection %d journal_mode: %v", index, err)
				return
			}
			if err := connection.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
				t.Errorf("connection %d busy_timeout: %v", index, err)
				return
			}
			if err := connection.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
				t.Errorf("connection %d foreign_keys: %v", index, err)
				return
			}
			if journal != "wal" || busyTimeout != 5000 || foreignKeys != 1 {
				t.Errorf("connection %d PRAGMAs = journal:%q timeout:%d foreign_keys:%d", index, journal, busyTimeout, foreignKeys)
			}
		}(index, connection)
	}
	wait.Wait()
}

func TestOpenAppliesVersionedMigrationsFromEmptyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lernae.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	var version, migrationCount int
	if err := db.QueryRow("SELECT MAX(version), COUNT(*) FROM schema_migrations").Scan(&version, &migrationCount); err != nil {
		t.Fatal(err)
	}
	if version != 19 || migrationCount != 19 {
		t.Fatalf("migration state = max %d count %d, want max 19 count 19", version, migrationCount)
	}
	var jobsExists bool
	if err := db.QueryRow("SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type='table' AND name='jobs')").Scan(&jobsExists); err != nil {
		t.Fatal(err)
	}
	if !jobsExists {
		t.Fatal("jobs table was not created by migration")
	}
	var legacyMembershipColumn int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('works') WHERE name='universe_id'").Scan(&legacyMembershipColumn); err != nil {
		t.Fatal(err)
	}
	if legacyMembershipColumn != 0 {
		t.Fatal("works table has a competing universe_id membership column")
	}
	for _, table := range []string{"works", "editions", "assets", "asset_parts", "asset_locations", "external_identities", "work_collection_external_identities", "metadata_search_cache", "sessions", "universes", "universe_memberships", "universe_aliases", "universe_exclusions", "acquisition_selections", "acquisition_executions"} {
		var exists bool
		if err := db.QueryRow("SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)", table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("table %q was not created by migration", table)
		}
	}
	columns, err := db.Query("PRAGMA table_info(metadata_search_cache)")
	if err != nil {
		t.Fatal(err)
	}
	wantColumns := map[string]int{"provider": 1, "normalized_query": 2, "results_json": 0, "fetched_at_utc": 0, "expires_at_utc": 0}
	for columns.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := columns.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if expected, exists := wantColumns[name]; exists {
			if expected > 0 && primaryKey != expected {
				t.Errorf("cache column %q primary-key position = %d, want %d", name, primaryKey, expected)
			}
			if notNull != 1 || columnType != "TEXT" {
				t.Errorf("cache column %q type/not-null = %q/%d, want TEXT/1", name, columnType, notNull)
			}
			delete(wantColumns, name)
		}
	}
	if err := columns.Err(); err != nil {
		t.Fatal(err)
	}
	if err := columns.Close(); err != nil {
		t.Fatal(err)
	}
	for column := range wantColumns {
		t.Errorf("cache column %q is missing", column)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopening migrated database: %v", err)
	}
	defer db.Close()
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if migrationCount != 19 {
		t.Fatalf("migration count after reopen = %d, want 19", migrationCount)
	}
}

func TestOpenCreatesRelationshipProviderCacheWithParsedFieldsOnly(t *testing.T) {
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "relationship-cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows, err := db.Query("PRAGMA table_info(relationship_provider_cache)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var columns []string
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(columns)
	want := []string{
		"entity_revision", "expires_at_utc", "external_id", "fetched_at_utc",
		"normalized_json", "parser_version", "provider",
	}
	if fmt.Sprint(columns) != fmt.Sprint(want) {
		t.Fatalf("relationship provider cache columns = %v, want parsed metadata fields %v", columns, want)
	}
}

func TestOpenUpgradesVersionSixDatabaseWithoutLosingExistingRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "phase2.db")
	legacy, err := openVersionSixDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `INSERT INTO jobs (id, kind, status, created_at_utc, updated_at_utc)
		VALUES ('legacy-job', 'metadata_search', 'succeeded', 'created', 'updated')`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `INSERT INTO works (id, medium, work_type, title)
		VALUES ('legacy-work', 'game', 'game', 'Legacy Work')`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `INSERT INTO external_identities (id, work_id, provider, external_id)
		VALUES ('legacy-identity', 'legacy-work', 'legacy-provider', 'exact-id')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade version-six database: %v", err)
	}
	defer db.Close()
	var version, migrationCount, jobs, works, identities int
	if err := db.QueryRowContext(ctx, `SELECT MAX(version), COUNT(*) FROM schema_migrations`).Scan(&version, &migrationCount); err != nil {
		t.Fatal(err)
	}
	if version != 19 || migrationCount != 19 {
		t.Fatalf("upgraded migration state = max %d count %d, want max 19 count 19", version, migrationCount)
	}
	for _, check := range []struct {
		table string
		want  *int
	}{{"jobs", &jobs}, {"works", &works}, {"external_identities", &identities}} {
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", check.table)).Scan(check.want); err != nil {
			t.Fatal(err)
		}
		if *check.want != 1 {
			t.Errorf("%s rows after upgrade = %d, want 1", check.table, *check.want)
		}
	}
	var foreignKeyViolations int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&foreignKeyViolations); err != nil {
		t.Fatal(err)
	}
	if foreignKeyViolations != 0 {
		t.Fatalf("foreign-key violations after upgrade = %d, want 0", foreignKeyViolations)
	}
	var legacySummary string
	if err := db.QueryRowContext(ctx, "SELECT summary FROM works WHERE id = 'legacy-work'").Scan(&legacySummary); err != nil {
		t.Fatal(err)
	}
	if legacySummary != "" {
		t.Fatalf("new summary column on legacy Work = %q, want empty default", legacySummary)
	}
}

func TestOpenUpgradesVersionSevenDatabasePreservingUniverseState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "universe-v7.db")
	legacy, err := openDatabaseAtVersion(path, 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, workID := range []string{"active-work", "excluded-work"} {
		if _, err := legacy.ExecContext(ctx, `INSERT INTO works (id, medium, work_type, title)
			VALUES (?, 'video', 'series', ?)`, workID, workID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := legacy.ExecContext(ctx, `INSERT INTO universes (id, title) VALUES ('v7-universe', 'Legacy Universe')`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `INSERT INTO universe_memberships
		(work_id, universe_id, provenance, confidence, accepted_at_utc)
		VALUES ('active-work', 'v7-universe', 'legacy-custom-source', 0.75, '2026-09-30T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `INSERT INTO universe_aliases
		(universe_id, alias, provenance, created_at_utc)
		VALUES ('v7-universe', 'Legacy Alias', 'manual', '2026-09-30T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `INSERT INTO universe_exclusions
		(work_id, universe_id, reason, recorded_at_utc)
		VALUES ('excluded-work', 'v7-universe', 'not related', '2026-09-30T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade version-seven database: %v", err)
	}
	defer db.Close()
	var version, migrationCount int
	if err := db.QueryRowContext(ctx, `SELECT MAX(version), COUNT(*) FROM schema_migrations`).Scan(&version, &migrationCount); err != nil {
		t.Fatal(err)
	}
	if version != 19 || migrationCount != 19 {
		t.Fatalf("upgraded migration state = max %d count %d, want max 19 count 19", version, migrationCount)
	}
	var provenance string
	var confidence float64
	var acceptedAt string
	var confirmedByUser bool
	if err := db.QueryRowContext(ctx, `SELECT provenance, confidence, accepted_at_utc, confirmed_by_user
		FROM universe_memberships WHERE work_id = 'active-work'`).Scan(&provenance, &confidence, &acceptedAt, &confirmedByUser); err != nil {
		t.Fatal(err)
	}
	if provenance != "legacy-custom-source" || confidence != 0.75 || acceptedAt != "2026-09-30T12:00:00Z" || confirmedByUser {
		t.Fatalf("upgraded membership = %q/%f/%q/confirmed=%t; want preserved row with unconfirmed default", provenance, confidence, acceptedAt, confirmedByUser)
	}
	for _, check := range []struct {
		table string
		want  int
	}{{"universes", 1}, {"universe_memberships", 1}, {"universe_aliases", 1}, {"universe_exclusions", 1}} {
		var count int
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", check.table)).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != check.want {
			t.Errorf("%s rows after upgrade = %d, want %d", check.table, count, check.want)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE universe_memberships SET confirmed_by_user = 2 WHERE work_id = 'active-work'`); err == nil {
		t.Fatal("confirmed_by_user accepted a non-boolean value")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO universe_memberships
		(work_id, universe_id, provenance, confidence, accepted_at_utc)
		VALUES ('excluded-work', 'v7-universe', 'unrecognized-source', 0.5, '2026-09-30T12:00:00Z')`); err == nil {
		t.Fatal("new membership accepted provenance outside provider/rule/manual/automatic")
	}
	var foreignKeyViolations int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&foreignKeyViolations); err != nil {
		t.Fatal(err)
	}
	if foreignKeyViolations != 0 {
		t.Fatalf("foreign-key violations after v7 upgrade = %d, want 0", foreignKeyViolations)
	}
}

func TestOpenUpgradesVersionTwelveMembershipReviewStateWithoutDeletingRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "universe-review-v12.db")
	legacy, err := openDatabaseAtVersion(path, 12)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `INSERT INTO universes
		(id, title, existence_confidence, naming_confidence, naming_confidence_level, naming_evidence, provenance, confirmed_by_user)
		VALUES ('legacy-universe', 'Evangelion', 0.98, 0, 'none', 'legacy_unknown', 'automatic', 0)`); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id              string
		provenance      string
		evidence        string
		reason          string
		confirmedByUser bool
	}{
		{id: "legacy-unknown", provenance: "automatic", evidence: "unknown", reason: "No supported matching evidence was found."},
		{id: "supported-auto", provenance: "automatic", evidence: "distinctive_whole_token", reason: "Supported title evidence."},
		{id: "manual-unknown", provenance: "manual", evidence: "unknown", reason: "No supported matching evidence was found."},
		{id: "confirmed-unknown", provenance: "automatic", evidence: "unknown", reason: "No supported matching evidence was found.", confirmedByUser: true},
	} {
		if _, err := legacy.ExecContext(ctx, `INSERT INTO works (id, medium, work_type, title) VALUES (?, 'game', 'game', ?)`, item.id, item.id); err != nil {
			t.Fatal(err)
		}
		if _, err := legacy.ExecContext(ctx, `INSERT INTO universe_memberships
			(work_id, universe_id, provenance, confidence, evidence, reason, accepted_at_utc, confirmed_by_user)
			VALUES (?, 'legacy-universe', ?, 0.9, ?, ?, '2026-09-30T12:00:00Z', ?)`,
			item.id, item.provenance, item.evidence, item.reason, item.confirmedByUser); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade version-twelve database: %v", err)
	}
	defer db.Close()
	var version, migrationsApplied, membershipRows int
	if err := db.QueryRowContext(ctx, `SELECT MAX(version), COUNT(*) FROM schema_migrations`).Scan(&version, &migrationsApplied); err != nil {
		t.Fatal(err)
	}
	if version != 19 || migrationsApplied != 19 {
		t.Fatalf("migration state = %d/%d, want version/count 19", version, migrationsApplied)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM universe_memberships`).Scan(&membershipRows); err != nil {
		t.Fatal(err)
	}
	if membershipRows != 4 {
		t.Fatalf("membership rows after migration = %d, want all 4 preserved", membershipRows)
	}
	for _, item := range []struct {
		id     string
		status string
	}{
		{id: "legacy-unknown", status: "review_needed"},
		{id: "supported-auto", status: "accepted"},
		{id: "manual-unknown", status: "accepted"},
		{id: "confirmed-unknown", status: "accepted"},
	} {
		var status, provenance, evidence, reason string
		var confirmed bool
		var confidence float64
		var acceptedAt string
		if err := db.QueryRowContext(ctx, `SELECT status, provenance, confidence, evidence, reason, accepted_at_utc, confirmed_by_user
			FROM universe_memberships WHERE work_id = ?`, item.id).Scan(&status, &provenance, &confidence, &evidence, &reason, &acceptedAt, &confirmed); err != nil {
			t.Fatal(err)
		}
		if status != item.status {
			t.Errorf("membership %q status = %q, want %q", item.id, status, item.status)
		}
		if provenance == "" || confidence != 0.9 || evidence == "" || reason == "" || acceptedAt != "2026-09-30T12:00:00Z" {
			t.Errorf("membership %q data changed: provenance=%q confidence=%v evidence=%q reason=%q accepted_at=%q", item.id, provenance, confidence, evidence, reason, acceptedAt)
		}
		if confirmed != (item.id == "confirmed-unknown") {
			t.Errorf("membership %q confirmed=%t after upgrade", item.id, confirmed)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO works (id, medium, work_type, title) VALUES ('new-unsupported', 'game', 'game', 'Unsupported')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO universe_memberships
		(work_id, universe_id, provenance, confidence, evidence, reason, accepted_at_utc, confirmed_by_user)
		VALUES ('new-unsupported', 'legacy-universe', 'automatic', 0.9, 'unknown', 'unsupported', '2026-09-30T12:00:00Z', 0)`); err == nil {
		t.Fatal("database accepted an unsupported unconfirmed automatic membership")
	}
	if _, err := db.ExecContext(ctx, `UPDATE universe_memberships SET status = 'accepted', confirmed_by_user = 1 WHERE work_id = 'legacy-unknown'`); err != nil {
		t.Fatalf("explicit user confirmation could not accept preserved suggestion: %v", err)
	}
}

func TestOpenUpgradesVersionNineUniverseMetadataDefaults(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "universe-v9.db")
	legacy, err := openDatabaseAtVersion(path, 9)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `INSERT INTO universes (id, title) VALUES ('legacy-universe', 'Legacy Universe')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade version-nine database: %v", err)
	}
	defer db.Close()
	var version, count int
	if err := db.QueryRowContext(ctx, `SELECT MAX(version), COUNT(*) FROM schema_migrations`).Scan(&version, &count); err != nil {
		t.Fatal(err)
	}
	if version != 19 || count != 19 {
		t.Fatalf("upgraded migration state = max %d count %d, want max 19 count 19", version, count)
	}
	var sortTitle, description, artwork sql.NullString
	var createdAt, updatedAt string
	if err := db.QueryRowContext(ctx, `SELECT sort_title, description, artwork, created_at_utc, updated_at_utc
		FROM universes WHERE id = 'legacy-universe'`).Scan(&sortTitle, &description, &artwork, &createdAt, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if sortTitle.Valid || description.Valid || artwork.Valid {
		t.Fatalf("legacy nullable Universe metadata = %#v/%#v/%#v, want NULL values", sortTitle, description, artwork)
	}
	created, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil || created.IsZero() {
		t.Fatalf("legacy created_at_utc = %q, want a valid migration-time timestamp: %v", createdAt, err)
	}
	updated, err := time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil || updated.IsZero() || !created.Equal(updated) {
		t.Fatalf("legacy updated_at_utc = %q, want the same valid migration-time fallback as created_at_utc %q: %v", updatedAt, createdAt, err)
	}
	var existenceConfidence float64
	var provenance string
	var confirmedByUser bool
	if err := db.QueryRowContext(ctx, `SELECT existence_confidence, provenance, confirmed_by_user
		FROM universes WHERE id = 'legacy-universe'`).Scan(&existenceConfidence, &provenance, &confirmedByUser); err != nil {
		t.Fatal(err)
	}
	if existenceConfidence != 1 || provenance != "manual" || !confirmedByUser {
		t.Fatalf("legacy Universe discovery metadata = %v/%q/confirmed=%t; want manual and confirmed defaults", existenceConfidence, provenance, confirmedByUser)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("restart upgraded version-nine database: %v", err)
	}
	defer restarted.Close()
	var restartedCreatedAt, restartedUpdatedAt string
	if err := restarted.QueryRowContext(ctx, `SELECT created_at_utc, updated_at_utc FROM universes WHERE id = 'legacy-universe'`).
		Scan(&restartedCreatedAt, &restartedUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if restartedCreatedAt != createdAt || restartedUpdatedAt != updatedAt {
		t.Fatalf("legacy timestamps after restart = %q/%q, want persisted %q/%q", restartedCreatedAt, restartedUpdatedAt, createdAt, updatedAt)
	}
}

func openVersionSixDatabase(path string) (*sql.DB, error) {
	return openDatabaseAtVersion(path, 6)
}

func openDatabaseAtVersion(path string, maxVersion int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		_ = db.Close()
		return nil, err
	}
	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		version, err := migrationVersion(entry.Name())
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		if version > maxVersion {
			continue
		}
		sqlBytes, err := migrations.Files.ReadFile(entry.Name())
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		tx, err := db.Begin()
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		if _, err := tx.Exec(string(sqlBytes)); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return nil, err
		}
		if _, err := tx.Exec("INSERT INTO schema_migrations(version, applied_at_utc) VALUES (?, 'legacy')", version); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return db, nil
}

func TestOpenUpgradesVersionSeventeenPreservingQueuedAcquisition(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "acquisition-v17.db")
	legacy, err := openDatabaseAtVersion(path, 17)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO works (id, medium, work_type, title) VALUES ('existing-work', 'literature', 'novel', 'Existing Work')`,
		`INSERT INTO editions (id, work_id, format) VALUES ('existing-edition', 'existing-work', 'epub')`,
		`INSERT INTO jobs (id, kind, status, target_edition_id, created_at_utc, updated_at_utc)
		 VALUES ('existing-acquisition', 'acquire', 'queued', 'existing-edition', 'created', 'updated')`,
	} {
		if _, err := legacy.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var status, target, created, updated string
	if err := db.QueryRowContext(ctx, `SELECT status, target_edition_id, created_at_utc, updated_at_utc
		FROM jobs WHERE id = 'existing-acquisition'`).Scan(&status, &target, &created, &updated); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || target != "existing-edition" || created != "created" || updated != "updated" {
		t.Fatalf("0018 changed existing acquisition: %s/%s/%s/%s", status, target, created, updated)
	}
	var selections, violations, version int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM acquisition_selections`).Scan(&selections); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if selections != 0 || violations != 0 || version != 19 {
		t.Fatalf("0018 upgrade state = selections:%d violations:%d version:%d", selections, violations, version)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs
		(id, kind, status, target_edition_id, created_at_utc, updated_at_utc)
		VALUES ('duplicate', 'acquire', 'queued', 'existing-edition', 'created', 'updated')`); err == nil {
		t.Fatal("0018 lost the P4-01 active-target unique constraint")
	}
}

func TestOpenUpgradesVersionEighteenWithoutClaimingSelectedAcquisition(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "selected-v18.db")
	legacy, err := openDatabaseAtVersion(path, 18)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO works (id, medium, work_type, title) VALUES ('work-1', 'literature', 'novel', 'Existing')`,
		`INSERT INTO editions (id, work_id, format) VALUES ('edition-1', 'work-1', 'epub')`,
		`INSERT INTO jobs (id, kind, status, target_edition_id, created_at_utc, updated_at_utc)
		 VALUES ('selected-job', 'acquire', 'queued', 'edition-1', 'created', 'updated')`,
		`INSERT INTO acquisition_selections
		 (job_id, provider_id, candidate_id, candidate_handle, execution_ref, title, label, language, selected_at_utc)
		 VALUES ('selected-job', 'source', 'exact', '` + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `',
		 'PRIVATE_EXECUTION_SENTINEL', 'Exact title', '', '', '2026-01-01T00:00:00.000000000Z')`,
	} {
		if _, err := legacy.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var status, ref, title, selectedAt string
	if err := db.QueryRowContext(ctx, `SELECT j.status, s.execution_ref, s.title, s.selected_at_utc
		FROM jobs j JOIN acquisition_selections s ON s.job_id = j.id WHERE j.id = 'selected-job'`).
		Scan(&status, &ref, &title, &selectedAt); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || ref != "PRIVATE_EXECUTION_SENTINEL" || title != "Exact title" || selectedAt != "2026-01-01T00:00:00.000000000Z" {
		t.Fatal("0019 changed exact queued selection")
	}
	var reservations, violations int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM acquisition_executions`).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if reservations != 0 || violations != 0 {
		t.Fatalf("0019 upgrade claimed work or violated FKs: %d/%d", reservations, violations)
	}
	var privateCopies int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('acquisition_executions')
		WHERE name IN ('execution_ref', 'payload', 'edition_id', 'work_id', 'provider_id', 'candidate_id')`).Scan(&privateCopies); err != nil {
		t.Fatal(err)
	}
	if privateCopies != 0 {
		t.Fatal("reservation duplicates selection payload/identity")
	}
}

func TestOpenRejectsDatabaseFromANewerSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lernae.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO schema_migrations(version, applied_at_utc) VALUES (99, 'future')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), path); err == nil {
		t.Fatal("expected newer schema version to be rejected")
	}
}
