// Copyright 2026 OrgMentem. Licensed under MIT.

import { expect, test } from "bun:test";

import {
  classifyPage,
  deriveStablePageDOI,
  doiFromURL,
  extractMetaDOI,
  extractPageDOI,
  isPDFPage,
  isPDFURL,
  pageAcquireOrigin,
  pdfSourceURL,
  sniffDOI,
} from "../src/deliver";

// A DOI read out of a URL must come from URL *structure*. Scanning the
// serialized URL as text was a live defect with two distinct consequences, both
// pinned below: a route suffix glued onto the DOI (ACM, Springer), and a query
// token absorbed into it (`?doi=…&token=…`), which `work.NormalizeDOI` accepts
// because `doiCoreRE`'s `\S` matches `&` and `=`.
test("doiFromURL reads real provider PDF URLs without absorbing route suffixes", () => {
  expect(doiFromURL("https://dl.acm.org/doi/pdf/10.1145/3630106.3660000.pdf")).toBe("10.1145/3630106.3660000");
  expect(doiFromURL("https://link.springer.com/content/pdf/10.1007/s11192-024-04901-y.pdf")).toBe("10.1007/s11192-024-04901-y");
  expect(doiFromURL("https://journals.sagepub.com/doi/pdf/10.1177/01634437251234567?download=true")).toBe("10.1177/01634437251234567");
  expect(doiFromURL("https://onlinelibrary.wiley.com/doi/pdfdirect/10.1111/jcpp.13440?download=true")).toBe("10.1111/jcpp.13440");
  expect(doiFromURL("https://doi.org/10.1002/prefer")).toBe("10.1002/prefer");
});

// The cardinal failure this project refuses: a wrong document filed under a
// right citation. ACM publishes supplements in the ARTICLE's DOI namespace, so
// "strip the trailing junk" would yield the article's real DOI and file an
// appendix as the paper. Declining is the only safe answer, which is why the
// bug's original form (a bogus identifier that resolves to nothing) was safer
// than its naive repair.
test("doiFromURL declines a URL that names a document other than its DOI", () => {
  expect(doiFromURL("https://dl.acm.org/doi/suppl/10.1145/3630106.3660000/suppl_file/appendix.pdf")).toBeUndefined();
  expect(doiFromURL("https://dl.acm.org/doi/suppl/10.1145/3630106.3660000/unrecognized/x.pdf")).toBeUndefined();
  expect(doiFromURL("https://onlinelibrary.wiley.com/doi/citedby/10.1111/jcpp.13440")).toBeUndefined();
  expect(doiFromURL("https://journals.sagepub.com/doi/pdf/10.1177/01634437251234567/full")).toBeUndefined();
  expect(doiFromURL("https://example.com/news/story-42")).toBeUndefined();
});

test("doiFromURL keeps a bounded query DOI and never absorbs a neighbouring token", () => {
  expect(doiFromURL("https://cdn.example/file.pdf?doi=10.1234/paper&token=SECRET123")).toBe("10.1234/paper");
  expect(doiFromURL("https://prov.example/doi/pdf/10.1111/abc?ticket=ST-9f8e7d")).toBe("10.1111/abc");
  // A library proxy wraps the real URL in a parameter; the inner URL is parsed
  // by these same rules rather than scanned, so its ticket cannot ride along.
  expect(
    doiFromURL("https://proxy.lib.edu/login?url=https%3A%2F%2Fwiley.com%2Fdoi%2Fpdfdirect%2F10.1111%2Fx%3Fticket%3DST-42"),
  ).toBe("10.1111/x");
});

test("doiFromURL unwraps a browser viewer tab and preserves DOI slash runs", () => {
  // The commonest Send PDF shape on Chrome: the DOI is inside an encoded `file`
  // parameter, so narrowing to `url.pathname` alone would lose it.
  expect(
    doiFromURL("chrome-extension://mhjfbmdgcfjbbpaeojofohoefgiehjai/index.html?file=https%3A%2F%2Fdl.acm.org%2Fdoi%2Fpdf%2F10.1145%2F3630106.3660000.pdf"),
  ).toBe("10.1145/3630106.3660000");
  // 10.48612//x and 10.48612/x are two separately registered DataCite works, so
  // a repeated slash is data, not a typo to normalize away (AGENTS.md).
  expect(doiFromURL("https://doi.org/10.48612//monograph-2025-2")).toBe("10.48612//monograph-2025-2");
});

test("pageAcquireOrigin drops everything a provider URL can carry a secret in", () => {
  expect(pageAcquireOrigin("https://cdn.example/file.pdf?doi=10.1234/p&token=SECRET123#frag")).toBe("https://cdn.example");
  expect(pageAcquireOrigin("https://prov.example:8443/doi/pdf/10.1111/abc?ticket=ST-9f8e7d")).toBe("https://prov.example:8443");
  expect(
    pageAcquireOrigin("chrome-extension://mhjfbmdgcfjbbpaeojofohoefgiehjai/index.html?file=https%3A%2F%2Fpapers.example%2Fp.pdf%3Ft%3DS"),
  ).toBe("https://papers.example");
  // No safe value exists: the caller must refuse rather than send the original.
  expect(pageAcquireOrigin("about:blank")).toBeUndefined();
  expect(pageAcquireOrigin("not a url")).toBeUndefined();
});

test("sniffDOI returns the first DOI-shaped match and trims sentence punctuation", () => {
  expect(sniffDOI("See 10.1000/first.example. Then 10.1000/second")).toBe("10.1000/first.example");
  expect(sniffDOI("No identifier here")).toBeUndefined();
});

test("classifyPage recognizes PDF paths, viewers, DOI URLs, and text fallback", () => {
  expect(classifyPage("https://papers.example/paper.PDF?download=1").kind).toBe("pdf");
  expect(isPDFURL("https://papers.example/paper.pdf?doi=10.1000/example")).toBe(true);
  expect(isPDFPage("chrome-extension://mhjfbmdgcfjbbpaeojofohoefgiehjai/index.html")).toBe(true);
  expect(classifyPage("https://journals.example/article", { text: "citation 10.1000/from-text" })).toEqual({
    kind: "doi",
    doi: "10.1000/from-text",
  });
  expect(classifyPage("https://example.com/news")).toEqual({ kind: "none" });
});

test("Cell's exact PII route is a direct PDF surface without a .pdf suffix", () => {
  const url = "https://www.cell.com/action/showPdf?pii=S240584401730308X";
  expect(isPDFURL(url)).toBe(true);
  expect(isPDFPage(url)).toBe(true);
  expect(classifyPage(url)).toEqual({ kind: "pdf" });
  expect(isPDFURL("http://www.cell.com/action/showPdf?pii=S240584401730308X")).toBe(false);
  expect(isPDFURL("https://heliyon.cell.com/action/showPdf?pii=S240584401730308X")).toBe(false);
  expect(isPDFURL("https://www.cell.com/action/showPdf?pii=S240584401730308X&extra=1")).toBe(false);
  expect(isPDFURL("https://www.cell.com/action/showPdf?pii=S240584401730308X#frag")).toBe(false);
  expect(isPDFURL("https://www.cell.com/action/showPdf")).toBe(false);
  expect(isPDFURL("https://cell.com/action/showPdf?pii=S240584401730308X")).toBe(false);
  expect(isPDFURL("https://attacker.example/action/showPdf?pii=S240584401730308X")).toBe(false);
});

test("extractMetaDOI honors metadata priority and accepts DOI-bearing PDF URLs", () => {
  expect(
    extractMetaDOI([
      { name: "dc.identifier", content: "doi:10.1000/dublin-core" },
      { name: "citation_doi", content: "10.1000/citation" },
      { name: "prism.doi", content: "10.1000/prism" },
    ]),
  ).toBe("10.1000/citation");
  expect(
    extractMetaDOI([
      { name: "citation_pdf_url", content: "https://doi.org/10.1000/from-pdf" },
    ]),
  ).toBe("10.1000/from-pdf");
});

test("extractPageDOI follows canonical, DOI links, and bounded body text layers", () => {
  expect(
    extractPageDOI({
      canonical: "https://publisher.example/article/10.1000/canonical",
      ogURL: "https://publisher.example/article/10.1000/og",
      href: "https://publisher.example/article/10.1000/page",
    }),
  ).toBe("10.1000/canonical");
  expect(
    extractPageDOI({
      href: "https://publisher.example/article",
      anchorHrefs: ["https://example.org/not-a-doi", "https://doi.org/10.1000/linked"],
    }),
  ).toBe("10.1000/linked");
  expect(
    extractPageDOI({
      href: "https://publisher.example/article",
      bodyText: `${"x".repeat(200_000)} 10.1000/too-late`,
    }),
  ).toBeUndefined();
  expect(
    extractPageDOI({
      href: "https://publisher.example/article",
      bodyText: "The article DOI is 10.1000/in-visible-page-text.",
    }),
  ).toBe("10.1000/in-visible-page-text");
});

test("JSTOR stable pages derive their documented 10.2307 DOI", () => {
  const url = "https://www.jstor.org/stable/20183234";
  expect(deriveStablePageDOI(url)).toBe("10.2307/20183234");
  expect(extractPageDOI({ href: url })).toBe("10.2307/20183234");
  expect(classifyPage(url)).toEqual({ kind: "doi", doi: "10.2307/20183234" });
});

test("pdfSourceURL unwraps browser viewer file parameters", () => {
  expect(pdfSourceURL("chrome-extension://mhjfbmdgcfjbbpaeojofohoefgiehjai/index.html?file=https%3A%2F%2Fpapers.example%2Fpaper.pdf")).toBe(
    "https://papers.example/paper.pdf",
  );
});
