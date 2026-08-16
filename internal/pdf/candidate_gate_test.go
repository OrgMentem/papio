// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	ID                       string              `json:"id"`
	Document                 string              `json:"document"`
	RiskFamily               string              `json:"risk_family"`
	Candidates               []manifestCandidate `json:"candidates"`
	Truth                    *string             `json:"truth"`
	CanonicalEquivalence     []string            `json:"canonical_equivalence"`
	EquivalenceJustification *string             `json:"equivalence_justification"`
	GroundTruthNote          string              `json:"ground_truth_note,omitempty"`
	ProbeGate                string              `json:"probe_gate"`
	VetoWindow               bool                `json:"veto_window"`
	VetoNote                 string              `json:"veto_note,omitempty"`

	// ProbeCandidate names the candidate whose traversal the case measures:
	// the one the document is designed to tempt the predicate with. A case
	// supplies a whole pool (a 1-of-N rule cannot be measured pairwise) and
	// the pool is what SelectAutoBindCandidate scores, but exactly one
	// candidate carries the gate the case claims to probe.
	ProbeCandidate string `json:"probe_candidate"`

	// ExpectedGate is the CandidateGate the probe candidate's evaluation must
	// terminate at. It is asserted against the OBSERVED trace, which is the
	// whole point: probe_gate is a label an author typed, expected_gate is a
	// prediction the run can falsify.
	ExpectedGate string `json:"expected_gate"`

	// KnownFailing marks a case the current rule gets wrong on purpose. The
	// case stays in the corpus, stays scored, and is reported as a /2
	// blocker; it does not fail the build, and it must never be relaxed to
	// pass.
	KnownFailing       bool   `json:"known_failing,omitempty"`
	KnownFailingReason string `json:"known_failing_reason,omitempty"`
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

// candidateGateOrder is the rule's gate sequence. Index in this slice is the
// only ordering the harness needs: a case that claims to probe gate G but
// whose evaluation terminates BEFORE G never reached G, and its coverage claim
// is false.
var candidateGateOrder = []CandidateGate{
	GateConclusiveVeto,
	GateNonArticle,
	GateCorrection,
	GateAuthor,
	GateTitle,
	GateYear,
	GateIdentifier,
}

func candidateGateIndex(g CandidateGate) int {
	for i, known := range candidateGateOrder {
		if known == g {
			return i
		}
	}
	return -1
}

// probeGateReaches maps a manifest probe_gate LABEL to the gate a case wearing
// that label must actually have reached. This is the executable half of the
// label: before it existed the harness indexed coverage by the label alone, so
// a case labelled "year-token-boundary" that died at the author gate was still
// counted as year coverage, and a "title-strict-prefix" case that never got as
// far as the title comparison still counted as a title probe. Nothing observed
// the difference. Now the observed terminal gate must sit at or past the
// labelled gate, and coverage is tallied from the observation, never the label.
var probeGateReaches = map[string]CandidateGate{
	"veto":                         GateConclusiveVeto,
	"veto-window":                  GateConclusiveVeto,
	"predicate":                    GateAuthor,
	"correction":                   GateCorrection,
	"correction-pointer":           GateCorrection,
	"author":                       GateAuthor,
	"author-collision":             GateAuthor,
	"author-one-numeric":           GateAuthor,
	"author-one-lettered":          GateAuthor,
	"author-positional":            GateAuthor,
	"title-strict-prefix":          GateTitle,
	"year-token-boundary":          GateYear,
	"composite-cited-identifier":   GateIdentifier,
	"composite-cover-card":         GateTitle,
	"composite-wrapped-subtitle":   GateTitle,
	"composite-numeric-extension":  GateTitle,
	"composite-year-in-dotted-doi": GateYear,
}

func probeGateLabels() []string {
	out := make([]string, 0, len(probeGateReaches))
	for k := range probeGateReaches {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func gateNames() []string {
	out := make([]string, 0, len(candidateGateOrder))
	for _, g := range candidateGateOrder {
		out = append(out, string(g))
	}
	return out
}

// TestCandidateSelectionGate is the Phase 2 measurement gate. It iterates the
// labeled semantic corpus under testdata/candidatecorpus/, runs
// SelectAutoBindCandidate over each case's WHOLE candidate pool, and
// classifies each outcome as wrong-accept, correct-accept, correct-abstain, or
// missed-accept. The headline number is wrong-accepts — zero is required to
// ship auto-bind. Missed-accepts are conservative abstentions and do not fail
// the build. Veto-window cases are reported separately, not folded into the
// main totals, measuring the residual left by the deliberately narrow 1 KiB
// blind window.
//
// Coverage is reported from the OBSERVED terminal gate of each case's probe
// candidate — the trace QualifyCandidate records as it runs — not from the
// manifest's probe_gate label. A label is a claim; only the trace is evidence
// that the case exercised the gate it advertises.
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

	// --- schema / semantic validation over manifest.json ---
	knownGate := make(map[string]bool, len(candidateGateOrder))
	for _, g := range candidateGateOrder {
		knownGate[string(g)] = true
	}
	seenIDs := make(map[string]bool)
	for i, c := range mf.Cases {
		pfx := fmt.Sprintf("manifest cases[%d] %q", i, c.ID)
		if c.ID == "" {
			t.Fatalf("%s: missing id", pfx)
		}
		if seenIDs[c.ID] {
			t.Fatalf("%s: duplicate id", pfx)
		}
		seenIDs[c.ID] = true
		if c.Document == "" {
			t.Fatalf("%s: missing document", pfx)
		}
		if c.RiskFamily == "" {
			t.Fatalf("%s: missing risk_family", pfx)
		}
		// A 1-of-N rule cannot be measured pairwise, and it cannot be measured
		// at all against a pool of one: with a single candidate there is no
		// competitor to mis-select, so the case scores the verification
		// question MatchIdentity answers, not the selection question this rule
		// answers. Every case supplies the full pool it implies.
		if len(c.Candidates) < 2 {
			t.Fatalf("%s: pool of %d — a 1-of-N selection needs at least two live candidates to be measurable", pfx, len(c.Candidates))
		}
		candKeys := make(map[string]bool, len(c.Candidates))
		for _, mc := range c.Candidates {
			candKeys[mc.Key] = true
		}
		if c.ProbeGate == "" {
			t.Fatalf("%s: missing probe_gate (must record which gate the case probes)", pfx)
		}
		if _, ok := probeGateReaches[c.ProbeGate]; !ok {
			t.Fatalf("%s: unknown probe_gate %q (allowed: %v)", pfx, c.ProbeGate, probeGateLabels())
		}
		if c.ProbeCandidate == "" {
			t.Fatalf("%s: missing probe_candidate (name the candidate whose traversal this case measures)", pfx)
		}
		if !candKeys[c.ProbeCandidate] {
			t.Fatalf("%s: probe_candidate %q is not among candidate keys %v", pfx, c.ProbeCandidate, keysOf(candKeys))
		}
		if c.ExpectedGate == "" {
			t.Fatalf("%s: missing expected_gate (the CandidateGate the probe candidate must terminate at; allowed: %v)", pfx, gateNames())
		}
		if !knownGate[c.ExpectedGate] {
			t.Fatalf("%s: unknown expected_gate %q (allowed: %v)", pfx, c.ExpectedGate, gateNames())
		}
		if c.KnownFailing && strings.TrimSpace(c.KnownFailingReason) == "" {
			t.Fatalf("%s: known_failing true but known_failing_reason empty — a suppressed failure must say what it is waiting on", pfx)
		}
		if !c.KnownFailing && strings.TrimSpace(c.KnownFailingReason) != "" {
			t.Fatalf("%s: known_failing_reason present but known_failing false", pfx)
		}
		if c.Truth != nil && *c.Truth == "" {
			t.Fatalf("%s: truth is empty string, use null for absent", pfx)
		}
		if c.Truth != nil && !candKeys[*c.Truth] {
			t.Fatalf("%s: truth %q is not among candidate keys %v", pfx, *c.Truth, keysOf(candKeys))
		}
		// Validate equivalence labels: if canonical_equivalence is non-empty, justification must be present.
		if len(c.CanonicalEquivalence) > 0 {
			if c.EquivalenceJustification == nil || strings.TrimSpace(*c.EquivalenceJustification) == "" {
				t.Fatalf("%s: canonical_equivalence %v requires equivalence_justification (reviewer: silent same-work label converts wrong-accept to scored abstention)", pfx, c.CanonicalEquivalence)
			}
			for _, ek := range c.CanonicalEquivalence {
				if !candKeys[ek] {
					t.Fatalf("%s: canonical_equivalence key %q not among candidate keys %v", pfx, ek, keysOf(candKeys))
				}
			}
			// We require that the declared equivalence actually covers the truth, otherwise the label is vacuous.
			if c.Truth != nil && !isEquivalent(*c.Truth, nil, c.CanonicalEquivalence) {
				t.Fatalf("%s: canonical_equivalence %v does not include truth %q (justification: %q)", pfx, c.CanonicalEquivalence, *c.Truth, *c.EquivalenceJustification)
			}
		} else {
			if c.EquivalenceJustification != nil && strings.TrimSpace(*c.EquivalenceJustification) != "" {
				t.Fatalf("%s: equivalence_justification present but canonical_equivalence empty", pfx)
			}
		}

		// Validate document exists
		docPath := filepath.Join("testdata", "candidatecorpus", c.Document)
		docBytes, err := os.ReadFile(docPath)
		if err != nil {
			t.Fatalf("%s: document %s not readable: %v", pfx, docPath, err)
		}
		// VetoWindow cases must have veto_note
		if c.VetoWindow && strings.TrimSpace(c.VetoNote) == "" {
			t.Fatalf("%s: veto_window true but veto_note empty", pfx)
		}
		// Structural reachability, both directions. Auto-bind only runs when
		// the 1 KiB blind window holds no conclusive DOI (bridge.go), so a
		// predicate case must be DOI-less there or it never reaches gates 2-5,
		// and a veto-labelled case must NOT be, or it never exercises the veto
		// it claims to measure.
		frontDOIs := FrontMatterDOIs(string(docBytes))
		vetoLabelled := c.ProbeGate == "veto" || c.ProbeGate == "veto-window"
		if vetoLabelled && len(frontDOIs) == 0 {
			t.Fatalf("%s: probe_gate %q claims the blind-window veto but FrontMatterDOIs (1 KiB) is empty — the veto is never consulted", pfx, c.ProbeGate)
		}
		if !vetoLabelled && len(frontDOIs) != 0 {
			t.Fatalf("%s: probe_gate %q indicates a DOI-less case but FrontMatterDOIs (1 KiB) is %v — a hard negative must be DOI-less in the blind window to reach gates 2-5", pfx, c.ProbeGate, frontDOIs)
		}
	}

	type tally struct {
		total          int
		wrongAccept    int
		correctAccept  int
		correctAbstain int
		missedAccept   int

		// blockers counts known-failing cases; blockedWrong counts the subset
		// of them whose failure is a wrong-accept, so the headline can subtract
		// exactly the suppressed wrong-accepts and no more.
		blockers     int
		blockedWrong int
	}
	var mainT, vetoT tally
	byGate := make(map[CandidateGate]*tally)
	var wrongDetails []string
	var mismatchDetails []string
	var blockerDetails []string
	// Per-risk-family coverage, from observation: familyEscapesVeto records a
	// family whose verdict some case reached the predicate to decide;
	// familyReachesPredicate records the deeper claim of reaching gate 2.
	familyEscapesVeto := make(map[string]bool)
	familyReachesPredicate := make(map[string]bool)
	familyHasAny := make(map[string]bool)

	t.Logf("observed traces (probe candidate per case):")
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

		// Observation. The probe candidate's traversal is what the case
		// claims to measure; every other candidate is traced too, so the log
		// shows the whole pool the 1-of-N decision actually saw.
		probe := QualifyCandidate(excerpt, bindCandidateByKey(candidates, c.ProbeCandidate))
		observed := probe.Gate
		t.Logf("  %-34s probe=%-12s gate=%-22s disposition=%-7s reached=%v reason=%q",
			c.ID, c.ProbeCandidate, observed, probe.Disposition(), probe.Reached, probe.Reason)
		for _, cand := range candidates {
			if cand.Key == c.ProbeCandidate {
				continue
			}
			q := QualifyCandidate(excerpt, cand)
			t.Logf("      pool  %-12s gate=%-22s disposition=%-7s reason=%q", q.Key, q.Gate, q.Disposition(), q.Reason)
		}

		// Assertion 1: the observed terminal gate must equal the prediction.
		var caseFailures []string
		if string(observed) != c.ExpectedGate {
			caseFailures = append(caseFailures, fmt.Sprintf(
				"case %q: expected_gate %q but candidate %q terminated at %q (disposition %s, reason %q, reached %v)",
				c.ID, c.ExpectedGate, c.ProbeCandidate, observed, probe.Disposition(), probe.Reason, probe.Reached))
		}
		// Assertion 2: the label must be reachable. Terminating before the
		// gate the label advertises means the case never probed that gate,
		// whatever the manifest says it measures.
		claimed := probeGateReaches[c.ProbeGate]
		if candidateGateIndex(observed) < candidateGateIndex(claimed) {
			caseFailures = append(caseFailures, fmt.Sprintf(
				"case %q: probe_gate label %q claims the run reaches %q, but candidate %q terminated earlier at %q (reason %q) — the label overstates coverage",
				c.ID, c.ProbeGate, claimed, c.ProbeCandidate, observed, probe.Reason))
		}

		// Scoring runs over the WHOLE pool: this is a 1-of-N selection and a
		// pairwise score would not see an ambiguous second qualifier.
		winner, ok, _ := SelectAutoBindCandidate(excerpt, candidates)

		cur := &mainT
		if c.VetoWindow {
			cur = &vetoT
		}
		cur.total++
		gTally := byGate[observed]
		if gTally == nil {
			gTally = &tally{}
			byGate[observed] = gTally
		}
		gTally.total++

		familyHasAny[c.RiskFamily] = true
		if candidateGateIndex(observed) > candidateGateIndex(GateConclusiveVeto) {
			familyEscapesVeto[c.RiskFamily] = true
		}
		if candidateGateIndex(observed) >= candidateGateIndex(GateAuthor) {
			familyReachesPredicate[c.RiskFamily] = true
		}

		wrongMsg := ""
		switch {
		case ok:
			if c.Truth == nil {
				wrongMsg = fmt.Sprintf("case %q (%s) observed-gate=%s: wrong-accept choosing %q but truth is absent (evidence %v)", c.ID, c.RiskFamily, observed, winner.Key, winner.Evidence)
			} else if isEquivalent(winner.Key, c.Truth, c.CanonicalEquivalence) {
				cur.correctAccept++
				gTally.correctAccept++
			} else {
				wrongMsg = fmt.Sprintf("case %q (%s) observed-gate=%s: wrong-accept choosing %q but truth is %q equiv %v evidence %v", c.ID, c.RiskFamily, observed, winner.Key, *c.Truth, c.CanonicalEquivalence, winner.Evidence)
			}
		default:
			if c.Truth == nil {
				cur.correctAbstain++
				gTally.correctAbstain++
			} else if len(c.CanonicalEquivalence) > 0 {
				cur.correctAbstain++
				gTally.correctAbstain++
			} else {
				cur.missedAccept++
				gTally.missedAccept++
			}
		}
		if wrongMsg != "" {
			cur.wrongAccept++
			gTally.wrongAccept++
		}

		switch {
		case c.KnownFailing && wrongMsg == "" && len(caseFailures) == 0:
			// A known-failing case that passes is a stale suppression: the
			// predicate improved and the marker now hides a real regression
			// signal. Fail loudly rather than let the marker rot.
			t.Errorf("case %q is marked known_failing (%s) but every assertion passed — remove known_failing so the case defends the fix", c.ID, c.KnownFailingReason)
		case c.KnownFailing:
			cur.blockers++
			gTally.blockers++
			if wrongMsg != "" {
				cur.blockedWrong++
				caseFailures = append(caseFailures, wrongMsg)
			}
			for _, f := range caseFailures {
				blockerDetails = append(blockerDetails, f+"\n        waiting on: "+c.KnownFailingReason)
			}
		default:
			mismatchDetails = append(mismatchDetails, caseFailures...)
			if wrongMsg != "" {
				wrongDetails = append(wrongDetails, wrongMsg)
			}
		}
	}

	t.Logf("gate report [%s]", CandidateBindingRule)
	t.Logf("main corpus: total=%d correct-accept=%d correct-abstain=%d missed-accept=%d wrong-accept=%d known-failing=%d", mainT.total, mainT.correctAccept, mainT.correctAbstain, mainT.missedAccept, mainT.wrongAccept, mainT.blockers)
	if vetoT.total > 0 {
		t.Logf("veto-window (1-4 KiB residual, not folded): total=%d correct-accept=%d correct-abstain=%d missed-accept=%d wrong-accept=%d known-failing=%d", vetoT.total, vetoT.correctAccept, vetoT.correctAbstain, vetoT.missedAccept, vetoT.wrongAccept, vetoT.blockers)
		t.Logf("veto-window detail: measures the deliberately narrow 1 KiB blind window; see manifest veto_note")
	}
	// Per-gate breakdown, keyed by the gate the run was OBSERVED to terminate
	// at. The manifest's probe_gate label does not appear here at all.
	t.Logf("per-gate breakdown (OBSERVED terminal gate of the probe candidate; labels are not counted):")
	for _, gate := range candidateGateOrder {
		gt := byGate[gate]
		if gt == nil {
			t.Logf("  gate %-22s total=0 — NO case was observed to terminate here", gate)
			continue
		}
		t.Logf("  gate %-22s total=%d correct-accept=%d correct-abstain=%d missed-accept=%d wrong-accept=%d known-failing=%d", gate, gt.total, gt.correctAccept, gt.correctAbstain, gt.missedAccept, gt.wrongAccept, gt.blockers)
	}
	// Risk family coverage, observed on two levels.
	//
	// The hard requirement is that every family has at least one case whose
	// verdict was DECIDED BY THE PREDICATE rather than by the 1 KiB blind
	// window: a family whose every case terminates at the conclusive-identity
	// veto has measured the veto, not the acceptance rule, and says nothing
	// about autonomous selection.
	//
	// Reaching gate 2 or later is reported but not required, because for some
	// families terminating earlier is the CORRECT behaviour — a correction
	// notice is meant to die at the front-matter marker gate, and demanding a
	// gate-2 case there would mean authoring a correction notice that does not
	// announce itself, which is a different family.
	missingPredicate := []string{}
	shallow := []string{}
	for fam := range familyHasAny {
		// veto-window families measure the 1 KiB blind-window residual explicitly; they are not required to have a predicate variant
		if strings.HasPrefix(fam, "veto-window") {
			continue
		}
		if !familyEscapesVeto[fam] {
			missingPredicate = append(missingPredicate, fam)
		} else if !familyReachesPredicate[fam] {
			shallow = append(shallow, fam)
		}
	}
	if len(shallow) > 0 {
		sort.Strings(shallow)
		t.Logf("coverage note: families decided at gate 1/1b only, never observed at gate 2 or later: %v", shallow)
	}
	if len(missingPredicate) > 0 {
		sort.Strings(missingPredicate)
		t.Errorf("coverage: families where every case was decided by the 1 KiB blind-window veto, so the acceptance rule was never measured: %v — add a DOI-less variant", missingPredicate)
	}

	if len(blockerDetails) > 0 {
		t.Logf("/2 BLOCKING SET — %d case(s) the current rule gets wrong. These are composite documents:", mainT.blockers+vetoT.blockers)
		t.Logf("  one relational block supplies title, authors, year and identifier at once, which is how a real")
		t.Logf("  wrong-accept arrives. They are ground-truthed to abstain, they do not abstain, and they must NOT")
		t.Logf("  be relaxed. Closing them is the acceptance criterion for candidate_auto_bind/2:")
		for _, d := range blockerDetails {
			t.Logf("    - %s", d)
		}
	}

	totalWrong := mainT.wrongAccept + vetoT.wrongAccept
	blockedWrong := mainT.blockedWrong + vetoT.blockedWrong
	t.Logf("headline: unblocked wrong-accepts=%d (must be 0); a further %d wrong-accept(s) are held in the /2 blocking set above, total wrong-accepts=%d", totalWrong-blockedWrong, blockedWrong, totalWrong)
	if mainT.missedAccept > 0 || vetoT.missedAccept > 0 {
		t.Logf("note: missed-accepts are conservative abstentions, not failures; coverage %d/%d main, %d/%d veto", mainT.correctAccept, mainT.correctAccept+mainT.missedAccept, vetoT.correctAccept, vetoT.correctAccept+vetoT.missedAccept)
	}

	for _, d := range mismatchDetails {
		t.Errorf("%s", d)
	}
	if len(wrongDetails) > 0 {
		for _, d := range wrongDetails {
			t.Errorf("%s", d)
		}
		t.Fatalf("gate FAILED: %d unblocked wrong-accept(s) — the wrong paper would be filed under the right citation", len(wrongDetails))
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
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

// candidateSentinelOptOut lets a developer without pdftotext installed run the
// rest of the package. It is the ONLY way past the check below and it must be
// set deliberately, per run.
const candidateSentinelOptOut = "PAPIO_SKIP_PDF_SENTINELS"

func TestCandidateGateSentinels(t *testing.T) {
	cap := DetectCapability()
	if !cap.Semantic() {
		// WHY this fails instead of skipping: these nine sentinels are the
		// only layer that pins hand-authored text against REAL extraction —
		// ligatures, hyphen wraps, two-column gluing, the form-feed and 4 KiB
		// window edges. Everything else in this package reads .txt files an
		// author typed, so with the extractor absent the suite goes green
		// having never once checked that pdftotext produces the text the rules
		// are written against. A green suite that silently dropped its only
		// contact with reality is worse than a red one: it is the same
		// failure class the corpus exists to catch, applied to the corpus
		// itself. The release path must therefore have the extractor.
		if os.Getenv(candidateSentinelOptOut) != "" {
			t.Skipf("sentinel gate: skipped by %s — extraction NOT verified; this must never be set on a release path", candidateSentinelOptOut)
		}
		t.Fatalf("sentinel gate: pdftotext unavailable — the nine extraction sentinels cannot run and the suite would be green without ever exercising real extraction. Install poppler-utils, or set %s=1 to run the rest of the package with extraction unverified.", candidateSentinelOptOut)
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
