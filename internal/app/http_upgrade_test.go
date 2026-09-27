// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/resolver"
	"papio/internal/work"
)

// An http:// resolver candidate is unfetchable as produced: the daemon
// refuses the http scheme before any request. Ingestion must try its https://
// form instead, and the http form must never reach fetch.
func TestHTTPResolverCandidateIsFetchedAsHTTPS(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	adapter := &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
		Source: "fixture", URL: "http://example.test/paper.pdf", Landing: "http://example.test/article",
		ResolvedWork: work.Work{DOI: "10.1002/example", Title: "Example Paper", Authors: []string{"A"}, Year: 2024},
		Version:      resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown",
		Direct: true, IdentityConfidence: 1,
	}}}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
	var fetched []string
	inner := fakeDownload(new(int))
	svc.Fetch = func(ctx context.Context, c resolver.Candidate, path string) (fetch.Result, error) {
		fetched = append(fetched, c.URL)
		return inner(ctx, c, path)
	}
	svc.Validate = passValidation()

	id, err := svc.Submit(ctx, doiRequest("wr_http_upgrade_0001"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(ctx, "w", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	if len(fetched) != 1 || fetched[0] != "https://example.test/paper.pdf" {
		t.Fatalf("fetched = %q, want exactly the https form (http must never be fetched)", fetched)
	}
	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateReady {
		t.Fatalf("job state = %s, want ready", got.State)
	}
	candidate, err := jobs.GetCandidate(ctx, got.SelectedCandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.URLRedacted != "https://example.test/paper.pdf" {
		t.Fatalf("url_redacted = %q, want the https form", candidate.URLRedacted)
	}
	if candidate.LandingRedacted != "https://example.test/article" {
		t.Fatalf("landing_redacted = %q, want the https form", candidate.LandingRedacted)
	}
	wantKey := resolver.Candidate{Source: "fixture", URL: "https://example.test/paper.pdf"}.Key()
	if candidate.URLKey != wantKey {
		t.Fatalf("url_key = %q, want the upgraded key %q", candidate.URLKey, wantKey)
	}
	if !strings.Contains(candidate.RankEvidence, resolver.UpgradeEvidenceURL) ||
		!strings.Contains(candidate.RankEvidence, resolver.UpgradeEvidenceLanding) {
		t.Fatalf("rank_evidence = %q, want both upgrade markers", candidate.RankEvidence)
	}
	var httpLeftovers int
	if err := jobs.S.DB().QueryRow(`SELECT COUNT(*) FROM candidates WHERE job_id = ? AND (url_redacted LIKE 'http://%' OR landing_redacted LIKE 'http://%')`, id).Scan(&httpLeftovers); err != nil {
		t.Fatal(err)
	}
	if httpLeftovers != 0 {
		t.Fatalf("%d persisted candidates still carry http URLs", httpLeftovers)
	}
}

// A resolver that refuses (OpenAlex and Unpaywall both refuse without a
// contact email, before any request) must park its source, not fail the job:
// the pass continues with the remaining sources.
func TestRefusingResolverDoesNotFailJob(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	refusing := &fakeResolver{name: "email-gated", err: errors.New("email-gated: contact email is required")}
	good := &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
		Source: "fixture", URL: "https://example.test/paper.pdf",
		ResolvedWork: work.Work{DOI: "10.1002/example", Title: "Example Paper", Authors: []string{"A"}, Year: 2024},
		Version:      resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown",
		Direct: true, IdentityConfidence: 1,
	}}}
	svc.Resolvers = []ResolverEntry{
		{Adapter: refusing, Policy: config.Source{Enabled: true}},
		{Adapter: good, Policy: config.Source{Enabled: true}},
	}
	svc.Fetch = fakeDownload(new(int))
	svc.Validate = passValidation()

	id, err := svc.Submit(ctx, doiRequest("wr_refusing_resolver_0001"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(ctx, "w", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateReady {
		t.Fatalf("job state = %s, want ready: a refusing source must not fail the job", got.State)
	}
}
