// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { afterEach, expect, test } from "bun:test";
import { Window } from "happy-dom";
import { agentDOM, type AgentDOMRequest, type AgentDOMResult } from "../src/agent-dom";

const entryURL = "https://publisher.example/viewer?article=42&ticket=WRAPPERSECRET";
const sourceURL = "https://publisher.example/article/42?ticket=SOURCESECRET";
const fileURL = "https://publisher.example/files/42.pdf?tp=&article=42&ref=FILESECRET#page=1";
const doi = "10.1234/article.42";
const wrapper = `<iframe src="${fileURL}"></iframe>`;
const base = { entryURL, sourceURL, doi, allowNavigation: true };
const saved = new Map<string, PropertyDescriptor | undefined>();
afterEach(() => {
  for (const [key, descriptor] of saved) {
    if (descriptor) Object.defineProperty(globalThis, key, descriptor); else Reflect.deleteProperty(globalThis, key);
  }
  saved.clear();
});
function expose(key: string, value: unknown) {
  if (!saved.has(key)) saved.set(key, Object.getOwnPropertyDescriptor(globalThis, key));
  Object.defineProperty(globalThis, key, { value, writable: true, configurable: true });
}
function setup(html = wrapper) {
  const win = new Window({ url: entryURL, settings: { enableJavaScriptEvaluation: false, disableCSSFileLoading: true, disableJavaScriptFileLoading: true, disableIframePageLoading: true } });
  win.document.write(html);
  for (const [key, value] of Object.entries({ document: win.document, location: win.location, getComputedStyle: win.getComputedStyle.bind(win), HTMLElement: win.HTMLElement, papioArticleAgent: undefined })) expose(key, value);
  Object.assign(win.HTMLElement.prototype, { getClientRects: () => [{ width: 10, height: 10 }] });
  const clicks: { href: string; download: string | null; target: string; connected: boolean }[] = [];
  win.document.addEventListener("click", event => {
    event.preventDefault();
    const anchor = event.target as InstanceType<typeof win.HTMLAnchorElement>;
    clicks.push({ href: anchor.href, download: anchor.getAttribute("download"), target: anchor.target, connected: anchor.isConnected });
  });
  return { win, clicks };
}
const observe = (extra: Partial<AgentDOMRequest> = {}) => agentDOM({ ...base, method: "observe_pdf", ...extra });
function observed(result: AgentDOMResult) {
  expect(result.status).toBe("pdf_observed");
  if (result.status !== "pdf_observed") throw new Error(result.status);
  return result;
}
const act = (result: ReturnType<typeof observed>, extra: Partial<AgentDOMRequest> = {}) => agentDOM({
  ...base, method: "act_pdf", document: result.document, revision: result.revision, actionDeadline: Date.now() + 10_000, ...extra,
});

test("wrapper identity permits only the bounded PDF path, never model observations or control clicks", async () => {
  const { win, clicks } = setup(`${wrapper}<main><button>Download PDF</button></main>`);
  let controlClicks = 0;
  win.document.querySelector("button")!.addEventListener("click", () => controlClicks++);
  const first = observed(await observe());
  expect(first).toEqual({ status: "pdf_observed", document: expect.any(String), revision: expect.stringMatching(/^[a-f0-9]{64}$/) });
  for (const method of ["observe", "prepare", "act", "check_menu"] as const) {
    expect(await agentDOM({ ...base, method, document: first.document, revision: first.revision, choice: "c1" })).toEqual({ status: "blocked", reason: "identity_missing" });
  }
  expect(clicks).toHaveLength(0);
  expect(controlClicks).toBe(0);
  expect(await act(first)).toEqual({ status: "dispatched", downloadExpected: true });
  expect(clicks).toEqual([{ href: fileURL, download: "", target: "_self", connected: true }]);
  expect(win.document.querySelectorAll("a")).toHaveLength(0);
  expect(win.document.querySelector("iframe")!.getAttribute("src")).toBe(fileURL);
  expect(controlClicks).toBe(0);
  expect((await act(first)).status).toBe("stale");
  expect(clicks).toHaveLength(1);
});

test("serialized injection is self-contained, keeps URLs private and never reads child DOM or fetches bytes", async () => {
  const { win, clicks } = setup();
  const frame = win.document.querySelector("iframe")!;
  for (const property of ["contentDocument", "contentWindow"]) Object.defineProperty(frame, property, { get() { throw new Error("child DOM read"); } });
  expose("fetch", () => { throw new Error("PDF fetch"); });
  const injected = new Function(`return (${agentDOM.toString()});`)() as typeof agentDOM;
  const first = observed(await injected({ ...base, method: "observe_pdf" }));
  const result = await injected({ ...base, method: "act_pdf", document: first.document, revision: first.revision, actionDeadline: Date.now() + 10_000 });
  expect(result).toEqual({ status: "dispatched", downloadExpected: true });
  for (const secret of ["publisher.example", "WRAPPERSECRET", "SOURCESECRET", "FILESECRET", doi, "iframe", "controls", "observation"]) expect(JSON.stringify([first, result])).not.toContain(secret);
  expect(clicks).toHaveLength(1);
});

for (const [html, expected] of [
  [wrapper, fileURL],
  ['<embed src="/files/42.PDF?exact=1">', 'https://publisher.example/files/42.PDF?exact=1'],
  ['<object data="/opaque?id=42" type="application/pdf"></object>', 'https://publisher.example/opaque?id=42'],
  ['<iframe src="/opaque?id=42" type="application/pdf"></iframe>', 'https://publisher.example/opaque?id=42'],
  ['<embed src="/opaque?id=42" type="APPLICATION/PDF">', 'https://publisher.example/opaque?id=42'],
  [`<iframe hidden src="https://elsewhere.example/hidden.pdf"></iframe>${wrapper}`, fileURL],
] as const) test(`acquires the exposed PDF URL: ${html}`, async () => {
  const { clicks } = setup(html);
  expect(await act(observed(await observe()))).toEqual({ status: "dispatched", downloadExpected: true });
  expect(clicks[0]?.href).toBe(expected);
});

for (const source of [undefined, "", "not a URL", "/article/42", "http://publisher.example/article/42", "https://elsewhere.example/article/42", "https://user:password@publisher.example/article/42", entryURL, "javascript:alert(1)", "data:text/html,article", "blob:https://publisher.example/id", "https:publisher.example/article/42", "https://publisher.example/bad%zz", "https://publisher.example/has space"]) {
  test(`refuses invalid inherited source ${source}`, async () => {
    const { clicks } = setup();
    const request = { ...base, method: "observe_pdf" as const };
    if (source === undefined) Reflect.deleteProperty(request, "sourceURL"); else request.sourceURL = source;
    expect(await agentDOM(request)).toEqual({ status: "blocked", reason: "page_binding_failed" });
    expect(clicks).toHaveLength(0);
  });
}
test("wrapper capability requires allowNavigation true and the exact current URL", async () => {
  const { clicks } = setup();
  expect(await observe({ allowNavigation: false })).toEqual({ status: "blocked", reason: "page_binding_failed" });
  const { allowNavigation: _, ...request } = base;
  expect(await agentDOM({ ...request, method: "observe_pdf" })).toEqual({ status: "blocked", reason: "page_binding_failed" });
  expect(await observe({ entryURL: entryURL + "&different=1" })).toEqual({ status: "blocked", reason: "page_binding_failed" });
  expect(clicks).toHaveLength(0);
});

for (const html of [
  "", '<a href="/paper.pdf">PDF</a>', '<meta name="citation_pdf_url" content="https://publisher.example/paper.pdf">',
  '<iframe src="/viewer?file=paper.pdf"></iframe>', '<embed src="/paper.pdf/extra">',
  '<object data="/opaque" type="text/html"></object>', '<iframe></iframe>',
  '<iframe src="https://other.example/paper.pdf"></iframe>', '<iframe src="http://publisher.example/paper.pdf"></iframe>',
  '<iframe src="https://user:pass@publisher.example/paper.pdf"></iframe>',
  '<iframe src="blob:https://publisher.example/paper.pdf"></iframe>', '<embed src="data:application/pdf,test" type="application/pdf">',
  '<object data="javascript:alert(1)" type="application/pdf"></object>', '<iframe src="https://[broken/paper.pdf"></iframe>',
  '<iframe src="/bad%zz.pdf"></iframe>', '<iframe src="/paper.pdf" srcdoc="<p>Sign in</p>"></iframe>',
  '<iframe src="/paper.pdf" hidden></iframe>', '<div aria-hidden="true"><iframe src="/paper.pdf"></iframe></div>',
  '<div style="display:none"><embed src="/paper.pdf"></div>', '<iframe style="visibility:hidden" src="/paper.pdf"></iframe>',
  '<iframe style="opacity:0" src="/paper.pdf"></iframe>', '<div style="clip-path:inset(50%)"><object data="/paper.pdf"></object></div>',
  `${wrapper}${wrapper}`, `${wrapper}<embed src="/other.pdf">`, `${wrapper}<object data="/opaque"></object>`,
  `${wrapper}<iframe src="https://other.example/frame"></iframe>`, `${wrapper}<video src="/video.mp4"></video>`,
  `${wrapper}<audio controls style="display:block" src="/audio.mp3"></audio>`,
]) test(`refuses unsupported, hidden or ambiguous media: ${html}`, async () => {
  const { clicks } = setup(html);
  expect((await observe()).status).toBe("blocked");
  expect(clicks).toHaveLength(0);
});

test("zero-layout PDF media cannot be acquired", async () => {
  const { win } = setup();
  Object.assign(win.document.querySelector("iframe")!, { getClientRects: () => [{ width: 0, height: 10 }] });
  expect((await observe()).status).toBe("blocked");
});

for (const badDOI of ["", "not-a-doi", "10.1/short", "10.1234/contains space", "10.1234/<tag>"]) test(`inherited identity still rejects malformed request DOI: ${badDOI}`, async () => {
  setup();
  expect(await observe({ doi: badDOI })).toEqual({ status: "blocked", reason: "identity_invalid" });
});
for (const metadata of [
  '<meta name="citation_doi" content="10.9999/wrong">', '<meta name="citation_doi" content="">',
  '<meta name="prism.doi" content="malformed">', '<meta name="dc.identifier" content="doi:malformed">',
  `<meta name="citation_doi" content="${doi}"><meta name="prism.doi" content="10.9999/wrong">`,
  '<main><p><strong>DOI:</strong><a href="https://doi.org/10.9999/wrong">10.9999/wrong</a></p></main>',
]) test(`inherited identity cannot override an existing DOI claim: ${metadata}`, async () => {
  setup(metadata + wrapper);
  expect(await observe()).toEqual({ status: "blocked", reason: "identity_conflicting" });
});

for (const [gate, reason] of [
  ['<input type="password">', "credentials_required"],
  ['<input autocomplete="one-time-code">', "credentials_required"],
  ['<input autocomplete="cc-number">', "payment_required"],
  ['<div data-sitekey="private"></div>', "challenge_required"],
  ['<iframe src="https://challenge.example/captcha"></iframe>', "challenge_required"],
  ['<dialog open>Accept terms</dialog>', "consent_required"],
  ['<div role="dialog">Sign in to continue</div>', "credentials_required"],
  ['<button>Accept terms and conditions</button>', "consent_required"],
  ['<div role="button" aria-label="Accept license"></div>', "consent_required"],
  ['<form><button>Sign in</button></form>', "credentials_required"],
  ['<button>Complete verification</button>', "challenge_required"],
] as const) test(`wrapper human gate remains closed at observe and act: ${reason}`, async () => {
  const { win, clicks } = setup();
  const first = observed(await observe());
  win.document.body.insertAdjacentHTML("beforeend", gate);
  expect(await act(first)).toEqual({ status: "blocked", reason });
  expect(await observe()).toEqual({ status: "blocked", reason });
  expect(clicks).toHaveLength(0);
});

for (const change of ["source", "doi", "document", "request-document", "revision", "url", "entry-and-url", "file", "query", "replace", "hidden", "second", "claim"] as const)
  test(`act_pdf refuses changed ${change}`, async () => {
    const { win, clicks } = setup();
    const first = observed(await observe());
    const extra: Partial<AgentDOMRequest> = {};
    const frame = win.document.querySelector("iframe")!;
    if (change === "source") extra.sourceURL = sourceURL + "&other=1";
    if (change === "doi") extra.doi = "10.9999/other";
    if (change === "document") setup();
    if (change === "request-document") extra.document = "another-document";
    if (change === "revision") extra.revision = "0".repeat(64);
    if (change === "url" || change === "entry-and-url") win.location.hash = "different";
    if (change === "entry-and-url") extra.entryURL = win.location.href;
    if (change === "file") frame.src = "/other.pdf";
    if (change === "query") frame.src += "changed";
    if (change === "replace") frame.replaceWith(frame.cloneNode(true));
    if (change === "hidden") frame.hidden = true;
    if (change === "second") win.document.body.insertAdjacentHTML("beforeend", wrapper);
    if (change === "claim") win.document.head.insertAdjacentHTML("beforeend", '<meta name="citation_doi" content="10.9999/other">');
    expect((await act(first, extra)).status).not.toBe("dispatched");
    expect(clicks).toHaveLength(0);
    expect(win.document.querySelectorAll("a")).toHaveLength(0);
  });

test("act_pdf cannot run without its observed document and revision", async () => {
  const { clicks } = setup();
  expect((await agentDOM({ ...base, method: "act_pdf" })).status).not.toBe("dispatched");
  const first = observed(await observe());
  for (const key of ["document", "revision", "actionDeadline"]) {
    const request = { ...base, method: "act_pdf" as const, document: first.document, revision: first.revision, actionDeadline: Date.now() + 10_000 };
    Reflect.deleteProperty(request, key);
    expect((await agentDOM(request)).status).not.toBe("dispatched");
  }
  expect(clicks).toHaveLength(0);
});

for (const deadline of [0, -1, 1.5, Infinity, NaN]) test(`invalid or expired PDF action deadline cannot click: ${deadline}`, async () => {
  const { clicks } = setup();
  const first = observed(await observe());
  expect((await act(first, { actionDeadline: deadline })).status).not.toBe("dispatched");
  expect(clicks).toHaveLength(0);
  expect(await act(first)).toEqual({ status: "dispatched", downloadExpected: true });
});

test("a deadline that expires during final DOM inspection cannot dispatch", async () => {
  const { win, clicks } = setup(`<embed src="${fileURL}">`);
  const first = observed(await observe());
  const deadline = Date.now() + 60_000;
  const originalNow = Date.now;
  Object.assign(win.document.querySelector("embed")!, { getClientRects: () => {
    Date.now = () => deadline;
    return [{ width: 10, height: 10 }];
  } });
  try {
    expect(await act(first, { actionDeadline: deadline })).toEqual({ status: "stale", reason: "observation_changed" });
    expect(clicks).toHaveLength(0);
  } finally { Date.now = originalNow; }
});

test("wrapper shares the ordinary document token and an observed URL change changes its revision", async () => {
  const { win } = setup(`<meta name="citation_doi" content="${doi}"><meta name="dc.identifier" content="local-id">${wrapper}`);
  const ordinary = await agentDOM({ ...base, method: "observe" });
  expect(ordinary.status).toBe("observed");
  if (ordinary.status !== "observed") throw new Error(ordinary.status);
  const first = observed(await observe());
  expect(first.document).toBe(ordinary.document);
  expect(observed(await observe()).revision).toBe(first.revision);
  const otherSource = observed(await observe({ sourceURL: sourceURL + "&different=1" }));
  expect(otherSource.revision).not.toBe(first.revision);
  win.document.querySelector("iframe")!.src = "/other.pdf";
  expect(observed(await observe()).revision).not.toBe(first.revision);
});

for (const change of ["file", "gate", "document", "state", "deadline"] as const) test(`observation revalidates ${change} after its asynchronous digest`, async () => {
  const { win, clicks } = setup();
  const original = crypto.subtle.digest;
  const deadline = Date.now() + 60_000;
  const originalNow = Date.now;
  try {
    crypto.subtle.digest = async (...args) => {
      if (change === "file") win.document.querySelector("iframe")!.src = "/other.pdf";
      if (change === "gate") win.document.body.insertAdjacentHTML("beforeend", '<input type="password">');
      if (change === "document") setup();
      if (change === "state") expose("papioArticleAgent", undefined);
      if (change === "deadline") Date.now = () => deadline;
      return original.apply(crypto.subtle, args);
    };
    expect((await observe({ actionDeadline: deadline })).status).toBe("stale");
    expect(clicks).toHaveLength(0);
  } finally { crypto.subtle.digest = original; Date.now = originalNow; }
});

test("temporary anchor is removed even when native dispatch throws and cannot be replayed", async () => {
  const { win } = setup('<base target="_blank">' + wrapper);
  const first = observed(await observe());
  let clicks = 0;
  const original = win.HTMLElement.prototype.click;
  win.HTMLElement.prototype.click = function () { clicks++; throw new Error("dispatch failed"); };
  try { await expect(act(first)).rejects.toThrow("dispatch failed"); }
  finally { win.HTMLElement.prototype.click = original; }
  expect(win.document.querySelectorAll("a")).toHaveLength(0);
  expect((await act(first)).status).toBe("stale");
  expect(clicks).toBe(1);
});
