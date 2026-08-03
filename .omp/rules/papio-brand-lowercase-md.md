---
name: papio-brand-lowercase-md
description: "User docs use lowercase papio/zotio — italicize *papio* (never zotio) in markdown prose"
condition: ["\\bPapio\\b", "\\bZotio\\b"]
scope: ["tool:write(*.md)", "tool:edit(*.md)"]
---

Brand names are **always lowercase**: `papio` and `zotio`, even at sentence start (e.g. "*papio* runs on macOS…").

In markdown docs, prose mentions of papio are italicized: `*papio*` (possessive: `*papio*'s`). **zotio is never italicized** — plain lowercase `zotio`.

Do NOT capitalize as `Papio`/`Zotio` in any user-facing doc. Leave untouched: inline/fenced code spans (`` `papio acquire` ``), `PAPIO_*` env vars, Go identifiers (`cfg.Zotio`), URLs/paths, image alt text, and quoted literal UI copy — markup there is unsafe or the casing is code-accurate. `Zotero` (the product) keeps its capital.