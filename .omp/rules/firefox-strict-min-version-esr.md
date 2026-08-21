---
name: firefox-strict-min-version-esr
description: "Never bump Firefox strict_min_version off 128.0 — 128 is the ESR that papio's institutional users run, and tab-group handoff already degrades at runtime"
condition:
  - 'strict_min_version'
scope:
  - "tool:edit(*.ts)"
  - "tool:write(*.ts)"
  - "tool:edit(*.json)"
  - "tool:write(*.json)"
interruptMode: always
---

**`strict_min_version` stays `128.0` on purpose.** Firefox 128 is the ESR that papio's institutional and library users run — the whole audience. The source of truth is `extension/build.ts` (around the `strict_min_version: "128.0"` literal); `extension/firefox/manifest.json` is generated from it, so never edit that copy (`rule://generated-files-not-hand-edited`).

**Do not bump it to silence `web-ext lint`'s `INCOMPATIBLE_API` warnings for `tabs.group` / `tabGroups.*`.** Those warnings are expected: tab-group handoff is dep-driven and runtime-detected (`chrome.tabGroups` / `chrome.tabs.group`), never gated on Chrome, and on Firefox < 139 `handoffSurface()` degrades tab-group mode to the work window at runtime. Static analysis cannot see that guard. Lint still exits 0 (0 errors).

So bumping buys nothing and costs every ESR user their tab-group handoff. If you genuinely need a newer floor, that is a product decision about dropping ESR support — raise it, do not land it as a lint fix.
