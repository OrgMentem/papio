// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { planGeneric } from "../src/plan";
import { captureOrigin, fixturePath, parseHTML } from "./harness";

const PMC_DOI = "10.1016/j.cpr.2013.09.002";
const PMC_ARTICLE = "https://pmc.ncbi.nlm.nih.gov/articles/PMC4109031/";
const PMC_PDF = `${PMC_ARTICLE}pdf/nihms583262.pdf`;

function capturedDocument(provider: string, scenario: string): Document {
  const html = readFileSync(fixturePath(provider, scenario), "utf8");
  const origin = captureOrigin(html);
  if (origin === null) throw new Error(`${provider} ${scenario} fixture lacks a capture origin`);
  return parseHTML(html, origin);
}

test("the captured PMC article yields its page-authored PDF for the exact requested DOI", () => {
  const doc = capturedDocument("pmc", "success");
  const planned = planGeneric(
    doc,
    {
      doi: PMC_DOI,
      title: "Mechanisms of change in interpersonal therapy (IPT)",
      year: 2013,
    },
    { access_mode: "delegated" },
  );

  expect(planned.candidates.map((candidate) => candidate.url)).toEqual([PMC_PDF]);
});

test("the PMC generic route refuses missing and wrong requested DOI identity", () => {
  const doc = capturedDocument("pmc", "success");

  const wrong = planGeneric(
    doc,
    { doi: "10.1016/j.cpr.2013.09.003" },
    { access_mode: "delegated" },
  );
  expect(wrong.candidates).toEqual([]);

  const titleOnly = planGeneric(
    doc,
    { title: "Mechanisms of change in interpersonal therapy (IPT)" },
    { access_mode: "delegated" },
  );
  expect(titleOnly.candidates).toEqual([]);
});

test("the captured Europe PMC JSON error cannot become a PDF download", () => {
  const failed = capturedDocument("europepmc", "api-error");
  const result = planGeneric(failed, { doi: PMC_DOI }, { access_mode: "delegated" });

  expect(result.candidates).toEqual([]);
});
