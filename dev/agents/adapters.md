# Adapters and fixtures

Read this reference before work in this area. Paths in code spans and commands are repository-root-relative unless stated otherwise.

### Adapters & fixtures
- **Reconnecting a newer adapter can retry existing jobs in the destination
  daemon.** `internal/app/handoff_repair.go:RepairAdapterUpgrade` repairs eligible
  provider parks when the live browser advertises a newer adapter revision. This
  also runs when restoring the normal native host after an isolated experiment:
  restoring the manifest does not restore the browser's old adapter versions.
  Snapshot normal job states before reconnecting and inspect
  `adapter_upgrade_repair` events afterward. Preserve those transitions; do not
  rewrite jobs to make the before/after states match or call them isolated probes.
- The procedure for building or repairing one lives in
  `.agents/skills/papio-provider-adapter/SKILL.md` — evidence order, the
  wrapper-versus-file test, route-boundary and localization rules, and the
  verification ladder. The footguns below are the standing facts it assumes.
- An adapter **cannot** enter `extension/src/adapters/types.ts` without captured fixtures —
  and `success.html` alone is only enough for an adapter whose rules are all `article`.
  `expectFixtureBackedRules` (`extension/test/adapters.test.ts`) derives the required
  scenario set from each rule's `kind` via `fixtureScenarioForRule`, so `login` owes
  `login-return.html`, `terms` owes `terms.html`, `no_entitlement` owes
  `no-entitlement.html`, and `wrong_work_check` owes `wrong-work.html`. Capture the
  authenticated page with the diagnostic page, locate the daemon-stored sanitized capture
  with `papio adapter captures` (it prints each capture's path; there is no retrieve
  subcommand — copy the file yourself), and commit it before adding the spec and the
  `adapters.test.ts` cases. Do **not** hand-guess selectors.
- **There is ONE provider-rule classifier, and the guard that keeps it that way bans
  its predecessor's name.** `extension/src/plan.ts:planExecution` owns provider-rule
  classification and its readiness loops; `background.ts` injects it. The second copy,
  `interpret` in `extension/src/adapters/types.ts`, is deleted (`bce6777`) — it was what
  `adapter-try` and the fixture tests used to exercise, so a classifier change once meant
  editing two files and `deferUntilDeadline` landed in `interpret` alone while 1,442 tests
  passed. A comment claiming two files mirror each other was never enforcement, which is
  why the copy is gone rather than re-synchronised.
  The readiness semantics that survived the consolidation: article readiness holds through
  the full budget, non-article readiness gets a 50 ms settle window, and deferred rules are
  excluded from every early classification. A change here still needs **asynchronous**
  tests where those cases diverge — another rule wakes early, an article marker appears
  then disappears, and an article marker remains through the deadline.
  `test/plan.test.ts`'s "planExecution body has no module-scope runtime dependencies"
  is the trap: a raw text scan from `planExecution` to end of file
  forbidding `adapters`, `interpret`, `chrome`, `globalThis`, `window`, and
  `resolveDownloadURL` **by word, comments included**. So naming the deleted twin in a
  comment there fails the build, and reintroducing a second classifier fails it too.
  `assessDrivenPage` (`extension/src/background.ts`) is a **different** classifier with
  a different input contract — it produces `load_failure` before `planExecution` runs — and
  is deliberately untouched by any of this. Do not merge it in.
- **`sanitizeFixture` strips URL query strings** (privacy). So classify selectors must key on
  **stable id/path/data-attrs, not `?...` params** (e.g. SAGE keys on `section.format--pdf_epub`,
  not `[href*='download=true']`). `method: "href"` reads the **live** anchor href (with query)
  at download time, so runtime downloads still get the full URL.
- **`fetch()` from the page 403s on many provider PDF endpoints** (bot-gated) but
  `chrome.downloads.download` (a browser-level request, like a real click) succeeds — a
  `fetch` 403 is **not** conclusive when picking a download method. Verify live.
- **A signed PDF viewer's bytes leave Chrome only through its first response.**
  ScienceDirect (`pdf.sciencedirectassets.com`) returns HTML to any second request,
  including `chrome.downloads.download` of the exact live URL (measured 2026-09-20 and
  2026-09-23). `chrome.pageCapture` is no way out: blink's `FrameSerializer` writes the
  PDF plugin document as its `<embed>` markup, so the MHTML never holds the PDF. What
  works is `src/viewer-download-rule.ts`: DNR session rules, scoped by `tabIds` to armed
  handoff tabs and by a `content-type: application/pdf` response condition (Chrome 128+),
  set `Content-Disposition: attachment` so the first response downloads. DNR compiles
  `regexFilter` under a 2 KB RE2 budget: the credential-parameter alternation does not
  fit, which is why each parameter is its own `urlFilter` rule. Chrome's DownloadItem has
  no tab id, so the download binds to its tab through the recorded `onBeforeNavigate`
  URL, not host correlation, which cannot separate two ScienceDirect jobs.

