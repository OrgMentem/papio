# Popup UX: recovery legibility

Status: revision 4 (2026-08-28)

Five of this plan's six sections have shipped. This file now records only the
one open item, so it does not read as active work that no longer exists.

- §1, institution session awareness: shipped under `dev/adr/0026-institution-session-awareness-after-publisher-landing.md`.
- §2, acquire truthfulness: shipped in commit `f90e01a`. `page_acquire_ack`
  carries a real `outcome`, the popup shows ADR-0008's exact wording, and
  migration `0049_jobs_by_work_request_created_at.sql` backs the query.
- §4, decision count on the inbox icon: shipped in commit `f90e01a`.
- §5, one add control: shipped in commit `f90e01a`. `renderHeaderAddControl`
  and `collapseHeaderAddMenu` in `extension/src/popup.ts`.
- §6, header button borders: shipped in commit `f90e01a`.
- §7, capture fixture panel: no work was needed; the release build already
  strips it.

Their design rationale, rejected alternatives, and citation corrections live
in `f90e01a`'s commit message and in the ADRs it cites (0008, 0010, 0019).
Git history is the archive; this file does not restate them.

Citation convention: `ADR-NNNN:N` is shorthand for that ADR's file in `dev/adr/`.

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

## 8. Reviewer demands rejected across both rounds

1. **"Honour `toolbarCountMode`, or drop the count badge."** Preference
   honoured; dropping refused.
2. **"Keep `#popup-pulse-review`."** Rejected on density.

## 10. Build order

1. **§3** recovery legibility — durable Activity entry plus catch-up copy.
