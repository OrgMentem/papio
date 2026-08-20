// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ApplyClaimObservationInput carries everything ApplyClaimObservation needs
// to reduce one claim_observation frame
// (dev/active/claim-observation-protocol.md §2.2.1, fenced by §3's
// idempotency/ordering rule). JobID is the envelope's own job (the
// requester) — distinct from the binding's owner job, which this function
// resolves itself from the live materialization claim. Generation is the
// daemon's current browser-holder epoch; FrameGeneration is the wire value
// the frame itself carried (validated against Generation, then folded away
// — every lease/evidence/journal write below uses Generation).
type ApplyClaimObservationInput struct {
	JobID                             string
	AuthenticationClaimID             string
	BindingID                         string
	ObservationID                     string
	GateOccurrenceID                  string
	EventKind                         string
	EventOrdinal                      int64
	FrameGeneration                   int64
	Generation                        int64
	AuthReturnedEvidenceObservationID string
	LeaseUntil                        time.Time
	Now                               time.Time
}

// ApplyClaimObservationResult is the durable outcome of one claim_observation
// apply: exactly the fields claim_observation_ack needs (dev/active/
// claim-observation-protocol.md §2.2's ack table).
type ApplyClaimObservationResult struct {
	Outcome          string
	Detail           string
	GateOccurrenceID string
	LeaseUntil       string
	// EntitledLanding is true only when this apply durably marked the lease
	// entitled (a fenced entitled_landing observation). The caller nudges
	// the materialization scheduler and the legacy sibling-reoffer path
	// only in that case, and only after this function's transaction has
	// committed — both of those are Bridge-level operations that open their
	// own store transactions and would deadlock the single-writer
	// connection if run while this one is still open.
	EntitledLanding bool
}

// ApplyClaimObservation is claim_observation's §2.2.1 reducer, run as ONE
// store transaction: the idempotency/ordering journal check, every
// lease/evidence/claim side effect the event authorizes, and the journal
// insert itself commit or roll back together. A side-effect failure after
// the journal check can therefore never strand a "genuinely new" journal
// entry that a legitimate retry would then see as already-applied and skip
// (the bug a two-phase check-then-write invites) — and, symmetrically, the
// journal dedup/ordering check itself runs before GetAuthenticationEntryLease,
// whose overdue-reservation path performs a mutating expiry UPDATE: a
// duplicate, stale, or rejected observation is a true no-op that never
// touches lease state.
func (js *Store) ApplyClaimObservation(ctx context.Context, in ApplyClaimObservationInput) (ApplyClaimObservationResult, error) {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return ApplyClaimObservationResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := applyClaimObservationTx(ctx, tx, in)
	if err != nil {
		return ApplyClaimObservationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ApplyClaimObservationResult{}, err
	}
	return result, nil
}

func applyClaimObservationTx(ctx context.Context, tx *sql.Tx, in ApplyClaimObservationInput) (ApplyClaimObservationResult, error) {
	result := ApplyClaimObservationResult{GateOccurrenceID: in.GateOccurrenceID}
	fail := func(outcome, detail string) (ApplyClaimObservationResult, error) {
		result.Outcome, result.Detail = outcome, detail
		return result, nil
	}

	// §2.2: the ack always echoes the daemon's CURRENT occurrence, which may
	// differ from the frame's when a sign-out/regrant rolled the gate over
	// since the extension last heard from the daemon — it is expected to
	// adopt this value for its next observation. Resolved first so every
	// early-return path below still carries it.
	currentOccurrenceID, occFound, err := currentAuthenticationClaimLoginOccurrenceTx(ctx, tx, in.AuthenticationClaimID)
	if err != nil {
		return fail("error", "gate occurrence state is unavailable")
	}
	if occFound {
		result.GateOccurrenceID = currentOccurrenceID
	}

	// §3 occurrence fencing: a frame naming a superseded occurrence targets a
	// login cycle the daemon has already closed out. Applying it under the
	// CURRENT occurrence's ordinal numbering (which is what the code below
	// would otherwise do, since it journals against result.GateOccurrenceID)
	// would let a queued or retried old-cycle event — most dangerously a
	// delayed auth_returned — renew or promote the current cycle's lease.
	// Answer stale instead: never applied, never journaled.
	if occFound && in.GateOccurrenceID != "" && in.GateOccurrenceID != currentOccurrenceID {
		return fail("stale", "gate occurrence has rolled over; adopt the current occurrence id")
	}

	if in.FrameGeneration != in.Generation {
		return fail("stale", "")
	}

	claim, err := materializationClaimByBindingIDTx(ctx, tx, in.BindingID)
	if err != nil {
		return fail("error", "binding state is unavailable")
	}
	ownerJobID := in.JobID
	var candidate *BrowserCandidate
	if claim != nil {
		candidate, err = getBrowserCandidateTx(ctx, tx, claim.CandidateID)
		if err != nil {
			return fail("error", "candidate state is unavailable")
		}
		if candidate != nil && candidate.JobID != "" {
			ownerJobID = candidate.JobID
		}
	}

	// §3: the dedup/ordering check runs before any state read that can
	// mutate — see this function's doc comment.
	journalOutcome, err := checkClaimObservationJournalTx(ctx, tx, in.ObservationID, result.GateOccurrenceID, in.EventOrdinal)
	if err != nil {
		return fail("error", "claim observation journal state is unavailable")
	}
	if journalOutcome != "" {
		result.Outcome = journalOutcome
		return result, nil
	}

	lease, leaseFound, err := getAuthenticationEntryLeaseTx(ctx, tx, in.AuthenticationClaimID)
	if err != nil {
		return fail("error", "authentication entry lease state is unavailable")
	}

	nowText := in.Now.UTC().Format(time.RFC3339Nano)
	// The lease's OWN BrowserHolderGeneration is deliberately not compared
	// against in.Generation on any path below. The sender's staleness is
	// already fenced above (in.FrameGeneration != in.Generation -> stale), so
	// by here the observing holder is proven current; the lease's field only
	// records the generation it was last reserved under, and every renewal
	// and promotion below carries in.Generation forward into it. Requiring
	// equality as a precondition forbade exactly that carry-forward: a
	// browser reconnect between arbitration and the human's sign-in bumps the
	// epoch, and every later observation for that login was then rejected for
	// good. §4.5's own rationale for human-paced renewal ("a login/MFA/
	// challenge prompt routinely outlives the arbitrary action-expiry window")
	// is precisely the case that outlives a reconnect. Measured live
	// 2026-08-19: claim_observation_journal held zero rows across weeks of
	// real sign-ins on the operator's own library, with nothing on either
	// side reporting why.
	switch in.EventKind {
	case "wall_observed", "login_started", "mfa", "challenge":
		if !leaseFound || lease.State != AuthenticationEntryLeaseReserved ||
			lease.OwnerID != ownerJobID {
			return fail("rejected", "no live reserved entry for this owner")
		}
		renewed, err := reserveAuthenticationEntryLeaseTx(ctx, tx, AuthenticationEntryLeaseInput{
			AuthenticationClaimID: in.AuthenticationClaimID, LeaseID: lease.LeaseID, OwnerID: ownerJobID,
			BrowserHolderGeneration: in.Generation, LeaseUntil: in.LeaseUntil,
		})
		if err != nil {
			if errors.Is(err, ErrAuthenticationEntryLeaseBusy) {
				return fail("rejected", "the entry is owned elsewhere")
			}
			return fail("error", "lease renewal is unavailable")
		}
		result.LeaseUntil = renewed.LeaseUntil
	case "auth_returned":
		if !leaseFound || lease.State != AuthenticationEntryLeaseReserved ||
			lease.OwnerID != ownerJobID {
			return fail("rejected", "no live reserved entry for this owner")
		}
		if candidate == nil {
			return fail("rejected", "binding has no live materialization claim")
		}
		candidateProfile, err := getInstitutionProfileTx(ctx, tx, candidate.InstitutionProfileID)
		if err != nil {
			return fail("error", "institution profile state is unavailable")
		}
		if candidateProfile == nil || candidateProfile.AuthenticationClaimID != in.AuthenticationClaimID {
			return fail("rejected", "binding does not belong to this authentication claim")
		}
		evidence := ProfileEvidenceObservation{
			ObservationID:              in.AuthReturnedEvidenceObservationID,
			BrowserHolderGeneration:    in.Generation,
			InstitutionProfileID:       candidate.InstitutionProfileID,
			InstitutionProfileRevision: candidate.InstitutionProfileRevision,
			Verdict:                    ProfileEvidenceAuthReturned,
			Source:                     ProfileEvidenceAuthReturn,
			ProducerObservedAt:         nowText,
			DaemonReceivedAt:           nowText,
		}
		if err := recordProfileEvidenceTx(ctx, tx, &evidence); err != nil {
			return fail("error", "profile evidence could not be recorded")
		}
		if err := convertAuthenticationEntryLeaseToHumanTx(ctx, tx, in.AuthenticationClaimID, lease.LeaseID, ownerJobID, in.Generation, evidence); err != nil &&
			!errors.Is(err, ErrAuthenticationEntryLeaseDenied) && !errors.Is(err, ErrAuthenticationEntryLeaseStale) {
			return fail("error", "authentication entry lease promotion is unavailable")
		}
	case "entitled_landing":
		if !leaseFound || lease.State != AuthenticationEntryLeaseHuman ||
			lease.HumanOwnerID != ownerJobID {
			return fail("rejected", "entry is not a settled human sign-in for this owner")
		}
		if err := markAuthenticationEntryLeaseEntitledTx(ctx, tx, in.AuthenticationClaimID, lease.LeaseID, ownerJobID, in.Generation, nowText); err != nil {
			return fail("error", "authentication entry lease entitlement could not be recorded")
		}
		result.EntitledLanding = true
	case "owner_closed":
		if err := abandonMaterializationClaimByBindingTx(ctx, tx, in.BindingID, nowText); err != nil {
			return fail("error", "materialization claim could not be abandoned")
		}
		if err := retireAuthenticationEntryLeaseAfterOwnerCloseTx(ctx, tx, in.AuthenticationClaimID, in.BindingID, in.Now); err != nil {
			return fail("error", "authentication entry lease owner binding could not be cleared")
		}
		if err := consumeCloseAuthorizationTx(ctx, tx, in.BindingID, nowText); err != nil {
			return fail("error", "close authorization could not be consumed")
		}
	case "navigation_error":
		// Daemon-committed park with no auth-attempt charge, no cooldown,
		// and the authentication-entry lease is never touched. The physical
		// route is exhausted, though: abandon/expire this materialization
		// claim so the extension's existing claim_abandoned close transaction
		// can retire the dead network-error surface. The candidate stays owned
		// - no automatic retry into a network which may still be offline. A
		// later explicit Open records the next-attempt decision.
		if err := abandonMaterializationClaimByBindingTx(ctx, tx, in.BindingID, nowText); err != nil {
			return fail("error", "materialization claim could not be abandoned")
		}

	}

	if err := recordClaimObservationTx(ctx, tx, ClaimObservationRecord{
		ObservationID: in.ObservationID, GateOccurrenceID: result.GateOccurrenceID,
		AuthenticationClaimID: in.AuthenticationClaimID, BindingID: in.BindingID,
		BrowserHolderGeneration: in.Generation, EventKind: in.EventKind, EventOrdinal: in.EventOrdinal,
	}); err != nil {
		return fail("error", "claim observation could not be recorded")
	}
	result.Outcome = "applied"
	return result, nil
}

// currentAuthenticationClaimLoginOccurrenceTx reads the id of the current
// human_gate.login occurrence scoped to one authentication claim, whatever
// its status (open or resolved) — claim_observation's ack always echoes
// "the daemon's current occurrence id" (§2.2), never the request's.
func currentAuthenticationClaimLoginOccurrenceTx(ctx context.Context, q dbtx, authenticationClaimID string) (string, bool, error) {
	var id string
	err := q.QueryRowContext(ctx, `
		SELECT id FROM human_gate_observations
		WHERE scope_class=? AND scope_key=? AND gate_type=?`,
		string(HumanGateScopeAuthenticationClaim), authenticationClaimID, string(HumanGateLogin)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}
