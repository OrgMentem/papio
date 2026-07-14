// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// ProQuest adapter spec against real captured fixtures (Example University-authenticated,
// sanitized; see fixtures/proquest/*.html). These tests run only when the
// fixtures exist, and the fixtures are committed, so in this repo they RUN.

import { expect, test } from "bun:test";

import { adapters, interpret } from "../src/adapters/types";
import { fixtureExists, loadFixture } from "./harness";

const spec = adapters.find((a) => a.id === "proquest");
if (!spec) throw new Error("proquest spec missing from registry");

const LEE_SEE = {
  expected: { title: "Trust in Automation: Designing for Appropriate Reliance", year: 2004 },
};

test.skipIf(!fixtureExists("proquest", "success"))(
  "entitled article page with matching work classifies article and exposes the download selector",
  () => {
    const doc = loadFixture("proquest", "success");
    if (!doc) throw new Error("unreachable");
    const v = interpret(doc, spec, LEE_SEE);
    expect(v.kind).toBe("article");
    expect(v.adapter_id).toBe("proquest");
    expect(doc.querySelector(spec.download?.selector ?? "")).not.toBeNull();
    // Evidence must be static labels only — never page text.
    for (const e of v.evidence) expect(e).not.toMatch(/trust in automation/i);
  },
);

test.skipIf(!fixtureExists("proquest", "success"))(
  "entitled article page for a DIFFERENT requested work classifies wrong_work",
  () => {
    const doc = loadFixture("proquest", "success");
    if (!doc) throw new Error("unreachable");
    const v = interpret(doc, spec, {
      expected: { title: "Calibrating Reliance on Automated Advice in Simulated Submarine Control" },
    });
    expect(v.kind).toBe("wrong_work");
  },
);

test.skipIf(!fixtureExists("proquest", "wrong-work"))(
  "citation-only docview page (no PDF link) fails closed to unknown",
  () => {
    const doc = loadFixture("proquest", "wrong-work");
    if (!doc) throw new Error("unreachable");
    const v = interpret(doc, spec, LEE_SEE);
    expect(v.kind).toBe("unknown");
  },
);

test.skipIf(!fixtureExists("proquest", "drift"))(
  "renamed download control (selector drift) fails closed to unknown",
  () => {
    const doc = loadFixture("proquest", "drift");
    if (!doc) throw new Error("unreachable");
    const v = interpret(doc, spec, LEE_SEE);
    expect(v.kind).toBe("unknown");
  },
);
