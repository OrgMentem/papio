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
actually *scores* 679 documents — not the 789 raw PDF attachments above,
which include everything Load later skips — so `N×(N−1)` is
679×678 = 460,362 candidate pairs, 460,352 after same-work exclusions.
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
