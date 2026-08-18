// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/grab"
	"papio/internal/pdf"
	"papio/internal/work"
)

// parkedGrabWithPool settles one DOI-less grab against the pool the caller has
// already seeded and returns its id, asserting it really parked.
//
// It relies on the production ambiguity rule rather than switching the decision
// off: with a qualifier AND a review in the pool, SelectAutoBindCandidate
// abstains ("qualifier alongside review") and the grab parks. That is the shape
// this whole surface exists for — two pending papers that look alike, which is
// exactly when a human is needed — so building it out of the real rule rather
// than out of the test flag keeps the fixture honest.
func parkedGrabWithPool(t *testing.T, b *Bridge, adoptionRoot, excerpt, title string) string {
	t.Helper()
	ctx := context.Background()
	b.svc.Validate = validateForExcerpt(excerpt)
	g, err := b.grabs.Allocate(ctx, "suggest.example.org", title)
	if err != nil {
		t.Fatal(err)
	}
	writeFixturePDF(t, filepath.Join(adoptionRoot, "grabs", g.ID, "main.pdf"))
	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, err := b.grabs.Get(ctx, g.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v %v", got, err)
	}
	if got.State != grab.StateParkedNoIdentifier {
		t.Fatalf("state = %q, want parked_no_identifier (autonomous binding should have abstained on an ambiguous pool)", got.State)
	}
	return g.ID
}

// versionPair is the ambiguity the ranked picker is for: the same paper pending
// twice under two identifiers, a preprint and a version of record. Title,
// authors and year agree for both, so only the printed identifier separates
// them — one qualifies, the other can only be offered for review.
func versionPair() (vor work.Work, preprint work.Work) {
	vor = work.Work{
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2026,
		DOI:     "10.1234/vor.0001",
	}
	preprint = vor
	preprint.DOI = "10.5555/preprint.0001"
	return vor, preprint
}

func TestSuggestRanksQualifyingCandidateAboveReview(t *testing.T) {
	ctx := context.Background()
	b, jobs, cfg, _ := newBridge(t)
	vor, preprint := versionPair()
	vorJob := parkManualDownload(t, jobs, "wr_sug_vor", vor)
	preprintJob := parkManualDownload(t, jobs, "wr_sug_pre", preprint)
	// A third, unrelated pending paper must be offered last: a picker that
	// buries the answer under noise is the thing being replaced.
	otherJob := parkManualDownload(t, jobs, "wr_sug_other", work.Work{
		Title: "Attention Mechanisms for Sequence Transduction Networks", Authors: []string{"David Chen"}, Year: 2019, DOI: "10.9999/other.1",
	})

	grabID := parkedGrabWithPool(t, b, cfg.EffectiveAdoptionRoot(), exoticExcerpt(vor), "Ranked Suggest")

	res := b.SuggestGrabCandidates(ctx, grabID, 10)
	if res.Outcome != "ok" {
		t.Fatalf("outcome = %q detail = %q, want ok", res.Outcome, res.Detail)
	}
	if len(res.Suggestions) != 3 {
		t.Fatalf("suggestions = %d, want all three pending jobs scored: %+v", len(res.Suggestions), res.Suggestions)
	}
	if res.Truncated {
		t.Fatal("truncated = true, want false: three candidates fit a limit of ten")
	}
	if res.Suggestions[0].JobID != vorJob || res.Suggestions[0].Verdict != "qualifies" {
		t.Fatalf("rank 1 = %+v, want the version of record qualifying (%s)", res.Suggestions[0], vorJob)
	}
	if res.Suggestions[1].JobID != preprintJob || res.Suggestions[1].Verdict != "review" {
		t.Fatalf("rank 2 = %+v, want the preprint for review (%s)", res.Suggestions[1], preprintJob)
	}
	if res.Suggestions[2].JobID != otherJob || res.Suggestions[2].Verdict != "rejected" {
		t.Fatalf("rank 3 = %+v, want the unrelated paper rejected (%s)", res.Suggestions[2], otherJob)
	}
	// Evidence is the reason a human can trust the order, so it must survive
	// the trip rather than being reduced to a verdict word.
	if len(res.Suggestions[0].Evidence) == 0 {
		t.Fatal("rank 1 carries no evidence; the ranking would be unauditable")
	}
	if joined := strings.Join(res.Suggestions[0].Evidence, "; "); !strings.Contains(joined, "title") {
		t.Fatalf("evidence = %q, want the printed-title match named", joined)
	}
	if res.Suggestions[2].Reason == "" {
		t.Fatal("rejected candidate carries no reason; a human cannot tell why it lost")
	}
	if res.Suggestions[0].Title != vor.Title || res.Suggestions[0].Year != vor.Year {
		t.Fatalf("rank 1 bibliography = %+v, want the job's own title and year for display", res.Suggestions[0])
	}

	// Read-only by construction: scoring must not move the grab.
	after, _ := b.grabs.Get(ctx, grabID)
	if after.State != grab.StateParkedNoIdentifier || after.JobID != "" {
		t.Fatalf("grab = %+v, want still parked and unbound after a suggestion read", after)
	}
}

func TestSuggestBoundsAndGuards(t *testing.T) {
	ctx := context.Background()
	b, jobs, cfg, _ := newBridge(t)
	vor, preprint := versionPair()
	parkManualDownload(t, jobs, "wr_bound_vor", vor)
	parkManualDownload(t, jobs, "wr_bound_pre", preprint)
	grabID := parkedGrabWithPool(t, b, cfg.EffectiveAdoptionRoot(), exoticExcerpt(vor), "Bounded Suggest")

	limited := b.SuggestGrabCandidates(ctx, grabID, 1)
	if limited.Outcome != "ok" || len(limited.Suggestions) != 1 {
		t.Fatalf("limited = %+v, want exactly one row", limited)
	}
	if !limited.Truncated {
		t.Fatal("truncated = false with a pool larger than the limit; the UI would imply it saw everything")
	}
	if limited.Suggestions[0].Verdict != "qualifies" {
		t.Fatalf("kept row = %+v, want the best candidate kept when truncating, not an arbitrary one", limited.Suggestions[0])
	}

	if got := b.SuggestGrabCandidates(ctx, "grab_does_not_exist", 5); got.Outcome != "unknown_grab" {
		t.Fatalf("unknown grab outcome = %q, want unknown_grab", got.Outcome)
	}

	// A grab that is not parked has no question to answer, and answering one
	// anyway would offer to re-file bytes that already belong to a job.
	other, err := b.grabs.Allocate(ctx, "suggest.example.org", "Not Parked")
	if err != nil {
		t.Fatal(err)
	}
	if got := b.SuggestGrabCandidates(ctx, other.ID, 5); got.Outcome != "wrong_state" {
		t.Fatalf("awaiting_file outcome = %q, want wrong_state", got.Outcome)
	}
}

// TestSuggestSurfacesDocumentIdentifiers covers the case that motivated the
// document_identifiers field: papio read an identifier out of the file's own
// embedded metadata, no pending job matched it, and the human was nevertheless
// asked to type it in. Showing it is display, not acceptance — nothing here is
// compared against a candidate — so the value appears even though no suggestion
// qualifies.
func TestSuggestSurfacesDocumentIdentifiers(t *testing.T) {
	ctx := context.Background()
	b, jobs, cfg, _ := newBridge(t)
	parkManualDownload(t, jobs, "wr_docid", work.Work{
		Title: "Unrelated Pending Paper", Authors: []string{"Grace Hopper"}, Year: 2025, DOI: "10.9999/unrelated.1",
	})
	excerpt := doiLessExcerpt(t, "Metadata Only Document", "Wilhelmina Farnsworth (2024)")
	b.svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 1},
			Text:       pdf.TextReport{Chars: int64(len(excerpt)), Excerpt: excerpt},
			Metadata:   pdf.MetadataFields{{Field: "xmp/prism:doi", Value: "https://doi.org/10.4321/metadata.only"}},
		}, nil
	}
	g, err := b.grabs.Allocate(ctx, "suggest.example.org", "Metadata Identifier")
	if err != nil {
		t.Fatal(err)
	}
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", g.ID, "main.pdf"))
	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	res := b.SuggestGrabCandidates(ctx, g.ID, 5)
	if res.Outcome != "ok" {
		t.Fatalf("outcome = %q detail = %q, want ok", res.Outcome, res.Detail)
	}
	if len(res.DocumentIdentifiers) != 1 {
		t.Fatalf("document identifiers = %+v, want the metadata DOI surfaced", res.DocumentIdentifiers)
	}
	got := res.DocumentIdentifiers[0]
	if got.Kind != "doi" || got.Value != "10.4321/metadata.only" {
		t.Fatalf("identifier = %+v, want the normalized doi ready to retype", got)
	}
	if got.Source != "xmp/prism:doi" {
		t.Fatalf("source = %q, want the field it was read from so the operator can judge it", got.Source)
	}
	for _, s := range res.Suggestions {
		if s.Verdict == "qualifies" {
			t.Fatalf("suggestion %+v qualifies; the metadata identifier must not authorise a candidate", s)
		}
	}
}
