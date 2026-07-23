// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// ACM Digital Library adapter against sanitized live captures. The "PDF/eReader"
// toolbar control is the entitlement signal; a paywalled "Get Access" page still
// carries the a#downloadPdfUrl anchor (no-entitlement fixture) and MUST stay
// unknown so the daemon never fetches an HTML access page.

import { expect, test } from "bun:test";

import { adapters, interpret } from "../src/adapters/types";
import { fixtureExists, loadFixture } from "./harness";

const spec = adapters.find((adapter) => adapter.id === "acm");
if (!spec) throw new Error("acm spec missing from registry");

const TRUST_PAPER = {
  expected: {
    title: "Who Should I Trust: AI or Myself?",
    doi: "10.1145/3544548.3581058",
    year: 2023,
  },
};

function fixture(scenario: string): Document {
  const doc = loadFixture("acm", scenario);
  if (!doc) throw new Error(`missing acm ${scenario} fixture`);
  return doc;
}

test.skipIf(!fixtureExists("acm", "success"))(
  "entitled ACM article classifies via the eReader control and builds the direct PDF endpoint",
  () => {
    const doc = fixture("success");
    const verdict = interpret(doc, spec, TRUST_PAPER);
    expect(verdict.kind).toBe("article");
    expect(verdict.adapter_id).toBe("acm");
    // The entitlement signal is the eReader toolbar control, not the anchor.
    expect(doc.querySelector(spec.download?.selector ?? "")).not.toBeNull();
    expect(spec.download?.method).toBe("url");
    // The deterministic PDF endpoint is built from the DOI in the page URL.
    const doi = TRUST_PAPER.expected.doi;
    const m = `https://dl.acm.org/doi/${doi}`.match(new RegExp(spec.download?.idPattern ?? ""));
    expect(m?.[1]).toBe(doi);
    const built = (spec.download?.urlTemplate ?? "").replace("{1}", m?.[1] ?? "");
    expect(built).toBe(`https://dl.acm.org/doi/pdf/${doi}?download=true`);
    // Evidence must never leak the requested identity or a secret token.
    for (const item of verdict.evidence) expect(item).not.toMatch(/who should i trust/i);
  },
);

test.skipIf(!fixtureExists("acm", "no-entitlement"))(
  "paywalled ACM page with the download anchor but no eReader control stays unknown",
  () => {
    // The a#downloadPdfUrl anchor is present on "Get Access" pages, so the
    // article rule must depend on the eReader entitlement control instead.
    expect(interpret(fixture("no-entitlement"), spec, TRUST_PAPER).kind).toBe("unknown");
  },
);

test.skipIf(!fixtureExists("acm", "drift"))(
  "renamed ACM download anchor with no eReader control fails closed to unknown",
  () => {
    expect(interpret(fixture("drift"), spec, TRUST_PAPER).kind).toBe("unknown");
  },
);

test("a non-ACM page classifies unknown under the ACM spec", () => {
  const doc = loadFixture("springer", "success");
  if (!doc) return; // springer fixture optional
  expect(interpret(doc, spec, TRUST_PAPER).kind).toBe("unknown");
});
