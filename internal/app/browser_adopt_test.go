// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"papio/internal/artifact"
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
	for _, mode := range []string{config.ModeAssisted, config.ModeDelegated} {
		t.Run(mode, func(t *testing.T) {
			svc, jobs := exhaustingService(t, mode, "https://openurl.example.edu/resolve")
			row := processToEnd(t, svc, jobs, "wr_handoff_"+mode)
			if row.State != job.StateAwaitingHuman {
				t.Fatalf("state = %s, want awaiting_human", row.State)
			}
			actions, err := jobs.ListHumanActions(context.Background(), true)
			if err != nil {
				t.Fatal(err)
			}
			for _, action := range actions {
				if action.JobID == row.ID && action.Kind == "openurl_handoff" {
					if !action.RequiresAuth || action.BlockedBy != "paywall" {
						t.Fatalf("handoff access = requires_auth %t, blocked_by %q, want true/paywall", action.RequiresAuth, action.BlockedBy)
					}
					return
				}
			}
			t.Fatal("missing institutional handoff")
		})
	}
}

func TestBotBlockedOACandidateRoutesToBrowserHandoff(t *testing.T) {
	const oaURL = "https://oa.example.org/articles/blocked-paper.pdf"
	svc, jobs := newTestService(t)
	svc.Config.AccessMode = config.ModeDelegated
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
			if action.RequiresAuth || action.BlockedBy != "anti_bot" {
				t.Fatalf("handoff access = requires_auth %t, blocked_by %q, want false/anti_bot", action.RequiresAuth, action.BlockedBy)
			}
			return
		}
	}
	t.Fatal("missing OA browser handoff")
}

func TestForbiddenNonOACandidateKeepsInstitutionalHandoff(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Config.AccessMode = config.ModeDelegated
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
			if !action.RequiresAuth || action.BlockedBy != "paywall" {
				t.Fatalf("handoff access = requires_auth %t, blocked_by %q, want true/paywall", action.RequiresAuth, action.BlockedBy)
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

// TestPerRequestAccessModeOverrideGovernsTheHandoffDecision pins the fix for a
// defect where access_mode_override was validated, snapshotted into
// job.Policy.AccessMode, and printed by status/diagnose while
// exhaustedCandidates read the daemon-wide s.Config.AccessMode instead. The
// override therefore looked like it worked and reported that it worked while
// changing nothing.
// Both directions are asserted deliberately, and they assert different things.
// Narrowing must take effect; widening must not. Checking only one direction
// would pass against a build that simply hardcoded that answer, which is how
// the original defect hid.
func TestPerRequestAccessModeOverrideGovernsTheHandoffDecision(t *testing.T) {
	const base = "https://openurl.example.edu/resolve"
	for _, tc := range []struct {
		name       string
		configMode string
		override   string
		wantMode   string
		wantState  string
		wantKind   string
		denyKind   string
	}{
		{
			name:       "conservative override narrows a delegated daemon and opens no handoff",
			configMode: config.ModeDelegated,
			override:   config.ModeConservative,
			wantMode:   config.ModeConservative,
			wantState:  job.StateUnavailable,
			wantKind:   "openurl_available",
			denyKind:   "openurl_handoff",
		},
		{
			// The daemon-wide mode is a ceiling, not a default: a submitter
			// cannot raise automation above what the operator configured.
			name:       "delegated override cannot widen a conservative daemon",
			configMode: config.ModeConservative,
			override:   config.ModeDelegated,
			wantMode:   config.ModeConservative,
			wantState:  job.StateUnavailable,
			wantKind:   "openurl_available",
			denyKind:   "openurl_handoff",
		},
		{
			name:       "assisted override narrows a delegated daemon and still hands off",
			configMode: config.ModeDelegated,
			override:   config.ModeAssisted,
			wantMode:   config.ModeAssisted,
			wantState:  job.StateAwaitingHuman,
			wantKind:   "openurl_handoff",
			denyKind:   "openurl_available",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, jobs := exhaustingService(t, tc.configMode, base)
			request := doiRequest("wr_override_gate")
			request.AccessModeOverride = tc.override

			ctx := context.Background()
			id, err := svc.Submit(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			row, err := jobs.ClaimNext(ctx, "worker", time.Minute)
			if err != nil || row == nil {
				t.Fatalf("claim = %+v, %v", row, err)
			}
			// The snapshot is the input the decision path must consult, and it
			// records the clamped mode rather than the requested one, so
			// diagnose never reports an override the daemon declined to honour.
			if row.Policy.AccessMode != tc.wantMode {
				t.Fatalf("policy snapshot = %q, want %q", row.Policy.AccessMode, tc.wantMode)
			}
			if err := svc.Process(ctx, row); err != nil {
				t.Fatalf("process: %v", err)
			}
			out, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if out.State != tc.wantState {
				t.Fatalf("state = %s, want %s; the daemon-wide %s mode governed instead of the override",
					out.State, tc.wantState, tc.configMode)
			}
			kinds := openActionKinds(t, jobs, id)
			if !kinds[tc.wantKind] {
				t.Fatalf("open actions = %v, want %s", kinds, tc.wantKind)
			}
			if kinds[tc.denyKind] {
				t.Fatalf("open actions = %v, must not contain %s", kinds, tc.denyKind)
			}
		})
	}
}

func TestExhaustedCandidatesWithoutOpenURLBaseStaysUnavailable(t *testing.T) {
	svc, jobs := exhaustingService(t, config.ModeDelegated, "")
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
	id, err := jobs.CreateRequest(ctx, reqID, work.Work{DOI: "10.1002/example"}, "", "", job.Policy{AccessMode: config.ModeDelegated, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
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

func TestAdoptDownloadResolvesSatisfiedHandoffActions(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 2},
			Text:       pdf.TextReport{Chars: 2000},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityReview},
		}, nil
	}
	ctx := context.Background()
	id := parkAwaitingHuman(t, jobs, "wr_adopt_resolves_handoff")
	handoffID, err := jobs.OpenHumanAction(ctx, id, "openurl_handoff", "institutional handoff")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(svc.Config.EffectiveAdoptionRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "paper.pdf")
	if err := os.WriteFile(path, pdfBytes("handoff satisfied"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.AdoptDownload(ctx, id, path); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateNeedsReview {
		t.Fatalf("adopted job state = %s, want needs_review", row.State)
	}
	actions, err := jobs.ListHumanActions(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var handoff, review *job.HumanAction
	for i := range actions {
		action := &actions[i]
		if action.JobID != id {
			continue
		}
		switch action.ID {
		case handoffID:
			handoff = action
		default:
			if action.Kind == "verify_identity" {
				review = action
			}
		}
	}
	if handoff == nil || handoff.Status != "resolved" {
		t.Fatalf("handoff action = %+v, want resolved", handoff)
	}
	if review == nil || review.Status != "open" {
		t.Fatalf("verify_identity action = %+v, want open", review)
	}
}

func TestAcceptedAdoptionReviewReusesExactContentOverride(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Config.Browser.OpenURLBase = "https://resolver.example.edu/openurl"
	svc.Config.AccessMode = config.ModeDelegated
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

	// The accepted binding points at the quarantined adopted bytes, so Process
	// validates and promotes them without requiring a second directory sweep.
	row, err := jobs.ClaimNext(ctx, "review-worker", time.Minute)
	if err != nil || row == nil || row.ID != id {
		t.Fatalf("claim accepted review = %+v, %v", row, err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatalf("reuse accepted review: %v", err)
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

// A browser-adopted download's version must always be `unknown`: adoption sees
// bytes arrive from a human's browser and never learns which version that human
// chose. Reporting the request's DesiredVersion *preference* as the obtained
// fact would let a consumer act on papio's own guess (ADR-0007).
func TestAdoptedCandidateVersionIsAlwaysUnknown(t *testing.T) {
	for _, desired := range []string{"any", resolver.VersionAccepted, resolver.VersionPublished, resolver.VersionPreprint} {
		t.Run("desired_"+desired, func(t *testing.T) {
			svc, jobs := newTestService(t)
			svc.Validate = passValidation()
			ctx := context.Background()
			id, err := jobs.CreateRequest(ctx, "wr_adopt_version_"+desired, work.Work{DOI: "10.1002/example"}, "", "", job.Policy{AccessMode: config.ModeDelegated, DesiredVersion: desired, FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
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
			dir := filepath.Join(svc.Config.EffectiveAdoptionRoot(), id)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "paper.pdf")
			if err := os.WriteFile(path, pdfBytes("adopted "+desired), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := svc.AdoptDownload(ctx, id, path); err != nil {
				t.Fatalf("adopt: %v", err)
			}
			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if row.SelectedCandidateID == 0 {
				t.Fatalf("adopted job has no selected candidate (state %s)", row.State)
			}
			candidate, err := jobs.GetCandidate(ctx, row.SelectedCandidateID)
			if err != nil {
				t.Fatal(err)
			}
			if candidate.Version != resolver.VersionUnknown {
				t.Fatalf("adopted version = %q, want %q", candidate.Version, resolver.VersionUnknown)
			}
			// The honest fields around it must not drift either.
			if candidate.AccessBasis != resolver.AccessInstitutional || candidate.ReuseLicense != "unknown" {
				t.Fatalf("adopted access/licence = %q/%q, want institutional/unknown",
					candidate.AccessBasis, candidate.ReuseLicense)
			}
		})
	}
}

// InsertCandidates is INSERT OR IGNORE on (job_id, url_key) and adoption keys by
// content hash, so a candidate written before papio stopped synthesizing versions
// survives re-adoption of the same bytes. Adoption must normalize it rather than
// re-read the stale `published` claim.
func TestAdoptDownloadNormalizesAPreUpgradeSynthesizedVersion(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Validate = passValidation()
	ctx := context.Background()
	id := parkAwaitingHuman(t, jobs, "wr_adopt_preupgrade")
	dir := filepath.Join(svc.Config.EffectiveAdoptionRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "paper.pdf")
	body := pdfBytes("pre-upgrade adoption")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sha, _, err := artifact.HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly the row an older papio wrote for these bytes.
	if _, err := jobs.InsertCandidates(ctx, id, []job.Candidate{{
		JobID: id, Source: "browser", URLRedacted: "browser://adopted-download",
		URLKey: "browser-adopt:sha256:" + sha, Version: resolver.VersionPublished,
		AccessBasis: resolver.AccessInstitutional, ReuseLicense: "unknown",
		ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 0.5, Rank: 0,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.AdoptDownload(ctx, id, path); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := jobs.GetCandidate(ctx, row.SelectedCandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Version != resolver.VersionUnknown {
		t.Fatalf("re-adopted candidate version = %q, want %q — the pre-upgrade claim outlived the fix",
			candidate.Version, resolver.VersionUnknown)
	}
}

// TestRetryAfterWideningAccessModeEscalatesAConservativeJob pins the recovery
// path the conservative advisory itself prescribes: "a route exists, this mode
// will not take it" -> operator widens access_mode -> `papio jobs retry`.
//
// Making the job's policy snapshot authoritative broke this, because the
// snapshot is immutable across a retry: the job would re-exhaust under its
// original conservative mode and reopen the same advisory forever, telling the
// operator to do the thing they had just done. Retry therefore releases the
// pinned mode when it cancels the advisory.
func TestRetryAfterWideningAccessModeEscalatesAConservativeJob(t *testing.T) {
	const base = "https://openurl.example.edu/resolve"
	ctx := context.Background()
	svc, jobs := exhaustingService(t, config.ModeConservative, base)

	id, err := svc.Submit(ctx, doiRequest("wr_escalate_after_retry"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(ctx, "worker", time.Minute)
	if err != nil || row == nil {
		t.Fatalf("claim = %+v, %v", row, err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	out, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != job.StateUnavailable || !openActionKinds(t, jobs, id)["openurl_available"] {
		t.Fatalf("state = %s with actions %v, want unavailable plus the conservative advisory", out.State, openActionKinds(t, jobs, id))
	}

	// The operator takes the advisory's advice.
	svc.Config.AccessMode = config.ModeDelegated
	if err := jobs.Retry(ctx, id); err != nil {
		t.Fatal(err)
	}
	row, err = jobs.ClaimNext(ctx, "worker", time.Minute)
	if err != nil || row == nil {
		t.Fatalf("claim after retry = %+v, %v", row, err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	out, err = jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != job.StateAwaitingHuman {
		t.Fatalf("state after widening access_mode and retrying = %s, want awaiting_human; the job stayed pinned to its original conservative snapshot", out.State)
	}
	if kinds := openActionKinds(t, jobs, id); !kinds["openurl_handoff"] {
		t.Fatalf("open actions after retry = %v, want an institutional handoff", kinds)
	}
}

// TestTighteningTheDaemonModeRestrainsAlreadySubmittedJobs is the other half of
// the ceiling. Honouring the job's snapshot made the clamp apply only at
// submit, so an operator revoking automation — delegated to conservative —
// would not have stopped jobs already in the queue from opening the very
// handoff tabs they had just revoked. The effective mode is therefore
// re-clamped against the current configuration on every read, which can only
// ever lower it.
func TestTighteningTheDaemonModeRestrainsAlreadySubmittedJobs(t *testing.T) {
	ctx := context.Background()
	svc, jobs := exhaustingService(t, config.ModeDelegated, "https://openurl.example.edu/resolve")

	id, err := svc.Submit(ctx, doiRequest("wr_tighten_after_submit"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(ctx, "worker", time.Minute)
	if err != nil || row == nil {
		t.Fatalf("claim = %+v, %v", row, err)
	}
	if row.Policy.AccessMode != config.ModeDelegated {
		t.Fatalf("policy snapshot = %q, want delegated so tightening has something to restrain", row.Policy.AccessMode)
	}

	// The operator revokes automation after the job was already recorded.
	svc.Config.AccessMode = config.ModeConservative

	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	out, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != job.StateUnavailable {
		t.Fatalf("state = %s, want unavailable; the revoked delegated snapshot still governed", out.State)
	}
	kinds := openActionKinds(t, jobs, id)
	if kinds["openurl_handoff"] {
		t.Fatalf("open actions = %v; a handoff was opened at a mode the operator had revoked", kinds)
	}
	if !kinds["openurl_available"] {
		t.Fatalf("open actions = %v, want the conservative advisory", kinds)
	}
}
