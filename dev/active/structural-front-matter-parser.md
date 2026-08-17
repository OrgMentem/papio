# Identity attribution for DOI-less binding (formerly "structural front-matter parser")

Status: **design v2 (2026-08-17)** — synthesised from three independent reviews of v1.
v1 was written by a single research unit and shipped unreviewed in `dd9c792`; it survived
none of the three. Review artifacts:

- Reviewer verdict **NEEDS REVISION** (7 findings) — `history://ParserDesignReview`
- Factual anchor audit — `history://ParserAnchorAudit`
- Independent competing design, GPT-5.6 Sol —
  `dev/scratch/oracle/20260817T131434Z-parser-independent-design/answer.md`

Every code claim below was re-verified against the tree during synthesis; anchors in v1 had
drifted and are corrected here. **Nothing in this document is built.** It supersedes v1's
design; v1's substrate survives where marked.

## What changed from v1, and why

v1 proposed a general front-matter parser with a vocabulary of positive and **negative** span
roles (`contents_list_entry`, `citation_context`, …), and asked gates to reject a match whose
span carried a negative role. Three independent objections converged on the same structural
flaw and it is fatal to that polarity:

> **v1 made safety depend on successfully detecting a mention.** A mention detector that misses
> leaves the wrong accept exactly where it is. That makes every detector's recall a
> library-integrity parameter, and v1 supplied no bound on any of them
> (reviewer finding 4; v1 `:247-262` promised a "predeclared" budget and never declared one).

v2 inverts the polarity. Attribution is **ternary** and only the positive class may authorise
an autonomous bind:

| Role | Meaning | May authorise a bind |
|---|---|---|
| `SELF` | the document asserts this span as its own identity | **yes** |
| `MENTION` | the span refers to another work | no |
| `UNKNOWN` | plain text does not justify either conclusion | no |

Consequence, and the whole point: **failing to recognise a mention yields `UNKNOWN`, not
`SELF`.** It costs a missed bind, never a wrong accept. The only dangerous error left is a
**false `SELF`**, and that is bounded *by construction* rather than by a threshold — see
§Bounding.

The second change is scope. v1 was named for a parser and sized like one. The capability
actually needed is narrower: **attribution over already-extracted text**, not document
understanding. Renamed accordingly.

The third change is sequencing: v2 puts a **cheap non-parser veto first**, with a predeclared
stop rule that can end this workstream before any parser exists (§Increment 1, §Viability).

## Corrections to v1's factual claims

These are errors, not drift. v1's argument rested on the first two.

1. **The 286/286 conjunction arm is a *synthetic regression*, not a real-library measurement.**
   `conjunctionDocument` (`internal/identitycorpus/candidates.go:893-913`) *constructs* each
   excerpt: it writes the target's title, authors and year as front matter, then a hard-coded
   `Extended from <cited identifier>` line, then `Article DOI: <different own DOI>`.
   The count is therefore trials of a hand-authored shape, and says **nothing about incidence
   in the real grab stream**.
   It is still valuable: its own comment (`:934-936`) calls the target-absent arm "the
   reproduction of the withdrawn failure", so it models a **real observed defect**. Keep it as
   a safety regression. Do not cite it as frequency evidence, and do not claim it converges
   *independently* with the pairwise instrument — the pairwise wrong accepts are real PDFs
   failing by a different mechanism (title *position*, `dev/identity-corpus.md:271-281`).
   v1's "two instruments naming one capability is the strongest evidence" is withdrawn.

2. **v1's Increment A was defeatable, and by one newline.** `isIdentitySpace`
   (`internal/pdf/identity.go:975-981`) includes `'\n'`, and `containsFlattenedToken`
   (`:902-933`) skips identity-space *mid-token* while matching. So `Extended from\nDOI:
   10.x/target` puts the introducer on one line and the label on the DOI's line: a
   line-scoped rule reads `own_identifier`, the flattened matcher still finds the target, and
   the wrong bind survives. A wrapped DOI does the same.
   **Requirement this creates:** attribution must be defined over the **flattened match
   extent** — from the match's start byte to its end, spanning the newlines the matcher
   skipped — plus multi-line context. `containsFlattenedToken` returns `bool` and exposes no
   offsets, so an **offset-bearing match contract** is a prerequisite, not a detail. Coarse
   region bands cannot satisfy this alone.

3. **v1's Increment A covered only DOIs.** `corroboratingIdentifier` accepts arXiv and PMID
   too, and the conjunction generator deliberately falls back to an `arXiv:` citation for a
   Work with no DOI (`candidates.go:922-929`). An arXiv-only target-absent conjunction would
   have passed v1's Increment A untouched. Attribution is defined over **every target-aware
   identifier class**, and any unclassified match **fails closed**.

4. **"Candidate selection has no notion of position" was too strong.** `candidate_select.go`
   already scopes authors relative to a found title and requires a candidate title match to
   precede the abstract, sit among the first few segments, and not recur elsewhere. That is
   **ordinal** position, not semantic role. The real defect is narrower and survives: the
   identifier gate runs `corroboratingIdentifier` over the 4 KiB page-one window, and that
   helper is a target-aware token search that cannot distinguish self from cited.

5. **Gate numbering was internally contradictory** — a spec defect, not a typo. v1 numbered
   `identifier-page-one` as **7** in its own list, then said a mention "cannot satisfy gate 5",
   that "gate 5" is the only gate yielding `Review`, and that Increment A should "change gate 5
   only" while citing `candidate_select.go:378-386`. Read literally, Increment A edits
   `title-printed-as-line`. v2 names gates by **constant**, never by ordinal.

6. **Corrected anchors** (v1's bridge references were all stale):
   `bridge.go:7573` → `7613`; `7573-7610` → `7613-7671`; `7578-7591` → `7613-7634`
   (`Extended from` commentary at `7622-7629`); `7609-7610` → `7657-7659` (DOI-less park) or
   `7670-7671` (mismatch park); `7627-7634` and `7618-7625` → `7637-7639` (call-site rationale)
   plus `7682-7689` (flag declared false). Also: `identity.go:810-869` is partial
   (`IdentityWindows` at `876-877`); the TOC evidence is at `dev/identity-corpus.md:271-281`,
   not `257-269`; `Extended from` is at `candidates.go:908-910`, not `:895`.

Verified unchanged from v1 and still load-bearing: `identityFrontMatterBytes` 1 KiB,
`identityBylineBytes` 2 KiB, `identityPageOneBytes` 4 KiB; `identityWindow` trims leading
whitespace/form-feed/BOM, cuts at the first form feed, then byte-caps; `MinChars` 1000 /
`OCRPages` 3 / excerpt 16 KiB in `semantic.go` vs daemon 400 / 4 / OCR enabled;
`CandidateBindingRule` = `/2` (`candidate_select.go:47`); the seven `CandidateGate` constants
in order at `:71-78`; `FrontMatterDOIs` exported while every other identity helper
(`identityPageOne`, `bylineSegments`, `wideGapSegments`, `corroboratingIdentifier`,
`titlePrintedAsLine`) is **unexported** — so attribution must live **inside `internal/pdf`**.

## New defect found during review (independent of this workstream)

**`identityPageOne` is not page one for any OCR'd document.** `extractOCR`
(`internal/pdf/semantic.go:254-273`) concatenates per-page Tesseract output with
`all.WriteString(text)` and **inserts no form feed** between pages. `identityWindow` derives
"page one" by cutting at the first form feed (`identity.go:825-826`); with none present, the
window becomes the first 4 KiB of the **whole document**, silently including pages 2-4 —
reference lists and other works' identifiers among them.

This is live in production today: the daemon defaults to OCR enabled with `max_ocr_pages` 4.
It is a pre-existing safety hole in the shipped identifier rule, wider than this workstream.
Two consequences:

- **No structure-derived `SELF` decision may run when `OCRUsed` is true**, until page
  boundaries are trustworthy (Increment 7).
- Fixing OCR page separators is **its own measured change** with its own baseline, and it
  should be filed independently of this plan rather than bundled into it.

## The capability

`internal/pdf/identity_structure.go` — beside the lexical normalisation code, not inside it.
Lexical identity answers *what* is printed; attribution answers *what role that span plays*;
candidate selection decides whether the attributed evidence is enough to act.

```go
type AssertionRole uint8   // AssertionUnknown | AssertionSelf | AssertionMention
type IdentityRegion uint8  // RegionUnknown | RegionIdentityFrame | RegionContents | RegionBody | RegionReferences

type IdentitySpan struct {
    Kind    SpanKind // title, DOI, arXiv, PMID
    Start   int      // flattened match extent, not the containing line
    End     int
    Segment int
    Region  IdentityRegion
    Role    AssertionRole
    Reason  string   // for measurement traces
}
```

`StructuralSegment` derives from the same line folding and wide-gap recovery `bylineSegments`
already uses — that machinery handles running heads and columns without treating single spaces
as boundaries — but **must retain byte offsets**. `titleSegment` currently discards them,
which is why requirement 2 above is a prerequisite.

**No numeric confidence score.** Recognition uses finite predicates: exact structural headings
(`abstract`, `keywords`, `contents`, `references`); numbered-heading syntax once the body has
started; the existing title delimiter machinery; **adjacency** (a `SELF` title must anchor a
plausible title→byline/affiliation sequence, not merely appear early); and **ambiguity** (two
positions that independently look like the document's own title yield `UNKNOWN`, not a pick).

`SELF` for an identifier requires positive structural evidence, initially only two forms:

1. it occurs in the parsed identity frame on a metadata-shaped identifier segment; or
2. it occupies a standalone page-one identifier line — essentially `DOI: <doi>` /
   `https://doi.org/<doi>` or the target-aware arXiv/PMID equivalent — with no surrounding prose.

Form 2 is what recovers part of the 17-of-40 population that prints its own DOI below the
abstract (`identity.go:993-994`) without declaring "anything on page one is self".

`MENTION` is assigned when the **region** establishes reference/contents/body provenance, or
the identifier sits in an explicitly referential construction. Deliberately **not** a large
introducer-phrase vocabulary: an incomplete mention lexicon must never create acceptance. It
is a diagnostic, not the safety boundary. The positive `SELF` grammar is the boundary.

**Delete, do not tune, the candidate-title `start <= 3` check.** It is an undocumented numeric
structural cutoff in wrong-accept-critical code — exactly the free knob this design forbids. A
masthead with four extracted segments before the real title and a section heading with two
before it must not be separated by the integer 3; adjacency and ambiguity replace it.

## Composition with the gates

**Feed the existing gates.** A generic "structure ran" gate in front of everything is
rejected: parser success is not evidence about a candidate, and running it before the
`conclusive-veto` and marker gates forces full parsing where those abort early.

One exception earns a new gate:

```
conclusive-veto → non-article-marker → correction-marker
  → self-identifier-conflict (NEW) → author-evidence → title-printed-as-line
  → year-token → identifier-page-one
```

`GateSelfIdentifierConflict` examines **every** positively `SELF`-attributed page-one
identifier, not just occurrences of this candidate's. A document that `SELF`-asserts an
identifier incompatible with the candidate **hard-abstains** that candidate.

It must be a hard abstain, not `Review`, and this is measured, not aesthetic:
`SelectAutoBindCandidate` returns `"ambiguous: qualifier alongside review"` whenever any
candidate reviews **alongside** a qualifying one (`candidate_select.go:703-704`). In the
target-present conjunction case the cited-work candidate must **hard-fail** so the real
candidate can still qualify; a `Review` there poisons it and reproduces today's all-abstain
result (`candidate-binding-measurement.md:371-375`).

Kept separate from `CheckConclusiveIdentity` on purpose: that veto's contract is the
conclusive DOI set from the blind 1 KiB window with slash-preserving comparison
(`candidate_binding.go:55-64`). Widening it would couple blind naming to targeted matching.

Per gate:

| Gate | Change |
|---|---|
| `author-evidence` | consume the byline attached to the unique `SELF`-title anchor; report "no structural byline" rather than falling back to a bag of pre-abstract tokens |
| `title-printed-as-line` | `structure.SelfTitle(...)` replaces `candidateTitlePrintedAsLine`; a title-shaped section heading or TOC entry is `MENTION`/`UNKNOWN`, never a pass. This is the piece later shared with `MatchIdentity` for the two pairwise survivors |
| `year-token` | read the year from the identity frame/byline block, not an independently sliced window; keep the existing posture where a year conflict independently disqualifies |
| `identifier-page-one` | **stays last and keeps its `Review` disposition.** Asks: is there a `SELF` occurrence of one of this candidate's target-aware identifiers? `MENTION` → `Review(identifier_only_mentioned_on_page_one)`; `UNKNOWN` → `Review(identifier_role_unknown_on_page_one)` |

Acceptance-set change ⇒ `CandidateBindingRule` bumps to `/3`.

## The blind 1 KiB path does not change

v1's Increment D is **dropped**. `FrontMatterDOIs` stays exactly as it is: DOI-only,
conclusive-only, targetless. Two independent reasons:

- Its contract is that reconstruction may *confirm* a known target but may never *manufacture*
  an identifier for a targetless capture. That is a different acceptance question, needing a
  targetless false-mint corpus and its own thresholds.
- Production performs blind naming **before** entering the DOI-less candidate branch
  (`bridge.go:7613-7615`). Changing blind reachability while measuring the candidate rule would
  move the population underneath the measurement.

A later `blind_identity/2` may revisit this. It is not this workstream.

## Increments and predeclared pass criteria

One increment at a time; each revertible on a safety regression; criteria fixed **before**
collecting.

| # | Change | Predeclared criterion |
|---|---|---|
| **0** | none — capture baselines, fixed seed/flags, and separately under daemon-equivalent `MinChars`/OCR | baselines captured; **no release conclusion** if the two extraction settings differ materially |
| **1** | **page-one multi-DOI ambiguity veto** — no parser at all: a candidate reaching corroboration cannot auto-bind if page one conclusively prints multiple distinct DOIs | conjunction target-absent **286/286 → 0**; every currently clean real cell stays at 0; target-present coverage **≥10%** of unique eligible documents or **stop** (§Viability) |
| **2** | attribution computed and traced, **decisions unchanged** | selector outcomes **identical** to Increment 1; both known pairwise false-title spans classify non-`SELF`; each conjunction's cited identifier classifies non-`SELF` **or** its own DOI classifies `SELF`; report what fraction of the 17-of-40 late own-DOI cases classify `SELF` |
| **3** | `GateSelfIdentifierConflict` replaces the coarse veto | 0 conjunction target-absent wrong binds; 0 new wrong-bind documents anywhere; must **recover ≥1 correct bind** lost by Increment 1, or the added complexity is deleted |
| **4** | `SELF` title/byline feed `title-printed-as-line` / `author-evidence`; same attribution applied experimentally to pairwise `MatchIdentity` | pairwise wrong accepts **2 → 0**; no new candidate wrong binds; pairwise own-document pass falls by **≤1 percentage point** on the same corpus, else revert to candidate-only |
| **5** | identifier gate accepts **only** `SELF` | 0 wrong binds in every target-absent cell and every named adversarial construction; coverage **≥10%** of unique target-present eligible documents |
| **6** | one attested `SELF` recovery form per increment (e.g. a specific late footer-DOI grammar) | each must recover **≥1 unique correct bind**, add **0** wrong binds and **0** pairwise wrong accepts; a no-op is deleted |
| **7** | OCR page boundaries preserved, structure enabled on OCR, then grab admission / pool construction / bind fence | same zero-wrong criteria under **production** extraction; no enabling flag until integration passes |

Additional gates on the whole sequence, from review:

- **Marker gates** (`non-article-marker`, `correction-marker`) recorded **0 trials** in the
  first run (`candidate-binding-measurement.md:390-392`). Release is gated on labelling the
  **14 composite proposals + 25 audit rows** — the one irreducibly human hour
  (`:396-398`). Until then, no claim about real-world composites.
- **Per-role budgets are required, not optional.** A labelled, held-out role set must include
  real footer-DOI placements and true titles that resemble headings and TOC entries, with
  per-role false-positive/false-negative and abstention budgets. Aggregate pass counts cannot
  identify *which* detector regressed, and an all-`UNKNOWN` parser trivially satisfies a
  wrong-accept bar.
- "Zero observed" is a **regression bar**, not a probability claim. Only 293 documents survived
  into scored pools in the first run; this corpus cannot substantiate a small production
  corruption rate. `autoBindDecisionEnabled` (`bridge.go:7682-7689`) stays **off** after all
  increments unless evidence beyond this one library justifies it.

## Bounding the two error directions

No "structure confidence threshold" is exposed — that would recreate the defect.

| Error | Cost | Bound |
|---|---|---|
| false `MENTION` / missed `SELF` | missed bind | ≥10% coverage floor; ≤1pp pairwise pass loss for shared changes |
| missed `MENTION` | **`UNKNOWN`, so a missed bind** — cannot preserve today's wrong accept | polarity, by construction |
| **false `SELF`** | **wrong accept** | finite positive grammar with no score; new forms one at a time, each earning ≥1 correct bind; ambiguous title anchors → `UNKNOWN`; foreign `SELF` identifier is negative evidence; no `SELF` on OCR until Increment 7; zero-wrong retained at every step |

The safety parameter is therefore **the reviewable, versioned set of implemented positive
`SELF` forms** — not an integer someone can quietly move from 0.72 to 0.65.

## Viability — the stop rule

Auto-binding today is a minority win: random N=2 produced **44 correct** against **249 missed**
(~85% abstention) *before* any of this tightening
(`candidate-binding-measurement.md:379-381`). The safe fallback — popup picker and inbox — is
**already shipped** (`bridge.go:7613-7671`).

So the sequence carries an explicit stop rule: **run Increment 1 first.** If safe
target-present coverage is already below the 10% floor, **do not build attribution for
autonomous binding at all.** Use qualification and structure to **rank the popup picker**
instead, and leave binding human-confirmed. Bad ranking is visible and reversible; wrong
autonomous acceptance is neither.

The 10% floor is a **product-policy choice**, not derivable from this corpus — the value is
open to revision, its existence is not.

That 61.1% of the real corpus was DOI-less makes the *admission path* important; it does not
make *automatic resolution inside it* valuable. Different questions.

## Adversarial shape that defeats this design

Page one, after extraction, of a related-work expansion:

```text
<same title as candidate>
<same authors>

Abstract
...

DOI: 10.1234/TARGET
```

with the sentence establishing the relationship lost to text ordering (a visually separate
box), and the document's **own** DOI on page two, beyond the 4 KiB window.

The standalone `DOI: TARGET` line satisfies `SELF` form 2. No foreign page-one `SELF`
identifier contradicts it. If title, authors and year genuinely agree, every gate agrees, and
it **wrong-binds**.

Both covers are refused, deliberately:

- Tightening `SELF` to "identifier structurally tied to the identity frame" sacrifices the
  17-of-40 late-own-DOI population — the reason page-one corroboration exists at all.
- Searching page two collides with the 16 KiB excerpt cap and with the OCR page-boundary
  defect above.

So it is **documented as an unsupported shape and left at `Review`**. Recovering it means
multi-page structural extraction: a new capability with its own measurement, not another
exception bolted onto `SELF`.

## Preserved from v1 (still verified)

- **No layout data exists.** `ExtractText` yields a `pdftotext`/OCR string; `textReport` stores
  a byte-truncated excerpt; no coordinates or font metadata. Attribution must work from text,
  line structure and segment boundaries only. TOC detection from numbered lines and indent
  stacks, not column x; headings from short lines, numbering prefixes and following blanks, not
  font size; running heads from repeated early lines.
- **Do not consolidate** `internal/work` (version-preserving) with `internal/ownership`
  (version-collapsing); preserve DOI slash runs. Attribution receives hits produced under
  existing identifier semantics and classifies their surroundings — it changes **no**
  equivalence relation.
- **What this does not fix:** pool cap as a safety lever (refuted); majority auto-bind
  coverage; `conclusive-veto` at 0 trials; unfilled `same-venue-year` / `title-superset` arms;
  foreign arXiv/PMID blind veto; integration safety (eligibility pool, bind fence, ownership,
  concurrency); slash-run foreign/same ambiguity; books/output cap; column-shredded text
  layers; whole-document bibliography past 16 KiB; backlog arm as calibration.

## Open before implementation

1. **Pro adversarial review of this v2** — commissioned; this document is the input.
2. The **10% viability floor** is a product decision and needs the operator's number.
3. The **OCR form-feed defect** should be filed as its own change before Increment 7 depends
   on it.
