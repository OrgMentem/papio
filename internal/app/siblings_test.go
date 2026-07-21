// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"errors"
	"testing"
	"time"

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
		t.Fatalf("sibling hop ran despite %d primary candidate(s)", len(adapter.fakeResolver.cands))
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
