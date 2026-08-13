// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Declarative adapter tests: the pure `interpret` classifier (rule precedence,
// every PageKind, the ≥60% wrong-work title-token check, static-only evidence),
// the skip-when-missing fixture harness, and the background verdict mapping
// (permission gate, unknown debounce, single-download latch, hello versions).

import { expect, test } from "bun:test";

import {
  adapters,
  interpret,
  providerViewerPDFURL,
  type AdapterContext,
  type AdapterSpec,
  type DownloadRule,
  type PageVerdict,
} from "../src/adapters/types";
import { planExecution, type Plan, type PlanResult } from "../src/plan";
import { parseBrowserMessage, type BrowserMessage } from "../src/protocol";
import { emptyStore, type StateBackend, type StoreShape, type TermsConsent } from "../src/state";
import {
  Bridge,
  assessDrivenPage,
  executePlannedPageEffect,
  resolveDownloadURL,
  type BridgeDeps,
  type DownloadDeltaLike,
  type DownloadItemLike,
  type NativePort,
} from "../src/background";
import { fixtureExists, loadFixture, parseHTML } from "./harness";
import { ChromeTabsFake } from "./fake-tabs";
import { Window } from "happy-dom";

// A representative ProQuest-shaped spec. Rules are ordered; first match wins.
const PROVIDER_WORK_EVIDENCE = {
  kind: "title" as const,
  selector: "meta[name='citation_title']",
  attribute: "content",
};
const SPEC: AdapterSpec = {
  id: "proquest",
  version: "0.3.1",
  hosts: ["www.proquest.com"],
  classify: [
    { kind: "login", any: ["#login-form", 'input[name="password"]'] },
    { kind: "terms", textAny: ["terms of use", "accept the terms"] },
    { kind: "no_entitlement", textAny: ["not available through your", "no full text available"] },
    { kind: "wrong_work_check", all: ["[data-mismatch]"] },
    { kind: "article", all: ["a.download-pdf"] },
  ],
  workEvidence: PROVIDER_WORK_EVIDENCE,
  download: { selector: "a.download-pdf", requireKind: "article", method: "href", workTarget: { kind: "opaque" } },
};

const EXPECTED_TITLE = "Trust in Automation: Designing for Appropriate Reliance";

function ctx(title?: string): AdapterContext {
  return { expected: title === undefined ? {} : { title } };
}

function fixtureScenarioForRule(kind: AdapterSpec["classify"][number]["kind"]): string {
  switch (kind) {
    case "article":
      return "success";
    case "login":
      return "login-return";
    case "terms":
      return "terms";
    case "no_entitlement":
      return "no-entitlement";
    case "wrong_work_check":
      return "wrong-work";
    case "unknown":
      throw new Error("unknown is the classifier fallback, not a fixture-backed rule");
  }
}

function expectFixtureBackedRules(
  specs: readonly AdapterSpec[],
  exists: (provider: string, scenario: string) => boolean = fixtureExists,
): void {
  for (const spec of specs) {
    const scenarios = new Set(spec.classify.map((rule) => fixtureScenarioForRule(rule.kind)));
    for (const scenario of scenarios) expect(exists(spec.id, scenario)).toBe(true);
  }
}

// --- Contract 1/2: interpret --------------------------------------------------

test("every registered adapter is fixture-backed, versioned, and host-scoped", () => {
  for (const spec of adapters) {
    expect(spec.id).toMatch(/^[a-z][a-z0-9_-]*$/);
    expect(spec.version).toMatch(/^\d+\.\d+\.\d+$/);
    expect(spec.hosts.length).toBeGreaterThan(0);
    expect(spec.classify.length).toBeGreaterThan(0);
    // Rules need captured evidence for their own verdict: `article` owes
    // `success` and `no_entitlement` owes `no-entitlement`. Ex Libris Primo
    // has no success capture because that terminal result never downloads here.
    expectFixtureBackedRules([spec]);
    if (spec.download) expect(spec.download.requireKind).toBe("article");
  }
  expect(adapters.map((a) => a.id)).toContain("proquest");
});

test("fixture backing rejects an adapter with no fixture directory", () => {
  const spec: AdapterSpec = {
    id: "fixtureless",
    version: "0.1.0",
    hosts: ["fixtureless.example"],
    classify: [{ kind: "article", all: ["main"] }],
  };
  expect(() => expectFixtureBackedRules([spec])).toThrow();
});

test("fixture backing rejects a rule without its matching fixture", () => {
  const spec: AdapterSpec = {
    id: "partially-fixtured",
    version: "0.1.0",
    hosts: ["partially-fixtured.example"],
    classify: [
      { kind: "article", all: ["main"] },
      { kind: "no_entitlement", all: ["aside"] },
    ],
  };
  expect(() => expectFixtureBackedRules([spec], (_provider, scenario) => scenario === "success")).toThrow();
});

test("registered adapters leave work-window visibility at the default", () => {
  for (const spec of adapters) expect(spec.requiresVisible).toBeUndefined();
});

test("interpret waits for late-upgraded custom elements when settleTimeoutMs is set", async () => {
  // JSTOR's tracked tab fires `complete` post-SSO before its `mfe-*` custom
  // elements upgrade. The live (doc === null) path must observe the DOM until
  // the download button appears, not classify once and give up.
  const jstor = adapters.find((a) => a.id === "jstor");
  expect(jstor?.settleTimeoutMs).toBeGreaterThan(0);

  const win = new Window({ url: "https://www.jstor.org/stable/259290" });
  win.document.write("<html><body><main>Loading full text</main></body></html>");
  const prev = {
    document: globalThis.document,
    MutationObserver: globalThis.MutationObserver,
    setTimeout: globalThis.setTimeout,
    clearTimeout: globalThis.clearTimeout,
  };
  Object.assign(globalThis, {
    document: win.document,
    MutationObserver: win.MutationObserver,
    setTimeout: win.setTimeout.bind(win),
    clearTimeout: win.clearTimeout.bind(win),
  });
  try {
    const verdict = interpret(null, jstor as AdapterSpec, ctx());
    win.document.body.insertAdjacentHTML(
      "beforeend",
      "<mfe-download-pharos-button data-qa=\"download-pdf\" data-doi=\"10.2307/259290\" data-sc=\"but click:pdf download\" variant=\"primary\"></mfe-download-pharos-button>",
    );
    expect((await verdict).kind).toBe("article");
  } finally {
    Object.assign(globalThis, prev);
  }
});

test("a rule with no conditions never matches (no blanket fallback)", () => {
  const spec: AdapterSpec = {
    id: "x",
    version: "0",
    hosts: ["h"],
    classify: [{ kind: "article" }], // empty conditions
  };
  const doc = parseHTML("<html><body><a class='download-pdf'>x</a></body></html>");
  expect(interpret(doc, spec, ctx()).kind).toBe("unknown");
});

test("first matching rule wins: login precedes article on an ambiguous page", () => {
  const doc = parseHTML(
    `<html><body><form id="login-form"><input name="password"></form>` +
      `<a class="download-pdf">PDF</a></body></html>`,
  );
  const v = interpret(doc, SPEC, ctx(EXPECTED_TITLE));
  expect(v.kind).toBe("login");
  expect(v.evidence).toEqual(["rule:login matched"]);
});

test("each PageKind is reachable from its own fixture", () => {
  const cases: { html: string; kind: PageVerdict["kind"] }[] = [
    { html: `<form id="login-form"></form>`, kind: "login" },
    { html: `<p>You must accept the terms of use to continue.</p>`, kind: "terms" },
    { html: `<p>No full text available for this item.</p>`, kind: "no_entitlement" },
    { html: `<div data-mismatch="1">different work</div>`, kind: "wrong_work_check" },
    { html: `<h1>${EXPECTED_TITLE}</h1><a class="download-pdf">PDF</a>`, kind: "article" },
    { html: `<p>an unrecognised page state</p>`, kind: "unknown" },
  ];
  for (const c of cases) {
    const doc = parseHTML(`<html><body>${c.html}</body></html>`);
    expect(interpret(doc, SPEC, ctx(EXPECTED_TITLE)).kind).toBe(c.kind);
  }
});

test("any[] matches on at least one selector; all[] needs every selector", () => {
  const anySpec: AdapterSpec = {
    id: "a",
    version: "0",
    hosts: ["h"],
    classify: [{ kind: "login", any: ["#a", "#b"] }],
  };
  expect(interpret(parseHTML(`<html><body><i id="b"></i></body></html>`), anySpec, ctx()).kind).toBe("login");
  expect(interpret(parseHTML(`<html><body><i id="c"></i></body></html>`), anySpec, ctx()).kind).toBe("unknown");

  const allSpec: AdapterSpec = {
    id: "a",
    version: "0",
    hosts: ["h"],
    classify: [{ kind: "login", all: ["#a", "#b"] }],
  };
  expect(interpret(parseHTML(`<html><body><i id="a"></i></body></html>`), allSpec, ctx()).kind).toBe("unknown");
  expect(
    interpret(parseHTML(`<html><body><i id="a"></i><i id="b"></i></body></html>`), allSpec, ctx()).kind,
  ).toBe("login");
});

test("wrong-work: matching title tokens keep article; mismatch downgrades to wrong_work", () => {
  // Title signal present in h1 -> ≥60% tokens -> article.
  const good = parseHTML(
    `<html><head><title>Trust in Automation: Designing for Appropriate Reliance</title></head>` +
      `<body><h1>Trust in Automation</h1><a class="download-pdf">PDF</a></body></html>`,
  );
  const gv = interpret(good, SPEC, ctx(EXPECTED_TITLE));
  expect(gv.kind).toBe("article");
  expect(gv.evidence).toContain("title-token-check passed");

  // A completely different work on an otherwise article-shaped page.
  const bad = parseHTML(
    `<html><head><title>Groupthink and the Bay of Pigs</title></head>` +
      `<body><h1>Collective Rationalization in Small Groups</h1>` +
      `<a class="download-pdf">PDF</a></body></html>`,
  );
  const bv = interpret(bad, SPEC, ctx(EXPECTED_TITLE));
  expect(bv.kind).toBe("wrong_work");
  expect(bv.evidence).toContain("title-token-check failed");
});

test("wrong-work check uses citation_title meta as a title source", () => {
  const doc = parseHTML(
    `<html><head><meta name="citation_title" content="Trust in Automation: Designing for Appropriate Reliance"></head>` +
      `<body><h1>Untitled viewer</h1><a class="download-pdf">PDF</a></body></html>`,
  );
  expect(interpret(doc, SPEC, ctx(EXPECTED_TITLE)).kind).toBe("article");
});

test("no expected title present: article is accepted without a token check", () => {
  const doc = parseHTML(`<html><body><h1>Anything</h1><a class="download-pdf">PDF</a></body></html>`);
  const v = interpret(doc, SPEC, ctx());
  expect(v.kind).toBe("article");
  expect(v.evidence).toEqual(["rule:article matched"]);
});

test("evidence carries only static rule labels — never page text", () => {
  const secret = "SECRETXYZ_page_body_marker_do_not_leak";
  const doc = parseHTML(
    `<html><head><title>${secret}</title></head><body><h1>${secret}</h1>` +
      `<p>${secret} more prose ${secret}</p><a class="download-pdf">${secret}</a></body></html>`,
  );
  const v = interpret(doc, SPEC, ctx("something entirely different"));
  const allowed = /^(rule:[a-z_]+ matched|title-token-check (passed|failed)|no rule matched)$/;
  for (const e of v.evidence) expect(e).toMatch(allowed);
  expect(JSON.stringify(v.evidence).includes(secret)).toBe(false);
  expect(JSON.stringify(v.evidence).toLowerCase().includes("secretxyz")).toBe(false);
});

// --- Contract 2: fixture harness skip-when-missing ----------------------------

test("harness reports a missing fixture as absent and loads it as null", () => {
  expect(fixtureExists("proquest", "__does_not_exist__")).toBe(false);
  expect(loadFixture("proquest", "__does_not_exist__")).toBeNull();
});

// Real capture lands later; this must SKIP (not fail) while absent.
const liveArticle = loadFixture("proquest", "article");
test.skipIf(liveArticle === null)("captured proquest article fixture classifies as article", () => {
  expect(interpret(liveArticle as Document, SPEC, ctx(EXPECTED_TITLE)).kind).toBe("article");
});

// JSTOR renders the same primary Download control on the stable/ viewer AND
// the article record page, but wires them differently: the viewer downloads
// on click, the record page calls window.open(...acceptTC=1), which Chrome's
// popup blocker eats for a gesture-less adapter click (field report
// 2026-08-03). The adapter therefore derives the direct endpoint from the tab
// URL, consent-gated because acceptTC=1 accepts JSTOR's terms.
const jstorArticle = loadFixture("jstor", "success");
const jstorRecord = loadFixture("jstor", "record");
const jstorURLFor = (rule: DownloadRule, href: string): string | null => {
  const m = href.match(new RegExp(rule.idPattern as string));
  if (!m) return null;
  return (rule.urlTemplate as string).replace(
    /\{(\d+|id)\}/g,
    (_, k: string) => m[k === "id" ? 1 : Number(k)] ?? "",
  );
};
test.skipIf(jstorArticle === null)(
  "captured JSTOR viewer page classifies and derives its consent-gated endpoint",
  () => {
    const article = jstorArticle as Document;
    const spec = adapters.find((a) => a.id === "jstor") as AdapterSpec;
    const verdict = interpret(article, spec, ctx());
    expect(verdict.kind).toBe("article");
    expect(article.querySelector("#pdf-viewer .page[data-page-number]")).not.toBeNull();

    const rule = spec.download as DownloadRule;
    expect(rule.method).toBe("url");
    // The consent gate is load-bearing: acceptTC=1 accepts publisher terms.
    expect(rule.requiresTermsConsent).toBe(true);
    expect(article.querySelector(rule.selector)?.getAttribute("data-doi")).toBe("20183234");
    expect(jstorURLFor(rule, "https://www.jstor.org/stable/pdf/20183234")).toBe(
      "https://www.jstor.org/stable/pdf/20183234.pdf?acceptTC=1",
    );
  },
);
test.skipIf(jstorRecord === null)(
  "captured JSTOR record page classifies and derives the endpoint from its stable id",
  () => {
    const record = jstorRecord as Document;
    const spec = adapters.find((a) => a.id === "jstor") as AdapterSpec;
    const verdict = interpret(record, spec, ctx());
    expect(verdict.kind).toBe("article");
    const rule = spec.download as DownloadRule;
    // The record page's own control is the entitlement evidence the url
    // method requires; its data-doi matches the id derived from the tab URL.
    expect(record.querySelector(rule.selector)?.getAttribute("data-doi")).toBe("45277272");
    // No anchor href exists on this page - the control window.open()s.
    expect(record.querySelector("a[href*='/stable/pdf/']")).toBeNull();
    expect(jstorURLFor(rule, "https://www.jstor.org/stable/45277272?seq=1")).toBe(
      "https://www.jstor.org/stable/pdf/45277272.pdf?acceptTC=1",
    );
    // Related-work download controls (secondary variant) never satisfy the
    // primary-control selector.
    expect(record.querySelectorAll(rule.selector)).toHaveLength(1);
  },
);

// Informit is an Atypon platform. Its entitled record exposes reader and PDF
// anchors but no citation PDF meta; the adapter clicks the captured PDF control
// so the browser, rather than a fetch of /doi/pdf, owns the download.
const informitArticle = loadFixture("informit", "success");
test.skipIf(informitArticle === null)(
  "captured Informit article classifies on its Atypon PDF controls",
  () => {
    expect(fixtureExists("informit", "success")).toBe(true);
    const article = informitArticle as Document;
    const spec = adapters.find((a) => a.id === "informit") as AdapterSpec;
    const verdict = interpret(article, spec, ctx());
    expect(verdict.kind).toBe("article");
    expect(verdict.adapter_id).toBe("informit");
    expect(article.querySelector("[data-doi='10.3316/informit.TOKEN']")).not.toBeNull();
    expect(
      article.querySelector("a[aria-label='View PDF'].main-link[href^='/doi/reader/']"),
    ).not.toBeNull();
    expect(article.querySelector("meta[name='citation_pdf_url']")).toBeNull();
    expect(article.querySelector("meta[name='citation_title']")).toBeNull();

    const rule = spec.download as DownloadRule;
    expect(rule.method).toBe("click");
    expect(rule.selector).toBe("a.pdf-button[href^='/doi/pdf/']");
    const control = article.querySelector(rule.selector);
    expect(control).not.toBeNull();
    expect(control?.getAttribute("href")).toBe("/doi/pdf/10.3316/informit.TOKEN");
  },
);

const informitTerms = loadFixture("informit", "terms");
test.skipIf(informitTerms === null)(
  "captured Informit SAML consent interstitial classifies as terms",
  () => {
    expect(fixtureExists("informit", "terms")).toBe(true);
    const terms = informitTerms as Document;
    const spec = adapters.find((a) => a.id === "informit") as AdapterSpec;
    const verdict = interpret(terms, spec, ctx());
    expect(verdict.kind).toBe("terms");
    expect(verdict.adapter_id).toBe("informit");
    expect(terms.querySelector("form.saml__consent__form")).not.toBeNull();
    expect(
      terms.querySelector("form.saml__consent__form input.saml__consent__yes[type='submit']"),
    ).not.toBeNull();
    expect(spec.termsAccept?.modalSelector).toBe("form.saml__consent__form");
    expect(spec.termsAccept?.control).toBe("input.saml__consent__yes");
    expect(spec.termsAccept?.textAny).toEqual([
      "i have read and agree to the terms and conditions",
    ]);
  },
);

test.skipIf(informitArticle === null)(
  "captured Informit article does not classify as terms",
  () => {
    const spec = adapters.find((a) => a.id === "informit") as AdapterSpec;
    expect(interpret(informitArticle as Document, spec, ctx()).kind).not.toBe("terms");
  },
);

// Wiley Online Library: captured 2026-07-17 from a Example University-authenticated article
// (fixtures/wiley/success.html). The article page carries the Highwire
// citation_pdf_url meta the adapter downloads through.
const wileyArticle = loadFixture("wiley", "success");
test.skipIf(wileyArticle === null)(
  "captured wiley article fixture classifies as article via the citation metas",
  () => {
    const spec = adapters.find((a) => a.id === "wiley") as AdapterSpec;
    const verdict = interpret(wileyArticle as Document, spec, ctx());
    expect(verdict.kind).toBe("article");
    expect(verdict.adapter_id).toBe("wiley");
  },
);

test("wiley stays unknown on a page lacking the citation_pdf_url/title metas", () => {
  const spec = adapters.find((a) => a.id === "wiley") as AdapterSpec;
  const page = parseHTML("<!doctype html><html><head><title>Journal home</title></head><body><h1>Psychology &amp; Marketing</h1></body></html>");
  expect(interpret(page, spec, ctx()).kind).toBe("unknown");
});

test("wiley download builds the /doi/pdfdirect endpoint from the DOI in the page URL", () => {
  const spec = adapters.find((a) => a.id === "wiley") as AdapterSpec;
  const rule = spec.download as DownloadRule;
  expect(rule.method).toBe("url");
  // Mirrors resolveDownloadURL's substitution against location.href.
  const build = (href: string): string | null => {
    const m = href.match(new RegExp(rule.idPattern as string));
    if (!m) return null;
    return (rule.urlTemplate as string).replace(
      /\{(\d+|id)\}/g,
      (_, k: string) => m[k === "id" ? 1 : Number(k)] ?? "",
    );
  };
  const want = "https://onlinelibrary.wiley.com/doi/pdfdirect/10.1002/mar.21498?download=true";
  expect(build("https://onlinelibrary.wiley.com/doi/10.1002/mar.21498")).toBe(want);
  expect(build("https://onlinelibrary.wiley.com/doi/full/10.1002/mar.21498")).toBe(want);
  expect(build("https://onlinelibrary.wiley.com/doi/epdf/10.1002/mar.21498")).toBe(want);
  // A different DOI (slashed suffix) still resolves.
  expect(build("https://onlinelibrary.wiley.com/doi/abs/10.1111/jcpp.13440")).toBe(
    "https://onlinelibrary.wiley.com/doi/pdfdirect/10.1111/jcpp.13440?download=true",
  );
});

// SAGE's View Options section signals entitled PDF access; its reader link is
// deliberately not treated as a file endpoint.
const sageArticle = loadFixture("sage", "success");
test.skipIf(sageArticle === null)(
  "captured sage article classifies via PDF/EPUB marker and derives its direct PDF URL",
  async () => {
    const spec = adapters.find((a) => a.id === "sage") as AdapterSpec;
    const verdict = interpret(sageArticle as Document, spec, ctx());
    expect(verdict.kind).toBe("article");
    expect(verdict.adapter_id).toBe("sage");

    const rule = spec.download as DownloadRule;
    expect(rule.method).toBe("url");
    const href = "https://journals.sagepub.com/doi/full/10.1177/0018720814547570";
    const prev = { document: globalThis.document, location: globalThis.location };
    Object.assign(globalThis, { document: sageArticle, location: { href } });
    try {
      expect(
        await resolveDownloadURL(rule.selector, rule.idPattern ?? null, rule.urlTemplate ?? null, null),
      ).toBe("https://journals.sagepub.com/doi/pdf/10.1177/0018720814547570?download=true");
    } finally {
      Object.assign(globalThis, prev);
    }
  },
);


test("packaged journal viewers resolve only their declared direct PDF endpoints", () => {
  expect(
    providerViewerPDFURL("https://journals.sagepub.com/doi/epdf/10.1177/0146167207301014"),
  ).toBe("https://journals.sagepub.com/doi/pdf/10.1177/0146167207301014?download=true");
  expect(
    providerViewerPDFURL("https://journals.sagepub.com/doi/epub/10.1177/14757257231222647"),
  ).toBe("https://journals.sagepub.com/doi/pdf/10.1177/14757257231222647?download=true");
  expect(
    providerViewerPDFURL("https://www.tandfonline.com/doi/epdf/10.1080/10705511.2018.1431046?needAccess=true"),
  ).toBe("https://www.tandfonline.com/doi/pdf/10.1080/10705511.2018.1431046?download=true");
  expect(
    providerViewerPDFURL("https://onlinelibrary.wiley.com/doi/epdf/10.1111/jcpp.13440"),
  ).toBe("https://onlinelibrary.wiley.com/doi/pdfdirect/10.1111/jcpp.13440?download=true");
  expect(
    providerViewerPDFURL("https://journals.sagepub.com/doi/full/10.1177/0146167207301014"),
  ).toBeUndefined();
  expect(
    providerViewerPDFURL("https://attacker.example/doi/epdf/10.1177/0146167207301014"),
  ).toBeUndefined();
});
test("sage stays unknown when its PDF/EPUB marker is absent", () => {
  const spec = adapters.find((a) => a.id === "sage") as AdapterSpec;
  const page = parseHTML(
    "<!doctype html><html><head><meta name='publication_doi' content='10.1177/0018720814547570'></head><body><a class='btn btn--pdf' href='/doi/reader/10.1177/0018720814547570'>View PDF/EPUB</a></body></html>",
  );
  expect(interpret(page, spec, ctx()).kind).toBe("unknown");
});

const halArticle = loadFixture("hal", "success");
test.skipIf(halArticle === null)(
  "captured HAL record classifies as article through citation_pdf_url",
  () => {
    const spec = adapters.find((a) => a.id === "hal") as AdapterSpec;
    const verdict = interpret(halArticle as Document, spec, ctx("Deep learning"));
    expect(verdict.kind).toBe("article");
    expect(verdict.adapter_id).toBe("hal");
    expect(spec.download?.method).toBe("meta");
  },
);

test("HAL records without a deposited document stay unknown", () => {
  const spec = adapters.find((a) => a.id === "hal") as AdapterSpec;
  const page = parseHTML(
    "<html><head><meta name='citation_title' content='Metadata-only record'>" +
      "<meta name='citation_doi' content='10.1000/no-file'></head></html>",
  );
  expect(interpret(page, spec, ctx()).kind).toBe("unknown");
});

const exLibrisPrimoNoEntitlement = loadFixture("exlibris-primo", "no-entitlement");
test.skipIf(exLibrisPrimoNoEntitlement === null)(
  "captured Ex Libris Alma resolver with no full text classifies as no entitlement",
  () => {
    const spec = adapters.find((a) => a.id === "exlibris-primo") as AdapterSpec;
    const verdict = interpret(exLibrisPrimoNoEntitlement as Document, spec, ctx());
    expect(verdict.kind).toBe("no_entitlement");
    expect(verdict.adapter_id).toBe("exlibris-primo");
  },
);

test("Alma resolver boilerplate terms footer without the no-full-text marker stays unknown", () => {
  const spec = adapters.find((a) => a.id === "exlibris-primo") as AdapterSpec;
  const page = parseHTML(
    "<html><body><form name='uResolverViewItForm'><div id='repDataLong'></div>" +
      "<c id='showAllLine'>0 - 0 of 0</c><h1>Additional services</h1>" +
      "<a>By continuing, you agree to our access Terms and Policies</a></form></body></html>",
  );
  expect(interpret(page, spec, ctx()).kind).toBe("unknown");
});

const natureArticle = loadFixture("nature", "success");
test.skipIf(natureArticle === null)(
  "captured Nature OA article classifies on access metadata and its PDF control",
  () => {
    const spec = adapters.find((a) => a.id === "nature") as AdapterSpec;
    const verdict = interpret(natureArticle as Document, spec, ctx());
    expect(verdict.kind).toBe("article");
    expect(spec.download?.method).toBe("href");
  },
);

const naturePaywall = loadFixture("nature", "no-entitlement");
test.skipIf(naturePaywall === null)(
  "captured Nature subscription preview is not mistaken for an article",
  () => {
    const spec = adapters.find((a) => a.id === "nature") as AdapterSpec;
    const verdict = interpret(naturePaywall as Document, spec, ctx());
    expect(verdict.kind).toBe("no_entitlement");
  },
);

const thiemeArticle = loadFixture("thieme", "success");
test.skipIf(thiemeArticle === null)(
  "captured Thieme full-text page classifies through rendered body and PDF anchor",
  () => {
    const spec = adapters.find((a) => a.id === "thieme") as AdapterSpec;
    const verdict = interpret(thiemeArticle as Document, spec, ctx());
    expect(verdict.kind).toBe("article");
    expect(spec.download?.method).toBe("href");
  },
);

const thiemeAbstract = loadFixture("thieme", "drift");
test.skipIf(thiemeAbstract === null)(
  "captured Thieme abstract route stays assisted despite universal PDF metadata",
  () => {
    const spec = adapters.find((a) => a.id === "thieme") as AdapterSpec;
    expect(interpret(thiemeAbstract as Document, spec, ctx()).kind).toBe("unknown");
  },
);

const cambridgeArticle = loadFixture("cambridge", "success");
test.skipIf(cambridgeArticle === null)(
  "captured Cambridge journal article classifies through its action-bar PDF controls",
  () => {
    const spec = adapters.find((a) => a.id === "cambridge") as AdapterSpec;
    expect(interpret(cambridgeArticle as Document, spec, ctx()).kind).toBe("article");
    expect(spec.download?.method).toBe("href");
  },
);

const cambridgePaywall = loadFixture("cambridge", "no-entitlement");
test.skipIf(cambridgePaywall === null)(
  "captured Cambridge purchase wall wins despite citation_pdf_url metadata",
  () => {
    const spec = adapters.find((a) => a.id === "cambridge") as AdapterSpec;
    expect(interpret(cambridgePaywall as Document, spec, ctx()).kind).toBe("no_entitlement");
  },
);

test("Cambridge book-shaped pages stay outside the journal adapter", () => {
  const spec = adapters.find((a) => a.id === "cambridge") as AdapterSpec;
  const page = parseHTML(
    "<html><head><meta name='citation_inbook_title' content='A book'>" +
      "</head><body><button data-test-id='buttonSavePDFOptions'></button>" +
      "<a href='/core/services/aop-cambridge-core/content/view/ID/book.pdf'>PDF</a></body></html>",
  );
  expect(interpret(page, spec, ctx()).kind).toBe("unknown");
});

const emeraldArticle = loadFixture("emerald", "success");
test.skipIf(emeraldArticle === null)(
  "captured Emerald OA page classifies through its real PDF anchor",
  () => {
    const spec = adapters.find((a) => a.id === "emerald") as AdapterSpec;
    expect(interpret(emeraldArticle as Document, spec, ctx()).kind).toBe("article");
    expect(spec.download?.method).toBe("href");
  },
);

const emeraldPaywall = loadFixture("emerald", "no-entitlement");
test.skipIf(emeraldPaywall === null)(
  "captured Emerald No License turnaway classifies as no entitlement",
  () => {
    const spec = adapters.find((a) => a.id === "emerald") as AdapterSpec;
    expect(interpret(emeraldPaywall as Document, spec, ctx()).kind).toBe("no_entitlement");
  },
);

const emeraldCurrent = loadFixture("emerald", "institutional");
test.skipIf(emeraldCurrent === null)(
  "captured Emerald current-platform page classifies through its migrated PDF anchor",
  () => {
    const spec = adapters.find((a) => a.id === "emerald") as AdapterSpec;
    const page = emeraldCurrent as Document;
    // The legacy Insight anchor is absent here: this page is only reachable
    // through the rule added for the migrated platform.
    expect(page.querySelector("a.intent_pdf_link")).toBeNull();
    expect(interpret(page, spec, ctx()).kind).toBe("article");
    // One download rule serves both platforms, so its union selector has to
    // resolve on each captured shape.
    expect(page.querySelector(spec.download?.selector as string)).not.toBeNull();
    expect(
      (emeraldArticle as Document).querySelector(spec.download?.selector as string),
    ).not.toBeNull();
  },
);

const tandfArticle = loadFixture("tandfonline", "success");
test.skipIf(tandfArticle === null)(
  "captured Taylor and Francis OA journal page classifies through its direct PDF control",
  () => {
    const spec = adapters.find((a) => a.id === "tandfonline") as AdapterSpec;
    expect(interpret(tandfArticle as Document, spec, ctx()).kind).toBe("article");
    expect(spec.download?.method).toBe("href");
  },
);

const tandfPaywall = loadFixture("tandfonline", "no-entitlement");
test.skipIf(tandfPaywall === null)(
  "captured Taylor and Francis Access Denial page classifies as no entitlement",
  () => {
    const spec = adapters.find((a) => a.id === "tandfonline") as AdapterSpec;
    expect(interpret(tandfPaywall as Document, spec, ctx()).kind).toBe("no_entitlement");
  },
);

const tandfInstitutional = loadFixture("tandfonline", "institutional");
test.skipIf(tandfInstitutional === null)(
  "captured Taylor and Francis institutional page classifies on the full-access badge",
  () => {
    const spec = adapters.find((a) => a.id === "tandfonline") as AdapterSpec;
    const page = tandfInstitutional as Document;
    // This is the state papio exists to drive, and it is NOT open access: the
    // OA badge the rule used to require is absent while the entitled badge and
    // a working PDF control are both rendered.
    expect(page.querySelector(".accessLogo .access-icon.oa")).toBeNull();
    expect(page.querySelector(".accessLogo .access-icon.full")).not.toBeNull();
    expect(interpret(page, spec, ctx()).kind).toBe("article");
  },
);

test("Taylor and Francis still needs a rendered access badge, not just a PDF link", () => {
  const spec = adapters.find((a) => a.id === "tandfonline") as AdapterSpec;
  const page = parseHTML(
    "<html><body><div class='downloadPDFLink'>" +
      "<a class='show-pdf' href='https://www.tandfonline.com/doi/pdf/10.1080/x'>Download PDF</a>" +
      "</div></body></html>",
  );
  expect(interpret(page, spec, ctx()).kind).toBe("unknown");
});

const sciencedirectArticle = loadFixture("sciencedirect", "success");
test.skipIf(sciencedirectArticle === null)(
  "captured ScienceDirect article uses its primary PDF control despite the cookie overlay",
  () => {
    const spec = adapters.find((a) => a.id === "sciencedirect") as AdapterSpec;
    const page = sciencedirectArticle as Document;
    // The live page that exposed the production failure has no
    // citation_pdf_url and keeps OneTrust visible. Its article-specific access
    // bar link, not a related-paper PDF link, is the positive entitlement
    // signal and download source.
    expect(page.querySelector("meta[name='citation_pdf_url']")).toBeNull();
    expect(page.querySelector("#onetrust-banner-sdk")).not.toBeNull();
    expect(page.querySelector(spec.download?.selector as string)).not.toBeNull();
    expect(interpret(page, spec, ctx()).kind).toBe("article");
    expect(spec.download?.method).toBe("click");
  },
);

const sciencedirectPaywall = loadFixture("sciencedirect", "no-entitlement");
test.skipIf(sciencedirectPaywall === null)(
  "captured ScienceDirect purchase wall reports no entitlement, not a coverage gap",
  () => {
    const spec = adapters.find((a) => a.id === "sciencedirect") as AdapterSpec;
    const page = sciencedirectPaywall as Document;
    // Reporting this as `unknown` told the user papio could not drive the page
    // and sent them hunting an adapter bug, when the resolver had simply
    // routed them somewhere they have no access.
    expect(page.querySelector("meta[name='citation_pdf_url']")).toBeNull();
    expect(interpret(page, spec, ctx()).kind).toBe("no_entitlement");
  },
);

test("an entitled ScienceDirect page still wins over the purchase-wall rule", () => {
  const spec = adapters.find((a) => a.id === "sciencedirect") as AdapterSpec;
  const page = parseHTML(
    "<html><head><meta name='citation_title' content='A paper'></head>" +
      "<body><div class='accessbar'><ul>" +
      "<li class='ViewPDF'><a class='accessbar-utility-link' href='/science/article/pii/S1/pdfft'>View PDF</a></li>" +
      "<li class='PurchasePDF'>Purchase PDF</li>" +
      "</ul></div></body></html>",
  );
  expect(interpret(page, spec, ctx()).kind).toBe("article");
});

test("Taylor and Francis book metadata is outside the journal adapter host scope", () => {
  const spec = adapters.find((a) => a.id === "tandfonline") as AdapterSpec;
  expect(spec.hosts).not.toContain("taylorfrancis.com");
});

const psycnetArticle = loadFixture("psycnet", "success");
test.skipIf(psycnetArticle === null)(
  "captured PsycNet full-text page classifies through its rendered PDF control",
  () => {
    const spec = adapters.find((a) => a.id === "psycnet") as AdapterSpec;
    expect(interpret(psycnetArticle as Document, spec, ctx()).kind).toBe("article");
    expect(spec.download?.method).toBe("href");
  },
);

const psycnetPaywall = loadFixture("psycnet", "no-entitlement");
test.skipIf(psycnetPaywall === null)(
  "captured PsycNet record with Get Access classifies as no entitlement",
  () => {
    const spec = adapters.find((a) => a.id === "psycnet") as AdapterSpec;
    expect(interpret(psycnetPaywall as Document, spec, ctx()).kind).toBe("no_entitlement");
  },
);

const annualReviewsArticle = loadFixture("annualreviews", "success");
test.skipIf(annualReviewsArticle === null)(
  "captured Annual Reviews OA page classifies through its PDF POST control",
  () => {
    const spec = adapters.find((a) => a.id === "annualreviews") as AdapterSpec;
    expect(interpret(annualReviewsArticle as Document, spec, ctx()).kind).toBe("article");
    expect(spec.download?.method).toBe("click");
  },
);

test("Annual Reviews PDF controls without the OA marker stay assisted", () => {
  const spec = adapters.find((a) => a.id === "annualreviews") as AdapterSpec;
  const page = parseHTML(
    "<html><head><meta name='citation_title' content='Closed review'></head>" +
      "<body><div id='html_fulltext'></div>" +
      "<form class='ft-download-content__form--pdf'><a aria-label='Download PDF'></a></form></body></html>",
  );
  expect(interpret(page, spec, ctx()).kind).toBe("unknown");
});

const oupArticle = loadFixture("oup", "success");
test.skipIf(oupArticle === null)(
  "captured Oxford Academic OA article classifies through its PDF action",
  () => {
    const spec = adapters.find((a) => a.id === "oup") as AdapterSpec;
    expect(interpret(oupArticle as Document, spec, ctx()).kind).toBe("article");
    expect(spec.download?.method).toBe("href");
  },
);

const oupPaywall = loadFixture("oup", "no-entitlement");
test.skipIf(oupPaywall === null)(
  "captured Oxford Academic abstract paywall classifies as no entitlement",
  () => {
    const spec = adapters.find((a) => a.id === "oup") as AdapterSpec;
    expect(interpret(oupPaywall as Document, spec, ctx()).kind).toBe("no_entitlement");
  },
);

const mitPressArticle = loadFixture("mitpress", "success");
test.skipIf(mitPressArticle === null)(
  "captured MIT Press OA article classifies through its PDF action",
  () => {
    const spec = adapters.find((a) => a.id === "mitpress") as AdapterSpec;
    expect(interpret(mitPressArticle as Document, spec, ctx()).kind).toBe("article");
    expect(spec.download?.method).toBe("href");
  },
);

const mitPressPaywall = loadFixture("mitpress", "no-entitlement");
test.skipIf(mitPressPaywall === null)(
  "captured MIT Press purchase wall classifies as no entitlement",
  () => {
    const spec = adapters.find((a) => a.id === "mitpress") as AdapterSpec;
    expect(interpret(mitPressPaywall as Document, spec, ctx()).kind).toBe("no_entitlement");
  },
);

const bmjArticle = loadFixture("bmj", "success");
test.skipIf(bmjArticle === null)(
  "captured BMJ Open article classifies through its explicit OA PDF action",
  () => {
    const spec = adapters.find((a) => a.id === "bmj") as AdapterSpec;
    expect(interpret(bmjArticle as Document, spec, ctx()).kind).toBe("article");
    expect(spec.download?.method).toBe("href");
  },
);

test("BMJ PDF metadata without explicit open access stays assisted", () => {
  const spec = adapters.find((a) => a.id === "bmj") as AdapterSpec;
  const page = parseHTML(
    "<html><head><meta name='citation_doi' content='10.1136/closed'>" +
      "<meta name='citation_pdf_url' content='https://bmj.com/content/closed.full.pdf'></head>" +
      "<body><a class='article-pdf-download' href='/content/closed.full.pdf'>PDF</a></body></html>",
  );
  expect(interpret(page, spec, ctx()).kind).toBe("unknown");
});

const psychiatryArticle = loadFixture("psychiatryonline", "success");
test.skipIf(psychiatryArticle === null)(
  "captured PsychiatryOnline full-access article classifies through its PDF action",
  () => {
    const spec = adapters.find((a) => a.id === "psychiatryonline") as AdapterSpec;
    expect(interpret(psychiatryArticle as Document, spec, ctx()).kind).toBe("article");
    expect(spec.download?.method).toBe("href");
  },
);

const psychiatryPaywall = loadFixture("psychiatryonline", "no-entitlement");
test.skipIf(psychiatryPaywall === null)(
  "captured PsychiatryOnline no-access article overrides its PDF-shaped link",
  () => {
    const spec = adapters.find((a) => a.id === "psychiatryonline") as AdapterSpec;
    expect(interpret(psychiatryPaywall as Document, spec, ctx()).kind).toBe("no_entitlement");
  },
);

const jamaArticle = loadFixture("jamanetwork", "success");
test.skipIf(jamaArticle === null)(
  "captured free JAMA article classifies through its access-checked PDF control",
  () => {
    const spec = adapters.find((a) => a.id === "jamanetwork") as AdapterSpec;
    expect(interpret(jamaArticle as Document, spec, ctx()).kind).toBe("article");
    expect(spec.download?.method).toBe("click");
  },
);

test("JAMA PDF controls without a Free or Open Access marker stay assisted", () => {
  const spec = adapters.find((a) => a.id === "jamanetwork") as AdapterSpec;
  const page = parseHTML(
    "<html><head><meta name='citation_doi' content='10.1001/closed'></head>" +
      "<body><div class='article-full-text' data-userhasaccess='True'></div>" +
      "<a id='pdf-link' class='pdfaccess' data-article-url='/article.pdf' " +
      "data-ajax-url='/Content/CheckPdfAccess'>PDF</a></body></html>",
  );
  expect(interpret(page, spec, ctx()).kind).toBe("unknown");
});

const lwwArticle = loadFixture("lww", "success");
test.skipIf(lwwArticle === null)(
  "captured LWW full-text article classifies through its wkhealth PDF metadata",
  () => {
    const spec = adapters.find((a) => a.id === "lww") as AdapterSpec;
    expect(interpret(lwwArticle as Document, spec, ctx()).kind).toBe("article");
    expect(spec.download?.method).toBe("meta");
  },
);

test("LWW PDF metadata without a rendered full-text body stays assisted", () => {
  const spec = adapters.find((a) => a.id === "lww") as AdapterSpec;
  const page = parseHTML(
    "<html><head><meta name='wkhealth_doi' content='10.1097/closed'>" +
      "<meta name='wkhealth_pdf_url' content='https://journals.lww.com/downloadpdf.aspx'></head>" +
      "<body><article id='ej-article-view'><section>Abstract only</section></article></body></html>",
  );
  expect(interpret(page, spec, ctx()).kind).toBe("unknown");
});

// ProQuest "Find your institution" wall (fixtures/proquest/login-return.html,
// captured live via CDP): Example University routes heavily through ProQuest, and without a
// ProQuest session it blocks the article behind an institution-selection form.
// The login rule (ordered before article) must catch it so papio surfaces a
// sign-in step instead of staying assisted.
const pqLogin = loadFixture("proquest", "login-return");
test.skipIf(pqLogin === null)(
  "proquest institution wall classifies as login, not unknown/article",
  () => {
    const spec = adapters.find((a) => a.id === "proquest") as AdapterSpec;
    expect(interpret(pqLogin as Document, spec, ctx()).kind).toBe("login");
  },
);

const pqSuccess = loadFixture("proquest", "success");
test.skipIf(pqSuccess === null)(
  "proquest entitled docview still classifies as article after the login rule",
  () => {
    const spec = adapters.find((a) => a.id === "proquest") as AdapterSpec;
    expect(interpret(pqSuccess as Document, spec, ctx()).kind).toBe("article");
  },
);

// --- Contract 4: background verdict mapping -----------------------------------

class FakeEmitter<A extends unknown[]> {
  private readonly cbs: ((...a: A) => unknown)[] = [];
  addListener(cb: (...a: A) => void): void {
    this.cbs.push(cb);
  }
  async emit(...a: A): Promise<void> {
    await Promise.all(this.cbs.map((cb) => cb(...a)));
  }
}

class FakePort implements NativePort {
  readonly posted: object[] = [];
  readonly onMessage = new FakeEmitter<[unknown]>();
  readonly onDisconnect = new FakeEmitter<[]>();
  postMessage(msg: object): void {
    this.posted.push(msg);
  }
  disconnect(): void {
    void this.onDisconnect.emit();
  }
  async inbound(msg: unknown): Promise<void> {
    await this.onMessage.emit(msg);
  }
}

class FakeBackend implements StateBackend {
  store: StoreShape = emptyStore();
  async load(): Promise<StoreShape> {
    return this.store;
  }
  async save(store: StoreShape): Promise<void> {
    this.store = store;
  }
}


class FakeDownloads {
  readonly onCreated = new FakeEmitter<[DownloadItemLike]>();
  readonly onChanged = new FakeEmitter<[DownloadDeltaLike]>();
  readonly onDeterminingFilename = new FakeEmitter<
    [DownloadItemLike, (s: { filename: string; conflictAction: "uniquify" }) => void]
  >();
  readonly items = new Map<number, DownloadItemLike>();
  determineBeforeReturn = false;
  emitOnCreated = false;
  crossOriginRedirect: string | undefined = undefined;
  readonly started: {
    url: string;
    filename: string;
    conflictAction: "uniquify";
    saveAs: false;
  }[] = [];
  async download(options: {
    url: string;
    filename: string;
    conflictAction: "uniquify";
    saveAs: false;
  }): Promise<number> {
    this.started.push(options);
    const id = 700 + this.started.length;
    if (this.emitOnCreated) {
      // Chrome dispatches onCreated at creation (pre-redirect, so the URL still
      // matches the pending offer) and does NOT wait for the async handler
      // before asking for a filename — so do not await here. This makes the
      // test exercise the synchronous ID binding, not the post-await one.
      void this.onCreated.emit({
        id,
        url: options.url,
        filename: "/Users/test/Downloads/out.pdf",
        fileSize: 12345,
        state: "in_progress",
      });
    }
    this.items.set(id, {
      id,
      url: this.crossOriginRedirect ?? options.url.replace("TOKEN=ephemeral", "TOKEN=normalized"),
      finalUrl: this.crossOriginRedirect ?? "https://media.proquest.com/redirected/out.pdf",
      filename: "/Users/test/Downloads/out.pdf",
      fileSize: 12345,
      state: "in_progress",
    });
    if (this.determineBeforeReturn) await this.determine(id);
    return id;
  }
  async removeFile(_downloadID: number): Promise<void> {}
  async erase(query: { id: number }): Promise<number[]> {
    return [query.id];
  }
  async determine(id: number): Promise<void> {
    const item = this.items.get(id);
    if (!item) throw new Error(`unknown fake download ${id}`);
    let relative = (item.filename ?? "").split(/[\\/]/).pop() ?? "";
    await this.onDeterminingFilename.emit(item, (s) => {
      relative = s.filename;
    });
    this.items.set(id, { ...item, filename: `/Users/test/Downloads/${relative}` });
  }
  async search(query: { id: number }): Promise<DownloadItemLike[]> {
    const item = this.items.get(query.id);
    return item ? [item] : [];
  }
}

// Fake chrome.scripting: planner injections execute the real self-contained
// planner against a happy-dom document, with the old verdict overrides mapped
// onto its returned Plan. Click and API helpers remain recorded exactly as
// Chrome would invoke them.
class FakeScripting {
  verdict: PageVerdict | undefined;
  readonly verdictQueue: PageVerdict[] = [];
  private hrefOverride: string | undefined;
  private plannerOrigin = "https://fixture.local";
  /** Optional page URL for specs whose declared rule needs a particular path. */
  documentURL: string | undefined;
  get href(): string {
    return this.hrefOverride ?? `${this.plannerOrigin}/media/signed?TOKEN=ephemeral`;
  }
  set href(value: string) {
    this.hrefOverride = value;
  }
  readonly extracted: { tabId: number; selector: string }[] = [];
  readonly clicked: {
    tabId: number;
    selector: string;
    shadowSelector?: string;
    followupSelector?: string;
  }[] = [];
  readonly rawClickArgs: unknown[][] = [];
  readonly termsAccepts: { tabId: number; modalSelector: string; textAny: unknown; control: unknown }[] = [];
  readonly interpretTabs: number[] = [];
  constructedURL: string | null = "https://provider.example.edu/pdf/default.pdf";
  readonly constructedArgs: { tabId: number; selector: string; idPattern: unknown; urlTemplate: unknown; jsonField: unknown }[] = [];
  private syntheticPageURL(spec: AdapterSpec): string {
    const supplied = this.documentURL?.trim();
    if (supplied !== undefined && supplied.length > 0) {
      try {
        const parsed = new URL(supplied);
        if (parsed.protocol === "https:" && parsed.hostname !== "") return parsed.href;
      } catch {
        // Fall through to the deterministic host-derived fixture.
      }
    }
    const declaredHost = spec.hosts[0]?.trim();
    if (declaredHost !== undefined && declaredHost.length > 0) {
      try {
        const parsed = new URL(declaredHost.includes("://") ? declaredHost : `https://${declaredHost}`);
        if (parsed.protocol === "https:" && parsed.hostname !== "") return `${parsed.origin}/fixture`;
      } catch {
        // The planner's invalid-page fallback remains deterministic.
      }
    }
    return "https://fixture.local/fixture";
  }


  private plannerResult(inj: { target: { tabId: number }; args?: unknown[] }): PlanResult {
    const args = inj.args ?? [];
    const spec = args[1] as AdapterSpec;
    const expected = (args[2] ?? {}) as { title?: string; doi?: string; year?: number };
    const policy = (args[3] ?? {}) as { access_mode?: "assisted" | "delegated" | "conservative"; terms_consent?: "accept" | "decline" };
    const override = this.verdictQueue.shift() ?? this.verdict;
    const pageURL = this.syntheticPageURL(spec);
    this.plannerOrigin = new URL(pageURL).origin;
    const win = new Window({ url: pageURL });
    const terms = override?.kind === "terms" ? spec.termsAccept : undefined;
    const planningSpec =
      terms !== undefined
        ? { ...spec, classify: [{ kind: "terms" as const, all: [terms.modalSelector] }] }
        : spec.classify.length === 0 && spec.download?.selector !== undefined
          ? { ...spec, classify: [{ kind: "article" as const, all: [spec.download.selector] }] }
          : spec;
    if (terms !== undefined) {
      const modalFirst = terms.modalSelector.split(/[.#\[]/u)[0]?.trim() ?? "div";
      const modalTag = /^[a-z][a-z0-9-]*/iu.exec(modalFirst)?.[0] ?? "div";
      const modal = win.document.createElement(modalTag);
      const modalID = /#([A-Za-z0-9_-]+)/u.exec(terms.modalSelector)?.[1];
      const modalClass = /\.([A-Za-z0-9_-]+)/u.exec(terms.modalSelector)?.[1];
      if (modalID !== undefined) modal.id = modalID;
      if (modalClass !== undefined) modal.className = modalClass;
      if (/\[open\]/u.test(terms.modalSelector)) modal.setAttribute("open", "");
      const controlSelector = terms.control ?? "button";
      const controlFirst = controlSelector.split(/[.#\[]/u)[0]?.trim() ?? "button";
      const controlTag = /^[a-z][a-z0-9-]*/iu.exec(controlFirst)?.[0] ?? "button";
      const control = win.document.createElement(controlTag);
      const controlID = /#([A-Za-z0-9_-]+)/u.exec(controlSelector)?.[1];
      const controlClass = /\.([A-Za-z0-9_-]+)/u.exec(controlSelector)?.[1];
      if (controlID !== undefined) control.id = controlID;
      if (controlClass !== undefined) control.className = controlClass;
      if (controlTag.toLowerCase() === "input") {
        control.setAttribute("type", "submit");
        control.setAttribute("value", terms.textAny[0] ?? "Accept");
      } else {
        control.textContent = terms.textAny[0] ?? "Accept";
      }
      modal.appendChild(control);
      win.document.body.appendChild(modal);
    }
    if (spec.workEvidence !== undefined) {
      const evidence = win.document.createElement("meta");
      const name = /meta\[name=['"]([^'"]+)['"]\]/u.exec(spec.workEvidence.selector)?.[1];
      if (name !== undefined) evidence.setAttribute("name", name);
      const value = spec.workEvidence.kind === "doi"
        ? expected.doi ?? ""
        : expected.title ?? "";
      evidence.setAttribute(spec.workEvidence.attribute, value);
      win.document.head.appendChild(evidence);
    }
    const selector = spec.download?.selector;
    if (selector !== undefined && terms === undefined) {
      const first = selector.split(",")[0]?.trim() ?? "div";
      const tag = /^[a-z][a-z0-9-]*/i.exec(first)?.[0] ?? "div";
      const element = win.document.createElement(tag);
      const id = /#([A-Za-z0-9_-]+)/.exec(first)?.[1];
      const className = /\.([A-Za-z0-9_-]+)/.exec(first)?.[1];
      if (id !== undefined) element.id = id;
      if (className !== undefined) element.className = className;
      if (spec.download?.method === "meta") {
        element.setAttribute("name", spec.download.metaName ?? "citation_pdf_url");
        element.setAttribute("content", this.href);
      } else if (tag.toLowerCase() === "a") {
        element.setAttribute("href", spec.download?.method === "url" ? "https://www.jstor.org/stable/4093878" : this.href);
      }
      if (spec.download?.shadowSelector !== undefined) {
        const host = element as unknown as {
          attachShadow?: (init: { mode: "open" }) => { appendChild: (child: typeof element) => unknown };
        };
        const shadow = host.attachShadow?.({ mode: "open" });
        if (shadow !== undefined) {
          const shadowSelector = spec.download.shadowSelector;
          const shadowTag = /^[a-z][a-z0-9-]*/i.exec(shadowSelector)?.[0] ?? "div";
          const shadowElement = win.document.createElement(shadowTag);
          const shadowID = /#([A-Za-z0-9_-]+)/.exec(shadowSelector)?.[1];
          if (shadowID !== undefined) shadowElement.id = shadowID;
          shadow.appendChild(shadowElement);
        }
      }
      (tag.toLowerCase() === "meta" ? win.document.head : win.document.body).appendChild(element);
    }
    const actual = planExecution(win.document as unknown as Document, planningSpec, expected, policy);
    if (override === undefined) return actual;
    // A requested identity with no declared page evidence must remain
    // assisted. Do not turn the fake's verdict override into an authority
    // binding that the real planner would have rejected.
    if (
      "assisted" in actual &&
      (override.kind === "article" || override.kind === "terms") &&
      (expected.title !== undefined || expected.doi !== undefined)
    ) return actual;
    const base: Plan = "assisted" in actual
      ? {
          adapter_id: spec.id,
          adapter_version: spec.version,
          verdict: override,
          decisive_rule: override.kind === "unknown" ? null : `rule:${override.kind} matched`,
          target_ref: null,
          method: null,
          url: null,
          required_consequence: "none",
          access_mode: policy.access_mode,
          terms_consent: policy.terms_consent ?? null,
          expected_work: {
            requested_doi: expected.doi?.trim().toLowerCase() ?? null,
            requested_title: expected.title?.trim().toLowerCase().replace(/\s+/g, " ") ?? null,
            doi: null,
            title: null,
          },
          effect_graph: {
            primary_target: null,
            followup_target: null,
            terms_target:
              override.kind === "terms" && spec.termsAccept !== undefined
                ? {
                    selector: spec.termsAccept.modalSelector,
                    shadow_selector: null,
                    fingerprint: "synthetic-modal",
                    text_any: [...spec.termsAccept.textAny],
                    control_selector: spec.termsAccept.control ?? null,
                    control_fingerprint: "synthetic-control",
                  }
                : null,
            api: null,
            consequence: "none",
            route: null,
          },
          route_origin: null,
          revalidation: {
            target_cardinality: 1,
            max_selector_length: 512,
            max_wait_ms: planningSpec.settleTimeoutMs ?? 0,
          },
        }
      : actual;
    const download = spec.download;
    const article = override.kind === "article" && download !== undefined;
    const target_ref = article
      ? (base.target_ref ?? {
          selector: download.selector,
          shadow_selector: download.shadowSelector ?? null,
          fingerprint: "synthetic",
        })
      : null;
    const method = article ? download.method : null;
    const url =
      article && (download.method === "href" || download.method === "meta")
        ? this.href
        : article
          ? base.url
          : null;
    return {
      ...base,
      adapter_id: override.adapter_id ?? spec.id,
      adapter_version: override.adapter_version ?? spec.version,
      verdict: override,
      decisive_rule: override.kind === "unknown" ? null : `rule:${override.kind} matched`,
      target_ref,
      method,
      url,
      required_consequence: article ? "download" : "none",
    };
  }
  async executeScript(inj: {
    target: { tabId: number };
    func: (...args: never[]) => unknown;
    args?: unknown[];
  }): Promise<{ result?: unknown }[]> {
    if (inj.func === assessDrivenPage) return [{ result: { kind: "normal" } }];
    if (inj.func === planExecution) {
      const planned = this.plannerResult(inj);
      if (
        typeof planned === "object" &&
        planned !== null &&
        !("assisted" in planned) &&
        planned.target_ref !== null &&
        !this.extracted.some((entry) => entry.tabId === inj.target.tabId)
      ) {
        this.extracted.push({ tabId: inj.target.tabId, selector: planned.target_ref.selector });
      }
      this.interpretTabs.push(inj.target.tabId);
      return [{ result: planned }];
    }
    const args = inj.args ?? [];
    if (inj.func === executePlannedPageEffect) {
      const plan = (args[0] ?? {}) as Plan;
      const rule = (args[1] ?? {}) as Partial<DownloadRule>;
      const termsTarget = plan.effect_graph?.terms_target;
      const target = plan.target_ref ?? plan.effect_graph?.primary_target ?? termsTarget;
      if (rule.method === "click" && target !== null && target !== undefined) {
        const followup = typeof rule.followupSelector === "string" ? rule.followupSelector : null;
        this.rawClickArgs.push([
          target.selector,
          target.shadow_selector,
          null,
          rule.postClickTimeoutMs ?? null,
          followup,
        ]);
        if (target === termsTarget) {
          this.termsAccepts.push({
            tabId: inj.target.tabId,
            modalSelector: target.selector,
            textAny: termsTarget?.text_any,
            control: termsTarget?.control_selector,
          });
          return [{ result: { ok: true } }];
        }
        this.clicked.push({
          tabId: inj.target.tabId,
          selector: target.selector,
          ...(typeof target.shadow_selector === "string" ? { shadowSelector: target.shadow_selector } : {}),
          ...(followup !== null ? { followupSelector: followup } : {}),
        });
        return [{ result: { ok: true } }];
      }
      if (rule.method === "api") {
        const api = plan.effect_graph?.api;
        this.constructedArgs.push({
          tabId: inj.target.tabId,
          selector: target?.selector ?? "",
          idPattern: null,
          urlTemplate: api?.endpoint ?? null,
          jsonField: api?.result_field ?? null,
        });
        return [{ result: { ok: this.constructedURL !== null, url: this.constructedURL ?? undefined } }];
      }
      return [{ result: { ok: plan.url !== null, url: plan.url ?? undefined } }];
    }
    if (args.length === 1) {
      this.extracted.push({ tabId: inj.target.tabId, selector: String(args[0]) });
      return [{ result: this.href }];
    }
    if (args.length === 5) {
      this.rawClickArgs.push([...args]);
      this.clicked.push({
        tabId: inj.target.tabId,
        selector: String(args[0]),
        ...(typeof args[1] === "string" ? { shadowSelector: args[1] } : {}),
        ...(typeof args[4] === "string" ? { followupSelector: args[4] } : {}),
      });
      return [{ result: true }];
    }
    if (args.length === 4) {
      this.constructedArgs.push({ tabId: inj.target.tabId, selector: String(args[0]), idPattern: args[1], urlTemplate: args[2], jsonField: args[3] });
      return [{ result: this.constructedURL }];
    }
    return [{ result: undefined }];
  }
}

class FakePermissions {
  granted = true;
  readonly checks: string[][] = [];
  async contains(perm: { origins: string[] }): Promise<boolean> {
    this.checks.push(perm.origins);
    return this.granted;
  }
}

const PROVIDER = "www.proquest.com";
const OPENURL = "https://resolver.example.edu/openurl?ctx=abc";
const EXPIRES = "2027-01-01T00:00:00Z";

interface MapHarness {
  bridge: Bridge;
  port: FakePort;
  backend: FakeBackend;
  scripting: FakeScripting;
  tabs: ChromeTabsFake;
  downloads: FakeDownloads;
  permissions: FakePermissions;
  settings: { consent: TermsConsent };
  clock: { now: number };
  timers: { fn: () => void | Promise<void>; ms: number }[];
  alarms: { created: { name: string }[]; fire(name: string): void };
  frames(): BrowserMessage[];
}

function makeMapHarness(specs: AdapterSpec[] = [SPEC]): MapHarness {
  const port = new FakePort();
  const backend = new FakeBackend();
  const scripting = new FakeScripting();
  const permissions = new FakePermissions();
  const tabs = new ChromeTabsFake();
  const downloads = new FakeDownloads();
  const clock = { now: 1_700_000_000_000 };
  const timers: { fn: () => void | Promise<void>; ms: number }[] = [];
  const settings = { consent: undefined as TermsConsent };
  const alarmListeners: ((a: { name: string }) => void)[] = [];
  const alarms = {
    created: [] as { name: string; info: { periodInMinutes: number } }[],
    create: (name: string, info: { periodInMinutes: number }) => {
      alarms.created.push({ name, info });
    },
    onAlarm: { addListener: (cb: (a: { name: string }) => void) => alarmListeners.push(cb) },
    fire: (name: string) => {
      for (const cb of alarmListeners) cb({ name });
    },
  };
  const deps: BridgeDeps = {
    connectNative: () => port,
    manifestVersion: "0.1.0",
    randomUUID: () => crypto.randomUUID(),
    now: () => clock.now,
    setTimeout: (fn, ms) => {
      timers.push({ fn, ms });
    },
    backend,
    tabs,
    downloads,
    adapterSpecs: specs,
    scripting,
    permissions,
    settings: {
      getTermsConsent: async () => settings.consent,
      setTermsConsent: async (v) => {
        settings.consent = v;
      },
      getHandoffSurface: async () => "work-window",
    },
    action: {
      setBadgeText: async () => {},
      setBadgeBackgroundColor: async () => {},
    },
    alarms,
  };
  return {
    bridge: new Bridge(deps),
    port,
    backend,
    tabs,
    scripting,
    downloads,
    permissions,
    clock,
    timers,
    settings,
    alarms,
    frames: () => port.posted.map(parseBrowserMessage),
  };
}

function offer(
  jobID: string,
  expected?: { title?: string; doi?: string },
  providerHosts: string[] = [PROVIDER],
  loginEntityID?: string,
  proquestAccountID?: string,
): unknown {
  return {
    protocol: "papio-browser/1",
    type: "job_offer",
    msg_id: "offer_00000001",
    job_id: jobID,
    seq: 0,
    payload: {
      openurl: OPENURL,
      provider_hosts: providerHosts,
      access_mode: "delegated",
      expires_at: EXPIRES,
      ...(expected !== undefined ? { expected } : {}),
      ...(loginEntityID !== undefined ? { login_entity_id: loginEntityID } : {}),
      ...(proquestAccountID !== undefined ? { proquest_account_id: proquestAccountID } : {}),
    },
  };
}

async function landOnProvider(
  h: MapHarness,
  jobID: string,
  host: string = PROVIDER,
  url: string = `https://${host}/pqdweb`,
): Promise<number> {
  const tabID = h.backend.store.activeJobs.find((j) => j.job_id === jobID)?.tab_id ?? -1;
  await h.tabs.completeNavigation(tabID, url);
  return tabID;
}

test("auth return classifies the provider landing even without a complete event", async () => {
  // JSTOR-class providers end SSO with a soft-nav landing that carries no
  // `status: "complete"`, so the complete-gated classify never fires. The
  // auth-return transition must classify the page itself.
  const h = makeMapHarness([SPEC]);
  h.scripting.verdict = { kind: "article", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_authreturn_0001"));
  const tabID = h.backend.store.activeJobs.find((j) => j.job_id === "job_authreturn_0001")?.tab_id ?? -1;
  expect(tabID).toBeGreaterThanOrEqual(0);

  const idpURL = "https://idp.example.edu/sso?SAMLRequest=x";
  await h.tabs.userNavigate(tabID, idpURL);
  expect(h.backend.store.activeJobs[0]?.status).toBe("auth_pending");

  const provURL = `https://${PROVIDER}/pqdweb?doc=1`;
  await h.tabs.userNavigate(tabID, provURL);

  expect(h.frames().some((f) => f.type === "auth_returned")).toBe(true);
  expect(h.scripting.interpretTabs).toContain(tabID);
  expect(h.downloads.started.length).toBe(1);
});

test("a transiently unknown provider page is reclassified until it renders", async () => {
  // The first classify sees an un-upgraded page (unknown); a bounded retry must
  // re-run the classifier so the eventually-rendered article still downloads.
  const h = makeMapHarness([SPEC]);
  h.scripting.verdictQueue.push(
    { kind: "unknown", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] },
    { kind: "article", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] },
  );
  await h.bridge.start();
  await h.port.inbound(offer("job_retry_0001"));
  const tabID = await landOnProvider(h, "job_retry_0001");

  // Unknown first: no download yet, but a retry is scheduled.
  expect(h.downloads.started.length).toBe(0);
  expect(h.timers.length).toBeGreaterThan(0);

  // The governor also owns a 3-minute drive timeout; execute only the 2.5s
  // classifier retry here or the harness would time the job out before the
  // late-rendering page gets its second assessment.
  const retryTimers = h.timers.filter((timer) => timer.ms === 2_500);
  h.timers.splice(0, h.timers.length, ...h.timers.filter((timer) => timer.ms !== 2_500));
  for (const timer of retryTimers) await timer.fn();
  expect(h.scripting.interpretTabs.length).toBeGreaterThanOrEqual(2);
  expect(h.downloads.started.length).toBe(1);
  expect(h.tabs.snapshot(tabID)?.url).toContain(PROVIDER);
});

test("a provider PDF opened in a new viewer tab is adopted for the opener job", async () => {
  // JSTOR-class providers "download" by opening the PDF in a new tab. That tab
  // is untracked; it must be adopted for the handoff tab that spawned it.
  const h = makeMapHarness([SPEC]);
  await h.bridge.start();
  await h.port.inbound(offer("job_viewer_0001"));
  const trackedTab = h.backend.store.activeJobs.find(j => j.job_id === 'job_viewer_0001')?.tab_id ?? -1;
  expect(trackedTab).toBeGreaterThanOrEqual(0);
  const viewerTab = 999;
  const pdfUrl = `https://${PROVIDER}/doc/259290.pdf?refreqid=x`;
  h.tabs.seed({ id: viewerTab, url: pdfUrl, openerTabId: trackedTab });
  await h.tabs.completeNavigation(viewerTab, pdfUrl);

  // The PDF is downloaded for the opener job and the viewer remains open.
  expect(h.downloads.started.map(d => d.url)).toContain(pdfUrl);
  expect(h.downloads.started.some(d => d.filename.includes('job_viewer_0001'))).toBe(true);
  expect(h.tabs.snapshot(viewerTab)).toBeDefined();
});

test("a stray non-opener PDF tab is not adopted", async () => {
  const h = makeMapHarness([SPEC]);
  await h.bridge.start();
  await h.port.inbound(offer("job_viewer_0002"));
  const strayTab = 998;
  const pdfUrl = `https://${PROVIDER}/doc/other.pdf`;
  // openerTabId points at an unrelated tab; no download_initiated job matches.
  h.tabs.seed({ id: strayTab, url: pdfUrl, openerTabId: 12345 });
  await h.tabs.completeNavigation(strayTab, pdfUrl);
  expect(h.downloads.started.length).toBe(0);
  expect(h.tabs.snapshot(strayTab)).toBeDefined();
});

const TERMS_SPEC: AdapterSpec = {
  id: "termsprov",
  version: "1.0.0",
  hosts: [PROVIDER],
  classify: [{ kind: "terms", all: ["div.terms[open]"] }],
  workEvidence: PROVIDER_WORK_EVIDENCE,
  termsAccept: { modalSelector: "div.terms[open]", textAny: ["accept and download"] },
};
const termsVerdict = { kind: "terms" as const, adapter_id: "termsprov", adapter_version: "1.0.0", evidence: [] };


test("terms verdict auto-accepts only when the user has consented", async () => {
  const h = makeMapHarness([TERMS_SPEC]);
  h.settings.consent = "accept";
  h.scripting.verdict = termsVerdict;
  await h.bridge.start();
  await h.port.inbound(offer("job_terms_0001", { title: EXPECTED_TITLE }));
  await landOnProvider(h, "job_terms_0001");

  expect(h.scripting.termsAccepts.length).toBe(1);
  expect(h.scripting.termsAccepts[0]?.modalSelector).toBe("div.terms[open]");
  // Auto-accept emits no provider_outcome; the ensuing download is the record.
  expect(h.frames().some((f) => f.type === "provider_outcome")).toBe(false);
});
test("terms modal without declared work evidence stays assisted and never clicks", async () => {
  const { workEvidence: _workEvidence, ...unbound } = TERMS_SPEC;
  const h = makeMapHarness([unbound]);
  h.settings.consent = "accept";
  h.scripting.verdict = { ...termsVerdict, adapter_id: unbound.id };
  await h.bridge.start();
  await h.port.inbound(offer("job_terms_unbound_0001", { title: EXPECTED_TITLE }));
  await landOnProvider(h, "job_terms_unbound_0001");
  expect(h.scripting.termsAccepts).toHaveLength(0);
});

test("terms verdict stays a human step and flags consent when undecided", async () => {

  const h = makeMapHarness([TERMS_SPEC]);
  h.scripting.verdict = termsVerdict; // consent stays undefined
  await h.bridge.start();
  await h.port.inbound(offer("job_terms_0002", { title: EXPECTED_TITLE }));
  await landOnProvider(h, "job_terms_0002");

  expect(h.scripting.termsAccepts.length).toBe(0);
  expect(h.frames().some((f) => f.type === "provider_outcome" && f.payload.outcome === "terms_acceptance_required")).toBe(true);
  expect(h.backend.store.activeJobs.find((j) => j.job_id === "job_terms_0002")?.needs_terms_consent).toBe(true);
});

test("granting consent clears the prompt flag and re-attempts the pending terms gate", async () => {
  const h = makeMapHarness([TERMS_SPEC]);
  h.scripting.verdict = termsVerdict;
  await h.bridge.start();
  await h.port.inbound(offer("job_terms_0003", { title: EXPECTED_TITLE }));
  await landOnProvider(h, "job_terms_0003");
  expect(h.backend.store.activeJobs[0]?.needs_terms_consent).toBe(true);

  await h.bridge.requestTermsConsent("accept");
  expect(h.settings.consent).toBe("accept");
  expect(h.backend.store.activeJobs[0]?.needs_terms_consent).toBe(false);
  expect(h.scripting.termsAccepts.length).toBeGreaterThanOrEqual(1);
});

test("declining consent records manual and never auto-accepts", async () => {
  const h = makeMapHarness([TERMS_SPEC]);
  h.scripting.verdict = termsVerdict;
  await h.bridge.start();
  await h.port.inbound(offer("job_terms_0004", { title: EXPECTED_TITLE }));
  await landOnProvider(h, "job_terms_0004");
  await h.bridge.requestTermsConsent("manual");
  expect(h.settings.consent).toBe("manual");
  expect(h.scripting.termsAccepts.length).toBe(0);
});

test("startup re-drives a pending terms gate when consent was granted while asleep", async () => {
  // The consent grant's one-shot re-drive can miss a job if the worker was
  // asleep when the popup message arrived. On the next connect, startup must
  // re-drive any still-flagged gate now that consent is "accept".
  const h = makeMapHarness([TERMS_SPEC]);
  h.scripting.verdict = termsVerdict; // consent undefined -> flags the gate
  await h.bridge.start();
  await h.port.inbound(offer("job_terms_wake_0001", { title: EXPECTED_TITLE }));
  await landOnProvider(h, "job_terms_wake_0001");
  expect(h.backend.store.activeJobs[0]?.needs_terms_consent).toBe(true);
  expect(h.scripting.termsAccepts.length).toBe(0);

  // Consent recorded directly (popup wrote it) while the one-shot re-drive
  // never ran for this job — it stays flagged with its tab still open.
  h.settings.consent = "accept";
  await h.bridge.start(); // worker wakes

  expect(h.backend.store.activeJobs[0]?.needs_terms_consent).toBe(false);
  expect(h.scripting.termsAccepts.length).toBeGreaterThanOrEqual(1);
});

const TERMS_DL_SPEC: AdapterSpec = {
  id: "termsdl",
  version: "1.0.0",
  hosts: [PROVIDER],
  classify: [],
  workEvidence: PROVIDER_WORK_EVIDENCE,
  download: { selector: "button.dl", requireKind: "article", method: "click", workTarget: { kind: "opaque" } },
  termsAccept: { modalSelector: "div.terms[open]", textAny: ["accept and download"] },
};

test("a latched download-click keeps re-classifying until a late terms modal is accepted", async () => {
  // Terms-gated providers (JSTOR) upgrade the terms modal AFTER the download
  // click latches download_initiated. The classify retry must keep watching so
  // the late modal is caught and accepted — without ever re-clicking / starting
  // a second download.
  const h = makeMapHarness([TERMS_DL_SPEC]);
  h.settings.consent = "accept";
  const article = { kind: "article" as const, adapter_id: "termsdl", adapter_version: "1.0.0", evidence: [] };
  const terms = { kind: "terms" as const, adapter_id: "termsdl", adapter_version: "1.0.0", evidence: [] };
  // 1st classify: article -> click (latches). Its revalidation consumes the
  // second article. The retry sees terms and acceptTerms revalidates terms once
  // more immediately before clicking the declared accept control.
  h.scripting.verdictQueue.push(article, article, terms, terms);
  await h.bridge.start();
  await h.port.inbound(offer("job_termsdl_0001", { title: EXPECTED_TITLE }));
  await landOnProvider(h, "job_termsdl_0001");

  expect(h.scripting.clicked.length).toBe(1); // download clicked once (latched)
  expect(h.backend.store.activeJobs[0]?.download_initiated).toBe(true);
  expect(h.scripting.termsAccepts.length).toBe(0); // modal not upgraded yet
  expect(h.timers.length).toBeGreaterThan(0); // retry scheduled despite the latch

  // Drain only classifier retries. The governor's 3-minute drive timeout is a
  // separate lifecycle and must not win this synthetic race.
  for (let i = 0; i < 8 && h.scripting.termsAccepts.length === 0; i++) {
    const retryTimers = h.timers.filter((timer) => timer.ms === 2_500);
    h.timers.splice(0, h.timers.length, ...h.timers.filter((timer) => timer.ms !== 2_500));
    if (retryTimers.length === 0) break;
    for (const timer of retryTimers) await timer.fn();
  }
  expect(h.scripting.termsAccepts.length).toBeGreaterThanOrEqual(1);
  expect(h.scripting.clicked.length).toBe(1); // retry never re-clicked the download
});


test("startup reconciliation re-queues a job whose pre-download tab vanished", async () => {
  // A tab closed while the worker slept never fired onTabRemoved, so the job
  // still points at a dead tab. Reconcile must recover it, not strand it.
  const h = makeMapHarness([SPEC]);
  await h.bridge.start();
  await h.port.inbound(offer("job_recon_0001"));
  const tabID = await landOnProvider(h, "job_recon_0001");
  expect(tabID).toBeGreaterThanOrEqual(0);

  h.tabs.forget(tabID); // vanished while worker was asleep
  await h.bridge.start(); // worker wakes

  // Recovered: no longer pointed at the dead tab. Auth evidence from the prior
  // landing lets the same start() reopen it immediately; otherwise it is queued
  // and the forced-release timer reopens it. Either way it lands on a live tab.
  const job = h.backend.store.activeJobs.find((j) => j.job_id === "job_recon_0001");
  expect(job).toBeDefined();
  expect(job?.tab_id).not.toBe(tabID);
  for (const t of h.timers.splice(0)) await t.fn();
  const reopened = h.backend.store.activeJobs.find((j) => j.job_id === "job_recon_0001");
  expect(reopened?.tab_id).toBeGreaterThanOrEqual(0);
  expect(h.tabs.snapshot(reopened?.tab_id ?? -1)).toBeDefined();
});

test("startup reconciliation parks a past-auth job whose tab vanished", async () => {
  const h = makeMapHarness([SPEC]);
  await h.bridge.start();
  await h.port.inbound(offer("job_recon_0002"));
  const tabID = await landOnProvider(h, "job_recon_0002");
  // Drive through auth to awaiting_download.
  const idp = "https://idp.example.edu/sso?SAMLRequest=x";
  await h.tabs.userNavigate(tabID, idp);
  const prov = `https://${PROVIDER}/doc?x=1`;
  await h.tabs.userNavigate(tabID, prov);
  expect(h.backend.store.activeJobs.find((j) => j.job_id === "job_recon_0002")?.status).toBe("awaiting_download");

  await h.tabs.userClose(tabID);
  await h.bridge.start();
  // Parked: download may have landed in the adoption dir for the daemon to scan.
  expect(h.backend.store.activeJobs.some((j) => j.job_id === "job_recon_0002")).toBe(false);
});

test("startup reconciliation leaves a job with a live tab untouched", async () => {
  const h = makeMapHarness([SPEC]);
  await h.bridge.start();
  await h.port.inbound(offer("job_recon_0003"));
  const tabID = await landOnProvider(h, "job_recon_0003");
  await h.bridge.start();
  const job = h.backend.store.activeJobs.find((j) => j.job_id === "job_recon_0003");
  expect(job?.tab_id).toBe(tabID);
  expect(job?.status).toBe("accepted");
});

test("repeated authentication failures cap re-driving and report human_auth_required", async () => {
  // A warm session that cannot clear the IdP (expired SSO) would otherwise be
  // re-offered and re-driven forever, thrashing the provider. After
  // MAX_AUTH_ATTEMPTS drives that reach auth without a download, the extension
  // must stop opening broker tabs and report the human step instead.
  const h = makeMapHarness([SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  const idp = "https://idp.example.edu/sso?SAMLRequest=x";

  // Three drives that each reach authentication but never download.
  for (let i = 0; i < 3; i++) {
    await h.port.inbound(offer("job_authstall_0001"));
    const tabID = h.backend.store.activeJobs.find((j) => j.job_id === "job_authstall_0001")?.tab_id ?? -1;
    expect(tabID).toBeGreaterThanOrEqual(0);
    await h.tabs.userNavigate(tabID, idp);
    expect(h.backend.store.activeJobs.find((j) => j.job_id === "job_authstall_0001")?.status).toBe("auth_pending");
    await h.tabs.userClose(tabID); // tab dies before the session ever authenticates
  }
  expect(h.backend.store.authAttempts?.["job_authstall_0001"]).toBe(3);

  const tabsBefore = h.tabs.nextId;
  const outcomesBefore = h.frames().filter((f) => f.type === "provider_outcome").length;

  // Fourth offer is capped: no broker tab opens and one human_auth_required
  // outcome is reported. The job is not re-tracked (no re-drive this session).
  await h.port.inbound(offer("job_authstall_0001"));
  expect(h.tabs.nextId).toBe(tabsBefore); // tabs.create never called
  expect(h.backend.store.activeJobs.some((j) => j.job_id === "job_authstall_0001")).toBe(false);
  const outcomes = h.frames().filter((f) => f.type === "provider_outcome");
  expect(outcomes.length).toBe(outcomesBefore + 1);
  expect(outcomes.at(-1)?.payload["outcome"]).toBe("human_auth_required");

  // A further capped offer this worker lifetime stays quiet (no re-report, no tab).
  await h.port.inbound(offer("job_authstall_0001"));
  expect(h.frames().filter((f) => f.type === "provider_outcome").length).toBe(outcomesBefore + 1);
  expect(h.tabs.nextId).toBe(tabsBefore);
});

test("a completed download clears the auth-failure budget", async () => {
  // An earlier expired-session streak must not cap a job whose session later
  // works: a real download proves auth succeeded and resets the counter.
  const h = makeMapHarness([SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  const idp = "https://idp.example.edu/sso?SAMLRequest=x";

  for (let i = 0; i < 2; i++) {
    await h.port.inbound(offer("job_authreset_0001"));
    const t = h.backend.store.activeJobs.find((j) => j.job_id === "job_authreset_0001")?.tab_id ?? -1;
    await h.tabs.userNavigate(t, idp);
    await h.tabs.userClose(t);
  }
  expect(h.backend.store.authAttempts?.["job_authreset_0001"]).toBe(2);

  // Third drive authenticates and downloads the article.
  h.scripting.verdict = { kind: "article", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.port.inbound(offer("job_authreset_0001"));
  const tabID = h.backend.store.activeJobs.find((j) => j.job_id === "job_authreset_0001")?.tab_id ?? -1;
  await h.tabs.userNavigate(tabID, idp);
  const prov = `https://${PROVIDER}/doc?x=1`;
  await h.tabs.userNavigate(tabID, prov);
  expect(h.downloads.started.length).toBe(1);
  await h.downloads.onChanged.emit({ id: 701, state: { current: "complete" } });

  expect(h.backend.store.authAttempts?.["job_authreset_0001"]).toBeUndefined();
});

test("startup registers the periodic keepalive alarm", async () => {
  const h = makeMapHarness([SPEC]);
  await h.bridge.start();
  expect(h.alarms.created.some((a) => a.name === "papio-keepalive")).toBe(true);
});

test("the keepalive alarm reconnects a worker whose native port had dropped", async () => {
  // MV3 dormancy / daemon restart kills the port; the setTimeout backoff dies
  // with a sleeping worker, so the alarm wake must re-establish the connection.
  const h = makeMapHarness([SPEC]);
  await h.bridge.start();
  const hellosBefore = h.frames().filter((f) => f.type === "hello").length;

  h.port.disconnect(); // port death; onDisconnect nulls the port + queues a backoff timer (unfired)
  await Promise.resolve();
  h.alarms.fire("papio-keepalive");
  await Promise.resolve();
  await Promise.resolve();

  const hellosAfter = h.frames().filter((f) => f.type === "hello").length;
  expect(hellosAfter).toBe(hellosBefore + 1);
});

test("the keepalive alarm is a no-op while the port is healthy", async () => {
  const h = makeMapHarness([SPEC]);
  await h.bridge.start();
  const hellosBefore = h.frames().filter((f) => f.type === "hello").length;
  h.alarms.fire("papio-keepalive");
  await Promise.resolve();
  await Promise.resolve();
  expect(h.frames().filter((f) => f.type === "hello").length).toBe(hellosBefore);
});

test("hello reports adapter_versions from the registered specs", async () => {
  const jstor: AdapterSpec = { id: "jstor", version: "1.2.0", hosts: ["www.jstor.org"], classify: [] };
  const h = makeMapHarness([SPEC, jstor]);
  await h.bridge.start();
  const hello = h.frames().find((f) => f.type === "hello");
  expect(hello?.payload["adapter_versions"]).toEqual({ proquest: "0.3.1", jstor: "1.2.0" });
});

test("empty registry reports an empty adapter_versions map", async () => {
  const h = makeMapHarness([]);
  await h.bridge.start();
  const hello = h.frames().find((f) => f.type === "hello");
  expect(hello?.payload["adapter_versions"]).toEqual({});
});

test("article verdict starts one browser-managed job-scoped download, no signed URL frame", async () => {
  const h = makeMapHarness();
  h.scripting.verdict = { kind: "article", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_article_0001", { title: EXPECTED_TITLE }));
  const tabID = await landOnProvider(h, "job_article_0001");

  expect(h.scripting.extracted).toEqual([{ tabId: tabID, selector: "a.download-pdf" }]);
  expect(h.downloads.started).toEqual([
    {
      url: h.scripting.href,
      filename: "papio/job_article_0001/paper.pdf",
      conflictAction: "uniquify",
      saveAs: false,
    },
  ]);
  expect(h.backend.store.activeJobs[0]?.download_initiated).toBe(true);
  // Signed URL remains extension-memory-only: no frame contains it.
  expect(h.frames().some((f) => JSON.stringify(f).includes("TOKEN=ephemeral"))).toBe(false);
  expect(h.frames().some((f) => f.type === "download_started")).toBe(false);
  expect(h.frames().some((f) => f.type === "provider_outcome")).toBe(false);

  // A re-classification (another page load) must NOT initiate a second download.
  await landOnProvider(h, "job_article_0001");
  expect(h.downloads.started.length).toBe(1);
  // Live Chrome returned the download ID before asking for a filename.
  await h.downloads.determine(701);
  expect(h.downloads.items.get(701)?.filename).toBe(
    "/Users/test/Downloads/papio/job_article_0001/out.pdf",
  );
  // Completion is correlated by chrome.downloads.download's returned ID even
  // if onCreated raced before the Promise resolved.
  await h.downloads.onChanged.emit({ id: 701, state: { current: "complete" } });
  const complete = h.frames().find((f) => f.type === "download_complete");
  expect(complete?.job_id).toBe("job_article_0001");
  expect(complete?.payload["filename"]).toBe("out.pdf");
  expect(complete?.payload["size_bytes"]).toBe(12345);
});

test("declared shadow click reclassifies an in-page terms gate", async () => {
  const clickSpec: AdapterSpec = {
    id: "jstor",
    version: "0.1.0",
    hosts: [PROVIDER],
    classify: [{ kind: "article", all: ["mfe-download"] }],
    download: {
      selector: "mfe-download",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "click",
      shadowSelector: "#button-element",
      postClickWaitFor: ".terms[open]",
      postClickTimeoutMs: 3000,
    },
  };
  const h = makeMapHarness([clickSpec]);
  h.scripting.verdictQueue.push(
    { kind: "article", adapter_id: "jstor", adapter_version: "0.1.0", evidence: [] },
    { kind: "article", adapter_id: "jstor", adapter_version: "0.1.0", evidence: [] },
    { kind: "terms", adapter_id: "jstor", adapter_version: "0.1.0", evidence: [] },
  );
  await h.bridge.start();
  await h.port.inbound(offer("job_jstor_terms_0001"));
  const tabID = await landOnProvider(h, "job_jstor_terms_0001");

  expect(h.scripting.clicked).toEqual([
    { tabId: tabID, selector: "mfe-download", shadowSelector: "#button-element" },
  ]);
  expect(h.backend.store.activeJobs[0]?.download_initiated).toBe(true);

  const outcome = h.frames().find((f) => f.type === "provider_outcome");
  expect(outcome?.payload["outcome"]).toBe("terms_acceptance_required");
  expect(outcome?.payload["adapter_version"]).toBe("0.1.0");
  expect(h.scripting.interpretTabs.length).toBe(3);
  expect(h.downloads.started).toHaveLength(0);
});
test("declared provider modal follow-up stays inside the one click helper", async () => {
  const clickSpec: AdapterSpec = {
    id: "ebsco",
    version: "0.1.0",
    hosts: [PROVIDER],
    classify: [{ kind: "article", all: ["meta[name='citation_title']"] }],
    download: {
      selector: "[data-auto='download']",
      requireKind: "article",
      method: "click",
      workTarget: { kind: "opaque" },
      followupSelector: "[data-auto='confirm-download']",
      postClickTimeoutMs: 3000,
    },
  };
  const h = makeMapHarness([clickSpec]);
  h.scripting.verdict = {
    kind: "article",
    adapter_id: "ebsco",
    adapter_version: "0.1.0",
    evidence: [],
  };
  await h.bridge.start();
  await h.port.inbound(offer("job_ebsco_click_0001"));
  const tabID = await landOnProvider(h, "job_ebsco_click_0001");

  expect(h.scripting.clicked).toEqual([
    {
      tabId: tabID,
      selector: "[data-auto='download']",
      followupSelector: "[data-auto='confirm-download']",
    },
  ]);
  expect(h.scripting.rawClickArgs).toEqual([
    [
      "[data-auto='download']",
      null,
      null,
      3000,
      "[data-auto='confirm-download']",
    ],
  ]);
  expect(h.backend.store.activeJobs[0]?.download_initiated).toBe(true);
  expect(h.downloads.started).toHaveLength(0);
});
test("click downloads correlate by adapter when concurrent handoffs share provider hosts", async () => {
  const jstor: AdapterSpec = {
    id: "jstor",
    version: "0.1.0",
    hosts: ["www.jstor.org"],
    classify: [{ kind: "article", all: [".download"] }],
    download: { selector: ".download", requireKind: "article", method: "click", workTarget: { kind: "opaque" } },
  };
  const ebsco: AdapterSpec = {
    id: "ebsco",
    version: "0.1.0",
    hosts: ["research.ebsco.com"],
    classify: [{ kind: "article", all: [".download"] }],
    download: { selector: ".download", requireKind: "article", method: "click", workTarget: { kind: "opaque" } },
  };
  const providerHosts = ["www.jstor.org", "research.ebsco.com"];
  const h = makeMapHarness([jstor, ebsco]);
  h.scripting.verdictQueue.push(
    { kind: "article", adapter_id: "jstor", adapter_version: "0.1.0", evidence: [] },
    { kind: "article", adapter_id: "ebsco", adapter_version: "0.1.0", evidence: [] },
  );
  await h.bridge.start();
  // Evidence is per-origin now: both offers carry OPENURL, so one release-grade
  // observation of that resolver authorizes both queued handoffs.
  await h.bridge.recordFreshSessionEvidence({
    origin: new URL(OPENURL).origin,
    observedAt: Date.now(),
    generation: 1,
    source: "live_tab",
  });
  await h.port.inbound(offer("job_jstor_concurrent_0001", undefined, providerHosts));
  const firstTab = await landOnProvider(h, "job_jstor_concurrent_0001", "www.jstor.org");

  // The second same-provider handoff is accepted but remains queued behind
  // the first drive. Its effect must not be attempted against the first tab.
  await h.port.inbound(offer("job_ebsco_concurrent_0001", undefined, providerHosts));
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_ebsco_concurrent_0001")).toMatchObject({
    tab_id: -1,
    status: "accepted",
  });
  expect(h.tabs.list()).toHaveLength(1);

  const firstItem: DownloadItemLike = {
    id: 900,
    url: "blob:https://www.jstor.org/download",
    referrer: "https://www.jstor.org/c/record",
    filename: "/Users/test/Downloads/JSTOR-FullText.pdf",
    state: "in_progress",
  };
  let firstSuggested: { filename: string; conflictAction: "uniquify" } | undefined;
  await h.downloads.onDeterminingFilename.emit(firstItem, (value) => {
    firstSuggested = value;
  });
  expect(firstSuggested).toEqual({
    filename: "papio/job_jstor_concurrent_0001/JSTOR-FullText.pdf",
    conflictAction: "uniquify",
  });
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_jstor_concurrent_0001")?.adapter_id).toBe(
    "jstor",
  );

  // Settle the first effect; only then may the queued handoff acquire a tab.
  await h.bridge.requestCancel("job_jstor_concurrent_0001");
  const secondTab = await landOnProvider(h, "job_ebsco_concurrent_0001", "research.ebsco.com");

  const item: DownloadItemLike = {
    id: 901,
    url: "blob:https://research.ebsco.com/download",
    referrer: "https://research.ebsco.com/c/record",
    filename: "/Users/test/Downloads/EBSCO-FullText.pdf",
    state: "in_progress",
  };
  let suggested: { filename: string; conflictAction: "uniquify" } | undefined;
  await h.downloads.onDeterminingFilename.emit(item, (value) => {
    suggested = value;
  });

  expect(secondTab).not.toBe(firstTab);
  expect(suggested).toEqual({
    filename: "papio/job_ebsco_concurrent_0001/EBSCO-FullText.pdf",
    conflictAction: "uniquify",
  });
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_jstor_concurrent_0001")).toBeUndefined();
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_ebsco_concurrent_0001")?.adapter_id).toBe(
    "ebsco",
  );
});



test("filename steering also handles determination before the download ID returns", async () => {
  const h = makeMapHarness();
  h.downloads.determineBeforeReturn = true;
  h.scripting.verdict = { kind: "article", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_early_name_0001", { title: EXPECTED_TITLE }));
  await landOnProvider(h, "job_early_name_0001");
  expect(h.downloads.items.get(701)?.filename).toBe(
    "/Users/test/Downloads/papio/job_early_name_0001/out.pdf",
  );
});

test("classification is gated on an optional-host-permission grant", async () => {
  const h = makeMapHarness();
  h.permissions.granted = false;
  h.scripting.verdict = { kind: "article", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_nogrant_0001", { title: EXPECTED_TITLE }));
  await landOnProvider(h, "job_nogrant_0001");
  expect(h.scripting.interpretTabs.length).toBe(0);
  expect(h.downloads.started.length).toBe(0);
  expect(h.permissions.checks).toContainEqual([`https://${PROVIDER}/*`]);
});

test("no registered adapter for the host stays assisted (no injection)", async () => {
  const h = makeMapHarness([]);
  h.scripting.verdict = { kind: "article", adapter_id: "x", adapter_version: "0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_noadapter_0001"));
  await landOnProvider(h, "job_noadapter_0001");
  expect(h.scripting.interpretTabs.length).toBe(0);
});

test("terms/no_entitlement/wrong_work map to their provider outcomes", async () => {
  const cases: { kind: PageVerdict["kind"]; outcome: string }[] = [
    { kind: "terms", outcome: "terms_acceptance_required" },
    { kind: "no_entitlement", outcome: "no_entitlement" },
    { kind: "wrong_work", outcome: "wrong_work" },
    { kind: "wrong_work_check", outcome: "wrong_work" },
  ];
  for (const c of cases) {
    const h = makeMapHarness();
    h.scripting.verdict = { kind: c.kind, adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
    await h.bridge.start();
    const jobID = `job_${c.kind}_0001`;
    await h.port.inbound(offer(jobID));
    await landOnProvider(h, jobID);
    const outcome = h.frames().find((f) => f.type === "provider_outcome");
    expect(outcome?.payload["outcome"]).toBe(c.outcome);
    expect(outcome?.payload["adapter_version"]).toBe("0.3.1");
    expect(h.downloads.started.length).toBe(0);
  }
});

test("login verdict stays auth_pending — no outcome frame, no click", async () => {
  const h = makeMapHarness();
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_login_0001"));
  await landOnProvider(h, "job_login_0001");
  expect(h.frames().some((f) => f.type === "provider_outcome")).toBe(false);
  expect(h.downloads.started.length).toBe(0);
});

// Federated login-routing: on a login wall, when the adapter declares a
// federatedLogin route and the offer carried the institution entityID, papio
// navigates the handoff tab straight to the IdP (auto-selecting the institution)
// — credential entry still stays with the human.
const FED_LOGIN_SPEC: AdapterSpec = {
  id: "proquest",
  version: "1.0.0",
  hosts: [PROVIDER],
  classify: [{ kind: "login", all: ["#login-form"] }],
  download: { selector: "a.download-pdf", requireKind: "article", method: "href", workTarget: { kind: "opaque" } },
  termsAccept: { modalSelector: "div.terms[open]", textAny: ["accept and download"] },
  federatedLogin: "https://sp.example/Shibboleth.sso/DS?entityID={entityID}&target=https://sp.example/home",
};

// Account-id unlock (ProQuest): on a login wall, appending ?accountid=<id> to
// the current URL unlocks institutional access with no sign-in. Preferred over
// the federated route when the offer carries an account id.
const ACCT_SPEC: AdapterSpec = {
  id: "proquest",
  version: "1.0.0",
  hosts: [PROVIDER],
  classify: [{ kind: "login", all: ["#login-form"] }],
  accountIdParam: "accountid",
  federatedLogin: "https://sp.example/Shibboleth.sso/DS?entityID={entityID}&target=https://sp.example/home",
};

test("login verdict appends the account id to the current URL, preferring it over federated login", async () => {
  const h = makeMapHarness([ACCT_SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_acct_0001", undefined, [PROVIDER], "https://idp.example.edu/entity", "12345"));
  const tabID = await landOnProvider(h, "job_acct_0001", PROVIDER, `https://${PROVIDER}/openurl/handler/x`);
  const nav = h.tabs.navigations.map((navigation) => navigation.url);
  expect(nav).toContain(`https://${PROVIDER}/openurl/handler/x?accountid=12345`);
  // Account id preferred: no federated (DS) navigation.
  expect(nav.some((u) => u?.includes("Shibboleth.sso/DS"))).toBe(false);
});

test("account id unlock does not fire without an offer account id (falls back to federated)", async () => {
  const h = makeMapHarness([ACCT_SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_acct_noacct_0001", undefined, [PROVIDER], "https://idp.example.edu/entity"));
  const tabID = await landOnProvider(h, "job_acct_noacct_0001", PROVIDER, `https://${PROVIDER}/openurl/handler/x`);
  const nav = h.tabs.navigations.map((navigation) => navigation.url);
  expect(nav.some((u) => u?.includes("accountid="))).toBe(false);
  expect(nav.some((u) => u?.includes("Shibboleth.sso/DS"))).toBe(true);
});

test("login verdict routes the handoff tab to the federated login with the offer entityID", async () => {
  const h = makeMapHarness([FED_LOGIN_SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_fedlogin_0001", undefined, [PROVIDER], "https://idp.example.edu/entity"));
  const tabID = await landOnProvider(h, "job_fedlogin_0001");
  expect(h.tabs.navigations).toContainEqual({
    tabID,
    url: "https://sp.example/Shibboleth.sso/DS?entityID=https%3A%2F%2Fidp.example.edu%2Fentity&target=https://sp.example/home",
  });
  // Still a human sign-in step: no outcome, no download.
  expect(h.frames().some((f) => f.type === "provider_outcome")).toBe(false);
  expect(h.downloads.started.length).toBe(0);
});

test("login verdict does not route without an offer entityID", async () => {
  const h = makeMapHarness([FED_LOGIN_SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_fedlogin_noent_0001"));
  await landOnProvider(h, "job_fedlogin_noent_0001");
  expect(h.tabs.navigations.some((navigation) => navigation.url !== undefined)).toBe(false);
});

test("login verdict does not re-route while the human is signing in (latched)", async () => {
  const h = makeMapHarness([FED_LOGIN_SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_fedlogin_latch_0001", undefined, [PROVIDER], "https://idp.example.edu/entity"));
  await landOnProvider(h, "job_fedlogin_latch_0001");
  await landOnProvider(h, "job_fedlogin_latch_0001");
  const routes = h.tabs.navigations;
  expect(routes.length).toBe(1);
});
test("duplicate loading callbacks for papio's own federated route are not operator evidence", async () => {
  const h = makeMapHarness([FED_LOGIN_SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_fedlogin_route_race_0001", undefined, [PROVIDER], "https://idp.example.edu/entity"));
  const tabID = await landOnProvider(h, "job_fedlogin_route_race_0001");
  const route = "https://sp.example/Shibboleth.sso/DS?entityID=https%3A%2F%2Fidp.example.edu%2Fentity&target=https://sp.example/home";
  await h.tabs.update(tabID, { url: route });
  await h.tabs.update(tabID, { url: route });
  await h.tabs.userNavigate(tabID, `https://${PROVIDER}/pqdweb?doc=route-race`);
  expect(h.tabs.navigations.filter((navigation) => navigation.url === OPENURL)).toHaveLength(0);
});


test("federated login return re-drives the openurl once, warm, to reach the article", async () => {
  const h = makeMapHarness([FED_LOGIN_SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_fedredrive_0001", undefined, [PROVIDER], "https://idp.example.edu/entity"));
  const tabID = await landOnProvider(h, "job_fedredrive_0001");
  // Simulate the federated round-trip: tab goes to the IdP, then returns.
  await h.tabs.completeNavigation(tabID, "https://sp.example/Shibboleth.sso/DS?entityID=https%3A%2F%2Fidp.example.edu%2Fentity&target=https://sp.example/home");
  const idp = "https://idp.example.edu/idp/profile/SAML2/Redirect/SSO";
  await h.tabs.userNavigate(tabID, idp);
  expect(h.backend.store.activeJobs[0]?.status).toBe("auth_pending");
  const prov = `https://${PROVIDER}/pqdweb?doc=1`;
  await h.tabs.userNavigate(tabID, prov);
  // On the auth return, papio re-drives the original openurl exactly once.
  const openurlDrives = h.tabs.navigations.filter((navigation) => navigation.url === OPENURL);
  expect(openurlDrives.length).toBe(1);
  expect(openurlDrives[0]?.tabID).toBe(tabID);
});
test("soft federated SPA return re-drives only after a non-login classification", async () => {
  const h = makeMapHarness([FED_LOGIN_SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_fed_soft_article_0001", undefined, [PROVIDER], "https://idp.example.edu/entity"));
  const tabID = await landOnProvider(h, "job_fed_soft_article_0001");
  await h.tabs.userNavigate(tabID, "https://idp.example.edu/idp/profile/SAML2/Redirect/SSO");
  expect(h.backend.store.activeJobs[0]?.status).toBe("auth_pending");
  h.scripting.verdict = { kind: "article", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
  await h.tabs.userNavigate(tabID, `https://${PROVIDER}/pqdweb?doc=soft-article`);
  expect(h.tabs.navigations.filter((navigation) => navigation.url === OPENURL)).toHaveLength(1);
});

test("soft federated SPA return showing login does not re-drive", async () => {
  const h = makeMapHarness([FED_LOGIN_SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_fed_soft_login_0001", undefined, [PROVIDER], "https://idp.example.edu/entity"));
  const tabID = await landOnProvider(h, "job_fed_soft_login_0001");
  await h.tabs.userNavigate(tabID, "https://idp.example.edu/idp/profile/SAML2/Redirect/SSO");
  await h.tabs.userNavigate(tabID, `https://${PROVIDER}/pqdweb?doc=soft-login`);

  expect(h.tabs.navigations.filter((navigation) => navigation.url === OPENURL)).toHaveLength(0);
});
test("federated auth evidence does not apply article, terms, or terminal verdicts", async () => {
  const kinds: PageVerdict["kind"][] = ["article", "terms", "no_entitlement", "wrong_work_check"];
  for (const [index, kind] of kinds.entries()) {
    const h = makeMapHarness([FED_LOGIN_SPEC]);
    h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
    await h.bridge.start();
    const jobID = `job_fed_evidence_only_${index}`;
    await h.port.inbound(offer(jobID, undefined, [PROVIDER], "https://idp.example.edu/entity"));
    const tabID = await landOnProvider(h, jobID);
    await h.tabs.userNavigate(tabID, "https://idp.example.edu/idp/profile/SAML2/Redirect/SSO");
    h.scripting.verdict = { kind, adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
    await h.tabs.userNavigate(tabID, `https://${PROVIDER}/pqdweb?doc=evidence-only`);
    expect(h.tabs.navigations.filter((navigation) => navigation.url === OPENURL)).toHaveLength(1);
    expect(h.downloads.started).toHaveLength(0);
    expect(h.scripting.clicked).toHaveLength(0);
    expect(h.scripting.termsAccepts).toHaveLength(0);
    expect(h.frames().some((frame) => frame.type === "provider_outcome")).toBe(false);
  }
});
test("unknown federated soft return retries evidence after the DOM upgrades", async () => {
  const h = makeMapHarness([FED_LOGIN_SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_fed_evidence_retry_0001", undefined, [PROVIDER], "https://idp.example.edu/entity"));
  const tabID = await landOnProvider(h, "job_fed_evidence_retry_0001");
  await h.tabs.userNavigate(tabID, "https://idp.example.edu/idp/profile/SAML2/Redirect/SSO");
  h.scripting.verdict = { kind: "unknown", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
  await h.tabs.userNavigate(tabID, `https://${PROVIDER}/pqdweb?doc=evidence-retry`);
  const evidenceTimer = h.timers.find((timer) => timer.ms === 2500);
  expect(evidenceTimer).toBeDefined();
  h.scripting.verdict = { kind: "article", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
  await evidenceTimer?.fn();
  expect(h.tabs.navigations.filter((navigation) => navigation.url === OPENURL)).toHaveLength(1);
  expect(h.downloads.started).toHaveLength(0);
});



test("unknown escalates to ui_changed only on the second observation ≥5s later", async () => {
  const h = makeMapHarness();
  h.scripting.verdict = { kind: "unknown", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_unknown_0001"));

  // First unknown: no outcome, streak recorded.
  await landOnProvider(h, "job_unknown_0001");
  expect(h.frames().some((f) => f.type === "provider_outcome")).toBe(false);
  expect(h.backend.store.activeJobs[0]?.unknown_count).toBe(1);

  // Second unknown, ≥5s later: ui_changed emitted once.
  h.clock.now += 5000;
  await landOnProvider(h, "job_unknown_0001");
  const outcomes = h.frames().filter((f) => f.type === "provider_outcome");
  expect(outcomes.length).toBe(1);
  expect(outcomes[0]?.payload["outcome"]).toBe("ui_changed");
});

test("two unknowns <5s apart do not escalate", async () => {
  const h = makeMapHarness();
  h.scripting.verdict = { kind: "unknown", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_unknown_0002"));
  await landOnProvider(h, "job_unknown_0002");
  h.clock.now += 4000;
  await landOnProvider(h, "job_unknown_0002");
  expect(h.frames().some((f) => f.type === "provider_outcome")).toBe(false);
});

test("a decisive verdict between two unknowns resets the streak", async () => {
  const h = makeMapHarness();
  await h.bridge.start();
  await h.port.inbound(offer("job_reset_0001"));

  h.scripting.verdict = { kind: "unknown", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await landOnProvider(h, "job_reset_0001");
  expect(h.backend.store.activeJobs[0]?.unknown_count).toBe(1);

  // A login page (decisive) breaks the streak.
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  h.clock.now += 6000;
  await landOnProvider(h, "job_reset_0001");
  expect(h.backend.store.activeJobs[0]?.unknown_count).toBe(0);

  // Next unknown starts a fresh streak (count 1), so no ui_changed yet.
  h.scripting.verdict = { kind: "unknown", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  h.clock.now += 6000;
  await landOnProvider(h, "job_reset_0001");
  expect(h.frames().some((f) => f.type === "provider_outcome")).toBe(false);
  expect(h.backend.store.activeJobs[0]?.unknown_count).toBe(1);
});

const URL_SPEC: AdapterSpec = {
  id: "urlprov",
  version: "1.0.0",
  hosts: [PROVIDER],
  classify: [],
  download: {
    selector: "button.dl",
    requireKind: "article",
    method: "url",
    workTarget: { kind: "opaque" },
    idPattern: "/stable/([^?#]+)",
    urlTemplate: "https://provider.example.edu/pdf/{id}.pdf",
    requiresTermsConsent: true,
  },
};
const URL_SPEC_FIXTURE_URL = `https://${PROVIDER}/stable/4093878`;

test("url-method adapter fetches the direct endpoint autonomously with terms consent", async () => {
  // JSTOR-class: the entitled PDF is at a constructible URL. With consent,
  // fetch it via the downloads API — no click, no gesture.
  const h = makeMapHarness([URL_SPEC]);
  h.scripting.documentURL = URL_SPEC_FIXTURE_URL;
  h.settings.consent = "accept";
  h.scripting.constructedURL = "https://provider.example.edu/pdf/4093878.pdf";
  h.scripting.verdict = { kind: "article", adapter_id: "urlprov", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_url_0001"));
  await landOnProvider(h, "job_url_0001", PROVIDER, URL_SPEC_FIXTURE_URL);

  expect(h.scripting.clicked.length).toBe(0); // no gesture click
  expect(h.downloads.started.length).toBe(1);
  expect(h.downloads.started[0]?.url).toBe("https://provider.example.edu/pdf/4093878.pdf");
  expect(h.backend.store.activeJobs[0]?.download_initiated).toBe(true);
});

test("url-method adapter stays assisted (prompts, no fetch) without terms consent", async () => {
  const h = makeMapHarness([URL_SPEC]);
  h.scripting.documentURL = URL_SPEC_FIXTURE_URL;
  // consent undefined -> gate stays human
  h.scripting.verdict = { kind: "article", adapter_id: "urlprov", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_url_0002"));
  await landOnProvider(h, "job_url_0002", PROVIDER, URL_SPEC_FIXTURE_URL);

  expect(h.downloads.started.length).toBe(0); // no autonomous fetch
  expect(h.backend.store.activeJobs[0]?.needs_terms_consent).toBe(true);
  expect(h.backend.store.activeJobs[0]?.download_initiated).not.toBe(true);
  const prompts = h.frames().filter((f) => f.type === "provider_outcome" && f.payload["outcome"] === "terms_acceptance_required");
  expect(prompts.length).toBeGreaterThanOrEqual(1);
});

test("resolveDownloadURL (url): builds the endpoint from a multi-group template + gate", async () => {
  const win = new Window({ url: "https://www.jstor.org/stable/4093878?seq=1" });
  win.document.body.innerHTML = "<button class='dl'></button>";
  const prev = { document: globalThis.document, location: globalThis.location };
  Object.assign(globalThis, { document: win.document, location: { href: "https://www.jstor.org/stable/4093878?seq=1" } });
  try {
    expect(await resolveDownloadURL("button.dl", "/stable/([^?#]+)", "https://www.jstor.org/stable/pdf/{id}.pdf", null)).toBe(
      "https://www.jstor.org/stable/pdf/4093878.pdf",
    );
    // entitlement gate: control absent -> null (never fetch a non-downloadable page)
    win.document.body.innerHTML = "";
    expect(await resolveDownloadURL("button.dl", null, "https://x/y.pdf", null)).toBeNull();
  } finally {
    Object.assign(globalThis, prev);
  }
});

test("resolveDownloadURL (api): fetches the aggregator JSON and extracts the PDF URL", async () => {
  // EBSCO's exact two-step: viewer URL -> aggregator API (JSON {url}) -> content URL.
  const win = new Window({ url: "https://research.ebsco.com/c/6to2aa/viewer/pdf/mhqkskujrf?route=details" });
  win.document.body.innerHTML = "<div id='v'></div>";
  let fetched = "";
  const prev = { document: globalThis.document, location: globalThis.location, fetch: globalThis.fetch };
  Object.assign(globalThis, {
    document: win.document,
    location: { href: "https://research.ebsco.com/c/6to2aa/viewer/pdf/mhqkskujrf?route=details" },
    fetch: async (u: string) => {
      fetched = u;
      return { ok: true, json: async () => ({ url: "https://content.ebscohost.com/cds/retrieve?content=TOKEN" }) } as Response;
    },
  });
  try {
    const url = await resolveDownloadURL(
      "#v",
      "/c/([^/]+)/viewer/pdf/([^/?#]+)",
      "https://research.ebsco.com/api/researcher-edge-aggregator/v1/records/{2}/fulltext/pdf?sourceRecordId={2}&opid={1}&intent=view",
      "url",
    );
    expect(fetched).toBe(
      "https://research.ebsco.com/api/researcher-edge-aggregator/v1/records/mhqkskujrf/fulltext/pdf?sourceRecordId=mhqkskujrf&opid=6to2aa&intent=view",
    );
    expect(url).toBe("https://content.ebscohost.com/cds/retrieve?content=TOKEN");
  } finally {
    Object.assign(globalThis, prev);
  }
});

test("cross-origin api download is relocated into papio/<job>/ via the ID bound at onCreated", async () => {
  const h = makeMapHarness();
  // Chrome fires onCreated (pre-redirect) before asking for the filename.
  h.downloads.emitOnCreated = true;
  // The provider's entitled download redirects to a different origin, so the
  // determine-time URL no longer matches the pending offer (the EBSCO case:
  // research.ebsco.com -> content.ebscohost.com).
  h.downloads.crossOriginRedirect = "https://content.ebscohost.com/cds/retrieve?content=signed";
  // The filename is determined before downloads.download resolves, so the
  // initiation code has not tracked the returned ID yet — only onCreated has.
  h.downloads.determineBeforeReturn = true;
  h.scripting.verdict = { kind: "article", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_xorigin_0001", { title: EXPECTED_TITLE }));
  await landOnProvider(h, "job_xorigin_0001");
  // Despite the cross-origin determine URL (no pending-URL match) and the ID
  // not yet tracked by the initiation code, the file lands under papio/<job>/
  // because onCreated bound the download ID to the job synchronously.
  expect(h.downloads.items.get(701)?.filename).toBe(
    "/Users/test/Downloads/papio/job_xorigin_0001/out.pdf",
  );
});

// Primo NDE renders Ex Libris's own "Get PDF" delivery anchor on entitled and
// Open Access full-display records; the language-independent key is the
// /discovery/sourceRecord href, and the live anchor carries the delivery
// query that sanitizeFixture strips from the stored capture.
const primoRecord = loadFixture("primo", "success");
test.skipIf(primoRecord === null)(
  "captured Primo record classifies on its Get PDF delivery anchor",
  () => {
    const record = primoRecord as Document;
    const spec = adapters.find((a) => a.id === "primo") as AdapterSpec;
    const verdict = interpret(record, spec, ctx());
    expect(verdict.kind).toBe("article");
    const rule = spec.download as DownloadRule;
    expect(rule.method).toBe("href");
    const anchor = record.querySelector(rule.selector);
    expect(anchor?.getAttribute("aria-label") ?? "").toContain("Get PDF");
    expect(anchor?.getAttribute("href") ?? "").toContain("/discovery/sourceRecord");
  },
);
test.skipIf(primoRecord === null)(
  "a Primo page without the delivery anchor stays assisted",
  () => {
    const record = primoRecord as Document;
    const spec = adapters.find((a) => a.id === "primo") as AdapterSpec;
    const stripped = record.cloneNode(true) as Document;
    for (const el of Array.from(stripped.querySelectorAll("a.anchor-tag-style[href*='/discovery/sourceRecord']"))) {
      el.remove();
    }
    expect(interpret(stripped, spec, ctx()).kind).not.toBe("article");
  },
);

// ClinicalKey's SPA renders a stable watermarked-PDF download anchor on
// entitled full-text articles; method href rides the site's own endpoint
// with session cookies.
const clinicalKeyArticle = loadFixture("clinicalkey", "success");
test.skipIf(clinicalKeyArticle === null)(
  "captured ClinicalKey article classifies on its watermarked PDF anchor",
  () => {
    const article = clinicalKeyArticle as Document;
    const spec = adapters.find((a) => a.id === "clinicalkey") as AdapterSpec;
    expect(interpret(article, spec, ctx()).kind).toBe("article");
    const rule = spec.download as DownloadRule;
    expect(rule.method).toBe("href");
    const anchor = article.querySelector(rule.selector);
    expect(anchor?.getAttribute("href") ?? "").toMatch(
      /\/service\/content\/pdf\/watermarked\/.+\.pdf$/,
    );
  },
);
test.skipIf(clinicalKeyArticle === null)(
  "a ClinicalKey page without the download anchor stays assisted",
  () => {
    const article = clinicalKeyArticle as Document;
    const spec = adapters.find((a) => a.id === "clinicalkey") as AdapterSpec;
    const stripped = article.cloneNode(true) as Document;
    for (const el of Array.from(stripped.querySelectorAll("a[data-testid='pdf-download-link']"))) el.remove();
    expect(interpret(stripped, spec, ctx()).kind).not.toBe("article");
  },
);


const mdpiArticle = loadFixture("mdpi", "success");
test.skipIf(mdpiArticle === null)(
  "captured MDPI article classifies on its PDF metadata and download anchor",
  () => {
    const article = mdpiArticle as Document;
    const spec = adapters.find((a) => a.id === "mdpi") as AdapterSpec;
    const verdict = interpret(article, spec, ctx());
    expect(verdict.kind).toBe("article");
    expect(verdict.adapter_id).toBe("mdpi");
    const rule = spec.download as DownloadRule;
    expect(rule.method).toBe("href");
    expect(article.querySelector(rule.selector)?.getAttribute("href")).toBe(
      "/2227-7102/12/6/369/pdf",
    );
  },
);

test.skipIf(mdpiArticle === null)(
  "an MDPI metadata shell without the provider download anchor stays assisted",
  () => {
    const article = (mdpiArticle as Document).cloneNode(true) as Document;
    const spec = adapters.find((a) => a.id === "mdpi") as AdapterSpec;
    for (const anchor of Array.from(article.querySelectorAll("a.UD_ArticlePDF"))) anchor.remove();
    expect(interpret(article, spec, ctx()).kind).toBe("unknown");
  },
);

const hogrefeArticle = loadFixture("hogrefe", "success");
test.skipIf(hogrefeArticle === null)(
  "captured Hogrefe article classifies on its direct PDF anchor",
  () => {
    const article = hogrefeArticle as Document;
    const spec = adapters.find((a) => a.id === "hogrefe") as AdapterSpec;
    const verdict = interpret(article, spec, ctx());
    expect(verdict.kind).toBe("article");
    expect(verdict.adapter_id).toBe("hogrefe");
    const rule = spec.download as DownloadRule;
    expect(rule.method).toBe("href");
    expect(article.querySelector(rule.selector)?.getAttribute("href")).toBe(
      "/doi/pdf/10.1024/2673-8627/a000074?download=true",
    );
  },
);

test.skipIf(hogrefeArticle === null)(
  "a Hogrefe page without the direct PDF anchor stays assisted",
  () => {
    const article = (hogrefeArticle as Document).cloneNode(true) as Document;
    const spec = adapters.find((a) => a.id === "hogrefe") as AdapterSpec;
    for (const anchor of Array.from(article.querySelectorAll("a[href^='/doi/pdf/']"))) anchor.remove();
    expect(interpret(article, spec, ctx()).kind).toBe("unknown");
  },
);