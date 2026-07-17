// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// ScienceDirect adapter. ScienceDirect is behind Cloudflare, which bot-challenges
// automated capture, so the fixtures here are SYNTHETIC minimal DOMs following
// the citation_pdf_url standard Elsevier exposes — not sanitized live captures.
// Live confirmation against a real entitled session is the remaining step.

import { expect, test } from "bun:test";

import { adapters, interpret } from "../src/adapters/types";
import { fixtureExists, loadFixture } from "./harness";

const spec = adapters.find((adapter) => adapter.id === "sciencedirect");
if (!spec) throw new Error("sciencedirect spec missing from registry");

const EXPECTED = { expected: { title: "Deep residual learning for image recognition" } };

function fixture(scenario: string): Document {
  const doc = loadFixture("sciencedirect", scenario);
  if (!doc) throw new Error(`missing sciencedirect ${scenario} fixture`);
  return doc;
}

test.skipIf(!fixtureExists("sciencedirect", "success"))(
  "entitled ScienceDirect article exposes citation_pdf_url for the autonomous meta download",
  () => {
    const doc = fixture("success");
    const verdict = interpret(doc, spec, EXPECTED);
    expect(verdict.kind).toBe("article");
    expect(verdict.adapter_id).toBe("sciencedirect");
    // Autonomous: fetch the meta URL via the privileged downloads API — no click,
    // no gesture — gated on recorded terms consent (it bypasses the terms UI).
    expect(spec.download?.method).toBe("meta");
    expect(spec.download?.metaName).toBe("citation_pdf_url");
    expect(spec.download?.requiresTermsConsent).toBe(true);
    expect(doc.querySelector(spec.download?.selector ?? "")).not.toBeNull();
    // The signed token in the PDF URL never leaks into classification evidence.
    for (const item of verdict.evidence) expect(item).not.toContain("abc123def456");
  },
);

test.skipIf(!fixtureExists("sciencedirect", "drift"))(
  "renamed ScienceDirect PDF meta fails closed to unknown",
  () => {
    expect(interpret(fixture("drift"), spec, EXPECTED).kind).toBe("unknown");
  },
);
