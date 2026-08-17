// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package identitycorpus

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"papio/internal/ownership"
	"papio/internal/pdf"
	"papio/internal/work"
)

// This file measures pdf.SelectAutoBindCandidate — a 1-of-N selection — where
// Measure (measure.go) scores pdf.MatchIdentity, a 1-of-1 predicate. The two
// answer different questions and neither substitutes for the other: a pairwise
// pass rate cannot express how false-accept grows with pool size, and it has no
// analogue at all for the everyday case where the paper a grab belongs to is
// not pending.
//
// Nothing here changes production behaviour. It builds pools, calls the shipped
// selector, and counts. A defect it finds is reported, never repaired here.
//
// Two rules shape everything below, and both invalidate the obvious
// implementation:
//
//  1. The measured corpus is the DOI-LESS subset. Production reaches the
//     selector only from processSettledGrab's `len(dois) == 0` branch
//     (internal/browser/bridge.go:7565-7592), where dois is FrontMatterDOIs over
//     the same 1 KiB window the conclusive-identity veto reads. A document with
//     a front-matter DOI never enters selection, so feeding the whole library in
//     would measure a population production never sees and would short-circuit
//     at GateConclusiveVeto for most of it. Both counts are reported:
//     DOILessDocuments bounds everything this instrument can claim.
//
//  2. Ground truth is an EQUIVALENCE CLASS, not a key. A library holds a
//     preprint and its version of record, duplicate rows from re-imports, and
//     occasionally wrong Zotero metadata. Scoring against a single key would
//     score a same-work bind as the cardinal failure — manufacturing the very
//     error the instrument exists to count. Where the class cannot be
//     established the document is EXCLUDED and counted, never guessed.
//
// A consequence of rule 1 worth stating because it looks like coverage and is
// not: for every document in this corpus CheckConclusiveIdentity returns
// VetoAbsent, so GateConclusiveVeto can never terminate a traversal here. Its
// zero in the terminal-gate table means "unreachable by construction", not
// "reached and passed". Render says so.

// BindOutcome is one trial's verdict.
type BindOutcome string

const (
	// BindCorrect: the selector chose a candidate inside the target's
	// equivalence class.
	BindCorrect BindOutcome = "correct-bind"
	// BindWrong: it chose a candidate outside the class, including any bind at
	// all when the class is empty. This is papio's cardinal failure — the wrong
	// paper filed under a right citation — and it is the number this file
	// exists to produce.
	BindWrong BindOutcome = "wrong-bind"
	// BindAbstainOK: it chose nothing and nothing should have been chosen.
	BindAbstainOK BindOutcome = "correct-abstain"
	// BindMissed: the target was present and it chose nothing. A cost in human
	// workload, not in library corruption.
	BindMissed BindOutcome = "missed-bind"
)

// Arm names one deliberate way of building a pool. Per-axis arms vary a single
// property of the distractors; ArmConjunction composes several at once, because
// per-axis arms alone reproduce one level up the methodological error that
// withdrew candidate_auto_bind/1 — the synthetic gate corpus supplied that
// failure's ingredients separately and never composed them into one document.
type Arm string

const (
	ArmRandom        Arm = "random"
	ArmSameAuthor    Arm = "same-author"
	ArmSameVenueYear Arm = "same-venue-year"
	ArmTitleSuperset Arm = "title-superset"
	ArmSameYear      Arm = "same-year"
	// ArmMarkerCorrection and ArmMarkerNonArticle synthesize documents bearing
	// correctionMarkers and nonArticleMarkers vocabulary respectively. They
	// close the gate-coverage hole where a zero trial count read as a gate that
	// held rather than coverage the run never had.
	ArmMarkerCorrection Arm = "marker-correction"
	ArmMarkerNonArticle Arm = "marker-non-article"
	// ArmConjunction is the composed adversary: a synthesized document that
	// carries the target's title, authors and year, cites the target's
	// identifier in body text, and prints its own different identifier past the
	// 1 KiB blind window but inside page one's 4 KiB cap. It is the direct
	// reproduction of the withdrawn failure and the arm whose result matters
	// most.
	ArmConjunction Arm = "conjunction"
	// ArmComposite and ArmBacklog are supplied by callers through
	// CandidateOptions.ExtraPools and measured by the same loop. Nothing here
	// synthesizes them: a real composite needs the all-attachments loader mode
	// and human confirmation of its label, and a backlog pool is the live
	// eligibility enumeration, neither of which this file may invent.
	ArmComposite Arm = "composite"
	ArmBacklog   Arm = "backlog"
)

// allArms is the fixed order every report renders arms in.
var allArms = []Arm{
	ArmRandom, ArmSameAuthor, ArmSameVenueYear, ArmTitleSuperset, ArmSameYear,
	ArmMarkerCorrection, ArmMarkerNonArticle,
	ArmConjunction, ArmComposite, ArmBacklog,
}

// AllArms returns every arm in rendering order. The result is a fresh slice, so
// a caller may sort or filter it without disturbing the package's own order.
func AllArms() []Arm { return append([]Arm(nil), allArms...) }

// ParseArm maps a command-line spelling to an Arm. It is case-insensitive and
// space-trimming and nothing else: an arm name is part of the measurement
// contract, so "sameauthor" is a typo rather than a synonym.
func ParseArm(s string) (Arm, error) {
	want := strings.ToLower(strings.TrimSpace(s))
	for _, arm := range allArms {
		if want == string(arm) {
			return arm, nil
		}
	}
	names := make([]string, 0, len(allArms))
	for _, arm := range allArms {
		names = append(names, string(arm))
	}
	return "", fmt.Errorf("unknown arm %q (valid: %s)", s, strings.Join(names, ", "))
}

// suppliedOnly reports whether an arm is measured exclusively from
// CandidateOptions.ExtraPools.
func suppliedOnly(arm Arm) bool { return arm == ArmComposite || arm == ArmBacklog }

// Provenance labels for Pool.Provenance. "identifier" means the class was
// derived by canonicalizing strong identifiers with
// ownership.NormalizeIdentifier; an "adjudicated:" prefix means a human (or, for
// the synthetic conjunction arm, construction) established it, and names the
// basis. Truth inferred from Document.Work alone is not admissible: Work is the
// Zotero PARENT's record, so a mis-curated row or a preprint/version-of-record
// attachment mismatch would make a wrong bind read as correct-bind.
const (
	provenanceIdentifier  = "identifier"
	provenanceAdjudicated = "adjudicated:operator-supplied"
)

// Pool is one measured selection: the document under test, the candidate jobs
// offered to the selector, and the ground truth to score the outcome against.
type Pool struct {
	DocKey     string
	Candidates []pdf.BindCandidate
	// TrueKeys is the target's equivalence class. EMPTY means target-absent.
	TrueKeys []string
	// Provenance records how TrueKeys was established: "identifier" or
	// "adjudicated:<note>".
	Provenance string
	// TargetAbsent is true iff TrueKeys is empty BY CONSTRUCTION rather than by
	// failure to establish it. The distinction is the whole point: an absent
	// target makes abstention correct, while an unestablished class makes the
	// trial unscorable, and collapsing the two would grade unscorable pools as
	// clean abstentions.
	TargetAbsent bool

	// text overrides the document text this pool is scored against. It is
	// unexported because only synthesized pools need it — ArmConjunction builds
	// a composite document that exists nowhere in the library — while every
	// caller-supplied pool names a real document by DocKey and is scored against
	// that document's own Text. A caller cannot set it, which is correct: a
	// caller supplying arbitrary text alongside a real document key would make
	// DocKey stop identifying what was measured, and DocKey is the sampling
	// unit every safety statistic below is computed over.
	text string
}

// Trial is one evaluated pool.
type Trial struct {
	DocKey       string
	Arm          Arm
	PoolSize     int
	TargetAbsent bool
	Outcome      BindOutcome
	ChosenKey    string
	// Reason is the selector's abstention reason. SelectAutoBindCandidate never
	// returns a blank one (candidate_select.go:684-716), so a blank here is a
	// real defect in the selector rather than a gap in its contract, and this
	// file records it as unexplainedAbstention rather than as an empty string.
	Reason string
	// Evidence is the winning qualification's evidence, on a bind.
	Evidence []string
	// TerminalGate is the OBSERVED terminal gate of the qualification the
	// selector decided on — read off CandidateQualification.Gate, never inferred
	// from a corpus label. See decisiveGate for which qualification is decisive
	// on an abstention.
	TerminalGate string
}

// ArmCounts tallies the four outcomes within one cell.
type ArmCounts struct{ Correct, Wrong, CorrectAbstain, Missed int }

func (c ArmCounts) total() int { return c.Correct + c.Wrong + c.CorrectAbstain + c.Missed }

// ArmResult is one cell of the measurement: one arm, at one pool size, in one
// target-present/absent form. Cells are never pooled into a single headline
// rate — an arm is a deliberate adversary and a size is a deliberate exposure,
// so averaging them reports a number no decision can use.
type ArmResult struct {
	Arm          Arm
	PoolSize     int
	TargetAbsent bool
	Counts       ArmCounts

	// Denominators, declared separately because they answer different
	// questions. The sampling unit for safety is the DOCUMENT: one document
	// reused across arms and sizes contributes many correlated observations, so
	// a per-trial denominator flatters the rate by roughly the replication
	// factor.
	Pools         int // evaluated pools -> operational rate
	UniqueDocs    int // distinct documents -> safety rate
	DocsEverWrong int // documents wrong-bound at least once in this cell
	Eligible      int // pools that COULD have been built for this cell
	Unestablished int // excluded for unresolvable ground truth

	// WrongPoolRate is operational: Counts.Wrong / Pools.
	// WrongDocRate is the safety headline: DocsEverWrong / UniqueDocs.
	// WrongDocBound is a per-document one-sided 95% upper bound (Clopper-
	// Pearson, documents as the unit). At zero observed it is the rule-of-three
	// analogue 1-0.05^(1/n); above zero read WrongDocRate and treat the bound as
	// context only.
	WrongPoolRate, WrongDocRate, WrongDocBound float64

	// Representative is false when Pools < Eligible, so a cell that thinned —
	// same-venue-year at N=25 surviving only for one heavily represented
	// journal — never reads as measuring the axis.
	Representative bool
}

// CandidateReport is the outcome of measuring the selector over a library.
type CandidateReport struct {
	LibraryDocuments int // everything loaded
	DOILessDocuments int // the admitted corpus -- a PRIMARY finding
	Trials           int
	Seed             int64
	PoolSizes        []int // {2, 5, 10, 25}; N=1 is invalid for a 1-of-N selection
	Results          []ArmResult
	GatesObserved    []OffsetBucket // terminal-gate distribution
	WrongBinds       []Trial
	Missed           []Trial
}

// CandidateOptions configures one measurement run.
type CandidateOptions struct {
	// Seed determines every draw. It is recorded in the report, so a run is
	// reproducible from its own output.
	Seed      int64
	PoolSizes []int
	Arms      []Arm
	// ExtraPools carries caller-built pools, measured by the same loop at their
	// GIVEN size rather than swept over PoolSizes. When an arm appears here its
	// synthesized cells are skipped entirely: the caller has taken that arm
	// over, and silently merging supplied pools with synthesized ones would pool
	// two different populations into one cell.
	ExtraPools map[Arm][]Pool
	// TrueClasses maps a document key to its adjudicated equivalence class.
	// Preprint/version-of-record pairs must be enumerated here rather than
	// inferred, since that is the case most likely to be silently wrong.
	TrueClasses map[string][]string
}

// defaultPoolSizes sweeps the sizes a real eligibility pool plausibly reaches.
// N=1 is absent and is rejected if supplied: a 1-of-1 decision is not the
// selection being measured, and the synthetic gate corpus rejects pools below 2
// for the same reason (candidate_gate_test.go:192-195).
var defaultPoolSizes = []int{2, 5, 10, 25}

// unexplainedAbstention stands in for a blank abstention reason. The selector
// cannot produce one today; if this string ever appears in a report, the
// selector regressed and the trial is the evidence.
const unexplainedAbstention = "(DEFECT: selector abstained with no reason)"

// maxListedTrials bounds how many WrongBinds and Missed entries a report keeps,
// for the reason maxListedPairs bounds Measure's pair listings: the Counts stay
// exact regardless, and a badly regressed rule must not produce a report whose
// listing dwarfs its summary.
const maxListedTrials = 200

// maxRenderedTrials bounds how many listed trials Render prints. A human does
// not read 200 lines of evidence; the full listing stays available on the
// struct.
const maxRenderedTrials = 20

// MeasureCandidateSets scores pdf.SelectAutoBindCandidate over pools built from
// docs, restricted to the DOI-less subset and recording each trial's observed
// terminal gate.
//
// docs must be everything the loader produced, not a pre-filtered subset:
// LibraryDocuments and the DOI-less share of it are primary findings, and a
// caller that filtered first would report that share as 100%.
func MeasureCandidateSets(docs []Document, opts CandidateOptions) CandidateReport {
	report := CandidateReport{
		LibraryDocuments: len(docs),
		Seed:             opts.Seed,
		PoolSizes:        normalizePoolSizes(opts.PoolSizes),
	}

	// Sort once, up front. The loader fans out over goroutines and Zotero's
	// rows arrive in no guaranteed order, so every traversal below walks this
	// slice instead of the caller's.
	all := append([]Document(nil), docs...)
	sort.Slice(all, func(i, j int) bool { return all[i].Key < all[j].Key })

	byKey := make(map[string]Document, len(all))
	for _, d := range all {
		byKey[d.Key] = d
	}

	admitted := make([]Document, 0, len(all))
	for _, d := range all {
		if len(pdf.FrontMatterDOIs(d.Text)) == 0 {
			admitted = append(admitted, d)
		}
	}
	report.DOILessDocuments = len(admitted)

	classes := buildEquivalenceClasses(all, opts.TrueClasses)

	// The population every synthesized cell draws from, and the cell-independent
	// Eligible denominator. A document is eligible when production could reach
	// the selector for it (admitted) AND its outcome is scorable (an established
	// class). Anything else is unestablished and excluded, never guessed into an
	// arm.
	var eligibleDocs []Document
	unestablished := 0
	for _, d := range admitted {
		if unscorableTarget(d, classes) {
			unestablished++
			continue
		}
		eligibleDocs = append(eligibleDocs, d)
	}

	b := &builder{
		seed:      opts.Seed,
		classes:   classes,
		universe:  candidateUniverse(all, classes),
		eligible:  eligibleDocs,
		byKey:     byKey,
		admitted:  documentKeySet(admitted),
		eligCount: len(eligibleDocs),
		unestab:   unestablished,
	}

	arms := opts.Arms
	if len(arms) == 0 {
		arms = allArms
	}
	gates := map[string]int{}
	for _, arm := range arms {
		if supplied, ok := opts.ExtraPools[arm]; ok {
			report.Results = append(report.Results, b.measureSupplied(arm, supplied, &report, gates)...)
			continue
		}
		if suppliedOnly(arm) {
			// No pools were supplied for an arm nothing synthesizes. Emit one
			// empty, explicitly nonrepresentative cell rather than nothing at
			// all: an arm that silently vanishes from the table reads as an arm
			// that was clean.
			report.Results = append(report.Results, ArmResult{
				Arm: arm, Eligible: b.eligCount, Unestablished: b.unestab,
			})
			continue
		}
		for _, n := range report.PoolSizes {
			for _, absent := range []bool{false, true} {
				// Marker-gate arms are target-absent only: synthesis closes the
				// correction/non-article coverage hole, not a bind/no-bind choice
				// at target-present.
				if (arm == ArmMarkerCorrection || arm == ArmMarkerNonArticle) && !absent {
					continue
				}
				report.Results = append(report.Results, b.measureCell(arm, n, absent, &report, gates))
			}
		}
	}

	report.GatesObserved = gateBuckets(gates)
	sortTrials(report.WrongBinds)
	sortTrials(report.Missed)
	return report
}

// normalizePoolSizes drops sizes below 2, deduplicates, and sorts ascending.
// N=1 cannot measure a 1-of-N selection, so it is dropped rather than measured;
// a caller with a command line should reject it before it gets here so the
// operator sees an error instead of a silently shorter sweep.
func normalizePoolSizes(sizes []int) []int {
	if len(sizes) == 0 {
		return append([]int(nil), defaultPoolSizes...)
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(sizes))
	for _, n := range sizes {
		if n < 2 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	if len(out) == 0 {
		return append([]int(nil), defaultPoolSizes...)
	}
	return out
}

// unscorableTarget reports whether a document cannot serve as a measured target.
//
// Two reasons, and the second is a correctness requirement rather than a
// convenience. A secondary attachment (a supplement, alternate scan or cover
// sheet, marked by the all-attachments loader mode) inherits its PARENT's
// curated Work, so its metadata describes the primary PDF rather than its own
// bytes. Deriving a class from that metadata would hand a supplement the
// paper's class, and a run that bound the paper's job to a supplement's bytes
// would then score correct-bind — the instrument would launder the exact
// failure it exists to count. Those documents belong to the composite arm,
// which assigns them an empty class deliberately.
func unscorableTarget(d Document, classes equivalenceClasses) bool {
	return d.Secondary || !classes.established(d.Key)
}

func documentKeySet(docs []Document) map[string]bool {
	out := make(map[string]bool, len(docs))
	for _, d := range docs {
		out[d.Key] = true
	}
	return out
}

// equivalenceClasses holds one class per scorable document, with the basis it
// was established on.
type equivalenceClasses struct {
	class      map[string][]string
	provenance map[string]string
}

func (e equivalenceClasses) established(key string) bool {
	_, ok := e.class[key]
	return ok
}

// buildEquivalenceClasses derives one class per document by canonicalizing
// strong identifiers with ownership.NormalizeIdentifier — the
// version-COLLAPSING relation ADR-0008 assigns to "is this the same work?" —
// and taking the transitive closure over shared canonical identifiers, so a
// record carrying both a DOI and an arXiv id unites the preprint and the
// version of record that each carry only one.
//
// It deliberately does NOT reuse sameWork (measure.go:119-133), which is wrong
// in both directions for this purpose: it compares raw exact DOI/arXiv/title and
// exact PMID with no normalizer, so it misses doi.org URL versus bare DOI, arXiv
// v1 versus v2, and PMID leading zeros; and its identical-title fallback would
// SUPPRESS legitimate distractors — a same-title, same-author,
// different-DOI/year/container pair is a genuinely different work and one of the
// most valuable distractors available. There is no title fallback here at all:
// without a canonical identifier a class is not established, and an unestablished
// class excludes the document rather than guessing it.
//
// An adjudicated class is used verbatim for the document it names, with that
// document's own key added if the operator omitted it, and is not symmetrized
// into any other document's class: an adjudication is a statement about the
// document it was recorded for.
func buildEquivalenceClasses(docs []Document, adjudicated map[string][]string) equivalenceClasses {
	uf := newUnionFind()
	byIdentifier := map[string]string{}
	identifiers := make(map[string][]string, len(docs))
	for _, d := range docs {
		keys := canonicalIdentifierKeys(d.Work)
		if len(keys) == 0 {
			continue
		}
		identifiers[d.Key] = keys
		uf.add(d.Key)
		for _, k := range keys {
			if first, ok := byIdentifier[k]; ok {
				uf.union(first, d.Key)
				continue
			}
			byIdentifier[k] = d.Key
		}
	}

	members := map[string][]string{}
	for _, d := range docs {
		if _, ok := identifiers[d.Key]; !ok {
			continue
		}
		root := uf.find(d.Key)
		members[root] = append(members[root], d.Key)
	}

	out := equivalenceClasses{
		class:      make(map[string][]string, len(docs)),
		provenance: make(map[string]string, len(docs)),
	}
	for _, d := range docs {
		if adj, ok := adjudicated[d.Key]; ok && len(adj) > 0 {
			out.class[d.Key] = sortedUnique(append([]string{d.Key}, adj...))
			out.provenance[d.Key] = provenanceAdjudicated
			continue
		}
		if _, ok := identifiers[d.Key]; !ok {
			continue
		}
		out.class[d.Key] = sortedUnique(members[uf.find(d.Key)])
		out.provenance[d.Key] = provenanceIdentifier
	}
	return out
}

// canonicalIdentifierKeys returns w's strong identifiers in the ownership
// package's canonical, version-collapsing form. work.Normalize* is deliberately
// not used: it is version-PRESERVING because acquisition must fetch the exact
// version asked for, and routing "same work?" through it would stop a v2
// preprint matching its v1.
func canonicalIdentifierKeys(w work.Work) []string {
	raw := []ownership.Identifier{
		{Kind: ownership.KindDOI, Value: w.DOI},
		{Kind: ownership.KindArXiv, Value: w.ArXiv},
		{Kind: ownership.KindPMID, Value: w.PMID},
	}
	out := make([]string, 0, len(raw))
	for _, id := range raw {
		if strings.TrimSpace(id.Value) == "" {
			continue
		}
		if norm, ok := ownership.NormalizeIdentifier(id); ok {
			out = append(out, norm.Key())
		}
	}
	return sortedUnique(out)
}

type unionFind struct{ parent map[string]string }

func newUnionFind() *unionFind { return &unionFind{parent: map[string]string{}} }

func (u *unionFind) add(key string) {
	if _, ok := u.parent[key]; !ok {
		u.parent[key] = key
	}
}

func (u *unionFind) find(key string) string {
	u.add(key)
	for u.parent[key] != key {
		u.parent[key] = u.parent[u.parent[key]]
		key = u.parent[key]
	}
	return key
}

func (u *unionFind) union(a, b string) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	// Lowest key wins, so a component's root — and therefore every derived
	// listing — is independent of the order documents arrived in.
	if rb < ra {
		ra, rb = rb, ra
	}
	u.parent[rb] = ra
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	kept := out[:0]
	for i, s := range out {
		if i == 0 || s != out[i-1] {
			kept = append(kept, s)
		}
	}
	return kept
}

func inClass(key string, class []string) bool {
	for _, k := range class {
		if k == key {
			return true
		}
	}
	return false
}

// builder holds everything the pool synthesis for one run needs.
type builder struct {
	seed      int64
	classes   equivalenceClasses
	universe  []Document // one candidate job per distinct work, sorted by key
	eligible  []Document // scorable admitted targets, sorted by key
	byKey     map[string]Document
	admitted  map[string]bool
	eligCount int
	unestab   int
}

// candidateUniverse is the set of works offered as candidate jobs, one per
// equivalence class, sorted by key.
//
// Candidates are drawn from the WHOLE library, not from the admitted subset:
// admission constrains the document under test — production only reaches the
// selector for a DOI-less grab — while a candidate is a pending job, whose own
// metadata routinely carries a DOI. Restricting candidates to DOI-less
// documents would shrink every pool for a reason production does not have.
//
// One document per class, and no secondary attachments, because two candidates
// carrying the same Work would both qualify or both fail together, and a
// resulting "ambiguous: multiple candidates qualify" abstention would be an
// artifact of Zotero re-imports and supplement inheritance rather than a fact
// about the rule. Duplicate pending jobs do occur in a real store; measuring
// that is a separate question from measuring the selector, and conflating them
// would attribute corpus duplication to the rule.
//
// A candidate must also carry at least one CANONICALIZABLE identifier, and this
// one is load-bearing in both directions. ownership.NormalizeIdentifier accepts
// only DOI, arXiv and PMID, so a job whose metadata is ISBN-only, OpenAlex-only
// or bare title canonicalizes to nothing and cannot be proven different from the
// target. Admitting such a candidate as a distractor would (a) leave a
// target-absent pool's absence merely assumed rather than established, since the
// unprovable candidate might BE the target, and (b) score a bind of a
// same-work-but-unidentifiable candidate as the cardinal failure — manufacturing
// the exact error this instrument exists to count. Excluding it shrinks pools and
// can thin a cell, which shows up as NOT REPRESENTATIVE; that is the honest
// reading, and it is the direction to err in.
func candidateUniverse(docs []Document, classes equivalenceClasses) []Document {
	seen := map[string]bool{}
	out := make([]Document, 0, len(docs))
	for _, d := range docs {
		if d.Secondary || len(canonicalIdentifierKeys(d.Work)) == 0 {
			continue
		}
		class := classes.class[d.Key]
		if len(class) == 0 {
			out = append(out, d)
			continue
		}
		root := class[0]
		if seen[root] {
			continue
		}
		seen[root] = true
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// distractorFilter reports whether other may serve as a distractor for base
// under one arm's axis.
type distractorFilter func(base, other Document) bool

func armFilter(arm Arm) distractorFilter {
	switch arm {
	case ArmSameAuthor:
		return func(base, other Document) bool {
			return sharesFamilyName(base.Work.Authors, other.Work.Authors)
		}
	case ArmSameVenueYear:
		return func(base, other Document) bool {
			if base.Work.Container == "" || base.Work.Year == 0 {
				return false
			}
			return base.Work.Year == other.Work.Year &&
				strings.EqualFold(base.Work.Container, other.Work.Container)
		}
	case ArmTitleSuperset:
		return func(base, other Document) bool {
			return titlePrefixOf(other.Work.Title, base.Work.Title)
		}
	case ArmSameYear:
		return func(base, other Document) bool {
			return base.Work.Year != 0 && base.Work.Year == other.Work.Year
		}
	default: // ArmRandom, and the random distractor padding the conjunction arm.
		return func(base, other Document) bool { return true }
	}
}

// titlePrefixOf reports whether candidate is a proper word-boundary prefix of
// printed — the direction that actually probes gate 3. The document prints its
// own longer title, so the candidate matches as a prefix up to the subtitle
// boundary and only the strict trailing-content rule
// (candidateStrictTitleRunMatches) separates it from the work it is not. The
// opposite direction — a candidate title longer than the printed one — can never
// be printed as a line and so tests nothing.
func titlePrefixOf(candidate, printed string) bool {
	c := strings.ToLower(strings.TrimSpace(candidate))
	p := strings.ToLower(strings.TrimSpace(printed))
	if c == "" || p == "" || len(c) >= len(p) {
		return false
	}
	if !strings.HasPrefix(p, c) {
		return false
	}
	// The boundary is compared as a STRING, not a byte: an em dash is three
	// bytes in UTF-8 and a byte comparison against it does not compile, let
	// alone match.
	for _, boundary := range []string{" ", ":", ",", ";", "-", "—", "–"} {
		if strings.HasPrefix(p[len(c):], boundary) {
			return true
		}
	}
	return false
}

// sharesFamilyName reports whether the two author lists share a surname.
//
// This is a deliberately local, approximate tokenizer rather than the rule's own
// familyToken, and the distinction matters: this function decides which POOL to
// build, not which candidate qualifies. Using the rule's tokenizer to select the
// pool would build pools out of the rule's own notion of an author match and
// then measure that notion against itself — the arm would be defined by the
// thing under test. An approximate axis that occasionally admits a distractor
// the rule would not call a shared author is the safe error here: it widens the
// arm, it cannot flatter it.
func sharesFamilyName(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, name := range a {
		if f := poolFamilyName(name); f != "" {
			set[f] = true
		}
	}
	for _, name := range b {
		if f := poolFamilyName(name); f != "" && set[f] {
			return true
		}
	}
	return false
}

func poolFamilyName(author string) string {
	name := author
	if surname, _, ok := strings.Cut(author, ","); ok {
		name = surname
	}
	fields := strings.Fields(strings.ToLower(name))
	for i := len(fields) - 1; i >= 0; i-- {
		token := strings.Trim(fields[i], ".,;:()[]'\"")
		if len([]rune(token)) >= 3 {
			return token
		}
	}
	return ""
}

// poolRand derives the generator for one pool from the run's seed and the cell's
// coordinates, so a cell's draws are identical whatever else the run measured.
// A single shared generator would make every arm's pools depend on which arms
// ran before it, and a report that changes when an unrelated arm is added is not
// reproducible in the sense that matters.
func poolRand(seed int64, arm Arm, size int, absent bool, docKey string) *rand.Rand {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d\x00%s\x00%d\x00%t\x00%s", seed, arm, size, absent, docKey)
	lo := h.Sum64()
	fmt.Fprint(h, "\x00hi")
	hi := h.Sum64()
	return rand.New(rand.NewPCG(lo, hi))
}

// sample draws k documents from a key-sorted slice. It reports false rather than
// padding when the slice is short: padding an adversarial arm with whatever is
// available turns it into the random arm, which would report a clean
// same-venue-year cell that never contained a same-venue distractor.
func sample(rng *rand.Rand, from []Document, k int) ([]Document, bool) {
	if k < 0 || len(from) < k {
		return nil, false
	}
	idx := make([]int, len(from))
	for i := range idx {
		idx[i] = i
	}
	rng.Shuffle(len(idx), func(i, j int) { idx[i], idx[j] = idx[j], idx[i] })
	picked := make([]Document, 0, k)
	for _, i := range idx[:k] {
		picked = append(picked, from[i])
	}
	return picked, true
}

func candidateOf(d Document) pdf.BindCandidate {
	// Bound is empty on purpose: a candidate-eligible job is pending, so it has
	// no durably bound DOI. attemptAutoBind passes whatever the job recorded
	// (bridge.go:7644-7650), and a non-empty Bound can only make the
	// conclusive-identity veto MORE permissive (VetoCompatible instead of
	// VetoForeign), so leaving it empty never flatters the rule.
	return pdf.BindCandidate{Key: d.Key, Work: d.Work}
}

func sortCandidates(cands []pdf.BindCandidate) {
	sort.Slice(cands, func(i, j int) bool { return cands[i].Key < cands[j].Key })
}

// buildPool builds one per-axis pool for base, or reports false when the arm
// cannot fill its distractors.
func (b *builder) buildPool(base Document, arm Arm, n int, absent bool) (Pool, bool) {
	class := b.classes.class[base.Key]
	need := n
	cands := make([]pdf.BindCandidate, 0, n)
	if !absent {
		need = n - 1
		cands = append(cands, candidateOf(base))
	}

	filter := armFilter(arm)
	pool := make([]Document, 0, len(b.universe))
	for _, other := range b.universe {
		if inClass(other.Key, class) {
			continue
		}
		if filter(base, other) {
			pool = append(pool, other)
		}
	}
	picked, ok := sample(poolRand(b.seed, arm, n, absent, base.Key), pool, need)
	if !ok {
		return Pool{}, false
	}
	for _, d := range picked {
		cands = append(cands, candidateOf(d))
	}
	sortCandidates(cands)

	p := Pool{
		DocKey:       base.Key,
		Candidates:   cands,
		Provenance:   b.classes.provenance[base.Key],
		TargetAbsent: absent,
	}
	if !absent {
		p.TrueKeys = class
	}
	return p, true
}

// Conjunction-arm geometry. The whole point of this arm is the OFFSET, so the
// three constants below are the arm.
//
// conjunctionCitedOffset is where the cited-identifier line starts. It must be
// past the 1 KiB blind front-matter window, or FrontMatterDOIs would see the
// cited DOI and the document would never be admitted at all (and the
// conclusive-identity veto would fire). It must be inside the 4 KiB page-one
// window, or gate 5 would never read it. That is exactly the band the withdrawn
// rule was blind in, and exactly where composite case25 places its own
// identifiers (cited at byte 1175, own at 1361, no form feed).
const (
	conjunctionCitedOffset  = 1120
	conjunctionOwnKeySuffix = "~expansion"
	// conjunctionOwnPrefix is the DOI registrant prefix reserved for synthetic
	// documents (10.5555 is the test prefix), so a synthesized own-identifier
	// can never collide with a real one in the operator's library.
	conjunctionOwnPrefix = "10.5555/papio.conjunction."
)

// conjunctionFiller is the padding that pushes the identifier lines past the
// blind window. It carries no identifier, no year token, and no segment that
// begins with a non-article or correction marker, so it moves the offset without
// touching any gate: an "supplementary information" prefix here would terminate
// the traversal at GateNonArticle and the arm would measure the padding.
const conjunctionFiller = "Instrument calibration, sampling frames, ablation settings and the reproducibility checklist for this study are described in the deposit released alongside this article. "

// conjunctionDocument builds the composed adversary: a journal expansion that
// prints the target's title, authors and year as its own front matter, cites the
// target's identifier in body text, and prints its OWN different identifier
// after the blind window.
//
// Every gate of candidate_auto_bind/2 sees a document that agrees with the
// target's job on title, authors and year, and gate 5 finds the target's
// identifier on page one — where it cannot tell a cited identifier from a
// self-asserted one. No form feed is emitted, so identityPageOne is the first
// 4 KiB and both identifier lines fall inside it.
func conjunctionDocument(w work.Work, citedPhrase, ownDOI string) string {
	container := w.Container
	if container == "" {
		container = "Journal of Record"
	}
	var b strings.Builder
	b.WriteString(w.Title)
	b.WriteString("\n\n")
	b.WriteString(strings.Join(w.Authors, ", "))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "%s, volume 8, pages 44-71, %d\n\n", container, w.Year)
	for b.Len() < conjunctionCitedOffset {
		b.WriteString(conjunctionFiller)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Extended from %s, presented at the earlier workshop edition of this study.\n", citedPhrase)
	b.WriteString("This version doubles the evaluation, adds two datasets, and supersedes the earlier one.\n")
	fmt.Fprintf(&b, "Article DOI: %s\n\n", ownDOI)
	b.WriteString("Abstract\nWe revisit the study with an extended evaluation and two additional datasets.\n\n")
	b.WriteString("Keywords: extended evaluation, replication\n")
	return b.String()
}

// citedPhrase renders w's strongest identifier the way a document prints it, in
// the form gate 5's corroboratingIdentifier reads. The value is the
// ownership-canonical one, which for a DOI or an arXiv id is the same string the
// rule looks for. PMID-only metadata is refused rather than approximated: the
// withdrawn failure is DOI-shaped, and an arm built on a shakier print form
// would report a gate-5 outcome that the print form, not the rule, decided.
func citedPhrase(w work.Work) (string, bool) {
	if id, ok := ownership.NormalizeIdentifier(ownership.Identifier{Kind: ownership.KindDOI, Value: w.DOI}); ok {
		return "DOI " + id.Value, true
	}
	if id, ok := ownership.NormalizeIdentifier(ownership.Identifier{Kind: ownership.KindArXiv, Value: w.ArXiv}); ok {
		return "arXiv:" + id.Value, true
	}
	return "", false
}

// buildConjunctionPool synthesizes the composed adversary for base.
//
// Target-absent form: the document is the expansion, and the only correct
// outcome is to bind nothing — the expansion is a different work from every
// candidate. Binding base's job is the reproduction of the withdrawn failure.
//
// Target-present form: the same document, with the expansion's OWN job added to
// the pool. Now something is correct to bind, and the pool contains two jobs
// that differ only in identifier, one of which the document prints as a citation
// and the other as its own. Which one the rule picks is precisely the
// distinction it is accused of not making.
func (b *builder) buildConjunctionPool(base Document, n int, absent bool) (Pool, bool) {
	if base.Work.Title == "" || len(base.Work.Authors) == 0 || base.Work.Year == 0 {
		return Pool{}, false
	}
	cited, ok := citedPhrase(base.Work)
	if !ok {
		return Pool{}, false
	}
	ownDOI := conjunctionOwnPrefix + fmt.Sprintf("%08x", fnvOf(base.Key))
	text := conjunctionDocument(base.Work, cited, ownDOI)
	// The arm is defined by the offset, so verify it rather than trusting the
	// arithmetic: a title long enough to push the header past the blind window,
	// or a container string that happens to contain a DOI, would leave a
	// conclusive DOI inside the first kilobyte and the document would not be one
	// production could ever reach the selector with.
	if len(pdf.FrontMatterDOIs(text)) != 0 {
		return Pool{}, false
	}

	class := b.classes.class[base.Key]
	cands := []pdf.BindCandidate{candidateOf(base)}
	trueKeys := []string(nil)
	need := n - 1
	if !absent {
		ownKey := base.Key + conjunctionOwnKeySuffix
		cands = append(cands, pdf.BindCandidate{Key: ownKey, Work: work.Work{
			DOI:       ownDOI,
			Title:     base.Work.Title,
			Authors:   base.Work.Authors,
			Container: base.Work.Container,
			Year:      base.Work.Year,
		}})
		trueKeys = []string{ownKey}
		need = n - 2
	}

	pool := make([]Document, 0, len(b.universe))
	for _, other := range b.universe {
		if inClass(other.Key, class) {
			continue
		}
		pool = append(pool, other)
	}
	picked, ok := sample(poolRand(b.seed, ArmConjunction, n, absent, base.Key), pool, need)
	if !ok {
		return Pool{}, false
	}
	for _, d := range picked {
		cands = append(cands, candidateOf(d))
	}
	sortCandidates(cands)

	provenance := "adjudicated:synthetic conjunction expansion of " + base.Key +
		" (own " + ownDOI + ", cites " + cited + ")"
	if absent {
		provenance += "; a different work from every candidate, so abstention is the only correct outcome"
	} else {
		provenance += "; only the expansion's own job is a correct bind"
	}
	return Pool{
		DocKey:       base.Key,
		Candidates:   cands,
		TrueKeys:     trueKeys,
		Provenance:   provenance,
		TargetAbsent: absent,
		text:         text,
	}, true
}

func fnvOf(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// measureCell builds and evaluates one synthesized cell.
func (b *builder) measureCell(arm Arm, n int, absent bool, report *CandidateReport, gates map[string]int) ArmResult {
	if (arm == ArmMarkerCorrection || arm == ArmMarkerNonArticle) && !absent {
		return ArmResult{
			Arm: arm, PoolSize: n, TargetAbsent: absent,
			Eligible: b.eligCount, Unestablished: b.unestab,
		}
	}
	pools := make([]Pool, 0, len(b.eligible))
	for _, base := range b.eligible {
		var (
			p  Pool
			ok bool
		)
		switch arm {
		case ArmConjunction:
			p, ok = b.buildConjunctionPool(base, n, absent)
		case ArmMarkerCorrection:
			p, ok = b.buildMarkerCorrectionPool(base, n)
		case ArmMarkerNonArticle:
			p, ok = b.buildMarkerNonArticlePool(base, n)
		default:
			p, ok = b.buildPool(base, arm, n, absent)
		}
		if !ok {
			continue // thinned, not padded; Pools < Eligible records it
		}
		pools = append(pools, p)
	}
	return b.evaluate(arm, n, absent, pools, b.eligCount, b.unestab, report, gates)
}

// measureSupplied evaluates caller-built pools at their GIVEN size, grouped by
// observed pool size and target form. A supplied arm is never swept over
// PoolSizes: the backlog arm's pools all carry the whole live eligibility
// enumeration, so its size is degenerate by construction and sub-sampling it to
// {2,5,10,25} would fabricate pools that never existed.
func (b *builder) measureSupplied(arm Arm, supplied []Pool, report *CandidateReport, gates map[string]int) []ArmResult {
	type cellKey struct {
		size   int
		absent bool
	}
	grouped := map[cellKey][]Pool{}
	dropped := map[cellKey]int{}
	var order []cellKey
	seen := map[cellKey]bool{}

	sorted := append([]Pool(nil), supplied...)
	sort.Slice(sorted, func(i, j int) bool {
		if len(sorted[i].Candidates) != len(sorted[j].Candidates) {
			return len(sorted[i].Candidates) < len(sorted[j].Candidates)
		}
		if sorted[i].TargetAbsent != sorted[j].TargetAbsent {
			return !sorted[i].TargetAbsent
		}
		return sorted[i].DocKey < sorted[j].DocKey
	})

	for _, p := range sorted {
		key := cellKey{size: len(p.Candidates), absent: p.TargetAbsent}
		if !seen[key] {
			seen[key] = true
			order = append(order, key)
		}
		// A supplied pool naming a document production could never reach the
		// selector with is outside the measured population entirely: it counts
		// in neither Eligible nor Pools.
		if !b.admitted[p.DocKey] {
			continue
		}
		resolved, ok := b.resolveSuppliedTruth(p)
		if !ok {
			dropped[key]++
			continue
		}
		grouped[key] = append(grouped[key], resolved)
	}

	results := make([]ArmResult, 0, len(order))
	for _, key := range order {
		results = append(results, b.evaluate(arm, key.size, key.absent, grouped[key],
			b.eligCount, b.unestab+dropped[key], report, gates))
	}
	return results
}

// resolveSuppliedTruth fills in a supplied pool's ground truth, or reports false
// when it cannot be established.
//
// A caller that supplied both TrueKeys and Provenance has established truth
// itself and is taken verbatim. Otherwise TargetAbsent is the ONLY disambiguator
// between "no candidate is correct, by construction" and "the class is unknown":
// the first is measured and abstention is its correct outcome, the second is
// unscorable and excluded. Collapsing them would grade unscorable pools as clean
// abstentions, which is how an instrument reports safety it never measured.
func (b *builder) resolveSuppliedTruth(p Pool) (Pool, bool) {
	if len(p.Candidates) == 0 {
		return Pool{}, false
	}
	if p.TargetAbsent {
		p.TrueKeys = nil
		if p.Provenance == "" {
			p.Provenance = "adjudicated:supplied target-absent pool"
		}
		return p, true
	}
	if len(p.TrueKeys) > 0 && p.Provenance != "" {
		p.TrueKeys = sortedUnique(p.TrueKeys)
		return p, true
	}
	doc, ok := b.byKey[p.DocKey]
	if !ok || unscorableTarget(doc, b.classes) {
		return Pool{}, false
	}
	p.TrueKeys = b.classes.class[p.DocKey]
	p.Provenance = b.classes.provenance[p.DocKey]
	return p, true
}

// evaluate scores every pool of one cell and aggregates it.
func (b *builder) evaluate(arm Arm, n int, absent bool, pools []Pool, eligible, unestablished int, report *CandidateReport, gates map[string]int) ArmResult {
	res := ArmResult{
		Arm:           arm,
		PoolSize:      n,
		TargetAbsent:  absent,
		Pools:         len(pools),
		Eligible:      eligible,
		Unestablished: unestablished,
	}
	docs := map[string]bool{}
	wrongDocs := map[string]bool{}
	for _, p := range pools {
		trial := b.evaluatePool(arm, n, p)
		docs[p.DocKey] = true
		gates[trial.TerminalGate]++
		report.Trials++
		switch trial.Outcome {
		case BindCorrect:
			res.Counts.Correct++
		case BindWrong:
			res.Counts.Wrong++
			wrongDocs[p.DocKey] = true
			if len(report.WrongBinds) < maxListedTrials {
				report.WrongBinds = append(report.WrongBinds, trial)
			}
		case BindAbstainOK:
			res.Counts.CorrectAbstain++
		case BindMissed:
			res.Counts.Missed++
			if len(report.Missed) < maxListedTrials {
				report.Missed = append(report.Missed, trial)
			}
		}
	}
	res.UniqueDocs = len(docs)
	res.DocsEverWrong = len(wrongDocs)
	res.WrongPoolRate = ratio(res.Counts.Wrong, res.Pools)
	res.WrongDocRate = ratio(res.DocsEverWrong, res.UniqueDocs)
	res.WrongDocBound = binomialUpper95(res.DocsEverWrong, res.UniqueDocs)
	// An empty cell is never representative: 0 >= 0 would otherwise let an arm
	// that built nothing render as one that measured everything.
	res.Representative = eligible > 0 && res.Pools >= eligible
	return res
}

// evaluatePool runs the shipped selector over one pool and classifies the
// outcome against the equivalence CLASS.
func (b *builder) evaluatePool(arm Arm, n int, p Pool) Trial {
	text := p.text
	var metadata pdf.MetadataFields
	if text == "" {
		// A caller-supplied or per-axis pool names a real document by
		// DocKey and is scored against that document's own extracted
		// text AND its own embedded metadata; only a synthesized pool
		// (conjunction, marker-gate) sets text directly, and it has no
		// real file behind it to carry metadata for.
		doc := b.byKey[p.DocKey]
		text = doc.Text
		metadata = doc.Metadata
	}
	bindDoc := pdf.BindDocument{Excerpt: text, Metadata: metadata}
	trial := Trial{
		DocKey:       p.DocKey,
		Arm:          arm,
		PoolSize:     len(p.Candidates),
		TargetAbsent: p.TargetAbsent,
	}
	winner, ok, reason := pdf.SelectAutoBindCandidate(bindDoc, p.Candidates)
	if ok {
		trial.ChosenKey = winner.Key
		trial.Evidence = winner.Evidence
		// The gate comes off the qualification the selector itself returned, so
		// it is an observation of the traversal rather than a claim about it.
		trial.TerminalGate = string(winner.Gate)
		if inClass(winner.Key, p.TrueKeys) {
			trial.Outcome = BindCorrect
		} else {
			trial.Outcome = BindWrong
		}
		return trial
	}
	trial.Reason = reason
	if reason == "" {
		trial.Reason = unexplainedAbstention
	}
	if len(p.TrueKeys) == 0 {
		trial.Outcome = BindAbstainOK
	} else {
		trial.Outcome = BindMissed
	}
	trial.TerminalGate = decisiveGate(bindDoc, p)
	return trial
}

// gateLabelNone is the terminal-gate label for a pool with no candidates. The
// selector reports "no candidates" for one and reaches no gate at all; the
// sweep never builds one (N starts at 2), so this appearing in a report means a
// caller supplied an empty pool.
const gateLabelNone = "(no gate reached)"

// decisiveGate returns the observed terminal gate of the qualification an
// ABSTENTION turned on. The selector returns a zero qualification when it
// abstains, so the traversal is re-run here — QualifyCandidate is a pure
// function of (doc, candidate) and the pool order is the same, so this
// observes the same traversal rather than modelling it.
//
// Which qualification is decisive follows the selector's own causality
// (candidate_select.go:684-716):
//
//   - Any Review candidate makes the selector abstain regardless of everything
//     else, and it names the FIRST one in pool order, so that one is decisive.
//   - Otherwise, with the target present, the interesting question is which gate
//     stopped the candidate that should have been bound, so the class member
//     that got furthest is decisive.
//   - Otherwise the question is which gate saved the run, so the candidate that
//     got furthest is decisive.
//
// Ties resolve to pool order, which is key-sorted, so the answer is
// deterministic.
func decisiveGate(doc pdf.BindDocument, p Pool) string {
	if len(p.Candidates) == 0 {
		return gateLabelNone
	}
	quals := make([]pdf.CandidateQualification, len(p.Candidates))
	for i, c := range p.Candidates {
		quals[i] = pdf.QualifyCandidate(doc, c)
	}
	for _, q := range quals {
		if q.Review {
			return string(q.Gate)
		}
	}
	best := -1
	for i, q := range quals {
		if len(p.TrueKeys) > 0 && !inClass(q.Key, p.TrueKeys) {
			continue
		}
		if best < 0 || gateDepth(q.Gate) > gateDepth(quals[best].Gate) {
			best = i
		}
	}
	if best < 0 {
		// Target present but no class member in the pool. buildPool never
		// produces that, and a supplied pool that does is measured as it stands:
		// the deepest gate any candidate reached is what was observed.
		for i, q := range quals {
			if best < 0 || gateDepth(q.Gate) > gateDepth(quals[best].Gate) {
				best = i
			}
		}
	}
	return string(quals[best].Gate)
}

// gateOrder is the rule's own gate order (candidate_select.go:71-78). There are
// SEVEN gates, not five: the non-article and correction-marker checks are gates
// in their own right, they evaluate on DOI-less input, and they can terminate a
// traversal — a report tracking only the five numbered ones would silently omit
// exactly the marker coverage the withdrawn rule lacked.
var gateOrder = []pdf.CandidateGate{
	pdf.GateConclusiveVeto,
	pdf.GateNonArticle,
	pdf.GateCorrection,
	pdf.GateAuthor,
	pdf.GateTitle,
	pdf.GateYear,
	pdf.GateIdentifier,
}

func gateDepth(g pdf.CandidateGate) int {
	for i, known := range gateOrder {
		if known == g {
			return i
		}
	}
	return -1
}

func gateBuckets(counts map[string]int) []OffsetBucket {
	out := make([]OffsetBucket, 0, len(gateOrder)+1)
	seen := map[string]bool{}
	for _, g := range gateOrder {
		out = append(out, OffsetBucket{Label: string(g), Count: counts[string(g)]})
		seen[string(g)] = true
	}
	// Anything the rule reported that this file does not know about is printed
	// rather than dropped: a new gate constant must show up in the report the
	// first time it fires, not the first time someone remembers to list it here.
	var extra []string
	for label := range counts {
		if !seen[label] {
			extra = append(extra, label)
		}
	}
	sort.Strings(extra)
	for _, label := range extra {
		out = append(out, OffsetBucket{Label: label, Count: counts[label]})
	}
	return out
}

func sortTrials(trials []Trial) {
	sort.Slice(trials, func(i, j int) bool {
		a, b := trials[i], trials[j]
		switch {
		case a.DocKey != b.DocKey:
			return a.DocKey < b.DocKey
		case a.Arm != b.Arm:
			return a.Arm < b.Arm
		case a.PoolSize != b.PoolSize:
			return a.PoolSize < b.PoolSize
		default:
			return !a.TargetAbsent && b.TargetAbsent
		}
	})
}

func ratio(n, of int) float64 {
	if of == 0 {
		return 0
	}
	return float64(n) / float64(of)
}

// binomialUpper95 is the exact one-sided 95% Clopper-Pearson upper bound on a
// proportion: the largest p whose binomial CDF still leaves 5% probability of
// having observed k or fewer successes in n trials.
//
// The unit here is always the DOCUMENT, which is what makes the bound
// cluster-aware. One document reused across arms and pool sizes contributes many
// correlated trials, so a rule-of-three over trials — 3/18,960 for a six-arm,
// five-size sweep — is roughly 30x more optimistic than the per-document
// 3/632 it should have reported. Printing the trial-level figure as a 95% bound
// over correlated observations is simply the wrong statistic.
//
// At k=0 this reduces to 1-0.05^(1/n), the correct form of the rule of three.
// Above zero it is still a valid upper bound, but the point estimate is the
// number to read.
func binomialUpper95(k, n int) float64 {
	const alpha = 0.05
	switch {
	case n <= 0:
		return 1 // no documents, no information: the bound is vacuous, not zero.
	case k >= n:
		return 1
	case k < 0:
		return 1
	}
	lo, hi := 0.0, 1.0
	for range 100 {
		mid := (lo + hi) / 2
		if binomialCDF(k, n, mid) > alpha {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// binomialCDF is P(X <= k) for X ~ Binomial(n, p), summed in log space so a
// library-sized n cannot overflow a binomial coefficient.
func binomialCDF(k, n int, p float64) float64 {
	switch {
	case p <= 0:
		return 1
	case p >= 1:
		return 0
	}
	logP := math.Log(p)
	logQ := math.Log1p(-p)
	sum := 0.0
	for i := 0; i <= k; i++ {
		logChoose, _ := math.Lgamma(float64(n + 1))
		lgI, _ := math.Lgamma(float64(i + 1))
		lgNI, _ := math.Lgamma(float64(n - i + 1))
		sum += math.Exp(logChoose - lgI - lgNI + float64(i)*logP + float64(n-i)*logQ)
	}
	if sum > 1 {
		return 1
	}
	return sum
}

// LoadTrueClasses reads adjudicated equivalence classes from a JSON object
// mapping a document key to the candidate keys in its class:
//
//	{"ABCD1234": ["ABCD1234", "WXYZ5678"]}
//
// The document's own key is added if the operator omitted it. An EMPTY class is
// rejected by name, because "the class is empty" and "the class is unknown" are
// different statements and only the second is admissible here — it is expressed
// by leaving the document out of the file, which routes it to Unestablished. A
// missing file is an error rather than an empty map, so a mistyped path cannot
// silently downgrade adjudicated truth to inferred truth.
func LoadTrueClasses(path string) (map[string][]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read true classes: %w", err)
	}
	var raw map[string][]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse true classes %s: %w", path, err)
	}
	out := make(map[string][]string, len(raw))
	for key, members := range raw {
		if len(members) == 0 {
			return nil, fmt.Errorf("true classes %s: document %q has an empty class; omit the document instead to leave its class unestablished", path, key)
		}
		out[key] = sortedUnique(append([]string{key}, members...))
	}
	return out, nil
}

// hasArm reports whether any cell measured arm, so a reading note is printed
// only for a report that actually contains the thing it explains.
func (r CandidateReport) hasArm(arm Arm) bool {
	for _, res := range r.Results {
		if res.Arm == arm && res.Pools > 0 {
			return true
		}
	}
	return false
}

// untestedGates names the gates other than the conclusive-identity veto that no
// trial reached. A gate at zero is coverage the report lacks, and the whole
// reason terminal gates are observed rather than labelled is that a cell which
// looks clean because nothing reached the gate under test must be visibly
// distinct from one that reached it and passed.
func (r CandidateReport) untestedGates() []string {
	var out []string
	for _, bucket := range r.GatesObserved {
		if bucket.Count > 0 || bucket.Label == string(pdf.GateConclusiveVeto) {
			continue
		}
		out = append(out, bucket.Label)
	}
	return out
}

// Render renders the report as aligned plain text with no ANSI colour, so it
// reads the same in a terminal, a log file or a pasted issue. Wrong binds come
// first: they are the cardinal failure, and a reader who stops after the first
// screen must have seen them.
func (r CandidateReport) Render() string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)

	fmt.Fprintf(w, "candidate binding report (%s, as it stands today)\n", pdf.CandidateBindingRule)
	fmt.Fprintf(w, "seed\t%d\n", r.Seed)
	fmt.Fprintf(w, "library documents\t%d\n", r.LibraryDocuments)
	fmt.Fprintf(w, "DOI-less documents (the measured corpus)\t%d\t%.1f%% of the library\n",
		r.DOILessDocuments, percent(r.DOILessDocuments, r.LibraryDocuments))
	fmt.Fprintf(w, "pool sizes measured\t%s\n", joinInts(r.PoolSizes))
	fmt.Fprintf(w, "trials\t%d\n", r.Trials)
	w.Flush()
	fmt.Fprintf(w, "\nproduction reaches this selector only for a grab whose front-matter DOI window is empty, so the DOI-less count above — not the library count — bounds everything this report can claim\n")
	w.Flush()

	fmt.Fprintf(w, "\nWRONG BINDS — the wrong paper filed under a right citation, the failure this instrument exists to count\n")
	if len(r.WrongBinds) == 0 {
		fmt.Fprintf(w, "none in this run — read the per-cell table below before reading that as safety: a cell marked NOT REPRESENTATIVE did not measure its axis\n")
	} else {
		fmt.Fprintf(w, "%d wrong binds\n", len(r.WrongBinds))
		fmt.Fprintf(w, "doc key\tarm\tN\ttarget\tchosen key\tterminal gate\tevidence\n")
		shown := r.WrongBinds
		if len(shown) > maxRenderedTrials {
			shown = shown[:maxRenderedTrials]
		}
		for _, t := range shown {
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
				t.DocKey, t.Arm, t.PoolSize, targetLabel(t.TargetAbsent),
				t.ChosenKey, t.TerminalGate, strings.Join(t.Evidence, "; "))
		}
		if len(r.WrongBinds) > maxRenderedTrials {
			fmt.Fprintf(w, "… %d more not shown (capped at %d captured)\n", len(r.WrongBinds)-maxRenderedTrials, maxListedTrials)
		}
		if r.hasArm(ArmConjunction) {
			fmt.Fprintf(w, "reading a conjunction row: its measured bytes are a SYNTHESIZED expansion, not the library document named in the doc key — that key records which document the composite was derived from, because the document is the sampling unit. A chosen key equal to the doc key there is the arm's designed failure (the rule bound the work the document merely CITES), not a bind of the document's own paper.\n")
		}
	}
	w.Flush()

	fmt.Fprintf(w, "\nmissed binds (target present and uniquely right, chose nothing) — a human-workload cost, not a corruption\n")
	if len(r.Missed) == 0 {
		fmt.Fprintf(w, "none\n")
	} else {
		fmt.Fprintf(w, "%d missed binds\n", len(r.Missed))
		fmt.Fprintf(w, "doc key\tarm\tN\ttarget\tterminal gate\tabstention reason\n")
		shown := r.Missed
		if len(shown) > maxRenderedTrials {
			shown = shown[:maxRenderedTrials]
		}
		for _, t := range shown {
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n",
				t.DocKey, t.Arm, t.PoolSize, targetLabel(t.TargetAbsent), t.TerminalGate, t.Reason)
		}
		if len(r.Missed) > maxRenderedTrials {
			fmt.Fprintf(w, "… %d more not shown (capped at %d captured)\n", len(r.Missed)-maxRenderedTrials, maxListedTrials)
		}
	}
	w.Flush()

	fmt.Fprintf(w, "\nper-cell outcomes — one arm at one pool size in one target form; never pooled into a single rate\n")
	fmt.Fprintf(w, "arm\tN\ttarget\tpools\tdocs\tcorrect\tWRONG\tabstain\tmissed\twrong/pool\twrong/doc\tdoc bound\telig\tunestab\trepresentative\n")
	var thin []ArmResult
	for _, res := range r.Results {
		if !res.Representative {
			thin = append(thin, res)
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%.2f%%\t%.2f%%\t%.2f%%\t%d\t%d\t%s\n",
			res.Arm, res.PoolSize, targetLabel(res.TargetAbsent),
			res.Pools, res.UniqueDocs,
			res.Counts.Correct, res.Counts.Wrong, res.Counts.CorrectAbstain, res.Counts.Missed,
			res.WrongPoolRate*100, res.WrongDocRate*100, res.WrongDocBound*100,
			res.Eligible, res.Unestablished, representativeLabel(res))
	}
	if r.hasArm(ArmConjunction) {
		fmt.Fprintf(w, "the conjunction arm's rate is a REPRODUCTION CHECK, not a prevalence estimate: its documents are synthesized, every one carries the same adversarial geometry, and its wrong-bind rate therefore answers \"does the rule fail on this construction\" with a count of how many library documents' metadata it was reproduced over. It is not the rate at which such documents occur in a library — deliverable 4's composite arm is the arm that bounds that, and only as a lower bound.\n")
	}
	w.Flush()

	if len(thin) > 0 {
		fmt.Fprintf(w, "\nNOT REPRESENTATIVE — these cells thinned, so they measure the pools they could fill and not the axis they name\n")
		fmt.Fprintf(w, "arm\tN\ttarget\tpools\teligible\n")
		for _, res := range thin {
			fmt.Fprintf(w, "%s\t%d\t%s\t%d\t%d\n", res.Arm, res.PoolSize, targetLabel(res.TargetAbsent), res.Pools, res.Eligible)
		}
		fmt.Fprintf(w, "a cell short of distractors is SKIPPED, never padded: padding an adversarial arm with whatever was available would turn it into the random arm and report a clean cell that never contained the adversary\n")
		w.Flush()
	}

	fmt.Fprintf(w, "\nterminal gates observed (the gate the decisive qualification actually stopped at)\n")
	fmt.Fprintf(w, "gate\ttrials\tshare\n")
	for _, bucket := range r.GatesObserved {
		fmt.Fprintf(w, "%s\t%d\t%.1f%%\n", bucket.Label, bucket.Count, percent(bucket.Count, r.Trials))
	}
	fmt.Fprintf(w, "%s is unreachable by construction in this report: every measured document has an empty front-matter DOI window, so the conclusive-identity veto always returns absent. Its zero means untested, not passed.\n", pdf.GateConclusiveVeto)
	if untested := r.untestedGates(); len(untested) > 0 {
		fmt.Fprintf(w, "no trial in this run reached %s. A zero above is coverage this report does not have, never a gate that held.\n", strings.Join(untested, ", "))
	}
	w.Flush()

	fmt.Fprintf(w, "\nhow to read the denominators\n")
	fmt.Fprintf(w, "wrong/pool\toperational: wrong binds over evaluated pools in that cell\n")
	fmt.Fprintf(w, "wrong/doc\tsafety headline: documents wrong-bound at least once, over distinct documents in that cell\n")
	fmt.Fprintf(w, "doc bound\tone-sided 95%% upper bound on the per-DOCUMENT wrong-bind probability (exact Clopper-Pearson). The document is the sampling unit because one document contributes many correlated trials; a per-trial rule of three over the same run is roughly 30x more optimistic and is the wrong statistic.\n")
	fmt.Fprintf(w, "elig\tdocuments that could have entered the cell: admitted (DOI-less) and with an established equivalence class\n")
	fmt.Fprintf(w, "unestab\texcluded for unresolvable ground truth — no canonical identifier, or a secondary attachment whose metadata describes its parent rather than its own bytes. Never guessed into an arm.\n")
	w.Flush()

	return b.String()
}

func targetLabel(absent bool) string {
	if absent {
		return "absent"
	}
	return "present"
}

func representativeLabel(res ArmResult) string {
	if res.Representative {
		return "yes"
	}
	return fmt.Sprintf("NOT REPRESENTATIVE (%d of %d eligible)", res.Pools, res.Eligible)
}

func joinInts(values []int) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, fmt.Sprint(v))
	}
	return strings.Join(parts, ", ")
}
