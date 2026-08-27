# Popup UX: acquire truthfulness, recovery legibility, header consolidation

Status: revision 3 (2026-08-28)

The institution-session section is complete under ADR-0026 and has left this
active plan. This file now contains only the remaining popup work.

Citation convention: `ADR-NNNN:N` is shorthand for that ADR's file in `dev/adr/`.

---

## 2. The add button tells the truth — REVISE

### What the operator sees now

They open a paper from the inbox, click **+**, and get *Added to papio* for a
paper papio already holds a job for.

### What round two established

**The central claim of revision 1 fails.** `page_acquire_ack` carries
`job_id`, `duplicate`, `error` (`extension/src/protocol.ts`). A boolean
carries two states. Three success outcomes plus an error need a discriminant
that does not exist, and the popup cannot derive it locally: `ActiveJob.status`
is `offered | queued | accepted | auth_pending | awaiting_download`
(`extension/src/state.ts`) — all non-terminal — and `removeJob` drops
finished jobs (`state.ts`), so a job absent from `activeJobs` may be
terminal *or* live-but-not-browser-relevant. Absence proves nothing.

So §2 is a real fork:

- **B-two:** two outcomes — *Added to papio* / *Already in papio*. No wire
  change. Never lies; loses the "already validated" distinction.
- **B-three:** three outcomes, requiring a `page_acquire_ack` discriminant.
  That is a breaking change to an existing message: `requireFields` rejects
  unknown ack fields (`protocol.ts`, ack case `3267-3295`), the schema
  sets `additionalProperties: false`
  (`protocol/browser-v1.schema.json`), and `onInbound` catches
  `ProtocolError` and calls `disconnect` (`background.ts`) — an
  older extension tears down its native port. Permitted only under the verified
  zero-install exception, with all three contract files in one commit.

**Recommendation: B-three**, because "papio already has this, validated" is the
answer the operator most needs and it is the case that currently lies loudest.
AMO reads `Users: 0` live. The Chrome Web Store listing displays no user count,
which is absence of a number rather than a stated zero — **the operator must
confirm zero in the CWS developer dashboard before this ships.**

**Five further constraints, all from round two:**

1. **`Existing → Duplicate` is not safe as a bare mapping.**
   `createRequest` checks `liveJobForRequest` *first* and returns
   `Existing: true` before any canonical match (`internal/job/job.go`),
   and ADR-0010 states plainly that `existing: true` "does not assert that the
   live job owns the work just submitted" (`ADR-0010`).
   `acquire.submit_v2` accepts an arbitrary caller `request_id`, so a CLI can
   submit DOI B under `page_acquire_<hash(DOI A)>`; page A's preflight finds
   nothing, submit returns `Existing` for B, and the popup would say *Already in
   papio* about the wrong paper. **Compare the returned job's identifiers to the
   requested DOI, and treat a mismatch as `pageAcquireError`.**

   A second, narrower race survives that fix: the preflight can see no
   artifact, another route can submit and reach `ready` or `imported` before the
   page's own `SubmitWithOptionsAs`, and `firstLiveJob` skips every terminal
   state (`internal/job/job.go`, `Terminal` at `195-201`) — so the page
   creates a new job and reports *Added* for a paper just filed. Making this
   atomic would mean moving the artifact check inside `createRequest`'s
   transaction, which changes behaviour for every caller. The proportionate fix
   is a terminal recheck **after** a submit that created a new job: if an
   artifact now exists, report the artifact outcome instead of *Added*.
2. **The index the query needs is not the one revision 1 named.**
   `0001_init.sql` already provides `identifiers_by_value(kind,value)`,
   `jobs_by_state(state)`, and a *partial* unique `jobs_active_per_request(work_request_id)`
   covering live rows only — which cannot serve ready, imported, or unavailable
   history. Add `jobs_by_work_request_created_at(work_request_id, created_at DESC)`
   as **migration 0049** (latest embedded is 0048), and bump the four
   latest-version assertion sites AGENTS.md names: `internal/cli/clean_install_test.go`,
   `internal/doctor/doctor_test.go`, `internal/store/migrate_forward_test.go`'s
   two post-migration compares, and `internal/store/migrate_guard_test.go`'s
   `TestOpenRefusesSchemaNewerThanBinary`. Leave the historical schema-33
   fixtures alone.
3. **Do not bound with one global `LIMIT`.** Newer terminal rows from other
   request ids can hide an older live row. Use separate bounded probes for live,
   artifact, and unavailable.
4. **Cover `imported`, never short-circuit `previouslyUnavailable`, route every
   failure through `pageAcquireError`, and use ADR-0008's exact wording** —
   "*papio* already has a validated artifact", never `[in library]`
   (`ADR-0008`). `Terminal` includes `StateImported`
   (`internal/job/job.go`); Zotero moves a successful export from ready
   to imported (`internal/zotio/plan.go`); `recordPageBulkScan` folds
   `previously_unavailable` into `eligible`, "the bucket that already means still
   selectable" (`bridge.go`). Bound the work: `Bridge.Sync` holds
   `b.mu` for the whole handler (`bridge.go`) and the IPC server
   applies a 30-second deadline (`internal/ipc/protocol.go`), so a slow
   lookup is session-fatal.

Rejected: reusing `page_bulk_status_request` for one key. The popup cannot call
`papio.pageBulk.status` (`background.ts`), and `owned_missing_pdf`
carries a `ZotioItemKey` the validator permits only on bulk items
(`internal/protocol/protocol.go`, `6889-6920`).

---

## 3. Recovery legibility — REPLACES the notification design

### What round two killed, and why it is right

Revision 1 proposed routing the tab-close recovery to the daemon notifier.
**Decision 5 forbids it:** "Working progress never creates a desktop
notification, and a scheduled retry or delivery poll is not an ETA for success"
(`ADR-0023`). A re-queued paper is working progress. The same ADR the
plan claimed to respect forbids the notification the plan proposed.

Three further blockers confirmed it:

- **No valid category.** The vocabulary is closed (`ADR-0023`) and
  `notify.validateIntent` rejects unknown or mismatched values
  (`internal/notify/intent.go`). `system_degraded` means a named
  condition stops progress; this reducer abandons a claim and waits for fresh
  arbitration (`bridge.go`). Mapping ordinary recovery to it would
  be a false statement.
- **The copy overpromises.** A restart-recovered close only enqueues
  `owner_closed` and returns (`background.ts`); the daemon abandons
  the claim and does not durably schedule a retry. "papio is retrying" would be
  untrue in exactly the case that motivated it.
- **No episode identity exists to coalesce on.** The ledger keys on
  `(category, event_kind, aggregate_key, phase, window_start)`
  (`internal/store/notifications.go`), and the observation journal stores
  claim, binding, event, and `applied_at`, but no episode and no affected count
  (`internal/store/migrations/0042_claim_observations.sql`). "3 papers"
  has nothing to coalesce on.

### papio already notifies when it needs the operator

`park` routes a `decision_opened` intent on every transition into
`awaiting_human` or `needs_review` (`internal/app/app.go`), and the
pending-action reminder digests the rest (`internal/app/action_reminder.go`).
So the proactive interruption the operator asked for exists, for the event that
actually warrants it.

### What the operator gets instead

**Make the recovery legible, do not interrupt for it.** papio recovering is good
news, and good news that needs no decision is not worth an OS interruption. The
felt problem was silence, not absence.

Two surfaces already exist and neither needs an ADR change:

- The pulse companion line already reports concurrent work
  (`popup.ts`) and `#popup-pulse-next` reports the next action
  (`popup.html`). A re-queued job reads as `Moving` or `Scheduled`.
- `#popup-catchup` (`popup.html`) is already the "While you were away"
  surface, reading the durable Activity watermark (`popup.ts`). A
  tab-close recovery is durable Activity material.

**One honest gap to close, and it is not a notification.** Review confirmed that
today nothing carries this event to any surface: `computeBadge` counts auth
walls, `auth_pending`, required turns, and triage pending, appending `queuedAuth`
only in the tooltip (`background.ts`, `6208-6257`); `owner_closed`
changes claim, lease, and journal, not a badge field; and the pulse skips gate
member jobs and counts one gate turn (`internal/api/pulse.go`), so it
cannot render "3 retrying". So the work is: emit the recovery as a durable
Activity entry, and let the existing catch-up surface read it. That is a
producer and a copy change, not a channel.

**Correction to revision 1's citations:** `popup.ts` is
`acknowledgeInPage`'s `mode !== "all"` guard, not evidence that a closed tab has
no page. The no-page fact follows from `onTabRemoved`. And `inbox.html` is
the undo-bar container height, not a 44px touch-target precedent.

---

## 4. Decision count on the inbox icon — PROCEED WITH CHANGES

### What the operator sees now

A card at the top reading *Waiting on you · 102 decisions* with a **Review**
button on its own second row.

### What the operator sees after

The envelope icon carries the number. The Review button is gone; the envelope is
the route. The card still appears whenever it has something else to say.

The number respects the count preference already set for the toolbar. Above 99
it reads `99+`, with the exact figure on hover.

### The exact hide condition, from review

`ToolbarCountMode` is `required | all | off`, default `required`
(`extension/src/state.ts`; consumed `background.ts`, `623-683`; options
`options.ts`). Mode `all` shows `pending_total`; mode `required` shows
`turns_required` only when schema v3 **and** `required_turns_complete`; mode
`off` hides the count. `required_turns_complete` is optional on the wire and
permitted only at schema 3 (`protocol.ts`, `1965-1970`); the daemon
omits it below that.

**Hide the pulse card only when all of these hold:** mode is `required`, the
count is complete and exact, the badge is actually displaying it, and the card
has no `next`, no `capacity`, and no other detail.

**Keep the card** for mode `off`, for mode `all` (a pending inventory is not a
decision count), for incomplete counts, for browser-local blockers, and for
every other primary. Of the six primaries — `Unknown`, `Moving`, `Waiting on
you`, `Stalled`, `Scheduled`, `Idle` (`popup.ts`, `2500`, `2528-2533`) —
`Unknown` carries *Status as of …* and `Idle` carries a measured "no nonterminal
work"; neither duplicates the badge.

Cap the display: the wire validates `turns_required` up to 1,000,000
(`protocol.ts`), and a 32px pill fits one digit at
`--font-size-label: 11px` (`popup.html`) with a 4px neighbour gap
(`popup.html`). Either `99+` with the exact number in `aria-label`, or a
`width: auto` pill reading `[icon] 12`. Room exists: brand plus three pills plus
two gaps is about 236px of a 356px content box (`popup.html`, `140-141`).

---

## 5. One add control — OPTION A CONFIRMED, needs go-ahead

### What the operator sees now

On a single article: a header **+**, plus *both* an Acquire button and a *Select
papers on this page* button in the body. The bulk button shows on every HTTPS
page, because `isBulkScannablePage` tests only
`scannerOriginForBinding(binding) !== null` (`popup.ts`).

### Review chose Option A — the operator's own shape

Option A is buildable, and revision 1 **overstated its cost in two ways**:

- **There is no permission-gesture problem.** Bulk scanning uses `activeTab`
  plus a daemon allowlist message, not `chrome.permissions.request`. The
  gesture-chain worry does not apply to this control.
- **The Enter model already fits.** One marked header trigger satisfies
  `markPrimaryRailAction` (`popup.ts`); no second representation is
  needed.

### Two corrections to the design

1. **Drop hover.** It is a liability, not an affordance: a browser-action popup
   opens *under the pointer*, so an expansion triggered by hover fires
   unbidden, and the expansion then shifts layout beneath the cursor.
   **Click and Enter only.**
2. **Do not drive the split from a bound DOI.** That loses the existing
   DOI-less PDF *Send PDF* path (`popup.ts`, pinned at
   `popup.test.ts`). Drive it from existing specific-action
   availability — the `hoistable` condition — plus scannability.

### One promise to withdraw

"The body is genuinely empty" is false in the integrated popup. Only the
current-page rail empties. `#popup-pulse`, `#institution-session`,
`#popup-catchup`, and `#impact-summary` remain independently gated and can each
still render. The honest claim is: **the current-page rail disappears on a
single-paper page, and its two buttons become one header control.**

### Constraints either way

- **Do not extend `hoistIdleAcquire`.** The header control exists only while the
  rail acquire button is visible and enabled (`popup.ts`), and acquire
  is hidden while a job for that DOI is in flight (`popup.ts`). Hanging
  bulk selection off it makes "add all papers" vanish mid-acquisition. Derive
  header visibility from `isBulkScannablePage` **or** hoistability.
- Do not reach for `renderedRecordCountHint` (`extension/src/page-scan.ts`);
  detection is "invoked, never ambient" (`popup.ts`).
- Keep the bare plus for single-paper add (`popup.html`). For multi-add
  use stacked layers with a plus (Lucide `copy-plus`); `list-plus` is
  indistinguishable from the inbox tray outline (`popup.html`) at 18px.
- Build with `createElement`/`textContent`: `extension/build.ts` throws on
  `innerHTML` in a page bundle.

---

## 6. Header button borders — PROCEED

### What the operator sees after

The envelope and gear read as bare glyphs, with a border appearing on hover. The
**+** keeps its accent fill and border. No size changes.

### Verified clean by review

Set `border-color: transparent` on `.settings-btn`. Global
`box-sizing: border-box` (`popup.html`) means border changes do not alter
the 32px dimensions, so `border: 0` would not have resized anything either — but
`border-color: transparent` preserves the border width, the transition
(`popup.html`, `207-210`), the hover affordance (`popup.html`), the
focus outline, and the accented `+` (`popup.html`). No other rule breaks.

The **+** is not dimensionally larger: all three are `.settings-btn` at 32px
square with `padding: 0` (`popup.html`, token `popup.html`) and 18px
icons (`popup.html`, `248-252`).

---

## 7. Capture fixture panel — NO WORK

Store users never saw it. The release build deletes the markup
(`build.ts`), fails if the removal did not happen (`build.ts`), and
asserts the output matches the build kind (`build.ts`). `wireDevTools`
(`popup.ts`) also hides it whenever the manifest has an `update_url`
(`popup.ts`). Dev-ness is `PAPIO_CAPTURE_TOOLS=1` or daemon version
`0.0.0-dev` (`build.ts`).

**Do not reformat `popup.html`:** the release regex is keyed on the exact
indentation of that `<details>` (`build.ts`).

---

## 8. Reviewer demands rejected across both rounds

1. **"Honour `toolbarCountMode`, or drop the count badge."** Preference
   honoured; dropping refused.
2. **"Keep `#popup-pulse-review`."** Rejected on density.

## 9. Citation corrections applied

- `background.ts` → the ownership comment is at `17712`.
- `popup.ts` is the `mode !== "all"` guard, not the closed-tab fact.
- `inbox.html` is the undo-bar container height, not a touch-target
  precedent.
- The `library`-icon argument cited `popup.html`, which is the
  institution-session section, not an icon token. Argument dropped.

## 10. Build order

1. **§6** borders — CSS only, verified clean.
2. **§4** inbox count badge — with the exact hide condition.
3. **§5** add control, Option A, click/Enter only — needs the go-ahead.
4. **§2** acquire truthfulness — needs the CWS zero-install confirmation if
   B-three.
5. **§3** recovery legibility — durable Activity entry plus catch-up copy.
