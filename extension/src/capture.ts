// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Fixture capture for adapter development (Phase 3). The popup injects a tiny
// serializer into the ACTIVE tab, then this module sanitizes the returned HTML
// *in the popup* — never in the page — and sends it through papio's native
// bridge for daemon-owned storage.
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
// emit a fixture whose sanitized form still contains a residual secret, so a
// dirty capture cannot cross the bridge.

import {
  BROWSER_PROTOCOL_VERSION,
  MAX_BROWSER_MESSAGE_BYTES,
  MsgPageCapture,
  type PageCapturePayload,
} from "./protocol";

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
  | "lww"
  | "informit"
  | "primo"
  | "mdpi"
  | "clinicalkey";

/** Scenarios that the daemon can retain as page-capture fixtures. */
export type Scenario = "success" | "login-return" | "no-entitlement" | "drift" | "terms";

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
  "informit",
  "primo",
  "mdpi",
  "clinicalkey",
];
export const SCENARIOS: readonly Scenario[] = ["success", "login-return", "no-entitlement", "drift", "terms"];

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
// Capture transport
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

export const MAX_CAPTURE_DECODED_BYTES = 2 << 20;
export const MAX_CAPTURE_FRAME_BYTES = MAX_BROWSER_MESSAGE_BYTES;
const MAX_CAPTURE_FRAME_OVERHEAD_BYTES = 1024;

interface PageCaptureMeta {
  host: string;
  scenario: Scenario | "observed";
  adapterID?: string;
  adapterVersion?: string;
  jobID?: string;
}

type EncodedCapture =
  | { ok: true; payload: PageCapturePayload }
  | { ok: false; error: string };

function base64(bytes: Uint8Array): string {
  let binary = "";
  const chunkSize = 0x8000;
  for (let start = 0; start < bytes.length; start += chunkSize) {
    binary += String.fromCharCode(...bytes.subarray(start, start + chunkSize));
  }
  return btoa(binary);
}

async function gzip(bytes: Uint8Array<ArrayBuffer>): Promise<Uint8Array | undefined> {
  if (typeof CompressionStream !== "function") {
    console.warn('papio: refusing page capture because CompressionStream("gzip") is unavailable');
    return undefined;
  }
  let stream: CompressionStream;
  try {
    stream = new CompressionStream("gzip");
  } catch (error) {
    console.warn("papio: refusing page capture because gzip compression is unavailable", error);
    return undefined;
  }
  const source = new ReadableStream<BufferSource>({
    start(controller) {
      controller.enqueue(bytes);
      controller.close();
    },
  });
  try {
    return new Uint8Array(await new Response(source.pipeThrough(stream)).arrayBuffer());
  } catch (error) {
    console.warn("papio: refusing page capture because gzip compression failed", error);
    return undefined;
  }
}

function encodedFrameBytes(payload: PageCapturePayload, jobID: string | undefined): number {
  const frame: Record<string, unknown> = {
    protocol: BROWSER_PROTOCOL_VERSION,
    type: MsgPageCapture,
    msg_id: "x".repeat(64),
    seq: Number.MAX_SAFE_INTEGER,
    payload,
  };
  if (jobID !== undefined) frame["job_id"] = jobID;
  return new TextEncoder().encode(JSON.stringify(frame)).byteLength;
}

/** Compress one already-sanitized capture for its single native frame. */
export async function encodePageCapture(sanitized: string, meta: PageCaptureMeta): Promise<EncodedCapture> {
  const decoded = new TextEncoder().encode(sanitized);
  if (decoded.byteLength === 0 || decoded.byteLength > MAX_CAPTURE_DECODED_BYTES) {
    return {
      ok: false,
      error: `sanitized page is ${decoded.byteLength} bytes; over the ${MAX_CAPTURE_DECODED_BYTES}-byte decoded capture cap`,
    };
  }

  const compressed = await gzip(decoded);
  if (compressed === undefined) return { ok: false, error: "gzip compression is unavailable" };
  if (compressed.byteLength > Math.floor(((MAX_CAPTURE_FRAME_BYTES - MAX_CAPTURE_FRAME_OVERHEAD_BYTES) * 3) / 4)) {
    return { ok: false, error: `compressed page exceeds the ${MAX_CAPTURE_FRAME_BYTES}-byte native frame cap` };
  }

  const payload: PageCapturePayload = {
    host: meta.host,
    scenario: meta.scenario,
    ...(meta.adapterID === undefined ? {} : { adapter_id: meta.adapterID }),
    ...(meta.adapterVersion === undefined ? {} : { adapter_version: meta.adapterVersion }),
    encoding: "gzip+base64",
    bytes: decoded.byteLength,
    body: base64(compressed),
  };
  if (encodedFrameBytes(payload, meta.jobID) > MAX_CAPTURE_FRAME_BYTES) {
    return { ok: false, error: `encoded page exceeds the ${MAX_CAPTURE_FRAME_BYTES}-byte native frame cap` };
  }
  return { ok: true, payload };
}

/** A capture send either succeeds/fails as a bare boolean (legacy fakes and
 * `observe.ts`'s own relay) or, from the popup's `chrome.runtime.sendMessage`
 * bridge to the background broker, as a structured result carrying the
 * refusal reason from `runtimeFailure` — e.g. "the connected daemon does not
 * support terms captures". captureFixture forwards that reason instead of a
 * generic failure string so the operator can tell "upgrade the daemon" from
 * an ordinary transport failure. */
export type SendPageCaptureResult = boolean | { captured: boolean; error?: string };

/** Minimal browser surface captureFixture needs. The real `chrome` satisfies it
 * structurally; tests inject a fake with scripting plus the native-frame relay. */
export interface ChromeCaptureApi {
  tabs: { query(info: { active: boolean; currentWindow: boolean }): Promise<Array<{ id?: number | undefined }>> };
  scripting: {
    executeScript(injection: {
      target: { tabId: number };
      func: () => PageCapture;
    }): Promise<Array<{ result?: PageCapture | undefined }>>;
  };
  sendPageCapture(payload: PageCapturePayload): Promise<SendPageCaptureResult>;
}

export type CaptureResult = { ok: true; bytes: number } | { ok: false; error: string };

/**
 * Capture the active tab into a daemon-owned fixture. Requires a user gesture
 * upstream (the popup Capture button) so `activeTab` is usable.
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

  let injected: { result?: PageCapture | undefined } | undefined;
  try {
    [injected] = await api.scripting.executeScript({ target: { tabId }, func: capturePage });
  } catch {
    return { ok: false, error: "could not read the active tab (is it a restricted page?)" };
  }
  const page = injected?.result;
  if (
    !page ||
    typeof page.html !== "string" ||
    typeof page.origin !== "string" ||
    typeof page.path !== "string"
  ) {
    return { ok: false, error: "could not read the active tab (is it a restricted page?)" };
  }

  let host: string;
  try {
    const origin = new URL(page.origin);
    if (origin.protocol !== "http:" && origin.protocol !== "https:") throw new Error("non-web origin");
    host = origin.hostname;
  } catch {
    return { ok: false, error: "could not determine the active tab host" };
  }
  if (host === "") return { ok: false, error: "could not determine the active tab host" };

  let capturedISO: string;
  try {
    capturedISO = now().toISOString();
  } catch {
    return { ok: false, error: "could not timestamp the capture" };
  }
  const sanitized = sanitizeFixture(page.html, {
    provider,
    scenario,
    originNoQuery: `${page.origin}${page.path}`,
    capturedISO,
  });

  const leak = residualLeak(sanitized);
  if (leak) return { ok: false, error: `refusing to emit a dirty fixture: ${leak}` };

  const encoded = await encodePageCapture(sanitized, { host, scenario, adapterID: provider });
  if (!encoded.ok) return encoded;
  try {
    const sent = await api.sendPageCapture(encoded.payload);
    const captured = typeof sent === "boolean" ? sent : sent.captured;
    if (!captured) {
      // A bare `false`/`{captured:false}` with no message is the legacy
      // shape (transport failure, no reason attached) — only trust a
      // non-empty message string; anything else falls back to the generic
      // text so the status line never renders "undefined" or blank.
      const message = typeof sent === "object" && typeof sent.error === "string" && sent.error.length > 0
        ? sent.error
        : "could not send the capture to papio";
      return { ok: false, error: message };
    }
  } catch {
    return { ok: false, error: "could not send the capture to papio" };
  }
  return { ok: true, bytes: encoded.payload.bytes };
}
