<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-wordmark-dark.svg">
    <source media="(prefers-color-scheme: light)" srcset="docs/assets/logo-wordmark.svg">
    <img alt="papio" src="docs/assets/logo-wordmark.svg" width="200">
  </picture>
</p>

Papio is a local paper-acquisition broker: it searches scholarly works, creates
validated provenance-tracked PDF jobs, and hands ready artifacts to
[zotio](https://github.com/orgmentem/zotio) through a preview-and-confirmation
boundary. It uses ordinary Chrome for human-authenticated access and never
handles credentials, MFA, CAPTCHAs, or subscription crawling.

## Prerequisites

- macOS with Go-built binaries from a release (or `go build ./cmd/papio`).
- Poppler and Tesseract for PDF validation and the OCR text gate:
  `brew install poppler tesseract` (or disable OCR in the config — see the
  [configuration reference](docs/reference/config-reference.md)).
- Google Chrome or Firefox with the papio extension loaded for
  human-authenticated access — Chrome loads `extension/` unpacked, Firefox
  loads the generated `extension/firefox/` build via `about:debugging`
  (`papio init` prints the exact steps; skip with `papio init --skip-browser`
  for OA-only headless use).
- [zotio](https://github.com/orgmentem/zotio) on PATH (or `[zotio] executable`
  in the config) for Zotero import; optional — without it papio stops at
  validated bundles.

## Quick start

```sh
papio init
# `papio doctor` checks the whole chain, including the browser extension and zotio
papio doctor
papio search "appropriate reliance on AI" --limit 10
papio acquire 10.1371/journal.pone.0262026 --wait
papio status
papio actions list
papio jobs list
papio version
papio daemon status
papio daemon stop
```

## Sister project: zotio

Papio acquires validated PDFs into immutable bundles. [zotio](https://github.com/orgmentem/zotio)
is the trust-and-automation layer for Zotero that imports those bundles
preview-first. Papio works without zotio, stopping at validated bundles.

## Documentation

- [User guide](docs/guide/user-guide.md) — research workflow, browser pass, reports,
  watches, and identity reviews.
- [MCP agent guide](docs/guide/agent-skill.md) — every `papio_*` tool, safety
  semantics, and CLI equivalence.
- [Configuration reference](docs/reference/config-reference.md) — every TOML key,
  default, constraint, and effect.
- [Troubleshooting](docs/guide/troubleshooting.md) — extension reload, daemon
  recovery, doctor output, and stable Zotio error classes.

License: MIT.
