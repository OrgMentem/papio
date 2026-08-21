# The institution entry lease has states with no exit

One expired row froze an entire library. Measured on the operator's machine
2026-08-21: 129 papers awaiting a human, 21 eligible browser candidates, **zero**
live claims, and one `authentication_entry_leases` row in state `expired`. No
churn, no starvation warning, nothing in the log — and nothing moving.

This is not the offer-churn bug (fixed, `aeb3783`), the rolling-renewal
starvation (fixed, `6fbeee2`), or the sign-in sharing gap (planned,
`institutional-signin-sharing.md`). It is the layer under all three: the entry
lease is a state machine whose states do not all have exits.

## The state machine as it exists

Writers, all in `internal/job/institutional_evidence.go`:

| transition | function | fence |
| --- | --- | --- |
| absent → `reserved` | `reserveAuthenticationEntryLeaseTx` | inserts, or replaces a non-live row |
| `reserved` → `reserved` | same function | same owner: renews `lease_id`/deadline |
| `reserved` → `human` | `convertAuthenticationEntryLeaseToHumanTx` | claim+lease+owner+generation, live deadline |
| `human` → entitled | `markAuthenticationEntryLeaseEntitledTx` | sets `entitled_at`; state stays `human` |
| `reserved`/`human` → `expired` | `expireAuthenticationEntryLeaseTx` | exact claim+lease+generation, `reserved` only |
| `reserved`/`human` → `expired` | `RetireAuthenticationEntryLeaseAfterOwnerClose` | by owner binding |
| `reserved`/`human` → `expired` | `releaseAuthenticationEntryLeasesForBindingsTx` | by retired binding |
| `reserved`/`human` → `expired` | `RetireTerminalAuthenticationEntryLeases` | owner job terminal |
| `reserved`/`human` → `expired` | `ExpireUnboundAuthenticationEntryLeases` | **no binding** + deadline passed |
| `reserved` → `expired` | `getAuthenticationEntryLeaseTx` normalization | past deadline, unless an institutional permit is `held`/`unknown_completion` |

Readers that gate work: `admitAutomaticMaterializationCandidates` and
`institutionSignInHeldElsewhere` (`internal/browser/bridge.go`), and the
observation guards in `internal/job/claim_observation_apply.go`.

## Dead end 1 — `expired` is terminal, and it parks everything

Nothing deletes an expired row, and the admission switch has cases for exactly
three shapes: absent, `human` **and** entitled, and `reserved` owned by the
asking job. An `expired` row matches none, so it falls to `default` and every
candidate at that institution is parked — permanently, because no later
transition can leave `expired`.

Consequence: an institution works until its first expiry, then never again. That
matches the live measurement exactly, and it explains a throughput of 1–2 papers
a day against a 129-paper backlog: only the legacy URL path, which does not
consult this lease, was still moving.

## Dead end 2 — a `human` lease with a binding never expires at all

`convertAuthenticationEntryLeaseToHumanTx` sets
`lease_until = CASE WHEN owner_binding_id IS NULL OR '' THEN <now+bind deadline>
ELSE NULL END`. So a sign-in that reached a bound surface has **no deadline by
design**. Every sweep then declines it:

- `ExpireUnboundAuthenticationEntryLeases` — excluded, the binding is non-empty.
- `RetireTerminalAuthenticationEntryLeases` — excluded, the owner sits
  `awaiting_human` indefinitely.
- `RetireAuthenticationEntryLeaseAfterOwnerClose` — needs an `owner_closed`
  report that a dead worker never sent.
- `AbandonStaleMaterializations` — retires the CLAIM and deliberately does not
  touch the lease (reconnect survival, pinned by
  `TestClaimObservationSurvivesAReconnectSinceArbitration`).

So a tab that dies without reporting leaves its institution held forever, and
`institutionSignInHeldElsewhere` returns held for a `human` lease with no
`entitled_at`. Same freeze, different door.

## Three constraints, all verified by making them fail

A one-line "treat `expired` as free" was written and reverted. Each failure is a
real constraint, not a fixture artifact:

1. **After `owner_closed`, dependents must not resume.**
   `TestAutomaticCandidateOfferGatesOnEntitledLandingAndOwnerCloseRetiresClaim`.
   Owner-close retirement sets `expired`; treating that as free let a dependent
   ride an entitlement whose surface is gone.
2. **An explicitly focused paper must win a freed slot.**
   `TestFocusedCandidateWaitsWhileAnotherJobHoldsTheSignIn`. The admission loop
   skips `focusPending` jobs, so making `expired` takeable let an automatic
   candidate reserve the slot the focused paper was waiting for.
3. **A slot retired mid-poll must not be taken in that same poll.**
   `TestCandidateParkedByAnotherJobsSignInIsNotOfferedAnyway`. Attributed by
   trace: candidate A's gate retires a phantom holder, then candidate B, later
   in the same loop, finds it free and takes it. Today's shipped code parks A
   and defers to the next poll, which is why it passes.

Constraint 3 is the general form of the other two: **a freed slot must be
arbitrated, not raced.**

## Options

**A. An explicit `free` state, reached only by arbitration.**
Add a state meaning "retired and available", set by every retirement path.
Admission may take a `free` row exactly as it takes an absent one, but only on a
later poll than the retirement (constraint 3), and focus-first (constraint 2).
Honest, but it is a schema migration plus every writer and reader touched, and
`expired` versus `free` is a distinction a reader can get wrong in the same way
`absent` versus `expired` is wrong today.

**B. Delete the row on retirement.**
Absent already means free and every reader handles it. Retirement becomes
`DELETE`, so dead end 1 disappears without a new state, and constraint 3 is
satisfied because the delete lands in a transaction the current poll has already
read past. Cost: the row currently carries the audit trail of who held the slot
and when, so deleting it loses evidence unless the retirement is evented first.
Also needs care that a fenced delete cannot race a reservation.

**C. Keep `expired` and give admission a fourth case, deadline-gated.**
Smallest diff: `expired` becomes takeable only when `updated_at` is older than a
cooling interval, which satisfies constraint 3 by construction and leaves the
audit row in place. Cost: a new interval to justify (and this project's rule is
to plot distributions before choosing one), and it does nothing for dead end 2.

## Recommendation

**B for dead end 1, plus a deadline for dead end 2.** Retirement should delete
the row — with the retirement recorded as an event first, so evidence survives
the row — because "absent means free" is a rule every reader already implements
correctly, and adding a fourth state to a machine that just froze a library for
having three is the wrong direction. Dead end 2 is separate and simpler: a
`human` lease whose owner has no live claim and no in-flight permit has nothing
to protect, so it needs a deadline like every other state, and the reconnect
rationale must be re-expressed as "a generation change does not release" rather
than "no deadline exists".

Sequenced deliberately: dead end 1 alone unfreezes the live queue and is
measurable within minutes. Do not bundle them.

## Slices

**Slice 1 — unfreeze.** Record a retirement event, then delete the row, in one
transaction, in every retirement path. Admission and the gate keep their
existing "absent means free" semantics untouched. Focus-first ordering is
verified, not assumed.

**Slice 2 — dead end 2.** Give a bound `human` lease a deadline, and make the
reconnect-survival rule explicit at the sweep that must honour it. Requires a
distribution first: how long does a real human sign-in take, measured from
`auth_pending` to `auth_returned` on this store, p50/p90/p99 — the bind deadline
is 2 minutes and a human sign-in is plainly longer than that.

## Invariants that must survive

- An institutional effect permit that is `held` or `unknown_completion` vetoes
  every retirement, in every path.
- One sign-in surface per institution at a time.
- A generation change means the browser session changed, not that the human's
  sign-in died; it must never by itself release a lease.
- `auth_returned` alone is not entitlement.
- An explicitly focused paper outranks an automatic candidate for a free slot.
- A freed slot is arbitrated on a later poll, never raced within one.

## Acceptance

- Live, Slice 1: the frozen queue moves. Baseline to beat is today's — 129
  awaiting, 21 eligible, 0 live claims, 0 admissions per poll. Success is a
  non-zero admission rate and the backlog falling.
- Unit: a retirement leaves no row; a candidate admitted after a retirement is
  admitted on a later poll than the retirement; a focused paper beats an
  automatic one to a freed slot; a permit in flight blocks every retirement path.
- The three constraint tests above must pass unmodified. If a slice needs one of
  them edited, that is a design error, not a test error.

## Abort criteria

1. A slice needs an in-flight permit veto relaxed.
2. A slice needs one of the three constraint tests rewritten.
3. Slice 1 exceeds roughly 300 changed lines outside tests.
4. Slice 2 is attempted before its distribution is plotted.
5. The live admission rate does not move after Slice 1 deploys.
