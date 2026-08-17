// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package identitycorpus

import (
	"strings"
	"testing"

	"papio/internal/pdf"
	"papio/internal/work"
)

func TestMeasureExcludesSecondaryAttachments(t *testing.T) {
	primary := Document{
		Key:  "PRI00001",
		Text: "Primary article text about calibration methods.",
		Work: work.Work{Title: "Primary Study", Authors: []string{"Ada Lovelace"}, Year: 2021, DOI: "10.5555/test.2021.100"},
	}
	secondary := Document{
		Key:       "SUP00001",
		Secondary: true,
		Text:      "Supplementary information for the primary study.",
		Work:      primary.Work,
	}
	other := Document{
		Key:  "OTH00001",
		Text: "Unrelated methods note.",
		Work: work.Work{Title: "Other Study", Authors: []string{"Grace Hopper"}, Year: 2020, DOI: "10.5555/test.2020.200"},
	}

	report := Measure([]Document{primary, secondary, other})
	if report.Documents != 2 {
		t.Fatalf("Documents = %d, want 2 (secondary excluded)", report.Documents)
	}
	if report.CorrectPairs != 2 {
		t.Fatalf("CorrectPairs = %d, want 2", report.CorrectPairs)
	}
	if report.MismatchedPairs != 2 {
		t.Fatalf("MismatchedPairs = %d, want 2 (one cross-pair each way between primaries)", report.MismatchedPairs)
	}
}

func TestMarkerGateArmsTerminateAtExpectedGates(t *testing.T) {
	alpha, _, beta, gamma := fixtureDocs()
	docs := []Document{alpha, beta, gamma}
	b := testBuilder(docs)

	cases := []struct {
		arm      Arm
		wantGate string
		build    func(Document, int) (Pool, bool)
	}{
		{ArmMarkerCorrection, string(pdf.GateCorrection), b.buildMarkerCorrectionPool},
		{ArmMarkerNonArticle, string(pdf.GateNonArticle), b.buildMarkerNonArticlePool},
	}

	for _, tc := range cases {
		t.Run(string(tc.arm), func(t *testing.T) {
			p, ok := tc.build(alpha, 2)
			if !ok {
				t.Fatal("marker-gate pool could not be built from a DOI-less fixture with title, authors and year")
			}
			if len(pdf.FrontMatterDOIs(p.text)) != 0 {
				t.Fatalf("synthesized document is inadmissible: front-matter DOIs %v", pdf.FrontMatterDOIs(p.text))
			}
			if !p.TargetAbsent || len(p.TrueKeys) != 0 {
				t.Fatal("marker-gate pool must be target-absent with an empty class")
			}
			if !strings.HasPrefix(p.Provenance, "adjudicated:marker-gate synthesis") {
				t.Fatalf("provenance %q must record marker-gate synthesis", p.Provenance)
			}
			if len(p.Candidates) != 2 {
				t.Fatalf("pool size = %d, want 2", len(p.Candidates))
			}
			if p.Candidates[0].Key != alpha.Key {
				t.Fatalf("first candidate %q, want referred-to work %q", p.Candidates[0].Key, alpha.Key)
			}

			trial := b.evaluatePool(tc.arm, 2, p)
			if trial.TerminalGate != tc.wantGate {
				t.Fatalf("terminal gate = %q, want %q", trial.TerminalGate, tc.wantGate)
			}
			if trial.ChosenKey != "" {
				t.Fatalf("selector bound %q, want abstention", trial.ChosenKey)
			}
			if trial.Outcome != BindAbstainOK {
				t.Fatalf("outcome = %s, want %s", trial.Outcome, BindAbstainOK)
			}
		})
	}
}

func TestMeasureCandidateSetsMarkerGateCoverage(t *testing.T) {
	alpha, _, beta, gamma := fixtureDocs()
	report := MeasureCandidateSets([]Document{alpha, beta, gamma}, CandidateOptions{
		Seed:      11,
		PoolSizes: []int{2},
		Arms:      []Arm{ArmMarkerCorrection, ArmMarkerNonArticle},
	})

	gateCounts := map[string]int{}
	for _, bucket := range report.GatesObserved {
		gateCounts[bucket.Label] = bucket.Count
	}
	if gateCounts[string(pdf.GateCorrection)] == 0 {
		t.Fatal("correction-marker gate observed 0 trials")
	}
	if gateCounts[string(pdf.GateNonArticle)] == 0 {
		t.Fatal("non-article-marker gate observed 0 trials")
	}

	for _, res := range report.Results {
		if !res.TargetAbsent {
			t.Errorf("%s target-present cell should not be measured", res.Arm)
		}
		if res.Counts.Wrong != 0 {
			t.Errorf("%s produced %d wrong binds", res.Arm, res.Counts.Wrong)
		}
		if res.Pools == 0 || !res.Representative {
			t.Errorf("%s: Pools=%d Representative=%v, want a filled representative cell", res.Arm, res.Pools, res.Representative)
		}
	}

	rendered := report.Render()
	if strings.Contains(rendered, "no trial in this run reached correction-marker") ||
		strings.Contains(rendered, "no trial in this run reached non-article-marker") {
		t.Error("Render still reports marker gates as untested after synthesis arms ran")
	}
}
