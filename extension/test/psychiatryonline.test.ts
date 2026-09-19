// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PsychiatryOnline adapter regression coverage against sanitized live captures.
// The successful capture is the requested article after manual navigation. The
// drift capture is the journal landing reached by the no-click resolver route.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { adapters } from "../src/adapters/types";
import { planExecution } from "../src/plan";
import { captureOrigin, fixtureExists, fixturePath, parseHTML } from "./harness";

const spec = adapters.find((adapter) => adapter.id === "psychiatryonline");
if (!spec) throw new Error("psychiatryonline spec missing from registry");

const REQUESTED = {
  doi: "10.1176/appi.ajp.161.4.598",
  year: 2004,
};

function fixture(scenario: string): Document {
  const html = readFileSync(fixturePath("psychiatryonline", scenario), "utf8");
  const origin = captureOrigin(html);
  if (!origin) throw new Error(`psychiatryonline ${scenario} fixture has no capture origin`);
  return parseHTML(html, origin);
}

test.skipIf(!fixtureExists("psychiatryonline", "target-article"))(
  "captured PsychiatryOnline article produces a DOI-bound direct PDF plan",
  () => {
    const result = planExecution(fixture("target-article"), spec, REQUESTED, { access_mode: "delegated" });

    expect("assisted" in result).toBe(false);
    if ("assisted" in result) throw new Error(result.assisted);
    expect(result.verdict.kind).toBe("article");
    expect(result.method).toBe("href");
    expect(result.required_consequence).toBe("download");
    expect(result.url).toBe(
      "https://psychiatryonline.org/doi/pdf/10.1176/appi.ajp.161.4.598",
    );
    expect(result.expected_work.doi?.normalized).toBe(REQUESTED.doi);
  },
);

test.skipIf(!fixtureExists("psychiatryonline", "target-article"))(
  "PsychiatryOnline refuses a PDF link whose destination names another DOI",
  () => {
    const doc = fixture("target-article");
    const link = doc.querySelector("a#downloadPdfUrl[data-doi]");
    if (!link) throw new Error("captured PsychiatryOnline PDF link missing");
    link.setAttribute("href", "/doi/pdf/10.1176/appi.ajp.161.4.599");

    const result = planExecution(doc, spec, REQUESTED, { access_mode: "delegated" });
    expect("assisted" in result).toBe(true);
    expect(result.verdict.kind).toBe("article");
  },
);

test.skipIf(!fixtureExists("psychiatryonline", "drift"))(
  "captured resolver journal landing has no requested article or PDF affordance",
  () => {
    const doc = fixture("drift");
    const result = planExecution(doc, spec, REQUESTED, { access_mode: "delegated" });

    expect(result.verdict.kind).toBe("unknown");
    expect("assisted" in result).toBe(false);
    if (!("assisted" in result)) expect(result.required_consequence).toBe("none");
  },
);
