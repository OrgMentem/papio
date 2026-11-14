# Browser protocol contracts

Read this reference before work in this area. Paths in code spans and commands are repository-root-relative unless stated otherwise.

### Protocol (dual Go/TS)
- **Document-delivery reconciliation now has ONE implementation, and the ordering inside
  it is still the load-bearing part.** `internal/api/delivery.go:deliveryConfirmRequestAbsent`
  and `internal/browser/bridge.go` both delegate to `internal/app`'s
  `Service.ReconcileDelivery`; the duplicated pair of functions with "mirrors internal/api's
  …" comments is gone. That duplication is why the single implementation exists:
  `confirm_request_absent` was fixed in the API (`9d95757`) and the bridge sibling kept the
  broken order for eight days while still claiming to mirror it, so the extension's "Confirm
  absent" failed on every normally parked job. Do not reintroduce a seam-local copy of a
  reconciliation operation; both seams keep only their own error mapping and transport
  decoding.
  The order is `Cancel -> Repair -> Submit`. `RepairAwaitingHuman` is the legal
  `awaiting_human -> resolving` edge, so the action MUST close **before** `SubmitDelivery`,
  whose reconciliation park supplies the `resolving -> awaiting_human` edge back.
  `Cancel -> Submit -> Repair` reads as safer, because a submit failure leaves the action
  open, but Submit then attempts `awaiting_human -> awaiting_human` and the state graph
  refuses it.
  Because the repair COMMITS before the submit runs, a submit failure has already consumed
  the operator's only affordance, so `Service.restoreDeliveryReconciliationAction`
  compensates: one transaction that transitions back to `awaiting_human` and reopens a
  `document_delivery` action. It deliberately does not call `job.Store.ParkWithHumanAction`
  despite matching its shape — that method opens the prompt BEFORE the transition, so
  anything keyed on the job's state during the insert aborts the whole restore. Park first,
  prompt second; its doc comment carries the detail.
  Tests pin the discriminating end state (job parked again, one open and one resolved
  action, row reused not duplicated). Keep them that way: a test asserting only "one open
  and one resolved" passes under **both** orders and is how the original defect shipped.
- The protocol is validated **twice** — `internal/protocol/protocol.go` (emit + decode +
  `validate()`) and `extension/src/protocol.ts` (`parseBrowserMessage`). A new offer field
  must be added to **both**, plus `protocol/browser-v1.schema.json`. New fields should be
  **optional/omitempty** and validated fail-closed; existing fixtures/round-trip tests must
  still pass (backward compatible).
- **`extension/src/protocol.ts`'s `FieldSpec<T>` is exhaustive, so the TS payload
  interfaces are now compiler-enforced rather than decorative.** Every field of the
  payload type MUST appear in the `requireFields<T>(...)` spec — adding a field to an
  interface without a parser disposition is a `bun run typecheck` error, and naming a
  field the interface does not declare is too. Dispositions are `"required"`,
  `"optional"`, and `"forbidden"` (a field the wire contract defines but *this* schema
  version must reject, e.g. `actions_requires_auth` on a `triage_snapshot_response` —
  `triage_counts_response` v2 is the only frame allowed to carry it, mirroring
  `protocol.go`'s gate). Do not silence a spec/interface mismatch by widening the
  interface: the mismatch is the intended signal that a field landed in one of the three
  places and not the others.
- **"Optional field" is only backward compatible in ONE direction, and it is not the one
  you need.** Both parsers reject unknown fields, so an optional field added to an
  *existing* message type is fine for a new parser reading an old frame and **fatal** for an
  old extension reading a new daemon's frame — it rejects the whole message. `stats_*` was
  added as a new message type for exactly this reason; putting a `version` field on
  `triage_snapshot_response` instead would have broken every shipped extension.

  That mechanism is still real, but the exception is not a standing fact of being pre-1.0 —
  it is gated on *verified* zero installs, checked before relying on it: the AMO API
  (`https://addons.mozilla.org/api/v5/addons/addon/papio@orgmentem.com/`,
  `average_daily_users`) and the Chrome Web Store listing's user count. Both read zero as of
  2026-08-06. While that holds, a breaking change to `papio-browser/1` **is allowed** when
  `internal/protocol/protocol.go`, `extension/src/protocol.ts`, and
  `protocol/browser-v1.schema.json` all land in the **same commit**, and the extension is
  rebuilt and reloaded alongside the daemon (`make dev-deploy` now rebuilds the extension for
  you — see the Makefile). The extension is publicly listed on the Chrome Web Store and AMO,
  so this is a fact with an expiry date, not a permanent one: the first real install ends the
  exception, and it can happen without anyone noticing — store extensions auto-update while
  daemons update by hand, so the skew becomes real the moment anyone installs. The release
  runbook's standing rule covers that post-install world: the extension must tolerate old
  daemons, and `hello_ack` feature flags are the mechanism — this release's sibling
  `page_capture_terms_v1` capability is the worked example of doing it the gated way. Until
  then, this floor removes the compatibility burden but **not the deploy burden**: this
  machine still runs two `papio` binaries (see [CLI and MCP](cli-and-mcp.md)) and can have a
  stale extension loaded in Chrome or Firefox, so skew between daemon, host, and extension is
  still the default local failure. `make dev-deploy` rebuilds both bundles and attempts
  `papio browser reload`; that step tolerates failure. Verify the new holder session.
  The command needs extension 0.15.0 or newer and a development install; older bundles
  need one manual load. See [reload limits](extension-lifecycle.md#current-reload-procedure).
- **`internal/nativehost/host.go` mirrors every stderr diagnostic to
  `<DataDir>/native-host.log`** (constant `diagLogName`). Past 1 MiB `openDiagLog` rotates
  it to `native-host.log.1` at process start rather than truncating — two host processes
  share this file whenever a service worker reconnects or Chrome and Firefox are both
  connected, and a rename leaves a live sibling's descriptor on the same inode instead of
  discarding the trace it is still writing. That pair of files is the first thing to read
  when a host dies mid-session — before resorting to driving it by hand (see "Chrome
  forwards native-host stderr nowhere" below).
- **Per-institution offer fields are per-profile, not default-only.** `offer()`
  (`internal/browser/bridge.go`) resolves the institution with
  `b.cfg.InstitutionFor(row.Policy.Resolver)` and sets `login_entity_id` /
  `proquest_account_id` from *that* institution unconditionally, so a named
  `institute` job routes its own federated login instead of inheriting the
  default institution's identity. (This used to be default-only, and this note
  used to call per-profile values "a future extension"; they shipped.) The
  live footgun is the resolution, not the gate: a new per-institution offer
  field MUST come from the `InstitutionFor(row.Policy.Resolver)` result, never
  from the top-level `[browser]` config, or every named profile silently
  re-acquires the default institution's identity for that one field.
- **The native host distinguishes application failures from transport/framing
  failures at the `browser.sync` boundary.** A daemon handler's ordinary
  failure is logged, surfaced as one structured error frame, and the browser
  session continues serving later requests. Only an explicitly classified
  transport/framing failure — a broken IPC connection, an undecodable frame, a
  wire size-cap violation, a daemon outbound self-validation failure, a
  `result_too_large` response, or a failed native stdout write — tears down the
  session, because continuing would mean the host can no longer trust the
  request/response stream. The API marks only the ordinary `browser.sync`
  operation path as application-level; protocol violations and unrepresentable
  outbound frames remain fatal. `internal/nativehost/host.go` keeps this
  disposition out of the IPC result shape so older peers remain decodable.
  Every handler in `internal/browser/bridge.go` MUST encode ordinary/expected failures (item
  gone, file missing, not configured, …) into a structured `outcome`/`detail`
  result (or the existing structured error frame when that result has no
  outcome/detail fields) — mirror `TriageDecideResultPayload`/
  `HumanActionResolveResultPayload`/`ReviewPreviewResultPayload` — never return
  a raw Go `error` for a routine condition. `reviewPreview` got this wrong and
  every click on a stale review action was silently disconnecting the extension;
  inbox-count and stats reads repeated the same failure through their old
  raw-error paths.
- **Adding a daemon feature flag breaks one hardcoded assertion** in
  `internal/browser/bridge_test.go`: the exact advertised list in the hello-ack test
  (`slices.Equal(payload.Features, …)`). Add the new feature to the `required` literal in
  `NewBridge` and to that assertion, in the same order. Same class of trap as the migration
  `user_version` bumps in [Config and daemon](config-and-daemon.md), but now only one site — the second assertion
  (`TestHelloAckFeatureCapReservesMandatoryFeatures`, whose slice bound was arithmetic on the
  mandatory-feature count) is gone along with the dead caller-supplied `features` seam it
  covered. The emitted list is capped at `protocol.MaxHelloAckFeatures` (64), the one
  constant `HelloAckPayload.validate` also decodes against, matching the extension's
  `HELLO_ACK_FEATURES_ACCEPT_CAP` and the schema. That is stage 2 of the mixed-version
  migration whose stage 1 shipped in ext-v0.15.0: `helloAck` sends an extension below
  `WideHelloAckMinExtensionVersion` (0.15.0, or unversioned) only the first
  `protocol.LegacyHelloAckFeatures` (32) entries, because its parser rejects the whole
  ack above 32. So **append a new feature after the 32nd
  (`institutionalAuthenticationClaimFeature`), never insert it earlier** — insertion
  would push an old feature out of a legacy peer's list. A new boolean capability is a
  feature; reserve version-gated `hello_ack` fields (like `adapter_control`) for
  negotiation that carries a payload. Do not reintroduce a second copy of either bound
  in the bridge.
- **Never `await` a correlated request from inside an inbound-frame handler.**
  `background.ts` serializes inbound native frames through one chain
  (`enqueueInbound`/`onInbound`); a correlated reply can only arrive *through that same
  chain*. So `await this.requestTriageCounts()` inside an inbound case deadlocks the queue
  against the very `triage_counts_response` it is waiting for. Use `void` and let it settle
  on its own. Any handler that awaits its own kind of reply from inside this chain deadlocks
  the same way.
- **Extension unit tests manually whitelist DOM globals.** `extension/test/inbox.test.ts`
  loads the real `src/inbox.html` (good, no fixture drift) but exposes only specific DOM
  constructors on `globalThis` for `instanceof` checks (`HTMLElement`, `HTMLButtonElement`,
  `HTMLTimeElement`, …). Adding a new form control (e.g. an `<input>` checked via
  `instanceof HTMLInputElement`) needs the matching constructor added to that
  `Object.assign(globalThis, {...})` block too, or every test in the file fails with an
  unhelpful generic error.
- **happy-dom loads subresources over the real network by default, and `<link rel=preload>`
  bypasses its loader flags.** A default `new Window({ url })` fetches stylesheets and iframes
  (with UA `HappyDOM`, outside any logging), and a self-nesting iframe loaded 1196 times in
  1.5 s. Two scratch scripts that parsed stored pages in a loop without closing their windows
  reached 26-30 GB and crashed the Mac (2026-09-26). The settings that prevent it live in
  one module, `extension/test/hermetic-dom.ts` (loaders off, a fetch interceptor that
  refuses and records every request). `extension/bunfig.toml` preloads
  `extension/test/preload-hermetic.ts`, which makes them happy-dom's `DefaultBrowserSettings`
  before any window exists, so every `new Window(...)` in the suite inherits them. That
  only holds under `bun test` from `extension/` (CI and the Makefile run it there). Code
  that runs under `bun run` gets no preload: the adapter tools import `parseHTML`, which
  passes `hermeticWindowSettings` explicitly for that reason. Do the same in any script,
  and call `happyDOM.abort()` and `close()` on every window that you create.
  `internal/cli/adapter_repair_cycle_test.go` copies the harness and its imports into a
  temp tree, so a new import of `extension/test/harness.ts` must go into its file list too.
- **`ipc.MaxRequestBytes` MUST exceed `protocol.MaxBrowserMessageBytes` (256 KiB), and
  `ipc.MaxResultBytes` MUST bound the whole response.** The native host relays each browser
  frame to the daemon inside **one** `browser.sync` request, so a legal max-size frame has
  to fit the IPC cap. It did not: the cap was 64 KiB, so every `page_capture` over ~63 KiB
  failed the relay with `ipc.ErrTooLarge`, the host treated the Sync failure as fatal, sent
  `goodbye`, and the daemon tore down the live session — `nav_failed: browser session
  disconnected during page capture`, capture lost, every in-flight handoff re-parked.
  Fixture-size pages fit, so the harness looked healthy until a real provider page (MDPI:
  545 KB sanitized → 102 KB frame) hit the wall. This is the same class as the
  bridge-handler rule above, one layer down: **the transport is fail-fatal too.** Raising a
  frame cap or adding a large frame type means checking BOTH directions —
  `TestSyncRequestFitsMaxBrowserFrame` (`internal/nativehost`) and
  `TestSyncResponseFitsResultCap` (`internal/browser`) pin them, and the response side also
  depends on `maxOutstandingOffers`/`maxFocusFramesPerPoll` staying small.
- **Chrome forwards native-host stderr nowhere** — not to `chrome_debug.log`, even with
  `--enable-logging --v=0` — so a host that dies mid-session leaves no browser-side trace.
  Check `<DataDir>/native-host.log` first (see above) — the host already mirrors its stderr
  there. If that's not enough (host never started, or you need to drive it live), spawn
  `papio-native-host chrome-extension://<id>/` and speak native messaging on stdin (4-byte
  little-endian length prefix + JSON), starting with a valid `hello`
  (`{extension_version, adapter_versions}` — `features` live on the ACK, not the hello).
  That is what surfaced `browser.sync: ipc message exceeds size limit` after comparing the
  Go and TS validators had exonerated the frame. Same spirit: replay a suspect frame
  through the real parsers in isolation (`parseBrowserMessage` under bun,
  `protocol.DecodeBrowserMessage` in a throwaway Go main) before touching a browser.
- **`dev_reload` is gated by `DevReloadMinExtensionVersion = "0.15.0"` in `internal/browser/bridge.go`, same mechanism as `ProviderDirectGetMinExtensionVersion`.** The frame is a NEW message type rather than a field on an existing one because both parsers reject unknown fields — a daemon that sent it to a 0.14.x extension would have the whole message rejected. Consequence: `papio browser reload` against an extension below 0.15.0 fails with a version message; rebuild and load the new bundle (the one reload you still have to do by hand) to fix it.
- **`hello_ack.role` and `hello_ack.browser_holder_generation` are gated by `SessionRolesMinExtensionVersion = "0.15.0"` in `internal/browser/bridge.go`.** They are fields added to an existing message, so the released ext-v0.14.0 hello_ack rule (`extension/src/protocol.ts:3728-3732` at that tag) rejected every ack from daemon v0.22.0, holder and pending alike, and redialled on each 5 s popup refresh. Below the floor a holder gets a role-less ack and a denied hello gets only `session_busy` (the v0.21 wire); 0.14.0 has no pending role, so any ack reads as holdership to it. `TestReleased014StaysPendingBehindA015Holder` pins that rule in Go.

