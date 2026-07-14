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

/** Providers with a live-verified handoff; the fixture tree is keyed by these. */
export type Provider = "proquest" | "jstor" | "ebsco" | "springer";

/** Adapter scenarios every provider must be fixture-tested against. */
export type Scenario =
  | "success"
  | "login-return"
  | "terms"
  | "no-entitlement"
  | "wrong-work"
  | "drift";

export const PROVIDERS: readonly Provider[] = ["proquest", "jstor", "ebsco", "springer"];
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

/** A token-shaped run: 24+ contiguous URL-safe / base64-ish characters. Long
 * enough to catch signed values, session ids, JWT segments, and API keys while
 * leaving ordinary words and short slugs alone. */
const TOKEN_RE = /[A-Za-z0-9+/_-]{24,}/g;

/** Elements whose *contents* never belong in a selector fixture. Their bodies
 * are emptied; the (attribute-scrubbed) open/close tags stay so structure and
 * selectors are preserved. `style` goes too — selectors matter, styling does
 * not — and `textarea` so no typed secret survives in its body. */
const EMPTIED_CONTENT = /(<(script|noscript|iframe|object|embed|style|textarea)\b[^>]*>)[\s\S]*?(<\/\2\s*>)/gi;

/** URL-bearing attributes: query string and fragment are removed from each. */
const URL_ATTRS: Record<string, true> = { href: true, src: true, action: true, "data-src": true };

/** Attribute names whose static values are selector-bearing. Semantic CSS/BEM
 * names survive, but opaque token-shaped runs inside them are masked so an
 * adapter can never depend on a per-session UUID/hash. Inline style is excluded
 * entirely; it carries no classifier signal. */
const STRUCTURAL_ATTRS: Record<string, true> = { class: true, id: true, name: true, rel: true, type: true, role: true };

/** Replace every token-shaped run with the literal `TOKEN`. */
function scrubTokens(text: string): string {
  return text.replace(TOKEN_RE, "TOKEN");
}

function isSemanticSelectorToken(token: string): boolean {
  const expanded = token.replace(/([a-z])([A-Z])/g, "$1-$2");
  const words = expanded.split(/[-_]+/).filter(Boolean);
  return words.length >= 2 && words.every((word) => /^[A-Za-z]{2,16}$/.test(word));
}

function scrubSelectorTokens(text: string): string {
  return text.replace(TOKEN_RE, (token) => (isSemanticSelectorToken(token) ? token : "TOKEN"));
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

  if (tag === "meta" && hasTokenContent(attrs)) return "";

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

    // Blank form and inline-style values: neither can be an adapter selector,
    // and both can carry user/session-specific content.
    if ((lname === "value" && isFormValue) || lname === "style") {
      value = "";
    } else {
      if (URL_ATTRS[lname]) {
        const cut = value.search(/[?#]/);
        if (cut !== -1) value = value.slice(0, cut);
      }
      value = STRUCTURAL_ATTRS[lname] ? scrubSelectorTokens(value) : scrubTokens(value);
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
 *     - comments      → mask token-shaped runs;
 *     - text nodes    → mask token-shaped runs;
 *     - end tags/etc. → passed through untouched.
 *  3. Prepend the papio-fixture header (added last, so it is never scrubbed).
 */
export function sanitizeFixture(html: string, meta: FixtureMeta): string {
  const emptied = html.replace(EMPTIED_CONTENT, "$1$3");

  let out = "";
  let last = 0;
  for (const m of emptied.matchAll(TOKEN_STREAM)) {
    const start = m.index;
    const token = m[0];
    // Text node before this tag/comment: mask tokens.
    if (start > last) out += scrubTokens(emptied.slice(last, start));

    if (token.startsWith("<!--")) {
      // Scrub only the comment body. Including the trailing `--` in a
      // token-shaped match would destroy the `-->` delimiter and merge later
      // markup into the comment.
      out += `<!--${scrubTokens(token.slice(4, -3))}-->`;
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

/** The first-line provenance comment the adapter harness keys on. */
export function fixtureHeader(meta: FixtureMeta): string {
  return (
    `<!-- papio-fixture provider="${meta.provider}" scenario="${meta.scenario}"` +
    ` origin="${meta.originNoQuery}" captured="${meta.capturedISO}" -->`
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
  const urlQuery = /(?:href|src|action|data-src)\s*=\s*"[^"]*\?[^"]*"/i.exec(sanitized);
  if (urlQuery) return "a href/src attribute still contains a query string";

  const residual = (value: string, allowSemanticSelector = false): string | undefined => {
    for (const match of value.matchAll(new RegExp(TOKEN_RE.source, "g"))) {
      const token = match[0];
      if (allowSemanticSelector && isSemanticSelectorToken(token)) continue;
      return token;
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
      const commentToken = residual(syntax.slice(4, -3));
      if (commentToken) return `a token-shaped value survived sanitization (${commentToken.slice(0, 8)}…)`;
    } else if (/^<\s*[a-zA-Z]/.test(syntax)) {
      const head = /^<\s*[a-zA-Z][-\w]*/.exec(syntax);
      const close = /\s*\/?>$/.exec(syntax);
      if (head && close) {
        const attrs = parseAttrs(syntax.slice(head[0].length, syntax.length - close[0].length));
        for (const attr of attrs) {
          if (!attr.hasValue) continue;
          const lname = attr.name.toLowerCase();
          const attrToken = residual(
            attr.value,
            lname === "class" || lname === "id" || lname === "name",
          );
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

  const filename = `papio-fixtures/${provider}/${scenario}.html`;
  const url = `data:text/html;charset=utf-8,${encodeURIComponent(sanitized)}`;
  const downloadId = await api.downloads.download({
    url,
    filename,
    conflictAction: "uniquify",
    saveAs: false,
  });

  return { ok: true, downloadId, filename };
}
