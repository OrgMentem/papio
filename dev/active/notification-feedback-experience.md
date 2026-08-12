# Notification, feedback, and liveness experience

Status: **Active implementation plan; requires a new ADR before protocol or badge changes.**

This plan covers *papio*'s daemon-owned desktop notifications, in-surface feedback,
full-page inbox and Activity presentation, browser-action popup, toolbar badge,
batch/liveness reporting, and the settings that govern interruption. It is a
handoff document, not a statement that the target behaviour has shipped.

The governing product promise is:

> *papio* works without asking whenever it has authority to continue. It makes
> progress and deliberate waiting legible, interrupts only when a researcher can
> usefully act or a consequential milestone is worth surfacing, and keeps every
> transient message recoverable in the inbox or Activity.

This plan preserves the accepted boundaries in ADR-0001 and ADR-0013: the daemon
is authoritative, the inbox is the durable full-tab surface, the popup remains a
launcher/current-page lens, browser-only facts remain browser-local, and Activity
remains a solicited bounded pull. It does **not** revive ADR-0005's rejected live
push/subscription mechanism.

## Current truth

The design starts from current code rather than a new event system imagined in
isolation.

- `internal/notify/notify.go` has a best-effort macOS `osascript` sender, a
  process-local 60-second coalescer, and exactly three application methods:
  `HumanAction`, `HumanActionReminder`, and `Imported`.
- `internal/bootstrap/bootstrap.go` gives the coalescer to `app.Service`, but
  gives the raw fanout to watch and retraction producers. Watch and retraction
  notices therefore do not share the human-action/import coalescing policy.
- `internal/app/action_reminder.go` durably records reminder cadence, applies
  per-action exponential backoff capped at 24 hours, stops volunteering after
  quiescence, groups six recovery classes, and may emit one notification per due
  class in a maintenance pass.
- `[notify]` in `internal/config/config.go` contains only `enabled`,
  `webhook_url`, and `webhook_secret`; local desktop notification is enabled by
  default. The webhook is independent and structured, but best effort.
- The extension has no `notifications` manifest permission. `background.ts`
  owns a toolbar badge and title, not an OS notification channel.
- `popup.ts` refreshes its browser-local/current-page state every five seconds.
  It contains persistent inline status regions and one five-second fading
  “sign-in released work” notice; it has no general toast host.
- `inbox.ts` polls counts every fifteen seconds only while visible and not in the
  middle of a decision, refreshes on return, keeps visible row-level operation
  results, announces operations through `#operation-status`, and gives
  dismissals a six-second deferred-commit undo bar.
- `#activity-list` is a polled `role="log" aria-live="polite"` capped at 50
  entries. A large run can refill it every poll and make it unusably chatty for
  screen-reader users. The browser wire has no Activity cursor.
- `TriageCounts.jobs_working` is not liveness. It includes queued work,
  `awaiting_human`, and `retry_wait`; it can say “working” when all work is
  blocked or scheduled for later.
- Real liveness facts already exist but are not projected: job leases,
  `retry_at`, delivery `next_check_at`, source-budget `next_allowed_at`, browser
  handoff-drive occupancy, provider cooldowns, and the daemon/browser worker
  limits.
- `page_bulk_submit_result.batch_id` exists, but the browser batch is not a
  durable cohort manifest linking the returned batch to its jobs. The current
  `page_bulk_runs` record is aggregate telemetry, not batch authority.
- `extension/src/inbox.html` styles `required` as a danger rail, `advisory` as a
  blue rail, and `working` as a faded row. The reviewed recommendation in
  `dev/scratch/oracle/design-review-left-accents.md` correctly identifies the
  category error: `attention` describes turn-taking, not severity.

These facts produce three user-visible defects:

1. A large autonomous run can look inert because *papio* exposes inventory and
   per-paper history but no honest pulse or next automatic action.
2. A large run can be noisy because imports, watches, retractions, and six
   reminder classes enter different notification paths with different limits.
3. Actions become visually alarm-like precisely when required work is the
   common case; repeated instructions and rails make a 39-row worklist harder,
   not easier, to use.

## Coordination boundary with institutional processing

`dev/adr/0022-institutional-processing-authority-and-enablement.md` fixes the
authority model; `dev/active/institutional-processing-acceleration.md` is the
current execution authority. Institutional Phase −1 through Phase 2 are
complete and Phase 3 is current. The daemon/extension handoff now includes
feature-negotiated, URL-free durable candidates, claims, two-party scaffold
bindings, transient route issuance, navigation acknowledgement, bounded retry,
and paginated startup reconciliation for explicit user-invoked work. Automatic
claiming, first-route selection, source-gate bypass, provider readiness, and
concurrency increases remain disabled.

Institutional processing owns orchestration, holder generations, durable
candidates/materialization claims, effect permits, profile evidence,
authentication ownership, typed human gates, surface budgets, artifact
fencing, and automatic sibling resumption. This plan owns how those facts are
routed and presented. Neither may create parallel authority.

The shared consumption contract is:

- one daemon-owned actionable row per typed current gate/claim, with dependent
  sibling count; never one sign-in action per paper;
- daemon-owned authentication-entry/human-owner identity; the extension only
  binds/focuses the physical tab and reports browser-local disposition;
- daemon-owned global effect-permit and human-surface budgets; the pulse
  projects those exact capacities rather than a separate worker/lane
  approximation;
- `decision_opened` aggregation keys use durable gate/claim identity, so
  dependent siblings cannot create duplicate notices;
- browser-local provider cooldown/tab facts may enrich the pulse only after
  holder-generation reconciliation and never override daemon eligibility; and
- staged rollout gates automatic routing. This plan adds no confirmation and
  does not delay ordinary autonomous work, but it also does not claim progress
  from an unfenced speculative route.

Implementation dependency: notification routing, Activity paging, inbox visual
hierarchy, local-action feedback, and cohort persistence may proceed now.
Pulse/effect-capacity projection must consume the Phase 3 daemon governor once
that authority lands; exact typed-gate/badge aggregation must consume Phase 4
projections rather than approximating them from current Phase 2 materialization
state.
## Decisions

### 1. Keep five surfaces with five distinct jobs

| Surface | Question it answers | Persistence | Scope |
|---|---|---:|---|
| **Inline result / toast strip** | Did the action I just took land? | Seconds, or until dismissed for errors | One focused *papio* surface |
| **Popup** | What is happening now for this page and this browser? | While open | Current page, direct browser unblockers, compact global pulse |
| **Badge + tooltip** | Is *papio* disconnected, blocked, or waiting for me? | Ambient, lossy | Highest-precedence blocker plus actionable-turn count |
| **Desktop notification** | Did something worth interrupting me for happen while I was elsewhere? | OS-controlled, not reliable storage | Coalesced action, milestone, integrity, or degradation event |
| **Inbox + Activity** | What needs a decision, what is continuing, and what happened? | Durable | Complete bounded/paginated read model |

No surface substitutes for another. A toast never becomes a work queue. The
popup never becomes a miniature inbox. The badge never becomes a progress bar.
A desktop notification never becomes the only record of a decision or outcome.

### 2. Separate turn-taking, event category, and persistence

Do not overload `attention`.

- `attention = working|required|advisory` answers **who acts next**.
- A new closed notification category answers **what kind of news this is** and
  drives routing.
- Durability answers **where the fact can be recovered**.

A retraction may be `advisory` because no immediate decision is required and
still merit an immediate integrity notification. A document-delivery request
may be `working` and remain silent even though it is important. A manual
Download action is `required` but should be coalesced with 38 siblings rather
than ping 39 times.

### 3. Desktop notification remains daemon-owned

Do not add `chrome.notifications` in this implementation.

The current extension would need a new install-time permission, browser/OS
presentation still differs, Firefox supports only the basic option set, MV3
worker lifetime makes scheduled delivery dependent on alarm wakeups, and a
second desktop sender would create a channel-arbitration and duplicate-delivery
problem. Chrome and Firefox can provide click callbacks; the current AppleScript
sender cannot. That benefit does not outweigh adding a second owner before a
single routing policy exists.

Primary references:

- Chrome notifications API:
  <https://developer.chrome.com/docs/extensions/reference/api/notifications>
- Firefox WebExtensions notifications API:
  <https://developer.mozilla.org/en-US/docs/Mozilla/Add-ons/WebExtensions/API/notifications>
- AppleScript `display notification`:
  <https://developer.apple.com/library/archive/documentation/LanguagesUtilities/Conceptual/MacAutomationScriptingGuide/DisplayNotifications.html>

Consequences:

- Notification copy always names a recoverable *papio* surface or CLI command;
  it never promises a clickable action.
- Duration, sound, placement, persistence, and delivery are OS/user controlled.
- The durable record and badge/inbox are the contract; desktop delivery remains
  best effort and non-fatal.
- `papio doctor` must stop implying that desktop delivery exists on unsupported
  platforms. A future browser-owned or native application sender may replace,
  not supplement, the daemon desktop channel after an explicit ADR amendment.

### 4. Use a single notification router

Replace the split coalescer/raw-fanout arrangement with one policy object in
`internal/notify`. The router consumes a typed notification intent; it does not
infer policy from prose or repurpose the existing webhook event name.

```text
NotificationIntent {
  event_kind          # existing producer vocabulary: watch.alert, library.retraction, ...
  category            # closed routing vocabulary below
  aggregate_key       # stable request, batch, action, scan, or state-episode identity
  phase               # closed category-specific phase below
  window_start        # replay-stable timestamp derived from durable producer fact
  job_id?
  batch_id?
  scan_id?
  happened_at
  message
  structured_detail
}
```

The producer persists `phase` and `window_start` with the durable fact before
calling the router. The closed mapping is:

| Category | Allowed phase |
|---|---|
| `request_outcome` | `terminal` |
| `decision_opened` | `opened` |
| `decision_pending` | `reminder` |
| `completion_batch` | `checkpoint` or `final` |
| `discovery_new` | `digest` |
| `integrity_notice` | `scan` |
| `system_degraded` | `episode` |

`window_start` is never router receipt time. For `request_outcome` and
`decision_opened`, it is the durable `happened_at` floored in UTC to the
effective category coalescing interval at producer time. For
`decision_pending`, it is the persisted reminder-eligibility window that made
the action due. For `completion_batch`, `discovery_new`, `integrity_notice`, and
`system_degraded`, it is respectively cohort start, watch-run start, scan start,
or episode start. The producer stores the selected value before dispatch, so a
delayed replay, daemon restart, or later policy change reuses the original
phase/window rather than creating another identity.

`event_kind` remains the value serialized as the webhook `event` field unless a
versioned webhook contract deliberately changes it. `category` is new policy
metadata and must not silently change that field. The router preserves every
existing structured webhook field, applies webhook and desktop policy
independently, and is the single object passed to:

- `app.Service.Notifier`;
- the watch runner; and
- the retraction sentinel.

Every producer records its durable domain fact before the router runs.
Notification failure never changes acquisition, import, watch, retraction,
human-action, or delivery state. Replace the current application sink's
context/text-only methods with typed job/batch/action intents. Replace the
watch-disable free-text `Sender.Send` path with a typed `watch.disabled`
`system_degraded` intent carrying watch/run/episode identity.

Held/coalesced desktop work needs an operational ledger distinct from Activity:

```text
notification_intents(
  id, aggregate_key, event_kind, category, phase, window_start, payload_json,
  first_at, last_at, count, available_at,
  desktop_state, desktop_reserved_at?, desktop_attempted_at?,
  webhook_state, webhook_attempted_at?
)
```

Desktop-leg identity is the unique tuple
`(category, event_kind, aggregate_key, phase, window_start)`. A transaction
moves exactly one due desktop leg from `pending`/`held` to `reserved` while
consuming the rolling-hour slot; platform invocation follows outside the
transaction. The closed desktop states are `pending`, `held`, `reserved`,
`attempted`, `suppressed_presence`, `dropped_quiet`,
`platform_unavailable`, and `superseded`. The last five are terminal;
`reserved` is also terminal for replay if the process dies before it can record
`attempted`. Quiet/digest accumulation updates a pending or held row durably.
These states affect only the desktop leg. `notify.*` Activity entries are audit
projections of this ledger, not the ledger itself. Webhook dispatch reads the
same typed intent but has its own immediate disposition and never inherits
desktop focus, quiet, supersession, or rate state.

The router categorizes a closed vocabulary and rejects unknown configured
category names:

| Category | Meaning | Default desktop treatment |
|---|---|---|
| `request_outcome` | Terminal result of one explicitly submitted standalone request | immediate, coalesced 60s |
| `decision_opened` | New work whose effective turn is the researcher’s | immediate, coalesced 5m |
| `decision_pending` | Reminder for already-known required work | digest, at most every 4h |
| `completion_batch` | Durable cohort checkpoint or final settlement | one checkpoint plus one meaningful final delta at most |
| `discovery_new` | Watch/backfill discovery | surface catch-up; desktop digest only |
| `integrity_notice` | Retraction/correction affecting a held work | immediate, capped once per explicit scan/run |
| `system_degraded` | A nameable condition stops *papio* from making progress | once per state episode |

Unknown producer kinds are not guessed into a category. They remain durable
Activity/domain events and webhook data, and an exhaustiveness test must force an
explicit routing disposition before a new notification-producing kind ships.

### 5. Default to milestones, not chatter

The default desktop policy is named `milestones`.

- A single requested paper becoming ready or failing after all autonomous paths
  is a useful request outcome.
- A new required turn is useful, but a large submission receives one coalesced
  notice, not one per paper or one per minute.
- Repeated pending actions enter one digest whose copy retains recovery-class
  counts and oldest age. The existing six reminder classes do not each send a
  desktop notice.
- Per-paper imports never notify. A terminal job outcome becomes
  `request_outcome` only when durable attribution proves it is a standalone
  request. Members of a durable cohort contribute to its checkpoint/final
  summary instead. Until cohort identity and job-aware terminal events land,
  suppress the old per-import desktop path rather than misclassify it.
- A durable batch receives either one final settlement summary, or one
  quiescent checkpoint after ten minutes with a two-hour maximum hold and then
  one final summary only if a meaningful outcome delta remains. A checkpoint
  says how many are still moving/scheduled/required; it never calls an active
  cohort complete.
- Watch discoveries do not interrupt by default. They appear in Watch hits,
  Activity, and the catch-up line. A retraction scan carries a stable `scan_id`
  plus an explicit aggregation/flush boundary, so all findings in one scan
  produce one integrity notice without suppressing a later scan.
- `working` progress never produces a desktop notification.
- A stall notifies only when it has a nameable cause, no future automatic action
  explains the wait, and the episode has lasted at least 30 minutes. Recovery
  rearms the episode. A provider quota until midnight or an ILL poll tomorrow is
  scheduled work, not a stall.

Global desktop budget: six notification **reservations** per rolling hour by
default. For every due desktop leg the router applies this exact order:

1. Revalidate the daemon-owned aggregate. If empty, resolved, or replaced by a
   later phase, record `superseded`.
2. If no platform sender exists, record `platform_unavailable`.
3. During quiet hours, `drop` records `dropped_quiet`; `hold` remains
   nonterminal and is re-evaluated when next due.
4. If any current focused-surface lease exists, record
   `suppressed_presence`.
5. Atomically reserve a rolling-hour slot and enter `reserved`. When all six
   slots are occupied, remain `held` until the next slot is due.
6. Invoke the sender once and record `attempted` whether it reports success or
   error; the platform offers no delivery acknowledgement.

Only `reserved` and `attempted` consume the rate budget, and one reservation
counts once across both states. Concurrent producers cannot read-count-send
past the cap, and a crash after reservation cannot replay the slot. Enqueuing a
`completion_batch` `final` intent transactionally marks every pending or held
checkpoint for the same aggregate `superseded`; a reserved or attempted
checkpoint remains immutable. After an attempted checkpoint, enqueue a final
only when its normalized public outcome/count payload, excluding timestamps
and wording, differs from that checkpoint.

Quiet hours are optional and empty by default. Researchers keep irregular hours;
a default clock window would suppress requested outcomes while someone is
actively working. When configured, quiet mode defaults to `hold`, not `drop`, and
releases one digest after the window. Interpret the interval as local civil time:
spring-forward gaps end at the first valid instant after the configured end;
fall-back overlap releases once, never twice. Webhooks are automation and are
not subject to human quiet hours or the desktop rate ceiling.

### 6. Suppress duplicate desktop notices while a *papio* surface is focused

Add a feature-gated, privacy-minimal `surface_presence_v1` frame from the
extension to the daemon:

```text
surface_presence {
  instance_id: opaque random browser-surface lease ID,
  surface: "popup" | "inbox",
  focused: boolean,
  at: RFC3339 timestamp
}
```

It carries no URL, title, tab id, host, identifier, or page content. Presence is
an expiring hint, not authority. The daemon tracks each `instance_id`
independently and suppresses a desktop attempt while **any** focused lease was
received within 120 seconds. Daemon receipt time controls expiry; client `at` is
diagnostic and bounded so a future browser clock cannot extend a lease. Every
focused popup/inbox refresh on its existing five-/fifteen-second poll also
refreshes its lease, keeping the heartbeat strictly below the TTL. A hidden or
closed instance sends `focused:false` best effort, but correctness does not
depend on the close event arriving.

The implementation contract includes all hops: popup/inbox lifecycle sender,
the background broker allowlist and switch, correlated native request/ack,
Go/TS/schema validators, bridge dispatch branch, feature negotiation, and an
end-to-end forward/reject/skew test. Presence is holder-independent,
non-authoritative session traffic and must be admitted by the bridge before
holder-only dispatch; a pending browser session can report its own focused
surface without claiming handoff authority. Missing/rejected acknowledgements
retry only on the next existing surface poll and never block UI or notification
routing. An old daemon receives no frame.

Focus suppression applies to all seven desktop categories. It records
`suppressed_presence` for that desktop leg; it never applies to the webhook
leg, erase Activity or the inbox row, or defer a duplicate until the lease
expires. A focused surface shows the same state inline and receives no
unsolicited background-event toast.

This is an extension-to-daemon fact, not daemon push. It does not reopen
ADR-0005.

### 7. Toasts acknowledge local actions only

There is no general background-event toast system.

#### Popup

The popup uses its existing inline status regions because it closes on focus
loss. `Acquire`, `Send PDF`, bulk scan, grant, retry, and open-inbox actions put
pending/result/error copy next to the control that caused them. No floating
four-to-six-second promise is made in a surface that may disappear immediately.
The existing sign-in-unblocked notice may retain its five-second/260ms fade
because it describes a browser-local transition and work continues without the
user.

#### Full-page inbox

Generalize the existing fixed `#undo-bar` geometry into one feedback strip:

- dismissal remains the same six-second deferred-commit undo operation;
- a row that remains visible keeps its persistent `.item-result`; it does not
  also create a strip;
- a successful operation that removes a row may use the strip for four seconds;
- an undoable removal uses six seconds and shows `Undo`;
- an error remains on the affected row and does not auto-dismiss;
- only one strip is visible; an active undo has priority and other local
  acknowledgements queue or collapse into one summary;
- focus never moves;
- `prefers-reduced-motion` reduces the transition to opacity only;
- `#operation-status` remains the sole global polite announcer. The visible
  strip must not create a second live-region announcement.

A toast/strip must point to an Activity entry or a surviving/undoable inbox
state. If no durable twin exists, the event is not toastable.

### 8. Make the popup a current-page lens plus a compact pulse

Keep the popup bounded. Top-to-bottom:

1. daemon/version state when abnormal;
2. one global pulse line;
3. current-page Acquire/Send PDF and the current page’s live acquisition;
4. page-bulk scan launcher/result;
5. browser-local work needing action now: security check, provider permission,
   institution sign-in, Downloads access;
6. institution session only while signed out, checking, or serving waiting work;
7. leftover *papio*-created tabs;
8. one catch-up line;
9. terms/resolver decisions;
10. a quiet history/impact footer.

The pulse answers three questions without becoming a dashboard:

```text
Working on 24 papers · 2 decisions waiting · updated 3s ago
Next: retrying 8 OpenAlex papers at 14:32
Acquisition effects 1/1 busy · 6 waiting their turn
```

Render only applicable lines. At idle, use `Idle` unless the typed response
includes an authoritative `last_finished_at`, in which case append
`· last finished 2h ago`. When state is stale or disconnected, say `Can’t tell —
daemon disconnected` or `Status as of 14:22`; never freeze a previous number and
imply it is live.

The current-page acquisition remains richer than the global pulse. It may show
`Queued`, `Fetching PDF`, `Adopted · verifying`, `Acquired`, or `This paper needs
review`, sourced from typed current-page job state. Once `work_pulse_v1` is
negotiated, free-text Activity and the existing ten-minute heuristic are never
authoritative for global liveness or stall classification. With an older daemon,
retain current-page status but label the global pulse unavailable. Do not
synthesize a percentage, ETA, or exact queue position.

The catch-up line is one bounded summary, not a ticker:

```text
While you were away: 12 acquired · 4 decisions waiting · 7 watch hits   Open inbox
```

The popup never lists daemon-side human actions as a second work queue. It shows
at most three unblocker **groups**, ordered: active security challenge,
Downloads/provider permission, institution sign-in, then stale-tab cleanup.
Typed daemon gate/claim identity performs the grouping; dependent siblings do
not consume slots. The extension attaches the current physical tab/platform
remediation and ends overflow with `N more in inbox`.

### 9. Add an honest solicited pulse read model

Do not derive liveness from `jobs_working` or free-text Activity. Add a new
feature-gated request/response pair, `work_pulse_v1`, beside triage counts and
stats. It is solicited by the popup/inbox on their existing timers; no new timer,
subscription, or push channel is introduced.

Daemon response, schema 1:

```text
work_pulse_response {
  request_id
  schema: 1
  generated_at
  nonterminal_total?
  projection_complete?
  in_flight?
  scheduled?
  waiting_required?
  continuing?
  stalled?
  effect_capacity? { busy, limit, waiting }
  human_surface_capacity? { busy, limit, waiting_claims }
  last_forward_at?
  stall_episodes[]? {
    episode_key
    cause_kind: execution_lease_overdue | browser_session_unavailable |
                source_state_unclassified | delivery_poll_overdue |
                cohort_projection_failed
    public_label
    since
    count
  }
  stall_episodes_truncated?
  last_finished_at?
  next_action? {
    at
    kind: retry | delivery_poll | source_gate
    source?
    count
  }
  gates[]? { kind: source_budget, source, until, count }
  gates_truncated?
  latest_batch? {
    batch_id
    label?
    started_at
    settled_at?
    membership: open | complete | partial
    projection_complete?
    total?
    settled?
    nonterminal_total?
    in_flight?
    scheduled?
    continuing?
    waiting_required?
    stalled?
    unavailable?
  }
}
```

Rules:

- Every measurement that can be unavailable is optional. A present count is
  exact; an absent field is Unknown, never zero. Partial objects reject
  impossible combinations but do not force unavailable measurements to exist.
- `continuing` means daemon-authorized autonomous work that is immediately
  eligible for scheduling, holds no active worker or effect lease, has no future
  scheduled timestamp, and requires neither an explicit operation nor a typed
  human gate. An institutional candidate contributes nothing to `continuing`
  while automatic candidate claiming is disabled.
- `in_flight`, `continuing`, `scheduled`, `waiting_required`, and `stalled` are
  mutually exclusive. When `projection_complete=true`, all five and
  `nonterminal_total` are present and their sum equals `nonterminal_total`;
  otherwise no exact whole-work denominator or Idle/Scheduled/Waiting/Stalled
  conclusion is inferred.
- `in_flight` comes from current worker execution or the Phase 3 daemon effect
  governor, not a broad job-state list or an unexpired materialization lease. A
  Phase 2 claim in `claimed`, `bound`, `route_issued`, or `navigated` is not
  activity by itself. Before the unified governor is authoritative, a currently
  executing claim/bind/route/navigation handler may be reported only by an
  explicit correlated-operation projection; otherwise the claim contributes no
  positive bucket.
- `scheduled` is backed by `retry_at`, delivery `next_check_at`, or a source gate.
- `last_forward_at` excludes recovery rewinds such as stale `fetching → resolving`.
- `last_finished_at` is the newest authoritative terminal job/cohort transition,
  not merely the last Activity or forward-progress timestamp.
- Every stalled work item is assigned to exactly one current durable stall
  episode. If a nonterminal institutional claim is bound, route-issued,
  navigated, or parked but neither a current operation/effect permit, future
  schedule, effective explicit action, nor current typed gate classifies it,
  the institutional portion is incomplete: do not put it in any bucket, set
  `projection_complete=false`, and render Unknown unless an independent
  positive `in_flight + continuing` count establishes Moving. A materialization
  phase, extension correlation, Activity sentence, or elapsed-time heuristic
  never creates `waiting_required` or `stalled`.
- `waiting_required` is scoped to nonterminal work: it counts the effective
  researcher-owned turns that block items in the nonterminal projection,
  including jobless PDF grabs, plus the daemon's current typed human-gate
  projection (`CurrentHumanAttention`), with one turn per gate/claim and
  dependent siblings excluded. It is **not** the number of decisions the
  researcher owes. Counts schema v3 `turns_required` is the turn authority and
  the only number any surface may present as `decisions waiting` or `need you`.
  The two values legitimately differ when an open action outlives its job (for
  example, an `openurl_available` action on a terminal `unavailable` job): that
  action remains an actionable inbox turn, but it is not nonterminal work and
  must not enter `waiting_required` or break the five-bucket partition. This is
  a scope distinction, not an arithmetic discrepancy to reconcile.
  Failure to read either authority makes the projection incomplete rather than
  zero.
- `stall_episodes` is the only authority for `Stalled` and
  `system_degraded` pulse copy. An episode key remains stable until recovery
  rearms it. Without a valid episode affecting stalled work, the UI never
  infers Stalled.

The five `cause_kind` values in the schema are also the exact bounded
`event_kind` values emitted for their degradation episodes; they are not a
second classifier. The notification router maps each exhaustively to
`system_degraded`, phase `episode`, and rejects any other stall event kind.
Episode recovery and rearming reuse the same durable identity/window contract
as notification routing.

- `next_action.at` is a scheduled check/retry, not an ETA for success.
- Gate source labels are public source/provider names, never quota identity
  fingerprints or bearer URLs.
- `latest_batch.membership=partial` omits the denominator and renders
  `Recent browser submissions`; it is not reconstructed from telemetry.
- For `latest_batch.membership=complete` and
  `latest_batch.projection_complete=true`, `settled + nonterminal_total =
  total`, and `in_flight + continuing + scheduled + waiting_required + stalled
  = nonterminal_total`. `unavailable` is an informational subset of `settled`,
  not another bucket.
- Older daemons expose no feature; the extension omits the global pulse or says
  live progress is unavailable. It never fabricates `Idle`.

Wire bounds for schema 1 are normative:

- request, batch, and stall episode IDs: 1–64 ASCII bytes; public source names:
  1–64 UTF-8 bytes; cohort labels: at most 256 UTF-8 bytes; stall
  `public_label`: 1–64 UTF-8 bytes; no control characters;
- every integer count: `0..1_000_000`, safely below JavaScript's integer limit;
  stall episode `count` is `1..1_000_000`;
- `stall_episodes`: at most 16 entries, unique by `episode_key`, ordered by
  `since` ascending then `episode_key`, and truncated from the end. Set
  `stall_episodes_truncated=true` exactly when more valid episodes exist.
  `stalled > 0` requires at least one returned episode; if not truncated,
  episode counts sum exactly to `stalled`; if truncated, their sum is less than
  or equal to `stalled`. An item assigned to zero or multiple episodes makes
  the containing projection incomplete rather than double-counted;
- `gates`: at most 16 entries with unique `(kind, source)`; omitted and empty are
  distinct only where the field's optionality says so, and `null` is forbidden;
- timestamps: RFC3339, at most 64 bytes, bounded to a plausible daemon window;
- `busy <= limit` for each capacity, each settled/bucket count `<= total` when
  both exist, the global and batch equations above hold whenever their
  projections are complete, and incompatible partial measurements reject
  fail-closed;
- a serialized response must remain below 32 KiB, well inside
  `protocol.MaxBrowserMessageBytes`; producers truncate only the bounded stall
  and gate lists using their deterministic orders and set the matching
  truncation flag.

The daemon API method is `work.pulse_v1`. The CLI surface is one-shot
`papio pulse [--json]`: text uses the same Moving/Scheduled/Waiting/Idle/Stalled/
Unknown vocabulary; JSON is one structured object (not a list envelope) with the
same optionality and an explicit `available:false` old-daemon result. Register
the method/command in API and CLI conformance and generated reference docs. Do
not add a second CLI follow timer in this work.

The extension merges only browser-owned facts after the response:

- physical bound/scaffold tabs reconciled to current holder generation;
- provider-local security cooldown disposition; and
- connection/freshness state.

Effect-permit, candidate-queue, authentication-owner, typed-gate, and
human-surface counts remain daemon authority. Browser-local values are
suppressed for one keepalive cycle after a worker restart unless reconciliation
has completed. Worker-local zero is not proof that no work exists.

Derived display state:

Choose one primary label in this precedence; companion exact bucket counts
remain visible, so the primary label never conceals scheduled, required, or
stalled work:

1. **Moving:** `in_flight + continuing > 0` in a fresh projection. Copy may
   distinguish active acquisition from eligible queued work.
2. **Waiting on you:** no work is moving and `waiting_required > 0`.
3. **Stalled:** nothing is moving or waiting on the researcher, `stalled > 0`,
   and at least one valid stall episode names the cause.
4. **Scheduled:** only future automatic work remains and `scheduled > 0`.
5. **Idle:** a complete projection says `nonterminal_total = 0`.
6. **Unknown:** the reading is missing, stale, disconnected, incomplete for the
   requested conclusion, contradictory, or reports stalled work without a
   valid episode.

### 10. Make browser and CLI batches durable cohorts

The existing browser `batch_id` is not enough for honest progress. Add durable
cohort authority shared by CLI and browser submissions rather than deriving a
cohort from timestamps, telemetry, or a common consumer string.

Do not widen the strict v1 page-bulk frame. Add feature
`page_bulk_cohort_v2` with strict
`page_bulk_submit_v2_request`/`page_bulk_submit_v2_result` messages:

```text
request {
  request_id, scan_id, cohort_id,
  source: {
    kind: browser_page
    origin: bare lowercase https scheme+host
    detector: bounded detector identifier
  },
  cohort_total: 1..200,
  chunk_index: 0..3,
  final_chunk: boolean,
  canonical_keys: 1..50
}
result {
  request_id, scan_id, cohort_id, chunk_index, final_chunk, batch_id,
  membership: complete | partial | open,
  cohort_total?,
  persisted_members,
  submitted, joined, already_owned, invalid
}
```

`cohort_total` is the ordered manifest length. Chunking is deterministic:
`ceil(cohort_total/50)` chunks, zero-based indices, exactly 50 keys in every
non-final chunk, and the exact remaining keys in the final chunk.
`final_chunk` is true exactly on the last derived index. The four outcome counts
are for the addressed chunk and sum exactly to its canonical-key count after
request-level validation. `persisted_members` is cumulative durable membership;
`membership` and `cohort_total` describe cumulative cohort state. A browser
applies a result at most once by `(cohort_id, chunk_index)`.

One `Acquire all eligible` action authorizes the complete 1–200-key manifest.
The page-bulk surface sends the ordered manifest once to background through the
existing authorized `papio.pageBulk.submit` runtime path; background owns
chunking, durable recovery, and replay. The runtime validator accepts 1–200
unique bounded canonical keys and the reply states `mode: "v2"` plus
`processed_count=cohort_total`. When `page_bulk_cohort_v2` is unavailable,
background retains the current v1 behavior for only the first 50 keys, replies
with `mode: "v1"` and `processed_count<=50`, and the page marks only that prefix
submitted and labels it `Progress covers this 50-item submission`. It never
claims a 50–200-paper denominator from v1.

#### Browser restart recovery

Create `extension/src/page-bulk-recovery.ts`; no existing storage abstraction
has the required restart and URL-privacy boundary. It owns only the dedicated
`page_bulk_cohort_recovery_v1` key in `chrome.storage.local`. It must never use
the session-preferred `chromeBackend`, `papio_state_v1`, `StoreShape`,
`serializeManagedState`, or their generic URL scrubber, and it must not weaken
those existing protections.

```text
PageBulkCohortRecoveryStoreV1 {
  version: 1
  cohorts: {
    [cohort_id]: {
      cohort_id
      scan_id
      source
      cohort_total
      canonical_keys       # complete ordered manifest
      next_chunk
      unresolved? {
        request_id
        chunk_index
        payload_digest
      }
      updated_at
    }
  }
}
```

Validate every loaded entry and every replacement write. Reuse one exported
TypeScript implementation of the current Go/schema bare-lowercase-origin rule:
`kind` is exactly `browser_page`; `origin` is at most 300 characters and is
exactly `https://` plus a lowercase DNS-shaped host and optional 1–5 digit port,
with no path, query, fragment, username, or password; `detector` is non-empty
bounded protocol text of at most 128 characters. The manifest contains 1–200
unique canonical keys, each non-empty, NUL-free, and at most 300 Unicode code
points. No other URL-bearing or page-title field is admitted. Extract the
currently duplicated TypeScript origin/key checks into exported protocol
validators and reuse them in strict frame parsing, the runtime request
validator, and this recovery module; do not add a third grammar.

`payload_digest` is a browser-local corruption guard, never daemon identity:
lowercase SHA-256 hex over UTF-8 JSON encoding of the fixed array
`[scan_id, cohort_id, source.kind, source.origin, source.detector, cohort_total,
chunk_index, final_chunk, canonical_keys]`. On load, recompute it before replay.
`updated_at` is browser-local RFC3339 time. Discard without sending any frame
when a record is malformed, its timestamp is more than five minutes in the
future, its digest mismatches, or 24 hours have elapsed since its last validated
replacement; log one bounded local recovery error with no identifiers or
origin. Serialize all read-modify-write operations for the top-level map so two
authorized page-bulk surfaces cannot overwrite one another.

Before the first mutation, persist the whole record and unresolved first chunk.
Before every later send, persist that chunk's `request_id`, index, and digest.
After a valid matching result, atomically clear `unresolved` and advance
`next_chunk`; do not send the next chunk unless that replacement succeeds. On
the final valid result, remove the cohort entry in one storage replacement.
Thus a storage failure before chunk zero sends nothing; a failure after a daemon
commit leaves the unresolved entry intact and safely replays the cached result.
Delete no live entry merely because its page or source tab closed.

After a worker suspension, reconnect, or browser restart, load valid entries and
resume automatically after the `hello_ack` handler has returned to the inbound
queue. Start resume with `void` outside the serialized inbound handler; never
await a correlated result from inside that handler. If the current daemon no
longer advertises v2, retain the entry until a capable connection returns or it
expires; never downgrade an unresolved v2 chunk into v1.

#### Caller-owned replay and daemon idempotency

For v2, `request_id` is caller-owned. The first attempt mints it once and
persists it before sending; every replay of that unresolved chunk reuses it.
Extend `Bridge.requestNative` and `Bridge.sendCorrelated` with an optional
supplied request ID. When supplied, `sendCorrelated` validates and uses it
verbatim instead of calling `nextRequestID()` or replacing it in the payload.
All existing callers omit it and retain current behavior. A concurrent pending
map entry with the same supplied ID fails locally without sending a duplicate.

Daemon idempotency is `(cohort_id, chunk_index)`. A matching replay requires the
stored request ID and normalized field-by-field equality of `scan_id`,
`cohort_id`, source, total, index, final flag, and ordered canonical keys; JSON
property order and either side's digest implementation are irrelevant. It
returns the identical cached result. A reused request ID, index, or cohort with
different semantic fields returns structured `conflict` before any domain
mutation. Chunks are accepted strictly in sequence. The first accepted chunk
fixes `scan_id`, normalized source, and total. A key duplicated across chunks,
an early/late final flag, a gap, or a chunk beyond the derived last index is a
conflict. The cohort becomes complete only after the final chunk commits the
expected number of unique member ordinals. A missing final chunk becomes
`partial` after ten minutes of inactivity but remains resumable with the same
record and idempotency keys for 24 hours.

Use these daemon tables; `page_bulk_runs` remains telemetry-only:

```text
acquisition_batches(
  id, cohort_id UNIQUE, source_kind, source_label, source_detector?,
  source_scan_id?, expected_total, created_at, updated_at, closed_at?,
  membership_state
)

acquisition_batch_chunks(
  batch_id, chunk_index, request_id UNIQUE, final_chunk,
  canonical_keys_json, result_json, created_at,
  PRIMARY KEY(batch_id, chunk_index)
)

acquisition_batch_members(
  batch_id, ordinal, canonical_key, job_id?, submission_outcome,
  PRIMARY KEY(batch_id, ordinal),
  UNIQUE(batch_id, canonical_key)
)
```

- `source_label` for a browser page is the validated bare origin only; never a
  path, query, fragment, page title, or bearer value. CLI submissions use an
  explicit non-browser `source_kind` and privacy-bounded `source_label` through
  the shared cohort service; they never fabricate a browser origin or detector.
- Each chunk stores its semantic request, appends member rows, and caches its
  exact result. Domain job submission may already have succeeded when cohort
  membership persistence fails; mark the cohort durably `partial`, omit
  `cohort_total` from subsequent pulse output, and say
  `Recent browser submissions`. Never reconstruct membership from telemetry.
- Submitted and joined-existing members retain `job_id` and follow that job's
  terminal projection even when it belongs to another cohort. Already-owned and
  invalid members settle immediately. One shared job may contribute to multiple
  cohort summaries but produces no duplicate per-job desktop outcome.
- CLI batch import uses the same batch/member service and exposes complete
  versus partial coverage in its structured result.
- A cohort settles when every submitted/joined job is terminal and every
  immediate member has an explicit outcome. “Settled” is not synonymous with
  “acquired” or “needs you”.

The latest active/recent cohort populates `work_pulse.latest_batch`. The inbox
may show full cohort detail; the popup shows one line. No progress percentage or
bar ships. Counts such as
`183 total · 137 settled · 24 moving · 22 waiting on you` remain honest when
outcomes are mixed.

### 11. Make the inbox action-first and repetition-aware

Adopt the reviewed presentation direction while preserving daemon ordering and
item semantics.

#### Attention

- Remove required/advisory inline-start rails and working opacity from
  `extension/src/inbox.html`.
- Required is the ordinary Actions-row baseline: imperative instruction plus
  operation-specific control.
- Working is full-opacity, explicitly says `papio is continuing — …`, and has no
  primary decision control.
- Advisory keeps domain-specific presentation. Retractions retain their
  integrity glyph/copy because of their domain meaning, not because `advisory`
  is a low or high severity.
- Keep the `attention` data attribute only if tests or semantic selectors need
  it; it must not drive generic colour/opacity.

#### Task families

Repeated homogeneous work receives a shared family heading and instruction:

```text
Manual downloads · 39 papers
Open each source and save the PDF — papio adopts it.
```

Family identity is daemon-authored, not reconstructed from rendered copy or the
currently loaded page. Negotiated triage snapshot schema 5 carries an all-or-none
`run_key`, `next_actor`, `guidance_variant`, and `operation_variant` quartet on
each participating row; counts schema v3 carries the matching exact run totals.
The strict fields and mappings are frozen in §14.

Presentation rules:

1. Hoist guidance only when at least two adjacent loaded rows have the same
   `run_key`, byte-identical locally rendered guidance for their declared
   variant, and the declared compatible operation variant.
2. A run is contiguous in full daemon rank order, not merely in the current
   page. It may cross a page boundary. Because the daemon groups action rows by
   family (see the ordering amendment below), one variant tuple is one run and
   one heading; a client that is nevertheless served two runs of the same tuple
   still renders two headings.
3. Preserve daemon rank exactly. Attention is not a global sort key. The client
   never reorders, groups, or promotes rows to assemble a family; it renders the
   family-grouped rank the daemon already supplies.
4. Any missing, unknown, or internally inconsistent variant remains standalone
   with its own instruction. The client never guesses a family from prose,
   `action_kind`, Activity text, or a detail-string regex.
5. Under a filter or page boundary, show `4 of 39 shown` only when the matching
   `family_runs` entry is present and the breakdown is complete. Otherwise omit
   the total. A page beginning in the middle of a run still renders one heading
   for its first visible member and uses that run's full exact count.
6. Do not add `Open all` or destructive family controls. Opening 39 tabs and
   treating a six-second undo as protection for a 39-job cancellation are both
   unacceptable.

**Ordering amendment (2026-08-12).** These rules originally required the client
to render a second block rather than move members together, so a family variant
recurring after an intervening row stayed fragmented. That protected a daemon
ranking that does not exist: open human actions are selected `ORDER BY a.id
ASC` — pure insertion order, with no priority, severity, rank, or attention
term. Fragmenting a family therefore preserved no signal while defeating the
feature's purpose; on the author's own library, 37 open actions produced ten
blocks and repeated the same manual-download instruction across four of them.
The daemon now orders action rows by family: families by their earliest member,
insertion order within a family, ranks assigned afterwards. Runs stay
maximal-contiguous, `family_runs` collapses to one entry per family,
`first_rank` stays coherent with the emitted order, and no client change is
required. The only genuine signal survives at both levels — the oldest action
still opens the first block, and the oldest member still leads its family.
Family identity is the full variant tuple, so `manual_download` and
`manual_download_adapter_missing` remain separate families. Rows with no mapped
variant join no family (rule 4) and stay standalone at their own position.
Accepted tradeoff: a new action joins its family block rather than appending at
the bottom, so an item can appear mid-list; blocks themselves do not jump,
because they are keyed on their earliest member. This applies to `human_action`
rows only — PDF grabs, watch hits, and retractions keep their own rank bases and
never interleave with actions.

The initial closed guidance vocabulary covers manual download, missing adapter,
institution sign-in, page open, PDF identity review, document-delivery
reconciliation, Downloads access, terms acceptance, typed security challenge,
PDF identifier, and autonomous-continuation variants. Unknown/new route or
action states remain standalone and make the exact breakdown incomplete until
their copy and operations are explicitly mapped.

#### Counts and empty state

Use researcher language:

```text
44 open · 39 need you · 5 for reference · papio is working on 7
```

`need you` requires the exact effective-required `turns_required` count. `for
reference` contains watch/advisory inventory and does not imply a task. When
Actions is empty while work continues: `No decisions waiting. *papio* is
working through 7 papers — see Activity.`

### 12. Make Activity quiet, pageable, and recoverable

Activity remains a pull-only durable ledger.

- Add a new negotiated Activity page contract rather than widening the current
  strict response. Request supports `before_seq` and the caller's prior
  `seen_through_seq`; response includes entries, `has_more`, the next cursor,
  authoritative `latest_seq`, and either exact `new_count_since` or `gap=true`.
  Keep a hard page size of 50. `new_count_since` counts raw durable Activity
  entries independently of visual repeat collapsing; it is omitted when the
  prior watermark predates retained/available history.
- Persist `activity_seen_through_seq` in `chrome.storage.local`. This is
  browser-profile read state, not daemon authority. Rendering the newest page
  in a visible Activity tab acknowledges all durable entries through that
  page's `latest_seq`, including older unpaged history; it does not claim every
  acknowledged row was individually viewed. Preserve the old watermark until
  that visible render completes.
- Show a `Since you were last here` divider at the prior watermark and an
  `Activity (N new)` tab label only when `gap=false`. The catch-up line uses the
  same response. With a gap, say newer Activity is available without an exact
  count or divider.
- Change the continuously replaced Activity list to `aria-live="off"` while
  retaining its log semantics. Remove `role="status"` from per-row
  `activity-live-status` chips; only a deliberate focused-row status channel may
  announce a row transition. Announce the bounded `N new Activity entries`
  affordance when appropriate; do not narrate every polled row.
- Preserve durable event order, repeat collapsing, job links, unavailable/error
  states, and user-initiated `Show more`.
- Add normalized notification-routing events as system-scoped Activity entries:
  `notify.attempted`, `notify.held`, and `notify.digest`. Use “attempted”, never
  “delivered”; AppleScript and OS notification centers provide no delivery or
  visibility acknowledgement.
- The notification router ignores `notify.*` events so recording an attempt
  cannot recursively notify.
- With Activity page v1, state `Activity history is limited to the latest 50
  entries with this daemon`; suppress exact `N new` and the since-divider when a
  gap may exist.

### 13. Change the badge from inventory to turns

This is an intentional ADR-0001 amendment, not a styling tweak.

Keep the existing precedence:

1. disconnected/broken `!`;
2. browser-local blockers and permissions;
3. actionable-turn count;
4. blank.

Change the numeric tier from `pending_total` to `turns_required`, the exact
daemon-owned count of effective `required` turns. `turns_required` is the sole
turn authority: every surface that says `decisions waiting` or `need you` must
use it. Watch hits, retractions, dependent sibling papers, and `working` rows
remain available in the tooltip/inbox but do not inflate the number. This may
legitimately differ from the pulse's nonterminal `waiting_required` bucket
when an open action outlives a terminal job; the badge never substitutes that
bucket for the turn count.

Institutional sign-in/challenge ownership comes from the institutional
processing contract: one current typed gate/claim produces one action, carries
its dependent sibling count, and points to the one bound browser/platform
remediation. Background may attach a physical tab/focus operation but never
elects a different owner.

### 14. Add negotiated count and family-run breakdowns

Counts schema v3 provides the exact daemon side:

```text
turns_required
turns_working
family_breakdown_complete
family_runs[]? {
  run_key,
  first_rank,
  route_class,
  action_kind,
  next_actor,
  guidance_variant,
  operation_variant,
  count
}   # max 128
required_turns_complete
required_turns[]? {
  item_id,
  item_kind: human_action | pdf_grab,
  action_id?,
  job_id?,
  grab_id?,
  route_class,
  gate_claim_id?,
  dependent_jobs
}   # max 1024
```

`turns_required` and `turns_working` count daemon-owned action-like rows by
their effective attention. A `human_action` required-turn entry requires
`action_id` and `job_id` and forbids `grab_id`; a `pdf_grab` entry requires
`grab_id`, forbids `action_id`, `job_id`, and `gate_claim_id`, and has
`dependent_jobs=0`. `item_id` is the exact snapshot row ID. A typed gate claim
appears once regardless of its dependent paper count; two profiles/claims remain
two turns.

`turns_required` is the only counts-v3 authority for researcher-owned
decisions. It is intentionally distinct from the pulse's `waiting_required`,
which is a nonterminal-work bucket used only in the five-way partition and
therefore excludes an open action whose parent job is already terminal. A
terminal-job action remains in the inbox and in `turns_required`; clients must
not reconcile these values by changing either scope or by presenting
`waiting_required` as a decisions count.

When `required_turns_complete=true`, the list has exactly `turns_required`
unique entries. Background, popup, and inbox consume that daemon count and use
`gate_claim_id` only to attach reconciled browser-local focus state. When more
than 1,024 required turns exist, the list is omitted and completeness is false;
the default badge shows no misleading number and its tooltip says
`Many decisions waiting — open inbox`. The legacy Everything-pending option may
still show the bounded pending inventory.

Add negotiated triage snapshot schema 5 rather than widening schemas 1–4. On
every participating `human_action` or `pdf_grab` row it permits only an
all-or-none quartet:

```text
run_key
next_actor
guidance_variant
operation_variant
```

The matching counts response uses `family_runs` as the sole exact-total
authority. `route_class` uses schema 5's closed route vocabulary.
`action_kind` is the closed family action kind: it equals the row's mapped
human-action kind, while a PDF grab uses `pdf_identifier_needed`.
`next_actor` is exactly `papio | researcher | reference`; current action-like
rows map `working → papio` and `required → researcher`, while `reference` is
reserved for an explicitly mapped advisory family rather than inferred from low
severity.

The initial closed `guidance_variant` enum is:

```text
manual_download
manual_download_adapter_missing
institution_sign_in
open_page
verify_identity
document_delivery
downloads_access
terms_acceptance
security_challenge
pdf_identifier
papio_continuing
```

The daemon maps `manual_download_adapter_missing` only from the stable
`provider_adapter_missing` diagnosis, never the action-detail sentence. It maps
`security_challenge` only from the institutional typed-gate projection, never
browser Activity text. Auth-required handoffs map to `institution_sign_in`;
known non-auth handoffs and `openurl_available` map to `open_page`; working
handoffs and fulfilled delivery continuation map to `papio_continuing`.
Identity, delivery, Downloads, terms, and PDF-grab rows map directly from their
closed route class. A new action kind, unknown auth state without an
authoritative Working mapping, unavailable diagnosis, or absent typed-gate
projection is unmapped rather than guessed.

The initial closed `operation_variant` enum describes the effective rendered
per-row operation set after Working suppresses primary decision controls:

```text
none
dismiss_only
open_and_dismiss
accept_reject
accept_reject_open
delivery_reconcile
provide_identifier_or_dismiss
```

The Go producer and TypeScript renderer share exhaustive disposition tests from
every representable route/action/attention state to both variant enums. Dynamic
disabled/pending state does not change a variant; an operation-set mismatch
makes the row standalone and the breakdown incomplete.

A family run is one maximal contiguous sequence in the daemon's full rank order
with the same tuple `(kind, route_class, action_kind, next_actor,
guidance_variant, operation_variant)`. The daemon computes runs before
pagination. Repeated non-contiguous occurrences of the same tuple are different
runs. `first_rank` is the first member's existing bounded snapshot rank.
`run_key` is `"fr1_"` plus the first 32 lowercase hex characters of SHA-256 over
a length-prefixed UTF-8 encoding of
`[schema, first_row_id, kind, route_class, action_kind, next_actor,
guidance_variant, operation_variant]`. `first_row_id` is the first member's
snapshot `id`. Only the daemon computes the key; clients treat it as opaque.
This keeps one run stable across page requests while its first member and tuple
remain unchanged without exposing its item identity in the key.

Entries are unique by `run_key`, ordered by `(first_rank, run_key)`, and use
ASCII identifiers of 1–64 bytes; `first_rank` follows the existing
`0..MaxBrowserInteger` bound and `count` is `1..1_000_000`. Every row in a run
carries the same quartet. When `family_breakdown_complete=true`,
`family_runs` is present, contains every action-like run, and its counts sum to
`turns_required + turns_working`. If any representable row lacks a mapped
variant, any action-like row is omitted by the selected snapshot schema, the
128-run bound is exceeded, or the daemon cannot compute the full ordered
projection, completeness is false and `family_runs` is absent. Individually
valid row quartets may still support loaded-page hoisting, but the UI omits exact
totals and never substitutes a route-class aggregate.

Thus one institution sign-in with three dependent papers contributes one turn.
Two separated runs of the same family render their own counts, and a run split
across pages retains one key and one total.

Tooltip carries the full breakdown:

```text
papio: 4 need you · 7 watch hits · 1 retraction notice
```

Add a browser-local option:

- `Decisions waiting` (default);
- `Everything pending` (legacy-style count); or
- `No number`.

The option never suppresses disconnected/blocker `!` states. Older daemons fall
back to `pending_total` and label the tooltip honestly as `pending items`; they
do not pretend to have a required-turn or family-run breakdown. Counts polling
includes every schema-v3 field in its refresh signature; snapshot paging
requests schema 5 only after negotiation. Browser-local tab/platform disposition
refreshes on the existing keepalive revision, so a legacy-total tie cannot leave
rows or family presentation stale.

### 15. Settings model

#### Daemon-owned notification settings

Extend `[notify]` with a preset and bounded overrides:

```toml
[notify]
enabled = true
preset = "milestones"          # quiet | milestones | verbose
max_per_hour = 6                # 0 = unlimited
quiet_hours = ""                # HH:MM-HH:MM local; empty disables
quiet_mode = "hold"             # hold | drop
digest_every_minutes = 240
completion_quiet_minutes = 10
completion_max_hold_minutes = 120
stall_after_minutes = 30
webhook_url = ""
webhook_secret = ""

[notify.categories.decision_opened]
desktop = "immediate"          # off | digest | immediate
webhook = "immediate"
window_seconds = 300
```

All seven category names and all enum values are fail-closed. Validate sensible
ranges and relationships. This is a strict-mode config change: the new daemon
binary and config deploy together.

Presets:

| Category | `quiet` | `milestones` default | `verbose` |
|---|---|---|---|
| standalone request outcome | immediate failures, digest success | immediate | immediate |
| new decision | digest | immediate / 5m coalesce | immediate / 60s |
| pending-decision reminder | off | 4h digest | immediate, one/pass |
| batch completion | off | one checkpoint if needed + meaningful final delta | one checkpoint if needed + meaningful final delta |
| watch discovery | off | catch-up/digest only | immediate/coalesced |
| integrity notice | immediate/capped | immediate/capped | immediate/capped |
| system degraded | immediate/state-deduped | immediate/state-deduped | immediate/state-deduped |

Webhook defaults stay immediate for all categories and ignore desktop quiet/rate
policy. Each webhook category may be overridden explicitly.

Add CLI/doctor surfaces before browser presentation:

- `papio notify show [--json]` prints the effective routing table and source
  (preset or override);
- `papio notify preview <category> [--count N]` renders the exact copy without
  sending;
- `papio notify test <category>` explicitly sends one local test through the
  configured channel;
- `papio doctor` reports desktop capability, effective preset, held-digest
  state, and webhook configuration without printing the secret.

Do not add a general config mutation API in this work. Daemon routing remains in
TOML and CLI-first documentation; a future live settings editor must solve
atomic config rewrite/reload rather than creating a second preference store.

#### Browser-local settings

Add an Options section named **Feedback and interruptions** using the existing
row/switch/control patterns:

- Toolbar count: `Decisions waiting` / `Everything pending` / `No number`;
- Catch-up line when a *papio* surface opens: on by default;
- Transient success acknowledgements: `All requests` / `Errors only` / `Off`.
  This setting never hides pending state, actionable errors, persistent row
  results, or their accessible announcements;
- Read-only daemon notification summary with `papio notify show` and the config
  path. Do not imply that the extension can modify daemon routing.

The options page does not gain browser notification controls because the
extension does not own desktop notifications.

## Routing matrix

| Event | Focused popup | Focused inbox | Badge | Desktop when no focused surface | Durable location |
|---|---|---|---|---|---|
| Acquire/Send PDF accepted | inline beside control | not applicable | none | none | job/Activity |
| Standalone paper ready | current-page live state | Activity/catch-up | none | immediate | Activity/job |
| Standalone paper exhausted | current-page error + inbox link | row/Activity | required if a decision opened | immediate failure | Activity/job/action |
| New manual-download/sign-in/review turn | relevant browser-local card only | row/count update; no unsolicited toast | required-turn count | coalesced `decision_opened` | Actions + Activity |
| Existing turn still waiting | persistent row | persistent row | unchanged | 4h digest | Actions + reminder event |
| Institution sign-in releases jobs | fading session notice | pulse/Activity update | count decreases | none | Activity |
| Provider/security cooldown | Needs you if user action exists; otherwise pulse | pulse/provider detail | blocker tier only when actionable | no “working” notice | Activity/browser state |
| Per-paper import inside batch | pulse/count only | Activity only | none | none | Activity/batch |
| Batch settles | catch-up/pulse | Activity/batch summary | none | one summary | batch + Activity |
| Watch discoveries | catch-up | Watch hits/Activity | tooltip only | digest/off by default | Watch hits + Activity |
| Retraction scan | catch-up | retraction row/Activity | tooltip only | one capped integrity notice | inbox + Activity |
| Daemon disconnects | persistent daemon state | persistent connection banner | `!` | impossible from dead daemon | logs/doctor; surface state |
| Nameable stall while daemon alive | pulse | pulse/Activity | no number | once per episode | Activity/doctor |
| Row operation succeeds and row leaves | inline status if popup owns it | four/six-second strip | update from state | none | Activity or deferred undo |
| Row operation fails | persistent inline error | persistent row error | unchanged | none | surviving row/Activity where applicable |

## Freshness and accessibility rules

1. Reuse existing timers: popup five seconds, visible inbox fifteen seconds,
   background keepalive one minute. Add no hidden high-frequency timer.
2. Hidden inboxes do not poll. Focus/visibility return triggers one refresh only
   through the same mutation guard as timer/count refreshes.
3. Full inbox refresh remains suppressed during a confirmation, mutation,
   deferred-dismissal window, **and the awaited dismissal commit after that
   window** so the list never reorders or resurrects rows under a decision.
4. Pulse freshness is based only on the browser's local receipt time of the
   last successfully validated response: 15 seconds in the popup and 45 seconds
   in the visible inbox. Daemon `generated_at` is source time for `as of` copy
   and skew detection, never the freshness clock. Cache the response with local
   receipt time and a worker-epoch marker; after a worker restart makes that
   receipt epoch untrustworthy, render Unknown until a fresh response arrives.
   Never rewrite daemon `generated_at` to browser time.
5. Countdowns update no more often than every ten seconds and use coarse units.
   Past deadlines say `due now`, never a negative duration.
6. If daemon/browser clock skew exceeds a poll interval, prefer absolute times
   over relative ages.
7. Never use colour as the only carrier. `attention` meaning is in copy and
   controls. Retraction/error colour remains paired with text/glyphs.
8. The Activity log is not a live announcement stream. One bounded status region
   announces new-entry counts on demand.
9. The toast/undo strip does not move focus. Dialog focus trapping and return,
   keyboard shortcuts, typing suppression, and row focus restoration remain as
   ADR-0001 specifies.
10. Reduced-motion mode removes translation/animated countdown flourishes; time
    still updates as text.
11. Batch/pulse numbers use tabular numerals but never percentages, invented
    time saved, success ETAs, or queue positions.
12. Unsupported features degrade explicitly. “Unavailable with this daemon” is
    valid; a fabricated zero is not.

13. Every repeated row control has an item-specific accessible name; a hoisted
    family instruction is programmatically associated with its rows.
14. The full-page inbox has no horizontal scroll at 320 CSS pixels. Below the
    existing wide breakpoint, header/pulse/counts stack, family rows become one
    column, and controls wrap beneath—not over—the citation. At 200% zoom, every
    operation and status remains reachable.
15. The popup remains a compact current-page surface between 320 and 420 CSS
    pixels. Long titles/origins wrap or truncate visually while retaining the
    complete accessible name; blocker groups and the `N more in inbox` link do
    not push the primary current-page action below an unbounded list.
16. Dialogs keep a viewport-bounded scroll region, visible close/submit controls,
    focus trap/return, and no horizontal clipping on the narrow layout.
17. Popup asynchronous-operation state lives in the render model under a stable
    group/job plus operation key, not only on a DOM button. Before a poll render,
    capture the focused control key and restore focus when it survives. A
    pending control remains visibly pending/disabled across refreshes. When its
    row genuinely disappears, move focus to the section heading or bounded
    status region and announce the transition once.

## Scenario walkthroughs

### Large browser batch

A researcher scans 183 works from a results page and selects
`Acquire all eligible`. The extension keeps one durable cohort while
sequentially submitting `50 + 50 + 50 + 33` under the wire cap. Each chunk
result updates one inline aggregate; the cohort view can summarize
`177 submitted · 4 joined existing work · 2 already in your library`. No
desktop notification fires while they are looking at the result.

Within five seconds the popup pulse shows the authoritative active projection;
if all 177 submitted and four joined jobs remain nonterminal, that is
`Working through 181 papers · acquisition effects 1/1 busy`. If any have
settled, it shows the smaller exact count instead. The inbox can show the durable
cohort as `183 total · 41 settled · 120 scheduled/working · 22 need you`.
It never shows `22%` or an ETA.

Forty OA papers land during the next twenty minutes. No per-paper toast or
desktop notice fires. Activity records all outcomes. After ten quiet minutes or
the two-hour maximum hold, one checkpoint may say what remains. A later final
summary fires only on settlement and only when it contains a meaningful delta.

### Thirty-nine manual downloads

The first required turns coalesce for five minutes into one desktop notice:
`39 papers need you — open the papio inbox`. The badge shows the effective
required-turn count. The inbox shows one `Manual downloads · 39 papers` family,
one instruction, and 39 ranked rows. It does not show 39 red rails or repeat the
same instruction 39 times.

At the first reminder eligibility point the actions enter the four-hour digest;
they do not create six class-specific notifications. Existing exponential
backoff and seven-day quiescence remain. After quiescence the rows still exist
and can be opened, but *papio* stops volunteering reminders.

### Institution sign-in with waiting siblings

Three papers depend on one institution sign-in. The daemon's typed current
gate/claim creates one required row with `3 dependent papers`; no browser-local
election is allowed. The badge counts one, not three. Dependent paper rows say
`papio is continuing — waiting for the sign-in already open in another tab` and
have no Focus control.

If the popup/inbox is focused, no desktop notice fires. After successful sign-in,
the popup may show its existing fading `Signed in — 3 papers released` notice;
the pulse resumes and Activity records the release. No success notification is
needed because nothing remains for the researcher.

### Scheduled quota wait

OpenAlex supplies a future source gate. With other work active, the pulse says
`Working · OpenAlex resumes at 00:00`. If it is the only remaining route, the
state is `Waiting until 00:00 — OpenAlex daily quota`, not stalled. No desktop
notice fires. At the due time normal scheduling resumes.

### Pending document delivery

An ILL request is submitted and its next poll is four hours away. The row says
`papio is continuing — next library check at 18:40`. Pulse state is Scheduled.
No toast, badge number, or desktop notification implies that a researcher must
act.

When the provider reports fulfilment and reconciliation genuinely needs a
choice, the row becomes Required, joins the appropriate task family, changes the
badge, and may produce one coalesced decision notification.

### Retraction scan

A scan finds 12 retractions. The router records all 12 durable notices but emits
one capped integrity notification. The badge number is unchanged; its tooltip
and the inbox show the notices. The rows keep retraction-specific prominence
even though their attention value is advisory.

### Watch run

Eight watches find 34 works. Default desktop routing holds discovery for
catch-up rather than sending eight notifications. The Watch hits tab and
Activity update durably; the next popup/inbox open shows `34 watch hits across 8
watches`. The webhook still receives structured events immediately unless its
own category override disables them.

### Daemon restart

Surfaces render Unknown/disconnected, never Idle. The toolbar shows `!` and the
inbox keeps its reconnect banner and disabled mutations. On reconnect, stale job
rewinds do not update `last_forward_at` as if they were success. Browser-local
tab/cooldown facts remain hidden until holder-generation reconciliation proves
them; daemon effect/human-surface capacity remains authoritative.

Because notification hold/coalescing watermarks are durable, restart does not
replay an entire batch of notices or forget a quiet-hours digest.

### Overnight completion during configured quiet hours

A 60-paper batch reaches a quiescent checkpoint at 02:00 while quiet hours are
`22:00-07:30`: 56 are acquired and 4 genuinely need the researcher. No
notification appears at 02:00. At 07:30 one held checkpoint says
`60 papers · 56 acquired · 4 need you`. The exact events and open Actions rows
were visible throughout; a later final summary requires those four to settle.

### Fast triage and undo

The researcher dismisses three rows. The rows disappear immediately but no RPC
is committed for six seconds. The single strip says `Dismissed 3 items · Undo`.
Pressing `u` restores all three and their ranked placement. A second local
acknowledgement cannot replace a live undo promise; it queues/collapses. If the
commit fails, the rows return with persistent row errors.

### Unsupported/old daemon

An older daemon exposes no `work_pulse_v1`, Activity page v2, counts schema v3,
or triage snapshot schema 5. The extension retains current snapshot/count
behavior, labels the badge as `pending items`, omits exact family totals, states
that live global progress is unavailable, and labels Activity as limited to the
latest 50 when a gap may exist. No old peer receives an unknown field/message.

## Implementation plan

### Phase 0 — Ratify the contract

1. Add the next ADR under `dev/adr/` covering:
   - the five-surface responsibility split;
   - attention/category/persistence separation;
   - daemon ownership of desktop notifications and explicit rejection of
     `chrome.notifications` for this version;
   - notification categories/default routing and webhook independence;
   - the solicited pulse read model and preservation of ADR-0005;
   - the ADR-0001 badge amendment from pending inventory to effective required
     turns, with a user-selectable legacy/all/off count;
   - Activity watermark/page semantics;
   - privacy-minimal focused-surface presence;
   - durable batch cohort authority.
2. Update the curated public ADR summary in
   `docs/contributing/architecture-decisions.md` in the same change.
3. Freeze wire names, schema versions, bounds, and fallback behavior before
   editing Go/TypeScript parsers.
4. Freeze the holder-arbitration class of every new frame:
   - `surface_presence_v1`, `work_pulse_v1`, and Activity page v2 are
     holder-independent, non-authoritative reads/hints admitted before
     holder-only dispatch;
   - `page_bulk_submit_v2` inherits v1 page-bulk admission;
   - handoff-driving traffic remains holder-only.
   Require two-session tests for holder, non-holder, outdated holder, and
   outdated non-holder behavior for each new message type.
5. Record the event-category, route-class, family-action, family-run,
   row-to-run, and arbitration exhaustiveness obligations as tests, not
   comments.

Acceptance: the ADR contains no claim that a desktop notification was delivered,
that a routed paper is accessible, that a scheduled retry is an ETA, or that
attention is severity.

### Phase 1 — Fix the loudest notification behavior

Target files:

- `internal/notify/notify.go`, `event.go`, `fanout.go`, the new router/ledger,
  and focused tests;
- `internal/bootstrap/bootstrap.go`;
- `internal/app/app.go`, `action_reminder.go`, focused tests;
- watch and retraction producers/tests;
- `internal/config/config.go`, defaults/validation tests;
- a store migration for notification intents plus minimum cohort attribution,
  and the three hard-coded schema-version assertions named in `AGENTS.md`;
- `internal/store/activity.go`/`activitytext.go` for system feedback audit events;
- CLI registration/conformance for `papio notify`.

Work:

1. Introduce the closed category vocabulary and exhaustive producer mapping,
   including typed `watch.disabled`, terminal-job, and retraction-scan events.
   Keep webhook `event_kind` separate from routing category.
2. Add durable notification intents/reservations with phase/window identity,
   separate desktop/webhook legs, pending/held/reserved states, and the closed
   terminal desktop dispositions rather than treating Activity events as an
   operational outbox. Activity remains the audit view.
3. Persist the minimum cohort/job attribution needed before terminal routing.
   Disable the old per-import desktop path until that identity exists; then emit
   standalone outcomes and accumulate cohort members.
4. Build one router shared by app/watch/retraction producers. Add platform
   capability selection before reservation so unsupported local senders are
   explicit while webhook routing remains live.
5. Compose all due action-reminder classes into one bounded digest while
   preserving oldest age and recovery-specific copy.
6. Add per-category coalescing, atomic global rate reservation, optional quiet
   hold/drop, default preset/override resolution, and state-episode dedupe.
7. Carry explicit retraction `scan_id` plus scan flush, and watch run/episode
   identity, through durable routing.
8. Record normalized attempted/held/digest audit events as system-scoped
   Activity entries. Do not call them delivered.
9. Apply terminal-control stripping before platform notification text is
   constructed.
10. Add platform capability reporting to doctor.
11. Add `papio notify show`, `preview`, and explicit `test`; register their JSON
   contracts in CLI conformance and regenerate reference docs through
   `go run ./cmd/docs-gen`.

Behavioral proof:

- Feed 200 cohort imports and verify zero per-paper desktop attempts; feed one
  standalone terminal job and verify one job-aware outcome.
- Feed all six reminder classes in one pass and verify one digest retaining the
  class breakdown/oldest age.
- Race concurrent producers at the last rate slot, restart after reservation,
  and verify the rolling ceiling and no replay.
- Restart with held/coalesced intents and verify quiet-window recovery.
- Feed multiple watch runs and retraction scans; verify explicit scan/episode
  boundaries, category caps, and unchanged webhook event kinds/structured data.
- Verify sender/webhook/intent-write failures never alter domain work and
  unsupported local platforms do not consume the desktop rate budget.

### Phase 2 — Repair inbox information architecture

Target files:

- `extension/src/inbox.ts`, `inbox.html`;
- `extension/test/inbox.test.ts` and DOM constructor whitelist;
- shared presentation helpers only if popup/inbox genuinely reuse pure copy.

Work:

1. Remove generic required/advisory rails and working opacity.
2. Render Required as action-first baseline and Working as explicit full-opacity
   `papio is continuing` copy without a primary decision control.
3. Give every row control an item-specific accessible name and connect its
   row-local guidance through an explicit description relationship.
4. Generalize the undo bar into the one visible feedback strip without creating
   a second announcer; keep persistent errors on rows.
5. Add explicit dismissal-commit state and route focus/visibility/count/timer
   refresh through the same mutation guard.
6. Turn the Activity list live region off, remove per-row `role=status`
   announcements during polling, and add the explicit new-entry affordance.
7. Fix current glyph-tooltip behavior if the existing hover/focus rule still
   leaves the pseudo-element invisible; glyph meaning must be available on
   keyboard focus as well as hover.
8. Preserve retraction/error domain styling and existing dialog/keyboard/focus
   contracts.

Behavioral proof:

- Render mixed Required/Working/Advisory rows; assert Working has honest copy and
  no primary action, Required is not marked as danger, and retraction identity
  remains explicit.
- Give every row operation an item-specific accessible name and description;
  no family grouping or exact total appears before snapshot schema 5 is
  negotiated in Phase 4.
- Exercise fast multi-row dismissal, undo, commit failure, filtering, paging,
  keyboard navigation, reduced motion, narrow layout, and screen-reader live
  regions in the real full-page UI.

### Phase 3 — Build durable cohorts and pulse projections

Target files:

- a store migration for acquisition batches/members and the three hard-coded
  schema-version assertions named in `AGENTS.md`;
- CLI/browser batch submission code and tests;
- `internal/triage` or a co-located `internal/pulse` read model;
- `internal/api` handler/method registration;
- `internal/browser/bridge.go` and bridge tests;
- `internal/protocol/protocol.go`;
- `extension/src/protocol.ts`, `background.ts`, `page-bulk.ts`, and focused
  tests;
- `extension/src/page-bulk-recovery.ts`;
- `protocol/browser-v1.schema.json` and fixtures.

Work:

1. Complete browser and CLI cohort membership/outcome persistence begun by the
   notification substrate: scanner continuation, joined-job following,
   membership health, settlement timestamps, and privacy-bounded labels.
2. Add cohort settlement projection and latest active/recent cohort selection.
3. Add `work_pulse_v1` as new feature-gated message types; do not widen an
   existing response.
4. Derive effect/human-surface capacity and future actions from the
   institutional plan's authoritative permits, gate owners, leases, retry
   timestamps, delivery polls, and source gates. Exclude rewinds from forward
   progress and emit authoritative `last_finished_at`.
5. Implement `work.pulse_v1` plus one-shot `papio pulse [--json]` with a
   structured-object conformance classification and explicit unavailable result.
6. Add strict Go/TS/schema validation for optional/unknown measurements,
   membership health, maximum list/count bounds, feature advertisement, the
   exact hello-ack assertion, and old/new skew.
7. Keep the request solicited and on existing poll cadence.
8. Add the dedicated 24-hour browser-local cohort manifest/replay store and
   caller-owned request-ID path. Simulate suspension and full browser restart
   after a daemon commit whose acknowledgement is lost; prove the identical
   chunk is retried, its cached result is applied once, and later chunks do not
   advance early. Malformed, expired, future-dated, and digest-mismatched records
   send no frame and disclose no source or identifier in their bounded error.

Behavioral proof:

- A currently executing correlated handler or current effect permit -> Moving;
  an unexpired but inactive materialization claim -> no positive bucket; future
  retry/gate -> Scheduled; required-only -> Waiting on you; no work under a
  complete projection -> Idle; incomplete claim, unavailable/stale transport ->
  Unknown.
- Long valid future waits never classify as stalls, and a stale lease rewind
  does not refresh forward-progress time.
- Batch joined/owned/invalid members produce an exact, mixed settlement summary.
- A failed membership write removes the denominator rather than inventing one.
- Old extension/new daemon and new extension/old daemon combinations remain
  connected and render their explicit fallbacks.

### Phase 4 — Integrate popup, inbox pulse, badge, and Activity paging

Target files:

- `extension/src/background.ts`, `state.ts`;
- `extension/src/popup.ts`, `popup.html`;
- `extension/src/inbox.ts`, `inbox.html`;
- `extension/src/options.ts`, `options.html`;
- extension tests for all four surfaces.

Work:

1. Fetch/merge pulse data on existing popup/inbox timers and apply freshness,
   skew, holder-generation reconciliation, and browser-local tab/cooldown rules.
2. Replace global popup liveness derived from Activity text/legacy ten-minute
   heuristics with the typed pulse. Retain typed current-page job state as the
   only richer local view; older daemons show global liveness unavailable.
3. Add compact popup/header pulse, latest-batch line, deterministic blocker
   grouping, and `N more in inbox` overflow.
4. Add bounded catch-up using a read watermark distinct from notification
   attempt state.
5. Add Activity page v2/cursor and `activity_seen_through_seq`; acknowledge
   through authoritative `latest_seq` only after a visible newest-page render,
   and expose exact raw-entry `new_count_since` versus `gap`.
6. Add counts schema v3 plus triage snapshot schema 5, exact family-run
   breakdowns, row-to-run identity, repetition-aware family rendering, and
   daemon-owned required-turn badge calculation. Join `gate_claim_id` only to
   reconciled physical focus/platform state; the extension never re-elects
   institutional ownership. Connect hoisted guidance to every member's controls
   through the explicit accessible-description relationship.
7. Suppress identical badge API writes; update tooltip with the complete
   breakdown.
8. Add browser-local feedback settings and read-only daemon routing summary.
9. Add `surface_presence_v1` end to end: surface lifecycle emitter, existing
   poll heartbeat, background allowlist/dispatch, wire request, bridge handler,
   per-instance lease store, feature negotiation, and old-peer suppression.
10. Persist popup operation state in the render model; preserve stable-key focus
    and disabled/pending controls across five-second refreshes.

Behavioral proof:

- Open the popup during idle, moving, scheduled, waiting, disconnected, stale,
  and active-batch states; copy never contradicts the underlying snapshot.
- Open popup on unrelated/current-job/PDF pages and verify the current-page
  action remains primary.
- Leave/reopen inbox with more than 50 events; cursor through Activity, place the
  “since” divider correctly, and avoid replaying old entries as toasts.
- One sign-in plus three waiting siblings badges one required turn.
- Watch hits/retractions change tooltip/inbox but not the default badge number.
- Render one 39-row family, then two non-contiguous runs of 20 and 19 separated
  by one exception, including a run crossing a page boundary. Assert one heading
  for the contiguous case, distinct stable run keys and exact totals for the
  split case, preserved daemon rank, and no total when completeness is false.
- A focused surface suppresses duplicate daemon desktop attempts only for the
  feature’s short TTL; privacy tests prove no page metadata crosses the wire.
- A keyboard-focused popup control survives a poll, each asynchronous blocker
  action remains visibly pending and cannot duplicate, and a removed row moves
  focus to one bounded announced fallback.

### Phase 5 — Documentation, release evidence, and cleanup

1. Update `docs/reference/config-reference.md` by hand for `[notify]`, including
   strict-mode deployment and webhook-vs-human routing.
2. Regenerate `docs/reference/commands.md`, `llms.txt`, and `llms-full.txt` via
   `go run ./cmd/docs-gen`; do not hand-edit generated command docs.
3. Update the user guide with the five-surface model, notification presets,
   badge options, batch pulse language, Activity read state, and platform
   capability limits.
4. Update `docs/privacy.md` for `surface_presence_v1`: focused surface type and
   timestamp only, local daemon only, no page metadata.
5. Update root and extension changelogs in their source files, not snippet pages.
6. Build docs with pinned Zensical and require `No issues found`.
7. Build both extension targets, inspect generated manifests, and manually verify
   that no `notifications` permission was added.
8. Exercise the built Chrome extension and Firefox build in real UI surfaces.
   Confirm narrow/wide layouts, dark mode, reduced motion, focus/keyboard paths,
   popup dismissal behavior, inbox reconnection, badge/tooltip, and Activity
   paging.
9. Run scoped Go tests, extension typecheck/tests/build, then the repository’s
   final applicable build/vet/docs gates once.
10. When shipped, salvage permanent decisions into the new ADR and delete this
    active plan; Git history is the archive.

## Required automated coverage

### Go

- Config defaults, preset layering, override validation, strict unknown category
  rejection, quiet-hours parsing/DST/overnight windows, and rate bounds.
- Router category exhaustiveness, structured event preservation, coalescing,
  quiet hold/drop, rate ceiling, state-episode dedupe, restart recovery, and
  non-fatal channel errors.
- One reminder digest across all recovery classes with existing backoff and
  quiescence unchanged.
- System Activity event insertion/formatting and recursion exclusion.
- Batch membership, joined/owned/invalid cases, settlement, privacy-bounded
  label, and failed-membership degradation.
- Pulse state facts, lease expiry, scheduled waits, next action, rewinds, clock
  skew, and bounds.
- CLI envelopes/conformance/docs drift for all new commands/methods.
- Protocol valid/invalid/round-trip/size/feature/skew tests in Go and TS.

### Extension

- Badge precedence remains intact; numeric source changes only under schema v3;
  all/required/off settings and old-daemon fallback.
- One typed gate/claim with dependent siblings counts as one turn across badge,
  popup, and inbox; two profiles remain independent.
- Pulse rendering for every state, stale/unknown, browser reconciliation, and no
  invented percentage/ETA/position.
- Popup section order, current-page primacy, catch-up bounds, and inline action
  feedback.
- Task-family exact-guidance split, rank/filter/pagination behavior, Working
  controls, retraction presentation, empty states, and responsive layout.
- Toast/undo priority, timing, queue/collapse, persistent failures, single
  announcer, focus, and reduced motion.
- Activity pagination/watermark, visible-tab read semantics, no background mark,
  new-divider behavior, and `aria-live` policy.
- Presence payload privacy and TTL behavior.

## Manual acceptance checklist

- Run a representative 50–200-paper cohort. Observe one batch completion
  summary, no per-paper success notifications, honest pulse transitions, and an
  exact final mixed outcome.
- Produce at least two simultaneous institution profiles and waiting siblings.
  Verify counts/copy never leak one institution’s session state into another.
- Produce a security cooldown, source-budget wait, future delivery poll, and real
  required action. Each must render with the correct actor and next-action copy.
- Leave the browser/daemon asleep or restart them mid-run. No stale zero, false
  Idle, notification replay, duplicate tab work, or lost durable outcome.
- Let >50 Activity events accumulate while away. Reopen and recover everything
  through paging without a live-region storm.
- Verify current macOS notification copy using the explicit test command; verify
  unsupported platform capability is visible through doctor rather than silently
  implied.
- Verify webhook payloads preserve structured watch/category data and remain
  independent of desktop quiet/rate policy.
- Verify no desktop/toast/badge setting can resolve, retry, open, or otherwise
  mutate a human action automatically.

## Explicit non-goals and revisit triggers

Do not build in this work:

- `chrome.notifications`, notification buttons, or click-through;
- a daemon push/subscription/change-version mechanism;
- progress percentages, success ETAs, queue positions, animated toolbar state,
  or a five-stage ladder inferred from free text;
- per-paper success toasts/desktop notices;
- stacked toasts or popup floating toasts;
- notification severity levels derived from `attention`;
- family-level `Open all`, `Dismiss all`, or mass confirmation;
- a new daemon-served web UI or side panel;
- a general browser editor for daemon TOML;
- telemetry or hosted notification delivery.

Revisit browser-owned notifications only when all are true:

1. click-through measurably reduces unresolved required actions;
2. the browser channel replaces the daemon desktop channel for an explicitly
   claimed session rather than duplicating it;
3. durable identity/deduplication survives worker/browser restart;
4. platform permission denial has an observable recovery path; and
5. Chrome/Firefox/macOS behavior has been exercised, not inferred.

Revisit live push only under ADR-0005’s existing triggers: genuinely
seconds-sensitive per-job progress while a person watches, or multi-device/shared
inbox writes that local polling cannot observe.

Revisit a stage ladder only after a stable typed stage model exists across all
acquisition branches. Until then, authoritative effect capacity, next automatic
action, Activity outcomes, and cohort settlement are the honest vocabulary.

## Completion criteria

This plan is complete only when:

- a 200-paper run produces bounded milestone notifications rather than per-item
  chatter;
- the user can tell Moving, Scheduled, Waiting on you, Idle, Stalled, and Unknown
  apart without relying on colour;
- every transient acknowledgement has a durable twin or a still-undoable state;
- the popup remains current-page-first and the inbox remains the sole durable
  decision surface;
- task families reduce repeated copy without hiding exceptions or changing
  daemon rank;
- Activity recovers more than 50 events and does not narrate a poll stream;
- the badge counts effective user turns by default and its fallback is honest;
- notification, pulse, presence, and counts changes remain strict, feature-gated,
  cross-version safe, and privacy-bounded;
- webhook automation remains complete and independent;
- all affected docs, changelogs, generated references, dual extension bundles,
  and scoped/full verification gates are clean.
