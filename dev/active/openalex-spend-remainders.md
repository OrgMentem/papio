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

## 0. First: measure the yield of the thing we are budgeting

Before building any ceiling, compute:

```
accepted artifacts uniquely attributable to OpenAlex title.search
-----------------------------------------------------------------
                title.search credits spent
```

for **both** the fuzzy sibling hop and metadata enrichment. The 2.3% is a
*result* rate; the number that matters is the *acquisition* rate — how many of
those 73 hits produced a validated, imported PDF that nothing else would have
found. `attempts` (source, stage, detail) joined to job outcomes carries enough
to estimate it, and `job.sibling_search` markers now date every search.

If the acquisition yield is as weak as the result rate, then narrowing
eligibility or defaulting the fuzzy search off removes most remaining spend with
**no new state machinery at all**, and items 2–3 become smaller. A ceiling makes
a poor operation stop at 3,000 credits instead of 10,000; it does not make it
worth buying. This measurement is cheap and it gates how much of the rest is
justified.

## 1. An egress invariant, not an audit checklist

The two independently-exploitable spend paths in the incident existed because
provider calls happened outside anything that could bound them. Five
`AcquireAny` sites in `internal/app` cover today's resolver, sibling, and enrich
paths, but `enrichDOIWork` reaches the discovery backend, and `internal/discovery`
and `internal/enrich` hold their own `sourcegate` clients — that is how the
*reactive* header floor reaches them and says nothing about *admission*. A code
audit fixes today's tree; it does not stop the next OpenAlex call added outside
the audited sites.

**Enforce it at the HTTP boundary instead:** an OpenAlex request may not leave
`sourcegate` unless it carries a valid credit-shape/admission marker for the
identity it will be served under. A missing marker fails *before* `Do`, loudly.
Then add a test per HTTP-producing path proving it reaches that guard.

This is the highest-leverage structural change in this document and it is worth
more than anything in the deferred per-job guard: it converts "we checked" into
"it cannot happen".

## 2. One conservative source-wide credit fuse

**Not** a reservation ledger. The goal is that *papio* provably stops making
requests, not that its books balance to the credit.

- **One durable counter, `credits_committed`, per source per UTC day**, debited
  atomically **before** admission by the request shape's conservative cost (1
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

## 4. Jitter the post-reset wake

Every job parked on a quota gate becomes runnable at the same instant, because
the park time is the provider's reset instant exactly. The token bucket protects
OpenAlex from the burst, but nothing protects the scheduler and SQLite from a
cohort waking together — and a synchronized cohort is what produced the original
25-minute burn shape. Wake at `reset + stable_jitter(jobID)` over a modest
window: deterministic, no new state, one line at the park site.

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

## 6. Deferred: the per-job no-progress guard (former A″)

A real but narrower hole exists: `Process` crash recovery rewinds to `resolving`,
and a pass can fail on durable post-wire state updates (`FillWorkMetadata`,
`InsertCandidates`) before any `retry_wait` transition exists — so
wire → credits spent → post-wire failure → no retry event → recovery → wire
again. Nothing charges that.

**Deferred, not rejected**, for four reasons:

1. The credit fuse (item 2) already bounds its *spend* consequence, which was the
   urgent property. What the per-job guard adds is that the offending job
   eventually dies rather than merely being prevented from draining the pool —
   liveness and fairness, not spend safety.
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

## Ordering

**0 (measure) → 1 (egress invariant) → 2 (credit fuse) → 3 (truthful park) → 4
(jitter) → 5 (identity audit, in parallel and arguably first) → 6 (deferred).**

Item 0 is cheap and can shrink 2. Item 1 is the structural win. Items 2–4 are one
migration and one new admission input between them. Item 5 is a different kind of
risk and should not queue behind spend work; it is the one whose failure mode is
unrecoverable.

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

## Rejected designs (do not re-derive)

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
