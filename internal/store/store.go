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
	if err := s.migrate(ctx); err != nil {
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

// migrate applies numbered migrations above the current user_version, each in
// its own transaction, then bumps user_version inside that transaction.
func (s *Store) migrate(ctx context.Context) error {
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

	var current int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("reading user_version: %w", err)
	}
	for _, name := range names {
		num, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: expected NNNN_name.sql", name)
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
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("applying %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", num)); err != nil {
			_ = tx.Rollback()
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

// Backup copies the live database to destPath using VACUUM INTO (safe under WAL).
func (s *Store) Backup(ctx context.Context, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("backup destination %s already exists", destPath)
	}
	_, err := s.db.ExecContext(ctx, "VACUUM INTO ?", destPath)
	return err
}

// Checkpoint truncates the WAL (used before backups and by doctor).
func (s *Store) Checkpoint(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}
