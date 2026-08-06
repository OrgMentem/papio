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

## Running it

```
go run ./cmd/identity-corpus
```

Flags:

| Flag | Default | Meaning |
|---|---|---|
| `-zotero` | `~/Zotero` | Zotero data directory (containing `zotero.sqlite` and `storage/`) |
| `-cache` | `$TMPDIR/papio-identity-corpus` | extracted-text cache dir; `-cache ""` disables caching |
| `-workers` | `0` (= `runtime.NumCPU()`) | extraction concurrency |
| `-json` | off | emit the `Report` as indented JSON instead of the text report |

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

Pair count is `N×(N−1)`: at ~789 documents that's roughly 620,000 identity
decisions per run. Once text is cached, that's seconds of CPU — the
extraction, not the matching, is the cost.

## Using it on a change to `internal/pdf/identity.go`

1. On `main` (or whatever you're comparing against), run
   `go run ./cmd/identity-corpus > before.txt` and keep it.
2. Make your change to `internal/pdf/identity.go`.
3. Run `go run ./cmd/identity-corpus > after.txt` again — same library, same
   cache, so extraction is skipped and only the matching logic re-runs.
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

## Privacy

The report — text or `-json` — contains the titles and author names of
every PDF in your Zotero library, i.e. your own reading. **Never** paste it
into a GitHub issue, a commit message, or `CHANGELOG.md`, and never commit
a captured `before.txt`/`after.txt`. If you need to share evidence for a
regression, share the aggregate counts (`Correct`, `Mismatch`,
`len(WrongAccepts)`) by hand, not the report itself.
