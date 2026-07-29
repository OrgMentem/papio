# ADR-0007: External acquisition receipt, and where provenance binds

Status: Accepted (2026-07-29), revised the same day after a plan review that
invalidated two of its own claims. Every correction is inline and attributed;
nothing was quietly deleted.

## Context

An external consumer (Inscribi, a local-first academic marking engine) needs to
read, once per completed work, *why* an acquisition ended as it did and *what
exactly* was obtained. Its use is adversarial in one direction that matters: it
will not emit a negative finding against a student ("this quotation is not in
the source") unless it can prove it checked the cited **version** — a quote from
the version of record is legitimately absent from the accepted manuscript. So
version, licence, and validation outcome are correctness inputs for the
consumer, not reporting garnish.

This is the first consumer to arrive through the seam ADR-0004 anticipated, and
the first to ask papio for provenance rather than for bytes. Its requests were
audited against HEAD; most of the original list was already shipped or was
rejected by ADR (handoff offers do not hard-expire, upheld). What remained
forced two questions with real design content: **where do acquisition facts
bind**, and **how do they cross the wire** given that nothing on the wire is
additive (`internal/ipc` `decodeStrict`/`decodeJSON` both
`DisallowUnknownFields()`, covering the `Response` envelope).

The consumer initially asked for version / `reuse_license` / `access_basis` to
be stamped onto the artifact. That request is wrong in an instructive way, which
is why this ADR exists rather than a changelog line.

## Options

### A. Stamp rights and version onto the artifact

Rejected, and this is the load-bearing rejection. `artifacts` is
content-addressed and shared across jobs (`UpsertArtifact` … `ON
CONFLICT(sha256)`, `internal/job/job.go`). The same bytes can legitimately be
reached by two candidates with different licences or access bases — an OA mirror
and an institutional copy of the identical file. Stamping rights on the hash
makes the last writer win and produces a **false rights record**: a consumer
gating retention or model-transfer on that field would be reading whichever
acquisition happened to run second. A digest identifies bytes; it cannot carry
the terms under which they were obtained.

### B. Widen existing results (`jobs.get`, `artifacts.get`)

Rejected by the wire rule. A widened result makes an older `papio` CLI reject
*every* response from a newer daemon, and one binary is CLI, daemon, and native
host, so skew is routine on any machine with two copies installed.

### C. Receipt as a distinct object, bound to the acquisition (accepted)

The facts already persist on the correct side of an existing join:
`job.Row.SelectedCandidateID` → `Candidate.Version` / `.AccessBasis` /
`.ReuseLicense`. Validation findings already persist on the artifact
(`PageCount`, `TextChars`, `OCRUsed`, `Encrypted`, `HasActiveContent`,
`IdentityResult`). So the receipt is a **projection**, not a migration.

## Decision

Option C.

- **Acquisition-provenance facts bind to the acquisition (job × selected
  candidate), never to the content-addressed blob.** Version, reuse licence,
  access basis, resolver tier, and acquiring principal are properties of *how
  these bytes were obtained*. The artifact store stays a pure byte store keyed
  by digest. No artifact schema change for rights or version — now or later.
- **`acquisition-bundle/1` already *is* the success receipt; nothing is added to
  it.** A plan review found the bundle already carries `candidate.version` as a
  typed enum (`published`/`accepted`/`preprint`/`unknown`), `candidate.access_basis`,
  `candidate.reuse_license`, the artifact digest/size/page/text/OCR facts, and a
  `validation` block (`protocol/acquisition-bundle-v1.schema.json:45-92`). A
  receipt object would have duplicated all of it — and could not have been added
  anyway: v1 sets `additionalProperties: false` and `DecodeAcquisitionBundle`
  applies `DisallowUnknownFields` recursively
  (`internal/protocol/protocol.go:58-69`), so the addition would have forced
  `acquisition-bundle/2` plus a `bundle.export_v2`. **The receipt is therefore a
  new IPC method only, for the states no bundle can describe**: failures, where
  the typed terminal reason lives. Bundle export is gated on `ready`/`imported`
  with a passing identity (`internal/bundle/export.go:82-104`), so this is not an
  edge case — it is half the consumer's use.
- **papio emits facts; the consumer judges identity.** papio does not model a
  `Manifestation`, does not decide whether a source package is complete, and
  does not decide whether an obtained version satisfies a citation. Two
  similar-but-different identity vocabularies across the boundary is the worst
  available outcome; papio declines to own one.
- **`unknown` is a real value, not a default.** papio does not synthesise a
  version and never echoes a request's `desired_version` *preference* back as an
  obtained *fact*. Browser adoption records `unknown` unconditionally
  (`internal/app/browser_adopt.go`): it observes bytes arriving from a human's
  browser and never learns which version that human chose. Because adoption keys
  its candidate by content hash under `INSERT OR IGNORE`, a row written before
  this rule existed is normalised on re-adoption and on review acceptance rather
  than re-read with its old claim.
  Two resolver inferences are defensible and stay: arXiv never claims `published`
  and downgrades to `accepted` on a DOI/journal-ref (`internal/resolvers/arxiv`),
  and Europe PMC's OA subset is the version of record by definition
  (`internal/resolvers/europepmc`). Both err toward suppressing an adverse
  finding, which is the safe direction. (An earlier draft of this ADR claimed
  arXiv emits `unknown`; it does not, and the drafting error was mine.)
- **The receipt is not a pure projection, and the "two new items" estimate was
  wrong.** `selected_candidate_id` is not an accepted-candidate pointer: it is
  written when a fetch starts, before validation, and the transition SQL
  `COALESCE`s it forward, so a job can carry a **rejected** selection through
  crash recovery or a scheduler retry. Reading provenance from it directly would
  publish the licence and version of a file papio threw away. Three rules follow,
  all now enforced in `internal/job`:
  **(a)** provenance is read only from a candidate whose status is `accepted` and
  which belongs to the job being described; the receipt's candidate block is
  absent unless an acquisition was accepted, never inferred from an attempt.
  **(b)** a job completing from the local cache records the **source
  acquisition's** accepted candidate, because these are that acquisition's bytes.
  The digest-keyed scan remains only as a fallback for rows written before that
  rule, and it too now requires an accepted candidate.
  **(c)** no provenance is ever recovered by picking "some job holding this
  digest" — that is first-writer-wins rights attribution on a content hash, the
  same error as stamping rights on the artifact.
- **Identity findings cannot be projected from the artifact.** Identity is
  computed against a per-job target, but `UpsertArtifact`'s
  `ON CONFLICT … DO UPDATE SET identity_result` overwrites it for every job
  sharing the digest, so a later acquisition retroactively rewrites an earlier
  receipt. Identity belongs on the acquisition edge, not the blob. Structured
  `pdf.ValidationReport` `Evidence` stays **withdrawn**; note also that no
  artifact row exists for a failed acquisition, so failed receipts carry a coarse
  terminal reason, not findings. Consumers may describe papio's validation
  *findings*; they may not claim to relay a full report.
- **List growth uses new methods carrying the `agentjson` `{name: [],
  truncated}` envelope**, never a widened existing result — but the envelope
  alone does not satisfy the consumer. `agentjson.Capped` documents its flag as
  deliberately "a 'may be more', not a proof" (`internal/agentjson/agentjson.go:98-101`),
  reasoning that no consumer needs certainty. A cohort-scale consumer does: it
  must distinguish a full page from a complete list. So the new methods must
  overfetch `limit+1` and report `truncated` as a fact, via `agentjson.Truncate`
  rather than `Capped`. That is a new obligation this ADR creates, not an existing
  guarantee.
- **No reopen machinery, and no generic repair verb.** The one
  consumer-unreachable state is an orphaned `awaiting_human` row (open action
  gone; `Retry` refuses parked jobs; `FocusHandoffs` requires both an open handoff
  action and `awaiting_human`). Expose the existing, tested `RepairAwaitingHuman`
  transition — but **scoped to the orphan case**, named for what it does
  (`jobs.repair_awaiting_human`), not as `reopen`. A verb taking arbitrary action
  ids would let a consumer close actions it never read; passing nil against a job
  that *does* have open actions correctly conflicts and rolls back
  (`internal/job/job.go:632-660`), which is the behaviour to preserve, not work
  around. The transactional lease predicate is load-bearing and must stay inside
  the verb (`internal/job/job.go:596-600`). No expiry, no relaxing `Retry`; ADR
  "handoff offers do not hard-expire" stands unchanged.
- **Multi-component artifacts bind the same way, and are the largest item here —
  not a store change.** Roles bind to the acquisition: the same bytes may be
  `main` in one job and `supplement` in another, so neither the role nor the
  identity finding can live on the shared `artifacts` row. `job_artifacts`
  (job, artifact, role, candidate, identity) is that edge, and it carries the
  per-acquisition identity finding as well, because that had exactly the same
  last-writer-wins defect.
  **`jobs.artifact_sha256` deliberately remains the main component.** It is read
  by the frozen `PAPIO_PDF`/`PAPIO_SHA256` hook contract (ADR-0004), by zotio
  attach, by the retraction sweep, and by bundle export; keeping it authoritative
  adds the capability without rewriting those paths, and an older binary opening a
  migrated database still reads a coherent main component.
  **What is deliberately NOT built:**
  *(i)* resolver-emitted components. The premise that resolvers already emit them
  is false — Europe PMC emits an HTML candidate only when no PDF candidate
  survived, and candidates are *alternatives*: the first accepted one ends the
  job. Emitting both would need component-aware ranking and an aggregation state
  before `ready`.
  *(ii)* `html_fulltext` storage, which is refused with a named error rather than
  accepted. Raw provider HTML is inherently active content, and the artifact
  store's integrity model assumes bounded, validated, inert files — the PDF
  pipeline exists to reject exactly what an HTML page is made of. Admitting it
  needs a sanitization design, and a role string is not one.
  So components are populated today through `AdoptComponent`: a human files a
  supplement or appendix from the job's adoption root, validated by the ordinary
  payload/structural gates. Identity is **not** asserted for a component — a
  supplement is usually not the article and carries neither its title nor its DOI,
  so requiring a match would reject every real supplement and asserting one would
  be a lie.
  Component roles remain facts; completeness is a judgement and stays with the
  consumer, which accepts `unknown` where papio cannot enumerate.

## Consequences

- The consumer's blocking question ("can we get the obtained version?") is
  answered for successful acquisitions by the existing bundle, with no new wire
  surface at all — but only once browser adoption stops synthesising the version.
  That prerequisite, not the receipt, is the critical path.
- The daemon-side work is smaller than the consumer's list implies and the
  correctness work is larger. Nothing here is deliverable by projection alone.
- Coverage claims made *about* papio by consumers turn out to need a correction
  this ADR should record, because it is not obvious from the registry: the axis
  that determines whether a provider downloads autonomously is **download method
  × browser**, not whether an adapter was verified live. On Firefox there is no
  `downloads.onDeterminingFilename`, so `click`-method adapters are excluded from
  autonomous download (`isFirefoxClickDownload`) and stay human-assisted, while
  `href`/`url`/`api`/`meta` adapters behave identically on both browsers. Anyone
  documenting papio's coverage must state that caveat and must not imply
  fixture-backed means unverified.

Scope tripwire: if the receipt grows a field that expresses a *judgement* —
completeness, citation match, version adequacy, rights permission rather than
the licence string — stop. That field belongs to the consumer, and its presence
here means papio has started to own meaning, which is out of scope permanently.

## Prerequisites

Three existing defects sit under this contract. They are not receipt features —
the receipt would simply publish them, which is why they are listed here rather
than in a backlog.

1. **Browser adoption synthesised the obtained version.** **FIXED** — adoption now
   always records `unknown` (`internal/app/browser_adopt.go`); the request's
   `desired_version` is a ranking preference and is never echoed as an obtained
   fact. A review of the first fix found it incomplete: adoption keys its candidate
   by content hash and `InsertCandidates` is `INSERT OR IGNORE`, so re-adopting the
   same bytes re-read the pre-existing row and its old `published` claim survived —
   as did a candidate parked for identity review before the fix. Both paths now
   normalise through `Store.MarkCandidateVersionUnobserved`, which is deliberately
   one-way: no setter can put a concrete version back, because no caller is
   entitled to invent one. Resolver candidates keep their source-reported version.
   Regressions: `TestAdoptedCandidateVersionIsAlwaysUnknown` (fails on the original
   code with the request echoed back — `accepted` in, `accepted` out) and
   `TestAdoptDownloadNormalizesAPreUpgradeSynthesizedVersion`.
2. **Provenance was resolved by content hash, borrowing another job's candidate.**
   **FIXED** — `Store.CandidateForArtifact(ctx, jobID, sha)` reads the job's own
   candidate, but only when it is `accepted` and belongs to that job; the
   digest-keyed scan is a fallback and also requires an accepted candidate. The
   same review found the first fix could newly prefer a *rejected* selection on a
   cache-completed job, and that the fallback reconstructed provenance from the
   earliest job holding the digest while `FindArtifactByDOI` had actually selected
   the newest — so the cache path now carries the source acquisition's candidate
   forward instead of leaving it to be guessed. Regressions:
   `TestExportReportsThisJobsProvenanceNotAnotherJobsSharingTheArtifact`,
   `TestExportIgnoresARejectedSelectionAndUsesTheAcceptedAcquisition` (fails
   without the accepted check by publishing `all-rights-reserved` from a rejected
   candidate), `TestLocalCacheCompletesWithoutResolverOrFetch`, and
   `TestExportFallsBackToOriginalAcquisitionForCacheCompletedJob`, which pins the
   fallback so it is not "simplified" away later.
3. **`identity_result` was last-writer-wins across jobs sharing a digest.**
   **FIXED** — identity now lives on the `job_artifacts` acquisition edge, written
   inside the same transaction that attaches the artifact, so it captures what
   *this* job's validation found. Bundle export reads it from there, and the
   shared `artifacts.identity_result` no longer decides whether a bundle is
   exportable. Regression: `TestExportIdentityIsNotRewrittenByALaterAcquisition`,
   which rewrites the shared artifact row to `reject` after the fact and asserts
   the bundle still reports the `pass` this acquisition recorded — it fails on the
   old read with the export refused outright.

Items 1 and 2 were live defects in shipped output, independent of any consumer:
item 2 mis-attributed reuse licences in exported bundles. Both were landed, then
reviewed, and the review found further defects in each — including one the first
fix introduced. Pre-fix bundles are **silently** wrong where they are wrong: the
`provenance_digest` signs whatever candidate block was written, so a bundle cannot
self-detect the mis-attribution.
