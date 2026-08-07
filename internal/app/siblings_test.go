// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/resolver"
	"papio/internal/work"
)

// fakeSiblingResolver yields nothing for the canonical identifier but offers
// an OA sibling version on the hop, mirroring the paywalled-DOI-with-preprint
// case.
type fakeSiblingResolver struct {
	fakeResolver
	siblings    []resolver.Candidate
	siblingErr  error
	hopRequests []work.Work
}

func (f *fakeSiblingResolver) ResolveSiblings(_ context.Context, requested work.Work) ([]resolver.Candidate, error) {
	f.hopRequests = append(f.hopRequests, requested)
	return append([]resolver.Candidate(nil), f.siblings...), f.siblingErr
}

func siblingCandidate() resolver.Candidate {
	return resolver.Candidate{
		Source: "openalex", URL: "https://ssrn.example/paper.pdf",
		Version: resolver.VersionPreprint, AccessBasis: resolver.AccessOpen,
		ReuseLicense: "cc-by-4.0", ExpectedMIME: "application/pdf",
		Direct: true, IdentityConfidence: 0.6,
		ResolvedWork: work.Work{DOI: "10.2139/ssrn.4020557", Title: "Example Paper"},
		Evidence:     []string{"openalex sibling_of=10.1002/example"},
	}
}

func TestZeroCandidatesTriggersSiblingHopAndFetches(t *testing.T) {
	svc, jobs := newTestService(t)
	adapter := &fakeSiblingResolver{
		fakeResolver: fakeResolver{name: "openalex"},
		siblings:     []resolver.Candidate{siblingCandidate()},
	}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
	fetches := 0
	svc.Fetch = fakeDownload(&fetches)
	svc.Validate = passValidation()

	id, err := svc.Submit(context.Background(), doiRequest("wr_sibling_hop_01"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateReady {
		t.Fatalf("job state = %q, want ready via the sibling candidate", got.State)
	}
	if fetches != 1 || len(adapter.hopRequests) != 1 {
		t.Fatalf("fetches = %d hopRequests = %d, want one each", fetches, len(adapter.hopRequests))
	}
	// The sibling candidate must be persisted as an ordinary ranked candidate.
	var persisted int
	if err := jobs.S.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM candidates WHERE job_id = ?`, id).Scan(&persisted); err != nil || persisted != 1 {
		t.Fatalf("persisted candidates = %d, %v; want 1", persisted, err)
	}
}

func TestSiblingHopErrorFallsThroughToExhaustion(t *testing.T) {
	svc, jobs := newTestService(t)
	adapter := &fakeSiblingResolver{
		fakeResolver: fakeResolver{name: "openalex"},
		siblingErr:   errors.New("openalex sibling search failed"),
	}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
	fetches := 0
	svc.Fetch = fakeDownload(&fetches)
	svc.Validate = passValidation()

	id, err := svc.Submit(context.Background(), doiRequest("wr_sibling_hop_02"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateUnavailable || got.TerminalReason != "no legal candidates" {
		t.Fatalf("job = state %q reason %q, want unavailable/no_legal_candidates", got.State, got.TerminalReason)
	}
}

func TestSiblingHopSkippedWhenPrimaryCandidatesExist(t *testing.T) {
	svc, jobs := newTestService(t)
	adapter := &fakeSiblingResolver{
		fakeResolver: fakeResolver{name: "openalex", cands: []resolver.Candidate{{
			Source: "openalex", URL: "https://oa.example/direct.pdf", Version: resolver.VersionPublished,
			AccessBasis: resolver.AccessOpen, ReuseLicense: "cc-by-4.0", ExpectedMIME: "application/pdf",
			Direct: true, IdentityConfidence: 1,
		}}},
		siblings: []resolver.Candidate{siblingCandidate()},
	}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
	fetches := 0
	svc.Fetch = fakeDownload(&fetches)
	svc.Validate = passValidation()

	if _, err := svc.Submit(context.Background(), doiRequest("wr_sibling_hop_03")); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if len(adapter.hopRequests) != 0 {
		t.Fatalf("sibling hop ran despite %d primary candidate(s)", len(adapter.cands))
	}
}

// primaryCandidate is a valid direct OA candidate whose fetch the test fails.
func primaryCandidate() resolver.Candidate {
	return resolver.Candidate{
		Source: "openalex", URL: "https://publisher.example/blocked.pdf", Version: resolver.VersionPublished,
		AccessBasis: resolver.AccessOpen, ReuseLicense: "cc-by-4.0", ExpectedMIME: "application/pdf",
		Direct: true, IdentityConfidence: 1,
	}
}

// failingThen returns a FetchFunc that permanently fails failURL (blocked
// class, never retried) and succeeds for everything else (mirroring
// fakeDownload's success shape).
func failingThen(failURL string, fetches *int) FetchFunc {
	inner := fakeDownload(fetches)
	return func(ctx context.Context, c resolver.Candidate, path string) (fetch.Result, error) {
		if c.URL == failURL {
			return fetch.Result{}, &fetch.Error{Class: fetch.ClassBlocked, HTTPStatus: 403}
		}
		return inner(ctx, c, path)
	}
}

func TestExhaustedPrimaryCandidatesTriggerSiblingHop(t *testing.T) {
	svc, jobs := newTestService(t)
	primary := primaryCandidate()
	adapter := &fakeSiblingResolver{
		fakeResolver: fakeResolver{name: "openalex", cands: []resolver.Candidate{primary}},
		siblings:     []resolver.Candidate{siblingCandidate()},
	}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
	fetches := 0
	svc.Fetch = failingThen(primary.URL, &fetches)
	svc.Validate = passValidation()

	id, err := svc.Submit(context.Background(), doiRequest("wr_sibling_hop_04"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateReady {
		t.Fatalf("job state = %q (%s), want ready via the sibling after primary exhaustion", got.State, got.TerminalReason)
	}
	if len(adapter.hopRequests) != 1 {
		t.Fatalf("hopRequests = %d, want exactly one", len(adapter.hopRequests))
	}
}

func TestSiblingHopFailureStillExhaustsWithoutLooping(t *testing.T) {
	svc, jobs := newTestService(t)
	primary := primaryCandidate()
	sibling := siblingCandidate()
	adapter := &fakeSiblingResolver{
		fakeResolver: fakeResolver{name: "openalex", cands: []resolver.Candidate{primary}},
		siblings:     []resolver.Candidate{sibling},
	}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
	fetches := 0
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		fetches++
		return fetch.Result{}, &fetch.Error{Class: fetch.ClassBlocked, HTTPStatus: 403}
	}
	svc.Validate = passValidation()

	id, err := svc.Submit(context.Background(), doiRequest("wr_sibling_hop_05"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	// Both primary and sibling failed: the exhaustion verdict stands and the
	// hop ran exactly once (dedupe/hopTried prevent any loop).
	if got.State == job.StateReady {
		t.Fatal("job must not be ready when every candidate fails")
	}
	if len(adapter.hopRequests) != 1 {
		t.Fatalf("hopRequests = %d, want exactly one", len(adapter.hopRequests))
	}
	if fetches != 2 {
		t.Fatalf("fetches = %d, want primary + sibling exactly once each", fetches)
	}
}

// fakeRelations satisfies MetadataEnricher and VersionRelations: a no-op
// enricher whose typed version edges are scripted.
type fakeRelations struct {
	siblings []string
	err      error
	calls    []string
}

func (f *fakeRelations) Enrich(_ context.Context, requested work.Work) (work.Work, bool, error) {
	return requested, false, nil
}

func (f *fakeRelations) VersionSiblings(_ context.Context, doi string) ([]string, error) {
	f.calls = append(f.calls, doi)
	return append([]string(nil), f.siblings...), f.err
}

// doiSwitchResolver answers per-DOI, so a test can give the sibling DOI a
// candidate while the canonical DOI stays empty.
type doiSwitchResolver struct {
	name  string
	byDOI map[string][]resolver.Candidate
	calls int
}

func (r *doiSwitchResolver) Name() string { return r.name }
func (r *doiSwitchResolver) Resolve(_ context.Context, requested work.Work) ([]resolver.Candidate, error) {
	r.calls++
	return append([]resolver.Candidate(nil), r.byDOI[requested.DOI]...), nil
}

// switchSiblingResolver is a doiSwitchResolver that also offers a fuzzy
// sibling hop, for tests pinning the typed-then-fuzzy precedence contract.
type switchSiblingResolver struct {
	doiSwitchResolver
	siblings    []resolver.Candidate
	hopRequests []work.Work
}

func (f *switchSiblingResolver) ResolveSiblings(_ context.Context, requested work.Work) ([]resolver.Candidate, error) {
	f.hopRequests = append(f.hopRequests, requested)
	return append([]resolver.Candidate(nil), f.siblings...), nil
}

func typedSiblingCandidate(source string) resolver.Candidate {
	c := siblingCandidate()
	c.Source = source
	c.Evidence = nil
	return c
}

func TestTypedVersionRelationsPrecedeTheFuzzySiblingHop(t *testing.T) {
	const siblingDOI = "10.2139/ssrn.4020557"
	svc, jobs := newTestService(t)
	svc.Enricher = &fakeRelations{siblings: []string{siblingDOI}}
	fuzzy := &fakeSiblingResolver{
		fakeResolver: fakeResolver{name: "openalex"},
		siblings:     []resolver.Candidate{siblingCandidate()},
	}
	svc.Resolvers = []ResolverEntry{
		{Adapter: &doiSwitchResolver{name: "unpaywall", byDOI: map[string][]resolver.Candidate{
			siblingDOI: {typedSiblingCandidate("unpaywall")},
		}}, Policy: config.Source{Enabled: true}},
		{Adapter: fuzzy, Policy: config.Source{Enabled: true}},
	}
	fetches := 0
	svc.Fetch = fakeDownload(&fetches)
	svc.Validate = passValidation()

	id, err := svc.Submit(context.Background(), doiRequest("wr_typed_hop_01"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateReady {
		t.Fatalf("job state = %q, want ready via the typed sibling candidate", got.State)
	}
	// The registrant-asserted edge answered, so the fuzzy hop never ran.
	if len(fuzzy.hopRequests) != 0 {
		t.Fatalf("fuzzy hop requests = %d, want 0: typed edges outrank fuzzy search", len(fuzzy.hopRequests))
	}
	var source string
	if err := jobs.S.DB().QueryRowContext(context.Background(),
		`SELECT source FROM candidates WHERE job_id = ?`, id).Scan(&source); err != nil || source != "unpaywall" {
		t.Fatalf("persisted candidate source = %q, %v; want unpaywall via the typed edge", source, err)
	}
}

func TestTypedSiblingsKeepOpenAccessCandidatesOnly(t *testing.T) {
	const siblingDOI = "10.2139/ssrn.4020557"
	svc, jobs := newTestService(t)
	svc.Enricher = &fakeRelations{siblings: []string{siblingDOI}}
	institutional := typedSiblingCandidate("unpaywall")
	institutional.AccessBasis = resolver.AccessInstitutional
	svc.Resolvers = []ResolverEntry{
		{Adapter: &doiSwitchResolver{name: "unpaywall", byDOI: map[string][]resolver.Candidate{
			siblingDOI: {institutional},
		}}, Policy: config.Source{Enabled: true}},
	}

	id, err := svc.Submit(context.Background(), doiRequest("wr_typed_oa_only_01"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cands, _ := svc.typedSiblings(context.Background(), row)
	if len(cands) != 0 {
		t.Fatalf("typed candidates = %#v, want none: an institutional route for a different DOI signs the operator into the wrong work", cands)
	}
	_ = id
}

func TestTypedRelationsRateLimitParksInsteadOfSettling(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Enricher = &fakeRelations{err: &resolver.TemporaryError{Err: errors.New("crossref 429"), RetryAfter: time.Minute}}
	svc.Resolvers = nil

	if _, err := svc.Submit(context.Background(), doiRequest("wr_typed_429_01")); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cands, plan := svc.typedSiblings(context.Background(), row)
	if len(cands) != 0 {
		t.Fatalf("typed candidates = %#v, want none on a rate limit", cands)
	}
	if plan.TemporaryResolvers != 1 || plan.ResolverTemporary.IsZero() {
		t.Fatalf("plan = %+v, want the 429 recorded as retryable: at the exhaustion boundary a missing retry time is the difference between parking and giving up", plan)
	}
}

func TestTypedSiblingsAllFilteredFallThroughToFuzzyHop(t *testing.T) {
	const siblingDOI = "10.2139/ssrn.4020557"
	svc, jobs := newTestService(t)
	svc.Enricher = &fakeRelations{siblings: []string{siblingDOI}}
	// The typed edge resolves, but only to an institutional candidate the
	// open-access filter drops — the fuzzy adapter must still get its turn.
	institutional := typedSiblingCandidate("openalex")
	institutional.AccessBasis = resolver.AccessInstitutional
	fuzzy := &switchSiblingResolver{
		doiSwitchResolver: doiSwitchResolver{
			name:  "openalex",
			byDOI: map[string][]resolver.Candidate{siblingDOI: {institutional}},
		},
		siblings: []resolver.Candidate{siblingCandidate()},
	}
	svc.Resolvers = []ResolverEntry{{Adapter: fuzzy, Policy: config.Source{Enabled: true}}}
	fetches := 0
	svc.Fetch = fakeDownload(&fetches)
	svc.Validate = passValidation()

	id, err := svc.Submit(context.Background(), doiRequest("wr_typed_fallthrough_01"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateReady {
		t.Fatalf("job state = %q, want ready via the fuzzy hop after every typed candidate was filtered", got.State)
	}
	if len(fuzzy.hopRequests) != 1 {
		t.Fatalf("fuzzy hop requests = %d, want 1: filtered typed candidates must not suppress the fuzzy adapters", len(fuzzy.hopRequests))
	}
}

func TestTypedSiblingsChargeTheBudgetPerLookup(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Enricher = &fakeRelations{siblings: []string{"10.1234/a", "10.1234/b", "10.1234/c"}}
	svc.Budgets = budget.New(jobs.S)
	adapter := &doiSwitchResolver{name: "unpaywall"}
	// Cap the monthly spend at two request-units: with one Acquire per
	// lookup the third sibling is refused, so the adapter sees exactly two
	// requests. The buggy shape (one Acquire for the whole fan-out) would
	// let all three through on a single reserved unit.
	svc.Resolvers = []ResolverEntry{{
		Adapter:       adapter,
		Policy:        config.Source{Enabled: true, MaxCostUSD: 2},
		EstimatedCost: 1,
	}}

	if _, err := svc.Submit(context.Background(), doiRequest("wr_typed_budget_01")); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _ = svc.typedSiblings(context.Background(), row); adapter.calls != 2 {
		t.Fatalf("resolver lookups = %d, want 2: the budget must be charged per request, not once per fan-out", adapter.calls)
	}
}
