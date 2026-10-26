// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"papio/internal/filing"
	"papio/internal/job"
	"papio/internal/store"
)

var ErrManagedFilingNotConfigured = errors.New("managed folder filing is not configured")

// ManagedFiling is a durable destination-specific receipt, not a hook outcome.
type ManagedFiling struct {
	JobID          string `json:"job_id"`
	Destination    string `json:"destination"`
	IdempotencyKey string `json:"idempotency_key"`
	ArtifactSHA256 string `json:"artifact_sha256"`
	State          string `json:"state"`
	Disposition    string `json:"disposition"`
	Attempts       int    `json:"attempts"`
	RetryAt        string `json:"retry_at,omitempty"`
	ErrorCode      string `json:"error_code,omitempty"`
	ReceiptPath    string `json:"receipt_path,omitempty"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

const managedFilingColumns = `f.job_id, f.destination, f.idempotency_key, f.artifact_sha256, f.state, j.acquisition_disposition, f.attempts, f.retry_at, f.error_code, f.receipt_path, f.created_at, f.updated_at`

func scanManagedFiling(row interface{ Scan(...any) error }) (ManagedFiling, error) {
	var f ManagedFiling
	err := row.Scan(&f.JobID, &f.Destination, &f.IdempotencyKey, &f.ArtifactSHA256, &f.State, &f.Disposition, &f.Attempts, &f.RetryAt, &f.ErrorCode, &f.ReceiptPath, &f.CreatedAt, &f.UpdatedAt)
	return f, err
}

func (s *Service) managedDestination() (string, error) {
	if strings.TrimSpace(s.ManagedFilingFolder) == "" {
		return "", ErrManagedFilingNotConfigured
	}
	return filepath.Abs(s.ManagedFilingFolder)
}

func (s *Service) queueManagedFiling(ctx context.Context, jobID string) error {
	destination, err := s.managedDestination()
	if err != nil {
		if errors.Is(err, ErrManagedFilingNotConfigured) {
			return nil
		}
		return err
	}
	row, err := s.Jobs.Get(ctx, jobID)
	if err != nil {
		return err
	}
	if row.ArtifactSHA256 == "" {
		return nil
	}
	now := store.FormatTime(s.Now())
	_, err = s.Jobs.S.DB().ExecContext(ctx, `
		INSERT INTO managed_filings(job_id, destination, idempotency_key, artifact_sha256, state, created_at, updated_at)
		SELECT j.id, ?, ?, j.artifact_sha256, 'pending', ?, ? FROM jobs j
		WHERE j.id = ? AND j.state IN ('ready', 'imported') AND j.acquisition_disposition = 'active'
		AND EXISTS (SELECT 1 FROM job_artifacts ja WHERE ja.job_id = j.id AND ja.artifact_sha256 = j.artifact_sha256 AND ja.role = 'main' AND ja.identity_result IN ('pass', 'user_confirmed'))
		ON CONFLICT(job_id, destination) DO NOTHING`, destination, filing.Key(jobID, destination, row.ArtifactSHA256), now, now, jobID)
	return err
}

func (s *Service) ManagedFilings(ctx context.Context, jobID string, limit int) ([]ManagedFiling, bool, error) {
	effective := job.EffectiveListLimit(limit)
	rows, err := s.Jobs.S.DB().QueryContext(ctx, `SELECT `+managedFilingColumns+` FROM managed_filings f JOIN jobs j ON j.id = f.job_id WHERE (? = '' OR f.job_id = ?) ORDER BY f.created_at DESC, f.job_id, f.destination LIMIT ?`, jobID, jobID, effective+1)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]ManagedFiling, 0)
	for rows.Next() {
		f, err := scanManagedFiling(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(out) > effective
	if truncated {
		out = out[:effective]
	}
	return out, truncated, nil
}

// RetryManagedFiling performs bounded automatic recovery. Only this folder
// adapter participates; arbitrary on_ready commands retain their one-shot rules.
// The maintenance pass runs it, never mandatory scheduler startup: the
// destination is optional and its I/O may stall (internal/filing bounds that).
func (s *Service) RetryManagedFiling(ctx context.Context) error {
	// Archive may commit during publication, including just before a crash.
	// Recover its unreceipted links even if the configured folder has changed
	// or filing has since been disabled.
	if err := s.recoverArchivedManagedFilings(ctx); err != nil {
		return err
	}
	destination, err := s.managedDestination()
	if err != nil {
		if errors.Is(err, ErrManagedFilingNotConfigured) {
			return nil
		}
		return err
	}
	// Reconstruct unjournaled ready transitions after restart, oldest first.
	rows, err := s.Jobs.S.DB().QueryContext(ctx, `SELECT j.id FROM jobs j WHERE j.state IN ('ready', 'imported') AND j.acquisition_disposition = 'active' AND EXISTS (SELECT 1 FROM job_artifacts ja WHERE ja.job_id = j.id AND ja.artifact_sha256 = j.artifact_sha256 AND ja.role = 'main' AND ja.identity_result IN ('pass', 'user_confirmed')) AND NOT EXISTS (SELECT 1 FROM managed_filings f WHERE f.job_id = j.id AND f.destination = ?) ORDER BY j.created_at, j.id LIMIT 50`, destination)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.queueManagedFiling(ctx, id); err != nil {
			return err
		}
	}
	rows, err = s.Jobs.S.DB().QueryContext(ctx, `SELECT f.job_id, f.destination FROM managed_filings f JOIN jobs j ON j.id = f.job_id WHERE f.destination = ? AND j.acquisition_disposition = 'active' AND j.state IN ('ready', 'imported') AND (f.state = 'pending' OR (f.state = 'failed' AND f.retry_at <> '')) AND (f.retry_at = '' OR f.retry_at <= ?) ORDER BY f.updated_at, f.job_id LIMIT 3`, destination, store.FormatTime(s.Now()))
	if err != nil {
		return err
	}
	type target struct{ jobID, destination string }
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.jobID, &t.destination); err != nil {
			_ = rows.Close()
			return err
		}
		targets = append(targets, t)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range targets {
		// A wedged destination fails every attempt fast. Stop here rather than
		// spend each due receipt's retry budget on a folder that cannot answer.
		if filing.Stalled() {
			return nil
		}
		if _, err := s.fileManaged(ctx, t.jobID, t.destination, false); err != nil && !errors.Is(err, job.ErrConflict) {
			return err
		}
	}
	return nil
}

func (s *Service) recoverArchivedManagedFilings(ctx context.Context) error {
	select {
	case managedFilingGate <- struct{}{}:
		defer func() { <-managedFilingGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	rows, err := s.Jobs.S.DB().QueryContext(ctx, `SELECT `+managedFilingColumns+` FROM managed_filings f JOIN jobs j ON j.id = f.job_id WHERE j.acquisition_disposition = 'archived' AND f.attempts > 0 AND (f.state = 'pending' OR (f.state = 'failed' AND f.retry_at <> '')) ORDER BY f.updated_at, f.job_id LIMIT 3`)
	if err != nil {
		return err
	}
	var pending []ManagedFiling
	for rows.Next() {
		f, err := scanManagedFiling(rows)
		if err != nil {
			_ = rows.Close()
			return err
		}
		pending = append(pending, f)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, f := range pending {
		if filing.Stalled() {
			return nil
		}
		// No database transaction spans destination I/O. Keep the attempt
		// recoverable until both the link and staged leftovers are gone.
		undoErr := filing.UndoPublished(ctx, f.Destination, f.IdempotencyKey, f.ArtifactSHA256)
		if undoErr == nil {
			undoErr = filing.CleanupStages(ctx, f.Destination, f.IdempotencyKey)
		}
		state, code := "failed", "archived"
		if undoErr != nil {
			state, code = "pending", "destination_unavailable"
			if errors.Is(undoErr, filing.ErrDestinationConflict) {
				code = "destination_conflict"
			}
			log.Printf("papio: recovering archived managed filing for job %s: %v", f.JobID, undoErr)
		}
		// A concurrent restore leaves the journal available for active replay.
		// The filing gate prevents another publisher from receipting the link
		// while this compensation runs.
		if _, err := s.Jobs.S.DB().ExecContext(ctx, `UPDATE managed_filings SET state = ?, error_code = ?, retry_at = '', receipt_path = '', updated_at = ? WHERE job_id = ? AND destination = ? AND state <> 'filed' AND EXISTS (SELECT 1 FROM jobs j WHERE j.id = managed_filings.job_id AND j.acquisition_disposition = 'archived')`, state, code, store.FormatTime(s.Now()), f.JobID, f.Destination); err != nil {
			return err
		}
	}
	return nil
}

// RetryManagedJob makes an explicit safe replay of one journaled folder target.
func (s *Service) RetryManagedJob(ctx context.Context, jobID, destination string) (ManagedFiling, error) {
	configured, err := s.managedDestination()
	if err != nil {
		return ManagedFiling{}, err
	}
	if destination == "" {
		destination = configured
	} else {
		destination, err = filepath.Abs(destination)
		if err != nil {
			return ManagedFiling{}, err
		}
	}
	if destination == configured {
		if err := s.queueManagedFiling(ctx, jobID); err != nil {
			return ManagedFiling{}, err
		}
	}
	filed, err := s.fileManaged(ctx, jobID, destination, true)
	if errors.Is(err, sql.ErrNoRows) {
		return filed, s.managedRefusal(ctx, jobID, destination, configured)
	}
	return filed, err
}

// managedRefusal explains why a retry found no journaled filing. The bare
// sql.ErrNoRows reads as "no such job" to the operator, so each cause that is
// really a refusal gets its own message; a job that does not exist keeps the
// not-found classification.
func (s *Service) managedRefusal(ctx context.Context, jobID, destination, configured string) error {
	row, err := s.Jobs.Get(ctx, jobID)
	if err != nil {
		return err
	}
	disposition, err := s.Jobs.Disposition(ctx, jobID)
	if err != nil {
		return err
	}
	switch {
	case disposition.Disposition == job.DispositionArchived:
		return fmt.Errorf("job %s is archived; run `papio jobs restore %s` first: %w", jobID, jobID, job.ErrConflict)
	case row.State != job.StateReady && row.State != job.StateImported:
		return fmt.Errorf("job %s is %s; managed filing needs ready or imported: %w", jobID, row.State, job.ErrConflict)
	case row.ArtifactSHA256 == "":
		return fmt.Errorf("job %s has no artifact to file: %w", jobID, job.ErrConflict)
	}
	var validated bool
	if err := s.Jobs.S.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_artifacts WHERE job_id = ? AND artifact_sha256 = ? AND role = 'main' AND identity_result IN ('pass', 'user_confirmed'))`, jobID, row.ArtifactSHA256).Scan(&validated); err != nil {
		return err
	}
	if !validated {
		return fmt.Errorf("job %s has no identity-validated artifact: %w", jobID, job.ErrConflict)
	}
	return fmt.Errorf("no journaled filing for %s at %s (configured destination is %s): %w", jobID, destination, configured, job.ErrConflict)
}

// Keep publication and compensation single-flight without holding a database
// writer. Otherwise a concurrent replay could receipt a link we must undo.
var managedFilingGate = make(chan struct{}, 1)

func (s *Service) fileManaged(ctx context.Context, jobID, destination string, force bool) (ManagedFiling, error) {
	select {
	case managedFilingGate <- struct{}{}:
		defer func() { <-managedFilingGate }()
	case <-ctx.Done():
		return ManagedFiling{}, ctx.Err()
	}
	tx, err := s.Jobs.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return ManagedFiling{}, err
	}
	defer func(tx *sql.Tx) { _ = tx.Rollback() }(tx)
	if _, err = tx.ExecContext(ctx, `UPDATE managed_filings SET updated_at = updated_at WHERE job_id = ? AND destination = ?`, jobID, destination); err != nil {
		return ManagedFiling{}, err
	}
	f, err := scanManagedFiling(tx.QueryRowContext(ctx, `SELECT `+managedFilingColumns+` FROM managed_filings f JOIN jobs j ON j.id = f.job_id WHERE f.job_id = ? AND f.destination = ?`, jobID, destination))
	if err != nil {
		return f, err
	}
	if f.Disposition != job.DispositionActive {
		return f, fmt.Errorf("job %s is archived; run `papio jobs restore %s` first: %w", jobID, jobID, job.ErrConflict)
	}
	if f.State == "filed" {
		if err := tx.Commit(); err != nil {
			return f, err
		}
		s.cleanupManagedStages(ctx, f)
		return f, nil
	}
	now := store.FormatTime(s.Now())
	if !force && f.RetryAt > now {
		return f, tx.Commit()
	}
	// Persist an attempt before touching the folder. A crash leaves pending
	// work with a due time; replay proves the deterministic target's hash.
	f.Attempts++
	f.State, f.ErrorCode, f.UpdatedAt = "pending", "", now
	f.RetryAt = store.FormatTime(s.Now().Add(managedRetryDelay(f.Attempts)))
	if _, err = tx.ExecContext(ctx, `UPDATE managed_filings SET attempts = ?, state = 'pending', error_code = '', retry_at = ?, updated_at = ? WHERE job_id = ? AND destination = ?`, f.Attempts, f.RetryAt, now, jobID, destination); err != nil {
		return f, err
	}
	if err = tx.Commit(); err != nil {
		return f, err
	}
	path, err := s.Artifacts.ArtifactPath(f.ArtifactSHA256)
	var staged *filing.Staged
	if err == nil {
		staged, err = filing.Stage(ctx, path, destination, f.IdempotencyKey, f.ArtifactSHA256)
	}
	if staged != nil {
		defer staged.Cleanup()
	}
	stageErr := err
	if stageErr == nil {
		stageErr = staged.Publish(ctx)
	}
	// The committed pending attempt journals the publish-before-receipt gap.
	// A crash replays the deterministic target; a live refusal or database
	// failure compensates only the link this attempt created.
	keepPublished := false
	if staged != nil {
		defer func() {
			if !keepPublished {
				if err := staged.Undo(); err != nil {
					log.Printf("papio: undoing managed filing for job %s: %v", jobID, err)
				}
			}
		}()
	}
	tx, err = s.Jobs.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return f, err
	}
	defer func(tx *sql.Tx) { _ = tx.Rollback() }(tx)
	// Serialize the receipt with archive, but never filesystem I/O.
	if _, err = tx.ExecContext(ctx, `UPDATE jobs SET acquisition_disposition = acquisition_disposition WHERE id = ?`, jobID); err != nil {
		return f, err
	}
	current, err := scanManagedFiling(tx.QueryRowContext(ctx, `SELECT `+managedFilingColumns+` FROM managed_filings f JOIN jobs j ON j.id = f.job_id WHERE f.job_id = ? AND f.destination = ?`, jobID, destination))
	if err != nil {
		return f, err
	}
	if current.Disposition != job.DispositionActive {
		return current, job.ErrConflict
	}
	if current.State == "filed" {
		err := tx.Commit()
		keepPublished = err == nil
		return current, err
	}
	f = current
	f.UpdatedAt = store.FormatTime(s.Now())
	if stageErr == nil {
		f.State, f.RetryAt, f.ErrorCode, f.ReceiptPath = "filed", "", "", staged.Target
	} else {
		f.State, f.ErrorCode = "failed", "destination_unavailable"
		if errors.Is(stageErr, filing.ErrArtifact) {
			f.ErrorCode, f.RetryAt = "artifact_invalid", ""
		}
		if errors.Is(stageErr, filing.ErrDestinationConflict) {
			f.ErrorCode, f.RetryAt = "destination_conflict", ""
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE managed_filings SET state = ?, retry_at = ?, error_code = ?, receipt_path = ?, updated_at = ? WHERE job_id = ? AND destination = ?`, f.State, f.RetryAt, f.ErrorCode, f.ReceiptPath, f.UpdatedAt, jobID, destination); err != nil {
		return f, err
	}
	detail, err := json.Marshal(map[string]any{"state": f.State, "idempotency_key": f.IdempotencyKey, "attempts": f.Attempts, "error_code": f.ErrorCode})
	if err != nil {
		return f, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(job_id, at, kind, detail_json) VALUES(?, ?, 'filing.folder', ?)`, jobID, f.UpdatedAt, string(detail)); err != nil {
		return f, err
	}
	if err := tx.Commit(); err != nil {
		return f, err
	}
	if f.State == "filed" {
		keepPublished = true
		s.cleanupManagedStages(ctx, f)
	}
	return f, nil
}

func (s *Service) cleanupManagedStages(ctx context.Context, f ManagedFiling) {
	if err := filing.CleanupStages(ctx, f.Destination, f.IdempotencyKey); err != nil {
		log.Printf("papio: cleaning managed filing stages for job %s: %v", f.JobID, err)
	}
}

func managedRetryDelay(attempts int) time.Duration {
	if attempts <= 1 {
		return time.Minute
	}
	if attempts == 2 {
		return 10 * time.Minute
	}
	return time.Hour
}

type ManagedFilingRetrier struct{ svc *Service }

func (s *Service) ManagedFilingRetrier() *ManagedFilingRetrier { return &ManagedFilingRetrier{svc: s} }

func (r *ManagedFilingRetrier) RunDue(ctx context.Context) error {
	if r == nil || r.svc == nil {
		return nil
	}
	return r.svc.RetryManagedFiling(ctx)
}
