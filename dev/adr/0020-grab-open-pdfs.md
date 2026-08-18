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
     `pdf_grab_v1` hello-ack feature): *Amended 2026-08-17:* **host and title
     only**, never the tab URL. This clause originally said "tab URL + title";
     a provider or library-proxy delivery URL carries a signing token or an
     interlibrary-loan ticket that works like a password, so the address is
     compared locally and never crosses the bridge or reaches a log. The daemon
     allocates a grab id and returns the steering path
     `papio/grabs/<grab-id>/` under the adoption root;
   - extension `chrome.downloads.download(...)` steered to that path — the
     browser's own cookies/session fetch the bytes; nothing is re-requested
     through papio and no bytes cross native messaging;
   - *Amended 2026-08-17:* **a single-use delivery URL is never fetched.**
     `carriesSignedCredential` recognises the class structurally, and the grab
     arms its steering and asks the researcher to press the PDF viewer's own
     **Download** button instead — bytes the browser already holds. Asking a
     second time returns the provider's session-timeout page, not the file,
     which stalled the capture and occupied the effect lane until its permit
     was resolved. Firefox refuses this path outright: without
     `onDeterminingFilename` a download papio did not start can never be
     steered or adopted, so promising to file it would be a promise the
     platform cannot keep;
   - *Amended 2026-08-17:* **an exact job binding outranks a grab; an inferred
     one does not.** A download this extension started for a job keeps that
     job. A stale tab correlation no longer beats a live grab, which had been
     steering a paper the researcher named into the directory of a paper the
     tab was opened for days earlier;
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

### Amended again, 2026-08-17: a capture must be cancellable

The original decision recorded how a capture is created and settled but not how
one is given up on, and the gap was total: no cancellation this extension sent
could take effect. `pdf_grab_abandon_request` is fenced on the capture's
originating `effect_request_id` — a grab id alone must not release occupancy,
which stands — but the extension minted a fresh correlation id per call, so every
attempt missed and came back `conflict` inside a successful reply. The extension
now mints that id itself, persists it, and presents it.

Two rules follow, and both are narrower than "cancel on request":

- **A capture whose permit is settled may be retired on the grab id alone**
  (`MarkAbandonedUnoccupied`), because a settled permit is positive evidence
  that there is no occupancy left to release. The predicate demands that
  evidence rather than merely the absence of an occupying permit: absence passes
  vacuously for a capture predating `0034_effect_permits.sql`, whose unresolved
  `legacy_effect_blocker` would then be stranded, and one unresolved blocker
  refuses every future allocation. Held and `unknown_completion` remain
  untouchable, so ADR-0022's rule that lost or ambiguous completion keeps
  occupying is unchanged.
- **A refused cancellation is reported, never retried into.** `conflict` inside
  an `ok` reply is not clearance, and treating it as such produced a second
  attempt that repeated the same refusal while telling the researcher the paper
  had been sent.

The storage half of this is also constrained: an armed capture persists the
download **route** (origin and path), never the URL, so the signing token stays
out of extension storage while the steering key remains exactly what
`sameDownloadRoute` compares.

One migration-shaped consequence is recorded here because it is a decision, not
an implementation detail. `0025_pdf_grabs.sql` was edited in place after release
to add `'abandoned'` to its state CHECK and to add the single-active-capture
unique index. Databases migrated in between have neither, so on those *no*
abandonment could be written at all. `0038` rebuilds the table for the CHECK but
**changes no data**, and the unique index is installed by
`ensurePdfGrabActiveSourceIndex` at open time, tolerating the one case that can
fail: a paper that already holds two active captures. Retiring one to make the
index fit was implemented and then rejected — a duplicate may be `quarantined`,
holding the only copy of a paper's bytes, and `SweepGrabs` skips retired
captures, so the repair would discard a paper instead of filing it. No migration
may guess which of two captures is the real one; `papio doctor`'s
`capture_uniqueness` check names the condition and the remedy instead.

### Amended again, 2026-08-18: autonomous binding is enabled, overriding the 2026-08-16 blocking set

**Autonomous binding is switched back on** (`autoBindDecisionEnabled = true`),
on the operator's decision, at rule version `candidate_auto_bind/3`. A settled
DOI-less grab that qualifies exactly one pending job is filed automatically;
everything else parks exactly as before.

This amendment **overrides** rather than satisfies the blocking set the
2026-08-16 amendment laid down for `/2`, and the honest accounting matters more
than the decision. Of the six items that amendment required:

| required for `/2` | status at `/3` |
|---|---|
| durable claim so two grabs cannot bind one job | **satisfied** — in-transaction fence, `ErrFenceRejected` |
| gate asserting *observed* traversal, not declared labels | **satisfied** — `CandidateQualification.Reached` is recorded during the traversal |
| self-identification rather than corroboration | **partly** — the metadata arm is genuine self-identification; the text arm still corroborates |
| one parsed front-matter assertion, abstain on unknown layouts | **not built** — `dev/active/structural-front-matter-parser.md`, ungated |
| exact daemon-minted delivery lease naming job and action | **not applicable** — that requirement belongs to the human picker path, which shipped with its own binding; an autonomous decision has no human choice to authenticate |
| backlog replay with full candidate pools | **cannot run** — the replay arm builds zero pools, because 318 of the operator's eligible jobs carry no canonicalizable identifier. It is not deferred; it is unanswerable against this store |

So three of six hold, one is partial, one is void, and one is unanswerable. What
authorises the change is therefore **not** design completion but measurement
against the population this path actually serves, plus one structural argument
about that population:

- A grab exists because a human clicked Send PDF **with the document open in
  front of them**. The wrong-kind-of-document families that no predicate catches
  reliably — supplements, cover sheets, obvious errata — are excluded by the
  person, before any rule runs. That is what makes the operator's own library a
  representative sample of this path rather than a convenient one, and it is the
  argument the 2026-08-16 amendment did not have available, because it was
  reasoning about bytes in general.
- Over ~9,800 trials at pool sizes 2/5/10/25 across the random, same-author,
  same-year, title-superset and same-venue-year arms: **zero wrong binds**,
  per-document one-sided 95% bound **0.94%**, **65 of 318** documents (20.4%)
  correctly bound — above the 10% viability floor. Pool size is not a risk axis:
  N=2 and N=25 are identical, because a randomly drawn distractor essentially
  never clears title AND author AND year together.
- `candidate_auto_bind/3` added embedded-metadata corroboration, which raised
  correct binds from 44 to 65 on the same corpus. Metadata cannot be reached by a
  reference list, so it carries the attribution the text arm can only approximate.

**The residual risk is named, not bounded.** The "conjunction" family — a
document printing another work's title, authors, year AND identifier with no
correction word — is bound wrongly 311 times out of 311 in the measurement's
synthetic arm. Adversarial review found real instances (an Oxford Academic
Editor's Note, an eNeuro "See related article" commentary) and both phrases are
now `correctionMarkers`, so labelled instances park. **An unlabelled instance
remains a live wrong-accept path**, and no vocabulary closes it — only the
structural parser does. This is a deliberate exposure, accepted with the
measurement above, and it is the reason the structural parser stays the plan of
record rather than being dropped as superseded.

Two operational gaps are recorded because they bound the decision's reversibility:

1. **There is no unbind.** `papio grabs identify` binds a parked grab; nothing
   reverses a bind. A wrong autonomous filing is corrected by hand, in Zotero and
   in the job, today.
2. Provenance was written and never surfaced. Because an irreversible automatic
   decision with no audit surface is not reviewable at all, `papio grabs binds`
   ships with this change: it lists what the machine decided, newest first, with
   the rule version, the candidates considered and the winner's evidence. That
   listing is the operator's only recourse until an unbind exists, and it is also
   how the 20.4% and the zero-wrong-binds figures get re-tested against real
   captures rather than a library replay.

Narrows by number: **Decision 4** — a grab with no identifier parks — now parks
only when the autonomous decision abstains. The 2026-08-16 amendment's sentence
"Autonomous binding is switched off" no longer describes the code; it is retained
above as the record of why it was off and of what was found unsafe, all of which
remains true.


