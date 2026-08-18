# Attempt six (rev 2): papio owns every surface it creates

Successor to `dev/active/login-handoff-plan.md` (attempt five). Revised
2026-08-18 after a four-reviewer round on rev 1 (verdicts: all four
"incorrect"/"sound-with-changes"; ~30 anchored findings). Rev 1's central error:
it proposed extension-local durable claims and gates — re-implementing, on the
wrong side of the authority boundary, machinery ADR-0022 already designed and
partially shipped. This revision builds on that machinery instead.

## The field failure (2026-08-18, operator's browser)

One papio tab group with ~17 tabs — six-plus IdP sign-in tabs, several
unrendered Primo tabs, a ScienceDirect ATN-12 error page — plus duplicate papio
groups/windows across extension reloads, drives fired into a dead network after
wake, and siblings stranded at sign-in walls after the operator authenticated
in one tab. The operator's directive: papio owns the full lifecycle of every
surface it creates; a tab is ceded to the user only when papio asks for user
action (rare by design) or the user takes the tab over.

## Corrected audit (rev 1 claims re-verified by reviewers; lines at `cfbfec6`)

What is true:

1. **Automated drives leave one tab per auth wall.** The drive timeout
   (3 min) parks a fresh handoff with its live tab preserved
   (`background.ts:4196-4221`); a sibling that classifies as "login" while
   another job owns the institution claim is deliberately left at the provider
   wall (`parkHandoffWaitingForSession:4376-4413`); the claim gate runs only
   inside `maybeRouteFederatedLogin` (15002-15067), **after** navigation, and
   only for adapters with a `federatedLogin` template — the generic auth path
   (13771-13788) takes no claim. Auth-stall (10401-10409) and challenge
   (10626-10629) parks also preserve tabs. N cold institutional jobs ⇒ up to N
   wall tabs, one live owner.
2. **Fresh-link parks are unreachable by every resume path.** `openFreshHandoff`
   deletes `offerURLs` at materialization (privacy cutover), so
   `reloadAuthenticationHandoffs` (11599-11625) cannot renavigate them,
   `isInstitutionalSessionLanding` cannot record their landings, and
   `resumeWaitingForSessionJobs` (14884-14933) matches only
   `waiting_for_session` — timeout parks lack the marker. Legacy URL-bearing
   parks do resume; the shipped fresh-link path strands. ("Everything waits
   forever" in rev 1 was overstated; this is the precise version.)
3. **Coordination state is session-scoped; surfaces are durable.** The whole
   managed store lives in `chrome.storage.session` (`state.ts:1645-1666`,
   wiped on update/reload/browser restart) — and `federatedLoginOwners` is not
   persisted at all: the serializer deletes it (`state.ts:1617-1618`). Claims
   are worker-memory. Startup does adopt: groups are re-found by title and
   folded, `reconcileTabs` revalidates tracked tabs, and both are awaited
   before the governor drain (4756-4834) — rev 1's "drives before reconcile"
   was wrong. What has no adoption: the work window (no rediscovery; stale ID
   ⇒ new window, 3959-3983), prior-generation tabs whose jobs the wiped store
   forgot (ledger classify closes 0 by documented design, 3587-3607), and any
   claim.
4. **No connectivity gate.** No online/probe signal exists anywhere in the
   background; `connectionStatus` reflects the native port only. Cold fresh
   `requires_auth` offers ARE already gated tabless (`hasHandoffReleaseEvidence`
   10462-10476, `onJobOffer` 12910-12990) — the ungated paths are legacy
   URL-bearing offers, re-offers of live jobs (13210-13214, 13353-13357), and
   the 45s queued-handoff release. The daemon emits up to 4 offers per 2s poll
   after a reconnect hello resets its offered map (`bridge.go:124-131`, 8405+).
5. **Silent dead ends.** `MAX_CLASSIFY_RETRIES` exhaustion deletes worker-local
   retry state and returns (14724-14728) — and because `classifyRetries` is a
   worker-local Map, an MV3 restart resets the count, so a dead page can retry
   indefinitely across worker lifetimes. Navigation errors are unobservable
   (no error event seam in `BridgeDeps`; an error document flows into generic
   auth detection and can charge an auth attempt). Daemon `goodbye` releases
   the session only (`bridge.go:914-916`) — it is transport loss, never
   terminal job evidence.

## Why five attempts did not land this

- **The replacement architecture already exists, disabled.** ADR-0022 shipped
  Phases 1-3: daemon-owned `materialization_claims`, two-party claim→bind→
  route→navigated→reconcile (`institutional_materialization_v1`, strict and
  feature-negotiated), self-identifying scaffolds, paginated reconciliation,
  effect permits, and scheduler grouping whose anti-join yields **one bound
  scaffold per institution by design**. Automatic first-route behavior is
  deliberately off until Phase 4/5 readiness. Every attempt so far patched the
  legacy offer/drive path that this machinery is meant to replace — the
  "resistance" was real: the legacy path cannot express ownership, and the
  ADRs forbid building a second authority beside the daemon's.
- **Scope boundary drawn at the wrong path.** Attempt five fixed cold
  human-action offers (tabless until engagement, fresh URL mint — shipped and
  effective). The pileup lives in the automated warm-session drive path.
- **Risk asymmetry ratcheted into taboo.** Reviews punished closing (real
  TOCTOU P0s) and never punished leaving open; the "do not re-attempt" list
  plus the AST close-allowlist were then read as "lifecycle ownership is
  forbidden," freezing policy along with mechanism.

## Architecture decision: finish ADR-0022, do not build beside it

Rev 1's extension-local v2-hash claims, durable browser-side claim owners, and
evidence-triggered auto-drives violate ADR-0022 Decisions 1/2/5/6 (daemon-issued
opaque authentication claims, two-party binding, transient routes, typed human
gates as the only attention authority) — confirmed by review. The corrected
shape, which also matches the daemon-side claim-arbitration design reviewed in
August:

- The **daemon** owns institution claims (authentication-entry leases, Phase
  4), durable job/park state, and resume scheduling. Claims survive extension
  updates and browser restarts for free.
- The **extension** owns browser-local facts only: physical tabs/groups/
  windows, binding acknowledgements, wall/landing observations, operator
  engagement, and the guarded close primitive.
- Sibling resume is Decision 6 verbatim: one authentication claim ⇒ one human
  surface with a dependent count; successful resolution resumes eligible
  siblings through normal daemon scheduling — never through keepalive-probe
  evidence callbacks.

## Amendments to attempt five's "do not re-attempt" list (operator decision)

- **"Automatic waiter-tab closure" is re-permitted, narrowly**: scaffold-only,
  through `closeOwnedTab`, after an explicit detach/terminal transition, with
  an intentional-close marker consumed by `onTabRemoved` (a narrow, tested
  addition — not the banned removal-state-machine rewrite). Since `e6ff3e4`
  every engagement mints a fresh URL, so a stale wall page has zero residual
  value. Unknown engagement ⇒ retain, never close.
- **Owner-age/URL-shape liveness stays banned.** Claim retirement and resume
  ride daemon claim state and explicit auth-return/landing transitions only.

Everything else on the list remains banned. Attempt five's still-normative rule
is also reaffirmed against rev 1: **missing federation metadata is a structured
engagement failure, never permission to pre-open** — rev 1's "no-metadata jobs
keep today's behavior" is retracted; all `requires_auth` work goes tabless
without a granted claim.

## Design invariants

- **Scaffold vs content, durably.** A scaffold is a papio-created tab with a
  durable URL-free ownership record (pre-create intent + post-create binding)
  and no recorded operator engagement. Engagement (activation or user
  navigation, excluding papio's own focus/navigation) is recorded durably at
  observation time; today's signals are worker-local and die with the worker.
  Content — engaged, active, PDF, adopted viewer — is never auto-closed.
- **A claim precedes a surface.** No tab for `requires_auth` work without a
  daemon-granted claim; reserve/claim before `tabs.create`, roll back on every
  create/bind failure path (the reverted attempt's dead-owner orphan class).
- **One institution, one sign-in surface** — enforced by daemon claim
  arbitration and scheduler grouping, not by racing extension maps.
- **Resume is a one-shot transition.** Successful auth-return/claim resolution
  resumes siblings once, epoch-fenced; repeated warm probe evidence never
  mints surfaces; failed retirement (owner closed without success) leaves
  waiters tabless awaiting the typed gate.
- **In-place renavigation is fenced.** Immediately before any automatic
  `tabs.update` of a parked tab: fresh `tabs.get`, active check, engagement
  check, no unrelated awaits between check and act. An operator-active tab is
  never renavigated. After the update, the continuation re-reads job and tab
  state before registering a drive — a removal during the await must not
  resurrect a dead job (Chrome events are not serialized with the drive
  chain). Automatic resumes never focus or foreground a tab
  (`openFreshHandoff`'s unconditional focus needs a non-focusing variant).
- **Positive evidence closes; absence retains.** A prior-generation scaffold
  closes only on daemon cancel/terminal acknowledgement or complete reconcile
  response — never on absence from a bounded 4-offer batch, never on `goodbye`.
- **Every dead end has a durable disposition.** Classify exhaustion (persisted
  retry epoch), navigation error (observed before auth detection, no auth
  charge, no cooldown), daemon cancel — each parks with an inbox action and
  retires its scaffold.
- **Startup: adopt before drive, bounded.** Every drive-producing entrypoint
  (queue drain, `onJobOffer` direct paths, runtime opens) waits on an adoption
  gate; hello/poll stay responsive; scans have deadlines that fail closed to
  no-adoption/no-close; browser-session-restore gets a grace pass.

## Slices

Discipline carried over from attempt five verbatim: one `background.ts` owner
per slice, exact deletion manifests, full-suite runs only, ≤400 changed lines
outside tests per slice, no assertion weakening. Protocol work lands at
four-site parity (Go validator, TS parser, JSON schema, corpus) behind
negotiated features.

### Slice 0 — harness seams (prerequisite, mostly test code)

Network/online/offline seam and navigation-error events in `BridgeDeps` +
`fake-tabs.ts` (genuinely absent today); a lifecycle helper formalizing the
existing update-simulation pattern (`background.test.ts:6035-6061`: wiped
session store, surviving fakes, new `Bridge`); durable-ledger assertions.

### Slice 1 — dispositions and the safe close fence (extension-only)

- Detach-then-close: a scaffold close first records an intentional-close
  marker and detaches/settles the job, then calls `closeOwnedTab`;
  `onTabRemoved` consumes the marker and skips the cancellation path (the
  reverted attempt's defect was dropping this consumption — restore it as a
  narrow guard, not a rewrite). Today `closeOwnedTab`'s `findByTab` guard
  makes every live-job close a silent no-op (3357-3359); the detach transition
  is what makes closes real without relaxing content/active/PDF guards.
- Navigation-error handling ordered **before** generic auth detection: park
  for retry, no auth-attempt charge, no provider cooldown, scaffold retired.
- Persisted classify-retry epoch; exhaustion parks with an inbox action and
  retires the scaffold.
- Timeout parks: retain the tab only while operator engagement is recorded;
  otherwise close the scaffold — the inbox action re-mints fresh on click.
- `goodbye`/disconnect is explicitly non-terminal: no closes.

### Slice 2 — durable ownership and startup adoption (extension-only)

- URL-free durable ownership skeleton beside the ledger: job/tab/group/window
  IDs, purpose, generation, engagement bit, bounded timestamps. No titles,
  DOIs, hosts, URLs, or entity material (the session store remains the only
  home for those; serializer stays fail-closed). Pre-create intent record so
  a worker death between `tabs.create` and ledger write cannot orphan;
  legacy ledger entries without `jobID` are retained for manual review, never
  auto-closed; legacy raw-URL ledger entries are redacted on migration.
- Restart-class validation before trusting any stored ID: SW restart (IDs
  valid, session intact), update (IDs valid, session wiped), browser restart
  (all IDs invalid) — every adopted ID is re-proven via `tabs.get`/query plus
  scaffold identity; claims/owners are revalidated only **after** physical
  remap; no proof ⇒ retire the record, retain the tab.
- Group folding requires a ledger-owned member proof, never title alone
  (ADR-0013: membership is not ownership); the work window gets a
  self-identifying rediscovery marker independent of keepalive.
- Adoption gate on every drive entrypoint with bounded scans and a
  session-restore grace pass; orphan closure only on positive daemon evidence
  (cancel/terminal ack or complete paginated reconcile).

### Slice 3 — authentication-entry lease and one attention surface (Phase 4 core)

Daemon-side, on the shipped Phase 1-3 projections and
`institutional_materialization_v1`:

- Daemon-issued opaque authentication claim (Decision 2) with an
  authentication-entry lease; one transaction arbitrates claim requests and
  answers `navigate_existing` / `open_new` / `focus_owner` / `park` (the
  August-reviewed design).
- Extension consults the claim **before** any `requires_auth` tab exists; all
  ungranted institutional work is tabless-parked daemon-side. Wall/landing/
  auth-return observations flow up as claim events; the login surface is one
  tab with a dependent-sibling count (Decision 6).
- Successful resolution resumes eligible siblings through daemon scheduling:
  fresh route mint per sibling (routes stay transient, Decision 5), in-place
  renavigation only through the Slice 1 fence, one-shot epoch semantics, a
  per-job mint latch, and slot reservation before mint (today
  `openFreshHandoff` materializes before the drive-limit check).
- Binding acknowledgement covers **both** materialization branches: today
  `openManagedTab`'s reusable-tab branch returns after `recordManagedTab`
  without the materialization callback (3300-3311), so a reserve-before-create
  claim would stay unbound forever on tab reuse. Claim create/bind rollback
  runs on every failure path, including a tab closed or job cancelled mid
  materialization (the reverted attempt's dual orphan race).
- Warm evidence is **not** a lease: it is exact-profile-scoped (Decision 2)
  and admits a drive without holding the claim. A stale warm session that
  bounces to a wall converges on the same single authentication-entry lease
  at wall observation; a fixed 30-minute origin timestamp never substitutes
  for claim ownership.
- Legacy extension claim code (`federatedLoginOwners`, v2 hash keys,
  `parkHandoffWaitingForSession` collision path) is retired in the same
  cutover — no second authority.

### Slice 4 — automatic drives ride materialization claims (Phase 5 cutover)

Move the automatic offer/drive path onto claim→bind→route→navigated→reconcile,
retiring legacy pre-open: persisted offer URLs disappear (routes are minted at
drive time), re-offers revalidate the claim instead of re-opening, and the
4-per-poll offer batch becomes claim-paced. This is the clean cutover that
makes "at most one bound scaffold per institution" structural rather than
best-effort. Gated by ADR-0022's staged-enablement readiness criteria; the
extension keeps a degraded legacy path only until the feature negotiates.

### Slice 5 — connectivity gating (extension-only, small)

A defined connectivity authority (online events + one probe lease), consulted
by **every** materialization entrypoint — queue drain, both `onJobOffer`
direct paths, queued-handoff release — failing closed to tabless parks.
Offers queue browser-side; the daemon is untouched.

Sequencing: 0 → 1 → 2 → 3 → 4 → 5. Slices 1-2 are extension-local hygiene that
shrink the litter immediately and are prerequisites for safe closure; Slice 3
kills the pileup at its source; Slice 4 is the structural cutover; Slice 5
closes the wake-flood residual. Rev 1's A-before-C ordering was reviewed as
unsound (claims were worker-memory; the gate had nothing durable to stand on).

## Test scenarios

1. Update simulation: session wiped, fakes survive, new Bridge — zero new
   groups/windows, prior scaffolds adopted or retained, none closed without
   positive evidence, no duplicate claim surfaces after re-offers.
2. N institutional jobs, cold session: exactly one sign-in tab, N−1 daemon
   parks; login succeeds once ⇒ all N resume (fresh routes), scaffolds retire;
   owner closed without success ⇒ zero new surfaces.
3. Wake flood: 4 offers, network down ⇒ zero tabs; online + probe ⇒ paced
   drives.
4. Nav-error and classify exhaustion: park + scaffold close, no auth charge,
   no cancellation emitted (marker consumed).
5. Operator-active parked tab is never renavigated or closed; engagement
   survives a worker restart.

## Abort criteria

Attempt five's carry over verbatim. Additionally: any slice that requires
widening `Response`/IPC shapes (fail-closed skew, see AGENTS.md) stops and
redesigns behind a new method/feature.
