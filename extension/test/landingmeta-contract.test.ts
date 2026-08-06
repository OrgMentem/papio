// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Cross-language contract test for citation_pdf_url extraction. The daemon's
// internal/landingmeta.PDFURL and this extension's extractMetaURL
// (src/background.ts) implement the same semantics independently — one
// parsing untrusted network HTML in Go, one reading the live DOM of a page
// the user is already on. internal/landingmeta/testdata/contract.json is the
// one corpus that pins both: it lives in the Go tree deliberately so a change
// on either side has to walk through the same fixtures, and this suite is
// what makes that enforceable from the extension side. Every case in the
// corpus is asserted by name below — a case silently skipped here is a case
// the two implementations are free to drift apart on without either side
// noticing.

import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

import { expect, test } from "bun:test";

import { parseHTML } from "./harness";

// The corpus and its HTML fixtures live under the Go package, not under
// extension/fixtures/ — see "The citation_pdf_url contract" in
// fixtures/README.md for why.
const CONTRACT_DIR = join(import.meta.dir, "..", "..", "internal", "landingmeta", "testdata");

interface ContractCase {
  name: string;
  file: string;
  base: string;
  want: string;
  agreed_with_extension: boolean;
  note: string;
}

const contract: ContractCase[] = JSON.parse(readFileSync(join(CONTRACT_DIR, "contract.json"), "utf8"));

function getCase(name: string): ContractCase {
  const found = contract.find((c) => c.name === name);
  if (found === undefined) throw new Error(`contract.json has no case named "${name}" — corpus drifted`);
  return found;
}

function loadCase(c: ContractCase): Document {
  return parseHTML(readFileSync(join(CONTRACT_DIR, c.file), "utf8"), c.base);
}

// The realm shape extractMetaURLMirror needs from a parsed document's window.
// Cast through `unknown` rather than the ambient DOM lib `Window` type: this
// file never sees a real Window, only happy-dom's, and the two are close but
// not identical, so pinning our own minimal shape is safer than trusting lib
// dom's shape to match happy-dom's exports byte for byte.
interface DocumentRealm {
  HTMLMetaElement: abstract new (...args: never[]) => Element;
  location: { href: string };
}

/**
 * Mirrors extractMetaURL in extension/src/background.ts (~line 860), body
 * for body:
 *
 *   function extractMetaURL(metaName: string): string | null {
 *     const el = document.querySelector(`meta[name="${metaName}"]`);
 *     if (!(el instanceof HTMLMetaElement)) return null;
 *     const raw = el.getAttribute("content")?.trim() ?? "";
 *     if (raw.length === 0) return null;
 *     try {
 *       const u = new URL(raw, location.href);
 *       if (u.protocol !== "https:") return null;
 *       if (u.username !== "" || u.password !== "") return null;
 *       const page = new URL(location.href);
 *       const isSelf = u.origin === page.origin &&
 *         u.pathname === page.pathname && u.search === page.search;
 *       return isSelf ? null : u.href;
 *     } catch { return null; }
 *   }
 *
 * That function cannot be imported here: it pulls chrome.* globals at module
 * load, and background.ts is owned by another change in flight this session.
 * This is a hand-copied mirror, not the real implementation — if
 * extractMetaURL's body ever changes, update this function in lockstep, or
 * this suite silently starts asserting a contract the extension no longer
 * honors. The drift-guard test below fails loudly if that happens instead.
 *
 * The only departure from the original: `HTMLMetaElement`/`location` are read
 * off the parsed document's own realm instead of the ambient globals a real
 * content script runs with. A live page has exactly one `HTMLMetaElement`
 * global to be `instanceof`; a happy-dom test document has none at the top
 * level, only its own window's — same identity check, same meaning, just
 * scoped to where the element actually lives.
 */
function extractMetaURLMirror(doc: Document): string | null {
  const view = doc.defaultView as unknown as DocumentRealm | null;
  const el = doc.querySelector('meta[name="citation_pdf_url"]');
  if (view === null || !(el instanceof view.HTMLMetaElement)) return null;
  const raw = el.getAttribute("content")?.trim() ?? "";
  if (raw.length === 0) return null;
  try {
    const u = new URL(raw, view.location.href);
    if (u.protocol !== "https:") return null;
    if (u.username !== "" || u.password !== "") return null;
    const page = new URL(view.location.href);
    const isSelf = u.origin === page.origin && u.pathname === page.pathname && u.search === page.search;
    return isSelf ? null : u.href;
  } catch {
    return null;
  }
}

// contract.json spells "no URL" as `want: ""` (plain JSON has no null-ish
// convention beyond that here); treat it the same as the mirror's `null`.
function wantURL(c: ContractCase): string | null {
  return c.want === "" ? null : c.want;
}

test("landingmeta contract corpus has at least 10 cases, each backed by a fixture on disk", () => {
  expect(contract.length).toBeGreaterThanOrEqual(10);
  for (const c of contract) {
    expect(existsSync(join(CONTRACT_DIR, c.file)), `${c.name}: missing fixture ${c.file}`).toBe(true);
  }
});

// The corpus currently pins exactly two deliberate JS/Go divergences, both
// daemon-stricter (asserted explicitly below by name). Any other case
// flipping to agreed_with_extension:false would otherwise be silently
// dropped by the agreement loop that follows — fail loudly instead so a new
// divergence has to be named and asserted here, not discovered by drift.
const KNOWN_DIVERGENT_CASES = ["malformed_content", "duplicate_conflicting"];

test("the only cases marked agreed_with_extension:false are the two named divergences below", () => {
  const divergent = contract
    .filter((c) => !c.agreed_with_extension)
    .map((c) => c.name)
    .sort();
  expect(divergent).toEqual([...KNOWN_DIVERGENT_CASES].sort());
});

for (const c of contract) {
  if (!c.agreed_with_extension) continue;
  test(`${c.name}: extension semantics match the daemon (${c.note})`, () => {
    expect(extractMetaURLMirror(loadCase(c))).toBe(wantURL(c));
  });
}

// --- The two deliberate divergences --------------------------------------------
//
// Each must be an explicit, named, PASSING test: the divergence is real and
// permanent, not a gap to close, so it has to stay visible rather than being
// silently skipped or folded into the agreement loop above. empty_content
// used to be a third one (the extension's `new URL("", location.href)`
// resolved to the landing page itself and returned it as a bogus "PDF");
// extractMetaURL now rejects a resolved URL that equals its own page, so
// empty_content is back in the agreed set above and asserts null there.

test("duplicate_conflicting: extension takes the first match; daemon fails closed on ambiguity", () => {
  const c = getCase("duplicate_conflicting");
  const doc = loadCase(c);
  // Go's contract: two different resolved URLs is unresolvable, so PDFURL
  // returns ErrConflictingPDFURL rather than guessing.
  expect(wantURL(c)).toBeNull();
  // The extension's actual behavior: querySelector returns the first match in
  // document order, so extractMetaURL silently prefers it. Safe there because
  // the page was already classified under the user's own session; not safe
  // for a daemon parsing unauthenticated network HTML, which is why Go fails
  // closed instead of copying this behavior.
  expect(extractMetaURLMirror(doc)).toBe("https://publisher.example/a.pdf");
  expect(c.note.length).toBeGreaterThan(0);
});

test("malformed_content: extension resolves it as a relative path; daemon's parser hard-errors", () => {
  const c = getCase("malformed_content");
  const doc = loadCase(c);
  // Go's contract: net/url.Parse("ht!tp://%%%") errors outright — "!" is not
  // a valid scheme character, and net/url has no relative-reference
  // fallback for a string shaped like this. PDFURL swallows that per-tag
  // parse failure and returns ("", nil), same as any other unparseable tag.
  expect(wantURL(c)).toBeNull();
  // The extension's actual behavior: WHATWG URL() has no such hard failure
  // mode here. "ht!tp:" fails scheme validation, so the parser falls back to
  // treating the whole string as a relative reference and resolves it
  // against location.href — producing a syntactically valid but bogus https
  // URL, not null.
  expect(extractMetaURLMirror(doc)).toBe("https://publisher.example/ht!tp://%%%");
  expect(c.note.length).toBeGreaterThan(0);
});

// A hand-copied mirror can drift silently — background.ts changed under this
// exact test twice already this session (the empty-content and self-URL
// rejections above were both added mid-session). extractMetaURL can't be
// imported (chrome.* globals at module load) or safely eval'd (it runs
// against a real page's DOM). The guard used to be three `toContain`
// substring checks; that missed any edit that didn't touch one of those
// three exact lines (a dropped `.trim()`, a renamed meta tag, a loosened
// `https:` check, or simple reordering all pass three-substring checks
// unscathed while the hand-copied mirror above keeps testing stale
// behavior). Instead, extract the function's real source text out of
// background.ts by brace-balanced scanning (string/template-literal aware,
// so a `${metaName}` interpolation or a `"//"`-bearing string never
// mistakenly closes the scan early), normalize it (strip comments, collapse
// whitespace) so formatting-only reflows don't false-positive, and diff the
// WHOLE body against a pinned verbatim copy below. Any semantic change —
// anywhere in the body — now fails this test. When it fails for a real,
// intentional change to extractMetaURL: update PINNED_EXTRACT_META_URL_SOURCE
// to match, re-sync extractMetaURLMirror above by hand, and re-run the full
// corpus loop so the two implementations are re-verified against
// contract.json before trusting them again.

/**
 * Extracts a top-level function's exact source text — from its signature
 * through the matching closing brace — by walking the source one character
 * at a time and tracking brace depth. `signature` MUST end in `{` (the
 * function's own opening brace); scanning starts there instead of via a
 * second `indexOf("{", …)`, which would otherwise skip past the real
 * opening brace and land on the first brace INSIDE the body (here, the
 * `${metaName}` template interpolation) — a bug caught while writing this
 * guard. String and template-literal contents are treated as opaque so a
 * literal `{`/`}` (or a `${…}` interpolation, or a `"//"` inside a URL)
 * never desyncs the depth count; a `${…}` interpolation gets its own nested
 * scan and resumes template-string mode on its matching `}`.
 */
function extractFunctionSource(src: string, signature: string): string {
  if (!signature.endsWith("{")) {
    throw new Error(`extractFunctionSource: signature must end with '{': ${signature}`);
  }
  const sigIndex = src.indexOf(signature);
  if (sigIndex === -1) {
    throw new Error(`extractFunctionSource: signature not found in source: ${signature}`);
  }
  let depth = 0;
  let i = sigIndex + signature.length - 1; // the signature's own trailing '{'
  const templateInterpDepths: number[] = [];
  let quote: '"' | "'" | "`" | null = null;
  for (; i < src.length; i++) {
    const ch = src[i];
    const prev = src[i - 1];
    if (quote !== null) {
      if (quote === "`" && ch === "$" && src[i + 1] === "{") {
        templateInterpDepths.push(depth);
        quote = null;
        depth++;
        i++; // consume the '{' of '${' together with the '$'
        continue;
      }
      if (ch === quote && prev !== "\\") quote = null;
      continue;
    }
    if (ch === '"' || ch === "'" || ch === "`") {
      quote = ch;
      continue;
    }
    if (ch === "{") {
      depth++;
    } else if (ch === "}") {
      depth--;
      if (templateInterpDepths.length > 0 && depth === templateInterpDepths[templateInterpDepths.length - 1]) {
        templateInterpDepths.pop();
        quote = "`"; // resume the enclosing template literal
      }
      if (depth === 0) return src.slice(sigIndex, i + 1);
    }
  }
  throw new Error(`extractFunctionSource: unbalanced braces scanning: ${signature}`);
}

/**
 * Strips `//` and `/* *\/` comments and collapses whitespace runs to a single
 * space, so two functions that differ only in formatting or commentary
 * compare equal while any change to actual code still fails. String and
 * template-literal contents are opaque here too, for the same reason as
 * above.
 */
function normalizeSource(src: string): string {
  let out = "";
  let quote: '"' | "'" | "`" | null = null;
  for (let i = 0; i < src.length; i++) {
    const ch = src[i];
    const next = src[i + 1];
    if (quote !== null) {
      out += ch;
      if (ch === quote && src[i - 1] !== "\\") quote = null;
      continue;
    }
    if (ch === '"' || ch === "'" || ch === "`") {
      quote = ch;
      out += ch;
      continue;
    }
    if (ch === "/" && next === "/") {
      while (i < src.length && src[i] !== "\n") i++;
      continue;
    }
    if (ch === "/" && next === "*") {
      i += 2;
      while (i < src.length && !(src[i] === "*" && src[i + 1] === "/")) i++;
      i++; // land on the '/' closing the block comment
      continue;
    }
    out += ch;
  }
  return out.replace(/\s+/g, " ").trim();
}

const EXTRACT_META_URL_SIGNATURE = "function extractMetaURL(metaName: string): string | null {";

// Verbatim copy of extractMetaURL's full source, signature through closing
// brace, as it stood when this guard was written. Re-sync this literal (and
// extractMetaURLMirror above it) whenever the real function legitimately
// changes — see the comment block above for the full re-sync procedure.
const PINNED_EXTRACT_META_URL_SOURCE = `function extractMetaURL(metaName: string): string | null {
  const el = document.querySelector(\`meta[name="\${metaName}"]\`);
  if (!(el instanceof HTMLMetaElement)) return null;
  const raw = el.getAttribute("content")?.trim() ?? "";
  if (raw.length === 0) return null;
  try {
    const u = new URL(raw, location.href);
    if (u.protocol !== "https:") return null;
    if (u.username !== "" || u.password !== "") return null;
    const page = new URL(location.href);
    const isSelf = u.origin === page.origin && u.pathname === page.pathname && u.search === page.search;
    return isSelf ? null : u.href;
  } catch {
    return null;
  }
}`;

test("extractMetaURL in background.ts still carries the behavior this mirror models", () => {
  const backgroundSrc = readFileSync(join(import.meta.dir, "..", "src", "background.ts"), "utf8");
  const actual = extractFunctionSource(backgroundSrc, EXTRACT_META_URL_SIGNATURE);
  expect(
    normalizeSource(actual),
    "extractMetaURL's body changed in src/background.ts. Update " +
      "PINNED_EXTRACT_META_URL_SOURCE in this file to match the new body, " +
      "hand-resync extractMetaURLMirror above to the same behavior, then " +
      "re-run this whole suite against internal/landingmeta/testdata/contract.json " +
      "before trusting either implementation again.",
  ).toBe(normalizeSource(PINNED_EXTRACT_META_URL_SOURCE));
});
