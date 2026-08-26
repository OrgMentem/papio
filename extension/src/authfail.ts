// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Detection of identity-provider failure pages on tracked handoff tabs.
// Pure URL + title heuristics only: no content-script injection and no page
// text ever leaves the browser, so the detector behaves identically on
// Chrome and Firefox and needs no extra permissions.

export type AuthFailureOutcome = "stale_sso" | "auth_error";

const STALE_MARKERS = ["stale", "expired", "assertion"];
const ERROR_MARKERS = ["error", "unauthorized", "denied"];

/** True for hosts that are identity-provider / WAYF infrastructure rather
 * than content providers. Deliberately conservative: a publisher page that
 * merely contains the word "error" must never classify. */
function isIdPHost(host: string, path: string): boolean {
  if (host === "login.openathens.net" || host.endsWith(".openathens.net")) return true;
  if (path.includes("/shibboleth.sso/")) return true;
  if (path.includes("/idp/profile/")) return true;
  return false;
}

/**
 * Classify a completed handoff-tab navigation as an SSO failure.
 * Returns undefined for anything that is not clearly an IdP failure page.
 */
export function detectAuthFailure(url: string, title: string | undefined): AuthFailureOutcome | undefined {
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return undefined;
  }
  const host = parsed.hostname.toLowerCase();
  const path = parsed.pathname.toLowerCase();
  // Elsevier's institutional OAuth handoff can end on this opaque session
  // resume route after the organization choice. The route is also the live
  // continuation endpoint, so URL shape alone is not a failure signal. The
  // terminal page's exact title is the stable distinction measured on
  // 2026-08-20, 2026-08-24, and 2026-08-26. Keep this outside the broad IdP
  // marker scan: id.elsevier.com also serves working authorization pages.
  if (
    host === "id.elsevier.com" &&
    /^\/as\/[^/]+\/resume\/as\/authorization\.ping$/i.test(path) &&
    title?.trim().toLowerCase() === "sorry"
  ) {
    return "auth_error";
  }
  if (!isIdPHost(host, path)) return undefined;
  const haystack = (path + " " + parsed.search + " " + (title ?? "")).toLowerCase();
  if (STALE_MARKERS.some((marker) => haystack.includes(marker))) return "stale_sso";
  if (ERROR_MARKERS.some((marker) => haystack.includes(marker))) return "auth_error";
  return undefined;
}
