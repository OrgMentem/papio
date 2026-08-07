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

The extension popup groups jobs into **needs you**, in-flight, and completed
sections. Use its Focus control only when authentication or a provider-owned
decision is required. `papio actions open` asks a compatible extension to use
the browser where its tracked session lives; without one, it opens the resolver
URLs normally.

The inbox keeps itself current without a manual refresh: it updates as soon
as you return to the tab, and checks in on its own every so often while the
tab stays open, so a new or resolved job doesn't wait for you to notice. The
toolbar badge keeps pace on the extension's own wake cycle — its existing
one-minute keepalive alarm — whether or not a *papio* tab is open, since the
browser decides how often a sleeping extension is woken.

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
While handoff jobs are still open, the extension keeps one pinned, muted tab and
reloads it now and then to keep your session alive. If it detects that your
institution's login page has taken over, it stops reloading, brings the tab
forward, and flags a single sign-in request. Sign in normally there; once you're
back, the extension resumes. This keeps you to one login per research session —
it does not automate your credentials.

### Watch the institution session

The popup's **Institution session** card reports the browser-local resolver
state: **Session warm**, **Signed out or expired**, **Sign-in needed - papio
paused**, or **Keep-warm off**. **Sign in now** focuses the ordinary resolver
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
`papio/<job-id>/paper.pdf`. With a browser download directory configured for
the *papio* adoption root, that commonly appears as
`Downloads/papio/<job-id>/`; *papio* adopts the file from that job-scoped
location and runs the same validation used for a directly fetched PDF. Keep
the browser directory aligned with the daemon's `download_adoption_root`;
otherwise a file in an unrelated Downloads folder is not adoptable. The popup
reports **Sending PDF to papio** and then **papio adopted (validating)**; the
inbox and `papio activity` show the later outcome.

The download path is deliberate: do not rename a file into another job's
directory. If validation rejects the PDF, the job remains actionable so you can
provide a different file. If the current tab is only a DOI or provider landing
page, use **Acquire this page** or the browser handoff first; **Send PDF to
papio** is for a PDF page.

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
existing-item-attached, import-failed, awaiting-human, needs-review, failed,
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
acquired, an estimated time saved, and your success rate — with a **View
history** link that opens a full-tab history page. The time-saved figure is
a rough estimate (about 5 minutes of manual chasing per acquired paper) that
the extension itself computes; *papio* does not measure how long anything
actually took you.

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
