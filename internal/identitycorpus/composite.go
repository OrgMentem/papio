// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package identitycorpus

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"papio/internal/pdf"
)

// The composite arm of the candidate-binding measurement: real documents that
// are ABOUT another work rather than being it — errata, corrigenda, retraction
// notices, comments and replies, supplements, publisher or repository cover
// sheets, and journal expansions of a conference paper. This is the class that
// withdrew candidate_auto_bind/1 (commit 0c85a52): the rule read a CITED
// identifier as the document identifying itself.
//
// Three properties of this file exist because the obvious implementation of
// each is wrong.
//
// 1. It proposes; it does not label. A signal is evidence, a label is ground
// truth, and this instrument's whole purpose is to count a failure class whose
// definition a heuristic cannot settle. So the proposer writes a reviewable
// file, a human edits it, and an unreviewed proposal is reported UNLABELLED and
// counted as neither composite nor not-composite (never quietly as either).
// Because proposer recall bounds measured prevalence, prevalence from proposals
// alone is a LOWER bound and is rendered as one, and a random sample of
// documents the proposer did NOT flag ships in the same file so recall can be
// bounded from evidence instead of assumed.
//
// 2. Marker vocabulary is read out of the production rule, not restated here.
// markerProbe calls pdf.QualifyCandidate and observes which gate it stopped at,
// so the correction and non-article marker lists (identity.go's unexported
// correctionMarkers/nonArticleMarkers, its wide-gap segment recovery and its
// pointer-phrase exclusion) are single-sourced. A local copy of that vocabulary
// would drift silently, and classifyOwnIdentifier (measure.go) already records
// why a decorative approximation of a rule is worse than no measurement.
//
// 3. A confirmed composite's pool carries an EMPTY equivalence class with
// TargetAbsent set, because the correct behaviour for a composite is to bind
// NOTHING — including when the work it refers to is present in the library,
// which is the interesting case rather than an exception. The referred-to work
// is included as a CANDIDATE, which is what makes the pool adversarial: every
// ingredient of a correct bind is present and the correct answer is still to
// abstain.
type CompositeClass string

// The classes a human may assign. ClassComposite is deliberately available:
// a confirmed composite whose exact kind the reviewer will not commit to is
// still a confirmed composite, and forcing a guess would put invented
// precision into ground truth. ClassNotComposite is a REVIEWED rejection —
// a proposal the human looked at and refused — which is a different fact from
// an unreviewed proposal (ClassUnlabelled).
const (
	ClassUnlabelled   CompositeClass = ""
	ClassNotComposite CompositeClass = "not-composite"
	ClassErratum      CompositeClass = "erratum"
	ClassCorrigendum  CompositeClass = "corrigendum"
	ClassRetraction   CompositeClass = "retraction-notice"
	ClassComment      CompositeClass = "comment-or-reply"
	ClassSupplement   CompositeClass = "supplement"
	ClassCoverSheet   CompositeClass = "cover-sheet"
	ClassExpansion    CompositeClass = "journal-expansion"
	ClassComposite    CompositeClass = "composite"
)

// compositeClasses lists every class a review file may name, in the fixed
// order Render prints them.
var compositeClasses = []CompositeClass{
	ClassErratum, ClassCorrigendum, ClassRetraction, ClassComment,
	ClassSupplement, ClassCoverSheet, ClassExpansion, ClassComposite,
	ClassNotComposite,
}

// IsComposite reports whether c names a composite document, as distinct from
// an unlabelled row or a reviewed rejection.
func (c CompositeClass) IsComposite() bool {
	return c != ClassUnlabelled && c != ClassNotComposite && c.valid()
}

func (c CompositeClass) valid() bool {
	if c == ClassUnlabelled {
		return true
	}
	for _, known := range compositeClasses {
		if c == known {
			return true
		}
	}
	return false
}

// Signal names. Each one is a claim about what the loaded data can actually
// show, and compositeSignalsUnavailable below records the signals this data
// cannot support at all — reporting those honestly is part of the deliverable,
// since a silently absent signal reads as an absent failure class.
const (
	signalCorrectionMarkerText  = "text-correction-marker"
	signalNonArticleMarkerText  = "text-non-article-marker"
	signalCorrectionMarkerTitle = "title-correction-marker"
	signalNonArticleMarkerTitle = "title-non-article-marker"
	signalForeignIdentifier     = "foreign-identifier"
	signalTitleQuotesTitle      = "title-quotes-title"
	signalSecondaryAttachment   = "secondary-attachment"
	signalShortDocument         = "short-document"
	signalNoLongerProposed      = "no-longer-proposed"
)

// compositeSignals is the fixed order Render tallies signals in.
var compositeSignals = []string{
	signalForeignIdentifier, signalCorrectionMarkerText, signalCorrectionMarkerTitle,
	signalNonArticleMarkerText, signalNonArticleMarkerTitle, signalTitleQuotesTitle,
	signalSecondaryAttachment, signalShortDocument, signalNoLongerProposed,
}

// compositeSignalsUnavailable names the signals the plan asks for that this
// data does not support, with what was checked. Each one was verified against
// the loader rather than assumed.
var compositeSignalsUnavailable = []string{
	"page range (\"a two-page notice\"): buildWork (corpus.go) reads title, DOI, ISBN, date, extra and creators only, so work.Work carries no page range and Document carries no page count. " + signalShortDocument + " is the available proxy and it is weaker: it counts extracted characters and excerpt page breaks, not printed pages, and it never proposes on its own.",
	"attachment title or filename (\"Supplementary Material.pdf\"): Document carries the attachment key and the resolved path, never Zotero's attachment title, and this harness keeps resolved paths out of every reported string (PRIV-2 in corpus.go), so a filename is not a signal it may read.",
	"own-identifier attribution for a secondary attachment: Document.Work is the Zotero PARENT's record, so a supplement inherits the article's DOI and has no separate curated identity of its own. " + signalForeignIdentifier + " is therefore structurally unable to fire on a supplement printing its own parent's DOI — that identifier IS its metadata's, as far as the loaded data can tell — and " + signalSecondaryAttachment + " carries that class instead.",
	"cover sheets and journal expansions have no marker vocabulary: the production correction/non-article lists do not cover \"Extended version of\", a repository citation card, or a publisher cover leaf (verified by probing pdf.QualifyCandidate with each shape; all three pass both marker gates). Those two classes are reachable only through " + signalForeignIdentifier + ", " + signalTitleQuotesTitle + " and " + signalSecondaryAttachment + ", which is a real recall limit and the reason the audit sample is not optional.",
}

// CompositeSignal is one piece of evidence for one proposal. Refers names the
// library documents the signal implicates — for a foreign printed identifier,
// the document whose identifier this one prints.
type CompositeSignal struct {
	Name   string   `json:"name"`
	Detail string   `json:"detail,omitempty"`
	Refers []string `json:"refers_to,omitempty"`
}

// CompositeEntry is one row of the review file. The first block is written by
// the proposer and overwritten on every re-run; the second block is owned by
// the human and preserved across re-runs by MergeCompositeReview.
//
// Reviewed is a separate field from Class, rather than Class != "" meaning
// reviewed, so that a row a human deliberately left unclassified is
// distinguishable from one they never opened — and so a typo in the class name
// fails loudly (LoadCompositeReview rejects it) instead of silently reading as
// unlabelled.
type CompositeEntry struct {
	Key              string            `json:"key"`
	ParentKey        string            `json:"parent_key"`
	Secondary        bool              `json:"secondary_attachment"`
	DOILess          bool              `json:"doi_less"`
	Title            string            `json:"title"`
	Proposed         CompositeClass    `json:"proposed_class"`
	Signals          []CompositeSignal `json:"signals"`
	ProposedRefersTo []string          `json:"proposed_refers_to"`

	// Human-owned. Reviewed false means UNLABELLED: counted as neither
	// composite nor not-composite, and never used to build a pool.
	Reviewed bool           `json:"reviewed"`
	Class    CompositeClass `json:"class"`
	RefersTo []string       `json:"refers_to"`
	Note     string         `json:"note"`
}

// CompositeReview is the whole review file: the proposer's output and the
// human's labels in one document, so there is exactly one artifact to edit and
// exactly one to load.
//
// AuditSample is not decoration. It holds documents the proposer did NOT flag,
// drawn at random from the recorded seed, so a reviewer can bound how much the
// proposer missed. Without it "composites are rare in this library" is
// unfalsifiable, and the prevalence number has no upper bound at all.
type CompositeReview struct {
	Seed               int64    `json:"seed"`
	Documents          int      `json:"documents"`
	SignalsUnavailable []string `json:"signals_unavailable"`

	// ForeignBeyondPageOne counts (document, other document) pairs where a
	// foreign identifier appears in the excerpt but past page one's 4 KiB
	// cap. Those are NOT proposed: past page one is where reference lists
	// live, so proposing on them would flag every short paper that cites a
	// library work. They are counted because the number bounds what a
	// wider window would add.
	ForeignBeyondPageOne int `json:"foreign_identifier_beyond_page_one"`

	// MarkerProbeBlocked counts documents whose marker signals could not be
	// observed at all: their front matter names more than one conclusive
	// DOI, so pdf.QualifyCandidate stops at the conclusive-identity veto
	// before either marker gate is reached. Such a document is never
	// DOI-less, so it is outside the admitted corpus anyway, but a blind
	// spot that is counted is a different thing from one that is not.
	MarkerProbeBlocked int `json:"marker_probe_blocked"`

	Proposals   []CompositeEntry `json:"proposals"`
	AuditSample []CompositeEntry `json:"audit_sample"`
}

// CompositeOptions parameterizes the proposer and the pool builder. Seed is
// recorded in the review file and in the summary: every draw this file makes —
// the audit sample and each pool's filler candidates — comes from it, so a run
// is reproducible from the report alone.
type CompositeOptions struct {
	Seed        int64
	AuditSample int   // documents the proposer did not flag, sampled for review; 0 means the default
	PoolSizes   []int // N values to build a pool at; 0 means the default
}

const (
	defaultAuditSample = 25
	// shortDocumentChars is the extracted-character count below which a
	// document is called short. Errata and retraction notices run one or
	// two pages; 6000 characters is roughly two pages of set text. Note
	// Document.Chars is the whole document's count on a fresh extraction
	// and the excerpt's length when the text came from the loader's cache
	// (extractOne, corpus.go) — the two agree for exactly the documents
	// this threshold is about, since a short document's excerpt IS its
	// whole text.
	shortDocumentChars = 6000
	// maxForeignPerDoc bounds how many foreign printed identifiers one
	// proposal lists. A repository cover card can reprint a whole
	// citation block; the count is what matters, and a human reads the
	// file.
	maxForeignPerDoc = 5
	// maxQuotedPerDoc bounds the same way for quoted titles.
	maxQuotedPerDoc = 3
	// minQuotedTitleTokens is how many folded tokens a title must have
	// before it can be read as being quoted inside another. Below that,
	// containment is coincidence.
	minQuotedTitleTokens = 4
)

func (o CompositeOptions) withDefaults() CompositeOptions {
	if o.AuditSample == 0 {
		o.AuditSample = defaultAuditSample
	}
	if o.AuditSample < 0 {
		o.AuditSample = 0
	}
	sizes := make([]int, 0, len(o.PoolSizes))
	for _, n := range o.PoolSizes {
		// N=1 cannot measure a 1-of-N selection, and the synthetic gate
		// corpus rejects pools below 2 for the same reason.
		if n >= 2 {
			sizes = append(sizes, n)
		}
	}
	if len(sizes) == 0 {
		sizes = []int{2, 5, 10, 25}
	}
	sort.Ints(sizes)
	o.PoolSizes = sizes
	return o
}

// Reason prefixes QualifyCandidate writes for the two marker gates. They are
// the observable half of markerProbe's contract with candidate_select.go: the
// gate identifier says which check stopped the traversal, and the reason
// carries the marker the check matched.
const (
	nonArticleReasonPrefix = "non_article_marker: "
	correctionReasonPrefix = "correction_marker: "
	markerProbeKey         = "composite-marker-probe"
)

// markerHit is one observed marker: the vocabulary entry that matched, and
// which of the two gates matched it.
type markerHit struct {
	Marker     string
	Correction bool // false means a non-article marker (supplementary information and friends)
}

// markerProbe reads text's correction and non-article markers by running the
// production rule over it and observing where the rule stopped, so this file
// holds no copy of either vocabulary.
//
// The probe candidate carries no Work — the marker gates are evaluated before
// any target comparison — and Bound is set to the document's OWN conclusive
// front-matter DOIs so gate 1 resolves compatible and the traversal reaches
// the marker gates. That is the whole trick, and it has one blind spot, which
// is returned rather than hidden: a document whose front matter names more than
// one conclusive DOI is VetoAmbiguous regardless of the bound set, so the rule
// stops at gate 1 and no marker is observable. Such a document is never
// DOI-less and so is outside the corpus candidate selection can ever see.
func markerProbe(text string) (hit markerHit, found, blocked bool) {
	if strings.TrimSpace(text) == "" {
		return markerHit{}, false, false
	}
	// This probes gate reachability on text alone. Metadata is
	// deliberately withheld from both callers below — the constructed
	// title-only probe has no file behind it at all, and the real
	// document.Text probe exists only to observe which marker gate the
	// vocabulary check stops at, never to decide a bind. Threading
	// metadata in here would make the marker-vocabulary probe sensitive
	// to a gate this function does not test for, rather than reading
	// vocabulary out of the production rule unchanged.
	q := pdf.QualifyCandidate(pdf.BindDocument{Excerpt: text}, pdf.BindCandidate{Key: markerProbeKey, Bound: pdf.FrontMatterDOIs(text)})
	switch {
	case q.Gate == pdf.GateConclusiveVeto:
		return markerHit{}, false, true
	case q.Gate == pdf.GateNonArticle && strings.HasPrefix(q.Reason, nonArticleReasonPrefix):
		return markerHit{Marker: strings.TrimPrefix(q.Reason, nonArticleReasonPrefix)}, true, false
	case q.Gate == pdf.GateCorrection && strings.HasPrefix(q.Reason, correctionReasonPrefix):
		return markerHit{Marker: strings.TrimPrefix(q.Reason, correctionReasonPrefix), Correction: true}, true, false
	default:
		return markerHit{}, false, false
	}
}

// classFromMarker maps an observed correction marker onto a proposed class.
// Anything the mapping does not recognize proposes the generic ClassComposite
// rather than the nearest guess: the marker vocabulary lives in another
// package and may grow, and a wrong class in a proposal costs a reviewer's
// trust in the whole file.
func classFromMarker(marker string) CompositeClass {
	m := strings.ToLower(strings.TrimSpace(marker))
	switch {
	case strings.HasPrefix(m, "erratum"), strings.HasPrefix(m, "errata"):
		return ClassErratum
	case strings.HasPrefix(m, "corrigendum"), strings.Contains(m, "correction"):
		return ClassCorrigendum
	case strings.HasPrefix(m, "retraction"), strings.HasPrefix(m, "retracted"), strings.HasPrefix(m, "expression of concern"):
		return ClassRetraction
	case strings.HasPrefix(m, "comment"), strings.HasPrefix(m, "commentary"),
		strings.HasPrefix(m, "reply"), strings.HasPrefix(m, "response"), strings.HasPrefix(m, "discussion of"):
		return ClassComment
	default:
		return ClassComposite
	}
}

func intersects(keys []string, set map[string]bool) bool {
	for _, k := range keys {
		if set[k] {
			return true
		}
	}
	return false
}

// stringSet indexes identifier keys or document keys for membership tests.
// Named apart from documentKeySet (candidates.go), which answers the same
// question over []Document.
func stringSet(keys []string) map[string]bool {
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[k] = true
	}
	return set
}

// foldTitle reduces a title to lowercase space-separated alphanumeric tokens,
// so containment can be tested on word boundaries without a tokenizer.
func foldTitle(title string) string {
	var b strings.Builder
	b.Grow(len(title))
	space := true
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			space = false
		default:
			if !space {
				b.WriteByte(' ')
				space = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func tokenCount(folded string) int {
	if folded == "" {
		return 0
	}
	return len(strings.Fields(folded))
}

// quotesTitle reports whether hay contains needle as a whole-token run and is
// strictly longer than it. Both arguments are foldTitle output.
func quotesTitle(hay, needle string) bool {
	if hay == "" || needle == "" || len(hay) <= len(needle) {
		return false
	}
	return strings.Contains(" "+hay+" ", " "+needle+" ")
}

// documentWindows is the detectable range, named once. A document's own
// front matter is the 1 KiB blind window the conclusive-identity veto reads;
// page one is the 4 KiB window identity.go's own-identifier rule reads,
// cut at the first form feed. An identifier on page two, or past
// MaxExcerpt's 16 KiB, is invisible to the rules and to this instrument
// alike, and is reported as such rather than searched for.
type documentWindows struct {
	frontMatter string
	pageOne     string
	excerpt     string
}

func windowsOf(doc Document) documentWindows {
	frontMatter, _, pageOne := pdf.IdentityWindows(doc.Text)
	return documentWindows{frontMatter: frontMatter, pageOne: pageOne, excerpt: doc.Text}
}

// Window labels reused from the histogram bucket names in measure.go, so a
// reader comparing the two reports is comparing the same words.
const (
	windowFrontMatter = bucketFrontMatter
	windowPageOne     = bucketPageOne
	windowBeyond      = bucketLater
)

// foreignIdentifierSignals is the strongest signal available, and the one
// that reproduces the withdrawn failure: a document printing an identifier
// that is NOT its own.
//
// It asks the question through pdf.IdentifierPrinted over pdf.IdentityWindows,
// the same functions the rules use, for the reason classifyOwnIdentifier
// (measure.go) records: a bare substring scan counts an identifier the matcher
// rejects and misses one the matcher accepts through letter-spacing tolerance,
// making the signal decorative exactly where it has to be exact.
//
// Foreignness is decided by canonical identifier, never by title: a printed
// identifier is foreign when it corroborates another document's work and that
// work shares no canonicalized strong identifier with this document's own
// metadata. A hit past page one is counted but NOT proposed — past page one is
// where reference lists live, and proposing there would flag every short paper
// that cites a library work.
func foreignIdentifierSignals(doc Document, docs []Document, own map[string]bool, ids map[string][]string) (signals []CompositeSignal, refers []string, beyond int) {
	win := windowsOf(doc)
	if strings.TrimSpace(win.excerpt) == "" {
		return nil, nil, 0
	}
	for _, other := range docs {
		if other.Key == doc.Key {
			continue
		}
		otherIDs := ids[other.Key]
		if len(otherIDs) == 0 || intersects(otherIDs, own) {
			continue
		}
		var window string
		switch {
		case pdf.IdentifierPrinted(win.frontMatter, other.Work):
			window = windowFrontMatter
		case pdf.IdentifierPrinted(win.pageOne, other.Work):
			window = windowPageOne
		case pdf.IdentifierPrinted(win.excerpt, other.Work):
			beyond++
			continue
		default:
			continue
		}
		if len(signals) >= maxForeignPerDoc {
			continue
		}
		signals = append(signals, CompositeSignal{
			Name:   signalForeignIdentifier,
			Detail: window + ": " + other.Work.Describe(),
			Refers: []string{other.Key},
		})
		refers = append(refers, other.Key)
	}
	return signals, refers, beyond
}

// quotedTitleSignals proposes on a curated title that quotes another
// document's whole title — "Erratum to: <title>", "Comment on '<title>'",
// "Extended version of <title>".
//
// It reads Document.Work.Title only, never the document text: a title quoted
// in body text is ordinarily a citation, and this instrument has no structural
// parser to tell a citation from a self-declaration (the gap
// dev/identity-corpus.md already names as the missing capability). Identical
// titles are excluded outright, in both directions, because manifest case06 is
// a same-title, same-author, different-DOI pair that is a genuinely different
// work: treating an identical title as quotation would suppress one of the most
// valuable distractors in the corpus and flag every duplicate import.
func quotedTitleSignals(doc Document, docs []Document, own map[string]bool, ids map[string][]string, folded map[string]string) (signals []CompositeSignal, refers []string) {
	hay := folded[doc.Key]
	if tokenCount(hay) <= minQuotedTitleTokens {
		return nil, nil
	}
	for _, other := range docs {
		if other.Key == doc.Key {
			continue
		}
		if intersects(ids[other.Key], own) {
			continue
		}
		needle := folded[other.Key]
		if tokenCount(needle) < minQuotedTitleTokens || needle == hay {
			continue
		}
		if !quotesTitle(hay, needle) {
			continue
		}
		if len(signals) >= maxQuotedPerDoc {
			continue
		}
		signals = append(signals, CompositeSignal{
			Name:   signalTitleQuotesTitle,
			Detail: "curated title contains another document's whole title",
			Refers: []string{other.Key},
		})
		refers = append(refers, other.Key)
	}
	return signals, refers
}

// shortDocumentSignal is the available proxy for "a two-page notice", and a
// weak one: see compositeSignalsUnavailable for why the page range this signal
// stands in for is not loadable, and proposeClass for why it never proposes on
// its own. It is still written into the file, including onto audit rows, so a
// reviewer sees what the proposer knew about a document it did not flag.
func shortDocumentSignal(doc Document) (CompositeSignal, bool) {
	if doc.Chars <= 0 || doc.Chars >= shortDocumentChars {
		return CompositeSignal{}, false
	}
	breaks := strings.Count(doc.Text, "\f")
	return CompositeSignal{
		Name:   signalShortDocument,
		Detail: fmt.Sprintf("%d extracted characters, %d page break(s) in the excerpt", doc.Chars, breaks),
	}, true
}

// proposeClass turns evidence into one proposed class, in evidence-strength
// order. A correction marker names its own class; a non-article marker or a
// secondary attachment is a supplement; a foreign printed identifier or a
// quoted title says composite without saying which kind, and deliberately does
// not guess — a cover sheet and a journal expansion produce identical evidence
// here, and the human is the one who can tell them apart.
//
// A short document alone proposes NOTHING. Short papers are common, and a
// signal that flags a large slice of an ordinary library would bury the
// proposals a reviewer must actually read, which costs recall through the
// reviewer rather than through the detector.
func proposeClass(signals []CompositeSignal) CompositeClass {
	for _, s := range signals {
		if s.Name == signalCorrectionMarkerText || s.Name == signalCorrectionMarkerTitle {
			return classFromMarker(s.Detail)
		}
	}
	for _, s := range signals {
		switch s.Name {
		case signalNonArticleMarkerText, signalNonArticleMarkerTitle, signalSecondaryAttachment:
			return ClassSupplement
		}
	}
	for _, s := range signals {
		if s.Name == signalForeignIdentifier || s.Name == signalTitleQuotesTitle {
			return ClassComposite
		}
	}
	return ClassUnlabelled
}

// ProposeComposites proposes which documents belong to the composite class and
// draws the recall-audit sample, returning the reviewable file's contents.
// Nothing here is a label: every row lands unreviewed, and CompositePools
// counts an unreviewed row as neither class.
func ProposeComposites(docs []Document, opts CompositeOptions) CompositeReview {
	opts = opts.withDefaults()

	sorted := make([]Document, len(docs))
	copy(sorted, docs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })

	ids := make(map[string][]string, len(sorted))
	folded := make(map[string]string, len(sorted))
	for _, doc := range sorted {
		ids[doc.Key] = canonicalIdentifierKeys(doc.Work)
		folded[doc.Key] = foldTitle(doc.Work.Title)
	}

	review := CompositeReview{
		Seed:               opts.Seed,
		Documents:          len(sorted),
		SignalsUnavailable: compositeSignalsUnavailable,
	}

	flagged := make(map[string]bool, len(sorted))
	weak := make(map[string][]CompositeSignal, len(sorted))

	for _, doc := range sorted {
		own := stringSet(ids[doc.Key])
		var signals []CompositeSignal

		if hit, found, blocked := markerProbe(doc.Text); blocked {
			review.MarkerProbeBlocked++
		} else if found {
			name := signalNonArticleMarkerText
			if hit.Correction {
				name = signalCorrectionMarkerText
			}
			signals = append(signals, CompositeSignal{Name: name, Detail: hit.Marker})
		}
		if hit, found, _ := markerProbe(doc.Work.Title + "\n"); found {
			name := signalNonArticleMarkerTitle
			if hit.Correction {
				name = signalCorrectionMarkerTitle
			}
			signals = append(signals, CompositeSignal{Name: name, Detail: hit.Marker})
		}

		foreign, foreignRefers, beyond := foreignIdentifierSignals(doc, sorted, own, ids)
		review.ForeignBeyondPageOne += beyond
		signals = append(signals, foreign...)

		quoted, quotedRefers := quotedTitleSignals(doc, sorted, own, ids, folded)
		signals = append(signals, quoted...)

		if doc.Secondary {
			signals = append(signals, CompositeSignal{
				Name:   signalSecondaryAttachment,
				Detail: "not its parent's primary PDF; the parent's curated metadata describes a different file",
				Refers: []string{doc.ParentKey},
			})
		}
		short, isShort := shortDocumentSignal(doc)
		if isShort {
			signals = append(signals, short)
		}

		class := proposeClass(signals)
		if class == ClassUnlabelled {
			if isShort {
				weak[doc.Key] = []CompositeSignal{short}
			}
			continue
		}
		flagged[doc.Key] = true
		review.Proposals = append(review.Proposals, newEntry(doc, class, signals, dedupeSorted(append(foreignRefers, quotedRefers...))))
	}

	review.AuditSample = auditSample(sorted, flagged, weak, opts)
	return review
}

// newEntry builds one review row. RefersTo is an empty slice rather than nil so
// the written JSON shows a human where to type.
func newEntry(doc Document, class CompositeClass, signals []CompositeSignal, refers []string) CompositeEntry {
	return CompositeEntry{
		Key:              doc.Key,
		ParentKey:        doc.ParentKey,
		Secondary:        doc.Secondary,
		DOILess:          len(pdf.FrontMatterDOIs(doc.Text)) == 0,
		Title:            doc.Work.Title,
		Proposed:         class,
		Signals:          signals,
		ProposedRefersTo: refers,
		RefersTo:         []string{},
	}
}

// auditSample draws opts.AuditSample documents the proposer did NOT flag,
// from the recorded seed, and returns them in key order. The draw is over
// documents rather than proposals because that is the population the recall
// question is about: how many composites did the proposer fail to flag.
func auditSample(sorted []Document, flagged map[string]bool, weak map[string][]CompositeSignal, opts CompositeOptions) []CompositeEntry {
	if opts.AuditSample == 0 {
		return nil
	}
	pool := make([]Document, 0, len(sorted))
	for _, doc := range sorted {
		if !flagged[doc.Key] {
			pool = append(pool, doc)
		}
	}
	if len(pool) == 0 {
		return nil
	}
	// One draw, one recorded seed: the shuffle runs over a key-sorted pool,
	// so the sample depends on the seed and the library, never on map
	// iteration or extraction scheduling.
	r := seededRand(opts.Seed)
	r.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	if len(pool) > opts.AuditSample {
		pool = pool[:opts.AuditSample]
	}
	rows := make([]CompositeEntry, 0, len(pool))
	for _, doc := range pool {
		rows = append(rows, newEntry(doc, ClassUnlabelled, weak[doc.Key], nil))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	return rows
}

// seededRand is the one place a random source is constructed, so every draw in
// this file is traceable to CompositeOptions.Seed and to nothing else. The
// second PCG word is a fixed constant rather than time or entropy for the same
// reason.
func seededRand(seed int64) *rand.Rand {
	return rand.New(rand.NewPCG(uint64(seed), 0x9E3779B97F4A7C15))
}

func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// WriteCompositeReview writes the review file at path, mode 0600, through a
// same-directory temp file and a rename. The file holds a human's labels, which
// are ground truth and unrecoverable if a crash truncates them mid-write; the
// operator's own library titles are in it too, hence 0600.
func WriteCompositeReview(path string, r CompositeReview) error {
	data, err := json.MarshalIndent(r, "", " ")
	if err != nil {
		return fmt.Errorf("encoding composite review: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "composite-review-*.tmp")
	if err != nil {
		return fmt.Errorf("creating composite review temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("securing composite review temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing composite review: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing composite review: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("installing composite review: %w", err)
	}
	tmpName = ""
	return nil
}

// LoadCompositeReview reads a review file and validates every human-owned
// field, refusing the file rather than reinterpreting it.
//
// Unknown JSON fields are rejected. That is the point of the strictness: a row
// carrying "reviewd": true would otherwise load as unreviewed, silently
// discarding a human's label and reporting the document as UNLABELLED — a
// failure that looks exactly like an honest absence of evidence. A class the
// vocabulary does not contain, a reviewed row with no class, and a class on an
// unreviewed row are refused for the same reason.
func LoadCompositeReview(path string) (CompositeReview, error) {
	f, err := os.Open(path)
	if err != nil {
		return CompositeReview{}, fmt.Errorf("opening composite review: %w", err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var review CompositeReview
	if err := dec.Decode(&review); err != nil {
		return CompositeReview{}, fmt.Errorf("decoding composite review: %w", err)
	}

	var problems []string
	seen := map[string]bool{}
	check := func(section string, rows []CompositeEntry) {
		for i, row := range rows {
			where := fmt.Sprintf("%s[%d]", section, i)
			switch {
			case row.Key == "":
				problems = append(problems, where+": no key")
			case seen[row.Key]:
				problems = append(problems, where+": duplicate key "+row.Key)
			default:
				seen[row.Key] = true
			}
			if !row.Class.valid() {
				problems = append(problems, fmt.Sprintf("%s (%s): unknown class %q", where, row.Key, row.Class))
				continue
			}
			if row.Reviewed && row.Class == ClassUnlabelled {
				problems = append(problems, fmt.Sprintf("%s (%s): reviewed with no class; set a class or clear reviewed", where, row.Key))
			}
			if !row.Reviewed && row.Class != ClassUnlabelled {
				problems = append(problems, fmt.Sprintf("%s (%s): class %q on an unreviewed row; set reviewed to true", where, row.Key, row.Class))
			}
		}
	}
	check("proposals", review.Proposals)
	check("audit_sample", review.AuditSample)
	if len(problems) > 0 {
		return CompositeReview{}, errors.New("composite review is not usable as ground truth: " + strings.Join(problems, "; "))
	}
	return review, nil
}

// MergeCompositeReview carries prior's human-owned fields onto fresh's rows by
// key, so re-running the proposer over a grown library never destroys a label.
//
// A prior row that fresh no longer proposes keeps its label and is retained
// with a no-longer-proposed signal, because the label is ground truth and the
// proposal was only ever evidence: dropping a reviewed row would quietly shrink
// the denominator of every rate computed from it.
func MergeCompositeReview(fresh, prior CompositeReview) CompositeReview {
	labels := make(map[string]CompositeEntry, len(prior.Proposals)+len(prior.AuditSample))
	for _, row := range prior.Proposals {
		labels[row.Key] = row
	}
	for _, row := range prior.AuditSample {
		labels[row.Key] = row
	}

	out := fresh
	out.Proposals = applyLabels(fresh.Proposals, labels)
	out.AuditSample = applyLabels(fresh.AuditSample, labels)

	present := make(map[string]bool, len(out.Proposals)+len(out.AuditSample))
	for _, row := range out.Proposals {
		present[row.Key] = true
	}
	for _, row := range out.AuditSample {
		present[row.Key] = true
	}
	for _, row := range prior.Proposals {
		if row.Reviewed && !present[row.Key] {
			row.Signals = []CompositeSignal{{Name: signalNoLongerProposed, Detail: "retained for its label; the proposer no longer flags this document"}}
			out.Proposals = append(out.Proposals, row)
			present[row.Key] = true
		}
	}
	for _, row := range prior.AuditSample {
		if row.Reviewed && !present[row.Key] {
			out.AuditSample = append(out.AuditSample, row)
			present[row.Key] = true
		}
	}
	sortEntries(out.Proposals)
	sortEntries(out.AuditSample)
	return out
}

func applyLabels(rows []CompositeEntry, labels map[string]CompositeEntry) []CompositeEntry {
	if len(rows) == 0 {
		return rows
	}
	out := make([]CompositeEntry, len(rows))
	copy(out, rows)
	for i := range out {
		prior, ok := labels[out[i].Key]
		if !ok {
			continue
		}
		out[i].Reviewed = prior.Reviewed
		out[i].Class = prior.Class
		out[i].RefersTo = prior.RefersTo
		out[i].Note = prior.Note
	}
	return out
}

func sortEntries(rows []CompositeEntry) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
}

// SameRows reports whether r and other cover exactly the same documents in
// both sections. A caller deciding whether to write a merged review back over
// a human's file wants this rather than a row count: a document leaving the
// library while another starts being proposed keeps the count identical and is
// exactly the case where the file must be rewritten.
func (r CompositeReview) SameRows(other CompositeReview) bool {
	return sameKeys(r.Proposals, other.Proposals) && sameKeys(r.AuditSample, other.AuditSample)
}

func sameKeys(a, b []CompositeEntry) bool {
	if len(a) != len(b) {
		return false
	}
	keys := make(map[string]bool, len(a))
	for _, row := range a {
		keys[row.Key] = true
	}
	for _, row := range b {
		if !keys[row.Key] {
			return false
		}
	}
	return true
}

// CompositeSummary is the composite arm's own report section. It is separate
// from ArmResult because the questions are different: ArmResult scores
// decisions over pools, while this counts documents, labels and what the
// proposer could not see. Nothing here is a rate over trials.
type CompositeSummary struct {
	Seed             int64
	Documents        int // documents scored
	DOILessDocuments int // the admitted corpus: candidate selection sees only these
	Proposed         int
	Reviewed         int
	Confirmed        int // reviewed and labelled a composite class
	Rejected         int // reviewed and labelled not-composite
	Unlabelled       int // proposed, never reviewed: counted as NEITHER class
	ConfirmedByClass []OffsetBucket

	// ConfirmedMissing counts confirmed labels for documents this run did
	// not load, and ConfirmedWithFrontMatterDOI counts confirmed
	// composites that print a conclusive front-matter DOI: production
	// reaches candidate selection only when that set is empty, so those
	// documents are real composites the selector never sees, and they get
	// no pool.
	ConfirmedMissing            int
	ConfirmedWithFrontMatterDOI int

	// Prevalence is an interval, never a number. The lower bound is
	// confirmed labels over documents scored. The upper bound exists only
	// once audit rows are reviewed, because until then nothing bounds what
	// the proposer missed.
	PrevalenceLowerBound float64
	PrevalenceUpperBound float64
	PrevalenceBounded    bool

	AuditRows       int
	AuditReviewed   int
	AuditComposites int     // proposer misses found by review
	AuditMissRate   float64 // observed, among reviewed audit rows
	AuditMissUpper  float64 // exact one-sided 95% upper bound on the miss rate

	Pools             int
	PoolsWithReferent int
	PoolsBySize       []OffsetBucket
	UnbuildableBySize []OffsetBucket

	SignalCounts         []OffsetBucket
	SignalsUnavailable   []string
	ForeignBeyondPageOne int
	MarkerProbeBlocked   int
}

// CompositePools builds the composite arm's pools from the human-confirmed
// labels in review, and summarizes what the labels support.
//
// Every pool is target-absent BY CONSTRUCTION: TrueKeys is empty and
// TargetAbsent is true, because the correct behaviour for a composite is to
// bind nothing. That holds even when the work the composite refers to is
// present in the library — and that case is the interesting one, so the
// referred-to work is included as a candidate whenever it is loaded. A bind of
// it is a wrong bind, and the whole point of this arm is that every ingredient
// of a "correct" bind is present while the correct answer is still abstain.
//
// Only DOI-less confirmed composites get a pool, since production reaches
// SelectAutoBindCandidate only from processSettledGrab's len(dois) == 0 branch.
func CompositePools(docs []Document, review CompositeReview, opts CompositeOptions) ([]Pool, CompositeSummary) {
	opts = opts.withDefaults()

	sorted := make([]Document, len(docs))
	copy(sorted, docs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })

	byKey := make(map[string]Document, len(sorted))
	doiLess := make([]Document, 0, len(sorted))
	isDOILess := make(map[string]bool, len(sorted))
	for _, doc := range sorted {
		byKey[doc.Key] = doc
		if len(pdf.FrontMatterDOIs(doc.Text)) == 0 {
			doiLess = append(doiLess, doc)
			isDOILess[doc.Key] = true
		}
	}

	summary := CompositeSummary{
		Seed:                 opts.Seed,
		Documents:            len(sorted),
		DOILessDocuments:     len(doiLess),
		SignalsUnavailable:   review.SignalsUnavailable,
		ForeignBeyondPageOne: review.ForeignBeyondPageOne,
		MarkerProbeBlocked:   review.MarkerProbeBlocked,
	}

	signalCounts := map[string]int{}
	classCounts := map[CompositeClass]int{}
	var confirmed []CompositeEntry
	for _, row := range review.Proposals {
		if row.Proposed != ClassUnlabelled {
			summary.Proposed++
		}
		for _, s := range row.Signals {
			signalCounts[s.Name]++
		}
		switch {
		case !row.Reviewed:
			if row.Proposed != ClassUnlabelled {
				summary.Unlabelled++
			}
		case row.Class == ClassNotComposite:
			summary.Reviewed++
			summary.Rejected++
		case row.Class.IsComposite():
			summary.Reviewed++
			summary.Confirmed++
			classCounts[row.Class]++
			confirmed = append(confirmed, row)
		}
	}
	for _, class := range compositeClasses {
		if class == ClassNotComposite {
			continue
		}
		summary.ConfirmedByClass = append(summary.ConfirmedByClass, OffsetBucket{Label: string(class), Count: classCounts[class]})
	}
	summary.SignalCounts = bucketize(signalCounts, compositeSignals)

	summary.AuditRows = len(review.AuditSample)
	for _, row := range review.AuditSample {
		if !row.Reviewed {
			continue
		}
		summary.AuditReviewed++
		if row.Class.IsComposite() {
			summary.AuditComposites++
		}
	}
	if summary.Documents > 0 {
		summary.PrevalenceLowerBound = float64(summary.Confirmed) / float64(summary.Documents)
	}
	if summary.AuditReviewed > 0 {
		summary.AuditMissRate = float64(summary.AuditComposites) / float64(summary.AuditReviewed)
		summary.AuditMissUpper = binomialUpper95(summary.AuditComposites, summary.AuditReviewed)
		if summary.Documents > 0 {
			// The upper bound admits three things the lower bound
			// refuses: every unlabelled proposal could be a
			// composite, and so could the audit-bounded share of
			// every document the proposer never flagged.
			notFlagged := summary.Documents - summary.Proposed
			if notFlagged < 0 {
				notFlagged = 0
			}
			upper := (float64(summary.Confirmed+summary.Unlabelled) + summary.AuditMissUpper*float64(notFlagged)) / float64(summary.Documents)
			summary.PrevalenceUpperBound = math.Min(upper, 1)
			summary.PrevalenceBounded = true
		}
	}

	sortEntries(confirmed)
	r := seededRand(opts.Seed)
	pools := make([]Pool, 0, len(confirmed)*len(opts.PoolSizes))
	built := map[int]int{}
	unbuildable := map[int]int{}
	for _, row := range confirmed {
		doc, ok := byKey[row.Key]
		if !ok {
			summary.ConfirmedMissing++
			continue
		}
		if !isDOILess[doc.Key] {
			summary.ConfirmedWithFrontMatterDOI++
			continue
		}

		referents, fromProposal := referentCandidates(row, byKey)
		fillers := make([]Document, 0, len(doiLess))
		exclude := stringSet(append([]string{doc.Key}, referents...))
		for _, other := range doiLess {
			if !exclude[other.Key] {
				fillers = append(fillers, other)
			}
		}
		// One shuffle per document, then prefixes: the pools this
		// document contributes at N=2 and N=25 nest, so the only thing
		// varying across the sweep is pool size, which is the axis the
		// sweep exists to isolate.
		r.Shuffle(len(fillers), func(i, j int) { fillers[i], fillers[j] = fillers[j], fillers[i] })

		for _, size := range opts.PoolSizes {
			if len(referents)+len(fillers) < size {
				unbuildable[size]++
				continue
			}
			candidates := make([]pdf.BindCandidate, 0, size)
			used := 0
			for _, key := range referents {
				if len(candidates) == size {
					break
				}
				candidates = append(candidates, bindCandidate(byKey[key]))
				used++
			}
			for _, filler := range fillers {
				if len(candidates) == size {
					break
				}
				candidates = append(candidates, bindCandidate(filler))
			}
			pools = append(pools, Pool{
				DocKey:       doc.Key,
				Candidates:   candidates,
				TrueKeys:     nil,
				TargetAbsent: true,
				Provenance:   compositeProvenance(row, referents[:used], fromProposal),
			})
			built[size]++
			if used > 0 {
				summary.PoolsWithReferent++
			}
		}
	}
	summary.Pools = len(pools)
	summary.PoolsBySize = sizeBuckets(built, opts.PoolSizes)
	summary.UnbuildableBySize = sizeBuckets(unbuildable, opts.PoolSizes)
	return pools, summary
}

// referentCandidates resolves the works a composite refers to, preferring the
// human's list and falling back to the proposer's. The fallback is reported in
// the pool's provenance rather than silently equated with a confirmed one: a
// reviewer who confirmed the class without filling in the referent has
// confirmed the class, nothing more.
func referentCandidates(row CompositeEntry, byKey map[string]Document) (keys []string, fromProposal bool) {
	source := row.RefersTo
	if len(source) == 0 {
		source = row.ProposedRefersTo
		fromProposal = len(source) > 0
	}
	for _, key := range dedupeSorted(source) {
		if key == row.Key {
			continue
		}
		if _, ok := byKey[key]; ok {
			keys = append(keys, key)
		}
	}
	return keys, fromProposal
}

// compositeProvenance records how this pool's (empty) equivalence class was
// established, which candidate stands in for the referred-to work, and whether
// that referent was confirmed by a human. Truth inferred from Document.Work is
// never admissible here — Work is the Zotero parent's record — so the only
// basis a composite pool ever cites is adjudication.
func compositeProvenance(row CompositeEntry, referents []string, fromProposal bool) string {
	var b strings.Builder
	b.WriteString("adjudicated:composite ")
	b.WriteString(string(row.Class))
	b.WriteString(" (human-confirmed; empty class by construction — the correct decision is to bind nothing)")
	if row.Note != "" {
		b.WriteString("; reviewer note: ")
		b.WriteString(row.Note)
	}
	switch {
	case len(referents) == 0:
		b.WriteString("; referred-to work not present in the corpus")
	case fromProposal:
		b.WriteString("; referred-to work present as a candidate, from the proposer and NOT human-confirmed: ")
		b.WriteString(strings.Join(referents, ","))
	default:
		b.WriteString("; referred-to work present as a candidate, human-confirmed: ")
		b.WriteString(strings.Join(referents, ","))
	}
	return b.String()
}

// bindCandidate presents one library document as a job the inbound bytes might
// belong to. Bound carries the document's own metadata DOI because that is the
// library's analogue of a job's durably bound DOI set, and it is what the
// conclusive-identity veto compares against — a candidate built with an empty
// Bound would make every DOI-printing document read as naming a foreign work.
func bindCandidate(doc Document) pdf.BindCandidate {
	c := pdf.BindCandidate{Key: doc.Key, Work: doc.Work}
	if doc.Work.DOI != "" {
		c.Bound = []string{doc.Work.DOI}
	}
	return c
}

func bucketize(counts map[string]int, order []string) []OffsetBucket {
	out := make([]OffsetBucket, 0, len(counts))
	seen := make(map[string]bool, len(order))
	for _, label := range order {
		seen[label] = true
		if counts[label] > 0 {
			out = append(out, OffsetBucket{Label: label, Count: counts[label]})
		}
	}
	extra := make([]string, 0, len(counts))
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

func sizeBuckets(counts map[int]int, sizes []int) []OffsetBucket {
	out := make([]OffsetBucket, 0, len(sizes))
	for _, size := range sizes {
		out = append(out, OffsetBucket{Label: fmt.Sprintf("N=%d", size), Count: counts[size]})
	}
	return out
}

// Render renders the composite arm's section as aligned plain text, in the
// same voice as Report.Render: counts first, the number the arm exists to
// bound second, and every unbounded quantity named as unbounded rather than
// omitted.
func (s CompositeSummary) Render() string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)

	fmt.Fprintf(w, "composite arm — documents that are ABOUT another work (errata, corrigenda, retraction notices, comments and replies, supplements, cover sheets, journal expansions)\n")
	fmt.Fprintf(w, "seed\t%d\n", s.Seed)
	fmt.Fprintf(w, "documents scored\t%d\n", s.Documents)
	fmt.Fprintf(w, "DOI-less documents (the corpus candidate selection can ever see)\t%d\n", s.DOILessDocuments)
	w.Flush()

	fmt.Fprintf(w, "\nlabels — proposals are evidence, labels are ground truth\n")
	fmt.Fprintf(w, "proposed by signal\t%d\n", s.Proposed)
	fmt.Fprintf(w, "reviewed\t%d\n", s.Reviewed)
	fmt.Fprintf(w, "confirmed composite\t%d\n", s.Confirmed)
	fmt.Fprintf(w, "reviewed and rejected\t%d\n", s.Rejected)
	fmt.Fprintf(w, "UNLABELLED (proposed, never reviewed — counted as NEITHER class)\t%d\n", s.Unlabelled)
	if s.ConfirmedMissing > 0 {
		fmt.Fprintf(w, "confirmed labels for documents this run did not load\t%d\n", s.ConfirmedMissing)
	}
	fmt.Fprintf(w, "confirmed composites printing a conclusive front-matter DOI (real composites the selector never sees; no pool)\t%d\n", s.ConfirmedWithFrontMatterDOI)
	w.Flush()

	fmt.Fprintf(w, "\nconfirmed by class\n")
	for _, bucket := range s.ConfirmedByClass {
		fmt.Fprintf(w, "%s\t%d\n", bucket.Label, bucket.Count)
	}
	w.Flush()

	fmt.Fprintf(w, "\nprevalence — an interval, never a number: proposer recall bounds it\n")
	fmt.Fprintf(w, "LOWER BOUND (confirmed / documents scored)\t>= %.2f%%\n", 100*s.PrevalenceLowerBound)
	if s.PrevalenceBounded {
		fmt.Fprintf(w, "upper bound (unlabelled proposals plus the audit-bounded share of unflagged documents)\t<= %.2f%%\n", 100*s.PrevalenceUpperBound)
	} else {
		fmt.Fprintf(w, "upper bound\tUNAVAILABLE — no audit row has been reviewed, so nothing bounds what the proposer missed; \"composites are rare\" is unfalsifiable from this run\n")
	}
	w.Flush()

	fmt.Fprintf(w, "\nrecall audit — a random sample of documents the proposer did NOT flag\n")
	fmt.Fprintf(w, "sampled\t%d\n", s.AuditRows)
	fmt.Fprintf(w, "reviewed\t%d\n", s.AuditReviewed)
	fmt.Fprintf(w, "composites the proposer missed\t%d\n", s.AuditComposites)
	if s.AuditReviewed > 0 {
		fmt.Fprintf(w, "miss rate among unflagged documents\t%.2f%% observed, <= %.2f%% (exact one-sided 95%%, n=%d)\n",
			100*s.AuditMissRate, 100*s.AuditMissUpper, s.AuditReviewed)
	}
	w.Flush()

	fmt.Fprintf(w, "\nsignals that proposed (a document may carry several)\n")
	if len(s.SignalCounts) == 0 {
		fmt.Fprintf(w, "none — no signal fired on any document\n")
	}
	for _, bucket := range s.SignalCounts {
		fmt.Fprintf(w, "%s\t%d\n", bucket.Label, bucket.Count)
	}
	fmt.Fprintf(w, "foreign identifiers found past page one (NOT proposed; that is where reference lists live)\t%d\n", s.ForeignBeyondPageOne)
	fmt.Fprintf(w, "documents whose markers could not be observed (front matter names >1 conclusive DOI, so the veto stops the probe)\t%d\n", s.MarkerProbeBlocked)
	w.Flush()

	fmt.Fprintf(w, "\nsignals this data does not support — recall limits, stated rather than left to look like absence\n")
	for _, note := range s.SignalsUnavailable {
		fmt.Fprintf(w, "- %s\n", note)
	}
	w.Flush()

	fmt.Fprintf(w, "\npools — every one target-absent by construction: the correct decision is to bind NOTHING\n")
	fmt.Fprintf(w, "pools built\t%d\n", s.Pools)
	fmt.Fprintf(w, "pools including the referred-to work as a candidate (the adversarial case)\t%d\n", s.PoolsWithReferent)
	for i, bucket := range s.PoolsBySize {
		unbuildable := 0
		if i < len(s.UnbuildableBySize) {
			unbuildable = s.UnbuildableBySize[i].Count
		}
		fmt.Fprintf(w, "%s\tbuilt %d\tunbuildable for want of candidates %d\n", bucket.Label, bucket.Count, unbuildable)
	}
	if s.Confirmed == 0 {
		fmt.Fprintf(w, "no confirmed composite: this arm measures NOTHING until a human reviews the proposal file\n")
	}
	w.Flush()

	return b.String()
}
