// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/protocol"
	"papio/internal/resolver"
	"papio/internal/work"
)

// vetoTitleOnlyRequest returns a DOI-less request with title/authors/year that
// the veto's regression case uses — a job whose identity is carried only by
// bibliographic metadata, never a DOI.
func vetoTitleOnlyRequest(requestID string) protocol.WorkRequest {
	return protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		RequestID:     requestID,
		Title:         "Core reporting practices in structural equation modeling",
		Authors:       []string{"James B. Schreiber", "Amaury Nora", "Frances K. Stage", "Elizabeth A. Barlow", "Jamie King"},
		Year:          2006,
	}
}

// seedTitleOnlyValidatingCandidate is like seedValidatingCandidate but for a
// DOI-less job whose requesting metadata is title/authors/year only.
func seedTitleOnlyValidatingCandidate(t *testing.T, svc *Service, jobs *job.Store, requestID, urlKey, seed string) (*job.Row, *job.Candidate, []byte, string, string) {
	t.Helper()
	ctx := context.Background()
	created, err := svc.Submit(ctx, vetoTitleOnlyRequest(requestID))
	if err != nil {
		t.Fatal(err)
	}
	id := created
	if _, err := jobs.InsertCandidates(ctx, id, []job.Candidate{{
		JobID: id, Source: "fixture", URLRedacted: "https://example.test/" + urlKey + ".pdf", URLKey: urlKey,
		Version: "published", AccessBasis: "open", ReuseLicense: "unknown",
	}}); err != nil {
		t.Fatal(err)
	}
	candidate, err := jobs.NextPendingCandidate(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]string{
		{job.StateQueued, job.StateResolving},
		{job.StateResolving, job.StateFetching},
		{job.StateFetching, job.StateValidating},
	} {
		if err := jobs.Transition(ctx, id, edge[0], edge[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	qdir, err := svc.Artifacts.QuarantineDir(id)
	if err != nil {
		t.Fatal(err)
	}
	body := pdfBytes(seed)
	tempPath := filepath.Join(qdir, "candidate.tmp")
	if err := os.WriteFile(tempPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	return row, candidate, body, tempPath, sha
}

// foreignDOIFrontMatter builds a front-matter excerpt that prints the job's
// exact title, its authors, a different year, and a foreign DOI at offset 0 so
// FrontMatterDOIs (1 KiB blind window) sees it.
func foreignDOIFrontMatter(foreignDOI string, w work.Work) string {
	var b strings.Builder
	fmt.Fprintf(&b, "DOI: %s\n", foreignDOI)
	b.WriteString(w.Title + "\n")
	b.WriteString(strings.Join(w.Authors, ", ") + "\n")
	// Different year so the year predicate does not mask the DOI check.
	b.WriteString("2024\n")
	// Pad slightly to be realistic but stay in window.
	b.WriteString("Abstract: this is the paper body.\n")
	return b.String()
}

func TestForeignConclusiveDOIParksDOILessJobInsteadOfFiling(t *testing.T) {
	// Regression: a DOI-less job whose work carries Title + Authors + Year and
	// whose document prints that exact title/authors plus a foreign front-matter
	// DOI must park verify_identity, not promote. MatchIdentityWithThreshold's
	// foreign-DOI rejection is gated on wantDOI != "" (identity.go:148-162), so
	// a DOI-less target never contradicts — exact title/authors returns
	// IdentityPass and the bytes get filed under the wrong citation unless the
	// conclusive-identity veto intervenes.
	foreignDOI := "10.9999/foreign.work"
	svc, jobs := newTestService(t)
	row, candidate, _, tempPath, sha := seedTitleOnlyValidatingCandidate(t, svc, jobs, "wr_veto_regression", "veto-regression", "veto-regression")

	target := work.Work{Title: row.Work.Title, Authors: row.Work.Authors, Year: row.Work.Year}
	excerpt := foreignDOIFrontMatter(foreignDOI, target)

	svc.Validate = func(_ context.Context, _, _ string, _ work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 8},
			Text:       pdf.TextReport{Chars: int64(len(excerpt)), Excerpt: excerpt},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass, Evidence: []string{"exact title + authors"}},
		}, nil
	}

	accepted, parked, err := svc.validateCandidate(context.Background(), row, candidate, fetch.Result{TempPath: tempPath, SHA256: sha, SizeBytes: 2048, SniffedMIME: "application/pdf", ContentType: "application/pdf"})
	if err != nil {
		t.Fatalf("validateCandidate: %v", err)
	}
	if accepted || !parked {
		t.Fatalf("accepted=%v parked=%v, want !accepted && parked", accepted, parked)
	}
	got, _ := jobs.Get(context.Background(), row.ID)
	if got.State != job.StateNeedsReview {
		t.Fatalf("job state = %s, want %s", got.State, job.StateNeedsReview)
	}
	if got.ArtifactSHA256 != "" {
		t.Fatalf("artifactSHA256 = %q, want no artifact promoted", got.ArtifactSHA256)
	}
	if art, _ := jobs.GetArtifact(context.Background(), sha); art != nil {
		t.Fatalf("artifact was promoted despite foreign conclusive DOI: %+v", art)
	}
	actions, err := jobs.ListHumanActions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	var found *job.HumanAction
	for i := range actions {
		if actions[i].JobID == row.ID && actions[i].Kind == "verify_identity" {
			found = &actions[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no open verify_identity action after foreign conclusive DOI veto")
	}
	if !strings.Contains(found.Detail, foreignDOI) {
		t.Fatalf("action detail = %q, want it to name the conclusive DOI %q", found.Detail, foreignDOI)
	}
}

func conclusiveQueueCandidate(url string, confidence float64) resolver.Candidate {
	return resolver.Candidate{
		Source: "fixture", URL: url, Version: resolver.VersionPublished,
		AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown", Direct: true,
		IdentityConfidence: confidence,
	}
}

func TestMultipleFrontMatterDOIsExplainAmbiguityWithoutPromoting(t *testing.T) {
	svc, jobs := newTestService(t)
	row, candidate, _, tempPath, sha := seedTitleOnlyValidatingCandidate(t, svc, jobs, "wr_veto_multiple", "veto-multiple", "veto-multiple")
	excerpt := "Book DOI: 10.9999/book\nChapter DOI: 10.9999/chapter\n" + row.Work.Title
	svc.Validate = func(_ context.Context, _, _ string, _ work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload: pdf.PayloadReport{OK: true}, Structural: pdf.StructuralReport{Valid: true, Pages: 36},
			Text:     pdf.TextReport{Chars: int64(len(excerpt)), Excerpt: excerpt},
			Identity: pdf.IdentityDecision{Result: pdf.IdentityPass},
		}, nil
	}
	accepted, parked, err := svc.validateCandidate(context.Background(), row, candidate, fetch.Result{
		TempPath: tempPath, SHA256: sha, SizeBytes: 2048, SniffedMIME: "application/pdf", ContentType: "application/pdf",
	})
	if err != nil || accepted || !parked {
		t.Fatalf("accepted=%v parked=%v err=%v, want identity review", accepted, parked, err)
	}
	actions, err := jobs.ListHumanActions(context.Background(), true)
	if err != nil || len(actions) != 1 {
		t.Fatalf("actions=%+v err=%v, want one identity review", actions, err)
	}
	action := actions[0]
	if action.Kind != "verify_identity" || action.QuarantineSHA256 != sha || action.QuarantinePath != tempPath {
		t.Fatalf("review lost its exact quarantined file binding: %+v", action)
	}
	if !strings.Contains(action.Detail, "ambiguous") || strings.Contains(action.Detail, "does not match") {
		t.Fatalf("ambiguous evidence reported as a definite mismatch: %s", action.Detail)
	}
}

func TestProcessTriesCorrectCandidateAfterConclusiveMismatch(t *testing.T) {
	const (
		wrongURL   = "https://example.test/conclusive-wrong.pdf"
		correctURL = "https://example.test/conclusive-correct.pdf"
		foreignDOI = "10.9999/wrong-first"
	)
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Resolvers = []ResolverEntry{{
		Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{
			conclusiveQueueCandidate(wrongURL, 1),
			conclusiveQueueCandidate(correctURL, 0.8),
		}},
		Policy: svc.Config.Sources["fixture"],
	}}

	fetchedURLs := make([]string, 0, 2)
	fetchedByPath := make(map[string]string)
	fetchedPaths := make(map[string]string)
	fetchedDigests := make(map[string]string)
	svc.Fetch = func(_ context.Context, candidate resolver.Candidate, path string) (fetch.Result, error) {
		if len(fetchedURLs) == 1 {
			actions, err := jobs.ListHumanActions(ctx, true)
			if err != nil {
				t.Fatal(err)
			}
			for _, action := range actions {
				if action.Kind == "verify_identity" {
					t.Fatalf("temporary identity review opened before the second fetch: %+v", action)
				}
			}
		}
		body := pdfBytes(candidate.URL)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return fetch.Result{}, err
		}
		sum := sha256.Sum256(body)
		digest := hex.EncodeToString(sum[:])
		fetchedURLs = append(fetchedURLs, candidate.URL)
		fetchedByPath[path] = candidate.URL
		fetchedPaths[candidate.URL] = path
		fetchedDigests[candidate.URL] = digest
		return fetch.Result{
			TempPath: path, SHA256: digest, SizeBytes: int64(len(body)),
			SniffedMIME: "application/pdf", ContentType: "application/pdf", HTTPStatus: 200,
		}, nil
	}
	svc.Validate = func(_ context.Context, path, _ string, target work.Work) (pdf.ValidationReport, error) {
		excerpt := target.Title + "\n" + strings.Join(target.Authors, ", ")
		if fetchedByPath[path] == wrongURL {
			excerpt = foreignDOIFrontMatter(foreignDOI, target)
		}
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 8},
			Text:       pdf.TextReport{Chars: 2000, Excerpt: excerpt},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
		}, nil
	}

	id, err := svc.Submit(ctx, vetoTitleOnlyRequest("wr_conclusive_queue_success"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(ctx, "worker", time.Minute)
	if err != nil || row == nil || row.ID != id {
		t.Fatalf("claim = %+v, %v", row, err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatalf("Process: %v", err)
	}

	if strings.Join(fetchedURLs, ",") != strings.Join([]string{wrongURL, correctURL}, ",") {
		t.Fatalf("fetched URLs = %v, want wrong then correct", fetchedURLs)
	}
	ready, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if ready.State != job.StateReady || ready.ArtifactSHA256 != fetchedDigests[correctURL] {
		t.Fatalf("ready job = %+v, want correct digest %s", ready, fetchedDigests[correctURL])
	}
	if artifact, err := jobs.GetArtifact(ctx, fetchedDigests[wrongURL]); err != nil || artifact != nil {
		t.Fatalf("wrong bytes published as artifact = %+v, %v", artifact, err)
	}
	if _, err := os.Stat(fetchedPaths[wrongURL]); !os.IsNotExist(err) {
		t.Fatalf("deferred wrong file remains at %s: %v", fetchedPaths[wrongURL], err)
	}
	actions, err := jobs.ListHumanActions(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.JobID == id && action.Kind == "verify_identity" {
			t.Fatalf("identity review was opened despite the correct fallback: %+v", action)
		}
	}
}

func TestConclusiveMismatchExhaustionRetainsFirstBoundReview(t *testing.T) {
	const (
		firstURL  = "https://example.test/conclusive-first.pdf"
		secondURL = "https://example.test/conclusive-second.pdf"
	)
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Resolvers = []ResolverEntry{{
		Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{
			conclusiveQueueCandidate(firstURL, 1),
			conclusiveQueueCandidate(secondURL, 0.8),
		}},
		Policy: svc.Config.Sources["fixture"],
	}}

	fetchedURLs := make([]string, 0, 2)
	fetchedByPath := make(map[string]string)
	fetchedPaths := make(map[string]string)
	fetchedDigests := make(map[string]string)
	svc.Fetch = func(_ context.Context, candidate resolver.Candidate, path string) (fetch.Result, error) {
		body := pdfBytes(candidate.URL)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return fetch.Result{}, err
		}
		sum := sha256.Sum256(body)
		digest := hex.EncodeToString(sum[:])
		fetchedURLs = append(fetchedURLs, candidate.URL)
		fetchedByPath[path] = candidate.URL
		fetchedPaths[candidate.URL] = path
		fetchedDigests[candidate.URL] = digest
		return fetch.Result{
			TempPath: path, SHA256: digest, SizeBytes: int64(len(body)),
			SniffedMIME: "application/pdf", ContentType: "application/pdf", HTTPStatus: 200,
		}, nil
	}
	svc.Validate = func(_ context.Context, path, _ string, target work.Work) (pdf.ValidationReport, error) {
		foreignDOI := "10.9999/first-mismatch"
		if fetchedByPath[path] == secondURL {
			foreignDOI = "10.9999/second-mismatch"
		}
		excerpt := foreignDOIFrontMatter(foreignDOI, target)
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 8},
			Text:       pdf.TextReport{Chars: 2000, Excerpt: excerpt},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
		}, nil
	}

	id, err := svc.Submit(ctx, vetoTitleOnlyRequest("wr_conclusive_queue_exhausted"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(ctx, "worker", time.Minute)
	if err != nil || row == nil {
		t.Fatalf("claim = %+v, %v", row, err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatalf("Process: %v", err)
	}

	if strings.Join(fetchedURLs, ",") != strings.Join([]string{firstURL, secondURL}, ",") {
		t.Fatalf("fetched URLs = %v, want both mismatch alternatives", fetchedURLs)
	}
	parked, err := jobs.Get(ctx, id)
	if err != nil || parked.State != job.StateNeedsReview || parked.ArtifactSHA256 != "" {
		t.Fatalf("parked job = %+v, %v", parked, err)
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].JobID != id || actions[0].Kind != "verify_identity" {
		t.Fatalf("open actions = %+v, want one identity review", actions)
	}
	review := actions[0]
	if review.QuarantinePath != fetchedPaths[firstURL] ||
		review.QuarantineSHA256 != fetchedDigests[firstURL] ||
		review.CandidateID <= 0 ||
		!strings.Contains(review.Detail, "10.9999/first-mismatch") {
		t.Fatalf("retained review = %+v, want first candidate path, digest and veto evidence", review)
	}
	actual, err := fileSHA256(review.QuarantinePath)
	if err != nil || actual != review.QuarantineSHA256 {
		t.Fatalf("retained review bytes digest = %q, %v; want %q", actual, err, review.QuarantineSHA256)
	}
	if _, err := os.Stat(fetchedPaths[secondURL]); !os.IsNotExist(err) {
		t.Fatalf("non-retained mismatch file remains at %s: %v", fetchedPaths[secondURL], err)
	}
	for _, digest := range fetchedDigests {
		if artifact, err := jobs.GetArtifact(ctx, digest); err != nil || artifact != nil {
			t.Fatalf("mismatched bytes published as artifact = %+v, %v", artifact, err)
		}
	}
}

func TestDeferredConclusiveMismatchParksSafely(t *testing.T) {
	for _, test := range []struct {
		name             string
		mutate           func(string) error
		actionKind       string
		bound            bool
		cancelFetch      bool
		cancelValidation bool
	}{
		{name: "intact", actionKind: "verify_identity", bound: true, cancelFetch: true},
		{name: "missing", mutate: os.Remove, actionKind: "validation_error", cancelFetch: true},
		{name: "digest_changed", mutate: func(path string) error {
			return os.WriteFile(path, pdfBytes("changed-after-validation"), 0o600)
		}, actionKind: "validation_error", cancelFetch: true},
		{name: "missing_without_cancellation", mutate: os.Remove, actionKind: "validation_error"},
		{name: "cancelled_after_validation", actionKind: "verify_identity", bound: true, cancelValidation: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			const (
				wrongURL = "https://example.test/conclusive-retained.pdf"
				nextURL  = "https://example.test/cancelled-next.pdf"
			)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			svc, jobs := newTestService(t)
			svc.Resolvers = []ResolverEntry{{
				Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{
					conclusiveQueueCandidate(wrongURL, 1),
					conclusiveQueueCandidate(nextURL, 0.8),
				}},
				Policy: svc.Config.Sources["fixture"],
			}}

			var retainedPath, retainedDigest string
			svc.Fetch = func(fetchCtx context.Context, candidate resolver.Candidate, path string) (fetch.Result, error) {
				if candidate.URL == nextURL {
					if test.mutate != nil {
						if err := test.mutate(retainedPath); err != nil {
							t.Fatal(err)
						}
					}
					if test.cancelFetch {
						cancel()
						return fetch.Result{}, fetchCtx.Err()
					}
					return fetch.Result{}, errors.New("candidate unavailable")
				}
				body := pdfBytes(candidate.URL)
				if err := os.WriteFile(path, body, 0o600); err != nil {
					return fetch.Result{}, err
				}
				sum := sha256.Sum256(body)
				retainedPath = path
				retainedDigest = hex.EncodeToString(sum[:])
				return fetch.Result{
					TempPath: path, SHA256: retainedDigest, SizeBytes: int64(len(body)),
					SniffedMIME: "application/pdf", ContentType: "application/pdf", HTTPStatus: 200,
				}, nil
			}
			svc.Validate = func(_ context.Context, _ string, _ string, target work.Work) (pdf.ValidationReport, error) {
				excerpt := foreignDOIFrontMatter("10.9999/cancelled-fallback", target)
				if test.cancelValidation {
					cancel()
				}
				return pdf.ValidationReport{
					Payload:    pdf.PayloadReport{OK: true},
					Structural: pdf.StructuralReport{Valid: true, Pages: 8},
					Text:       pdf.TextReport{Chars: 2000, Excerpt: excerpt},
					Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
				}, nil
			}

			id, err := svc.Submit(ctx, vetoTitleOnlyRequest("wr_conclusive_evidence_"+test.name))
			if err != nil {
				t.Fatal(err)
			}
			row, err := jobs.ClaimNext(ctx, "worker", time.Minute)
			if err != nil || row == nil {
				t.Fatalf("claim = %+v, %v", row, err)
			}
			err = svc.Process(ctx, row)
			if test.cancelFetch || test.cancelValidation {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Process error = %v, want context cancellation", err)
				}
			} else if err != nil {
				t.Fatalf("a parked validation failure must not stop the scheduler: %v", err)
			}

			parked, getErr := jobs.Get(context.Background(), id)
			if getErr != nil || parked.State != job.StateNeedsReview || parked.ArtifactSHA256 != "" {
				t.Fatalf("parked job = %+v, %v", parked, getErr)
			}
			actions, actionErr := jobs.ListHumanActions(context.Background(), true)
			if actionErr != nil {
				t.Fatal(actionErr)
			}
			if len(actions) != 1 || actions[0].JobID != id || actions[0].Kind != test.actionKind {
				t.Fatalf("open actions = %+v, want one %s action", actions, test.actionKind)
			}
			if test.bound {
				if actions[0].CandidateID <= 0 ||
					actions[0].QuarantinePath != retainedPath ||
					actions[0].QuarantineSHA256 != retainedDigest {
					t.Fatalf("bound cancellation review = %+v", actions[0])
				}
			} else if actions[0].CandidateID != 0 ||
				!strings.Contains(actions[0].Detail, retainedPath) ||
				!strings.Contains(actions[0].Detail, retainedDigest) ||
				!strings.Contains(actions[0].Detail, "10.9999/cancelled-fallback") {
				t.Fatalf("safe evidence failure action = %+v", actions[0])
			}
			if artifact, artifactErr := jobs.GetArtifact(context.Background(), retainedDigest); artifactErr != nil || artifact != nil {
				t.Fatalf("changed or missing mismatch bytes published = %+v, %v", artifact, artifactErr)
			}
			var unfinished int
			if err := jobs.S.DB().QueryRowContext(context.Background(),
				`SELECT count(*) FROM attempts WHERE job_id = ? AND (ended_at IS NULL OR outcome IS NULL)`, id,
			).Scan(&unfinished); err != nil {
				t.Fatal(err)
			}
			if unfinished != 0 {
				t.Fatalf("parked job has %d unfinished attempts", unfinished)
			}
		})
	}
}

func TestDeferredIdentityReviewRollsBackActionWhenParkFails(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	row, candidate, _, path, digest := seedTitleOnlyValidatingCandidate(
		t, svc, jobs, "wr_deferred_park_failure", "deferred-park-failure", "retained-evidence")
	if _, err := jobs.S.DB().ExecContext(ctx, `
		CREATE TRIGGER fail_review_park BEFORE UPDATE OF state ON jobs
		WHEN NEW.state = 'needs_review'
		BEGIN SELECT RAISE(ABORT, 'review park failed'); END;
	`); err != nil {
		t.Fatal(err)
	}
	review := &conclusiveIdentityReview{
		detail: "conclusive DOI mismatch",
		binding: job.HumanActionBinding{
			CandidateID: candidate.ID, QuarantinePath: path, QuarantineSHA256: digest,
		},
	}
	if err := svc.materializeDeferredIdentityReview(ctx, row.ID, review); err == nil {
		t.Fatal("materializing review succeeded despite failed park")
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil || len(actions) != 0 {
		t.Fatalf("failed park left open actions: %+v, %v", actions, err)
	}
	current, err := jobs.Get(ctx, row.ID)
	if err != nil || current.State != job.StateValidating {
		t.Fatalf("failed park changed job: %+v, %v", current, err)
	}
	actual, err := fileSHA256(path)
	if err != nil || actual != digest {
		t.Fatalf("failed park lost quarantine evidence: %s, %v", actual, err)
	}
}

func TestDeferredReviewSurvivesLaterCandidateParkFailure(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	const firstURL = "https://example.test/first-mismatch.pdf"
	const nextURL = "https://example.test/next-unsafe.pdf"
	svc.Resolvers = []ResolverEntry{{
		Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{
			conclusiveQueueCandidate(firstURL, 1),
			conclusiveQueueCandidate(nextURL, 0.8),
		}},
		Policy: svc.Config.Sources["fixture"],
	}}
	var firstPath, firstDigest string
	svc.Fetch = func(_ context.Context, candidate resolver.Candidate, path string) (fetch.Result, error) {
		body := pdfBytes(candidate.URL)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return fetch.Result{}, err
		}
		sum := sha256.Sum256(body)
		digest := hex.EncodeToString(sum[:])
		if candidate.URL == firstURL {
			firstPath, firstDigest = path, digest
		}
		return fetch.Result{
			TempPath: path, SHA256: digest, SizeBytes: int64(len(body)),
			SniffedMIME: "application/pdf", ContentType: "application/pdf", HTTPStatus: 200,
		}, nil
	}
	svc.Validate = func(_ context.Context, path, _ string, target work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 8, HasJavaScript: path != firstPath},
			Text:       pdf.TextReport{Chars: 2000, Excerpt: foreignDOIFrontMatter("10.9999/fallback", target)},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
		}, nil
	}
	if _, err := jobs.S.DB().ExecContext(ctx, `
		CREATE TRIGGER fail_unsafe_park BEFORE UPDATE OF state ON jobs
		WHEN NEW.state = 'needs_review' AND EXISTS (
			SELECT 1 FROM human_actions WHERE job_id = NEW.id AND kind = 'unsafe_pdf' AND status = 'open'
		)
		BEGIN SELECT RAISE(ABORT, 'unsafe park failed'); END;
	`); err != nil {
		t.Fatal(err)
	}
	id, err := svc.Submit(ctx, vetoTitleOnlyRequest("wr_later_candidate_park_failure"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(ctx, "worker", time.Minute)
	if err != nil || row == nil {
		t.Fatalf("claim = %+v, %v", row, err)
	}
	if err := svc.Process(ctx, row); err == nil {
		t.Fatal("Process succeeded despite injected store failure")
	}
	current, err := jobs.Get(ctx, id)
	if err != nil || current.State != job.StateNeedsReview || current.ArtifactSHA256 != "" {
		t.Fatalf("fallback did not park safely: %+v, %v", current, err)
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil || len(actions) != 1 || actions[0].Kind != "verify_identity" ||
		actions[0].QuarantinePath != firstPath || actions[0].QuarantineSHA256 != firstDigest {
		t.Fatalf("failed later park left conflicting review actions: %+v, %v", actions, err)
	}
}

func TestDeferredConclusiveMismatchIsRefetchableAfterRecovery(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	row, candidate, body, path, digest := seedTitleOnlyValidatingCandidate(
		t, svc, jobs, "wr_conclusive_recovery", "conclusive-recovery", "conclusive-recovery")
	if err := jobs.MarkCandidate(ctx, candidate.ID, "fetching"); err != nil {
		t.Fatal(err)
	}
	svc.Validate = func(_ context.Context, _ string, _ string, target work.Work) (pdf.ValidationReport, error) {
		excerpt := foreignDOIFrontMatter("10.9999/recoverable-mismatch", target)
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 8},
			Text:       pdf.TextReport{Chars: 2000, Excerpt: excerpt},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
		}, nil
	}

	accepted, parked, deferred, err := svc.validateFetchCandidate(ctx, row, candidate, fetch.Result{
		TempPath: path, SHA256: digest, SizeBytes: int64(len(body)),
		SniffedMIME: "application/pdf", ContentType: "application/pdf",
	})
	if err != nil {
		t.Fatalf("validateFetchCandidate: %v", err)
	}
	if accepted || parked || deferred == nil {
		t.Fatalf("deferred result = accepted %t, parked %t, review %+v", accepted, parked, deferred)
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil || len(actions) != 0 {
		t.Fatalf("actions before recovery = %+v, %v; want none", actions, err)
	}

	recovered, err := jobs.RecoverStale(ctx)
	if err != nil || len(recovered) != 1 || recovered[0] != row.ID {
		t.Fatalf("RecoverStale = %v, %v", recovered, err)
	}
	if err := svc.Artifacts.CleanQuarantine(row.ID); err != nil {
		t.Fatal(err)
	}
	if err := jobs.ResetCandidates(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	retry, err := jobs.NextPendingCandidate(ctx, row.ID)
	if err != nil || retry == nil || retry.ID != candidate.ID {
		t.Fatalf("candidate after recovery = %+v, %v; want deferred candidate %d", retry, err, candidate.ID)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale quarantine file remains after recovery cleanup: %v", err)
	}
}

func TestForeignConclusiveDOIBoundToJobPromotesNormally(t *testing.T) {
	// Control: same excerpt DOI, but durably bound to the job — the veto is
	// compatible and the bytes promote to ready as normal.
	foreignDOI := "10.9999/bound.work"
	svc, jobs := newTestService(t)
	wr := vetoTitleOnlyRequest("wr_veto_bound")
	wr.Identifiers = &protocol.Identifiers{DOI: foreignDOI}
	created, err := svc.Submit(context.Background(), wr)
	if err != nil {
		t.Fatal(err)
	}
	id := created
	if _, err := jobs.InsertCandidates(context.Background(), id, []job.Candidate{{
		JobID: id, Source: "fixture", URLRedacted: "https://example.test/bound.pdf", URLKey: "bound",
		Version: "published", AccessBasis: "open", ReuseLicense: "unknown",
	}}); err != nil {
		t.Fatal(err)
	}
	candidate, _ := jobs.NextPendingCandidate(context.Background(), id)
	for _, edge := range [][2]string{
		{job.StateQueued, job.StateResolving},
		{job.StateResolving, job.StateFetching},
		{job.StateFetching, job.StateValidating},
	} {
		if err := jobs.Transition(context.Background(), id, edge[0], edge[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	row, _ := jobs.Get(context.Background(), id)
	qdir, _ := svc.Artifacts.QuarantineDir(id)
	body := pdfBytes("bound-promotes")
	tempPath := filepath.Join(qdir, "candidate.tmp")
	if err := os.WriteFile(tempPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])

	target := work.Work{Title: row.Work.Title, Authors: row.Work.Authors, Year: row.Work.Year}
	excerpt := foreignDOIFrontMatter(foreignDOI, target)

	svc.Validate = func(_ context.Context, _, _ string, _ work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 8},
			Text:       pdf.TextReport{Chars: int64(len(excerpt)), Excerpt: excerpt},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
		}, nil
	}

	accepted, parked, err := svc.validateCandidate(context.Background(), row, candidate, fetch.Result{TempPath: tempPath, SHA256: sha, SizeBytes: int64(len(body)), SniffedMIME: "application/pdf", ContentType: "application/pdf"})
	if err != nil {
		t.Fatalf("validateCandidate: %v", err)
	}
	if !accepted || parked {
		t.Fatalf("accepted=%v parked=%v, want accepted && !parked", accepted, parked)
	}
	got, _ := jobs.Get(context.Background(), id)
	if got.State != job.StateReady {
		t.Fatalf("job state = %s, want %s", got.State, job.StateReady)
	}
}

func TestNoConclusiveDOIPromotesNormally(t *testing.T) {
	// Control: excerpt front matter with no DOI at all and IdentityPass
	// promotes normally — the veto does not park every DOI-less document.
	svc, jobs := newTestService(t)
	row, candidate, _, tempPath, sha := seedTitleOnlyValidatingCandidate(t, svc, jobs, "wr_veto_no_doi", "no-doi", "no-doi-promotes")

	target := work.Work{Title: row.Work.Title, Authors: row.Work.Authors, Year: row.Work.Year}
	excerpt := target.Title + "\n" + strings.Join(target.Authors, ", ") + "\nAbstract: body.\n"

	svc.Validate = func(_ context.Context, _, _ string, _ work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 8},
			Text:       pdf.TextReport{Chars: int64(len(excerpt)), Excerpt: excerpt},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
		}, nil
	}

	accepted, parked, err := svc.validateCandidate(context.Background(), row, candidate, fetch.Result{TempPath: tempPath, SHA256: sha, SizeBytes: 2048, SniffedMIME: "application/pdf", ContentType: "application/pdf"})
	if err != nil {
		t.Fatalf("validateCandidate: %v", err)
	}
	if !accepted || parked {
		t.Fatalf("accepted=%v parked=%v, want accepted && !parked", accepted, parked)
	}
	got, _ := jobs.Get(context.Background(), row.ID)
	if got.State != job.StateReady {
		t.Fatalf("job state = %s, want %s", got.State, job.StateReady)
	}
}

func TestReviewOverridePromotesDespiteForeignConclusiveDOI(t *testing.T) {
	foreignDOI := "10.9999/review-override"
	svc, jobs := newTestService(t)
	row, candidate, _, tempPath, sha := seedTitleOnlyValidatingCandidate(t, svc, jobs, "wr_veto_override", "veto-override", "veto-override")

	// Mark the candidate as human-reviewed so the veto's ReviewOverride
	// escape preserves human review authority. There is no exported setter —
	// use the store column directly.
	if _, err := jobs.S.DB().ExecContext(context.Background(), `UPDATE candidates SET review_override = 1 WHERE id = ?`, candidate.ID); err != nil {
		t.Fatal(err)
	}
	candidate, _ = jobs.GetCandidate(context.Background(), candidate.ID)
	row, _ = jobs.Get(context.Background(), row.ID)

	target := work.Work{Title: row.Work.Title, Authors: row.Work.Authors, Year: row.Work.Year}
	excerpt := foreignDOIFrontMatter(foreignDOI, target)

	svc.Validate = func(_ context.Context, _, _ string, _ work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 8},
			Text:       pdf.TextReport{Chars: int64(len(excerpt)), Excerpt: excerpt},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
		}, nil
	}

	accepted, parked, err := svc.validateCandidate(context.Background(), row, candidate, fetch.Result{TempPath: tempPath, SHA256: sha, SizeBytes: 2048, SniffedMIME: "application/pdf", ContentType: "application/pdf"})
	if err != nil {
		t.Fatalf("validateCandidate: %v", err)
	}
	if !accepted || parked {
		t.Fatalf("accepted=%v parked=%v, want accepted && !parked despite veto", accepted, parked)
	}
	got, _ := jobs.Get(context.Background(), row.ID)
	if got.State != job.StateReady {
		t.Fatalf("job state = %s, want %s (override)", got.State, job.StateReady)
	}
}

func TestBoundDOIsCollectsEligibleDOIs(t *testing.T) {
	anchor := job.SubmittedIdentity{
		Attested: true,
		Work:     work.Work{DOI: "10.1000/anchor"},
		Identifiers: []job.Identifier{
			{Kind: "doi", Value: "10.1000/submitted", Provenance: job.ProvenanceSubmitted},
			{Kind: "doi", Value: "10.1000/verified", Provenance: job.ProvenanceVerified},
			{Kind: "doi", Value: "10.1000/adopted", Provenance: job.ProvenanceAdopted},
			{Kind: "pmid", Value: "12345", Provenance: job.ProvenanceSubmitted},
			{Kind: "doi", Value: "", Provenance: job.ProvenanceSubmitted},
			{Kind: "doi", Value: "10.1000/submitted", Provenance: job.ProvenanceSubmitted},
			{Kind: "doi", Value: "10.1000/unattested", Provenance: job.ProvenanceUnattested},
		},
	}
	got := job.BoundDOIs(anchor, work.Work{DOI: "10.1000/row"})
	want := map[string]bool{
		"10.1000/anchor":    true,
		"10.1000/submitted": true,
		"10.1000/verified":  true,
		"10.1000/row":       true,
	}
	if len(got) != len(want) {
		t.Fatalf("BoundDOIs = %v, want %v", got, want)
	}
	for _, v := range got {
		if !want[v] {
			t.Fatalf("unexpected bound DOI %q in %v", v, got)
		}
	}
	for w := range want {
		found := false
		for _, v := range got {
			if v == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing bound DOI %q in %v", w, got)
		}
	}
	for _, v := range got {
		if v == "10.1000/adopted" || v == "10.1000/unattested" || v == "12345" || v == "" {
			t.Fatalf("excluded DOI %q appeared in bound set %v", v, got)
		}
	}
}

func TestBoundDOIsWithNilRow(t *testing.T) {
	anchor := job.SubmittedIdentity{
		Attested: true,
		Work:     work.Work{DOI: "10.1000/a"},
		Identifiers: []job.Identifier{
			{Kind: "doi", Value: "10.1000/b", Provenance: job.ProvenanceSubmitted},
		},
	}
	got := job.BoundDOIs(anchor, work.Work{})
	if len(got) != 2 {
		t.Fatalf("BoundDOIs with empty work = %v, want 2", got)
	}
}
