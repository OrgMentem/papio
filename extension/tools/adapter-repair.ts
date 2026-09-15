// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { readFileSync } from "node:fs";

import { adapters, type AdapterSpec, type ClassifyRule, type PageKind } from "../src/adapters/types";
import { planExecution } from "../src/plan";
import { captureOrigin, parseHTML, verdictOf } from "../test/harness";

export type RepairRuleKind = Exclude<PageKind, "unknown">;

export interface SelectorCandidate {
  score: number;
  selector: string;
  outer_html: string;
  verified: boolean;
  replace_selector: string | null;
}

export interface AdapterRepairOutput {
  provider: string;
  scenario: string;
  rule_kind: RepairRuleKind;
  rule_index: number;
  candidates: SelectorCandidate[];
}

interface RankedSelector {
  score: number;
  selector: string;
  node: Element;
}

const ARTICLE_WORDS = /(?:pdf|download|full[\s_-]*text)/i;
const LOGIN_WORDS = /(?:sign[\s_-]*in|log[\s_-]*in|login|institution|password)/i;
const TERMS_WORDS = /(?:accept|agree|consent|terms)/i;
const PAYWALL_WORDS = /(?:paywall|purchase|subscribe|subscription|required|no[\s_-]*(?:access|entitlement)|not[\s_-]*available|access[\s_-]*denied|get[\s_-]*access)/i;

function elementWords(node: Element): string {
  const values = [
    node.tagName,
    node.textContent ?? "",
    node.getAttribute("id") ?? "",
    node.getAttribute("class") ?? "",
    node.getAttribute("name") ?? "",
    node.getAttribute("title") ?? "",
    node.getAttribute("aria-label") ?? "",
    node.getAttribute("role") ?? "",
    node.getAttribute("href") ?? "",
    node.getAttribute("action") ?? "",
    node.getAttribute("type") ?? "",
  ];
  for (const attr of Array.from(node.attributes)) {
    if (attr.name.startsWith("data-")) values.push(attr.name, attr.value);
  }
  return values.join(" ");
}

function semanticNodes(doc: Document, kind: RepairRuleKind): Element[] {
  const all = Array.from(doc.querySelectorAll("*"));
  switch (kind) {
    case "article":
      return all.filter((node) => {
        const tag = node.tagName.toLowerCase();
        if (tag === "meta" && node.getAttribute("name")?.toLowerCase() === "citation_pdf_url") return true;
        if (tag === "a") {
          const href = node.getAttribute("href") ?? "";
          let pathname = href;
          try {
            pathname = new URL(href, doc.location?.href ?? "https://fixture.local/").pathname;
          } catch {
            pathname = href.split("?", 1)[0] ?? href;
          }
          return ARTICLE_WORDS.test(`${pathname} ${elementWords(node)}`);
        }
        return (tag === "button" || node.getAttribute("role") === "button" || tag === "input") && ARTICLE_WORDS.test(elementWords(node));
      });
    case "login":
      return all.filter((node) => {
        const tag = node.tagName.toLowerCase();
        if (tag === "form") {
          return node.querySelector('input[type="password"]') !== null || LOGIN_WORDS.test(elementWords(node));
        }
        return (tag === "a" || tag === "button" || tag === "input") && LOGIN_WORDS.test(elementWords(node));
      });
    case "terms":
      return all.filter((node) => {
        const tag = node.tagName.toLowerCase();
        return (tag === "button" || tag === "input" || tag === "a") && TERMS_WORDS.test(elementWords(node));
      });
    case "no_entitlement":
      return all.filter((node) => PAYWALL_WORDS.test(elementWords(node)));
    case "wrong_work_check":
      return all.filter((node) => {
        const tag = node.tagName.toLowerCase();
        const name = node.getAttribute("name")?.toLowerCase() ?? "";
        return (
          tag === "h1" ||
          (tag === "meta" && ["citation_title", "citation_doi", "publication_doi", "prism.doi"].includes(name)) ||
          node.hasAttribute("data-doi")
        );
      });
  }
}

function cssIdentifier(value: string): string {
  return value.replace(/(^-?\d)|[^a-zA-Z0-9_-]/g, (match, leadingDigit: string | undefined) => {
    if (leadingDigit !== undefined) return `\\3${match} `;
    return `\\${match}`;
  });
}

function cssString(value: string): string {
  return value.replace(/\\/g, "\\\\").replace(/'/g, "\\'").replace(/[\r\n\f]/g, " ");
}

function stablePath(node: Element): string {
  const parts: string[] = [];
  let current: Element | null = node;
  while (current !== null && parts.length < 6) {
    const tag = current.tagName.toLowerCase();
    const parent: Element | null = current.parentElement;
    let position = 1;
    if (parent !== null) {
      for (const sibling of Array.from(parent.children)) {
        if (sibling === current) break;
        if (sibling.tagName === current.tagName) position += 1;
      }
    }
    parts.unshift(`${tag}:nth-of-type(${position})`);
    if (tag === "html") break;
    current = parent;
  }
  return parts.join(" > ");
}

function selectorsFor(node: Element): RankedSelector[] {
  const result: RankedSelector[] = [];
  const id = node.getAttribute("id")?.trim() ?? "";
  if (id !== "" && !id.includes("?")) {
    const tokenPenalty = /(?:TOKEN|[a-f0-9]{16,}|\d{7,})/i.test(id) ? 35 : 0;
    result.push({ score: 100 - tokenPenalty, selector: `#${cssIdentifier(id)}`, node });
  }

  for (const attr of Array.from(node.attributes)) {
    if (!attr.name.startsWith("data-") || attr.value.trim() === "" || attr.value.includes("?")) continue;
    const valuePenalty = /(?:TOKEN|[a-f0-9]{16,})/i.test(attr.value) ? 25 : 0;
    result.push({
      score: 80 - valuePenalty,
      selector: `${node.tagName.toLowerCase()}[${attr.name}='${cssString(attr.value)}']`,
      node,
    });
  }

  const tag = node.tagName.toLowerCase();
  const name = node.getAttribute("name")?.trim() ?? "";
  if (name !== "" && !name.includes("?") && ["meta", "input", "form"].includes(tag)) {
    result.push({ score: 78, selector: `${tag}[name='${cssString(name)}']`, node });
  }

  result.push({ score: 60, selector: stablePath(node), node });

  const classes = Array.from(node.classList).filter((value) => value !== "" && !value.includes("?"));
  for (const className of classes.slice(0, 4)) {
    result.push({
      score: Math.max(10, 42 - Math.max(0, classes.length - 1) * 6),
      selector: `${tag}.${cssIdentifier(className)}`,
      node,
    });
  }
  return result;
}

function replacementTarget(doc: Document, rule: ClassifyRule): string | null {
  for (const selector of rule.all ?? []) {
    try {
      if (doc.querySelector(selector) === null) return selector;
    } catch {
      return selector;
    }
  }
  if ((rule.any?.length ?? 0) > 0) {
    let anyMatches = false;
    for (const selector of rule.any ?? []) {
      try {
        if (doc.querySelector(selector) !== null) anyMatches = true;
      } catch {
        // A malformed selector cannot satisfy the rule.
      }
    }
    if (!anyMatches) return rule.any?.[0] ?? null;
  }
  return rule.all?.[0] ?? rule.any?.[0] ?? null;
}

function candidateSpec(spec: AdapterSpec, ruleIndex: number, replaceSelector: string | null, candidate: string): AdapterSpec {
  const sourceRule = spec.classify[ruleIndex];
  if (sourceRule === undefined) throw new Error(`rule index ${ruleIndex} is unavailable`);
  const replace = (values: string[]): string[] =>
    values.map((value) => (replaceSelector !== null && value === replaceSelector ? candidate : value));
  const rule: ClassifyRule = { kind: sourceRule.kind };
  if (sourceRule.all !== undefined) rule.all = replace(sourceRule.all);
  if (sourceRule.any !== undefined) rule.any = replace(sourceRule.any);
  if (sourceRule.textAny !== undefined) rule.textAny = [...sourceRule.textAny];
  if (sourceRule.deferUntilDeadline !== undefined) rule.deferUntilDeadline = sourceRule.deferUntilDeadline;
  if (replaceSelector === null) rule.all = [...(rule.all ?? []), candidate];
  return { ...spec, classify: [rule] };
}

function truncateOuterHTML(node: Element): string {
  const compact = node.outerHTML.replace(/\s+/g, " ").trim();
  return compact.length <= 320 ? compact : `${compact.slice(0, 317)}...`;
}

export function synthesizeAdapterRepair(
  html: string,
  spec: AdapterSpec,
  scenario: string,
  ruleKind: RepairRuleKind,
  limit = 10,
): AdapterRepairOutput {
  const origin = captureOrigin(html);
  let base = "https://fixture.local/";
  if (origin !== null) {
    try {
      const parsed = new URL(origin);
      if (parsed.protocol === "https:") base = origin;
    } catch {
      // The diagnostic stays offline and uses the fixture-local base.
    }
  }
  const doc = parseHTML(html, base);
  const ruleIndex = spec.classify.findIndex((rule) => rule.kind === ruleKind);
  if (ruleIndex < 0) throw new Error(`adapter ${spec.id} has no ${ruleKind} rule`);
  const rule = spec.classify[ruleIndex] as ClassifyRule;
  const replaceSelector = replacementTarget(doc, rule);
  const ranked = semanticNodes(doc, ruleKind).flatMap(selectorsFor);
  const bySelector = new Map<string, RankedSelector>();
  for (const item of ranked) {
    if (item.selector === "" || item.selector.includes("?") || !doc.querySelector(item.selector)) continue;
    const prior = bySelector.get(item.selector);
    if (prior === undefined || item.score > prior.score) bySelector.set(item.selector, item);
  }

  const candidates = Array.from(bySelector.values())
    .sort((a, b) => b.score - a.score || a.selector.localeCompare(b.selector))
    .slice(0, Math.max(0, limit))
    .map((item): SelectorCandidate => {
      const trial = candidateSpec(spec, ruleIndex, replaceSelector, item.selector);
      const verdict = verdictOf(planExecution(doc, trial, {}, {}));
      return {
        score: item.score,
        selector: item.selector,
        outer_html: truncateOuterHTML(item.node),
        verified: verdict.kind === ruleKind,
        replace_selector: replaceSelector,
      };
    });

  return { provider: spec.id, scenario, rule_kind: ruleKind, rule_index: ruleIndex, candidates };
}

interface Args {
  htmlPath: string;
  id: string;
  scenario: string;
  ruleKind: RepairRuleKind;
}

function usage(message?: string): never {
  if (message !== undefined) console.error(message);
  console.error("usage: adapter-repair.ts <capture.html> --id <provider> --scenario <scenario> --rule-kind <kind>");
  return process.exit(2);
}

function parseArgs(argv: string[]): Args {
  const htmlPath = argv[0];
  if (htmlPath === undefined || htmlPath.startsWith("--")) usage("missing capture path");
  let id: string | undefined;
  let scenario: string | undefined;
  let ruleKind: RepairRuleKind | undefined;
  for (let i = 1; i < argv.length; i += 1) {
    const flag = argv[i];
    const value = argv[i + 1];
    if (value === undefined) usage(`${flag ?? "flag"} requires a value`);
    if (flag === "--id") id = value;
    else if (flag === "--scenario") scenario = value;
    else if (flag === "--rule-kind") {
      if (!["article", "login", "terms", "no_entitlement", "wrong_work_check"].includes(value)) {
        usage(`unsupported rule kind: ${value}`);
      }
      ruleKind = value as RepairRuleKind;
    } else usage(`unknown flag: ${flag}`);
    i += 1;
  }
  if (id === undefined || scenario === undefined || ruleKind === undefined) usage();
  return { htmlPath, id, scenario, ruleKind };
}

function main(): void {
  const args = parseArgs(process.argv.slice(2));
  const spec = adapters.find((item) => item.id === args.id);
  if (spec === undefined) usage(`no registered adapter with id "${args.id}"`);
  const html = readFileSync(args.htmlPath, "utf8");
  process.stdout.write(`${JSON.stringify(synthesizeAdapterRepair(html, spec, args.scenario, args.ruleKind), null, 2)}\n`);
}

if (import.meta.main) main();
