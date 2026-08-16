# Send PDF without the inbox "Open" step: candidate binding for DOI-less PDFs

Status: plan v2 (2026-08-16). Revised after oracle review (GPT-5.6 Sol,
verdict REVISE; session `review-request-plan-for-eliminatin`, answer in
`dev/scratch/oracle/20260816T040838Z-send-pdf-binding-plan/`). All
code-level review claims were verified against source before adoption.
Work in flight; salvage normative content into the ADR-0020 amendment on
ship, then delete this file.

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
- The pin is dual-purpose. Besides the inbox pre-pin, `startPDFDelivery`
  itself sets it for `requiresNativeViewerDownload` URLs (the
  `waiting_manual` flow): the extension cannot fetch those bytes, so the pin
  is the durable link between the picked job and the *later* viewer
  Download click. Deleting all pin state breaks native-viewer delivery.
- The daemon grab sweep is deliberately blind: `processSettledGrab`
  (`bridge.go:7229`) → `pdf.FrontMatterDOIs` (1 KiB window, conclusive DOIs
  only, `identity.go:703-717`). No DOI → `MarkParkedNoIdentifier` → triage
  `KindPdfGrab` row, ops `[provide_identifier, dismiss]`.
- Grab bind durability order (`createGrabJob`, `bridge.go:7322`): copy →
  `MarkJobCreated` durably first (rollback `os.Remove(dest)` on failure) →
  cleanup → `ingestAdoptedFile`; ingest failure →
  `recordAdoptionDeferred`. Ingestion never precedes durable grab
  ownership.
- `MatchIdentityWithThreshold` (`identity.go:85`) properties that bound any
  reuse for 1-of-N selection (all verified):
  - Zero-author targets short-circuit author evidence:
    `authorOK := len(target.Authors) == 0 || …` (`:242`).
  - Whole-text identifier corroboration + `authorOK` returns `Pass`
    *before* `yearConflict` and `titlePrinted` are enforced (`:272-275`
    vs. the switch at `:300-309`).
  - The year guard never fires when all significant title tokens match
    (`matches < len(tokens)` conjunct, `:260`).
  - A conclusive foreign DOI only contradicts when the target itself has a
    DOI (`wantDOI != ""` gate, `:150`); DOI-less targets never see it.
- `corroboratingIdentifier` (`identity.go:899-926`) searches the whole
  supplied text for a KNOWN target's DOI/arXiv/PMID; 17 of 40 real papers
  print their identifier outside the 1 KiB front-matter window. Excerpt cap
  is 16 KiB (`semantic.go:36`).
- Wrong picks in the delivery path are backstopped by `ingestAdoptedFile` →
  `validateCandidate` → identity mismatch parks `verify_identity` — but per
  the `wantDOI` gate above this backstop is **not** sufficient to enforce
  "extracted identity outranks the pick" for DOI-less target metadata.
- Grab outcome/state enums are closed in both validators
  (`protocol.go:3891-3963`, `protocol.ts:949-978`); `job_created` already
  exists as an unsolicited disposition the extension handles.
- Firefox: no download steering → `grabSupported()` false → the grab
  transport does not exist there. Any Firefox story must live in the
  Send-PDF-time path.

## Design

Ordered by delivery; earlier phases are useful without later ones.

### Phase 1 — popup picker + scoped post-send correlation (extension-only)

Directly replaces the Open step at the moment intent exists, and is the
only tier that works on Firefox.

- When `startPDFDelivery` finds no exact correlation and the PDF is
  DOI-less, the popup offers "Which paper is this?" over the locally-held
  `activeJobs` rows with `status: awaiting_download`.
- **New authority primitive, not the pin-gated hint.** The pick mints a
  one-shot correlation scoped to: this Send PDF interaction, the current
  tab, and a currently eligible `awaiting_download` job. It is consumed by
  this delivery (or its `waiting_manual` continuation) and expires when the
  pending delivery terminates. It is never an ambient reusable pin, and
  `payload.job_id` never becomes unrestricted authority — the narrowness
  the `:4926` comment defends is preserved by construction.
- **Native-viewer continuation kept.** For `requiresNativeViewerDownload`
  URLs the same one-shot correlation (job + tab/URL evidence +
  `pendingDelivery: waiting_manual`) carries the association to the later
  viewer Download click. This replaces the pin's second job; only the
  inbox-created *pre*-pin semantics are deleted.
- **Pre-bind identity precedence (also applies here):** if the delivered
  bytes print a conclusive DOI set, the picked job must be canonically
  compatible with it (re-extract from the immutable bytes at bind time;
  DOI-less job metadata does not get a pass via the `wantDOI` gate).
  Incompatible or multiple conclusive identities → park `verify_identity`,
  never silent accept.
- Cleanup in this phase: delete `requestManualDownloadOpen` binding
  semantics (inbox "Open" becomes a plain link), the inbox pre-pin path,
  and `uniqueManualDeliveryTarget`'s cross-tab ambient authority. The blind
  grab → `papio grabs identify` fallback stays intact.

### Phase 2 — selection-gate harness (blocking any autonomous binding)

Two-part gate, replacing the v1 plan's random-N-draw experiment (which
cannot reveal new false passes if the pairwise corpus is already zero —
uniqueness only suppresses existing passes):

1. **Hard-negative false-pass testing of the selection predicate** (not of
   plain `MatchIdentity`): same-author near titles, exact-title/
   different-year works, preprint vs. VoR, conference vs. journal versions,
   corrections/comments/retractions, editions/numbered series, and the
   citing-sequel shape (a later paper by the same group citing the
   target's DOI in its introduction). Include metadata-ablation cases
   (author-less, year-less targets) matching real `manual_download` row
   missingness — the corpus README already documents that a real library
   cannot measure the erratum shape and needs hand-built negatives.
2. **Realistic backlog replays** for coverage, multiple-pass abstention
   rate, and true-target-absent behaviour (backlogs are topically
   correlated, same-field, overlapping-author sets — not random library
   draws). Random N-sets remain a secondary metric only.

Ship Phase 3 only if the predicate holds zero wrong-accepts across part 1.
Reports name the operator's library; never paste them anywhere.

### Phase 3 — daemon auto-bind on a strict selection predicate (gated)

When a settled grab has no conclusive front-matter DOI, score against the
candidate pool (jobs with open `manual_download` actions). Auto-bind
requires `candidate_auto_bind`, a strictly narrower predicate than
`IdentityPass`:

- Actual author evidence required — the zero-author shortcut
  (`identity.go:242`) does not qualify.
- Exact printed title (`titlePrintedAsLine`) and no year conflict required
  unconditionally — the early corroboration return (`:272`) does not
  qualify on its own.
- Identifier corroboration counts only within a defensible own-identity
  window (page one), not the whole 16 KiB excerpt — a sequel citing the
  target's DOI in its introduction must not corroborate.
- Exactly one candidate qualifies; a second qualifier, or any candidate at
  `Review`, demotes to suggestion-only parking.
- Conclusive-identity precedence as a pre-bind rule (as in Phase 1): any
  conclusive DOI in the immutable bytes must be canonically compatible
  with the selected job, independent of whether the job's metadata carries
  a DOI.

Mechanics:

- **Transactional bind, existing durability order.** One
  `bindGrabToExistingJob` used by Phases 3–4: revalidate → copy →
  `MarkJobCreated` (CAS from the exact expected grab state) → cleanup →
  `ingestAdoptedFile` (failure → `recordAdoptionDeferred`), mirroring
  `createGrabJob:7385-7394`. Ingestion never precedes durable grab
  ownership.
- **Uniqueness fence.** Score against a versioned snapshot of the eligible
  action/job set; immediately before commit, reload grab, target job,
  current `Row.Work`, and the manual action, recompute the decision, and
  restart if the set or metadata changed. `canonicalJobStatus` is one
  component of the fence, not the fence.
- **Audit provenance.** Persist with the binding: `candidate_auto_bind`
  disposition, matcher/rule version, the structured evidence that
  authorized it, and candidate-set provenance (which jobs were considered
  and why the pass was unique). The case that must be reconstructable is
  the rare autonomous success that later proves wrong; durable audit plus
  a re-verification path, not a transactional undo system.
- No wire change: reuse the `job_created` unsolicited outcome.

### Phase 4 — ranked one-click confirm (only if parked-grab burden persists)

Deliberately last: it adds persisted suggestion state, a triage schema
bump, a `bind_candidate` op, an RPC, a cobra command (`papio grabs bind`),
inbox UI, and conformance/docs work — justified only by observed volume of
parked grabs surviving Phases 1+3, not load-bearing for eliminating the
Open step.

- Suggestions (≤3, with `IdentityDecision.Evidence` strings) persisted at
  park time as decision artifacts; re-validated at bind time through the
  same `bindGrabToExistingJob` fence (reload + recompute + CAS — the
  suggestion is a hint, never authority).
- Same pre-bind conclusive-identity precedence rule.
- Footguns: hello-ack exact-feature-list assertion if a feature flag is
  added; `FieldSpec` exhaustiveness in `protocol.ts`; three protocol
  artifacts in one commit; `commandClassification` registry; `cmd/docs-gen`
  regen + drift tests.

## ADR check

- **ADR-0020 — requires in-place amendment, stated honestly.** Tier-style
  candidate binding IS a new autonomous acceptance-affecting route; the v1
  claim "no new acceptance path exists, only selection" understated the
  change. Amendment language (adapted from review):

  > Blind capture never creates or names a work from title evidence. A
  > capture lacking a conclusive blind identifier may be correlated with an
  > already-established job, but that correlation is itself an identity
  > decision. Automatic correlation must satisfy the separately specified
  > candidate-binding acceptance rule (`candidate_auto_bind`) and must
  > abstain on ambiguity or contradictory identity evidence. A human job
  > selection supplies correlation evidence, not authority to override
  > conclusive document identity; ordinary candidate validation remains
  > mandatory before the artifact becomes the job's accepted PDF.

  The amendment names the new route and documents its gate (Phase 2).
- **ADR-0019 (title-only stance) — holds.** No phase creates a work from
  title evidence.
- **ADR-0010 (dedupe) — holds.** No new job-creation path.
- **ADR-0017 (manual_download generic) — holds.** The action queue is read
  as a matching pool for inbound bytes; nothing auto-opens or drives it.
- **ADR-0022 (effect permits) — holds.** All phases operate
  post-settlement on bytes on disk or ride the existing user-click
  delivery; no new irreversible effect.
- **ADR-0001/0023 (triage) — additive** (Phase 4 only); rank bases and
  attention-not-a-sort-key preserved.
- **ADR-0002 — untouched;** grab preview remains an explicitly
  out-of-scope follow-up.

## Review findings adopted (traceability)

From the oracle REVISE verdict, all verified against source before
adoption: (1) IdentityPass is too weak for open-set 1-of-N → strict
`candidate_auto_bind` predicate; (2) `wantDOI` gate breaks the precedence
invariant → explicit pre-bind conclusive-identity rule; (3) the job_id
hint path is pin-gated authority → new one-shot correlation primitive;
(4) the pin's native-viewer continuation is load-bearing → scoped
`waiting_manual` correlation retained; (5) bind order must keep durable
grab ownership before ingestion; (6) uniqueness needs a snapshot + CAS
fence; (7) random-N corpus draws are insufficient → two-part gate;
(8) ADR amendment must name the new acceptance-affecting route; (9) phase
order inverted to picker-first, automation-gated-later, Tier 2 on
demonstrated burden; (10) auto-binds persist audit provenance.

## Open questions

- Own-identity window for corroboration in `candidate_auto_bind`: page one
  (preferred, matches FrontMatterDOIs philosophy at a wider bound) vs. a
  fixed byte window — decide from Phase 2 part-1 data.
- Whether Phase 1's picker should also list `awaiting_download` jobs whose
  action arrived via routes other than manual_download rows; start with
  manual_download only.
- Phase 4 `bound_job` vs. reusing `job_created` for the human-confirmed
  outcome — both enums are closed; default is reuse.
