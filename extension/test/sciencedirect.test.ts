// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// ScienceDirect adapter, backed by a live sanitized entitled capture. The
// production layout exposes the article's PDF in its primary access bar while
// a OneTrust cookie overlay remains rendered.

import { expect, test } from "bun:test";

import { adapters, interpret } from "../src/adapters/types";
import { fixtureExists, loadFixture } from "./harness";

const spec = adapters.find((adapter) => adapter.id === "sciencedirect");
if (!spec) throw new Error("sciencedirect spec missing from registry");

const EXPECTED = {
  expected: {
    title: "Student motivation and need satisfaction in GenAI-supported classrooms: A self-determination theory perspective",
  },
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
    const verdict = interpret(doc, spec, EXPECTED);
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
    expect(interpret(fixture("drift"), spec, EXPECTED).kind).toBe("unknown");
  },
);
