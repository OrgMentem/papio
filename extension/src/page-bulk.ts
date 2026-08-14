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
import { durablePdfGrabState, type PageBulkIdentifier, type PageBulkStatus, type PageBulkStatusItem } from "./protocol";

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
  /** Feature detection is supplied by the background broker, never inferred
   * from a user-agent string. */
  pdfGrabAvailable?: boolean;
};

const MAX_MANIFEST = 200;

const KIND_LABEL: Record<DetectedPaper["identifier"]["kind"], string> = {
  doi: "DOI",
  pmid: "PMID",
  arxiv: "arXiv",
  openalex: "OpenAlex",
};

const STATUS_LABEL: Record<PageBulkStatus, string> = {
  eligible: "Eligible",
  owned_with_pdf: "Already in your library",
  owned_missing_pdf: "Eligible — no PDF on file",
  queued: "Queued",
  previously_unavailable: "No route previously",
  ownership_incomplete: "Ownership unclear",
  ownership_unknown: "Library check unavailable",
  invalid: "Not a recognized identifier",
  frame_too_large: "Could not fit in daemon response",
};

/** Decision 5: only a fresh owned_with_pdf claim disables acquisition, plus
 * a live queued job, an invalid identifier that never resolved, or a result
 * refused because it could not fit the daemon response. */
function isEligibleStatus(status: PageBulkStatus | null): boolean {
  return (
    status !== null &&
    status !== "owned_with_pdf" &&
    status !== "queued" &&
    status !== "invalid" &&
    status !== "frame_too_large"
  );
}

interface RowState {
  localId: string;
  kind: "identifier" | "pdf_grab";
  identifier: DetectedPaper["identifier"] | null;
  label: string;
  occurrences: number;
  grabURL: string | null;
  grabTitle: string | null;
  grabID: string | null;
  status: PageBulkStatus | null;
  canonicalKey: string | null;
  jobId: string | null;
  selected: boolean;
  submitted: boolean;
}

interface SubmitResult {
  mode: "v1" | "v2";
  processedCount: number;
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
  loadError: string | null;
  expired: boolean;
  sourceTabClosed: boolean;
  statusError: string | null;
  statusLoaded: boolean;
  statusRetryAttempted: boolean;
  rescanning: boolean;
  submitting: boolean;
  result: SubmitResult | null;
  grabState: "idle" | "grabbed" | "identifying" | "job_created" | "already_owned" | "needs_identifier" | "failed";
  grabID: string | null;
  grabDetail: string | null;
  allowlistStored: boolean;
  allowlistPending: boolean;
  allowlistError: string | null;
  rescanRefusal: string | null;
  rescanSourceChanged: boolean;
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
  allowlistMessage: HTMLElement;
}

let elements: PageElements | null = null;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

const CONNECTION_LOST_COPY = "papio lost its connection to the daemon and is retrying…";
const RUNTIME_FAILURE_COPY = "papio could not complete that request. Please try again.";

/** Chrome and Firefox reject a runtime message with these texts when the
 * receiving end (the background worker) is gone, and background rejects with
 * its own daemon-disconnected text. Any other rejection is a different
 * failure and must not be reported as a lost connection. */
const CONNECTION_LOST_PATTERN = /message channel closed|message port closed|receiving end does not exist|daemon.*(?:disconnect|unavailable)/i;

function errorFromResponse(value: unknown): string {
  if (isRecord(value) && value["error"] === "connection_lost") return CONNECTION_LOST_COPY;
  if (isRecord(value) && value["error"] === "internal") {
    return typeof value["message"] === "string" ? value["message"] : RUNTIME_FAILURE_COPY;
  }
  if (isRecord(value) && isRecord(value["error"]) && typeof value["error"]["message"] === "string") {
    return value["error"]["code"] === "connection_lost" ? CONNECTION_LOST_COPY : value["error"]["message"];
  }
  return "The extension runtime did not return a usable response.";
}

function errorCode(value: unknown): string | undefined {
  if (!isRecord(value)) return undefined;
  if (typeof value["error"] === "string") return value["error"];
  if (isRecord(value["error"]) && typeof value["error"]["code"] === "string") return value["error"]["code"];
  return undefined;
}

function isConnectionLost(value: unknown): boolean {
  return errorCode(value) === "connection_lost" || (value instanceof Error && CONNECTION_LOST_PATTERN.test(value.message));
}

/** A thrown failure is a connection loss only when `isConnectionLost` says so —
 * the same predicate that gates retry scheduling, so copy and retry can never
 * disagree. Everything else says what actually failed rather than fabricating a
 * transport cause the page cannot observe. Untrusted thrown text is bounded
 * exactly the way `boundedProse` bounds runtime error copy in
 * inbox.ts: whitespace collapsed, capped at 240 characters. A thrown non-Error
 * (or an empty message) carries no trustworthy detail at all. */
function thrownErrorMessage(error: unknown): string {
  if (isConnectionLost(error)) return CONNECTION_LOST_COPY;
  if (!(error instanceof Error) || typeof error.message !== "string") return RUNTIME_FAILURE_COPY;
  const message = error.message.replace(/\s+/g, " ").trim();
  if (message === "") return RUNTIME_FAILURE_COPY;
  return `papio could not complete that request: ${message.length <= 240 ? message : `${message.slice(0, 237)}…`}`;
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

const STATE_STORAGE_KEY = "papio_state_v1";

function installConnectionStatusListener(): void {
  const storage = chrome.storage;
  if (storage === undefined || storage.onChanged === undefined) return;
  const sessionArea = storage.session;
  const stateArea = sessionArea ?? storage.local;
  const stateAreaName = stateArea === sessionArea ? "session" : "local";
  storage.onChanged.addListener((changes, areaName) => {
    if (areaName !== stateAreaName || state.statusError !== CONNECTION_LOST_COPY) return;
    const change = changes[STATE_STORAGE_KEY];
    if (!isRecord(change) || !isRecord(change["oldValue"]) || !isRecord(change["newValue"])) return;
    if (
      change["oldValue"]["connectionStatus"] === "connected" ||
      change["newValue"]["connectionStatus"] !== "connected"
    ) {
      return;
    }
    statusRetryPending = false;
    void loadStatus();
  });
}

function isDetectedPaper(value: unknown): value is DetectedPaper {
  if (!isRecord(value) || typeof value["localId"] !== "string" || typeof value["label"] !== "string") return false;
  if (typeof value["occurrences"] !== "number" || value["detector"] !== "generic-identifiers/1") return false;
  if (value["kind"] === "pdf_grab") {
    return (value["url"] === undefined || typeof value["url"] === "string") && typeof value["title"] === "string";
  }
  const identifier = value["identifier"];
  if (!isRecord(identifier) || typeof identifier["value"] !== "string") return false;
  const kind = identifier["kind"];
  return kind === "doi" || kind === "pmid" || kind === "arxiv" || kind === "openalex";
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
    status === "ownership_unknown" ||
    status === "invalid" ||
    status === "frame_too_large";
  if (!validStatus) return false;
  if (status !== "invalid" && status !== "frame_too_large" && typeof value["canonical_key"] !== "string") return false;
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
): value is {
  ok: true;
  mode: "v1" | "v2";
  processed_count: number;
  submitted: number;
  joined: number;
  already_owned: number;
  invalid: number;
  batch_id: string;
} {
  if (!isRecord(value) || value["ok"] !== true) return false;
  const mode = value["mode"];
  const processed = value["processed_count"];
  return (
    (mode === "v1" || mode === "v2") &&
    typeof processed === "number" &&
    Number.isSafeInteger(processed) &&
    processed >= 0 && processed <= MAX_MANIFEST &&
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
  return snapshot.items.map((item) => {
    const grab = item.kind === "pdf_grab";
    const record = item as unknown as Record<string, unknown>;
    const grabObject = isRecord(record["grab"]) ? record["grab"] : null;
    const grabID =
      typeof record["grab_id"] === "string" ? record["grab_id"] :
      grabObject !== null && typeof grabObject["grab_id"] === "string" ? grabObject["grab_id"] : null;
    return {
      localId: item.localId,
      kind: grab ? "pdf_grab" : "identifier",
      identifier: grab ? null : item.identifier,
      label: item.label,
      occurrences: item.occurrences,
      grabURL: grab ? item.url ?? null : null,
      grabTitle: grab ? item.title ?? null : null,
      grabID,
      status: null,
      canonicalKey: null,
      jobId: null,
      selected: false,
      submitted: false,
    };
  });
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
  statusRetryAttempted: false,
  rescanning: false,
  submitting: false,
  result: null,
  grabState: "idle",
  grabID: null,
  grabDetail: null,
  allowlistStored: false,
  allowlistPending: false,
  allowlistError: null,
  rescanRefusal: null,
  rescanSourceChanged: false,
};

function eligibleRows(): RowState[] {
  return state.rows.filter((row) => row.kind === "identifier" && !row.submitted && isEligibleStatus(row.status));
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
  if (state.rescanRefusal !== null) {
    elements.statusError.hidden = false;
    elements.statusErrorMessage.textContent = state.rescanRefusal;
    elements.statusRetryButton.hidden = true;
  } else {
    elements.statusError.hidden = state.statusError === null;
    elements.statusErrorMessage.textContent = state.statusError ?? "";
    elements.statusRetryButton.hidden = false;
    // A Rescan in flight is about to replace state.rows outright (Decision 4);
    // a Retry click racing that swap would send a status request keyed to rows
    // that are seconds from being discarded, so gate it the same way the
    // Rescan button itself is gated.
    elements.statusRetryButton.disabled = state.rescanning || state.snapshot === null;
  }
  elements.ownershipNote.hidden = !ownershipUnclearOnly();
}

function renderAllowlistRow(): void {
  if (elements === null) return;
  elements.allowlistCheckbox.disabled = state.allowlistPending || state.snapshot === null;
  elements.allowlistCheckbox.checked = state.allowlistStored;
  if (state.allowlistError === null) {
    elements.allowlistMessage.hidden = true;
    elements.allowlistMessage.textContent = "";
  } else {
    elements.allowlistMessage.hidden = false;
    elements.allowlistMessage.textContent = state.allowlistError;
  }
}

async function commitAllowlistChange(requested: boolean): Promise<void> {
  if (elements === null || state.snapshot === null || state.allowlistPending) return;
  state.allowlistPending = true;
  state.allowlistError = null;
  renderAllowlistRow();
  let response: unknown;
  try {
    response = await runtimeMessage("papio.pageBulk.allowlist.set", {
      origin: state.snapshot.sourceOrigin,
      allowed: requested,
    });
  } catch (error) {
    state.allowlistPending = false;
    state.allowlistError = thrownErrorMessage(error);
    renderAllowlistRow();
    return;
  }
  state.allowlistPending = false;
  const parsed = responseValue<boolean>(response, "allowed");
  if (!parsed.ok) {
    state.allowlistError = parsed.message;
    renderAllowlistRow();
    return;
  }
  state.allowlistStored = parsed.value;
  state.allowlistError = null;
  if (parsed.value) state.rescanRefusal = null;
  renderAllowlistRow();
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
  openalex: (encoded) => `https://openalex.org/works/${encoded}`,
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
  /(?:\b(?:doi|arxiv|pmid|openalex)\s*:\s*|https?:\/\/(?:dx\.)?doi\.org\/|https?:\/\/arxiv\.org\/abs\/|https?:\/\/(?:www\.|api\.)?openalex\.org\/(?:works\/)?)\s*$/i;

/** Remove an identifier already repeated in a page-derived display label. */
function displayLabel(row: RowState): string {
  if (row.kind === "pdf_grab" || row.identifier === null) return row.label;
  const label = row.label;
  const value = row.identifier.value;
  if (value === "") return label;
  const at = label.toLowerCase().indexOf(value.toLowerCase());
  if (at < 0) return label;
  let start = at;
  let end = at + value.length;
  const prefix = IDENTIFIER_PREFIX_RE.exec(label.slice(0, start));
  if (prefix !== null) start -= prefix[0].length;
  const opener = label[start - 1];
  const closer = label[end];
  if ((opener === "(" && closer === ")") || (opener === "[" && closer === "]")) {
    start -= 1;
    end += 1;
  }
  const trimmed = `${label.slice(0, start)}${label.slice(end)}`
    .replace(/\s+/g, " ")
    .replace(/^[\s.,;:|·\u2013\u2014-]+|[\s.,;:|·\u2013\u2014-]+$/g, "");
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

function grabSupported(): boolean {
  const downloads = typeof chrome !== "undefined" ? chrome.downloads : undefined;
  const steering = downloads !== undefined &&
    (downloads as unknown as { onDeterminingFilename?: unknown }).onDeterminingFilename !== undefined;
  return steering && state.snapshot?.pdfGrabAvailable === true;
}

async function handleGrab(row: RowState): Promise<void> {
  if (row.kind !== "pdf_grab" || state.snapshot === null || !grabSupported() || state.grabState !== "idle") return;
  state.grabState = "grabbed";
  state.grabDetail = null;
  render();
  try {
    const response = await runtimeMessage("papio.pageBulk.grabPdf", {
      tab_id: state.snapshot.sourceTabId,
      scan_id: state.scanId,
      ...(row.grabURL !== null ? { url: row.grabURL } : {}),
      title: row.grabTitle ?? row.label,
    });
    if (isRecord(response) && response["ok"] === true && typeof response["grab_id"] === "string") {
      state.grabID = response["grab_id"];
      render();
      return;
    }
    state.grabState = "failed";
    state.grabDetail = errorFromResponse(response);
  } catch (error) {
    state.grabState = "failed";
    state.grabDetail = thrownErrorMessage(error);
  }
  render();
}

function buildRow(row: RowState, ownershipCollapsed: boolean): HTMLElement {
  const wrapper = element("div");
  wrapper.className = "pb-row";
  wrapper.dataset.localId = row.localId;
  wrapper.dataset.status = row.kind === "pdf_grab" ? state.grabState : (row.submitted ? "submitted" : (row.status ?? "pending"));
  wrapper.dataset.disabled = String(row.kind === "pdf_grab" ? !grabSupported() : !isRowCheckable(row));
  if (row.kind === "pdf_grab") {
    const content = element("div");
    content.className = "pb-row-content";
    content.append(element("div", row.grabTitle ?? row.label));
    const meta = element("div");
    meta.className = "pb-row-meta";
    const host = (() => {
      try { return new URL(row.grabURL ?? "").hostname; } catch { return "the open PDF"; }
    })();
    meta.append(element("span", host));
    const canReacquire = state.snapshot !== null && state.snapshot.sourceTabId >= 0;
    const statusText =
      state.grabState === "idle" && row.grabURL === null && !canReacquire ? "Reopen or rescan the PDF tab to grab it" :
      state.grabState === "idle" ? (grabSupported() ? "Ready to grab" : "PDF grabbing needs Chrome download steering and a compatible daemon") :
      state.grabState === "grabbed" ? "Grabbed" :
      state.grabState === "identifying" ? "Identifying…" :
      state.grabState === "job_created" ? "Job created" :
      state.grabState === "already_owned" ? "Already in your library" :
      state.grabState === "needs_identifier" ? "Needs an identifier" :
      state.grabDetail ?? "Grab failed";
    meta.append(element("span", statusText));
    const button = element("button", "Grab this PDF");
    button.type = "button";
    button.disabled = !grabSupported() || state.grabState !== "idle" || (!canReacquire && row.grabURL === null);
    button.addEventListener("click", () => { void handleGrab(row); });
    content.append(meta, button);
    wrapper.append(content);
    return wrapper;
  }
  const labelText = displayLabel(row);
  const checkboxId = `pb-row-check-${row.localId}`;
  const checkbox = element("input");
  checkbox.type = "checkbox";
  checkbox.id = checkboxId;
  checkbox.checked = row.selected;
  checkbox.disabled = !isRowCheckable(row);
  checkbox.setAttribute("aria-label", `Select ${labelText}`);
  checkbox.addEventListener("change", () => { row.selected = checkbox.checked; render(); });
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
  const kind = element("span", `${KIND_LABEL[row.identifier!.kind]}:`);
  kind.className = "pb-row-kind";
  identifier.append(kind, document.createTextNode(" "));
  const url = identifierURL(row.identifier!);
  if (url === null) identifier.append(document.createTextNode(row.identifier!.value));
  else {
    const link = element("a", row.identifier!.value);
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
    elements.primaryButton.textContent = !state.statusLoaded
      ? "Acquire papers — checking availability…"
      : `Acquire all ${eligible.length} eligible`;
    elements.primaryButton.disabled = state.submitting || !state.statusLoaded || eligible.length === 0;
    elements.submitStatus.textContent = "";
    return;
  }
  elements.primaryButton.textContent = `Acquire ${selected.length} selected`;
  elements.primaryButton.disabled = state.submitting || !state.statusLoaded;
  elements.submitStatus.textContent = "";
}

function renderActionBar(): void {
  if (elements === null) return;
  const haveSnapshot = state.snapshot !== null && !state.expired;
  elements.actionBar.hidden = !haveSnapshot;
  elements.returnButton.hidden = state.sourceTabClosed;
  elements.returnButton.disabled = state.snapshot === null;
  elements.sourceClosedNote.hidden = !state.sourceTabClosed;
  elements.rescanButton.hidden = state.sourceTabClosed || state.rescanSourceChanged;
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
  const parts = [
    `${result.submitted} submitted`,
    `${result.joined} joined`,
    `${result.alreadyOwned} already owned`,
    `${result.invalid} invalid`,
  ];
  const summary = element("p", parts.join(" · "));
  if (result.mode === "v1") {
    summary.appendChild(element("span", " · Progress covers this 50-item submission"));
  }
  const link = element("a", "Open inbox");
  link.href = typeof chrome !== "undefined" ? chrome.runtime.getURL(inboxPagePath()) : "inbox.html";
  link.target = "_blank";
  link.rel = "noopener noreferrer";
  elements.resultSummary.hidden = false;
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
  renderAllowlistRow();
}
// -----------------------------------------------------------------------

async function loadAllowlist(origin: string): Promise<void> {
  const response = await runtimeMessage("papio.pageBulk.allowlist.get", { origin });
  const parsed = responseValue<boolean>(response, "allowed");
  if (parsed.ok) {
    state.allowlistStored = parsed.value;
    if (elements !== null) elements.allowlistCheckbox.checked = parsed.value;
  }
  renderAllowlistRow();
}

async function loadGrabStatus(): Promise<void> {
  const grabRow = state.rows.find((row) => row.kind === "pdf_grab" && row.grabID !== null);
  if (grabRow?.grabID === null || grabRow === undefined) return;
  let response: unknown;
  try {
    response = await runtimeMessage("papio.pageBulk.grabStatus", { grab_id: grabRow.grabID });
  } catch (error) {
    state.grabState = "identifying";
    state.grabDetail = thrownErrorMessage(error);
    return;
  }
  if (!isRecord(response) || response["ok"] !== true) {
    state.grabState = "identifying";
    state.grabDetail = errorFromResponse(response);
    return;
  }
  if (response["outcome"] === "not_found") {
    state.grabID = null;
    state.grabState = "idle";
    state.grabDetail = "This PDF grab is no longer available.";
    return;
  }
  const durable = durablePdfGrabState(response["state"]);
  if (durable === null) {
    state.grabState = "identifying";
    return;
  }
  if (durable === "abandoned") {
    state.grabID = null;
    state.grabState = "idle";
    state.grabDetail = typeof response["detail"] === "string" ? response["detail"] : "The PDF grab download was abandoned";
    return;
  }
  state.grabID = typeof response["grab_id"] === "string" ? response["grab_id"] : grabRow.grabID;
  state.grabState = durable;
  state.grabDetail = typeof response["detail"] === "string" ? response["detail"] : null;
}

let statusRetryPending = false;

function scheduleStatusRetry(requestGeneration: number): void {
  if (state.statusRetryAttempted) return;
  state.statusRetryAttempted = true;
  statusRetryPending = true;
  setTimeout(() => {
    if (!statusRetryPending) return;
    statusRetryPending = false;
    if (state.snapshot?.documentGeneration !== requestGeneration) return;
    void loadStatus();
  }, 500);
}

async function loadStatus(): Promise<void> {
  if (state.snapshot === null) return;
  statusRetryPending = false;
  const requestGeneration = state.snapshot.documentGeneration;
  state.statusError = null;
  render();
  await loadGrabStatus();
  if (state.grabDetail === CONNECTION_LOST_COPY) {
    state.statusError = CONNECTION_LOST_COPY;
    scheduleStatusRetry(requestGeneration);
    render();
    return;
  }
  if (state.rows.length === 0 || state.rows.every((row) => row.kind === "pdf_grab")) {
    state.statusLoaded = true;
    render();
    return;
  }
  const identifiers: PageBulkIdentifier[] = state.rows
    .filter((row): row is RowState & { identifier: DetectedPaper["identifier"] } => row.kind === "identifier" && row.identifier !== null)
    .map((row) => ({ local_id: row.localId, kind: row.identifier.kind, value: row.identifier.value }));
  let response: unknown;
  try {
    response = await runtimeMessage("papio.pageBulk.status", {
      scan_id: state.snapshot.scanId,
      identifiers,
      ...(state.snapshot.renderedRecordCountHint !== null ? { rendered_record_count_hint: state.snapshot.renderedRecordCountHint } : {}),
    });
  } catch (error) {
    if (state.snapshot === null || state.snapshot.documentGeneration !== requestGeneration) return;
    state.statusError = thrownErrorMessage(error);
    if (isConnectionLost(error)) scheduleStatusRetry(requestGeneration);
    render();
    return;
  }
  if (state.snapshot === null || state.snapshot.documentGeneration !== requestGeneration) return;
  if (!isStatusReply(response)) {
    state.statusError = errorFromResponse(response);
    if (errorCode(response) === "connection_lost") scheduleStatusRetry(requestGeneration);
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
  state.statusRetryAttempted = false;
  statusRetryPending = false;
  state.statusError = null;
  state.sourceTabClosed = false;
  state.expired = false;
  state.loadError = null;
  state.result = null;
  state.grabState = "idle";
  state.grabID = null;
  state.grabDetail = null;
  state.rescanRefusal = null;
  state.rescanSourceChanged = false;
  const grabRow = state.rows.find((row) => row.kind === "pdf_grab" && row.grabID !== null);
  if (grabRow?.grabID !== null && grabRow !== undefined) {
    state.grabID = grabRow.grabID;
    const item = snapshot.items.find((candidate) => candidate.localId === grabRow.localId);
    const record = item as unknown as Record<string, unknown>;
    const grabObject = isRecord(record["grab"]) ? record["grab"] : null;
    const durable = durablePdfGrabState(grabObject?.["state"] ?? record["grab_state"]);
    if (durable === "abandoned") {
      state.grabID = null;
      state.grabState = "idle";
    } else if (durable !== null) {
      state.grabState = durable;
    }
    if (typeof record["grab_detail"] === "string") state.grabDetail = record["grab_detail"];
  }
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
    state.loadError = thrownErrorMessage(e);
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
  state.rescanRefusal = null;
  state.rescanSourceChanged = false;
  render();
  let response: unknown;
  try {
    response = await runtimeMessage("papio.pageBulk.rescan", { scan_id: state.snapshot.scanId });
  } catch (e) {
    state.rescanning = false;
    state.loadError = thrownErrorMessage(e);
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
  if (errorCode(response) === "scanner_consent_required") {
    state.rescanRefusal =
      `${errorFromResponse(response)} Your selection snapshot is still here — allow scanning below, then choose Rescan again.`;
    render();
    return;
  }
  if (errorCode(response) === "source_changed") {
    state.rescanRefusal = errorFromResponse(response);
    state.rescanSourceChanged = true;
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
  const canonicalKeys = targetRows
    .map((row) => row.canonicalKey)
    .filter((key): key is string => key !== null)
    .slice(0, MAX_MANIFEST);
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
    elements.submitStatus.textContent = thrownErrorMessage(e);
    render();
    return;
  }
  state.submitting = false;
  if (!isSubmitReply(response)) {
    elements.submitStatus.textContent = errorFromResponse(response);
    render();
    return;
  }
  const processedCount = Math.min(response.processed_count, canonicalKeys.length);
  for (const row of targetRows.slice(0, processedCount)) {
    row.submitted = true;
    row.selected = false;
  }
  state.result = {
    mode: response.mode,
    processedCount,
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
  const allowlistMessage = document.getElementById("allowlist-message");
  if (
    scanTitle === null || scanMeta === null || scanSummary === null || scanError === null || scanExpired === null ||
    statusError === null || statusErrorMessage === null || statusRetryButton === null || truncatedNote === null ||
    ownershipNote === null || workspaceMain === null || rows === null || emptyState === null || actionBar === null ||
    returnButton === null || sourceClosedNote === null || rescanButton === null || primaryButton === null ||
    submitStatus === null || resultSummary === null || allowlistCheckbox === null || allowlistMessage === null
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
    statusRetryButton: statusRetryButton as HTMLButtonElement,
    truncatedNote,
    ownershipNote,
    workspaceMain,
    rows,
    emptyState,
    actionBar,
    returnButton: returnButton as HTMLButtonElement,
    sourceClosedNote,
    rescanButton: rescanButton as HTMLButtonElement,
    primaryButton: primaryButton as HTMLButtonElement,
    submitStatus,
    resultSummary,
    allowlistCheckbox: allowlistCheckbox as HTMLInputElement,
    allowlistMessage,
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
    if (elements === null || state.snapshot === null || state.allowlistPending) return;
    const requested = elements.allowlistCheckbox.checked;
    elements.allowlistCheckbox.checked = state.allowlistStored;
    void commitAllowlistChange(requested);
  });
  chrome.runtime?.onMessage?.addListener((message: unknown) => {
    if (!isRecord(message) || message["type"] !== "papio.pageBulk.grabState") return;
    const scanID = message["scan_id"];
    if (typeof scanID !== "string" || scanID !== state.scanId) return;
    const grabID = message["grab_id"];
    if (typeof grabID !== "string" || (state.grabID !== null && state.grabID !== grabID)) return;
    if (state.grabID === null) state.grabID = grabID;
    const next = message["state"];
    if (next === "abandoned") {
      state.grabID = null;
      state.grabState = "idle";
      state.grabDetail = typeof message["detail"] === "string" ? message["detail"] : "The PDF grab download was abandoned";
      render();
      return;
    }
    if (next === "grabbed" || next === "identifying" || next === "job_created" || next === "already_owned" || next === "needs_identifier" || next === "failed") {
      state.grabState = next;
      state.grabDetail = typeof message["detail"] === "string" ? message["detail"] : null;
      render();
    }
  });
  installConnectionStatusListener();
  void loadInitial();
}

if (typeof document !== "undefined") {
  if (document.getElementById("rows") !== null) bootstrap();
  else document.addEventListener("DOMContentLoaded", bootstrap, { once: true });
}
