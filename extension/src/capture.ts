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
// dirty capture cannot cross the bridge. Fixtures are committed to a PUBLIC
// repo, so the sanitizer fails closed: JSON-LD keeps only an allowlist of
// bibliographic keys (a person keeps only its name and affiliation name;
// contact data, profile links and every unlisted key are dropped), and its
// output must be a fixed point of the scrub or residualLeak refuses it.
// Public bibliographic identifiers are NOT secrets: a DOI whose suffix is not
// token-shaped survives masking verbatim in DOI-named meta tags and JSON-LD.

import {
  BROWSER_PROTOCOL_VERSION,
  MAX_BROWSER_MESSAGE_BYTES,
  MsgPageCapture,
  type PageCapturePayload,
} from "./protocol";

/** Scenarios that the daemon can retain as page-capture fixtures. */
export type Scenario = "success" | "login-return" | "no-entitlement" | "drift" | "terms";

/** Capture vocabulary includes every actual adapter id plus historical names
 * such as elsevier. capture-registry.test.ts pins coverage against the real
 * adapter registry without pulling that runtime registry into the sanitizer. */
export const PROVIDERS = [
  "chemrxiv",
  "proquest",
  "jstor",
  "ebsco",
  "springer",
  "elsevier",
  "acm",
  "acs",
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
  "sciencedirect",
  "cochrane",
  "europepmc",
  "hogrefe",
  "figshare",
  "exlibris-primo",
  "iospress",
  "science",
] as const;
export type Provider = (typeof PROVIDERS)[number];
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
/** A bibliographic DOI candidate. A DOI names a public work, so a DOI whose
 * suffix is not token-shaped (`isSafeDOI`) is kept verbatim even where a long
 * separator-joined suffix trips TOKEN_RE (for example the run
 * `1590/1806-9061-2017-0499` in `10.1590/1806-9061-2017-0499`). `?` and `#`
 * end the match so a query or fragment can never hide inside a DOI. A match
 * whose suffix IS token-shaped gets no exemption and is scrubbed like any
 * other token. */
const DOI_RE = /\b10\.\d{4,9}\/[^\s"'<>?#]+/g;

/** Meta names whose `content` is a bibliographic DOI. Their tags survive the
 * token-content drop and their values keep the DOI verbatim (queries are still
 * stripped from URL-shaped values). */
const DOI_META_NAMES: Record<string, true> = {
  citation_doi: true,
  "dc.identifier.doi": true,
  bepress_citation_doi: true,
  "dc.doi": true,
  "dcterms.identifier": true,
  doi: true,
  "dc.identifier": true,
};

/** Elements whose *contents* never belong in a selector fixture. Their bodies
 * are emptied; the (attribute-scrubbed) open/close tags stay so structure and
 * selectors are preserved. `style` and SVG internals go too — neither is
 * classifier evidence, and inline SVG style nodes confuse HTML fixture parsers
 * by entering raw-text mode. `textarea` is emptied so no typed secret survives. */
const EMPTIED_CONTENT = /(<(script|noscript|iframe|object|embed|style|textarea|svg)\b[^>]*>)[\s\S]*?(<\/\2\s*>)/gi;

/** Email addresses identify real people (authors, librarians, the capturing
 * user) and are never selector evidence. Masked in text and attribute values,
 * and rejected by residualLeak if one survives. Every part is length-bounded
 * (RFC 5321: local part 64, label 63) so matching stays linear: an unbounded
 * local part made one 300 KB alphanumeric run cost ~57 s of backtracking. */
const EMAIL_RE = /[A-Za-z0-9._%+-]{1,64}@(?:[A-Za-z0-9-]{1,63}\.){1,8}[A-Za-z]{2,63}/g;

/** URL userinfo (`scheme://user:pass@`) anywhere in a value or text. Short
 * credentials pass every token rule, and a single-label host escapes the
 * email rule, so userinfo is removed wherever URLs are scrubbed and refused
 * by residualLeak anywhere in the output. The scheme is length-bounded and
 * the userinfo class stops at `/`, so matching stays linear. */
const USERINFO_RE = /([a-z][a-z0-9+.-]{0,31}:\/\/)[^\s/?#@"'<>]*@/gi;

/** IP addresses identify the capturing browser: Elsevier's refusal page, for
 * one, prints the operator's own address beside its reference number. They
 * are never selector evidence. An IPv4 address is four dotted octets that are
 * not part of a longer dotted run or a `name/version` pair, so a DOI
 * (10.1016/j.x), a date, and a user-agent version (Chrome/153.0.0.0) are left
 * alone while a URL host (https://192.0.2.1/) is not. An IPv6 address needs
 * eight groups or a `::`, so a clock time (13:29:37) is left alone too.
 * internal/redact.IPAddresses applies the same rules in the daemon. */
const IPV4_RE =
  /(?<![A-Za-z0-9_.-])(?<![A-Za-z0-9_.-]\/)(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(?![A-Za-z0-9_-]|\.\d)/g;
const IPV6_RE =
  /(?<![A-Za-z0-9_:.])(?:(?:[0-9A-Fa-f]{1,4}:){7}[0-9A-Fa-f]{1,4}|(?:[0-9A-Fa-f]{1,4}(?::[0-9A-Fa-f]{1,4}){0,6})?::(?:[0-9A-Fa-f]{1,4}(?::[0-9A-Fa-f]{1,4}){0,6})?)(?![A-Za-z0-9_:])/g;
/** A four-part number a page labels as a version is not an address. */
const VERSION_LABEL_RE = /\b(?:version|release|build|v)\s*:?\s*$/i;

function scrubIPAddresses(text: string): string {
  return text
    .replace(IPV4_RE, (address, offset: number, whole: string) =>
      VERSION_LABEL_RE.test(whole.slice(Math.max(0, offset - 16), offset)) ? address : "TOKEN",
    )
    .replace(IPV6_RE, (address) => (address === "::" ? address : "TOKEN"));
}

/** Whether an IP address is left in `text`. */
function containsIPAddress(text: string): boolean {
  for (const match of text.matchAll(new RegExp(IPV4_RE.source, "g"))) {
    if (!VERSION_LABEL_RE.test(text.slice(Math.max(0, match.index - 16), match.index))) return true;
  }
  for (const match of text.matchAll(new RegExp(IPV6_RE.source, "g"))) {
    if (match[0] !== "::") return true;
  }
  return false;
}

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

/** Replace every token-shaped run, email address, and IP address with the
 * literal `TOKEN`, drop URL userinfo, and cut the query/fragment from any
 * absolute URL written into text: a page can print a short signed link that
 * no length rule sees. */
function scrubTokens(text: string): string {
  return scrubIPAddresses(text.replace(USERINFO_RE, "$1").replace(TEXT_URL_RE, (url) => scrubURLValue(url)))
    .replace(EMAIL_RE, "TOKEN")
    .replace(TOKEN_RE, "TOKEN");
}
/**
 * Opaque-piece rule, shared by DOI suffixes and retained JSON-LD values.
 * A value is cut into pieces at every character outside `[A-Za-z0-9+=]`
 * (so `-._;()/:` all separate). A piece is plain when it reads as a word or a
 * short number: lowercase up to 16, Capitalized up to 16, ALLCAPS up to 8,
 * digits up to 13 (an ISBN-13), or a camelCase/compound semantic word.
 * Everything else (letter+digit mixes, random case, long runs) is opaque.
 * A value is token-shaped when its opaque pieces carry 16 or more characters
 * IN TOTAL: this catches one bearer token split across several path
 * segments (`AbCdEf0123456789/GhIjKl0123456789`), which a per-segment
 * 24-character check misses, while record ids such as `p1hs8-6wm96` or
 * `2012v10n16` stay below the bound.
 */
const PIECE_RE = /[A-Za-z0-9+=]+/g;
const OPAQUE_BUDGET = 16;

function isPlainPiece(piece: string): boolean {
  return (
    /^[a-z]{1,16}$/.test(piece) ||
    /^[A-Z][a-z]{1,15}$/.test(piece) ||
    /^[A-Z]{1,8}$/.test(piece) ||
    /^\d{1,13}$/.test(piece) ||
    isSemanticSelectorToken(piece)
  );
}

function opaqueChars(text: string): number {
  let total = 0;
  for (const m of text.matchAll(PIECE_RE)) if (!isPlainPiece(m[0])) total += m[0].length;
  return total;
}

/** A DOI keeps its exemption only when its suffix is not token-shaped (no
 * opaque budget overrun, which also covers any 24+ character run between
 * separators). The 8 measured `citation_doi` losses, e.g.
 * `10.1590/1806-9061-2017-0499` and
 * `10.21062/ujep/x.2016/a/1213-2489/MT/16/5/1033`, all qualify;
 * `10.1234/ABCDEFGHIJKLMNOPQRSTUVWXYZ012345` does not. */
function isSafeDOI(doi: string): boolean {
  return opaqueChars(doi.slice(doi.indexOf("/") + 1)) < OPAQUE_BUDGET;
}

/** Split `text` around safe DOIs: `parts` never contains a safe DOI; `dois`
 * holds them in order, to be put back verbatim between transformed parts. */
function splitSafeDOIs(text: string): { parts: string[]; dois: string[] } {
  const parts: string[] = [];
  const dois: string[] = [];
  let last = 0;
  for (const m of text.matchAll(new RegExp(DOI_RE.source, "g"))) {
    if (!isSafeDOI(m[0])) continue;
    parts.push(text.slice(last, m.index));
    dois.push(m[0]);
    last = m.index + m[0].length;
  }
  parts.push(text.slice(last));
  return { parts, dois };
}

function joinAroundDOIs(parts: string[], dois: string[]): string {
  let out = parts[0] ?? "";
  for (let i = 0; i < dois.length; i += 1) out += `${dois[i]}${parts[i + 1] ?? ""}`;
  return out;
}

/** Same as scrubTokens, but a safe bibliographic DOI passes through verbatim:
 * the DOI is a public work identifier, and its suffix alone can trip
 * TOKEN_RE. A token-shaped DOI suffix gets no exemption. */
function scrubTokensPreservingDOI(text: string): string {
  const { parts, dois } = splitSafeDOIs(text);
  return joinAroundDOIs(parts.map(scrubTokens), dois);
}

/** Remove URL userinfo (`user:pass@`): at the start of a URL value
 * (including protocol-relative `//user@host`) and after every `scheme://`
 * anywhere in the text. */
function stripUserinfo(value: string): string {
  return value.replace(/^((?:[a-z][a-z0-9+.-]{0,31}:)?\/\/)[^/?#]*@/i, "$1").replace(USERINFO_RE, "$1");
}

/** Scrub the `content` of a DOI-named meta tag: a safe DOI survives verbatim,
 * URL-shaped values lose userinfo and query/fragment, and every other secret
 * pattern (email, IP, token runs, token-shaped DOI suffixes) is masked. */
function scrubDoiMetaContent(value: string): string {
  const isURL = /^(?:[a-z][a-z0-9+.-]*:\/\/|\/)/i.test(value);
  const bare = isURL ? stripUserinfo(value) : value;
  const cut = bare.search(/[?#]/);
  return scrubTokensPreservingDOI(isURL && cut !== -1 ? bare.slice(0, cut) : bare);
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
  const bare = stripUserinfo(value);
  const cut = bare.search(/[?#]/);
  const queryless = cut === -1 ? bare : bare.slice(0, cut);
  return queryless
    .split("/")
    .map((segment) =>
      segment.replace(URL_TOKEN_RE, (token) => (isSemanticSelectorToken(token) ? token : "TOKEN")),
    )
    .join("/");
}

/** An absolute http(s) URL written into text or a non-URL attribute value. */
const TEXT_URL_RE = /\bhttps?:\/\/[^\s<>"'`]+/gi;
/** The same URL still carrying a query or fragment — what residualLeak rejects. */
const TEXT_URL_QUERY_RE = /\bhttps?:\/\/[^\s<>"'`?#]*[?#]/i;
/** `<meta http-equiv="refresh" content="0; url=/next?code=…">` names its
 * target after `url=`, often as a relative URL. */
const REFRESH_URL_RE = /(\burl\s*=\s*['"]?)([^'"\s]*)/i;

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
 * `<meta>` tags that ship a CSRF token / build hash / session value. DOI
 * substrings are carved out first: a DOI suffix can itself be token-shaped,
 * and a DOI is a public identifier, never a drop reason. */
function hasTokenContent(attrs: ParsedAttr[]): boolean {
  for (const a of attrs) {
    if (a.name.toLowerCase() !== "content") continue;
    const withoutDOI = splitSafeDOIs(a.value).parts.join(" ");
    if (new RegExp(TOKEN_RE.source).test(withoutDOI)) return true;
  }
  return false;
}

/** Meta content that is itself a URL (`citation_pdf_url`, `og:url`,
 * `og:image`, …) is selector evidence, but its path/query can carry record or
 * session identifiers and must be scrubbed like href/src, whichever of
 * `name`/`property`/`itemprop` labels it. A meta is URL-valued when its content
 * is shaped like an absolute or root-relative URL, or when its label names a
 * URL — the planner resolves a bare relative `citation_pdf_url` against the
 * page, so `download.pdf?token=…` is a URL too. */
const URL_META_LABEL_RE = /(?:^|[_:.-])(?:url|image|video|audio)$/;

function urlMeta(attrs: ParsedAttr[]): boolean {
  const content = attrs.find((a) => a.name.toLowerCase() === "content")?.value;
  if (content === undefined) return false;
  if (/^(?:https?:\/\/|\/)/i.test(content)) return true;
  return attrs.some((a) => {
    const lname = a.name.toLowerCase();
    return (
      (lname === "name" || lname === "property" || lname === "itemprop") &&
      URL_META_LABEL_RE.test(a.value.trim().toLowerCase())
    );
  });
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
  const refreshMeta =
    tag === "meta" &&
    attrs.some((a) => a.name.toLowerCase() === "http-equiv" && a.value.trim().toLowerCase() === "refresh");
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
      } else if (refreshMeta && lname === "content") {
        value = scrubTokens(
          value.replace(REFRESH_URL_RE, (_match, lead: string, url: string) => lead + scrubURLValue(url)),
        );
      } else if (tag === "meta" && lname === "content" && DOI_META_NAMES[metaName] === true) {
        value = scrubDoiMetaContent(value);
      } else {
        value = STRUCTURAL_ATTRS[lname] ? scrubSelectorTokens(value) : scrubTokens(value);
      }
      value = scrubIPAddresses(value.replace(USERINFO_RE, "$1").replace(EMAIL_RE, "TOKEN"));
    }

    rendered.push(`${a.name}="${value}"`);
  }

  const body = rendered.length ? ` ${rendered.join(" ")}` : "";
  return `<${head[1]}${body}${closing}`;
}

const TOKEN_STREAM = /<!--[\s\S]*?-->|<[^>]*>/g;
/** `application/ld+json` script elements: structured bibliographic metadata
 * (identifiers, DOI, name/headline, datePublished, isPartOf, url, contentUrl)
 * that E0 reads. They are stashed before the emptying pass and restored by
 * `scrubJsonLdBody` instead of being emptied: emptying them both loses the
 * metadata and leaves an empty script that E0 reports as `e0:json-ld-invalid`.
 * The unquoted-type alternative is matched because provider markup varies. */
const LDJSON_RE =
  /<script\b[^>]*type=(?:"application\/ld\+json"|'application\/ld\+json'|application\/ld\+json)[^>]*>[\s\S]*?<\/script\s*>/gi;
const LDJSON_MARKER_RE = /<script\b[^>]*data-papio-ldjson="(\d+)"[^>]*><\/script\s*>/g;
const LDJSON_BODY_RE = /^<script\b[^>]*>([\s\S]*?)<\/script\s*>$/i;

/** A JSON-LD body larger or deeper than these is not parsed at all: it
 * becomes empty. Measured maxima on 297 pages: 9.4 KB and depth 7. */
const JSONLD_MAX_BYTES = 256 * 1024;
const JSONLD_MAX_DEPTH = 16;

/**
 * JSON-LD privacy rule. Fixtures are committed to a PUBLIC repo, so the walk
 * is an ALLOWLIST: a key survives only when its context lists it; every other
 * key (contact data, birthDate, a person's url/sameAs/identifier, token-shaped
 * property names, anything unforeseen) is dropped with its whole subtree.
 *
 * - work: the bibliographic record and nested works/media/publishers.
 * - person: anything typed Person (`Person`, `schema:Person`, the
 *   `http(s)://schema.org/Person` IRIs, alone or in a type array), anything
 *   carrying givenName/familyName, and every untyped or non-Organization
 *   value under author/creator/editor/contributor. Keeps only the name.
 * - org: an affiliation, or an Organization in an author slot: name only.
 * - identifier: a PropertyValue under `identifier`.
 * - context: `@context` keeps strings only (an inline term map is dropped).
 */
type JsonLdContext = "work" | "personSlot" | "person" | "org" | "identifier" | "context";

type JsonLdScope = "work" | "person" | "org" | "identifier";

const JSONLD_KEYS: Record<JsonLdScope, Record<string, true>> = {
  work: {
    "@context": true, "@type": true, "@id": true, "@graph": true,
    identifier: true, doi: true, name: true, headline: true, alternativeHeadline: true,
    datePublished: true, dateCreated: true, dateModified: true,
    isPartOf: true, hasPart: true, publisher: true, url: true, sameAs: true,
    // plan.ts reads contentUrl under encoding, associatedMedia, and any nested
    // record; a PDF part declared through hasPart was measured (2026-09).
    encoding: true, associatedMedia: true, contentUrl: true, encodingFormat: true,
    pagination: true, pageStart: true, pageEnd: true, volumeNumber: true, issueNumber: true, inLanguage: true,
    author: true, creator: true, editor: true, contributor: true,
  },
  person: { "@type": true, name: true, givenName: true, familyName: true, affiliation: true },
  org: { "@type": true, name: true },
  identifier: { "@type": true, name: true, propertyID: true, value: true },
};
const JSONLD_PERSON_SLOTS: Record<string, true> = { author: true, creator: true, editor: true, contributor: true };

/** Normalized schema.org type names of a node (`schema:Person`,
 * `https://schema.org/Person` and `Person` all read as `person`). */
function jsonLdTypes(record: Record<string, unknown>): string[] {
  const raw = record["@type"];
  return (Array.isArray(raw) ? raw : [raw])
    .filter((t): t is string => typeof t === "string")
    .map((t) => t.replace(/^(?:https?:\/\/schema\.org\/|schema:)/i, "").toLowerCase());
}

/**
 * Scrub one retained JSON-LD string. URL-shaped values lose userinfo and
 * query/fragment. Safe DOIs stay verbatim (`isSafeDOI`). The whole value
 * (host and path joined) then passes the opaque-piece rule, so a token split
 * across path segments is masked; after that URLs get the per-segment token
 * rule and other text the full token rule, plus email and IP masking.
 * Opaque masking runs first so the result is a fixed point, which
 * `residualLeak` relies on.
 */
function scrubJsonLdString(text: string): string {
  const isURL = /^(?:[a-z][a-z0-9+.-]*:\/\/|\/)/i.test(text);
  let value = text;
  if (isURL) {
    value = stripUserinfo(value);
    const cut = value.search(/[?#]/);
    if (cut !== -1) value = value.slice(0, cut);
  }
  const { parts, dois } = splitSafeDOIs(value);
  const masked =
    opaqueChars(parts.join(" ")) >= OPAQUE_BUDGET
      ? parts.map((part) => part.replace(PIECE_RE, (piece) => (isPlainPiece(piece) ? piece : "TOKEN")))
      : parts;
  const scrubbed = masked.map((part) =>
    isURL ? scrubIPAddresses(scrubURLValue(part).replace(EMAIL_RE, "TOKEN")) : scrubTokens(part),
  );
  return joinAroundDOIs(scrubbed, dois);
}

/** Walk one parsed JSON-LD value under the allowlist; `undefined` drops it.
 * Depth is bounded by the caller's pre-parse scan. */
function scrubJsonLdValue(value: unknown, context: JsonLdContext): unknown {
  if (typeof value === "string") return scrubJsonLdString(value);
  if (typeof value === "number" || typeof value === "boolean" || value === null) return value;
  if (Array.isArray(value)) {
    return value.map((item) => scrubJsonLdValue(item, context)).filter((item) => item !== undefined);
  }
  if (typeof value !== "object" || context === "context") return undefined;
  const record = value as Record<string, unknown>;
  const types = jsonLdTypes(record);
  const isOrg = types.includes("organization");
  let scope: JsonLdScope;
  if (types.includes("person") || "givenName" in record || "familyName" in record) scope = "person";
  else if (context === "personSlot") scope = isOrg ? "org" : "person";
  else if (context === "person" || context === "org") scope = context === "person" && !isOrg ? "person" : "org";
  else scope = context === "identifier" ? "identifier" : "work";
  const allowed = JSONLD_KEYS[scope];
  const out: Record<string, unknown> = {};
  for (const [key, item] of Object.entries(record)) {
    // `=== true`, not truthiness: inherited names such as `constructor` or
    // `__proto__` must never pass the allowlist.
    if (allowed[key] !== true) continue;
    let child: JsonLdContext = scope;
    if (key === "@context") child = "context";
    else if (scope === "work" && JSONLD_PERSON_SLOTS[key] === true) child = "personSlot";
    else if (scope === "work" && key === "identifier") child = "identifier";
    else if (scope === "person" && key === "affiliation") child = "org";
    const scrubbed = scrubJsonLdValue(item, child);
    if (scrubbed !== undefined) out[key] = scrubbed;
  }
  return out;
}

/** Whether a JSON text nests deeper than `max`, by a linear scan that never
 * recurses (strings and escapes are skipped). */
function jsonDeeperThan(body: string, max: number): boolean {
  let depth = 0;
  let inString = false;
  for (let i = 0; i < body.length; i += 1) {
    const ch = body[i];
    if (inString) {
      if (ch === "\\") i += 1;
      else if (ch === '"') inString = false;
    } else if (ch === '"') inString = true;
    else if (ch === "{" || ch === "[") {
      depth += 1;
      if (depth > max) return true;
    } else if (ch === "}" || ch === "]") depth -= 1;
  }
  return false;
}

/**
 * Scrub one stashed JSON-LD body, returning its replacement. An oversized,
 * over-deep, empty, or invalid body becomes empty (the raw page already
 * reports an empty/invalid block as `e0:json-ld-invalid`, so the sanitizer
 * never invents that signal). A parsed body is walked under the allowlist
 * and serialized with `<`, `>`, `&`, U+2028 and U+2029 escaped as `\uXXXX`,
 * so no retained string can close the script element or open markup.
 * The output is a fixed point: scrubbing it again returns it unchanged.
 */
function scrubJsonLdBody(body: string): string {
  if (body.length > JSONLD_MAX_BYTES || jsonDeeperThan(body, JSONLD_MAX_DEPTH)) return "";
  let parsed: unknown;
  try {
    parsed = JSON.parse(body);
  } catch {
    return "";
  }
  const scrubbed = scrubJsonLdValue(parsed, "work");
  if (scrubbed === undefined) return "";
  return JSON.stringify(scrubbed).replace(
    /[<>&\u2028\u2029]/g,
    (ch) => `\\u${ch.charCodeAt(0).toString(16).padStart(4, "0")}`,
  );
}

/**
 * Deterministically sanitize a captured document into an adapter fixture.
 *
 * Order matters and is fixed:
 *  0. Stash `application/ld+json` script elements as markers (their bodies
 *     are structured metadata, scrubbed and restored in step 4).
 *  1. Empty the bodies of script/style/etc. (their content is never a selector).
 *  2. Walk the remaining markup as a tag/comment/text stream:
 *     - start tags   → strip URL tails, blank form values, mask token attrs;
 *     - comments      → emptied (comments are never selector evidence);
 *     - text nodes    → mask token-shaped runs;
 *     - end tags/etc. → passed through untouched.
 *  3. Restore the stashed JSON-LD bodies, scrubbed by `scrubJsonLdBody`.
 *  4. Prepend the papio-fixture header (added last, so it is never scrubbed).
 */
export function sanitizeFixture(html: string, meta: SanitizedFixtureMeta): string {
  const stashed: string[] = [];
  const withPlaceholders = html.replace(LDJSON_RE, (m) => {
    stashed.push(m);
    return `<script type="application/ld+json" data-papio-ldjson="${stashed.length - 1}"></script>`;
  });
  const emptied = withPlaceholders.replace(EMPTIED_CONTENT, "$1$3");

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

  // Restore JSON-LD with person-identifying subtrees removed (step 0 stashed
  // the raw elements; the marker carries only an index, never page content).
  // A plain `<script type>` open tag replaces the original: the original's
  // attributes (nonces, ids) are never selector evidence.
  const restored = out.replace(LDJSON_MARKER_RE, (_m, i: string) => {
    const original = stashed[Number(i)] ?? "";
    const body = LDJSON_BODY_RE.exec(original)?.[1] ?? "";
    return `<script type="application/ld+json">${scrubJsonLdBody(body)}</script>`;
  });

  return `${fixtureHeader(meta)}\n${restored}`;
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
  // Never echo the address itself: this message reaches logs.
  if (containsIPAddress(sanitized)) return "an IP address survived sanitization";
  // URL credentials anywhere: after any `scheme://`, or at the start of a
  // URL-bearing attribute (`//user@host`, checked with the attributes below).
  if (new RegExp(USERINFO_RE.source, "i").test(sanitized)) return "URL credentials survived sanitization";

  const residual = (value: string, allowSemanticSelector = false): string | undefined => {
    for (const match of value.matchAll(new RegExp(TOKEN_RE.source, "g"))) {
      const token = match[0];
      if (allowSemanticSelector && isSemanticSelectorToken(token)) continue;
      return token;
    }
    return undefined;
  };
  // A safe DOI is a public work identifier, not a secret: its suffix alone
  // can trip TOKEN_RE, so safe DOIs are carved out before the token check on
  // DOI-named meta contents. A token-shaped DOI suffix (`isSafeDOI` false)
  // gets no exemption.
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
  let inLdJson = false;
  // A restored `application/ld+json` body must be exactly what
  // `scrubJsonLdBody` emits: re-scrubbing it must return it unchanged. That
  // one check enforces every JSON-LD rule at once (allowlisted keys only,
  // person scope, userinfo, whole-URL opaque pieces, token-shaped DOIs,
  // escaped `<`/`>`/`&`, size and depth bounds). A body cut short by a stray
  // tag fails to parse and is refused too. All other script bodies are
  // emptied, so only JSON-LD gets this treatment; other text keeps the
  // token check.
  const textLeak = (text: string): string | undefined => {
    if (inLdJson) {
      return text === "" || scrubJsonLdBody(text) === text ? undefined : "a JSON-LD block is not in sanitized form";
    }
    const token = residual(text);
    if (token) return `a token-shaped value survived sanitization (${token.slice(0, 8)}…)`;
    return TEXT_URL_QUERY_RE.test(text) ? "a URL in page text still contains a query string" : undefined;
  };
  for (const m of sanitized.matchAll(TOKEN_STREAM)) {
    const start = m.index;
    const syntax = m[0];
    const leak = textLeak(sanitized.slice(last, start));
    if (leak) return leak;

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
      if (!provenance && TEXT_URL_QUERY_RE.test(syntax)) return "a URL in a comment still contains a query string";
    } else if (/^<\s*[a-zA-Z]/.test(syntax)) {
      const head = /^<\s*([a-zA-Z][-\w]*)/.exec(syntax);
      const close = /\s*\/?>$/.exec(syntax);
      if (head && close) {
        const tag = head[1]?.toLowerCase() ?? "";
        const attrs = parseAttrs(syntax.slice(head[0].length, syntax.length - close[0].length));
        if (tag === "script") {
          inLdJson =
            attrs.some(
              (a) => a.name.toLowerCase() === "type" && a.value.toLowerCase() === "application/ld+json",
            );
        }
        if (tag === "meta") {
          const metaName =
            attrs.find((a) => a.name.toLowerCase() === "name")?.value.toLowerCase() ?? "";
          const content = attrs.find((a) => a.name.toLowerCase() === "content")?.value ?? "";
          if (isSessionAttr(metaName) && content !== "") {
            return `a per-request identifier meta survived sanitization (${metaName})`;
          }
        }
        const safeURLMeta = tag === "meta" && urlMeta(attrs);
        const refreshMeta =
          tag === "meta" &&
          attrs.some((a) => a.name.toLowerCase() === "http-equiv" && a.value.trim().toLowerCase() === "refresh");
        const doiMeta =
          tag === "meta" &&
          DOI_META_NAMES[attrs.find((a) => a.name.toLowerCase() === "name")?.value.toLowerCase() ?? ""] ===
            true;
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
          if (
            TEXT_URL_QUERY_RE.test(attr.value) ||
            (refreshMeta && lname === "content" && /[?#]/.test(REFRESH_URL_RE.exec(attr.value)?.[2] ?? ""))
          ) {
            return "a URL inside an attribute value still contains a query string";
          }
          const doiContent = doiMeta && lname === "content";
          if ((urlValued || doiContent) && /^(?:[a-z][a-z0-9+.-]{0,31}:)?\/\/[^/?#]*@/i.test(attr.value)) {
            return "a URL-bearing attribute still contains credentials";
          }
          if (doiContent && /^(?:[a-z][a-z0-9+.-]*:\/\/|\/)/i.test(attr.value) && /[?#]/.test(attr.value)) {
            return "a URL-bearing attribute still contains a query string";
          }
          const attrToken = urlValued
            ? residualURL(attr.value)
            : doiContent
              ? residual(splitSafeDOIs(attr.value).parts.join(" "))
              : residual(attr.value, STRUCTURAL_ATTRS[lname] === true);
          if (attrToken) return `a token-shaped value survived sanitization (${attrToken.slice(0, 8)}…)`;
        }
      }
    } else if (/^<\s*\//.test(syntax)) {
      if (/^<\s*\/\s*script\b/i.test(syntax)) inLdJson = false;
    }
    last = start + syntax.length;
  }
  return textLeak(sanitized.slice(last)) ?? null;
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
 * here (sanitization happens back in the popup).
 *
 * papio's own injected surfaces are removed from the serialized copy. Both are
 * transient — the acknowledgement chip lasts three seconds, the loss toast
 * eight — but a capture is a fixture candidate, and a fixture is evidence about
 * a PROVIDER's page. A stray `<div id="papio-…">` in committed bytes is papio
 * describing itself. Their copy is already out of reach because both render
 * into a shadow root and `outerHTML` omits shadow trees; this removes the host
 * elements too, so the capture contains no trace of papio at all.
 *
 * The live DOM is not touched: the hosts are removed from a clone. Removing
 * them for real would make a capture cancel an offer the researcher is reading.
 */
export function capturePage(): PageCapture {
  const root = document.documentElement.cloneNode(true) as HTMLElement;
  for (const host of root.querySelectorAll(
    "#papio-extension-action-ack-v1, #papio-extension-loss-toast-v1",
  )) {
    host.remove();
  }
  return {
    html: root.outerHTML,
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
  /** Set only when this capture answers a daemon page_capture_request. It is
   * what lets the daemon bind the content frame to that specific request
   * instead of guessing from provider+scenario (papio-85a7420f4cd2564f), so
   * an unsolicited capture must leave it undefined. */
  requestID?: string;
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
    ...(meta.requestID === undefined ? {} : { request_id: meta.requestID }),
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
