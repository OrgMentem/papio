# Privacy policy

_Last updated: 2026-08-14_

*papio* runs on your computer. It has no hosted service, user account, telemetry, or
analytics. This policy covers the *papio* application (daemon and CLI) and the
**_papio_ browser extension**.

## Summary

*papio* does not collect your data or send it to OrgMentem.

The extension communicates with the local *papio* application through the browser's
native-messaging interface. It does not contact OrgMentem or any other external
service on its own, except when it asks a provider page to turn a link into a PDF
URL.

The local application does contact third-party scholarly services to find papers.
These requests include the identifier you ask *papio* to resolve, such as a DOI,
PMID, arXiv ID, or title. Some services also receive your configured email address
or API credentials. The table below lists every destination and the data it receives.

## Requests to third-party services

Every destination below is contacted directly by the daemon on your computer. Each
request carries the identifier being looked up. The “Data sent besides the lookup”
column lists anything else sent.

| Service | Data sent besides the lookup | Used for | Default |
| --- | --- | --- | --- |
| `api.unpaywall.org` | your `email` (required by their terms) | Resolving a DOI | **On** |
| `api.crossref.org` | your `email`, if set | Adding metadata to a title-only request; checking a DOI's registered version relations when other candidates are exhausted | **On** |
| `api.crossref.org` | — | Daily retraction checks for papers already in your library | **On** |
| `www.ebi.ac.uk` (Europe PMC) | — | Resolving a DOI, PMID, or title | **On** |
| `export.arxiv.org` | — | Resolving an arXiv ID or DOI | **On** |
| `doi.org` | your `email`, in the User-Agent | Confirming that a DOI exists before an institutional handoff | **On** |
| Your institution's configured delivery API (`document_delivery.base_url` — ILLiad in v1) | your `api_key`, `patron_ref`, and the request's bibliographic identifiers | Submitting or polling one of your document-delivery requests | Off — requires configuration |
| `api.openalex.org` | your `email`, your API key | Resolving and `papio search` | Off |
| `api.core.ac.uk` | your API key | Resolving | Off |
| `api.crossref.org` (TDM) | your subscriber token | Resolving | Off |
| `api.semanticscholar.org` | your API key, if set | Resolving a DOI, arXiv ID, or PMID, and `papio search` when configured | **On** |
| `api.openaire.eu` | your API token, if set | Resolving | **On** |
| Publisher and repository hosts | — | Downloading the PDF | **On** |
| `api.github.com` | **nothing** | Once a day, checking for a new *papio* or zotio release | **On**, `updates.check = false` disables it |
| Your webhook URL | job event and message | Job state changes | Off |

### Important details

**Email address.** The `email` setting is sent to services that require or accept
it. Unpaywall and OpenAlex require an email address. Crossref and DOI lookup use it
when configured. Leaving it empty prevents Unpaywall and OpenAlex lookups.

**Retraction checks.** Retraction checks send the DOIs of papers already in your
library to Crossref. Disable them with
`[sources.retraction_watch] enabled = false`.

**Update checks.** Update checks make an unauthenticated request to the public
GitHub releases page. GitHub receives your IP address, as it would for any web
request, but *papio* does not send your identifiers or usage information. Disable
the check with `updates.check = false`.

**Document delivery.** Document delivery is disabled unless you configure it. When
enabled, *papio* contacts only the institution's configured delivery service and
sends the credentials and bibliographic details required by that service. It never
uses a shared or *papio*-operated delivery service. See the
[configuration reference](reference/config-reference.md#browserdocument_delivery)
for the full field list.

## Browser extension

- **No OrgMentem data collection.** The extension has no backend and does not send data to OrgMentem. It communicates with the local native-messaging host `com.orgmentem.papio`.
- **No browser credentials.** You enter institutional credentials and complete MFA or CAPTCHA in your browser. The extension does not read, store, or transmit your usernames, passwords, cookies, or session tokens.
- **No background scraping, and scanning is separately consented.** Page scanning runs only when you click it, reads only the top frame of that one tab, and runs entirely inside the page: identifier detection is local JavaScript, not a network request. Before *papio* reads a page, the page's bare HTTPS origin must already be in a scanning allowlist that is separate from the host permissions used for downloads — granting a publisher page for acquisition never authorizes scanning it. The first scan of a new site asks once and performs no reading until you allow it. You can revoke any site later from the extension's **Page scanning** settings section or from the **Always allow scanning on this site** checkbox in the selection workspace; revoking blocks further reading immediately. Selection acts only on the papers you choose, with a maximum of 200 canonical keys per durable cohort, submitted in bounded chunks. It does not crawl, harvest, or auto-submit pages.
- **What a scan sends to the local application.** The scan itself sends nothing. When the selection workspace opens, the detected identifiers (each a `doi`, `pmid`, `arxiv`, or `openalex` kind and value) plus a structural count of visible records go to the **local** *papio* application so it can mark which papers you already own and which are eligible. When you submit, only the canonical keys of the rows you selected are acquired, along with the page's bare lowercase HTTPS origin and the detector name. The short citation label shown beside each row — up to 240 characters of the nearest citation-shaped container's visible text — is display-only and stays in the browser; it is never sent. No page text, path, query, fragment, page title, or credential leaves the browser for a scan.
- **The host-page action acknowledgement is ephemeral and local.** When a popup action succeeds and transient acknowledgements are set to show for all requests, the extension briefly draws a small chip in the page you acted on. It carries one of four fixed short phrases and nothing else — no identifier, title, URL, provider name, or job id — is not interactive, sends nothing anywhere, stores nothing, and removes itself after three seconds. It reads no page content and installs no watcher or content script.
- **Focused-surface presence is minimal and local.** The feature-gated `surface_presence_v1` hint carries only an opaque per-instance id, the focused surface type (`popup` or `inbox`), a boolean focused value, and a timestamp. It goes to the local daemon only. It contains no URL, title, tab id, host, identifier, or page content.
- **Page-bulk recovery is origin-bounded.** The browser-local restart-safe cohort record stores only a bare lowercase HTTPS origin, a bounded detector identifier, and canonical keys (plus the recovery bookkeeping needed to replay chunks). It stores no path, query, fragment, page title, or bearer value.
- **Ownership marks come from a local check, not the network.** A page-bulk row's `owned_with_pdf` / `owned_missing_pdf` / `ownership_unknown` marks can come from your Zotero library through zotio, which the local application invokes as a local subprocess in local-only mode: the lookup answers from zotio's existing on-disk library mirror, and no network request is made for this check — in particular, papio never triggers a Zotero-account sync from a workspace scan. The mirror refreshes only through your own zotio activity (for example, when a paper is filed after acquisition), so a stale or failed check reports `ownership_unknown` honestly rather than a false "not owned."
- **Normal browser session.** The extension uses browser extension APIs and native messaging. It does not use WebDriver, CDP, or other browser-automation frameworks.

## Local storage

**Browser storage.** The extension stores its settings and temporary job and tab
state in browser storage. This data stays in the browser so the extension can
survive service-worker suspension and reconnect to the local application.

The dedicated `page_bulk_cohort_recovery_v1` browser-local record is limited to
restart-safe replay data: a bare lowercase HTTPS origin, a detector identifier,
and the ordered canonical keys, together with opaque cohort/chunk bookkeeping
and timestamps. It never stores a path, query, fragment, page title, or bearer
value.

The `papio_scanner_allowlist_v1` browser-local record holds only the bare
lowercase HTTPS origins you have allowed page scanning for — no path, query,
fragment, page title, or visit history. It is a permission list, not a record of
where you have been: a site appears only because you allowed it, and removing it
from **Page scanning** in settings deletes the entry.

**Application storage.** The local application stores papers, metadata, and job
records in its data directory. Validated PDFs live in `artifacts/`. Downloaded
candidates awaiting validation live in `quarantine/`. Papers go only to your own
Zotero library if you enable that integration. Notification routing also keeps
the `notification_intents` ledger in `papio.db`; its payloads are durable and
may retain identifiers such as a retraction finding's DOI indefinitely. For a
single-finding integrity notice, the DOI may also appear in the macOS
Notification Center notification text, subject to your operating system's
notification and lock-screen settings.

**Diagnostic captures.** Diagnostic captures are sanitized HTML from pages you
choose to capture. They are stored in `<data_dir>/captures/<host>/` and may still
contain article text, account labels, or other page content. Captures are retained
for 14 days and up to 10 per host by default; both limits are configurable in
`[captures]`. Run `papio adapter captures purge` to remove every capture, or
`papio adapter captures purge --host <host>` to remove captures for one host.

**Bug reports.** The data directory may also contain `papio.db` (request history,
titles, identifiers, and notification-intent payloads), `native-host.log`
(browser-session diagnostics, including URLs), a legacy `adoptions/` directory
on installs that predate the browser-download adoption root, and the
`update-cache*.json` and `retraction-cache.json` files. Browser-downloaded
files awaiting adoption now live in the adoption root itself
(`<your download folder>/papio` by default). *papio* does not upload any of
these. Review and minimize them before sharing a bug report; they describe
what you have been reading.

**Adapter evidence.** Reaching a provider with no adapter can create a sanitized
diagnostic capture, but *papio* never uploads it, opens a public issue, or sends
telemetry. Review and minimize a capture before sharing it yourself. Sanitized HTML
can still contain article text, account labels, or other page content.

**Acquisition history and impact figures.** The extension's figures — papers
acquired, success rate, weekly acquisition trend, access-route breakdown, and
human-handoff rate — are calculated locally from job records. They are displayed
only to you and are never transmitted anywhere.

## Permissions

Each browser permission is used to perform a requested download, read a page needed
for that job, or report the result to the local application. For example,
`nativeMessaging` reaches the local daemon, `downloads` saves the requested PDF, and
host permissions allow the extension to read library and publisher pages needed for
a specific job. The extension store listing explains each permission in detail.

## Third parties

*papio* does not sell your data or share it for advertising, credit decisions, or
unrelated purposes.

The third-party scholarly services in the table receive the data listed there
because they are needed to find or download the paper you requested. During browser
handoff, your browser contacts your institution and the publisher directly. *papio*
is not an intermediary for those requests.

## Changes

If this policy changes, the “Last updated” date above will change. The current
version will always be available at this URL.

## Contact

Questions about privacy: open an issue at
[github.com/OrgMentem/papio](https://github.com/OrgMentem/papio/issues).
