// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { adapters, type AdapterSpec } from "../src/adapters/types";
import { planExecution } from "../src/plan";
import { captureOrigin, fixturePath, parseHTML } from "./harness";

const html = readFileSync(fixturePath("chemrxiv", "success"), "utf8");
const doi = "10.26434/chemrxiv-2026-example-v2";
const page = () => parseHTML(html, captureOrigin(html)!);
const spec = () => adapters.find((a) => a.id === "chemrxiv") as AdapterSpec;
const refuses = (doc: Document) => {
  const planned = planExecution(doc, spec(), { doi }, {});
  expect("assisted" in planned || planned.required_consequence === "none").toBe(true);
};

test("ChemRxiv binds the preprint DOI and one rendered PDF control", () => {
  expect(spec()).toBeDefined();
  const doc = page();
  const planned = planExecution(doc, spec(), { doi }, {});
  expect(planned.verdict.kind).toBe("article");
  expect("assisted" in planned).toBe(false);
  expect(doc.querySelectorAll(spec().download!.selector)).toHaveLength(1);
});

test("ChemRxiv refuses a publication DOI or another preprint version", () => {
  expect(spec()).toBeDefined();
  for (const expected of ["10.1000/example-publication", doi.replace("-v2", "-v1")]) {
    expect("assisted" in planExecution(page(), spec(), { doi: expected }, {})).toBe(true);
  }
});

test("ChemRxiv refuses metadata, hidden controls, and supplements without its PDF affordance", () => {
  expect(spec()).toBeDefined();
  const doc = page();
  doc.querySelector(".info-panel__formats")!.remove();
  refuses(doc);
});

test("ChemRxiv refuses changed PDF targets and absent primary DOI evidence", () => {
  expect(spec()).toBeDefined();
  for (const href of [
    `https://chemrxiv.org/doi/pdf/${doi.replace("-v2", "-v1")}`,
    `https://chemrxiv.org/doi/suppl/${doi}/suppl_file/supporting_information.pdf`,
    `https://elsewhere.example/doi/pdf/${doi}`,
  ]) {
    const doc = page();
    doc.querySelector(spec().download!.selector)!.setAttribute("href", href);
    refuses(doc);
  }
  const doc = page();
  doc.querySelector(".core-self-citation")!.remove();
  refuses(doc);
});

test("ChemRxiv homepage is unknown, not a sign-in wall or article", () => {
  const drift = readFileSync(fixturePath("chemrxiv", "drift"), "utf8");
  const planned = planExecution(parseHTML(drift, captureOrigin(drift)!), spec(), { doi }, {});
  expect(planned.verdict.kind).toBe("unknown");
  expect("assisted" in planned || planned.required_consequence === "none").toBe(true);
});
