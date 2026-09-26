// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package livecohort

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // read-only store access for the candidate column
)

// StoreInspector reads the operator's live papio store read-only to fill the
// untried-candidate column, which no RPC exposes.
//
// Read-only for the same reasons internal/identitycorpus.OpenBacklogEligibility
// documents: mode=ro makes the driver refuse every write, but opening a
// WAL-mode database for READ still recreates the -wal/-shm sidecars beside
// it if the daemon has checkpointed and closed. That is harmless and the
// next daemon open reuses them, but it is a visible effect of taking a
// measurement, so it is stated rather than left to be found.
type StoreInspector struct {
	db   *sql.DB
	path string
}

// OpenStoreInspector opens <dataDir>/papio.db read-only. A missing file is
// reported so a caller can run without the column rather than fail: the
// column is evidence, and a run that cannot gather it is still a run.
func OpenStoreInspector(dataDir string) (*StoreInspector, error) {
	path := filepath.Join(dataDir, "papio.db")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("papio store %s: %w", path, err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("opening papio store %s read-only: %w", path, err)
	}
	return &StoreInspector{db: db, path: path}, nil
}

// Close releases the read-only handle.
func (s *StoreInspector) Close() error { return s.db.Close() }

// Path is the database this inspector read, for the report's provenance.
func (s *StoreInspector) Path() string { return s.path }

// UntriedCandidates counts the job's fetch candidates that papio never
// attempted, and the total it had.
//
// "Untried" is status='pending' only — a candidate papio never fetched at
// all. It deliberately excludes 'retryable' and 'invalid', which were tried
// and failed, and 'skipped', which papio passed over for a stated reason.
// The narrow definition is the point: this column exists to answer "did
// papio stop while still holding something it had not attempted", and a
// looser count would answer a softer question nobody asked.
func (s *StoreInspector) UntriedCandidates(ctx context.Context, jobID string) (int, int, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN status = 'pending' THEN 1 ELSE 0 END), 0),
			COUNT(*)
		FROM candidates WHERE job_id = ?`, jobID)
	var untried, total int
	if err := row.Scan(&untried, &total); err != nil {
		return 0, 0, fmt.Errorf("counting candidates for %s: %w", jobID, err)
	}
	return untried, total, nil
}

// JobForRequest returns the job the daemon committed for a work request id,
// TERMINAL ones included, and whether one exists at all.
//
// This is the lookup the submit path needs and no RPC provides. The daemon
// reuses a LIVE job for a request id and mints a fresh one once that job is
// terminal (internal/job/job.go createRequest), so resubmitting a request id
// to discover its job is only safe while the job is live and duplicates the
// acquisition after it is not. Reading the committed row answers the same
// question without asking for a second acquisition, which is why this
// lookup must be available even when the untried-candidate column is off.
func (s *StoreInspector) JobForRequest(ctx context.Context, requestID string) (string, string, bool, error) {
	var jobID, state string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, state FROM jobs
		WHERE work_request_id = ?
		ORDER BY created_at DESC LIMIT 1`, requestID).Scan(&jobID, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("looking up the job for request %s: %w", requestID, err)
	}
	return jobID, state, true, nil
}
