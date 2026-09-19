// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { readFileSync } from "node:fs";
import { Window } from "happy-dom";

import { adapters, type AdapterSpec, type ClassifyRule, type PageKind } from "../src/adapters/types";
import { planExecution, type ExpectedWork } from "../src/plan";
import { captureOrigin, verdictOf } from "../test/harness";

export type RepairRuleKind = Exclude<PageKind, "unknown">;

export interface SelectorCandidate {
  score: number;
  selector: string;
  outer_html: string;
  classifier_verified: boolean;
  plan_complete: boolean;
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
    node.getAttribute("download") ?? "",
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
        const words = elementWords(node);
        const hasActionAttribute = ["href", "data-href", "onclick", "formaction"].some((name) => node.hasAttribute(name));
        const customControl = tag.includes("-") || tag.includes("button");
        const ownValues = [tag, node.getAttribute("id") ?? "", node.getAttribute("class") ?? "", node.getAttribute("name") ?? "", node.getAttribute("title") ?? "", node.getAttribute("aria-label") ?? ""];
        for (const attr of Array.from(node.attributes)) {
          if (attr.name.startsWith("data-")) ownValues.push(attr.name, attr.value);
        }
        return ARTICLE_WORDS.test(words) && (hasActionAttribute || customControl || ARTICLE_WORDS.test(ownValues.join(" ")));
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
  return value.replace(/[\\'\r\n\f]/g, (char) => `\\${char.charCodeAt(0).toString(16)} `);
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

const DOCUMENT_SCOPED_VALUE = /(?:TOKEN|[a-f0-9]{16,}|\d{7,})/i;

function selectorsFor(node: Element): RankedSelector[] {
  const result: RankedSelector[] = [];
  const id = node.getAttribute("id")?.trim() ?? "";
  if (id !== "" && !id.includes("?")) {
    if (!DOCUMENT_SCOPED_VALUE.test(id)) {
      result.push({ score: 100, selector: `#${cssIdentifier(id)}`, node });
    } else {
      // The sanitizer preserves semantic ID prefixes but masks the record suffix.
      // An exact TOKEN selector could pass the fixture and never match a live page.
      const prefix = /^(.+[_-])TOKEN$/.exec(id)?.[1];
      if (prefix !== undefined && !DOCUMENT_SCOPED_VALUE.test(prefix)) {
        result.push({ score: 85, selector: `${node.tagName.toLowerCase()}[id^='${cssString(prefix)}']`, node });
      }
    }
  }

  for (const attr of Array.from(node.attributes)) {
    if (!attr.name.startsWith("data-") || attr.value.trim() === "" || attr.value.includes("?") || DOCUMENT_SCOPED_VALUE.test(attr.value)) continue;
    result.push({
      score: 80,
      selector: `${node.tagName.toLowerCase()}[${attr.name}='${cssString(attr.value)}']`,
      node,
    });
  }

  const tag = node.tagName.toLowerCase();
  const name = node.getAttribute("name")?.trim() ?? "";
  if (name !== "" && !name.includes("?") && !DOCUMENT_SCOPED_VALUE.test(name) && ["meta", "input", "form"].includes(tag)) {
    result.push({ score: 78, selector: `${tag}[name='${cssString(name)}']`, node });
  }

  result.push({ score: 60, selector: stablePath(node), node });

  const classes = Array.from(node.classList).filter((value) => value !== "" && !value.includes("?") && !DOCUMENT_SCOPED_VALUE.test(value));
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
  return null;
}

function candidateSpec(spec: AdapterSpec, ruleIndex: number, replaceSelector: string | null, candidate: string): AdapterSpec {
  const sourceRule = spec.classify[ruleIndex];
  if (sourceRule === undefined) throw new Error(`rule index ${ruleIndex} is unavailable`);
  if (replaceSelector === null) return structuredClone(spec);
  const replace = (values: string[]): string[] =>
    values.map((value) => (value === replaceSelector ? candidate : value));
  const rule: ClassifyRule = { kind: sourceRule.kind };
  if (sourceRule.all !== undefined) rule.all = replace(sourceRule.all);
  if (sourceRule.any !== undefined) rule.any = replace(sourceRule.any);
  if (sourceRule.textAny !== undefined) rule.textAny = [...sourceRule.textAny];
  if (sourceRule.deferUntilDeadline !== undefined) rule.deferUntilDeadline = sourceRule.deferUntilDeadline;

  const trial = structuredClone(spec);
  trial.classify[ruleIndex] = rule;
  if (trial.download?.selector === replaceSelector) trial.download.selector = candidate;
  if (trial.download?.shadowSelector === replaceSelector) trial.download.shadowSelector = candidate;
  if (trial.download?.postClickWaitFor === replaceSelector) trial.download.postClickWaitFor = candidate;
  if (trial.download?.followupSelector === replaceSelector) trial.download.followupSelector = candidate;
  if (trial.download?.workTarget?.selector === replaceSelector) trial.download.workTarget.selector = candidate;
  if (trial.workEvidence?.selector === replaceSelector) trial.workEvidence.selector = candidate;
  if (trial.termsAccept?.modalSelector === replaceSelector) trial.termsAccept.modalSelector = candidate;
  if (trial.termsAccept?.control === replaceSelector) trial.termsAccept.control = candidate;
  return trial;
}

function expectedWorkFor(doc: Document, spec: AdapterSpec): ExpectedWork {
  const contract = spec.workEvidence;
  if (contract === undefined) return {};
  const node = doc.querySelector(contract.selector);
  if (node === null) return {};
  let value = contract.attribute === undefined ? node.textContent?.trim() ?? "" : node.getAttribute(contract.attribute)?.trim() ?? "";
  if (value === "") return {};
  if (contract.pattern !== undefined) {
    const match = new RegExp(contract.pattern).exec(value);
    value = match?.[1]?.trim() ?? "";
  }
  if (value === "") return {};
  return contract.kind === "doi" ? { doi: value } : { title: value };
}

function truncateOuterHTML(node: Element): string {
  const compact = node.outerHTML.replace(/\s+/g, " ").trim();
  return compact.length <= 320 ? compact : `${compact.slice(0, 317)}...`;
}

function parseOfflineCapture(html: string, baseURL: string, onFetchAttempt?: (url: string) => void): Document {
  const window = new Window({
    url: baseURL,
    settings: {
      disableJavaScriptFileLoading: true,
      disableJavaScriptEvaluation: true,
      disableCSSFileLoading: true,
      disableIframePageLoading: true,
      disableComputedStyleRendering: true,
    },
  });
  window.fetch = ((input: unknown) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : String(input);
    onFetchAttempt?.(url);
    return Promise.reject(new Error(`network access is disabled while parsing adapter captures: ${url}`));
  }) as typeof window.fetch;
  window.document.write(html);
  return window.document as unknown as Document;
}

export function synthesizeAdapterRepair(
  html: string,
  spec: AdapterSpec,
  scenario: string,
  ruleKind: RepairRuleKind,
  limit = 10,
  onFetchAttempt?: (url: string) => void,
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
  const doc = parseOfflineCapture(html, base, onFetchAttempt);
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
  const expected = expectedWorkFor(doc, spec);
  const hasFixtureIdentity = ruleKind !== "article" || expected.doi !== undefined || expected.title !== undefined;
  // A PDF control proves nothing about a separate missing access/identity
  // check. Such candidates may classify, but must not unlock a source patch.
  const repairsDeclaredTarget = ruleKind !== "article" || replaceSelector === null ||
    replaceSelector === spec.download?.selector ||
    replaceSelector === spec.download?.shadowSelector ||
    replaceSelector === spec.download?.postClickWaitFor ||
    replaceSelector === spec.download?.followupSelector ||
    replaceSelector === spec.download?.workTarget?.selector;

  const candidates = Array.from(bySelector.values())
    .map((item): SelectorCandidate => {
      const trial = candidateSpec(spec, ruleIndex, replaceSelector, item.selector);
      const planned = planExecution(doc, trial, expected, {});
      const classifierVerified = verdictOf(planned).kind === ruleKind;
      // An executable same-origin href can still be the HTML "Full text" tab
      // or a citation export. Only PDF-specific affordances can unlock an
      // article repair proposal; live bytes still need maintainer verification.
      const hasPDFAffordance = ruleKind !== "article" || /pdf/i.test(elementWords(item.node));
      // A PDF viewer tab may have a stable id while the actual download has a
      // document-scoped one. Prefer the explicit download before id stability.
      const explicitPDFDownload = ruleKind === "article" && (
        /\.pdf$/i.test(item.node.getAttribute("download") ?? "") ||
        [item.node.textContent, ...["title", "aria-label", "id", "class"].map((name) => item.node.getAttribute(name))]
          .some((value) => /(?:download[\s_-]*pdf|pdf[\s_-]*download)/i.test(value ?? ""))
      );
      return {
        score: item.score + (explicitPDFDownload ? 100 : 0),
        selector: item.selector,
        outer_html: truncateOuterHTML(item.node),
        classifier_verified: classifierVerified,
        plan_complete: classifierVerified && hasFixtureIdentity && repairsDeclaredTarget && hasPDFAffordance && !("assisted" in planned),
        replace_selector: replaceSelector,
      };
    })
    // A lexical score cannot prove that a control downloads the article. Verify
    // before limiting output, or generic full-text controls can hide every repair.
    .sort((a, b) => Number(b.plan_complete) - Number(a.plan_complete) ||
      Number(b.classifier_verified) - Number(a.classifier_verified) ||
      b.score - a.score || a.selector.localeCompare(b.selector))
    .slice(0, Math.max(0, limit));

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
