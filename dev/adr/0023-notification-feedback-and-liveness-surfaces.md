# ADR-0023: Notification, feedback, and liveness surfaces

Status: Accepted (2026-08-12)

## Context

*papio* has several surfaces that expose daemon state and researcher actions, but
those surfaces have different lifetimes, scopes, and authorities. The popup is
short-lived and current-page-oriented; the inbox is a durable full-tab decision
surface; the toolbar badge is ambient and lossy; desktop notifications are
best-effort operating-system interruptions; and Activity is a durable history.
Treating these as interchangeable has produced noisy per-item notices, visual
alarm treatment for ordinary required work, and a misleading `jobs_working`
count that includes queued, scheduled, and human-blocked work.

The extension and daemon also evolve under strict, feature-negotiated protocol
compatibility. Existing result shapes and browser frames cannot be widened for
new behavior. Activity remains a solicited pull, and the daemon remains the
authority for durable work, decisions, and institutional processing. ADR-0022
sets the institutional authority, holder-generation fencing, typed human-gate,
effect-permit, and surface-budget model that this ADR must consume rather than
reproduce.

## Decisions

### 1. Give the five surfaces distinct responsibilities

The product uses five surfaces, each with one job:

| Surface | Question it answers | Persistence | Scope |
| --- | --- | --- | --- |
| Inline result / feedback strip | Did the action I just took land? | Seconds, or until dismissed for errors | One focused *papio* surface |
| Popup | What is happening now for this page and browser? | While open | Current page, direct browser unblockers, compact global pulse |
| Badge and tooltip | Is *papio* disconnected, blocked, or waiting for me? | Ambient, lossy | Highest-precedence blocker plus actionable-turn count |
| Desktop notification | Did something worth interrupting me for happen while I was elsewhere? | OS-controlled, not reliable storage | Coalesced action, milestone, integrity, or degradation event |
| Inbox and Activity | What needs a decision, what is continuing, and what happened? | Durable | Complete bounded or paginated read model |

No surface substitutes for another. A feedback strip is not a work queue, the
popup is not a miniature inbox, the badge is not a progress bar, and a desktop
notification is never the sole record of a decision or outcome.

### 2. Keep turn-taking, category, and persistence separate

`attention` is a closed turn-taking value: `working`, `required`, or `advisory`.
It answers who acts next; it is not severity. A separate closed notification
category describes what kind of news an event represents and drives routing.
Durability describes where the fact can be recovered. Thus an advisory
retraction can merit an integrity notice, a working delivery request can remain
silent while it waits, and a required manual download can be grouped with its
siblings without being rendered as an alarm.

### 3. Keep desktop notification ownership in the daemon

The daemon owns desktop notification policy and the one sender. This version
explicitly rejects adding `chrome.notifications`: it would require a new
install-time permission, vary across browser and operating-system presentation,
have a more limited Firefox option set, depend on MV3 worker/alarm lifetime for
scheduled delivery, and create channel arbitration and duplicate-notification
risks beside the daemon sender. The extension therefore does not become a
second desktop-notification owner.

Notification copy names a recoverable *papio* surface or CLI command and does
not promise a clickable action. OS duration, sound, placement, persistence, and
platform presentation are outside *papio*'s control. The durable record, badge,
and inbox remain the contract. `papio doctor` reports unsupported desktop
capability instead of implying that the platform acknowledged an attempt. A
future browser-owned or native application sender may replace, not supplement,
the daemon channel only through an explicit ADR amendment.

Browser-owned notifications may be reconsidered only when all five conditions
hold:

1. Click-through measurably reduces unresolved required actions.
2. The browser channel replaces the daemon channel for an explicitly claimed
   session rather than duplicating it.
3. Durable identity and deduplication survive worker and browser restart.
4. Permission denial has an observable recovery path.
5. Chrome, Firefox, and macOS behavior has been exercised rather than inferred.

### 4. Route typed intents through one closed notification policy

A single `internal/notify` router consumes typed notification intents after the
producer has persisted the domain fact. It preserves `event_kind` as the
webhook `event` value, unless a separately versioned webhook contract changes
it; `category` is policy metadata and never silently rewrites that field. The
intent carries `event_kind`, `category`, `aggregate_key`, `phase`,
`window_start`, optional job/batch/scan identity, `happened_at`, message, and
structured detail. Unknown producer kinds remain durable Activity/domain events
and webhook data; they are not guessed into a category, and exhaustiveness
coverage must require an explicit disposition before a new notification kind
ships.

The closed category and phase vocabulary is:

| Category | Allowed phase | Meaning |
| --- | --- | --- |
| `request_outcome` | `terminal` | Terminal result of one explicitly submitted standalone request |
| `decision_opened` | `opened` | New work whose effective turn is the researcher's |
| `decision_pending` | `reminder` | Reminder for already-known required work |
| `completion_batch` | `checkpoint` or `final` | Durable cohort checkpoint or final settlement |
| `discovery_new` | `digest` | Watch or backfill discovery |
| `integrity_notice` | `scan` | Retraction or correction affecting held work |
| `system_degraded` | `episode` | A nameable condition stops *papio* making progress |

`window_start` is never router receipt time. For `request_outcome` and
`decision_opened`, the producer floors durable `happened_at` in UTC to the
effective category coalescing interval. For `decision_pending`, it is the
persisted reminder-eligibility window. For `completion_batch`, `discovery_new`,
`integrity_notice`, and `system_degraded`, it is respectively cohort start,
watch-run start, scan start, and episode start. The producer stores this value
with the durable fact before dispatch so replay, restart, and later policy
changes preserve the same identity.

The operational ledger is distinct from Activity. Its desktop-leg identity is
the unique tuple
`(category, event_kind, aggregate_key, phase, window_start)`. A
`notification_intents` row records the intent payload and timing, count,
availability, desktop state, reservation/attempt timestamps, and independent
webhook state. The closed desktop states are `pending`, `held`, `reserved`,
`attempted`, `suppressed_presence`, `dropped_quiet`, `platform_unavailable`, and
`superseded`; `pending` and `held` are nonterminal, while the other states are
terminal, with `reserved` terminal for replay if a process dies before it can
record `attempted`. `notify.*` Activity entries audit this ledger and are not
its source of truth.

The same typed intent is passed to the application notifier, watch runner, and
retraction sentinel. Producers persist their domain fact before routing, and a
notification or webhook failure never changes acquisition, import, watch,
retraction, human-action, or delivery state. Watch-disable is a typed
`watch.disabled` `system_degraded` intent carrying watch, run, and episode
identity rather than a free-text sender call.

### 5. Use milestones, coalescing, and independent webhook policy

The default desktop preset is `milestones`. Standalone terminal outcomes may
notify, but per-paper imports inside durable cohorts do not create individual
attempts. New required turns coalesce into a group; pending reminders form a
digest retaining recovery-class counts and oldest age; cohort completion has at
most one meaningful checkpoint and final delta; watch discovery is catch-up or
digest material by default; integrity notices are capped per explicit scan; and
a degraded condition notifies once per state episode. Working progress never
creates a desktop notification, and a scheduled retry or delivery poll is not
an ETA for success.

For each due desktop leg, policy is applied in this exact order:

1. Revalidate the daemon-owned aggregate; if it is empty, resolved, or replaced
   by a later phase, record `superseded`.
2. If no platform sender exists, record `platform_unavailable`.
3. During quiet hours, record `dropped_quiet` for drop mode; hold mode remains
   nonterminal and is re-evaluated after the window.
4. If any current focused-surface lease exists, record
   `suppressed_presence`.
5. Atomically reserve one rolling-hour slot and enter `reserved`; when all six
   default slots are occupied, remain `held` until a slot is due.
6. Invoke the platform sender once and record `attempted`, whether it reports
   success or error; the platform supplies no delivery acknowledgement.

Only `reserved` and `attempted` consume the default six-reservations-per-rolling-
hour budget. A reservation is not replayed after a crash. Quiet hours and the
desktop rate ceiling apply only to the desktop leg. Webhook dispatch is
independent, immediate by default, and retains all existing structured fields;
it does not inherit desktop focus suppression, quiet-hours handling,
supersession, or rate state. Webhook event names remain the producer's
`event_kind`, not the new category.

### 6. Add a solicited, typed liveness read without live push

The daemon exposes a new feature-gated, solicited `work_pulse_v1` request/
response pair. It is read on existing popup and inbox polling cadence; it does
not add a subscription, timer, or unsolicited daemon-to-extension stream. A
schema-1 response may include exact optional counts, projection completeness,
effect and human-surface capacity, last forward/finished times, a scheduled
next action, bounded source gates, latest cohort summary, and typed
`stall_episodes`.

The five nonterminal buckets are mutually exclusive: `in_flight`, `continuing`,
`scheduled`, `waiting_required`, and `stalled`. A complete projection includes
all five and `nonterminal_total`, whose sum is exact. `continuing` means daemon-
authorized work immediately eligible for autonomous scheduling, without an
active lease, future schedule, explicit operation, or typed human gate; an
institutional candidate does not count merely because it exists in a Phase 2
materialization state. `in_flight` comes from current execution or the
authoritative effect governor, not an unexpired inactive claim. `scheduled` is
backed by a future retry, delivery poll, or source gate. `waiting_required`
counts effective explicit turns and the daemon's typed current human-gate
projection, one turn per gate/claim with dependent siblings excluded.

Typed stall episodes are the sole authority for `Stalled` pulse copy and
`system_degraded` episode notices. Each stalled item belongs to exactly one
current durable episode; an incomplete, contradictory, stale, or unavailable
projection renders Unknown rather than inferring a stall from elapsed time,
Activity prose, a materialization phase, or a speculative route. A scheduled
retry/check is a future action, not an ETA. This read model preserves ADR-0005's
rejection of live push and its pull-loop reopen triggers; `work_pulse_v1` is a
solicited read, not an exception to that decision.

### 7. Amend the badge to count effective turns

The ADR-0001 badge precedence remains: disconnected/broken `!`, browser-local
blockers and permissions, actionable turns, then blank. The numeric tier changes
from broad `pending_total` inventory to the exact daemon-owned count of effective
`required` turns. Watch hits, retractions, dependent sibling papers, and working
rows do not inflate that number.

The researcher may choose a browser-local count mode: **Decisions waiting**
(default), **Everything pending** (legacy-style inventory), or **No number**.
The choice never hides a disconnected or blocker `!`. An older daemon falls
back honestly to `pending items`; if the required-turn projection is incomplete
or exceeds its bounded list, the badge does not invent a precise number.

### 8. Make Activity pageable and watermark-aware

Activity remains a durable, solicited pull. A negotiated Activity page contract
uses a hard page size of 50 and supports `before_seq` plus the caller's prior
`seen_through_seq`; the response includes entries, `has_more`, the next cursor,
authoritative `latest_seq`, and either exact `new_count_since` or `gap=true`.
The extension persists `activity_seen_through_seq` as browser-profile read state,
not daemon authority.

After a visible Activity tab successfully renders the newest page, it advances
the watermark through that page's `latest_seq`, acknowledging all durable
Activity entries through that sequence, including older entries not individually
paged or viewed. The old watermark is retained until that visible render
completes. A `Since you were last here` divider and exact `Activity (N new)`
label appear only when `gap=false`; with a gap, the UI says newer history is
available without claiming an exact count. Activity remains ordered,
repeat-collapsed, recoverable, and quiet during polling; it is not a live
announcement stream.

### 9. Use privacy-minimal focused-surface presence

The extension may send a feature-gated `surface_presence_v1` hint containing
only an opaque surface lease `instance_id`, `surface` (`popup` or `inbox`), a
`focused` boolean, and a timestamp. It contains no URL, title, tab ID, host,
identifier, or page content. The hint is holder-independent and
non-authoritative. The daemon tracks each instance independently and suppresses
a desktop attempt while any focused lease was received within 120 seconds.
Daemon receipt time controls expiry; the client timestamp is diagnostic and is
bounded so a future browser clock cannot extend a lease. Missing or rejected
acknowledgements do not block a surface poll, and a hidden/closed hint is only
best effort.

### 10. Make browser and CLI batches durable cohorts

A browser or CLI batch has durable cohort authority rather than deriving a
cohort from timestamps, telemetry, or a shared consumer label. The strict
feature-gated `page_bulk_cohort_v2` protocol carries a caller-owned `cohort_id`,
ordered manifest total, chunk index/final flag, source metadata, and up to 50
canonical keys per chunk. The daemon persists acquisition batches, chunks, and
members; `page_bulk_runs` remains aggregate telemetry only. Browser origins are
validated bare lowercase HTTPS origins and no path, query, fragment, title, or
bearer value becomes durable source data.

For v2, `request_id` is caller-owned. The first attempt mints and persists it
before sending; every replay of an unresolved chunk reuses it. Daemon idempotency
is `(cohort_id, chunk_index)`: matching replays return the identical cached
result, while a reused identity with different semantic fields returns a
structured conflict before domain mutation. Chunks are accepted strictly in
sequence, and duplicate keys, gaps, invalid final flags, or incorrect cohort
length are conflicts. Recovery persists the unresolved chunk before sending and
advances only after applying a matching result; it never silently downgrades to
v1.

Result accounting is deliberately two-level. `submitted`, `joined`,
`already_owned`, and `invalid` describe the addressed chunk and sum to that
chunk's canonical-key count. `persisted_members` and `membership` describe the
cumulative cohort state; a complete cohort is reached only after the final
chunk commits the expected unique members. Partial coverage removes the
unavailable denominator rather than reconstructing one from telemetry. Joined
jobs retain their job identity and may contribute to multiple cohort summaries
without producing duplicate per-job notification attempts.

### 11. Respect the institutional-processing authority boundary

ADR-0022 owns institutional orchestration, holder generations, durable
candidates and materialization claims, effect permits, profile evidence,
authentication ownership, typed human gates, surface budgets, artifact fencing,
and automatic sibling resumption. This ADR owns how those daemon-authoritative
facts are routed and presented across notification, pulse, badge, Activity,
inbox, popup, and cohort surfaces. Neither ADR creates parallel authority.

The consumption boundary is explicit: one daemon-owned actionable row represents
a typed current gate or claim, with dependent siblings counted but not duplicated;
the daemon owns authentication-entry and human-owner identity; and the pulse
projects the daemon's exact effect and human-surface capacities rather than a
browser worker approximation. Browser-local cooldown and tab facts may enrich a
pulse only after holder-generation reconciliation and never override daemon
eligibility. `decision_opened` aggregation uses durable gate/claim identity.

Notification routing, Activity paging, inbox hierarchy, local-action feedback,
and cohort persistence may proceed against current authority. Pulse and
effect-capacity projection must consume the Phase 3 daemon governor once that
authority lands. Exact typed-gate and badge aggregation must consume Phase 4
projections rather than approximate them from Phase 2 materialization state.
Staged rollout continues to gate automatic routing; this ADR adds no parallel
claiming, first-route selection, source-gate bypass, provider-readiness claim,
or concurrency increase.

## Consequences

Positive:

- Researchers can distinguish moving, scheduled, waiting on them, idle, stalled,
  and unknown without treating color or inventory as liveness.
- Durable inbox and Activity state recover facts that a feedback strip, badge, or
  OS-controlled notification cannot reliably retain.
- Coalesced milestones replace per-paper notification chatter while preserving
  independent, structured webhook automation.
- Required work is presented as ordinary action rather than severity styling, and
  repeated homogeneous work can use daemon-authored cohort and family identity.
- Activity can recover more than one page without making every poll a live
  screen-reader announcement.
- Focused popup/inbox surfaces suppress redundant desktop attempts without
  disclosing browser page metadata.
- Batch progress has durable membership and replay identity, so partial coverage
  is explicit instead of reconstructed from telemetry.

Negative / obligations:

- Every new notification-producing event must map exhaustively to a closed
  category and phase or remain a durable non-notifying event.
- Notification ledger, webhook, Activity, pulse, cohort, and protocol contracts
  require focused cross-version and restart coverage; old peers must not receive
  unknown frames.
- Desktop notification state records platform attempts and dispositions, never a
  claim that an operating system displayed the notification.
- Pulse consumers must preserve Unknown when measurements are incomplete or
  stale, and must not turn scheduled checks into success ETAs.
- Badge and Activity semantics are intentionally lossy/read-state projections;
  the daemon remains authoritative and the browser cannot elect institutional
  owners or mutate durable authority from local observations.
- The five browser-notification revisit triggers and ADR-0005's live-push reopen
  conditions remain gates for future amendments rather than implementation work
  in this ADR.
