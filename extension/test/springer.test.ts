// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Springer Nature Link adapter against captured article and access-prompt
// pages. An institutional login prompt does not establish no entitlement.

import { expect, test } from "bun:test";

import { adapters } from "../src/adapters/types";
import { planExecution, type Plan } from "../src/plan";
import { classifyFixture, fixtureExists, loadFixture } from "./harness";

const spec = adapters.find((adapter) => adapter.id === "springer");
if (!spec) throw new Error("springer spec missing from registry");

const HUMAN_MACHINE_TRUST = {
  title: "In human-machine trust, humans rely on a simple averaging strategy",
  year: 2024,
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
    const verdict = classifyFixture(doc, spec, HUMAN_MACHINE_TRUST);
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
    expect(classifyFixture(fixture("login-return"), spec, HUMAN_MACHINE_TRUST).kind).toBe("article");
  },
);

test.skipIf(!fixtureExists("springer", "wrong-work"))(
  "different requested work fails the Springer title identity check",
  () => {
    const requestedOtherWork = {
      title: "Calibrating Reliance on Automated Advice in Simulated Submarine Control",
    };
    expect(classifyFixture(fixture("wrong-work"), spec, requestedOtherWork).kind).toBe("wrong_work");
  },
);

test.skipIf(!fixtureExists("springer", "no-entitlement"))(
  "Springer subscription preview keeps institutional sign-in pending",
  () => {
    const doc = fixture("no-entitlement");
    const requested = {
      title:
        "The influence of information overload on the development of trust and purchase intention based on online product reviews in a mobile vs. web environment: an empirical investigation",
    };
    expect(classifyFixture(doc, spec, requested).kind).toBe("login");
    expect(doc.querySelector("[data-test='access-article']")).not.toBeNull();
    expect(doc.querySelector("[data-test='access-via-institution']")).not.toBeNull();
    expect(doc.querySelector(spec.download?.selector ?? "")).toBeNull();
  },
);

test.skipIf(!fixtureExists("springer", "terms"))(
  "unverified Springer terms gates remain assisted",
  () => {
    expect(classifyFixture(fixture("terms"), spec, HUMAN_MACHINE_TRUST).kind).toBe("unknown");
  },
);

test.skipIf(!fixtureExists("springer", "drift"))(
  "renamed Springer PDF marker fails closed to unknown",
  () => {
    expect(classifyFixture(fixture("drift"), spec, HUMAN_MACHINE_TRUST).kind).toBe("unknown");
  },
);

test("Springer institutional login prompt is not proof of no entitlement", () => {
  for (const scenario of ["no-entitlement", "institutional-access"]) {
    const page = fixture(scenario);
    expect(classifyFixture(page, spec).kind).toBe("login");
    const doi = page.querySelector("meta[name='citation_doi']")!.getAttribute("content")!;
    const plan = planExecution(page, spec, { doi }, { access_mode: "delegated" }) as Plan;
    expect(plan.verdict.kind).toBe("login");
    expect(plan.method).toBeNull();
    expect(plan.target_ref).toBeNull();
  }
});

test("Springer requires an institutional login control and prefers an available PDF", () => {
  const page = fixture("no-entitlement");
  for (const control of page.querySelectorAll("[data-test='access-via-institution']")) control.remove();
  expect(classifyFixture(page, spec).kind).toBe("unknown");
  const entitled = fixture("success");
  const access = entitled.createElement("div");
  access.setAttribute("data-test", "access-article");
  access.innerHTML = `<a href="//wayf.springernature.com"><span data-test="access-via-institution">Log in via an institution</span></a>`;
  entitled.body.append(access);
  expect(classifyFixture(entitled, spec).kind).toBe("article");
  expect(classifyFixture(fixture("login-return"), spec).kind).toBe("article");
});

test("Springer plans one header PDF despite the duplicate sticky-banner link", () => {
  for (const scenario of ["success", "login-return"]) {
    const page = fixture(scenario);
    expect(page.querySelectorAll("a[data-test='pdf-link']")).toHaveLength(2);
    const result = planExecution(page, spec, {
      doi: "10.1186/s41235-024-00583-5",
      title: HUMAN_MACHINE_TRUST.title,
    }, { access_mode: "delegated" });
    expect("assisted" in result).toBe(false);
    if ("assisted" in result) throw new Error(result.assisted);
    expect(result.verdict.kind).toBe("article");
    expect(result.method).toBe("href");
    const targets = page.querySelectorAll(result.target_ref!.selector);
    expect(targets).toHaveLength(1);
    expect(targets[0]!.closest(".app-masthead__access-container")).not.toBeNull();
    expect(result.url).toContain("/content/pdf/10.1186/s41235-024-00583-5.pdf");
  }
});

test("Springer never substitutes the sticky banner when the header PDF is absent", () => {
  const page = fixture("success");
  page.querySelector(".app-masthead__access-container")!.remove();
  expect(page.querySelectorAll("a[data-test='pdf-link']")).toHaveLength(1);
  const result = planExecution(page, spec, { doi: "10.1186/s41235-024-00583-5" }, { access_mode: "delegated" });
  expect("assisted" in result || result.method === null).toBe(true);
});
