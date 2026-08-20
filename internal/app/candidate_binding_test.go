// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/protocol"
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
