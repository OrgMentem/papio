// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { adapters, type AdapterSpec } from "../src/adapters/types";
import { planExecution } from "../src/plan";
import { captureOrigin, fixturePath, parseHTML } from "./harness";
import { Window } from "happy-dom";

const html = readFileSync(fixturePath("figshare", "no-entitlement"), "utf8");
const page = () => parseHTML(html, captureOrigin(html)!);
const spec = () => adapters.find((a) => a.id === "figshare") as AdapterSpec;

test("Figshare's captured unavailable-file banner ends only this repository route", () => {
  expect(spec()).toBeDefined();
  expect(spec().download).toBeUndefined();
  expect(planExecution(page(), spec(), {}, {})).toMatchObject({
    verdict: { kind: "no_entitlement" }, required_consequence: "none",
  });
});

test("Figshare does not infer file availability from an abstract or an incomplete page", () => {
  expect(spec()).toBeDefined();
  for (const selector of [".jbW3L", "meta[name='citation_title']", "[data-id='layout-header'] h1"]) {
    const doc = page();
    doc.querySelector(selector)!.remove();
    doc.body.append("File(s) not publicly available");
    expect(planExecution(doc, spec(), {}, {}).verdict.kind).toBe("unknown");
  }
});

test("Figshare ignores unavailable-file wording outside its unique status heading", () => {
  expect(spec()).toBeDefined();
  const doc = page();
  doc.querySelector("h2")!.textContent = "Files available";
  doc.body.append("This article discusses the phrase File(s) not publicly available.");
  expect(planExecution(doc, spec(), {}, {}).verdict.kind).toBe("unknown");

  const ambiguous = page();
  ambiguous.querySelector(".jbW3L")!.append(ambiguous.querySelector("h2")!.cloneNode(true));
  expect(planExecution(ambiguous, spec(), {}, {}).verdict.kind).toBe("unknown");

  const invalid = structuredClone(spec());
  invalid.classify[0]!.textSelector = "[";
  expect(planExecution(page(), invalid, {}, {}).verdict.kind).toBe("unknown");
});

test("Figshare waits for the status heading even when the body already contains its wording", async () => {
  expect(spec()).toBeDefined();
  const win = new Window({ url: captureOrigin(html)! });
  win.document.write(html);
  win.document.querySelector("h2")!.textContent = "Loading files";
  win.document.body.append("File(s) not publicly available");
  const previous = { document: globalThis.document, MutationObserver: globalThis.MutationObserver };
  Object.assign(globalThis, { document: win.document, MutationObserver: win.MutationObserver });
  const timer = setTimeout(() => {
    win.document.querySelector("h2")!.textContent = "File(s) not publicly available";
  }, 150);
  try {
    expect(await planExecution(null, { ...spec(), settleTimeoutMs: 1000 }, {}, {})).toMatchObject({
      verdict: { kind: "no_entitlement" }, required_consequence: "none",
    });
  } finally {
    clearTimeout(timer);
    Object.assign(globalThis, previous);
    await win.happyDOM.close();
  }
});
