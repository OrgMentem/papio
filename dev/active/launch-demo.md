# *papio* demo storyboard — reviewed 40-second loop

Status: blocked until every preflight check passes. The recording, affiliation
choice, asset approval, and public release remain Ellis's actions.

The three plan reviews found unsafe isolation, incorrect command output, and
unproved Zotero routing. This version replaces those instructions.

This file lives in `dev/active/` because the launch is work in flight. It was
previously in `dev/scratch/`, which is gitignored: the whole plan was invisible
to a fresh checkout, so the next session would have rebuilt it from nothing.
When the launch ships, salvage anything still normative into an ADR and delete
this file.

## Rehearsal environment

`scripts/launch-demo-env.sh` provisions the isolated environment. Read its
`usage` before the first run. It is idempotent and never touches an existing
Zotero library.

    scripts/launch-demo-env.sh init         # build and provision
    scripts/launch-demo-env.sh zotero-cmd   # print the Zotero launch command
    scripts/launch-demo-env.sh snapshot     # record the pristine library
    scripts/launch-demo-env.sh restore      # reset before each rehearsal
    scripts/launch-demo-env.sh status       # what exists, which versions

The root defaults to `$HOME/.local/state/papio-launch-demo`. **Never put it in
`/tmp`.** A first attempt did, and macOS clears `/tmp`: on 2026-08-28 no file in
`/tmp` predated the last boot, so a 256 MB environment containing the only
working demo Zotero profile was one restart from gone.

Three details are load-bearing and were each learned by getting them wrong:

* zotio reads `ZOTERO_*` variables, not `ZOTIO_*`. The wrapper
  `bin/zotio-demo-profile` clears every inherited Zotero credential and pins
  `ZOTERO_BASE_URL` at the demo profile's local API. That wrapper is the
  isolation boundary; run zotio through it and never through a bare binary.
* The profile uses `user.js`, not `prefs.js`. Zotero rewrites `prefs.js` when it
  exits, so settings written there do not survive. Verified: a `user.js` profile
  honoured the demo data directory, enabled the local API, and answered
  `/connector/ping` with 200 on first launch.
* Port 23119 is Zotero's default, so it needs no preference. Only one process
  can hold it, which is why the operator's real Zotero must be closed.

### What the script cannot do

* Close the operator's real Zotero.
* Install the released extension, or grant provider host permissions. Chrome
  requires a trusted prompt; synthesized activation does not satisfy it.
* Sign in to the institution.
* Approve a paper, an affiliation, or any captured frame.

### Reset mechanism

`restore` untars the pristine snapshot over the data directory. It is the only
reset available: `zotio items delete` fails on this profile with
`HTTP 428: Zotero-Server-ID not provided`, because a credential-free profile
makes Zotero's local API refuse writes and there is no API key for the Web API
route. The connector can create and can never delete.

Verified end to end on 2026-08-28 in the durable root: pristine snapshot at 0
item rows, one connector import creating parent `Q6URBQB3` with attachment
`2WI9HVP2`, then `restore` returning the library, the trash, and the storage
directory to empty.

## Fixed artifact choice

- Release the zotio connector-key fix and record its exact version.
- Release a *papio* daemon version that contains commit `7f478cb`.
- Install that exact public daemon binary as both CLI and native host.
- Release extension 0.15.0 or later with the Wiley PDF-viewer fix.
- Record only after the public stores report the selected extension version.
- Pin one exact extension id in the config and matching native-host manifest.
- Abort for any daemon, host, zotio, extension version, or extension id mismatch.

## Fail-closed preflight

### *papio* isolation

- [ ] Create the demo config file before any demo command starts.
- [ ] Set an absolute demo `data_dir`.
- [ ] Set `access_mode = "delegated"`.
- [ ] Copy only the required institution route values.
- [ ] Copy only the selected Chrome extension id.
- [ ] Omit the Firefox extension id.
- [ ] Give the demo browser its own download directory.
- [ ] Set `download_adoption_root` to that directory's direct `papio` child.
- [ ] Give the demo Chrome profile its own matching native-host manifest.
- [ ] Start a new Chrome process with `PAPIO_CONFIG_DIR` in its environment.
- [ ] Keep every demo command in one shell with that same environment.
- [ ] Abort if the config file is missing.
- [ ] Abort if `papio jobs list --json` contains any existing job.
- [ ] Abort if `papio actions list --json` contains any existing action.
- [ ] Abort unless `papio pulse` reports zero waiting items.
- [ ] Abort unless exactly one demo browser session holds the bridge.

Do not run `papio doctor` on camera. Do not use the real config for comparison.
The native host opens its log before origin validation.

### Zotero and zotio isolation

- [ ] Save work and close the real Zotero process.
- [ ] Run the separate demo Zotero profile on local API port 23119.
- [ ] Give that profile a separate data directory.
- [ ] Keep that profile signed out and disconnected from Zotero Sync.
- [ ] Enable only its local API and desktop connector.
- [ ] Put only approved public demo items in one demo collection.
- [ ] Give zotio separate config, data, state, and cache directories.
- [ ] Clear inherited Zotero API keys, user ids, base URLs, and group ids.
- [ ] Point zotio only at `http://127.0.0.1:23119/api/users/0`.
- [ ] Configure the demo `zotio.executable` as that fail-closed wrapper.
- [ ] Prove one rehearsal attachment appears only in the demo profile.
- [ ] Prove the real Zotero library remains unchanged.

`scripts/launch-demo-env.sh init` satisfies the directory, credential, base-URL,
and wrapper items above. The remaining items are the operator's.

Do not use `ZOTIO_DEMO=1` for this recording. Its `demo.db` is a read sandbox,
not a writable Zotero desktop profile.

### Route proof

- [x] Use Wiley DOI `10.1111/rego.12568`.
- [x] Ellis approved its public paper title and access fact.
- [x] Create the proof jobs only in the empty demo store.
- [x] Confirm the job reaches `awaiting_human`.
- [x] Confirm it creates one open `openurl_handoff` action.
- [x] Confirm the isolated resolver reaches Wiley's PDF viewer.
- [x] Grant Wiley access through Chrome's trusted permission prompt.
- [x] Record the public 0.14.0 adapter's `ui_changed` result.
- [ ] Verify the current 0.15.0 extension completes the download hands-free.
- [ ] Release that working extension before the final recording.
- [ ] Complete one full unrecorded download, validation, and Zotero import.
- [ ] Abort the launch after two days without one clean rehearsal.

### Capture boundary

- [ ] Use a neutral terminal prompt and empty scrollback.
- [ ] Capture a fixed browser page viewport only.
- [ ] Exclude the URL bar, tabs, bookmarks, profile controls, extensions, and
      download bubble.
- [ ] Fold terminal output that contains a resolver URL.
- [ ] Pause capture before the first sign-in page.
- [ ] Complete login, MFA, account prompts, and consent prompts off camera.
- [ ] Resume only on a stable article page with no identity prompt.
- [ ] Label every cut and every time compression.
- [ ] Record no title, DOI, or affiliation until Ellis approves that exact value.

## Frames

One edited successful run. Every cut is visible. The browser crop never includes
browser chrome.

| # | Time | Action | Screen and caption | Normal stop | Abort and discard |
|---|---|---|---|---|---|
| 1 | 0–4s | `papio acquire "<approved DOI>" --auto-import` | Neutral terminal. "One command." | The demo job id appears. | Any real path, identity, wrong DOI, or unrelated job appears. |
| 2 | 4–9s | `papio jobs get <job-id>` after the job parks | Final `awaiting_human` snapshot. "Open sources first. This one needs your library." | The one demo job and action appear. | Another job, action, title, or route appears. |
| 3 | 9–14s | `papio actions open --job <job-id>` | Fixed browser viewport on the institution resolver. "One tab in your browser." | Exactly one demo resolver tab appears. | Another tab, badge, private title, or browser chrome appears. |
| 4 | 14–22s | Pause capture. Ellis completes sign-in if needed. Resume on Wiley's stable article or PDF-viewer page. | Labeled cut: "Human sign-in omitted." The granted adapter completes one hands-free download. "You sign in. *papio* handles the paper." | The one approved download starts without another human action. | Any manual download click, credential, account value, signed URL, browser chrome, or unrelated handoff appears. |
| 5 | 22–30s | `papio jobs get <job-id>` after completion | Final `imported` state and event names. "Validated before import." | The final imported snapshot appears. | A path, filename, institution value, or unrelated job appears. |
| 6 | 30–37s | Show the separate demo Zotero profile | Crop to the approved item and its new attachment. "Filed into Zotero." | One attachment appears on the approved item. | Any other item, collection, account, or sync surface appears. |
| 7 | 37–40s | `papio jobs receipt <job-id>` | Principal, attempted tiers, component roles, SHA-256 values, and bundle hint. "Hash and provenance bundle available." | The command exits. | A path, filename, institution value, or unexpected field appears. |

## Command contract

- `papio acquire <doi> --auto-import` returns the created job. It does not stream
  later states.
- `papio jobs get <id>` prints one snapshot and event names. It does not print
  validation evidence.
- `papio actions open --job <id>` focuses the one open handoff.
- `papio jobs receipt <id>` prints receipt summary fields and a bundle-export
  hint. It does not print the selected source.
- `papio acquire --from-zotio` only queues missing items. It is not part of this
  recording.

## Prime-time release gate

The launch remains paused until every item below passes. A partial pass does not
authorize recording or store submission.

### Artifact gate

- [ ] Release the zotio connector-key fix first.
- [ ] Raise *papio*'s zotio floor to that released version.
- [ ] Release the empty-store protocol fix in the daemon.
- [ ] Release the Wiley PDF-viewer route in the extension.
- [ ] Install the exact released daemon, native host, zotio, and store extension.
- [ ] Complete the extension release QA matrix against those frozen artifacts.
- [ ] Provision the environment with `ZOTIO_BIN` set to the released zotio, so
      `manifest.json` records a released artifact and not a local build.

### Clean environment gate

- [ ] Use one `PAPIO_CONFIG_DIR`, one data directory, and one socket.
- [ ] Use one browser profile and one installed extension id.
- [ ] Use no CDP, WebDriver, unpacked extension, or second browser session.
- [ ] Start with zero jobs, actions, downloads, managed tabs, and extension jobs.
- [ ] Run one isolated Zotero profile on connector port 23119.
- [ ] Keep the real Zotero profile closed during the run.
- [ ] Confirm zotio returns both permanent parent and attachment keys. Proven
      live against Zotero 7 on 2026-08-28; see
      `zotio/dev/field-report-2026-08-28-connector-live-verification.md`.
- [ ] Run `scripts/launch-demo-env.sh restore` before every rehearsal, and
      confirm `status` reports zero library items.
- [ ] Confirm `scripts/launch-demo-env.sh status` reports the released zotio
      version and a present pristine snapshot.

### End-to-end gate

Complete three consecutive rehearsals with three distinct approved works.
Reset the count to zero after any failure or manual correction.

Each run must satisfy every condition:

- [ ] One `papio acquire` creates one job.
- [ ] One `papio actions open` creates or focuses one provider tab.
- [ ] Login is the only permitted human browser action.
- [ ] No human clicks a PDF or download control.
- [ ] Exactly one download starts.
- [ ] The job reaches `imported`.
- [ ] The demo Zotero profile shows one new parent and one stored PDF.
- [ ] The import records both permanent Zotero keys.
- [ ] `papio jobs receipt` returns the final receipt.
- [ ] No `ui_changed`, transport disconnect, auth loop, duplicate tab, retry,
      extension reload, state reset, or manual database repair occurs.

### Review gate

- [ ] Full *papio*, extension, and zotio suites pass from the release commits.
- [ ] Technical and security reviewers report no release blocker.
- [ ] The unrecorded 40-second storyboard passes once against public artifacts.
- [ ] Ellis approves the paper, affiliation treatment, and capture boundary.

## Export review

1. Inspect the source footage at normal size.
2. Inspect the exported GIF and MP4 frame by frame.
3. Inspect both exports at 4× zoom.
4. Search terminal text for local paths, route values, and account values.
5. Confirm no captured frame contains authentication.
6. Confirm every cut has a visible label.
7. Confirm the GIF is at most 8 MB.
8. Get separate approval for the affiliation, paper, GIF, and public post.

Save an approved GIF as `docs/assets/demo-loop.gif`. Do not stage or publish the
Show HN post until the README and docs use the same reviewed wording.

## Unresolved

* `zotio journal undo` cannot reverse an import-created item. The refusal is
  fail-closed, so it is a capability gap and not a data risk, but it means the
  snapshot restore is the only reset. Recorded in the zotio field report.
* The connector failure paths (unresolved `saveItems`, failed attachment,
  ambiguous read-back) are covered against a fake server only. Forcing them
  against real Zotero needs a fault-injection proxy in front of port 23119, and
  no such harness exists.
