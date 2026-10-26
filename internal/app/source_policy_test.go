// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"testing"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/resolver"
	"papio/internal/work"
)

// TestResolveHonorsDenySource pins the acquire --deny-source contract at the
// resolver loop: a denied source is never resolved and records no attempt.
func TestResolveHonorsDenySource(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	allow := &fakeResolver{name: "allow", cands: []resolver.Candidate{{
		Source: "allow", URL: "https://example.test/allowed.pdf", Direct: true,
		Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown",
		IdentityConfidence: 0.9, Authority: resolver.AuthoritySearch,
		ResolvedWork: work.Work{DOI: "10.1002/deny-source", Title: "Denied Source Paper"},
	}}}
	deny := &fakeResolver{name: "deny", cands: []resolver.Candidate{{
		Source: "deny", URL: "https://example.test/denied.pdf", Direct: true,
		Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown",
		IdentityConfidence: 0.9, Authority: resolver.AuthoritySearch,
		ResolvedWork: work.Work{DOI: "10.1002/deny-source", Title: "Denied Source Paper"},
	}}}
	svc.Resolvers = []ResolverEntry{
		{Adapter: allow, Policy: config.Source{Enabled: true}},
		{Adapter: deny, Policy: config.Source{Enabled: true}},
	}
	policy := testPolicy()
	policy.SourcesDeny = []string{"deny"}
	created, err := jobs.CreateRequestForWork(ctx, "wr_deny_source", work.Work{DOI: "10.1002/deny-source", Title: "Denied Source Paper"}, "", "", policy, nil, job.Attribution{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, created.JobID, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, created.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.resolve(ctx, row); err != nil {
		t.Fatal(err)
	}
	if deny.calls != 0 {
		t.Fatalf("denied resolver calls = %d, want 0", deny.calls)
	}
	if allow.calls == 0 {
		t.Fatal("allowed resolver was never called; the loop did not run")
	}
	var deniedAttempts, allowedAttempts int
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts WHERE job_id = ? AND source = ?`, created.JobID, "deny").Scan(&deniedAttempts); err != nil {
		t.Fatal(err)
	}
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts WHERE job_id = ? AND source = ?`, created.JobID, "allow").Scan(&allowedAttempts); err != nil {
		t.Fatal(err)
	}
	if deniedAttempts != 0 || allowedAttempts == 0 {
		t.Fatalf("attempts denied=%d allowed=%d, want 0 and >=1", deniedAttempts, allowedAttempts)
	}
}
