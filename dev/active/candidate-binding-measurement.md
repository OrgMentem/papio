# Measuring candidate binding before rebuilding it

Status: plan v2 (2026-08-16). Workstream 4 of the acquisition roadmap, gating
workstream 3. Revised after three parallel plan reviews (blind-spot,
plan-versus-source, statistics/method), all three returning NEEDS REVISION on
v1. Every correction below is anchored to source; v1's overstated claims are
corrected in place rather than quietly dropped, because the overstatements were
the reviewable part.

## Why the instrument comes before the rule

`candidate_auto_bind/1` was withdrawn (commit `0c85a52`) after a pro-tier
review found five deterministic wrong-accept paths in papio's cardinal failure
class: the wrong paper filed under a right citation. Its root error was reading
a *cited* identifier as the document identifying *itself* — an erratum,
supplement or journal expansion printing "Extended from DOI X", with its own
DOI past the 1 KiB window the safety veto reads.

The synthetic gate corpus reported zero wrong-accepts at the time because its
hard negatives supplied the ingredients of that failure separately and never
composed them into one document. Four review rounds examined gates
individually and missed the composition.

What this justifies is narrow and worth stating precisely, because v1 of this
plan overclaimed it. The corpus was not incapable of expressing the failure in
principle; it did not contain it. Since the withdrawal it does: the five
composite documents are held as `known_failing` cases
(`internal/pdf/testdata/candidatecorpus/manifest.json`, cases at lines
1386-1603), and the corpus grew 33 → 38 cases. It also already contains
`true-absent` and `true-absent-no-doi` cases (manifest lines 389-423, 895-929),
so "the target-absent case was never measured" is **false** of the synthetic
corpus and true only of the real-library instrument.

The honest argument is therefore about *population*, not expressiveness:

- The synthetic corpus is hand-authored, so it measures the cases someone
  thought of. The rule of three bounds what it can ever claim — ten
  predicate-reaching cases at zero errors bounds the error rate near 30%.
- The real-library instrument (`internal/identitycorpus`) measures a real
  population but scores the wrong decision: `pdf.MatchIdentity` **pairwise**,
  one document against one metadata record.
- Nothing measures `pdf.QualifyCandidate` or `pdf.SelectAutoBindCandidate`
  against a real population at all. `QualifyCandidate` does have synthetic gate
  tests (`internal/pdf/candidate_gate_test.go:155-356`), so v1's flat "never
  scored" was an overstatement; "never scored against a real library" is the
  claim that holds.

## The admission condition, which defines the corpus

This is the correction that reshapes the plan. In production,
`SelectAutoBindCandidate` is reached **only** from `processSettledGrab`'s
`if len(dois) == 0` branch (`internal/browser/bridge.go:7565-7592`), where
`dois` comes from `FrontMatterDOIs` over the same 1 KiB window
`CheckConclusiveIdentity` uses. A document with a DOI in its front matter never
enters candidate selection.

So feeding every library document into the measurement would measure a
population production never sees, and `QualifyCandidate` would short-circuit at
`GateConclusiveVeto` (`candidate_select.go:139-144`) for every document
carrying a front-matter DOI. What share of the library that is has never been
published — each run emits an own-identifier histogram, not a front-matter
bucket count — so the proportion is a measurement result of this workstream,
not a premise of it. The synthetic corpus already enforces the same admission:
`candidate_gate_test.go:265-285` requires DOI-less inputs for the predicate
gates.

**Therefore: the measured corpus is the DOI-less subset** — documents whose
`FrontMatterDOIs` over the production window is empty. Reporting the size of
that subset is itself a primary finding, because it bounds everything the
instrument can claim, and nobody currently knows it. A library where 40 of 632
documents are DOI-less supports very different conclusions than one where 400
are.

Every trial must also record its **observed terminal gate**, so a cell that
looks clean because nothing reached the gate under test is visibly distinct
from one that reached it and passed. The gate rule has **seven**
`CandidateGate` constants (`candidate_select.go:71-78`), not five as v1 said;
non-article and correction markers are gates in their own right.

## What the existing instrument measures, and the real gaps

`identitycorpus.Measure` scores `MatchIdentity` over ~632 documents from the
operator's Zotero library, each against its own metadata (must pass) and
against every other document's *except* pairs it skips as the same work or the
same document (`measure.go:247-249` — v1's "every other document's" was
imprecise). Two surviving wrong accepts are documented and understood.

Four gaps, each now stated as a decision the instrument cannot reach rather
than a capability it lacks:

1. **It measures a predicate, not a selection.** Production picks at most one
   from a pool via `SelectAutoBindCandidate`. Pairwise scoring cannot express
   how false-accept grows with pool size.
2. **It has no target-absent semantics.** The everyday case — a PDF whose paper
   is not pending at all — has no pairwise analogue.
3. **It scores a different predicate** than production's qualification gates.
   There are **seven** observable gates (`candidate_select.go:71-78`), not the
   five numbered ones: the non-article and correction-marker gates are
   evaluated on DOI-less input and can terminate the traversal
   (`candidate_select.go:180-190`), so marker-gate coverage is required and a
   report that tracks only five would silently omit it.
4. **The composite class is invisible to its loader.** `Load`'s
   `dedupOnePerParent` (`corpus.go:165`, `555-584`) keeps one PDF per
   bibliographic parent and explicitly drops secondary attachments including
   supplements — exactly the class that defeats the rule. The composite arm is
   unbuildable without an all-attachments mode; v1 did not notice this and
   would have produced an empty arm reading as a clean one.

## Deliverables

### 1. Candidate-set measurement over the DOI-less corpus

`MeasureCandidateSets(docs, opts) CandidateReport` beside `Measure`, scoring
`SelectAutoBindCandidate` over pools built from the library, restricted to the
DOI-less subset and recording each trial's observed terminal gate.

Four outcomes:

| outcome | meaning |
|---|---|
| `correct-bind` | chose a candidate in the target's equivalence class |
| **`wrong-bind`** | chose a candidate outside it — the cardinal failure |
| `correct-abstain` | chose nothing when nothing should be chosen |
| `missed-bind` | target present and uniquely right, chose nothing |

**Pool sizes start at N=2.** N=1 cannot measure a 1-of-N selection and the
synthetic gate corpus already rejects pools below 2
(`candidate_gate_test.go:192-195`). Sweep N ∈ {2, 5, 10, 25}.

### 2. Ground truth as equivalence classes, not identity

v1 set `TrueKey` to the document's own metadata row. Three reviewers
independently rejected that, and they are right: a library holds a preprint and
its version of record, duplicate rows from re-imports, and occasionally wrong
Zotero metadata. Under v1's rule, binding a *same-work* candidate carrying
different metadata would score as a `wrong-bind` — manufacturing the very
failure the instrument exists to count.

So ground truth is an **equivalence class** of candidate keys per document, and
a bind inside the class is correct. Building it:

- Canonicalize strong identifiers with `ownership.NormalizeIdentifier`
  (`internal/ownership/ownership.go:374-415`), the **version-collapsing**
  relation, because "is this the same work?" is exactly the question ADR-0008
  gives that normalizer. Do **not** use `work.Normalize*`, which is
  version-preserving for acquisition.
- **Do not reuse `sameWork`** (`measure.go:119-133`) as the distractor guard.
  It is wrong in both directions: it compares raw exact DOI/arXiv/title and
  exact PMID with no normalizer, so it misses `doi.org` URL versus bare DOI,
  arXiv `v1`/`v2`, and PMID leading zeros; and its identical-title fallback
  would **suppress legitimate distractors** — manifest case06 (lines 217-260)
  is a same-title, same-author, different-DOI/year/container pair, which is a
  genuinely different work and one of the most valuable distractors available.
  Canonicalize identifiers; be conservative with any title fallback.
- Preprint/VoR pairs are declared **same class** and must be enumerated rather
  than inferred, since that is the case most likely to be silently wrong.
- Every class carries **recorded provenance** — which candidate keys are in it
  and on what basis (canonical identifier match, or human adjudication of a
  named pair). Truth inferred from `Document.Work` alone is not admissible,
  because `Work` is the Zotero parent's record (`corpus.go:42-51`) and a
  mis-curated record or a preprint/VoR attachment mismatch would make a wrong
  bind read as `correct-bind`.
- Where the class cannot be established, the trial is **excluded and counted as
  unestablished**, never guessed into an arm.

### 3. Pools built deliberately, including one conjunction arm

Per-axis arms — `same-author`, `same-venue-year`, `title-superset`,
`same-year`, `random` — plus a `target-absent` form of each.

But per-axis arms alone reproduce v1's methodological error one level up:
each varies a single axis, so no pool ever contains the *composed* adversary
that withdrew the rule. So there is an explicit **conjunction arm**: a pool
containing a distractor that simultaneously carries the target's title,
authors and year, cites the target's DOI in body text, and prints its own
different DOI past the blind window — in both target-present and target-absent
forms. This arm is the direct reproduction of the withdrawn failure and is the
arm whose result matters most.

### 4. Real composites, with an honest recall bound

Requires an all-attachments loader mode (see gap 4). Signals propose; a human
confirms, because the label is ground truth. An unreviewed proposal is reported
unlabelled and counted as neither class.

Proposer recall bounds the measured prevalence, so prevalence from proposals
alone is a lower bound and must be labelled as one. A **random-sample audit** of
documents the proposer did *not* flag is required to bound recall; without it
"composites are rare" is unfalsifiable.

For a confirmed composite the correct behaviour is to bind **nothing**, so its
pool carries an empty target class even when the work it refers to is present.

### 5. Backlog replay — descriptive coverage, not calibration

v1 called this "the true distribution". It cannot be. `Grab` persists id,
title, state, quarantine, job and outcome (`internal/grab/grab.go:64-82`), and
no selection-time pool snapshot for historical or manually delivered rows,
while `attemptAutoBind` enumerates live eligibility at selection
(`bridge.go:7635-7652`). Automatic-bind provenance does persist partial
evidence — `Candidates []CandidateVerdict` plus `ExcerptSHA256`
(`grab.go:421-454`, `bridge.go:7768-7794`) — but that is per-candidate verdicts
and keys for binds that already ran, not the pool as it stood for the rows this
arm would replay. The pool that existed when a historical grab settled is
therefore unrecoverable, and a present-day snapshot of pending rows is not a
time-weighted distribution.

So: the backlog arm is **descriptive stress coverage** and explicitly may not
be used alone to choose a production pool cap. Making it calibration-grade
requires event-time pool snapshots recorded going forward — a small, separable
change worth doing early so the data exists later, but not a prerequisite here.

Use `Store.ListCandidateEligibleJobs` (`internal/job/candidate_eligibility.go:169-194`)
for a standalone read, matching the daemon's own initial pool
(`bridge.go:7635`); the `...Tx` form (lines 197-211) is the daemon's freshness
fence, not the enumeration. The pending-row count v1 quoted as "~27" is an
operator-run figure with no repository evidence and must be reported from a
query, not asserted.

### 6. Statistics stated correctly

- **The sampling unit is the document**, not the trial. One document reused
  across arms and sizes contributes many correlated observations, so a
  per-trial denominator flatters the rate. If six arms and five sizes all fill,
  `3/18,960 ≈ 0.016%` is roughly **30×** more optimistic than the
  per-document-cluster bound `3/632 ≈ 0.47%`.
- **Replace `3/K` with a cluster-aware bound** — a per-document one-sided
  interval or cluster bootstrap — or report the raw count with an explicit
  non-independence caveat. Printing `3/K` as a 95% bound over correlated trials
  is simply the wrong statistic.
- **Declare a denominator per reported quantity**: per-document safety (was
  this document ever misbound, at each arm and N); per-pool operational
  wrong-bind rate (wrong decisions over evaluated pools at that N);
  target-absent abstention (correct-abstain over target-absent pools);
  missed-bind (over unique target-present documents); composite prevalence
  (labelled composites over all scored documents, never over replicated
  trials). Never pool arms or sizes into one headline rate.
- **Flag underfilled cells.** Report evaluated-versus-eligible counts per cell
  and mark any cell that thinned as nonrepresentative — `same-venue-year` at
  N=25 may survive only for one heavily-represented journal, which measures
  that journal rather than the axis.

### 7. Report, runbook, and thresholds

Extend the rendered report with the four outcomes per arm and N, unique-document
counts, terminal-gate distribution, and the cluster-aware bound. Add a
`dev/identity-corpus.md` section in that runbook's voice, with its before/after
and one-increment-at-a-time discipline, wrong-binds read first, and its Privacy
section applying unchanged.

**Predeclare the measurement thresholds**, because v1 called these outputs
"decisions" while specifying no comparison that decides anything:

- pool cap = the largest N whose cluster-adjusted wrong-bind upper bound sits
  under a predeclared risk budget;
- viability needs a maximum target-absent wrong-bind rate, not an abstention
  rate alone;
- parser sufficiency needs conditional composite wrong- and missed-bind rates
  plus a coverage criterion, not prevalence alone;
- missed-bind needs an acceptable human-workload budget.

A numeric release bar for `/2` stays deferred — that is this workstream's
scope — but the measurement's own estimands and risk bars must be fixed before
data is collected, or the thresholds get chosen after seeing the numbers.

## Scope honesty: what this gates, and what it does not

A selector-level measurement cannot see `FrontMatterDOIs` reachability in
production, eligibility-pool construction, the durable bind fence, ownership
arbitration, or concurrency. So this workstream gates **the rule**, not the
feature. Shipping `/2` additionally requires an integration-level gate over
those paths, which v1 elided by claiming the report gates `/2` outright.

## Boundaries

- **No production behaviour changes.** Autonomous binding stays disabled behind
  the unexported `autoBindDecisionEnabled`, initialized false and set true only
  by tests (`bridge.go:7618-7625`, `grab_autobind_test.go:185-189`); the popup
  picker and the conclusive-identity veto stay exactly as shipped. Note the
  in-tree substrate is already labelled `/2` (`CandidateBindingRule`,
  `candidate_select.go:47`): `/1` is historical provenance and a doctor check,
  not a currently reachable function. So what this instrument measures is the
  `/2` substrate as it stands today, which is the baseline any `/2` rule change
  must then be compared against.
- **No changes to `internal/pdf/identity.go` or `candidate_select.go`.** Measure
  first; rule changes are workstream 3 and are gated on this report.
- Align the instrument's extraction with production or report the divergence:
  the corpus loader uses `DefaultSemanticOptions` (MinChars 1000, OCR 3) while
  the daemon's configured defaults are MinChars 400, OCR 4 with OCR enabled, and
  `Document.Text` is an excerpt bounded by `MaxExcerpt` = 16 KiB
  (`semantic.go:34-36`, sliced at lines 188-193). State the detectable range
  rather than assuming it: a DOI printed past the 1 KiB blind window but inside
  page one's 4 KiB cap **is** observable — composite case25 is built exactly
  there — while `identityWindow` stops at the first form feed and caps page one
  at 4 KiB (`identity.go:819-869`), so an identifier on page two, or anywhere
  past 16 KiB, is invisible to the instrument and to the candidate-binding
  page-one rules. Scope that claim carefully: it does **not** hold for
  `MatchIdentity`, which calls `corroboratingIdentifier` over the whole excerpt
  (`identity.go:287`), so a page-two DOI can still corroborate in the pairwise
  rule up to the 16 KiB bound.
- The existing pairwise `Measure` keeps working unchanged; its two documented
  wrong accepts are the baseline.
- `cmd/identity-corpus` stays a local operator tool, never wired into CI: it
  reads a personal library and its output names the operator's own papers.

## The convergence worth noting

`dev/identity-corpus.md` records that its two surviving wrong accepts both print
the requested title as a genuinely delimited line — one inside a contents list,
one as a section heading — and that separating them "needs a signal this rule
doesn't have: structural position ... knowing where the contents list or heading
structure is, not just how a phrase is delimited."

That is independently the same conclusion the oracle review reached about `/1`,
and the same parsed front-matter assertion `/2` specifies. Two instruments
arrived at separately, naming one missing capability — the strongest available
evidence that `/2` aims at the right thing, and a reason to build that parser
once and let both modes grade it.
