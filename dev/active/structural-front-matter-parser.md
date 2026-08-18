# Identity attribution for DOI-less binding (formerly "structural front-matter parser")

Status: **design v3 (2026-08-17)** — four reviews, three of them adverse. v1 shipped
unreviewed in `dd9c792` and survived none. Review artifacts:

- Reviewer verdict **NEEDS REVISION** (7 findings) — `history://ParserDesignReview`
- Factual anchor audit — `history://ParserAnchorAudit`
- Independent competing design, GPT-5.6 Sol —
  `dev/scratch/oracle/20260817T131434Z-parser-independent-design/answer.md`
- **Adversarial review of v2, GPT-5.6 Sol Pro — NEEDS REVISION (10 findings)** —
  `dev/scratch/oracle/20260817T132720Z-parser-v2-pro-review/answer.md`


**Role changed 2026-08-18.** Autonomous binding is now ON (ADR-0020, 2026-08-18
amendment), enabled on measurement rather than on this design being finished. So
this plan is no longer a *precondition* for autonomous binding — it is the only
mitigation for an exposure that is now **live**: an unlabelled document that
reprints another work's title, authors, year and identifier is filed as that
work, 311 times out of 311 in the measurement's synthetic arm. The two real
forms found by review are now `correctionMarkers` and park, but a vocabulary
cannot close the family. That raises this work's priority and lowers its
optionality; it does not change a line of the design below, and §The
monotonicity invariant is still what it must satisfy.

The viability floor question in §Open before implementation is **answered**: the
operator's number is the decision to enable at 20.4% correct binds, so the floor
is at or below that. The remaining open item is the labelling hour.

Every code claim below was verified against the tree. **Nothing in this document is built**
except the OCR page-separator fix, which the fourth review moved out of this plan entirely
(§P0, shipped).

v2's central safety claim was **false**, and v3 exists to state the invariant that makes it
true. The correction is in §The monotonicity invariant; read that before anything else.

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

Consequence, and v2 stopped here: **failing to recognise a mention yields `UNKNOWN`, not
`SELF`.** True as far as it goes, and not sufficient — see immediately below.

## The monotonicity invariant

v2 claimed the ternary type removed detector recall as a safety parameter. **That claim was
false**, and the fourth review broke it: the proposed `SELF` grammar contained predicates
whose truth is *created by missing evidence*.

- "**only one** plausible title anchor" — miss the real anchor and the false one becomes the
  only anchor *found*, so the ambiguity veto never fires and `UNKNOWN` becomes `SELF`.
- "standalone identifier line with **no surrounding prose**" — lose the relationship sentence
  to column shredding or reading order and absent context becomes positive evidence.
- "**no** competing identity assertion" — an assertion that was not detected reads as one that
  does not exist.

These are not hypothetical: the corpus already contains correct documents whose real titles
are unavailable as contiguous text (column shredding) and wrong documents whose requested
title survives intact as a contents-list entry or section heading. A document combining two
already-observed extraction phenomena leaves the false anchor as the only found anchor.

So the ternary type is a genuine improvement — it removes v1's "not recognised as a mention
means acceptable" — but by itself it **relocates** the safety parameter into the `SELF` proof
system rather than removing it. The label changed; the dependence did not.

The polarity becomes an end-to-end safety property only under one invariant, which v3 adopts
as its governing rule:

> **Information loss may demote `SELF`. It may never create it.**
>
> Any predicate of the form "no prose **found**", "only one anchor **found**", "no conflicting
> assertion **found**" may **veto** or **abstain**. None may positively establish `SELF`.
> Every authorising `SELF` proof must rest on evidence that is **present**.

Two immediate casualties, both required:

1. **`SELF` form 2 — the standalone labelled identifier line — is no longer authorising.** It
   becomes `UNKNOWN`, i.e. `Review`. Its warrant was an absence ("no surrounding prose"), and
   the review found an ordinary-publisher counterexample family, not just a constructed one:
   an Oxford Academic *Editor's Note* has its own DOI while rendering the 2004 article it
   discusses as a bare `doi: 10.1210/en.2003-0985` line, and eNeuro commentaries carry
   "See related article" DOI footnotes alongside their own article DOI. `Editor's Note` is not
   in the correction-marker vocabulary. A publisher-controlled standalone `DOI: X` line is
   therefore **not** intrinsically self-identifying. Only identity-frame evidence authorises.
2. **The ambiguity rule is a veto, not a selector.** Two found anchors abstain; one found
   anchor does not thereby become `SELF`.

v2 also contradicted itself here and the contradiction is instructive: it said the adversarial
shape "wrong-binds" and then claimed it is "left at `Review`", with no specified transition
producing that. Under the invariant it genuinely is `Review`, because form 2 no longer
authorises.

The remaining scope change from v1: v1 was named for a parser and sized like one. The
capability is narrower — **attribution over already-extracted text**, not document
understanding. Renamed accordingly.
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

## P0 — the OCR page-boundary defect (found in review, **shipped**, not part of this plan)

`extractOCR` concatenated per-page Tesseract output with `all.WriteString(text)` and inserted
**no form feed**. `identityWindow` derives "page one" by cutting at the first form feed, so
with none present every front-matter window became the first N bytes of the **whole document**.

v2 filed this as parser risk and deferred it to the last increment. The fourth review showed
that reading is wrong, and it is right: the defect corrupts the **blind** path, today.
`FrontMatterDOIs` takes the *conclusive* DOI set from the 1 KiB window
(`identity.go:805-807`), and blind naming runs *before* the DOI-less candidate branch
(`bridge.go:7613-7615`). The blind path has no candidate to check against, so a DOI reaching
that window does not corroborate an identity — **it mints one**. A scanned page one with
little text lets a DOI printed on page two land inside the synthetic "first 1 KiB", and the
capture is filed as whatever work page two happened to cite. Production defaults to OCR
enabled with `max_ocr_pages` 4.

Fixed independently of this plan: `appendOCRPage` inserts `\f` between pages, with the
reproduction pinned in `internal/pdf/ocr_page_boundary_test.go` — sparse page one plus a
foreign DOI on page two, asserted against all three windows, including a test that the
*unseparated* form still leaks so the causal claim cannot silently stop being true.

Two consequences that **did** stay with this plan:

- The extraction cache had no version (`<key>-<size>-<mtime>.txt`), so a warm cache would have
  served pre-fix text while the code under test was fixed — the same shape as editing an
  applied migration in place. Entries are now `-v<N>.json` with `cacheFormatVersion`; bump it
  with any change to what `ExtractText` produces.
- A cache hit reconstructed only text and char count, leaving `OCRUsed` **false** for every
  cached document. Any rule conditioned on OCR — including this plan's refusal to trust page
  boundaries in OCR text — would have read that lie as "this document has a real text layer".
  Flags are now part of the entry.

Still true for this plan: **no structure-derived `SELF` decision runs when `OCRUsed` is true**
until the fix has been measured on a cold cache.

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
started; the existing title delimiter machinery; and **adjacency** — a `SELF` title must anchor
a *present* title→byline/affiliation sequence, not merely appear early.

**Ambiguity is a veto, never a selector.** Two positions that independently look like the
document's own title yield `UNKNOWN`. One position that looks like it does **not** thereby
become `SELF`: "only one anchor found" is an absence, and under the monotonicity invariant an
absence may abstain but may not authorise. A missed real anchor must not promote the false one.

`SELF` for an identifier requires positive structural evidence, and initially **one** form
only:

1. it occurs in the parsed identity frame on a metadata-shaped identifier segment, adjacent to
   present title/byline evidence for the same document.

The standalone labelled page-one line — `DOI: <doi>`, `https://doi.org/<doi>` and the
arXiv/PMID equivalents — is **`UNKNOWN`, not `SELF`**. v2 made it authorising to recover part
of the 17-of-40 population that prints its own DOI below the abstract
(`identity.go:993-994`); the fourth review showed it is an ordinary-publisher wrong-accept
primitive (§The monotonicity invariant: Editor's Notes, related-article commentaries). Those
documents now reach `Review`, which is the honest outcome — recovering them needs a structural
feature stronger than the label itself, demonstrated on real extracted PDFs, and enters as an
Increment 8 recovery form that must earn ≥1 correct bind with zero new wrong ones.

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

**And no new gate.** v2 proposed `GateSelfIdentifierConflict` as a pre-author hard abstain, on
the argument that `Review` alongside a qualifier suppresses selection
(`candidate_select.go:703-704`, verified) so the cited candidate must hard-fail for the real
one to qualify. The fourth review inverted that argument, and the inversion holds:

> That suppression **is a shield**. Take the dangerous direction this design is built to
> resist — a false `SELF`. Candidate A is the real work but its own identifier lies outside
> the observed window, so A would reach `Review`. A mention of candidate B is falsely
> classified `SELF`, so B qualifies. **Without** the new gate, A's `Review` suppresses B and a
> human is asked. **With** it, A is hard-failed before ever reaching `Review` — B is left as
> the sole qualifier and the false `SELF` becomes a wrong autonomous bind.

A gate that deletes candidates before the selector can weigh them converts the selector's
conservatism into confidence. So conflict is resolved **at selector level, after every
candidate has been evaluated**, and eliminates a candidate only when a positively established
document-self identifier is *positively compatible* with a different candidate under an
**explicit typed equivalence relation**.

Cross-kind absence of equivalence is `UNKNOWN`, never conflict. There is no safe implicit
relation available: an accepted manuscript may legitimately carry an arXiv stamp while the
journal job is DOI-oriented, and `BoundDOIs` cannot express arXiv or PMID identity at all. So
"incompatible with the candidate" must not mean "not among its durable DOIs", and must not
mean cross-kind inequality.

`CheckConclusiveIdentity` stays untouched: its contract is the conclusive DOI set from the
blind 1 KiB window with slash-preserving comparison (`candidate_binding.go:55-64`), and
widening it would couple blind naming to targeted matching.

If a later increment does add a gate, `gateOrder` in `internal/identitycorpus/candidates.go`
**must change in the same increment**. It duplicates the rule's gate list and `gateDepth`
returns `-1` for anything unlisted (`candidates.go:1288-1305`), so a candidate terminating at a
new gate gets depth `-1` and the report can nominate an earlier candidate as decisive — the
per-gate evidence would be wrong exactly during the increment that introduced the gate. Better:
single-source the order in `internal/pdf`, or derive depth from `CandidateQualification.Reached`.

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
| **P0** | OCR page separator — **shipped**, see §P0 | reproduction pinned; blind window no longer spans pages |
| **0** | none — freeze the baseline: exact document keys, truth classes, extraction mode, pool definitions, and the specific cell(s) whose coverage constitutes the floor. Capture pairwise and candidate baselines on a **cold** cache (P0 changed extraction) | baselines captured under both corpus and daemon-equivalent `MinChars`/OCR; **no release conclusion** if they differ materially; admission changes from P0 reported separately |
| **1** | **single-source offset-bearing matcher**, decision-inert: replace the `bool` primitive with `findFlattenedTokenSpans(text, needle) []Span` and define `containsFlattenedToken` as `len(spans) != 0` | every corroborating and attribution caller consumes the **same** spans; property tests pin start boundary, trailing prefix collision (PMID `12345` vs `123456`), whitespace/newline/Unicode spacing, punctuation, and `bool == (len(spans) > 0)`; **all** occurrences classified, not the first |
| **2** | measure page-one identifier **multiplicity** across DOI **and arXiv and PMID** using those spans | adversarial construction per identifier class, including letter-spaced and line-wrapped variants; report multiplicity distribution over real documents |
| **3** | *optional* conservative multiplicity veto as a temporary safety baseline | conjunction target-absent → 0 for **every** identifier class; every currently clean real cell stays at 0. **No viability stop attached** (see §Viability) |
| **4** | attribution computed and traced, **decisions unchanged** | every known **cited** identifier classifies `MENTION` or `UNKNOWN` — **never** `SELF`, as an independent mandatory assertion; every labelled own-identifier case matches its own expected role; both pairwise false-title spans classify non-`SELF`; report the fraction of late own-DOI cases reachable |
| **5** | smallest attribution-aware acceptance rule: **identity-frame `SELF` only**; standalone labelled lines stay `UNKNOWN` | 0 wrong binds in every target-absent cell and every named adversarial construction; **this** is where the viability floor applies, on the frozen denominator |
| **6** | `SELF` title/byline feed `title-printed-as-line` / `author-evidence`; same attribution applied experimentally to pairwise `MatchIdentity` | pairwise wrong accepts **2 → 0**; no new candidate wrong binds; pairwise own-document pass falls by **≤1 percentage point**, else revert to candidate-only |
| **7** | selector-level conflict resolution under a typed equivalence relation | must **recover ≥1 correct bind** lost by the multiplicity veto, or it is deleted; no candidate is eliminated except by a positively established, positively compatible self identifier |
| **8** | one attested `SELF` recovery form per increment | each recovers **≥1 unique correct bind**, adds **0** wrong binds and **0** pairwise wrong accepts; a no-op is deleted |
| **9** | enable structure on OCR text; then grab admission / pool construction / bind fence | same zero-wrong criteria under **production** extraction; no enabling flag until integration passes |

Three changes here are corrections, not reordering, and each has a reason:

- **Increment 4's criterion lost an `or`.** v2 required that each conjunction's cited
  identifier classify non-`SELF` **or** its own DOI classify `SELF`. The second arm makes the
  test green while the *cardinal* classifier error — cited identifier read as `SELF` — passes
  unnoticed. Worse, a later conflict rule would then mask it: while the synthetic own DOI is
  present, the conflict resolution hard-fails the cited candidate and the arm still records
  zero wrong binds. Move that same false-`SELF` classifier to the production adversary where
  the own DOI is on page two or absent, the masking disappears, and the cited identifier is the
  only `SELF` left. The two assertions are now independent and both mandatory.
- **Multiplicity is measured before it is enforced**, and over every identifier class. A
  multi-**DOI** veto does nothing to an arXiv-target conjunction (`arXiv:TARGET` plus one own
  DOI is not multiple DOIs), and PMID has no conjunction regression at all.
- **The matcher comes first** because there is already a live semantic divergence to close:
  `documentDOIs` recognises contiguous regex-shaped DOI text while the corroborator uses
  `containsFlattenedToken`, which deliberately ignores whitespace inside identifiers. So
  `Extended from DOI: 10.1145/ 30 6 5 3 8 6` can corroborate `10.1145/3065386` while a
  conclusive-DOI multiplicity check sees only the own DOI. Any multiplicity observation must
  derive from the **same** spans the corroborator used, or the veto and the acceptance rule
  disagree about what the document contains.

Additional gates on the whole sequence:

- **Real-library composite labelling is complete (2026-08-17).** The operator reviewed all
  **15 signal proposals + 25 random audit rows** (`make composite-labels`): every document was
  the article/item its metadata purported it to be, so all 40 labels are `not-composite`.
  Re-running the composite arm produced **0 confirmed composites**, **0 missed in the 25-row
  audit**, and therefore **0 adversarial pools**. This establishes only a prevalence interval:
  observed lower bound 0%; audit-bounded upper bound **11.04%** overall and **11.29%** among
  unflagged documents (exact one-sided 95%). It does **not** establish that the class is absent
  and does **not** test an authorising `SELF` classifier against a positive composite. Release
  therefore still requires the predeclared held-out positive shapes below; the library arm is
  recorded as **not measured for classifier safety**, never passed.
- **Per-role criteria must be numeric and fixed before labels are collected.** For an
  authorising role: **any** observed held-out false `SELF` is an automatic failure. Sample
  insufficiency is reported as **not measured**, never as pass. The held-out set must include
  real footer-DOI placements, true titles resembling headings and TOC entries, and the
  Editor's-Note / related-article commentary templates from §The monotonicity invariant. The
  role inventory is frozen at Increment 0; aggregate pass counts cannot identify which detector
  regressed, and an all-`UNKNOWN` classifier trivially satisfies a wrong-accept bar.
- **"Zero observed" is a regression bar, not a probability claim.** Only 293 documents survived
  into scored pools in the first run; this corpus cannot substantiate a small production
  corruption rate. `autoBindDecisionEnabled` (`bridge.go:7682-7689`) stays **off** after every
  increment unless evidence beyond this one library justifies it.
- **A parser failure is a missed bind, never a reason to remove a document from the
  denominator.** Eligibility is constructed before pools and arms thin independently — the
  first run already shows the gap (293 documents scored, 286 conjunctions built) — so an
  increment must not satisfy the floor by changing eligibility.

## Bounding the two error directions

No "structure confidence threshold" is exposed — that would recreate the defect.

| Error | Cost | Bound |
|---|---|---|
| false `MENTION` / missed `SELF` | missed bind | coverage floor at Increment 5; ≤1pp pairwise pass loss for shared changes |
| missed `MENTION` | **`UNKNOWN`, so a missed bind** — cannot preserve today's wrong accept | polarity |
| **false `SELF`** | **wrong accept** | the monotonicity invariant: no absence may establish `SELF`; finite positive grammar with no score; forms added one at a time, each earning ≥1 correct bind; ambiguity is a veto, never a selector; no `SELF` on OCR until Increment 9; any held-out false `SELF` fails the increment outright |

The safety parameter is therefore **the reviewable, versioned set of implemented positive
`SELF` forms** — not an integer someone can quietly move from 0.72 to 0.65. Note what left this
table: v2 listed "foreign `SELF` identifier is negative evidence" as a bound on false `SELF`.
Under selector-level resolution it is no longer a bound at all — a foreign `SELF` that is
itself false is precisely the hazard, so it cannot also be the mitigation.

## The authorisation ceiling — measured, 2026-08-17

Before the increments, one question had to be answered or the whole design could be
**structurally empty**: admission to the candidate path requires `FrontMatterDOIs` to be
**empty**, and v3 requires `SELF` to rest on **identity-frame** evidence. A conclusive DOI in
the blind 1 KiB window means the work is already named and candidate selection is never
reached. So if the identity frame is roughly the front matter, the population where v3's
identifier gate can ever authorise anything might be near zero *by construction*.

Neither review raised this. It is measurable now, through the same corroborator the verdict
uses (`IdentifierPrinted`, `identity.go:887-889`) over the real library:

| | documents |
|---|---|
| corpus documents | 668 |
| with a known own identifier | 576 |
| blind path names them — never reach candidates | 254 |
| **admitted to the candidate path** | **322** |

Of those 322, where is their **own** identifier printed?

| window | documents | share of admitted |
|---|---|---|
| 1 KiB front matter (matchable but not conclusive) | 33 | 10% |
| **2 KiB byline — proxy for v3's identity frame** | **58** | **18%** |
| anywhere in 4 KiB page one | 149 | 46% |
| page one but **not** byline — v3 refuses these | 91 | 28% |
| admitted documents from OCR | 5 (0 byline-printed) | 2% |

Two conclusions, and they point in opposite directions:

1. **The design is not structurally empty.** 18% of admitted documents print their own
   identifier inside the byline window despite the blind window having found nothing
   conclusive there — because those occurrences are line-wrapped or letter-spaced (matchable,
   not conclusive), or sit between 1 KiB and 2 KiB. That is a real population, and it is
   already above the proposed 10% floor **before** any recovery form. The mechanism has
   something to work with.
2. **The cost of dropping form 2 is now quantified, and it is the majority.** Of the 149
   admitted documents that print their own identifier anywhere on page one, **91 (61%)** print
   it outside the byline window. v3 sends every one of those to `Review`. That is the honest
   price of the monotonicity invariant, measured on this library rather than extrapolated from
   the 17-of-40 sample — and it is the strongest argument for prioritising Increment 8 recovery
   forms once the safe core is proven.

Read as a bound, not a forecast: 18% is a **ceiling** on identifier-gate authorisation, not
predicted coverage. A bind still needs title, author and year to agree, and false-`SELF`
filtering only removes candidates from that 18%. The byline window is also a *proxy* — a
narrower parsed frame measures lower, all of page one would measure 46%.

Reproduce: load the corpus, filter to `len(FrontMatterDOIs(text)) == 0`, and ask
`IdentifierPrinted(w, doc.Work)` for each window from `IdentityWindows(text)`. Aggregate counts
only — never emit per-document output, which names the operator's papers.

## Alternative technologies — measured, 2026-08-17

This design recovers structure from a shredded text string. That is one technology choice
among several, and two of the alternatives were cheap enough to measure on the same 322
admitted documents rather than argued about.

### A. Embedded publisher metadata — the strongest result in this workstream

`ExtractText` reads only the text stream. But publishers write the DOI into the Info dictionary
and XMP during production, and papio already discovers `pdfinfo` (`semantic.go:62`, used today
only for structural cross-checks, never identity).

| | documents | share of admitted |
|---|---|---|
| own identifier present in Info dict or XMP | 110 | **34.2%** |
| present **and absent** from the byline window — net new | 101 | **31.4%** |
| attributable to a specific named field | 106 | 32.9% |
| metadata naming **another library work** | **0** | **0.0%** |

Field histogram (documents; several fields may carry it): `xmp/dc:identifier` 76,
`xmp/prism:url` 73, `info/Subject` 68, `xmp/prism:doi` 67, `xmp/crossmark:DOI` 62,
`xmp/pdfx:doi` 60, `info/Title` 17. These are PRISM, CrossMark and pdfx production fields —
publisher assertions about **this** document, not text that happens to appear on a page.

Three things follow, and together they outrank the parser:

1. **Nearly twice the reach of the frame rule, and almost disjoint from it.** 34.2% against
   18%, with 31.4 points of it in documents the frame rule cannot serve. Combined reach of
   metadata *or* today's frame is **49.4%**.
2. **The attribution problem does not arise.** `prism:doi` *means* "this document's DOI". No
   `SELF`/`MENTION` inference, no monotonicity invariant, no positive-form grammar — the field's
   semantics answer the question that the entire 364-line parser exists to guess at.
3. **Zero measured wrong-accept exposure — among primary attachments.** Every admitted document
   was checked against every other identified library work (~185k pairwise checks): **no
   document's metadata carried another work's identifier**. Exact one-sided 95% upper bound on
   the per-document contamination rate: **≤0.93%**. Contrast the text path, where the whole
   design problem is that page one routinely carries other works' identifiers.

**That third claim has a structural blind spot, found by re-measuring rather than by review.**
The run loaded the corpus one-per-parent, so every **secondary** attachment — supplement,
alternate scan, publisher cover sheet — was excluded *before* measurement. And a supplement is
the one shape that defeats metadata corroboration: the publisher produced it as part of the
article, so its XMP ordinarily carries the **parent article's** DOI. Bind target-aware with the
parent as candidate and the metadata agrees while the bytes are the wrong document.

Re-run with `AllAttachments: true`, only **4** secondary attachments reach the admitted
population, and **1 of the 4** carries the parent's identifier in metadata. So this library
**cannot measure the hazard** — n=4 puts the one-sided 95% upper bound near 50% — and the
honest statement is *unmeasured*, not low. Two consequences, both binding:

- **Metadata corroborates; it never authorises alone.** It enters as an additional source for
  the existing `identifier-page-one` role, inside the existing conjunction, so a supplement must
  still pass `title-printed-as-line`, `author-evidence` and `year-token`. Metadata may not
  substitute for identity-frame agreement, only for where the identifier was found.
- **The composite signals are the specific counter-measure**, not a general safety story:
  `secondary-attachment` and `title-quotes-title` are precisely the detectors for "supplement
  carrying its parent's identity", and they are the arm the labelling run left at zero
  confirmed positives.

Constraints that still apply, and are not negotiable:

- **Target-aware only.** Ask "does this document's metadata name *the candidate's* identifier?"
  Never mint identity from metadata blind: a template or aggregator error would name a work
  with nothing to check it against, which is the `FrontMatterDOIs` hazard with a new source.
- **A named field is required**, not a substring of the whole blob — 4 of the 110 matched only
  the concatenated output, which is a probe artifact, not evidence.
- **This library under-samples aggregator cover sheets.** ProQuest is papio's highest-volume
  destination and a stamped cover leaf is exactly the shape that could rewrite XMP; 48
  papio-owned artifacts and 175 missing files were outside this run. Contamination must be
  re-measured on a grab-sourced population before metadata is allowed to authorise alone.
- **XMP is author-controllable.** Unlike the text path, where a wrong identifier must survive
  visible typesetting, metadata is invisible and freely writable by whoever produced the file.
  That is a threat-model question this measurement does not address and is the one part of this
  increment worth adversarial review.

### A1. Shipped, 2026-08-17 — metadata corroboration

Implemented as the first behaviour-changing increment, ahead of the parser, in
`internal/pdf/metadata.go` with the gate change in `candidate_select.go`:

- `ExtractMetadata` reads the XMP packet through `pdfinfo` in one bounded subprocess.
  The Info dictionary is **not** read: the PDF specification gives it no
  identifier-semantic key, and every Info-dict hit in the measurement was free text.
- Allowlist of fields whose defined meaning is "this document's identifier":
  `prism:doi`, `prism:url`, `crossmark:doi`, `crossmark:doiurl`, `pdfx:doi`,
  `pdfx:WPS-ARTICLEDOI`, `dc:identifier`, `dcterms:identifier`. Free-text fields are
  excluded even though the exploratory probe found 68 documents with a DOI in
  `info/Subject` — that field asserts nothing about whose identifier it is.
- Vocabularies resolve by **namespace URI substring**, not by the prefix written in
  the file (arbitrary) and not by exact URI (PRISM ships four schema versions, and
  exact matching would fail closed on the next one).
- Values are attributed to the nearest enclosing **property**, not to the RDF
  container they sit in. This was a real bug found before the first test run:
  `dc:identifier` → `rdf:Bag` → `rdf:li` is the commonest shape, and resolving
  `rdf:li` to its namespace URI silently dropped the largest measured population.
  Pinned by `TestParseXMPAttributesValuesToTheEnclosingProperty`.
- `NamesWork` delegates to `corroboratingIdentifier` — the same matcher the text arm
  uses — closing the two-matcher divergence Pro's finding 9 named, in this arm.
- Gate 5 accepts metadata **as an alternative source for the same gate**, never as a
  bypass, so title/author/year still gate a supplement carrying its parent's DOI.
- Acceptance set widened ⇒ `CandidateBindingRule` is now `candidate_auto_bind/3`.
- `BindDocument.Digest` pins what the predicate read. A document with no metadata
  digests to exactly its excerpt hash (so existing provenance stays comparable);
  metadata that contributed changes it, because an audit row that cannot
  distinguish its own inputs cannot reconstruct its decision.

**Production reader measured on the real library**, admitted population (n=322):

| | documents | share |
|---|---|---|
| carry at least one allowlisted field | 94 | 29.2% |
| **metadata names the work** | **87** | **27.0%** |
| field present but no match | 7 | 2.2% |
| — of those, curated record has no DOI (arXiv/PMID-only) | 2 | |
| — of those, naming a **different** library work | **0** | |
| exploratory probe, whole blob incl. free text | 110 | 34.2% |

So the safety-defensible narrowing costs 23 documents (7.2 points) against the probe,
and still reaches **27.0%** versus the text frame rule's 18%. The five remaining
mismatches name no other library work — preprint/version-of-record DOI differences,
which are misses, not wrong accepts.

`autoBindDecisionEnabled` stays **false**. This increment improves the predicate and
the evidence; it does not turn autonomous binding on, which still requires the
corpus measurement and the floor.

### A2. Reviewed after shipping, 2026-08-17 — two questions A1 had not asked

A1's safety claim ("0 documents whose metadata named a different library work,
~185k pairs") was measured over **primary** attachments. Two populations it
therefore says nothing about, both measured now.

**1. Intra-metadata disagreement — non-monotone by construction, zero prevalence.**
`NamesWork` returns on the FIRST allowlisted field carrying the candidate's
identifier and never asks whether a sibling field names something else. So a file
whose XMP carries two DOIs corroborates any candidate matching either one, and
finding *more* identifiers can never cause abstention — the exact inversion of the
monotonicity invariant §The monotonicity invariant states for the text arm.

Measured: of 95 admitted documents carrying allowlisted metadata, **0 carry ≥2
distinct DOIs** (exact one-sided 95% upper bound ≤3.1%). So the defect is real in
logic and unmeasurable here. **Do not fence it yet**: a fence costs a rule version
and this library cannot show it firing. Recorded so the next grab-sourced
population re-asks it — aggregator cover sheets are the shape that would carry two.

**2. Supplements scored against their own parents — the fence is one field thick.**
A supplement's XMP ordinarily carries its parent's DOI, and in the corpus
`Document.Work` for a secondary attachment IS the parent's record, so every
secondary attachment is a ready-made adversarial trial with ground truth: an
`accept` means supplementary bytes filed under the article's citation.

| | trials |
|---|---|
| secondary attachments, empty front-matter DOI window | 6 |
| **accept (wrong-accept)** | **0** |
| review (parks) | 1 |
| abstain (hard-fenced) | 5 |

Fenced by `GateTitle` 3, `GateAuthor` 1, `GateYear` 1 — and by
`candidateNonArticleMarker` **0**. Two consequences:

- The marker vocabulary is **not** what protects this path, so extending it
  (`"supplementary appendix"` (NEJM), `"additional file"` (BMC), `"data supplement"`
  (AHA) are all absent) would be theatre against this evidence. Left alone.
- The single park **reached the identifier gate** and stopped only because that
  file's metadata did not name its parent. Had it, the trial would have been an
  accept. So in front of the metadata arm the fence is title+author+year and
  nothing else, and 6 trials cannot bound it.

`n=6` is the finding. A Zotero library holds the papers the operator **kept**, so
it structurally under-samples supplements, cover sheets and aggregator rewrites —
which is what the browser path captures, ProQuest being papio's highest-volume
destination. Both of A1's safety numbers are therefore measured on the wrong
population for authorising autonomous binding, and no larger run against this
library fixes that. **Release gate: the grab-sourced population owed by §A's
constraints is now the blocking measurement, not an improvement to the predicate.**


### B. Layout-preserving extraction — refuted, as measured

`pdftotext` runs with **no flags** (`semantic.go:90`), so "no layout data exists" is a property
of the invocation, not of poppler: `-layout`, `-bbox` and `-bbox-layout` are all available. The
obvious hypothesis is that column shredding hides own identifiers from the byline window.

It does not survive measurement. With `-layout`:

| window | bare (today) | `-layout` |
|---|---|---|
| 2 KiB byline | 58 | **47** |
| 4 KiB page one | 149 | **85** |

`-layout` **loses** reach, and adds exactly **1** document to the frame. The cause is
structural: the identity windows are **byte**-bounded, and layout padding fills them with
whitespace, so the same 4 KiB reaches less of the page. Any real test of layout awareness
therefore requires structure-bounded windows first — which is a larger change than this plan,
and one whose payoff is now known to be small on this population.

`-bbox-layout` (word-level coordinates and font data) is a genuinely different capability and
remains unmeasured. It is the only version of "layout" worth revisiting, and only after
metadata corroboration ships.

### C. Assessed, not measured — where ML belongs and does not

- **GROBID** is the purpose-built tool for exactly this task: TEI header extraction with
  reference-list segmentation, trained on it. It would subsume both the parser and the
  reference/body separation. The cost is architectural, not technical: a Java service contradicts
  papio's single-binary, offline, zero-runtime-dependency posture, and it is not obviously worth
  it once metadata covers a third of the population for free. Reconsider only if metadata plus
  picker ranking leaves a large residue — and then measure its wrong-accept rate on this same
  corpus before adopting, not its published F1.
- **Document-AI models** (Nougat, Marker, MinerU, Docling) reconstruct documents to markdown.
  They **hallucinate**, including in identifier strings, and identifier acceptance is the one
  place papio cannot tolerate a plausible invention. Disqualified for authorising. Tolerable as
  a ranking signal.
- **A local LLM verdict on "is this DOI the document's own?"** is disqualified for the same
  reason plus a specific one: v3's settled safety parameter is *the reviewable, versioned set of
  implemented positive forms*. A model's decision boundary is neither reviewable nor versionable,
  so it cannot be the acceptance authority. It can veto, and it can rank.
- **Embeddings for picker ranking is the safe, high-value ML use, and it needs none of this
  machinery.** Rank the already-shipped popup picker by similarity between page-one text and
  candidate metadata. Bad ranking is visible and reversible — the plan's own stop-rule
  reasoning — so it carries no wrong-accept budget at all. If the floor is not cleared, this is
  the fallback, and it can ship independently of every increment here.
- **Crossref/OpenAlex reverse lookup by extracted title** adds little *for binding*: the
  candidate's metadata is already known, so `MatchIdentity` compares title/author/year directly
  without a network round trip, rate limits, or offline failure. Reverse lookup earns its keep
  for **blind** identification (a PDF with no pending job), which is a different feature.

### Consequence for the sequence

**Metadata corroboration becomes the first behaviour-changing increment, ahead of the parser.**
It is deterministic, needs no new safety vocabulary, reuses the existing target-aware
corroboration seam, and measures better on both axes than the rule this plan was built to add.
The parser's marginal value must then be re-derived against the residue it actually leaves —
not against today's 100% gap.

## Viability — the stop rule, and what it must not be attached to

Auto-binding today is a minority win: random N=2 produced **44 correct** against **249 missed**
(~85% abstention) *before* any of this tightening
(`candidate-binding-measurement.md:379-381`). The safe fallback — popup picker and inbox — is
**already shipped** (`bridge.go:7613-7671`).

So the sequence carries a stop rule: if safe target-present coverage falls below the floor,
**do not build attribution for autonomous binding.** Use qualification and structure to **rank
the popup picker** instead, and leave binding human-confirmed. Bad ranking is visible and
reversible; wrong autonomous acceptance is neither.

**v2 attached that stop to the wrong measurement.** It fired on the coarse multiplicity veto —
but Increment 7 exists precisely to *recover* binds the veto lost, so the veto's coverage is a
**lower bound** on what attribution can safely recover, not an upper bound on its value. A
result of 8% would be unreadable: it could mean "this capability is worthless" or "the crude
veto is extremely crude and attribution has a large recovery population". Those are opposite
conclusions and the veto cannot distinguish them. Worse, the veto craters coverage *precisely
when* ordinary papers carry a second DOI — a data-availability, related-work, correction or
funder DOI — which is exactly the distinction attribution exists to make. The stop rule would
have killed the workstream on the crudeness of its own placeholder.

So: the floor applies at **Increment 5**, the first attribution-aware acceptance rule, on the
denominator frozen at Increment 0. The multiplicity veto may still ship as a temporary safety
baseline; it carries no viability verdict. If the safe positive-only rule cannot clear the
floor, **that** is a meaningful negative result and the answer is picker ranking.

The floor's **value** (10% was proposed) is a product-policy choice, not derivable from this
corpus, and needs the operator's number. Its existence is not optional.

That 61.1% of the real corpus was DOI-less makes the *admission path* important; it does not
make *automatic resolution inside it* valuable. Different questions.

## The adversarial shape, and how v3 resolves it

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

**Under v2 this wrong-bound**, and v2's own text admitted it while simultaneously claiming the
shape was "left at `Review`" — a contradiction with no transition to support it. The standalone
`DOI: TARGET` line satisfied form 2; nothing contradicted it; every other gate agreed.

**Under v3 it reaches `Review`**, and by construction rather than by exception: form 2 no longer
authorises, so the only page-one identifier evidence is `UNKNOWN`. This is the single clearest
demonstration of why the monotonicity invariant is the design, not a caveat on it — the
adversary's whole method is *removing* the relationship sentence, and under the invariant
removal can never manufacture `SELF`.

The cost is stated honestly: the 17-of-40 population that prints its own DOI below the abstract
now reaches `Review` too. That is a **missed bind**, the cheap error, and it is recoverable
later — but only by a positive structural feature stronger than the label itself, demonstrated
on real extracted PDFs, entering one form at a time (Increment 8).

Refusing multi-page extraction remains correct, and is now consistent: "page-two evidence is
unavailable, therefore this page-one standalone DOI stays `UNKNOWN`" is coherent. v2's position
— refuse page two *and* treat a standalone page-one DOI as `SELF` — was not.

## Preserved from v1 (still verified)

- **No layout data exists *in what papio extracts*** — a correction to v1's flat claim.
  `ExtractText` runs `pdftotext` with no flags, so it yields a plain `pdftotext`/OCR string and
  `textReport` stores a byte-truncated excerpt: no coordinates, no font metadata. So attribution
  as designed here must work from text, line structure and segment boundaries only — TOC
  detection from numbered lines and indent stacks, not column x; headings from short lines,
  numbering prefixes and following blanks, not font size; running heads from repeated early
  lines. But poppler *can* supply coordinates and font data (`-bbox-layout`), so this is a
  boundary of the current invocation rather than of the format. §Alternative technologies
  measures the cheap version (`-layout`, which loses reach) and leaves `-bbox-layout` open.
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

1. **The viability floor's value** is a product decision and needs the operator's number. 10%
   was proposed; its placement (Increment 5, frozen denominator) is settled, its value is not.
2. **Composite labelling — complete.** All 15 proposals and 25 audit rows were
   `not-composite`. The prevalence bound is recorded above; because it yielded no positives,
   held-out positive composite shapes remain a release prerequisite rather than a claim this
   library can substantiate.
3. **Whether to build *this* at all — reopened by §Alternative technologies.** The ceiling
   measurement said the design is not structurally empty (18% of admitted documents), which
   answered viability. The technology measurement then found a source with **34.2%** reach,
   31.4 points of it net new, and **zero** measured contamination — where the attribution
   problem does not exist at all. So the order changed: **ship metadata corroboration first**,
   then re-derive the parser's value against the residue. Broad autonomous binding remains
   unsupported either way; 61% of page-one-printed own identifiers fall outside the frame.
4. **Contamination re-measurement on a grab-sourced population** before metadata may authorise
   alone — this library under-samples aggregator cover sheets, which are the one shape that
   plausibly rewrites XMP.

## Review history

| Version | Fatal finding | Source |
|---|---|---|
| v1 | safety depended on **detecting** a mention; unbounded detector recall | reviewer, 7 findings |
| v1 | 286/286 was a **synthetic** arm, not a real measurement; every `bridge.go` anchor stale | reviewer + anchor audit |
| v1 | one newline defeated Increment A; DOI-only while the gate accepts arXiv/PMID; gate numbering self-contradictory | reviewer + my verification |
| v2 | the ternary claim was **false** — absence-based predicates create `SELF` | Pro, finding 1 |
| v2 | `SELF` form 2 has an **ordinary-publisher** counterexample family | Pro, finding 2 |
| v2 | the OCR defect corrupts the **blind path today**; belongs first, not last | Pro, finding 3 |
| v2 | the new hard gate **removes** an existing selector shield | Pro, finding 7 |
| v2 | the stop rule drew the **opposite** inference from its own measurement | Pro, finding 5 |
| v2 | Increment 2's `or` let the cardinal classifier error pass | Pro, finding 6 |
