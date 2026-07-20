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

## Extension store submission

The extension is **decoupled from `v*` daemon tags** — its own version cadence
(`extension/manifest.json`), resubmitted only when it changes. Listing copy,
per-permission rationale, and privacy disclosures live in
`extension/docs/chrome-web-store-listing.md` (Chrome) and
`extension/docs/amo-listing.md` (Firefox). The privacy-policy URL CWS requires is
`https://orgmentem.github.io/papio/privacy/` (`docs/privacy.md`).

### Automated submission (`extension-submit.yml`)

Bump `extension/manifest.json`, push an **`ext-v<version>`** tag (must match the
manifest), or run the workflow manually. The **`store-submit` GitHub Environment**
gates the job behind a required-reviewer approval — that approval IS the "human
authorizes publication" step. Chrome uploads a **draft** by default (you click
Publish in the dashboard); `chrome_publish=true` also submits for review. Firefox
signs + submits the listed version to AMO (`--upload-source-code` is required —
the bundle is bun-processed). Local equivalents: `cd extension && bun run
submit:chrome [--publish]` / `bun run submit:firefox listed`.

### The `store-submit` environment

Required-reviewer gate on the submission job. Config that bit us this session:
- **Deployment ref rules must allow both `ext-v*` (tag) AND `main` (branch).** The
  tag rule alone blocks manual `workflow_dispatch` runs (they execute on `main`),
  which then fail *instantly with zero steps* — the deploy ref is rejected before
  the job starts, which looks baffling until you check the environment rules.
- Keep **"Prevent self-review" OFF** for a solo maintainer, or you can never
  approve your own run.
- **No environment secrets needed** — the org/repo secrets are visible to the
  gated job.
- Approve in the Actions UI, or: `gh api
  repos/OWNER/REPO/actions/runs/<id>/pending_deployments -F
  'environment_ids[]=<id>' -f state=approved`.

### One-time setup (per store — cannot be automated)

- **CWS item** must be created by hand once (the API cannot create the initial
  listing). Upload a first ZIP in the dashboard; that assigns the permanent
  extension ID.
- **CWS OAuth creds** (mint once): Google Cloud → enable **Chrome Web Store API**
  → OAuth consent screen **External**, **published to production** (in "Testing"
  the refresh token expires after 7 days) → OAuth client of type **Desktop app**
  (NOT "Chrome extension" — that type is for in-page OAuth and cannot do this
  flow) → `npx chrome-webstore-upload-keys` mints `CWS_REFRESH_TOKEN`.
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
The per-extension `CWS_EXTENSION_ID` is a repo/environment secret — never org-wide.

**Current status — all secrets exist and work; do not re-verify or re-mint.**
As of v0.6.0 (2026-07-20): org secrets `CWS_CLIENT_ID`, `CWS_CLIENT_SECRET`,
`CWS_REFRESH_TOKEN`, `WEB_EXT_API_KEY`, `WEB_EXT_API_SECRET`,
`HOMEBREW_TAP_GITHUB_TOKEN`, `SCOOP_BUCKET_GITHUB_TOKEN`, `WINGET_GITHUB_TOKEN`
(all visibility=selected, incl. papio; set 2026-07-19) plus repo secret
`CWS_EXTENSION_ID`. The tap/bucket tokens are proven by the green v0.6.0
release run; the CWS/AMO creds by the initial store submissions. Only revisit
on a `401`/auth failure (names via `gh secret list --org OrgMentem` /
`--repo OrgMentem/papio`; values are write-only — recover from the password
manager or re-mint, never from GitHub).

Set them without leaking values: `gh secret set NAME --org OrgMentem --visibility
selected --repos papio` uses a hidden prompt — never pass `--body` (shell history).
Reuse account-wide creds (`WEB_EXT_*`, the CWS OAuth trio) by adding repos to
`--repos`; keep `CWS_EXTENSION_ID` per-repo.

### Screenshots

CWS wants 1280×800 or 640×400. *Generating* them is scriptable (render
`popup.html`/`options.html` with representative state via headless Chromium — no
daemon needed; capture at 3× and downscale so text stays crisp), but *uploading*
is **manual**: neither store exposes a listing-asset API. Listing metadata
(description, screenshots, category, privacy) is dashboard-only and is
**preserved when you upload a new package** — Save draft, then Package → Upload
new package replaces only the code.

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
  0.3.0, AMO 0.3.1) is fine; the listings are independent.
- **The popup's daemon-update hint goes stale under decoupled cadences.** The
  extension compares the daemon's reported version against
  `__PAPIO_DAEMON_VERSION__` stamped at extension *build* time — daemon
  releases shipped after the last extension build won't trigger it. Treat it
  as a floor hint; the CLI's once-daily update hint is the real channel.
