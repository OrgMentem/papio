# papio

Legitimate paper-acquisition broker: identifier in → validated, provenance-tracked
PDF out → handoff to [zotio](https://github.com/orgmentem/zotio) for all Zotero writes.

Papio resolves an explicitly requested work through open-access and licensed
sources first, then the institution's OpenURL resolver with a visible,
human-authenticated ordinary-Chrome handoff. It never bypasses access controls,
never touches credentials/MFA/CAPTCHA, and never crawls subscription content.

## Status

Phase 0 scaffold. Contracts are draft `0.x`; nothing here is stable yet.

- `protocol/` — draft JSON Schema contracts (work request, acquisition bundle,
  browser bridge). The shared fixture corpus in `testdata/protocol/` is
  validated by both the Go core and the extension TypeScript.
- `internal/protocol/` — Go structs with strict, fail-closed decoding.
- `extension/` — Manifest V3 extension workspace (TypeScript; Bun for package
  management/scripts only). Built out in Phase 2.

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
