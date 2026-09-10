import { expect, test, vi } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { Window } from "happy-dom";

import { adapters, type AdapterSpec, type PageVerdict } from "../src/adapters/types";
import { planExecution, planGeneric, type ExpectedWork } from "../src/plan";
import { captureOrigin, parseHTML, verdictOf } from "./harness";

function fixtureHTML(provider: string, scenario: string): string {
  return readFileSync(join(import.meta.dir, "..", "fixtures", provider, `${scenario}.html`), "utf8");
}

/** Every verdict kind `planExecution` may project. Exhaustive by type: adding a
 * `PageKind` fails typecheck here until this table names it. */
const PLANNED_VERDICT_KINDS: Record<PageVerdict["kind"], true> = {
  article: true,
  login: true,
  terms: true,
  no_entitlement: true,
  wrong_work_check: true,
  unknown: true,
  wrong_work: true,
};

/** Every evidence label `planExecution`'s classifier can emit for this spec and
 * this requested work. The decisive label is built as `"rule:" + rule.kind +
 * " matched"` (plan.ts:435-436), so its parameterized form is derived from the
 * rule kinds the spec under test declares rather than matched loosely; the
 * remaining labels are literal (plan.ts:456, 462, 471), and the two
 * title-token labels become reachable only once a requested title is present
 * (plan.ts:437-439). `PageVerdict.evidence` reaches the daemon and the
 * operator's screen, so it holds static labels only and never page text: exact
 * membership in this closed set is that rule. An entry carrying a title, DOI,
 * or body string cannot equal a label — not even in the `rule:… matched`
 * shape, whose middle must be a rule kind this adapter declares. */
function plannedEvidenceVocabulary(spec: AdapterSpec, expected: ExpectedWork): string[] {
  const labels = ["no rule matched"];
  for (const rule of spec.classify) labels.push("rule:" + rule.kind + " matched");
  if (expected.title !== undefined && expected.title.length > 0) {
    labels.push("title-token-check passed", "title-token-check failed");
  }
  return labels;
}

test("planExecution preserves passive verdicts and fails closed on unbound effects for every fixture", () => {
  const root = join(import.meta.dir, "..", "fixtures");
  let swept = 0;
  let checkedEvidenceEntries = 0;
  // The sweep requests no particular work, so the title-token labels stay
  // unreachable and out of every fixture's expected vocabulary.
  const expected: ExpectedWork = {};
  for (const providerEntry of readdirSync(root, { withFileTypes: true })) {
    if (!providerEntry.isDirectory()) continue;
    const spec = adapters.find((candidate) => candidate.id === providerEntry.name);
    if (spec === undefined) continue;
    for (const fixtureEntry of readdirSync(join(root, providerEntry.name))) {
      if (!fixtureEntry.endsWith(".html")) continue;
      const scenario = fixtureEntry.slice(0, -5);
      const html = fixtureHTML(providerEntry.name, scenario);
      const doc = parseHTML(html, captureOrigin(html) ?? "https://fixture.local/");
      const planned = planExecution(doc, { ...spec, settleTimeoutMs: 0 }, expected, {});
      const verdict = verdictOf(planned);
      swept++;
      // One classifier, so there is no second verdict to agree with. The
      // projected verdict must still name a declared kind and stay bound to the
      // adapter that produced it — `rule.kind` reaches here from spec data, so
      // the vocabulary check is a runtime check, not just a type-level one.
      expect(Object.keys(PLANNED_VERDICT_KINDS)).toContain(verdict.kind);
      expect(verdict.adapter_id).toBe(spec.id);
      expect(verdict.adapter_version).toBe(spec.version);
      // Evidence is a closed vocabulary: the only parameterized label names a
      // rule kind this spec declares, and every other label is a fixed string.
      // Arbitrary text and page-derived text alike therefore fail exact
      // membership, which is what keeps page content out of the verdict.
      const evidenceVocabulary = plannedEvidenceVocabulary(spec, expected);
      expect(verdict.evidence.length).toBeGreaterThan(0);
      for (const entry of verdict.evidence) {
        expect(evidenceVocabulary).toContain(entry);
        checkedEvidenceEntries++;
      }
      if ("assisted" in planned) {
        // A fixture without requested-work evidence may classify as an article
        // or terms page, but its unbound effect must stay assisted rather than
        // being repaired into a plan.
        expect(["article", "terms"]).toContain(verdict.kind);
        expect(planned.assisted.length).toBeGreaterThan(0);
        continue;
      }
      if (verdict.kind === "article" && spec.download !== undefined) {
        expect(planned.method).toBe(spec.download.method);
        expect(planned.target_ref).not.toBeNull();
        expect(planned.required_consequence).toBe("download");
      }
    }
  }
  // An empty fixture tree would satisfy every assertion above vacuously.
  expect(swept).toBeGreaterThan(0);
  // Every projected verdict emits at least one label, so checking fewer entries
  // than fixtures would mean the evidence loop ran vacuously somewhere.
  expect(checkedEvidenceEntries).toBeGreaterThanOrEqual(swept);
  // Explicit timeout: the sweep walks all 66 committed fixtures through the
  // real planner and checks every evidence entry, measured at ~5.3s. That is
  // just over bun's 5s default, so the default made this fail intermittently
  // on nothing but corpus growth. Raise this bound as the corpus grows; never
  // thin the sweep to fit a timeout.
}, 30_000);

test("planExecution refuses an ambiguous action target instead of choosing the first match", () => {
  const spec: AdapterSpec = {
    id: "ambiguous",
    version: "1.0.0",
    hosts: ["example.test"],
    classify: [{ kind: "article", all: ["a.pdf"] }],
    download: { selector: "a.pdf", requireKind: "article", method: "href", workTarget: { kind: "opaque" } },
  };
  const doc = parseHTML(
    '<html><body><a class="pdf" href="https://example.test/one.pdf">one</a>' +
      '<a class="pdf" href="https://example.test/two.pdf">two</a></body></html>',
    "https://example.test/article",
  );
  expect(planExecution(doc, spec, {}, {})).toEqual({
    assisted: "declared action target is not unique",
    verdict: expect.objectContaining({ kind: "article" }),
  });
});

test("planExecution keeps href and meta URL extraction equivalent to live download semantics", () => {
  const hrefSpec: AdapterSpec = {
    id: "href",
    version: "1.0.0",
    hosts: ["example.test"],
    classify: [{ kind: "article", all: ["a.pdf"] }],
    download: { selector: "a.pdf", requireKind: "article", method: "href", workTarget: { kind: "opaque" } },
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
      workTarget: { kind: "opaque" },
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
    verdict: expect.objectContaining({ kind: "article" }),
  });
});

test("planGeneric requires exact origin authority for page-derived candidates", () => {
  const targets = [
    "https://cdn.publisher.example/download/right.pdf",
    "https://publisher.example/download/right.pdf",
    "https://other.example/download/right.pdf",
  ];
  for (const target of targets) {
    const doc = parseHTML(
      `<head><meta name="citation_doi" content="10.1000/right"><meta name="citation_pdf_url" content="${target}"></head>`,
      "https://www.publisher.example/article",
    );
    expect(planGeneric(doc, { doi: "10.1000/right" }, { access_mode: "delegated" }).candidates).toEqual([]);
  }
});

test("planExecution rejects a selected target whose explicit DOI is another work", () => {
  const spec: AdapterSpec = {
    id: "bound-target",
    version: "1",
    hosts: ["example.test"],
    classify: [{ kind: "article", all: ["meta[name='citation_doi']", "a.pdf[data-doi]"] }],
    download: {
      selector: "a.pdf[data-doi]",
      requireKind: "article",
      method: "href",
      workTarget: { kind: "doi", attribute: "data-doi" },
    },
    workEvidence: { kind: "doi", selector: "meta[name='citation_doi']", attribute: "content" },
  };
  const wrong = parseHTML(
    '<head><meta name="citation_doi" content="10.1000/right"></head>' +
      '<body><a class="pdf" data-doi="10.1000/wrong" href="/pdf/wrong.pdf">PDF</a></body>',
    "https://example.test/article",
  );
  expect(planExecution(wrong, spec, { doi: "10.1000/right" }, { access_mode: "delegated" })).toEqual({
    assisted: "declared action target does not match the requested work",
    verdict: expect.objectContaining({ kind: "article" }),
  });
  const valid = parseHTML(
    '<head><meta name="citation_doi" content="10.1000/right"></head>' +
      '<body><a class="pdf" data-doi="10.1000/right" href="/pdf/right.pdf">PDF</a></body>',
    "https://example.test/article",
  );
  const planned = planExecution(valid, spec, { doi: "10.1000/right" }, { access_mode: "delegated" });
  expect("assisted" in planned ? null : planned.url).toBe("https://example.test/pdf/right.pdf");
});

test("planGeneric records E0 evidence but emits no E1 candidate for assisted access", () => {
  const doc = parseHTML(
    '<head><meta name="citation_doi" content="doi:10.1000/ABC"></head><body></body>',
    "https://publisher.example/article",
  );
  const planned = planGeneric(doc, { doi: "10.1000/abc" }, { access_mode: "assisted" });
  expect(planned.candidates).toEqual([]);
  expect(planned.evidence).toContain("e0:citation-doi=exact");
});

test("planGeneric requires an exact normalized DOI before emitting candidates", () => {
  const doc = parseHTML(
    '<head><meta name="citation_doi" content="10.1000/right"></head><body><article><a href="/pdf/right.pdf">PDF</a></article></body>',
    "https://publisher.example/article",
  );
  expect(planGeneric(doc, { doi: "10.1000/wrong" }, { access_mode: "delegated" }).candidates).toEqual([]);
});

test("planGeneric refuses two article PDF anchors instead of choosing the first", () => {
  const doc = parseHTML(
    '<head><meta name="citation_doi" content="10.1000/right"></head><body><article>' +
      '<a href="/pdf/one.pdf">one</a><a href="/pdf/two.pdf">two</a></article></body>',
    "https://publisher.example/article",
  );
  const planned = planGeneric(doc, { doi: "doi:10.1000/right" }, { access_mode: "delegated" });
  expect(planned.candidates).toEqual([]);
  expect(planned.evidence).toContain("e0:article-pdf-link=ambiguous");
});

test("planGeneric prioritizes one declared citation PDF before article links", () => {
  const doc = parseHTML(
    '<head><meta name="citation_doi" content="10.1000/right">' +
      '<meta name="citation_pdf_url" content="/download/citation.pdf"></head>' +
      '<body><article><a href="/pdf/anchor.pdf">anchor</a></article></body>',
    "https://publisher.example/article",
  );
  const planned = planGeneric(doc, { doi: "10.1000/right" }, { access_mode: "delegated" });
  expect(planned.candidates[0]).toEqual({
    strategy_id: "generic-citation-pdf/1",
    strategy_version: "1",
    url: "https://publisher.example/download/citation.pdf",
  });
});

test("planGeneric pairs JSON-LD PDF URLs only with the record carrying the exact DOI", () => {
  const doc = parseHTML(
    '<script type="application/ld+json">' +
      JSON.stringify([
        { identifier: "10.1000/other", contentUrl: "https://publisher.example/wrong.pdf" },
        { identifier: "doi:10.1000/right", contentUrl: "/pdf/right.pdf" },
      ]) +
      "</script>",
    "https://publisher.example/article",
  );
  const planned = planGeneric(doc, { doi: "10.1000/right" }, { access_mode: "delegated" });
  expect(planned.candidates).toEqual([{
    strategy_id: "generic-citation-pdf/1",
    strategy_version: "1",
    url: "https://publisher.example/pdf/right.pdf",
  }]);
});

test("planGeneric rejects private, local, IP-literal, and cross-tenant destinations", () => {
  const targets = [
    "https://localhost/paper.pdf",
    "https://127.0.0.1/paper.pdf",
    "https://[::1]/paper.pdf",
    "https://foreign.example/paper.pdf",
    "https://attacker.github.io/paper.pdf",
  ];
  for (const target of targets) {
    const host = target.includes("github.io") ? "https://victim.github.io/article" : "https://publisher.example/article";
    const doc = parseHTML(
      `<head><meta name="citation_doi" content="10.1000/right"><link rel="alternate" type="application/pdf" href="${target}"></head>`,
      host,
    );
    expect(planGeneric(doc, { doi: "10.1000/right" }, { access_mode: "delegated" }).candidates).toEqual([]);
  }
});

test("planGeneric accepts a public same-origin JSON-LD record", () => {
  const doc = parseHTML(
    '<script type="application/ld+json">' +
      JSON.stringify({ identifier: "10.1000/right", associatedMedia: { contentUrl: "/download/right.pdf" } }) +
      "</script>",
    "https://publisher.example/article",
  );
  expect(planGeneric(doc, { doi: "10.1000/right" }, { access_mode: "delegated" }).candidates[0]?.url)
    .toBe("https://publisher.example/download/right.pdf");
});

test("planGeneric deduplicates nested regions, repeated metadata, JSON-LD, and links", () => {
  const doc = parseHTML(
    '<head><meta name="citation_doi" content="10.1000/right">' +
      '<meta name="citation_pdf_url" content="/pdf/right.pdf">' +
      '<meta name="citation_pdf_url" content="/pdf/right.pdf">' +
      '<link rel="alternate" type="application/pdf" href="/pdf/right.pdf"></head>' +
      '<main><article><a href="/pdf/right.pdf">PDF</a></article></main>' +
      '<script type="application/ld+json">' +
      JSON.stringify({ identifier: "10.1000/right", contentUrl: "/pdf/right.pdf" }) +
      "</script>",
    "https://publisher.example/article",
  );
  const planned = planGeneric(doc, { doi: "10.1000/right" }, { access_mode: "delegated" });
  expect(planned.candidates).toHaveLength(1);
  expect(planned.candidates[0]?.strategy_id).toBe("generic-citation-pdf/1");
});

test("planGeneric keeps distinct article URLs ambiguous, including meaningful query differences", () => {
  const doc = parseHTML(
    '<head><meta name="citation_doi" content="10.1000/right"></head>' +
      '<main><article><a href="/download/paper.pdf?edition=1">one</a>' +
      '<a href="/download/paper.pdf?edition=2">two</a></article></main>',
    "https://publisher.example/article",
  );
  const planned = planGeneric(doc, { doi: "10.1000/right" }, { access_mode: "delegated" });
  expect(planned.candidates).toEqual([]);
  expect(planned.evidence).toContain("e0:article-pdf-link=ambiguous");
});

test("planGeneric does nothing when the page exposes no metadata", () => {
  const planned = planGeneric(
    parseHTML("<html><body><article><a href='/pdf/paper.pdf'>PDF</a></article></body></html>", "https://publisher.example/article"),
    { doi: "10.1000/right" },
    { access_mode: "delegated" },
  );
  expect(planned.candidates).toEqual([]);
  expect(planned.evidence).toContain("e0:citation-doi=missing");
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

test("planExecution normalizes every DOI presentation while preserving slash runs", () => {
  const spec: AdapterSpec = {
    id: "doi-plan",
    version: "1",
    hosts: ["example.test"],
    classify: [{ kind: "article", all: ["a.pdf"] }],
    workEvidence: { kind: "doi", selector: "meta[name='citation_doi']", attribute: "content" },
  };
  const forms = [
    "10.1000//ABC",
    "doi:10.1000//ABC",
    " DOI: 10.1000//ABC ",
    "https://doi.org/10.1000//ABC",
    "HTTP://DX.DOI.ORG/10.1000//ABC",
  ];
  for (const citation of forms) {
    const doc = parseHTML(
      `<head><meta name="citation_doi" content="${citation}"></head><body><a class="pdf" href="/paper.pdf">PDF</a></body>`,
      "https://example.test/article",
    );
    const planned = planExecution(doc, spec, { doi: "doi:10.1000//abc" }, { access_mode: "delegated" });
    expect("assisted" in planned ? null : planned.expected_work?.doi?.normalized).toBe("10.1000//abc");
  }
});

test("planExecution requires the exact unique citation DOI meta and declared meta target", () => {
  const spec: AdapterSpec = {
    id: "meta-plan",
    version: "1",
    hosts: ["example.test"],
    workEvidence: { kind: "doi", selector: "meta[name='citation_doi']", attribute: "content" },
    classify: [{ kind: "article", all: ["meta"] }],
    download: {
      selector: "meta[name='citation_pdf_url']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "meta",
      metaName: "citation_pdf_url",
    },
  };
  const cases = [
    '<meta name="citation_pdf_url" content="/paper.pdf"><meta name="citation_doi" content="10.1000/right">',
    '<meta name="citation_pdf_url" content="/paper.pdf"><meta name="citation_doi" content="10.1000/right"><meta name="citation_doi" content="10.1000/other">',
    '<meta name="citation_pdf_url" content="/paper.pdf"><div name="citation_doi" content="10.1000/right">',
    '<meta name="other_pdf_url" content="/paper.pdf"><meta name="citation_doi" content="10.1000/right">',
  ];
  expect("assisted" in planExecution(parseHTML(`<head>${cases[0]}</head>`, "https://example.test/article"), spec, { doi: "10.1000/right" }, { access_mode: "delegated" })).toBe(false);
  for (const html of cases.slice(1)) {
    expect(planExecution(parseHTML(`<head>${html}</head>`, "https://example.test/article"), spec, { doi: "10.1000/right" }, { access_mode: "delegated" })).toHaveProperty("assisted");
  }
});

test("no-entitlement verdicts are bound to the requested work", () => {
  const spec: AdapterSpec = {
    id: "no-entitlement-bound",
    version: "1",
    hosts: ["publisher.test"],
    workEvidence: {
      kind: "doi",
      selector: "meta[name='citation_doi']",
      attribute: "content",
    },
    classify: [{ kind: "no_entitlement", all: [".purchase"] }],
  };
  const purchase = '<body><a class="purchase">Purchase PDF</a></body>';
  const correct = planExecution(
    parseHTML(
      '<html><head><meta name="citation_doi" content="10.1000/right"></head>' +
        purchase +
        "</html>",
      "https://publisher.test/article",
    ),
    spec,
    { doi: "10.1000/right" },
    {},
  );
  expect("assisted" in correct ? null : correct.verdict.kind).toBe(
    "no_entitlement",
  );
  const wrongPage = parseHTML(
    '<html><head><meta name="citation_doi" content="10.1000/other"></head>' +
      purchase +
      "</html>",
    "https://publisher.test/article",
  );
  expect(
    planExecution(wrongPage, spec, { doi: "10.1000/right" }, {}),
  ).toHaveProperty("assisted");
  // Older no-entitlement adapters such as Ex Libris Primo have no page
  // identity contract. This shared hardening must not turn their passive
  // terminal verdict into a coverage gap; only a declared contract can bind.
  const { workEvidence: _workEvidence, ...unbound } = spec;
  const compatible = planExecution(
    wrongPage,
    unbound,
    { doi: "10.1000/right" },
    {},
  );
  expect("assisted" in compatible ? null : compatible.verdict.kind).toBe(
    "no_entitlement",
  );
  // A declared contract on an identity axis the offer does not carry cannot
  // bind either. Existing title-backed adapters still settle a DOI-only offer;
  // once the offer carries a title, the same contract becomes enforceable.
  const titleOnly: AdapterSpec = {
    ...spec,
    workEvidence: {
      kind: "title",
      selector: "meta[name='citation_title']",
      attribute: "content",
    },
  };
  const doiOnly = planExecution(
    wrongPage,
    titleOnly,
    { doi: "10.1000/right" },
    {},
  );
  expect("assisted" in doiOnly ? null : doiOnly.verdict.kind).toBe(
    "no_entitlement",
  );
});

test("ACM work evidence uses the declared publication DOI and rejects absent, duplicate, or wrong evidence", () => {
  const spec = adapters.find((candidate) => candidate.id === "acm") as AdapterSpec;
  const html = fixtureHTML("acm", "success");
  const doc = parseHTML(html, captureOrigin(html) ?? "https://dl.acm.org/doi/10.1145/3544548.3581058");
  const expected = { doi: "10.1145/3544548.3581058" };
  const planned = planExecution(doc, spec, expected, { access_mode: "delegated" });
  expect("assisted" in planned ? null : planned.expected_work.doi?.normalized).toBe(expected.doi);
  expect("assisted" in planned ? null : planned.expected_work.doi?.selector).toBe(spec.workEvidence?.selector);
  expect("assisted" in planned ? null : planned.expected_work.doi?.attribute).toBe("content");
  // Keep the ACM article classifier matched while removing the DOI value:
  // deleting the element itself makes the page unknown (and therefore passive),
  // not a mis-bound article plan.
  const missing = parseHTML(
    html.replace(/<meta name="publication_doi"[^>]*>/, '<meta name="publication_doi">'),
    "https://dl.acm.org/doi/10.1145/3544548.3581058",
  );
  expect(planExecution(missing, spec, expected, { access_mode: "delegated" })).toHaveProperty("assisted");
  const duplicate = parseHTML(html.replace("</head>", '<meta name="publication_doi" content="10.1145/3544548.3581058"></head>'), "https://dl.acm.org/doi/10.1145/3544548.3581058");
  expect(planExecution(duplicate, spec, expected, { access_mode: "delegated" })).toHaveProperty("assisted");
  const wrong = parseHTML(
    html.replace(
      /<meta name="publication_doi" content="[^"]*">/,
      '<meta name="publication_doi" content="10.1145/wrong">',
    ),
    "https://dl.acm.org/doi/10.1145/3544548.3581058",
  );
  expect(planExecution(wrong, spec, expected, { access_mode: "delegated" })).toHaveProperty("assisted");
});

test("page-derived foreign destinations require a declared origin and path", () => {
  const base: AdapterSpec = {
    id: "destination",
    version: "1",
    hosts: ["publisher.test"],
    classify: [{ kind: "article", all: ["a.pdf"] }],
    download: { selector: "a.pdf", requireKind: "article", method: "href", workTarget: { kind: "opaque" } },
  };
  const doc = parseHTML('<a class="pdf" href="https://cdn.publisher.test/files/paper.pdf">PDF</a>', "https://publisher.test/article");
  expect(planExecution(doc, base, {}, { access_mode: "delegated" })).toEqual({
    assisted: "declared action URL is not a distinct HTTPS URL",
    verdict: expect.objectContaining({ kind: "article" }),
  });
  const declared = {
    ...base,
    download: {
      ...base.download!,
      allowedDestinations: [{ origin: "https://cdn.publisher.test", pathPrefix: "/files/" }],
    },
  };
  const planned = planExecution(doc, declared, {}, { access_mode: "delegated" });
  expect("assisted" in planned ? null : planned.url).toBe("https://cdn.publisher.test/files/paper.pdf");
  const metaSpec: AdapterSpec = {
    id: "meta-destination",
    version: "1",
    hosts: ["publisher.test"],
    classify: [{ kind: "article", all: ["meta[name='citation_pdf_url']"] }],
    download: {
      selector: "meta[name='citation_pdf_url']",
      requireKind: "article",
      method: "meta",
      metaName: "citation_pdf_url",
      workTarget: { kind: "opaque" },
    },
  };
  const metaDoc = parseHTML(
    '<meta name="citation_pdf_url" content="https://foreign.test/paper.pdf">',
    "https://publisher.test/article",
  );
  expect(planExecution(metaDoc, metaSpec, {}, { access_mode: "delegated" })).toHaveProperty("assisted");
});

test("planExecution binds terms text resolution to the packaged modal and unique control", () => {
  const spec: AdapterSpec = {
    id: "terms-plan",
    version: "1",
    hosts: ["example.test"],
    classify: [{ kind: "terms", all: ["div.terms[open]"] }],
    workEvidence: { kind: "doi", selector: "meta[name='publication_doi']", attribute: "content" },
    termsAccept: { modalSelector: "div.terms[open]", textAny: ["accept and download"] },
  };
  const doc = parseHTML(
    '<head><meta name="publication_doi" content="10.1000/terms"></head>' +
      '<div class="terms" open><button>Cancel</button><button>Accept and download</button></div>',
    "https://example.test/article",
  );
  const planned = planExecution(doc, spec, { doi: "10.1000/terms" }, { access_mode: "delegated" });
  expect("assisted" in planned ? null : planned.effect_graph.terms_target).toMatchObject({
    selector: "div.terms[open]",
    text_any: ["accept and download"],
  });
  expect("assisted" in planned ? null : planned.effect_graph.terms_target?.control_fingerprint).toEqual(expect.any(String));
  expect(planExecution(doc, spec, {}, { access_mode: "delegated" })).toEqual({
    assisted: "terms effect has no requested work binding",
    verdict: expect.objectContaining({ kind: "terms" }),
  });

  const ambiguous = parseHTML(
    '<div class="terms" open><button>Accept and download</button><button>Accept and download</button></div>',
    "https://example.test/article",
  );
  expect(planExecution(ambiguous, spec, { doi: "10.1000/terms" }, { access_mode: "delegated" })).toHaveProperty("assisted");
  const wrongWork = parseHTML(
    '<head><meta name="publication_doi" content="10.1000/other"></head>' +
      '<div class="terms" open><button>Accept and download</button></div>',
    "https://example.test/article",
  );
  expect(planExecution(wrongWork, spec, { doi: "10.1000/terms" }, { access_mode: "delegated" })).toHaveProperty("assisted");
});

test("planExecution carries a not-yet-present terms target in an article graph", () => {
  const spec: AdapterSpec = {
    id: "article-terms-plan",
    version: "1",
    hosts: ["example.test"],
    classify: [{ kind: "article", all: ["a.pdf"] }],
    download: { selector: "a.pdf", requireKind: "article", method: "click", workTarget: { kind: "opaque" } },
    termsAccept: { modalSelector: "div.terms[open]", textAny: ["accept and download"] },
  };
  const planned = planExecution(
    parseHTML('<a class="pdf" href="/paper.pdf">PDF</a>', "https://example.test/article"),
    spec,
    {},
    { access_mode: "delegated" },
  );
  expect("assisted" in planned ? null : planned.effect_graph.terms_target).toMatchObject({
    selector: "div.terms[open]",
    fingerprint: null,
    must_appear_after_effect: true,
    text_any: ["accept and download"],
  });
});

test("planExecution live settling waits for the exact article action target", async () => {
  const win = new Window({ url: "https://example.test/article" });
  win.document.write('<main class="article"></main>');
  const priorDocument = (globalThis as { document?: unknown }).document;
  const priorMutationObserver = (globalThis as { MutationObserver?: unknown }).MutationObserver;
  const priorSetTimeout = (globalThis as { setTimeout?: unknown }).setTimeout;
  const priorClearTimeout = (globalThis as { clearTimeout?: unknown }).clearTimeout;
  Object.assign(globalThis, {
    document: win.document,
    MutationObserver: win.MutationObserver,
    setTimeout: win.setTimeout.bind(win),
    clearTimeout: win.clearTimeout.bind(win),
  });
  try {
    const pending = planExecution(
      null,
      {
        id: "settle-target",
        version: "1",
        hosts: ["example.test"],
        settleTimeoutMs: 100,
        classify: [{ kind: "article", all: [".article"] }],
        download: { selector: "a.pdf", requireKind: "article", method: "href", workTarget: { kind: "opaque" } },
      },
      {},
      { access_mode: "delegated" },
    );
    win.setTimeout(() => {
      win.document.querySelector("main")?.insertAdjacentHTML("beforeend", '<a class="pdf" href="/paper.pdf">PDF</a>');
    }, 10);
    const planned = await pending;
    expect("assisted" in planned ? null : planned.url).toBe("https://example.test/paper.pdf");
  } finally {
    Object.assign(globalThis, {
      document: priorDocument,
      MutationObserver: priorMutationObserver,
      setTimeout: priorSetTimeout,
      clearTimeout: priorClearTimeout,
    });
  }
});
test("planExecution live settling lets an ordered blocking rule win", async () => {
  const win = new Window({ url: "https://example.test/article" });
  win.document.write('<main class="article"><a class="pdf" href="/paper.pdf">PDF</a></main>');
  vi.useFakeTimers();
  const priorDocument = (globalThis as { document?: unknown }).document;
  const priorMutationObserver = (globalThis as { MutationObserver?: unknown }).MutationObserver;
  Object.assign(globalThis, {
    document: win.document,
    MutationObserver: win.MutationObserver,
  });
  try {
    const pending = planExecution(
      null,
      {
        id: "settle-order",
        version: "1",
        hosts: ["example.test"],
        settleTimeoutMs: 250,
        classify: [
          { kind: "login", all: [".login"] },
          { kind: "article", all: [".article"] },
        ],
        download: { selector: "a.pdf", requireKind: "article", method: "href", workTarget: { kind: "opaque" } },
      },
      {},
      { access_mode: "delegated" },
    );
    setTimeout(() => {
      win.document.body.insertAdjacentHTML("afterbegin", '<div class="login">Sign in</div>');
    }, 50);
    vi.advanceTimersByTime(50);
    await Promise.resolve();
    vi.advanceTimersByTime(250);
    const planned = await pending;
    expect("assisted" in planned ? null : planned.verdict.kind).toBe("login");
  } finally {
    Object.assign(globalThis, {
      document: priorDocument,
      MutationObserver: priorMutationObserver,
    });
    vi.useRealTimers();
  }
});

// Every real job supplies a requested identity, so an adapter that declares an
// article rule without workEvidence can never execute in production, however
// green its fixture classification test is. ProQuest shipped in exactly that
// state — the resolver's highest-volume destination, silently degraded to a
// human click. This test states which shipped adapters bind an identity and
// which are assisted by design, so neither condition can change unnoticed.
test("adapters declaring an article rule bind the requested identity, or are pinned as assisted", () => {
  const requested: Record<string, ExpectedWork> = {
    proquest: { title: "Trust in Automation: Designing for Appropriate Reliance" },
    primo: { title: "Intrinsic motivation and self-determination in human behavior" },
    clinicalkey: {
      title:
        "Autonomy is equally important across East and West: Testing the cross-cultural universality of self-determination theory",
    },
  };
  // Assisted by design: no stable identity node in the committed capture. See
  // each spec's comment; binding these needs a fresh capture.
  const assistedByDesign = new Set(["primo", "clinicalkey"]);

  for (const [id, expected] of Object.entries(requested)) {
    const spec = adapters.find((candidate) => candidate.id === id);
    if (spec === undefined) throw new Error(`${id} spec missing from registry`);
    const html = fixtureHTML(id, "success");
    const doc = parseHTML(html, captureOrigin(html) ?? "https://fixture.local/");
    const planned = planExecution(doc, { ...spec, settleTimeoutMs: 0 }, expected, {});
    if (assistedByDesign.has(id)) {
      expect(spec.workEvidence).toBeUndefined();
      expect(planned).toHaveProperty("assisted");
      expect("assisted" in planned ? planned.assisted : "").toBe("declared work evidence is missing");
      continue;
    }
    expect(spec.workEvidence).toBeDefined();
    expect("assisted" in planned ? planned.assisted : null).toBeNull();
    if ("assisted" in planned) throw new Error("unreachable");
    expect(planned.verdict.kind).toBe("article");
    expect(planned.method).not.toBeNull();
    expect(planned.url).not.toBeNull();
    expect(planned.expected_work.title).not.toBeNull();
  }
});
