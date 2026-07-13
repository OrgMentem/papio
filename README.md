# papio

Legitimate paper-acquisition broker: identifier in → validated, provenance-tracked
PDF out → handoff to [zotio](https://github.com/orgmentem/zotio) for all Zotero writes.

Papio resolves an explicitly requested work through open-access and licensed
sources first, then the institution's OpenURL resolver with a visible,
human-authenticated ordinary-Chrome handoff. It never bypasses access controls,
never touches credentials/MFA/CAPTCHA, and never crawls subscription content.

## Status

Phase 1 acquisition core is implemented. Contracts remain draft `0.x`.

- Durable SQLite jobs, leases, cancellation/retry, source budgets, redacted
  events, quarantine, and immutable SHA-256 artifact storage.
- arXiv, Europe PMC, Unpaywall, OpenAlex, CORE, and explicitly configured
  Crossref TDM resolvers with deterministic ranking and bounded secure HTTP.
- Isolated structural PDF parsing, Poppler text/page cross-checks, deterministic
  identity matching, and bounded Tesseract OCR fallback.
- Strict Unix-socket daemon IPC/autostart, structured CLI output, readiness
  diagnostics, and idempotent acquisition-bundle export.
- `extension/` remains the Phase 2 ordinary-Chrome institutional handoff.

## Use

```sh
papio config init --access-mode maximal --email you@example.org
papio doctor
papio acquire 10.1371/journal.pone.0262026 --wait
papio jobs list --json
papio artifacts get <job-id>
papio bundle export <job-id> --output ./export
papio daemon stop
```

`config init` deliberately requires an explicit access mode. Credentialed
sources are disabled until configured and enabled in
`~/.config/papio/config.toml`. The daemon autostarts on the first client command;
`papio daemon` runs it in the foreground.

## Fixed identifiers

| Purpose | Value |
|---|---|
| Go module / binary | `papio` |
| Config directory | `~/.config/papio/` |
| Native-host executable | `papio-native-host` (basename dispatch on the `papio` binary) |
| Native-host manifest name | `com.orgmentem.papio` |
| Extension product name | Papio |

The Chrome extension ID is assigned by the signing key, generated and preserved
outside this repository; it is pinned in the native-host manifest at install time.

## Boundaries

- Zotio alone mutates Zotero (`zotio attachments add`, `zotio import …`).
- Inscribi consumes selected PDFs plus Zotero CSL/BibTeX exports; it never sees
  broker internals.
- The instsci fork is a read-only behavioral reference; none of its
  architecture, publisher profiles, or identity carries over.

License: MIT.
