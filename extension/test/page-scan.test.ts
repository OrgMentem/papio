// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// ADR-0019 Decision 3: pure DOM tests for the on-page detector. scanDocument
// takes no chrome.* dependency, so every case here runs against a happy-dom
// document built by harness.ts's parseHTML \u2014 no extension runtime needed.

import { expect, test } from "bun:test";

import { parseHTML } from "./harness";
import { PAGE_BULK_RAW_CANDIDATE_CAP, scanDocument, type DetectedPaper } from "../src/page-scan";

function doc(html: string, baseURL = "https://scholar.example.edu/refs"): Document {
  return parseHTML(`<!doctype html><html><body>${html}</body></html>`, baseURL);
}

function identifiers(papers: readonly DetectedPaper[]): Array<{ kind: string; value: string }> {
  return papers.map((p) => p.identifier);
}

// --- Recognized links (Decision 3 recognition order, item 1) ---------------

test("a doi.org link is recognized as a DOI", () => {
  const result = scanDocument(doc(`<ul><li><a href="https://doi.org/10.1234/abcd.5678">A paper</a></li></ul>`));
  expect(identifiers(result.papers)).toEqual([{ kind: "doi", value: "10.1234/abcd.5678" }]);
});

test("a dx.doi.org link is recognized as a DOI", () => {
  const result = scanDocument(doc(`<li><a href="https://dx.doi.org/10.5555/xyz.1">Paper</a></li>`));
  expect(identifiers(result.papers)).toEqual([{ kind: "doi", value: "10.5555/xyz.1" }]);
});

test("a publisher /doi/10.x path is recognized as a DOI", () => {
  const result = scanDocument(
    doc(`<li><a href="https://onlinelibrary.wiley.com/doi/10.1002/anie.202012345">Wiley paper</a></li>`),
  );
  expect(identifiers(result.papers)).toEqual([{ kind: "doi", value: "10.1002/anie.202012345" }]);
});

test("a publisher /doi/abs/10.x and /doi/epdf/10.x path both resolve to the same DOI", () => {
  const result = scanDocument(
    doc(`
      <li><a href="https://onlinelibrary.wiley.com/doi/abs/10.1002/anie.202099999">Abstract</a></li>
      <li><a href="https://onlinelibrary.wiley.com/doi/epdf/10.1002/anie.202099999">PDF</a></li>
    `),
  );
  expect(result.papers).toHaveLength(1);
  expect(result.papers[0]?.identifier).toEqual({ kind: "doi", value: "10.1002/anie.202099999" });
  expect(result.papers[0]?.occurrences).toBe(2);
});

test("an arxiv.org /abs/ link is recognized as arXiv", () => {
  const result = scanDocument(doc(`<li><a href="https://arxiv.org/abs/2101.00001">Preprint</a></li>`));
  expect(identifiers(result.papers)).toEqual([{ kind: "arxiv", value: "2101.00001" }]);
});

test("an arxiv.org /pdf/ link strips the .pdf suffix", () => {
  const result = scanDocument(doc(`<li><a href="https://arxiv.org/pdf/2101.00001.pdf">PDF</a></li>`));
  expect(identifiers(result.papers)).toEqual([{ kind: "arxiv", value: "2101.00001" }]);
});

test("a pubmed.ncbi.nlm.nih.gov article link is recognized as a PMID", () => {
  const result = scanDocument(doc(`<li><a href="https://pubmed.ncbi.nlm.nih.gov/12345678/">Article</a></li>`));
  expect(identifiers(result.papers)).toEqual([{ kind: "pmid", value: "12345678" }]);
});

// --- Explicitly labeled text (Decision 3 recognition order, item 2) --------

test("a strict DOI in plain text is recognized without a link", () => {
  const result = scanDocument(doc(`<p>See 10.1000/xyz123 for the full record.</p>`));
  expect(identifiers(result.papers)).toEqual([{ kind: "doi", value: "10.1000/xyz123" }]);
});

test("an arXiv: labeled id in plain text is recognized", () => {
  const result = scanDocument(doc(`<p>Preprint arXiv:2101.00001 discusses this.</p>`));
  expect(identifiers(result.papers)).toEqual([{ kind: "arxiv", value: "2101.00001" }]);
});

test("a PMID: labeled id in plain text is recognized", () => {
  const result = scanDocument(doc(`<p>See PMID: 87654321 for details.</p>`));
  expect(identifiers(result.papers)).toEqual([{ kind: "pmid", value: "87654321" }]);
});

test("an unlabeled bare integer is never recognized as a PMID", () => {
  const result = scanDocument(doc(`<p>See reference 87654321 in the appendix.</p>`));
  expect(result.papers).toHaveLength(0);
});

// --- Local trailing-punctuation cleanup, never a second canonicalizer ------

test("trailing punctuation around a plain-text DOI is trimmed", () => {
  const result = scanDocument(doc(`<p>The paper (10.1234/abcd.5678) is cited here.</p>`));
  expect(identifiers(result.papers)).toEqual([{ kind: "doi", value: "10.1234/abcd.5678" }]);
});

test("trailing punctuation on a doi.org link path is trimmed", () => {
  const result = scanDocument(doc(`<li><a href="https://doi.org/10.1234/abcd.5678,">Paper</a></li>`));
  expect(identifiers(result.papers)).toEqual([{ kind: "doi", value: "10.1234/abcd.5678" }]);
});

// --- Own-page canonical identifier exclusion --------------------------------

test("the page's own citation_doi meta is excluded from the bulk list", () => {
  const html = `
    <head><meta name="citation_doi" content="10.1234/own.page"></head>
    <body>
      <li><a href="https://doi.org/10.1234/own.page">Self</a></li>
      <li><a href="https://doi.org/10.9999/other.paper">Other</a></li>
    </body>
  `;
  const result = scanDocument(parseHTML(`<!doctype html><html>${html}</html>`, "https://scholar.example.edu/article"));
  expect(identifiers(result.papers)).toEqual([{ kind: "doi", value: "10.9999/other.paper" }]);
});

test("the page's own canonical <link> is excluded from the bulk list", () => {
  const html = `
    <head><link rel="canonical" href="https://doi.org/10.1234/own.canonical"></head>
    <body>
      <li><a href="https://doi.org/10.1234/own.canonical">Self</a></li>
      <li><a href="https://doi.org/10.9999/other.paper">Other</a></li>
    </body>
  `;
  const result = scanDocument(parseHTML(`<!doctype html><html>${html}</html>`, "https://scholar.example.edu/article"));
  expect(identifiers(result.papers)).toEqual([{ kind: "doi", value: "10.9999/other.paper" }]);
});

test("the page's own og:url is excluded from the bulk list", () => {
  const html = `
    <head><meta property="og:url" content="https://doi.org/10.1234/own.og"></head>
    <body>
      <li><a href="https://doi.org/10.1234/own.og">Self</a></li>
      <li><a href="https://doi.org/10.9999/other.paper">Other</a></li>
    </body>
  `;
  const result = scanDocument(parseHTML(`<!doctype html><html>${html}</html>`, "https://scholar.example.edu/article"));
  expect(identifiers(result.papers)).toEqual([{ kind: "doi", value: "10.9999/other.paper" }]);
});

// --- Container labels, occurrence merging -----------------------------------

test("the label is the nearest citation-shaped container's normalized visible text, capped at 240 chars", () => {
  const longTitle = "A very long citation title that goes on and on ".repeat(8).trim();
  const result = scanDocument(
    doc(`<li>${longTitle} <a href="https://doi.org/10.1234/abcd.5678">DOI</a></li>`),
  );
  expect(result.papers).toHaveLength(1);
  expect(result.papers[0]?.label.length).toBeLessThanOrEqual(240);
  expect(result.papers[0]?.label.startsWith("A very long citation title")).toBe(true);
});

test("closest()'s [class*='result'] match on a page-level wrapper is overridden by the nearest bounded row: 5 bare-div citation rows get 5 distinct labels", () => {
  // A common non-<li>/<article> results layout: bare <div> rows (matching
  // none of CONTAINER_SELECTOR's tag/class terms) inside one wrapper whose
  // OWN class happens to contain "result". Pre-fix, start.closest(...) walks
  // past every row (none matches) and returns the wrapper, so every DOI on
  // the page gets the identical wrapper-wide (concatenated) label.
  const rows = Array.from({ length: 5 }, (_, i) => `
    <div class="entry">
      Author ${i}, J. (2026). Distinctive title number ${i} about generic identifier
      detection in citation lists rendered as bare divs, not list items.
      <a href="https://doi.org/10.1234/row.${i}">View</a>
    </div>
  `).join("\n");
  const result = scanDocument(doc(`<div class="search-results-list">${rows}</div>`));

  expect(result.papers).toHaveLength(5);
  const labels = result.papers.map((p) => p.label);
  expect(new Set(labels).size).toBe(5);
  for (let i = 0; i < 5; i += 1) {
    expect(labels[i]).toContain(`Distinctive title number ${i}`);
    for (let j = 0; j < 5; j += 1) {
      if (j !== i) expect(labels[i]).not.toContain(`Distinctive title number ${j}`);
    }
  }
});

test("normalized label collapses internal whitespace", () => {
  const result = scanDocument(
    doc(`<li>Some\n   messy\t\twhitespace <a href="https://doi.org/10.1234/abcd.5678">DOI</a></li>`),
  );
  expect(result.papers[0]?.label).toBe("Some messy whitespace DOI");
});

test("the same identifier seen twice merges into one entry with occurrences: 2", () => {
  const result = scanDocument(
    doc(`
      <li><a href="https://doi.org/10.1234/abcd.5678">First mention</a></li>
      <p>Also see 10.1234/abcd.5678 again.</p>
    `),
  );
  expect(result.papers).toHaveLength(1);
  expect(result.papers[0]?.occurrences).toBe(2);
});

test("a link's own visible text repeating its href DOI is not double-counted", () => {
  const result = scanDocument(
    doc(`<li><a href="https://doi.org/10.1234/abcd.5678">10.1234/abcd.5678</a></li>`),
  );
  expect(result.papers).toHaveLength(1);
  expect(result.papers[0]?.occurrences).toBe(1);
});

// --- 200-candidate cap with explicit truncation -----------------------------

test("raw candidates cap at 200 with truncated reported, never silent", () => {
  const items = Array.from(
    { length: 250 },
    (_, i) => `<li><a href="https://doi.org/10.1234/paper.${String(i).padStart(4, "0")}">Paper ${i}</a></li>`,
  ).join("\n");
  const result = scanDocument(doc(`<ul>${items}</ul>`));
  expect(result.papers).toHaveLength(PAGE_BULK_RAW_CANDIDATE_CAP);
  expect(result.truncated).toBe(true);
});

test("under the cap, truncated is false", () => {
  const result = scanDocument(doc(`<li><a href="https://doi.org/10.1234/abcd.5678">Paper</a></li>`));
  expect(result.truncated).toBe(false);
});

// --- script/style/hidden/extension-injected nodes are skipped ---------------

test("script tag contents are never scanned", () => {
  const result = scanDocument(doc(`<script>var doi = "10.1234/hidden.script";</script>`));
  expect(result.papers).toHaveLength(0);
});

test("style tag contents are never scanned", () => {
  const result = scanDocument(doc(`<style>/* 10.1234/hidden.style */</style>`));
  expect(result.papers).toHaveLength(0);
});

test("a hidden attribute element is skipped", () => {
  const result = scanDocument(doc(`<div hidden>10.1234/hidden.attr</div>`));
  expect(result.papers).toHaveLength(0);
});

test("a display:none element is skipped", () => {
  const result = scanDocument(doc(`<div style="display: none">10.1234/hidden.style-inline</div>`));
  expect(result.papers).toHaveLength(0);
});

test("an aria-hidden=true element is skipped", () => {
  const result = scanDocument(doc(`<div aria-hidden="true">10.1234/hidden.aria</div>`));
  expect(result.papers).toHaveLength(0);
});

test("a visible sibling next to a hidden node is still scanned", () => {
  const result = scanDocument(
    doc(`<div hidden>10.1234/hidden.one</div><p>10.1234/visible.two</p>`),
  );
  expect(identifiers(result.papers)).toEqual([{ kind: "doi", value: "10.1234/visible.two" }]);
});

// --- extension-injected nodes: other-extension URL markers, shadow DOM -----

test("an anchor whose own href is a foreign chrome-extension:// URL is skipped entirely, not merely unrecognized as a link", () => {
  // Pre-fix: identifierFromURL rejects the chrome-extension:// scheme as a
  // link (no known host matches), so the walk fell through to scan the
  // anchor's own text as plain content and would have found the DOI there.
  const result = scanDocument(
    doc(`<li><a href="chrome-extension://other-ext-id/citation.html">10.1234/should.not.count</a></li>`),
  );
  expect(result.papers).toHaveLength(0);
});

test("a container carrying a foreign moz-extension:// src marker is skipped along with everything inside it", () => {
  const result = scanDocument(
    doc(`<div src="moz-extension://other-ext-id/widget-frame"><p>10.1234/ancestor.marked</p></div>`),
  );
  expect(result.papers).toHaveLength(0);
});

test("an extension-URL marker on one element does not suppress an unrelated sibling elsewhere in the page", () => {
  const result = scanDocument(
    doc(`<div><img src="chrome-extension://other-ext-id/icon.png"></div><p>10.1234/visible.sibling</p>`),
  );
  expect(identifiers(result.papers)).toEqual([{ kind: "doi", value: "10.1234/visible.sibling" }]);
});

test("text placed only inside a Shadow DOM subtree is never scanned (childNodes never crosses a shadow boundary)", () => {
  const document = doc(`<div id="host"></div>`);
  const host = document.getElementById("host") as unknown as {
    attachShadow(init: { mode: "open" }): { innerHTML: string };
  };
  const shadow = host.attachShadow({ mode: "open" });
  shadow.innerHTML = "<p>10.1234/injected.shadow</p>";
  const result = scanDocument(document);
  expect(result.papers).toHaveLength(0);
});

test("scan result survives the executeScript serialization boundary", () => {
  // chrome.scripting.executeScript JSON-serializes the injected function's
  // return value: array elements survive, expando properties do not. The
  // result must therefore be a plain object — this test simulates the
  // boundary and would have caught the array-with-expando regression that
  // made every real-world scan report "Could not scan the page".
  const result = scanDocument(
    doc(`<ul><li><a href="https://doi.org/10.1234/abcd.5678">A paper</a></li></ul>`),
  );
  const crossed = JSON.parse(JSON.stringify(result)) as typeof result;
  expect(Array.isArray(crossed.papers)).toBe(true);
  expect(typeof crossed.truncated).toBe("boolean");
  expect(crossed.papers.length).toBe(1);
});
