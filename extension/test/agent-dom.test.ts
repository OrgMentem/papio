// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { afterEach, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { Window } from "happy-dom";
import { agentDOM, type AgentDOMRequest, type AgentDOMResult } from "../src/agent-dom";

const entryURL = "https://ebooks.iospress.nl/doi/10.3233/SHTI000001";
const doi = "10.3233/SHTI000001";
const fixture = readFileSync(new URL("../fixtures/iospress/success.html", import.meta.url), "utf8");
const saved = new Map<string, PropertyDescriptor | undefined>();
afterEach(() => {
  for (const [key, descriptor] of saved) {
    if (descriptor) Object.defineProperty(globalThis, key, descriptor); else Reflect.deleteProperty(globalThis, key);
  }
  saved.clear();
});
function setup(html = fixture) {
  const win = new Window({ url: entryURL, settings: { enableJavaScriptEvaluation: false, disableCSSFileLoading: true, disableJavaScriptFileLoading: true, disableIframePageLoading: true } });
  win.document.write(html);
  for (const [key, value] of Object.entries({ document: win.document, location: win.location, getComputedStyle: win.getComputedStyle.bind(win), HTMLElement: win.HTMLElement, papioArticleAgent: undefined })) {
    if (!saved.has(key)) saved.set(key, Object.getOwnPropertyDescriptor(globalThis, key));
    Object.defineProperty(globalThis, key, { value, writable: true, configurable: true });
  }
  Object.assign(win.HTMLElement.prototype, { getClientRects: () => [{ width: 10, height: 10 }] });
  return win;
}
const observe = () => agentDOM({ method: "observe", entryURL, doi });
function observed(result: AgentDOMResult) {
  if (result.status !== "observed") throw new Error(result.status);
  return result;
}
const act = (result: ReturnType<typeof observed>, extra: Partial<AgentDOMRequest> = {}) => agentDOM({ method: "act", entryURL, doi,
  document: result.document, revision: result.observation.revision,
  choice: result.observation.controls.find(c => c.label.startsWith("Download PDF"))?.id ?? "", ...extra });

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
  expect(controls.map(c => c.label)).toEqual(["Download", "Options [Local formats]", "Share", "Save PNG [Figures]", "Cite"]);
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
    ["Formats", false], ["Article PDF", true], ["Download PDF", false], ["Share 0 [Download PDF]", false],
  ]);
  expect(first.observation.controls.some(c => /citation|Figures|Supplementary/.test(c.label))).toBe(false);
  expect(first.observation.controls[0]!.id).not.toBe("c1"); // IDs belong to elements, not rank positions.
  const pdf = first.observation.controls[1]!;
  let clicks = 0; win.document.querySelector("a")!.addEventListener("click", () => clicks++);
  expect(await act(first, { choice: pdf.id })).toEqual({ status: "stale", reason: "observation_changed" });
  expect(clicks).toBe(0);
  expect(observed(await observe()).observation).toEqual(first.observation);
  win.document.querySelector("main")!.insertAdjacentHTML("afterbegin", '<button>PDF options</button>');
  const next = observed(await observe());
  expect(next.observation.controls[0]!.label).toBe("PDF options");
  expect(next.observation.controls.find(c => c.label === "Formats")!.id).toBe(first.observation.controls[0]!.id);
  // A control outside the transmitted cap still participates in freshness.
  win.document.querySelector("section button:last-child")!.setAttribute("onclick", "changed()");
  expect(await act(next)).toEqual({ status: "stale", reason: "observation_changed" });
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

test("scope and live citation are required without an adapter registry", async () => {
  const win = setup('<meta name="citation_doi" content="10.3233/SHTI000001"><article><button>Formats</button></article>');
  expect((await observe()).status).toBe("observed");
  win.location.pathname = "/different";
  expect(await observe()).toEqual({ status: "blocked", reason: "page_binding_failed" });
  win.location.href = entryURL;
  win.document.querySelector("meta")!.remove();
  expect(await observe()).toEqual({ status: "blocked", reason: "identity_missing" });
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
    const win = setup(`<meta name="dc.identifier" content="${content}"><main><button>PDF</button></main>`);
    expect(await observe()).toEqual({ status: "blocked", reason: "identity_missing" });
    win.document.head.insertAdjacentHTML("beforeend", `<meta name="prism.doi" content="${doi}">`);
    expect((await observe()).status).toBe("observed");
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
  const win = setup(), first = observed(await observe());
  let clicks = 0;
  win.document.querySelector(".getpdf")!.addEventListener("click", () => clicks++);
  const citation = win.document.querySelector('meta[name="citation_doi"]')!;
  if (change === "missing") citation.remove();
  if (change === "empty") citation.setAttribute("content", "");
  if (change === "conflicting") citation.setAttribute("content", "10.9999/PRIVATEIDENTITY");
  if (change === "extra") win.document.head.insertAdjacentHTML("beforeend", '<meta name="citation_doi" content="10.9999/PRIVATEIDENTITY">');
  const expected = change === "invalid expected" ? "PRIVATEINVALID" : doi;
  expect(await agentDOM({ method: "observe", entryURL, doi: expected })).toEqual({ status: "blocked", reason });
  expect(await act(first, { doi: expected })).toEqual({ status: "blocked", reason });
  expect(clicks).toBe(0);
});

test("identity failure does not assert a human gate even when one is also present", async () => {
  const win = setup();
  win.document.querySelector('meta[name="citation_doi"]')!.remove();
  win.document.body.insertAdjacentHTML("beforeend", '<dialog open>Accept PRIVATECONSENT</dialog>');
  expect(await observe()).toEqual({ status: "blocked", reason: "identity_missing" });
});

test("reload and stale controls have separate bounded reasons", async () => {
  const win = setup(), first = observed(await observe());
  win.document.querySelector(".getpdf")!.setAttribute("disabled", "");
  expect(await act(first)).toEqual({ status: "stale", reason: "observation_changed" });
  setup();
  expect(await act(first)).toEqual({ status: "stale", reason: "document_changed" });
});

test("a validation failure during the observation digest retains its exact reason", async () => {
  const win = setup();
  const digest = crypto.subtle.digest.bind(crypto.subtle);
  const original = crypto.subtle.digest;
  try {
    crypto.subtle.digest = async (...args) => {
      win.document.querySelector('meta[name="citation_doi"]')!.remove();
      return digest(...args);
    };
    expect(await observe()).toEqual({ status: "stale", reason: "identity_missing" });
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
