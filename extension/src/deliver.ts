// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { isAuthenticationURL } from "./keepalive";
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

/** A URL whose path is a PDF, without treating a query or fragment as part
 * of the extension. */
export function isPDFURL(value: string): boolean {
  try {
    return new URL(value).pathname.toLowerCase().endsWith(".pdf");
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