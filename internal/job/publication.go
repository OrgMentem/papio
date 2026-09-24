// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"papio/internal/store"
)

// PublicationRole identifies the acquisition-local role of published bytes.
type PublicationRole string

const (
	PublicationRoleMain         PublicationRole = PublicationRole(ComponentMain)
	PublicationRoleHTMLFullText PublicationRole = PublicationRole(ComponentHTMLFullText)
	PublicationRoleSupplement   PublicationRole = PublicationRole(ComponentSupplement)
	PublicationRoleAppendix     PublicationRole = PublicationRole(ComponentAppendix)
)

// PublicationInput binds one quarantined file to its durable publication work.
// A nil CandidateID and LeaseOwner mean that the publication has neither.
type PublicationInput struct {
	ID               string
	JobID            string
	CandidateID      *int64
	Role             PublicationRole
	SHA256           string
	QuarantinePath   string
	LeaseOwner       *string
	Artifact         Artifact
	FromState        string
	ToState          string
	TransitionDetail map[string]any
}

// PromotionResult reports the result of the one filesystem publication call.
type PromotionResult struct {
	Path    string
	Created bool
}

// PromoteFunc publishes a prepared quarantine file exactly once per finalization
// attempt. It runs while the final transaction holds SQLite's writer fence.
type PromoteFunc func() (PromotionResult, error)

// PreparedPublication is a durable journal row available after a restart.
type PreparedPublication struct {
	PublicationInput
	PreparedAt string
}

// publicationBeforeCommitForTest interrupts finalization after promotion but
// before the final transaction commits. It stays package-private by design.
var publicationBeforeCommitForTest func() error

// publicationAfterCommitForTest simulates an ambiguous commit reply after the
// database commit has succeeded. It stays package-private by design.
var publicationAfterCommitForTest func() error

// IsStandaloneJob reports whether a durable job has no batch membership.
func (js *Store) IsStandaloneJob(ctx context.Context, jobID string) (bool, error) {
	var standalone int
	err := js.S.DB().QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM jobs WHERE id = ?)
		   AND NOT EXISTS(SELECT 1 FROM acquisition_batch_members WHERE job_id = ?)`,
		jobID, jobID).Scan(&standalone)
	if err != nil {
		return false, err
	}
	return standalone != 0, nil
}

// SweepOrphanComponentStages removes abandoned component staging directories.
// A prepared publication owns its quarantine path until finalization consumes it.
func (js *Store) SweepOrphanComponentStages(ctx context.Context) error {
	if js == nil || js.S == nil {
		return errors.New("job store is not initialized")
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Reserve the writer before reading ownership or deleting filesystem state.
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET updated_at = updated_at WHERE id IN (SELECT id FROM jobs LIMIT 1)`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT quarantine_path FROM artifact_publications`)
	if err != nil {
		return err
	}
	owned := make(map[string]struct{})
	for rows.Next() {
		var quarantinePath string
		if err := rows.Scan(&quarantinePath); err != nil {
			_ = rows.Close()
			return err
		}
		owned[filepath.Clean(filepath.Dir(quarantinePath))] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	root := filepath.Join(filepath.Dir(js.S.Path()), "quarantine")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	var cleanupErr error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "component-stage_") {
			continue
		}
		stageDir := filepath.Join(root, entry.Name())
		if _, ok := owned[filepath.Clean(stageDir)]; ok {
			continue
		}
		if err := os.RemoveAll(stageDir); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove orphan component stage %s: %w", entry.Name(), err))
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return cleanupErr
}

// PreparePublication persists the metadata and publication owner before bytes
// can become visible. Repeating the identical preparation is idempotent.
func (js *Store) PreparePublication(ctx context.Context, input PublicationInput) error {
	if err := validatePublicationInput(input); err != nil {
		return err
	}
	detailJSON, err := publicationDetailJSON(input)
	if err != nil {
		return err
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := checkPublicationLeaseTx(ctx, tx, input.JobID, input.LeaseOwner); err != nil {
		return err
	}
	if err := upsertArtifactTx(ctx, tx, input.Artifact); err != nil {
		return err
	}
	candidateID := publicationCandidateValue(input.CandidateID)
	leaseOwner := publicationLeaseValue(input.LeaseOwner)
	fromState, toState, detail := publicationTransitionValues(input, detailJSON)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_publications
			(id, job_id, candidate_id, role, sha256, quarantine_path, lease_owner, from_state, to_state, transition_detail_json, prepared_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		input.ID, input.JobID, candidateID, string(input.Role), input.SHA256,
		input.QuarantinePath, leaseOwner, fromState, toState, detail, store.Now())
	if err != nil {
		return err
	}
	if changed, _ := res.RowsAffected(); changed == 0 {
		existing, err := publicationByIDTx(ctx, tx, input.ID)
		if err != nil {
			return err
		}
		if !samePublication(existing.PublicationInput, input) {
			return fmt.Errorf("%w: publication %s already binds different work", ErrConflict, input.ID)
		}
	}
	return tx.Commit()
}

// FinalizePublication calls promote while holding a SQLite writer reservation.
// It then atomically records the acquisition and consumes the journal row.
// A non-nil error after a successful callback is ambiguous until a fresh Store
// confirms the durable edge with HasPublicationEdge and PreparedPublications.
func (js *Store) FinalizePublication(ctx context.Context, publicationID string, promote PromoteFunc) (PromotionResult, error) {
	if strings.TrimSpace(publicationID) == "" {
		return PromotionResult{}, errors.New("publication ID is required")
	}
	if promote == nil {
		return PromotionResult{}, errors.New("publication promotion is required")
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return PromotionResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// BeginTx is deferred in SQLite. This no-op write obtains the reservation
	// before the callback, so a competing lease replacement cannot enter it.
	res, err := tx.ExecContext(ctx,
		`UPDATE artifact_publications SET prepared_at = prepared_at WHERE id = ?`, publicationID)
	if err != nil {
		return PromotionResult{}, err
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return PromotionResult{}, fmt.Errorf("%w: publication %s is not prepared", ErrConflict, publicationID)
	}
	prepared, err := publicationByIDTx(ctx, tx, publicationID)
	if err != nil {
		return PromotionResult{}, err
	}
	terminal, err := publicationJobTerminalTx(ctx, tx, prepared.JobID)
	if err != nil {
		return PromotionResult{}, err
	}
	if !terminal {
		if err := checkPublicationLeaseTx(ctx, tx, prepared.JobID, prepared.LeaseOwner); err != nil {
			return PromotionResult{}, err
		}
	}

	promoted, err := promote()
	if err != nil {
		return promoted, err
	}
	if err := finalizePublicationTx(ctx, tx, prepared, terminal); err != nil {
		return promoted, err
	}
	if publicationBeforeCommitForTest != nil {
		if err := publicationBeforeCommitForTest(); err != nil {
			return promoted, err
		}
	}
	if err := tx.Commit(); err != nil {
		return promoted, err
	}
	if publicationAfterCommitForTest != nil {
		if err := publicationAfterCommitForTest(); err != nil {
			return promoted, err
		}
	}
	return promoted, nil
}

// LeaseAwaitingHuman claims one parked job for browser adoption. It mirrors the
// ordinary lease predicate but intentionally admits only awaiting_human jobs.
func (js *Store) LeaseAwaitingHuman(ctx context.Context, jobID, owner string, lease time.Duration) (bool, error) {
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(owner) == "" || lease <= 0 {
		return false, errors.New("awaiting-human lease requires a job, owner, and positive duration")
	}
	now := time.Now()
	expires := store.FormatTime(now.Add(lease))
	res, err := js.S.DB().ExecContext(ctx, `
		UPDATE jobs SET lease_owner = ?, lease_expires_at = ?
		 WHERE id = ? AND state = ? AND (lease_owner IS NULL OR lease_expires_at < ?)`,
		owner, expires, jobID, StateAwaitingHuman, store.FormatTime(now))
	if err != nil {
		return false, err
	}
	claimed, err := res.RowsAffected()
	return claimed == 1, err
}

// CandidateIDByKey resolves a candidate only inside its owning job.
func (js *Store) CandidateIDByKey(ctx context.Context, jobID, key string) (int64, error) {
	var id int64
	err := js.S.DB().QueryRowContext(ctx,
		`SELECT id FROM candidates WHERE job_id = ? AND url_key = ?`, jobID, key).Scan(&id)
	return id, err
}

// ReclaimPublicationLease transfers an expired lease-bound journal to recovery.
// It updates the job lease and journal lease in one transaction before retrying
// publication on the new process.
func (js *Store) ReclaimPublicationLease(ctx context.Context, publicationID, owner string, lease time.Duration) (PreparedPublication, error) {
	if strings.TrimSpace(publicationID) == "" || strings.TrimSpace(owner) == "" || lease <= 0 {
		return PreparedPublication{}, errors.New("publication lease reclaim requires an ID, owner, and positive duration")
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return PreparedPublication{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// Take the writer reservation before observing and transferring ownership.
	res, err := tx.ExecContext(ctx,
		`UPDATE artifact_publications SET prepared_at = prepared_at WHERE id = ?`, publicationID)
	if err != nil {
		return PreparedPublication{}, err
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return PreparedPublication{}, fmt.Errorf("%w: publication %s is not prepared", ErrConflict, publicationID)
	}
	prepared, err := publicationByIDTx(ctx, tx, publicationID)
	if err != nil {
		return PreparedPublication{}, err
	}
	if prepared.LeaseOwner == nil {
		return PreparedPublication{}, fmt.Errorf("%w: publication %s has no lease to reclaim", ErrConflict, publicationID)
	}
	now := time.Now()
	expires := store.FormatTime(now.Add(lease))
	res, err = tx.ExecContext(ctx, `
		UPDATE jobs SET lease_owner = ?, lease_expires_at = ?
		 WHERE id = ? AND (lease_owner IS NULL OR lease_expires_at < ?)`,
		owner, expires, prepared.JobID, store.FormatTime(now))
	if err != nil {
		return PreparedPublication{}, err
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return PreparedPublication{}, fmt.Errorf("%w: publication job %s still has an active lease", ErrConflict, prepared.JobID)
	}
	res, err = tx.ExecContext(ctx,
		`UPDATE artifact_publications SET lease_owner = ? WHERE id = ? AND lease_owner = ?`,
		owner, publicationID, *prepared.LeaseOwner)
	if err != nil {
		return PreparedPublication{}, err
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return PreparedPublication{}, fmt.Errorf("%w: publication %s lease changed", ErrConflict, publicationID)
	}
	if err := tx.Commit(); err != nil {
		return PreparedPublication{}, err
	}
	prepared.LeaseOwner = &owner
	return prepared, nil
}

// RebindPublicationLease binds a prepared lease-bound journal to its already
// active worker. It refuses to replace a different or expired job lease.
func (js *Store) RebindPublicationLease(ctx context.Context, publicationID, owner string) (PreparedPublication, error) {
	if strings.TrimSpace(publicationID) == "" || strings.TrimSpace(owner) == "" {
		return PreparedPublication{}, errors.New("publication lease rebind requires an ID and owner")
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return PreparedPublication{}, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`UPDATE artifact_publications SET prepared_at = prepared_at WHERE id = ?`, publicationID)
	if err != nil {
		return PreparedPublication{}, err
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return PreparedPublication{}, fmt.Errorf("%w: publication %s is not prepared", ErrConflict, publicationID)
	}
	prepared, err := publicationByIDTx(ctx, tx, publicationID)
	if err != nil {
		return PreparedPublication{}, err
	}
	if prepared.LeaseOwner == nil {
		return PreparedPublication{}, fmt.Errorf("%w: publication %s has no lease to rebind", ErrConflict, publicationID)
	}
	activeOwner := owner
	if err := checkPublicationLeaseTx(ctx, tx, prepared.JobID, &activeOwner); err != nil {
		return PreparedPublication{}, err
	}
	res, err = tx.ExecContext(ctx,
		`UPDATE artifact_publications SET lease_owner = ? WHERE id = ?`, owner, publicationID)
	if err != nil {
		return PreparedPublication{}, err
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return PreparedPublication{}, fmt.Errorf("%w: publication %s lease changed", ErrConflict, publicationID)
	}
	if err := tx.Commit(); err != nil {
		return PreparedPublication{}, err
	}
	prepared.LeaseOwner = &owner
	return prepared, nil
}

// PreparedPublications lists durable publication owners. An empty job ID lists
// every row and supports process-wide restart recovery.
func (js *Store) PreparedPublications(ctx context.Context, jobID string) ([]PreparedPublication, error) {
	query := `SELECT p.id, p.job_id, p.candidate_id, p.role, p.sha256, p.quarantine_path, p.lease_owner,
		       p.from_state, p.to_state, p.transition_detail_json, p.prepared_at,
		       a.sha256, a.size_bytes, a.mime, COALESCE(a.page_count, 0), COALESCE(a.text_chars, 0),
		       a.ocr_used, a.encrypted, a.has_active_content, COALESCE(a.identity_result, ''), a.path, a.created_at
		  FROM artifact_publications p JOIN artifacts a ON a.sha256 = p.sha256`
	args := []any{}
	if jobID != "" {
		query += ` WHERE job_id = ?`
		args = append(args, jobID)
	}
	query += ` ORDER BY prepared_at, id`
	rows, err := js.S.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PreparedPublication
	for rows.Next() {
		prepared, err := scanPreparedPublication(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, prepared)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, rows.Close()
}

// HasPublicationEdge reports whether a committed acquisition edge owns this
// job, digest, and role. Call this from a fresh Store after an ambiguous commit.
func (js *Store) HasPublicationEdge(ctx context.Context, jobID, sha256 string, role PublicationRole) (bool, error) {
	var one int
	err := js.S.DB().QueryRowContext(ctx,
		`SELECT 1 FROM job_artifacts WHERE job_id = ? AND artifact_sha256 = ? AND role = ? LIMIT 1`,
		jobID, sha256, string(role)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ConsumePublishedPublication deletes one journal row whose acquisition edge is
// already committed for the same job, digest and role. It exists because a
// redundant journal row is not harmless: the scheduler reads any prepared
// publication as unfinished work and refuses to process the job. Artifact
// metadata is left untouched, because the committed edge still owns the digest
// (unlike DiscardPublication, which requires no edge at all). It reports
// whether a row was removed and refuses when no edge owns the digest.
func (js *Store) ConsumePublishedPublication(ctx context.Context, publicationID string) (bool, error) {
	if strings.TrimSpace(publicationID) == "" {
		return false, errors.New("publication ID is required")
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	prepared, err := publicationByIDTx(ctx, tx, publicationID)
	if err != nil {
		return false, err
	}
	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM job_artifacts WHERE job_id = ? AND artifact_sha256 = ? AND role = ? LIMIT 1`,
		prepared.JobID, prepared.SHA256, string(prepared.Role)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w: publication %s has no committed acquisition edge", ErrConflict, publicationID)
	}
	if err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM artifact_publications WHERE id = ?`, publicationID)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	deleted, err := res.RowsAffected()
	return deleted == 1, err
}

// DiscardPublication removes an unfinalized publication after recovery proves
// both the destination and quarantine are absent. It deletes artifact metadata
// only when no edge and no remaining journal row owns the digest.
func (js *Store) DiscardPublication(ctx context.Context, publicationID string) (bool, error) {
	if strings.TrimSpace(publicationID) == "" {
		return false, errors.New("publication ID is required")
	}
	for {
		deleted, err := js.discardPublicationOnce(ctx, publicationID)
		if !publicationBusy(err) {
			return deleted, err
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (js *Store) discardPublicationOnce(ctx context.Context, publicationID string) (bool, error) {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	prepared, err := publicationByIDTx(ctx, tx, publicationID)
	if err != nil {
		return false, err
	}
	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM job_artifacts WHERE artifact_sha256 = ? LIMIT 1`, prepared.SHA256).Scan(&one)
	if err == nil {
		return false, fmt.Errorf("%w: digest %s has a committed acquisition edge", ErrConflict, prepared.SHA256)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM artifact_publications WHERE id = ?`, publicationID); err != nil {
		return false, err
	}
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM artifact_publications WHERE sha256 = ? LIMIT 1`, prepared.SHA256).Scan(&one)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM artifacts WHERE sha256 = ?`, prepared.SHA256)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	deleted, err := res.RowsAffected()
	return deleted == 1, err
}

func publicationBusy(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "database is locked") || strings.Contains(message, "SQLITE_BUSY")
}

func finalizePublicationTx(ctx context.Context, tx *sql.Tx, prepared PreparedPublication, terminal bool) error {
	input := prepared.PublicationInput
	if input.CandidateID != nil {
		res, err := tx.ExecContext(ctx,
			`UPDATE candidates SET status = 'accepted' WHERE id = ? AND job_id = ?`, *input.CandidateID, input.JobID)
		if err != nil {
			return err
		}
		if changed, _ := res.RowsAffected(); changed != 1 {
			return fmt.Errorf("%w: candidate %d does not belong to job %s", ErrConflict, *input.CandidateID, input.JobID)
		}
	}
	if input.Role == PublicationRoleMain && !terminal {
		if err := transitionPublicationMainTx(ctx, tx, input); err != nil {
			return err
		}
	}
	if terminal {
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET lease_owner = NULL, lease_expires_at = NULL WHERE id = ?`, input.JobID); err != nil {
			return err
		}
	}
	if err := addPublicationEdgeTx(ctx, tx, input); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM artifact_publications WHERE id = ?`, input.ID)
	if err != nil {
		return err
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return fmt.Errorf("%w: publication %s disappeared during finalization", ErrConflict, input.ID)
	}
	return nil
}

func transitionPublicationMainTx(ctx context.Context, tx *sql.Tx, input PublicationInput) error {
	if input.FromState == "" {
		return nil
	}
	if !allowed[input.FromState][input.ToState] {
		return fmt.Errorf("%w: %s -> %s not allowed", ErrConflict, input.FromState, input.ToState)
	}
	detailJSON, err := publicationDetailJSON(input)
	if err != nil {
		return err
	}
	now := store.Now()
	releaseLease := releasesLease(input.ToState)
	candidate := publicationCandidateValue(input.CandidateID)
	res, err := tx.ExecContext(ctx, `
		UPDATE jobs SET state = ?, updated_at = ?, retry_at = NULL, artifact_sha256 = ?,
			selected_candidate_id = COALESCE(?, selected_candidate_id),
			lease_owner = CASE WHEN ? THEN NULL ELSE lease_owner END,
			lease_expires_at = CASE WHEN ? THEN NULL ELSE lease_expires_at END
		WHERE id = ? AND state = ?`,
		input.ToState, now, input.SHA256, candidate, releaseLease, releaseLease,
		input.JobID, input.FromState)
	if err != nil {
		return err
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return fmt.Errorf("%w: job %s not in state %s", ErrConflict, input.JobID, input.FromState)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, 'job.transition', ?)`,
		input.JobID, now, detailJSON); err != nil {
		return err
	}
	if input.ToState == StateReady && input.Role == PublicationRoleMain {
		if err := recordArtifactProducerTx(ctx, tx, input, now); err != nil {
			return err
		}
	}
	if Terminal(input.ToState) {
		return closeTerminalHumanActions(ctx, tx, input.JobID, input.ToState, now)
	}
	return nil
}

func addPublicationEdgeTx(ctx context.Context, tx *sql.Tx, input PublicationInput) error {
	candidate := publicationCandidateValue(input.CandidateID)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO job_artifacts (job_id, artifact_sha256, role, candidate_id, identity_result, created_at)
		SELECT ?, ?, ?, ?, a.identity_result, ? FROM artifacts a WHERE a.sha256 = ?
		ON CONFLICT(job_id, role, artifact_sha256) DO UPDATE SET
			candidate_id = excluded.candidate_id,
			identity_result = excluded.identity_result`,
		input.JobID, input.SHA256, string(input.Role), candidate, store.Now(), input.SHA256)
	return err
}

func publicationJobTerminalTx(ctx context.Context, tx *sql.Tx, jobID string) (bool, error) {
	var state string
	err := tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, jobID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w: publication job %s is missing", ErrConflict, jobID)
	}
	if err != nil {
		return false, err
	}
	return Terminal(state), nil
}

func checkPublicationLeaseTx(ctx context.Context, tx *sql.Tx, jobID string, leaseOwner *string) error {
	if leaseOwner == nil {
		return nil
	}
	var one int
	err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM jobs
		 WHERE id = ? AND lease_owner = ? AND lease_expires_at >= ?`,
		jobID, *leaseOwner, store.Now()).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: publication lease on %s is not held by %s", ErrConflict, jobID, *leaseOwner)
	}
	return err
}

func upsertArtifactTx(ctx context.Context, tx *sql.Tx, a Artifact) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO artifacts (sha256, size_bytes, mime, page_count, text_chars, ocr_used, encrypted, has_active_content, identity_result, path, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(sha256) DO UPDATE SET identity_result = excluded.identity_result`,
		a.SHA256, a.SizeBytes, a.MIME, a.PageCount, a.TextChars, boolInt(a.OCRUsed), boolInt(a.Encrypted),
		boolInt(a.HasActiveContent), nullable(a.IdentityResult), a.Path, store.Now())
	return err
}

func publicationByIDTx(ctx context.Context, tx *sql.Tx, publicationID string) (PreparedPublication, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT p.id, p.job_id, p.candidate_id, p.role, p.sha256, p.quarantine_path, p.lease_owner,
		       p.from_state, p.to_state, p.transition_detail_json, p.prepared_at,
		       a.sha256, a.size_bytes, a.mime, COALESCE(a.page_count, 0), COALESCE(a.text_chars, 0),
		       a.ocr_used, a.encrypted, a.has_active_content, COALESCE(a.identity_result, ''), a.path, a.created_at
		  FROM artifact_publications p JOIN artifacts a ON a.sha256 = p.sha256 WHERE p.id = ?`, publicationID)
	return scanPreparedPublication(row)
}

type publicationScanner interface {
	Scan(dest ...any) error
}

func scanPreparedPublication(scanner publicationScanner) (PreparedPublication, error) {
	var prepared PreparedPublication
	var candidateID sql.NullInt64
	var leaseOwner, fromState, toState, detailJSON sql.NullString
	var ocrUsed, encrypted, active int
	err := scanner.Scan(
		&prepared.ID, &prepared.JobID, &candidateID, &prepared.Role, &prepared.SHA256, &prepared.QuarantinePath,
		&leaseOwner, &fromState, &toState, &detailJSON, &prepared.PreparedAt,
		&prepared.Artifact.SHA256, &prepared.Artifact.SizeBytes, &prepared.Artifact.MIME, &prepared.Artifact.PageCount,
		&prepared.Artifact.TextChars, &ocrUsed, &encrypted, &active, &prepared.Artifact.IdentityResult,
		&prepared.Artifact.Path, &prepared.Artifact.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PreparedPublication{}, fmt.Errorf("%w: publication is not prepared", ErrConflict)
	}
	if err != nil {
		return PreparedPublication{}, err
	}
	prepared.Artifact.OCRUsed = ocrUsed != 0
	prepared.Artifact.Encrypted = encrypted != 0
	prepared.Artifact.HasActiveContent = active != 0
	if candidateID.Valid {
		id := candidateID.Int64
		prepared.CandidateID = &id
	}
	if leaseOwner.Valid {
		owner := leaseOwner.String
		prepared.LeaseOwner = &owner
	}
	if fromState.Valid {
		prepared.FromState = fromState.String
		prepared.ToState = toState.String
		if err := json.Unmarshal([]byte(detailJSON.String), &prepared.TransitionDetail); err != nil {
			return PreparedPublication{}, fmt.Errorf("decode publication transition detail: %w", err)
		}
	}
	return prepared, nil
}

func validatePublicationInput(input PublicationInput) error {
	if strings.TrimSpace(input.ID) == "" || len(input.ID) > 128 {
		return errors.New("publication ID is required and must be at most 128 bytes")
	}
	if strings.TrimSpace(input.JobID) == "" {
		return errors.New("publication job ID is required")
	}
	if input.CandidateID != nil && *input.CandidateID <= 0 {
		return errors.New("publication candidate ID must be positive")
	}
	switch input.Role {
	case PublicationRoleMain, PublicationRoleHTMLFullText, PublicationRoleSupplement, PublicationRoleAppendix:
	default:
		return fmt.Errorf("unknown publication role %q", input.Role)
	}
	if len(input.SHA256) != 64 {
		return errors.New("publication SHA-256 must be 64 characters")
	}
	if strings.TrimSpace(input.QuarantinePath) == "" || len(input.QuarantinePath) > 4096 {
		return errors.New("publication quarantine path is required and must be at most 4096 bytes")
	}
	if input.LeaseOwner != nil && strings.TrimSpace(*input.LeaseOwner) == "" {
		return errors.New("publication lease owner must not be empty")
	}
	if (input.FromState == "") != (input.ToState == "") {
		return errors.New("publication transition requires both from and to states")
	}
	if input.FromState == "" && input.TransitionDetail != nil {
		return errors.New("publication transition detail requires a transition")
	}
	if input.FromState != "" && input.Role != PublicationRoleMain {
		return errors.New("only a main publication may transition a job")
	}
	if input.Artifact.SHA256 != input.SHA256 {
		return errors.New("publication artifact SHA-256 does not match publication SHA-256")
	}
	if input.Artifact.SizeBytes < 0 || strings.TrimSpace(input.Artifact.MIME) == "" || strings.TrimSpace(input.Artifact.Path) == "" {
		return errors.New("publication artifact metadata is incomplete")
	}
	return nil
}

func publicationDetailJSON(input PublicationInput) (string, error) {
	if input.FromState == "" {
		return "", nil
	}
	detail := make(map[string]any, len(input.TransitionDetail)+2)
	for key, value := range input.TransitionDetail {
		detail[key] = value
	}
	detail["from"] = input.FromState
	detail["to"] = input.ToState
	encoded, err := json.Marshal(detail)
	return string(encoded), err
}

func publicationCandidateValue(candidateID *int64) any {
	if candidateID == nil {
		return nil
	}
	return *candidateID
}

func publicationLeaseValue(leaseOwner *string) any {
	if leaseOwner == nil {
		return nil
	}
	return *leaseOwner
}

func publicationTransitionValues(input PublicationInput, detailJSON string) (any, any, any) {
	if input.FromState == "" {
		return nil, nil, nil
	}
	return input.FromState, input.ToState, detailJSON
}

func samePublication(left, right PublicationInput) bool {
	leftDetail, leftErr := publicationDetailJSON(left)
	rightDetail, rightErr := publicationDetailJSON(right)
	return leftErr == nil && rightErr == nil &&
		left.JobID == right.JobID &&
		publicationCandidateEqual(left.CandidateID, right.CandidateID) &&
		left.Role == right.Role && left.SHA256 == right.SHA256 &&
		left.QuarantinePath == right.QuarantinePath &&
		publicationStringEqual(left.LeaseOwner, right.LeaseOwner) &&
		left.FromState == right.FromState && left.ToState == right.ToState && leftDetail == rightDetail &&
		samePublicationArtifact(left.Artifact, right.Artifact)
}

func publicationCandidateEqual(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func publicationStringEqual(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func samePublicationArtifact(left, right Artifact) bool {
	return left.SHA256 == right.SHA256 &&
		left.SizeBytes == right.SizeBytes && left.MIME == right.MIME &&
		left.PageCount == right.PageCount && left.TextChars == right.TextChars &&
		left.OCRUsed == right.OCRUsed && left.Encrypted == right.Encrypted &&
		left.HasActiveContent == right.HasActiveContent &&
		left.IdentityResult == right.IdentityResult && left.Path == right.Path
}
