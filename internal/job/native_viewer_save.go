// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"papio/internal/store"
)

// NativeViewerSaveStrategy is reserved for the manual-action continuation.
// It uses global effect occupancy without creating a provider drive epoch.
const NativeViewerSaveStrategy = "native_viewer_save"

var ErrNativeViewerSaveConsumed = errors.New("native viewer save operation or step already consumed")

// NativeViewerSaveInput contains only opaque authority, never a source URL or
// filesystem path. The bridge supplies a binding digest after observing the
// exact live viewer. ExplicitSelection means the operator explicitly selected
// this manual action; otherwise its diagnosis must name a native viewer.
type NativeViewerSaveInput struct {
	RequestID          string `json:"request_id"`
	OperationID        string `json:"operation_id"`
	JobID              string `json:"job_id"`
	ActionID           int64  `json:"action_id"`
	ActionRevision     int64  `json:"action_revision"`
	JobAttemptRevision int64  `json:"job_attempt_revision"`
	HolderGeneration   int64  `json:"holder_generation"`
	BindingSHA256      string `json:"binding_sha256"`
	SafetyDomainID     string `json:"safety_domain_id"`
	ExpiresAtMS        int64  `json:"expires_at_ms"`
	ExplicitSelection  bool   `json:"explicit_selection"`
}

type NativeViewerSaveReservation struct {
	NativeViewerSaveInput
	PermitID string                   `json:"permit_id"`
	Producer ArtifactProducerIdentity `json:"producer"`
}

func (r NativeViewerSaveReservation) Filename() string {
	return "papio-viewer-" + r.OperationID + ".pdf"
}

func nativeViewerID(s string) bool {
	return nonempty(s) && len(s) <= 128 && !strings.ContainsAny(s, "/\\\x00\r\n:")
}

func (in NativeViewerSaveInput) valid(now time.Time) bool {
	suffix := strings.TrimPrefix(in.OperationID, "viewer_")
	_, idErr := hex.DecodeString(suffix)
	return strings.HasPrefix(in.OperationID, "viewer_") && len(suffix) == 26 && idErr == nil && strings.ToLower(suffix) == suffix &&
		nativeViewerID(in.RequestID) && nativeViewerID(in.JobID) && in.ActionID > 0 &&
		in.ActionRevision > 0 && in.JobAttemptRevision > 0 && in.HolderGeneration > 0 &&
		nativeSHA(in.BindingSHA256) && nonempty(in.SafetyDomainID) && in.ExpiresAtMS > now.UnixMilli() && in.ExpiresAtMS <= now.Add(2*time.Minute).UnixMilli()
}

// nativeViewerActionTx deliberately does not accept an open handoff in place of
// the selected action. A revision change invalidates the entire continuation.
func nativeViewerActionTx(ctx context.Context, tx *sql.Tx, in NativeViewerSaveInput) error {
	var present int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM human_actions a JOIN jobs j ON j.id=a.job_id
		WHERE a.id=? AND a.revision=? AND a.job_id=? AND a.status='open' AND a.kind='manual_download'
		AND (? OR a.diagnosis=?) AND j.state='awaiting_human' AND j.artifact_sha256 IS NULL
		AND ?=1+(SELECT COUNT(*) FROM events WHERE job_id=j.id AND kind='job.retry_requested')`,
		in.ActionID, in.ActionRevision, in.JobID, in.ExplicitSelection, DiagnosisReasonNativeViewerDownload, in.JobAttemptRevision).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEffectPermitStale
	}
	return err
}

func nativeViewerGenerationTx(ctx context.Context, tx *sql.Tx, generation int64) error {
	var current int64
	if err := tx.QueryRowContext(ctx, `SELECT holder_generation FROM daemon_authority_key WHERE singleton=1`).Scan(&current); err != nil {
		return err
	}
	if current != generation {
		return ErrEffectPermitStale
	}
	return nil
}

// ReserveNativeViewerSave consumes one action revision permanently, including
// after restart or a lost reply. A live bridge may cache an exact reply, but
// calling this method again never grants another dispatch or baseline.
func (js *Store) ReserveNativeViewerSave(ctx context.Context, in NativeViewerSaveInput, now time.Time) (NativeViewerSaveReservation, error) {
	var zero NativeViewerSaveReservation
	if !in.valid(now) {
		return zero, ErrEffectPermitStale
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer func() { _ = tx.Rollback() }()
	// Take the write lock before reading the action and the one-shot latch.
	res, err := tx.ExecContext(ctx, `UPDATE human_actions SET revision=revision WHERE id=? AND revision=? AND status='open'`, in.ActionID, in.ActionRevision)
	if err != nil {
		return zero, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return zero, err
		}
		return zero, ErrEffectPermitStale
	}
	if err := nativeViewerActionTx(ctx, tx, in); err != nil {
		return zero, err
	}
	if err := nativeViewerGenerationTx(ctx, tx, in.HolderGeneration); err != nil {
		return zero, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=? AND kind='browser.native_viewer_save_reserved'
		AND json_extract(detail_json,'$.action_id')=? AND json_extract(detail_json,'$.action_revision')=?`, in.JobID, in.ActionID, in.ActionRevision).Scan(&count); err != nil {
		return zero, err
	}
	if count != 0 {
		return zero, ErrNativeViewerSaveConsumed
	}
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM effect_permits WHERE status IN ('held','unknown_completion'))+
		(SELECT COUNT(*) FROM legacy_effect_blockers WHERE status='unresolved')`).Scan(&count); err != nil {
		return zero, err
	}
	if count != 0 {
		return zero, ErrEffectPermitBusy
	}
	// Operation IDs are minted by the bridge before pure helper preparation.
	// Reusing one for another action would alias helper/file ownership.
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE kind='browser.native_viewer_save_reserved'
		AND json_extract(detail_json,'$.operation_id')=?`, in.OperationID).Scan(&count); err != nil {
		return zero, err
	}
	if count != 0 {
		return zero, ErrNativeViewerSaveConsumed
	}
	r := NativeViewerSaveReservation{NativeViewerSaveInput: in, PermitID: NewID("permit")}
	ordinal := int64(0)
	r.Producer = ArtifactProducerIdentity{Kind: GenericDrive, DriveAttemptID: r.OperationID, Ordinal: &ordinal, Strategy: NativeViewerSaveStrategy, Revision: in.BindingSHA256}
	stamp := store.Now()
	_, err = tx.ExecContext(ctx, `INSERT INTO effect_permits
		(id,job_id,job_attempt_revision,browser_holder_generation,safety_domain_id,effect_kind,slot_index,
		drive_attempt_id,ordinal,strategy,revision,status,lease_until,created_at,updated_at)
		VALUES(?,?,?,?,?,'generic_drive',0,?,0,?,?,'held',?,?,?)`, r.PermitID, in.JobID, in.JobAttemptRevision,
		in.HolderGeneration, in.SafetyDomainID, r.OperationID, NativeViewerSaveStrategy, in.BindingSHA256,
		time.UnixMilli(in.ExpiresAtMS).UTC().Format(time.RFC3339Nano), stamp, stamp)
	if err != nil {
		return zero, err
	}
	if err := nativeEventTx(ctx, tx, in.JobID, "browser.native_viewer_save_reserved", r); err != nil {
		return zero, err
	}
	if err := tx.Commit(); err != nil {
		return zero, err
	}
	return r, nil
}

func nativeViewerCurrentTx(ctx context.Context, tx *sql.Tx, r NativeViewerSaveReservation, now time.Time) error {
	if !r.NativeViewerSaveInput.valid(now) || !nativeViewerID(r.OperationID) || !nativeViewerID(r.PermitID) {
		return ErrEffectPermitStale
	}
	p, err := nativeViewerRecordTx(ctx, tx, r)
	if err != nil {
		return err
	}
	if p.Status != Held || p.LeaseUntil == nil || !p.LeaseUntil.After(now) {
		return ErrEffectPermitStale
	}
	if err := nativeViewerActionTx(ctx, tx, r.NativeViewerSaveInput); err != nil {
		return err
	}
	return nativeViewerGenerationTx(ctx, tx, r.HolderGeneration)
}

// Historical cleanup authenticates the immutable reservation without requiring
// current authority. It can stop occupancy; it can never dispatch or admit.
func nativeViewerRecordTx(ctx context.Context, tx *sql.Tx, r NativeViewerSaveReservation) (*EffectPermit, error) {
	ordinal := int64(0)
	expected := ArtifactProducerIdentity{Kind: GenericDrive, DriveAttemptID: r.OperationID, Ordinal: &ordinal, Strategy: NativeViewerSaveStrategy, Revision: r.BindingSHA256}
	if !artifactProducerEqual(r.Producer, expected) {
		return nil, ErrEffectPermitStale
	}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT detail_json FROM events WHERE job_id=? AND kind='browser.native_viewer_save_reserved'
		AND json_extract(detail_json,'$.operation_id')=?`, r.JobID, r.OperationID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEffectPermitStale
	}
	if err != nil {
		return nil, err
	}
	var stored NativeViewerSaveReservation
	if json.Unmarshal([]byte(raw), &stored) != nil {
		return nil, ErrEffectPermitStale
	}
	want, _ := json.Marshal(r)
	got, _ := json.Marshal(stored)
	if string(want) != string(got) {
		return nil, ErrEffectPermitStale
	}
	p, err := scanPermit(tx.QueryRowContext(ctx, permitSelect+` WHERE id=?`, r.PermitID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEffectPermitStale
	}
	if err != nil {
		return nil, err
	}
	if p.Kind != GenericDrive || p.Strategy != NativeViewerSaveStrategy || p.JobID != r.JobID || p.DriveAttemptID != r.OperationID ||
		p.Ordinal == nil || *p.Ordinal != 0 || p.Revision != r.BindingSHA256 || p.JobAttemptRevision != r.JobAttemptRevision ||
		p.BrowserHolderGeneration != r.HolderGeneration || p.SafetyDomainID != r.SafetyDomainID {
		return nil, ErrEffectPermitStale
	}
	return p, nil
}

// CancelNativeViewerSave performs exact historical cleanup after the bridge
// stops its helper. No begun step proves non-dispatch, so that permit settles.
// Any begun step remains unknown: closing the helper cannot undo Firefox's
// already-submitted save. Neither case removes the action-revision latch.
func (js *Store) CancelNativeViewerSave(ctx context.Context, r NativeViewerSaveReservation, now time.Time) (EffectPermitStatus, error) {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	if err := nativeViewerWriteLock(ctx, tx, r); err != nil {
		return "", err
	}
	p, err := nativeViewerRecordTx(ctx, tx, r)
	if err != nil {
		return "", err
	}
	if p.Status == Settled {
		return Settled, nil
	}
	if p.Status != Held && p.Status != UnknownCompletion {
		return "", ErrEffectPermitStale
	}
	var steps int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=? AND kind='browser.native_viewer_save_step'
		AND json_extract(detail_json,'$.operation_id')=?`, r.JobID, r.OperationID).Scan(&steps); err != nil {
		return "", err
	}
	next := UnknownCompletion
	if steps == 0 {
		next = Settled
		if err := nativeEventTx(ctx, tx, r.JobID, "browser.native_viewer_save_result", map[string]any{
			"drive_attempt_id": r.OperationID, "ordinal": int64(0), "strategy": NativeViewerSaveStrategy,
			"revision": r.BindingSHA256, "safety_domain": r.SafetyDomainID, "outcome": "not_dispatched", "cleanup_only": true,
		}); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE effect_permits SET status=?,updated_at=? WHERE id=?`, next, now.UTC().Format(time.RFC3339Nano), r.PermitID); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return next, nil
}

// CheckNativeViewerSave is a read-only check, not a dispatch authorization.
// BeginNativeViewerSaveStep must commit immediately before each helper effect.
func (js *Store) CheckNativeViewerSave(ctx context.Context, r NativeViewerSaveReservation, now time.Time) error {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return nativeViewerCurrentTx(ctx, tx, r, now)
}

func nativeViewerWriteLock(ctx context.Context, tx *sql.Tx, r NativeViewerSaveReservation) error {
	_, err := tx.ExecContext(ctx, `UPDATE effect_permits SET updated_at=updated_at WHERE id=?`, r.PermitID)
	return err
}

// BeginNativeViewerSaveStep consumes a stable request ID before dispatch.
// The bridge serializes the deterministic helper, retains its stage, checks the
// live document, and never treats an uncertain step as permission to repeat it.
func (js *Store) BeginNativeViewerSaveStep(ctx context.Context, r NativeViewerSaveReservation, requestID string, now time.Time) error {
	if !nativeViewerID(requestID) || requestID == r.RequestID {
		return ErrEffectPermitStale
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := nativeViewerWriteLock(ctx, tx, r); err != nil {
		return err
	}
	if err := nativeViewerCurrentTx(ctx, tx, r, now); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=? AND json_extract(detail_json,'$.operation_id')=?
		AND (kind='browser.native_viewer_save_admitted' OR (kind='browser.native_viewer_save_step' AND json_extract(detail_json,'$.request_id')=?))`, r.JobID, r.OperationID, requestID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return ErrNativeViewerSaveConsumed
	}
	// Advance also polls pending native dialogs. The operation deadline and
	// the bridge's deterministic helper bound execution; polls are not effects.
	if err := nativeEventTx(ctx, tx, r.JobID, "browser.native_viewer_save_step", map[string]any{
		"operation_id": r.OperationID, "permit_id": r.PermitID, "request_id": requestID,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// AdmitNativeViewerSave records daemon-verified bytes before they become
// sweep-visible. The bridge must have observed final Save dispatch and copied
// the exact new stable file through its pinned baseline; a helper assertion or
// download filename alone is not sufficient. No source path enters this API.
func (js *Store) AdmitNativeViewerSave(ctx context.Context, r NativeViewerSaveReservation, filename, digest string, size int64, now time.Time) error {
	if filename != r.Filename() || !nativeSHA(digest) || size < 1 {
		return ErrEffectPermitStale
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := nativeViewerWriteLock(ctx, tx, r); err != nil {
		return err
	}
	if err := nativeViewerCurrentTx(ctx, tx, r, now); err != nil {
		return err
	}
	var admitted, steps int
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(kind='browser.native_viewer_save_admitted'),0), COALESCE(SUM(kind='browser.native_viewer_save_step'),0)
		FROM events WHERE job_id=? AND json_extract(detail_json,'$.operation_id')=?`, r.JobID, r.OperationID).Scan(&admitted, &steps); err != nil {
		return err
	}
	if admitted != 0 {
		return ErrNativeViewerSaveConsumed
	}
	if steps == 0 {
		return ErrEffectPermitStale
	}
	if err := nativeEventTx(ctx, tx, r.JobID, "browser.native_viewer_save_admitted", map[string]any{
		"operation_id": r.OperationID, "permit_id": r.PermitID, "action_id": r.ActionID, "action_revision": r.ActionRevision,
		"job_attempt_revision": r.JobAttemptRevision, "holder_generation": r.HolderGeneration, "binding_sha256": r.BindingSHA256,
		"filename": filename, "sha256": digest, "size_bytes": size, "producer": r.Producer,
	}); err != nil {
		return err
	}
	if err := nativeEventTx(ctx, tx, r.JobID, "browser.download_complete", map[string]any{
		"filename": filename, "sha256": digest, "size_bytes": size, "producer": r.Producer,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

type nativeViewerAdmission struct {
	OperationID        string                   `json:"operation_id"`
	PermitID           string                   `json:"permit_id"`
	ActionID           int64                    `json:"action_id"`
	ActionRevision     int64                    `json:"action_revision"`
	JobAttemptRevision int64                    `json:"job_attempt_revision"`
	HolderGeneration   int64                    `json:"holder_generation"`
	BindingSHA256      string                   `json:"binding_sha256"`
	Filename           string                   `json:"filename"`
	SHA256             string                   `json:"sha256"`
	SizeBytes          int64                    `json:"size_bytes"`
	Producer           ArtifactProducerIdentity `json:"producer"`
}

// nativeViewerAdmissionTx authenticates historical, daemon-admitted bytes. It
// deliberately does not require a live holder/lease: a sweep can finish already
// admitted bytes after restart, but cannot use this proof to repeat a save.
func nativeViewerAdmissionTx(ctx context.Context, tx *sql.Tx, jobID, operationID string) (NativeViewerSaveReservation, nativeViewerAdmission, error) {
	var r NativeViewerSaveReservation
	var a nativeViewerAdmission
	var count int
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MIN(detail_json),'') FROM events
		WHERE job_id=? AND kind='browser.native_viewer_save_admitted' AND json_extract(detail_json,'$.operation_id')=?`, jobID, operationID).Scan(&count, &raw)
	if err != nil {
		return r, a, err
	}
	if count != 1 || json.Unmarshal([]byte(raw), &a) != nil {
		return r, a, ErrEffectPermitStale
	}
	err = tx.QueryRowContext(ctx, `SELECT detail_json FROM events WHERE job_id=? AND kind='browser.native_viewer_save_reserved'
		AND json_extract(detail_json,'$.operation_id')=?`, jobID, operationID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return r, a, ErrEffectPermitStale
	}
	if err != nil {
		return r, a, err
	}
	if json.Unmarshal([]byte(raw), &r) != nil {
		return r, a, ErrEffectPermitStale
	}
	if r.JobID != jobID || r.OperationID != operationID || a.OperationID != operationID ||
		a.PermitID != r.PermitID || a.ActionID != r.ActionID || a.ActionRevision != r.ActionRevision ||
		a.JobAttemptRevision != r.JobAttemptRevision || a.HolderGeneration != r.HolderGeneration ||
		a.BindingSHA256 != r.BindingSHA256 || a.Filename != r.Filename() || !nativeSHA(a.SHA256) || a.SizeBytes < 1 ||
		!artifactProducerEqual(a.Producer, r.Producer) {
		return r, a, ErrEffectPermitStale
	}
	_, err = nativeViewerRecordTx(ctx, tx, r)
	return r, a, err
}

func nativeViewerProducerAdmissionTx(ctx context.Context, tx *sql.Tx, jobID string, producer ArtifactProducerIdentity) (nativeViewerAdmission, error) {
	_, a, err := nativeViewerAdmissionTx(ctx, tx, jobID, producer.DriveAttemptID)
	if err != nil {
		return a, err
	}
	if !artifactProducerEqual(a.Producer, producer) {
		return a, ErrEffectPermitStale
	}
	return a, nil
}

// TransitionAwaitingToValidatingForNativeViewer is the adoption boundary for
// papio-viewer-* files, including sweeps. Receipt, exact bytes/candidate, original
// action revision and attempt are checked in the same transaction as the state
// change. A missing receipt never falls back to ordinary any-open-action adoption.
// An infrastructure retry uses the recorded adoption closure and exact repark
// history; it never reopens an action or grants another browser effect.
func (js *Store) TransitionAwaitingToValidatingForNativeViewer(ctx context.Context, jobID string, candidateID int64, filename, digest string) error {
	if !strings.HasPrefix(filename, "papio-viewer-") || !strings.HasSuffix(filename, ".pdf") || !nativeSHA(digest) || candidateID < 1 {
		return ErrAdoptNotAwaiting
	}
	operationID := strings.TrimSuffix(strings.TrimPrefix(filename, "papio-viewer-"), ".pdf")
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Serialize against dismissal, revision edits, cancellation and retries.
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET updated_at=updated_at WHERE id=?`, jobID); err != nil {
		return err
	}
	r, admission, err := nativeViewerAdmissionTx(ctx, tx, jobID, operationID)
	if errors.Is(err, ErrEffectPermitStale) {
		return fmt.Errorf("%w: %w", ErrAdoptNotAwaiting, err)
	}
	if err != nil {
		return err
	}
	if admission.Filename != filename || admission.SHA256 != digest {
		return ErrAdoptNotAwaiting
	}
	now := store.Now()
	proof := nativeViewerValidationTransition{Reason: "adopt_browser_download", Source: "browser", From: StateAwaitingHuman, To: StateValidating,
		OperationID: r.OperationID, ActionID: r.ActionID, ActionRevision: r.ActionRevision, JobAttemptRevision: r.JobAttemptRevision,
		CandidateID: candidateID, SHA256: digest}
	actionErr := nativeViewerActionTx(ctx, tx, r.NativeViewerSaveInput)
	switch {
	case actionErr == nil:
		var previous int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=? AND kind='job.transition'
			AND json_extract(detail_json,'$.native_viewer_operation_id')=?`, jobID, r.OperationID).Scan(&previous); err != nil {
			return err
		}
		if previous != 0 {
			return ErrAdoptNotAwaiting
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM human_actions WHERE job_id=?`, jobID).Scan(&proof.ActionSetMaxID); err != nil {
			return err
		}
		proof.ActionClosedByAdoption, proof.ActionResolvedAt = true, now
		res, err := tx.ExecContext(ctx, `UPDATE human_actions SET status='resolved',resolved_at=? WHERE id=? AND job_id=? AND revision=? AND status='open'`, now, r.ActionID, jobID, r.ActionRevision)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return ErrAdoptNotAwaiting
		}
	case errors.Is(actionErr, ErrEffectPermitStale):
		initial, err := nativeViewerValidationRetryTx(ctx, tx, r, candidateID, digest)
		if err != nil {
			return err
		}
		proof.ActionResolvedAt, proof.ActionSetMaxID = initial.ActionResolvedAt, initial.ActionSetMaxID
	default:
		return actionErr
	}
	var matched int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM candidates WHERE id=? AND job_id=? AND source='browser' AND url_key=?`, candidateID, jobID, "browser-adopt:sha256:"+digest).Scan(&matched)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAdoptNotAwaiting
	}
	if err != nil {
		return err
	}
	detail, err := json.Marshal(proof)
	if err != nil {
		return err
	}
	if err := js.TransitionTx(ctx, tx, jobID, StateAwaitingHuman, StateValidating, string(detail), TransitionTxConfig{}, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET selected_candidate_id=? WHERE id=?`, candidateID, jobID); err != nil {
		return err
	}
	return tx.Commit()
}

// These fields extend the existing state transition, not a second ledger. Only
// the first transition closes the action; subsequent retries retain its proof.
type nativeViewerValidationTransition struct {
	Reason                 string `json:"reason"`
	Source                 string `json:"source"`
	From                   string `json:"from"`
	To                     string `json:"to"`
	OperationID            string `json:"native_viewer_operation_id"`
	ActionID               int64  `json:"action_id"`
	ActionRevision         int64  `json:"action_revision"`
	JobAttemptRevision     int64  `json:"job_attempt_revision"`
	CandidateID            int64  `json:"candidate_id"`
	SHA256                 string `json:"sha256"`
	ActionClosedByAdoption bool   `json:"native_viewer_action_closed,omitempty"`
	ActionResolvedAt       string `json:"native_viewer_action_resolved_at"`
	ActionSetMaxID         int64  `json:"native_viewer_action_set_max_id"`
}

func (p nativeViewerValidationTransition) matches(r NativeViewerSaveReservation, candidateID int64, digest string) bool {
	return p.Reason == "adopt_browser_download" && p.Source == "browser" && p.From == StateAwaitingHuman && p.To == StateValidating &&
		p.OperationID == r.OperationID && p.ActionID == r.ActionID && p.ActionRevision == r.ActionRevision &&
		p.JobAttemptRevision == r.JobAttemptRevision && p.CandidateID == candidateID && p.SHA256 == digest &&
		p.ActionResolvedAt != "" && p.ActionSetMaxID >= r.ActionID
}

func nativeViewerValidationRetryTx(ctx context.Context, tx *sql.Tx, r NativeViewerSaveReservation, candidateID int64, digest string) (nativeViewerValidationTransition, error) {
	var initial nativeViewerValidationTransition
	var raw string
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MIN(detail_json),'') FROM events WHERE job_id=? AND kind='job.transition'
		AND json_extract(detail_json,'$.native_viewer_operation_id')=? AND json_extract(detail_json,'$.native_viewer_action_closed')=1`, r.JobID, r.OperationID).Scan(&count, &raw)
	if err != nil {
		return initial, err
	}
	if count != 1 || json.Unmarshal([]byte(raw), &initial) != nil || !initial.matches(r, candidateID, digest) {
		return initial, ErrAdoptNotAwaiting
	}
	// The original action must still be exactly the one adoption resolved. A
	// replacement remains a veto after closure, as does another open task.
	var eligible int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM jobs j JOIN human_actions a ON a.job_id=j.id
		WHERE j.id=? AND j.state='awaiting_human' AND j.artifact_sha256 IS NULL AND j.selected_candidate_id=?
		AND a.id=? AND a.kind='manual_download' AND a.revision=? AND a.status='resolved' AND a.resolved_at=?
		AND ?=1+(SELECT COUNT(*) FROM events WHERE job_id=j.id AND kind='job.retry_requested')
		AND NOT EXISTS(SELECT 1 FROM human_actions x WHERE x.job_id=j.id AND (x.id>? OR (x.status='open' AND x.kind<>?)))`,
		r.JobID, candidateID, r.ActionID, r.ActionRevision, initial.ActionResolvedAt, r.JobAttemptRevision, initial.ActionSetMaxID, informationalActionKind).Scan(&eligible)
	if errors.Is(err, sql.ErrNoRows) {
		return initial, ErrAdoptNotAwaiting
	}
	if err != nil {
		return initial, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT detail_json FROM events WHERE job_id=? AND kind='job.transition' ORDER BY seq DESC LIMIT 2`, r.JobID)
	if err != nil {
		return initial, err
	}
	defer rows.Close()
	var transitions []nativeViewerValidationTransition
	for rows.Next() {
		var p nativeViewerValidationTransition
		if err := rows.Scan(&raw); err != nil {
			return initial, err
		}
		if json.Unmarshal([]byte(raw), &p) != nil {
			return initial, ErrAdoptNotAwaiting
		}
		transitions = append(transitions, p)
	}
	if err := rows.Err(); err != nil {
		return initial, err
	}
	if len(transitions) != 2 || transitions[0].Reason != "adoption_validation_error" || transitions[0].From != StateValidating || transitions[0].To != StateAwaitingHuman ||
		!transitions[1].matches(r, candidateID, digest) || transitions[1].ActionResolvedAt != initial.ActionResolvedAt || transitions[1].ActionSetMaxID != initial.ActionSetMaxID {
		return initial, ErrAdoptNotAwaiting
	}
	return initial, nil
}
