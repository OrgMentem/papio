// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Development-only, one bound article; self-contained for chrome.scripting.
export interface ProviderSpikeRequest {
  method: "observe" | "act"; entryURL: string; doi: string; goal: string; choice?: string; revision?: string;
}
export async function providerSpikeDOM(request: ProviderSpikeRequest): Promise<any> {
  const normalizeDOI = (raw: string) => raw.trim().replace(/^(?:doi:\s*|https?:\/\/(?:dx\.)?doi\.org\/)/i, "").toLowerCase();
  const doi = normalizeDOI(request.doi), entry = new URL(request.entryURL);
  const binding = JSON.stringify([entry.origin, entry.pathname, doi]);
  const host = globalThis as typeof globalThis & { papioProviderSpike?: {
    node: Document; document: string; binding: string; ids: WeakMap<Element, string>; next: number;
    observed?: { revision: string; targets: Map<string, Element>; fingerprints: Map<string, string> };
  } };
  const safe = (raw: string | null | undefined, limit = 240) => (raw ?? "")
    .replace(/\b[a-z][a-z\d+.-]*:\/\/\S+|\bwww\.\S+|(?:^|\s)\/\S+|[?#]\S+/gi, " [redacted]")
    .replace(/\b[^\s@]+@[^\s@]+\.[^\s@]+\b/g, "[redacted]")
    .replace(/\b[\w.-]+=[^\s]+|[\w-]{24,}/g, "[redacted]").replace(/\s+/g, " ").trim().slice(0, limit);
  const privateSelector = 'header,footer,nav,[role="banner"],[role="navigation"],[role="contentinfo"],[data-private],[id*="account" i],[class*="account" i],[id*="profile" i],[class*="profile" i],[id*="login" i],[class*="login" i]';
  const fields = "input,textarea,select,script,style,[contenteditable]";
  const visible = (element: Element) => {
    if (element.closest('[hidden],[inert],[aria-hidden="true"],dialog:not([open])')) return false;
    for (let node: Element | null = element; node; node = node.parentElement) {
      const style = getComputedStyle(node);
      if (style.display === "none" || /hidden|collapse/.test(style.visibility) || style.opacity === "0") return false;
    }
    return Array.from(element.getClientRects()).some(rect => rect.width > 0 && rect.height > 0);
  };
  // Read only control/heading text; never values, scripts, hidden or account descendants.
  const publicText = (element: Element): string => Array.from(element.childNodes).map(node => {
    if (node.nodeType === 3) return node.textContent ?? "";
    if (node.nodeType !== 1) return "";
    const child = node as Element;
    return child.matches(`${fields},${privateSelector}`) || !visible(child) ? "" : publicText(child);
  }).join(" ");
  const label = (element: Element) => safe(element.getAttribute("aria-label") || publicText(element).trim() || element.getAttribute("title") || element.getAttribute("alt"));
  const validate = () => {
    const current = new URL(location.href);
    if (entry.protocol !== "https:" || current.protocol !== "https:" || entry.username || entry.password || current.username || current.password || current.origin !== entry.origin || current.pathname !== entry.pathname)
      throw new Error("Outside bound provider article");
    const citations = Array.from(document.querySelectorAll('meta[name="citation_doi" i]')).map(node => normalizeDOI(node.getAttribute("content") ?? ""));
    if (!/^10\.\d{4,9}\/\S+$/.test(doi) || !citations.length || citations.some(value => value !== doi)) throw new Error("Provider article citation DOI mismatch");
    if (Array.from(document.querySelectorAll('input[type="password" i]')).some(visible)) throw new Error("Human gate: visible password input");
    const challenge = /captcha|turnstile|challenge/i;
    if (Array.from(document.querySelectorAll('iframe,frame,[data-sitekey],[id*="captcha" i],[class*="captcha" i],.cf-turnstile')).some(node => visible(node) && (node.hasAttribute("data-sitekey") || challenge.test(["src", "title", "id", "class", "name"].map(key => node.getAttribute(key)).join(" ")))))
      throw new Error("Human gate: CAPTCHA/challenge");
    const gateText = (node: Node): string => node.nodeType === 3 ? node.textContent ?? "" : Array.from(node.childNodes).map(gateText).join(" ");
    for (const dialog of document.querySelectorAll('dialog[open],[role="dialog"],[role="alertdialog"],[aria-modal="true"]')) {
      if (!visible(dialog)) continue;
      const text = `${dialog.getAttribute("aria-label") ?? ""} ${gateText(dialog)}`;
      if (/\b(password|passcode|credentials?|sign[ -]?in|log[ -]?in|authentication|verification code)\b/i.test(text) || (/\b(accept|agree|consent|acknowledge|confirm)\b/i.test(text) && /\b(terms|conditions|licen[sc]e|agreement)\b/i.test(text)))
        throw new Error("Human gate: terms/credential dialog");
    }
  };
  validate();
  if (request.method !== "observe" && request.method !== "act") throw new Error("Unknown provider DOM method");
  if (!host.papioProviderSpike || host.papioProviderSpike.node !== document)
    host.papioProviderSpike = { node: document, document: crypto.randomUUID(), binding, ids: new WeakMap(), next: 0 };
  const state = host.papioProviderSpike;
  if (state.binding !== binding) throw new Error("Outside bound provider experiment");
  const previous = state.observed;
  const scope = 'main,article,[role="main"]';
  const native = 'a[href],button,summary,input[type="button"],input[type="submit"],input[type="image"]';
  // Generic custom affordances include JS-backed divs with no inline handler.
  const selector = `${native},[role="button"],[role="link"],[role="menuitem"],[role="tab"],[aria-controls],[aria-haspopup],[tabindex],[onclick],.button,.btn`;
  const disabled = (element: Element) => element.matches(':disabled,[aria-disabled="true"]') || !!element.closest('[aria-disabled="true"]') || getComputedStyle(element).pointerEvents === "none";
  const role = (element: Element) => {
    const value = element.getAttribute("role") ?? "";
    return ["button", "link", "menuitem", "tab"].includes(value) ? value : element.tagName === "A" ? "link" : "button";
  };
  const fingerprint = (element: Element) => {
    const form = (element as HTMLButtonElement).form || element.closest("form");
    // These strings stay local. Hash hidden fields and resolved URLs, not just the model label.
    return JSON.stringify([element.outerHTML, element.closest<HTMLAnchorElement>("a[href]")?.href, document.baseURI, form?.outerHTML,
      form ? Array.from(form.elements).map(node => {
        const field = node as HTMLInputElement & HTMLSelectElement;
        return [field.outerHTML, field.value, field.checked, field.selectedIndex];
      }) : []]);
  };
  const snapshot = () => {
    validate();
    const candidates = Array.from(document.querySelectorAll(selector)).filter(element =>
      element.namespaceURI === "http://www.w3.org/1999/xhtml" && element.closest(scope) && !element.closest(privateSelector) && visible(element) &&
      (!element.matches(fields) || element.matches(native)) &&
      (element.matches(native) || !element.querySelector(selector)) &&
      !/\b(my account|sign[ -]?(?:in|out)|log[ -]?(?:in|out)|profile)\b/i.test(label(element)));
    const targets = new Map<string, Element>(), fingerprints = new Map<string, string>();
    const controls = candidates.slice(0, 80).map(element => {
      let id = state.ids.get(element);
      if (!id) { id = `c${++state.next}`; state.ids.set(element, id); }
      targets.set(id, element); fingerprints.set(id, fingerprint(element));
      const section = element.closest(`section,aside,[role="region"],${scope}`);
      const heading = section && Array.from(section.querySelectorAll("h1,h2,h3,h4,h5,h6,[role='heading']")).find(node => visible(node) && !node.closest(privateSelector));
      const context = safe(section?.getAttribute("aria-label") || (heading ? publicText(heading) : ""), 100);
      return { id, role: role(element), label: safe(`${label(element)}${context ? ` [${context}]` : ""}`), disabled: disabled(element) };
    });
    const title = safe(document.querySelector('meta[name="citation_title" i]')?.getAttribute("content"), 400);
    const page = { url: "https://papio-provider.invalid/article", title, text: `DOI: ${safe(doi)}. Public citation metadata and visible article controls only.` };
    const coverage = { candidates: candidates.length, omitted: Math.max(0, candidates.length - controls.length), frames: document.querySelectorAll("iframe,frame").length };
    const projection = { goal: safe(request.goal, 1000), page, controls, coverage };
    const source = JSON.stringify({ document: state.document, binding, url: location.href, projection, fingerprints: [...fingerprints] });
    return { projection, source, targets, fingerprints };
  };
  const current = snapshot();
  const revision = Array.from(new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(current.source)))).map(n => n.toString(16).padStart(2, "0")).join("");
  // Hashing yields: re-read scope, identity, gates, nodes and execution data afterwards.
  const fresh = snapshot();
  if (fresh.source !== current.source) return { status: "stale" };
  if (request.method === "observe") {
    state.observed = { revision, targets: fresh.targets, fingerprints: fresh.fingerprints };
    return { ...fresh.projection, provenance: { kind: "extension-dom", revision, document: state.document } };
  }
  const choice = request.choice ?? "", target = fresh.targets.get(choice);
  if (!previous || state.observed !== previous || previous.revision !== request.revision || revision !== request.revision || !target || previous.targets.get(choice) !== target || !target.isConnected || !visible(target) || disabled(target) || fresh.fingerprints.get(choice) !== previous.fingerprints.get(choice)) return { status: "stale" };
  // Consume before dispatch, including synchronous re-entry and failed effects.
  delete state.observed;
  const anchor = target.closest<HTMLAnchorElement>("a[href]");
  if (anchor) {
    const url = new URL(anchor.href);
    if (url.protocol !== "https:" || url.origin !== entry.origin || url.username || url.password) throw new Error("Anchor leaves bound HTTPS origin");
  }
  const trace = { tag: target.tagName.toLowerCase(), role: role(target), label: label(target), ...(target.id ? { id: target.id } : {}), ...(target.classList.length ? { classes: Array.from(target.classList) } : {}) };
  HTMLElement.prototype.click.call(target);
  return { status: "dispatched", effect: "click", target: trace };
}
