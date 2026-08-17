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

/**
 * A DOI read out of a URL is structurally different from a DOI read out of
 * text, and conflating them was a live defect: a URL has delimiters, so a
 * text scan absorbs whatever follows the identifier. `?doi=10.1234/paper&
 * token=SECRET123` scanned as text yields `10.1234/paper&token=SECRET123`,
 * which `work.NormalizeDOI` accepts (`doiCoreRE`'s `\S` matches `&` and `=`),
 * so a session token became the job's persisted identity. URL-origin
 * candidates therefore go through this function, never through `sniffDOI`.
 *
 * Rules, in the order they matter:
 *   1. Structure decides where an identifier ends — `URL` parsing and
 *      `searchParams`, never a regex over the serialized URL.
 *   2. A URL that names a document other than its DOI declines. A wrong DOI
 *      that resolves to a real *other* paper is the cardinal failure; returning
 *      nothing is not.
 *   3. Only a *declared* URL suffix may be stripped, and DOI text is never
 *      normalized — a repeated slash (`10.48612//x`) names a different work
 *      from a single one, so slash runs are preserved exactly (AGENTS.md).
 *
 * The recognized structure is a DOI-shaped **path segment**, not a per-host
 * route table. Host-keying was considered and rejected: it would decline
 * Springer's `/content/pdf/{doi}.pdf`, IET's `/content/{doi}`, and every
 * publisher nobody has enumerated yet, while adding no safety — the safety here
 * comes from strict validation plus the two reject lists below, none of which
 * depends on knowing the host. What made the old scan unsafe was reading the
 * *query and fragment* as if they were path, and that is closed structurally.
 */
// `(?![\s\S])` rather than `$`: JS `$` matches before a FINAL line terminator, so
// `10.1234/paper$` accepts `10.1234/paper\n` — reachable from a path carrying an
// encoded `%0A`, which would then be stored as the identifier.
const DOI_STRICT_RE = /^10\.\d{4,9}\/[^\s?#&=]+(?![\s\S])/;

/**
 * Path segments that, when they appear *after* the DOI, mean the URL names a
 * different document than the DOI it contains. This list is why the obvious
 * "strip the trailing junk" fix is unsafe: ACM publishes supplements in the
 * ARTICLE's DOI namespace (`/doi/suppl/<article DOI>/suppl_file/<file>`), so
 * trimming the tail yields the article's real DOI and files an appendix as the
 * paper it supplements. These must decline, not be cleaned up.
 *
 * The check runs across the whole path, before any candidate is read, so a
 * supplement declines even when its file segment is named something else and even
 * when the URL also carries a `?doi=<article>` parameter.
 *
 * `abs`, `full`, `abstract`, `epdf`, `pdf`, `pdfdirect` are deliberately NOT here:
 * they name a *view of the article*, whichever side of the DOI they fall on, so
 * `TRAILING_ROUTE_SEGMENTS` strips them instead of declining. Ordering is what
 * makes that safe — see its comment.
 */
const NON_ARTICLE_URL_SEGMENTS: ReadonlySet<string> = new Set([
  "suppl",
  "suppl_file",
  "supplementary",
  "supplemental",
  "media",
  // Springer supplementary files: `…/esm/art:<doi>/MediaObjects/<file>.pdf`.
  "mediaobjects",
  "esm",
  "figure",
  "figures",
  "table",
  "tables",
  "references",
  "citations",
  "citedby",
  "cited-by",
  "metrics",
  "permissions",
]);

/**
 * Route words that can trail a DOI in a publisher's article route:
 * Emerald's `/content/doi/<doi>/full/pdf`, SAGE's `/doi/pdf/<doi>/full`. They
 * name a *view of the article*, so the article's DOI is exactly what the URL
 * means and they are stripped rather than rejected.
 *
 * Stripping is only safe because `NON_ARTICLE_URL_SEGMENTS` is rejected first.
 * That ordering is the whole design: trimming a tail is dangerous precisely when
 * the URL names a *different* document (a supplement under its article's DOI),
 * and those never reach here.
 */
const TRAILING_ROUTE_SEGMENTS: ReadonlySet<string> = new Set([
  "full",
  "abs",
  "abstract",
  "pdf",
  "epdf",
  "epub",
  "pdfdirect",
  "full-xml",
  "download",
  "summary",
  "html",
  "meta",
]);

function decodeURLPart(value: string): string {
  try {
    return decodeURIComponent(value);
  } catch {
    return value;
  }
}

/**
 * Validate a DOI candidate taken from URL structure. Nothing about the text is
 * altered beyond removing trailing route segments the caller's route declares;
 * in particular no slash run is ever collapsed.
 */
function qualifyURLDOI(candidate: string, routed = false): string | undefined {
  let value = candidate;
  if (routed) {
    // Peel view segments off the end. A DOI needs a registrant and at least one
    // suffix segment, so peeling stops before it could empty the suffix.
    for (;;) {
      const segments = value.split("/");
      const last = segments[segments.length - 1];
      if (segments.length <= 2 || last === undefined) break;
      if (!TRAILING_ROUTE_SEGMENTS.has(last.toLowerCase())) break;
      value = segments.slice(0, -1).join("/");
    }
    // Peeling stops before emptying the suffix, so a route word can survive as
    // the whole suffix. `/doi/pdf/10.1177/full` is a truncated route missing its
    // article id, not the DOI `10.1177/full`.
    const remaining = value.split("/");
    const tail = remaining[remaining.length - 1];
    if (remaining.length === 2 && tail !== undefined && TRAILING_ROUTE_SEGMENTS.has(tail.toLowerCase())) {
      return undefined;
    }
  }
  if (!DOI_STRICT_RE.test(value)) return undefined;
  // A view marker fused onto the last segment, as bioRxiv and medRxiv write it
  // (`10.1101/2024.06.04.594010v1.full`). It cannot be repaired by trimming: the
  // real DOI also drops the `v1`, and version-collapsing is forbidden in the
  // acquisition path (`internal/work` is version-preserving, AGENTS.md), so
  // trimming would silently name a different work. Declining turns a job created
  // for a DOI that does not exist into an honest no-answer.
  if (/\.(?:full|abstract|supplementary|full-text)$/i.test(value)) return undefined;
  for (const segment of value.split("/").slice(1)) {
    if (NON_ARTICLE_URL_SEGMENTS.has(segment.toLowerCase())) return undefined;
  }
  return value;
}

/**
 * Extract a DOI from a URL's structure, or decline.
 *
 * Three independent structures can name a work: a wrapped inner URL, a declared
 * `doi` parameter, and the path. They are gathered rather than raced, because
 * whichever is consulted first would otherwise silently override the others —
 * a supplement path with an `?doi=<article>` parameter returned the article
 * while addressing the appendix, which is the cardinal failure. Every candidate
 * found must name the same work, and the route is rejected before any of them
 * is trusted.
 *
 * `base` resolves a relative value — a page's `<link rel=canonical href="/…">`
 * is legitimate and dropping it lost identifiers the previous text scan found.
 */
export function doiFromURL(value: string, base?: string, depth = 0): string | undefined {
  let url: URL;
  try {
    url = new URL(pdfSourceURL(value), base);
  } catch {
    return undefined;
  }

  const host = url.hostname.toLowerCase();
  const path = decodeURLPart(url.pathname);
  const isDOIResolver = host === "doi.org" || host === "dx.doi.org" || host.endsWith(".doi.org");

  // A non-article route disqualifies the whole URL, before any candidate is
  // read: the URL names a supplement, a cited-by list or a media file, and no
  // parameter elsewhere in it can make that the article. On a DOI resolver the
  // path is the identifier rather than a route, so these words would be data.
  if (!isDOIResolver) {
    for (const segment of path.split("/")) {
      if (NON_ARTICLE_URL_SEGMENTS.has(segment.toLowerCase())) return undefined;
    }
  }

  const candidates: string[] = [];

  // A library proxy or link resolver wraps the real URL in a parameter. Recurse
  // once so the inner URL is parsed by these same rules rather than scanned.
  if (depth === 0) {
    for (const key of ["url", "qurl", "target"]) {
      const inner = url.searchParams.get(key);
      if (inner === null || !/^https?:\/\//i.test(inner)) continue;
      const nested = doiFromURL(inner, undefined, depth + 1);
      if (nested !== undefined) candidates.push(nested);
    }
  }

  // An exact `doi` parameter is the publisher's own declaration and is bounded
  // by the query grammar, so a neighbouring token cannot be absorbed. It is a
  // declared value rather than a file path, so no suffix is stripped from it.
  for (const [key, raw] of url.searchParams) {
    if (key.toLowerCase() !== "doi") continue;
    const doi = qualifyURLDOI(decodeURLPart(raw.trim()));
    if (doi !== undefined) candidates.push(doi);
  }

  if (isDOIResolver) {
    // The whole path is the identifier by definition, so there is no route and
    // no external suffix: `10.1234/article.pdf` is a DOI that ends in `.pdf`,
    // not a PDF of `10.1234/article`.
    const resolved = qualifyURLDOI(path.replace(/^\//, ""));
    if (resolved !== undefined) candidates.push(resolved);
  } else {
    const start = /\/(10\.\d{4,9}\/.+)$/.exec(path);
    // Here the DOI is a file path, so one declared external suffix may be removed.
    const fromPath = start?.[1] === undefined
      ? undefined
      : qualifyURLDOI(start[1].replace(/\.pdf$/i, ""), true);
    if (fromPath !== undefined) candidates.push(fromPath);
  }

  const first = candidates[0];
  if (first === undefined) return undefined;
  // Disagreement means one of them is wrong and nothing here can tell which.
  return candidates.every((candidate) => candidate === first) ? first : undefined;
}

/** Extract a DOI from the ordered meta-tag layer. `base` resolves a relative
 * `citation_pdf_url`, which is a legitimate shape. */
export function extractMetaDOI(meta: readonly PageMetaProbe[] = [], base?: string): string | undefined {
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
  // tags. publication_doi remains for older SAGE/Atypon pages. citation_pdf_url
  // is a URL and is parsed structurally; the bibliographic tags are text, but a
  // page may legitimately state one as a doi.org link, so any value that is
  // itself a URL takes the URL rules.
  for (const name of [
    "citation_doi",
    "dc.identifier",
    "prism.doi",
    "publication_doi",
    "citation_pdf_url",
  ]) {
    for (const value of byName.get(name) ?? []) {
      const doi = /^https?:\/\//i.test(value) || name === "citation_pdf_url"
        ? doiFromURL(value, base)
        : sniffDOI(value);
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
  const fromMeta = extractMetaDOI(probe.meta, probe.href);
  if (fromMeta !== undefined) return fromMeta;

  for (const value of [probe.canonical, probe.ogURL, probe.href]) {
    if (typeof value !== "string") continue;
    const doi = doiFromURL(value, probe.href);
    if (doi !== undefined) return doi;
  }

  for (const href of probe.anchorHrefs ?? []) {
    let absolute: string;
    try {
      const anchorURL = new URL(href, probe.href);
      const hostname = anchorURL.hostname.toLowerCase();
      if (hostname !== "doi.org" && !hostname.endsWith(".doi.org")) continue;
      absolute = anchorURL.href;
    } catch {
      continue;
    }
    const doi = doiFromURL(absolute);
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

/**
 * The only part of a page URL that may cross to the daemon on the Send-PDF
 * path: scheme and host, with path, query, fragment, and any userinfo dropped.
 *
 * `sanitizePageHost` above serves `pdf_grab_request`, which wants a bare
 * hostname; `page_acquire.url` must be a parseable http(s) URL
 * (`protocol.go`'s `PageAcquirePayload.validate`), so this returns an origin
 * rather than a host. Undefined means no safe value exists — callers must
 * refuse rather than send the original, because an unrepresentable outbound
 * frame is a fatal transport failure, not a refusal (AGENTS.md).
 */
export function pageAcquireOrigin(value: string): string | undefined {
  let url: URL;
  try {
    url = new URL(pdfSourceURL(value));
  } catch {
    return undefined;
  }
  if (url.protocol !== "https:" && url.protocol !== "http:") return undefined;
  if (url.hostname.length === 0) return undefined;
  return `${url.protocol}//${url.host}`;
}

/** Query parameter names that carry a bearer-grade credential when their value
 * is long enough to be one. Publisher and CDN delivery links are signed with
 * these; a short `?token=1` is a page parameter, not a credential. */
const CREDENTIAL_PARAMS = new Set([
  "token",
  "signature",
  "x-amz-signature",
  "x-amz-credential",
  "x-amz-security-token",
  "key-pair-id",
  "awsaccesskeyid",
  "hmac",
  "jwt",
  "access_token",
  "sessionid",
  "ticket",
]);

/** A credential value shorter than this is a page parameter, not a signature. */
const CREDENTIAL_VALUE_MIN = 32;

/**
 * True when a URL's query carries a signed, expiring delivery credential.
 *
 * Such a URL cannot be fetched a second time, so papio must never try: the
 * grant is bound to the session that minted it, and asking again returns an
 * error page rather than the file. Measured against a live publisher link
 * (`watermark02.silverchair.com`, `token` of 852 characters), which answered a
 * second request with `HTTP 400` and, in the provider's own words, "Your
 * session has timed out. Please go back to the article page and click the PDF
 * link again."
 *
 * Over-reporting is the safe direction: papio degrades to asking the
 * researcher to press the viewer's own Download button — which serves bytes
 * already in hand, needing no fetch — instead of failing after a doomed one.
 */
export function carriesSignedCredential(value: string): boolean {
  let url: URL;
  try {
    url = new URL(pdfSourceURL(value));
  } catch {
    return false;
  }
  for (const [name, param] of url.searchParams.entries()) {
    if (!CREDENTIAL_PARAMS.has(name.toLowerCase())) continue;
    if (param.length >= CREDENTIAL_VALUE_MIN) return true;
  }
  return false;
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