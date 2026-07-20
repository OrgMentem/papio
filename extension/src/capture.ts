// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Fixture capture for adapter development (Phase 3). The popup injects a tiny
// serializer into the ACTIVE tab, then this module sanitizes the returned HTML
// *in the popup* — never in the page — and the popup writes it to disk as a
// versioned adapter fixture.
//
// Sanitization is dependency-free string processing so it runs identically in a
// bun test and in the extension popup: a tolerant tag/text walk, no DOM, no
// happy-dom at runtime. Determinism is a hard requirement — the same page in
// must always produce the same fixture out — so every transform is a pure
// string rewrite with no clock, randomness, or ordering dependence.
//
// Privacy contract (Contract item 3): the fixture that leaves the tab carries
// no secrets. Scripts/inline JS, query strings, fragments, form values, and any
// token-shaped string are stripped or masked. The popup additionally REFUSES to
// write a fixture whose sanitized form still contains a residual secret, so a
// dirty capture fails closed rather than landing on disk.

/** Providers the capture tool can record fixtures for. Superset of the enabled
 * adapter set: a provider appears here as soon as fixture capture is wanted,
 * and in `adapters/types.ts` only once its fixtures and tests exist. */
export type Provider =
  | "proquest"
  | "jstor"
  | "ebsco"
  | "springer"
  | "elsevier"
  | "acm"
  | "wiley"
  | "tandfonline"
  | "sage"
  | "psycnet"
  | "hal"
  | "nature"
  | "thieme"
  | "cambridge"
  | "emerald"
  | "annualreviews"
  | "oup"
  | "mitpress"
  | "bmj"
  | "psychiatryonline"
  | "jamanetwork"
  | "lww";

/** Adapter scenarios the capture UI can record; unreachable states stay assisted. */
export type Scenario =
  | "success"
  | "login-return"
  | "terms"
  | "no-entitlement"
  | "wrong-work"
  | "drift";

export const PROVIDERS: readonly Provider[] = [
  "proquest",
  "jstor",
  "ebsco",
  "springer",
  "elsevier",
  "acm",
  "wiley",
  "tandfonline",
  "sage",
  "psycnet",
  "hal",
  "nature",
  "thieme",
  "cambridge",
  "emerald",
  "annualreviews",
  "oup",
  "mitpress",
  "bmj",
  "psychiatryonline",
  "jamanetwork",
  "lww",
];
export const SCENARIOS: readonly Scenario[] = [
  "success",
  "login-return",
  "terms",
  "no-entitlement",
  "wrong-work",
  "drift",
];

export interface FixtureMeta {
  provider: Provider;
  scenario: Scenario;
  /** Origin + path only — the caller MUST have already dropped query/fragment. */
  originNoQuery: string;
  /** ISO-8601 capture timestamp. */
  capturedISO: string;
}

/** Auto-observation is development material, deliberately outside the
 * user-selectable adapter `Scenario` union. */
export interface ObservedFixtureMeta {
  provider: string;
  scenario: "observed";
  originNoQuery: string;
  capturedISO: string;
}

type SanitizedFixtureMeta = FixtureMeta | ObservedFixtureMeta;

/** A token-shaped run: 24+ contiguous URL-safe / base64-ish characters. Long
 * enough to catch signed values, session ids, JWT segments, and API keys while
 * leaving ordinary words and short slugs alone. */
const TOKEN_RE = /[A-Za-z0-9+/_-]{24,}/g;

/** Elements whose *contents* never belong in a selector fixture. Their bodies
 * are emptied; the (attribute-scrubbed) open/close tags stay so structure and
 * selectors are preserved. `style` and SVG internals go too — neither is
 * classifier evidence, and inline SVG style nodes confuse HTML fixture parsers
 * by entering raw-text mode. `textarea` is emptied so no typed secret survives. */
const EMPTIED_CONTENT = /(<(script|noscript|iframe|object|embed|style|textarea|svg)\b[^>]*>)[\s\S]*?(<\/\2\s*>)/gi;

/** Email addresses identify real people (authors, librarians, the capturing
 * user) and are never selector evidence. Masked in text and attribute values,
 * and rejected by residualLeak if one survives. */
const EMAIL_RE = /[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g;

/** URL-bearing attributes: query string and fragment are removed from each.
 * Beyond the fixed names, any attribute whose (dash/underscore-insensitive)
 * name ends in url/uri/href/src/link is URL-valued — providers ship
 * `data-fullTexturl`, `institution-log-in-url`, `register-url`, and similar
 * auth-return carriers whose queries must not reach a committed fixture. */
const URL_ATTRS: Record<string, true> = {
  href: true,
  src: true,
  action: true,
  "data-src": true,
  "data-href": true,
};

function isURLAttr(lname: string): boolean {
  if (URL_ATTRS[lname]) return true;
  const flat = lname.replace(/[-_:.]/g, "");
  return /(?:url|uri|href|src|link)$/.test(flat);
}

/** Attributes that exist only to carry a per-request or per-session value —
 * CSP nonces, CDN request ids, session ids, CSRF fields. Name-keyed and
 * blanked outright: their values are short enough to slip under TOKEN_RE but
 * are still request-scoped identifiers, never selector evidence. */
function isSessionAttr(lname: string): boolean {
  const flat = lname.replace(/[-_:.]/g, "");
  return (
    flat.endsWith("nonce") ||
    flat.endsWith("requestid") ||
    flat.endsWith("sessionid") ||
    flat.includes("csrf") ||
    flat.includes("xsrf")
  );
}

/** Attribute names whose static values are selector-bearing. Semantic CSS/BEM
 * names and explicit provider test hooks survive, but opaque token-shaped runs
 * inside them are masked so an adapter can never depend on a per-session
 * UUID/hash. Inline style is excluded entirely; it carries no classifier
 * signal. */
const STRUCTURAL_ATTRS: Record<string, true> = {
  class: true,
  id: true,
  name: true,
  rel: true,
  type: true,
  role: true,

  "data-auto": true,
  "data-test": true,
  "data-testid": true,
  "data-automation-id": true,
  "data-qa": true,
};

/** Replace every token-shaped run and email address with the literal `TOKEN`. */
function scrubTokens(text: string): string {
  return text.replace(EMAIL_RE, "TOKEN").replace(TOKEN_RE, "TOKEN");
}

function isSemanticSelectorToken(token: string): boolean {
  const expanded = token.replace(/([a-z])([A-Z])/g, "$1-$2");
  const words = expanded.split(/[-_]+/).filter(Boolean);
  return words.length >= 2 && words.every((word) => /^[A-Za-z]{2,16}$/.test(word));
}

/** Structural attribute values keep semantic word runs and lose only the
 * opaque parts of a mixed identifier: `downloadPDFLink_MSTAR_216440925`
 * becomes `downloadPDFLink_MSTAR_TOKEN`, so an adapter can match a stable
 * prefix (`[id^='downloadPDFLink_']`) while the per-record suffix is masked. */
function scrubSelectorTokens(text: string): string {
  return text.replace(TOKEN_RE, (token) => {
    if (isSemanticSelectorToken(token)) return token;
    const parts = token.split(/([-_]+)/);
    if (parts.length === 1) return "TOKEN";
    return parts
      .map((part) => {
        if (/^[-_]+$/.test(part) || part === "") return part;
        const words = part.replace(/([a-z])([A-Z])/g, "$1-$2").split("-");
        return words.every((word) => /^[A-Za-z]{2,16}$/.test(word)) ? part : "TOKEN";
      })
      .join("")
      .replace(/TOKEN(?:[-_]+TOKEN)+/g, "TOKEN");
  });
}

/** URL path segments are checked independently. A slash is routing syntax, not
 * evidence that adjacent semantic path words form one opaque credential. */
const URL_TOKEN_RE = /[A-Za-z0-9+_-]{24,}/g;

function scrubURLValue(value: string): string {
  const cut = value.search(/[?#]/);
  const queryless = cut === -1 ? value : value.slice(0, cut);
  return queryless
    .split("/")
    .map((segment) =>
      segment.replace(URL_TOKEN_RE, (token) => (isSemanticSelectorToken(token) ? token : "TOKEN")),
    )
    .join("/");
}

const ATTR_RE = /([-\w:]+)(\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+)))?/g;

interface ParsedAttr {
  name: string;
  hasValue: boolean;
  value: string;
}

/** Parse the attribute region of a start tag into (name, value) pairs. Tolerant
 * of unquoted and valueless attributes; tag names have no `=` so they are never
 * captured as attributes. */
function parseAttrs(attrRegion: string): ParsedAttr[] {
  const attrs: ParsedAttr[] = [];
  for (const m of attrRegion.matchAll(ATTR_RE)) {
    const name = m[1];
    if (!name) continue;
    const hasValue = m[2] !== undefined;
    const value = m[3] ?? m[4] ?? m[5] ?? "";
    attrs.push({ name, hasValue, value });
  }
  return attrs;
}

/** Does a start tag carry a token-shaped `content` attribute? Used to drop
 * `<meta>` tags that ship a CSRF token / build hash / session value. */
function hasTokenContent(attrs: ParsedAttr[]): boolean {
  for (const a of attrs) {
    if (a.name.toLowerCase() === "content" && new RegExp(TOKEN_RE.source).test(a.value)) return true;
  }
  return false;
}

/** URL-valued provider metadata is selector evidence, but its path/query can
 * carry record or session identifiers and must be scrubbed like href/src. */
function urlMeta(attrs: ParsedAttr[]): boolean {
  const name = attrs.find((a) => a.name.toLowerCase() === "name")?.value.toLowerCase();
  const content = attrs.find((a) => a.name.toLowerCase() === "content")?.value;
  return (
    name !== undefined &&
    content !== undefined &&
    /(?:^|[_:.-])url$/.test(name) &&
    /^(?:https?:\/\/|\/)/i.test(content)
  );
}

/** Rewrite a single start tag: strip URL query/fragment, blank form values,
 * drop autofill hints, mask token-shaped attribute values. Returns the empty
 * string when the whole tag must disappear (token-bearing `<meta>`). */
function rewriteStartTag(raw: string): string {
  const head = /^<\s*([a-zA-Z][-\w]*)/.exec(raw);
  if (!head) return raw;
  const tag = (head[1] ?? "").toLowerCase();

  // Split "<name attrs...>" (keeping any trailing "/" and ">").
  const openLen = head[0].length;
  const closeMatch = /\s*\/?>$/.exec(raw);
  const closeStart = closeMatch ? raw.length - closeMatch[0].length : raw.length - 1;
  const attrRegion = raw.slice(openLen, closeStart);
  const closing = closeMatch ? closeMatch[0].replace(/^\s+/, "") : ">";

  const attrs = parseAttrs(attrRegion);

  const metaName =
    tag === "meta"
      ? (attrs.find((a) => a.name.toLowerCase() === "name")?.value.toLowerCase() ?? "")
      : "";
  // A meta *named* for a per-request identifier is dropped whole even when its
  // value is too short for TOKEN_RE (e.g. CDN ray/request ids).
  if (isSessionAttr(metaName)) return "";
  const safeURLMeta = tag === "meta" && urlMeta(attrs);
  if (tag === "meta" && !safeURLMeta && hasTokenContent(attrs)) return "";

  const isFormValue = tag === "input" || tag === "select" || tag === "option" || tag === "button";

  const rendered: string[] = [];
  for (const a of attrs) {
    const lname = a.name.toLowerCase();

    // Autofill / autofocus hints carry nothing structural and leak intent.
    if (lname === "autocomplete" || lname === "autofocus") continue;

    if (!a.hasValue) {
      rendered.push(a.name);
      continue;
    }

    let value = a.value;

    // Blank form values, inline style, and per-request identifier attributes:
    // none can be an adapter selector, and all can carry user/session-specific
    // content (nonces and CDN request ids are short enough to pass TOKEN_RE).
    if ((lname === "value" && isFormValue) || lname === "style" || isSessionAttr(lname)) {
      value = "";
    } else {
      if (isURLAttr(lname) || (safeURLMeta && lname === "content")) {
        value = scrubURLValue(value);
      } else {
        value = STRUCTURAL_ATTRS[lname] ? scrubSelectorTokens(value) : scrubTokens(value);
      }
      value = value.replace(EMAIL_RE, "TOKEN");
    }

    rendered.push(`${a.name}="${value}"`);
  }

  const body = rendered.length ? ` ${rendered.join(" ")}` : "";
  return `<${head[1]}${body}${closing}`;
}

const TOKEN_STREAM = /<!--[\s\S]*?-->|<[^>]*>/g;

/**
 * Deterministically sanitize a captured document into an adapter fixture.
 *
 * Order matters and is fixed:
 *  1. Empty the bodies of script/style/etc. (their content is never a selector).
 *  2. Walk the remaining markup as a tag/comment/text stream:
 *     - start tags   → strip URL tails, blank form values, mask token attrs;
 *     - comments      → emptied (comments are never selector evidence);
 *     - text nodes    → mask token-shaped runs;
 *     - end tags/etc. → passed through untouched.
 *  3. Prepend the papio-fixture header (added last, so it is never scrubbed).
 */
export function sanitizeFixture(html: string, meta: SanitizedFixtureMeta): string {
  const emptied = html.replace(EMPTIED_CONTENT, "$1$3");

  let out = "";
  let last = 0;
  for (const m of emptied.matchAll(TOKEN_STREAM)) {
    const start = m.index;
    const token = m[0];
    // Text node before this tag/comment: mask tokens.
    if (start > last) out += scrubTokens(emptied.slice(last, start));

    if (token.startsWith("<!--")) {
      // Comments are never selector evidence and frequently hide disabled
      // markup containing session-bearing URLs. Keep an empty node so adjacent
      // text cannot merge, but retain none of the comment body.
      out += "<!---->";
    } else if (/^<\s*\//.test(token)) {
      out += token; // end tag: no attributes to touch
    } else if (/^<\s*[a-zA-Z]/.test(token)) {
      out += rewriteStartTag(token);
    } else {
      out += token; // doctype, processing markers, stray "<...>"
    }
    last = start + token.length;
  }
  if (last < emptied.length) out += scrubTokens(emptied.slice(last));

  return `${fixtureHeader(meta)}\n${out}`;
}

/** The first-line provenance comment the adapter harness keys on. Dynamic
 * provider routes can carry session/record identifiers in their path, so mask
 * token-shaped path runs while preserving the stable origin. */
export function fixtureHeader(meta: SanitizedFixtureMeta): string {
  const parsed = new URL(meta.originNoQuery);
  const scrubbedPath = scrubURLValue(parsed.pathname);
  const safePath = scrubbedPath.startsWith("/") ? scrubbedPath : `/${scrubbedPath}`;
  return (
    `<!-- papio-fixture provider="${meta.provider}" scenario="${meta.scenario}"` +
    ` origin="${parsed.origin}${safePath}" captured="${meta.capturedISO}" -->`
  );
}

/**
 * Fail-closed residual-secret detector run on the *sanitized* output before it
 * is written. Attribute/tag names are syntax, not values: modern provider
 * frameworks legitimately use names such as `data-sveltekit-preload-data`,
 * which must not be mistaken for credentials. Values, text, and comments
 * remain guarded.
 */
export function residualLeak(sanitized: string): string | null {
  const email = new RegExp(EMAIL_RE.source).exec(sanitized);
  if (email) return `an email address survived sanitization (${email[0].slice(0, 8)}…)`;

  const residual = (value: string, allowSemanticSelector = false): string | undefined => {
    for (const match of value.matchAll(new RegExp(TOKEN_RE.source, "g"))) {
      const token = match[0];
      if (allowSemanticSelector && isSemanticSelectorToken(token)) continue;
      return token;
    }
    return undefined;
  };
  const residualURL = (value: string): string | undefined => {
    for (const segment of value.split("/")) {
      for (const match of segment.matchAll(new RegExp(URL_TOKEN_RE.source, "g"))) {
        const token = match[0];
        if (isSemanticSelectorToken(token)) continue;
        return token;
      }
    }
    return undefined;
  };
  let last = 0;
  for (const m of sanitized.matchAll(TOKEN_STREAM)) {
    const start = m.index;
    const syntax = m[0];
    const textToken = residual(sanitized.slice(last, start));
    if (textToken) return `a token-shaped value survived sanitization (${textToken.slice(0, 8)}…)`;

    if (syntax.startsWith("<!--")) {
      // The first comment is generated by fixtureHeader(). Its origin is
      // already query-free and path-scrubbed, but a short semantic path can
      // combine with the hostname into a token-shaped run
      // (e.g. nature.com/articles/nature14539). Validate the whole generated
      // shape before exempting it; every other comment remains guarded.
      const provenance =
        start === 0
          ? /^<!-- papio-fixture provider="([a-z][a-z0-9_-]*)" scenario="[a-z][a-z0-9_-]*" origin="https:\/\/[^"?\s]+\/[^"?\s]*" captured="\d{4}-\d{2}-\d{2}T[^"\s]+" -->$/.exec(
              syntax,
            )
          : null;
      // The generated shape is trusted, but the provider field may originate
      // from an observed hostname. Keep guarding it against opaque per-user
      // or session subdomains.
      const commentToken = provenance
        ? residual(provenance[1] ?? "")
        : residual(syntax.slice(4, -3));
      if (commentToken) return `a token-shaped value survived sanitization (${commentToken.slice(0, 8)}…)`;
    } else if (/^<\s*[a-zA-Z]/.test(syntax)) {
      const head = /^<\s*([a-zA-Z][-\w]*)/.exec(syntax);
      const close = /\s*\/?>$/.exec(syntax);
      if (head && close) {
        const attrs = parseAttrs(syntax.slice(head[0].length, syntax.length - close[0].length));
        if (head[1]?.toLowerCase() === "meta") {
          const metaName =
            attrs.find((a) => a.name.toLowerCase() === "name")?.value.toLowerCase() ?? "";
          const content = attrs.find((a) => a.name.toLowerCase() === "content")?.value ?? "";
          if (isSessionAttr(metaName) && content !== "") {
            return `a per-request identifier meta survived sanitization (${metaName})`;
          }
        }
        const safeURLMeta = head[1]?.toLowerCase() === "meta" && urlMeta(attrs);
        for (const attr of attrs) {
          if (!attr.hasValue) continue;
          const lname = attr.name.toLowerCase();
          if (isSessionAttr(lname) && attr.value !== "") {
            return `a per-request identifier attribute survived sanitization (${attr.name})`;
          }
          const urlValued = isURLAttr(lname) || (safeURLMeta && lname === "content");
          if (urlValued && /[?#]/.test(attr.value)) {
            return "a URL-bearing attribute still contains a query string";
          }
          const attrToken = urlValued
            ? residualURL(attr.value)
            : residual(attr.value, STRUCTURAL_ATTRS[lname] === true);
          if (attrToken) return `a token-shaped value survived sanitization (${attrToken.slice(0, 8)}…)`;
        }
      }
    }
    last = start + syntax.length;
  }
  const tailToken = residual(sanitized.slice(last));
  if (tailToken) return `a token-shaped value survived sanitization (${tailToken.slice(0, 8)}…)`;
  return null;
}

// ---------------------------------------------------------------------------
// Popup capture wiring
// ---------------------------------------------------------------------------

/** Serializable snapshot returned by the injected page function. */
export interface PageCapture {
  html: string;
  origin: string;
  path: string;
}

/** Injected into the active tab via chrome.scripting.executeScript. Must be
 * fully self-contained — it is serialized, so it may not close over any popup
 * state. It reads only structure and location; nothing sensitive is computed
 * here (sanitization happens back in the popup). */
export function capturePage(): PageCapture {
  return {
    html: document.documentElement.outerHTML,
    origin: location.origin,
    path: location.pathname,
  };
}

/** 8 MiB cap on the captured document; larger pages are refused with a clear
 * error rather than serialized into a data: URL. */
export const MAX_CAPTURE_BYTES = 8 * 1024 * 1024;

/** Minimal chrome surface captureFixture needs. The real `chrome` satisfies it
 * structurally; tests inject a fake with scripting + downloads. */
export interface ChromeCaptureApi {
  tabs: { query(info: { active: boolean; currentWindow: boolean }): Promise<Array<{ id?: number | undefined }>> };
  scripting: {
    executeScript(injection: {
      target: { tabId: number };
      func: () => PageCapture;
    }): Promise<Array<{ result?: PageCapture | undefined }>>;
  };
  downloads: {
    download(options: {
      url: string;
      filename: string;
      conflictAction: "uniquify";
      saveAs: boolean;
    }): Promise<number>;
  };
}

/** Chrome ignores downloads.download's `filename` for `data:` URLs, so fixture
 * writes would land as `download (N).html`. downloadFixture enqueues the
 * intended relative path here; the background onDeterminingFilename listener
 * dequeues it to relocate the file. FIFO is safe because fixture writes are
 * serialized (observe queue) and manual capture is a discrete user gesture. */
const fixtureFilenameReservationTTLMS = 60_000;

type FixtureFilenameTimer = number | Timer;

interface FixtureFilenameReservation {
  filename: string;
  timeout: FixtureFilenameTimer;
}

const pendingFixtureFilenames: FixtureFilenameReservation[] = [];

function removePendingFixtureFilename(reservation: FixtureFilenameReservation): void {
  const index = pendingFixtureFilenames.indexOf(reservation);
  if (index !== -1) {
    pendingFixtureFilenames.splice(index, 1);
  }
  clearTimeout(reservation.timeout);
}

/** Dequeue the intended path for a fixture `data:` download, for the
 * onDeterminingFilename listener. Non-fixture downloads pass through untouched. */
export function takePendingFixtureFilename(url: string): string | undefined {
  if (!url.startsWith("data:text/html")) return undefined;
  const reservation = pendingFixtureFilenames.shift();
  if (!reservation) return undefined;
  clearTimeout(reservation.timeout);
  return reservation.filename;
}

/** Write already-sanitized fixture HTML through Chrome's download manager. Both
 * manual captures and auto-observations use this exact final write path. */
export async function downloadFixture(
  api: Pick<ChromeCaptureApi, "downloads">,
  filename: string,
  sanitized: string,
): Promise<{ downloadId: number; filename: string }> {
  const url = `data:text/html;charset=utf-8,${encodeURIComponent(sanitized)}`;
  const reservation: FixtureFilenameReservation = { filename, timeout: 0 };
  pendingFixtureFilenames.push(reservation);
  reservation.timeout = setTimeout(() => {
    removePendingFixtureFilename(reservation);
  }, fixtureFilenameReservationTTLMS);
  if (typeof reservation.timeout === "object" && "unref" in reservation.timeout) {
    reservation.timeout.unref();
  }
  try {
    const downloadId = await api.downloads.download({
      url,
      filename,
      conflictAction: "uniquify",
      saveAs: false,
    });
    return { downloadId, filename };
  } catch (err) {
    removePendingFixtureFilename(reservation);
    throw err;
  }
}

export type CaptureResult =
  | { ok: true; downloadId: number; filename: string }
  | { ok: false; error: string };

/**
 * Capture the active tab into an adapter fixture and download it. Requires a
 * user gesture upstream (the popup Capture button) so `activeTab` is usable.
 *
 * Fails closed at every boundary: no active tab, no injection result, oversized
 * payload, or a residual secret in the sanitized output all return an error
 * without writing anything.
 */
export async function captureFixture(
  api: ChromeCaptureApi,
  provider: Provider,
  scenario: Scenario,
  now: () => Date,
): Promise<CaptureResult> {
  const [tab] = await api.tabs.query({ active: true, currentWindow: true });
  const tabId = tab?.id;
  if (typeof tabId !== "number") return { ok: false, error: "no active tab to capture" };

  const [injected] = await api.scripting.executeScript({ target: { tabId }, func: capturePage });
  const page = injected?.result;
  if (!page || typeof page.html !== "string") {
    return { ok: false, error: "could not read the active tab (is it a restricted page?)" };
  }

  const bytes = new TextEncoder().encode(page.html).length;
  if (bytes > MAX_CAPTURE_BYTES) {
    return { ok: false, error: `page is ${bytes} bytes; over the ${MAX_CAPTURE_BYTES}-byte capture cap` };
  }

  const sanitized = sanitizeFixture(page.html, {
    provider,
    scenario,
    originNoQuery: `${page.origin}${page.path}`,
    capturedISO: now().toISOString(),
  });

  const leak = residualLeak(sanitized);
  if (leak) return { ok: false, error: `refusing to write a dirty fixture: ${leak}` };

  const written = await downloadFixture(api, `papio-fixtures/${provider}/${scenario}.html`, sanitized);
  return { ok: true, ...written };
}
