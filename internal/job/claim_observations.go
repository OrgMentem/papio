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
	return checkClaimObservationJournalTx(ctx, js.S.DB(), observationID, gateOccurrenceID, eventOrdinal)
}

// checkClaimObservationJournalTx is CheckClaimObservationJournal's core.
// ApplyClaimObservation (claim_observation_apply.go) runs it first, inside
// the observation's own transaction and strictly before any lease read —
// GetAuthenticationEntryLease performs a mutating expiry UPDATE for an
// overdue reserved lease, and §3 requires a duplicate/stale/rejected
// observation to be a true no-op that never touches lease state.
func checkClaimObservationJournalTx(ctx context.Context, q dbtx, observationID, gateOccurrenceID string, eventOrdinal int64) (string, error) {
	if strings.TrimSpace(observationID) == "" || strings.TrimSpace(gateOccurrenceID) == "" {
		return "", errors.New("claim observation journal check requires observation and occurrence ids")
	}
	var existingOrdinal int64
	var existingOccurrence string
	err := q.QueryRowContext(ctx,
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
	if err := q.QueryRowContext(ctx,
		`SELECT MAX(event_ordinal) FROM claim_observation_journal WHERE gate_occurrence_id=?`,
		gateOccurrenceID).Scan(&maxOrdinal); err != nil {
		return "", err
	}
	if maxOrdinal.Valid && eventOrdinal <= maxOrdinal.Int64 {
		return "stale", nil
	}
	return "", nil
}

// claimObservationIsOrdered reports whether an event kind takes a position in
// its login cycle's ordinal sequence.
//
// Every kind does except `owner_closed`, which is not a step in the login
// narrative at all: it reports the PHYSICAL loss of one surface, keyed by a
// binding id that is minted per claim and globally unique. Its three effects
// (abandon the claim by binding, retire the entry lease whose
// `owner_binding_id` is exactly that binding, consume that binding's close
// authorization) are each fenced on the binding alone, so they can only ever
// retire the surface the report names — the surface that is, by the report's
// own content, already gone.
//
// Ordering it was wrong in both of the ways ordering can be wrong. The
// extension cannot know the current ordinal when this fires: the case that
// matters is a report recovered after an MV3 worker death, which is precisely
// when its counter is gone. And the occurrence fence rejected it outright
// whenever the human had signed out and back in since — a rollover makes the
// old tab MORE certainly gone, not less. Measured live 2026-08-22: 36
// rejections reading "gate occurrence has rolled over", each one leaving a
// materialization claim in a non-terminal phase for a tab that no longer
// existed, which is the stranded-surface family entry-lease-lifecycle.md
// documents.
func claimObservationIsOrdered(eventKind string) bool {
	return eventKind != "owner_closed"
}

// checkClaimObservationReplayTx is §3's idempotency rule WITHOUT its ordering
// rule, for the unordered kinds above. Idempotency still comes from the
// journal's own primary key: one observation id is applied exactly once. The
// ordered path's "rejected" outcome has no meaning here — an unordered
// observation's recorded ordinal is assigned by the daemon
// (nextClaimObservationOrdinalTx), not carried on the frame, so comparing a
// replay's ordinal against it would report a conflict for every honest retry.
func checkClaimObservationReplayTx(ctx context.Context, q dbtx, observationID string) (string, error) {
	if strings.TrimSpace(observationID) == "" {
		return "", errors.New("claim observation replay check requires an observation id")
	}
	var recorded string
	err := q.QueryRowContext(ctx,
		`SELECT observation_id FROM claim_observation_journal WHERE observation_id=?`,
		observationID).Scan(&recorded)
	switch {
	case err == nil:
		return "duplicate", nil
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	default:
		return "", err
	}
}

// nextClaimObservationOrdinalTx assigns an unordered observation its journal
// position: after everything already applied under this gate occurrence. The
// daemon assigns it because the daemon is the only party that knows it, and
// because the schema's UNIQUE (gate_occurrence_id, event_ordinal) index would
// otherwise reject a frame-supplied ordinal that a live cycle has already used.
func nextClaimObservationOrdinalTx(ctx context.Context, q dbtx, gateOccurrenceID string) (int64, error) {
	if strings.TrimSpace(gateOccurrenceID) == "" {
		return 0, errors.New("claim observation ordinal assignment requires an occurrence id")
	}
	var maxOrdinal sql.NullInt64
	if err := q.QueryRowContext(ctx,
		`SELECT MAX(event_ordinal) FROM claim_observation_journal WHERE gate_occurrence_id=?`,
		gateOccurrenceID).Scan(&maxOrdinal); err != nil {
		return 0, err
	}
	if !maxOrdinal.Valid {
		return 0, nil
	}
	return maxOrdinal.Int64 + 1, nil
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
	return recordClaimObservationTx(ctx, js.S.DB(), in)
}

func recordClaimObservationTx(ctx context.Context, q dbtx, in ClaimObservationRecord) error {
	if err := in.validate(); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx, `
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
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := abandonMaterializationClaimByBindingTx(ctx, tx, bindingID, store.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

func abandonMaterializationClaimByBindingTx(ctx context.Context, q dbtx, bindingID, now string) error {
	if strings.TrimSpace(bindingID) == "" {
		return errors.New("binding is required")
	}
	_, err := q.ExecContext(ctx, `
		UPDATE materialization_claims
		   SET phase='abandoned', lease_until=?, updated_at=?
		 WHERE binding_id=? AND phase IN ('claimed','bound','route_issued','navigated')`,
		now, now, bindingID)
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
	return consumeCloseAuthorizationTx(ctx, js.S.DB(), bindingID, now.UTC().Format(time.RFC3339Nano))
}

func consumeCloseAuthorizationTx(ctx context.Context, q dbtx, bindingID, now string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE close_authorizations
		   SET status='consumed', consumed_at=?
		 WHERE binding_id=? AND status='issued'`,
		now, bindingID)
	return err
}

// consumeCloseAuthorizationsTx marks every live ('issued') close-authorization
// token for the given bindings 'consumed', in the caller's own transaction.
// It exists so every daemon-side materialization-claim terminal transition —
// not only the owner_closed observation reducer — retires whatever token it
// mints: SettleMaterialization, ClaimMaterialization's own lease-timeout
// abandon, ReconcileMaterializationClaims, and AbandonStaleMaterializations
// (institutional_materialization.go) all call this right after they retire
// the claim(s) owning those bindings. Without it, a close authorized for a
// disposition the extension never explicitly acks (the common case: the
// daemon itself drove the claim to settled/abandoned, so surface_close_request
// for that disposition may never arrive) is left 'issued' forever, and
// IssueCloseAuthorization's idempotent replay keeps re-minting the same
// already-moot token. Empty binding ids are skipped rather than rejected —
// callers pass whatever a claim's binding_id column held, which is legally
// empty for pre-bind claims.
func consumeCloseAuthorizationsTx(ctx context.Context, q dbtx, bindingIDs []string, now string) error {
	for _, id := range bindingIDs {
		if strings.TrimSpace(id) == "" {
			continue
		}
		if err := consumeCloseAuthorizationTx(ctx, q, id, now); err != nil {
			return err
		}
	}
	return nil
}
