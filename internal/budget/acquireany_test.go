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
