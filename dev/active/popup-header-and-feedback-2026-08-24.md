# Popup header consolidation, acquire truthfulness, and proactive feedback

Status: plan, reviewed, ready to build (2026-08-24)

Eight operator concerns from a live Sage Journals session. Reviewed by three
agents before any code was written; every reviewer blocker is resolved below,
and the three rejected demands are recorded with reasons in §5.

Every `file:line` and `file:Symbol` citation was resolved against the tree.
`ADR-0023:N` is shorthand for
`dev/adr/0023-notification-feedback-and-liveness-surfaces.md:N`.

---

## A. Institution session is blind to a publisher-initiated sign-in

### What is true now

The verdict is computed in the extension. `classifyResolverMarkers`
(`extension/src/keepalive.ts:353`) returns `in`, `out`, or `unknown` from two
inputs only: sign-in and sign-out affordances in a page's DOM, and an unexpired
identity JWT in that origin's `localStorage` or `sessionStorage`.

The readable set is closed on purpose:

- `resolverOrigin` is "The configured resolver origin, **never** an
  authentication/IdP URL" (`keepalive.ts:137-138`).
- `isConfiguredMember` (`keepalive.ts:844`) records that an offer, a persisted
  origin, or a granted permission "may **SELECT** among these, but none of them
  may **WIDEN** this set (verified defect #5)" (`keepalive.ts:840-843`).
- `MAX_OBSERVED_TABS_PER_ORIGIN = 5` (`keepalive.ts:547`) bounds one probe.
- `SESSION_STALE_MS = 2 * 60_000` (`keepalive.ts:544`) is a display-trust budget,
  explicitly not a probe gate.
- No `cookies` permission. `permissions` is exactly `nativeMessaging, activeTab,
  tabs, downloads, scripting, storage, alarms, tabGroups, webNavigation`
  (`extension/manifest.json:18-28`); standing `host_permissions` is the two Ex
  Libris wildcards plus `login.openathens.net` (`manifest.json:29-33`).

papio learns from a warm institutional landing only inside a tab it opened for
one of its own jobs: `emitSessionEvidence` (`background.ts:14304`) called with
`"auth_returned"` at `background.ts:17094`, beside `recordInstitutionalSession`
(`background.ts:14062`). `entitled_landing` is claim-scoped and the daemon
applies it per claim, per binding, per gate occurrence (`bridge.go:2835-2908`).

### Rejected: the article-classification hook (was proposal A1)

The first draft proposed triggering a resolver re-probe from the adapter's
`article` classification. **That path never fires on the operator's tab.**
`maybeClassify` requires `findByJob` and an accepted or awaiting job
(`background.ts:17713-17727`), and its comment forbids touching a tab not owned
by that job (`background.ts:17680-17683`). `onTabUpdated` returns early for an
untracked tab (`background.ts:16725-16747`).

Worse, even a tracked trigger would not fix the symptom.
`probeOriginAutomatically` only calls `requestProbe` (`keepalive.ts:1124-1130`),
and `probeOrigin` queries *existing* resolver tabs (`keepalive.ts:1290-1332`).
With no resolver tab open it commits `no_tab`, preserves the prior verdict, and
releases nothing (`keepalive.ts:1504-1538`). A prior fresh `out` still renders
"Signed out or expired" (`popup.ts:1397-1406`).

### A1 (revised) — a dedicated observer for the untracked tab

Two parts, both required:

1. **An observer that runs on tabs papio does not own.** The existing
   classification path is job-scoped by design and must stay that way. This
   needs its own permission-scoped hook with an institution mapping.
2. **A manager-level configured-origin guard.** `isConfiguredMember` gates
   `onFreshSessionEvidence` *after* observation (`keepalive.ts:843-845`,
   `1605-1609`); it does not gate observation. `probeOriginAutomatically` only
   normalises its argument (`keepalive.ts:1124-1130`), and `requestProbe` checks
   only the in-flight slot and spacing floor (`keepalive.ts:1169-1180`). A
   caller passing a provider host would inject into provider pages. Reject
   non-members before querying.

Deliberate non-goal: the publisher page is never evidence. "Available access" on
Sage can mean open access, so reading it as entitlement would manufacture a
false `in`. The page supplies timing only.

### Declared intended: queued release on fresh evidence

A decisive resolver `in` fires `onFreshSessionEvidence`
(`keepalive.ts:1557-1609`) → `recordFreshSessionEvidence`
(`background.ts:14351-14364`), which drains origin-scoped queued handoffs and
reloads authentication tabs. So a new trigger is also a release trigger.

**This is the feature, not a hazard.** papio observing a live session and then
declining to release queued institutional work would be a pure automation
regression. Declare it and test it. Do **not** add a non-authorizing probe
reason. Keep the claim-scoped `entitled_landing` path separate
(`background.ts:18959-18963`).

### Probe budget

The bound is per origin: `probeInFlight`, `pendingProbes`, and
`lastProbeStartedAt` are keyed by origin (`keepalive.ts:765-768`) and repeated
requests coalesce by that key (`keepalive.ts:1169-1210`). So one origin allows at
most 6 automatic starts per minute and up to 30 injections per minute, even from a
route-changing SPA. The shared `requestProbe` floor is sufficient; no
source-specific floor is needed. Across N configured origins the aggregate is
6N starts and 30N injections per minute, so an untracked provider event must
**never** fan out to every configured origin.

### A2 — cookie-scoped observation (follow-up, not this pass)

Blocked on specifics the plan cannot yet state: `login_entity_id` is an entityID
URL, not a cookie host, and generic cookie presence is not session liveness once
partitioning and `SameSite` are considered. If pursued, the grant must be
optional with an options-page privacy disclosure. `extension/build.ts` spreads
the manifest to the Firefox target unchanged (manifest spread at
`build.ts:106-107`, write at `build.ts:123`), so any permission lands in both.

### A3 — daemon profile evidence (bounded third option)

`bridge.go:7150-7280` records profile evidence with a 60-second throttle. Useful
for facts already recorded; it cannot see a visit papio never claimed.

---

## B. The `+` reports "Added to papio" for work already in papio

### What is true now

- `pageAcquireStatus` (`popup.ts:2328`) renders "Already in papio" only when
  `duplicate === true`, otherwise "Added to papio" (`popup.ts:2335-2337`); the
  button label matches (`popup.ts:3538-3540`), compact form at
  `popup.ts:3163-3164`.
- Ack payload is `{job_id?, duplicate?, error?}` (`extension/src/protocol.ts:158-161`).
- The daemon sets `Duplicate: true` only when `liveJobForRequest` matches a
  stored `work_request_id` (`bridge.go:5445-5455`, query at `bridge.go:5494-5507`).
- That id is **route-specific**: `"page_acquire_" + sha256("doi:" + doi)`
  (`bridge.go:5488-5490`). A job from Zotero sync, the CLI, a batch, or the bulk
  scanner has a different `work_request_id`, so the lookup misses. The operator
  arrived from the inbox.
- `SubmitAs` discards the app layer's correct canonical-work answer.
  `CreateRequestForWork` (`internal/job/job.go:474`) consults
  `liveJobForCanonicalWork` (`job.go:502`, defined `job.go:607-621`) and returns
  `Existing` (`internal/app/app.go:266-271`), but `pageAcquire` calls
  `b.svc.SubmitAs` (`bridge.go:5459`), which drops it (`app.go:192-198`).

### B1 (revised) — canonical preflight, no wire change

**Do not widen `page_acquire_ack`.** The stated fix needs only the existing
`duplicate` boolean: `pageAcquireStatus` already maps it to "Already in papio".
Widening would be actively harmful — `parseBrowserMessage` rejects unknown ack
fields (`protocol.ts:1761-1795`, ack case `3267-3295`) and `onInbound` catches
`ProtocolError` and calls `disconnect` (`background.ts:15527-15532`), so an older
extension receiving a new field **tears down its native port**. That is not an
ignored field; it is a session teardown.

Four constraints, each from review evidence:

1. **Cover `imported`, not just `ready`.** `canonicalJobStatus` records only
   `StateReady` (`bridge.go:5903`), but `imported` is terminal too, and today's
   route-keyed SQL excludes only `failed`, `cancelled`, `unavailable`
   (`bridge.go:5497`) — so the current check already treats an imported paper as
   a duplicate. A naive swap would **lose** that and report "Added to papio" for
   a paper already filed in Zotero. Duplicate suppression covers live, ready,
   and imported.
2. **Never short-circuit on `previouslyUnavailable`.** `canonicalJobStatus`
   returns it, but ADR-0019 Decision 5 treats it as history, not exclusion:
   `recordPageBulkScan` folds `previously_unavailable` into `eligible`, "the
   bucket that already means still selectable" (`bridge.go:5623-5628`). Blocking
   a retry on a past failure would regress exactly the case the operator is
   asking papio to try again.
3. **Bound the lookup.** `canonicalJobStatus` (`bridge.go:5885-5914`) has no
   `LIMIT`, joins `identifiers` to `jobs`, and sorts by `created_at`; there is no
   `jobs(work_request_id)` index (`internal/store/migrations/0001_init.sql`).
   `Bridge.Sync` holds `b.mu` for the whole handler (`bridge.go:1057-1059`) and
   the IPC server applies `DefaultConnIdleTimeout = 30 * time.Second`
   (`internal/ipc/protocol.go:36`, applied `internal/ipc/server.go:197-238`). An
   unbounded query on a large forced-history store can therefore delay the
   response past the transport deadline, which is session-fatal. Add the index
   and a bound.
4. **Every failure through `pageAcquireError`.** The existing handler maps live
   lookup errors to a structured ack (`bridge.go:5437-5446`, `5509-5516`). The
   canonical call must do the same on every error path and must never return a
   raw Go error, and must not mislabel an error as an ownership verdict.
5. **Consume `Existing` at submit, not just at preflight.** The preflight closes
   the common case but not the window between preflight and submit: a CLI,
   batch, or second-browser submission landing in that gap still reports "Added
   to papio". `CreateRequestForWork` already returns the right answer
   (`app.go:266-271`) and `job_test.go:1667-1700` proves sixteen simultaneous
   submissions of one work yield one job and exactly one `existing=false`. So
   call `SubmitWithOptionsAs` instead of `SubmitAs` and map `Existing` to
   `Duplicate`. The preflight stays, because canonical dedupe skips terminal
   states and therefore cannot see a `ready` or `imported` paper at all.

**Copy.** "Already in papio" is accurate for a live job. For a terminal
artifact, ADR-0008 Decision 5 fixes the wording exactly: report "*papio*
already has a validated artifact", **never** `[in library]`
(`dev/adr/0008-holdings-claims-for-non-zotero-deduplication.md:105-113`), because
a `ready` job proves papio validated a PDF and nothing about a manager receiving
it — auto-import can be disabled or fail while the job stays `ready`
(`internal/app/app.go:3222-3248`), and only a durable Zotero apply reaches
`imported` (`internal/zotio/plan.go:427-444`). `Terminal` covers `StateReady`
and `StateImported` (`internal/job/job.go:195-201`).

**Double-click idempotency is preserved.** `Bridge.Sync` serialises the frame
loop (`bridge.go:1057`), so two header clicks run in order and the first commits
before the second preflight; canonical DOI identity then reproduces what the
route key gave. `liveJobForRequest` is **not** removed globally — `internal/job`
uses its own (`job.go:494`, `602-605`) for app dedupe, and
`internal/zotio/service.go:434` has an unrelated namesake.

**Rejected alternative: reuse `page_bulk_status_request` for one key.** Not
wire-free. The popup cannot call `papio.pageBulk.status` — the background sender
gate requires the exact page-bulk URL (`background.ts:20518-20538`). It would
need a new popup route, a scan id, two round trips, a `page_bulk_runs`
measurement row (`bridge.go:5615`), and it imports bulk holdings semantics
(`ownership_incomplete`, `ownership_unknown`, `owned_missing_pdf`) that have no
single-DOI meaning.

---

## C. Proactive feedback: route it through the daemon, amend nothing

### The motivating case is already automated

`onTabRemoved` (`background.ts:19449-19639`) never waits for the operator:

| Branch | What papio does | Code |
| --- | --- | --- |
| `waiting_for_session` | re-queues to `queued`, schedules release | `background.ts:19600-19614` |
| delivery in flight | detaches the tab, keeps the download | `background.ts:19616-19625` |
| `awaiting_download` | parks; the daemon poll adopts the file | `background.ts:19626-19633` |
| everything else | `provider_outcome: cancelled`, then re-drains | `background.ts:19635-19638` |

The first branch states the policy: a tab closing "is not the operator
abandoning the job, just losing the page it was quietly waiting on. Re-enter it
as an ordinary queued drive" (`background.ts:19594-19599`). `removeJobWithOffer`
re-drains immediately (`background.ts:13209-13215`).

So a "reopen?" prompt would offer to undo a decision papio already made and
acted on. **The gap is that the recovery is silent, not that it is absent.**

### "Reopen" is not representable on the institutional path

`owner_closed` is terminal for the claim: it abandons the materialization claim
(`internal/job/claim_observation_apply.go:240-243`), retires the
authentication-entry lease (`internal/job/institutional_evidence.go:1601-1604`),
and consumes the one-use close authorization
(`internal/job/claim_observations.go:245-248`). A dependent then "stays tabless
until the daemon observes entitled_landing or grants a fresh arbitration"
(`bridge.go:11167-11171`), pinned by
`internal/browser/authentication_claim_test.go:1740-1755`.

**Correction to the first draft.** It claimed the daemon already mints a fresh
route by job id for this case. It does not: `handoffLink` routes through
`WithOpenRouteJob`, restricted to `openurl_handoff` and `manual_download`
(`internal/job/job.go:3439-3441`). A closed sign-in surface is neither. That
same function records the boundary: it "is deliberately NOT the accessor the
offer path uses — an offer opens a tab by itself, and papio must never do that
for a paper it asked a human to fetch" (`job.go:3437-3438`).

### Rejected: `chrome.notifications`

Not on ADR grounds alone — on concrete defects. A browser send consumes zero
reservation slots, so Decision 5's six-per-rolling-hour ceiling becomes `6 + N`
with `N` unbounded and unrecorded (`internal/store/notifications.go:277-292`).
The coalescing branch merges only while
`desktop_state IN ('pending','held')` (`notifications.go:96-104`), so a late
duplicate cannot merge and leaves no audit row. Presence suppression inverts:
the browser holds the very focus lease that silences the daemon
(`ADR-0023:178-179`). And `PlatformCapability()` returns false off darwin
(`internal/notify/notify.go:17-19`), so a browser sender on Linux would have the
ledger record "no platform sender exists" while the platform just delivered.

Also rejected: making the in-page chip interactive. There is no page to inject
into for a closed tab (`popup.ts:569-572`); the injected function must stay
fully self-contained across serialization (`popup.ts:451-455`) so a page-context
click cannot call `chrome.*`; and three assertions pin the inertness —
`popup.test.ts:3956` (no `button, a, input`), `:3957` (`pointer-events: none`),
and `:4219` (exactly one 3000ms timer). A three-second window to find and hit a
button is also a WCAG 2.2.1 failure.

### C1 (revised) — send the browser-side recovery to the existing daemon router

The daemon already learns the tab closed: `owner_closed` arrives as a claim
observation. It simply does not notify.

`Service.Notifier` (`internal/app/app.go:123`) is a `notify.Sink` already driven
by three producers — human-action opened (`app.go:1912-1918`), standalone
outcome (`app.go:3211-3217`), and the pending-action reminder
(`internal/app/action_reminder.go:174-180`). The bridge already supplies that
router with presence data (`bridge.go:509`). Routing a tab-close recovery
through it gives a real OS interruption inside the existing ledger, dedupe
identity, rate budget, quiet hours, and presence suppression.

Decision 3 requires copy that "names a recoverable *papio* surface or CLI
command" and does not promise a clickable action (`ADR-0023:88-89`). "papio is
retrying 3 papers — open the inbox" satisfies that and is honest, because papio
is retrying. Take-back-control is then one click away in the inbox, which
already has a deferred-commit undo bar with a six-second window and a `u`
keyboard binding (`inbox.ts:2804-2809`, `inbox.html:1399-1406`); the commit
waits for the window (`scheduleDismissal` `inbox.ts:2988-3007`,
`commitDismissals` `inbox.ts:3035-3060`), so undo is exact.

Net: proactive interruption, no new permission, no second sender, no wire
change, no ADR amendment.

Producer ordering rule to respect: Decision 4 requires the domain fact durable
before routing, and `window_start` is never router receipt time
(`ADR-0023:131-138`, `152-153`). The daemon side already satisfies this, which
is another reason the producer belongs there and not in the extension, whose
observation outbox drains asynchronously (`background.ts:4388-4390`, `4948`).

---

## 5. Reviewer demands rejected, with reasons

Recorded so a later reader does not reopen them without new evidence.

1. **"Add a non-authorizing probe reason so a provider-triggered probe cannot
   release queued work."** Rejected. papio observing a live session and then
   leaving queued institutional work parked is an automation regression.
   Declared intended and tested instead. The membership guard from the same
   finding is adopted.
2. **"Honour `toolbarCountMode`, or drop the inbox count badge."** Half
   adopted. The preference is honoured (`extension/src/state.ts:61`,
   `background.ts:622-639`, `ADR-0023:232-235`). Dropping the item is refused:
   the operator asked for it.
3. **"Keep `#popup-pulse-review` beside the badge."** Rejected. The operator's
   concern is density, and the count keeps a route through the badge, so the
   affordance is demoted rather than lost. `ADR-0023:39-41` is noted.

---

## 6. The five smaller items

1. **The `+` is not larger.** All three header buttons are `.settings-btn` with
   `width`/`min-height: var(--control-height-compact)` and `padding: 0`
   (`popup.html:229-241`); the token is `32px` (`popup.html:56`). Icons are
   `18px` (`popup.html:59`, `248-252`). `.header-acquire` changes only
   `background`, `border-color`, `color` (`popup.html:257-261`). No change.

2. **Remove the border from the unaccented header buttons.** Use
   `border-color: transparent`, not `border: 0`.
   **Corrected reason:** `border: 0` does not change the box —
   `* { box-sizing: border-box }` (`popup.html:111-113`) means the 32px already
   includes the border. The real reason is that `border: 0` deletes the hover
   affordance the base rule transitions (`popup.html:207-210`) and
   `.settings-btn:hover` reveals (`popup.html:243-246`).

3. **Count badge on the inbox icon.** Source is `counts.turns_required`, rendered
   by `renderWorkPulse` (`popup.ts:2651`) from `derivePulseDisplay`
   (`popup.ts:2481`). Three requirements:
   - **Honour `toolbarCountMode`** (`state.ts:61`, options at `options.ts:265-266`):
     `required`, `all`, `off`.
   - **Gate on `required_turns_complete`** (`extension/src/protocol.ts:434`,
     validated `protocol.ts:1965-1970`). `computeBadge` honours it
     (`background.ts:659-664`); `derivePulseDisplay` does not
     (`popup.ts:2542-2546`). Never invent a precise number
     (`ADR-0023:234-236`).
   - **Cap the display.** The wire validates `turns_required` up to 1,000,000
     (`protocol.ts:1953-1956`). A 32px pill fits one digit at the smallest token,
     `--font-size-label: 11px` (`popup.html:38`), and `.header-actions` gap is
     `4px` (`popup.html:597-601`), so a corner badge collides with its neighbour.
     Either render `99+` with the exact number in `aria-label`, or let the inbox
     control become a `width: auto` pill showing `[icon] 12`. Room exists: brand
     plus three 32px pills plus two gaps is about 236px of a 356px content box
     (`--popup-width: 380px`, `popup.html:60`; body padding `popup.html:140-141`).

   The pulse card stays, conditional. Its six primaries are `Unknown`, `Moving`,
   `Waiting on you`, `Stalled`, `Scheduled`, `Idle` (`popup.ts:2490`, `2500`,
   `2528-2533`), and it also carries `next` and `capacity` lines
   (`popup.html:989-990`).

4. **Collapse "Select papers on this page" into the rail, with no menu.**
   `isBulkScannablePage` tests only `scannerOriginForBinding(binding) !== null`
   (`popup.ts:3648-3649`), which is why it appears on a single article.
   - **Do not extend `hoistIdleAcquire`.** The header control exists only while
     the rail acquire button is visible and enabled (`popup.ts:3184-3185`), and
     acquire is hidden while a job for that DOI is in flight
     (`popup.ts:3866`). Hanging bulk selection off it makes "add all papers"
     vanish mid-acquisition — a functional regression. Derive header visibility
     from `isBulkScannablePage` **or** hoistability.
   - **Do not build an overlay menu.** Chrome sizes the popup window to content,
     and an absolutely positioned menu adds no layout height
     (`popup.html:119-128` records the measured oscillation from
     viewport-relative lengths); it would be clipped. `position: absolute`
     appears once in the file, on `.visually-hidden` (`popup.html:551`).
   - **Adopted design.** When a DOI is bound, keep "Acquire" as the rail action
     and demote bulk selection to a text link in the same rail card; when no DOI
     is bound, bulk selection is the rail's only action and keeps its full label.
     One conditional in `renderPageContext`. This keeps both actions beside the
     consent prompt and the result, which live in the same section
     (`popup.html:1038-1045`) and bring the card back anyway once a scan produces
     a result (`railOwnsSomething` counts `scanStatus`, `popup.ts:3748`). The
     rail exists for that adjacency (`popup.html:347-349`). It also preserves the
     single-Enter-target model (`markPrimaryRailAction`, `popup.ts:3655-3670`)
     and the synchronous gesture forward (`popup.ts:3504-3507`), which matters
     because `chrome.permissions.request` must be reached inside the click
     gesture (`popup.ts:151-153`, `options.ts:145-146`).
   - **Icons.** Keep the bare plus for single-paper add (`popup.html:960-963`);
     at 18px it is the only unmistakable add glyph. For multi-paper add use
     stacked layers with a plus (Lucide `copy-plus`); `list-plus` is
     indistinguishable from the existing inbox tray outline
     (`popup.html:967-968`) at that size. Avoid `folder-plus` and `library`.
   - **Build with `createElement`/`textContent`:** `extension/build.ts:34-36`
     throws if a page bundle contains `innerHTML`.

5. **"Capture fixture (dev)" never reaches store users.** The release build
   deletes the markup (`build.ts:47`), fails if the removal did not happen
   (`build.ts:48-50`), and asserts the output matches the build kind
   (`build.ts:40-42`). `wireDevTools` (`popup.ts:4288`) also hides it whenever
   the manifest has an `update_url` (`popup.ts:4301-4306`). Dev-ness is
   `PAPIO_CAPTURE_TOOLS=1` or daemon version `0.0.0-dev` (`build.ts:21-23`). No
   work. **Do not reformat `popup.html`:** the release regex is keyed on exact
   indentation of that `<details>` (`build.ts:24`).

---

## 7. Popup density: what this pass does not fix

The markup holds twelve independently gated surfaces. Four route to the inbox:
`#open-inbox-btn` (`popup.html:965-970`), `#popup-pulse-review`
(`popup.html:1004`), `#popup-catchup-open` (`popup.html:1102`), and
`#page-acquire-open-inbox` (`popup.html:1053`).

Two genuinely compete. `#popup-pulse` renders `Waiting on you · N decisions`
(`popup.ts:2546`) and `#popup-catchup` renders `While you were away: N updates`
(`popup.ts:2818`) — two counts from two authorities, in cards that share the
`popup-pulse` class (`popup.html:1100`), each with its own inbox button.
`#popup-catchup` is gated on unseen Activity sequence numbers
(`popup.ts:2806-2809`), which is read state, not a turn.

Separately, `#impact-summary` shows lifetime `acquired · success` whenever
`stats !== null` (`popup.ts:2278`, `2283`), including directly under a live
blocker count.

Card arbitration is the real density problem. It is out of scope for this pass
and should be its own note.

---

## 8. Validation status

- **AMO: `Users: 0`**, version 0.14.0 — read live 2026-08-24.
- **Chrome Web Store: no user count displayed**, version 0.14.0, updated
  14 August 2026, 0 ratings. Absence of a number, not a stated zero; the
  authoritative figure is the CWS developer dashboard. **No longer blocking:**
  B1 keeps the existing `duplicate` boolean and does not widen the ack.
- **`derivePulseDisplay` primaries:** enumerated, six values (§6.3).
- **Still open:** whether `webNavigation` events require a host permission for
  the reported URL (affects A2's alternatives), and the exact IdP cookie host
  for A2.

## 9. Build order

1. **B** — canonical preflight covering live, ready, and imported; keep the
   `duplicate` boolean; index and bound the lookup; every failure through
   `pageAcquireError`; never short-circuit on `previouslyUnavailable`.
2. **6.2, 6.3, 6.4** — CSS and rail changes.
3. **A1** — dedicated observer plus configured-origin guard; queued release
   declared intended and tested.
4. **C1** — route the tab-close recovery through `Service.Notifier`.
