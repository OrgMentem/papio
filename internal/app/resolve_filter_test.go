// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"testing"

	"papio/internal/config"
	"papio/internal/resolver"
	"papio/internal/work"
)

// The primary resolve pass is the only app-level gate over resolver output:
// it must drop candidates with a foreign Source, failing ValidateCandidate,
// or a ResolvedWork that conflicts with the job's work. Without this test a
// refactor losing the filter passes the suite while a misattributed,
// header-injecting, or wrong-paper candidate reaches fetch and budget
// accounting.
func TestResolveDropsInvalidAndConflictingCandidates(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	good := resolver.Candidate{
		Source: "fixture", URL: "https://example.test/good.pdf",
		ResolvedWork: work.Work{DOI: "10.1002/example"},
		Version:      resolver.VersionPublished, AccessBasis: resolver.AccessOpen,
		ReuseLicense: "unknown", Direct: true, IdentityConfidence: 0.9,
	}
	adapter := &fakeResolver{name: "fixture", cands: []resolver.Candidate{
		good,
		{ // Conflicting identity: names a different DOI.
			Source: "fixture", URL: "https://example.test/conflict.pdf",
			ResolvedWork: work.Work{DOI: "10.1000/other-paper"},
			Version:      resolver.VersionPublished, AccessBasis: resolver.AccessOpen,
			ReuseLicense: "unknown", Direct: true, IdentityConfidence: 0.9,
		},
		{ // Foreign source: attributed to another resolver.
			Source: "unpaywall", URL: "https://example.test/foreign.pdf",
			ResolvedWork: work.Work{DOI: "10.1002/example"},
			Version:      resolver.VersionPublished, AccessBasis: resolver.AccessOpen,
			ReuseLicense: "unknown", Direct: true, IdentityConfidence: 0.9,
		},
		{ // Rejected by ValidateCandidate: non-http(s) scheme.
			Source: "fixture", URL: "ftp://example.test/evil.pdf",
			ResolvedWork: work.Work{DOI: "10.1002/example"},
			Version:      resolver.VersionPublished, AccessBasis: resolver.AccessOpen,
			ReuseLicense: "unknown", Direct: true, IdentityConfidence: 0.9,
		},
		{ // Rejected by ValidateCandidate: CRLF in request headers.
			Source: "fixture", URL: "https://example.test/headers.pdf",
			RequestHeaders: map[string]string{"X-Evil": "a\r\nb"},
			ResolvedWork:   work.Work{DOI: "10.1002/example"},
			Version:        resolver.VersionPublished, AccessBasis: resolver.AccessOpen,
			ReuseLicense: "unknown", Direct: true, IdentityConfidence: 0.9,
		},
	}}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}

	id, err := svc.Submit(ctx, doiRequest("wr_resolve_filter_0001"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	live, _, err := svc.resolve(ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("live candidates = %d, want 1 (only the good candidate)", len(live))
	}
	for _, c := range live {
		if c.URL != good.URL {
			t.Fatalf("live candidate URL = %q, want %q", c.URL, good.URL)
		}
	}
	var stored int
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM candidates WHERE job_id = ?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("stored candidates = %d, want 1", stored)
	}
	var redacted string
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT url_redacted FROM candidates WHERE job_id = ?`, id).Scan(&redacted); err != nil {
		t.Fatal(err)
	}
	if redacted != good.URL {
		t.Fatalf("stored url_redacted = %q, want %q", redacted, good.URL)
	}
	var detail string
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT detail FROM attempts WHERE job_id = ? AND stage = 'resolve'`, id).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if detail != "candidates=1" {
		t.Fatalf("resolve attempt detail = %q, want %q", detail, "candidates=1")
	}
}
