# The institution entry lease: one expired row froze a library

## Shipped and measured, 2026-08-21

Three defects fixed, each mutation-verified, each committed on its own:

1. `18e3db6` — the permit veto covered only the expired-reservation path, so
   browser churn alone could replace a live sign-in holding an unresolved
   provider permit. Slice 0, item 1.
2. `7fa0908` — an entitled sign-in was shared at the offer and refused at the
   BIND. Measured in the operator's own log: **5,562 refusals**, the worst storm
   4,872 in 41 minutes at ~2/s, each one a scaffold tab built and torn down.
   That is the reported tab/group churn and the stranded siblings, one bug.
   Slice 0, item 2.
3. `3998bb5` — slot arbitration was priced as an offer. `limit` arrived as 0 on
   32 of 34 consecutive polls, and that loop is the only code that retires a
   dead slot. Plus the cold-`expired` reader, gated on the existing bind
   deadline rather than a new tunable. Slice 1.

A correction worth keeping: earlier in the session the refusal counter was flat
at 5,561 and I read that as "the churn is dead". It was flat because the freeze
had **starved** the storm — no claims to refuse. A counter that stops moving
because its input stopped is not a counter that reports success.

## The queue still does not move, and the reason is not this lease

Measured after deploying all three: 128 awaiting, 0 live claims, and the entry
lease sitting `expired` and cold with nothing taking it. One diagnostic pass
named the reason per descriptor instead of inferring it:

- three of the four schedulable candidates are skipped `no-handoff-action` —
  their jobs hold only a `manual_download` action, which the automatic path
  does not read;
- the fourth is skipped `focus=true` — an **explicit operator focus request**,
  which the automatic loop defers to the focus path;
- every line carried `canAdmit=false`.

And the four offers holding the entire `maxOutstandingOffers` budget belong to
papers with **zero browser candidates** — legacy-path papers that reported 1, 3,
7 and 15 authentication walls, cannot bind because binding needs a candidate,
and therefore hold their offer until a human finishes the download by hand. They
rotate the budget among a pool of 73 such papers. The paper that HAS a candidate
and a landed sign-in is starved behind them.

So the transport budget is doing two jobs at once: bounding how many browser
surfaces exist, and bounding how many papers may be in flight. A paper parked at
a wall consumes it indefinitely, and the legacy path wins by arriving first.
That is a design decision about which path owns a paper — the same question
raised earlier as "87 papers can only go the legacy path" — and it is
deliberately NOT taken here. Slice 1's fixes are prerequisites for it either
way: without them the winning path would still deadlock on cleanup.

Measured on the operator's machine 2026-08-21: 129 papers awaiting a human, 21
eligible browser candidates, **zero** live claims, and one
`authentication_entry_leases` row in state `expired`. No churn, no errors,
nothing in the log — and nothing moving.

Reviewed by three reviewers before any code was written. The first draft was
**wrong in its central premise, wrong in one stated invariant, and its
recommendation carried four P0s.** This is the corrected plan; the corrections
are recorded rather than quietly folded in, because the same mistakes are easy to
repeat.

## What is actually broken: the reader

The draft said "nothing can leave `expired`". **False.**
`reserveAuthenticationEntryLeaseTx` treats `expired` as replaceable and resets
it to `reserved`, clearing the old owner and binding. The exit exists and is
fenced. What is broken is that **`admitAutomaticMaterializationCandidates` never
asks for it**: its switch matches an absent row, a `human` row that is also
entitled, and a `reserved` row this job already owns, then parks everything else.
An `expired` row parks forever because no reader ever offers it to Reserve.

One reader, one missing case. That removes the migration, the new state, the
delete, and the new event kind the draft proposed.

## Corrected state table

Writers, all `internal/job/institutional_evidence.go` unless noted:

| transition | writer | fence |
| --- | --- | --- |
| absent → `reserved` | `reserveAuthenticationEntryLeaseTx` | INSERT |
| `expired` → `reserved` | same | replaces; clears owner/binding/entitlement |
| `human` → `reserved` | same, via `humanRevoked` | generation differs, owner terminal, evidence absent, or `signed_out` |
| `reserved` → `reserved` | same | same owner: renews lease id and deadline |
| `reserved` → `human` | `convertAuthenticationEntryLeaseToHumanTx` | claim+lease+owner+generation, live deadline; sets a deadline ONLY if unbound |
| `human` → entitled | `markAuthenticationEntryLeaseEntitledTx` | sets `entitled_at`; state stays `human` |
| `reserved` → `expired` | `expireAuthenticationEntryLeaseTx` | exact claim+lease+generation, **`state='reserved'` only — it can never retire `human`** |
| `reserved`/`human` → `expired` | `RetireAuthenticationEntryLeaseAfterOwnerClose` | claim + owner binding |
| `reserved`/`human` → `expired` | `releaseAuthenticationEntryLeasesForBindingsTx` | by retired binding |
| `reserved`/`human` → `expired` | `RetireTerminalAuthenticationEntryLeases` | owner terminal, no `held`/`unknown_completion` permit |
| `reserved`/`human` → `expired` | `ExpireUnboundAuthenticationEntryLeases` | unbound + deadline null/past + `updated_at` older than the bind window |
| `reserved` → `expired` | `getAuthenticationEntryLeaseTx` normalization | past deadline, unless an institutional permit is `held`/`unknown_completion` |

`+` the exact expiry sets only `state`/`lease_until`, so an `expired` row can
**retain** `owner_binding_id` and `owner_tab_hint` while other retirement writers
clear them. Expired rows are not all the same shape.

## P0s in existing code — fix these first, each on its own

These are older than this plan and must not be folded into a slice.

1. **The permit veto is caller discipline, not a fence.**
   `retireAuthenticationEntryLeaseAfterOwnerCloseTx` matches only claim and
   binding; `expireAuthenticationEntryLeaseTx` and
   `releaseAuthenticationEntryLeasesForBindingsTx` have no permit predicate at
   all. Worse, `reserveAuthenticationEntryLeaseTx` checks permits only on its
   `reservedExpired` path and **not** on `humanRevoked` — so a bound `human` row
   whose binding holds a `held`/`unknown_completion` permit is reset to
   `reserved` and its binding cleared when a new generation arrives, the owner
   goes terminal, or evidence is absent. That permits a second irreversible
   provider action on one paper.

   **SHIPPED, scoped to the replacement paths.** The veto now gates every
   reason Reserve replaces a row — `expired`, `humanRevoked`, `reservedExpired`
   — through one extracted `institutionalEffectInFlightTx`, replacing two
   divergent inline copies of the predicate. Pinned by
   `TestAuthenticationEntryLeaseHumanRevocationRefusedWhileEffectPermitHeld`,
   which asserts the refusal leaves the human owner, binding and entitlement
   intact, and that settling the permit lets the same takeover through.
   Mutation-verified: restoring the narrow guard turns it red.

   The three retirement writers are **deliberately left alone**, reversing this
   plan's own "the veto belongs on each transition". Vetoing an owner-close
   retirement would create a FIFTH dead end: nothing else retires a bound row
   whose owner is still `awaiting_human`, since the terminal sweep requires a
   terminal owner. Retirement-under-unresolved-permit needs a disposition, not
   an indefinite hold, so it belongs to Slice 2. Reserve is safe to veto
   because it is retried every poll and migration 0034's
   `effect_permits_live_slot` index admits at most one unresolved permit
   process-wide — a permit that never resolves has already stopped all
   institutional work and has an operator resolution path.
2. **A dependent that is admitted cannot bind.** The `human`+entitled admission
   case proceeds every dependent, but `institutionalBind` then calls Reserve for
   it; at the same generation with fresh evidence `humanRevoked` is false, so
   Reserve answers busy, the bind returns `not_eligible`, and the scaffold is
   torn down. The existing test asserts only that the dependent receives an
   OFFER. So the entitled case is not an end-to-end usable state, and this — not
   the offer side — is the better explanation of "I log in on one tab and the
   others stay stranded". It falsifies the boundary
   `institutional-signin-sharing.md` was about to measure.

## Dead ends, with the sweeps stated precisely

The draft claimed "every sweep declines" a bound `human` lease. Too strong. A
bound `human` row IS released by `releaseAuthenticationEntryLeasesForBindingsTx`
when `ReconcileMaterializationClaims` retires a lapsed claim with no permit row,
when `AbandonTerminalMaterializations` retires a terminal owner's claim, or when
`evictLostSurfaceClaimTx` evicts a lapsed claim for a newer holder; and by
`RetireTerminalAuthenticationEntryLeases` once the owner is terminal with no
`held`/`unknown_completion` permit.

What genuinely has no exit:

1. **`expired`, any binding** — Reserve can take it, no reader asks. This is the
   live freeze.
2. **`human`, bound, NULL deadline, owner alive, no `owner_closed`, and either no
   claim or a claim holding a settled permit.** Nothing reaches it.
3. **`human`, bound, NON-NULL deadline.** A legal sequence produces it: convert
   sees no binding so it sets the bind deadline, then `SetAuthenticationEntryLease
   OwnerBinding` fills the binding without clearing or extending that deadline.
   Past the deadline, the getter normalizes only `reserved` rows and
   `ExpireUnbound` excludes non-empty bindings. Adding a deadline to bound humans
   does not fix this one.
4. **`navigation_error`, an ordinary path.** `applyClaimObservationTx` abandons
   the binding and deliberately never touches the lease; the extension's
   authorized `claim_abandoned` close then SUPPRESSES `owner_closed`. An active
   owner's bound `human` row is left with no claim for any release helper to
   find. Outages and DNS failures make this common, not exotic.

## Why "delete the row on retirement" was rejected

1. **It breaks the same-poll fence.** `admitAutomaticMaterializationCandidates`
   reads the lease per descriptor with no transaction across the loop, so
   candidate A's retirement is immediately visible and candidate B reserves in
   the same poll. An `owner_closed` frame is handled before `poll()` within one
   `Sync`, so deleting its row lets a dependent be admitted in that same
   response — which also breaks the owner-close constraint. The draft's claim
   that the delete "lands in a transaction the poll has read past" was false;
   there is no such transaction.
2. **It would not have unfrozen the live queue.** Changing future retirements to
   DELETE leaves the existing `expired` row untouched and unreadable, so Slice 1
   would have failed its own acceptance on the very institution that motivated
   it.
3. **It opens a TOCTOU at bind.** `BindMaterializationWithLeaseOwner`'s raw
   SELECT treats a missing lease row as a benign no-fence case. Reserve and Bind
   are separate transactions, so a retirement between them makes
   `setAuthenticationEntryLeaseOwnerBindingTx` affect zero rows while the
   existence fallback lets the claim bind anyway — **a live sign-in surface with
   no lease**, which defeats one-entry arbitration entirely.

Also: absence is NOT uniformly "free". `ApplyClaimObservation` deliberately
rejects an absent lease for `wall_observed`, `auth_returned`, and
`entitled_landing`. Cardinality was never the risk — `PK(authentication_claim_id)`,
no inbound foreign keys, one SQLite writer.

## Recommendation: teach the reader, gated by age

Give the admission switch a fourth case: an `expired` row may be taken over
through the ordinary Reserve reset that already exists, but only once its
`updated_at` is older than a cooling interval. The tombstone is load-bearing — it
is what defers a mid-poll retirement to a later poll, satisfying the same-poll
fence by construction instead of by a poll-local set every future caller must
remember.

Properties: no migration, no new state, no delete, no new event kind, the audit
row survives, no bind TOCTOU, and the legacy frozen row is picked up once aged.
Costs: the interval must come from measurement, and the admission switch and
`institutionSignInHeldElsewhere` must agree for every shape in the table — a
disagreement between those two readers is exactly today's class of bug.

## Slices

**Slice 0 — the two P0s above**, separately, each with its own test. The permit
veto moves onto the transitions; the entitled-dependent bind path is decided
(special-case entitled sharing at bind, or move lease authority). Nothing else
ships until these do, because Slice 1 increases traffic through both.

**Slice 1 — unfreeze the reader.** The fourth case, the cooling interval, and
reader parity. Realistic size **350–450 lines including tests**, across six
touchpoints; the draft's 300 was wrong.

**Slice 2 — the three remaining dead ends.** Shapes 2, 3 and 4 above. Needs a
distribution first, and the draft named the wrong metric: `auth_pending` →
`auth_returned` measures how long a human takes, which is not the question. The
quantity is the **post-auth dead-tab TTL** — after a sign-in lands, how long
before a surface that will never report `owner_closed` may be declared gone.

Do NOT ship Slice 1 alone: unfreezing sends more papers into the bound-`human`
shapes that have no exit. It ships with a guard or a monitor and a rollback.

## Invariants — with one correction

- An institutional effect permit that is `held` or `unknown_completion` vetoes
  every retirement, takeover, and human replacement, **on the transition itself**.
- One sign-in surface per institution at a time.
- `auth_returned` alone is not entitlement.
- A slot retired mid-poll is arbitrated on a later poll, never raced within one —
  including retirements caused by a frame handled earlier in the same `Sync`.
- Every takeover keeps its exact `(claim, lease_id, generation)` or exact-binding
  fence, so a stale callback cannot retire a SUCCESSOR row.
- `navigation_error` must never mutate the lease.
- **Corrected:** the draft asserted "a generation change must never release a
  human sign-in". The code deliberately does the opposite —
  `reserveAuthenticationEntryLeaseTx` computes `humanRevoked` on a generation
  difference and overwrites the human row, clearing owner, evidence, binding and
  entitlement, and a new-generation claim request or bind can trigger it
  directly. Either that revocation is intended, in which case the reconnect-
  survival rationale applies only to the sweeps and must be written that way, or
  the replacement fence is wrong. **Decide this before Slice 2** — it is listed
  below as an open question, not as an invariant.

## Acceptance

"The backlog moves" is insufficient: it can be satisfied by the legacy URL path,
which never touches this lease, or by an unrelated institution. The live check is
a bounded soak recording, for every admitted candidate, its claim, lease owner
and binding, asserting:

- **liveness** — institutional candidate offers per poll rise from zero, claims
  reach `bound` with a lease owner, and the 129-paper backlog falls;
- **safety, all exactly zero** — more than one live sign-in binding per
  authentication claim at any instant; any retirement, takeover or human
  replacement affecting a binding with an unresolved permit; any bound surface
  whose lease row is absent.

Unit acceptance: a takeover is refused while a permit is unresolved; refused
inside the cooling interval and allowed after it; refused when it would replace a
successor row; and the admission switch and the gate return the same verdict for
every shape in the corrected table. Any retirement assertion is scoped to the
no-permit case.

`TestAutomaticCandidateOfferGatesOnEntitledLandingAndOwnerCloseRetiresClaim` must
pass unmodified. Needing to edit it is a design error, not a test error.

Add regardless: a two-candidate test on ONE claim — one focused, one automatic,
at a free slot — asserting which owns the lease afterwards. Nothing pins it
today.

## Open questions, deliberately not constraints

1. **Does a focused paper outrank an automatic candidate for a free slot?**
   Today automatic admission runs before the focus loop in `Sync`, so it does
   not. `TestFocusedCandidateWaitsWhileAnotherJobHoldsTheSignIn` does not pin
   this — the draft mistook an ordering accident for an invariant.
2. **Is `humanRevoked` on a generation change intended?** See the corrected
   invariant above.

## Abort criteria

1. A slice needs a permit veto relaxed.
2. A slice needs `TestAutomaticCandidateOfferGatesOnEntitledLandingAndOwnerCloseRetiresClaim`
   rewritten.
3. Slice 1 exceeds 450 lines outside tests.
4. Slice 2 starts before the post-auth dead-tab TTL is plotted.
5. Slice 1 deploys and the institutional offer/claim counters stay at zero.
6. The cooling interval becomes a guessed number rather than a measured one.
7. Either open question above is still open when Slice 2 starts.
