// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Offline diagnostic for one declarative adapter spec against one captured
// page. Exists because the only way to learn WHY an adapter failed on a real
// provider page used to be: edit the registry, rebuild the extension, reload
// it, and re-drive a live institutional handoff. Measured on the live
// install, 99 of 103 adapter failures were adapters that matched the host and
// then could not classify the page — a purely selector-level problem that
// `planExecution` (the production planner the extension injects; DOM-only on
// its Document path, exported from ../src/plan) can diagnose against a stored
// capture with no browser, daemon, or network — by
// default. A spec whose download method is "url" or "api" is the exception:
// resolving it calls the real resolveDownloadURL, which performs a live,
// credentialed `fetch` whenever the rule declares `jsonField`. That branch is
// off unless you pass --allow-network; without it the tool reports the
// method as unresolved instead of touching the network.
//
//   bun run adapter:try -- fixtures/tandfonline/success.html --id tandfonline
//   bun run adapter:try -- capture.html --spec draft-adapter.json --expect article
//
// The per-rule table independently re-checks every classify rule's selectors,
// not just the ones `planExecution` itself reaches before its
// first-match-wins return — a losing rule still shows exactly which selector
// cost it the match, which is the actual repair signal.

import { readFileSync } from "node:fs";

import { captureOrigin, parseHTML, verdictOf } from "../test/harness";
import { adapters, type AdapterSpec, type ClassifyRule, type DownloadRule } from "../src/adapters/types";
import { planExecution, type Plan } from "../src/plan";

function usage(): never {
  console.error(
    "usage: adapter-try.ts <captured.html> (--id <adapterId> | --spec <spec.json>) " +
      "[--title <t>] [--doi <d>] [--year <y>] [--expect <kind>] [--allow-network]",
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
  allowNetwork: boolean;
}

function parseArgs(argv: string[]): Args {
  let htmlPath: string | undefined;
  let id: string | undefined;
  let specPath: string | undefined;
  let title: string | undefined;
  let doi: string | undefined;
  let year: number | undefined;
  let expect: string | undefined;
  let allowNetwork = false;

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
      case "--allow-network":
        allowNetwork = true;
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
    allowNetwork,
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
// Mirrors the rule matching planExecution performs (its `classify` loop in
// ../src/plan.ts) exactly, but evaluates every rule instead of stopping at
// the first match, and never throws on a malformed selector — a draft spec
// under active repair is expected to have exactly those problems.

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
    // planExecution never lowercases the needle — textAny is documented as
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

  console.log("\nClassify rules (declared order; planExecution stops at the first full match)");
  for (const [i, rule] of spec.classify.entries()) {
    const report = reports[i];
    if (report === undefined) continue;
    const isWinner = winner === i;
    const skipped = winner !== null && winner < i;
    const tag = isWinner
      ? "  <-- WINNER (first match wins; planExecution classifies here)"
      : skipped && winner !== null
        ? `  (never evaluated by planExecution: rule [${winner + 1}] already won)`
        : "";
    console.log(`  [${i + 1}] kind=${rule.kind}  ${report.matched ? "MATCHED" : "not matched"}${tag}`);
    printSelectorChecks("all", report.all);
    printSelectorChecks("any", report.any);
    for (const t of report.textAny) console.log(`        textAny  ${t.hit ? "HIT " : "MISS"}  "${t.needle}"`);
    if (report.all.length === 0 && report.any.length === 0 && report.textAny.length === 0) {
      console.log("        (rule declares no all/any/textAny — planExecution always skips it)");
    }
  }
}

// ---- planner outcome --------------------------------------------------------
// Which outcome planExecution produced is the first thing an adapter author
// needs, and there are three of them, not two. Read them off the result's own
// fields — never off a list of verdict kinds — so a verdict kind added later
// is described by the plan it actually produces instead of by a list this
// tool would have to remember to update:
//
//   refused  an AssistedReason and no Plan, so nothing is executable and the
//            planner's own reason says why;
//   passive  composePlan's `base` Plan: no method, no target_ref,
//            required_consequence "none". Every non-article verdict plans
//            exactly this, and for those verdicts it is the correct result,
//            not a failure;
//   action   a Plan with a download method on a resolved target_ref.
//
// A terms verdict also plans `base`, but with a terms-accept target in its
// effect graph — background.ts drives that target when primary_target is
// null — so it is reported on its own rather than as "nothing to execute".
type PlanState =
  | { kind: "refused"; reason: string }
  | { kind: "action"; method: NonNullable<Plan["method"]>; url: string | null }
  | { kind: "terms" }
  | { kind: "passive" }
  | { kind: "incomplete"; detail: string };

function planStateOf(plan: Plan | undefined, assisted: string | null): PlanState {
  if (assisted !== null) return { kind: "refused", reason: assisted };
  if (plan === undefined) return { kind: "refused", reason: "the planner returned no plan" };
  const { method, target_ref: target, url, required_consequence: consequence } = plan;
  if (method !== null && target !== null) return { kind: "action", method, url };
  if (method === null && target === null) {
    if (plan.effect_graph.terms_target !== null) return { kind: "terms" };
    if (consequence === "none") return { kind: "passive" };
  }
  // composePlan sets method, target_ref and required_consequence together, so
  // this shape is not reachable from it today. Report the fields as they are
  // rather than guess which half is missing.
  return {
    kind: "incomplete",
    detail: `method ${method ?? "none"}, target ${target === null ? "none" : "resolved"}, required consequence ${consequence}`,
  };
}

/** The `executable:` line. Short on purpose: it is the line read first.
 * `actionDeclared` says whether the spec itself declares a download action
 * for this verdict, so "this is correct, not a failure" is a statement about
 * the spec rather than an assertion this tool makes on its own. */
function executableLine(state: PlanState, verdictKind: string, actionDeclared: boolean): string {
  switch (state.kind) {
    case "refused":
      return `NO — classified ${verdictKind}, but not executable because ${state.reason}`;
    case "action":
      return `yes — the plan carries a "${state.method}" action on a resolved target`;
    case "terms":
      return "no download action — the plan carries only a terms-accept target";
    case "passive":
      return actionDeclared
        ? `no action — the plan carries no method and no target, yet the spec declares a download action for ${verdictKind}`
        : `no action — the plan carries no method and no target (the spec declares no download action for ${verdictKind}, so this is correct, not a failure)`;
    case "incomplete":
      return `no action — the plan is incomplete (${state.detail})`;
  }
}

/** Why no download URL was resolved, for the states where the planner
 * produced no action. Never a list of hypothetical causes: in every one of
 * these states the real cause is already known and printed above. */
function unresolvedBecause(state: PlanState): string {
  return state.kind === "refused"
    ? "not attempted — the planner stayed assisted (reason above)"
    : "not attempted — the planner planned no download action for this verdict";
}


// resolveDownloadURL is the live API-only resolver. The declarative href/meta
// planning and URL construction above come from planExecution; this helper is
// retained only for --allow-network's JSON-field fetch diagnostic.
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
  // page, so it reads ambient document/location globals rather than taking a
  // Document argument. Point those globals at the fixture window for this call.
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

async function printDownloadResolution(
  doc: Document,
  rule: DownloadRule,
  state: PlanState,
  allowNetwork: boolean,
): Promise<void> {
  console.log("\nDownload resolution (verdict is article; spec declares a download rule)");
  console.log(`  method:   ${rule.method}`);
  console.log(`  selector: ${rule.selector}`);
  console.log(`  resolves: ${doc.querySelector(rule.selector) !== null ? "yes" : "no"}`);

  switch (rule.method) {

    case "href":
    case "meta":
      if (rule.method === "meta") console.log(`  metaName: ${rule.metaName ?? "citation_pdf_url"}`);
      if (state.kind !== "action") {
        // No speculative cause list here. In every non-action state the
        // planner's real reason is already printed above, and naming a
        // missing selector, a non-unique target and a non-https URL would
        // name three causes that are all false: an assisted href/meta plan
        // got as far as a resolved, unique target and never evaluated a URL.
        console.log(`  url:      ${unresolvedBecause(state)}`);
        break;
      }
      // planExecution refuses an href/meta article plan whose URL is not a
      // distinct HTTPS URL, so an action plan for this rule always carries one.
      console.log(`  url:      ${state.url ?? "(none — planExecution planned this action with no URL)"}`);
      break;
    case "url":
    case "api": {
      if (!allowNetwork) {
        console.log(
          `  url:      not resolved (method "${rule.method}" resolves against the live endpoint; re-run with --allow-network)`,
        );
        break;
      }
      if (state.kind !== "action") {
        // Also the gate on the live fetch below: resolving a rule the planner
        // planned no action for would hit a credentialed endpoint for an
        // effect nothing authorized.
        console.log(`  url:      ${unresolvedBecause(state)}`);
        break;
      }
      if (rule.method === "api") {
        // Printed BEFORE the fetch, not after: the whole point of surfacing
        // this is so a reader who did not expect a real network call sees
        // the warning before it happens, not as a footnote once it is done.
        console.log(
          '  note:     method "api" fetches urlTemplate live to read jsonField — about to perform that fetch against the real endpoint, not a stub.',
        );
      }
      const resolved = await tryResolveViaBackground(doc, rule);
      if (resolved.ok) {
        console.log(`  url:      ${resolved.url ?? "(resolveDownloadURL returned null)"}`);
      } else {
        console.log(`  url:      not resolvable offline (${resolved.reason})`);
      }
      break;
    }
    case "click":
      // Even here the planner's own refusal outranks the method-intrinsic
      // reason: with no action planned, no gesture was ever the blocker.
      console.log(
        state.kind === "action"
          ? '  url:      not resolvable offline (method "click" requires a real user gesture against a live page)'
          : `  url:      ${unresolvedBecause(state)}`,
      );
      break;
  }
}

// A capture's `origin` header is attacker-controlled input: this tool is
// meant for "here is my failing page" captures sent by someone else, and the
// resolved origin becomes the parse base that "url"/"api" downstream builds
// a fetch target from. Validate it like any other untrusted URL before using
// it — new URL() plus an explicit https check — rather than trusting the
// regex-extracted string verbatim.
function resolveCaptureOrigin(html: string): string | undefined {
  const origin = captureOrigin(html);
  if (origin === null) return undefined;
  try {
    const u = new URL(origin);
    if (u.protocol !== "https:") {
      console.error(`warning: capture origin "${origin}" is not https — using the default fixture-local base instead`);
      return undefined;
    }
    return origin;
  } catch {
    console.error(`warning: capture origin "${origin}" is not a valid URL — using the default fixture-local base instead`);
    return undefined;
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
  const origin = resolveCaptureOrigin(html);
  const doc = origin === undefined ? parseHTML(html) : parseHTML(html, origin);

  const expected = {
    ...(args.title !== undefined ? { title: args.title } : {}),
    ...(args.doi !== undefined ? { doi: args.doi } : {}),
    ...(args.year !== undefined ? { year: args.year } : {}),
  };

  const planned = planExecution(doc, spec, expected, {});
  // `assisted` is the only authority discriminator, so an assisted result is
  // never executed here: `plan` stays undefined and nothing resolves an
  // action from it. Its verdict is still the classification this same
  // planning pass reached, so report that verdict verbatim — "classified
  // article, but not executable because X" is the repair signal a synthetic
  // `unknown` used to hide.
  const plan = "assisted" in planned ? undefined : planned;
  const assisted = "assisted" in planned ? planned.assisted : null;
  const verdict = verdictOf(planned);
  // One classification of what the planner produced, shared by the
  // `executable:` line and the download-resolution block, so the two can
  // never describe the same result differently.
  const state = planStateOf(plan, assisted);

  console.log(`=== adapter-try: ${spec.id}@${spec.version} vs ${args.htmlPath} ===\n`);
  console.log("Verdict");
  console.log(`  kind:            ${verdict.kind}`);
  console.log(`  adapter_id:      ${verdict.adapter_id}`);
  console.log(`  adapter_version: ${verdict.adapter_version}`);
  console.log("  evidence:");
  for (const line of verdict.evidence) console.log(`    - ${line}`);
  // Read off the spec, not off a verdict-kind list: the spec is what decides
  // whether an action was ever meant to be planned for this classification.
  const actionDeclared = spec.download !== undefined && spec.download.requireKind === verdict.kind;
  console.log(`  executable:      ${executableLine(state, verdict.kind, actionDeclared)}`);

  printClassifyRules(doc, spec);

  if (verdict.kind === "article" && spec.download !== undefined) {
    await printDownloadResolution(doc, spec.download, state, args.allowNetwork);
  }

  console.log("");
  if (args.expect !== undefined) {
    if (verdict.kind !== args.expect) {
      console.log(`Result: FAIL (--expect ${args.expect}, got ${verdict.kind})`);
      process.exit(1);
    }
    if (assisted !== null) {
      // The classification matched but the planner produced no executable
      // action. An assisted result exited non-zero before, because it was
      // reported as `unknown`; keep that non-zero exit now that the real
      // verdict is printed, so a classified-but-unexecutable adapter cannot
      // start passing.
      console.log(`Result: FAIL (--expect ${args.expect} matched the classification, but the plan stayed assisted: ${assisted})`);
      process.exit(1);
    }
    console.log(`Result: PASS (--expect ${args.expect} matched)`);
    process.exit(0);
  }
  // Same three states as the `executable:` line, so the last line a reader
  // sees cannot call a plan with no action "planned successfully". `--expect`
  // still owns the exit code; every branch here exits 0 as before.
  console.log(
    state.kind === "action"
      ? "Result: planned an executable action (no --expect given)"
      : state.kind === "refused"
        ? "Result: planned, but assisted — no executable action (no --expect given)"
        : "Result: planned, no executable action (no --expect given)",
  );
  process.exit(0);
}

await main();
