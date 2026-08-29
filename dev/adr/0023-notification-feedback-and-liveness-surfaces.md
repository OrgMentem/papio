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

### 1. Give the six surfaces distinct responsibilities

The product uses six surfaces, each with one job:

| Surface | Question it answers | Persistence | Scope |
| --- | --- | --- | --- |
| Inline result / feedback strip | Did the action I just took land? | Seconds, or until dismissed for errors | One focused *papio* surface |
| Popup | What is happening now for this page and browser? | While open | Current page, direct browser unblockers, compact global pulse |
| Host-page action acknowledgement | Did the popup action I just requested enter *papio*? | Three seconds | The exact bound active page |
| Badge and tooltip | Is *papio* disconnected, blocked, or waiting for me? | Ambient, lossy | Highest-precedence blocker plus actionable-turn count |
| Desktop notification | Did something worth interrupting me for happen while I was elsewhere? | OS-controlled, not reliable storage | Coalesced action, milestone, integrity, or degradation event |
| Inbox and Activity | What needs a decision, what is continuing, and what happened? | Durable | Complete bounded or paginated read model |

Amended 2026-08-28: a seventh surface was added, the loss toast. See Decision 12
below; this table and every rule under it are otherwise unchanged.

No surface substitutes for another. A feedback strip is not a work queue, the
popup is not a miniature inbox, the badge is not a progress bar, and a desktop
notification is never the sole record of a decision or outcome.

The host-page action acknowledgement is a noninteractive, browser-local
projection of popup action acceptance, and nothing more. It exists because the
popup closes on click, so its own inline result can disappear before the
researcher reads it, leaving a deliberate action with no visible receipt on the
page the researcher is still looking at. It is bounded accordingly: it carries
one closed short label for one accepted action, persists for three seconds, and
is scoped to the exact bound active page whose tab ID and byte-identical tab URL
were validated for that action.

It never carries progress, errors, identifiers, titles, URLs, provider names,
job IDs, daemon prose, or background events. It is emitted only for a validated
successful response to an action the researcher just requested, never for a
failure, a later job transition, or an event that arrived on its own. It obeys
the existing `SuccessAckMode` preference and appears only under `all`. It is
never a substitute for popup inline feedback or for durable Activity and job
state, both of which remain authoritative; if injection is refused on a viewer
or privileged page, the inline popup result stands alone and no broader
permission or content script is requested.

This surface does not change Decision 3: the daemon remains the sole owner of
desktop notification policy, and the rejection of browser notification channels
stands. A page-local acknowledgement inside the tab the researcher already
granted for the action is not a notification channel, gains no new install-time
permission, and creates no second sender to arbitrate.

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

## Addendum (2026-08-12): nonterminal waiting is not the decision count

Live UI validation exposed a scope distinction needed to read Decision 6
alongside the counts-v3 badge contract. `waiting_required` is a bucket in the
complete nonterminal projection: it counts only nonterminal work blocked on the
researcher, so it excludes an open action whose parent job has already reached
a terminal state. `turns_required` is the exact daemon-owned count of
researcher-owned actionable turns and is the sole authority for any surface
that says `decisions waiting` or `need you`.

The values may therefore legitimately differ. For example, an
`openurl_available` action can remain open on an `unavailable` job; it remains
an actionable inbox row and contributes to `turns_required`, but it is not
nonterminal work and must not contribute to `waiting_required` or break the
five-bucket sum. This is a scope distinction, not an arithmetic discrepancy
to reconcile.

## Addendum (2026-08-12): action rows are ordered by family

The accepted family contract assumed the daemon's action order carried a
ranking worth protecting, and therefore required a family variant recurring
after an intervening row to render as a second block rather than move its
members together. Reading the shipped query settles it: open human actions are
selected `ORDER BY a.id ASC` in `internal/triage`, which is insertion order
with no priority, severity, rank, or attention term. Fragmenting a family
preserved no signal and defeated the feature — 37 open actions on the author's
library rendered as ten blocks, repeating one manual-download instruction four
times to save 27 repetitions the feature exists to remove.

The daemon therefore orders action rows by family: families by their earliest
member, insertion order within a family, ranks assigned after grouping. Family
identity remains the full variant tuple from the counts-v3 contract, so
`manual_download` and `manual_download_adapter_missing` stay separate families,
and a row with no mapped variant joins none — it stays standalone at its own
position and still makes the breakdown incomplete. Runs remain
maximal-contiguous, so `family_runs` now holds exactly one entry per family,
`first_rank` stays coherent with the emitted rank, and turn counts are
untouched: this is ordering, not counting. Only `human_action` rows are
affected; PDF grabs, watch hits, and retractions keep their own rank bases.

Decision 7's boundary is unchanged and is the reason this is safe: attention is
still not a sort key, and the client still preserves daemon rank exactly — the
daemon simply supplies a family-grouped rank instead of a fragmented one, so no
client reorders, groups, or promotes rows.

Accepted tradeoff: a new action joins its family block rather than appending at
the bottom, so an item can appear mid-list rather than last. Blocks themselves
do not jump, because each is keyed on its earliest member. The 128-run wire
bound stays enforced against the run count even though grouping makes it
unreachable from the closed variant vocabulary.

## Addendum (2026-08-28): a lost access surface is Activity, never a notification

A researcher who closes the tab *papio* opened to reach a paper asked to be
told about it, and asked for the telling to be proactive. Decision 5 answers
the second half: *papio* releasing a route and waiting for another attempt is
working progress, and working progress never creates a desktop notification.
That is the whole answer, and this addendum records it so the same request does
not re-open Decision 5 a third time.

Two mechanical facts confirm the notifier cannot carry this event even if
Decision 5 permitted it. The category vocabulary is closed and
`notify.validateIntent` rejects an unknown or mismatched value, and the only
near fit, `system_degraded`, means a named condition stops progress — this
reducer abandons one claim and waits for fresh arbitration, so claiming
degradation would be false. And the obvious copy overpromises: the reducer
schedules no retry, so "*papio* is retrying" would be untrue in exactly the case
that motivates it.

A third reason given here on 2026-08-28 was withdrawn on review the same day. It
said the ledger's `(category, event_kind, aggregate_key, phase, window_start)`
coalescing had nothing to group on because the observation journal stores no
episode and no count. The journal does store `gate_occurrence_id` and
`authentication_claim_id`, and `NotificationLedger.Upsert` increments a count on
its own identity, so a producer could group by claim or occurrence and derive
one. Grouping was therefore possible and the two reasons above are what decide
it. The correction is recorded rather than deleted because a false mechanical
claim is worse than a weaker true one: the next reader would have trusted it.

The event therefore travels as one durable Activity row, `browser.surface_closed`,
which Decision 8's pageable, watermark-aware Activity contract already carries
and the popup catch-up card already counts. Decision 1's surface split is
unchanged: no new channel, no new sender, no badge tier.

Two rules about honesty rather than routing, and the limits of each.

The row is written only when the observation abandoned a claim that was still
live. A successful provider outcome retires the binding before the tab
physically closes, so the trailing `owner_closed` abandons nothing; announcing a
loss there would contradict the delivery the researcher is about to receive. The
reducer reports that distinction (`ApplyClaimObservationResult.SurfaceLost`)
rather than letting the caller infer it from the event kind, because the kind
alone cannot tell the two apart. That silence depends on the outcome path having
retired the binding first, and that path is best-effort: it skips retirement when
the browser generation is unavailable and logs its own errors while continuing.
When it is skipped, a trailing `owner_closed` finds a live claim and reports the
loss it genuinely observed. The gate is therefore exact about what the reducer
saw, not about what the provider eventually delivered.

The row names the job that OWNS the observed binding, not the job that sent the
observation. A dependent paper's missing-tab repair reports a dead surface for
the binding owned by the paper actually signing in, so attributing the row to the
sender filed a stranger's loss against a paper that never had a surface — and
left the paper that did lose one silent, since Activity reads join on that job
id. `ApplyClaimObservationResult.OwnerJobID` carries the reducer's own
resolution to the caller.

The write is best-effort and stays outside the reducer's transaction, for the
single-writer reason Decision 11's ordering already forces. So a committed
observation whose append fails leaves no row and cannot be recovered: the journal
has recorded the observation, so a replay is answered `duplicate`. The failure
mode is a missing row, never a duplicated or wrong one. Any copy promising that
every lost surface is legible must therefore be read as best-effort, and this
ADR does not claim more.

## Addendum (2026-08-28): popup density rulings, salvaged from a retired plan

These were reviewer rulings in `popup-ux-consolidated-2026-08-24`, a plan file
deleted when its last section shipped. They constrain future popup work, so they
belong here rather than in commit archaeology. Recorded on review after the
deletion, which is the failure this addendum also documents: git history is the
archive for *narrative*, never for a rule someone must not re-litigate.

**The count badge is not droppable.** Removing the header count in favour of a
card was rejected. The count rides the inbox control, and Decision 7's
`ToolbarCountMode` — `required`, `all`, `off` — remains the researcher's control
over it. A researcher who chose **No number** must not get a number back by a
different route.

**`#popup-pulse-review` is rejected on density.** A labelled 32px button beside a
glyph that already carries the same count and the same route is duplication in a
380px lens. The header control keeps the route; the button does not return.

**The header count and the toolbar badge gate differently, deliberately.** The
badge gates on `required_turns_complete`, because an incomplete projection must
not invent a precise number for a surface with no other content
(`extension/src/background.ts`'s `computeBadge`). The header count does not gate
on it, because that flag describes the per-ITEM projection list: past its cap the
daemon drops the list and keeps the count exact, so gating hid a number it had
(`extension/src/popup.ts`'s `deriveInboxCount`). The cap renders `999+` and keeps
the exact figure in the accessible name. These are two different questions about
one field; a future change that unifies them will break one of the two surfaces.

**The pulse card hides only when it has nothing else to say.** It also produces a
`next` line and a capacity line, so hiding it whenever a count exists would lose
guidance the count does not carry.

## Decision 12 (2026-08-28): a seventh surface — the interruption that carries an action

Decision 1's table has six surfaces and none of them can offer a researcher a
choice about something that just happened without being asked. The inline strip
and the popup require the researcher to already be looking. The badge carries no
action. Activity and the inbox are durable, correct, and silent. And the desktop
notification is the daemon's, deliberately, with Decision 3 and Decision 5
keeping working progress out of it.

So a request that arrived three times — a toast in the browser offering to undo a
tab close — was answered three times with a surface that could not do it, and
the third answer was an OS notification the operator had not asked for. The gap
was never conservatism about permissions. It was that no surface in the table had
the shape.

**The seventh surface is an in-browser toast for a loss *papio* observed on a tab
it opened itself, carrying exactly one take-back-control action.**

| Surface | Question it answers | Persistence | Scope |
| --- | --- | --- | --- |
| Loss toast | Something *papio* was driving just went away — do you want it back? | Eight seconds, re-armed once on focus, dismissable | One observed loss, one action |

Delivery is an unfocused extension window (`chrome.windows.create` with
`type: "popup"`, `focused: false`). That needs no new manifest permission and no
host permission, so it reaches the researcher on any page — which an injected
in-page toast cannot, because provider hosts are `optional_host_permissions` and
*papio* only ever requests the exact configured resolver origin.

### Addendum (2026-08-30): the in-page route, decided

The in-page variant reserved above is now **allowed, as a second delivery route
for the same surface**, and the reservation is discharged. It is not an eighth
surface: the copy, the single action, the eight seconds, and the one-at-a-time
bound are the same, and `extension/src/toast-view.ts` remains the one place the
copy lives so the two routes cannot say different things.

The relaxation of Decision 1 that this needed is narrow and is stated here
rather than left implied. Decision 1 scopes the page-local surface to a page the
researcher just acted on, because that surface is a receipt for their own
action. This route is not a receipt, so the scoping rule it must satisfy instead
is **the researcher's own standing choice**, expressed as two separate consents
that are deliberately not collapsed into one:

- The **all-sites host grant** (`https://*/*`, already declared in
  `optional_host_permissions` and already offered as its own options control)
  answers whether *papio* may reach an arbitrary page. A per-provider grant is
  explicitly **not** sufficient: it was given so *papio* could complete a
  download on that host, not so *papio* could draw on it.
- The **`papio_in_page_toast_v1` preference**, off by default, answers whether
  *papio*'s own interruption may appear there. Revoking the grant clears the
  preference, so re-granting all-sites access later cannot silently restore an
  injected surface.

Five further bounds, all test-enforced:

- **Never into a surface *papio* owns.** A tab with a live job row, or an entry
  in the durable tab ledger, is refused — the second check is what covers the
  tab whose loss raised this very toast, whose ledger entry outlives its job.
  Drawing on a provider page *papio* is about to classify would also put
  *papio*'s own words into the `body.innerText` its adapters read.
- **HTTPS pages only, never a PDF.** The grant covers exactly that scheme.
- **Refusal falls back to the window, never to silence.** A withdrawn grant, a
  privileged page, or a tab that navigated mid-call still reports the loss.
- **Authorization is a one-use token bound to the job and the tab.** The
  injected toast's sender is the researcher's own page, so `sender.url` cannot
  authorize it the way it authorizes the toast page; a separate message type
  keeps `isToastSender` unrelaxed. The tab binding is what makes a superseded
  toast inert, since *papio* cannot reach into a tab it no longer targets to
  remove one — that toast expires on its own within the eight seconds.
- **Invisible to a page capture.** The toast renders in a shadow root, which
  `outerHTML` omits, and `capturePage` additionally strips both papio host ids
  from its own copy of the document. A fixture is evidence about a provider's
  page; a stray `papio-…` element in committed bytes is *papio* describing
  itself. Working from a copy is what keeps a capture from cancelling an offer
  the researcher is reading.

What is still **not** decided here: a persistent banner on *papio*-owned tabs.
That needs `scripting.registerContentScripts`, which no part of the tree uses
and which ADR-0019 Decision 1 names in its own scope; it would also be present
in every capture and in every `innerText` read rather than for eight seconds.

Bounds, all enforced by tests rather than convention:

1. One toast at a time. A second loss replaces the first; the producer holds the
   single pending offer, so the surface cannot become a stack of windows.
2. Eight seconds, and expiry commits **nothing**. The recovery stays in the
   inbox, so the window is a shortcut and not a timed decision — which is what
   keeps it clear of WCAG 2.2.1. The inbox undo bar's six seconds is the
   comparison; this is the longer of the two.

   Amended 2026-08-29, from a live measurement: the clock restarts when the
   window is brought forward, **once**. On macOS the first click on an
   unfocused window is spent activating it and never reaches the button
   underneath, so a researcher who noticed the toast at seven seconds would
   have lost the offer between their two clicks. The bound is therefore eight
   seconds from arrival, or eight from being brought forward, whichever is
   later — and the re-arm is deliberately once, because a window cycled in and
   out of the foreground must not be able to live forever. An offer that never
   lapses is a decision *papio* is still holding.
3. One action plus a dismissal. No progress, no error text, and no identifier,
   title, URL, provider name, or job id in anything rendered. The job id travels
   in the extension's own message only, exactly as Decision 1 already requires of
   the host-page acknowledgement.
4. It is raised only for a loss *papio* itself observed on a tab it opened, and
   only after the deliberate-close and classify-retry returns in
   `onTabRemoved`: *papio* closing its own tab is housekeeping, not a loss, and
   must never interrupt.
5. Suppressed while a *papio* surface holds focus, from Decision 9's presence
   hint. The suppression is age-bounded and resolves to *not focused* when stale,
   because a popup that closed while the worker slept never reports
   `focused: false` — an unbounded record would silence this surface for ever.
6. The offered action is truthful per branch. A resumable route offers `Reopen
   now`. An abandoned institutional claim offers `Open a new sign-in tab`,
   because `owner_closed` retires the authentication-entry lease and consumes the
   one-use close authorization: there is no reversal to undo, and a button
   claiming otherwise would lie. A loss that cost nothing — a download still
   correlated, an `awaiting_download` park the daemon adopts — raises no toast at
   all.

This does not change Decision 3 or Decision 5. No desktop notification is
created, no second notification sender exists, and the daemon remains the sole
owner of OS-level interruption policy. It also does not change the 2026-08-28
addendum above: the durable record of a lost access surface is still the Activity
row, and this surface is an offer layered on top of it, never its replacement. If
the window fails to open, the Activity row and the inbox are unchanged.

The action reuses `handoff_link_request`, the route `papio actions open` already
mints, so there is no new wire message and no protocol change. That boundary is
deliberate: the extension cannot mint a route itself, because an offer that
opened a tab by itself is what *papio* must never do for a paper it asked a human
to fetch.

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
