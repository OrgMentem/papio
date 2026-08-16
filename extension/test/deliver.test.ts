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
  expect(doiFromURL("https://example.com/news/story-42")).toBeUndefined();
  // A view marker fused onto the DOI cannot be trimmed back: the real bioRxiv DOI
  // also drops the `v1`, and version-collapsing would name a different work.
  expect(doiFromURL("https://www.biorxiv.org/content/10.1101/2024.06.04.594010v1.full.pdf")).toBeUndefined();
  // Springer publishes supplementary files at `…/esm/art:<doi>/MediaObjects/…`.
  expect(
    doiFromURL("https://media.springernature.com/original/springer-static/esm/art%3A10.1038%2Fs41592-022-01415-4/MediaObjects/41592_2022_1415_MOESM1_ESM.pdf"),
  ).toBeUndefined();
});

// A trailing route word names a VIEW of the article, so the article's DOI is
// exactly what the URL means and it is stripped rather than rejected. This is
// only safe because a non-article route is rejected first: trimming a tail is
// dangerous precisely when the URL names a different document.
test("doiFromURL strips a trailing article-view route", () => {
  expect(doiFromURL("https://www.frontiersin.org/journals/medicine/articles/10.3389/fmed.2026.1830485/full")).toBe("10.3389/fmed.2026.1830485");
  expect(doiFromURL("https://www.emerald.com/insight/content/doi/10.1108/QEA-07-2024-0055/full/pdf")).toBe("10.1108/QEA-07-2024-0055");
  expect(doiFromURL("https://journals.sagepub.com/doi/pdf/10.1177/01634437251234567/full")).toBe("10.1177/01634437251234567");
  // Taylor & Francis's References tab carries the article's own DOI.
  expect(doiFromURL("https://www.tandfonline.com/doi/ref/10.1080/0144929X.2019.1578828")).toBe("10.1080/0144929X.2019.1578828");
  // A component DOI's own multi-segment suffix is not a route and survives.
  expect(doiFromURL("https://doi.org/10.1109/tem.2022.3197196/mm1")).toBe("10.1109/tem.2022.3197196/mm1");
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

// A route suffix is a property of the ROUTE, not of the candidate. On doi.org
// the whole path is the identifier by definition, so there is no route and no
// external suffix to remove: `10.1234/article.pdf` is a DOI that ends in `.pdf`.
// Stripping it unconditionally named a different work.
test("doiFromURL only strips an external suffix where the route declares one", () => {
  expect(doiFromURL("https://doi.org/10.1234/article.pdf")).toBe("10.1234/article.pdf");
  expect(doiFromURL("https://dl.acm.org/doi/pdf/10.1145/3630106.3660000.PDF")).toBe("10.1145/3630106.3660000");
  // A declared `doi` parameter is a value, not a file path.
  expect(doiFromURL("https://cdn.example/f?doi=10.1234/article.pdf")).toBe("10.1234/article.pdf");
});

// Three independent structures can name a work — a wrapped inner URL, a declared
// `doi` parameter, and the path. Whichever were consulted first would silently
// override the others, and that is a cardinal path: a supplement URL carrying
// `?doi=<article>` returned the ARTICLE while addressing the appendix.
test("doiFromURL declines when a URL names two different works", () => {
  expect(doiFromURL("https://dl.acm.org/doi/suppl/10.1145/X1/suppl_file/a.pdf?doi=10.1145/X1")).toBeUndefined();
  expect(doiFromURL("https://pub.example/article/10.1000/real?target=https%3A%2F%2Fo.example%2Fdoi%2Fpdf%2F10.2000%2Fother")).toBeUndefined();
  expect(doiFromURL("https://doi.org/10.1234/A?doi=10.5678/B")).toBeUndefined();
  expect(doiFromURL("https://pub.example/article/10.1000/real?doi=10.2000/other")).toBeUndefined();
  expect(doiFromURL("https://cdn.example/f.pdf?doi=10.1234/first&doi=10.1234/second")).toBeUndefined();
  // Agreement is not ambiguity.
  expect(doiFromURL("https://pub.example/article/10.1000/real?doi=10.1000/real")).toBe("10.1000/real");
});

test("doiFromURL declines a route word standing in for the whole DOI suffix", () => {
  // `/doi/pdf/10.1177/full` is a truncated route, not `10.1177/full`.
  expect(doiFromURL("https://sage.example/doi/pdf/10.1177/full")).toBeUndefined();
  // JS `$` matches before a FINAL line terminator, so an encoded `%0A` suffix
  // would pass a `…$`-anchored pattern and be stored as the identifier.
  expect(doiFromURL("https://publisher.example/doi/10.1234/paper%0A")).toBeUndefined();
});

test("doiFromURL rejects URL delimiters inside the identifier itself", () => {
  // The old text scan decoded first and accepted `10.1234/paper&token=SECRET`,
  // which `work.NormalizeDOI` also accepts, so the secret became the identity.
  expect(doiFromURL("https://doi.org/10.1234/paper%26token%3DSECRET")).toBeUndefined();
});

test("doiFromURL accepts article prefix routes and multi-segment DOI suffixes", () => {
  // `abs`/`full` before the DOI name the article itself; only after it do they
  // name a view of it. A genuine DOI suffix may also have several segments.
  expect(doiFromURL("https://journals.sagepub.com/doi/abs/10.1177/01634437251234567")).toBe("10.1177/01634437251234567");
  expect(doiFromURL("https://journals.sagepub.com/doi/full/10.1177/01634437251234567")).toBe("10.1177/01634437251234567");
  expect(doiFromURL("https://doi.org/10.1234/alpha/beta/gamma")).toBe("10.1234/alpha/beta/gamma");
});

test("doiFromURL resolves a relative candidate and bounds proxy unwrapping to one hop", () => {
  expect(doiFromURL("/doi/pdf/10.1177/abc.pdf", "https://journals.sagepub.com/x")).toBe("10.1177/abc");
  const inner = encodeURIComponent("https://wiley.com/doi/pdfdirect/10.1111/x?ticket=ST-42");
  expect(doiFromURL(`https://a.proxy/login?url=${inner}`)).toBe("10.1111/x");
  expect(doiFromURL(`https://a.proxy/login?url=${encodeURIComponent(`https://b.proxy/login?url=${inner}`)}`)).toBeUndefined();
});

test("pageAcquireOrigin drops userinfo credentials", () => {
  expect(pageAcquireOrigin("https://user:pass@papers.example/p.pdf?ticket=S")).toBe("https://papers.example");
});

// The probe's tiers are URL-origin, so a relative value in any of them must
// resolve against the page rather than be dropped.
test("extractPageDOI resolves relative canonical and citation_pdf_url values", () => {
  expect(extractPageDOI({ meta: [], canonical: "/article/10.1234/x", href: "https://pub.example/p" })).toBe("10.1234/x");
  expect(extractMetaDOI([{ name: "citation_pdf_url", content: "/doi/pdf/10.1177/abc.pdf" }], "https://sage.example/p")).toBe("10.1177/abc");
  // A bibliographic tag stating a URL takes the URL rules, so its delimiters
  // cannot be absorbed either.
  expect(extractMetaDOI([{ name: "dc.identifier", content: "https://doi.org/10.1234/paper%26t%3DS" }])).toBeUndefined();
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
