# UI density review, 2026-08-21 — for the operator's decision

Commissioned after: *"the nag is still way too verbose and too many lines of UI
taken - and i think that's true of others."* Four parallel audits (popup, inbox,
options, remaining surfaces). Nothing here is implemented. Every number below
was **measured**, not estimated: the popup and inbox audits rendered the real
markup with its real stylesheet in headless Chromium at the real widths and read
line boxes off `Range.getClientRects()`.

## The headline

**The popup is 861.5px tall. Chrome caps a popup near 600px.** So 261.5px —
three cards — sit below the fold, every time it opens.

That is not merely ugly. It has a functional cost the audit caught: the
**"Open institutional access" row was pushed off-screen**. That row is a real
ask with a real button, and verbosity in the four cards above it made it
invisible.

**The nag is worse than reported.** It is not five lines, it is **6 rendered
text lines + a control = 160.1px = 27% of the visible popup, for one ask** — the
section message wraps too. At the three-row cap it is 306.5px, and the popup
becomes 1,007.9px.

The operator's suspicion that "that's true of others" is confirmed. On the
inbox, five same-kind rows cost **576px and 1,126 visible characters**; the
same five rows carrying the same information cost **369px and 511 characters**
under the proposal below. Six rows fit above the fold today; ten would.

## Defects found while measuring — these are bugs, not taste

Say go and these go, no design decision required.

1. **The current-page rail paints 10.1px outside its own card.** `#page-acquire-btn`
   renders `Acquisition in progress · 10.1177/15480518221144895` (51 chars) with
   `white-space: nowrap` (`extension/src/popup.html`, base `button` rule) and
   `min-width: max-content` (`.current-page-controls > button`). 51 chars of
   12px/700 need 353.1px inside a 330px card box, and because the rail is a grid
   the whole column widens — the scan button, paper title, status line and
   `Open inbox item` all bleed past the card border. **Any label over 48
   characters does this.** Verified from source. Fix: keep the DOI out of button
   labels (put it in `title`/`aria-label`, as the refused-Send-PDF path already
   does).
2. **A dead disabled control restates the live card 40px below it.**
   `Acquisition in progress · <doi>` is a permanently disabled button while
   `renderLiveAcquisition` prints the same job's actual standing, more precisely
   and clickably. The no-DOI path already hides its button on the stated
   principle that "a permanently dead control teaches nothing"; this path does
   not. Costs 40px.
3. **A route named in copy that the card does not carry.** The blocked-source
   message says `or manage all sources in Settings`, but `renderNeedsAttention`
   discards the handler (`void onOpenOptions;`) and the section has no Settings
   control. This is tonight's "an ask must carry its route" rule, inverted.
4. **Two dead strings.** `popup.html` ships `Close them` in the leftover-tabs
   card, overwritten with `Review in browser` before the card can ever show.
   `inbox.ts` sets `Connected to daemon.` on an element the same file
   unconditionally hides when connected. `inbox.html` declares
   `--content-measure: 60rem` and nothing uses it.
5. **One badge tooltip is missing the `papio: ` prefix** every sibling has:
   `Many decisions waiting — open inbox` in the incomplete-counts branch of
   `computeBadge`. (Its vocabulary is legitimate — that branch *is* the turn
   authority — so this is cosmetic only.)
6. **`Materialization binding ready` is shipped jargon on a page the operator
   can land on.** `materialize.html`/`materialize.ts` show only that, or
   `Invalid materialization binding`, with no action and no explanation. The
   operator saw this tab tonight. Proposed: `This papio access tab is ready —
   return to your paper.` and `This access tab is no longer valid — you can
   close it.`

## Popup — ranked, with the recommended package

Loss tiers: **L0** nothing leaves the surface · **L1** the fact moves onto the
control it explains (`title` + `aria-describedby`, so hover and screen readers
keep it) · **L2** the fact leaves the popup.

| # | change | px | loss |
|---|---|---|---|
| 1 | **One ask = one row.** Drop the section message unless there are 2+ rows; move each row's reason onto its button as `title` + `aria-describedby`. Renders as `Needs you` / `Security check — sagepub.com  [Open tab]` | **−66.9** (3 rows: −115) | L1, and a **net ARIA gain** — rows have no `aria-describedby` today |
| 2 | **Hide the dead disabled Acquire button** (defect 2) | −40.0 | **L0** |
| 3 | **Institution status 14px → 12px**, matching every other secondary line | −23.2 | **L0** — nothing reworded; also unwraps 9 other session states |
| 4 | **Waiting sub-heading only at 2+ rows** — `Open institutional access` above a single row whose button says `Open` | −23.9 | L0 |
| 5 | **Count once in the pulse**: `Review 125 decisions` → `Review` (the line above already says 125) | 0 | **L0** |
| 6 | **DOI out of button labels** (defect 1) | 0, fixes the overflow | L1 |
| 7 | **Overflow string once**: it is both appended to the message and set as a full-width button | 0…−20.3 | L0 |
| 8 | **Rename the blocker heading.** `Needs you` is browser-local blockers, but ADR-0023 reserves that phrase for the daemon's `turns_required` — which the pulse 245px above is already using. Proposed `Do this in the browser` | 0 | L0, returns the reserved phrase |
| 9 | Live status one line; demoted event to `title` | −17.4 | L1 — **must keep the `No progress for 12m` prefix visible** |
| 10 | Dev fixture panel out of the popup into Options | −57 (dev only) | L0 |

**Recommended package: 1–9.** Result: **861.5px → 690px (−20%)**, the nag goes
**6 lines + button → 2 lines + button**, and everything tonight's popup showed
is still visible except three explanations that move onto the controls they
explain. Only the impact line and the dev panel stay below the fold.

Adding three L2 items (pulse line becomes the control, drop the batch line, drop
the session row when a waiting list exists) reaches **596.8px — the whole popup
above the fold** — but each gives up a real fact, and one of them contradicts a
written plan decision (`notification-feedback-experience.md` allots the popup
exactly one `latest_batch` line). **Not recommended without your call.**

## Inbox — ranked

1. **One block header carries kind + count + instruction; the row carries title
   + one fact.** Fold the family instruction into the heading (sentence case, not
   the current tracked uppercase — the audit measured both and the uppercase
   merge reads as shouting), delete the per-row reason, compress the citation to
   `Author Year · host`. Measured on five rows: **576px → 369px (−36%), 1,126 →
   511 characters (−55%)**; at an 800px window 705px → 408px. Rows above the
   fold: **6 → 10**. The 14-variant copy table shrinks 1,103 → 802 characters and
   a whole parallel switch is deleted.
2. **Render that block header even for a single row.** Today a lone
   security-check, terms or institution-sign-in row gets **no instruction at
   all** — the per-row path has no case for those kinds. Strictly additive
   information at equal height (104px → 97px).
3. **Give retraction rows an identity.** Every notice is titled
   `Library update notice` with one fact `NATURE retraction`; five rows are
   546px of which **48% of the visible characters are the same two strings
   repeated**. Needs a daemon change to pass the work title through.
4. Shorten the four longest row strings in place (the 119-char encryption
   explanation, the 93-char Downloads-access path, the CLI incantation, two
   90+ char family instructions).
5. Fix the reason de-dupe: it suppresses a duplicate only on literal substring
   match, so `papio reached a different work` vs `papio landed on a different
   work` prints the same fact twice.
6. **The counts line double-counts.** `127 open · 125 need you · 4 for reference`
   — retractions are counted in both `Actions (N)` and `N for reference`, and
   `pending_total` is just the two tab counts plus grabs. The only fact it adds
   is `125 need you`.

## Options

1. **Permission rows are two lines each.** Every publisher renders its label
   *and* the raw match pattern (`SAGE Journals` / `https://*.journals.sagepub.com/*`),
   and the pattern is already in the switch's `aria-label`. Hiding the visible
   pattern saves **one line per row — 12 lines at a dozen granted hosts**. The
   objection is permission transparency, which is why this is your call, not mine.
2. The five longest helper texts run 156–305 characters: provider access (305),
   page scanning (291), publisher terms (282), library resolver (250), daemon
   notifications (156). `docs/guide/user-guide.md` already explains keep-warm,
   the interval, the toolbar count, the resolver flow and notifications at
   length. Shortening the *page* is safe only where the consequence survives —
   the terms text carries a legal warning and the provider text carries the
   credentials/MFA boundary and revoke-all behaviour, so those two should shrink
   least.
3. Keep-warm's heading, lede, control label and hint state the same fact four
   times.

## The cross-surface redundancy

**One security-check fact is written four times**, and an operator who clicks
the popup's new `Review 125 decisions` button reads it twice more:

- popup message: `Solve the check in the open tab — papio resumes on its own.`
- popup row reason: `Complete it in the open tab; papio resumes without retrying the provider.`
- inbox group: `Solve each security check in its tab.`
- inbox row: `papio resumes automatically after you solve the security check.`

**And one count is rendered three ways in one click-path**: `Review 125
decisions` → `Actions (127)` + `125 need you`. Two words for `turns_required`
and a third, larger number beside them.

## Risks that constrain all of the above

1. **`role="status"`/`aria-live` on the pulse primary line is load-bearing** — it
   is both the section's accessible name and its announcer. Any proposal that
   hides it must keep the element as `.visually-hidden`, never delete it. Same
   for both `aria-labelledby` heading targets.
2. **Terseness becomes ambiguity at the challenge row.** With the reason on the
   button, a sighted user sees `Security check — sagepub.com [Open tab]` and must
   infer that solving it in that tab is enough and papio will not re-ask the
   publisher — the inference that stops them closing the tab early. Hover
   restores it; hover is not discoverable. If that reassurance must stay
   visible, keep it **once**, in the section message — never in both places.
3. **A section message above mixed rows must summarise, not instruct.** With two
   rows of different kinds one instruction cannot honestly describe both. If the
   message returns only at 2+ rows it must read like `2 things to clear here`.
4. **The inbox's density depends on a daemon boolean.** All inbox measurements
   assume `family_breakdown_complete`. One action anywhere in the snapshot with
   an unmapped guidance flips it, and the surface silently reverts to N repeated
   instructions **with no group heading at all** — 20 rows: 2,077px and 900
   instruction characters instead of 46. Any density work that leaves that cliff
   in place gets undone in the field.
5. **The inbox cannot name the blocked host.** The popup can say
   `Security check — sagepub.com` because the browser knows `challenge_host`; the
   daemon has no such field for these actions. A shorter inbox row must not
   imply it knows which tab is blocked — that would be inventing knowledge, the
   thing tonight's honesty work exists to prevent.
6. **Do not compress the Firefox platform caveat.** It is the only place a
   Firefox operator learns a saved download can never reach papio, and it is
   already printed once per block rather than once per row.

## What needs your decision

- **Defects 1–6**: say go. No taste required.
- **Popup package 1–9**: my recommendation. −20%, nothing lost from the surface.
- **Popup L2 items** (fit under 600px): needs your call; one contradicts a plan.
- **Inbox 1–2**: my recommendation; 2 also fixes a real gap (a lone gate row
  with no route).
- **Options permission rows**: transparency versus 12 lines — your call.
- **Retraction titles**: needs a daemon change; worth it (48% repeated characters).
