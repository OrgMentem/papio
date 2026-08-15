// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"testing"

	"papio/internal/job"
	"papio/internal/resolver"
	"papio/internal/work"
)

func testPolicy() job.Policy {
	return job.Policy{DesiredVersion: "any", AccessMode: "conservative"}
}

func attestSubmittedFields(t *testing.T, jobs *job.Store, workRequestID, fields string) {
	t.Helper()
	_, err := jobs.S.DB().ExecContext(context.Background(),
		`UPDATE work_requests SET submitted_fields = ? WHERE id = ?`, fields, workRequestID)
	if err != nil {
		t.Fatal(err)
	}
}

func TestWeakTitleMatchDoesNotPromoteDOI(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	const title = "Effects of Example Intervention"
	wrongDOI := "10.1000/wrong-paper"
	rightDOI := "10.1000/right-paper"
	svc.Resolvers = []ResolverEntry{{
		Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{
			{
				Source: "fixture", URL: "https://example.org/wrong.pdf", Direct: true,
				Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown",
				IdentityConfidence: 0.75,
				Authority:          resolver.AuthoritySearch,
				ResolvedWork:       work.Work{Title: title, DOI: wrongDOI, Authors: []string{"Wrong Author"}},
			},
			{
				Source: "fixture", URL: "https://example.org/right.pdf", Direct: true,
				Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown",
				IdentityConfidence: 0.75,
				Authority:          resolver.AuthoritySearch,
				ResolvedWork:       work.Work{Title: title, DOI: rightDOI, Authors: []string{"Right Author"}},
			},
		}},
		Policy: svc.Config.SourcePolicy("fixture"),
	}}
	created, err := jobs.CreateRequestForWork(ctx, "wr_title_only", work.Work{Title: title}, "", "", testPolicy(), nil, job.Attribution{}, false)
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, created.JobID)
	if err != nil {
		t.Fatal(err)
	}
	attestSubmittedFields(t, jobs, row.WorkRequestID, "title")
	if err := jobs.Transition(ctx, created.JobID, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	_, plan, err := svc.resolve(ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.IsZero() {
		t.Fatalf("plan = %+v, want zero after settlement", plan)
	}
	got, err := jobs.Get(ctx, created.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateUnavailable || got.TerminalReason != string(job.TerminalReasonInsufficientIdentityEvidence) {
		t.Fatalf("job = state %q reason %q, want unavailable/%s", got.State, got.TerminalReason, job.TerminalReasonInsufficientIdentityEvidence)
	}
	if got.Work.DOI != "" {
		t.Fatalf("promoted DOI = %q, want empty", got.Work.DOI)
	}
}

func TestExactEchoPromotesMetadata(t *testing.T) {
	const doi = "10.1000/echo"
	anchor := work.Work{DOI: doi, Title: "Echo Paper"}
	ranked := []resolver.Candidate{{
		Authority: resolver.AuthorityExactEcho,
		ResolvedWork: work.Work{
			DOI: doi, Title: "Echo Paper", Year: 2020, Authors: []string{"Ada Lovelace"},
		},
	}}
	promoted := accumulatePromotedIdentity(anchor, ranked)
	if promoted.Year != 2020 || len(promoted.Authors) != 1 {
		t.Fatalf("promoted = %+v, want year and authors filled", promoted)
	}
}

func TestTypedRelationDoesNotPromote(t *testing.T) {
	anchor := work.Work{Title: "Sibling Paper"}
	ranked := []resolver.Candidate{{
		Authority:    resolver.AuthorityTypedRelation,
		ResolvedWork: work.Work{DOI: "10.1000/sibling", Title: "Sibling Paper", Year: 2019},
	}}
	promoted := accumulatePromotedIdentity(anchor, ranked)
	if promoted.DOI != "" || promoted.Year != 0 {
		t.Fatalf("promoted = %+v, want no identity adoption", promoted)
	}
}

func TestCrossCandidateIdentifierConflictRejected(t *testing.T) {
	anchor := work.Work{Title: "Shared"}
	ranked := []resolver.Candidate{
		{
			Authority:    resolver.AuthorityExactEcho,
			ResolvedWork: work.Work{DOI: "10.1000/one"},
		},
		{
			Authority:    resolver.AuthorityExactEcho,
			ResolvedWork: work.Work{DOI: "10.1000/two"},
		},
	}
	promoted := accumulatePromotedIdentity(anchor, ranked)
	if promoted.DOI != "10.1000/one" {
		t.Fatalf("promoted DOI = %q, want first candidate only", promoted.DOI)
	}
}

func TestUnattestedAnchorSkipsDOICache(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	const doi = "10.1000/cache-test"
	first, err := jobs.CreateRequest(ctx, "wr_cache_first", work.Work{DOI: doi, Title: "Cached"}, "", "", testPolicy(), nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	firstRow, err := jobs.Get(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	attestSubmittedFields(t, jobs, firstRow.WorkRequestID, "doi,title")
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := jobs.UpsertArtifact(ctx, job.Artifact{SHA256: sha, MIME: "application/pdf", Path: t.TempDir() + "/x.pdf"}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, first, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, first, job.StateResolving, job.StateReady, map[string]any{"source": "test"}, job.WithArtifact(sha)); err != nil {
		t.Fatal(err)
	}
	second, err := jobs.CreateRequest(ctx, "wr_cache_second", work.Work{DOI: doi, Title: "Cached"}, "", "", testPolicy(), nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a legacy/unattested row: provenance plumbing now marks every
	// fresh request attested, so clear the anchor the way a pre-migration row
	// would have it (NULL submitted_fields).
	secondRow, err := jobs.Get(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx, `UPDATE work_requests SET submitted_fields = NULL WHERE id = ?`, secondRow.WorkRequestID); err != nil {
		t.Fatal(err)
	}
	anchor, err := jobs.SubmittedIdentity(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if anchor.Attested {
		t.Fatal("legacy row must be unattested")
	}
	if anchor.AnchorAllowsDOICache(doi) {
		t.Fatal("unattested anchor must not authorize cache reuse")
	}
	cached, _, err := jobs.FindArtifactByDOI(ctx, doi)
	if err != nil {
		t.Fatal(err)
	}
	if cached == nil {
		t.Fatal("fixture cache row missing")
	}
	if cached != nil && svc.Artifacts.Verify(cached.SHA256) == nil && anchor.AnchorAllowsDOICache(doi) {
		t.Fatal("Process would have taken the DOI cache fast path")
	}
}
