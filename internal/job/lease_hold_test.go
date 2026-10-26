// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"testing"
	"time"

	"papio/internal/store"
)

// TestClaimNextSkipsLocallyHeldJobAfterWallClockJump pins papio-e1e6e896: a
// wall-clock jump (laptop sleep, NTP step) that expires a live holder's
// durable lease text must not hand the job to another local worker. The
// holder's heartbeat keeps working; after it releases, the job is claimable
// again.
func TestClaimNextSkipsLocallyHeldJobAfterWallClockJump(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, err := js.CreateRequest(ctx, "wr_walljump_01", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	held, err := js.ClaimNext(ctx, "worker-a", time.Minute)
	if err != nil || held == nil || held.ID != id {
		t.Fatalf("ClaimNext = %#v, %v; want the job for worker-a", held, err)
	}
	// Simulate the wall clock jumping past the lease while worker A is still
	// alive: the durable text reads expired although the holder never left.
	past := store.FormatTime(time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC))
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE jobs SET lease_expires_at = ? WHERE id = ?`, past, id); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if other, err := js.ClaimNext(ctx, "worker-b", time.Minute); err != nil || other != nil {
		t.Fatalf("ClaimNext for worker-b = %#v, %v; want nil (job still held by worker-a)", other, err)
	}
	if err := js.Heartbeat(ctx, id, "worker-a", time.Minute); err != nil {
		t.Fatalf("Heartbeat for worker-a: %v", err)
	}
	if err := js.Release(ctx, id, "worker-a"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if other, err := js.ClaimNext(ctx, "worker-b", time.Minute); err != nil || other == nil || other.ID != id {
		t.Fatalf("ClaimNext after release = %#v, %v; want the job for worker-b", other, err)
	}
}

// TestReleasingTransitionKeepsANewerClaimantsHold pins the owner-specific
// unhold: a lease-clearing transition by the old holder must not delete a
// hold that another claimant registered between that transition's commit and
// its unhold. Otherwise a wall-clock jump hands the new holder's job to a
// third worker.
func TestReleasingTransitionKeepsANewerClaimantsHold(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, err := js.CreateRequest(ctx, "wr_unhold_owner_01", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if row, err := js.ClaimNext(ctx, "worker-a", time.Minute); err != nil || row == nil || row.ID != id {
		t.Fatalf("ClaimNext for worker-a = %#v, %v", row, err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatalf("to resolving: %v", err)
	}
	// Worker B registers its hold in the window after A's releasing commit
	// and before A's unhold runs.
	js.heldMu.Lock()
	js.held[id] = "worker-b"
	js.heldMu.Unlock()
	if err := js.Transition(ctx, id, StateResolving, StateRetryWait, nil, WithRetryAt(time.Now().Add(-time.Second))); err != nil {
		t.Fatalf("to retry_wait: %v", err)
	}
	// B's durable claim, then a wall-clock jump past its lease text.
	past := store.FormatTime(time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC))
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE jobs SET lease_owner = 'worker-b', lease_expires_at = ? WHERE id = ?`, past, id); err != nil {
		t.Fatalf("simulate worker-b lease: %v", err)
	}
	if other, err := js.ClaimNext(ctx, "worker-c", time.Minute); err != nil || other != nil {
		t.Fatalf("ClaimNext for worker-c = %#v, %v; want nil (job held by worker-b)", other, err)
	}
}
