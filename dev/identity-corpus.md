# identity-corpus — measuring `internal/pdf/identity.go`

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
  than scoring it.
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

`<user cache dir>` is `os.UserCacheDir()` — `~/Library/Caches` on macOS,
`$XDG_CACHE_HOME` or `~/.cache` on Linux, `%LocalAppData%` on Windows —
which is per-user by construction. It falls back to
`$TMPDIR/papio-identity-corpus` only if `os.UserCacheDir()` itself can't
resolve a directory; see [Privacy](#privacy) for why the default isn't
shared temp.

Needs `pdftotext` (poppler) on `PATH` — the same extraction dependency the
daemon itself needs (`brew install poppler`, see the README).

It opens `zotero.sqlite` through a temporary copy, so it's safe to run
while Zotero is open. It never writes to the library and never makes a
network call.

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

**The cost, stated plainly.** 27 correct documents moved from pass to
review. They are not a random sample — they're dominated by exactly what
token overlap could never separate in the first place: 7 volumes of the
numbered series report, generic one- and two-token titles ("Code of
Ethics", "Organisational behaviour"), one metadata typo ("Fundamentals of
EEF Measurement" cataloged against a paper titled about EEG), and papers
whose printed subtitle differs from the catalogue record. The trade is
deliberate: papio parking a correct paper costs a human a moment of
review; papio filing the wrong paper costs a library its trust.

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
