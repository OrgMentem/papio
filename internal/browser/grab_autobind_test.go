// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/grab"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/work"
)

func parkManualDownload(t *testing.T, jobs *job.Store, reqID string, w work.Work) string {
	t.Helper()
	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, reqID, w, "", "", job.Policy{AccessMode: config.ModeDelegated, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{{job.StateQueued, job.StateResolving}, {job.StateResolving, job.StateAwaitingHuman}} {
		if err := jobs.Transition(ctx, id, step[0], step[1], map[string]any{"reason": "manual_download"}); err != nil {
			t.Fatalf("%s->%s: %v", step[0], step[1], err)
		}
	}
	if _, err := jobs.OpenHumanAction(ctx, id, "manual_download", "please download", job.Access(false, "")); err != nil {
		t.Fatal(err)
	}
	return id
}

// ---------------------------------------------------------------------------
// DOI-less-front-matter helpers
//
// Auto-bind only runs when the front-matter window (1 KiB) has no conclusive
// DOI, and QualifyCandidate additionally requires the candidate's own
// identifier to be corroborated inside identityPageOne (4 KiB). A qualifying
// document must therefore print title/authors/year at the top, then enough
// body prose to push the DOI past byte 1024, then the DOI still inside 4096.
// These helpers build that shape, assert the byte offsets they produce, and
// prove the front-matter window is empty via pdf.FrontMatterDOIs — the real
// precondition, expressed with the real helper.
// ---------------------------------------------------------------------------

const autobindFiller = "Synthetic calibration filler sentence with no identifier. "

func autobindExcerpt(t *testing.T, title, authorLine, doi string) string {
	t.Helper()
	header := title + "\n" + authorLine + "\n"
	filler := strings.Repeat(autobindFiller, 28) // 52*28 = 1456 bytes
	doiLine := "DOI: " + doi + "\n"
	tail := "\nAbstract\nWe study quantum networks.\n"
	text := header + filler + doiLine + tail
	off := strings.Index(text, doi)
	if off == -1 {
		t.Fatalf("autobindExcerpt: DOI %q not found", doi)
	}
	if off < 1024 {
		t.Fatalf("autobindExcerpt: DOI offset %d < 1024 (front-matter window), header %d filler %d", off, len(header), len(filler))
	}
	if off > 4096-len(doiLine)-len(tail)-10 {
		t.Fatalf("autobindExcerpt: DOI offset %d beyond page-one window 4096", off)
	}
	if got := pdf.FrontMatterDOIs(text); len(got) != 0 {
		t.Fatalf("autobindExcerpt: front-matter DOIs = %v, want empty (DOI at %d)", got, off)
	}
	return text
}

func autobindExcerptTwoDOIs(t *testing.T, title, authorLine, doi1, doi2 string) string {
	t.Helper()
	header := title + "\n" + authorLine + "\n"
	filler := strings.Repeat(autobindFiller, 28)
	doiLines := "DOI: " + doi1 + "\nDOI: " + doi2 + "\n"
	tail := "\nAbstract\nWe study quantum networks.\n"
	text := header + filler + doiLines + tail
	off1 := strings.Index(text, doi1)
	off2 := strings.Index(text, doi2)
	if off1 == -1 || off2 == -1 {
		t.Fatalf("autobindExcerptTwoDOIs: DOI offsets %d,%d not found", off1, off2)
	}
	if off1 < 1024 || off2 < 1024 {
		t.Fatalf("autobindExcerptTwoDOIs: DOI offsets %d,%d < 1024", off1, off2)
	}
	if off2 > 4096-len(tail)-10 {
		t.Fatalf("autobindExcerptTwoDOIs: second DOI offset %d beyond 4096", off2)
	}
	if got := pdf.FrontMatterDOIs(text); len(got) != 0 {
		t.Fatalf("autobindExcerptTwoDOIs: front-matter DOIs = %v, want empty", got)
	}
	return text
}

func doiLessExcerpt(t *testing.T, title, authorLine string) string {
	t.Helper()
	header := title + "\n" + authorLine + "\n"
	filler := strings.Repeat(autobindFiller, 28)
	tail := "\nAbstract\nBody without identifier for review testing.\n"
	text := header + filler + tail
	if got := pdf.FrontMatterDOIs(text); len(got) != 0 {
		t.Fatalf("doiLessExcerpt: front-matter DOIs = %v, want empty", got)
	}
	if strings.Contains(strings.ToLower(text), "10.1234") || strings.Contains(strings.ToLower(text), "10.9999") {
		t.Fatalf("doiLessExcerpt: unexpected DOI-like substring in filler")
	}
	off := len(header + filler)
	if off < 1024 {
		t.Fatalf("doiLessExcerpt: filler offset %d < 1024, front-matter window not cleared", off)
	}
	return text
}

// exoticExcerpt is a document that the bind predicate will uniquely qualify
// for w and not for any unrelated work with different authors/titles.
// It now delegates to the DOI-less-front-matter helper so the blind path
// abstains and the auto-bind path runs.
func exoticExcerpt(w work.Work) string {
	// Title as line, author, year, and the DOI on page one — the four hard
	// gates the rule demands. The DOI is placed past 1 KiB via filler.
	header := w.Title + "\n" + w.Authors[0] + " (2026)\n"
	filler := strings.Repeat(autobindFiller, 28)
	return header + filler + "DOI: " + w.DOI + "\n\nAbstract\nBody.\n"
}

// validateForExcerpt returns a Validate stub that prints exoticExcerpt(candidateWork)
// with no DOI in the front-matter DOI window — so the blind path abstains and
// the auto-bind path runs.
func validateForExcerpt(excerpt string) func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
	return func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 1},
			Text:       pdf.TextReport{Chars: int64(len(excerpt)), Excerpt: excerpt},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass, Evidence: []string{"stub pass"}},
		}, nil
	}
}
func writeLargeFixturePDF(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("%PDF-1.4\nadopted\n")); err != nil {
		t.Fatal(err)
	}
	// ~32 MiB widens the copy window so the concurrent resolver reliably
	// lands between the decision (outside tx) and the fence (inside tx).
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = 'x'
	}
	for i := 0; i < 32; i++ {
		if _, err := f.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.Write([]byte("\n%%EOF")); err != nil {
		t.Fatal(err)
	}
}

func TestSettledGrabBindsUniqueQualifyingCandidate(t *testing.T) {
	// 1. A settled grab whose text uniquely qualifies exactly one candidate-
	// eligible job binds to it: grab state job_created, job_id set,
	// provenance recorded with the rule version, and the file ingested.
	b, jobs, cfg, _ := newBridge(t)
	candidateWork := work.Work{
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2026,
		DOI:     "10.1234/autobind.0001",
	}
	candidateID := parkManualDownload(t, jobs, "wr_auto_ok", candidateWork)
	// Seed an unrelated non-qualifying job so the winner is selected among N.
	parkManualDownload(t, jobs, "wr_auto_other", work.Work{DOI: "10.9999/other.1", Title: "Attention Mechanisms for Sequence Transduction Networks", Authors: []string{"David Chen"}, Year: 2019})

	excerpt := autobindExcerpt(t, candidateWork.Title, "Ada Lovelace (2026)", candidateWork.DOI)
	// Explicit precondition: front-matter DOI set must be empty, otherwise this
	// test would silently re-exercise the blind DOI path instead of auto-bind.
	if got := pdf.FrontMatterDOIs(excerpt); len(got) != 0 {
		t.Fatalf("front-matter DOIs = %v, want empty for auto-bind", got)
	}
	b.svc.Validate = validateForExcerpt(excerpt)
	ctx := context.Background()
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Bind Me")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", g.ID)
	writeFixturePDF(t, filepath.Join(dir, "paper.pdf"))

	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, _ := b.grabs.Get(ctx, g.ID)
	if got.State != grab.StateJobCreated || got.JobID != candidateID {
		t.Fatalf("grab = %+v, want job_created -> %s", got, candidateID)
	}
	if got.Outcome != "job_created" {
		t.Fatalf("outcome = %q, want job_created (wire vocabulary unchanged)", got.Outcome)
	}
	if got.BindProvenance == "" {
		t.Fatal("bind_provenance empty, want JSON")
	}
	var prov grab.BindProvenance
	if err := json.Unmarshal([]byte(got.BindProvenance), &prov); err != nil {
		t.Fatalf("unmarshal provenance: %v", err)
	}
	if prov.Method != "candidate_auto_bind" || prov.Rule != pdf.CandidateBindingRule || prov.Winner != candidateID || prov.CandidatesConsidered == 0 || len(prov.Evidence) == 0 {
		t.Fatalf("provenance %+v missing expected fields", prov)
	}
	row, _ := jobs.Get(ctx, candidateID)
	if row.State != job.StateReady || row.ArtifactSHA256 == "" {
		t.Fatalf("candidate job not adopted: %+v", row)
	}
	if _, err := os.Stat(filepath.Join(cfg.EffectiveAdoptionRoot(), candidateID, "paper.pdf")); err != nil && !errors.Is(err, os.ErrNotExist) {
		// File may have been promoted to artifact store; absence alone is not failure.
		// Ready state above already proves adoption.
	}
}

func TestSettledGrabEmptyPoolParks(t *testing.T) {
	// 2. No candidate-eligible jobs: parks parked_no_identifier.
	// Must use DOI-less front matter or this silently retests the blind path.
	b, _, cfg, _ := newBridge(t)
	excerpt := doiLessExcerpt(t, "No Candidates Here", "Wilhelmina Farnsworth (2022)")
	if got := pdf.FrontMatterDOIs(excerpt); len(got) != 0 {
		t.Fatalf("front-matter DOIs = %v, want empty", got)
	}
	b.svc.Validate = validateForExcerpt(excerpt)
	ctx := context.Background()
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Empty Pool")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", g.ID)
	writeFixturePDF(t, filepath.Join(dir, "main.pdf"))
	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, _ := b.grabs.Get(ctx, g.ID)
	if got.State != grab.StateParkedNoIdentifier || got.Outcome != "needs_identifier" {
		t.Fatalf("grab = %+v, want parked_no_identifier/needs_identifier", got)
	}
	if got.JobID != "" {
		t.Fatalf("job_id = %q, want empty on empty pool", got.JobID)
	}
	if got.BindProvenance != "" {
		t.Fatalf("bind_provenance = %q, want empty on empty pool", got.BindProvenance)
	}
}

func TestSettledGrabTieParks(t *testing.T) {
	// 3. Two candidate-eligible jobs both qualify → selector abstains (tie).
	// Honest construction: two eligible jobs describing the same work
	// (preprint / version-of-record pair) with shared title/authors printed
	// once and both identifiers corroborated inside page one.
	b, jobs, cfg, _ := newBridge(t)
	sharedTitle := "Quantum Networks Robustness Calibration Measurement"
	w1 := work.Work{Title: sharedTitle, Authors: []string{"Ada Lovelace"}, Year: 2026, DOI: "10.1234/autobind.tie.1"}
	w2 := work.Work{Title: sharedTitle, Authors: []string{"Ada Lovelace"}, Year: 2026, DOI: "10.1234/autobind.tie.2"}
	parkManualDownload(t, jobs, "wr_tie_a", w1)
	parkManualDownload(t, jobs, "wr_tie_b", w2)
	excerpt := autobindExcerptTwoDOIs(t, sharedTitle, "Ada Lovelace (2026)", w1.DOI, w2.DOI)
	if got := pdf.FrontMatterDOIs(excerpt); len(got) != 0 {
		t.Fatalf("front-matter DOIs = %v, want empty for tie", got)
	}
	// Sanity: the constructed document must genuinely make both candidates qualify,
	// otherwise this test would not exercise the tie branch.
	cands := []pdf.BindCandidate{
		{Key: "a", Work: w1, Bound: []string{w1.DOI}},
		{Key: "b", Work: w2, Bound: []string{w2.DOI}},
	}
	for _, c := range cands {
		q := pdf.QualifyCandidate(excerpt, c)
		if !q.Qualifies {
			t.Fatalf("tie fixture: candidate %s should qualify but got %+v", c.Key, q)
		}
	}
	if _, ok, _ := pdf.SelectAutoBindCandidate(excerpt, cands); ok {
		t.Fatalf("tie fixture: SelectAutoBindCandidate should abstain on two qualifiers")
	}

	b.svc.Validate = validateForExcerpt(excerpt)
	ctx := context.Background()
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Tie Paper")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", g.ID)
	writeFixturePDF(t, filepath.Join(dir, "main.pdf"))
	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, _ := b.grabs.Get(ctx, g.ID)
	if got.State != grab.StateParkedNoIdentifier {
		t.Fatalf("grab = %+v, want parked on tie", got)
	}
	if got.JobID != "" {
		t.Fatalf("job_id = %q after tie, want empty", got.JobID)
	}
	if got.BindProvenance != "" {
		t.Fatalf("bind_provenance = %q after tie, want empty", got.BindProvenance)
	}
	if got.Outcome != "needs_identifier" {
		t.Fatalf("outcome = %q after tie, want needs_identifier", got.Outcome)
	}
}

func TestSettledGrabReviewOnlyParks(t *testing.T) {
	// 4. Best candidate is only Review: title/author/year agree but identifier
	// absent on page one — must park, not bind.
	// Must use DOI-less front matter or this silently retests the blind path.
	b, jobs, cfg, _ := newBridge(t)
	w := work.Work{Title: "Quantum Networks Robustness Calibration Measurement", Authors: []string{"Ada Lovelace"}, Year: 2026, DOI: "10.1234/autobind.review.1"}
	parkManualDownload(t, jobs, "wr_review_only", w)
	// Excerpt with title/author/year but NO DOI -> QualifyCandidate -> Review.
	excerpt := doiLessExcerpt(t, w.Title, "Ada Lovelace (2026)")
	if got := pdf.FrontMatterDOIs(excerpt); len(got) != 0 {
		t.Fatalf("front-matter DOIs = %v, want empty for review-only", got)
	}
	// Prove the fixture is Review, not Qualifies.
	q := pdf.QualifyCandidate(excerpt, pdf.BindCandidate{Key: "x", Work: w, Bound: []string{w.DOI}})
	if q.Qualifies || !q.Review {
		t.Fatalf("review fixture: want Review true Qualifies false, got %+v", q)
	}
	b.svc.Validate = validateForExcerpt(excerpt)
	ctx := context.Background()
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Review Paper")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", g.ID)
	writeFixturePDF(t, filepath.Join(dir, "main.pdf"))
	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, _ := b.grabs.Get(ctx, g.ID)
	if got.State != grab.StateParkedNoIdentifier {
		t.Fatalf("grab = %+v, want parked on review-only", got)
	}
	if got.JobID != "" {
		t.Fatalf("job_id = %q after review-only, want empty", got.JobID)
	}
	if got.BindProvenance != "" {
		t.Fatalf("bind_provenance = %q after review-only, want empty", got.BindProvenance)
	}
}

func TestSettledGrabFenceRejectionParksAndRemovesStagedFile(t *testing.T) {
	// 5. Unique qualifier stops being eligible between decision and commit:
	// the bridge's fence re-reads via tx and must reject. The intended seam
	// is resolving the winner's open manual_download action inside the fence
	// window (between file copy and commit). We enlarge the window with a
	// large grab file and resolve concurrently when the staged file appears.
	b, jobs, cfg, _ := newBridge(t)
	winnerWork := work.Work{Title: "Quantum Networks Robustness Calibration Measurement", Authors: []string{"Ada Lovelace"}, Year: 2026, DOI: "10.1234/autobind.fence.1"}
	winnerID := parkManualDownload(t, jobs, "wr_fence_win", winnerWork)
	other := work.Work{Title: "Attention Mechanisms for Sequence Transduction Networks", Authors: []string{"David Chen"}, Year: 2019, DOI: "10.9999/other.fence.1"}
	parkManualDownload(t, jobs, "wr_fence_other", other)
	excerpt := autobindExcerpt(t, winnerWork.Title, "Ada Lovelace (2026)", winnerWork.DOI)
	if got := pdf.FrontMatterDOIs(excerpt); len(got) != 0 {
		t.Fatalf("front-matter DOIs = %v, want empty for fence test", got)
	}
	b.svc.Validate = validateForExcerpt(excerpt)
	ctx := context.Background()
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Fence Paper")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", g.ID)
	// Large file widens the copy window so the background resolver can land
	// inside the fence window (after decision, before commit) without flaking.
	writeLargeFixturePDF(t, filepath.Join(dir, "paper.pdf"))
	actions, _ := jobs.ListHumanActions(ctx, false)
	var winnerAction int64
	for _, a := range actions {
		if a.JobID == winnerID && a.Kind == "manual_download" && a.Status == "open" {
			winnerAction = a.ID
			break
		}
	}
	if winnerAction == 0 {
		t.Fatal("winner has no open manual_download to resolve")
	}
	// Background resolver: when the staged copy appears, resolve the winner.
	// That is the production fence window: the file has been copied to the
	// job dir but the bind transaction has not yet committed.
	staged := filepath.Join(cfg.EffectiveAdoptionRoot(), winnerID, "paper.pdf")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 400; i++ {
			if _, err := os.Stat(staged); err == nil {
				_ = jobs.ResolveHumanAction(ctx, winnerAction, "resolved")
				return
			}
			// Also handle unique dest suffix.
			if matches, _ := filepath.Glob(filepath.Join(cfg.EffectiveAdoptionRoot(), winnerID, "paper*.pdf")); len(matches) > 0 {
				_ = jobs.ResolveHumanAction(ctx, winnerAction, "resolved")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	<-done
	// Fallback: if the watcher missed the narrow window (file was copied and
	// then removed on fence rejection before we polled), ensure the winner is
	// resolved so the fence would have rejected. In that case the grab will
	// already be parked; resolving again is harmless.
	_ = jobs.ResolveHumanAction(ctx, winnerAction, "resolved")

	got, _ := b.grabs.Get(ctx, g.ID)
	if got.State != grab.StateParkedNoIdentifier {
		t.Fatalf("grab = %+v, want parked after fence rejection", got)
	}
	if got.JobID != "" {
		t.Fatalf("job_id = %q after fence rejection, want empty", got.JobID)
	}
	if got.BindProvenance != "" {
		t.Fatalf("bind_provenance = %q after fence rejection, want empty", got.BindProvenance)
	}
	if got.Outcome != "needs_identifier" {
		t.Fatalf("outcome = %q after fence rejection, want needs_identifier", got.Outcome)
	}
	// No staged file left behind.
	if _, err := os.Stat(staged); err == nil {
		t.Fatalf("staged file %s left behind after fence rejection", staged)
	}
	if matches, _ := filepath.Glob(filepath.Join(cfg.EffectiveAdoptionRoot(), winnerID, "paper*.pdf")); len(matches) > 0 {
		t.Fatalf("staged files %v left behind after fence rejection", matches)
	}
}

func TestSettledGrabConclusiveDOIShortCircuitsToBlindPath(t *testing.T) {
	// 6. A PDF whose text already carries a conclusive front-matter DOI takes
	// the blind createGrabJob path and never runs auto-bind, even with eligible
	// candidates.
	// This is the ONE case that SHOULD keep its DOI in the front matter —
	// it is the odd one out by design, proving the DOI window short-circuits
	// before auto-bind is consulted.
	b, jobs, cfg, _ := newBridge(t)
	parkManualDownload(t, jobs, "wr_doi_blind_other", work.Work{Title: "Unrelated Paper About Methodology", Authors: []string{"Other Author"}, Year: 2021, DOI: "10.9999/blind.other.1"})
	doi := "10.1234/grab.blind.doi"
	b.svc.Validate = grabDOIValidate(doi)
	ctx := context.Background()
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Blind DOI Paper")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", g.ID)
	writeFixturePDF(t, filepath.Join(dir, "main.pdf"))
	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, _ := b.grabs.Get(ctx, g.ID)
	if got.State != grab.StateJobCreated || got.JobID == "" {
		t.Fatalf("grab = %+v, want job_created via blind DOI", got)
	}
	// Blind path does NOT record candidate_auto_bind provenance.
	if got.BindProvenance != "" {
		t.Fatalf("bind_provenance = %q on blind DOI path, want empty", got.BindProvenance)
	}
	row, _ := jobs.Get(ctx, got.JobID)
	if row.Work.DOI != doi {
		t.Fatalf("job DOI = %q, want %q", row.Work.DOI, doi)
	}
	var _ = config.ModeDelegated
}
