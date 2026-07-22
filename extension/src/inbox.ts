// Copyright 2026 OrgMentem. Licensed under MIT.

import type { TriageCounts, TriageSnapshotItem, TriageSnapshotResponsePayload } from "./protocol";

type Snapshot = Omit<TriageSnapshotResponsePayload, "request_id">;
type TriageOperation = TriageSnapshotItem["ops"][number];
type Verdict = "accept" | "reject" | "dismiss";

type CitationStyle = "apa" | "mla" | "chicago";

const CITATION_STYLE_KEY = "papio_inbox_citation_style_v1";

function storedCitationStyle(): CitationStyle {
  try {
    const value = window.localStorage.getItem(CITATION_STYLE_KEY);
    if (value === "apa" || value === "mla" || value === "chicago") return value;
  } catch {
    // Storage can be unavailable; fall back to the default.
  }
  return "apa";
}

function persistCitationStyle(style: CitationStyle): void {
  try {
    window.localStorage.setItem(CITATION_STYLE_KEY, style);
  } catch {
    // Non-fatal: the choice simply resets on the next visit.
  }
}

interface PageElements {
  connection: HTMLElement;
  counts: HTMLElement;
  filterInput: HTMLInputElement;
  refresh: HTMLButtonElement;
  reconnect: HTMLButtonElement;
  list: HTMLElement;
  operationStatus: HTMLElement;
  generatedAt: HTMLTimeElement;
  loadMore: HTMLButtonElement;
  dialog: HTMLElement;
  dialogMessage: HTMLElement;
  dialogCancel: HTMLButtonElement;
  dialogConfirm: HTMLButtonElement;
  citationStyle: HTMLSelectElement;
}

interface Confirmation {
  itemID: string;
  verdict: Verdict;
  returnFocus: HTMLElement | null;
}

interface PageState {
  snapshot: Snapshot | null;
  counts: TriageCounts | null;
  generatedAt: string | null;
  connected: boolean;
  connectionMessage: string;
  selectedID: string | null;
  pending: Set<string>;
  previewed: Set<string>;
  itemMessages: Map<string, { text: string; tone: "info" | "error" | "offline" }>;
  confirmation: Confirmation | null;
  focusSelectionAfterRender: boolean;
  loading: boolean;
  filterQuery: string;
  citationStyle: CitationStyle;
}

const state: PageState = {
  snapshot: null,
  counts: null,
  generatedAt: null,
  connected: false,
  connectionMessage: "Connecting to daemon…",
  selectedID: null,
  pending: new Set(),
  previewed: new Set(),
  itemMessages: new Map(),
  confirmation: null,
  focusSelectionAfterRender: false,
  loading: false,
  filterQuery: "",
  citationStyle: storedCitationStyle(),
};

let elements: PageElements | null = null;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function errorFromResponse(value: unknown): string {
  if (isRecord(value) && isRecord(value["error"]) && typeof value["error"]["message"] === "string") {
    return value["error"]["message"];
  }
  return "The daemon did not return a usable response.";
}

function responseValue<T>(value: unknown, key: string): { ok: true; value: T } | { ok: false; message: string } {
  if (isRecord(value) && value["ok"] === true && key in value) {
    return { ok: true, value: value[key] as T };
  }
  return { ok: false, message: errorFromResponse(value) };
}

async function runtimeMessage(type: string, request: Record<string, unknown>): Promise<unknown> {
  if (typeof chrome === "undefined" || !chrome.runtime?.sendMessage) {
    throw new Error("The extension runtime is unavailable.");
  }
  return chrome.runtime.sendMessage({ type, request });
}

// A disconnect is usually the daemon's own port healing (extension reload,
// SW nap, brief restart) — the background worker already reconnects with its
// own backoff. Mirror that here so the banner clears itself instead of
// leaving the user staring at a stale error until they click Reconnect.
let reconnectToken = 0;
let reconnectScheduled = false;
let reconnectAttempts = 0;
const RECONNECT_DELAYS_MS = [1000, 2000, 4000, 8000, 15000];

function cancelAutoReconnect(): void {
  reconnectToken += 1;
  reconnectScheduled = false;
  reconnectAttempts = 0;
}

function scheduleAutoReconnect(): void {
  if (reconnectScheduled) return;
  reconnectScheduled = true;
  const delay = RECONNECT_DELAYS_MS[Math.min(reconnectAttempts, RECONNECT_DELAYS_MS.length - 1)];
  reconnectAttempts += 1;
  const token = reconnectToken;
  setTimeout(() => {
    reconnectScheduled = false;
    if (token !== reconnectToken) return;
    void refreshInbox();
  }, delay);
}

// Errors caused by connectivity loss (tone "offline") describe a condition
// that resolves itself once reconnected; a stale "daemon disconnected"
// message left on a row after the daemon is back would be misleading.
// Errors from the daemon actually rejecting the request (tone "error")
// persist until the item changes.
function clearOfflineItemMessages(): void {
  for (const [id, entry] of state.itemMessages) {
    if (entry.tone === "offline") state.itemMessages.delete(id);
  }
}

function setConnection(connected: boolean, message: string): void {
  const wasConnected = state.connected;
  state.connected = connected;
  state.connectionMessage = message;
  if (connected) {
    cancelAutoReconnect();
    if (!wasConnected) clearOfflineItemMessages();
  } else {
    scheduleAutoReconnect();
  }
}

function itemForID(id: string): TriageSnapshotItem | null {
  return state.snapshot?.items.find((item) => item.id === id) ?? null;
}

function matchesFilter(item: TriageSnapshotItem, query: string): boolean {
  if (query === "") return true;
  const haystack = [item.title, ...item.facts.map((fact) => fact.text)].join(" \u0000 ").toLowerCase();
  return haystack.includes(query);
}

function orderedItems(): TriageSnapshotItem[] {
  if (state.snapshot === null) return [];
  const classRank: Record<TriageSnapshotItem["kind"], number> = {
    retraction: 0,
    human_action: 1,
    watch_hit: 2,
  };
  const query = state.filterQuery.trim().toLowerCase();
  return [...state.snapshot.items]
    .filter((item) => matchesFilter(item, query))
    .sort((left, right) => classRank[left.kind] - classRank[right.kind] || left.rank - right.rank);
}

function safeExternalURL(value: string): string | null {
  try {
    const url = new URL(value);
    return url.protocol === "https:" ? url.href : null;
  } catch {
    return null;
  }
}

function safePreviewURL(value: string): string | null {
  try {
    const url = new URL(value);
    if (
      url.protocol !== "http:" ||
      url.hostname !== "127.0.0.1" ||
      url.port === "" ||
      !url.pathname.startsWith("/p/") ||
      url.search !== "" ||
      url.hash !== "" ||
      url.username !== "" ||
      url.password !== ""
    ) {
      return null;
    }
    return url.href;
  } catch {
    return null;
  }
}

function firstSafeLink(item: TriageSnapshotItem): string | null {
  for (const link of item.links) {
    const url = safeExternalURL(link.url);
    if (url !== null) return url;
  }
  return null;
}

function openNewTab(url: string): void {
  window.open(url, "_blank", "noopener,noreferrer");
}

function previewToken(item: TriageSnapshotItem): string | null {
  if (item.kind !== "human_action" || item.action_kind !== "verify_identity") return null;
  if (typeof item.revision !== "number" || typeof item.sha256 !== "string" || item.sha256.length === 0) return null;
  return `${item.id}:${item.revision}:${item.sha256}`;
}

function hasViewedPreview(item: TriageSnapshotItem): boolean {
  const token = previewToken(item);
  return token !== null && state.previewed.has(token);
}

function hasOperation(item: TriageSnapshotItem, operation: TriageOperation): boolean {
  return item.ops.includes(operation);
}

function isMutation(operation: TriageOperation): boolean {
  return operation === "acquire" || operation === "dismiss" || operation === "accept" || operation === "reject";
}

function operationLabel(operation: TriageOperation): string {
  switch (operation) {
    case "acquire":
      return "Acquire";
    case "dismiss":
      return "Dismiss";
    case "accept":
      return "Accept";
    case "reject":
      return "Reject";
    case "open":
      return "Open";
    case "retry":
      return "Retry";
  }
}

function element<K extends keyof HTMLElementTagNameMap>(tag: K, text?: string): HTMLElementTagNameMap[K] {
  const created = document.createElement(tag);
  if (text !== undefined) created.textContent = text;
  return created;
}

function operationMessage(itemID: string, text: string, tone: "info" | "error" | "offline" = "info"): void {
  state.itemMessages.set(itemID, { text, tone });
  if (elements !== null) elements.operationStatus.textContent = text;
}

function announce(message: string): void {
  if (elements !== null) elements.operationStatus.textContent = message;
}

function rowForItem(itemID: string): HTMLElement | null {
  if (elements === null) return null;
  return Array.from(elements.list.querySelectorAll<HTMLElement>("[data-triage-item-id]"))
    .find((row) => row.dataset.triageItemId === itemID) ?? null;
}

function updateRovingTabIndex(): void {
  if (elements === null) return;
  for (const row of Array.from(elements.list.querySelectorAll<HTMLElement>("[data-triage-item-id]"))) {
    row.tabIndex = row.dataset.triageItemId === state.selectedID ? 0 : -1;
  }
}

function selectItem(itemID: string, focus: boolean): void {
  if (itemForID(itemID) === null) return;
  state.selectedID = itemID;
  updateRovingTabIndex();
  if (focus) rowForItem(itemID)?.focus();
}

const LINK_LABELS: Record<string, string> = { arxiv: "arXiv", openalex: "OpenAlex", landing: "landing page" };

function factText(item: TriageSnapshotItem, label: string): string | null {
  const fact = item.facts.find((candidate) => candidate.label === label);
  return fact === undefined || fact.text === "" ? null : fact.text;
}

interface AuthorName {
  family: string;
  givens: string[];
}

function parseAuthor(name: string): AuthorName {
  const words = name.split(/\s+/).filter((word) => word !== "" && !word.startsWith("("));
  const family = words[words.length - 1];
  if (family === undefined) return { family: name, givens: [] };
  return { family, givens: words.slice(0, -1) };
}

function invertedInitials(name: string): string {
  const { family, givens } = parseAuthor(name);
  const initials = givens.map((given) => `${given.charAt(0).toUpperCase()}.`).join(" ");
  return initials === "" ? family : `${family}, ${initials}`;
}

function invertedFull(name: string): string {
  const { family, givens } = parseAuthor(name);
  return givens.length === 0 ? family : `${family}, ${givens.join(" ")}`;
}

function apaAuthors(authors: string[]): string {
  const names = authors.map(invertedInitials);
  if (names.length === 1) return names[0]!;
  if (names.length <= 7) return `${names.slice(0, -1).join(", ")}, & ${names[names.length - 1]!}`;
  return `${names.slice(0, 6).join(", ")}, … ${names[names.length - 1]!}`;
}

function mlaAuthors(authors: string[]): string {
  const first = invertedFull(authors[0]!);
  if (authors.length === 1) return first;
  if (authors.length === 2) return `${first}, and ${authors[1]!}`;
  return `${first}, et al.`;
}

function chicagoAuthors(authors: string[]): string {
  const first = invertedFull(authors[0]!);
  if (authors.length === 1) return first;
  if (authors.length > 7) return `${first}, ${authors.slice(1, 7).join(", ")}, et al.`;
  const rest = authors.slice(1);
  const last = rest.pop()!;
  return rest.length === 0 ? `${first}, and ${last}` : `${first}, ${rest.join(", ")}, and ${last}`;
}

function sentence(text: string): string {
  return text.endsWith(".") ? text : `${text}.`;
}

function citationAnchor(url: string, text: string): HTMLAnchorElement {
  const anchor = element("a", text);
  anchor.href = url;
  anchor.target = "_blank";
  anchor.rel = "noopener noreferrer";
  return anchor;
}

// One reference-style line per item: authors and year in the selected
// citation style, with the DOI hyperlinked as its own URL — the link IS the
// citation's locator, replacing a separate "Open DOI" row. Non-DOI links
// follow as short labeled anchors. A row whose displayed title is already
// the DOI (placeholder fallback) does not repeat that link here.
function renderCitation(item: TriageSnapshotItem, placeholderURL: string | null): HTMLElement | null {
  const authorsText = factText(item, "Authors");
  const year = factText(item, "Year");
  const safe: Array<{ rel: string; url: string }> = [];
  for (const link of item.links) {
    const url = safeExternalURL(link.url);
    if (url !== null) safe.push({ rel: link.rel, url });
  }
  const doi = safe.find((link) => link.rel === "doi");
  const extras = safe.filter((link) => link !== doi);
  if (authorsText === null && year === null && safe.length === 0) return null;

  const style = state.citationStyle;
  const authors = authorsText === null ? [] : authorsText.split(", ").filter((name) => name !== "");
  let prefix = "";
  if (authors.length > 0) {
    if (style === "apa") {
      const names = apaAuthors(authors);
      prefix = year === null ? sentence(names) : `${names} (${year}).`;
    } else if (style === "mla") {
      const names = sentence(mlaAuthors(authors));
      prefix = year === null ? names : doi === undefined ? `${names} ${year}.` : `${names} ${year},`;
    } else {
      const names = sentence(chicagoAuthors(authors));
      prefix = year === null ? names : `${names} ${year}.`;
    }
  } else if (year !== null) {
    prefix = style === "apa" ? `(${year}).` : `${year}.`;
  }

  const citation = element("p");
  citation.className = "item-citation";
  if (prefix !== "") citation.append(document.createTextNode(`${prefix} `));
  const doiShown = doi !== undefined && placeholderURL !== null && doi.url.replace(/^https:\/\//, "") === placeholderURL;
  if (doi !== undefined && !doiShown) {
    citation.append(citationAnchor(doi.url, style === "apa" ? doi.url : doi.url.replace(/^https:\/\//, "")));
    if (style !== "apa") citation.append(document.createTextNode("."));
  }
  for (const link of extras) {
    if (citation.childNodes.length > 0) citation.append(document.createTextNode(" · "));
    citation.append(citationAnchor(link.url, LINK_LABELS[link.rel] ?? link.rel));
  }
  return citation.childNodes.length > 0 ? citation : null;
}

function previewButton(item: TriageSnapshotItem): HTMLButtonElement | null {
  if (item.kind !== "human_action" || item.action_kind !== "verify_identity") return null;
  const button = element("button", "View PDF");
  button.type = "button";
  button.dataset.operation = "preview";
  button.disabled = state.pending.has(item.id) || !state.connected;
  button.addEventListener("click", () => {
    void requestPreview(item);
  });
  return button;
}

function operationButton(item: TriageSnapshotItem, operation: TriageOperation): HTMLButtonElement {
  const button = element("button", operationLabel(operation));
  button.type = "button";
  button.dataset.operation = operation;

  const needsPreview = operation === "accept" && item.action_kind === "verify_identity";
  const unavailable = operation === "retry" || (operation === "open" && firstSafeLink(item) === null);
  button.disabled =
    state.pending.has(item.id) ||
    unavailable ||
    (isMutation(operation) && !state.connected) ||
    (needsPreview && !hasViewedPreview(item));
  if (needsPreview && !hasViewedPreview(item)) button.title = "View the PDF before accepting it.";
  if (operation === "retry") button.title = "Retry is not available from this inbox version.";
  button.addEventListener("click", () => {
    activateOperation(item, operation);
  });
  return button;
}

// The daemon falls back to the action kind ("manual download") when a job
// has no bibliographic title. Prefer the first safe link (usually the DOI)
// as the display title, and mark either fallback as a placeholder so it does
// not masquerade as a paper title. Ingested titles sometimes arrive with the
// author list appended after " - "; that would duplicate the citation line,
// so a suffix matching the Authors fact is stripped.
function displayTitle(item: TriageSnapshotItem): { text: string; placeholder: boolean } {
  const kindLabel =
    item.kind === "human_action" && typeof item.action_kind === "string"
      ? item.action_kind.replaceAll("_", " ")
      : null;
  if (kindLabel === null || item.title !== kindLabel) {
    return { text: stripAuthorSuffix(item.title, factText(item, "Authors")), placeholder: false };
  }
  const url = firstSafeLink(item);
  if (url !== null) return { text: url.replace(/^https:\/\//, ""), placeholder: true };
  return { text: item.title, placeholder: true };
}

function stripAuthorSuffix(title: string, authors: string | null): string {
  if (authors === null) return title;
  const index = title.lastIndexOf(" - ");
  if (index <= 0) return title;
  const suffix = title.slice(index + 3).trim().toLowerCase();
  const known = authors.trim().toLowerCase();
  if (suffix.length < 8 || known.length < 8) return title;
  if (known.startsWith(suffix) || suffix.startsWith(known)) return title.slice(0, index).trimEnd();
  return title;
}

function isFilePath(token: string): boolean {
  return /^\/(?:[^/]+\/){2,}[^/]+$/.test(token);
}

// Absolute filesystem paths inside a fact (quarantine files) render as an
// ellipsized code span with the full path in the tooltip, so a long path
// cannot dominate the row. URLs keep their scheme and stay plain text.
function appendFactText(target: HTMLElement, text: string): void {
  const parts = text.split(/(\s+)/);
  if (!parts.some(isFilePath)) {
    target.textContent = text;
    return;
  }
  for (const part of parts) {
    if (isFilePath(part)) {
      const span = element("span", part);
      span.className = "file-path";
      span.title = part;
      target.append(span);
    } else if (part !== "") {
      target.append(document.createTextNode(part));
    }
  }
}

const KNOWN_FACT_LABELS: Record<string, true> = { Action: true, Authors: true, Year: true, Detail: true, Job: true };

const STATUS_META: Record<string, { glyph: string; label: string }> = {
  manual_download: { glyph: "↓", label: "Manual download needed" },
  openurl_handoff: { glyph: "↗", label: "Browser handoff ready" },
  verify_identity: { glyph: "?", label: "Identity verification needed" },
  watch_hit: { glyph: "✶", label: "New watch hit" },
  retraction: { glyph: "!", label: "Retraction notice" },
};

// The status glyph is the row's quick-reference column; its meaning rides in
// the tooltip and accessible name. The action-kind vocabulary is open (a new
// daemon can ship new kinds), so unknown kinds degrade to a neutral dot with
// the raw kind as the label instead of breaking the row.
function statusMeta(item: TriageSnapshotItem): { key: string; glyph: string; label: string } {
  const key = item.kind === "human_action" && typeof item.action_kind === "string" ? item.action_kind : item.kind;
  const meta = STATUS_META[key];
  if (meta !== undefined) return { key, glyph: meta.glyph, label: meta.label };
  return { key: "unknown", glyph: "•", label: key.replaceAll("_", " ") };
}

// Backend identifiers remain out of the ordinary triage flow. Their compact
// disclosure sits beside the action/status text and preserves native button
// keyboard semantics and state.
function renderDebug(item: TriageSnapshotItem): { toggle: HTMLButtonElement; list: HTMLDListElement } {
  const rows: Array<[string, string]> = [["item", item.id]];
  const job = factText(item, "Job");
  if (job !== null) rows.push(["job", job]);
  if (item.kind === "human_action" && typeof item.revision === "number") {
    rows.push(["revision", String(item.revision)]);
  }
  const list = element("dl");
  list.className = "item-debug";
  list.hidden = true;
  list.id = `backend-details-${item.id}`;
  for (const [label, value] of rows) {
    const field = element("div");
    field.className = "item-debug-field";
    const valueElement = element("dd", value);
    valueElement.title = value;
    field.append(element("dt", label), valueElement);
    list.append(field);
  }

  const toggle = element("button");
  const icon = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  icon.setAttribute("aria-hidden", "true");
  icon.setAttribute("viewBox", "0 0 16 16");
  const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
  path.setAttribute("d", "m4 6 4 4 4-4");
  icon.append(path);
  toggle.append(icon);
  toggle.className = "item-debug-toggle";
  toggle.type = "button";
  toggle.dataset.label = "Backend details";
  toggle.setAttribute("aria-controls", list.id);
  toggle.setAttribute("aria-expanded", "false");
  toggle.setAttribute("aria-label", "Backend details");
  toggle.addEventListener("click", () => {
    const expanded = toggle.getAttribute("aria-expanded") === "true";
    toggle.setAttribute("aria-expanded", String(!expanded));
    list.hidden = expanded;
  });
  return { toggle, list };
}

function renderItem(item: TriageSnapshotItem): HTMLElement {
  const card = element("article");
  card.className = "triage-item";
  card.dataset.triageItemId = item.id;
  card.tabIndex = item.id === state.selectedID ? 0 : -1;
  const title = displayTitle(item);
  card.setAttribute("aria-label", title.text);
  card.addEventListener("focusin", () => selectItem(item.id, false));
  card.addEventListener("click", () => selectItem(item.id, false));

  const status = statusMeta(item);
  const badge = element("span", status.glyph);
  badge.className = "item-status";
  badge.dataset.status = status.key;
  badge.dataset.label = status.label;
  badge.setAttribute("role", "img");
  badge.setAttribute("aria-label", status.label);
  card.append(badge);

  const body = element("div");
  body.className = "item-body";
  card.append(body);

  const headingText = element("h3", title.text);
  if (title.placeholder) headingText.classList.add("title-placeholder");
  const debug = renderDebug(item);
  body.append(headingText);

  const citation = renderCitation(item, title.placeholder ? title.text : null);
  if (citation !== null) body.append(citation);

  const leftovers = item.facts.filter((fact) => KNOWN_FACT_LABELS[fact.label] !== true);
  if (leftovers.length > 0) {
    const facts = element("dl");
    facts.className = "item-facts";
    for (const fact of leftovers) {
      const slug = fact.label.toLowerCase().replace(/[^a-z0-9]+/g, "-");
      const dt = element("dt", fact.label);
      dt.dataset.fact = slug;
      const dd = element("dd");
      dd.dataset.fact = slug;
      appendFactText(dd, fact.text);
      facts.append(dt, dd);
    }
    body.append(facts);
  }

  const detail = factText(item, "Detail");
  if (detail !== null) {
    const paragraph = element("p");
    paragraph.className = "item-detail";
    appendFactText(paragraph, detail);
    paragraph.append(debug.toggle);
    body.append(paragraph);
  } else if (citation !== null) {
    // Non-action items have no instruction line; keep the disclosure at the
    // end of their metadata rather than creating a standalone controls row.
    citation.append(debug.toggle);
  } else {
    headingText.append(debug.toggle);
  }
  body.append(debug.list);

  const entry = state.itemMessages.get(item.id);
  if (entry !== undefined) {
    const result = element("p", entry.text);
    result.className = "item-result";
    result.dataset.tone = entry.tone;
    result.setAttribute("role", "status");
    body.append(result);
  }

  const controls = element("div");
  controls.className = "item-controls";
  controls.setAttribute("aria-label", `Actions for ${title.text}`);
  const preview = previewButton(item);
  if (preview !== null) controls.append(preview);
  for (const operation of item.ops) controls.append(operationButton(item, operation));
  if (controls.childElementCount > 0) card.append(controls);

  return card;
}

function renderGroup(kind: TriageSnapshotItem["kind"], heading: string, items: TriageSnapshotItem[]): HTMLElement | null {
  if (items.length === 0) return null;
  const section = element("section");
  section.className = `triage-group triage-group-${kind}`;
  section.append(element("h2", `${heading} (${items.length})`));
  for (const item of items) section.append(renderItem(item));
  return section;
}

function renderCounts(): void {
  if (elements === null) return;
  const counts = state.counts ?? state.snapshot?.counts;
  if (counts === undefined || counts === null) {
    elements.counts.textContent = "Counts unavailable";
    return;
  }
  const plural = (count: number, singular: string): string =>
    `${count} ${count === 1 ? singular : `${singular}s`}`;
  const parts = [`${counts.pending_total} pending`];
  if (counts.retractions > 0) parts.push(plural(counts.retractions, "retraction"));
  if (counts.actions > 0) parts.push(plural(counts.actions, "human action"));
  if (counts.watch_hits > 0) parts.push(plural(counts.watch_hits, "watch hit"));
  elements.counts.textContent = parts.join(" · ");
}

function renderDialog(): void {
  if (elements === null) return;
  const confirmation = state.confirmation;
  if (confirmation === null) {
    elements.dialog.hidden = true;
    elements.dialogConfirm.disabled = false;
    return;
  }
  const item = itemForID(confirmation.itemID);
  if (item === null) {
    closeDialog(false);
    return;
  }
  if (confirmation.verdict === "accept" && item.action_kind === "verify_identity") {
    elements.dialogMessage.textContent = `Accept this PDF as ${item.title}? It leaves quarantine.`;
  } else if (confirmation.verdict === "reject") {
    elements.dialogMessage.textContent = `Reject ${item.title}? It cancels the job.`;
  } else if (confirmation.verdict === "dismiss") {
    elements.dialogMessage.textContent = `Dismiss ${item.title}? It cancels the job and closes this action.`;
  } else {
    elements.dialogMessage.textContent = `Accept ${item.title}?`;
  }
  elements.dialogConfirm.textContent = confirmation.verdict === "accept" ? "Accept" : confirmation.verdict === "reject" ? "Reject" : "Dismiss";
  elements.dialogConfirm.disabled = state.pending.has(item.id);
  elements.dialogCancel.disabled = state.pending.has(item.id);
  elements.dialog.hidden = false;
}

function render(): void {
  if (elements === null) return;
  const isDisconnected = !state.connected;
  elements.connection.textContent = isDisconnected
    ? `Disconnected: ${state.connectionMessage} Reconnecting automatically — run papio status if this persists.`
    : state.connectionMessage;
  elements.connection.dataset.state = isDisconnected ? "disconnected" : "connected";
  elements.reconnect.hidden = !isDisconnected;
  elements.refresh.disabled = state.loading;
  elements.reconnect.disabled = state.loading;
  renderCounts();

  elements.list.replaceChildren();
  const items = orderedItems();
  if (state.selectedID !== null && !items.some((item) => item.id === state.selectedID)) state.selectedID = items[0]?.id ?? null;
  if (state.selectedID === null && items.length > 0) state.selectedID = items[0]?.id ?? null;

  const retractions = items.filter((item) => item.kind === "retraction");
  const actions = items.filter((item) => item.kind === "human_action");
  const hits = items.filter((item) => item.kind === "watch_hit");
  const groups = [
    renderGroup("retraction", "Retractions", retractions),
    renderGroup("human_action", "Human actions", actions),
    renderGroup("watch_hit", "Watch hits", hits),
  ];
  for (const group of groups) if (group !== null) elements.list.append(group);

  if (state.snapshot === null) {
    elements.list.append(element("p", "No snapshot is available yet. Reconnect to retrieve the inbox."));
  } else if (state.snapshot.items.length === 0) {
    elements.list.append(element("p", "Your inbox is clear."));
  } else if (items.length === 0) {
    elements.list.append(element("p", `No items match "${state.filterQuery.trim()}".`));
  }
  if (state.snapshot?.unsupported_items_count && state.snapshot.unsupported_items_count > 0) {
    elements.list.append(element("p", `${state.snapshot.unsupported_items_count} newer item(s) need a newer extension.`));
  }

  elements.loadMore.hidden = state.snapshot?.has_more !== true;
  elements.loadMore.disabled = state.loading || !state.connected;
  if (state.generatedAt === null) {
    elements.generatedAt.textContent = "generated at —";
    elements.generatedAt.removeAttribute("datetime");
  } else {
    elements.generatedAt.dateTime = state.generatedAt;
    elements.generatedAt.textContent = `generated at ${state.generatedAt}`;
  }

  renderDialog();
  if (state.focusSelectionAfterRender) {
    state.focusSelectionAfterRender = false;
    if (state.selectedID !== null) rowForItem(state.selectedID)?.focus();
  }
}

function decrementCounts(item: TriageSnapshotItem): void {
  const counts = state.counts;
  if (counts === null) return;
  const decrease = (value: number): number => Math.max(0, value - 1);
  state.counts = {
    ...counts,
    pending_total: decrease(counts.pending_total),
    watch_hits: item.kind === "watch_hit" ? decrease(counts.watch_hits) : counts.watch_hits,
    actions: item.kind === "human_action" ? decrease(counts.actions) : counts.actions,
    retractions: item.kind === "retraction" ? decrease(counts.retractions) : counts.retractions,
  };
}

function removeItem(itemID: string): void {
  if (state.snapshot === null) return;
  const items = orderedItems();
  const index = items.findIndex((item) => item.id === itemID);
  const removed = items[index];
  if (removed === undefined) return;
  const remaining = state.snapshot.items.filter((item) => item.id !== itemID);
  state.snapshot = { ...state.snapshot, items: remaining };
  state.itemMessages.delete(itemID);
  decrementCounts(removed);
  if (state.selectedID === itemID) {
    const next = items[index + 1] ?? items[index - 1] ?? null;
    state.selectedID = next?.id ?? null;
    state.focusSelectionAfterRender = true;
  }
}

function resultForMutation(value: unknown): { ok: true; outcome: "applied" | "already_applied" | "conflict" | "error"; detail?: string } | { ok: false; message: string } {
  if (!isRecord(value) || value["ok"] !== true || typeof value["outcome"] !== "string") {
    return { ok: false, message: errorFromResponse(value) };
  }
  const outcome = value["outcome"];
  if (outcome !== "applied" && outcome !== "already_applied" && outcome !== "conflict" && outcome !== "error") {
    return { ok: false, message: "The daemon returned an unknown mutation result." };
  }
  const detail = typeof value["detail"] === "string" ? value["detail"] : undefined;
  return detail === undefined ? { ok: true, outcome } : { ok: true, outcome, detail };
}

async function refreshInbox(append = false): Promise<void> {
  const cursor = append ? state.snapshot?.cursor : undefined;
  if (append && (cursor === undefined || state.snapshot === null)) return;
  state.loading = true;
  render();
  const snapshotRequest: Record<string, unknown> = { schema_versions: [1] };
  if (cursor !== undefined) snapshotRequest["cursor"] = cursor;
  const snapshotPromise = runtimeMessage("papio.triage.snapshot", snapshotRequest)
    .then((response) => responseValue<Snapshot>(response, "snapshot"))
    .catch((error: unknown) => ({ ok: false as const, message: error instanceof Error ? error.message : "The daemon is unavailable." }));
  const countsPromise = append
    ? Promise.resolve({ ok: false as const, message: "Counts were not refreshed." })
    : runtimeMessage("papio.triage.counts", {})
      .then((response) => responseValue<TriageCounts>(response, "counts"))
      .catch((error: unknown) => ({ ok: false as const, message: error instanceof Error ? error.message : "The daemon is unavailable." }));
  const [snapshotResult, countsResult] = await Promise.all([snapshotPromise, countsPromise]);
  state.loading = false;

  if (snapshotResult.ok) {
    const snapshot = snapshotResult.value;
    state.snapshot = append && state.snapshot !== null
      ? { ...snapshot, items: [...state.snapshot.items, ...snapshot.items] }
      : snapshot;
    state.counts = snapshot.counts;
    state.generatedAt = snapshot.generated_at;
    setConnection(true, "Connected to daemon.");
  } else {
    setConnection(false, snapshotResult.message);
  }
  if (countsResult.ok) state.counts = countsResult.value;
  render();
}

function requestRefresh(): void {
  void refreshInbox();
}

function beginMutation(item: TriageSnapshotItem): boolean {
  if (!state.connected) {
    operationMessage(item.id, "Daemon unavailable — reconnecting automatically.", "offline");
    render();
    return false;
  }
  if (state.pending.has(item.id)) return false;
  state.pending.add(item.id);
  render();
  return true;
}

async function finishMutation(item: TriageSnapshotItem, response: unknown): Promise<void> {
  const result = resultForMutation(response);
  state.pending.delete(item.id);
  if (!result.ok) {
    setConnection(false, result.message);
    operationMessage(item.id, result.message, "offline");
    render();
    return;
  }
  switch (result.outcome) {
    case "applied":
    case "already_applied":
      announce(result.outcome === "applied" ? "Change applied." : "Change was already applied.");
      removeItem(item.id);
      render();
      return;
    case "conflict":
      operationMessage(item.id, "changed elsewhere — refreshed", "info");
      render();
      await refreshInbox();
      return;
    case "error":
      operationMessage(item.id, result.detail ?? "The daemon could not apply this change.", "error");
      render();
      return;
  }
}

// decide drives the watch_hit-only triage-decide RPC (acquire/dismiss a
// watch digest entry). human_action dismiss goes through requestConfirmation
// + papio.action.resolve instead — it cancels a job, not a watch entry.
async function decide(item: TriageSnapshotItem, operation: "acquire" | "dismiss"): Promise<void> {
  if (!beginMutation(item)) return;
  try {
    const response = await runtimeMessage("papio.triage.decide", {
      item_id: item.id,
      op: operation,
      ...(operation === "dismiss" ? { watch_scope: "all" } : {}),
    });
    await finishMutation(item, response);
  } catch (error) {
    await finishMutation(item, { ok: false, error: { message: error instanceof Error ? error.message : "The daemon is unavailable." } });
  }
}

function closeDialog(restoreFocus: boolean): void {
  if (elements === null) return;
  const confirmation = state.confirmation;
  state.confirmation = null;
  elements.dialog.hidden = true;
  if (restoreFocus) confirmation?.returnFocus?.focus();
}

function requestConfirmation(item: TriageSnapshotItem, verdict: Verdict): void {
  if (item.kind !== "human_action" || typeof item.action_id !== "number" || typeof item.revision !== "number") {
    operationMessage(item.id, "This action is missing its revision and cannot be changed.", "error");
    render();
    return;
  }
  if (verdict === "accept" && item.action_kind === "verify_identity" && !hasViewedPreview(item)) {
    operationMessage(item.id, "View the PDF before accepting it.", "info");
    render();
    return;
  }
  state.confirmation = {
    itemID: item.id,
    verdict,
    returnFocus: document.activeElement instanceof HTMLElement ? document.activeElement : null,
  };
  renderDialog();
  elements?.dialogCancel.focus();
}

async function resolveConfirmation(): Promise<void> {
  const confirmation = state.confirmation;
  if (confirmation === null) return;
  const item = itemForID(confirmation.itemID);
  if (
    item === null ||
    item.kind !== "human_action" ||
    typeof item.action_id !== "number" ||
    typeof item.revision !== "number"
  ) {
    closeDialog(true);
    return;
  }
  if (confirmation.verdict === "accept" && item.action_kind === "verify_identity" && !hasViewedPreview(item)) {
    operationMessage(item.id, "View the PDF before accepting it.", "info");
    closeDialog(true);
    render();
    return;
  }
  if (confirmation.verdict === "accept" && (typeof item.sha256 !== "string" || item.sha256.length === 0)) {
    operationMessage(item.id, "This PDF is missing its snapshot hash and cannot be accepted.", "error");
    closeDialog(true);
    render();
    return;
  }
  if (!beginMutation(item)) return;
  renderDialog();
  try {
    const response = await runtimeMessage("papio.action.resolve", {
      action_id: item.action_id,
      verdict: confirmation.verdict,
      expected_revision: item.revision,
      ...(confirmation.verdict === "accept" ? { expected_sha256: item.sha256 } : {}),
    });
    closeDialog(false);
    await finishMutation(item, response);
  } catch (error) {
    closeDialog(false);
    await finishMutation(item, { ok: false, error: { message: error instanceof Error ? error.message : "The daemon is unavailable." } });
  }
}

async function requestPreview(item: TriageSnapshotItem): Promise<void> {
  if (item.kind !== "human_action" || item.action_kind !== "verify_identity" || typeof item.action_id !== "number") return;
  if (!beginMutation(item)) return;
  try {
    const response = await runtimeMessage("papio.preview", { action_id: item.action_id });
    state.pending.delete(item.id);
    // Only a genuine transport/RPC failure (ok !== true) means connectivity
    // is actually down. The daemon rejecting this specific preview (action
    // gone, quarantine file missing, …) comes back as ok:true with
    // outcome:"error" — an ordinary business result, not a disconnect.
    if (!isRecord(response) || response["ok"] !== true || typeof response["outcome"] !== "string") {
      const message = errorFromResponse(response);
      setConnection(false, message);
      operationMessage(item.id, message, "offline");
      render();
      return;
    }
    if (response["outcome"] === "error") {
      const detail = typeof response["detail"] === "string" ? response["detail"] : "This PDF could not be previewed.";
      operationMessage(item.id, detail, "error");
      render();
      return;
    }
    const preview = response["preview"];
    if (!isRecord(preview) || typeof preview["url"] !== "string" || typeof preview["sha256"] !== "string") {
      operationMessage(item.id, "The daemon returned an invalid preview.", "error");
      render();
      return;
    }
    const previewURL = preview["url"];
    const previewSHA256 = preview["sha256"];
    const url = safePreviewURL(previewURL);
    if (url === null || previewSHA256 !== item.sha256) {
      operationMessage(item.id, "Preview did not match this snapshot — refreshed.", "info");
      render();
      await refreshInbox();
      return;
    }
    const token = previewToken(item);
    if (token === null) {
      operationMessage(item.id, "This PDF is missing a verifiable snapshot hash.", "error");
      render();
      return;
    }
    state.previewed.add(token);
    openNewTab(url);
    operationMessage(item.id, "PDF opened. Accept is now available.", "info");
    render();
  } catch (error) {
    state.pending.delete(item.id);
    const message = error instanceof Error ? error.message : "The daemon is unavailable.";
    setConnection(false, message);
    operationMessage(item.id, message, "offline");
    render();
  }
}

function activateOperation(item: TriageSnapshotItem, operation: TriageOperation): void {
  switch (operation) {
    case "acquire":
      void decide(item, operation);
      return;
    case "dismiss":
      if (item.kind === "human_action") requestConfirmation(item, "dismiss");
      else void decide(item, operation);
      return;
    case "accept":
    case "reject":
      requestConfirmation(item, operation);
      return;
    case "open": {
      const url = firstSafeLink(item);
      if (url === null) {
        operationMessage(item.id, "This item has no safe link to open.", "error");
        render();
      } else {
        openNewTab(url);
        announce("Opened the first link in a new tab.");
      }
      return;
    }
    case "retry":
      operationMessage(item.id, "Retry is not available from this inbox version.", "error");
      render();
      return;
  }
}

function activatePrimary(item: TriageSnapshotItem): void {
  if (item.kind === "watch_hit" && hasOperation(item, "acquire")) {
    void decide(item, "acquire");
    return;
  }
  if (item.kind === "human_action" && hasOperation(item, "accept")) {
    requestConfirmation(item, "accept");
    return;
  }
  if (hasOperation(item, "open")) activateOperation(item, "open");
}

function isTypingTarget(target: EventTarget | null): boolean {
  if (!(target instanceof Element)) return false;
  const tag = target.tagName;
  return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || target.getAttribute("contenteditable") === "true";
}

function handleKeyboard(event: KeyboardEvent): void {
  if (event.defaultPrevented || event.ctrlKey || event.metaKey || event.altKey) return;
  if (state.confirmation !== null || isTypingTarget(event.target)) return;
  const items = orderedItems();
  const current = state.selectedID === null ? -1 : items.findIndex((item) => item.id === state.selectedID);
  switch (event.key) {
    case "j":
      if (current >= 0 && current < items.length - 1) {
        event.preventDefault();
        selectItem(items[current + 1]!.id, true);
      }
      return;
    case "k":
      if (current > 0) {
        event.preventDefault();
        selectItem(items[current - 1]!.id, true);
      }
      return;
    case "a":
      if (current >= 0) {
        event.preventDefault();
        activatePrimary(items[current]!);
      }
      return;
    case "d": {
      const current_ = items[current];
      if (current >= 0 && current_ !== undefined && hasOperation(current_, "dismiss")) {
        event.preventDefault();
        if (current_.kind === "human_action") requestConfirmation(current_, "dismiss");
        else void decide(current_, "dismiss");
      }
      return;
    }
    case "o":
      if (current >= 0) {
        event.preventDefault();
        const url = firstSafeLink(items[current]!);
        if (url !== null) {
          openNewTab(url);
          announce("Opened the first link in a new tab.");
        }
      }
      return;
    default:
      return;
  }
}

function trapDialogFocus(event: KeyboardEvent): void {
  if (state.confirmation === null || elements === null) return;
  if (event.key === "Escape") {
    event.preventDefault();
    closeDialog(true);
    return;
  }
  if (event.key !== "Tab") return;
  const focusable = [elements.dialogCancel, elements.dialogConfirm].filter((button) => !button.disabled);
  if (focusable.length === 0) {
    event.preventDefault();
    return;
  }
  const first = focusable[0]!;
  const last = focusable[focusable.length - 1]!;
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault();
    first.focus();
  }
}

function bootstrap(): void {
  const connection = document.getElementById("connection-status");
  const counts = document.getElementById("inbox-counts");
  const filterInput = document.getElementById("item-filter");
  const refresh = document.getElementById("refresh-inbox");
  const reconnect = document.getElementById("reconnect-daemon");
  const list = document.getElementById("item-list");
  const operationStatus = document.getElementById("operation-status");
  const generatedAt = document.getElementById("generated-at");
  const loadMore = document.getElementById("load-more");
  const dialog = document.getElementById("confirm-dialog");
  const dialogMessage = document.getElementById("confirm-dialog-message");
  const dialogCancel = document.getElementById("confirm-cancel");
  const dialogConfirm = document.getElementById("confirm-submit");
  const citationStyle = document.getElementById("citation-style");
  if (
    !(connection instanceof HTMLElement) ||
    !(counts instanceof HTMLElement) ||
    !(filterInput instanceof HTMLInputElement) ||
    !(refresh instanceof HTMLButtonElement) ||
    !(reconnect instanceof HTMLButtonElement) ||
    !(list instanceof HTMLElement) ||
    !(operationStatus instanceof HTMLElement) ||
    !(generatedAt instanceof HTMLTimeElement) ||
    !(loadMore instanceof HTMLButtonElement) ||
    !(citationStyle instanceof HTMLSelectElement) ||
    !(dialog instanceof HTMLElement) ||
    !(dialogMessage instanceof HTMLElement) ||
    !(dialogCancel instanceof HTMLButtonElement) ||
    !(dialogConfirm instanceof HTMLButtonElement)
  ) {
    return;
  }
  elements = {
    connection,
    counts,
    filterInput,
    refresh,
    reconnect,
    list,
    operationStatus,
    generatedAt,
    loadMore,
    citationStyle,
    dialog,
    dialogMessage,
    dialogCancel,
    dialogConfirm,
  };
  refresh.addEventListener("click", requestRefresh);
  reconnect.addEventListener("click", requestRefresh);
  citationStyle.value = state.citationStyle;
  citationStyle.addEventListener("change", () => {
    const value = citationStyle.value;
    if (value === "apa" || value === "mla" || value === "chicago") {
      state.citationStyle = value;
      persistCitationStyle(value);
      render();
    }
  });
  filterInput.addEventListener("input", () => {
    state.filterQuery = filterInput.value;
    render();
  });
  loadMore.addEventListener("click", () => {
    void refreshInbox(true);
  });
  dialogCancel.addEventListener("click", () => closeDialog(true));
  dialogConfirm.addEventListener("click", () => {
    void resolveConfirmation();
  });
  document.addEventListener("keydown", handleKeyboard);
  document.addEventListener("keydown", trapDialogFocus);
  render();
  void refreshInbox();
}

if (typeof document !== "undefined") {
  if (document.getElementById("item-list") !== null) bootstrap();
  else document.addEventListener("DOMContentLoaded", bootstrap, { once: true });
}
