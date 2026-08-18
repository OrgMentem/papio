// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"papio/internal/store"
)

// claimObservationEventKinds is the closed vocabulary
// dev/active/claim-observation-protocol.md §2.2 defines for
// claim_observation.event_kind, mirrored here so the journal INSERT's
// caller-side validation matches the CHECK constraint in migration 0042
// exactly rather than trusting the database to be the only fence.
var claimObservationEventKinds = map[string]bool{
	"wall_observed": true, "login_started": true, "mfa": true, "challenge": true,
	"auth_returned": true, "entitled_landing": true, "owner_closed": true, "navigation_error": true,
}

// ClaimObservationRecord is one durable, idempotency-fenced application of a
// claim_observation event to claim_observation_journal (migration 0042).
type ClaimObservationRecord struct {
	ObservationID           string
	GateOccurrenceID        string
	AuthenticationClaimID   string
	BindingID               string
	BrowserHolderGeneration int64
	EventKind               string
	EventOrdinal            int64
}

func (r ClaimObservationRecord) validate() error {
	if strings.TrimSpace(r.ObservationID) == "" || len(r.ObservationID) > 128 ||
		strings.TrimSpace(r.GateOccurrenceID) == "" ||
		strings.TrimSpace(r.AuthenticationClaimID) == "" || len(r.AuthenticationClaimID) > 256 ||
		strings.TrimSpace(r.BindingID) == "" || len(r.BindingID) > 256 ||
		r.BrowserHolderGeneration < 0 || r.EventOrdinal < 0 ||
		!claimObservationEventKinds[r.EventKind] {
		return errors.New("invalid claim observation record")
	}
	return nil
}

// CheckClaimObservationJournal is the read-only half of §3's idempotency and
// ordering rule, run before any lease/evidence/scheduler side effect. It
// reports "" for a genuinely new (observation_id, event_ordinal) pair,
// "duplicate" for an exact replay (matching event_ordinal and
// gate_occurrence_id already recorded under this observation_id),
// "rejected" for a supposedly-identical observation_id whose recorded
// ordinal/occurrence disagrees with the replayed frame, and "stale" for a
// new observation_id whose event_ordinal does not exceed the highest
// applied ordinal for this gate_occurrence_id.
func (js *Store) CheckClaimObservationJournal(ctx context.Context, observationID, gateOccurrenceID string, eventOrdinal int64) (string, error) {
	if strings.TrimSpace(observationID) == "" || strings.TrimSpace(gateOccurrenceID) == "" {
		return "", errors.New("claim observation journal check requires observation and occurrence ids")
	}
	var existingOrdinal int64
	var existingOccurrence string
	err := js.S.DB().QueryRowContext(ctx,
		`SELECT event_ordinal, gate_occurrence_id FROM claim_observation_journal WHERE observation_id=?`,
		observationID).Scan(&existingOrdinal, &existingOccurrence)
	switch {
	case err == nil:
		if existingOrdinal == eventOrdinal && existingOccurrence == gateOccurrenceID {
			return "duplicate", nil
		}
		return "rejected", nil
	case errors.Is(err, sql.ErrNoRows):
	default:
		return "", err
	}
	var maxOrdinal sql.NullInt64
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT MAX(event_ordinal) FROM claim_observation_journal WHERE gate_occurrence_id=?`,
		gateOccurrenceID).Scan(&maxOrdinal); err != nil {
		return "", err
	}
	if maxOrdinal.Valid && eventOrdinal <= maxOrdinal.Int64 {
		return "stale", nil
	}
	return "", nil
}

// RecordClaimObservation durably appends one applied observation to the
// journal. Callers MUST call this only after CheckClaimObservationJournal
// reported "" (genuinely new) and after the corresponding side effect
// succeeded — recording first would let a side-effect failure strand a
// journal entry that a legitimate retry can then never re-apply (the retry
// would see "duplicate" and skip it). The schema's unique
// (gate_occurrence_id, event_ordinal) index is the second line of defense
// against a concurrent double-apply this single-writer daemon should never
// reach in practice.
func (js *Store) RecordClaimObservation(ctx context.Context, in ClaimObservationRecord) error {
	if err := in.validate(); err != nil {
		return err
	}
	_, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO claim_observation_journal
		  (observation_id, gate_occurrence_id, authentication_claim_id, binding_id,
		   browser_holder_generation, event_kind, event_ordinal, applied_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		in.ObservationID, in.GateOccurrenceID, in.AuthenticationClaimID, in.BindingID,
		in.BrowserHolderGeneration, in.EventKind, in.EventOrdinal, store.Now())
	return err
}

// EligibleAuthenticationClaimDependents is the §4.4 live dependent count:
// eligible siblings sharing this authentication claim through their
// institution profile, derived fresh rather than maintained as a counter.
func (js *Store) EligibleAuthenticationClaimDependents(ctx context.Context, authenticationClaimID string) (int64, error) {
	if strings.TrimSpace(authenticationClaimID) == "" {
		return 0, errors.New("authentication claim is required")
	}
	var n int64
	err := js.S.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM browser_candidates bc
		JOIN institution_profiles p
		  ON p.id = bc.institution_profile_id AND p.revision = bc.institution_profile_revision
		WHERE p.authentication_claim_id = ? AND bc.status = 'eligible'`,
		authenticationClaimID).Scan(&n)
	return n, err
}

// AbandonMaterializationClaimByBinding marks the live materialization claim
// for one binding "abandoned" (owner_closed, §2.2.1: "the owning surface
// closed without success"). A no-op when no live claim exists for the
// binding — an idle scaffold that never advanced past nothing, or a claim
// already resolved, is not this call's business.
func (js *Store) AbandonMaterializationClaimByBinding(ctx context.Context, bindingID string) error {
	if strings.TrimSpace(bindingID) == "" {
		return errors.New("binding is required")
	}
	_, err := js.S.DB().ExecContext(ctx, `
		UPDATE materialization_claims SET phase='abandoned', updated_at=?
		 WHERE binding_id=? AND phase IN ('claimed','bound','route_issued','navigated')`,
		store.Now(), bindingID)
	return err
}

// ConsumeCloseAuthorizationForBinding marks any live ('issued') close
// authorization for a binding "consumed" (§4.3: "that consumed-marking
// write itself arrives with Slice 3's owner_closed reducer"). A no-op when
// no token was ever issued for the binding.
func (js *Store) ConsumeCloseAuthorizationForBinding(ctx context.Context, bindingID string, now time.Time) error {
	if strings.TrimSpace(bindingID) == "" {
		return errors.New("binding is required")
	}
	_, err := js.S.DB().ExecContext(ctx, `
		UPDATE close_authorizations
		   SET status='consumed', consumed_at=?
		 WHERE binding_id=? AND status='issued'`,
		now.UTC().Format(time.RFC3339Nano), bindingID)
	return err
}
