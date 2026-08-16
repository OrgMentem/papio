// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// ADR-0019 Decision 3: pure DOM tests for the on-page detector. scanDocument
// takes no chrome.* dependency, so every case here runs against a happy-dom
// document built by harness.ts's parseHTML \u2014 no extension runtime needed.

import { expect, test } from "bun:test";

import { parseHTML } from "./harness";
import { PAGE_BULK_RAW_CANDIDATE_CAP, scanDocument, type DetectedPaper } from "../src/page-scan";
import { doiFromURL } from "../src/deliver";

function doc(html: string, baseURL = "https://scholar.example.edu/refs"): Document {
  return parseHTML(`<!doctype html><html><body>${html}</body></html>`, baseURL);
}

function identifiers(papers: readonly DetectedPaper[]): Array<{ kind: string; value: string }> {
  return papers.map((p) => p.identifier);
}

// papio has two DOI-from-URL extractors and cannot have one: `doiFromURL`
// (deliver.ts) is the authority, but `scanDocument` is serialized into the page
// by `chrome.scripting.executeScript`, which crosses only the function's own
// source — an imported helper is unresolved in page scope. So the duplication is
// structural, and this test is the mechanism that keeps it from drifting: both
// extractors run over one shared table and must agree.
//
// A row where they legitimately differ belongs in `differsByDesign` with a
// stated reason, never silently in one implementation.
test("the page scanner and the delivery extractor agree on DOI-bearing URLs", () => {
  const shared = [
    "https://doi.org/10.1234/abcd.5678",
    "https://doi.org/10.48612//monograph-2025-2",
    "https://dl.acm.org/doi/pdf/10.1145/3630106.3660000.pdf",
    "https://journals.sagepub.com/doi/abs/10.1177/01634437251234567",
    "https://onlinelibrary.wiley.com/doi/pdfdirect/10.1111/jcpp.13440",
    "https://link.springer.com/article/10.1007/s11192-024-04901-y",
    // Non-article routes: a supplement lives in its ARTICLE's DOI namespace, so
    // both extractors must decline rather than name the article.
    "https://dl.acm.org/doi/suppl/10.1145/3630106.3660000/suppl_file/appendix.pdf",
    // Article-view routes: the trailing segment names a view of the article, so
    // both must strip it and yield the article rather than decline.
    "https://www.emerald.com/insight/content/doi/10.1108/QEA-07-2024-0055/full/pdf",
    "https://journals.sagepub.com/doi/pdf/10.1177/01634437251234567/full",
    "https://www.tandfonline.com/doi/ref/10.1080/0144929X.2019.1578828",
    // A view marker fused onto the DOI cannot be trimmed off safely.
    "https://www.biorxiv.org/content/10.1101/2024.06.04.594010v1.full.pdf",
    // A component DOI's own multi-segment suffix is not a route.
    "https://doi.org/10.1109/tem.2022.3197196/mm1",
    "https://example.com/news/story-42",
  ];
  const differsByDesign = new Set<string>([
    // The scanner reads a link out of a citation list, so it has no page to
    // resolve a `?doi=` parameter against and does not implement that tier.
  ]);
  for (const url of shared) {
    if (differsByDesign.has(url)) continue;
    const scanned = scanDocument(doc(`<ul><li><a href="${url}">t</a></li></ul>`)).papers
      .filter((paper) => paper.identifier.kind === "doi")
      .map((paper) => paper.identifier.value)[0];
    expect({ url, doi: scanned }).toEqual({ url, doi: doiFromURL(url) });
  }
});

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

test("an openalex.org /works/w<digits> link is recognized as openalex, value uppercased", () => {
  const result = scanDocument(doc(`<li><a href="https://openalex.org/works/w1976043798">A paper</a></li>`));
  expect(identifiers(result.papers)).toEqual([{ kind: "openalex", value: "W1976043798" }]);
});

test("a bare openalex.org /w<digits> path (no /works/ segment) is recognized", () => {
  const result = scanDocument(doc(`<li><a href="https://openalex.org/w1976043798">A paper</a></li>`));
  expect(identifiers(result.papers)).toEqual([{ kind: "openalex", value: "W1976043798" }]);
});

test("www. and api. openalex.org hosts are both recognized", () => {
  const result = scanDocument(
    doc(`
      <li><a href="https://www.openalex.org/works/W2741809807">Paper A</a></li>
      <li><a href="https://api.openalex.org/works/W2741809808">Paper B</a></li>
    `),
  );
  expect(identifiers(result.papers)).toEqual([
    { kind: "openalex", value: "W2741809807" },
    { kind: "openalex", value: "W2741809808" },
  ]);
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

test("publisher service-link chrome is stripped from labels but never from titles", () => {
  // Frontiers-shaped row: citation text plus trailing CrossRef / Google
  // Scholar / View-reference anchors welded together in one <li>.
  const result = scanDocument(
    doc(
      `<li>Aina, C. (2013). Parental background and university dropout in Italy. High. Educ. 65, 437–456.` +
        ` <a href="https://doi.org/10.1007/s10734-012-9554-z">CrossRef</a>` +
        ` <a href="https://scholar.google.com/x">Google Scholar</a><a href="#B1">View reference in article</a></li>`,
    ),
  );
  expect(result.papers.length).toBe(1);
  const label = result.papers[0]!.label;
  expect(label).toContain("Parental background and university dropout in Italy");
  expect(label).not.toMatch(/CrossRef|Google Scholar|View reference/i);
  // Element boundaries get spaces: no "ScholarView"-style welding anywhere.
  expect(label).not.toMatch(/[a-z][A-Z]/);
});

test("a genuine title ending in a weak service word keeps it", () => {
  const result = scanDocument(
    doc(`<li><a href="https://doi.org/10.1234/weak.1">How to read a research article</a></li>`),
  );
  expect(result.papers[0]!.label).toContain("How to read a research article");
});

test("adjacent inline elements never weld words together", () => {
  const result = scanDocument(
    doc(`<li><span>1</span><span>Aina</span><span>C.</span> (2013) <a href="https://doi.org/10.1007/s10734-012-9554-z">x</a></li>`),
  );
  expect(result.papers[0]!.label).toContain("1 Aina C.");
});

test("a dt-anchored identifier labels from its dd sibling (arXiv listing shape)", () => {
  // arXiv's listing pages are definition lists: the <dt> holds the abs/pdf
  // links (pure chrome), the sibling <dd> holds the citation.
  const result = scanDocument(
    doc(
      `<dl>
        <dt>[1] <a href="https://arxiv.org/abs/2608.06340">arXiv:2608.06340</a> [<a href="https://arxiv.org/pdf/2608.06340">pdf</a>, <a href="#">html</a>, <a href="#">other</a>]</dt>
        <dd><div>Title: Handling Missing Data in Probabilistic Regression Trees</div><div>Authors: T. Prass, A. Neimaier</div><div>Subjects: Machine Learning (stat.ML)</div></dd>
        <dt>[2] <a href="https://arxiv.org/abs/2608.06337">arXiv:2608.06337</a></dt>
        <dd><div>Title: A Second Paper Entirely</div></dd>
      </dl>`,
    ),
  );
  expect(result.papers.length).toBe(2);
  expect(result.papers[0]!.label).toContain("Handling Missing Data in Probabilistic Regression Trees");
  expect(result.papers[0]!.label).not.toMatch(/\[\s*pdf|other\s*\]/i);
  expect(result.papers[1]!.label).toContain("A Second Paper Entirely");
  // Both dt anchors for one paper merge into one row.
  expect(result.papers[0]!.occurrences).toBe(2);
});

test("a dt without a dd sibling keeps its own text as the label", () => {
  const result = scanDocument(
    doc(`<dl><dt>Prass et al 2026 <a href="https://arxiv.org/abs/2608.06340">arXiv:2608.06340</a></dt></dl>`),
  );
  expect(result.papers[0]!.label).toContain("Prass et al 2026");
});

test("DOI-shaped path segments match without a /doi/ prefix (Scholar publisher links)", () => {
  for (const [href, doi] of [
    ["https://link.springer.com/article/10.1007/s10734-012-9554-z", "10.1007/s10734-012-9554-z"],
    ["https://link.springer.com/chapter/10.1007/978-3-031-42902-6_6", "10.1007/978-3-031-42902-6_6"],
    ["https://digital-library.example.org/content/10.1049/ip-cta:20020087", "10.1049/ip-cta:20020087"],
    ["https://journals.example.edu/download/10.5840/monist198669318.pdf", "10.5840/monist198669318"],
  ] as const) {
    const result = scanDocument(doc(`<li><a href="${href}">Paper</a></li>`));
    expect(result.papers.length).toBe(1);
    expect(result.papers[0]!.identifier).toEqual({ kind: "doi", value: doi });
  }
});

test("non-DOI numeric paths never match the generic segment rule", () => {
  for (const href of [
    "https://www.jstor.org/stable/10.2307",
    "https://example.org/posts/10.5",
    "https://example.org/v/10/12345",
  ]) {
    const result = scanDocument(doc(`<li><a href="${href}">Not a paper</a></li>`));
    expect(result.papers.length).toBe(0);
  }
});

test("card layouts label from the card heading when the identifier sits in an action row", () => {
  // Semantic Scholar-shaped card: title in a heading, identifier link in a
  // small button row, card text well past the bounded-container limit.
  const filler = "TLDR This paper studies preconditioning methods at considerable length. ".repeat(8);
  const result = scanDocument(
    doc(
      `<div class="result-page">
        <article class="card">
          <h3><a href="/paper/some-slug/12345">A new version of a preconditioning method for certain two-by-two block matrices</a></h3>
          <div class="authors">O. Axelsson, DK Salkuyeh</div>
          <div class="tldr">${filler}</div>
          <div class="actions">9 <a href="https://doi.org/10.1007/s10543-018-0741-x">Publisher (opens in a new tab)</a> <button>Save</button> <button>Cite</button></div>
        </article>
      </div>`,
    ),
  );
  expect(result.papers.length).toBe(1);
  expect(result.papers[0]!.label).toContain("preconditioning method for certain two-by-two block matrices");
  expect(result.papers[0]!.label).not.toMatch(/opens in a new tab|Save|Cite/i);
});

test("a real citation label is never mistaken for low-information chrome", () => {
  const result = scanDocument(
    doc(`<li>Aina, C. (2013). Parental background and university dropout. <a href="https://doi.org/10.1007/s10734-012-9554-z">CrossRef</a></li>`),
  );
  expect(result.papers[0]!.label).toContain("Parental background and university dropout");
});

// --- renderedRecordCountHint: honest structural denominator ----------------
// (dev/post-build-followups.md item 3). Counts rendered records for a
// recognized page-class family without reading their contents; null when no
// family is recognized, never a guess.

test("a definition-list page reports its <dt> row count as the hint", () => {
  const rows = Array.from(
    { length: 4 },
    (_, i) => `<dt><a href="https://arxiv.org/abs/210${i}.00001">arXiv:210${i}.00001</a></dt><dd>Title ${i}</dd>`,
  ).join("\n");
  const result = scanDocument(doc(`<dl>${rows}</dl>`));
  expect(result.papers).toHaveLength(4);
  expect(result.renderedRecordCountHint).toBe(4);
});

test("a reference/citation-list page reports its <li> item count as the hint", () => {
  const items = Array.from(
    { length: 6 },
    (_, i) => `<li>Author ${i}. Title ${i}. <a href="https://doi.org/10.1000/ref.${i}">CrossRef</a></li>`,
  ).join("\n");
  const result = scanDocument(doc(`<ul class="reference-list">${items}</ul>`));
  expect(result.renderedRecordCountHint).toBe(6);
});

test("repeated sibling result cards report the sibling count as the hint", () => {
  const cards = Array.from(
    { length: 3 },
    (_, i) => `
      <article class="card">
        <h3>Distinctive title ${i}</h3>
        <a href="https://doi.org/10.1234/card.${i}">Publisher</a>
      </article>`,
  ).join("\n");
  const result = scanDocument(doc(`<div class="result-page">${cards}</div>`));
  expect(result.papers).toHaveLength(3);
  expect(result.renderedRecordCountHint).toBe(3);
});

test("a single result card never crosses the 2-record floor into a hint", () => {
  const result = scanDocument(
    doc(`<div class="result-page"><article class="card"><h3>Only one</h3></article></div>`),
  );
  expect(result.renderedRecordCountHint).toBeNull();
});

test("a page with no recognized structural family reports no hint, never a guess", () => {
  const result = scanDocument(doc(`<p>See 10.1000/xyz123 for the full record.</p>`));
  expect(result.papers).toHaveLength(1);
  expect(result.renderedRecordCountHint).toBeNull();
});

test("a hidden reference-list item is not counted toward the hint", () => {
  const items = [
    `<li>Visible one <a href="https://doi.org/10.1000/hidden.0">CrossRef</a></li>`,
    `<li>Visible two <a href="https://doi.org/10.1000/hidden.1">CrossRef</a></li>`,
    `<li hidden>Hidden three <a href="https://doi.org/10.1000/hidden.2">CrossRef</a></li>`,
  ].join("\n");
  const result = scanDocument(doc(`<ul class="citation-list">${items}</ul>`));
  expect(result.renderedRecordCountHint).toBe(2);
});

// --- OpenAlex-shaped result cards: title-anchor detection + same-container
// kind preference (Change #2: a card carrying both a registered identifier
// and an openalex id folds into ONE row keyed on the registered one) -------

test("an OpenAlex-shaped result list detects every card via its W-id title link, folding a dual doi+openalex card into one row", () => {
  const cards = [
    {
      w: "w2963446712",
      title: "Attention is all you need",
      meta: "Vaswani et al. (2017). Advances in Neural Information Processing Systems. A foundational paper on the transformer architecture for sequence modeling.",
    },
    {
      w: "w2194775991",
      title: "Deep residual learning for image recognition",
      meta: "He et al. (2016). IEEE Conference on Computer Vision and Pattern Recognition. Introduces residual connections for training very deep networks.",
    },
    {
      w: "w2741809807",
      title: "A stray publisher PDF link alongside the OpenAlex title",
      meta: "Example et al. (2019). Journal of Testing. This card also carries a publisher PDF anchor pointing at a registered DOI.",
      doi: "10.1234/stray-link.99",
    },
  ];
  const html = cards
    .map(
      (c) => `
      <div class="entry">
        <a href="https://openalex.org/works/${c.w}">${c.title}</a>
        <span>${c.meta}</span>
        ${c.doi ? `<a href="https://publisher.example/doi/pdf/${c.doi}">PDF</a>` : ""}
      </div>`,
    )
    .join("\n");
  const result = scanDocument(doc(`<div class="results-list">${html}</div>`));

  // All 3 cards detected as exactly 3 rows — the dual-identifier card never
  // produces a duplicate second row.
  expect(result.papers).toHaveLength(3);
  const byTitle = (t: string): DetectedPaper | undefined => result.papers.find((p) => p.label.includes(t));

  const transformer = byTitle("Attention is all you need");
  expect(transformer?.identifier).toEqual({ kind: "openalex", value: "W2963446712" });
  expect(transformer?.label).toContain("Attention is all you need");

  const resnet = byTitle("Deep residual learning");
  expect(resnet?.identifier).toEqual({ kind: "openalex", value: "W2194775991" });

  // The dual-identifier card is keyed on the registered DOI, not openalex,
  // and its W-id occurrence folds into the same row (seen-2x semantics).
  const dual = byTitle("A stray publisher PDF link");
  expect(dual?.identifier).toEqual({ kind: "doi", value: "10.1234/stray-link.99" });
  expect(dual?.occurrences).toBe(2);
  expect(dual?.label).toContain("A stray publisher PDF link alongside the OpenAlex title");
});


test("a PDF document with an identifier-free tab URL yields exactly one grab row", () => {
  const page = doc(`<title>Open PDF</title>`, "https://pdf.example.org/assets/main.pdf?token=abc");
  Object.defineProperty(page, "contentType", { configurable: true, value: "application/pdf" });
  const result = scanDocument(page);
  expect(result.papers).toHaveLength(1);
  expect(result.papers[0]?.kind).toBe("pdf_grab");
  expect(result.papers[0]?.url).toBe("https://pdf.example.org/assets/main.pdf?token=abc");
});

test("a PDF tab URL with a DOI remains an ordinary identifier row", () => {
  const page = doc(`<title>Known PDF</title>`, "https://publisher.example/doi/10.1234/known.pdf");
  Object.defineProperty(page, "contentType", { configurable: true, value: "application/pdf" });
  const result = scanDocument(page);
  expect(result.papers).toHaveLength(1);
  expect(result.papers[0]?.kind).toBeUndefined();
  expect(result.papers[0]?.identifier).toEqual({ kind: "doi", value: "10.1234/known" });
});

test("a small embedded PDF preview inside an HTML page is not offered as a grab", () => {
  const page = doc(`<main><embed type="application/pdf" width="300" height="200"></main>`, "https://reader.example.org/article");
  const result = scanDocument(page);
  expect(result.papers.some((paper) => paper.kind === "pdf_grab")).toBe(false);
});

test("a 100% PDF embed inside a small HTML container is not offered as a grab", () => {
  const page = doc(`<main style="width:300px;height:200px"><embed type="application/pdf" width="100%" height="100%"></main>`, "https://reader.example.org/article");
  const result = scanDocument(page);
  expect(result.papers.some((paper) => paper.kind === "pdf_grab")).toBe(false);
});