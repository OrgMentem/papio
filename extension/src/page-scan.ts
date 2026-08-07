// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// ADR-0019 Decision 3: a local-only, top-frame, container-scoped detector.
// scanDocument is pure DOM/JS with no chrome.* dependency, so it is both
// directly unit-testable (call it with a happy-dom root) and directly
// injectable via chrome.scripting.executeScript — that boundary serializes
// only the function's own source, not this module's other exports, so every
// helper it needs lives NESTED inside its body rather than beside it (mirrors
// background.ts's capturePage/extractMetaURL "must stay self-contained"
// comments). `root` defaults to the live global `document` so injection needs
// no `args`; tests pass an explicit happy-dom root instead.

/** One deduplicated identifier found on the scanned page. `localId` is the
 * `local_id` a page_bulk_status_request/page_bulk_submit_request round-trip
 * correlates back to this row (extension/src/protocol.ts PageBulkIdentifier). */
export interface DetectedPaper {
  localId: string;
  detector: "generic-identifiers/1";
  identifier: { kind: "doi" | "pmid" | "arxiv"; value: string };
  /** Up to 240 normalized characters of the nearest citation-shaped
   * container's visible text (ADR-0019 Decision 3). Never acquisition
   * identity — display only. */
  label: string;
  occurrences: number;
}

/** scanDocument's return value. A plain JSON object, deliberately: the
 * result crosses chrome.scripting.executeScript's serialization boundary,
 * which preserves array ELEMENTS but silently drops expando properties —
 * an earlier DetectedPaper[] & {truncated} shape returned perfectly from
 * the page and arrived in the extension with `truncated` gone, failing the
 * background's shape check on every scan. Truncation is reported, never
 * silent (Decision 3). */
export type ScanResult = { papers: DetectedPaper[]; truncated: boolean };

/** Raw-candidate cap before the scan stops walking the DOM (Decision 3). */
export const PAGE_BULK_RAW_CANDIDATE_CAP = 200;

export function scanDocument(root: Document | Element = document): ScanResult {
  const MAX_RAW = 200;
  const MAX_LABEL_CHARS = 240;
  const SKIP_TAGS: Record<string, true> = { SCRIPT: true, STYLE: true, NOSCRIPT: true, TEMPLATE: true };
  const CONTAINER_SELECTOR = "li, tr, p, article, [class*='result']";
  // Decision 3's "nearest bounded citation-shaped container": comfortably
  // above the 240-char label cap (so a real citation row is never cut off
  // before visibleText() gets to it) but well short of a multi-citation
  // wrapper's concatenated text (see boundedAncestor below).
  const BOUNDED_CONTAINER_CHARS = 400;
  // Decision 3's "extension-injected nodes" exclusion. scanDocument has no
  // chrome.* dependency (file header) and so cannot compare against papio's
  // own extension id here; every chrome-extension:// or moz-extension://
  // reference is therefore treated as foreign, papio's own included. That is
  // safe today — papio injects no such URLs into any page — but is a known
  // residual gap, not full "different id" generality, should that change.
  const EXTENSION_URL_RE = /^(?:chrome|moz)-extension:\/\//i;
  // A DOI's own syntax (10.<registrant>/<suffix>) is self-labeling; arXiv and
  // PMID values are short and ambiguous, so only an explicit label counts
  // (Decision 3: an unlabeled bare integer is never a PMID).
  const DOI_TEXT_RE = /10\.\d{4,}\/\S+/g;
  const ARXIV_LABELED_RE = /\barXiv\s*:\s*([a-zA-Z0-9./-]+)/gi;
  const PMID_LABELED_RE = /\bPMID\s*:\s*(\d+)/gi;
  const STRICT_DOI_RE = /^10\.\d{4,}\/\S+$/;

  function trimTrailingPunct(value: string): string {
    return value.replace(/[).,;:'"\]}]+$/, "").replace(/\/+$/, "");
  }

  function decodeSafe(value: string): string {
    try {
      return decodeURIComponent(value);
    } catch {
      return value;
    }
  }

  type Identifier = { kind: "doi" | "pmid" | "arxiv"; value: string };

  /** Recognized-link recognition order (Decision 3): doi.org, publisher
   * /doi/10.x paths, arXiv /abs|/pdf, PubMed article URLs. */
  function identifierFromURL(raw: string, base: string | undefined): Identifier | null {
    let url: URL;
    try {
      url = new URL(raw, base);
    } catch {
      return null;
    }
    const host = url.hostname.toLowerCase();
    const path = url.pathname;
    if (host === "doi.org" || host === "dx.doi.org") {
      const doi = trimTrailingPunct(decodeSafe(path.replace(/^\//, "")));
      return STRICT_DOI_RE.test(doi) ? { kind: "doi", value: doi } : null;
    }
    const doiPath = /\/doi\/(?:abs\/|full\/|e?pdf\/|full-xml\/)?(10\.\d{4,}\/[^?#]+)/i.exec(path);
    if (doiPath?.[1]) {
      const doi = trimTrailingPunct(decodeSafe(doiPath[1]));
      if (STRICT_DOI_RE.test(doi)) return { kind: "doi", value: doi };
    }
    // Generic fallback: a DOI-shaped run starting at any path segment —
    // Springer's /article/10.1007/…, /chapter/10.1007/…, IET's
    // /content/10.1049/… — with a trailing .pdf trimmed. Scholar results
    // link straight to publisher pages, so every shape matched here is a
    // paper the sheet would otherwise silently omit. STRICT_DOI_RE still
    // gates, and a rare non-DOI lookalike fails closed daemon-side at the
    // DOI-registration check rather than acquiring anything wrong.
    const doiSegment = /\/(10\.\d{4,}\/[^?#]+?)(?:\.pdf)?\/?$/i.exec(path);
    if (doiSegment?.[1]) {
      const doi = trimTrailingPunct(decodeSafe(doiSegment[1]));
      if (STRICT_DOI_RE.test(doi)) return { kind: "doi", value: doi };
    }
    if (host === "arxiv.org" || host.endsWith(".arxiv.org")) {
      const m = /^\/(?:abs|pdf)\/(.+?)(?:\.pdf)?\/?$/i.exec(path);
      if (m?.[1]) return { kind: "arxiv", value: trimTrailingPunct(decodeSafe(m[1])) };
    }
    if (host === "pubmed.ncbi.nlm.nih.gov") {
      const m = /^\/(\d+)\/?$/.exec(path);
      if (m?.[1]) return { kind: "pmid", value: m[1] };
    }
    return null;
  }

  // nodeType !== 9 means root is an Element (not a Document), and every
  // Element attached to a parsed document has a non-null ownerDocument —
  // a well-known DOM invariant, not a fabricated shape.
  const doc: Document = root.nodeType === 9 ? (root as Document) : (root as Element).ownerDocument as Document;
  const pageBase: string | undefined = doc.baseURI || doc.URL || undefined;

  function isHiddenSelf(el: Element): boolean {
    if (el.hasAttribute("hidden")) return true;
    if (el.getAttribute("aria-hidden") === "true") return true;
    const style = el.getAttribute("style") ?? "";
    return /display\s*:\s*none/i.test(style) || /visibility\s*:\s*hidden/i.test(style);
  }

  // Decision 3: "script, style, hidden, and extension-injected nodes" are
  // never scanned. An element carrying its own src/href pointing at a
  // chrome-extension://.../moz-extension://... URL is assumed injected by
  // something other than the page itself (see EXTENSION_URL_RE above); this
  // is checked on entry to a subtree, so nothing beneath it is walked either.
  // Shadow DOM: walk()/visibleText() only ever recurse through childNodes,
  // which never surfaces a shadow root's content (open or closed) — an
  // element with an attached shadow root is walked for its own light-DOM
  // children only, so nothing another extension (or the page itself) mounts
  // inside a shadow root is ever scanned. papio attaches no shadow roots of
  // its own into pages today, so this exclusion costs no false negatives yet.
  function isExtensionInjected(el: Element): boolean {
    const src = el.getAttribute("src");
    const href = el.getAttribute("href");
    return EXTENSION_URL_RE.test(src ?? "") || EXTENSION_URL_RE.test(href ?? "");
  }

  // textContent would also pick up script/style bodies and hidden nodes, so
  // the visible label is built with the same skip rules as the main walk
  // rather than delegated to el.textContent.
  function visibleText(el: Element): string {
    let out = "";
    const stack: Node[] = [el];
    while (stack.length > 0 && out.length < MAX_LABEL_CHARS * 2) {
      const node = stack.shift() as Node;
      if (node.nodeType === 3) {
        out += (node as Text).data;
        continue;
      }
      if (node.nodeType !== 1) continue;
      const child = node as Element;
      if (SKIP_TAGS[child.tagName] === true || isHiddenSelf(child) || isExtensionInjected(child)) continue;
      // A single space at every element boundary: textContent-style
      // concatenation welds adjacent anchors and spans into artifacts like
      // "ScholarView" or "1AinaC." on real reference lists; whitespace
      // collapsing later removes any doubles.
      out += " ";
      const kids = Array.from(child.childNodes);
      stack.splice(0, 0, ...kids, child.ownerDocument.createTextNode(" "));
    }
    return out;
  }

  /** Publisher reference lists append per-citation service links —
   * "CrossRef", "Google Scholar", "PubMed Abstract", "View reference in
   * article", Springer's bare "Article" — inside the same container as the
   * citation text, so they survive any structural container choice. They
   * are UI chrome, not citation content: strip a trailing run of them from
   * the DISPLAY label only. Weak tokens ("article", "pdf", "full text")
   * also appear in genuine titles, so a trailing run is stripped only when
   * it contains at least one unambiguous service token. Identifiers and
   * snapshots are never affected. */
  const SERVICE_RUN =
    /(?:\s*(?:crossref|google scholar|pubmed(?:\s+abstract)?|scopus|web of science|view (?:reference )?in article|full\s?text|article|pdf)[.,]?)+$/i;
  const STRONG_SERVICE =
    /crossref|google scholar|pubmed|scopus|web of science|view (?:reference )?in article/i;

  function stripServiceChrome(label: string): string {
    const match = label.match(SERVICE_RUN);
    if (match === null || !STRONG_SERVICE.test(match[0])) return label;
    const stripped = label.slice(0, label.length - match[0].length).trim();
    // Never strip a label down to nothing: a row whose entire text was
    // chrome keeps its original text rather than rendering blank.
    return stripped.length >= 8 ? stripped : label;
  }

  /** Climb from the identifier's own element through ancestors, keeping the
   * largest one whose visible text is still "bounded" (≤ BOUNDED_CONTAINER_
   * CHARS) — that is the nearest citation row, not a page-level wrapper.
   * closest(CONTAINER_SELECTOR) alone degrades badly when a citation list is
   * markup as bare rows (e.g. plain <div>s) inside one wrapper whose OWN
   * class happens to match the selector (e.g. [class*='result']): closest()
   * walks straight past every row and returns the wrapper, so every
   * identifier on the page gets the same near-identical wrapper-wide label.
   * BODY/HTML are never candidates — they are definitionally page-level, not
   * citation-shaped, however short a test fixture's body happens to be. A
   * container two identifiers both climb to is only ever reused when they
   * genuinely share it — each call climbs independently from its own start
   * element, never from a cache keyed by anything coarser. Returns null when
   * even `start` itself already exceeds the bound (rare — containerLabel
   * falls back to the CONTAINER_SELECTOR match). */
  function boundedAncestor(start: Element): Element | null {
    let candidate: Element | null = null;
    let el: Element | null = start;
    while (el !== null && el.tagName !== "BODY" && el.tagName !== "HTML") {
      if (visibleText(el).length > BOUNDED_CONTAINER_CHARS) break;
      candidate = el;
      // A <dt> is definitionally one term of a definition list — one
      // citation row. Climbing past it can only reach the whole <dl>
      // (every row on the page) or beyond; stop here and let
      // containerLabel pair it with its <dd> description.
      if (el.tagName === "DT") break;
      el = el.parentElement;
    }
    return candidate;
  }

  function containerLabel(start: Element): string {
    let container =
      boundedAncestor(start) ??
      (typeof start.closest === "function" ? start.closest(CONTAINER_SELECTOR) : null) ??
      start;
    // Definition lists split one citation across siblings by standardized
    // semantics: the <dt> carries the term (identifier + format links on
    // arXiv listings — pure chrome), the <dd> carries the description
    // (title, authors, subjects). Ancestor-climbing can never reach a
    // sibling, so a dt-anchored identifier labels from its dd instead.
    if (container.tagName === "DT") {
      const description = container.nextElementSibling;
      if (description !== null && description.tagName === "DD") {
        const text = visibleText(description).replace(/\s+/g, " ").trim();
        if (text.length >= 8) container = description;
      }
    }
    const raw = visibleText(container).replace(/\s+/g, " ").trim();
    const label = stripServiceChrome(raw);
    if (!isLowInformationLabel(label)) return label.slice(0, MAX_LABEL_CHARS);
    // Card layouts (Semantic Scholar result cards, and friends) put the
    // identifier link in a small action-button row — "Publisher · Save ·
    // Cite" — while the title lives in a sibling subtree of a card too
    // large for the bounded climb. The card is exactly the first ancestor
    // the climb refused, and cards title themselves with a heading: rescue
    // that heading rather than labeling a row of buttons.
    let card: Element | null = container.parentElement;
    while (card !== null && card.tagName !== "BODY" && card.tagName !== "HTML") {
      if (visibleText(card).length > BOUNDED_CONTAINER_CHARS) break;
      card = card.parentElement;
    }
    if (card !== null && card.tagName !== "BODY" && card.tagName !== "HTML") {
      const heading = card.querySelector("h1, h2, h3, h4, h5, h6, [role='heading']");
      if (heading !== null && !isHiddenSelf(heading) && !isExtensionInjected(heading)) {
        const title = visibleText(heading).replace(/\s+/g, " ").trim();
        if (title.length >= 8) return title.slice(0, MAX_LABEL_CHARS);
      }
    }
    return label.slice(0, MAX_LABEL_CHARS);
  }

  /** A label that is nothing but action chrome — "9 PDF (opens in a new
   * tab) Springer Nature Save Cite" — identifies no paper. Low information
   * = what remains after removing bracketed asides, standalone numbers,
   * and common action words is shorter than a minimal title. */
  function isLowInformationLabel(label: string): boolean {
    const residue = label
      .replace(/\((?:opens in a new tab|new tab|external link)\)/gi, " ")
      .replace(/\b(?:publisher|pdf|html|save|cite|expand|collapse|share|springer nature|elsevier|wiley|open access)\b/gi, " ")
      .replace(/[\d\s[\](),.·|:;-]+/g, " ")
      .trim();
    return residue.length < 12;
  }

  interface MergedEntry {
    kind: "doi" | "pmid" | "arxiv";
    value: string;
    label: string;
    occurrences: number;
  }

  const merged = new Map<string, MergedEntry>();
  let raw = 0;
  let truncated = false;

  function addOccurrence(identifier: Identifier, startEl: Element): void {
    if (raw >= MAX_RAW) {
      truncated = true;
      return;
    }
    raw += 1;
    const key = `${identifier.kind}:${identifier.value.toLowerCase()}`;
    let entry = merged.get(key);
    if (entry === undefined) {
      entry = { kind: identifier.kind, value: identifier.value, label: containerLabel(startEl), occurrences: 0 };
      merged.set(key, entry);
    }
    entry.occurrences += 1;
  }

  function scanText(text: string, startEl: Element | null): void {
    if (startEl === null) return;
    if (raw >= MAX_RAW) {
      truncated = true;
      return;
    }
    for (const m of text.matchAll(DOI_TEXT_RE)) {
      if (raw >= MAX_RAW) {
        truncated = true;
        return;
      }
      const value = trimTrailingPunct(m[0]);
      if (STRICT_DOI_RE.test(value)) addOccurrence({ kind: "doi", value }, startEl);
    }
    for (const m of text.matchAll(ARXIV_LABELED_RE)) {
      if (raw >= MAX_RAW) {
        truncated = true;
        return;
      }
      const value = trimTrailingPunct(m[1] ?? "");
      if (value !== "") addOccurrence({ kind: "arxiv", value }, startEl);
    }
    for (const m of text.matchAll(PMID_LABELED_RE)) {
      if (raw >= MAX_RAW) {
        truncated = true;
        return;
      }
      const value = m[1] ?? "";
      if (value !== "") addOccurrence({ kind: "pmid", value }, startEl);
    }
  }

  function walk(node: Node): void {
    if (raw >= MAX_RAW) {
      truncated = true;
      return;
    }
    if (node.nodeType === 1) {
      const el = node as Element;
      if (SKIP_TAGS[el.tagName] === true || isHiddenSelf(el) || isExtensionInjected(el)) return;
      if (el.tagName === "A") {
        const href = el.getAttribute("href");
        if (href !== null && href.trim() !== "") {
          const found = identifierFromURL(href, pageBase);
          if (found !== null) {
            addOccurrence(found, el);
            // The link's own text almost always repeats the same identifier
            // (e.g. an anchor whose visible text is the DOI itself); do not
            // double-count it as a second, unlabeled occurrence.
            return;
          }
        }
      }
      for (const child of Array.from(el.childNodes)) walk(child);
      return;
    }
    if (node.nodeType === 3) {
      const text = (node as Text).data;
      if (text.trim() !== "") scanText(text, node.parentElement);
    }
  }

  // The page's own canonical identifier (Decision 3) is excluded from the
  // bulk list — the popup's existing single-page Acquire action already
  // covers the page being read.
  function ownIdentifier(): Identifier | null {
    const citationDOI = doc.querySelector('meta[name="citation_doi"]')?.getAttribute("content")?.trim();
    if (citationDOI) {
      const value = trimTrailingPunct(citationDOI);
      if (STRICT_DOI_RE.test(value)) return { kind: "doi", value };
    }
    const canonical = doc.querySelector('link[rel="canonical"]')?.getAttribute("href")?.trim();
    const ogURL = doc.querySelector('meta[property="og:url"]')?.getAttribute("content")?.trim();
    for (const candidate of [canonical, ogURL]) {
      if (!candidate) continue;
      const found = identifierFromURL(candidate, pageBase);
      if (found !== null) return found;
    }
    return null;
  }

  const startNode: Node = root.nodeType === 9 ? ((doc.body as Element | null) ?? doc.documentElement) : root;
  if (startNode) walk(startNode);

  const own = ownIdentifier();
  if (own !== null) merged.delete(`${own.kind}:${own.value.toLowerCase()}`);

  const papers: DetectedPaper[] = [];
  let index = 0;
  for (const entry of merged.values()) {
    index += 1;
    papers.push({
      localId: `id-${index}`,
      detector: "generic-identifiers/1",
      identifier: { kind: entry.kind, value: entry.value },
      label: entry.label,
      occurrences: entry.occurrences,
    });
  }
	return { papers, truncated };
}

// ---------------------------------------------------------------------------
// Ephemeral scan snapshot shape (ADR-0019 Decision 4): lives in
// chrome.storage.session only — never chrome.storage.local/sync, never the
// daemon. Shared between background.ts (writer) and page-bulk.ts (reader) so
// neither hand-rolls a second copy of the shape.
// ---------------------------------------------------------------------------

export interface PageBulkSnapshot {
  scanId: string;
  sourceTabId: number;
  /** Bare scheme+host only, matching page_bulk_submit_request.source.origin
   * (ADR-0019 Decision 6) — never path, query, fragment, or page title. */
  sourceOrigin: string;
  /** Increments each time this scanId is rescanned, so a workspace can
   * discard an in-flight reply that a newer rescan has already superseded. */
  documentGeneration: number;
  items: DetectedPaper[];
  truncated: boolean;
}

export interface PageBulkScanStore {
  /** Oldest-first scanId insertion order, bounded to PAGE_BULK_SNAPSHOT_LIMIT
   * entries so an abandoned tab's snapshots cannot grow storage.session
   * without bound. */
  order: string[];
  byId: Record<string, PageBulkSnapshot>;
}

export const PAGE_BULK_SCAN_STORAGE_KEY = "papio_page_bulk_scans_v1";
export const PAGE_BULK_SNAPSHOT_LIMIT = 4;

export function emptyPageBulkScanStore(): PageBulkScanStore {
  return { order: [], byId: {} };
}

/** Insert or replace one snapshot, evicting the oldest beyond the bound. A
 * rescan reuses its scanId, so it never consumes a second slot. */
export function withPageBulkSnapshot(store: PageBulkScanStore, snapshot: PageBulkSnapshot): PageBulkScanStore {
  const order = store.order.filter((id) => id !== snapshot.scanId);
  order.push(snapshot.scanId);
  const byId: Record<string, PageBulkSnapshot> = { ...store.byId, [snapshot.scanId]: snapshot };
  while (order.length > PAGE_BULK_SNAPSHOT_LIMIT) {
    const evicted = order.shift();
    if (evicted !== undefined) delete byId[evicted];
  }
  return { order, byId };
}
