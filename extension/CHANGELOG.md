# Changelog — browser extension

All notable changes to the *papio* browser extension (Chrome/Firefox MV3,
`extension/`) are documented here. The extension is versioned and released
independently of the daemon: its version lives in `extension/manifest.json`
(and must match `extension/package.json`), and a release is cut with an
`ext-v<version>` tag. Daemon/CLI changes live in the root `CHANGELOG.md`.

A change that spans the native-messaging protocol appears in **both**
changelogs — each file describes the behavior change visible to users of that
artifact.

History before 0.3.1 was recorded in the root `CHANGELOG.md` (the extension
and daemon shared a version stream through 0.3.0); see its `[0.3.0]` section
for the full pre-split extension history.

## [Unreleased]

### Added
- **Sending a DOI-less PDF asks which paper it belongs to instead of asking you to pre-pick one.** When you click **Send PDF** and no exact tab or page-DOI correlation exists, the popup now shows *Which paper is this?* over the list of papers still awaiting a download — the inbox **Open** step is gone, and **Open** in the inbox is now a plain link. Picking a paper binds those bytes to that job alone; there is no ambient pin that a DOI-less PDF in another tab could borrow. **Send PDF** keeps joining by the current tab, then a unique page DOI, before it ever consults a pick, so a DOI-less PDF opened elsewhere is still identified from the file rather than from whichever row was opened last.

### Fixed
- **The popup no longer resizes itself over and over.** Opened, and sometimes
  after a click and a mouse-off, it flickered between two widths as fast as the
  screen could draw. A browser popup has no window size of its own — Chrome
  measures the page and sizes the window to fit — so the stylesheet asking for
  "380px, or the window's width, whichever is smaller" was asking for a width
  defined by itself. There is no answer to that question, and the browser kept
  trying: measured on the author's machine, 394px and 364px, alternating,
  forever, with nobody touching it. The width is now simply 380px, and the
  scrollbar's space is always reserved so a line of text appearing cannot start
  the same argument about height.
- **Tabs *papio* opened for a paper it never finished are now cleaned up.**
  When a paper reached your library's sign-in and *papio* ran out of time
  waiting, it always meant to close that tab — but every one of those closures
  was silently refused, so the tab stayed open forever, and a new one appeared
  for the next paper. Live on the author's machine: fifteen surviving tabs,
  thirteen of them the same resolver page, outliving the papers that opened
  them by days. *papio* now retires those surfaces.

  A tab you have touched is never taken: activating it, pinning it, opening a
  PDF in it, or dragging it out of *papio*'s window all make it yours
  permanently, checked twice — once before the close and once in the same
  instant as it, so even a click that lands mid-close is caught.

### Changed
- **The popup says what is happening, and offers the way to act on it.**
  "Waiting on you · 125 decisions" was plain text: the popup named you as the
  hold-up and then offered nothing but the two buttons for the page you happened
  to be on. That count is now the button. Four other lines were rewritten
  because they read as riddles on the author's own screen. The line under it is
  one submission, not your whole library, and now says so — "Last submission:
  199 papers · 127 settled · 72 still open", where "72 remaining" used to sit
  under a different total and look like a contradiction. The session card no
  longer chains two different times into one sentence: "Signed in — rechecking
  now · via your library tab · 1h ago" could not tell you whether the check was
  an hour old or happening now, and now reads "Signed in 1h ago — rechecking
  now". "Session warm" is now "Signed in", and "All sessions warm" is "All
  institutions signed in" — *warm* was *papio*'s word for how fresh its
  evidence is, never yours. And a paper's card no longer prints a bare line
  from the activity log where its status belongs: "Institution login returned ·
  2m ago" said what once happened and nothing about where the paper stands, so
  the standing comes first now, with the event kept and dated after it.
- **The toolbar badge no longer reports *papio*'s own queue as papers waiting
  on you.** A library is served by one sign-in at a time, so most institutional
  papers are waiting for *papio* to reach them, not for you to do anything —
  yet every one of them was counted into "N papers waiting on your institution
  sign-in". Live, the badge read 13 while exactly one paper could proceed, and
  a twenty-hour internal stall looked like *papio* patiently waiting on the
  operator. The count is now exactly the papers whose own page is sitting at a
  login screen — the ones you can actually finish — and the rest are named as
  *papio*'s work: "1 paper needs your institution sign-in · 12 more queued for
  your library". Nothing is hidden: the queued clause also appears when the ask
  count is zero, so institutional work in flight is still visible with no
  session evidence and keepalive off.
- **An acknowledgement now says whether *papio* is driving a paper or queueing
  it.** The extension drives one paper at a time and queues the rest, but both
  cases sent the same bare "got it" — so the app counted a paper's turn in the
  queue as a browser attempt that produced nothing, and gave up on it after
  three. One stuck sign-in holding the only drive slot could therefore retire
  every paper behind it without any of them ever opening a tab. The queued case
  is now named on the wire, and the disposition is read from the paper's own
  status so seventeen call sites cannot drift apart. Papers already retired
  this way are restored by the app on upgrade.
- **A *papio* reload no longer forgets the paper you were signing in for.**
  Reloading or updating the extension wipes its working state, and the daemon
  cannot re-announce a paper that already owns a live page — so a paper stopped
  at a login screen went unmentioned, and the badge read "connected" while the
  one paper you could actually finish sat there. The ask is now recovered from
  the durable record of the tabs *papio* opened: same browser session, not
  yours, still live, still on a login page. It only ever reports — it cannot
  revive work or open anything, and a *papio* tab that has moved past its login
  page reports nothing.
- **A *papio* tab you close while nothing is watching is now reported, not just
  forgotten.** Startup repair used to drop its record of a vanished tab in
  silence, so the daemon never learned the page was gone — and because that
  paper had already been granted its navigation, its claim is never expired on
  a timer, leaving the library's sign-in slot held by a paper with no page.
  Measured live: closing *papio*'s tab group during a reload did exactly that.
  The loss is now reported from the same durable record, using the same
  one-time closing vocabulary an observed close uses.
- **A paper's sign-in progress is no longer thrown away when the browser
  reconnects.** Every queued sign-in observation was stamped with the browser
  session it was made in, and the daemon answers anything from an older session
  "stale" — so a reconnect between making an observation and sending it
  discarded the whole backlog the reload path exists to preserve, silently.
  Observations now carry the session they are *sent* from; which paper and
  which sign-in they describe was never in that field.
- **Closing every *papio* tab and then reloading no longer hides the closures.**
  The reload could only recognise its own tabs by finding one still open, so
  closing all of them made it look like a browser restart, and every tab it
  should have reported became unrecognised. A tab that is *gone* can't be
  confused with anyone else's, so its closure is now reported regardless.
- **A paper you ask for is no longer sent to a sign-in page that isn't there.**
  When *papio* points a paper at another paper's live sign-in and that page
  turns out to be gone, the extension now says so instead of just failing the
  click — so the next click gets a real page. This is the recovery that needs
  no memory of the tab at all: a library slot held by a paper whose page
  vanished before this release could otherwise stay held forever.
- ***papio* no longer opens institutional sign-in tabs on its own.** Every
  autonomous path that used to create a `requires_auth` surface — a warm
  session admitting a queued offer, the 45-second fallback timer, a daemon
  re-offer recreating a closed tab, startup re-driving a restored backlog,
  and the sibling resume after a login — now parks the work tabless in the
  inbox as *needs your engagement* instead, until the daemon advertises the
  ADR-0022 authentication-claim feature AND the network is up (the daemon
  arbitration that grants it, below, has since shipped). Clicking **Open**
  works exactly as before; what is gone is papio deciding by itself that
  now is the moment for a sign-in tab, which is how a tab group grew
  seventeen tabs across a weekend. Three sibling fixes land with it: a wake
  from sleep now checks connectivity before releasing queued work (tabs
  were being driven into a dead network), a repeated *Sign in* request
  reuses the live sign-in tab instead of minting another, and a
  papio-created tab that has navigated through its resolver→SSO→provider
  redirect chain is no longer forgotten by the tab ledger — it is
  remembered as papio's own, though never touched, until the
  durable-ownership work below can prove tab identity across restarts.
- **Every paper waiting on the same institution now shares one sign-in, and
  resumes together the moment it succeeds.** The tabless park above is now
  a real grant: the daemon brokers exactly one login tab per institution
  across every paper that needs it — a second paper's autonomous drive is
  told to wait behind it or focus it explicitly, never to open a duplicate
  — and when that sign-in succeeds, every sibling paper resumes on its own
  freshly revalidated route without you touching them individually. An
  abandoned sign-in (the tab closed without success) commits that outcome
  once; it does not leave a sibling guessing whether to keep waiting.
- **A papio-opened institutional tab now starts on papio's own blank page,
  never the provider's.** Automatic sign-in and access tabs open first to
  an opaque internal `materialize.html#<binding>` page and only navigate to
  the real provider once the daemon has admitted the route — the provider
  address is never the tab's first destination, and never a stored one
  either: the on-disk ledger no longer records any route, provider, host,
  or paper for a papio-owned tab, only an opaque binding id, a SHA-256
  digest of the tab's origin, and the bookkeeping needed to prove it is
  still papio's own. This replaces the previous ledger, which kept the
  actual sign-in URL in `storage.local`.
- ***papio* never closes a tab you have touched.** Every automatic close
  now requires a fresh, one-use authorization the daemon issues for that
  exact tab and reason — an idle scaffold you never engaged with, one whose
  job already finished, or one whose sign-in was abandoned. A tab that went
  active, was pinned, turned into a PDF, or was adopted into a job is
  retained instead, every time; a papio tab from before this shipped that
  cannot be re-verified this way is listed in the popup's existing
  stray-tabs review card rather than acted on.
- **A page that fails to load is no longer mistaken for a sign-in wall.** A
  dead-end navigation — a broken link, a timeout, a resolver page that
  never resolves — is now recognized before papio asks whether the page is
  an authentication wall, so it stops the attempt cleanly instead of
  charging it against your sign-in retries or opening a cooldown a real
  wall would deserve.
- **Sign-in progress is no longer lost or double-counted across a
  service-worker restart.** Every wall/login/MFA/challenge/landing/close
  event papio observes on a sign-in tab is now queued durably before it is
  sent and replayed under the same identity after a restart, so a login
  completed right as the worker restarted is not asked of you again, and
  nothing observed before the restart can be counted twice.
- **Firefox below 139 keeps working, with grouping degraded rather than
  assumed.** Visually grouping a papio sign-in tab with its siblings needs
  the `tabGroups` API (Firefox 139+); where it is absent, group identity
  now falls back cleanly to papio's existing work window instead of ever
  inferring ownership from a group's title alone. The minimum supported
  Firefox stays 128.0 ESR, the release many institutions run, unchanged by
  any of this.
- **A paper you picked no longer stays picked after you leave the PDF.** The popup's offer is now one-shot: the background mints an opaque nonce held only in service-worker memory, consumes it synchronously on selection, and freezes the page identity — tab, document, same-document navigation sequence and resolved source URL — against that pick. The offer dies when the worker restarts, when the tab navigates, closes, or is replaced, or when the delivery settles or fails; a later page in the same tab id never revives it. The native-viewer *Download* continuation is the one state that does survive a restart, held in session storage with its own job binding and page identity and re-validated when the download is claimed, and it is destroyed on every cancel or close path. This replaces the old `manual_delivery_target` pin (`Open` in the inbox) and `uniqueManualDeliveryTarget` cross-tab authority — a DOI-less file that reaches the daemon without your pick follows the blind grab path as before. To recognise when a tab has navigated away from the PDF you picked, the extension now requests the `webNavigation` permission (Chrome and Firefox); no browsing history is collected.

- **A browser that is not the session holder no longer claims the daemon is
  unreachable.** With the extension enabled in two browsers, the one waiting
  its turn showed *papio daemon isn't reachable — run: papio daemon status*,
  which answers *ok*, and its inbox promised an automatic reconnect that could
  never happen. A `session_busy` refusal is now its own state: the popup names
  the situation and the single command that moves the session
  (`papio browser use --latest`), the toolbar tooltip says another browser
  holds the session, the inbox says which browser has it, and the options
  footer reports the daemon as reachable. The refusal is also treated as a
  settled answer rather than a slow one, so popup and inbox reads in a waiting
  browser fail immediately instead of after the five-second hello wait.
  A browser demoted by `papio browser use` reports the same state as soon as
  the daemon tells it, and keeps every capability it negotiated — holdership
  gates offers and handoffs, not page acquisition or inbox reads.
- **The popup no longer flashes a status nobody had measured.** Both the popup
  and the inbox shipped a default *Can't tell — live progress is unavailable*
  line in their markup, painted before the first read returned, so every popup
  opened with a flash of it. Both now start empty and hidden, and only the
  renderer reveals them.
- **Opening the inbox no longer flashes "Disconnected".** Connectivity started
  as `false`, which is "not asked yet" rather than a verdict, and the first
  paint reported it as lost connectivity — complete with an automatic-reconnect
  promise and a `papio status` suggestion — before anything had been asked of
  the daemon. Connectivity is now a third state until the first read settles:
  the banner stays silent, and a daemon that really is slow gets a neutral
  *Connecting to daemon…* after a short grace instead of an accusation.
- **Plain words for an unknown or scheduled status.** *Can't tell* never said
  whether papio had looked and found nothing or had not looked yet; every
  unknown state now names its cause (*Progress unknown — the papio daemon
  isn't answering*). A scheduled instant names the day whenever it is not
  today, so a retry thirteen hours out reads *tomorrow at 08:00* instead of a
  bare *at 08:00* that looked imminent. The next action counts papers, and
  *Acquisition effects 0/1 busy* — an internal mechanism, reporting a
  configured limit as though it were news — is gone: the line appears only
  when something is actually held or queued behind the limit.
- **A fresh line no longer says "just now".** The institution-session card and
  live job status spent their remaining width restating that nothing had aged
  yet; the age appears once there is one.
- **A daemon that has begun advertising more than 32 `hello_ack` features no
  longer locks this extension out of the whole native session.** The parser's
  accept-side bound on `hello_ack.features` is now 64, up from the 32 every
  daemon still emits (`internal/browser/bridge.go`'s fail-closed feature list
  is unchanged). This is stage 1 of a two-stage mixed-version migration: an
  extension that tolerates a longer list, so a daemon's emitted cap can be
  raised later without a manually-upgraded daemon failing the whole
  `hello`/`hello_ack` handshake against an as-yet-unupdated extension. An
  older daemon's short feature list still negotiates exactly as before.

### Fixed
- ***papio* now closes its own inactive working tab when the daemon retires the
  handoff — even after either side restarts.** Provider pages used to outlive
  cancelled and finished papers forever because a navigated materialization
  had no authorized closing reason, and both the daemon's cancel index and the
  extension's job lookup were worker-local. Cleanup now acts from either live
  job state or a modern same-browser-epoch birth record, asks the daemon for a
  one-use, binding-scoped `job_inactive` authorization, and closes only an
  inactive tab still inside *papio*'s work window or tab group. After removal,
  its durable claim identity reports `owner_closed` so the daemon can consume
  the token and free the institution's sign-in slot. Pinned tabs, PDFs, tabs
  you moved elsewhere, tabs you took over, and browser-restarted tabs remain
  yours.
- ***papio* no longer mistakes its own focus for you taking a tab over.**
  Opening a paper brings its tab to the front — and *papio* read that exact
  activation as "the human is using this now", permanently, so every tab it
  opened became one it could never tidy up again. That is why finished papers
  piled up in the group even after cleanup shipped. *papio* now marks its own
  focus before it happens, so only an activation it did not cause counts as
  yours; it never closes the tab you are looking at, and when you move to
  another tab, the working tab you left behind retires itself if its paper is
  over.
- **Chrome's network-error page no longer stays in the *papio* group.** Once
  the daemon acknowledges that exact navigation failure, the extension closes
  the abandoned surface through the same one-use transaction. It does not
  retry automatically while the network may still be unavailable.
- **Asking for a paper's sign-in no longer opens two tabs for it.** *papio* was
  racing itself: the daemon answers a request to open a paper by both starting
  the paper's own working page *and* telling the extension to bring that paper
  to the front — and the extension answered the second half by minting a
  *separate* sign-in tab of its own. You got two *papio* tabs for one paper,
  one of them stranded on an internal placeholder page while your library
  sign-in loaded in the other, and papio watching the wrong one. It now waits
  for the single page it is already building.
- **papio no longer loses track of which tab your sign-in is on.** The daemon
  re-offers a paper's institutional work whenever it reconnects — after a
  restart, say — and *papio* treated that as "no tab yet", even when your
  library login was already open in front of you. The paper let go of its own
  tab: your sign-in was left stranded, nothing you did on that page was
  noticed, and the next attempt opened a *second* tab for the same paper
  instead of using the one you were looking at. A re-offer now refreshes the
  paper's claim without disowning its tab.
- **A leftover *papio* placeholder is cleaned up instead of becoming the
  paper's tab.** Every institutional paper starts on a blank *papio* page that
  is then sent to your library. If a second blank page was ever left behind,
  the next tidy-up pass mistook it for the paper's real tab — because the real
  one had, correctly, moved on to your library's sign-in — and told the daemon
  that the empty placeholder was where the work was happening. The paper then
  waited forever on a page nobody was looking at, while the sign-in tab you
  were actually using went unwatched. Leftover placeholders are now retired,
  and the tab holding your sign-in stays the paper's tab.
- **papio no longer cancels a paper by tidying up after itself.** When *papio*
  closed one of its own working tabs — a duplicate it had just replaced, or a
  placeholder it was finished with — it then told the daemon the tab had
  closed, and the daemon read that the only way it could: as you giving up on
  the paper. The paper was cancelled, seconds after you asked for it, with
  nothing on screen to explain why. Two papers were lost this way while
  testing on a real library. *papio*'s own closes are now marked as its own, so
  they retire the tab and keep the paper; closing a *papio* tab **yourself**
  still cancels the paper, exactly as before.
- **Closing an abandoned sign-in tab frees the other papers immediately, even
  hours later.** Giving up on a library sign-in released that institution's
  single sign-in slot only while *papio*'s background worker happened to still
  be awake — and the browser shuts that worker down after about thirty seconds
  of quiet. Close the tab after that, and nothing was reported: every other
  paper waiting on the same sign-in stayed silently parked for up to half an
  hour, and pressing **Open** on one appeared to do nothing at all. Each tab
  *papio* opens now carries the sign-in it belongs to in its own durable
  record, so closing it reports the abandonment whether or not the worker was
  still running.
- **The placeholder page *papio* opens for institutional work now loads.** It
  was pointed at a file that shipped one directory away, so every tab *papio*
  opened for a paper behind your library rendered the browser's own *"Your
  file couldn't be accessed"* error instead of the placeholder — visible in
  the *papio* tab group, and alarming for something that is meant to be
  invisible plumbing. The page's location is now derived from the extension's
  own manifest, the way every other *papio* page already was.
- **A tab you are looking at is never pulled back under papio's control.**
  An independent review of the ownership work above found the one remaining
  path that could: rediscovering an existing placeholder page could flip it
  back to inactive and re-adopt it, and could close a duplicate it had never
  proven it owned. Rediscovery now permanently cedes any tab you have
  activated or pinned, adopts only a tab whose birth record proves it is
  papio's own, and retires a proven duplicate through the daemon-authorized
  close — an unproven one is simply left alone. The close primitive itself
  now also notices a touch that lands *during* the close — activate a tab in
  the same instant papio decides to retire it and the tab stays, ceded to
  you, even if it looks untouched again by the time the close would land.
- **A close that was authorized just before the extension restarted now
  finishes instead of stranding the tab forever.** The restart replay ran
  before the daemon handshake had supplied the authority it needs, so it
  silently did nothing — every restart repeated the same dead race. The
  daemon now tells a reconnecting extension its live holder generation in
  the handshake itself, and that acknowledgement re-runs the pending close.
- **A second sign-in gate for the same paper reports its progress again.**
  Login/wall progress events were de-duplicated per paper rather than per
  gate, so a paper whose first sign-in ended without success could never
  report the second gate — its siblings stayed parked. And a page that
  failed to *load* is no longer mistaken for a login wall after an
  extension restart: the navigation-error marker now survives the worker.
- **Send PDF works again.** It was refused in every browser, every time, with
  *Reload the papio extension to finish updating* — advice that could not work,
  because reloading never changed the thing being complained about. The
  extension was not telling the daemon it could grab PDFs at all, so the daemon
  turned down a capability the extension had had all along.

- **Pressing Send PDF a second time no longer says "PDF sent to papio" while
  doing nothing.** *papio* keeps one request per paper, so a second press was
  answered "you already have one of those" — and that was reported as a
  success, for a paper nothing was fetching. Worse, a paper left in that state
  could never be sent again: every later press repeated the same false
  reassurance. The second press now resumes the request against the page you
  are looking at and asks for the one click that can finish it. If the earlier
  attempt belongs to a browser session that is gone, *papio* clears it and starts
  a fresh one; if it cannot be cleared safely — because something may still be
  arriving — you are told that instead of being told it worked.

- **A paper served on an expiring link now reaches *papio* instead of stalling.**
  Publishers such as Silverchair — JAMA and Oxford University Press among them
  — hand your browser a single-use link. *papio* was asking for the file a
  second time, and being told *"Your session has timed out"*, so **Send PDF**
  fetched an error page and then went quiet. It now recognises a link that can
  only be used once and, rather than trying, asks you to press the PDF viewer's
  own **Download** button — the file is already in your browser, so nothing is
  fetched twice. *papio* takes that file and reads the paper's identity out of
  it; when the document does not name itself anywhere *papio* can see, it waits
  in your inbox to be identified rather than guessing.

  On Firefox this is declined outright rather than half-promised. Firefox gives
  an extension no way to adopt a download it did not start itself, so *papio*
  now says to open that paper in Chrome instead of asking for a **Download**
  click it could never act on.

- **The viewer's Download button files the paper you are reading, not the one
  the tab was opened for.** A tab *papio* opened for one paper and you then
  reused for another was steering downloads into the first paper's folder while
  the request for the second went unanswered. The paper you named when you
  clicked **Send PDF** now wins over what the tab used to hold. Nothing was
  misfiled by this — the file was checked against the citation and set aside for
  review — but it cost you a review that had no reason to exist.

- **A download that fails instantly is reported instead of leaving *papio*
  waiting.** When a link failed the moment it was tried, the failure arrived
  before *papio* had finished learning which download it belonged to, so it was
  dropped: the request stayed open, and the one download *papio* permits itself
  at a time stayed spoken for, so the next **Send PDF** answered *busy* until
  the browser restarted the extension on its own schedule. The failure is now
  noticed straight away and you are told the link did not work.

- **A paper is no longer left unfiled because its PDF's link confused *papio*.**
  On ACM and Springer, and on any link that carried a DOI beside other
  parameters, *papio* could read the DOI wrong and then look for a paper that
  does not exist — so **Send PDF** appeared to fail on papers it should have
  handled without a thought. It now keeps the identifier separate from the rest
  of a link, so what it looks up is the paper and nothing else in the address.

  Two consequences worth knowing. Where a link carries a DOI alongside a private
  access token, that token is no longer mistaken for part of the paper's
  identity — it used to be stored as such. And where a link points at something
  that is *not* the paper — a supplementary file, a cited-by list, a preprint's
  full-text view — *papio* now leaves it for you instead of filing it under the
  article, because a supplement filed as the paper it accompanies is the one
  mistake you would never notice.

- **Private link details no longer leave your browser when you send a paper.**
  Publisher and library links routinely carry signed tokens and interlibrary-loan
  tickets, which work like passwords. *papio* now shares only the publisher's
  site, never the rest of the link — on every path that files a paper from a
  page, not just the one this started with. A page title that is really an
  address is dropped too, rather than kept as the paper's title.

- **A pick is now checked against the page the browser reports, not the address the popup sent.** The acceptance check rebuilt the "live" page identity from the URL the popup had passed in, so it only ever proved the popup sent the same string twice — and on provider viewer routes the two sides computed that value by different rules, so the check could never succeed at all. Both sides now derive it the same way from the live tab. Ordinary page loads and reloads also count as page changes now (previously only in-page navigation did, so reloading a tab left an earlier pick valid), a pick expires once it is old rather than only when a newer one displaces it, and an epoch known on one side but not the other fails closed instead of falling back to the weaker comparison.
- **A pick no longer outlives what it was granted for.** Finishing or removing a delivery revokes any outstanding offer that listed that paper, so a second PDF sent from the same page cannot be filed under a choice made for the first. Replacing a tab — a prerendered page activating, for example — clears the *Download* continuation bound to the old tab instead of leaving it stranded so every later pick returned the same stale state. A *sending* delivery interrupted by a service-worker restart is now reconciled at startup rather than leaving a paper you cannot retry.
- **Only the address without its query is remembered.** A signed provider download link is a credential; the stored comparison value is now the origin and path only, so no signed link is written to extension storage.
- **The picker no longer promises Firefox behaviour that cannot happen.** Firefox has no way for *papio* to follow a viewer's own *Download* button, so offering the picker for a native-viewer PDF left the delivery waiting forever. Firefox now says to use the viewer's Download button and explains that *papio* files that download.
- **A pick made against a restarted worker re-offers the list instead of just clearing.** The popup re-requests the choice once and shows the fresh list, rather than resetting the button and making you click **Send PDF** again — which, given how aggressively the worker is stopped, was the common path rather than a rare one.
- **Send PDF is decided before you press it, and never refuses because another browser is connected.** The button used to render enabled whenever a delivery was not already in flight, so a refusal *papio* could have predicted arrived only after the click — including one that named internal machinery and was not warranted in the first place, since saving a PDF you are looking at has nothing to do with which browser receives the papers *papio* finds. The button now reports what is actually known: it stays enabled when nothing is known to be missing, disables while a send is in progress and says so as progress rather than refusal, and disables with the remedy on it — and on its accessible label — when *papio* genuinely cannot save PDFs. Enter is stopped by the same state as the click. Every refusal *papio* can send now has its own plain sentence naming what to do; a refusal from an older *papio* falls back to its own wording unless that wording is internal, in which case a plain sentence is shown instead. No message this browser can produce says "holder", "permit" or "negotiated".
- **A browser that is waiting for its turn is no longer a dead end.** While another browser held the connection, this one received no acknowledgement at all, so it believed *papio* supported nothing and refused the inbox, page acquisition and Send PDF locally without ever asking. It is now acknowledged as waiting: everything you start here works, while the offers and handoffs *papio* initiates continue to go to the browser that holds the slot, and page capture — which only that browser can do — is declined here on purpose. The popup no longer claims this browser "gets no papers until you switch it", which stopped being true.
- **A refused cancellation is no longer reported as a cancellation.** Any outcome that was not an explicit conflict was treated as success, so a download *papio* declined to cancel was shown as cancelled.

## [0.14.0] - 2026-08-14

### Added
- **Provider effects reconcile safely after a service-worker restart.** The
  extension negotiates `effect_permit_v1`, persists only the daemon-issued
  permit identity after authorization, and reports exact bounded observations
  for generic/direct downloads, PDF grabs, configured terms acceptance, and
  institutional navigation. Missing or ambiguous completion remains occupied
  in the daemon instead of authorizing a second click or download; old daemons
  return `unsupported` and no protected effect starts.

- **The extension surfaces honest, recoverable progress.** The inbox now uses
  action-first information architecture with one local feedback strip, and
  hoists repeated guidance only from daemon-authored family run keys. The
  toolbar badge counts effective required turns by default, with
  **Decisions waiting**, **Everything pending**, and **No number** options.
  Activity is pageable with a browser-local read watermark, while the popup
  shows the negotiated pulse vocabulary and current-page lens. Focused popup
  and inbox presence is sent through the privacy-minimal
  `surface_presence_v1` hint. Page-bulk acquisition accepts 1–200 canonical
  keys as durable cohorts, chunks them under the wire bound, and replays
  unresolved chunks safely across worker suspension and browser restart.

- **Firefox asks once before page captures leave the browser.** The Firefox
  build now declares website-content data collection (Firefox 140+ shows the
  built-in consent at install); on Firefox 128–139 sanitized failure captures
  are not transmitted to the local *papio* app until a one-time settings
  checkbox is enabled. Chrome behaviour is unchanged.
- **Unrecognized provider pages get a generic, identity-proven attempt.**
  When no adapter recognizes a page during a delegated handoff, the
  extension now reads the page's citation metadata and — only when the
  page's DOI exactly matches the requested work — tries at most two
  strictly-sequential candidates (the declared `citation_pdf_url`, then a
  unique article PDF link) before parking. Assisted jobs record the evidence
  without downloading anything, ambiguous pages stay hands-off, and every
  attempt is capped per drive even across extension restarts.

### Changed

- **Assisted jobs never act on their own.** When a job's negotiated access
  mode is `assisted`, the extension no longer clicks provider download
  controls, starts direct-file downloads, or navigates on the job's behalf —
  it records what a click would open and leaves the action to you, while
  anything you download or open yourself is still adopted.
- **The requested paper is checked before an automatic download.** When the
  provider page exposes a DOI in its citation metadata and it does not match
  the job's DOI, the automatic download is refused and the mismatch is
  reported as the wrong work instead of silently filing the wrong PDF.
- **The impact summary no longer estimates time saved.** The popup footer and
  the history page dropped the "Est. time saved" figure and its five-minutes-
  per-paper assumption. *papio* counts what it did — papers acquired and the
  share of finished acquisitions that succeeded — and no longer multiplies a
  count by an invented per-paper duration.

### Fixed

- **Popup refreshes no longer starve on a slow daemon read.** The periodic tick
  now waits for an in-flight refresh instead of repeatedly superseding it, so
  disconnected and slow-start states eventually render. Action labels wrap
  rather than clipping in the narrow popup.
- **Scanner consent and revocation stay accurate under concurrency.** The
  consent prompt now states that detected identifiers go to the local *papio*
  app, while short citation labels stay in the browser, matching the actual
  ownership-check wire payload. Revoking one allowed origin disables only that
  row while other origins remain usable.
  The catch-up setting's label and explanation now stack instead of running
  together on one line.
- **Manual-download actions use an unambiguous label.** The inbox now says
  **Open link** instead of **Open source**, avoiding both open-source and
  open-access readings while keeping **Open tab** reserved for focusing an
  existing *papio* handoff tab.
- **Repeated handoffs no longer stack duplicate tabs.** After a service-worker
  restart the extension recovers the job's existing broker tab from its
  durable ledger instead of opening another copy for every re-offer.
- **Manual PDF delivery stays attached to the row you opened.** Opening a
  manual-download item now pins that existing job through later daemon
  re-offers, long login flows, and MV3 restarts. Re-offers merge adapter and
  host fields onto that pin instead of replacing it. **Send PDF** joins by the
  current tab, then a unique page DOI, and only then the Open pin when it owns
  this tab — a DOI-less PDF in another tab is identified from the file rather
  than bound to whichever row was opened last. **Send PDF** works directly on
  recognized SAGE, Taylor & Francis, and Cell journal PDF viewers. Selecting
  another Open row now also revokes the old row's tab authority, so a native
  download cannot be adopted into that stale job and rejected as the wrong
  paper. On Chrome, a provider-started download from the selected landing host
  is steered into that job automatically instead of landing loose in Downloads.
  ScienceDirect asset viewers are deliberately not re-fetched — that endpoint
  can return `init.html` outside the viewer navigation. **Send PDF** now arms
  the current viewer for the job and asks for the viewer's own Download button;
  the popup keeps showing that instruction, then **papio adopted PDF
  (validating)** after the daemon acknowledges adoption. Viewer tabs remain
  open intentionally so papio never closes user-visible reading context.
- **On-screen PDFs on publisher CDNs are grabbed again.** A PDF viewer tab
  landing on a host outside the provider's own domain (e.g. a ScienceDirect
  asset CDN) is now adopted when exactly one driven job matches it.
- **Send PDF without a page DOI identifies the file.** On Chrome, a DOI-less
  PDF now uses download steering to identify the file itself instead of
  claiming the job has no DOI. Opening a manual-download row with an adapter
  resumes driving that tab, and closing that tab keeps the pinned job awaiting
  download.
- **Institution-session status stays attached to the right institution.**
## [0.12.0] - 2026-08-10

### Added

- **Institutional sign-in now starts only when you click Open.** A cold
  `handoff_link_v1` offer stays tabless in the inbox and popup; that explicit
  click reserves the institution's one shared login claim, asks the daemon for
  a fresh route, and creates exactly one managed tab. Concurrent papers focus
  or wait behind the same sign-in instead of opening duplicate IdP tabs.
  One-use routes are discarded at tab materialization and never enter browser
  storage or the managed-tab ledger. Worker restarts recover the live tab,
  claim, and completed login-wall landing without persisting the entity ID.

- **Inbox: "downloads access required" items.** When the daemon cannot read
  the Downloads adoption folder (macOS privacy consent), the inbox shows a
  required-attention item naming the blocked path and the System Settings
  grant; it clears itself once access is restored and the download is
  adopted. Dismissing the item never cancels the job.
- **Workspace: a green ✓ marks papers already in your library**
  (`owned_with_pdf` rows), alongside the existing eligibility states.
- **"Select papers on this page" — bulk selection from a reference list,
  bibliography, or results page (ADR-0019).** The popup gains a scan action
  beside the existing single-paper Acquire button. Clicking it runs one
  top-frame `scripting.executeScript` under the ordinary, temporary
  `activeTab` grant — there is no persistent scanner, no dynamic
  content-script registration, no `MutationObserver`, and no standing
  all-sites grant; a page whose list has since changed is rescanned with an
  explicit **Rescan** control, not a background watcher. Detection runs
  entirely inside that tab and is local-only: recognized links (`doi.org`,
  publisher `/doi/10.x` paths, arXiv, PubMed) and explicitly labeled text (a
  strict DOI, `arXiv:<id>`, or `PMID: <digits>` — an unlabeled bare integer
  is never treated as a PMID), each paired with a short citation label from
  its nearest bounded container, capped at 200 raw candidates with
  truncation always reported, never silent. Results open a new full-tab
  selection workspace, one per scan (`?scan=<id>`), showing the source
  page's title, origin, and scan time entirely from the local snapshot.
  Rows start **unselected**: `owned_with_pdf` and already-queued rows are
  disabled, and every other state — including an incomplete lookup or a
  previously-unavailable mark — stays checkable, because an incomplete or
  failed check is `unknown`, never a negative ownership fact. The primary
  action reads "Acquire all *N* eligible" until a row is checked, then
  morphs to "Acquire *N* selected"; submission caps at 50 per batch (the
  existing batch limit, distinct from the 200-item detection cap), and rows
  past the cap are not auto-chained — they stay selected or unselected for
  the next submit. The workspace marks itself expired if the background's
  bounded scan store has since dropped the snapshot. Nothing is sent
  anywhere until you click "Select papers on this page": the daemon lookup
  that follows sends only the detected identifiers, never the page's title,
  URL, or surrounding text, and the submitted batch's manifest records only
  the page's bare scheme+host origin, never its path, query, or title. No
  scholarly service is contacted until you submit selected papers. Requires
  the daemon's `page_bulk_acquire_v1` feature, negotiated over
  `hello_ack.features`; an older daemon that never advertises it fails the
  scan with a plain error instead of silently doing nothing.
- **Page-bulk rows now show what your Zotero library already has.** Each
  row's status merges the daemon's own ready-bundle and queue state with a
  zotio library lookup: `owned_with_pdf` and `owned_missing_pdf` marks
  (with the owning item key, for missing-pdf rows) come from your personal
  library, not a guess. When the library lookup is stale, fails, or zotio
  isn't configured to answer, the row reports the new `ownership_unknown`
  status — labeled **Library check unavailable** — never a false "not
  owned". `owned_with_pdf` rows stay disabled and unselected exactly as
  before; an `ownership_unknown` row stays checkable, the same as any other
  unresolved state.
- **Selection workspace scans report an honest "how many records were on
  the page" count.** For a page whose result-list shape the scanner
  recognizes structurally — definition-list rows, a reference/citation
  list, or a repeated card grouping, each needing at least two matching
  entries to count as a list — the scan attaches a
  `rendered_record_count_hint`: a single integer count of visible result
  records, read from element structure only. No title, URL, query string,
  or document id is ever inspected to produce it, and it never leaves this
  page: it rides only the `page_bulk_status` call to feed the daemon's own
  aggregate `papio stats page-bulk` reporting, and stays absent, never a
  guess, when no recognized page shape is found.
- **The inbox renders document-delivery reconciliation.** A parked
  ILL/document-delivery request now appears as its own `document_delivery`
  item, always showing its provider, provider reference, and current
  state, with attention-driven styling — a quiet, muted treatment for
  `working` items papio is still handling on its own, an emphasized accent
  for `required` items waiting on you, and a subdued informational accent
  for `advisory` items. Two new mutating operations, **Confirm exists** and
  **Confirm absent**, drive Decision 4's reconciliation for a request stuck
  in `unknown_outcome`; a third, **History**, opens the request's own
  history without mutating anything. A stuck request's card also explains
  itself: "papio paused automatic polling until you confirm what the
  library has on file for this request."

- **One sign-in tab per institution.** Three papers that all need the same
  institutional sign-in used to each open their own IdP login tab — logging
  in on one never resumed the other two, which sat on a dead resolver page
  until the operator noticed and drove them by hand. A handoff whose login
  wall would open a *second* tab at an IdP already being signed in on now
  parks instead, quietly, at the provider page it was already on. Completing
  sign-in in the one open tab resumes every paper waiting on that
  institution automatically, through the same two-at-a-time drive governor
  as any other handoff.
- **Open PDF tabs can now be grabbed from the selection workspace.** When the
  tab URL has no recognizable identifier, Chrome offers a one-click grab row;
  Firefox shows the row disabled with honest download-steering guidance, and
  grab outcomes report job creation, existing library ownership, or the need
  for an identifier directly in the row.
- **Inbox renders recoverable PDF grabs.** A captured PDF without an
  identifier now appears as a required-attention `pdf_grab` row with its
  state, identifier guidance, and a dismiss action that removes only the grab.

### Changed

- **Waiting sibling handoffs are now one actionable sign-in.** The browser
  stores only opaque claim digests, drops pre-r6 raw claim keys on restart,
  and renders sibling papers as working while the owner's institution sign-in
  remains open in another tab. The waiting overlay expires back to the
  daemon's required rendering if the owner does not finish in time.

- **OpenAlex works-search pages now detect every result on the page.**
  `openalex.org` result cards carry no `doi.org` anchor at all — the title
  itself links to `openalex.org/works/w<digits>` — so the bulk-selection
  scanner previously caught only the rare row with a stray publisher DOI
  link. It now recognizes the OpenAlex work-id link directly (`openalex`
  kind, normalized to uppercase `W<digits>`), so a page of 25 works detects
  all 25. A card that also carries a registered DOI/arXiv/PMID link still
  produces exactly one row, keyed on that registered identifier.

### Fixed

- **The Institution session card no longer invents "unknown" two minutes
  after a verified sign-in.** Inconclusive probe outcomes (`no_tab`,
  `no_markers`, and the rest) still resolve to their own honest labels, but
  an aged decisive verdict now retains what papio actually verified:
  **Last verified signed in — rechecking** or **Last verified signed out —
  rechecking**. The popup schedules one bounded probe for that verdict
  timestamp and deduplicates it, rather than converting durable signed-in
  evidence to unknown or injecting another probe on every five-second
  refresh.

- **A dropped daemon connection no longer leaves the browser talking to
  itself.** Every asynchronous message handler now always answers. When the
  native port died mid-request, the handler simply stopped, and Chrome filled
  the silence with its own "message channel closed before a response was
  received" — which is what a selection workspace showed after the daemon
  disconnected, above ten rows frozen on "Checking availability…". Failures
  now come back as papio's own sentence, including for runtime errors the
  extension does not recognise.
- **Heartbeat triage counts negotiate the daemon's advertised schema again.**
  The extension had started sending triage-snapshot schema versions on the
  separate counts request, then rejected its own frame before native messaging
  could send it. Keepalive refreshes now request counts schema 2 only when the
  daemon advertises that capability, preserving badge and inbox refreshes
  across compatible daemon versions.
- **Entitled ScienceDirect pages no longer fall through to `ui_changed` or
  download Cookie Notice HTML.** The live article layout exposes its PDF
  through the primary access-bar link, not the `citation_pdf_url` metadata the
  adapter expected. Papio now recognizes and activates that exact article
  control, then adopts the PDF viewer ScienceDirect opens, without confusing
  related-paper PDF links for the requested work.
- **PDF-grab status pulls reached no handler in a built extension.** The
  message type was missing from the dispatcher's accepted list, so a reopened
  grab workspace could never recover its state from the daemon. The list and
  the handlers behind it are now pinned equal by a test that fails if either
  side gains an entry the other lacks.
- **The selection workspace stops claiming "0 eligible" before it knows.**
  While availability is still unknown the button says so and stays disabled,
  and the workspace reloads by itself once the daemon reconnects instead of
  asking you to retry papio's own work.

## [0.10.0] - 2026-08-06

### Added

- **A requested page capture echoes its `request_id`.** The daemon can now tie
  the sanitized content frame to the specific `page_capture_request` it
  answers, instead of guessing from provider and scenario. The popup's own
  Capture button omits the field, and the background broker refuses to accept
  one from an extension page, so an unsolicited capture can never claim a
  request it did not answer. Requires papio 0.18.0 or newer: the extension's
  daemon floor moves to 0.18.0 with this change, and an older daemon rejects
  the frame outright — rebuild and reload the extension alongside the daemon.

### Fixed

- **Signing in to a second institution no longer makes the first report
  failure.** The popup renders one Sign-in button per institution and disables
  only the clicked one, so two clicks in quick succession reached the keepalive
  manager concurrently. Both callers ended up waiting on the same tab-creation
  promise, and the second cleared `tabID` before the first could read it — so
  the first returned failure for a managed tab it had genuinely created, and
  the background broker fell through to an unmanaged sign-in tab that startup
  orphan reconciliation can close mid-SAML. Sign-in requests are now serialised
  per manager: the first completes and reports honestly before the second
  supersedes it.
- **Taylor & Francis and Emerald articles you are entitled to no longer park as
  manual downloads.** The T&F rule required the *open-access* badge
  (`.access-icon.oa`), so an article reached through an institutional session —
  which renders `.access-icon.full` next to a working `/doi/pdf/` control — fell
  through to `unknown`, i.e. exactly the case papio exists to drive. The badge
  is now a disjunction over both entitlement shapes, still required so the rule
  never fires on a bare download-looking link. Emerald has meanwhile migrated
  article delivery off `a.intent_pdf_link` -> `/insight/content/doi/<doi>/full/pdf`
  onto `a.article-pdfLink` -> `/<journal>/article-pdf/…`; the migrated shape gets
  its own rule and the single download rule now resolves either anchor.
- **An unentitled ScienceDirect page reports no entitlement instead of a
  coverage gap.** A purchase wall published no `citation_pdf_url`, matched no
  rule, and surfaced as "papio could not drive the provider page" — sending you
  to hunt an adapter bug when the resolver had simply routed you somewhere you
  have no access. The access bar's purchase control is now a `no_entitlement`
  rule, ordered after the entitled-article rule so a positive signal always wins.
- **A provider adapter's declared settle budget is honoured.** `interpret`
  clamped `settleTimeoutMs` to 5000 ms while ClinicalKey declared 8000, so that
  adapter's extra budget was silently discarded and a provider whose content
  player is genuinely slower than five seconds when reached through a resolver
  hop kept classifying `unknown`. The ceiling is now 15000 ms and ClinicalKey
  asks for it. This is a worst case, not a delay: classification resolves the
  instant a declared selector appears.
- **A diagnostic page capture is no longer thrown away for the failure you most
  need to see.** The capture rate limiter was keyed on the bare host at one per
  hour and five per day, so one host failing the same way repeatedly consumed
  the budget every other provider needed, and the sanitized page required to
  repair an adapter was discarded in the overwhelming majority of coverage
  gaps. The limiter is now keyed on the failure shape (host plus adapter
  identity, so a version bump on a still-broken host is captured immediately),
  the daily ceiling is twenty, and a repeat of an already-captured page shape is
  detected by digest and costs neither an upload nor a budget slot.
- **Session evidence never invents an institution.** When the observed origin
  was unknown, `emitSessionEvidence` substituted one — the keepalive snapshot's
  resolver origin, which itself degrades to an arbitrary granted host, or the
  origin of the most recent institutional offer. The daemon treats a resolvable
  hint as authoritative, so a wrong-but-resolvable one released a second
  institution's parked handoffs without its session having been verified. That
  is reachable for any institution whose own resolver hostname contains `sso`,
  `idp`, `login`, `auth` or `shibboleth`, since the hint derivation fails closed
  on those and fell through to the substitute. The hint is now sent only when it
  is the origin actually observed for that evidence, and omitted otherwise —
  which is safe, because the daemon scopes an unattributable frame to the
  default profile rather than picking an institution by list order.
- **A delivery's access basis reflects the page that produced the bytes.** The
  session evidence behind `access_basis` was read when the download completed,
  from mutable global warm-auth state, so an institutional probe or sign-in
  finishing anywhere in the browser mid-download recorded a PDF fetched from a
  public page as institutionally acquired. Evidence is now frozen at request
  time beside the page host, and applied only to the delivery it was captured
  for. Freezing the host alone could not fix this: the host is not an input to
  the basis decision.
- **A resumed handoff is no longer stranded by its own parked marker.** The
  marker recording a deliberate park was cleared only when a drive actually
  registered, but with both governor slots busy — the normal steady state —
  every resume path defers to an in-memory queue that no restart survives. A
  worker teardown in that window left the job marked parked forever, so the
  restore loop skipped it and it dropped out of governor supervision
  entirely: no timeout, no capacity accounting, no recovery. The marker is
  now cleared at the point of intent to drive, which both the queueing and
  registering paths share.
- **Switching institutions mid-creation no longer leaks a pinned tab.** When
  a re-authentication for a second origin arrived while a tab creation for
  the first was still in flight, its own stale-tab teardown was skipped —
  that teardown is gated on a tab already existing, which is exactly false
  during the window — so the first creation completed into a pinned, muted
  tab nothing referenced again. The tab governor skips pinned tabs by design,
  so it was never reclaimed. The superseded tab is now torn down before the
  replacement is created.
- **A parked handoff no longer re-consumes its governor slot after a
  service-worker restart.** Parking for manual auth frees a slot but leaves
  the tab open, and the slot lives only in worker memory while the tab id is
  persisted — so after MV3's ~30s idle teardown a parked job looked exactly
  like one still mid-drive and was re-registered, re-arming its timeout. On a
  slow institutional sign-in that repeated on every restart, halving effective
  capacity for every other queued job. The park is now recorded on the job
  itself, at the single point that releases the slot, and cleared whenever the
  job is genuinely driven again. This also covers the auth-exhausted and
  challenge-blocked parks, which had the same shape before the timeout change
  made it visible.
- **A sign-in for one institution cannot be handed another institution's
  tab.** Concurrent keepalive callers share one in-flight tab creation, but the
  creation re-read the resolver *after* awaiting the tab query, so a caller
  switching origins mid-flight could redirect a creation already underway —
  producing a tab for the wrong origin, or surfacing one institution's session
  tab as another's explicitly requested re-authentication. The resolver is now
  snapshotted for the duration, and a caller wanting a different origin waits
  for the in-flight attempt rather than riding it.
- **A `terms` capture refused by the daemon gate reports the refusal.** It
  returned success, so the popup printed `Sent N-byte capture` for a frame that
  was never transmitted — in exactly the daemon-swap case the gate exists for,
  leaving the developer hunting a storage bug that did not exist. It now
  surfaces an error naming the cause.
- **Concurrent keepalive reconciliation creates one resolver tab, not two.**
  `reconcile`, `onObserve`, `onReload` and `openReauth` each tested whether a
  tab existed and then awaited creation, with nothing held across the await.
  Creation itself awaits a query before a create, so two callers could both
  find no tab and both make one; the later assignment orphaned the first. The
  tab governor deliberately skips pinned tabs, so that orphan was never
  reconciled and could hold a work window open indefinitely. Concurrent
  callers now share one in-flight creation, cleared on failure as well as
  success so a single rejected create cannot wedge the manager.
- **A handoff that times out mid-login keeps its page.** The three-minute
  drive timeout unconditionally closed the tab before parking the job, which
  contradicts `parkHandoffForManual`'s own contract — "leaves the exact page
  available to the operator" — and destroyed whatever the operator had in
  flight: a half-completed IdP form, entered credentials, a pending 2FA
  challenge. A timeout on a recognised authentication page now leaves the tab
  open and only releases the governor slot. A timeout anywhere else still
  closes the tab as before.
- **The handoff-drive cap is enforced where it is defined.** Callers checked
  `HANDOFF_DRIVE_LIMIT` themselves, but several do so before `await`s that
  open a tab or persist the job, so two entry points that are not both on the
  serialized inbound-frame chain could each pass the check and then register,
  running more concurrent drives than the governor allows. The registration
  itself now refuses at capacity and queues the job instead, so every call
  site is safe by construction and no job is dropped — the queued drive reuses
  the tab its caller already opened.
- **The developer `terms` capture scenario can no longer disconnect the
  session.** `terms` was appended to the *existing* `page_capture` scenario
  enum, and the capture panel sent it gated only on `page_capture_v1` — a
  capability older daemons already advertise. Selecting it against one of them
  made the daemon reject the frame, and a browser-protocol decode failure is
  fatal to the whole native-messaging session, not just that capture. The
  option is now withheld unless the daemon advertises `page_capture_terms_v1`,
  and the gate fails closed: an unknown or not-yet-received feature list
  withholds it too, so the pre-hello window behaves like an old daemon rather
  than optimistically offering the option. The gate is enforced twice: the
  panel withholds the option, and the runtime boundary that actually emits the
  frame refuses a `terms` payload independently, so a daemon swapped out
  underneath the popup between refreshes cannot be sent a scenario it will
  reject. Release builds were never exposed — the panel requires a developer
  build and an unpacked manifest.
- **The developer capture panel no longer multiplies its own click handler.**
  Reading the daemon feature list for the gate above moved panel wiring into
  the popup's five-second refresh, which had been running once. Each tick
  attached another `click` listener, so after a few minutes one click fired a
  dozen concurrent captures — each a sanitized page frame up to the 256 KiB
  protocol cap, each its own relay request — and every tick also reset the
  operator's chosen provider and scenario to the first option. Wiring is now
  one-shot and the periodic pass only refreshes the options, preserving the
  current selection and falling back safely when the selected scenario is
  gated away.
- **Handoffs queued behind the drive governor survive a service-worker
  restart.** With both drive slots busy an accepted job waits at `tab_id -1`
  in an in-memory FIFO. MV3 suspends the worker on idle and nothing restored
  that queue: the startup scan skipped every job at `tab_id -1`, the
  queued-release pass only handles jobs in the `queued` state, and a daemon
  re-offer for the same URL merely re-acknowledged it. The job never opened,
  never completed and never timed out — silently lost, and most likely under
  exactly the flood the governor exists to manage. Startup now re-enqueues
  those drives; deliveries and direct-file downloads, which legitimately park
  at `tab_id -1`, are left alone.
- **A per-institution "Sign in" hands its tab to the keepalive manager.** The
  popup's origin-specific sign-in rows opened a plain managed tab, so the
  manager never knew a sign-in was in progress: its own reload cycle could
  fire mid-SAML exchange and destroy the login, and because the tab was
  ledgered but carried no job, startup orphan reconciliation could close it
  while the operator was still signing in. It now takes the same
  manager-owned path as the general re-authentication action, falling back to
  a managed tab only when there is no manager.
- **Delivery provenance records the page that requested the download.** The
  host reported on `delivery_context` was read from the tab at
  download-completion time rather than when the delivery was requested. A
  download takes seconds and the tab stays interactive throughout, so
  navigating away mid-download recorded the page the operator happened to land
  on as the candidate's landing page, naming somewhere the bytes never came
  from. The host is now frozen alongside the URL at request time, and is
  applied only to the delivery it was captured for — a failed delivery no
  longer lends its host to the next download of the same job. This corrects
  the recorded landing page only: a delivery's `access_basis` is still derived
  from its route and session evidence, both still evaluated when the download
  lands.

## [0.9.0] - 2026-08-04

### Added

- **ClinicalKey full-text pages are autonomous.** The `clinicalkey` 0.1.0
  adapter classifies an entitled SPA article on its stable
  `pdf-download-link` anchor and downloads the provider's watermarked PDF
  endpoint with the operator's session cookies. The `.com.au` front is
  fixture-backed; other ClinicalKey fronts remain assisted until captured.
- **MDPI article pages are autonomous.** The `mdpi` 0.1.0 adapter uses the
  live page's Highwire PDF metadata plus its provider-owned download anchor,
  then downloads the exact PDF URL with the operator's browser session.
- **Coverage gaps explain the next step.** A blocked provider permission now
  offers a one-click **Allow** action in the popup and resumes the exact
  governed tab after the grant. A stable resolver landing outside the capped
  provider list gets one bounded render window, attempts a sanitized local
  diagnostic capture, then becomes a **No adapter yet** manual-download action
  instead of spinning indefinitely.
- **Adapters for two more entitled routes.** Informit (`informit` 0.1.0):
  the SAML terms wall classifies as `terms` with an explicit consent control
  and the Atypon article page downloads via its captured PDF control. JSTOR
  (`jstor` 0.3.0): article *record* pages wire the primary Download control to
  a popup (`window.open` with `acceptTC=1`) that Chrome blocks for
  gesture-less adapter clicks, so the download now derives the direct
  `stable/pdf/<id>.pdf?acceptTC=1` endpoint from the tab URL — consent-gated,
  because `acceptTC` accepts JSTOR's terms on your behalf.
- **The daemon can request a page capture.** Under the
  `page_capture_request_v1` feature, a solicited `page_capture_request`
  directive (riding the ordinary sync cycle) opens a governed, ledger-tracked
  tab, waits for load plus a bounded settle, runs the existing sanitized
  capture pipeline, reports a structured outcome, and closes the tab. The
  page is never interacted with.
- **Acquire is a labelled header button.** "Acquire" / "Send PDF" render as a
  primary text pill beside the inbox button (an icon proved too subtle); the
  full DOI detail stays on hover. Inbox tab labels now carry the daemon's
  totals instead of the loaded page size, operation feedback renders only on
  the affected row (with a screen-reader announcer), pagination controls are
  panel-scoped, and watch hits load themselves when the daemon count says
  they exist — no Load more click required.
- **One session row per institution.** The popup's institution card renders a
  row for every config-derived resolver origin advertised in the daemon hello —
  never for a provider offer or a host-permission grant. Each row has its own
  evidence-based verdict, freshness, and a Sign in button that opens *that*
  institution, and keep-warm tracks each origin separately. Warm evidence
  releases only that origin's queued extension handoffs and daemon-side sibling
  re-offers; another institution's queue remains parked.
  A warm, freshly verified session earns zero pixels: rows render only when
  the operator can act or the evidence has gone stale, so a quiet popup
  means everything is live.
- **Session verdicts got stricter — and evidence is never erased.** An
  auth-shaped URL alone never reads as signed out (inspection evidence is
  required); asserting "signed out" requires a *visible* sign-in affordance,
  because Ex Libris platforms keep one buried in a closed drawer permanently
  (sign-out affordances still count from inside closed menus, which is where
  signed-in pages keep them); expired identity tokens no longer count as
  signed in; and markers are read only from real controls, links, and forms —
  never from page scripts, styles, or aggregate text. Earned verdicts stand
  until new evidence replaces them: closing a library tab, opening a fresh
  probe tab, or a service-worker nap no longer resets a warm session to
  "unknown" (per-origin state now survives worker restarts). And because a
  tab loaded before sign-in keeps rendering signed-out while the browser's
  cookies are signed in, the probe now checks sibling library tabs too — any
  tab evidencing a live session outranks one stale render.
- **papio owns its surfaces — leftover tabs clean themselves up.** Broker tabs
  papio creates are recorded in a durable ledger that survives extension
  reloads. Shortly after startup, ledger-owned leftovers still sitting in the
  papio tab group or work window are closed automatically; the popup's
  "Leftover papio tabs · Close them" card appears only for ambiguous strays
  outside those surfaces. Only ledger-owned tabs are ever closed: a tab papio
  merely reused, a tab the user is viewing, a pinned keepalive tab, a tab an
  active job tracks, or a stranger tab that merely sits in a papio-titled
  group is never touched.
- **The popup can send the current PDF to *papio*.** It classifies the active tab
  as a PDF, DOI page, or neither; for a PDF it queues or joins the matching
  job, starts a browser-managed download under `papio/<job-id>/`, and reports
  sending, adoption, validation, or a recoverable failure. DOI detection is
  layered — citation/Dublin-Core/PRISM meta tags, canonical and `doi.org`
  links, a bounded page-text scan, and a JSTOR stable-page fallback
  (`10.2307/<id>`). Provider click downloads remain human-assisted on
  Firefox, whose WebExtensions API has no filename-routing hook.
- **Institution sessions are visible at a glance — and the verdict is
  evidence-based.** The popup shows the resolver session as warm, signed out,
  unclear, or keep-warm disabled, always naming its evidence source and time
  ("via your open library tab · 12:38 pm"). The verdict comes from inspecting
  a real resolver tab — the focused tab first — for sign-out affordances
  (including inside closed menus, `href`/`formaction` targets, and ARIA
  labels), with a web-storage JWT identity fallback for Ex Libris-style
  platforms (a bare `sub` claim never counts: anonymous tokens carry one).
  papio never asserts "signed out" without a completed inspection saying so,
  an inspection failure is reported distinctly from a clean miss, and an
  evidence-free probe never latches. A warm session offers no sign-in button,
  and freshness reads as relative age ("just now", "3m ago") rather than a
  wall-clock time; "Sign-in unblocked N items" announces once per release
  event. Options adds
  extension-local keep-warm enable/disable and refresh-interval controls;
  credentials and identity-provider hosts never enter the daemon protocol.
- **Pages with an acquisition already in flight show live progress, not a dead
  button.** The popup renders the job's latest activity with its age, an
  honest "No progress for <time>" prefix when stalled, waiting-on-you wording
  for parked jobs, and Open-inbox-item / Go-to-tab actions. A verified-warm
  session outranks a stale login-required echo, and Go-to-tab hides when you
  are already on the job's tab — a download from that tab is adopted
  automatically.
- **The inbox separates concerns into Actions, Watch hits, and Activity tabs**,
  with actionable items first and keyboard-accessible tab navigation. The
  Activity tab groups a bounded timeline by job, collapses repeats, and
  truncates behind **Show more**; it appears only when the daemon advertises
  `activity_feed_v1`, and older daemons get an explanation rather than an
  implied live push.
- **Every job and resolver tab papio opens lands in the papio tab group** (or
  the work window, matching the existing handoff surface), and repeated opens
  of the same item focus the existing tab instead of spawning duplicates —
  changed re-offers navigate the tab in place.
- **Handoffs are governed, not flooded.** papio drives at most two handoff tabs
  at a time (one until a sign-in is verified), queues the rest, closes tabs
  whose job settled, times out stalled drives after three minutes, and updates
  the papio tab group at most once per five seconds instead of thrashing it
  per job.
- **Security checks get attention instead of silence.** A Cloudflare/Turnstile
  challenge or redirect loop on a driven page pauses that job (never solved
  automatically), keeps its tab, cools the provider host down for ten minutes,
  raises the badge, and appears in the popup and inbox with a Go-to-tab
  action; solving it by hand resumes the drive automatically.

### Changed

- **Solicited CLI fixture captures open visibly.** The explicit
  `papio adapter capture` request restores the work window while it runs, so
  visibility-dependent SPAs and consent managers can finish rendering; the
  governed tab is still closed after capture.
- **The popup puts actions where operators look for them.** The current-page
  acquire action is now a paper-plus icon beside Inbox and leaves behind only
  live progress or delivery feedback; idle pages no longer consume a card.
  Papers waiting at institution sign-in sit under the institution session
  rows instead of in a separate amber card, while security challenges retain
  their own attention card. The impact heading and history link now share one
  compact row.

- **Badge precedence is explicit and centralized in the background broker.** A
  broken or disconnected bridge shows `!` first, a blocking permission or
  sign-in state takes the next slot, and the pending triage count is shown only
  when higher-priority attention is clear. Popup and options surfaces continue
  to expose the fuller status and version details.

### Fixed

- **Closing a handoff tab no longer breaks its classification retry.** A
  scheduled retry for a paper whose tab you closed — or one still queued for a
  browser slot — now ends quietly instead of raising an unhandled error inside
  the extension's background timer.

## [0.8.1] - 2026-08-02

### Fixed

- **A file-shaped link is no longer treated as permission to download without
  you.** An offer whose URL looked like a PDF was downloaded straight away, on
  the shape of the link alone, with no reference to whether the work sits behind
  a sign-in. That was safe only by accident: the one kind of offer that carries
  a file URL happens to be marked as needing no sign-in today, and nothing
  prevented a future one from differing. It is also reachable through
  configuration rather than code — an institutional offer carries whatever
  resolver address the operator configured, and *papio* places no restriction on
  its path, so a resolver whose address merely *looks* like a PDF would have
  routed a sign-in-required work straight into a download with nobody present.
  Whether a human must sign in is a property of the work, so it is now checked
  where the download decision is made. Only an explicit "sign-in required"
  refuses; everything else behaves exactly as before.
- **Works behind an institutional paywall are now recognised as needing a
  sign-in.** The daemon recorded several hand-offs as freely fetchable when they
  were not, including ones where this extension had itself reported an
  authentication wall. Those arrive correctly classified now, so such a work is
  held for you to sign in rather than being treated as open access — visible
  here as an amber sign-in count that was previously undercounted, and as work
  correctly queued instead of attempted.
- **A development daemon is no longer told to `brew upgrade`.** The popup's
  version notice assumed every out-of-date daemon came from the cask, so a
  source checkout — whose version carries a `dev` marker — was handed the one
  remediation that cannot work on it, and following it would install a release
  binary over the build under development. A `dev` daemon is now named as a
  development build and pointed at `make dev-deploy`, which is what actually
  rebuilds it, repoints the native-messaging host, and restarts both.
- **The legacy work-window flag is honored in exactly one place.** The
  tri-state handoff-surface setting superseded the older boolean, but the
  migration had been implemented twice — once in the settings getter and once as
  a fallback in the bridge that could never execute. Anyone changing upgrade
  behavior would have edited the unreachable copy and shipped nothing. The
  surface a user gets is unchanged, including the Firefox 128 ESR degradation
  from tab-group mode.

### Changed

- **The wire contract's TypeScript half is now compiler-enforced.** Eighteen
  payload interfaces in `src/protocol.ts` were referenced by nothing, so a field
  added to the daemon struct and the JSON schema but forgotten here produced no
  typecheck error and no test failure. The parser's per-message field lists are
  now typed against those interfaces in both directions: a field the interface
  declares but the parser does not validate, and a field validated under a name
  the interface does not have, are both build errors. No frame that was accepted
  or rejected before changes status.

## [0.8.0] - 2026-07-27

### Added

- **Captures travel over the native-messaging bridge instead of your Downloads
  folder.** Sanitized page HTML is gzipped and sent to the daemon, which stores
  it in its own data directory. This works identically on Chrome and Firefox —
  the old path depended on `downloads.onDeterminingFilename`, which is
  Chrome-only and did not reliably fire even there, so a capture could land as
  `download (1).html` or vanish silently. Gated on the daemon advertising
  `page_capture_v1`. The capture panel is now a build-time developer
  affordance, stripped from shipped Chrome and Firefox bundles rather than
  hidden by a Chrome-only heuristic that left it visible on AMO.

- **Institutional sign-ins now stay visible until you act.** A handoff waiting
  at an identity provider gives the toolbar an amber count, draws attention to
  its restored work window, names its paper while a tab group is expanded, and
  appears in the popup with a **Focus** control. The signal no longer depends
  on catching the first focus grab at the exact moment it happens.
- **Cold institutional handoffs now preflight without disappearing.** An offer
  that needs institutional access waits briefly for a live resolver or provider
  return instead of minting an unattended SAML request. Its amber toolbar count
  remains visible while it waits, and a bounded fallback presents the sign-in
  tab even when session keepalive is disabled; open-access offers still open
  immediately.
- **`papio actions open` can now surface the tab papio already owns.**
  Extension 0.8.0 accepts the job-scoped `handoff_focus` command and restores
  the tracked handoff tab and its minimized work window instead of leaving the
  CLI to open an untracked duplicate. Older extensions are version-gated so
  their strict parser never receives an unknown native-messaging frame.

### Changed

- **The time-saved estimate is now a defensible 5 minutes per paper**, down
  from 20. The old figure implied hours saved after a handful of papers and
  read as marketing; the headline is still a rough estimate the extension
  computes from counts, never a measurement.
- **Dismissing an inbox item no longer asks first.** The row leaves the list
  immediately and the daemon call is held for six seconds behind an **Undo**
  bar (keyboard `u`); dismissing several rows in a row batches them into one
  undo. The confirmation dialog it replaces protected nothing — the daemon
  cannot un-cancel a job, so clicking through it committed the same
  irreversible change, one extra click per row. Leaving or hiding the page
  commits whatever is still waiting. Accepting or rejecting a quarantined PDF
  still asks.
- **The inbox says what a dismissal actually costs.** It mirrors the daemon's
  own rule (a dismissal cancels the job only when the job is parked on that
  action) and names the cancelled acquisition only in that case. Advisory
  `openurl_available` rows and actions left behind on a job that moved on are
  reported as what they are: a closed dead row.

### Fixed

- **SAGE article pages had become unreadable.** The adapter keyed on
  `a#downloadPdfUrl[data-doi]`, an element SAGE no longer renders — the page now
  offers a `View PDF/EPUB` link to an eReader route inside a `View Options`
  panel. Six of nine failed handoffs on one machine were SAGE, every one
  reported as "the provider's UI changed" and parked for manual download. The
  adapter now keys on the semantic `section.format--pdf_epub` panel and derives
  the download from the DOI rather than following the eReader link, which is a
  viewer rather than a file. Captured from a real authenticated page and
  committed as its fixture.
- **Only one `papio` tab group again.** Group rediscovery matched the title
  exactly, so once a group was renamed to carry the paper being worked on it
  became invisible to its own lookup and a fresh group was created beside it —
  three had accumulated on one machine. Adoption is now window-aware and
  serialised so concurrent folds converge, a renamed group is still recognised,
  and existing duplicates are merged on startup rather than left for the user
  to tidy up.
- **The options page reported permissions it did not have, and "revoke all"
  silently did nothing.** `https://*/*` is offered as an escape hatch for
  unlisted providers but appeared in no list the page rendered, so once granted
  it made every specific origin report as granted, and revoking a specific
  origin resolved successfully while changing nothing — the toggle snapped
  straight back. Underneath, the UI painted from the return value of
  `permissions.request`/`remove` rather than re-reading the resulting state, so
  it could claim a change that never took effect. The page now paints from a
  fresh `permissions.getAll()` snapshot, shows all-sites coverage as coverage
  rather than as a broken toggle, makes the all-sites grant itself visible and
  revocable, and either removes it as part of "revoke all" or says plainly why
  access remains.
- **papio no longer blames the adapter when it was never allowed to look.**
  Every page read goes through `chrome.scripting`, so a provider whose host
  permission is not granted is unreadable — and that was reported as the same
  "the provider's UI changed" as a stale adapter, with no indication anywhere.
  A blocked host is now named exactly in the outcome detail, surfaced on the
  toolbar and in the popup with a route to the grant control, and deduplicated
  so a standing condition reports once rather than once per tab update.
- **A single handoff no longer reports the same outcome seven times.** The
  classify retry loop re-emitted `ui_changed` on every second unknown verdict,
  up to the retry cap. It was invisible until the daemon began recording
  provider outcomes; the report is now latched once per drive and released when
  a genuinely new drive begins.
- **The inbox "View PDF" control never worked in any shipped version.**
  `requestNative` named `review_preview_result` as the reply it waits for, but
  `onInbound`'s correlation switch enumerated the other five result types and
  omitted that one — so the daemon issued the loopback preview capability, the
  frame fell through to the ignore-echo default, and every click sat until the
  request timed out reporting "The daemon did not respond in time". Correlated
  replies now route from a single `CORRELATED_RESULT_TYPES` list, and
  `requestNative` rejects an unrouted expected reply immediately regardless of
  whether its caller uses a literal, variable, or helper wrapper.
- **Recoverable institutional login pages are no longer navigated away from.**
  A password-expiry or retry error still surfaces its handoff window, but only
  a title-confirmed Shibboleth/OpenAthens **Stale Request** page is re-driven
  through the resolver.
- **Cancelled handoffs no longer leave a paper title on a live tab group.**
  When a keepalive tab retains the collapsed "papio" group after its last
  handoff closes, papio now resets the group title and collapse state first.
- **A handoff stranded on a dead sign-in page now raises the work window.**
  Stale-SSO recovery reported the failure and re-drove the tab through the
  resolver, but did so *before* the code that surfaces the minimized work
  window — so the one moment papio was certain a human was needed was the one
  moment it stayed hidden. Handoffs could sit unnoticed on an expired
  institutional sign-in for hours. The window is now restored and focused
  first, once per job.
- **Shibboleth "Stale Request" pages are no longer missed.** Some identity
  providers serve the dead page at the *same URL* as the working login form —
  only the document title differs — and detection ran solely on the tab's
  `complete` event, which a browser can deliver before that title resolves.
  Detection now also runs on title-only updates. It deliberately still
  requires the title: a URL-only rule would reload the page out from under a
  half-typed password on the live login form.
- **The stale re-drive is now bounded across service-worker restarts.** Its
  "once per outcome" latch lived in service-worker memory while the dead tab,
  the parked job, and the user's next sign-in attempt all survived a restart,
  so the resolver loop was effectively unbounded. The re-drive is charged to
  the same durable per-job authentication budget as a normal handoff drive.
  At the cap the tab is deliberately left on the failure page — the user needs
  to see it — and the job is reported `human_auth_required`, which keeps it
  parked daemon-side rather than silently looping.
- **A closed inbox page no longer keeps polling the daemon.** The counts poll
  re-armed itself unconditionally, so a page module that outlived its document
  kept issuing triage requests against whatever page replaced it. It now stops
  when the live document is no longer the one it bootstrapped against.
- **The popup’s Focus control can now surface its institutional handoff.**
  Its narrowly scoped broker permission is limited to that focus operation;
  the popup still cannot make inbox or triage changes.
- **`papio actions open` now refreshes an expired sign-in exchange.** A focus
  request re-drives a tab only while it is on an authentication page or is
  waiting for authentication, and otherwise leaves an active provider download
  alone.
- **One stale sign-in document now consumes one recovery attempt.** Browsers
  can report its title and completion at the same time; duplicate callbacks no
  longer exhaust the retry budget or send repeated resolver navigations.
- **The badge returns to blue after institutional sign-in completes.** Pending
  triage work no longer inherits the amber colour reserved for a sign-in
  blocker.

## [0.7.0] - 2026-07-25

### Added

- **Tab-group handoff on Firefox.** The collapsed "papio" tab group now works
  on Firefox 139+ (which added the `tabGroups` WebExtensions API), not just
  Chrome. Older Firefox still falls back to the background work window
  automatically. The handoff group is now coloured orange.
- **Acquisition history and impact stats.** The popup now shows a compact
  "Your papio impact" summary — papers acquired, estimated time saved (at a
  rough 20 minutes of manual chasing per paper), and success rate — with a
  **View history** control that opens a new full-tab history page. The page
  charts weekly acquisitions over the last 12 weeks and breaks down success
  rate, access routes (open access / institutional / licensed API / other),
  and how often an acquisition needed a human handoff. Needs a daemon with
  the `browser_stats_v1` feature; without one the popup hides the summary and
  the history page shows a muted "stats unavailable" note instead of an error.
- **The inbox keeps itself current.** It now refreshes the moment you return to
  the tab, and checks in on its own periodically while the tab stays open, so a
  new or resolved job doesn't wait for a manual Refresh. An auto-refresh never
  reorders the list while a confirmation dialog is open or an action is still
  in flight — it waits until you are done.

### Fixed

- Reloading the extension no longer creates a second "papio" tab group: the
  existing group is rediscovered by title first. (An extension reload clears the
  in-memory group id but leaves the physical group in the window.)
- The inbox popup now closes after you click **Open inbox** on Firefox, instead
  of staying on "Opening inbox…". Chrome already dismissed the popup when the
  new tab took focus; Firefox does not, so it is now closed explicitly.
- A daemon-side inbox or stats query that fails no longer disconnects the
  extension. The daemon used to treat it as a dead connection and drop the
  whole native-messaging session, taking page acquire, the triage inbox and
  the handoff flow down with it; it now replies with an error the extension
  renders as the muted "unavailable" state. Needs a daemon carrying the fix.

## [0.6.0] - 2026-07-24

### Added

- **Tab-group handoff mode.** A new "Where papio opens tabs" setting lets you
  put handoffs in a collapsed "papio" tab group in your current window instead
  of a separate background window (or inline, as before). The group folds away
  when idle, expands and focuses the tab only when you are needed for sign-in,
  then re-collapses; the keepalive session tab joins the same group. Chrome
  only (adds the `tabGroups` permission); Firefox falls back to the background
  work window. Your existing on/off choice is preserved.
- **Options page redesign.** Per-host library access and publisher-terms
  controls are now toggle switches, and the handoff-tab location is a
  highlighted button group, for a clearer at-a-glance view of what is granted
  and where handoffs open.
- **Popup and settings UX polish.** The inbox popup and options surfaces were
  refreshed for clearer status, actions, and at-a-glance state.

### Fixed

- **The background work window no longer accumulates.** papio's dedicated
  handoff window is now closed automatically once no handoff owns a tab in it,
  instead of lingering (or multiplying) across acquisitions. A pinned keepalive
  session tab keeps the window alive; a stale window id left by a manual close
  is dropped so the next handoff opens exactly one fresh window.

## [0.5.1] - 2026-07-23

### Fixed

- **ACM Digital Library articles now download autonomously and paywalled ACM
  pages stay assisted.** The adapter keyed the `article` verdict (and its PDF
  href) on the bottom-of-document `a#downloadPdfUrl` anchor, but ACM emits that
  anchor even on non-entitled "Get Access" pages — so an accessible free/entitled
  article was often left as a manual-download handoff, while a paywalled one
  risked fetching an HTML access page. The adapter now keys on the "PDF/eReader"
  toolbar control (the real entitlement signal, present only when this session
  can read the PDF) and builds the deterministic `/doi/pdf/<doi>?download=true`
  endpoint from the DOI in the page URL, fetched through the session cookie jar.

- **The sticky inbox footer is decluttered to two slim rows.** The access
  legend is gone — every item already states its access requirement inline —
  and the keyboard help shrank from a paragraph to key-chip pairs
  (`j`/`k` select · `a` act · `d` dismiss · `o` open), sharing its row with
  the generated-at stamp. The status-glyph legend stays.
- **Dismissing a human action from the inbox works again.** The inbox and the
  native protocol both speak verdict `dismiss`, but the background broker's
  request guard only accepted `accept`/`reject`, so every dismiss died as
  "Invalid action resolution request".
- **A structured broker rejection no longer masquerades as a daemon
  disconnect.** It renders inline on the affected row; only a genuinely
  failed runtime call flips the connection banner.
- **Inbox browser handoffs now open the broker-owned tab**, rather than opening
  the paper's canonical DOI in an untracked tab. The background service keeps
  the resolver or OA URL private, releases queued handoffs through the existing
  work-window choreography, and focuses the exact tab already correlated with
  the job.
- **Explicit zero-electronic-holdings resolver results now stop the handoff**:
  Alma “No full text available” and Primo NDE “No links are available for this
  record” pages report the existing `no_entitlement` outcome once. Inconclusive
  empty or slow resolver pages remain assisted, and no page text or URL leaves
  the browser.

## [0.5.0] - 2026-07-22

### Added

- **Stale-SSO detection and recovery on handoff tabs**: when a tracked
  institutional handoff lands on an identity-provider failure page
  (OpenAthens/Shibboleth stale or expired session), the extension reports a
  `handoff_outcome` to the daemon for the job's audit trail and re-drives the
  tab through the resolver once, minting a fresh sign-in exchange — no page
  content leaves the browser, only the outcome and the IdP hostname.
- `job_offer` now carries `requires_auth`, so the extension can distinguish
  "open access — just render it" handoffs from ones needing an institutional
  sign-in (groundwork for surfacing this in the popup).
- **Inbox access guidance**: triage actions now say “open access — no login needed” or “sign in to your institution first” when the daemon has classified their access requirements.
- **Citation-style rendering in the inbox**: each item now shows a
  reference-style line (authors, year, hyperlinked DOI) in a user-selected
  citation style — APA, MLA, or Chicago — persisted across visits. The DOI
  link is the citation's locator, replacing the separate "Open DOI" row.
- **Status glyph column in the inbox**: every row leads with a colored glyph
  for its kind (manual download ↓, browser handoff ↗, verify identity ?,
  watch hit ✶, retraction !); unknown kinds from a newer daemon degrade to a
  neutral dot. This replaces the action-kind pill. Hovering a glyph shows
  its meaning instantly (no native-tooltip delay), and the footer legend
  spells out all five. The footer (legend + keyboard help) sticks to the
  viewport bottom, so both stay visible without scrolling.
- **Collapsible backend details per inbox row**: a quiet "⋯" chip at the end
  of the actionable status line reveals item id, job id, and revision as
  three compact columns. Its meaning remains available on hover and as
  "Backend details" to screen readers.

### Changed

- **Inbox visual overhaul**: paper titles are larger, semibold, and clamp to
  two lines instead of truncating at one; the action-kind pill no longer
  stretches into a full-width bar; authors/year render as plain metadata
  prose (labels kept for screen readers); job ids demote to a muted
  monospace line; quarantine file paths collapse to an ellipsized code span
  with the full path in the tooltip. "Open" is styled as the primary action
  on rows where it is the advancing step, while Dismiss/Reject become quiet
  ghost buttons with a danger hover. The header consolidates to two rows,
  the counts line omits zero buckets, link labels capitalize properly
  ("Open DOI"), and rows whose title is just the action kind fall back to
  the paper's DOI styled as a placeholder. Detail text lost its "DETAIL"
  label and reads as plain prose, author lists duplicated into a title's
  " - " suffix are stripped, and the counts line pluralizes correctly.

## [0.4.3] - 2026-07-20

### Fixed

- The options page now requests host access for every registered adapter,
  keeping provider support and Firefox runtime grants in sync.
- Assisted downloads are attributed through the complete adapter registry when
  exactly one tracked job matches the provider host; ambiguous downloads remain
  unowned.
- Firefox now ignores broad native/manual download correlation because it
  cannot steer those files into *papio*'s adoption directory; only exact
  extension-started downloads are acknowledged, so assisted controls remain
  manual while direct extension-API downloads remain automatic.

## [0.4.2] - 2026-07-20

### Fixed

- Repackages the 0.4.1 tracked-provider-host fix after its store workflow
  stopped before either upload when Chrome's publishing client added a required
  API v2 publisher identifier. Extension runtime behavior is unchanged.

## [0.4.1] - 2026-07-20

### Fixed

- Tracked institutional handoffs now classify provider landings from the
  extension's complete adapter registry instead of relying only on the
  protocol-capped `provider_hosts` offer list. Resolver redirects can therefore
  reach every 0.4.0 adapter family while unregistered hosts remain assisted.

## [0.4.0] - 2026-07-20

### Added

- **Acquire this page**: a popup button (shown only when the connected daemon
  advertises the `page_acquire` feature) reads the current tab's
  `citation_doi` metadata under the activeTab grant and asks the daemon to
  acquire the paper; pages without a DOI show "no DOI found on this page"
  and send nothing.
- Adapters can declare `requiresVisible`; their handoff tabs then open in a
  normal, unfocused window instead of the minimized work window (fix path
  for providers that under-render while hidden). No current adapter sets it.
- **14 new fixture-backed provider adapter families**: APA PsycNet, Annual
  Reviews, Taylor & Francis Online, Emerald Insight, Cambridge Core, Thieme
  Connect, Nature, Oxford Academic (Silverchair), MIT Press, BMJ,
  PsychiatryOnline, JAMA Network, Wolters Kluwer/LWW (Ovid journals), and
  HAL — each registered from an authentic captured page (success plus a
  denial capture where one was reachable), doubling adapter coverage of the
  real missing-PDF corpus. Ovid SSO-walled and ISHS Acta Horticulturae
  member-credit pages stay assisted: no authentic entitled capture exists,
  so no adapter is registered for them.
- `scripts/sanitize-fixture.ts`: one-command capture sanitation — reads a raw
  saved page, runs `sanitizeFixture`, verifies the residual-leak guard, and
  writes the committable fixture with its provenance header.

### Fixed

- The developer-only fixture-capture tool no longer leaks its filename
  reservation when Chrome rejects a download; unclaimed reservations expire
  after one minute.
- Fixture sanitization hardened for the new captures: URL-valued provider
  metas (e.g. `citation_pdf_url`, `wkhealth_pdf_url`) keep queryless selector
  evidence instead of being dropped, comments are emptied without merging
  adjacent markup, script/style/SVG bodies are always emptied, and the
  provenance header's provider label is itself guarded against opaque
  observed-host names.

## [0.3.1] - 2026-07-19

First version submitted to Firefox Add-ons (AMO, listed channel). Chrome Web
Store carries 0.3.0 — the cross-store skew is intentional; the listings are
independent.

### Fixed

- The developer-only "Capture fixture" panel in the popup no longer ships to
  store users: it is gated on
  `chrome.management.getSelf().installType === "development"` and appears only
  for unpacked/dev installs.
- The manifest `description` was shortened to fit the Chrome Web Store's
  132-character summary limit (an over-limit summary blocks the store upload;
  `web-ext lint` does not catch it).
- `extension/package.json` version brought back in sync with
  `extension/manifest.json` (the compat preflight in CI enforces they match).

### Changed

- *papio* is now italicised in the extension's own UI, matching the
  product-wide brand convention: a `renderPapio` helper (`src/dom.ts`) wraps
  the wordmark in `<em>` across the popup (daemon status, resolver lede), the
  options page (consent, work-window, and daemon-footer status lines), and the
  static popup/options HTML.

## [0.3.0] - 2026-07-18

First store-submitted version (Chrome Web Store). Shared a version stream with
daemon v0.3.0 — see the root `CHANGELOG.md` `[0.3.0]` section for the complete
extension changes (library-resolver access grants, daemon version-skew
surfaces, Firefox support, background work window, store submission tooling).
