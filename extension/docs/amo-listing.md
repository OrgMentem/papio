# *papio* — Firefox Add-ons (AMO) listing kit

Paste-ready copy and reviewer notes for the *papio* extension on
addons.mozilla.org. Submission is driven by `scripts/submit-firefox.sh`
(`bun run submit:firefox`); final publication is a human step after AMO review.

- Public listing: <https://addons.mozilla.org/firefox/addon/papio/> (first
  listed version 0.5.0, approved 2026-07-25 — later uploads are version
  updates against the existing listing, not new listings)
- Extension ID (gecko): `papio@orgmentem.com`
- Minimum Firefox: 128.0
- Version source of truth: `extension/manifest.json`

## Name

papio

## Summary (AMO, <= 250 chars)

papio's browser connector: the last mile of a local, agent-drivable paper-acquisition tool. Open-access and licensed sources are tried first, then a visible institutional handoff in your own Chrome/Firefox session — login and CAPTCHAs stay human.

## Categories and tags

- Category: Other (or Bookmarks / Productivity if a closer fit is offered)
- Suggested tags: zotero, research, papers, academic, pdf, library, openurl

## Full description (paste into AMO — Markdown)

AMO renders this field as Markdown; HTML is shown literally. Keep this body in
sync with the Chrome kit's plain-text version in `chrome-web-store-listing.md`.

```md
**_papio_ automates the tedious part of getting research papers** — the gap between "want it" and "validated PDF in my library." It searches scholarly works, turns your picks into repeatable jobs, fetches each one, validates every PDF, and files it into Zotero. You — or an AI agent — drive it; _papio_ does the legwork.

This extension is _papio_'s browser half: it runs the institutional OpenURL handoff and relays the download to the _papio_ app over native messaging. You'll need that app installed — see the [setup guide](https://orgmentem.github.io/papio/guide/getting-started/) for your platform.

**What makes it different**

- **No credentials stored, no bulk scraping.** _papio_ never keeps your institution logins, and it fetches only the papers you explicitly request — one at a time — never mass-downloading from publishers.
- **Page scanning happens only when you ask, one page at a time.** When you ask _papio_ to find papers on a page, it reads only the top frame of that one tab, only at the moment you click, and never in the background. There is no persistent scanner, and scanning never uses all-sites access even when you have granted it. Detection runs inside the page; the identifiers found go only to the local _papio_ app, and only the papers you select are acquired.
- **Session checks stay with the paper that needs them.** While a paper is waiting for sign-in, a completed HTTPS navigation on its declared publisher host can schedule a check of the matching configured library resolver. The trigger reads no publisher page content and never sets the popup verdict. Other hosts and queued-only work do not authorize it.
- **Your real session, not a bot.** Native messaging and extension APIs only — no WebDriver, no CDP, no stealth — so your browser never looks automated.
- **Validated before trusted.** Every candidate PDF is checked for structure and identity; anything ambiguous parks for your review instead of importing the wrong paper.
- **Built for AI agents.** _papio_ runs as an MCP server, so an assistant can drive the whole workflow.

**Privacy.** _papio_ collects no data.

[Documentation](https://orgmentem.github.io/papio/)
```

## Privacy and data-collection disclosure

```text
papio does not collect, sell, share, or transmit user data for analytics, advertising, profiling, or telemetry. The extension has no backend of its own.

The extension communicates only with a papio daemon running locally on the user's own computer, over Firefox native messaging (the com.orgmentem.papio native host). It receives one acquisition job at a time and reports the result of a single requested PDF download back to that local daemon.

To perform a download, the extension reads the current provider page (in a tab the user's own session opened) solely to locate the download link for the one paper requested. It does not retain that page during ordinary acquisition, and it never reads a cookie, a password field, or your browsing history. Institutional login is performed by the user; the extension does not handle credentials.

Diagnostic captures are the local-only exception. An explicit capture request, or a provider page for which papio has no working adapter, can send sanitized HTML through native messaging to the papio daemon on the same computer. The daemon stores it under its local data directory for adapter repair. It never uploads the capture, but sanitized HTML can still contain article text, account labels, or other page content. Authentication and identity-provider pages are excluded. The default retention is 14 days and 10 captures per host; the user can purge all captures or one host at any time.

While a paper is waiting for institutional sign-in, the extension can use a completed HTTPS navigation on that paper's declared publisher host as a reason to check the matching configured library resolver. This trigger reads no publisher page content, and the publisher page cannot set the popup's signed-in or signed-out verdict. If papio's own resolver tab remains paused on an identity-provider page, the trigger leaves that page open and creates a muted background resolver tab for the check. The replacement surfaces only when the resolver itself says sign-in is still required. A tracked papio tab returning from authentication can still provide existing, same-origin release evidence for queued work. The trigger adds no landing URL, page-derived publisher data, title, path, query, fragment, cookie, credential, or session token to storage or native messages. Ranked DOM and browser-storage observations on the configured library page remain the only source of the popup verdict.

Page scanning is an explicitly invoked feature. It runs only when the user clicks it, reads only the top frame of the current tab, and holds no standing access: there is no persistent scanner and no dynamically registered content script, and it uses only the one-shot activeTab grant your click implies, never the optional all-sites grant. Detection is local page JavaScript. The identifiers it finds and the page's bare origin go only to the local papio daemon so it can mark which papers are already owned; the short display-only citation labels shown in the selection view never leave the browser. No page text, path, query, fragment, page title, or credential is transmitted.

When papio loses a tab it opened for a paper, it can offer to reopen that route. With all-sites access granted, "Show the lost-tab message in the page you are reading" is on by default and draws the message into the page in front of the user; the user can turn it off to use papio's own extension window instead. Revoking all-sites access turns the in-page route off, and re-granting access does not restore an explicit opt-out. The in-page message reads nothing from the page, renders inside a shadow root, and removes itself before reporting the choice. The window route reads no web page content, adds nothing to one, and needs no host access. Both routes show one fixed sentence, one button, and papio's own mark, contain no identifier, title, URL, or job reference, close eight seconds after arriving, and perform nothing when they close.

After a popup action succeeds, and only when the user has left transient acknowledgements set to show for all requests, the extension draws a small noninteractive confirmation in the page it acted on. It shows one of four fixed short phrases, contains no identifier, title, URL, or job reference, reads no page content, stores nothing, transmits nothing, and removes itself after three seconds.

Extension settings and short-lived job/tab correlation state are stored locally in Firefox storage.
```

For the AMO "data collection" declaration, select **No** — this add-on does not
collect any data. The Firefox manifest also encodes this in source via
`browser_specific_settings.gecko.data_collection_permissions.required = ["none"]`
(generated by `build.ts`). Because the add-on's `strict_min_version` is 128.0 and
that manifest key is honored from Firefox 140, `web-ext lint` emits a benign
"unsupported by min version" warning — Firefox 128–139 simply ignore a
"collects nothing" declaration. This is expected; it is not an error.

## Permission rationale (AMO reviewer notes)

| Manifest item | Type | Why *papio* needs it |
| --- | --- | --- |
| `nativeMessaging` | Required | The extension's only channel: it connects to the local `com.orgmentem.papio` daemon to receive a job and report the download result. There is no other network activity. |
| `downloads` | Required | Performs the single, explicitly requested PDF download for each acquisition job. |
| `tabs` / `activeTab` | Required | Opens and manages the handoff tab for a job, correlates its download, and observes completed HTTPS navigation while that job waits for sign-in. Only a hostname matching the job's declared provider can schedule the new resolver check; queued-only work does not authorize this trigger. |
| `tabGroups` | Required | *(New in 0.7.0 — the first version requesting this permission.)* Firefox 139+ only capability: groups the handoff tab into a collapsed "papio" tab group in the user's own window, so a provider sign-in/download flow stays visually separate from their own tabs (mirrors the tab-group mode already shipped on Chrome in 0.6.0). Runtime-detected: on Firefox < 139 — including the 128.0 ESR line `strict_min_version` targets — the API is simply absent and handoffs silently fall back to the background work window instead. |
| `scripting` | Required | Runs a small content routine on the requested provider page to locate its download link, on a configured library resolver to check session indicators while a paper waits for sign-in, on the current page when the user explicitly starts a scan, and on the acted-on page to draw a three-second confirmation. It reads only what those tasks need. |
| `storage` | Required | Stores extension settings and short-lived job/tab correlation state across MV3 suspension. An active institutional job can retain one configured bare resolver origin and one pending-session-check reason; it never stores the publisher landing or identity-provider data. |
| `alarms` | Required | Schedules reconnect backoff and bounded resolver keepalive checks without keeping the event page awake continuously. |
| `host_permissions`: `*.alma.exlibrisgroup.com`, `*.primo.exlibrisgroup.com` | Required host access | Reads the configured library discovery/resolver surface to route a requested paper and to check that resolver session while the paper is waiting for sign-in. The session check reads that page's visible affordances and its browser-storage entries, and returns only a signed-in/signed-out/unknown classification. |
| `host_permissions`: `login.openathens.net` | Required host access | Recognises the OpenAthens sign-in step of a federated route papio itself opened, so it can tell a login wall from a delivered file. It reads that page only to classify it and returns a fixed outcome. |
| `optional_host_permissions`: jstor.org, proquest.com, ebsco, springer, sciencedirect, dl.acm.org, wiley, tandfonline, sagepub, psycnet.apa.org | Optional host access (runtime opt-in) | Publisher/provider sites where a licensed PDF may live. Firefox prompts for each domain only when a job actually needs it; none are granted at install. |
| `optional_host_permissions`: `https://*/*` | Optional host access (runtime opt-in) | Some libraries run their OpenURL resolver on a custom domain (e.g. `onesearch.library.<uni>.edu.au`) outside the Ex Libris hosts above. This pattern is **never granted at install and never requested in bulk**: *papio* only ever calls `permissions.request` for the exact resolver origin the user configured in their local daemon (`[browser] openurl_base_url`), so the effective grant is that one host. It exists so any institution works without hard-coding its domain in the extension. |

**Note to reviewer (0.7.0):** this is the first version to request `tabGroups`
(see the row above); every other permission has already been reviewed and
approved in a prior version.

## Reviewer notes and build instructions

The shipped Firefox code is bundled by Bun, so `firefox/dist/*.js` is not the
authored source. The `--upload-source-code` archive submitted with each version
contains the human-readable inputs. To reproduce the exact bundle:

1. Install [Bun](https://bun.sh) (version pinned by `bun.lock`).
2. `cd extension && bun install --frozen-lockfile`
3. `bun run build`
4. The Firefox add-on is the generated `firefox/` directory (`manifest.json`
   plus `dist/{background,options,popup}.js`, HTML shells, and `icons/`). The
   submitted XPI is packaged from `firefox/`.

The build is deterministic and requires no network access. `build.ts` bundles
`src/*.ts` into plain browser JavaScript; the Firefox background is a classic
IIFE event-page script (Firefox MV3 has no service worker). The extension has
zero runtime dependencies.

To exercise the extension, a reviewer needs the separately distributed *papio*
daemon and a configured `papio init` (which installs the native-messaging host).
Without the local daemon the extension connects to nothing and performs no
action — it has no standalone behavior to test.

## Screenshot shot list

1. **Toolbar popup — healthy:** popup showing the daemon-connected state and
   version line, with a clear (no `!`) toolbar badge.
2. **Toolbar popup — attention:** popup showing an actionable state
   (daemon unreachable / out-of-date) with the `!` badge.
3. **Options page:** the settings/footer view showing extension and daemon
   versions and the host-permission grant controls.
4. **Handoff in progress:** a provider tab opened for a job, with the popup
   reflecting the in-flight download.

## Release checklist (version update)

The listing exists and is approved, so every later run is a version update.

1. `bun run lint:firefox` — web-ext lint the built `firefox/` with no errors.
2. `bun run status:firefox` — prints the listing's latest listed version and
   state, anything still in AMO's review queue, and whether the manifest
   version is already taken. AMO rejects a duplicate version number across
   every channel, listed or unlisted, so this is the check that matters; a
   version awaiting review does not block a new upload (AMO reviews per
   version). `submit:firefox` runs it as a preflight and stops before building
   on a duplicate.
3. `bun run submit:firefox listed` — signs and submits the new version for
   review. `amo-metadata.json` supplies `version.license` (`MIT`) and
   `categories` (`["other"]`). The script passes `--approval-timeout=0`, so it
   returns once the version is in the review queue rather than blocking on the
   multi-day human review.
4. In the AMO Developer Hub, paste this version's `extension/CHANGELOG.md`
   entry into the per-version **Release Notes** field (the API cannot set it),
   and refresh the screenshots if the UI changed. The listing copy above is
   preserved across uploads — only re-paste it when this file changes.
5. Wait for AMO review. `nativeMessaging` keeps *papio* out of auto-approval,
   so a human reviews every version; installed users auto-update once it is
   approved. Never gate a daemon release on store approval.
