// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { adapters, type DownloadRule } from "../src/adapters/types";
import { executePlannedPageEffect } from "../src/background";
import { planExecution } from "../src/plan";
import { captureOrigin, fixturePath, parseHTML } from "./harness";

const spec = adapters.find((a) => a.id === "iospress")!;
const doi = "10.3233/SHTI000001";
const load = (scenario = "success") => {
  const html = readFileSync(fixturePath("iospress", scenario), "utf8");
  return parseHTML(html, captureOrigin(html)!);
};

test("IOS Press plans only its captured open-access article POST control", () => {
  const doc = load();
  const plan = planExecution(doc, spec, { doi }, {});
  expect(plan.verdict.kind).toBe("article");
  expect("assisted" in plan).toBe(false);
  if ("assisted" in plan) throw new Error(plan.assisted);
  expect(plan.method).toBe("click");
  expect(doc.querySelectorAll(spec.download!.selector)).toHaveLength(1);
});

for (const change of ["doi", "metadata", "license", "control", "form-route", "form-method", "outside-article", "disabled", "duplicate"] as const) {
  test(`IOS Press refuses ${change}`, () => {
    const doc = load();
    const control = doc.querySelector(spec.download!.selector)!;
    if (change === "doi") doc.querySelector("meta[name='citation_doi']")!.setAttribute("content", "10.3233/SHTI000002");
    if (change === "metadata") doc.querySelector("meta[name='citation_doi']")!.remove();
    if (change === "license") doc.querySelector(".openaccesslicense")!.remove();
    if (change === "control") control.remove();
    if (change === "form-route") control.parentElement!.setAttribute("action", "/Download/Supplement");
    if (change === "form-method") control.parentElement!.setAttribute("method", "get");
    if (change === "outside-article") doc.body.append(control.parentElement!);
    if (change === "disabled") control.setAttribute("aria-disabled", "true");
    if (change === "duplicate") control.after(control.cloneNode(true));
    const plan = planExecution(doc, spec, { doi }, {});
    expect("assisted" in plan || plan.required_consequence === "none").toBe(true);
  });
}

test("IOS Press captured book-series page grants no article action", () => {
  const plan = planExecution(load("drift"), spec, { doi }, {});
  expect(plan.verdict.kind).toBe("unknown");
  expect("assisted" in plan || plan.required_consequence === "none").toBe(true);
});

test("IOS Press executes its captured control and refuses changed work or disabled control", async () => {
  const doc = load();
  const plan = planExecution(doc, spec, { doi }, { access_mode: "delegated" });
  if ("assisted" in plan) throw new Error(plan.assisted);
  const control = doc.querySelector(spec.download!.selector)!;
  let clicks = 0;
  control.addEventListener("click", (event) => { event.preventDefault(); clicks++; });
  const values = { document: doc, location: new URL(doc.URL), HTMLElement: doc.defaultView!.HTMLElement };
  const prior = new Map(Object.keys(values).map((key) => [key, Object.getOwnPropertyDescriptor(globalThis, key)]));
  try {
    for (const [key, value] of Object.entries(values)) Object.defineProperty(globalThis, key, { configurable: true, writable: true, value });
    expect(await executePlannedPageEffect(plan, spec.download as DownloadRule)).toEqual({ ok: true });
    expect(clicks).toBe(1);
    control.setAttribute("aria-disabled", "true");
    expect(await executePlannedPageEffect(plan, spec.download as DownloadRule)).toMatchObject({ ok: false });
    control.removeAttribute("aria-disabled");
    doc.querySelector("meta[name='citation_doi']")!.setAttribute("content", "10.3233/SHTI000002");
    expect(await executePlannedPageEffect(plan, spec.download as DownloadRule)).toMatchObject({ ok: false });
    expect(clicks).toBe(1);
  } finally {
    for (const [key, descriptor] of prior) {
      if (descriptor === undefined) Reflect.deleteProperty(globalThis, key);
      else Object.defineProperty(globalThis, key, descriptor);
    }
  }
});
