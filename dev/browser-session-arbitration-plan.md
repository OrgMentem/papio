# Browser session arbitration — fix P1 "multiple browsers silently fight over the bridge session"

## Context

The daemon bridge holds exactly one in-memory hello-session; every `hello` replaces it (`internal/browser/bridge.go:182-184`), so with two browsers connected (e.g. store extension in the daily driver + dev build in a web-ext profile) each native-host respawn steals the session: offers land on an arbitrary browser, `daemon status` version flaps, jobs stall. Fix: per-native-host-process session identity carried on the daemon↔host IPC envelope (NOT the browser protocol — no extension change needed, covers old store extensions), single-holder arbitration with deny + auto-takeover-on-stale + clean goodbye release, a `papio browser` CLI to inspect and switch sessions (the dev workflow), and doctor/status surfacing.

Decisions already made (do not revisit):
- **Identity rides the IPC `syncRequest`**, minted per native-host process. The browser protocol (`papio-browser/1`) is untouched; extensions of any version participate.
- **First hello holds; different session denied while holder is fresh; stale holder auto-yields; `papio browser use` switches explicitly.** No version- or activity-based auto-preference.
- **No durable job events for arbitration** (`job.Store.RecordEvent` requires a job ID; arbitration is job-independent). Log lines + in-memory counters surfaced via status/doctor instead.
- **Stateless request/response frames stay allowed from ANY session** (`page_acquire`, `triage_*`, `human_action_resolve*`, `review_preview_*`): clicking "Acquire this page" or using the inbox in the non-holder browser must keep working. Only the offer/handoff flow is holder-exclusive: `poll()` runs for the holder only, and job-scoped handoff frames (`job_accept`, `job_reject`, `auth_pending`, `auth_returned`, `download_started`, `download_complete`, `provider_outcome`, `cancel`, `handoff_outcome`) from a non-holder are dropped with one `session_busy` error frame.
- Extension-side takeover UI (`instance_id`/`takeover` hello fields, popup "Use this browser" button) is a separate later plan — out of scope here.

## Approach

Steps 1→2 are the core and sequential. Steps 3 (RPCs+CLI), 4 (status/doctor), 5 (native-host) depend on 2's bridge API; 3/4/5 are mutually independent.

### Step 1 — session identity on the IPC envelope (native host side)

`internal/nativehost/host.go`:
- In `Run` (`host.go:92-123`): mint `sessionID := hex.EncodeToString(random 16 bytes)` (crypto/rand; on rand failure fall back to `fmt.Sprintf("pid-%d-%d", os.Getpid(), time.Now().UnixNano())` — identity must never abort the host). Pass it into `ipcSyncer`.
- `syncRequest` (`host.go:160-162`) gains two fields:
  ```go
  type syncRequest struct {
      SessionID string            `json:"session_id,omitempty"`
      Goodbye   bool              `json:"goodbye,omitempty"`
      Messages  []json.RawMessage `json:"messages"`
  }
  ```
  `ipcSyncer` stores the sessionID and sets it on every call.
- **Goodbye on clean shutdown:** in `Run`, after `newBridge(...).run(ctx)` returns (any path — stdin EOF is the normal "browser closed the port" exit, `host.go:245-246`), best-effort send one `syncRequest{SessionID: id, Goodbye: true}` with a fresh 2-second context (not the cancelled ctx). Ignore its error entirely.
- Version-skew note (no code): host and daemon are the same binary; a not-yet-restarted old daemon may reject the new `session_id` param if `ipc.DecodeParams` is strict — acceptable, the AGENTS deploy order (`mv` binary → `papio daemon stop` → autostart) closes that window in seconds. No compat shim.

### Step 2 — bridge: single holder, pending sessions, deny/auto-takeover/release

`internal/browser/bridge.go`:

Data model (replace the flat hello fields at `bridge.go:70-79`):
```go
type browserSession struct {
    ID               string
    ExtensionVersion string
    AdapterVersions  map[string]string
    HelloAt          time.Time
    LastSyncAt       time.Time
}
```
- Bridge fields: `holder *browserSession`, `pending map[string]*browserSession`, `deniedHellos int`, `takeovers int`. Keep `helloSeen`/`extensionOutdated` semantics attached to the holder (`helloSeen ≡ holder != nil`); `offered`/`cancelSent` stay as-is but are RESET whenever the holder's session ID changes (promotion or takeover), exactly like today's fresh-hello reset.
- `const sessionStaleAfter = 10 * time.Second` — 5× the native host `pollInterval` (2s, `host.go:43`); a live host always syncs well inside it.

Signature change: `Sync(ctx, frames)` → `Sync(ctx context.Context, sessionID string, goodbye bool, frames []json.RawMessage)`. Callers to update: `internal/api/handler.go:653` (`browserSync`), every `b.Sync(` in `bridge_test.go` (the `runSync` helper at `bridge_test.go:128-143` grows a default session const `testSession = "sess-primary"`; add a `runSyncAs(t, b, sessionID, frames...)` variant for multi-session tests — all existing tests compile via `runSync` unchanged).

`Sync` entry logic (before the frame loop at `bridge.go:148`):
1. `goodbye && sessionID != ""` → release: if holder matches, clear holder (log `papio: browser session <id8> disconnected`); always delete from pending; return empty.
2. `sessionID != ""` → stamp `LastSyncAt = b.now()` on whichever of holder/pending matches.
3. **Auto-takeover:** holder exists, `sessionID` is a pending session, and `b.now().Sub(holder.LastSyncAt) > sessionStaleAfter` → promote: pending becomes holder, old holder is dropped (it stopped syncing — do not keep it pending), reset `offered`/`cancelSent`, `takeovers++`, log. The promoted session then proceeds normally (its poll in this same Sync receives the outstanding offers — same one-sync recovery shape as the handoff re-arm).
4. `sessionID == ""` (legacy host) → exact current behavior: single implicit session, last-hello-wins. Implement as a reserved id `"legacy"` internally so the code has one path; a real-id hello while `"legacy"` holds replaces it (legacy host can't be arbitrated), and vice versa.

`handle` hello case (`bridge.go:182+`):
- No holder, or holder.ID == sessionID → become/refresh holder (reset `offered`/`cancelSent` as today), remove from pending, normal hello_ack.
- Different live holder (fresh within `sessionStaleAfter`) → upsert into `pending`, `deniedHellos++`, reply with the existing error-frame mechanism (pattern: `helloRequired`, `bridge.go:665-671`):
  `{code: "session_busy", message: "another browser holds the papio session (v<holderVersion>); run 'papio browser sessions' or 'papio browser use'"}`. Do NOT set outdated/holder state. (Old extensions log the error frame and idle — protocol.ts already parses arbitrary error codes.)
- Different STALE holder → takeover at hello: replace holder (rule 3's logic), normal hello_ack.

Non-holder frame policy (top of `handle`, after the hello case): if `sessionID` is not the holder's:
- Allowed pass-through: `MsgPageAcquire`, `MsgTriageSnapshotRequest`, `MsgTriageCountsRequest`, `MsgTriageDecide`, `MsgHumanActionResolve`, `MsgReviewPreviewRequest`.
- Everything else job-scoped → drop, reply one `session_busy` error frame (with the frame's job_id).
- `poll()` (`bridge.go:973`) is invoked from `Sync` only when `sessionID` matches the holder (or legacy).

`SessionInfo()` (`bridge.go:130-134`) keeps its signature (holder-only view). Add:
```go
type SessionSummary struct {
    ID               string `json:"id"`
    ExtensionVersion string `json:"extension_version"`
    Holder           bool   `json:"holder"`
    HelloAt          string `json:"hello_at"`
    LastSyncAt       string `json:"last_sync_at"`
}
func (b *Bridge) Sessions() (sessions []SessionSummary, deniedHellos, takeovers int)
func (b *Bridge) Claim(sessionID string) error  // promote a pending session
```
`Claim`: unknown id → `fmt.Errorf("unknown browser session %s (run 'papio browser sessions')", id)`; already holder → nil no-op; else demote current holder into `pending` (it is still live and polling), promote target, reset `offered`/`cancelSent`, `takeovers++`. Prefix match on the id is accepted when unambiguous (ids are displayed truncated to 12 hex chars).

### Step 3 — RPCs + `papio browser` CLI

`internal/api/handler.go`:
- Update `browserSync` params struct (`:647-649`) with `SessionID string json:"session_id,omitempty"` and `Goodbye bool json:"goodbye,omitempty"`; pass both through.
- Two new methods in the router map (pattern of `"browser.sync"`, `:154-156`):
  - `"browser.sessions"` (read-only) → `{"sessions": [...SessionSummary], "denied_hellos": n, "takeovers": n}`.
  - `"browser.claim"` params `{"session_id": "..."}` → `{"claimed": true, "session_id": "<full id>"}`; bridge `Claim` error → `invalid_argument`.

`internal/cli/` new file `browser.go`, registered in `root.go:77-89` (`newBrowserCommand(opt)`):
- `papio browser sessions` (annotation `mcp:read-only`): text rows `<id12>\t<holder|pending>\t<version>\tlast sync <age>`; `--json` prints the RPC result verbatim. Zero sessions → `no browser has connected since daemon start`.
- `papio browser use [session-id]` with `--latest` flag: exactly one of positional id or `--latest` required (error otherwise). `--latest` fetches `browser.sessions`, picks the pending session with the newest `last_sync_at` (no pending → error `no other browser session to switch to`), then calls `browser.claim`. Success prints `browser session <id12> (v<version>) now holds the papio session`.
- Follow the `newActionsCommand` structure (`commands.go:148+`); age formatting reuses `formatStatusAge` if exported, else inline `time.Since` rounding — check `internal/cli/status.go` first.

### Step 4 — status + doctor surfacing

- `statusResult` (`handler.go:196-205`): add `PendingBrowserSessions int json:"pending_browser_sessions,omitempty"` and `BrowserSessionDenied int json:"browser_session_denied,omitempty"`; fill in `ping` (`:213-215`) from `Bridge.Sessions()` (pending = non-holder count). Version flap fixes itself: denied hellos no longer touch `SessionInfo`.
- `internal/doctor/doctor.go`: mirror the two fields on the local status struct (`:196-201`). Extension check (`:275-283`): when connected AND `PendingBrowserSessions > 0`, keep Pass but set detail `connected (v<X>); N other browser(s) waiting` with fix `run 'papio browser sessions' and 'papio browser use' to switch, or disable the papio extension in browsers you don't use`.

### Step 5 — native-host + handler tests

- `internal/nativehost/host_test.go`: `fakeSyncer` (`:20-34`) records the session envelope — change `Syncer` interface? NO: the `Syncer` interface (`host.go:83-87`) stays `Sync(ctx, messages)`; the session id/goodbye are `ipcSyncer` concerns. Instead test `ipcSyncer` directly with a stub `ipc` transport is overkill — assert at the JSON layer: give `ipcSyncer` a fake `*ipc.Client`? `ipc.Client` is concrete; if not stubbable, extract `ipcSyncer.request(goodbye bool, msgs)` building the `syncRequest` and unit-test that marshalled JSON contains a 32-hex `session_id` (and `goodbye:true` on the goodbye call). Also test: `Run`'s goodbye fires after clean EOF — cover via the extracted builder + a flag on ipcSyncer recording the goodbye call if `Run` refactoring allows; otherwise the builder test plus bridge-side goodbye tests suffice (state this is the accepted coverage).
- `internal/api/handler_test.go`: `browser.sessions` returns the holder after a synced hello; `browser.claim` with unknown id → `invalid_argument`.

### Step 6 — bridge behavior tests (the load-bearing ones)

`internal/browser/bridge_test.go` (harness: `newBridge`, `park`, `hello()`, `runSync`; bridge `now` is injectable — confirm the field, else add a settable clock the way `b.now` is already stored at `bridge.go:80`):
1. **Deny:** hello as `sess-A` (holder), hello as `sess-B` → error frame `session_busy`; `SessionInfo()` still reports A's version; a parked job's offer goes only to A; B's poll returns no offers.
2. **Stateless pass-through:** `page_acquire` from `sess-B` while A holds → normal ack (job submitted).
3. **Handoff frame block:** `job_accept` from B for A's offered job → `session_busy` error frame, no `browser.job_accept` event.
4. **Auto-takeover:** A stops syncing; advance clock past 10s; B's next sync auto-promotes B and its poll re-offers the parked handoff (`job_offer` frame present); `SessionInfo` now reports B.
5. **Goodbye:** A sends goodbye → holder cleared; B's next hello becomes holder immediately (no stale wait).
6. **Claim:** A holds, B pending; `b.Claim(B)` → B receives offers on next sync, A demoted to pending; `b.Claim("nonexistent")` errors.
7. **Legacy:** `sessionID=""` for every call reproduces today's semantics (existing suite passing via the `runSync` default is the regression net; add one explicit last-hello-wins test with empty ids).

## Critical files & anchors

- `internal/browser/bridge.go` — `Sync` (`144-178`), hello in `handle` (`182-201`), `SessionInfo` (`130`), `helloRequired` error-frame pattern (`665`), `poll` (`973`): all arbitration lands here.
- `internal/nativehost/host.go` — `Run` (`92-123`), `ipcSyncer`/`syncRequest` (`152-178`), clean-EOF exit (`245-246`), `pollInterval` (`43`).
- `internal/api/handler.go` — router map (`154-157`), `browserSync` (`646-665`), `statusResult`/`ping` (`196-215`).
- `internal/doctor/doctor.go` — status mirror struct (`196-201`), extension check (`275-283`).
- `internal/cli/root.go` — command registration (`77-89`); `internal/cli/commands.go` `newActionsCommand` (`148`) as the structural template for `browser.go`.

## Verification

```
go build ./... && go vet ./...
go test ./internal/browser ./internal/nativehost ./internal/api ./internal/cli ./internal/doctor
go test ./...   # full suite last
```
No extension changes: `cd extension && bun run typecheck && bun test` must pass untouched (run once to confirm no accidental protocol drift).

New-behavior proof (beyond the Step 6 unit tests):
- **Two-browser fight repro, in-process:** Step 6 tests 1+4 ARE the bead's repro (second browser steals within seconds → now: denied, then takes over only after the holder goes silent >10s).
- **CLI:** `internal/cli` test via `NewInProcessRoot` stubbing `browser.sessions`/`browser.claim`: `papio browser use --latest` picks the newest pending and calls claim; `papio browser use` with no args and no `--latest` errors.
- **Live smoke (deployed binary, both browsers):** start daily Firefox (store ext) + dev browser; `papio browser sessions` shows holder+pending with stable versions (no flap in `papio daemon status`); `papio browser use --latest` flips offers to the dev browser (park a job, watch the handoff tab open there); quit the dev browser → within ~10s the daily browser auto-reclaims (visible in `browser sessions`). Deploy order per AGENTS: build → mv → `papio daemon stop` → next command autostarts.

## Assumptions & contingencies

- **`ipc.DecodeParams` strictness:** if it rejects unknown params fields, the old-daemon/new-host skew window during deploy is seconds (AGENTS order) — accepted, no shim. If it is lenient, nothing to do.
- **`ipc.Client` not stubbable for the goodbye test:** fall back to unit-testing the extracted `syncRequest` builder plus bridge-side goodbye behavior (Step 6 test 5); do not introduce a transport abstraction just for one test.
- **Legacy sentinel collision:** the reserved internal id `"legacy"` cannot collide with real ids (32 hex chars); if implementation finds the sentinel awkward, an unexported bool flag on `browserSession` is equivalent — either way, exactly one code path for arbitration.
- **`extensionOutdated` interaction:** the outdated gate (`bridge.go:162-164, 206-208`) applies to the HOLDER only; a denied pending session is never version-gated (it gets `session_busy` first). If tests reveal an ordering issue, evaluate outdated after holder assignment, never before.
- **10s stale window too slow/fast in live smoke:** tune `sessionStaleAfter` between 6s (3× poll) and 15s; never below 2× poll.
