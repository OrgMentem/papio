// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { afterEach, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { Window } from "happy-dom";
import { providerSpikeDOM, type ProviderSpikeRequest } from "../tools/provider-spike-dom";

const entryURL = "https://ebooks.iospress.nl/doi/10.3233/SHTI000001";
const doi = "10.3233/SHTI000001", goal = "Acquire the main article PDF";
function withFixtureCredentials(input: string) {
  const url = new URL(input);
  url.username = "fixture-user";
  url.password = "pass";
  return url.href;
}
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
  for (const [key, value] of Object.entries({ document: win.document, location: win.location, getComputedStyle: win.getComputedStyle.bind(win), HTMLElement: win.HTMLElement, papioProviderSpike: undefined })) {
    if (!saved.has(key)) saved.set(key, Object.getOwnPropertyDescriptor(globalThis, key));
    Object.defineProperty(globalThis, key, { value, writable: true, configurable: true });
  }
  // Happy DOM has no layout. Visibility still checks hidden attributes and ancestor CSS.
  Object.assign(win.HTMLElement.prototype, { getClientRects: () => [{ width: 10, height: 10 }] });
  return win;
}
const observe = (extra: Partial<ProviderSpikeRequest> = {}) => providerSpikeDOM({ method: "observe", entryURL, doi, goal, ...extra });
const act = (observation: any, choice = observation.controls.find((control: any) => control.label.startsWith("Download PDF"))?.id, extra: Partial<ProviderSpikeRequest> = {}) =>
  providerSpikeDOM({ method: "act", entryURL, doi, goal, choice, revision: observation.provenance.revision, ...extra });

test("captured IOS Press JS-backed div is represented and its native click fires without focus", async () => {
  const win = setup(), control = win.document.querySelector("div.button.getpdf")!;
  expect(control.closest("form")?.getAttribute("method")).toBe("post");
  expect(control.hasAttribute("onclick")).toBe(false);
  let clicks = 0;
  control.addEventListener("click", () => clicks++);
  // A provider-defined property must not replace native click dispatch.
  Object.assign(control, { click: () => { throw new Error("provider global called"); } });
  const observed = await observe(), selected = observed.controls.find((value: any) => value.label.startsWith("Download PDF"));
  expect(selected).toEqual({ id: expect.stringMatching(/^c\d+$/), role: "button", label: "Download PDF [An example open-access research article]", disabled: false });
  expect(observed.provenance).toEqual({ kind: "extension-dom", revision: expect.stringMatching(/^[a-f0-9]{64}$/), document: expect.any(String) });
  expect(observed.page.url).toBe("https://papio-provider.invalid/article");
  expect((await observe()).provenance.revision).toBe(observed.provenance.revision);
  expect(await act(observed)).toEqual({ status: "dispatched", effect: "click", target: { tag: "div", role: "button", label: "Download PDF", id: "downloadlink00001", classes: ["button", "getpdf"] } });
  expect(clicks).toBe(1);
  expect(win.document.activeElement).toBe(win.document.body);
});

test("serialized injected function has no module closure dependencies", async () => {
  setup();
  const injected = new Function(`return (${providerSpikeDOM.toString()});`)() as typeof providerSpikeDOM;
  const observed = await injected({ method: "observe", entryURL, doi, goal });
  expect(observed.controls.some((control: any) => control.label.startsWith("Download PDF"))).toBe(true);
  expect((await injected({ method: "act", entryURL, doi, goal, choice: observed.controls[0].id, revision: observed.provenance.revision })).status).toBe("dispatched");
});

test("main/article/role=main and section headings retain context; private navigation and hidden controls are omitted", async () => {
  const win = setup();
  win.document.body.insertAdjacentHTML("beforeend", '<article><section><h2>Supplementary material</h2><span role="button">Appendix PDF</span></section></article><div role="main"><div class="btn">Full text</div></div><aside><button>Outside article</button></aside>');
  win.document.querySelector("main")!.insertAdjacentHTML("beforeend", '<nav><a href="/account">Account nav</a></nav><header><button>Header control</button></header><div class="account-menu"><button>Personal identity</button></div><button hidden>Hidden</button><div style="display:none"><button>CSS hidden</button></div><button disabled>Unavailable</button><textarea>Private field</textarea>');
  const observed = await observe(), labels = observed.controls.map((control: any) => control.label).join(" ");
  expect(labels).toContain("Appendix PDF [Supplementary material]");
  expect(labels).toContain("Full text");
  for (const omitted of ["Outside article", "Account nav", "Header control", "Personal identity", "Hidden", "CSS hidden", "Private field"]) expect(labels).not.toContain(omitted);
  const disabled = observed.controls.find((control: any) => control.label.startsWith("Unavailable"));
  expect(disabled.disabled).toBe(true);
  expect(await act(observed, disabled.id)).toEqual({ status: "stale" });
});

test("model projection excludes body prose, real URLs, selectors, queries, cookies and all form values", async () => {
  const win = setup(fixture, entryURL + "?account=ACCOUNTSECRET");
  win.document.title = "ACCOUNTSECRET";
  win.document.cookie = "session=COOKIESECRET";
  win.document.querySelector("main")!.insertAdjacentHTML("beforeend", '<p>BODYPROSESECRET</p><div class="account-details">NAMEDPERSONSECRET</div><button aria-label="PDF https://private.test/file?secret=QUERYSECRET user@example.test ABCDEFGHIJKLMNOPQRSTUVWXYZ012345">PDF</button>');
  win.document.querySelector("form")!.insertAdjacentHTML("beforeend", '<input name="FIELDNAMESECRET" value="FIELDVALUESECRET"><textarea>TEXTAREASECRET</textarea><select><option selected>OPTIONSECRET</option></select>');
  win.document.querySelector("input")!.value = "HIDDENSECRET";
  win.document.querySelector("div.getpdf")!.insertAdjacentHTML("beforeend", '<input value="NESTEDSECRET"><span hidden>HIDDENTEXTSECRET</span>');
  const observed = await observe(), json = JSON.stringify(observed);
  for (const secret of ["ACCOUNTSECRET", "COOKIESECRET", "BODYPROSESECRET", "NAMEDPERSONSECRET", "QUERYSECRET", "user@example.test", "ABCDEFGHIJKLMNOPQRSTUVWXYZ012345", "FIELDNAMESECRET", "FIELDVALUESECRET", "TEXTAREASECRET", "OPTIONSECRET", "HIDDENSECRET", "NESTEDSECRET", "HIDDENTEXTSECRET", "ebooks.iospress.nl", "private.test", "creativecommons.org", "downloadlink00001", "getpdf", "/Download/Pdf"]) expect(json).not.toContain(secret);
  expect(observed.page.title).toBe("An example open-access research article");
  expect(observed.controls.some((control: any) => control.label.includes("[redacted]"))).toBe(true);
});

test("DOI normalization accepts prefixes and case but never a different or ambiguous citation", async () => {
  const win = setup(), meta = win.document.querySelector('meta[name="citation_doi"]')!;
  meta.setAttribute("content", " https://doi.org/10.3233/shti000001 ");
  const observed = await observe({ doi: " DOI:10.3233/SHTI000001 " });
  expect(observed.controls.length).toBeGreaterThan(0);
  meta.setAttribute("content", "10.3233/SHTI000002");
  await expect(observe()).rejects.toThrow("citation DOI mismatch");
  await expect(act(observed)).rejects.toThrow("citation DOI mismatch");
  meta.setAttribute("content", doi);
  win.document.head.insertAdjacentHTML("beforeend", '<meta name="citation_doi" content="10.3233/other">');
  await expect(observe()).rejects.toThrow("citation DOI mismatch");
  win.document.querySelectorAll('meta[name="citation_doi"]').forEach(node => node.remove());
  await expect(observe()).rejects.toThrow("citation DOI mismatch");
});

test("bound HTTPS origin and exact pathname reject scope escape on observe and act", async () => {
  for (const url of ["http://ebooks.iospress.nl/doi/10.3233/SHTI000001", "https://other.test/doi/10.3233/SHTI000001", entryURL + "/extra", entryURL + "/", withFixtureCredentials(entryURL)]) {
    const win = setup(), observed = await observe();
    win.location.href = url;
    await expect(observe()).rejects.toThrow("Outside bound");
    await expect(act(observed)).rejects.toThrow("Outside bound");
  }
});

test("entry query is not authority, but local URL changes invalidate a revision", async () => {
  const win = setup(fixture, entryURL + "?session=one");
  const first = await observe({ entryURL: entryURL + "?session=different" });
  expect((await observe()).provenance.revision).toBe(first.provenance.revision);
  win.location.search = "?session=two";
  expect(await act(first)).toEqual({ status: "stale" });
  expect((await observe()).provenance.revision).not.toBe(first.provenance.revision);
  win.location.pathname += "/other";
  await expect(observe({ entryURL: win.location.href })).rejects.toThrow("Outside bound provider experiment");
});

const gates = [
  '<input type="password">',
  '<iframe src="https://challenges.cloudflare.com/turnstile"></iframe>',
  '<iframe title="CAPTCHA challenge"></iframe>',
  '<div class="g-recaptcha" data-sitekey="private"></div>',
  '<dialog open><h2>Terms and conditions</h2><button>Accept</button></dialog>',
  '<div role="dialog"><div class="login"><h2>Sign in</h2><input type="text"></div></div>',
  '<div aria-modal="true">Agree to the license agreement<button>Continue</button></div>',
];
for (const gate of gates) test(`human gate refuses observation and a previously observed action: ${gate}`, async () => {
  const win = setup(), observed = await observe();
  let clicks = 0;
  win.document.querySelector(".getpdf")!.addEventListener("click", () => clicks++);
  win.document.body.insertAdjacentHTML("beforeend", gate);
  await expect(observe()).rejects.toThrow("Human gate:");
  await expect(act(observed)).rejects.toThrow("Human gate:");
  expect(clicks).toBe(0);
});

test("ordinary license links and hidden password controls are not human gates", async () => {
  const win = setup();
  win.document.body.insertAdjacentHTML("beforeend", '<div hidden><input type="password"><iframe title="captcha"></iframe></div><dialog>Sign in</dialog>');
  const observed = await observe();
  expect(observed.controls.some((control: any) => control.label.includes("Creative Commons"))).toBe(true);
  expect((await act(observed)).status).toBe("dispatched");
});

for (const mutation of ["replace", "duplicate", "concurrent", "form action", "hidden value", "data attribute", "label", "disabled", "hidden", "remove", "query", "base", "document"]) test(`refuses stale or repeated action: ${mutation}`, async () => {
  const win = setup(), observed = await observe(), control = win.document.querySelector(".getpdf")!;
  let clicks = 0;
  control.addEventListener("click", () => clicks++);
  if (mutation === "replace") control.replaceWith(control.cloneNode(true));
  if (mutation === "form action") win.document.querySelector("form")!.action = "/different";
  if (mutation === "hidden value") win.document.querySelector("input")!.value = "changed-secret";
  if (mutation === "data attribute") control.setAttribute("data-token", "changed-secret");
  if (mutation === "label") control.textContent = "Changed PDF";
  if (mutation === "disabled") control.setAttribute("aria-disabled", "true");
  if (mutation === "hidden") control.setAttribute("hidden", "");
  if (mutation === "remove") control.remove();
  if (mutation === "query") win.location.search = "?token=changed-secret";
  if (mutation === "base") win.document.head.insertAdjacentHTML("beforeend", '<base href="https://other.test/">');
  if (mutation === "document") setup();
  if (mutation === "duplicate") expect((await act(observed)).status).toBe("dispatched");
  if (mutation === "concurrent") {
    expect((await Promise.all([act(observed), act(observed)])).map(result => result.status).sort()).toEqual(["dispatched", "stale"]);
  } else expect(await act(observed)).toEqual({ status: "stale" });
  expect(clicks).toBe(["duplicate", "concurrent"].includes(mutation) ? 1 : 0);
});

test("state changes during asynchronous hashing are rechecked before clicking", async () => {
  const win = setup(), observed = await observe();
  let clicks = 0;
  win.document.querySelector(".getpdf")!.addEventListener("click", () => clicks++);
  const pending = act(observed);
  win.document.body.insertAdjacentHTML("beforeend", '<input type="password">');
  await expect(pending).rejects.toThrow("Human gate:");
  expect(clicks).toBe(0);
});

test("only existing same-origin HTTPS anchor destinations can be clicked", async () => {
  for (const href of ["/Download/Pdf?private=token", "https://other.test/pdf", "http://ebooks.iospress.nl/pdf", "javascript:void(0)", withFixtureCredentials("https://ebooks.iospress.nl/pdf")]) {
    const win = setup();
    win.document.querySelector("main")!.insertAdjacentHTML("beforeend", `<a href="${href}">Anchor PDF</a>`);
    const anchor = win.document.querySelector("main > a")!;
    let clicks = 0;
    anchor.addEventListener("click", event => { event.preventDefault(); clicks++; });
    const observed = await observe(), selected = observed.controls.find((control: any) => control.label.startsWith("Anchor PDF"));
    if (href.startsWith("/")) expect(await act(observed, selected.id)).toMatchObject({ status: "dispatched", effect: "click", target: { tag: "a" } });
    else await expect(act(observed, selected.id)).rejects.toThrow("Anchor leaves bound HTTPS origin");
    expect(clicks).toBe(href.startsWith("/") ? 1 : 0);
    expect(await act(observed, selected.id)).toEqual({ status: "stale" });
  }
});

test("hidden execution changes affect revision without revealing their values", async () => {
  const win = setup(), before = await observe();
  win.document.querySelector("input")!.value = "SECRET_LOCAL_VALUE";
  const after = await observe();
  expect(after.provenance.revision).not.toBe(before.provenance.revision);
  expect(after.controls).toEqual(before.controls);
  expect(after.page).toEqual(before.page);
  expect(JSON.stringify(after)).not.toContain("SECRET_LOCAL_VALUE");
});

test("coverage accounts for capped controls without exposing omitted text", async () => {
  const win = setup();
  for (let index = 0; index < 90; index++) win.document.querySelector("main")!.insertAdjacentHTML("beforeend", `<button>Control ${index}</button>`);
  const observed = await observe();
  expect(observed.controls).toHaveLength(80);
  expect(observed.coverage).toEqual({ candidates: 92, omitted: 12, frames: 0 });
});
