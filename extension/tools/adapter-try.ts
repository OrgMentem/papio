// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Offline diagnostic for one declarative adapter spec against one captured
// page. Exists because the only way to learn WHY an adapter failed on a real
// provider page used to be: edit the registry, rebuild the extension, reload
// it, and re-drive a live institutional handoff. Measured on the live
// install, 99 of 103 adapter failures were adapters that matched the host and
// then could not classify the page — a purely selector-level problem that
// `interpret` (pure, DOM-only, exported from ../src/adapters/types) can
// diagnose against a stored capture with no browser, daemon, or network:
//
//   bun run adapter:try -- fixtures/tandfonline/success.html --id tandfonline
//   bun run adapter:try -- capture.html --spec draft-adapter.json --expect article
//
// The per-rule table independently re-checks every classify rule's selectors,
// not just the ones `interpret` itself reaches before its first-match-wins
// return — a losing rule still shows exactly which selector cost it the
// match, which is the actual repair signal.

import { readFileSync } from "node:fs";

import { captureOrigin, parseHTML } from "../test/harness";
import { adapters, interpret, type AdapterContext, type AdapterSpec, type ClassifyRule, type DownloadRule } from "../src/adapters/types";

function usage(): never {
  console.error(
    "usage: adapter-try.ts <captured.html> (--id <adapterId> | --spec <spec.json>) " +
      "[--title <t>] [--doi <d>] [--year <y>] [--expect <kind>]",
  );
  return process.exit(2);
}

// ---- argv parsing ---------------------------------------------------------

interface Args {
  htmlPath: string;
  id?: string;
  specPath?: string;
  title?: string;
  doi?: string;
  year?: number;
  expect?: string;
}

function parseArgs(argv: string[]): Args {
  let htmlPath: string | undefined;
  let id: string | undefined;
  let specPath: string | undefined;
  let title: string | undefined;
  let doi: string | undefined;
  let year: number | undefined;
  let expect: string | undefined;

  for (let i = 0; i < argv.length; i++) {
    const tok = argv[i];
    if (tok === undefined) continue;
    const takeValue = (flag: string): string => {
      const value = argv[++i];
      if (value === undefined) {
        console.error(`${flag} requires a value`);
        usage();
      }
      return value;
    };
    switch (tok) {
      case "--id":
        id = takeValue(tok);
        break;
      case "--spec":
        specPath = takeValue(tok);
        break;
      case "--title":
        title = takeValue(tok);
        break;
      case "--doi":
        doi = takeValue(tok);
        break;
      case "--year": {
        const raw = takeValue(tok);
        const parsed = Number(raw);
        if (!Number.isFinite(parsed)) {
          console.error(`--year expects a number, got "${raw}"`);
          usage();
        }
        year = parsed;
        break;
      }
      case "--expect":
        expect = takeValue(tok);
        break;
      default:
        if (tok.startsWith("--")) {
          console.error(`unknown flag: ${tok}`);
          usage();
        }
        if (htmlPath !== undefined) {
          console.error(`unexpected extra positional argument: ${tok}`);
          usage();
        }
        htmlPath = tok;
    }
  }

  if (htmlPath === undefined) {
    console.error("missing required positional argument: <captured.html>");
    usage();
  }
  if (id === undefined && specPath === undefined) {
    console.error("exactly one of --id or --spec is required");
    usage();
  }
  if (id !== undefined && specPath !== undefined) {
    console.error("--id and --spec are mutually exclusive");
    usage();
  }

  return {
    htmlPath,
    ...(id !== undefined ? { id } : {}),
    ...(specPath !== undefined ? { specPath } : {}),
    ...(title !== undefined ? { title } : {}),
    ...(doi !== undefined ? { doi } : {}),
    ...(year !== undefined ? { year } : {}),
    ...(expect !== undefined ? { expect } : {}),
  };
}

// ---- spec / capture loading -------------------------------------------------

function readFileOrExit(path: string, what: string): string {
  try {
    return readFileSync(path, "utf8");
  } catch (err) {
    console.error(`cannot read ${what} "${path}": ${(err as Error).message}`);
    return process.exit(2);
  }
}

/** `--id` resolves a registered spec; `--spec` loads an unregistered draft so
 * a candidate can be iterated before it is ever added to the registry. */
function loadSpec(args: Args): AdapterSpec {
  if (args.id !== undefined) {
    const id = args.id;
    const found = adapters.find((a) => a.id === id);
    if (found === undefined) {
      const known = adapters.map((a) => a.id).join(", ");
      console.error(`no registered adapter with id "${id}" (known: ${known})`);
      return process.exit(2);
    }
    return found;
  }

  const specPath = args.specPath;
  if (specPath === undefined) {
    // parseArgs already enforces exactly one of --id / --spec.
    throw new Error("unreachable: neither --id nor --spec resolved");
  }
  const raw = readFileOrExit(specPath, "spec file");
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch (err) {
    console.error(`spec file "${specPath}" is not valid JSON: ${(err as Error).message}`);
    return process.exit(2);
  }
  const record = parsed as Record<string, unknown> | null;
  if (record === null || typeof record !== "object" || typeof record["id"] !== "string" || !Array.isArray(record["classify"])) {
    console.error(`spec file "${specPath}" does not look like an AdapterSpec (needs "id" and "classify")`);
    return process.exit(2);
  }
  return parsed as AdapterSpec;
}

// ---- per-rule diagnostic ----------------------------------------------------
// Mirrors interpret()'s own rule-matching (types.ts lines ~148-186) exactly,
// but evaluates every rule instead of stopping at the first match, and never
// throws on a malformed selector — a draft spec under active repair is
// expected to have exactly those problems.

interface SelectorCheck {
  selector: string;
  hit: boolean;
  error?: string;
}

function checkSelector(doc: Document, selector: string): SelectorCheck {
  try {
    return { selector, hit: doc.querySelector(selector) !== null };
  } catch (err) {
    return { selector, hit: false, error: (err as Error).message };
  }
}

interface RuleReport {
  all: SelectorCheck[];
  any: SelectorCheck[];
  textAny: { needle: string; hit: boolean }[];
  matched: boolean;
}

function evaluateRule(doc: Document, bodyText: string, rule: ClassifyRule): RuleReport {
  const all = (rule.all ?? []).map((selector) => checkSelector(doc, selector));
  const any = (rule.any ?? []).map((selector) => checkSelector(doc, selector));
  const textAny = (rule.textAny ?? []).map((needle) => ({
    needle,
    // interpret() never lowercases the needle — textAny is documented as
    // already-lowercase static labels matched against lowercased body text.
    hit: bodyText.indexOf(needle) !== -1,
  }));
  const hasCondition = all.length > 0 || any.length > 0 || textAny.length > 0;
  const allOK = all.every((r) => r.hit);
  const anyOK = any.length === 0 || any.some((r) => r.hit);
  const textOK = textAny.length === 0 || textAny.some((r) => r.hit);
  return { all, any, textAny, matched: hasCondition && allOK && anyOK && textOK };
}

function printSelectorChecks(kind: "all" | "any", checks: SelectorCheck[]): void {
  for (const r of checks) {
    const mark = r.error !== undefined ? "ERR " : r.hit ? "HIT " : "MISS";
    const note = r.error !== undefined ? `  (invalid selector: ${r.error})` : "";
    console.log(`        ${kind === "all" ? "all     " : "any     "} ${mark}  ${r.selector}${note}`);
  }
}

function printClassifyRules(doc: Document, spec: AdapterSpec): void {
  const bodyText = doc.body && doc.body.innerText ? doc.body.innerText.toLowerCase() : "";
  const reports = spec.classify.map((rule) => evaluateRule(doc, bodyText, rule));

  let winner: number | null = null;
  for (const [i, report] of reports.entries()) {
    if (report.matched) {
      winner = i;
      break;
    }
  }

  console.log("\nClassify rules (declared order; interpret() stops at the first full match)");
  for (const [i, rule] of spec.classify.entries()) {
    const report = reports[i];
    if (report === undefined) continue;
    const isWinner = winner === i;
    const skipped = winner !== null && winner < i;
    const tag = isWinner
      ? "  <-- WINNER (first match wins; interpret() returns here)"
      : skipped && winner !== null
        ? `  (never evaluated by interpret(): rule [${winner + 1}] already won)`
        : "";
    console.log(`  [${i + 1}] kind=${rule.kind}  ${report.matched ? "MATCHED" : "not matched"}${tag}`);
    printSelectorChecks("all", report.all);
    printSelectorChecks("any", report.any);
    for (const t of report.textAny) console.log(`        textAny  ${t.hit ? "HIT " : "MISS"}  "${t.needle}"`);
    if (report.all.length === 0 && report.any.length === 0 && report.textAny.length === 0) {
      console.log("        (rule declares no all/any/textAny — interpret() always skips it)");
    }
  }
}

// ---- download resolution ----------------------------------------------------
// "href" and "meta" are resolved locally: their extractors (extractDownloadURL,
// extractMetaURL) are private to background.ts, unexported, so they are
// reimplemented here against the same contract. "url"/"api" reuse the
// exported resolveDownloadURL — background.ts imports cleanly in plain bun
// (its chrome.* wiring is gated behind `typeof chrome !== "undefined"`), but
// the reuse is still wrapped in try/catch so a future coupling to chrome.*
// degrades to a clear message instead of crashing this tool. "click" is never
// resolvable offline: it requires a real user gesture against a live page.

function resolveHrefLocally(doc: Document, selector: string): string | null {
  const el = doc.querySelector(selector);
  if (el === null || el.tagName.toUpperCase() !== "A") return null;
  const href = el.getAttribute("href");
  if (href === null) return null;
  try {
    const u = new URL(href, doc.location?.href ?? "https://fixture.local/");
    return u.protocol === "https:" ? u.href : null;
  } catch {
    return null;
  }
}

function resolveMetaLocally(doc: Document, metaName: string): string | null {
  const el = doc.querySelector(`meta[name="${metaName}"]`);
  if (el === null || el.tagName.toUpperCase() !== "META") return null;
  const content = el.getAttribute("content");
  if (content === null) return null;
  try {
    const u = new URL(content, doc.location?.href ?? "https://fixture.local/");
    return u.protocol === "https:" ? u.href : null;
  } catch {
    return null;
  }
}

// resolveDownloadURL's signature is part of the SERIALIZATION CONTRACT on
// interpret in adapters/types.ts (self-contained, chrome.scripting-injected),
// so it is stable enough to name explicitly here without importing the value.
type ResolveDownloadURLFn = (
  selector: string,
  idPattern: string | null,
  urlTemplate: string | null,
  jsonField: string | null,
) => Promise<string | null>;

async function tryResolveViaBackground(
  doc: Document,
  rule: DownloadRule,
): Promise<{ ok: true; url: string | null } | { ok: false; reason: string }> {
  let resolveDownloadURL: ResolveDownloadURLFn;
  try {
    // Dynamic on purpose: background.ts's chrome.* wiring is gated behind
    // `typeof chrome !== "undefined"` today, but that guard is its contract
    // to keep, not this tool's to assume. A static top-level import would
    // abort the whole CLI — including href/meta specs that never touch this
    // module — the instant that guard is ever loosened; the dynamic import
    // confines that risk to url/api resolution alone.
    ({ resolveDownloadURL } = await import("../src/background"));
  } catch (err) {
    return {
      ok: false,
      reason: `background.ts is not importable in a plain bun process: ${(err as Error).message}`,
    };
  }

  // resolveDownloadURL is written to be serialized verbatim into the live
  // page (see the SERIALIZATION CONTRACT note on interpret in
  // adapters/types.ts), so it reads the ambient `document`/`location`
  // globals instead of taking a Document argument. Point those globals at
  // the fixture window for the one call, then put back whatever was there.
  const g = globalThis as unknown as { document?: unknown; location?: unknown };
  const savedDoc = g.document;
  const savedLoc = g.location;
  g.document = doc;
  g.location = doc.location;
  try {
    const url = await resolveDownloadURL(
      rule.selector,
      rule.idPattern ?? null,
      rule.urlTemplate ?? null,
      rule.jsonField ?? null,
    );
    return { ok: true, url };
  } catch (err) {
    return { ok: false, reason: `resolveDownloadURL threw: ${(err as Error).message}` };
  } finally {
    g.document = savedDoc;
    g.location = savedLoc;
  }
}

async function printDownloadResolution(doc: Document, rule: DownloadRule): Promise<void> {
  console.log("\nDownload resolution (verdict is article; spec declares a download rule)");
  console.log(`  method:   ${rule.method}`);
  console.log(`  selector: ${rule.selector}`);
  console.log(`  resolves: ${doc.querySelector(rule.selector) !== null ? "yes" : "no"}`);

  switch (rule.method) {
    case "href": {
      const url = resolveHrefLocally(doc, rule.selector);
      console.log(`  url:      ${url ?? "(none — selector missing, not an <a>, or href not https)"}`);
      break;
    }
    case "meta": {
      const metaName = rule.metaName ?? "citation_pdf_url";
      const url = resolveMetaLocally(doc, metaName);
      console.log(`  metaName: ${metaName}`);
      console.log(`  url:      ${url ?? "(none — meta tag missing or content not https)"}`);
      break;
    }
    case "url":
    case "api": {
      const resolved = await tryResolveViaBackground(doc, rule);
      if (resolved.ok) {
        console.log(`  url:      ${resolved.url ?? "(resolveDownloadURL returned null)"}`);
        if (rule.method === "api") {
          console.log(
            "  note:     method \"api\" fetches urlTemplate live to read jsonField — the call above just performed that fetch against the real endpoint, not a stub.",
          );
        }
      } else {
        console.log(`  url:      not resolvable offline (${resolved.reason})`);
      }
      break;
    }
    case "click":
      console.log('  url:      not resolvable offline (method "click" requires a real user gesture against a live page)');
      break;
  }
}

// ---- main -------------------------------------------------------------------

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2));
  const spec = loadSpec(args);
  const html = readFileOrExit(args.htmlPath, "captured HTML");
  // Parse against the origin the capture was taken from, so a relative PDF
  // href resolves to the URL the extension would really request rather than a
  // fixture-local address that would send someone debugging the wrong host.
  const origin = captureOrigin(html);
  const doc = origin === null ? parseHTML(html) : parseHTML(html, origin);

  const ctx: AdapterContext = {
    expected: {
      ...(args.title !== undefined ? { title: args.title } : {}),
      ...(args.doi !== undefined ? { doi: args.doi } : {}),
      ...(args.year !== undefined ? { year: args.year } : {}),
    },
  };

  const verdict = interpret(doc, spec, ctx);

  console.log(`=== adapter-try: ${spec.id}@${spec.version} vs ${args.htmlPath} ===\n`);
  console.log("Verdict");
  console.log(`  kind:            ${verdict.kind}`);
  console.log(`  adapter_id:      ${verdict.adapter_id}`);
  console.log(`  adapter_version: ${verdict.adapter_version}`);
  console.log("  evidence:");
  for (const line of verdict.evidence) console.log(`    - ${line}`);

  printClassifyRules(doc, spec);

  if (verdict.kind === "article" && spec.download !== undefined) {
    await printDownloadResolution(doc, spec.download);
  }

  console.log("");
  if (args.expect !== undefined) {
    if (verdict.kind === args.expect) {
      console.log(`Result: PASS (--expect ${args.expect} matched)`);
      process.exit(0);
    } else {
      console.log(`Result: FAIL (--expect ${args.expect}, got ${verdict.kind})`);
      process.exit(1);
    }
  } else {
    console.log("Result: interpreted successfully (no --expect given)");
    process.exit(0);
  }
}

await main();
