// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { adapters, type AdapterSpec } from "../src/adapters/types";
import { fixturePath } from "./harness";
import { synthesizeAdapterRepair } from "../tools/adapter-repair";

const mdpi = adapters.find((spec) => spec.id === "mdpi") as AdapterSpec;

test("the top article selector candidate verifies on a committed capture", () => {
  const html = readFileSync(fixturePath("mdpi", "success"), "utf8");
  const result = synthesizeAdapterRepair(html, mdpi, "success", "article");

  expect(result.candidates.length).toBeGreaterThan(0);
  expect(result.candidates[0]?.verified).toBe(true);
});

test("an article capture without a PDF affordance has no verified candidate", () => {
  const html = "<html><head><meta name='citation_doi' content='10.1000/no-pdf'></head><body><main>Abstract only</main></body></html>";
  const result = synthesizeAdapterRepair(html, mdpi, "success", "article");

  expect(result.candidates.filter((candidate) => candidate.verified)).toEqual([]);
});

test("query-string values never become selector candidates", () => {
  const html =
    "<html><head><meta name='citation_doi' content='10.1000/query'></head><body>" +
    "<a data-download='file.pdf?token=secret' href='/file.pdf?token=secret'>Download PDF</a>" +
    "</body></html>";
  const result = synthesizeAdapterRepair(html, mdpi, "success", "article");

  expect(result.candidates.length).toBeGreaterThan(0);
  for (const candidate of result.candidates) expect(candidate.selector).not.toContain("?");
});
