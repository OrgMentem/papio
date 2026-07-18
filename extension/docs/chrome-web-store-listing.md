# *papio* — Chrome Web Store listing kit

Paste-ready copy and submission notes for the Chrome Web Store (CWS). Version
updates are driven by `scripts/submit-chrome.sh` (`bun run submit:chrome`); the
first listing and final publication are human steps.

- Version source of truth: `extension/manifest.json`
- Minimum Chrome: 120

## Name

papio

## Short description (CWS, <= 132 chars)

Browser half of papio: one visible, requested institutional PDF download per job, in your own session. Talks only to a local daemon.

## Category

Productivity (or Developer Tools if a research/utility category is unavailable)

## Detailed description (paste into CWS)

```text
papio is a paper-acquisition broker for your Zotero library. This extension is the browser half of papio: it performs the visible institutional handoff for an acquisition job in your own, already-logged-in Chrome session.

When the local papio daemon needs a licensed PDF that open-access sources did not provide, it opens the provider page in a tab and — for the one paper you asked for — locates and downloads that single PDF. Discovery, open-access resolution, PDF validation, and filing into Zotero all happen in the separately installed papio command-line daemon. This extension only runs the part that must happen inside your real browser.

- Your session, your login. papio never automates or drives a separate browser. Institutional sign-in stays entirely human.
- One download per job. Each job results in a single, explicitly requested PDF download. No crawling, scraping, or bulk downloads.
- Local-only communication. The extension talks exclusively to a papio daemon on your own machine via Chrome native messaging. It has no backend of its own.
- Permission on demand. Publisher domains are optional permissions, requested only when a job needs them.
- No telemetry. No analytics, ads, tracking, or data collection.

papio requires the separately installed papio daemon and command-line tool. On its own, this extension does nothing.
```

## Privacy practices (CWS Data-usage form)

- **Single purpose:** Perform the browser-side institutional PDF download for a
  locally running *papio* acquisition daemon.
- **Data collection:** None. Declare that the extension does **not** collect or
  transmit user data. It communicates only with a local native-messaging host.
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
| `tabs` / `activeTab` | Opens and manages the one handoff tab and correlates the download with the job. |
| `scripting` | Runs a small routine on the provider page to locate the requested paper's download link. |
| `storage` | Stores extension settings and short-lived job/tab state across service-worker suspension. |
| `alarms` | Schedules reconnect backoff to the local daemon without a persistently awake service worker. |
| Host permissions (library resolver domains) | Read the library discovery/resolver pages needed to route a job to the right licensed source. |
| Optional host permissions (publisher domains) | Access a publisher site only when a job needs its licensed PDF; requested at runtime, not at install. |
| Remote code use | None. All code is bundled and shipped in the package; no remote code is loaded. |

## Screenshots (1280x800 or 640x400)

Reuse the shot list in `amo-listing.md`:
1. Toolbar popup — healthy (version line, clear badge).
2. Toolbar popup — attention state (`!` badge).
3. Options page (versions + host-permission controls).
4. Handoff tab in progress.

## Obtaining Chrome Web Store API credentials

`scripts/submit-chrome.sh` uses `chrome-webstore-upload-cli`, which needs an
OAuth2 client and refresh token with the Chrome Web Store API enabled:

1. In Google Cloud Console, enable the **Chrome Web Store API** and create an
   OAuth client (type: Desktop app). Record the client id and secret.
2. Mint a refresh token for that client with scope
   `https://www.googleapis.com/auth/chromewebstore` (see the
   chrome-webstore-upload docs for the one-time consent flow).
3. Put the values in `extension/.env` as `CWS_CLIENT_ID`, `CWS_CLIENT_SECRET`,
   `CWS_REFRESH_TOKEN`, and `CWS_EXTENSION_ID` (the item id from the dashboard
   URL, available after the first manual upload).

## Launch checklist

1. Confirm `extension/manifest.json` version is the intended release version.
2. First release only: build the Chrome ZIP and create the item by hand in the
   Chrome Web Store Developer Dashboard (the API cannot create the initial
   listing). The ZIP is `web-ext-artifacts/papio-chrome-<version>.zip` after a
   `bun run build` + zip, or reuse `dist/release/<version>/papio-extension-<version>.zip`
   from `scripts/release.sh`.
3. Fill the listing with the name, description, category, screenshots, privacy
   practices, per-permission justifications, and privacy-policy URL above.
4. Subsequent versions: `bun run submit:chrome` (uploads a draft) or
   `bun run submit:chrome --publish` (uploads and submits for review).
5. Wait for CWS review. Store-installed users auto-update once approved; never
   gate the daemon release on store approval.
