// Package database owns SQLite initialization and versioned migration execution.
package database

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"lernae/migrations"
)

const migrationTable = "schema_migrations"

// Open initializes SQLite, applies connection-wide PRAGMAs and applies pending
// versioned migrations before returning the shared database pool.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("database path must be absolute: %q", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	db, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize SQLite database: %w", err)
	}
	if err := migrate(ctx, db, migrations.Files); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply SQLite migrations: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure SQLite database file permissions: %w", err)
	}
	return db, nil
}

func dataSourceName(path string) string {
	fileURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
	query := url.Values{}
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(ON)")
	return fileURL + "?" + query.Encode()
}

func migrate(ctx context.Context, db *sql.DB, migrationFS embed.FS) error {
	files, err := migrationFS.ReadDir(".")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	var names []string
	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".sql") {
			names = append(names, file.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Errorf("no SQL migrations are embedded")
	}
	versions := make([]int, len(names))
	for index, name := range names {
		version, err := migrationVersion(name)
		if err != nil {
			return err
		}
		if version != index+1 {
			return fmt.Errorf("migration sequence has a gap: %s has version %d, want %d", name, version, index+1)
		}
		versions[index] = version
	}

	trackingExists, err := migrationApplied(ctx, db)
	if err != nil {
		return err
	}
	if trackingExists {
		var currentVersion int
		if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&currentVersion); err != nil {
			return fmt.Errorf("read current schema version: %w", err)
		}
		if currentVersion > versions[len(versions)-1] {
			return fmt.Errorf("database schema version %d is newer than this Server supports (latest migration %d)", currentVersion, versions[len(versions)-1])
		}
	}

	for index, name := range names {
		version := versions[index]
		applied, err := migrationApplied(ctx, db)
		if err != nil {
			return err
		}
		if applied {
			var exists bool
			if err := db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = ?)", version).Scan(&exists); err != nil {
				return fmt.Errorf("check migration %d: %w", version, err)
			}
			if exists {
				continue
			}
		} else if version != 1 {
			return fmt.Errorf("cannot apply migration %s before migration 0001 initializes schema_migrations", name)
		}

		sqlBytes, err := migrationFS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("execute migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at_utc) VALUES (?, ?)", version, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

func migrationApplied(ctx context.Context, db *sql.DB) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)", migrationTable).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check migration tracking table: %w", err)
	}
	return exists, nil
}

func migrationVersion(name string) (int, error) {
	parts := strings.SplitN(name, "_", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid migration filename %q: expected <version>_<name>.sql", name)
	}
	version, err := strconv.Atoi(parts[0])
	if err != nil || version < 1 {
		return 0, fmt.Errorf("invalid migration version in %q", name)
	}
	return version, nil
}
