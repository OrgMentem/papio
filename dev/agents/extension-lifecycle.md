# Extension pages, worker lifetime and reload

Read this reference before work in this area. Paths in code spans and commands are repository-root-relative unless stated otherwise.

### Extension page paths & MV3 worker memory
- **Every extension page ships under `dist/`, so a page path must be DERIVED from the
  manifest's declared popup, never written root-relative** — a bare
  `getURL("materialize.html")` renders `ERR_FILE_NOT_FOUND` in every deployment, and
  unit tests cannot catch it (the fake `runtimeGetURL` resolves any string). Guarded by
  `rule://extension-page-path-derived`.
- **Anything gated on a worker-memory `Map` silently stops working ~30s after the last
  event, because MV3 sleeps the service worker.** `claimGrants` is the load-bearing
  example: `owner_closed` was gated on `claimGrants.has(job_id)`, so an institutional
  sign-in tab abandoned *after* the worker slept — the common case, not the edge —
  reported nothing. The daemon then held the claim in phase `navigated` until its
  30-minute lease expired, and every sibling job at that institution parked tablessly
  and silently (`handoff.opened` recorded, no surface, no message): the stranded
  sign-in the surface-lifecycle work exists to prevent, arriving by a different route.
  The durable `SurfaceBirthRecord` ledger entry is the state that survives a worker
  death (and a browser restart), so claim identity lives there and `onTabRemoved`
  falls back to it. When adding a new observation or effect, ask which side of that
  boundary its identity lives on; `navigation_error`'s durable-marker path and
  `enqueueRestartRecoveredObservation` are the shape to copy. Note the generation fence
  that makes this safe: `claim_observation_apply.go` rejects a frame as stale only when
  `FrameGeneration != Generation` (the daemon's current holder epoch), and the extension
  stamps every drained observation with `lastKnownBrowserHolderGeneration` from the
  fresh `hello_ack` (`background.ts` `drainObservationOutboxBatch`), so a
  restart-recovered `owner_closed` is applied rather than rejected. The lease's own
  `BrowserHolderGeneration` is compared on NO path (removed in 2d4eec3d: a browser
  reconnect between arbitration and sign-in bumps the epoch, and requiring equality
  rejected every later observation for that login) — do not re-add it to
  `wall_observed`/`auth_returned`/`entitled_landing`.
- **A live smoke run is not optional for this subsystem, and synthetic input cannot do
  it.** Both defects above passed a green 1,211-test suite and were found in one pass
  against a real browser. But an OS-level accessibility `press()` on the popup's own
  **Open** button produces no tab and no state change — extension gesture tracking does
  not accept synthesized activation, the same fact already recorded for CDP's
  `userGesture`. Drive handoffs from the trusted operator surface instead:
  `papio actions open --job <id>` (`--dry-run` first to see the route), and read ground
  truth from the store — `sqlite3 <data-dir>/papio.db` on `materialization_claims`
  (phase, `tab_id`, `lease_until`) and `claim_observation_journal`. A claim whose
  `tab_id` no longer exists, beside an empty journal, is this section's second footgun.
  `papio jobs show <id>` timestamps the daemon's side of the same story.
- **Verify an extension reload actually happened — `papio browser sessions` must show a NEW session id.** A reload tears down the native port, so the daemon logs `browser session <id> disconnected` and the next id differs. This is not pedantry: an accessibility `press()` on chrome://extensions' own **Reload** button reports success and silently does nothing (same synthesized-activation limit as the popup button above), so a whole smoke run can be spent measuring a stale build while believing the fix is loaded — including "adoption survived a reload" conclusions drawn from a reload that never occurred. A page whose URL lacks the `dist/` prefix is the other tell that the running bundle predates the page-path fix. `papio browser reload` is the way to reload: it relays one `dev_reload` frame over the connection the browser already holds; the native host delivers it inside its 2-second poll (`internal/nativehost/host.go:46`); the extension calls `chrome.runtime.reload()`; and the command then WAITS and reports the new holder session id, so the verification is no longer a ritual a human can forget. A real pointer **click** at the Reload icon's screenshot coordinates (`window.click(x, y)`) remains the fallback when no daemon or no connected extension is available. By design the extension declines `dev_reload` unless `chrome.management.getSelf()` reports `installType === "development"` — `chrome.runtime.reload()` on a store-installed copy re-reads nothing and only restarts the worker.


## Current reload procedure

- `make dev-deploy` builds both extension bundles before swapping the binary, restarts the daemon and native host, and attempts `papio browser reload`. The Makefile tolerates reload failure; a successful deploy does not prove the browser loaded the new bundle.
- The connected extension must report version 0.15.0 or newer. An older loaded bundle needs one manual rebuild/load before it understands `dev_reload`.
- The extension must report `installType === "development"` through `chrome.management.getSelf()`. Missing support, lookup failure, and every other install type refuse the reload. A store-installed copy does not load local build files.
- The command reloads the current holder by default. Use `papio browser sessions` to inspect the target; `papio browser reload --session <id-prefix>` selects a session and promotes it when pending. Read [session arbitration](browser-sessions.md) before changing the holder.
- The default wait is 15 seconds. Success reports a new holder session and `reconnect_verified`; `--timeout=0` only sends the request and proves no reconnect. A timeout does not establish installation mode. Concurrent sibling reconnects prevent attribution.
- When no daemon or connected extension is available, use the browser's reload control and verify a new session afterward. Chrome uses `chrome://extensions`; Firefox uses `about:debugging` → This Firefox → Reload. A synthesized accessibility press can report success without reloading.

Evidence: `Makefile:132-158`; `internal/cli/browser.go:138-249`;
`internal/browser/bridge.go:142-144,951-969`;
`extension/src/background.ts:16820-16846`. The older manual-only statement in
`Makefile:99-100` conflicts with the executable recipe. This documentation change
does not edit the Makefile.
