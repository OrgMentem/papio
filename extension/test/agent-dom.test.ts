// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { afterEach, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { Window } from "happy-dom";
import { agentDOM, type AgentDOMRequest, type AgentDOMResult } from "../src/agent-dom";

const entryURL = "https://ebooks.iospress.nl/doi/10.3233/SHTI000001";
const doi = "10.3233/SHTI000001";
const metadataOnlyURL = "https://ebooks.iospress.nl/articles/63646";
const fixture = readFileSync(new URL("../fixtures/iospress/success.html", import.meta.url), "utf8");
const saved = new Map<string, PropertyDescriptor | undefined>();
afterEach(() => {
  for (const [key, descriptor] of saved) {
    if (descriptor) Object.defineProperty(globalThis, key, descriptor); else Reflect.deleteProperty(globalThis, key);
  }
  saved.clear();
});
function setup(html = fixture, url = entryURL) {
  const win = new Window({ url, settings: { enableJavaScriptEvaluation: false, disableCSSFileLoading: true, disableJavaScriptFileLoading: true, disableIframePageLoading: true } });
  win.document.write(html);
  for (const [key, value] of Object.entries({ document: win.document, location: win.location, getComputedStyle: win.getComputedStyle.bind(win), HTMLElement: win.HTMLElement, papioArticleAgent: undefined })) {
    if (!saved.has(key)) saved.set(key, Object.getOwnPropertyDescriptor(globalThis, key));
    Object.defineProperty(globalThis, key, { value, writable: true, configurable: true });
  }
  Object.assign(win.HTMLElement.prototype, { getClientRects: () => [{ width: 10, height: 10 }] });
  return win;
}
const observe = () => agentDOM({ method: "observe", entryURL, doi });
const observeMetadataOnly = () => agentDOM({ method: "observe", entryURL: metadataOnlyURL, doi });
function observed(result: AgentDOMResult) {
  if (result.status !== "observed") throw new Error(result.status);
  return result;
}
const act = (result: ReturnType<typeof observed>, extra: Partial<AgentDOMRequest> = {}) => agentDOM({ method: "act", entryURL, doi,
  document: result.document, revision: result.observation.revision,
  choice: result.observation.controls.find(c => c.label.startsWith("Download PDF"))?.id ?? "", ...extra });

test("navigation prepares the exact original anchor without a PDF label and retires source handles", async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><main><a href="/article/full?edition=2">Read this work</a></main>`);
  const anchor = win.document.querySelector("a")!;
  let clicks = 0;
  anchor.addEventListener("click", event => { event.preventDefault(); clicks++; });
  const first = observed(await agentDOM({ method: "observe", entryURL, doi, allowNavigation: true }));
  expect(first.observation.controls[0]?.disabled).toBe(false);
  expect(JSON.stringify(first.observation)).not.toContain("edition=2");
  const request = { entryURL, doi, allowNavigation: true, document: first.document, revision: first.observation.revision, choice: "c1" };
  expect(await agentDOM({ ...request, method: "act" })).toMatchObject({ status: "stale" });
  const prepared = await agentDOM({ ...request, method: "prepare" });
  expect(prepared).toEqual({ status: "prepared", effect: "navigate", destination: anchor.href });
  expect(clicks).toBe(0);
  expect(await agentDOM({ ...request, method: "act", destination: anchor.href })).toEqual({ status: "dispatched", downloadExpected: true });
  expect(clicks).toBe(1);
  expect(anchor.hasAttribute("download")).toBe(false);
  expect(await agentDOM({ method: "observe", entryURL, doi, allowNavigation: true })).toEqual({ status: "stale", reason: "document_changed" });
  expect(await agentDOM({ ...request, method: "act", destination: anchor.href })).toMatchObject({ status: "stale" });
  expect(clicks).toBe(1);
});

for (const mutation of ["href", "base", "identity", "document", "destination"] as const)
  test(`prepared navigation refuses ${mutation} drift without clicking`, async () => {
    const win = setup(`<meta name="citation_doi" content="${doi}"><main><a href="/article/full">Read article</a></main>`);
    const anchor = win.document.querySelector("a")!;
    let clicks = 0; anchor.addEventListener("click", event => { event.preventDefault(); clicks++; });
    const first = observed(await agentDOM({ method: "observe", entryURL, doi, allowNavigation: true }));
    const request = { entryURL, doi, allowNavigation: true, document: first.document, revision: first.observation.revision, choice: "c1" };
    expect((await agentDOM({ ...request, method: "prepare" })).status).toBe("prepared");
    const destination = anchor.href;
    if (mutation === "href") anchor.href = "/article/other";
    if (mutation === "base") win.document.head.insertAdjacentHTML("beforeend", '<base target="_blank">');
    if (mutation === "identity") win.document.querySelector("meta")!.setAttribute("content", "10.9999/wrong");
    if (mutation === "document") setup();
    expect((await agentDOM({ ...request, method: "act", destination: mutation === "destination" ? destination + "?different" : destination })).status).not.toBe("dispatched");
    expect(clicks).toBe(0);
  });

test("navigation capability preserves new-context, cross-origin and form refusals", async () => {
  setup(`<meta name="citation_doi" content="${doi}"><main><a href="/next" target="_blank">Read work</a><a href="https://other.example/next">Read work</a><a href="http://ebooks.iospress.nl/next">Read work</a><a href="https://user@ebooks.iospress.nl/next">Read work</a><form action="/next"><button>Read work</button></form></main>`);
  const first = observed(await agentDOM({ method: "observe", entryURL, doi, allowNavigation: true }));
  expect(first.observation.controls).toHaveLength(5);
  expect(first.observation.controls.every(control => control.disabled)).toBe(true);
});

test("production injection serializes independently and clicks a JS-backed article control", async () => {
  const win = setup();
  const injected = new Function(`return (${agentDOM.toString()});`)() as typeof agentDOM;
  let clicks = 0;
  win.document.querySelector(".getpdf")!.addEventListener("click", () => clicks++);
  const result = observed(await injected({ method: "observe", entryURL, doi }));
  expect(result.observation.doi).toBe(doi.toLowerCase());
  expect(result.observation.revision).toMatch(/^[a-f0-9]{64}$/);
  expect(result.observation.controls[0]).toEqual({ id: "c1", role: "button", label: "Download PDF", disabled: false });
  expect(await injected({ method: "act", entryURL, doi, document: result.document, revision: result.observation.revision, choice: "c1" })).toMatchObject({ status: "dispatched" });
  expect(clicks).toBe(1);
});

test("sanitized projection omits account/form/URL/body data while hidden execution changes alter revision", async () => {
  const win = setup();
  win.location.search = "?account=ACCOUNTSECRET";
  win.document.querySelector("input")!.value = "FORMSECRET";
  win.document.querySelector("main")!.insertAdjacentHTML("beforeend", '<p>BODYSECRET</p><div class="account-details"><button>PERSONSECRET</button></div><button aria-label="PDF https://private.test/file?token=URLSECRET user@example.test">PDF</button>');
  const before = observed(await observe());
  const json = JSON.stringify(before);
  for (const secret of ["ACCOUNTSECRET", "FORMSECRET", "BODYSECRET", "PERSONSECRET", "URLSECRET", "user@example.test", "ebooks.iospress.nl", "private.test", "downloadform00001", "getpdf", "/Download/Pdf"]) expect(json).not.toContain(secret);
  win.document.querySelector("input")!.value = "NEWSECRET";
  const after = observed(await observe());
  expect(after.observation.revision).not.toBe(before.observation.revision);
  expect(after.observation.controls).toEqual(before.observation.controls);
});

test("a nested download heading cannot name unrelated article controls or unnamed candidates", async () => {
  setup(`<meta name="citation_doi" content="${doi}"><main>
    <div><h3>Downloads</h3><a href="#"></a><button><span hidden>Hidden PDF</span></button>
      <button aria-label="https://private.test/SECRET user@example.test"></button></div>
    <button>Share</button><button>Cite</button><a href="#formats">Download</a>
    <section><h2>Figures</h2><button>Save PNG</button></section>
    <section aria-label="Local formats"><div><h3>Unowned heading</h3></div><button>Options</button></section>
  </main>`);
  const controls = observed(await observe()).observation.controls;
  expect(controls.map(c => c.label)).toEqual(["Download", "Options [Local formats]", "Save PNG [Figures]", "Share", "Cite"]);
  expect(controls.every(c => !c.disabled)).toBe(true);
  expect(JSON.stringify(controls)).not.toContain("SECRET");
});

test("only visible public direct headings supply implicit region context", async () => {
  setup(`<meta name="citation_doi" content="${doi}"><main><h1 hidden>Hidden heading</h1>
    <h2 data-private>PRIVATEHEADING</h2><div><h3>Nested download</h3></div><button>Share</button>
    <section><h2>Article formats</h2><button>PDF options</button></section>
  </main>`);
  expect(observed(await observe()).observation.controls.map(c => c.label)).toEqual(["PDF options [Article formats]", "Share"]);
});

for (const [metadata, title] of [
  ['<meta name="dc.title" content="DC article">', "DC article"],
  ['<meta name="PRISM.TITLE" content="Prism article">', "Prism article"],
  ['<meta name="prism.title" content="Prism article"><meta name=" DC.Title " content="DC article">', "DC article"],
  ['<meta name="dc.title" content="DC article"><meta name="citation_title" content="Citation article">', "Citation article"],
  ['<meta name="citation_title" content=" "><meta name="dc.title" content="https://private.test/SECRET"><meta name="prism.title" content="Public article user@example.test">', "Public article [redacted]"],
  ["", ""],
] as const) test(`title uses only the first public standard metadata value: ${metadata || "absent"}`, async () => {
  setup(`<title>PRIVATEBROWSERTITLE</title><meta name="citation_doi" content="${doi}">${metadata}<main><h1>PRIVATEBODYTITLE</h1><button>Formats</button></main>`);
  expect(observed(await observe()).observation.title).toBe(title);
});

test("standard title fallback retains the existing public text length cap", async () => {
  setup(`<meta name="citation_doi" content="${doi}"><meta name="dc.title" content="${"Public article ".repeat(40)}"><main><button>Formats</button></main>`);
  expect(observed(await observe()).observation.title).toHaveLength(400);
});

test("own-label ranking precedes the cap without promoting region context or authorizing viewer navigation", async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><main>
    <button>Download citation</button><button>Figures PDF</button><button>Supplementary PDF</button>
    <section aria-label="Download PDF">${Array.from({ length: 85 }, (_, i) => `<button>Share ${i}</button>`).join("")}</section>
    <button>Formats</button><a href="/download/opaque/viewer">Article PDF</a>
    <button>Download PDF</button>
  </main>`);
  const first = observed(await observe());
  expect(first.observation.controls).toHaveLength(80);
  expect(first.observation.controls.slice(0, 4).map(c => [c.label, c.disabled])).toEqual([
    ["Formats", false], ["Download PDF", false], ["Share 0 [Download PDF]", false], ["Share 1 [Download PDF]", false],
  ]);
  expect(first.observation.controls.some(c => /citation|Figures|Supplementary/.test(c.label))).toBe(false);
  expect(first.observation.controls.some(c => c.label === "Article PDF")).toBe(false);
  expect(first.observation.controls[0]!.id).not.toBe("c1"); // IDs belong to elements, not rank positions.
  let clicks = 0; win.document.querySelector("a")!.addEventListener("click", () => clicks++);
  expect(observed(await observe()).observation).toEqual(first.observation);
  win.document.querySelector("main")!.insertAdjacentHTML("afterbegin", '<button>PDF options</button>');
  const next = observed(await observe());
  expect(next.observation.controls[0]!.label).toBe("PDF options");
  expect(next.observation.controls.find(c => c.label === "Formats")!.id).toBe(first.observation.controls[0]!.id);
  // A control outside the transmitted cap still participates in freshness.
  win.document.querySelector("section button:last-child")!.setAttribute("onclick", "changed()");
  expect(await act(next)).toEqual({ status: "stale", reason: "observation_changed" });
  win.document.querySelector("section")!.remove();
  const uncapped = observed(await observe());
  const viewer = uncapped.observation.controls.find(c => c.label === "Article PDF")!;
  expect(viewer.disabled).toBe(true);
  expect(await act(uncapped, { choice: viewer.id })).toEqual({ status: "stale", reason: "observation_changed" });
  expect(clicks).toBe(0);
});

for (const [gate, reason] of [
  ['<input type="password">', "credentials_required"],
  ['<input autocomplete="one-time-code">', "credentials_required"],
  ['<input autocomplete="cc-number">', "payment_required"],
  ['<iframe title="CAPTCHA challenge"></iframe>', "challenge_required"],
  ['<div class="cf-turnstile" data-sitekey="secret"></div>', "challenge_required"],
  ['<dialog open>Accept terms and conditions<button>Continue</button></dialog>', "consent_required"],
  ['<div role="dialog">Purchase access<button>Continue</button></div>', "payment_required"],
  ['<div role="dialog">Document delivery<button>Continue</button></div>', "human_action_required"],
  ['<div role="dialog">Grant permissions<button>Continue</button></div>', "consent_required"],
  ['<div role="dialog">Sign in PRIVATEACCOUNT<button>Continue</button></div>', "credentials_required"],
  ['<div role="dialog">Verification PRIVATEACCOUNT<button>Continue</button></div>', "challenge_required"],
] as const) test(`human gate blocks with a fixed reason before observation and click: ${gate}`, async () => {
  const win = setup(), first = observed(await observe());
  let clicks = 0;
  win.document.querySelector(".getpdf")!.addEventListener("click", () => clicks++);
  win.document.body.insertAdjacentHTML("beforeend", gate);
  expect(await observe()).toEqual({ status: "blocked", reason });
  expect(await act(first)).toEqual({ status: "blocked", reason });
  expect(clicks).toBe(0);
});

for (const label of ["Accept license", "Buy article", "Request a copy", "Document delivery", "Grant permission", "Subscribe", "Verify credentials"])
  test(`human action is never executable: ${label}`, async () => {
    const win = setup();
    win.document.querySelector("main")!.insertAdjacentHTML("beforeend", `<button>${label}</button>`);
    const first = observed(await observe());
    const control = first.observation.controls.find(c => c.label.startsWith(label))!;
    expect(control.disabled).toBe(true);
    expect(await act(first, { choice: control.id })).toEqual({ status: "stale", reason: "observation_changed" });
  });

for (const mutation of ["form", "field", "label", "ancestor", "disabled", "hidden", "replace", "query", "base", "document", "doi"]) test(`live fingerprint or gate rejects ${mutation} change`, async () => {
  const win = setup(), first = observed(await observe());
  const button = win.document.querySelector(".getpdf")!;
  let clicks = 0; button.addEventListener("click", () => clicks++);
  if (mutation === "form") win.document.querySelector("form")!.action = "/changed";
  if (mutation === "field") win.document.querySelector("input")!.value = "secret";
  if (mutation === "label") button.textContent = "Other";
  if (mutation === "ancestor") button.parentElement!.setAttribute("onclick", "anything()");
  if (mutation === "disabled") button.setAttribute("aria-disabled", "true");
  if (mutation === "hidden") button.setAttribute("hidden", "");
  if (mutation === "replace") button.replaceWith(button.cloneNode(true));
  if (mutation === "query") win.location.search = "?token=changed";
  if (mutation === "base") win.document.head.insertAdjacentHTML("beforeend", '<base href="https://other.test">');
  if (mutation === "document") setup();
  if (mutation === "doi") win.document.querySelector('meta[name="citation_doi"]')!.setAttribute("content", "10.3233/other");
  expect((await act(first)).status).not.toBe("dispatched");
  expect(clicks).toBe(0);
});

test("concurrent observations cannot overwrite newer state; repeated or re-observed revision cannot click twice", async () => {
  const win = setup(); let clicks = 0;
  win.document.querySelector(".getpdf")!.addEventListener("click", () => clicks++);
  const pair = await Promise.all([observe(), observe()]);
  expect(pair.map(r => r.status).sort()).toEqual(["observed", "stale"]);
  const first = observed(pair.find(r => r.status === "observed")!);
  expect((await Promise.all([act(first), act(first)])).map(r => r.status).sort()).toEqual(["dispatched", "stale"]);
  const again = observed(await observe());
  expect(again.observation.revision).toBe(first.observation.revision);
  expect(await act(again)).toEqual({ status: "stale", reason: "observation_changed" });
  expect(clicks).toBe(1);
});

test("WAIT can be followed by a fresh observation and click, and menu changes permit another step", async () => {
  const win = setup();
  const first = observed(await observe());
  const waited = observed(await observe());
  expect(waited.observation.revision).toBe(first.observation.revision);
  expect(await act(waited)).toMatchObject({ status: "dispatched" });
  win.document.querySelector("main")!.insertAdjacentHTML("beforeend", '<button role="menuitem">Full text</button>');
  const second = observed(await observe());
  expect(second.observation.revision).not.toBe(first.observation.revision);
  expect(await act(second, { choice: second.observation.controls.find(c => c.role === "menuitem")!.id })).toMatchObject({ status: "dispatched" });
});

test("explicit navigation stays on the bound article; external/new-tab links and native form submits are disabled", async () => {
  const win = setup();
  win.document.querySelector("main")!.insertAdjacentHTML("beforeend", `<a href="${entryURL}#pdf">Menu</a><a href="/other">Other article</a><a href="${entryURL}" target="_blank">New tab</a><a href="https://other.test/pdf" download>External PDF</a><a href="/paper.pdf" download>Local PDF</a><form action="/Download/Pdf"><button>Submit</button></form>`);
  const controls = observed(await observe()).observation.controls;
  for (const label of ["Other article", "New tab", "External PDF", "Submit"]) expect(controls.find(c => c.label.startsWith(label))?.disabled).toBe(true);
  for (const label of ["Menu", "Local PDF"]) expect(controls.find(c => c.label.startsWith(label))?.disabled).toBe(false);
});

test("the bound publisher URL can identify an article without DOI metadata", async () => {
  const pageURL = "https://psycnet.apa.org/doiLanding?doi=10.1037%2Fcfp0000143";
  const requestedDOI = "10.1037/cfp0000143";
  setup("<main><h1>Example article</h1><button>Download PDF</button></main>", pageURL);
  expect((await agentDOM({ method: "observe", entryURL: pageURL, doi: requestedDOI })).status).toBe("observed");
});

test("the bound article path can identify a DOI without metadata", async () => {
  const pageURL = "https://publisher.example/doi/10.1002/pits.20149";
  setup("<main><button>Download PDF</button></main>", pageURL);
  expect((await agentDOM({ method: "observe", entryURL: pageURL, doi: "10.1002/pits.20149" })).status).toBe("observed");
});

test("a conflicting citation overrides a matching DOI in the bound URL", async () => {
  const pageURL = "https://psycnet.apa.org/doiLanding?doi=10.1037%2Fcfp0000143";
  setup('<meta name="citation_doi" content="10.9999/other"><main><button>Download PDF</button></main>', pageURL);
  expect(await agentDOM({ method: "observe", entryURL: pageURL, doi: "10.1037/cfp0000143" }))
    .toEqual({ status: "blocked", reason: "identity_conflicting" });
});

test("a longer DOI in the bound URL cannot identify its prefix", async () => {
  const pageURL = "https://psycnet.apa.org/doiLanding?doi=10.1037%2Fcfp00001430";
  setup("<main><button>Download PDF</button></main>", pageURL);
  expect(await agentDOM({ method: "observe", entryURL: pageURL, doi: "10.1037/cfp0000143" }))
    .toEqual({ status: "blocked", reason: "identity_missing" });
});

test("a bound URL without the requested DOI still needs article identity", async () => {
  setup("<main><button>Download PDF</button></main>", metadataOnlyURL);
  expect(await observeMetadataOnly()).toEqual({ status: "blocked", reason: "identity_missing" });
});

for (const [suffix, expected] of [
  ["/doi/10.1037/cfp0000143/", "observed"],
  ["/prefix10.1037/cfp0000143", "identity_missing"],
  ["/doi/10.1037/cfp0000143/more", "identity_missing"],
  ["/doiLanding?doi=10.1037%2Fcfp0000143%ZZ", "identity_missing"],
] as const) test(`URL DOI boundaries and malformed escapes: ${suffix}`, async () => {
  const pageURL = `https://publisher.example${suffix}`;
  setup("<main><button>Download PDF</button></main>", pageURL);
  const result = await agentDOM({ method: "observe", entryURL: pageURL, doi: "10.1037/cfp0000143" });
  if (expected === "observed") expect(result.status).toBe("observed");
  else expect(result).toEqual({ status: "blocked", reason: expected });
});

test("scope and live citation are required without an adapter registry", async () => {
  const win = setup('<meta name="citation_doi" content="10.3233/SHTI000001"><article><button>Formats</button></article>', metadataOnlyURL);
  expect((await observeMetadataOnly()).status).toBe("observed");
  win.location.pathname = "/different";
  expect(await observeMetadataOnly()).toEqual({ status: "blocked", reason: "page_binding_failed" });
  win.location.href = metadataOnlyURL;
  win.document.querySelector("meta")!.remove();
  expect(await observeMetadataOnly()).toEqual({ status: "blocked", reason: "identity_missing" });
});

// Minimal public structure from a labelled article DOI field; no captured
// title, identifiers, account state, or provider-specific selectors.
const primaryDOIField = (value = doi) => `<div><strong>DOI: </strong><a href="https://doi.org/${value}">${value}</a></div>`;
const primaryDOIPage = (field = primaryDOIField()) => `<main><h1>Example article</h1><section><h2>Abstract</h2>${field}</section><button type="button">Download PDF</button></main>`;

test("visible primary DOI field supports independently serialized observation and action", async () => {
  const win = setup(primaryDOIPage());
  const injected = new Function(`return (${agentDOM.toString()});`)() as typeof agentDOM;
  let clicks = 0;
  win.document.querySelector("button")!.addEventListener("click", () => clicks++);
  const first = observed(await injected({ method: "observe", entryURL, doi }));
  expect(first.observation.doi).toBe(doi.toLowerCase());
  const pdf = first.observation.controls.find(control => control.label.startsWith("Download PDF"))!;
  expect(pdf.disabled).toBe(false);
  expect(await injected({ method: "act", entryURL, doi, document: first.document, revision: first.observation.revision, choice: pdf.id })).toEqual({ status: "dispatched", downloadExpected: true });
  expect(clicks).toBe(1);
});

for (const secondary of [
  `<section><h2>References</h2>${primaryDOIField()}</section>`,
  `<section><div><h2>References</h2></div>${primaryDOIField()}</section>`,
  `<section><div><div><h2>References</h2><button>Export</button></div></div>${primaryDOIField()}</section>`,
  `<div class="reference-list">${primaryDOIField()}</div>`,
  `<section role="doc-bibliography">${primaryDOIField()}</section>`,
  `<aside>${primaryDOIField()}</aside>`,
  `<section><h2>More Like This</h2>${primaryDOIField()}</section>`,
  `<ol><li>${primaryDOIField()}</li></ol>`,
  `<p>For details see <a href="https://doi.org/${doi}">${doi}</a>.</p>`,
  `<div hidden>${primaryDOIField()}</div>`,
  primaryDOIField().replace("<div>", '<div class="reference-item">'),
  primaryDOIField().replace("<a ", '<a role="doc-biblioref" '),
] as const) test(`secondary or unlabelled DOI cannot establish article identity: ${secondary.slice(0, 45)}`, async () => {
  setup(`<main><h1>Example article</h1>${secondary}<button>Download PDF</button></main>`, metadataOnlyURL);
  expect(await observeMetadataOnly()).toEqual({ status: "blocked", reason: "identity_missing" });
});

test("wrong metadata wins over matching primary and reference DOI fields", async () => {
  setup(`<meta name="citation_doi" content="10.9999/other">${primaryDOIPage()}<article><h2>References</h2>${primaryDOIField()}</article>`);
  expect(await observe()).toEqual({ status: "blocked", reason: "identity_conflicting" });
});

test("a primary DOI field ignores a different DOI in a later reference section", async () => {
  setup(primaryDOIPage().replace("</main>", `<h2>References</h2>${primaryDOIField("10.9999/reference")}</main>`));
  expect((await observe()).status).toBe("observed");
});

test("wrapped primary headings and later wrapped references preserve the primary DOI field", async () => {
  setup(`<main><h1>Example article</h1><section><div><h2>Abstract</h2></div>${primaryDOIField()}</section><section><div><h2>References</h2></div>${primaryDOIField("10.9999/reference")}</section><button>Download PDF</button></main>`);
  expect((await observe()).status).toBe("observed");
});

test("conflicting primary DOI fields refuse even without standard metadata", async () => {
  setup(primaryDOIPage(primaryDOIField() + primaryDOIField("10.9999/other")));
  expect(await observe()).toEqual({ status: "blocked", reason: "identity_conflicting" });
});

for (const primary of [
  primaryDOIField() + primaryDOIField("10.9999/other"),
  `<div><strong>DOI:</strong><a href="https://doi.org/10.9999/other">${doi}</a></div>`,
  primaryDOIField("10.9999/other"),
]) test(`conflicting primary DOI claims refuse: ${primary.slice(-55)}`, async () => {
  setup(`<meta name="citation_doi" content="${doi}">${primaryDOIPage(primary)}`);
  expect(await observe()).toEqual({ status: "blocked", reason: "identity_conflicting" });
});

for (const change of ["href", "text", "remove", "reference", "metadata"] as const)
  test(`primary DOI identity is rechecked before action after ${change} changes`, async () => {
    const win = setup(primaryDOIPage(), metadataOnlyURL);
    let clicks = 0; win.document.querySelector("button")!.addEventListener("click", () => clicks++);
    const first = observed(await observeMetadataOnly());
    const field = win.document.querySelector("section div")!;
    if (change === "href") field.querySelector("a")!.setAttribute("href", "https://doi.org/10.9999/other");
    if (change === "text") field.querySelector("a")!.textContent = "10.9999/other";
    if (change === "remove") field.remove();
    if (change === "reference") win.document.querySelector("h2")!.textContent = "References";
    if (change === "metadata") win.document.head.insertAdjacentHTML("beforeend", '<meta name="citation_doi" content="10.9999/other">');
    expect(await act(first, { entryURL: metadataOnlyURL })).toEqual({ status: "blocked", reason: change === "remove" || change === "reference" ? "identity_missing" : "identity_conflicting" });
    expect(clicks).toBe(0);
  });

for (const [name, content] of [
  ["citation_doi", doi], ["dc.identifier", doi], ["DC.Identifier", `doi:${doi}`],
  ["dc.identifier", `https://doi.org/${doi}`], ["dc.identifier", `http://dx.doi.org/${doi}`],
  ["prism.doi", doi], ["PRISM.DOI", `doi:${doi}`],
] as const) test(`standard DOI metadata provides exact identity: ${name} ${content}`, async () => {
  const win = setup(`<meta name="${name}" content="${content}"><main><button>Formats</button><a href="/articles/63646/viewer">PDF</a></main>`);
  let clicks = 0; win.document.querySelector("a")!.addEventListener("click", () => clicks++);
  const injected = new Function(`return (${agentDOM.toString()});`)() as typeof agentDOM;
  const first = observed(await injected({ method: "observe", entryURL, doi }));
  expect(first.observation.doi).toBe(doi.toLowerCase());
  expect(first.observation.controls.find(c => c.label === "Formats")?.disabled).toBe(false);
  const pdf = first.observation.controls.find(c => c.label === "PDF")!;
  expect(pdf.disabled).toBe(true); // Metadata support does not authorize cross-path navigation.
  expect(await act(first, { choice: pdf.id })).toEqual({ status: "stale", reason: "observation_changed" });
  expect(clicks).toBe(0);
});

for (const content of ["urn:isbn:9781234567890", "local-record-63646", "https://publisher.example/63646"])
  test(`non-DOI DC identifier is not identity evidence: ${content}`, async () => {
    const win = setup(`<meta name="dc.identifier" content="${content}"><main><button>PDF</button></main>`, metadataOnlyURL);
    expect(await observeMetadataOnly()).toEqual({ status: "blocked", reason: "identity_missing" });
    win.document.head.insertAdjacentHTML("beforeend", `<meta name="prism.doi" content="${doi}">`);
    expect((await observeMetadataOnly()).status).toBe("observed");
  });

for (const name of ["citation_doi", "dc.identifier", "prism.doi"]) {
  test(`wrong DOI in ${name} refuses`, async () => {
    setup(`<meta name="${name}" content="doi:10.9999/PRIVATEOTHER"><main><button>PDF</button></main>`);
    expect(await observe()).toEqual({ status: "blocked", reason: "identity_conflicting" });
  });
  for (const other of ["citation_doi", "dc.identifier", "prism.doi"].filter(other => other !== name))
    test(`conflicting DOI claims across ${name} and ${other} refuse before click`, async () => {
      const win = setup(`<meta name="${name}" content="${doi}"><main><button>PDF</button></main>`);
      const first = observed(await observe());
      win.document.head.insertAdjacentHTML("beforeend", `<meta name="${other}" content="https://doi.org/10.9999/PRIVATEOTHER">`);
      expect(await observe()).toEqual({ status: "blocked", reason: "identity_conflicting" });
      expect(await act(first, { choice: first.observation.controls[0]!.id })).toEqual({ status: "blocked", reason: "identity_conflicting" });
    });
}

test("matching standard claims agree but malformed declared DC DOI still refuses", async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><meta name="dc.identifier" content="doi:${doi}"><meta name="prism.doi" content="https://doi.org/${doi}"><main><button>Formats</button></main>`);
  expect((await observe()).status).toBe("observed");
  win.document.head.insertAdjacentHTML("beforeend", '<meta name="dc.identifier" content="doi:PRIVATEINVALID">');
  expect(await observe()).toEqual({ status: "blocked", reason: "identity_conflicting" });
});

for (const [change, reason] of [
  ["missing", "identity_missing"], ["empty", "identity_missing"],
  ["conflicting", "identity_conflicting"], ["extra", "identity_conflicting"],
  ["invalid expected", "identity_invalid"],
] as const) test(`identity refusal is distinct from a human gate: ${change}`, async () => {
  const win = setup(fixture, metadataOnlyURL), first = observed(await observeMetadataOnly());
  let clicks = 0;
  win.document.querySelector(".getpdf")!.addEventListener("click", () => clicks++);
  const citation = win.document.querySelector('meta[name="citation_doi"]')!;
  if (change === "missing") citation.remove();
  if (change === "empty") citation.setAttribute("content", "");
  if (change === "conflicting") citation.setAttribute("content", "10.9999/PRIVATEIDENTITY");
  if (change === "extra") win.document.head.insertAdjacentHTML("beforeend", '<meta name="citation_doi" content="10.9999/PRIVATEIDENTITY">');
  const expected = change === "invalid expected" ? "PRIVATEINVALID" : doi;
  expect(await agentDOM({ method: "observe", entryURL: metadataOnlyURL, doi: expected })).toEqual({ status: "blocked", reason });
  expect(await act(first, { entryURL: metadataOnlyURL, doi: expected })).toEqual({ status: "blocked", reason });
  expect(clicks).toBe(0);
});

test("identity failure does not assert a human gate even when one is also present", async () => {
  const win = setup(fixture, metadataOnlyURL);
  win.document.querySelector('meta[name="citation_doi"]')!.remove();
  win.document.body.insertAdjacentHTML("beforeend", '<dialog open>Accept PRIVATECONSENT</dialog>');
  expect(await observeMetadataOnly()).toEqual({ status: "blocked", reason: "identity_missing" });
});

test("reload and stale controls have separate bounded reasons", async () => {
  const win = setup(), first = observed(await observe());
  win.document.querySelector(".getpdf")!.setAttribute("disabled", "");
  expect(await act(first)).toEqual({ status: "stale", reason: "observation_changed" });
  setup();
  expect(await act(first)).toEqual({ status: "stale", reason: "document_changed" });
});

test("a validation failure during the observation digest retains its exact reason", async () => {
  const win = setup(fixture, metadataOnlyURL);
  const digest = crypto.subtle.digest.bind(crypto.subtle);
  const original = crypto.subtle.digest;
  try {
    crypto.subtle.digest = async (...args) => {
      win.document.querySelector('meta[name="citation_doi"]')!.remove();
      return digest(...args);
    };
    expect(await observeMetadataOnly()).toEqual({ status: "stale", reason: "identity_missing" });
  } finally { crypto.subtle.digest = original; }
});

const credentialBinding = new URL("https://ebooks.iospress.nl/article");
credentialBinding.username = "test-user";
credentialBinding.password = "test-password"; // betterleaks:allow -- synthetic negative-test input, never sent
for (const target of ["https://other.example/article", "http://ebooks.iospress.nl/article", credentialBinding.href, "invalid PRIVATEURL"])
  test(`invalid or different article binding refuses without exposing its value: ${target}`, async () => {
    setup();
    expect(await agentDOM({ method: "observe", entryURL: target, doi })).toEqual({ status: "blocked", reason: "page_binding_failed" });
  });


test("ordinary header search and unrelated newsletter do not disable an entitled PDF", async () => {
  const win = setup();
  win.document.body.insertAdjacentHTML("beforeend", '<header><form><input type="search" value="PRIVATEQUERY"><button>Search</button></form></header><aside><form><input type="email" value="private@example.test"><button>Subscribe</button></form></aside>');
  win.document.querySelector(".getpdf")!.textContent = "Read subscription PDF";
  const first = observed(await observe());
  const control = first.observation.controls.find(c => c.label.startsWith("Read subscription PDF"))!;
  expect(control.disabled).toBe(false);
  expect(JSON.stringify(first)).not.toContain("PRIVATEQUERY");
  expect(JSON.stringify(first)).not.toContain("private@example.test");
  expect(await act(first, { choice: control.id })).toMatchObject({ status: "dispatched" });
});

for (const attributes of ['type="password" hidden', 'type="hidden" name="password"', 'type="hidden" autocomplete="cc-number"'])
  test(`sensitive form inputs are never read by the fingerprint: ${attributes}`, async () => {
    const win = setup();
    const first = observed(await observe());
    win.document.querySelector("form")!.insertAdjacentHTML("beforeend", `<input ${attributes}>`);
    const field = win.document.querySelector("form input:last-child")!;
    let reads = 0;
    Object.defineProperty(field, "value", { get: () => { reads++; throw new Error("password read"); } });
    const next = await observe();
    expect(next.status).toBe("observed");
    if (next.status === "observed") expect(next.observation.controls.some(c => c.label.startsWith("Download PDF"))).toBe(false);
    expect((await act(first)).status).not.toBe("dispatched");
    expect(reads).toBe(0);
  });

test("an explicit same-origin PDF anchor uses its original native click with temporary download intent", async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><main><a href="/download/opaque/paper.PDF?ticket=PRIVATE" referrerpolicy="same-origin">Article PDF</a></main>`);
  const anchor = win.document.querySelector("a")!;
  const before = anchor.outerHTML;
  let clicks = 0;
  anchor.addEventListener("click", event => {
    clicks++;
    expect(event.target).toBe(anchor);
    expect(anchor.getAttribute("download")).toBe("");
    expect(anchor.getAttribute("href")).toBe("/download/opaque/paper.PDF?ticket=PRIVATE");
    expect(anchor.getAttribute("referrerpolicy")).toBe("same-origin");
    event.preventDefault(); // No network from this DOM regression.
  });
  const injected = new Function(`return (${agentDOM.toString()});`)() as typeof agentDOM;
  const first = observed(await injected({ method: "observe", entryURL, doi }));
  expect(first.observation.controls[0]?.disabled).toBe(false);
  const request = { method: "act" as const, entryURL, doi, document: first.document, revision: first.observation.revision, choice: first.observation.controls[0]!.id };
  expect(await injected(request)).toEqual({ status: "dispatched", downloadExpected: true });
  expect(anchor.outerHTML).toBe(before);
  expect(await injected(request)).toEqual({ status: "stale", reason: "observation_changed" });
  expect(clicks).toBe(1);
});

test("a same-origin PDF route whose last segment is the bare word receives download intent", async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><main><a href="/2026/1/e83927/PDF" aria-label="Download PDF">Download PDF</a><a href="/2026/1/e83927/pdfviewer">Viewer</a></main>`);
  const anchors = Array.from(win.document.querySelectorAll("a"));
  let clicks = 0;
  anchors[0]!.addEventListener("click", event => {
    clicks++;
    expect(anchors[0]!.getAttribute("download")).toBe("");
    event.preventDefault();
  });
  const first = observed(await observe());
  const [route, viewer] = first.observation.controls;
  expect(route?.disabled).toBe(false);
  expect(await act(first, { choice: route!.id })).toEqual({ status: "dispatched", downloadExpected: true });
  expect(clicks).toBe(1);
  // `/pdfviewer` is not a PDF route: with navigation off it stays disabled.
  expect(viewer?.disabled).toBe(true);
});

for (const [attributes, text] of [
  ['href="https://external.example/paper.pdf"', "Article PDF"],
  ['href="http://ebooks.iospress.nl/paper.pdf"', "Article PDF"],
  ['href="/paper.pdf" target="_blank"', "Article PDF"],
  ['href="/paper.pdf" target="other"', "Article PDF"],
  ['href="/viewer?format=pdf"', "Article PDF"],
  ['href="/paper.pdf"', "Next"],
  ['href="/paper.pdf"', "Accept terms and download PDF"],
] as const) test(`PDF download intent retains restrictions: ${attributes} ${text}`, async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><main><a ${attributes}>${text}</a></main>`);
  let clicks = 0; win.document.querySelector("a")!.addEventListener("click", () => clicks++);
  const first = observed(await observe());
  expect(first.observation.controls[0]?.disabled).toBe(true);
  expect(await act(first, { choice: first.observation.controls[0]!.id })).toEqual({ status: "stale", reason: "observation_changed" });
  expect(clicks).toBe(0);
  expect(win.document.querySelector("a")!.hasAttribute("download")).toBe(false);
});

test("PDF download intent restores its attribute even if native dispatch throws", async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><main><a href="/paper.pdf">Article PDF</a></main>`);
  const anchor = win.document.querySelector("a")!;
  const first = observed(await observe());
  const original = win.HTMLElement.prototype.click;
  try {
    win.HTMLElement.prototype.click = function () { throw new Error("synthetic dispatch failure"); };
    await expect(act(first, { choice: first.observation.controls[0]!.id })).rejects.toThrow("synthetic dispatch failure");
    expect(anchor.hasAttribute("download")).toBe(false);
  } finally { win.HTMLElement.prototype.click = original; }
  expect(await act(first, { choice: first.observation.controls[0]!.id })).toEqual({ status: "stale", reason: "observation_changed" });
});

test("PDF intent belongs to the selected original anchor, never a conflicting child label", async () => {
  setup(`<meta name="citation_doi" content="${doi}"><main><a href="/paper.pdf" aria-label="Next"><span role="button">PDF</span></a></main>`);
  expect(observed(await observe()).observation.controls.every(c => c.disabled)).toBe(true);
});

for (const existing of [false, true]) test(`PDF intent preserves publisher download values, existing=${existing}`, async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><main><a href="/paper.pdf" aria-haspopup="menu" ${existing ? 'download="publisher.pdf"' : ''}>PDF options</a></main>`);
  const anchor = win.document.querySelector("a")!;
  anchor.addEventListener("click", event => {
    expect(anchor.getAttribute("download")).toBe(existing ? "publisher.pdf" : "");
    if (!existing) anchor.setAttribute("download", "provider-changed.pdf");
    event.preventDefault();
  });
  const first = observed(await observe());
  expect(await act(first, { choice: first.observation.controls[0]!.id })).toEqual({ status: "dispatched", downloadExpected: true });
  expect(anchor.getAttribute("download")).toBe(existing ? "publisher.pdf" : "provider-changed.pdf");
});

for (const change of ["href", "base-target", "identity"]) test(`PDF intent rechecks ${change} before mutation or click`, async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><main><a href="/paper.pdf">Article PDF</a></main>`);
  const anchor = win.document.querySelector("a")!;
  let clicks = 0; anchor.addEventListener("click", () => clicks++);
  const first = observed(await observe());
  if (change === "href") anchor.href = "https://external.example/wrong.pdf";
  if (change === "base-target") win.document.head.insertAdjacentHTML("beforeend", '<base target="_blank">');
  if (change === "identity") win.document.querySelector("meta")!.setAttribute("content", "10.1234/wrong");
  expect((await act(first, { choice: first.observation.controls[0]!.id })).status).not.toBe("dispatched");
  expect(clicks).toBe(0);
  expect(anchor.hasAttribute("download")).toBe(false);
});

for (const css of [
  "position:absolute;clip:rect(1px,1px,1px,1px)",
  "position:fixed;clip:rect(0px,0px,0px,0px)",
  "clip-path:inset(50%)", "clip-path:inset(0 60% 0 40%)",
]) test(`empty CSS clipping hides menu controls without a class-name heuristic: ${css}`, async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><main><button>Formats</button><div style="${css}"><a href="/paper.pdf">Article PDF</a></div></main>`);
  expect(observed(await observe()).observation.controls.map(c => c.label)).toEqual(["Formats"]);
  win.document.querySelector("main div")!.removeAttribute("style");
  const open = observed(await observe());
  expect(open.observation.controls.find(c => c.label === "Article PDF")?.disabled).toBe(false);
});

test("offviewport, partially clipped and visible accessible menus stay projected", async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><main>
    <div class="visuallyhidden" aria-expanded="false"><button>PDF options</button></div>
    <div style="position:absolute;clip:rect(0px,10px,10px,0px)"><button>Full text</button></div>
    <div style="clip-path:inset(40%)"><button>Formats</button></div>
    <div style="position:static;clip:rect(0px,0px,0px,0px)"><button>Download PDF</button></div>
  </main>`);
  Object.assign(win.HTMLElement.prototype, { getClientRects: () => [{ width: 10, height: 10, top: 50000, left: 50000, bottom: 50010, right: 50010 }] });
  expect(observed(await observe()).observation.controls.map(c => c.label)).toEqual(["PDF options", "Full text", "Formats", "Download PDF"]);
});

test("clipping changes invalidate a selected control without weakening conservative credential detection", async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><main><button>Download PDF</button></main>`);
  const first = observed(await observe());
  win.document.querySelector("main")!.style.clipPath = "inset(50%)";
  expect(await act(first)).toEqual({ status: "stale", reason: "observation_changed" });
  win.document.querySelector("main")!.insertAdjacentHTML("beforeend", '<input type="password">');
  expect(await observe()).toEqual({ status: "blocked", reason: "credentials_required" });
});

test("non-article file controls are disabled and refused PDF links cannot crowd out usable controls", async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><main>
    ${Array.from({ length: 85 }, (_, i) => `<a href="https://external.example/${i}.pdf">Article PDF</a>`).join("")}
    <button>Share</button><button>Download citation</button><button>Figures PDF</button>
    <button>Supplementary PDF</button><button>Article metrics</button><button>Formats</button>
  </main>`);
  const first = observed(await observe());
  expect(first.observation.controls).toHaveLength(80);
  expect(first.observation.controls.filter(c => !c.disabled).map(c => c.label)).toEqual(["Formats", "Share"]);
  win.document.querySelectorAll("a").forEach(node => node.remove());
  const next = observed(await observe());
  for (const control of next.observation.controls.filter(c => !["Formats", "Share"].includes(c.label))) {
    expect(control.disabled).toBe(true);
    expect(await act(next, { choice: control.id })).toEqual({ status: "stale", reason: "observation_changed" });
  }
});

for (const name of ["Share", "Cite this article"]) test(`${name} can open a menu containing the article PDF`, async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><main><button>${name}</button></main>`);
  win.document.querySelector("button")!.addEventListener("click", () => {
    win.document.querySelector("main")!.insertAdjacentHTML("beforeend", '<a href="/paper.pdf">Article PDF</a>');
  });
  const first = observed(await observe());
  expect(first.observation.controls[0]?.disabled).toBe(false);
  expect(await act(first, { choice: first.observation.controls[0]!.id })).toEqual({ status: "dispatched", downloadExpected: false });
  const next = observed(await observe());
  const pdf = next.observation.controls[0]!;
  expect(pdf.label).toBe("Article PDF");
  win.document.querySelector("a")!.addEventListener("click", event => event.preventDefault());
  expect(await act(next, { choice: pdf.id })).toEqual({ status: "dispatched", downloadExpected: true });
});

test("bare Download progresses after a clipped panel visibly exposes a usable PDF", async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><main><div><a href="">Download</a>
    <div aria-expanded="false" style="position:absolute;clip:rect(1px,1px,1px,1px)"><a href="/paper.pdf">Article PDF</a></div>
  </div></main>`);
  const trigger = win.document.querySelector("a")!;
  const panel = trigger.nextElementSibling!;
  trigger.addEventListener("click", event => {
    event.preventDefault(); panel.removeAttribute("style"); panel.setAttribute("aria-expanded", "true");
  });
  const first = observed(await observe());
  expect(first.observation.controls.map(c => c.label)).toEqual(["Download"]);
  expect(await act(first, { choice: first.observation.controls[0]!.id })).toEqual({ status: "dispatched", downloadExpected: false });
  const second = observed(await observe());
  expect(second.observation.revision).not.toBe(first.observation.revision);
  const pdf = second.observation.controls.find(c => c.label === "Article PDF")!;
  win.document.querySelector('a[href="/paper.pdf"]')!.addEventListener("click", event => event.preventDefault());
  expect(await act(second, { choice: pdf.id })).toEqual({ status: "dispatched", downloadExpected: true });
});

for (const variant of ["unchanged", "still-clipped", "refused-pdf", "explicit-download"] as const)
  test(`bare Download retains download grace without usable PDF progression: ${variant}`, async () => {
    const win = setup(`<meta name="citation_doi" content="${doi}"><main><div><a href="" ${variant === "explicit-download" ? 'download="paper.pdf"' : ''}>Download</a>
      <div aria-expanded="false" style="clip-path:inset(50%)"><a href="${variant === "refused-pdf" ? 'https://external.example' : ''}/paper.pdf">Article PDF</a></div>
    </div></main>`);
    const trigger = win.document.querySelector("a")!;
    const panel = win.document.querySelector('[aria-expanded="false"]')!;
    trigger.addEventListener("click", event => {
      event.preventDefault();
      if (variant !== "unchanged") panel.setAttribute("aria-expanded", "true");
      if (variant !== "unchanged" && variant !== "still-clipped") panel.removeAttribute("style");
    });
    const first = observed(await observe());
    expect(await act(first, { choice: first.observation.controls.find(c => c.label === "Download")!.id })).toEqual({ status: "dispatched", downloadExpected: true,
      ...(variant === "explicit-download" ? {} : { menuPending: true }) });
  });

for (const variant of ["inserted", "enabled", "existing", "gate", "identity", "navigation"] as const)
  test(`bare Download uses fresh PDF progression, without depending on menu markup: ${variant}`, async () => {
    const win = setup(`<meta name="citation_doi" content="${doi}"><main><button>Download</button>
      <section>${variant === "enabled" || variant === "existing" ? `<button ${variant === "enabled" ? 'disabled' : ''}>Download PDF</button>` : ''}</section>
    </main>`);
    const trigger = win.document.querySelector("button")!;
    trigger.addEventListener("click", () => {
      if (variant === "enabled") win.document.querySelector("section button")!.removeAttribute("disabled");
      else if (variant !== "existing") win.document.querySelector("section")!.insertAdjacentHTML("beforeend", '<button>Download PDF</button>');
      if (variant === "gate") win.document.body.insertAdjacentHTML("beforeend", '<dialog open>Accept terms</dialog>');
      if (variant === "identity") win.document.querySelector("meta")!.setAttribute("content", "10.9999/other");
      if (variant === "navigation") win.location.pathname = "/different";
    });
    const first = observed(await observe());
    const result = await act(first, { choice: first.observation.controls.find(c => c.label === "Download")!.id });
    if (["gate", "identity", "navigation"].includes(variant)) expect(result.status).not.toBe("dispatched");
    else expect(result).toEqual({ status: "dispatched", downloadExpected: variant === "existing", ...(variant === "existing" ? { menuPending: true } : {}) });
  });

test("main article PDF controls mentioning additional material remain executable", async () => {
  setup(`<meta name="citation_doi" content="${doi}"><main>
    <button>Download full article including supplementary material</button><button>Article PDF with figures</button>
  </main>`);
  expect(observed(await observe()).observation.controls.every(c => !c.disabled)).toBe(true);
});

test("a visible menu trigger keeps its clipped accessible name", async () => {
  setup(`<meta name="citation_doi" content="${doi}"><main><button aria-haspopup="menu">
    <span style="position:absolute;clip:rect(1px,1px,1px,1px)">PDF options</span><span aria-hidden="true">icon</span>
  </button></main>`);
  const first = observed(await observe());
  expect(first.observation.controls).toEqual([{ id: "c1", role: "button", label: "PDF options", disabled: false }]);
  expect(await act(first, { choice: "c1" })).toEqual({ status: "dispatched", downloadExpected: false });
});

test("cap reshuffling does not turn an already usable PDF control into menu progression", async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><main><button>Download</button>
    ${Array.from({ length: 80 }, () => '<button>Formats</button>').join("")}<button>Download PDF</button>
  </main>`);
  win.document.querySelector("button")!.addEventListener("click", () => {
    for (const button of win.document.querySelectorAll("button")) if (button.textContent === "Formats") button.remove();
  });
  const first = observed(await observe());
  expect(first.observation.controls.some(c => c.label === "Download PDF")).toBe(false);
  expect(await act(first, { choice: first.observation.controls[0]!.id })).toEqual({ status: "dispatched", downloadExpected: true, menuPending: true });
});

test("local menu checks bind the consumed revision, document and original URL without enabling replay", async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><main><button>Download</button></main>`);
  const first = observed(await observe());
  const check = { method: "check_menu" as const, entryURL, doi, document: first.document, revision: first.observation.revision };
  expect(await agentDOM(check)).toEqual({ status: "stale", reason: "observation_changed" });
  expect(await act(first, { choice: "c1" })).toEqual({ status: "dispatched", downloadExpected: true, menuPending: true });
  expect(await agentDOM(check)).toEqual({ status: "menu_checked", ready: false });
  win.document.querySelector("main")!.insertAdjacentHTML("beforeend", '<button>Download PDF</button>');
  expect(await agentDOM({ ...check, revision: "0".repeat(64) })).toEqual({ status: "stale", reason: "observation_changed" });
  expect(await agentDOM({ ...check, document: "another-document" })).toEqual({ status: "stale", reason: "document_changed" });
  expect(await agentDOM(check)).toEqual({ status: "menu_checked", ready: true });
  expect(await act(first, { choice: "c1" })).toEqual({ status: "stale", reason: "observation_changed" });
  win.location.search = "?changed=1";
  expect(await agentDOM(check)).toEqual({ status: "stale", reason: "page_binding_failed" });
});

test("local menu checking uses the uncapped original baseline and never polls an explicit PDF action", async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><main><button>Download</button>
    ${Array.from({ length: 80 }, () => '<button>Formats</button>').join("")}<a href="/paper.pdf">Article PDF</a>
  </main>`);
  const first = observed(await observe());
  expect(await act(first, { choice: "c1" })).toEqual({ status: "dispatched", downloadExpected: true, menuPending: true });
  win.document.querySelectorAll("button").forEach(node => { if (node.textContent === "Formats") node.remove(); });
  const check = { method: "check_menu" as const, entryURL, doi, document: first.document, revision: first.observation.revision };
  expect(await agentDOM(check)).toEqual({ status: "menu_checked", ready: false });
  const second = observed(await observe());
  const pdf = second.observation.controls.find(c => c.label === "Article PDF")!;
  win.document.querySelector("a")!.addEventListener("click", event => event.preventDefault());
  expect(await act(second, { choice: pdf.id })).toEqual({ status: "dispatched", downloadExpected: true });
  expect(await agentDOM(check)).toEqual({ status: "stale", reason: "observation_changed" });
});

test("local menu readiness requires the new PDF control to fit the next projection", async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><main><button>Download</button>
    ${Array.from({ length: 80 }, () => '<button>Formats</button>').join("")}
  </main>`);
  const first = observed(await observe());
  await act(first, { choice: "c1" });
  const check = { method: "check_menu" as const, entryURL, doi, document: first.document, revision: first.observation.revision };
  win.document.querySelector("main")!.insertAdjacentHTML("beforeend", '<button>Download PDF</button>');
  expect(await agentDOM(check)).toEqual({ status: "menu_checked", ready: false });
  win.document.querySelectorAll("button").forEach(node => { if (node.textContent === "Formats") node.remove(); });
  expect(await agentDOM(check)).toEqual({ status: "menu_checked", ready: true });
});

for (const deadline of ["expired", "fraction", "infinite", "negative"] as const)
  test(`dispatch rejects ${deadline} absolute action deadline without consuming or clicking`, async () => {
    const win = setup(`<meta name="citation_doi" content="${doi}"><main><button>Formats</button></main>`);
    let clicks = 0; win.document.querySelector("button")!.addEventListener("click", () => clicks++);
    const first = observed(await observe());
    const actionDeadline = deadline === "expired" ? Date.now() - 1 : deadline === "fraction" ? 1.5 : deadline === "negative" ? -1 : Infinity;
    const rejected = await act(first, { choice: "c1", actionDeadline });
    expect(rejected.status).toBe(deadline === "expired" ? "stale" : "blocked");
    expect(clicks).toBe(0);
    expect(await act(first, { choice: "c1", actionDeadline: Date.now() + 10_000 })).toMatchObject({ status: "dispatched" });
    expect(clicks).toBe(1);
  });

for (const reveal of ["synchronous", "asynchronous"] as const) test(`prepared navigation can become a ${reveal} same-document menu without replay`, async () => {
  const win = setup(`<meta name="citation_doi" content="${doi}"><main><a href="/article/download">Download</a></main>`);
  const insert = () => win.document.querySelector("main")!.insertAdjacentHTML("beforeend", '<button type="button">Download PDF</button>');
  let clicks = 0;
  win.document.querySelector("a")!.addEventListener("click", event => { event.preventDefault(); clicks++; if (reveal === "synchronous") insert(); });
  const first = observed(await agentDOM({ method: "observe", entryURL, doi, allowNavigation: true }));
  const request = { entryURL, doi, allowNavigation: true, document: first.document, revision: first.observation.revision, choice: "c1" };
  const prepared = await agentDOM({ ...request, method: "prepare" });
  if (prepared.status !== "prepared") throw new Error(prepared.status);
  await agentDOM({ ...request, method: "act", destination: prepared.destination! });
  if (reveal === "asynchronous") {
    expect(await agentDOM({ ...request, method: "check_menu", resumeNavigation: true })).toEqual({ status: "menu_checked", ready: false });
    insert();
  }
  expect(await agentDOM({ ...request, method: "check_menu", resumeNavigation: true })).toEqual({ status: "menu_checked", ready: true });
  const next = observed(await agentDOM({ method: "observe", entryURL, doi, allowNavigation: true, document: first.document }));
  expect(next.document).toBe(first.document);
  expect(next.observation.revision).not.toBe(first.observation.revision);
  expect(next.observation.controls.some(control => control.label === "Download PDF" && !control.disabled)).toBe(true);
  expect((await agentDOM({ ...request, method: "act", destination: prepared.destination! })).status).toBe("stale");
  expect(clicks).toBe(1);
});
