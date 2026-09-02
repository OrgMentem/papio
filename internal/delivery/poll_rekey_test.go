// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package delivery

import (
	"context"
	"testing"
)

// A 404 reconciliation rekeys provider_reference without touching state or
// next_check_at, so a CAS over those two fields alone still matches after
// the rekey. A worker that already fetched the OLD transaction would then
// commit its status, terminal state and settlement event onto a row that
// now names a DIFFERENT transaction — the row and its own event disagreeing
// about which provider request was observed. provider_reference is part of
// the poll snapshot for exactly this interleaving.
func TestStalePollLosesAfterProviderReferenceRekey(t *testing.T) {
	svc, clock := testServiceClock(t)
	ctx := context.Background()
	live := newLiveRequest(t, svc, "rekey1", *clock)

	// The stale worker's snapshot, read while the row still named 555.
	stale, err := svc.Get(ctx, live.ID)
	if err != nil || stale == nil {
		t.Fatalf("get: %v, %v", stale, err)
	}

	// The reconciling worker rekeys the row to the replacement transaction.
	// This is handleNotFound's write, and it changes neither state nor
	// next_check_at.
	applied, err := svc.persistProviderReference(ctx, live, "999")
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("rekey did not apply, want the reconciling worker to win")
	}

	// The stale worker's own fetch of 555 came back terminal.
	client := sequencedIlliadServer(t, nil, txnResponse(illiadStatusDeliveredToWeb, 555))
	result, err := svc.Poll(ctx, stale, PollDeps{Client: client, StatusPollMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	if result.Settled {
		t.Fatalf("stale poll = %+v, want the CAS lost: its status belongs to transaction 555, the row now names 999", result)
	}

	got, err := svc.Get(ctx, live.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProviderReference != "999" {
		t.Fatalf("provider_reference = %q, want the rekey preserved", got.ProviderReference)
	}
	if got.State == StateFulfilled {
		t.Fatal("row settled fulfilled from a status observed for a transaction it no longer names")
	}
	if got.ProviderStatusRaw == illiadStatusDeliveredToWeb {
		t.Fatalf("provider_status_raw = %q, want transaction 555's status not recorded against 999", got.ProviderStatusRaw)
	}

	var n int
	if err := svc.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id = ? AND kind = ?`,
		"rekey1", eventKindFulfilled).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("delivery.fulfilled events = %d, want none: no settlement actually applied", n)
	}
}
