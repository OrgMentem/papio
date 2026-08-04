---
name: papio-release
description: "Release runbook for the papio family — papio daemon/CLI, browser extension, and coordination with sibling zotio releases. Use when tagging, bundling, submitting the extension to stores, or bumping cross-component compatibility floors."
---

# *papio* release runbook

Read `AGENTS.md`, `scripts/release.sh`, `.goreleaser.yaml`,
`.github/workflows/release.yml`, `.github/workflows/ci.yml`, and
`.github/workflows/extension-submit.yml` before a release. Read
`~/@dev/zotio/notes/releasing.md` for zotio's authoritative release mechanics;
do not duplicate or improvise its internal flow.

## Artifact map

| Artifact | Authority and release path |
| --- | --- |
| papio daemon/CLI | `.goreleaser.yaml` builds `papio` for all targets and stamps `papio/internal/api.Version` with `-X`. A `v*` tag triggers `.github/workflows/release.yml`; its snapshot smoke test requires `papio version` to contain the tag without `v`. |
| zotio binary | zotio is a separate repository with its own `v0.x` tags, GoReleaser configuration, and MCPB packing. Follow `~/@dev/zotio/notes/releasing.md`; papio only requires the installed version to satisfy `MinimumVersion`. |
| browser extension | `extension/manifest.json` is the version source of truth, versioned and released **independently of `v*` daemon tags** (resubmit only when it changes). `scripts/release.sh <version>` builds Chrome `dist/` and packages the Chrome ZIP (`manifest.json` + `dist/` + `icons/` at the ZIP **root**, not just `dist/`) plus the Firefox package. Store *submission* is automated behind a human gate (`extension-submit.yml`); final *publish* is always the human's dashboard step. |
| direct bundle | `scripts/release.sh <version>` creates `dist/release/<version>/` with papio and zotio Darwin binaries, extension packages, SBOMs, a manifest, and checksums. |

The `papio-{{ .Version }}` branch in `.goreleaser.yaml` is the WinGet PR
branch; GoReleaser updates the Homebrew tap directly. Do not describe it as a
Homebrew branch.

Release CI needs the `HOMEBREW_TAP_GITHUB_TOKEN` and `SCOOP_BUCKET_GITHUB_TOKEN`
repo secrets — fine-grained PATs with `contents: write` on
`OrgMentem/homebrew-tap` and `OrgMentem/scoop-bucket` (the default `GITHUB_TOKEN`
cannot push to those sibling repos). If missing/expired, GoReleaser still
publishes the GitHub release and all binaries, but the Homebrew/Scoop steps fail
`401 Bad credentials` and the whole run goes red (this bit 0.4.0).

## Compatibility floors

| Floor | Protects | Bump only when |
| --- | --- | --- |
| `internal/browser/bridge.go:34` — `MinExtensionVersion` | Daemon acceptance of an extension hello. | The daemon drops support for old extension behavior. |
| `extension/src/background.ts:54` — `MIN_DAEMON_VERSION` | Extension acceptance of a daemon in `hello_ack`. | The extension requires a `hello_ack` feature. |
| `internal/zotio/client.go:23` — `MinimumVersion` | *papio*'s zotio subprocess preflight. | *papio* invokes a capability that older zotio lacks. Prefer adding it to `RequiredCapabilities` before raising a version floor. |

Floor policy — compatibility is carried by floors and `hello_ack` feature
flags, never by version-number correlation between artifacts:

- **Additive capability** → advertise it in `hello_ack` `features[]` and gate
  extension behavior on its presence. Never bump a floor for an additive
  change. (Store extensions auto-update while daemons update manually, so the
  common skew is *new extension + old daemon* — the extension carries the
  backward-compat burden.)
- **Hard dependency** → bump the floor **in the same commit/PR** as the change
  that creates the dependency, to the counterpart release that introduced it.
- New protocol fields stay optional/`omitempty` within `papio-browser/1`.

Run the compatibility preflight before tagging: `release_metadata.py compat`
runs from `scripts/release.sh` and release CI. Floors are mechanically
enforced there; decide **when** to move them by the rules above, then let the
script verify the declaration is coherent. Running it standalone requires
explicit args (there are no defaults):
`python3 scripts/release_metadata.py compat --repo-root . --papio-version
<x.y.z> --zotio-binary "$(command -v zotio)"` — `--zotio-binary` is required
unless `--skip-zotio` is passed.

## Changelogs

Two files, split by **shipped artifact** (not by commit):

| File | Covers | Keyed to |
| --- | --- | --- |
| `CHANGELOG.md` | daemon + CLI | `v*` tags |
| `extension/CHANGELOG.md` | browser extension | `ext-v*` tags / manifest version |

Attribution rule: *which artifact's user-visible behavior changed?* A protocol
change spanning both sides gets an entry in **both** files — that is correct,
not duplication (each user population sees a different behavior change).
Commits never need to be cleanly one-sided; changelogs are written by hand,
not derived from `git log`. Both accumulate under `## [Unreleased]` and are
finalized to `## [x.y.z] - date` at tag time.

Both changelogs are published on the docs site (`docs/changelog/*.md` are
snippet includes of the real files — edit the real files, never the docs
mirrors). This is the only changelog CWS users get (CWS has no version-history
UI). AMO *does* have a public per-version "Release Notes" field — paste the
extension changelog entry into the AMO Developer Hub after each upload
(`web-ext sign` cannot set it).

**No private-tool names in public prose.** Changelogs, README, and `docs/`
are published; never name internal tooling (glean, beads, run IDs) in them —
attribute fixes neutrally ("a triaged audit pass"). Enforced mechanically:
both repos' `.githooks/pre-commit` reject staged CHANGELOG/README/docs
content matching `glean|beads` (papio and zotio alike). Commit messages and
GitHub release notes deserve the same discipline — GoReleaser derives release
notes from commit *subjects*, and the hook cannot see those, so keep tool
names out of subjects for release-bound commits.

## Release order

1. If `MinimumVersion` moved, release zotio first: follow
   `~/@dev/zotio/notes/releasing.md`, ensure its tag satisfies papio's floor,
   and complete zotio-side validation. Otherwise do not manufacture a zotio
   release.
2. Tag papio. The tag-triggered release workflow runs GoReleaser, publishes the
   binaries, updates Homebrew, and opens the WinGet PR from
   `papio-{{ .Version }}`.
3. Release the extension last. Store review can lag days. The extension must
   tolerate old daemons; `hello_ack` feature flags exist for that purpose.
   Never gate a daemon release on store approval.
4. Build the direct-distribution bundle locally with
   `scripts/release.sh <version>` after the required tagged inputs exist.

Treat config deployment as load-bearing: strict mode rejects unknown fields, so
ship a config change and the binary that understands it together. Build, move
the binary into place, then run `papio daemon stop`; there is no `daemon
restart`, and the next command autostarts the new daemon.

Local deploy after a tagged release: papio installs as a Homebrew **cask**
(`/opt/homebrew/Caskroom/papio/<ver>/papio`), and the tap can deliver the new
binary via scheduled `brew upgrade` without any action — the running daemon
keeps its old version until restarted, so the machine sits in silent skew
(0.6.0 deployed under a live 0.1.0-dev daemon within hours of tagging). The
CLI detects this and prints the remediation (`daemon is running X but this
CLI is Y — run 'papio daemon stop'`); `papio daemon status` surfaces it too.
Deploy = `brew upgrade --cask papio` (or let the tap do it) → `papio daemon
stop` → any command autostarts the new daemon and runs pending schema
migrations. Verify with `papio daemon status` + `papio doctor`.

**A papio deploy has THREE binary locations, and the third one bites.**
`papio native-host install` pins `~/.config/papio/bin/papio-native-host` as a
symlink to whichever `papio` binary ran the install (e.g. `~/.local/bin/papio`)
— NOT the Homebrew path. The browser-facing native host is a
frame-validating hop: a stale one accepts `hello`, proxies the NEW daemon's
`hello_ack` (so the extension enables new features), then drops the session
on the first frame type it predates — the popup flashes an error and shows
"daemon isn't reachable" while `papio daemon status` says ok. The hello_ack
feature contract does not cover this hop. After deploying a binary with
protocol changes, also refresh the symlink target (`ls -la
~/.config/papio/bin/`) and kill the running `papio-native-host` process so
the browser respawns it on the new binary.

## Protocol bump policy

Keep additive optional fields on `papio-browser/1`. Update both parsers, the
schema, and fixtures in **one commit**:
`internal/protocol/protocol.go`, `extension/src/protocol.ts`, and
`protocol/browser-v1.schema.json`.

Use `papio-browser/2` only for an incompatible change. Document a lockstep
migration plan before merging; do not tag first and design compatibility later.

## Pre-tag checklist

- [ ] Confirm the relevant gates are green in both repositories.
- [ ] Run the compatibility preflight and resolve every floor mismatch.
- [ ] Finalize the `[Unreleased]` section of the changelog matching the tag
      stream (`CHANGELOG.md` for `v*`, `extension/CHANGELOG.md` for `ext-v*`);
      see the Changelogs section for the attribution rule.
- [ ] Regenerate documentation with `make docs-gen` and commit any generated
      drift.
- [ ] Confirm `extension/manifest.json` has the intended extension version.
- [ ] Confirm the papio tag is on the exact reviewed commit.
- [ ] Exercise one **real, large provider page capture** end to end through a live
      browser session: `papio adapter capture <real provider article URL> --provider
      <id> --scenario success` must report `outcome: captured`. Every committed
      fixture is small, and the frame path only fails on bodies well above fixture
      size, so a fixture-only run cannot see a transport-cap regression — it shows up
      as `nav_failed: browser session disconnected during page capture` and takes the
      live session down with it.

## Post-tag checklist

- [ ] Watch `.github/workflows/release.yml` to green; confirm its version-stamp
      smoke test passed.
- [ ] Check the Homebrew formula **and** Scoop bucket updated. A `401` there means
      the tap/bucket PAT secrets are missing/expired — the release binaries are
      fine; fix the secrets and re-run. (A fully green release run already
      proves both pushes worked — no separate check needed.)
- [ ] WinGet: check `.goreleaser.yaml`'s `skip_upload` before tagging, not after.
      It is currently `true` — paused until the first-package PR
      (microsoft/winget-pkgs#404562) merges, so a release cut while it's
      pending doesn't open a duplicate new-package PR. Once #404562 merges,
      delete the `skip_upload` line so subsequent releases ride the normal
      version-bump path.
- [ ] Validate the tagged papio artifacts before announcing them.
- [ ] Confirm the installed native host is the released binary. `papio doctor`'s
      `native host (version)` check must pass (or read `papio native-host status`).
      One binary is CLI, daemon, **and** native host, and the host enforces its own
      copy of the transport caps and protocol rules, so a symlink still pointing at a
      previous copy keeps all browser work on the old code while the daemon reports
      current. Fix by running `papio native-host install` from the binary you want the
      host to be, then killing the running `papio-native-host` so the browser respawns
      it. On a machine carrying both a packaged install and a local build, update both.

## Extension store submission

The extension is **decoupled from `v*` daemon tags** — its own version cadence
(`extension/manifest.json`), resubmitted only when it changes. Listing copy,
per-permission rationale, and privacy disclosures live in
`extension/docs/chrome-web-store-listing.md` (Chrome) and
`extension/docs/amo-listing.md` (Firefox). The privacy-policy URL CWS requires is
`https://orgmentem.github.io/papio/privacy/` (`docs/privacy.md`).

### Automated submission (`extension-submit.yml`)

Bump `extension/manifest.json`, push an **`ext-v<version>`** tag (must match the
manifest), or run the workflow manually. **The trigger is the authorization** —
there is no reviewer prompt (see below).

**A tag push submits for review on BOTH stores.** CWS and AMO each auto-publish
once their own review passes, so `ext-v<version>` means "release this version",
with no dashboard step left dangling. Before 2026-07-25 the Chrome half stopped
at a draft while AMO went all the way — which is how 0.6.0 sat unpublished for
a day after a red run, live only because someone clicked it by hand.

**A manual `workflow_dispatch` is the staging path**, and its defaults are the
least committal action each store offers, because the stores are asymmetric. A
Chrome draft upload is reversible — replace it and nothing was public. An AMO
submission is not: it permanently consumes the version number (even if you
cancel) and reaches every installed user once review passes. So `chrome`
defaults **true** (draft only), `chrome_publish` **false**, `firefox`
**false**. A dispatch also builds the chosen `--ref` — pass the `ext-v*` tag,
not `main`, if `main` has moved past the release commit.

Firefox signs + submits the listed version (`--upload-source-code` is required
— the bundle is bun-processed). Local equivalents: `cd extension && bun run
submit:chrome [--publish]` / `bun run submit:firefox listed`.

Publishing straight from a tag makes the CWS review lock load-bearing: a tag
pushed while the item is in review would fail at the publish call. That is what
`submit-chrome.sh`'s `status:chrome` preflight now catches, before anything is
built or uploaded.

### The `store-submit` environment

Deployment record plus ref policies; **no required reviewers** (removed
2026-07-25). The gate was theatre under agentic releases: a human gate an agent
clears is latency, not safety. Protection is mechanical instead — the
tag/manifest match step, the two store preflights (`status:chrome` /
`status:firefox`), and the opt-in AMO default above.
- **Deployment ref rules must allow both `ext-v*` (tag) AND `main` (branch).** The
  tag rule alone blocks manual `workflow_dispatch` runs (they execute on `main`),
  which then fail *instantly with zero steps* — the deploy ref is rejected before
  the job starts, which looks baffling until you check the environment rules.
  Removing the reviewers via `PUT /repos/{o}/{r}/environments/store-submit` wipes
  the ref policies unless the body repeats
  `deployment_branch_policy: {protected_branches: false, custom_branch_policies: true}`;
  the named policies themselves survive.
- **No environment secrets needed** — the org/repo secrets are visible to the job.
- **A failed submission leaves the environment page red** and it stays red until
  the next successful deployment supersedes it. It gates nothing; no protection
  rule keys off a previous deployment's outcome. Retire one early with
  `gh api --method POST repos/{o}/{r}/deployments/<id>/statuses -f state=inactive`.
- Runs no longer park at `waiting`, so watching an `ext-v*` run works normally.

### One-time setup (per store — cannot be automated)

- **CWS item** must be created by hand once (the API cannot create the initial
  listing). Upload a first ZIP in the dashboard; that assigns the permanent
  extension ID.
- **CWS OAuth creds** (mint once): Google Cloud → enable **Chrome Web Store API**
  → OAuth consent screen **External**, **published to production** (in "Testing"
  the refresh token expires after 7 days) → OAuth client of type **Desktop app**
  (NOT "Chrome extension" — that type is for in-page OAuth and cannot do this
  flow) → `npx chrome-webstore-upload-keys` mints `CWS_REFRESH_TOKEN`.
- **CWS publisher ID**: Developer Dashboard → **Publisher → Settings**. Store it
  as `CWS_PUBLISHER_ID`; it identifies the developer account and is distinct
  from each extension's item ID. API v2 requires both values even for a
  draft-only upload.
- **AMO**: `WEB_EXT_API_KEY`/`WEB_EXT_API_SECRET` from the AMO API-key page — these
  are **account-wide** (reused across sibling repos, e.g. from Tabloupe). AMO needs
  no manual item creation: `submit:firefox listed` matches the add-on by its Gecko
  id (`papio@orgmentem.com`) and updates the existing listing (or creates it on the
  first listed submission). Contrast CWS, whose first item must be made by hand.
- **Trader/non-trader (EU DSA)**: *papio* declares **Non-trader** — free,
  off-profession OSS, which avoids the trader-only public name+address
  verification. One-time dashboard step; revisit only if the extension is
  monetized (see beads `papio-g9i`).

### Secrets

Shared OAuth creds are **org** secrets scoped to selected repos: `CWS_CLIENT_ID`,
`CWS_CLIENT_SECRET`, `CWS_REFRESH_TOKEN`, `WEB_EXT_API_KEY`, `WEB_EXT_API_SECRET`.
The account-wide `CWS_PUBLISHER_ID` belongs beside those org settings (it is an
identifier, not a credential). The per-extension `CWS_EXTENSION_ID` is a
repo/environment secret — never org-wide.

**Current status.** As of ext-v0.4.3 (2026-07-20) everything is configured and
verified: the OAuth trio, `WEB_EXT_*`, tap/bucket tokens, per-repo
`CWS_EXTENSION_ID`, and `CWS_PUBLISHER_ID` (org secret, selected for
papio+zotio). `submit-chrome.sh` takes the CWS API v2 path
(`chrome-webstore-upload-cli@4.0.1` with `--publisher-id`); the v1 fallback and
its 2026-10-15 retirement are moot unless the secret is removed. Do not re-mint
OAuth credentials.

Secret names can be inspected with `gh secret list --org OrgMentem` /
`--repo OrgMentem/papio`; values are write-only — recover credentials from the
password manager or re-mint them, never from GitHub.

Set them without leaking values: `gh secret set NAME --org OrgMentem --visibility
selected --repos papio` uses a hidden prompt — never pass `--body` (shell history).
Reuse account-wide settings (`WEB_EXT_*`, the CWS OAuth trio and publisher ID)
by adding repos to `--repos`; keep `CWS_EXTENSION_ID` per-repo.

### Screenshots and promo tiles

CWS wants screenshots at 1280×800 or 640×400. *Generating* them is scriptable
(render `popup.html`/`options.html` with representative state via headless
Chromium — no daemon needed; capture at 3× and downscale so text stays crisp),
but *uploading* is **manual**: neither store exposes a listing-asset API.
Listing metadata (description, screenshots, category, privacy) is
dashboard-only and is **preserved when you upload a new package** — Save draft,
then Package → Upload new package replaces only the code.

Promo tiles are listing assets too, and they are easy to forget because nothing
in the repo tracks them: `bun run capture:store`
(`extension/scripts/capture-store-assets.ts`) writes `promo-small` (440×280)
and `promo-marquee` (1400×560) into `extension/web-ext-artifacts/store-assets/`,
which is **gitignored** — the tiles are regenerated output, never committed, so
there is no diff to remind you they changed. They are derived from
`docs/assets/logo-wordmark.svg`, so any brand edit silently invalidates the
live listing until someone re-uploads.

Visuals can only be edited while a submission draft is open, so a tile refresh
cannot be pushed standalone — it rides the next submission or it does not ship.
**Outstanding:** the marquee now carries the peeking-baboon cameo; the live CWS
listing still shows the pre-cameo tile. Re-run `capture:store` and upload
`promo-marquee.png` during the next CWS submission.

### Footguns

- **Store extension ID ≠ dev ID.** CWS assigns its own 32-char ID (it ignores the
  packed key), different from the pinned/unpacked dev ID. A store-installed
  extension reports the CWS ID, so add it to the daemon config
  (`browser.extension_id`, with the dev ID kept in `browser.extension_ids`) and
  re-run `papio native-host install`, or the native host rejects the store build.
- **Manifest `description` ≤ 132 chars** — it is the CWS *Summary*; over-limit
  blocks the upload and `web-ext lint` does NOT catch it. The 16,000-char field is
  the separate detailed *Description*. The summary is plain text (no markup — the
  *papio* italic convention cannot apply there or in the store Name field).
- **Dev-only UI must be gated.** The popup "Capture fixture" panel is gated on
  `chrome.management.getSelf().installType === "development"`; anything dev-only
  must be gated the same way or it ships to store users.
- **`--load-extension` is blocked in Chrome 137+** for automation — you cannot
  auto-load an unpacked build to screenshot it; render the HTML directly.
- **Review timing.** A new listing with broad permissions can take days–weeks;
  version updates that do NOT change permissions review much faster (often
  same-day). Keep the permission set stable across releases.
- **Permissions are already lean.** `activeTab` is reviewer-cheap (no install
  warning; Google's preferred alternative to host perms) and
  `optional_host_permissions` (incl. `https://*/*`) are runtime opt-in — they do
  not surface in the install-time justification form. Do not trim either for
  review speed; the initial full review is the slow part, not these.
- Store-installed extensions auto-update once approved; manually loaded
  (`about:debugging`/unpacked) builds need the new ZIP. The human performs final
  publication; never gate a daemon release on store approval.
- **The extension version lives in TWO files** — `extension/manifest.json` **and**
  `extension/package.json`. The compat preflight (`release_metadata.py compat`, run
  in CI) fails the build if they differ. Bump both atomically with
  `make ext-bump VERSION=x.y.z`. (`bun.lock` doesn't pin the root version, so
  `--frozen-lockfile` is unaffected.)
- **AMO version numbers are unique across channels.** "Version X already exists"
  means it was uploaded before — even as an unlisted/self-distributed signed
  build — so bump the version and resubmit. Cross-store version skew (e.g. CWS
  0.3.0, AMO 0.3.1) is fine; the listings are independent. `bun run
  status:firefox` (`extension/scripts/amo-status.ts` → AMO v5
  `/addons/addon/<guid>/versions/?filter=all_with_unlisted`) checks this before
  a submission, and `submit-firefox.sh` preflights it and stops before building
  on a duplicate. **The two stores block on different things**: AMO reviews per
  *version*, so a version sitting in its queue does NOT stop a new upload —
  only the reused version number does. CWS locks the whole *item* during
  review. So `status:firefox` gates on the duplicate and merely reports the
  queue; `status:chrome` gates on the open review.
- **CWS locks the item while a submission is in review, and the error wording
  varies.** A second *upload* fails with "You may not edit or publish an item
  that is in review"; a *publish* attempted against a review-locked item can
  instead come back as the generic "Your submission does not meet the
  requirements to be published in the store" — which reads like a listing
  defect and is not one. Observed 2026-07-24: run `30067523675` uploaded
  `papio-chrome-0.6.0.zip` fine, then `--publish` died with that message,
  because CI had published 0.5.1 for review ~12h earlier. **CI can now see
  this coming**: `submit-chrome.sh` preflights `bun run status:chrome`
  (`extension/scripts/cws-status.ts` → v2 `publishers.items.fetchStatus`) and
  aborts before building when `submittedItemRevisionStatus.state` is
  `PENDING_REVIEW`, naming the lock instead of failing at the publish call.
  Run `status:chrome` yourself before tagging: it prints the live version and
  state, the submitted version and state (`PENDING_REVIEW` locked, `STAGED`
  approved-awaiting-publish, `REJECTED`), and any takedown/warning. It needs
  `CWS_PUBLISHER_ID`; the v1 fallback path has no equivalent endpoint. Wait
  for review to clear, then `gh run rerun <id>` or re-dispatch. Don't stack
  extension releases while one is in review.
- **A failed `--publish` after a successful upload still leaves a good draft.**
  `submit-chrome.sh` is two API calls; the job goes red on the second one, but
  the uploaded package is already the item's draft. Do **not** re-upload to
  "fix" it — click **Submit for review** in the dashboard once the previous
  review clears (that is how 0.6.0 went live on 2026-07-25 after CI's publish
  step failed the day before). Check the public listing page before assuming a
  red run means nothing shipped.
- **The CWS Items row conflates draft and live.** It shows the *item* status
  beside the *latest uploaded* version — "Published - public" + "Version 0.4.3"
  can mean 0.3.0 is live and 0.4.3 is an unpublished draft. The public listing
  page (`chromewebstore.google.com/detail/<item-id>`) shows the real live
  version.
- **AMO's first listed approval is the slow one — and only that one.** The
  public page 404s until it lands (looks "unlisted"; the dev hub's "Listed
  Version … Awaiting Review" is the true state). `nativeMessaging` excludes
  papio from auto-approval *for a new listing*, so a human reviews it: AMO
  first listing submitted 0.5.0 on 2026-07-22, approved 2026-07-25 — three
  days. Fill that first submission's **Notes to Reviewer** with
  companion-daemon install/test instructions; a reviewer cannot exercise
  papio without the daemon and bounces the review with questions otherwise.
  **Version updates are a different animal: they auto-approve in minutes**,
  with no human in the loop and no reviewer note needed — 0.7.0 was submitted
  and public within two minutes on 2026-07-25, *including* its new
  `tabGroups` permission, so a permission change does not by itself force a
  manual review. Do not budget days for an update or block on writing
  reviewer notes for one. That first approval is now done:
  <https://addons.mozilla.org/firefox/addon/papio/> is public and every later
  upload is a version update, so the install docs (README,
  `docs/guide/getting-started.md`, `docs/concepts/browser-handoff.md`, and
  `papio init`'s printed hint) link the listing instead of `about:debugging`.
- **The popup's daemon-update hint goes stale under decoupled cadences.** The
  extension compares the daemon's reported version against
  `__PAPIO_DAEMON_VERSION__` stamped at extension *build* time — daemon
  releases shipped after the last extension build won't trigger it. Treat it
  as a floor hint; the CLI's once-daily update hint is the real channel.
