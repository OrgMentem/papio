// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package grab

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"papio/internal/job"
	"papio/internal/store"
)

const (
	// EligibilityPoolSnapshotSchema names the JSON payload version stored in
	// pdf_grab_eligibility_snapshots.snapshot.
	EligibilityPoolSnapshotSchema = "eligibility_pool_snapshot/1"

	// MaxEligibilitySnapshotBytes is the hard byte bound for one snapshot JSON
	// payload. Settlement fails closed when exceeded so measurement cannot
	// silently truncate the pool it is meant to record.
	MaxEligibilitySnapshotBytes = 512 * 1024

	// SnapshotPhasePreBind records the pool enumerated before park or before
	// the binding transaction opens.
	SnapshotPhasePreBind = "pre_bind"
	// SnapshotPhaseFencedCommit records the in-transaction pool that committed
	// with a fenced bind.
	SnapshotPhaseFencedCommit = "fenced_commit"

	autoBindOutcomeBound        = "bound"
	autoBindOutcomeAbstained    = "abstained"
	autoBindOutcomeNotAttempted = "not_attempted"
)

// ErrEligibilitySnapshotTooLarge reports that a snapshot JSON payload exceeds
// MaxEligibilitySnapshotBytes.
var ErrEligibilitySnapshotTooLarge = errors.New("eligibility pool snapshot exceeds size bound")

// EligibilityPoolSnapshot is the eligibility_pool_snapshot/1 envelope stored
// in pdf_grab_eligibility_snapshots.snapshot.
type EligibilityPoolSnapshot struct {
	Schema            string                       `json:"schema"`
	RecordedAt        string                       `json:"recorded_at"`
	Phase             string                       `json:"phase"`
	RuleEnabled       bool                         `json:"rule_enabled"`
	AutoBindAttempted bool                         `json:"auto_bind_attempted"`
	AutoBindOutcome   string                       `json:"auto_bind_outcome"`
	PoolSize          int                          `json:"pool_size"`
	Predicate         EligibilitySnapshotPredicate `json:"predicate"`
	Entries           []EligibilitySnapshotEntry   `json:"entries"`
}

// EligibilitySnapshotPredicate freezes the durable predicate literals used to
// enumerate the pool at write time.
type EligibilitySnapshotPredicate struct {
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	JobState string `json:"job_state"`
}

// EligibilitySnapshotEntry is one ordered pool member. Work carries only
// bibliographic fields needed to replay the selector — never zotio_item_key or
// URLs of any kind.
type EligibilitySnapshotEntry struct {
	JobID     string                  `json:"job_id"`
	Work      EligibilitySnapshotWork `json:"work"`
	BoundDOIs []string                `json:"bound_dois"`
}

// EligibilitySnapshotWork is the bibliographic snapshot for one pool entry.
// It deliberately omits zotio_item_key and any URL fields.
type EligibilitySnapshotWork struct {
	Title    string   `json:"title,omitempty"`
	Authors  []string `json:"authors,omitempty"`
	Year     int      `json:"year,omitempty"`
	DOI      string   `json:"doi,omitempty"`
	PMID     string   `json:"pmid,omitempty"`
	ArXiv    string   `json:"arxiv,omitempty"`
	ISBN     string   `json:"isbn,omitempty"`
	OpenAlex string   `json:"openalex,omitempty"`
}

// NewEligibilityPoolSnapshot builds the envelope from one enumeration result.
// candidates must already be in queryCandidateEligibleJobs order.
func NewEligibilityPoolSnapshot(candidates []job.CandidateEligibleJob, phase string, ruleEnabled, autoBindAttempted bool, autoBindOutcome string) EligibilityPoolSnapshot {
	entries := make([]EligibilitySnapshotEntry, 0, len(candidates))
	for _, c := range candidates {
		entries = append(entries, EligibilitySnapshotEntry{
			JobID: c.JobID,
			Work: EligibilitySnapshotWork{
				Title:    c.Work.Title,
				Authors:  append([]string(nil), c.Work.Authors...),
				Year:     c.Work.Year,
				DOI:      c.Work.DOI,
				PMID:     c.Work.PMID,
				ArXiv:    c.Work.ArXiv,
				ISBN:     c.Work.ISBN,
				OpenAlex: c.Work.OpenAlex,
			},
			BoundDOIs: append([]string(nil), c.BoundDOIs...),
		})
	}
	return EligibilityPoolSnapshot{
		Schema:            EligibilityPoolSnapshotSchema,
		RecordedAt:        store.Now(),
		Phase:             phase,
		RuleEnabled:       ruleEnabled,
		AutoBindAttempted: autoBindAttempted,
		AutoBindOutcome:   autoBindOutcome,
		PoolSize:          len(entries),
		Predicate: EligibilitySnapshotPredicate{
			Kind:     job.CandidateEligibleKind,
			Status:   job.CandidateEligibleStatus,
			JobState: job.StateAwaitingHuman,
		},
		Entries: entries,
	}
}

func marshalEligibilityPoolSnapshot(snap EligibilityPoolSnapshot) (string, error) {
	b, err := json.Marshal(snap)
	if err != nil {
		return "", err
	}
	if len(b) > MaxEligibilitySnapshotBytes {
		return "", fmt.Errorf("%w (%d bytes)", ErrEligibilitySnapshotTooLarge, len(b))
	}
	return string(b), nil
}

// RecordEligibilitySnapshotTx inserts one snapshot row in the caller's
// transaction. The grab terminal transition must share this transaction.
func RecordEligibilitySnapshotTx(ctx context.Context, tx *sql.Tx, grabID, phase string, snap EligibilityPoolSnapshot) error {
	if snap.Schema == "" {
		snap.Schema = EligibilityPoolSnapshotSchema
	}
	if snap.RecordedAt == "" {
		snap.RecordedAt = store.Now()
	}
	if snap.Phase == "" {
		snap.Phase = phase
	}
	raw, err := marshalEligibilityPoolSnapshot(snap)
	if err != nil {
		return err
	}
	recordedAt := snap.RecordedAt
	if recordedAt == "" {
		recordedAt = store.Now()
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO pdf_grab_eligibility_snapshots (grab_id, recorded_at, phase, snapshot)
		VALUES (?, ?, ?, ?)`,
		grabID, recordedAt, phase, raw)
	return err
}

// MarkParkedNoIdentifierWithEligibilitySnapshot settles parked_no_identifier
// and records the event-time eligibility pool in the same transaction.
func (s *Service) MarkParkedNoIdentifierWithEligibilitySnapshot(ctx context.Context, id string, snap EligibilityPoolSnapshot) error {
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := RecordEligibilitySnapshotTx(ctx, tx, id, SnapshotPhasePreBind, snap); err != nil {
		return err
	}
	now := store.Now()
	res, err := tx.ExecContext(ctx, `
		UPDATE pdf_grabs SET state = ?, job_id = NULL, outcome = 'needs_identifier', updated_at = ?
		WHERE id = ? AND state IN (?, ?, ?)`,
		string(StateParkedNoIdentifier), now, id,
		string(StateAwaitingFile), string(StateQuarantined), string(StateIdentified))
	if err != nil {
		return err
	}
	if err := requireOneRow(res, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE effect_permits SET status='settled', updated_at=? WHERE grab_id=? AND effect_kind='pdf_grab' AND status IN ('held','unknown_completion')`, now, id); err != nil {
		return err
	}
	if err := settleLegacyPDFBlockerTx(ctx, tx, id, now); err != nil {
		return err
	}
	return tx.Commit()
}

// EligibilitySnapshot returns the stored snapshot for one grab, if any.
func (s *Service) EligibilitySnapshot(ctx context.Context, grabID string) (EligibilityPoolSnapshot, error) {
	var phase, raw string
	err := s.store.DB().QueryRowContext(ctx, `
		SELECT phase, snapshot FROM pdf_grab_eligibility_snapshots WHERE grab_id = ?`,
		grabID).Scan(&phase, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return EligibilityPoolSnapshot{}, err
	}
	if err != nil {
		return EligibilityPoolSnapshot{}, err
	}
	var snap EligibilityPoolSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return EligibilityPoolSnapshot{}, err
	}
	if snap.Phase == "" {
		snap.Phase = phase
	}
	return snap, nil
}

// AutoBindOutcomeNotAttempted is the auto_bind_outcome when the rule was off.
func AutoBindOutcomeNotAttempted() string { return autoBindOutcomeNotAttempted }

// AutoBindOutcomeAbstained is the auto_bind_outcome when attemptAutoBind ran
// but did not bind.
func AutoBindOutcomeAbstained() string { return autoBindOutcomeAbstained }

// AutoBindOutcomeBound is the auto_bind_outcome for a committed bind.
func AutoBindOutcomeBound() string { return autoBindOutcomeBound }
