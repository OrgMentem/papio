// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package budget

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/store"
	"papio/internal/store/storetest"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	s, err := store.Open(context.Background(), storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return New(s)
}

func TestAcquireReservesMonthlyBudgetAtomically(t *testing.T) {
	m := testManager(t)
	p := config.Source{Enabled: true, MaxCostUSD: 1}
	if err := m.Acquire(context.Background(), "paid", p, 0.60); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(context.Background(), "paid", p, 0.41); err == nil {
		t.Fatal("request crossing monthly limit was accepted")
	} else {
		var exceeded *ErrExceeded
		if !errors.As(err, &exceeded) {
			t.Fatalf("error = %T %v, want ErrExceeded", err, err)
		}
	}
	snap, err := m.Snapshot(context.Background(), "paid", p)
	if err != nil {
		t.Fatal(err)
	}
	if snap.RequestsInWindow != 1 || snap.SpentUSD != 0.60 {
		t.Fatalf("snapshot = %+v, rejected reservation mutated counters", snap)
	}
}

func TestAcquireTokenBucketHonorsBurstAndCancellation(t *testing.T) {
	m := testManager(t)
	p := config.Source{Enabled: true, RatePerSec: 1, Burst: 2}
	ctx := context.Background()
	if err := m.Acquire(ctx, "limited", p, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(ctx, "limited", p, 0); err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if err := m.Acquire(blocked, "limited", p, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third immediate token = %v, want context deadline", err)
	}
	snap, _ := m.Snapshot(ctx, "limited", p)
	if snap.RequestsInWindow != 2 {
		t.Fatalf("cancelled wait reserved a request: %+v", snap)
	}
}

func TestDurableRetryAfterGateSurvivesManager(t *testing.T) {
	m := testManager(t)
	until := time.Now().UTC().Add(time.Hour)
	if err := m.Defer(context.Background(), "remote", config.Source{Enabled: true}, until); err != nil {
		t.Fatal(err)
	}
	// A new manager over the same DB must still observe the gate.
	m2 := &Manager{db: m.db, limiters: make(map[string]*tokenBucket), now: time.Now}
	err := m2.Acquire(context.Background(), "remote", config.Source{Enabled: true}, 0)
	var deferred *ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("durable gate returned %T %v, want *ErrDeferred", err, err)
	}
	if deferred.Source != "remote" || deferred.Until.Sub(until).Abs() > time.Second {
		t.Fatalf("deferred = %+v, want the persisted gate for remote at %s", deferred, until)
	}
}

// A provider expresses an exhausted daily quota as a Retry-After pointing at
// the next reset — up to a day out. Acquire must hand that back rather than
// sleep, because the caller holds an acquisition worker whose lease heartbeat
// would keep the job claimed for the whole window and stall the queue behind
// it. Pins the bug that froze a 309-job cohort on three claimed rows.
func TestAcquireReturnsRatherThanSleepingOnADailyQuotaGate(t *testing.T) {
	m := testManager(t)
	p := config.Source{Enabled: true, RatePerSec: 2, Burst: 2}
	midnight := time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	if err := m.Defer(context.Background(), "openalex", p, midnight); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := m.Acquire(context.Background(), "openalex", p, 0)
	var deferred *ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("Acquire = %T %v, want *ErrDeferred", err, err)
	}
	if elapsed := time.Since(start); elapsed > MaxInlineWait {
		t.Fatalf("Acquire blocked %v on a far gate; it must return within %v", elapsed, MaxInlineWait)
	}
	// The gate must not be consumed: it is durable state the next caller reads.
	snap, err := m.Snapshot(context.Background(), "openalex", p)
	if err != nil || snap.NextAllowedAt == nil {
		t.Fatalf("snapshot = %+v, %v; the deferral must survive a rejected Acquire", snap, err)
	}
	if snap.RequestsInWindow != 0 {
		t.Fatalf("requests_in_window = %d, want 0: a deferred call never reached the source", snap.RequestsInWindow)
	}
}

// The other half of the bound: a short Retry-After blip is still absorbed
// inline, so a two-second gate does not drop a resolver out of the chain.
func TestAcquireStillWaitsOutAShortGate(t *testing.T) {
	m := testManager(t)
	if err := m.Defer(context.Background(), "unpaywall", config.Source{Enabled: true}, time.Now().UTC().Add(20*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(context.Background(), "unpaywall", config.Source{Enabled: true}, 0); err != nil {
		t.Fatalf("Acquire on a short gate = %v, want it waited and succeeded", err)
	}
}

// reserve is the second half of Acquire's gate check, and the pre-loop
// Snapshot in Acquire is not the whole story: after that Snapshot passes,
// Acquire still calls takeToken, which can itself sleep for up to
// MaxInlineWait waiting on the token bucket to refill. A concurrent worker's
// Defer — the same call app.go's 429 handling makes — can land a fresh gate
// during exactly that sleep. reserve must re-check next_allowed_at itself
// rather than trust the caller's earlier snapshot, or the reservation
// commits and the request goes out against a gate every other caller
// believes is closed. Drives reserve directly (skipping Acquire's own
// pre-loop) to land the Defer deterministically between the "gate is clear"
// read and the reservation, without relying on goroutine timing.
func TestReserveRefusesAGateThatLandedDuringTheRaceWindow(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	p := config.Source{Enabled: true, MaxCostUSD: 10}
	identity := identityFor(p)

	// Stand in for Acquire's pre-loop Snapshot observing no gate.
	snap, err := m.Snapshot(ctx, "openalex", p)
	if err != nil {
		t.Fatal(err)
	}
	if snap.NextAllowedAt != nil {
		t.Fatalf("snapshot = %+v, want no gate before the simulated race", snap)
	}

	// A concurrent worker's Defer lands the gate in the window between that
	// snapshot and this worker's reserve — e.g. while it was asleep in
	// takeToken.
	gate := time.Now().UTC().Add(time.Hour)
	if err := m.Defer(ctx, "openalex", p, gate); err != nil {
		t.Fatal(err)
	}

	err = m.reserve(ctx, "openalex", identity, p.MaxCostUSD, 0.10)
	var deferred *ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("reserve = %T %v, want *ErrDeferred for the gate set during the race window", err, err)
	}
	if deferred.Until.Sub(gate).Abs() > time.Second {
		t.Fatalf("deferred until = %s, want the persisted gate at %s", deferred.Until, gate)
	}

	snap, err = m.Snapshot(ctx, "openalex", p)
	if err != nil {
		t.Fatal(err)
	}
	if snap.RequestsInWindow != 0 || snap.SpentUSD != 0 {
		t.Fatalf("snapshot = %+v, a refused reservation must not touch the counters", snap)
	}
}

// The fix must not degrade into "refuse whenever the row has ever carried a
// gate": an EXPIRED gate is still cleared inside reserve's own transaction
// and the reservation still proceeds, exactly as before the race fix.
func TestReserveStillClearsAnExpiredGate(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	p := config.Source{Enabled: true, MaxCostUSD: 10}
	identity := identityFor(p)

	if err := m.Defer(ctx, "openalex", p, time.Now().UTC().Add(10*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)

	if err := m.reserve(ctx, "openalex", identity, p.MaxCostUSD, 0.10); err != nil {
		t.Fatalf("reserve on an expired gate = %v, want it cleared and the reservation to proceed", err)
	}

	snap, err := m.Snapshot(ctx, "openalex", p)
	if err != nil {
		t.Fatal(err)
	}
	if snap.NextAllowedAt != nil {
		t.Fatalf("snapshot = %+v, an expired gate must still be cleared", snap)
	}
	if snap.RequestsInWindow != 1 || snap.SpentUSD != 0.10 {
		t.Fatalf("snapshot = %+v, want the reservation recorded once the gate expired", snap)
	}
}

// A provider with a clock bug or a malformed Retry-After could otherwise park
// every job needing that source for as long as it asked, recoverable only by
// editing the database. Every real quota resets within a day.
func TestDeferIsClampedToADayEvenIfTheServerAsksForLonger(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	if err := m.Defer(ctx, "confused", config.Source{}, time.Now().UTC().Add(365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot(ctx, "confused", config.Source{})
	if err != nil || snap.NextAllowedAt == nil {
		t.Fatalf("snapshot = %+v, %v", snap, err)
	}
	if until := time.Until(*snap.NextAllowedAt); until > MaxDeferHorizon+time.Minute {
		t.Fatalf("gate is %v out, want no more than %v", until, MaxDeferHorizon)
	}
	// A gate inside the horizon is still honoured exactly.
	want := time.Now().UTC().Add(2 * time.Hour)
	if err := m.Defer(ctx, "polite", config.Source{}, want); err != nil {
		t.Fatal(err)
	}
	snap, err = m.Snapshot(ctx, "polite", config.Source{})
	if err != nil || snap.NextAllowedAt == nil {
		t.Fatalf("snapshot = %+v, %v", snap, err)
	}
	if snap.NextAllowedAt.Sub(want).Abs() > time.Minute {
		t.Fatalf("gate = %s, want the server's %s untouched", snap.NextAllowedAt, want)
	}
}

func TestDeferNeverShortensExistingGate(t *testing.T) {
	m := testManager(t)
	later := time.Now().UTC().Add(2 * time.Hour)
	if err := m.Defer(context.Background(), "remote", config.Source{}, later); err != nil {
		t.Fatal(err)
	}
	if err := m.Defer(context.Background(), "remote", config.Source{}, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot(context.Background(), "remote", config.Source{})
	if err != nil || snap.NextAllowedAt == nil {
		t.Fatalf("snapshot = %+v, %v", snap, err)
	}
	if snap.NextAllowedAt.Before(later.Add(-time.Second)) {
		t.Fatalf("gate shortened: got %s, wanted at least %s", snap.NextAllowedAt, later)
	}
}

func TestDisabledAndInvalidRequestsFailBeforeMutation(t *testing.T) {
	m := testManager(t)
	if err := m.Acquire(context.Background(), "off", config.Source{}, 0); err == nil {
		t.Fatal("disabled source acquired")
	}
	if err := m.Acquire(context.Background(), "on", config.Source{Enabled: true}, -1); err == nil {
		t.Fatal("negative cost acquired")
	}
	if err := m.Acquire(context.Background(), "", config.Source{Enabled: true}, 0); err == nil {
		t.Fatal("empty source acquired")
	}
}

// The production fault this key shape fixes. OpenAlex 429'd under the
// anonymous identity with a Retry-After at the next UTC midnight, and papio
// wrote it to the single `openalex` row. Adding an API key opened a separate
// 10,000-credit budget, but the row still said closed: 95 jobs parked against
// a quota that had nothing to do with them, cleared only by a raw UPDATE on
// the live database.
func TestGateUnderOneIdentityDoesNotGateAnother(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	anon := config.Source{Enabled: true}
	keyed := config.Source{Enabled: true, APIKey: "k"}
	if err := m.Defer(ctx, "openalex", anon, time.Now().UTC().Add(18*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(ctx, "openalex", keyed, 0); err != nil {
		t.Fatalf("keyed Acquire = %v, want success: the gate was earned by the anonymous account", err)
	}
}

// The other half of the same contract: separating the identities must not
// disable the gate for the account that actually earned it.
func TestGatedIdentityIsStillGated(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	anon := config.Source{Enabled: true}
	if err := m.Defer(ctx, "openalex", anon, time.Now().UTC().Add(18*time.Hour)); err != nil {
		t.Fatal(err)
	}
	err := m.Acquire(ctx, "openalex", anon, 0)
	var deferred *ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("anonymous Acquire = %T %v, want *ErrDeferred", err, err)
	}
	if deferred.Identity != "anonymous" {
		t.Fatalf("deferred identity = %q, want anonymous: the error must name the gated account", deferred.Identity)
	}
}

// Counters are per-account too, not just gates: a provider meters each
// credential's allowance separately, so spending one must not draw down the
// other.
func TestRequestCountersAreIndependentPerIdentity(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	anon := config.Source{Enabled: true}
	keyed := config.Source{Enabled: true, APIKey: "k"}
	for range 2 {
		if err := m.Acquire(ctx, "openalex", anon, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Acquire(ctx, "openalex", keyed, 0); err != nil {
		t.Fatal(err)
	}
	anonSnap, err := m.Snapshot(ctx, "openalex", anon)
	if err != nil {
		t.Fatal(err)
	}
	keyedSnap, err := m.Snapshot(ctx, "openalex", keyed)
	if err != nil {
		t.Fatal(err)
	}
	if anonSnap.RequestsInWindow != 2 || keyedSnap.RequestsInWindow != 1 {
		t.Fatalf("anonymous = %d, keyed = %d; want 2 and 1 counted against separate accounts",
			anonSnap.RequestsInWindow, keyedSnap.RequestsInWindow)
	}
}

// Identity is the credential, not the whole policy: two clients configured
// with the same key share one budget even when their throttles differ. That is
// what makes the OpenAlex resolver and the OpenAlex discovery client — which
// read the same api_key through different policy paths — agree about one
// provider account rather than inventing two.
func TestSameKeyIsOneIdentityAcrossDifferingPolicies(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	first := config.Source{Enabled: true, APIKey: "shared", RatePerSec: 1}
	second := config.Source{Enabled: true, APIKey: "shared", RatePerSec: 9, Burst: 3}
	// Further out than MaxInlineWait, or Acquire waits instead of returning.
	if err := m.Defer(ctx, "openalex", first, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	err := m.Acquire(ctx, "openalex", second, 0)
	var deferred *ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("Acquire = %T %v, want *ErrDeferred: the same key is the same account", err, err)
	}
}

// The identity is written to the database and read back in diagnostics, so it
// has to be a fingerprint rather than the credential — Snapshot's contract is
// that it never contains one. Checked on the durable surface too: the column
// is what survives a crash, gets copied into bug reports, and outlives the
// process that wrote it.
func TestIdentityFingerprintNeverContainsTheCredential(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	const secret = "supersecret"
	policy := config.Source{Enabled: true, APIKey: secret}
	snap, err := m.Snapshot(ctx, "openalex", policy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snap.Identity, secret) {
		t.Fatalf("identity %q leaks the credential", snap.Identity)
	}
	if !strings.HasPrefix(snap.Identity, "key-") {
		t.Fatalf("identity = %q, want a key- fingerprint", snap.Identity)
	}
	if snap.Identity == identityFor(config.Source{}) {
		t.Fatalf("keyed identity collided with the anonymous one: %q", snap.Identity)
	}
	// Persist a row, then read the stored column rather than the computed one.
	if err := m.Acquire(ctx, "openalex", policy, 0); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := m.db.QueryRowContext(ctx,
		`SELECT identity FROM source_budgets WHERE source = 'openalex'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, secret) {
		t.Fatalf("stored identity %q leaks the credential into the database", stored)
	}
	if stored != snap.Identity {
		t.Fatalf("stored %q != reported %q; the durable key and the diagnostic must agree", stored, snap.Identity)
	}
}

// identityFor trims before deciding, and every provider client trims before
// deciding whether to send the credential. The two must agree: a whitespace-only
// key sends anonymous traffic, so metering it as a distinct keyed account would
// give that traffic its own budget and let it bypass the anonymous gate.
func TestWhitespaceOnlyKeyIsAnonymous(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	blank := config.Source{Enabled: true, APIKey: "   \t\n "}
	if got, want := identityFor(blank), identityFor(config.Source{Enabled: true}); got != want {
		t.Fatalf("whitespace-only key = %q, want %q: it sends no credential", got, want)
	}
	if err := m.Defer(ctx, "openalex", config.Source{Enabled: true}, time.Now().UTC().Add(18*time.Hour)); err != nil {
		t.Fatal(err)
	}
	err := m.Acquire(ctx, "openalex", blank, 0)
	var deferred *ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("Acquire with a whitespace-only key = %T %v, want the anonymous gate to apply", err, err)
	}
}

// A deferral from the token bucket must name the account too. The bucket is
// shared per source rather than per identity, but the error is still the
// caller's: leaving Identity unset here rendered "source openalex () is
// deferred until ..." and handed callers reading ErrDeferred.Identity an empty
// string even for keyed traffic.
func TestTokenBucketDeferralNamesTheIdentity(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	// One token, then a refill far slower than MaxInlineWait, so the second
	// call is turned away by the bucket rather than by a durable gate.
	keyed := config.Source{Enabled: true, APIKey: "k", RatePerSec: 0.001, Burst: 1}
	if err := m.Acquire(ctx, "openalex", keyed, 0); err != nil {
		t.Fatal(err)
	}
	err := m.Acquire(ctx, "openalex", keyed, 0)
	var deferred *ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("second Acquire = %T %v, want *ErrDeferred from the token bucket", err, err)
	}
	if want := identityFor(keyed); deferred.Identity != want {
		t.Fatalf("deferred identity = %q, want %q", deferred.Identity, want)
	}
	if strings.Contains(err.Error(), "()") {
		t.Fatalf("error renders an empty identity: %s", err)
	}
}
