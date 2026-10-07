// Copyright 2026 OrgMentem. Licensed under MIT.

package batch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // read-only store access for the submit retry path
)

// ErrBatchStoreNotFound means <dataDir>/papio.db does not exist yet, so no
// daemon could have committed a job for any request ID. Callers treat it as
// a positive absence answer, not a failure.
var ErrBatchStoreNotFound = errors.New("batch store not found")

// storeInspector answers one question the daemon RPC does not: which job, if
// any, did the daemon already commit for a work request ID, terminal jobs
// included. The daemon reuses a live job for a request ID and mints a fresh
// one once that job is terminal, so asking acquire.submit to rediscover the
// job is only safe while it is live. A CLI killed after the daemon committed
// but before the reply reached recordJob leaves a jobless intent; reading
// the committed row lets the retry adopt instead of duplicating.
//
// Read-only for the same reason internal/livecohort documents: mode=ro makes
// the driver refuse every write, and opening a WAL-mode database for READ
// may recreate the -wal/-shm sidecars beside it. That is harmless and the
// next daemon open reuses them.
type storeInspector struct {
	db *sql.DB
}

// openStoreInspector opens <dataDir>/papio.db read-only. A missing file
// yields ErrBatchStoreNotFound; every other failure is returned as is.
func openStoreInspector(dataDir string) (*storeInspector, error) {
	path := filepath.Join(dataDir, "papio.db")
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrBatchStoreNotFound
		}
		return nil, fmt.Errorf("statting batch store: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("opening batch store read-only: %w", err)
	}
	return &storeInspector{db: db}, nil
}

// close releases the read-only handle. A nil inspector is a no-op so
// callers can defer it unconditionally.
func (s *storeInspector) close() error {
	if s == nil {
		return nil
	}
	return s.db.Close()
}

// jobForRequest returns the latest job the daemon committed for a work
// request ID, whatever its state, and whether one exists. Absence is a
// usable answer: nothing was ever committed, so submitting is safe.
func (s *storeInspector) jobForRequest(ctx context.Context, requestID string) (jobID, state string, found bool, err error) {
	var id, st string
	err = s.db.QueryRowContext(ctx, `
		SELECT id, state FROM jobs
		WHERE work_request_id = ?
		ORDER BY created_at DESC, id DESC LIMIT 1`, requestID).Scan(&id, &st)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("looking up the job for request %s: %w", requestID, err)
	}
	return id, st, true, nil
}

// jobForScopedRequest includes convergence onto another request's job. A lost
// submit_once reply leaves that association only in submission_receipts.
func (s *storeInspector) jobForScopedRequest(ctx context.Context, requestID string) (jobID, state string, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT j.id, j.state FROM submission_receipts r
		LEFT JOIN jobs j ON j.id = r.job_id
		WHERE r.request_id = ?`, requestID).Scan(&jobID, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return s.jobForRequest(ctx, requestID)
	}
	if err != nil {
		return "", "", false, fmt.Errorf("looking up the submission receipt for request %s: %w", requestID, err)
	}
	return jobID, state, true, nil
}
