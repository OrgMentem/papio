// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// EBSCO adapter against sanitized live Example University-authenticated captures. The
// no-entitlement fixture is an EBSCO metadata record with only the institution's
// link resolver; the synthetic drift fixture proves selector changes fail closed.

import { expect, test } from "bun:test";

import { adapters } from "../src/adapters/types";
import { classifyFixture, fixtureExists, loadFixture } from "./harness";
import { Window } from "happy-dom";
import { planExecution, type Plan } from "../src/plan";
import { executePlannedPageEffect } from "../src/background";

const spec = adapters.find((adapter) => adapter.id === "ebsco");
if (!spec) throw new Error("ebsco spec missing from registry");

const DIRKS_FERRIN = {
  title: "Trust in leadership: Meta-analytic findings and implications for research and practice",
  year: 2002,
};

function fixture(scenario: string): Document {
  const doc = loadFixture("ebsco", scenario);
  if (!doc) throw new Error(`missing ebsco ${scenario} fixture`);
  return doc;
}

test.skipIf(!fixtureExists("ebsco", "success"))(
  "matching EBSCO record retains article classification and the declared API route",
  () => {
    const doc = fixture("success");
    const verdict = classifyFixture(doc, spec, DIRKS_FERRIN);
    expect(verdict.kind).toBe("article");
    expect(verdict.adapter_id).toBe("ebsco");
    expect(doc.querySelector(spec.download?.selector ?? "")).not.toBeNull();
    expect(spec.download?.method).toBe("api");
    expect(spec.download?.urlTemplate).toContain("researcher-edge-aggregator");
    expect(spec.download?.jsonField).toBe("url");
    for (const item of verdict.evidence) expect(item).not.toMatch(/trust in leadership/i);
  },
);

test.skipIf(!fixtureExists("ebsco", "login-return"))(
  "authenticated EBSCO return page is immediately article-shaped",
  () => {
    expect(classifyFixture(fixture("login-return"), spec, DIRKS_FERRIN).kind).toBe("article");
  },
);

test.skipIf(!fixtureExists("ebsco", "wrong-work"))(
  "different requested work fails the EBSCO title identity check",
  () => {
    const requestedOtherWork = { title: "Deep Learning for Image Recognition in Autonomous Vehicles" };
    expect(classifyFixture(fixture("wrong-work"), spec, requestedOtherWork).kind).toBe("wrong_work");
  },
);

test.skipIf(!fixtureExists("ebsco", "no-entitlement"))(
  "metadata record with only the institutional link resolver reports no entitlement",
  () => {
    const doc = fixture("no-entitlement");
    expect(classifyFixture(doc, spec).kind).toBe("no_entitlement");
    expect(doc.querySelector("button[data-auto='card-call-to-action']")).not.toBeNull();
  },
);

test.skipIf(!fixtureExists("ebsco", "drift"))(
  "renamed EBSCO download marker fails closed to unknown",
  () => {
    expect(classifyFixture(fixture("drift"), spec, DIRKS_FERRIN).kind).toBe("unknown");
  },
);

test("EBSCO PDF viewer classifies as article (canvas-rendered) for the api download", () => {
  // The live flow lands on the viewer, not the record page. Entitlement is
  // implied (the article renders to canvas); the aggregator api downloads from
  // the viewer URL — no click, no gesture.
  const win = new Window({ url: "https://research.ebsco.com/c/6to2aa/viewer/pdf/mhqkskujrf?route=details" });
  win.document.head.insertAdjacentHTML("beforeend", "<meta name='citation_title' content='Long short-term memory'>");
  win.document.body.insertAdjacentHTML("beforeend", "<canvas></canvas>");
  const verdict = classifyFixture(win.document as unknown as Document, spec, { title: "Long short-term memory" });
  expect(verdict.kind).toBe("article");
  expect(spec.download?.method).toBe("api");
  expect(spec.download?.idPattern).toContain("viewer/pdf");
});

test("EBSCO viewer API accepts only the packaged content-file destination", async () => {
  const href = "https://research.ebsco.com/c/example/viewer/pdf/record?route=details";
  const doc = fixture("viewer-current");
  doc.defaultView!.location.href = href;
  const result = planExecution(doc, spec, { title: "Example scholarly article", doi: "10.1000/example-paper" }, { access_mode: "delegated" });
  if ("assisted" in result) throw new Error(result.assisted);
  const planned = JSON.parse(JSON.stringify(result, (_key, value) => value === null ? undefined : value)) as Plan;
  expect(planned.url).toBe("https://research.ebsco.com/api/researcher-edge-aggregator/v1/records/record/fulltext/pdf?sourceRecordId=record&opid=example&intent=view&lang=en-US");
  const previous = { document: globalThis.document, location: globalThis.location, fetch: globalThis.fetch };
  Object.assign(globalThis, { document: doc, location: new URL(href) });
  try {
    for (const [url, accepted] of [
      ["https://content.ebscohost.com/cds/retrieve?test-only=1", true],
      ["https://content.ebscohost.com/account", false],
      ["https://content.ebscohost.com/cds/retrieve-other", false],
      ["https://content.ebscohost.com.attacker.example/cds/retrieve", false],
      ["http://content.ebscohost.com/cds/retrieve", false],
      ["https://user@content.ebscohost.com/cds/retrieve", false],
    ] as const) {
      globalThis.fetch = Object.assign(async () => Response.json({ url }), { preconnect: previous.fetch.preconnect });
      const effect = await executePlannedPageEffect(planned, spec.download!);
      expect(effect.ok).toBe(accepted);
      if (accepted) expect(effect.url).toBe(url);
    }
    globalThis.fetch = Object.assign(async () => Response.json({ url: "https://content.ebscohost.com/cds/retrieve" }), { preconnect: previous.fetch.preconnect });
    expect((await executePlannedPageEffect(planned, { ...spec.download!, allowedDestinations: [] })).ok).toBe(false);
    doc.querySelector("meta[name='citation_doi']")!.setAttribute("content", "10.1000/another-paper");
    expect((await executePlannedPageEffect(planned, spec.download!)).ok).toBe(false);
  } finally { Object.assign(globalThis, previous); }
});

test("EBSCO refuses missing or mismatched DOI metadata even when the title matches", () => {
  for (const doi of [null, "10.1000/another-paper"]) {
    const doc = fixture("viewer-current");
    doc.defaultView!.location.href = "https://research.ebsco.com/c/example/viewer/pdf/record";
    const meta = doc.querySelector("meta[name='citation_doi']")!;
    if (doi === null) meta.remove(); else meta.setAttribute("content", doi);
    const planned = planExecution(doc, spec, { title: "Example scholarly article", doi: "10.1000/example-paper" }, { access_mode: "delegated" });
    expect(planned).toHaveProperty("assisted");
  }
});

test("EBSCO refuses record and nested viewer routes before execution", () => {
  for (const path of ["/c/example/details/record", "/c/example/viewer/pdf/record/extra", "/nested/c/example/viewer/pdf/record"]) {
    const doc = fixture("viewer-current");
    doc.defaultView!.location.href = `https://research.ebsco.com${path}`;
    const planned = planExecution(doc, spec, { title: "Example scholarly article", doi: "10.1000/example-paper" }, { access_mode: "delegated" });
    expect(planned).toHaveProperty("assisted");
  }
});
