// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Springer Nature Link adapter against sanitized live entitled/open-access and
// isolated no-entitlement captures. Missing states remain assisted.

import { expect, test } from "bun:test";

import { adapters, interpret } from "../src/adapters/types";
import { fixtureExists, loadFixture } from "./harness";

const spec = adapters.find((adapter) => adapter.id === "springer");
if (!spec) throw new Error("springer spec missing from registry");

const HUMAN_MACHINE_TRUST = {
  expected: {
    title: "In human-machine trust, humans rely on a simple averaging strategy",
    year: 2024,
  },
};

function fixture(scenario: string): Document {
  const doc = loadFixture("springer", scenario);
  if (!doc) throw new Error(`missing springer ${scenario} fixture`);
  return doc;
}

test.skipIf(!fixtureExists("springer", "success"))(
  "matching Springer article exposes the declared direct PDF link",
  () => {
    const doc = fixture("success");
    const verdict = interpret(doc, spec, HUMAN_MACHINE_TRUST);
    expect(verdict.kind).toBe("article");
    expect(verdict.adapter_id).toBe("springer");
    const link = doc.querySelector(spec.download?.selector ?? "") as HTMLAnchorElement | null;
    expect(link).not.toBeNull();
    expect(link?.getAttribute("href")).toContain("/content/pdf/");
    expect(spec.download?.method).toBe("href");
    for (const item of verdict.evidence) expect(item).not.toMatch(/human.machine trust/i);
  },
);

test.skipIf(!fixtureExists("springer", "login-return"))(
  "authenticated Springer return page is immediately article-shaped",
  () => {
    expect(interpret(fixture("login-return"), spec, HUMAN_MACHINE_TRUST).kind).toBe("article");
  },
);

test.skipIf(!fixtureExists("springer", "wrong-work"))(
  "different requested work fails the Springer title identity check",
  () => {
    const requestedOtherWork = {
      expected: { title: "Calibrating Reliance on Automated Advice in Simulated Submarine Control" },
    };
    expect(interpret(fixture("wrong-work"), spec, requestedOtherWork).kind).toBe("wrong_work");
  },
);

test.skipIf(!fixtureExists("springer", "no-entitlement"))(
  "Springer subscription preview reports no entitlement before any download",
  () => {
    const doc = fixture("no-entitlement");
    const requested = {
      expected: {
        title:
          "The influence of information overload on the development of trust and purchase intention based on online product reviews in a mobile vs. web environment: an empirical investigation",
      },
    };
    expect(interpret(doc, spec, requested).kind).toBe("no_entitlement");
    expect(doc.querySelector("[data-test='access-article']")).not.toBeNull();
    expect(doc.querySelector(spec.download?.selector ?? "")).toBeNull();
  },
);

test.skipIf(!fixtureExists("springer", "terms"))(
  "unverified Springer terms gates remain assisted",
  () => {
    expect(interpret(fixture("terms"), spec, HUMAN_MACHINE_TRUST).kind).toBe("unknown");
  },
);

test.skipIf(!fixtureExists("springer", "drift"))(
  "renamed Springer PDF marker fails closed to unknown",
  () => {
    expect(interpret(fixture("drift"), spec, HUMAN_MACHINE_TRUST).kind).toBe("unknown");
  },
);
