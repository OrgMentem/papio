// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { isAuthenticationURL } from "./keepalive";
import {
  EFFECT_PERMIT_FEATURE,
  PDF_GRAB_REFUSAL_REASONS,
  type PdfGrabRefusalReason,
} from "./protocol";
import type { DaemonConnectionStatus, PendingDeliveryStatus } from "./state";
/** DOI-shaped identifiers are deliberately conservative: the daemon remains the
 * authority, while the popup only needs a useful first candidate. */
export const DOI_PATTERN = /\b10\.\d{4,9}\/[^\s"'<>?#]+/;

export type PageKind = "pdf" | "doi" | "none";

export interface PageClassification {
  kind: PageKind;
  doi?: string;
}

export interface PageMetaProbe {
  name: string;
  content: string;
}

export interface PageDOIProbe {
  /** Meta tags in document order. */
  meta?: readonly PageMetaProbe[];
  canonical?: string;
  ogURL?: string;
  href?: string;
  /** Anchor hrefs, in document order. */
  anchorHrefs?: readonly string[];
  bodyText?: string;
}

function trimDOI(value: string): string {
  return value.trim().replace(/[.,;:!?\]}>'"]+$/g, "");
}

/** Return the first DOI-shaped identifier in the supplied text. */
export function sniffDOI(value: string): string | undefined {
  let decoded = value;
  try {
    decoded = decodeURIComponent(value);
  } catch {
    // Keep raw text when an untrusted URL contains malformed escapes.
  }
  const match = DOI_PATTERN.exec(decoded);
  if (match === null) return undefined;
  const doi = trimDOI(match[0]);
  return doi.length > 0 ? doi : undefined;
}

/**
 * The stable-ID namespace is a documented JSTOR mapping, not a provider
 * adapter: a /stable/<id> landing names the Crossref DOI 10.2307/<id>.
 */
const STABLE_ID_DOI_PREFIXES: Readonly<Record<string, string>> = {
  "jstor.org": "10.2307",
};

/** Derive the DOI for a host-specific stable identifier when documented. */
export function deriveStablePageDOI(value: string): string | undefined {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return undefined;
  }
  const hostname = url.hostname.toLowerCase();
  const prefix = Object.entries(STABLE_ID_DOI_PREFIXES).find(
    ([suffix]) => hostname === suffix || hostname.endsWith(`.${suffix}`),
  )?.[1];
  if (prefix === undefined) return undefined;
  const stable = url.pathname.match(/^\/stable\/([^/]+)\/?$/i)?.[1];
  if (stable === undefined || stable.length === 0) return undefined;
  let decoded = stable;
  try {
    decoded = decodeURIComponent(stable);
  } catch {
    // Keep the path component if an untrusted page contains malformed escapes.
  }
  return decoded.length > 0 ? `${prefix}/${decoded}` : undefined;
}

/** Extract a DOI from the ordered meta-tag layer. */
export function extractMetaDOI(meta: readonly PageMetaProbe[] = []): string | undefined {
  const byName = new Map<string, string[]>();
  for (const tag of meta) {
    const name = tag.name.trim().toLowerCase();
    const content = tag.content.trim();
    if (name.length === 0 || content.length === 0) continue;
    const values = byName.get(name);
    if (values === undefined) byName.set(name, [content]);
    else values.push(content);
  }
  // Preserve the priority of specific metadata standards over broad fallback
  // tags. publication_doi remains for older SAGE/Atypon pages.
  for (const name of [
    "citation_doi",
    "dc.identifier",
    "prism.doi",
    "publication_doi",
    "citation_pdf_url",
  ]) {
    for (const value of byName.get(name) ?? []) {
      const doi = sniffDOI(value);
      if (doi !== undefined) return doi;
    }
  }
  return undefined;
}

/**
 * Extract a page DOI in the same first-hit order used by popup's injected
 * metadata probe. This function is DOM-free so classifier behavior can be
 * pinned by unit tests and reused by non-DOM callers.
 */
export function extractPageDOI(probe: PageDOIProbe): string | undefined {
  const fromMeta = extractMetaDOI(probe.meta);
  if (fromMeta !== undefined) return fromMeta;

  for (const value of [probe.canonical, probe.ogURL, probe.href]) {
    if (typeof value !== "string") continue;
    const doi = sniffDOI(value);
    if (doi !== undefined) return doi;
  }

  for (const href of probe.anchorHrefs ?? []) {
    try {
      const anchorURL = new URL(href, probe.href);
      const hostname = anchorURL.hostname.toLowerCase();
      if (hostname !== "doi.org" && !hostname.endsWith(".doi.org")) continue;
    } catch {
      continue;
    }
    const doi = sniffDOI(href);
    if (doi !== undefined) return doi;
  }

  if (typeof probe.bodyText === "string") {
    const doi = sniffDOI(probe.bodyText.slice(0, 200_000));
    if (doi !== undefined) return doi;
  }

  return typeof probe.href === "string" ? deriveStablePageDOI(probe.href) : undefined;
}

/** A direct PDF response whose route does not use a `.pdf` suffix. Keep these
 * exact and host-bound; HTML viewers that need endpoint conversion belong in
 * their declarative adapter's viewerRoutes contract instead. */
function isKnownDirectPDFRoute(url: URL): boolean {
  if (url.protocol !== "https:") return false;
  if (url.hostname.toLowerCase() !== "www.cell.com") return false;
  if (url.pathname.toLowerCase() !== "/action/showpdf") return false;
  if (url.hash !== "") return false;
  const params = [...url.searchParams];
  if (params.length !== 1) return false;
  const only = params[0];
  if (only === undefined || only[0] !== "pii") return false;
  const pii = only[1];
  return /^[A-Za-z0-9()_-]{5,128}$/.test(pii);
}

/** A URL whose path is a PDF, or an exact host-bound direct-PDF route,
 * without treating a query or fragment as a filename extension. */
export function isPDFURL(value: string): boolean {
  try {
    const url = new URL(value);
    return url.pathname.toLowerCase().endsWith(".pdf") || isKnownDirectPDFRoute(url);
  } catch {
    return false;
  }
}

/** Chrome's built-in viewer and the Firefox pdf.js viewer both expose a PDF
 * document through an extension/resource URL rather than a .pdf pathname. */
export function isPDFViewerURL(value: string): boolean {
  try {
    const url = new URL(value);
    if (url.hostname === "mhjfbmdgcfjbbpaeojofohoefgiehjai") return true;
    const viewerPath = /(?:^|\/)viewer\.html$/i.test(url.pathname);
    return viewerPath &&
      (url.protocol === "moz-extension:" || url.protocol === "resource:" || url.protocol === "chrome-extension:") &&
      url.searchParams.has("file");
  } catch {
    return false;
  }
}

/** Extract the underlying file URL from a browser PDF viewer tab when the
 * viewer has wrapped it in a `file` query parameter. */
export function pdfSourceURL(value: string): string {
  try {
    if (!isPDFViewerURL(value)) return value;
    const source = new URL(value).searchParams.get("file");
    return source?.length ? source : value;
  } catch {
    return value;
  }
}

export function isPDFPage(url: string, contentType?: string): boolean {
  return isPDFURL(url) || isPDFViewerURL(url) || contentType?.split(";", 1)[0]?.trim().toLowerCase() === "application/pdf";
}

/** Classify the active page without touching the DOM. `text` is the bounded
 * page text probe returned by popup's injected metadata reader. */
export function classifyPage(
  url: string,
  options: { contentType?: string; doi?: string; text?: string } = {},
): PageClassification {
  if (isPDFPage(url, options.contentType)) return { kind: "pdf", ...(options.doi ? { doi: options.doi } : {}) };
  const doi =
    options.doi ??
    extractPageDOI({
      href: url,
      ...(options.text !== undefined ? { bodyText: options.text } : {}),
    });
  return doi === undefined ? { kind: "none" } : { kind: "doi", doi };
}


/** Return a privacy-safe HTTPS hostname for a completed PDF tab. Paths,
 * queries, fragments, browser-internal URLs, and authentication/IdP pages are
 * intentionally discarded rather than sent to the daemon. */
export function sanitizePageHost(value: string): string | undefined {
  let url: URL;
  try {
    url = new URL(pdfSourceURL(value));
  } catch {
    return undefined;
  }
  if (url.protocol !== "https:" || isAuthenticationURL(url.href)) return undefined;
  const host = url.hostname.toLowerCase();
  if (
    host.length < 3 ||
    host.length > 128 ||
    !/^[a-z0-9.-]+$/.test(host) ||
    host.includes("..") ||
    host.startsWith(".") ||
    host.endsWith(".") ||
    host.split(".").length < 2
  ) {
    return undefined;
  }
  return host;
}

/** The daemon feature that advertises daemon-side PDF grabbing. Declared here,
 * beside the copy that explains its absence, so the popup's pre-click decision
 * and the bridge's send-time refusal cannot disagree about the name. */
export const PDF_GRAB_FEATURE = "pdf_grab_v1";

/** Shown when papio refused a grab for a reason the researcher cannot act on,
 * and whenever an older daemon classified nothing at all. */
export const PDF_GRAB_GENERIC_REFUSAL = "papio couldn't save this PDF — try again.";

/** The single translation from the daemon's closed refusal vocabulary to the
 * words a researcher reads. Every entry names either the one command that
 * fixes it or the one fact that ends the attempt; none names holdership,
 * effect permits, sessions, or feature negotiation, because none of those is
 * something the person holding the mouse can do anything about. */
export const PDF_GRAB_REFUSAL_COPY: Readonly<Record<PdfGrabRefusalReason, string>> = {
  no_session: PDF_GRAB_GENERIC_REFUSAL,
  extension_outdated: "Reload the papio extension to finish updating.",
  daemon_unsupported: "papio needs an update — run papio doctor.",
  busy: "papio is busy with another download — try again in a moment.",
  not_configured: "papio isn't set up to save PDFs yet — run papio doctor.",
  adoption_unhealthy: "papio can't reach its downloads folder — run papio doctor.",
  tab_unusable: "This tab isn't a PDF papio can save.",
  internal: PDF_GRAB_GENERIC_REFUSAL,
};

/** Daemon-internal vocabulary. `detail` is diagnostic prose written for someone
 * reading a log, so it is only ever a fallback, and never one when it leaks the
 * arbitration machinery the researcher is not party to. */
const GRAB_DETAIL_INTERNAL_VOCABULARY =
  /holder|holdership|permit\w*|negotiat\w*|session slot|effect lane/i;

function isPdfGrabRefusalReason(value: unknown): value is PdfGrabRefusalReason {
  return (
    typeof value === "string" &&
    (PDF_GRAB_REFUSAL_REASONS as readonly string[]).includes(value)
  );
}

/** Translate one pdf_grab_result refusal into researcher-facing copy.
 *
 * An absent or unrecognized `reason` means an older daemon classified nothing,
 * so the daemon's own `detail` is the best available text — unless it carries
 * internal vocabulary, in which case there is nothing worth showing and the
 * generic copy wins. */
export function pdfGrabRefusalText(reason: unknown, detail?: string): string {
  if (isPdfGrabRefusalReason(reason)) return PDF_GRAB_REFUSAL_COPY[reason];
  const text = typeof detail === "string" ? detail.trim() : "";
  if (text === "" || GRAB_DETAIL_INTERNAL_VOCABULARY.test(text))
    return PDF_GRAB_GENERIC_REFUSAL;
  return text;
}

/** Everything the extension already knows about a Send-PDF click before it
 * happens. Deliberately store-shaped: the popup must not need a round trip to
 * the worker to decide whether to offer a control.
 *
 * There is no `role` field, and that is the fix rather than an omission. The
 * daemon's session role reaches the popup as `connectionStatus`:
 * `session_elsewhere` IS the acknowledged-but-pending role, and a pending
 * session routes its own grab, so it is exactly as able to send a PDF as the
 * holder. Holdership decides who receives daemon-initiated work; it has no
 * say over work the researcher initiates here. */
export interface SendPdfFacts {
  /** Last hello_ack outcome for the daemon session. Anything other than an
   * acknowledged status means the extension does not yet know the daemon's
   * real capabilities and must not claim one is missing. */
  connectionStatus: DaemonConnectionStatus | undefined;
  /** Features the daemon acknowledged. Empty until an ack arrives. */
  daemonFeatures: readonly string[] | undefined;
  /** True when identifying this PDF needs the daemon's grab lane: no live job
   * owns the page and the page carries no DOI of its own. A page with either
   * travels the ordinary acquisition route, which needs none of this. */
  needsGrab: boolean;
  /** This page's own in-flight delivery, if any. */
  deliveryStatus: PendingDeliveryStatus | undefined;
}

/** Whether Send PDF can do anything if clicked right now.
 *
 * `refused` is only ever returned for something the extension KNOWS, which is
 * why an unreachable daemon stays `ready`: the click reconnects, and its
 * established error path is more honest than a disabled button asserting a
 * capability gap nobody has confirmed. */
export type SendPdfState =
  | { kind: "ready" }
  | { kind: "in_flight" }
  | { kind: "refused"; reason: PdfGrabRefusalReason; message: string };

function refusedSendPdf(reason: PdfGrabRefusalReason): SendPdfState {
  return { kind: "refused", reason, message: PDF_GRAB_REFUSAL_COPY[reason] };
}

export function sendPdfState(facts: SendPdfFacts): SendPdfState {
  if (facts.deliveryStatus === "sending" || facts.deliveryStatus === "downloaded")
    return { kind: "in_flight" };
  // The daemon refuses every frame from a version it will not talk to, so this
  // one is known-dead regardless of which route the click would have taken.
  if (facts.connectionStatus === "extension_outdated")
    return refusedSendPdf("extension_outdated");
  if (!facts.needsGrab) return { kind: "ready" };
  const acknowledged =
    facts.connectionStatus === "connected" ||
    facts.connectionStatus === "session_elsewhere" ||
    facts.connectionStatus === "daemon_outdated";
  if (!acknowledged) return { kind: "ready" };
  const features = facts.daemonFeatures ?? [];
  if (
    !features.includes(PDF_GRAB_FEATURE) ||
    !features.includes(EFFECT_PERMIT_FEATURE)
  )
    return refusedSendPdf("daemon_unsupported");
  return { kind: "ready" };
}