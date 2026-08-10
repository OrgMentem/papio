import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

import { adapters, interpret, type AdapterSpec } from "../src/adapters/types";
import { planExecution } from "../src/plan";
import { captureOrigin, parseHTML } from "./harness";

function fixtureHTML(provider: string, scenario: string): string {
  return readFileSync(join(import.meta.dir, "..", "fixtures", provider, `${scenario}.html`), "utf8");
}

test("planExecution preserves the interpreter verdict and declared action for every fixture", () => {
  const root = join(import.meta.dir, "..", "fixtures");
  for (const providerEntry of readdirSync(root, { withFileTypes: true })) {
    if (!providerEntry.isDirectory()) continue;
    const spec = adapters.find((candidate) => candidate.id === providerEntry.name);
    if (spec === undefined) continue;
    for (const fixtureEntry of readdirSync(join(root, providerEntry.name))) {
      if (!fixtureEntry.endsWith(".html")) continue;
      const scenario = fixtureEntry.slice(0, -5);
      const html = fixtureHTML(providerEntry.name, scenario);
      const doc = parseHTML(html, captureOrigin(html) ?? "https://fixture.local/");
      const expected = { expected: {} };
      const verdict = interpret(doc, spec, expected);
      const planned = planExecution(doc, spec, expected.expected, {});
      if ("assisted" in planned) {
        expect(verdict.kind).toBe("article");
        expect(spec.download).toBeDefined();
        expect(doc.querySelectorAll(spec.download?.selector ?? "").length).not.toBe(1);
        continue;
      }
      expect(planned.verdict).toEqual(verdict);
      if (verdict.kind === "article" && spec.download !== undefined) {
        expect(planned.method).toBe(spec.download.method);
        expect(planned.target_ref).not.toBeNull();
        expect(planned.required_consequence).toBe("download");
      }
    }
  }
});

test("planExecution refuses an ambiguous action target instead of choosing the first match", () => {
  const spec: AdapterSpec = {
    id: "ambiguous",
    version: "1.0.0",
    hosts: ["example.test"],
    classify: [{ kind: "article", all: ["a.pdf"] }],
    download: { selector: "a.pdf", requireKind: "article", method: "href" },
  };
  const doc = parseHTML(
    '<html><body><a class="pdf" href="https://example.test/one.pdf">one</a>' +
      '<a class="pdf" href="https://example.test/two.pdf">two</a></body></html>',
    "https://example.test/article",
  );
  expect(planExecution(doc, spec, {}, {})).toEqual({ assisted: "declared action target is not unique" });
});

test("planExecution keeps href and meta URL extraction equivalent to live download semantics", () => {
  const hrefSpec: AdapterSpec = {
    id: "href",
    version: "1.0.0",
    hosts: ["example.test"],
    classify: [{ kind: "article", all: ["a.pdf"] }],
    download: { selector: "a.pdf", requireKind: "article", method: "href" },
  };
  const hrefPage = parseHTML(
    '<html><body><a class="pdf" href="/download/paper.pdf?x=1">PDF</a></body></html>',
    "https://example.test/article",
  );
  const hrefPlan = planExecution(hrefPage, hrefSpec, {}, {});
  expect("assisted" in hrefPlan ? null : hrefPlan.url).toBe("https://example.test/download/paper.pdf?x=1");

  const metaSpec: AdapterSpec = {
    id: "meta",
    version: "1.0.0",
    hosts: ["example.test"],
    classify: [{ kind: "article", all: ["meta[name='citation_pdf_url']"] }],
    download: {
      selector: "meta[name='citation_pdf_url']",
      requireKind: "article",
      method: "meta",
      metaName: "citation_pdf_url",
    },
  };
  const metaPage = parseHTML(
    '<html><head><meta name="citation_pdf_url" content="/download/paper.pdf"></head><body></body></html>',
    "https://example.test/article",
  );
  const metaPlan = planExecution(metaPage, metaSpec, {}, {});
  expect("assisted" in metaPlan ? null : metaPlan.url).toBe("https://example.test/download/paper.pdf");

  const selfPage = parseHTML(
    '<html><body><a class="pdf" href="?">same page</a></body></html>',
    "https://example.test/article",
  );
  expect(planExecution(selfPage, hrefSpec, {}, {})).toEqual({
    assisted: "declared action URL is not a distinct HTTPS URL",
  });
});

test("planExecution body has no module-scope runtime dependencies", () => {
  const source = readFileSync(join(import.meta.dir, "..", "src", "plan.ts"), "utf8");
  const start = source.indexOf("export function planExecution");
  const bodyStart = source.indexOf("{", start);
  const body = source.slice(bodyStart);
  for (const external of ["adapters", "interpret", "resolveDownloadURL", "chrome", "globalThis", "window"]) {
    expect(body).not.toMatch(new RegExp(`\\b${external}\\b`));
  }
});
