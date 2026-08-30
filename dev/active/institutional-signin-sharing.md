# One sign-in, many papers: why sharing never fires

Operator report, 2026-08-18: *"if i login on one tab, the others remain stranded
at the sign in page - dumb."*

That is not a missing feature. The sharing mechanism exists, is test-pinned three
ways, and effectively never fires in the field. This plan is about closing the
gap between the two, and its first slice is measurement because the cause is not
yet known.

Successor to nothing. `dev/active/surface-lifecycle-plan.md` covers the *surface*
lifecycle (churn, adoption, closure) and its work has shipped; it is due for ADR
extraction. This file must not become that plan's ninth round by accretion.

## Measured, 2026-08-21, on the operator's own store

| fact | value |
| --- | --- |
| papers that ever reached an institutional wall | 254 |
| of those, papers that also produced `browser.auth_returned` | 209 (82%) |
| `browser.auth_returned` events, all time | 673 |
| **`entitled_landing` records, all time** | **2** (both today) |
| papers awaiting a human at ONE institution | 108 |
| waiting papers older than 7 days | 126 of 129 |
| papers reaching `ready`/`imported` per day | 1–2 |

Sign-ins **work**: 82% of walled papers come back from the IdP. What almost never
happens is the record that lets the *next* paper reuse that sign-in.

## Mechanism, verified in the tree

- Sibling resumption is gated on `entitled_landing`, and that gate is
  deliberate: `auth_returned` alone proves only an IdP round trip, not that
  entitled content was reached. Pinned by
  `TestAutomaticCandidateOfferParksDependentUntilEntitledLanding`,
  `TestClaimObservationEntitledLandingReoffersParkedSibling`, and
  `TestAutomaticCandidateOfferGatesOnEntitledLandingAndOwnerCloseRetiresClaim`
  (all `internal/browser/authentication_claim_test.go`).
- `entitled_landing` is emitted from exactly ONE place: the `case "article"`
  branch of `applyVerdict` in `extension/src/background.ts`. An adapter must
  classify the landed page as an article.
- Therefore: no `article` verdict ⇒ no entitlement ⇒ every paper pays for its own
  human sign-in, and the one-sign-in-per-institution rule means the queue
  advances one sign-in at a time. 108 papers, 1–2 papers a day.

## Falsified this session — do not re-litigate

- **Not rejection.** Zero `entitled_landing` rejections in `daemon.log`; four
  observation rejections total, none of this kind. The observation is not being
  refused, it is not being sent.
- **Not general adapter absence.** Twenty-seven hosts ship adapters, including
  `proquest.com`, `primo.exlibrisgroup.com`, `jstor.org`, `link.springer.com`,
  `onlinelibrary.wiley.com`, and `journals.sagepub.com` — the operator's main
  routes.
- **Not the entry-lease bind deadline.** During an active attempt, wall reports
  arrive every 2–5 seconds (n=573 intervals), far inside
  `AuthenticationEntryBindDeadline` (2 minutes,
  `internal/job/institutional_evidence.go`), so the lease is renewed
  continuously while a sign-in is live. An earlier draft of this diagnosis
  blamed a "3-minute report cadence against a 2-minute deadline"; that 353s
  average was the gap *between* attempts, not within one. The bimodal
  distribution is the whole correction — plot before tuning.

## Slice 0 — attribute the gap (no behaviour change)

The `article` verdict is rare and nobody knows why. Four candidate causes, none
yet evidenced:

1. **Resolver-terminal.** The landing stops at Primo results or the ProQuest
   openurl handler and never reaches an article page at all.
2. **Wrong verdict.** Classify runs and answers `login`, `no_entitlement`, or
   `unknown`.
3. **Never classified.** The optional host permission for that provider was
   never granted, so no injection happens.
4. **Retired first.** The verdict arrives after the tab is gone.

Record, per walled attempt, which of these four occurred — and nothing else. The
privacy contract is absolute here: no URL, no host, no page text in any event or
frame, which is why this needs a deliberate encoding rather than "log the page".

**Acceptance:** every walled attempt over a measurement week maps to exactly one
cause, and the four counts sum to the attempt count. Slice 0 is the only slice
allowed to ship without knowing the answer.

## Slice 1 — branches, so this plan is falsifiable before it is written

- **If (1) resolver-terminal:** the signal is wrong, not missing. A per-paper
  article verdict cannot express "this institution's SSO is live"; the honest
  evidence is institution-level. The keepalive already maintains a session
  verdict with a `probeSource` — promoting that to entitlement evidence for the
  institution is the candidate, and it must keep `auth_returned` powerless.
- **If (2) wrong verdict:** adapter rules need fixtures captured from the real
  landings (`papio adapter captures`), per the standing rule that no adapter
  rule ships without a fixture for its scenario. Never hand-guess selectors.
- **If (3) permission:** this is an options-page grant-flow problem and a UX
  slice, not a lifecycle one. Note the platform limit: nothing can grant an
  optional host permission programmatically, so the fix is making the real
  prompt reachable and legible.
- **If (4) retired first:** ordering work that belongs with the surface
  lifecycle rules, not here.

Do not implement more than one branch. If Slice 0 shows a mix, take the largest
class first and re-measure before touching the second.

## Invariants that must survive every slice

- `auth_returned` alone NEVER resumes dependents. State `human` proves an IdP
  round trip, nothing more.
- One sign-in surface per institution at a time.
- No URL, host, or page text in any event, frame, or persisted record.
- An in-flight institutional effect permit vetoes any retirement.
- Closing a sign-in's owner retires its occupancy; dependents park again rather
  than riding a stale entitlement.

## Acceptance, live, on the operator's machine

- Baseline to beat: 673 `auth_returned` against 2 `entitled_landing`.
- After Slice 1: one human sign-in carries N papers with N measurably greater
  than 1, and opening a sibling needs no second sign-in.
- Throughput: papers reaching `ready`/`imported` per day, against today's
  baseline of 1–2.
- The queue: 126 papers older than seven days should start falling.

## Abort criteria

1. A slice needs `auth_returned` to mean entitlement.
2. A slice puts a URL, host, or page text into an event or frame.
3. Slice 0 cannot attribute more than 80% of attempts.
4. A slice exceeds roughly 400 changed lines outside tests.
5. Two consecutive rounds fail to move the live entitlement count.

## Review round, 2026-08-21 — four reviewers on the day's shipped work

Run because the day's production diff was +1442/-242 across 26 files including a
protocol change, two migrations, and the claim/permit/lease core — and because
the author had self-corrected five times in that subsystem in one day. One P0 and
six P1s came back, **every one of them in code shipped that same day**. Fixed in
`315a477`, `6fbeee2`, `6686f69`.

### Fixed

- **P0, live.** An entry lease is renewed on every same-owner reserve call, so
  its deadline is rolling, not absolute: a holder re-reserving inside the bind
  deadline never reaches the getter's expiry and holds its institution forever.
  The new offer gate honoured that, converting a morning spin into an afternoon
  starvation. The slot is now RETIRED (fenced; a stale error reads as
  still-held, because the slot changed hands rather than freeing), a holder with
  a binding still blocks, and a spent candidate no longer counts as capability.
- **P1.** The automatic admission branch parked unconditionally, so a dependent
  behind a candidate-less owner never reached the gate that frees it — the
  starvation survived its own fix.
- **P1.** The offline gate returned bare on a false premise; a claimed candidate
  has no daemon-side re-drive and both local triggers self-consume. Papers now
  park a revival in a dedicated map.
- **P1.** `institutionalBind` could emit `bound` without the identity pair when
  a best-effort occurrence lookup failed, stranding a surface that could never
  report its own loss. The occurrence is now established BEFORE the bind and
  refused with a structured outcome.
- **P1.** `onTabRemoved` raced a queued durable claim write; it now awaits the
  serialized ledger snapshot.
- **P1.** Protocol: the identity pair is presence-checked rather than
  value-checked (Go accepted an explicitly empty pair that TypeScript rejected),
  and the published schema's bound/refusal branches now match both parsers.

### Deliberately NOT fixed — residuals, with reasons

1. **A `human` lease that never became entitled has no expiry path.** A bound
   surface converts reserved→human with a null deadline; if its tab dies without
   `owner_closed`, the stale sweep retires the claim but deliberately does not
   release the lease (reconnect survival), and the unbound sweep excludes it
   because `owner_binding_id` is set. Dependents then gate forever. This is the
   same immortality class fixed twice for claims, now in the lease. **Most
   likely next defect; no fix attempted.**
2. **Capability is still approximated.** `leaseHolderCanConvert` excludes spent
   candidates but a candidate that exists and is permanently unschedulable
   (route-suppressed, non-awaiting) still blocks dependents. Tightening to "live
   claim OR eligible candidate" needs a proven path first.
3. **Accounting cannot end an always-queued loop.** Confirmed: a queued accept
   correctly opens no epoch, so `FruitlessEpochs` stays 0 forever and only the
   independent seven-day age fence can silence it. An offer-side suppression was
   written and reverted the same day because its test could not be made to fail
   — the harness quiesces the action once the clock advances. Needs a harness
   capability before a fix.
4. **`0046` mistakes a missing candidate row for "no surface ever existed".**
   Legacy URL offers open real tabs without creating that row, so up to all 85
   survivors may have had legitimate attempts reset. Self-healing — they
   re-quiesce after three real drives — and a third migration would need the
   same unprovable predicate.
5. **`evictLostSurfaceClaimTx` proves "gone" by generation, not by absence.**
   Structurally unreachable in production (every generation bump runs the stale
   sweep first, and a failed sweep disables the claim path), so P2: a
   dev-harness or direct Store caller can still abandon a live tab.
6. **The zero-install exception is stale.** AMO verified at zero daily users
   today, but the Chrome Web Store listing no longer exposes a count, and
   `AGENTS.md` still records 2026-08-06. The protocol break shipped under an
   exception that cannot currently be fully re-verified.

## Live after the review fixes, 2026-08-21 — no churn, no starvation, no work

Deployed and reloaded. The good news is negative: zero bind refusals, zero
phantom retirements needed, no live claim held by anything. The bad news is also
negative: **zero live claims with 21 eligible candidates and 129 papers
waiting.** Nothing is blocked and nothing is moving.

Measured cause, and it is the next defect to fix: the automatic admission
switch has cases for an ABSENT lease, a landed-and-entitled lease, and a lease
this job already owns. It has **no case for `expired`** — and nothing ever
deletes an expired row. Both `ExpireUnboundAuthenticationEntryLeases` and the
getter's own normalization leave `state='expired'` behind, so from the first
expiry onward every candidate at that institution falls to `default` and is
parked forever.

**A one-line "treat expired as free" is wrong, and three pinned tests say so.**
It was tried and reverted:

- `TestAutomaticCandidateOfferGatesOnEntitledLandingAndOwnerCloseRetiresClaim` —
  after `owner_closed`, dependents must wait for fresh arbitration, and an
  expired row is not that.
- `TestFocusedCandidateWaitsWhileAnotherJobHoldsTheSignIn` — the admission loop
  would reserve the freed slot for some other candidate, stealing it from the
  explicitly focused paper.
- `TestCandidateParkedByAnotherJobsSignInIsNotOfferedAnyway` — a holder whose
  deadline lapsed under the bridge clock would stop blocking.

So the question is a design one, not a predicate one: **what retires an expired
entry-lease row, and who is allowed to take the slot next?** Absent-versus-
expired is currently a meaningful distinction the admission path honours in one
direction only. Answer that before touching the switch, and expect it to
interact with residual 1 above (a `human` lease that never became entitled has
no expiry path at all).

## Slice 0 answered from existing events, 2026-08-30 — no instrumentation needed

Slice 0 asked for a per-attempt cause encoding. It did not need one: the
`browser.provider_outcome` event already carries `outcome`, `adapter_id`,
`adapter_version` and (since the host-attribution change) `host`, so the four
causes are separable from the store as it stands. Measured on the operator's
own store, all time, 212 outcomes:

| outcome | count |
| --- | --- |
| `ui_changed` | 172 (81%) |
| `no_entitlement` | 15 |
| `cancelled` | 14 |
| `wrong_work` | 10 |
| `rate_limited` | 1 |
| **`article`** | **0** |

Zero `article` verdicts, ever. That is the whole entitlement gap in one row:
`entitled_landing` is emitted only from `applyVerdict`'s `case "article"`, so a
corpus with no `article` verdict can never produce entitlement evidence, and
sibling sharing has nothing to fire on.

`ui_changed` by adapter — 124 rows predate host/adapter attribution, so 48 are
attributable:

| adapter | `ui_changed` | cause |
| --- | --- | --- |
| `sciencedirect` | 20 | **unpainted page** (new, see below) |
| `primo` | 19 | 1, resolver-terminal |
| `proquest` | 4 | 1, resolver-terminal |
| `wiley` | 3 | unpainted or 2, unconfirmed |
| `jamanetwork` | 1 | unconfirmed |
| `ebsco` | 1 | 1, resolver-terminal |

So the mix is roughly even between cause 1 (24: the landing stops at the
resolver or a database and never reaches an article) and a cause the plan's
taxonomy does not contain.

### The fifth cause: classify ran against a document that never painted

This is NOT cause 2. The selectors are correct; the page was not rendered.
Proven three ways on the 0.6.0 drift capture
(`captures/www.sciencedirect.com/2026-08-27T03:49:34.017086Z-observed.html`):

- 28,349 bytes, against 262 KB for the same article in a visible tab — the
  ScienceDirect adapter comment records the measured 32 KB unpainted signature
  (`extension/src/adapters/types.ts:777-780`).
- Its only `a.accessbar-utility-link` carries **no `href` attribute at all** and
  `aria-disabled="true"`.
- `bun run tools/adapter-try.ts <capture> --id sciencedirect --expect article`
  reports `no rule matched`, with `meta[name='citation_title']` HIT and both
  `any` href selectors MISS.

`requiresVisible` + `revealForHydration`
(`extension/src/background.ts:revealForHydration`) exist for exactly this and did not
prevent it. Three candidate causes were considered; the first is now fixed, the
second is ruled out, the third is still open.

**Fixed — the reveal was inert outside work-window mode.**
`revealForHydration` returned early whenever `this.store.workWindowID` was
undefined, but `requiresVisible` is a property of the ADAPTER, not of the
surface. `in-window` and `tab-group` mode have no work window, so the reveal
did nothing at all in either, and tab-group is the worse case: the handoff tab
is created inactive inside a COLLAPSED papio group
(`test/background.test.ts` pins `{ url: OPENURL, active: false }` plus
`collapsed: true`), so the document is even less likely to render than in a
minimized window. The window half is now conditional and the tab half — the
half that actually makes a page paint — always runs. Pinned by
`"a visible-required adapter is revealed in tab-group mode too"`; restoring the
early bail fails exactly that one test and nothing else, so the work-window
path is provably unchanged.

**Ruled out — the reused-window tab being created inactive.** It is created
`active: false` (`extension/src/background.ts:6105`), but the reveal cycle
recovers it in one extra load. The test `"a resolver-routed landing on a
visible-required adapter reloads instead of drifting"` pins that recovery,
including the background-tab return. Not the cause.

**Still open — whether a `normal` but unfocused window paints at all.** Every
measurement in the spec comment that produced 262 KB was a genuinely visible
tab. If an unfocused work window does not hydrate this SPA, no reveal cycle can
fix it and `requiresVisible` needs a different contract. This needs the
operator's own authenticated browser: a scratch profile is not a valid
instrument — measured 2026-08-30, a fresh unauthenticated profile never
rendered the access bar in any of the three window states inside 30s, so it
cannot discriminate. `papio browser sessions` reported "no browser has
connected since daemon start", so the measurement was not available.

Ruled out for the 2026-08-27 landing: `job.tab_id` was valid (the claim for
`binding_9f5f8b514d8418e1d88fcfb477013234` records `tab_id=1421129698`), and
`access_mode = 'delegated'` in the deployed config, so neither reveal gate
precondition failed.

**To settle the open item:** with the extension connected, drive one
ScienceDirect paper in each handoff surface and compare the drift capture's
byte size against the 28 KB unpainted signature.

### Why a completed sign-in recorded no reusable entitlement, found 2026-08-30

Measured during a live operator sign-in. `browser.auth_returned` was recorded at
11:05:18Z with `elapsed_ms: 267951`, and `recordAuth` converted the lease from
`reserved` to `human` (`internal/browser/bridge.go:recordAuth`). Entitlement
still depends on an applied `entitled_landing`, and
`admitAutomaticMaterializationCandidates` admits dependents only when the human
lease also has `entitled_at`
(`internal/browser/bridge.go:admitAutomaticMaterializationCandidates`,
`internal/job/claim_observation_apply.go:ApplyClaimObservation`).

The browser never produced that article verdict. Its in-memory classification
net lasted `8 × 2500 ms = 20 s`, then stopped silently. The only recovery path
was also gated on the worker-memory `federatedLoginRouted` Set. The measured
sign-in took 268 s, long enough for MV3 to stop the worker.

The trace after `auth_returned` contains `handoff.opened` at 11:10:24 and
`browser.job_accept` at 11:10:42. What it does **not** contain is any later
classification or entitlement evidence before `auth_pending` at 11:13:42.

#### Recovery fix, corrected after review

`recheckUnclassifiedLandings`
(`extension/src/background.ts:recheckUnclassifiedLandings`) runs from the
one-minute keepalive wake. Its counters survive a worker restart through
`storage.session`; a full browser restart clears them
(`extension/src/state.ts:chromeBackend`). Before any packaged download takes a
new local latch, `claimDownloadInitiated` queries the browser's durable download
list for an in-progress item carrying that job's filename, so a browser-resumed
fetch still blocks a duplicate
(`extension/src/background.ts:claimDownloadInitiated`).

The review found three defects in the first version, all fixed:

1. `auth_started_ms` was not proof of federated login. Generic non-provider
   navigation and a drive timeout can set it. `maybeRouteFederatedLogin` now
   writes `federated_login_routed_ms` before papio navigates the exact federated
   route, clears it on navigation failure, and `retryFederatedEvidence` accepts
   only that marker or the live Set
   (`extension/src/background.ts:maybeRouteFederatedLogin`,
   `extension/src/background.ts:retryFederatedEvidence`).
2. The two status arms each took three rows, so one wake could run six probes,
   the first stuck rows could starve the rest, and no attempt ceiling existed.
   The sweep now takes three rows total, orders by the persisted last-check
   time, and stops after ten attempts per job.
3. The auth-pending arm omitted `download_initiated`. Both selection and
   `retryFederatedEvidence` now reject an in-flight download.

The same ScienceDirect paper provides the measured reason for a durable wake:
an unpainted 27,491-byte capture at 11:35 and painted 261,738/261,674-byte
captures at 12:33, fifty-eight minutes apart. A one-minute recovery closes that
gap without waiting for another tab event.

### The `ui_changed` bucket conflated distinct failures, found 2026-08-30

Historical snapshots must name their unit. At the commit cutoff, the fortnight
contained **37 `ui_changed` events across 31 unique jobs**:

| adapter class | unique jobs | events where different |
| --- | ---: | ---: |
| `primo` | 14 | 14 |
| `sciencedirect` | 7 | 11 |
| `proquest` | 2 | 2 |
| `wiley` | 3 | 3 |
| `ebsco` | 1 | 1 |
| Figshare, no adapter id | 1 | 1 |
| no host or adapter id | 3 | 5 |

The all-time snapshot was 174 `ui_changed` events. Of those, 117 carried
adapter version `0.1.0`, had no host field, and came from the earliest
development build. That historical population must not set current repair
priority without a version or time filter.

Capture inspection separated four causes:

| class | examples | adapter drift? |
| --- | --- | --- |
| provider error page | `Internal Server Error` (ScienceDirect, 22 KB), `Error 404 \| Cochrane Library` ×2 | no |
| page never rendered | MDPI 439 B ×3, Wiley 305–314 B ×3, Elsevier auth shell 878 B ×2, ScienceDirect 27–31 KB ×4 | no |
| authentication infrastructure | identity-provider pages | no |
| genuine coverage gap | IEEE, MDPI, Cochrane, JSTOR, Frontiers, JAMA, Springer, JMIR, Gale, Informit, ClinicalKey, ProQuest, ChemRxiv | yes |

#### Resolver result fixed

Ten retained Primo captures split into three app shells (1.1–5.8 KB), five
records whose title had not rendered (22.8–29.9 KB), and two painted not-held
records (53.7/53.8 KB). The source selector appears in none of those ten.

No positive phrase names the not-held state. Against
`extension/fixtures/primo/success.html`, the availability control and source
link start at bytes 35,130 and 52,954. They are 17,824 bytes and 26 closing
containers apart, so they paint independently.

`ClassifyRule.deferUntilDeadline`
(`extension/src/adapters/types.ts:ClassifyRule`) keeps the not-held rule out of
every early classification. At the deadline, the earlier article rule wins if
the source link exists; otherwise the scoped
`nde-record-availability .available-at-button` marker names
`no_entitlement`. `primo` is version `0.3.0` with a 15-second settle budget.
Across the same captures: eight stay `unknown`, two become `no_entitlement`,
and the held fixture remains `article`.

The serial cost is explicit. At `HANDOFF_DRIVE_LIMIT = 1`, 110 not-held Primo
records would consume `110 × 15 s = 27m30s` before navigation and cleanup; the
measured fortnight had 14 such jobs, or 3m30s of settle budget. The 15-second
value remains because eight of ten captures from the former five-second era
were still shells. Raising drive concurrency is a separate stabilization gate,
not something an adapter may do to hide its own render cost.

Both async classifier copies now use the same temporal policy. Article
readiness remains provisional through the budget; a non-article marker gets the
existing 50 ms settle window; a deferred rule can classify only at the
deadline. Tests cover another ready rule, a transient source link, and a source
link that remains (`extension/src/adapters/types.ts:interpret`,
`extension/src/plan.ts:planExecution`).

#### Error pages and IdP captures fixed

`assessDrivenPage` recognizes only two measured, two-signal load failures:
ScienceDirect's exact internal-error title plus its provider failure sentence,
and Cochrane's 404 title plus its missing-page sentence
(`extension/src/background.ts:assessDrivenPage`). An article with the title
“Internal Server Error” and ordinary article text stays normal.

The current implementation reloads a detected failure at most three times
inside the ten-attempt landing budget, then leaves the existing action parked.
It never records the transport page as `ui_changed`.

#### Provider load-failure interface selected, not yet implemented

Four interface shapes were compared:

1. A dedicated `provider_load_failure` message was precise, but added another
   message family, a feature flag, and a sixth triage schema for one observation.
2. Widening `handoff_outcome` reused its open-action guard, but required a new
   enum, a conditional payload field, a feature retirement, and a staged
   strict-parser migration.
3. Consolidating seven read-model flags would free five slots, but mixes a
   broad capability cleanup with this one user-visible failure.
4. The existing job-scoped `error` frame already accepts a bounded normalized
   code, preserves the action, and is valid across old and new peers. This is
   the selected interface.

The wire shape stays unchanged
(`internal/protocol/protocol.go:ErrorPayload`):

```json
{
  "code": "provider_load_failure",
  "message": "Provider page failed to load after three automatic reloads."
}
```

The sentence is fixed extension copy. It carries no host, URL, title, page
text, path, query, fragment, credential, session value, or browser error. The
daemon persists only `code`; the generic `MsgError` branch already appends a
non-terminal `browser.error` event and leaves the job and action untouched
(`internal/browser/bridge.go:handle`).

The extension reports only after the page still shows the same measured
500/404 state following the third reload. `retryProviderLoadFailure` returns
`reloaded`, `exhausted`, or `ignored`; both classifier callers consume that one
atomic result. A session-persisted `provider_load_failure_parked` marker keeps a
closed failed tab from taking the normal cancellation path. A separate sent
marker is written only after `send` succeeds
(`extension/src/background.ts:retryProviderLoadFailure`,
`extension/src/state.ts:ActiveJob`).

For `provider_load_failure`, the daemon transaction selects the current open
handoff action, writes its daemon-owned action id into the event, and sets the
existing `human_actions.diagnosis` to `provider_load_failure`. A duplicate for
the same still-open action is a no-op. The transaction never changes the action
kind, detail, access binding, status, or job state. This keeps the route
discriminator intact and gives the Inbox durable state instead of inferring
from its bounded Activity page.

No new Inbox field or enum is needed. `humanActionItems` maps that diagnosis to
the existing `open_page` guidance and substitutes fixed display detail:
“Provider page did not load after three retries. Open the link to try again.”
The Inbox labels that existing open operation “Open link”
(`internal/triage/triage.go:humanActionItems`,
`extension/src/inbox.ts:operationOpenLabel`). `ActivityText` renders “Provider
page failed to load after three automatic retries”
(`internal/store/activitytext.go:ActivityText`). Job diagnosis gets a matching
closed reason and retains `can_open_action`.

An explicit Open is a new operator attempt, not a fourth automatic retry.
`openHandoffUnlocked` first re-probes a tracked tab. It reloads only while that
tab still classifies as `load_failure`, then resets the local report marker and
three-reload budget after Chrome accepts the reload. A missing tab follows the
existing fresh-route path (`extension/src/background.ts:openHandoffUnlocked`).

Release the daemon presentation and idempotent action annotation first, then
the extension sender. A new extension with an old daemon still sends a valid
frame; the old daemon records the safe generic Activity text and preserves the
action. An old extension with the new daemon never sends the code. No feature
slot, version floor, parser alias, or protocol compatibility shim is required.

`recordUnknown` now reads the live tab before the current host can authorize its
own first capture. `isAuthenticationURL` rejects login, IdP, SSO, auth,
Shibboleth and OpenAthens routes, so authentication pages cannot enter
diagnostic HTML or a `page_capture` frame
(`extension/src/background.ts:recordUnknown`,
`extension/src/keepalive.ts:isAuthenticationURL`). Unregistered provider pages
remain capturable for adapter work.

### The starvation has a named cause, found 2026-08-30

The operator's screen showed a papio-group tab on
`idp.example.edu/idp/profile/SAML2/Redirect/SSO`, titled
"Example University Login Service - Stale Request". That page is a
terminal Shibboleth dead end: the SAML conversation is spent, so no wait and no
click completes it. Only a fresh request from the resolver entry point can.

Measured on the live store at 10:43 that day, job_74e8e6f245e8048481991e5d25:

| observation | value |
| --- | --- |
| `browser.auth_pending` | 10:20:20, then again 10:30:12 |
| claim phase / tab | `navigated`, tab 1421129965 |
| claim `lease_until` | renewed to 11:00:12 |
| auth lease rows in store | exactly 1, state `human`, created 2026-08-13, `entitled_at` empty |
| open human actions | 118, of which 87 `requires_auth` |
| oldest open `paywall` action | 2026-08-06 (24 days) |
| `stale_sso` reports, all time | **0** |
| `browser.handoff_failed` rows | 34, all `auth_error`, all `login.openathens.net`, all 2026-08-03 |

**Cause.** `openHandoff` -> `openFreshHandoff` ->
`consultAuthenticationClaim` answers `navigate_existing`.
`focusClaimOwnerTab` fetched the owner tab twice and treated existence as
usability (`extension/src/background.ts:focusClaimOwnerTab`). A tab on a
terminal page therefore passed. `registerHandoffDrive` then asserted
`auth_pending`, opened the login gate, and reserved the institution's one
sign-in slot (`extension/src/background.ts:registerHandoffDrive`). Each cycle
renewed the claim. A renewed claim is neither unbound nor stranded, so the
normal expiry paths cannot fire
(`internal/job/institutional_evidence.go:ExpireUnboundAuthenticationEntryLeases`,
`internal/job/institutional_evidence.go:ExpireStrandedBoundAuthenticationEntryLeases`).

This is the same false claim already measured for an open-access preprint: the
tab reported `auth_pending` every three minutes for two days and held the slot
while 22 papers queued.

**Why the existing detector never fired.** `detectAuthFailure` already
classifies this page `stale_sso`: the path matches `/idp/profile/` and the page
title contains `stale` (`extension/src/authfail.ts:detectAuthFailure`). Its only
normal caller needed a `tabs.onUpdated` event
(`extension/src/background.ts:onTabUpdated`). Reusing a tab that is already
dead navigates nothing, so no event arrives.

**Fix.** `claimOwnerTabFailedSignIn` classifies the proven-live owner tab from
the `url` and `title` that `tabs.get` already returned
(`extension/src/background.ts:claimOwnerTabFailedSignIn`). A recognized failure
reports the existing `handoff_outcome` frame and parks for engagement instead
of asserting a sign-in. It reads no page, needs no host permission, and adds no
wire message.

Recovery is deliberately not attempted in that branch. It is reachable only
when the job has no retained offer URL; a retained-URL job follows the legacy
path instead (`extension/src/background.ts:openHandoff`). `engagement_required`
is the recovery: the operator's next open mints a fresh route.

**Known sibling case:** `focus_owner` parks one job behind another job's claim.
Reporting the owner's dead page under the waiting job would attribute the
failure to the wrong paper. The owner's own next consult clears it.

### A second ScienceDirect layout, found 2026-08-30

Classifying every capture on disk against the repaired rule exposed a shape no
fixture covered. `2026-08-24T12:33:45Z-success` (261 KB, entitled subscription
article pii/S0747563216303168, control `aria-disabled="false"`,
href=`<pii>/pdfft`) contains **no `.accessbar` container and no `.ViewPDF` at
all** — the enabled control lives in
`div.content-details-actions > div.content-actions`. Both committed fixtures were
the access-bar layout, so an access-bar-only rule read a fully-rendered entitled
page as a changed provider, and that is the layout an entitled institutional
route lands on.

Now committed as `extension/fixtures/sciencedirect/subscription.html` with a
second scoped selector. Whether the layout is current or superseded is NOT
established: it is one observed sample and the three later captures are all
access-bar. Both rules are kept because each admits exactly one anchor and each
fails closed on every other fixture.

Verified classification across every sample on disk, adapter 0.8.0:

| sample | verdict |
| --- | --- |
| `fixtures/.../success.html` (access bar) | `article` |
| `fixtures/.../open-access.html` (access bar) | `article` |
| `fixtures/.../subscription.html` (no access bar) | `article` |
| `fixtures/.../no-entitlement.html` | `no_entitlement` |
| `captures/...2026-08-27T03:49:34Z-observed` (28 KB, unpainted) | `unknown` |

### Live cost of this one cause

`job_272d01737a12bbb6a68958eab1` (`doi:10.1016/j.sbspro.2011.10.099`): created
2026-08-23, five `challenge_blocked` errors, quiesced once on
`fruitless_drive_limit`, re-driven across adapter 0.4.0 → 0.5.0 → 0.6.0,
drifted four times, and received 29 `action.reminder` events. It was still
`awaiting_human` on 2026-08-30: one paper, six days, three adapter releases, no
artifact.

### Correction to "Live after the review fixes, 2026-08-21" above

That section's defect — the admission switch having no `expired` case — **is
fixed and shipped.** `internal/browser/bridge.go:11435` now carries
`case lease.State == job.AuthenticationEntryLeaseExpired &&
b.retiredSlotIsCold(lease)`, and `retiredSlotIsCold`
(`internal/browser/bridge.go:11506-11531`) answers the design question that section posed: a
retired row is free once it is older than `AuthenticationEntryBindDeadline`
(2 minutes, `internal/job/institutional_evidence.go:1017`), age being the only
honest discriminator. Residual 1's bound-lease immortality is also addressed,
by `StrandedBoundEntryGrace`
(`internal/job/institutional_evidence.go:1773`, used at
`internal/job/institutional_evidence.go:1800`).

Live confirmation, 2026-08-30: `authentication_entry_leases` holds exactly one
row, `expired`, last updated 2026-08-28 — two days old, therefore cold and
admissible. **No lease is blocking anything.** Leave those paragraphs in place
as history, but do not treat them as the live defect.
