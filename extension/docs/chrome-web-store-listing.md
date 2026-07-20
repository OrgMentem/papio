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
- Your real session, not a bot. Native messaging and extension APIs only — no WebDriver, no CDP, no stealth — so your browser never looks automated.
- Validated before trusted. Every candidate PDF is checked for structure and identity; anything ambiguous parks for your review instead of importing the wrong paper.
- Built for AI agents. papio runs as an MCP server, so an assistant can drive the whole workflow.

Privacy: papio collects no data.

Docs: https://orgmentem.github.io/papio/
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
4. Subsequent versions: `bun run submit:chrome` (uploads a draft) or
   `bun run submit:chrome --publish` (uploads and submits for review).
5. Wait for CWS review. Store-installed users auto-update once approved; never
   gate the daemon release on store approval.
