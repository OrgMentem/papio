# An interactive in-browser toast, with take-back-control

Status: Slices 1, 2, 4 and 5 shipped (2026-08-28). Slice 3 is deliberately open —
see the operator question at the end.

## What was asked for

Verbatim, 2026-08-24:

> I want to explore toasts that appear like genuine toasts, not a card within the
> popup. […] if the browser closes a tab, a toast could offer the user the option
> to re-open it - that gives a more proactive papio, while giving the user an
> option to take back control […] but I've tried to express this intent in recent
> plans, yet it seems to get diminished by inherent conservatism.

Two requirements, both explicit: the surface is a **real toast in the browser**,
not a popup card, and it carries **one action the researcher can take**.

## What the previous plan did with it

`f90e01a:dev/active/popup-header-and-feedback-2026-08-24.md:269-275` recorded the
request and rejected it under the heading "Also rejected: making the in-page chip
interactive", then shipped an OS-notification route instead
(`§C1`, same file `:277-300`). The OS route was never what was asked for. Three
of the four stated reasons do not survive checking.

**Reason 1: "There is no page to inject into for a closed tab."** A non-argument.
The toast does not belong in the tab that closed; it belongs in the tab the
researcher is looking at now, which is exactly how a browser's own "reopen closed
tab" works. The real constraint is host permission for that arbitrary tab, which
is a different and partly solvable problem — see **Routes** below.

**Reason 2: "A page-context click cannot call `chrome.*`."** False.
`extension/src/popup.ts`'s `renderInPageAcknowledgement` injection site passes no `world` argument, and Chrome
documents `world` as defaulting to `ISOLATED` — "the execution environment unique
to this extension"
([`chrome.scripting`, ExecutionWorld](https://developer.chrome.com/docs/extensions/reference/api/scripting)).
That is a content-script world, where `chrome.runtime.sendMessage` is available.
The genuine rule the code comment states
(`extension/src/popup.ts:renderInPageAcknowledgement`'s doc comment) is that the injected function must not close
over outer scope, because the body is serialized. That forbids sharing a constant.
It does not remove runtime API access.

**Reason 3: "Three assertions pin the inertness."**
`extension/test/popup.test.ts:4471` (no `button, a, input`) and `:4734` (exactly
one 3000ms timer) are tests papio wrote to enforce this decision. They are the
decision restated, not a constraint on changing it. Citing them as the reason the
decision cannot change is circular.

**Reason 4: "A three-second window to hit a button is a WCAG 2.2.1 failure."**
Real, and it argues for a different toast rather than no toast. papio already
ships a six-second undo window and treats it as acceptable:
`extension/src/inbox.ts:UNDO_WINDOW_MS` (6000 ms). A toast is also not a
deadline when the same recovery stays reachable afterwards in the inbox, so the
timing rule applies to a toast that *commits* on expiry, not to one that offers a
shortcut.

## The one real constraint, which is not technical

ADR-0023 Decision 1 defines the existing chip narrowly, and deliberately:
noninteractive, three seconds, one closed label, scoped to the exact bound active
page, "emitted only for a validated successful response to an action the
researcher just requested, never for a failure, a later job transition, or an
event that arrived on its own"
(ADR-0023 Decision 1, the host-page-acknowledgement paragraphs).

A tab closing is precisely "an event that arrived on its own". So this is a
**seventh surface**, not a widening of the third. It needs its own decision with
its own bounds. That is the honest cost, and it is a cost worth paying rather than
a reason to refuse.

## Routes

| Route | Always deliverable | New permission | Carries a button | Notes |
| --- | --- | --- | --- | --- |
| **A.** In-page toast via `scripting.executeScript` | No — only in a tab papio already has host permission for | None | Yes, via `chrome.runtime.sendMessage` from the isolated world | Best fidelity; unavailable on an arbitrary page |
| **B.** Small unfocused extension window (`chrome.windows.create({type:"popup", focused:false})`) | Yes | None — already used by `extension/src/background.ts:openWorkWindowTab` | Yes, it is an extension page | Works on Chrome and Firefox; the reliable channel |
| **C.** Widen `optional_host_permissions: https://*/*` | Yes | Yes, in bulk | Yes | **Refused.** Both store listings promise this pattern is "never granted at install and never requested in bulk", only the exact configured resolver origin. Widening it would falsify a published disclosure. |

Decision: build **B** as the channel that always works, and use **A** when the
current tab is already granted, because an in-page toast reads better than a
window. One shared renderer, two delivery mechanisms, one action contract.

## What "reopen" honestly means, per branch

`extension/src/background.ts:onTabRemoved` has four terminal branches after its
two early returns, and the answer differs across them:

| Branch | What papio already does | Code | Is reopen truthful? |
| --- | --- | --- | --- |
| `waiting_for_session` | re-queues to `queued`, schedules release | `beginProviderDrive` + `scheduleQueuedHandoffRelease` | **Yes** — reopen means drive it now instead of waiting for the next drain |
| delivery in flight | keeps the download correlation, detaches the tab | `deliveryJobs`/`pendingDelivery` branch | No — nothing was lost |
| `awaiting_download` | parks; the daemon poll adopts the file | `job.status === "awaiting_download"` branch | No — nothing was lost |
| everything else | `provider_outcome: cancelled`, then re-drains | the fall-through `send("provider_outcome", …)` | **Yes** — reopen means re-drive |

On the **institutional** path the word "undo" is wrong and the previous plan was
right about why: `owner_closed` abandons the materialization claim
(`internal/job/claim_observation_apply.go`'s `case "owner_closed"`), retires the
authentication-entry lease
(`internal/job/institutional_evidence.go:RetireAuthenticationEntryLeaseAfterOwnerClose`),
and consumes the one-use close authorization
(`internal/job/claim_observations.go:ConsumeCloseAuthorizationForBinding`). Those three
are not reversible. The button there requests a **fresh** sign-in tab, and the
copy must say so.

So the toast carries one of two labels, chosen from state papio already holds:

- `Reopen` — the two branches above where the route is genuinely resumable.
- `Open a new sign-in tab` — the institutional case, where a new claim is the
  only honest offer.

Never both, and never a bare "Undo", which would promise a reversal papio cannot
perform on the institutional path.

## Bounds this surface accepts

Written as bounds because a seventh surface without them is how the popup filled
up with cards in the first place.

1. One toast at a time. A second event replaces the first; it never stacks.
2. Eight seconds, dismissable, and it never commits anything on expiry. The
   recovery stays in the inbox afterwards, which is what keeps WCAG 2.2.1
   satisfied.
3. One action, plus a close affordance. No progress, no error text, no identifier,
   no title, no URL, no provider name, no job id in the rendered body.
4. It carries the job id in the extension's own message only, never in the page.
5. It obeys a preference, defaulting to on, and it is suppressed entirely while a
   *papio* surface holds focus — the popup already reports the same event.
6. It is emitted only for a loss papio itself observed on a tab it opened. Never
   for a tab the researcher opened, and never for an ordinary job transition.

## Slices

**Slice 1 — SHIPPED. The shared renderer and the action contract.** One module producing
the toast DOM and one typed message (`papio.toast.action`) with `{ kind, job_id }`.
Unit tests only. No delivery yet.

**Slice 2 — SHIPPED. Route B, the always-available window.** `windows.create` with
`focused: false`, an extension page, the two labels, the eight-second timer, the
single-instance rule. This is the slice that makes the feature real.

**Slice 3 — NOT SHIPPED, pending the operator question below. Route A, the
in-page toast where permission exists.** Reuses Slice 1's
renderer, injected with the existing `scripting` permission, with a
`chrome.runtime.sendMessage` reply from the isolated world. Falls back to Route B
when the tab is not granted.

**Slice 4 — SHIPPED. Wire it to the tab-close branches**, per the truthfulness table above,
and to the institutional case with the second label.

**Slice 5 — SHIPPED. ADR-0023 Decision 12** recording the seventh surface and its bounds,
plus the disclosure edits, since a new page-visible surface changes what
`docs/privacy.md` and both store listings must say.

## What the build found that this plan had not priced

**No wire change was needed.** The plan implied the institutional action might
need a new daemon message. It does not: `handoff_link_request` already mints the
route `papio actions open` uses, and `requestFreshHandoffLink`
(`extension/src/background.ts`) already sends it. Both offered actions resolve to
that one call, so `internal/protocol/protocol.go`,
`extension/src/protocol.ts`, and `protocol/browser-v1.schema.json` are untouched.

**Two early returns the plan's branch table missed.** `onTabRemoved` returns for
an authorized-or-deliberate close and for a settled classify retry *before* the
four branches the plan tabulated. A toast raised above those returns fires every
time *papio* closes its own tab — housekeeping reported as loss. The raise
therefore sits after both, and a test drives the `deliberateRemovals` marker to
pin it.

**A recycled window id could have closed the researcher's own window.** A
researcher who closes the toast manually reports nothing, so the producer's
window id outlives the window. Removing that id later, after a browser reused it,
would close whatever now holds it. `closeToastWindow` therefore re-reads the
window and removes it only while it still holds exactly one tab showing the toast
page. Naming the new removal site in `extension/test/tab-window-close-ast.test.ts`
is what surfaced this: that allowlist exists to force exactly this review.

**The window size was a guess, and the guess clipped the button.** 420px — the
popup's width — was chosen for visual consistency. Measured in a real browser
against the built bundle: at 420 the institutional message wraps to FOUR lines
and needs 106px of inner height, and `windows.create`'s height includes the
platform frame, so 108 outer left roughly 80 inner and hid the action entirely.
At 520 both messages wrap to two lines and need 65 inner. The window is now
520x116, `toast.html` scrolls rather than clips as a safety net, and a test pins
the copy bound the measurement rests on so a longer sentence fails there rather
than in a researcher's browser. Unit tests could not have caught this: they
render into a detached document with no viewport.

**Driving a real unfocused window found two more things, one of them a defect.**
Measured in headed Chrome 152, scratch profile, via the identical
`windows.create` call:

| Question | Answer |
| --- | --- |
| Outer size honoured? | Yes — `520x116` exactly, `type: "popup"` |
| Inner size on macOS | `520x84`; the frame is 32px, and the copy does not scroll |
| Is `type: "popup"` load-bearing? | Yes. `type: "normal"` clamps to `520x375` |
| Does `focused: false` hold? | Yes. The reading window reported `focused: true` before, immediately after, and 1.5s later |

The defect: **on macOS the first click on an unfocused window is spent
activating it** and never reaches the button underneath, so a researcher who
noticed the toast at seven seconds lost the offer between their two clicks.
Being brought forward now restarts the clock, once — proven live: the window
was still open at 9.2s past load and gone by 12.2s. Bounded at once on purpose,
or a window cycled in and out of the foreground lives forever.

One measurement trap worth keeping: `--window-size`/`--window-position` on the
Chrome command line override `windows.create`'s width and height silently. The
first run reported `1100x760` for the toast and looked like a shipped defect;
it was the harness. Relaunching without those flags is what produced the real
numbers above.

**The suppression needed the injected clock.** `papioSurfaceLikelyFocused` first
used `Date.now()` rather than the bridge's `deps.now()` seam, so the stale-focus
test could not advance time and failed. Every time-dependent path in this file
uses the seam.

## Open question for the operator

Route A renders inside a publisher page papio already has permission for. That is
a page the researcher did not ask papio to draw on for a *background* event —
Decision 1 restricted the existing chip to the page bound to an action the
researcher had just taken, and this relaxes exactly that. Route B never touches a
page. If the in-page fidelity is not worth that relaxation, Slice 3 is droppable
and Slices 1, 2, 4, 5 still deliver the feature.
