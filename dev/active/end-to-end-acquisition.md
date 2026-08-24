# End-to-end acquisition: the queue is waiting on adapters

Status: **proposal, not ratified.** Written 2026-08-24 against HEAD `9660d38`.
Third revision, after five adversarial reviews refuted most of the first two.

Operator intent: *"I want papio to do everything it can to get all papers, all
the time, unless I have specified conservative mode."*

---

## 1. What the reviews changed

The first two drafts blamed gate conservatism and proposed to widen drive
authority. That diagnosis was wrong in its central claim.

**The recovery mechanism already exists and already covers hand-fetches.**
`RepairAdapterUpgrade` (`internal/app/handoff_repair.go:148-249`) "returns
manual-download parks to resolving when a live browser session proves that the
adapter which stranded them is newer". It gates on `allManualDownloads(open)`,
scopes the comparison to the captured adapter id so an unrelated adapter cannot
churn a park, and uses the transition event as "both the audit record and the
durable one-shot latch". The bridge invokes it on a live holder
(`internal/browser/bridge.go:1179,1258-1288`), and
`internal/app/handoff_repair_test.go:482-565` proves an exact ScienceDirect
upgrade retries once, does not retry again, and ignores unrelated upgrades.

So the claim "nothing retries a parked hand-fetch" was false, and the proposed
recovery phase was already shipped. **Parked papers are waiting on an adapter
version increase that no one has shipped.** That reorders the whole plan: adapter
work is not one phase among five, it is the release valve for the existing queue.

**The hand-fetch queue is not a drift queue.** `classify()`
(`extension/src/plan.ts:375-459`) returns `unknown` whenever no rule
matches, and `internal/browser/bridge.go:8000-8037` labels any `ui_changed`
carrying an adapter id as `provider_adapter_drift`. But a legitimate unknown is
common by design: ProQuest's spec comments
(`extension/src/adapters/types.ts:383-409`) say citation-only, HTML-only, and
unentitled pages stay unknown, and Primo declares only an `article` rule
(`extension/src/adapters/types.ts:529-545`). A page the operator has no
entitlement to is therefore
counted as drift. Repairing selectors against that population could be wasted
work.

---

## 2. Evidence, with its limits stated

### 2.1 Solid

**Papers that park do not come back.** Of 115 papers in `awaiting_human`, **111
are more than a week old**, 28 are more than three weeks old, and only 4 are
younger than a week.

**papio stops one step short, broadly.** `openurl_handoff -> manual_download`
occurs **182 times across 141 distinct jobs**; 113 jobs saw it once. Not churn
on a few papers.

**29 open hand-fetches were submitted as `delegated`.** `policy_json` records
intent at submit; `EffectiveAccessMode` (`internal/config/config.go:1417-1441`)
re-clamps on read, so this proves what the operator asked for, not today's
effective ceiling.

### 2.2 Corrected

**"62% of filed papers cost zero human steps" was wrong.** It conditioned on
`imported` and omitted 346 terminal failures. Against
`imported + cancelled + unavailable` the rate is **191/652 = 29.3%**; against
all 801 jobs, 23.8%. And the 191 are mostly not new acquisitions: matching
`zotio` outcomes are `no_op` 124, `duplicate` 62, `applied` 5, with
`internal/zotio/plan.go:548-550` defining `duplicate` as Zotero already holding
the PDF. Only 5 show an applied attachment. Separately, **100 of the 191 came
from a 199-member `source_kind='browser_page'` batch** — the operator explicitly
acquiring pages. Zero human *actions* is not zero operator effort.

**"Adapter drift is 114 of 237 hand-fetches" is unsupported.** Commit `af0ab60`
(2026-07-27) assigned the detail string `papio could not drive the provider page`
to every `ui_changed` outcome without inspecting `AdapterID`, so a page on a host
with no adapter got the same wording as a drifted selector; `f70a79a`
(2026-08-12) split them. 102 of 135 such rows predate
attribution. Only **21 carry an explicit `provider_adapter_drift` diagnosis**.
Attribution begins 2026-08-11 with commit `197924d`, so both tables below
describe six weeks, not the store's life — and per §1 they count unentitled
pages as drift.

`ui_changed` events carrying an adapter id, all time:

| Adapter | drift events | note |
|---|---|---|
| `primo` | 18 | discovery layer, not a publisher |
| `sciencedirect` | 11 | never verified end to end |
| `proquest` | 4 | spec expects unknown for unentitled pages |
| `wiley` | 2 | |
| `jamanetwork` | 1 | no open paper remains |

The **repair queue** is the more useful table, and it differs: papers with an
open hand-fetch today, by attributed adapter.

| Adapter | open papers |
|---|---|
| `primo` | 18 |
| `sciencedirect` | 7 |
| `proquest` | 4 |
| `wiley` | 2 |

Both tables are reproducible, and the filter is the reason earlier drafts
disagreed with themselves. Counting explicit `provider_adapter_drift`
diagnoses, counting detail strings, and counting outcome events each give a
different answer, because diagnosis was added after most rows were written.

```sql
-- drift events, all time
SELECT json_extract(detail_json,'$.adapter_id') AS a, COUNT(*)
FROM events WHERE kind='browser.provider_outcome'
  AND json_extract(detail_json,'$.outcome')='ui_changed'
  AND json_extract(detail_json,'$.adapter_id') IS NOT NULL
GROUP BY a ORDER BY 2 DESC;

-- repair queue: open hand-fetches whose job has an attributed outcome
SELECT json_extract(e.detail_json,'$.adapter_id') AS a,
       COUNT(DISTINCT ha.job_id)
FROM human_actions ha JOIN events e ON e.job_id=ha.job_id
WHERE ha.kind='manual_download' AND ha.status='open'
  AND e.kind='browser.provider_outcome'
  AND json_extract(e.detail_json,'$.adapter_id') IS NOT NULL
GROUP BY a ORDER BY 2 DESC;
```

**"50 validation failures were human-supplied" is not measurable.**
`internal/app/browser_adopt.go:139-205` stores every adopted file as
`source='browser'`, `access_basis='manual'`, with no initiator;
`extension/src/background.ts:19847-19890` tags generic, direct, and
institutional producers but never a provider `click`. Of 67 validation-failure
actions, **one** has a producer-tagged download event.

**ScienceDirect has never worked end to end.** `a4aeab3` added a hand-built
synthetic fixture without live verification; `e6ff3e4` replaced it with a real
capture on 2026-08-09; `dev/active/acquisition-stack-remainders.md:19` still
records it unverified with no validated artifact reaching `ready`. Its own spec
comments (`extension/src/adapters/types.ts:690-697`) note a bare `/pdfft`
returns a cookie notice, and
adoption depends on new-window correlation
(`extension/src/background.ts:16698-16733`).

---

## 3. The design correction worth keeping

The first draft proposed granting drive authority to an already-parked
hand-fetch. Review killed it correctly: `LiveEffectPermit` serialises **papio's
own** effects and cannot fence a human clicking in the same tab, so this really
does race the person papio just asked, exactly as commit `041db18` recorded.

The remedy is ordering, not a new protocol:

> **papio attempts first, and asks only after its attempt failed.**

This removes the *explicit handoff* race, which is the one commit `041db18`
named: papio is not competing with a person it just asked to fetch the file.
It does not remove all operator interference. A delegated offer still opens a
provider tab, and the operator can focus or click in it while papio's
revalidation is in flight, because `LiveEffectPermit` serialises papio's effects
and not the operator's events. Ordering narrows the race to that residue, and
`manual_download` then means what it says: papio tried and could not.

---

## 4. Phases

### Phase 1 — Provenance on adopted files

Nothing downstream is measurable without it, and it is three changes rather than
one. Review found each.

**The producer union is closed.** `internal/job/effect_permit.go:20-24` admits
only `generic_drive`, `direct_get`, and `institutional`, and its `default` branch
rejects anything else with "artifact producer kind cannot produce a job-scoped
artifact" (`:135`). The same union is fixed in
`extension/src/protocol.ts`, `protocol/browser-v1.schema.json`, and
`internal/protocol/protocol.go`. So a `provider_click` producer is a dual Go and
TypeScript protocol change plus a schema change, and both parsers reject unknown
fields, so it needs the usual same-commit treatment. A separate initiator field
may be cheaper than widening the union — decide that first.

**The failure path never reaches the context code.**
`AdoptDownloadWithContextCandidate` (`internal/app/browser_adopt.go:357-380`)
calls `AdoptDownloadCandidate` at `:364` and returns at `:366` on a validation
error, applying delivery context only at `:372` after success. Every one of the
67 validation failures took the early return, so an initiator added to the
post-adoption path can never appear on a rejected file — which is the exact
population Phase 5 needs. The initiator must be written before validation, or
the failure action must carry a durable candidate or download id.

**ScienceDirect loses provenance between two effects.** The adapter click
(`extension/src/background.ts:19149-19177`) sets `download_initiated` and
returns; `maybeAdoptViewerTab` then begins a *separate* `downloads.download`
(`:17511-17533`) whose `DownloadTrack` carries only ids, and
`artifactProducer(track)` (`:19847-19890`) sees none of the click's context.
Tagging the click alone still files the ScienceDirect bytes as `unknown`.

**It also needs a migration.** Neither `candidates` nor `human_actions` carries an
initiator column today, and `AdoptDownload` inserts only the synthetic browser URL
and access basis. So Phase 1 costs a numbered migration under
`internal/store/migrations/`, the four `user_version` assertions AGENTS.md
enumerates, the two historical fixtures that must **not** be bumped, plus Store
struct, query, and scan changes.

*Acceptance:* a new adopted file names its initiator, **including a file that
fails validation**, and including a ScienceDirect file adopted through the viewer
tab. Pre-existing rows read `unknown` and are counted as neither.

### Phase 2 — Separate "page changed" from "no PDF here"

The precondition for all adapter work. Today both produce `unknown`, and the
daemon labels both drift. Split the verdict so an unentitled or citation-only
page is reported as itself, and reserve drift for a page that carries an article
shape no rule matched.

`extension/src/observe.ts:30-45,184-223` already captures unknown pages, capped
at one per host and adapter version per hour, 20 per day, keeping three digests.
Nothing promotes those captures for comparison. Surface them, so a real drift is
visible before it becomes a hand-fetch.

**First, the drift latch must record which page defeated papio. SHIPPED
2026-08-24 in `ffe8ffb` and `4950ab7`** — `provider_outcome` now carries an
optional sanitized `host`, validated in all three protocol legs and pinned by
nine shared corpus fixtures, and the daemon records it on the drift latch and
on the durable outcome event. It is not purely diagnostic:
`directRouteCandidates` matches that host against each compiled route's
`AllowedOrigin`, so an attributed outcome can now select a direct route papio
could not previously find. **No live drift has yet been observed carrying a
host**, and the emit fails empty by design, so a systematic failure would look
exactly like success — confirm against a real provider page before trusting any
ranking built on it. Diagnosing the
operator's own reported paper found the reason the queue cannot be trusted. Job
`job_33be7342943fa7604f4d06e939` — the ScienceDirect paper from their report —
parked on 2026-08-11 with a `job.latch` event carrying
`{"adapter_id":"","adapter_version":"","host":"","kind":"drift"}` and a provider
outcome reading "No source-controlled adapter matched this provider page." Its
only route evidence is the offer's `safety_domain`,
`institution:une.primo.exlibrisgroup.com`.

So papio recorded a drift with no host, no adapter, and no capture. That is not a
historical gap: host matching is suffix-based
(`extension/src/adapters/types.ts:1376`, `page.hostname === host ||
page.hostname.endsWith('.' + host)`), and the `primo` adapter shipped 2026-08-03
in `258727d`, eight days before this park — so `une.primo.exlibrisgroup.com`
should have matched. Either the browser ended up on a fourth host nobody
recorded, or the lookup did not run. **The record cannot distinguish those, and
that is the defect.** Until a drift names its host, the ranked repair queue is
guesswork and adapter work cannot be aimed.

Surfacing captures is necessary but not sufficient, and the earlier draft's
acceptance could not be met. The two target adapters declare no denial rule at
all: ProQuest has only `login` and `article`
(`extension/src/adapters/types.ts:383-409`) and Primo only `article`
(`extension/src/adapters/types.ts:529-545`), so
`extension/test/adapters.test.ts:63-87` requires no
denial fixture from either. An unentitled page on those hosts will keep
returning `unknown` until each adapter gains a provider-specific denial signal
and a captured `no-entitlement.html` to pin it.

*Acceptance:* `primo` and `proquest` each declare a denial rule backed by a
captured denial page, and the attributed queue splits into two named
populations. A deliberately altered selector shows up as drift; an unentitled
page shows up as a denial.

### Phase 3 — Adapter work, ranked by the split queue

Ranked after Phase 2, not before. `primo` leads at 18 open papers, but it is the
discovery layer rather than a publisher, so establish whether its unknowns are
holdings gaps before writing selectors. `sciencedirect` is new integration work,
not repair.

Fixture obligations are not uniform. `extension/test/adapters.test.ts:63-87`
maps each rule kind to a scenario: `primo` owes `success.html`;
`sciencedirect` owes `success.html` and `no-entitlement.html`; `proquest` owes
`login-return.html` and `success.html`.

Add a check that each adapter's `article` rule still matches its committed
`success.html`, so drift fails a test rather than a paper.

*Acceptance:* per adapter, a committed live capture and a passing rule. For
`sciencedirect`, a retained validated artifact reaching `ready` — classification
plus a resolvable target does not prove a click yields an adopted file, because
the click path (`extension/src/background.ts:2375-2443`) returns only `ok`.

**Each shipped adapter version release is verified by the existing repair path,
not by new code:** `RepairAdapterUpgrade` should return the matching parked
papers to `resolving` on the next live holder. That is the acceptance test for
this phase and the measurement for the whole plan.

**6 of the 45 open hand-fetches were unreachable by that path — not 19, as an
earlier draft said.** Repairability is not the same as having a diagnosis:
`providerAdapterUpgradeSource` (`internal/app/handoff_repair.go:265-317`) needs a
provider outcome naming an adapter, and falls back to a capture from the same
adapter version. Classifying all 45 that way gave 32 repairable from an outcome,
7 more from the capture fallback, 3 with an outcome carrying no adapter id, and 3
with no provider outcome at all. So **39 of 45 will self-release when the
relevant adapter ships a version bump**, which is a far better position than the
earlier drafts described.

The remaining 6 were swept on 2026-08-24 by operator instruction, using the
existing `papio actions dismiss`. No new command was needed:
`dismissalCancelsParkedJob` (`internal/job/job.go:3072-3083`) lists
`manual_download` on an `awaiting_human` job, so a dismissal cancels the job with
`user_dismissed` — an explicit operator action of the kind ADR-0007 and ADR-0013
already permit. Two were `ProviderAdapterMissing` parks and four were rejected
adopted files. The queue went from 115 to 109 papers.

**Record this against the guard metric.** Those 6 moved into `cancelled` by
deliberate operator action, not by a regression, so the completion-rate guard in
section 5 must treat 2026-08-24 as a step change rather than a signal.

### Phase 4 — Try before asking

`internal/app/app.go:1729-1743` parks a paper as a hand-fetch whenever no
resolver candidate was `Direct`. At `internal/app/app.go:1588-1591` an
open-access landing page is instead routed to the browser for the adapters; a
paywalled one is not, which is where the operator's entitlement lives. Route
paywalled landing pages to the browser too when an adapter covers the host. The
URL and access metadata must reach the browser, and the resolver contract
constrains how. `internal/resolver/resolver.go:53-54` states that a candidate
URL "may be bearer-signed and MUST NOT be persisted or logged", and
`internal/app/app.go:699-708` persists only the redacted form. So a paywalled
route cannot be stored on a new action; it has to be minted fresh or held
ephemerally, and the phase must say which. The `AccessOpen` branch carries a
live URL only as an in-flight browser hint.

*Acceptance:* on a `delegated` job, a paywalled landing page on a covered host is
attempted before any human action opens. On `assisted` and `conservative` jobs it
is not attempted, matching `hasDelegatedAuthority`
(`extension/src/background.ts:3076-3077`) and the operator's own opt-out. An
uncovered host still parks at once, named honestly.

### Phase 5 — Validation failures

67 actions, and **their cause is not recoverable.** Phase 1 instruments future
files only; `internal/app/browser_adopt.go:139-205` never stored an initiator or
a raw failure class, so no backfill can say who fetched these 67 or why each was
rejected. The historical question stays unanswered, and this phase must not
pretend otherwise.

**The instrument named in the earlier draft was also wrong:**
`cmd/identity-corpus/main.go` compares `internal/pdf/identity.go` against the
operator's Zotero PDFs and never consumes `validation_reports`, adopted files,
or MIME and structure failures. It cannot tell a viewer-wrapper HTML file from
an over-strict identity rule.

**And the two populations must be split.** A normal adapter download that returns
HTML is discarded before adoption: `extension/src/background.ts:20100-20125`
rejects `text/html` and XHTML ahead of `download_complete` and reports
`download_not_pdf`, so it never reaches PDF validation. A viewer wrapper can
therefore enter the validation population only through a manually supplied file
or an unusual MIME mismatch. Count pre-adoption MIME rejections separately from
adopted-file validation failures, rather than treating every validation action as
a possible papio-downloaded wrapper.

*Acceptance:* on the post-instrumentation cohort only, a written finding naming
whether rejections come from the download target or from an over-strict rule,
with retained validation reports behind it. The 67 historical rows are reported
as unattributable and excluded. No threshold moves before that finding exists.

### Phase 6 — Continue after the human-only step

The captcha case. Two prerequisites the earlier draft missed.

**There is no surface to continue on.**
`extension/src/background.ts:18839-18890` deliberately closes the provider tab
via `retainForManualDownload`, strips `access_mode`, and keeps a tabless
`awaiting_download` record. Changing the authority read at `:18977-18988` cannot
resume that paper. Retention policy has to change first, or this phase applies
only to a still-tracked handoff.

**The state must be durable.** `hasDelegatedAuthority` reads `job.access_mode`
(`extension/src/background.ts:3076-3077`), and AGENTS.md records worker-memory
`Map` state dying after about 30 seconds. Persist clearance per job and tab in
the existing durable ledger and replay on wake, with holder and claim fencing.
Do not flip `access_mode`, which would let assisted jobs download.

Clearance is not entitlement: ADR-0018 permits `operator_browser_session` only
from the candidate's own `fresh_auth` evidence.

*Acceptance:* on a `delegated` job, clearing a challenge by hand and landing on
an entitled article completes with no further human action, and survives a
worker restart. On an `assisted` job, it does not.

---

## 5. Verification

Baseline at `9660d38`, 2026-08-24.

```sql
-- 1. Parked-queue age. Baseline: 111 of 115 older than a week.
SELECT COUNT(*) FROM jobs WHERE state='awaiting_human'
  AND julianday('now')-julianday(created_at) > 7;

-- 2. Stop-one-step-short. Baseline: 182 over 141 distinct jobs.
WITH seq AS (SELECT job_id, kind,
  ROW_NUMBER() OVER (PARTITION BY job_id ORDER BY created_at, id) AS n
  FROM human_actions)
SELECT COUNT(*), COUNT(DISTINCT a.job_id) FROM seq a
JOIN seq b ON b.job_id=a.job_id AND b.n=a.n+1
WHERE a.kind='openurl_handoff' AND b.kind='manual_download';

-- 3. Attributed causes only. Baseline: 21 diagnosis-confirmed.
SELECT COALESCE(diagnosis,'(unattributed)'), COUNT(*) FROM human_actions
WHERE kind='manual_download' GROUP BY 1 ORDER BY 2 DESC;

-- 4. GUARD: papers filed, and failures not hidden.
SELECT state, COUNT(*) FROM jobs
WHERE state IN ('imported','unavailable','cancelled') GROUP BY state;
```

Measurements 1 to 3 count action rows and can fall without more papers arriving.

**Hard guard 2: the completion rate must not regress.** Phases 2 and 4 could cut
open actions by moving papers into `unavailable` or `cancelled`, which would
improve every measurement above while the operator receives fewer papers. Track
`imported / (imported + unavailable + cancelled)` over a fixed cohort — 29.3% at
baseline — and require no regression. A change that raises the terminalization
rate has failed even if the queue shrinks.

**Hard guard: `identity_corpus_wrong_accepts` must not rise above its baseline.**
Every other measurement can improve while papio files wrong papers faster, and a
wrong paper under a right citation is the worst outcome papio has.

A live smoke run is required; AGENTS.md records two defects that passed a green
suite and were found in one pass against a real browser.

---

## 6. Risks

**A wrong paper is filed.** Guarded above. No phase weakens
`pdf.ValidatePayload` or the identity rules.

**papio races the operator.** Removed by ordering in Phase 4, not mitigated.

**Adapter work targets the wrong pages.** The reason Phase 2 precedes Phase 3.

**A repaired adapter re-drifts silently.** Guarded by the Phase 3 fixture check
and Phase 2's promoted captures.

**Recovery moves many papers at once.** Shipping an adapter version releases
every park attributed to it. `RepairAdapterUpgrade` is one-shot per upgrade, and
the existing offer bounds (`maxOutstandingOffers` = 4, focus cap 32) apply, but
the first adapter release after a long queue should be watched.

**ScienceDirect may not be completable by click.** Phase 3 must produce a
validated artifact or report that the provider needs a different method.

---

## 7. Policy gates

`RepairAdapterUpgrade` is already ratified and shipped, so the recovery path
needs no new decision. Two phases do.

| Item | Gate |
|---|---|
| Phase 4, try before asking | ADR-0022 Decision 5 requires automatic routing to use the daemon claim, opaque binding, revalidation, and effect-permit pipeline. Phase 4 must route through it, not reuse a legacy drive epoch. ADR-0022/0023 stage automatic routing, so enablement must advance deliberately rather than by side effect. |
| Phase 6, continuation | ADR-0013's tripwire states that a background retry, even if bounded, requires a new ADR. ADR-0018 restricts `operator_browser_session` to candidate-owned `fresh_auth` evidence. |
| Attention contract | ADR-0022 Decision 6 grants attention only from a typed current human gate, and ADR-0023 says one actionable row is the researcher's current turn. If a paper's action stays open while papio retries, the inbox claims a human turn that is not real. State whether the action closes, suspends, or is reclassified. |
| Correction | The earlier draft cited ADR-0020 as having disabled autonomous binding. It was amended on 2026-08-18 to enable `candidate_auto_bind/3`, with a residual wrong-accept path recorded. This plan leaves that unchanged. |

**Scope note for the record.** ADR-0009 Decision 2's autonomous-drain bullet
scopes the *ratified consumer IPC surface*: "a consumer's reconciliation",
"actions the consumer never read", "A background **consumer** must not resolve,
open, or retry human work on its own". ADR-0014 Decision 6 frames the same
boundary around a consumer and an `actions.open` selector loop. Neither makes
papio's own scheduler a background consumer, and ADR-0014 Decision 5's
"nothing acts on staleness" protects against *abandoning* work, not against
retrying it. Review initially read these as a blanket prohibition on papio
re-attempting its own failed acquisition and withdrew that reading. The live
constraint on background retry is ADR-0013's tripwire, which requires a
ratified decision rather than forbidding the behaviour.
