// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package sourcegate

import (
	"context"
	"errors"
	"testing"

	"papio/internal/config"
)

// pacingReserver records the full admission tuple, because the pacing client's
// contract is what it forwards to the reserver and not merely that it reserves.
type pacingReserver struct {
	sources  []string
	policies []config.Source
	costs    []float64
	err      error
}

func (p *pacingReserver) Acquire(_ context.Context, source string, policy config.Source, cost float64) error {
	p.sources = append(p.sources, source)
	p.policies = append(p.policies, policy)
	p.costs = append(p.costs, cost)
	return p.err
}

// A pacing client without a reserver or without an inner client is unpaced or
// unusable egress. Both are wiring bugs the constructor must refuse rather than
// return a half-built client that silently skips admission.
func TestPacingOnlyRefusesIncompleteWiring(t *testing.T) {
	policy := config.Source{Enabled: true, RatePerSec: 2, Burst: 3}

	client, err := NewPacingOnly(nil, config.SourceOpenAlex, policy, 1, &countingHTTP{})
	if err == nil || client != nil {
		t.Fatalf("NewPacingOnly(nil reserver) = %v, %v, want nil client and an error", client, err)
	}

	client, err = NewPacingOnly(&pacingReserver{}, config.SourceOpenAlex, policy, 1, nil)
	if err == nil || client != nil {
		t.Fatalf("NewPacingOnly(nil inner) = %v, %v, want nil client and an error", client, err)
	}
}

// The pacing client is identity-agnostic on purpose: OpenAlex discovery derives
// its admission identity at the wire. A construction-time api_key surviving
// into the pacing config would attribute one caller's paced egress to another
// caller's key, so the constructor must strip it while keeping the rate knobs.
func TestPacingOnlyStripsPolicyAPIKey(t *testing.T) {
	reserve := &pacingReserver{}
	policy := config.Source{Enabled: true, APIKey: "private-key", RatePerSec: 4, Burst: 7}
	client, err := NewPacingOnly(reserve, config.SourceOpenAlex, policy, 2.5, &countingHTTP{})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(request(t, "https://api.openalex.org/works?api_key=private-key"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if len(reserve.policies) != 1 {
		t.Fatalf("reservations = %d, want 1", len(reserve.policies))
	}
	got := reserve.policies[0]
	if got.APIKey != "" {
		t.Fatalf("reserved policy APIKey = %q, want it stripped", got.APIKey)
	}
	if got.RatePerSec != policy.RatePerSec || got.Burst != policy.Burst || !got.Enabled {
		t.Fatalf("reserved policy = %+v, want rate %v burst %d enabled", got, policy.RatePerSec, policy.Burst)
	}
	if reserve.sources[0] != config.SourceOpenAlex {
		t.Fatalf("reserved against %q, want %q", reserve.sources[0], config.SourceOpenAlex)
	}
	if reserve.costs[0] != 2.5 {
		t.Fatalf("reserved cost = %v, want 2.5", reserve.costs[0])
	}

	// The caller's own request must keep its key: stripping is scoped to the
	// admission policy, never to the wire.
	if policy.APIKey != "private-key" {
		t.Fatalf("caller policy mutated to %+v", policy)
	}
}

// Pacing must happen before the physical call, not alongside it. A reserver
// that refuses is the only discriminating probe for that ordering: a client
// that forwarded first would still show an inner request here.
func TestPacingRefusalProducesZeroInnerRequests(t *testing.T) {
	refusal := errors.New("paced out")
	reserve := &pacingReserver{err: refusal}
	inner := &countingHTTP{}
	client, err := NewPacingOnly(reserve, config.SourceOpenAlex, config.Source{Enabled: true}, 1, inner)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(request(t, "https://api.openalex.org/works"))
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatalf("Do returned a response %v on refusal, want none", resp.StatusCode)
	}
	if !errors.Is(err, refusal) {
		t.Fatalf("Do = %v, want %v", err, refusal)
	}
	if inner.calls != 0 {
		t.Fatalf("inner calls = %d, want 0 when pacing refuses", inner.calls)
	}
	if len(reserve.sources) != 1 {
		t.Fatalf("reservations = %d, want 1", len(reserve.sources))
	}
}
