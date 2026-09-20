// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// ScienceDirect adapter, backed by a live sanitized entitled capture. The
// production layout exposes the article's PDF in its primary access bar while
// a OneTrust cookie overlay remains rendered.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { planExecution } from "../src/plan";

import { adapters } from "../src/adapters/types";
import { captureOrigin, classifyFixture, fixtureExists, fixturePath, loadFixture, parseHTML } from "./harness";

const spec = adapters.find((adapter) => adapter.id === "sciencedirect");
if (!spec) throw new Error("sciencedirect spec missing from registry");

const EXPECTED = {
  title: "Student motivation and need satisfaction in GenAI-supported classrooms: A self-determination theory perspective",
};

function fixture(scenario: string): Document {
  const doc = loadFixture("sciencedirect", scenario);
  if (!doc) throw new Error(`missing sciencedirect ${scenario} fixture`);
  return doc;
}

test.skipIf(!fixtureExists("sciencedirect", "success"))(
  "entitled ScienceDirect article exposes its primary PDF viewer control",
  () => {
    const doc = fixture("success");
    const verdict = classifyFixture(doc, spec, EXPECTED);
    expect(verdict.kind).toBe("article");
    expect(verdict.adapter_id).toBe("sciencedirect");
    expect(verdict.evidence).toEqual(["rule:article matched", "title-token-check passed"]);
    // The live page has no citation_pdf_url. The visible cookie overlay does
    // not remove the provider-owned control; activate that control so the
    // extension can adopt the PDF viewer opened by ScienceDirect.
    expect(doc.querySelector("meta[name='citation_pdf_url']")).toBeNull();
    expect(doc.querySelector("#onetrust-banner-sdk")).not.toBeNull();
    expect(spec.download?.method).toBe("click");
    const link = doc.querySelector(spec.download?.selector ?? "");
    expect(link?.getAttribute("href")).toBe("/science/article/pii/S2666557326000194/pdfft");
  },
);

test.skipIf(!fixtureExists("sciencedirect", "drift"))(
  "renamed ScienceDirect primary PDF control fails closed to unknown",
  () => {
    expect(classifyFixture(fixture("drift"), spec, EXPECTED).kind).toBe("unknown");
  },
);

for (const capture of ["success", "subscription", "open-access"]) {
  for (const state of ["enabled", "omitted-aria", "aria-disabled", "disabled-attribute", "missing", "duplicate", "wrong-work"] as const) {
    test(`ScienceDirect click guard: ${capture}, ${state}`, () => {
      const html = readFileSync(fixturePath("sciencedirect", capture), "utf8");
      const origin = captureOrigin(html);
      if (origin === null) throw new Error("ScienceDirect capture has no origin");
      const doc = parseHTML(html, origin);
      const control = doc.querySelector(spec.download!.selector)!;
      expect(control.getAttribute("aria-disabled")).toBe("false");
      expect(control.hasAttribute("disabled")).toBe(false);
      const doi = doc.querySelector("meta[name='citation_doi']")!.getAttribute("content")!;
      if (state === "omitted-aria") control.removeAttribute("aria-disabled");
      if (state === "aria-disabled") control.setAttribute("aria-disabled", "true");
      if (state === "disabled-attribute") control.setAttribute("disabled", "");
      if (state === "missing") control.remove();
      if (state === "duplicate") control.after(control.cloneNode(true));
      const plan = planExecution(doc, spec, {
        doi: state === "wrong-work" ? "10.1016/example-wrong-work" : doi,
      }, { access_mode: "delegated" });
      if (state === "enabled" || state === "omitted-aria") {
        expect("assisted" in plan).toBe(false);
        if ("assisted" in plan) throw new Error(plan.assisted);
        expect(plan.verdict.kind).toBe("article");
        expect(plan.method).toBe("click");
        expect(plan.expected_work.doi?.normalized).toBe(doi);
      } else {
        if (state === "wrong-work") expect(plan.verdict.kind).toBe("wrong_work");
        else if (state === "duplicate") expect("assisted" in plan).toBe(true);
        else expect(plan.verdict.kind).toBe("unknown");
        expect("assisted" in plan || plan.method === null).toBe(true);
      }
      expect(spec.requiresVisible).toBe(true);
    });
  }
}
