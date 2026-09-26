// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package captures

import (
	"context"
	"os"
	"testing"
	"time"
)

// A pin without an index entry predates index-first ordering: PendingJobs
// cannot see it while retention sweeps must keep it. The orphan reconciler
// must collect exactly those pins and leave indexed leases alone.
func TestReleaseOrphanPendingPinsCollectsOnlyIndexlessPins(t *testing.T) {
	ctx := context.Background()
	store := New(t.TempDir(), Retention{MaxPerHost: 1, MaxAge: 24 * time.Hour})

	orphanA, err := store.StoreSanitizedPinned(ctx, "job-orphan-a", "provider.example", "drift", "provider", "1", pendingLeaseFixture("provider.example", "drift", "orphan-a"))
	if err != nil {
		t.Fatal(err)
	}
	orphanB, err := store.StoreSanitizedPinned(ctx, "job-orphan-b", "provider.example", "drift", "provider", "1", pendingLeaseFixture("provider.example", "drift", "orphan-b"))
	if err != nil {
		t.Fatal(err)
	}
	live, err := store.StoreSanitizedPinned(ctx, "job-live", "provider.example", "drift", "provider", "1", pendingLeaseFixture("provider.example", "drift", "live"))
	if err != nil {
		t.Fatal(err)
	}

	// Pinned captures are exempt from count eviction, so all three survive
	// despite MaxPerHost 1. That is the unbounded growth the orphan path had.
	listed, err := store.List(ctx)
	if err != nil || len(listed) != 3 {
		t.Fatalf("List() = %d, %v; want 3 pinned captures", len(listed), err)
	}

	// Simulate the pre-fix crash window (pin written, index never recorded)
	// for two jobs by dropping only their index entries. There is no API to
	// remove one entry, so rewrite the index with just the live job. The
	// fingerprint key is opaque by design; read it back from the live pin.
	livePin, ok := readPin(live)
	if !ok {
		t.Fatal("live capture has no pin")
	}
	if err := os.WriteFile(pendingIndexPath(store.root), []byte(`{"`+livePin.Fingerprint+`":"job-live"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0] != "job-live" {
		t.Fatalf("pending jobs = %v, want [job-live]", pending)
	}

	removed, err := store.ReleaseOrphanPendingPins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2 orphan pins", removed)
	}
	for _, path := range []string{orphanA, orphanB} {
		if pin, ok := readPin(path); ok {
			t.Fatalf("orphan pin survives on %s: %#v", path, pin)
		}
	}
	if pin, ok := readPin(live); !ok || pin.Fingerprint != livePin.Fingerprint {
		t.Fatalf("live lease pin = %#v, %v; want it preserved", pin, ok)
	}

	// The released captures are ordinary again, so the sweep must enforce the
	// per-host bound instead of keeping all three forever.
	if err := store.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	listed, err = store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("List() after sweep = %d, want 1", len(listed))
	}
}
