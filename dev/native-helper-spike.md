# Native helper reuse and first executable spike

Evidence date: 2026-09-21. Development tooling only; no production daemon,
extension, job-policy or native-host changes.

## Result

An independent program completed a local article → PDF viewer → Download → Save
sequence with four live Jev decisions and native OS actions. The coding agent
opened the fixture and put Chrome in the foreground before starting the program;
it supplied no targets or clicks during that run. No CDP, WebDriver or debugger
was used. The downloaded file matched the source SHA-256, was 5,661 bytes and
three pages, and its rendered first page matched the synthetic corpus fixture.

The run took 13.16 seconds, with 4,799 input and 250 output tokens. Median model
latency was 333 ms (Jev 1.13.0). These are fixture observations, not provider
performance estimates. The local deterministic backend also executes through
the same interface; it is a fixture oracle, not a local AI model.

**The initial article-to-viewer background run failed.** The background Jev run
made no progress after one decision. Native AXPress also acknowledged browser
actions without causing navigation in both initial background and foreground
trials. The private window-event sequence worked in the foreground reference
run; AXPress worked for the native Save dialog. Dispatch success must not become
an acquisition-success signal.

The pointer matched at action endpoints and the run's beginning/end. Focus
stayed in the owned Chrome window and its Save dialog during the successful
run. This is sampled evidence, not proof that no temporary keyboard-focus change
occurred between samples. Creating the initial fixture tab brought Chrome to
the foreground despite `open -g`; setup must also be included in interference
accounting. The one owned tab was closed, the previous foreground app restored,
and both fixture servers stopped. The original 21 browser tabs remain.

This proves a native decision/action/download seam. It does **not** prove a real
publisher acquisition, daemon adoption, identity validation through Papio,
repair learning, Windows/Linux operation or fully unattended background use.
Private receipts, failed trials and the supervised positive control remain in
`dev/scratch/`; failed attempts were not reset or counted as successes.

## Resident PDF save check — 2026-09-21

A later test started with a fully loaded synthetic PDF in Chrome 153.0.8010.52.
Pure AXPress on Download and the native Save dialog saved the correct 5,661-byte,
three-page file while another app stayed in front. The passive monitor recorded
267 samples with no app, window or pointer changes and no missing channels.
This proves background saving for that viewer state; it does not prove background
article navigation or uninterrupted keyboard focus. The request log showed a
second PDF GET during saving.

The next test revoked the fixture URL after the viewer loaded. Every subsequent
request, including Range and query variants, returned HTML with status 410.
Three independent replay checks confirmed this before native actions began.
Chrome dispatched Download and Save, made another GET, and saved no PDF. The
viewer still displayed the loaded paper. The run ended `no_progress`; 630 monitor
samples showed no app, window or pointer changes or missing channels.

The fixture now has an irreversible revocation control and a request journal.
The runner's `--resident-pdf` mode checks the loaded viewer, revokes the URL,
checks the replay failures, and permits only the native save sequence. Success
requires the exact source SHA-256 and byte count, with no PDF response after
revocation. It uses the local deterministic backend and makes no model calls.
It neither adopts the file nor creates a Papio job.

Firefox 156 opened a native Save As dialog after one background AX Save action.
It made no new PDF request after revocation. The helper nevertheless ended
`no_progress`: Firefox exposed the attached sheet through `AXFocusedWindow`,
while omitting it from the document window's children. The sheet's `AXParent`
and `AXWindow` both identified the retained fixture window. The dialog also
remembered a different save folder. No file was saved. The monitor recorded
384 samples with no app, window or pointer changes or missing channels.

The helper now binds that attached sheet by element identity, checks the exact
fixture filename, and exposes a deterministic Downloads sidebar action before
Save when needed. It retains the sheet identity and rejects replacement or
loss. Filename and folder changes affect the revision. This is a development
fixture mechanism: the folder label alone does not establish a production
save-path binding. The runner still requires the exact file in its expected
Downloads path. The corrected live test is pending a desktop unlock. After final dialog Save, the runner
checks only the expected file until its deadline; it does not replay the save
or treat the dismissed dialog as an immediate failure.

Checks after this slice: 2,753 extension tests passed, one existing test skipped,
TypeScript checking passed, and all eight Swift tests passed. The helper builds.
These checks do not replace the pending live Firefox test.

Run this check with a new fixture directory and its exact `pdfURL` already open
in one owned browser tab. Use a new result directory and retain the server until
the final request audit finishes:

```sh
bun run extension/tools/native-spike-run.ts \
  --helper dev/scratch/native-spike-swift-build/debug/papio-native-spike \
  --fixture dev/scratch/NEW_FIXTURE/fixture.json \
  --run-dir dev/scratch/NEW_RESULT \
  --browser chrome --resident-pdf \
  --backend local-fixture --delivery ax --attention background
```

`--browser firefox` selects the installed release Firefox. The helper emits an
explicit document/dialog surface and includes it in the observation revision.
The Firefox check requires the known fixture's title, DOI and page-count marker;
it does not treat that check as general PDF readiness. Read `result.json` and
`resident-proof.json`: a zero process exit status alone does not mean success.
The monitor report records sampled attention changes and gaps separately.

This test rules out the observed Chrome Download/Save path as a resident-byte
solution. A visible PDF and a successful baseline save are insufficient. Keep
the signed-URL guard in production; a second fetch can fail after the original
transfer succeeds. The fixture results do not establish publisher support.

## Reuse decision

| Candidate | Relevant capability | Decision |
| --- | --- | --- |
| [AXorcist](https://github.com/openclaw/AXorcist/tree/4bc531de8c36bf0e4740ab1b1a3a026b3c6a5eb9) | Swift accessibility elements, queries, actions and window resolution; macOS only | Linked for the Mac spike, pinned to a source revision |
| [trycua native platform crates](https://github.com/trycua/cua/tree/9bbfa7dd3e27ca7f1861ede70aaca390174493f9/libs/cua-driver/rust/crates) | Mac AX, Windows UIA, Linux AT-SPI and native input | Leading portable source base to evaluate; reuse selected native code |
| [Terminator](https://github.com/mediar-ai/terminator/tree/73a381c0c1c33eda55f2c0ecb1d918bf5ec7561a/crates/terminator/src/platforms) | Windows UIA implementation in inspected revision | Useful Windows reference; not a demonstrated three-platform foundation |
| [macOS-use](https://github.com/browser-use/macOS-use) / [TypeSafe computer use](https://github.com/awlevin/typesafe-computer-use) | Mac AX/input and model-loop examples | Useful references; not portable, unobtrusive drop-ins |
| [RobotGo](https://github.com/go-vgo/robotgo) / [nut.js](https://github.com/nut-tree/nut.js) | Pointer/keyboard automation | Not the semantic accessibility foundation; physical-input fallbacks need separate evaluation |

AXorcist and trycua are MIT licensed; the adapted event code retains its notice
in the helper directory. Audit transitive licenses when distributing a binary.
AXorcist currently requires Swift 6.2 and macOS 14+. This spike builds only its
library product, not its separate CLI.

Do not adopt trycua's complete driver blindly. Its browser layer includes CDP;
its native implementations are separately useful. Its
[Chromium window-click recipe](https://github.com/trycua/cua/blob/9bbfa7dd3e27ca7f1861ede70aaca390174493f9/libs/cua-driver/rust/crates/platform-macos/src/input/mouse.rs)
uses private SkyLight APIs and calls a focus-preparation function. That
[preparation](https://github.com/trycua/cua/blob/9bbfa7dd3e27ca7f1861ede70aaca390174493f9/libs/cua-driver/rust/crates/platform-macos/src/input/skylight.rs)
explicitly defocuses the previous app without raising the target window.
No window raise is therefore insufficient proof of unobtrusiveness.

The spike evaluates the event sequence **without** that focus mutation. Its
background failure does not disprove the full upstream implementation. It also
records a minimal public PID-post variant separately. Neither failed variant
silently escalates to global input or changes permissions.

## Running the fixture

From the repository root, build with the scratch directory outside the unpacked
extension. Dependency checkouts and generated files must not enter the extension
tree.

```sh
swift build --package-path extension/tools/native-spike-macos \
  --scratch-path dev/scratch/native-spike-swift-build \
  --product papio-native-spike

bun run extension/tools/native-spike-fixture.ts \
  --run-dir dev/scratch/native-fixture-example
```

The server binds loopback on a free port and prints a nonce-scoped URL. Open it
in one owned Chrome tab. The fixture uses an existing synthetic PDF corpus file;
it cannot contact a provider. Keep the server running until the trial finishes.

```sh
bun run extension/tools/native-spike-run.ts \
  --helper dev/scratch/native-spike-swift-build/debug/papio-native-spike \
  --fixture dev/scratch/native-fixture-example/fixture.json \
  --run-dir dev/scratch/native-run-example \
  --backend local-fixture --delivery cua-window --attention owned
```

For the foreground reference, select the fixture window before starting. Use
`--attention background` to require unchanged foreground app/window endpoints.
`owned` permits focus transitions only within the bound browser window and its
native dialog; it is not a claim of background operation. The helper never
activates an app. Endpoints and final state are checked, but continuous focus
monitoring is still needed for a stronger guarantee.

`--backend jev` uses the existing trial client and Keychain credential, so it
makes paid calls and needs the user's cloud-inference authorization. It sends
only the local fixture observation. `local-fixture` needs no credential or
network inference. `--delivery ax` selects pure semantic actions; `pid-click`
selects the minimal public event experiment. Native Save dialogs use AXPress.

Budgets are configurable with `--max-decisions`, `--deadline-seconds` and
`--no-progress-seconds`. The fixture defaults are not production policy. An
unchanged page after dispatch waits without more model calls or duplicate
effects. A changed observation during inference invalidates its target. Runs
refuse a pre-existing matching download; use a fresh fixture to prove another
download. Evidence directories must be new and are created private.

Close only the owned fixture tab, stop the server, and restore foreground focus
afterward. Preserve both successes and failures. The fixture makes no Papio
job or native-host configuration changes and performs no Zotero import.

## Next implementation boundary

Retain one decision interface for local and cloud backends, and one native
driver interface across operating systems. Evaluate Windows/Linux native
implementations before expanding publisher coverage. Continue investigation of
background delivery with independent focus/input monitoring; test the actual
upstream implementation rather than infer its behavior from this subset.

For the product, prefer the existing extension for background tab/document
operations it can perform without CDP, and route native viewer/dialog gaps to
the helper. The model can choose freshly observed page controls without adding
a publisher-specific adapter for each one. Unobtrusiveness is an execution
requirement: no unannounced focus/input fallback, and no hidden keyboard-focus
transfer presented as background operation. The precise browser/helper split
and any native attention lease need the next live evidence.

The IOS Press acceptance below now proves one publisher download and adoption.
The subsequent repaired declarative route must independently produce that result.

## Background page follow-up

The next development runner is `extension/tools/page-spike-run.ts`. It builds a
nonce-scoped controller page into the existing unpacked extension's ignored
`dist/` directory. The controller creates one inactive loopback fixture tab and
uses `chrome.scripting`, `chrome.tabs` and `chrome.downloads`; it never attaches a
debugger. The page observer retains actual DOM elements in the isolated world,
uses opaque choices and a per-document identity, and refuses changed links,
replaced nodes, disabled controls and duplicate delivery. The fixture includes
an article link, a button that reveals download options, a PDF link and unrelated
references. This remains fixture tooling, not production acquisition authority.

The first browser attempt failed: Chrome refuses `scripting.executeScript` on
an extension's own page, including its own packaged fixture. The runner now
serves the fixture on loopback HTTP. This requires temporary activeTab access,
which the existing Papio toolbar action can grant on that one fixture tab.
The user approved that temporary access. It was granted on each owned fixture
tab and ended when the tab closed. No permanent host permission changed.

```sh
bun run extension/tools/page-spike-run.ts \
  --run-dir dev/scratch/page-spike-example \
  --extension-id YOUR_UNPACKED_EXTENSION_ID \
  --monitor-helper dev/scratch/native-spike-swift-build/debug/papio-native-spike \
  --backend local-fixture
```

Open the printed controller URL. Once authorized, select the local fixture tab
and invoke Papio's toolbar action. Return to the previous foreground app, then
POST to the printed `startURL` from the terminal. Use a fresh run with `--backend
jev` for model acceptance; the same Keychain authorization and private receipts
apply. Closing the fixture tab revokes its activeTab access. The runner closes
its owned fixture on completion, cancellation or its fifteen-minute setup timeout;
close the controller tab separately and remove only its printed generated bundle
directory. Generated bundles are development artifacts and must not ship.

The native helper now supports `start_monitor` / `stop_monitor`. A separate
child samples app, window and pointer state every 25 ms while inference and UI
operations run. Reports include missing samples, gaps over 75 ms, maximum read
latency and bounded change history. A live one-second smoke check produced 41
samples, no missing channels and one startup gap. These are polling observations:
fast transitions and keyboard-focus changes inside the same window can still be
missed. Native runs retain the report and classify observed disallowed transitions
as interference; the page runner records the independent report alongside the
actual download result. Missing data or a missing report never proves quietness.

The helper build, standalone report-accumulation checks, 20 focused extension
tests and TypeScript check pass. The full extension suite passes 1,714 tests
with one skip. Both test-owned tabs were closed, leaving the original 21 tabs;
the generated controller bundles and loopback servers were removed/stopped. The new browser action path passed the live run below; publisher and adoption
validation remain outstanding.


### Approved background acceptance result

A fresh Jev run completed **three decisions in 2.29 seconds**: follow the article
link, activate the button revealing download options, and select the article PDF.
The normal Chrome extension APIs performed the actions while Finder remained in
the foreground. No native input, CDP, debugger or coding-agent clicks supplied any
acquisition step after the run began. The model made three calls using Jev 1.13.0,
with 1,969 input tokens and 150 output tokens. The downloaded file was 5,661 bytes,
matched the expected SHA-256, had three pages, and its rendered first page matched
the synthetic article.

The independent monitor recorded 94 samples with zero app, window or pointer
changes and no missing channels. Its one 114.5 ms startup gap ended before the
runner started acquisition. This is evidence for an unobtrusive background
fixture path, still subject to polling's limits. It does not measure keyboard
focus within the same window, provider behavior, signed resident-viewer bytes,
Papio adoption or adapter learning.

An earlier run made all three model-selected actions and completed a download,
but Chrome saved it as `paper.pdf` despite the requested unique filename. The
verifier rejected the destination mismatch. That failure receipt is preserved.
The fresh fixture gives the PDF resource itself a unique filename; its actual
browser-reported download path and bytes both pass verification. This avoids a
fixture assumption about Chrome's filename selection; it does not establish
that production download adoption is fixed. Chrome's
[download filename API](https://developer.chrome.com/docs/extensions/reference/api/downloads#event-onDeterminingFilename)
allows filename listeners to influence the destination, which remains an
independent integration check.

Private calls, events, monitor report and first-page verification are retained
in `dev/scratch/page-spike-jev-2026-09-20-d/`; the failed filename check is in the
sibling `-c/` directory. Both owned test tabs and the temporary Finder window were
closed; the original 21 browser tabs remain. Both loopback servers stopped and
the generated extension bundles were removed. No daemon, native-host, original
job or persistent extension-permission configuration changed.

### Job adoption result

The next isolated run used the public
[*Attention Is All You Need* PDF](https://arxiv.org/abs/1706.03762) as fixture input.
Preparation saved the source in private scratch storage. During acceptance, Chrome
downloaded a fresh copy from the loopback fixture into `Downloads/papio/<job>/`.
No file was manually placed in an adoption directory.

Jev 1.13.0 made three decisions using 1,969 input and 150 output tokens. The
download completed in 3.20 seconds. The stored artifact matched the source hash,
contained 15 pages and 2,215,244 bytes, passed Papio's identity validation, and
reached `ready`. Its rendered first page confirmed the title, authors and arXiv
identifier. Chrome download ID 409 and the matching daemon completion event
were captured. Adoption finished before the completion handler's retry, which
logged a misleading `adoption_deferred` event against the already-ready job.
This result establishes adoption and receipt of the completion frame; it does
not establish that the frame handler won the race with directory discovery.

Finder stayed in front. The passive monitor recorded 130 samples, no app/window/
pointer changes and no missing channels. Its 157.9 ms startup gap ended before
acquisition began. Setup included one explicit Open for this isolated job,
extension reload, one inbox visit and the approved temporary fixture grant.
After the start signal, no operator or coding-agent action supplied a PDF step.

An earlier run stopped before download because its delivery choice disappeared
on reload. Chrome documents that
[session storage clears on extension reload, update and browser restart](https://developer.chrome.com/docs/extensions/reference/api/storage#property-session).
The daemon still held the manual-download action. The extension now reconstructs
inert PDF choices from those actions and refreshes them on an otherwise-unbound
Send PDF request. Recovery never reconstructs a provider route or delegated
authority. Regression tests cover empty storage, direct Send PDF without an inbox
visit, changed pages, partial snapshots, retirement and exact download ownership.

The optional adoption runner takes `--job-id`, `--adoption-root` and `--pdf-source`.
It writes `delivery-config.json`; build with `PAPIO_PAGE_SPIKE_CONFIG` pointing to
that file, then reload the extension. This explicit development build enables
one exact controller, loopback PDF and job. Its broker consumes the existing
one-use delivery choice and document checks. It cannot seed job state or accept
an arbitrary URL or job from the controller. Normal builds disable the entry;
release builds reject an experiment configuration. Rebuild normally and reload
before restoring the native host. This is developer plumbing, not a production
agent-acquisition API or evidence that a model action was a human gesture.

The full extension suite passes 1,732 tests with one skip; typecheck and both
browser bundles pass. Normal manifest and isolated-config bytes were restored,
the normal daemon reconnected and doctor passed. All 1,001 original job rows
remained unchanged. The original 21 tabs remain; one original page temporarily
used for the controller was restored from its navigation history. Owned fixture
tabs, two Finder windows, three generated bundles and all test servers were
cleaned up. Private evidence is in
`dev/scratch/page-spike-adoption-2026-09-20/`.

This is a successful fixture-to-Papio adoption test. Real publisher fallback,
signed viewer delivery, cross-platform native coverage and the independent
declarative repair canary remain separate acceptance work.

### Real provider and recovery results

`extension/tools/provider-spike-run.ts` now runs the shared decision loop against
one explicit HTTPS article and DOI. The extension supplies observed controls and
executes the selected existing element through `chrome.scripting`. It uses no
CDP, debugger, WebDriver, global input or browser-generated PDF URL. The native
helper only monitors foreground app/window and pointer changes during these runs.
The provider observer sends the article citation and visible control labels to
Jev; real URLs, form values, account/navigation areas and execution fingerprints
stay local. Local fingerprints bind the decision to the current document,
element identity, surrounding form and hidden values.

The first successful provider trial used an assisted job to prevent the packaged
adapter from downloading first. Jev selected IOS Press's JavaScript-backed PDF
control. Chrome download 411 landed in the job directory; Papio adopted the file,
passed identity validation and reached `ready`. The stored PDF was 363,923 bytes,
had five pages and matched the independently checked title, DOI and first page.
Two Jev 1.13.0 calls used 1,705 input and 109 output tokens; elapsed time through
download was 2.85 seconds. A separate controlled-fault recovery repeated the
result in 2.81 seconds after the daemon recorded the broken adapter's drift.
Both monitors had 113 samples, no missing channels and no app/window/pointer
changes. Startup gaps preceded acquisition; polling does not establish continuous
keyboard focus or provider invisibility.

The first provider attempt exposed a harness error: after a correct click, Jev
classified the temporarily disabled button as blocked while Chrome was starting
the download. Closing the managed tab cancelled the job. That failed job remains
preserved. The shared loop now checks correlated transfer progress before another
decision and before accepting a blocked result. The controller retains a managed
tab until the daemon retires its binding. No model opinion overrides real download
completion; adoption remains a separate daemon check.

For an already-recorded drift, Papio has deliberately closed the failed tab and
retained an inert manual-download correlation window. The runner can open its own
background article tab against that existing record and validate the live DOI.
It never creates or edits an ActiveJob, offer, permit or daemon binding. This is
an explicitly authorized developer experiment, not a shipped fallback scheduler
or a reinterpretation of an assisted job as delegated authority.

Run with a fresh private directory, isolated job/configuration, existing provider
host access and the passive monitor helper:

```sh
bun run extension/tools/provider-spike-run.ts \
  --run-dir dev/scratch/provider-run-example \
  --extension-id YOUR_UNPACKED_EXTENSION_ID \
  --job-id YOUR_ISOLATED_JOB_ID \
  --entry-url HTTPS_ARTICLE_URL --doi ARTICLE_DOI \
  --adoption-root YOUR_BROWSER_DOWNLOADS_PAPIO_DIRECTORY \
  --monitor-helper dev/scratch/native-spike-swift-build/debug/papio-native-spike
```

Open the printed controller URL, return to the idle foreground app, and POST to
its nonce-scoped `startURL`. For controlled recovery, `--drift-evidence` accepts a
private `jobs show --json` receipt naming that parked job and its daemon-recorded
`ui_changed` outcome. Verify `ready`, the stored PDF and interventions separately.
A fresh job in an existing store may reuse an artifact cache: the declarative
canary needs a new empty store, with earlier stores preserved. Close owned
controller tabs and remove their generated bundle directories after each run.

Private source captures, model receipts, browser events, failed attempts and
artifact checks are in `dev/scratch/provider-spike-2026-09-20/`. These results
cover one provider's POST-form mechanism. Signed resident-viewer delivery and
Windows/Linux acceptance remain separate work.

### Controlled repair canary

A separate checkout deliberately replaced the IOS Press control's class with an
absent one. The live adapter failed, and the daemon retained a sanitized drift
capture correlated with its own `ui_changed` outcome. Jev then recovered that
same parked job through the retained manual-download window, producing Chrome
download 412 and a validated `ready` artifact. No stored state was erased.

The repair generator initially proposed the passive form container. Ranking now
uses interactive affordances and avoids numeric article IDs. Its next candidate
selected the actual control but dropped surrounding constraints; applying it in
the isolated checkout failed five existing IOS Press adverse-case tests. That
candidate was reverted. The corrected generator replaces an obsolete positive
leaf class while preserving ancestor scope, form route/method, ID prefixes and
negative/disabled guards. It leaves unsupported compound transformations blocked
with an explanation rather than emitting a broader executable repair.

`papio adapter repair` regenerated an independently evidenced `0.1.1` proposal.
The applied patch passed the isolated full suite (1,757 tests, one skip), typecheck
and both builds. After a verified browser reload, an empty canary store acquired
the article through the ordinary declarative adapter: Chrome download 413,
363,923 bytes, five pages, matching SHA-256 and first page, identity validation,
and `ready` 5.19 seconds after explicit Open. There were no model calls or manual
PDF clicks. An earlier cache-only result was excluded from browser acceptance.

This establishes the controlled failure → recovery → proposal → rejection and
correction → fresh declarative success cycle. A developer orchestrated the stages;
automatic fallback enrollment, trace ingestion and repair promotion are not yet
production features. The injected fault and experimental adapter revision stay
in the isolated checkout; the normal IOS Press adapter did not need changing.
