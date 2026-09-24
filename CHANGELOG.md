# Changelog

All notable changes to the *papio* daemon and CLI are documented here, keyed
to `v*` release tags. The browser extension is versioned and released
independently (`ext-v*` tags): from extension 0.3.1 onward its changes live in
`extension/CHANGELOG.md`. Through `[0.3.0]` the two shared a version stream,
so older sections below include extension entries. The initial release entry
is synthesized from the complete `papio` and `zotio` Git histories and the
execution records kept during the initial build.

## [Unreleased]

### Added
- **`papio stats producers` counts a signed viewer PDF that the extension
  saved as `viewer_capture`.** Before, such a paper (for example a
  ScienceDirect PDF that Firefox kept a copy of, or that Chrome's download
  rule saved) counted as `unknown`, with the basis `no_download_record`. The
  daemon often adopts that file before the extension's download report
  arrives, so nothing named the source. The extension now sends a
  `viewer_capture` message before the file can land, and the
  `artifact.producer` record names the adapter that matched the paper's page.
  An extension that asks for the new record gets the
  `native_viewer_download_v2` feature in the handshake, in the place of
  `native_viewer_download_v1`, and sends the message only when it sees that
  feature. The handshake still lists 32 features. An older extension sees no
  change, and a daemon older than this one never receives the message.

### Changed
- **A new paper reaches its Zotero collection, with its DOI and abstract,
  when Zotero desktop saves it.** Before, *papio* filed the paper and filled a
  missing DOI and abstract after the import, through the Zotero Web API. The
  Web API has the paper only after Zotero desktop syncs it, so both steps
  waited for that sync, and never finished while sync was paused. *papio* now
  gives the collection, the DOI and the abstract to zotio with the paper, and
  Zotero desktop saves and files them in one session. The abstract comes from
  the DOI registry (Crossref or DataCite), or from OpenAlex when the registry
  has none and `[zotio] auto_enrich` is on; *papio* never replaces a value that
  the registry supplied. The job's events record these steps as `applied`
  with `with_import`. *papio* still runs the Web API steps after the import
  when the save did not do them: for a paper that was already in your
  library, for a collection name that matches no collection or more than one
  (or a collection list that *papio* cannot read to its end), and for a save
  that zotio did not report as complete.
- **In `stored` mode, a new paper goes to Zotero desktop only.** *papio* now
  asks zotio for `--via connector` in place of `--via auto`. With `auto`, a
  closed Zotero desktop sent the paper through the Web API, which stored the
  PDF in Zotero's own file storage whatever storage you chose in Zotero. A
  closed Zotero desktop now fails the import, and the import is tried again
  later. If Zotero desktop does not
  show the job's collection yet, for example a collection that the Web API
  created before the desktop synced, zotio saves nothing. *papio* then records
  the collection filing as `deferred`, imports the paper without the
  collection in the same pass, and files it after the import as before. A
  `linked-file` import does not change: it goes through the Web API, which
  has the paper at once.

### Removed
- **The experimental macOS helper that saved a PDF from Firefox's viewer.**
  The next extension release saves that PDF itself, with nothing to build or
  install. The daemon no longer offers the helper's save to the extension, and
  `papio stats producers` no longer lists `native_viewer`; a paper that the
  helper saved counts as `unknown` there. A config that still sets
  `browser.native_viewer_helper` keeps loading: *papio* ignores the value, and
  `papio doctor` shows a `config_ignored_settings` warning until you delete the
  line. If a helper save was interrupted before you upgraded, its effect permit
  can still hold the browser effect lane. *papio* marks it
  `unknown_completion`, and `papio doctor` reports it; release it with
  `papio browser resolve <permit-id> --reason <text>`. A saved file the helper
  left in the download folder as `native_stage_<id>.tmp` is no longer moved
  into the paper's folder; it stays where it is for you. A manual-download
  task in the inbox no longer shows a `Diagnosis` line.

### Fixed
- **A sign-in on a new tab for your library is recorded again.** One library
  sign-in often serves many papers, and *papio* keeps one login record open
  across them. Each browser tab numbers its sign-in reports from zero, but
  the daemon compared every report with the highest number that any earlier
  tab had used. After a few papers, it refused every report from the next
  tab: the sign-in return, the arrival on the article, and the progress
  reports. The daemon log showed each refusal as `stale ()`. So a completed
  sign-in did not release the other papers that wait for your library. The
  daemon now compares a report only with the earlier reports from the same
  tab. A late report from that tab is still refused, and the log now says
  why. This change adds database migration 55.
- **A late download report for a paper that is already filed no longer says
  the download "needs attention".** The daemon can file a PDF from the
  download folder before the browser reports that the download finished. For
  a Firefox viewer capture, the report came 14 seconds later. By then the
  daemon had removed the paper's download folder, so the report started a
  second adoption, which failed and recorded `browser.adoption_deferred`
  twice. The daemon now recognizes a report for the download that the ready
  paper came from, records one `browser.delivery_provenance_unconfirmed`
  note, and does nothing else. A download that started after the paper was
  filed still records `browser.adoption_deferred`. The deferral reason now
  names the configured download folder, not the old `<data_dir>/adoptions`
  folder that is only searched for leftover files.
- **`papio actions open` shows a paper's tab again after papio has opened
  the paper's route.** When *papio* had already sent the paper's tab to the
  library, the command recorded the open and did nothing in the browser. It
  now brings that tab to the front. It does not open a second tab, and it does
  not load the route again. This works with extension 0.15.0 and later.
- **A paper imported through Zotero desktop reaches its collection and gets
  its abstract.** After an import, *papio* files the paper into the job's
  collection and fills a missing DOI and abstract. zotio does both through the
  Zotero Web API, and both first read the new item there. An import through
  Zotero desktop reaches the Web API only after the desktop syncs it, some
  seconds later, but *papio* asked about one second after the import. So the
  Web API answered HTTP 404, and nothing asked again: the paper stayed outside
  its collection and without its abstract. The daemon now tries such a
  follow-up again after 1 minute, and after each further failure after 10
  minutes, 1 hour, and 6 hours, and then stops. It does not import the paper
  again, and it does not retry any other failure. The config key
  `[zotio] auto_import_paused` pauses these retries too. The
  job's events now name the 404 for the enrichment, which read `unknown` and
  "mutation incomplete" before, and the hint for both says that Zotero
  desktop has not synced the item. `papio activity` no longer shows a failed
  filing as "Filed into Zotero collection" or a failed enrichment as "Zotero
  metadata enriched".
- **A paper acquired while Zotero desktop is closed waits for Zotero and
  reaches it after you open it.** When an import failed, the daemon tried
  again on each one-minute maintenance pass. A closed Zotero refuses every
  import, so the daemon used all five attempts in four minutes and then
  stopped. The paper stayed out of Zotero after you opened it, and nothing
  told you. Now, when an import must go through Zotero desktop (every import in
  `stored` mode), the daemon asks zotio whether Zotero runs before it tries. While Zotero is closed, the
  paper waits. The daemon does not try the import, uses none of the five
  attempts, and records one `waiting` import event for the paper. One
  `zotio desktop wait` process sleeps until Zotero starts, and then the
  daemon imports the waiting papers at once, at the usual pace of 3 papers a
  minute. The papers that must wait for a later pass change from `waiting`
  to `queued`, so no surface tells you to open a Zotero that is open. You get
  one desktop notification when papers start to wait: "Zotero is closed. 3
  papers are ready to add. Open Zotero and papio adds them." A notification
  that quiet hours held is dropped if Zotero opens before it is shown, or
  when no paper waits for that reason any more. `papio activity`
  and the extension show "Waiting for Zotero desktop", `papio status` shows
  `import=waiting` and what to do, and the new `papio doctor` check
  `zotero_desktop_waiting` gives the number of papers. When Zotero is open
  but does not respond, or its connector is turned off, the papers also wait
  without using attempts. Then every surface tells you to restart Zotero, or
  to turn on "Allow other applications to communicate with Zotero" in its
  Advanced settings, and you get one notification for that condition.
  *papio* asks zotio again every 3 minutes, and imports the papers when
  Zotero answers. If zotio cannot say whether Zotero runs, an import that
  Zotero's connector refused also waits and uses no attempt. The
  `undelivered_zotero_imports` check no longer counts waiting or queued
  papers, and a batch report gives them the reason `import_waiting_for_zotero`
  or `import_queued`. This needs a
  zotio that has the `desktop status` and `desktop wait` commands. With an
  older zotio, and for every other import failure, the daemon tries a failed
  import again after 1 minute, 10 minutes, 1 hour and 12 hours. A paper that
  already used its five attempts before you upgrade does not try again by
  itself; import it with `papio zotio import-backfill --apply`.
- **One Zotero import of a paper runs at a time.** The import that starts
  when a paper becomes ready and the import-retry pass did not know about
  each other. When the pass ran during the first import, it imported the same
  paper again. zotio refused the second import because the first one was
  still running, and *papio* counted the refusal as a failed attempt. Now the
  second import skips the paper and records nothing.
- **`papio zotio apply` refuses a plan whose zotio manifest changed after
  the preview.** zotio reads the manifest from disk when it applies a new
  item, but the plan's confirmation digest covered only the manifest's file
  name. A manifest that changed after `papio zotio plan` was therefore applied
  under the old confirmation. The plan now records the manifest's SHA-256, the
  digest covers it, and the apply stops with `plan_confirmation_mismatch`
  before zotio runs. Each plan also gets a manifest file of its own, so a
  second plan for the same paper cannot rewrite the first plan's manifest. A
  new-item plan made by an earlier *papio* has no manifest SHA-256 and is
  refused; run `papio zotio plan` again.
- **A Zotero import that saved the paper's item but did not finish no longer
  creates the item a second time.** Zotero desktop saves a new item, files
  it, and then saves its PDF in separate steps. When a later step failed,
  *papio* recorded an ordinary failed import. The next attempt asked zotio
  about the paper again, zotio did not show the saved item yet, and the paper
  was created again as a duplicate. *papio* now records the saved item on the
  job as `zotio.import_committed` and never creates another item for that
  job. It waits until zotio shows the item, then attaches the PDF to it and
  files it after the import. If zotio shows more than one item for the paper,
  the import stops and asks you to delete the extra items in Zotero.

## [0.22.1] - 2026-09-23

### Fixed
- **The released 0.14.0 extension connects to this daemon again.** Daemon
  0.22.0 added a `role` field to the handshake reply. The 0.14.0 extension
  rejects any field it does not know, so it dropped the connection and
  reconnected every 5 seconds, and its popup showed that the daemon was not
  reachable. This happened whether it held the bridge or waited behind
  another browser. An extension below 0.15.0 now gets the handshake it
  knows. When another browser holds the bridge, it gets only the
  `session_busy` notice and waits without reconnecting.
- **An extension reload during a direct download no longer stops every
  browser effect until you resolve a permit by hand.** When the extension
  reloaded seconds after papio offered a direct route, the browser could save
  the download with no result reaching the daemon. The permit stayed
  `unknown_completion`, held the global effect lane, and papio refused every
  later browser effect as busy, for every paper. The adoption sweep now
  settles such a `direct_get` permit from evidence it reads itself. A file the
  route saved into the job's adoption directory settles it: a PDF is adopted
  and validated as usual, and anything else is moved to `rejected/<job>/` so
  it is not adopted. With no file and no download event since the permit, it
  settles only after a new holder generation replaced the one that received
  it and its lease has expired. Each settlement records an
  `effect_permit.orphan_resolved` event with its evidence. A held permit, a
  download still being written, or two candidate files are never settled, and
  `papio doctor` still reports any permit papio cannot resolve.

## [0.22.0] - 2026-09-23

### Added
- **The paced drive works through parked handoffs without you.** With
  `[drive] enabled = true` (off by default), the daemon opens one parked
  handoff at a time, oldest first, through the same path as `papio actions
  open`, and records `drive.paced_open` plus `handoff.opened` with principal
  `pacer`. It opens at most 10 papers an hour, leaves a paper alone for 6
  hours after it opens it, and waits while a claim, effect permit or sign-in
  is live, while no browser holds the session, during `notify.quiet_hours`,
  and for any provider host in cooldown (every paper under that DOI prefix).
  A manual download is redriven first when `papio jobs redrive` would accept
  it; an `openurl_available` advisory is never opened. When one paper's
  sign-in does not return within 10 minutes, the drive settles that paper,
  cools its route (its DOI prefix) for 6 hours and goes on to the next paper.
  Only when the two most recent paced papers, from different providers, both
  stall at the sign-in does the drive pause and send one notification; it
  resumes when any sign-in returns. `papio drive status` shows what it would
  do and what blocks it; `papio drive pause` and `papio drive resume` hold it,
  and a resume is not undone by sign-in evidence that is already known.
  The drive never accepts terms, submits a delivery request or resolves an
  identity review. ADR-0009 records the operator decision that permits it.
- **Every acquired artifact now records who produced it.** Each promotion to
  `ready` writes one `artifact.producer` event: `adapter`, `agent`,
  `native_viewer`, `daemon_fetch`, `manual` or `unknown`, with the adapter or
  agent decision when the record names one, and the interventions the attempt
  needed (`open`, `sign_in`, `terms`, `challenge`, `review`, `manual_file`). A
  handoff opened by the paced drive is not an intervention. `papio jobs show`
  prints the record, and `papio stats producers --since <instant>` counts a
  period by producer and as unattended, sign-in only, or intervened. A browser
  download that carries no effect tuple stays `unknown`: the extension does
  not report which adapter clicked, so papio does not guess.
- **`papio jobs redrive` gives a stuck paper a fresh institutional handoff.**
  `papio jobs redrive <job-id> --revision <n>` replaces a parked manual
  download, a parked open-access handoff, or a terms step with no live browser
  claim with a fresh institutional handoff. It keeps the failed action and
  browser history, releases the old browser claim, and allows one request per
  observed browser outcome; the job needs a configured institutional resolver
  and no unresolved browser effect. The redrive starts a fresh browser attempt:
  provider latches from the earlier attempt stay in history but do not veto the
  restored route, so the adapter or article agent runs again. Redrive accepts no
  terms itself; the extension's consent setting decides again when the handoff
  is driven. Use `--revision 0` when the action is already resolved, or to
  reopen an `unavailable` job whose terminal reason is `browser_rejected`.
  Redrive also reopens a `needs_review` job whose only open action asks you to
  remove an adopted file that failed validation and could not be moved to
  `rejected/`, once no file remains in the job's adoption directories; while a
  file remains, or the directory cannot be read, it refuses. New actions of this
  kind name the command.
- **Pause automatic Zotero imports without disabling acquisition or library lookups.**
  Set `zotio.auto_import_paused = true` and restart the daemon. The pause covers
  older jobs and import retries, preserves their saved policies, and appears in
  `papio doctor`. Clearing it resumes eligible pending imports after restart.
- **One credential service for integration secrets.** `papio config credentials`
  sets, binds, inspects, detaches, deletes and migrates typed records in the OS
  credential store. Config contains stable references; explicit environment
  references support headless use. OpenAIRE pairs and complete webhook endpoints
  are atomic records, and each institution keeps its own ILLiad binding.
  Migration verifies stored records and rejects observed concurrent config edits.
  Acquisition, discovery, delivery and doctor share resolved runtime credentials
  without putting secrets back into config. OpenAIRE client pairs now have their
  own quota identity, so a keyless deferral cannot block a newly authenticated
  account. Legacy credentials remain readable.
- **Article-agent downloads can continue after a same-origin page change.** A
  compatible extension can transfer an unused Firefox download reservation to
  a freshly verified document. The daemon retains the original download-directory
  baseline, expiry and permit, and rejects imports from the previous document.
- **Persistent article-agent setup.** `papio config agent set` saves a TypeSafe
  key in the OS credential store and enables Jev for that profile. A hidden
  prompt and `--key-stdin` keep it out of command arguments. `status` reports
  setup without exposing the key; `remove` disables the agent in config without
  deleting a shared record. Restart the daemon after changes.
  `PAPIO_TYPESAFE_API_KEY` still works, but a saved key takes precedence over
  it; after `remove`, also unset the variable if you use it. A locked credential
  store leaves ordinary acquisition available.
- **Firefox can adopt downloads made by the optional article agent.** The daemon
  reserves the attempt before a click, copies the newly observed file from the
  configured download directory, and applies normal PDF and identity checks.
  It leaves the browser's original file intact and never repeats the download.
- **Experimental: *papio* can save a PDF that Firefox on macOS already shows in
  its viewer.** Set `browser.native_viewer_helper` to the absolute (or `~/`)
  path of the separately built native helper, then restart the daemon; empty,
  the default, leaves it off, and the release archives and packages do not
  include the helper. When a paper's manual download waits on a PDF that
  Firefox's own viewer already shows, *papio* asks the helper to save that
  loaded file once, with no new request to the publisher, and the saved file
  must pass the normal PDF and identity checks. The helper needs macOS
  Accessibility permission that you already granted and never asks for it;
  Windows and Linux have no driver yet. If the daemon stops or cannot move the
  saved file into the paper's folder, the next adoption sweep publishes and
  validates that exact file without a second Save, with growing waits between
  tries; after 20 deferred tries, or if the saved file is missing or altered,
  *papio* releases the save's effect permit so other work can continue, keeps
  the file and leaves the manual download action open.
- **Optional daemon-owned agent acquisition.** A TypeSafe key supplied through
  `PAPIO_TYPESAFE_API_KEY` enables bounded Jev decisions for delegated article
  downloads when packaged and generic routes fail, including publishers without
  adapters. Decisions share the existing job and effect permit; cancellation,
  stale sessions and consumed requests cannot authorize a new action. The
  backend interface also accepts local implementations without cloud access.
  Each decision request sends TypeSafe only the job's normalized DOI, the page's
  metadata title (at most 400 characters), a SHA-256 revision of the
  observation, and at most 80 visible controls, each with an opaque id, its role
  (button, link, menu item or tab), a label of at most 240 characters and
  whether it is disabled. The extension removes URLs, paths, e-mail addresses,
  `key=value` pairs and long tokens from the title and labels, and it skips form
  fields and header, navigation, account, profile and sign-in regions; no page
  address, page body, form value or library credential is sent.
- **Adapter repair proposals now preserve the exact verified change.** The
  generator examines later rules as well as the first, edits parsed source
  fields without changing sibling rules, and explains blocked proposals.
  Separate workspaces prevent stale patches from leaking into repeat runs.
  Generated regressions preserve existing fixtures, fail if their capture is
  missing, and use its real origin. CI applies the generated patch and checks
  that the regression fails before it and passes afterward. Proposals prefer the
  interactive PDF control over its surrounding form and a control's class or a
  bounded PDF path segment over page position, avoid numeric article IDs and
  redacted document tokens, and keep ancestor, form and disabled-control guards
  when they repair a leaf class. Selectors built from attributes that contain
  apostrophes use CSS hexadecimal escapes. All complete plans are checked before
  the list of proposals is cut, so a low-ranked working selector is not lost
  behind generic controls. A file labelled as a preview, sample, abstract,
  supplement or full issue stays a diagnostic candidate and cannot produce a
  source patch, and a PDF control cannot stand in for a separate access or
  identity check.
- **Retry a failed resolver route through the paper's DOI.** `papio actions
  retry-publisher <action-id> --revision <revision>` replaces a manual download
  left by a wrong page, adapter drift or a missing adapter with one publisher
  attempt through the paper's DOI. The retry starts a fresh browser attempt that
  keeps the original action, its failure evidence and the old route's provider
  latch, and a superseded route keeps any unresolved effect under its original
  safety domain. The retry waits while the job has any other open action
  (including its own terms step), a live browser claim, or an active challenge
  or rate-limit suppression; while a sign-in, MFA or captcha gate is open on the
  job's institution, or a platform-wide gate is open; and while any browser
  effect is unresolved. An institution's terms gate therefore blocks only a
  paper that has its own open terms action, and publisher access still depends
  on the browser session's entitlement. If the DOI attempt parks again and the
  connected extension has a newer version of the adapter that refused the
  original route, the same command re-offers the original institutional route
  once per job, in a fresh attempt whose earlier latches stay in history but do
  not veto it.
- **A paper that failed to file into a non-Zotero library can now be found and
  filed again.** When `[hooks] on_ready` fails once (papis mid-upgrade, target
  volume unmounted, a timeout), the PDF was acquired but never filed, and no
  command showed it. `papio jobs unfiled` lists ready papers whose newest hook
  run is missing or failed, `papio jobs refile <id>` runs the hook again for one
  paper and records the result, and `papio doctor` warns with the count. A `jobs
  unfiled` table that stops at the limit ends with `truncated: showing N jobs;
  use --limit (max 500)`. Hooks are still never retried on their own.
- **`papio doctor` warns about papers that were fetched but never reached
  Zotero.** A paper with automatic import turned on could have a validated PDF
  but no successful import, and it looked finished. The new
  `undelivered_zotero_imports` check counts these papers, gives the age of the
  oldest, and tells you to inspect their `zotio.auto_import` events.
- **`papio zotio import-backfill` files papers that were fetched but never
  reached Zotero.** A paper with a validated PDF whose import never ran, or
  never succeeded, stayed out of your library, and no command could deliver
  these papers together. The command takes them oldest first, one page at a time
  with `--limit` (at most 50) and `--cursor`, and only reports what it would do
  until you pass `--apply`. Papers you submitted without automatic import are
  included only with `--include-not-requested`, and the report separates papers
  newly filed from papers already in your library and from failures.
- **`papio doctor` warns when Zotero refuses to store a paper's file.** The new
  `zotero_file_storage_refused` check counts the Zotero imports of the last
  seven days that failed on file storage, gives the dates first and last seen,
  and ignores a refusal for a paper that *papio* has since filed. It separates a
  full Zotero storage plan, reported with Zotero's own figures and the date they
  were measured, from an upload that had no route because the Zotero item
  already exists and your library keeps its files on your own file store. When
  both causes appear, the more recent one and its advice come first, and an
  older full-plan reading stays in the past tense. The PDF stays in *papio*'s
  own store, and the advice names the way out for each cause: free space in
  Zotero, set `attachment_mode = "linked-file"`, or upgrade *zotio* to a release
  whose `attachments add` accepts `--via connector` and then run `papio zotio
  import-backfill --include-not-requested --apply`.
- **Desktop notifications on Linux and Windows.** *papio* pinged only through
  macOS `osascript`; Linux and Windows builds had no desktop leg at all, so a
  paper waiting on a sign-in pass went unnoticed until you looked. Linux now
  uses `notify-send` (or `gdbus` when that is absent) and Windows a built-in
  PowerShell toast, chosen by probe at startup so `papio doctor` and the
  notification ledger report the real mechanism.
- **Papers that were unavailable are checked again for everyone.** A paywalled
  paper often becomes free later: embargoes lift, authors post copies, holdings
  change. Only Zotero users with a backfill watch got that re-check. The daemon
  now re-submits stale `unavailable` jobs through the ordinary path after
  `[zotio] unavailable_recheck_days`, one bounded batch per pass, skipping
  reasons a wait cannot change and work that is already live or delivered. The
  old job records the re-check so `papio jobs get` shows what happened.
- **Retraction warnings for your whole library, not only the papers *papio*
  fetched.** Set `[retraction] scope = "library"` and the daily sweep also
  covers every DOI in your configured bibliographic exports and your Zotero
  library. The sweep now reads the Retraction Watch dataset Crossref publishes
  in one cached request per day instead of one request per paper, so cost no
  longer grows with your library. The default stays `"acquired"`, so an upgrade
  does not widen anything on its own.
- **`papio doctor` reports the retraction sweep.** A new `retraction` row passes
  with the age of the last successful sweep and the number of current notices.
  It warns when the latest Retraction Watch fetch failed, when the last success
  is more than 48 hours old, or when the saved sweep status cannot be read. It
  is skipped when `sources.retraction_watch` is disabled or no sweep has
  completed yet.
- **`papio adapter repair` now proposes a patch instead of instructions.** It
  scores candidate selectors from the captured page by stability, verifies each
  one against the production classifier, and writes the fixture at its final
  path plus two review-only patches: the adapter test case and the adapter
  source change with its revision bump. When no candidate verifies it says so
  and writes no source patch — never a guessed selector.
- **`papio adapter repair --recovery-job <job-id>` can use a paper that was
  recovered after its adapter failed.** Name a job that recorded the capture,
  then failed on that adapter version (or was handed to the article agent), and
  then reached `ready` through validation with a PDF delivered through the
  browser and a DOI. This works only for article repairs. The link counts as
  independent evidence, so it can unlock the source patch, and the generated
  regression uses the recovered DOI and must refuse a different DOI; a link that
  rests only on timing labels the regression but does not unlock the patch. Each
  repair folder now also holds `repair.json`, and a linked proposal adds
  `canary.md`, a check to run with the article agent turned off.
- **arXiv is a discovery backend.** `papio search --source arxiv` and standing
  watches now query arXiv directly with no key and no credit spend, so weekly
  "what is new" watches in CS, ML, physics, and math are not gated on aggregator
  indexing lag. Citation-snowball searches decline on this backend rather than
  answering partially.
- **Losing the tab *papio* opened for you is no longer silent.** When you close
  the tab *papio* used to reach a paper through your institution while it is
  still driving that tab, it releases the route and the paper waits for another
  attempt. Nothing told you: the toolbar count reports sign-in walls and
  decisions, and the popup reads neither the access claim nor the journal row
  that recorded the loss — so a paper you watched disappear left no visible
  trace. That release now writes a durable Activity entry, which the popup's
  "While you were away" card counts and `papio activity` prints. The entry names
  the paper whose tab closed, which is not always the paper that noticed. It is
  deliberately not a desktop notification: *papio* getting on with the work does
  not interrupt you. A tab you close after the paper is already delivered stays
  silent, because that route finished rather than being lost — as does a tab
  *papio* had already stopped driving, which it had already re-queued.
- **A paper bundled with a supplementary file is now delivered instead of
  parked.** Publishers routinely attach one extra file to a PDF, and *papio*
  will not file a document that carries anything runnable, so those papers
  stopped and asked you — for a decision you had no way to give: approving that
  kind of review was refused outright, and rejecting it asked you to fetch by
  hand a paper *papio* was already holding. *papio* now removes the attachments,
  re-checks the result from scratch, and files it when it comes back clean. An
  encrypted paper and one carrying a script are left alone deliberately: those
  are different repairs with different risks. Your library keeps the paper, not
  the extra file, and *papio* records both fingerprints - the publisher's file
  and the one it filed - so the difference is never silent.
- **`papio doctor` now tells you when the same paper is queued twice.** Two live
  jobs for one paper each open their own row, so the popup asks you to fetch the
  same paper twice with nothing marking the pair — and until now *papio* recorded
  that it had noticed into an event no surface ever read. It happens because
  duplicate detection runs when you submit, and a batch picked off a page has
  only titles at that point; the shared identifier turns up later, when a
  resolver fills the metadata in. *papio* deliberately does not merge the two
  then — something may already be holding the other job's id, and a wrong merge
  breaks it, while a duplicate costs one wasted fetch — so doctor names the
  condition and leaves the choice to you, exactly as it does for a paper holding
  two captures. It falls silent once one of the two has finished, since a fresh
  attempt after a completed one is not a duplicate.
- **`papio doctor` now tells you which paper is holding your library's sign-in
  slot.** *papio* allows one institutional sign-in at a time, and a paper that
  reaches the sign-in page with its tab still open holds that slot with no
  deadline — deliberately, because a real person needs however long they need.
  Nothing surfaced it, so every other paper at that library waited with no
  explanation anywhere. Doctor now names the holder and counts the papers behind
  it, and tells you the two ways out: finish the sign-in in that tab, which
  shares the session with everything waiting, or close the tab, which releases
  the slot. *papio* will not break the tie on a timer. A paper whose human is
  genuinely working through an identity provider looks exactly like a stranded
  one while it is happening, and cutting a live sign-in is the half of this that
  cannot be undone. The count of waiting papers leaves out open-access papers,
  which do not need that sign-in, and with more than one institution configured
  it says those papers are *also* waiting for an institutional sign-in rather
  than claiming they wait behind this one. Doctor stays quiet once the sign-in
  has landed and is being shared, and for a slot that already carries a
  deadline.
- **`papio browser reload` reloads a development extension without a mouse
  click.** Working on the extension used to end with a click on Chrome's
  **Reload** button, which cannot be scripted and which reports success even
  when it does nothing. The new command sends one `dev_reload` command down the
  connection the browser already holds, then waits for the extension to come
  back and reports the new session id — the only proof that the new bundle is
  the one running. It refuses to guess: a browser that does not reconnect
  inside `--timeout` is reported as a failure, and the extension itself
  declines the command unless the browser reports it as an unpacked
  development load, so a store-installed *papio* is never restarted. `make
  dev-deploy` now runs it for you. A reload is also safe when you run two
  browsers with *papio* installed: restarting the extension frees the browser
  slot for a moment, and a second browser sitting idle used to take it, which
  left the browser you were working in unable to reach *papio* and pointed
  every later reload at the wrong browser. The slot is now held for the
  browser that is coming back, and the command reports a failure — naming
  `papio browser use` — if another browser takes it anyway.
- **The inbox now answers "which paper is this?" itself, instead of telling you
  to go and type an identifier.** A captured PDF that *papio* could not file on
  its own parks in the inbox, and until now the button on that row printed a
  terminal command for you to run — it made no request of the daemon at all.
  It now asks the daemon to score every paper you are waiting for against the
  bytes it actually captured, and shows them ranked, best first, each with the
  evidence that put it there: the title matched, the authors matched, the year
  matched, the identifier was printed on page one. One click on the right one
  files it.

  Nothing here can misfile a paper, because you are the one choosing — which is
  why the ranking is allowed to use weaker signals than automatic filing is. Two
  things it still refuses to do. It will not accept your pick if the document's
  own front matter names a different paper: the bytes get the last word on what
  they are, you get the last word on which pending paper you meant. And when the
  file states its own identity in its embedded metadata but no pending paper
  matches, it now shows you that identifier, with the exact command to file it —
  previously *papio* read that value, discarded it, and asked you to find it
  yourself.

  Same thing from the terminal: `papio grabs suggest <capture>` ranks the
  candidates and prints the `papio grabs confirm` line for the top one. Older
  daemons keep the previous guidance text, since the browser asks first whether
  the daemon can answer.

  A choice you make in the browser's "Which paper is this?" is checked against
  the page as the browser reports it, and it lapses on a page load or reload,
  with age, when its tab is replaced, or when its delivery finishes. A choice
  that survives a browser restart is re-checked against the live tab. Only the
  address without its query is kept, so a signed download link is never
  stored. On Firefox, where *papio* cannot follow a viewer's own Download
  button, the picker does not promise to file something it cannot see.
- **A captured PDF that names itself in its own file metadata can now be
  recognised, without reading a word of the page.** Publishers stamp a paper's
  DOI into the PDF's invisible metadata during production (the PRISM, CrossMark,
  `pdfx` and Dublin Core fields), and *papio* never looked. Reading it answers a
  question the page text cannot: a reference list can never reach a file's
  metadata packet, so a DOI found there is the document stating its own identity
  rather than text that might be a citation of someone else's work — the
  ambiguity that made automatic filing unsafe. Most of the captures it
  recognises are papers where the printed page never shows the identifier at
  all. It is deliberately narrow: only fields whose defined meaning is "this
  document's identifier" are read, never free text like the Subject or Title,
  which anyone can fill with anything. It corroborates and never decides alone —
  title, authors and year must still agree, which is what keeps a
  supplementary-materials file, whose metadata carries its parent article's DOI,
  from being filed as the article. It also lets *papio* file more captures on
  its own — see the next entry.
- **A captured PDF with no DOI on its first page can now be filed automatically,
  against the one paper you are waiting for that it matches.** You send a PDF;
  if exactly one pending paper agrees on title, authors, year and identifier,
  *papio* files it and says so, instead of parking it and asking you which paper
  it was. Captures that do not meet that bar still park and ask, as before. A
  *mention* of a paper's identifier does not count as the document identifying
  itself, so an erratum or a journal expansion printing "Extended from DOI …" is
  not filed as the paper it merely discusses. The match uses the complete title
  and the document's own byline: a numeric title marker cannot hide a different
  subtitle, and an earlier title prefix cannot supply author evidence for a
  later exact title. In testing that replayed a real library against pending
  lists of 2 to 25 papers, it made no wrong filing, and the size of the pending
  list did not matter. One document family is still known to defeat it — an
  editorial note or commentary that reprints the discussed paper's title,
  authors, year and DOI with no correction word anywhere. The two real forms of
  it (an Oxford Academic "Editor's Note", an eNeuro "See related article") are
  recognised and park; an unlabelled one would still be filed wrongly. The claim
  that owns a capture is recorded before any bytes reach the paper's folder, and
  only the validated quarantine copy is staged, so a crash cannot leave a file
  that a later sweep adopts for a paper that never claimed it. A paper cancelled
  between the claim and ingestion keeps its bytes recoverable.
- **`papio grabs binds` shows you what *papio* filed on its own.** An automatic
  filing cannot currently be undone — `papio grabs identify` binds a parked
  capture, but nothing reverses a bind — so the decision record is the only
  recourse, and until now it was written to the database and never shown. The
  command lists automatic filings newest first with the rule version, how many
  pending papers were considered, and the evidence the winning match was made
  on. Worth a glance after a batch of captures.
- **A daily credit ceiling for OpenAlex.** *papio* now charges each OpenAlex
  request against the day's allowance before it sends the request. Both OpenAlex
  accounts, with and without an API key, share the allowance. *papio* stops for
  the day when it has used `daily_credit_fraction` (default `0.5`) of the daily
  limit that OpenAlex reports, and work that reaches the ceiling waits until
  `00:00 UTC`. Under `[sources.openalex]`, `daily_credit_limit` sets an absolute
  maximum (`0`, the default, means no absolute cap), and `daily_credit_fraction
  = 0` turns the ceiling off. Until OpenAlex reports its limit for the day, a
  small fixed allowance applies. If the prepaid balance that OpenAlex reports
  goes down, *papio* closes the source, and a running daemon does not reopen it
  at midnight.
- **OpenAlex is paced by its own daily-budget headers.** Every OpenAlex response
  says how much of the day's allowance is left, and *papio* now stops using a
  credential when 5% of its allowance remains, until the reset time OpenAlex
  gives. The stop holds for every OpenAlex caller — resolving, metadata
  enrichment, discovery, watch digests and the MCP server — and it binds from
  the moment the headers are read, even if *papio* cannot save it to disk. It is
  checked again when each request is committed, so a request that was waiting
  its turn cannot pass it, and a paper stopped this way waits for the reset
  instead of retrying on the ordinary schedule. When a configured API key
  reaches its stop, resolving and enrichment continue on OpenAlex's keyless
  tier, which has its own allowance and the same 5% stop; discovery does not
  fall back and waits for the reset. Both credentials still count against the
  one daily credit ceiling. A `429` without these figures is still an ordinary
  retry, figures that do not add up are ignored, and an ordinary retry wait or
  local throttle never switches credentials.
- **`papio doctor` shows how much of today's OpenAlex allowance is gone.** It
  names today's committed OpenAlex credits, the ceiling, where the ceiling came
  from — the provider's reported limit, or the conservative cap used before the
  day's first response — and the credentials that share it, while the allowance
  is still healthy. A spent allowance warns and says work resumes at `00:00
  UTC`.
- **OpenAIRE can now be authenticated in a way that survives the night, raising
  its ceiling 120×.** Authenticating with OpenAIRE lifts you from 60 requests an
  hour to 7,200, and *papio* accepted a token for this before — but the only
  credential it accepted was OpenAIRE's *personal access token*, which OpenAIRE
  expires **one hour** after issuing it. Pasted into `api_key` it authenticated
  through a test and then refused every request for the rest of the day, and
  read as an OpenAIRE outage rather than as an expiry. *papio* now accepts a
  registered service's `client_id` and `client_secret` — credentials that do not
  expire — and exchanges them for short-lived tokens itself, refreshing before
  each one runs out. Setting them also raises the request pacing to match the
  higher ceiling, because a credential that authenticated and changed nothing
  observable is two settings for one intention; an explicit `rate_per_sec` still
  wins. Pasting a personal token into `api_key` keeps working for manual checks
  and deliberately does *not* raise pacing, since pacing to 7,200 an hour on a
  credential that can vanish mid-hour would leave *papio* running at 120× what
  an unauthenticated caller is allowed. `papio doctor` names which credential
  you are on and warns when it is the expiring one. Setup is on the
  configuration reference page.
- **One paper can no longer use up a day's OpenAlex allowance while your other
  papers wait.** While other work is waiting, no single paper may use more than
  a quarter of the daily credit allowance, and the rest stays available to the
  queue. A paper that reaches its quarter waits for the next UTC day and then
  continues; it is never abandoned. When nothing else is waiting, one paper may
  use the whole allowance, because an unused allowance cannot be saved. The rule
  counts credits spent while others wait, not attempts, because papers that do
  arrive can need many more attempts than usual. A paper stopped this way says
  so, separately from the whole day's allowance being gone.
- **Institutional sign-ins are now brokered so only one tab per institution ever
  asks for one.** The daemon now arbitrates every request for an autonomous
  sign-in tab in one transaction over a durable authentication-entry lease
  (migration `0042`, extending the already-shipped `authentication_entry_leases`
  table and adding an append-only `claim_observation_journal`): a job whose
  institution has no live sign-in gets a fresh grant, a job whose institution is
  already mid-sign-in is told to wait or focus the tab that has it, and every
  paper waiting behind that one login resumes automatically, on its own freshly
  revalidated route, the moment it succeeds. The lease is renewed only by the
  human-paced wall/login/MFA/challenge events the extension reports — never by a
  worker-local timer — and a duplicated or out-of-order report can never mutate
  it twice, since every observation is journaled against a strictly increasing
  sequence number. The daemon advertises this as
  `institutional_authentication_claim_v1`, the 32nd and last protocol feature
  the current fail-closed negotiation cap allows — any protocol feature added
  after this one has to retire or fold in an existing one first. A sign-in
  return that arrives after the same paper's lease already settled is recorded
  as a late return: *papio* keeps the evidence and journals the observation but
  leaves the lease unchanged. A late return never reserves, renews or promotes a
  lease, and a return for another paper's settled lease is still rejected. While
  the login gate stays open, a repeated sign-in-pending report reuses the gate's
  open occurrence, so the extension's observations for that sign-in stay
  current; a sign-out after a resolve still starts a new occurrence. A sign-in
  reservation whose deadline has passed cannot have a tab bound to it, and
  retiring an expired claim also frees the institution slot that claim held. An
  extension that has not yet negotiated it keeps behaving exactly as before:
  tabless parking until you click Open.
- **Automatic institutional offers no longer race every waiting paper into
  the same poll.** Offering a candidate for automatic sign-in is now paced
  by its authentication claim: an institution with a sign-in still
  unresolved admits exactly one candidate per poll instead of one attempt
  per paper at once, and an institution whose sign-in already has a live
  owner never generates a second, redundant candidate for it while that
  owner stands. Automatic admissions continue to share the existing
  four-per-poll transport budget with every other kind of offer.
- **The daemon now authorizes, one use at a time, every tab the extension
  automatically closes.** Migrations `0041`, `0044`, `0047` and `0048` add a
  `close_authorizations` table behind the new `surface_close_v1` feature: the
  extension closes a daemon-bound tab only with a one-use token that the daemon
  issues for that exact binding and reason, and only one token per binding is
  live at a time. The reasons are an idle placeholder (`scaffold_idle`), a
  settled materialization (`materialization_settled`), an abandoned claim
  (`claim_abandoned`), a paper that is over or has no open browser handoff
  (`job_inactive`), a handoff the browser has parked (`handoff_parked`), and a
  duplicate or superseded tab for the same paper (`surface_superseded`); the
  daemon refuses while that claim's own provider effect is in flight. A tab with
  no claim gets the answer `unclaimed`, because *papio* has no stake in it, and
  the extension's own guards decide. After the close, the extension reports
  `owner_closed`, which consumes the token and frees the institution's sign-in
  slot. The daemon derives terminal browser cancellations from durable claims,
  so this works after either side restarts. The extension never closes a pinned
  tab, a tab you took over or moved out of *papio*'s window or group, a tab from
  before a browser restart, or the tab you are looking at. An extension that has
  not yet negotiated `surface_close_v1` keeps its existing closing behavior
  unchanged.

### Changed
- **Approving a held-back PDF now means "check it again", and is allowed.**
  *papio* used to refuse approval for a paper held back over active content,
  because filing it as it was would have broken its own rule. Now that *papio*
  can repair the common case, approval re-runs the whole check on the same file:
  a paper it can clean reaches your library, and one it cannot is held back
  again rather than filed. The rule itself is unchanged - nothing you approve
  can put a runnable document in your library.
- **Retraction notices now name the affected paper.** The inbox used the same
  `Library update notice` title for every retraction and correction, so a list
  of five notices spent almost half its visible text repeating a label that did
  not identify any of them. It now uses the paper title already held by the
  library record, or the title returned by the existing Crossref lookup when
  the local record has none. No additional request is made; old cached notices
  without a title keep the generic fallback until refreshed.
- **`papio doctor` now reports a data directory that other users can read, and
  does not change it.** The `data_dir` check set the directory to mode `0700`
  before it tested it, so the check always passed and overrode the mode you
  chose. On Unix it now fails with `data directory is not private`, gives `chmod
  0700 <dir>` as the fix, and leaves the mode as it is. A directory that is not
  writable, is not a directory, is not set, or cannot be read now gets its own
  message. A missing directory is still created with mode `0700`.
- **`papio acquire --batch` refuses a batch that lists the same work twice.**
  Two entries with the same identifiers, or with the same title, authors and
  year when they have no identifiers, got the same request id. The earlier entry
  never got its job id, so the batch manifest and report were wrong. The batch
  now fails before anything is submitted, with `batch contains duplicate work
  "…" at positions N and M`, and the MCP batch tool applies the same rule.
- **Metadata sources now open one connection per request.** Metadata requests
  use HTTP/1.1 and do not reuse connections, so a failed request cannot be sent
  again silently and charged twice. To allow reuse, set `allow_keep_alives =
  true` under `[sources.<name>]`. The client that several sources share reuses
  connections only when all of them allow it. When *papio* looks up a paper to
  acquire it, the OpenAlex key now goes in an `Authorization: Bearer` header and
  not in the query string.
- **New Zotero items now hand their PDF to Zotero desktop, so the file goes
  where Zotero is set to store it.** *papio* created every new item with
  *zotio*'s `--via web` route. That route uploads the file to Zotero's own cloud
  storage even when Zotero keeps your files on a WebDAV server, and a full
  storage plan then stopped every new filing. *papio* now asks for `--via auto`,
  which uses Zotero desktop when it is running and the web API only when it is
  not. The route is now part of each cached import plan, so a paper planned
  before the upgrade is planned again with the new route. *papio* waits at least
  10 seconds between Zotero imports, automatic or from `papio zotio
  import-backfill --apply`, so Zotero desktop is not overloaded. One maintenance
  pass tries at most three imports, and the rest wait for the next pass. The
  first import in a run starts at once.
- **A PDF for an existing Zotero item now goes through Zotero desktop, with a
  readable name.** In `stored` mode, *papio* now asks *zotio* to attach the file
  with `--via connector`, so Zotero desktop stores it wherever you set Zotero to
  keep files, WebDAV included. Before, the file went through Zotero's web API,
  which used Zotero's own storage plan, and a library that keeps files on WebDAV
  refused it. Zotero desktop must be running, and you need a *zotio* release
  whose `attachments add` accepts `--via connector`. An older *zotio* ignores
  the flag and uses the web API as before; for a refused paper, the
  `zotero_file_storage_refused` check in `papio doctor` tells you to upgrade
  *zotio* and run `papio zotio import-backfill --include-not-requested --apply`.
  The attachment is now named after the paper's title, or its identifier when it
  has no title, not after a 64-character content hash.
- **Adding a page's paper from the browser now says whether *papio* already had
  it.** The reply to a browser add now carries an `outcome` of `submitted`,
  `already_queued` or `already_validated`. Before it adds anything, the daemon
  checks the paper's DOI against live, ready and imported papers, so the popup
  no longer says *Added to papio* for a paper that *papio* already has.
- **The daemon log now records a malformed request from a local client.** A
  command or native host that sent a partial or malformed request was
  disconnected with no trace. The daemon now logs it as `ipc: decode request:
  …`. A connection that closes without sending anything, such as the check each
  command makes to see whether the daemon is running, is not logged.
- **Searching OpenAlex by title for another copy of a paywalled paper is now off
  by default.** Each search costs 10 credits, and it used far more credits for
  each paper it delivered than it saved. Turning it off does not change how
  papers are accepted or ranked, and the free lookups through publisher-declared
  version links still run. If you turn it back on, it runs only when the paper's
  other sources have no ordinary retry left and only when *papio* knows an
  author to match the results against. It is not asked again for the same title,
  year, authors and DOI unless enrichment changes them; a search that fails in
  transit can still be retried. Each search makes one request, and it uses the
  record that the same pass already read, or your own citation details, instead
  of fetching the record again. To turn it back on:

  ```toml
  [sources.openalex]
  sibling_title_search = true
  ```

### Fixed
- **Diagnostic captures and support reports no longer carry your IP address.**
  Elsevier's refusal page prints the reader's IP address, and every capture of
  it kept that address. The capture store now masks IPv4 and IPv6 addresses
  before it writes a capture, even one from an older extension, and records
  `sanitizer_version` `2`. `papio adapter diagnose` masks them too. DOIs,
  dates, clock times and software versions such as `Chrome/153.0.0.0` are
  kept. `papio adapter repair` accepts only version 2 captures, so capture a
  page again to repair from it; version 1 captures may still hold an address.
- **A provider refusal page no longer latches adapter drift or parks the
  paper.** A compatible extension now reports a provider's block or rate-limit
  page as `rate_limited` instead of as a broken adapter. The daemon then
  releases the drive's binding and records a `browser.provider_cooldown` event
  with the refusing host and the time until which *papio* should not drive that
  host. As before, it closes the handoff and puts the job in `retry_wait` for
  `browser.action_expiry_seconds`, with no drift latch and no manual download,
  because nothing on a refusal page is for a person to do.
- **A waiting PDF grab no longer makes your browser reconnect every few
  seconds.** When every open action belonged to a known family, the inbox
  count listed the grab's family first even though it ranks last. The
  browser protocol rejected that order, and the native host restarted each
  time the count was read. The families are now listed in rank order.
- **An empty browser `job_reject` no longer ends a job as `unavailable /
  browser_rejected`.** The frame carries no reason, and the extension sent it
  only when it had lost its own offer URL. The job now stays `awaiting_human`
  with its handoff open, and a later poll offers it again. `papio jobs redrive
  <job-id> --revision 0` reopens a job that was already ended this way.
- **A paper submitted by PMID alone now gains its DOI before routing.** The job
  takes its DOI, title, authors and year from the PMID's own Europe PMC record,
  so resolvers can use the DOI and the institutional OpenURL carries
  `rft_id=info:doi/...` instead of only `info:pmid/...`. The Europe PMC resolver
  read that record on every pass but kept nothing unless it was open access. The
  PMID stays the submitted anchor. Handoff repair returns an existing PMID-only
  institutional park to resolving once so it gets the DOI; a record with no DOI
  parks again as before.
- **A browser drive that ends without a provider outcome no longer holds its
  safety domain for 30 minutes.** If no outcome follows a generic drive result
  that has no daemon successor (for example `html`) within 30 seconds, the
  daemon ends the drive itself: it retires the claim, resolves the handoff, and
  opens a manual download task. Sibling papers queued behind that provider
  safety domain are then offered.
- **Unencrypted PDFs with an Identity `Crypt` filter are no longer rejected.**
  When pdfcpu cannot parse such a file, *papio* now accepts it only if Poppler's
  `pdfinfo` and `pdfdetach` confirm its page count, that it is not encrypted,
  and that it has no JavaScript and no embedded files. If any check fails, the
  PDF stays rejected.
- **`papio jobs diagnose` names the job an opened handoff is waiting behind.**
  When another job holds a live claim in the same provider safety domain, or
  holds the institution's sign-in slot, `next` reads `waiting: institution
  sign-in slot / live claim held by <job> (phase <p>, since <t>)` instead of
  "open the handoff". The JSON shape is unchanged.
- **A truncated job or action list now says so.** `papio jobs list` and `papio
  actions list` tables that stop at the limit now end with `truncated: showing N
  jobs; use --limit (max 500)` (`actions` or `open actions` for `actions list`),
  so a clipped table no longer reads as the whole queue.
- **American Journal of Psychiatry, ACS and Science papers now open at the
  publisher first.** For DOI prefixes `10.1176`, `10.1021` and `10.1126`, the
  first browser handoff is the paper's DOI, because a packaged adapter
  (`psychiatryonline`, `acs`, `science`) drives that publisher page. The
  institution's resolver sent these papers to the journal homepage or archive
  page, not the article. If the DOI route fails, *papio* offers the
  institution's resolver as the next route in a new attempt.
- **An open-access browser route that downloads HTML now falls back to the
  institution's resolver.** Before, *papio* kept the open-access handoff, so
  each new browser claim opened the same URL again. For example, a Wiley
  `pdfdirect` link without an entitlement kept returning HTML. The fallback
  happens once, releases the old browser claim, and uses the institution's
  safety domain for the resolver route.
- **A terms page after sign-in no longer holds the institution's sign-in slot.**
  When a provider asks for terms acceptance after the sign-in returned, *papio*
  parks that paper on its own tab and releases the slot, so sibling papers
  proceed while you accept the terms. A paper parked on its own terms step is
  not offered again automatically; `papio actions open` still drives it. A terms
  outcome before the sign-in returns, or while an institutional effect is in
  flight, keeps the slot.
- **Explicitly opened handoffs survive a daemon restart.** The bridge restores
  pending focus from job events and does not repeat an open after an offer or
  provider outcome. An open made without a browser can start on the next sync.
- **Browser sessions show Chrome or Firefox before a reload.** `papio browser
  sessions` names the reported browser family. `papio browser reload --session
  <id-prefix>` selects a connected session, including a pending one, instead
  of reloading whichever browser holds the bridge. Without the flag, reload
  keeps its previous holder-only behavior.
- **A retired browser claim no longer holds an institution's sign-in slot.**
  A holder-generation fence releases the claim's bound entry lease in the
  same transaction. An unsettled institutional effect still keeps the slot.
  The 30-minute sweep remains a backstop for older stranded leases, and it
  now frees a returned sign-in whose claim is already abandoned or settled on
  the next poll instead of after the grace; a sign-in still in progress keeps
  the grace.
- **Old browser sessions cannot reclaim work after a holder sweep.** The
  daemon records each swept holder generation before offering work to its
  replacement. A late claim from the old session now receives a stale response
  instead of blocking the institution's next job.
- **An explicit `papio actions open` takes the next browser slot before session-live reoffers.**
  The daemon offers older explicit opens first and waits one poll before releasing
  reoffers when it first sends a focused institutional candidate.
- **Dismissing one human action leaves a job waiting on its other open actions.**
  Closing an obsolete browser handoff no longer cancels a job that still needs
  a terms decision. Dismissing its last open action can still cancel a parked job.
- **Late browser receipts recognize PDFs that Papio sanitized before storing.**
  The daemon verifies the downloaded source, accepted candidate, recorded
  sanitization and stored artifact before pairing delivery details. Removing
  embedded content no longer causes a successfully acquired paper to be reported
  as an unmatched download. Unrelated files and missing producer evidence remain
  refused.
- **Late browser receipts no longer report a failed adoption for an acquired PDF.**
  If a directory sweep validates the file before its producer receipt arrives,
  Papio records the missing delivery provenance separately. It keeps the accepted
  PDF and refuses to invent the missing producer binding from a late receipt.
- **Windows can create its incident key on first use.** Publication still flushes
  complete key bytes and never overwrites a competing key. It preserves Windows
  ACLs and read-only attributes without requiring an unsupported directory flush.
- **Windows native-host installation now works while a browser is connected.**
  The installer preserves running host copies, publishes the replacement, and
  restores the prior image if publication fails. Existing connections keep
  running; new connections use the installed version.
- **Provider diagnostics can now be saved on Windows.** Capture filenames avoid
  timestamp colons, which Windows rejects. Existing captures remain readable;
  fixture contents and JSON timestamps are unchanged.
- **Open actions remain usable after conservative acquisition finds no file.**
  `papio actions open` now finds advisory actions on unavailable jobs and opens
  their canonical work links. Selecting an action also works beyond the bulk
  job-list limit. Opening an advisory leaves the terminal job unchanged.
- **Windows health checks use Windows permissions and a real worker probe.**
  `papio doctor` checks file ownership and access-control lists instead of Unix
  permission bits, and launches the PDF worker to verify its response. Private
  Windows installations no longer fail these checks or receive `chmod` advice.
- **A late browser completion no longer reports adoption as deferred after the
  same PDF is already ready.** The daemon checks the landed bytes, stored artifact
  and accepted browser candidate before acknowledging the duplicate. Mismatched
  files and unsupported producer claims remain refused.
- **HTML downloads no longer consume a waiting paper's browser handoff during
  folder scans.** The daemon checks the file header before starting adoption,
  leaving the original action available for a later PDF. The check shares the
  existing filesystem timeout and still requires full PDF and identity validation.
- **A manual-download task cannot restart an automated sign-in attempt.**
  Late browser requests are refused once the provider handoff has become a
  manual-download task, preventing a spent attempt from reserving the
  institution's sign-in slot again. The operator's Open action remains available.
- A provider route that ends in a manual-download task no longer reuses the
  route's earlier Open request. Explicitly opening that task also withholds
  delegated adapter clicks, so papio does not race the manual download it
  just requested.
- Browser effect reconciliation now accepts a delayed reply after intervening polls. A busy connection no longer keeps replacing the request before the browser can answer it; uncertain effects still require independent resolution.
- When a provider opens a PDF that needs the browser viewer’s Download button, papio now records a manual task and releases the institution’s sign-in slot. Other queued papers can proceed; the waiting paper remains unfinished.
- **A cited author no longer makes a short-title paper pass identity checks.**
  A surname followed by a citation year before the printed title does not
  establish authorship. If that is the only author evidence, papio holds the
  PDF for review.
- **Open-access article handoffs survive resolver retries.** The daemon
  preserves a skipped article page as a browser route before retrying an
  unrelated resolver. It no longer loses that route when the candidate
  leaves the pending queue.
- **Job diagnosis directs document-delivery actions to their requests.**
  `papio jobs diagnose` reports `document_delivery` and the scoped
  `papio delivery get` command instead of `unknown` or an earlier browser
  failure. It does not recommend opening or retrying a browser handoff.
- **Manual downloads open on current browser sessions.** The daemon no
  longer sends an explicit manual-download open into the automatic
  materialization path, which has no candidate for that action. It sends
  the institution's route and the focus request without automatic download
  authority. Existing safety latches still apply.
- **Status shows the paper and the next action.** It uses known titles rather
  than preferring identifiers, groups matching advice by institution profile
  and recorded provider, and keeps each job ID visible. Browser commands
  select one action instead of opening the whole queue. Status names actions
  whose automatic offers or reminders have stopped, without changing either
  limit. Document-delivery actions point to the request and its blockers;
  an explicit browser rejection no longer appears as missing institutional
  access. `acquire --wait` also shows the scoped action command.
- **Stopping *papio* while a filing hook is still running no longer loses the
  hook's result.** Shutdown waited five seconds for hooks, then closed the
  database whether or not they had finished, while a hook may be allowed ten
  minutes; a slow papis or folder hook then recorded its outcome against a
  closed database, or not at all. *papio* now cancels a hook that outlives the
  wait, records it as cancelled so `papio jobs unfiled` lists the paper for a
  refile, and only then closes the database.
- **Dismissing a held PDF from the command line now works.** The inbox offers
  "dismiss" on a PDF *papio* is holding without an identity, and the browser
  window honoured it, but the same request over the command line or an agent
  connection answered "conflict" and left the file in place. One dismissal path
  now serves every surface, so the file and its record go away whichever
  surface you use.
- **A cancelled paper no longer leaves a document-delivery request looking
  live.** Cancelling a paper, or dismissing its request, from the browser window
  skipped the reconciliation the command line already did, so a delivery request
  *papio* had stopped watching stayed recorded as submitted forever.
- **A cancel *papio* could not finish is no longer silent.** Cancelling a paper,
  or dismissing its request, records the cancellation first and then releases the
  institutional request it was waiting on. If whatever asked for the cancel went
  away in between, the release was skipped and nothing reported it, so the
  request stayed recorded as live while nothing was watching it. The release now
  runs to completion regardless of who walked away, and if it still fails, the
  message tells you the cancel itself did go through — so you retry the release
  rather than the cancel.
- **Confirming that a request never arrived can no longer leave the paper with
  nothing to answer.** Telling *papio* a document-delivery request is absent
  closes the question it asked you, and only then records the fresh request. If
  recording the fresh request failed, the question was already gone: the paper
  sat waiting with no prompt on any surface, and nothing short of reading the
  database explained why. The question now comes back when that happens.
- **Opening one paper no longer reads every paper's decisions.** `papio jobs
  show` and the status card gathered the whole decision table in order to display
  a single paper's, so both got slower as your library grew. They now read only
  the paper you asked for.
- **An interrupted text check is no longer reported as a failed one.** Shutting
  *papio* down while it was reading a PDF's text recorded the interruption as a
  document that needs your review, and left the attempt open. The same applies
  to an interrupted sibling-version lookup. Both now record the interruption for
  what it is.
- **`papio jobs failures` no longer leaks a database connection** when a row
  fails to read, and two advisories in an indirect dependency
  (`golang.org/x/crypto`) are closed by moving to v0.57.0.
- **A vanished sign-in tab no longer strands your library's one sign-in slot.**
  *papio* gives that slot to the paper that is signing in, and deliberately
  keeps giving it when your browser restarts its background worker, so a sign-in
  you are part-way through survives the restart. Nothing bounded that: if the
  tab had already gone by then, the slot stayed held, and every other paper
  waiting on that institution parked with no tab and no message. *papio* now
  releases a slot that has reported nothing for half an hour once it is no
  longer driving that tab, and a download or navigation still in flight keeps
  the slot regardless of the clock. A sign-in *papio* is still driving is never
  cut short. One honest limit: after a background-worker restart *papio* is no
  longer driving the tab it opened, so if you then sit on a code prompt for half
  an hour without the page changing, another paper may take the slot. Your
  sign-in still completes in your own browser. Releasing needs the browser
  extension connected, because that is what drives the check.
  `papio doctor` now says this in its own remedy, instead of only telling you to
  close a tab that is already closed.
- **A fresh empty library no longer disconnects the browser every minute.**
  The exact inbox-count response correctly says that both the task-family list
  and required-turn list are complete and empty. JSON omits those empty arrays,
  but the daemon mistook omission for missing data, rejected its own response,
  and tore down the native-messaging session. Both protocol readers and the
  schema now treat omitted complete lists as empty only when their exact count
  is zero. Non-empty responses still fail closed when either list is missing.
- **A paper's stale tab can now be retired while the paper is still being worked
  on.** A retry opens a fresh tab. Retiring the previous tab needs the daemon's
  permission, and the daemon granted it only for a tab that was not the tab its
  own record named - which is never true of a retry, because a retry records a
  new attempt and leaves the previous one alone. The old attempt then held its
  tab for the rest of the browsing session. The daemon now answers from the
  paper's CURRENT attempt: a tab belonging to a superseded attempt may be
  retired, the tab of the attempt actually driving the paper may not, and an
  unfinished publisher action on the superseded attempt still refuses.
- **A publisher that proves there is no access no longer keeps the whole
  library's sign-in slot.** The browser correctly ended that provider drive, but
  the daemon requeued the paper before its tab-close request arrived.
  Rediscovery then parked the paper in document delivery, making it active
  again, so the close was refused and the old navigated claim held the
  institution's only sign-in slot without a deadline. *papio* now retires the
  exact materialization binding and its authentication lease as part of
  accepting `no_entitlement`, before rediscovery runs. The tab can still
  complete its one-use close transaction afterwards, but correctness no longer
  depends on winning that race.
- **A refused Zotero filing now says which of two very different things went
  wrong.** A library that keeps its files somewhere Zotero's upload cannot reach
  was recognised only when *papio* could read *zotio*'s machine envelope; the
  plain error line beside it, which is what *papio* usually holds, names the
  same cause and went unrecognised, so the failure arrived as `unknown` with its
  explanation cut mid-word. It is now recognised from either form. When Zotero
  desktop rejects the hand-off that files bytes into your own file store,
  *papio* names that fault separately: that route is the only one that respects
  your storage choice, so *papio* does not quietly upload to Zotero's cloud
  instead. It tells you to restart Zotero and check its plugins, and keeps
  retrying the filing by itself. The paper is in *papio*'s store throughout;
  nothing is lost either way.
- **Papers already filed in Zotero no longer sit in the queue as unfinished.**
  Some papers that had been delivered — the PDF attached, the item created —
  still read as outstanding for weeks. Every part of *papio* agreed there was
  nothing left to do, which is exactly why nobody did it: the retry pass skips a
  paper whose import already succeeded, and doctor's undelivered-imports check
  excludes it for the same reason. The step that marks the paper finished runs
  inline with the import, and these had missed it, so no later pass owned the
  repair. That left more than a wrong number on a list: such a paper could
  refuse a download that arrived afterwards, because a finished paper was still
  holding an unfinished paper's place. *papio* now marks a delivered paper
  finished on its next maintenance pass, reading the Zotero keys it already
  recorded rather than asking Zotero again — so nothing re-opens the library
  application to re-learn something it already knows.
- **A paper you have to fetch yourself now opens through your library, not at
  the paywall.** When *papio* asks you to download a PDF by hand, the page it
  needs you to reach is almost always behind your institution's sign-in, and
  the publisher's own link paywalls it. `papio actions open` handed that link
  to the operating system, which opened a plain browser window on the
  publisher's page and recorded nothing. Now `papio actions open` and the
  extension's Open button both ask the daemon for the route when you press
  them. The paper is focused in the tab you are signed into and recorded like
  every other handoff. If no library route applies, which is the open-access
  case, Open still uses the paper's own link.

  Two things deliberately do not change. *papio* never starts opening these
  papers on its own, only when you ask. And it never tries to drive the page
  for you — you asked to fetch this one yourself, so it opens the page and
  stays out of the way.
- **One paper can no longer reach your library as two interlibrary-loan
  requests.** *papio* keeps at most one live request per paper by digesting a
  key from the paper's identity. That identity is not fixed: if a paper arrives
  as a PMID and a resolver later supplies its DOI, *papio* prefers the DOI, and
  the key it computes changes. The earlier request kept the old key, so a
  lookup on the new one found nothing, *papio* concluded no request existed,
  and it could lodge a second — while the first was still outstanding and its
  outcome unknown. The uniqueness rule in the database could not catch this,
  because the two keys really were different. Confirming a request absent made
  it reachable in ordinary use, since that hands the paper back to the
  scheduler before re-checking. *papio* now resolves a paper's request by the
  paper's own record whenever the computed key finds nothing, so a renamed
  identity cannot walk past a request that already exists. No existing request
  changes, and nothing is resubmitted by upgrading.
- **A reconciliation prompt is no longer shown for a paper that is not waiting
  on you.** If *papio* failed to hand a paper back to you after a delivery
  problem, it could leave the prompt on screen while the paper was already
  moving again. Pressing it failed, because the paper was not parked. *papio*
  now hands the paper back first and only then raises the prompt, so a failure
  leaves no prompt rather than an unusable one.
- **"Confirm absent" in the inbox now works at all.** When you check with your
  library, find that no interlibrary-loan request was ever lodged, and tell
  *papio* so from the extension, it cancelled the stale request and then failed
  with a job state conflict — every time, on any normally parked paper. The
  paper was left with a cancelled request and the same unresolved prompt, so the
  one button that clears a phantom request cleared nothing. The equivalent
  `papio delivery confirm-absent` on the command line was fixed earlier; the
  extension kept the old order, which closed the prompt only after re-running
  the gate and so asked the paper to move from "waiting for you" to "waiting for
  you" — a step the state graph refuses. Both paths now close the prompt first,
  then re-run the gate, and both re-open a fresh prompt rather than
  resubmitting: *papio* still never sends a second request while an earlier
  one's outcome is unknown. Nothing about your existing papers changes, and no
  request is resubmitted by upgrading.
- **`document_delivery.allowed_hosts` now actually restricts where a request can
  go.** The setting has been documented since it shipped as controlling the
  hosts a prefilled request form or API base may reach, and the config
  reference even carries an example setting it. Nothing read it. The field
  parsed, `papio config save` wrote it back, and every request went wherever
  `base_url` pointed — so an operator who listed their library's host believed
  a control was in force when none was. *papio* now checks `base_url`, and
  `patron_web_base_url` when it is set, against the list while loading the
  config, so a mismatch is refused before any job runs rather than reported
  from inside a fetch. An entry is a bare hostname with an optional port: an
  entry without a port matches that host on any port, an entry with one must
  match both, and matching ignores case. An entry carrying a scheme, a path, or
  user information is rejected as malformed, since it cannot express a host.
  An **empty list stays permissive**, exactly as before, so no existing config
  changes behaviour by upgrading. If you have both a non-empty `allowed_hosts`
  and a destination outside it, that config has never done what it said and the
  daemon will now say so on startup: add the host, or clear the list.
- **One open-access paper no longer freezes every other paper at your library.**
  *papio* runs one browser route at a time per provider, so that two papers
  cannot take an irreversible step at the same publisher at once. Every paper
  was being filed under the library's name for that purpose — including papers
  whose route goes to doi.org and never touches the library at all. So an
  open-access paper that opened a tab held that lane for the life of its
  attempt, and every paper genuinely waiting for the library was passed over
  until it lapsed. Then the next attempt took the lane again. An open-access
  route is now filed under the site it actually visits, and a paper already
  recorded the old way is corrected the next time *papio* looks at it —
  including while it is driving a tab you can see, because only the filing
  changes and the tab itself is never disturbed.
- **A rate-limited open-access paper no longer gets sent to your library.** When
  every open-access source *papio* wanted to ask was temporarily gated — a rate
  limit, a spend cap, a provider cooldown — it spent its retry budget on the
  ordinary failures beside them and then treated the whole route as exhausted,
  which opens a library sign-in. The paper was fetchable the whole time; nothing
  had refused it, and no request had even been made. A pass that met only a
  closed gate and still has a live open-access candidate now keeps waiting for
  the gate to open. A route that is genuinely exhausted — no candidates, or a
  real refusal — still goes to a handoff, and a gated route still cannot wait
  with no wake-up time.
- **An open-access paper no longer takes your library's sign-in slot and blocks
  every other paper behind it.** *papio* signs in to one institution at a time
  on purpose: two logins at one library can invalidate each other's session, so
  while one paper is signing in the rest are refused and wait. An open-access
  paper reaches doi.org and needs no library at all, yet it was taking that slot
  and holding it. The two were impossible to tell apart at the point it
  mattered, because both arrive as the same kind of handoff and every route
  *papio* builds for a browser is stamped institutional. An open-access handoff
  now stays out of that arbitration entirely, and is also denied the side
  channel whose close would end the whole library session — a paper you are only
  reading must never be able to sign you out.
- **One parked capture no longer hides your whole inbox.** `papio inbox` failed
  outright with `unsupported triage item kind "pdf_grab"` the moment a captured
  PDF was waiting to be identified — the very row the new picker above exists to
  answer. It printed nothing at all, so every other pending paper vanished with
  it. The capture now prints as its own row, with its state and label.
- **Solving a security check no longer counts against the paper.** *papio* stops
  driving a paper on its own after three attempts that achieved nothing — the
  rule that stops it hammering a publisher forever. A provider CAPTCHA counted
  as one of those attempts, even when you went and solved it: the daemon could
  not see security checks at all, so the attempt simply looked silent and
  expired. The browser now reports when a check it drove into is gone, and that
  attempt is neither counted against the paper nor credited to it. A check
  nobody clears still expires and still counts, so a paper stuck behind a wall
  is still retired rather than retried forever. Timing only crosses the channel
  — the publisher that showed the check does not.
- **A tab you closed is now recorded as closed even if you signed in again
  since.** When the browser reported that one of *papio*'s tabs was gone,
  *papio* rejected the report if your institution sign-in had cycled in the
  meantime — and then went on holding that tab's slot, for a tab that no longer
  existed, which is one of the ways a leftover tab became impossible to clean
  up. A closed tab cannot un-close because you signed in again, so the report is
  now accepted on its own terms. Reports that *can* affect a live sign-in are
  still rejected when they belong to a finished one.
- **A paper waiting for you no longer holds a browser tab as well.** After the
  fix below, one refusal was left standing: a paper that has asked you for
  something keeps that request open by definition, so the daemon read every
  attempt to retire its tab as an attempt to discard live work and refused it —
  truthfully, and for days at a time. Waiting for a person is a request, not a
  claim on their browser. The browser may now say a paper has parked, the daemon
  authorizes that closure, and the request stays open and waiting in your inbox
  with the tab gone. An unfinished job at the publisher still vetoes it. Adds
  database schema version 47.
- **A tab *papio* opened for a paper it never finished is no longer kept open
  forever.** The extension has always asked the daemon before closing a tab, and
  the daemon answered "not eligible" for every ordinary sign-in tab — wording
  the extension correctly obeyed as a refusal. But the daemon was not
  withholding anything: it simply had no record to consult, because that kind of
  tab never creates one. So every paper that reached your library's sign-in and
  ran out of time left a tab behind, and the next paper opened another. The
  daemon now distinguishes "I am holding this one" from "this is not mine to
  hold", and only the first is a refusal.
- **Search results no longer call a PubMed paper "not in your library" when it
  is.** Discovery asked Zotero about each result using only its DOI and arXiv
  id, so a paper identified by PMID alone — ordinary for PubMed-sourced results
  — was never matched against your library and showed as unowned, inviting you
  to fetch a paper you already have. Books identified only by ISBN had the same
  hole. The cause was a second, hand-built copy of the lookup shape rather than
  a missing field, so the duplicate is gone: discovery now asks through the same
  converter the import and filing paths use, and a future identifier cannot
  reach one of them and miss the other.
- **Signing in at your library now releases the papers waiting for it, even when
  that library is configured twice.** Naming your own institution under
  `[browser.resolvers.<name>]` while it is also the top-level default is
  ordinary configuration, and it had two silent consequences: one library held
  *two* sign-in slots, and every sign-in *papio* observed at that library's own
  address was discarded as ambiguous — no record, and no paper released. A
  library is now identified by the sign-in it actually uses, and one address's
  sign-in counts for every profile that shares that same sign-in. Two genuinely
  different logins behind one address are still kept apart.
- **Signing in for one paper no longer strands every other paper at that
  library.** A sign-in that completed before *papio* had a page to sign in on
  claimed the library's single sign-in slot with no expiry at all, so it held
  that library forever: every other paper was refused with "another sign-in for
  this institution is in progress" while nothing was in progress. A sign-in with
  no page attached now keeps the short deadline it was granted, a real sign-in
  in progress still runs as long as you need, and an abandoned one is released
  automatically instead of waiting for another paper to collide with it.
- **Closing *papio*'s tabs yourself no longer strands your library.** If a
  *papio* tab disappears while nothing is watching — you close it during an
  extension reload, or the browser crashes — the paper behind it had already
  been granted its one navigation, and a granted paper's page is deliberately
  never expired on a timer. So the library's single sign-in slot stayed held by
  a paper with no page at all, and every other paper queued behind it forever.
  The extension now reports that exact loss from its own durable record of the
  tabs it opened, and a paper whose page is retired releases the library slot it
  was occupying. A sign-in still survives a browser reconnect untouched — that
  is a new session, not a lost page.
- **A library slot held by a paper whose page vanished now frees itself three
  ways.** The first release of this fix could only report a vanished tab when
  the extension still recognised its own record of it — and reloading after you
  close every *papio* tab is exactly when it does not, because it recognises
  its tabs by finding one still open. Two further paths close that: a report of
  a *gone* tab no longer needs that recognition (a tab that does not exist
  cannot be mistaken for someone else's), and when *papio* points a paper at
  another paper's sign-in page that turns out to be missing, the extension
  reports the loss instead of only failing the click — which needs no memory of
  the tab at all, so even a slot stranded by an older version frees itself the
  next time you ask for a paper. Relatedly, queued sign-in progress is no
  longer discarded when the browser reconnects.
- **Signing in no longer releases the same dead papers every time.** When an
  institutional sign-in returns, *papio* releases a few of the papers that were
  waiting on it. Papers it had already given up on — each having been handed to
  the browser three times without ever reporting an outcome, and so permanently
  retired from being offered — were still first in that queue, and took every
  slot, on every sign-in. The release now applies the same "papio has stopped
  volunteering this one" rule the ordinary path already applied, so the slots go
  to papers that can still move. Nothing is dismissed or hidden: `papio actions
  open` still drives any of them on demand, and `papio doctor` still reports the
  count that has gone quiet.
- **Papers no longer get retired for waiting their turn.** The extension drives
  one paper at a time; the rest queue behind it. But its acknowledgement said
  only "got it", never whether it was *driving* that paper or *queueing* it —
  and *papio* counted every acknowledgement as a drive that produced nothing,
  giving up on a paper after three. So one stuck sign-in, holding the only drive
  slot, could retire the whole queue behind it without a single browser tab ever
  opening. The acknowledgement now says which it is, only a real drive counts, a
  paper that waits behind another paper's sign-in is not charged for the wait,
  and the drive that later starts is reported. Papers retired this way that
  never reached a page are restored on upgrade — a one-off repair, recorded per
  paper; a paper that genuinely failed stays retired, and a paper that really is
  dead goes quiet again after its next three real attempts.
- **A cancelled paper no longer keeps its browser surface on the books
  forever.** The two sweeps that retire browser claims both refuse to touch one
  carrying any provider-effect record, so an irreversible provider action is
  never interrupted mid-flight. For a paper that has already finished there is
  nothing left to interrupt, and the record made the claim immortal: claims on
  cancelled papers stayed live with tab ids long dead, and *papio* re-announced
  their teardown on every restart while never closing its own book on them. A
  finished paper's claim is now retired, guarded the same way the institution's
  sign-in slot already was — only an effect still in flight defers it — and only
  after the teardown was actually delivered to the browser, so the tab can never
  outlive *papio*'s knowledge of it. A cancellation that the browser reported
  itself does not count as that delivery, so the paper keeps its place at the
  library while its page is still open.
- **A paper that needs you to download it by hand now hands you the page.**
  *papio* asks for a manual download when its own attempt could not produce the
  file — then had no way to take you where the file is. `papio actions open`
  refused those rows outright, the inbox's link went to the publisher's
  canonical page rather than through your library, and the listing still told
  you to sign in and download it. It now resolves the same library route a
  handoff gets, and the reminder and listing name the command that opens it.
- **An open-access paper's listing names the command that fetches it.** A row
  needing no login read "open access — no login needed" and stopped, which says
  nothing about how to get the paper.
- **A network failure no longer leaves a dead browser tab behind or retries
  into an offline network.** The extension already asked to close a route after
  Chrome reported a navigation error, but the daemon never marked that route
  abandoned, so it refused its own closing request. The observation now
  abandons and expires that exact claim, authorizes closing the dead surface,
  and keeps the candidate owned. Nothing retries automatically; choosing Open
  later records the new attempt.
- **A cancelled paper no longer holds every other paper at the institution
  gate until its old timer expires.** The scheduler's one-surface-per-library
  fence counted a terminal paper's parked route even after its provider effect
  had settled, so direct Open requests stayed queued behind work that could
  never resume. Terminal routes now leave the domain fence once their effect is
  settled; an effect still in flight continues to block.
- **A paper whose library sign-in you never finished can be opened again.** Once
  *papio* has sent a paper to your library it will not send it a second time on
  its own — that is deliberate, so one request to a publisher never becomes two.
  But if the sign-in was never completed, nothing released the paper either: it
  kept its place forever, "Open" did nothing at all, and no command could free
  it. Asking for it again now starts a fresh attempt, recorded as your decision,
  while a paper that already arrived is still never sent twice.
- **A paper that is finished waiting no longer talks to your browser about it.**
  Those same stuck papers were re-offered to the browser about once a second,
  each time to be turned down — invisible, endless, and for nothing. *papio* now
  stops offering a paper whose attempt is over and waits for you to ask again.
- **Papers that could never open now open — and *papio* stops opening and closing
  a blank tab every two seconds.** When *papio* prepares a paper's library
  sign-in it also records which paper holds that library's single sign-in slot.
  If the slot still had another paper's name on it — including a paper that had
  long since finished or been cancelled — the preparation was refused, and
  *papio* simply kept trying: a fresh blank tab made and thrown away every two
  seconds, for as long as the browser stayed open, with the paper reporting that
  it was waiting for *you* the whole time. Preparing a paper now takes the slot
  when it is genuinely free, and stands down quietly when another sign-in is
  really in progress — one sign-in per library, as before, without the churn.
- **When *papio* cannot go ahead, the log now says so.** Refusals along the
  path from "found a copy" to "tab open on your library" were silent: a paper
  could stall for a day with nothing written down anywhere. Each refusal is now
  logged with its reason, and one of them — the refusal above — had no reason
  attached at all.
- **A paper that is turned away now gets back in the queue instead of sitting
  out.** Only one paper at a time may sign in to a given institution, so the
  others are told to wait — but a paper reserves its place a moment *before* it
  asks, and being turned away did not give that place back. It looked busy to
  *papio* for the next half hour: it could not try again, and it was skipped when
  the sign-in it was waiting for finished. Turned-away papers now hand their
  place straight back, and are picked up as soon as the way is clear. A paper
  that already has a window open, or has already asked the publisher for the
  file, keeps its place as before.
- **One unopened sign-in no longer blocks a library for half an hour.** *papio*
  asks for permission to open one sign-in per institution at a time. Permission
  that was granted and then never used was held as long as a real sign-in — so a
  single paper that stopped short could keep every other paper on that library
  waiting, with nothing to say why. Unused permission now lapses in about two
  minutes, while a sign-in you are actually working through keeps its full time.
- **Your library sign-in now survives *papio* going briefly idle mid-login.** A
  browser puts an extension to sleep after about thirty seconds of quiet, so it
  wakes up as a new session — which routinely happens while you are still typing
  a password or approving a push notification. *papio* had tied your
  institution's one sign-in slot to the session that opened it, so waking up
  stranded that slot for the next half hour: it could not be extended, and could
  not be re-taken even by the very paper waiting on it, while every note *papio*
  tried to make about your sign-in was refused. Signing in was still possible,
  but *papio* learned nothing from it and could not start the papers waiting
  behind it. The slot now belongs to the paper, not to the session, so it is
  extended when *papio* wakes and picks up exactly where it left off. One
  sign-in per institution at a time is unchanged, and a finished sign-in still
  needs fresh proof after a browser restart.
- **When *papio* refuses one of its own notes about your sign-in, it now says
  so.** Those refusals were silent on both sides, so a login *papio* could never
  record looked identical to one nobody had ever attempted — which is how the
  fault above went unnoticed for weeks. The reason is now written to the log.
- **A paper you cancelled no longer makes a live one wait its turn.** *papio*
  works one paper at a time per provider, so it never has two institutional
  sign-ins racing on the same site. Deciding whose turn it is looked at every
  paper ever queued for that provider — including papers you had cancelled and
  papers that had already arrived, which keep their place in the queue forever.
  Each one cost a live paper another wait, with no tab and no message in
  between. Finished and cancelled papers now give up their place.
- **Asking again for a paper *papio* is already working on no longer reports a
  refusal.** `papio actions open` on a paper whose sign-in was already queued
  said the paper's access mode did not permit an institutional handoff — a
  different, alarming thing that was not true. Retrying is now reported for what
  it is, and when *papio* genuinely has nothing it can offer, the message no
  longer name-checks a cause it did not verify: several unrelated conditions
  produce it, and access mode was only one of them.
- **A browser-authorized tab close that raced an extension restart now
  completes instead of stranding the tab.** The daemon's handshake
  acknowledgement to a holder session now carries its live holder
  generation (`hello_ack.browser_holder_generation`), so an extension that
  restarted after persisting a close authorization can replay that close
  the moment the handshake lands, rather than waiting on state that only
  arrived through a later materialization — which on a quiet profile meant
  never.
- **Sending a PDF from your browser no longer refuses every time.** The daemon
  required a capability the extension never claimed, so every **Send PDF** was
  turned down as though the extension were out of date, in every browser. The
  extension now states that capability during its handshake, and a daemon test
  fails if any request is gated on something the extension does not offer —
  which was previously invisible, because the daemon's own tests supplied a
  handshake no browser ever sends.
- **A download *papio* could not finish no longer blocks that paper for good.**
  On databases created during a narrow window of earlier development, *papio*
  was unable to record a download as given up on — the database itself rejected
  the word. Nothing could clear such a download: not *papio*, not the periodic
  tidy-up that exists for exactly this case. Since *papio* keeps one download per
  paper, every later attempt was answered "there's already one of those", and a
  paper that failed once — a link that had expired, a download the browser
  interrupted — could never be sent again. The database is repaired on the next
  start, keeping every record. Installations set up after the correction landed
  were never affected, which is why this survived a suite that builds its
  database from scratch — and if two captures of one paper are still open, that
  repair leaves both alone and `papio doctor` says so, rather than guessing which
  one is the paper you wanted.
- **Private link details no longer reach *papio* when you send a paper.**
  Publisher and library links routinely carry signed tokens and interlibrary-loan
  tickets, which work like passwords. Sending a paper reported the whole link;
  only the publisher's site is shared now, on every path that files a paper from
  a page. A page title that is really an address is dropped rather than stored as
  the paper's title.
- **A paper is no longer left unfiled because its link confused *papio*.** Where
  a link carried a DOI beside other parameters, *papio* could read the identifier
  wrong and then look for a paper that does not exist. It now keeps the
  identifier separate from the rest of the address, so a private access token is
  never mistaken for part of a paper's identity, and a link pointing at something
  that is not the paper — a supplementary file, a cited-by list, a preprint's
  full-text view — is left for you rather than filed under the article.
- **Sending a PDF works in whichever browser you clicked in.** With *papio* connected to more than one browser — your everyday one and a second profile, or a stale connection left by an upgrade — **Send PDF** could fail outright, reporting that it "requires the current holder with negotiated effect permits". Two things were wrong. The message described *papio*'s internals rather than anything you could act on, and the refusal itself was unwarranted: one browser receives the papers *papio* goes and finds, but that has never had any bearing on an action you start yourself, which names its own browser by the fact that you clicked in it. Saving a PDF you are looking at is now independent of which browser is receiving handoffs, and the thing that stops two browsers racing a download is the single outstanding download authorization, which is what reports "busy" and can be retried.
- **A waiting browser is no longer a browser that cannot do anything.** When another browser was already connected, the second one was never acknowledged at all, so it knew none of *papio*'s capabilities and turned away everything locally — the inbox, page acquisition and Send PDF alike — while `papio daemon status` correctly answered that all was well. It is now acknowledged as waiting: everything you initiate there works, and only the papers *papio* finds by itself go to the other browser. A consequence worth knowing if you switch between browsers: a browser handing over the slot no longer discards its record of institutional sign-ins in progress, which it previously cleared along with their tabs.
- **Cancelling someone else's download no longer disconnects your browser.** The refusal for "only the browser that started this download can cancel it" was assembled in a form *papio* rejects on its way out, so instead of declining one request it failed the whole exchange and dropped the browser's connection. The same refusal is also no longer reported back as though the download had been successfully cancelled.
- **The identity rules that guard a picked paper got stricter, and the ones that could not be made safe were withdrawn.** A correction notice, comment, retraction or supplement is now recognised and never treated as the work it refers to, while a footnote merely pointing at a chapter's erratum is still the chapter. A candidate title that is only a prefix of the printed title (`Foo` against `Foo: A Different Study`) no longer counts as that title; a running head or a citation in a second column no longer counts as the document's own title; an author surname appearing only in the title or journal line no longer counts as author evidence; and a year is matched as a year rather than as digits that can hide inside an identifier. Five documents that still defeat these rules — a journal expansion citing the conference paper's DOI, a repository cover card reprinting someone else's citation, a subtitle wrapped across two lines, a numbered sequel, and a year hidden in a dotted DOI — are kept in the measurement corpus as the standing bar any future automatic rule must clear.
- **Two identifiers that name different papers are no longer treated as one.** Some registrars have published the same paper with a doubled slash after the registrant, and others have registered genuinely different papers at the single- and double-slash forms of one identifier. Nothing in the text can tell those apart, so *papio* now stops and asks instead of guessing, in both directions; a document printing both forms is likewise ambiguous rather than silently collapsed to one. Separately, a paper whose registered identifier contains a doubled slash could previously be "confirmed" by a page printing the other paper's identifier.
- **A capture that stops being wanted is no longer filed anyway.** Bytes are accepted into a paper awaiting a human only while some human action for it is still open, checked in the same transaction that accepts them. Dismissing a download request left a brief window where the request was closed but the paper still looked like it was waiting, and a delivery already in flight was filed into it.
- **A document that names a different paper is now held for review instead of
  being filed.** A captured or directly delivered PDF whose front matter
  conclusively names a DOI belonging to a work the job is not bound to now parks
  for `verify_identity` instead of being accepted. This closes a wrong-accept
  that ordinary identity scoring did not cover for DOI-less jobs: a PDF whose
  printed title and authors matched the requested paper exactly could be filed
  under the wrong citation, because the foreign-DOI check only applied when the
  job itself had a DOI. The veto runs in `validateCandidate` — the single
  convergence for direct delivery, grab binds, adoption sweeps and resolver
  fetches — so every bytes-to-artifact path is gated the same way. An explicit
  human review of the quarantined preview (`ReviewOverride`, ADR-0002) still
  overrides the veto, exactly as it overrides the neighbouring identity-review
  arm; picking a job in the popup does not — a selection supplies correlation
  evidence, not authority to overrule conclusive document identity, and a review
  remains the only way to accept a document whose own front matter says it is a
  different work. That extra step is deliberate: a silent wrong-accept is a
  worse failure than an honest `verify_identity` turn. A conclusive mismatch
  does not stop the candidate queue: the fetch path tries the remaining
  candidates first, and if none succeeds it keeps the first mismatch and binds
  the review to its checked quarantine bytes. A PDF with several front-matter
  DOIs, such as a book DOI and a chapter DOI, is ambiguous rather than a
  mismatch, and the review asks which one identifies the requested work. An
  accepted review of such a document promotes the file, even when the job's own
  DOI check also rejects the foreign DOI. The waiver holds only for a
  `verify_identity` accept, and only while the file hashes to the SHA-256 that
  the accept named, so adopting the same file again also promotes it. Other
  rejects, other bytes and an `unsafe_pdf` accept still go through every check,
  and the `ready` transition records `reason: review_accepted` and
  `identity_override: operator`.
- **A paper found by title search no longer gets filed under that search's own
  identifier.** Looking a title up returns a record, and *papio* was writing
  that record's DOI onto your request as if you had supplied it. Two papers can
  share a title — a preprint and an unrelated paper, a common review title, a
  translation — so the identifier was a guess. It then became the thing the
  downloaded PDF was checked against, and the PDF agreed, because it was the
  document the guess came from: a wrong paper, filed under a citation it
  matched, with nothing reporting a problem. The search still guides the same
  attempt's search for a copy; it just no longer renames your request. Gaps you
  left open — an author list, a year — are still filled in. When a request gives
  only a title, with no year, authors or identifier, and every lead comes from
  search results, the job now ends as `unavailable` with reason
  `insufficient_identity_evidence`, and *papio* fetches no PDF for it. Resubmit
  the paper with a DOI, PMID or arXiv id, or add its year or authors.
- **Checking a downloaded PDF now asks whether it is the paper you asked for.**
  The comparison used whichever identity the job had accumulated, so an
  identifier adopted from an earlier search was the thing the document had to
  agree with. It is now compared against what you submitted. Jobs already in
  your database that picked up an identifier this way are re-checked against
  your original request rather than against that identifier.
- **An OpenAlex address copied out of a browser is now accepted.** The address
  the OpenAlex website shows — `https://openalex.org/works/W2741809807` — was
  refused as an invalid work id, because *papio* recognized only the forms
  OpenAlex uses in its own records and its API. It is accepted now, along with
  the `www.` and `api.` hosts and a trailing slash. A bare `works/…` with no
  address around it is still refused: the positional argument guesses only at
  shapes that cannot name a different kind of identifier.
- **Clean scheduler completion no longer races its final heartbeat.** A
  heartbeat already in flight when a worker parks or completes its job now
  recognizes the lease-releasing state as success, while a wrong owner on
  active work still fails closed.
- **A browser demoted by `papio browser use` is told once.** Claiming the
  session moved the offer/handoff flow without the previous holder's extension
  ever hearing about it, so that browser kept reporting a live *papio*
  connection while receiving no work. Its next poll now carries the same
  `session_busy` frame a refused hello gets, which the extension renders as
  the browser that holds the session instead of a healthy connection.
- **The work pulse's next action counts every paper sharing its deadline.** A
  backoff cohort is scheduled on one common instant, and only the first row's
  count was reported — so a browser popup said *retrying 1* directly beneath
  *51 scheduled*, reading as though papio had forgotten the other fifty.
- **An agent driving *papio* over MCP can no longer sever its own connection by
  asking for standard input.** The MCP tools run each CLI command in-process
  with its output captured, but standard input was left pointing at the real one
  — which, for an MCP server, is the connection to the agent itself. Asking for
  `acquire --batch -` therefore had the command read the agent's own messages as
  though they were a list of papers, taking them away from the conversation that
  was waiting for them. Commands run this way are now given no standard input at
  all, so that request fails plainly and the connection is untouched. Supplying
  many papers at once over MCP is what `papio_acquire_batch` is for.
- **The latest batch in the work pulse now counts a paper whose worker died as
  stalled.** A queued or running batch paper whose lease had expired still
  counted as `continuing`, so `latest_batch.stalled` in `papio pulse --json` was
  always `0`. An expired lease now counts as `stalled`. A lease that expires at
  this exact moment still counts as in flight.
- **Document-delivery bookkeeping no longer loses a provider result or a
  reconciliation prompt.** Cancelling a paper or dismissing its request could
  overwrite a status check that had just recorded the request as fulfilled,
  declined or cancelled with `unknown_outcome`; the provider's answer now
  stands. `papio delivery confirm-exists`, and the same answer in the popup, now
  commits in one step, so a failure part-way leaves the reconciliation prompt
  open. A prepared but unsent request that a new paper reuses from a cancelled
  paper now moves to the new paper before *papio* sends it, so the library's
  transaction does not belong to a cancelled paper. After failed status checks,
  random jitter could push the next check past the 24-hour maximum interval; it
  now stays inside it.
- **Cancelling a document-delivery request can no longer report success for a
  request your library already received.** *papio* sends a request to the
  library first and records it as submitted a moment later. A `papio delivery
  cancel` in that gap overwrote the live request with "cancelled" and said it
  had worked. The cancel now applies only if the request is still in the state
  it read, and otherwise reports the state the request reached. Two similar
  races are closed: confirming a request absent while it was being confirmed
  present could cancel the live request, and a status check that overlapped a
  change of the request's library transaction number could record the old
  transaction's result on the new one.
- **A failed update check no longer repeats on every command.** When the release
  check failed (network error, error status, bad reply or empty release),
  *papio* did not record the attempt, so every later command and `papio doctor`
  checked again for as long as the failure lasted. A failed check now waits for
  the same daily interval as a successful one, and the cached release is kept. A
  cache time in the future no longer counts as recent. The *zotio* release check
  works the same way.
- **The update notice appears once a day, however many *papio* processes are
  running.** The daemon and each command read and rewrote the same update cache
  without a common lock, so both could print the notice, or one could overwrite
  a newer check result. They now take a shared lock on the cache and re-read it
  before each change.
- **Very large timeout and cadence values are now refused instead of wrapping.**
  `fetch.timeout_seconds`, `browser.action_expiry_seconds` and
  `actions.stale_after_seconds` had only a lower limit, so a very large value
  overflowed and every deadline derived from it expired at once. They must now
  be in `5..3600`, `0..2592000` and `0..31536000`, and a value outside these
  limits fails config validation. `papio watch add --cadence` also refuses more
  than `87600h`, because a larger value overflowed and made the watch run on
  every scheduler pass.
- **A timed-out Zotero change is no longer recorded as a failure.** When a
  *zotio* command that changes your library timed out or was cancelled, *papio*
  recorded a definite failure and discarded the plan, but Zotero may already
  have made the change, so a retry could import the paper or attach its PDF a
  second time. The outcome is now recorded as ambiguous. The plan is kept, and
  *papio* does not run the change again on its own. A staged PDF whose checksum
  does not match is now removed, not left where the import treats it as
  verified. Two plans for the same paper no longer race while they stage files.
- **A failed Zotero import now keeps its reason, and a paper already in your
  library is no longer reported as a failure.** An import error that *papio*
  could not classify was recorded as `unknown` with no message, and the daemon
  log said nothing. The import record now keeps the cleaned-up message, the
  daemon logs each failed automatic import, and a missing citation title or
  authors has its own class, `bundle_validation`. Before an automatic import,
  *papio* now checks whether Zotero already holds the paper with a PDF, and if
  it does, records a duplicate (`already_in_library`) instead of an error.
- **A paper with no citation title or authors can now be filed in Zotero.** When
  `papio acquire --doi` matched a PDF already in *papio*'s store, the paper
  became ready with no title or authors, and every later Zotero import failed
  with `identity.title length out of range`; no retry could fill the gap.
  *papio* now looks up the citation from the DOI in that case, and before it
  files a ready paper whose citation has no title, it gets the missing title and
  authors by DOI from your configured discovery source. A paper that used up its
  automatic attempts is repaired when you run `papio zotio import-backfill
  --apply`. If no citation is found, the import error asks you to check the DOI
  or supply the title and authors.
- **A Zotero upload refused with HTTP 413 now says why.** *papio* recorded it as
  a generic `zotero_http_4xx` error. When Zotero's reply names its storage
  quota, *papio* now records `zotero_storage_quota_exceeded` with Zotero's own
  figures, and otherwise records `zotero_file_storage_refused`.
- **A paper whose cached Zotero plan selected nothing is planned again instead
  of failing on every retry.** *papio* replayed a cached plan in which *zotio*
  had selected no import, so the paper failed with `Zotio manifest entry is
  "unresolved", not resolved` on every attempt. *papio* now discards that plan
  and makes a new one. A plan that may already have written to your library is
  still never discarded.
- **A paper without a DOI can now be filed in Zotero as a new item.** *papio*
  required a DOI to create a Zotero item, but it used the DOI only to name the
  PDF it hands to Zotero. Book chapters, preprints, reports and older articles
  identified by PMID, arXiv ID or ISBN were refused, and they are now filed. A
  paper known only by its title is still refused.
- **Checks for papers already in your Zotero library work with current *zotio*
  again.** `zotio items find` returns its results inside a `{meta, results}`
  envelope, but *papio* read only a bare list. Every lookup failed: search
  results came back unclassified, watch runs stopped with an ownership-lookup
  error, and the browser could not tell that you already had a paper. *papio*
  now reads both shapes. It also reads `zotio items missing-pdf` in both shapes,
  so a newer *zotio* that wraps that list does not break the queue of Zotero
  items without a PDF.
- **A paper whose Zotero item already has a PDF is now marked as filed.**
  *papio* tried to attach the PDF to the existing Zotero item again on every
  retry pass, even when the item already held a PDF. On a library that keeps its
  files on its own storage, each upload was refused, so the paper stayed unfiled
  and was retried without end. *papio* now checks whether the item already holds
  a PDF. If it does, *papio* records the paper as already in your library and
  makes no upload.
- **An HTTP 408 from arXiv or Europe PMC is now a temporary failure.** A request
  timeout from these two sources was treated as a permanent error, and
  `Retry-After` was ignored. It is now handled like a 429 or 5xx reply, as the
  other sources already did.
- **BibTeX export keeps an author name that contains "and" as one author.**
  BibTeX reads a top-level ` and ` as the separator between authors, so a name
  such as `Research and Development, Ada` became two authors on import. `papio
  export --format bibtex` now wraps each author name in braces, so BibTeX reads
  it as one literal name, as the CSL-JSON export already does.
- **`papio bundle export` no longer reuses a symlink or leaves a partial
  export.** A symlink at the artifact's place in the destination counted as the
  exported artifact when it pointed at matching bytes, so the bundle depended on
  a file outside it that could change. *papio* now replaces a symlink, or any
  other file that is not a regular file, with its own read-only copy. A failure
  while it created the artifacts folder or copied the artifact also left a
  half-written tree, and any failure now rolls the export back.
- **A PDF that *papio* has already stored is no longer reported as a failed
  save.** *papio* made the stored file read-only and removed its temporary copy
  only after the file was already in place. A failure in either step reported
  the save as failed, and *papio* deleted the file's record while the file
  stayed in its store. The file is now made read-only before it is put in place,
  and a failed cleanup of the temporary copy no longer fails the save.
- **A crash while *papio* files a paper no longer leaves a file that no paper
  owns.** *papio* now records each file it is about to publish before it moves
  the bytes into place, then records the file's owner, the accepted candidate
  and the paper's next state in one step. After a crash or an interrupted
  database write, it checks the published file or the held copy and finishes the
  same publication; if neither survives, it removes the record. If the paper
  already owns the file, recovery removes the leftover record and keeps the
  filed copy, so the paper does not stall.
- **Search results no longer merge different papers that share a title.** When
  search sources returned results with no DOI, *papio* merged them on the title
  alone, so two different works with the same title became one row in `papio
  search` and in watch runs. Such results now merge only when the title, year
  and authors agree, and a result that has only a title is never merged.
  Semantic Scholar arXiv ids are now put in the standard form, and an id that
  cannot be read is dropped, so an arXiv result matches the same id in your
  library.
- **`papio init` reads a ProQuest account id only from the query of your library
  URL.** When you gave a library URL with `--institution-url` or at the resolver
  prompt, *papio* ran the account-id pattern over the whole address. An
  `accountid=` in the fragment or the path could be saved as
  `proquest_account_id`, which *papio* adds to ProQuest link-resolver requests.
  The id now comes only from an `accountid` query parameter that contains only
  digits.
- **A truncated RIS file is now refused.** A final record with no `ER` end tag
  was read as complete, so a cut-off RIS export loaded without an error and its
  last entry could be incomplete. `papio acquire --batch` now fails with `ris:
  unterminated record: missing ER terminator`. A RIS library export that *papio*
  reads to know what you own keeps its last good reading.
- **A retraction notice that covers several papers now flags all of them.** New
  notices were matched by the notice's own DOI, so when one notice covered
  several papers in your library, only the first reached the inbox and the
  notification. Each affected paper now gets its own inbox item.
- **A daemon that never becomes ready is now stopped.** When an automatic start
  timed out before the daemon opened its socket (after 5 seconds by default),
  the half-started process kept running, and each later attempt started another
  one. *papio* now asks that process to stop, kills it if it has not exited
  after 2 seconds, and reaps it. On Unix the signals go to the daemon's whole
  process group.
- **Watch runs and digests no longer lose or repeat work.** When the reply that
  says which discoveries are already in your library came back shorter than the
  request, with no incomplete marker, *papio* read it as the end of the list,
  dropped the trailing discoveries, and still reported success. The run now
  fails. Acquiring from a digest saves its manifest before it submits, so a
  retry after a write failure reuses the manifest and does not create the jobs
  again. An inbox decision on a paper that several watches found now checks
  every watch before it changes any, so one conflict no longer leaves the
  earlier watches consumed.
- **Paging through the inbox no longer skips or repeats items.** The page cursor
  was a plain position in a list that changes while watches run, so items moved
  between pages. The cursor now marks the last item it returned. A cursor whose
  item has gone, that was made for a different inbox format, or that an older
  daemon issued is refused, and the next read starts again from the first page.
- **A PDF whose DOI wraps onto the next line is no longer rejected as the wrong
  paper.** PLOS and similar publishers print a long DOI that `pdftotext` breaks
  after a `.` or `/`. *papio* read the cut-off prefix (for example
  `10.1371/journal`) as the document naming a different paper, so it refused
  every copy of the correct PDF. A DOI split after a separator is now joined
  again, but only to confirm the DOI you asked for. A joined DOI never refuses a
  file and never names a captured file.
- **A spent monthly budget now parks a paper instead of ending it.** When the
  only sources left had used up their `max_cost_usd` budget, *papio* skipped
  them without a record, and the job could end with no legal candidates. A spent
  budget is a reason to wait. It is not proof that no legal copy exists. The job
  now waits for the budget to reset at the start of the next month.
- **`papio doctor` now names the papers whose files it will never collect from
  the old download folder.** The `adoption_root_legacy` warning said that
  *papio* still adopts settled files from the superseded download folder. That
  never happens for a paper you cancelled before its file reached *papio*'s
  store, because *papio* will not delete the only copy, so the warning never
  cleared. Doctor now separates the folders that still drain from the folders
  that never will, and lists those papers by title. To file one, open its PDF
  and use **Send PDF** in the extension popup, or acquire the paper again with
  **Acquire this page** on the publisher page.
- **The popup's progress line no longer fails while a sign-in wait still lists a
  finished paper.** A paper that had finished or been cancelled could stay
  listed on an open institutional sign-in. *papio* counted it in one total but
  not in the other, rejected its own progress summary as inconsistent, and the
  popup said live progress was temporarily unavailable. Finished papers no
  longer count toward a sign-in wait. When a popup read fails the same way on
  every refresh, `daemon.log` now logs it once and then a count of the repeats
  every five minutes, and a new or different failure is still logged at once.
- **Identity checks on a scanned PDF now read only its first page.** *papio*
  joined the OCR text of each page with no page break, so the checks meant for
  page one read every OCR page. A DOI on page two, for example in the reference
  list, could then be taken as the scan's own identifier, and a captured PDF
  could be filed as a paper it only cites. OCR text now keeps its page breaks.
- **Repository copies of a paper found through OpenAIRE are now tried.**
  OpenAIRE lists `doi.org` links among a paper's copies, and *papio* keeps only
  three copies from each record. Those links lead back to the publisher page
  that *papio* already knew, and they often took all three places, so no
  repository copy was tried. *papio* now skips `doi.org` and `dx.doi.org` links,
  and still keeps handle links, which point to repository copies.
- **A cancelled or finished paper no longer holds its library's sign-in slot.**
  The slot was freed only when another paper tried to take it, so with no other
  paper asking it could stay held for hours, and every status read showed a
  sign-in in progress. *papio* now frees such a slot the next time the browser
  extension checks in. A paper whose request to the publisher may still be in
  progress keeps the slot.
- **A paper is no longer blocked by its own earlier attempt after the browser
  restarts.** When a paper's request to the publisher had finished on a tab that
  was then closed, *papio* kept that attempt as the paper's live claim and
  waited for a result that could no longer arrive. The paper was not offered
  again, and when it asked for a new page it was refused. *papio* now retires
  such an attempt when the browser session that made it is gone, and frees the
  library sign-in slot that it held. An attempt whose publisher request may
  still be running still blocks a second attempt.
- **A browser restart no longer gives your library's sign-in to a second paper
  while a publisher request is unresolved.** A finished sign-in was released
  when the extension's background worker restarted, when its paper ended, or
  when the proof of sign-in aged out. This happened even while that paper's
  request to the publisher was still unresolved, so a second paper could take
  the library's only sign-in slot. The slot now stays with its paper until that
  request resolves, whatever the reason for the release.
- **Papers queued in your browser no longer stop other papers from being
  offered.** The daemon keeps at most four papers in flight in the browser. It
  counted a paper that the extension had only queued behind its single drive
  slot, so four queued papers could fill every place and the rest of the queue
  did not move. The daemon now leaves out each paper that the extension reports
  as queued. The extension still drives one paper at a time, and each library
  still has one sign-in at a time.
- **A paper that meets the same library sign-in wall on every attempt now stops
  being retried.** *papio* stops offering a paper on its own after three browser
  drives that achieve nothing. Drives through the institutional sign-in path
  were not counted, so such a paper went back into the same wall without end. It
  also held its library's publisher lane, so other papers at that library waited
  behind it. That path now counts toward the limit, and a download on a later
  drive still clears the count. `papio actions open` still drives the paper when
  you ask.
- **A publisher page that no adapter recognises is now recorded with its host.**
  When no adapter rule matched a publisher page, the outcome and the drift
  record named no host, because the host came only from a page capture that this
  case never takes. The browser now reports the page's host with its outcome,
  and *papio* keeps it in the paper's history, so a compiled direct download
  route for that host can be found. An outcome from an older extension that
  sends no host still uses the capture.
- **A filing hook that times out or is cancelled on Windows now stops the
  programs it started.** *papio* runs each hook in a Windows job object and ends
  the whole process tree at the deadline. Before, only the shell was stopped,
  and programs it had started kept running.
- **A replaced notification stays replaced, and a webhook digest keeps every
  event.** A batch progress notification held for quiet hours could come back
  after the final notification had replaced it, and was then shown after the
  final one. A webhook digest also stopped taking new events once the desktop
  notification was sent, or when desktop notifications were off for that
  category, so it reported only the first part of its window. It now includes
  every event in the window.
- **The preview of a PDF held for identity review works again after its local
  server stops.** When the preview server stopped on its own, *papio* still
  treated it as running and handed out preview links to a closed port until the
  daemon restarted. The next preview request now starts the server again.
- **Saving a page capture no longer stalls the browser connection.** The daemon
  wrote each diagnostic capture while it held the lock that every browser
  request needs, so a slow disk delayed all of them. It now writes the capture
  outside that lock and discards it if the request or the connected browser
  changed during the write.
- **A long batch label in a non-Latin script no longer disconnects the
  browser.** The work summary sent to the extension cut a batch label to 256
  characters, but the limit is 256 bytes. A long label with accented or
  non-Latin characters therefore produced a message that *papio* itself refused
  to send, and that refusal ended the browser session. The label is now cut to
  256 bytes at a character boundary.
- **A browser download keeps how it was reached when its route details arrive
  during the file check.** If the browser's route and sign-in details for a
  download arrived while *papio* was still checking the downloaded file, *papio*
  kept the file but never recorded those details, so the paper's access basis
  stayed empty. *papio* now applies them to the file when the check ends.
- **A paper whose candidates have all failed no longer repeats its lookups
  forever.** When another source was closed in the same pass, a pass that did
  reach a source was counted as held back and not charged against the paper's
  retry limit. The limit then never took effect, and the paper repeated its
  lookups at every retry interval, spending provider credits each time. Metadata
  and DOI enrichment requests now count toward the limit too, and a pass that
  made no request still does not. If the retry history cannot be read, the paper
  settles instead of trying again; `papio jobs retry` restarts it. When several
  sources are closed for different times, the one final wait after the limit now
  lasts until the last of them reopens.
- **An OpenAlex lookup by DOI or OpenAlex ID must now come back about the paper
  that was asked for.** Both lookups count as exact, fully confident matches,
  but *papio* used whatever record arrived, so a misrouted or duplicated answer
  could be filed under your citation. The record must now repeat the identifier
  that was requested; a record that OpenAlex has merged into another is accepted
  under its new identifier. OpenAlex gives most identifiers in two places, and
  *papio* took whichever came first. When the two disagree, that identifier is
  now discarded.

## [0.21.0] - 2026-08-14

### Added
- **Browser effects remain at-most-once across extension restarts.** Store
  schema `34` adds daemon-owned effect permits for generic and direct provider
  downloads, PDF grabs, configured terms acceptance, and institutional
  navigation. Each effect must acquire the single global and provider-domain
  lane before execution; a lost callback remains visibly blocking until its
  exact result, correlated artifact winner, restart reconciliation, or an
  exact-ID operator resolution proves the outcome. `papio pulse`,
  `papio doctor`, and `papio browser permit resolve` expose unresolved
  occupancy. The strict `effect_permit_v1` protocol requires extension 0.14.0;
  older peers receive `unsupported` rather than acting without authority.

- **Notification routing and liveness are durable and bounded.** The daemon now
  uses one typed notification router and durable notification ledger, with
  migrations `0030` and `0031` (store schema version 31), independent
  human-desktop and automation-webhook policies, and the `quiet`,
  `milestones`, and `verbose` presets. `papio notify show|preview|test` exposes
  the effective policy and an explicit local test; `papio pulse` and the
  feature-gated `work.pulse_v1` read model expose the honest Moving, Scheduled,
  Waiting, Stalled, Idle, and Unknown vocabulary. The browser bridge now
  negotiates the four new request frames `surface_presence`,
  `work_pulse_request`, `activity_page_request`, and
  `page_bulk_submit_v2_request` plus their responses, each feature-gated
  without widening older frames. Counts schema v3 and triage snapshot schema v5
  carry daemon-owned required-turn and family-run projections. Browser and CLI
  submissions persist durable acquisition cohorts and chunk membership rather
  than deriving progress from telemetry. `papio doctor` reports desktop
  notification capability explicitly; unsupported platforms and OS
  notification outcomes are never presented as delivered.

- **Direct provider PDF routes.** For delegated jobs whose provider is already
  known from resolver policy or prior browser evidence, the daemon now
  computes direct PDF endpoint candidates from a compiled, versioned route
  table (Wiley `pdfdirect`, SAGE `pdf`, ScienceDirect `pdfft`) and offers them
  through the browser ahead of the ordinary institutional handoff — one
  candidate at a time, with the route revision recorded in the job's events
  and automatic fallback to the normal path when candidates are exhausted.
  Requires extension 0.13.0; `[browser] direct_routes_enabled` (default
  `true`) turns the behaviour off.
- **Cell Press PII PDF route.** The compiled route table now includes `cell-pii-showpdf/1`: `https://www.cell.com/action/showPdf?pii={pii}`. The compiler accepts the identifier slot in the query (`pii={pii}`) as well as in the path, still exactly one named slot, still no freeform interpolation.

- **Drift and safety latches.** A browser outcome that proves the wrong work,
  a failed validation, or an unexpected effect now durably stops further
  automatic browser offers for that job and provider; ordinary page-drift
  outcomes latch only the exact adapter revision and host, so a repaired
  adapter is offered again while the broken one is not.
- **`papio jobs failures` groups failures into incidents.** Terminal and
  parked jobs are aggregated by a keyed failure-shape fingerprint. The
  fingerprint excludes raw hosts and identifiers and uses a per-installation
  secret, resisting stable cross-install correlation; local `jobs failures` and
  `jobs incidents` output intentionally includes bounded `safety_domain` and
  registrable `host_family` labels for diagnosis. Older daemons simply omit the
  incident rows.
- **Failure evidence is pinned while an incident is open.** The first
  decisive and latest page captures of an open incident are exempt from the
  per-host capture eviction until the job resolves, so a burst of newer
  failures cannot destroy the evidence a repair needs.
- **`papio adapter repair <capture>` scaffolds a fix.** From a stored or
  local capture it produces a ready-to-review workspace under
  `dev/scratch/repair/`: the sanitized fixture with its header, an
  adapter-try analysis report, the adapter's next revision string, and the
  exact apply steps — without touching extension source.
- **`papio jobs diagnose` explains parked work.** The read-only, daemon-owned
  diagnosis classifies provider adapter gaps, page drift, rejected adopted
  files, wrong-work outcomes, landing pages, and human-authentication steps,
  then reports the exact next action and whether the selected job can be
  opened or retried. It does not drain the human-action queue.
- **Title-only works get an OpenAlex rescue.** When a request has a title but
  no usable identifier and Crossref finds nothing, the daemon now searches
  OpenAlex with the same strict corroboration (exact normalized title,
  matching year, agreeing authors) and adopts the work's OpenAlex ID — and
  its DOI when one exists — unlocking the normal resolver pipeline. Runs
  behind the existing `openalex` source policy and its contact-email
  requirement.
- **ISBN-only books park for you instead of dying.** A book request that has
  an ISBN but no DOI no longer ends `unavailable: no_identifier` when an
  institutional OpenURL resolver is configured: *papio* opens an assisted
  handoff with a book-formatted OpenURL (`rft.isbn`, title, year, authors) so
  you can fetch it through your library while *papio* tracks the outcome.

### Fixed
- **Publisher PDFs with embedded files are held for review instead of rejected.** Encrypted PDFs and those carrying active content (JavaScript or embedded files) now correctly park in `needs_review` with the quarantined file held for `unsafe_pdf` review. Accepting that review is refused (the original bytes are never promoted). Rejecting it returns the job to `awaiting_human` with a new `manual_download` so a different file can be supplied. Previously SAGE and Taylor & Francis publisher PDFs that bundle supplementary files hit the generic `invalid_pdf` path — the daemon asked the operator to supply a different file and moved the original to `rejected/`, while the `unsafe_pdf` review path was dead for those structural flags.
- **A grabbed PDF that matches a live job is adopted into that job.** When front-matter identification finds a DOI that already has an in-progress job, the captured file is moved into that job's adoption directory under a unique name instead of being discarded as `already_owned`. A ready artifact still reports `already_owned` only after `MatchIdentity` passes, and the bytes are dropped only after that outcome is durable. Embedded/JS/encrypted captures are no longer deleted as `failed_validation` before that join can run.
- **Browser downloads are adopted on a fresh install.** `download_adoption_root`
## [0.20.0] - 2026-08-10

### Added

- **Fresh, click-time institutional handoff links.** The browser bridge now
  advertises `handoff_link_v1` and answers a correlated request only while the
  named job is still `awaiting_human` with an open `openurl_handoff` action.
  Each response rebuilds the route from current configuration, so a cold offer
  no longer depends on a signed resolver URL that may have expired while it
  waited in the inbox. Routine misses are structured outcomes and never tear
  down the native-messaging session.

- **A delivery request stranded before submission recovers itself.** Nothing
  ever retried a row left in `offered`: the status poller only follows live
  requests and `papio delivery resume` refuses anything that never reached a
  provider, so a submission that failed in transit waited for a human to
  notice. A bounded maintenance pass now retries them unattended — but only
  on the request's own creation-time gate evidence (auto-capable profile,
  digest still matching current configuration, and a recorded decision to
  submit), so a prefilled request waiting on its human, and any row whose
  evidence is missing, are left alone. Because ILLiad has no uniqueness
  check, a retry that could duplicate a real loan request is never guessed
  at: submission failures are classified when they happen and only a request
  whose bytes provably never left the machine is replayed. Rows are claimed
  under a lease so two daemons cannot both submit.

- **An ambiguous submission is investigated before anyone is interrupted.**
  When papio cannot prove its request never reached the provider, it now
  looks instead of asking: a read-only search of the patron's own request
  list for papio's echoed idempotency token. Exactly one match whose every
  exposed identity field agrees is adopted silently and resumes polling, so
  a request that did land stops being invisible. Absence is never treated as
  proof — no one can prove a provider's list was complete — so it can never
  authorize a retry: it schedules bounded rechecks, then asks. Two matches, a
  contradicting title, author, DOI or request type, a failed read, or a
  patron mapping papio cannot look up are all human cases. The search is
  structurally incapable of writing to the provider, which matters because
  the failure it exists to prevent is a second real interlibrary loan.
  Reusing the poll path's lookup surfaced two defects in it: it accepted the
  first token match while ignoring a second, and it called a route that does
  not exist on a conforming ILLiad, so patron-list reconciliation had never
  actually worked.

- **Recorded consent is bound to the identity that granted it.** The gate
  digest described policy but not who authorized a submission, so a profile
  rebound to a different patron, or a rotated credential, still matched — and
  a stranded request could have been submitted under an identity that never
  approved it. Patron mapping, credential, endpoint and reference field now
  bind into a keyed digest that persists no secret and no patron identifier.

- **Grab an open PDF straight from a browser tab (ADR-0020).** A tab
  rendered in Chrome's own PDF viewer has no DOM to scan, so the selection
  workspace used to report "no recognizable identifiers" for a paper the
  operator was already looking at. Scanning a PDF tab now offers a one-click
  "grab this PDF" row when the tab's own URL carries no identifier; accepting
  it allocates a `pdf_grab_v1` capture slot (`pdf_grab_request`/
  `pdf_grab_result`), steers `chrome.downloads.download` to
  `papio/grabs/<grab-id>/` — no PDF bytes ever cross native messaging — and
  the daemon's grab sweeper quarantines the settled file, structurally
  validates it, and extracts a front-matter DOI (`internal/pdf`'s
  `documentDOIs`). A found identifier creates the ordinary identifier-keyed
  job (ADR-0010's ledger dedupe applies naturally: an already-owned work
  reports "already in your library" rather than a duplicate job); no
  identifier leaves the captured bytes in a durable `parked_no_identifier`
  grab row for triage, and `papio grabs identify <grab-id> --doi|--pmid|--arxiv`
  binds those bytes to the canonical job without a network re-fetch. Firefox
  has no download-steering API, so the row renders disabled there with honest
  copy. New migration 0025 adds the `pdf_grabs` table.
- **PDF grabs remain recoverable in the inbox.** The triage-snapshot/4
  negotiation adds a jobless `pdf_grab` row for a captured PDF that still
  needs an identifier. It shows the grab state and the exact
  `papio grabs identify <grab-id> --doi|--pmid|--arxiv <value>` guidance;
  dismissing it removes only the grab, never a job.

- **An agent skill (`SKILL.md`) that drives the CLI directly.** MCP was the only
  documented way to hand *papio* to a coding agent, which put a server between
  the agent and a CLI it can already run. The repo now ships a single root
  `SKILL.md` — installable with `npx skills add OrgMentem/papio` or by copying
  it into an agent's skill store — covering the acquisition loop, the `--json`
  contract, and the rules that keep an autonomous agent inside papio's design:
  a human gate is an outcome rather than an error, bare `actions open` drains
  the whole handoff queue and is never an agent's to run, `actions resolve
  --accept` records a *human* verdict, `zotio apply` needs the plan's exact
  digest, and everything a publisher hands back is data rather than
  instruction. `papio mcp` is unchanged and stays the path for hosts that
  cannot run commands.
  `TestSkillInvocationsResolve` and `TestSkillFlagMentionsResolve` pin every
  command the skill runs and every flag it names — including the ones discussed
  in prose rather than on a command line — to the live cobra tree, so it cannot
  drift into telling an agent to run something that no longer exists.
- **`page_bulk_status_request` accepts OpenAlex work identifiers.** The
  `identifiers.kind` vocabulary gains `openalex` alongside `doi`/`pmid`/`arxiv`
  — mirrored across `internal/protocol`, the browser extension's TS
  validator, and `protocol/browser-v1.schema.json` so the three stay
  provably in agreement. The bridge normalizes a scanned W-id through the
  same `work.NormalizeOpenAlex` the CLI and MCP already use and resolves it
  through the existing OpenAlex source, so ownership answers come from
  papio's own ledger. zotio's matcher (DOI/ArXiv/PMID only) is never
  consulted for an OpenAlex-only row — it follows the same not-yet-checked
  presentation as any other identifier no configured source covers, never a
  false `eligible` claim.

### Changed

- **Rejected identity reviews close every non-advisory action.** Review
  rejection now uses the same transaction-local terminal closure as ordinary
  job transitions, so a parked `downloads_access_required` (or any other
  non-advisory action) cannot survive beside a cancelled job; the
  `openurl_available` advisory remains intentionally open.

- **Negative document-delivery limits no longer reject the whole config.**
  `monthly_request_cap` and `status_poll_minutes` values below zero were
  refused at load, which made a hand-edited config unparseable for every CLI
  command; they now fall back to their runtime meanings (unlimited cap, the
  default poll interval) like other tolerated values.

- **Dependency: `golang.org/x/net` 0.54.0 -> 0.55.0** (GHSA-5cv4-jp36-h3mw,
  medium: HTML-parser denial of service). papio parses provider landing
  pages with this parser; exposure was already bounded by the 512-byte
  landing-page read cap, and govulncheck reports no reachable vulnerable
  call after the bump.

- **The docs landing page leads with the animated wordmark.** The mark that
  opens the README now opens <https://orgmentem.github.io/papio/> too, in a
  full-width two-column hero rendered by a `home.html` template override. The
  hero columns are sized to the page's own: the mark sits over the navigation
  rail and the headline, blurb, and calls to action start exactly where the
  article text below them does. It stacks once the theme drops that rail.
  Both ink variants are stacked and cross-faded rather than display-toggled,
  so switching the palette does not restart the animation.

### Fixed

- **The daemon no longer kills its own browser session over a delivery
  request it just created.** `offered` is the state every delivery request
  starts in, and ADR-0017 has listed it since the feature was designed, but
  it was missing from the wire vocabulary — so the daemon's outbound
  self-validation rejected its own triage snapshot, returned a raw error
  from a bridge handler, and tore down the whole native-messaging session.
  Two rows were enough to disconnect the extension every 60 seconds and
  kill every request in flight with it, including the availability check a
  selection workspace was waiting on. The state is now carried in Go, the
  TypeScript parser, and the JSON Schema, and the inbox renders it as
  created-but-not-submitted rather than borrowing `pending`'s meaning.
- **A snapshot that cannot be represented degrades instead of
  disconnecting.** Items are validated as they are assembled, an
  unrepresentable one is omitted and logged, and the assembled frame is
  re-validated; every branch that used to return a raw error — unconfigured
  service, stale cursor, query failure, oversize single item — now returns a
  structured unavailable result. Omission adjusts counts only where a schema
  defines them over the frame's own items; triage-snapshot/4's totals stay
  global, because hiding an item there would under-report real work.
- **Interlibrary-loan submission can actually reach the provider.** The
  ILLiad client was the shared SSRF-hardened fetch client, which rejects
  every non-GET and discards the body, so `CreateTransaction` POSTs never
  left the daemon and automatic document delivery had never worked outside
  tests. POST is now an explicit per-client capability with the same
  destination policy and dial-time pinning as GET, redirects refused rather
  than replayed, and exactly one client — ILLiad's — holding it.
- **Only a transport failure can end a browser session now.** Any non-nil
  error out of a bridge handler used to tear down the whole
  native-messaging session — page-bulk, inbox, grabs, session evidence and
  every handoff at once — which is how a single unrepresentable delivery
  state disconnected an extension every 60 seconds, and how failed inbox
  counts, stats reads and a stale review preview each disconnected it
  before that. The rule was never a safety property: the host's own idle
  poll already treated the same errors as transient. Application failures
  are now logged and answered with an error frame that carries the failed
  request's id, so the waiting caller settles instead of hanging, while
  framing, size-cap and self-validation failures stay fatal because an
  unrepresentable frame means the peer contract is broken. A page-bulk row
  too large to fit its response is reported as `frame_too_large` — "Could
  not fit in daemon response" — rather than being mislabelled as an
  unrecognized identifier.
- **A submitted delivery request can no longer be stranded by a crash.**
  State and provider reference were written in two transactions after the
  provider had already created a real loan request, so a failure between
  them left a row that recovery skipped and the poller could not read.
  Both now commit in one transaction, guarded so only the state a
  submission legitimately runs from can complete it; if the guard misses,
  the reference papio just received is recorded and surfaced for
  reconciliation rather than dropped. A submission is also marked
  ambiguous before the request leaves, so a persistence failure after a
  successful send can never authorize a second real loan.

## [0.19.1] - 2026-08-08

### Fixed

- **Workspace ownership checks no longer trigger a Zotero-account sync.** A
  triaged documentation review caught the privacy page promising "no request
  leaves your machine" for page-bulk ownership marks while the underlying
  zotio lookup unconditionally refreshed its mirror first — a network
  operation against the user's Zotero account, once per status poll. The
  page-bulk path now asks zotio in local-only mode (the on-disk mirror
  answers; nothing is synced), which makes the published claim true and
  removes per-poll cloud traffic. Pre-acquisition dedupe for batch submit
  and watches keeps the refresh-first behavior deliberately.

## [0.19.0] - 2026-08-08

### Added

- **A blocked Downloads folder is now a visible action, not a silent stall.**
  When macOS privacy consent (TCC) leaves the daemon unable to read the
  download-adoption root, a completed browser download opens one
  `downloads_access_required` human action on its job — surfaced in the
  extension inbox with required attention and the exact grant remedy — and
  resolves itself the moment adoption succeeds. Poll and sweep passes open
  at most one per job; dismissing it never cancels the job.
- **Adoption sweeps can no longer wedge on a hung filesystem.** Both sweeper
  passes route the adoption-root listing through the bounded, latch-aware
  reader (previously only per-job scans were protected), a single-flight
  gate guarantees at most one outstanding hung syscall bridge-wide (the
  suspend/resume log pair now fires exactly once per episode), and the
  terminal sweep removes empty stray directories from prior database eras —
  a store hiccup is never treated as evidence a job is unknown.

- **Page-bulk status now knows what you own.** `zotio.Service.LookupWorks` —
  already serving batch submit's ownership classification — is wired into
  `page_bulk_status`: a scanned page's identifiers merge zotio's
  `owned_with_pdf`/`owned_missing_pdf`/staleness verdicts with the daemon's
  own ledger, under a fixed precedence — papio's own ready bundle first,
  then zotio's `owned_with_pdf`, then zotio's `owned_missing_pdf` (carrying
  the Zotero item key), then a live queued job, then a complete negative
  lookup (`eligible`). `LookupWork` gains PMID (the facade previously
  carried only DOI/arXiv id; zotio itself already resolved PMID). A stale
  or failed zotio round reports the new `ownership_unknown` status rather
  than a false `eligible` or `ownership_incomplete` mark — page-bulk must
  never claim a work is unowned when it could not check. A `nil` zotio
  client (unconfigured) preserves the prior behavior byte-for-byte.
- **`papio stats page-bulk` measures the feature honestly, denominator and
  all.** Migration 0023 adds `rendered_record_count_hint` to
  `page_bulk_runs`: the extension's scanner counts visible result records
  for structurally-recognized page shapes only — definition-list rows, a
  reference/citation list, or a repeated card grouping, each requiring at
  least two matching siblings before it counts as a "list" at all — without
  reading a single title, URL, or docid, and reports `null` rather than a
  guess when no shape is recognized. The new `stats page-bulk` command
  reports, per source-origin class, total scan sessions, useful-scan rate,
  bulk leverage, submit conversion, and `identifier_yield` — now backed by
  that rendered-count denominator instead of standing without one.
- **`papio bench` compares acquisition coverage, not absolute results.** A
  new hermetic harness reads a `papio-bench-cohort/1` file — a work request
  plus its expected outcome class (`autonomous_ready`,
  `ready_after_human_boundary`, `honest_unavailable`, `identity_review`;
  never an expected provider or route) — and runs it twice against an
  ephemeral database, an empty artifact cache, and injected resolver
  fixtures: once with a baseline overlay (Semantic Scholar, OpenAIRE, and
  typed-relations resolution disabled) and once with the current build.
  `fixture_missing` is an explicit reported state, never a silent skip. The
  headline is `incremental_autonomous_ready` — how many more works the
  current build resolves without a human boundary than the baseline does.
  The measuring instrument ships now; the frozen field cohort seeded from
  the 2026-07-21 report is defined but awaits recorded resolver fixtures,
  so every field-cohort row honestly reports `fixture_missing` and no
  incremental field number is claimed yet. `papio bench --cohort <path>`
  never talks to the daemon; a manual live mode is deferred and does not
  block this release.
- **ILLiad requests now poll their own status, and a stuck poll can be
  recovered.** Migration 0024 adds poll-health bookkeeping
  (`provider_status_raw`, display status, `last_successful_poll_at`,
  consecutive-failure count, error class) to `delivery_requests`. The poll
  executor's state map is fail-closed: any successful nonterminal read
  resets the failure count and settles `pending`; `Delivered to Web`
  becomes `fulfilled`; `Cancelled by Customer`/`Cancelled by ILL Staff`
  become `cancelled`/`declined`; an unmapped custom status — ILLiad's
  statuses are institution-customizable, so there is no exhaustive enum —
  stays `pending` with the raw string persisted; and `Request Finished`
  classifies from prior recorded observations, never a guess. **A failed
  poll never becomes `unknown_outcome`.** Transient, credential, and schema
  failures leave the request's recorded state untouched and degrade
  integration health instead: three consecutive failures mark a row
  degraded, and 24 hours with no successful poll raises an advisory that
  *papio* cannot *observe* the request — never that it failed.
  `unknown_outcome` is reserved for genuine provider-side uncertainty after
  successful communication and exhausted reconciliation (a 404 following a
  prior successful lookup, reconciled first against ILLiad's `UserRequests`
  and papio's own idempotency reference). Every poll persists its state as
  a compare-and-swap on the row's originally-read state and next-check
  time, in the same transaction as its event insert, so two pollers racing
  the same row cannot double-append a `fulfilled` event or regress a
  settled state — a lost race is a plain no-op. `papio doctor` reports
  observation health per profile (`poll_health`) without ever claiming a
  request failed, and its remedy for a contract-drift park now names the
  actual recovery command: **`papio delivery resume <request-id>`** clears
  a live request's poll-failure bookkeeping (a terminal row is refused with
  a structured reason) so the next scheduled poll is no longer a no-op —
  pair it with `papio jobs retry <job-id>` to poll immediately rather than
  waiting for the schedule.
- **A fulfilled ILLiad request can retrieve its document through the
  ordinary browser handoff (ADR-0017 Decision 6).**
  `document_delivery.patron_web_base_url` is new, `illiad`-only
  configuration for the patron-facing ILLiadWeb portal — distinct from
  `base_url`'s Web Platform API, and never derived from it, because
  shared-server deployments and customized directories make that guess
  unreliable. When configured, it compiles a new gate-profile capability,
  `fulfillment_channel = "patron_web"`, orthogonal to submission
  auto-capability: a profile can auto-submit requests and still have no
  fulfillment channel, in which case every fulfilled request still lands on
  the existing manual reconciliation action rather than claiming automation
  *papio* cannot back. On `fulfilled`, *papio* builds the form-75 "View
  PDF" URL (`patron_web_base_url` plus `?Action=10&Form=75&Value=<provider
  transaction reference>`) and carries it through the same
  `openurl_handoff` human-action machinery every other browser-driven
  candidate already uses — delegated drives it, assisted opens it,
  conservative only records that retrieval was discovered — so a
  downloaded file lands in the same adoption, quarantine, structural, and
  identity pipeline as any other browser capture. A custom, non-inline-PDF
  landing page is not heuristically scanned for a download link; that stays
  a recorded human action pending a fixture-backed adapter. **`fulfilled`
  still means "the provider supplied the document," never that *papio*
  holds trusted bytes** — only a file that clears validation moves the job
  to `ready`. Live acceptance needs a real ILLiad deployment and is not
  reproducible in CI; everything else ships now.
- **Triage snapshot schema 3 adds `attention`, routing detail, and delivery
  reconciliation.** Every item now carries a closed `attention` field —
  `working` (papio is proceeding on its own), `required` (a human decision
  is needed), or `advisory` (informational, e.g. a retraction notice) —
  replacing any UI inference from `action_kind` or `requires_auth`.
  Human-action items add `route_class` (a fixed enum formalizing the
  existing action-kind vocabulary, now including `document_delivery`) and a
  tri-state `auth_requirement` (`"true"`/`"false"`/`"unknown"`, wired
  separately from the existing boolean `requires_auth`, which keeps its
  narrow execution-gate meaning unchanged). `blocked_by`'s vocabulary grows
  to a v3 superset — adding `login`, `terms`, `delivery_outcome`,
  `identity_review`, `unknown` — without reinterpreting any v2 value;
  schema 2 emission stays byte-identical. A `document_delivery`
  human-action item carries a `delivery` sub-object (provider, provider
  reference, state) and three new operations —
  `open_request_history`, `confirm_request_exists`, and
  `confirm_request_absent` — wired end-to-end through a new
  `delivery_reconcile_request`/`delivery_reconcile_result` message pair. A
  routine store error while assembling an item's delivery detail now
  degrades that one field to absent with a logged line, rather than tearing
  down the whole native-messaging session over a database hiccup. Go's
  `confirm_request_absent` validation now matches the TypeScript and
  JSON-schema sides: an explicit empty `provider_reference` is rejected,
  not silently accepted. Store schema version 24.

- **Document delivery and ILL become a durable, configured route (ADR-0017).**
  A dead end is fixed: a work only obtainable through interlibrary loan used
  to make *papio* observe that fact and stop. A new `delivery_requests`
  ledger — idempotency-keyed on institution profile + canonical work
  identity + provider + request type, store migration 0021 — now records
  the request instead. `[browser.document_delivery]` and
  `[browser.resolvers.<name>.document_delivery]` declare a `kind`
  (`openurl` | `libkey` | `illiad` | `custom` — `oclc` and `rapido` are
  named as intended providers but rejected until their adapters ship) plus
  the gate-profile facts *papio* cannot discover on its own (`legal_basis`,
  `patron_attestation`, `patron_fee_policy`, `monthly_request_cap`,
  `submit_policy`). Those declarations compile at config time into one
  institution-profile × provider × request-class gate profile —
  `auto_capable` | `prefill_only` | `invalid`, with a closed 13-code
  blocker vocabulary and recorded evidence — and a per-request seven-point
  gate (delegated access mode, `auto_if_unconditional`, a supported and
  configured request class, complete metadata, zero required human steps,
  the zero-fee policy, and cap headroom) decides submit vs. prefill for
  every request the profile is asked to place; any condition false or
  *unknown* routes to prefill, never a guess. `illiad` is v1's only
  auto-capable provider: an institution-issued API key that is never sent
  to, stored in, or observable from the extension or the browser wire,
  zero-patron-fee digital journal articles only, and one recorded live
  acceptance — a supervised submit-and-reconcile against the real
  deployment — before the compile can ever read `auto_capable`. Profiles
  declaring Australian `legal_basis = "copyright_act_s49"` compile
  `prefill_only` permanently, by statute rather than caution: the ceiling
  there is automatic prefill followed by one human declaration, and *papio*
  must never tick, script, or represent that declaration itself. New
  `papio delivery get|submit|cancel|history|confirm-exists|confirm-absent`
  commands (MCP-exposed automatically, per the existing command-derived
  facade); `jobs.get_v3` adds a delivery section additively, keeping
  `jobs.get_v2` ratified and byte-identical. A pending request parks its
  job in the existing `retry_wait` state under a new
  `document_delivery_pending` reason — it is not an open action, so
  `actions list` never misrepresents a self-driving poll as one — and only
  once four rounds of deterministic reconciliation (provider reference,
  *papio*'s own idempotency key, a patron request-list search, one delayed
  re-check) are exhausted does it become an `awaiting_human` action of the
  new `document_delivery` kind, offering `open_request_history` /
  `confirm_request_exists` / `confirm_request_absent` and **never**
  resubmission: *papio* must not submit a second request while an earlier
  one's outcome is unknown. `papio init` prints the compiled gate verdict
  before saving (`AUTO-CAPABLE` with its evidence, or `PREFILL ONLY` with
  the specific blocker), and `papio doctor` keeps `DECLARED` configuration
  and `PASS`/`OBSERVED` verified facts strictly separate — it never creates
  a probe request and never prints `PASS` for a policy it merely read from
  config.

- **The daemon side of on-page bulk acquisition ships:
  `page_bulk_acquire_v1` (ADR-0019).** A newly negotiated feature carries
  two strict request/reply families — `page_bulk_status_request` /
  `page_bulk_status_result` and `page_bulk_submit_request` /
  `page_bulk_submit_result` — as thin transport adapters over the existing
  ownership/holdings lookup and `internal/batch` services, adding no
  browser-specific acquisition policy: a page-bulk job enters the same
  waterfall as any CLI- or MCP-submitted job, including LibKey-routed
  institutional handoff where configured. Status resolves up to 200 scanned
  identifiers through *papio*'s own canonicalizers and existing
  live-job/terminal lookups into a closed vocabulary (`eligible`,
  `owned_with_pdf`, `owned_missing_pdf`, `queued`, `previously_unavailable`,
  `ownership_incomplete`, `invalid`); a complete holdings lookup finding
  nothing is `eligible`, and a failed or partial one is
  `ownership_incomplete`, never a negative ownership fact. Submit creates
  one ordinary batch of up to 50 canonical keys through the same
  app-service entry `acquire.submit_v3` uses, with the daemon — never the
  extension — assigning `consumer = "browser-page"`; only a fresh, explicit
  `owned_with_pdf` claim is suppressed server-side, matching the extension's
  own row states. Every scan additionally writes one local, URL-free
  `page_bulk_runs` measurement row (store migration 0022; origin only, no
  path, query, or title — nothing leaves the machine) feeding the feature's
  own bulk-leverage metric.

- **`papio export` writes normalized CSL-JSON, RIS, and BibTeX.** Four
  scopes: `export job <id>...` (argument order, any state — citation
  metadata stays useful when retrieval failed), `export batch <batch-id>`
  (manifest order, including skipped and unavailable works — citation
  export is not an acquisition-success report), `export watch <watch-id>`
  (pending digest entries), and `export ledger` (one record per canonical
  work; ready acquisitions by default, `--state any`, `--since`,
  `--consumer`). Duplicates collapse by canonical identity — DOI, then
  PMID, then arXiv id, then normalized title/first-author/year — unless
  `--include-duplicates`; BibTeX keys are stable
  (`firstauthor-year-titleword-<identity-hash>`), so a small title
  correction never renames an entry. The projections are normalized, never
  a round-trip: only known values are exported, author names stay literal
  rather than being split into family/given by guesswork, and type is
  identifier-based only (ISBN without DOI or container = book). `--format`
  wins, the `-o` extension infers (`.json`/`.ris`/`.bib`), CSL-JSON is the
  default; with the global `--json`, `-o` is required and stdout carries a
  receipt (format, record count, duplicates collapsed, SHA-256, path). The
  MCP facade exposes the new command automatically. Projected values pass
  through the repo's one terminal-control filter, so a third-party title
  cannot inject escape sequences into the no-`-o` stdout path; PMID rides
  CSL 1.0.2's standard `PMID` variable (only arXiv uses the `custom`
  object). `export watch --include-consumed` from the consult design is
  deferred: consumed digest entries never leave the daemon today, so
  including them needs a `watch.digest` v2 method first.

- **OpenAIRE joins the open-access resolver waterfall.** The Graph API's
  research-product lookup by DOI (else PMID) contributes up to three
  candidates per work from the record's licensed or explicitly OPEN
  instances — European institutional repositories being the marginal
  coverage. OpenAIRE marks no URL as the file itself, so every candidate is
  a landing observation and the existing landing-expansion step derives the
  PDF when the page advertises one; a record that is not OPEN, an instance
  without a license or OPEN access right, and an echoed identifier naming a
  different work are all skipped. Keyless access is paced at OpenAIRE's
  public 60 requests/hour (`rate_per_sec = 0.016`); a personal-token
  `api_key` raises the ceiling. OpenAIRE Graph metadata is CC-BY and is
  acknowledged in candidate provenance and the privacy table. Verified
  live: a PLoS DOI returns its licensed publisher instances, query strings
  redacted in evidence.

- **The version hop follows Crossref's typed relations before any fuzzy
  search.** When a DOI's own candidates are exhausted, papio now asks
  Crossref for the registrant-asserted `has-preprint` / `is-preprint-of` /
  `has-version` / `is-version-of` edges (depth one, capped at three, through
  the existing `crossref_metadata` source and its budget) and resolves those
  sibling DOIs through the enabled open-access resolvers — a typed edge was
  asserted by the registrant, so it outranks the OpenAlex title-match hop,
  which now runs only when no typed edge produced a candidate. Typed sibling
  candidates are open-access only (routing a *different* DOI to an
  institutional resolver would sign the operator into the wrong work's
  paywall), are identity-checked against the sibling DOI they were resolved
  from, and still cross PDF semantic-identity validation against the
  requested work before acceptance. A rate-limited relation lookup parks the
  job like any other temporary source failure; it never settles it
  unavailable. Verified live: a paywalled Nature article resolves its typed
  medRxiv preprint in one hop.
  The extension derives each job's institution origin from the offer's
  provider hosts when LibKey fronts the route (the offer origin is
  `libkey.io`, which the daemon's resolver origins never contain), so
  first-sign-in queue release, `auth_returned` session evidence, and
  Alma/Primo resolver auto-routing keep working under link mode; and the
  default profile refuses `link` mode without an OpenURL base — the handoff
  gates key on the base, so a base-less LibKey config would sit validated
  but unreachable.

- **A browser adoption whose session was freshly evidenced as authenticated now
  publishes its entitlement (ADR-0018).** `acquisition-bundle/2`'s
  `operator_browser_session` acquisition mode has had no producer since it was
  reserved; the delivery context added in 0.17.0 supplies one. An exported
  bundle now carries `candidate.entitlement` when the accepted candidate's own
  row records `session_evidence = fresh_auth` on an institutional route, naming
  the page origin the extension observed at adoption. The claim is precisely
  that the bytes arrived through a browser session evidenced as authenticated at
  that origin — not that a login happened during the acquisition, and not that
  the work was paywalled. The gate is the recorded evidence, never the access
  basis: `warm` means the evidence aged past its TTL with nothing confirming the
  session since, a resolver-produced `institutional` candidate has no browser
  session behind it at all, and an adoption that carried no context (a
  directory-scan adoption always does) has an empty binding. All three stay
  entitlement-less, because a false positive here invents rights evidence while
  a false negative costs only a field (ADR-0007). Nothing is backfilled. An
  `oa`-route adoption also gains a route it previously lacked, still as
  `open_access` with no session claim. The bundle shape, schema, and IPC surface
  are unchanged — the mode was already in the v2 enum and the route is already a
  bare origin — so no consumer change is required.

- **Institutional handoffs can route through LibKey's institution link
  (ADR-0016, keyless `link` mode).** A resolver profile — the default
  `[browser]` institution or any `[browser.resolvers.*]` entry — gains
  `libkey_mode` and `libkey_library_id`; with `libkey_mode = "link"`, a
  handoff for a work with a DOI or PMID opens the documented
  `libkey.io/libraries/<id>/<doi-or-pmid>` institution link instead of the
  bare OpenURL resolver, which routes through the library's LibKey
  configuration (direct PDF, resolver, or request path) with no credential
  involved. LibKey augments institutional routing, never replaces it: works
  without either identifier, profiles without a library id, and every
  failure fall through to the plain OpenURL route, and both the CLI's
  `actions open` and the extension offer take the same route from the same
  builder. The offer keeps the resolver host beside `libkey.io` on its host
  list, so the extension stays sighted across the redirect. Config is
  fail-closed per this repo's strict-mode rules: `libkey_mode = "api"` is
  rejected as unimplemented, `link` without a positive `libkey_library_id`
  is rejected, and a library id without `link` mode is rejected rather than
  left silently dead. `papio init` collects the library id — a bare number
  or a pasted BrowZine/LibKey.io URL — interactively after the resolver
  step, or via `--libkey-library-id`; an explicit blank clears the pair.

- **Semantic Scholar now contributes open-access acquisition candidates, not
  just search results.** The provider papio already queried for discovery joins
  the resolver waterfall between OpenAlex and CORE: an exact-identifier lookup
  (DOI, arXiv id, or PMID — never a title search, because a weak title match
  must not become an automatic candidate) that emits one candidate only when
  the record carries a usable `openAccessPdf` URL. `isOpenAccess` without a
  PDF location is treated as metadata, an echoed identifier that names a
  different work rejects the record, the stated license is carried through
  (`unknown` otherwise — a reachable PDF is not a redistribution right), and
  the version stays `unknown` because the API supplies no typed version
  evidence. `[sources.semanticscholar].enabled` gains the documented meaning
  "may also contribute OA acquisition candidates" (default on, paced at the
  keyless 1 req/s public limit; discovery backend selection remains separate
  in `[discovery]`). The privacy table now lists `api.semanticscholar.org`
  under resolving, and 429s defer the source by the server's `Retry-After`
  like every other resolver.

- **`papio actions open` takes a row selector, so a consumer can open the
  handoff it chose.** The command opened the head of the queue and nothing else,
  which is the wrong shape for a caller that ranks its own routes — one
  consumer ranking 164 queued institutional routes by script reach could only
  ever reach the newest. `--job <job-id>` and `--action <id>` open exactly that
  row, and a job holding several open actions is refused with their ids rather
  than resolved by picking one. A selector naming no open action is a clean
  error naming where to look; it does not claim to know whether the row was
  resolved or never existed, because the open-action list it reads cannot tell
  those apart. What it never does is fall back to the head of the queue, which
  would open an unrelated institution's handoff and report success.
- **Acquisitions can carry a consumer name, and listings can be partitioned by
  it.** `work_requests.requester` records the transport principal (`cli`,
  `mcp`, `unknown`), which answers "how did this arrive" and never "who asked
  for it", so a daemon shared between people produced one undifferentiated
  total. `papio acquire --consumer <name>` (also with `--batch`) records the
  submitter through the new `acquire.submit_v3`; `papio jobs list --consumer`,
  `papio actions list --consumer`, and the new `jobs.list_v3` /
  `actions.list_v3` / `jobs.get_v2` methods return and filter it. Attribution is
  nullable with no backfill and no default: a request that named no consumer has
  none, the key is absent rather than empty, and a submission matching an
  in-flight job never rewrites the attribution that job was queued with. It
  binds to the job rather than the work request, so resubmitting a request id
  whose earlier jobs are terminal attributes the new acquisition to whoever
  resubmitted it. A `--consumer` filter against a daemon that predates the
  column is refused rather than answered with every consumer's rows. It is a
  caller's own accounting label: *papio* authenticates nobody, so it is not an
  identity and must never be read as a rights input (ADR-0014). Schema
  version 20. On landing, `acquire.submit_v3` was ratified alongside the
  readers (ADR-0014 Decision 4 addendum): its consumer asked, which was the
  condition Decision 4 set.
- **`papio artifacts validation <job-id>` returns the complete validation
  report.** Every stage's evidence was computed and then discarded: only the
  projections that fit the artifact row survived (page count, text characters,
  OCR use, encryption, active content, identity result), so the payload gate's
  reason, the structural rejection reason, and the identity and capability
  evidence were unrecoverable, and a consumer making a rights or quality call
  had to re-derive them from fragments. Each validation now persists a versioned
  `validation-report/1` document keyed to the job and candidate — for rejected
  candidates too, which is the set "why not this one?" is asked about. The
  extracted text excerpt is deliberately not persisted, and every reason and
  evidence line is bounded and stripped of control characters: several are a
  third-party parser's stderr produced while reading a publisher-supplied file.
  `artifacts.get` is unchanged: it returns the shared, content-addressed artifact
  row, and ADR-0007 forbids projecting one job's identity decision through it.
  Jobs validated before this release list no reports; that is an absence, not an
  empty verdict. This reverses one clause of ADR-0007, which had withdrawn
  structured validation evidence from the external surface; ADR-0014 records why
  the reasoning behind that withdrawal — no per-job identity through a shared
  artifact — is satisfied rather than evaded by keying the evidence to the job
  and candidate. The accepted candidate's bundle remains the only success
  provenance document.
- **`papio actions open` now leaves an audit trail.** Each opened handoff records
  a `handoff.opened` event carrying the owning consumer, the transport principal,
  and the batch size. ADR-0009 does not ratify autonomous drain — a background
  consumer must not open human work on its own — and ADR-0014 Decision 6 declines
  to enforce that with a gate, because a script passes any flag a human passes and
  an agent driving the CLI is meant to get exactly what a human gets. That trade
  is only honest if the boundary is observable: "consumer X opened N human actions
  in M minutes" is now answerable from the event stream, and a drain reads as an
  anomaly rather than as normal traffic. The event names the handoff's owner,
  recorded at submit, rather than an unverifiable label supplied by the caller
  under audit.
- **An open human action that has waited too long is reported stale.** A
  handoff queued weeks ago sat in the queue indistinguishable from one queued
  this morning. `papio actions list` now reports `stale` and `age_seconds` per
  row (a trailing marker in the text listing) against the new
  `actions.stale_after_seconds` config key, default 7 days. Nothing expires as
  a result: *papio* does not cancel, sweep, or close a handoff on a timer,
  because abandoning an acquisition is a person's decision. The threshold is
  separate from `browser.action_expiry_seconds`, which is a 30-minute reminder
  cadence and would have called a handoff queued over lunch abandoned.

### Changed

- **A paper now has to print the requested title, not merely contain its
  vocabulary.** Title identity was unordered token membership — stopwords and
  every word under five runes discarded, 60% of the remainder enough to pass —
  and the corpus harness showed all 52 of its wrong accepts came through that one
  gate, in three families. "How to do a meta-analysis" reduces to the single
  token `analysis`, which matched eight unrelated papers by an author of the same
  surname. "Final Report - Volume 3, Impacts" discards the `3` for being one rune
  long, so eighteen pairs of a seventeen-volume government report matched each
  other on the words they share. And a set cannot tell "Core reporting practices
  in structural equation modeling" from "Update to core reporting practices in
  structural equation modeling", which matched 5/5.

  A pass now also requires the title to be printed as a delimited unit in the
  front matter: the whole requested title, every short word and stopword
  included, beginning where a line begins or a label of at most three words ends
  ("Original Article:", "1."), ending where the line ends or punctuation does,
  and spanning consecutive lines where it wraps. A hyphen broken across the line
  break rejoins first, and a running head glued on with a run of spaces is split
  off, so the shapes a text layer actually produces still match.

  `titleThreshold` therefore means something different: it is the floor below
  which a candidate is rejected, not a level at which a partial title match
  passes. A 3/5 match with a matching author and year is now review where it used
  to be a pass — that combination was the single largest wrong-accept family.
  Measured over 632 documents and 398,786 mismatched pairs: wrong accepts 52 → 2,
  correct passes 586 → 565. The title is compared as one character run rather
  than word by word, because a text layer decides where words begin and decides
  badly — "PsychologicalSafety and LearningBehavior in WorkTeams" is one real
  paper's printed title, and a footnote digit welded to the last word is another
  — and a digit-only run inside a wrap is stepped over, because a submitted
  manuscript numbers every line. Only a colon, full stop, or rule ends the short
  label the start edge allows: a citing sentence reaches for a quote, and
  accepting one let `We cite "…" for guidance` pass as a printed title.

  The 21 documents that moved to review are the ones token overlap can never
  separate, and every one is accounted for in `dev/identity-corpus.md`: eight
  numbered-series covers that print the catalogue's words in another order, five
  text layers shredded by column interleaving, three catalogue records that
  disagree with the printed title, two that concatenate a teaser or drop a
  publisher prefix, one mojibake dash, and two differing subtitles. papio parking
  a correct paper costs a moment where filing the wrong one costs a library its
  trust.
- **`make identity-corpus` no longer grades the identity rules on papio's own
  output.** 47 of the 679 documents it scored resolved, through Zotero
  linked-file attachments, into papio's own artifact, bundle, and staging
  directories — 9 of them artifacts papio itself delivered and had already
  scored. Every one of the 47 passed, which is what recycled output looks like,
  and independence from it was the whole reason the corpus reads Zotero rather
  than papio's store. They are now excluded by name and counted in the report's
  new skip summary, alongside the long documents the 1 MiB extraction cap drops:
  24 of 54 books against 11 of 637 journal articles, so the corpus is the
  library minus most of its books and a book-shaped rule change cannot be judged
  on it. The report says so rather than leaving the reader to assume otherwise.
  A relative linked-file path is resolved against Zotero's own
  `extensions.zotero.baseAttachmentPath` instead of a directory that was never
  the base, and says so when the pref is unset rather than reporting a missing
  file. The identifier-placement histogram asks the real matcher where an
  identifier is printed, so the six documents whose DOI is stored as an EZproxy
  URL are now reported as unusable metadata instead of as a window that needs
  retuning. The library snapshot prefers an atomic `VACUUM INTO`, falls back to
  a byte copy while Zotero holds the database open, retries a copy the WAL grew
  under, and names which path it took. The extracted-text cache — the front
  matter of every paper in the library — now defaults to a per-user cache
  directory rather than shared `/tmp`, refuses a directory it does not own,
  writes each entry atomically, and is documented as retained until deleted.
  Skip lines name an attachment key and a class, never a path or a filename.
- **The privacy policy now says what the daemon actually sends.** It claimed
  "*papio* collects no personal data. Nothing is sent to OrgMentem or any third
  party" while scoping itself to the daemon *and* the extension. That is true of
  the extension, which has no backend, and false of the daemon: finding a paper
  means asking the services that index papers, so every acquisition sends the
  DOI, PMID, arXiv id or title to Unpaywall, Crossref, Europe PMC, arXiv and now
  doi.org — and, where those services require it, the `email` from the config.
  The daily retraction sweep additionally sends the DOIs of papers already in
  the library. `docs/privacy.md` now carries a destination-by-destination table
  naming what is sent, when, and whether it is on by default; the
  `email` row in the config reference names its recipients; and the "no server,
  no account, no telemetry, no analytics" claim is kept, because it is true, but
  stated separately from the claim it was hiding. The once-a-day GitHub release
  check is disclosed rather than glossed: it sends nothing, and
  `updates.check = false` turns it off.

### Fixed

- **One blocked syscall no longer freezes every daemon RPC.** macOS
  Files-and-Folders consent (TCC) can leave `open(2)` on the
  download-adoption root blocked in-kernel for a background daemon — and the
  consent resets every time the binary is rebuilt or upgraded. Adoption
  scanning previously ran that syscall while holding the browser-bridge lock,
  so a single consent wall hung every RPC (`papio daemon status` included)
  until a force-kill. Scans now run through a bounded reader with a
  bridge-wide latch: the daemon stays fully responsive, offers and sessions
  keep flowing, and scanning resumes on its own the moment the grant lands.
  `papio doctor` gains an adoption-root check — bounded the same way, so
  doctor itself can never hang on the wall — that names the folder and the
  System Settings remediation instead of leaving a silent stall.
- **An open-access paper whose cached PDF link has rotted is now recovered from
  the publisher's own landing page instead of being sent to your institution.**
  Aggregators cache a direct PDF URL, and those URLs expire: for one Frontiers
  paper both Unpaywall and OpenAlex returned the same Azure blob link whose
  signature ran out on 2021-02-16, so both candidates 403'd and the job fell
  through to an institutional sign-in — for a paper anyone can download without
  one. Both candidate rows already carried the publisher landing page, and that
  page advertises the working file in `citation_pdf_url`; papio had the recovery
  route in hand and never looked at it. When every open-access candidate fails
  permanently, the landing page is now read (once per pass, before the sibling
  hop and before any retry park, so a slow unrelated resolver cannot delay a
  deterministic recovery) and the file it advertises becomes a derived
  candidate. The derivation inherits its parent's source, access basis and
  policy — it is the same observation reached a second way, not a new source, so
  no source needs enabling and no configuration changes. The reader sends no
  caller credentials, because carrying a signed candidate URL's headers to a
  different origin would leak them.
- **A browser handoff that cannot succeed now stops asking.** The only brake on
  automatic offers was the seven-day `QuiesceAfter` fence, so a handoff that
  could never complete kept costing a tab: one was offered 38 times across three
  days with no outcome of any kind, pinning both browser drive slots and
  starving every other job of them. Offers are now also bounded by evidence —
  three fruitless drive epochs and papio stops offering unprompted, recording a
  single `browser.handoff_quiesced` audit event. Reconnects are not evidence: a
  service-worker restart re-acknowledging the same live drive collapses into the
  epoch already running, so recovering transport is never mistaken for failure.
  Both fences apply, the action stays open and visible, and an explicit
  `papio actions open` always gets its drive.
- **A parked job no longer reports a cause it does not know.** Every retry park
  was labelled `candidate_temporarily_unavailable`, including passes where the
  candidates had failed permanently and something else entirely — a temporary
  resolver, a closed source gate — was holding the job. The transition now
  reports what was actually observed, counting retryable candidates, temporary
  resolvers and closed source gates separately. Scheduling is unchanged; only
  the explanation is. The former reason string still classifies, because events
  already written carry it.
- **An open-access page behind a public anti-bot gate keeps its browser route
  after the first pass.** papio detects that such a candidate is worth one
  ordinary-browser attempt, but the finding lived in a variable scoped to the
  pass that made it. Once the candidate was marked failed, later passes found an
  empty queue, never re-evaluated it, and routed the job to an institutional
  sign-in instead — papio noticed the right answer and then forgot it. The
  finding now survives as a job event recording only the candidate's key, never
  the URL, and the live URL is recovered from the current pass's own candidates.
- **A DOI that does not exist no longer parks the job on an institutional
  sign-in.** A well-formed but unregistered DOI — one transposed digit in
  `10.1016/j.cedpsych.2020.101816` for `…101860` was the reported case —
  survived every upstream check, because Crossref, OpenAlex, EuropePMC and
  Unpaywall all report "I have no record of this" and "this work exists but I
  hold no open copy" through the same empty result. The job therefore reached
  the institutional handoff, the link resolver had nothing to match, and the
  user landed on doi.org's "DOI NOT FOUND" page. That action can never be
  completed, so it was re-offered on every session-live tick — roughly sixty
  tabs over three days — and escalated seven reminders. Before offering a
  handoff for a work whose only fetchable identifier is a DOI, *papio* now
  asks the DOI system itself whether that handle is registered; an
  unregistered one settles `unavailable` with the new terminal reason
  `doi_not_registered`, which names the typo and says plainly that signing in
  will not help. The probe is skipped when a PMID, arXiv id or OpenAlex id is
  also present (each is its own route) and fails open when the registry is
  unreachable, so an outage cannot terminate fetchable work. Like
  `no_identifier`, the new reason is exempt from the zotio backfill cool-down,
  because correcting the DOI makes the item fetchable immediately.
  The same gate now runs in the maintenance repair pass, so a job that was
  *already* parked when this shipped is reclaimed rather than nagging forever;
  the existing "contradicted park" rule could never reach it, because that one
  waits on a `browser.no_entitlement_requeue` event and a dead DOI never gets
  as far as the institutional resolver. Registry answers are memoized so the
  once-a-minute sweep does not become a request per parked job per tick.
  Two hardening notes on the probe itself, both found in review: the request
  path is built by concatenation rather than `path.Join`, because Join Cleans —
  which collapsed the repeated slash that makes `10.48612//monograph-2025-2` a
  different registered work from `10.48612/monograph-2025-2`, and let a `..`
  segment (`doiCoreRE` admits any non-whitespace suffix) escape `/api/handles/`
  into doi.org's own resolver root; dot segments are now rejected outright. And
  a missing HTTP client is an error rather than a plain `http.Client`, which
  would have silently dropped the SSRF guard, redirect cap and body bound the
  daemon's shared metadata client provides.
- **An open human action stops nagging after a week.** The reminder backoff
  capped the *interval* at 24h but never the *count*, and the browser bridge
  re-offered every open handoff whenever a session went live, so one handoff
  nobody could complete produced a tab per session and a notification per day
  indefinitely — sixty tabs and seven reminders over three days in the reported
  case. Some of those causes are now caught at the source, but most never will
  be: a title the library does not hold, a provider that changed its login, a
  job the user has decided to ignore. Past `job.QuiesceAfter` (seven days) an
  action goes quiet — no automatic offer, including on the institutional
  re-offer sweep that a fresh sign-in triggers, and no further reminder.
  This is deliberately **not** expiry: the action stays open, stays listed, and
  `papio actions open` still drives it, because an explicit command is user
  intent in a way a session-live tick is not. `papio doctor` gains a
  `quiesced_actions` check so a quiet queue does not become an invisible one —
  out of band on purpose, since the IPC layer decodes strictly and widening an
  existing result shape would make an older CLI reject every response from a
  newer daemon.
- **A paper whose catalogue record names one author no longer goes to review
  over a superscript.** Identity accepts an identifier printed anywhere in the
  document, but only once the byline agrees on an author — and where a
  superscript affiliation marker is glued to the surname ("Keith D. Ciani1∗")
  that agreement needs *two* marked surnames, because the two-character
  tolerance cannot tell a marker from a different name. A record naming a
  single author can never show two, so the rule was unsatisfiable: a Wiley PDF
  matching 7/7 title tokens, the year, and printing the requested DOI verbatim
  below its abstract — past the front-matter DOI window, which stays narrow so
  a reference-list DOI is never read as the document's own — was parked for
  human verification. A *numbered* marker on the requested surname now carries
  the author check by itself where the record names one author, because no
  surname ends in a digit: "Ciani1" is Ciani, whereas "Clarke" is Clark plus a
  letter or a different person, and nothing in the text says which. The
  identifier must also be printed on the document's own page one. Both bounds
  answer the same document — a comment, reply, or erratum on the requested paper
  carries its title and prints its DOI, so on its own the identifier does not
  establish that a file is the paper rather than a note about it. A lettered
  near-miss surname still parks, as does a numbered one whose only identifier
  match is a citation past page one.
- **A page that announces itself as a correction can no longer be filed as the
  paper it corrects.** An identifier printed in the first kilobyte of page one
  passed identity outright, with no title or author check — correct for the 40%
  of real papers that print their own DOI in the masthead, wrong for an erratum,
  corrigendum, retraction notice, or comment article, which is a different work
  that reprints the requested paper's DOI at the top of its own first page. A
  1508-byte correction notice was accepted as the paper. A page-one line
  beginning with one of those markers now caps the verdict at review rather than
  rejecting it, because the operator may have asked for the erratum and a park
  can be undone where a discard cannot, and the marker is named in the evidence
  on every verdict a human sees. Detection reads the 2 KiB byline window, not
  the kilobyte the DOI rule reads, and tests each line in the segments pdftotext
  leaves when it glues a running header or a page number onto the line that
  carries the marker — `"J Sensor Syst 2025;12:1  Erratum: …"` escaped a
  line-prefix test entirely, as did a leading byte-order mark. Runs of two or
  more spaces are what segment a line, so a Bonferroni correction mid-sentence
  is still a real paper, and the list carries "retraction of" rather than
  "retraction" and omits "response to". A pointer to a correction published
  elsewhere — Springer prints "Erratum to this chapter is available at …" on the
  corrected chapter itself — is excluded by name, so the corrected work does not
  park itself.
- **A PDF that opens with a blank cover leaf is read instead of half-read.** The
  windows every identity rule reads were cut at the first form feed, so a
  leading one handed all three of them an empty page one: the DOI rule saw no
  DOI, the title gate saw no byline, and a paper that matched on all four axes
  parked with "title tokens matched only outside the front matter". Leading
  whitespace, form feeds, and byte-order marks are now trimmed before the cut.
  Two documents in a 632-document real library were parked by this and now pass.

- **`papio jobs list` and `papio actions list` state truncation on the human
  surface too.** `--json` has carried a proven `truncated` since 0.16.0, while
  the text listing stopped at the limit and looked complete. Both now say how
  many rows they showed and that more exist behind the page. The `--json`
  envelope is unchanged at exactly two keys.
- **`papio acquire --batch --request-id` no longer points at a mechanism that
  does not exist.** The flag was refused with "put per-work values in JSONL",
  but the JSONL work decoder is strict and has no `request_id` field, so
  following that advice failed with `unknown field "request_id"` and left no
  working option. One flag cannot key many works: batch works get deterministic
  per-work request ids derived from the batch identity and the work identity, so
  resubmitting the same works on the same day reproduces the same keys — the
  batch identity mixes in the calendar date. The refusal now names that instead,
  and calls `request_id` what ADR-0010 calls it: a live-job convergence key, not
  an idempotency key.

## [0.18.0] - 2026-08-06

### Added

- **`page_capture_terms_v1` capability.** The `terms` capture scenario was
  appended to the *existing* `page_capture` scenario enum, so a daemon that
  predates it rejects the whole frame during validation — and a browser
  protocol decode failure tears down the entire native-messaging session, not
  just that request. With two papio binaries on one machine a routine
  occurrence, the daemon now advertises a dedicated capability and the
  extension withholds the option until it sees it. `terms` remains a valid
  value in all three validators; only the producer is gated.
- **`page_capture` carries an optional `request_id`.** A requested capture now
  echoes the `request_id` of the `page_capture_request` it answers, which is
  the only thing that ties the content frame to a specific request. An
  unsolicited capture — the developer capture panel's own button — omits it.
  This is a breaking wire change to an existing message type, landed under the
  pre-1.0 compatibility floor now stated in `AGENTS.md`: daemon, extension, and
  JSON schema move together and the extension must be rebuilt and reloaded
  alongside the daemon. Correlation needs an extension of 0.10.0 or newer;
  below that `papio adapter capture` now refuses up front, naming the connected
  and required versions, rather than capturing the page and then reporting a
  failure it did not have. Only adapter capture is version-coupled, so the
  daemon's own extension floor does not move and every offer and handoff keeps
  working with an older extension.
- **The native host mirrors its diagnostics to `<DataDir>/native-host.log`.**
  Browsers forward a native-messaging host's stderr nowhere — not even into
  `chrome_debug.log` with logging enabled — so a host that rejected a frame and
  tore the session down left no trace, and the operator saw only a downstream
  `nav_failed` or a session that would not connect. Every diagnostic the host
  writes, including the exact reason it exited, is now appended to that file.
  Past 1 MiB it rotates to `native-host.log.1` rather than truncating, so a
  second host process — a service-worker reconnect, or Chrome and Firefox
  connected at once — cannot discard the trace a live sibling is still writing.

### Fixed

- **The quarantine preview re-verifies the file on every serve.** The digest
  was checked once and the result cached on the capability, so any later GET
  of the same URL re-opened the file and served it with no hash check at all —
  a different read from the one that was verified. A quarantined file is
  untrusted by definition and its path is known to the process that wrote it,
  so a swap between the operator's first look and a reload served bytes nobody
  had checked. Verification now runs per serve, from the same handle the
  response is written from, and a mismatch revokes the capability. The hash
  runs outside the server mutex: a PDF viewer issues a range request per chunk,
  and holding the lock across a full file read would serialise every preview in
  the process behind the largest document.
- **`papio activity --json`, and every other `--json` payload, can no longer
  carry a terminal escape.** Go's JSON encoder escapes only bytes below 0x20
  plus quote and backslash, so DEL and the entire C1 block reached the writer
  raw — and a UTF-8 terminal reads U+009B and U+009D as the CSI and OSC
  introducers, the same injection primitive as ESC without an ESC byte in the
  input. A provider-supplied download filename or a third-party bibliographic
  title could therefore inject escape sequences into the terminal of anyone
  piping or eyeballing the output, even though the human-readable rows were
  already stripped. Those code points are now emitted as `\uXXXX`, which every
  conformant JSON parser decodes back to the original value: `--json` stays
  byte-exact for tooling, which stripping would have broken.
- **A page capture can no longer be bound to the wrong request.** The daemon
  matched an incoming capture to a pending `papio adapter capture` on provider
  and scenario alone, so an unsolicited capture of the same pair on the same
  browser session could overwrite the pending request's path and return the
  wrong file to its caller, with no error surfaced. Correlation is now the
  echoed `request_id`, the same key the capture result already used, with no
  provider/scenario fallback.
- **A capture that arrives as its deadline expires is reported as a success.**
  The result channel and the timeout became ready together and Go's select
  chose between them pseudo-randomly, so a capture that had already been
  delivered and stored could still be reported as a timeout — leaving the file
  on disk with nothing pointing at it. The timeout path now re-checks the
  channel under the same lock a deliverer takes, so whichever side arrives
  first wins outright.
- **`papio batch report --markdown` strips terminal control bytes too.** The
  markdown flag renders through its own `describe` helper rather than the
  plain-text path that was hardened, so the same third-party manifest titles
  still reached the terminal raw through a sibling flag of a command already
  listed as covered.
- **Every command that prints third-party text strips terminal control
  bytes.** Bibliographic strings are stored with only `TrimSpace`, so a
  record whose title or DOI carries an OSC sequence could rewrite the
  operator's terminal. `papio search` is the most exposed — anyone able to
  register a work in a discovery backend controls those bytes, and any
  keyword search renders them — alongside `watch digest`, `inbox` (both the
  watch-hit and retraction rows), `status`, `jobs list`/`jobs get`, `batch
  report` and the activity feed. Note that a DOI is not self-sanitising:
  `doiCoreRE`'s `\S` excludes only the five ASCII whitespace bytes in RE2, so
  ESC, BEL, DEL and C1 all survive normalisation. `--json` output is
  deliberately unchanged and still carries exact bytes, since it is the
  authoritative machine-readable form.
- **A page capture that redirects is no longer reported as a failure.** The
  previous release matched a pending capture against the host of the URL it
  requested, but the extension reports the host the page actually *landed* on,
  so an ordinary `www` canonicalization, CDN swap or SSO round-trip left the
  capture uncorrelated and downgraded a stored, successful capture to
  `nav_failed` with no path back to the file. That guard is reverted and the
  episode recorded in the code, because the residual it was aimed at is much
  rarer than the failure it caused.
- **A concurrent review submission is no longer told the verdict was recorded
  before it was.** A second POST arriving while the first was still inside
  `ResolveReviewCAS` was answered with the recorded shell — buttons disabled,
  no retry path — even though nothing had committed yet. If that first
  resolution then failed, the review was silently lost. The handler now tracks
  a real pending/recorded state, answers a genuinely in-flight submission with
  retry guidance, and returns a failed resolution to a retryable state. The
  page decides whether to re-enable its buttons from the response content type
  rather than the status code, which had conflated the two conflicts, and it
  now also re-enables them when the request fails outright.
- **The failure-summary example job is chosen chronologically.** The most
  recently updated job in a group was selected by comparing RFC3339 timestamps
  byte-wise, and `store.Now()` omits the fractional part entirely when the
  nanoseconds are zero. Since `Z` sorts above `.`, a whole-second timestamp beat
  a genuinely later fractional one. Timestamps are parsed before comparison.
- **An out-of-enum candidate value errors instead of vanishing.** Migration
  0019 added CHECK constraints on `browser_route` and `session_evidence`, but
  `InsertCandidates` uses `INSERT OR IGNORE`, and SQLite treats a CHECK
  violation under OR IGNORE as "skip this row" — so a bad value would have
  dropped the candidate silently while the dedupe the clause exists for went on
  working. The enums are validated in Go before the insert. No writer sets
  either field today, so this closes a latent footgun rather than a live bug.
- **`page_host` and `origin_hint` validate identically in all three places.**
  The published schema accepted `page_host` values both parsers rejected
  (leading, trailing and doubled dots), and `origin_hint` disagreed three ways
  at once: Go accepted an uppercase host that the TypeScript URL round-trip
  rejected, while the schema alone rejected a single-label host both parsers
  allowed. One rule now applies everywhere: an https origin, with no userinfo,
  path, query or fragment, and a host that is already lowercase. Label count
  and host length are deliberately NOT constrained — a single-label intranet
  resolver such as `https://library` is a legitimate configuration, since
  `browser.openurl_base_url` requires only an https URL with a host, and the
  wire validator must never be stricter than a valid config can produce.
  Corpus fixtures pin each previously divergent value.
- **`papio adapter capture` reports a daemon upgrade instead of a raw RPC
  error.** Against an older daemon it surfaced the bare `unknown_method`
  failure rather than the actionable message every other versioned command
  renders. With two papio binaries on one machine documented as routine, that
  skew is an ordinary outcome.
- **A page capture can no longer be satisfied by an unrelated capture of a
  different host.** The pending-request match now includes the requested host
  alongside session, provider and scenario. A residual remains and is
  deliberately deferred: nothing on the wire distinguishes a solicited capture
  from one the operator triggers from the developer panel, so a same-host,
  same-provider, same-scenario capture can still bind. Closing that needs a
  negotiated `request_id` on `page_capture`, which is an existing message type
  and therefore cannot take a new field without capability negotiation.
- **A rate-limit gate set mid-flight can no longer be raced.** `budget.Acquire`
  checked the durable gate up front, then called `takeToken`, which may itself
  sleep for up to `MaxInlineWait` waiting on the in-memory bucket. `reserve`'s
  transaction never re-read `next_allowed_at` — it only ever *cleared* an
  already-expired gate — so a worker sleeping in that window committed its
  reservation and sent its request against a gate another worker's 429 had just
  persisted, defeating the guarantee that one 429 stops every caller of that
  source and credential. The gate is now re-read inside the same transaction
  that moves the counters, and a live gate defers the reservation instead.
- **The quarantined-PDF review shell refuses to be framed.** Its only controls
  apply an irreversible, CAS-guarded verdict, so any page holding the
  capability URL could have overlaid the shell and harvested a confirmation for
  a file the operator never looked at. The shell now sends
  `frame-ancestors 'none'` and an `X-Frame-Options: DENY` fallback; the
  extension only ever opens it as a top-level tab, so no legitimate embed is
  lost.
- **`download_id` must be at least 1 on the wire.** It is half of the
  `{job, download}` key the daemon correlates delivery provenance on, and all
  three validators accepted `0` — so two downloads reported as `0` for one job
  collided, letting a `delivery_context` for the first apply its access basis to
  the second, unrelated candidate. Chrome allocates download ids from 1, so no
  genuine client is affected; this closes the hole against a buggy or
  compromised one. Tightened in the Go validators, the TypeScript mirror, and
  the JSON schema together.
- **Session evidence with no origin no longer releases another institution's
  parked handoffs.** `origin_hint` is optional on `session_evidence`, and an
  absent one was read as "match any profile": the fallback scan took the first
  institutional handoff in list order and reoffered that institution's whole
  queue. A sign-in at one institution could therefore reopen tabs and drive
  OpenURL navigations for a second one whose session was never verified — the
  opposite of the isolation per-institution resolver profiles exist to provide,
  and looser than an *unresolvable* hint, which already failed closed. An
  unattributable frame now releases only the default profile. Single-institution
  setups leave the job's resolver profile empty, which *is* the default profile,
  so their behaviour is unchanged. An unattributable frame also may not retire
  the sticky reoffer pin: demoting a named institution's pin to the default
  profile made that institution's own next `auth_returned` a no-op, starving
  its queue for the rest of the session. Only an attributable hint retires a
  pin, and an unhinted frame arriving while a named pin is live now does
  nothing at all.
- **`browser.resolvers.default` is rejected instead of silently ignored.** The
  resolver-name rule accepted the literal name `default`, but `InstitutionFor`
  resolves that name to the implicit top-level institution before ever
  consulting the map — so the profile was unreachable. A job meant for that
  institution routed to the top-level OpenURL base, `ResolverNames()` returned
  a duplicated `default`, and with no top-level base configured
  `browser.default_resolver = "default"` failed with the self-contradictory
  message `not configured (configured profiles: default)`. The name is now
  refused at load with an error naming the reserved key, matching how
  `[sources.*]` already whitelists its own names. No existing config breaks:
  nothing in `papio init` or `config save` ever writes that key.
- **`papio activity` cannot be used to inject terminal escape sequences.** The
  summary sanitizer stripped only NUL, while the browser-reported download
  filename it interpolates is bounded by a regex that forbids just the path
  separators — so ESC and every other C0 byte reached the operator's terminal
  verbatim. A provider page, a spoofed `Content-Disposition`, or a compromised
  browser session could rewrite the terminal on the next `activity` run. Every
  C0 byte, DEL, and the C1 block are now stripped at one exported choke point
  shared by the CLI row, the friendly summary, and the browser activity feed.
  C1 matters because a UTF-8 terminal decodes U+009B and U+009D as CSI and OSC,
  reconstituting an escape introducer with no ESC byte in sight. The job title
  printed beside the summary is covered too: it comes from third-party
  bibliographic metadata and was stored with only whitespace trimming, so it
  bypassed the sanitizer on the very same output row. The `--json` output was
  never affected.

## [0.17.0] - 2026-08-04

### Added

- **`papio doctor` reports native-host version skew.** One papio binary is CLI,
  daemon, and native host, but the native-messaging manifest can point at a
  different copy than the daemon that is running (a packaged install beside a
  local build, or a symlink an upgrade left behind). A stale host enforces its
  own older transport rules, so browser work keeps failing while the daemon
  looks healthy. Doctor now runs the executable the manifest names and fails
  when its version differs from the daemon's, naming the path and the fix.
  `papio-native-host --version` now answers directly, so the probe works whichever
  copy the symlink resolves to.
- **Quarantined PDF previews now include the decision at the point of
  inspection.** The loopback capability URL renders a citation-aware review
  bar above the PDF and records accept or reject through the same durable,
  revision-and-SHA-bound review transition used by the browser inbox. PDF
  bytes remain capability-bound at the `/file` sibling, and each preview
  capability permits only one decisive verdict.
  The review bar carries papio branding, follows the system light/dark
  scheme, and a recorded verdict closes its tab automatically (with a
  graceful "you can close this tab" fallback when the browser blocks it).
- **`papio adapter capture <url>` captures a provider page through the live
  browser session.** The new `adapter.capture_v1` RPC queues one solicited
  capture directive per browser session; the extension opens a governed tab,
  captures the sanitized DOM through the existing pipeline, and the CLI
  returns the structured outcome and stored path. Fixture collection for
  adapter work is now fully agent-drivable — no screenshots, no human clicks.
  The capture vocabulary gains the `informit` provider and a `terms` scenario
  for consent-wall fixtures, validated on both sides of the wire.
- **`papio failures` aggregates terminal and parked failure reasons** by
  reason (default) or provider (`--by-provider`) from each job's most recent
  decisive events, via the new `failures.list_v1` RPC with the usual `--json`
  envelope.
- **The CLI explains itself at three former dead ends.** `papio actions open`
  without a connected browser session prints what is missing and how to check
  (`papio doctor`) instead of a bare exit code; `papio doctor` names the exact
  remedy for a stopped daemon rather than only diagnosing it; and `papio jobs
  show <id>` is an exact alias of `jobs get` instead of a silent no-op.
- **`papio activity` exposes the daemon's recent operator activity.** The new
  `activity.list` RPC and CLI command show a bounded, newest-first view of the
  durable events table, with `--limit`, optional `--job` filtering, and the
  usual `--json` page envelope.
- **The browser activity feed is feature-gated and solicited.** The
  `activity_request` / `activity_response` messages and `activity_feed_v1`
  feature carry bounded display-only entries to an open inbox; this is a pull
  view, not daemon push.
- **Early browser delivery can adopt a PDF for a live job.** The extension
  steers the browser download into `papio/<job-id>/`, and the daemon adopts it
  through existing legal job transitions and the ordinary validation pipeline.
  Queued, resolving, and fetching jobs no longer dead-end when the operator
  already has the PDF in the browser; download races remain structured,
  retryable outcomes.
- **Activity summaries read the same everywhere.** `store.ActivityText` renders
  one friendly line per event kind for both `papio activity` and the browser
  activity feed — raw kinds like `action.reminder` or `browser.error` no
  longer leak to either surface, and the CLI line view drops the raw detail
  JSON (still available under `--json`).
- **Two institutions no longer share one session.** Browser session evidence
  carries the resolver origin that produced it. The extension releases only
  queued handoffs for that origin, and the daemon maps the origin to a
  configured resolver profile before re-offering only that profile's
  store-backed siblings — a second-institution sign-in never releases the default institution's queue on either
  side, and vice versa. Named `[browser.resolvers.*]` profiles now send their
  own `shibboleth_entity_id` / `proquest_account_id` on handoffs (previously
  default-profile only), and a new `browser.default_resolver` key selects
  which profile bare acquisitions use.
- **Contextless browser adoptions are recorded as `manual` access**, matching
  migration 0019's reclassification, and delivery context binds to the exact
  candidate its download created instead of whichever candidate existed.
- **Offer pacing is strictly oldest-first** across ordinary offers (not only
  re-offers), and the two-second browser poll uses one paginated joined query
  instead of one query per open handoff.
- **A verified institution session now unblocks parked institutional work.**
  The extension reports timing-only session evidence (`session_evidence_v1`),
  and the daemon re-offers parked institutional handoffs from *any* prior
  session — discovery is store-backed rather than limited to the current
  connection's offers. Manual-download actions are never auto-driven.
- **Keep-warm follows daemon demand.** Negotiated triage counts
  (`triage_counts_schema_v2`) carry the number of open actions requiring
  institution sign-in, so the browser keeps the session warm while parked
  auth work exists — not only while a handoff tab is open.
- **Adopted downloads record their true access route.** The extension attests
  route (resolver/direct/open-access), a sanitized page host, and session
  evidence per delivery (`delivery_context_v1`); candidates persist
  `browser_route`/`session_evidence`, and `access_basis` is derived from
  evidence — an uncontexted adoption is never marked institutional. This
  gives ADR-0010's reserved `operator_browser_session` basis its producer.
  Schema version 19.
- **Institutional handoffs are paced.** The bridge keeps at most four
  unsettled handoffs in flight per browser session regardless of backlog,
  re-offers at most four parked jobs per cycle (oldest first), and records a
  single `browser.offers_paced` event carrying how many are held back —
  bulk-releasing a hundred parked jobs now drains as a trickle, not a flood.

### Changed

- **Unsupported provider pages now become actionable instead of occupying a
  browser-drive slot indefinitely.** A stable resolver landing outside the
  capped provider list enters the same bounded evidence path. The extension
  can send its existing sanitized diagnostic and `ui_changed` outcome; the
  daemon creates a manual-download action that explicitly names the missing
  adapter and whether local evidence was retained. A missing browser grant is
  not terminal: the governed tab stays live and resumes after the operator
  allows that exact provider origin.
- **Provider-adapter evidence now has a documented contribution path.** The
  guide starts with automatic local gap captures, `papio adapter captures`, and
  `papio adapter diagnose`; it requires review and minimization before any
  fixture is shared, then identifies the declarative spec and focused checks
  needed for a code contribution.

### Fixed

- **Large page captures no longer kill the browser session.** The IPC request
  cap (64 KiB) was smaller than a legal 256 KiB browser frame, so any
  `page_capture` over ~63 KiB failed the native host's relay mid-sync; the
  host treated that as a fatal transport error and said goodbye, tearing down
  the live session and aborting the capture (`nav_failed: browser session
  disconnected during page capture`) and re-parking every in-flight handoff.
  The cap is now 512 KiB and a nativehost regression test pins the invariant.
- **A large focus batch can no longer overflow one sync response.** Handoff
  focus requests now drain in bounded batches, with the remainder riding the
  next ordinary poll, so a caller-supplied job-id list cannot push a
  `browser.sync` response past the IPC result cap — the same class of fatal
  transport failure as the request-side bug above, in the other direction.
  `TestSyncResponseFitsResultCap` pins the response-side invariant.
- **A failed adopted download no longer mislabels paywalled work as needing no
  sign-in.** The replacement manual-download action inherits the resolved
  handoff's authentication classification instead of a blank one.

- **A rate-limit gate or request count earned under one set of credentials no
  longer applies to another.** Providers meter by credential, not by source
  name: measured against OpenAlex from one machine in the same second, an
  anonymous read reported 0 of 1,000 requests remaining while the same source
  carrying an API key reported 8,792 of 10,000. *papio* kept a single
  `source_budgets` row per source, so the two shared one budget. A 429 taken
  anonymously wrote a `Retry-After` lasting until the next UTC midnight; adding
  an API key opened a fresh 10,000-credit allowance, but the row still said
  closed, and 95 jobs parked against a quota that had nothing to do with them
  until it was cleared by editing the database by hand. Rate-limit state is now
  keyed by source *and* a non-secret fingerprint of the credential in use
  (`anonymous`, or `key-` followed by a truncated SHA-256 — never the
  credential itself), so every account carries its own gate, request counter
  and spend figure. Existing rows are preserved under the identity `legacy`:
  which credential earned them is unknowable, and a `legacy` row can never gate
  live traffic. Schema version 18.

## [0.16.0] - 2026-08-02

### Added

- **The six-method consumer IPC contract is ratified and mechanically pinned.**
  External tools can build against `jobs.list_v2`, `actions.list_v2`,
  `actions.open`, `jobs.receipt`, `jobs.add_component`, and
  `jobs.repair_awaiting_human` knowing their names and result shapes will not
  drift. The receipt records typed job outcomes and component inventory where a
  bundle cannot exist; an accepted main component's bundle remains the success
  provenance document. `truncated` on the two paged methods is a proven fact
  from reading one row past the page limit, not a hint that more may exist.
- **`acquire.submit_v2` is ratified, so an external tool can now start an
  acquisition.** The six ratified methods all read or act on a job that already
  exists, so a consumer had no ratified way to ask *papio* for anything and
  *papio*'s acquisition success rate was unmeasurable from the only side
  counting it. One work per call — bulk submission, a generic reopen verb,
  method aliases, and autonomous drain all remain unratified. Frozen: the
  method name, the params `request` / `auto_import` / `force`, the result
  `job_id` / `existing`, and the work-request identity subset plus
  `access_mode_override`. Policy fields stay served but unratified so a
  consumer cannot pin *papio*'s policy vocabulary. Note that `request_id` is a
  live-job convergence key and not an idempotency key: a terminal job plus the
  same `request_id` creates a new job, and `existing` means "a live job already
  owns this work", so a consumer resuming a run must persist the returned
  `job_id`. See ADR-0010.
- **A duplicate discovered after submission is now recorded rather than
  ignored.** *papio* deduplicates at submit, and a title-only request correctly
  matches nothing because canonical convergence keys on strong identifiers.
  Enrichment can supply a DOI much later, and at that moment the job provably
  names the same work as one already running — two of 309 works on the first
  real cohort. It is written down as a `job.duplicate_work_detected` event and
  **not** merged: `existing` answers a question asked at submit about a handle
  issued at submit, consumers poll that handle, and silently merging would cost
  them a work they believe they are tracking against a duplicate fetch that
  content addressing already collapses to one stored file. See ADR-0010.
- **`bundle.document` and `artifacts.locate` are ratified, so a consumer can
  collect what it acquired.** No ratified method could produce a bundle or
  locate artifact bytes, leaving a consumer to call unratified names or read
  *papio*'s data directory — which would make the on-disk layout a contract
  nobody agreed to. Both are pure reads keyed on `job_id`. The two methods that
  already existed were deliberately **not** the ones frozen: `artifacts.get`
  returns the `job.Artifact` persistence struct including `identity_result`,
  which is last-writer-wins across every job sharing a digest and which ADR-0007
  forbids projecting from an artifact; and `bundle.export_v2` writes a directory
  as a side effect and carries the bundle inline, which — because results reject
  unknown fields recursively — would have frozen the document's shape into the
  RPC contract and forced a new method name for every future bundle schema.
  `bundle.document` returns the document as JSON text instead, byte-identical to
  the exported `bundle.json`, so the bundle keeps evolving under its own
  `schema_version`. Both remain served and unratified for existing callers. See
  ADR-0011.
- **Bundles now emit `acquisition-bundle/2`, adding `candidate.entitlement`.**
  It records the route by which access was obtained — `route`, an optional
  `entitlement_ref`, and `acquisition_mode` — and it is a route, never an
  identity: *papio* never authenticates a human and never holds institutional
  credentials. `acquisition_mode` is derived from the accepted candidate's
  existing `access_basis`, so nothing is inferred, and the whole object is
  omitted whenever *papio* did not observe a route. `daemon_held_credential` is
  not a future mode: CORE and Crossref TDM already acquire with *papio*'s own
  configured API credential, and `entitlement_ref` names which one in cleartext
  (`entitlement:source:crossref_tdm`) rather than hashing a public constant.
  `operator_browser_session` is reserved and has no producer: browser adoption
  records `institutional` for every adopted download, including an open-access
  PDF handed to the browser only because a provider's anti-bot wall refused
  *papio*'s own fetch, so claiming an institutional route would invent
  entitlement evidence for a route nobody walked. v1 decoding is retained
  indefinitely, and a v1 document carrying an entitlement — including an
  explicit `null` — is rejected rather than silently accepted.
- **`bundle.export_v2` carries the exported document; `bundle.export` returns
  the path alone.** IPC results are decoded with unknown fields rejected, and
  that applies to nested objects, so returning a v2 bundle body from the old
  method would make an older CLI reject every export response from a newer
  daemon. The new CLI prefers `bundle.export_v2` and falls back automatically.
- **`papio acquire` accepts a bare arXiv id.** `papio acquire 2301.08745` and
  `papio acquire math/0211159` previously failed with "cannot infer identifier
  type" and needed an `arxiv:` prefix or `--arxiv`. Both forms are unambiguous
  against every other scheme papio accepts — a DOI starts `10.` and contains a
  slash, a PMID has no dot or slash, an ISBN has neither — so they are now
  inferred. A bare ten-digit string is still refused on purpose: it is
  simultaneously a valid ISBN-10 and a valid PMID, so it names two schemes at
  once and the user is asked to disambiguate with a flag.

- **`papio actions dismiss` closes a stale human action without touching its
  job.** An advisory on a terminal job previously had no supported way out:
  cancel refuses a terminal job, resolve handles only identity review, and the
  startup sweep deliberately spares informational advisories so a genuine trace
  survives. Retiring one meant editing the database. `--revision` comes from
  `papio actions list --json` so a stale listing cannot dismiss an action that
  changed underneath it.

- **`papio doctor` reports acquisitions nobody ever collected.** A full text
  papio fetched and no one exported is the one thing a consumer cannot detect
  for itself: a job is stranded exactly when the key naming it stops being
  derivable, so the orphan is the job it can no longer ask about. Counted after
  a grace period, so a freshly acquired work is not mistaken for an abandoned
  one.

### Changed

- **`jobs.receipt`'s `principal` is request-origin classification, not a rights
  input.** It cannot identify whose entitlement obtained the bytes, so consumers
  must not use it as proof of entitlement.
- **A misspelled `[sources.*]` name is now a startup error instead of silence.**
  Source names were never validated, so `[sources.unpaywal]` parsed cleanly and
  did nothing — indistinguishable from "that source found nothing for your
  paper". `papio` now fails closed on an unrecognized source name and lists the
  valid ones. The removed `openalex_content` name is tolerated and dropped on
  load, so a config written by an earlier `papio init`/`papio config save`
  still parses; the next `papio config save` rewrites the file without it.
- **An unrecognized positional identifier gives better guidance.** A bare
  argument beginning with `W` was routed to the OpenAlex parser on the prefix
  alone, so an ordinary word got an OpenAlex-specific complaint instead of the
  actionable list of identifier flags. Scheme inference now checks the full
  shape.

- **`papio doctor` no longer passes cleanly when OpenAlex has no API key.** It
  now warns and names the roughly tenfold difference between the anonymous
  allowance and an account's. Passing read as fully configured, and that is
  exactly how a real operator missed it: they measured the anonymous tier
  against an unkeyed client and recorded multi-day cohort acquisition as a
  property of the design.

- **A negative `rate_per_sec`, `burst` or `max_cost_usd` is now rejected at
  load.** A negative does not throttle harder, it removes the throttle: the
  budget manager reads a rate at or below zero as unlimited and a cost ceiling
  at or below zero as unmetered, so a typed minus sign silently deleted the
  protection it appeared to configure. Zero keeps its documented meaning,
  because no pacing and no ceiling is a choice someone can state.

### Removed

- **`openalex_content` is gone from `[sources]`.** It was reserved end to end —
  constant, default policy row, resolver priority rank, and published config
  reference — but no adapter was ever written, so enabling it produced no
  candidate, no warning, and no `papio doctor` complaint.
- **`work.ClassifyIdentifier`, a second identifier classifier that never ran.**
  It duplicated the CLI's positional-argument inference and had already diverged
  from it, carrying the more inviting name and a passing test suite that asserted
  behaviour `papio acquire` did not have. Its disagreement on the ten-digit case
  was a latent bug — it classified a bare ISBN-10 as a PMID. Bare-string
  inference now lives only in `internal/cli`, where the sole caller is, and its
  accepted shapes and refusals are pinned by tests against the live function.
- **Roughly 570 lines of unreachable code and its false coverage.** Removed
  `store.Backup`/`Checkpoint` and their hard-link fallback subgraph (no command,
  IPC method, or scheduled task ever called them), the orphaned
  `internal/doctor/manifest.go` (a divergent duplicate of the live native-host
  manifest paths in `internal/cli`), six exported alias wrappers in
  `internal/pdf` and `internal/fetch`, `app.RequestForCandidate` and
  `StableResolverNames`, `budget.ClearDefer`, `errcat.WaitGuidance` (superseded
  by `WaitGuidanceWithOpenAction`, whose coverage now sits on the function
  production calls), and `work.FindDOI` (a weaker duplicate of the live
  multi-DOI scanner in `internal/pdf`). No behaviour changes.
- **The dead caller-supplied feature seam in the browser bridge.** `NewBridge`'s
  `features` parameter was always `nil` in production, so its merge, dedupe, and
  32-entry cap could never run, while a test-only synthetic feature made the
  hello-ack assertion describe a wire contract no extension ever received. The
  daemon still advertises exactly the seven mandatory features, and the
  fail-closed 32-feature bound in `internal/protocol` is unchanged. This also
  removes both hardcoded `bridge_test.go` assertions flagged in `AGENTS.md`.

### Fixed

- **A source's daily quota no longer freezes the whole acquisition queue.**
  Providers express an exhausted daily quota as a `Retry-After` pointing at the
  next reset — OpenAlex answers with the next UTC midnight, up to a day out —
  and `budget.Acquire` slept that out inline. The sleeping caller is an
  acquisition worker holding a job claim whose scheduler heartbeat keeps
  renewing the lease, so the claim never expires and the row can never be
  reclaimed: with three workers, three quota-gated jobs stalled a 309-job
  cohort at zero throughput for the rest of the day. `Acquire` now waits out
  only short blips (`budget.MaxInlineWait`, five seconds) and returns
  `*budget.ErrDeferred` for anything longer. Resolution skips the gated source
  and parks the job in `retry_wait` at the gate; a gated fetch candidate stays
  retryable; enrichment is simply skipped. The wait belongs to the scheduler,
  which can run every other job meanwhile.
- **A human action that needs an institutional sign-in now says so.** Whether
  finishing an action requires authenticating is what the conservative access
  mode is checked against, and it was recorded deliberately at only four of the
  twelve places that open one — everywhere else it defaulted to "no sign-in
  needed", and two of those defaults were wrong. The action opened when the
  browser extension has *explicitly reported* an authentication wall claimed the
  opposite, and every landing-page hand-off was recorded the same way including
  paywalled ones, so a work behind an institutional paywall looked freely
  fetchable. Both now record what was observed. A provider's terms-acceptance
  step fails closed, because it can sit behind a sign-in, a free account, or
  nothing at all and *papio* cannot tell which. The value is now a required
  argument, so a future omission is a build failure rather than a silent "no".
- **`papio jobs cancel` no longer reports a cancellation that did not happen.**
  Cancelling is deliberately a no-op once a job has already finished, which is
  right for repeat calls and scripting — but the command printed `Cancelled
  <id>` regardless, so a job that completed a moment before the command ran was
  reported as stopped. It now reads the state back and says what actually
  happened, naming the state the job was already in.
- **Waiting for a closed source gate no longer spends a job's retry budget, and
  never manufactures a terminal verdict about a source that was never called.**
  `retryBudgetExhausted` counted every `retry_wait` transition alike, so once
  gated sources began parking jobs instead of blocking workers, a day-long
  OpenAlex quota gate alongside ordinary thirty-second gates burned all eight
  attempts within minutes and settled the job `temporary source failures did
  not clear` — naming a source that had never been asked. The retry accounting
  now distinguishes the two reasons a pass can end without a verdict: a
  *temporary* failure means a request went out and failed, and costs an
  attempt; a *source gate* means no request was made, and costs nothing.
  Transitions record which, and older events without the discriminator keep
  the original bound. When the attempts genuinely are spent but a gate is still
  closed, the job now parks until it opens — and parks at the *gate*, not at
  the shorter temporary time it would only spin on. A retry time that elapsed
  during the same pass is floored rather than persisted into the past, which
  used to cause an instant re-claim.

- **A rate-limited sibling lookup no longer settles the job `unavailable`.**
  `resolveSiblings` recorded every error as a plain `failed`, never consulting
  `resolver.Temporary`. The version hop runs at the exhaustion boundary, where
  the presence of a retry time is precisely the difference between parking and
  giving up, so a 429 during the hop turned a retryable job into a terminal
  negative claim. Temporary sibling failures now defer the source and schedule
  a retry like any other. A pure source gate also no longer suppresses the hop:
  no request was made, and the sibling source may not be gated at all.

- **A forced resubmission now withdraws the verdict it supersedes.** `Retry`
  out of `unavailable` already cancelled the conservative `openurl_available`
  advisory, because "left open it outlives its own remedy and keeps telling the
  user to do the thing they just did". A `--force` submission withdraws a
  verdict just as much, but created an unlinked fresh job and left the old
  advisory open forever — and that advisory is deliberately exempt from both
  the terminal transition and the startup sweep, so nothing would ever retire
  it. Every resubmission therefore double-counted the work's institutional
  opportunity against a job that no longer represented it. Superseded terminal
  jobs for the same canonical work now have their advisory cancelled and record
  a `job.superseded` event naming the replacement, so a consumer that cached
  the old outcome can learn that a terminal papio record was not final.

- **A trailing full stop no longer hides a work from Crossref title
  enrichment.** APA and several other publishers deposit article titles with a
  closing period; citations and reference managers almost never carry one.
  Title matching normalised case and whitespace but compared for exact
  equality, so a perfect Crossref hit was rejected on one character — and the
  job then settled `no_identifier`, a claim that no identifier could be found
  for a work whose DOI was sitting at rank 0 of the response with matching
  authors and year. Only the trailing period is folded, and on both sides:
  replaying one cohort's twenty-six clean-title failures showed every
  recoverable case was a deposited full stop, so folding `?` or `!` too would
  buy nothing measurable while making `Who?` and `Who!` equal — and the same
  helper normalises author family names, so any widening loosens author
  corroboration as well.

- **The Crossref enricher now identifies itself to the polite pool.** It was
  the one source client that sent no `mailto`, while every other client sends
  one and `papio doctor` reports the contact identity as configured.

- **The per-source token bucket is bounded too, and can no longer busy-spin.**
  `Acquire`'s durable-gate wait was bounded, but the rate-limiter loop beside
  it was not: a slow refill held a leased acquisition worker, contention let a
  waiter lose each refilled token without converging, and at a very low
  configured rate the computed wait overflowed `int64` nanoseconds into a
  non-positive duration, so the sleep returned immediately and the loop spun
  hot. It now shares `MaxInlineWait` and returns `ErrDeferred` past it, so the
  job parks like any other gated source. The wait is never persisted — a token
  is process-local and advisory, and another caller may take it first.

- **A per-request `access_mode_override` now actually governs the acquisition,
  and can only narrow.** The override was validated, snapshotted into the job's
  policy, and printed by `papio diagnose` — while the only code that decides
  whether to open an institutional browser handoff read the daemon-wide
  `access_mode` instead. Submitting one work as `conservative` against an
  `assisted` or `delegated` daemon therefore opened a handoff anyway, and
  reported that it had honoured the override. The job's own snapshot is now
  authoritative at both decision points — the exhaustion gate and the
  `access_mode` sent to the extension in a job offer. An override may now only
  *narrow*: the configured `access_mode` is the operator's standing decision and
  the only brake papio has, so a submitter cannot raise automation above it, and
  the ceiling is re-applied on every read, so tightening `access_mode` restrains
  jobs already queued rather than only new ones. Narrowing is what the override
  is for — a cohort run asking for `conservative` on a delegated daemon records
  `openurl_available` advisories and opens nothing, instead of parking hundreds
  of handoffs that never expire.
- **Widening `access_mode` and retrying a conservative job works again.** The
  conservative advisory tells the operator to switch access mode and retry, but
  a retry preserves the job's policy — so once the policy became authoritative
  the job re-exhausted under its original mode and reopened the same advisory,
  telling the operator to do the thing they had just done. Retry now releases
  the pinned mode in the same step that cancels the advisory.
- **A stale parked job can no longer disconnect the browser extension.** A job
  whose access mode resolves to `conservative` cannot be expressed in a
  `papio-browser/1` job offer, and any error out of the bridge is treated by
  the native host as a dead connection — so one such row would have torn down
  the whole native-messaging session instead of being skipped. It is now
  skipped, and its siblings are still offered.
- **`papio acquire --batch` works again against an older running daemon.** Batch
  moved to `acquire.submit_v2` without the `unknown_method` fallback the
  single-work path has carried since that method shipped, so every work in a
  batch failed against a daemon older than 0.13.0 — a routine state, since one
  binary serves as CLI, daemon, and native host.
- **`update.Checker.Check` no longer returns an error it can never produce.**
  Its failure policy is deliberately soft — a GitHub outage must not make
  `papio doctor` noisy — so every path returned `nil`, leaving unreachable
  error branches in the doctor integration checks and five test assertions that
  could never fail. The signature now matches the contract, and the tests pin
  the behaviour that matters: a transport error, a 304, a non-200, and a
  malformed body each return the previously cached release, or `nil` on a cold
  cache.

- **Asking for more rows no longer returns fewer.** A `--limit` above the
  maximum reset to the *default*, so `--limit 600` yielded 100 rows where
  `--limit 500` yielded 500 — silently, and in the direction of
  under-reporting, which is the worst way to be wrong for the only people who
  pass a large limit: the ones counting. Two separate consumers hit it on the
  same day. Over-large now clamps down to the maximum; unspecified still means
  the default. The same non-monotonic clamp existed in four places, including a
  client-side copy that ran before the request and so decided the answer
  regardless.

- **Discovery's provider calls are accounted for and can be paused.** papio
  reached OpenAlex through two clients and only one of them was budgeted: the
  resolvers reserve at the job level, while search, MCP, watch digests and
  DOI-only enrichment held a bare HTTP client. So papio under-reported its own
  consumption by an unknown amount, and a durable gate that paused acquisition
  did not pause discovery at all — it kept calling an API that had already said
  stop. Discovery now reserves per request and reports a rate limit as one.
  It also shares the resolvers' secure HTTP client, so its redirect and size
  policy is the one `papio doctor` reports.

- **A server cannot defer a source for longer than a day.** `Retry-After` was
  honoured unconditionally and an existing gate was never shortened, so a clock
  skew, a malformed header or a provider bug could park every job needing that
  source for as long as it asked — recoverable only by editing the database.
  Waits beyond the horizon are clamped; anything inside it is still honoured to
  the second.

## [0.15.0] - 2026-07-30

### Added

- **`papio jobs receipt`, `papio jobs repair-awaiting-human`, and `papio jobs
  add-component` are now available to operators and MCP agents.** The daemon
  could already perform all three, but only a program dialling the socket could
  ask: neither the CLI nor MCP agents could. The bundle remains the canonical
  provenance document for an accepted main component; the receipt is the
  canonical job outcome and component inventory for every state.
- **`papio inbox decide` acts on a triage inbox item from the terminal.**
  `papio inbox` and `papio inbox counts` could show the inbox, but only the
  browser extension could answer an item — so acquiring or dismissing a watch
  hit, or acknowledging a retraction notice, required the browser even for
  someone working entirely in a terminal. `--op acquire|dismiss` with
  `--watch-scope all` or an explicit watch-id list now does it, reporting the
  daemon's own outcome (including `conflict` and `already_applied`) rather than
  assuming the decision applied.

### Changed

- **Repair audit reasons and request origin no longer overstate who acted.**
  Orphan repair now records `orphan_repair`, rather than naming a caller it cannot
  identify. `principal` is documented as request-origin classification, not proof
  of whose entitlement obtained the bytes; do not use it as a rights input.

### Fixed

- **`attempted_tiers` retains every tier a job genuinely attempted after an
  explicit retry.** It now reads append-only attempt records instead of current
  candidate status, which retry resets can erase.
- **Attaching a supplement now says what to fix.** Every expected refusal from
  `jobs.add_component` — the job holds no main artifact yet, the file sits outside
  the job's adoption root, the file is not a readable PDF, the file carries active
  content — arrived as `internal: operation failed`, because only an unrecognised
  error was meant to collapse that way and none of these were classified. They now
  return the condition and the remedy. The confinement case deliberately does not
  echo the offending path: that belongs in the daemon log, not an RPC message.
  Nothing here loosens a guard; the same files are refused, and now the operator
  can tell which guard refused them. Found by exercising the new command against a
  live daemon rather than a stub.

## [0.14.0] - 2026-07-30

### Added

- **De-duplication against a non-Zotero library** (`[[library.sources]]`, new
  `library.lookup_works` method). Ownership lookup was a zotio capability, so
  without Zotero `papio search` never marked a result `[in library]` and
  `acquire --batch` happily re-acquired papers you already held. Point *papio* at
  BibTeX/RIS/CSL-JSON/NBIB export of what you hold and it stops. Each source
  declares what it asserts — `claim = "pdf_present"` may skip an acquisition,
  `claim = "record_present"` only annotates search — because *papio* refuses to
  infer PDF presence from per-manager attachment conventions and thereby skip a
  paper the source never vouched for. Matching is exact on identifiers supported
  by each format: BibTeX supports DOI, arXiv, and PMID; CSL-JSON and NBIB
  support DOI and PMID; RIS supports DOI only. Titles are never matched and
  ISBN is excluded, since an edited volume shares one ISBN with every chapter
  in it. There is no supported-app list: anything that exports one of those
  four formats works.

  The load-bearing rule is that **a source unreadable to *papio* is not a
  source that holds nothing**. An unreadable or malformed export leaves the
  answer *incomplete*, so `--batch` creates no jobs and names the failing source
  rather than re-downloading the batch. An acquire watch fails the run and
  retries on cadence until the standard fifth-consecutive-failure auto-disable;
  it has no `--include-owned` override. That override is available only for
  `papio acquire --batch`. During a transient provider failure, a fresh
  cached positive may still annotate `papio search` and raw lookup, but batch
  and watch acquisition remain fail-closed. The runtime last-known-good guard
  is not persisted across daemon restarts; restart establishes the current
  export as the new baseline. Because a bibliographic export cannot say
  *which* manifestation it holds, such a source never satisfies an explicit
  `--desired-version published` request. `papio doctor` performs a fresh
  one-shot source probe and reports that read's record count and outcome, not
  daemon cached age, count-collapse, or retained failure state. See ADR-0008.
- **`papio stats`** reports what the pipeline has actually obtained: lifetime
  acquired and failed totals, how many acquisitions needed a browser handoff, the
  split of acquired works by access basis (open access / institutional /
  licensed API / other), and a twelve-week series. The daemon has computed this
  all along for the extension's stats view (`stats_request`), but no RPC or
  command exposed it, so the CLI — and the MCP surface derived from it — was the
  only interface that could not see whether institutional access was buying
  anything. `stats.get` is a passthrough of that read model, not a second
  aggregation: `papio status` still owns the live job board and
  `papio jobs failures` the failure groups.
- **`jobs.receipt`** reports what happened to one acquisition: its state, its
  typed terminal reason, who requested it, which access tiers it actually reached,
  and which components it holds. It exists for the outcomes an acquisition bundle
  cannot describe — a failed job has no bundle — and deliberately does not restate
  what a bundle already carries, so the two can never disagree. Successful jobs get
  `bundle_available: true` and the bundle remains the full provenance record.
- **A job can now hold supplements and appendices beside its main PDF**
  (`jobs.add_component`). A quotation missing from a main PDF may simply live in a
  supplement, so a tool that reports "not in the source" without them is making a
  claim papio's own evidence does not support. Components are validated by the same
  payload and structural gates as a main file; identity is not asserted, because a
  supplement is usually not the article and carries neither its title nor its DOI.
  `html_fulltext` is refused for now with a clear error: raw provider HTML is
  inherently active content, and admitting it needs a sanitization design rather
  than a new role name. No resolver emits components yet.
- **`jobs.list_v2` and `actions.list_v2`, whose `truncated` is a proof.** The
  existing flag is inferred from a full page, so an exactly-full final page is
  indistinguishable from a partial one — fine for a human raising `--limit`, not
  fine for a program reconciling a cohort of hundreds of works. The new methods
  reach one row past the limit and report whether it was there. `papio jobs list`
  and `papio actions list` use them and fall back to the old methods against an
  older daemon; `papio actions list` gains `--limit` to go with it.
- **`jobs.repair_awaiting_human`** returns an *orphaned* parked job — one
  awaiting a human with no open action left to act on — to `resolving`. Nothing
  else could: `jobs.retry` refuses parked jobs by design and `actions open` needs
  an open handoff action, so the row was only recoverable by the daemon's own
  background sweep. Deliberately orphan-only and it takes no action ids, so it
  cannot close an action the caller never read. Handoff offers still never expire.
- **Acquisitions record which principal requested them** (`cli`, `mcp`, or
  `unknown`) instead of a hardcoded `cli`, and terminal reasons are now a closed
  vocabulary rather than free text. Persisted values are unchanged.
- **A retraction notice can be acknowledged.** The sentinel recomputes update
  notices from Crossref for as long as the work stays in the library, so a notice
  never resolved itself: it sat at the top of the inbox — above the actions that
  still need work — for the life of the library copy. Retraction items now carry
  `dismiss` (and `open`), and `triage.decide` routes a `retraction:` item to the
  sentinel instead of looking for a watch digest that will never exist. The
  acknowledgement is bound to the notice identity, so a nature that escalates
  (concern → retraction) or a newly issued notice DOI surfaces the work again, and
  the daily sweep prunes acknowledgements whose notice is no longer current. The
  wire shape is unchanged, so an already-installed extension gains the button.
  Schema version 17.
- **`papio acquire` accepts several identifier flags at once.** A work carries a
  DOI *and* a PMID *and* an arXiv id, and `protocol.Identifiers`, the
  `identifiers` table, and every batch parser have always modelled that — only the
  CLI insisted on one, forcing callers to discard identifiers they already held.
  Flags now compose; mixing them with the positional identifier is still refused,
  because that one really is ambiguous.

### Changed

- **`papio init` accepts `none` for the zotio executable.** An empty answer means
  "keep the default" at every init prompt, and the zotio default is the bare name
  `zotio`, so someone who does not run Zotero had no way to decline it: init wrote
  `zotio.executable = "zotio"` and `papio doctor` then *failed* on a binary that
  was never wanted. Answering `none` clears the key — `doctor` reports zotio as
  `not configured (optional)` — and the attachment-mode question, which only
  describes how zotio files an attachment, is skipped. `--zotio-path ""` already
  did this non-interactively.
- **The README and docs no longer read as if Zotero were the only destination.**
  The `on_ready` hook, standard-format batch input (RIS, BibTeX, CSL-JSON,
  MEDLINE/NBIB), and a zotio-free `doctor` all shipped already, but every entry
  point — README headline, docs landing page, getting-started, user guide, the
  site description, and the pipeline diagram — described the Zotero path
  exclusively, and the hooks page was missing from the landing page's own index.
  Filing is now two paths (zotio → Zotero, or the hook into papis, a plain
  folder, or your own script), getting-started ends in a filing step for
  whichever one you use, and the de-duplication you give up without zotio is
  stated rather than discovered.

### Fixed

- **`papio acquire --batch` no longer reports a decode error after successfully
  creating its jobs.** The batch client samples one field — the job state — out of
  the `jobs.get` result, but declared a struct containing only that field while
  `internal/ipc` rejects unknown fields on the whole envelope. So every batch
  submitted its work, then failed decoding the daemon's reply on the first field
  it did not know about (`job.id`), printed `decode ipc result: json: unknown
  field "id"`, reported every state as `unknown`, and exited non-zero. It now
  decodes the siblings it does not read as raw messages, which is the general rule
  this violated: a caller that reads one field must not be coupled to the shape of
  everything beside it. Found by smoke-testing the batch path end to end.
- **The conservative-mode OpenURL advisory no longer outlives its own remedy.**
  `openurl_available` records that an institutional route existed and
  conservative mode did not take it, and it is deliberately exempt from the
  terminal-action close and the startup sweep so that trace survives on a job
  that stayed unavailable. But its guidance is "switch access mode and retry" —
  and `jobs.retry`, the only edge out of `unavailable`, never touched it. Taking
  the advice therefore left the advisory sitting in the inbox on a job that had
  since reached `ready`, still telling you to do the thing you just did. A retry
  now clears it; re-exhausting in conservative mode raises it again.
- **One job's identity check no longer overwrites another's.** Identity is decided
  against the work *this* job asked for, but it was stored on the shared,
  content-addressed artifact row, so a second acquisition of identical bytes
  rewrote the first job's recorded finding — and with it that job's exported
  validation block. Each acquisition now records its own finding, and bundle
  export reads that rather than the shared row.
- **An acquisition no longer inherits another acquisition's licence.** Artifacts
  are content-addressed and shared, so two jobs can hold identical bytes obtained
  under different terms — an open-access mirror and an institutional copy of the
  same file. Bundle export resolved provenance by content hash alone and picked
  the *earliest* job holding that hash, so an institutionally-acquired job could
  export the earlier job's `access_basis` and `reuse_license`: first-writer-wins
  rights attribution on a digest. Provenance now comes from the job's own
  candidate, and only when that candidate was actually *accepted* — a job can
  carry a rejected selection forward through crash recovery, and reading it would
  have published the licence of a file papio threw away. A job completing from the
  local cache now records the source acquisition's candidate rather than leaving it
  to be reconstructed from the digest later.
  **If you gate retention or redistribution on a bundle's `reuse_license`,
  re-export any bundle written before this release whose PDF you acquired through
  more than one job — including under different identifiers, since the affected
  condition is a shared file, not a shared work record.** A wrong bundle cannot
  self-detect: its `provenance_digest` signs whichever candidate block was written.
- **A browser-adopted download no longer claims a version papio never observed.**
  Adoption labelled the file `published`, or — worse — copied the request's
  `desired_version` *preference* into the obtained *fact*, so asking for an
  accepted manuscript produced a candidate asserting it had received one. papio
  sees bytes arrive from your browser and never learns which version you chose,
  so the adopted version is now always `unknown`. Its access basis
  (`institutional`) and licence (`unknown`) were already honest and are unchanged.
  Adopted candidates written before this release are normalised when the same
  bytes are adopted again or an identity review of them is accepted.
- **The README architecture diagram's `alt` text is no longer truncated
  mid-word.** The `<img>` attribute was cut off after "login, MFA, a" and left
  unterminated, so the tag never closed.

## [0.13.0] - 2026-07-27

### Added

- **Diagnostic page captures no longer land in your Downloads.** They travelled
  via `chrome.downloads` because ordinary extensions have no filesystem access —
  but papio has a native-messaging host and a daemon that already owns its data
  directory. Captures now cross the bridge as a gzip-compressed `page_capture`
  frame (the largest real capture is 40% of the frame budget, so one frame, no
  chunking) and the daemon stores them under `<data dir>/captures/`, bounded to
  10 per host and 14 days. `papio adapter captures` lists them and
  `papio adapter captures purge` removes them. Enabled by default; set
  `[captures] enabled = false` to turn it off. The old
  `~/Downloads/papio-fixtures/` directory is no longer written and is safe to
  delete. Gated behind the `page_capture_v1` feature per ADR-0001, so an
  extension paired with an older daemon simply does not capture.
- **A fixed adapter now retries what the broken one stranded.** Provider
  outcomes record the extension version that produced them, and a job parked on
  a manual download is re-offered once when the live browser reports a newer
  version. Adapters ship inside the extension bundle, so an adapter fix is an
  extension upgrade — precisely the moment a job's failure reason stops
  existing. Bounded to one retry per job per upgrade, only with a live holder
  session, and never touching a leased job, a route already proven empty, or a
  `verify_identity` awaiting your decision.
- **Re-submitting a paper that is already in flight no longer queues a
  duplicate.** Four separate jobs for one DOI had accumulated on one machine,
  because the underlying handoff kept failing and re-submitting was the only
  recourse. `papio acquire` now returns the live job and says so, naming
  `--force` for a genuinely fresh attempt. Terminal jobs never block a new
  submission — `ready`, `failed`, `unavailable` and `no_identifier` are all
  finished, and re-queueing after adding a missing DOI is the documented
  remedy. Works identified only by title are deliberately never merged: a title
  describes rather than identifies, and an erratum or a preprint can share one
  with the paper the user actually wants. Delivered through a new
  `acquire.submit_v2` method with a fallback to the old one, rather than
  widening a result an older CLI would reject.

- **Durable escalating reminders for stranded human actions.** The daemon now
  re-notifies the configured desktop and webhook sinks after
  `[browser] action_expiry_seconds` (30 minutes by default), then doubles each
  action's delay through 24 hours instead of repeating every maintenance pass.
  Notices distinguish an institutional sign-in, an openable no-login handoff,
  and a review decision; each names only actions whose own schedule is due.
  Their per-action schedule is stored as an `action.reminder` event, so daemon
  restarts do not reset it.
- **`papio actions open` now reuses a live extension handoff.** With a
  compatible browser holder, the CLI asks the daemon to focus the job-scoped
  handoff tab instead of launching a second, untracked OpenURL tab through the
  operating system. When that tab is stranded on authentication, reopening it
  re-drives its retained resolver URL; provider pages mid-download are only
  focused. No session, legacy native host, or older extension retains the
  OS-launcher fallback.

### Fixed

- **Guidance for a parked job now comes from its current action, not a frozen
  reason.** A job that parked long ago as `login_required` and later had its
  handoff replaced by a `manual_download` still displayed the original advice:
  sign in, then run `papio actions open` — a command that cannot open a manual
  download. The reason was a historical transition record and never updated
  when the action changed. Where a job has an open action, that action is now
  the source of truth for what the user is told, across `papio status`,
  `papio acquire --wait` and the MCP status surface; jobs with no open action,
  such as terminal `no_identifier` or `no_entitlement`, are explained exactly as
  before. This was the fourth surface to carry the same defect, so the
  conformance guard was extended to the state-driven path as well — each earlier
  guard only covered the surface it was written for, which is precisely how it
  kept reappearing.
- **papio was abandoning handoffs while the provider was still verifying the
  browser.** Nine of ten real handoffs on one machine ended `ui_changed`,
  reported as "the provider's UI changed" and parked as a manual download. The
  pages papio captured at those moments were Cloudflare interstitials — one of
  them reading *"Verification successful. Waiting for journals.sagepub.com to
  respond"*. The challenge was passing; papio's two-unknown-verdicts-5s-apart
  escalation was simply faster than the challenge, so it gave up seconds before
  the article would have loaded and told the user to fetch by hand a PDF that
  was already on its way. A bot check is now recognised as the transient state
  it is: escalation is deferred, the unknown streak cleared, and classification
  retried for up to a minute; only if it never clears is the job reported
  blocked by a bot check rather than mislabelled as adapter rot. A genuinely
  unreadable provider page with no challenge present still escalates exactly as
  before, so real adapter rot stays visible. The detector was validated against
  all 25 pages papio had captured from live failures, matching both Cloudflare
  variants on every affected host and none of the 15 ordinary pages.
- **papio asked users to sign in for items their library had already said it
  does not have.** The institutional resolver answers "No full text available"
  on its own page, but no adapter covered the resolver host, so that terminal
  answer classified as unknown and became a manual-download park — complete
  with advice to sign in first. A resolver adapter now reads that answer as
  `no_entitlement`, which papio already knows how to handle.
- **A reminder told users to run a command that cannot act on their action.**
  `papio actions open` cannot open a `manual_download`, yet the escalating
  reminder named it for every auth-requiring action. This was the third
  appearance of one defect: three separate places derived "what should the user
  do next" from an action's kind and sign-in requirement, each written when only
  handoffs carried that requirement. They now share one authority, and a new
  conformance test asserts that any command named for an action can actually act
  on it — the earlier guard only checked that the command existed, which is why
  it passed all three times.
- **One sign-in now releases the batch.** Jobs parked on the same institutional
  route are re-offered when a session becomes live, instead of each waiting for
  its own handoff and eventually being reported as needing a manual download.
  Bounded by the usual guards: same resolver profile, live holder, never a route
  already proven empty, and never a job an adoption is mid-flight on.
- **Provider outcomes now record why they happened.** `adapter_version` and
  `detail` already crossed the bridge, validated and bounded, and the daemon
  discarded both — so nine failed handoffs left an audit trail that said only
  that they had failed. `papio jobs get` now shows which adapter version saw
  what, which is how the Cloudflare diagnosis above was possible at all.
- **A handoff whose provider page papio could not drive became a dead end.**
  When the extension reported `wrong_work` or `ui_changed`, the daemon resolved
  the job's only open action and parked it in `needs_review` — a state whose
  entire contract is that a human approves or rejects something — without
  opening anything to approve or reject. `papio status` said "see `papio
  actions` and approve or reject it" while `papio actions` was empty, and
  `needs_review` is not retryable, so the job could be neither acted on nor
  re-run. Nine jobs sat like that for nine days on one machine. Such outcomes
  now park in `awaiting_human` with an open `manual_download` action, which is
  actionable, visible to the reminder escalation, and still adoptable if the
  user downloads the PDF themselves. Existing stranded jobs are healed by
  handoff maintenance rather than needing manual cleanup, and the replacement
  action inherits the sign-in requirement of the handoff it replaces so it
  cannot advertise "no login needed" for a paywalled work.
- **The recovery command `papio status` recommended did not exist.** Every
  parked browser handoff, open-access browser fetch, and generic
  `awaiting_human` job was told to run `papio actions --open`, which exits 1
  with `unknown flag: --open`; the real command is the subcommand
  `papio actions open`. So the surface a stuck user actually reads pointed
  them at a dead end — precisely when a stale institutional sign-in had
  already cost them the handoff. The institutional guidance now also states
  that re-running it mints a fresh resolver link, which is the recovery for a
  "stale request" / "expired" sign-in page. A new test parses every backticked
  `papio …` command out of the `internal/errcat` catalog and resolves it
  against the live command tree, so guidance can no longer quote a command or
  flag that does not exist.
- **A daemon started by the browser could not find Poppler, so every PDF was
  staged for human review.** macOS gives a launchd child
  `PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin`, and capability detection
  trusted `PATH` alone — so a daemon autostarted by the browser's
  native-messaging host saw no `pdftotext`, failed semantic extraction on
  everything, and marked every document's identity unverifiable. Whether papio
  could read a PDF at all depended on *who happened to start the daemon*: from
  a shell it worked, from the browser nothing did. Detection now falls back to
  the usual package-manager prefixes (Homebrew on both architectures, MacPorts,
  Linuxbrew, snap) when `PATH` comes up empty, preferring `PATH` when it
  resolves. `papio doctor` also states the consequence — "every PDF will be
  staged for human review" — and, when the tool is installed but unreachable,
  says to restart the daemon from a shell rather than telling you to install
  software you already have.
- **PDF identity verification accepted the wrong document roughly one time in
  ten.** Title, author, and year evidence was matched against the *entire*
  extracted text, and a paper's bibliography is several hundred other papers'
  titles and thousands of their authors — so any long document satisfied almost
  any other document's author and year. Measured over 1560 deliberately
  mismatched document/metadata pairs drawn from one real 40-paper library, 155
  (9.9%) were accepted as the wrong work, unlocked by given names such as
  "david" and "john", by an organisational "the", and by any recent year
  appearing in a citation. Identity evidence now comes from the byline window
  at the top of page one, the author test requires a family name rather than
  any name token, and the year is recorded as corroboration instead of gating
  acceptance. The same corpus now yields no wrong-document acceptances, while
  correct acceptances rose from 39/40 to 40/40.
- **A printed DOI, arXiv id, or PMID now corroborates identity wherever it
  appears.** The front-matter scan reads only the top of page one so a
  reference-list DOI is never mistaken for the document's own, which also meant
  it missed an identifier printed in a running footer or below the abstract —
  17 of 40 real papers. Once a document clears the title gate, its own printed
  identifier is accepted as proof, which also fixes reprints: the CACM edition
  of a NIPS paper is catalogued under 2017 and contains no "2017" anywhere in
  its text, so it was staged for human review despite printing the exact
  requested DOI on page one.
- **Identifiers are read even when the publisher letter-spaces them.** ACM
  typesets a DOI as `DOI:10.1145/ 30 6 5 3 8 6`, which no regex could match.
  Identifier corroboration now compares whitespace-insensitively.
- **Line-break hyphenation and ligatures no longer cost title matches.**
  Justified text yields `classifi-\ncation` and some producers keep `ﬁ`/`ﬂ`
  codepoints; neither tokenized as the word the title contains. Superscript
  affiliation markers glued to byline surnames (`Arrietaa`, `Tabikg`) are
  tolerated too — one real 12-author paper had all twelve marked that way.
  Document text is also tokenized once per decision rather than re-walked for
  every candidate token.
- **Work with no fetchable identifier no longer asks for an institutional
  sign-in.** `exhaustedCandidates` asked only whether a resolver base was
  configured, so a book, chapter, report, or thesis with no DOI parked as an
  institutional OpenURL handoff — telling the user to spend an SSO round trip
  on something a library can only return as a catalogue record. On one real
  112-item backlog that was 27% of the queue, and each one parked forever.
  Ordinary exhausted jobs now settle `unavailable` with the reason
  `no_identifier`, with no institutional handoff, and explain the remedy for
  either origin: re-submit a manual request with its DOI, or
  apply Zotero DOI enrichment and re-queue. A metadata-corrected Zotero item
  bypasses the ordinary unavailable cool-down, so that remedy works
  immediately. The gate is a *fetchable* identifier — DOI, PMID, or arXiv id
  — so a chapter with a publisher DOI keeps its handoff, while an ISBN alone
  does not. A title-matched anti-bot page remains available even without an
  identifier. If that OA offer reaches an auth or terms wall, reports no
  entitlement, or is rejected, it settles `no_identifier` rather than falling
  back to an institutional sign-in it cannot complete. Existing parks are healed
  automatically: handoff maintenance now returns an unfetchable park to
  `resolving`, where the same gate reclassifies it.
- **Zotero ISBNs reach the resolver, and monographs are described as books.**
  `--from-zotio` dropped the ISBN, and every OpenURL put the work's title in
  `rft.atitle` — asking the library for an *article* by that name. A work with
  an ISBN and no DOI is now sent as `rft.isbn` plus `rft.btitle` under the book
  metadata format. Multi-edition fields now split on edition separators before
  whitespace, so space-formatted ISBNs do not shatter into invalid digit groups.
- **A completed browser download can no longer be lost to handoff repair.**
  Repair now atomically checks the adoption lease before closing stale actions
  or returning a parked job to resolution, so a just-completed download remains
  on the adoption path instead of becoming eligible for cleanup.
- **Handoff maintenance now rotates through the whole parked queue.** A
  bounded pass no longer revisits the same first 500 awaiting jobs forever, so
  newer stranded handoffs and reminder-eligible actions are eventually reached.
- **Reminder backoff survives a backward wall-clock correction.** A future
  reminder timestamp is rebased while retaining its escalation count, rather
  than suppressing a stranded handoff until wall time catches up.
- **`papio actions open` can focus handoffs outside the ordinary poll page.**
  An explicit focus request now force-offers its parked job before asking the
  extension to surface it, rather than suppressing the CLI's OS-launcher
  fallback for a job the extension was never told about.

## [0.12.0] - 2026-07-25

### Added

- `papio doctor` now warns when an OpenURL resolver base — the default
  `[browser] openurl_base_url` or any named `[browser.resolvers.*]` profile —
  points at a raw Alma link resolver (`…/view/uresolver/…`). That deep link
  sends an unauthenticated patron to the Alma *staff* login (`…/mng/login`),
  which patrons cannot complete; the check recommends the institution's Primo
  OpenURL endpoint (`…/discovery/openurl` or `…/nde/openurl?vid=…`) instead.
- **Acquisition-history stats over the browser bridge** (`browser_stats_v1`).
  The daemon answers a new `stats_request` frame with lifetime acquisition
  aggregates: works acquired, works failed, how many needed a human handoff,
  a breakdown by access route (open access / institutional / licensed API /
  other), and a 12-week weekly series bucketed by when each work was actually
  acquired rather than when its row was last touched. Counts only — the
  daemon reports facts and never a "time saved" figure. Read-only; the one
  schema change is an index, no table or column touched.
- `papio doctor` now reports discovery backend health. A backend that failed
  during a recent search is surfaced as a warning naming the backend and the
  cause, so thin search results can be told apart from a broken backend; a
  backend that has since answered is cleared automatically. Previously a
  backend could fail on every search with nothing said anywhere, because a
  partial failure was discarded whenever another backend answered — the daemon
  now logs it too.

### Changed

- **BREAKING for anyone parsing `--json` as a bare array:** `search`, `jobs
  list`, `actions list`, `actions open`, `watch list`, and `watch digest` now
  return `{"<name>": [...], "truncated": bool}` envelopes — the same shape
  the MCP resources (`papio://jobs` and friends) already used — instead of a
  bare top-level array. `truncated` means the page filled its row cap — the
  default or a `--limit` bound — so more rows may exist; it is not proof that
  they do. An empty result is `[]`, never `null`. `jobs failures` was
  already an object (`{"failures": [...], "since": ...}`), never a bare
  array; its breaking change is narrower — the `since` metadata key is no
  longer re-emitted in the `--json` output (the daemon reply still carries
  it and the CLI still decodes it, so `--since` itself keeps working).
  Commands returning a single structured record (`jobs get`, `doctor`,
  `status`, `batch report`, `zotio plan`, `inbox`) are unchanged.
- `papio init` now points Firefox users at the published add-on listing
  (`https://addons.mozilla.org/firefox/addon/papio/`) instead of the
  temporary `about:debugging` load it recommended while the listing was in
  review. A non-default add-on ID still gets the development-build hint.

### Fixed

- `papio daemon status`, `papio browser sessions`, and `papio browser use` no
  longer fail with `unknown field "pending_browser_sessions"` when more than
  one browser is connected. The CLI's daemon-ping decoder was missing the
  browser-session-arbitration fields (added to the daemon in 0.8.0), so the
  strict version-check pre-hook rejected any ping that reported waiting or
  denied sessions.
- `papio --version` now works alongside the existing `papio version`
  subcommand, printing identical output.
- A failed discovery search no longer risks echoing the request URL — and with
  it the configured contact email and any backend API key — into the error
  message or the daemon log. Backend errors are now redacted to
  scheme/host/path before being reported, keeping search failures inside the
  same rule that already governs stored URLs.
- `papio search` no longer buries an exact-title match behind unrelated
  higher-cited papers. Results are annotated with `match_score` (0..1) and
  `match_kind` (`exact_title`, `title_phrase`, `title_tokens`, `weak`,
  `unscored`) so it's clear why a row ranked where it did; confident title
  matches are promoted to the front, best score first, and every other row
  keeps the order the backend gave it — a discovery backend weighs the
  abstract, concepts, and citation graph better than title comparison can on
  a keyword search, so *papio* only refuses to bury an obvious title match,
  never tries to out-rank the backend generally. Ranking now happens before
  `--limit` is applied, so a match the backend ranked low can still surface.
  A short-token query or a citation-snowball search (`--cites`/`--cited-by`/
  `--related-to` with no query) is reported `unscored` and deliberately left
  unreordered. The human-readable output now says so plainly when nothing in
  the result set matched strongly, instead of silently returning the closest
  unrelated papers.
- A failed inbox-counts or acquisition-stats query no longer disconnects the
  browser extension. Both handlers returned a raw error, and the native host
  treats any error out of a browser-bridge handler as a dead connection, so
  one failed aggregate tore down page acquire, the triage inbox, and the
  handoff flow along with it. They now answer with an ordinary error frame —
  the extension already renders that as a muted "unavailable" state — and log
  the cause for the operator. The stats query that was most likely to fail no
  longer scans `human_actions` in full once per acquired work either.

## [0.11.0] - 2026-07-24

### Added

- When the institutional OpenURL route reports no entitlement (or only
  document delivery), the daemon now re-enters resolving once and lets the
  existing sibling discovery look for an open-access copy of an alternate
  version before giving up. A durable `browser.no_entitlement_requeue` job
  event guards the retry: once the route proved empty, later no-entitlement
  reports — including from a rediscovered open-access handoff — park the job
  `unavailable` with terminal reason `no_entitlement` and the honest
  no-access guidance instead of offering that route again.
- A maintenance pass (run at startup and once a minute) heals handoff parks
  stranded by a crash mid-transition: an `awaiting_human` job with no open
  action, or one still offering an institutional route its own event history
  already disproved, is sent back to resolving to finish honestly.
- DOI-only submissions now get their title, authors, and year backfilled from
  one bounded OpenAlex lookup at resolve time, so inbox rows — especially
  identity-verification prompts — show what the paper is supposed to be
  instead of a bare DOI.

### Fixed

- Open-access works whose only candidate is a landing page (the publisher's
  DOI page rather than a direct PDF) now route to the browser handoff instead
  of a dead-end manual download. The daemon cannot fetch an HTML landing page
  as a file, but the extension's provider adapters resolve the PDF from that
  page — so an open-access landing on a supported provider is handed to the
  browser (no login) and downloaded automatically. Paywalled landing pages
  still park as a manual download or take the institutional handoff.

- `papio native-host install` now pins the browser native-messaging host to the
  invocation path (e.g. Homebrew's stable `/opt/homebrew/bin/papio`) instead of
  the fully symlink-resolved `…/Caskroom/papio/<version>/papio`. A `brew upgrade`
  deletes the old versioned directory, which previously dangled the host symlink
  and disconnected the extension ("could not establish a current daemon
  session"); pinning the stable launcher survives upgrades. `papio doctor` now
  also fails when the host executable a manifest points at is missing, with the
  exact remediation, instead of only validating the manifest JSON.

- Single-word author names (bare family names and mononyms) no longer sink
  metadata corroboration: Europe PMC and OpenAlex title-search matching and
  Crossref enrichment now compare them by name instead of rejecting the whole
  author list, so title-only requests with such authors can resolve.
- BibTeX/BibLaTeX imports now split authors separated by uppercase or
  mixed-case `AND`, matching BibTeX's case-insensitive separator, instead of
  ingesting them as one concatenated name.
- `papio actions open` launches the browser on Windows and Linux (default
  browser via `rundll32`/`xdg-open`) instead of failing on the macOS-only
  `open -b` command; macOS still pins Chrome.
- Development and pre-release builds (`X.Y.Z-dev`) no longer trigger false
  "update available" nudges: version comparison strips pre-release/build
  suffixes instead of parsing the component as 0.
- Adopting a browser download now resolves the handoff action it satisfied.
  Previously the "sign in to your institution" row lingered beside the
  verification prompt for the same paper, and dismissing that stale row
  cancelled the whole job — deleting the quarantined PDF that was waiting for
  human review.
- Dismissing a human action cancels its job only when the job is actually
  parked on that action (`awaiting_human` for handoff/manual-download rows,
  `needs_review` for identity verification). A stale action from an earlier
  state is closed without disturbing the job's live work.
- `papio actions open` no longer fails with a bare "exit status 1" when the
  browser cannot be launched: the error now says the browser handoff could not
  open and points at enabling the extension and `papio doctor`.
- `papio doctor` autostarts the daemon like ordinary daemon-backed commands
  (`daemon status` and `daemon stop` still never autostart) and runs its
  database integrity check through the daemon, so a stopped daemon no longer
  produces a FAIL/WARN/SKIP cascade — a genuinely unstartable daemon is one
  failure line plus one collapsed skip.
- Unknown verbs under command groups (for example `papio jobs show`) now fail
  with the list of valid verbs instead of silently printing nothing, and a
  bare command group still prints its help.
- Accepting an identity review now promotes the exact quarantined PDF you
  verified instead of discarding it, re-resolving, and downloading the work
  again — a flow that could silently drop the acceptance when a resolver did
  not re-offer the same URL. The reviewed file is hash-checked against the
  recorded review binding before promotion; a missing or altered file still
  falls back to a fresh fetch.
- The CORE and Crossref TDM resolvers work again: both rejected the daemon's
  hardened HTTP client as unsafe and had failed every request since that
  client was introduced.
- `papio ping` and triage RPCs no longer stall behind background network work:
  the daily update check and the retraction sweep both released their caches'
  locks only after their HTTP requests finished.
- A single failed DOI lookup no longer aborts the whole retraction sweep; the
  failed DOI keeps its previous notices and the sweep commits partial results.
- A transient `accept` error (such as running out of file descriptors) no
  longer shuts down the daemon IPC server and severs active CLI connections;
  accepts now retry with bounded backoff.
- Coalesced notifications delayed into a later window are no longer dropped:
  the flush timer detached from the request context that scheduled it.

## [0.10.1] - 2026-07-23

### Fixed

- Direct-distribution release manifests now record the bundled zotio version,
  rather than incorrectly repeating the papio release version.

## [0.10.0] - 2026-07-23

### Added

- **Zotero exception-tag ledger** (`zotio.exception_tags`, off by default):
  the daemon now answers "is this item coming, or is it mine now?" inside
  Zotero itself by maintaining two reconciled automatic tags on linked items —
  `papio:needs-action` (acquisition parked on a human action) and
  `papio:unavailable` (OA and institutional routes exhausted, as of the last
  attempt). Desired state combines each item's newest job with a live
  exact-key attachment check, so a PDF attached manually clears the marker.
  Schema v14 rolls the v13 applied-state ledger forward with personal-library
  provenance plus pending/owned/foreign/missing-target state; per-item
  mutations survive partial failures, serialize concurrent passes, and never
  claim or remove a same-name manual tag.
  `papio zotio tags reconcile` runs one pass on demand. Requires zotio ≥
  0.13.0 (`items tags add --automatic`, `items tags remove --automatic-only`);
  personal library only. Turning the feature off cleans up papio-owned tags.
  Design record: zotio `dev/adr/0004`.
- Backfill re-checks `unavailable` items after a cool-down
  (`zotio.unavailable_recheck_days`, default 14) instead of retrying them on
  every watch cadence — availability drifts upward (green-OA deposits,
  holdings changes, new adapters), and a `papio:unavailable` saved search
  doubles as an ILL/manual worklist only if it neither rots nor flaps.

### Changed

- `papio init` autodiscovers more of its own answers: the zotio executable is
  resolved from PATH and labeled with its source, prompt defaults name where
  they came from (`keep current` vs `from Zotero`), the Browser step names
  the supported browsers and lists those detected on the machine, the Chrome
  extension ID prompt and `--extension-id` accept an unpacked extension
  *folder path* and compute the ID from it (macOS/Linux; Windows requires
  the literal ID), and the browser setup instructions are suppressed when
  doctor reports a healthy, current extension connection.

### Fixed

- Upgraded `golang.org/x/text` to 0.39.0, fixing GO-2026-5970 (an infinite
  loop on invalid normalization input reachable through PDF inspection).

## [0.9.2] - 2026-07-22

### Fixed

- `papio doctor` no longer fails against a healthy daemon whose daily update
  check has results: the strict ping decode now accepts the optional
  `update_available`/`latest_version` fields (and their zotio counterparts).

## [0.9.1] - 2026-07-22

### Changed

- Doctor's extension check is floor-aware: a connected extension below the
  daemon's minimum version now WARNs with the exact skew and remediation
  instead of reporting a bare `connected` PASS while every handoff refuses.
- `papio init`'s guided flow was redesigned: numbered step sections with
  one-line explanations, one question per prompt, a single `[Y/n]` grammar,
  middle-ellipsized long defaults, per-browser setup instructions, a doctor
  summary that shows only checks needing attention, and a closing `Next:`
  action. Behavior and flags are unchanged.

## [0.9.0] - 2026-07-22

### Added

- **Institution discovery in `papio init`**: paste a library discovery URL or
  use the resolver configured in Zotero to derive the OpenURL base; scripted
  setup supports `--institution-url`.
- **`papio adapter diagnose <job-id>`** — sanitized, paste-safe support report
  for provider/adapter failures (job policy, actions, redacted event timeline,
  extension connectivity).
- **Library-completeness line in `papio status`**: when zotio is configured,
  status leads with `Library: N item(s) missing PDFs — papio acquire
  --from-zotio fills them` (or `Library: complete`), backed by a new
  `zotio.missing_count` RPC over zotio's local mirror.
- **`papio doctor --start`**: opts doctor into the same daemon autostart every
  ordinary command uses, so a first run can bring the stack up instead of
  reporting it down. Default behavior still diagnoses a stopped daemon.
- **`papio init` defaults the Chrome extension ID to the Chrome Web Store
  package** (`npccengdhjmpojpjmjoeeclpdhcjelhf`) — a store install needs no ID
  copying; only unpacked development builds still paste theirs.
- Triage snapshot schema 2 exposes institutional sign-in/open-access guidance
  only for actions classified by the acquisition waterfall; schema 1 remains
  available unchanged for older browser extensions.
- **Standard bibliographic ingestion for `acquire --batch`**: RIS, BibTeX,
  CSL-JSON, and MEDLINE/NBIB use one robust interchange pipeline for reference
  managers, database exports, and systematic-review tools.
- **Provider compatibility matrix**: a hand-maintained page records the
  individually live-verified provider routes, adapter status, and limitations
  without inventing aggregate success rates.

### Changed

- Doctor's daemon-unreachable report collapses the four dependent SKIP lines
  into one `integrations` line and names the actual problem ("not running or
  unreachable") with the `--start` remediation, instead of a six-line cascade
  for a stopped daemon.
- Opener failures in `papio actions open` now name the failing command even
  when it produced no output (no more bare `exit status 1`).
- Getting-started, README, and troubleshooting now document the Chrome Web
  Store install as the default path; unpacked loading moved under a
  development-install tab. Firefox remains a temporary add-on until the AMO
  listing clears review.
- **BREAKING:** Access mode `maximal` is now `delegated` in the config value,
  `--access-mode`, and wire enum. Existing configs must be updated by hand;
  stored job policies migrate automatically.

## [0.8.0] - 2026-07-21

### Added

- **`[hooks] on_ready`** — a generic post-acquisition hand-off: when a job
  reaches `ready`, the daemon runs a user-configured shell command once with
  the job's metadata in `PAPIO_*` environment variables (`PAPIO_DOI`,
  `PAPIO_PDF`, `PAPIO_TITLE`, …). Fire-and-forget with a durable
  `hook.on_ready` job event as the audit trail; file acquisitions into papis,
  Calibre, a plain folder, or any script without a bespoke integration
  (ADR-0004). zotio remains the deep Zotero path.
- **zotio is now optional**: an empty `zotio.executable` disables the deep
  Zotero integration instead of rejecting the config. Ownership lookup
  degrades to not-owned with a staleness warning so `batch submit` and
  watches keep working, `doctor` reports zotio as "not configured
  (optional)", and `zotio.auto_import = true` still requires the executable.
- **Browser session arbitration** (`papio browser sessions` / `papio browser
  use <id>|--latest`, +`browser.sessions`/`browser.claim` RPCs): with *papio*
  installed in more than one browser, exactly one session now holds the
  offer/handoff flow. Later hellos wait as pending (`session_busy`) instead of
  silently stealing the session; a holder silent for 10 s yields to a live
  pending session; a cleanly closing browser releases immediately. Session
  identity rides the daemon↔native-host envelope, so extensions of every
  version participate. `papio status` and `doctor` report waiting browsers and
  denied hellos, and the extension-version flap between competing browsers is
  gone (ADR-0003).
- **Handoff actions say whether a login is needed**: human actions carry
  `requires_auth` and `blocked_by` (`anti_bot`, `paywall`, `landing_page`),
  `papio actions list` prints "open access — no login needed" vs "sign in to
  your institution first", and batch reports classify handoffs from the
  structured fields (schema v11 migrates and backfills existing parked
  actions). The `job_offer` protocol message carries `requires_auth` for the
  extension.
- **Open-access sibling fallback**: when a DOI's own record yields no legal
  candidate — or every candidate it did yield fails to fetch — the OpenAlex
  resolver searches for open-access sibling versions (preprints or repository
  copies under a different DOI) with strict title/year/author matching and
  tries those before parking the job — a paywalled paper with a free
  SSRN/arXiv copy no longer requires a manual re-search.
- **OpenAlex works without an API key**: the resolver now runs in OpenAlex's
  free polite pool with just the configured contact email — matching the
  search/discovery client — and treats `sources.openalex.api_key` as optional
  premium capacity. `doctor` reports the polite-pool state instead of failing.
- **Stale-SSO handoff recovery**: the daemon records `browser.handoff_offered`
  and `browser.handoff_failed` job events (new `handoff_outcome` protocol
  message), and institutional handoff guidance now says to sign in first and
  simply re-open on a stale-session error — every open mints a fresh link.
- **`imported` terminal job state**: a successful `zotio apply` (manual or
  auto-import) advances the job `ready → imported` with the Zotero item keys
  on the transition, so `jobs list` and `status` stop presenting filed work as
  actionable; batch reports read the keys from the job itself.
- `--label` now works on single `papio acquire` (not just `--batch`), seeding
  the target collection when `--collection` is unset.

### Fixed

- `papio actions open` failures now name the URL and underlying error (with a
  `papio doctor` hint) instead of a bare `exit status 1`, and explain when
  open actions exist but none are openable from the CLI.

## [0.7.2] - 2026-07-20

### Fixed

- Scheduled backfills now scan the complete zotio missing-PDF queue and skip
  already-live request IDs without consuming the per-run cap, preventing one
  persistent entry from starving later work.
- Alert-watch digests now retain consumed identities as a durable seen set,
  merge title, arXiv, OpenAlex, and DOI aliases, preserve author names
  losslessly, recheck zotio ownership before acquisition, and serialize clear
  and acquire operations.
- Long failure reasons remain separate groups until after display truncation,
  so unrelated failures with a common prefix no longer collapse together.
- Semantic Scholar citation and reference snowballs now enforce year and
  open-access filters and reject ambiguous query-plus-snowball requests.

## [0.7.1] - 2026-07-20

### Fixed

- Browser-download adoption now waits for Firefox `.part` writes and their
  zero-byte target placeholders to settle before validating the final file,
  preventing valid PDFs from being quarantined mid-download.
- Accepting an identity review for a browser-adopted PDF now binds the override
  to that file's SHA-256 candidate key, so the scheduler's safe re-resolution
  path reaches `ready` for the same bytes instead of parking the same PDF for
  review again.

## [0.7.0] - 2026-07-20

### Added

- **`papio jobs failures`** (+`jobs.failures` RPC): groups jobs that did not
  complete on their own (failed, unavailable, needs-review, awaiting-human)
  by state, provider host, and terminal reason, with `--since` (Go durations,
  `Nd` days, or RFC3339) — so adapter and resolver work can be prioritized
  from real failure data.
- **`papio acquire --from-digest <watch> [--keys ...]`** (+`watch.digest_acquire`
  RPC): queues pending alert-watch digest entries as acquisition jobs through
  the same submission path watches use, preserving the recorded identifiers so
  deduplication holds; entries are removed only after their manifest is
  persisted and submission succeeded. `papio watch digest clear <id>`
  (+`watch.digest_clear`) empties a digest.
- **Structured webhook notifications**: webhook payloads now carry `event`,
  `watch_id`, `watch_label`, and `count` alongside `message` for routable
  receivers; plain notices keep the exact previous `{source, message,
  sent_at}` shape.
- **Click-to-acquire support**: the daemon accepts the new `page_acquire`
  browser-protocol message (advertised via the `page_acquire` `hello_ack`
  feature) and queues the page's DOI through the normal acquisition pipeline,
  answering with `page_acquire_ack` (job id or duplicate marker).

### Fixed

- `papio acquire --from-digest --keys` now treats every `--keys` value as one
  opaque work key: keys containing commas (title-derived digest keys) are no
  longer split into fragments that miss valid entries. Repeat the flag for
  multiple keys.
- `papio jobs failures --since` rejects negative durations (`-1h`, `-1d`)
  instead of silently computing a future cutoff that hid every failure.

## [0.6.0] - 2026-07-20

### Added

- **Backfill watches**: `papio watch add --kind backfill` schedules the
  existing `acquire --from-zotio` queue on a cadence, so a growing Zotero
  library steadily self-completes its missing PDFs. Runs are bounded by
  `--limit-per-run` and idempotent (deterministic per-item request IDs), and
  the watch is force-runnable with `papio watch run` like any other.
- **Alert-only watches**: `papio watch add --mode alert` runs the scheduled
  discovery search and library-ownership filter but *reports* new works
  instead of acquiring them. New finds are recorded once per watch (re-runs
  never re-report) and browsable with the new `papio watch digest <id>`
  command; notifications point at the digest.
- **Webhook notifications**: a new `notify.webhook_url` config field delivers
  every daemon notification (watch results, human-action handoffs, imports) as
  a JSON POST — Slack/Discord/ntfy-style receivers work out of the box — in
  addition to the local desktop channel. Optional `notify.webhook_secret` is
  sent as a bearer token. Delivery is best-effort and never fails the work
  that triggered it.
- **Semantic Scholar discovery backend**: discovery is now pluggable behind a
  source seam. `discovery.sources = ["openalex", "semanticscholar"]` in config
  fans searches (and watches) across both backends with DOI/title
  deduplication in preference order; `papio search --source` selects one
  explicitly. Citation snowball (`--cites`, `--cited-by`) is supported on both;
  arXiv-only Semantic Scholar results now carry their identifier through to
  acquisition. API key (optional) lives at `sources.semanticscholar.api_key`.

### Changed

- The MCP read resources (`papio://jobs`, `papio://artifacts`,
  `papio://exports`, …) now return `{"<name>": [...], "truncated": bool}`
  envelopes instead of bare arrays, making the 100-row cap honest. Filtered
  and paginated access remains available through the command facade
  (`jobs list --state --limit`).

## [0.5.0] - 2026-07-19

### Added

- Broader browser reach for the native-messaging connector. `papio native-host
  install` now registers the host with every installed Chromium browser it
  detects — Chrome, Edge, Vivaldi, Brave, Opera (Chromium too) — plus Firefox,
  each at its own per-user location (directory on macOS/Linux, registry key on
  Windows), so the same extension works across them. A new `browser.extension_ids`
  config field lists additional Chrome-family extension IDs (e.g. an Edge
  Add-ons build) alongside `extension_id`; the daemon accepts any of them and the
  manifest's `allowed_origins` lists them all.

### Fixed

A triaged audit pass (33 confirmed findings fixed, each with a
regression test where behavior changed):

- OA resolver identity verification: CORE, Europe PMC, and OpenAlex title
  searches now verify the normalized title — plus publication year and the
  full author list whenever the request supplies them — before trusting a
  result; Unpaywall requires the returned DOI to match the requested one;
  arXiv compares exact version-stripped IDs instead of substring matching.
  Cuts wrong-paper acquisition risk across the discovery plane.
- PDF identity matching scopes DOI and supplementary-material signals to the
  document's front matter, so a bibliography citing other DOIs — or a body
  mention of "supplementary material" — no longer rejects a correct article.
- Download safety: caller-supplied headers (Authorization, API keys) are
  stripped on cross-origin and HTTPS→HTTP redirects, and the body-reader
  goroutine no longer leaks when a response ignores cancellation.
- Storage integrity: bundle export and Zotero plan staging copy artifacts
  instead of hard-linking the immutable store (a consumer mutating the copy
  could corrupt it); concurrent same-hash promotions converge atomically;
  failed exports roll back the files they created; failed SQLite backups no
  longer strand a partial destination file; promotion and backup fall back
  gracefully on filesystems without hard-link support.
- Concurrency: RPC calls on separate IPC connections no longer serialize
  daemon-wide behind one slow call; the browser bridge releases its session
  lock during PDF validation on download adoption; the serial auto-importer
  releases its lock during retry backoff; concurrent zotio plan applies of the
  same plan are now mutually exclusive, and a claim abandoned by a crash or
  cancellation heals after a 15-minute lease instead of wedging the plan.
- Job lifecycle: context cancellation during auto-import stays retryable
  instead of recording a permanent failure; crash recovery clears abandoned
  quarantine files and the quarantine sweep continues past individual cleanup
  failures; a validation-persistence failure can no longer orphan a
  just-promoted artifact; watch-discovered works keep their OpenAlex
  identifier; pending notifications flush on a timer instead of waiting for
  the next event; `ping` answers from cache instead of blocking on the daily
  update check.
- Protocol strictness: strict JSON decoding rejects trailing documents; batch
  submissions reject unknown fields; Zotero item keys are validated
  fail-closed at the zotio integration boundary (the published v1 protocol
  contract is unchanged); batch identity hashes widened from 32 to 128 bits,
  with manifests from earlier releases still readable.

## [0.4.0] - 2026-07-19

### Added

- First-class Windows support. The daemon's local RPC runs over a named pipe on
  Windows — restricted to the current user via an explicit SDDL, the analog of
  the Unix socket's `0600` — while macOS and Linux keep their Unix-domain
  socket; the transport is chosen at build time. `papio init`, `papio
  native-host install/uninstall/status`, and `papio doctor` register the browser
  connector through the per-user registry
  (`HKCU\Software\...\NativeMessagingHosts`) instead of a manifest directory,
  and — because Windows has no unprivileged symlinks — install a copy of the
  `papio` binary as the native host (rerun `papio init` after upgrading to
  refresh it). Configuration lives at `%APPDATA%\papio` and data at
  `%LOCALAPPDATA%\papio`, and the update hint recognizes Scoop
  (`scoop update papio`). macOS and Linux behavior is unchanged.

## [0.3.0] - 2026-07-18

### Added

- Automatic, institution-agnostic library-resolver access. When a library's
  OpenURL resolver shows a "full text options" menu instead of direct-linking
  to the provider, *papio* follows the institution's top-ranked electronic
  service link itself — gated on a host permission for that resolver origin.
  The daemon advertises its configured resolver origins in the `hello_ack`
  handshake (new optional `resolver_origins`, backward compatible within
  `papio-browser/1`); the extension requests exactly those origins, so the
  popup surfaces a one-click "Allow library access" prompt (and the toolbar
  badge counts them) whenever a configured resolver isn't granted yet, and the
  options page lists the user's own resolvers under "Your library". Custom
  resolver domains outside the built-in Ex Libris hosts are reached through an
  optional `https://*/*` pattern that is never granted in bulk — only the exact
  configured origin is ever requested. Institution identity lives only in
  `config.toml`, never in extension code.

- Update discovery, without auto-install and without silent network calls.
  Store-delivered extension builds are stamped with the daemon version they
  shipped with, so the popup can show a calm "papio X.Y is available" line
  when the connected daemon is older — *papio* itself performs no network
  activity for this. Separately, an opt-in `[updates] check = true` setting
  (offered by the `papio init` prompt, default yes) has the daemon consult the
  *papio* and zotio GitHub releases APIs independently at most once a day. *papio*
  status appears in daemon status; both targets surface in `papio doctor` and a
  once-daily stderr hint. Configurations without the setting never check.

- Version-skew awareness across every surface. The `hello_ack` handshake now
  carries the daemon's version and a feature list (optional, backward
  compatible within `papio-browser/1`), so the auto-updating extension can
  degrade gracefully against an older daemon instead of failing opaquely. The
  popup reports daemon health directly: a quiet version line when healthy, and
  actionable states for daemon-unreachable, daemon-out-of-date, and
  extension-out-of-date; the toolbar badge shows `!` when attention is needed
  and stays clear otherwise. The options page footer shows extension and
  daemon versions at a glance. The daemon records the connected extension's
  version and rejects extensions below a minimum floor with a clear
  update-the-extension message.

- `papio doctor` now walks the whole integration chain in one report: the
  Phase-1 readiness checks (config paths, database, PDF tooling, credentials)
  followed by integration checks — daemon reachability and version match,
  browser-extension connectivity, native-messaging-host manifests for Chrome
  and Firefox, and the zotio preflight — each failure with a concrete `fix:`
  line. The same diagnostics are exposed to agents as a read-only
  `papio_doctor` MCP tool.

- Every CLI command now warns on stderr (once per invocation, never on
  stdout) when the running daemon's version differs from the CLI binary,
  with the exact recovery command.

- Release engineering: `release_metadata.py compat` mechanically verifies the
  cross-artifact compatibility floors (daemon↔extension minimums, zotio
  minimum version, extension manifest/package version agreement) as a
  `release.sh` step and a source-only CI check; `release.sh` now also
  packages the Firefox extension archive alongside the Chrome one. A shared
  release runbook lives at `.agents/skills/papio-release/SKILL.md` and is
  cross-referenced from zotio.

- Extension store submission path for Chrome Web Store and Firefox Add-ons
  (AMO). `extension/scripts/submit-firefox.sh` signs and submits the built
  Firefox package via `web-ext` (AMO API credentials from `extension/.env`),
  and `extension/scripts/submit-chrome.sh` uploads the Chrome package via
  `chrome-webstore-upload-cli`; both are exposed as `bun run submit:firefox`
  and `bun run submit:chrome`. Paste-ready store listing kits (name, summary,
  full description, per-permission rationale, data-collection disclosure, and
  reviewer build instructions for the bundled source) live at
  `extension/docs/amo-listing.md` and
  `extension/docs/chrome-web-store-listing.md`.

- The bundled-zotio compatibility floor (`internal/zotio/client.go`
  `MinimumVersion`) now targets a released zotio line (`0.10.0`) instead of an
  unreleased `1.0.0`; a built zotio 0.10.0 satisfies every capability, operation,
  and write-target *papio*'s preflight requires. `release.sh` now stamps the
  bundled zotio binary with zotio's own version rather than *papio*'s, so the
  cross-artifact compatibility check reflects the real zotio being shipped.

- Documentation: a `Version skew and updates` troubleshooting section (update
  flow, popup states, config-newer-than-binary errors), sister-project
  cross-references between *papio* and zotio in both READMEs and docs, and
  regenerated command reference.

- The MCP `papio_status` tool now surfaces the same actionable `category` and
  `guidance` as the CLI for parked and no-file jobs (including the config-aware
  `institution_not_configured`), so agents driving *papio* over MCP get the same
  diagnosis and next step as a human. The category catalog moved to a shared
  `internal/errcat` package consumed by both the CLI and the MCP server, so the
  two surfaces cannot drift.

- `papio init` now captures the browser extension IDs during first-run setup, so
  the native messaging host installs on the first run instead of failing with
  `browser.extension_id is not set` and forcing a config hand-edit and re-run.
  The Firefox add-on ID defaults to the built extension's fixed gecko id
  (`papio@orgmentem.com`) so Firefox works out of the box; the Chrome ID is
  prompted (paste the value from `chrome://extensions`). New `--extension-id`
  and `--firefox-extension-id` flags cover non-interactive setup. Unit-tested
  (flag and interactive paths, including that the captured Chrome ID reaches the
  native-host install) and smoke-verified end to end.

- Actionable error categories in `papio status`. Every parked or settled-without-
  a-file job now shows a short, stable category and a one-line next step instead
  of a raw internal reason (or nothing, for failed/unavailable). The catalog is
  config-aware: a job that found no copy under assisted/maximal mode with no
  institution configured surfaces as `institution_not_configured` pointing at
  `papio init`, rather than a silent `unavailable`. Categories/guidance are added
  to the status JSON (`category`, `guidance`) for agents. The same category and
  next-step now print under `papio acquire --wait` when a job parks or settles
  without a file, and the desktop human-action notification tells the user to
  `run papio status to see why` instead of a bare count. Unit-tested; the status
  view and `acquire --wait` guidance are smoke-verified against the live daemon.

- Per-institution access profiles and guided institution onboarding. Named
  resolver profiles under `[browser.resolvers.<name>]` are now full institution
  tables (`openurl_base_url` plus optional `shibboleth_entity_id` and
  `proquest_account_id`), so a multi-institution user routes each job's login to
  the right library. This lifts the earlier "default profile only" limitation on
  federated login-routing and the ProQuest account-id unlock: the daemon now
  wires `login_entity_id`/`proquest_account_id` per selected profile, and a
  named institution never inherits the default institution's identity.
  `papio init` gained an "Institution" step (and `--openurl-base`,
  `--shibboleth-entity-id`, `--proquest-account-id` flags); the ProQuest prompt
  accepts a pasted resolver URL and extracts `accountid=` for users who don't
  know their numeric id. Config validation, per-profile offer wiring, and the
  account-id extractor are unit-tested; the interactive flow is smoke-verified
  end to end.
  Older single-base configs keep loading: a resolver profile may still be a bare
  `name = "https://…"` string (shorthand for `openurl_base_url`), so no config
  migration is required.

- ProQuest account-id unlock: on ProQuest's "Find your institution" wall, *papio*
  appends `?accountid=<id>` to the current URL, which unlocks Example University's
  institutional access with **no sign-in at all** (verified live 2026-07-18 —
  resolves the wall cold, "Access provided by EXAMPLE UNIVERSITY"). New
  per-institution config `[browser] proquest_account_id` (digits); the daemon
  passes it as the optional job-offer field `proquest_account_id` (default
  profile only); the ProQuest adapter gains `accountIdParam: "accountid"`, and
  on a `login` verdict *papio* appends it (latched, once) — preferred over the
  federated route since it needs no credentials. This is the fix for the
  ProQuest openurl-handler blocker (the Shibboleth-DS route authenticated only
  ProQuest's main context, not the link-resolver handler). Config + protocol +
  adapter + bridge are unit-tested; the full download still needs a live pass on
  a ProQuest-*held* title.

- Institution auto-selection ("login routing"): on a provider login wall, *papio*
  navigates the handoff tab straight to the institution's federated login,
  skipping the provider's institution picker — selection is deterministic config
  (which institution you're at), not a secret, so only credential entry stays
  with you. New per-institution config `[browser] shibboleth_entity_id` (the
  Shibboleth IdP entityID, e.g. Example University's `https://idp.example.edu/entity`); the
  daemon passes it to the extension as the optional job-offer field
  `login_entity_id` (default resolver profile only, to avoid mis-routing another
  institution's job); and an adapter gains an optional `federatedLogin` template
  (`{entityID}` placeholder). On a `login` verdict *papio* navigates once (latched)
  to `<federated-login>?entityID=<configured>`. ProQuest ships the route
  (verified live 2026-07-17: Example University's entityID via ProQuest's Shibboleth DS URL
  routes straight to `idp.example.edu` login, skipping the WAYF picker). Config +
  protocol + adapter classify + bridge routing are unit-tested; the full
  post-sign-in download on a ProQuest-held title still needs a live pass.

- ProQuest institution-wall handling (`proquest` adapter v0.2.0): a `login`
  classify rule (ordered before `article`) now recognizes ProQuest's "Find your
  institution" wall (`form#institutionForm` + `input#institutionName`,
  fixture-backed `fixtures/proquest/login-return.html` captured live via CDP).
  *papio* surfaces it as a human sign-in step (`login` → `auth_pending`) instead
  of silently staying assisted/`unknown`. Matters disproportionately because
  Example University's OpenURL resolver routes many titles (incl. SAGE/T&F journals) to
  ProQuest rather than the publisher. Classify verified by fixtures; the full
  post-sign-in download recovery (authenticate ProQuest → re-drive → entitled
  docview → download) still needs a live pass.

- SAGE Journals adapter (`journals.sagepub.com`), fixture-backed
  (`fixtures/sage/success.html`, captured live via CDP from an institutionally authenticated
  article). SAGE emits no Highwire metas; classifies on `publication_doi` + the
  `downloadPdfUrl` anchor (same shape as ACM) and downloads that anchor's
  `/doi/pdf/<doi>?download=true` href. Classify is fixture-verified; the
  end-to-end download is not yet live-exercised because Example University's resolver routed
  the SAGE test title to ProQuest rather than sagepub (the adapter fires when a
  title routes to journals.sagepub.com).

- Wiley Online Library adapter (`onlinelibrary.wiley.com`), fixture-backed
  (`fixtures/wiley/success.html`, captured from an institutionally authenticated article).
  Classifies via the Highwire `citation_pdf_url`/`citation_title` metas, then
  builds and fetches Wiley's direct `/doi/pdfdirect/<doi>?download=true` file
  through the privileged downloads API — `citation_pdf_url` (`/doi/pdf/`) and
  the `/doi/epdf/` link both return an HTML viewer wrapper, only `pdfdirect`
  returns the file (verified live end-to-end: 1.15 MB PDF → `ready`). Closes the
  gap where Wiley pages classified `unknown` and stayed assisted (browser-
  agnostic; affected Chrome too). tandfonline/psycnet remain unimplemented —
  permissioned but not yet fixture-backed (both paywalled in the dev session;
  psycnet also emits no standard metadata).

- Firefox dev loop: `bun run dev` runs `build.ts --watch` (rebuilds `firefox/`
  on any `src/`, `icons/`, or `manifest.json` change) alongside `web-ext run`,
  which hot-reloads the add-on in a dedicated Firefox Developer Edition instance.
  `web-ext-config.mjs` pins an absolute, gitignored dev profile
  (`.ff-dev-profile`) so permissions and institutional logins persist across
  reloads — and, being path-based, boots straight in without Firefox's
  profile-chooser modal. web-ext installs and hot-reloads over the devtools
  RDP (not WebDriver/Marionette), so it does not set `navigator.webdriver` — but
  that live RDP connection makes Firefox show its remote-control indicator and
  is itself an automation surface a bot wall could fingerprint. Two modes:
  `bun run dev` for fast iteration and fixture testing; for real Cloudflare-
  walled providers, `bun run build` then load `firefox/` manually via
  `about:debugging` (one-shot install, no persistent connection, no indicator,
  `navigator.webdriver` false).

- Brand: a *papio* logo — an oblique lowercase **p** (coral `#E85D4A`) inside a
  broken ink ring (`#2B2D42`); the p's descender becomes a download arrow that
  exits through the ring's bottom gap. Structural sibling of the zotio badge
  with its own palette. Vector sources live in `docs/assets/` (`logo.svg`,
  `logo-dark.svg` for dark surfaces, `logo-tile.svg` for theme-agnostic toolbar
  icons, `logo-wordmark.svg`, `logo-wordmark-dark.svg`) and are used in the
  README wordmark header, the docs site logo/favicon (`mkdocs.yml`), the Chrome
  extension toolbar/action icons (`extension/icons/`, wired in
  `manifest.json`), and the extension popup header.
- Brand: the README header wordmark (`logo-wordmark.svg`,
  `logo-wordmark-dark.svg`) is now an animated SVG. The mark builds in on a calm
  ~10s loop — the broken ring draws on, the coral **p** and download arrow drop
  into place, the wordmark rises in — then a cheeky little papio (baboon) head
  peeks over the wordmark to blink, tilt, and wave before ducking away, leaving a
  long clean hold on the finished logo. Pure CSS (no script/SMIL, self-contained
  for GitHub's `<img>` rendering); the resting state is byte-for-byte the prior
  static logo and `prefers-reduced-motion: reduce` shows it with no animation.
- Background work window: *papio* now does its browsing in one dedicated
  minimized, unfocused Chrome window instead of the user's tab strip. Every
  broker handoff tab (first, queued, and download-fallback) and the keepalive
  tab route there; provider-spawned viewer tabs inherit it via their opener.
  A tab surfaces (window restored + focused, tab activated) only when the
  human is needed: on the IdP transition (`auth_pending`), on keepalive
  reauth, and from the popup's Focus button — which now also restores a
  minimized window. Opt out anytime via the options page ("Keep papio tabs in
  a background window"); disabling restores the legacy visible-handoff
  behavior, as does any runtime without `chrome.windows`.
- Firefox support, day one: `bun run build` now emits a second complete
  extension at `extension/firefox/` (MV3 event-page background as a classic
  iife bundle, `browser_specific_settings.gecko.id = papio@orgmentem.com`,
  `strict_min_version 128`) generated from the same `manifest.json` source of
  truth. The native-host installer registers a Firefox manifest
  (`allowed_extensions`, Mozilla `NativeMessagingHosts` dir) alongside
  Chrome's when the new `[browser] firefox_extension_id` config is set, and
  the host accepts Firefox's bare-ID invocation with the same exact-match,
  fail-closed validation as Chrome's origin. The options page gained a
  "Library resolver access" grant section because Firefox treats MV3
  `host_permissions` as runtime-optional; on Chrome it simply shows the
  install-time grants. No behavior change for Chrome users. The provider
  section also gained "Grant all providers" / "Revoke all" — one click issues a
  single `permissions.request` for every publisher origin (one Firefox
  doorhanger) instead of ten separate grants.

### Changed

- Rewrote the README on the zotio template: centered wordmark + tagline +
  badges + docs nav, a "Why papio" section with the hard boundaries, a
  hand-drawn two-row serpentine architecture diagram in the brand palette
  (`docs/assets/architecture.svg` + `-dark.svg`, theme-switched via a
  `<picture>` element; replacing the mermaid flowchart, which rendered
  poorly on GitHub) with the
  access-mode table, the research loop, validation/provenance and
  zotio-boundary sections, the MCP tool surface, and install paths (brew,
  scoop, signed releases, source). Brand style: *papio* italic in prose,
  zotio plain.
- Redesigned the wordmark's baboon cameo: the abstract head is now a
  recognizable hamadryas baboon (cape mantle, long muzzle, heavy brow) that
  peeks up holding a stack of papers instead of waving. Light and dark
  wordmark variants stay in sync; in dark mode the paper stack renders navy
  against the cream mantle for contrast, and the face details (eyes, brows,
  muzzle) stay navy on the coral face in both modes.
- Config unknown-field errors now explain that the config was likely written
  for a newer *papio* and name the offending fields, instead of surfacing a raw
  TOML parse error. zotio preflight failures name the installed version, the
  configured executable path, and the action that fixes the mismatch.
- MCP tool surface now derives from the *papio* CLI command tree instead of a
  parallel set of hand-maintained typed tools, so the CLI is the single source
  of truth and the two can no longer drift. The default surface is a command
  facade — `papio_command_search` to discover commands and `papio_command_run`
  to execute one (JSON output, command-local flags only, inherited globals
  rejected); `PAPIO_MCP_SURFACE=mirror` instead exposes one `papio_<command>`
  tool apiece. Setup and lifecycle commands (`init`, `config`, `daemon`,
  `native-host`, `mcp`) are hidden via `mcp:hidden` annotations. Two composite
  tools with no single-command equivalent stay first-class — `papio_acquire_batch`
  (bulk work input) and `papio_batch_wait` (bounded polling) — alongside the
  five read resources. Migrated the server library from
  `modelcontextprotocol/go-sdk` to `mark3labs/mcp-go` for parity with zotio.

### Fixed

- Reliability: overlapping extension state writes are now persisted through a
  serialized save chain, so a reordered `chrome.storage` write can no longer
  resurrect a stale snapshot after a service-worker restart.
- Reliability: concurrent queued-handoff fallback timers no longer drop each
  other's forced releases; a single drain loop consumes every pending release,
  so queued jobs can no longer be stranded invisibly with `tab_id -1`.
- Reliability: a failed native-host idle-poll write now tears the bridge down
  instead of leaving the process alive but no longer polling (which starved the
  extension of offers and cancels).
- Reliability: `fetchCandidates` propagates the `OpenHumanAction` write error
  before parking a landing-page-only job, matching `exhaustedCandidates`, so a
  transient write failure can no longer strand a job with no human-action row.
- Concurrency: removed a redundant drain goroutine in `readBodyWithContext`
  that doubled leaked goroutines when a response body read hung.
- MCP `acquire.report` now classifies failures — missing batch as `not_found`,
  malformed batch ID as `invalid_argument`, and other failures as `internal` —
  instead of collapsing every error into `not_found`.
- Batch settlement is now a single source of truth (`batch.Report.Settled`),
  removing a stale duplicate outcome list in `papio_batch_wait` that carried
  legacy outcome spellings.
- Docs/schema for `papio_batch_wait` `timeout_seconds` now state that `0` or an
  omitted value defaults to 300, matching the implementation.

## [0.2.0] - 2026-07-15

### Phase 0 — contracts and prerequisite

- Established the *papio* Go/Bun workspace, fail-closed shared protocol fixtures,
  and draft work-request, acquisition-bundle, and browser contracts.
- Added zotio's stored-attachment upload path with reconciliation and retry-safe
  Web API registration, which is the import prerequisite for *papio* exports.

### Phase 1 — durable open-access acquisition

- Added private configuration, SQLite migrations, daemon IPC, durable job and
  lease recovery, source budgets, redacted events, quarantine, and content-hash
  artifact storage.
- Added normalized work identity, deterministic candidate ranking, bounded
  HTTPS acquisition, PDF validation, OCR fallback, and review/rejection paths.

### Phase 2 — institutional browser handoff

- Added the native-host bridge, versioned bounded browser protocol, native-host
  install/status commands, and a least-privilege MV3 extension for one requested
  institutional download per job.
- Added adoption confinement and validation for browser downloads, with
  restart-safe daemon and extension lifecycle handling.

### Phase 3 — provider adapters and protocol lock

- Added declarative, permission-gated adapter execution and sanitized fixture
  capture for ProQuest, JSTOR, EBSCO, and Springer flows.
- Locked `work-request/1`, `acquisition-bundle/1`, and `papio-browser/1` with
  strict cross-runtime fixtures; retained Go as the core after the reversal
  review.

### Phase 4 — zotio, MCP, and human resolution

- Added zotio capability/version preflight, preview/apply plans, confirmation
  hashes, import-ledger idempotency, missing-PDF intake, and stored attachments.
- Added MCP tools and resources over the same application service, plus bounded
  human identity-review resolution and action lifecycle cleanup.
- Added extension session recovery across daemon restarts and startup wake-up.

### Post-Phase 4 — autonomous acquisition

- Added OpenAlex discovery, batch acquisition, serialized retry-safe auto-import,
  session keepalive, observed-provider fixture capture, library-aware batches,
  OA browser fallback, snowball search, status/reporting, notifications,
  watchlists, MCP loop closure, and first-run onboarding.
- Updated zotio integration with collection-aware missing-PDF scopes, item-type
  valid container-title mapping, exact-key enrichment, and transactional
  workflow execution.

### Phase 5 — release preparation

- Added local release artifacts for *papio* and zotio binaries, the extension ZIP,
  dependency inventories, license reports, hashes, and a machine-readable
  release manifest.
