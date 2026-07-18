# User guide

*papio* finds scholarly papers, checks each PDF is the paper you asked for, and
hands finished PDFs to your Zotero library through `zotio`, which always shows you
a preview first. It does not handle institution logins, two-factor codes,
CAPTCHAs, or bulk-downloading from subscription databases.

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

Start with an OpenAlex search:

```sh
papio search "appropriate reliance on AI" --limit 20 --year-from 2023
```

`--oa-only` limits results to works marked open access. `--year-to` sets an
upper publication-year limit. Search output marks a result already found in the
local zotio library as `[in library]`; JSON output exposes the same state as
`owned` and, when available, `owned_item_key`.

Use `--new-only` when you want the result set to omit library-owned works:

```sh
papio search "appropriate reliance on AI" --limit 20 --new-only --json
```

Ownership filtering happens after OpenAlex applies `--limit`, so a `--new-only`
search can return fewer rows than its limit. If zotio ownership lookup is not
available, discovery continues with all results treated as unowned.

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

Give `acquire --batch` a JSONL file of work records (or `-` for standard input).
A batch holds up to 50 works, each with a stable ID, so running the same file
again is safe and won't create duplicates.

```sh
papio acquire --batch works.jsonl --auto-import \
  --collection "AI reading" --label "appropriate-reliance"
```

`--auto-import` asks *papio* to plan and apply the zotio import after a job becomes
ready. It is non-fatal to acquisition: an import error remains visible in the
batch report and can be retried through the normal zotio preview flow.

`--collection` carries the requested zotio collection with each work; the
collection is created on demand by zotio, and importing the same work again is safe.
`--label` is batch query context for later reports. *papio* first classifies batch
works against your zotio library: works already owning a PDF are skipped, a known
item without a PDF is queued on its existing-item attachment route, and other
works are acquired as new items. Add `--include-owned` only when a batch should
also submit works that already carry a zotio PDF.

You can queue one work instead:

```sh
papio acquire 10.1371/journal.pone.0262026 --auto-import --wait
```

The one-work command also accepts `--doi`, `--pmid`, `--arxiv`, `--isbn`, or
`--openalex`; title-based requests need `--title`, repeatable `--author`, and
`--year`. Use `--desired-version` with `published`, `accepted`, `preprint`, or
`any`, `--source` or `--deny-source` to constrain sources, and `--max-cost` to
cap paid-source cost.

## 4. Follow the work instead of guessing

`status` groups your jobs into working, awaiting-human, needs-review, ready,
and failed or unavailable phases:

```sh
papio status --follow
```

`--follow` refreshes the dashboard every two seconds. For a single job, use
`papio jobs get <job-id> --wait`; `papio jobs list --state <state>` filters the
job list, and `papio jobs retry <job-id>` explicitly retries a failed,
unavailable, or retry-wait job.

## 5. Complete one browser pass when required

When no usable direct candidate remains, assisted and maximal access modes can
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
decision is required. `papio actions open` always targets Chrome, where the
extension and your institutional session live.

The popup also reports the background service's health: it shows a version line
when all is well, and clear warnings when the service is unreachable or the two sides are out of date.
The toolbar badge shows `!` when attention is needed, and the options-page
footer shows the extension and background-service versions together.

For institutional handoffs, *papio* uses your library's OpenURL resolver first.
If it links straight to the provider, papio follows it. When Alma/Primo shows an
online-services menu instead, the extension follows your library's top full-text
link in papio's own tab; you do not need to click **Available Online** or **View
full text**. It never chooses physical-item, scan, interlibrary-loan, or
terms-acceptance options — those stay your decisions. If your library's resolver
is on a domain the extension isn't preapproved for, that step stays assisted.

Grant optional extension host permissions only for publisher sites you use.
While handoff jobs are still open, the extension keeps one pinned, muted tab and
reloads it now and then to keep your session alive. If it detects that your
institution's login page has taken over, it stops reloading, brings the tab
forward, and flags a single sign-in request. Sign in normally there; once you're
back, the extension resumes. This keeps you to one login per research session —
it does not automate your credentials.

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

## 8. Resolve identity reviews deliberately

A PDF can be well-formed yet still land in `needs_review` when papio isn't sure
it's the paper you asked for. `papio actions list` shows the open
`verify_identity` action and the path to the quarantined file. Open that file and
check it before deciding:

```sh
papio actions resolve <action-id> --accept
# or
papio actions resolve <action-id> --reject
```

`--accept` states that you opened the quarantined PDF and confirmed it is the
work you wanted. It requeues the candidate; the result is recorded as
`user_confirmed`, not as an automatic match. `--reject` records that it is not
the right work and cancels the review. Resolution
applies only to an open `verify_identity` action; it does not waive explicit
wrong-work, encrypted, or active-content rejection.

## Why a batch parks

A batch report labels `awaiting_human` work with one of these reasons:

| Reason | Meaning | Next action |
| --- | --- | --- |
| `institutional` | No direct candidate completed; an institutional OpenURL handoff is waiting. | Open the queue, sign in through ordinary Chrome if needed, and complete the allowed provider flow. |
| `oa_browser` | An open-access URL was handed to the browser after papio's own download could not complete it. | Use the offered browser handoff; the browser may download through its existing cookie jar or present a page for you. |
| `terms` | The extension observed terms acceptance is required. | Read and decide on the publisher's terms yourself; *papio* does not accept them for you. |

`needs_review` is separate from these browser states: it is an identity decision
on a quarantined file. `openurl_available` is an advisory action in
conservative mode; it records that institutional access exists but was not
opened automatically.
