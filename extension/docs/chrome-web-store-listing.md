# *papio* — Chrome Web Store listing kit

Paste-ready copy and submission notes for the Chrome Web Store (CWS). Version
updates are driven by `scripts/submit-chrome.sh` (`bun run submit:chrome`); the
first listing and final publication are human steps.

- Version source of truth: `extension/manifest.json`
- Minimum Chrome: 120

## Name

papio

## Short description (CWS, <= 132 chars)

papio's browser half: open-access first, then a visible institutional handoff in your own session. Login stays human; not a bot.

## Category

Productivity (or Developer Tools if a research/utility category is unavailable)

## Detailed description (paste into CWS)

CWS renders this field as plain text (no Markdown/HTML; URLs auto-link). Keep it
in sync with the AMO kit's Markdown version in `amo-listing.md`.

```text
papio automates the tedious part of getting research papers — the gap between "want it" and "validated PDF in my library." It searches scholarly works, turns your picks into repeatable jobs, fetches each one, validates every PDF, and files it into Zotero. You — or an AI agent — drive it; papio does the legwork.

This extension is papio's browser half: it runs the institutional OpenURL handoff and relays the download to the papio app over native messaging. You'll need that app installed — see the setup guide for your platform: https://orgmentem.github.io/papio/guide/getting-started/

What makes it different:
- No credentials stored, no bulk scraping. papio never keeps your institution logins, and it fetches only the papers you explicitly request — one at a time — never mass-downloading from publishers.
- Page scanning happens only when you ask, one page at a time. When you ask papio to find papers on a page, it reads only the top frame of that one tab, only at the moment you click, and never in the background. Detection runs inside the page; the identifiers it found go only to the local papio app, and only the papers you select are acquired.
- Session checks stay with the paper that needs them. While a paper is waiting for sign-in, a completed HTTPS navigation on its declared publisher host can schedule a check of the matching configured library resolver. The trigger reads no publisher page content, and the publisher page cannot set the popup verdict. Other hosts and queued-only work do not authorize it.
- Your real session, not a bot. Native messaging and extension APIs only — no WebDriver, no CDP, no stealth — so your browser never looks automated.
- Validated before trusted. Every candidate PDF is checked for structure and identity; anything ambiguous parks for your review instead of importing the wrong paper.
- Built for AI agents. papio runs as an MCP server, so an assistant can drive the whole workflow.

Privacy: papio collects no data.

Docs: https://orgmentem.github.io/papio/
```

## Privacy practices (CWS Data-usage form)

- **Single purpose:** Perform the browser-side institutional PDF download for a
  locally running *papio* acquisition daemon.
- **Data collection:** None. No data leaves the user's computer. The extension
  communicates only with a local native-messaging host.
- **Page scanning:** Invoked only by an explicit click, top frame only, with no
  standing access — no persistent scanner and no dynamically registered content
  script. It uses only the one-shot activeTab grant the click implies, never the
  optional all-sites grant. Detection is local page JavaScript; the
  detected identifiers and the page's bare origin go only to the local *papio*
  application, and the display-only citation labels never leave the browser.
- **Diagnostic captures:** An explicit capture request, or a provider page for
  which *papio* has no working adapter, can send sanitized HTML to the local
  *papio* daemon for adapter repair. It is stored only in the application's
  local data directory, never uploaded, and may still contain article text,
  account labels, or other page content. Authentication and identity-provider
  pages are excluded. Default retention is 14 days and 10 captures per host;
  the user can purge all captures or one host.
- **Institution-session check:** While a paper is waiting for sign-in, a completed HTTPS navigation on its declared publisher host can schedule a check of the matching configured library resolver. The trigger reads no publisher page content, and the publisher page cannot set the popup's signed-in or signed-out verdict. A tracked papio tab returning from authentication can still provide existing, same-origin release evidence for queued work. The trigger adds no landing URL, page-derived publisher data, title, path, query, fragment, cookie, credential, or session token to storage or native messages. Ranked DOM and browser-storage observations on the configured library page remain the only source of the popup verdict.
- **Loss toast:** When papio loses a tab it opened for a paper, it can offer to reopen that route. With all-sites access granted, "Show the lost-tab message in the page you are reading" is on by default; the user can turn it off to use papio's extension window instead. Revoking all-sites access turns the in-page route off, and re-granting access does not restore an explicit opt-out. The in-page message reads nothing from the page, renders inside a shadow root, and removes itself before reporting the choice. The window route reads no page content, adds nothing to one, and needs no host access. Either route shows one fixed sentence, one button, papio's own mark, no identifier or URL, remains for eight seconds, and performs nothing on close.
- **Host-page acknowledgement:** After a successful popup action, and only when
  transient acknowledgements are set to all requests, a three-second
  noninteractive chip is drawn in the acted-on page. It carries one of four fixed
  short phrases, no identifiers or URLs, reads nothing, stores nothing, and
  transmits nothing.
- **Certifications:** Data is not sold to third parties; data is not used or
  transferred for purposes unrelated to the single purpose; data is not used or
  transferred for creditworthiness or lending.
- **Privacy policy URL:** required by CWS — publish one (e.g.
  `https://orgmentem.github.io/papio/` privacy section) and paste the URL.

### Per-permission justifications (CWS requires one line each)

| Permission | Justification |
| --- | --- |
| `nativeMessaging` | Sole communication channel: connects to the local `com.orgmentem.papio` daemon to receive a job and report the download result. |
| `downloads` | Performs the single requested PDF download per acquisition job. |
| `tabs` / `activeTab` | Opens and manages the handoff tab, correlates its download, and observes completed HTTPS navigation while that job waits for sign-in. Only a hostname matching the job's declared provider can schedule the new resolver check; queued-only work does not authorize this trigger. |
| `tabGroups` | Groups the handoff tab into a collapsed "papio" tab group in the user's own window, so a provider sign-in/download flow stays visually separate from their own tabs. |
| `scripting` | Runs a small routine on the requested provider page to locate its download link, on a configured library resolver to check session indicators while a paper waits for sign-in - that routine reads the page's visible affordances and its `localStorage`/`sessionStorage` entries and returns only a signed-in/signed-out/unknown classification, discarding every value it read, on the current page when the user explicitly starts a scan, and on the acted-on page to draw a three-second confirmation. |
| `storage` | Stores settings and short-lived job/tab state across service-worker suspension. An active institutional job can retain one configured bare resolver origin and one pending-session-check reason; no publisher landing or identity-provider data is added. |
| `alarms` | Schedules reconnect backoff and bounded resolver keepalive checks without a persistently awake service worker. |
| Host permissions (library resolver domains, plus `login.openathens.net`) | Read the configured library discovery/resolver page to route a requested paper and check that resolver session while the paper waits for sign-in; the session check returns only a signed-in/signed-out/unknown classification. The OpenAthens login host is included so papio can recognise the sign-in step of a federated route it opened itself. |
| Optional host permissions (publisher domains) | Access a publisher page only when a job needs its licensed PDF; requested at runtime, not at install. The session trigger reads only a completed landing hostname from the `tabs` event and compares it with that job's declared providers; it does not read page content. |
| Remote code use | None. All code is bundled and shipped in the package; no remote code is loaded. |

## Store visuals (screenshots + promo tiles)

Regenerate every visual asset from the *real* extension UI in one command:

```
bun run capture:store      # or: make store-assets (from repo root)
```

Outputs land in `web-ext-artifacts/store-assets/` (git-ignored). It builds
`dist/`, serves it over http, renders the shipped `popup/options/inbox` pages in
headless Chrome with a stubbed `chrome.*` (real pixels, mock daemon data), then
Lanczos-downscales from 2x for crisp text. Requires system Chrome (or
`$PAPIO_CHROME`) and ImageMagick.

Screenshots (1280x800, upload 3–5):
- `screenshot-popup.png` — toolbar popup, connected + DOI detected.
- `screenshot-popup-attention.png` — toolbar popup, daemon-unreachable warning.
- `screenshot-options.png` — options page, host-permission grant controls.
- `screenshot-inbox.png` — triage inbox, human actions + watch hits.

Promo tiles (24-bit PNG, no alpha):
- `promo-small.png` — 440x280 small promo tile.
- `promo-marquee.png` — 1400x560 marquee promo tile.

Edit the sample data or promo copy/branding at the top of
`scripts/capture-store-assets.ts`. One shot still needs a live daemon and isn't
scripted: the handoff tab in progress — capture it by hand if wanted.

## Obtaining Chrome Web Store API credentials

`scripts/submit-chrome.sh` uses `chrome-webstore-upload-cli`, which needs an
OAuth2 client and refresh token with the Chrome Web Store API enabled:

1. In Google Cloud Console, enable the **Chrome Web Store API** and create an
   OAuth client (type: Desktop app). Record the client id and secret.
2. Mint a refresh token for that client with scope
   `https://www.googleapis.com/auth/chromewebstore` (see the
   chrome-webstore-upload docs for the one-time consent flow).
3. Put the values in `extension/.env` as `CWS_CLIENT_ID`, `CWS_CLIENT_SECRET`,
   `CWS_REFRESH_TOKEN`, `CWS_EXTENSION_ID` (the item id from the dashboard URL),
   and `CWS_PUBLISHER_ID` (Developer Dashboard → **Publisher → Settings**).
   The publisher ID identifies the developer account; it is not the extension
   ID. Chrome Web Store API v2 requires both.

## Launch checklist

1. Confirm `extension/manifest.json` version is the intended release version.
2. First release only: build the Chrome ZIP and create the item by hand in the
   Chrome Web Store Developer Dashboard (the API cannot create the initial
   listing). The ZIP is `web-ext-artifacts/papio-chrome-<version>.zip` after a
   `bun run build` + zip, or reuse `dist/release/<version>/papio-extension-<version>.zip`
   from `scripts/release.sh`.
3. Fill the listing with the name, description, category, screenshots, privacy
   practices, per-permission justifications, and privacy-policy URL above.
4. Subsequent versions: pushing an `ext-v<version>` tag runs
   `bun run submit:chrome --publish` — it uploads and submits for review, and
   CWS auto-publishes when review passes, so nothing is left waiting on a
   dashboard click. A manual dispatch (or a bare `bun run submit:chrome`)
   uploads a **draft** instead, which is the reversible staging path. Both
   preflight the item's review state and abort before building if a previous
   submission is still under review — the Web Store locks the item and rejects
   the publish with a misleading "does not meet the requirements to be
   published" instead of naming the lock.
5. `bun run status:chrome` answers "is a review open?" on its own, from the v2
   `publishers.items.fetchStatus` endpoint: the live version and state, the
   submitted version and state (`PENDING_REVIEW` = locked, `STAGED` = approved
   and waiting to be published, `REJECTED`), and any policy takedown/warning.
   It needs `CWS_PUBLISHER_ID`; the retiring v1 API has no equivalent.
6. Wait for CWS review. Store-installed users auto-update once approved; never
   gate the daemon release on store approval. A red submission run does not
   mean nothing shipped — if the upload succeeded and only the publish failed,
   the package is the item's draft and can be submitted from the dashboard.
