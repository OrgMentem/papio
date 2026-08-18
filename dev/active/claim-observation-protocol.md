# Claim-observation protocol: written design (Slice 3 gate)

Status: **implemented, shipped**. The design below shipped in two parts: the
protocol design (this document) landed in `e2c7c45` (2026-08-18); the full
four-site implementation landed in `7662f6a` (2026-08-18), with Slice 4's
consumption of the claim-bound path in `5b866d2` (2026-08-19). This document
was the deliverable `dev/active/surface-lifecycle-plan.md` line 330 required
before Slice 3 implementation started ("Gated on a written protocol design
(four-site parity) before implementation"). It specifies wire messages,
storage, ordering, lease, and rollout precisely enough for three implementing
agents — Go daemon (`internal/protocol`, `internal/browser/bridge.go`,
`internal/store/migrations`), TS extension (`extension/src/protocol.ts`,
`extension/src/background.ts`), and JSON Schema
(`protocol/browser-v1.schema.json`) — to build without negotiating shape
between themselves. It supersedes no ADR; it fills in the mechanism ADR-0022
Phase 4 reserved (Decisions 2, 3, 5, 6) and the August-reviewed arbitration
design referenced by the plan (`navigate_existing` / `open_new` /
`focus_owner` / `park`).

Having shipped, this document now also serves as **the reference for the
shipped wire contract** — the exact message shapes in §2, the
ordering/idempotency rules in §3, and the storage/migration detail in §4 —
until that content is salvaged into an ADR. Per AGENTS.md's `dev/active/`
discipline ("a file leaves this directory when the work ships: salvage
anything still normative into an ADR, then delete it" — and an ADR must
never depend on a `dev/active/` file for normative content), this file
cannot be deleted yet: it is deletable only once its normative wire-contract
detail has a permanent ADR home, not merely because the code it describes
has shipped.

Every `file:symbol` citation below was read from the current tree before
being written down (AGENTS.md's `dev/active/` discipline). §8 lists them
together for an easy re-verification pass.

## 0. What already exists and what this adds

ADR-0022 Phase 1 (migration `0026`, `dev/adr/0022-….md` "Phase 1
implementation note") shipped, dark, the **materialization** pipeline: one
`browser_candidates` row per (job, institution profile revision, route
revision), `materialization_claims` (claim → binding → phase machine
`claimed→bound→route_issued→navigated→settled|abandoned`), `profile_evidence`
(verdict/source observations), `human_gate_observations` (typed, occurrence-
keyed gates), `route_suppressions`, `artifact_winners`. Migration `0029`
(`internal/store/migrations/0029_authentication_entry_leases.sql`) already
added `authentication_entry_leases` — a durable, holder-generation-fenced,
idempotently-renewable reservation keyed by `authentication_claim_id`, with
`reserved`/`human`/`expired` states and a store API
(`internal/job/institutional_evidence.go`:
`ReserveAuthenticationEntryLease`, `GetAuthenticationEntryLease`,
`ExpireAuthenticationEntryLease`, `ConvertAuthenticationEntryLeaseToHuman`).
**None of this is wired to any protocol handler.** `institutional_claim_request`
et al. (`internal/protocol/protocol.go:1008-1090`,
`internal/browser/bridge.go` institutional handlers) exist and are strict,
dual-validated, and feature-gated (`InstitutionalMaterializationFeature`,
`protocol.go:985`) — but they materialize a **candidate**, not a **human
sign-in**, and every handler is dark: "Automatic candidate creation,
materialization, tab creation, navigation... remain off"
(ADR-0022 Phase 1 note).

Slice 3 does not re-invent this. It adds exactly one new thing:
**authentication-claim-level arbitration and observation**, sitting in front
of the existing candidate materialization flow, deciding *whether and how* a
human sign-in surface for a candidate's authentication claim may exist right
now, and streaming the human-paced events (wall/login/MFA/challenge/landing/
close/nav-error) that keep the entry lease alive and eventually promote it to
`human` (real login) via the **already-implemented**
`ConvertAuthenticationEntryLeaseToHuman`. The generic close-authorization
transaction (Slice 2's close primitive) is designed here too, under its own
feature, because Slice 3's `owner_closed` observation needs somewhere to
point.

## 1. Feature names

Two features, not one, and the split is deliberate:

- **`surface_close_v1`** — gates the generic one-use close-authorization
  request/response pair (§2.3). It authorizes closing *any* daemon-bound
  scaffold (institutional or not), so Slice 2b can implement and ship the
  close transaction (`dev/active/surface-lifecycle-plan.md` lines 288-299)
  **before** Slice 3 lands, with no dependency on authentication-claim
  arbitration existing yet. Recommended because Slice 2b is sequenced strictly
  before Slice 3 (`Sequencing: 0 → 1 → 2a → 2b → 3 → 4`, plan line 357) and
  gating its own close mechanism behind a feature name that also implies
  full claim arbitration would falsely tell an extension "the close
  transaction works" only once the much larger Slice 3 daemon surface ships,
  delaying 2b for no protocol reason.
- **`institutional_authentication_claim_v1`** — gates the full claim-request
  and claim-observation family (§2.1, §2.2). This is **already** the exact
  string the extension checks
  (`extension/src/background.ts:310`,
  `const AUTHENTICATION_CLAIM_FEATURE = "institutional_authentication_claim_v1"`,
  pinned by `extension/test/background.test.ts:100`'s `AUTH_CLAIM` constant
  and the Slice-0-shipped containment gate). Slice 3 is the first daemon that
  advertises it; the extension-side gate that already exists
  (`dev/active/surface-lifecycle-plan.md` lines 218-236, Slice 0 "SHIPPED")
  starts passing the moment this feature and a passing connectivity probe are
  both true — no extension change required to consume it.

**Implementation hazards, both load-bearing, neither optional to check:**

1. `internal/browser/bridge_test.go:845-848` hardcodes the exact advertised
   feature list as a `slices.Equal` assertion, and `NewBridge`
   (`internal/browser/bridge.go:587-590`) hardcodes the same list as the
   `required` slice. Landing `surface_close_v1` and later
   `institutional_authentication_claim_v1` each needs a matching edit to
   *both* sites, in the same order (AGENTS.md "Adding a daemon feature flag
   breaks one hardcoded assertion").
2. **The 32-feature cap is nearly saturated.** Counting the current
   `required` literal (`bridge.go:587-590`) gives **30** advertised features.
   `surface_close_v1` makes 31; `institutional_authentication_claim_v1` makes
   **32** — the exact fail-closed ceiling enforced independently in
   `internal/protocol/protocol.go:2413` (`hello.features capped at 32`),
   `protocol.go:4396-4397` (`hello_ack.features capped at 32`), and
   `extension/src/protocol.ts:3023,3876` (`CLIENT_FEATURE_RE`/hello_ack
   array-length checks), all pinned by `protocol_test.go:181-182,346-347`.
   Slice 3 ships with **zero headroom left**: any protocol feature added
   after Slice 3 — including anything discovered while implementing Slice 3
   itself — requires retiring or consolidating an existing feature flag
   first. This is a blocking planning fact for whoever picks up Slice 3, not
   a nice-to-know; flag it at the start of that slice's own design pass
   rather than discovering it mid-implementation when the 33rd flag won't
   decode.

## 2. Message family

Three new message-type pairs. None widens an existing frame — the
AGENTS.md rule ("Optional field is only backward compatible in ONE
direction... an old extension reading a new daemon's frame" is fatal) means
every new field set lives behind a brand-new `type` string, mirroring how
`institutional_materialization_v1` itself landed as five new pairs rather
than fields bolted onto `job_offer`/`handoff_outcome`. The frozen
timing-only `auth_pending`/`auth_returned` (`extension/src/protocol.ts:218`
`AuthPayload`, `protocol.go` `MsgAuthPending`/`MsgAuthReturned`) and
`session_evidence` (`protocol.ts:224` `SessionEvidencePayload`) are **not
touched** — see §7.

**Naming collision to watch, not to "fix" by renaming**: the plan's closed
`event_kind` vocabulary (line 314) includes the string `auth_returned`. This
is a value **inside** the new `claim_observation` payload's `event_kind`
field, wire-distinct from the existing top-level message `type:
"auth_returned"` (`MsgAuthReturned`). They must never be confused by a
reader of this doc or an implementer skimming diffs: the old message stays
exactly as `AuthPayload{elapsed_ms?}` with no URL/host ever, forever; the new
`event_kind` value is one string among eight inside a different envelope
entirely. Keep the plan's chosen name — renaming it would only add a second
source of truth to reconcile against the plan text.

Every ID field below reuses the existing opaque-ID convention: 8-128 chars,
`institutionalID`/`institutionalRequestID`-shaped (`protocol.go:4900-4911`,
`requestIDRE`), no URL/host/title/DOI/query/IdP/credential material ever, per
the plan's Slice 3 bullet (line 315) and the schema's standing description
(`protocol/browser-v1.schema.json:5`).

### 2.1 `authentication_claim_request` / `authentication_claim_response`

Precedes any `requires_auth` tab. Job-scoped (carries the envelope's
`job_id`, matching every other job-scoped institutional pair —
`protocol.go:1092-1105` `jobScoped` map gets both new types added). Resolves
the human-surface disposition for one candidate's authentication claim in
one daemon transaction, per the August-reviewed design
(`dev/active/surface-lifecycle-plan.md` lines 322-323).

**`authentication_claim_request`** (extension → daemon):

| field | type | disposition | notes |
|---|---|---|---|
| `request_id` | string | required | correlation id, `institutionalRequestID` shape |
| `candidate_id` | string | required | an existing `browser_candidates.id` from a prior `institutional_candidate_offer` — the daemon resolves `institution_profile_id`/`revision` → `authentication_claim_id` from it (Decision 2: never accepted from the wire) |
| `materialization_kind` | `"browser_tab" \| "direct_download"` | required | mirrors `institutional_claim_request`'s field; a direct-download route can hit the same login wall |
| `trigger` | `"automatic" \| "explicit"` | required | distinguishes an autonomous drive attempt from operator-initiated engagement (Open/focus); the arbitration reducer (§2.1.1) uses this to choose `park` vs `focus_owner` when this candidate is not, and will not become, the owner |

**`authentication_claim_response`** (daemon → extension):

| field | type | disposition | present on |
|---|---|---|---|
| `request_id` | string | required | all |
| `outcome` | enum | required | all — `"navigate_existing" \| "open_new" \| "focus_owner" \| "park" \| "feature_disabled" \| "not_eligible" \| "busy" \| "error"` |
| `detail` | string (≤1000) | optional; **forbidden** on the four operational outcomes | mirrors `InstitutionalClaimResponsePayload`'s `claimed`-forbids-`detail` rule (`protocol.go:4970-4972`) |
| `authentication_claim_id` | string | required on the 4 operational outcomes; forbidden otherwise | lets the extension correlate every subsequent `claim_observation` |
| `browser_holder_generation` | int64 | required on the 4 operational outcomes; forbidden otherwise | current fence (Decision 3) |
| `gate_occurrence_id` | string | required on the 4 operational outcomes; forbidden otherwise | the `human_gate_observations.id` row this claim's login gate is now keyed to (§4.1) |
| `lease_until` | RFC3339 string | required on `navigate_existing`/`open_new`/`focus_owner`; forbidden otherwise | display-only deadline mirroring `authentication_entry_leases.lease_until`; never an authority the extension enforces itself |
| `dependent_count` | int64 ≥ 0 | required on `park` only; forbidden otherwise | Decision 6's "dependent count" — live count of other eligible siblings waiting on this claim (§4.4, no stored counter) |
| `owner_binding_id` | string | required on `navigate_existing`/`focus_owner`; forbidden otherwise | the existing owning surface to act on |
| `owner_tab_hint` | int64 ≥ 0 | optional on `navigate_existing`/`focus_owner`; forbidden otherwise | best-known tab id; the extension still re-proves it live via `tabs.get` before touching it (the in-place-renavigation fence, plan lines 175-184) — this is a hint, never trusted authority |

`open_new` carries **no** `binding_id`. Granting it only clears the
authentication gate; the extension then proceeds through the **existing**,
already-shipped `institutional_claim_request` → `institutional_bind_request`
→ ... sequence for `candidate_id` (now permitted to run because the
authentication arbitration above admitted it). This avoids a second, parallel
binding-mint mechanism competing with the one that already exists and is
already dual-validated.

#### 2.1.1 Arbitration reducer (daemon-side, one transaction)

Resolve `authentication_claim_id` from `candidate_id` (Decision 2). Then:

1. Call `ReserveAuthenticationEntryLease` (existing,
   `internal/job/institutional_evidence.go:1043`) with
   `OwnerID = <job_id owning candidate_id>`, a freshly minted `LeaseID`, the
   current `browser_holder_generation`, and a lease-until computed from the
   configured entry-lease window.
2. **Success, and this is a genuinely fresh reservation** (no live row
   existed, or the prior row was `expired`/human-revoked): `open_new`.
3. **Success as an idempotent replay** (same `LeaseID`/`OwnerID`/generation
   as the current row — i.e. this exact candidate/job already owns it,
   e.g. after a worker restart or a lost response retried):
   `navigate_existing`, echoing the already-known `owner_binding_id` from
   `authentication_entry_leases.owner_binding_id` (§4.1).
4. **`ErrAuthenticationEntryLeaseBusy`** (a different job/candidate owns a
   live lease): `trigger = "explicit"` → `focus_owner` with that owner's
   `owner_binding_id`/`owner_tab_hint`; `trigger = "automatic"` → `park` with
   `dependent_count` (§4.4).
5. Feature not advertised locally negotiated (should not reach the handler,
   defense in depth) → `feature_disabled`. Store error → `error`. A holder
   generation that changed mid-transaction → `stale` was considered and
   rejected: `ReserveAuthenticationEntryLease` already re-validates the
   generation atomically inside its own transaction, so a genuine race
   surfaces as `busy` or a fresh `open_new`, never a distinct `stale` path
   here — keep the outcome vocabulary exactly as small as the states that
   actually occur.

### 2.2 `claim_observation` / `claim_observation_ack`

Job-scoped, fire-and-forget-with-ack (extension sends, daemon acks
correlated by `request_id` — the same shape as `triage_decide`/
`triage_decide_result`, never awaited from inside the inbound chain per
AGENTS.md's "Never `await` a correlated request from inside an inbound-frame
handler").

**`claim_observation`** (extension → daemon):

| field | type | disposition |
|---|---|---|
| `request_id` | string | required |
| `authentication_claim_id` | string | required |
| `binding_id` | string | required — the physical surface the event happened on; by Decision 5's pipeline order (claim→binding→scaffold→bind-ack→route→navigate) a bind ack always precedes any observable wall/login/MFA/challenge/landing/close/error, so this is never optional |
| `materialization_claim_id` | string | optional, echoed only when the extension's local state already associates one with `binding_id` (diagnostic cross-check only — the daemon's authority is `binding_id`, never this field) |
| `browser_holder_generation` | int64 | required |
| `gate_occurrence_id` | string | required — echoes the value the extension last received (from `authentication_claim_response` or a prior `claim_observation_ack`) |
| `observation_id` | string | required — idempotency key (§5) |
| `event_ordinal` | int64 ≥ 0 | required — business order (§5) |
| `event_kind` | enum | required — `"wall_observed" \| "login_started" \| "mfa" \| "challenge" \| "auth_returned" \| "entitled_landing" \| "owner_closed" \| "navigation_error"` |

**`claim_observation_ack`** (daemon → extension):

| field | type | disposition |
|---|---|---|
| `request_id` | string | required |
| `outcome` | enum | required — `"applied" \| "duplicate" \| "stale" \| "rejected" \| "error"` |
| `detail` | string (≤1000) | optional; **forbidden** on `applied`/`duplicate` |
| `gate_occurrence_id` | string | required always — the daemon's **current** occurrence id, which may differ from the request's when the gate rolled over (a fresh sign-out reopened it — the 2026-08-12 amendment's "gate id carries the frame's `msg_id`" behavior); the extension must adopt this value for its next observation |
| `browser_holder_generation` | int64 | required always — current fence |
| `lease_until` | RFC3339 string | required on `applied` for `wall_observed`/`login_started`/`mfa`/`challenge`; forbidden otherwise — realizes "renewed only by current wall/MFA/challenge observations, never worker-local timers" (plan line 324-326) |

#### 2.2.1 Reducer semantics per `event_kind` (§4 has the storage detail)

- `wall_observed`, `login_started`, `mfa`, `challenge`: human-paced evidence
  the entry is still being worked. Re-call `ReserveAuthenticationEntryLease`
  with the **same** `LeaseID`/`OwnerID`/generation and a fresh `lease_until`
  — the existing store method already treats this as an idempotent renewal
  (`institutional_evidence.go:1100-1112`), so no new renewal code path is
  needed, only a caller. Outcome `applied`, `lease_until` echoed.
- `auth_returned`: write one `profile_evidence` row
  (`verdict='auth_returned', source='auth_return'`) for the candidate's exact
  `institution_profile_id`/`revision` (looked up via
  `binding_id → materialization_claims.candidate_id → browser_candidates`),
  then call `ConvertAuthenticationEntryLeaseToHuman` (existing,
  `institutional_evidence.go:1217`) with that evidence. Promotes
  `reserved → human`. Outcome `applied`.
- `entitled_landing`: confirms the landing is actually on entitled content,
  not just an IdP redirect-back (a wrong-work or re-walled landing after
  `auth_returned` must not resume siblings). This is the event that
  triggers the **existing** materialization scheduler
  (`Bridge.Sync` → `ScheduleEligibleBrowserCandidates`,
  `bridge.go:1026-1055`) to naturally pick up dependents — no new scheduling
  code, because eligible siblings already sit in `browser_candidates` with
  `status='eligible'` (§4.4) and the scheduler already polls that state on
  every holder-generation or schedule-version change.
- `owner_closed`: the owning surface closed without success. Marks the live
  `materialization_claims` row (for `binding_id`) `abandoned`, clears
  `authentication_entry_leases.owner_binding_id`/`owner_tab_hint` for the
  claim (lease itself stays `human` or `reserved` per its own expiry —
  closing a tab is not evidence about the sign-in outcome), and leaves
  dependents tabless per Decision 6's "owner closure without success commits
  abandonment and leaves dependants tabless" (plan line 170-171).
- `navigation_error`: daemon-committed park with no auth charge, no cooldown
  — the plan's Slice 1/3 invariant (line 191-194). Never touches the entry
  lease.

**Stale/rejected, never disconnect** (bridge.go's structured-outcome rule,
AGENTS.md "Every handler in `internal/browser/bridge.go` MUST encode
ordinary/expected failures... into a structured outcome/detail result...
never return a raw Go error"):

- `stale`: `browser_holder_generation` below the daemon's current fence, or
  `event_ordinal` not strictly greater than the last applied ordinal for
  this `gate_occurrence_id` (§5) — **application-level**, ack `stale`,
  connection stays up, extension drops the local retry.
- `rejected`: `event_kind` inconsistent with current claim state (e.g. `mfa`
  after the lease already expired to a different owner, or
  `materialization_kind` mismatch) — **application-level**, ack `rejected`.
- `error`: unexpected daemon-side failure (store error mid-transaction) —
  still a structured ack, never a raw error, per the same rule; the caller
  retries with the same `observation_id`.

Only genuine transport/framing failures — an undecodable frame, a wire
size-cap violation, a daemon outbound self-validation failure — are fatal,
exactly as today (`bridge.go:901-905` `ErrOutboundFrame`,
`nativehost` fail-fatal classification cited in AGENTS.md). Nothing in this
family introduces a new transport-fatal condition.

### 2.3 `surface_close_request` / `surface_close_response`

Generic — not authentication-specific — so Slice 2b can implement the close
transaction (plan lines 288-299) under `surface_close_v1` alone. Job-scoped
is wrong here (a scaffold being closed may have no live job, e.g. an
abandoned claim's scaffold after `owner_closed`), so this pair carries **no**
`job_id` and is **not** added to `jobScoped`.

**`surface_close_request`** (extension → daemon):

| field | type | disposition |
|---|---|---|
| `request_id` | string | required |
| `binding_id` | string | required |
| `browser_holder_generation` | int64 | required |
| `disposition` | enum | required — `"scaffold_idle" \| "materialization_settled" \| "claim_abandoned"`, the three closed-permitted reasons from the plan's re-permitted-narrowly amendment (lines 148-150): idle scaffold never engaged, settled after artifact win, or an authentication claim's abandonment (`owner_closed` reducer above) |
| `gate_occurrence_id` | string | optional — populated only when `disposition = "claim_abandoned"`, echoing the occurrence that closed |

**`surface_close_response`** (daemon → extension):

| field | type | disposition |
|---|---|---|
| `request_id` | string | required |
| `outcome` | enum | required — `"authorized" \| "stale" \| "not_eligible" \| "busy" \| "error"` |
| `close_authorization_id` | string | required on `authorized`; forbidden otherwise |
| `nonce` | string | required on `authorized`; forbidden otherwise |
| `browser_holder_generation` | int64 | required on `authorized`; forbidden otherwise |
| `detail` | string (≤1000) | optional; forbidden on `authorized` |

This directly satisfies the cross-slice contract already fixed for this
phase: `extension/src/ledger.ts`'s `SurfaceBirthRecord.pending_close` shape
(`{ authorization_id, nonce, holder_generation, recorded_at }`, written by
the sibling Slice 2a agent this same session) is populated **exactly** from
`close_authorization_id`, `nonce`, and `browser_holder_generation` on this
response — the tombstone-before-remove step in the plan's close transaction
(line 291, "tombstone persisted (tab ID, binding, generation, authorization,
nonce) before `tabs.remove`") writes what this frame hands it, field for
field, with `recorded_at` stamped locally by the extension at receipt.
`not_eligible` covers every fail-closed re-check the transaction performs
before closing (still bound, unceded, inactive, non-PDF, not adopted — plan
line 293): the daemon and extension both re-verify independently, but only
the extension can observe live tab state, so `not_eligible` here is the
daemon-side half (binding no longer matches the claimed disposition) and the
extension's own fresh `tabs.get` re-check (plan line 293) is a **local**
refusal that never calls this RPC at all — no wire round trip needed to say
"I decided not to close after all."

## 3. Ordering and idempotency

- **Business order is `(gate_occurrence_id, event_ordinal)`, never native
  receipt order.** `inboundChain`/`onInbound` (background.ts) is FIFO per
  port generation only — a worker restart, a lost response, or a retried
  frame can all reorder or duplicate delivery relative to when events
  actually happened in the browser. The daemon reducer keys off the pair,
  not arrival order.
- **Idempotency table**: new `claim_observation_journal` (§4.2),
  `observation_id` as primary key. A `claim_observation` whose
  `observation_id` already has a row is **not reapplied** — the daemon reads
  back its recorded `event_ordinal`/`gate_occurrence_id`, confirms they match
  the replayed frame (mismatch on a supposedly-identical id is `rejected`,
  never silently accepted), and acks `duplicate` without touching lease,
  evidence, or scheduler state a second time.
- **Monotonic-apply**: within one transaction, before touching any lease/
  evidence/scheduler state, `SELECT MAX(event_ordinal) FROM
  claim_observation_journal WHERE gate_occurrence_id = ?` (or `0` absent).
  `event_ordinal <= current_max` on a **new** `observation_id` is `stale` —
  a late, superseded event, rejected without mutation. This mirrors the
  existing exact-ordinal CAS pattern already used for
  `institutional_route_request.expected_effect_ordinal`
  (`protocol.go:1040`) rather than inventing a second idiom.
- **Stale holder generation cannot revive**: every reducer path above checks
  `browser_holder_generation` against the daemon's current epoch before
  mutating (Decision 3). A stale generation's event is `stale`, ack'd, no
  disconnect — an old holder's browser is still allowed to *report* what
  already happened locally (bounded diagnosis, Decision 4), just never to
  mutate current state.
- **A parse failure retains, resolves nothing** — an undecodable
  `claim_observation` frame fails `Sync` closed for that call (existing
  `ErrInvalidFrame` path, `bridge.go:984-986`); nothing about a surface is
  concluded from silence.

## 4. Authentication-entry lease and storage sketch

Migration numbers: **`0041`** and **`0042`** (next free after
`0040_job_credit_share.sql` — verified via `internal/store/migrations/`
listing; no gap to fill).

**Deviation from the split originally sketched here, landed during Slice
2b's implementation:** `0041_close_authorizations.sql` carries **only**
the `close_authorizations` table (§4.3, SQL verbatim) — nothing else. The
`authentication_entry_leases` `ALTER` (§4.1) and the new
`claim_observation_journal` table (§4.2) move to Slice 3's own `0042`
migration. The reason is sequencing, not storage: the plan sequences Slice
2b strictly before Slice 3 (`Sequencing: 0 → 1 → 2a → 2b → 3 → 4`, plan
line 357), and Slice 2b's close transaction (plan lines 288-299) needs
only `close_authorizations` under `surface_close_v1` — it has no
dependency on the authentication-claim arbitration tables §4.1/§4.2 exist
for. Bundling all three into one `0041` would have made Slice 2b's
migration wait on Slice 3 schema that Slice 2b never reads, for no
protocol reason. Whoever implements Slice 3 mints `0042` from
`authentication_entry_leases`'s `ALTER` and `claim_observation_journal`'s
`CREATE TABLE` exactly as sketched in §4.1/§4.2 below; nothing about
either table's shape changes because of the split.

Bump the four `user_version`-pinned assertions AGENTS.md names as a
checklist item for whoever implements each migration:
`internal/cli/clean_install_test.go` (two "schema version N" strings plus
a `user_version` compare), `internal/doctor/doctor_test.go`,
`internal/store/migrate_forward_test.go`'s two post-migration compares,
and `internal/store/migrate_guard_test.go`'s
`TestOpenRefusesSchemaNewerThanBinary` (the exact-string refusal
assertion, the one AGENTS.md calls "the easiest to miss"). Leave
`TestGuardCapableSchema33RefusesSchema34` and `migrate_forward_test.go`'s
schema-33 fixture untouched, per the same note.

### 4.1 Reuse, don't duplicate: `authentication_entry_leases`

This table **already exists** (migration `0029`) with exactly the shape
Slice 3 needs: `authentication_claim_id` (PK), `lease_id`, `owner_id`
(a `job_id`), `browser_holder_generation`, `state`
(`reserved`/`human`/`expired`), `lease_until`, `human_owner_id`,
`evidence_observation_id`. Its store methods
(`ReserveAuthenticationEntryLease`, idempotent replay included;
`ConvertAuthenticationEntryLeaseToHuman`; `ExpireAuthenticationEntryLease`)
are complete and tested (`internal/job/institutional_evidence_test.go`) but
have **zero production callers** — Slice 3's arbitration reducer (§2.1.1)
and observation reducer (§2.2.1) are the first callers. Migration `0042`
(Slice 3; see the split note at the top of §4) **alters** this table
rather than adding a parallel one:

```sql
ALTER TABLE authentication_entry_leases ADD COLUMN owner_binding_id TEXT;
ALTER TABLE authentication_entry_leases ADD COLUMN owner_tab_hint INTEGER
  CHECK (owner_tab_hint IS NULL OR owner_tab_hint >= 0);
```

Set when the owning candidate's `institutional_bind_response` lands (join
`owner_id` = job → its live `browser_candidates` row → `materialization_claims
.binding_id`/`.tab_id`); cleared on lease reassignment (`open_new` for a
different owner) or `owner_closed`. This avoids inventing a second
"who owns this claim's surface" record that could drift from the lease row
that already exists.

**No new `authentication_claims` registry table.** `authentication_claim_id`
remains exactly what Decision 2 says it is: a value the daemon computes
deterministically from institution-profile authority
(`institution_profiles.authentication_claim_id`, migration `0026`) — every
read in this design (§2.1.1, §2.2.1, §4.4) joins through
`institution_profiles` and `authentication_entry_leases`, which is a stable
enough pair of parents that a third table would only be a cache that could
go stale, not new authority.

### 4.2 New: `claim_observation_journal` (Slice 3's `0042`)

Idempotency and ordering ledger for §3, and the append-only home for what
`profile_evidence`/`human_gate_observations` intentionally do **not** track
(raw per-event replay safety, as opposed to the current-verdict/current-
occurrence projections those tables already are). Lands in `0042` beside
§4.1's `authentication_entry_leases` `ALTER` (see the split note at the
top of §4) — nothing about this table's shape changes because of the
split.

```sql
CREATE TABLE claim_observation_journal (
  observation_id            TEXT PRIMARY KEY
    CHECK (length(observation_id) BETWEEN 1 AND 128),
  gate_occurrence_id        TEXT NOT NULL
    REFERENCES human_gate_observations(id),
  authentication_claim_id   TEXT NOT NULL
    CHECK (length(authentication_claim_id) BETWEEN 1 AND 256),
  binding_id                TEXT NOT NULL
    CHECK (length(binding_id) BETWEEN 1 AND 256),
  browser_holder_generation INTEGER NOT NULL CHECK (browser_holder_generation >= 0),
  event_kind                TEXT NOT NULL CHECK (event_kind IN
    ('wall_observed','login_started','mfa','challenge','auth_returned',
     'entitled_landing','owner_closed','navigation_error')),
  event_ordinal              INTEGER NOT NULL CHECK (event_ordinal >= 0),
  applied_at                 TEXT NOT NULL
);
CREATE UNIQUE INDEX claim_observation_journal_ordinal
  ON claim_observation_journal(gate_occurrence_id, event_ordinal);
CREATE INDEX claim_observation_journal_by_claim
  ON claim_observation_journal(authentication_claim_id, applied_at DESC);
```

The unique `(gate_occurrence_id, event_ordinal)` index enforces the
monotonic-apply rule (§3) at the schema level as a second line of defense
behind the transactional `MAX()` check; a bug that races two concurrent
appliers into the same ordinal fails the `INSERT` instead of silently
double-applying.

### 4.3 New: `close_authorizations` (shipped in `0041`)

One-use tokens for §2.3, deliberately its own table (not folded into
`materialization_claims`) because a close authorization can be issued for a
scaffold that never had a live materialization claim row at all (an idle,
never-engaged scaffold that timed out before any claim advanced past
`claimed`).

```sql
CREATE TABLE close_authorizations (
  id                          TEXT PRIMARY KEY
    CHECK (length(id) BETWEEN 1 AND 128),
  binding_id                  TEXT NOT NULL
    CHECK (length(binding_id) BETWEEN 1 AND 256),
  browser_holder_generation   INTEGER NOT NULL CHECK (browser_holder_generation >= 0),
  nonce                       TEXT NOT NULL CHECK (length(nonce) BETWEEN 1 AND 128),
  disposition                 TEXT NOT NULL CHECK (disposition IN
    ('scaffold_idle','materialization_settled','claim_abandoned')),
  status                      TEXT NOT NULL CHECK (status IN
    ('issued','consumed','expired')),
  issued_at                   TEXT NOT NULL,
  consumed_at                 TEXT
);
CREATE UNIQUE INDEX close_authorizations_live_binding
  ON close_authorizations(binding_id)
  WHERE status = 'issued';
CREATE INDEX close_authorizations_by_status ON close_authorizations(status);
```

The partial unique index enforces "at most one live authorization per
binding" the same way `materialization_claims_live_candidate`
(migration `0026`) enforces "at most one live claim per candidate" — matching
an established idiom rather than inventing a new one. Token issuance
(`internal/job.Store.IssueCloseAuthorization`) is idempotent per binding: a
repeated `authorized` request for the same live binding returns the SAME
`close_authorization_id`/`nonce` rather than racing a second token into
existence against the partial unique index; a repeat carrying a strictly
higher `browser_holder_generation` re-stamps the row, and a live token with
a *different* disposition than the one now requested is refused as `busy`
rather than silently repurposed. Consuming a token (reported by a later
reconcile pass or the `onTabRemoved` tombstone-replay path Slice 2b owns)
sets `status='consumed', consumed_at=now`; that consumed-marking write
itself arrives with Slice 3's `owner_closed` reducer (§2.2.1) and Slice
2b's own `onTabRemoved` path, not with this migration. Expired-but-never-
consumed tokens are swept by `internal/job.Store.ExpireCloseAuthorizations`,
a plain housekeeping pass with no reducer of its own. Rows are never
deleted, so a startup reconciliation pass can distinguish "never asked"
from "asked and already used" from "asked and it timed out."

### 4.4 Dependent count: derived, not stored

`dependent_count` (§2.1) and sibling resumption (§2.2.1 `entitled_landing`)
both come from a **live query**, not a maintained counter — matching this
codebase's general aversion to redundant mutable state (`artifact_winners`
is insert-only; `job_credit_share`'s comment explicitly rejects a
progress-triggered reset in favor of a monotonic value read fresh):

```sql
SELECT COUNT(*) FROM browser_candidates bc
JOIN institution_profiles p
  ON p.id = bc.institution_profile_id AND p.revision = bc.institution_profile_revision
WHERE p.authentication_claim_id = ? AND bc.status = 'eligible';
```

A `park`ed dependent's `browser_candidates` row simply stays `eligible` —
the **existing** `ScheduleEligibleBrowserCandidates` scheduler
(`bridge.go:1026-1055`, already polled every `Sync` on holder-generation or
schedule-version change) naturally picks it up once eligibility genuinely
opens (the `entitled_landing` reducer path writes nothing scheduler-specific
at all; it only ensures the claim's evidence/lease state is now truthfully
"resolved," and the existing eligibility predicate does the rest). No new
scheduling code is in scope for this protocol design.

### 4.5 Lease semantics, restated precisely

- Granted only by the arbitration transaction (§2.1.1), never by an
  observation.
- Renewed only by `wall_observed`/`login_started`/`mfa`/`challenge`
  (§2.2.1) — human-paced, because a login/MFA/challenge prompt routinely
  outlives the arbitrary action-expiry window a worker-local timer would
  otherwise apply (the closed 2026-08-12 amendment already states this
  rationale for claim renewal generally).
- A restarted worker reconciles before renewing: on `hello`/promotion, the
  extension's existing `materializationRecoveryPending`/
  `reconcileMaterializationGeneration` path (`bridge.go:833,842-856`,
  already shipped for the materialization claims layer) is the model to
  extend — before a freshly promoted holder generation may renew any
  authentication-entry lease, it must first replay any outstanding
  `claim_observation`s buffered locally (durable in `chrome.storage.session`
  per the plan's storage-tier design, line 134-136) so the daemon's
  `event_ordinal` high-water-mark reflects reality before a renewal could
  otherwise appear to move it backward.
- An event from an old holder generation cannot revive a lease: every
  reducer path checks the generation before mutating (§3).
- Expiry alone never authorizes replacement while an effect permit is
  unresolved: `ReserveAuthenticationEntryLease` already checks
  `ownerTerminal` (job state) as part of what counts as "expired" — Slice 3
  additionally must check, before treating a `reserved` lease as replaceable
  on expiry, that no `effect_permits` row
  (`internal/store/migrations/0034_effect_permits.sql`) with
  `effect_kind='institutional'` is still `held`/`unknown_completion` for the
  owning candidate's `claim_id`/`binding_id` — an unresolved browser-local
  effect (a navigation genuinely in flight) must keep occupying even past a
  timer, per ADR-0022 Decision 3's "effect-permit occupancy does not
  expire."

## 5. Size/cap analysis

Every field in every new payload is either a bounded opaque ID (≤256 chars,
matching `institutionalID`'s existing 8-128 bound or the wider IDs already
used elsewhere in this family), a small enum, or a bounded integer
(`MaxBrowserInteger`, `protocol.go:44`). Worst case for the largest new
frame, `claim_observation` (9 fields, none longer than 256 bytes, most far
shorter): well under 3 KB. That is trivial against
`protocol.MaxBrowserMessageBytes` (256 KiB, `protocol.go:40`) and doubly
trivial against `ipc.MaxRequestBytes` (512 KiB) / `ipc.MaxResultBytes`
(1 MiB, `internal/ipc/protocol.go:20-29`) — the pair AGENTS.md's transport-
cap footgun requires checking in both directions, and the reason that check
matters at all is the MDPI incident it documents (a 545 KB page captured
into a 102 KB frame silently exceeded the *old* 64 KiB IPC cap and tore down
the whole browser session as a fatal transport failure, not an application
error) — small-by-construction frames do not make this check optional, they
make it a one-line confirmation instead of a redesign.

**The real cap risk is batch size, not frame size.** A native host poll is
one `browser.sync` IPC call carrying every queued inbound frame; a burst of
buffered `claim_observation`s (e.g. many candidates independently hitting
`navigation_error` after a wake-from-sleep network drop, per the plan's Wake
flood test scenario) could otherwise grow unboundedly in one poll. Bound it
the same way `maxFocusFramesPerPoll` (`bridge.go:139-148`, pinned by
`TestSyncResponseFitsResultCap`) already bounds focus frames: the extension
MUST send at most **32** queued `claim_observation` frames per `Sync` call,
carrying the remainder to the next 2-second poll. At worst-case ~3 KB each,
32 frames is ~96 KB inbound — comfortably inside `MaxRequestBytes`, and the
correlated 32 acks outbound are smaller still, comfortably inside
`MaxResultBytes` alongside everything else one `Sync` reply already carries.
This cap belongs beside `maxOutstandingOffers`/`maxFocusFramesPerPoll` in
`bridge.go`'s const block, not as a new standalone magic number.

`authentication_claim_request`/`response` and `surface_close_request`/
`response` are naturally one-at-a-time correlated calls (a candidate asks
once, a close is asked once); no batching rule needed for those.

## 6. Four-site parity checklist and rollout

Parity means the same four artifacts every prior institutional slice
touched together: **Go validator** (`internal/protocol/protocol.go`
`validate()` + `decodeBrowserMessage` dispatch + `jobScoped`/feature
constants), **TS parser** (`extension/src/protocol.ts` `FieldSpec`
interfaces + `parseBrowserMessage` cases + `MSG_TYPES`/`JOB_SCOPED`),
**JSON Schema** (`protocol/browser-v1.schema.json` `type` enum, `$defs`,
`allOf` per-type schema), and **corpus**
(`testdata/protocol/valid`/`testdata/protocol/invalid` — both Go and TS
tests assert against the identical corpus per `protocol.go`'s package
doc comment). Land all four in one commit per the standing pre-install
compatibility floor (AGENTS.md "a breaking change to `papio-browser/1` is
allowed when ... all land in the same commit, and the extension is rebuilt
and reloaded alongside the daemon"), same discipline `effect_permit_v1`
used (ADR-0022 Amendment 2026-08-13).

Rollout order, mirroring Slice 4's already-decided pattern
(`dev/active/surface-lifecycle-plan.md` lines 350-352) rather than inventing
a new one:

1. **Daemon/host first, dark.** `surface_close_v1` and
   `institutional_authentication_claim_v1` land in `NewBridge`'s `required`
   list; every new handler exists and answers real outcomes, but nothing
   *daemon-initiated* changes — the daemon never sends an unsolicited
   authentication-claim frame, only answers requests. Ship and deploy with
   zero extension change; `papio doctor`/`browser sessions` show the new
   features advertised to any connected extension immediately (existing
   extensions ignore unrecognized-but-unused features fine, since these are
   pull-only from the extension side).
2. **Extension second, emits nothing until `hello_ack` advertises.** The
   Slice-0-shipped gate (`AUTHENTICATION_CLAIM_FEATURE` check,
   `background.ts:310` plus its consumers) already enforces this — it is
   the reason Slice 0 could ship containment before this protocol existed.
   No extension-side gating code is new; only the request-sending code
   behind that already-closed gate is new.
3. **Mixed-version evidence.** Run the plan's Test scenario 1 (old daemon /
   no feature: zero autonomous surfaces, explicit Open still works) and
   scenario 2 (update simulation) against this exact feature pair before
   calling Slice 3 done.
4. **Daemon enables the claim-bound automatic path** only after connectivity
   admission (Slice 0's online probe) and this protocol both pass — matching
   Decision 3's "connectivity admission precedes route issuance by
   construction," carried into this slice rather than re-litigated.

### Mixed-version behavior table

| Extension | Daemon | Behavior |
|---|---|---|
| old (no `institutional_authentication_claim_v1` check) | old (feature absent) | Unchanged: legacy federated-login claim code still runs exactly as today (retired only in Slice 3's own cutover, plan line 337-339, which ships *with* this protocol, not before it) |
| old (no check) | new (feature advertised) | Extension never asks; daemon never offers unsolicited claim traffic; daemon's new tables/handlers sit unused for that session — safe no-op |
| new (Slice-0 gate present) | old (feature absent) | Slice 0's shipped behavior: gate stays closed, job parks tabless with `engagement_required`, explicit Open still works via the fresh-link path — **the permanent degraded-compatibility state**, not a transitional one (plan line 244-246, "there is no legacy pre-open fallback") |
| new | new (feature advertised) + online probe passes | Full arbitration: `authentication_claim_request` flows, claim-observation stream begins, close authorizations available |
| new | new (feature advertised), probe fails | Gate stays closed on the connectivity half alone (Slice 0's existing rule), independent of this protocol being present |

## 7. Non-goals

- **No `Response`/IPC envelope widening.** Every new field lives inside a
  brand-new `payload` shape behind a brand-new `type` string, never inside
  the shared envelope (`internal/ipc` `Request`/`Response`,
  `protocol/browser-v1.schema.json`'s top-level `protocol`/`type`/`msg_id`/
  `seq`/`payload` properties) and never inside an existing method's result
  shape. Any implementation that finds itself adding an optional field to
  `institutional_claim_response`, `hello_ack`, or any other already-shipped
  payload to carry claim-observation data has violated this design and must
  stop and use a new type instead (AGENTS.md's stated abort criterion, plan
  line 388-390).
- **No widening of the timing-only frames.** `auth_pending`, `auth_returned`
  (the message type), and `session_evidence` keep exactly their current
  fields forever. The new `claim_observation` family is a fully separate
  channel that happens to observe some of the same real-world moments (a
  login wall, an IdP return) — it does not replace, wrap, or gain fields
  from the old ones, and the old ones gain nothing from it either.
- **Phase 5 signed-out-route enablement keeps its own readiness gate.** This
  protocol only builds and enables the *mechanism* for one human sign-in
  surface per authentication claim; it says nothing about, and does not
  unblock, ADR-0022 Phase 5's separate qualification of exact profile/route/
  provider-safety/adapter/identifier tuples for automatic signed-out first
  routing. `canary_ready_route_exists` (Decision 8) stays exactly as
  conservative as it is today; nothing here touches source-gate bypass or
  effect concurrency.
- **No new close path beyond the one generic `surface_close_v1`
  transaction.** The AST close-allowlist test
  (`extension/test/tab-window-close-ast.test.ts`) structurally forbids new
  `tabs.remove`/`windows.remove` call sites; `owner_closed` and the other
  event kinds in this design report *observations about* a close, they never
  themselves author a new `tabs.remove` call site outside the one transaction
  Slice 2b implements.

## 8. Grounded citations (verify-before-cite, per AGENTS.md)

Every citation above was read from the current tree in this session; the
file:line anchors were current as of that read and may drift with unrelated
edits, but the symbols and shapes they describe are real, not inferred:
`dev/active/surface-lifecycle-plan.md` (whole plan, lines cited inline),
`dev/adr/0022-institutional-processing-authority-and-enablement.md`
(Decisions 1-10, Phase 1/3 implementation notes, both amendments),
`dev/adr/0003-browser-session-arbitration.md`, `AGENTS.md` (Protocol section,
migration-checklist footgun, feature-cap footgun, transport-cap footgun,
handler structured-outcome rule), `internal/protocol/protocol.go` (message
type consts, `InstitutionalClaimRequestPayload`/`ResponsePayload` and
siblings, `institutionalID`/`institutionalRequestID`/`institutionalOrdinal`,
`HelloAckPayload.validate`, `MaxBrowserMessageBytes`/`MaxBrowserInteger`,
`jobScoped`), `extension/src/protocol.ts` (`FieldSpec`, `requireFields`,
`MSG_TYPES`/`JOB_SCOPED`, `InstitutionalClaimRequestPayload` and siblings,
hello/hello_ack feature-array validation), `internal/browser/bridge.go`
(`NewBridge`'s `required` feature list, `maxOutstandingOffers`/
`maxFocusFramesPerPoll`, `Sync`'s handler loop and structured-outcome
comments, `promote`/`reconcileMaterializationGeneration`), 
`internal/browser/bridge_test.go:845-848` (hardcoded feature-list
assertion), `protocol/browser-v1.schema.json` (envelope shape,
`$defs`/`allOf` convention), `internal/ipc/protocol.go`
(`MaxRequestBytes`/`MaxResultBytes`), `internal/store/migrations/`
(directory listing for the next-free migration number; `0026`, `0027`,
`0029`, `0034` read in full for existing-table shapes),
`internal/job/institutional_evidence.go` (`AuthenticationEntryLease*` types
and store methods, read in full), `extension/src/background.ts:299-403`
(feature-constant block including `AUTHENTICATION_CLAIM_FEATURE`),
`extension/src/federated-claim.ts` (legacy claim code retired alongside this
cutover per plan line 337-339), `extension/src/ledger.ts` (the Slice 2a
`SurfaceBirthRecord.pending_close` shape this design's `surface_close_response`
must match field-for-field — confirmed consistent, §2.3).
