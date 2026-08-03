# ADR-0013: Operator experience: activity feed, send-to-*papio* delivery, session visibility

Status: Accepted (2026-08-03)

## Context

Phase 1 adds the operator surfaces around an acquisition that is already in
flight: a daemon activity feed, a PDF delivery action in the browser popup,
and browser-local visibility into an institution session. The surfaces answer
three different questions without moving authority out of the existing
boundaries:

- **What did *papio* do?** The daemon already has an append-only `events` table,
  but there was no bounded operator view of it. The inbox needs recent,
  display-safe activity without turning the browser bridge into a subscription
  transport.
- **How do I finish a page that *papio* cannot fetch?** A raw PDF page is already
  a browser-mediated success from the operator's point of view. Requiring a
  second manual-download handoff leaves the operator with a file and no clear
  adoption path. The browser can steer a download into a job-scoped directory;
  the daemon can then run its existing adoption and validation pipeline.
- **Is my institution session usable?** The extension can observe its own
  resolver tab, authentication return, and keep-warm state. The daemon must not
  become an identity-provider observer or a credential holder.

The work also has a compatibility constraint. `papio-browser/1` is strict and
feature-negotiated: old parsers reject unknown fields and unsolicited messages.
The CLI remains the single source of truth for daemon capabilities, while the
extension remains the reporter of browser-local facts. The Zotero Connector is
useful prior art for operator affordances, but its source is AGPLv3, so its
implementation is not a drop-in dependency for this MIT project.

## Options

### A. Reopen live daemon push for the activity panel (rejected)

ADR-0005 already built and measured a derived-fingerprint push design and
rejected it. This phase does not reopen that decision. The activity feed is a
solicited pull (`activity_request` / `activity_response`) and the inbox keeps
its page-local polling. The exact ADR-0005 reopen test remains:

> A genuinely latency-sensitive surface — for example live per-job acquisition
> progress, where seconds visibly matter while someone is watching a job run —
> or a multi-device / shared-inbox scenario, where one browser's local poll
> cannot observe another session's writes at all. Neither exists in *papio*
> today.

Those conditions are still unmet. Live per-job progress may meet the first
condition later; this activity timeline is not that feature and does not create
an exception to ADR-0005's no-push decision.

### B. Leave PDF pages as manual-download handoffs (rejected)

Rejected: it makes the operator discover and move a file by hand, and it leaves
a raw PDF page without a clear relationship to a live job. A browser-steered,
job-scoped download reuses the daemon's adoption boundary instead of inventing a
second artifact path or acquisition state.

### C. Reuse Zotero Connector implementation (rejected for now)

The Zotero Connector repository declares the GNU Affero General Public License,
version 3 (AGPLv3), in `COPYING`. Copying its implementation into the extension
would therefore require a deliberate licensing and attribution decision. The
option of relicensing *papio* to AGPL to enable code reuse was considered and
declined for now. A future relicensing decision is possible, but it must be
explicit; it is not implied by borrowing product ideas.

### D. Keep the browser ordinary and adopt through the existing boundary (accepted)

Accepted: the extension owns browser-local interaction and steers only the
operator's ordinary browser. The daemon owns jobs, files, validation, and
transitions. New wire messages are added only where a typed, feature-gated,
solicited exchange is necessary; existing messages are not widened.

## Decision

### Activity is a solicited, bounded pull

The daemon exposes its durable event read model as `activity.list`, and the
CLI exposes the same capability as `papio activity` (with bounded `--limit` and
optional `--job` filtering). This honors ADR-0001's CLI-first rule: the browser
activity panel consumes a capability the CLI can already reach; it does not
invent a browser-only read model.

The extension protocol adds `activity_request` and `activity_response` under the
`activity_feed_v1` feature. A request is sent only after `hello_ack.features`
advertises that feature, carries a correlation id, and receives one bounded,
newest-first page from the daemon events table. The bridge emits display-only
`{seq, at, job_id?, kind, text, title?}` entries; the extension renders text as
untrusted text. When the feature is absent, the inbox reports activity as
unavailable rather than guessing from job state. There is no daemon-originated
push, subscription, watermark, or background activity stream in this ADR: the
inbox polls while its page is open, and the CLI remains the authoritative
operator path.

### Early browser delivery uses legal adoption transitions

The popup can classify the current page as a PDF and offer **Send PDF to
papio**. It creates or associates a job through the existing page-acquire path,
then asks the browser to download the PDF into the job-scoped relative path
`papio/<job_id>/paper.pdf`. The bytes stay in the browser's configured download
folder and never cross native messaging; download frames carry only bounded
metadata and the filename needed to correlate adoption.

The same adoption path is available when the extension steers a download for a
live queued, resolving, or fetching job. The daemon uses existing legal
transitions: a queued job first moves to resolving, and a resolving or fetching
job moves to `awaiting_human`; no new acquisition state and no synthetic human
action are introduced. The job-scoped file is copied into quarantine and passes
the ordinary payload, structure, and identity validation pipeline. It can become
`ready`, enter `needs_review`, or remain `awaiting_human` with a fresh
`manual_download` action when the supplied file is rejected.

A download frame for a live job that has not been offered to this browser is an
ordinary, structured outcome. Missing files, rename races, a file saved
elsewhere, and confinement/adoption failures are recorded as deferred adoption
work and acknowledged without tearing down the native-messaging session; the
next directory sweep can retry a file that arrives late. A persistent daemon
failure is still an error, but an operator-facing download race is never allowed
to become a session-fatal shortcut.

Browser adoption preserves the invariant from ADR-0007:

> **`unknown` is a real value, not a default.**

> Browser adoption records `unknown` unconditionally (`internal/app/browser_adopt.go`): it observes bytes arriving from a human's browser and never learns which version that human chose.

It does not infer a version from the requested version, page URL, or access
mode.

Recording the true `access_basis` and `route` at adoption time is deliberately
deferred. ADR-0010 states:

> **`operator_browser_session` therefore has no producer yet, and that is deliberate.**

and:

> The enum value stays reserved; recording the true basis and route at adoption time is the fix that gives it a producer.

That future producer needs a new, typed message type. It MUST NOT widen an
existing message or result: strict old parsers must continue to receive no new
fields, and the new message must be independently feature-gated and solicited.
Until then, adoption records the safe, observed facts only and does not claim an
institutional route that *papio* did not observe.

### Login visibility is a browser-local overlay

ADR-0001 defines the page model as a daemon snapshot merged with a browser-local
overlay:

> The page merges that snapshot with a **browser-local overlay** (connection status, permissions, live handoff-tab availability, focus-this-tab) joined by `job_id`.

The popup's institution-session card is an extension report of that overlay. It
can say whether keep-warm is off, the local resolver session is warm, sign-in is
needed, or a sign-in has unblocked queued work. The options page stores the
keep-warm enabled flag and refresh interval in extension-local settings. Neither
those settings nor the card's login state crosses the wire, and *papio* never
receives credentials, page content, or identity-provider hosts. The existing
authentication frames are the narrow exception: `auth_pending` and
`auth_returned` carry timing-only facts, never a URL, host, title, query, or
fragment.

Keep-warm remains an ordinary browser session: the extension refreshes one
pinned resolver tab while institutional work is open, pauses when the tab is on
a sign-in page, and brings that tab forward for the operator. It never fills a
login form, copies cookies, or treats a successful timing frame as an identity
assertion.

#### Addendum (2026-08-03): the session verdict is evidence-based, tri-state, and introspectable

Field testing on Primo NDE showed URL-shape heuristics asserting "signed out"
at an operator who was looking at their signed-in library account page. The
card's verdict is therefore grounded in ranked observations, never inferred
from URL shape alone:

1. **Explicit sign-out affordance** in the resolver tab's DOM (whole-document
   scan: text, `href`/`formaction` targets, ARIA labels, closed menus) —
   signed in. A sign-in prompt with no sign-out affordance — signed out.
2. **Web-storage JWT identity** — an Ex Libris-style session token whose
   payload carries a named-user claim, or a subject plus an explicit non-guest
   group claim, is signed-in evidence. A bare `sub` claim is NOT identity:
   anonymous session tokens carry opaque subjects on many platforms, and a
   false "signed in" is the harmful direction.
3. **Anything else is `unknown`, stated as such.** "No tab to inspect", "scan
   found no indicators", and "could not read the page" are three different
   honest answers with three different remedies, and the card names its
   evidence source and time ("via your open library tab · 12:38 pm") so a
   wrong verdict identifies the lying branch from a screenshot.

Probes prefer the operator's focused tab over URL-pattern tab queries, and a
completed probe that inspected no tab never latches behind the freshness
window. Hardening for further institutions follows the adapter fixture loop —
a captured page pins the classifier in tests — not speculative vendor tables.

#### Addendum (2026-08-03, later): ownership is ledgered and institution sessions are origin-scoped

Later field validation exposed two places where a convenient browser-local
signal had been mistaken for authority.

**Managed-tab ownership is durable creation evidence, not group membership.**
The broker records only tabs that *papio* created in a durable ledger; a reused
tab is never ledgered. The papio-titled tab group and the dedicated work window
are managed surfaces, not proofs of ownership. After startup has allowed the
daemon to reclaim live work, delayed reconciliation passes automatically close
untracked, ledger-owned leftovers that remain in either surface. Ledger-owned
tabs that have strayed outside those surfaces are the only tabs shown in the
operator cleanup card. Tracked, active, and pinned tabs remain protected, and
an unledgered tab is never closed merely because it belongs to a papio-titled
group.

**Institution sessions are scoped by resolver origin end to end.** A field
incident produced phantom institution rows because provider offer URLs, and
then broad host-permission grants, were folded into the session-card origin
set. The row universe now comes exclusively from the config-derived
`hello_ack.resolver_origins`; offer traffic and permission grants never create
an institution. Session evidence carries a bare `origin_hint`. The extension
releases only queued handoffs whose resolver origin matches that evidence, and
the daemon maps the hint back to a configured resolver profile before
re-offering store-backed siblings from that same profile. An unknown hint fails
closed. Evidence for one institution therefore cannot release either the
extension queue or daemon-side parked work for another.

### Authentication retry is an explicit local budget reset

When a handoff is auth-stalled, the popup/inbox **Retry** control is an explicit
operator action. It clears that job's extension-local authentication-attempt
budget and re-drives the retained resolver URL (or opens the tracked handoff),
then charges future drives normally. It does not ask the daemon to drain human
work and does not create an autonomous retry loop.

This preserves ADR-0009's boundary verbatim:

> **Autonomous drain** is not ratified. A background consumer must not resolve, open, or retry human work on its own: its view can be stale and the action is intentionally operator-mediated.

### Zotero Connector research is clean-room, ideas only

The Zotero Connector research confirmed the AGPLv3 license and informed the
shape of this surface without copying implementation, selectors, translators,
or code. The adopted ideas are:

- **saveability-driven action states** — show the operator what can be saved or
  what decision blocks saving, rather than presenting an opaque generic action;
- **send-current-PDF** — make the PDF currently open in the browser an explicit
  save/delivery action;
- **progress visibility** — show an in-flight operation and its outcome instead
  of silently handing work to a background process; and
- **layered DOI fallback** — use the strongest page metadata first and fall back
  through stable, weaker signals when classifying the current page.

These are product ideas implemented independently under *papio*'s existing
protocol, browser-permission, and provenance rules. Reusing Connector code would
require the future, deliberate relicensing decision described above.

### Adapter gaps are an explicit, local evidence loop

A stable non-authentication landing in a governed resolver tab is provider
evidence even when its host is absent from the protocol's capped legacy list
and from the adapter registry. It gets one bounded render window. If no adapter
appears, the extension attempts the existing sanitized `observed` capture,
reports `ui_changed` over the existing negotiated message, and releases the
browser-drive slot. The daemon turns that outcome into a `manual_download`
action whose copy names the missing adapter and whether a local diagnostic was
saved.

A missing host permission remains a different, non-terminal condition: the
popup names the provider host and requests that exact HTTPS origin from the
operator's click. The governed tab and job remain live, and the permission
change resumes classification on that exact tab.

This is the low-effort contribution boundary:

1. *papio* captures and retains diagnostic evidence locally; an operator or
   agent can inspect it with `papio adapter captures`.
2. A contributor supplies a capture reference, not guessed selectors. An agent
   or maintainer turns that fixture into the declarative adapter spec and
   observable tests.
3. CI keeps the existing fixture-backed registry invariant: no adapter enters
   the registry without captured evidence and matching classification/download
   tests.

There is deliberately no automatic upload, issue creation, telemetry, or
community sharing. Sanitization removes secrets and unstable values; it does
not prove that the remaining page text is appropriate to redistribute. A
future `papio adapter contribute` command may prepare a minimal review bundle,
but it must re-run the leak scanner, show the exact files and destination, let
the operator remove page content, and require an explicit final publish action.

## Consequences

Positive:

- Operators can inspect durable daemon activity from both `papio activity` and
  the inbox without accepting a daemon push transport.
- A PDF already open in the ordinary browser has a visible, one-action route to
  a job-scoped adoption directory and the same validation bar as a fetched PDF.
- Live queued/resolving/fetching jobs can converge through existing transitions;
  there is no parallel adoption state machine to recover.
- The institution card makes the extension's local session and keep-warm choice
  visible without turning an identity provider into a daemon data source.
- Auth-stalled recovery remains human-mediated and bounded, so a stale browser
  view cannot autonomously reopen or drain human work.
- The implementation captures useful Connector affordances while preserving
  *papio*'s MIT licensing boundary and clean-room provenance.

Negative / obligations:

- Activity is bounded and pull-based. An open inbox gets fresh entries through
  its own polling; the daemon does not wake a sleeping extension to push a
  timeline, and an older daemon simply reports the feature as unavailable.
- Browser download adoption depends on the browser's filesystem behavior. On
  Chrome the filename-routing hook can steer provider downloads into
  `papio/<job_id>/`; Firefox has no `downloads.onDeterminingFilename`, so click
  adapters remain human-assisted and only exact extension-started downloads are
  owned. The direct popup PDF delivery still names its job-scoped download, but
  a provider click that Firefox cannot route must be saved into the job's
  adoption location before *papio* can adopt it.
- The adoption path reports only observed facts. Version remains `unknown`, and
  the true access basis/route cannot be exported as `operator_browser_session`
  until a future message type records them at the moment of adoption.
- Extension-local session state can be lost or reset with browser storage and
  service-worker lifecycle; the daemon's job/events state remains authoritative
  for durable outcomes.
- Protocol additions require Go, TypeScript, schema, fixtures, feature
  negotiation, and skew tests. Any future route/access-basis producer must add a
  message rather than widening one.
- Activity and browser outcome text is untrusted display data: render it as
  text, validate links as `https`, and preserve semantic controls, live-region
  announcements, and keyboard rules from ADR-0001.

## Tripwires

- **No push by accretion.** Reopen ADR-0005 only if its quoted latency-sensitive
  per-job-progress or multi-device/shared-inbox evidence exists. A request for a
  prettier timeline or a shorter poll interval is not enough.
- **No provenance inference.** If a browser adoption implementation wants to
  fill `access_basis`, `route`, or `operator_browser_session` from configuration,
  a requested version, or a current URL, stop and design the new typed message
  first. The observed-at-adoption fact must be captured without widening an
  existing message.
- **No version synthesis.** The ADR-0007 `unknown` rule is load-bearing; a page
  or desired-version preference is not evidence of the version obtained.
- **No autonomous drain.** Auth retry, open, resolve, and retry controls remain
  explicit operator actions. A background retry, even if bounded, requires a
  new ADR against ADR-0009.
- **No license drift.** Connector implementation reuse or relicensing *papio* to
  AGPL is a future legal/product decision, not an implied consequence of these
  clean-room ideas.
- **No identity-provider leakage.** Timing-only auth frames must remain timing
  only; URLs, hosts, titles, query strings, credentials, page contents, and
  signed links never enter the daemon activity feed through the session card.
