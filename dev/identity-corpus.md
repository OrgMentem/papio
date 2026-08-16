# identity-corpus — measuring `internal/pdf/identity.go` and `candidate_select.go`

`internal/pdf/identity.go`'s window sizes and thresholds (the 1 KiB DOI
window, the 2 KiB byline window, the 4 KiB page-one window, the 60%
title-token share, the family-name-only author check) each cite a
measurement in a doc comment: 40 papers, 1560 deliberately mismatched
document/metadata pairs from one real library, 155 wrong accepts (9.9%)
before the current rules, none after. That measurement was never saved as
code. Nobody touching those rules since has been able to tell whether a
change made matching better or worse — only whether the existing tests
still passed. `cmd/identity-corpus` reproduces the measurement so it
becomes a rerunnable check instead of a one-time claim.

It reads every PDF attachment of a scholarly item out of a Zotero library
together with its parent item's curated metadata (`work.Work`), extracts
text the same way `internal/pdf` does, then runs `pdf.MatchIdentity` twice
per document: once against its own metadata (must return `pass`) and once
against every *other* document's metadata (must never return `pass`). It
also reports where in each document's text its own identifier actually
printed, against the three windows above — that offset distribution is
what the window bounds are tuned against.

## Why Zotero, not a papio store

A papio store only holds what papio already fetched successfully, and its
metadata is what papio *requested*, not what a human curated — co-author
lists there are frequently truncated to one name, which is exactly the
axis the author-matching rules are judged on. Measuring the rules against
a papio store would grade them on the sample they already pass. A Zotero
library is bigger (roughly 789 PDF attachments against 172 in a typical
papio store) and has complete creator lists, because a person put them
there by hand. That's the corpus this tool reads.

## What the corpus leaves out

The corpus is not "the whole library" — some of what Zotero holds is
excluded, on purpose or as a side effect, and the exclusions are not
symmetric across item types. What's actually in a given run is always in
that run's own skip summary (`Report.SkipsByReason`, printed as its own
section); the numbers below are this tool's reference-library run, not a
universal constant, and are here to explain the *shape* of the bias, not
to stand in for reading your own report.

- **Attachments that resolve into papio's own data directory are excluded
  by design.** The whole point of measuring against Zotero instead of a
  papio store (above) is independence from papio's own, already-scored
  output. A linked Zotero attachment that happens to point at
  `~/.local/share/papio/{artifacts,bundles,zotio/staging}` — because the
  operator linked papio's delivered file back into their library instead
  of re-downloading it — would silently reintroduce that dependency, so
  Load skips it under the `"papio-owned artifact"` reason class rather
  than scoring it. **This resolves papio's DEFAULT data directory, not
  whatever `data_dir` your `config.toml` actually sets.** `papioDataDir`
  deliberately reads `config.Default().DataDir` rather than your live
  config, so if you have relocated `data_dir`, the exclusion is checking
  the wrong tree: papio's already-scored output under your real data
  directory re-enters the corpus as if it were independent evidence,
  quietly inflating the pass rate the same way measuring against a papio
  store would. Confirm where your `data_dir` actually points before
  trusting a "papio-owned artifact" count of zero.
- **`attachments:`-relative linked files need a Zotero preference the
  corpus can't see.** A `linkMode=2` (linked_file) attachment stored as
  `attachments:some/relative/path` resolves, in Zotero itself, against
  whatever the operator has set for
  *Preferences → Advanced → Files and Folders → Linked Attachment Base
  Directory* (`extensions.zotero.baseAttachmentPath`). When that
  preference is unset — the common case — there is no base directory to
  resolve against, and Load skips the attachment with a named reason
  (folded into the `"file missing"` class in the summary) instead of
  guessing at a path. An absolute linked path needs no such preference and
  is unaffected.
- **An attachment key outside Zotero's own shape is refused, not
  trusted.** A Zotero item key is always 8 characters, uppercase letters
  and digits. Every row Load reads comes over sync from whoever added the
  item — not necessarily this operator, on a shared library — and the key
  composes both the storage path and the cache filename, so one outside
  that shape is skipped (also folded into the `"file missing"` class)
  before it reaches either.
- **The 1 MiB `pdftotext` output cap drops long documents unevenly.** A
  document whose extracted text would exceed the cap is skipped under the
  `"output cap"` class rather than truncated silently mid-window. In the
  reference library that hits books far harder than articles — 24 of 54
  `book` candidates against 11 of 637 `journalArticle` candidates — so a
  corpus with that class non-empty is closer to "the library, minus most
  of its books" than to the library itself. Books also have different
  front-matter conventions (often no DOI, editor-as-author bylines), so
  this isn't just a smaller sample, it's a sample biased against exactly
  the kind of item those rules are weakest on. Treat any conclusion about
  a book-shaped rule change as untested until the `"output cap"` count is
  at or near zero for the library it was tested on.

## Running it

```
go run ./cmd/identity-corpus
```

Flags:

| Flag | Default | Meaning |
|---|---|---|
| `-zotero` | `~/Zotero` | Zotero data directory (containing `zotero.sqlite` and `storage/`) |
| `-cache` | `<user cache dir>/papio/identity-corpus` | extracted-text cache dir; `-cache ""` disables caching |
| `-workers` | `0` (= `runtime.NumCPU()`) | extraction concurrency |
| `-json` | off | emit the `Report` as indented JSON instead of the text report |
| `-candidates` | off | measure `pdf.SelectAutoBindCandidate` over candidate pools, DOI-less subset only, instead of the pairwise report |
| `-seed` | `20260816` | fixed seed every candidate pool is drawn from; the report records it |
| `-pool-sizes` | `2,5,10,25` | pool sizes to sweep; anything below 2 is refused by name, not dropped |
| `-arms` | `""` (= every arm) | comma-separated pool-construction arms |
| `-composite-labels` | `""` (arm off) | human-reviewed composite label file; written with fresh proposals if absent |
| `-true-classes` | `""` | JSON map of document key → adjudicated equivalence class, for pairs that must be enumerated rather than inferred |
| `-papio-data-dir` | papio's default `data_dir` | papio data directory whose store is read read-only to enumerate the backlog arm |

The last seven apply only under `-candidates`, and passing one without it is
an error rather than a silently ignored flag: the run would produce the
pairwise report, and a captured file would then look like it had measured
pools at a seed it never used. See
[the candidate-set mode](#the-candidate-set-mode-a-selection-not-a-predicate)
for what they mean.

`<user cache dir>` is `os.UserCacheDir()` — `~/Library/Caches` on macOS,
`$XDG_CACHE_HOME` or `~/.cache` on Linux, `%LocalAppData%` on Windows —
which is per-user by construction. It falls back to
`$TMPDIR/papio-identity-corpus` only if `os.UserCacheDir()` itself can't
resolve a directory; see [Privacy](#privacy) for why the default isn't
shared temp.

Needs `pdftotext` (poppler) on `PATH` — the same extraction dependency the
daemon itself needs (`brew install poppler`, see the README).

It opens `zotero.sqlite` through a temporary copy, so it's safe to run
while Zotero is open. Library data is never modified and no network call
is ever made — though on the atomic snapshot path (`VACUUM INTO`), the
read-only connection SQLite uses to read the live database can still
leave the ordinary `zotero.sqlite-wal`/`zotero.sqlite-shm` sidecar files
beside it if they weren't already there, the same as any other read
connection opened against a WAL-mode database would.

The first run extracts text for every PDF attachment, which is slow at
this scale — several minutes for ~789 PDFs — that's the caching `-cache`
dir is for. Every later run against the same library hits the cache and
finishes in seconds, so the before/after workflow below stays fast as long
as you don't change the corpus loading itself between runs.

Pair count is `N×(N−1)`, minus same-work exclusions (duplicate metadata
rows describing one paper, which `sameWork` drops before scoring since
pairing them isn't a real mismatch). In the reference library the corpus
actually *scores* 632 documents — not the 789 raw PDF attachments above,
which include everything Load later skips — so `N×(N−1)` is
632×631 = 398,792 candidate pairs, 398,786 after same-work exclusions.
`N` is `Report.Documents`; for your library it's whatever the corpus-size
line at the top of your own report says, not a fixed constant and not the
raw attachment count. Once text is cached, scoring that many pairs is
seconds of CPU — the extraction, not the matching, is the cost.

## Using it on a change to `internal/pdf/identity.go`

1. On `main` (or whatever you're comparing against), run
   `mkdir -p dev/scratch && go run ./cmd/identity-corpus > dev/scratch/before.txt`
   and keep it. `dev/scratch/` is repo-ignored (see `.gitignore`) —
   capture there, never into the module root or anywhere else git tracks.
2. Make your change to `internal/pdf/identity.go`.
3. Run `go run ./cmd/identity-corpus > dev/scratch/after.txt` again — same
   library, same cache, so extraction is skipped and only the matching
   logic re-runs.
4. Compare **WRONG ACCEPTS first**: the count in `after.txt` must not be
   higher than in `before.txt`. A wrong accept is a mismatched
   document/metadata pair that returned `pass` — the failure mode the whole
   corpus exists to catch, since it's the one that files a PDF under the
   wrong work.
5. Then compare the correct-pair pass rate (own-document-against-own-metadata):
   it must not fall. A document that stops matching its own metadata is a
   new false negative your change introduced.

The absolute counts are **not** comparable across machines or across
different people's runs — a Zotero library is one person's collection, so
its size, its authors, and its title collisions are specific to it. Only
before/after on the **same library** (ideally the same cache) is a
meaningful comparison. Don't read anything into "your machine says 12
wrong accepts and mine says 3" — that's two different libraries, not two
different rule sets.

## Worked example: the printed-title rule

This is the first rule change this harness judged, and the shape every
later change to `internal/pdf/identity.go` should follow.

**The instrument found the weakness, not a reviewer.** A `before.txt`
capture on the reference library showed 52 wrong accepts, and every one of
them came through the same gate: `identityTitleTokens`, which drops
stopwords and every word under five runes, then passes at 60% overlap of
what's left. Three failure families fell out of reading the pairs:

- **Degenerate titles.** "How to do a meta-analysis" reduces to
  `{analysis}` — one token, matching any paper that contains the word.
  Titles this short or shorter accounted for 8 of the 52 wrong accepts.
- **Numbered series.** "Final Report - Volume 3, Impacts" drops the `3`
  (one rune, under the five-rune floor), so a multi-volume government
  report's volumes matched each other.
- **Superset titles.** An unordered token set can't distinguish "Core
  reporting practices in structural equation modeling" from "Update to
  core reporting practices in structural equation modeling" — both score
  5/5 — nor a paper's title from a later paper's title that contains it.

**The candidate rule and each increment, measured in turn.** The fix
requires the requested title to be *printed as a delimited unit* — a line
of its own, allowing for a short label and a hyphenated line break — in
the byline window, on top of (not instead of) the unchanged token gate:

| increment | wrong accepts | correct passes |
|---|---|---|
| before (token overlap only) | 52 | 586 (92.7%) |
| + printed-title required | 3 | 560 |
| + label allowance (≤3 words) | 2 | 559 (88.4%) |
| − label allowance removed | 2 | 557 |
| + character-stream matching, label-terminator narrowing | 2 | 564 (89.2%) |
| + line numbers stepped over inside a wrap | 2 | 565 (89.4%) |

The label allowance (tolerating a short prefix like "Original Article:" or
"1." before the title) stayed: it recovers 2 correct passes — 557 → 559 —
at zero cost in wrong accepts. An increment earns its place by moving one
of these two numbers in the right direction; this one did, so it shipped.

**The increment that didn't ship.** A trailing-superscript tolerance —
loosening the end-of-phrase match to allow a footnote marker glued to the
title's last word — was implemented and measured the same way. It changed
neither number on 632 documents: still 2 wrong accepts, still 559 correct
passes. It was deleted rather than shipped. The rule this leaves standing:
an increment that moves neither number does not ship, on principle,
because the harness exists precisely so a plausible-sounding tolerance has
to earn its place instead of being taken on faith.

**Three more increments that did ship.** Three later changes to the same
gate each moved one of the two numbers, so — unlike the trailing-superscript
tolerance above — all three stayed. Character-stream matching changed
`titleRunMatches` to compare the printed words against the requested title
as one concatenated run of characters instead of word by word: `pdftotext`
doesn't always keep the spaces inside a word, and "PsychologicalSafety and
LearningBehavior in WorkTeams" — byte-identical to its catalogue title
except for three missing spaces — was parked by a word-by-word comparison
that character-stream matching now passes. It also let a footnote digit
welded to the title's last word through ("MODEL OF CHANGE1"), which is the
same tolerance the reverted increment above attempted and failed to measure:
it failed because it looked for the marker as a separate word, and the text
layer had glued it inside one. Label-terminator narrowing changed
`labelTerminators` so only a colon, full stop, rule, or bullet ends the
short label the start edge allows, not any punctuation; a citing sentence
reaching for a quote or a dash to introduce a title no longer qualifies as
a label. Those two moved correct passes from 559 to 564. Stepping over a
digit-only segment inside a wrap took it to 565: a submitted manuscript
numbers every line, so one preprint's title arrived as "…with an initial
structure of", "6", "vertical leadership", and digits alone cannot belong to
a title that has already begun. Wrong accepts held at 2 throughout — the
same standard the label allowance was held to, and the same reason all three
shipped.

**The cost, stated plainly.** 21 correct documents moved from pass to
review, measured against the pre-rule baseline of 586. Every one of them
is accounted for: 8 are numbered-series volumes whose covers print the
catalogue's words in a different order ("FINAL REPORT / Impacts / VOLUME
3" for "Final Report - Volume 3, Impacts"), 5 have a text layer shredded
by column interleaving so no contiguous title exists, 3 are catalogue
errors where the record disagrees with the printed title ("EEF" for EEG,
"Altering" for Altered, and a Nature piece whose record carries a
different title entirely), 2 are records that concatenate a teaser or drop
a publisher prefix, 1 is a mojibake dash, and the remaining 2 are ordinary
subtitle differences. The trade is deliberate: papio parking a correct
paper costs a human a moment of review; papio filing the wrong paper costs
a library its trust.

**The two wrong accepts that survive, and why they're hard.** Both print
the requested title as a genuinely delimited line, so `titlePrintedAsLine`
has no way to tell them from an original: a technical report prints the
requested title inside its own contents list, and a review paper prints it
as one of its section headings. Neither is a label, a wrapped title, or a
citation glued into running text — by every signal this rule reads, they
*are* the title printed as a line. Separating them needs a signal this
rule doesn't have: structural position — knowing that a byline window
sitting inside a table of contents or under a numbered heading is not the
document's own title line, which means knowing where the contents list or
heading structure is, not just how a phrase is delimited.

That missing signal — structural position, i.e. parsed front matter rather
than delimiter shape — is the same capability `candidate_auto_bind/2` is
specified to add (`dev/active/send-pdf-candidate-binding.md`), so one
parsed-front-matter change is expected to be graded by both modes: wrong
accepts here, wrong binds in the candidate-set mode below.

## How to judge a change

1. Capture `before.txt` on an unmodified baseline.
2. Change exactly one increment in `internal/pdf/identity.go` — not a
   bundle of related tweaks; a bundle can't tell you which part earned its
   place.
3. Capture `after.txt` on the same library, same cache.
4. Compare wrong accepts first. Any increase kills the increment,
   regardless of what else it does.
5. Compare correct passes second, and only once wrong accepts are flat or
   lower.
6. Revert the increment if it moves neither number — a change that
   measures as a no-op is a no-op, whatever the code review said.
7. Never quote the absolute counts as if they held on another library —
   they're a property of one person's Zotero collection, not of the rule.

## The candidate-set mode: a selection, not a predicate

```
go run ./cmd/identity-corpus -candidates
```

Everything above measures `pdf.MatchIdentity`, a predicate over one document
and one metadata record that either passes or doesn't. This mode measures
`pdf.SelectAutoBindCandidate` instead: **one choice out of N**, where one of
the available answers is to choose nothing. So the pairwise
pass/review/reject gives way to four outcomes:

| outcome | meaning |
|---|---|
| `correct-bind` | chose a candidate inside the target's equivalence class |
| **`wrong-bind`** | chose one outside it — the cardinal failure, the wrong paper filed under a right citation |
| `correct-abstain` | chose nothing when nothing should have been chosen |
| `missed-bind` | the target was present and uniquely right, and it chose nothing |

**Abstention is an answer, and target-absent is an axis rather than an arm.**
The everyday case — a PDF whose paper is not pending at all — has no pairwise
analogue, because there is no pair to score. Here every arm runs in both a
target-present and a target-absent form, and in the absent form
`correct-abstain` is the only correct outcome. A `wrong-bind` in a
target-absent cell is the most serious number in the report: nothing in the
library was the right answer and it chose something anyway. An abstention
always carries a reason; a blank one would be a defect in the selector, not a
gap in the report.

**Seven gates, and every trial records the one it stopped at.** The rule has
seven observable gates, not the five numbered ones — the non-article and
correction-marker gates are evaluated on DOI-less input and can terminate the
traversal on their own. A cell that looks clean because nothing in it ever
reached the gate under test is a completely different fact from a cell that
reached the gate and passed, and the two are indistinguishable from the
outcome counts alone. That's what the terminal-gate distribution is for: read
it before believing a gate is sound. One of the seven can never appear in it —
every admitted document has an empty front-matter window, so the
conclusive-identity veto always returns absent — and the report says so beside
the zero, because there the zero means untested rather than passed. Treat any
other gate sitting at zero trials the same way until an arm reaches it.

**It runs over the DOI-less subset, and the size of that subset is itself a
headline number.** In production the selector is reached from exactly one
place: the branch of `processSettledGrab` where `FrontMatterDOIs` over the
1 KiB window returned nothing. A document with a DOI in its front matter
never enters candidate selection at all. So the mode admits a document only
when that same window yields no DOI, and reports both counts — the library it
loaded, and the DOI-less subset it admitted. Read that pair first, before any
rate: it bounds everything the run can claim. A library where 40 of 632
documents are DOI-less supports very different conclusions from one where 400
are, and nobody knew the number before this mode existed. Measuring the whole
library instead would measure a population production never sees, and most of
it would short-circuit at the conclusive-identity veto anyway.

### Reading the report: wrong-binds first, per document

The pairwise ordering carries over — wrong-binds before anything else — with
one correction that is easy to get wrong. **The sampling unit is the
document, not the trial.** One document is drawn into many arms at many pool
sizes, so its trials are re-readings of the same PDF rather than independent
samples. A per-trial denominator flatters the rate by roughly the replication
factor: three wrong binds over 18,960 trials reads as 0.016%, while the same
three over 632 documents is 0.47% — about 30× worse, and the honest number.
So each cell prints its denominators separately, because they answer
different questions:

- `WrongDocRate`, the `wrong/doc` column — documents wrong-bound at least
  once, over unique documents. **This is the safety headline.**
- `WrongPoolRate`, the `wrong/pool` column — wrong decisions over evaluated
  pools. This is operational: what share of selections at that N went wrong.
- `WrongDocBound`, the `doc bound` column — a one-sided 95% upper bound on the
  per-document wrong-bind probability, and the only thing that makes a zero
  readable.

**Never read a zero without its bound beside it.** Zero wrong binds over 9
documents and zero over 400 are the same count and entirely different
evidence. A per-trial `3/K` bound over correlated trials is not a
conservative reading of that difference, it is the wrong statistic — which is
why the bound printed here is per document and why a zero quoted without it
means nothing. Never pool arms or pool sizes into one headline rate either;
the cells measure different things on purpose.

**A thinned cell is not a measurement of its axis.** Every cell reports
evaluated pools against the pools that were eligible, and prints
`NOT REPRESENTATIVE (n of m eligible)` when it thinned — a cell short of
distractors is skipped rather than padded, since padding an adversarial arm
with whatever was available would quietly turn it into the random arm.
`same-venue-year` at N=25 may survive only for one heavily represented
journal, in which case that cell measures that journal and not the axis. Read
that marker before reading a rate; a narrow arm thins first, which is normal
rather than a defect, and is exactly why the marker is there.

**Ground truth is an equivalence class, not a row.** A library holds a
preprint and its version of record, duplicate rows from re-imports, and the
occasional wrong Zotero record. Binding a same-work candidate that happens
to carry different metadata is not a wrong bind, and scoring it as one would
manufacture the very failure the instrument exists to count. Classes are
built from canonicalized strong identifiers, and the pairs that can't be
derived — preprint against version of record above all — are enumerated by
hand in `-true-classes` rather than inferred, because that's the case most
likely to be silently wrong. Every class carries its provenance, so a reader
can tell a derived class from an adjudicated one. Where no class can be
established the trial is excluded and counted under the `unestab` column,
never guessed into an arm — watch that count, since a run where it grows is a
run measuring less than it appears to. `-true-classes` takes one JSON object
mapping a document key to the keys in its class
(`{"ABCD1234": ["ABCD1234", "WXYZ5678"]}`); a missing file is an error rather
than an empty map, because a typo'd path must not silently downgrade
adjudicated truth to inferred truth, and an empty class is refused because
"the class is empty" and "the class is unknown" are different statements —
the second is said by leaving the document out of the file.

### The arms, and what each one is worth

`random`, `same-author`, `same-venue-year`, `title-superset` and `same-year`
each vary one axis of the pool, over real library documents. `conjunction` is
different in kind: it composes the withdrawn failure deliberately, measuring
synthesized bytes that print the target's DOI as a *cited* identifier and
their own different DOI a little further on — both past the 1 KiB blind window,
both inside page one's 4 KiB cap — around a distractor carrying the target's
title, authors and year.

**Read `conjunction` first, and read it as "reproduces" or "does not
reproduce", never as a prevalence.** Per-axis arms on their own reproduce, one
level up, exactly the mistake that withdrew `candidate_auto_bind/1`: the
synthetic gate corpus supplied the ingredients of that failure separately and
never composed them into one document, and four review rounds examined gates
individually and missed the composition. An arm that varies a single axis
cannot contain the composed adversary, so a clean sweep of per-axis arms is
not evidence about the failure that actually happened. But the flip side is
that this arm's rate is a reproduction check and not an estimate of anything:
its documents are synthesized and all carry the same geometry, so a cell at
100% says the rule fails on that construction, not that a hundred percent of
anything is at risk. Only the composite arm bounds real prevalence, and only
from below.

Two things about that arm's rows will otherwise mislead you, and the report
says both beside the numbers: the document key names the library document the
synthesized bytes were *derived from*, not the bytes measured, so a chosen key
equal to the document key is the arm's designed failure rather than the rule
binding a paper to its own PDF; and the target-present form abstaining on
every pool is the honest reading too, because the pool then holds two jobs
differing only in identifier, one printed as a citation and one as the
document's own, and the rule genuinely cannot tell them apart.

An arm that could not be built reports zero eligible pools and marks itself
non-representative rather than failing the run — which is the honest reading,
the same way an empty arm is not a clean one. Two of the seven gates are in
that position today: no arm synthesizes a non-article or a correction marker,
so those gates sit at zero trials and are simply untested by this instrument.
That is a real limit on what the report can gate, not a detail — the report
names any such gate explicitly, and a rule change aimed at either of them
cannot be judged here at all.

**The backlog arm is descriptive coverage, not calibration.** `Grab` persists
id, title, state, quarantine, job and outcome, and no candidate snapshot, so
the pool that existed when a historical grab settled is unrecoverable. A
present-day read of candidate-eligible jobs is a snapshot of what is pending
now, not a time-weighted distribution of what was pending then. It is stress
coverage, and it **may not be used on its own to choose a production pool
cap** — its section prints that caveat beside its own numbers, which is why
it renders next to the report rather than inside it. Making it
calibration-grade needs event-time pool snapshots recorded going forward:
a small, separable change, and not a prerequisite for reading this report.

**Composite prevalence from proposals is a lower bound.** Signals propose, a
human confirms — the label is ground truth, so an unreviewed proposal is
reported unlabelled and counted as neither class. Proposer recall bounds what
the prevalence can be, so the number is a *lower* bound until the audit
sample (rows drawn from documents the proposer did **not** flag) has been
reviewed as well. An audit row a human labels a composite is a proposer miss;
until those rows are labelled, "composites are rare" is unfalsifiable rather
than measured. For a confirmed composite the correct behaviour is to bind
nothing, so its pool carries an empty target class even when the work it
refers to is present in the library.

How wide that bound is depends entirely on how many audit rows you reviewed,
and at small n it is uselessly wide by construction: one reviewed audit row
with no miss in it bounds the miss rate at 95%, which says nothing. Reviewing
the default sample of ~25 is what makes the number mean anything — no misses
in 25 bounds the proposer's miss rate at roughly 11%. The summary also lists
the signals this library could not support, and two composite classes have no
marker vocabulary in the production rule at all — a repository cover sheet and
a journal expansion pass both marker gates — so they are only ever reachable
through the other signals. Read that list rather than assuming the classes
were absent.

`-composite-labels` names that label file. If it doesn't exist the run writes
fresh proposals there and says so; if it does, the run re-proposes over the
current library and merges the new proposals *underneath* the labels already
recorded, rewriting the file only when the row set actually changed. The four
fields a human edits per row (`reviewed`, `class`, `refers_to`, `note`) are
never overwritten. The file holds titles and Zotero keys, so it is library
data: keep it in `dev/scratch/` like a captured report, and never commit it.

The composite arm also needs the loader's all-attachments mode, because the
ordinary load keeps one PDF per bibliographic parent and drops exactly the
supplements and second scans this arm is about. Passing `-composite-labels`
switches the load, and that changes what the corpus counts: the library and
DOI-less counts both rise (supplements are usually DOI-less), and the added
documents land under the `unestab` column for every synthesized arm instead of
becoming targets. That exclusion is a correctness requirement, not
fastidiousness — a supplement inherits its parent's DOI through
`Document.Work`, so letting it be a target would let a run that bound a
paper's job to a supplement PDF score as `correct-bind`, laundering the exact
failure being counted. **Consequence for before/after:** at one seed, a
composite-enabled run and a plain run must agree exactly on every synthesized
cell's pools and counts, with only the composite cell new. If a synthesized
cell moves between those two runs, that is a bug in the exclusion, not a
finding.

### Using it on a change to `internal/pdf/candidate_select.go`

The same before/after workflow and the same one-increment-at-a-time
discipline as [How to judge a change](#how-to-judge-a-change), with the
wrong-bind ordering above:

1. On the baseline, run
   `mkdir -p dev/scratch && go run ./cmd/identity-corpus -candidates > dev/scratch/before-candidates.txt`
   and keep it. `dev/scratch/` is repo-ignored; capture there, never
   anywhere git tracks.
2. Change exactly one increment in `internal/pdf/candidate_select.go` — not a
   bundle. A bundle can't tell you which part earned its place, and with
   seven gates a bundle is even harder to attribute than it was with one.
3. Capture `after-candidates.txt` on the same library, the same cache, the
   same seed, and the **same flags**. The arm set and the pool sweep are part
   of what the numbers mean; changing them changes the question, not the
   answer.
4. Compare **wrong-binds first, per document** — `WrongDocRate` and the
   wrong-bind listing — and give the target-absent cells the same eye. Any
   increase kills the increment, regardless of what else it does.
5. Compare `missed-bind` second, and only once wrong-binds are flat or
   lower. A missed bind costs a human a moment of review; a wrong bind costs
   a library its trust. That ordering is the same trade the printed-title
   rule was judged on.
6. Revert an increment that moves neither number. Same rule as above, same
   reason: the harness exists so that a plausible-sounding tolerance has to
   earn its place instead of being taken on faith.
7. Check the `NOT REPRESENTATIVE` markers and the terminal-gate distribution
   before believing a cell improved. A gate that stopped being *reached* looks
   exactly like a gate that started passing.
8. Never quote the absolute counts as if they held on another library. Same
   caveat as the pairwise mode, and for the same reason.

**The seed is fixed on purpose.** `-seed` defaults to a constant and is never
derived from the clock, and the report prints the value it used: two runs at
one seed over one library draw the same pools, which is the only reason a
before/after diff means anything at all. Change the seed and you have started
a new baseline — capture a new `before`.

`-pool-sizes` starts at 2, and a 1 is refused by name rather than dropped. A
pool of one cannot measure a 1-of-N selection: there is nothing to select
between, so a "correct" bind would show only that the qualification gates
passed, which is what the pairwise mode already measures pair by pair.

Two mechanical notes. The candidate report has no skip-summary section of its
own, so the skip classes described in
[What the corpus leaves out](#what-the-corpus-leaves-out) are printed to
stderr as `skip summary: <class>=<count>` beside the per-skip lines; they are
library data and need the same handling as the rest of that stream. And
`-json` in this mode emits an object wrapping the report rather than the bare
report the pairwise mode emits, because the composite summary and the backlog
section — the lower-bound caveat and the not-calibration caveat — live outside
the report struct, and a captured JSON run must not lose them.

**Extraction still isn't the daemon's.** The corpus extracts with
`pdf.DefaultSemanticOptions` while the daemon runs its own configured PDF
settings; the backlog section computes and prints that divergence from both
sources at run time rather than quoting it, so it can't go stale against
either. Read it once per run: a DOI printed past the 1 KiB blind window but
inside page one's 4 KiB cap **is** visible to this instrument, while one on
page two, or past the 16 KiB excerpt bound, is invisible to the instrument
and to the rules alike.

**Privacy applies unchanged.** [Privacy](#privacy) below covers this mode
too, with two additions rather than any relaxation: the composite label file
is library data on disk (above), and the backlog arm opens your papio store
read-only. Read-only means the database cannot be modified through that
handle, but opening a WAL-mode database still recreates the
`papio.db-wal`/`papio.db-shm` sidecars beside it and leaves them there —
the same behaviour already documented above for `zotero.sqlite`, harmless,
and startling if your daemon had checkpointed and closed the store.

**What this mode does not gate.** A selector-level measurement can't see
`FrontMatterDOIs` reachability in production, eligibility-pool construction,
the durable bind fence, ownership arbitration, or concurrency. It grades the
rule, not the feature; shipping a rule change additionally needs an
integration-level gate over those paths.

## Privacy

The report — text or `-json` — contains the titles and author names of
every PDF in your Zotero library, i.e. your own reading. **Never** paste it
into a GitHub issue, a commit message, or `CHANGELOG.md`, and never commit
a captured report file. If you need to share evidence for a regression,
share the aggregate counts (`Correct`, `Mismatch`, `len(WrongAccepts)`) by
hand, not the report itself.

Stderr is not exempt from this. Every skipped candidate prints its Zotero
attachment key and the reason class it was excluded under (the same
classes `Report.SkipsByReason` tallies — `"file missing"`, `"output cap"`,
and so on). Neither is a filesystem path, but an attachment key and a
skip class are still library data about a specific paper — a captured
stderr needs the same handling as a captured report, not "it's just an
error log".

The `-cache` directory persists the front matter of every paper in your
library — title, byline, and the identifier windows `identity.go`
reads — across runs, so a second run doesn't re-extract. It is not
cleared automatically; delete it yourself once you're done with it, or
pass `-cache ""` to disable caching for a run entirely (extraction then
re-runs from scratch every time — slow, but nothing persists). By default
it now lives under your per-user cache directory rather than shared temp
(see the flags table above), so another account on a multi-user machine
can't enumerate or tamper with it, but that's isolation, not expiry: it's
still your responsibility to clear it when you're done.
