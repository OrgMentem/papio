// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import type { AgentDecideObservation } from "./protocol";
export type AgentObservation = AgentDecideObservation;
export interface AgentDOMRequest {
  method: "observe" | "act";
  entryURL: string;
  doi: string;
  /** Worker-local document identity: a navigation/reload cannot resume a loop. */
  document?: string;
  choice?: string;
  revision?: string;
}
/** Fixed public diagnostics only; never include page text or identity values. */
export type AgentDOMRefusalReason =
  | "identity_invalid" | "identity_missing" | "identity_conflicting"
  | "page_binding_failed" | "document_changed" | "observation_changed"
  | "credentials_required" | "challenge_required" | "consent_required"
  | "payment_required" | "human_action_required" | "invalid_request";
export type AgentDOMResult =
  | { status: "observed"; document: string; observation: AgentObservation }
  | { status: "dispatched"; downloadExpected: boolean }
  | { status: "stale" | "blocked"; reason: AgentDOMRefusalReason };

/** Self-contained isolated-world injection. Only the projection leaves the page;
 * URLs, selectors, form execution data and account content never leave it.
 * This first slice stays in one article document. Explicit cross-page links,
 * form navigation and new browsing contexts are refused. Wider navigation needs
 * its own binding/authority design, not a more permissive model instruction. */
export async function agentDOM(request: AgentDOMRequest): Promise<AgentDOMResult> {
  const normalizeDOI = (raw: string) => raw.trim().replace(/^(?:doi:\s*|https?:\/\/(?:dx\.)?doi\.org\/)/i, "").toLowerCase();
  const doi = normalizeDOI(request.doi);
  let entry: URL;
  try { entry = new URL(request.entryURL); }
  catch { return { status: "blocked", reason: "page_binding_failed" }; }
  const binding = JSON.stringify([entry.origin, entry.pathname, doi]);
  const host = globalThis as typeof globalThis & { papioArticleAgent?: {
    node: Document; document: string; binding: string; ids: WeakMap<Element, string>; next: number; serial: number;
    consumed: Set<string>;
    observed?: { revision: string; source: string; targets: Map<string, Element> };
  } };
  const safe = (raw: string | null | undefined, limit = 240) => (raw ?? "")
    .replace(/\b[a-z][a-z\d+.-]*:\/\/\S+|\bwww\.\S+|(?:^|\s)\/\S+|[?#]\S+/gi, " [redacted]")
    .replace(/\b[^\s@]+@[^\s@]+\.[^\s@]+\b/g, "[redacted]")
    .replace(/\b[\w.-]+=[^\s]+|[\w-]{24,}/g, "[redacted]")
    .replace(/[\u0000-\u001f\u007f]/g, " ").replace(/\s+/g, " ").trim().slice(0, limit);
  const privateSelector = 'header,footer,nav,[role="banner"],[role="navigation"],[role="contentinfo"],[data-private],[id*="account" i],[class*="account" i],[id*="profile" i],[class*="profile" i],[id*="login" i],[class*="login" i]';
  const fields = "input,textarea,select,script,style,[contenteditable]";
  const visible = (element: Element) => {
    if (element.matches('input[type="hidden" i]')) return false;
    if (element.closest('[hidden],[inert],[aria-hidden="true"],dialog:not([open])')) return false;
    for (let node: Element | null = element; node; node = node.parentElement) {
      const style = getComputedStyle(node);
      if (style.display === "none" || /hidden|collapse/.test(style.visibility) || style.opacity === "0") return false;
    }
    return Array.from(element.getClientRects()).some(rect => rect.width > 0 && rect.height > 0);
  };
  const publicText = (element: Element): string => Array.from(element.childNodes).map(node => {
    if (node.nodeType === 3) return node.textContent ?? "";
    if (node.nodeType !== 1) return "";
    const child = node as Element;
    return child.matches(`${fields},${privateSelector}`) || !visible(child) ? "" : publicText(child);
  }).join(" ");
  const label = (element: Element) => safe(element.getAttribute("aria-label") || publicText(element).trim() || element.getAttribute("title") || element.getAttribute("alt"));
  const human = /\b(password|passcode|credentials?|sign[ -]?(?:in|out)|log[ -]?(?:in|out)|authentication|verification|captcha|challenge|accept|agree|consent|acknowledge|purchase|buy|pay|checkout|subscribe|document delivery|interlibrary|request (?:a |the )?(?:copy|document)|permissions?|authorize|allow access)\b/i;
  const sensitiveField = (node: Element) => node.matches('input[type="password" i]') ||
    /password|passcode|credential|one-time-code|cc-number|cc-csc|cc-exp|credit.?card|card.?number|cvv|cvc/i.test(
      ["type", "name", "id", "autocomplete"].map(key => node.getAttribute(key) ?? "").join(" "));
  // Inspect attributes only. Even a hidden password must never be read while
  // fingerprinting a form's execution inputs.
  const sensitiveForm = (element: Element) => {
    const form = (element as HTMLButtonElement).form || element.closest("form");
    return Array.from(element.querySelectorAll("input,textarea,select")).some(sensitiveField) ||
      (form !== null && form !== undefined && (
        Array.from(form.elements).some(sensitiveField) ||
        Array.from(form.querySelectorAll("input,textarea,select")).some(sensitiveField)));
  };
  const humanReason = (text: string): AgentDOMRefusalReason => {
    if (/\b(password|passcode|credentials?|sign[ -]?(?:in|out)|log[ -]?(?:in|out)|authentication)\b/i.test(text)) return "credentials_required";
    if (/\b(verification|captcha|challenge)\b/i.test(text)) return "challenge_required";
    if (/\b(purchase|buy|pay|checkout|subscribe)\b/i.test(text)) return "payment_required";
    if (/\b(accept|agree|consent|acknowledge|permissions?|authorize|allow access)\b/i.test(text)) return "consent_required";
    return "human_action_required";
  };
  const validate = (): AgentDOMRefusalReason | undefined => {
    const current = new URL(location.href);
    if (entry.protocol !== "https:" || current.protocol !== "https:" || entry.username || entry.password || current.username || current.password || current.origin !== entry.origin || current.pathname !== entry.pathname) return "page_binding_failed";
    // DC identifiers can name ISBNs, local records or URLs unrelated to a DOI.
    // Only explicit DOI forms count there; every DOI claim across all three
    // standard fields must agree. No body/URL sniffing or first-hit fallback.
    const citations = Array.from(document.querySelectorAll("meta[name]"))
      .filter(node => ["citation_doi", "dc.identifier", "prism.doi"].includes((node.getAttribute("name") ?? "").trim().toLowerCase()))
      .filter(node => node.getAttribute("name")?.trim().toLowerCase() !== "dc.identifier" ||
        /^(?:10\.|doi:|https?:\/\/(?:dx\.)?doi\.org\/)/i.test((node.getAttribute("content") ?? "").trim()))
      .map(node => normalizeDOI(node.getAttribute("content") ?? ""));
    if (!/^10\.\d{4,9}\/[^\s<>"\u0000-\u001f\u007f]+$/.test(doi)) return "identity_invalid";
    if (!citations.length || citations.every(value => value === "")) return "identity_missing";
    if (citations.some(value => value !== doi)) return "identity_conflicting";
    // Visible credential/payment entry is a human gate. Ordinary search and
    // newsletter fields are unrelated; their values are never projected.
    const field = Array.from(document.querySelectorAll("input,textarea,select")).find(node => sensitiveField(node) && visible(node));
    if (field) return /cc-number|cc-csc|cc-exp|credit.?card|card.?number|cvv|cvc/i.test(
      ["type", "name", "id", "autocomplete"].map(key => field.getAttribute(key) ?? "").join(" ")) ? "payment_required" : "credentials_required";
    if (Array.from(document.querySelectorAll('iframe,frame,[data-sitekey],[id*="captcha" i],[class*="captcha" i],.cf-turnstile')).some(node => visible(node) && (node.hasAttribute("data-sitekey") || /captcha|turnstile|challenge/i.test(["src", "title", "id", "class", "name"].map(key => node.getAttribute(key)).join(" "))))) return "challenge_required";
    const gateText = (node: Node): string => node.nodeType === 3 ? node.textContent ?? "" : Array.from(node.childNodes).map(gateText).join(" ");
    for (const dialog of document.querySelectorAll('dialog[open],[role="dialog"],[role="alertdialog"],[aria-modal="true"]')) {
      if (!visible(dialog)) continue;
      const text = `${dialog.getAttribute("aria-label") ?? ""} ${gateText(dialog)}`;
      if (human.test(text)) return humanReason(text);
    }
    return undefined;
  };
  const refusal = validate();
  if (refusal) return { status: "blocked", reason: refusal };
  if (!host.papioArticleAgent || host.papioArticleAgent.node !== document) {
    host.papioArticleAgent = { node: document, document: crypto.randomUUID(), binding, ids: new WeakMap(), next: 0, serial: 0, consumed: new Set() };
  }
  const state = host.papioArticleAgent;
  if (state.binding !== binding || (request.document !== undefined && state.document !== request.document)) return { status: "stale", reason: "document_changed" };
  const scope = 'main,article,[role="main"]';
  const native = 'a[href],button,summary,input[type="button"],input[type="submit"],input[type="image"]';
  const selector = `${native},[role="button"],[role="link"],[role="menuitem"],[role="tab"],[aria-controls],[aria-haspopup],[tabindex],[onclick],.button,.btn`;
  const disabled = (element: Element) => element.matches(':disabled,[aria-disabled="true"]') || !!element.closest('[aria-disabled="true"]') || getComputedStyle(element).pointerEvents === "none";
  const role = (element: Element): AgentObservation["controls"][number]["role"] => {
    const value = element.getAttribute("role");
    return value === "button" || value === "link" || value === "menuitem" || value === "tab" ? value : element.tagName === "A" ? "link" : "button";
  };
  const allowed = (element: Element) => {
    if (human.test(label(element))) return false;
    const anchor = element.closest<HTMLAnchorElement>("a[href]");
    if (anchor) {
      const url = new URL(anchor.href);
      const target = anchor.target || document.querySelector("base")?.target || "_self";
      if (target !== "_self" || url.protocol !== "https:" || url.origin !== entry.origin || url.username || url.password) return false;
      // A download attribute explicitly prevents article navigation. Other links
      // can only move within this exact article (including its query binding).
      if (!anchor.hasAttribute("download") && (url.pathname !== entry.pathname || url.search !== location.search)) return false;
    }
    const form = (element as HTMLButtonElement).form || element.closest("form");
    if (form) {
      const target = element.getAttribute("formtarget") || form.target || document.querySelector("base")?.target || "_self";
      const action = new URL(element.getAttribute("formaction") || form.action);
      if (target !== "_self" || action.protocol !== "https:" || action.origin !== entry.origin || action.username || action.password) return false;
      // Native submit navigation is outside this slice. JS-backed download
      // controls in forms remain usable; their whole form is fingerprinted.
      if (element.matches('button:not([type="button"]),input[type="submit"],input[type="image"]')) return false;
    }
    return true;
  };
  const snapshot = () => {
    const reason = validate();
    if (reason) return { status: "blocked" as const, reason };
    const candidates = Array.from(document.querySelectorAll(selector)).filter(element =>
      element.namespaceURI === "http://www.w3.org/1999/xhtml" && !sensitiveForm(element) && element.closest(scope) && !element.closest(privateSelector) && visible(element) &&
      (!element.matches(fields) || element.matches(native)) && (element.matches(native) || !element.querySelector(selector)) &&
      !/\b(my account|sign[ -]?(?:in|out)|log[ -]?(?:in|out)|profile)\b/i.test(label(element)));
    const targets = new Map<string, Element>();
    const fingerprints: unknown[] = [];
    const controls = candidates.map(element => {
      let id = state.ids.get(element);
      if (!id) { id = `c${++state.next}`; state.ids.set(element, id); }
      targets.set(id, element);
      const form = (element as HTMLButtonElement).form || element.closest("form");
      const ancestors = [];
      for (let node: Element | null = element; node; node = node.parentElement) ancestors.push(Array.from(node.attributes).map(a => [a.name, a.value]));
      fingerprints.push([id, element.outerHTML, element.closest<HTMLAnchorElement>("a[href]")?.href, ancestors, form?.outerHTML,
        form ? Array.from(form.elements).map(node => { const field = node as HTMLInputElement & HTMLSelectElement; return [field.outerHTML, field.value, field.checked, field.selectedIndex]; }) : []]);
      const section = element.closest(`section,aside,[role="region"],${scope}`);
      const heading = section && Array.from(section.querySelectorAll("h1,h2,h3,h4,h5,h6,[role='heading']")).find(node => visible(node) && !node.closest(privateSelector));
      const context = safe(section?.getAttribute("aria-label") || (heading ? publicText(heading) : ""), 100);
      return { id, role: role(element), label: safe(`${label(element)}${context ? ` [${context}]` : ""}`), disabled: disabled(element) || !allowed(element) };
    });
    const projection = { doi, title: safe(document.querySelector('meta[name="citation_title" i]')?.getAttribute("content"), 400), controls: controls.slice(0, 80) };
    const source = JSON.stringify([state.document, binding, location.href, document.baseURI, projection, fingerprints]);
    return { status: "snapshot" as const, projection, source, targets };
  };
  const current = snapshot();
  if (current.status === "blocked") return current;
  if (request.method === "act") {
    const previous = state.observed;
    const choice = request.choice ?? "";
    const target = current.targets.get(choice);
    if (!previous || previous.revision !== request.revision || state.consumed.has(previous.revision) || previous.source !== current.source || !target || previous.targets.get(choice) !== target || !target.isConnected || !current.projection.controls.some(c => c.id === choice && !c.disabled)) return { status: "stale", reason: "observation_changed" };
    // No await between the complete freshness/gate check and dispatch. Consume
    // before the click even when dispatch throws or synchronously re-enters.
    delete state.observed;
    state.consumed.add(previous.revision);
    state.serial++;
    const menu = target.hasAttribute("aria-haspopup") || target.hasAttribute("aria-controls") ||
      target.hasAttribute("aria-expanded") || /\b(options?|formats?|menu)\b/i.test(label(target));
    const downloadExpected = !menu && (/\b(download|pdf|save (?:article|full text))\b/i.test(label(target)) || target.closest("a[download]") !== null);
    HTMLElement.prototype.click.call(target);
    return { status: "dispatched", downloadExpected };
  }
  if (request.method !== "observe") return { status: "blocked", reason: "invalid_request" };
  const serial = ++state.serial;
  delete state.observed;
  const revision = Array.from(new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(current.source)))).map(n => n.toString(16).padStart(2, "0")).join("");
  const fresh = snapshot();
  if (fresh.status === "blocked") return { status: "stale", reason: fresh.reason };
  if (state.serial !== serial || fresh.source !== current.source) return { status: "stale", reason: "observation_changed" };
  state.observed = { revision, source: fresh.source, targets: fresh.targets };
  return { status: "observed", document: state.document, observation: { revision, ...fresh.projection } };
}
