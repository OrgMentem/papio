// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

/** DOI-shaped identifiers are deliberately conservative: the daemon remains the
 * authority, while the popup only needs a useful first candidate. */
export const DOI_PATTERN = /\b10\.\d{4,9}\/[^\s"'<>?#]+/;

export type PageKind = "pdf" | "doi" | "none";

export interface PageClassification {
  kind: PageKind;
  doi?: string;
}

function trimDOI(value: string): string {
  return value.trim().replace(/[.,;:!?\]}>'\"]+$/g, "");
}

/** Return the first DOI-shaped identifier in the supplied text. */
export function sniffDOI(value: string): string | undefined {
  const match = DOI_PATTERN.exec(value);
  if (match === null) return undefined;
  const doi = trimDOI(match[0]);
  return doi.length > 0 ? doi : undefined;
}

/** A URL whose path is a PDF, without treating a query or fragment as part of
 * the extension. */
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
    const url = new URL(value);
    if (!isPDFViewerURL(value)) return value;
    const source = url.searchParams.get("file");
    return source?.length ? source : value;
  } catch {
    return value;
  }
}

export function isPDFPage(url: string, contentType?: string): boolean {
  return isPDFURL(url) || isPDFViewerURL(url) || contentType?.split(";", 1)[0]?.trim().toLowerCase() === "application/pdf";
}

/** Classify the active page without touching the DOM. `text` is the bounded
 * page text/links probe result returned by popup's injected metadata reader. */
export function classifyPage(
  url: string,
  options: { contentType?: string; doi?: string; text?: string } = {},
): PageClassification {
  if (isPDFPage(url, options.contentType)) return { kind: "pdf", ...(options.doi ? { doi: options.doi } : {}) };
  const doi = options.doi ?? sniffDOI(url) ?? (options.text === undefined ? undefined : sniffDOI(options.text));
  return doi === undefined ? { kind: "none" } : { kind: "doi", doi };
}
