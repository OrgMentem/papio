# ADR-0022: Institutional processing authority and staged enablement

Status: **Accepted** (2026-08-11). Phase 0 is the current implementation phase. This
ADR ratifies the authority, identity, fencing, cutover, and enablement rules for
institutional processing. It supersedes the conflicting execution clauses in
older active plans while retaining the browser-safety invariants in ADR-0003,
ADR-0013, ADR-0018, and ADR-0021.

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

A stale holder cannot mutate current daemon state, close a current claim, clear a
newer lease, import or attach bytes, or win the artifact race. Automatic stale
promotion waits while an unexpired materialization/effect permit is live. An
explicit holder switch may increment the generation immediately, but a
replacement effect waits for the old permit to expire or reconcile.

This is not a claim of distributed atomicity with Chrome. A demoted browser may
complete a click or navigation in the narrow interval after it received a
permit. Its callback and bytes are fenced and cannot win. Browser-local effects
cannot be rolled back by SQLite or native messaging.

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
- **Phase 0 — current:** land this authority contract, transactional cutover
  classification, diagnosis v2, stable vocabulary, privacy inventory, and
  active-plan reconciliation. Observation only; no acquisition decision changes.
- **Phase 1:** add dark, additive durable state, migrations, strict messages,
  profile revisions, holder generations, claims, evidence projections,
  suppressions, winner CAS, and typed gates. No automatic materialization.
- **Phase 2:** migrate explicit Open/focus/redrive/retry/restart recovery to the
  self-identifying scaffold and two-party binding. Automatic claims remain off.
- **Phase 3:** unify tab and direct effects under one permit at concurrency one;
  add fair daemon scheduling, exact pre-route/landed lanes, and bounded parked
  scaffolds.
- **Phase 4:** add exact-profile evidence, authentication-entry leases, typed
  gate projection, terms/declaration authority, and one attention surface.
  Automatic first-route behavior remains disabled until readiness.
- **Phase 5:** qualify exact profile/route/provider-safety/adapter/identifier
  tuples from a current live entitled run through validated ready and successful
  adoption or attachment. Only then may automatic signed-out/unknown first
  routing be canaried after ordinary OA exhaustion. Source-gate bypass remains
  off. No provider is declared ready by this ADR; ScienceDirect, specifically,
  has no ready claim here.
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

New protocol work is feature-negotiated and solicited. Old extensions receive
no unknown new request or frame and retain the current explicit handoff fallback.
Migrations scrub legacy fresh routes, global terms authority, deterministic
claim hashes, and unmaterialized browser queues without promoting ambiguous
history into new authority. Every implementation slice remains small enough for
one maintainer to review and land directly on main.
