# Send PDF without the inbox "Open" step: candidate binding for DOI-less PDFs

Status: implemented 2026-08-16 — Phases 1–3 shipped (popup picker replacing the inbox pin, conclusive-identity veto, `candidate_auto_bind/1` auto-bind fenced in the binding transaction with `pdf_grabs.bind_provenance` / migration 0037 / schema 37, zero-wrong-accept gate on the main corpus). Phase 4 (ranked one-click confirm) remains deliberately unbuilt, gated on observed parked-grab volume after these phases ship. ADR-0020 amendment landed; see the ADR; salvage normative content into that amendment before deleting this file.

## Problem

A DOI-less PDF can only reach its job today if the researcher first clicks
"Open" on the inbox's manual-download row (`requestManualDownloadOpen`,
`extension/src/inbox.ts:2829`), which sets the browser-local
`manual_delivery_target` pin (`extension/src/state.ts:203`), and then clicks
Send PDF from the pinned context. Forgetting the Open step sends the bytes
down the blind grab path, which parks `needs_identifier` and demands
`papio grabs identify <id> --doi …` in a terminal.

## Evidence base (verified 2026-08-16)

- Send PDF resolution order (`startPDFDelivery`, `background.ts:4990`):
  `findByTab(tab_id)` → `deliveryJobForOpener` → `deliveryJobForDOI` (exact
  normalized equality, unique match only) → pin (tab-matched
  `uniqueManualDeliveryTarget`, `:5022`) → `manualDeliveryTarget(job_id)`
  (`:5026` — the job_id hint is **pin-gated**, deliberately: "a wider shape
  could let stale autonomous authority borrow a later popup click",
  `:4926`). Matched → direct delivery into `papio/<job_id>/`; unmatched +
  no DOI + `grabSupported()` → `pdf_grab_request {host, title}`.
- The pin is dual-purpose: inbox pre-pin AND the `waiting_manual`
  native-viewer continuation `startPDFDelivery` sets for
  `requiresNativeViewerDownload` URLs.
- The daemon grab sweep is deliberately blind: `processSettledGrab`
  (`bridge.go:7229`) → `pdf.FrontMatterDOIs` (1 KiB window, conclusive DOIs
  only, `identity.go:703-717`). No DOI → `MarkParkedNoIdentifier` → triage
  `KindPdfGrab` row, ops `[provide_identifier, dismiss]`.
- Grab bind durability order (`createGrabJob`, `bridge.go:7322`): copy →
  `MarkJobCreated` durably first (rollback on failure) → cleanup →
  `ingestAdoptedFile`; ingest failure → `recordAdoptionDeferred`.
- `MatchIdentityWithThreshold` (`identity.go:85`) properties that bound any
  reuse for 1-of-N selection (all verified):
  - Zero-author targets short-circuit author evidence (`:242`).
  - Whole-text identifier corroboration + `authorOK` returns `Pass`
    *before* `yearConflict`/`titlePrinted` are enforced (`:272-275` vs.
    `:300-309`).
  - `yearConflict` is only set when `matches < len(tokens)` (`:260`) — an
    exact printed title defeats it.
  - A conclusive foreign DOI only contradicts when the target itself has a
    DOI (`wantDOI != ""` gate, `:150`). **Ordinary validation therefore
    does NOT implement the veto below for DOI-less jobs; it must be added
    daemon-side.**
- Identity-comparison DOI normalization: `identity.go:624-639` applies
  `work.NormalizeDOI` then collapses the legacy doubled suffix slash *for
  identity comparison only*. Reuse this function; never mint a fourth
  equivalence relation.
- "Page one" has one executable meaning: `identityPageOne`
  (`identity.go:767-778`) — first non-blank extracted page, form-feed
  delimited, capped at 4 KiB.
- `corroboratingIdentifier` (`identity.go:899-926`): target-aware search;
  17 of 40 real papers print their identifier outside the 1 KiB window.
  Excerpt cap 16 KiB.
- Grab outcome/state enums are closed in both validators; `job_created`
  already exists as an unsolicited disposition the extension handles.
- **Extension manifest does NOT declare `webNavigation`**
  (`manifest.json:17-26`) — the page-identity design below adds it (root
  manifest; `firefox/manifest.json` is generated).
- **`ActiveJob` carries no manual-download-action projection**
  (`state.ts:14-15,139-153`): `awaiting_download` does not distinguish
  manual rows. Extension-side candidate lists are therefore advisory;
  the authoritative predicate runs daemon-side (below).
- Firefox: no download steering → grab transport absent; the Send-PDF-time
  path is the only Firefox story.
- **All bytes→artifact promotions converge on ONE function**:
  `Service.validateCandidate` (`internal/app/app.go:2973`). Direct
  user-picked delivery (`ingestAdoptedFile` → `adopt` →
  `AdoptDownload`), settled-grab bind (`createGrabJob`/`IdentifyGrab`),
  the adoption sweep (`SweepAdoptions`), and resolver-fetched bytes all
  route through it. The veto therefore needs one insertion point, not
  three; a bridge-only veto would leave the resolver path unguarded.
- **`awaiting_download` does not exist daemon-side.** It is an
  extension-local `JobStatus` (`state.ts:14`). The daemon equivalent is
  `job.StateAwaitingHuman` (`internal/job/job.go:35`) plus an open
  `human_actions` row of kind `manual_download`; `Terminal()`
  (`job.go:195`) defines live. There is no store query filtering human
  actions by kind — `ListOpenHumanActionsForJobs` (`job.go:3440`)
  returns rows to filter in Go, and it does not project `jobs.state`.
- **`verify_identity` is a free-form `human_actions.kind` literal**, not
  a `job.TerminalReason` and not a declared action-kind constant; the
  park destination is `job.StateNeedsReview` via `OpenHumanAction`
  (`job.go:2646`) with `WithHumanActionBinding`. `ReviewOverride` on the
  candidate bypasses the neighbouring identity-review arm.
- **The submission anchor is the durable equivalence source the veto
  needs**: `Store.SubmittedIdentity` (`internal/job/submitted_identity.go:8`)
  returns `Work` + `Attested` + `Identifiers` (each with provenance
  `submitted`/`verified`), loaded unconditionally at `app.go:2981`
  BEFORE validation, and `validationTarget(anchor, row)`
  (`identity_promotion.go:153`) already prefers the attested anchor over
  the mutable `row.Work`. No runtime resolver lookup is required.
- **Every identity helper the predicate needs is unexported** —
  `normalizeDOI` (:624), `identityFrontMatter` (:745), `identityPageOne`
  (:778), `corroboratingIdentifier` (:905), `titlePrintedAsLine` (:424),
  `documentDOIs`. Only `FrontMatterDOIs`, `IdentityWindows`,
  `IdentifierPrinted`, `MatchIdentity`, `MatchIdentityWithThreshold` are
  exported. Consequence: the veto and the candidate-binding predicate
  MUST live inside `internal/pdf`, not in `internal/browser`/`internal/app`.
- Store facts: the table is `pdf_grabs` (`migrations/0025_pdf_grabs.sql`,
  ALTERed once by `0034`); highest migration is `0036`, so
  `user_version` is **36** and a provenance column is **0037**. Grab
  transitions hand-roll `BeginTx(ctx, nil)` + CAS `UPDATE … WHERE id = ?
  AND state IN (…)` + `requireOneRow`; there is no `WithTx` helper, no
  `BEGIN IMMEDIATE`, and serialization comes from
  `db.SetMaxOpenConns(1)` (`store.go:54`) on a WAL DSN.

## Shared definitions (used by every phase)

**Candidate eligibility predicate (single source of truth, enforced
daemon-side).** A job is candidate-eligible iff it is live
(`!job.Terminal(state)`), is in `job.StateAwaitingHuman`, and has a
currently open `human_actions` row of kind `manual_download`. The
extension's `awaiting_download` is the local projection of that pair and
has no daemon column. The daemon enforces the predicate at pre-accept
validation (Phase 1) and pool construction (Phase 3). The
extension renders an *advisory* candidate list from the state it already
holds (`awaiting_download` activeJobs, intersected with triage
manual-download rows when loaded); render-time eligibility is never
authority. Widening the predicate is an explicit change, never a
status-only widening.

**Conclusive-identity veto (daemon-side pre-accept invariant; applies to
autonomous binds, human confirms, and picker deliveries alike).** Enforced
in ordinary candidate validation before an artifact becomes a job's
accepted PDF — the extension never inspects bytes, so this CANNOT live
extension-side (ADR-0020 keeps PDF bytes off native messaging). Let `D` =
the conclusive DOI set derived from the immutable extraction with the
exact `pdf.FrontMatterDOIs` semantics — the 1 KiB blind window, NEVER
whole-document and NOT `identityPageOne`: blind conclusive naming and
target-aware corroboration answer different questions, and widening the
blind window is a new veto-rule version with its own gate, decided from
Phase 2 data, not folded in silently.
- `|D| = 0`: no veto.
- `|D| > 1`: park `verify_identity`.
- `|D| = 1`: compatible iff the job's DOI equals it under the identity
  comparison normalization (`identity.go`'s `normalizeDOI`), or the job's
  own **submission-time recorded metadata** already binds that DOI to the
  job. **No runtime resolver lookup in v1** — absent durable recorded
  equivalence, park `verify_identity`, even for a human pick. A
  PMID/arXiv-only job without recorded DOI equivalence parks. Title/author
  similarity never manufactures DOI equivalence.

`ReviewOverride` (set only by an explicit human review of the
quarantined preview, ADR-0002) still overrides the veto, exactly as it
overrides the neighbouring identity-review arm — otherwise a legitimately
mismatched document (a chapter DOI against a book job) would be
unacceptable forever, which is a worse failure than the one the veto
prevents. A job *selection* never sets `ReviewOverride`, so picks stay
gated. Correlation evidence is not review authority; review authority is.

Regression case that MUST exist (it is the veto's reason for being):
DOI-less job J; picked PDF has J's exact printed title/authors, a
different year, and its own foreign front-matter DOI → park, never file.

## Design

Ordered by delivery; earlier phases are useful without later ones.

### Phase 1 — popup picker + daemon pre-accept validation

Directly replaces the Open step at the moment intent exists; the only
phase that works on Firefox. Extension work plus a bounded daemon change
(the shared veto + eligibility check in candidate validation); **no
picker-specific wire provenance and no protocol change.**

- When `startPDFDelivery` finds no exact correlation and the PDF is
  DOI-less, the popup offers "Which paper is this?" over the advisory
  candidate list.
- **One-shot correlation state machine, MV3-lifecycle-pinned:**
  - **Mint (volatile).** The background mints an opaque interaction nonce
    when it offers choices, held in service-worker memory ONLY. SW death
    invalidates the offer; a selection against a dead worker gets "stale
    interaction" and re-mints. Unconsumed offers are never persisted.
  - **Consume (atomic).** Selection looks the nonce up and deletes it in
    one synchronous statement pair, with no `await` between the read and
    the delete — that adjacency, not the absence of any earlier `await`,
    is what makes the nonce one-shot: JS is single-threaded, so two
    concurrent accepts cannot both observe it. (An earlier draft of this
    plan said "before the first `await`", and the shipped code does await
    `this.ready` first; the wording was wrong, not the code.) A crash
    before conversion fails closed. At acceptance the background
    revalidates the advisory facts and the frozen page identity, and
    every refusal path emits `code: "choice_expired"` so callers classify
    on the code rather than on user-facing copy.
  - **Direct delivery:** no picker authority survives; the existing
    durable pending-delivery machinery owns the operation from here.
  - **Convert (session-persistent).** A `requiresNativeViewerDownload`
    URL atomically converts the nonce into the `waiting_manual`
    continuation — the ONE authority-bearing state that must survive SW
    restarts (the viewer Download click can come after arbitrary worker
    sleep). It lives in `chrome.storage.session`: survives SW restart,
    dies with the browser session/extension reload. Nonce and
    continuation never coexist.
  - **Page identity.** `{tabId, documentId, sameDocumentNavigationSeq,
    resolvedSourceURL}`: `webNavigation.documentId` is unique per loaded
    document; the session-stored sequence advances on top-frame
    `onHistoryStateUpdated`/`onReferenceFragmentUpdated`; `onTabReplaced`
    invalidates. Requires adding the `webNavigation` permission to the
    root manifest. Qualifying same-document navigation is DEFINED as
    these browser-observable events — an SPA that swaps its logical
    document with no URL/history change has no generic signal; anything
    finer is provider-specific observation, out of scope.
  - **Destroy.** On every failed/cancelled start (including the
    `delivery_busy` early return), job ineligibility, tab closure,
    qualifying navigation, `onTabReplaced`, browser-session end, or
    pending-delivery terminal state. A later page in the same `tab_id`
    never revives authority.
  - **Re-verify.** The continuation revalidates job eligibility and page
    identity when the viewer download is claimed; the daemon's
    pre-accept validation (eligibility predicate + veto) remains the
    authoritative gate on the delivered bytes.
- Cleanup in this phase: delete `requestManualDownloadOpen` binding
  semantics (inbox "Open" becomes a plain link), the inbox pre-pin, and
  `uniqueManualDeliveryTarget`'s cross-tab ambient authority. The blind
  grab → `papio grabs identify` fallback stays intact.

### Phase 2 — selection-gate harness (blocking Phase 3)

Hard-negative false-pass testing establishes safety; backlog replays
establish coverage/abstention; random N-draws are secondary. Three
layers; ship Phase 3 only at zero wrong-accepts across layers 1+2.

1. **Blocking semantic fixtures** — labeled extracted-document text per
   risk family: same-author near-title; exact-title/different-year;
   citing sequel; correction/comment/retraction; edition/numbered series;
   conference vs. journal distinct works; preprint vs. VoR; plus
   metadata-ablation variants (author-less, year-less targets). The
   manifest carries ground-truth canonical-equivalence labels: same-work
   version pairs measure conservative abstention, not wrong-accept.
   Two veto-window cases: correct document with a foreign DOI printed at
   1–4 KiB; wrong document whose own DOI sits at 1–4 KiB while the
   target's identifier is cited on page one (measures the 1 KiB `D`
   residual before any widening decision).
2. **Extractor sentinels (~8–10 real PDFs) with an extraction-artifact
   coverage matrix**, not a count: ligature; hyphenated title wrap;
   multi-line title segmentation; two-column/wide-gap line gluing;
   author affiliation-marker glue; plus the window cases (blank cover
   leaf; own identifier at 2–4 KiB; identifier only after first form
   feed; dense no-form-feed page with identifier past the 4 KiB cap).
   Cases may overlap. Each sentinel asserts the final predicate verdict
   AND golden substrings of the extracted text — hand-authored layer-1
   text drifts from real `pdftotext` output (ligatures, hyphenation,
   line order are exactly what `identity.go` accumulated special
   handling for); the sentinels pin that boundary. Note the assertion is
   `strings.Contains` over chosen fragments, not an exact whole-document
   snapshot: it catches the extraction artifacts these cases exist for
   and tolerates unrelated layout churn, so a wholesale extractor change
   could still pass. An exact snapshot per sentinel would be stronger and
   is the obvious upgrade if the extractor is ever swapped.
3. **Real backlog replay — DEFERRED, not shipped.** Locally extracted
   PDFs + historical manual-download metadata for coverage, multiple-pass
   abstention, and true-target-absent behaviour. Report contents never
   leave the machine. This layer was never built: no replay harness or
   backlog corpus exists in the tree. Layers 1+2 are what authorized
   Phase 3 (this section's own gate is "zero wrong-accepts across layers
   1+2"), so the safety condition was met as written — but coverage over
   the operator's real library is therefore UNMEASURED. What is unknown
   without it: how often auto-bind actually fires on the real backlog,
   whether multi-candidate abstention is common enough to make Phase 4
   worth building, and the true-absent rate. Build this before widening
   the rule or claiming a coverage number.

### Phase 3 — daemon auto-bind on `candidate_auto_bind` (gated)

When a settled grab has no conclusive front-matter DOI, score against the
candidate-eligible pool. Auto-bind requires ALL of:

- Real author evidence — the zero-author shortcut (`identity.go:242`)
  does not qualify.
- Exact printed title (`titlePrintedAsLine`) unconditionally — the early
  corroboration return (`:272`) does not qualify on its own.
- **Candidate-binding year predicate** (NOT `MatchIdentity.yearConflict`):
  target yearless or own-byline window yearless → neutral; target year
  present in the window → compatible; window exposes year(s), none equal
  → not auto-bindable, regardless of exact title.
- Identifier corroboration evaluated over
  `identityPageOne(report.Text.Excerpt)` — one notion of page one.
  Changing the bound is a new predicate version and reruns the gate.
- Exactly one candidate qualifies; a second qualifier or any `Review`
  demotes to suggestion-only parking.
- Conclusive-identity veto (shared definition).

Mechanics:

- **Transactional bind.** One `bindGrabToExistingJob` used by Phases 3–4:
  revalidate → copy → `MarkJobCreated` CAS → cleanup → `ingestAdoptedFile`
  (failure → `recordAdoptionDeferred`), mirroring `createGrabJob`.
  Ingestion never precedes durable grab ownership.
- **Uniqueness fence: serialized final recompute — no generation
  counter.** Stage copy → begin the serialized write transaction →
  enumerate candidate-eligible rows from authoritative DB state →
  recompute every qualification against the immutable excerpt → require
  exactly one qualifier and no Review → CAS grab to `job_created` in that
  same transaction → commit. Result no longer uniquely the staged job →
  abort, delete staged destination, retry/park. Single-writer SQLite
  makes this sufficient; a generation column adds mutator burden and
  false retries for nothing.
- **Audit provenance: one store migration, named.** Nullable structured
  provenance column on `pdf_grabs` (JSON: `method=candidate_auto_bind`,
  predicate/rule version, qualification evidence, candidates considered,
  unique winner), written **in the same transaction as `MarkJobCreated`**.
  Outward outcome stays `job_created` — the method is never encoded in
  the wire outcome. The column lands as migration `0037`, which bumps
  `user_version` 36 → 37 and therefore **six** hardcoded literals, not
  four: `internal/cli/clean_install_test.go:102` and `:130-131`,
  `internal/doctor/doctor_test.go:78`,
  `internal/store/migrate_forward_test.go:203-204` and `:276-277`, plus
  `internal/store/migrate_guard_test.go:62-63`, whose message pins BOTH
  the refused future version (37 → 38) and the supported one (36 → 37).
- No wire change.

### Phase 4 — ranked one-click confirm (only if parked-grab burden persists)

Deliberately last; justified only by observed volume of parked grabs
surviving Phases 1+3.

- Suggestions (≤3, `IdentityDecision.Evidence` strings) persisted at park
  time as decision artifacts; re-validated at bind time through the same
  `bindGrabToExistingJob` fence (suggestion is a hint, never authority).
- Conclusive-identity veto applies to the human confirm.
- Surface: triage schema bump + `bind_candidate` op + RPC + `papio grabs
  bind` CLI. Footguns: hello-ack exact-feature-list assertion if a feature
  flag is added; `FieldSpec` exhaustiveness in `protocol.ts`; three
  protocol artifacts in one commit; `commandClassification` registry;
  `cmd/docs-gen` regen + drift tests.

## ADR check

- **ADR-0020 — requires in-place amendment, stated honestly.** Candidate
  binding IS a new autonomous acceptance-affecting route. Amendment
  language (adapted from review round 1):

  > Blind capture never creates or names a work from title evidence. A
  > capture lacking a conclusive blind identifier may be correlated with an
  > already-established job, but that correlation is itself an identity
  > decision. Automatic correlation must satisfy the separately specified
  > candidate-binding acceptance rule (`candidate_auto_bind`) and must
  > abstain on ambiguity or contradictory identity evidence. A human job
  > selection supplies correlation evidence, not authority to override
  > conclusive document identity; ordinary candidate validation remains
  > mandatory before the artifact becomes the job's accepted PDF.

- **ADR-0019 (title-only stance) — holds.** No phase creates a work from
  title evidence.
- **ADR-0010 (dedupe) — holds.** No new job-creation path.
- **ADR-0017 (manual_download generic) — holds.** The action queue is read
  as a matching pool for inbound bytes; nothing auto-opens or drives it.
- **ADR-0022 (effect permits) — holds.** Post-settlement bytes or existing
  user-click delivery; no new irreversible effect.
- **ADR-0001/0023 (triage) — additive** (Phase 4 only).
- **ADR-0002 — untouched;** grab preview remains out-of-scope follow-up.

## Review traceability

Round 1 (10 findings): strict predicate over IdentityPass; pre-bind
identity precedence; one-shot authority replacing the pin-gated hint;
native-viewer continuation retained; durable-ownership-first bind order;
uniqueness fence; hard-negative gate; honest ADR amendment; picker-first
phasing; auto-bind provenance.

Round 2 (7 findings): correlation state machine; fence atomic with the
grab CAS; function-level veto semantics; candidate-binding year
predicate; page one = `identityPageOne`; executable three-layer corpus;
shared eligibility predicate.

Round 3 (6 findings, all adopted after verification): Phase 1 retitled —
the veto is daemon-side pre-accept validation, never extension-side
(bytes live daemon-side; `wantDOI` gate means ordinary validation lacks
the rule; regression case pinned); MV3 authority lifetime pinned
(volatile nonce / synchronous consume / session-persistent continuation /
`documentId`-based page identity; `webNavigation` manifest addition
verified missing); dual windows kept deliberately (1 KiB blind `D`,
4 KiB target-aware corroboration) with widening as a gated rule version;
generation counter deleted in favor of serialized final recompute;
provenance column on `pdf_grabs` named, same-transaction, migration
listed; layer-2 coverage matrix over extraction artifacts. Verified
additions: `ActiveJob` has no manual-action projection → advisory
extension list + authoritative daemon predicate.

## Open questions

- Phase 4 `bound_job` vs. reusing `job_created` for the human-confirmed
  outcome — both enums are closed; default is reuse.
