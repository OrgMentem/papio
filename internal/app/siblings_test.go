// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"papio/internal/config"
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
