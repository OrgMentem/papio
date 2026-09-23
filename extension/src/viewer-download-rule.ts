/**
 * Chrome session rules that make a signed PDF viewer response download
 * instead of render, only in the handoff tabs papio arms for a job.
 *
 * A signed viewer URL is not reusable: ScienceDirect answered a second request
 * with HTML (2026-09-20, and again through Chrome downloads on 2026-09-23), and
 * Silverchair answered HTTP 400. The PDF exists only in the one response the
 * browser already received. `Content-Disposition: attachment` on THAT response
 * turns the navigation into a download of the same bytes, with no second
 * request, and Chrome's own download then runs through papio's filename
 * steering and the daemon's adoption and identity checks.
 *
 * `chrome.pageCapture` cannot do this: blink's FrameSerializer writes a PDF
 * plugin document as its `<embed>` markup, so MHTML never holds the PDF.
 *
 * The URL conditions follow `requiresNativeViewerDownload` without its
 * credential-length bound. DNR compiles `regexFilter` under a 2 KB RE2 budget,
 * and the credential-parameter alternation exceeds it with or without the
 * counted repetition, so each parameter is its own `urlFilter` rule. The PDF
 * content-type condition keeps a login or ticket redirect from matching, and
 * `tabIds` keeps every rule inside papio's own handoff tabs. That condition
 * needs Chrome 128; an older Chrome rejects the rules and papio keeps its
 * previous behaviour.
 */
import { CREDENTIAL_PARAMS } from "./deliver";

export const SIGNED_VIEWER_HOST = "pdf.sciencedirectassets.com";

const FIRST_RULE_ID = 7301;
const PARAMS = [...CREDENTIAL_PARAMS];

/** Every session rule id this module owns; removal always names all of them. */
export const VIEWER_DOWNLOAD_RULE_IDS: readonly number[] = Array.from(
  { length: PARAMS.length + 1 },
  (_, index) => FIRST_RULE_ID + index,
);

export interface ViewerDownloadRule {
  id: number;
  priority: number;
  action: {
    type: "modifyHeaders";
    responseHeaders: { header: string; operation: "set"; value: string }[];
  };
  condition: {
    tabIds: number[];
    resourceTypes: ("main_frame" | "sub_frame")[];
    responseHeaders: { header: string; values: string[] }[];
    requestDomains?: string[];
    urlFilter?: string;
  };
}

/** The rules for `tabIds`. An empty list yields no rules: DNR rejects an empty
 * `tabIds`, and a rule without one would reach every tab in the browser. */
export function viewerDownloadRules(tabIds: readonly number[]): ViewerDownloadRule[] {
  if (tabIds.length === 0) return [];
  const action: ViewerDownloadRule["action"] = {
    type: "modifyHeaders",
    responseHeaders: [{ header: "content-disposition", operation: "set", value: 'attachment; filename="paper.pdf"' }],
  };
  const condition: ViewerDownloadRule["condition"] = {
    tabIds: [...tabIds],
    resourceTypes: ["main_frame", "sub_frame"],
    responseHeaders: [{ header: "content-type", values: ["application/pdf*", "application/x-pdf*"] }],
  };
  return [
    { id: FIRST_RULE_ID, priority: 1, action, condition: { ...condition, requestDomains: [SIGNED_VIEWER_HOST] } },
    // `^` is DNR's separator class: it matches `?` or `&`, never `_`, so
    // `^token=` does not also match `access_token=`.
    ...PARAMS.map((param, index) => ({
      id: FIRST_RULE_ID + 1 + index, priority: 1, action, condition: { ...condition, urlFilter: `^${param}=` },
    })),
  ];
}

/** True when a download's URL has the shape these rules act on, so papio can
 * bind the download to the tab whose navigation produced it. */
export function matchesViewerDownloadRule(value: string): boolean {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return false;
  }
  const host = url.hostname.toLowerCase();
  if (host === SIGNED_VIEWER_HOST || host.endsWith(`.${SIGNED_VIEWER_HOST}`)) return true;
  for (const name of url.searchParams.keys()) {
    if (CREDENTIAL_PARAMS.has(name.toLowerCase())) return true;
  }
  return false;
}
