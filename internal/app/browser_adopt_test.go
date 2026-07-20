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
	"papio/internal/pdf"
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

func TestBotBlockedOACandidateRoutesToBrowserHandoff(t *testing.T) {
	const oaURL = "https://oa.example.org/articles/blocked-paper.pdf"
	svc, jobs := newTestService(t)
	svc.Config.AccessMode = config.ModeMaximal
	svc.Config.Browser.OpenURLBase = "https://openurl.example.edu/resolve"
	svc.Resolvers = []ResolverEntry{{
		Adapter: &fakeResolver{name: "openalex", cands: []resolver.Candidate{{
			Source: "openalex", URL: oaURL, ResolvedWork: work.Work{DOI: "10.1002/example"},
			Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown",
			ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 1,
		}}},
		Policy: config.Source{Enabled: true},
	}}
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		return fetch.Result{}, &fetch.Error{Class: fetch.ClassInvalid, HTTPStatus: 403, Msg: "permanent HTTP response"}
	}
	svc.Validate = passValidation()

	row := processToEnd(t, svc, jobs, "wr_oa_bot_block")
	if row.State != job.StateAwaitingHuman {
		t.Fatalf("state = %s, want awaiting_human", row.State)
	}
	actions, err := jobs.ListHumanActions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.JobID == row.ID && action.Kind == "openurl_handoff" {
			if action.Detail != OABrowserHandoffActionDetail(oaURL) {
				t.Fatalf("handoff detail = %q, want OA browser marker and URL", action.Detail)
			}
			return
		}
	}
	t.Fatal("missing OA browser handoff")
}

func TestForbiddenNonOACandidateKeepsInstitutionalHandoff(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Config.AccessMode = config.ModeMaximal
	svc.Config.Browser.OpenURLBase = "https://openurl.example.edu/resolve"
	svc.Resolvers = []ResolverEntry{{
		Adapter: &fakeResolver{name: "licensed", cands: []resolver.Candidate{{
			Source: "licensed", URL: "https://licensed.example.org/paper.pdf",
			ResolvedWork: work.Work{DOI: "10.1002/example"},
			Version:      resolver.VersionPublished, AccessBasis: resolver.AccessLicensedAPI, ReuseLicense: "unknown",
			ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 1,
		}}},
		Policy: config.Source{Enabled: true},
	}}
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		return fetch.Result{}, &fetch.Error{Class: fetch.ClassInvalid, HTTPStatus: 403, Msg: "permanent HTTP response"}
	}
	svc.Validate = passValidation()

	row := processToEnd(t, svc, jobs, "wr_non_oa_forbidden")
	actions, err := jobs.ListHumanActions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.JobID == row.ID && action.Kind == "openurl_handoff" {
			if action.Detail != InstitutionalOpenURLHandoffDetail {
				t.Fatalf("handoff detail = %q, want institutional marker", action.Detail)
			}
			return
		}
	}
	t.Fatal("missing institutional handoff")
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

func TestAcceptedAdoptionReviewReusesExactContentOverride(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Config.Browser.OpenURLBase = "https://resolver.example.edu/openurl"
	svc.Config.AccessMode = config.ModeMaximal
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		return fetch.Result{}, context.Canceled
	}
	svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 2},
			Text:       pdf.TextReport{Chars: 2000},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityReview},
		}, nil
	}
	ctx := context.Background()
	id := parkAwaitingHuman(t, jobs, "wr_adopt_review")
	dir := filepath.Join(svc.Config.EffectiveAdoptionRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "paper.pdf")
	if err := os.WriteFile(path, pdfBytes("reviewed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.AdoptDownload(ctx, id, path); err != nil {
		t.Fatalf("first adopt: %v", err)
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	var reviewID int64
	for _, action := range actions {
		if action.JobID == id && action.Kind == "verify_identity" {
			reviewID = action.ID
			break
		}
	}
	if reviewID == 0 {
		t.Fatal("missing verify_identity action")
	}
	if _, state, err := jobs.ResolveReview(ctx, reviewID, "accept"); err != nil || state != job.StateFetching {
		t.Fatalf("accept review = %q, %v", state, err)
	}

	// The scheduler cannot retain a live browser candidate, so it re-resolves
	// and returns to the institutional handoff. The unchanged source file is
	// then adopted again by the directory sweep.
	row, err := jobs.ClaimNext(ctx, "review-worker", time.Minute)
	if err != nil || row == nil || row.ID != id {
		t.Fatalf("claim accepted review = %+v, %v", row, err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatalf("re-resolve accepted review: %v", err)
	}
	if err := svc.AdoptDownload(ctx, id, path); err != nil {
		t.Fatalf("second adopt: %v", err)
	}
	ready, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if ready.State != job.StateReady || ready.ArtifactSHA256 == "" {
		t.Fatalf("reviewed adoption = %+v", ready)
	}
	art, err := jobs.GetArtifact(ctx, ready.ArtifactSHA256)
	if err != nil || art == nil || art.IdentityResult != "user_confirmed" {
		t.Fatalf("reviewed artifact = %+v, %v", art, err)
	}
}

func TestAdoptDownloadRepArksToAwaitingHumanOnValidationInfraError(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Validate = passValidation()
	id := parkAwaitingHuman(t, jobs, "wr_adopt_infra")
	dir := filepath.Join(svc.Config.EffectiveAdoptionRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pdfPath := filepath.Join(dir, "paper.pdf")
	if err := os.WriteFile(pdfPath, pdfBytes("adopted"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Make the immutable artifact store read-only so Promote (an atomic rename
	// into it) fails after validation passes: a post-validation infra error
	// that leaves validateCandidate returning (false, false, err). The
	// quarantine and database dirs stay writable, so only Promote fails.
	artRoot := filepath.Join(svc.Config.DataDir, "artifacts")
	if err := os.Chmod(artRoot, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(artRoot, 0o700) })

	if err := svc.AdoptDownload(context.Background(), id, pdfPath); err == nil {
		t.Fatal("expected the promote failure to surface")
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateAwaitingHuman {
		t.Fatalf("adoption infra error left job in %s, want awaiting_human (a validating strand lets RecoverStale rewind it to resolving and discard the file)", row.State)
	}
	// The adopted file must be preserved so the directory sweep can retry it.
	if _, statErr := os.Stat(pdfPath); statErr != nil {
		t.Fatalf("adopted file was not preserved for retry: %v", statErr)
	}
}

func TestAdoptDownloadRejectedUnquarantinableGoesNeedsReview(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 2},
			Text:       pdf.TextReport{Chars: 2000},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityReject},
		}, nil
	}
	id := parkAwaitingHuman(t, jobs, "wr_reject_stuck")
	dir := filepath.Join(svc.Config.EffectiveAdoptionRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pdfPath := filepath.Join(dir, "paper.pdf")
	if err := os.WriteFile(pdfPath, pdfBytes("adopted"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Occupy the rejected/ sibling with a regular file so MkdirAll(rejected/<job>)
	// fails: the rejected download then cannot be moved out of the adoption dir.
	if err := os.WriteFile(filepath.Join(svc.Config.EffectiveAdoptionRoot(), "rejected"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.AdoptDownload(context.Background(), id, pdfPath); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateNeedsReview {
		t.Fatalf("unquarantinable rejection left job in %s, want needs_review so the sweep stops re-adopting", row.State)
	}
	// The file remains where the user can act on it.
	if _, statErr := os.Stat(pdfPath); statErr != nil {
		t.Fatalf("rejected file not preserved: %v", statErr)
	}
}
