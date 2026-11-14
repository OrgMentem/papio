# Firefox and cross-browser contracts

Read this reference before work in this area. Paths in code spans and commands are repository-root-relative unless stated otherwise.

### Firefox / cross-browser extension
- Firefox MV3 has **no service worker** — background is a classic **event-page iife**
  (`build.ts` bundles `--format=iife`; a top-level `export` breaks it, and the build asserts
  against that). Needs `browser_specific_settings.gecko.id`. `manifest.json` is the single
  source of truth; `firefox/manifest.json` is generated from it.
- Firefox has **no `chrome.downloads.onDeterminingFilename`**, so download-path
  steering is unavailable. Diagnostic page captures do not use the downloads API:
  the extension sends the sanitized HTML over native messaging and the daemon stores it
  under its data directory on both Chrome and Firefox.
- Firefox treats MV3 `host_permissions` as **runtime opt-in** — the options page must let
  the user grant them (Chrome grants at install). Same gecko id (`papio@orgmentem.com`) as
  the Web Store build, so the native host `allowed_extensions` matches.
- **Firefox acknowledges native downloads only through the ADR-0029 agent path.**
  Without `onDeterminingFilename` a file cannot be steered into `papio/<job>/`,
  so broad tab/host download correlation stays disabled on Firefox and click
  adapters stay human-assisted there by design. The narrow exception is a
  `native_click_adoption_v1` reservation held under a generic-drive permit:
  the browser reserves one native download before dispatch and admits it only
  on an unambiguous fresh download with the exact original article referrer.
  Don't "fix" a Firefox click adapter by widening generic correlation; the
  daemon would acknowledge a file it can never adopt.
- **Tab-group handoff now runs on Firefox 139+ (`tabGroups` API), not just Chrome.**
  `build.ts` keeps the `tabGroups` permission in `firefox/manifest.json`; the bridge
  is dep-driven (`chrome.tabGroups`/`chrome.tabs.group` runtime-detected), never gated
  on Chrome. `strict_min_version` stays **`128.0`** on purpose — Firefox 128 is the ESR
  papio's institutional/library users run, and on FF < 139 `handoffSurface()` degrades
  tab-group mode to the work window at runtime. **Don't bump the min-version to silence
  `web-ext lint`'s `INCOMPATIBLE_API` warnings for `tabs.group`/`tabGroups.*`** — those
  are expected (static analysis can't see the runtime guard), lint still exits 0
  (0 errors), and bumping would drop every ESR user to no tab-group handoff.

