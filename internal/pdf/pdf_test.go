// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/work"
)

func TestValidatePayload(t *testing.T) {
	valid := append([]byte("%PDF-1.7\n"), []byte(strings.Repeat("x", MinimumPayloadBytes))...)
	valid = append(valid, []byte("\n%%EOF")...)
	cases := []struct {
		name string
		body []byte
		want bool
	}{
		{"valid", valid, true},
		{"html claiming PDF", []byte("<html><body>nope</body></html>" + strings.Repeat("x", MinimumPayloadBytes)), false},
		{"short body", []byte("%PDF-1.7\n%%EOF"), false},
		{"header not at byte zero", append([]byte("\xef\xbb\xbf"), valid...), false},
		{"early eof with appended payload", append([]byte("%PDF-1.7\n%%EOF"), []byte(strings.Repeat("x", 9000))...), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidatePayload(tc.body, "text/html") // MIME must not be decisive.
			if got.OK != tc.want {
				t.Fatalf("OK=%v, reason=%q", got.OK, got.Reason)
			}
		})
	}
}

func TestStructuralWorkerContractCapsAndDeadline(t *testing.T) {
	path := writeTempPDF(t)
	t.Run("worker contract", func(t *testing.T) {
		worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":true,"Pages":2}'`)
		got, err := ValidateStructural(context.Background(), worker, path, StructuralOptions{MaxPages: 10})
		if err != nil || !got.Valid || got.Pages != 2 {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	})
	t.Run("output cap", func(t *testing.T) {
		worker := fakeTool(t, `cat >/dev/null; yes x | tr -d '\n' | head -c 10000`)
		_, err := ValidateStructural(context.Background(), worker, path, StructuralOptions{MaxOutputBytes: 32})
		if err == nil || !strings.Contains(err.Error(), "output exceeds") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("deadline kills worker", func(t *testing.T) {
		worker := fakeTool(t, `cat >/dev/null; sleep 10`)
		start := time.Now()
		_, err := ValidateStructural(context.Background(), worker, path, StructuralOptions{Timeout: 50 * time.Millisecond})
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err=%v", err)
		}
		if time.Since(start) > time.Second {
			t.Fatal("worker was not killed promptly")
		}
	})
	t.Run("encrypted report is rejected", func(t *testing.T) {
		worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":false,"Encrypted":true,"Reason":"encrypted PDF"}'`)
		got, err := ValidateStructural(context.Background(), worker, path, StructuralOptions{})
		if err != nil || got.Valid || !got.Encrypted {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	})
}

func TestStructuralWorkerParsesOnlyAtWorkerEntry(t *testing.T) {
	validPath := writeRealPDF(t)
	var out bytes.Buffer
	request, _ := json.Marshal(workerRequest{Path: validPath, MaxPages: 10})
	if err := RunStructuralWorker(bytes.NewReader(request), &out); err != nil {
		t.Fatal(err)
	}
	var valid StructuralReport
	if err := json.Unmarshal(out.Bytes(), &valid); err != nil || !valid.Valid || valid.Pages != 1 {
		t.Fatalf("valid report=%+v err=%v", valid, err)
	}

	malformed := filepath.Join(t.TempDir(), "malformed.pdf")
	if err := os.WriteFile(malformed, []byte("%PDF-1.7 broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	request, _ = json.Marshal(workerRequest{Path: malformed, MaxPages: 10})
	if err := RunStructuralWorker(bytes.NewReader(request), &out); err != nil {
		t.Fatal(err)
	}
	var bad StructuralReport
	if err := json.Unmarshal(out.Bytes(), &bad); err != nil || bad.Valid || bad.Reason == "" {
		t.Fatalf("malformed report=%+v err=%v", bad, err)
	}
	if err := RunStructuralWorker(bytes.NewReader(bytes.Repeat([]byte("x"), 16<<10+1)), &out); err == nil {
		t.Fatal("expected worker request body cap rejection")
	}
}

func writeRealPDF(t *testing.T) string {
	t.Helper()
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << >> /Contents 4 0 R >>",
		"<< /Length 0 >>\nstream\n\nendstream",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, 0, len(objects))
	for i, object := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	path := filepath.Join(t.TempDir(), "valid.pdf")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractTextAndOCRCapabilityEvidence(t *testing.T) {
	path := writeTempPDF(t)
	pdftotext := fakeTool(t, `printf 'short text'`)
	report, err := ExtractText(context.Background(), path, Capability{PDFToText: pdftotext}, DefaultSemanticOptions())
	if err != nil || !report.NeedsReview || report.OCRUsed {
		t.Fatalf("got=%+v err=%v", report, err)
	}
	if !strings.Contains(strings.Join(report.Evidence, " "), "capability") {
		t.Fatalf("missing capability evidence: %+v", report)
	}

	hung := fakeTool(t, `sleep 10`)
	report, err = ExtractText(context.Background(), path, Capability{PDFToText: hung}, SemanticOptions{Timeout: 50 * time.Millisecond})
	if err != nil || !report.NeedsReview || !strings.Contains(strings.Join(report.Evidence, " "), "deadline") {
		t.Fatalf("got=%+v err=%v", report, err)
	}

	pdftoppm := fakeTool(t, `for last; do :; done; printf png > "$last-1.png"`)
	tesseract := fakeTool(t, `i=0; while [ "$i" -lt 300 ]; do printf 'imageword '; i=$((i+1)); done`)
	report, err = ExtractText(context.Background(), path, Capability{PDFToText: pdftotext, PDFToPPM: pdftoppm, Tesseract: tesseract}, DefaultSemanticOptions())
	if err != nil || !report.OCRUsed || report.NeedsReview || report.Chars < 1000 {
		t.Fatalf("OCR report=%+v err=%v", report, err)
	}
}

func TestMatchIdentity(t *testing.T) {
	target := work.Work{DOI: "10.1234/ABC.9", Title: "Deterministic Validation of Scholarly Article Identity", Authors: []string{"Ada Lovelace"}, Year: 2026}
	if got := MatchIdentity("Supporting Information doi:10.1234/abc.9", target); got.Result != IdentityReject {
		t.Fatalf("marker: %+v", got)
	}
	if got := MatchIdentity("Journal header\nSupplementary Information\nDOI: 10.1234/abc.9", target); got.Result != IdentityReject {
		t.Fatalf("supplementary heading after journal header: %+v", got)
	}
	if got := MatchIdentity("Journal header\nThis article cites supplementary material for methodological detail.\nDOI: 10.1234/abc.9", target); got.Result != IdentityPass {
		t.Fatalf("front-matter supplementary citation: %+v", got)
	}
	if got := MatchIdentity("doi:10.9999/nope", target); got.Result != IdentityReject {
		t.Fatalf("wrong DOI: %+v", got)
	}
	if got := MatchIdentity("References doi:10.9999/nope; article DOI:10.1234/abc.9", target); got.Result != IdentityPass {
		t.Fatalf("matching DOI after reference: %+v", got)
	}
	text := "Deterministic validation of scholarly article identity. Ada Lovelace (2026)."
	if got := MatchIdentity(text, target); got.Result != IdentityPass {
		t.Fatalf("title match: %+v", got)
	}
	if got := MatchIdentity(text+"\fReferences\n10.9999/nope", target); got.Result != IdentityPass {
		t.Fatalf("reference DOI: %+v", got)
	}
	if got := MatchIdentity(text+"\nAbstract\nSee supplementary material for methods.", target); got.Result != IdentityPass {
		t.Fatalf("body supplementary mention: %+v", got)
	}
	legacyAPA := work.Work{DOI: "10.1037/0021-9010.87.4.611"}
	if got := MatchIdentity("Copyright line DOI: 10.1037//0021-9010.87.4.611", legacyAPA); got.Result != IdentityPass {
		t.Fatalf("legacy APA DOI: %+v", got)
	}
}

// titleThreshold is a REJECT FLOOR, not a pass level: below it a candidate
// is rejected outright, but clearing it only keeps the candidate in play —
// whether it actually PASSES depends on the printed-title-line requirement
// below, at every threshold. This test used to assert that a 3/5 partial
// title match PASSED at a 0.6 threshold, which is the exact unordered-
// membership shortcut responsible for all 52 of the corpus's wrong accepts
// (see the family tests further down), so the old assertion defended
// precisely the bug the corpus exposed. The contract below is the fix:
// reject below the floor, review above it without a printed title line, and
// pass only once the full title is printed as its own line, at any
// threshold that clears the floor.
func TestMatchIdentityHonorsTitleThreshold(t *testing.T) {
	target := work.Work{Title: "Quantum Networks Robustness Calibration Measurement", Authors: []string{"Lovelace"}, Year: 2026}
	partial := "Quantum networks robustness. Lovelace (2026)."
	if got := MatchIdentityWithThreshold(partial, target, 0.8); got.Result != IdentityReject {
		t.Fatalf("80%% threshold result = %+v, want reject: 3/5 tokens is below the floor", got)
	}
	if got := MatchIdentityWithThreshold(partial, target, 0.6); got.Result != IdentityReview {
		t.Fatalf("60%% threshold result = %+v, want review: 3/5 tokens clears the floor but the title is not printed as a line", got)
	}
	full := "Quantum Networks Robustness Calibration Measurement\nLovelace (2026)."
	if got := MatchIdentityWithThreshold(full, target, 0.8); got.Result != IdentityPass {
		t.Fatalf("80%% threshold result = %+v, want pass: the full title is printed as its own line", got)
	}
	if got := MatchIdentityWithThreshold(full, target, 0.6); got.Result != IdentityPass {
		t.Fatalf("60%% threshold result = %+v, want pass: the full title is printed as its own line", got)
	}
}

// Every case below is a shape taken from a real PDF, measured over a 40-paper
// library and the 1560 deliberately mismatched document/metadata pairs it
// yields. Before these rules, 155 of those 1560 pairs (9.9%) were accepted as
// the wrong work; afterwards none were, and correct acceptance rose from 39/40
// to 40/40.

// ACM letter-spaces the DOI on page one, so pdftotext emits "10.1145/ 30 6 5 3
// 8 6". The exact requested identifier is printed on the page and no regex
// could see it, so a perfect title match was staged for human review over a
// reprint year that the document does not contain and never will.
func TestIdentityReadsALetterSpacedDOI(t *testing.T) {
	target := work.Work{
		DOI: "10.1145/3065386", Year: 2017,
		Title:   "ImageNet classification with deep convolutional neural networks",
		Authors: []string{"Alex Krizhevsky", "Ilya Sutskever", "Geoffrey E. Hinton"},
	}
	text := "research highlights\nDOI:10.1145/ 30 6 5 3 8 6\n\n" +
		"ImageNet Classification with Deep\nConvolutional Neural Networks\n" +
		"By Alex Krizhevsky, Ilya Sutskever, and Geoffrey E. Hinton\n\nAbstract\n" +
		"We trained a large, deep convolutional neural network.\n" +
		"\fReferences\n1. Cireşan et al., 2012.\n"
	got := MatchIdentity(text, target)
	if got.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass on the printed DOI", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "prints the requested DOI") {
		t.Fatalf("evidence = %v, want the corroborating identifier named", got.Evidence)
	}
}

// A bibliography is several hundred other papers' titles and thousands of their
// authors. Matching identity against the whole document let any long paper
// satisfy any other paper's author and year.
func TestIdentityIgnoresEvidenceFoundOnlyInTheBibliography(t *testing.T) {
	target := work.Work{
		Title:   "Attention Mechanisms for Sequence Transduction Networks",
		Authors: []string{"David Chen"}, Year: 2019,
	}
	text := "An Unrelated Study of Soil Composition\nBy Wilhelmina Farnsworth\n\n" +
		"Abstract\nWe examine soil.\n\fReferences\n" +
		"David Chen. Attention mechanisms for sequence transduction networks. 2019.\n"
	if got := MatchIdentity(text, target); got.Result == IdentityPass {
		t.Fatalf("result = %+v, want the reference list not to establish identity", got)
	}
}

// "david", "john", "robert" and even an organisational "the" appear in almost
// any reference list; only the family name discriminates.
func TestIdentityRequiresTheFamilyNameNotAGivenName(t *testing.T) {
	target := work.Work{Title: "Robustness Calibration Measurement Networks", Authors: []string{"David Okonkwo"}}
	byline := "Robustness Calibration Measurement Networks\nBy David Ferreira\n\nAbstract\n"
	if got := MatchIdentity(byline, target); got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: a shared given name is not the author", got)
	}
	if got := MatchIdentity("Robustness Calibration Measurement Networks\nBy David Okonkwo\n", target); got.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass on the family name", got)
	}
}

// Superscript affiliation markers glue a letter onto every surname in a byline.
// One real 12-author paper had all twelve marked this way and failed outright.
func TestIdentityToleratesAffiliationMarkersGluedToSurnames(t *testing.T) {
	target := work.Work{
		Title:   "Explainable Artificial Intelligence: Concepts, Taxonomies, Opportunities",
		Authors: []string{"Alejandro Barredo Arrieta", "Siham Tabik"},
	}
	text := "Explainable Artificial Intelligence: Concepts, Taxonomies,\nOpportunities\n" +
		"Alejandro Barredo Arrietaa , Natalia Díaz-Rodríguezb , Siham Tabikg\n"
	if got := MatchIdentity(text, target); got.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass despite the glued markers", got)
	}
	// The tolerance must not turn a prefix into a wildcard.
	other := work.Work{Title: "Explainable Artificial Intelligence: Concepts, Taxonomies, Opportunities", Authors: []string{"Bo Arrietasanchez"}}
	if got := MatchIdentity(text, other); got.Result == IdentityPass {
		t.Fatalf("result = %+v, want a longer surname not to match on a prefix", got)
	}
}

// Justified text hyphenates across line breaks and some producers keep ligature
// codepoints; neither survives tokenization as the word the title contains.
func TestIdentityFoldsHyphenationAndLigatures(t *testing.T) {
	target := work.Work{Title: "Classification of Difficult Workflow Instances", Authors: []string{"Ada Lovelace"}}
	text := "Classifi-\ncation of Diﬃcult Workﬂow Instances\nAda Lovelace\n"
	if got := MatchIdentity(text, target); got.Result != IdentityPass {
		t.Fatalf("result = %+v, want the broken and ligatured words to match", got)
	}
}

// Reject discards the candidate; a scanned first page whose title surfaces
// further in is a question for a human, not a verdict.
func TestIdentityReviewsRatherThanRejectsWhenTheTitleIsOnlyDeeperIn(t *testing.T) {
	target := work.Work{Title: "Deterministic Validation of Scholarly Article Identity", Authors: []string{"Ada Lovelace"}}
	text := "SCANNED COVER SHEET\nProvided by the library\n\f" +
		"Deterministic Validation of Scholarly Article Identity\nAda Lovelace\n"
	if got := MatchIdentity(text, target); got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review rather than a hard reject", got)
	}
}

// arXiv and PMID are as strong as a DOI and were never consulted.
func TestIdentityCorroboratesArXivAndPMID(t *testing.T) {
	title := "Deep Learning in Neural Networks: An Overview"
	body := "Deep Learning in Neural Networks: An Overview\n\narXiv:1404.7828v4 [cs.NE] 8 Oct 2014\nJürgen Schmidhuber\n"
	if got := MatchIdentity(body, work.Work{Title: title, ArXiv: "1404.7828", Year: 2015}); got.Result != IdentityPass {
		t.Fatalf("arXiv result = %+v, want pass", got)
	}
	pubmed := "Deep Learning in Neural Networks: An Overview\nPMID: 25462637\n"
	if got := MatchIdentity(pubmed, work.Work{Title: title, PMID: "25462637", Year: 2015}); got.Result != IdentityPass {
		t.Fatalf("pmid result = %+v, want pass", got)
	}
}

// The front-matter rules stay strict: a corroborating identifier elsewhere in
// the document must never overturn a document that names a different DOI.
func TestIdentityCorroborationNeverOverturnsADOIMismatch(t *testing.T) {
	target := work.Work{DOI: "10.1234/abc.9", Title: "Deterministic Validation of Scholarly Article Identity"}
	text := "DOI: 10.9999/other\nDeterministic Validation of Scholarly Article Identity\n" +
		"\fReferences\n10.1234/abc.9\n"
	if got := MatchIdentity(text, target); got.Result != IdentityReject {
		t.Fatalf("result = %+v, want the front-matter DOI mismatch to stand", got)
	}
}

// A catalogue record that names one author cannot clear the two-marker rule, so
// the marker tolerance had no way to establish authorship at all. Wiley prints
// the surname as "Ciani1∗" and the paper's own DOI below the abstract, past the
// front-matter window, so an exact identifier match plus a 7/7 title match was
// staged for human review.
func TestIdentityCorroboratesASingleMarkedAuthor(t *testing.T) {
	target := work.Work{
		DOI:   "10.1348/000709910x517399",
		Title: "Antecedents and trajectories of achievement goals: A self-determination theory perspective",
		// The record names the first author only; the paper has four.
		Authors: []string{"Ciani"}, Year: 2011,
	}
	text := "223\n\nBritish Journal of Educational Psychology (2011), 81, 223–243\n" +
		"Antecedents and trajectories of achievement\ngoals: A self-determination theory perspective\n" +
		"Keith D. Ciani1∗ , Kennon M. Sheldon2 , Jonathan C. Hilpert3\nand Matthew A. Easter4\n" +
		"Department of Counseling and Educational Psychology\n" +
		"Background. " + strings.Repeat("Achievement goal theory and self-determination theory. ", 30) +
		"\nDOI:10.1348/000709910X517399\n" +
		"\f224 Keith D. Ciani et al.\nAchievement goal theory provides a framework.\n"
	got := MatchIdentity(text, target)
	if got.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass on the printed DOI", got)
	}
	// Corroboration is still not a wildcard: a different surname stays review.
	other := target
	other.Authors = []string{"Okonkwo"}
	if got := MatchIdentity(text, other); got.Result == IdentityPass {
		t.Fatalf("result = %+v, want an unrelated single author not to pass", got)
	}
}

// A single marker carries the author check only when the byline settles the
// surname and page one settles the identifier. Both bounds answer the same
// document: a comment, reply, or erratum on the requested paper carries its
// title and cites its DOI, and would otherwise be filed as the paper itself.
func TestIdentityCorroborationOfOneAuthorNeedsANumberedMarkerOnPageOne(t *testing.T) {
	target := work.Work{
		DOI:   "10.1234/abc.9",
		Title: "Robustness Calibration Measurement Networks",
		// Clark, not Clarke; the tolerance cannot see the difference.
		Authors: []string{"Alice Clark"}, Year: 2024,
	}
	body := "Abstract. " + strings.Repeat("We calibrate robustness measurements over networks. ", 30)

	// A lettered near-miss surname, with the DOI cited in the bibliography.
	cited := "Comment on: Robustness Calibration Measurement Networks\nBob Clarke\n2024\n" +
		"\fReferences\nAlice Clark. Robustness calibration measurement networks, 2024. doi:10.1234/abc.9\n"
	if got := MatchIdentity(cited, target); got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: a cited DOI is not this document's own", got)
	}
	// The same near-miss surname on a one-page note that emits no form feed, so
	// the DOI it prints inline IS on its page one — past the front-matter window,
	// which would otherwise decide it. Only the marker rule can refuse this one.
	note := "Correction to: Robustness Calibration Measurement Networks\nBob Clarke\n2024\n" + body +
		"\nThe published version of doi:10.1234/abc.9 contained an error in Table 2.\n"
	if got := MatchIdentity(note, target); got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: Clarke may not be Clark", got)
	}
	// A numbered marker cannot be a different surname, and here the identifier
	// is the document's own, below the abstract past the front-matter window.
	own := "Robustness Calibration Measurement Networks\nAlice Clark1\n2024\n" + body +
		"\ndoi:10.1234/abc.9\n\fReferences\nUnrelated, 2019.\n"
	if got := MatchIdentity(own, target); got.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass on a page-one identifier", got)
	}
	// Numbered marker, but the only identifier is in the bibliography.
	numberedCite := "Robustness Calibration Measurement Networks\nAlice Clark1\n2024\n" + body +
		"\fReferences\nAlice Clark. Robustness calibration measurement networks, 2024. doi:10.1234/abc.9\n"
	if got := MatchIdentity(numberedCite, target); got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: page one prints no identifier", got)
	}
}

func writeTempPDF(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "input.pdf")
	if err := os.WriteFile(p, []byte("%PDF-1.7\n"+strings.Repeat("x", MinimumPayloadBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func fakeTool(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// macOS hands a launchd child PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin,
// so a daemon autostarted by the browser's native-messaging host cannot see
// Homebrew. Detection that trusted PATH alone made text extraction depend on
// who started the daemon — from a shell it worked, from the browser every PDF
// failed semantic extraction and was staged for human review.
func TestCapabilityDetectionSurvivesAGUIStrippedPath(t *testing.T) {
	tool := filepath.Join(t.TempDir(), "pdftotext")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/nonexistent-for-test")
	if got := lookTool("pdftotext"); got != "" && got != tool {
		t.Logf("host also provides %s outside PATH", got)
	}

	original := toolSearchPath
	t.Cleanup(func() { toolSearchPath = original })
	toolSearchPath = []string{filepath.Dir(tool)}
	if got := lookTool("pdftotext"); got != tool {
		t.Fatalf("lookTool = %q, want the well-known-prefix hit %q", got, tool)
	}
	if got := lookTool("definitely-not-installed"); got != "" {
		t.Fatalf("lookTool = %q, want empty for a genuinely missing tool", got)
	}
}

func TestCapabilityDetectionPrefersPATH(t *testing.T) {
	dir := t.TempDir()
	onPath := filepath.Join(dir, "pdftotext")
	if err := os.WriteFile(onPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fallbackDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(fallbackDir, "pdftotext"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	original := toolSearchPath
	t.Cleanup(func() { toolSearchPath = original })
	toolSearchPath = []string{fallbackDir}
	t.Setenv("PATH", dir)
	if got := lookTool("pdftotext"); got != onPath {
		t.Fatalf("lookTool = %q, want the PATH entry %q to win", got, onPath)
	}
}

// A directory or a non-executable file with the right name must not be mistaken
// for the tool: papio would then spawn it and treat the failure as a review.
func TestCapabilityDetectionIgnoresNonExecutableMatches(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "pdfinfo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pdftotext"), []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	original := toolSearchPath
	t.Cleanup(func() { toolSearchPath = original })
	toolSearchPath = []string{dir}
	t.Setenv("PATH", "/nonexistent-for-test")
	if got := lookTool("pdfinfo"); got != "" {
		t.Fatalf("lookTool matched a directory: %q", got)
	}
	if got := lookTool("pdftotext"); got != "" {
		t.Fatalf("lookTool matched a non-executable file: %q", got)
	}
}

// The review findings below are all false-ACCEPT paths introduced by the
// byline/corroboration rework. A wrong PDF filed into the library under the
// right citation is the worst outcome papio has, so each is pinned separately.

// A corroborated identifier is strong evidence but not a verdict: the title
// gate is unordered token membership at 60%, so "Comment on Deep Learning"
// clears a target titled "Deep Learning" and can carry a bare "Correction to:
// <DOI>" further down the page.
func TestIdentityRequiresBylineAgreementBesideAPrintedIdentifier(t *testing.T) {
	target := work.Work{DOI: "10.1234/deep.9", Title: "Deep Learning Representation Overview", Authors: []string{"Ada Lovelace"}, Year: 2020}
	comment := "Comment on Deep Learning Representation Overview\nBy Mallory Attacker\n\n" +
		strings.Repeat("filler discussion text. ", 80) + "\nCorrection to: 10.1234/deep.9\n"
	if got := MatchIdentity(comment, target); got.Result == IdentityPass {
		t.Fatalf("result = %+v, want the differing byline to withhold the pass", got)
	}
	genuine := "Deep Learning Representation Overview\nAda Lovelace\n" +
		strings.Repeat("body. ", 200) + "\n10.1234/deep.9\n"
	if got := MatchIdentity(genuine, target); got.Result != IdentityPass {
		t.Fatalf("result = %+v, want the genuine article to pass", got)
	}
}

// containsFlattenedToken used to return on a raw prefix, so PMID 12345 was
// "found" in PMID:123456 and DOI 10.1/foo in 10.1/foobar.
func TestIdentityRejectsPrefixCollisionsBetweenIdentifiers(t *testing.T) {
	byline := "Deterministic Validation of Scholarly Article Identity\nAda Lovelace\n"
	for name, test := range map[string]struct {
		target work.Work
		body   string
	}{
		"pmid prefix": {work.Work{PMID: "12345"}, "PMID: 123456\n"},
		"doi prefix":  {work.Work{DOI: "10.1234/foo"}, "see 10.1234/foobar for details\n"},
		"doi infix":   {work.Work{DOI: "10.1234/foo"}, "see 10.1234/foo.bar for details\n"},
	} {
		t.Run(name, func(t *testing.T) {
			test.target.Title = "Deterministic Validation of Scholarly Article Identity"
			test.target.Authors = []string{"Grace Hopper"}
			if got := MatchIdentity(byline+test.body, test.target); got.Result == IdentityPass {
				t.Fatalf("result = %+v, want a longer identifier not to corroborate", got)
			}
		})
	}
	// A sentence-final period still ends the identifier.
	sentence := work.Work{DOI: "10.1234/foo", Title: "Deterministic Validation of Scholarly Article Identity", Authors: []string{"Ada Lovelace"}}
	if got := MatchIdentity(byline+"available at 10.1234/foo.\n", sentence); got.Result != IdentityPass {
		t.Fatalf("result = %+v, want a trailing sentence period to corroborate", got)
	}
}

// An absent year is normal (preprints predate their issue, reprints postdate
// their text). A byline printing a DIFFERENT year is evidence against, and
// dropping the year gate entirely would accept a same-author neighbour.
func TestIdentityDistinguishesAnAbsentYearFromAContradictedOne(t *testing.T) {
	target := work.Work{Title: "Machine Learning Methods for Cancer Diagnosis", Authors: []string{"Alice Smith"}, Year: 2024}
	if got := MatchIdentity("Machine Learning Methods for Cancer Prognosis\nAlice Smith\n2023\n", target); got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review when the byline is dated differently", got)
	}
	if got := MatchIdentity("Machine Learning Methods for Cancer Diagnosis\nAlice Smith\n", target); got.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass when the byline simply carries no year", got)
	}
}

// The affiliation-marker tolerance cannot tell a marker from a different
// surname: "Clarke" is "Clark" plus one letter.
func TestIdentityWillNotAcceptASingleNearMissSurname(t *testing.T) {
	target := work.Work{Title: "Robustness Calibration Measurement Networks", Authors: []string{"Alice Clark"}, Year: 2024}
	if got := MatchIdentity("Robustness Calibration Measurement Networks\nBob Clarke\n2024\n", target); got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: Clarke is not Clark", got)
	}
	// Two marked surnames together are a marked byline, not two coincidences.
	multi := work.Work{Title: "Robustness Calibration Measurement Networks", Authors: []string{"Alice Clark", "Siham Tabik"}}
	if got := MatchIdentity("Robustness Calibration Measurement Networks\nAlice Clarka , Siham Tabikg\n", multi); got.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass on a consistently marked byline", got)
	}
}

// RIS, BibTeX and NBIB ingestion preserve "Last, First" verbatim, so taking the
// last token blindly looked for the GIVEN name of every imported reference.
func TestIdentityReadsCommaFormattedAuthorNames(t *testing.T) {
	target := work.Work{Title: "Deterministic Validation of Scholarly Article Identity", Authors: []string{"Smith, Ada"}}
	if got := MatchIdentity("Deterministic Validation of Scholarly Article Identity\nA. Smith\n", target); got.Result != IdentityPass {
		t.Fatalf("result = %+v, want the surname before the comma to be used", got)
	}
}

// Publishers letter-space with Unicode separators, not only ASCII spaces.
func TestIdentityReadsIdentifiersSeparatedByUnicodeSpaces(t *testing.T) {
	target := work.Work{DOI: "10.1145/3065386", Title: "ImageNet Classification Deep Convolutional Networks", Authors: []string{"Alex Krizhevsky"}}
	text := "ImageNet Classification Deep Convolutional Networks\nAlex Krizhevsky\n" +
		"DOI:10.1145/\u00a030\u200965\u202f386\n"
	if got := MatchIdentity(text, target); got.Result != IdentityPass {
		t.Fatalf("result = %+v, want no-break and thin spaces to be ignored", got)
	}
}

// The tests below pin the correction/comment guard. A correction, erratum, or
// comment article is a DIFFERENT work from the paper it discusses, and it
// routinely prints that paper's own DOI in its own front matter — a
// synthetic 1508-byte correction notice demonstrated this against
// IdentityPass before the guard existed. correctionMarkers names the shapes
// a correction takes and is matched exactly like nonArticleMarkers above: a
// case-folded LINE PREFIX over identityFrontMatter, and its presence caps the
// verdict at IdentityReview no matter which path through
// MatchIdentityWithThreshold would otherwise have returned IdentityPass.

// Before this guard, the front-matter DOI rule was unconditional: it runs
// before any title or author check at all, so an erratum notice printing the
// original paper's DOI passed as the paper outright, and neither the
// erratum's own title ("Erratum: …") nor its own byline ("Bob Clarke") ever
// entered the decision. The fixture is 92 bytes — comfortably inside the
// 1 KiB front-matter window the DOI rule reads — so the DOI genuinely sits in
// the window this test means to exercise.
func TestIdentityCapsAnErratumPrintingTheRequestedDOI(t *testing.T) {
	target := work.Work{
		DOI: "10.1234/cal.7", Year: 2025,
		Title:   "Sensor Network Calibration Under Adverse Weather",
		Authors: []string{"Priya Natarajan"},
	}
	erratum := "Erratum: Sensor Network Calibration Under Adverse Weather\nBob Clarke\n2025\ndoi:10.1234/cal.7\n"
	got := MatchIdentity(erratum, target)
	if got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: an erratum is not the paper it corrects", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "front matter marks a correction or comment: erratum") {
		t.Fatalf("evidence = %v, want the correction marker named", got.Evidence)
	}
}

// The same erratum, requested on purpose: its own title, its own author, its
// own year, and no requested DOI to force the front-matter DOI branch, so
// without the guard this clears title tokens, author, and year and reaches
// the unconditional pass at the end of MatchIdentityWithThreshold. The guard
// parks it anyway, and that is the intended cost of closing the door, not a
// regression: a correction notice never auto-passes, because the one case it
// cannot itself distinguish is being asked for the correction rather than the
// paper it corrects, and that call is left to a human.
func TestIdentityCapsTheErratumWhenItIsTheRequestedWork(t *testing.T) {
	target := work.Work{
		Title:   "Erratum: Sensor Network Calibration Under Adverse Weather",
		Authors: []string{"Bob Clarke"},
		Year:    2025,
	}
	erratum := "Erratum: Sensor Network Calibration Under Adverse Weather\nBob Clarke\n2025\ndoi:10.1234/cal.7\n"
	got := MatchIdentity(erratum, target)
	if got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: a correction notice never auto-passes, even when requested", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "front matter marks a correction or comment: erratum") {
		t.Fatalf("evidence = %v, want the correction marker named", got.Evidence)
	}
}

// A correction marker must never soften a wrong-document verdict into a
// park: a correction notice whose front matter names a DIFFERENT DOI than
// requested has to keep rejecting exactly as it did before the guard existed,
// proving the DOI-mismatch reject still runs — and still wins — ahead of the
// cap.
func TestIdentityCorrectionMarkerDoesNotSoftenADOIMismatch(t *testing.T) {
	target := work.Work{
		DOI: "10.1234/cal.7", Year: 2025,
		Title:   "Sensor Network Calibration Under Adverse Weather",
		Authors: []string{"Priya Natarajan"},
	}
	erratum := "Erratum: Sensor Network Calibration Under Adverse Weather\nBob Clarke\n2025\ndoi:10.9999/other\n"
	if got := MatchIdentity(erratum, target); got.Result != IdentityReject {
		t.Fatalf("result = %+v, want the front-matter DOI mismatch to stand", got)
	}
}

// The marker match is a line PREFIX, not a substring scan, so a real paper
// that merely mentions a marker word mid-sentence — a Bonferroni statistical
// correction, not an erratum about the paper itself — must keep passing
// exactly as before. The list is deliberately narrow at its edges too:
// "retraction of" is a marker because "Retraction of scientific papers: a
// bibliometric study" is a correction-adjacent title that legitimately opens
// a real paper, so that title is an accepted false-positive cost of the
// guard, parked rather than passed. "response to" is deliberately absent
// because "Response to Intervention" is a real educational framework, and a
// paper titled that way must still pass undisturbed.
func TestIdentityCorrectionMarkersAnchorAsLinePrefixes(t *testing.T) {
	t.Run("mid-line mention is not a marker", func(t *testing.T) {
		target := work.Work{Title: "Statistical Power Analysis for Clinical Trials", Authors: []string{"Alice Smith"}, Year: 2024}
		text := "Statistical Power Analysis for Clinical Trials\nAlice Smith\n2024\n" +
			"We applied a Bonferroni correction for multiple comparisons across all endpoints.\n"
		if got := MatchIdentity(text, target); got.Result != IdentityPass {
			t.Fatalf("result = %+v, want pass: \"correction\" mid-sentence is not the marker \"correction to\"/\"correction:\"", got)
		}
	})
	t.Run("retraction of is a marker and parks its own title", func(t *testing.T) {
		target := work.Work{Title: "Retraction of Scientific Papers: A Bibliometric Study", Authors: []string{"Jordan Lee"}, Year: 2022}
		text := "Retraction of Scientific Papers: A Bibliometric Study\nJordan Lee\n2022\n"
		got := MatchIdentity(text, target)
		if got.Result != IdentityReview {
			t.Fatalf("result = %+v, want review: \"retraction of\" is a marker even when it opens a genuine title, and that is an accepted cost", got)
		}
		if !strings.Contains(strings.Join(got.Evidence, " "), "front matter marks a correction or comment: retraction of") {
			t.Fatalf("evidence = %v, want the correction marker named", got.Evidence)
		}
	})
	t.Run("response to is deliberately not a marker", func(t *testing.T) {
		target := work.Work{Title: "Response to Intervention in Australian Schools", Authors: []string{"Maria Gomez"}, Year: 2022}
		text := "Response to Intervention in Australian Schools\nMaria Gomez\n2022\n"
		if got := MatchIdentity(text, target); got.Result != IdentityPass {
			t.Fatalf("result = %+v, want pass: \"response to\" is deliberately absent from the marker list", got)
		}
	})
}

// A comment article can cite the requested paper's DOI past the first form
// feed rather than in its own front matter — "Comment on: <title>" clears the
// title gate, the byline names a different author, and corroboratingIdentifier
// still finds the DOI because it searches the whole document, not the
// windowed front matter. That combination was already IdentityReview before
// this guard existed, for the unrelated reason that no requested author
// appears in the front matter, so this test asserts on the correction-marker
// evidence specifically: it is the one part of the review that would silently
// stop being true if those other reasons changed, and the marker evidence is
// the seam the guard's implementation is measured against.
func TestIdentityCorrectionMarkerNamedEvenWhenAlreadyReview(t *testing.T) {
	target := work.Work{
		DOI: "10.1234/abc.9", Year: 2026,
		Title:   "Deterministic Validation of Scholarly Article Identity",
		Authors: []string{"Ada Lovelace"},
	}
	text := "Comment on: Deterministic Validation of Scholarly Article Identity\nMallory Attacker\n2026\n" +
		"This commentary discusses issues raised by the original publication.\n" +
		"\fReferences\n10.1234/abc.9\n"
	got := MatchIdentity(text, target)
	if got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "front matter marks a correction or comment: comment on") {
		t.Fatalf("evidence = %v, want the correction marker named even though the verdict was already review", got.Evidence)
	}
}

// pdftotext routinely glues a running header and a page number onto the
// first text line of a page, because the header, the page number, and the
// erratum's own heading all sit in the same horizontal band and extraction
// concatenates whatever it read left to right — "J Sensor Syst 2025;12:1
// Erratum: <title>". Detection used to be an exact line prefix after
// strings.TrimSpace, so this glued line matched no entry in correctionMarkers
// and the erratum reached IdentityPass on nothing but "exact normalized DOI
// match: 10.1234/cal.7" — the escape a reviewer demonstrated with an
// executed MatchIdentity call. Splitting the line into segments on the
// double space extraction leaves behind restores the erratum heading to its
// own segment, where the ordinary prefix test can see it again.
func TestIdentityCorrectionMarkerSurvivesAGluedRunningHeader(t *testing.T) {
	target := work.Work{
		DOI: "10.1234/cal.7", Year: 2025,
		Title:   "Sensor Network Calibration Under Adverse Weather",
		Authors: []string{"Priya Natarajan"},
	}
	erratum := "J Sensor Syst 2025;12:1  Erratum: Sensor Network Calibration Under Adverse Weather\n" +
		"Bob Clarke\n2025\ndoi:10.1234/cal.7\n"
	got := MatchIdentity(erratum, target)
	if got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: a glued running header must not hide the erratum marker", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "front matter marks a correction or comment: erratum") {
		t.Fatalf("evidence = %v, want the correction marker named", got.Evidence)
	}
}

// The same escape with a bare page number standing in for the running
// header — "1  Erratum: <title>" — is the shape a single-column journal
// leaves behind instead of a full masthead, and needs the same segmentation
// to recover.
func TestIdentityCorrectionMarkerSurvivesAGluedPageNumber(t *testing.T) {
	target := work.Work{
		DOI: "10.1234/cal.7", Year: 2025,
		Title:   "Sensor Network Calibration Under Adverse Weather",
		Authors: []string{"Priya Natarajan"},
	}
	erratum := "1  Erratum: Sensor Network Calibration Under Adverse Weather\nBob Clarke\n2025\ndoi:10.1234/cal.7\n"
	got := MatchIdentity(erratum, target)
	if got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: a glued page number must not hide the erratum marker", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "front matter marks a correction or comment: erratum") {
		t.Fatalf("evidence = %v, want the correction marker named", got.Evidence)
	}
}

// wideGapSegments only recognized a run of two or more ASCII spaces as a
// join, so a running header glued to the erratum heading by a single TAB, or
// by a run of U+00A0 no-break spaces — pdftotext's other two ways of
// representing the same column break — defeated it exactly like the
// exact-prefix test did before the double-space fix above. Both bytes are
// real: 28 of 632 cached documents in the operator's reference library carry
// a literal tab, and one carries a run of no-break spaces. Before
// wideGapSegments recognised anything but two-or-more plain spaces, each of
// the first two cases below reached IdentityPass with nothing but "exact
// normalized DOI match" as evidence — the correction marker was right there
// on the page and invisible to the guard. The third case is the boundary
// that makes the other two meaningful: a single plain ASCII space must stay
// inert, or "applied a Bonferroni correction for multiple comparisons"
// starts tripping the same rule.
func TestIdentityCorrectionMarkerSurvivesATabOrNoBreakSpaceGlue(t *testing.T) {
	target := work.Work{
		DOI: "10.1234/cal.7", Year: 2025,
		Title:   "Sensor Network Calibration Under Adverse Weather",
		Authors: []string{"Priya Natarajan"},
	}
	t.Run("glued by a single tab", func(t *testing.T) {
		erratum := "J Sensor Syst 2025;12:1\tErratum: Sensor Network Calibration Under Adverse Weather\n" +
			"Bob Clarke\n2025\ndoi:10.1234/cal.7\n"
		got := MatchIdentity(erratum, target)
		if got.Result != IdentityReview {
			t.Fatalf("result = %+v, want review: a single tab must join a running header to the marker exactly like two spaces", got)
		}
		if !strings.Contains(strings.Join(got.Evidence, " "), "front matter marks a correction or comment: erratum") {
			t.Fatalf("evidence = %v, want the correction marker named", got.Evidence)
		}
	})
	t.Run("glued by two no-break spaces", func(t *testing.T) {
		erratum := "J Sensor Syst 2025;12:1\u00a0\u00a0Erratum: Sensor Network Calibration Under Adverse Weather\n" +
			"Bob Clarke\n2025\ndoi:10.1234/cal.7\n"
		got := MatchIdentity(erratum, target)
		if got.Result != IdentityReview {
			t.Fatalf("result = %+v, want review: no-break spaces must join a running header to the marker exactly like ASCII spaces", got)
		}
		if !strings.Contains(strings.Join(got.Evidence, " "), "front matter marks a correction or comment: erratum") {
			t.Fatalf("evidence = %v, want the correction marker named", got.Evidence)
		}
	})
	t.Run("a single plain space stays inert", func(t *testing.T) {
		plainSpaceTarget := work.Work{Title: "Statistical Power Analysis for Clinical Trials", Authors: []string{"Alice Smith"}, Year: 2024}
		text := "Statistical Power Analysis for Clinical Trials\nAlice Smith\n2024\n" +
			"We applied a Bonferroni correction for multiple comparisons across all endpoints.\n"
		if got := MatchIdentity(text, plainSpaceTarget); got.Result != IdentityPass {
			t.Fatalf("result = %+v, want pass: a single ASCII space must never widen into a gap", got)
		}
	})
}

// correctionMarkerIn used to split the byline window into lines before any
// typographic folding ran, while its sibling bylineSegments folds first
// precisely because pdftotext hyphenates justified text across a line wrap.
// A marker broken the same way — "Errat-\num: <title>" — is one word the
// text layer split across the wrap, not two, and splitting on the raw "\n"
// before folding left each half on its own line where no prefix test could
// see either.
func TestIdentityFoldsAHyphenatedCorrectionMarker(t *testing.T) {
	target := work.Work{
		DOI: "10.1234/cal.7", Year: 2025,
		Title:   "Sensor Network Calibration Under Adverse Weather",
		Authors: []string{"Priya Natarajan"},
	}
	erratum := "Errat-\num: Sensor Network Calibration Under Adverse Weather\nBob Clarke\n2025\ndoi:10.1234/cal.7\n"
	got := MatchIdentity(erratum, target)
	if got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: a hyphenated marker must be folded before the line split, not after", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "front matter marks a correction or comment: erratum") {
		t.Fatalf("evidence = %v, want the correction marker named", got.Evidence)
	}
}

// strings.TrimSpace does not strip U+FEFF: a byte-order mark some extractors
// prepend to the very first character of the document survives untouched and
// sits between the start of the line and "Erratum:", so a document opening
// with a BOM defeated the exact-prefix test exactly like a glued header did.
func TestIdentityCorrectionMarkerSurvivesALeadingByteOrderMark(t *testing.T) {
	target := work.Work{
		DOI: "10.1234/cal.7", Year: 2025,
		Title:   "Sensor Network Calibration Under Adverse Weather",
		Authors: []string{"Priya Natarajan"},
	}
	erratum := "\ufeffErratum: Sensor Network Calibration Under Adverse Weather\nBob Clarke\n2025\ndoi:10.1234/cal.7\n"
	got := MatchIdentity(erratum, target)
	if got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: a leading byte-order mark must not hide the erratum marker", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "front matter marks a correction or comment: erratum") {
		t.Fatalf("evidence = %v, want the correction marker named", got.Evidence)
	}
}

// correctionMarkers was missing the plural "errata", the heading a journal
// uses for a single notice that covers more than one correction, so this
// shape matched no marker at all before the plural was added.
func TestIdentityCorrectionMarkersIncludeErrata(t *testing.T) {
	target := work.Work{
		DOI: "10.1234/cal.7", Year: 2025,
		Title:   "Sensor Network Calibration Under Adverse Weather",
		Authors: []string{"Priya Natarajan"},
	}
	erratum := "Errata: Sensor Network Calibration Under Adverse Weather\nBob Clarke\n2025\ndoi:10.1234/cal.7\n"
	got := MatchIdentity(erratum, target)
	if got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: \"errata\" must be recognised alongside \"erratum\"", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "front matter marks a correction or comment: errata") {
		t.Fatalf("evidence = %v, want the errata marker named", got.Evidence)
	}
}

// A long copyright line or a repeated running header before the title can
// push an erratum's own heading well past the 1 KiB front-matter window the
// DOI rule reads, while it still sits inside the wider byline window a
// dozen-author paper legitimately needs. Correction-marker detection used to
// scan only the front-matter window, so a marker at this depth was invisible
// to it even after the segmentation fix above. The offset assertion below
// keeps the fixture honest about which window it exercises — if the padding
// arithmetic ever drifted the marker back inside the front matter, or past
// the byline window entirely, this test would stop proving anything and the
// Fatalf catches that rather than silently passing for the wrong reason.
func TestIdentityCorrectionMarkerReachesPastTheFrontMatter(t *testing.T) {
	target := work.Work{
		DOI: "10.1234/cal.7", Year: 2025,
		Title:   "Sensor Network Calibration Under Adverse Weather",
		Authors: []string{"Priya Natarajan"},
	}
	header := "Sensor Network Calibration Under Adverse Weather\nPriya Natarajan\n2025\n"
	padding := strings.Repeat("Field measurements were logged across the deployment window. ", 24)
	marker := "Erratum: Sensor Network Calibration Under Adverse Weather\n"
	// The newline matters: without it the marker would sit mid-sentence after a
	// single space, which segmentation deliberately refuses to split — the same
	// rule that keeps "a Bonferroni correction for multiple comparisons" inert.
	text := header + padding + "\n" + marker + "doi:10.1234/cal.7\n"
	offset := strings.Index(text, "Erratum:")
	if offset < identityFrontMatterBytes || offset >= identityBylineBytes {
		t.Fatalf("fixture drifted: marker sits at byte %d, want inside [%d, %d) — past the front matter and inside the byline window", offset, identityFrontMatterBytes, identityBylineBytes)
	}
	got := MatchIdentity(text, target)
	if got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: a marker past the front matter but inside the byline window must still be seen", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "front matter marks a correction or comment: erratum") {
		t.Fatalf("evidence = %v, want the correction marker named", got.Evidence)
	}
}

// identityWindow used to cut text at the first form feed unconditionally, so
// a document whose extracted text opens with one — a blank cover leaf, which
// a publisher inserts as an unnumbered title page — produced an EMPTY
// front-matter, byline, and page-one window. Every rule that reads one of
// those windows went blind, and this document did not even reach a
// marker-named review: it parked for the unrelated reason that its title
// tokens "matched only outside the front matter", which happened to be true
// but was coincidence rather than the guard doing its job. Trimming a
// leading form feed before cutting at the next one restores the intended
// window and makes the marker itself the reason for the park.
func TestIdentityCorrectionMarkerSurvivesALeadingFormFeed(t *testing.T) {
	target := work.Work{
		DOI: "10.1234/cal.7", Year: 2025,
		Title:   "Sensor Network Calibration Under Adverse Weather",
		Authors: []string{"Priya Natarajan"},
	}
	erratum := "\fErratum: Sensor Network Calibration Under Adverse Weather\nBob Clarke\n2025\ndoi:10.1234/cal.7\n"
	got := MatchIdentity(erratum, target)
	if got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: a leading form feed must not blind every window", got)
	}
	evidence := strings.Join(got.Evidence, " ")
	if !strings.Contains(evidence, "front matter marks a correction or comment: erratum") {
		t.Fatalf("evidence = %v, want the marker named rather than a park for an unrelated reason", got.Evidence)
	}
	if strings.Contains(evidence, "title tokens matched only outside the front matter") {
		t.Fatalf("evidence = %v, want the marker itself to explain the park, not a blind window", got.Evidence)
	}
}

// This exact shape is in the operator's 679-document library: a Springer
// book chapter's own page one carries a footnote reading "Erratum to this
// chapter is available at 10.1007/978-3-319-57379-3_20" — a pointer to a
// correction published SEPARATELY, printed on the very chapter it corrects,
// not a self-declaration that this chapter IS the correction. Widening the
// scan window to 2 KiB puts this footnote in reach for the first time, and
// without the pointer-phrase exclusion its "Erratum to this chapter" prefix
// would satisfy the bare "erratum" marker before the longer, more specific
// phrase ever got a chance to rule it out — parking the very document the
// operator asked for.
//
// The fixture used to join the body to this footnote with a trailing space
// and no newline, so the footnote was never its own line: correctionMarkerIn
// splits on "\n" before it looks for a wide gap, and a single space is not a
// gap, so the footnote stayed glued to the end of the preceding sentence as
// one long segment whose PREFIX was still ordinary prose — the pointer-phrase
// exclusion this test claims to defend was never even consulted. Proof:
// emptying correctionPointerPhrases left the old fixture green. A newline
// before the footnote, not a trailing space, is the fix, and both assertions
// below are needed to keep it honest: the first proves the pointer stays
// inert with no marker evidence at all, and the second proves the exclusion
// is doing real work rather than the marker never having been reached — the
// SAME fixture with the pointer reworded one word over, as a self-declaration
// ("Erratum to this chapter: <title>"), must cap. Emptying
// correctionPointerPhrases now turns the first assertion red, where before
// the newline fix it did not.
func TestIdentityCorrectionPointerToAnotherWorkDoesNotPark(t *testing.T) {
	target := work.Work{
		DOI: "10.1007/978-3-319-57379-3_19", Year: 2018,
		Title:   "Synaptic Plasticity in the Developing Cortex",
		Authors: []string{"Elena Ruiz"},
	}
	header := "Synaptic Plasticity in the Developing Cortex\nElena Ruiz\n2018\ndoi:10.1007/978-3-319-57379-3_19\n"
	body := strings.Repeat("The developing cortex undergoes extensive synaptic remodeling during this period. ", 21) +
		"Additional histological analysis was performed on this tissue."
	tail := "Layer V pyramidal neurons showed reduced dendritic complexity in the sample.\n"

	pointer := "Erratum to this chapter is available at 10.1007/978-3-319-57379-3_20\n"
	pointerText := header + body + "\n" + pointer + tail
	if offset := strings.Index(pointerText, "Erratum"); offset < identityFrontMatterBytes || offset >= identityBylineBytes {
		t.Fatalf("fixture drifted: footnote sits at byte %d, want inside [%d, %d) — past the front matter and inside the byline window", offset, identityFrontMatterBytes, identityBylineBytes)
	}
	got := MatchIdentity(pointerText, target)
	if got.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass: a pointer to an erratum published elsewhere must not park the corrected chapter itself", got)
	}
	if strings.Contains(strings.Join(got.Evidence, " "), "front matter marks a correction or comment") {
		t.Fatalf("evidence = %v, want no correction marker named: the pointer phrase must suppress it entirely", got.Evidence)
	}

	selfDeclared := "Erratum to this chapter: Synaptic Plasticity in the Developing Cortex\n"
	selfText := header + body + "\n" + selfDeclared + tail
	if offset := strings.Index(selfText, "Erratum"); offset < identityFrontMatterBytes || offset >= identityBylineBytes {
		t.Fatalf("fixture drifted: heading sits at byte %d, want inside [%d, %d) — past the front matter and inside the byline window", offset, identityFrontMatterBytes, identityBylineBytes)
	}
	got = MatchIdentity(selfText, target)
	if got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: a colon after the pointer phrase makes it a self-declaration, one word away from the pointer above", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "front matter marks a correction or comment: erratum") {
		t.Fatalf("evidence = %v, want the correction marker named", got.Evidence)
	}
}

// The pointer-phrase exclusion above is anchored on "erratum to this
// chapter"; publishers use "article" and "paper" just as often, and the bit
// that tells a pointer from a self-declaration is the same in every case:
// whether a colon follows the phrase. "Erratum to this article is available
// at 10.1007/…" points at a correction published elsewhere; "Erratum to this
// article: <title>" is the article announcing its own. Both are real
// publisher conventions, and the colon is the only thing that separates them.
func TestIdentityErratumPointerRequiresNoColonToStayInert(t *testing.T) {
	target := work.Work{
		Title:   "Sensor Network Calibration Under Adverse Weather",
		Authors: []string{"Priya Natarajan"},
		Year:    2025,
	}
	header := "Sensor Network Calibration Under Adverse Weather\nPriya Natarajan\n2025\n"
	t.Run("pointer without a colon stays inert", func(t *testing.T) {
		text := header + "Erratum to this article is available at 10.1007/xyz.9\n"
		got := MatchIdentity(text, target)
		if got.Result != IdentityPass {
			t.Fatalf("result = %+v, want pass: a pointer with no colon names a correction published elsewhere", got)
		}
		if strings.Contains(strings.Join(got.Evidence, " "), "front matter marks a correction or comment") {
			t.Fatalf("evidence = %v, want no correction marker named", got.Evidence)
		}
	})
	t.Run("self-declaration with a colon caps", func(t *testing.T) {
		text := header + "Erratum to this article: Sensor Network Calibration Under Adverse Weather\n"
		got := MatchIdentity(text, target)
		if got.Result != IdentityReview {
			t.Fatalf("result = %+v, want review: a colon after the pointer phrase makes it a self-declaration heading", got)
		}
		if !strings.Contains(strings.Join(got.Evidence, " "), "front matter marks a correction or comment: erratum") {
			t.Fatalf("evidence = %v, want the correction marker named", got.Evidence)
		}
	})
}

// "…applied a Bonferroni correction for multiple comparisons…" is ordinary
// statistics prose, not a correction notice, and this copy sits past the old
// 1 KiB front-matter cutoff — proving the widened byline window does not
// turn a real sentence into a false park. Segmentation only splits on runs
// of two or more spaces, the shape extraction leaves behind when it glues
// unrelated content together; a single space is left alone deliberately, so
// this sentence stays one segment and its prefix is the whole sentence,
// which matches no marker.
func TestIdentityBonferroniCorrectionStaysOneSegment(t *testing.T) {
	target := work.Work{Title: "Statistical Power Analysis for Clinical Trials", Authors: []string{"Alice Smith"}, Year: 2024}
	header := "Statistical Power Analysis for Clinical Trials\nAlice Smith\n2024\nAbstract. "
	text := header + strings.Repeat("We measured effect sizes across all clinical sites. ", 19) +
		"\nWe applied a Bonferroni correction for multiple comparisons across all primary and secondary endpoints in this trial.\n"
	if got := MatchIdentity(text, target); got.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass: single-spaced prose must never be split into a segment that matches a marker", got)
	}
}

// The segmentation above must never soften the one verdict the guard is not
// allowed to touch: a correction notice glued to a running header exactly
// like the escape above, but printing a DIFFERENT DOI than requested, still
// names the wrong document and has to reject outright — proving the
// DOI-mismatch reject still runs, and still wins, ahead of the new
// detection path.
func TestIdentityCorrectionMarkerDetectionDoesNotSoftenAGluedHeaderDOIMismatch(t *testing.T) {
	target := work.Work{
		DOI: "10.1234/cal.7", Year: 2025,
		Title:   "Sensor Network Calibration Under Adverse Weather",
		Authors: []string{"Priya Natarajan"},
	}
	erratum := "J Sensor Syst 2025;12:1  Erratum: Sensor Network Calibration Under Adverse Weather\n" +
		"Bob Clarke\n2025\ndoi:10.9999/other\n"
	if got := MatchIdentity(erratum, target); got.Result != IdentityReject {
		t.Fatalf("result = %+v, want the front-matter DOI mismatch to stand even behind a glued header", got)
	}
}

// The tests below pin the printed-title requirement layered on top of the
// token gate above. Measured over 632 documents and 398,786 mismatched
// document/metadata pairs, the token gate alone let through 52 wrong
// accepts and correctly passed 586/632 documents (92.7%); requiring the
// title to be printed as a delimited line cut wrong accepts to 3, then to 2
// once the label allowance below was capped at three words, at a cost of
// correct passes falling to 559/632 (88.4%) — 27 documents newly parked,
// dominated by exactly the shapes unordered token overlap can never tell
// apart: the three families pinned first below.

// identityTitleTokens drops every word under five runes, so "How to do a
// meta-analysis" reduces to the single token {analysis} and matched 1/1
// against 8 different documents in the corpus that happened to contain the
// word "analysis" and an author whose surname the request also named. The
// printed-title line requirement is the only thing that tells this apart
// from the real paper, because the words it discards — "how", "to", "do",
// "a" — are exactly the words that make this NOT a title about
// meta-analysis. Without the printed-title check this passes on the 1/1
// token overlap alone.
func TestIdentityParksADegenerateTitleThatReducesToOneToken(t *testing.T) {
	target := work.Work{Title: "How to do a meta-analysis", Authors: []string{"Ada Lovelace"}}
	text := "Training Needs Analysis at Work\nAda Lovelace\n"
	got := MatchIdentity(text, target)
	if got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: a single surviving token must not pass on its own", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "requested title is not printed as a line in the front matter") {
		t.Fatalf("evidence = %v, want the printed-title park named", got.Evidence)
	}
}

// identityTitleTokens also drops any word one rune short of five, so "Final
// Report - Volume 3, Impacts" reduces to {final, report, volume, impacts}
// and discards the only word that tells one volume of a series from
// another. Eighteen pairs of one 17-volume government report matched each
// other at 3/4 this way. The printed-title check still requires the exact
// printed word sequence, "3" included, so it catches the mismatch — "1"
// where the request says "3" — that the token set threw away. Without the
// printed-title check this passes on the 3/4 token overlap alone.
func TestIdentityParksANumberedSeriesWhoseOnlyDiscriminatingWordIsADigit(t *testing.T) {
	target := work.Work{Title: "Final Report - Volume 3, Impacts", Authors: []string{"Committee"}}
	text := "Final Report - Volume 1, Our inquiry\nCommittee\n"
	got := MatchIdentity(text, target)
	if got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: volume 1 must not stand in for volume 3", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "requested title is not printed as a line in the front matter") {
		t.Fatalf("evidence = %v, want the printed-title park named", got.Evidence)
	}
}

// An unordered token set cannot tell a title from the same title with words
// prepended: "Update to core reporting practices in structural equation
// modeling" matched a request for "Core reporting practices in structural
// equation modeling" at 5/5 tokens. The printed-title check requires the
// phrase to BEGIN a segment (or a short label), so a phrase that starts
// mid-clause — after two words with no punctuation before it — never
// qualifies. Without the printed-title check this passes outright on 5/5.
func TestIdentityParksASupersetTitleThatSharesEveryToken(t *testing.T) {
	target := work.Work{
		Title:   "Core reporting practices in structural equation modeling",
		Authors: []string{"Ada Lovelace"}, Year: 2024,
	}
	text := "Update to core reporting practices in structural equation modeling\nAda Lovelace (2024).\n"
	got := MatchIdentity(text, target)
	if got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: a superset title must not pass on 5/5 tokens alone", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "requested title is not printed as a line in the front matter") {
		t.Fatalf("evidence = %v, want the printed-title park named", got.Evidence)
	}
}

// The shapes below are real documents the printed-title rule must keep
// passing. Token-set membership alone already passed every one of these
// before the rule existed and still does after, so each assertion checks
// the specific "printed as a line" evidence string, not only the verdict —
// a Result-only assertion would not notice the printed-title rule being
// deleted.

// A title legitimately wraps across two printed lines — the same Wiley
// byline TestIdentityCorroboratesASingleMarkedAuthor exercises via a
// page-one DOI, isolated here with no identifier at all so a regression in
// consuming a wrapped title shows up as a park rather than being masked by
// the corroboration branch.
func TestIdentityAcceptsATitleWrappedAcrossTwoLines(t *testing.T) {
	target := work.Work{
		Title:   "Antecedents and trajectories of achievement goals: A self-determination theory perspective",
		Authors: []string{"Ciani"},
	}
	text := "Antecedents and trajectories of achievement\ngoals: A self-determination theory perspective\nKeith D. Ciani\n"
	got := MatchIdentity(text, target)
	if got.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass on a title that wraps across two printed lines", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "requested title printed as a line in the front matter") {
		t.Fatalf("evidence = %v, want the printed-title line named", got.Evidence)
	}
}

// The same title, broken one word earlier by justified-text hyphenation
// exactly at the point it also wraps: "self-deter-\nmination" is one word a
// publisher split across the line break, not "deter" followed by
// "mination". Folding must run over the whole byline before ANY line is
// split into segments, or the two halves land in different segments and
// never rejoin — this fixture folds to the exact text of the wrap test
// above, so a fold-after-split regression is what this test alone catches.
func TestIdentityFoldsHyphenationBeforeSplittingTitleLines(t *testing.T) {
	target := work.Work{
		Title:   "Antecedents and trajectories of achievement goals: A self-determination theory perspective",
		Authors: []string{"Ciani"},
	}
	text := "Antecedents and trajectories of achievement\ngoals: A self-deter-\nmination theory perspective\nKeith D. Ciani\n"
	got := MatchIdentity(text, target)
	if got.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass: folding must rejoin the hyphenated word before the line split", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "requested title printed as a line in the front matter") {
		t.Fatalf("evidence = %v, want the printed-title line named", got.Evidence)
	}
}

// A publisher prints a short label ahead of the title — "Original Article:",
// a numbered heading — and titleLabelWords tolerates it two ways: consumed
// inline within one segment when a colon or period ends it, or recovered as
// its own segment when pdftotext glues it to the title with a double space,
// the same escape TestIdentityCorrectionMarkerSurvivesAGluedRunningHeader
// defends for the correction marker. Both must still pass. A rule character
// ("RESEARCH ARTICLE | <title>") closes a label the same way a colon or full
// stop does — labelTerminators lists it explicitly — because a section
// heading reaches for a pipe where a sentence citing the title never does;
// that asymmetry is what TestIdentityParksATitleQuotedOrDashedInsideACitingSentence
// depends on below, so this case guards labelTerminators from narrowing away
// a real publisher convention along with the false accept.
func TestIdentityAcceptsATitleAfterAShortPublisherLabel(t *testing.T) {
	target := work.Work{Title: "Robustness Calibration Measurement Networks", Authors: []string{"Ada Lovelace"}}
	colon := MatchIdentity("Original Article: Robustness Calibration Measurement Networks\nAda Lovelace\n", target)
	if colon.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass after a colon-terminated label", colon)
	}
	if !strings.Contains(strings.Join(colon.Evidence, " "), "requested title printed as a line in the front matter") {
		t.Fatalf("evidence = %v, want the printed-title line named", colon.Evidence)
	}
	numbered := MatchIdentity("1.  Robustness Calibration Measurement Networks\nAda Lovelace\n", target)
	if numbered.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass after a numbered heading glued by a double space", numbered)
	}
	if !strings.Contains(strings.Join(numbered.Evidence, " "), "requested title printed as a line in the front matter") {
		t.Fatalf("evidence = %v, want the printed-title line named", numbered.Evidence)
	}
	piped := MatchIdentity("RESEARCH ARTICLE | Robustness Calibration Measurement Networks\nAda Lovelace\n", target)
	if piped.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass after a pipe-terminated section label", piped)
	}
	if !strings.Contains(strings.Join(piped.Evidence, " "), "requested title printed as a line in the front matter") {
		t.Fatalf("evidence = %v, want the printed-title line named", piped.Evidence)
	}
}

// The title's end edge accepts punctuation as well as a line end, so a
// title followed by its authors on the SAME printed line — the exact shape
// TestMatchIdentity already relies on for its own title-match case — must
// still count as printed rather than being read as a title with authors
// appended to it.
func TestIdentityAcceptsATitleFollowedByAuthorsOnTheSameLine(t *testing.T) {
	target := work.Work{Title: "Deterministic Validation of Scholarly Article Identity", Authors: []string{"Ada Lovelace"}, Year: 2026}
	text := "Deterministic Validation of Scholarly Article Identity. Ada Lovelace (2026)."
	got := MatchIdentity(text, target)
	if got.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass: punctuation closes the title as well as a line end", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "requested title printed as a line in the front matter") {
		t.Fatalf("evidence = %v, want the printed-title line named", got.Evidence)
	}
}

// pdftotext glues a running head and page number onto the title's own
// line — "J Sensor Syst 2025;12:1  <title>" — the same escape
// TestIdentityCorrectionMarkerSurvivesAGluedRunningHeader defends for the
// correction marker. Token-set membership never needed this recovery since
// it ignores line structure entirely; only the printed-title line check
// does, and without wide-gap segmentation the label allowance's three-word
// cap refuses a five-word running head and parks this document.
func TestIdentityRecoversATitleGluedToARunningHead(t *testing.T) {
	target := work.Work{Title: "Sensor Network Calibration Under Adverse Weather", Authors: []string{"Priya Natarajan"}, Year: 2025}
	text := "J Sensor Syst 2025;12:1  Sensor Network Calibration Under Adverse Weather\nPriya Natarajan\n2025\n"
	got := MatchIdentity(text, target)
	if got.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass: wide-gap segmentation must recover the title from a glued running head", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "requested title printed as a line in the front matter") {
		t.Fatalf("evidence = %v, want the printed-title line named", got.Evidence)
	}
}

// The label allowance is capped at titleLabelWords (3) rather than left
// unbounded because the other shape ahead of a genuine delimited title is a
// sentence CITING it: "We extend and update the earlier Core reporting
// practices in structural equation modeling, which …" is the shape of the
// two wrong accepts that still survive this rule in the corpus. Six words
// and no punctuation precede the title here, so it must not count as
// printed even though the title itself is a perfect 5/5 token match.
func TestIdentityCapsTheLabelAllowanceAtThreeWords(t *testing.T) {
	target := work.Work{
		Title:   "Core reporting practices in structural equation modeling",
		Authors: []string{"Ada Lovelace"}, Year: 2024,
	}
	text := "We extend and update the earlier Core reporting practices in structural equation modeling, " +
		"which we call CRP-2.\nAda Lovelace (2024).\n"
	got := MatchIdentity(text, target)
	if got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: a citing sentence must not count as the label allowance", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "requested title is not printed as a line in the front matter") {
		t.Fatalf("evidence = %v, want the printed-title park named", got.Evidence)
	}
}

// The next two shapes are real documents that word-by-word matching still
// parked even after the printed-title line rule above landed, because the
// printed words were not the same TOKENS as the requested title even though
// they were the same CHARACTERS. Comparing the title and the printed run as
// one concatenated character stream — identityTitlePhrase drops the spaces
// between words for exactly this reason — recovers both without loosening
// either edge of the rule. Both are among the documents behind the
// 632-document corpus's correct-pass count rising from 559 to 564.

// A real paper's text layer read "PsychologicalSafety\nand
// LearningBehavior\nin WorkTeams" — the interior space inside three
// consecutive words silently dropped, wrapped across three printed lines —
// for a catalogue title of "Psychological safety and learning behavior in
// work teams" that is otherwise byte-identical. Comparing segment.words[i]
// against the title's own word at that position fails immediately:
// "psychologicalsafety" is not "psychological". Concatenating both sides
// before comparing tolerates the missing spaces without caring where inside
// the run they went missing.
func TestIdentityAcceptsATitleWhoseInteriorSpacesWereLostAcrossAWrap(t *testing.T) {
	target := work.Work{
		Title:   "Psychological safety and learning behavior in work teams",
		Authors: []string{"Edmondson"}, Year: 1999,
	}
	text := "PsychologicalSafety\nand LearningBehavior\nin WorkTeams\nAmy Edmondson\nHarvard University\n\n" +
		"This paper presents a model of team learning and psychological safety, examining how safety " +
		"shapes learning behavior across work teams."
	got := MatchIdentity(text, target)
	if got.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass: a concatenated match must survive interior spaces the text layer dropped", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "requested title printed as a line in the front matter") {
		t.Fatalf("evidence = %v, want the printed-title line named", got.Evidence)
	}
}

// Another real paper printed "TRANSTHEORETICAL THERAPY: TOWARD A MORE
// INTEGRATIVE\nMODEL OF CHANGE1" for a catalogue title ending "...model of
// change": the text layer welded a footnote digit onto the title's own last
// word. titleRunMatches now accepts a final printed word that is the
// remaining title text followed only by digits, and treats those digits as
// closing the clause the way punctuation would — the match must still begin
// at the segment carrying "TRANSTHEORETICAL", not merely tolerate a trailing
// digit anywhere.
func TestIdentityAcceptsATitleWithAFootnoteDigitWeldedToItsLastWord(t *testing.T) {
	target := work.Work{
		Title:   "Transtheoretical therapy: Toward a more integrative model of change",
		Authors: []string{"Prochaska"}, Year: 1982,
	}
	text := "PSYCHOTHERAPY: THEORY, RESEARCH AND PRACTICE\nVOLUME 19, # 3 , FALL, 1982\n\n" +
		"TRANSTHEORETICAL THERAPY: TOWARD A MORE INTEGRATIVE\nMODEL OF CHANGE1\n" +
		"JAMES O. PROCHASKA*\nUniversity of Rhode Island\n"
	got := MatchIdentity(text, target)
	if got.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass: a flattened footnote digit must not block the title's last word", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "requested title printed as a line in the front matter") {
		t.Fatalf("evidence = %v, want the printed-title line named", got.Evidence)
	}
}

// The start edge used to accept ANY punctuation ahead of the title, so a
// sentence quoting another paper's title — one of the most common citation
// styles there is — satisfied it as easily as a real label: `We cite "Core
// reporting practices in structural equation modeling" for guidance.`
// reached IdentityPass on a document that is not the requested paper at all,
// the exact false accept the printed-title rule exists to refuse. Both this
// and the em-dash form below were real wrong accepts in the corpus.
// labelTerminators now narrows the start edge to a colon, full stop, or rule
// — ":.|•·" — so neither the quote nor the dash preceding the title counts
// as a label. The end edge is untouched and still accepts any punctuation,
// including the quote and the dash trailing the title here, because "<title>.
// Authors (Year)." is a real shape TestIdentityAcceptsATitleFollowedByAuthorsOnTheSameLine
// already depends on.
func TestIdentityParksATitleQuotedOrDashedInsideACitingSentence(t *testing.T) {
	target := work.Work{
		Title:   "Core reporting practices in structural equation modeling",
		Authors: []string{"Ada Lovelace"}, Year: 2024,
	}
	quoted := MatchIdentity(
		"We cite \"Core reporting practices in structural equation modeling\" for guidance.\nAda Lovelace (2024).\n",
		target)
	if quoted.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: a quoted citation of the title must not count as printed", quoted)
	}
	if !strings.Contains(strings.Join(quoted.Evidence, " "), "requested title is not printed as a line in the front matter") {
		t.Fatalf("evidence = %v, want the printed-title park named", quoted.Evidence)
	}
	dashed := MatchIdentity(
		"We cite \u2014 Core reporting practices in structural equation modeling \u2014 for guidance.\nAda Lovelace (2024).\n",
		target)
	if dashed.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: an em-dash-set citation of the title must not count as printed", dashed)
	}
	if !strings.Contains(strings.Join(dashed.Evidence, " "), "requested title is not printed as a line in the front matter") {
		t.Fatalf("evidence = %v, want the printed-title park named", dashed.Evidence)
	}
}
