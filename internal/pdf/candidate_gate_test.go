// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/work"
)

type manifestCandidate struct {
	Key   string    `json:"key"`
	Work  work.Work `json:"work"`
	Bound []string  `json:"bound"`
}

type manifestCase struct {
	ID                   string              `json:"id"`
	Document             string              `json:"document"`
	RiskFamily           string              `json:"risk_family"`
	Candidates           []manifestCandidate `json:"candidates"`
	Truth                *string             `json:"truth"`
	CanonicalEquivalence []string            `json:"canonical_equivalence"`
	VetoWindow           bool                `json:"veto_window"`
	VetoNote             string              `json:"veto_note,omitempty"`
}

type manifestFile struct {
	Cases []manifestCase `json:"cases"`
}

func isEquivalent(key string, truth *string, equiv []string) bool {
	if truth != nil && key == *truth {
		return true
	}
	for _, e := range equiv {
		if e == key {
			return true
		}
	}
	return false
}

// TestCandidateSelectionGate is the Phase 2 measurement gate. It iterates the
// labeled semantic corpus under testdata/candidatecorpus/, runs
// SelectAutoBindCandidate per case, and classifies each outcome as
// wrong-accept, correct-accept, correct-abstain, or missed-accept. The
// headline number is wrong-accepts — zero is required to ship auto-bind.
// Missed-accepts are conservative abstentions and do not fail the build.
// Veto-window cases are reported separately, not folded into the main totals,
// measuring the residual left by the deliberately narrow 1 KiB blind window.
func TestCandidateSelectionGate(t *testing.T) {
	t.Logf("candidate binding rule: %s", CandidateBindingRule)

	manifestPath := filepath.Join("testdata", "candidatecorpus", "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest %s: %v", manifestPath, err)
	}
	var mf manifestFile
	if err := json.Unmarshal(data, &mf); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if len(mf.Cases) == 0 {
		t.Fatalf("manifest has no cases")
	}

	type tally struct {
		total          int
		wrongAccept    int
		correctAccept  int
		correctAbstain int
		missedAccept   int
	}
	var mainT, vetoT tally
	var wrongDetails []string

	for _, c := range mf.Cases {
		docPath := filepath.Join("testdata", "candidatecorpus", c.Document)
		excerptBytes, err := os.ReadFile(docPath)
		if err != nil {
			t.Fatalf("case %q: read document %s: %v", c.ID, docPath, err)
		}
		excerpt := string(excerptBytes)

		candidates := make([]BindCandidate, 0, len(c.Candidates))
		for _, mc := range c.Candidates {
			candidates = append(candidates, BindCandidate{
				Key:   mc.Key,
				Work:  mc.Work,
				Bound: append([]string(nil), mc.Bound...),
			})
		}

		winner, ok, _ := SelectAutoBindCandidate(excerpt, candidates)

		cur := &mainT
		if c.VetoWindow {
			cur = &vetoT
		}
		cur.total++

		switch {
		case ok:
			if c.Truth == nil {
				cur.wrongAccept++
				wrongDetails = append(wrongDetails, fmt.Sprintf("case %q (%s): wrong-accept choosing %q but truth is absent (evidence %v)", c.ID, c.RiskFamily, winner.Key, winner.Evidence))
			} else if isEquivalent(winner.Key, c.Truth, c.CanonicalEquivalence) {
				cur.correctAccept++
			} else {
				cur.wrongAccept++
				truth := "<nil>"
				if c.Truth != nil {
					truth = *c.Truth
				}
				wrongDetails = append(wrongDetails, fmt.Sprintf("case %q (%s): wrong-accept choosing %q but truth is %q equiv %v evidence %v", c.ID, c.RiskFamily, winner.Key, truth, c.CanonicalEquivalence, winner.Evidence))
			}
		default:
			if c.Truth == nil {
				cur.correctAbstain++
			} else if len(c.CanonicalEquivalence) > 0 {
				cur.correctAbstain++
			} else {
				cur.missedAccept++
			}
		}
	}

	t.Logf("gate report [%s]", CandidateBindingRule)
	t.Logf("main corpus: total=%d correct-accept=%d correct-abstain=%d missed-accept=%d wrong-accept=%d", mainT.total, mainT.correctAccept, mainT.correctAbstain, mainT.missedAccept, mainT.wrongAccept)
	if vetoT.total > 0 {
		t.Logf("veto-window (1-4 KiB residual, not folded): total=%d correct-accept=%d correct-abstain=%d missed-accept=%d wrong-accept=%d", vetoT.total, vetoT.correctAccept, vetoT.correctAbstain, vetoT.missedAccept, vetoT.wrongAccept)
		t.Logf("veto-window detail: measures the deliberately narrow 1 KiB blind window; see manifest veto_note")
	}
	totalWrong := mainT.wrongAccept + vetoT.wrongAccept
	t.Logf("headline: wrong-accepts=%d (must be 0)", totalWrong)
	if totalWrong == 0 {
		t.Logf("gate: PASS — zero wrong-accepts")
	}
	if mainT.missedAccept > 0 || vetoT.missedAccept > 0 {
		t.Logf("note: missed-accepts are conservative abstentions, not failures; coverage %d/%d main, %d/%d veto", mainT.correctAccept, mainT.correctAccept+mainT.missedAccept, vetoT.correctAccept, vetoT.correctAccept+vetoT.missedAccept)
	}

	if len(wrongDetails) > 0 {
		for _, d := range wrongDetails {
			t.Errorf("%s", d)
		}
		t.Fatalf("gate FAILED: %d wrong-accept(s) — the wrong paper would be filed under the right citation", totalWrong)
	}
}

type sentinelSpec struct {
	file         string
	description  string
	contains     []string
	candidates   []BindCandidate
	expectOk     bool
	expectWinner string
	expectReview bool
	reviewKey    string
}

func TestCandidateGateSentinels(t *testing.T) {
	cap := DetectCapability()
	if !cap.Semantic() {
		t.Logf("sentinel gate: SKIP — pdftotext unavailable, extraction toolchain missing (SKIP, not PASS)")
		t.Skip("pdftotext unavailable — sentinel PDFs committed but extraction skipped (SKIP, not PASS)")
	}

	sentinels := []sentinelSpec{
		{
			file:        "testdata/candidatecorpus/sentinels/ligature.pdf",
			description: "ligature fi/ff preserved, folded by typographicFolder",
			contains:    []string{"\uFB01", "\uFB02"},
			candidates: []BindCandidate{
				{Key: "TRUE", Work: work.Work{Title: "Classification of Difficult Workflow Instances", Authors: []string{"Ada Lovelace"}, Year: 2024, DOI: "10.5555/sentinel.lig.001"}, Bound: []string{"10.5555/sentinel.lig.001"}},
				{Key: "OTHER", Work: work.Work{Title: "Unrelated Synthetic Work", Authors: []string{"Foreign Author"}, Year: 2024, DOI: "10.5555/other.999"}, Bound: []string{"10.5555/other.999"}},
			},
			expectOk:     true,
			expectWinner: "TRUE",
		},
		{
			file:        "testdata/candidatecorpus/sentinels/hyphen_wrap.pdf",
			description: "hyphenated title wrapping across lines, folded",
			contains:    []string{"Ada Lovelace"},
			candidates: []BindCandidate{
				{Key: "TRUE", Work: work.Work{Title: "Classification of Difficult Workflow Instances", Authors: []string{"Ada Lovelace"}, Year: 2024, DOI: "10.5555/sentinel.hyph.002"}, Bound: []string{"10.5555/sentinel.hyph.002"}},
				{Key: "OTHER", Work: work.Work{Title: "Other Work", Authors: []string{"Other Person"}, Year: 2024, DOI: "10.5555/other.998"}, Bound: []string{"10.5555/other.998"}},
			},
			expectOk:     true,
			expectWinner: "TRUE",
		},
		{
			file:        "testdata/candidatecorpus/sentinels/title_wrap.pdf",
			description: "title split across multiple lines, consumed as wrapped line",
			contains:    []string{"Network Embedding With Adaptive Sampling", "for Community Detection"},
			candidates: []BindCandidate{
				{Key: "TRUE", Work: work.Work{Title: "Network Embedding With Adaptive Sampling for Community Detection", Authors: []string{"Elena Vargas", "Kenji Tanaka"}, Year: 2024, DOI: "10.5555/sentinel.wrap.003"}, Bound: []string{"10.5555/sentinel.wrap.003"}},
				{Key: "OTHER", Work: work.Work{Title: "Network Embedding With Adaptive Sampling for Link Prediction", Authors: []string{"Elena Vargas", "Kenji Tanaka"}, Year: 2024, DOI: "10.5555/other.997"}, Bound: []string{"10.5555/other.997"}},
			},
			expectOk:     true,
			expectWinner: "TRUE",
		},
		{
			file:        "testdata/candidatecorpus/sentinels/two_column_gap.pdf",
			description: "two-column layout with wide gap that can glue unrelated lines",
			contains:    []string{"Network Embedding With Adaptive Sampling for Community Detection", "Elena Vargas"},
			candidates: []BindCandidate{
				{Key: "TRUE", Work: work.Work{Title: "Network Embedding With Adaptive Sampling for Community Detection", Authors: []string{"Elena Vargas", "Kenji Tanaka"}, Year: 2024, DOI: "10.5555/sentinel.twocol.004"}, Bound: []string{"10.5555/sentinel.twocol.004"}},
				{Key: "OTHER", Work: work.Work{Title: "Unrelated Title", Authors: []string{"Foreign Author"}, Year: 2024, DOI: "10.5555/other.996"}, Bound: []string{"10.5555/other.996"}},
			},
			expectOk:     true,
			expectWinner: "TRUE",
		},
		{
			file:        "testdata/candidatecorpus/sentinels/affiliation_markers.pdf",
			description: "author affiliation markers glued to surnames",
			contains:    []string{"Rebecca Klein1"},
			candidates: []BindCandidate{
				{Key: "TRUE", Work: work.Work{Title: "Scaling Laws for Synthetic Language Models", Authors: []string{"Rebecca Klein", "Omar Haddad"}, Year: 2023, DOI: "10.5555/sentinel.affil.005"}, Bound: []string{"10.5555/sentinel.affil.005"}},
				{Key: "OTHER", Work: work.Work{Title: "Scaling Laws for Synthetic Language Models", Authors: []string{"Foreign Author"}, Year: 2023, DOI: "10.5555/other.995"}, Bound: []string{"10.5555/other.995"}},
			},
			expectOk:     true,
			expectWinner: "TRUE",
		},
		{
			file:        "testdata/candidatecorpus/sentinels/blank_cover.pdf",
			description: "blank cover leaf before page one (leading form feed trimmed)",
			contains:    []string{"\x0c", "Scaling Laws for Synthetic Language Models"},
			candidates: []BindCandidate{
				{Key: "TRUE", Work: work.Work{Title: "Scaling Laws for Synthetic Language Models", Authors: []string{"Rebecca Klein", "Omar Haddad"}, Year: 2023, DOI: "10.5555/sentinel.blank.006"}, Bound: []string{"10.5555/sentinel.blank.006"}},
				{Key: "OTHER", Work: work.Work{Title: "Other Work", Authors: []string{"Foreign Author"}, Year: 2023, DOI: "10.5555/other.994"}, Bound: []string{"10.5555/other.994"}},
			},
			expectOk:     true,
			expectWinner: "TRUE",
		},
		{
			file:        "testdata/candidatecorpus/sentinels/doi_at_2k.pdf",
			description: "document's own identifier at 2-4 KiB (inside pageOne, outside frontMatter)",
			contains:    []string{"Deep Learning for Histopathology Image Classification", "10.5555/sentinel.doi2k.007"},
			candidates: []BindCandidate{
				{Key: "TRUE", Work: work.Work{Title: "Deep Learning for Histopathology Image Classification", Authors: []string{"Clara Jensen", "Rashid Ali"}, Year: 2023, DOI: "10.5555/sentinel.doi2k.007"}, Bound: []string{"10.5555/sentinel.doi2k.007"}},
				{Key: "OTHER", Work: work.Work{Title: "Other Histopathology Work", Authors: []string{"Foreign Author"}, Year: 2023, DOI: "10.5555/other.993"}, Bound: []string{"10.5555/other.993"}},
			},
			expectOk:     true,
			expectWinner: "TRUE",
		},
		{
			file:        "testdata/candidatecorpus/sentinels/doi_after_ff.pdf",
			description: "identifier only after first form feed (page two)",
			contains:    []string{"\x0c", "10.5555/sentinel.afterff.008"},
			candidates: []BindCandidate{
				{Key: "TRUE", Work: work.Work{Title: "Federated Learning With Differential Privacy Guarantees", Authors: []string{"Ana Ruiz", "Daniel Wu"}, Year: 2024, DOI: "10.5555/sentinel.afterff.008"}, Bound: []string{"10.5555/sentinel.afterff.008"}},
				{Key: "OTHER", Work: work.Work{Title: "Other Federated Work", Authors: []string{"Foreign Author"}, Year: 2024, DOI: "10.5555/other.992"}, Bound: []string{"10.5555/other.992"}},
			},
			expectOk:     false,
			expectReview: true,
			reviewKey:    "TRUE",
		},
		{
			file:        "testdata/candidatecorpus/sentinels/dense_no_ff_past_4k.pdf",
			description: "dense page with no form feed whose identifier falls past the 4 KiB page-one cap",
			contains:    []string{"Efficient Attention Mechanisms for Long Sequences", "10.5555/sentinel.dense.009"},
			candidates: []BindCandidate{
				{Key: "TRUE", Work: work.Work{Title: "Efficient Attention Mechanisms for Long Sequences", Authors: []string{"Sofia Berg", "Hiroshi Sato"}, Year: 2023, DOI: "10.5555/sentinel.dense.009"}, Bound: []string{"10.5555/sentinel.dense.009"}},
				{Key: "OTHER", Work: work.Work{Title: "Other Attention Work", Authors: []string{"Foreign Author"}, Year: 2023, DOI: "10.5555/other.991"}, Bound: []string{"10.5555/other.991"}},
			},
			expectOk:     false,
			expectReview: true,
			reviewKey:    "TRUE",
		},
	}

	for _, s := range sentinels {
		s := s
		t.Run(filepath.Base(s.file), func(t *testing.T) {
			ctx := context.Background()
			report, err := ExtractText(ctx, s.file, cap, DefaultSemanticOptions())
			if err != nil {
				t.Fatalf("ExtractText %s: %v", s.file, err)
			}
			if report.NeedsReview {
				t.Fatalf("extraction needs review for %s: evidence %v", s.file, report.Evidence)
			}
			excerpt := report.Excerpt
			for _, want := range s.contains {
				if !strings.Contains(excerpt, want) {
					t.Fatalf("sentinel %s (%s): excerpt missing golden %q; head %q", s.file, s.description, want, excerpt[:minInt(800, len(excerpt))])
				}
			}

			if strings.Contains(s.file, "doi_at_2k") {
				if got := FrontMatterDOIs(excerpt); len(got) != 0 {
					t.Fatalf("doi_at_2k sentinel: FrontMatterDOIs should be empty (DOI past 1 KiB) but got %v", got)
				}
				pageOne := identityPageOne(excerpt)
				if !strings.Contains(pageOne, "10.5555/sentinel.doi2k.007") {
					t.Fatalf("doi_at_2k sentinel: DOI should be in pageOne window but was not; excerpt len %d", len(excerpt))
				}
			}
			if strings.Contains(s.file, "dense_no_ff") {
				pageOne := identityPageOne(excerpt)
				if strings.Contains(pageOne, "10.5555/sentinel.dense.009") {
					t.Fatalf("dense_no_ff sentinel: DOI should not be in pageOne (past 4 KiB cap) but was")
				}
			}
			if strings.Contains(s.file, "doi_after_ff") {
				pageOne := identityPageOne(excerpt)
				if strings.Contains(pageOne, "10.5555/sentinel.afterff.008") {
					t.Fatalf("doi_after_ff sentinel: DOI should not be in pageOne (after form feed) but was")
				}
			}

			winner, ok, reason := SelectAutoBindCandidate(excerpt, s.candidates)
			if ok != s.expectOk {
				t.Fatalf("sentinel %s (%s): SelectAutoBind ok=%v want %v reason %q winner %+v", s.file, s.description, ok, s.expectOk, reason, winner)
			}
			if s.expectOk {
				if winner.Key != s.expectWinner {
					t.Fatalf("sentinel %s (%s): winner %q want %q reason %q evidence %v", s.file, s.description, winner.Key, s.expectWinner, reason, winner.Evidence)
				}
				if !winner.Qualifies {
					t.Fatalf("sentinel %s (%s): winner should qualify but got %+v", s.file, s.description, winner)
				}
			} else if s.expectReview {
				q := QualifyCandidate(excerpt, bindCandidateByKey(s.candidates, s.reviewKey))
				if !q.Review {
					t.Fatalf("sentinel %s (%s): expected Review for %q but got Qualify %+v (Select reason %q)", s.file, s.description, s.reviewKey, q, reason)
				}
			}
		})
	}
}

func bindCandidateByKey(cands []BindCandidate, key string) BindCandidate {
	for _, c := range cands {
		if c.Key == key {
			return c
		}
	}
	return BindCandidate{Key: key}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
