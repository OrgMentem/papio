// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { adapters, type AdapterSpec } from "../src/adapters/types";
import { synthesizeAdapterRepair } from "../tools/adapter-repair";
import { fixturePath } from "./harness";

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

test("query-string values never become selector candidates", () => {
  const html =
    "<html><head><meta name='citation_doi' content='10.1000/query'></head><body>" +
    "<a data-download='file.pdf?token=secret' href='/file.pdf?token=secret'>Download PDF</a>" +
    "</body></html>";
  const result = synthesizeAdapterRepair(html, REPAIR_SPEC, "success", "article");

  expect(result.candidates.length).toBeGreaterThan(0);
  for (const candidate of result.candidates) expect(candidate.selector).not.toContain("?");
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
