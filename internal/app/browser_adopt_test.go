// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/resolver"
	"papio/internal/work"
)

// exhaustingService returns a service whose single direct candidate always fails
// with an invalid-payload fetch error, so processing reaches the
// candidates_exhausted boundary.
func exhaustingService(t *testing.T, mode, openURLBase string) (*Service, *job.Store) {
	t.Helper()
	svc, jobs := newTestService(t)
	svc.Config.AccessMode = mode
	svc.Config.Browser.OpenURLBase = openURLBase
	adapter := &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
		Source: "fixture", URL: "https://example.test/paper.pdf",
		ResolvedWork: work.Work{DOI: "10.1002/example"},
		Version:      resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown",
		ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 1,
	}}}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		return fetch.Result{}, &fetch.Error{Class: fetch.ClassInvalid, Msg: "wrong payload"}
	}
	svc.Validate = passValidation()
	return svc, jobs
}

func processToEnd(t *testing.T, svc *Service, jobs *job.Store, reqID string) *job.Row {
	t.Helper()
	ctx := context.Background()
	id, err := svc.Submit(ctx, doiRequest(reqID))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(ctx, "worker", time.Minute)
	if err != nil || row == nil {
		t.Fatalf("claim = %+v, %v", row, err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatalf("process: %v", err)
	}
	out, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func openActionKinds(t *testing.T, jobs *job.Store, jobID string) map[string]bool {
	t.Helper()
	actions, err := jobs.ListHumanActions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, a := range actions {
		if a.JobID == jobID {
			kinds[a.Kind] = true
		}
	}
	return kinds
}

func TestExhaustedCandidatesRouteToInstitutionalHandoff(t *testing.T) {
	for _, mode := range []string{config.ModeAssisted, config.ModeMaximal} {
		t.Run(mode, func(t *testing.T) {
			svc, jobs := exhaustingService(t, mode, "https://openurl.example.edu/resolve")
			row := processToEnd(t, svc, jobs, "wr_handoff_"+mode)
			if row.State != job.StateAwaitingHuman {
				t.Fatalf("state = %s, want awaiting_human", row.State)
			}
			if !openActionKinds(t, jobs, row.ID)["openurl_handoff"] {
				t.Fatal("no open openurl_handoff action")
			}
		})
	}
}

func TestExhaustedCandidatesConservativeRecordsActionButStaysUnavailable(t *testing.T) {
	svc, jobs := exhaustingService(t, config.ModeConservative, "https://openurl.example.edu/resolve")
	row := processToEnd(t, svc, jobs, "wr_conservative")
	if row.State != job.StateUnavailable {
		t.Fatalf("state = %s, want unavailable", row.State)
	}
	kinds := openActionKinds(t, jobs, row.ID)
	if !kinds["openurl_available"] {
		t.Fatal("conservative did not record an openurl_available action")
	}
	if kinds["openurl_handoff"] {
		t.Fatal("conservative must not open a handoff")
	}
}

func TestExhaustedCandidatesWithoutOpenURLBaseStaysUnavailable(t *testing.T) {
	svc, jobs := exhaustingService(t, config.ModeMaximal, "")
	row := processToEnd(t, svc, jobs, "wr_nobase")
	if row.State != job.StateUnavailable {
		t.Fatalf("state = %s, want unavailable", row.State)
	}
	if len(openActionKinds(t, jobs, row.ID)) != 0 {
		t.Fatal("no OpenURL base configured; no institutional action expected")
	}
}

func parkAwaitingHuman(t *testing.T, jobs *job.Store, reqID string) string {
	t.Helper()
	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, reqID, work.Work{DOI: "10.1002/example"}, "", "",
		job.Policy{AccessMode: config.ModeMaximal, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{
		{job.StateQueued, job.StateResolving},
		{job.StateResolving, job.StateFetching},
		{job.StateFetching, job.StateAwaitingHuman},
	} {
		if err := jobs.Transition(ctx, id, step[0], step[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestAdoptDownloadRejectsPathOutsideAdoptionRoot(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Validate = passValidation()
	id := parkAwaitingHuman(t, jobs, "wr_escape")
	// The adoption root must exist for the confinement check to resolve it.
	if err := os.MkdirAll(filepath.Join(svc.Config.EffectiveAdoptionRoot(), id), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "evil.pdf")
	if err := os.WriteFile(outside, []byte("%PDF-1.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.AdoptDownload(context.Background(), id, outside); err == nil {
		t.Fatal("expected rejection of a path outside the adoption root")
	}
	row, _ := jobs.Get(context.Background(), id)
	if row.State != job.StateAwaitingHuman {
		t.Fatalf("job disturbed by rejected adoption: %s", row.State)
	}
}

func TestAdoptDownloadValidatesAndPromotes(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Validate = passValidation()
	id := parkAwaitingHuman(t, jobs, "wr_adopt_ok")
	dir := filepath.Join(svc.Config.EffectiveAdoptionRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pdf := filepath.Join(dir, "paper.pdf")
	if err := os.WriteFile(pdf, pdfBytes("adopted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.AdoptDownload(context.Background(), id, pdf); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	row, _ := jobs.Get(context.Background(), id)
	if row.State != job.StateReady || row.ArtifactSHA256 == "" {
		t.Fatalf("adopted job = %+v", row)
	}
	if err := svc.Artifacts.Verify(row.ArtifactSHA256); err != nil {
		t.Fatalf("artifact verify: %v", err)
	}
}
