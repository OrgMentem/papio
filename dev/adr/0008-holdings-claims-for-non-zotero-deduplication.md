# ADR-0008: Holdings claims for non-Zotero de-duplication

Status: Accepted (2026-07-30). Extends ADR-0004's accepted degradation
("ownership lookup degrades to not-owned with a staleness warning"). Reviewed
against a second model (oracle session `non-zotero-dedupe-pro`). Citations below
refer to the current implementation; deferred work is marked explicitly.

## Context

*papio* acquires validated PDFs and files them either through zotio → Zotero
(deep integration) or through the generic `[hooks] on_ready` command
(ADR-0004). Before the v1 holdings work, de-duplication did not make that
transition. With `zotio.executable` empty, the pre-v1 behavior was:

- `papio search` did not mark a result `[in library]`
- `papio acquire --batch refs.bib` re-acquired papers the user already held
- backfill-style watches were impossible

The v1 implementation now addresses generic-source de-duplication; these
bullets are historical context, not current behavior.

The initial plan was to generalise the existing zotio ownership lookup behind a
provider interface, on the premise that its contract was already
destination-agnostic. **That premise is false in the way that matters.**

`internal/zotio/service.go` exposes `LookupWork{DOI, ArXiv}` → a tri-state
(`not_owned` / `owned_missing_pdf` / `owned_with_pdf`) plus `ItemKey` and a
`StalenessWarning`. The JSON looks neutral. The semantics are not:
`internal/batch/submit.go:220-225` **rejects** `owned_missing_pdf` that carries
no item key and writes that key into `request.ZoteroItemKey`. So the status does
not mean "the record exists without a PDF"; it means *"attach into this Zotero
parent"* — an action, routed to one destination. Meanwhile
`internal/discovery/ownership.go:19-55` flattens both owned states to a single
`Owned` boolean. One type, several destination-specific caller policies.

Four further facts constrain any design here, all verified:

- **Historical parser context.** Before the raw-parser split, acquisition parsing
  was strict and coupled to batch submission; that observation motivated the
  extraction, but is no longer a description of the current code. Today
  `internal/bibparse.ParseRecords` returns neutral records and rejects only
  structural errors (`internal/bibparse/bibparse.go:140-164`). The native JSONL
  batch reader remains outside this package (`:149-161`), so the ownership core
  does not inherit acquisition's `internal/batch` dependency.
- **Current identifier coverage is format-specific.** BibTeX/BibLaTeX provides
  DOI, arXiv, and PMID; CSL-JSON provides DOI and PMID; NBIB provides DOI and
  PMID; RIS provides DOI. No supported parser populates ISBN. The coverage is
  documented by `internal/bibparse/bibparse.go:62-78` and the format parsers
  (`internal/bibparse/bibtex.go:160-168`,
  `internal/bibparse/csljson.go:7-16`,
  `internal/bibparse/nbib.go:74-89`,
  `internal/bibparse/ris.go:81-99`).
- **PDF-text DOI extraction is deliberately front-matter-only.**
  `documentDOIs` is only ever fed a 1 KiB first-page window
  (`internal/pdf/identity.go:53,64,199-208`), because "a document's own DOI must
  sit at the very top or it is probably a reference". Pointing it at full
  `pdftotext` output would index *cited* works as owned — manufacturing exactly
  the false-positive skips this ADR exists to prevent.
- **Ownership lookup is not backfill.** Discovery watches use the holdings
  selector when configured, while the Zotero-only path still discards both
  owned states rather than acquiring the missing-PDF one
  (`internal/watch/runner.go:302-360`). Backfill watches call Zotero's queue
  directly (`internal/watch/runner.go:271-301`). A snapshot cannot enumerate
  records needing PDFs.

## Options

### A. A supported-app list (papis, Calibre, JabRef, Mendeley…)

Rejected, for ADR-0004's reason: bespoke integrations do not scale, and here
they are not even necessary — matching is app-agnostic once you have
identifiers.

### B. Generalise the zotio tri-state behind a provider interface

Rejected. `owned_missing_pdf` carries a Zotero routing obligation
(`internal/batch/submit.go:220-225`); a generic source can never satisfy it, because
the type forces every caller policy into precedence exceptions instead of
eliminating them.

### C. Positive holdings claims, aggregated by *papio* (accepted)

A **holdings provider** emits *positive evidence only* — this record exists,
this artifact is present/missing/unknown, this identifier matched. It never
emits "not owned". "No match" is computed by an aggregator and is actionable
only when every source required for a negative decision was successfully read.

## Decision

Option C, with the following invariants. **In v1, invariants 1–4 and 6–10
are enforced; invariant 5 is deferred.** Deferred items are not acceptance
criteria for this ADR's v1.
 
1. **[v1 enforced] Provider failure never creates a negative ownership fact.**
   `not_owned` means "successfully checked, no qualifying claim", never "the
   check failed". A source failure yields *incomplete*, and the lookup result
   carries structured completeness — `doctor` is standing diagnostics, not the
   runtime channel.
2. **[v1 enforced] Only a fresh, explicit artifact-present claim may suppress
   acquisition.** A false-positive skip silently withholds a paper the user
   asked for; a false negative costs one download. Bias hard.
3. **[v1 enforced] A generic source never produces a zotio attachment route.**
4. **[v1 enforced] `owned_missing_pdf` stays a zotio compatibility concept**, not
   the new core model.
5. **[deferred] Version-aware local artifact reuse.** *papio*'s own acquisition
   history proves artifact availability, not destination ownership. A `ready`
   job means *papio* validated a PDF; hooks are fire-and-forget and unretried
   (ADR-0004), so *papio* cannot know a manager received it. The current DOI
   artifact cache is not version-aware, and `force` is not retained as a cache
   bypass. Version-aware reuse (exact identifier, candidate's actual version,
   artifact row present, file still on disk, and explicit re-download bypass)
   remains future work, reported as "*papio* already has a validated artifact",
   never as `[in library]`.
6. **[v1 enforced] ISBN is never a sole match** without explicit book-level
   identity on both sides. An edited volume shares one ISBN across every
   chapter, and neither the query nor `Record` preserves entity granularity,
   so "ISBN only for book-level works" is currently unimplementable. Excluded.
7. **[v1 enforced] PDF scanning indexes only self-identifiers**, never
   identifiers found anywhere in document text.
8. **[v1 enforced] Old RPC methods retain old semantics, not merely old schemas.**
   `zotio.lookup_works` stays zotio-only and is *not* delegated to the
   aggregator: an old CLI acts on the zotio meaning of `owned_missing_pdf` and
   fails without an item key. A new CLI falls back to it on method-not-found,
   DOI/arXiv only.
9. **[v1 enforced] In-process catastrophic source-record-count collapse keeps
   the last-known-good index and fails closed.** A successful empty snapshot
   and an unavailable snapshot are different states. Restart establishes the
   current file as a new baseline; v1 has no persistent baseline and no explicit
   acceptance command, so this is not restart-proof safety. Persistent
   acceptance is future work.
10. **[v1 enforced] Source facts are unioned.** One library's absence never
    negates another library's positive claim. Negative claims are source-scoped.

### Shape

```text
internal/ownership/          claim model, aggregation, decision helpers
internal/ownershipsnapshot/  file/command loaders, cache, index
internal/bibparse/           raw Format/Record/Detect/ParseRecords
internal/ingest/             bibparse.Record -> acquisition WorkRequest
internal/zotio/              existing service + ownership adapter
```

The raw parsers split out of `internal/ingest` so the ownership core does not
inherit `internal/batch`. Holdings parsing is *tolerant* where acquisition
parsing is strict: a structural parse error rejects the refresh and retains the
prior index; a record with no supported identifier is counted and ignored; an
invalid identifier is dropped while the record's other identifiers survive;
titles are never indexed for ownership.

File and command sources share one parser and index but compile to different
loaders — a file has a cheap revision probe (identity, size, mtime), a command
needs a TTL, timeout, stdout cap, single-flight refresh, and last-good state.
Commands are `argv`, never an implicit shell string; a pipeline is spelled
`["/bin/sh", "-c", …]` so the shell is visible. One execution per refresh, never
one per work.

Each source declares what its output asserts, rather than *papio* inferring it
from per-manager attachment conventions:

```toml
[[library.sources]]
name   = "owned-pdfs"
kind   = "file"          # or "command"
path   = "~/library/papers-with-pdfs.bib"
format = "bibtex"
claim  = "pdf_present"   # or "record_present"
```

`record_present` may annotate search results; it must not suppress acquisition.
A manager-specific filter ("only entries with a usable PDF") lives in the user's
command, not in *papio*.

### Sequencing

**v1** — new `internal/ownership` core and a new RPC carrying `[]{kind,value}`
identifiers plus `desired_version` and `entity_kind`; one **file** snapshot
source; DOI, arXiv, and PMID identifiers; in-memory immutable index, therefore
**no migration**; generic sources active only when zotio is unconfigured; wired
into `search`, `--batch`, discovery acquire watches, and one `doctor` check per
source. Version-aware artifact reuse is not part of v1.

**v1.1** — the command loader behind the same source concept.

**v2** — folder scanning, behind a real `ExtractSelfIdentifiers` (PDF metadata,
constrained front matter, unambiguous arXiv stamps, declining ambiguous
multi-DOI candidates) and a scanner-specific persistent store with
generation-based deletion handling. This is the tier that costs the migration and
the three hardcoded `user_version` assertions.

**Not in scope:** mixed zotio/generic precedence (there is no universally correct
rule — "make this Zotero item complete" and "do I own a PDF anywhere?" are
different questions, so it needs an explicit lookup purpose); generic backfill
(needs an `Enumerator`, not a lookup, plus a way for a hook to update an existing
record instead of duplicating it); a `papio library` CLI surface.

## Consequences

- Non-Zotero users get real de-duplication in `search`, `--batch`, and acquire
  watches without *papio* learning any manager's name.
- `PAPIO_PMID` is implemented in the hook environment (ADR-0004 freezes names
  but permits additions).
- Hooks remain eventually consistent: a repeat lookup straight after an
  acquisition may still see the old snapshot. The current artifact cache is
  not version-aware and `force` is not retained as a cache bypass; version-aware
  artifact reuse is deferred.
- In-process count-collapse protection retains the last-known-good snapshot and
  fails closed; a daemon restart establishes the current export as a new
  baseline. Persistent acceptance remains future work.

Scope tripwire: if a source ever needs per-manager branching, stderr
interpretation, retry strategy, or write-back, stop and reopen the
bespoke-importer option (ADR-0004 option A). The command's stdout is a public
integration API on the critical path — unlike `on_ready`, a bad claim can
withhold requested work — so it gets a contract, a cap, and a freshness window,
not trust.
