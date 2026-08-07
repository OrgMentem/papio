// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// ADR-0019 Decision 4: the on-page bulk selection workspace. One instance of
// this module runs per `?scan=<id>` tab (Decision 4: one workspace per active
// scan, never a singleton like the inbox). It never touches chrome.storage or
// the daemon directly — every read and mutation is a finite, correlated
// runtime message to the background broker (mirrors inbox.ts's own
// discipline), and every dynamic label is written through textContent/
// createTextNode, never innerHTML — labels in this page are untrusted page
// text lifted off a site papio does not control.

import type { DetectedPaper, PageBulkSnapshot } from "./page-scan";
import type { PageBulkIdentifier, PageBulkStatus, PageBulkStatusItem } from "./protocol";

/** background.ts's PageBulkSnapshotView: page-scan.ts's PageBulkSnapshot plus
 * two background-local, browser-only UI fields (source page title and scan
 * wall time) that are never sent to the daemon and so are deliberately kept
 * off page-scan.ts's shared detector/daemon shape. Declared locally rather
 * than imported from background.ts — importing the service-worker module
 * here would re-run its top-level `chrome.runtime.id` wiring inside this
 * page. */
type WorkspaceSnapshot = PageBulkSnapshot & {
  sourceTitle: string;
  scannedAt: string;
};

const SUBMIT_CAP = 50;

const KIND_LABEL: Record<DetectedPaper["identifier"]["kind"], string> = {
  doi: "DOI",
  pmid: "PMID",
  arxiv: "arXiv",
};

const STATUS_LABEL: Record<PageBulkStatus, string> = {
  eligible: "Eligible",
  owned_with_pdf: "Already in your library",
  owned_missing_pdf: "Eligible — no PDF on file",
  queued: "Queued",
  previously_unavailable: "No route previously",
  ownership_incomplete: "Ownership unclear",
  invalid: "Not a recognized identifier",
};

/** Decision 5: only a fresh owned_with_pdf claim disables acquisition, plus
 * a live queued job and an invalid identifier that never resolved. Every
 * other status stays eligible — an incomplete or failed lookup is never a
 * negative ownership fact (ADR-0008). */
function isEligibleStatus(status: PageBulkStatus | null): boolean {
  return status !== null && status !== "owned_with_pdf" && status !== "queued" && status !== "invalid";
}

interface RowState {
  localId: string;
  identifier: DetectedPaper["identifier"];
  label: string;
  occurrences: number;
  /** null until the status round trip resolves (or fails) for this row. */
  status: PageBulkStatus | null;
  canonicalKey: string | null;
  jobId: string | null;
  selected: boolean;
  /** Set once this row's canonical key has gone out in a successful submit;
   * it stays visible (Decision 5 gives no reason to hide it) but can never
   * be selected again. */
  submitted: boolean;
}

interface SubmitResult {
  submitted: number;
  joined: number;
  alreadyOwned: number;
  invalid: number;
  batchId: string;
}

interface WorkspaceState {
  scanId: string;
  snapshot: WorkspaceSnapshot | null;
  detector: string;
  rows: RowState[];
  /** Generic, unrecoverable load failure — distinct from `expired`, which
   * gets its own operator-actionable copy. */
  loadError: string | null;
  /** The snapshot aged out of the background's bounded store, or the
   * browser session that held it ended (Decision 4: chrome.storage.session
   * only). */
  expired: boolean;
  /** The source tab was closed — discovered lazily, only when "Return to
   * source page" or Rescan actually tries to reach it (never probed ambiently). */
  sourceTabClosed: boolean;
  statusError: string | null;
  statusLoaded: boolean;
  rescanning: boolean;
  submitting: boolean;
  result: SubmitResult | null;
}

interface PageElements {
  scanTitle: HTMLElement;
  scanMeta: HTMLElement;
  scanSummary: HTMLElement;
  scanError: HTMLElement;
  scanExpired: HTMLElement;
  statusError: HTMLElement;
  statusErrorMessage: HTMLElement;
  statusRetryButton: HTMLButtonElement;
  truncatedNote: HTMLElement;
  ownershipNote: HTMLElement;
  workspaceMain: HTMLElement;
  rows: HTMLElement;
  emptyState: HTMLElement;
  actionBar: HTMLElement;
  returnButton: HTMLButtonElement;
  sourceClosedNote: HTMLElement;
  rescanButton: HTMLButtonElement;
  primaryButton: HTMLButtonElement;
  submitStatus: HTMLElement;
  resultSummary: HTMLElement;
  allowlistCheckbox: HTMLInputElement;
}

let elements: PageElements | null = null;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function errorFromResponse(value: unknown): string {
  if (isRecord(value) && isRecord(value["error"]) && typeof value["error"]["message"] === "string") {
    return value["error"]["message"];
  }
  return "The extension runtime did not return a usable response.";
}

function errorCode(value: unknown): string | undefined {
  if (isRecord(value) && isRecord(value["error"]) && typeof value["error"]["code"] === "string") {
    return value["error"]["code"];
  }
  return undefined;
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

function isDetectedPaper(value: unknown): value is DetectedPaper {
  if (!isRecord(value) || typeof value["localId"] !== "string" || typeof value["label"] !== "string") return false;
  if (typeof value["occurrences"] !== "number" || value["detector"] !== "generic-identifiers/1") return false;
  const identifier = value["identifier"];
  if (!isRecord(identifier) || typeof identifier["value"] !== "string") return false;
  const kind = identifier["kind"];
  return kind === "doi" || kind === "pmid" || kind === "arxiv";
}

function isWorkspaceSnapshot(value: unknown): value is WorkspaceSnapshot {
  return (
    isRecord(value) &&
    typeof value["scanId"] === "string" &&
    typeof value["sourceTabId"] === "number" &&
    typeof value["sourceOrigin"] === "string" &&
    typeof value["sourceTitle"] === "string" &&
    typeof value["scannedAt"] === "string" &&
    typeof value["documentGeneration"] === "number" &&
    typeof value["truncated"] === "boolean" &&
    Array.isArray(value["items"]) &&
    value["items"].every(isDetectedPaper)
  );
}

function isStatusItem(value: unknown): value is PageBulkStatusItem {
  if (!isRecord(value) || typeof value["local_id"] !== "string" || typeof value["ownership_complete"] !== "boolean") {
    return false;
  }
  const status = value["status"];
  const validStatus =
    status === "eligible" ||
    status === "owned_with_pdf" ||
    status === "owned_missing_pdf" ||
    status === "queued" ||
    status === "previously_unavailable" ||
    status === "ownership_incomplete" ||
    status === "invalid";
  if (!validStatus) return false;
  if (status !== "invalid" && typeof value["canonical_key"] !== "string") return false;
  if ("job_id" in value && typeof value["job_id"] !== "string") return false;
  return true;
}

function isStatusReply(value: unknown): value is { ok: true; items: PageBulkStatusItem[]; truncated: boolean } {
  return (
    isRecord(value) &&
    value["ok"] === true &&
    Array.isArray(value["items"]) &&
    value["items"].every(isStatusItem) &&
    typeof value["truncated"] === "boolean"
  );
}

function isSubmitReply(
  value: unknown,
): value is { ok: true; submitted: number; joined: number; already_owned: number; invalid: number; batch_id: string } {
  return (
    isRecord(value) &&
    value["ok"] === true &&
    typeof value["submitted"] === "number" &&
    typeof value["joined"] === "number" &&
    typeof value["already_owned"] === "number" &&
    typeof value["invalid"] === "number" &&
    typeof value["batch_id"] === "string"
  );
}

function element<K extends keyof HTMLElementTagNameMap>(tag: K, text?: string): HTMLElementTagNameMap[K] {
  const created = document.createElement(tag);
  if (text !== undefined) created.textContent = text;
  return created;
}

function rowsFromSnapshot(snapshot: WorkspaceSnapshot): RowState[] {
  // Decision 5: rows always open unselected, whether this is the first load
  // or the result of pressing Rescan.
  return snapshot.items.map((item) => ({
    localId: item.localId,
    identifier: item.identifier,
    label: item.label,
    occurrences: item.occurrences,
    status: null,
    canonicalKey: null,
    jobId: null,
    selected: false,
    submitted: false,
  }));
}

function scanIdFromLocation(): string | null {
  if (typeof window === "undefined") return null;
  const scanId = new URL(window.location.href).searchParams.get("scan");
  return scanId !== null && scanId.length > 0 ? scanId : null;
}

function formatScanTime(iso: string): string {
  const parsed = new Date(iso);
  return Number.isNaN(parsed.getTime()) ? "" : parsed.toLocaleString();
}

/** Mirrors popup.ts's historyPagePath()/background.ts's inboxURL: derive the
 * inbox page's path from the manifest's declared default_popup so it
 * inherits the real build output directory (dist/ for both Chrome and
 * Firefox) instead of a hardcoded extension-root path that never exists
 * (build.ts's copyExtensionPages() only ever writes inbox.html under
 * dist/ or firefox/dist/). */
function inboxPagePath(): string {
  const declaredPopup = chrome.runtime.getManifest().action?.default_popup ?? "dist/popup.html";
  return declaredPopup.replace(/[^/]*$/, "inbox.html");
}

const state: WorkspaceState = {
  scanId: scanIdFromLocation() ?? "",
  snapshot: null,
  detector: "generic-identifiers/1",
  rows: [],
  loadError: null,
  expired: false,
  sourceTabClosed: false,
  statusError: null,
  statusLoaded: false,
  rescanning: false,
  submitting: false,
  result: null,
};

function eligibleRows(): RowState[] {
  return state.rows.filter((row) => !row.submitted && isEligibleStatus(row.status));
}

function selectedRows(): RowState[] {
  return state.rows.filter((row) => row.selected && !row.submitted && isEligibleStatus(row.status));
}

// -----------------------------------------------------------------------
// Rendering — every function below is a pure function of `state`, called
// through the master render() after each state change (mirrors inbox.ts's
// render() convention). No dynamic text ever goes through innerHTML.
// -----------------------------------------------------------------------

function renderHeader(): void {
  if (elements === null) return;
  const snapshot = state.snapshot;
  if (snapshot === null) {
    elements.scanTitle.textContent = "";
    elements.scanMeta.textContent = "";
    return;
  }
  document.title = `papio — select papers: ${snapshot.sourceTitle}`;
  elements.scanTitle.textContent = `Select papers: ${snapshot.sourceTitle}`;
  elements.scanMeta.textContent = `${snapshot.sourceOrigin} · scanned ${formatScanTime(snapshot.scannedAt)}`;
  const count = snapshot.items.length;
  let summary = count === 1 ? "1 identified paper found" : `${count} identified papers found`;
  if (state.statusLoaded) summary += ` — ${eligibleRows().length} eligible`;
  elements.scanSummary.textContent = summary;
  elements.truncatedNote.hidden = !snapshot.truncated;
}

function renderBanners(): void {
  if (elements === null) return;
  elements.scanExpired.hidden = !state.expired;
  elements.scanError.hidden = state.loadError === null;
  elements.scanError.textContent = state.loadError ?? "";
  elements.statusError.hidden = state.statusError === null;
  elements.statusErrorMessage.textContent = state.statusError ?? "";
  // A Rescan in flight is about to replace state.rows outright (Decision 4);
  // a Retry click racing that swap would send a status request keyed to rows
  // that are seconds from being discarded, so gate it the same way the
  // Rescan button itself is gated.
  elements.statusRetryButton.disabled = state.rescanning || state.snapshot === null;
  elements.ownershipNote.hidden = !ownershipUnclearOnly();
}

function rowStatusText(row: RowState): string {
  if (row.submitted) return "Submitted";
  if (row.status === null) return "Checking availability…";
  const label = STATUS_LABEL[row.status];
  return row.status === "queued" && row.jobId !== null ? `${label} (job ${row.jobId})` : label;
}

function isRowCheckable(row: RowState): boolean {
  return !row.submitted && isEligibleStatus(row.status);
}

// The identifier line doubles as the row's outbound link, built exactly the
// way inbox.ts's citationAnchor builds its DOI link (fixed origin, _blank,
// rel="noopener noreferrer"). Only the identifier's own value is
// interpolated, and it is percent-encoded first: these values were lifted
// off a page papio does not control, so they never reach the URL's
// structure.
const IDENTIFIER_URL: Record<DetectedPaper["identifier"]["kind"], (encoded: string) => string> = {
  doi: (encoded) => `https://doi.org/${encoded}`,
  arxiv: (encoded) => `https://arxiv.org/abs/${encoded}`,
  pmid: (encoded) => `https://pubmed.ncbi.nlm.nih.gov/${encoded}/`,
};

function identifierURL(identifier: DetectedPaper["identifier"]): string | null {
  const value = identifier.value.trim();
  if (value === "") return null;
  // encodeURI keeps a DOI suffix's meaningful slashes; the two structural
  // characters it leaves alone would still cut the path short, so escape
  // those by hand.
  const encoded = encodeURI(value).replace(/#/g, "%23").replace(/\?/g, "%3F");
  return IDENTIFIER_URL[identifier.kind](encoded);
}

const IDENTIFIER_PREFIX_RE =
  /(?:\b(?:doi|arxiv|pmid)\s*:\s*|https?:\/\/(?:dx\.)?doi\.org\/|https?:\/\/arxiv\.org\/abs\/)\s*$/i;

/** Page-derived labels routinely carry the very identifier this row already
 * prints on its own linked line ("Some Title doi:10.1234/x"). Mirrors
 * inbox.ts's renderCitation rule — a row whose displayed title is already
 * the DOI does not repeat that link — from the other side: here the label
 * is what gets trimmed. Display only; the row's `label` and the
 * background's snapshot keep the full page text. */
function displayLabel(row: RowState): string {
  const label = row.label;
  const value = row.identifier.value;
  if (value === "") return label;
  const at = label.toLowerCase().indexOf(value.toLowerCase());
  if (at < 0) return label;
  let start = at;
  let end = at + value.length;
  const prefix = IDENTIFIER_PREFIX_RE.exec(label.slice(0, start));
  if (prefix !== null) start -= prefix[0].length;
  // "Some Title (doi:10.1234/x)" loses the whole parenthetical, not just
  // its contents.
  const opener = label[start - 1];
  const closer = label[end];
  if ((opener === "(" && closer === ")") || (opener === "[" && closer === "]")) {
    start -= 1;
    end += 1;
  }
  const trimmed = `${label.slice(0, start)}${label.slice(end)}`
    .replace(/\s+/g, " ")
    .replace(/^[\s.,;:|·\u2013\u2014-]+|[\s.,;:|·\u2013\u2014-]+$/g, "");
  // A label that is nothing but its identifier has nothing left to show,
  // and the checkbox still needs an accessible name: keep it whole.
  return trimmed === "" ? label : trimmed;
}

/** Decision 5 leaves "ownership unclear" eligible, so a daemon that cannot
 * see the library at all stamps every row with the same badge and the badge
 * stops carrying information. Collapse it into one explanation under the
 * header; any mixed result keeps its per-row badges. */
function ownershipUnclearOnly(): boolean {
  if (!state.statusLoaded) return false;
  const graded = state.rows.filter((row) => row.status !== "invalid");
  return graded.length > 0 && graded.every((row) => row.status === "ownership_incomplete");
}

function buildRow(row: RowState, ownershipCollapsed: boolean): HTMLElement {
  const wrapper = element("div");
  wrapper.className = "pb-row";
  wrapper.dataset.localId = row.localId;
  wrapper.dataset.status = row.submitted ? "submitted" : (row.status ?? "pending");
  wrapper.dataset.disabled = String(!isRowCheckable(row));
  const labelText = displayLabel(row);

  const checkboxId = `pb-row-check-${row.localId}`;
  const checkbox = element("input");
  checkbox.type = "checkbox";
  checkbox.id = checkboxId;
  checkbox.checked = row.selected;
  checkbox.disabled = !isRowCheckable(row);
  checkbox.setAttribute("aria-label", `Select ${labelText}`);
  checkbox.addEventListener("change", () => {
    row.selected = checkbox.checked;
    render();
  });
  wrapper.append(checkbox);

  const content = element("div");
  content.className = "pb-row-content";

  const label = element("label", labelText);
  label.className = "pb-row-label";
  label.htmlFor = checkboxId;
  content.append(label);

  const meta = element("div");
  meta.className = "pb-row-meta";
  const identifier = element("span");
  identifier.className = "pb-row-identifier";
  const kind = element("span", `${KIND_LABEL[row.identifier.kind]}:`);
  kind.className = "pb-row-kind";
  identifier.append(kind, document.createTextNode(" "));
  const url = identifierURL(row.identifier);
  if (url === null) {
    identifier.append(document.createTextNode(row.identifier.value));
  } else {
    const link = element("a", row.identifier.value);
    link.className = "pb-row-link";
    link.href = url;
    link.target = "_blank";
    link.rel = "noopener noreferrer";
    identifier.append(link);
  }
  meta.append(identifier);
  const collapsedHere = ownershipCollapsed && !row.submitted && row.status === "ownership_incomplete";
  if (!collapsedHere) {
    const status = element("span", rowStatusText(row));
    status.className = "pb-row-status";
    meta.append(status);
  }
  if (row.occurrences > 1) meta.append(element("span", `seen ${row.occurrences}×`));
  content.append(meta);

  wrapper.append(content);
  return wrapper;
}

function renderRows(): void {
  if (elements === null) return;
  const ownershipCollapsed = ownershipUnclearOnly();
  elements.emptyState.hidden = state.rows.length !== 0;
  elements.rows.replaceChildren(...state.rows.map((row) => buildRow(row, ownershipCollapsed)));
}

function updatePrimaryButton(): void {
  if (elements === null) return;
  const eligible = eligibleRows();
  const selected = selectedRows();
  if (selected.length === 0) {
    // Nothing checked: acquire-all mode. Past the cap, the button itself
    // says what will actually happen — "50 selected" with zero checkboxes
    // checked read as a bug in the first live session.
    const capped = Math.min(eligible.length, SUBMIT_CAP);
    elements.primaryButton.textContent =
      eligible.length > SUBMIT_CAP
        ? `Acquire ${capped} of ${eligible.length} eligible`
        : `Acquire all ${eligible.length} eligible`;
    elements.primaryButton.disabled = state.submitting || !state.statusLoaded || eligible.length === 0;
    elements.submitStatus.textContent =
      state.statusLoaded && eligible.length > SUBMIT_CAP
        ? `papio batches are limited to ${SUBMIT_CAP} — the remaining ${eligible.length - SUBMIT_CAP} stay listed for the next batch`
        : "";
    return;
  }
  elements.primaryButton.textContent = `Acquire ${selected.length} selected`;
  elements.primaryButton.disabled = state.submitting || !state.statusLoaded;
  elements.submitStatus.textContent =
    selected.length > SUBMIT_CAP
      ? `${SUBMIT_CAP} of ${selected.length} selected will be submitted · papio batches are limited to ${SUBMIT_CAP}`
      : "";
}

function renderActionBar(): void {
  if (elements === null) return;
  const haveSnapshot = state.snapshot !== null && !state.expired;
  elements.actionBar.hidden = !haveSnapshot;
  elements.returnButton.hidden = state.sourceTabClosed;
  elements.returnButton.disabled = state.snapshot === null;
  elements.sourceClosedNote.hidden = !state.sourceTabClosed;
  elements.rescanButton.hidden = state.sourceTabClosed;
  elements.rescanButton.disabled = state.rescanning || state.snapshot === null;
  updatePrimaryButton();
}

function renderResult(): void {
  if (elements === null) return;
  const result = state.result;
  if (result === null) {
    elements.resultSummary.hidden = true;
    elements.resultSummary.replaceChildren();
    return;
  }
  elements.resultSummary.hidden = false;
  const parts = [
    `${result.submitted} submitted`,
    `${result.joined} joined`,
    `${result.alreadyOwned} already owned`,
    `${result.invalid} invalid`,
  ];
  const summary = element("p", parts.join(" · "));
  const link = element("a", "Open inbox");
  link.href = typeof chrome !== "undefined" ? chrome.runtime.getURL(inboxPagePath()) : "inbox.html";
  link.target = "_blank";
  link.rel = "noopener noreferrer";
  elements.resultSummary.replaceChildren(summary, link);
}

function render(): void {
  if (elements === null) return;
  renderHeader();
  renderBanners();
  const showWorkspace = state.snapshot !== null && !state.expired && state.loadError === null;
  elements.workspaceMain.hidden = !showWorkspace;
  if (showWorkspace) renderRows();
  else elements.rows.replaceChildren();
  renderActionBar();
  renderResult();
}

// -----------------------------------------------------------------------
// Background round trips — the finite papio.pageBulk.* message set
// (ADR-0019 Decision 4/7). Each is a single correlated request/reply, no
// {method, params} pass-through.
// -----------------------------------------------------------------------

async function loadAllowlist(origin: string): Promise<void> {
  const response = await runtimeMessage("papio.pageBulk.allowlist.get", { origin });
  const parsed = responseValue<boolean>(response, "allowed");
  if (parsed.ok && elements !== null) elements.allowlistCheckbox.checked = parsed.value;
}

async function loadStatus(): Promise<void> {
  if (state.snapshot === null) return;
  // Decision 4: documentGeneration bumps on every rescan (page-scan.ts). A
  // reply is only ever applied against the exact generation it was
  // requested under — Rescan replaces state.rows with fresh RowStates whose
  // localIds are recomputed deterministically in detection order, so a
  // stale reply's local_ids can collide with the NEW rows and silently
  // overwrite their status/canonicalKey/jobId with data about entirely
  // different papers if this were not checked.
  const requestGeneration = state.snapshot.documentGeneration;
  if (state.rows.length === 0) {
    state.statusLoaded = true;
    render();
    return;
  }
  state.statusError = null;
  render();
  const identifiers: PageBulkIdentifier[] = state.rows.map((row) => ({
    local_id: row.localId,
    kind: row.identifier.kind,
    value: row.identifier.value,
  }));
  let response: unknown;
  try {
    response = await runtimeMessage("papio.pageBulk.status", { scan_id: state.snapshot.scanId, identifiers });
  } catch (e) {
    if (state.snapshot === null || state.snapshot.documentGeneration !== requestGeneration) return;
    state.statusError = e instanceof Error ? e.message : "Could not reach the extension runtime.";
    render();
    return;
  }
  if (state.snapshot === null || state.snapshot.documentGeneration !== requestGeneration) return;
  if (!isStatusReply(response)) {
    state.statusError = errorFromResponse(response);
    render();
    return;
  }
  const byLocalID = new Map(response.items.map((item) => [item.local_id, item]));
  for (const row of state.rows) {
    const item = byLocalID.get(row.localId);
    if (item === undefined) continue;
    row.status = item.status;
    row.canonicalKey = item.canonical_key ?? null;
    row.jobId = item.job_id ?? null;
    if (!isEligibleStatus(row.status)) row.selected = false;
  }
  state.statusLoaded = true;
  render();
}

function applySnapshot(snapshot: WorkspaceSnapshot): void {
  state.snapshot = snapshot;
  state.scanId = snapshot.scanId;
  state.detector = snapshot.items[0]?.detector ?? "generic-identifiers/1";
  state.rows = rowsFromSnapshot(snapshot);
  state.statusLoaded = false;
  state.statusError = null;
  state.sourceTabClosed = false;
  state.expired = false;
  state.loadError = null;
  state.result = null;
  void loadAllowlist(snapshot.sourceOrigin);
}

async function loadInitial(): Promise<void> {
  if (state.scanId === "") {
    state.loadError = "No scan specified. Open this workspace from papio's popup.";
    render();
    return;
  }
  let response: unknown;
  try {
    response = await runtimeMessage("papio.pageBulk.load", { scan_id: state.scanId });
  } catch (e) {
    state.loadError = e instanceof Error ? e.message : "Could not reach the extension runtime.";
    render();
    return;
  }
  if (errorCode(response) === "scan_not_found") {
    state.expired = true;
    render();
    return;
  }
  const parsed = responseValue<unknown>(response, "snapshot");
  if (!parsed.ok || !isWorkspaceSnapshot(parsed.value)) {
    state.loadError = parsed.ok ? "The extension runtime returned an invalid scan snapshot." : parsed.message;
    render();
    return;
  }
  applySnapshot(parsed.value);
  render();
  void loadStatus();
}

async function handleRescan(): Promise<void> {
  if (elements === null || state.snapshot === null || state.rescanning) return;
  state.rescanning = true;
  render();
  let response: unknown;
  try {
    response = await runtimeMessage("papio.pageBulk.rescan", { scan_id: state.snapshot.scanId });
  } catch (e) {
    state.rescanning = false;
    state.loadError = e instanceof Error ? e.message : "Could not reach the extension runtime.";
    render();
    return;
  }
  state.rescanning = false;
  if (errorCode(response) === "tab_unavailable") {
    state.sourceTabClosed = true;
    render();
    return;
  }
  if (errorCode(response) === "scan_not_found") {
    state.expired = true;
    render();
    return;
  }
  const parsed = responseValue<unknown>(response, "snapshot");
  if (!parsed.ok || !isWorkspaceSnapshot(parsed.value)) {
    state.loadError = parsed.ok ? "The extension runtime returned an invalid scan snapshot." : parsed.message;
    render();
    return;
  }
  applySnapshot(parsed.value);
  render();
  void loadStatus();
}

/** "Return to source page" focuses sourceTabId directly — the popup
 * already talks to chrome.tabs this way (readCurrentPageMetadata,
 * startPageBulkScan), and this extension page carries the same "tabs"
 * manifest permission, so no extra background round trip is needed for a
 * plain focus. A rejection (tab closed) degrades to the plain "source tab
 * closed" state instead of surfacing an error. */
async function focusSourceTab(tabID: number): Promise<boolean> {
  try {
    const tab = await chrome.tabs.update(tabID, { active: true });
    const windowId = tab?.windowId;
    if (windowId !== undefined) {
      try {
        const win = await chrome.windows.get(windowId);
        await chrome.windows.update(windowId, {
          focused: true,
          ...(win.state === "minimized" ? { state: "normal" as const } : {}),
        });
      } catch {
        // Best-effort window focus; the tab itself is already active.
      }
    }
    return true;
  } catch {
    return false;
  }
}

async function handleReturnToSource(): Promise<void> {
  if (state.snapshot === null) return;
  const focused = await focusSourceTab(state.snapshot.sourceTabId);
  if (!focused) {
    state.sourceTabClosed = true;
    render();
  }
}

async function handleSubmit(): Promise<void> {
  if (elements === null || state.snapshot === null || state.submitting) return;
  const selected = selectedRows();
  const targetRows = selected.length === 0 ? eligibleRows() : selected;
  // Decision 5: submission caps at 50; excess rows are not auto-chained —
  // they simply keep their current selected/unselected state for the next
  // submit click ("remainder retained").
  const batch = targetRows.slice(0, SUBMIT_CAP);
  const canonicalKeys = batch.map((row) => row.canonicalKey).filter((key): key is string => key !== null);
  if (canonicalKeys.length === 0) return;
  state.submitting = true;
  render();
  let response: unknown;
  try {
    response = await runtimeMessage("papio.pageBulk.submit", {
      scan_id: state.snapshot.scanId,
      canonical_keys: canonicalKeys,
      source: { kind: "browser_page", origin: state.snapshot.sourceOrigin, detector: state.detector },
    });
  } catch (e) {
    state.submitting = false;
    elements.submitStatus.textContent = e instanceof Error ? e.message : "Could not reach the extension runtime.";
    render();
    return;
  }
  state.submitting = false;
  if (!isSubmitReply(response)) {
    elements.submitStatus.textContent = errorFromResponse(response);
    render();
    return;
  }
  for (const row of batch) {
    row.submitted = true;
    row.selected = false;
  }
  state.result = {
    submitted: response.submitted,
    joined: response.joined,
    alreadyOwned: response.already_owned,
    invalid: response.invalid,
    batchId: response.batch_id,
  };
  render();
}

function bootstrap(): void {
  const scanTitle = document.getElementById("scan-title");
  const scanMeta = document.getElementById("scan-meta");
  const scanSummary = document.getElementById("scan-summary");
  const scanError = document.getElementById("scan-error");
  const scanExpired = document.getElementById("scan-expired");
  const statusError = document.getElementById("status-error");
  const statusErrorMessage = document.getElementById("status-error-message");
  const statusRetryButton = document.getElementById("status-retry-btn");
  const truncatedNote = document.getElementById("truncated-note");
  const ownershipNote = document.getElementById("ownership-unclear-note");
  const workspaceMain = document.getElementById("workspace-main");
  const rows = document.getElementById("rows");
  const emptyState = document.getElementById("empty-state");
  const actionBar = document.getElementById("action-bar");
  const returnButton = document.getElementById("return-to-source-btn");
  const sourceClosedNote = document.getElementById("source-closed-note");
  const rescanButton = document.getElementById("rescan-btn");
  const primaryButton = document.getElementById("primary-btn");
  const submitStatus = document.getElementById("submit-status");
  const resultSummary = document.getElementById("result-summary");
  const allowlistCheckbox = document.getElementById("allowlist-checkbox");
  if (
    !(scanTitle instanceof HTMLElement) ||
    !(scanMeta instanceof HTMLElement) ||
    !(scanSummary instanceof HTMLElement) ||
    !(scanError instanceof HTMLElement) ||
    !(scanExpired instanceof HTMLElement) ||
    !(statusError instanceof HTMLElement) ||
    !(statusErrorMessage instanceof HTMLElement) ||
    !(statusRetryButton instanceof HTMLButtonElement) ||
    !(truncatedNote instanceof HTMLElement) ||
    !(ownershipNote instanceof HTMLElement) ||
    !(workspaceMain instanceof HTMLElement) ||
    !(rows instanceof HTMLElement) ||
    !(emptyState instanceof HTMLElement) ||
    !(actionBar instanceof HTMLElement) ||
    !(returnButton instanceof HTMLButtonElement) ||
    !(sourceClosedNote instanceof HTMLElement) ||
    !(rescanButton instanceof HTMLButtonElement) ||
    !(primaryButton instanceof HTMLButtonElement) ||
    !(submitStatus instanceof HTMLElement) ||
    !(resultSummary instanceof HTMLElement) ||
    !(allowlistCheckbox instanceof HTMLInputElement)
  ) {
    return;
  }
  elements = {
    scanTitle,
    scanMeta,
    scanSummary,
    scanError,
    scanExpired,
    statusError,
    statusErrorMessage,
    statusRetryButton,
    truncatedNote,
    ownershipNote,
    workspaceMain,
    rows,
    emptyState,
    actionBar,
    returnButton,
    sourceClosedNote,
    rescanButton,
    primaryButton,
    submitStatus,
    resultSummary,
    allowlistCheckbox,
  };
  returnButton.addEventListener("click", () => {
    void handleReturnToSource();
  });
  rescanButton.addEventListener("click", () => {
    void handleRescan();
  });
  primaryButton.addEventListener("click", () => {
    void handleSubmit();
  });
  statusRetryButton.addEventListener("click", () => {
    void loadStatus();
  });
  allowlistCheckbox.addEventListener("change", () => {
    if (state.snapshot === null) return;
    void runtimeMessage("papio.pageBulk.allowlist.set", {
      origin: state.snapshot.sourceOrigin,
      allowed: allowlistCheckbox.checked,
    });
  });
  render();
  void loadInitial();
}

if (typeof document !== "undefined") {
  if (document.getElementById("rows") !== null) bootstrap();
  else document.addEventListener("DOMContentLoaded", bootstrap, { once: true });
}
