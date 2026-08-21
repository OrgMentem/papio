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
