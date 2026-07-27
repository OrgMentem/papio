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

func TestMatchIdentityHonorsTitleThreshold(t *testing.T) {
	target := work.Work{Title: "Quantum Networks Robustness Calibration Measurement", Authors: []string{"Lovelace"}, Year: 2026}
	text := "Quantum networks robustness. Lovelace (2026)."
	if got := MatchIdentityWithThreshold(text, target, 0.8); got.Result != IdentityReject {
		t.Fatalf("80%% threshold result = %+v, want reject", got)
	}
	if got := MatchIdentityWithThreshold(text, target, 0.6); got.Result != IdentityPass {
		t.Fatalf("60%% threshold result = %+v, want pass", got)
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
