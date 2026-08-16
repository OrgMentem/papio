// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package identitycorpus

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"papio/internal/pdf"
	"papio/internal/work"
)

// pad is filler long enough to push what follows past the 1 KiB blind
// front-matter window while leaving it inside page one's 4 KiB cap — the range
// the plan requires the proposer to respect, and where manifest case25 puts a
// composite's cited identifier.
func pad() string { return strings.Repeat("filler text of no consequence. ", 40) }

func syntheticDoc(key, parentKey, title, doi, text string) Document {
	return Document{
		Key:       key,
		ParentKey: parentKey,
		Work:      work.Work{Title: title, DOI: doi, Authors: []string{"Jane Doe"}, Year: 2022},
		Text:      text,
		Chars:     int64(len(text)),
	}
}

// compositeCorpus is one synthetic library holding a document per signal the
// proposer claims, plus the two documents that must NOT be proposed.
func compositeCorpus() []Document {
	return []Document{
		// Correction marker in the document's own front matter.
		syntheticDoc("AAAA0001", "PAR00001", "Erratum to: Adaptive Sampling for Streaming Graph Partitioning", "10.5555/synth.2022.900",
			"Erratum to: Adaptive Sampling for Streaming Graph Partitioning\n\nJane Doe\n\nThe original article contained an error in Table 2.\n"),
		// Correction marker in the curated title only: the document text
		// opens with the paper's own running head.
		syntheticDoc("AAAA0002", "PAR00002", "Retraction note: Robust Calibration of Ensemble Forecasts", "10.5555/synth.2022.901",
			"Journal of Synthetic Results\n\nJane Doe\n\nThis notice concerns a previously published study.\n"),
		// Non-article marker.
		syntheticDoc("AAAA0003", "PAR00003", "Adaptive Sampling for Streaming Graph Partitioning", "10.5555/synth.2022.902",
			"Supplementary Information\n\nfor Adaptive Sampling for Streaming Graph Partitioning\n\nFigure S1.\n"),
		// Title quoting another document's whole title, with no marker
		// vocabulary anywhere: the journal-expansion shape.
		syntheticDoc("AAAA0004", "PAR00004", "Extended version of Robust Calibration of Ensemble Forecasts", "10.5555/synth.2022.903",
			"Extended version of Robust Calibration of Ensemble Forecasts\n\nJane Doe\n\nWe extend the earlier study.\n"),
		// The work AAAA0004 quotes, and the work AAAA0006 cites.
		syntheticDoc("AAAA0005", "PAR00005", "Robust Calibration of Ensemble Forecasts", "10.5555/synth.2022.610",
			"Robust Calibration of Ensemble Forecasts\n\nJane Doe\n\nAbstract. We calibrate ensembles.\n"),
		// Foreign printed identifier, past 1 KiB and inside page one.
		syntheticDoc("AAAA0006", "PAR00006", "Verification Workflows for Operational Forecasting", "10.5555/synth.2022.777",
			"Verification Workflows for Operational Forecasting\n\nJane Doe\n\n"+pad()+"\nExtended from DOI 10.5555/synth.2022.610, which this report supersedes.\n"),
		// Prints only its OWN identifier, in the same position. Must not
		// be proposed.
		syntheticDoc("AAAA0007", "PAR00007", "Attention Routing in Sparse Mixture Models", "10.5555/synth.2023.301",
			"Attention Routing in Sparse Mixture Models\n\nJane Doe\n\n"+pad()+"\nThis article is registered as 10.5555/synth.2023.301.\n"),
		// A foreign identifier on page two: invisible to the rules, so
		// counted and not proposed.
		syntheticDoc("AAAA0008", "PAR00008", "Streaming Partition Quality Metrics", "10.5555/synth.2023.400",
			"Streaming Partition Quality Metrics\n\nJane Doe\n\nAbstract.\n\f\nReferences\n10.5555/synth.2022.610\n"),
		// Same title as AAAA0005, different DOI: manifest case06's shape.
		// A genuinely different work, and one of the most valuable
		// distractors available, so it must never be proposed.
		syntheticDoc("AAAA0009", "PAR00009", "Robust Calibration of Ensemble Forecasts", "10.5555/synth.2019.111",
			"Robust Calibration of Ensemble Forecasts\n\nJane Doe\n\nAbstract. An earlier, unrelated study.\n"),
	}
}

func proposalsByKey(t *testing.T, review CompositeReview) map[string]CompositeEntry {
	t.Helper()
	out := make(map[string]CompositeEntry, len(review.Proposals))
	for _, row := range review.Proposals {
		if _, dup := out[row.Key]; dup {
			t.Fatalf("proposal for %s appears twice", row.Key)
		}
		out[row.Key] = row
	}
	return out
}

func hasSignal(entry CompositeEntry, name string) (CompositeSignal, bool) {
	for _, s := range entry.Signals {
		if s.Name == name {
			return s, true
		}
	}
	return CompositeSignal{}, false
}

func TestCompositeProposerSignals(t *testing.T) {
	review := ProposeComposites(compositeCorpus(), CompositeOptions{Seed: 7, AuditSample: 3})
	byKey := proposalsByKey(t, review)

	cases := []struct {
		key    string
		class  CompositeClass
		signal string
		refers string
	}{
		{key: "AAAA0001", class: ClassErratum, signal: signalCorrectionMarkerText},
		{key: "AAAA0002", class: ClassRetraction, signal: signalCorrectionMarkerTitle},
		{key: "AAAA0003", class: ClassSupplement, signal: signalNonArticleMarkerText},
		{key: "AAAA0004", class: ClassComposite, signal: signalTitleQuotesTitle, refers: "AAAA0005"},
		{key: "AAAA0006", class: ClassComposite, signal: signalForeignIdentifier, refers: "AAAA0005"},
	}
	for _, tc := range cases {
		entry, ok := byKey[tc.key]
		if !ok {
			t.Errorf("%s: not proposed; signal %s did not fire", tc.key, tc.signal)
			continue
		}
		if entry.Proposed != tc.class {
			t.Errorf("%s: proposed class %q, want %q", tc.key, entry.Proposed, tc.class)
		}
		signal, ok := hasSignal(entry, tc.signal)
		if !ok {
			t.Errorf("%s: signals %v carry no %s", tc.key, entry.Signals, tc.signal)
			continue
		}
		if tc.refers != "" {
			if len(signal.Refers) != 1 || signal.Refers[0] != tc.refers {
				t.Errorf("%s: %s implicates %v, want [%s]", tc.key, tc.signal, signal.Refers, tc.refers)
			}
			if len(entry.ProposedRefersTo) == 0 || entry.ProposedRefersTo[0] != tc.refers {
				t.Errorf("%s: proposed_refers_to %v, want %s", tc.key, entry.ProposedRefersTo, tc.refers)
			}
		}
		if !entry.DOILess {
			t.Errorf("%s: recorded as having a front-matter DOI; the fixture puts every identifier past the blind window", tc.key)
		}
	}

	// The foreign printed identifier must be seen where the RULES see it:
	// past the 1 KiB blind window, inside page one's 4 KiB cap.
	if signal, ok := hasSignal(byKey["AAAA0006"], signalForeignIdentifier); ok && !strings.HasPrefix(signal.Detail, windowPageOne) {
		t.Errorf("foreign identifier reported in window %q, want %q", signal.Detail, windowPageOne)
	}

	if entry, ok := byKey["AAAA0007"]; ok {
		t.Errorf("AAAA0007 prints only its own identifier and was proposed as %q with signals %v", entry.Proposed, entry.Signals)
	}
	if entry, ok := byKey["AAAA0009"]; ok {
		t.Errorf("AAAA0009 shares a title with a different work (manifest case06's shape) and was proposed as %q", entry.Proposed)
	}
	if entry, ok := byKey["AAAA0008"]; ok {
		t.Errorf("AAAA0008 prints a foreign identifier only on page two, invisible to the rules, and was proposed as %q", entry.Proposed)
	}
	if review.ForeignBeyondPageOne == 0 {
		t.Error("a foreign identifier past page one was neither proposed nor counted; the blind spot must be counted")
	}
	if review.Seed != 7 || review.Documents != 9 {
		t.Errorf("review records seed %d over %d documents, want 7 over 9", review.Seed, review.Documents)
	}
	if len(review.SignalsUnavailable) == 0 {
		t.Error("review claims no unsupported signals; the page-range and filename limits must be reported")
	}
	for _, row := range review.Proposals {
		if row.Reviewed || row.Class != ClassUnlabelled {
			t.Errorf("%s: proposer produced a LABEL, not a proposal (reviewed=%v class=%q)", row.Key, row.Reviewed, row.Class)
		}
	}
}

func TestCompositeProposerSecondaryAttachment(t *testing.T) {
	docs := compositeCorpus()
	// A supplement filed as a second attachment on the article's own
	// Zotero item: it inherits the article's DOI, so no identifier signal
	// can fire and the attachment-level flag is what carries the class.
	supplement := syntheticDoc("AAAA0010", "PAR00005", "Robust Calibration of Ensemble Forecasts", "10.5555/synth.2022.610",
		"Tables and figures.\n\nTable 1. Calibration scores.\n")
	supplement.Secondary = true
	docs = append(docs, supplement)

	review := ProposeComposites(docs, CompositeOptions{Seed: 7, AuditSample: 0})
	entry, ok := proposalsByKey(t, review)["AAAA0010"]
	if !ok {
		t.Fatal("a secondary attachment was not proposed; the class the all-attachments mode exists to reach would be unmeasured")
	}
	if entry.Proposed != ClassSupplement {
		t.Errorf("proposed class %q, want %q", entry.Proposed, ClassSupplement)
	}
	if _, ok := hasSignal(entry, signalSecondaryAttachment); !ok {
		t.Errorf("signals %v carry no %s", entry.Signals, signalSecondaryAttachment)
	}
	if _, ok := hasSignal(entry, signalForeignIdentifier); ok {
		t.Error("a supplement inheriting its parent's DOI must not read as printing a foreign identifier")
	}
}

func TestCompositeProposerShortDocumentDoesNotPropose(t *testing.T) {
	docs := []Document{
		syntheticDoc("AAAA0020", "PAR00020", "A Perfectly Ordinary Short Paper", "10.5555/synth.2022.500",
			"A Perfectly Ordinary Short Paper\n\nJane Doe\n\nAbstract. Short but ordinary.\n"),
	}
	review := ProposeComposites(docs, CompositeOptions{Seed: 1, AuditSample: 1})
	if len(review.Proposals) != 0 {
		t.Fatalf("a short document alone was proposed: %+v", review.Proposals)
	}
	if len(review.AuditSample) != 1 {
		t.Fatalf("audit sample holds %d rows, want 1", len(review.AuditSample))
	}
	if _, ok := hasSignal(review.AuditSample[0], signalShortDocument); !ok {
		t.Errorf("audit row hides the weak signal the proposer did observe: %+v", review.AuditSample[0].Signals)
	}
}

func TestCompositeAuditSampleDrawnFromUnflaggedDocuments(t *testing.T) {
	docs := compositeCorpus()
	review := ProposeComposites(docs, CompositeOptions{Seed: 11, AuditSample: 4})

	flagged := proposalsByKey(t, review)
	if len(review.AuditSample) != 4 {
		t.Fatalf("audit sample holds %d rows, want 4", len(review.AuditSample))
	}
	for _, row := range review.AuditSample {
		if _, ok := flagged[row.Key]; ok {
			t.Errorf("%s is both proposed and audited; the audit exists to bound what the proposer MISSED", row.Key)
		}
		if row.Proposed != ClassUnlabelled {
			t.Errorf("%s: audit row carries a proposed class %q", row.Key, row.Proposed)
		}
		if row.Reviewed {
			t.Errorf("%s: audit row arrives reviewed", row.Key)
		}
	}
	// Same seed, same draw.
	again := ProposeComposites(docs, CompositeOptions{Seed: 11, AuditSample: 4})
	for i, row := range review.AuditSample {
		if again.AuditSample[i].Key != row.Key {
			t.Fatalf("seed 11 drew %s then %s at position %d; the draw must be reproducible from the recorded seed",
				row.Key, again.AuditSample[i].Key, i)
		}
	}
}

// confirm marks one proposal reviewed and confirmed, as a human editing the
// file would.
func confirm(t *testing.T, review CompositeReview, key string, class CompositeClass, refersTo ...string) CompositeReview {
	t.Helper()
	found := false
	for i := range review.Proposals {
		if review.Proposals[i].Key != key {
			continue
		}
		review.Proposals[i].Reviewed = true
		review.Proposals[i].Class = class
		review.Proposals[i].RefersTo = refersTo
		found = true
	}
	if !found {
		t.Fatalf("no proposal for %s to confirm", key)
	}
	return review
}

func TestCompositePoolsConfirmedCompositeIsTargetAbsentWithReferentPresent(t *testing.T) {
	docs := compositeCorpus()
	opts := CompositeOptions{Seed: 3, AuditSample: 2, PoolSizes: []int{2, 5}}
	review := confirm(t, ProposeComposites(docs, opts), "AAAA0006", ClassExpansion, "AAAA0005")

	pools, summary := CompositePools(docs, review, opts)
	if summary.Confirmed != 1 {
		t.Fatalf("summary counts %d confirmed composites, want 1", summary.Confirmed)
	}
	if len(pools) != 2 {
		t.Fatalf("built %d pools, want one per requested size (2)", len(pools))
	}
	for _, pool := range pools {
		if pool.DocKey != "AAAA0006" {
			t.Errorf("pool built for %s, want AAAA0006", pool.DocKey)
		}
		if len(pool.TrueKeys) != 0 {
			t.Errorf("pool carries an equivalence class %v; a composite's correct decision is to bind NOTHING", pool.TrueKeys)
		}
		if !pool.TargetAbsent {
			t.Error("pool is not marked target-absent, so a correct abstention would score as a missed bind")
		}
		if !strings.HasPrefix(pool.Provenance, "adjudicated:") {
			t.Errorf("provenance %q does not record human adjudication", pool.Provenance)
		}
		if !strings.Contains(pool.Provenance, "AAAA0005") {
			t.Errorf("provenance %q does not name the referred-to work present as a candidate", pool.Provenance)
		}
		var keys []string
		for _, c := range pool.Candidates {
			keys = append(keys, c.Key)
			if c.Key == "AAAA0006" {
				t.Error("the composite document appears as a candidate for itself")
			}
		}
		if keys[0] != "AAAA0005" {
			t.Errorf("candidates %v do not lead with the referred-to work; that presence is what makes the pool adversarial", keys)
		}
	}
	if len(pools[0].Candidates) != 2 || len(pools[1].Candidates) != 5 {
		t.Fatalf("pool sizes are %d and %d, want 2 and 5", len(pools[0].Candidates), len(pools[1].Candidates))
	}
	// The referred-to candidate must be presented the way production
	// presents a job: bound to its own DOI, so the conclusive-identity veto
	// compares against something real.
	if got := pools[0].Candidates[0].Bound; len(got) != 1 || got[0] != "10.5555/synth.2022.610" {
		t.Errorf("referred-to candidate bound to %v, want its own DOI", got)
	}
	if summary.PoolsWithReferent != 2 {
		t.Errorf("%d pools recorded as holding the referent, want 2", summary.PoolsWithReferent)
	}
	if summary.PrevalenceLowerBound <= 0 {
		t.Error("prevalence lower bound is zero with one confirmed composite")
	}

	// Nested pools: the same seed and document give the smaller pool as a
	// prefix of the larger, so the only thing varying across the sweep is N.
	for i, c := range pools[0].Candidates {
		if pools[1].Candidates[i].Key != c.Key {
			t.Fatalf("pool at N=2 is not a prefix of the pool at N=5 (%s vs %s)", c.Key, pools[1].Candidates[i].Key)
		}
	}

	// Determinism across runs.
	againPools, _ := CompositePools(docs, review, opts)
	for i, pool := range pools {
		for j, c := range pool.Candidates {
			if againPools[i].Candidates[j].Key != c.Key {
				t.Fatalf("pool %d candidate %d moved between runs at the same seed", i, j)
			}
		}
	}
}

func TestCompositePoolsUnreviewedProposalCountsAsNeitherClass(t *testing.T) {
	docs := compositeCorpus()
	opts := CompositeOptions{Seed: 3, AuditSample: 2, PoolSizes: []int{2}}
	review := ProposeComposites(docs, opts)

	pools, summary := CompositePools(docs, review, opts)
	if len(pools) != 0 {
		t.Fatalf("built %d pools from proposals no human confirmed", len(pools))
	}
	if summary.Confirmed != 0 || summary.Rejected != 0 || summary.Reviewed != 0 {
		t.Errorf("unreviewed proposals scored as labels: confirmed=%d rejected=%d reviewed=%d",
			summary.Confirmed, summary.Rejected, summary.Reviewed)
	}
	if summary.Unlabelled != summary.Proposed || summary.Proposed == 0 {
		t.Errorf("%d of %d proposals reported UNLABELLED, want all of them", summary.Unlabelled, summary.Proposed)
	}
	if summary.PrevalenceLowerBound != 0 {
		t.Errorf("prevalence lower bound %v from zero confirmed labels", summary.PrevalenceLowerBound)
	}
	if summary.PrevalenceBounded {
		t.Error("prevalence reported as bounded above while no audit row is reviewed")
	}
	rendered := summary.Render()
	for _, want := range []string{"UNLABELLED", "UNAVAILABLE", "LOWER BOUND", "measures NOTHING"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered summary does not say %q:\n%s", want, rendered)
		}
	}

	// A reviewed rejection is a different fact from an unreviewed proposal.
	rejected := confirm(t, review, "AAAA0001", ClassNotComposite)
	_, summary = CompositePools(docs, rejected, opts)
	if summary.Rejected != 1 || summary.Confirmed != 0 {
		t.Errorf("reviewed rejection counted as confirmed=%d rejected=%d", summary.Confirmed, summary.Rejected)
	}
}

func TestCompositePoolsSkipConfirmedCompositeWithFrontMatterDOI(t *testing.T) {
	// Production reaches SelectAutoBindCandidate only when the document's
	// front-matter DOI set is empty, so a composite that prints one is a
	// real composite the selector never sees. It must be counted, not
	// pooled.
	docs := []Document{
		syntheticDoc("BBBB0001", "PAR00001", "Erratum to: Adaptive Sampling", "10.5555/synth.2022.900",
			"Erratum to: Adaptive Sampling\nhttps://doi.org/10.5555/synth.2022.900\n\nJane Doe\n"),
		syntheticDoc("BBBB0002", "PAR00002", "Adaptive Sampling", "10.5555/synth.2022.501",
			"Adaptive Sampling\n\nJane Doe\n\nAbstract.\n"),
		syntheticDoc("BBBB0003", "PAR00003", "Another Paper Entirely", "10.5555/synth.2022.502",
			"Another Paper Entirely\n\nJane Doe\n\nAbstract.\n"),
	}
	if len(pdf.FrontMatterDOIs(docs[0].Text)) == 0 {
		t.Fatal("fixture precondition: the erratum must print a conclusive front-matter DOI")
	}
	opts := CompositeOptions{Seed: 5, AuditSample: 0, PoolSizes: []int{2}}
	review := confirm(t, ProposeComposites(docs, opts), "BBBB0001", ClassErratum, "BBBB0002")

	pools, summary := CompositePools(docs, review, opts)
	if len(pools) != 0 {
		t.Errorf("built %d pools for a composite production never routes to the selector", len(pools))
	}
	if summary.ConfirmedWithFrontMatterDOI != 1 {
		t.Errorf("%d confirmed composites recorded as carrying a front-matter DOI, want 1", summary.ConfirmedWithFrontMatterDOI)
	}
	if summary.Confirmed != 1 {
		t.Errorf("the label was dropped rather than counted: confirmed=%d", summary.Confirmed)
	}
}

func TestCompositeReviewRoundTripAndStrictness(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "composite-labels.json")
	docs := compositeCorpus()
	review := confirm(t, ProposeComposites(docs, CompositeOptions{Seed: 2, AuditSample: 2}), "AAAA0001", ClassErratum, "AAAA0005")

	if err := WriteCompositeReview(path, review); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("review file mode %v; it holds the operator's own library titles", perm)
	}
	loaded, err := LoadCompositeReview(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded.SameRows(review) {
		t.Error("round trip changed which documents the file covers")
	}
	confirmedRows := 0
	for _, row := range loaded.Proposals {
		if row.Reviewed && row.Class == ClassErratum && len(row.RefersTo) == 1 && row.RefersTo[0] == "AAAA0005" {
			confirmedRows++
		}
	}
	if confirmedRows != 1 {
		t.Errorf("%d rows survived the round trip as confirmed errata, want 1", confirmedRows)
	}

	// A misspelled field must fail the load, not silently discard a label:
	// "reviewd" would otherwise read as unreviewed and report the document
	// as UNLABELLED, which looks exactly like honest absence of evidence.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	typoPath := filepath.Join(dir, "typo.json")
	if err := os.WriteFile(typoPath, []byte(strings.Replace(string(raw), `"reviewed": true`, `"reviewd": true`, 1)), 0o600); err != nil {
		t.Fatalf("write typo fixture: %v", err)
	}
	if _, err := LoadCompositeReview(typoPath); err == nil {
		t.Error("a misspelled field loaded silently; a human's label would be discarded without a word")
	}

	for name, mutate := range map[string]func(*CompositeReview){
		"unknown class": func(r *CompositeReview) {
			r.Proposals[0].Reviewed = true
			r.Proposals[0].Class = CompositeClass("errata-ish")
		},
		"reviewed with no class": func(r *CompositeReview) {
			r.Proposals[0].Reviewed = true
			r.Proposals[0].Class = ClassUnlabelled
		},
		"class without reviewed": func(r *CompositeReview) {
			r.Proposals[0].Reviewed = false
			r.Proposals[0].Class = ClassErratum
		},
	} {
		bad := review
		bad.Proposals = append([]CompositeEntry(nil), review.Proposals...)
		mutate(&bad)
		badPath := filepath.Join(dir, "bad.json")
		data, err := json.Marshal(bad)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if err := os.WriteFile(badPath, data, 0o600); err != nil {
			t.Fatalf("%s: write: %v", name, err)
		}
		if _, err := LoadCompositeReview(badPath); err == nil {
			t.Errorf("%s: loaded as usable ground truth", name)
		}
	}
}

func TestCompositeMergePreservesHumanLabels(t *testing.T) {
	docs := compositeCorpus()
	opts := CompositeOptions{Seed: 4, AuditSample: 2}
	prior := confirm(t, ProposeComposites(docs, opts), "AAAA0001", ClassErratum, "AAAA0005")
	// The human also reviewed an audit row and found a proposer miss.
	prior.AuditSample[0].Reviewed = true
	prior.AuditSample[0].Class = ClassCoverSheet
	missedKey := prior.AuditSample[0].Key

	// A later run over a library that no longer holds AAAA0001 at all.
	shrunk := make([]Document, 0, len(docs))
	for _, doc := range docs {
		if doc.Key != "AAAA0001" {
			shrunk = append(shrunk, doc)
		}
	}
	fresh := ProposeComposites(shrunk, opts)
	merged := MergeCompositeReview(fresh, prior)

	rows := proposalsByKey(t, merged)
	retained, ok := rows["AAAA0001"]
	if !ok {
		t.Fatal("a reviewed label was dropped because the proposer stopped proposing it; the label is ground truth")
	}
	if !retained.Reviewed || retained.Class != ClassErratum {
		t.Errorf("retained row lost its label: reviewed=%v class=%q", retained.Reviewed, retained.Class)
	}
	if _, ok := hasSignal(retained, signalNoLongerProposed); !ok {
		t.Errorf("retained row does not say why it is there: %+v", retained.Signals)
	}
	found := false
	for _, row := range merged.AuditSample {
		if row.Key == missedKey {
			found = true
			if !row.Reviewed || row.Class != ClassCoverSheet {
				t.Errorf("audit label lost: reviewed=%v class=%q", row.Reviewed, row.Class)
			}
		}
	}
	if !found {
		t.Errorf("reviewed audit row %s vanished from the merge; the recall bound depends on it", missedKey)
	}
	if merged.SameRows(prior) && len(merged.Proposals) != len(prior.Proposals) {
		t.Error("SameRows agreed while the covered documents differ")
	}

	// The audit label now bounds prevalence from above, which an unreviewed
	// audit cannot do.
	_, summary := CompositePools(shrunk, merged, opts)
	if summary.AuditReviewed != 1 || summary.AuditComposites != 1 {
		t.Fatalf("audit review not counted: reviewed=%d composites=%d", summary.AuditReviewed, summary.AuditComposites)
	}
	if !summary.PrevalenceBounded {
		t.Error("prevalence still unbounded above after an audit row was reviewed")
	}
	if summary.PrevalenceUpperBound < summary.PrevalenceLowerBound {
		t.Errorf("upper bound %v below lower bound %v", summary.PrevalenceUpperBound, summary.PrevalenceLowerBound)
	}
	if summary.ConfirmedMissing != 1 {
		t.Errorf("%d confirmed labels reported as naming documents this run did not load, want 1", summary.ConfirmedMissing)
	}
}

func TestCompositeAuditBoundIsOneSidedAndExact(t *testing.T) {
	// Zero misses in 25 reviewed documents bounds the miss rate at
	// 1 - 0.05^(1/25) ≈ 11.3%, not at zero and not at 3/25 = 12% by the
	// rule of thumb.
	got := binomialUpper95(0, 25)
	if got < 0.11 || got > 0.115 {
		t.Errorf("upper bound for 0/25 is %.4f, want ≈0.1129", got)
	}
	if binomialUpper95(0, 100) >= got {
		t.Error("a larger clean sample did not tighten the bound")
	}
	if binomialUpper95(2, 10) <= 0.2 {
		t.Error("the bound for 2/10 sits at or below the point estimate")
	}
	if binomialUpper95(3, 3) != 1 || binomialUpper95(0, 0) != 1 {
		t.Error("a degenerate sample must bound at 1, never claim precision")
	}
}

// --- all-attachments loader mode -------------------------------------------

func TestAttachmentSelectionDefaultKeepsOnePerParent(t *testing.T) {
	candidates := []candidate{
		{attachmentKey: "ATT00002", attachmentID: 12, parentKey: "PAR00001", parentID: 1},
		{attachmentKey: "ATT00001", attachmentID: 11, parentKey: "PAR00001", parentID: 1},
		{attachmentKey: "ATT00003", attachmentID: 21, parentKey: "PAR00002", parentID: 2},
	}

	kept, skips := selectAttachments(candidates, false)
	wantKept, wantSkips := dedupOnePerParent(candidates)
	if len(kept) != len(wantKept) || len(skips) != len(wantSkips) {
		t.Fatalf("default mode kept %d/skipped %d, dedupOnePerParent kept %d/skipped %d",
			len(kept), len(skips), len(wantKept), len(wantSkips))
	}
	keptKeys := map[string]bool{}
	for _, c := range kept {
		keptKeys[c.attachmentKey] = true
		if c.secondary {
			t.Errorf("%s marked secondary in the default mode, where every document is its parent's primary", c.attachmentKey)
		}
	}
	if !keptKeys["ATT00001"] || !keptKeys["ATT00003"] || len(keptKeys) != 2 {
		t.Errorf("default mode kept %v, want the lowest-itemID PDF of each parent", keptKeys)
	}
	if len(skips) != 1 || skips[0].Key != "ATT00002" || skips[0].Reason != "parent has another PDF attachment" {
		t.Errorf("default mode skips %+v, want ATT00002 with the unchanged reason", skips)
	}
}

func TestAttachmentSelectionAllAttachmentsKeepsSecondaries(t *testing.T) {
	candidates := []candidate{
		{attachmentKey: "ATT00002", attachmentID: 12, parentKey: "PAR00001", parentID: 1},
		{attachmentKey: "ATT00001", attachmentID: 11, parentKey: "PAR00001", parentID: 1},
		{attachmentKey: "ATT00003", attachmentID: 21, parentKey: "PAR00002", parentID: 2},
	}

	kept, skips := selectAttachments(candidates, true)
	if len(skips) != 0 {
		t.Errorf("all-attachments mode skipped %+v", skips)
	}
	if len(kept) != 3 {
		t.Fatalf("all-attachments mode kept %d attachments, want 3", len(kept))
	}
	want := map[string]bool{"ATT00001": false, "ATT00002": true, "ATT00003": false}
	for _, c := range kept {
		got, ok := want[c.attachmentKey]
		if !ok {
			t.Errorf("unexpected attachment %s", c.attachmentKey)
			continue
		}
		if c.secondary != got {
			t.Errorf("%s: secondary=%v, want %v", c.attachmentKey, c.secondary, got)
		}
	}
	for i := 1; i < len(kept); i++ {
		if kept[i-1].attachmentID > kept[i].attachmentID {
			t.Fatalf("all-attachments mode returned an unordered slice: %d after %d",
				kept[i].attachmentID, kept[i-1].attachmentID)
		}
	}
}

// writeZoteroFixture builds the smallest Zotero-shaped database queryCandidates
// can read: two PDFs on one journal article, one on another, one deleted
// attachment, and one PDF whose parent is a note (never a candidate pairing).
func writeZoteroFixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer db.Close()

	stmts := []string{
		`CREATE TABLE itemTypes (itemTypeID INTEGER PRIMARY KEY, typeName TEXT)`,
		`CREATE TABLE items (itemID INTEGER PRIMARY KEY, key TEXT, itemTypeID INTEGER)`,
		`CREATE TABLE itemAttachments (itemID INTEGER PRIMARY KEY, parentItemID INTEGER, contentType TEXT, linkMode INTEGER, path TEXT)`,
		`CREATE TABLE deletedItems (itemID INTEGER PRIMARY KEY)`,
		`INSERT INTO itemTypes VALUES (1, 'journalArticle'), (2, 'note'), (3, 'attachment')`,
		`INSERT INTO items VALUES
			(1, 'PAR00001', 1), (2, 'PAR00002', 1), (3, 'NOTE0001', 2),
			(11, 'ATT00001', 3), (12, 'ATT00002', 3), (13, 'ATT00003', 3),
			(21, 'ATT00004', 3), (31, 'ATT00005', 3)`,
		`INSERT INTO itemAttachments VALUES
			(11, 1, 'application/pdf', 1, 'storage:primary.pdf'),
			(12, 1, 'application/pdf', 1, 'storage:supplement.pdf'),
			(13, 1, 'application/pdf', 1, 'storage:deleted.pdf'),
			(21, 2, 'application/pdf', 1, 'storage:other.pdf'),
			(31, 3, 'application/pdf', 1, 'storage:note-attachment.pdf')`,
		`INSERT INTO deletedItems VALUES (13)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("fixture statement failed: %v\n%s", err, stmt)
		}
	}
}

func TestAttachmentSelectionOverZoteroFixture(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "zotero.sqlite")
	writeZoteroFixture(t, path)

	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	candidates, err := queryCandidates(ctx, db)
	if err != nil {
		t.Fatalf("queryCandidates: %v", err)
	}
	if len(candidates) != 3 {
		t.Fatalf("query returned %d candidates, want 3 (deleted and note-parented rows excluded)", len(candidates))
	}

	kept, skips := selectAttachments(candidates, false)
	if len(kept) != 2 || len(skips) != 1 {
		t.Fatalf("default mode over the fixture kept %d and skipped %d, want 2 and 1", len(kept), len(skips))
	}
	for _, c := range kept {
		if c.attachmentKey == "ATT00002" {
			t.Error("default mode kept the supplement; Measure's baseline corpus would change")
		}
	}

	all, allSkips := selectAttachments(candidates, true)
	if len(all) != 3 || len(allSkips) != 0 {
		t.Fatalf("all-attachments mode kept %d and skipped %d, want 3 and 0", len(all), len(allSkips))
	}
	secondaries := 0
	for _, c := range all {
		if c.secondary {
			secondaries++
			if c.attachmentKey != "ATT00002" {
				t.Errorf("%s marked secondary, want ATT00002", c.attachmentKey)
			}
		}
	}
	if secondaries != 1 {
		t.Errorf("%d secondary attachments found, want 1", secondaries)
	}
}
