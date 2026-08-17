# OpenAlex spend remainders: a credit fuse, an egress invariant, and an identity audit

Salvaged from the 2026-08-14/17 OpenAlex spend work. The shipped decisions now live
in **ADR-0024** (credit egress authority) and **ADR-0025** (evidence authority over
canonical identity); git history and `CHANGELOG.md` hold the implementation record.
What remains in this file is the work that was deliberately *not* built.

**Twelve adversarial reviews, eleven rewrites.** Three early rounds sharpened a
two-part plan (a per-job spend guard plus a per-identity credit ceiling); a fourth,
given no prior-round context and a wide brief, rejected the shape of both; three
more reviewed the shipped code and found live defects in it; the eighth falsified a
premise this file had just been rebuilt around; the ninth, given the deployed code
and no prior context, found three more live defects — two of them created by the
previous rounds' own fixes — and named the two places where this plan promises what
its mechanism cannot deliver. Anything a review asserted about the provider or the
language was confirmed independently before acceptance. Every finding is verified
against the tree or the live provider before being written down. Reviews are
preserved under `dev/scratch/oracle/20260815T*`. The tenth found no defect in shipped
code — every finding was a plan claim the mechanism could not deliver. The eleventh
found three more live defects, two of them in code earlier rounds had already fixed
twice, so "a clean round" is not evidence of a clean slice. The twelfth found the
eleventh's own fix enforcing a rate limit at the wrong scope — and its literal
remedy would have reverted `b9af0e5`, so a finding being correct about the race does
not make its remedy safe.

**Rejected designs** at the bottom records every dead end with its reason —
twenty-three of the forty-four from earlier drafts of this file. Read it before
proposing a simplification; most of the obvious ones are in it, with the sequence
that breaks them.

The recurring shape of every defect found so far, mine included: **a decision made
from information that is not durable yet, or is not what it claims to be.** An
in-memory charge a crash erases; a fail-closed guard that becomes a permission
when a second caller reads it; a best-effort note treated as an authority; a cache
treated as a fact; a durable fact read by only one of its two readers.

## State 2026-08-17 — trimmed to the open remainder

Verified item-by-item against the tree on 2026-08-16, then trimmed on 2026-08-17.
Everything that shipped has moved to an ADR:

- Items 0, 1+2 and 3 → **ADR-0024**: the egress authority, the replay-free
  transport, outgoing-request identity and the bearer credential, the source-wide
  daily credit fuse with its frozen denominator and drift closure, the
  pacing/credential split, the measured yield that made the sibling hop opt-in, and
  truthful parking when a local budget is exhausted.
- Item 5 → **ADR-0025**: search evidence may create candidates but may not promote
  canonical identity; the immutable submitted-identity anchor, four-state identifier
  provenance, per-field `submitted_fields`, and the insufficient-authority
  disposition.
- Item 8 was merged into item 5 and is gone with it.

The pre-trim text is recoverable in full at
`git show 2d29e7a:dev/active/openalex-spend-remainders.md`. Cut: `Audience:
heterogeneous tiers`, `Context: the incident`, `0.`, `1+2.`, `3.`, `5.`, `8. Merged
into item 5`, `Fixed while reviewing`.

**What is left, and why each is still open:**

- **Item 4 — jitter the budget-reset wake.** Deferred by design: operational
  smoothing, not an invariant.
- **Item 6 — per-job spend guard.** Its stated condition ("deferrable only once the
  fuse is deployed") is now satisfied, so the deferral rests on its own terms rather
  than on a pending prerequisite. The residue is cross-day starvation: one job can
  consume a disproportionate share of a day's allowance without ever exceeding it.
  Lease fencing must be decided before it can be built safely.
- **Item 7 — derive the effective basis explicitly.** Not built deliberately: item 0
  measured the sibling hop as not worth its cost, so this primitive has no consumer.
  Reviving it requires the paid three-shape comparison, which spends real credits and
  is therefore the operator's call.
- **OpenAIRE self-throttle.** Measured, unscheduled, needs a live per-provider smoke
  first.

## OpenAIRE self-throttle — measured, unscheduled

Extracted from the fuse generalisation analysis; the rest of that analysis, including
why the header-derived floor does not generalise beyond `core` and `openaire`, is in
ADR-0024.

**papio's own OpenAIRE bucket refuses ~42× more often than OpenAIRE does.**
Re-measured against the operator's live store on 2026-08-16, last 7 days: `openaire`
recorded **27,375** attempts of which **26,738** ended `budget_blocked` and only
**632** succeeded — **3.77 requests per hour actually reaching the wire** against a
documented keyless ceiling of **60/hour**. So ~94% of a free allowance goes unused
while papio declines its own work, and the shape is papio's config, not the
provider's: `RatePerSec: 0.016, Burst: 1` (`internal/config/config.go:625`) spreads
permits evenly while demand arrives in bursts, so a burst of 1 refuses the second
caller in the same second and the unused permits never accumulate. `openalex` shows
the same pattern less starkly (40,060 of 48,547 blocked), but there the refusals are
partly the fuse doing its job, so OpenAIRE is the clean case. Fixing it recovers
throughput on a free tier and costs nothing to get wrong-way — raising burst spreads
bursts into an allowance already ours.

**This is not scheduled because it needs a live per-provider smoke first, and that is
the operator's call:** capture `x-ratelimit-used` before and after a burst and confirm
the used count stays under the *documented* 60/hour, not under the live `limit: 7199`
header, which is untrustworthy — it contradicts OpenAIRE's own documented ceiling. Do
not tighten or loosen pacing from that header alone.

## 4. Jitter the budget-reset wake — deferred out of this tranche

**Cut on review, and the reasoning is worth keeping:** once final-egress admission
and the token bucket are correct, a UTC-midnight wake cohort is an operational
smoothing problem, not a monetary invariant. The burn shape was caused by the
uncharged loop, not by the synchronised wake — the wake only made it visible.
Implement when telemetry actually shows scheduler or store contention at a reset
boundary. The design below is settled, so this is a scheduling decision, not
unfinished work.

Every job parked on a quota or local-budget gate becomes runnable at the same
instant, because the park time is the reset instant exactly. The token bucket
protects OpenAlex from the burst, but nothing protects the scheduler and SQLite
from a cohort waking together — and a synchronized cohort is what produced the
original 25-minute burn shape. Wake at `reset + stable_jitter(jobID)`:
deterministic, no new state, one line at the park site. **Window: 0–120 seconds**,
chosen so a several-hundred-job cohort spreads to a handful of wakes per second
while the added latency stays invisible against a day-long gate. Give the constant
a name and let tests reference it rather than picking their own.

**Jitter the job's wake time, never the underlying gate.** The gate instant is
the provider's own fact and other logic reads it; smearing the gate would corrupt
that meaning, while smearing the wake preserves it. **Scope it to quota and
local-budget reset parks**, not to every source-gate wake: an ordinary short gate
is not a synchronized cohort, and smearing those adds latency for no benefit.

## 6. Cross-day starvation — the residue the fuse does not bound (condition now satisfied)

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
is not hypothetical on this machine. It is listed sixth because the egress
authority (item 1+2) is the structural fix for its *consequence*: once every
OpenAlex wire attempt requires a fail-closed credit commit, an uncharged repeat
pass can no longer produce unbounded provider spend.

**Be precise about what the fuse actually buys, though.** It does not make a
pathological job's spend bounded over its lifetime — that job can consume the
allowance again tomorrow, and the day after, starving unrelated jobs each time.
What the fuse gives is an operator-defined **per-day blast radius**. If that is
the accepted safety invariant then deferring the per-job guard is reasonable, but
what remains deferred is a **cross-day starvation and liveness problem**, not
cosmetic fairness.

Reasons it is deferred rather than built now:

1. The fuse bounds the per-day spend consequence, which was the urgent property.
   What the per-job guard adds is that the offending job eventually dies rather
   than merely being prevented from draining today's pool.
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

Revisit when "one job may not occupy acquisition indefinitely" is adopted as a
product invariant, or on an observable concentration signal — one job's share of a
day's `credits_committed`, or repeated same-work passes visible in external logs.
**Do not rely on in-database telemetry of post-wire/pre-park failures as the
trigger**: storage failure is precisely the condition that produces those failures
*and* erases their record, so that signal is quietest exactly when it matters.
Then frame it as exactly that: a job-liveness fuse, not the remainder of the
OpenAlex incident fix. Its worked design (conditional SQL charge, progress-only
reset via `InsertCandidates`' inserted-row count, a separate
`retryProgressBasis`, prospective checking, a dedicated terminal reason, and the
mandatory regression that crossing the ceiling must **not** make `atBoundary`
true) is in git history at the third rewrite of this file.

## 7. Derive the effective basis explicitly, before novelty gating — **Not built: hop is opt-in, default off**

> Item 0 reported 2026-08-16: the fuzzy sibling hop is now gated off by default
> (`[sources.openalex].sibling_title_search = false`) because it was measured at
> ≥138 credits per accepted artifact even under the most generous possible
> attribution — and plainly did not produce all attributed artifacts. The only
> thing that could rehabilitate it is the paid three-shape comparison, which
> spends real credits and is the operator's call. Building the memo protocol
> first is pure sunk cost. **Do not build this section while the hop is gated
> off.** The design below is correct and cheap to revive if that comparison
> succeeds; it is preserved intact and unsimplified.

Two problems, one abstraction. The first is availability: a DOI-only job whose own
metadata carries no usable basis cannot search for siblings when the memo has
expired or been evicted, and silently discovers nothing. The second is worse and
is live today: **the durable marker can name a different question from the one
searched.** The app hashes `row.Work` *before* calling the adapter, and the
adapter may then substitute a positive memoized canonical record and search on
*its* title instead.

**The hashed value must include the canonical DOI, and must not be `work.Work`.**
Sibling acceptance does not depend only on title/year/authors: `ResolveSiblings`
also *excludes* the canonical DOI from its result set (`resolved.DOI ==
canonicalDOI`, `openalex.go`), so the canonical identifier is part of the question
being asked. Hashing only the textual basis suppresses a search that genuinely
changed: basis `{DOI: X, title: T, authors: A}` is marked done; canonical identity
is later legitimately corrected to DOI Z with the text unchanged; under Z the
excluded row is different and X is itself a candidate sibling, yet the hash matches
and the needed search never runs. Hashing the whole `work.Work` makes the opposite
error — an unrelated PMID or OpenAlex-ID enrichment buys a fresh 10-credit search.
So the effective basis is a dedicated immutable value carrying exactly the fields
that affect the request and the acceptance predicate: normalized search title, year,
canonicalized author-match keys, and canonical DOI. Hash that value, and pass *that
same value* into the search — do not use `work.Work` as the protocol object.

**And it must carry a search-protocol version, because item 0 is explicitly
considering changing that protocol.** The marker's whole claim is "this exact
question was already asked and paid for", and the question includes the physical
query (`search=<title>`, `per_page=10`) and the acceptance predicate (canonical-DOI
exclusion, title normalization, year bounds, surname matching) — none of which the
bibliographic fields describe. Sequence: basis B is marked complete under today's
shape; a later release moves to `per_page=100`, a title-scoped filter, or a
materially different matcher; B hashes identically and the marker suppresses a
question that has never been asked. So the hashed value carries a
`SiblingSearchProtocolVersion` constant, bumped whenever query shape or acceptance
semantics change. Since item 0 may change the query *before* item 7 lands, treat
every pre-item-7 marker as stale deliberately, rather than relying on a
serialization difference to do it by accident.

A sentinel-triggered recovery does **not** fix that, which is what an earlier
draft of this item got wrong. The damaging sequence never produces a sentinel at
all:

1. `row.Work` already has a usable title, so no sentinel is returned.
2. A positive canonical memo also exists.
3. The app hashes `row.Work` as basis A and finds no marker for A.
4. The adapter silently searches memoized basis B.
5. Basis B's search completes; the marker records **A**.
6. Later enrichment changes `row.Work` to A′; the memo still holds B.
7. A′ looks novel, and buys the identical B search again.

**The fix is to make the effective basis explicit and authoritative.** A
side-effect-free `SiblingSearchBasis(requested)` returns the basis the adapter
*would* use — positive memo metadata or caller metadata, whichever wins, subject to
one **explicit precedence rule: a positive memo wins only if it yields a usable
basis; otherwise a usable caller basis remains eligible.** (That rule is now
shipped in the adapter — wholesale replacement let a positive-but-incomplete
canonical record suppress caller authors, which was the negative-memo defect
arriving by the opposite route — so item 7 must preserve it rather than reintroduce
a plain override.) Usable means title plus a canonicalizable author surname; a
negative DOI memo suppresses only the memo-derived basis, never the caller's own.
The app hashes **that returned basis** for the novelty marker, and the subsequent
search is forced to use exactly it — no hidden substitution after the marker check.

**The result must be ternary, not `(work.Work, bool)`.** `recordFor` deliberately
returns `false` for both "never asked" and "asked, fresh negative memo", and a
two-valued basis result collapses them again — so a pass whose DOI `Resolve` just
got a 404 would immediately buy the *same* singleton back as a basis lookup. Return
`usable` / `known_no_remote_basis` / `lookup_needed` (names immaterial): a fresh
negative memo suppresses the redundant singleton without suppressing usable caller
metadata.

Only in the `lookup_needed` state does the caller, under its *own* separate
admission and credit commit, ask for a one-credit `SiblingBasis` network lookup.
That keeps three properties: each paid request is separately admitted, so a floor
installed by the singleton's own response stops the search; `ErrNoSearchBasis`
keeps meaning "no request happened"; and the marker names the question actually
asked. This item must be designed against the *current* basis semantics, which
changed after the earlier draft was written.

## Ordering

Nothing here blocks anything else, which is why this is a remainder rather than a
plan.

- **Item 6 before item 4.** Item 6 is a correctness residue; item 4 is smoothing.
- **Item 6 needs lease fencing decided first.** `InsertCandidates` performs no
  generation check, so a stale pass can insert a genuinely novel candidate and rearm
  the *current* holder's authority. A per-job cap built on top of that resets in
  situations nobody intended, so "same transaction as progress" is insufficient — it
  must be *progress from the currently authorized pass*. Either prove lease expiry
  cannot overlap live `Process` execution, or carry a pass token in every
  authoritative charge and reset.
- **Item 7 is sequenced after a paid measurement**, never before it.
- **The OpenAIRE smoke is independent of all three**, and is the cheapest.

## Rejected designs (do not re-derive)

- **Feeding provider-reported `cost-usd` into `spent_usd`.** Written into an
  earlier draft of this file as "the dormant axis can finally be fed truthfully".
  `spent_usd` is not a telemetry column: `reserve` reads it before the wire,
  refuses on `spent + cost > limit`, and increments it as monthly admission. A
  second writer either double-counts every non-zero estimate or activates an
  admission authority this plan defers. Observed USD gets its own diagnostic
  column or none.
- **A fuse that "refuses to draw down prepaid balance implicitly".** Not
  implementable from passive response headers: the local counter knows only what
  *papio* committed, the provider balance is known only after *papio* sends, and an
  OpenAlex account is shared with the web UI and any other client. Another consumer
  can exhaust the free allowance while *papio* sits under its fraction, making
  *papio*'s next admitted call the first charged to prepaid. Replaced by the
  honest bound plus a stop on observed prepaid movement.
- **Retrying or settling a failed floor write to regain availability.** Considered
  when fixing the fail-open. Rejected: a latch held until the provider's own reset
  is the safe failure mode, and retry machinery adds a second state machine whose
  own failure modes need the same analysis. Availability lost for one reset period
  is cheap; the alternative spends real money.
- **Treating the 2.3% search yield as a property of OpenAlex.** It is the yield of
  `?search=<title>&per_page=10` plus a local exact-title filter — a broad
  relevance-ranked query (title, abstract, full text) truncated at ten rows, so an
  exactly-titled record ranked eleventh is a paid miss. Compare query shapes before
  concluding the feature is uneconomic; the per-request price does not change with
  `per_page`.
- **Building item 7 before item 0 reports.** Item 7's ternary basis and re-earn
  protocol exists to make the sibling hop correctly available. If the measurement
  says delete the hop, that protocol is sunk cost. Item 0 gates nothing else, but
  it gates this.
- **Bearer authentication as a substitute for the redirect work.** Moving the
  credential to an `Authorization` header is worth doing — it removes the
  query-stripping half of the merge-`301` identity downgrade and keeps keys out of
  logged URLs — but it does not remove per-hop re-admission or re-debit, so it is
  an addition to item 1+2, not a replacement for any part of it.
- **An absolute credit ceiling as the shipped default (`daily_credit_limit =
  4000`).** Derived from the author's own keyed 10,000-credit day. Measured live,
  the keyless pool reports a limit of 1,000, so the default would have been
  **permanently inert** on the low tier — on which one 10-credit search is 1% of the
  day. A fraction of the provider-reported limit is one policy for every tier. The
  absolute form survives as an operator override and as the hard maximum.
- **"API keys are granted, not self-serve", and the keyless-predominance argument
  built on it.** Factually wrong: OpenAlex documents a free, self-serve key that
  takes about thirty seconds and raises the budget 10×. Relative policy survives on
  tier heterogeneity; the population claim does not. Recorded rather than deleted
  because the conclusion outlived its premise.
- **`DisableKeepAlives` alone as the one-physical-request boundary.** Closes HTTP/1
  connection-reuse retry only. `http.DefaultTransport` sets
  `ForceAttemptHTTP2: true`, and Go's HTTP/2 transport has its own retry loop that
  re-sends a bodyless GET; papio's `fetch` also runs its own redirect loop, so one
  admission already covers N hops today. HTTP/1-only plus no automatic redirects
  plus per-hop re-admission is the actual requirement.
- **A fraction of "the provider-reported limit" without a defined denominator.**
  The counter is source-wide but the reported limit is identity-specific, so a keyed
  10,000 establishing 5,000 followed by anonymous fallback reporting 1,000 either
  shrinks the allowance below what is already committed or stops meaning what it
  says — and a malformed `X-RateLimit-Limit: 1000000000` would *enlarge* the fuse.
  Freeze the primary identity's limit for the UTC day, monotone downward, under a
  local hard maximum.
- **A two-valued `SiblingSearchBasis(work.Work, bool)`.** `recordFor` returns
  `false` for both "never asked" and "fresh negative memo", so a two-valued result
  re-buys the singleton *papio* was just told does not exist. The result is ternary.
- **Treating the provider's published price list as authoritative.** Its pricing
  page says single-entity retrieval is *free*; the live header charges 1 credit
  ($0.0001) for exactly that request. Documented prices are telemetry; the header is
  the fact, and the drift closure stays.
- **Offering the replay-safe transport as a configuration choice.** Three options
  were drafted (disable keep-alives / wrap the dialer / accept slight
  undercounting). Handing a stranger a knob whose wrong setting produces quietly
  wrong money accounting under packet loss is not a choice worth exposing — and the
  failure is unreproducible remotely. Decided in-tree instead.
- **Re-earning a sibling search basis inside `ResolveSiblings`.** Shipped and
  reverted within the hour. One admission covered two HTTP requests, so the
  ten-credit search could go out after the singleton's own response had installed
  the quota floor; a paid singleton followed by `ErrNoSearchBasis` put a false
  "no request happened" fact into the retry plan; and the durable marker then
  named a question that was not the one searched. Item 7 is the caller-side fix.
- **`RoundTripper.RoundTrip` as the one-wire boundary.** The standard `Transport`
  may replay an idempotent request after certain connection failures, and every
  OpenAlex call is a GET — so one debit could still cover two physical requests.
  Disabling redirects is necessary and insufficient.
- **Latching the lost floor only when the durable write fails.** The write can
  block for seconds; during that window the process knows the fact while egress
  still sees no gate. Latch on the parsed header instead.
- **A process-only pricing-drift closure.** Commit 10, learn the shape cost 100,
  mark it closed, crash — only the under-sized debit survives and the operation is
  authorized again. The closure must be durable.
- **Leaving the late-refusal error contract to item 3.** `openalex.fetch` flattens
  every client error into a fresh generic `TemporaryError`, so a quota refusal
  raised at egress cannot reach the fallback logic and a credit refusal cannot
  reach the park logic. The taxonomy belongs with the egress design.
- **"Independently tied to the submitted identity" as the enrichment test.** A
  typed sibling relation is genuinely tied to work X while describing work Y. The
  test is *verified as describing the same work*.
- **Comparing validation against the mutable `row.Work`.** That is the
  self-confirming loop: the anchor must be an immutable snapshot of the submitted
  identity.
- **Verifying the echoed identity on the DOI endpoint only.** The OpenAlex-ID
  endpoint publishes at the same 1.0 confidence and had no check at all.
- **Expecting the daily credit fuse to restore the provider floor.** They are
  different boundaries: a source-wide counter answers "how much may this instance
  commit today", the floor answers "is this credential's provider balance spent".
  A committed credit satisfies the first and is silent on the second, so the
  egress authority must revalidate the outgoing identity's gate too.
- **Gating only the credential when observed cost exceeds the committed shape
  cost.** Pricing is a property of the operation, not the identity, so keyed
  detects the drift, gets gated, and the next call falls through to keyless and
  repeats the same undercharged shape.
- **A sentinel-triggered basis recovery as the fix for the marker mismatch.** The
  damaging sequence never returns the sentinel: caller metadata is usable, the app
  hashes basis A, the adapter silently searches memoized basis B, and the marker
  records A.
- **Putting the egress commit at a generic `HTTPClient.Do`.** `http.Client.Do`
  follows redirects, so one commit can authorize several physical requests. Use
  `RoundTripper.RoundTrip`, or disable redirects and treat one as a response
  needing fresh admission.
- **Letting `0 == unmetered` skip the durable commit.** Disabling enforcement
  would then also bypass the egress invariant and its accounting — exactly when an
  operator most needs the numbers.
- **A conditional UPSERT whose limit test lives only in the conflict-update arm.**
  The first write of a day then admits a 10-credit request against an allowance of
  5.
- **Item 8 as a fuzzy-sibling-specific rule.** Typed siblings and ordinary weak
  title matches use the same pre-validation merge, so the narrow rule looked
  complete while leaving two equally dangerous routes open.
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
