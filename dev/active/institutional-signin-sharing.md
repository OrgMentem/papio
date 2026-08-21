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
