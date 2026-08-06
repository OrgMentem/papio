// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package identitycorpus

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"papio/internal/pdf"
	"papio/internal/work"
)

// maxListedPairs bounds how many WrongAccepts and CorrectParks entries Measure
// keeps. A store with hundreds of documents produces tens of thousands of
// mismatched pairs, and a badly regressed identity rule could in principle
// turn a large share of them into wrong accepts; without a cap, printing that
// list would dwarf the report it is meant to summarize. The Counts fields
// stay exact regardless — only the per-pair listings are truncated.
const maxListedPairs = 200

// maxRenderedPairs bounds how many of the (possibly capped-at-200) listed
// pairs Render actually prints. The full list is still useful to a caller
// that wants to inspect Report programmatically; the rendered text report is
// read by a human, and a human does not read 200 lines of evidence.
const maxRenderedPairs = 20

// Counts tallies identity verdicts across a set of pairs.
type Counts struct{ Pass, Review, Reject int }

func (c *Counts) tally(result string) {
	switch result {
	case pdf.IdentityPass:
		c.Pass++
	case pdf.IdentityReview:
		c.Review++
	case pdf.IdentityReject:
		c.Reject++
	}
}

func (c Counts) total() int { return c.Pass + c.Review + c.Reject }

func (c Counts) passRate() float64 {
	if c.total() == 0 {
		return 0
	}
	return float64(c.Pass) / float64(c.total()) * 100
}

// PairResult records one document/metadata pairing a report calls out by
// name: either an own-metadata pair that failed to pass (a "park", filed
// under CorrectParks) or a mismatched pair that passed when it should not
// have (a "wrong accept", filed under WrongAccepts).
type PairResult struct {
	DocKey    string
	MetaKey   string
	DocTitle  string
	MetaTitle string
	Result    string
	Evidence  []string
}

// OffsetBucket is one row of a histogram: a label and how many items fell
// into it. Report uses this same shape for two different tallies —
// OwnIdentifier (where a document prints its own requested identifier) and
// SkipsByReason (why a candidate never became a document at all) — so the
// type itself carries no assumption about which histogram a given slice
// holds.
type OffsetBucket struct {
	Label string
	Count int
}

// Report is the outcome of measuring pdf.MatchIdentity over a corpus of real
// documents against both their own and every other document's metadata.
type Report struct {
	Documents       int
	MetadataRecords int
	CorrectPairs    int
	MismatchedPairs int
	Correct         Counts
	Mismatch        Counts
	WrongAccepts    []PairResult
	CorrectParks    []PairResult
	OwnIdentifier   []OffsetBucket
	// SkipsByReason tallies the candidates that never became a Document, one
	// row per coarse reason class, sorted by descending count then label so
	// two runs over the same library render identically. Measure does not
	// populate this field: Load's skips never reach Measure, and giving
	// Measure a path to them would couple corpus loading to identity
	// measurement for a field that has nothing to do with either document's
	// identity verdict. The caller in cmd/identity-corpus classifies the
	// []Skip it already holds, sorts it, and assigns this field before
	// calling Render. Leave it nil rather than wiring skips through Measure
	// to fill it in.
	SkipsByReason []OffsetBucket
}

// hasUsableMetadata reports whether target carries anything MatchIdentity
// could possibly decide on: a title to token-match, or a strong identifier to
// corroborate. A work request with neither (a bare author-only stub, say) is
// not a defect in identity.go's rules — there is nothing for those rules to
// read — so it is excluded from MetadataRecords, which counts only the
// metadata that actually exercises the rules being measured.
func hasUsableMetadata(target work.Work) bool {
	return target.Title != "" || target.DOI != "" || target.PMID != "" || target.ArXiv != ""
}

// sameWork reports whether a and b describe the same underlying paper, by DOI,
// PMID, arXiv id, or an identical case-folded title. papio deduplicates jobs
// at request time, but a store built up over months of re-imports and manual
// re-runs accumulates more than one job row for the same paper. Without this
// check, pairing one document against a duplicate job's metadata would score
// as a mismatch purely because two rows happen to describe the same work, not
// because the identity rules confused two different papers — manufacturing a
// wrong-accept-shaped failure that has nothing to do with the rules under
// test.
func sameWork(a, b work.Work) bool {
	if a.DOI != "" && b.DOI != "" && strings.EqualFold(a.DOI, b.DOI) {
		return true
	}
	if a.PMID != "" && b.PMID != "" && a.PMID == b.PMID {
		return true
	}
	if a.ArXiv != "" && b.ArXiv != "" && strings.EqualFold(a.ArXiv, b.ArXiv) {
		return true
	}
	if a.Title != "" && b.Title != "" && strings.EqualFold(a.Title, b.Title) {
		return true
	}
	return false
}

// requestedIdentifier reports whether target's metadata names any strong
// identifier at all — DOI, PMID, or arXiv id — regardless of whether the
// rules in identity.go could ever use it. classifyOwnIdentifier asks this
// first: a document whose metadata requests nothing has no window question
// to ask, which is a different fact from a document that requests an
// identifier the rules cannot corroborate.
func requestedIdentifier(target work.Work) bool {
	return target.DOI != "" || target.PMID != "" || target.ArXiv != ""
}

// identifierUsable reports whether at least one of target's requested
// identifiers is one identity.go's rules could ever corroborate. A DOI must
// survive work.NormalizeDOI — six documents in one real library store their
// DOI as an EZproxy-rewritten URL
// ("http://dx.doi.org.ezproxy.…/10.1023/A:1009048817385"), which
// NormalizeDOI rejects outright, so no window would ever find a match for
// them. PMID and arXiv carry no equivalent validation in identity.go —
// corroboratingIdentifier accepts either under the right label whenever it
// is non-empty — so their metadata is usable as soon as it is present.
func identifierUsable(target work.Work) bool {
	if target.DOI != "" {
		if _, err := work.NormalizeDOI(target.DOI); err == nil {
			return true
		}
	}
	return strings.TrimSpace(target.PMID) != "" || strings.TrimSpace(target.ArXiv) != ""
}

// Histogram bucket labels, in the fixed order Render and Measure both use.
// The order matters: a document whose only requested identifiers the rules
// can never use is filed under "identifier unusable" before any window is
// even consulted, because no window would change that answer; a document
// with a usable identifier is then filed under the first window that
// contains it, so front matter must be tested before page one, and page one
// before the rest of the excerpt.
const (
	bucketUnusable    = "identifier unusable"
	bucketFrontMatter = "front matter (1 KiB)"
	bucketPageOne     = "page one (4 KiB)"
	bucketLater       = "later in excerpt"
	bucketNotPrinted  = "not printed"
	bucketNoIdentity  = "no identifier requested"
)

// classifyOwnIdentifier reports where doc prints its own requested
// identifier, using pdf.IdentityWindows for the window boundaries and
// pdf.IdentifierPrinted to decide whether a given window corroborates —
// exactly the same functions identity.go's own rules use, so this histogram
// measures where the RULES can see an identifier rather than a second,
// potentially divergent, copy of what counts as a match. A bare substring
// scan here would count a DOI the matcher rejects (wrong normalization) as
// found, and miss one the matcher accepts through letter-spacing tolerance,
// making the bucket a decorative number at exactly the moment someone is
// retuning a bound.
//
// The byline window IdentityWindows also returns is deliberately not a
// bucket here: it exists to scope the author check, not to place an
// identifier, and identity.go's own front-matter/page-one rules never read it
// for that purpose either.
func classifyOwnIdentifier(doc Document) string {
	target := doc.Work
	if !requestedIdentifier(target) {
		return bucketNoIdentity
	}
	if !identifierUsable(target) {
		return bucketUnusable
	}
	frontMatter, _, pageOne := pdf.IdentityWindows(doc.Text)
	switch {
	case pdf.IdentifierPrinted(frontMatter, target):
		return bucketFrontMatter
	case pdf.IdentifierPrinted(pageOne, target):
		// pageOne is itself a prefix of frontMatter's superset, so a hit here
		// that missed the frontMatter check above lies strictly beyond it.
		return bucketPageOne
	case pdf.IdentifierPrinted(doc.Text, target):
		return bucketLater
	default:
		return bucketNotPrinted
	}
}

// Measure runs pdf.MatchIdentity over every document in docs against its own
// metadata (which should always pass) and against every other document's
// metadata (which should never pass), and tabulates a window-placement
// histogram for each document's own identifier.
func Measure(docs []Document) Report {
	report := Report{Documents: len(docs)}

	for _, doc := range docs {
		if hasUsableMetadata(doc.Work) {
			report.MetadataRecords++
		}
	}

	for _, doc := range docs {
		decision := pdf.MatchIdentity(doc.Text, doc.Work)
		report.Correct.tally(decision.Result)
		report.CorrectPairs++
		if decision.Result != pdf.IdentityPass && len(report.CorrectParks) < maxListedPairs {
			report.CorrectParks = append(report.CorrectParks, PairResult{
				DocKey:    doc.Key,
				MetaKey:   doc.Key,
				DocTitle:  doc.Work.Title,
				MetaTitle: doc.Work.Title,
				Result:    decision.Result,
				Evidence:  decision.Evidence,
			})
		}
	}

	for _, doc := range docs {
		for _, other := range docs {
			if doc.Key == other.Key || sameWork(doc.Work, other.Work) {
				continue
			}
			decision := pdf.MatchIdentity(doc.Text, other.Work)
			report.Mismatch.tally(decision.Result)
			report.MismatchedPairs++
			if decision.Result == pdf.IdentityPass && len(report.WrongAccepts) < maxListedPairs {
				report.WrongAccepts = append(report.WrongAccepts, PairResult{
					DocKey:    doc.Key,
					MetaKey:   other.Key,
					DocTitle:  doc.Work.Title,
					MetaTitle: other.Work.Title,
					Result:    decision.Result,
					Evidence:  decision.Evidence,
				})
			}
		}
	}

	buckets := map[string]int{}
	for _, doc := range docs {
		buckets[classifyOwnIdentifier(doc)]++
	}
	for _, label := range []string{bucketUnusable, bucketFrontMatter, bucketPageOne, bucketLater, bucketNotPrinted, bucketNoIdentity} {
		report.OwnIdentifier = append(report.OwnIdentifier, OffsetBucket{Label: label, Count: buckets[label]})
	}

	sortPairResults(report.WrongAccepts)
	sortPairResults(report.CorrectParks)

	return report
}

// sortPairResults orders by DocKey then MetaKey so two runs over the same
// library — where map iteration and goroutine scheduling in the loader give no
// ordering guarantee — always render an identical report.
func sortPairResults(pairs []PairResult) {
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].DocKey != pairs[j].DocKey {
			return pairs[i].DocKey < pairs[j].DocKey
		}
		return pairs[i].MetaKey < pairs[j].MetaKey
	})
}

// truncateTitle bounds a title to roughly limit runes so a listing of 20
// pairs stays one screen tall; the pair's evidence, not its full title, is
// what a reader is there to check.
func truncateTitle(title string, limit int) string {
	if title == "" {
		return "(untitled)"
	}
	r := []rune(title)
	if len(r) <= limit {
		return title
	}
	return string(r[:limit]) + "…"
}

func percent(n, of int) float64 {
	if of == 0 {
		return 0
	}
	return float64(n) / float64(of) * 100
}

// skipReasonOutputCap is the exact label cmd/identity-corpus assigns a
// Report.SkipsByReason row for a candidate that extraction dropped for
// exceeding the output-size cap (see the field comment on Report). Render
// matches on this literal to decide whether to print the book-bias sentence
// below, so the string is a contract between the two packages, not free text
// — changing it here without changing the classifier silently turns the
// sentence off.
const skipReasonOutputCap = "output cap"

// Render renders the report as aligned plain text with no ANSI colour, so it
// reads the same in a terminal, a log file, or a CI artifact.
func (r Report) Render() string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)

	fmt.Fprintf(w, "identitycorpus report\n")
	fmt.Fprintf(w, "documents\t%d\n", r.Documents)
	fmt.Fprintf(w, "metadata records\t%d\n", r.MetadataRecords)
	fmt.Fprintf(w, "correct pairs (doc vs its own metadata)\t%d\n", r.CorrectPairs)
	fmt.Fprintf(w, "mismatched pairs (doc vs every other document's metadata)\t%d\n", r.MismatchedPairs)
	w.Flush()

	fmt.Fprintf(w, "\nskipped candidates (excluded from the corpus counted above, by reason)\n")
	if len(r.SkipsByReason) == 0 {
		fmt.Fprintf(w, "no candidates skipped\n")
	} else {
		fmt.Fprintf(w, "reason\tcount\n")
		var outputCap int
		for _, bucket := range r.SkipsByReason {
			fmt.Fprintf(w, "%s\t%d\n", bucket.Label, bucket.Count)
			if bucket.Label == skipReasonOutputCap {
				outputCap = bucket.Count
			}
		}
		// The bias is stated qualitatively, not as a book/article breakdown, because
		// Document (corpus.go) carries no parent-item-type field for Measure to
		// bucket candidates by — inventing one here to print a ratio would credit
		// this run with a measurement it never took. The reference library's own
		// book/article ratio lives in dev/identity-corpus.md as an attributed,
		// one-time observation instead of a claim this run repeats about itself.
		if outputCap > 0 {
			fmt.Fprintf(w, "output cap alone accounts for %d of the skips above — extraction drops long documents before they ever become a candidate, which falls hardest on book-length works, so this corpus under-represents them relative to the library it was drawn from\n", outputCap)
		}
	}
	w.Flush()

	fmt.Fprintf(w, "\ncorrect pairs — every one of these SHOULD pass\n")
	fmt.Fprintf(w, "pass\treview\treject\tpass rate\n")
	fmt.Fprintf(w, "%d\t%d\t%d\t%.1f%%\n", r.Correct.Pass, r.Correct.Review, r.Correct.Reject, r.Correct.passRate())
	w.Flush()

	fmt.Fprintf(w, "\nmismatched pairs — none of these should ever pass\n")
	fmt.Fprintf(w, "WRONG ACCEPTS\t%d\n", r.Mismatch.Pass)
	fmt.Fprintf(w, "pass\treview\treject\n")
	fmt.Fprintf(w, "%d\t%d\t%d\n", r.Mismatch.Pass, r.Mismatch.Review, r.Mismatch.Reject)
	w.Flush()

	fmt.Fprintf(w, "\nown identifier placement (measures the window constants in identity.go)\n")
	for _, bucket := range r.OwnIdentifier {
		fmt.Fprintf(w, "%s\t%d\t%.1f%%\n", bucket.Label, bucket.Count, percent(bucket.Count, r.Documents))
	}
	w.Flush()

	fmt.Fprintf(w, "\nwrong accepts (mismatched pairs that passed) — the number this harness exists to catch\n")
	if len(r.WrongAccepts) == 0 {
		fmt.Fprintf(w, "none — every mismatched pair was correctly rejected or parked\n")
	} else {
		renderPairs(w, r.WrongAccepts)
	}
	w.Flush()

	fmt.Fprintf(w, "\ncorrect parks (own-metadata pairs that did not pass)\n")
	if len(r.CorrectParks) == 0 {
		fmt.Fprintf(w, "none — every document passed against its own metadata\n")
	} else {
		renderPairs(w, r.CorrectParks)
	}
	w.Flush()

	return b.String()
}

// renderPairs writes up to maxRenderedPairs of pairs as an aligned table.
// pairs may hold up to maxListedPairs entries; only a screenful is ever
// printed, with a trailing note when more were captured than shown.
func renderPairs(w *tabwriter.Writer, pairs []PairResult) {
	fmt.Fprintf(w, "doc key\tmeta key\tresult\tdoc title\tmeta title\tevidence\n")
	shown := pairs
	if len(shown) > maxRenderedPairs {
		shown = shown[:maxRenderedPairs]
	}
	for _, p := range shown {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			p.DocKey, p.MetaKey, p.Result,
			truncateTitle(p.DocTitle, 60), truncateTitle(p.MetaTitle, 60),
			strings.Join(p.Evidence, "; "))
	}
	if len(pairs) > maxRenderedPairs {
		fmt.Fprintf(w, "… %d more not shown (capped at %d captured)\n", len(pairs)-maxRenderedPairs, maxListedPairs)
	}
}
