// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { afterEach, expect, test } from "bun:test";
import { Window } from "happy-dom";
import { pageSpikeDOM } from "../tools/page-spike-dom";
const prefix = "https://example.test/papio-fixture";
const saved = new Map<string, PropertyDescriptor | undefined>();
afterEach(() => { for (const [key, descriptor] of saved) { if (descriptor) Object.defineProperty(globalThis, key, descriptor); else Reflect.deleteProperty(globalThis, key); } saved.clear(); });
function setup() {
  const win = new Window({ url: prefix + "/start" });
  win.document.body.innerHTML = '<main><h1>Main article</h1><a href="article">Read article</a><a href="paper.pdf" type="application/pdf">PDF</a><button>Next</button><button hidden>Hidden</button><button disabled>Disabled</button></main><aside><h2>References</h2><a href="other">Related PDF</a></aside>';
  for (const [key, value] of Object.entries({ document: win.document, location: win.location, getComputedStyle: win.getComputedStyle.bind(win), papioPageSpike: undefined })) {
    if (!saved.has(key)) saved.set(key, Object.getOwnPropertyDescriptor(globalThis, key)); Object.defineProperty(globalThis, key, { value, writable: true, configurable: true });
  }
  for (const element of win.document.querySelectorAll("a,button")) Object.assign(element, { getClientRects: () => [{ width: 10, height: 10 }] });
  return win;
}
const observe = () => pageSpikeDOM({ method: "observe", prefix, goal: "Get main article PDF" });
test("observations retain section context, omit hidden controls and distinguish navigation from download", async () => {
  setup(); const first = await observe();
  expect(first.controls).toHaveLength(5);
  expect(first.controls[0].label).toBe("Read article [Main article]");
  expect(first.controls[4].label).toBe("Related PDF [References]");
  const second = await observe(); expect(second.provenance.revision).toBe(first.provenance.revision);
  expect(await pageSpikeDOM({ method: "act", prefix, goal: "", choice: first.controls[1].id, revision: first.provenance.revision })).toEqual({ status: "dispatched", effect: "download", url: prefix + "/paper.pdf" });
});
test("replaced identical DOM, changed href, changed document and duplicate delivery cannot replay a target", async () => {
  for (const mutate of ["replace", "href", "navigation", "duplicate"]) {
    const win = setup(), observed = await observe(), anchor = win.document.querySelector("a")!;
    const request = { method: "act" as const, prefix, goal: "", choice: observed.controls[0].id, revision: observed.provenance.revision };
    if (mutate === "replace") { const clone = anchor.cloneNode(true); Object.assign(clone, { getClientRects: () => [{}] }); anchor.replaceWith(clone); }
    if (mutate === "href") anchor.setAttribute("href", "other");
    if (mutate === "navigation") win.location.href = prefix + "/other";
    if (mutate === "duplicate") expect((await pageSpikeDOM(request)).status).toBe("dispatched");
    expect(await pageSpikeDOM(request)).toEqual({ status: "stale" });
  }
});
test("ordinary button activation works without focusing it; disabled targets are refused", async () => {
  const win = setup(); let clicks = 0;
  win.document.querySelector("button")!.addEventListener("click", () => clicks++);
  const observed = await observe();
  expect(await pageSpikeDOM({ method: "act", prefix, goal: "", choice: observed.controls[3].id, revision: observed.provenance.revision })).toEqual({ status: "stale" });
  expect(await pageSpikeDOM({ method: "act", prefix, goal: "", choice: observed.controls[2].id, revision: observed.provenance.revision })).toEqual({ status: "dispatched", effect: "click" });
  expect(clicks).toBe(1); expect(win.document.activeElement).toBe(win.document.body);
});
