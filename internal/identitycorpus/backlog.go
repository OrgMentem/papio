// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package identitycorpus

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/ownership"
	"papio/internal/pdf"
	"papio/internal/work"
)

// BacklogCaveat is the one sentence every rendering of this arm must carry.
//
// WHY it is a constant and not a comment: the backlog arm looks exactly like
// a calibration arm — real jobs, real pool, a wrong-bind count — and a reader
// who meets its number without this sentence will read it as "papio's
// production wrong-bind rate at the real pool size". It is not, and it cannot
// be made into one by this code. grab.Grab persists id, title, state,
// quarantine, job and outcome (internal/grab/grab.go:64-82) and NO candidate
// snapshot, while attemptAutoBind enumerates live eligibility at selection
// time (internal/browser/bridge.go:7635-7652). The pool that existed when any
// historical grab settled is therefore not recoverable from this database at
// all, and the pool this arm reads is one instant of one operator's queue —
// not a time-weighted distribution over the grabs that actually ran. Making
// this arm calibration-grade needs event-time pool snapshots recorded going
// forward; nothing in a present-day read can substitute.
const BacklogCaveat = "DESCRIPTIVE ONLY, NOT A RATE: this arm replays one instant of one operator's live eligibility queue. No candidate snapshot is persisted per grab, so the pool a historical grab actually faced is unrecoverable and nothing here may be extrapolated to a production wrong-bind rate or used alone to choose a pool cap."

// backlogRowMarker is stamped into every rendered row of this arm's tables so
// the caveat survives a reader who scrolls past the banner, copies one line
// into a ticket, or greps the report for a number.
const backlogRowMarker = "descriptive-only"

// EligibilitySource enumerates the candidate-eligible jobs a settled DOI-less
// grab would be offered. The method set is exactly job.Store's
// ListCandidateEligibleJobs, so *job.Store satisfies this directly and a test
// can hand the real store to the real enumeration; BacklogEligibility below is
// the read-only adapter for an operator's live database.
type EligibilitySource interface {
	ListCandidateEligibleJobs(ctx context.Context) ([]job.CandidateEligibleJob, error)
}

// BacklogEligibility reads an operator's live papio store strictly read-only.
//
// WHY not store.Open: store.Open is the daemon's startup path. It MkdirAlls
// the data directory, applies numbered migrations and opens the database
// read-write in WAL mode (internal/store/store.go:44-69). Pointing that at the
// live database of a running daemon to take a measurement would have this
// harness migrate an operator's real store as a side effect of reporting on
// it. mode=ro at the driver level is the same discipline
// internal/openalexyield/store.go already applies for the same reason, and the
// same one corpus.go uses against the Zotero library.
type BacklogEligibility struct {
	db   *sql.DB
	path string
}

// OpenBacklogEligibility opens <dataDir>/papio.db read-only. A missing file is
// reported as os.ErrNotExist so a caller can skip the arm rather than fail the
// run: an operator who has never run the daemon has no backlog to replay, and
// that is not an error in the measurement.
//
// mode=ro forbids every write to the DATABASE — an UPDATE through this handle
// is refused by the driver, and the test pins that — but it does not make the
// open leave no trace on disk. papio.db is a WAL-mode database, and SQLite
// treats the -wal/-shm sidecars as part of opening one for READ. Against a
// store the daemon has checkpointed and closed, where those sidecars are
// currently absent, this connection alone recreates papio.db-wal (empty, no
// frames) and papio.db-shm beside papio.db and leaves them there once closed.
// corpus.go's F2 comment records exactly the same fact for zotero.sqlite. It
// is harmless — the next daemon open reuses them and the operator's data is
// untouched — but it is a visible effect of taking a measurement, so it is
// stated here rather than left to be found.
func OpenBacklogEligibility(dataDir string) (*BacklogEligibility, error) {
	path := filepath.Join(dataDir, "papio.db")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("papio store %s: %w", path, err)
	}
	dsn := "file:" + path + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening papio store %s read-only: %w", path, err)
	}
	return &BacklogEligibility{db: db, path: path}, nil
}

// Close releases the read-only handle.
func (e *BacklogEligibility) Close() error { return e.db.Close() }

// Path is the database this source read, for the report's provenance line.
func (e *BacklogEligibility) Path() string { return e.path }

// ListCandidateEligibleJobs runs the daemon's own pool enumeration against the
// read-only handle.
//
// WHY the Tx form here, when the enumeration this arm is specified to use is
// job.Store.ListCandidateEligibleJobs: the two are the same query. Both call
// queryCandidateEligibleJobs and both then attach BoundDOIs
// (internal/job/candidate_eligibility.go:182-211), so nothing about the
// predicate differs. What differs is only the handle: the non-Tx method hangs
// off *job.Store, which wraps a *store.Store whose connection can only come
// from store.Open — that is, only from the read-write, migrating path this
// type exists to avoid. Restating the SQL locally would be the other way out
// and is strictly worse: the pool predicate is single-sourced on purpose
// (see the CandidateEligibleKind/Status comment at candidate_eligibility.go:15)
// and a second copy here could drift from the one the daemon actually uses,
// which would make every number this arm reports describe a pool production
// never builds. So the enumeration is reused verbatim through the one entry
// point that accepts a foreign handle, inside an explicitly read-only
// transaction.
func (e *BacklogEligibility) ListCandidateEligibleJobs(ctx context.Context) ([]job.CandidateEligibleJob, error) {
	tx, err := e.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("read-only transaction on %s: %w", e.path, err)
	}
	defer func() { _ = tx.Rollback() }()
	jobs, err := job.ListCandidateEligibleJobsTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	return jobs, nil
}

// BacklogBasis names how one document's equivalence class was established, or
// why it could not be. It is recorded per document because "we could not tell"
// is a finding about the operator's data, not a gap to be papered over with a
// guess in either direction.
type BacklogBasis string

const (
	// BasisDurableBinding: an eligible job carries this document's identifier
	// among the DOIs durably bound to it (job.BoundDOIs) WITHOUT its current
	// work record still carrying it — the attested submission or verification
	// anchor, which the store already committed about that job and which
	// survives a later metadata edit.
	BasisDurableBinding BacklogBasis = "durable-binding"
	// BasisCanonicalIdentifier: an eligible job's own metadata carries an
	// identifier that canonicalizes equal to one of the document's, under
	// ownership.NormalizeIdentifier. This is the ordinary case, and it
	// subsumes most durable bindings: job.BoundDOIs always re-adds the
	// current work DOI (internal/job/bound_dois.go:41), so the two bases
	// separate only where the anchor and the current record disagree.
	BasisCanonicalIdentifier BacklogBasis = "canonical-identifier"
	// BasisEstablishedAbsent: the document carries a canonicalizable
	// identifier, every eligible job carries at least one too, and none
	// matches. Absence is then a fact about identifiers rather than a
	// failure to look.
	BasisEstablishedAbsent BacklogBasis = "established-absent"
	// BasisUnestablishedNoDocID: the document carries no canonicalizable
	// identifier, so nothing can be matched or excluded.
	BasisUnestablishedNoDocID BacklogBasis = "unestablished: document has no canonicalizable identifier"
	// BasisUnestablishedOpaqueJob: at least one eligible job carries no
	// canonicalizable identifier, so it can be neither matched to this
	// document nor ruled out as it.
	BasisUnestablishedOpaqueJob BacklogBasis = "unestablished: an eligible job has no canonicalizable identifier"
)

// BacklogTruth records one document's ground-truth determination, kept whether
// it succeeded or not.
type BacklogTruth struct {
	DocKey string
	Basis  BacklogBasis
	Detail string   // the identifier or count the basis rests on
	Keys   []string // the equivalence class, empty for absent and unestablished
}

// BacklogArm is the backlog replay's output: the pools to measure plus the
// three quantities that describe what was actually read. Those three have no
// home in ArmResult and must not be inferred from it — an ArmResult row cannot
// say how many documents were dropped for unresolvable truth, nor that its
// pool size was observed rather than swept.
type BacklogArm struct {
	// Pools is what a caller assigns to CandidateOptions.ExtraPools[ArmBacklog].
	Pools []Pool

	// StorePath is the database the enumeration read, so the report can say
	// which store produced these numbers.
	StorePath string

	// PendingRows is what the eligibility enumeration returned. It is reported
	// from the query and nowhere else: no figure for this is asserted from
	// documentation or memory.
	PendingRows int

	// PoolSizes is the OBSERVED distribution, pool size to number of pools.
	// It is degenerate by construction (see BuildBacklogArm) and the render
	// says so rather than letting a single-bucket histogram imply a sweep.
	PoolSizes map[int]int

	// LibraryDocuments, DOILessDocuments: the admission split, recomputed here
	// with the production predicate so this arm's denominators are its own.
	LibraryDocuments int
	DOILessDocuments int

	// TargetPresent, TargetAbsent, Unestablished partition DOILessDocuments
	// once the enumeration is non-empty.
	TargetPresent int
	TargetAbsent  int
	Unestablished int

	// Truths records every determination, sorted by document key, so a large
	// Unestablished count can be read rather than merely counted.
	Truths []BacklogTruth

	// Caveat is BacklogCaveat, carried in the value so a caller that renders
	// the arm itself still carries it.
	Caveat string

	// ExtractionNote states whether this corpus's text extraction matches the
	// daemon's configured defaults. See backlogExtractionNote.
	ExtractionNote string
}

// BuildBacklogArm builds the backlog arm from the current eligibility pool.
//
// Every pool carries the SAME candidate list: the whole enumeration, in the
// order the enumeration returned it. That is not a simplification, it is what
// attemptAutoBind does (bridge.go:7636-7651) — it takes every eligible job and
// hands the lot to the selector, with no sampling, capping or reordering. Two
// consequences follow and are reported rather than smoothed over: the observed
// pool-size distribution is a single bucket, and this arm must not be swept
// over the {2,5,10,25} pool sizes, because its pool size is an observation and
// not a parameter.
//
// Documents are admitted by the production condition — FrontMatterDOIs empty
// over the same window processSettledGrab reads (bridge.go:7564-7565) — so a
// document with a front-matter DOI, which never reaches selection in
// production, never reaches it here either.
func BuildBacklogArm(ctx context.Context, src EligibilitySource, docs []Document) (BacklogArm, error) {
	if src == nil {
		return BacklogArm{}, errors.New("backlog arm: nil eligibility source")
	}
	jobs, err := src.ListCandidateEligibleJobs(ctx)
	if err != nil {
		return BacklogArm{}, fmt.Errorf("backlog arm: enumerating candidate-eligible jobs: %w", err)
	}

	arm := BacklogArm{
		PendingRows:      len(jobs),
		PoolSizes:        map[int]int{},
		LibraryDocuments: len(docs),
		Caveat:           BacklogCaveat,
		ExtractionNote:   backlogExtractionNote(),
	}
	if live, ok := src.(*BacklogEligibility); ok {
		arm.StorePath = live.Path()
	}

	admitted := make([]Document, 0, len(docs))
	for _, d := range docs {
		if len(pdf.FrontMatterDOIs(d.Text)) == 0 {
			admitted = append(admitted, d)
		}
	}
	arm.DOILessDocuments = len(admitted)

	// An empty queue yields no pools at all rather than a pool of size zero
	// per document. A zero-candidate pool makes SelectAutoBindCandidate return
	// its "no candidates" abstention for every document
	// (candidate_select.go:684-716), which would fill this arm with
	// correct-abstain rows that measure the empty queue and nothing else — the
	// exact shape of a clean result that means nothing was tested.
	if len(jobs) == 0 {
		return arm, nil
	}

	// One candidate slice, shared by every pool. That is not just an
	// allocation saving: it is the claim this arm makes. Production hands the
	// selector one list, so two pools differing in their candidates would be
	// two different observations of a queue that only ever had one state.
	candidates := make([]pdf.BindCandidate, 0, len(jobs))
	for _, j := range jobs {
		candidates = append(candidates, pdf.BindCandidate{
			Key:   j.JobID,
			Work:  j.Work,
			Bound: append([]string(nil), j.BoundDOIs...),
		})
	}
	opaque := opaqueJobs(jobs)

	for _, d := range admitted {
		truth := establishBacklogTruth(d, jobs, opaque)
		arm.Truths = append(arm.Truths, truth)
		switch truth.Basis {
		case BasisUnestablishedNoDocID, BasisUnestablishedOpaqueJob:
			arm.Unestablished++
			continue
		case BasisEstablishedAbsent:
			arm.TargetAbsent++
		default:
			arm.TargetPresent++
		}
		arm.Pools = append(arm.Pools, Pool{
			DocKey:     d.Key,
			Candidates: candidates,
			TrueKeys:   truth.Keys,
			// Provenance stays inside the contracted vocabulary
			// ("identifier" | "adjudicated:<note>"): both bases here are
			// identifier equality under ownership.NormalizeIdentifier, and
			// neither is human adjudication. The finer basis lives in
			// Truths, where it cannot be mistaken for an adjudication.
			Provenance:   "identifier",
			TargetAbsent: truth.Basis == BasisEstablishedAbsent,
		})
	}

	sort.Slice(arm.Pools, func(i, j int) bool { return arm.Pools[i].DocKey < arm.Pools[j].DocKey })
	sort.Slice(arm.Truths, func(i, j int) bool { return arm.Truths[i].DocKey < arm.Truths[j].DocKey })
	for _, p := range arm.Pools {
		arm.PoolSizes[len(p.Candidates)]++
	}
	return arm, nil
}

// opaqueJobs counts the eligible jobs carrying no canonicalizable identifier.
// One such job is enough to make absence unprovable for every document: it
// could be any paper, including this one, and a title comparison is not
// allowed to decide that (see establishBacklogTruth).
func opaqueJobs(jobs []job.CandidateEligibleJob) int {
	n := 0
	for _, j := range jobs {
		if len(canonicalKeys(j.Work, j.BoundDOIs)) == 0 {
			n++
		}
	}
	return n
}

// establishBacklogTruth decides one document's equivalence class, or declines.
//
// The only admissible evidence is canonical identifier equality under
// ownership.NormalizeIdentifier — the version-COLLAPSING relation ADR-0008
// assigns to "is this the same work?" (ownership.go:367-375), which is exactly
// the question a class membership asks. work.Normalize* is the wrong relation
// here: it preserves the arXiv version suffix because acquisition must fetch
// the version it asked for, so under it a preprint v1 and its v2 are different
// works and the same paper would score as a wrong bind.
//
// There is deliberately no title fallback, in either direction. Using an
// identical title to CREATE a class would file two genuinely different papers
// together — the manifest's case06 pair (candidatecorpus/manifest.json:217-260)
// is same-title, same-author, different DOI, year and container — and every
// wrong bind so created would be scored as correct. Using an identical title to
// RULE OUT membership is no better founded. So a document that cannot be
// resolved by identifier is excluded and counted, which is the finding.
func establishBacklogTruth(d Document, jobs []job.CandidateEligibleJob, opaque int) BacklogTruth {
	docIDs := canonicalKeys(d.Work, nil)
	if len(docIDs) == 0 {
		return BacklogTruth{DocKey: d.Key, Basis: BasisUnestablishedNoDocID, Detail: "no doi, arxiv id or pmid in the curated record"}
	}

	var keys []string
	var basis BacklogBasis
	var detail string
	for _, j := range jobs {
		// The job's own metadata is checked first. A BoundDOI that is also the
		// current work DOI is not extra evidence — BoundDOIs re-adds it
		// unconditionally — so only a match that the current record CANNOT
		// account for is credited to the durable anchor.
		own := canonicalKeys(j.Work, nil)
		hit, ok := intersect(docIDs, own)
		hitBasis := BasisCanonicalIdentifier
		if !ok {
			hit, ok = intersect(docIDs, canonicalKeys(work.Work{}, j.BoundDOIs))
			hitBasis = BasisDurableBinding
		}
		if !ok {
			continue
		}
		keys = append(keys, j.JobID)
		if detail == "" {
			basis, detail = hitBasis, hit
		}
	}
	if len(keys) > 0 {
		sort.Strings(keys)
		return BacklogTruth{DocKey: d.Key, Basis: basis, Detail: detail, Keys: keys}
	}

	if opaque > 0 {
		return BacklogTruth{
			DocKey: d.Key,
			Basis:  BasisUnestablishedOpaqueJob,
			Detail: fmt.Sprintf("%d of %d eligible jobs carry no canonicalizable identifier", opaque, len(jobs)),
		}
	}
	return BacklogTruth{
		DocKey: d.Key,
		Basis:  BasisEstablishedAbsent,
		Detail: fmt.Sprintf("all %d eligible jobs identifier-bearing, none matching %s", len(jobs), strings.Join(docIDs, " ")),
	}
}

// canonicalKeys returns w's identifiers plus extra raw DOI strings, each
// canonicalized by ownership.NormalizeIdentifier and rendered as its exact
// match key, sorted and deduplicated. ISBN and OpenAlex are absent by
// construction: NormalizeIdentifier accepts only doi, arxiv and pmid, and an
// identifier with no canonical form is dropped rather than compared raw.
func canonicalKeys(w work.Work, extraDOIs []string) []string {
	raw := []ownership.Identifier{
		{Kind: ownership.KindDOI, Value: w.DOI},
		{Kind: ownership.KindArXiv, Value: w.ArXiv},
		{Kind: ownership.KindPMID, Value: w.PMID},
	}
	for _, doi := range extraDOIs {
		raw = append(raw, ownership.Identifier{Kind: ownership.KindDOI, Value: doi})
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, id := range raw {
		norm, ok := ownership.NormalizeIdentifier(id)
		if !ok {
			continue
		}
		key := norm.Key()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// intersect reports the first shared key of two sorted, deduplicated key
// slices.
func intersect(a, b []string) (string, bool) {
	if len(a) == 0 || len(b) == 0 {
		return "", false
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			return a[i], true
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return "", false
}

// backlogExtractionNote states whether the text this arm scores is the text
// production would have scored.
//
// No second extractor is introduced: this arm consumes the Documents Load
// already produced, so it inherits extractAll's pdf.DefaultSemanticOptions
// (corpus.go:1104) verbatim. But the daemon does not run those options — its
// configured defaults are config.Default().PDF — and measuring a different
// input than production sees, silently, would make every number here describe
// a document the daemon never had. The comparison is computed from both
// sources rather than quoted, so it cannot go stale against either.
func backlogExtractionNote() string {
	corpusOpts := pdf.DefaultSemanticOptions()
	daemon := config.Default().PDF
	var diffs []string
	if corpusOpts.MinChars != daemon.MinTextChars {
		diffs = append(diffs, fmt.Sprintf("MinChars %d vs daemon min_text_chars %d", corpusOpts.MinChars, daemon.MinTextChars))
	}
	if corpusOpts.OCRPages != daemon.MaxOCRPages {
		diffs = append(diffs, fmt.Sprintf("OCRPages %d vs daemon max_ocr_pages %d", corpusOpts.OCRPages, daemon.MaxOCRPages))
	}
	if daemon.OCREnabled {
		diffs = append(diffs, "daemon ocr_enabled true")
	}
	if len(diffs) == 0 {
		return "extraction matches the daemon's configured defaults"
	}
	return "EXTRACTION DIVERGES from the daemon's configured defaults (" + strings.Join(diffs, "; ") +
		"), so a document whose text layer is thin enough to sit between the two thresholds is extracted differently here than in production; " +
		fmt.Sprintf("Document.Text is in both cases an excerpt bounded by MaxExcerpt %d bytes", corpusOpts.MaxExcerpt)
}

// Render writes this arm's own section. It is deliberately self-contained:
// PendingRows, the observed pool-size distribution and Unestablished have no
// ArmResult field to live in, and the caveat has to sit next to the numbers
// rather than in a doc comment nobody printing a report will read.
func (a BacklogArm) Render() string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)

	fmt.Fprintf(w, "backlog replay (arm %q)\n", ArmBacklog)
	fmt.Fprintf(w, "%s\n", a.Caveat)
	if a.StorePath != "" {
		fmt.Fprintf(w, "store (read-only)\t%s\n", a.StorePath)
	}
	fmt.Fprintf(w, "%s\n", a.ExtractionNote)
	w.Flush()

	fmt.Fprintf(w, "\nwhat was read\tcount\tnote\n")
	fmt.Fprintf(w, "pending rows returned by the eligibility enumeration\t%d\t%s\n", a.PendingRows, backlogRowMarker)
	fmt.Fprintf(w, "library documents loaded\t%d\t%s\n", a.LibraryDocuments, backlogRowMarker)
	fmt.Fprintf(w, "doi-less documents admitted (production admission condition)\t%d\t%s\n", a.DOILessDocuments, backlogRowMarker)
	fmt.Fprintf(w, "pools built\t%d\t%s\n", len(a.Pools), backlogRowMarker)
	w.Flush()

	fmt.Fprintf(w, "\nground truth\tcount\tnote\n")
	fmt.Fprintf(w, "target present (equivalence class established)\t%d\t%s\n", a.TargetPresent, backlogRowMarker)
	fmt.Fprintf(w, "target absent (absence established, empty class)\t%d\t%s\n", a.TargetAbsent, backlogRowMarker)
	fmt.Fprintf(w, "unestablished (excluded, never guessed into an arm)\t%d\t%s\n", a.Unestablished, backlogRowMarker)
	fmt.Fprintf(w, "the count above is THIS ARM'S OWN and is the authoritative one for it. It is a different quantity from ArmResult.Unestablished on the %q row, which the measurement loop fills with a corpus-level count (admitted documents whose class could not be resolved) plus any pool it had to drop — by this arm's construction, none. So the loop's column never shows the %q exclusions below; that reason exists only here. Do not add the two.\n", ArmBacklog, BasisUnestablishedOpaqueJob)
	fmt.Fprintf(w, "the same fact about the operator's data reaches the two sections by opposite levers: the synthesized arms EXCLUDE an unidentifiable work from the distractor universe, shrinking the pool and surfacing as a nonrepresentative cell, while this arm cannot — its pool is an observation, not a draw — so the unidentifiable job stays in and shrinks the SCORED SET instead. Neither section is contradicting the other.\n")
	fmt.Fprintf(w, "and because this arm covers only the documents the live queue can speak to, its cell renders Representative=false. No number here may be read as coverage of the doi-less population.\n")
	w.Flush()

	if a.Unestablished > 0 {
		fmt.Fprintf(w, "\nunestablished breakdown — this is a finding about the data, not a gap in the run\treason\tcount\n")
		for _, bucket := range a.unestablishedBuckets() {
			fmt.Fprintf(w, "\t%s\t%d\n", bucket.Label, bucket.Count)
		}
		w.Flush()
	}

	fmt.Fprintf(w, "\nobserved pool-size distribution\tpool size\tpools\tnote\n")
	if len(a.PoolSizes) == 0 {
		fmt.Fprintf(w, "\t(none)\t0\t%s\n", backlogRowMarker)
	}
	for _, size := range sortedSizes(a.PoolSizes) {
		fmt.Fprintf(w, "\t%d\t%d\t%s\n", size, a.PoolSizes[size], backlogRowMarker)
	}
	w.Flush()
	fmt.Fprintf(w, "the distribution above is a single bucket by construction: attemptAutoBind hands the selector the entire eligibility enumeration, so every pool is that one pool. It is an observation, not a swept parameter, and this arm is not measured at the {2,5,10,25} pool sizes.\n")
	if a.PendingRows == 1 {
		fmt.Fprintf(w, "the queue held one eligible job, so this arm observed a 1-of-1 selection — which is a real production situation, but not a measurement of how false accepts grow with pool size.\n")
	}
	if a.PendingRows == 0 {
		fmt.Fprintf(w, "the queue was empty, so no pools were built. An empty queue is not a clean result: nothing was tested.\n")
	}
	w.Flush()

	return b.String()
}

// unestablishedBuckets tallies the exclusion reasons, sorted by descending
// count then label, matching Report.SkipsByReason's ordering discipline so two
// runs over the same data render identically.
func (a BacklogArm) unestablishedBuckets() []OffsetBucket {
	counts := map[string]int{}
	for _, t := range a.Truths {
		switch t.Basis {
		case BasisUnestablishedNoDocID, BasisUnestablishedOpaqueJob:
			counts[string(t.Basis)]++
		}
	}
	out := make([]OffsetBucket, 0, len(counts))
	for label, n := range counts {
		out = append(out, OffsetBucket{Label: label, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Label < out[j].Label
	})
	return out
}

func sortedSizes(m map[int]int) []int {
	out := make([]int, 0, len(m))
	for size := range m {
		out = append(out, size)
	}
	sort.Ints(out)
	return out
}
