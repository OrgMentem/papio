# ADR-0019: On-page bulk acquisition

Status: Proposed (2026-08-07). Consolidated from the integration consult
rounds r3/r4 (dev/scratch/oracle/) and the maintainer's UX picks; not yet
implemented.

## Context

The browser contributes something neither the CLI nor MCP can: a human
looking at a page bearing many papers — a reference list, a bibliography, a
syllabus, a table of contents — and visually choosing which ones matter. *papio*
already owns bulk acquisition, durable jobs, validation, and agent-driven
workflows; the browser's unique contribution is page-local selection input,
not a second acquisition policy.

The first draft of this feature (`dev/scratch/oracle/onpage-acquisition-plan.md`)
built that thesis into an opt-in, always-on **persistent scanner**: a
`scripting.registerContentScripts` grant walking every page on granted
origins, a `MutationObserver` for dynamic lists, and a toolbar badge showing
the running detected-paper count. That mechanism is **rejected**. It answers
"show me counts while I browse," not the stated problem, "I am looking at 40
papers right now and want to select some" — and it installs a standing
page-observation system before *papio* has any evidence that ambient
detection is worth its cost (r3 Topic 1 overall verdict). The product thesis
survives; the mechanism does not.

Two independent consult rounds reviewed the replacement design: r3, then r4
as an independent second sample against the same brief. Both rounds reached
the same top-line verdict — accept the thesis, replace the persistent
scanner with an explicit one-shot scan — and both flagged largely the same
defects in the superseded draft (badge collision, holdings-state
oversimplification, the 200/50 cap conflation, unqualified privacy and
review-tier claims). They diverged on two points: the selection sheet's
default checked state, and the granularity of consumer attribution. Both
splits are resolved by the maintainer below and the losing position is
recorded under *Rejected alternatives* with its argument.

Existing decisions constrain the design:

- **CLI-first.** No browser-only acquisition policy or read model may exist;
  the browser is a transport adapter over daemon capabilities the CLI can
  already reach (ADR-0001).
- **The full-tab extension page is the established surface for durable,
  multi-row daemon-backed work**, chosen over the popup (focus-loss
  dismissal) and the side panel (no cross-browser story) for exactly that
  reason (ADR-0001).
- **Sender validation is exact and operations are finite.** ADR-0001's
  security posture requires "no `{method, params}` pass-through, ever" and
  privileged runtime messages to require the extension's own ID and the
  exact page URL; content scripts and provider tabs cannot invoke them.
- **Badge precedence is fixed and documented**: "(1) disconnected/broken `!`,
  (2) blocking permission state, (3) pending triage count" (ADR-0001) — no
  other producer may claim the badge.
- **Only a fresh, explicit artifact-present claim may suppress acquisition**;
  a holdings source that fails or is incomplete yields `unknown`, never a
  negative ownership fact (ADR-0008, invariant 2).
- **`consumer` is a caller's accounting label, bounded to
  `[A-Za-z0-9._:@/+-]{1,128}`, never an identity or a per-source-page
  provenance field** (ADR-0014, Decision 1).
- **Institutional routing, including LibKey, already lives entirely in the
  daemon's acquisition waterfall**; the extension never holds routing logic
  and receives only a resolved browser route through the existing handoff
  contract (ADR-0016, Decision 1).
- **Browser frames are capped at 256 KiB** (`protocol.MaxBrowserMessageBytes`,
  ADR-0001, AGENTS.md).

## Decision 1: One-shot explicit scan, never ambient

Detection is invoked, not ambient. The popup gains **"Select papers on this
page"** beside the existing single-paper Acquire action. Clicking it runs one
top-frame `scripting.executeScript` under the temporary `activeTab` grant —
the same mechanism that already tests the exact user moment, rather than the
persistent registration that installs a standing observer (r3 Topic 1). There
is:

- **no persistent scanner** and no dynamic content-script registration;
- **no `MutationObserver`** — a page whose list changed (pagination, lazy
  load, an expanded accordion) is rescanned by pressing **Rescan**, an
  explicit control, not by a background watcher (r3 §C);
- **no badge count** — the badge's three-state precedence above is untouched;
  the detected count is shown only inside the selection workspace opened by
  the scan (r3 Topic 1, r4 defect 1);
- **no standing all-sites grant** — the scan uses only the ordinary
  `activeTab` grant already implied by clicking the extension action.

## Decision 2: Scanner consent is its own allowlist

An existing host permission — granted so *papio* can complete a requested
handoff or run an adapter against a publisher page — is **not** scanner
consent. A user who granted a publisher origin for one requested acquisition
did not thereby agree to have every ordinary page on that origin scanned for
citations (r4 defect 2). Even though the v1 scan is one-shot and
`activeTab`-gated rather than a standing grant, it still honors a
**scanner-scoped origin allowlist**, kept and revocable separately from
acquisition/adapter host grants, mirroring the existing separation between
"may inspect/download a requested paper on this host" and "may detect
identifiers on pages I browse here." Reusing one grant for the other purpose
is exactly the convenient-signal-mistaken-for-authority failure ADR-0013's
addenda already record for institution-session evidence; this decision
avoids repeating it for scanner consent.

## Decision 3: Detection is local-only, top-frame, container-scoped

Detection runs entirely in the content script against the top frame only —
no iframes — and never involves a network request (r3 §C, r4 §C). Recognition
order:

1. **Recognized links**: `doi.org/<doi>`, publisher `/doi/10.x` paths, arXiv
   `/abs/` and `/pdf/`, PubMed URLs.
2. **Explicitly labeled text**: a strict DOI (`10.\d{4,}/...`), `arXiv:<id>`,
   or `PMID: <digits>`. An unlabeled bare integer is **never** recognized as a
   PMID (r3 §C) — the false-positive direction is the harmful one here, as it
   is for holdings claims (ADR-0008).

Each identifier is associated with the nearest bounded citation-shaped
container — `li`, `tr`, `p`, `article`, or a result-card element — and up to
roughly 240 characters of that container's normalized visible text is
retained as a browser-local citation label. Titles are **required** for the
feature's own thesis, not decorative: without local citation text the human
cannot make the visual selection the feature exists to enable (r3 §C,
correcting the superseded plan's "titles are display sugar"). The label is
what the content script sends; it is never used as acquisition identity.

Other bounds:

- Raw candidates cap at **200** with explicit truncation reported, never
  silent (r3 §C, r4 §C).
- The page's own canonical DOI (`citation_doi`, canonical `<link>`, top-level
  article metadata) is **excluded** from the bulk list — the popup's existing
  Acquire/Send-PDF action already owns acquiring the page being read (r3 §C,
  r4 §C).
- `script`, `style`, hidden, and extension-injected nodes are ignored.
- The **daemon's existing identifier normalizers are the canonical
  validators**. The browser performs only local trailing-punctuation cleanup
  before display; it does not implement a second canonicalizer (r3 §C, r4
  §C).

r4 §C additionally proposed tolerating a `MutationObserver` for this
scan — bounded to the top document, debounced 500–1,000 ms, paused while
hidden, and stopped past 200 identifiers. That budget is a genuine
improvement *on a persistent observer*, but it is still a persistent
observer, and Decision 1 already rejects that mechanism outright in favor of
an explicit Rescan button. r4's budget is not adopted; see *Rejected
alternatives*.

## Decision 4: Surface — a full-tab selection workspace, one per scan

The selection surface is a new route inside the existing extension app
shell, not a new app, an injected in-page panel, or a side panel — the same
UI ADR that rejected the popup and side panel for durable multi-row work
governs here too (ADR-0001; r3 §A, r4 §A). One workspace opens per active
scan snapshot, addressed `?scan=<id>`, so scanning a second tab never
silently replaces an unfinished selection in the first (r3 §A). The snapshot
lives in ephemeral extension memory or `storage.session` — never
`chrome.storage`, and never the daemon — mirroring the triage inbox's
browser-local-overlay rule (ADR-0001).

The content script that ran the scan **never owns the mutation**. It hands a
bounded, local snapshot (identifiers, confidence, and label — never raw page
HTML, URL path, or surrounding prose) to the extension background, which
opens the workspace. All lookup and submit calls to the daemon originate from
that extension-origin page, whose sender the daemon validates by exact ID and
exact page URL, and which sends only the finite `page_bulk_status_request` /
`page_bulk_submit_request` operations below — never an opaque
`{method, params}` pass-through (r4 defect 6, applying ADR-0001's
exact-sender and finite-operation rules to a page an untrusted site
authored).

## Decision 5: Selection model — unselected by default, acquire-all shortcut

r3 §B and r4 §B disagree on the sheet's default checked state; the
maintainer's pick is r4's. Rows open **unselected**. The primary action reads
**"Acquire all *N* eligible"**; checking any row morphs it to **"Acquire *N*
selected"**. Neither path shows a confirmation dialog — the deliberate click
on a labeled action is the intentional human act (r4 §B, r3 §B agree on no
second confirmation). Submission is capped at **50** per batch, the existing
batch limit; the 200-item detection cap and the 50-item submit cap are
different limits governing different stages (r3 "Hard cap around 200",
r4 defect 5). Excess eligible rows are not auto-chained into further
batches — they remain selectable for the next submit (r3 §B, r4 §B).

Row states, populated from the daemon's existing ownership/job lookup:

| Local state | Treatment |
| --- | --- |
| `owned_with_pdf` | unchecked, disabled |
| `queued` (live job exists) | unchecked, disabled, shows job status |
| `owned_missing_pdf` | eligible, checkable |
| `record_present` (generic holdings, no PDF proof) | eligible, checkable |
| `ownership_incomplete` | eligible, checkable — incomplete is `unknown`, never a negative fact |
| `previously_unavailable` | eligible, checkable, labeled `No route on <date>` — information, never an exclusion |
| `invalid` | not selectable |

Only a fresh, explicit `owned_with_pdf` claim disables acquisition; every
other non-`queued` state stays eligible, per ADR-0008's invariant that
provider failure or an incomplete lookup never becomes a negative ownership
fact. "Previously unavailable" is historical, not durable exclusion — the
same route can open later as OA and holdings change (r3 "Previously
unavailable mark", r4 defect 3).

## Decision 6: Attribution — a stable consumer, provenance on the manifest

r3 §F and r4 §F disagree on granularity; the maintainer's pick is r3's. Every
page-bulk job carries the single stable value:

```text
consumer = "browser-page"
```

Per-source provenance is **not** folded into `consumer`. It is recorded on
the batch manifest instead:

```json
{
  "source": {
    "kind": "browser_page",
    "origin": "https://scholar.example.edu",
    "detector": "generic-identifiers/1"
  }
}
```

`origin` is the bare scheme+host only — never path, query, fragment, or page
title. `consumer` answers "which calling surface generated these jobs?";
`source.origin` answers "where did the input come from?" — ADR-0014 already
establishes those as different axes for consumer attribution, and
`acquire.submit_v3`'s frozen params make `consumer` the attribution field, so
page-bulk should not distort that vocabulary with a second meaning grafted
onto it (r3 §F). The daemon assigns `consumer`, not the extension — a content
script never supplies an attribution string that flows unvalidated into a job
row.

## Decision 7: Protocol — `page_bulk_acquire_v1` over existing services

A new negotiated feature, gated behind `hello_ack.features` per ADR-0001's
solicited-only rule:

```text
page_bulk_acquire_v1
```

carrying two strict request/reply families:

```text
page_bulk_status_request  / page_bulk_status_result
page_bulk_submit_request  / page_bulk_submit_result
```

`page_bulk_status_request` carries `scan_id` plus the scanned identifiers;
`page_bulk_status_result` returns, per identifier, a canonical key and a
status drawn from a closed vocabulary:

```text
eligible, owned_with_pdf, owned_missing_pdf, queued,
previously_unavailable, ownership_incomplete, invalid
```

`page_bulk_submit_request` carries only `scan_id` and up to 50 canonical
keys; the daemon assigns `consumer` (Decision 6) and creates one ordinary
batch through the existing `internal/batch` and ownership services. Neither
message is a new acquisition policy surface — both are thin transport
adapters over daemon capabilities the CLI can already reach, honoring the
CLI-first rule (ADR-0001; r3 "Product shape 3"). A job created this way
enters the same acquisition waterfall as any other submission, including
LibKey-routed institutional handoff where configured (ADR-0016, Decision 1) —
this ADR adds no acquisition-policy branch specific to the browser. Every
frame stays bounded under `protocol.MaxBrowserMessageBytes` (256 KiB,
ADR-0001, AGENTS.md).

## Decision 8: Privacy copy

The shipped copy is exact, not paraphrased at implementation time:

> *papio* does not read the page until you choose "Select papers on this
> page." Detection runs locally in that tab. The identifiers and short
> citation labels shown in the selection workspace may be sent to the *papio*
> application on your computer for local library and job matching. No
> scholarly service is contacted until you submit selected papers.

Two claims are explicitly refused:

- **"Nothing leaves the tab."** False once the daemon lookup exists — the
  status request sends identifiers and labels to the local daemon before
  submission (r3 Topic 1, correcting the superseded plan's wording).
- **A specific store-review tier.** No published Chrome or Mozilla rule makes
  the one-shot `activeTab` mechanism review-equivalent to, or exempt from,
  the scrutiny a broader content-script grant gets; only the claim that
  continuous background scanning would face is refused, not the (accurate)
  claim that this mechanism is narrower (r3 §G, r4 §G). The store listing and
  privacy documentation must disclose the user-facing scan function, since
  data practice — not the manifest diff — is what review considers.

## Decision 9: Explicitly excluded from v1

Recorded as deliberate, not deferred by omission:

- persistent scanning, badge counts, `MutationObserver`, standing all-sites
  grants (Decision 1);
- Scholar-specific extraction — the generic detector runs and reports "*N*
  identified papers found," never a completeness claim like "*N* of *M*
  results found." A later detector class 2 (source-controlled, fixture-backed,
  invoked only by the explicit scan action, limited to already-rendered
  cards, forbidden from navigating or paginating) may extract title-only
  candidates, but those need daemon identity resolution and are not this
  ADR's v1 (r3 §D, r4 §D);
- title-only submissions — v1 only ever submits validated identifiers.
  (Amended 2026-08-07, r5: the exclusion is evidence-based, not a permanent
  policy split with the CLI. `acquire --batch` rightly submits title-only
  entries because structured citation fields deliberately supplied as
  acquisition input are identity evidence; v1's detector produces only a
  bounded display label, which must never be relabeled as a title into
  enrichment. A detector class 2 emitting exact visible title + author
  evidence + year + detector identity + source-record binding submits
  through the same batch path under the same enrichment/validation gates —
  no new caution policy.);
- an availability preflight. A later selected-only **"Check routes for *N*
  selected"** action is coherent product shape, but not v1: it would report
  `Open copy found`, `Institution route configured`, or `No route yet` and
  must never report `You have access` — a routed link is not proof of the
  current browser user's entitlement (r3 "The honest middle"). In v1,
  **Acquire selected** already performs this work through the ordinary
  waterfall;
- DOM-based watches. A watch is created only from a durable structured
  source the existing discovery backends already understand — RSS/Atom,
  ISSN, proceedings identifier, author identifier, or a saved query — never
  from "revisit this URL and rescan the DOM." That is crawling under another
  name (r3 §E, r4 §E);
- multi-batch auto-chaining and collection pickers (Decision 5).

**Pre-click, per-link availability badges are permanently out**, not merely
deferred. That is the trade LibKey Nomad makes — per-page external lookups
to disclose availability before a click — and it is the trade *papio*
refuses under its current privacy posture (superseded plan, "Explicitly out
of scope"; r3/r4 "Product shape 1"). *papio*'s honest positioning is
narrower than an unqualified Nomad replacement: it replaces the
acquisition-and-filing workflow for papers a human selects, not ambient
availability disclosure while browsing (r3, r4 "Product shape 1").

## Decision 10: Measurement — local-only, URL-free run records

Every scan persists one local, URL-free `page_bulk_runs` record (origin only,
no path/query/title; no telemetry leaves the machine):

```text
page_bulk_runs
  id, detector_id, source_origin, detected_raw, canonical_unique,
  eligible, owned_with_pdf, owned_missing_pdf, queued,
  ownership_incomplete, selected, submitted, invalid,
  batch_id, opened_at, submitted_at
```

Primary thesis metric:

```text
bulk leverage = median works submitted per completed selection sheet
```

If that median is two, the feature is not solving a bulk problem (r3
"Measurement: expand or stop"). Supporting metrics: useful scan rate (scans
with ≥3 eligible works / scans opened), submit conversion, canonicalization
yield, selected validity, autonomous ready rate, rescan rate.

**Pilot gate** (run until all hold): ≥25 scan sessions, ≥5 distinct
site/page classes, ≥100 canonical detected works.

**Expand** detector coverage when: ≥40% of useful scans are submitted; median
submitted batch ≥4; ≥95% of selected items canonicalize into a created,
joined, or positively owned work; audited false detections ≤5%.

**Retreat** to the exact-identifier one-shot scanner only, when: <20% of
useful scans are submitted, or median submitted batch <3, or audited false
detections >10%.

**Reconsider persistent scanning** only if rescans occur in ≥25% of useful
sessions — and until that evidence exists, "on-demand scanning is not an MVP
compromise; it is the finished product" (r3).

## Rejected alternatives

**Persistent opt-in scanner** (the superseded draft's starting mechanism).
Solves "show me counts while browsing," not the stated problem of selecting
from a page already open; installs a standing page-observation system before
there is evidence ambient detection has value. Superseded by Decision 1's
one-shot `activeTab` scan (r3 Topic 1).

**`MutationObserver` with an operational budget** (r4 §C). r4 proposed
bounding a persistent observer — top document only, 500–1,000 ms debounce,
paused while hidden, stopped past 200 identifiers — as an amendment rather
than a rejection of observation itself. Not adopted: it is still a standing
observer, and Decision 1 already replaces the entire class with an explicit
**Rescan** button, which needs no lifecycle, debounce, or visibility-pause
logic at all (r3 §C).

**Injected in-page panel.** Puts acquisition controls into DOM the host page
controls — CSS collision, per-site layout defects, and a much harder
sender-authenticity boundary even behind Shadow DOM. Rejected in favor of the
full-tab extension workspace (r3 §A, r4 §A).

**Chrome Side Panel / Firefox sidebar APIs.** Unnecessary Chrome/Firefox API
asymmetry for v1, the same reason ADR-0001 rejected the side panel for the
triage inbox (r3 §A, r4 §A).

**Popup as the selection list.** A 40-row selection cannot survive popup
focus loss — the same reasoning ADR-0001 already recorded for the triage
inbox (r3 §A, r4 §A).

**Per-host consumers**, `browser-page:<host>` (r4 §F). Would create one
consumer key per site with no bound on how many, making `--consumer`
filtering noisy, and conflates "which calling surface generated this job"
with "where did the input come from" — two different axes under ADR-0014.
Superseded by Decision 6's single stable `consumer = "browser-page"` plus
origin/detector in batch provenance (r3 §F).

**Preselected-all default** (r3 §B). r3's original position opened the sheet
with every eligible row checked, on the reasoning that bulk efficiency is the
point. Rejected in favor of r4's unselected-by-default model: preselecting
everything trades away the visual-selection thesis this feature exists to
serve for a marginal click, and risks an accidental large batch from one
missed uncheck. The **Acquire all *N* eligible** shortcut (Decision 5)
preserves one-click bulk without that risk.

**Context-menu single acquisition.** Dropped by the superseded draft itself,
before this ADR: redundant with the already-shipped single-page popup
acquire action and with agent/MCP-driven single-work submission.

**Nomad-style pre-click availability overlay.** Requires a per-link external
lookup before the user has chosen anything — exactly the network-backed
availability-disclosure product *papio* declines to build, since its
detection is local by design (Decision 9; superseded plan's "structural
difference from LibKey Nomad"; r3/r4 "Product shape 1").

## Consequences

- **Protocol.** A new negotiated feature and two strict request/reply
  families join `papio-browser/1` without a protocol-string bump, per
  ADR-0001's negotiated-immutable-schema rule. Both parsers (Go and
  TypeScript) need fixtures for the closed status vocabulary, the 200/50
  boundary, and truncation reporting, and both sides pin the 256 KiB frame
  cap the way `TestSyncRequestFitsMaxBrowserFrame` already pins it for other
  frame types.
- **Store and privacy docs.** Shipping the scan requires updated Chrome/AMO
  listing copy, an updated privacy policy describing local detection and
  what crosses to the local daemon, an options-page explanation of the
  scanner allowlist (Decision 2), and a permission-use disclosure — the
  existing privacy text describing "reads library/publisher pages for a
  specific job, no bulk scraping" no longer describes the product once this
  ships (r4 §G).
- **Extension release.** This is a store-review-bearing release like the
  triage inbox before it (ADR-0001): new UI route, new permission-adjacent
  behavior even without a new manifest permission, and a CWS/AMO review pass
  before it reaches users.
- **No new acquisition policy.** Submitted jobs are ordinary batch jobs; they
  inherit the configured resolver, access mode, budgets, source policy,
  auto-import defaults, and — where configured — LibKey-routed institutional
  handoff exactly as any CLI- or MCP-submitted job does (ADR-0016). Nothing
  about how a job is acquired differs because it originated from a page scan.
- **Holdings and consumer semantics are reused, not reinvented.** Row
  eligibility rests on the existing holdings aggregator's positive-evidence
  model (ADR-0008); attribution rests on the existing `consumer` accounting
  axis (ADR-0014). Neither gains a browser-specific carve-out.
- **Measurement is an obligation, not a courtesy.** The pilot gate in
  Decision 10 must be met before detector coverage expands, and the retreat
  condition is a real off-ramp back to the exact-identifier scanner alone —
  this ADR does not commit to ever building detector class 2, a Scholar
  extractor, or an availability preflight; each remains gated on its own
  future decision.
- **What this ADR deliberately leaves open.** Detector class 2 (source-
  controlled, fixture-backed result-card extraction for Scholar-like pages),
  the selected-only "Check routes" preflight, and structured-source watch
  creation from a scanned page are named future work, not silently dropped —
  see Decision 9.
