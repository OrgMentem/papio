// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Extension page paths. Every extension page ships under dist/ beside the
// declared popup (see build.ts), and the manifest is the source of truth; see
// rule://extension-page-path-derived.

export const POPUP_PAGE_PATH = "dist/popup.html";
/** Derived, never hardcoded: extension pages ship beside the declared popup
 * (`dist/` in every manifest — Chrome, generated Firefox, and dev-unpacked),
 * so a root-relative "materialize.html" resolves to nothing and every
 * automatically-owned institutional tab lands on ERR_FILE_NOT_FOUND. Same
 * rule as the authorized page URLs derived in realDeps(); see popup.ts's
 * historyPagePath(). */
export const MATERIALIZE_PAGE_PATH = POPUP_PAGE_PATH.replace(
  /[^/]*$/,
  "materialize.html",
);
/** Same derivation rule, same reason: a bare "toast.html" resolves in no
 * deployment, and this surface's whole job is to appear when something already
 * went wrong — a broken page URL here would be silent. */
export const TOAST_PAGE_PATH = POPUP_PAGE_PATH.replace(/[^/]*$/, "toast.html");
/** Same derivation rule again: where Reopen sends the operator when the
 * worker slept and the closed papers' URLs went with it. */
export const HISTORY_PAGE_PATH = POPUP_PAGE_PATH.replace(/[^/]*$/, "history.html");
