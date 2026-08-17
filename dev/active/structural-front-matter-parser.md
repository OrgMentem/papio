Status: design v1 (2026-08-17). Workstream 3 of the acquisition roadmap.
Ungated by the first real-library candidate-set run (`dev/active/candidate-binding-measurement.md`,
§First run, 2026-08-16). Read-only design; no implementation in this document.

## Why this exists

The measurement run settled a verdict the synthetic per-axis corpus could not reach:
**286 of 286** conjunction-arm, target-absent pools produced **wrong binds**, flat at N=2 and
N=25 (`dev/active/candidate-binding-measurement.md:344-359`). Every wrong bind passed all seven
gates and terminated at `identifier-page-one` with evidence that the document “prints the
requested DOI” — where that DOI is the one the document **cites** (`bridge.go:7578-7591`,
`candidate_select.go:359-386`). Pool size is not the risk axis; **composition** is
(`candidate-binding-measurement.md:361-369`).

Independently, the pairwise instrument’s two surviving wrong accepts need **structural
position**, not delimiter shape (`dev/identity-corpus.md:271-281`). Two instruments naming one
missing capability is the strongest evidence this design targets the right defect
(`candidate-binding-measurement.md:316-328`).

## Problem statement

Today’s rules answer: “Does this **phrase** appear as a delimited line?” and “Does this **token**
appear in this **byte window**?” They do not answer: “Is this document **asserting** that it
is work X, or **mentioning** work X?”

That distinction is load-bearing because:

- **Blind grab admission** — production reaches `SelectAutoBindCandidate` only when
  `FrontMatterDOIs` over the **1 KiB** blind window is empty (`bridge.go:7573-7610`,
  `identity.go:834-836`, `identity.go:805-807`). The adversary’s **own** DOI sits past that
  window; the **cited** DOI sits inside page one (`candidates.go:847-853`,
  `candidates.go:870-900`).
- **Gate 5** — `corroboratingIdentifier(identityPageOne(excerpt), candidate.Work)` treats any
  page-one occurrence of the candidate’s DOI as self-corroboration (`candidate_select.go:378-386`,
  `identity.go:1008-1010`). `MatchIdentity` can corroborate over the **whole excerpt** up to
  `MaxExcerpt` (`identity.go:287-288`, `semantic.go:36`, `semantic.go:188-192`).
- **Title gate** — `candidateTitlePrintedAsLine` / `titlePrintedAsLine` accept a delimited
  match in a **contents list** or **section heading** (`dev/identity-corpus.md:271-281`).

The structural front-matter parser is the capability that assigns **roles** to spans inside
the executable page-one window so gates can ask self-assertion questions instead of substring
questions.

## Executable window (what the parser reads)

All candidate-binding gates and blind `FrontMatterDOIs` ultimately read text that has already
been:

1. Extracted by `pdftotext` (or OCR fallback when below `MinChars`) (`semantic.go:82-127`).
2. Truncated to `MaxExcerpt` (**16 KiB**) for `report.Text.Excerpt` (`semantic.go:188-192`).
3. Cut to **page one** at the first form feed, with leading blank-leaf trimming, then capped
   (`identity.go:810-832`, `identity.go:867-869`).

Three nested windows inside that excerpt matter today:

| Window | Bytes | Used for |
|--------|-------|----------|
| Front matter (blind) | 1 KiB (`identityFrontMatterBytes`) | `FrontMatterDOIs`, `CheckConclusiveIdentity` (`identity.go:834-836`, `candidate_binding.go:55-64`) |
| Byline | 2 KiB (`identityBylineBytes`) | Author, title, year, marker gates (`identity.go:854-856`, `candidate_select.go:147-148`) |
| Page one | 4 KiB (`identityPageOneBytes`) | Identifier gate (`identity.go:867-869`, `candidate_select.go:379`) |

The parser’s **primary input** should be `identityPageOne(excerpt)` — the same executable
“page one” the identifier gate already uses (`identity.go:858-869`). Pairwise
`MatchIdentity` title evidence uses the 2 KiB byline (`identity.go:108`, `identity.go:227`);
corroboration can use whole excerpt to 16 KiB (`identity.go:287`). The parser should expose
role queries on page one first; byline-only consumers can query a sub-view.

**Extraction divergence (measurement vs production):** corpus loader uses
`DefaultSemanticOptions()` (`MinChars` **1000**, `OCRPages` **3**) (`semantic.go:30-40`);
daemon defaults `min_text_chars` **400**, `max_ocr_pages` **4**, OCR enabled
(`config/config.go:592`, `bootstrap/bootstrap.go:330`). Both bound excerpt at 16 KiB
(`candidate-binding-measurement.md:297-300`). Any parser increment must be graded with that
divergence noted in the report, not assumed identical OCR paths.

## Structural roles (vocabulary)

Roles are assigned to **line spans** (contiguous line ranges) or **segments** (within-line
runs from existing `wideGapSegments` / `bylineSegments` — `identity.go:548-577`,
`identity.go:355-375`). They are hypotheses over plain text, not PDF geometry.

### Positive roles (self-assertion band)

| Role | Meaning | Typical cues in papio’s text layer |
|------|---------|-------------------------------------|
| `title_block` | Document’s own title line(s) | Early page-one position; largest standalone line block before author band; matches publisher title-wrap segments (`identity.go:426-454`) |
| `author_byline` | Author / affiliation band | Between title block and abstract keywords; tokens used by `candidateAuthorTokenSet` (`candidate_select.go:473-527`) |
| `own_identifier` | Document asserts **its** identifier | DOI/arXiv/PMID with publisher label (`doi:`, `DOI:`, `https://doi.org/`, `Article DOI:`) in masthead/footer band **outside** citation introducers; may use `documentDOIs` conclusive set in blind 1 KiB when complete (`identity.go:717-761`) |
| `abstract_start` | Start of abstract body | Line matching `abstract` / `summary` heading; upper bound for title/byline scoping (already approximated in `candidateAuthorTokenSet` via keywords — `candidate_select.go:473-527`) |

### Negative roles (defeat “printed as title” / “prints DOI”)

| Role | Meaning | Why it matters |
|------|---------|----------------|
| `contents_list_entry` | Line inside a table of contents / outline | Pairwise wrong accept #1 (`dev/identity-corpus.md:273-274`) |
| `section_heading` | Numbered or unnumbered section title | Pairwise wrong accept #2 (`dev/identity-corpus.md:274-275`) |
| `reference_citation` | Bibliography / reference-list line | DOI in citation shape; must not corroborate candidate (`identity.go:990-995`) |
| `citation_context` | In-body mention of another work | “Extended from …”, “cited in …”, parenthetical citation lines — conjunction adversary (`candidates.go:895`, `bridge.go:7583-7584`) |
| `extended_from_marker` | Provenance / expansion line | Journal expansion printing target metadata + citing target DOI (`candidates.go:870-877`) |
| `correction_about_other` | Document is *about* another work | `correctionMarkers` / `nonArticleMarkers` families (`identity.go:22-44`, `candidate_select.go:179-190`) — **0 trials** in first measurement run (`candidate-binding-measurement.md:390-392`) |

### Document-kind (orthogonal to span roles)

| Kind | Effect on gates |
|------|-----------------|
| `primary_article` | Default |
| `non_article` | Hard disqualify (supplement, data sheet, etc.) — gate `non-article-marker` (`candidate_select.go:72-73`, `180-184`) |
| `correction_or_comment` | Abstain for 1-of-N — gate `correction-marker` (`candidate_select.go:73-74`, `186-190`) |
| `unknown` | Parser could not classify; gates must fail closed |

Roles are **not** mutually exclusive at line level (a line can be both `section_heading` and
contain a DOI); the parser returns the **strongest applicable negative role** for a candidate
match span when adjudicating gates.

## Self-asserted DOI vs cited DOI — concrete signals

| Signal | Self-assertion evidence | Mention evidence | Papio today | Needs new extraction |
|--------|-------------------------|------------------|-------------|----------------------|
| **Window position** | Blind 1 KiB (`identityFrontMatter`) for *blind naming* only (`identity.go:805-807`) | Cited DOI in page-one body past 1 KiB but inside 4 KiB (`candidates.go:847-853`) | **Yes** — byte offset via `identityPageOne` vs `identityFrontMatter` | **Yes** — role “in blind band vs body band” is not a gate input today |
| **Label proximity** | `DOI:`, `doi.org/`, `Article DOI:` on same line/segment (`conjunctionDocument` uses `Article DOI:` for own — `candidates.go:897`) | DOI after “Extended from”, “Available at”, “Erratum to” (`identity.go:46-57`, `candidates.go:895`) | **Partial** — regex finds DOI; introducer phrases are not classified | **Yes** — introducer + DOI pairing |
| **Relative to byline / abstract** | Footer / masthead below abstract keywords | Mid-page provenance paragraph before abstract (`candidates.go:891-898`) | **Partial** — `abstract` keyword scoping exists for authors (`candidate_select.go:473-527`) | **Yes** — explicit region graph |
| **Inside references region** | Rare on page one | Citation lines | **No** — whole-excerpt search unsafe for 1-of-N (`candidate_select.go:367-370`) | **Yes** — references band detector |
| **Citation shape** | Standalone labeled identifier line | DOI embedded in sentence with citation verbs | **No** | **Yes** |
| **Duplicate DOI roles** | One DOI labeled own, another cited | Conjunction: target DOI cited, `10.5555/...` own (`candidates.go:897`) | **Partial** — `documentDOIs` lists all; no role (`identity.go:717-761`) | **Yes** — per-occurrence classification |
| **Slash-run verbatim** | `normalizeDOI` preserves suffix (`identity.go:639-658`) | Same | **Yes** | No |
| **Line-break reconstruction** | `documentDOIs` matchable vs conclusive (`identity.go:702-708`) | Incomplete DOI must not blind-name | **Yes** | Parser should attach roles only to **conclusive** occurrences for blind naming |

**Decision rule (gate input, not a second binding rule):** a candidate’s DOI corroborates only
if at least one page-one occurrence is classified `own_identifier` **or** appears in
`title_block`/`author_byline` with a publisher label and **no** overriding negative role on
that span. An occurrence in `citation_context`, `contents_list_entry`, `section_heading`, or
`reference_citation` **cannot** satisfy gate 5, even if `containsFlattenedToken` would match
(`identity.go:897-933`).

For **blind** `FrontMatterDOIs`, the parser should only promote DOIs to “conclusive blind
identity” when they appear in `own_identifier` within the 1 KiB front-matter slice — **not**
when they appear only as cited mentions in that slice. That is a coordinated ADR-0020 /
three-artifact change if production blind naming changes; gate-5-only classification is the
first increment (see Phasing).

## Uncertainty — outcome vocabulary

The parser must **fail closed**: “no confident self-assertion” routes to popup picker, inbox,
and `MarkParkedNoIdentifier` — already correct shipped behaviour (`bridge.go:7609-7610`).

### Parser-level outcomes (per document, not per candidate)

| Outcome | Meaning | Downstream |
|---------|---------|------------|
| `parsed_ok` | Region graph built with stated confidence | Gates query roles |
| `sparse_text` | Below useful structure (OCR noise, shattered columns) | All self-assertion queries → unknown; identifier gate → `Review` / abstain |
| `no_confident_self_assertion` | No identifier span classified `own_identifier` | Gate 5 → `Review` (`candidate_select.go:380-383`); blind path unchanged if 1 KiB empty |
| `ambiguous_own_identifier` | Multiple competing labeled DOIs in assertion band | Veto-compatible with `CheckConclusiveIdentity` multi-DOI (`candidate_binding.go:55-64`) |
| `structure_unknown` | Could not separate TOC/headings from title block | Title/identifier gates treat candidate matches in unknown regions as **not** self-assertion |

### Per-span classification (for measurement traces)

| Label | Use |
|-------|-----|
| `self_asserted` | Span may support gate 5 or blind naming |
| `mention` | Span may support metadata similarity but not corroboration |
| `excluded` | Negative role active (TOC, heading, citation) |
| `unclassified` | Do not use for accept paths |

Gate traces should record **parser role at match site**, not replace `CandidateGate` constants
(`candidate_select.go:71-78`) — extend `Evidence` strings and measurement export, not the gate
enumeration, until a rule version bump warrants new gate IDs.

## How this changes the seven gates

Current order (`candidate_select.go:66-78`, `126-391`):

1. `conclusive-veto` — blind 1 KiB DOIs (`candidate_select.go:139-144`)
2. `non-article-marker` (`candidate_select.go:180-184`)
3. `correction-marker` (`candidate_select.go:186-190`)
4. `author-evidence` (`candidate_select.go:236-263`)
5. `title-printed-as-line` (`candidate_select.go:301-306`)
6. `year-token` (`candidate_select.go:338-357`)
7. `identifier-page-one` (`candidate_select.go:378-386`)

### Rejected: replace `identifier-page-one`

Gate 5 is also the **only** gate that produces `Review` instead of hard abstain when metadata
agrees but identifier is missing (`candidate_select.go:122-124`, `380-383`). Replacing it
with a monolithic “parser gate” would collapse suggestive ranking into a single pass/fail
or duplicate Review semantics. The measurement contract explicitly needs terminal gate +
disposition (`candidate_select.go:96-107`).

### Rejected: add an eighth gate before everything

A standalone `structural-parser` gate that runs before author/title would force full parsing
even when `conclusive-veto` or marker gates would abort early (`candidate_select.go:139-190`).
It also splits “title printed as line” from “title in TOC” across two gates, duplicating
segmentation work (`bylineSegments` vs parser). Gate count in the measurement plan is seven
(`candidate-binding-measurement.md:76-78`); adding an eighth is a **rule version** and
instrument migration without moving the conjunction arm until gate 5 still accepts citations.

### **Chosen: feed existing gates (parser as shared substrate)**

Introduce `ParseFrontMatter(pageOne string) FrontMatterParse` (name TBD) in `internal/pdf`,
computed **once per excerpt** per `QualifyCandidate` call (or once per `SelectAutoBindCandidate`
batch). Gates consult it:

| Gate | Parser feed |
|------|-------------|
| `non-article-marker` / `correction-marker` | `document_kind` + span roles (erratum **heading** vs pointer footnote — `identity.go:614-620`) |
| `title-printed-as-line` | Match must lie in `title_block`; reject if match span has `contents_list_entry` or `section_heading` |
| `identifier-page-one` | `corroboratingIdentifier` only after filter: candidate DOI must match in `own_identifier` or labeled self band, not `mention` roles |
| `conclusive-veto` | **Phase 2+** optional: blind 1 KiB `own_identifier` only — coordinated with `FrontMatterDOIs` (`identity.go:805-807`) |

`MatchIdentity` / `titlePrintedAsLine` / pairwise corroboration consume the same parser for
the two wrong accepts (`dev/identity-corpus.md:283-287`) without changing the 60% token gate
(`identity.go:179-203`).

## Layout / position data — feasibility (verified in source)

**The text pipeline does not preserve x/y layout or font metadata.**

- `ExtractText` runs `pdftotext` to stdout string (`semantic.go:90-94`) or OCR text
  (`semantic.go:115-121`).
- `textReport` stores a **byte-truncated** excerpt (`semantic.go:188-192`); no coordinates.
- Page boundaries appear only as **form feed** `\f` in the string (`identity.go:825-826`).
- Positional logic today is **line order**, **wide-gap splits**, and **byte windows**
  (`identity.go:548-577`, `identity.go:810-832`) — not boxes.

Therefore the parser **must** work from **plain text + line structure + segment boundaries**
only. Feasibility is **confirmed**; dependence on PDF layout APIs is **out of scope** unless
extraction is extended (new rule version, new measurement baseline).

Implications:

- TOC detection: numbered lines + indent/stack heuristics, “Contents” heading, not column x.
- Section headings: short lines, numbering prefixes, following blank line — not font size.
- Running heads: repeated identical early lines (already used in `candidateTitlePrintedAsLine`
  — `candidate_select.go:536-537`).
- Column interleaving failures remain **parser-opaque** (`dev/identity-corpus.md:261-262`).

## Measurement

Both instruments must grade each increment (`dev/identity-corpus.md:283-287`,
`candidate-binding-measurement.md:316-328`):

| Instrument | Command | Primary estimands |
|------------|---------|-------------------|
| Pairwise | `go run ./cmd/identity-corpus` | Wrong accepts (mismatched pairs `pass`); then own-metadata pass rate (`dev/identity-corpus.md:166-173`) |
| Candidate-set | `go run ./cmd/identity-corpus -candidates` | **Wrong binds first**; then `missed-bind` on target-present unique pools (`dev/identity-corpus.md:317-322`, `candidate-binding-measurement.md:117-124`) |

### Pass criteria (fixed before collecting after each increment)

1. **Wrong accepts / wrong binds:** count must **not increase** vs baseline on the **same**
   library and cache (`dev/identity-corpus.md:296-297`).
2. **Conjunction arm, target-absent:** wrong binds must drop from **286/286** — the arm that
   matters (`candidate-binding-measurement.md:348-359`). Target-present conjunction should
   move from ambiguous multi-qualify (`candidate-binding-measurement.md:371-375`) toward
   correct abstain or correct bind, measured separately.
3. **Pairwise wrong accepts:** target **2 → 0** on reference library (`dev/identity-corpus.md:271-281`).
4. **Correct passes / correct binds:** compared **only after** safety is flat or better
   (`dev/identity-corpus.md:298-299`). Acceptable missed-bind budget: predeclared per
   increment; today ~**85%** missed on random N=2 target-present (`candidate-binding-measurement.md:379-381`) — improvements are welcome but not required for safety increments.
5. **Marker gates:** synthetic + real arms must produce **non-zero** trials for
   `non-article-marker` and `correction-marker` (`candidate-binding-measurement.md:390-392`);
   label 14 composite proposals + audit (`candidate-binding-measurement.md:396-398`).
6. **Denominators:** per-document cluster bounds for safety; per-arm/N operational rates;
   never one headline rate (`candidate-binding-measurement.md:232-247`).
7. **Gate traces:** terminal gate + parser role at match site for conjunction failures.

Report extraction divergence when corpus MinChars/OCR ≠ daemon (`identitycorpus/backlog.go:471-472`).

## Phasing — one increment at a time

Discipline: one measured increment; revert if wrong accepts/bindings rise; ship only what moves
numbers (`dev/identity-corpus.md:289-301`).

### Increment A (smallest move off 100% conjunction wrong-binds)

**Citation-aware identifier gate only** — no TOC/heading yet.

- Parse page one into coarse bands: `masthead/byline`, `body_before_abstract`, `abstract+`.
- Classify each DOI occurrence: `own_identifier` if labeled `Article DOI:` / `DOI:` on its line
  without citation introducer prefix; `mention` if on line matching introducers used in
  conjunction synthesis (`Extended from`, etc. — `candidates.go:895`) or reference shape.
- Change gate 5 only: call existing `corroboratingIdentifier` but **ignore** matches whose
  span role is `mention` (`candidate_select.go:378-386`).

**Expected measurement:** conjunction target-absent wrong binds **0**; marker gates still 0
trials; pairwise wrong accepts still **2** until Increment B.

### Increment B (pairwise wrong accepts + title gate)

- Detect `contents_list_entry` and `section_heading` regions (heuristic TOC/heading bands).
- Feed `title-printed-as-line` and `candidateTitlePrintedAsLine` (`candidate_select.go:301-306`,
  `identity.go:426-454`).

**Expected measurement:** pairwise wrong accepts **0**; conjunction still clean.

### Increment C (marker gates + composites)

- Feed `document_kind` for `correction-marker` / `non-article-marker` using parsed erratum
  headings vs `correctionPointerPhrases` exclusion (`identity.go:46-57`, `identity.go:614-620`,
  `candidate_select.go:186-190`).
- Real composite labels + audit (`candidate-binding-measurement.md:396-398`).

### Increment D (optional, coordinated product change)

- Blind `FrontMatterDOIs` uses parser `own_identifier` in 1 KiB only — affects grab path
  before candidate selection (`bridge.go:7573`, ADR-0020). **Not** required to fix conjunction
  arm; separate gate and protocol review.

Do **not** bundle A+B+C in one PR.

## What this parser does **not** fix

| Gap | Why |
|-----|-----|
| **Pool cap as safety lever** | Refuted: 100% wrong binds at all N (`candidate-binding-measurement.md:361-367`) |
| **Majority auto-bind coverage** | ~85% missed binds when binding is “safe” (`candidate-binding-measurement.md:379-381`) |
| **`conclusive-veto` at 0 trials** | Unreachable in DOI-less corpus by design (`candidate-binding-measurement.md:388-389`) |
| **Unfilled arms** | `same-venue-year`, `title-superset` 0 eligible (`candidate-binding-measurement.md:393-395`) |
| **Foreign arXiv/PMID blind veto** | DOI-only blind class (`identity.go:771-800`, `candidate_binding_test.go:216-220`) |
| **Integration safety** | Eligibility pool, bind fence, ownership, concurrency (`candidate-binding-measurement.md:277-282`) |
| **Slash-run foreign/same ambiguity** | `CheckConclusiveIdentity` parks (`identity.go:673-679`) |
| **Books / output cap** | Corpus bias (`dev/identity-corpus.md:79-90`) |
| **Column-shredded text layers** | No layout (`dev/identity-corpus.md:261-262`) |
| **Whole-document bibliography** | Excerpt ends at 16 KiB; page two+ invisible to page-one rules (`candidate-binding-measurement.md:304-310`) |
| **Operator backlog pool at settlement time** | Backlog arm is descriptive, not calibration (`candidate-binding-measurement.md:194-211`) |

## Value case vs popup picker (do not overclaim)

Even a **safe** `/2` rule is a **modest minority** win: the measurement quantified cost as
~**85%** missed binds on random N=2 target-present vs 44 correct (`candidate-binding-measurement.md:379-381`). The win case is not “most PDFs file themselves” but **some**
DOI-less grabs bind without a human when composition is benign — weighed against the **popup
picker and inbox already shipped** (`bridge.go:7575-7610`, `candidate-binding-measurement.md:382-384`).

Autonomous binding stays behind `autoBindDecisionEnabled` until integration gates pass
(`bridge.go:7627-7634`, `candidate-binding-measurement.md:286-294`). The parser makes a
**defensible** predicate possible; it does not by itself justify turning the decision on.

## Contrary evidence and strongest risks

| Risk | Source |
|------|--------|
| Heuristic TOC/heading detectors false-negative real titles (more missed binds) | Trade-off explicit in printed-title rule (`dev/identity-corpus.md:257-269`) |
| Heuristic false-positive “mention” on real footer DOIs (new missed binds, not wrong binds) | 17/40 papers print own DOI below abstract (`identity.go:993-994`) — label + region rules must stay conservative |
| Introducer phrase list incomplete | Conjunction uses one shape (`Extended from` — `candidates.go:895`); real publishers vary |
| Marker gates need **labeled composites** | 0 trials without human labels (`candidate-binding-measurement.md:390-392`) |
| Parser on OCR text differs from pdftotext | MinChars 400 vs 1000 (`semantic.go:34`, `config/config.go:592`) |

## Implementation boundaries (for later workstreams)

- Parser and gate feeds live in `internal/pdf` beside existing windows (`identity.go:871-878`).
- Do not consolidate `internal/work` (version-preserving) with `internal/ownership`
  (version-collapsing) (`candidate-binding-measurement.md:142-146`).
- Wire/protocol changes only if blind `FrontMatterDOIs` semantics change (Increment D).
- `CandidateBindingRule` bumps to `/3` when acceptance set changes (`candidate_select.go:39-47`).

## References (load-bearing)

- `identityPageOne` / windows: `identity.go:810-869`
- `FrontMatterDOIs` 1 KiB blind: `identity.go:805-807`, `identity.go:834-836`
- `corroboratingIdentifier` whole excerpt vs page one: `identity.go:287-288`, `identity.go:1008-1010`, `candidate_select.go:378-380`
- Gates: `candidate_select.go:71-78`, `candidate_select.go:126-391`
- Grab admission: `bridge.go:7573-7610`
- Text excerpt: `semantic.go:30-40`, `semantic.go:188-192`
- Measurement verdict: `candidate-binding-measurement.md:330-405`
- Pairwise wrong accepts: `dev/identity-corpus.md:271-287`
