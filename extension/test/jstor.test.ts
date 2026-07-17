// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// JSTOR adapter against sanitized live Example University-authenticated and isolated
// logged-out captures. Missing states stay assisted rather than being guessed.

import { expect, test } from "bun:test";

import { adapters, interpret } from "../src/adapters/types";
import { fixtureExists, loadFixture } from "./harness";

const spec = adapters.find((a) => a.id === "jstor");
if (!spec) throw new Error("jstor spec missing from registry");

const IRON_CAGE = {
  expected: {
    title: "The Iron Cage Revisited: Institutional Isomorphism and Collective Rationality in Organizational Fields",
    year: 1983,
  },
};

function fixture(scenario: string): Document {
  const doc = loadFixture("jstor", scenario);
  if (!doc) throw new Error(`missing jstor ${scenario} fixture`);
  return doc;
}

test.skipIf(!fixtureExists("jstor", "success"))(
  "authenticated matching article classifies article and exposes the declared custom control",
  () => {
    const doc = fixture("success");
    const verdict = interpret(doc, spec, IRON_CAGE);
    expect(verdict.kind).toBe("article");
    expect(verdict.adapter_id).toBe("jstor");
    expect(doc.querySelector(spec.download?.selector ?? "")).not.toBeNull();
    expect(spec.download?.method).toBe("url");
    expect(spec.download?.urlTemplate).toContain("/stable/pdf/");
    for (const item of verdict.evidence) expect(item).not.toMatch(/iron cage/i);
  },
);

test.skipIf(!fixtureExists("jstor", "wrong-work"))(
  "different JSTOR article fails the title identity check",
  () => {
    expect(interpret(fixture("wrong-work"), spec, IRON_CAGE).kind).toBe("wrong_work");
  },
);

test.skipIf(!fixtureExists("jstor", "login-return"))(
  "isolated logged-out article classifies as login before article",
  () => {
    expect(interpret(fixture("login-return"), spec, IRON_CAGE).kind).toBe("login");
  },
);

test.skipIf(!fixtureExists("jstor", "terms"))(
  "open terms overlay takes precedence over the still article-shaped page",
  () => {
    expect(interpret(fixture("terms"), spec, IRON_CAGE).kind).toBe("terms");
  },
);

test.skipIf(!fixtureExists("jstor", "drift"))(
  "renamed download marker fails closed to unknown",
  () => {
    expect(interpret(fixture("drift"), spec, IRON_CAGE).kind).toBe("unknown");
  },
);
