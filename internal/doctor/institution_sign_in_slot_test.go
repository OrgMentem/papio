// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doctor

import (
	"context"
	"strings"
	"testing"

	"papio/internal/store"
	"papio/internal/store/storetest"
)

func signInSlotStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedLease writes one authentication entry lease directly. The doctor check
// reads the durable row, so the row is the whole input.
func seedLease(t *testing.T, db *store.Store, state, leaseUntil, entitledAt, binding string) {
	t.Helper()
	var until, entitled, bindingID any
	if leaseUntil != "" {
		until = leaseUntil
	}
	if entitledAt != "" {
		entitled = entitledAt
	}
	if binding != "" {
		bindingID = binding
	}
	if _, err := db.DB().ExecContext(context.Background(), `
		INSERT INTO authentication_entry_leases(
			authentication_claim_id, lease_id, owner_id, browser_holder_generation,
			state, lease_until, human_owner_id, entitled_at, owner_binding_id,
			created_at, updated_at)
		VALUES('auth-claim-slot','lease-slot','job_holder_paper',7,?,?,'job_holder_paper',?,?,
		       '2026-08-23T00:00:00Z','2026-08-23T00:00:00Z')`,
		state, until, entitled, bindingID); err != nil {
		t.Fatal(err)
	}
}

func seedWaitingHandoff(t *testing.T, db *store.Store, jobID string) {
	t.Helper()
	if _, err := db.DB().ExecContext(context.Background(), `
		INSERT INTO human_actions(job_id, kind, status, detail, created_at)
		VALUES(?,'openurl_handoff','open','handoff available','2026-08-23T00:00:00Z')`,
		jobID); err != nil {
		t.Fatal(err)
	}
}

// A bound lease in the human window has no deadline by design, so nothing
// else in papio will ever surface it. Measured live 2026-08-23: siblings were
// refused "another sign-in for this institution is in progress" while the
// operator had no way to learn which paper held the slot.
func TestSignInSlotHeldWithNoDeadlineIsNamedWithWaitingPapers(t *testing.T) {
	db := signInSlotStore(t)
	seedLease(t, db, "human", "", "", "binding-live")
	seedWaitingHandoff(t, db, "job_waiting_one")
	seedWaitingHandoff(t, db, "job_waiting_two")

	checks := collectChecks(t, func(add func(string, string, string, string)) {
		checkInstitutionSignInSlot(context.Background(), db, add)
	})
	if len(checks) != 1 {
		t.Fatalf("checks = %+v, want exactly one held-slot warning", checks)
	}
	if checks[0].Name != "institution_sign_in_slot" || checks[0].Status != Warn {
		t.Fatalf("check = %+v, want a warn named institution_sign_in_slot", checks[0])
	}
	if !strings.Contains(checks[0].Detail, "job_holder_paper") {
		t.Fatalf("detail must name the holder: %q", checks[0].Detail)
	}
	if !strings.Contains(checks[0].Detail, "2 other papers are waiting") {
		t.Fatalf("detail must count the papers waiting behind it: %q", checks[0].Detail)
	}
	if !strings.Contains(checks[0].Remediation, "papio actions open --job job_holder_paper") {
		t.Fatalf("remediation must name the command that finishes the sign-in: %q", checks[0].Remediation)
	}
}

// The three shapes that are NOT a stalled slot. An entitled lease is shared,
// so siblings are admitted rather than refused; a lease carrying a deadline
// expires on its own; an unbound lease is swept by
// ExpireUnboundAuthenticationEntryLeases. Warning about any of them would
// train the operator to ignore this line.
func TestSignInSlotStaysQuietForSharedBoundedAndUnboundLeases(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		state, until, entitled, binding string
	}{
		{"entitled is shared", "human", "", "2026-08-23T01:00:00Z", "binding-live"},
		{"deadline expires on its own", "human", "2026-08-23T01:00:00Z", "", "binding-live"},
		{"unbound is swept", "human", "", "", ""},
		{"reserved is not the human window", "reserved", "", "", "binding-live"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := signInSlotStore(t)
			seedLease(t, db, tc.state, tc.until, tc.entitled, tc.binding)
			seedWaitingHandoff(t, db, "job_waiting_one")
			checks := collectChecks(t, func(add func(string, string, string, string)) {
				checkInstitutionSignInSlot(context.Background(), db, add)
			})
			if len(checks) != 0 {
				t.Fatalf("checks = %+v, want silence", checks)
			}
		})
	}
}
