# Privacy policy

_Last updated: 2026-08-06_

*papio* is a **local** paper-acquisition tool. It runs on your own machine and, for
the institutional handoff, inside your own browser. This policy covers both the
*papio* application (daemon and CLI) and the **_papio_ browser extension**.

## The short version

**There is no _papio_ server, no account, no telemetry, and no analytics.** Nothing
about you or your activity is collected, and nothing is reported back to OrgMentem.

That is the whole story for the **extension**, which has no backend at all: it talks
only to the *papio* application running locally on your computer, over the browser's
native-messaging channel.

The **application** is a different matter, and the honest version is longer. Finding
a paper means asking the services that index papers, so *papio* sends the identifier
you asked for — a DOI, a PMID, an arXiv id, or a title — to scholarly metadata APIs
like Unpaywall and Crossref, and (if you set one) your contact email address along
with it. That is not incidental: it is the work. The full list is below, and none of
it goes to us.

## What the application sends, and to whom

Every destination below is a third-party scholarly service, contacted directly by the
daemon on your machine. Each request carries the identifier being looked up; the
"Also sends" column is everything else.

| Destination | Also sends | When | Default |
| --- | --- | --- | --- |
| `api.unpaywall.org` | your `email` (required by their terms) | Resolving a DOI | **On** |
| `api.crossref.org` | your `email`, if set | Filling in metadata for a title-only request | **On** |
| `api.crossref.org` | — | Daily retraction check over papers **already in your library** | **On** |
| `www.ebi.ac.uk` (Europe PMC) | — | Resolving a DOI, PMID, or title | **On** |
| `export.arxiv.org` | — | Resolving an arXiv id or DOI | **On** |
| `doi.org` | your `email`, in the User-Agent | Confirming a DOI exists, before an institutional handoff | **On** |
| `api.openalex.org` | your `email`, your API key | Resolving, and `papio search` | Off |
| `api.core.ac.uk` | your API key | Resolving | Off |
| `api.crossref.org` (TDM) | your subscriber token | Resolving | Off |
| `api.semanticscholar.org` | your API key, if set | `papio search`, when configured | Off |
| Publisher and repository hosts | — | Downloading the PDF itself | **On** |
| `api.github.com` | **nothing** | Once a day, checking for a new *papio* or zotio release | **On**, `updates.check = false` disables it |
| Your webhook URL | job event and message | Job state changes | Off |

Three things worth calling out:

- **The `email` setting is a contact address, sent deliberately.** Unpaywall and
  OpenAlex require it and refuse to run without one; Crossref and the DOI handle
  lookup include it if you have set one. It exists so those services can reach a
  human about traffic from this tool — it is what they call a "polite pool", and it
  buys higher rate limits. Leave it empty and *papio* sends no address, but Unpaywall
  and OpenAlex will not work.
- **The daily retraction check is the one thing that talks about papers you already
  have**, rather than one you just asked for. It sends their DOIs to Crossref's public
  metadata API. Set `[sources.retraction_watch] enabled = false` to turn it off.
- **The update check sends nothing at all.** It is an unauthenticated `GET` of a public
  GitHub releases page, the same request anyone visiting that page makes. GitHub sees
  your IP, as any web request would; OrgMentem receives no notification, no identifier,
  and no count. Set `updates.check = false` if you would rather it did not happen.

## What the extension does and does not do

- **No data collection or transmission to us.** The extension has no backend. It
  communicates solely with the local native-messaging host `com.orgmentem.papio`
  on your own machine, and makes no request of its own to any external service.
  The one request it originates runs *inside the provider page you are already on*,
  to turn a link into a PDF URL, and goes to that same site.
- **No credentials are stored.** You sign in to your institution and solve any
  MFA or CAPTCHA yourself, in your own browser session. *papio* never sees, stores,
  or transmits your usernames, passwords, cookies, or session tokens.
- **No bulk scraping.** The extension downloads only the papers you explicitly
  request, one at a time, as part of a specific acquisition job. It does not
  crawl, harvest, or mass-download from publishers.
- **Your real session, not a bot.** The extension uses only standard extension
  APIs and native messaging — no WebDriver, CDP, or automation frameworks — so it
  operates as an ordinary part of your browsing.

## What is stored, and where

- **In your browser:** the extension keeps its own settings and short-lived job
  and tab state (via the `storage` API) so it can survive service-worker
  suspension and reconnect to the local app. This never leaves your browser.
- **On your computer:** the *papio* application stores papers, metadata, and job
  records in its local data directory. Validated PDFs live in `artifacts/` and
  downloaded candidates awaiting validation live in `quarantine/`. Diagnostic page
  captures live in `<data_dir>/captures/<host>/` as sanitized HTML from the authenticated
  page in your browser session; they can still contain information visible on that page, so
  treat them as private local data. Captures are retained for 14 days and up to 10 per host
  by default (configurable in `[captures]`). Run `papio adapter captures purge`
  to remove every capture, or `papio adapter captures purge --host <host>` for one host.
  The extension no longer writes `~/Downloads/papio-fixtures/`; an existing directory
  there is safe to delete. These files stay on your machine (and papers go only to your
  own Zotero library if you enable that integration).
- **Before you share a bug report,** know that `<data_dir>` also holds `papio.db`
  (every request you have made, with titles and identifiers), `native-host.log`
  (the browser session's diagnostic trace, including URLs), `adoptions/`
  (browser-downloaded files awaiting adoption), and the `update-cache*.json` and
  `retraction-cache.json` files. None of it is uploaded by *papio*, but all of it
  describes what you have been reading.
- **Adapter evidence is local unless you explicitly share it.** Reaching a
  provider that has no adapter can create a sanitized diagnostic capture, but
  *papio* never uploads it, opens a public issue, or sends telemetry. Sanitized
  HTML can still contain article text, account labels, or other page content.
  Review and minimize a capture before contributing it; a future contribution
  helper must show the exact files and destination and require a final publish
  action.
- **Acquisition-history and impact figures:** the numbers the extension shows you —
  papers acquired, an estimated time saved, success rate, weekly acquisition trend,
  access-route breakdown, and human-handoff rate — are aggregates computed locally
  from those same job records, on demand, for display to you alone. No new data is
  collected to produce them, and they are never transmitted anywhere.

## Permissions

Each browser permission the extension requests is used solely to perform a
requested download and report the result to the local app — for example,
`nativeMessaging` to reach the local daemon, `downloads` to save the one
requested PDF, and host permissions to read the library/publisher pages needed
for a specific job. A per-permission explanation is available on the extension's
store listing.

## Third parties

*papio* does not sell your data, does not share it with anyone for their own
purposes, and does not use or transfer it for advertising, creditworthiness, or any
purpose unrelated to performing the acquisition you requested. What it does send to
the scholarly APIs in the table above is sent solely to find and fetch the paper you
asked for. When you request a paper through the browser handoff, your browser
contacts your institution and the relevant publisher directly, exactly as it would if
you visited those sites yourself; *papio* adds no intermediary.

## Changes

If this policy changes, the "Last updated" date above will change and the current
version will always be available at this URL.

## Contact

Questions about privacy: open an issue at
[github.com/OrgMentem/papio](https://github.com/OrgMentem/papio/issues).
