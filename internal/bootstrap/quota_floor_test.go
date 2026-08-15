// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package bootstrap

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/sourcegate"
	"papio/internal/store"
)

type countingInner struct{ calls int }

func (c *countingInner) Do(*http.Request) (*http.Response, error) {
	c.calls++
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}, nil
}

func floorTestBudgets(t *testing.T) *budget.Manager {
	t.Helper()
	s, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return budget.New(s)
}

func floorRequest(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return (&http.Request{Method: http.MethodGet, URL: parsed}).WithContext(context.Background())
}

// Discovery shares OpenAlex's keyed daily budget with the resolver and
// enrichment paths, so it must be refused at the same header-derived floor —
// before any request leaves the process. It gains no fallback identity.
func TestQuotaAwareReserverHonorsFloor(t *testing.T) {
	budgets := floorTestBudgets(t)
	ctx := context.Background()
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	if err := budgets.Defer(ctx, "openalex_quota", keyed, time.Now().UTC().Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	inner := &countingInner{}
	gated, err := sourcegate.New(quotaAwareReserver{budgets}, config.SourceOpenAlex, keyed, 0, inner)
	if err != nil {
		t.Fatal(err)
	}
	_, err = gated.Do(floorRequest(t, "https://api.openalex.org/works?api_key=private-key"))
	var deferred *budget.ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("err = %v, want *budget.ErrDeferred from the quota floor", err)
	}
	if inner.calls != 0 {
		t.Fatalf("inner requests = %d, want zero: the floor refuses at admission", inner.calls)
	}
}

// A source nothing writes a "_quota" row for keeps its ordinary gate semantics
// exactly, and an OpenAlex quota gate never leaks across source names.
func TestOrdinaryGateStillRefusesOtherDiscoverySources(t *testing.T) {
	budgets := floorTestBudgets(t)
	ctx := context.Background()
	policy := config.Source{Enabled: true}
	inner := &countingInner{}
	gated, err := sourcegate.New(budgets, config.SourceSemanticScholar, policy, 0, inner)
	if err != nil {
		t.Fatal(err)
	}
	if err := budgets.Defer(ctx, config.SourceSemanticScholar, policy, time.Now().UTC().Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, err = gated.Do(floorRequest(t, "https://api.semanticscholar.org/graph/v1/paper/search"))
	var deferred *budget.ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("err = %v, want the ordinary durable gate to refuse admission", err)
	}
	if inner.calls != 0 {
		t.Fatalf("inner requests = %d, want zero", inner.calls)
	}

	// Cross-source isolation: an OpenAlex quota gate alone leaves Semantic
	// Scholar admission untouched.
	fresh := floorTestBudgets(t)
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	if err := fresh.Defer(ctx, "openalex_quota", keyed, time.Now().UTC().Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	isolatedInner := &countingInner{}
	isolated, err := sourcegate.New(fresh, config.SourceSemanticScholar, policy, 0, isolatedInner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := isolated.Do(floorRequest(t, "https://api.semanticscholar.org/graph/v1/paper/search")); err != nil {
		t.Fatalf("err = %v, want an OpenAlex quota gate to leave other sources alone", err)
	}
	if isolatedInner.calls != 1 {
		t.Fatalf("inner requests = %d, want the request forwarded", isolatedInner.calls)
	}
}

// The OpenAlex enricher shares the same daily budget as the resolver and
// discovery paths, so its client must be the observer too — a silently
// unobserved client is the invisible-spend defect all over again.
func TestOpenAlexEnricherClientIsObserved(t *testing.T) {
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	cfg.PDF.OCREnabled = false
	cfg.Zotio.AutoEnrich = false
	openAlex := cfg.Sources[config.SourceOpenAlex]
	openAlex.Enabled = true
	openAlex.APIKey = "private-key"
	cfg.Sources[config.SourceOpenAlex] = openAlex
	system, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := system.Close(); err != nil {
			t.Errorf("close system: %v", err)
		}
	})
	var enricher any
	for _, entry := range system.App.MetadataEnrichers {
		if entry.Name == config.SourceOpenAlex {
			enricher = entry.Enricher
		}
	}
	if enricher == nil {
		t.Fatal("OpenAlex metadata enricher was not wired while its source is enabled")
	}
	client := reflect.ValueOf(enricher).Elem().FieldByName("client")
	if !client.IsValid() || client.IsNil() {
		t.Fatalf("enricher client = %#v, want a wired client", client)
	}
	// reflect cannot hand back a value read from an unexported field, so compare
	// the interface's dynamic type instead.
	if got := client.Elem().Type(); got != reflect.TypeFor[*sourcegate.Observer]() {
		t.Fatalf("enricher client = %s, want *sourcegate.Observer", got)
	}
}
