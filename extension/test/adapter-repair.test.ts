// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { adapters, type AdapterSpec } from "../src/adapters/types";
import { planExecution } from "../src/plan";
import { synthesizeAdapterRepair } from "../tools/adapter-repair";
import { captureOrigin, fixturePath, parseHTML } from "./harness";

const REPAIR_SPEC: AdapterSpec = {
  id: "repair-test",
  version: "1.0.0",
  hosts: ["example.test"],
  workEvidence: { kind: "doi", selector: "meta[name='citation_doi']", attribute: "content" },
  classify: [{ kind: "article", all: ["meta[name='citation_doi']", ".old-pdf"] }],
  download: { selector: ".old-pdf", requireKind: "article", method: "click", workTarget: { kind: "opaque" } },
};

test("ranking selects the stable article PDF id and the proven missing target", () => {
  const html =
    "<html><head><meta name='citation_doi' content='10.1000/repair'></head><body>" +
    "<button class='download-full-issue'>Download full issue PDF</button>" +
    "<a data-action='citation-download' href='/citation'>Download citation</a>" +
    "<a id='article-pdf' href='/article.pdf'>View article PDF</a>" +
    "</body></html>";
  const result = synthesizeAdapterRepair(html, REPAIR_SPEC, "success", "article");
  const top = result.candidates[0];

  expect(top?.selector).toBe("#article-pdf");
  expect(top?.replace_selector).toBe(".old-pdf");
  expect(top?.classifier_verified).toBe(true);
  expect(top?.plan_complete).toBe(true);
  expect(top?.score).toBeGreaterThan(result.candidates[1]?.score ?? 0);
});

test("complete repairs survive the output limit and do not bind sanitized document tokens", () => {
  const spec = adapters.find((candidate) => candidate.id === "proquest") as AdapterSpec;
  const html = readFileSync(fixturePath("proquest", "drift"), "utf8");
  const result = synthesizeAdapterRepair(html, spec, "drift", "article", 1);
  const top = result.candidates[0];

  expect(top?.plan_complete).toBe(true);
  expect(top?.selector).not.toContain("TOKEN");
  const repaired = structuredClone(spec);
  const rule = repaired.classify[result.rule_index]!;
  rule.all = rule.all!.map((selector) => selector === top!.replace_selector ? top!.selector : selector);
  repaired.download!.selector = top!.selector;
  const live = parseHTML(html.replaceAll("MSTAR_TOKEN", "MSTAR_987654321"), captureOrigin(html)!);
  const title = live.querySelector("h1#documentTitle")!.textContent!.trim();
  const plan = planExecution(live, repaired, { title }, {});

  expect(plan.verdict.kind).toBe("article");
  expect("assisted" in plan).toBe(false);
});

test("an HTML full-text tab cannot outrank the captured ProQuest PDF control", () => {
  const spec = structuredClone(adapters.find((candidate) => candidate.id === "proquest") as AdapterSpec);
  const missing = "a[data-papio-induced-drift='true']";
  spec.classify.find((rule) => rule.kind === "article")!.all = [missing, "h1"];
  spec.download!.selector = missing;
  // Reduced from the 2026-09-19 capture; work identity and session paths redacted.
  const html = `<!-- papio-fixture provider="proquest" scenario="drift" origin="https://www.proquest.com/docview/123456789" captured="2026-09-19T14:51:47Z" -->
    <h1 id="documentTitle">Captured article</h1>
    <a id="addFlashPageParameterformat_fulltext" class="active"
       title="Full text from Medical Database and other databases"
       href="https://www.proquest.com/docview/123456789/fulltext/TOKEN/1">Full text</a>
    <a id="addFlashPageParameterformat_fulltextPDF"
       title="Full text - PDF (Scanned image) from Medical Database and other databases"
       href="https://www.proquest.com/docview/123456789/fulltextPDF/TOKEN/1">Full text - PDF</a>
    <a download="ProQuestDocument.pdf" title="Download PDF" class="tool-option-link pdf-download"
       href="https://media.proquest.com/media/hms/OBJ/TOKEN"
       id="downloadPDFLink_MSTAR_TOKEN" role="button">Download PDF</a>`;
  const result = synthesizeAdapterRepair(html, spec, "drift", "article", 100);

  expect(result.candidates[0]?.selector).toBe("a[id^='downloadPDFLink_MSTAR_']");
  expect(result.candidates[0]?.plan_complete).toBe(true);
  expect(result.candidates.find((candidate) => candidate.selector === "#addFlashPageParameterformat_fulltext"))
    .toMatchObject({ classifier_verified: true, plan_complete: false });
  expect(synthesizeAdapterRepair(html, spec, "drift", "article", 1).candidates[0]?.selector)
    .toBe("a[id^='downloadPDFLink_MSTAR_']");
});

test("generic full-text and citation downloads do not qualify as complete PDF repairs", () => {
  const html = `<meta name="citation_doi" content="10.1000/repair">
    <a id="fulltext" href="/fulltext">Full text</a>
    <a id="citation-download" download="citation.ris" href="/citation">Download citation</a>`;
  const result = synthesizeAdapterRepair(html, REPAIR_SPEC, "drift", "article");

  expect(result.candidates.some((candidate) => candidate.classifier_verified)).toBe(true);
  expect(result.candidates.some((candidate) => candidate.plan_complete)).toBe(false);
});

test("repair candidates exclude redacted values from IDs, attributes, and classes", () => {
  const html =
    "<html><head><meta name='citation_doi' content='10.1000/redacted'></head><body>" +
    "<a id='TOKEN' data-download='TOKEN' class='TOKEN' href='/article.pdf'>Download PDF</a>" +
    "</body></html>";
  const result = synthesizeAdapterRepair(html, REPAIR_SPEC, "drift", "article");

  expect(result.candidates.some((candidate) => candidate.plan_complete)).toBe(true);
  for (const candidate of result.candidates) expect(candidate.selector).not.toContain("TOKEN");
});

test("a missing access check cannot become a complete PDF-control repair", () => {
  const spec = structuredClone(adapters.find((candidate) => candidate.id === "annualreviews") as AdapterSpec);
  spec.classify[0]!.all = spec.classify[0]!.all!.map((selector) =>
    selector === ".article-access.item-meta-data__oa" ? `${selector} .accesstext` : selector);
  const html = readFileSync(fixturePath("annualreviews", "marker-only"), "utf8");
  const result = synthesizeAdapterRepair(html, spec, "drift", "article");

  expect(result.candidates.some((candidate) => candidate.classifier_verified)).toBe(true);
  expect(result.candidates.some((candidate) => candidate.plan_complete)).toBe(false);
});

test("candidate verification preserves higher-priority rule precedence", () => {
  const spec: AdapterSpec = {
    ...REPAIR_SPEC,
    classify: [
      { kind: "login", all: ["#login"] },
      { kind: "article", all: ["meta[name='citation_doi']", ".old-pdf"] },
    ],
  };
  const html =
    "<html><head><meta name='citation_doi' content='10.1000/repair'></head><body>" +
    "<form id='login'><input type='password'></form><a id='article-pdf' href='/article.pdf'>View PDF</a>" +
    "</body></html>";
  const result = synthesizeAdapterRepair(html, spec, "success", "article");

  expect(result.candidates[0]?.selector).toBe("#article-pdf");
  expect(result.candidates[0]?.classifier_verified).toBe(false);
  expect(result.candidates[0]?.plan_complete).toBe(false);
});

test("a matching committed fixture has no fabricated replacement target", () => {
  const spec = adapters.find((candidate) => candidate.id === "mdpi") as AdapterSpec;
  const html = readFileSync(fixturePath("mdpi", "success"), "utf8");
  const result = synthesizeAdapterRepair(html, spec, "success", "article");

  expect(result.candidates.length).toBeGreaterThan(0);
  expect(result.candidates[0]?.classifier_verified).toBe(true);
  expect(result.candidates[0]?.replace_selector).toBeNull();
});

test("an article capture without a PDF affordance has no candidates", () => {
  const html = "<html><head><meta name='citation_doi' content='10.1000/no-pdf'></head><body><main>Abstract only</main></body></html>";
  const result = synthesizeAdapterRepair(html, REPAIR_SPEC, "success", "article");

  expect(result.candidates).toEqual([]);
});

test("PDF route selectors survive layout changes and exclude sibling previews", () => {
  // Reduced from the Oxford chapter capture that ranked a six-level
  // positional selector above the rendered PDF control.
  const html = `<meta name="citation_doi" content="10.1000/repair">
    <div><ul><li><a class="al-link pdf article-pdfLink" href="/chapter/123456789/chapter-ag-pdf/987654321/TOKEN.ag.pdf">PDF</a></li></ul></div>`;
  const spec: AdapterSpec = { ...REPAIR_SPEC, download: { ...REPAIR_SPEC.download!, method: "href" } };
  const top = synthesizeAdapterRepair(html, spec, "drift", "article", 1).candidates[0]!;
  expect(top.plan_complete).toBe(true);
  expect(top.selector).not.toContain("nth-of-type");
  expect(top.selector).toContain("/chapter-ag-pdf/");
  expect(top.selector).not.toContain("TOKEN");
  expect(top.selector).not.toContain("123456789");

  const changed = parseHTML(html.replace("<li>", `<li><a class="al-link pdf article-pdfLink" href="/preview.pdf">PDF preview</a></li><li>`));
  expect(changed.querySelectorAll(top.selector)).toHaveLength(1);
  expect(changed.querySelector(top.selector)?.getAttribute("href")).toContain("/chapter-ag-pdf/");
});

test("query-string values never become selector candidates", () => {
  const html =
    "<html><head><meta name='citation_doi' content='10.1000/query'></head><body>" +
    "<a data-download='file.pdf?token=secret' href='/file.pdf?token=secret'>Download PDF</a>" +
    "</body></html>";
  const result = synthesizeAdapterRepair(html, REPAIR_SPEC, "success", "article");

  expect(result.candidates.length).toBeGreaterThan(0);
  for (const candidate of result.candidates) expect(candidate.selector).not.toContain("?");
});

test("quoted attribute values remain usable repair candidates", () => {
  const html =
    "<html><head><meta name='citation_doi' content='10.1000/quoted'></head><body>" +
    "<a data-action=\"reader's PDF\" href='/article.pdf'>Download PDF</a>" +
    "</body></html>";
  const result = synthesizeAdapterRepair(html, REPAIR_SPEC, "success", "article");
  const candidate = result.candidates.find((item) => item.selector.startsWith("a[data-action="));

  expect(candidate?.replace_selector).toBe(".old-pdf");
  expect(candidate?.plan_complete).toBe(true);
});

test("article discovery includes custom download controls", () => {
  const spec = adapters.find((candidate) => candidate.id === "jstor") as AdapterSpec;
  const html = readFileSync(fixturePath("jstor", "success"), "utf8");
  const result = synthesizeAdapterRepair(html, spec, "success", "article", 50);

  expect(result.candidates.some((candidate) => candidate.outer_html.startsWith("<mfe-download-pharos-button"))).toBe(true);
});

test("capture parsing never attempts subresource requests", () => {
  const attempts: string[] = [];
  const html =
    "<html><head>" +
    "<link rel='stylesheet' href='http://127.0.0.1:1/style.css'>" +
    "<link rel='preload' as='fetch' href='http://127.0.0.1:1/data'>" +
    "</head><body>" +
    "<iframe src='http://127.0.0.1:1/frame'></iframe>" +
    "<meta name='citation_doi' content='10.1000/offline'>" +
    "</body></html>";

  synthesizeAdapterRepair(html, REPAIR_SPEC, "success", "article", 10, (url) => attempts.push(url));

  expect(attempts).toEqual([]);
});
