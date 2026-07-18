---
name: papio-release
description: "Release runbook for the papio family — papio daemon/CLI, browser extension, and coordination with sibling zotio releases. Use when tagging, bundling, submitting the extension to stores, or bumping cross-component compatibility floors."
---

# *papio* release runbook

Read `AGENTS.md`, `scripts/release.sh`, `.goreleaser.yaml`, and
`.github/workflows/release.yml` before a release. Read
`~/@dev/zotio/notes/releasing.md` for zotio's authoritative release mechanics;
do not duplicate or improvise its internal flow.

## Artifact map

| Artifact | Authority and release path |
| --- | --- |
| papio daemon/CLI | `.goreleaser.yaml` builds `papio` for all targets and stamps `papio/internal/api.Version` with `-X`. A `v*` tag triggers `.github/workflows/release.yml`; its snapshot smoke test requires `papio version` to contain the tag without `v`. |
| zotio binary | zotio is a separate repository with its own `v0.x` tags, GoReleaser configuration, and MCPB packing. Follow `~/@dev/zotio/notes/releasing.md`; papio only requires the installed version to satisfy `MinimumVersion`. |
| browser extension | `extension/manifest.json` is the version source of truth. `scripts/release.sh <version>` builds Chrome `dist/`, packages the Chrome ZIP, and produces the Firefox package. Store submission is MANUAL; final publish is always the human's step. |
| direct bundle | `scripts/release.sh <version>` creates `dist/release/<version>/` with papio and zotio Darwin binaries, extension packages, SBOMs, a manifest, and checksums. |

The `papio-{{ .Version }}` branch in `.goreleaser.yaml` is the WinGet PR
branch; GoReleaser updates the Homebrew tap directly. Do not describe it as a
Homebrew branch.

## Compatibility floors

| Floor | Protects | Bump only when |
| --- | --- | --- |
| `internal/browser/bridge.go:34` — `MinExtensionVersion` | Daemon acceptance of an extension hello. | The daemon drops support for old extension behavior. |
| `extension/src/background.ts:54` — `MIN_DAEMON_VERSION` | Extension acceptance of a daemon in `hello_ack`. | The extension requires a `hello_ack` feature. |
| `internal/zotio/client.go:23` — `MinimumVersion` | *papio*'s zotio subprocess preflight. | *papio* invokes a capability that older zotio lacks. Prefer adding it to `RequiredCapabilities` before raising a version floor. |

Run the compatibility preflight before tagging: `release_metadata.py compat`
runs from `scripts/release.sh` and release CI. Floors are mechanically
enforced there; decide **when** to move them by the rules above, then let the
script verify the declaration is coherent.

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
- [ ] Update both applicable `CHANGELOG.md` files.
- [ ] Regenerate documentation with `make docs-gen` and commit any generated
      drift.
- [ ] Confirm `extension/manifest.json` has the intended extension version.
- [ ] Confirm the papio tag is on the exact reviewed commit.

## Post-tag checklist

- [ ] Watch `.github/workflows/release.yml` to green; confirm its version-stamp
      smoke test passed.
- [ ] Check the Homebrew formula update.
- [ ] Note the WinGet PR; its `skip_upload` setting may mean no PR is created
      until that temporary setting is removed.
- [ ] Validate the tagged papio artifacts before announcing them.

## Extension store submission

Submission tooling lives in `extension/` (bun + web-ext). Credentials come from
`extension/.env` (gitignored; template in `extension/.env.example`). Listing
copy, per-permission rationale, privacy disclosures, and reviewer build
instructions are in `extension/docs/amo-listing.md` (Firefox) and
`extension/docs/chrome-web-store-listing.md` (Chrome).

- [ ] Confirm `extension/manifest.json` version is the release version.
- [ ] `cd extension && bun run lint:firefox` — web-ext lint the built `firefox/`.
- [ ] Firefox (AMO): `bun run submit:firefox listed`
      (`scripts/submit-firefox.sh`). Builds, then `web-ext sign --channel=listed`
      from `firefox/`, uploading a reviewer source archive via
      `--upload-source-code` (required — the shipped bundle is bun-processed).
      The first listed submission creates the public listing. AMO API key/secret
      come from `WEB_EXT_API_KEY`/`WEB_EXT_API_SECRET`.
- [ ] Chrome (CWS): create the store item by hand on the first release (the API
      cannot create the initial listing), then `bun run submit:chrome`
      (upload draft) or `bun run submit:chrome --publish` (upload + submit).
      OAuth creds are `CWS_CLIENT_ID`/`CWS_CLIENT_SECRET`/`CWS_REFRESH_TOKEN`/
      `CWS_EXTENSION_ID`.
- [ ] Fill each store listing from its kit in `extension/docs/`, add screenshots.
- [ ] Store-installed extensions auto-update once approved; manually loaded
      `about:debugging`/unpacked builds need the new release-bundle ZIP. Store
      review can lag days — never gate a daemon release on store approval.
- [ ] The human performs final publication after review; do not automate it.
