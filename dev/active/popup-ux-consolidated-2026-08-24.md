# Popup UX: session awareness, acquire truthfulness, recovery legibility, header consolidation

Status: revision 2, after two review rounds (2026-08-24)

Supersedes `popup-header-and-feedback-2026-08-24.md`. Round one reviewed three
items in isolation; round two reviewed the integrated shape and killed or
reshaped three of them. This revision records what survived.

Citation convention: `ADR-NNNN:N` is shorthand for that ADR's file in `dev/adr/`.

**Two items are ready to build (§4, §6). Two need bounded further design
(§1, §2). One is dead as originally conceived and replaced (§3). One needs a
one-line go-ahead (§5).**

---

## 1. Institution session awareness — REVISE

### What the operator sees now

They sign in from a publisher's access link. The popup keeps saying *Signed out
or expired · via your library tab*. Three papers sit under *Waiting on your
sign-in* doing nothing until they sign in again through the library page.

### What the operator should see

They sign in once, wherever they are. papio re-checks, and when the check
succeeds the card flips to *Signed in* and the waiting papers move by
themselves.

**This is a probability, not a promise** — see the honest fallback below. When
the check cannot conclude, the card must say so rather than assert either state.

### What round two established

**The trigger works.** `tabs` is in `permissions`
(`extension/manifest.json:17-27`), so Chrome populates `tab.url` in
`onUpdated` without any host grant. The Sage URL is visible even on a provider
the operator never granted. Optional host grants control `scripting`, not URL
visibility.

**Correction to revision 1:** it claimed the branch "sees a URL only for hosts
the operator already granted". That is false. The branch sees every tab's URL.
Privacy therefore depends on discipline, not on the permission model: parse
`change.url ?? tab.url` transiently, retain only `.hostname`, persist nothing.
Review confirmed that satisfies the URL-free contracts — `ledger.ts:106-108`
rejects any record carrying a `url`, keepalive stores only bare origins, and
`background.ts`'s top-of-file invariant forbids URL, host, or title in outgoing
or persisted auth state.

**The probe has its grant.** `probeOrigin`'s `executeScript` needs a resolver
host permission (`keepalive.ts:1888-1942`), and standing `host_permissions`
cover `*.primo.exlibrisgroup.com`, `*.alma.exlibrisgroup.com`, and
`login.openathens.net` (`manifest.json:28-32`). The configured resolver here is
`une.primo.exlibrisgroup.com`, so it matches.

**But the outcome is not guaranteed, and the plan must not promise it.**
`createTabOnce` (`keepalive.ts:2004-2070`) is private and only creates or adopts
a pinned, muted tab; it never inspects the new document.
`probeOriginAutomatically` (`keepalive.ts:1124-1130`) only normalises and calls
`requestProbe`, and `probeOrigin` queries *existing* tabs
(`keepalive.ts:1301-1338`). So calling today's trigger commits `no_tab` or scans
a still-loading page.

The full working sequence review traced is: select target → create tab → the
created tab's own `onUpdated` calls `noteResolverNavigation` → `reloadSettleMs`
(default 1s) → trailing `requestProbe` → `observeTab` →
`resolverMarkerVerdict` → `commitOriginProbe` → `recordFreshSessionEvidence`
(warm_verified, drain) → the popup's 5s poll renders `in`. That requires **one
new public ensure-and-probe method** that creates the exact resolver tab, waits
for its navigation to settle, then requests the probe.

Three ways it still legitimately fails, all of which must render honestly rather
than as *Signed out*: a missing or revoked resolver grant yields `scan_failed`
(`keepalive.ts:1888-1942`); an IdP redirect classifies as `auth_url`; and a
resolver page without qualifying markers stays inconclusive
(`keepalive.ts:1438-1483`). Nothing in the code can guarantee that a resolver
reflects a publisher-triggered SSO session.

A fourth requirement follows: route creation through the keepalive lifecycle,
not the private path. The `enabled` and `warmDemand` checks, tab removal, and
the next lifecycle timer live in `reconcile` (`keepalive.ts:1760-1770`) and its
observe path (`keepalive.ts:2073-2081`), and the per-origin floor bounds probe
*starts*, not tab creation or origin switching. A branch calling creation
directly would reopen on every institution switch, ignore `keepalive.enabled`,
and leave the tab behind after demand ends. The ensure method must close through
`closeTab` and schedule reconciliation.

Copy note: the honest fallback string *No library page open — open your library
to verify* is at `popup.ts:1397-1406`. Revision 1 also cited
`popup.ts:1479-1485` for it; that range is the stale-verdict branch
(*Can't tell yet* / *Signed out or expired*) and does not carry the same copy.

**Five gaps the design must close before it is buildable:**

1. **Host-to-job correlation is undefined.** `jobInstitutionOrigin`
   (`background.ts:3128`) needs an `ActiveJob`, and an untracked tab has none.
   `signInBlockerCount` (`background.ts:3565-3567`) returns only a length.
   Without a correlation the branch could probe the wrong resolver and release
   another institution's queued work. Require: map the provider host to the
   blocked jobs, take their institution origins, and act only when that set is
   a single origin.
2. **The identity-provider path is not mapped at all.**
   `jobInstitutionOrigin` resolves from offer and provider hosts, not from
   `login_entity_id`, so an event on the IdP host triggers nothing. Either match
   an IdP host against the job's own `login_entity_id` and derive the origin
   from that, or drop the IdP case and trigger on resolver return.
3. **A same-document publisher login may fire no `onUpdated` event at all.** Add
   a bounded `auth_pending` wake so the question still gets asked.
4. **The per-origin floor does not bound cross-origin tab churn.**
   `MIN_PROBE_START_SPACING_MS` is keyed per origin (`keepalive.ts:765-768`),
   and the manager closes one managed tab when `selectResolver` changes. Opening
   and switching managed tabs across origins needs its own global cooldown or
   latch, or a multi-institution operator gets tab churn.
5. **The rejected demand still needs its own test.** Revision 1 leaned on
   `background.ts:14054-14057` as precedent for unconditional sibling resume.
   Review is right that it is narrower: that path is a job's own first-hand
   `auth_returned` landing on a tab papio owned. The new path runs through
   `warm_verified` → `recordFreshSessionEvidence` (`background.ts:14332+`),
   which the daemon claim-fences per origin profile. **The rejection stands** —
   observing a live session and then leaving queued work parked is an automation
   regression — but it must be tested on the `warm_verified` path directly, not
   justified by the narrower precedent.

No `cookies` permission in this pass.

---

## 2. The add button tells the truth — REVISE

### What the operator sees now

They open a paper from the inbox, click **+**, and get *Added to papio* for a
paper papio already holds a job for.

### What round two established

**The central claim of revision 1 fails.** `page_acquire_ack` carries
`job_id`, `duplicate`, `error` (`extension/src/protocol.ts:158-161`). A boolean
carries two states. Three success outcomes plus an error need a discriminant
that does not exist, and the popup cannot derive it locally: `ActiveJob.status`
is `offered | queued | accepted | auth_pending | awaiting_download`
(`extension/src/state.ts:13-14`) — all non-terminal — and `removeJob` drops
finished jobs (`state.ts:831-836`), so a job absent from `activeJobs` may be
terminal *or* live-but-not-browser-relevant. Absence proves nothing.

So §2 is a real fork:

- **B-two:** two outcomes — *Added to papio* / *Already in papio*. No wire
  change. Never lies; loses the "already validated" distinction.
- **B-three:** three outcomes, requiring a `page_acquire_ack` discriminant.
  That is a breaking change to an existing message: `requireFields` rejects
  unknown ack fields (`protocol.ts:1761-1795`, ack case `3267-3295`), the schema
  sets `additionalProperties: false`
  (`protocol/browser-v1.schema.json:2113-2156`), and `onInbound` catches
  `ProtocolError` and calls `disconnect` (`background.ts:15527-15532`) — an
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
   `Existing: true` before any canonical match (`internal/job/job.go:493-509`),
   and ADR-0010 states plainly that `existing: true` "does not assert that the
   live job owns the work just submitted" (`ADR-0010:302-317`).
   `acquire.submit_v2` accepts an arbitrary caller `request_id`, so a CLI can
   submit DOI B under `page_acquire_<hash(DOI A)>`; page A's preflight finds
   nothing, submit returns `Existing` for B, and the popup would say *Already in
   papio* about the wrong paper. **Compare the returned job's identifiers to the
   requested DOI, and treat a mismatch as `pageAcquireError`.**

   A second, narrower race survives that fix: the preflight can see no
   artifact, another route can submit and reach `ready` or `imported` before the
   page's own `SubmitWithOptionsAs`, and `firstLiveJob` skips every terminal
   state (`internal/job/job.go:729-747`, `Terminal` at `195-201`) — so the page
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
   (`ADR-0008:105-113`). `Terminal` includes `StateImported`
   (`internal/job/job.go:195-201`); Zotero moves a successful export from ready
   to imported (`internal/zotio/plan.go:427-444`); `recordPageBulkScan` folds
   `previously_unavailable` into `eligible`, "the bucket that already means still
   selectable" (`bridge.go:5623-5628`). Bound the work: `Bridge.Sync` holds
   `b.mu` for the whole handler (`bridge.go:1057-1059`) and the IPC server
   applies a 30-second deadline (`internal/ipc/protocol.go:36`), so a slow
   lookup is session-fatal.

Rejected: reusing `page_bulk_status_request` for one key. The popup cannot call
`papio.pageBulk.status` (`background.ts:20518-20538`), and `owned_missing_pdf`
carries a `ZotioItemKey` the validator permits only on bulk items
(`internal/protocol/protocol.go:2262-2268`, `6889-6920`).

---

## 3. Recovery legibility — REPLACES the notification design

### What round two killed, and why it is right

Revision 1 proposed routing the tab-close recovery to the daemon notifier.
**Decision 5 forbids it:** "Working progress never creates a desktop
notification, and a scheduled retry or delivery poll is not an ETA for success"
(`ADR-0023:167-169`). A re-queued paper is working progress. The same ADR the
plan claimed to respect forbids the notification the plan proposed.

Three further blockers confirmed it:

- **No valid category.** The vocabulary is closed (`ADR-0023:119-129`) and
  `notify.validateIntent` rejects unknown or mismatched values
  (`internal/notify/intent.go:106-120`). `system_degraded` means a named
  condition stops progress; this reducer abandons a claim and waits for fresh
  arbitration (`bridge.go:11167-11171`). Mapping ordinary recovery to it would
  be a false statement.
- **The copy overpromises.** A restart-recovered close only enqueues
  `owner_closed` and returns (`background.ts:19499-19531`); the daemon abandons
  the claim and does not durably schedule a retry. "papio is retrying" would be
  untrue in exactly the case that motivated it.
- **No episode identity exists to coalesce on.** The ledger keys on
  `(category, event_kind, aggregate_key, phase, window_start)`
  (`internal/store/notifications.go:61-104`), and the observation journal stores
  claim, binding, event, and `applied_at`, but no episode and no affected count
  (`internal/store/migrations/0042_claim_observations.sql:24-43`). "3 papers"
  has nothing to coalesce on.

### papio already notifies when it needs the operator

`park` routes a `decision_opened` intent on every transition into
`awaiting_human` or `needs_review` (`internal/app/app.go:1864-1870`), and the
pending-action reminder digests the rest (`internal/app/action_reminder.go:174-180`).
So the proactive interruption the operator asked for exists, for the event that
actually warrants it.

### What the operator gets instead

**Make the recovery legible, do not interrupt for it.** papio recovering is good
news, and good news that needs no decision is not worth an OS interruption. The
felt problem was silence, not absence.

Two surfaces already exist and neither needs an ADR change:

- The pulse companion line already reports concurrent work
  (`popup.ts:2609-2623`) and `#popup-pulse-next` reports the next action
  (`popup.html:989`). A re-queued job reads as `Moving` or `Scheduled`.
- `#popup-catchup` (`popup.html:1100-1103`) is already the "While you were away"
  surface, reading the durable Activity watermark (`popup.ts:2798-2824`). A
  tab-close recovery is durable Activity material.

**One honest gap to close, and it is not a notification.** Review confirmed that
today nothing carries this event to any surface: `computeBadge` counts auth
walls, `auth_pending`, required turns, and triage pending, appending `queuedAuth`
only in the tooltip (`background.ts:557-564`, `6208-6257`); `owner_closed`
changes claim, lease, and journal, not a badge field; and the pulse skips gate
member jobs and counts one gate turn (`internal/api/pulse.go:193-218`), so it
cannot render "3 retrying". So the work is: emit the recovery as a durable
Activity entry, and let the existing catch-up surface read it. That is a
producer and a copy change, not a channel.

**Correction to revision 1's citations:** `popup.ts:569-572` is
`acknowledgeInPage`'s `mode !== "all"` guard, not evidence that a closed tab has
no page. The no-page fact follows from `onTabRemoved`. And `inbox.html:1175` is
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
(`extension/src/state.ts:61`; consumed `background.ts:550`, `623-683`; options
`options.ts:265-266`). Mode `all` shows `pending_total`; mode `required` shows
`turns_required` only when schema v3 **and** `required_turns_complete`; mode
`off` hides the count. `required_turns_complete` is optional on the wire and
permitted only at schema 3 (`protocol.ts:1936-1938`, `1965-1970`); the daemon
omits it below that.

**Hide the pulse card only when all of these hold:** mode is `required`, the
count is complete and exact, the badge is actually displaying it, and the card
has no `next`, no `capacity`, and no other detail.

**Keep the card** for mode `off`, for mode `all` (a pending inventory is not a
decision count), for incomplete counts, for browser-local blockers, and for
every other primary. Of the six primaries — `Unknown`, `Moving`, `Waiting on
you`, `Stalled`, `Scheduled`, `Idle` (`popup.ts:2490`, `2500`, `2528-2533`) —
`Unknown` carries *Status as of …* and `Idle` carries a measured "no nonterminal
work"; neither duplicates the badge.

Cap the display: the wire validates `turns_required` up to 1,000,000
(`protocol.ts:1953-1956`), and a 32px pill fits one digit at
`--font-size-label: 11px` (`popup.html:38`) with a 4px neighbour gap
(`popup.html:597-601`). Either `99+` with the exact number in `aria-label`, or a
`width: auto` pill reading `[icon] 12`. Room exists: brand plus three pills plus
two gaps is about 236px of a 356px content box (`popup.html:60`, `140-141`).

---

## 5. One add control — OPTION A CONFIRMED, needs go-ahead

### What the operator sees now

On a single article: a header **+**, plus *both* an Acquire button and a *Select
papers on this page* button in the body. The bulk button shows on every HTTPS
page, because `isBulkScannablePage` tests only
`scannerOriginForBinding(binding) !== null` (`popup.ts:3648-3649`).

### Review chose Option A — the operator's own shape

Option A is buildable, and revision 1 **overstated its cost in two ways**:

- **There is no permission-gesture problem.** Bulk scanning uses `activeTab`
  plus a daemon allowlist message, not `chrome.permissions.request`. The
  gesture-chain worry does not apply to this control.
- **The Enter model already fits.** One marked header trigger satisfies
  `markPrimaryRailAction` (`popup.ts:3655-3670`); no second representation is
  needed.

### Two corrections to the design

1. **Drop hover.** It is a liability, not an affordance: a browser-action popup
   opens *under the pointer*, so an expansion triggered by hover fires
   unbidden, and the expansion then shifts layout beneath the cursor.
   **Click and Enter only.**
2. **Do not drive the split from a bound DOI.** That loses the existing
   DOI-less PDF *Send PDF* path (`popup.ts:3754-3775`, pinned at
   `popup.test.ts:3140-3156`). Drive it from existing specific-action
   availability — the `hoistable` condition — plus scannability.

### One promise to withdraw

"The body is genuinely empty" is false in the integrated popup. Only the
current-page rail empties. `#popup-pulse`, `#institution-session`,
`#popup-catchup`, and `#impact-summary` remain independently gated and can each
still render. The honest claim is: **the current-page rail disappears on a
single-paper page, and its two buttons become one header control.**

### Constraints either way

- **Do not extend `hoistIdleAcquire`.** The header control exists only while the
  rail acquire button is visible and enabled (`popup.ts:3184-3185`), and acquire
  is hidden while a job for that DOI is in flight (`popup.ts:3866`). Hanging
  bulk selection off it makes "add all papers" vanish mid-acquisition. Derive
  header visibility from `isBulkScannablePage` **or** hoistability.
- Do not reach for `renderedRecordCountHint` (`extension/src/page-scan.ts:263-333`);
  detection is "invoked, never ambient" (`popup.ts:3646-3647`).
- Keep the bare plus for single-paper add (`popup.html:960-963`). For multi-add
  use stacked layers with a plus (Lucide `copy-plus`); `list-plus` is
  indistinguishable from the inbox tray outline (`popup.html:966-969`) at 18px.
- Build with `createElement`/`textContent`: `extension/build.ts:34-36` throws on
  `innerHTML` in a page bundle.

---

## 6. Header button borders — PROCEED

### What the operator sees after

The envelope and gear read as bare glyphs, with a border appearing on hover. The
**+** keeps its accent fill and border. No size changes.

### Verified clean by review

Set `border-color: transparent` on `.settings-btn`. Global
`box-sizing: border-box` (`popup.html:111-113`) means border changes do not alter
the 32px dimensions, so `border: 0` would not have resized anything either — but
`border-color: transparent` preserves the border width, the transition
(`popup.html:199`, `207-210`), the hover affordance (`popup.html:243-246`), the
focus outline, and the accented `+` (`popup.html:257-265`). No other rule breaks.

The **+** is not dimensionally larger: all three are `.settings-btn` at 32px
square with `padding: 0` (`popup.html:229-241`, token `popup.html:56`) and 18px
icons (`popup.html:59`, `248-252`).

---

## 7. Capture fixture panel — NO WORK

Store users never saw it. The release build deletes the markup
(`build.ts:47`), fails if the removal did not happen (`build.ts:48-50`), and
asserts the output matches the build kind (`build.ts:40-42`). `wireDevTools`
(`popup.ts:4288`) also hides it whenever the manifest has an `update_url`
(`popup.ts:4301-4306`). Dev-ness is `PAPIO_CAPTURE_TOOLS=1` or daemon version
`0.0.0-dev` (`build.ts:21-23`).

**Do not reformat `popup.html`:** the release regex is keyed on the exact
indentation of that `<details>` (`build.ts:24`).

---

## 8. Reviewer demands rejected across both rounds

1. **"Add a non-authorizing probe reason."** Rejected. Observing a live session
   and leaving queued work parked is an automation regression. Round two
   correctly narrowed the precedent, so the authority must be tested on the
   `warm_verified` path directly (§1 gap 5) rather than asserted.
2. **"Honour `toolbarCountMode`, or drop the count badge."** Preference
   honoured; dropping refused.
3. **"Keep `#popup-pulse-review`."** Rejected on density.

## 9. Citation corrections applied

- `background.ts:17680-17683` → the ownership comment is at `17712`.
- `popup.ts:569-572` is the `mode !== "all"` guard, not the closed-tab fact.
- `inbox.html:1175` is the undo-bar container height, not a touch-target
  precedent.
- The `library`-icon argument cited `popup.html:1069-1071`, which is the
  institution-session section, not an icon token. Argument dropped.
- Revision 1's claim that the §1 branch sees only granted hosts was false;
  corrected in §1.

## 10. Build order

1. **§6** borders — CSS only, verified clean.
2. **§4** inbox count badge — with the exact hide condition.
3. **§5** add control, Option A, click/Enter only — needs the go-ahead.
4. **§2** acquire truthfulness — needs the CWS zero-install confirmation if
   B-three.
5. **§3** recovery legibility — durable Activity entry plus catch-up copy.
6. **§1** session awareness — after its five gaps are closed.
