# Attempt five: institutional sign-in handoffs without tab litter

Supersedes the reverted restructure (`dev/scratch/oracle/login-restructure-reverted.patch`).
Written after a four-angle plan review and one iteration round with the
reviewers; findings in `dev/scratch/oracle/login-review-findings.md`. This
document is the plan of record.

## The failure being fixed

papio parks a job needing an institution sign-in behind a per-institution claim,
tabless. A ten-minute governor cleared each waiter's marker; a job with no marker
is an ordinary parked job, so it drove again and opened its own login tab.
Fifteen handoffs over twenty-one hours produced fourteen SSO tabs, every one
stale — a Shibboleth flow token expires in minutes, so they were dead forms
inviting credentials into flows that could not succeed.

The governor is already deleted (shipped). This plan removes the remaining
sources: papio creating institutional login surfaces nobody asked for, and papio
being able to close tabs it should not.

## Resolved: where claim state lives

Two reviewers reached opposite conclusions; after one iteration both converged on
the same answer, which the ADRs support.

- Claim/tab state stays **browser-side**, with `storage.session` as the
  authority. ADR-0013: "login visibility is a browser-local overlay", "the
  extension remains the reporter of browser-local facts". The daemon cannot
  observe tab removal or active-tab state — the very facts that retire a claim —
  so a daemon-side owner would arbitrate on delayed observations.
- The cross-process hazard is closed by **holder-exclusivity, not migration**.
  ADR-0003: "only the offer/handoff flow is holder-exclusive". The reverted patch
  admitted `handoff_link_request` from non-holders, letting a former holder with
  stale state open a handoff in the wrong browser. Holder-only dispatch closes
  that without moving state.
- The daemon's only new responsibility is **durable action validation and fresh
  URL resolution**, through one resolver shared with `papio actions open`.

**Accepted boundary (decide, do not discover):** holder-only dispatch guarantees
one owner under a stable holder. It does not fence an explicit mid-engagement
takeover — an old holder can hold an authorized URL and execute `tabs.create`
after demotion, and an old holder with an already-driving tab can process a
login-wall event after takeover without making any request. We accept that
boundary for now and state it; fencing it needs only daemon session-epoch /
pending-engagement state, not daemon ownership of the tab lifecycle. Revisit if
takeover-during-sign-in is ever observed in the field.

## Sequence

Each slice ships independently and is verified before the next starts.
Ordering note: closure-ban precedes harness work deliberately. Its safety is
structural (capabilities deleted + AST test) and does not depend on event
fidelity, whereas building a faithful fake first would model `closingTabs` and
cancellation semantics that Slice 1 immediately deletes.

### Slice 1 — one guarded close primitive (LANDED)

The total closure ban originally specified here was built, and the operator
reversed it: trading a rare race for a guaranteed daily tab-litter chore was
the wrong trade. What shipped instead, after six review rounds and a final
simplification:

- **One primitive, `closeOwnedTab(tabID, reason)`.** Every lifecycle close
  routes through it. An AST completeness test pins the call sites to exactly
  {the primitive, the Chrome adapter that wires it, keepalive's three
  pinned-tab teardowns} with count-based bidirectional comparison — a new
  `.remove` anywhere fails CI.
- **Chrome is asked at the moment of decision.** The gate is: ledger says
  papio created it → job lifecycle settled → not a content tab → one
  `tabs.get` → refuse if active, refuse if its CURRENT window/group is
  outside papio's surface (dragged-out and recycled-id protection in one) →
  remove in the same turn. The lesson from the failed rounds is written on
  the primitive: the forbidden pattern is UNRELATED awaits between check and
  act, not the one authoritative freshness read. No shadow-state
  reconstruction — the six rounds of seeding races, tombstones and
  completeness markers all existed to avoid one await, and are deleted.
- **papio never auto-closes content.** A PDF viewer — whoever opened it, and
  including a scaffold tab that NAVIGATED to a PDF (checked against the
  fresh `tabs.get` url, not just the creation record) — always stays. Only
  papio's own scaffolding (job/handoff/capture tabs) closes. This deletes
  the durable activation-history machinery entirely; the litter the operator
  actually reported was login tabs, which Slice 3 removes at the source.
- Timeout keeps detach-then-park semantics (no close); cancel, settle,
  replacement and page capture close through the primitive; the work window
  is never closed directly — Chrome discards it when its last tab closes.
- The inertness net: every closing route has a positive test asserting the
  tab is genuinely gone, and per-predicate negatives proven non-vacuous
  (each fails if its predicate is deleted).

### Slice 2 — make the harness tell the truth (LANDED)

Three times this session, work was green in tests and wrong in a real browser.

- Replace the three independent tab fakes (`adapters`, `background`, `keepalive`)
  with one stateful fake that behaves like Chrome: clone returned snapshots,
  reject absent ids, maintain one active tab per window, and emit `onUpdated`,
  `onActivated` and `onRemoved` for programmatic operations. Do **not**
  auto-complete navigation — Chrome completes later and tests must control it.
- Expose explicit `completeNavigation`, `userNavigate`, `userActivate`,
  `userClose` helpers and migrate the hand-rolled `live.set/delete` + manual
  emits.
- Add a sanitized cross-origin journey fixture: distinct local origins for SP,
  discovery, IdP and callback; the IdP form carrying only a freshly generated
  `execution` token; a callback; an expired-token response. Every engagement gets
  a different token; replaying an old one must fail as stale.
- Breakage in governor draining, self-navigation gates or cancellation is
  **signal**; mechanical helper migration is churn.

The faithful event model surfaced seven production defects that the old silent
fakes could not express: login redrives during an unfinished sign-in, outcomes
from closed tabs, duplicate stale-page surfacing, duplicate waiter redrives,
and three keepalive supersession races. Each is now fixed against the shared
fake; the SAML journey fixture also pins fresh execution tokens and stale-token
replay rejection.

Correction to an earlier assumption: the manifest requests broad `tabs`
permission, so undefined `tab.url` is **not** a current production cause. Keep
the case covered because the API type permits it, but the live risk is
scripting/host permission across the IdP origin.

### Slice 3 — cold offers are tabless until engagement

Behind a negotiated `handoff_link_v1` feature flag:

- Every `requires_auth: true` cold `job_offer` is stored as engagement-required
  with **no tab and no persisted URL**. `requires_auth` selects this branch — not
  "first adapter with federated login plus entity id". Missing federation
  metadata is a structured engagement failure, never permission to pre-open.
- `openHandoff` requests the URL by job id and creates exactly one tab. A later
  click, or a click after restart, requests again rather than reading browser
  storage.
- The request is **holder-only** (ADR-0003). An old daemon keeps today's
  behaviour rather than receiving an unknown request.
- Daemon side: extract the CLI `actionURL` resolver into one shared helper used
  by the CLI, offers and this handler — the reverted patch grew a second resolver
  that had already diverged. Add the pair through Go, TypeScript, schema and
  corpus, with fixtures both parsers decode.
- The already-in-flight path is unchanged: a drive that hits a login wall still
  navigates **its own** tab (pinned by `adapters.test.ts:1904-1962`).

**The choke point (this is the part that failed before).** ONE claim-level choke
point must cover both cold `openHandoff` engagements across different jobs AND
in-flight login-wall routing. Today `openHandoffRequests` dedupes per job only,
so "one click produces one tab" does not stop two sibling jobs concurrently
passing an owner check. The transition must be:

1. check the claim, and reserve durable ownership **before** any navigation —
   the owner write lands before `tabs.update`/`tabs.create`, never after;
2. re-check after the reservation;
3. create or navigate;
4. bind the returned tab id **synchronously**, before any later await;
5. roll back the reservation on failure.

A mutex wrapped around the reverted order (navigate, then write the owner) is
**not** sufficient — that was the original defect. Worker death may then leave a
stale reservation, which restart reconciliation repairs; it cannot produce two
navigations. This lives in a small pure reducer, not a global lifecycle rewrite.

Ship gates: offer produces zero tabs; one click produces one correlated request
and one tab; a later click or a restart re-requests rather than reading browser
storage; missing entity metadata stays tabless; direct-action, institutional and
retrieval URLs match `actions open`; both skew directions stay connected;
owner-write-before-navigate is pinned by a test; two sibling jobs racing the same
institution produce exactly one navigation.

### Slice 4 — only if field evidence still warrants it

Explicit operator refresh of an already-owned stale in-flight tab, minting a new
URL into that same tab. **If Slice 3 removes the accumulation, stop.**

## Explicitly dropped

Do not re-attempt: `forceNew`, cross-job engagement locks, `federatedLoginVisited`,
sign-in-count rewrites, owner-age or URL-shape liveness, unknown-state re-probing,
universal backstops, automatic waiter-tab closure, ledger-based close
authorization, the rewritten `onTabRemoved` state machine, or changes to the
central removal/governor/work-window/page-capture paths beyond the explicit
per-caller transitions in Slice 1. These delivered none of the operator value and
caused the final round's TOCTOU, persisted-URL, intentional-close and
drive-release regressions.

## Working discipline

- **One owner for `background.ts` per slice.** No two agents edit it
  concurrently. Parallel work only across frozen contracts and disjoint surfaces:
  daemon resolver/handler, protocol+schema+corpus parity, harness.
- **Do not split the 9,000-line file first.** Only two of the final eight
  findings were collateral deletions; the rest were design/integration defects,
  so a whole-file split is another large zero-value rewrite. New transition logic
  goes in a small pure `handoff-lifecycle` reducer with explicit inputs and
  commands, leaving `background.ts` as the Chrome/port dispatcher.
- **Exact deletion manifest.** Six times this session an edit silently dropped an
  adjacent line — twice changing unrelated semantics. Every edit round states
  what it intends to delete, and the diff's removed lines are checked against
  that manifest before review.
- **Full-suite runs only.** Per-file runs hid breakage repeatedly; a hang counts
  as a failure.
- **No assertion may be weakened to make a test pass.** Softening was explicitly
  forbidden and still happened twice. A changed expectation needs a stated
  justification naming the mechanism that no longer exists.

## Abort criteria

Stop and reassess rather than starting another fix round when any holds:

1. A review round introduces a new defect of a class already fixed in an earlier
   round — the signature of accretion rather than convergence.
2. Two consecutive rounds fail to reduce open findings.
3. Any slice exceeds roughly 400 changed lines outside tests; the reverted
   attempt was 2,300.
4. A fix requires weakening an invariant listed above.

## Status of the shipped stopgap

Deleting the governor is stable enough to sit on indefinitely. Waiters park until
owner-claim retirement, owner job removal, or fresh session evidence. Worst case
is one stale login tab per institution instead of fifteen; the operator resolves
it by signing in once, or by closing the tab, which retires the claim.
