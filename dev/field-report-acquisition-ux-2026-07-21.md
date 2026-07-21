# Field report — acquisition UX friction (2026-07-21)

Real end-user session, not a synthetic test. Reporter drove papio to acquire a
set of ~9 papers (from a Google Scholar Labs result list) into Zotero.

- **Versions:** papio `0.7.1-dev`, zotio `1.0.0`, extension `v0.4.3`, macOS (arm64).
- **Outcome:** 3 papers fully imported to Zotero; 4 stuck on browser handoff; the
  session was derailed by a stale institutional SSO for papers that **did not need
  SSO at all**.
- **Interface used:** `papio` CLI (the MCP tools were not mounted in this harness),
  so some findings are CLI-specific but most are behavioural/model issues shared
  with the MCP surface.

Findings are ordered by severity. Each has evidence from this session, impact, and
a proposed direction. A "what worked" section is at the end — several things are
genuinely good and should not regress.

---

## CRITICAL

### C1. OA-but-bot-blocked is indistinguishable from paywalled → user burns effort on SSO for open-access papers

This is the finding that actually broke the session.

Four works parked for human action. Their action surface:

| Work | Access reality | Action `kind` | Action `detail` (first line) |
| --- | --- | --- | --- |
| Yang, *My Advisor, Her AI, and Me* (informs) | **Open access** | `openurl_handoff` | "open-access fetch via browser" |
| Xu, *Enhancing Intuitive Decision-Making* (MDPI) | **Open access** | `openurl_handoff` | "open-access fetch via browser" |
| Ferrario & Loi (ACM FAccT) | Paywalled (OA SSRN exists) | `openurl_handoff` | "open-access candidates exhausted; institutional OpenURL handoff available in your browser" |
| Luca, *Trust Asymmetries* (T&F IJHCI) | Paywalled | `manual_download` | "a resolver returned a landing page but no verified direct PDF" |

From the user's seat these all look like "go to the browser and finish this." The
user reasonably drove the **institutional OpenURL / OpenAthens** path for the batch
— and hit a stale SAML session (see C2). But **two of the four (Yang, Xu) are fully
open access**; they were parked only because the *direct* fetch was blocked by
publisher anti-bot, not because of any paywall. SSO was never required for them, so
the entire stale-SSO detour was wasted work on OA content.

**Impact:** The single most important distinction in an acquisition broker —
"do I need to authenticate, or just render this in a real browser?" — is not
expressed. The `openurl_handoff` kind is overloaded across "OA, no auth" and
"institutional, needs SSO," and its `detail` copy actively conflates them
("open-access fetch via browser" vs "institutional OpenURL handoff" are the same
`kind`).

**Proposed:**
- Split the action taxonomy by *auth requirement*, not by mechanism:
  `browser_fetch_oa` (render in a real browser, **no login**) vs
  `institutional_handoff` (**requires** an authenticated session).
- Carry the reason on the action: `{ requires_auth: bool, blocked_by:
  "anti_bot" | "paywall" | "landing_page", oa_url?, resolver_url? }` so the UI can
  say "this is open access, just open it" vs "sign in to UNE first."
- When `is_oa` is true (papio already knows this — see W4), never route the user to
  an institutional resolver; route to the OA location and only fall back to
  institutional if the OA fetch fails validation.

### C2. Institutional SSO handoff goes "stale" during interactive login, with zero papio-side detection or guidance

The user's report verbatim: *"the SSO returned stale."* The OpenAthens page
returned a stale-assertion error; **nothing in papio's job log recorded it** — a
grep of the four jobs' events for `stale|expire|auth|sso|fail|error` returned
nothing. The staleness is the classic pattern: papio initiates the OpenURL handoff
(mints a SAML `RelayState`/assertion), the user takes time to complete login + MFA,
and by the time they land the assertion has expired; resuming the original handoff
tab replays a dead assertion.

**Impact:** A time-boxed institutional assertion + a human doing MFA is a guaranteed
race. The user is left with an opaque publisher-side error and no papio affordance
to recover. They cannot tell whether to retry, re-login, or give up.

**Proposed:**
- Order of operations guidance in the handoff: "authenticate to your institution
  **first**, then trigger the fetch," rather than mint-then-login.
- Detect expired/stale assertions (SAML `RelayState` age, or the provider's stale
  error page) and **re-issue** the OpenURL against the now-authenticated session
  instead of failing.
- Record a job event when a handoff is abandoned/expired so there is an audit trail
  and `jobs get` can explain it.
- Offer an OA fallback prompt on institutional failure (papio already found the OA
  SSRN copy for Ferrario when asked — see C3 — but only because the user knew to
  look).

### C3. No automatic OA-alternative when the canonical DOI is paywalled

Ferrario & Loi's ACM DOI (`10.1145/3531146.3533202`) is paywalled and papio
declared "open-access candidates exhausted." But an **OA SSRN version of the same
paper** exists (`10.2139/ssrn.4020557`, `is_oa: true`) and was trivially findable
via `papio search`. Acquiring that DOI instead removed the SSO dependency entirely.

**Impact:** papio gave up on OA and pushed the user to institutional access for a
paper that has a free legal copy. The user (or agent) had to know to re-search and
resubmit a sibling DOI by hand.

**Proposed:** When the primary identifier is paywalled, resolve other
versions/preprints (OpenAlex `locations[]`, SSRN/arXiv/repository copies) and try
those before declaring OA exhausted / routing to institutional. This is squarely
inside the "acquisition waterfall" the design already espouses.

---

## HIGH

### H1. `papio actions open` fails with a bare "exit status 1" when the extension isn't connected

With the extension not yet connected:

```
$ papio actions open
papio: exit status 1
```

No message. Only `--json` revealed it actually returns the handoff URL list, and
only `papio doctor`'s WARN line explained the real cause ("extension has not
connected since daemon start"). After the extension connected, the identical
command exited 0.

**Impact:** The failure mode most likely to hit a new user (extension not wired up
yet) produces the least informative error in the tool.

**Proposed:** Actionable error text: *"browser extension not connected — open your
browser with the papio extension enabled (see `papio doctor`), then retry."*

### H2. `papio doctor` is uninformative until the daemon is up, and does not start it

First run (daemon not running):

```
FAIL  daemon      dial ipc daemon: ... connect: no such file or directory
      fix: papio status
SKIP  extension            skipped: daemon is unreachable
SKIP  native host (Chrome) skipped: daemon is unreachable
SKIP  native host (Firefox)skipped: daemon is unreachable
SKIP  zotio                skipped: daemon is unreachable
WARN  database    database not opened for this doctor run
      fix: run doctor through the daemon for integrity status
```

`papio status` autostarted the daemon; a second `doctor` then went green. So the
whole cascade was "the daemon wasn't running yet," but doctor emits one FAIL, one
WARN, and four SKIPs to say it.

**Impact:** The first-run diagnostic is dominated by a self-inflicted "daemon down"
cascade. New users read six lines of red/yellow for a one-line problem.

**Proposed:** `doctor` autostarts the daemon (or `doctor --start`), and collapses
the dependent SKIPs into a single "daemon not running → starting / run `papio
status`" line. Also reword the `database` WARN (H5-adjacent) so it does not read as
"standalone doctor is degraded."

### H3. Job → work mapping is opaque everywhere except buried event detail

`jobs list --json` rows for freshly queued jobs came back with empty
`provider`/`doi`/`detail`:

```
job_85c24e5505865803e2496b37dc | awaiting_human |  |  |
job_913973a224922e2386a3235692 | awaiting_human |  |  |
```

`jobs get --json` top-level had the same empty fields. The only place the DOI
appeared was inside the `job.created` event: `detail.work = "doi:10.1287/..."`.
So to answer "which paper is job_913973?" you must fetch the job and walk its
events. (Meanwhile `papio status` *does* print `doi:...` as the title — inconsistent
between surfaces.)

**Impact:** With a 7-job batch, tracking which opaque `job_<hex>` is which paper
required per-job event spelunking and a lookup table maintained by hand.

**Proposed:** Populate a work identifier + short title on the job row itself
(`jobs list`, `jobs get` top-level), matching what `status` already shows.

---

## MEDIUM

### M1. `--label` rejected on single `acquire`

```
$ papio acquire --doi 10.1186/... --label "AI-reasoning-visibility trust handoff"
papio: --label is supported only with --batch
```

`--label` (batch query context / default target collection) is useful for a single
acquire too — it is how you tag *why* a paper was pulled and where it should land.
Restricting it to `--batch` is arbitrary from the user's view and forced dropping
provenance for single submissions.

**Proposed:** Accept `--label` on single `acquire` (it can seed the same collection
default), or state the rationale in `--help`.

### M2. Cancelled job has an empty event log

The Ferrario/ACM job ended in state `cancelled` with `events: []` — no reason, no
timestamp, no actor. Impossible to tell whether the user cancelled it, it was
deduped/superseded, or it timed out.

**Proposed:** Always emit a terminal `job.cancelled` event with `{reason, at,
source}`.

### M3. Successful `zotio apply` does not advance the job lifecycle

After `zotio apply` returned real Zotero keys:

```
Zotio applied: parent=DFNJ92UN attachment=H8V322DW
```

the three imported jobs still reported state `ready` (not `imported`/`done`). So
`jobs list` continues to present completed-and-filed work as if action remains.

**Proposed:** Transition to a terminal imported/filed state on successful apply, and
link the resulting Zotero item key on the job.

---

## LOW / DX

### L1. `--json` schema is inconsistent and undocumented across subcommands

Every command's JSON shape had to be reverse-engineered by trial and error:

- `search --json` is a **top-level array** (not an envelope object).
- Search results nest the bibliographic record under `work` (`work.doi`,
  `work.title`), with siblings `is_oa`, `oa_url`, `openalex_id`, `cited_by`,
  `abstract`, `owned`, `source`.
- `jobs get --json` event `detail` is a **structured object** for some kinds
  (`{from,to,reason}`, `{work,request_id}`) — not a string.
- Key naming drifts: `plan_id` vs `id`, `doi` vs `identifier`. A parser that assumed
  `plan_id` hit `NoneType` on the `--json` shape.

The MCP list resources already document a `{"<name>": [...], "truncated": bool}`
envelope — the CLI `--json` output does not match that contract.

**Proposed:** Publish JSON Schemas (or at least document shapes in
`docs/reference`), and normalize CLI `--json` to the same envelope contract the MCP
resources use.

### L2. `jobs show` silently does nothing

`papio jobs show <id> --json` (the natural guess; the real verb is `jobs get`)
produced no error and no output — it just printed nothing parseable. An
unknown-subcommand error would have saved a round trip.

### L3. Search relevance for recent / niche works

For 2026 titles, exact-title queries still ranked **generic high-citation AI
papers above the target**; the wanted paper often landed at rank 4–5 behind
irrelevant hits (e.g. the 2019 Dwivedi *Multidisciplinary perspectives* mega-review
appeared as the top hit for four unrelated queries). Two targets (Jirak et al.,
IEEE Access 2026; Schlicht thesis) were **not indexed at all**, and the tool gave
no "no strong match" signal — it just returned unrelated results.

**Proposed:** A title/phrase-match boost or an exact-title mode; a per-result
match-score or `why_ranked` hint; a "no confident match" flag when the top hit is a
weak title match; and a per-query note of which backend(s) answered (the `source`
field per row is good — a query-level summary would complete it).

---

## What worked (do not regress)

- **zotio digest-confirmation flow is excellent.** `plan` → inspect the immutable
  `preview.plan.summary` (`selected/planned/no_op/invalid/destructive`) → `apply`
  with the exact `--confirm-sha256`. Clear, safe, and the non-destructive summary
  gave real confidence before mutating Zotero. Keep this exactly as is.
- **OA fallback discovery via `search`** (finding the SSRN copy of a paywalled ACM
  paper) was smooth once the user knew to do it — the raw capability is there; it
  just needs to be automatic (C3).
- **Extension auto-connected** once Chrome was up; `doctor` then went fully green
  (daemon / native host / zotio v1.0.0, 8 capabilities). No native-host skew.
- **`search` triage fields** (`is_oa`, `oa_url`, `cited_by`, `source`, `abstract`)
  are genuinely useful for deciding what to acquire.
- **acquire → ready** for clean OA works (3 of 7) was fast and needed zero
  intervention.

---

## One-line summary for triage

The broker cannot tell the user "this is open access, just open it" from "sign in
to your institution first" (C1); institutional handoffs race the user's MFA and go
stale with no recovery (C2); and it declares OA exhausted while an OA copy of the
same paper is one `search` away (C3). Fix those three and this session would have
been fully hands-off for 3 of 4 stuck papers, with only the one genuinely-paywalled
paper needing a login.
