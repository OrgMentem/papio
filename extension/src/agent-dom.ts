// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import type { AgentDecideObservation } from "./protocol";
export type AgentObservation = AgentDecideObservation;
export interface AgentDOMRequest {
  method: "observe" | "prepare" | "act" | "check_menu" | "observe_pdf" | "act_pdf";
  entryURL: string;
  doi: string;
  /** Worker-local document identity: a navigation/reload cannot resume a loop. */
  document?: string;
  choice?: string;
  revision?: string;
  /** Negotiated worker capability; URLs remain internal, never model input. */
  allowNavigation?: boolean;
  destination?: string;
  /** Worker-only predecessor of its exact selected same-origin navigation.
   * Authorizes only one exposed PDF transfer, never inherited model controls. */
  sourceURL?: string;
  /** Absolute browser clock deadline; queued injections cannot outlive it.
   * Required at runtime for act_pdf, which cannot inherit an unbounded grant. */
  actionDeadline?: number;
  /** Only the worker's unchanged-source navigation wait can resume this menu. */
  resumeNavigation?: boolean;
}
/** Fixed public diagnostics only; never include page text or identity values. */
export type AgentDOMRefusalReason =
  | "identity_invalid" | "identity_missing" | "identity_conflicting"
  | "page_binding_failed" | "document_changed" | "observation_changed"
  | "credentials_required" | "challenge_required" | "consent_required"
  | "payment_required" | "human_action_required" | "invalid_request";
export type AgentDOMResult =
  | { status: "observed"; document: string; observation: AgentObservation }
  | { status: "pdf_observed"; document: string; revision: string }
  | { status: "prepared"; effect: "local" | "navigate"; destination?: string }
  | { status: "dispatched"; downloadExpected: boolean; menuPending?: true }
  | { status: "menu_checked"; ready: boolean }
  | { status: "stale" | "blocked"; reason: AgentDOMRefusalReason };

/** Self-contained isolated-world injection. Only the projection leaves the page;
 * Selectors, form execution data and account content never leave it. Navigation
 * preparation returns the selected URL only to the worker, which owns the
 * document transition. Form navigation and new browsing contexts stay refused. */
export async function agentDOM(request: AgentDOMRequest): Promise<AgentDOMResult> {
  const normalizeDOI = (raw: string) => raw.trim().replace(/^(?:doi:\s*|https?:\/\/(?:dx\.)?doi\.org\/)/i, "").toLowerCase();
  const doi = normalizeDOI(request.doi);
  let entry: URL;
  try { entry = new URL(request.entryURL); }
  catch { return { status: "blocked", reason: "page_binding_failed" }; }
  const binding = JSON.stringify([entry.origin, entry.pathname, doi]);
  const pdfWrapper = request.method === "observe_pdf" || request.method === "act_pdf";
  const host = globalThis as typeof globalThis & { papioArticleAgent?: {
    node: Document; document: string; binding: string; ids: WeakMap<Element, string>; next: number; serial: number;
    consumed: Set<string>;
    retired?: boolean;
    prepared?: { revision: string; choice: string; destination: string };
    menuWait?: { revision: string; url: string; enabled: Set<string>; navigation?: true };
    observed?: { revision: string; source: string; targets: Map<string, Element> };
    pdfObserved?: { revision: string; source: string; target: Element };
  } };
  const safe = (raw: string | null | undefined, limit = 240) => (raw ?? "")
    .replace(/\b[a-z][a-z\d+.-]*:\/\/\S+|\bwww\.\S+|(?:^|\s)\/\S+|[?#]\S+/gi, " [redacted]")
    .replace(/\b[^\s@]+@[^\s@]+\.[^\s@]+\b/g, "[redacted]")
    .replace(/\b[\w.-]+=[^\s]+|[\w-]{24,}/g, "[redacted]")
    .replace(/[\u0000-\u001f\u007f]/g, " ").replace(/\s+/g, " ").trim().slice(0, limit);
  const privateSelector = 'header,footer,nav,[role="banner"],[role="navigation"],[role="contentinfo"],[data-private],[id*="account" i],[class*="account" i],[id*="profile" i],[class*="profile" i],[id*="login" i],[class*="login" i]';
  const fields = "input,textarea,select,script,style,[contenteditable]";
  // Detect empty clip regions, not viewport position or CSS class names. A
  // positive layout box can still be entirely clipped (common for closed menus).
  const emptyClip = (style: CSSStyleDeclaration) => {
    const rect = /^(?:absolute|fixed)$/.test(style.position) && /^rect\(([^()]*)\)$/.exec(style.clip);
    if (rect) {
      const sides = rect[1]!.trim().split(/[,\s]+/).map(value => /^-?(?:\d*\.)?\d+(?:px)?$/.test(value) ? parseFloat(value) : NaN);
      if (sides.length === 4 && (sides[1]! <= sides[3]! || sides[2]! <= sides[0]!)) return true;
    }
    const inset = /^inset\(([^()]*)\)$/.exec(style.clipPath);
    if (!inset) return false;
    const values = inset[1]!.split(/\s+round\s+/)[0]!.trim().split(/\s+/);
    // Percentage insets describe the untransformed reference box. Avoid using
    // viewport geometry to guess mixed lengths, transforms or arbitrary paths.
    if (values.length < 1 || values.length > 4 || values.some(value => !/^-?(?:\d*\.)?\d+%$|^0(?:px)?$/.test(value))) return false;
    const [top, right = top, bottom = top, left = right] = values.map(parseFloat);
    return top! + bottom! >= 100 || right! + left! >= 100;
  };
  const visible = (element: Element, checkClip = true) => {
    if (element.matches('input[type="hidden" i]')) return false;
    if (element.closest('[hidden],[inert],[aria-hidden="true"],dialog:not([open])')) return false;
    for (let node: Element | null = element; node; node = node.parentElement) {
      const style = getComputedStyle(node);
      if (style.display === "none" || /hidden|collapse/.test(style.visibility) || style.opacity === "0") return false;
      if (checkClip && emptyClip(style)) return false;
    }
    return Array.from(element.getClientRects()).some(rect => rect.width > 0 && rect.height > 0);
  };
  const publicText = (element: Element): string => Array.from(element.childNodes).map(node => {
    if (node.nodeType === 3) return node.textContent ?? "";
    if (node.nodeType !== 1) return "";
    const child = node as Element;
    // Clipped text can be the accessible name of an otherwise visible button.
    return child.matches(`${fields},${privateSelector}`) || !visible(child, false) ? "" : publicText(child);
  }).join(" ");
  const label = (element: Element) => safe(element.getAttribute("aria-label") || publicText(element).trim() || element.getAttribute("title") || element.getAttribute("alt"));
  const hasPublicLabel = (text: string) => text.replace(/\[redacted\]/g, "").trim() !== "";
  // Rank only the control's own public name, never a neighbouring heading.
  // This orders evidence within the cap; it grants no execution permission.
  const priority = (text: string) => /\b(share|metrics|statistics|citations?|cite|bibtex|ris|figures?|tables?|supplement(?:ary|al)?|appendi(?:x|ces)|references?)\b/i.test(text) ? 2
    : /\b(pdf|download|full[ -]?text|formats?|options?|menu|(?:read|save|view) article)\b/i.test(text) ? 0 : 1;
  // Whole-purpose labels only: an article PDF may include figures/supplements.
  // Share/Cite can open an acquisition menu; rank them lower, don't forbid it.
  const unrelated = (text: string) => /^(?:download citation|figures? pdf|supplement(?:ary|al)? pdf|article (?:metrics|statistics))$/i.test(text);
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
  const cookieNotice = (dialog: Element) => {
    const text = `${dialog.getAttribute("aria-label") ?? ""} ${dialog.textContent ?? ""}`;
    return /\b(cookies?|privacy (?:notice|preferences|settings)|tracking)\b/i.test(text) &&
      !/\b(terms|licen[cs]e|agreement|purchase|buy|subscribe|password|sign[ -]?in|log[ -]?in)\b/i.test(text);
  };
  const primaryArticleDOIs = (): string[] => {
    const claims: string[] = [];
    const secondary = /(?:^|[\s_-])(?:refs?|references?|bibliograph(?:y|ies)|citations?|related|recommended|recommendations?)(?:$|[\s_-])/i;
    const secondaryHeading = /^(?:references?|bibliography|citations?|related (?:articles?|content)|recommended (?:articles?|content)|more like this|further reading)\b/i;
    const headingSelector = 'h1,h2,h3,h4,h5,h6,[role="heading"]';
    const sectionSelector = 'section,article,aside,main,[role="region"],[role="main"]';
    const labels = ["main", "article", '[role="main"]'].flatMap(scope => ["strong", "b", "span"].map(tag => `${scope} ${tag}`)).join(",");
    for (const name of document.querySelectorAll(labels)) {
      // A property field, not a DOI mentioned in prose or a reference link:
      // exactly a leading visible label and its value, with no other text.
      // The value is one doi.org link, or plain DOI text that line-break hints
      // (<wbr>) may split. Measured 2026-09-23 on methods.sagepub.com: a
      // chapter page names its own DOI only as a "Chapter DOI:" list item.
      const field = name.parentElement;
      if (!field || field.firstElementChild !== name || !/doi/i.test(name.textContent ?? "")) continue;
      const labelText = publicText(name).trim();
      // Reference and related-content lists never label an entry "Chapter
      // DOI", so only that label may sit in a list; plain DOI lists stay out.
      const chapter = /^chapter\s+doi\s*:$/i.test(labelText);
      if (!chapter && !/^doi\s*:$/i.test(labelText)) continue;
      const children = Array.from(field.children);
      const texts = Array.from(field.childNodes).filter(node => node.nodeType === 3).map(node => node.textContent ?? "");
      const anchor = children.length === 2 && children[1]!.matches("a[href]") ? children[1]! : undefined;
      const href = anchor?.getAttribute("href") ?? "";
      const text = texts.join("").trim();
      if (!field.matches(chapter ? "div,p,dd,li" : "div,p,dd") || (anchor
        ? !/^https?:\/\/(?:dx\.)?doi\.org\//i.test(href) || texts.some(value => value.trim())
        : children.slice(1).some(child => !child.matches("wbr")) || !/^(?:https?:\/\/(?:dx\.)?doi\.org\/)?10\.\d{4,9}\/\S+$/i.test(text))) continue;
      const value = anchor ?? field;
      if (!visible(value) || !visible(name) ||
        value.closest(`${privateSelector},aside,blockquote,cite,${chapter ? "" : "ol,ul,"}[itemprop~="citation"],[role="doc-biblioref"],[role="doc-bibliography"],[role="doc-endnotes"]`)) continue;
      let excluded = false;
      for (let branch: Element = anchor ?? name, region: Element | null = field; region; branch = region, region = region.parentElement) {
        if (secondary.test([region.id, region.getAttribute("class"), region.getAttribute("aria-label"), region.getAttribute("itemprop")].join(" "))) { excluded = true; break; }
        // Headings can sit inside layout wrappers. Read only semantic headings
        // in preceding siblings, within this same section; a separate sibling
        // section's heading does not relabel the article field.
        const siblings = Array.from(region.children);
        const section = region.closest(sectionSelector);
        const heading = siblings.slice(0, siblings.indexOf(branch)).reverse()
          .flatMap(node => node.matches(headingSelector) ? [node] : Array.from(node.querySelectorAll(headingSelector)))
          .find(node => node.closest(sectionSelector) === section && visible(node));
        if (heading && secondaryHeading.test(publicText(heading).trim())) { excluded = true; break; }
      }
      if (!excluded) claims.push(...anchor ? [normalizeDOI(href), normalizeDOI(publicText(anchor))] : [normalizeDOI(text)]);
    }
    return claims;
  };
  const validate = (): AgentDOMRefusalReason | undefined => {
    const current = new URL(location.href);
    if (entry.protocol !== "https:" || current.protocol !== "https:" || entry.username || entry.password || current.username || current.password || current.origin !== entry.origin || current.pathname !== entry.pathname) return "page_binding_failed";
    if (request.allowNavigation && current.href !== entry.href) return "page_binding_failed";
    if (pdfWrapper) {
      try {
        if (!request.sourceURL || !/^https:\/\//i.test(request.sourceURL) || /[\u0000-\u0020\u007f\\]/.test(request.sourceURL) ||
          /%(?![a-f\d]{2})/i.test(request.sourceURL)) return "page_binding_failed";
        const source = new URL(request.sourceURL ?? "");
        if (request.allowNavigation !== true || source.protocol !== "https:" || source.username || source.password ||
          source.origin !== current.origin || source.href === current.href) return "page_binding_failed";
      } catch { return "page_binding_failed"; }
    }
    // DC identifiers can name ISBNs, local records or URLs unrelated to a DOI.
    // Only explicit DOI forms count there. Every DOI claim across the three
    // standard fields and labelled public fields must be the requested DOI or
    // its container: a book DOI that the requested chapter DOI extends at a
    // separator (10.4135/9781849209823 for 10.4135/9781849209823.n2). A
    // container never proves the chapter; the requested DOI itself must be
    // claimed or carried by the bound URL. Siblings and unrelated DOIs conflict.
    const citations = Array.from(document.querySelectorAll("meta[name]"))
      .filter(node => ["citation_doi", "dc.identifier", "prism.doi"].includes((node.getAttribute("name") ?? "").trim().toLowerCase()))
      .filter(node => node.getAttribute("name")?.trim().toLowerCase() !== "dc.identifier" ||
        /^(?:10\.|doi:|https?:\/\/(?:dx\.)?doi\.org\/)/i.test((node.getAttribute("content") ?? "").trim()))
      .map(node => normalizeDOI(node.getAttribute("content") ?? ""));
    const doiShape = /^10\.\d{4,9}\/[^\s<>"\u0000-\u001f\u007f]+$/;
    if (!doiShape.test(doi)) return "identity_invalid";
    const claims = [...citations.some(value => value !== "") || pdfWrapper ? citations : [], ...primaryArticleDOIs()];
    if (claims.some(value => value !== doi && !(doiShape.test(value) && value.length < doi.length &&
      doi.startsWith(value) && /[._/-]/.test(doi[value.length]!)))) return "identity_conflicting";
    if (!claims.includes(doi) && !(pdfWrapper && claims.length === 0)) {
      let hasURLDOI = false;
      if (!pdfWrapper) try {
        const pathAndQuery = decodeURIComponent(current.pathname + current.search).toLowerCase();
        for (let index = pathAndQuery.indexOf(doi); index !== -1; index = pathAndQuery.indexOf(doi, index + 1)) {
          const before = pathAndQuery[index - 1];
          const after = pathAndQuery[index + doi.length];
          if ((!before || /[\/=:\s]/.test(before)) &&
            (!after || /[&#?\s]/.test(after) || (after === "/" && index + doi.length + 1 === pathAndQuery.length))) {
            hasURLDOI = true;
            break;
          }
        }
      } catch { /* Malformed escapes cannot establish article identity. */ }
      if (!hasURLDOI) return claims.length ? "identity_conflicting" : "identity_missing";
    }
    // Visible credential/payment entry is a human gate. Ordinary search and
    // newsletter fields are unrelated; their values are never projected.
    const field = Array.from(document.querySelectorAll("input,textarea,select")).find(node => sensitiveField(node) && visible(node, false));
    if (field) return /cc-number|cc-csc|cc-exp|credit.?card|card.?number|cvv|cvc/i.test(
      ["type", "name", "id", "autocomplete"].map(key => field.getAttribute(key) ?? "").join(" ")) ? "payment_required" : "credentials_required";
    if (Array.from(document.querySelectorAll('iframe,frame,[data-sitekey],[id*="captcha" i],[class*="captcha" i],.cf-turnstile')).some(node => visible(node, false) && (node.hasAttribute("data-sitekey") || /captcha|turnstile|challenge/i.test(["src", "title", "id", "class", "name"].map(key => node.getAttribute(key)).join(" "))))) return "challenge_required";
    const gateText = (node: Node): string => node.nodeType === 3 ? node.textContent ?? "" : Array.from(node.childNodes).map(gateText).join(" ");
    // A cookie or privacy notice is ambient, not a gate: it asks nothing about
    // the article, and its buttons are never offered to the model (see
    // `cookieNotice` in the candidate filter). Measured 2026-09-23 on
    // methods.sagepub.com: a cookie banner's "Accept" halted the attempt as
    // consent_required on an entitled page. A notice that also names terms,
    // a licence, a purchase or credentials stays a gate.
    for (const dialog of document.querySelectorAll('dialog[open],[role="dialog"],[role="alertdialog"],[aria-modal="true"]')) {
      if (!visible(dialog, false) || cookieNotice(dialog)) continue;
      const text = `${dialog.getAttribute("aria-label") ?? ""} ${gateText(dialog)}`;
      if (human.test(text)) return humanReason(text);
    }
    // A wrapper transfer has no model-selected control to run through allowed().
    // Visible gate controls outside dialogs must therefore stop it here too.
    if (pdfWrapper) for (const control of document.querySelectorAll('a[href],button,[role="button"],[role="link"]')) {
      if (visible(control, false) && human.test(label(control))) return humanReason(label(control));
    }
    return undefined;
  };
  const refusal = validate();
  if (refusal) return { status: "blocked", reason: refusal };
  if (!host.papioArticleAgent || host.papioArticleAgent.node !== document) {
    host.papioArticleAgent = { node: document, document: crypto.randomUUID(), binding, ids: new WeakMap(), next: 0, serial: 0, consumed: new Set() };
  }
  const state = host.papioArticleAgent;
  const checkingNavigationMenu = request.method === "check_menu" && request.resumeNavigation === true && state.menuWait?.navigation === true;
  if ((state.retired && !checkingNavigationMenu) || state.binding !== binding || (request.document !== undefined && state.document !== request.document)) return { status: "stale", reason: "document_changed" };
  if (pdfWrapper) {
    if (request.method === "act_pdf" && request.actionDeadline === undefined) return { status: "blocked", reason: "invalid_request" };
    // Only the worker can attest to the preceding selected navigation. This
    // branch reads the wrapper's exposed attributes, never an embedded document
    // or a native viewer, and produces no model observation or URL-bearing result.
    const pdfSnapshot = () => {
      if (host.papioArticleAgent !== state || state.node !== document) return { status: "stale" as const, reason: "document_changed" as const };
      const reason = validate();
      if (reason) return { status: "blocked" as const, reason };
      // Count every visible embedded medium before testing PDF eligibility. A
      // second frame of unknown type cannot silently be assumed unrelated.
      const media = Array.from(document.querySelectorAll("embed,iframe,object,frame,video,audio")).filter(element => visible(element));
      const target = media[0];
      if (media.length !== 1 || !target || target.namespaceURI !== "http://www.w3.org/1999/xhtml" ||
        !target.matches("embed,iframe,object") || target.hasAttribute("srcdoc")) return { status: "blocked" as const, reason: "observation_changed" as const };
      const raw = target.getAttribute(target.tagName === "OBJECT" ? "data" : "src");
      if (!raw?.trim() || /[\u0000-\u0020\u007f\\]/.test(raw) || /%(?![a-f\d]{2})/i.test(raw)) return { status: "blocked" as const, reason: "observation_changed" as const };
      let file: URL;
      try { file = new URL(raw, document.baseURI); }
      catch { return { status: "blocked" as const, reason: "observation_changed" as const }; }
      if (file.protocol !== "https:" || file.origin !== entry.origin || file.username || file.password ||
        (!/\.pdf$/i.test(file.pathname) && target.getAttribute("type")?.trim().toLowerCase() !== "application/pdf")) return { status: "blocked" as const, reason: "observation_changed" as const };
      const source = JSON.stringify(["pdf", state.document, location.href, file.href, request.sourceURL, doi]);
      if (request.actionDeadline !== undefined && (!Number.isSafeInteger(request.actionDeadline) || request.actionDeadline < 0)) return { status: "blocked" as const, reason: "invalid_request" as const };
      if (request.actionDeadline !== undefined && Date.now() >= request.actionDeadline) return { status: "stale" as const, reason: "observation_changed" as const };
      return { status: "pdf" as const, source, target, fileURL: file.href };
    };
    const current = pdfSnapshot();
    if (current.status !== "pdf") return current;
    if (request.method === "act_pdf") {
      const previous = state.pdfObserved;
      if (!previous || request.document !== state.document || request.revision !== previous.revision || state.consumed.has(previous.revision) ||
        previous.source !== current.source || previous.target !== current.target || !current.target.isConnected) return { status: "stale", reason: "observation_changed" };
      // Synchronous freshness check above, consumption before dispatch: even a
      // throwing or re-entrant click cannot reuse this inherited authorization.
      delete state.pdfObserved;
      delete state.observed;
      delete state.prepared;
      delete state.menuWait;
      state.consumed.add(previous.revision);
      state.serial++;
      state.retired = true;
      const anchor = document.createElement("a");
      anchor.href = current.fileURL;
      anchor.download = "";
      anchor.target = "_self";
      try {
        document.body.appendChild(anchor);
        HTMLElement.prototype.click.call(anchor);
      } finally { anchor.remove(); }
      return { status: "dispatched", downloadExpected: true };
    }
    const serial = ++state.serial;
    delete state.pdfObserved;
    delete state.observed;
    delete state.prepared;
    delete state.menuWait;
    const revision = Array.from(new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(current.source)))).map(n => n.toString(16).padStart(2, "0")).join("");
    const fresh = pdfSnapshot();
    if (fresh.status !== "pdf") return { status: "stale", reason: fresh.reason };
    if (state.serial !== serial || fresh.source !== current.source || fresh.target !== current.target) return { status: "stale", reason: "observation_changed" };
    state.pdfObserved = { revision, source: fresh.source, target: fresh.target };
    return { status: "pdf_observed", document: state.document, revision };
  }
  const scope = 'main,article,[role="main"]';
  const native = 'a[href],button,summary,input[type="button"],input[type="submit"],input[type="image"]';
  const selector = `${native},[role="button"],[role="link"],[role="menuitem"],[role="tab"],[aria-controls],[aria-haspopup],[tabindex],[onclick],.button,.btn`;
  const disabled = (element: Element) => element.matches(':disabled,[aria-disabled="true"]') || !!element.closest('[aria-disabled="true"]') || getComputedStyle(element).pointerEvents === "none";
  const role = (element: Element): AgentObservation["controls"][number]["role"] => {
    const value = element.getAttribute("role");
    return value === "button" || value === "link" || value === "menuitem" || value === "tab" ? value : element.tagName === "A" ? "link" : "button";
  };
  // A PDF path is `…/name.pdf` or a route whose last segment is the bare word
  // (JMIR serves `/2026/1/e83927/PDF`, measured 2026-09-23: the agent chose
  // that "Download PDF" anchor as navigation, the tab became a viewer, and
  // the attempt stopped as "no fresh matching article").
  const explicitPDFLink = (element: Element, anchor: HTMLAnchorElement) =>
    element === anchor && /\bpdf\b/i.test(label(anchor)) && /(?:\.pdf|\/pdf)$/i.test(new URL(anchor.href).pathname);
  const navigationTarget = (element: Element): string | undefined => {
    const anchor = element.closest<HTMLAnchorElement>("a[href]");
    if (!request.allowNavigation || element !== anchor || anchor.hasAttribute("download") || explicitPDFLink(element, anchor)) return undefined;
    const url = new URL(anchor.href);
    return url.pathname !== entry.pathname || url.search !== location.search ? url.href : undefined;
  };
  const allowed = (element: Element) => {
    if (human.test(label(element)) || unrelated(label(element))) return false;
    const anchor = element.closest<HTMLAnchorElement>("a[href]");
    if (anchor) {
      const url = new URL(anchor.href);
      const target = anchor.target || document.querySelector("base")?.target || "_self";
      if (url.protocol !== "https:" || url.origin !== entry.origin || url.username || url.password) return false;
      // Explicit PDF links receive download intent at dispatch. Browser policy
      // and publisher handlers still control the outcome; document/receipt
      // checks, rather than the attribute, establish continued ownership.
      // A `_blank` explicit PDF link is still a download, not a new context:
      // the download attribute makes the browser save the file in place
      // (measured 2026-09-23 on pubs.acs.org, whose "Open PDF" anchor is a
      // same-origin .pdf path with target="_blank"; the model chose it and
      // the refusal parked an entitled paper). Any other target is a
      // navigation into a context this attempt does not own.
      if (target !== "_self" && !(target === "_blank" && explicitPDFLink(element, anchor))) return false;
      if (!anchor.hasAttribute("download") && !explicitPDFLink(element, anchor) &&
        (url.pathname !== entry.pathname || url.search !== location.search) && navigationTarget(element) === undefined) return false;
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
    const notices = Array.from(document.querySelectorAll('dialog[open],[role="dialog"],[role="alertdialog"],[aria-modal="true"]')).filter(cookieNotice);
    const candidates = Array.from(document.querySelectorAll(selector)).filter(element =>
      element.namespaceURI === "http://www.w3.org/1999/xhtml" && !sensitiveForm(element) && element.closest(scope) && !element.closest(privateSelector) && visible(element) &&
      (!element.matches(fields) || element.matches(native)) && (element.matches(native) || !element.querySelector(selector)) &&
      !notices.some(notice => notice.contains(element)) &&
      !/\b(my account|sign[ -]?(?:in|out)|log[ -]?(?:in|out)|profile)\b/i.test(label(element)))
      .map(element => ({ element, ownLabel: label(element) }))
      .filter(({ ownLabel }) => hasPublicLabel(ownLabel));
    const targets = new Map<string, Element>();
    const fingerprints: unknown[] = [];
    const controls = candidates.map(({ element, ownLabel }) => {
      let id = state.ids.get(element);
      if (!id) { id = `c${++state.next}`; state.ids.set(element, id); }
      targets.set(id, element);
      const form = (element as HTMLButtonElement).form || element.closest("form");
      const ancestors = [];
      for (let node: Element | null = element; node; node = node.parentElement) ancestors.push(Array.from(node.attributes).map(a => [a.name, a.value]));
      fingerprints.push([id, element.outerHTML, element.closest<HTMLAnchorElement>("a[href]")?.href, ancestors, form?.outerHTML,
        form ? Array.from(form.elements).map(node => { const field = node as HTMLInputElement & HTMLSelectElement; return [field.outerHTML, field.value, field.checked, field.selectedIndex]; }) : []]);
      const section = element.closest(`section,aside,[role="region"],${scope}`);
      // A nested menu's heading does not label every control in its article.
      const heading = section && Array.from(section.children).find(node => node.matches("h1,h2,h3,h4,h5,h6,[role='heading']") && visible(node) && !node.closest(privateSelector));
      const context = safe(section?.getAttribute("aria-label") || (heading ? publicText(heading) : ""), 100);
      return { priority: priority(ownLabel), control: { id, role: role(element), label: safe(`${ownLabel}${context ? ` [${context}]` : ""}`), disabled: disabled(element) || !allowed(element) } };
    }).sort((a, b) => Number(a.control.disabled) - Number(b.control.disabled) || a.priority - b.priority).map(({ control }) => control);
    const metadata = Array.from(document.querySelectorAll("meta[name]"));
    const title = ["citation_title", "dc.title", "prism.title"].flatMap(name => metadata
      .filter(node => node.getAttribute("name")?.trim().toLowerCase() === name)
      .map(node => safe(node.getAttribute("content"), 400))).find(hasPublicLabel) ?? "";
    const projection = { doi, title, controls: controls.slice(0, 80) };
    const source = JSON.stringify([state.document, binding, location.href, document.baseURI, request.allowNavigation === true, projection, fingerprints]);
    return { status: "snapshot" as const, projection, source, targets, controls };
  };
  const menuProgress = (view: Extract<ReturnType<typeof snapshot>, { status: "snapshot" }>, enabled: Set<string>) =>
    view.projection.controls.some(control => !control.disabled && !enabled.has(control.id) && /\bpdf\b/i.test(label(view.targets.get(control.id)!)));
  const current = snapshot();
  if (current.status === "blocked") return current;
  if (request.method === "check_menu") {
    const pending = state.menuWait;
    if (!pending || request.document !== state.document || request.revision !== pending.revision || !state.consumed.has(pending.revision)) return { status: "stale", reason: "observation_changed" };
    if (location.href !== pending.url) return { status: "stale", reason: "page_binding_failed" };
    const ready = menuProgress(current, pending.enabled);
    if (ready && checkingNavigationMenu) {
      // The consumed answer stays consumed. Only a fresh observation may use
      // newly exposed controls in this same, still-bound source document.
      state.retired = false;
      delete state.menuWait;
    }
    return { status: "menu_checked", ready };
  }
  if (request.method === "act" || request.method === "prepare") {
    const previous = state.observed;
    const choice = request.choice ?? "";
    const target = current.targets.get(choice);
    if (!previous || previous.revision !== request.revision || state.consumed.has(previous.revision) || previous.source !== current.source || !target || previous.targets.get(choice) !== target || !target.isConnected || !current.projection.controls.some(c => c.id === choice && !c.disabled)) return { status: "stale", reason: "observation_changed" };
    const destination = navigationTarget(target);
    if (request.method === "prepare") {
      delete state.prepared;
      if (destination === undefined) return { status: "prepared", effect: "local" };
      state.prepared = { revision: previous.revision, choice, destination };
      return { status: "prepared", effect: "navigate", destination };
    }
    if (destination !== undefined && (request.document !== state.document || request.destination !== destination ||
      state.prepared?.revision !== previous.revision || state.prepared.choice !== choice || state.prepared.destination !== destination)) return { status: "stale", reason: "observation_changed" };
    if (destination === undefined && request.destination !== undefined) return { status: "stale", reason: "observation_changed" };
    if (request.actionDeadline !== undefined && (!Number.isSafeInteger(request.actionDeadline) || request.actionDeadline < 0)) return { status: "blocked", reason: "invalid_request" };
    if (request.actionDeadline !== undefined && Date.now() >= request.actionDeadline) return { status: "stale", reason: "observation_changed" };
    // No await between the complete freshness/gate check and dispatch. Consume
    // before the click even when dispatch throws or synchronously re-enters.
    delete state.observed;
    state.consumed.add(previous.revision);
    state.serial++;
    delete state.prepared;
    if (destination !== undefined) {
      // Retire before the native click: unload can destroy the injection's
      // response. Keep only the identity token for a source download receipt.
      state.retired = true;
      state.menuWait = { revision: previous.revision, url: location.href, navigation: true,
        enabled: new Set(current.controls.filter(control => !control.disabled).map(control => control.id)) };
      HTMLElement.prototype.click.call(target);
      return { status: "dispatched", downloadExpected: true };
    }
    const menu = target.hasAttribute("aria-haspopup") || target.hasAttribute("aria-controls") ||
      target.hasAttribute("aria-expanded") || /\b(options?|formats?|menu)\b/i.test(label(target));
    const anchor = target.closest<HTMLAnchorElement>("a[href]");
    const explicitPDF = anchor !== null && explicitPDFLink(target, anchor);
    const addDownload = explicitPDF && !anchor.hasAttribute("download");
    let downloadExpected = explicitPDF || (!menu && (/\b(download|pdf|save (?:article|full text))\b/i.test(label(target)) || target.closest("a[download]") !== null));
    const bareDownload = /^downloads?$/i.test(label(target)) && !explicitPDF && !anchor?.hasAttribute("download");
    const beforeURL = location.href;
    delete state.menuWait;
    if (bareDownload) state.menuWait = { revision: previous.revision, url: beforeURL,
      enabled: new Set(current.controls.filter(control => !control.disabled).map(control => control.id)) };
    // Click the original provider element once, preserving its URL, handlers,
    // target and referrer policy. No cloned link, URL replay or invented path.
    if (addDownload) anchor.setAttribute("download", "");
    try { HTMLElement.prototype.click.call(target); }
    finally {
      // Leave a publisher handler's own replacement value intact.
      if (addDownload && anchor.getAttribute("download") === "") anchor.removeAttribute("download");
    }
    if (bareDownload) {
      if (host.papioArticleAgent !== state || state.node !== document) return { status: "stale", reason: "document_changed" };
      if (location.href !== beforeURL) return { status: "stale", reason: "page_binding_failed" };
      const after = snapshot();
      if (after.status === "blocked") return after;
      // A download menu is demonstrated by new usable PDF evidence, regardless
      // of publisher markup. The pre-click baseline includes controls beyond
      // the cap; a new choice must also fit the next transmitted projection.
      // Explicit download intent and an unchanged page keep their full grace.
      if (menuProgress(after, state.menuWait!.enabled)) downloadExpected = false;
    }
    return { status: "dispatched", downloadExpected, ...(bareDownload && downloadExpected ? { menuPending: true as const } : {}) };
  }
  if (request.method !== "observe") return { status: "blocked", reason: "invalid_request" };
  const serial = ++state.serial;
  delete state.pdfObserved;
  delete state.observed;
  delete state.prepared;
  const revision = Array.from(new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(current.source)))).map(n => n.toString(16).padStart(2, "0")).join("");
  const fresh = snapshot();
  if (fresh.status === "blocked") return { status: "stale", reason: fresh.reason };
  if (state.serial !== serial || fresh.source !== current.source) return { status: "stale", reason: "observation_changed" };
  state.observed = { revision, source: fresh.source, targets: fresh.targets };
  return { status: "observed", document: state.document, observation: { revision, ...fresh.projection } };
}

/** Load state of the article-agent tab, never its content. */
export interface AgentPageReadiness {
  readyState: DocumentReadyState;
  /** Characters of document text, including inline script text. */
  textLength: number;
  /** Time since the document's load event ended, when the page exposes it. */
  loadedAgoMs?: number;
}

/** Self-contained isolated-world injection. Tells the worker whether the
 * document finished loading, how long ago, and whether it is a tiny shell
 * (a cookie check or bot interstitial) rather than an article. */
export function agentPageReadiness(): AgentPageReadiness {
  const navigation = performance.getEntriesByType?.("navigation")[0] as PerformanceNavigationTiming | undefined;
  const loadedAgoMs = navigation !== undefined && navigation.loadEventEnd > 0
    ? Math.max(0, Math.round(performance.now() - navigation.loadEventEnd)) : undefined;
  return {
    readyState: document.readyState,
    textLength: document.documentElement?.textContent?.length ?? 0,
    ...(loadedAgoMs !== undefined ? { loadedAgoMs } : {}),
  };
}
