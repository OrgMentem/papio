---
name: generated-files-not-hand-edited
description: "docs/reference/commands.md and extension/firefox/manifest.json are generated — edit the source, not the artifact"
condition: ['.*']
scope:
  - "tool:edit(docs/reference/commands.md)"
  - "tool:write(docs/reference/commands.md)"
  - "tool:edit(extension/firefox/manifest.json)"
  - "tool:write(extension/firefox/manifest.json)"
interruptMode: always
---

**This file is generated. Your edit will be overwritten by the next build, and the real defect will still be there.** Edit the source instead:

| artifact | source | regenerate with |
| --- | --- | --- |
| `docs/reference/commands.md` | the cobra command tree in `internal/cli` | `go run ./cmd/docs-gen` (`make docs-gen`); it carries a `DO NOT EDIT` header |
| `extension/firefox/manifest.json` | `extension/manifest.json` (single source of truth) + `extension/build.ts` | `bun run build` (emits Chrome `dist/` **and** Firefox `firefox/`) |

Not generated despite living beside generated files — edit these directly: **`docs/reference/config-reference.md`** is hand-authored (keep it in sync with the config defaults by hand) and so is `docs/reference/mcp-tools.md`. `llms.txt` / `llms-full.txt` are generated into the built site at deploy time (`cmd/docs-gen -llms-out`) and are not tracked files at all.
