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
  expect(result.observation.controls[0]).toEqual({ id: "c1", role: "button", label: "Download PDF [An example open-access research article]", disabled: false });
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

for (const gate of [
  '<input type="password">', '<input autocomplete="one-time-code">',
  '<iframe title="CAPTCHA challenge"></iframe>', '<div class="cf-turnstile" data-sitekey="secret"></div>',
  '<dialog open>Accept terms and conditions<button>Continue</button></dialog>',
  '<div role="dialog">Purchase access<button>Continue</button></div>',
  '<div role="dialog">Document delivery<button>Continue</button></div>',
  '<div role="dialog">Grant permissions<button>Continue</button></div>',
]) test(`human gate blocks before observation and before click: ${gate}`, async () => {
  const win = setup(), first = observed(await observe());
  win.document.body.insertAdjacentHTML("beforeend", gate);
  expect(await observe()).toEqual({ status: "blocked" });
  expect(await act(first)).toEqual({ status: "blocked" });
});

for (const label of ["Accept license", "Buy article", "Request a copy", "Document delivery", "Grant permission", "Subscribe", "Verify credentials"])
  test(`human action is never executable: ${label}`, async () => {
    const win = setup();
    win.document.querySelector("main")!.insertAdjacentHTML("beforeend", `<button>${label}</button>`);
    const first = observed(await observe());
    const control = first.observation.controls.find(c => c.label.startsWith(label))!;
    expect(control.disabled).toBe(true);
    expect(await act(first, { choice: control.id })).toEqual({ status: "stale" });
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
  expect(await act(again)).toEqual({ status: "stale" });
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
  expect(await observe()).toEqual({ status: "blocked" });
  win.location.href = entryURL;
  win.document.querySelector("meta")!.remove();
  expect(await observe()).toEqual({ status: "blocked" });
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
