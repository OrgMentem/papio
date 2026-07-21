# AGENTS.md — papio dev notes & footguns

papio = a Go **daemon + CLI** (`cmd/papio`, `internal/*`) that acquires scholarly PDFs,
plus a MV3 **browser extension** (`extension/`) that talks to the daemon over a
**native-messaging host** (`com.orgmentem.papio`) using the `papio-browser/1`
**protocol** (Go: `internal/protocol`, TS: `extension/src/protocol.ts`, schema:
`protocol/browser-v1.schema.json`). The extension rides the user's *own*
authenticated browser session — it never drives a separate/automated browser.

## Build & test

```
# Go
go build ./...
go test ./internal/config ./internal/protocol ./internal/browser   # scope to what you touch
go vet ./...

# Extension (bun)
cd extension
bun run typecheck      # tsc --noEmit
bun test               # bun's test runner
bun run build          # emits Chrome dist/ AND Firefox firefox/ (both targets)
bun run dev            # web-ext hot-reload loop into Firefox Developer Edition
```

The extension has **zero runtime deps** — both bundles are plain browser JS. `bun` is a build tool only.

---

## Footguns (each cost real debugging time — read before touching these areas)

### Config & daemon deploy
- **Config is strict-mode: unknown fields are rejected.** Adding a `[browser]` field
  (e.g. `shibboleth_entity_id`, `proquest_account_id`) means the **old daemon binary
  rejects the whole config** the moment the field is present. So a config change and the
  binary that understands it **must deploy together** — otherwise every CLI call fails to
  parse. Deploy order: build binary → `mv` into place → `papio daemon stop` → next command
  autostarts the new daemon. There is no `daemon restart`; `daemon status`/`stop` don't
  autostart, other commands do.
- **A new store migration bumps `user_version`, and three tests hardcode the number**:
  `internal/cli/clean_install_test.go` ("schema version N", twice),
  `internal/doctor/doctor_test.go`, `internal/store/migrate_forward_test.go`.
  `go test ./...` fails after adding `internal/store/migrations/NNNN_*.sql` until
  all three assertions are bumped.

### Protocol (dual Go/TS)
- The protocol is validated **twice** — `internal/protocol/protocol.go` (emit + decode +
  `validate()`) and `extension/src/protocol.ts` (`parseBrowserMessage`). A new offer field
  must be added to **both**, plus `protocol/browser-v1.schema.json`. New fields should be
  **optional/omitempty** and validated fail-closed; existing fixtures/round-trip tests must
  still pass (backward compatible).
- Per-institution offer fields (`login_entity_id`, `proquest_account_id`) are sent **only
  for the default resolver profile** (`row.Policy.Resolver == "" || "default"`) in
  `internal/browser/bridge.go` `offer()` — sending them for a `institute` job would mis-route
  another institution's login. Per-profile values are a future extension.

### Firefox / cross-browser extension
- Firefox MV3 has **no service worker** — background is a classic **event-page iife**
  (`build.ts` bundles `--format=iife`; a top-level `export` breaks it, and the build asserts
  against that). Needs `browser_specific_settings.gecko.id`. `manifest.json` is the single
  source of truth; `firefox/manifest.json` is generated from it.
- Firefox has **no `chrome.downloads.onDeterminingFilename`** — the download-path steering
  and the **fixture-capture popup tool are Chrome-only**. On Firefox, `downloads.download({filename})`
  honors sub-paths directly. The popup capture *hangs* on Firefox.
- Firefox treats MV3 `host_permissions` as **runtime opt-in** — the options page must let
  the user grant them (Chrome grants at install). Same gecko id (`papio@orgmentem.com`) as
  the Web Store build, so the native host `allowed_extensions` matches.
- **Firefox never acknowledges native/manual downloads.** Without `onDeterminingFilename`
  a file cannot be steered into `papio/<job>/`, so broad tab/host download correlation is
  disabled on Firefox — only exact `downloads.download`-started files are owned, and click
  adapters stay human-assisted there by design. Don't "fix" a Firefox click adapter by
  widening correlation; the daemon would acknowledge a file it can never adopt.

### Automation detection (this is load-bearing — papio's whole value is "real human browser")
- **Never drive the user's browser via WebDriver/BiDi for real work.** Firefox BiDi sets
  `navigator.webdriver = true` → Cloudflare/Turnstile hardens and often becomes unpassable.
- Chrome via `--remote-debugging-port` alone keeps `navigator.webdriver = false` (only
  `--enable-automation`, which `puppeteer.launch` adds, sets it). **But** Cloudflare also
  fingerprints the **CDP attachment itself**, independent of the flag — a fresh profile with
  a live CDP client still gets challenged on aggressive providers (SAGE, Wiley, T&F).
- Production papio uses **native messaging + extension APIs only — no CDP/WebDriver** — so
  `navigator.webdriver` stays false and there's no automation surface. Keep it that way.

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
- **MV3 SW lifecycle**: `chrome.runtime.reload()` from the SW leaves it **dormant** (not
  re-registered as a target). Wake it by loading an extension page (`dist/options.html`).
- A fresh dev-Chrome profile has the library **SSO** but **not per-publisher entitlements** —
  providers paywall unless reached via the resolver/proxy. `?accountid=<id>` is the exception
  for ProQuest (see below).

### Adapters & fixtures
- An adapter **cannot** enter `extension/src/adapters/types.ts` without a captured fixture —
  the `every registered adapter is fixture-backed` test requires `fixtures/<id>/success.html`.
  Capture the authenticated page, run it through `sanitizeFixture` (`src/capture.ts`), commit,
  then add the spec + `adapters.test.ts` cases. Do **not** hand-guess selectors.
- **`sanitizeFixture` strips URL query strings** (privacy). So classify selectors must key on
  **stable id/path/data-attrs, not `?...` params** (e.g. SAGE keys on `a#downloadPdfUrl[data-doi]`,
  not `[href*='download=true']`). `method: "href"` reads the **live** anchor href (with query)
  at download time, so runtime downloads still get the full URL.
- **`fetch()` from the page 403s on many provider PDF endpoints** (bot-gated) but
  `chrome.downloads.download` (a browser-level request, like a real click) succeeds — a
  `fetch` 403 is **not** conclusive when picking a download method. Verify live.

### Provider / resolver quirks (institution-specific; current setup = Example University)
- **Example University's resolver routes many titles to ProQuest** (`proquest.com/openurl/handler/…`), not
  the publisher — so publisher adapters (Wiley/SAGE) only fire when routing lands there
  (title/holdings dependent). ProQuest is the highest-volume destination.
- **ProQuest openurl-handler needs the `accountid`.** The Shibboleth-DS federated route
  authenticates ProQuest's *main* context but the openurl handler re-walls. Appending
  `?accountid=<id>` unlocks Example University access with **no sign-in** (config `proquest_account_id`,
  adapter `accountIdParam`). A title still shows "no results" if Example University doesn't *hold* it.
- **Provider PDF-URL shapes differ**: Wiley `citation_pdf_url` (`/doi/pdf/`) is an HTML
  *viewer wrapper* — the real file is `/doi/pdfdirect/<doi>?download=true`. SAGE
  `/doi/pdf/<doi>?download=true` *is* the file. Always confirm which URL returns `%PDF`.
- **OA short-circuits institutional routing**: if the OA resolvers (unpaywall/EuropePMC/etc.)
  find an open copy, the daemon fetches it during `resolving` and never hits the browser. To
  exercise an institutional/provider path, use a **non-OA** title.
- Institution federated-login entityID for Example University is `https://idp.example.edu/entity` (NOT
  `/idp/shibboleth`); ProQuest account id is `12345`. These live in `config.toml`, not code.

### Windows / UX
- Work-window mode (`papio_work_window_v1`, default on) puts handoff tabs in a **minimized
  background window**; provider SPAs may under-render while hidden — a per-adapter
  `requiresVisible` fallback is the intended fix if one stalls. Toggle off in options to debug.
