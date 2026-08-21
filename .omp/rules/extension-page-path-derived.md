---
name: extension-page-path-derived
description: "Extension page paths must be derived from the manifest's declared popup, never written as a root-relative getURL literal — unit tests cannot catch this"
condition:
  - '[gG]etURL(\?\.)?\(\s*["''](?!dist/)'
scope:
  - "tool:edit(*.ts)"
  - "tool:write(*.ts)"
interruptMode: always
---

**Every extension page ships under `dist/`, so a page path must be DERIVED, never written root-relative.** All three manifests declare `dist/popup.html` / `dist/options.html` — the hand-authored `extension/manifest.json`, the generated `firefox/manifest.json`, and `dev-unpacked/manifest.json` (whose `dist` is a symlink to `../dist`). So `chrome.runtime.getURL("materialize.html")` resolves to a file that exists in **no** deployment, and the tab renders Chrome's `ERR_FILE_NOT_FOUND`.

Derive it:

```ts
const POPUP_PAGE_PATH = "dist/popup.html";
const MATERIALIZE_PAGE_PATH = POPUP_PAGE_PATH.replace(/popup\.html$/, "materialize.html");
```

`realDeps()` already derives inbox/history/options/page-bulk from the manifest's declared popup for this reason ("so the authorized URLs can never drift from the shipped page layout again"); `popup.ts`'s `historyPagePath()` reads `getManifest().action.default_popup`. `MATERIALIZE_PAGE_PATH` was added later as a bare literal and made *every* automatically-owned institutional tab a browser error page — the entry point of the whole automatic materialization path.

**Unit tests provably cannot catch this class:** the fake `runtimeGetURL` in the test deps resolves any string, so 13 assertions pinned the same wrong literal the source built. Any regression test must anchor the URL to the shipped manifest's declared page directory, not to a literal.

If this string is not an extension page path (a fixture URL, an asset outside `dist/`, a test double), say so and continue.
