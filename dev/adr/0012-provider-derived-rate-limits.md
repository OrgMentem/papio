# ADR-0012: Rate limits are learned from providers, never configured by operators

Status: Accepted (2026-08-02). Governs `internal/budget` and every source
client. Records the reasoning behind `internal/sourcegate` and the
`source_openalex` doctor warning.

## Context

A 309-work cohort froze at exactly zero throughput. OpenAlex answered an
exhausted daily allowance with `Retry-After` pointing at the next UTC midnight,
`budget.Acquire` slept that out inside an acquisition worker, and the worker's
lease heartbeat kept the job claimed for the whole window. That bug is fixed
elsewhere. What it exposed is the subject here: **papio did not know what its
own limits were, and had no way to find out except by hitting them.**

Three facts emerged while diagnosing it, none of which papio could have stated
beforehand.

**The limit was not what anyone believed.** The operator's consumer measured
1000 requests a day and recorded multi-day cohort acquisition as a property of
the design. It is not: 1000 is OpenAlex's *anonymous* allowance. A free account
is roughly ten times that, and the `api_key` field papio already had was empty.
A real limit was measured against an unconfigured client and nearly written down
as a fact about the world.

**The quota is shared with consumers papio has never heard of.** It is scoped to
the machine, not to papio. Two probes an hour apart read `x-ratelimit-remaining`
of 3 and then 0 while papio was provably idle — zero attempts, zero runnable
jobs. The credits went to the consumer's own test suite, which had been calling
OpenAlex on every unit run since it was written. The operator's browser hit the
same wall in the same hour. papio cannot enumerate its co-consumers, so it
cannot compute its own headroom from its own records however carefully it
counts.

**The provider states the answer on every response.** `x-ratelimit-limit`,
`x-ratelimit-remaining`, `x-ratelimit-reset`, and for OpenAlex's credit model
`x-ratelimit-cost-usd` and `x-ratelimit-limit-usd`. Measured directly: the
anonymous pool read `remaining: 0` while an account key on the same machine at
the same moment read `remaining: 9998`. The account is a *separate* budget, not
a larger one — a distinction no configuration file could have expressed and
which decided how a stalled cohort was unblocked.

## Decision

**A source's rate limit is whatever the provider says it is, on the response in
front of us. papio does not accept a rate limit as operator configuration.**

Concretely:

- **Credentials are configuration; limits are not.** `sources.<name>.api_key`
  and `email` stay, because they are facts about the operator that no provider
  can tell us. `max_requests_per_day` and its relatives are rejected.
- **`Retry-After` is authoritative, within a bound.** `budget.Defer` persists
  what the server asks and never shortens an existing gate — but never honours
  one beyond `budget.MaxDeferHorizon` (24h), because every real quota resets
  within a day and anything longer is a clock skew, a malformed header or a bug
  that would otherwise park a source until someone edited the database.
- **Local pacing is a courtesy, not a limit.** `rate_per_sec` and `burst` stay
  configurable: they express how hard papio is willing to lean on a provider,
  which is an operator's decision. They are not claims about what the provider
  permits.
- **Every provider call is accounted for, or the rest is unusable.**
  `internal/sourcegate` binds each source client to its budget so discovery's
  requests are reserved and paced like the resolvers'. This is a precondition
  rather than a peer of the rest: papio's counter previously omitted an unknown
  amount of its own traffic, and a headroom figure derived from a counter that
  is wrong is worse than no figure at all.
- **Absent capacity is reported, not assumed.** `papio doctor` warns when
  `sources.openalex.api_key` is unset rather than passing cleanly, naming the
  roughly tenfold difference. Passing read as fully configured, and that is
  precisely how the gap survived.

## Why not a configured limit

The obvious alternative is `max_requests_per_day` in `config.toml`. It was
considered and rejected on four grounds, in ascending order of importance.

**It couples deployment.** Config decoding is strict, so a new `[sources.*]`
field makes an older binary reject the whole file. Config and binary must then
ship together — a real operational cost for a value the provider already sends.

**It cannot express the shape.** OpenAlex meters credits with per-request costs,
a USD ceiling, and a prepaid balance. A single integer models none of that, and
the model changed under us mid-incident.

**It cannot see the other consumers.** The number an operator would write is
papio's share of a machine-wide budget. Nobody can compute that share, because
it depends on a test suite, a browser and whatever else runs on the host.

**And decisively: it records a belief, not a fact.** A configured limit encodes
what someone once understood about a provider. That understanding is exactly
what proved wrong here, and it changed twice within one day in both directions —
1000 measured against an unkeyed client, then 10000 on an account, then 0 when a
co-consumer spent it. A header-derived limit self-corrects when the answer
changes. A configured one institutionalises the error and makes it durable,
authoritative-looking, and wrong.

This is the general form of a mistake the operator's consumer made and named:
measuring a limit against an unconfigured client and nearly recording it as a
property of the design.

## Consequences

papio learns a source's limits only by talking to it, so headroom is unknown
until the first call of a session and after any credential change. That is
accepted: an unknown that resolves on first contact is honest, where a
configured number is confidently wrong at exactly the moments that matter.

A stale gate is now possible in one narrow way — a credential change can
invalidate a persisted `next_allowed_at` set under the old identity, which
happened here when an account key arrived mid-incident. `MaxDeferHorizon` bounds
the damage to a day. There is deliberately no automatic re-probe: papio does not
second-guess a `Retry-After` it was given.

Local pacing remaining configurable means an operator can still be more
conservative than a provider requires, and cannot be less.

## What this does not decide

Whether papio *surfaces* learned headroom, and in what shape. A `doctor` line is
a gauge for whoever happens to look; the first external consumer, asked
directly, said it would not poll a credits figure and would not schedule a
marking loop against another system's accounting — but would use a **submit-time
preflight**: one answer, before a batch is committed, of the form "N works, M
credits, expect to park at roughly X%". Expectation-setting at the moment a
decision is made rather than a number to monitor. That is a separate decision
with a separate consumer contract, and adding it to a ratified result is
forbidden by ADR-0009 regardless.

Whether papio should proactively `Defer` on `x-ratelimit-remaining: 0` before a
429 arrives is also unsettled. It is cheap and strictly better than discovering
the wall, but it is papio-internal behaviour rather than a consumer contract,
and it earns its keep only once the accounting above is trusted.
