# ADR-0020: Grab open PDFs from the browser

Status: Accepted (2026-08-08). Drafted from the 2026-08-08 field session: an
operator reading `pdf.sciencedirectassets.com/...main.pdf` and a repository
`..._lead.pdf` asked papio to grab the paper they were already looking at;
the selection workspace honestly reported "no recognizable identifiers"
because a browser PDF tab has no DOM to scan.

## Context

The scanner (ADR-0019) reads rendered HTML. A PDF tab renders inside
Chrome's viewer: the injected scan context sees only the viewer wrapper, so
even a paper whose first page prints its own DOI yields zero detections.
Meanwhile the daemon already owns everything needed to identify a PDF it
holds: `internal/pdf` extracts front-matter DOIs (`documentDOIs`) and scores
identity (`MatchIdentity`), quarantine performs structural validation, and
browser-download adoption (steered `papio/<dir>/` downloads) is a proven
transport that rides the user's authenticated session.

Constraints that bound the design:

- **Native messaging frames cap at 256 KiB** (`MaxBrowserMessageBytes`); PDF
  bytes must never ride the wire.
- **Jobs are keyed by canonical work identity at creation** (ADR-0010's
  dedupe invariants — `liveJobForCanonicalWork` on `work.Describe()`).
  Identity-less jobs would undermine every dedupe guarantee, so the capture
  must precede the job, not the reverse.
- **ADR-0019's title-only stance**: nothing browser-sourced is submitted on
  a title guess; v1 of the detector refuses weak matches. The same applies
  here — a PDF whose front matter yields no identifier parks for a human,
  it does not enrich-and-hope.
- Firefox cannot steer downloads (`onDeterminingFilename` absent), so the
  capture transport is Chrome-only; Firefox degrades to guidance.

## Decision

1. **Entry point: the existing scan flow.** Scanning a PDF tab (detected by
   the viewer wrapper / URL shape) renders the selection workspace with a
   single "grab this PDF" row (tab title + URL host) instead of the empty
   "no identifiers" state. Same consent copy, same explicit invocation —
   the grab is one more thing "Select papers on this page" can mean.
2. **Tab-URL identifiers first, bytes second.** Before offering a grab, the
   scan applies the ordinary URL identifier rules to the *tab URL itself*
   (arXiv `/pdf/` shapes, DOI-shaped path segments). A hit becomes a normal
   identifier row — no grab needed, full ordinary pipeline. The grab
   affordance covers the remainder (asset CDNs, repositories, signed URLs).
3. **Capture before job.** Accepting the grab:
   - extension → daemon `pdf_grab_request` (new message pair behind a
     `pdf_grab_v1` hello-ack feature): tab URL + title; daemon allocates a
     grab id and returns the steering path `papio/grabs/<grab-id>/` under
     the adoption root;
   - extension `chrome.downloads.download(tab.url)` steered to that path —
     the browser's own cookies/session fetch the bytes; nothing is
     re-requested through papio and no bytes cross native messaging;
   - the daemon's grab sweeper (same bounded, latch-aware reader as
     adoption) picks up the settled file.
4. **Identity from the file, then an ordinary job.** The daemon quarantines
   the capture, runs structural validation, and extracts front-matter
   identifiers. *Amended 2026-08-16:* the blind identifier class is **DOI
   only** (`FrontMatterDOIs`). This clause originally said "plus the
   existing arXiv/PMID front-matter patterns where present"; that is
   superseded. arXiv and PMID recognition is **target-aware** — it requires
   a candidate to check against, so a target-less pipeline cannot use it
   without minting an identifier, which Decision 4 forbids. The consequence
   is deliberate and worth stating: a capture whose front matter names only
   an arXiv id or a PMID parks rather than creating a job, and the
   conclusive-identity veto likewise cannot see a foreign arXiv/PMID work.
   Widening the blind class later means widening it in `FrontMatterDOIs`,
   the veto, and ordinary grab identification together, using the existing
   normalizers — not in one of the three.
   - **Identifier found** → create the ordinary identifier-keyed job
     (consumer `browser-pdf:<host>`), with the captured file injected as a
     top-ranked local candidate. Resolution proceeds normally — metadata
     from the registrar, `MatchIdentity` against the captured bytes — so a
     wrong PDF under a right-looking DOI still fails identity review
     exactly as any acquisition would. Ledger dedupe applies naturally; an
     already-owned work reports "already in your library" as the grab
     outcome instead of a duplicate job.
   - **No identifier** → the grab parks as a human action carrying the
     extracted title guess and the quarantine path. Never a title-only
     submission (ADR-0019's line holds).
5. **Privacy.** The grab is explicit per-click consent; the captured bytes
   stay on the machine; no scholarly service is contacted until the
   extracted identifier enters the ordinary resolution pipeline — the same
   moment consent semantics already cover for every submitted work.

## Consequences

- New protocol pair + feature flag (`pdf_grab_request`/`pdf_grab_result`,
  `pdf_grab_v1`), all three protocol artifacts, fail-closed.
- New reserved `grabs/` namespace under the adoption root; the grab sweeper
  shares the bounded-reader latch, and `SweepTerminalAdoptions`'s
  unknown-dir hygiene must learn the reserved prefix.
- A grab store row (id, url host, state, quarantine binding) — one
  migration — so grabs survive restarts and surface in doctor/stats.
- Firefox: the workspace shows the grab row disabled with honest copy (no
  download steering on Firefox); the URL-identifier path (Decision 2) works
  everywhere.
- The identity-corpus wrong-accept doctrine is untouched: no new acceptance
  path exists — grabs converge into the standard resolution + identity
  pipeline.

## Rejected alternatives

- **Uploading PDF bytes over native messaging** — the 256 KiB transport cap
  is fail-fatal (the MDPI page-capture incident), and chunking protocols
  for multi-MB files over stdin framing buy nothing over a steered
  download that already exists.
- **In-extension text extraction (pdf.js)** — a heavyweight dependency in a
  zero-dependency extension, duplicating a better extractor the daemon
  already has, against bytes the daemon will hold anyway.
- **Identity-less jobs re-keyed after extraction** — breaks ADR-0010's
  canonical-identity dedupe; two grabs of the same paper would race to
  distinct jobs and reconcile never.
- **Automatic grabbing of every PDF tab** — the Nomad lesson (ADR-0019):
  ambient collection is the product papio refused to be; grabs are
  explicit, one click, one file.

## Amendment 2026-08-16: candidate binding is an acceptance-affecting route

Candidate binding IS a new acceptance-affecting route, superseding the
Consequences claim that "no new acceptance path exists" and narrowing
Decision 4.

Blind capture still never creates or names a work from title evidence. A
capture lacking a conclusive blind identifier may be correlated with an
already-established job, but that correlation is itself an identity
decision. Automatic correlation must satisfy the separately specified
candidate-binding rule (`candidate_auto_bind/1`) and must abstain on
ambiguity or contradictory identity evidence. A human job selection
supplies correlation evidence, not authority to override conclusive
document identity, and ordinary candidate validation remains mandatory
before an artifact becomes a job's accepted PDF.

Narrows by number: **Decision 4** — "No identifier → the grab parks as a
human action" now parks only when automatic correlation abstains (zero or
multiple qualifiers, any `Review`, or a conclusive-identity veto); and the
Consequences bullet that the wrong-accept doctrine is untouched. Decisions
1–3 and 5, the privacy posture, and the transport (steered download, no
bytes over native messaging) are unchanged. ADR-0019's title-only stance
holds — no phase creates a work from title — and ADR-0010's dedupe
invariants hold — no new job-creation path.

Two predicates enforce the amendment at the single convergence
`validateCandidate` (direct delivery, grab binds, adoption sweeps, resolver
fetches) and at the settled-grab sweep:

- **Conclusive-identity veto.** `D` is the conclusive DOI set from the
  1 KiB blind front-matter window (`pdf.FrontMatterDOIs`). `|D| = 0` → no
  veto; `|D| > 1` → park `verify_identity`; `|D| = 1` → compatible only
  when the job's DOI equals it under the identity-comparison normalisation
  or the job's submission-time recorded metadata already binds that DOI.
  No runtime resolver lookup. `ReviewOverride` (explicit human review of
  the quarantined preview, ADR-0002) still overrides; a picker selection
  never does.
- **`candidate_auto_bind/1`.** DOI-less settled grabs are scored against
  the daemon-authoritative candidate-eligible pool (live,
  `StateAwaitingHuman`, open `manual_download` action). Auto-bind requires
  real author evidence, exact printed title (`titlePrintedAsLine`), the
  candidate-binding year predicate (distinct from `MatchIdentity.yearConflict`),
  identifier corroboration over `identityPageOne` (4 KiB), and the
  conclusive-identity veto — with exactly one qualifier and no `Review`.
  Otherwise the grab stays parked.

Binding is fenced by a serialized final recompute inside the same
transaction that CASes the grab to `job_created`; provenance
(`method=candidate_auto_bind`, rule version, evidence, candidates
considered, winner) is written atomically as nullable
`pdf_grabs.bind_provenance` (migration 0037, store schema 37). The outward
wire outcome remains `job_created` — the method is never encoded in the
outcome. The rule shipped only after the
measurement gate reported zero wrong-accepts. Two of the three planned
layers ran: the labeled semantic corpus and the extractor sentinels over
real PDFs. The third layer — replay against the local backlog of
historical manual-download jobs — is deferred, so what is measured is
safety against curated hard negatives, not coverage over the operator's
real library. Coverage is therefore unknown: the gate establishes that
auto-bind does not misfile the cases it was shown, not how often it fires
in practice.

### Amended again, 2026-08-16: autonomous binding is disabled

A fourth review round (pro-tier oracle, verdict NEEDS REVISION) found that
`candidate_auto_bind/1` had deterministic wrong-accept paths, and that the
gate above could not have seen them. **Autonomous binding is switched off.**
A settled DOI-less grab parks for human identification, exactly as Decision 4
originally specified. The picker (Phase 1) and the conclusive-identity veto
stand; only the autonomous decision is withdrawn.

The root cause was one error repeated across gates, not five separate bugs:
the rule treated a hit anywhere in a 2 KiB window as positional evidence, and
treated a candidate-aware identifier hit as the document identifying **itself**
— when the entire danger class is "another document mentions the candidate".
A journal expansion printing "Extended from DOI *X*", with its own DOI just
past the 1 KiB blind window, satisfied every gate and would have been filed as
*X*. The gate could not see it because its hard negatives supplied the
ingredients separately (a foreign DOI with unrelated title and authors; a
conference/journal pair distinguished by year) and never composed them into a
single relational block — a citation card, a repository cover sheet, an
"extends" line — which is how the failure actually presents.

Two further corrections landed with the disablement, both independent of the
rule: the veto **collapsed DOI registrant slash runs**, so a document
conclusively naming `10.48612//x` read as compatible with a job bound to
`10.48612/x` — two separately registered DataCite works (the pinned pair in
`internal/ownership`). It now abstains on that difference, which is correct
under both competing facts, since a legacy APA reprint printing `10.1037//…`
for the same work is equally unresolvable lexically. And
`corroboratingIdentifier` collapsed only its needle and never the page, so a
target registered with a doubled slash searched for the single-slash form and
reported the *other* work as corroboration.

`candidate_auto_bind/2` is a redesign, not a patch. It requires
self-identification rather than corroboration (scan the whole window for every
identifier, abstain on any foreign or additional one, and require the
candidate's identifier in a self-identifier locus), one parsed front-matter
assertion supplying title/byline/year/own-identifier together with abstention
on unrecognised layouts, an exact daemon-minted delivery lease naming job and
action (which needs a new feature-gated message kind — the no-wire-change
preference does not outrank exact authority), a durable claim that reserves
the winning job and action so two grabs cannot both bind one job, a gate that
asserts **observed** traversal rather than declared labels, and the previously
deferred backlog replay with full candidate pools. That last item is now a
blocker: deferring it was only defensible while the rule was believed sound.
The working plan (`dev/active/send-pdf-candidate-binding.md`) carries the
itemised blocking set; this ADR carries the decision.
