// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// JSTOR adapter against sanitized live the default institution-authenticated and isolated
// logged-out captures. Missing states stay assisted rather than being guessed.

import { expect, test } from "bun:test";

import { adapters, interpret } from "../src/adapters/types";
import { fixtureExists, loadFixture } from "./harness";

const spec = adapters.find((a) => a.id === "jstor");
if (!spec) throw new Error("jstor spec missing from registry");

// fixtures/jstor/success.html is the 2026-08-03 institutionally entitled capture of
// https://www.jstor.org/stable/20183234 (spec version 0.2.0: click-invoked
// custom download control; the page exposes no PDF URL to synthesize).
const STRENGTH_MODEL = {
  expected: {
    title: "The Strength Model of Self-Control",
    year: 2007,
  },
};

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
    const verdict = interpret(doc, spec, STRENGTH_MODEL);
    expect(verdict.kind).toBe("article");
    expect(verdict.adapter_id).toBe("jstor");
    expect(doc.querySelector(spec.download?.selector ?? "")).not.toBeNull();
    // No citation_pdf_url/meta/anchor URL exists in the capture: the download
    // is a click on the primary custom control's shadow button.
    expect(spec.download?.method).toBe("click");
    expect(spec.download?.shadowSelector).toBe("#button-element");
    for (const item of verdict.evidence) expect(item).not.toMatch(/strength model/i);
  },
);

test.skipIf(!fixtureExists("jstor", "success"))(
  "a different expected work on the entitled page fails the identity check",
  () => {
    expect(interpret(fixture("success"), spec, IRON_CAGE).kind).toBe("wrong_work");
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
