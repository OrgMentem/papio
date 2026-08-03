// Copyright 2026 OrgMentem. Licensed under MIT.

import { expect, test } from "bun:test";

import {
  classifyPage,
  deriveStablePageDOI,
  extractMetaDOI,
  extractPageDOI,
  isPDFPage,
  isPDFURL,
  pdfSourceURL,
  sniffDOI,
} from "../src/deliver";

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
