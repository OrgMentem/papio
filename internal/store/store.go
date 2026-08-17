// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package store owns the SQLite database: WAL, foreign keys, busy timeout,
// a single writer connection, numbered transactional migrations gated on
// PRAGMA user_version, startup integrity check, and append-only redacted
// events. Only the daemon process opens the store for writing.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store wraps the single-writer database handle.
type Store struct {
	db   *sql.DB
	path string
}

// Open creates/opens the database at dir/papio.db, applies migrations, and
// verifies integrity. The connection pool is capped at one connection so all
// writes serialize in-process.
func Open(ctx context.Context, dir string) (*Store, error) {
	return open(ctx, dir, 0)
}

// open is the startup path with an optional migration ceiling. A non-zero
// ceiling models an older guard-capable binary while the current migration
// files are present in this source tree.
func open(ctx context.Context, dir string, migrationCeiling int) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}
	path := filepath.Join(dir, "papio.db")
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.migrate(ctx, migrationCeiling); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := ensurePdfGrabActiveSourceIndex(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		_ = db.Close()
		if err != nil {
			return nil, fmt.Errorf("integrity check on %s failed: %w", path, err)
		}
		return nil, fmt.Errorf("integrity check on %s returned %q, want \"ok\"", path, integrity)
	}
	return s, nil
}

// Close closes the handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for package-internal query helpers elsewhere in papio.
func (s *Store) DB() *sql.DB { return s.db }

// Path returns the database file path (for doctor/backup).
func (s *Store) Path() string { return s.path }

// IntegrityCheck verifies the live database. Open already runs it once; doctor
// uses this method for an explicit readiness report.
func (s *Store) IntegrityCheck(ctx context.Context) error {
	var result string
	if err := s.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("integrity_check: %s", result)
	}
	return nil
}

// ensurePdfGrabActiveSourceIndex creates the at-most-one-active-capture-per-paper
// index when the data allows it, and leaves it absent when it does not.
//
// The index is unusual in that it entered the schema through an in-place edit of
// an already-applied migration (0025) rather than a new one, so whether a
// database has it does not follow from its schema version. Databases predating
// that edit also predate the existence check in Allocate, so they can hold
// several active captures for one paper — and a plain CREATE UNIQUE INDEX over
// those rows fails, which inside migration 0038's transaction would mean
// Store.Open refusing to open the database and papio not starting at all.
//
// Deciding the collision here is not an option: a duplicate may be
// 'quarantined', holding the only copy of a paper's bytes, and guessing which of
// two captures is real is exactly what this project must never do. So a
// collision leaves the index absent, which is the state that database was
// already in, and papio doctor reports it with the remedy. Every other database
// — every fresh install, and every legacy one without duplicates — gets the
// constraint.
func ensurePdfGrabActiveSourceIndex(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE UNIQUE INDEX IF NOT EXISTS pdf_grabs_active_source
		  ON pdf_grabs(url_host, title)
		  WHERE state IN ('awaiting_file','quarantined','identified','parked_no_identifier')`)
	if err == nil {
		return nil
	}
	// Only the duplicate collision is tolerated. Anything else — a missing
	// table, a corrupt file — is a real failure to open.
	if strings.Contains(err.Error(), "UNIQUE constraint failed") ||
		strings.Contains(err.Error(), "constraint failed") {
		return nil
	}
	return fmt.Errorf("creating pdf_grabs_active_source: %w", err)
}

// migrate applies numbered migrations above the current user_version, each in
// its own transaction, then bumps user_version inside that transaction.
func (s *Store) migrate(ctx context.Context, migrationCeiling int) error {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("reading embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Errorf("no embedded migrations")
	}
	latestName := names[len(names)-1]
	latest, err := strconv.Atoi(strings.SplitN(latestName, "_", 2)[0])
	if err != nil {
		return fmt.Errorf("migration %s: expected NNNN_name.sql", latestName)
	}
	if migrationCeiling > 0 && migrationCeiling < latest {
		latest = migrationCeiling
	}

	var current int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("reading user_version: %w", err)
	}
	if current > latest {
		return fmt.Errorf(
			"database schema version %d is newer than this binary supports (%d); refusing to open",
			current,
			latest,
		)
	}
	for _, name := range names {
		num, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: expected NNNN_name.sql", name)
		}
		if num > latest {
			continue
		}
		if num <= current {
			continue
		}
		if num != current+1 {
			return fmt.Errorf("migration gap: at version %d, next file is %s", current, name)
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		// A failed rollback here is a second fault on top of the migration
		// error: the on-disk schema state is then ambiguous, so it must not be
		// swallowed behind the original error.
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			if rbErr := tx.Rollback(); rbErr != nil {
				return fmt.Errorf("applying %s: %w (rollback also failed: %w)", name, err, rbErr)
			}
			return fmt.Errorf("applying %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", num)); err != nil {
			if rbErr := tx.Rollback(); rbErr != nil {
				return fmt.Errorf("bumping user_version for %s: %w (rollback also failed: %w)", name, err, rbErr)
			}
			return fmt.Errorf("bumping user_version for %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing %s: %w", name, err)
		}
		current = num
	}
	return nil
}

// UserVersion returns the applied schema version.
func (s *Store) UserVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

// Now formats the canonical UTC timestamp used across tables.
func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// AppendEvent writes one append-only event. Detail must already be redacted;
// this is enforced by convention at call sites plus the redact package, since
// the store cannot distinguish a secret from a string.
func (s *Store) AppendEvent(ctx context.Context, jobID, kind string, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	data, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("encoding event detail: %w", err)
	}
	var job any
	if jobID != "" {
		job = jobID
	}
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, ?, ?)",
		job, Now(), kind, string(data))
	return err
}
