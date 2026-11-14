# Development browser harness

Read this reference before work in this area. Paths in code spans and commands are repository-root-relative unless stated otherwise.

### Dev harness (only for adapter/fixture work — not production)
- **Chrome 136+ refuses the debug port on the default profile.** Use a dedicated
  `--user-data-dir` (e.g. `open -na "Google Chrome" --args --remote-debugging-port=9222
  --user-data-dir="$HOME/.chrome-papio-dev"`).
- **A custom `--user-data-dir` Chrome looks for native-messaging manifests relative to that
  dir**, not the fixed `~/Library/Application Support/Google/Chrome/NativeMessagingHosts/`.
  Copy `com.orgmentem.papio.json` into `<user-data-dir>/NativeMessagingHosts/` or
  `connectNative` fails "Specified native messaging host not found".
- `open -a "Google Chrome" --args …` is **ignored if Chrome is already running** (macOS just
  focuses the existing instance) — fully quit first, or use `-na` for a new instance.
- **web-ext**: a bare `--firefox-profile=<name>` becomes `-P <name>`, which pops Firefox's
  **profile-chooser modal** and blocks the debugger (ECONNREFUSED). Use an **absolute path**
  (see `web-ext-config.mjs`) → `-profile <dir>`, boots straight in. web-ext keeps a live
  devtools-RDP connection (shows the "robot" address-bar icon; does **not** set
  `navigator.webdriver`) — fine for iteration, but for real Cloudflare-walled providers load
  the built `firefox/` manually via `about:debugging` instead.
- **MV3 SW lifecycle**: `chrome.runtime.reload()` on an UNPACKED extension re-reads the files from disk — Chromium's `ExtensionRegistrar::DoReloadExtension` (`extensions/browser/extension_registrar.cc:1143-1223`) disables the loaded copy and then calls `LoadExtensionForReload(id, path)`. The docs state the same from outside: an unpacked reload "is treated as an update" and fires `chrome.runtime.onInstalled` with reason `"update"`, including via `chrome.runtime.reload()` (https://developer.chrome.com/docs/extensions/reference/api/runtime#unpacked). That `onInstalled` dispatch is what starts the worker, which is exactly why `extension/src/background.ts` registers a bodyless top-level `onInstalled` listener. The old "dormant" note described the CDP/DevTools target not being re-registered — a debugging artifact, not a dead worker. A genuinely sleeping worker still wakes when you load an extension page (`dist/options.html`).
- A fresh dev-Chrome profile has the library **SSO** but **not per-publisher entitlements** —
  providers paywall unless reached via the resolver/proxy. `?accountid=<id>` is the exception
  for ProQuest (see [Providers](providers.md)).
- **Branded Chrome 137+ ignores `--load-extension`, and pairing it with
  `--disable-extensions-except` disables the extension subsystem entirely** — every
  extension page returns `ERR_BLOCKED_BY_CLIENT` and `chrome://extensions` lists nothing,
  while the flags look like they worked. Spawn with `--enable-unsafe-extension-debugging`
  and install over CDP (`Extensions.loadUnpacked`) instead; `extension/dev-unpacked`'s
  deterministic id (`maghibajggmcgmbeoipnlfmceillgapk`) is already in the native-host
  manifest allowlist. Install **once** — a second `loadUnpacked` into the same profile
  corrupts it (service worker runs, every extension page blocked); wipe the profile.
- **Provider hosts are `optional_host_permissions`, so nothing grants them
  programmatically.** `chrome.developerPrivate.addHostPermission` silently no-ops for them
  (it only un-withholds declared manifest hosts), and CDP's `Runtime.evaluate`
  `userGesture` does not satisfy extension gesture tracking. The only path is the real
  prompt: fire the options-page toggle with trusted `Input.dispatchMouseEvent`, then accept
  Chrome's dialog — a separate ~448×196 window whose screenshot renders the **parent**
  window, so pixel clicks miss. Resolve its buttons with `desktop.elementAt(x, y)` and
  `press()` the rightmost one.
- **A `_`-prefixed file anywhere in an unpacked extension directory breaks the whole
  load** — one leftover scratch file in `extension/` bricks an extension reload,
  including the user's daily browser. Guarded by
  `rule://no-underscore-file-in-extension`.

