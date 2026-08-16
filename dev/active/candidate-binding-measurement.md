# Measuring candidate binding before rebuilding it

Status: plan (2026-08-16). Workstream 4 of the acquisition roadmap. Must
complete and report before `candidate_auto_bind/2` is designed, per the
disablement recorded in ADR-0020 and `dev/active/send-pdf-candidate-binding.md`.

## Why this comes before the redesign

`candidate_auto_bind/1` was withdrawn (commit `0c85a52`) because it read a
*cited* identifier as the document identifying *itself*. Four review rounds
examined its gates individually and missed the composition. The reason they
could miss it is the instrument: the gate corpus
(`internal/pdf/testdata/candidatecorpus/manifest.json`, 36 cases) is
hand-authored, and its hard negatives supplied the ingredients of the failure
*separately* — a foreign DOI with unrelated title and authors, a
conference/journal pair separated by year — and never assembled them into one
relational block.

So the corpus could not fail. Building `/2` against the same instrument would
reproduce exactly that, one rule generation later. The rule of three also puts
a floor on what the synthetic corpus can ever claim: ten predicate-reaching
cases at zero errors bounds the error rate at roughly 30%, which is not a
safety argument.

`cmd/identity-corpus` already exists and already earned its keep — it found the
printed-title weakness that no reviewer had (52 wrong accepts, all through one
gate), and it killed a plausible-sounding trailing-superscript tolerance that
measured as a no-op. This plan extends that instrument to the decision
auto-binding actually makes, rather than inventing a second one.

## What the instrument measures today, and the three gaps

`identitycorpus.Measure` scores `pdf.MatchIdentity` pairwise over ~632
documents from a real Zotero library: each document against its own curated
metadata (must `pass`) and against every other document's (must never `pass`),
reported as `Correct`/`Mismatch` × `Pass`/`Review`/`Reject` with a wrong-accept
list. Two surviving wrong accepts are documented and understood.

Auto-binding makes a different decision in three ways this cannot see.

1. **It is 1-of-N, not 1-of-1.** The daemon holds a pool of jobs awaiting a
   manual download (`job.ListCandidateEligibleJobsTx`) and must pick at most
   one. Pairwise scoring measures a predicate; it does not measure a
   *selection*. Joint false-accept probability grows with pool size, and
   nothing today reports how it grows.
2. **It must abstain when the true target is absent.** The dominant real case
   is a PDF whose paper is not in the pending pool at all — a supplement, a
   reference someone sent you, a paper you grabbed before requesting it. The
   pairwise corpus has no notion of a pool without a right answer, so the
   behaviour that matters most for an autonomous rule is unmeasured.
3. **It scores the wrong predicate.** Auto-bind ran
   `pdf.QualifyCandidate` (`internal/pdf/candidate_select.go`), a five-gate
   rule documented as stricter than `MatchIdentity`. That claim was wrong in
   the one direction that mattered — an erratum printing the target's title,
   authors, year *and its own DOI* passed all five — and `QualifyCandidate` has
   never been scored against a real library at all.

Two further gaps are about corpus content rather than shape:

4. **The composite class does not exist in the corpus.** Errata, supplements,
   comments, retraction notices, cover sheets and journal expansions of
   conference papers are the documents that defeat the rule, and they are real
   library contents, not synthetic constructions. None are labelled today.
5. **The operator's own backlog has never been replayed.** ~27 rows awaiting
   manual download, plus historical grab metadata, are the only sample drawn
   from the actual distribution the rule would run against.

## Deliverables

### 1. A candidate-set measurement mode

`identitycorpus.MeasureCandidateSets(docs, opts) CandidateReport` beside the
existing `Measure`, scoring the real selection path — `QualifyCandidate` and
whatever `/2` replaces it with — over pools synthesized from the library.

Four outcomes, not two, because a selection can fail in two directions and
succeed in two:

| outcome | meaning |
|---|---|
| `correct-bind` | target present, chosen, and it is the right paper |
| **`wrong-bind`** | a paper was chosen and it is the wrong one — the cardinal failure |
| `correct-abstain` | nothing chosen when nothing should be (no unique qualifier, or target absent) |
| `missed-bind` | target present and uniquely correct, but nothing chosen — the cost side |

`wrong-bind` is the number read first, and the only one whose increase kills an
increment outright, mirroring the existing wrong-accept discipline.

### 2. Pool construction with a declared shape

A uniformly random pool understates collisions, which is exactly how the
synthetic corpus flattered the rule. Pools must be built deliberately:

- **Size sweep** — N ∈ {1, 2, 5, 10, 25}. Report each N separately; a rule
  that is safe at N=2 and unsafe at N=25 is a rule with a pool cap, not a safe
  rule.
- **Target-absent arm** — for every document, a pool of N distractors *not*
  containing its own metadata. Any bind here is a `wrong-bind`. This arm alone
  is the measurement `/1` never had.
- **Adversarial arms**, each reported separately, drawn by the axis the gates
  actually read: same first author; same venue and year; title-superset and
  title-prefix pairs; same year with different authors. These are the axes the
  round-3 guards were written against, so they are the axes that must be
  measured rather than asserted.

### 3. A labelled composite class from the real library

Find and label the documents whose whole difficulty is that they *refer* to
another work: errata, corrigenda, retraction notices, comments/replies,
supplements, cover sheets, and journal expansions of conference papers. Zotero
carries enough signal to propose candidates (item type, title markers, short
page ranges, `relatedItem` links); the labelling itself is a human pass over
the proposals, because the label is the ground truth and guessing it would
recreate the original mistake at the corpus level.

Then score them as their own arm. The five synthetic blockers held in
`candidatecorpus` are stand-ins for this class; this arm is whether the class
is rare or routine in a real library, which nobody currently knows.

### 4. Backlog replay

Score the operator's real pending pool and historical grabs: locally extracted
PDFs plus the `manual_download` rows and terminal grab records. This is the
only arm drawn from the true distribution — pool sizes as they actually occur,
target-absent frequency as it actually occurs.

Contents never leave the machine, same handling as the existing report
(`dev/identity-corpus.md`'s Privacy section applies unchanged, including
stderr).

### 5. Report, runbook, and an honest bound

- Extend the rendered report with the four outcomes per arm and per N.
- Print the **rule-of-three upper bound** beside every zero: with no wrong-bind
  in K trials the rate is bounded at about 3/K, and the report should say so
  rather than letting "zero wrong-binds" imply safety at any N.
- Add a `dev/identity-corpus.md` section for the candidate-set workflow, with
  the same before/after discipline and one-increment-at-a-time rule that
  section already establishes for `identity.go`.

## What this measurement is expected to decide

Not "is the rule good" but four specific numbers `/2` needs and does not have:

1. The wrong-bind rate per pool size, which decides whether an autonomous rule
   needs a pool cap and what it is.
2. The target-absent abstention rate, which decides whether autonomous binding
   is viable at all — a rule that binds *something* when the right answer is
   absent cannot ship at any pool size.
3. The composite-class frequency, which decides whether the front-matter
   structural parser is the whole fix or merely the first one.
4. The missed-bind rate, which is the user-visible cost of abstention and the
   only argument for the feature existing.

## Boundaries

- **No production behaviour changes in this workstream.** Auto-binding stays
  disabled; the popup picker and the conclusive-identity veto stay exactly as
  shipped. This is instrumentation.
- **No `internal/pdf/identity.go` rule changes.** Measure first. Any rule
  change is workstream 3 and is gated on this report.
- The existing pairwise `Measure` keeps working unchanged; its two documented
  wrong accepts are the baseline, not a regression to fix here.
- `cmd/identity-corpus` stays a local operator tool. It is not wired into CI:
  it reads a personal library, and its output is the operator's reading list.

## The convergence worth noting

`dev/identity-corpus.md` already records why its last two wrong accepts are
hard: both print the requested title as a genuinely delimited line — one inside
a contents list, one as a section heading — so separating them "needs a signal
this rule doesn't have: structural position ... knowing where the contents list
or heading structure is, not just how a phrase is delimited."

That is independently the same conclusion the oracle review reached about
`/1`, and the same fix `/2` specifies: a parsed front-matter assertion with one
title span, a contiguous byline, an explicit byline end, a year locus and a
self-identifier locus. Two instruments, arrived at separately, naming one
missing capability. That is the strongest evidence available that `/2` is
aimed at the right thing — and a reason to build the structural parser once,
scored by both.
