// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/protocol"
)

// The store sweep is useless unless the daemon runs it, and neither of its two
// siblings has a wiring test — so an untested call site is exactly the kind a
// later cleanup deletes in good faith. One ordinary poll must free a stranded
// slot, because that poll is the only thing a researcher waiting on 42 parked
// papers can rely on.
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

	// The live shape: bound, human-paced, no deadline, and its claim abandoned
	// by a holder-generation fence rather than by any observation.
	if _, err := jobs.S.DB().ExecContext(ctx,
		`UPDATE authentication_entry_leases SET state='human', human_owner_id=?, lease_until=NULL
		  WHERE authentication_claim_id='auth-stranded-sweep'`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.AbandonStaleMaterializations(ctx, b.epoch+1); err != nil {
		t.Fatal(err)
	}
	stranded, ok, err := jobs.GetAuthenticationEntryLease(ctx, "auth-stranded-sweep")
	if err != nil || !ok || stranded.State != job.AuthenticationEntryLeaseHuman ||
		stranded.OwnerBindingID != bindingID {
		t.Fatalf("pre-sweep lease = %+v ok=%v err=%v; want the stranded shape", stranded, ok, err)
	}

	// One poll inside the grace changes nothing: the reconnect window is real.
	runSync(t, b)
	held, _, err := jobs.GetAuthenticationEntryLease(ctx, "auth-stranded-sweep")
	if err != nil || held.State != job.AuthenticationEntryLeaseHuman {
		t.Fatalf("a poll inside the grace freed the slot: %+v err=%v", held, err)
	}

	// One poll past the grace frees it, and the owner job is still parked —
	// which is the whole point, because the terminal-owner sweep cannot help.
	b.now = func() time.Time { return time.Now().UTC().Add(job.StrandedBoundEntryGrace + time.Minute) }
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
