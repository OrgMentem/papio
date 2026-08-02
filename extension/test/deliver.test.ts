// Copyright 2026 OrgMentem. Licensed under MIT.

import { expect, test } from "bun:test";

import { classifyPage, isPDFPage, isPDFURL, pdfSourceURL, sniffDOI } from "../src/deliver";

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

test("pdfSourceURL unwraps browser viewer file parameters", () => {
  expect(pdfSourceURL("chrome-extension://mhjfbmdgcfjbbpaeojofohoefgiehjai/index.html?file=https%3A%2F%2Fpapers.example%2Fpaper.pdf")).toBe(
    "https://papers.example/paper.pdf",
  );
});
