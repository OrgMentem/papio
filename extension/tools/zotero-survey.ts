// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//
// Deterministic, network-free static survey of a pinned Zotero translator
// corpus. The only network operation this tool ever performs is the initial
// shallow clone needed to materialise the explicitly pinned revision.

import { execFileSync } from "node:child_process";
import { mkdirSync, readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { join, relative, resolve } from "node:path";

const REPOSITORY = "https://github.com/zotero/translators.git";
// This is deliberately a commit, never a branch name: changing the corpus
// must be an intentional source change.
const PINNED_SHA = "fbee32689eca0d88105ac518c3b7f53bdbdd2508";
const TOOL_ROOT = resolve(import.meta.dir);
const PROJECT_ROOT = resolve(TOOL_ROOT, "../..");
const OUTPUT_ROOT = join(PROJECT_ROOT, "dev", "scratch", "zotero-survey");
const CORPUS_ROOT = join(OUTPUT_ROOT, "corpus");

interface Header {
  id: string;
  label: string;
  target: string | null;
  translatorType: number | null;
  priority: number | null;
  lastUpdated: string | null;
  license: string | null;
  parseError: string | null;
}

interface Extraction {
  method: string;
  selector: string;
  line: number;
}

interface AttachmentPattern {
  mime: string;
  urls: string[];
  line: number;
}

interface TranslatorReport {
  path: string;
  header: Header;
  targetRegexParseable: boolean;
  isWebTranslator: boolean;
  detectWebPresent: boolean;
  detectWebLiteralOnly: boolean;
  detectWebTypes: string[];
  extractionCalls: Extraction[];
  attachmentPatterns: AttachmentPattern[];
  staticPdfAttachment: boolean;
  networkCalls: string[];
  helperCalls: string[];
  requiresNetworkOrHelpers: boolean;
  multiple: { present: boolean; calls: number; literalOnly: boolean };
  verdict: string;
  adjacentPublishers: string[];
}

interface SurveyReport {
  generatedBy: string;
  repository: string;
  pinnedSha: string;
  corpusPath: string;
  totalTranslators: number;
  parseFailures: number;
  metrics: {
    parseableTargetRegexPercent: number;
    webTranslatorCount: number;
    webDetectWebLiteralOnlyPercent: number;
    staticPdfAttachmentPercent: number;
    requiringNetworkOrHelpersPercent: number;
    e1RepresentableCandidateCount: number;
  };
  translators: TranslatorReport[];
  topAdjacentPublishers: TranslatorReport[];
}

function fail(message: string): never {
  console.error(message);
  process.exit(2);
}

function runGit(args: string[]): string {
  try {
    return execFileSync("git", args, { cwd: PROJECT_ROOT, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
  } catch (error) {
    const detail = error instanceof Error ? error.message : String(error);
    fail(`git ${args.join(" ")} failed: ${detail}`);
  }
}

function ensureCorpus(): string {
  mkdirSync(OUTPUT_ROOT, { recursive: true });
  const gitDir = join(CORPUS_ROOT, ".git");
  if (!statExists(gitDir)) {
    runGit(["clone", "--no-tags", "--no-checkout", "--depth", "1", REPOSITORY, CORPUS_ROOT]);
    runGit(["-C", CORPUS_ROOT, "fetch", "--depth", "1", "origin", PINNED_SHA]);
  }
  runGit(["-C", CORPUS_ROOT, "checkout", "--detach", PINNED_SHA]);
  const actual = runGit(["-C", CORPUS_ROOT, "rev-parse", "HEAD"]);
  if (actual !== PINNED_SHA) fail(`corpus resolved to ${actual}, expected pinned ${PINNED_SHA}`);
  if (!/^[0-9a-f]{40}$/.test(actual)) fail(`unexpected corpus revision ${actual}`);
  return actual;
}

function statExists(path: string): boolean {
  try {
    statSync(path);
    return true;
  } catch {
    return false;
  }
}

function translatorFiles(root: string): string[] {
  const found: string[] = [];
  const visit = (directory: string): void => {
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      if (entry.name.startsWith(".") || entry.name === "node_modules") continue;
      const path = join(directory, entry.name);
      if (entry.isDirectory()) visit(path);
      else if (entry.isFile() && entry.name.endsWith(".js")) found.push(path);
    }
  };
  visit(root);
  return found.sort((a, b) => {
    const pathA = relative(root, a);
    const pathB = relative(root, b);
    return pathA < pathB ? -1 : pathA > pathB ? 1 : 0;
  });
}

function lineAt(source: string, offset: number): number {
  let line = 1;
  for (let i = 0; i < offset; i++) if (source[i] === "\n") line++;
  return line;
}

function balancedObject(source: string, start: number): string | null {
  let depth = 0;
  let quote: string | null = null;
  let escaped = false;
  for (let i = start; i < source.length; i++) {
    const char = source[i];
    if (quote !== null) {
      if (escaped) escaped = false;
      else if (char === "\\") escaped = true;
      else if (char === quote) quote = null;
      continue;
    }
    if (char === '"' || char === "'") {
      quote = char;
      continue;
    }
    if (char === "{") depth++;
    else if (char === "}" && --depth === 0) return source.slice(start, i + 1);
  }
  return null;
}

function parseHeader(source: string): Header {
  const start = source.search(/\{/);
  if (start < 0) return { id: "", label: "", target: null, translatorType: null, priority: null, lastUpdated: null, license: null, parseError: "no JSON object" };
  const block = balancedObject(source, start);
  if (block === null) return { id: "", label: "", target: null, translatorType: null, priority: null, lastUpdated: null, license: null, parseError: "unterminated JSON object" };
  try {
    const value = JSON.parse(block) as Record<string, unknown>;
    const stringField = (...names: string[]): string | null => {
      for (const name of names) if (typeof value[name] === "string") return value[name] as string;
      return null;
    };
    const numberField = (...names: string[]): number | null => {
      for (const name of names) if (typeof value[name] === "number") return value[name] as number;
      return null;
    };
    return {
      id: stringField("translatorID", "id") ?? "",
      label: stringField("label", "name") ?? "",
      target: stringField("target"),
      translatorType: numberField("translatorType"),
      priority: numberField("priority"),
      lastUpdated: stringField("lastUpdated"),
      license: stringField("license"),
      parseError: null,
    };
  } catch (error) {
    return { id: "", label: "", target: null, translatorType: null, priority: null, lastUpdated: null, license: null, parseError: error instanceof Error ? error.message : String(error) };
  }
}

function validRegex(pattern: string | null): boolean {
  if (pattern === null || pattern.trim() === "") return false;
  try {
    new RegExp(pattern);
    return true;
  } catch {
    return false;
  }
}

function functionBody(source: string, name: string): { body: string; start: number } | null {
  const match = new RegExp(`(?:function\\s+${name}\\s*\\([^)]*\\)|(?:const|let|var)\\s+${name}\\s*=\\s*function\\s*\\([^)]*\\))\\s*\\{`, "m").exec(source);
  if (match === null || match.index === undefined) return null;
  const open = match.index + match[0].lastIndexOf("{");
  const body = balancedObject(source, open);
  return body === null ? null : { body: body.slice(1, -1), start: open };
}

function quoted(value: string): string | null {
  const trimmed = value.trim();
  if (trimmed.length < 2 || !((trimmed.startsWith('"') && trimmed.endsWith('"')) || (trimmed.startsWith("'") && trimmed.endsWith("'")))) return null;
  try {
    if (trimmed.startsWith('"')) return JSON.parse(trimmed) as string;
    return trimmed.slice(1, -1).replace(/\\(['\\])/g, "$1");
  } catch {
    return null;
  }
}

function detectWebInfo(source: string): { present: boolean; literalOnly: boolean; types: string[] } {
  const found = functionBody(source, "detectWeb");
  if (found === null) return { present: false, literalOnly: false, types: [] };
  const returns = [...found.body.matchAll(/\breturn\s+([^;}]*)/g)].map((match) => match[1]?.trim() ?? "");
  const values = returns.map(quoted);
  const types = [...new Set(values.filter((value): value is string => value !== null))].sort();
  return { present: true, literalOnly: returns.length > 0 && values.every((value) => value !== null), types };
}

function extractionCalls(source: string): Extraction[] {
  const result: Extraction[] = [];
  const pattern = /\b((?:ZU|Zotero\.Utilities)(?:\.xpathText|\.xpath|\.text|\.attr|\.xpathText|\.HTTP\.requestJSON|\.requestJSON|\.doGet|\.doPost|\.HTTP)|xpathText|xpath|text|attr)\s*\(\s*[^,]+,\s*(["'])(.*?)\2/g;
  for (const match of source.matchAll(pattern)) {
    const method = match[1] ?? "";
    if (/HTTP|doGet|doPost|requestJSON/.test(method)) continue;
    result.push({ method, selector: match[3] ?? "", line: lineAt(source, match.index ?? 0) });
  }
  return result.sort((a, b) => a.line - b.line || a.method.localeCompare(b.method));
}

function attachmentPatterns(source: string): AttachmentPattern[] {
  const result: AttachmentPattern[] = [];
  for (const match of source.matchAll(/application\/pdf/gi)) {
    const offset = match.index ?? 0;
    const windowStart = Math.max(0, offset - 700);
    const windowEnd = Math.min(source.length, offset + 700);
    const window = source.slice(windowStart, windowEnd);
    const urls = [...window.matchAll(/\burl\s*:\s*(["'])(.*?)\1/g)].map((url) => url[2] ?? "");
    result.push({ mime: "application/pdf", urls: [...new Set(urls)].sort(), line: lineAt(source, offset) });
  }
  return result.sort((a, b) => a.line - b.line);
}

function networkCalls(source: string): string[] {
  const calls = [...source.matchAll(/\b(?:Zotero\.Utilities\.HTTP(?:\.(?:doGet|doPost|request|requestJSON))?|ZU\.(?:doGet|doPost|requestJSON)|requestJSON)\b/g)].map((match) => match[0]);
  return [...new Set(calls)].sort();
}

function helperCalls(source: string): string[] {
  const known = new Set(["xpath", "xpathText", "text", "attr", "doGet", "doPost", "requestJSON"]);
  const calls = [...source.matchAll(/\bZU\.([A-Za-z_$][\w$]*)\s*\(/g)].map((match) => match[1] ?? "").filter((name) => !known.has(name));
  const zotero = [...source.matchAll(/\bZotero\.Utilities\.([A-Za-z_$][\w$]*)\s*\(/g)].map((match) => match[1] ?? "").filter((name) => !["HTTP"].includes(name));
  return [...new Set([...calls, ...zotero].filter(Boolean))].sort();
}

function multipleInfo(source: string): { present: boolean; calls: number; literalOnly: boolean } {
  const found = functionBody(source, "doWeb");
  const declarations = /\bfunction\s+multiple\s*\(/.test(source) || /\b(?:const|let|var)\s+multiple\s*=/.test(source);
  const body = found?.body ?? "";
  const calls = [...body.matchAll(/\bmultiple\s*\(/g)].length;
  const expressions = [...source.matchAll(/\bmultiple\s*\([^)]*\)/g)].map((m) => m[0]);
  const literalOnly = expressions.every((expression) => /^multiple\s*\(\s*(?:true|false)\s*\)$/.test(expression));
  return { present: declarations || calls > 0, calls, literalOnly };
}

const PUBLISHER_ORDER = ["wiley", "sage", "sciencedirect", "springer", "tandf", "jstor", "proquest", "ebsco"];
const PUBLISHERS: Record<string, string[]> = {
  wiley: ["wiley"],
  sage: ["sage", "sagpub"],
  sciencedirect: ["sciencedirect", "elsevier"],
  springer: ["springer", "link.springer"],
  tandf: ["tandf", "taylorandfrancis", "taylor & francis", "taylor-francis"],
  jstor: ["jstor"],
  proquest: ["proquest"],
  ebsco: ["ebsco"],
};

function adjacentPublishers(header: Header): string[] {
  const haystack = [header.id, header.label, header.target ?? ""].join(" ").toLowerCase();
  return Object.entries(PUBLISHERS).filter(([, names]) => names.some((name) => haystack.includes(name))).map(([publisher]) => publisher);
}

function verdictFor(report: Omit<TranslatorReport, "verdict">): string {
  if (report.header.parseError !== null) return "unparseable-header";
  if (!report.targetRegexParseable) return "no-parseable-target";
  if (!report.isWebTranslator) return "non-web-translator";
  if (!report.detectWebLiteralOnly) return "computed-detectWeb";
  if (!report.staticPdfAttachment) return "no-static-pdf-attachment";
  if (report.requiresNetworkOrHelpers) return "network-or-helper-dependent";
  return "static-e1-candidate";
}

function analyse(path: string): TranslatorReport {
  const source = readFileSync(path, "utf8");
  const header = parseHeader(source);
  const detect = detectWebInfo(source);
  const extraction = extractionCalls(source);
  const attachments = attachmentPatterns(source);
  const network = networkCalls(source);
  const helpers = helperCalls(source);
  const multiple = multipleInfo(source);
  const partial: Omit<TranslatorReport, "verdict"> = {
    path: relative(CORPUS_ROOT, path),
    header,
    targetRegexParseable: validRegex(header.target),
    isWebTranslator: header.translatorType !== null && (header.translatorType & 4) !== 0,
    detectWebPresent: detect.present,
    detectWebLiteralOnly: detect.literalOnly,
    detectWebTypes: detect.types,
    extractionCalls: extraction,
    attachmentPatterns: attachments,
    staticPdfAttachment: attachments.length > 0,
    networkCalls: network,
    helperCalls: helpers,
    requiresNetworkOrHelpers: network.length > 0 || helpers.length > 0,
    multiple,
    adjacentPublishers: adjacentPublishers(header),
  };
  return { ...partial, verdict: verdictFor(partial) };
}

function percent(numerator: number, denominator: number): number {
  return denominator === 0 ? 0 : Number(((numerator / denominator) * 100).toFixed(2));
}

function mdCell(value: unknown): string {
  return String(value).replaceAll("|", "\\|").replaceAll("\n", " ");
}

function markdown(report: SurveyReport): string {
  const web = report.metrics.webTranslatorCount;
  const top = report.topAdjacentPublishers;
  const lines = [
    "# Zotero translator static survey",
    "",
    `- Repository: [zotero/translators](${REPOSITORY})` ,
    `- Pinned commit: \`${report.pinnedSha}\``,
    `- Corpus clone: \`${report.corpusPath}\``,
    "- Method: parse translator JSON headers and inspect JavaScript source text; translator code is never executed.",
    "- Network: only the initial shallow clone may access the network; all analysis and report generation are local.",
    "",
    "## Headline metrics",
    "",
    `- Total translators: **${report.totalTranslators}** (header parse failures: ${report.parseFailures})`,
    `- Parseable target regex: **${report.metrics.parseableTargetRegexPercent}%**`,
    `- Web translators with literal-only detectWeb: **${report.metrics.webDetectWebLiteralOnlyPercent}%** of ${web}`,
    `- Static PDF attachment patterns: **${report.metrics.staticPdfAttachmentPercent}%**`,
    `- Requiring network/helpers: **${report.metrics.requiringNetworkOrHelpersPercent}%**`,
    `- Representable E1 candidates: **${report.metrics.e1RepresentableCandidateCount}**`,
    "",
    "## Top adjacent scholarly publishers",
    "",
    "Ranked by route-table adjacency (publisher name in translator id, label, or target), then path. Verdicts are static evidence only, not runtime authorization.",
    "",
    "| # | Publisher | Translator | Target | PDF | Network/helpers | Verdict |",
    "|---:|---|---|---|:---:|:---:|---|",
  ];
  top.forEach((translator, index) => {
    const publisher = translator.adjacentPublishers.join(", ");
    lines.push(`| ${index + 1} | ${mdCell(publisher)} | ${mdCell(translator.header.label || translator.header.id || translator.path)} | ${mdCell(translator.header.target ?? "—")} | ${translator.staticPdfAttachment ? "yes" : "no"} | ${translator.requiresNetworkOrHelpers ? "yes" : "no"} | ${translator.verdict} |`);
  });
  lines.push("", "## Field definitions", "", "`static-e1-candidate` requires a parseable target, web translator metadata, literal-only `detectWeb`, at least one nearby `application/pdf` attachment pattern, and no statically observed network or unknown helper call. Literal selectors and attachment URL strings are evidence for later review; they do not execute or grant access.", "", "Full per-translator records are in `report.json`.", "");
  return lines.join("\n");
}
function main(): void {
  const pinnedSha = ensureCorpus();
  const reports = translatorFiles(CORPUS_ROOT).map(analyse);
  const web = reports.filter((report) => report.isWebTranslator);
  const topAdjacentPublishers = reports
    .filter((report) => report.adjacentPublishers.length > 0)
    .sort((a, b) => {
      const rankA = Math.min(...a.adjacentPublishers.map((publisher) => PUBLISHER_ORDER.indexOf(publisher)));
      const rankB = Math.min(...b.adjacentPublishers.map((publisher) => PUBLISHER_ORDER.indexOf(publisher)));
      if (rankA !== rankB) return rankA - rankB;
      return a.path < b.path ? -1 : a.path > b.path ? 1 : 0;
    })
    .slice(0, 30);
  const report: SurveyReport = {
    generatedBy: "extension/tools/zotero-survey.ts",
    repository: REPOSITORY,
    pinnedSha,
    corpusPath: "dev/scratch/zotero-survey/corpus",
    totalTranslators: reports.length,
    parseFailures: reports.filter((item) => item.header.parseError !== null).length,
    metrics: {
      parseableTargetRegexPercent: percent(reports.filter((item) => item.targetRegexParseable).length, reports.length),
      webTranslatorCount: web.length,
      webDetectWebLiteralOnlyPercent: percent(web.filter((item) => item.detectWebLiteralOnly).length, web.length),
      staticPdfAttachmentPercent: percent(reports.filter((item) => item.staticPdfAttachment).length, reports.length),
      requiringNetworkOrHelpersPercent: percent(reports.filter((item) => item.requiresNetworkOrHelpers).length, reports.length),
      e1RepresentableCandidateCount: reports.filter((item) => item.verdict === "static-e1-candidate").length,
    },
    translators: reports,
    topAdjacentPublishers,
  };
  mkdirSync(OUTPUT_ROOT, { recursive: true });
  writeFileSync(join(OUTPUT_ROOT, "report.json"), `${JSON.stringify(report, null, 2)}\n`);
  writeFileSync(join(OUTPUT_ROOT, "report.md"), markdown(report));
  console.log(`Surveyed ${report.totalTranslators} translators at ${pinnedSha}`);
  console.log(`E1 representable candidates: ${report.metrics.e1RepresentableCandidateCount}`);
}

main();
