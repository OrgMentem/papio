// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// EBSCO adapter against sanitized live Example University-authenticated captures. The
// no-entitlement fixture is an EBSCO metadata record with only the institution's
// link resolver; the synthetic drift fixture proves selector changes fail closed.

import { expect, test } from "bun:test";

import { adapters, interpret } from "../src/adapters/types";
import { fixtureExists, loadFixture } from "./harness";

const spec = adapters.find((adapter) => adapter.id === "ebsco");
if (!spec) throw new Error("ebsco spec missing from registry");

const DIRKS_FERRIN = {
  expected: {
    title: "Trust in leadership: Meta-analytic findings and implications for research and practice",
    year: 2002,
  },
};

function fixture(scenario: string): Document {
  const doc = loadFixture("ebsco", scenario);
  if (!doc) throw new Error(`missing ebsco ${scenario} fixture`);
  return doc;
}

test.skipIf(!fixtureExists("ebsco", "success"))(
  "matching EBSCO article exposes the declared two-click PDF controls",
  () => {
    const doc = fixture("success");
    const verdict = interpret(doc, spec, DIRKS_FERRIN);
    expect(verdict.kind).toBe("article");
    expect(verdict.adapter_id).toBe("ebsco");
    expect(doc.querySelector(spec.download?.selector ?? "")).not.toBeNull();
    expect(spec.download?.method).toBe("click");
    expect(spec.download?.followupSelector).toBe(
      "button[data-auto='bulk-download-modal-download-button']",
    );
    expect(spec.download?.postClickTimeoutMs).toBe(5000);
    for (const item of verdict.evidence) expect(item).not.toMatch(/trust in leadership/i);
  },
);

test.skipIf(!fixtureExists("ebsco", "login-return"))(
  "authenticated EBSCO return page is immediately article-shaped",
  () => {
    expect(interpret(fixture("login-return"), spec, DIRKS_FERRIN).kind).toBe("article");
  },
);

test.skipIf(!fixtureExists("ebsco", "wrong-work"))(
  "different requested work fails the EBSCO title identity check",
  () => {
    const requestedOtherWork = {
      expected: { title: "Deep Learning for Image Recognition in Autonomous Vehicles" },
    };
    expect(interpret(fixture("wrong-work"), spec, requestedOtherWork).kind).toBe("wrong_work");
  },
);

test.skipIf(!fixtureExists("ebsco", "no-entitlement"))(
  "metadata record with only the institutional link resolver reports no entitlement",
  () => {
    const doc = fixture("no-entitlement");
    expect(interpret(doc, spec, { expected: {} }).kind).toBe("no_entitlement");
    expect(doc.querySelector("button[data-auto='card-call-to-action']")).not.toBeNull();
    expect(doc.querySelector(spec.download?.selector ?? "")).toBeNull();
  },
);

test.skipIf(!fixtureExists("ebsco", "drift"))(
  "renamed EBSCO download marker fails closed to unknown",
  () => {
    expect(interpret(fixture("drift"), spec, DIRKS_FERRIN).kind).toBe("unknown");
  },
);
