// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"encoding/json"
	"errors"
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
func (s *Service) RetryManagedFiling(ctx context.Context) error {
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
		if _, err := s.fileManaged(ctx, t.jobID, t.destination, false); err != nil && !errors.Is(err, job.ErrConflict) {
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
	return s.fileManaged(ctx, jobID, destination, true)
}

func (s *Service) fileManaged(ctx context.Context, jobID, destination string, force bool) (ManagedFiling, error) {
	tx, err := s.Jobs.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return ManagedFiling{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE managed_filings SET updated_at = updated_at WHERE job_id = ? AND destination = ?`, jobID, destination); err != nil {
		return ManagedFiling{}, err
	}
	f, err := scanManagedFiling(tx.QueryRowContext(ctx, `SELECT `+managedFilingColumns+` FROM managed_filings f JOIN jobs j ON j.id = f.job_id WHERE f.job_id = ? AND f.destination = ?`, jobID, destination))
	if err != nil {
		return f, err
	}
	if f.Disposition != job.DispositionActive {
		return f, job.ErrConflict
	}
	if f.State == "filed" {
		if err := tx.Commit(); err != nil {
			return f, err
		}
		s.cleanupManagedStages(f)
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
	tx, err = s.Jobs.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return f, err
	}
	defer func() { _ = tx.Rollback() }()
	// The short writer fence serializes the publish/receipt with archive.
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
		return current, tx.Commit()
	}
	f = current
	if stageErr == nil {
		stageErr = staged.Publish()
	}
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
		s.cleanupManagedStages(f)
	}
	return f, nil
}

func (s *Service) cleanupManagedStages(f ManagedFiling) {
	if err := filing.CleanupStages(f.Destination, f.IdempotencyKey); err != nil {
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
