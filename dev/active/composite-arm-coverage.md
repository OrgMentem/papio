# Composite-arm coverage holes — scoping (2026-08-17)

Status: **holes 1 and 2 shipped** in `aa242f2` (marker-gate synthesis arms, all-attachments path, anti-empty-arm reporting). **Hole 3 remains open and needs the operator**: 14 composite proposals plus 25 audit rows require human labelling before prevalence has any upper bound.
No implementation in this file; anchors are to the tree at the time of writing.

## Context from the 2026-08-16 run

The real-library candidate-set instrument (`identitycorpus.MeasureCandidateSets`,
`cmd/identity-corpus -candidates`) reported three **coverage holes** that must
not be read as clean gates:

| hole | symptom in report | why it is not “passed” |
|---|---|---|
| 1 | `non-article-marker` and `correction-marker` at **0 trials** | no arm synthesizes bytes bearing those markers |
| 2 | composite arm **empty** under default `Load` | `dedupOnePerParent` drops the attachment class |
| 3 | composite prevalence **lower bound only** | 14 proposals, all unreviewed; recall unbounded |

Standing constraints (already arbitrated in the plan — carried here because they
bound every design below):

- Ground truth is an **equivalence class**, canonicalized with
  `ownership.NormalizeIdentifier` (`internal/ownership/ownership.go:342-362`,
  version-collapsing). Never `work.Normalize*`. Never `sameWork`
  (`internal/identitycorpus/measure.go:119-133`).
- A confirmed composite’s correct outcome is **bind nothing** → empty target
  class even when the referred work is in the pool (`composite.go:48-54`,
  `candidates.go:151-161`).
- Unestablished class → **exclude**, count `unestablished`, never guess
  (`candidates.go:46-47`, `408-421`; first run: 98 of 391).
- Sampling unit = **document**, not trial (`candidates.go:211-215`, `1329-1338`).
- `cmd/identity-corpus` stays a **local operator tool**, never CI
  (`candidate-binding-measurement.md:313-314`).

---

## Hole 1 — marker gates at zero trials

### What the instrument currently proves

`CandidateReport.Render` already states the contract (`candidates.go:1538-1546`):
seven gates (`candidate_select.go:71-78`), and a zero on
`non-article-marker` / `correction-marker` means **coverage this run does not
have**, not a gate that held.

The conjunction arm deliberately **avoids** marker vocabulary in its padding
(`candidates.go:863-867`): an accidental `"supplementary information"` prefix
would terminate at `GateNonArticle` and measure the filler, not the composed
adversary. So conjunction cannot close marker coverage without a separate arm.

The synthetic gate corpus *does* contain DOI-less correction cases (e.g.
`case04b_correction_notice_no_doi.txt`, manifest `correction-notice-no-doi` with
`expected_gate: correction-marker`), but **nothing in the library measurement
reuses that geometry** — the real instrument only calls `SelectAutoBindCandidate`
over library text or conjunction synthesis.

### Scoping: a dedicated **marker-gate synthesis arm**

Add one new synthesized arm (name suggestion: `marker-gate`, or two reported
sub-rows `marker-correction` / `marker-non-article`) beside `conjunction`, **not**
folded into composite prevalence.

#### Document synthesis spec (minimal, production-faithful)

Each trial uses `Pool.text` override (same mechanism as `ArmConjunction`,
`candidates.go:163-171`) so the **doc key remains the sampling unit** (a real
DOI-less eligible document whose metadata supplies title/authors/year).

**Admission (non-negotiable):**

1. `len(pdf.FrontMatterDOIs(text)) == 0` — production admission
   (`candidates.go:33-40`, `bridge.go` branch cited in plan).
2. No conclusive foreign DOI in the 1 KiB blind window (otherwise
   `GateConclusiveVeto` fires first; in this corpus veto is unreachable by
   construction for admitted docs — `candidates.go:49-53`).

**Byline geometry:** use the same windows as the rule:

- Marker detection: folded byline + `wideGapSegments` path
  (`candidate_select.go:179-190`, `399-410`, `identity.go:522-537`).
- For “glued running head” regression: optional variant with `"12  Erratum: …"`
  two-space gap (called out in `identity.go:99-103`).

**Family A — `correction-marker` (erratum / corrigendum shape)**

Template derived from `case04b_correction_notice_no_doi.txt` and
`QualifyCandidate` comments (`candidate_select.go:151-175`):

```
{correctionMarkers prefix}: {target title}

{author lines matching target.Work.Authors}
{year token matching target.Work.Year}

{filler to ≥1 KiB without DOI, without non-article prefix, without correction pointer phrases}

Optional page-one line (after filler): DOI: {target DOI}   # cites original; must NOT be only self-assertion test for THIS arm
Own identifier line (optional, conjunction-style): past 1 KiB, inside page-one 4 KiB — only if a second sub-cell wants to test “marker before cited DOI” ordering
```

Constraints:

- Prefix must match `correctionMarkers` (`identity.go:39-44`), not
  `correctionPointerPhrases` (`identity.go:54-57`) — e.g. forbid
  `"Erratum to this chapter is available at …"`.
- **Do not** place a conclusive DOI in the first 1 KiB (would change admission).

**Family B — `non-article-marker` (supplement / SI shape)**

```
{nonArticleMarkers prefix}: {short title or “for {target title}”}

{minimal author/year if needed for pool realism — gates 1b fire before author gate}

{filler ≥1 KiB, DOI-less, no correction prefix}
```

Use vocabulary from `nonArticleMarkers` (`identity.go:22-26`), including a
wide-gap variant (`"1  Supplementary information …"`) per
`candidate_select.go:396-398`.

#### Pool construction

- **Target-absent only** for the primary marker coverage cell: `TrueKeys` empty,
  `TargetAbsent: true`, provenance `adjudicated:marker-gate synthesis`
  (`candidates.go:151-161`).
- Pool size: **N=2 minimum** (`candidate_gate_test.go:192-195`); optionally sweep
  `{2,5}` — marker termination should be **independent of N** (if not, that is a
  finding).
- Candidates:
  - **Distractor:** job built from the **referred-to work** (same title/authors/year
    as printed) — the adversarial “would bind the paper” case.
  - **Second candidate:** random DOI-less filler from `candidateUniverse`
    (`candidates.go:603-652`).
- Draw semantics: same seeded `poolRand` as other arms (`candidates.go:755-767`).

#### Assertions (what “closed” means)

Per trial, record via existing `evaluatePool` / `decisiveGate`
(`candidates.go:1159-1257`):

| check | expected |
|---|---|
| `TerminalGate` | `correction-marker` **or** `non-article-marker` (one sub-arm each) |
| `SelectAutoBindCandidate` | `ok == false` |
| `Outcome` | `correct-abstain` (empty class) |
| `BindWrong` | **0** — a bind here is cardinal failure |

Report success criterion for hole 1:

- `GatesObserved[correction-marker] > 0` and `GatesObserved[non-article-marker] > 0`
  on a full `-candidates` run, **or**
- explicit `NOT REPRESENTATIVE` with `eligible == 0` for that sub-arm (should not
  happen if synthesis always fills).

**Secondary path (external validity, not a substitute for synthesis):** once hole 2
is active, **confirmed** DOI-less composites from the composite arm whose
`markerProbe` (`composite.go:278-305`) hits a marker gate also increment the same
gate counters — but the library may contain zero such documents; synthesis remains
the closable-by-construction half.

---

## Hole 2 — real composite arm unbuildable without all-attachments mode

### Current loader behaviour (ground truth for scoping)

Default `Load` → `load(..., allAttachments false)` (`corpus.go:155-157`).

`dedupOnePerParent` (`corpus.go:614-647`):

- Keeps **one** PDF per Zotero parent (lowest `attachmentID`).
- Skips others with reason `"parent has another PDF attachment"` — supplements,
  alternate scans, errata PDFs filed as second attachments.

`LoadOptions` / `LoadWithOptions` (`corpus.go:159-189`):

```go
type LoadOptions struct {
    ZoteroDir, CacheDir string
    Workers             int
    AllAttachments      bool // opt-in; documented at :167-181
}
```

When `AllAttachments: true`, `selectAttachments` (`corpus.go:649-680`):

- Keeps **every** PDF candidate.
- Sets `candidate.secondary` when `attachmentID != primary[parentID]`.
- Sorts by `attachmentID` for deterministic order.
- **No** skip rows for “duplicate attachment” (those attachments become documents).

`Document.Secondary` (`corpus.go:53-63`, set in `extractOne` `:1495`) marks non-primary attachments.

CLI wiring (`cmd/identity-corpus/main.go:437-449`): `-candidates` with
`-composite-labels` and composite arm selected → `LoadWithOptions{AllAttachments: true}`.
Default pairwise mode and candidate runs **without** composite labels stay on
one-per-parent so `Measure`’s baseline does not move (`corpus.go:152-154`).

### What all-attachments mode changes (pairing & ground truth)

| topic | default corpus | all-attachments mode |
|---|---|---|
| Document count | ~one PDF per work | +secondaries (plan: seven parents with two PDFs on reference library) |
| `Document.Work` | parent bibliographic metadata | **unchanged** — still parent’s curated record |
| `Measure` pairwise | safe to run | **must not** — secondary front matter ≠ parent metadata (`corpus.go:177-181`) |
| Equivalence class for secondaries | N/A (dropped) | **not established** from parent metadata → `unscorableTarget` (`candidates.go:408-421`) |
| Composite arm | invisible | secondaries become scorable **only after human composite label** assigns empty class |

Ground-truth rule for composite pools (`composite.go:950-959`, `candidates.go:1089-1110`):

- Human-confirmed composite → `TrueKeys == nil`, `TargetAbsent: true`,
  provenance `adjudicated:…` (`composite.go:1145-1170`).
- Referred-to work added as **candidate** when known (`composite.go:951-953`).
- Only **DOI-less** confirmed rows get pools (`composite.go:961-967`) — matches
  production admission.

Identifier canonicalization in composite signals uses
`ownership.NormalizeIdentifier` for foreign printed identifiers
(`composite.go:421-423`) — same as equivalence classes (`candidates.go:443-458`).

### The empty-arm trap and anti-empty reporting

**Failure mode (plan):** run composite arm over default `Load` → zero secondary
documents → zero pools → headline “0 wrong binds” looks like safety.

**Defences already in tree (scoping requirement: never regress these):**

1. **`ArmResult.Representative`** — false when `Pools < Eligible`; empty cell when
   `eligible > 0` fails representative check (`candidates.go:1153-1155`).
   Label: `NOT REPRESENTATIVE (0 of N eligible)` (`candidates.go:1567-1571`).

2. **Supplied-only arms with no pools** — emit explicit empty `ArmResult` with
   `Eligible: eligCount`, not silence (`candidates.go:361-368`).

3. **Wrong-bind banner caveat** — `none in this run — read the per-cell table…`
   (`candidates.go:1466-1467`).

4. **`CompositeSummary.Render`** — `no confirmed composite: this arm measures NOTHING`
   (`composite.go:1293-1295`); prevalence upper bound **UNAVAILABLE** until audit
   reviewed (`composite.go:1247-1253`).

5. **stderr on first proposal write** — composite arm measures nothing yet
   (`cmd/identity-corpus/main.go:221-222`).

**Scoping additions to treat as contract:**

| arm state | report must say |
|---|---|
| composite requested, default loader (bug) | `NOT REPRESENTATIVE`; `Pools=0`; composite section “measures NOTHING” |
| composite + all-attachments, 0 reviewed labels | same; distinguish **unlabelled proposals** vs **missing attachments** |
| composite + labels, 0 DOI-less confirmed | count `ConfirmedWithFrontMatterDOI` (`composite.go:916-923`) — real but **out of selector population** |
| per-axis cell `eligible=293`, `pools=0` | already `NOT REPRESENTATIVE` (first run: `same-venue-year`, `title-superset`) |

Skip accounting: in default load, `"duplicate attachment"` skips (`cmd/identity-corpus/main.go:74-80`) are the **suppressed population** the composite arm needs — report should cross-reference skip count vs `AllAttachments` document delta when composite arm runs.

---

## Hole 3 — composite prevalence recall (human step)

### What automation already does

`ProposeComposites` (`composite.go:556-636`):

- Writes proposals from signals (`foreign-identifier`, `text-correction-marker`,
  `secondary-attachment`, etc. — `composite.go:106-116`).
- Draws **`audit_sample`**: default **25** documents the proposer did **not** flag
  (`composite.go:214-216`, `654-685`; seed from `CompositeOptions.Seed`).

`CompositeSummary` prevalence interval (`composite.go:925-931`, `1036-1051`):

- **Lower bound:** `confirmed / documents_scored` (proposer recall ≤ true rate).
- **Upper bound:** available only when **`AuditReviewed > 0`**, combining
  unlabelled proposals + `AuditMissUpper * notFlagged` (`composite.go:1040-1050`).

Until audit rows are reviewed, Render prints upper bound **UNAVAILABLE**
(`composite.go:1251-1252`) — “composites are rare” is **unfalsifiable**.

### Minimal human labelling protocol (cheapest defensible bound)

**Artifact:** single JSON review file (`-composite-labels`), schema
`CompositeReview` (`composite.go:179-202`). Human edits only:

```json
"reviewed": true,
"class": "<composite-class>|not-composite",
"refers_to": ["<attachmentKey>", ...],
"note": "optional"
```

**Two queues in one file:**

| queue | rows | purpose |
|---|---|---|
| `proposals` | 14 (current run) | confirm/refute proposer |
| `audit_sample` | **25** (existing default) | bound misses among unflagged |

**Per-row decision (one question, two allowed answers + class pick):**

> Is this PDF **about another work** rather than being that work — such that
> autonomous candidate binding should **bind nothing** even if the referred paper
> is a pending job?

- **`not-composite`** — reviewed rejection (ordinary paper, duplicate scan mis-filed as supplement, citation in body only, etc.).
- **Composite class** — pick closest from `compositeClasses` (`composite.go:78-82`); `composite` generic allowed when reviewer will not subtype (`composite.go:57-60`).

**Do not require** for minimal bound:

- Full referent linkage on every row (improves pool adversariality but not prevalence interval).
- Page-by-page annotation or marker transcription (proposer signals already listed).

**Do require** for pool-building (can be second pass on confirmed subset only):

- `refers_to` keys for DOI-less composites where foreign-identifier or title-quote fired — else pool may lack referred job (`composite.go:1123-1140`).

**Presentation (minimize operator time):**

1. Sort proposals by signal strength: `text-correction-marker` / `foreign-identifier` first (`composite.go:525-554`).
2. For each row show: attachment key, `secondary_attachment` flag, DOI-less flag, title (from review entry), **signal list only** — not full excerpt (privacy: `candidate-binding-measurement.md:333-334`).
3. Audit queue: same card; explicitly label “proposer did **not** flag this”.
4. Batch goal: **label all 25 audit rows first** (opens upper bound), then proposals (opens composite arm pools).

**Recall / prevalence math (document is unit for prevalence; audit is Bernoulli on unflagged sample):**

- After `a` audit rows reviewed, `c` composites found among them:
  - Miss rate among unflagged ≤ `AuditMissUpper = binomialUpper95(c, a)` (`composite.go:1037-1038`, `candidates.go:1329-1363`).
- Upper prevalence bound (`composite.go:1044-1050`):

  `upper = min(1, (confirmed + unlabelled + AuditMissUpper × (documents − proposed)) / documents)`

- **Defensible with `a = 25`:** exact one-sided 95% on miss rate; if `c = 0`, upper on miss rate is **≈ 11.3%** (rule-of-three on 25), not “zero composites in library”.
- **Not claimed:** per-document independence across proposals (proposals are enriched, not random); global prevalence remains an **interval**, not a point estimate.

**Existing 25 audit rows:** do not shrink without updating `defaultAuditSample` (`composite.go:215`) and documenting loss of power; if library grows, merge preserves labels (`MergeCompositeReview`, `composite.go:808-851`).

---

## Cross-hole sequencing (recommended)

1. Enable all-attachments load for composite path (already wired when `-composite-labels` set).
2. Operator labels **25 audit + 14 proposals** → upper prevalence bound + composite pools.
3. Implement **marker-gate synthesis arm** → non-zero trials on gates 1b.
4. Re-run `-candidates` with fixed seed `20260816` (or recorded seed) for comparability.

## Out of scope (explicit)

- Changes to `internal/pdf/candidate_select.go` or `identity.go` (measure first).
- CI wiring for `identity-corpus`.
- Pool-cap tuning (refuted by conjunction arm on 2026-08-16 run).

## Strongest contrary evidence (for reviewers)

- **Marker synthesis is not prevalence:** it proves the rule **stops at gate 1b** on constructed bytes; it does not prove the library contains errata at any rate — hole 3 still needs human audit.
- **All-attachments inflates `unestablished`:** secondaries without composite labels are excluded (`candidates.go:411-418`), which can **reduce** eligible count for synthesized arms vs first run’s 293 — not a regression, but easy to misread.
- **Some real composites print front-matter DOI:** they are counted in `ConfirmedWithFrontMatterDOI` but **never enter** DOI-less candidate measurement (`composite.go:916-923`) — correct omission, but lowers composite-arm trial count vs naive expectation.
