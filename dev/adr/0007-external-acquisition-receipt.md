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
- **`unknown` is a real value, not a default — and papio does not honour this
  yet.** The promise stands as the contract; the code does not currently meet it,
  and closing that gap is a **prerequisite** of the receipt, not a follow-up.
  Browser adoption assigns `published` to a file nobody inspected and, worse,
  copies the request's `desired_version` *preference* into the obtained *fact*
  (`internal/app/browser_adopt.go:93-96`) — on the institutional path, which is
  the only path this consumer uses papio for. That must become `unknown`:
  adoption observes bytes arriving, never which version a human chose.
  Two other inferences are defensible and stay: arXiv never claims `published`
  and downgrades to `accepted` on a DOI/journal-ref (`arxiv.go:154-162`), and
  Europe PMC's OA subset is the version of record by definition
  (`europepmc.go:176`). Both err toward suppressing an adverse finding, which is
  the safe direction. (The claim in an earlier draft that arXiv emits `unknown`
  was simply wrong.)
- **The receipt is not a pure projection, and the "two new items" estimate was
  wrong.** `SelectedCandidateID` is not an accepted-candidate pointer: it is
  written before validation (`internal/app/app.go:776-777`), survives rejection
  because the transition SQL `COALESCE`s it forward (`internal/job/job.go:557-558`),
  and is zero for the cache-hit path, which reaches `ready` with only the artifact
  hash (`internal/app/app.go:299-300`). So a *failed* receipt projecting it would
  report the last **rejected** file's licence and version — the precise error class
  this ADR exists to prevent. Two consequences, both binding:
  **(a)** the receipt's candidate block is **absent unless an acquisition was
  accepted**; it is never inferred from an attempt.
  **(b)** the cache-hit path must carry the source acquisition's candidate
  forward. Today the exporter recovers it by scanning for the *earliest* ready job
  sharing that hash (`internal/job/job.go:1487-1490`), which borrows another job's
  `access_basis` and `reuse_license` whenever identical bytes were obtained under
  different terms. That is first-writer-wins rights attribution on a content hash
  — the artifact-stamping error this ADR rejects, already live in the surface the
  receipt depends on. Fix it before the receipt reads it.
- **Identity findings cannot be projected from the artifact either.** Identity is
  computed against a per-job target (`internal/pdf/pipeline.go:64-65`) but
  `UpsertArtifact`'s `ON CONFLICT … DO UPDATE SET identity_result` overwrites it
  for every job sharing the digest (`internal/job/job.go:1453-1456`), so a later
  acquisition retroactively rewrites an earlier receipt. Identity belongs on the
  acquisition edge, not the blob. Structured `pdf.ValidationReport` `Evidence`
  stays **withdrawn**; note also that no artifact row exists for a failed
  acquisition (`internal/app/app.go:933-937` deletes the file and keeps only a
  coarse attempt detail), so failed receipts carry a coarse reason, not findings.
  Consumers may describe papio's validation *findings*; they may not claim to
  relay a full report.
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
  not a store change.** Roles bind to the acquisition *and its candidate*: the
  same bytes may be `main` in one job and `supplement` in another, so a
  `(job, artifact, role)` association without a candidate would falsify provenance
  as soon as components come from different candidates. The premise that resolvers
  already emit components is **false**: Europe PMC emits an HTML candidate only
  when no PDF candidate survived (`internal/resolvers/europepmc/europepmc.go:195`),
  candidates are *alternatives* — the first accepted one transitions the job
  straight to `ready` (`internal/app/app.go:1017-1024`) — and the artifact store
  hardcodes `<sha>.pdf` (`internal/artifact/artifact.go:52-65`). Components
  therefore need resolver semantics, an aggregation state before `ready`, and
  non-PDF content-addressed storage plus validation before any schema helps.
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
   fact. Regression: `TestAdoptedCandidateVersionIsAlwaysUnknown` asserts `unknown`
   across every `desired_version`, and fails on the old code with the request
   echoed back (`accepted` in, `accepted` out).
2. **Provenance was resolved by content hash, borrowing another job's candidate.**
   **FIXED** — `Store.CandidateForArtifact(ctx, jobID, sha)` prefers the job's own
   accepted candidate and keeps the hash scan only for cache-completed jobs, which
   have no candidate of their own. Regressions:
   `TestExportReportsThisJobsProvenanceNotAnotherJobsSharingTheArtifact` (fails on
   the old lookup with `open_access` reported for an institutional acquisition) and
   `TestExportFallsBackToOriginalAcquisitionForCacheCompletedJob`, which pins the
   fallback so it is not "simplified" away later.
3. **`identity_result` is last-writer-wins across jobs sharing a digest**
   (`internal/job/job.go` `UpsertArtifact`'s `ON CONFLICT … DO UPDATE`). **OPEN.**
   Identity is computed against a per-job target, so a later acquisition rewrites
   an earlier one's finding. Move it to the acquisition edge. Not yet load-bearing:
   nothing published today reads it as a per-acquisition fact — the receipt would
   be the first, so this must land before the receipt does, not before the contract
   is agreed.

Items 1 and 2 were live defects in shipped output, independent of any consumer:
item 2 mis-attributed reuse licences in exported bundles.
