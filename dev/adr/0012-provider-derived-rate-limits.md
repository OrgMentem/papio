# ADR-0012: Provider capacity is observed at runtime; operator policy may only narrow it

Status: **Draft — not accepted.** Blocked on the scoping change in "Blockers"
below. An earlier revision of this file was briefly committed as `Accepted` by
an over-broad `git add`; it was rejected in review and the reasoning it carried
should not be relied on. See "What review corrected" for what changed and why.

## Context

A 309-work cohort froze at exactly zero throughput. OpenAlex answered an
exhausted daily allowance with `Retry-After` pointing at the next UTC midnight,
`budget.Acquire` slept that out inside an acquisition worker, and the worker's
lease heartbeat kept the job claimed for the whole window. That bug is fixed
elsewhere. What it exposed is the subject here: **papio did not know what its
own limits were, and had no way to find out except by hitting them.**

Three things were measured during the incident. All are reproducible; all
constrain any design that follows.

**The limit was not what anyone believed.** The first external consumer measured
1000 requests/day and recorded multi-day cohort acquisition as a property of the
design. 1000 is OpenAlex's *anonymous* allowance; an account is roughly ten
times that, and papio's existing `api_key` field was empty. A real limit was
measured against an unconfigured client and nearly written down as a fact about
the world.

**The quota is shared with consumers papio cannot enumerate.** Two probes an
hour apart read `x-ratelimit-remaining` of 3 and then 0 while papio was provably
idle — zero attempts, zero runnable jobs. The credits went to the consumer's own
test suite, which had been calling OpenAlex on every unit run since it was
written. The operator's browser hit the same wall the same hour.

**One source exposes multiple independent quota identities.** Measured at the
same instant on the same machine: the anonymous pool read `remaining: 0` while
an account key read `remaining: 9998`. An account is a *separate* budget, not a
larger one.

## The invariant

**Operator configuration must never be treated as evidence of current provider
headroom.**

That is what the incident supports, and it is narrower than "operators may not
configure limits". Configured values may constrain admission; they may never be
represented, surfaced, or used as provider capacity. For comparable dimensions
the effective gate is the intersection of provider capacity, operator policy,
and a conservative fallback when capacity is unknown.

Four concepts, not two:

| Concept | Examples | Authority |
| --- | --- | --- |
| Quota identity | anonymous IP pool, API-key account, institutional account | selected by credentials and request context |
| Provider observation | `remaining`, `reset`, request cost, `Retry-After` | provider response or status endpoint |
| Operator policy | pacing, concurrency, allocated share, maximum spend | configuration |
| Local accounting | attempted calls, estimated cost, papio-attributed spend | papio |

A configured 2 rps means "papio will not exceed 2 rps", never "the provider
permits 2 rps". `max_cost_usd` is operator *authorisation* — neither a
credential nor provider capacity — and ADR-0010 already classifies it that way.
The existence of a prepaid balance is not permission to spend it.

## Blockers

This ADR cannot be accepted while any of these hold.

**Quota state is keyed by source name alone.** `budget.Manager` buckets and the
`source_budgets` row are addressed by source, but the incident proved one source
has several quota identities. Every learned field — remaining, reset, cost,
`Retry-After`, local attribution — would be mis-scoped identically. Observations
need something equivalent to `(source, quota_identity, provider_bucket)`, where
the stored identity is a non-secret fingerprint, never the credential. Adding
learned state before fixing this only makes the wrong answer more durable.

**Nothing currently learns.** `budget.Snapshot` exposes a local request count, a
local spend estimate, a monthly window and `next_allowed_at`. There is no
learned limit, remaining, reset, cost, or observation timestamp.
`sourcegate.Reserver` exposes only `Acquire`; the client reserves before
forwarding and returns the response untouched, so it cannot observe anything. It
implements the accounting half only. Discovery converts any non-2xx into a
generic error and sets no durable gate, so a 429 there is currently invisible.

**Accounting is not structurally complete.** Only discovery clients are gated;
resolvers and the Crossref enricher receive the bare shared client and reserve
at the logical call level. `sourcegate.New` returns the unwrapped client when
the reserver is nil, which is a silent bypass of the very invariant the package
exists to hold.

## What review corrected

Recorded because the errors are instructive, not to pad the file.

*"papio does not accept a rate limit as operator configuration"* was too strong,
and the `rate_per_sec`-is-"courtesy" carve-out does not survive contact: a token
bucket that delays requests is a rate limit whatever it is called. Legitimate
operator-known constraints exist — a contractual ceiling, a share of an
institutional account, a provider that publishes no headers, Crossref's separate
concurrency limit, GitHub's undisclosed secondary limits.

*"Every real quota resets within a day"*, used to justify a 24-hour clamp, is
exactly the kind of provider folklore this ADR condemns, derived from one
incident. HTTP places no maximum on `Retry-After`, and manual blocks, sanctions
and monthly allocations need not reset at midnight. The constant can survive as
a **local safety policy** — papio will not let one unverified server value make
a source unusable without another observation — but not as a fact about
providers. `MaxTrustedRetryAfter` is the honest name, and clamping necessarily
implies a bounded probe at the horizon, so "no automatic re-probe" was
self-contradictory: the real choice is between a silent retry and an observable
one.

*"Absent capacity is reported"* overstated the doctor check, which detects an
empty key string. It does not verify the credential or query capacity.

## Deferred deliberately

How learned capacity is *surfaced*. Widening a ratified result is forbidden by
ADR-0009, so any external contract needs a new method regardless. Asked
directly, the first external consumer said it would not poll a credits figure
and would not schedule a marking loop against another system's accounting, but
would use a **submit-time preflight**: one answer before a batch is committed —
"N works, M credits, expect to park at roughly X%". Expectation-setting at the
moment of decision rather than a gauge to monitor. Separate ADR.

Whether `remaining: 0` should trigger a proactive defer is *not* safely
deferred as a generic rule: OpenAlex exposes daily and prepaid pools separately
and the prepaid pool can fund calls after the daily allowance is spent. A
generic zero-means-blocked rule in shared code would be overconfident. The
decision belongs to each source adapter, which alone knows its provider's
zero and reset semantics.
