# Institutional processing acceleration

**Status:** Phase −1 complete; **Phase 0 current** (observation-only)  
**Ownering model:** solo-maintainer-sized changes landed directly on `main`  
**Scope:** daemon authority, browser materialization, institutional cutover, and
staged enablement. Existing UI work remains a dependency for typed attention
rendering, but does not move authority into the extension.

## Current rollout state

The current release keeps automatic first-route behaviour and source-gate
bypass disabled. The global browser-effect permit is one. Existing explicit
handoff and direct/generic paths remain the fallback behind their current
feature, access-mode, holder, and safety gates.

Phase 0 records decisions; it does not create candidates, open tabs, bypass a
source gate, or change concurrency. No provider tuple is broadly enabled. The
readiness stream continues independently; ScienceDirect has no `ready` claim in
this plan because no current live validated success plus adoption evidence is
being asserted.

The hard enablement chain is:

```text
Phase −1 stability
  → Phase 0 decision observability
  → durable state/protocol and recoverable materialization
  → effects and evidence correct at concurrency one
  → exact provider readiness
  → automatic first-route canary
  → source-gate canary
  → broad-at-one evidence
  → concurrency four
```

The chain is strict. Provider repair, cohort preparation, and fixture work may
run in parallel with Phases 0–4, but no phase may consume evidence from a later
phase or silently enable its behavior.

## Immediate first three changes

These are the first three direct-to-main changes. Each has one primary owner,
one bounded review surface, and a narrow rollback.

### Change 0.1 — Ratify the authority and identity contract

**Targets:** ADR-0022, this plan, and the curated architecture summary.  
**Purpose:** freeze the daemon/extension split, the three business identities,
holder generation, attempt/revision rules, typed gates, cooldown scopes,
suppression/winner rules, privacy inventory, and the −1–8 sequence before new
state or protocol is built.  
**Non-goals:** no runtime behavior change, no new protocol frame, no provider
qualification, and no source-gate bypass.

**Exit:** the three documents agree on terminology and all later changes can
cite one authority model without reopening it.

### Change 0.2 — Add transactional cutover classification

**Targets:** `internal/job/cutover.go` and the decisive processing transition.  
**Purpose:** normalize and persist exactly one closed cutover decision on the
same transaction as the `job.transition` that records it. The stable payload has
`institution_cutover_blocker` and `canary_ready_route_exists`; `none` is explicit.
Phase 0 conservatively records the route flag as false because no qualified-
route registry exists yet.  
**Non-goals:** no automatic institutional candidate, no source-gate bypass, no
new route registry, and no provider URL or credential in detail.

**Exit:** every decisive cutover path has one atomic, parse-enforced decision;
ordinary unrelated transitions do not acquire a fabricated decision.

### Change 0.3 — Add backward-compatible diagnosis v2

**Targets:** `jobs.diagnose_v2`, current CLI preference/fallback, and focused
contract tests.  
**Purpose:** project the latest valid transactional decision without changing
the byte shape of `jobs.diagnose_v1`. The CLI tries v2 first and falls back to
v1 only for a bounded unknown-method response.  
**Non-goals:** no widening of v1, no generic error fallback, no autonomous
repair, and no new acquisition decision.

**Exit:** old strict clients decode v1 exactly as before; new diagnosis can state
why the current decision was made without exposing URLs, credentials, local
paths, institution names, or work identifiers.

## Authority and identity contract

The daemon owns jobs, canonical work identity, policy/access mode, profile and
route revisions, candidate order, materialization/effect claims, cooldown
projections, suppression, artifact winners, durable transitions, and diagnosis.
The extension owns physical tabs/scaffolds/downloads and browser-local facts:
current location, DOM/session observations, navigation, browser permissions, and
execution of a daemon-authorized effect. Claim-to-tab binding is acknowledged
by both parties and fenced by holder generation.

The three business identities are deliberately not aliases:

1. **Authentication claim:** daemon-issued opaque grouping for profiles that
   share one human authentication entry. It owns an authentication-entry lease
   and one human login owner; it does not transfer profile evidence.
2. **Institution profile:** install-local opaque identity for one configured
   resolver profile. Authority-relevant edits increment its revision; removal
   tombstones it; a later profile receives a new identity.
3. **Provider safety domain:** exact pre-route and landed provider key, with a
   reviewed packaged alias only when one is justified. It scopes effect lanes
   and provider cooldowns, not entitlement or institution identity.

Canonical work identity is the acquisition key, not one of the three business
identities. `browser_holder_generation` is the execution fence, not a business
identity.

### Fences and revisions

Every claim, binding, evidence observation, lease, permit, result, download
correlation, and winner decision carries the current holder generation. The
fence is an opaque daemon boot plus holder epoch equivalent; the extension may
only mirror the generation acknowledged by its live connection.

`job_attempt_revision` increments for institutional cutover, explicit redrive,
or a retry that starts a new authority epoch. Profile, route, adapter/effect
contract, claim, binding, and effect ordinal each have one narrower purpose. A
stale value is a no-op for durable mutation, import, settlement, suppression
clearing, or artifact winning. A stale browser may still finish a narrow local
effect after demotion; its callback and bytes cannot win.

No fresh route is durable. Route bytes may exist only in a correlated response,
extension memory, the active tab, and ordinary browser history. Durable state,
events, diagnostics, logs, captures, and errors retain only opaque claim data,
route class, revisions, and bounded result codes.

## Phase 0 contract and observability

Phase 0 is an observation-only cutover. At the current decision point, the
processing epoch classifies one blocker from this closed vocabulary:

```text
none
source_gate_only
live_source_remaining
transient_retry_remaining
no_legal_route
policy_gate
identifier_gate
```

`none` is written when no blocker exists; omission is not a second meaning.
`canary_ready_route_exists` is a separate boolean. It is conservatively false
now and becomes meaningful only once Phase 5 adds exact qualified-route state.

The decision payload is committed atomically with the decisive transition and
is never a separate appended event. It contains no provider route, bearer,
credential, local path, institution name, or work identifier. `jobs.diagnose_v1`
remains byte-shape compatible. `jobs.diagnose_v2` is additive and projects the
latest valid decision; the current CLI uses bounded unknown-method fallback only.

Phase 0 also reconciles active plans and records migration/rollback constraints:
legacy fresh URLs, deterministic claim hashes, global terms authority, and
browser-side unmaterialized queues are not promoted into new authority.

## Phase −1 — complete baseline

The baseline is fixed and stays enabled while later phases land:

- exact forced-open selection never substitutes one job for another;
- provider leases remain held through protected effects;
- direct downloads participate in the single effect governor;
- provider admission and cooldown keys are action-specific;
- evidence processing is exact-profile and handles every unique profile in a
  sync; and
- tab ownership, safe close behavior, faithful browser/download fakes, and
  current integration ownership remain intact.

Automatic first-route behavior, source-gate bypass, and concurrency four remain
off. Phase −1 completion is not evidence that any provider is ready.

## Phase 1 — additive durable state and strict protocol (dark)

Add only state that cannot be safely reconstructed from replay:

- institution profile IDs/revisions and authentication claims;
- browser candidates with no URL;
- materialization claims and opaque binding IDs;
- profile evidence observations/current projections;
- typed human-gate observations;
- route suppressions and artifact-winner CAS state; and
- holder generations, permits, revisions, and migration markers.

Use new feature-gated, solicited message families. Do not widen strict job
offers, handoff links, session evidence, provider outcomes, or ratified v1
results. Old extension/new daemon and new extension/old daemon pairs retain the
current explicit fallback with no unknown frame. Prove worst-case request and
aggregate response sizes with explicit headroom before activation.

Scrub rather than promote ambiguous state: old fresh URLs, URL-bearing tab
rows, global terms consent, old deterministic claim hashes, old job-wide
suppression, and unmaterialized browser queues do not become new authority.
Old warm evidence is not migrated into a new profile revision.

**Gate:** migrations, rollback/startup behavior, mixed-version behavior, and
IPC-size proof pass. All capabilities remain dark.

## Phase 2 — recoverable explicit materialization

Move only explicit Open/focus/redrive/retry/restart recovery onto the common
operation:

```text
claim → self-identifying scaffold → two-party binding → revalidation
→ transient route → navigation → acknowledgement → existing handling
```

A scaffold contains only an opaque binding ID and is recoverable after a worker
crash. Direct downloads use the tabless form of the same claim and fence. Fault
injection after every boundary must converge to one scaffold or a clean retry.
Automatic candidate claiming remains disabled.

**Gate:** crash/restart, duplicate binding, stale callback, old-peer, and fresh-
route storage scans pass. The old explicit path remains a fallback until the
new path is proven.

## Phase 3 — unified effects and fair scheduling at one

Use one global effect permit for provider navigation, clicks/forms, provider
APIs, direct downloads, configured terms effects, and PDF-viewer adoption. Hold
it through the declared consequence or bounded failure; do not release the
provider lane before the protected effect starts or establishes its result.

The daemon scheduler uses indexed keyset pagination and fair rotation across
profiles and pre-route/provider safety domains. A descriptor cache is never
eligibility authority. It must not stop at a fixed first page or hold the
session-arbitration mutex during database scheduling work. Keep one parked bound
scaffold per landed safety domain; leave other siblings daemon-side.

**Gate:** fairness beyond positions 200 and 500, long database stall without
false holder takeover, direct/tab parity, effect-permit, duplicate result, and
artifact-winner tests pass at concurrency one.

## Phase 4 — profile evidence and typed human gates

Persist idempotent current-fact evidence with holder generation, exact profile
revision, daemon receipt time, source, and validity. Daemon receipt time drives
TTL. New decisive signed-out evidence revokes warm; unknown authorizes nothing
and does not erase a decisive fact. Two profile observations in one sync both
become durable and independently schedulable.

Before an automatic signed-out/unknown route is eventually enabled, reserve an
authentication-entry lease for its authentication claim. Convert it to a human
owner only after login, MFA, CAPTCHA, or security evidence is observed. Exact
profile warm evidence may proceed independently even when profiles share an
authentication claim.

Project one current action per gate scope: login, MFA, CAPTCHA/security,
browser-host permission, downloads-folder permission, terms, contractual
declaration, or identity ambiguity. Keep one live human-attention surface and
aggregate dependent siblings. A successful gate closes that gate and lets the
daemon schedule eligible siblings; it does not autonomously resolve an
unrelated human action.

**Gate:** two-profile, restart, lost-response, stale-profile, one-claim/many-
siblings, typed-platform-permission, and terms/declaration separation tests
pass. Automatic first-route remains disabled until Phase 5.

## Phase 5 — provider readiness and automatic first-route canary

Provider work may continue in parallel, but enablement is allowlisted by the
exact tuple:

```text
institution_profile_revision
route_revision
provider_safety_domain
adapter_revision
identifier_strategy
```

A tuple is canary-ready only after a current supervised entitled run reaches the
intended provider state, performs the expected effect, adopts the file, passes
structure and work-identity validation, commits the artifact winner, and reaches
`ready` or `imported` with sanitized evidence. A page capture or provider
outcome without a validated artifact is insufficient.

Only qualified tuples may automatically try one signed-out/unknown first route,
and only after ordinary OA exhaustion. Source-gate bypass remains off. Per-route
kill switches stop new effects without closing operator-owned content tabs or a
current human gate.

**Gate:** private canary at concurrency one has complete readiness traces and no
unexplained wrong-work, duplicate-winner, stale-download, or privacy result.
No broad provider readiness is inferred from installation counts or from a
provider outcome alone.

## Phase 6 — source-gate cutover canary

For delegated work with a legal configured and qualified route, one processing
pass:

1. classifies each OA source as completed, in-flight, retryable, or gated;
2. determines whether a current qualified institutional route exists;
3. checks access mode, policy/terms, identifier eligibility, and suppression;
4. writes exactly one transactional cutover decision; and
5. either creates one institutional candidate or retains the OA schedule.

When all callable OA sources have completed and only unopened source gates
remain, the institutional candidate is created in that pass without another
source-gate timer. An already in-flight OA call gets one precise job-level
cutover deadline; it completes normally before the deadline, or is cancelled/
fenced by the incremented attempt revision at the deadline. A missing qualified
route remains at the real source gate. Conservative/assisted mode, missing
identity, policy/terms gates, and suppressed routes create no automatic
candidate.

Late OA callbacks cannot add candidates, promote bytes, settle the job, clear
suppression, or win. `no_entitlement` suppresses only the exact job/profile/
route/provider-safety/adapter/identifier tuple. A later source-gate opening may
start a new OA attempt but does not silently reissue that suppressed route.

**Gate:** private source-gate canary at concurrency one passes decision-point
classification, late-result fencing, readiness allowlisting, fairness, privacy,
and all-outcome measurement. Then expand only to qualified routes at one.

## Phase 7 — concurrency four

Change only the global effect-permit count from one to four. Retain independent
limits for provider safety domains, authentication claims, human attention,
parked scaffolds, candidate fairness, and artifacts. Keep direct and tab effects
in the same count; do not raise descriptor offers merely because effect count
rises.

**Gate:** broad-at-one evidence shows no starvation, wrong work, duplicate
artifacts, stale-holder mutation, unexplained human-surface growth, or IPC-size
regression. Without that evidence, concurrency remains one.

## Phase 8 — coverage and integration expansion

Expand exact route templates, packaged adapters, resolver integrations, delivery
workflows, discovery/watch paths, and destinations one route/profile tuple at a
time. Continue the repair workflow and restrictive controls from ADR-0021.
LibKey remains daemon-side under ADR-0016; delivery remains configured and
seven-point gated under ADR-0017; browser entitlement remains fresh-evidence
only under ADR-0018.

Every expansion retains route kill switches, typed gates, source/profile
cooldowns, attempt/generation fencing, artifact-winner CAS, and strict protocol
compatibility. No expansion authorizes positive remote adapter behavior or
creates durable fresh URLs.

## Measurement and rollback

Every routeable delegated job reports exactly one primary outcome:

```text
unattended_ready
ready_after_one_human_gate
ready_after_multiple_human_gates
manual_fallback
terminal_unavailable
suppressed_route
still_working
```

The benchmark denominator includes manual, challenge, restart, and failure
outcomes. Track unattended-ready rate, human interruptions per job and per
authentication claim, tabs per ready artifact, peak attention surfaces, parked
scaffolds per safety domain, stale/duplicate callbacks, wrong-work prevention,
queue wait/starvation by profile and safety domain, kill-switch activations, and
candidate-to-ready conversion after source-gate bypass.

Rollback stops new claims/effects, disables the affected canary or source-gate
switch, or returns the global permit count to one. It does not close operator-
owned/content tabs or a current human gate. Existing fenced claims either finish
under their revision or expire; an older control/config revision cannot revive
them.

## Solo-maintainer change discipline

Each change should have one primary contract, one owner, focused tests for its
observable boundary, and an explicit non-goal. Land changes directly on `main`
in dependency order; do not combine daemon, protocol, schema, extension,
migration, and UI work into an unreviewable batch. Generated documentation is
updated only by its normal generation change after the source contracts land.

The plan deliberately does not add confirmation prompts for speculative risk.
Human attention is reserved for genuine authentication/security, platform
permission, changed or unconfigured terms/declarations, unresolved identity, or
an explicit conservative/assisted stance.
