// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// ACM Digital Library adapter against a sanitized live entitled capture. The
// entitled download anchor is a direct PDF href; non-entitled ACM pages have no
// isolated capture and stay assisted (classify unknown).

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
  "matching ACM article exposes the declared direct PDF href",
  () => {
    const doc = fixture("success");
    const verdict = interpret(doc, spec, TRUST_PAPER);
    expect(verdict.kind).toBe("article");
    expect(verdict.adapter_id).toBe("acm");
    const link = doc.querySelector(spec.download?.selector ?? "") as HTMLAnchorElement | null;
    expect(link).not.toBeNull();
    expect(link?.getAttribute("href")).toContain("/doi/pdf/");
    expect(spec.download?.method).toBe("href");
    // Evidence must never leak the requested identity or a secret token.
    for (const item of verdict.evidence) expect(item).not.toMatch(/who should i trust/i);
  },
);

test.skipIf(!fixtureExists("acm", "drift"))(
  "renamed ACM download anchor fails closed to unknown",
  () => {
    // publication_doi still present, but the download anchor id changed: the
    // article rule requires both, so it must not false-positive.
    expect(interpret(fixture("drift"), spec, TRUST_PAPER).kind).toBe("unknown");
  },
);

test("a non-ACM page classifies unknown under the ACM spec", () => {
  const doc = loadFixture("springer", "success");
  if (!doc) return; // springer fixture optional
  expect(interpret(doc, spec, TRUST_PAPER).kind).toBe("unknown");
});
