# ADR-0031: Host-aware daemon fetch suppression

Status: Accepted (2026-09-27). The operator asked to implement it after review.
The review's required amendments R1–R11 and optional O1–O3 are adopted; the
optional allowlist R12 is rejected (see Alternatives). Two refinements were
added after the offline replay over the operator's store showed the reviewed
rule would have skipped two daemon fetches that succeeded (see Measurement):
a minimum wall span, and the probe as one pass.

## Problem

The daemon keeps fetching from hosts that never serve it a file. Measured on
a read-only backup of the live store (1027 jobs, 2026-07-13–2026-09-26): hosts
with zero validated accepts and at least three 403s absorb 181 of 689 daemon
fetch attempts, 26.3% of the daemon's fetch work. The heavy hosts are Wiley
(23 of 25 attempts 403), MDPI (24 of 24), ScienceDirect (18 of 30), `doi.org`
(17 of 39), BMJ Open (12 of 12), Oxford Academic (10 of 10), Taylor & Francis
(8 of 8), and SAGE (7 of 8). ACM (17 of 34 × 403) is not a zero-accept host: it
has one validated accept, and it is the host the reviewed rule would have
wrongly skipped. `www.igi-global.com` burned 98 attempts, all 429 with zero
accepts: a rate-limit sink, not a 403 sink. Contrast arXiv (21 of 28
accepted), Springer (13 of 15), and Nature (12 of 14): suppression must never
touch hosts that serve.

46 jobs that later reached `ready` or `imported` had a daemon 403 before the
browser route succeeded — 24% of browser-route successes — with a median
46.5 hours between the first 403 and success. That lag is an upper bound (it
includes retry waits, queue time, and human latency); its floor is the doomed
fetch attempts themselves.

Before this decision no host-level suppression existed: `route_suppressions`
holds 25 rows, all `no_entitlement`, keyed per job by
`internal/job/institutional_evidence.go:RouteSuppressionKey` — institutional
route fencing, not host suppression. Per-source budget deferral
(`internal/app/retry_policy.go:observeCandidateTemporary`,
`internal/app/retry_policy.go:observeBudgetRefusal`, and the source budgets'
`Budgets.Defer`) paces resolver sources, not publisher hosts. And the store
could not name the cause: every failed fetch persisted only the Go type name
`*fetch.Error`, so a challenge and a plain 403 were identical rows.

## Decision

Suppress the daemon's wire attempt — never the candidate — on hosts the
daemon has learned do not serve it, and pace hosts that rate-limit it.

**What the store records (I11-obs, R2).** A failed fetch persists, in the
existing `attempts.detail` column,

    *fetch.Error(<kind>) host=<candidate host> [final=<responding host>]

`<kind>` is a closed vocabulary (`challenge`, `http-403`, `http-429`,
`http-5xx`, `http-<n>`, `not-a-pdf`, `size-limit`, `redirect`, `network`,
`timeout`, `cancelled`, `blocked`, `local`, or the fetch class) from
`internal/fetch/types.go:FailureKind`. `final=` is present exactly when a host
answered the last request hop — an HTTP status, a challenge, a non-PDF body, a
size violation — and is the host after redirects. It is absent when nothing
answered (network, timeout, policy or gate refusal): the store never names a
responder that did not refuse anything. Success rows already name the
responding host (`bytes=… mime=… host=<final>`). Legacy rows (bare
`*fetch.Error`) parse as unknown kind with no host
(`internal/fetch/types.go:ParsePersistedDetail`) and count for nothing.
No schema change, no IPC field: the detail is schemaless storage.

**Key and wall (R2, R11).** Failures count against the responding host when
known, the candidate host otherwise. `challenge` and `http-403` pool into one
wall: they are one access-control refusal, and a host with two challenges and
two 403s is walled four times. Separate kinds stay in the store for
measurement. `http-429` never counts toward a wall; no other kind does either
(they are not the host refusing). A 429 is a rate limit even when a WAF
header names a challenge beside it: the fetch classifier reads it as a
retryable 429 that keeps its `Retry-After`, and `FailureKind` gives HTTP 429
precedence over any challenge message, so a rate limit can never become a
wall. The body-marker challenge scan only runs on 2xx responses.

**Rule (R1, R5, and the span refinement).** A host is suppressed when, inside
the trailing window W, it has at least N wall rows, zero daemon fetch-stage
success rows, and its wall rows span at least S hours (earliest to latest).
Only a fetch-stage success row clears a host: validate accepts,
browser-source accepts, and adoption results never clear, because none of them
tests unauthenticated daemon access. The span requirement separates a
persistent wall from a burst of refusals by an intermittent host: on the
operator store ACM refused twelve times inside fifteen hours and then served
the daemon a PDF five seconds after a refusal. Defaults: N = 3, W = 90 days,
S = 24 hours.

**Redirectors (R3).** The check runs at every request hop inside the
downloader, beside the per-hop SSRF check (`internal/fetch/fetch.go:HostGate`,
installed with `SetHostGate`), so a redirect into a suppressed host stops
before the request reaches it. Redirectors never suppress: `doi.org` is on the
shipped exclusion list, and a host whose only rows handed the request to
another host (it never answered) is a pure redirector and never suppresses.

**Probe (R4, R9, and the pass refinement).** A suppressed host admits one
probe per P days (default 7), timed from the host's latest real attempt or
probe admission. Checking eligibility and granting the probe is one locked
step (`internal/hostsuppress/hostsuppress.go:Tracker`, `Claim`), so of any
number of concurrent jobs exactly one probes. A probe granted to a candidate
that is then refused before any request (cost ceiling, budget gate, local
error) is handed back (`Release`), so the host is not left shut for an
interval with nothing revivable. The probe passes normal budget admission and every attempt
it covers carries a `probe` marker in its detail. A failed probe is a new wall
row and re-arms the interval; a successful probe is a fetch-stage success and
clears the host. A probe is one pass: once granted for a job — on a candidate's own host or
on a redirect target the per-hop gate reaches — every other candidate of that
job on that host in the same pass runs too, and every attempt that touched
the probed host carries the marker, because on the
operator store SAGE served one source's URL for a work one second after
refusing another source's URL for the same work. Every other job still skips.
Revival is explicit: before the pending-queue loop, the daemon resets to
`pending` at most one row per host whose latest fetch attempt was
`suppressed`, whose host is owed its probe, and whose URL this pass resolved
again (`internal/app/host_suppression_wire.go:reviveSuppressionProbe`): a row
the resolvers no longer return cannot be fetched and must not shadow the live
one.
`ResetCandidates` never revives `skipped` rows, and `candidates_dedupe` keeps a
re-resolved identical URL on the skipped row, so without this the probe would
have nothing to fetch. Rows skipped for any other reason are never touched.

**Skip encoding (R8).** A skip is a fetch-stage attempt row with outcome
`suppressed` (`attempts.outcome` has no CHECK), `http_status` NULL, and detail
`*fetch.Error(wall) host=<candidate host> suppressed=<refused host>`; the
candidate becomes `skipped` (the existing status CHECK needs no migration).
The skip sits before `ReserveCost` and `Budgets.Acquire`, so it spends neither
quota nor a request; a per-hop refusal happens after reservation and releases
the reserved cost. A skip makes no retry-plan observation, so a pass of only
skips exhausts and the landing expansion, the sibling hop, and the
institutional or browser route run exactly as they do after invalid fetches.

**Open-access recovery (R7).** A skipped open-access candidate records the same
durable `job.oa_browser_hint` event (url_key only) a blocked OA fetch records
through `internal/app/app.go:isOABrowserBlocked`, and contributes its landing
seed to the landing-page expansion, exactly as a permanent failure does. The OA
browser handoff therefore survives on exactly the walled hosts where it
matters.

**Operator visibility (R10).** When a pass that skipped fetches parks or
exhausts, its transition detail carries `suppressed_hosts`: a list of
`{host, kind, count}`. The pass carries the summary in its context and every
park it can reach merges it: the retry wait, the landing-page-only park, the
browser and institutional handoffs, the unavailable exhaustion, the
document-delivery parks and waits, and the validation and identity-review
parks. `jobs.failures` grouping (state, provider, transition
reason) and `Diagnose` need no change, and no IPC struct field changes: detail
maps are additive-safe (ADR-0009).

**State (R6).** The rule lives in memory (`internal/hostsuppress/hostsuppress.go:Tracker`)
and is pure bookkeeping over attempt rows. On first use after start the
daemon rebuilds it with one bounded query over fetch-stage rows inside the
window, fed through the same `Observe` path live attempts use. No new table.

**429 pacing (O3).** A 429 never writes a suppression. It starts a per-host
backoff honoring `Retry-After` when named, otherwise doubling from one minute
per consecutive 429, capped (default 3600 s); an answer other than 429 resets
the doubling, and a daemon success ends the backoff. A candidate on a host inside its backoff is marked `retryable` and joins
the pass's retry plan without a request. The existing per-source deferral is
unchanged. Backoff state is not rebuilt at start: it is at most the cap long.

**Network context (O1).** No egress-IP, VPN, or campus dimension. The store
has no such signal, durable network identifiers carry a privacy cost, and a
wall lifted by a network change surfaces as a successful probe within P days.

**Configuration.** `[host_suppression]` in the strict-mode config, with the
shipped defaults above: `enabled`, `failure_threshold`, `window_days`,
`probe_interval_days`, `rate_limit_backoff_cap_seconds`,
`min_wall_span_hours`, `excluded_hosts` (`internal/config/config.go:HostSuppression`,
`internal/config/config.go:DefaultHostSuppression`). A config that never
mentions the section loads the defaults. Because config is strict-mode, a
config that names the section needs the binary that understands it: deploy
binary and config together.

## Alternatives rejected

- **Never-suppress allowlist (review R12).** Rejected: the zero-daemon-success
  gate already protects serving hosts, because a host that serves the daemon
  on any path has fetch-stage successes in the window and never suppresses.
  An allowlist would be a second, hand-maintained authority that goes stale;
  the span requirement and the probe pass handle the intermittent hosts the
  replay found.
- **Clear on any validated accept.** Rejected (R1): browser and adoption
  accepts prove institutional or human access, not daemon access, and would
  clear Wiley and MDPI on every institutional win.
- **Pure window decay without probes.** Rejected (R4): a suppressed host never
  fetches, so it never accepts; decay alone is a window-long ban.
- **Consecutive-failure counters.** Rejected (R5): the measured cut is a
  windowed count, and consecutive counting plus decay plus clearing described
  three rules for one decision.
- **A persisted suppression table.** Not needed: `attempts.detail` is the
  durable memory and the rebuild is one bounded query.

## Consequences

Good: daemon fetches stop going to hosts that have only ever refused it, and
jobs bound for the browser route get there without paying those attempts
first. The per-host record is operator-readable directly from
`attempts.detail`.

Bad and watched: the rule acts on a candidate host's history, and a host that
walls one path and serves another can still lose a daemon fetch until its next
probe; the span requirement and the probe pass are the measured mitigations.
Attribution of pre-change rows uses the candidate host because they recorded
no responder. With the defaults, the rule removes fewer attempts than the
hindsight cut (54 against 181 on the replay), because it must learn each host
online and probe it weekly; that is the price of zero wrong skips.

## Measurement

Offline replay (2026-09-27) of the real `Tracker` over the operator store's
fetch-stage attempts, in time order, from a read-only snapshot that was deleted
afterwards; legacy rows mapped to the kind the new daemon would have written
(403 → `http-403`, 429 → `http-429`) with the candidate host as responder.
677 of 689 rows have an end time.

| Rule | Skipped | Wrong skips (skipped a daemon success) | Probes |
|---|---|---|---|
| Reviewed rule (N=3, W=90d, P=7d) | 117 (17.3%) | 2 (ACM, SAGE) | 14 |
| + probe as one pass | 100 | 1 (ACM) | 14 |
| + minimum wall span 24 h | 70 | 1 (SAGE) | 13 |
| Both (shipped defaults) | 54 (8.0%) | 0 | 13 |

Shipped defaults skip ScienceDirect 17, MDPI 15, Wiley 14, Oxford Academic 5,
Emerald 2, JAMA 1; no skipped host served the daemon at any later point in the
data. 95 of igi-global's 98 attempts arrive inside a 429 backoff and would be
deferred (not skipped).

Live acceptance: `make live-cohort` before and after (see `dev/live-cohort.md`;
a one-work difference is noise, a cohort shift is signal), plus these store
queries. Ship criteria: the share of daemon fetches on zero-success walled
hosts falls, per-host daemon successes do not, and no job that would have
reached `ready` through a daemon fetch exhausts through suppression.

```sql
-- per-host walls, pooled kinds, new-format rows only
SELECT substr(detail, instr(detail, 'host=') + 5) AS tail, COUNT(*)
  FROM attempts WHERE stage = 'fetch'
   AND (detail LIKE '*fetch.Error(http-403)%' OR detail LIKE '*fetch.Error(challenge)%')
 GROUP BY tail ORDER BY 2 DESC;
-- skips and probes
SELECT COUNT(*) FROM attempts WHERE stage = 'fetch' AND outcome = 'suppressed';
SELECT outcome, COUNT(*) FROM attempts WHERE stage = 'fetch' AND detail LIKE '% probe' GROUP BY outcome;
-- rate limits per host
SELECT substr(detail, instr(detail, 'host=') + 5) AS tail, COUNT(*)
  FROM attempts WHERE stage = 'fetch' AND detail LIKE '*fetch.Error(http-429)%' GROUP BY tail;
```

## Implementation

`internal/hostsuppress` holds the rule (`Tracker.Observe`, `Decide`, `Claim`,
`Release`, `Observe429`, `BackedOff`). `internal/fetch` records the responder
(`FinalHost` on `Error`) and runs the per-hop gate. `internal/app`
(`host_suppression_wire.go` and `fetchCandidates`) rebuilds state, records
skips, revives probes, keeps OA recovery, and carries the exhaustion summary.
Bootstrap enables the rule from `[host_suppression]` and installs the gate on
the daemon downloader.
