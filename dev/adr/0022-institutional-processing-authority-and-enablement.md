# ADR-0022: Institutional processing authority and staged enablement

Status: **Accepted** (2026-08-11). Phases 0 through 3 are complete; Phase 4 is
current. Automatic first-route, source-gate bypass, and effect concurrency
greater than one remain disabled. This ADR ratifies the authority, identity, fencing,
cutover, and enablement rules for institutional processing. It supersedes the
conflicting execution clauses in older active plans while retaining the
browser-safety invariants in ADR-0003, ADR-0013, ADR-0018, and ADR-0021.

## Context

Delegated access is standing operator consent for ordinary acquisition. It is
not permission to invent an entitlement, bypass a configured policy, or let a
browser worker become a second durable scheduler. The daemon already owns jobs,
policy snapshots, transitions, and artifacts. The extension already owns the
ordinary browser, including tabs, navigation, DOM observations, and browser
permission facts. Institutional processing needs a cutover that uses those
boundaries rather than moving authority into a restartable MV3 worker.

The cutover also has to survive strict version skew. Existing IPC result shapes
remain frozen under ADR-0009. New observations use additive methods or
feature-gated messages, and daemon-initiated frames follow ADR-0006's narrow
compatibility exception. No phase in this ADR changes acquisition decisions
until its stated enablement gate passes.

## Decision 1: authority is split, and binding is two-party

The daemon is authoritative for durable, unmaterialized work and decisions:

- jobs, canonical work identity, policy and access mode;
- institution profiles and their revisions;
- candidate ordering, fairness, route revisions, and cutover decisions;
- materialization claims, effect permits, suppressions, cooldown projections,
  and artifact-winner CAS; and
- durable transitions, diagnosis, and redacted audit detail.

The extension is authoritative only for physical browser resources and facts:

- scaffold and tab existence, ownership, current location, and visibility;
- browser-local navigation, DOM, session, download, and permission observations;
- execution of an already-authorized browser effect; and
- the physical tab or download binding acknowledged for a daemon claim.

A candidate claim and its physical binding are therefore **two-party state**.
The daemon creates a fenced claim and opaque binding ID; the extension creates
or recovers the physical resource and acknowledges that binding; the daemon
revalidates the claim before issuing a route or effect permit. Neither side may
use an unacknowledged local record as authority for the other side.

The extension has no durable queue of accepted unmaterialized jobs. A bounded
local descriptor cache is permitted only as a recovery aid and is never the
source of candidate eligibility, ordering, suppression, or winner decisions.

## Decision 2: three business identities remain distinct

These are the three business identities used for institutional processing:

| Identity | Meaning | Non-meaning |
| --- | --- | --- |
| **Authentication claim** | A daemon-issued opaque identity grouping profiles that genuinely share one human authentication entry. It scopes one authentication-entry lease and one human login owner. | It is not an extension hash of an identity-provider value, and warm evidence for one profile does not become evidence for every profile in the claim. |
| **Institution profile** | An install-local opaque identity for one configured resolver profile. It survives ordinary edits through a new revision, is tombstoned on removal, and is never reused for a later profile. | It is not a provider host, a route URL, a patron identity, or proof that a session is signed in. |
| **Provider safety domain** | The exact pre-route admission key and, after landing, the exact normalized host unless a packaged adapter declares a reviewed safety-family alias. It scopes effect serialization and provider cooldowns. | It is not an institution, an authentication claim, an entitlement subject, or a wildcard over recognition hosts. |

Canonical work identity remains a separate acquisition identity. A job may carry
all four concepts, but none may be substituted for another. A holder generation
is an execution fence, not a fourth business identity.

Profile changes that affect route construction, authentication equivalence,
terms or declarations, or delivery policy increment the profile revision.
Authentication equivalence is recomputed by the daemon; a change creates a new
opaque authentication claim. Old evidence, claims, terms authority, and
suppressions never participate in the new profile revision.

## Decision 3: holder generation is the execution fence

ADR-0003's first-holder arbitration remains the session boundary. Every holder
promotion additionally mints an opaque generation equivalent to
`(daemon_boot_id, holder_epoch)`. The extension may mirror only the generation
acknowledged on its live connection; it cannot infer a daemon restart, explicit
holder switch, or promotion.

The current holder generation is carried by every durable or correlated action
that can affect a job:

- candidate and materialization claims;
- binding acknowledgements;
- profile evidence and authentication-entry leases;
- effect permits, route issuance ordinals, and result callbacks;
- download/adoption correlations and artifact-winner decisions.

A stale holder cannot mutate current-generation daemon state, displace a
committed artifact winner, or acquire a new effect. It may report only the
exact consequence of its historical permit; that result or correlated winner
can settle the historical occupancy without authorizing a successor.
Automatic stale promotion waits while an unexpired materialization claim is
live. Effect-permit occupancy does not expire: a replacement effect waits for
exact settlement or exact-ID operator resolution even after the diagnostic
lease elapses.

This is not a claim of distributed atomicity with Chrome. A demoted browser may
complete a click or navigation after it received a permit. SQLite and native
messaging cannot roll that browser-local effect back, which is why uncertainty
continues occupying instead of authorizing a duplicate.

## Decision 4: attempts and revisions are explicit and monotonic

The following values have one responsibility each:

- `job_attempt_revision` increments when institutional cutover, explicit
  redrive, or retry starts a new authority epoch. It fences late OA and browser
  callbacks; it is not a provider cooldown.
- `institution_profile_revision` identifies the exact authority-relevant
  configuration used by a candidate.
- `route_revision` identifies the compiled route choice. A route revision never
  contains a URL in durable state.
- `adapter_revision` and `effect_contract_id` identify packaged browser
  behaviour and its reviewed effect contract.
- `materialization_claim_id` identifies the daemon's current claim over one
  candidate.
- `binding_id` identifies the recoverable physical scaffold binding and carries
  no work, profile, host, or route text.
- `effect_ordinal` orders effects within one claim. Existing strategy-level
  drive-attempt values remain useful for strategy sequencing but are not the
  universal materialization fence.

A late result carrying an earlier attempt, profile revision, route revision,
holder generation, claim, binding, or effect ordinal is a stale result. It may
be counted as bounded diagnosis, but it cannot add a candidate, mutate current
state, clear suppression, import bytes, settle the job, or win the artifact.

## Decision 5: materialization is recoverable and fresh routes are transient

Automatic routing, explicit Open, focus, redrive, retry, restart recovery, and
holder replacement use one operation:

```text
daemon claim → opaque binding → extension scaffold → binding acknowledgement
→ daemon revalidation → permits → transient route → navigation → landed key
→ effect permit → result/download/adoption → artifact-winner CAS
```

The tab path uses a self-identifying extension scaffold. The direct-download
path uses the same claim, holder generation, permits, safety key, result fence,
and winner CAS without a scaffold tab. A crash at each boundary reconciles to
one current claim and at most one recoverable scaffold, or returns the candidate
to eligibility.

A fresh route is recomputed only after complete claim revalidation. It may exist
in the correlated native response, an extension local variable, the active tab,
and ordinary browser history. It is never persisted in daemon state, extension
storage, events, diagnostics, logs, captures, or error strings. The daemon records
an issuance ordinal rather than route bytes. A lost response may be reissued for
the same current binding; only the current issuance may mutate daemon state.

## Decision 6: typed human gates are the only attention authority

Standing delegated consent authorizes ordinary automatic work. Attention is
created only by a typed current gate, not by a cold candidate, `requires_auth`,
unknown evidence alone, or an expectation that login may be needed.

The closed initial gate vocabulary is:

```text
human_gate.login
human_gate.mfa
human_gate.captcha_or_security
human_gate.browser_host_permission
human_gate.downloads_folder_permission
human_gate.terms_required
human_gate.contractual_declaration
human_gate.identity_ambiguous
```

Browser-host permission is a browser-bound user-gesture gate. Downloads/folders
permission is a daemon/platform gate and need not own a provider tab. Ordinary
website terms and contractual or institutional declarations are separate action
classes; one never authorizes the other. A changed or unconfigured terms or
declaration authority performs no click.

Initially there is one global effect permit, one live human-attention surface,
one live human authentication owner per authentication claim, and at most one
parked bound scaffold per landed provider safety domain. A hundred sibling jobs
sharing one authentication claim therefore produce one human surface with a
dependent count; successful resolution resumes eligible siblings through normal
daemon scheduling.

## Decision 7: cooldown scopes are not interchangeable

Every cooldown has an explicit scope:

- source/quota gates use the source and quota identity observed from the provider;
  configured limits are never evidence of provider headroom (ADR-0012);
- pre-route admission uses the exact profile revision, route revision, provider
  safety domain, adapter revision, and applicable authentication claim;
- landed provider challenge or rate-limit cooldown uses the exact normalized
  host or reviewed safety-family domain;
- authentication-entry ownership uses the authentication claim, with profile
  evidence still exact-profile scoped;
- route-gateway throttling uses the explicit profile/route gateway key; and
- human/platform gates use their gate type and bound scope.

A provider cooldown never blocks unrelated provider domains. A login owner never
transfers warm evidence across profile revisions. Opening effect capacity never
releases human-surface or parked-scaffold budgets. Recognition-host lists,
job-wide booleans, and global terms scalars are not cooldown or authority keys.

## Decision 8: cutover is classified transactionally

A processing epoch records exactly one cutover decision payload on the decisive
`job.transition`. The payload uses only these stable keys:

```json
{
  "institution_cutover_blocker": "none | source_gate_only | live_source_remaining | transient_retry_remaining | no_legal_route | policy_gate | identifier_gate",
  "canary_ready_route_exists": true
}
```

`none` is an explicit value meaning that no blocker exists at the current
decision; it is never omitted. `canary_ready_route_exists` is conservatively
`false` in Phase 0 because there is no qualified-route registry yet. It becomes
meaningful only in Phase 5, after exact route readiness is represented.

The detail is committed in the same transaction as the transition that records
the decision. It never becomes a separately appended, non-atomic event. No
provider URL, route text, credential, bearer value, local path, institution
name, or work identifier is permitted in this detail.

Diagnosis v1 remains byte-shape compatible for strict old clients. Diagnosis v2
is additive and projects the latest valid decision. The current CLI may prefer
v2 and may fall back to v1 only on a bounded unknown-method response; it must not
reinterpret an arbitrary error as compatibility. This is an additive reader,
not a widening of any ratified v1 result.

## Decision 9: suppression and artifact winners are daemon rules

`no_entitlement` is recorded only by the current fenced attempt after correct
work identity, current profile and route revisions, decisive entitlement
classification, non-unknown/non-signed-out evidence, and current holder and
materialization identity are all proven. Its suppression scope is:

```text
(job, institution_profile_revision, route_revision,
 provider_safety_domain, adapter_or_capability_revision,
 identifier_strategy)
```

Profile, route, adapter/capability, evidence classification, explicit redrive,
or eligible-route changes invalidate that exact suppression. An OA source gate
opening may begin a new OA attempt, but does not silently reissue a suppressed
institutional route. Wrong work, stale callbacks, authentication uncertainty,
and adapter drift are never `no_entitlement` facts.

Before promotion or import, the daemon claims one artifact winner per job with
an atomic uniqueness/CAS operation. Duplicate browser outcomes, duplicate
download events, late OA results, and restart recovery all converge on this
winner. Losing bytes are cleaned or remain quarantined under bounded retention;
none can be imported through a race.

## Decision 10: staged enablement and rollback

The staged order is normative:

- **Phase −1 — complete:** stabilize current forced-open selection, leases,
  effect concurrency one, exact evidence handling, safe tab closure, faithful
  browser fakes, and obsolete remainder cleanup. Automatic first-route and
  source-gate bypass stayed disabled.
- **Phase 0 — complete:** land this authority contract, transactional cutover
  classification, diagnosis v2, stable vocabulary, privacy inventory, and
  active-plan reconciliation. Observation only; no acquisition decision changes.
- **Phase 1 — complete:** migration `0026` (with `user_version` `26`) added
  seven URL-free daemon projections: `institution_profiles`,
  `browser_candidates`, `materialization_claims`, `profile_evidence`,
  `human_gate_observations`, `route_suppressions`, and insert-only
  `artifact_winners`. The strict, feature-negotiated
  `institutional_materialization_v1` contract covers claim, bind, route,
  navigated, and reconcile. Ambiguous extension authority was scrubbed rather
  than promoted.
- **Phase 2 — complete:** explicit Open/focus/redrive/retry/restart recovery
  uses the self-identifying scaffold, two-party binding, and paginated
  reconciliation. Automatic claims remain off.
- **Phase 3 — complete:** migration `0034` (schema version `34`) adds one
  daemon-durable effect permit globally and per provider safety domain.
  Generic drive, direct get, PDF grab, terms, and institutional effects all
  acquire before execution and reconcile exact completion after worker loss.
  The fair scheduler remains at concurrency one.
- **Phase 4:** add exact-profile evidence, authentication-entry leases, typed
  gate projection, terms/declaration authority, and one attention surface.
  Automatic first-route behavior remains disabled until readiness.
- **Phase 5:** qualify exact profile/route/provider-safety/adapter/identifier
  tuples from a current live entitled run through validated ready and successful
  adoption or attachment. Only then may automatic signed-out/unknown first
  routing be canaried after ordinary OA exhaustion. Source-gate bypass remains
  off. No provider is declared ready by this ADR.
- **Phase 6:** canary source-gate cutover only for qualified tuples. Classify
  OA sources, determine route readiness, policy and identifier eligibility,
  and commit one decision with the institutional candidate or retained OA
  schedule. Fence in-flight OA before it can compete. Keep a per-route kill
  switch.
- **Phase 7:** raise only the global effect permit from one to four after
  broad-at-one evidence passes. Keep independent authentication, provider,
  human-surface, parked-tab, fairness, and artifact limits.
- **Phase 8:** expand exact route templates, adapters, integrations, delivery,
  workflows, and destinations per-route; do not weaken earlier gates.

## Phase 1 implementation note: dark and direct-to-main

The coordinated Phase 1 implementation is complete but dark. Migration `0026`
creates exactly seven daemon-owned projections: institution profiles, URL-free
browser candidates, materialization claims, profile-evidence observations,
current human-gate observations, route suppressions, and insert-only artifact
winners. IDs are opaque daemon-minted values; profile and claim authority
columns are revisioned and immutable for a candidate claim. The migration
advances SQLite `user_version` to `26`.

The strict `institutional_materialization_v1` surface lands together in the
Go validator, TypeScript types/parser, and JSON Schema. It consists of paired
`institutional_claim_request`/`institutional_claim_response`,
`institutional_bind_request`/`institutional_bind_response`,
`institutional_route_request`/`institutional_route_response`,
`institutional_navigated_request`/`institutional_navigated_response`, and
`institutional_reconcile_request`/`institutional_reconcile_response` families.
Every request except reconcile is job-scoped. IDs are bounded opaque strings;
tab IDs and ordinals are bounded safe nonnegative integers. URLs are absent from
requests and all durable or diagnostic state; only a successful route response
may contain a transient URL. Responses use only `feature_disabled`, `claimed`,
`bound`, `issued`, `acknowledged`, `reconciled`, `stale`, `not_eligible`, `busy`,
or `error`, with disposition-gated fields and strict unknown-field rejection.

The extension performs an explicit versioned managed-state scrub migration:
legacy URLs, hashes, global terms authority, and ambiguous browser queues are
discarded rather than promoted, while safe current non-URL job, download, and
lease state is preserved. All new handlers are hard-disabled and return
structured `feature_disabled` without mutation. Automatic candidate creation,
materialization, tab creation, navigation, downloads, source-gate bypass,
provider canaries, and concurrency changes remain off. This note claims no
Phase 2 behavior or provider readiness.


Rollback disables new claims/effects, suppresses the affected profile/route,
or returns the permit count to one. It never closes operator-owned content tabs
or a current human gate, and a lower revision cannot revive an expired claim.

## Consequences and compatibility

This ADR preserves the boundaries ratified by ADR-0003 (holder arbitration),
ADR-0006 (directional frame compatibility), ADR-0009 (additive strict IPC and
no autonomous drain), ADR-0012 (observed provider limits), ADR-0013 (browser-
local facts and daemon-owned transitions), ADR-0016 (daemon-side institutional
routing and tri-state auth), ADR-0017 (configured delivery gates), ADR-0018
(fresh session evidence only), and ADR-0021 (packaged positive behaviour and
restrictive-only control).

New protocol work is strict, feature-negotiated, and solicited. Compatibility is
bounded to feature negotiation; correctness does not carry a broad old/new
shim burden. Existing messages remain unchanged, and no unknown institutional
frame is sent to a peer that did not negotiate the feature. Migrations scrub
legacy fresh routes, global terms authority, deterministic claim hashes, and
unmaterialized browser queues without promoting ambiguous history into new
authority. Every implementation slice remains small enough for one maintainer
to review and land directly on `main`.

## Amendment 2026-08-12: adversarial review dispositions

An external adversarial review of the shipped Phase 0–4 code, followed by two
internal reviewers of the resulting fixes, produced the decisions below. They
are recorded here because each one is a boundary a later change could
plausibly "simplify" back.

**The artifact winner is per job ATTEMPT, not per job.** `artifact_winners`
was keyed by `job_id` alone while every column, index and caller treated it as
per attempt. Migration 0033 re-keys it. A winner is committed only AFTER
validation, so rejected bytes cannot permanently win an attempt and lock out the
correct file that arrives next. Every browser-delivered file — correlated,
swept, or grab-adopted — goes through one institutional-aware ingest.

**A CAS failure after validation is not an adoption failure.** The bytes are
already attached; only the record of who won failed. Returning an error there
reports a landed file as deferred, skips settlement and skips the conclusive
latch.

**An observation is stored against the revision it was produced under.** The
correlated lookup must consult candidate HISTORY, not the "current candidate"
query, which hides a candidate whose profile revision was superseded and so
silently promotes stale evidence into the live revision. A rejected observation
must abort the gate and lease mutations that would otherwise act on it.

**Human gates are keyed to the occurrence.** Exact replay stays idempotent, but
two genuinely distinct occurrences must not collapse — `auth_pending` without
`elapsed_ms` did, so a second sign-out silently failed to reopen a resolved
gate. The gate id carries the frame's `msg_id`; the evidence id deliberately
does not, because `profile_evidence` is append-only and would otherwise grow
with browsing activity rather than with distinct facts.

**A process-local throttle is not a source refusing papio.** It is never a wake
time, and it is never charged against the retry budget — but the exemption is
narrow: only a pass where no source was reached at all. A pass that called
sources and came back empty stays chargeable, or the job re-parks forever
without a verdict.

**Authorization is at most once, and refusal is not a stop.** A replayed drive
epoch answers `duplicate` rather than re-authorizing. Because the extension
retires such an epoch without ever sending a result, the release is at the next
offer, which supersedes a stalled epoch and mints a successor; superseding at
the refusal instead would discard a result that is merely late.

**The safety domain is a provider fence, shared across jobs.** Deriving it from
the job id gave every job a private domain, so the scheduler's sibling anti-join
— the only cross-job serialization that exists — could never match. Scheduler
grouping deliberately keys on the same value: the anti-join only counts claims
at `bound` and beyond, so grouping is what stops many candidates sitting
`claimed` at once. One bound scaffold per institution is the intended shape, and
it matches the extension's own effect governor. Route suppressions keep a
permanently failing candidate from holding the head of that queue.

**Suppressions and claim renewal have producers.** Both were complete,
consumed, and never written. A route that proved `no_entitlement`, or answered
with a challenge, is fenced by its exact tuple so rediscovery cannot re-select
it. Authentication traffic renews the materialization lease, because a login,
MFA prompt or CAPTCHA is human-paced and routinely outlives the action expiry.

### Knowingly deferred at review time

- **A daemon-durable leased effect permit.** Closed by the 2026-08-13
  amendment below. `AdvanceMaterializationEffect` remains dark; the permit
  acquire is the authority.
- **A produced-under fence for uncorrelated session evidence.** It carries no
  correlation, so nothing proves which revision produced it. Mitigated by
  refusing uncorrelated evidence briefly after an authority change; the real fix
  is a daemon-minted profile fence echoed by the extension, which is a
  coordinated Go/TypeScript/schema change.
- **Opaque provider domains in durable events.** `final_host`/`final_path`
  survive because route-family correlation reads them, and they are retained
  only when they matched the origin and path papio itself proposed. Free text
  from an adapter is redacted at all three sinks.
- **A late adoption after claim expiry.** A stale materialization claim cannot
  win the artifact or mutate the current claim/candidate. The validated bytes
  are still adopted. Exact durable filename-plus-SHA producer evidence settles
  only its historical permit or legacy blocker; missing or ambiguous producer
  evidence leaves occupancy unresolved rather than guessing.

## Amendment 2026-08-13: Phase 3 effect authority

The deferred daemon-durable permit is implemented. Migration `0034` adds
`effect_permits` with one occupying global slot and one occupying row per
provider safety domain; both `held` and `unknown_completion` occupy. Generic
drive and direct get share one attempt identity namespace. PDF grab uses its
exact grab correlation, terms uses a daemon-authorized job-scoped occurrence,
and institutional navigation uses expected-ordinal CAS plus a stable request
identity. Institutional acquire and route issuance are one transaction and
the sole production writer of `materialization_claims.effect_ordinal`.

Every reachable irreversible browser path acquires before execution. Exact
results, correlated artifact winners, and kind-specific reconciliation settle
the permit; silence, elapsed time, ordinary retry, cancellation, and claim
expiry do not. Worker restart changes a held permit to
`unknown_completion` unless the browser can prove its exact consequence.
Unresolved occupancy blocks claim abandonment and further global or same-domain
effects. Operators can resolve only an exact unknown permit through
`papio browser permit resolve <permit-id> --reason <text>`; `papio pulse`
and `papio doctor` expose the blocking permit identity.

Pre-`0034` started epochs import as cleanup-only `legacy_effect_blockers`.
Exact late results and uniquely correlated artifact winners settle those
blockers without mutating a current job generation. Startup performs the
import before browser work can acquire. Schema-33 binaries containing the
future-version guard refuse schema 34; downgrade to earlier binaries requires
restoring a pre-migration backup.

The additive `effect_permit_v1` protocol is implemented in Go, TypeScript, and
the published JSON Schema. A peer that does not advertise it receives
`unsupported` and cannot start a protected effect. The extension's in-memory
effect governor remains defense in depth, not authority.

This closes the first knowingly deferred item above. The produced-under fence
for uncorrelated session evidence and opaque provider-domain cleanup remain
deferred. Claim renewal reduces late-adoption exposure; exact correlated late
bytes can release only their historical occupancy, while unresolved effects
remain blocking and cannot be re-driven.

## Amendment 2026-08-26: content retention is per paper, not per attempt

papio never auto-closes content, and that stands. The rule's own justification
is that one visible tab showing an acquired paper is confirmation rather than
litter, so retention is scoped to ONE surface per paper. The implementation
could not honour that scope: a papio-created tab that navigated to a PDF was
ceded, and ceding deletes the record's job binding, so the retained copy lost
the paper identity. No later drive could recognise it, every drive minted
another copy, and each copy was retained in turn. Measured live on 2026-08-26:
fourteen retained tabs for one paper, thirty-one stale papio surfaces in one
work window, none reachable by any close path.

A PDF surface still inside papio's own container is now RETAINED rather than
ceded: the record keeps `job_id` and gains `content`. Pinning a tab, making it
active, or moving it out of papio's container remains an operator takeover and
still cedes permanently. Retained content is excluded from surface reuse, so a
drive never navigates an acquired paper away.

A retained copy may be retired only as a superseded duplicate, and only on
positive evidence: papio created it, a NEWER retained copy of the same paper
exists, the surface is still content inside papio's container, the operator has
not made it active or pinned it, and it has outlived the measured cold window.
The newest copy is never a candidate, so the paper never leaves the operator's
screen. This retires no operator-owned content tab, and rollback remains as
stated above.

The daemon decides independently, and `surface_superseded` gains the one
eligibility this needs. Its previous rule authorized only a tab OTHER than the
named binding's driven tab, which fits duplicates that share one binding but
refuses the case measured here: a re-drive mints a new claim for the same paper
and abandons nothing, so the superseded claim keeps `navigated` and its own tab
until the next holder promotion sweeps its lapsed lease. `claim_abandoned` is
therefore false for it, and every ask was refused. The authority is now the
JOB's live claim: a binding whose claim is not the job's live claim no longer
drives that paper, so its surface may be retired, with the pre-existing effect
veto retained — an unsettled provider effect on that exact claim still refuses.
A binding that IS the job's live claim keeps its tab, and a binding with no
claim at all remains browser-local by the existing contract.