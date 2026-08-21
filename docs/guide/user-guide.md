# User guide

*papio* finds scholarly papers, checks each PDF is the paper you asked for, and
offers finished PDFs toward your reference library — into Zotero through
`zotio`, which always shows you a preview first, or toward any other destination
through a best-effort [`on_ready` hook](hooks.md). Hook failures never fail or
retry the acquisition job. It does not handle institution logins,
two-factor codes, CAPTCHAs, or bulk-downloading from subscription databases.

Use [`config-reference.md`](../reference/config-reference.md) to change policy and
[`troubleshooting.md`](troubleshooting.md) when a job needs attention.

## 1. Initialize the local profile

Run the guided setup before any acquisition:

```sh
papio init
```

`papio init` writes your configuration, creates the data folder and its database,
checks the `zotio` program, installs the browser connector (unless you skip
browser setup), and runs `doctor`. You can run it again safely. It asks for a
contact email, the `zotio` path, an attachment mode, and whether to set up
browser integration.

For an unattended profile, retain existing values unless an option overrides
them:

```sh
papio init --non-interactive --email you@example.org --skip-browser
```

Use `--zotio-path` to select the executable and `--attachment-mode` with either
`stored` or `linked-file` when those values need changing. Run `papio doctor`
after any manual configuration change.

## 2. Discover a research set

Start with a discovery search (OpenAlex by default; add Semantic Scholar via
`discovery.sources` in config, or pick one backend for a single query with
`--source openalex|semanticscholar`):

```sh
papio search "appropriate reliance on AI" --limit 20 --year-from 2023
```

`--oa-only` limits results to works marked open access. `--year-to` sets an
upper publication-year limit. Search output marks a result already found by
the local zotio library or a configured `library.sources` authority as
`[in library]`; JSON output exposes the same state as `owned` and, when
available, `owned_item_key`. Without either authority, results are
unowned/unclassified.

Search results lead with confident title matches: when a result's title
clearly answers the query — an exact match, a phrase match, or most of the
query's words — it moves to the front of the list, best match first. Every
other result keeps the order the backend returned, since ranking by abstract,
concepts, and citation graph is what a discovery backend does better than
title comparison can. Each result reports how well it matched (`match_score`
and `match_kind` in `--json` output), and text output says so plainly when
nothing in the result set matched strongly, instead of returning the closest
unrelated papers as if they were right. A query under three words is treated
as a keyword search and left in the backend's own order, as is a
citation-snowball search below.

Use `--new-only` when you want the result set to omit works marked owned by
zotio or a configured `library.sources` authority:

```sh
papio search "appropriate reliance on AI" --limit 20 --new-only --json
```

Ownership filtering happens after OpenAlex applies `--limit`, so a `--new-only`
search can return fewer rows than its limit. Without zotio or a configured
library source, ownership cannot be classified and results remain unowned.

### Grow from a seed paper

The three citation-snowball options take a DOI. Free-text query is optional when
one of them is present.

```sh
papio search --cites 10.1000/example --limit 20
papio search --cited-by 10.1000/example --limit 20
papio search --related-to 10.1000/example --limit 20
```

`--cites` finds papers that cite the DOI (forward citations); `--cited-by`
finds papers cited by the DOI (backward references); and `--related-to` finds
OpenAlex-related papers.

## 3. Acquire the selected works as a batch

Give `acquire --batch` a JSONL file of work records, or a RIS (`.ris`), BibTeX
(`.bib` or `.bibtex`), CSL-JSON (a `.json` file whose top level is an array), or
MEDLINE/NBIB (`.nbib`) file. *papio* detects file formats by extension and
content-sniffs standard input (`-`). A batch holds up to 50 works; identifier
normalization and deduplication are identical for every format, so running the
same file again is safe and will not create duplicates.

```sh
papio acquire --batch works.jsonl --auto-import \
  --collection "AI reading" --label "appropriate-reliance"
```

For example, export a reference list from Zotero, Rayyan, or Covidence as RIS:

```sh
papio acquire --batch refs.ris --label "thesis background"
```

Each record needs an identifier (DOI, PMID, arXiv, ISBN, OpenAlex) or a
complete title/authors/year tuple — the same identity rule as JSONL input.

`--auto-import` asks *papio* to plan and apply the zotio import after a job becomes
ready. It is non-fatal to acquisition: an import error remains visible in the
batch report and can be retried through the normal zotio preview flow.

`--collection` carries the requested zotio collection with each work; the
collection is created on demand by zotio, and importing the same work again is safe.
When zotio is configured, *papio* first classifies batch works against your zotio
library: works already owning a PDF are skipped, a known item without a PDF is
queued on its existing-item attachment route, and other works are acquired as new
items. Add `--include-owned` only when a batch should also submit works that
already carry a zotio PDF.

Without zotio configured, the `--auto-import` and `--collection` behaviour above
does not apply. A ready job's PDF receives a best-effort
[`on_ready` hook](hooks.md) handoff instead. De-duplication still works if you
configure a [library source](hooks.md#de-duplicating-against-a-non-zotero-library)
— `--batch` then skips papers that source says you hold, and refuses to create
any jobs at all if it cannot read the source, unless you explicitly use
`papio acquire --batch --include-owned`. This batch-only override accepts
ownership uncertainty and proceeds despite the unreadable source. With no
library source configured, every work is treated as new and a batch will
re-acquire a paper you already have. Hook failures never fail or retry the
acquisition job.

You can queue one work instead:

```sh
papio acquire 10.1371/journal.pone.0262026 --auto-import --wait
```

The one-work command also accepts `--doi`, `--pmid`, `--arxiv`, `--isbn`, or
`--openalex`; title-based requests need `--title`, repeatable `--author`, and
`--year`. Use `--desired-version` with `published`, `accepted`, `preprint`, or
`any`, `--source` or `--deny-source` to constrain sources, and `--max-cost` to
cap paid-source cost. `--label` works here too: it records the query context
and seeds the target collection when `--collection` is unset.

### Bulk import from discovery interfaces

A discovery interface's own search result list is often the easiest place to
gather a reading set — a OneSearch or Primo results page, a Google Scholar
search, a systematic-review tool's export — but *papio* does not query,
scrape, or paginate a third-party discovery interface's search API on your
behalf. The reliable route through one is silent-UI: use the interface's own
**Export** control to write a RIS (or BibTeX / CSL-JSON) file of the results
you selected, then hand that file to `--batch` directly:

```sh
papio acquire --batch export.ris --label "spring literature review"
```

This is the same `--batch` command described above — RIS `.ris`, BibTeX
`.bib`/`.bibtex`, CSL-JSON, JSONL, and MEDLINE/NBIB `.nbib` are all accepted
regardless of where the file came from, the same ledger/cache deduplication
applies, and a title-only entry with no identifier still submits through the
same enrichment path as a title-only JSONL work — arriving as an exported RIS
row is not itself grounds for refusal.

Where a page instead shows exact, on-page identifiers you can select
yourself — a reference list, bibliography, or results page with visible
DOIs — the extension popup's **"Select papers on this page"** action is the
browser-native path: it reads only what's visibly on the page and submits
your selection as an ordinary batch. Structured citation export through this
section is the preferred fallback whenever the interface withholds
identifiers from the page itself, which is the common case for a search
portal's own results list — the export already carries identifiers a visual
scan cannot recover.

## 4. Follow the work instead of guessing

`status` groups your jobs into working, awaiting-human, needs-review, ready,
imported, and failed or unavailable phases. `ready` means a validated PDF is
waiting for import; once a zotio apply files it, the job moves to `imported`
with its Zotero item keys, so neither surface keeps presenting finished work
as actionable:

```sh
papio status --follow
```

`--follow` refreshes the dashboard every two seconds. For a single job, use
`papio jobs get <job-id> --wait`; `papio jobs list --state <state>` filters the
job list, and `papio jobs retry <job-id>` explicitly retries a failed,
unavailable, or retry-wait job.

For a compact event-oriented view of the daemon's durable work, run
[`papio activity`](../reference/commands.md). It is newest-first and bounded;
`--limit` changes the number of rows, `--job <job-id>` narrows the view, and
`--json` gives the same page envelope used by other list commands. The command
and the inbox activity panel read the same daemon events; the reference page is
generated, so use it for the complete option list.

### Use the inbox activity panel

Open **Open inbox** in the extension popup when you want the browser-side
triage view. Alongside jobs and human actions, the inbox can show a compact
activity timeline: download started/completed, institutional sign-in returned,
provider outcomes, adoption, and other recent daemon events. The panel is a
solicited pull, not a live push stream. It refreshes while the inbox tab is
open, and older daemons that do not advertise the activity-feed feature show a
clear unavailable message rather than stale or guessed entries.

### Use each papio surface for one job

The extension and daemon deliberately keep five surfaces distinct:

| Surface | What it answers |
| --- | --- |
| Inline result / toast strip | Did the action I just took land? |
| Popup | What is happening now for this page and this browser? |
| Badge and tooltip | Is *papio* disconnected, blocked, or waiting for me? |
| Desktop notification | Did something worth interrupting me for happen while I was elsewhere? |
| Inbox and Activity | What needs a decision, what is continuing, and what happened? |

The popup is a current-page lens with a compact global pulse, not a second
inbox. The badge is ambient and lossy, not a progress bar. The inbox is the
durable decision surface, and Activity is its recoverable history. A feedback
strip acknowledges a local action; it never becomes a work queue.

### Choose notification interruptions

Desktop notification routing is daemon-owned. The default `milestones` preset
interrupts for useful milestones rather than every event: newly opened
decisions are coalesced, pending decisions become a digest no more often than
every four hours, batch completion gets at most a useful checkpoint and a
meaningful final summary, discoveries stay in catch-up/digest views, integrity
notices are capped per scan, and a named degraded episode notifies once.
Continuing work and scheduled retries stay quiet. The `quiet` preset is less
interruptive, while `verbose` surfaces more category events. Use
[`[notify]` in the configuration reference](../reference/config-reference.md#notify)
for the preset table and per-category overrides.

Webhooks are a separate automation channel. They are not delayed by human
quiet hours or the desktop rate limit, and each category can override its
webhook mode independently of desktop routing. Webhook delivery and desktop
delivery are both best-effort; the durable inbox and Activity records remain
the recovery path.

### Read the pulse for a batch

For a one-shot daemon reading, run:

```sh
papio pulse
papio pulse --json
```

The popup and inbox request the same typed pulse on their existing refresh
cadence. Depending on the authoritative projection, its vocabulary is:

- **Moving** — work is in flight or eligible to continue automatically.
- **Waiting on you** — no work is moving and an effective researcher turn is
  open.
- **Stalled** — work has a named, durable degradation episode rather than an
  ordinary scheduled wait.
- **Scheduled** — only future retries, delivery polls, or source gates remain.
- **Idle** — a complete projection reports no nonterminal work.
- **Unknown** — the daemon is disconnected, the reading is stale or
  contradictory, or the projection is incomplete for the requested conclusion.

When a batch is partial or a measurement is unavailable, *papio* says so
instead of turning missing data into zero. It shows no progress percentage,
success ETA, or queue position. A next action time is a scheduled retry or
check, not a promise that the work will succeed then.

### Choose the toolbar count

In **Options → Feedback and interruptions**, choose one of:

- **Decisions waiting** (the default): show the exact daemon-owned count of
  effective required turns;
- **Everything pending**: show the broader legacy pending inventory; or
- **No number**: keep the badge's disconnected and blocker states without a
  numeric count.

The `!` disconnected/blocker indicator always takes precedence. Watch hits,
retractions, dependent sibling papers, and continuing work do not inflate the
default required-turn number. When an older daemon cannot provide the required
turn projection, the extension labels the fallback honestly as pending items.

### Recover Activity after being away

Activity remains a solicited pull, not a live push stream. The inbox requests
pages of at most 50 entries and can use **Show more** to walk older entries.
The extension stores an `activity_seen_through_seq` read watermark in browser
profile storage. It advances that watermark only after a visible Activity
render succeeds; background polling does not mark entries read.

When the daemon can calculate the difference, the inbox shows `Activity (N
new)` and a **Since you were last here** divider. If retained history has a
gap, it says that newer Activity is available without inventing an exact
count. Activity remains ordered and recoverable while repeated polling stays
quiet for screen readers.

### Desktop capability and acknowledgement

Desktop notifications are daemon-owned and best-effort. The supported sender
is macOS-only today; other platforms report the capability as unavailable
instead of pretending that a notification was shown. The operating system
does not provide *papio* with a delivery or visibility acknowledgement, so
the Activity audit language says **attempted**, never **delivered**. A desktop
notification is never the only record of an outcome: use the inbox, Activity,
or the corresponding CLI command to recover it.

When an inbox row says **manual download**, open the provider's PDF in the
ordinary browser and use the popup's **Send PDF to papio** action. The activity
panel then makes the download and adoption steps visible while validation runs.

## 5. Complete one browser pass when required

When no usable direct candidate remains, assisted and delegated access modes can
park a job for the ordinary Chrome extension. First inspect the queue without
opening a browser:

```sh
papio actions list
papio actions open --dry-run
```

Then open the current handoff URLs:

```sh
papio actions open
```

The extension popup keeps daemon-owned decisions in its pulse and browser-local
work under **Do this in the browser**. A shared explanation appears once rather
than under every paper; hover or focus a compact control for its secondary
detail. Use **Focus** only when authentication or a provider-owned decision is
required. `papio actions open` asks a compatible extension to use the browser
where its tracked session lives; without one, it opens the resolver URLs
normally.

The acquire action itself sits in the popup's header: an accented **+** beside
the inbox and settings icons, offered only when the current tab has a paper to
add and nothing already running for it. Hover or focus it to read exactly what
it will act on, DOI included. Everything else about that action — a refusal and
its remedy, a *which paper is this?* choice, an acquisition already in
progress — appears in the card below instead, so the icon is never the thing
reporting a problem. **Select papers on this page** keeps its full label there:
it is a different, multi-step action with its own consent step.

The inbox keeps itself current without a manual refresh: it updates as soon
as you return to the tab, and checks in on its own every so often while the
tab stays open, so a new or resolved job doesn't wait for you to notice. The
toolbar badge keeps pace on the extension's own wake cycle — its existing
one-minute keepalive alarm — whether or not a *papio* tab is open, since the
browser decides how often a sleeping extension is woken.

The inbox follows the same rule: one sentence-case heading and instruction per
same-kind block, with each paper reduced to its title and one identifying fact.
A one-paper security check, terms page, or institutional sign-in still keeps its
instruction; shared copy is never hidden merely because there is only one row.
The summary line carries only the exact effective turn count (`N need you`);
Actions and Watch hits keep their own inventory counts in the tabs instead of
those totals being repeated and added together above them.

The popup also reports the background service's health: it shows a version line
when all is well, and clear warnings when the service is unreachable or the two sides are out of date.
The toolbar badge shows `!` when attention is needed, and the options-page
footer shows the extension and background-service versions together.

For institutional handoffs, *papio* uses your library's OpenURL resolver first.
If it links straight to the provider, *papio* follows it. When Alma/Primo shows an
online-services menu instead, the extension follows your library's top full-text
link in *papio*'s own tab; you do not need to click **Available Online** or **View
full text**. It never chooses physical-item, scan, interlibrary-loan, or
terms-acceptance options — those stay your decisions. If your library's resolver
is on a domain the extension isn't preapproved for, that step stays assisted.

Opening a browser-handoff row from the inbox focuses that job's tracked
resolver tab; it does not open the DOI in a separate, untracked tab. Re-running
`papio actions open` behaves the same unless that tracked tab is on an
authentication page (or awaiting authentication), when the extension re-drives
its retained resolver URL for a fresh sign-in attempt. It never navigates a
provider page that is already progressing a download. If Alma or Primo
explicitly reports that no electronic full text or online link exists, the
extension marks the route unavailable instead of sending you through the same
institutional-login loop again. An empty or still-loading resolver page remains
assisted rather than being treated as proof of no access.

Grant optional extension host permissions only for publisher sites you use.
The friendly publisher name is the only visible row label; hover the row or
focus its switch to reveal the exact host pattern being granted. The full
pattern remains in the control's accessible name.
While handoff jobs are still open, the extension keeps one pinned, muted tab and
reloads it now and then to keep your session alive. If it detects that your
institution's login page has taken over, it stops reloading, brings the tab
forward, and flags a single sign-in request. Sign in normally there; once you're
back, the extension resumes. This keeps you to one login per research session —
it does not automate your credentials.

### Watch the institution session

The popup's **Institution session** card reports the browser-local resolver
state: **Signed in**, **Signed in <age> - due a recheck**, **Signed out or
expired**, **Sign-in needed - papio paused**, or **Keep-warm off**. When every
configured institution is signed in and freshly verified, the card says **All
institutions signed in**. **Sign in now** focuses the ordinary resolver
tab when a library route is configured. Sign in, complete any MFA or other
institution step yourself, and return to the provider page; *papio* never fills
credentials or copies cookies.

The **Options** page controls this browser-local behavior. **Keep-warm
session** enables or pauses refreshes of the pinned resolver tab, and
**Refresh interval** chooses how often it is refreshed (2–30 minutes). The
card and the options controls describe this browser's session only; they do not
change daemon access policy or send login details anywhere.

### Send a PDF already open in the browser

If a handoff or a provider page has reached a PDF, open the popup and choose
**Send PDF to papio**. The popup recognizes a direct PDF URL and the built-in
Chrome or Firefox PDF viewer, then queues the DOI or associates the current tab
with its existing job. The extension starts a browser-managed download named
`papio/<job-id>/paper.pdf`. That name is *relative*: Chrome's download
steering can only place a file inside the browser's own download directory,
so the file lands at `<your browser download folder>/papio/<job-id>/`. The
daemon's adoption root is the matching `papio` folder — by default
`<your download folder>/papio`, which `papio init` creates for you — and it
adopts the file from that job-scoped location, running the same validation
used for a directly fetched PDF. The popup reports **Sending PDF to papio**
and then **papio adopted (validating)**; the inbox and `papio activity` show
the later outcome.

You only need `download_adoption_root` if your browser downloads somewhere
other than this account's download folder; set it to that folder's `papio`
subdirectory. `papio doctor` fails the `adoption_root` check, and names the
path it resolved, whenever the configured root is not a `papio` directory a
browser could steer into — the failure mode is otherwise completely silent,
because nothing errors, files simply never get adopted.

The download path is deliberate: do not rename a file into another job's
directory. If validation rejects the PDF, the job remains actionable so you can
provide a different file. If the current tab is only a DOI or provider landing
page, use the header's **+** (**Acquire this page**) or the browser handoff
first; **Send PDF to papio** is for a PDF page.

Firefox does not expose Chrome's `downloads.onDeterminingFilename` hook. The
popup's direct **Send PDF to papio** download still uses its job-scoped
filename, but a provider button that starts its own click download cannot be
rerouted automatically and remains human-assisted. When that happens, wait
until the PDF is open and use **Send PDF to papio** rather than assuming an
unrelated file in `Downloads` will be adopted.

## 6. Read the batch outcome

Ask for a joined view of the original batch manifest, live job state, events,
and human actions:

```sh
papio batch report latest --markdown
```

Use a concrete batch ID instead of `latest` when tracking more than one run.
Without `--markdown`, the command prints the normal table; `--json` provides the
structured report. Outcomes include imported, browser-fetched-then-imported,
existing-item-attached, acquired (validated but not imported, the normal outcome
without `--auto-import`), import-failed, awaiting-human, needs-review, failed,
skipped-owned, and in-progress.

## 7. Turn a successful search into a watchlist

A watch repeats discovery, ownership filtering, capped submission,
auto-import policy, collection routing, and notifications on a schedule:

```sh
papio watch add "appropriate reliance on AI" \
  --cadence weekly --limit-per-run 10 --collection "AI reading" --oa-only
papio watch list
papio watch run <watch-id>
papio watch remove <watch-id>
```

`--cadence` accepts `daily`, `weekly`, or `Nh`; `--limit-per-run` accepts 1
through 50. `--year-from` and `--year-to` apply the same publication-year limits
as search. Watch execution is serial, records its last result, and auto-disables
a watch after five consecutive failures. Removing a watch does not remove jobs
or Zotero items created by earlier runs.

### Alert-only watches: report first, acquire on demand

`--mode alert` runs the same discovery and ownership filtering but records new
works in a per-watch digest instead of acquiring them — each work is reported
once, ever:

```sh
papio watch add "appropriate reliance on AI" --cadence weekly --mode alert
papio watch digest <watch-id>            # review what's new
papio acquire --from-digest <watch-id>   # queue everything pending
papio acquire --from-digest <watch-id> --keys 10.1000/example  # or just some
papio watch digest clear <watch-id>      # discard the rest
```

Acquired entries leave the digest automatically; cleared ones simply stop
being pending (they will not be re-reported).

### Backfill watches: acquire missing PDFs

`--kind backfill` takes no query — each run queues Zotero items that are
missing an attached PDF, exactly like `papio acquire --from-zotio`, bounded by
`--limit-per-run`:

```sh
papio watch add --kind backfill --cadence daily --limit-per-run 10 \
  --collection "AI reading"
```

Re-runs are idempotent: already-queued or completed items are skipped. Items whose
last attempt ended `unavailable` wait for
`zotio.unavailable_recheck_days` (default 14) before being checked again.

### See exception state inside Zotero

With `zotio.exception_tags = true` (requires zotio ≥ 0.13.0), the daemon
maintains two automatic tags on linked items in your personal library. These tags
show acquisition exceptions directly on the linked Zotero items:

- `papio:needs-action` — acquisition is parked on you (SSO login, terms
  consent, identity review). Open your browser; the extension shows the
  prompt.
- `papio:unavailable` — every OA and institutional route failed as of the last
  attempt. A saved search for this tag lists items that need another access route
  or manual follow-up. The tag clears itself if a later re-check succeeds or you
  attach a PDF manually.

Nothing else is tagged: a clean acquisition's only trace is the attached PDF. Tags
converge with job state on the daemon's maintenance cadence;
`papio zotio tags reconcile` forces one pass. Both are automatic-type tags, so
Zotero's tag selector can hide the whole namespace, and you can assign colors as
you prefer.

*papio* never retypes or removes a same-name manual tag. Before uninstalling,
disable the feature, restart the daemon so it reloads that setting, then force
the cleanup pass:

```sh
# after setting zotio.exception_tags = false
papio daemon stop
papio zotio tags reconcile
```

The pass removes only automatic tags owned by papio.

### Triage failures

When acquisitions die, see where they cluster before digging into single jobs:

```sh
papio jobs failures --since 30d
```

Rows group by state, provider host, and terminal reason with a sample job id
for `papio jobs get`.

Incident rows show a keyed fingerprint plus bounded `safety_domain` and
registrable `host_family` labels for local diagnosis. The fingerprint omits raw
hosts and identifiers and is keyed per installation, so it resists stable
cross-install correlation; those bounded labels are intentionally visible in
local `jobs failures` and `jobs incidents` output.

## 8. Resolve identity reviews deliberately

A PDF can be well-formed yet still land in `needs_review` when *papio* isn't sure
it's the paper you asked for. `papio actions list` shows the open
`verify_identity` action and the path to the quarantined file. Open that file and
check it before deciding:

```sh
papio actions resolve <action-id> --accept
# or
papio actions resolve <action-id> --reject
```

`--accept` states that you opened the quarantined PDF and confirmed it is the
work you wanted. The daemon imports that same file — no second download — and
records the result as `user_confirmed`, not as an automatic match. If the
quarantined file has since been removed or altered, the candidate is fetched
again instead. `--reject` records that it is not
the right work and cancels the review. Resolution
applies only to an open `verify_identity` action; it does not waive explicit
wrong-work, encrypted, or active-content rejection.

## See your acquisition history and impact

The extension popup shows a compact **Your papio impact** summary — papers
acquired and your success rate — with a **View history** link that opens a
full-tab history page. Both figures are counted from your own job records:
*papio* reports what it did and does not estimate time saved, hours of
searching avoided, or any other figure it cannot measure.

The history page adds a 12-week chart of weekly acquisitions, your success
rate (acquired vs. failed), a breakdown by access route (open access /
institutional / licensed API / other), and how often an acquisition needed a
human handoff. Every figure is an aggregate computed locally from your own
job history — nothing is sent anywhere to produce it. Against an older
daemon that doesn't support the feature, the popup hides the summary and the
history page shows a muted "stats unavailable" note instead of an error.

## Why a batch parks

A batch report labels `awaiting_human` work with one of these reasons:

| Reason | Meaning | Next action |
| --- | --- | --- |
| `institutional` | No direct candidate completed; an institutional OpenURL handoff is waiting. **Sign in to your institution first**, then open the handoff. | Open the queue, sign in through ordinary Chrome if needed, and complete the allowed provider flow. If the provider reports a stale session, re-run `papio actions open` for a fresh link. |
| `oa_browser` | The work is **open access — no login needed**; its URL just refuses non-browser downloads. | Use the offered browser handoff; the browser may download through its existing cookie jar or present a page for you. |
| `terms` | The extension observed terms acceptance is required. | Read and decide on the publisher's terms yourself; *papio* does not accept them for you. |

`needs_review` is separate from these browser states: it is an identity decision
on a quarantined file. `openurl_available` is an advisory action in
conservative mode; it records that institutional access exists but was not
opened automatically.

Work with **no fetchable identifier never parks for a sign-in at all**. If no
DOI, PMID, or arXiv id can be confirmed — the usual case for books, chapters,
reports, and theses — the job settles `unavailable` with the reason
`no_identifier` instead of opening an institutional handoff. A library login
or retrying the same identifier-less request cannot make it fetchable, so
*papio* says so rather than spending your sign-in on it. Find a DOI and
re-submit a manual request with `papio acquire --doi <doi>`; for a Zotero item,
apply `zotio --yes items enrich --missing-doi` and re-run
`papio acquire --from-zotio`. An ISBN alone is carried into the institutional
link so a monograph is described as a book, but it is not enough to fetch full
text.

A DOI that **does not exist** is treated the same way. Before opening an
institutional handoff for a work whose only identifier is a DOI, *papio* asks
the DOI system whether that handle is registered at all. Metadata sources
cannot answer this — Crossref, OpenAlex, EuropePMC and Unpaywall all report "no
record" and "no open copy" as the same empty result — so a mistyped DOI used to
reach the link resolver, match nothing, and drop you on a "DOI NOT FOUND" page
with a handoff that could never be completed. An unregistered DOI now settles
`unavailable` with the reason `doi_not_registered`; correct the DOI and
re-submit. If the registry itself is unreachable the handoff is offered as
before, because an outage must not terminate fetchable work.
