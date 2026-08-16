// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package identitycorpus

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"papio/internal/pdf"
	"papio/internal/work"
)

// Every document below is synthetic: these tests never read a Zotero library or
// a papio store. The synthesis is not decoration — a document that can be
// auto-bound at all must print its identifier PAST the 1 KiB blind window (or it
// would never be admitted) and INSIDE page one's 4 KiB window (or gate 5 could
// never read it), so the offset is a property the fixtures have to have.

// synthText builds a page-one document in the geometry production actually
// admits. frontDOI, when non-empty, is printed in the first line and therefore
// inside the 1 KiB blind window, which is how a document is made INADMISSIBLE.
// pageOneDOI is printed past that window and inside page one, which is how a
// document is made bindable.
func synthText(title string, authors []string, year int, frontDOI, pageOneDOI string) string {
	var b strings.Builder
	if frontDOI != "" {
		fmt.Fprintf(&b, "DOI: %s\n\n", frontDOI)
	}
	b.WriteString(title + "\n\n")
	b.WriteString(strings.Join(authors, ", ") + "\n\n")
	fmt.Fprintf(&b, "Journal of Test Systems, volume 1, pages 1-20, %d\n\n", year)
	for b.Len() < conjunctionCitedOffset {
		b.WriteString(conjunctionFiller)
	}
	b.WriteString("\n")
	if pageOneDOI != "" {
		fmt.Fprintf(&b, "Article DOI: %s\n", pageOneDOI)
	}
	b.WriteString("\nAbstract\nA synthetic page for the candidate measurement harness.\n\nKeywords: measurement\n")
	return b.String()
}

// synthDoc is a document that prints its own metadata, bindable by construction.
func synthDoc(key, title string, authors []string, year int, doi string) Document {
	return Document{
		Key:  key,
		Text: synthText(title, authors, year, "", doi),
		Work: work.Work{DOI: doi, Title: title, Authors: authors, Year: year, Container: "Journal of Test Systems"},
	}
}

func candidateFor(d Document) pdf.BindCandidate {
	return pdf.BindCandidate{Key: d.Key, Work: d.Work}
}

func testBuilder(docs []Document) *builder {
	classes := buildEquivalenceClasses(docs, nil)
	byKey := make(map[string]Document, len(docs))
	for _, d := range docs {
		byKey[d.Key] = d
	}
	return &builder{
		seed:     7,
		classes:  classes,
		universe: candidateUniverse(docs, classes),
		byKey:    byKey,
		admitted: documentKeySet(docs),
	}
}

// alpha is the paper every "bindable" fixture below is built around; beta and
// gamma are unrelated works whose metadata a document about alpha satisfies no
// gate of.
func fixtureDocs() (alpha, sibling, beta, gamma Document) {
	alpha = synthDoc("AAAA", "Adaptive Sampling for Streaming Graphs",
		[]string{"Elena Vargas", "Kenji Tanaka"}, 2022, "10.5555/test.2022.501")
	// sibling is the SAME work under a different lexical spelling of the same
	// DOI: the version-of-record row a library accumulates beside its preprint.
	sibling = synthDoc("AAAB", "Adaptive Sampling for Streaming Graphs",
		[]string{"Elena Vargas", "Kenji Tanaka"}, 2022, "https://doi.org/10.5555/TEST.2022.501")
	beta = synthDoc("BBBB", "Robust Calibration of Ensemble Forecasts",
		[]string{"Marta Silva", "Andre Bouchard"}, 2019, "10.5555/test.2019.610")
	gamma = synthDoc("CCCC", "Thermal Drift in Cryogenic Detectors",
		[]string{"Piotr Nowak", "Hana Kimura"}, 2016, "10.5555/test.2016.222")
	return
}

// TestCandidateFixtureGeometry pins the fixtures themselves. If a fixture stops
// being admissible, or stops printing its identifier where gate 5 can read it,
// every outcome test below would still "pass" while measuring nothing.
func TestCandidateFixtureGeometry(t *testing.T) {
	alpha, _, _, _ := fixtureDocs()
	if dois := pdf.FrontMatterDOIs(alpha.Text); len(dois) != 0 {
		t.Fatalf("fixture is inadmissible: front-matter DOIs %v", dois)
	}
	offset := strings.Index(alpha.Text, alpha.Work.DOI)
	if offset <= 1024 {
		t.Fatalf("identifier at offset %d is inside the 1 KiB blind window", offset)
	}
	if offset >= 4096 {
		t.Fatalf("identifier at offset %d is past page one's 4 KiB cap", offset)
	}
	if strings.Contains(alpha.Text, "\f") {
		t.Fatal("fixture emits a form feed, which would truncate the page-one window")
	}
	q := pdf.QualifyCandidate(alpha.Text, candidateFor(alpha))
	if !q.Qualifies {
		t.Fatalf("fixture does not qualify against its own metadata: gate %s reason %q", q.Gate, q.Reason)
	}
}

func TestCandidateAdmission(t *testing.T) {
	alpha, _, beta, _ := fixtureDocs()
	// Same paper, same metadata, but its DOI is printed in the first line, so
	// FrontMatterDOIs is non-empty and production would never reach the selector.
	withFrontMatter := alpha
	withFrontMatter.Key = "DDDD"
	withFrontMatter.Text = synthText(alpha.Work.Title, alpha.Work.Authors, alpha.Work.Year,
		alpha.Work.DOI, alpha.Work.DOI)

	report := MeasureCandidateSets([]Document{alpha, beta, withFrontMatter}, CandidateOptions{
		Seed: 1, PoolSizes: []int{2}, Arms: []Arm{ArmRandom},
	})

	if report.LibraryDocuments != 3 {
		t.Errorf("LibraryDocuments = %d, want 3", report.LibraryDocuments)
	}
	if report.DOILessDocuments != 2 {
		t.Errorf("DOILessDocuments = %d, want 2 (the front-matter-DOI document must be excluded)", report.DOILessDocuments)
	}
	for _, res := range report.Results {
		if res.Eligible != 2 {
			t.Errorf("cell %s N=%d absent=%v: Eligible = %d, want 2", res.Arm, res.PoolSize, res.TargetAbsent, res.Eligible)
		}
	}
	// The excluded document must not appear as a measured target anywhere.
	for _, trial := range append(append([]Trial(nil), report.WrongBinds...), report.Missed...) {
		if trial.DocKey == "DDDD" {
			t.Errorf("inadmissible document DDDD was measured as a target: %+v", trial)
		}
	}
}

// TestCandidateOutcomeClassification covers all four outcomes plus the case the
// contract exists for: a bind INSIDE the equivalence class is correct, not the
// cardinal failure.
func TestCandidateOutcomeClassification(t *testing.T) {
	alpha, sibling, beta, gamma := fixtureDocs()
	docs := []Document{alpha, sibling, beta, gamma}
	b := testBuilder(docs)
	class := b.classes.class[alpha.Key]
	if !inClass(sibling.Key, class) {
		t.Fatalf("fixture precondition: sibling %s is not in alpha's class %v", sibling.Key, class)
	}

	// A document that prints alpha's front matter but no identifier at all: gate
	// 5 has nothing to corroborate, so the candidate lands in Review and the
	// selector abstains.
	noIdentifier := alpha
	noIdentifier.Key = "AAAC"
	noIdentifier.Text = synthText(alpha.Work.Title, alpha.Work.Authors, alpha.Work.Year, "", "")

	cases := []struct {
		name      string
		docText   string
		pool      Pool
		want      BindOutcome
		wantKey   string
		wantGate  pdf.CandidateGate
		reasonHas string
	}{
		{
			// alpha's own page, alpha's job present, class established.
			name:     "correct bind",
			docText:  alpha.Text,
			pool:     Pool{DocKey: alpha.Key, Candidates: []pdf.BindCandidate{candidateFor(alpha), candidateFor(beta)}, TrueKeys: class, Provenance: provenanceIdentifier},
			want:     BindCorrect,
			wantKey:  alpha.Key,
			wantGate: pdf.GateIdentifier,
		},
		{
			// The same page, declared by construction to be a DIFFERENT work —
			// an expansion of alpha, whose own job is not pending. Binding
			// alpha's job files the wrong paper under a right citation.
			name:     "wrong bind on an empty class",
			docText:  alpha.Text,
			pool:     Pool{DocKey: alpha.Key, Candidates: []pdf.BindCandidate{candidateFor(alpha), candidateFor(beta)}, TargetAbsent: true, Provenance: "adjudicated:test"},
			want:     BindWrong,
			wantKey:  alpha.Key,
			wantGate: pdf.GateIdentifier,
		},
		{
			// A bind on a class SIBLING. Under a single-key ground truth this
			// would score as the cardinal failure; it is a correct bind.
			name:     "bind inside the equivalence class",
			docText:  alpha.Text,
			pool:     Pool{DocKey: alpha.Key, Candidates: []pdf.BindCandidate{candidateFor(sibling), candidateFor(gamma)}, TrueKeys: class, Provenance: provenanceIdentifier},
			want:     BindCorrect,
			wantKey:  sibling.Key,
			wantGate: pdf.GateIdentifier,
		},
		{
			name:     "correct abstain",
			docText:  alpha.Text,
			pool:     Pool{DocKey: alpha.Key, Candidates: []pdf.BindCandidate{candidateFor(beta), candidateFor(gamma)}, TargetAbsent: true, Provenance: "adjudicated:test"},
			want:     BindAbstainOK,
			wantGate: pdf.GateAuthor,
		},
		{
			name:      "missed bind",
			docText:   noIdentifier.Text,
			pool:      Pool{DocKey: noIdentifier.Key, Candidates: []pdf.BindCandidate{candidateFor(alpha), candidateFor(beta)}, TrueKeys: class, Provenance: provenanceIdentifier},
			want:      BindMissed,
			wantGate:  pdf.GateIdentifier,
			reasonHas: "review",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.pool
			p.text = tc.docText
			trial := b.evaluatePool(ArmComposite, len(p.Candidates), p)
			if trial.Outcome != tc.want {
				t.Fatalf("outcome = %s, want %s (chosen %q reason %q gate %q)",
					trial.Outcome, tc.want, trial.ChosenKey, trial.Reason, trial.TerminalGate)
			}
			if trial.ChosenKey != tc.wantKey {
				t.Errorf("chosen key = %q, want %q", trial.ChosenKey, tc.wantKey)
			}
			if trial.TerminalGate != string(tc.wantGate) {
				t.Errorf("terminal gate = %q, want %q", trial.TerminalGate, tc.wantGate)
			}
			bound := trial.Outcome == BindCorrect || trial.Outcome == BindWrong
			if bound && len(trial.Evidence) == 0 {
				t.Error("a bind recorded no evidence")
			}
			if !bound && trial.Reason == "" {
				t.Error("an abstention recorded no reason; SelectAutoBindCandidate never returns a blank one")
			}
			if bound && trial.Reason != "" {
				t.Errorf("a bind recorded an abstention reason %q", trial.Reason)
			}
			if tc.reasonHas != "" && !strings.Contains(trial.Reason, tc.reasonHas) {
				t.Errorf("reason %q does not mention %q", trial.Reason, tc.reasonHas)
			}
		})
	}
}

// TestCandidateEquivalenceClasses pins the canonicalization sameWork gets wrong
// in both directions.
func TestCandidateEquivalenceClasses(t *testing.T) {
	docs := []Document{
		{Key: "D1", Work: work.Work{DOI: "10.5555/test.1", Title: "One"}},
		{Key: "D2", Work: work.Work{DOI: "https://doi.org/10.5555/TEST.1", Title: "One (accepted version)"}},
		{Key: "A1", Work: work.Work{ArXiv: "2201.00001v1", Title: "Two"}},
		{Key: "A2", Work: work.Work{ArXiv: "arXiv:2201.00001v3", Title: "Two"}},
		{Key: "P1", Work: work.Work{PMID: "0012345", Title: "Three"}},
		{Key: "P2", Work: work.Work{PMID: "12345", Title: "Three"}},
		// Transitivity: this row carries both the DOI of D1 and the arXiv id of
		// A1, so all five of D1, D2, A1, A2 and it are one work.
		{Key: "B1", Work: work.Work{DOI: "10.5555/test.1", ArXiv: "2201.00001", Title: "One"}},
		// manifest case06's shape: same title and author as T2, different DOI.
		// A genuinely DIFFERENT work, and one of the most valuable distractors
		// available -- sameWork's identical-title fallback would suppress it.
		{Key: "T1", Work: work.Work{DOI: "10.5555/test.900", Title: "Same Title Different Work", Authors: []string{"R Roe"}, Year: 2020}},
		{Key: "T2", Work: work.Work{DOI: "10.5555/test.901", Title: "Same Title Different Work", Authors: []string{"R Roe"}, Year: 2021}},
		// No canonicalizable identifier at all: class unestablished.
		{Key: "U1", Work: work.Work{Title: "Untraceable", ISBN: "9780000000001"}},
	}
	classes := buildEquivalenceClasses(docs, nil)

	wantClass := map[string][]string{
		"D1": {"A1", "A2", "B1", "D1", "D2"},
		"A2": {"A1", "A2", "B1", "D1", "D2"},
		"P1": {"P1", "P2"},
		"T1": {"T1"},
		"T2": {"T2"},
	}
	for key, want := range wantClass {
		if got := classes.class[key]; !reflect.DeepEqual(got, want) {
			t.Errorf("class(%s) = %v, want %v", key, got, want)
		}
		if got := classes.provenance[key]; got != provenanceIdentifier {
			t.Errorf("provenance(%s) = %q, want %q", key, got, provenanceIdentifier)
		}
	}
	if classes.established("U1") {
		t.Errorf("U1 has no canonicalizable identifier but got class %v", classes.class["U1"])
	}

	// An adjudicated class is used verbatim, with the document's own key added,
	// and is labelled as adjudicated so a reader can tell enumerated
	// preprint/version-of-record pairs from derived ones.
	adjudicated := buildEquivalenceClasses(docs, map[string][]string{"T1": {"T2"}})
	if got, want := adjudicated.class["T1"], []string{"T1", "T2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("adjudicated class(T1) = %v, want %v", got, want)
	}
	if got := adjudicated.provenance["T1"]; got != provenanceAdjudicated {
		t.Errorf("adjudicated provenance(T1) = %q, want %q", got, provenanceAdjudicated)
	}
}

// TestCandidateUnestablishedExcluded pins that a document whose ground truth
// cannot be established is excluded and COUNTED, never guessed into an arm.
func TestCandidateUnestablishedExcluded(t *testing.T) {
	alpha, _, beta, gamma := fixtureDocs()
	// No canonicalizable identifier: nothing separates a same-work candidate
	// from a different one, so no outcome for this document is scorable.
	untraceable := Document{
		Key:  "EEEE",
		Text: synthText("Untraceable Observations", []string{"Ada Lovelace"}, 2001, "", ""),
		Work: work.Work{Title: "Untraceable Observations", Authors: []string{"Ada Lovelace"}, Year: 2001},
	}
	// A secondary attachment inherits its PARENT's Work, so its metadata
	// describes the primary PDF rather than its own bytes; treating it as a
	// target would let a bind of the parent's job score as correct.
	supplement := alpha
	supplement.Key = "FFFF"
	supplement.Secondary = true

	report := MeasureCandidateSets([]Document{alpha, beta, gamma, untraceable, supplement}, CandidateOptions{
		Seed: 3, PoolSizes: []int{2}, Arms: []Arm{ArmRandom},
	})

	if report.DOILessDocuments != 5 {
		t.Errorf("DOILessDocuments = %d, want 5 (admission is about the document, not its truth)", report.DOILessDocuments)
	}
	for _, res := range report.Results {
		if res.Eligible != 3 {
			t.Errorf("cell %s absent=%v: Eligible = %d, want 3", res.Arm, res.TargetAbsent, res.Eligible)
		}
		if res.Unestablished != 2 {
			t.Errorf("cell %s absent=%v: Unestablished = %d, want 2", res.Arm, res.TargetAbsent, res.Unestablished)
		}
		if res.UniqueDocs > res.Eligible {
			t.Errorf("cell %s absent=%v: measured %d documents from %d eligible", res.Arm, res.TargetAbsent, res.UniqueDocs, res.Eligible)
		}
	}
}

// TestCandidateThinCellNotPadded pins that a cell short of distractors is
// skipped rather than padded, and is marked unmistakably.
func TestCandidateThinCellNotPadded(t *testing.T) {
	alpha, _, beta, gamma := fixtureDocs()
	docs := []Document{alpha, beta, gamma}

	report := MeasureCandidateSets(docs, CandidateOptions{
		Seed: 5, PoolSizes: []int{2, 10}, Arms: []Arm{ArmRandom},
	})

	var small, large []ArmResult
	for _, res := range report.Results {
		switch res.PoolSize {
		case 2:
			small = append(small, res)
		case 10:
			large = append(large, res)
		}
	}
	if len(small) != 2 || len(large) != 2 {
		t.Fatalf("want two cells per size, got %d at N=2 and %d at N=10", len(small), len(large))
	}
	for _, res := range small {
		if res.Pools != 3 || !res.Representative {
			t.Errorf("N=2 absent=%v: Pools=%d Representative=%v, want 3 and true", res.TargetAbsent, res.Pools, res.Representative)
		}
	}
	for _, res := range large {
		if res.Pools != 0 {
			t.Errorf("N=10 absent=%v: Pools=%d, want 0 — three documents cannot fill ten candidates and must not be padded", res.TargetAbsent, res.Pools)
		}
		if res.Representative {
			t.Errorf("N=10 absent=%v: an empty cell must never read as representative", res.TargetAbsent)
		}
		if res.Counts.total() != 0 {
			t.Errorf("N=10 absent=%v: %d outcomes recorded in a cell that could not be built", res.TargetAbsent, res.Counts.total())
		}
	}
	rendered := report.Render()
	if !strings.Contains(rendered, "NOT REPRESENTATIVE") {
		t.Error("Render does not mark the thinned cells")
	}
	if !strings.Contains(rendered, "seed") || !strings.Contains(rendered, "DOI-less documents") {
		t.Error("Render omits the seed or the corpus counts")
	}
}

// TestCandidateConjunctionArm measures the composed adversary that withdrew
// candidate_auto_bind/1: one document carrying the target's title, authors and
// year, citing the target's identifier in body text, and printing its own
// different identifier past the blind window.
//
// This test asserts the CLASSIFICATION, never the rule's verdict. Whether
// today's rule binds here is a finding to be reported, not a test to fail: the
// instrument exists precisely because nobody knows the answer over a real
// population.
func TestCandidateConjunctionArm(t *testing.T) {
	alpha, _, beta, gamma := fixtureDocs()
	docs := []Document{alpha, beta, gamma}
	b := testBuilder(docs)

	for _, absent := range []bool{true, false} {
		t.Run(fmt.Sprintf("absent=%v", absent), func(t *testing.T) {
			p, ok := b.buildConjunctionPool(alpha, 3, absent)
			if !ok {
				t.Fatal("conjunction pool could not be built from a fixture that has title, authors, year and a DOI")
			}
			if p.text == "" {
				t.Fatal("conjunction pool carries no synthesized document")
			}
			// The arm IS the offset, so pin both identifiers' positions.
			citedAt := strings.Index(p.text, alpha.Work.DOI)
			ownAt := strings.Index(p.text, conjunctionOwnPrefix)
			if citedAt <= 1024 || citedAt >= 4096 {
				t.Errorf("cited identifier at offset %d, want past 1024 and inside 4096", citedAt)
			}
			if ownAt <= 1024 || ownAt >= 4096 {
				t.Errorf("own identifier at offset %d, want past 1024 and inside 4096", ownAt)
			}
			if ownAt <= citedAt {
				t.Errorf("own identifier at %d must follow the cited one at %d", ownAt, citedAt)
			}
			if dois := pdf.FrontMatterDOIs(p.text); len(dois) != 0 {
				t.Errorf("synthesized composite is inadmissible: front-matter DOIs %v", dois)
			}
			if len(p.Candidates) != 3 {
				t.Errorf("pool size = %d, want 3", len(p.Candidates))
			}
			if !strings.HasPrefix(p.Provenance, "adjudicated:") {
				t.Errorf("provenance %q must record how truth was established", p.Provenance)
			}

			trial := b.evaluatePool(ArmConjunction, 3, p)
			ownKey := alpha.Key + conjunctionOwnKeySuffix
			switch {
			case absent && trial.ChosenKey != "":
				if trial.Outcome != BindWrong {
					t.Errorf("bound %q against an empty class: outcome = %s, want %s", trial.ChosenKey, trial.Outcome, BindWrong)
				}
				t.Logf("FINDING: conjunction/target-absent BINDS %q at gate %s — the withdrawn failure reproduces. evidence: %s",
					trial.ChosenKey, trial.TerminalGate, strings.Join(trial.Evidence, "; "))
			case absent:
				if trial.Outcome != BindAbstainOK {
					t.Errorf("abstained with an empty class: outcome = %s, want %s", trial.Outcome, BindAbstainOK)
				}
				t.Logf("FINDING: conjunction/target-absent abstains at gate %s: %s", trial.TerminalGate, trial.Reason)
			case trial.ChosenKey == ownKey:
				if trial.Outcome != BindCorrect {
					t.Errorf("bound the expansion's own job: outcome = %s, want %s", trial.Outcome, BindCorrect)
				}
				t.Logf("FINDING: conjunction/target-present binds the expansion's OWN job at gate %s", trial.TerminalGate)
			case trial.ChosenKey != "":
				if trial.Outcome != BindWrong {
					t.Errorf("bound %q outside the class %v: outcome = %s, want %s", trial.ChosenKey, p.TrueKeys, trial.Outcome, BindWrong)
				}
				t.Logf("FINDING: conjunction/target-present binds the CITED work %q at gate %s — the wrong job of two that differ only in identifier",
					trial.ChosenKey, trial.TerminalGate)
			default:
				if trial.Outcome != BindMissed {
					t.Errorf("abstained with a present target: outcome = %s, want %s", trial.Outcome, BindMissed)
				}
				t.Logf("FINDING: conjunction/target-present abstains at gate %s: %s", trial.TerminalGate, trial.Reason)
			}
		})
	}
}

// TestCandidateSeedDeterminism pins that a run is reproducible from its recorded
// seed, and that the seed is actually doing the drawing.
func TestCandidateSeedDeterminism(t *testing.T) {
	alpha, sibling, beta, gamma := fixtureDocs()
	docs := []Document{alpha, sibling, beta, gamma,
		synthDoc("GGGG", "Sparse Retrieval over Noisy Corpora", []string{"Liu Wei", "Nadia Farouk"}, 2018, "10.5555/test.2018.7"),
		synthDoc("HHHH", "Bounded Confidence in Opinion Dynamics", []string{"Ivan Petrov", "Sara Costa"}, 2015, "10.5555/test.2015.8"),
	}
	opts := CandidateOptions{Seed: 11, PoolSizes: []int{2, 3}, Arms: []Arm{ArmRandom, ArmSameYear}}

	first := MeasureCandidateSets(docs, opts)
	second := MeasureCandidateSets(docs, opts)
	if !reflect.DeepEqual(first, second) {
		t.Error("two runs at the same seed produced different reports")
	}
	if first.Render() != second.Render() {
		t.Error("two runs at the same seed rendered differently")
	}
	if first.Seed != 11 {
		t.Errorf("report seed = %d, want 11 — a run must be reproducible from its own output", first.Seed)
	}

	// The seed must reach the draws themselves. Pool composition is where that
	// is observable: outcomes can coincide across seeds, candidate sets should
	// not.
	poolsFor := func(seed int64) [][]string {
		b := testBuilder(docs)
		b.seed = seed
		b.eligible = nil
		for _, d := range docs {
			if !unscorableTarget(d, b.classes) {
				b.eligible = append(b.eligible, d)
			}
		}
		var out [][]string
		for _, base := range b.eligible {
			p, ok := b.buildPool(base, ArmRandom, 3, true)
			if !ok {
				continue
			}
			keys := make([]string, 0, len(p.Candidates))
			for _, c := range p.Candidates {
				keys = append(keys, c.Key)
			}
			out = append(out, keys)
		}
		return out
	}
	same := poolsFor(11)
	if !reflect.DeepEqual(same, poolsFor(11)) {
		t.Error("pool composition at one seed is not reproducible")
	}
	if reflect.DeepEqual(same, poolsFor(12)) {
		t.Error("pool composition is identical across two seeds; the seed is not reaching the draws")
	}
}

// TestCandidateWrongDocBoundPerDocument pins the statistic the plan replaced:
// the bound's denominator is the DOCUMENT, not the trial.
func TestCandidateWrongDocBoundPerDocument(t *testing.T) {
	alpha, _, beta, gamma := fixtureDocs()
	docs := []Document{alpha, beta, gamma}
	// Three supplied pools over TWO documents, one of which is wrong-bound in
	// two of them. Per-document counting must see one wrong document out of two,
	// not two wrong trials out of three.
	supplied := []Pool{
		{DocKey: alpha.Key, Candidates: []pdf.BindCandidate{candidateFor(alpha), candidateFor(beta)}, TargetAbsent: true, Provenance: "adjudicated:test"},
		{DocKey: alpha.Key, Candidates: []pdf.BindCandidate{candidateFor(alpha), candidateFor(gamma)}, TargetAbsent: true, Provenance: "adjudicated:test"},
		{DocKey: beta.Key, Candidates: []pdf.BindCandidate{candidateFor(alpha), candidateFor(gamma)}, TargetAbsent: true, Provenance: "adjudicated:test"},
	}
	report := MeasureCandidateSets(docs, CandidateOptions{
		Seed: 2, PoolSizes: []int{2}, Arms: []Arm{ArmComposite},
		ExtraPools: map[Arm][]Pool{ArmComposite: supplied},
	})
	if len(report.Results) != 1 {
		t.Fatalf("want one supplied cell, got %d: %+v", len(report.Results), report.Results)
	}
	res := report.Results[0]
	if res.Pools != 3 {
		t.Fatalf("Pools = %d, want 3 (supplied pools are measured at their given size)", res.Pools)
	}
	if res.PoolSize != 2 {
		t.Errorf("PoolSize = %d, want the observed size 2", res.PoolSize)
	}
	if res.UniqueDocs != 2 {
		t.Fatalf("UniqueDocs = %d, want 2", res.UniqueDocs)
	}
	if res.Counts.Wrong != 2 || res.DocsEverWrong != 1 {
		t.Fatalf("Wrong = %d over %d documents, want 2 wrong trials concentrated in 1 document",
			res.Counts.Wrong, res.DocsEverWrong)
	}
	if got, want := res.WrongPoolRate, 2.0/3.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("WrongPoolRate = %v, want %v", got, want)
	}
	if got, want := res.WrongDocRate, 0.5; math.Abs(got-want) > 1e-9 {
		t.Errorf("WrongDocRate = %v, want %v", got, want)
	}
	if got, want := res.WrongDocBound, binomialUpper95(1, 2); math.Abs(got-want) > 1e-9 {
		t.Errorf("WrongDocBound = %v, want the per-document bound %v", got, want)
	}
	if perTrial := binomialUpper95(2, 3); math.Abs(res.WrongDocBound-perTrial) < 1e-9 {
		t.Error("WrongDocBound equals the per-trial bound; documents are the sampling unit")
	}
	if res.WrongDocBound < res.WrongDocRate {
		t.Errorf("bound %v is below the observed rate %v", res.WrongDocBound, res.WrongDocRate)
	}
}

func TestCandidateBinomialUpper95(t *testing.T) {
	cases := []struct {
		k, n int
		want float64
	}{
		{0, 1, 0.95},
		// The rule of three, correctly: 1-0.05^(1/n).
		{0, 10, 1 - math.Pow(0.05, 1.0/10)},
		{0, 632, 1 - math.Pow(0.05, 1.0/632)},
		// Exact one-sided Clopper-Pearson upper limit: the p solving
		// P(X<=1; 100, p) = 0.05, independently computed at 0.0465598114535.
		{1, 100, 0.0465598},
	}
	for _, tc := range cases {
		got := binomialUpper95(tc.k, tc.n)
		if math.Abs(got-tc.want) > 1e-5 {
			t.Errorf("binomialUpper95(%d, %d) = %v, want %v", tc.k, tc.n, got, tc.want)
		}
	}
	// A vacuous bound is 1, never 0: an empty cell must not read as a measured
	// zero.
	if got := binomialUpper95(0, 0); got != 1 {
		t.Errorf("binomialUpper95(0, 0) = %v, want 1", got)
	}
	// The per-trial figure the plan rejects is roughly 30x more optimistic than
	// the per-document one over the same run; pin the direction so nobody
	// "simplifies" the denominator back.
	perDoc := binomialUpper95(0, 632)
	perTrial := binomialUpper95(0, 632*30)
	if !(perDoc > perTrial*20) {
		t.Errorf("per-document bound %v should be far above the per-trial bound %v", perDoc, perTrial)
	}
}

func TestCandidateArmsAndPoolSizes(t *testing.T) {
	if got, err := ParseArm(" Same-Author "); err != nil || got != ArmSameAuthor {
		t.Errorf("ParseArm(\" Same-Author \") = %q, %v", got, err)
	}
	if _, err := ParseArm("sameauthor"); err == nil {
		t.Error("ParseArm accepted a spelling that is not an arm name")
	}
	if got := normalizePoolSizes([]int{10, 1, 2, 2}); !reflect.DeepEqual(got, []int{2, 10}) {
		t.Errorf("normalizePoolSizes = %v, want [2 10] (N=1 cannot measure a 1-of-N selection)", got)
	}
	if got := normalizePoolSizes(nil); !reflect.DeepEqual(got, defaultPoolSizes) {
		t.Errorf("normalizePoolSizes(nil) = %v, want %v", got, defaultPoolSizes)
	}
	if got := AllArms(); len(got) != len(allArms) || got[0] != ArmRandom {
		t.Errorf("AllArms = %v", got)
	}
}
