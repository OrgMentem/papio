// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package budget

import (
	"context"
	"errors"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/store"
)

// keyedAndAnon is the OpenAlex identity pair AcquireAny arbitrates between: the
// configured key, and the same policy with the key removed, which reaches
// OpenAlex's separately-budgeted keyless tier.
func keyedAndAnon() (config.Source, config.Source) {
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	anon := keyed
	anon.APIKey = ""
	return keyed, anon
}

func TestAcquireAnyPrefersKeyed(t *testing.T) {
	m := testManager(t)
	keyed, anon := keyedAndAnon()
	chosen, err := m.AcquireAny(context.Background(), "openalex", []config.Source{keyed, anon}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if chosen.APIKey != keyed.APIKey {
		t.Fatalf("chosen = %+v, want the keyed identity while nothing is gated", chosen)
	}
}

func TestAcquireAnyFallsBackOnQuotaGate(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	keyed, anon := keyedAndAnon()
	if err := m.Defer(ctx, "openalex_quota", keyed, time.Now().UTC().Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	chosen, err := m.AcquireAny(ctx, "openalex", []config.Source{keyed, anon}, 0)
	if err != nil {
		t.Fatalf("fallback returned %v, want the anonymous identity admitted", err)
	}
	if chosen.APIKey != "" {
		t.Fatalf("chosen = %+v, want the anonymous identity", chosen)
	}
	// The keyed identity must not have been given a real admission attempt: a
	// request reserved against it would spend quota to prove what its own
	// header already said. The token bucket is source-wide, so only the durable
	// per-identity reservation can witness this.
	keyedSnap, err := m.Snapshot(ctx, "openalex", keyed)
	if err != nil {
		t.Fatal(err)
	}
	if keyedSnap.RequestsInWindow != 0 {
		t.Fatalf("keyed reservations = %d, want 0 on a quota-gated identity", keyedSnap.RequestsInWindow)
	}
	anonSnap, err := m.Snapshot(ctx, "openalex", anon)
	if err != nil {
		t.Fatal(err)
	}
	if anonSnap.RequestsInWindow != 1 {
		t.Fatalf("anonymous reservations = %d, want the admitted call metered here", anonSnap.RequestsInWindow)
	}
}

func TestAcquireAnyOrdinaryGateNoFallback(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	keyed, anon := keyedAndAnon()
	// An ordinary retry/backoff gate under the bare source name says nothing
	// about the keyed identity's daily quota, so it must never authorize a
	// credential switch.
	if err := m.Defer(ctx, "openalex", keyed, time.Now().UTC().Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	chosen, err := m.AcquireAny(ctx, "openalex", []config.Source{keyed, anon}, 0)
	var deferred *ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("err = %v, want the keyed identity's own ErrDeferred", err)
	}
	if chosen.APIKey != keyed.APIKey {
		t.Fatalf("chosen = %+v, want the keyed identity refused rather than replaced", chosen)
	}
	if deferred.Advisory {
		t.Fatalf("deferral = %+v, want the durable gate", deferred)
	}
	anonSnap, err := m.Snapshot(ctx, "openalex", anon)
	if err != nil {
		t.Fatal(err)
	}
	if anonSnap.RequestsInWindow != 0 {
		t.Fatalf("anonymous reservations = %d, want 0: an ordinary gate is not a quota signal", anonSnap.RequestsInWindow)
	}
}

func TestAcquireAnyAdvisoryNoFallback(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	keyed := config.Source{Enabled: true, APIKey: "private-key", RatePerSec: 1, Burst: 1}
	anon := keyed
	anon.APIKey = ""
	// Spend the source-wide bucket, then confirm the refusal is reported as the
	// process-local advisory backoff it is, without switching credentials.
	if _, err := m.AcquireAny(ctx, "openalex", []config.Source{keyed, anon}, 0); err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	chosen, err := m.AcquireAny(blocked, "openalex", []config.Source{keyed, anon}, 0)
	if err == nil {
		t.Fatal("exhausted token bucket admitted a second request")
	}
	if chosen.APIKey != keyed.APIKey {
		t.Fatalf("chosen = %+v, want the keyed identity: its own throttle is not a quota signal", chosen)
	}
}

func TestAcquireAnyFailsClosedOnSnapshotError(t *testing.T) {
	s, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := New(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	keyed, anon := keyedAndAnon()
	chosen, err := m.AcquireAny(context.Background(), "openalex", []config.Source{keyed, anon}, 0)
	if err == nil {
		t.Fatal("an unreadable quota signal admitted a request")
	}
	var deferred *ErrDeferred
	if errors.As(err, &deferred) {
		t.Fatalf("err = %v, want the read failure itself, not a deferral", err)
	}
	if chosen.APIKey != "" || chosen.Enabled {
		t.Fatalf("chosen = %+v, want no identity on a failed read", chosen)
	}
}

// Every provider client in the tree admits through one of these two entry
// points, and the floor must bind on both. sourcegate.Client — which is what
// discovery, DOI-only enrichment, watch digests and MCP use — admits with the
// single-policy Acquire, so a floor honoured only by AcquireAny left exactly
// the independent callers sourcegate exists to account for spending freely.
func TestAcquireHonoursTheProviderQuotaFloor(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	keyed, anon := keyedAndAnon()
	reset := time.Now().UTC().Add(6 * time.Hour).Truncate(time.Second)
	if err := m.Defer(ctx, "openalex_quota", keyed, reset); err != nil {
		t.Fatal(err)
	}

	err := m.Acquire(ctx, "openalex", keyed, 0)
	var deferred *ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("err = %v, want *ErrDeferred: a fixed-policy caller has no other identity to try", err)
	}
	if !deferred.Until.Equal(reset) {
		t.Fatalf("Until = %s, want the provider's own reset %s", deferred.Until, reset)
	}

	// Cross-identity isolation: the keyless pool is metered separately, so its
	// own admission is untouched by the keyed floor.
	if err := m.Acquire(ctx, "openalex", anon, 0); err != nil {
		t.Fatalf("anonymous Acquire = %v, want admission: that pool has its own balance", err)
	}

	// Cross-source isolation.
	if err := m.Acquire(ctx, "crossref", keyed, 0); err != nil {
		t.Fatalf("crossref Acquire = %v, want admission: an openalex floor is not a crossref floor", err)
	}
}

// The floor must bind atomically with the debit, not merely at a pre-check.
// A worker that clears the pre-check can still wait locally — in the gate loop
// or on the token bucket — and during that wait another goroutine's response
// headers commit the floor. Such a worker has not reached the transport, so
// admitting it is a NEW request against a floor papio had already recorded.
func TestReserveRefusesAFloorThatLandedDuringTheWait(t *testing.T) {
	m := testManager(t)
	keyed, _ := keyedAndAnon()
	ctx := context.Background()
	until := time.Now().UTC().Add(6 * time.Hour)

	// Simulate the race precisely: the floor becomes durable after any
	// pre-check would have passed, so go straight to the committing step.
	if err := m.Defer(ctx, QuotaSourceName("openalex"), keyed, until); err != nil {
		t.Fatal(err)
	}
	err := m.reserve(ctx, "openalex", identityFor(keyed), keyed.MaxCostUSD, 0)
	var deferred *ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("reserve = %v, want *ErrDeferred from the provider floor", err)
	}
	if !deferred.Quota {
		t.Fatalf("deferred.Quota = false, want the provider-quota discriminator")
	}
}

// Making the floor authoritative inside Acquire silently destroyed the
// fallback: AcquireAny's own pre-check passed, Acquire's did not, and the
// resulting refusal was indistinguishable from an ordinary gate — so the
// keyless identity sitting there unspent was never tried.
func TestAcquireAnyFallsBackWhenTheFloorLandsAfterItsPreCheck(t *testing.T) {
	m := testManager(t)
	keyed, anon := keyedAndAnon()
	ctx := context.Background()

	// Gate the keyed identity's quota row. AcquireAny's pre-check sees it too,
	// so to isolate the Acquire-side path the test asserts the observable
	// outcome both share: the keyless identity is admitted, not parked.
	if err := m.Defer(ctx, QuotaSourceName("openalex"), keyed, time.Now().UTC().Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	chosen, err := m.AcquireAny(ctx, "openalex", []config.Source{keyed, anon}, 0)
	if err != nil {
		t.Fatalf("AcquireAny = %v, want the keyless identity admitted", err)
	}
	if chosen.APIKey != "" {
		t.Fatalf("chosen = %+v, want the keyless identity", chosen)
	}
}

// An ordinary gate is NOT a credential-switch licence: it says this source is
// unavailable no matter who asks, so AcquireAny must return it rather than
// spending a second identity's allowance on a refusal that has nothing to do
// with quota.
func TestAcquireAnyDoesNotFallBackOnAnOrdinaryGate(t *testing.T) {
	m := testManager(t)
	keyed, anon := keyedAndAnon()
	ctx := context.Background()
	if err := m.Defer(ctx, "openalex", keyed, time.Now().UTC().Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	chosen, err := m.AcquireAny(ctx, "openalex", []config.Source{keyed, anon}, 0)
	var deferred *ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("AcquireAny = %v, want the ordinary gate returned", err)
	}
	if deferred.Quota {
		t.Fatalf("deferred.Quota = true, want an ordinary gate")
	}
	if chosen.APIKey != keyed.APIKey {
		t.Fatalf("chosen = %+v, want the keyed identity that was actually refused", chosen)
	}
}

// The exact sequence, isolated: the floor lands AFTER the keyed identity's
// pre-check and BEFORE its egress. An ordinary gate inside MaxInlineWait makes
// Acquire sleep after its own quota check has already passed, and the floor is
// committed during that sleep — so only the transactional re-read in reserve can
// catch it, and only the typed refusal can keep the fallback alive. Before the
// fix this parked the job with the keyless tier untouched.
func TestAcquireAnyFallsBackOnAFloorCommittedDuringAnInlineWait(t *testing.T) {
	m := testManager(t)
	keyed, anon := keyedAndAnon()
	ctx := context.Background()

	// A short ordinary gate: well inside MaxInlineWait, so Acquire waits it out
	// inline rather than deferring, leaving a window with no keyed quota gate.
	if err := m.Defer(ctx, "openalex", keyed, time.Now().UTC().Add(300*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	committed := make(chan error, 1)
	go func() {
		time.Sleep(80 * time.Millisecond)
		committed <- m.Defer(ctx, QuotaSourceName("openalex"), keyed, time.Now().UTC().Add(6*time.Hour))
	}()

	chosen, err := m.AcquireAny(ctx, "openalex", []config.Source{keyed, anon}, 0)
	if deferErr := <-committed; deferErr != nil {
		t.Fatal(deferErr)
	}
	if err != nil {
		t.Fatalf("AcquireAny = %v, want the keyless identity admitted after the keyed floor landed mid-wait", err)
	}
	if chosen.APIKey != "" {
		t.Fatalf("chosen = %+v, want the keyless identity", chosen)
	}
}

// A rate-limit backoff is a property of the source and the egress IP, not of the
// credential presented, so it must survive a quota-driven credential switch.
// Durable rows are keyed by (source, identity): skipping the keyed identity on
// quota used to skip its 429 row with it, and the keyless identity - untouched,
// same machine, same IP - was admitted inside the backoff the provider demanded.
func TestAcquireAnyRefusesPastAnOrdinaryGateOnASkippedIdentity(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	keyed, anon := keyedAndAnon()
	reset := time.Now().UTC().Add(6 * time.Hour)
	backoff := time.Now().UTC().Add(90 * time.Second)
	if err := m.Defer(ctx, "openalex_quota", keyed, reset); err != nil {
		t.Fatal(err)
	}
	if err := m.Defer(ctx, "openalex", keyed, backoff); err != nil {
		t.Fatal(err)
	}
	chosen, err := m.AcquireAny(ctx, "openalex", []config.Source{keyed, anon}, 0)
	var deferred *ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("AcquireAny = (%+v, %v), want the 429 backoff returned, not a credential switch", chosen, err)
	}
	if deferred.Quota {
		t.Fatalf("deferred.Quota = true, want the ordinary rate-limit gate reported as such")
	}
	if !deferred.Until.Equal(backoff.Truncate(time.Second)) && !deferred.Until.Equal(backoff) {
		t.Fatalf("deferred.Until = %s, want the 429 backoff instant %s", deferred.Until, backoff)
	}
	anonSnap, err := m.Snapshot(ctx, "openalex", anon)
	if err != nil {
		t.Fatal(err)
	}
	if anonSnap.RequestsInWindow != 0 {
		t.Fatalf("anonymous reservations = %d, want none: the source is rate-limited for every identity", anonSnap.RequestsInWindow)
	}
}
