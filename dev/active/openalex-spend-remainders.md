# OpenAlex spend remainders: a credit fuse, an egress invariant, and an identity audit

Salvaged from the 2026-08-14/15 OpenAlex spend work (shipped and deployed:
charging mixed passes, the record memo, `select=`, the header floor, the
anonymous fallback, the fuzzy-search boundary/basis gate, and three fail-closed
guards). Those changes are complete; git history and `CHANGELOG.md` hold the
record.

**Four adversarial reviews, four rewrites.** Three rounds sharpened a two-part
plan (a per-job spend guard "A″" plus a per-identity credit ceiling "B″"); a
fourth, deliberately given no prior-round context and a wide brief, rejected the
shape of both and is the basis for this version. Its judgement:

> The proposed plan currently spends too much engineering on making spend
> bookkeeping exact and too little on deciding whether the expensive operation is
> worth buying at all.

Reviews are preserved under `dev/scratch/oracle/20260815T0*`. **Rejected
designs** at the bottom records every dead end with its reason — nine of them
from earlier drafts of this file. Read it before proposing a simplification.

## Context: the incident, and what production says now

*papio* burned 10,000 OpenAlex credits in 25 minutes because a job whose
candidates are all dead re-ran the resolver chain every cadence forever: the pass
was classified `source_gate` (uncharged) whenever any unrelated source was gated,
so the bounded retry budget never bound.

That is fixed, and the first post-fix reset confirms it: the cohort ran ten
minutes, decayed `83 → 26 → 0` attempts/minute, and settled with zero jobs left
in `retry_wait` and no future `retry_at`. **The liveness problem is solved.**

What remains is economics, and the measurements are decisive:

- OpenAlex prices `GET /works/{doi}` at **1 credit** and `filter=title.search:`
  at **10** (`x-ratelimit-credits-used`, measured live).
- That post-fix run: 664 attempts, **3,393 credits**. 304 were title searches and
  280 found nothing.
- Lifetime: **73 of 3,165 title searches (2.3%) returned anything at all.**
- Keyed and keyless are separately metered pools (`limit: 10000` vs `1000`).

So the remaining exposure is not an unbounded loop; it is a legal, terminating
workload buying a 10-credit operation that fails 97.7% of the time, with no
aggregate ceiling of any kind.

## 0. In parallel: estimate the yield of the thing we are budgeting

This **does not gate items 1–2** — a post-commit review of the shipped code
established that the egress boundary and the fuse are the missing crash and
storage-failure safety boundary, not merely economics for a low-yield
operation. Measure alongside, and let the result decide whether the fuzzy search
stays enabled or gets narrowed, never whether *papio* needs a hard OpenAlex
egress ceiling.

Estimate, for both the fuzzy sibling hop and metadata enrichment:

```
accepted artifacts attributable to OpenAlex title.search
--------------------------------------------------------
              title.search credits spent
```

The 2.3% is a *result* rate; what matters is the *acquisition* rate — how many
of those 73 hits produced a validated, imported PDF nothing else would have
found.

**Treat the number as a lower-bound estimate, not a computation.** Two reasons,
both structural: `job.sibling_search` is written post-wire and is lossy under
the crash window described in item 2, and `FinishAttempt` is best-effort
everywhere. And "attributable" needs winner/candidate provenance — if the
ledger does not record which candidate won, do not manufacture causation from a
title-search attempt happening to precede a ready job.

## 1+2. One egress authority: commit the credit at the wire, or do not go

These were two items until a post-commit review showed they are one boundary.
A reusable admission marker minted back in `AcquireAny` does not solve the
problem, because it is still separated from egress by a step that can block —
and it could authorize more than one `Do`.

**The defect this closes, in the shipped code.** Two live holes, both found in
review:

1. **The quota floor is not an egress barrier.** `Observer.observe` logs and
   continues when `Defer` fails, so valid low-quota headers followed by a failed
   write leave no durable `_quota` row and the next job spends again. On a
   full disk that runs straight through the supposed 5% stop until some later
   write succeeds or the provider itself refuses.
2. **TOCTOU between admission and the wire.** `AcquireAny` consults `quotaGate`
   *before* an ordinary `Acquire` that may itself wait up to `MaxInlineWait`.
   Workers A…N all pass the quota precheck while it is open, queue in `Acquire`,
   another in-flight response installs the floor, and the queued workers then
   send anyway. The code's comment promises "no new admission once the gate is
   visible"; the implementation does not deliver it. With a synchronized cohort
   the 5% headroom is covering not just requests already on the wire but every
   worker that passed the precheck.

**The shape that fixes both.** Do all cheap validation and ordinary local gating
first — token bucket, advisory throttle, applicability. Then, at the final
OpenAlex HTTP boundary, **atomically commit the request's conservative credit
cost and immediately call the network client.** That commit *is* the egress
authority. No blocking wait may occur after it, a failed commit means no wire,
and a request without a commit must fail before `Do`, loudly. Do not fix the
race with a manager-wide mutex: a decision taken before a blocking step cannot
be authoritative for egress.

Then add a test per HTTP-producing path proving it reaches that boundary — the
audit alone fixes today's tree and not the next call added outside it.

**Debit placement matters for availability.** Committing before ordinary local
admission would let a reset cohort burn a large slice of the daily allowance on
requests that never leave the machine. After local admission, immediately before
egress: `ErrNoSearchBasis` and `ErrNotApplicable` then cost zero with no
settlement machinery at all.

### The fuse itself

**Not** a reservation ledger. The goal is that *papio* provably stops making
requests, not that its books balance to the credit.

- **One durable counter, `credits_committed`, per source per UTC day**, committed
  atomically at the egress boundary by the request shape's conservative cost (1
  for a singleton, 10 for a search), refused when the debit would exceed the
  configured daily allowance. **Never refunded within the window.**
- **Source-wide, not per identity.** A limit enforced per `(source, identity)`
  means a configured 4,000 actually authorizes 4,000 keyed *plus* 4,000 keyless —
  an operator misreading that weakens the cap's whole meaning. The
  provider-reported floor already protects each real pool separately and
  per-identity; *papio*'s own budget should answer the different question: **how
  many OpenAlex credits may this instance commit today, across all identities?**
  Then `AcquireAny` can still fall through to the keyless identity when the keyed
  one is provider-gated, while the instance-wide allowance keeps bounding the
  total.
- **Availability is the safe failure mode.** A crash leaves the debit. A
  transport error leaves the debit. A path that declines after admission
  (`ErrNoSearchBasis`, `ErrNotApplicable`) may leave the debit. Each costs a
  little availability inside one UTC day and clears at rollover; none can
  overspend. This is what removes the reservation handle, one-shot
  `Confirm`/`Release`, aggregate-reservation correlation, the settlement
  transaction, adapter preflight, and nearly all of the cross-midnight problem.
- **Call it `committed`, not `spent`,** because that is what it is.
- **State the invariant truthfully.** The fuse bounds credits *papio*
  **authorizes**; it cannot bound what the provider actually bills, because a
  request admitted at 23:59:59 may be accounted in either window, and a shape
  that costs more than its conservative charge has already happened by the time
  anyone knows. If observed `credits-used` ever exceeds the committed shape cost,
  treat it as provider-contract drift: gate that identity for the rest of the
  window and surface it. Do not build reconciliation to recover a few
  conservatively overcounted credits.
- **Credits are a new unit, not the existing `estimatedCost`.** That argument
  flows to `reserve(ctx, source, identity, policy.MaxCostUSD, estimatedCost)` and
  into `spent_usd`: it is dollars. Using it for credits implements the
  credits-as-dollars design this document rejects.
- **Atomic in SQL, not read-compare-write.** Hundreds of jobs target the same hot
  row; it must be one conditional mutation.
- **Config:** a per-source daily credit allowance. `0` means unmetered, matching
  the existing convention — so the shipped OpenAlex default and `papio init`
  output must be **non-zero**, with a test on the defaults, or the fuse ships
  inert. Document it in the hand-authored
  `docs/reference/config-reference.md`. Strict config decoding means the field
  and the binary deploy together.
- One migration; three hardcoded `user_version` assertions
  (`internal/cli/clean_install_test.go` twice, `internal/doctor/doctor_test.go`,
  `internal/store/migrate_forward_test.go`).

## 3. Park truthfully when a local budget is exhausted

`resolve` converts `budget.ErrExceeded` into `continue` without recording any
gate fact (app.go:539-542), unlike `ErrDeferred`; enrichment does the same. So
with every source exceeded and no candidates, resolution reaches ordinary
`no_legal_candidates` — reporting "no copies exist" when the truth is "out of
budget until the window resets". Dormant today only because monetary reservations
are all `0.0`; the fuse makes it live.

**A local policy budget being exhausted is not evidence that no legal candidate
exists.** Credit exhaustion must carry its UTC reset instant and become a durable
park, and the pre-existing monetary `ErrExceeded` path must be fixed in the same
change rather than leaving two dispositions for one condition.

## 4. Jitter the budget-reset wake

Every job parked on a quota or local-budget gate becomes runnable at the same
instant, because the park time is the reset instant exactly. The token bucket
protects OpenAlex from the burst, but nothing protects the scheduler and SQLite
from a cohort waking together — and a synchronized cohort is what produced the
original 25-minute burn shape. Wake at `reset + stable_jitter(jobID)` over a
modest window: deterministic, no new state, one line at the park site.

**Scope it to quota and local-budget reset parks specifically**, not to every
source-gate wake. An ordinary short gate is not a synchronized cohort, and
smearing those wakes adds latency for no benefit.

## 5. The identity-integrity audit (higher consequence than any of the above)

*papio*'s worst outcome is not a spent quota; it is the wrong PDF filed under the
right citation. The fresh review flagged two authority boundaries that deserve a
dedicated pass **before** more spend machinery:

- **Enrichment can add a strong identifier to `row.Work` from a fuzzy title
  match**, after which acquisition operates on the enriched identity. That is
  safe only if the originally submitted identity remains an immutable validation
  anchor. If semantic validation asks merely "does this PDF match the *now
  enriched* `row.Work`?", a false positive becomes self-confirming: wrong title
  match → wrong DOI adopted → resolvers fetch that DOI → the PDF "agrees".
- **`fillMissing` accumulates metadata across ranked candidates.** Candidates are
  each conflict-checked against the *pre-merge* `row.Work`, so unless
  `fillMissing` enforces cross-candidate consistency, a sparse request lets
  candidate A contribute identifier X and candidate B contribute identifier Y
  while A and B disagree with each other.
- **Amplifier:** a DOI cache hit verifies the artifact hash and transitions
  straight to `ready`, with no visible semantic revalidation — correct only if
  the cached artifact carries a strong exact validation attestation. Otherwise
  one bad identity acceptance is reusable forever.

**Invariant to establish:** enrichment may add search and routing evidence, but
only immutable request evidence plus independently validated evidence may
authorize final artifact identity; and cache reuse must consume a durable
identity attestation, not merely `DOI → SHA256`. Audit `conflicts`,
`fillMissing`, `sameWork`, `validateCandidate`, and `FindArtifactByDOI` against
it. `make identity-corpus` measures wrong-accepts against the real library and
must be run before and after any change here.

## 6. A live defect now; deferrable only once the fuse is deployed

`Process` crash recovery rewinds to `resolving`, and a pass can fail on durable
post-wire state updates (`FillWorkMetadata`, `InsertCandidates`,
`ResetCandidates`) before any `retry_wait` transition exists — so
wire → credits spent → post-wire failure → no retry event → recovery → the same
provider call authorized again. `retryBudgetExhausted` cannot contain it,
because its count comes from persisted event history that this sequence never
writes. The charging information is only an in-memory `retryPlan` until the job
reaches its durable retry transition.

**This is a current shipped spend-safety defect, not a future fairness
concern** — the post-commit review was explicit about that, and storage failure
is not hypothetical on this machine. It is listed sixth because the egress fuse
(item 1+2) is the structural fix for its *consequence*: once every OpenAlex wire
attempt requires a fail-closed daily credit commit, an uncharged repeat pass can
no longer produce unbounded provider spend.

**Only after that fuse is deployed** does the remainder become a per-job
liveness and fairness property, and only then is deferring the per-job guard
reasonable. Reasons it is deferred rather than built now:

1. The fuse bounds the spend consequence, which was the urgent property. What
   the per-job guard adds is that the offending job eventually dies rather than
   merely being prevented from draining the pool.
2. It never actually delivers a hard per-job bound anyway: novel candidate
   insertion and material metadata changes reset the episode, so a source
   producing continuing low-value novelty keeps a job alive indefinitely.
3. It carries a migration, a new terminal reason, transactionally coupled reset
   semantics, adapter preflight changes, and grace-gate provenance — a large
   surface for a fairness property.
4. **Lease fencing is unresolved.** The store has `lease_owner` /
   `lease_expires_at`, but `InsertCandidates` takes only a job ID and performs no
   generation check. If a resolver call can outlive its lease, a stale pass can
   insert a genuinely novel candidate and rearm the *current* holder's authority —
   or consume it. "Same transaction as progress" is insufficient; it must be
   *progress from the currently authorized pass*. Either prove lease expiry
   cannot overlap live `Process` execution, or carry a pass token in every
   authoritative charge and reset.

Revisit only if telemetry shows repeated post-wire/pre-park failures, or if "one
job may not occupy acquisition indefinitely" is adopted as a product invariant.
Then frame it as exactly that: a job-liveness fuse, not the remainder of the
OpenAlex incident fix. Its worked design (conditional SQL charge, progress-only
reset via `InsertCandidates`' inserted-row count, a separate
`retryProgressBasis`, prospective checking, a dedicated terminal reason, and the
mandatory regression that crossing the ceiling must **not** make `atBoundary`
true) is in git history at the third rewrite of this file.

## 7. Two-step sibling basis, admitted and charged per call

The availability gap left open by the reverted re-earn: a DOI-only job whose own
metadata carries no title cannot search for siblings when the memo has expired or
been evicted, and silently discovers nothing. The adapter cannot fix this, which
is what the revert established — a resolver that pays for a second request inside
one admission breaks both the "no request happened" sentinel and the
one-admission-one-wire rule.

**The caller owns it.** `app.resolveSiblings` should treat `ErrNoSearchBasis` as
"this adapter needs a basis" and then, under its *own* separate admission and
charge, ask the adapter for one — a `SiblingBasis(ctx, work) (work.Work, error)`
capability that performs exactly the one-credit singleton lookup — before
re-admitting and calling `ResolveSiblings` with the enriched work. Three
properties fall out for free: each paid request is separately admitted, so a
quota floor installed by the singleton's own response stops the search; the
sentinel keeps meaning "no request happened"; and the durable basis marker is
computed by the app from the work it actually passed, so it names the question
that was really asked. That last point is a defect in the shipped marker
today — the app hashes `row.Work` while the adapter may search a memoized
canonical title — and this is its fix.

## 8. Do not enrich the canonical work from an unvalidated fuzzy sibling

A fuzzy sibling is accepted on normalized-title plus surname/year heuristics at
`IdentityConfidence: 0.6`, and it deliberately names a *different* DOI. But back
in `resolve`, every ranked candidate feeds `fillMissing`, and the merged result is
persisted by `FillWorkMetadata` **before** fetch and semantic validation — even
though PDF semantic identity is supposed to be the acceptance gate for exactly
these candidates.

Sequence: citation is DOI X with a sparse bibliography; canonical X establishes a
common title; fuzzy result Y shares that normalized title, a surname, and a
nearby year but is another work; Y's title/year/authors are persisted alongside
DOI X before Y's PDF proves anything. Even when validation later rejects Y, the
job is already bibliographically contaminated — and a validator that trusts the
enriched work is then less independent, which is the self-confirming loop item 5
describes.

**Rule:** only DOI-verified canonical metadata may enrich the work pre-validation.
Fuzzy-sibling metadata may inform candidate ranking and nothing else until its
artifact passes identity validation. Audit `fillMissing`, `conflicts`, and
`FillWorkMetadata` against that, and run `make identity-corpus` before and after.

## Ordering

**`(0 estimate ‖ 5 identity audit ‖ 8 no fuzzy pre-enrichment)` alongside
`(1+2 egress authority)` → 3 truthful park → 4 jitter → 7 two-step basis → 6
(deferrable only once 1+2 is deployed).**

An earlier draft wrote `… → 4 → 5` while its own prose said item 5 must not queue
behind spend work; the two contradicted each other and the ordering above is the
honest one. Item 0 informs whether the fuzzy search stays enabled but gates
nothing. Items 1+2 are one boundary and one migration. Items 5 and 8 are the same
class of risk — the only failure mode here that cannot be undone — and 8 is small
and self-contained, so it should not wait. Item 7 restores an availability
property and is cheapest once 1+2 has settled how admission is expressed, because
it needs two admissions per hop.

Each migration means daemon and CLI deploy together (`make dev-deploy`), which on
this machine means both *papio* binaries plus the native-host symlink.

Genuinely deferred beyond all of this: wiring the dormant **monthly USD** axis
for real dollars. A billing feature, not a safety mechanism.

## Fixed while reviewing (already shipped)

- **`siblingSearchRecorded` failed open on an unreadable marker.** `Jobs.Events`
  decodes each detail with `_ = json.Unmarshal(...)` and yields a nil map on
  failure, so a corrupt `job.sibling_search` row left the basis `""`, matched
  nothing, and bought another ten-credit search — precisely when storage was
  already misbehaving. A marker of the right kind now counts as proof a search
  happened; only a legible, different basis is evidence the question is new.
  Regression: `TestUnreadableSiblingMarkerFailsClosed`.
- **The `atBoundary` comment over-claimed.** A pass held back only by a durable
  source gate has no temporary failure, so it satisfies the first arm and the
  search may run although that gated source will get another opportunity. The
  behaviour is deliberate (it matches `siblingHop`'s caller and the per-basis
  marker bounds it to one search per question), but the comment now says exactly
  that instead of implying necessity. **Open question for item 0's measurement:**
  whether a pending durable gate should also defer the search.
- **An unreadable retry history was also a positive spend permit.** Exhaustion
  is read in two opposite senses: for liveness "unknown" must mean stop, but
  `resolve` also reads it as one arm of `atBoundary`, which is what *authorizes*
  the ten-credit search. So a single transient `Jobs.Events` failure made
  exhaustion read true, the separate marker read then succeeded and found no
  marker, and the expensive query ran with a temporary retry still pending.
  Split into `retryBudgetExhausted` (fails closed, liveness) and
  `retryBudgetExhaustedProven` (returns the read error; only a proven fact is a
  permit). Regression:
  `TestUnreadableHistoryIsNoPermitForTheExpensiveSearch`.
- **The memo made cache residency part of acquisition correctness — half fixed,
  half reverted.** At the cap, `writeMemo` dropped the entire map, and a DOI-only
  job whose caller metadata has no title depends on that memo for its search
  basis, so unrelated traffic crossing the cap silently cancelled that job's
  sibling discovery. The cap now evicts expired entries first and drops
  wholesale only if that frees nothing (`TestMemoCapEvictsOnlyExpiredEntries`),
  and a fresh negative memo is now consulted instead of being reported like an
  absent one, so a DOI the provider does not know costs nothing to re-ask inside
  the TTL (`TestSiblingHonoursAFreshNegativeMemo`).

  **The one-credit basis re-earn was reverted.** It closed the availability gap
  and opened three worse holes, all found in review: it broke the
  `ErrNoSearchBasis` contract (the caller reads that sentinel as "the adapter
  made no request at all" and skips charging, so a paid singleton followed by
  the sentinel put a false fact in the retry plan — the exact class of defect
  this whole change set exists to remove); it let one `AcquireAny` admission
  cover two HTTP requests, so the ten-credit search could go out *after* the
  singleton's own response had installed the quota floor; and it made the
  durable basis marker name a question that was not the one searched, because
  the app hashes `row.Work` while the adapter searched the canonical record's
  title. **The gap is real and remains open**; the fix belongs to the caller,
  which can admit and charge two calls separately — see item 7 below.
- **A misrouted record could still be accepted and cached.** `echoesDOI` took the
  first parseable identity and stopped, so a record whose `doi` named the
  requested work while `ids.doi` named a different one passed. It now requires at
  least one parseable DOI and demands that *every* parseable echo equal the
  requested one, rejecting internally inconsistent records rather than trusting
  field order — and it is applied before the memo write, so an unverified record
  never becomes a later pass's search basis. Regressions:
  `TestEchoedDOIRejectsInconsistentIdentities`,
  `TestMisroutedRecordIsNeitherPublishedNorMemoized`.
- **An illegible transition detail silently shrank the retry budget's evidence.**
  `retryBudgetExhaustedProven` type-asserted the detail map and treated a nil
  result as a transition that does not count, so corrupt history read as *more*
  budget remaining. It now returns an error, which the fail-closed liveness
  wrapper turns into "stop" while the permit path correctly declines to treat it
  as proof.

## Rejected designs (do not re-derive)

- **Re-earning a sibling search basis inside `ResolveSiblings`.** Shipped and
  reverted within the hour. One admission covered two HTTP requests, so the
  ten-credit search could go out after the singleton's own response had installed
  the quota floor; a paid singleton followed by `ErrNoSearchBasis` put a false
  "no request happened" fact into the retry plan; and the durable marker then
  named a question that was not the one searched. Item 7 is the caller-side fix.
- **Accepting a record on the first parseable DOI echo.** A record naming the
  requested work in `doi` and a different one in `ids.doi` passed. Every parseable
  echo must agree.
- **Re-basing `retryBudgetExhausted` on a lifetime `PaidAttempts(jobID)` count.**
  Wrong dimension (58–1,470 attempt rows per settled job, worst 4,949), and it
  inverts the sibling gate: that predicate is what *permits* the ten-credit
  search, so exhausting the budget would unlock it.
- **Treating a NULL attempt `outcome` as proof of spend.** `StartAttempt`
  precedes admission and `FinishAttempt` errors are ignored, so NULL means
  unknown.
- **Counting a generic forward state transition as progress.** `retry_wait ->
  resolving` (332×/day) and `resolving -> fetching` (231×) fire every pass; the
  counter would reset every pass and never bind.
- **A single last-sibling-basis column replacing the event set.** Breaks A→B→A: a
  scalar buys basis A a second ten-credit search.
- **A second per-job `lifetime_pass_charges` ceiling.** No evidence supports a
  defensible N, and a pass count is loosely coupled to spend once passes contain
  differently priced calls.
- **A reservation/settlement ledger with one-shot `Confirm`/`Release`.**
  Over-engineered for a safety ceiling: a never-refunded pre-admission debit
  gives the same guarantee, and treating availability as the safe failure mode
  removes correlation, settlement, cross-midnight, and preflight problems
  wholesale.
- **Retryable settlement against aggregate columns.** Two settlements of one
  handle silently consume a different in-flight reservation.
- **Enforcing the daily allowance per `(source, identity)`.** Doubles the
  configured cap's real meaning across keyed and keyless; the provider floor
  already protects each pool per identity.
- **Charging a per-job episode after the adapter returns** so post-call no-wire
  sentinels can be honoured. That is exactly the wire→crash→uncharged gap the
  guard existed to close.
- **Putting credit shape into `Acquire`'s existing `estimatedCost`.** It is
  dollars, and flows to `spent_usd`.
- **Making OpenAlex's `source_budgets` window daily.** One `window_start` also
  governs the request counter and the monthly USD counter.
- **A rolling time window as an episode reset.** Time alone re-authorizes a
  permanently stuck job — the original defect on a timer.
- **Restricting the fuzzy search to paywalled canonical records.** Premature; the
  hop can rescue an OA record whose advertised locations are unusable. Item 0's
  measurement is the right way to decide its fate.
- **`LatestGate` → earliest gate.** With one post-exhaustion wait available,
  waking at the earliest gate starves the latest-gated source of the call the
  grace rule exists to give it.
- **Merging the two identifier normalizers.** `internal/work` is
  version-preserving (acquisition), `internal/ownership` version-collapsing
  ("same work?").
