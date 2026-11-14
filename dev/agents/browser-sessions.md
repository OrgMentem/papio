# Browser sessions and arbitration

Read this reference before work in this area. Paths in code spans and commands are repository-root-relative unless stated otherwise.

### Browser sessions & holder arbitration
- **Only one connected browser holds the bridge; the rest poll as pending.** Two browsers
  with the extension installed (the user's Chrome and a dev harness) will trade the slot —
  every service-worker restart re-claims it or is denied (`denied: session held by …` in
  the daemon log), and offers/captures route **only** to the holder, so a harness can
  silently steal the user's session. `papio browser use <id-prefix>` pins a chosen session
  (the demoted one stays pending and keeps polling); `papio browser sessions` lists them.
- **`dev_reload` is the one disconnect that is not a departure, and the bridge reserves the
  slot for it.** A reload tears down the native port, so `release` nils the holder and the
  ordinary succession path (`Sync`, `previous holder disconnected`) would hand the slot to
  whatever sibling browser is polling — measured at **4 of 5 reloads** with one idle sibling
  connected. The reloaded extension's fresh hello is then *denied* against the now-live new
  holder (`handleHello`'s `holderAlive && !sameSession` branch), so the browser you are
  developing in ends up pending with `session_busy` (every feature-gated surface failing
  closed, per the next bullet) and every later `papio browser reload` targets the **sibling**.
  `devReloadReservation` (10s, set when the frame goes on the wire, cleared by any hello or
  explicit `promote`) keeps the slot vacant for the browser that is coming back; the CLI
  additionally fails when an already-known session id captures the slot, because "holder
  changed" alone was reporting that theft as success. Three invariants to preserve if you
  touch this: an ordinary disconnect with no reload latched MUST still promote a sibling
  immediately; the reservation MUST stay bounded, or an extension that never comes back
  strands the bridge forever; and the latch MUST be dropped when its session stops holding
  the bridge (`promote` clears it), or it fires on a later promotion and restarts a browser
  long after its command was reported failed.
  The reservation is keyed on **time, not identity**, and that is a known limit rather than
  an oversight: a sibling whose own worker restarts inside the window arrives as a brand-new
  session id too, and nothing on the wire distinguishes it from the extension coming back —
  the reloaded extension would have to echo the `reload_id` on its `hello`, and `hello` is
  the frame least worth breaking. The CLI closes the gap by observation instead: a sibling
  that restarts necessarily **loses its old session id**, so a pre-reload session
  disappearing means the new holder cannot be attributed, and the command says so rather
  than claiming success. Note the consequence for `RequestDevReload`: it is idempotent while
  a latch is un-emitted (the IPC server answers calls concurrently, and overwriting handed
  the first caller a `reload_id` that was never delivered), but a request after the emit
  mints a new one.
- **A pending identified hello is acknowledged with an explicit role before it is
  refused holdership.** `handleHello` answers a denied identified hello with a
  `hello_ack` whose role is `pending`, then one `session_busy` error frame. The
  acknowledgement gives that session the daemon feature list without granting it
  holder work. The extension can therefore serve the holder-independent surfaces
  that `bridge.go` admits while it keeps offers, handoffs, materialization, effect
  retries, and other holder-routed work disabled. `session_busy` remains the
  extension's holdership-loss signal and maps to its `session_elsewhere` display
  state. The extension treats that frame as a settled answer, so requests fail fast
  instead of burning the 5s hello wait. A legacy empty-session hello never becomes
  pending: it keeps last-hello-wins behavior in both directions. A **claim-demoted**
  holder keeps its negotiated features and only loses offers and handoffs, so
  `daemonNegotiated()` — not `connectionStatus === "connected"` — gates capabilities.
  It learns it was demoted from the one-shot `demotedNotice` frame `Sync` sends after
  `promote()`, because a hello-time takeover drops the old holder (next poll →
  `expected_hello`) while a claim keeps it as pending.
- **Read a refused capture precisely.** `busy` = the holder's handoff drive slots are
  occupied by real work; `not_permitted` = the provider host grant is missing;
  `nav_failed: browser session disconnected …` = the transport teardown above, not a
  provider problem.
- **"Offered" is not "driving": a `job_accept` with `disposition: "queued"` means the
  browser holds no surface for that job.** The daemon tracks it in `queuedOffers`. A
  requires-auth offer that reaches a worker without release evidence (after a reload,
  say) becomes a tabless `engagement_required` park, and with `handoff_link_v1` the
  extension keeps no URL for it; the extension's own evidence drain skips it by design,
  so only a daemon re-offer moves it. Any daemon rule that assumes an offered job carries
  itself forward must check `queuedOffers` — the session-evidence sweep's source
  exemption in `reofferInstitutionalSiblings` did not, and stranded the only waiting
  paper after a sign-in until `papio actions open`.
- **Arbitration state is `b.mu`-guarded except what adoption must read.** Adoption
  ingestion (`ingestAdoptedFile` and everything under it) runs without `b.mu` by design,
  because it does file I/O and `b.mu` must never be held across that (TCC). Yet
  `weighArtifact`/`commitArtifact` fence the winner on the holder generation. So
  `sessionArbitration.epoch`, `Bridge.materializationGenerationUnavailable` and
  `sessionArbitration.status` are atomics; every other arbitration field (`holder`,
  `pending`, …) and the reoffer maps (`offered`, `queuedOffers`, `reofferPending`) are
  read only under `b.mu`. `reofferQueued` is the one reoffer map that commit paths write,
  under its own leaf lock `reofferQueueMu` (taken after `b.mu` when both are held, never
  the reverse). A new field that adoption must read needs the same atomic treatment.

