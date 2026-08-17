# Institutional processing acceleration

**Status:** Phase −1 through **Phase 4 complete**; Phases 5–7 deferred by design;
Phase 8 and the measurement vocabulary open. See the audit below.

**Ownering model:** solo-maintainer-sized changes landed directly on `main`  
**Scope:** daemon authority, browser materialization, institutional cutover, and
staged enablement. Existing UI work remains a dependency for typed attention
rendering, but does not move authority into the extension.

## Audit 2026-08-16 — verified against the tree, not against this document

Every claim below was checked by reading the named symbol. Shipped sections are
**deletion candidates** under the `dev/active` rule (salvage anything normative
into ADR-0022, then remove); this file stays only for the open and deferred work.

- **SHIPPED** — Changes 0.1–0.3: authority/identity contract (ADR-0022;
  `internal/job/institutional_materialization.go`), transactional cutover
  classification (`internal/job/cutover.go` `InstitutionCutoverDecision`,
  `WithCutoverDecision`), diagnosis v2 with bounded CLI fallback
  (`internal/job/diagnosis.go` `DiagnoseV2`, `internal/cli/jobs_diagnose.go`).
- **SHIPPED** — Phase −1 stability baseline: `internal/job/effect_permit.go`.
- **SHIPPED** — Phase 0 decision observability: closed blocker vocabulary and
  strict parser in `internal/job/cutover.go`.
- **SHIPPED** — Phase 1 durable state and strict protocol: migration
  `0026_institutional_materialization.sql`, `institutional_*` families in
  `internal/protocol/protocol.go` and `extension/src/protocol.ts`.
- **SHIPPED** — Phase 2 recoverable explicit materialization: migration
  `0027_institution_authority_key.sql`; `prepareMaterializationCandidate` and the
  claim/bind/route/reconcile flow in `internal/browser/bridge.go`.
- **SHIPPED** — Phase 3 unified effects and fair scheduling at one: migration
  `0034_effect_permits.sql`, `AcquireInstitutionalEffectPermit`,
  `internal/job/institutional_scheduler.go`.
- **SHIPPED** — Phase 4 profile evidence and typed human gates:
  `internal/job/institutional_evidence.go`, `internal/job/institutional_gates.go`.
  This document previously called Phase 4 "current"; it is not.
- **DEFERRED-BY-DESIGN** — Phases 5–7 (provider readiness canary, source-gate
  cutover canary, concurrency four). `canary_ready_route_exists` is still always
  false, `retryCutoverDecision` still only records a blocker, and
  `0034_effect_permits.sql` still pins one live permit at `slot_index=0`.
- **OPEN** — Phase 8 coverage expansion: no expansion registry exists; packaged
  adapters remain individually configured.
- **OPEN** — Measurement and rollback: none of the seven primary outcome values
  (`unattended_ready` … `still_working`) nor the benchmark denominator exist. This
  is the gap that matters — the phases above shipped without the instrument that
  was supposed to judge them.

## Trimmed 2026-08-17

Sections describing shipped work were removed. The pre-trim text is recoverable in
full at `git show 2d29e7a:dev/active/institutional-processing-acceleration.md`. Cut:

- `Initial three direct-to-main changes — complete`
- `Phase 0 contract and observability — complete`
- `Phase −1 — complete baseline`
- `Phase 1 — additive durable state and strict protocol (implemented-but-dark)`
- `Phase 2 — recoverable explicit materialization`
- `Phase 3 — unified effects and fair scheduling at one`
- `Phase 4 — profile evidence and typed human gates`

Normative sentences from the cut phases were relocated into
`Authority and identity contract` (baseline invariants, the atomic decision payload
and its redaction rule, the single global effect permit, "a descriptor cache is never
eligibility authority", the signed-out/unknown evidence rules, and the rule that one
current action is projected per gate scope) and into `Phase 5` (the
authentication-entry lease precondition). `Current rollout state` was condensed to
present-tense fact only.

## Current rollout state

The current release keeps automatic first-route behaviour and source-gate
bypass disabled. The global browser-effect permit is one. Existing explicit
handoff and direct/generic paths remain the fallback behind their current
feature, access-mode, holder, and safety gates. No provider tuple is broadly
enabled; the readiness stream continues independently, and no provider holds
a `ready` claim here.

Phase 1's durable protocol, `institutional_materialization_v1`, is active
only for holders that explicitly negotiate it in `hello`; responses stay
disposition-gated and unknown fields are rejected. Phase 2's recoverable,
user-invoked candidate materialization is live. A scaffold contains only an
opaque binding ID and is recoverable after a worker crash. The legacy
URL-bearing `job_offer` path remains available to peers that do not
advertise the feature. Automatic candidate claiming, automatic first-route
behavior, source-gate bypass, provider canaries, and concurrency changes
remain off.

`effect_permits` is its own table, not extra columns on
`materialization_claims`. The single global slot is `slot_index = 0`;
occupying status is `held | unknown_completion`. Peers must advertise
`effect_permit_v1` before `started`. `AdvanceMaterializationEffect` stays
dark.

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

The baseline is fixed and stays enabled while later phases land:

- exact forced-open selection never substitutes one job for another;
- provider leases remain held through protected effects;
- direct downloads participate in the single effect governor;
- provider admission and cooldown keys are action-specific;
- evidence processing is exact-profile and handles every unique profile in a
  sync; and
- tab ownership, safe close behavior, faithful browser/download fakes, and
  current integration ownership remain intact.

The decision payload is committed atomically with the decisive transition and
is never a separate appended event. It contains no provider route, bearer,
credential, local path, institution name, or work identifier.

Use one global effect permit for provider navigation, clicks/forms, provider
APIs, direct downloads, configured terms effects, and PDF-viewer adoption.
Hold it through the declared consequence or bounded failure; do not release
the provider lane before the protected effect starts or establishes its
result. A descriptor cache is never eligibility authority.

New decisive signed-out evidence revokes warm; unknown authorizes nothing and
does not erase a decisive fact.

Project one current action per gate scope: login, MFA, CAPTCHA/security,
browser-host permission, downloads-folder permission, terms, contractual
declaration, or identity ambiguity. Keep one live human-attention surface and
aggregate dependent siblings. A successful gate closes that gate and lets the
daemon schedule eligible siblings; it does not autonomously resolve an
unrelated human action.

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

Before an automatic signed-out/unknown route is eventually enabled, reserve
an authentication-entry lease for its authentication claim. Convert it to a
human owner only after login, MFA, CAPTCHA, or security evidence is
observed. Exact profile warm evidence may proceed independently even when
profiles share an authentication claim.

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
