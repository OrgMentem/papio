// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Self-contained for chrome.scripting serialization. Development fixture only;
// there is no production job/effect authority in this module.
export interface PageSpikeRequest {
  method: "observe" | "act";
  prefix: string;
  goal: string;
  choice?: string;
  revision?: string;
}
export async function pageSpikeDOM(request: PageSpikeRequest): Promise<any> {
  if (!location.href.startsWith(request.prefix + "/")) throw new Error("Outside bound fixture");
  const host = globalThis as typeof globalThis & { papioPageSpike?: {
    document: string; ids: WeakMap<Element, string>; next: number;
    observed?: { revision: string; targets: Map<string, Element>; fingerprints: Map<string, string> };
  } };
  const state = host.papioPageSpike ??= { document: crypto.randomUUID(), ids: new WeakMap(), next: 0 };
  const text = (raw: string | null | undefined, limit = 240) => (raw ?? "").replace(/\s+/g, " ").trim().slice(0, limit);
  const visible = (element: Element) => {
    if (element.closest('[hidden],[inert],[aria-hidden="true"]')) return false;
    const style = getComputedStyle(element);
    return style.display !== "none" && style.visibility !== "hidden" && element.getClientRects().length > 0;
  };
  const label = (element: Element) => text(element.getAttribute("aria-label") || element.textContent || element.getAttribute("title"));
  const fingerprint = (element: Element) => JSON.stringify([element.tagName, label(element), element.getAttribute("href"), element.getAttribute("type"), element.hasAttribute("download"), element.getAttribute("aria-disabled"), element.hasAttribute("disabled")]);
  const disabled = (element: Element) => element.matches(":disabled,[aria-disabled='true']");
  const candidates = Array.from(document.querySelectorAll("a[href],button,[role='button'],[role='link']")).filter(visible);
  const targets = new Map<string, Element>(), fingerprints = new Map<string, string>();
  const controls = candidates.slice(0, 80).map(element => {
    let id = state.ids.get(element);
    if (!id) { id = `c${++state.next}`; state.ids.set(element, id); }
    targets.set(id, element); fingerprints.set(id, fingerprint(element));
    const section = element.closest("main,article,aside,section,[role='dialog']");
    const heading = section?.querySelector("h1,h2,h3");
    const context = text(section?.getAttribute("aria-label") || heading?.textContent, 100);
    return { id, role: element.getAttribute("role") || (element.tagName === "A" ? "link" : "button"), label: text(`${label(element)}${context ? ` [${context}]` : ""}`), disabled: disabled(element) };
  });
  // This producer is intentionally fixture-scoped. A production producer needs
  // redaction and coverage for frames, shadow DOM and authenticated page context.
  const page = { url: location.href, title: document.title.slice(0, 400), text: text(document.querySelector("main")?.textContent, 10000) };
  const source = JSON.stringify({ document: state.document, page, controls, fingerprints: [...fingerprints] });
  const revision = Array.from(new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(source)))).map(n => n.toString(16).padStart(2, "0")).join("");
  const observation = { goal: request.goal, page, controls, provenance: { kind: "extension-dom", revision, document: state.document }, coverage: { candidates: candidates.length, omitted: Math.max(0, candidates.length - controls.length), frames: document.querySelectorAll("iframe").length } };
  if (request.method === "observe") {
    state.observed = { revision, targets, fingerprints };
    return observation;
  }
  const previous = state.observed, target = targets.get(request.choice ?? "");
  if (!previous || previous.revision !== request.revision || revision !== request.revision || !target || previous.targets.get(request.choice!) !== target || !target.isConnected || !visible(target) || disabled(target) || fingerprint(target) !== fingerprints.get(request.choice!) || fingerprint(target) !== previous.fingerprints.get(request.choice!)) return { status: "stale" };
  // Consume the observation before the effect; duplicate delivery cannot replay it.
  delete state.observed;
  if (target.tagName === "A") {
    const url = (target as HTMLAnchorElement).href;
    if (!url.startsWith(request.prefix + "/")) throw new Error("Link leaves bound fixture");
    return { status: "dispatched", effect: target.hasAttribute("download") || target.getAttribute("type") === "application/pdf" ? "download" : "navigate", url };
  }
  (target as HTMLElement).click();
  return { status: "dispatched", effect: "click" };
}
