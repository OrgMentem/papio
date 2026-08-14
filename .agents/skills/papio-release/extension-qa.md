# Extension QA gate — agent execution runbook

The **what to check** and the **how to execute it** both live here; `SKILL.md`
routes to this file and keeps no second copy of the matrix.

Automated e2e against the extension is architecturally prohibited: driving it
means attaching to the user's own browser over CDP or WebDriver, and both
`navigator.webdriver` and CDP attachment get fingerprinted by provider
anti-bot — the same detection this project exists to avoid tripping. This
manual matrix is therefore the extension's **permanent** release gate, not a
stopgap to automate away. Run it before every store submission (Release order
step 3), on Chrome stable and the built `firefox/`, against a deployed daemon.

Drive the browsers with **native desktop control** (the `computer` tool: AX
tree, screenshots, native input). Never substitute CDP, WebDriver, Playwright,
or the `browser` tool — using them to test this would violate the property
under test.

## 1. Preflight — do these in order, before touching a browser

1. **Displays must be awake.** `desktop.displays()` returning an empty list, or
   a focused `loginwindow`, means the session is locked and the entire visual
   matrix is unrunnable. Check this *first*; it is the cheapest possible
   blocker to discover.
2. **Start the driver after any harness upgrade.** Native addons cannot reload
   in-process. If `omp` upgrades while a QA agent is resident, that agent's
   `computer` backend dies irrecoverably mid-run and silently degrades to
   AppleScript. A fresh process is the only fix.
3. **Freeze the artifact.** Record `git rev-parse HEAD`, confirm it equals
   `origin/main`, and confirm `git status --short` is empty. Record the built
   bundle mtimes:
   `stat -f '%Sm %N' extension/dist/*.html extension/dist/background.js`.
4. **Deploy the matching daemon** (`make dev-deploy DEV_VERSION=…` with a
   unique stamp) so daemon, CLI, and native host share one provenance point.
   Confirm with `papio version && papio daemon status && papio doctor --json`.

### Provenance is the rule that catches the failure you cannot see

A pass is only about the code you froze if the *loaded* extension is that code.
Two distinct ways this run silently produced meaningless evidence:

- **A sibling session edited a tracked file mid-run.** Re-check
  `git status --short` before and after every phase; any tracked change
  invalidates the pass. Untracked/ignored paths (`dev/scratch/`) do not.
- **The loaded service worker predated the build.** Chrome had loaded the
  extension at 10:13 while the bundles were built at 11:43:45, so every UI
  observation mixed old worker with new pages. **The reload timestamp must be
  later than the bundle mtime**, and you must record both.

### Evidence file

Append per-item verdicts to `dev/scratch/extension-qa-evidence-<sha>.md` **as
you go**, not at the end. `dev/scratch/` is gitignored (`.gitignore:26`), so
this does not dirty the tree — verify that with `git status --short` after the
first append. This file is what lets a successor resume after a context
compaction or a driver death, both of which happened this run.

## 2. Priority order

Required gates first; a run that dies mid-way must die having done the
irreplaceable parts. Optional checks are allowed to consume what is left.

1. Preflight and provenance (§1)
2. Chrome surfaces and copy (§4.1)
3. Firefox (§4.2)
4. Lifecycle and connection (§4.3)
5. Real transport proof (§4.5)
6. Skew matrix (§4.4)
7. Security and update lifecycle (§4.6, §4.7)

Do not spend a required gate's budget on an optional one. This run burned ~20%
of a context window on DevTools console AX for an optional security sub-check
and nearly lost the Firefox gate.

## 3. Execution mechanics

**Prefer AX to pixels.** `win.find({role, title})` then `el.click()` needs no
screenshot and does not drift. Pixel clicks near browser chrome hit the wrong
target — a toolbar pixel hunt in this run hit Firefox's site-identity shield.

**Keyboard input to a multi-window browser** throws `BackgroundUnavailable`
(macOS accepts only a process id and may key the wrong window). Use
`delivery: "foreground"`, or AX actions.

**The toolbar popup** is reachable via the `Loaded by extension: papio` AX
image's **parent** button — `(await img.parent()).click()`.

**Extension page URLs are `dist/`-prefixed.** The manifest declares
`dist/popup.html`; the built pages are `dist/{popup,options,inbox,history,materialize,page-bulk}.html`.

| Browser | Base | Identity |
| --- | --- | --- |
| Chrome | `chrome-extension://<id>/dist/<page>.html` | unpacked id is derived from the checkout path (`ehhfplhmddankkocjpldplaokajlbmah` for `/Users/ellis/@dev/papio/extension`); `extension/dev-unpacked` is `maghibajggmcgmbeoipnlfmceillgapk`. Verify at `chrome://extensions-internals`. |
| Firefox | `moz-extension://<uuid>/dist/<page>.html` | add-on id is `papio@orgmentem.com`, but the **UUID is regenerated on every temporary load** — read it from `about:debugging` each run, never hardcode it. |

> Navigating to a **bare** path (`moz-extension://<uuid>/inbox.html`) yields a
> blank Quirks-mode document plus a spurious *"Content Script execution … was
> blocked"* warning. That looks exactly like a real rendering regression and is
> not one — papio declares no `content_scripts` at all. Check the path before
> filing a blocker.

### The autostart rule — this is what makes the daemon-down checks runnable

**Every `papio` command autostarts the daemon except `papio daemon status` and
`papio daemon stop`.** So during any daemon-down observation window, run *no*
papio command besides those two. Observe only through screenshots and AX.

This is the whole reason the disconnect-banner, disconnect-mutation, and
badge-precedence checks were previously recorded as unobservable: the operator
kept invoking `papio` to inspect state, which resurrected the daemon in under a
second, before the UI could be captured. They are runnable; the discipline is.

## 4. The matrix

### 4.1 Chrome — new extension + new daemon

- [ ] Reload via `chrome://extensions`; record the reload wall-clock and prove
      it is later than the bundle mtime. Confirm version, unpacked path, and id
      at `chrome://extensions-internals`, and confirm a fresh holder
      hello/sync (`papio browser sessions --json`) before counting any UI
      evidence.
- [ ] Popup, inbox, options, and page-bulk render current copy and layout:
      manual-download rows read **`Open link`** (never `Open source`); the
      catch-up setting's label and explanation are separated; page-bulk states
      that short citation labels stay in the browser; Page scanning and
      Provider access are distinct sections; popup controls are not clipped;
      connected state and counters are coherent.

### 4.2 Firefox

- [ ] Load `extension/firefox/manifest.json` via `about:debugging`. Confirm
      version and the gecko id, then read the fresh UUID.
- [ ] `dist/inbox.html`, `dist/options.html`, and the toolbar popup all render,
      with counts matching Chrome's for the same daemon state. Firefox-specific
      consent copy is present and distinct (host permissions are runtime opt-in
      on Firefox; Chrome grants at install).
- [ ] Event page: "Terminate background script" in `about:debugging`, then act.
      State re-establishes. papio's keepalive means the status may read
      `Running` again immediately — prove the lifecycle from a **fresh holder
      session** in `papio browser sessions --json`, not from that label.
- [ ] Holder arbitration behaves: pending/denied hello and takeover counts move
      coherently; no permanent takeover of the user's session.

Firefox is the user's real profile. Temporary add-ons unload on restart, so the
load itself is non-destructive — but do not alter profiles, other tabs,
installed extensions, or provider grants.

### 4.3 Lifecycle and connection

- [ ] Chrome: let the service worker idle 30s+, then act — the first request
      reconnects and succeeds, no stuck spinner.
- [ ] Daemon restart with a papio page open: the reconnect banner appears and
      the page recovers. Hold the daemon down per §3's autostart rule.
- [ ] A mutation attempted during disconnect fails cleanly, is not replayed on
      reconnect, and a refresh shows canonical state. Use an
      extension-local setting, never a daemon job/action/provider effect.
- [ ] Singleton pages focus the existing tab instead of duplicating; a
      duplicate opened by direct URL does not corrupt shared state.
- [ ] Badge precedence resolves when several states are simultaneously true
      (disconnected outranks a pending count); the tooltip names the active
      state.
- [ ] A notification click opens/focuses the right tab. Use
      **`papio notify test`** to send one local notification — do not wait for
      a real acquisition event, and never manufacture one by mutating daemon
      data.

### 4.4 Skew matrix

The old artifacts are not kept anywhere, so each of these begins by
**reconstructing** one from its tag in a throwaway worktree. Nothing here
touches the working tree.

- [ ] **old extension + new daemon.** `git worktree add /tmp/qa-ext-old
      ext-v<prev>`, then `cd /tmp/qa-ext-old/extension && bun install
      --frozen-lockfile && bun run build`. Load that `dist/` (Chrome, disposable
      `--user-data-dir`) or its `firefox/manifest.json` (temporary add-on).
      Prior behavior is unchanged and the daemon never emits a frame type the
      old extension cannot parse (check `<data-dir>/daemon.log` and
      `native-host.log`).
- [ ] **new extension + old daemon.** `git worktree add /tmp/qa-daemon-old
      v<prev> && go build -o /tmp/papio-old ./cmd/papio`. Give it an **isolated
      config** whose `data_dir` differs from the live one — the IPC socket is
      `<data_dir>/papio.sock` and the store refuses to open a `user_version`
      newer than the binary's newest embedded migration, so isolation is
      mandatory, not hygiene. Repoint the native host with
      `/tmp/papio-old native-host install`, kill the running
      `papio-native-host`, run the checks, then **restore** with
      `papio native-host install` from the real binary and kill the host again.
      Expect the compat/feature-unavailable state, not an error, and no
      unknown-message-type errors in the log. This one reaches into the user's
      live browser wiring — never run it while an acquisition is in flight, and
      verify restoration with `papio native-host status` and `papio doctor`.
- [ ] **fresh install, no daemon running.** A disposable Chrome
      `--user-data-dir` needs `com.orgmentem.papio.json` copied into
      `<user-data-dir>/NativeMessagingHosts/` or `connectNative` fails with
      "host not found". `papio daemon stop`, then observe under §3's autostart
      rule: popup and extension pages render daemon-down states cleanly.

### 4.5 Real transport proof

- [ ] One real page capture, effect lane `0/1` before and after
      (`papio pulse --json`):
      `papio adapter capture '<real provider article URL>' --provider <p> --scenario success`
      must report `outcome: captured`; record the path and byte size.

Fixture-size pages fit every cap; only a real provider page exercises the
`ipc.MaxRequestBytes` / `protocol.MaxBrowserMessageBytes` relationship, whose
violation used to tear down the whole browser session. Do not navigate to or
download the PDF, and do not change provider state.

### 4.6 Security spot-checks

- [ ] Hostile or markup-bearing text from a provider or the daemon renders
      **inert** everywhere it is shown (lists, dialogs, notifications) — never
      as HTML. This is UI-observable and belongs in the manual gate.

The sender-authorization boundary is **code-verified**, not eyeballed:
`extension/test/background.test.ts` pins "inbox runtime messages validate the
exact extension sender", "papio.activity and counts accept popup senders while
snapshot and mutations stay inbox-only", and "papio.stats rejects foreign
senders and malformed requests without touching the bridge". A deterministic
test asserts message rejection better than a DevTools console can, so do not
spend the gate's budget re-deriving it by hand; cite those tests instead.

### 4.7 Update lifecycle

- [ ] With a papio page open, trigger an update (unpacked reload, or a real
      store update): the update banner appears and the reload flow lands on the
      new version. A same-version unpacked reload may legitimately show no
      banner — distinguish that precisely and do not claim a banner you did not
      see.

## 5. Verdict discipline

Report every item **PASS / FAIL / NOT-RUN**. Any FAIL blocks release.

- **Never infer PASS from unit tests, a build, or a lint run.** The one
  exception is §4.6's sender boundary, and only because it names the exact
  tests that own it.
- **Every NOT-RUN names the concrete missing prerequisite** ("no prior
  extension artifact and no authorization to build one", not "couldn't test").
- A refused capture is diagnostic, not noise: `busy` = holder drive slots
  occupied; `not_permitted` = missing provider host grant;
  `nav_failed: browser session disconnected …` = transport teardown, not a
  provider problem.

## 6. Teardown

Close only tabs and windows you opened. Restore any extension-local setting you
changed and the native-host symlink if §4.4 repointed it. Remove throwaway
worktrees (`git worktree remove`). Re-verify: clean tree, unchanged HEAD,
`papio version`/`daemon status`, holder session, and effect lane `0/1`.
