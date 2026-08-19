# Attempt six (rev 3): *papio* owns every surface it creates

Successor to `dev/active/login-handoff-plan.md` (attempt five). Rev 1
(extension-local durable claims) was rejected by a four-reviewer round —
wrong side of the ADR-0022 authority boundary. Rev 2 rebuilt on the daemon's
machinery; an independent pro-effort oracle review of rev 2 against the raw
sources (verdict: **sound-with-changes**;
`dev/scratch/oracle/20260818T074205Z-surface-lifecycle/answer.md`) confirmed
the foundation and corrected the slice order, the cardinality rule, the
compatibility fallback, and the connectivity sequencing. This revision folds
in all twelve of its prioritized changes.

**Status (2026-08-19): Slices 1, 2a, 2b, 3, and 4 have shipped** (per-slice
markers below), on top of Slice 0 (`b550a9d` + review-fix commit). This file
is not deletable yet: per AGENTS.md's `dev/active/` discipline, a plan
leaves this directory only after whatever is still normative is salvaged
into an ADR, and the design invariants and test scenarios here have not yet
been validated against real institutional traffic in the field — that
evidence, not the code landing, is the remaining gate on deletion.

## The field failure (2026-08-18, operator's browser)

One *papio* tab group with ~17 tabs — six-plus IdP sign-in tabs, several
unrendered Primo tabs, a ScienceDirect ATN-12 error page — plus duplicate
groups/windows across extension reloads, drives fired into a dead network
after wake, and siblings stranded at sign-in walls after the operator
authenticated in one tab. The operator's directive: *papio* owns the full
lifecycle of every surface it creates; a tab is ceded to the user only when
*papio* asks for user action (rare by design) or the user takes the tab over.

## Corrected audit (rev 1 → reviewers → oracle; lines verified pre-Slice-0, 2026-08-18)

Items 3, 5 (ledger erasure), and 6 (wake ordering, missing online signal)
describe the tree BEFORE Slice 0 shipped (`b550a9d` + its review-fix commit)
and are annotated below; the rest still describes HEAD.

1. **Automated drives leave one tab per auth wall — and the drive limit is
   not a surface limit.** `HANDOFF_DRIVE_LIMIT = 1` caps tabs that may
   *drive an effect* (`background.ts:168-171`); the waiting-for-session
   (4393), auth-stall (`reportAuthStalled` 10506-10511), challenge
   (10694-10732), and fresh-link timeout parks preserve their live tab while
   releasing the slot (the legacy timeout park detaches and closes its tab,
   4213-4226), so one nominal drive coexists with a 17-tab group. The claim
   gate runs only inside `maybeRouteFederatedLogin` (15104+), **after**
   navigation and only for adapters with a `federatedLogin` template; the
   generic off-provider path takes no claim and charges an auth attempt
   (`auth_pending` send + `noteAuthAttempt`, 13846-13848). Sibling discovery
   of an existing owner retires nothing — the redundant wall tab is kept
   (`parkHandoffWaitingForSession`).
2. **Stranded siblings are a park-type mismatch.** Fresh-link timeout parks
   set `auth_pending` via `parkHandoffForManual` and never set
   `waiting_for_session` (4213-4218); every cross-job resume path filters on
   `waiting_for_session`/claim key or matches origins from `offerURLs`
   (`resumeWaitingForSessionJobs` 14986, `…ByClaim` 15053-15058,
   `…Handoffs` 15041-15044) — both absent for fresh-link jobs
   (`openFreshHandoff` deletes `offerURLs` at materialization, 6023).
   The fix is a daemon claim-resolution transition, **not** a broad
   "session evidence redrives every auth tab" sweep.
3. **A separate sign-in multiplier: the jobless fallback** *(fixed in
   Slice 0: ledger reuse for jobless sign-in opens)*.
   `requestSessionSignIn` falls back to `openManagedTab` without a `jobId`
   (2717-2823; `openManagedTab` calls at 2786, 2814 carry no `jobId`), and
   ledger scanning/reuse runs only under
   `options.jobId !== undefined` — repeated fallbacks mint repeated sign-in
   tabs even when an earlier one is live.
4. **Coordination state is session-scoped; surfaces are durable.** The
   managed store prefers `chrome.storage.session` with a `storage.local`
   fallback when the session API is absent (`state.ts:1645-1652`) — so on
   such runtimes job mirrors DO survive reload/update/restart, and only the
   serializer's migration keeps URL-bearing state out either way:
   `federatedLoginOwners` and `offerURLs` are deleted on every save
   (`state.ts:1615-1618`) — claims are worker-memory.
   Startup adoption is real but **per-window and startup-only**: groups are
   re-found by title then partitioned by `windowId` with a primary per
   partition (3895-3915), so multiple *papio* groups across windows are a
   supported steady state; the work window has no rediscovery at all (stale
   ID ⇒ new window, 3944-3987); and `connect()` precedes reconciliation while
   `onInbound` awaits only storage hydration (4747-4764) — native offers can
   materialize mid-adoption. There is a crash gap between `tabs.create` and
   `foldIntoHandoffGroup` that leaves an ungrouped tab.
5. **Ordinary navigation erases ownership evidence** *(erasure fixed in
   Slice 0: navigated entries are retained but surfaced nowhere — by URL
   alone they are indistinguishable from a recycled tab id, so identity
   proof waits for Slice 2)*. `ledgerManagedTab`
   records the creation URL (3470-3497); after the resolver→SSO→provider
   redirect chain, `classifyLedgeredTabs` used to see a URL mismatch and
   **delete the ledger record**. Reconciliation then closes zero by design
   (`reconcileOwnedTabs` 3594-3597) while startup schedules it as cleanup
   (4853-4855).
   `closeOwnedTab` additionally refuses live-job tabs (`findByTab`,
   3356-3365) and recognizes only the single remembered group ID despite the
   group code keeping one group per window. The timeout callback fires
   `closeOwnedTab(tabID, "timeout")` without awaiting it (4224-4225).
6. **Wake releases before it probes** *(fixed in Slice 0: probe kicks off
   first, releases run only on a connected wake with `navigator.onLine`;
   the 45s evidence bypass is refused for institutional auth work while the
   gate is closed — with the gate open it remains ADR-0009-ratified
   behavior until Slice 3 retires the timer)*. No
   online/probe/navigation-error signal exists anywhere in the background;
   an error document flows into generic auth detection. `classifyRetries`
   is a worker-local Map — MV3 restart resets exhaustion (14826-14829).
   Daemon `goodbye` releases the session only (`bridge.go:923-925`): it is
   transport loss, never terminal job evidence.

## Why five attempts did not land this

- **The replacement architecture already exists, disabled.** ADR-0022 shipped
  Phases 1-3: daemon-owned `materialization_claims`, two-party claim→bind→
  route→navigated→reconcile (`institutional_materialization_v1`), opaque
  self-identifying scaffolds (`materialize.html#<binding>`), paginated
  reconciliation, effect permits. Automatic first-route behavior is off until
  Phase 4/5 readiness. Every attempt patched the legacy offer/drive path this
  machinery is meant to replace; the ADRs forbid a second authority beside it.
- **Scope boundary drawn at the wrong path.** Attempt five fixed cold
  human-action offers (tabless until engagement, fresh URL mint — shipped).
  The pileup lives in the automated warm-session drive path.
- **Risk asymmetry ratcheted into taboo.** Reviews punished closing (real
  TOCTOU P0s) and never punished leaving open; the "do not re-attempt" list
  plus the AST close-allowlist were read as "lifecycle ownership is
  forbidden," freezing policy along with mechanism.

## Architecture decision: finish ADR-0022, do not build beside it

- The **daemon** owns jobs, candidate ordering, the opaque authentication
  claim and its authentication-entry lease, holder generation,
  materialization claims/bindings, typed human-gate occurrences, dependent
  counts, durable park/retry state, sibling resume scheduling, effect
  permits, and terminal/detach dispositions.
- The **extension** owns browser-local facts: physical tabs/groups/windows,
  binding acknowledgements, wall/landing/error observations, operator
  engagement and cession, the current connectivity observation plus a short
  probe lease, and the guarded close primitive. Loss of worker memory never
  authorizes a replacement tab, a close, or a gate resolution.
- **Cardinality (corrected):** one unresolved human sign-in surface per
  **daemon-issued authentication claim** — not "per institution."
  Institution-profile evidence (exact profile+revision) and provider safety
  domains are separate axes: a claim may group profiles sharing one human
  entry; a profile may span unrelated provider safety domains whose parked
  scaffolds are limited independently, alongside the global effect permit.
  Resolving a claim never auto-asserts entitled session evidence for every
  profile grouped under it.
- **Storage tiers:** `chrome.storage.session` holds re-derivable mirrors
  only (binding→tab map, observation outbox, *papio*-issued action tokens,
  page epochs, pending close transaction, advisory deadlines) — loss ⇒
  retain/no-drive, never inference. `storage.local` holds settings plus a
  URL-free birth certificate per owned surface: opaque `binding_id`, tab-ID
  hint, purpose, browser-session epoch, extension generation, creation
  timestamp, cession state, pending-close tombstone. No route URLs, titles,
  DOIs, hosts, entity material, candidate ordering, or accepted-work queue —
  the extension is never a durable queue (ADR-0022 Decision 1). A
  browser-start epoch invalidates old tab-ID authority; after a browser
  restart only a self-identifying scaffold is remapped automatically.
  **Scope note:** this digest-only promise is about the lifecycle state
  this document introduces (the birth certificate above, and the
  claim/observation/close frames in
  `dev/active/claim-observation-protocol.md`). It does not apply to, and
  does not narrow, the pre-existing `institutional_materialization_v1`
  candidate-offer/claim/bind/route family (shipped before this effort,
  `0b716b3`), whose frames carry provider hosts, a bounded DOI/title hint,
  and institution login identifiers by design — the extension needs them
  to navigate and verify a route, mirroring the long-standing `job_offer`
  contract. That route material is never itself persisted (see
  claim-observation-protocol.md §2 scope note for the code citations).

## Amendments to attempt five's "do not re-attempt" list (operator decision)

- **"Automatic waiter-tab closure" is re-permitted, narrowly**: scaffold-only,
  through the close transaction below, never for engaged/active/PDF/adopted
  content. Unknown engagement ⇒ retain.
- **Owner-age/URL-shape liveness stays banned.** Retirement and resume ride
  daemon claim state and explicit transitions only.

Everything else on the list remains banned. Reaffirmed: **missing federation
metadata is a structured engagement failure, never permission to pre-open**;
all `requires_auth` work goes tabless without a granted claim.

## Design invariants (each names its enforcing slice)

- **A claim precedes a surface** (Slices 0/3): no tab for `requires_auth`
  work without a daemon-granted claim; scaffold-first creation; rollback on
  every create/bind/cancel path including the reuse branch (today
  `openManagedTab`'s reusable branch skips the materialization callback,
  3300-3311).
- **One unresolved human surface per authentication claim** (Slice 3):
  enforced by the daemon claim transaction and monotonic event reducer.
- **Resume is a one-shot transition** (Slice 3): keyed by gate occurrence ID
  and event ordinal; duplicates ack idempotently; stale holder/binding/
  ordinal cannot mutate current state; warm probe evidence never mints
  surfaces; owner closure without success commits abandonment and leaves
  dependants tabless.
- **Warm evidence is not a lease** (Slice 3): exact-profile-scoped, admits an
  attempted route only; a wall bounce converges on the claim at wall
  observation.
- **In-place renavigation is fenced** (minimal fence SHIPPED in Slice 0:
  a claim-resume redrive does a fresh `tabs.get` immediately before
  `tabs.update` and never renavigates an operator-active tab — the sibling
  the operator may be typing credentials into. The FULL fence — every
  automatic renavigation, engagement checks, post-update re-read before
  drive registration, non-focusing `openFreshHandoff` variant — needs the
  Slice 2 cession tokens first: today papio cannot distinguish its own
  focus (in-window active creation, stale-IdP work-window raise) from the
  operator's, so a blanket active-tab fence would block its own recovery
  paths; Slices 2/4).
- **Positive evidence closes; absence retains** (Slice 2): close requires the
  one-use daemon authorization below — never absence from a bounded 4-offer
  batch, never `goodbye`, never timer expiry or transport loss.
- **Causal operator cession** (Slice 2): *papio*-issued focus/navigation
  action tokens keyed to tab + document epoch; matching events consume the
  token and do not count as takeover; ambiguity ⇒ engagement ⇒ retain.
- **Every dead end has a daemon-side disposition** (Slices 1/3): navigation
  error (observed before auth detection, no auth charge, no cooldown),
  classify exhaustion (extension reports the exhausted page epoch; the
  daemon commits the terminal park), daemon cancel.
- **`surfaceReady` gates every effect-producing entrypoint** (Slice 2): one
  barrier (managed-state load + birth-record validation + scaffold scan +
  complete paginated reconcile + tombstone replay + group/window adoption)
  awaited by native offers, runtime opens, queue drains, materialization
  retries, and close paths; hello/poll and reads stay responsive; bounded
  scans fail closed to no-adoption/no-close; session-restore grace pass.
- **Lifecycle work never rides the global effect permit** (Slices 2/4):
  adoption scans, lease renewal, claim observations, group folding, and
  terminal reconciliation use a separate lifecycle mutex; only irreversible
  provider navigation, page mutation, and download initiation acquire the
  effect permit.

## Slices

Discipline carried over from attempt five verbatim: one `background.ts` owner
per slice, exact deletion manifests, full-suite runs only, ≤400 changed lines
outside tests per slice, no assertion weakening. Protocol work lands at
four-site parity (Go validator, TS parser, JSON schema, corpus) behind
negotiated features; timing-only `auth_pending`/`auth_returned` frames are
never widened.

### Slice 0 — containment (extension-only; stops new litter now)

**SHIPPED 2026-08-18** (`b550a9d` + review-fix commit; three-reviewer round
applied). Feature gate `institutional_authentication_claim_v1`
(extension-side check only; the daemon defines it in Slice 3) — open only
with a **current** hello on the live port, holder role, the feature, and a
`navigator.onLine` snapshot (`BridgeDeps.online`; a true probe/lease is
Slice 5 follow-on work, so "online" here is a hint, not proof). Gates at
every autonomous entry: offer tail, fresh-link cold/recovery, legacy
recovery, drive-queue create (operator-flagged requests exempt), startup
reopen/requeue, 45s release, forced release, opportunistic drain,
claim-resume auto-drive, stale-IdP recovery when its tracked tab is gone.
Probe-before-release wake ordering; ledger retention on navigation
(surfaced nowhere until Slice 2 identity); jobless sign-in tab reuse (known
limit: a recycled tab id parked on an IdP page can be wrongly focused —
browser-restart identity is Slice 2). Legacy engagement parks keep their
offer URL, survive hydration, and open via the forced queued release;
fresh parks mint. Pinned by the "Slice 0:" test block in
`extension/test/background.test.ts`. The AUTH_CLAIM-negotiating tests pin
the LEGACY gate-open machinery and are deletion-manifest entries for
Slices 3/4.

The smallest slice with most operator value; no aggressive cleanup.

- No autonomous `requires_auth` surface unless the daemon advertises the
  authentication-claim feature **and** the current connectivity probe passes.
  Old daemon or failed probe ⇒ the job stays tabless with
  `engagement_required`; explicit Open remains via attempt five's fresh-link
  path. This is also the permanent degraded-compatibility behavior — there is
  **no** legacy pre-open fallback (store extensions auto-update against
  hand-updated daemons; the legacy path *is* the field failure).
- Wake ordering reversed: probe before any release; the 45s
  evidence-bypassing queued-handoff release is disabled for institutional
  authentication work.
- `classifyLedgeredTabs` stops deleting ledger records on URL change
  (navigation is a lifecycle transition, not lost ownership) — but legacy
  URL-ledger records never authorize auto-closing remote content.
- The jobless `requestSessionSignIn` fallback reuses a live ledger-owned
  sign-in tab instead of minting another.
- Slice 0 adds **no new close paths** (the AST close-allowlist is
  untouched). The pre-existing timeout/cancel close of an inactive ledgered
  in-surface tab — which can remove navigated non-PDF remote content on a
  legacy URL ledger record — remains until Slice 2's close transaction
  replaces it; scaffold-only, daemon-authorized closure is a Slice 2
  deliverable, not a shipped Slice 0 property.

### Slice 1 — harness seams (test code)

**SHIPPED 2026-08-18** (`e2c7c45`).

Network/online/offline seam and navigation-error events in `BridgeDeps` +
`fake-tabs.ts` (genuinely absent today); a lifecycle helper formalizing the
update-simulation pattern (`background.test.ts:6035-6061`); full background
teardown between claim/create/bind/route/close/ack steps (Firefox event-page
timing); durable-ledger assertions. Navigation-error handling ordered before
generic auth detection lands here with its seam.

### Slice 2 — durable identity, adoption, and the close transaction

**SHIPPED**: 2a `e2c7c45` (2026-08-18), 2b `06892c7` (2026-08-18).

Pre-split (size): **2a** schema/migration, **2b** adoption + close.

- 2a: the `storage.local` birth certificate (above) as the ledger's URL-free
  successor; legacy raw-URL entries redacted on migration; entries without
  `jobID` retained for manual review. The record is a birth certificate for
  a daemon binding — never a second claim or scheduling record. **Shipped
  as designed**: `ManagedTabLedgerEntry`, `PRIVATE_HANDOFF_LEDGER_URL`, and
  `managedTabURLFamily` are deleted from `background.ts`; `ledger.ts`'s
  `SurfaceBirthRecord` carries only an opaque `binding_id`, an
  `origin_digest` (SHA-256 of scheme+host+port), and bookkeeping fields —
  no route, host, title, or DOI.
- 2b: restart-class validation (SW restart: IDs valid, session intact;
  update: IDs valid, session wiped; browser restart: all IDs invalid) —
  every adopted ID re-proven via `tabs.get`/query plus scaffold identity
  before any claim/owner revalidation; session-restore timing gets bounded
  grace/retries (absence ≠ dead). Group adoption starts from positively
  owned member tabs and derives group/window — a *papio*-titled group with
  no owned member is never adopted, merged, or closed; the work window is
  rediscovered only through an owned member or extension sentinel.
  `surfaceReady` barrier wired into every entrypoint.
- The close transaction: (1) extension reports the disposition against
  current claim/binding/holder generation/occurrence; (2) daemon transaction
  commits state and returns a one-use `close_authorization_id` for that
  exact binding; (3) tombstone persisted (tab ID, binding, generation,
  authorization, nonce) before `tabs.remove`; (4) one fresh `tabs.get` —
  still bound, unceded, inactive, non-PDF, not adopted — with no unrelated
  await before remove; (5) `onTabRemoved` consumes the exact tombstone and
  suppresses the user-cancel path; (6) failed remove or worker death leaves
  a reconciliable tombstone revalidated with the daemon at startup; (7) a
  tab that became active/navigated/kept is marked ceded and retained, its
  binding detached. Every close call is awaited (the fire-and-forget at
  4224-4225 is removed).
- **Migration promise, narrowed:** only exact current bindings and
  self-identifying scaffolds are cleaned automatically; pre-cutover
  ambiguous tabs (ledger evidence already erased) go to a bounded one-time
  operator review. No inference from group/window/title. **Shipped**: the
  review queue surfaces through the existing popup stray-tabs card
  (`orphanTabStatus`/`legacyLedgerReview` in `background.ts`).

### Slice 3 — claim-observation protocol and the authentication-entry lease

**SHIPPED**: protocol design gate `e2c7c45` (2026-08-18); implementation
`7662f6a` (2026-08-18).

Daemon-side, on the shipped Phase 1-3 projections. **Gated on a written
protocol design** (four-site parity) before implementation:

- New feature-gated correlated claim-observation method family. Each
  observation carries `request_id`, `authentication_claim_id`,
  `materialization_claim_id`, `binding_id`, `browser_holder_generation`,
  `gate_occurrence_id`, `observation_id`, `event_ordinal`, `event_kind`
  (wall observed / login started / MFA / challenge / auth returned /
  entitled landing / owner closed / navigation error) — no URL, host,
  title, query, IdP material, or credentials. Business ordering comes from
  occurrence + ordinal, never native receipt order (`inboundChain` is FIFO
  per port generation only; the transport is fail-fatal on framing errors).
  Unacked events replay with the same ID; the daemon reducer is idempotent
  and monotonic — duplicates ack, stale transitions are rejected without
  disconnecting; a parse failure retains surfaces and resolves nothing.
- One transaction arbitrates claim requests (`navigate_existing` /
  `open_new` / `focus_owner` / `park` — the August-reviewed design) and
  issues the authentication-entry lease. Human-paced flows renew the lease
  through current wall/MFA/challenge observations, never worker-local
  timers; a restarted worker reconciles before renewing; an event from an
  old holder generation cannot revive a lease; lease expiry alone never
  authorizes a replacement while an effect permit is unresolved.
- Extension consults the claim **before** any `requires_auth` tab exists;
  ungranted work parks tabless daemon-side. The login surface is one tab
  with a dependent-sibling count (Decision 6).
- Successful resolution schedules eligible siblings through the daemon:
  fresh revalidated route per sibling (routes stay transient), renavigation
  only through the fence, slot reservation before mint, per-job mint latch
  (today `openFreshHandoff` materializes before the drive-limit check and
  has no latch).
- Legacy extension claim code (`federatedLoginOwners`, v2 hash keys,
  `parkHandoffWaitingForSession` collision path) retired in the same
  cutover — no second authority. **Shipped**: `extension/src/federated-claim.ts`
  is deleted; `federatedLoginOwners`/`parkHandoffWaitingForSession` no
  longer exist as code (comment-only references to the retirement remain).

### Slice 4 — automatic drives ride materialization claims (Phase 5 cutover)

**SHIPPED 2026-08-19** (`5b866d2`).

Scaffold-first becomes structural: every automatically owned institutional
tab begins as the opaque `materialize.html#<binding>` scaffold —
claim → binding → inactive scaffold tab → bind ack (both create **and**
reuse branches) → revalidation → connectivity admission + effect permit →
transient route → same-tab navigation → navigated ack. Persisted offer
URLs disappear (**shipped**: `storage.local`'s birth certificate carries
no route; the in-memory `offerURLs` map that remains is worker-only and
excluded from every save, serving the still-separate explicit-engagement
`handoff_link_v1` path, not automatic materialization); re-offers
revalidate the claim instead of re-opening; the
4-per-poll offer batch becomes claim-paced. Connectivity admission precedes
route issuance by construction. Rollout order: daemon/host first (feature
advertised, automatic routing dark) → extension second (emits nothing until
`hello_ack` advertises) → mixed-version evidence → daemon enables the
claim-bound automatic path. Structural migration is distinct from ADR-0022
Phase 5 *enablement* of signed-out first routes, which keeps its own
readiness criteria — the litter fix does not wait for provider readiness.

Sequencing: 0 → 1 → 2a → 2b → 3 → 4. Slice 0 stops the bleeding on day one;
1-2 build the identity and close machinery that 3-4 stand on. Rev 2's order
(closes before durable identity; connectivity last; degraded legacy
fallback) was reviewed as unsafe and is retracted.

## Test scenarios

1. Old daemon / no feature: zero autonomous `requires_auth` tabs; explicit
   Open still works; nothing legacy pre-opens.
2. Update simulation: session wiped, fakes survive, new Bridge — zero new
   groups/windows, scaffolds adopted or retained, none closed without a
   one-use authorization, no duplicate claim surfaces after re-offers.
3. N institutional jobs, cold session: exactly one sign-in tab per
   authentication claim, N−1 daemon parks; login succeeds once ⇒ all N
   resume (fresh routes), scaffolds retire; owner closed without success ⇒
   zero new surfaces; duplicate/late claim events (old holder generation,
   lower ordinal) mutate nothing.
4. Wake flood: 4 offers, network down ⇒ zero tabs (probe precedes release);
   online + probe ⇒ paced drives.
5. Nav-error and classify exhaustion: daemon-committed park + scaffold
   close, no auth charge, no cancellation emitted (tombstone consumed);
   exhaustion survives a worker restart.
6. Operator-active parked tab is never renavigated or closed; engagement
   and cession survive a worker restart; *papio*'s own focus (action token)
   does not count as engagement.
7. Firefox <139: group identity degrades to work-window without silently
   becoming work-window *ownership*; full event-page teardown between every
   protocol step.

## Abort criteria

Attempt five's carry over verbatim. Additionally: any slice that requires
widening `Response`/IPC shapes or the timing-only auth frames (fail-closed
skew, see AGENTS.md) stops and redesigns behind a new method/feature.

## Live smoke run defect (2026-08-19) — FIXED, with one open question

**An internal scaffold removal is reported to the daemon as operator
cancellation, and the daemon cancels the paper.** Reproduced twice on the
operator's own browser, on both sides of the page-path fix, so it is neither
caused by nor fixed by today's commits:

| job | build | trace |
| --- | --- | --- |
| `job_7246c11621dacc4a01e6929aea` (`doi:10.1207/s15327043hup1101_3`) | pre-`d2f4474` | `browser.auth_pending` 02:37:09Z → `browser.provider_outcome` 02:39:17.822Z → `job.transition` 02:39:17.823Z → **cancelled** |
| `job_39fd95207c8880a4b079cf8297` (`doi:10.1371/journal.pone.0173664`) | HEAD (`0ef7b4b`) | `handoff.opened` 03:09:33Z → `provider_outcome` + `job.latch` + `provider_outcome` → `job.transition` 03:09:41.799Z → **cancelled**, then `browser.auth_pending` 03:09:41.921Z *after* the cancellation |

Neither had any operator interaction: the first happened while the machine was
asleep and locked, the second 8s after a `papio actions open` with the tab
never touched. `close_authorizations` is empty for both, so no authorized close
ran — the removal came from a browser-local path that leaves
`onTabRemoved`'s `authorizedClose` false, which falls through to the ordinary
cancellation branch (`provider_outcome` → daemon cancel). The late
`auth_pending` shows the surface was still mid-authentication when it went.

This is the hazard already named in the tab-lifecycle notes: a deliberate
internal close must first detach or settle its job and carry an intent marker,
or papio reads its own housekeeping as the operator giving up. Slice 2b built
exactly that transaction for authorized closes; the defect is the path that
removes a surface *without* using it.

**Fixed** by an explicit intent marker (`deliberateRemovals`), consumed once in
`onTabRemoved`: a removal this worker initiated retires the surface, detaches
the job, and reports nothing — no `provider_outcome`, no cancellation, no
`owner_closed`. The marker is set in `removeMaterializationTab`, the single
chokepoint all fourteen internal removal sites share, and dropped again when
`closeOwnedTab` refuses (the tab survives, so a genuine operator close later is
still a real cancellation). Worker-memory is the correct tier here, unlike the
durable claim identity beside it: the `onRemoved` event for a removal we
initiate always arrives in the same worker lifetime.

Two tests pin the distinction, both verified against the pre-fix source: an
internal removal of a job-owned surface reports nothing, and an operator close
of the same surface still cancels. They drive the chokepoint directly because
every existing materialization test used `materializationActiveJob`, whose
`tab_id: -1` made `findByTab` miss and left the whole cancellation branch
unreachable — which is why 1,212 green tests coexisted with cancelled papers.

**Verified live after the fix** (2026-08-19, `b626966` loaded, session
`d4c171523c0e`): one institutional open on the recovered T&F paper
(`job_abecef3df2b89b92dcbcc473c5`) stayed `awaiting_human` for 3.5 minutes —
past both windows that killed papers before (+8s and +2m08s) — with
`auth_pending` → `institutional_effect_authorized` →
`institutional_effect_result` and no `provider_outcome` at all. Its claim
reached `navigated` with a real `tab_id`, the full claim→bind→route→navigate
sequence that never completed on the broken build (it stalled at
`claimed`/`tab_id 0`), and the previously stuck claim was retired to
`abandoned` by reconciliation. `dist/materialize.html` rendered
"Materialization binding ready" in the extension's own code path, not just a
hand-typed probe.

**Residual, now with better evidence than the earlier guess:** that open left
TWO papio surfaces for one job — the explicit path's own minted
`session-signin` tab (which is where the UNE login actually landed) beside the
materialization binding's scaffold. Claim observations are attributed to the
binding's tab, so `wall_observed`/`login_started`/`auth_returned` never fire on
the explicit path: `claim_observation_journal` stayed empty through a live login
wall. Two consequences worth separating:

1. *Not destructive now* — the job is healthy and the operator can sign in; the
   daemon still learns the state through `auth_pending`/effect results.
2. *But it is the likely trigger of the cancellation above*: two surfaces for
   one job means one of them is surplus, and whichever removal tidies the
   surplus one was hitting `onTabRemoved` while the job still pointed at it.
   That also explains the two `provider_outcome` frames. The marker makes every
   ordering safe, which is why the run above is clean, but the duplicate
   surface itself is unfinished business: the explicit path should either bind
   its minted tab as the materialization surface or not mint one.

Both papers were recovered with `papio acquire`: the PLOS ONE one went straight
to `imported` (open access, no browser), the T&F one is the live subject above.

## The duplicate surface, chased (2026-08-19) — three defects fixed

The "unfinished business" above turned out to be three separate defects. Each
was reproduced in `extension/test/background.test.ts` before being fixed, and
each test was verified to fail on pre-fix source.

1. **A candidate re-offer disowned a live surface.**
   `onInstitutionalCandidateOffer` reset the job record's `tab_id` to `-1` on
   every offer, including a re-offer of the same candidate whose correlation
   already held a bound tab. `reduceMaterialization` (`state.ts`) already
   applied the right rule to the correlation — a same-candidate re-offer
   refreshes the lease and only restarts materialization when no binding was
   ever established — and the job record contradicted it. The daemon pins a
   re-offered candidate's expiry in memory (`bridge.go:10181-10183`), so a
   daemon restart or its own lapse check (`10174`) re-offers with a fresh
   expiry: the live trigger. Consequences were exactly the reported symptoms —
   every observation keyed on `job.tab_id` (challenge, mfa, auth_returned,
   entitled_landing) went silent, and the next open could not find the tab to
   reuse.
2. **Reconcile adopted a leftover placeholder as the paper's surface.**
   `reconcileMaterializationTabs` matched surfaces by scaffold URL, so a
   *navigated* surface could never be among its candidates — it has left that
   URL by design — while a leftover scaffold for the same binding could.
   `candidates.find(...) ?? candidates[0]` therefore chose the placeholder,
   repointed the correlation at it, and submitted **it** to the daemon as the
   job's surface. A navigated correlation now keeps its own tab and every
   scaffold-URL tab for that binding is retired.
3. **An explicit open raced the pipeline building its surface.** `FocusHandoffs`
   queues both a candidate offer (`serviceMaterializationCandidate`) and a
   `handoff_focus`; the extension routes the focus to `focusDaemonHandoff`,
   which for a URL-free candidate falls through to `openHandoff`, whose
   `engagement_required` branch called `openFreshHandoff("explicit")`. That
   function applied the architecture ruling "the extension never self-mints a
   materialization binding" to the *automatic* trigger only. The focus arrives
   while the correlation is pre-bind — exactly when `tab_id` is legitimately
   `-1` and the daemon's consult therefore answers `open_new` (§2.1.1 case 3) —
   so neither side could see the collision. The ruling now covers both triggers
   whenever the pipeline is actually driving the job. It deliberately does not
   cover a correlation nothing is driving: fourteen Slice 3 tests pin that
   state, and `seedClaimCandidate`'s own comment says it seeds a candidate
   *without* negotiating the feature "which would drive the unrelated
   daemon-orchestrated materialization workflow".

**Live verification** (operator's own Chrome, `7068539` then `1d9581d`): the
stale placeholder was retired on the post-update pass while the operator's
sign-in tab was kept, twice, for two different bindings — with
`close_authorizations` empty and no `provider_outcome`, so this morning's
deliberate-removal marker kept papio's own housekeeping from reading as
cancellation. Defect 3's fix is verified in the harness only; see the ordering
race below for why a fresh live open could not be driven afterwards.

## Still open, with evidence (2026-08-19)

1. **A focus frame that wins the race against its own candidate offer burns the
   paper's one-use institutional authorization and produces nothing.**
   `job_673c22adda606ce0959b4034df`, opened 04:30:11Z: candidate
   `38b00e5e6a1013` created `eligible`, zero claims, zero tabs, no surface. The
   second attempt at 04:34:01Z was refused outright — "the job's access mode
   does not permit an institutional handoff" — so the operator cannot retry. The
   extension is right to refuse minting with no candidate correlation (Slice 0
   containment, `background.ts` "No live institutional candidate ever ran for
   this job"), and the daemon queues focus and candidate offer with no ordering
   guarantee; nothing re-drives the request when focus loses. The 04:07 run on
   the same build family won the race and completed, which is what makes this an
   ordering defect rather than a capability one.
2. **Claim observations never reach the journal.**
   `claim_observation_journal` is empty for every run today, including one whose
   job events show `browser.auth_returned` — so the timing-only frame landed
   while the observation did not. Defect 1 above explains the earlier runs
   (job-keyed emitters had no tab), but not this one. Ranked candidates from the
   read: emission requires both a live `claimGrants` entry (worker memory, gone
   after a ~30s MV3 sleep) and `tabLedgerCache[tabID].binding_id`; a daemon-side
   rejection would also leave the journal empty and is not recorded anywhere.
   Distinguishing them needs the ack outcome, which nothing persists.
3. **Dead candidates and expired claims are never reaped, and they hold
   institutional state.** Live: candidates `claimed` for jobs whose claims'
   leases expired hours earlier (`99b301abba` lease 02:37Z, `19e6b3aa4c` lease
   03:07Z, both still phase `navigated` pointing at tab ids deleted by hand),
   plus two `eligible` candidates belonging to **cancelled** jobs
   (`7246c11621`, `39fd95207c`). `maxOutstandingOffers` is 4
   (`bridge.go:152`), so enough corpses can starve every new institutional
   offer; the daemon's in-memory offer map hides this until a restart clears it.
