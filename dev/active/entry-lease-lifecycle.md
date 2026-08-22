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

## The in-flight limit, fixed and verified live

`maxOutstandingOffers` counted offers SENT, not surfaces driven. The four
papers holding all four slots were answering `job_accept` with disposition
`queued` — the extension saying it had parked the offer and was **not** driving
it. The fruitless-epoch fold already respects that signal, and
`job.HandoffAcceptedLease`'s own comment already names the mistake ("charging a
job for waiting its turn … the same mistake as counting raw offers, one layer
down"). This was that layer. Queued papers no longer consume the budget.

Measured before and after on the operator's machine:

| | before | after |
| --- | --- | --- |
| distinct papers offered per 5 min | 4, rotating | **21** |
| accepts: driving vs queued | 4 driving | **1 driving, 20 queued** |
| papio group tabs, over 150s | grew ~1 per 2s during churn | **15 → 15 → 15** |

The tab count is the load-bearing one: freeing the budget did NOT produce a tab
storm, because concurrency was never the budget's to bound — the extension's
`HANDOFF_DRIVE_LIMIT` and the one-sign-in-per-institution lease do that, and
both still hold. One paper drives, twenty wait in the extension's queue.

Two honest notes. A first pass at this reached for `QuiesceAfter` (7 days) and
then drive-recency (10 min); **both were falsified by measurement** — the slot
holders were 6.41 days old and had drive evidence 0.7 minutes old, so neither
rule would have freed anything. The disposition the extension was already
reporting was the only signal that actually separated waiting from working. And
a claim-churn alarm during verification was **my own measurement bug**: an ISO
`T` timestamp compared against SQLite's space-separated `datetime('now')` makes
every historical row look current.

Still open, and now cleanly isolated: those 15 tabs are debris no code will
close. `cleanupOrphanTabs` classifies and closes nothing, and
`reconcileHandoffGroups` is deliberately non-destructive, so every tab from the
churn era survives. That is the operator's "doesn't clean up after itself",
and it is now the only part of that complaint still standing.

## Surface lifecycle: why nothing could close a tab, 2026-08-21

Four defects, in the order they were uncovered — each one hidden behind the
previous, and the last two found only because the third made refusals visible.

1. `f078270` — **every close route is claim-scoped.** `surface_close_request`
   looks up a materialization claim by binding id and answered `not_eligible`
   when it found none. An ordinary URL-bearing handoff has no candidate, so no
   claim, so it took that branch every time — and `not_eligible` reads as a
   refusal the extension must obey. The handoff-drive timeout has always
   intended to retire that tab; the intent was refused on every paper, forever.
   Now answered `unclaimed`: no claim means no candidate, no route and no
   permit, so the daemon has no stake and says so. Verified live: **9
   `unclaimed` closes in one evening**, at exact 3-minute drive-timeout
   intervals.
2. `3630b9b` — **reconcile asked whether the paper still existed**, not whether
   anything still pointed at the surface. The legacy timeout deliberately
   detaches the job (`tab_id: -1`) and leaves it alive, so a tabless paper
   shielded its own abandoned tab from the only retry path there is.
3. `24d365b` — `handoff_parked`: **a paper waiting for a human keeps its handoff action
   open**, so `job_inactive` is false for it and was refused every pass ("the
   binding still has an active browser handoff"). True, and never a reason to
   hold a tab. Asking a human is a request, not a lease on their browser.
   Storage vocabulary is a second gate and a DB CHECK: migration `0047`.
4. `eecba8d` — the **fresh-link park keeps its tab on purpose** — "detaching it would leave
   the job with neither a reusable URL nor a way back to the operator's page".
   The second half stopped being true when engagement began minting fresh
   routes, so the preserved page is a spent single-use link. Retirement is gated
   on measured coldness: 674 recorded returns from a wall, p50 1.2s, p90 5.5s,
   p99 603s, 671 of 674 inside thirty minutes; the threshold is 3x p99.

Also shipped, and the reason 3 and 4 were found at all: **every non-authorized
close now logs its outcome and reason.** A refused close was the one event in
this subsystem with no trace anywhere — the extension asks once and retains the
surface in silence — which is exactly how a blanket refusal of every ordinary
handoff tab survived months of review. The log line found the next two defects
in ninety seconds.

### What is still on the operator's screen, and why

Measured after all four fixes: 22 tabs in the papio group — 12 identical
resolver relics, 8 of the operator's own PDFs, 1 keepalive session anchor, 1
live provider challenge.

The PDFs, the anchor and the challenge are all correct. The **12 relics are not
in the ledger at all**: the popup reports zero reviewable strays, and reconcile
never asks about them. papio has no birth certificate for them, and
`ledgerManagedTab`'s own rule is that it must never earn close authority over a
tab it did not open. They predate the birth-record cutover, so they are inert
and permanently unmanaged.

That leaves one genuine product decision, deliberately not taken here: may
papio close an UNLEDGERED tab sitting inside its own tab group, on an explicit
operator request? Group membership is live positive proof that papio created it
(reuse never folds a tab into the group), and the operator asking is the
consent this whole design reserves. The counter-argument is that "I cannot
prove I opened this" is exactly when a wrong close costs the most. Until it is
decided, the operator's one-gesture answer is to close the tab group by hand.

### The constraint that replaced the frozen queue

Freeing the transport budget let papers move, and 16 of them moved **down**:
`unavailable / browser_rejected`, in one burst at 18:09-18:12 local, nothing in
the 100 minutes since. Each had reached a wall and burned its attempt budget
against a library sign-in nobody has completed. The fruitless-drive rule fired
as designed — it exists to stop exactly the churn this session removed — but the
papers deserved a shared sign-in, not retirement. That is
`institutional-signin-sharing.md`'s Slice 1, now the binding constraint on
throughput, and it needs one real sign-in to measure against.

Measured on the operator's machine 2026-08-21: 129 papers awaiting a human, 21
eligible browser candidates, **zero** live claims, and one
`authentication_entry_leases` row in state `expired`. No churn, no errors,
nothing in the log — and nothing moving.

Reviewed by three reviewers before any code was written. The first draft was
**wrong in its central premise, wrong in one stated invariant, and its
recommendation carried four P0s.** This is the corrected plan; the corrections
are recorded rather than quietly folded in, because the same mistakes are easy to
repeat.

## The three that survived all four, 2026-08-22

The operator reported three tabs for one paper, `job_012f55be2bbfe0abd0ce456e36`
(`10.1177/15480518221144895`), after all four fixes above had shipped. Each
tab was one drive epoch, and the daemon log named the cause outright — the
logging from the previous round doing its job:

```
11:34:01 surface close not_eligible for binding binding_1b006765…:
         disposition does not match the binding's current phase
```

Fourteen of those, every one at an exact 3-minute drive-timeout interval.

5. **The drive timeout asserted `scaffold_idle`, then parked the handoff on the
   very next line.** `scaffold_idle` claims the claim is in phase
   `claimed`/`bound`; a drive that has already opened and navigated a tab is in
   `route_issued`/`navigated`. So the one close attempt this path makes was
   structurally refused whenever a claim existed — and the disposition that
   fits, `handoff_parked`, was already in the vocabulary, already sent by
   reconcile for exactly this state, and defect 3 above had added it a day
   earlier. Claimless handoffs short-circuit to `unclaimed` before the phase
   switch, which is why the previous round's 9 verified closes all passed: they
   were the claimless kind, and they hid this from view.
6. **`isSurfaceCloseDisposition` omitted `handoff_parked`** while the union
   feeding it declared it, so `replayPendingCloseTombstones`' fallback
   downgraded a persisted `handoff_parked` tombstone to `scaffold_idle` — the
   one disposition a navigated claim can never satisfy. A worker death between
   tombstone persistence and `tabs.remove` therefore converted a close the
   daemon would authorize into one it must refuse, permanently. The type is now
   exported and the three hand-maintained copies in `background.test.ts`, all
   of which omitted the same value, bind to it.
7. **The repair pass ran before the damage it repairs.** `reconcileOwnedTabs`
   is the only path that sends a disposition a navigated claim can satisfy, and
   it ran solely from `start()`, at 12s and 90s. The failure it repairs happens
   at 180s. So every startup ran the repair, found nothing, and slept; the
   stranded surface then survived until the next extension restart, which
   restarts the same race. It now also runs on the one-minute keepalive wake,
   rate-limited to `OWNED_TAB_RECONCILE_INTERVAL_MS` (5 min) — the same
   argument `recheckChallengeBlocks` won a day earlier, for the same reason:
   that wake is the one that survives a worker death.

The durable lesson is narrower than "another close bug". Defects 5 and 6 are
both papio asserting a fact about its own state that was **false**, in a
protocol whose whole design is that the daemon checks the assertion and refuses
when it does not hold. The mechanism worked perfectly; it was fed the wrong
claim. A close disposition is a claim about phase, so any new close site must
name the phase it believes it is in, and any new disposition must be added to
the exported type — never to a second copy of the list.

### What this says about the conservatism

Nothing here was too conservative. The retain-by-default rules — cold-park
gating, `active` retains, PDF/pinned/out-of-container cedes, positive evidence
closes — all behaved as designed, and the cold gate is what kept the operator's
live provider challenge on screen while this was going wrong. The apparent
conservatism was three wiring defects wearing its clothes: a wrong assertion, a
silent downgrade of a right one, and a repair pass scheduled where it could
never see the failure. Loosening any guard would have hidden all three.

## A solved captcha is not a fruitless drive, 2026-08-22

The same paper exposed a second, independent defect, and this one had already
retired it. `ProjectHandoffOfferState` counted only
`browser.provider_outcome`, `browser.download_started`,
`browser.download_complete` and a transition out of `awaiting_human` as proof a
drive did anything. A provider security check is none of those — the daemon
received `browser.error {code: challenge_blocked}`, which is neither terminal
nor progress — so the epoch ran out `HandoffAcceptedLease` and was charged as
**silence**. Three of those and `MaxAutomaticHandoffEpochs` fires. That is
exactly this job's history: `browser.handoff_quiesced {"reason":
"fruitless_drive_limit","drive_epochs":3}`, every one of the three interrupted
by a Cloudflare check the operator had gone on to solve. The operator solved
captchas and the paper was retired for it.

The extension already knew. `clearChallengeBlock` retires the ask on a
positive, current re-assessment of papio's own tab and resumes the drive — that
path works, and has four tests. It just told nobody.

New frame `challenge_cleared` (`MsgChallengeCleared`), the same timing-only
`AuthPayload` as `auth_pending`/`auth_returned` and bound by the same
structural privacy invariant: the provider host that showed the check never
crosses the channel, only the fact that it is gone. The daemon records
`job.ChallengeClearedEvent`, and the fold gains a **third** epoch-close mode.

Three modes, and the third is the point:

| close | streak | used by |
| --- | --- | --- |
| `chargeFruitless` | `+1` | lease elapsed with nothing reported |
| `clearStreak` | `0` | a terminal outcome |
| `neither` | unchanged | `browser.challenge_cleared` |

Both halves of `neither` are load-bearing, and the second is what keeps this
inside the existing decisions:

- **Not charged**, because the fold's own rule is that the lease bounds
  SILENCE, and a drive that reported a human gate and then reported the gate
  cleared was never silent. This is the same argument `2d70fbc` made one layer
  up when it stopped charging a queue wait as a drive.
- **Not credited**, because a cleared check is not evidence the drive can now
  succeed — only that this obstacle is gone. Crediting it would zero the
  count, so a provider that challenges on every attempt would refill the budget
  forever and the three-strike rule could never fire. That is the immortal
  handoff `HandoffEpochsResetEvent`'s own comment refuses to open ("never by a
  live path"), and it is not opened here. Pinned by
  `bridge_test.go:TestProjectHandoffOfferStateClearedChallengesCannotMakeAHandoffImmortal`:
  four drives, each cleared and then silent, still quiesce.

This is deliberately not a budget reset, so ADR-0013's boundary holds verbatim
— "does not reset authentication-attempt budgets, and it does not open,
resolve, or retry an auth-stalled human action". It neither credits nor debits,
and it resolves nothing. A check that is never cleared still ages out and is
still charged.

Wire cost: a breaking `papio-browser/1` change, taken under the verified
zero-install floor — AMO `average_daily_users` **0** and the Chrome Web Store
listing showing no user count and no ratings, both re-checked 2026-08-22, not
assumed from the earlier reading. All three validators
(`internal/protocol/protocol.go`, `extension/src/protocol.ts`,
`protocol/browser-v1.schema.json`) land in one commit, and no `hello_ack`
feature gates it because the daemon's emitted feature list is fail-closed at
exactly 32 and slot 32 is taken. The first real install ends that exception; at
that point this frame needs a feature flag and the emitted cap needs the
accept-side widening that is already shipped in the extension.

## A closed tab cannot un-close, 2026-08-22

The eighth defect in this family, and the one the previous three left visible
in the log: 36 rejections reading

```
claim observation owner_closed for <job> not applied:
  stale (gate occurrence has rolled over; adopt the current occurrence id)
```

Each one left a `materialization_claims` row in a non-terminal phase for a tab
that no longer existed — the unclosable surface, arrived at from the other
direction. Two gates, both over-broad, and the second is the one that would
have survived fixing only the first.

**The occurrence fence.** `applyClaimObservationTx` rejects any frame naming a
superseded login occurrence, and its stated reason is exact: an old-cycle
event applied under the current cycle's numbering "would let a queued or
retried old-cycle event — most dangerously a delayed `auth_returned` — renew or
promote the current cycle's lease." True of every kind that touches a lease.
Not true of `owner_closed`, which renews and promotes nothing: it abandons the
claim by binding, retires the entry lease whose `owner_binding_id` is exactly
that binding, and consumes that binding's close authorization. Binding ids are
minted per claim and globally unique — "a binding alone is an exact fence" —
so the report can only ever retire the surface it names. And a rollover means
the human signed out and back in, which makes the old tab *more* certainly
gone, not less. This is the same exemption the lease-generation check inside
the switch already makes for this same kind, one gate up.

**The ordinal.** Lifting the fence alone would not have worked, which is worth
recording because it looked like it would. `owner_closed` also has to pass §3's
ordering rule — a new observation's `event_ordinal` must exceed the highest
applied under that occurrence — and it cannot: it fires from a worker that may
have just died and been recovered, so the extension's counter is gone exactly
when this event is generated. It sends 0, and any occurrence with a single
event already applied rejects that as stale. So `owner_closed` now takes no
position in the ordinal sequence at all, because it is not a step in the login
narrative: it is the end of a surface. The daemon assigns its journal ordinal
(`nextClaimObservationOrdinalTx`, after everything applied so far), which also
keeps the schema's `UNIQUE (gate_occurrence_id, event_ordinal)` index satisfied
without a migration.

Idempotency is unchanged in substance and simpler in mechanism: the journal's
own primary key. One observation id applies exactly once, and a replay answers
`duplicate`. The ordered path's `rejected` outcome — recorded ordinal disagrees
with the replayed frame's — is meaningless for a kind whose ordinal the daemon
assigns, so `checkClaimObservationReplayTx` does not compute it; keeping it
would have reported a conflict for every honest retry.

`TestClaimObservationRolledOverAuthReturnedIsStillStale` pins the other half:
the fence still rejects a delayed `auth_returned` from a closed-out cycle,
which is the case it exists for.

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

## Two tab-accumulation models, both disproved (2026-08-22)

"Why do I have multiple tabs with the same paper" was investigated after the
download fix landed, and the first two explanations were both wrong. Recording
them because each one is a plausible reading of this code that a future pass
will reach for again.

**Model 1: `recordManagedTab` orphans the outgoing tab.** It does overwrite
`job.tab_id` without retiring anything (`extension/src/background.ts`,
`recordManagedTab`). But by the time a later cycle mints a replacement, the
drive timeout has already detached the job (`tab_id: -1`, in
`registerHandoffDrive`'s timeout), so a retirement keyed on the job's own
outgoing `tab_id` is dead code. A fix built on this model was written and
reverted.

**Model 2: the sweep cannot see the orphan.** Also wrong, in the useful
direction: `reconcileOwnedTabs` skips a ledgered surface only when
`owner?.tab_id === tabID` and it is not a cold park, so an orphan whose owner
names a different tab is *already* eligible. Making the sweep run promptly on
replacement was written and reverted too, because the replacement never
happens: reuse keys on the ledger as well as on `job.tab_id`
(`findManagedTab`'s tracked-id match, fed by `ledgerOwnedTabID`), so a
detached-but-ledgered surface is recovered rather than replaced. That is now
pinned by `a re-offer for a job detached from its live tab reuses that surface`
in `extension/test/background.test.ts`.

**What was actually true.** Measured live: fifteen drive cycles for one paper
across one morning left TWO tabs in the group, not fifteen. The accumulation
the report described was one tab per *failed* cycle, and the cycles repeated
only because the paper could never complete — the null-erasing serialization
boundary in the effect executor. Fixing that removed the repetition, and with
it the pile. The lesson is the ordering one: a symptom counted per-cycle is a
symptom of the cycle repeating, so establish why it repeats before building
machinery to clean up after it.

**Residue, not yet addressed.** A settled institutional scaffold tab can
survive: `closeOwnedTab` refuses a tab that is `active`, deliberately, and the
scaffold is often the focused tab at exactly the moment its claim settles.
`reconcileOwnedTabs`' own rule for an active tab is "ambiguity retains, it does
not cede". So the spent scaffold is retained indefinitely rather than closed,
and the designed answer for a surface papio will not close on its own is
`orphanTabStatus`/`cleanupOrphanTabs` — whether a spent scaffold should reach
that surface, or be navigated to the paper it was minted for, is a product
decision and is deliberately left open here.
