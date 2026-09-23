// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/protocol"
)

// An ordinary poll still runs the backstop for a slot left bound to a claim
// abandoned outside the generation fence. A `human` entry whose claim is
// terminal is released on the next poll, not after the 30-minute grace
// (measured live 2026-09-23: six opened papers queued behind one such slot).
func TestSyncFreesAStrandedBoundEntryLease(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "wr_stranded_sweep", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-stranded-sweep")
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "domain-stranded-sweep")

	granted, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, jobID,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-stranded-sweep-req", CandidateID: candidateID,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	if authClaimResponse(t, granted) == nil {
		t.Fatal("no authentication claim grant")
	}
	bindingID := bindCandidate(t, b, jobID, candidateID, "auth-stranded-sweep", 21)

	// Recreate a legacy stranded slot: bound, human-paced, no deadline.
	if _, err := jobs.S.DB().ExecContext(ctx,
		`UPDATE authentication_entry_leases SET state='human', human_owner_id=?, lease_until=NULL
		  WHERE authentication_claim_id='auth-stranded-sweep'`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx,
		`UPDATE materialization_claims SET phase='abandoned', updated_at=? WHERE binding_id=?`,
		time.Now().UTC().Format(time.RFC3339Nano), bindingID); err != nil {
		t.Fatal(err)
	}
	stranded, ok, err := jobs.GetAuthenticationEntryLease(ctx, "auth-stranded-sweep")
	if err != nil || !ok || stranded.State != job.AuthenticationEntryLeaseHuman ||
		stranded.OwnerBindingID != bindingID {
		t.Fatalf("legacy stranded lease = %+v ok=%v err=%v; want the bound human shape", stranded, ok, err)
	}

	// The next ordinary poll frees it, and the owner job is still parked —
	// which is the whole point, because the terminal-owner sweep cannot help.
	runSync(t, b)
	freed, ok, err := jobs.GetAuthenticationEntryLease(ctx, "auth-stranded-sweep")
	if err != nil || !ok {
		t.Fatalf("post-sweep lease read: %+v ok=%v err=%v", freed, ok, err)
	}
	if freed.State == job.AuthenticationEntryLeaseHuman || freed.OwnerBindingID != "" {
		t.Fatalf("ordinary polling left the institution held by a dead surface: %+v", freed)
	}
	row, rowErr := jobs.Get(ctx, jobID)
	if rowErr != nil || job.Terminal(row.State) {
		t.Fatalf("owner job = %+v err=%v; the test is vacuous unless it is still parked", row, rowErr)
	}
}
