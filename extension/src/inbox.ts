// Copyright 2026 OrgMentem. Licensed under MIT.

import { derivePulseDisplay, pulseIsUnmeasured, requestWorkPulse, type PopupPulseCache } from "./popup";
import { chromeBackend, getSuccessAckMode, type SuccessAckMode } from "./state";
import { PDF_GRAB_SUGGEST_FEATURE } from "./deliver";
import type { ActivityEntryPayload, TriageCounts, TriageDelivery, TriageSnapshotItem, TriageSnapshotResponsePayload } from "./protocol";

type Snapshot = Omit<TriageSnapshotResponsePayload, "request_id">;
type ActivityEntry = ActivityEntryPayload;
type TriageOperation = TriageSnapshotItem["ops"][number];
type Verdict = "accept" | "reject";

type CitationStyle = "apa" | "mla" | "chicago";
type InboxTab = "actions" | "watch" | "activity";

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
  pulse: HTMLElement;
  filterInput: HTMLInputElement;
  refresh: HTMLButtonElement;
  reconnect: HTMLButtonElement;
  list: HTMLElement;
  watchList: HTMLElement;
  actionsPanel: HTMLElement;
  watchPanel: HTMLElement;
  activityPanel: HTMLElement;
  actionsTab: HTMLButtonElement;
  watchTab: HTMLButtonElement;
  activityTab: HTMLButtonElement;
  activityList: HTMLElement;
  activityNew: HTMLElement;
  activityShowMore: HTMLButtonElement;
  operationStatus: HTMLElement;
  generatedAt: HTMLTimeElement;
  loadMore: HTMLButtonElement;
  dialog: HTMLElement;
  dialogMessage: HTMLElement;
  dialogCancel: HTMLButtonElement;
  dialogConfirm: HTMLButtonElement;
  citationStyle: HTMLSelectElement;
  undoBar: HTMLElement;
  undoMessage: HTMLElement;
  undoButton: HTMLButtonElement;
}
let elements: PageElements | null = null;

interface Confirmation {
  itemID: string;
  verdict: Verdict;
  returnFocus: HTMLElement | null;
}

type StatusTone = "actionable" | "continuing" | "degraded" | "danger" | "neutral";

interface FeedbackNotice {
  text: string;
  deadline: number;
  count: number;
  phase: "enter" | "visible" | "exit";
}

interface QueuedFeedback {
  text: string;
  count: number;
}


// One dismissal waiting out its undo window. The item is kept whole so an undo
// can put the exact row back without a round trip.
interface PendingDismissal {
  item: TriageSnapshotItem;
  cancelsJob: boolean;
}

/** One document's identifier, read out of its own embedded metadata by the
 * daemon (never compared against a candidate — see extractDocumentIdentifiers
 * in internal/browser/bridge.go); shown so the operator's `papio grabs
 * identify` retype is copy-ready instead of hunted for. */
interface GrabDocumentIdentifier {
  kind: string;
  value: string;
  source: string;
}

/** One candidate-eligible job scored against the parked grab's bytes by the
 * daemon's production QualifyCandidate — the same predicate autonomous
 * binding uses, just run against every candidate instead of stopping at the
 * first unambiguous one. */
interface GrabSuggestionRow {
  job_id: string;
  title?: string;
  year?: number;
  doi?: string;
  verdict: "qualifies" | "review" | "rejected";
  reason?: string;
  evidence: string[];
}

/** Ranked-picker state for one pdf_grab row, fetched on click and never
 * persisted: grabs.suggest recomputes the candidate pool fresh on every
 * call, so a cached list would name a job the pool has since filed or
 * abandoned. */
interface GrabPickerState {
  status: "loading" | "loaded";
  outcome?: string;
  detail?: string;
  documentIdentifiers: GrabDocumentIdentifier[];
  suggestions: GrabSuggestionRow[];
  truncated: boolean;
  /** Set after a confirm response whose outcome must stay visible without
   * clearing the picker — refused_identity above all: the pick was not
   * applied and this is the only place that says so. */
  confirmNotice?: { text: string; tone: "info" | "error" };
  /** job_id of a confirm currently in flight, so only that suggestion's
   * button shows a busy state and a second click cannot double-fire. */
  confirmingJobID?: string;
}

interface PageState {
  snapshot: Snapshot | null;
  counts: TriageCounts | null;
  pulse: PopupPulseCache | undefined;
  successAckMode: SuccessAckMode;
  generatedAt: string | null;
  connected: boolean;
  connectionMessage: string;
  /** The daemon answered and refused: another browser holds its session. No
   * amount of reconnecting fixes that, so the banner must not promise it. */
  connectionSessionElsewhere: boolean;
  /** False until the first probe settles. `connected` alone cannot express
   * "not asked yet", and its false default rendered as lost connectivity. */
  connectionKnown: boolean;
  /** True once the grace period has elapsed with connectivity still unknown, so
   * a genuinely slow daemon says "Connecting…" instead of leaving a blank page. */
  connectingVisible: boolean;
  selectedID: string | null;
  expandedItemIDs: Set<string>;
  pending: Set<string>;
  previewed: Set<string>;
  itemMessages: Map<string, { text: string; tone: "info" | "error" | "offline" }>;
  confirmation: Confirmation | null;
  dismissals: PendingDismissal[];
  undoDeadline: number | null;
  undoUndoable: boolean;
  dismissalCommitInFlight: boolean;
  feedbackNotice: FeedbackNotice | null;
  feedbackQueue: QueuedFeedback[];
  lastAnnounced: string | null;
  focusSelectionAfterRender: boolean;
  loading: boolean;
  filterQuery: string;
  citationStyle: CitationStyle;
  activeTab: InboxTab;
  activityFeature: boolean;
  activityKnown: boolean;
  activityEntries: ActivityEntry[];
  activityExpanded: boolean;
  activityNewCount: number;
  activityCursor: string | undefined;
  activityLatestSeq: number;
  activitySeenThroughSeq: number | null;
  activityGap: boolean;
  activityLimited: boolean;
  activityHasMore: boolean;
  waitingJobs: Map<string, number>;
  grabPickers: Map<string, GrabPickerState>;
  /** Features from the last hello_ack, read directly off the persisted
   * BrokerStore (state.ts) the way popup.ts already does — the
   * triage-snapshot RPC carries no capability information. Empty until the
   * first successful read, so the picker gate below always fails closed to
   * today's guidance text before this has ever loaded or when the daemon
   * has never negotiated. */
  daemonFeatures: string[];
}

type FocusTarget =
  | { kind: "item"; itemID: string; control?: string }
  | { kind: "activity"; jobID: string };

const state: PageState = {
  snapshot: null,
  counts: null,
  pulse: undefined,
  successAckMode: "all",
  generatedAt: null,
  connected: false,
  connectionMessage: "Connecting to daemon…",
  connectionSessionElsewhere: false,
  connectionKnown: false,
  connectingVisible: false,
  selectedID: null,
  expandedItemIDs: new Set(),
  pending: new Set(),
  previewed: new Set(),
  itemMessages: new Map(),
  confirmation: null,
  dismissals: [],
  undoDeadline: null,
  undoUndoable: false,
  dismissalCommitInFlight: false,
  feedbackNotice: null,
  feedbackQueue: [],
  lastAnnounced: null,
  focusSelectionAfterRender: false,
  loading: false,
  filterQuery: "",
  citationStyle: storedCitationStyle(),
  activeTab: "actions",
  activityFeature: false,
  activityKnown: false,
  activityEntries: [],
  activityExpanded: false,
  activityNewCount: 0,
  activityCursor: undefined,
  activityLatestSeq: 0,
  activitySeenThroughSeq: null,
  activityGap: false,
  activityLimited: false,
  activityHasMore: false,
  waitingJobs: new Map(),
  grabPickers: new Map(),
  daemonFeatures: [],
};

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function errorFromResponse(value: unknown): string {
  if (isRecord(value) && isRecord(value["error"]) && typeof value["error"]["message"] === "string") {
    return value["error"]["message"];
  }
  return "The daemon did not return a usable response.";
}

function responseValue<T>(value: unknown, key: string): { ok: true; value: T } | { ok: false; message: string; code?: string } {
  if (isRecord(value) && value["ok"] === true && key in value) {
    return { ok: true, value: value[key] as T };
  }
  const code =
    isRecord(value) && isRecord(value["error"]) && typeof value["error"]["code"] === "string"
      ? value["error"]["code"]
      : undefined;
  return { ok: false, message: errorFromResponse(value), ...(code === undefined ? {} : { code }) };
}
function waitingSibling(item: TriageSnapshotItem): boolean {
  return typeof item.job_id === "string" && state.waitingJobs.has(item.job_id);
}

let waitingOverlayTimer: number | Timer | undefined;

function scheduleWaitingOverlayExpiry(): void {
  clearTimeout(waitingOverlayTimer);
  const deadlines = [...state.waitingJobs.values()];
  const next = deadlines.length > 0 ? Math.min(...deadlines) : undefined;
  if (next === undefined) return;
  waitingOverlayTimer = setTimeout(() => {
    const now = Date.now();
    for (const [jobID, deadline] of state.waitingJobs) {
      if (deadline <= now) state.waitingJobs.delete(jobID);
    }
    render();
    scheduleWaitingOverlayExpiry();
  }, Math.max(0, next - Date.now()));
}

const ACTIVITY_SEEN_THROUGH_KEY = "activity_seen_through_seq";

async function loadActivityWatermark(): Promise<number> {
  try {
    if (typeof chrome !== "undefined" && chrome.storage?.local) {
      const got = await chrome.storage.local.get(ACTIVITY_SEEN_THROUGH_KEY);
      const value = got[ACTIVITY_SEEN_THROUGH_KEY];
      if (typeof value === "number" && Number.isSafeInteger(value) && value >= 0) return value;
      if (typeof value === "string" && /^[0-9]+$/u.test(value)) return Number(value);
    }
  } catch {
    // Browser storage may be unavailable in private/test contexts.
  }
  return 0;
}

async function persistActivityWatermark(value: number): Promise<void> {
  try {
    if (typeof chrome !== "undefined" && chrome.storage?.local) {
      await chrome.storage.local.set({ [ACTIVITY_SEEN_THROUGH_KEY]: value });
    }
  } catch {
    // Read state is best effort; retain the in-memory watermark.
  }
}
async function runtimeMessage(type: string, request: Record<string, unknown>): Promise<unknown> {
  if (typeof chrome === "undefined" || !chrome.runtime?.sendMessage) {
    throw new Error("The extension runtime is unavailable.");
  }
  return chrome.runtime.sendMessage({ type, request });
}

async function loadSuccessAckMode(): Promise<void> {
  if (typeof chrome === "undefined" || chrome.storage?.local === undefined) return;
  state.successAckMode = await getSuccessAckMode(chrome.storage.local);
  render();
}

async function loadDaemonFeatures(): Promise<void> {
  if (typeof chrome === "undefined" || chrome.storage === undefined) return;
  try {
    const store = await chromeBackend(chrome.storage).load();
    state.daemonFeatures = store.daemonFeatures ?? [];
  } catch {
    // Storage may be unavailable in private/test contexts; keep whatever
    // was last learned rather than silently disabling the picker.
  }
}

// Both halves must hold before the picker replaces today's guidance text: a
// stale feature list surviving a disconnect (the persisted store is cleared
// only at the START of the next hello, not the moment the port drops) must
// not by itself promise a picker the daemon just stopped answering to — see
// deliver.ts's sendPdfState, which gates the same way on its own surface.
function grabPickerAvailable(): boolean {
  return state.connected && state.daemonFeatures.includes(PDF_GRAB_SUGGEST_FEATURE);
}

// A disconnect is usually the daemon's own port healing (extension reload,
// SW nap, brief restart) — the background worker already reconnects with its
// own backoff. Mirror that here so the banner clears itself instead of
// leaving the user staring at a stale error until they click Reconnect.
let reconnectToken = 0;
let reconnectScheduled = false;
let reconnectAttempts = 0;
const RECONNECT_DELAYS_MS = [1000, 2000, 4000, 8000, 15000];

// Opening the inbox asks the daemon a question; until it answers, connectivity
// is unknown. Saying "Connecting…" straight away is a flash on every open,
// because the answer normally beats the eye — but staying blank forever hides a
// daemon that is genuinely slow. So: silence, then the neutral line.
const CONNECTING_GRACE_MS = 750;
let connectionGrace: number | Timer | undefined;

function startConnectionGrace(): void {
  if (connectionGrace !== undefined || state.connectionKnown) return;
  connectionGrace = setTimeout(() => {
    connectionGrace = undefined;
    if (state.connectionKnown) return;
    state.connectingVisible = true;
    render();
  }, CONNECTING_GRACE_MS);
}

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

function setConnection(connected: boolean, message: string, code?: string): void {
  const wasConnected = state.connected;
  state.connected = connected;
  // Until the first probe settles, connectivity is unknown rather than lost:
  // rendering the initial "not connected" default flashed a red "Disconnected"
  // banner on every open, before anything had been asked of the daemon.
  state.connectionKnown = true;
  if (connectionGrace !== undefined) {
    clearTimeout(connectionGrace);
    connectionGrace = undefined;
  }
  state.connectionMessage = message;
  state.connectionSessionElsewhere = !connected && code === "session_busy";
  if (connected) {
    cancelAutoReconnect();
    if (!wasConnected) clearOfflineItemMessages();
  } else {
    scheduleAutoReconnect();
  }
}
function normalizeSnapshotItem(item: TriageSnapshotItem): TriageSnapshotItem {
  if (item.kind !== "pdf_grab" || item.grab === undefined) return item;
  return {
    ...item,
    id: `pdf_grab:${item.grab.grab_id}`,
    rank: 0,
    title: item.label ?? "PDF",
    facts: [],
    links: [],
  };
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
    pdf_grab: 3,
  };
  const query = state.filterQuery.trim().toLowerCase();
  return [...state.snapshot.items]
    .filter((item) => matchesFilter(item, query))
    .sort((left, right) => classRank[left.kind] - classRank[right.kind] || left.rank - right.rank);
}

/** An https link and the host it names. The host is read off the daemon's own
 * URL and is never inferred: the inbox has no `challenge_host` and must not
 * imply it knows one. Display drops only a leading `www.`. */
function safeExternalLink(value: string): { url: string; host: string } | null {
  try {
    const url = new URL(value);
    return url.protocol === "https:" ? { url: url.href, host: url.hostname.replace(/^www\./u, "") } : null;
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
    const safe = safeExternalLink(link.url);
    if (safe !== null) return safe.url;
  }
  return null;
}

function openNewTab(url: string): void {
  window.open(url, "_blank", "noopener,noreferrer");
}
function announce(text: string): void {
  if (elements === null || text === state.lastAnnounced) return;
  state.lastAnnounced = text;
  elements.operationStatus.textContent = text;
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
function handoffJobID(item: TriageSnapshotItem): string | null {
  if (item.kind !== "human_action" || item.action_kind !== "openurl_handoff" || typeof item.job_id !== "string") return null;
  return /^[A-Za-z0-9_-]{8,128}$/.test(item.job_id) ? item.job_id : null;
}
function manualDownloadJobID(item: TriageSnapshotItem): string | null {
  if (item.kind !== "human_action" || item.action_kind !== "manual_download" || typeof item.job_id !== "string") return null;
  return /^[A-Za-z0-9_-]{8,128}$/.test(item.job_id) ? item.job_id : null;
}


/** One-line bound for text papio did not author: collapse whitespace, cap the
 * length, and mark a truncation with an ellipsis. Every surface that prints
 * untrusted daemon or runtime prose goes through this. */
function boundedProse(text: string, limit: number): string {
  const message = text.replace(/\s+/g, " ").trim();
  return message.length <= limit ? message : `${message.slice(0, limit - 3)}…`;
}

function openFailure(item: TriageSnapshotItem, value: unknown, tone: "error" | "offline" = "error"): void {
  operationMessage(item.id, boundedProse(errorFromResponse(value), 240), tone);
  render();
}

function isMutation(operation: TriageOperation): boolean {
  return (
    operation === "acquire" ||
    operation === "dismiss" ||
    operation === "accept" ||
    operation === "reject" ||
    operation === "confirm_request_exists" ||
    operation === "confirm_request_absent"
  );
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
    case "open_request_history":
      return "History";
    case "confirm_request_exists":
      return "Confirm exists";
    case "confirm_request_absent":
      return "Confirm absent";
    case "provide_identifier":
      return "Provide identifier";
  }
}
function itemGuidanceID(item: TriageSnapshotItem): string {
  return `item-guidance-${item.id.replace(/[^A-Za-z0-9_-]/g, "_")}`;
}


function element<K extends keyof HTMLElementTagNameMap>(tag: K, text?: string): HTMLElementTagNameMap[K] {
  const created = document.createElement(tag);
  if (text !== undefined) created.textContent = text;
  return created;
}

function operationMessage(itemID: string, text: string, tone: "info" | "error" | "offline" = "info"): void {
  const previous = state.itemMessages.get(itemID);
  state.itemMessages.set(itemID, { text, tone });
  if (previous?.text === text && previous.tone === tone) return;
  announce(text);
}
function activityResponse(value: unknown): {
  feature: boolean;
  entries: ActivityEntry[];
  hasMore: boolean;
  cursor?: string | undefined;
  latestSeq: number;
  newCountSince?: number | undefined;
  gap: boolean;
  paged: boolean;
} | null {
  if (!isRecord(value) || value["ok"] !== true || typeof value["feature"] !== "boolean") return null;
  if (value["feature"] === false) return { feature: false, entries: [], hasMore: false, latestSeq: 0, gap: false, paged: false };
  const rawEntries = value["entries"];
  if (!Array.isArray(rawEntries)) return null;
  const latestSeq: number = typeof value["latest_seq"] === "number" && Number.isSafeInteger(value["latest_seq"])
    ? value["latest_seq"] : rawEntries.reduce<number>((max, entry) => isActivityEntry(entry) ? Math.max(max, entry.seq) : max, 0);
  return {
    feature: true,
    entries: rawEntries.filter(isActivityEntry),
    hasMore: value["has_more"] === true,
    cursor: typeof value["cursor"] === "string" ? value["cursor"] : undefined,
    latestSeq,
    newCountSince: typeof value["new_count_since"] === "number" ? value["new_count_since"] : undefined,
    gap: value["gap"] === true,
    paged: "latest_seq" in value,
  };
}

function rowForItem(itemID: string): HTMLElement | null {
  if (elements === null) return null;
  const lists = [elements.list, elements.watchList];
  for (const list of lists) {
    const row = Array.from(list.querySelectorAll<HTMLElement>("[data-triage-item-id]"))
      .find((candidate) => candidate.dataset.triageItemId === itemID);
    if (row !== undefined) return row;
  }
  return null;
}
function itemsForTab(tab: InboxTab): TriageSnapshotItem[] {
  if (tab === "activity") return [];
  const items = orderedItems();
  if (tab === "watch") return items.filter((item) => item.kind === "watch_hit");
  return items.filter((item) => item.kind === "retraction" || item.kind === "human_action" || item.kind === "pdf_grab");
}

function isActivityEntry(value: unknown): value is ActivityEntry {
  if (!isRecord(value)) return false;
  return (
    typeof value["seq"] === "number" &&
    Number.isSafeInteger(value["seq"]) &&
    typeof value["at"] === "string" &&
    typeof value["kind"] === "string" &&
    typeof value["text"] === "string" &&
    (value["job_id"] === undefined || typeof value["job_id"] === "string") &&
    (value["title"] === undefined || typeof value["title"] === "string")
  );
}

function toGrabDocumentIdentifier(raw: unknown): GrabDocumentIdentifier | null {
  if (!isRecord(raw) || typeof raw["kind"] !== "string" || typeof raw["value"] !== "string" || typeof raw["source"] !== "string") {
    return null;
  }
  return { kind: raw["kind"], value: raw["value"], source: raw["source"] };
}

function toGrabSuggestionRow(raw: unknown): GrabSuggestionRow | null {
  if (!isRecord(raw) || typeof raw["job_id"] !== "string") return null;
  const verdict = raw["verdict"];
  if (verdict !== "qualifies" && verdict !== "review" && verdict !== "rejected") return null;
  const evidence = Array.isArray(raw["evidence"])
    ? raw["evidence"].filter((entry): entry is string => typeof entry === "string")
    : [];
  return {
    job_id: raw["job_id"],
    verdict,
    evidence,
    ...(typeof raw["title"] === "string" ? { title: raw["title"] } : {}),
    ...(typeof raw["year"] === "number" ? { year: raw["year"] } : {}),
    ...(typeof raw["doi"] === "string" ? { doi: raw["doi"] } : {}),
    ...(typeof raw["reason"] === "string" ? { reason: raw["reason"] } : {}),
  };
}


function relativeActivityTime(at: string): string {
  const timestamp = Date.parse(at);
  if (!Number.isFinite(timestamp)) return at;
  const seconds = Math.round((Date.now() - timestamp) / 1000);
  if (seconds < -45) {
    const minutes = Math.max(1, Math.round(-seconds / 60));
    return `in ${minutes}m`;
  }
  if (seconds < 45) return "just now";
  if (seconds < 90) return "1m ago";
  if (seconds < 3600) return `${Math.round(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.round(seconds / 3600)}h ago`;
  if (seconds < 172800) return "yesterday";
  return `${Math.round(seconds / 86400)}d ago`;
}

function itemJobID(item: TriageSnapshotItem): string | null {
  if (typeof item.job_id === "string" && item.job_id !== "") return item.job_id;
  const job = factText(item, "Job");
  return job === null ? null : job;
}

function activityStatusForJob(jobID: string): string | null {
  const entries = state.activityEntries
    .filter((entry) => entry.job_id === jobID)
    .sort((left, right) => right.seq - left.seq);
  for (const entry of entries) {
    if (entry.kind === "browser.download_started") return "downloading…";
    if (entry.kind === "browser.download_complete") return "adopted ✓ validating";
    if (entry.kind !== "job.transition") continue;
    const text = entry.text.toLowerCase();
    if (text.includes("validat") || text.includes("adopt")) return "adopted ✓ validating";
    if (text.includes("fetch") || text.includes("download")) return "downloading…";
    return "working…";
  }
  return null;
}

function actionForActivityJob(jobID: string): TriageSnapshotItem | null {
  return state.snapshot?.items.find((item) =>
    item.kind !== "watch_hit" && itemJobID(item) === jobID
  ) ?? null;
}

function shortActivityJobID(jobID: string): string {
  return jobID.length <= 8 ? jobID : jobID.slice(-8);
}

function focusActivityJob(jobID: string): void {
  const item = actionForActivityJob(jobID);
  if (item === null) {
    announce(`No Actions item found for job ${jobID}.`);
    return;
  }
  state.activeTab = "actions";
  state.selectedID = item.id;
  render();
  const row = rowForItem(item.id);
  if (row === null) {
    announce(`Inbox item for job ${jobID} is not visible with the current filter.`);
    return;
  }
  row.classList.add("activity-highlight");
  row.focus();
  row.scrollIntoView?.({ behavior: "smooth", block: "center" });
  announce(`Focused inbox item for job ${jobID}.`);
}
function captureFocusTarget(): FocusTarget | null {
  if (elements === null || typeof document === "undefined") return null;
  const active = document.activeElement;
  if (!(active instanceof HTMLElement)) return null;
  const itemRow = active.closest<HTMLElement>("[data-triage-item-id]");
  if (itemRow !== null && itemRow.dataset.triageItemId !== undefined) {
    const control = active instanceof HTMLButtonElement
      ? active.dataset.operation ?? active.dataset.label
      : undefined;
    if (control === undefined) return { kind: "item", itemID: itemRow.dataset.triageItemId };
    return { kind: "item", itemID: itemRow.dataset.triageItemId, control };
  }
  if (active instanceof HTMLButtonElement && active.classList.contains("activity-job-link")) {
    const jobID = active.dataset.activityJobId;
    if (jobID !== undefined) return { kind: "activity", jobID };
  }
  return null;
}


function restoreFocusTarget(target: FocusTarget | null): void {
  if (target === null || elements === null) return;
  if (target.kind === "activity") {
    const chip = Array.from(elements.activityList.querySelectorAll<HTMLButtonElement>(".activity-job-link"))
      .find((candidate) => candidate.dataset.activityJobId === target.jobID);
    chip?.focus();
    return;
  }
  const row = rowForItem(target.itemID);
  if (row === null) return;
  if (target.control !== undefined) {
    const control = Array.from(row.querySelectorAll<HTMLButtonElement>("button"))
      .find((candidate) => candidate.dataset.operation === target.control || candidate.dataset.label === target.control);
    if (control !== undefined) {
      control.focus();
      return;
    }
  }
  row.focus();
}

interface ActivityRow {
  entry: ActivityEntry;
  count: number;
}

interface ActivityGroup {
  jobID: string | null;
  title: string;
  rows: ActivityRow[];
  newestSeq: number;
}

function activityGroups(): ActivityGroup[] {
  const groups = new Map<string, ActivityGroup>();
  const entries = [...state.activityEntries].sort((left, right) => right.seq - left.seq || right.at.localeCompare(left.at));
  for (const entry of entries) {
    const jobID = typeof entry.job_id === "string" && entry.job_id !== "" ? entry.job_id : null;
    const key = jobID ?? "system";
    const actionTitle = jobID === null ? null : actionForActivityJob(jobID)?.title ?? null;
    let group = groups.get(key);
    if (group === undefined) {
      group = {
        jobID,
        title: jobID === null ? "System" : (entry.title !== undefined && entry.title !== "" ? entry.title : actionTitle ?? "Job activity"),
        rows: [],
        newestSeq: entry.seq,
      };
      groups.set(key, group);
    } else if (group.title === "Job activity" && entry.title !== undefined && entry.title !== "") {
      group.title = entry.title;
    }
    const previous = group.rows[group.rows.length - 1];
    if (previous !== undefined && previous.entry.text === entry.text) {
      previous.count += 1;
    } else {
      group.rows.push({ entry, count: 1 });
    }
  }
  return [...groups.values()].sort((left, right) => right.newestSeq - left.newestSeq);
}

function renderActivityRow(row: ActivityRow): HTMLElement {
  const item = element("li");
  item.className = "activity-entry";
  const time = element("time", relativeActivityTime(row.entry.at));
  time.className = "activity-time";
  time.dateTime = row.entry.at;
  const text = element("span", row.entry.text);
  text.className = "activity-text";
  item.append(time, text);
  if (row.count > 1) {
    const count = element("span", `×${row.count}`);
    count.className = "activity-count";
    count.setAttribute("aria-label", `${row.count} repeated entries`);
    item.append(count);
  }
  return item;
}

function renderActivityGroup(group: ActivityGroup, rows: ActivityRow[]): HTMLElement {
  const section = element("section");
  section.className = "activity-group";
  const heading = element("h3", group.title);
  if (group.jobID !== null) {
    const action = actionForActivityJob(group.jobID);
    const suffix = shortActivityJobID(group.jobID);
    if (action === null) {
      const chip = element("span", suffix);
      chip.className = "activity-job-chip";
      heading.append(chip);
    } else {
      const chip = element("button", suffix);
      chip.type = "button";
      chip.className = "activity-job-chip activity-job-link";
      chip.dataset.activityJobId = group.jobID;
      chip.setAttribute("aria-label", "Show matching Actions item");
      chip.addEventListener("click", () => focusActivityJob(group.jobID!));
      heading.append(chip);
    }
  }
  const list = element("ul");
  list.className = "activity-group-list";
  for (const row of rows) list.append(renderActivityRow(row));
  section.append(heading, list);
  return section;
}

function renderActivity(): void {
  if (elements === null) return;
  elements.activityList.replaceChildren();
  elements.activityNew.replaceChildren();
  elements.activityNew.hidden = true;
  elements.activityShowMore.hidden = true;
  if (!state.activityKnown) {
    elements.activityList.append(element("p", "Checking activity availability…"));
    return;
  }
  if (!state.activityFeature) {
    elements.activityList.append(element("p", "Activity is unavailable with this daemon. Upgrade papio to see recent activity."));
    return;
  }
  if (!state.activityLimited && !state.activityGap && state.activityNewCount > 0) {
    const count = Math.min(50, state.activityNewCount);
    const suffix = count === 1 ? "entry" : "entries";
    const affordance = element("button", `${count} new Activity ${suffix} — show recent activity`);
    affordance.type = "button";
    affordance.setAttribute("aria-label", `${count} new Activity ${suffix}`);
    affordance.addEventListener("click", () => {
      state.activityNewCount = 0;
      announce(`${count} new Activity ${suffix} available.`);
      render();
    });
    elements.activityNew.append(affordance);
    elements.activityNew.hidden = false;
  } else if (state.activityGap) {
    elements.activityNew.append(element("p", "Newer Activity is available; exact unread count is unavailable."));
    elements.activityNew.hidden = false;
  }
  if (state.activityLimited) elements.activityList.append(element("p", "Activity history is limited to the latest 50 entries with this daemon"));
  const groups = activityGroups();
  if (groups.length === 0) {
    elements.activityList.append(element("p", "No recent activity."));
    return;
  }
  const maxGroups = state.activityExpanded ? groups.length : 5;
  const maxRows = state.activityExpanded ? Number.POSITIVE_INFINITY : 15;
  let shownGroups = 0;
  let shownRows = 0;
  for (const group of groups) {
    if (shownGroups >= maxGroups || shownRows >= maxRows) break;
    const rows = group.rows.slice(0, Math.max(0, maxRows - shownRows));
    if (rows.length === 0) break;
    elements.activityList.append(renderActivityGroup(group, rows));
    shownGroups += 1;
    shownRows += rows.length;
  }
  const totalRows = groups.reduce((total, group) => total + group.rows.length, 0);
  const needsLocalExpansion = !state.activityExpanded && (groups.length > 5 || totalRows > 15);
  elements.activityShowMore.hidden = !(state.activityHasMore || needsLocalExpansion);
  if (
    state.activeTab === "activity" &&
    state.activityEntries.length > 0 &&
    (typeof document === "undefined" || document.visibilityState === "visible") &&
    !state.activityGap &&
    state.activitySeenThroughSeq !== null &&
    state.activityLatestSeq > state.activitySeenThroughSeq
  ) {
    state.activitySeenThroughSeq = state.activityLatestSeq;
    void persistActivityWatermark(state.activitySeenThroughSeq);
  }
}
function renderTabs(): void {
  if (elements === null) return;
  const tabs = [elements.actionsTab, elements.watchTab, elements.activityTab];
  // Tab labels carry the daemon's totals, not the loaded page size: the
  // snapshot streams 50 rows at a time, and watch rows load lazily, so
  // "Actions (50)" / "Watch hits (0)" both lied until now.
  const actionCount =
    state.counts === null
      ? itemsForTab("actions").length
      : state.counts.actions + state.counts.retractions;
  const watchCount = state.counts === null ? itemsForTab("watch").length : state.counts.watch_hits;
  elements.actionsTab.textContent = `Actions (${actionCount})`;
  elements.watchTab.textContent = `Watch hits (${watchCount})`;
  elements.activityTab.textContent = state.activityKnown && !state.activityFeature
    ? "Activity (unavailable)"
    : (!state.activityLimited && !state.activityGap && state.activityNewCount > 0
      ? `Activity (${Math.min(50, state.activityNewCount)} new)`
      : "Activity");
  elements.activityTab.hidden = false;
  for (const tab of tabs) {
    const selected = tab.dataset.tab === state.activeTab;
    tab.setAttribute("aria-selected", String(selected));
    tab.tabIndex = selected ? 0 : -1;
  }
  elements.actionsPanel.hidden = state.activeTab !== "actions";
  elements.watchPanel.hidden = state.activeTab !== "watch";
  elements.activityPanel.hidden = state.activeTab !== "activity";
  elements.filterInput.disabled = state.activeTab === "activity";
}

function selectTab(tab: InboxTab, focus: boolean): void {
  state.activeTab = tab;
  render();
  if (focus && elements !== null) {
    const button = tab === "actions" ? elements.actionsTab : tab === "watch" ? elements.watchTab : elements.activityTab;
    button.focus();
  }
}

function handleTabKeydown(event: KeyboardEvent): void {
  if (event.key !== "ArrowLeft" && event.key !== "ArrowRight" && event.key !== "Home" && event.key !== "End") return;
  const target = event.currentTarget;
  if (!(target instanceof HTMLButtonElement) || elements === null) return;
  const tabs = [elements.actionsTab, elements.watchTab, elements.activityTab];
  const current = tabs.indexOf(target);
  if (current < 0) return;
  let next = current;
  if (event.key === "ArrowLeft") next = (current + tabs.length - 1) % tabs.length;
  if (event.key === "ArrowRight") next = (current + 1) % tabs.length;
  if (event.key === "Home") next = 0;
  if (event.key === "End") next = tabs.length - 1;
  event.preventDefault();
  const tab = tabs[next];
  if (tab !== undefined) selectTab(tab.dataset.tab as InboxTab, true);
}

function updateRovingTabIndex(): void {
  if (elements === null) return;
  for (const list of [elements.list, elements.watchList]) {
    for (const row of Array.from(list.querySelectorAll<HTMLElement>("[data-triage-item-id]"))) {
      row.tabIndex = row.dataset.triageItemId === state.selectedID ? 0 : -1;
    }
  }
}

function selectItem(itemID: string, focus: boolean): void {
  const item = itemForID(itemID);
  if (item === null) return;
  const itemTab: InboxTab = item.kind === "watch_hit" ? "watch" : "actions";
  const tabChanged = state.activeTab !== itemTab;
  state.activeTab = itemTab;
  state.selectedID = itemID;
  if (tabChanged) render();
  else updateRovingTabIndex();
  if (focus) rowForItem(itemID)?.focus();
}

const LINK_LABELS: Record<string, string> = { doi: "DOI", arxiv: "arXiv", openalex: "OpenAlex", landing: "landing page" };

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

/** One short reference line per item: author and year in the selected citation
 * style, then the links. Where an author or a year already identifies the
 * paper, each link shrinks to the host it names and the full URL moves onto
 * the anchor as `title` plus an `aria-label` that keeps the link's relation —
 * the demotion `setAcquireButton` already ships in the popup, so nothing
 * leaves the surface and the screen-reader path stays whole. Where neither
 * fact exists the link IS the row's identity (a retraction notice, a
 * title-less job), so it keeps its locator: a bare `doi.org` would name every
 * paper equally. A row whose displayed title is already the DOI (placeholder
 * fallback) does not repeat that link here. */
function renderCitation(item: TriageSnapshotItem, placeholderURL: string | null): HTMLElement | null {
  const authorsText = factText(item, "Authors");
  const year = factText(item, "Year");
  const safe: Array<{ rel: string; url: string; host: string }> = [];
  for (const link of item.links) {
    const parsed = safeExternalLink(link.url);
    if (parsed !== null) safe.push({ rel: link.rel, ...parsed });
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
  const links = doi === undefined || doiShown ? extras : [doi, ...extras];
  const identified = prefix !== "";
  for (const link of links) {
    if (link !== links[0]) citation.append(document.createTextNode(" · "));
    const anchor = citationAnchor(link.url, identified ? link.host : link.url.replace(/^https:\/\//, ""));
    anchor.title = link.url;
    anchor.setAttribute("aria-label", `${LINK_LABELS[link.rel] ?? link.rel}: ${link.url}`);
    citation.append(anchor);
  }
  return citation.childNodes.length > 0 ? citation : null;
}

function previewButton(item: TriageSnapshotItem): HTMLButtonElement | null {
  if (item.kind !== "human_action" || item.action_kind !== "verify_identity") return null;
  const title = displayTitle(item).text;
  const button = element("button", "View PDF");
  button.type = "button";
  button.dataset.operation = "preview";
  button.dataset.label = "View PDF";
  button.setAttribute("aria-label", `View PDF for ${title}`);
  button.disabled = state.pending.has(item.id) || !state.connected;
  button.addEventListener("click", () => {
    void requestPreview(item);
  });
  return button;
}

function operationOpenLabel(item: TriageSnapshotItem): string {
  if (item.kind === "watch_hit") return "Open result";
  // Manual download opens the item's first safe link; "Open tab" is reserved
  // for focusing an existing papio handoff tab.
  if (item.kind === "human_action" && item.action_kind === "manual_download") return "Open link";
  if (item.kind === "human_action" && item.action_kind === "openurl_handoff") {
    return item.requires_auth === true ? "Sign in" : "Open page";
  }
  return "Open";
}

function operationButton(item: TriageSnapshotItem, operation: TriageOperation): HTMLButtonElement | null {
  // Retry is reserved for a newer inbox contract. Do not expose a dead,
  // permanently disabled placeholder when the daemon includes it.
  if (operation === "retry") return null;
  if (operation === "accept" && item.action_kind === "unsafe_pdf") return null;
  const label = operation === "open"
    ? operationOpenLabel(item)
    // Relabelled only once the daemon has actually advertised the picker
    // (grabPickerAvailable checks both daemonFeatures and live
    // connectivity): an old daemon still gets "Provide identifier" and the
    // guidance-only behaviour activateOperation falls back to below.
    : operation === "provide_identifier" && item.kind === "pdf_grab" && grabPickerAvailable()
      ? "Which paper is this?"
      : operationLabel(operation);
  const title = displayTitle(item).text;
  const button = element("button", label);
  button.type = "button";
  button.dataset.operation = operation;
  button.dataset.label = label;
  button.setAttribute("aria-label", `${label} ${title}`);

  const needsPreview = operation === "accept" && item.action_kind === "verify_identity";
  const handoff = item.kind === "human_action" && item.action_kind === "openurl_handoff";
  const unavailable = operation === "open" && (handoff ? handoffJobID(item) === null : firstSafeLink(item) === null);
  const grabLoading = operation === "provide_identifier" && state.grabPickers.get(item.id)?.status === "loading";
  button.disabled =
    state.pending.has(item.id) ||
    unavailable ||
    (isMutation(operation) && !state.connected) ||
    (needsPreview && !hasViewedPreview(item)) ||
    grabLoading;
  if (needsPreview && !hasViewedPreview(item)) button.title = "View the PDF before accepting it.";
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
  unsafe_pdf: { glyph: "⚠", label: "Held PDF needs review" },
  document_delivery: { glyph: "⇄", label: "Document delivery reconciliation" },
  downloads_access_required: { glyph: "⚠", label: "Downloads folder access needed" },
  watch_hit: { glyph: "✶", label: "New watch hit" },
  retraction: { glyph: "!", label: "Retraction notice" },
};

const DELIVERY_STATE_LABELS: Record<TriageDelivery["state"], string> = {
  offered: "request created but not submitted",
  submitted: "submitted",
  pending: "pending",
  fulfilled: "fulfilled",
  declined: "declined",
  cancelled: "cancelled",
  unknown_outcome: "unknown outcome",
};

// The status glyph is the row's quick-reference column; its meaning rides in
// the tooltip and accessible name. The action-kind vocabulary is open (a new
// daemon can ship new kinds), so unknown kinds degrade to a neutral dot with
// the raw kind as the label instead of breaking the row.

function statusTone(item: TriageSnapshotItem, key: string, working: boolean, waiting: boolean): StatusTone {
  if (working || waiting) return "continuing";
  if (item.kind === "retraction" || item.action_kind === "unsafe_pdf") return "danger";
  if (key === "manual_download_rejected_file" || key === "manual_download_wrong_work") return "degraded";
  if (key === "watch_hit" || key === "unknown") return "neutral";
  return "actionable";
}

function statusMeta(item: TriageSnapshotItem, working: boolean, waiting: boolean): { key: string; glyph: string; label: string; tone: StatusTone } {
  const key = item.kind === "human_action" && typeof item.action_kind === "string" ? item.action_kind : item.kind;
  // A rejected file and a wrong work are not "one more PDF to download"; they
  // get their own glyph and accessible name so a scanning eye cannot read them
  // as an ordinary manual download.
  const manual = manualDownloadCopy(item);
  const resolvedKey = manual !== null ? (item.guidance_variant ?? key) : key;
  const glyph = manual?.glyph ?? STATUS_META[key]?.glyph ?? "•";
  const label = manual?.statusLabel ?? STATUS_META[key]?.label ?? key.replaceAll("_", " ");
  const resolvedMetaKey = manual !== null ? resolvedKey : (STATUS_META[key] !== undefined ? key : "unknown");
  return { key: resolvedMetaKey, glyph, label, tone: statusTone(item, resolvedMetaKey, working, waiting) };
}

// Access classification chooses the next action instead of adding a second
// always-visible precondition. Supporting mechanics and daemon detail live in
// the row's disclosure below.
function challengeBlocked(item: TriageSnapshotItem): boolean {
  if (item.kind !== "human_action") return false;
  const jobID = itemJobID(item);
  if (jobID === null) return false;
  return state.activityEntries.some(
    (entry) =>
      entry.job_id === jobID &&
      entry.kind === "browser.error" &&
      /\bchallenge[_ ]blocked\b/i.test(entry.text),
  );
}

function actionDetail(item: TriageSnapshotItem): string {
  return item.facts.find((fact) => fact.label.toLowerCase() === "detail")?.text ?? "";
}

function missingAdapter(item: TriageSnapshotItem): boolean {
  return item.action_kind === "manual_download" && /\bno adapter for this provider\b/i.test(actionDetail(item));
}

interface ManualDownloadCopy {
  /** Hoisted block heading; the block appends its exact paper count. */
  heading: string;
  /** Hoisted instruction, printed once per block. */
  instruction: string;
  /** The same imperative where the block holds exactly one paper. Only these
   * five sentences need it: they are the only family copy that counts
   * providers rather than papers, and "these providers" cannot stand above a
   * single row. */
  one: string;
  glyph: string;
  statusLabel: string;
}

// Five genuinely different situations mint a manual download, and the closed
// guidance variant is the only trustworthy discriminator — never the detail
// prose. A rejected file in particular must not read as "download this PDF",
// which is exactly the file papio already refused.
const MANUAL_DOWNLOAD_COPY: Partial<Record<NonNullable<TriageSnapshotItem["guidance_variant"]>, ManualDownloadCopy>> = {
  manual_download: {
    heading: "Manual downloads",
    instruction: "Download each PDF — papio takes it from there.",
    one: "Download the PDF — papio takes it from there.",
    glyph: "↓",
    statusLabel: "Manual download needed",
  },
  manual_download_adapter_missing: {
    heading: "Manual downloads · no adapter yet",
    instruction: "papio has no adapter for these providers yet. Download each PDF — papio takes it from there.",
    one: "papio has no adapter for this provider yet. Download the PDF — papio takes it from there.",
    glyph: "↓",
    statusLabel: "Manual download needed — no adapter for this provider yet",
  },
  manual_download_page_undriveable: {
    heading: "Manual downloads · page changed",
    instruction: "papio could not drive these provider pages. Download each PDF — papio takes it from there.",
    one: "papio could not drive this provider page. Download the PDF — papio takes it from there.",
    glyph: "↓",
    statusLabel: "Manual download needed — papio could not drive the page",
  },
  manual_download_rejected_file: {
    heading: "Replace rejected files",
    instruction: "The file papio adopted was not the paper. Download a different PDF for each.",
    one: "The file papio adopted was not the paper. Download a different PDF.",
    glyph: "↺",
    statusLabel: "Rejected file — download a different PDF",
  },
  manual_download_wrong_work: {
    heading: "Wrong paper reached",
    instruction: "papio landed on a different work. Find and download the requested PDF.",
    one: "papio landed on a different work. Find and download the requested PDF.",
    glyph: "≠",
    statusLabel: "Wrong paper reached — find the requested PDF",
  },
};

function manualDownloadCopy(item: TriageSnapshotItem): ManualDownloadCopy | null {
  if (item.kind !== "human_action" || item.action_kind !== "manual_download") return null;
  const variant = item.guidance_variant;
  // An unmapped variant stays standalone with its legacy copy rather than
  // being guessed into a family.
  return variant === undefined ? null : MANUAL_DOWNLOAD_COPY[variant] ?? null;
}

function guidanceText(item: TriageSnapshotItem, blockedByChallenge: boolean): string | null {
  if (item.kind === "pdf_grab") {
    const grabID = item.grab?.grab_id ?? item.id.replace(/^pdf_grab:/, "");
    return `Provide an identifier: papio grabs identify ${grabID} --doi <value> (or --pmid/--arxiv <value>)`;
  }
  if (item.kind !== "human_action") return null;
  if (blockedByChallenge) return "Solve the security check in its tab";
  switch (item.action_kind) {
    // A daemon older than the closed manual-download variants ships no
    // structured discriminator at all; its detail prose is the only signal
    // there is. Every mapped variant is answered by its block copy instead,
    // which is why this legacy pair is only ever reached without one.
    case "manual_download":
      return missingAdapter(item) ? "No adapter yet - download this PDF manually" : "Download the PDF yourself - papio adopts it";
    case "openurl_handoff":
      return item.requires_auth === true ? "Sign in to your institution" : "Open the page";
    case "verify_identity":
      return "Review the PDF, then accept or reject";
    case "unsafe_pdf":
      return "File is held because it is encrypted or has active/embedded content; Reject returns to manual download; Dismiss cancels";
    case "document_delivery":
      return "Confirm what the library has on file";
    case "downloads_access_required":
      return `papio can't read ${actionDetail(item) || "your Downloads folder"} — grant access in System Settings → Privacy & Security`;
    default:
      return null;
  }
}
/** Two states are the browser's own rather than the daemon's: an institution
 * sign-in already open in another tab, and a tab a security check has
 * blocked. Copy written for a whole block can describe neither, so such a row
 * keeps its own instruction line inside the block it still belongs to. */
function rowStateOverridesBlock(item: TriageSnapshotItem): boolean {
  return waitingSibling(item) || challengeBlocked(item);
}

/** The imperative a row would print for itself. This is what gets hoisted
 * where the daemon shipped no guidance variant for the page to author copy
 * from. Null for a row whose own state overrides the block, and for a row
 * with no imperative at all. */
function hoistableGuidance(item: TriageSnapshotItem): string | null {
  if (rowStateOverridesBlock(item)) return null;
  const guidance = guidanceText(item, false);
  if (guidance === null || guidance.trim() === "") return null;
  return item.attention === "working" ? `papio is continuing — ${guidance.charAt(0).toLowerCase()}${guidance.slice(1)}` : guidance;
}

interface FamilyCopy {
  /** Sentence-case kind; the block appends its exact paper count. */
  heading: string;
  /** The one instruction printed above the block's rows. */
  instruction: string;
  /** The same imperative where the block holds exactly one paper. */
  one: string;
}

interface FamilyRender {
  heading: string;
  instruction: string;
  descriptionID: string;
  total: number;
  shown: number;
  /** Print each row's own reason line. False only where several loaded rows
   * share one reason — the exact repetition hoisting exists to remove. One
   * row's reason repeats nothing, so a lone row keeps it. */
  rowReasons: boolean;
  /** The honest download route on this browser, printed once per block where
   * the hoisted instruction alone would over-promise. Null where papio can
   * take the download itself. */
  platformRoute: string | null;
}

// Eight of these read correctly above one row and above twenty: they name
// papers, checks or pages, never a count of providers. The five manual
// downloads do count providers, so each carries its own one-row form. The
// pdf_identifier variant is deliberately absent — see blockCopy.
function familyCopy(item: TriageSnapshotItem): FamilyCopy | null {
  const manual = manualDownloadCopy(item);
  if (manual !== null) return { heading: manual.heading, instruction: manual.instruction, one: manual.one };
  const copy = (heading: string, instruction: string): FamilyCopy => ({ heading, instruction, one: instruction });
  switch (item.guidance_variant) {
    case "institution_sign_in": return copy("Institution sign-in", "Sign in to your institution once — papio continues the waiting papers.");
    case "open_page": return copy("Pages to open", "Open each source page so papio can continue.");
    case "verify_identity": return copy("PDF identity review", "Review each PDF, then accept or reject it.");
    case "document_delivery": return copy("Document delivery", "Confirm what the library has on file for each request.");
    case "downloads_access": return copy("Downloads access", "Grant Downloads access so papio can adopt the pending files.");
    case "terms_acceptance": return copy("Publisher terms", "Review and accept the publisher terms for each source.");
    case "security_challenge": return copy("Security checks", "Solve each security check in its tab.");
    // pdf_identifier: see blockCopy. A grab's imperative names its own id.
    case "papio_continuing": return copy("papio continuing", "papio is continuing automatically — no decision is needed.");
    default: return null;
  }
}

/** The heading a block keeps when the daemon shipped no guidance variant, or
 * shipped one this extension does not know. The action kind is still
 * trustworthy, so such a block never loses its header and its rows never
 * repeat one instruction N times. A kind absent here has no honest block name
 * and its rows stay standalone. */
const ACTION_KIND_HEADINGS: Record<string, string> = {
  manual_download: "Manual downloads",
  openurl_handoff: "Pages to open",
  verify_identity: "PDF identity review",
  document_delivery: "Document delivery",
  downloads_access_required: "Downloads access",
  unsafe_pdf: "Held files",
};

/** The copy a block prints above its rows. Authored family copy wins wherever
 * the daemon named a guidance variant: it is the only path that can name the
 * kind, and it renders at any count — which is what finally gives a lone
 * security check, terms gate or institution sign-in the instruction the
 * per-row path has no case for. Without a variant the row's own imperative is
 * hoisted verbatim under an action-kind heading; that form earns its header
 * only by removing repetition, so a lone row keeps its instruction inline. */
function blockCopy(item: TriageSnapshotItem): { copy: FamilyCopy; needsSiblings: boolean } | null {
  // A grab's imperative is its own `papio grabs identify <id>` command, which
  // no block sentence can stand in for: hoisting one summary would delete as
  // many distinct routes as there are rows. Grabs stay standalone under the
  // group's own kind label.
  if (item.kind === "pdf_grab") return null;
  const authored = familyCopy(item);
  if (authored !== null) return { copy: authored, needsSiblings: false };
  if (item.kind !== "human_action") return null;
  const heading = ACTION_KIND_HEADINGS[item.action_kind ?? ""];
  const instruction = hoistableGuidance(item);
  if (heading === undefined || instruction === null) return null;
  return { copy: { heading, instruction, one: instruction }, needsSiblings: true };
}

/** Rows share a block when the daemon puts them in one run and they carry the
 * same copy. Run identity stays in the key because a run is a batch: two
 * adjacent lone rows that happen to read alike are still two batches, and one
 * heading claiming a single count for both would be false. Rows with no run
 * key group on their copy alone, which is all an older daemon supplies. */
function blockKey(item: TriageSnapshotItem, copy: FamilyCopy): string {
  return [
    item.run_key ?? "",
    item.guidance_variant ?? "",
    item.operation_variant ?? "",
    item.next_actor ?? "",
    copy.heading,
    copy.instruction,
  ].join("\u0000");
}

function blockRender(
  items: readonly TriageSnapshotItem[],
  runSizes: ReadonlyMap<string, number>,
  start: number,
  shown: number,
  copy: FamilyCopy,
): FamilyRender {
  const first = items[start]!;
  const counts = state.counts;
  const run = counts?.family_breakdown_complete === true
    ? counts.family_runs?.find((candidate) =>
      candidate.run_key === first.run_key &&
      candidate.guidance_variant === first.guidance_variant &&
      candidate.operation_variant === first.operation_variant &&
      candidate.next_actor === first.next_actor)
    : undefined;
  // The daemon counts a whole run. That count can only stand above this block
  // where the block holds every loaded row of the run; a run the list has
  // split is counted by what is actually under the heading.
  const holdsWholeRun = first.run_key !== undefined && runSizes.get(first.run_key) === shown;
  const total = run !== undefined && holdsWholeRun ? run.count : shown;
  const instruction = total === 1 ? copy.one : copy.instruction;
  const ownReason = reasonSummary(first, instruction);
  let rowReasons = shown === 1;
  for (let at = start + 1; !rowReasons && at < start + shown; at += 1) {
    rowReasons = reasonSummary(items[at]!, instruction) !== ownReason;
  }
  return {
    heading: copy.heading,
    instruction,
    descriptionID: `family-guidance-${first.id.replace(/[^A-Za-z0-9_-]/g, "_")}`,
    total,
    shown,
    rowReasons,
    // Once per block, never per row: the hoisted imperative reads the same on
    // every browser, but how a saved file reaches papio does not.
    platformRoute: first.action_kind === "manual_download" && !downloadSteeringAvailable() ? NO_STEERING_ROUTE : null,
  };
}

/** One entry per row: the block that row sits under, or null where it stands
 * alone. Blocks are derived from the loaded rows themselves, so a single
 * unmapped guidance flag anywhere in the snapshot can no longer flip the whole
 * surface back to one repeated instruction per row with no header at all — the
 * daemon's run breakdown is consulted only for how many papers a run holds. */
function familyBlocks(items: readonly TriageSnapshotItem[]): Array<FamilyRender | null> {
  const copies = items.map((item) => blockCopy(item));
  const keys = copies.map((block, index) => block === null ? null : blockKey(items[index]!, block.copy));
  const runSizes = new Map<string, number>();
  for (const item of items) {
    if (item.run_key === undefined) continue;
    runSizes.set(item.run_key, (runSizes.get(item.run_key) ?? 0) + 1);
  }
  const blocks: Array<FamilyRender | null> = items.map(() => null);
  let index = 0;
  while (index < items.length) {
    const block = copies[index];
    if (block === null || block === undefined) {
      index += 1;
      continue;
    }
    let end = index + 1;
    while (end < items.length && keys[end] === keys[index]) end += 1;
    const shown = end - index;
    if (block.needsSiblings && shown < 2) {
      index += 1;
      continue;
    }
    blocks.fill(blockRender(items, runSizes, index, shown, block.copy), index, end);
    index = end;
  }
  return blocks;
}

/** The daemon's reason prose and this page's own hoisted copy name the same
 * situation in different words — "reached" for "landed on", one provider for
 * many — so comparing them literally printed the identical fact twice on every
 * live manual-download family: once hoisted, once per row. Each entry is a
 * closed pair of the exact sentences the two sides ship (the details minted in
 * internal/browser/bridge.go's manual-download branch, against
 * MANUAL_DOWNLOAD_COPY above), matched after the same alphanumeric collapsing
 * both sides already get. A lookup table, never a fuzzy matcher: a reason that
 * is not listed here still prints. */
const EQUIVALENT_REASONS: ReadonlyArray<readonly string[]> = [
  ["papio reached a different work", "papio landed on a different work"],
  ["papio could not drive the provider page", "papio could not drive these provider pages"],
  ["papio has no adapter for this provider yet", "papio has no adapter for these providers yet"],
];

// Collapsing leaves only lowercase alphanumerics and single spaces, so a NUL
// stands in for an equivalence class without any chance of colliding with
// prose.
function canonicalReason(collapsed: string): string {
  let text = collapsed;
  for (const [index, phrases] of EQUIVALENT_REASONS.entries()) {
    for (const phrase of phrases) text = text.replaceAll(phrase, `\u0000${index}`);
  }
  return text;
}

/** The row's own durable reason, reduced to the leading clause the block
 * heading cannot carry. Daemon detail prose habitually appends the very
 * instruction that is already hoisted ("…; download the requested PDF
 * yourself"), so only the reason itself survives, bounded like every other
 * string papio did not author. Returns null when it would merely restate the
 * instruction. */
function reasonSummary(item: TriageSnapshotItem, instruction: string): string | null {
  const detail = boundedProse(actionDetail(item), 400);
  if (detail === "") return null;
  const lead = detail.split(/[;.]\s+/u)[0] ?? detail;
  const reason = boundedProse(lead.replace(/[.;,]+$/u, ""), 120);
  if (reason === "") return null;
  const collapsed = reason.toLowerCase().replace(/[^a-z0-9]+/g, " ").trim();
  if (collapsed === "") return null;
  const compact = canonicalReason(collapsed);
  const said = canonicalReason(instruction.toLowerCase().replace(/[^a-z0-9]+/g, " ").trim());
  return said.includes(compact) || compact.includes(said) ? null : reason;
}

/** Chrome exposes chrome.downloads.onDeterminingFilename and can therefore
 * take a download papio is expecting; Firefox has no equivalent, so steering
 * is a platform fact rather than a setting. Probed exactly as page-bulk.ts
 * probes it, never by sniffing the user agent. */
function downloadSteeringAvailable(): boolean {
  const downloads: unknown = typeof chrome === "undefined" ? undefined : chrome.downloads;
  if (typeof downloads !== "object" || downloads === null || !("onDeterminingFilename" in downloads)) return false;
  return downloads.onDeterminingFilename !== undefined;
}

// Neither sentence names a folder: papio's adoption root is a daemon setting
// the browser cannot see, and the researcher has no way to learn a job id.
// Send PDF passes its own filename and needs no steering at all, so it is the
// route that is true on every browser.
const NO_STEERING_ROUTE =
  "This browser cannot pass a saved download to papio. Open the PDF and use Send PDF in the papio toolbar popup instead.";
const STEERED_ROUTE =
  "papio picks the download up from your browser's downloads. When it cannot tell which paper a file belongs to, " +
  "open the PDF and use Send PDF in the papio toolbar popup.";

function mechanismText(item: TriageSnapshotItem, blockedByChallenge: boolean): string | null {
  if (item.kind !== "human_action") return null;
  if (blockedByChallenge) {
    return "papio resumes automatically after you solve the security check.";
  }
  switch (item.action_kind) {
    case "manual_download": {
      // Steering claims the researcher's own click only when exactly one
      // retained job matches the download's host, so this promises nothing.
      const adoption = downloadSteeringAvailable() ? STEERED_ROUTE : NO_STEERING_ROUTE;
      const adapterMissing = manualDownloadCopy(item) === null
        ? missingAdapter(item)
        : item.guidance_variant === "manual_download_adapter_missing";
      return adapterMissing
        ? `A sanitized page diagnostic is saved locally; run papio adapter captures to find it. ${adoption}`
        : adoption;
    }
    case "openurl_handoff":
      return "A fresh link is generated each time you open this action. papio continues automatically after the handoff.";
    case "verify_identity":
      return "papio files accepted PDFs into your library.";
    case "document_delivery":
      return "papio paused automatic polling until you confirm what the library has on file for this request.";
    case "downloads_access_required":
      return "papio adopts the pending download automatically once access is granted — nothing to redo.";
    default:
      return null;
  }
}

/** Render the next action. Working rows explicitly name papio as the actor
 * and never present their imperative guidance as a decision. */
function renderInstruction(
  item: TriageSnapshotItem,
  blockedByChallenge: boolean,
  working = false,
): HTMLElement | null {
  const guidance = guidanceText(item, blockedByChallenge);
  if (guidance === null || guidance.trim() === "") return null;
  const text = working
    ? `papio is continuing — ${guidance.charAt(0).toLowerCase()}${guidance.slice(1)}`
    : guidance;
  const instruction = element("p", text);
  instruction.className = "item-instruction item-guidance";
  instruction.id = itemGuidanceID(item);
  if (blockedByChallenge) instruction.classList.add("challenge-annotation");
  return instruction;
}

function liveStatusChip(item: TriageSnapshotItem): HTMLElement | null {
  if (item.kind !== "human_action") return null;
  const jobID = itemJobID(item);
  if (jobID === null) return null;
  const status = activityStatusForJob(jobID);
  if (status === null) return null;
  const chip = element("span", status);
  chip.className = "activity-live-status";
  chip.dataset.jobId = jobID;
  return chip;
}

// Always-visible provider/reference/state for a document_delivery item —
// the reconciliation ops act on exactly this record, so it stays on the
// card rather than behind a disclosure.
function renderDeliveryDetail(item: TriageSnapshotItem): HTMLElement | null {
  if (item.delivery === undefined) return null;
  const list = element("dl");
  list.className = "item-delivery";
  const rows: Array<[string, string]> = [["Provider", item.delivery.provider]];
  if (item.delivery.provider_reference !== undefined && item.delivery.provider_reference !== "") {
    rows.push(["Reference", item.delivery.provider_reference]);
  }
  rows.push(["Status", DELIVERY_STATE_LABELS[item.delivery.state]]);
  for (const [label, value] of rows) {
    list.append(element("dt", label), element("dd", value));
  }
  return list;
}

const GRAB_VERDICT_LABELS: Record<GrabSuggestionRow["verdict"], string> = {
  qualifies: "Qualifies",
  review: "Needs review",
  rejected: "Rejected",
};

function renderGrabSuggestionRow(item: TriageSnapshotItem, grabID: string, suggestion: GrabSuggestionRow, confirming: boolean): HTMLElement {
  const row = element("li");
  row.className = "grab-picker-suggestion";
  row.dataset.verdict = suggestion.verdict;

  const heading = element("div");
  heading.className = "grab-picker-suggestion-heading";
  const displayName = suggestion.title !== undefined && suggestion.title !== "" ? suggestion.title : suggestion.job_id;
  const titleText = suggestion.year !== undefined ? `${displayName} (${suggestion.year})` : displayName;
  const titleEl = element("span", titleText);
  titleEl.className = "grab-picker-suggestion-title";
  const badge = element("span", GRAB_VERDICT_LABELS[suggestion.verdict]);
  badge.className = "grab-picker-verdict";
  badge.dataset.verdict = suggestion.verdict;
  heading.append(titleEl, badge);
  row.append(heading);

  if (suggestion.reason !== undefined && suggestion.reason !== "") {
    const reason = element("p", suggestion.reason);
    reason.className = "grab-picker-suggestion-reason";
    row.append(reason);
  }

  // The evidence is the reason the operator can trust the ranking, so it is
  // never hidden behind a disclosure — even a one-line qualifying match
  // shows what qualified it.
  if (suggestion.evidence.length > 0) {
    const evidenceList = element("ul");
    evidenceList.className = "grab-picker-evidence";
    for (const line of suggestion.evidence) evidenceList.append(element("li", line));
    row.append(evidenceList);
  }

  const confirmButton = element("button", confirming ? "Filing…" : "This is the one");
  confirmButton.type = "button";
  confirmButton.className = "grab-picker-confirm";
  confirmButton.disabled = confirming || state.pending.has(item.id);
  confirmButton.setAttribute("aria-label", `File this capture as ${displayName}`);
  confirmButton.addEventListener("click", () => {
    void confirmGrabCandidate(item, grabID, suggestion.job_id);
  });
  row.append(confirmButton);
  return row;
}

/** Renders the ranked "which paper is this?" picker for one pdf_grab row.
 * Nothing here fires until requestGrabSuggestions has been asked for by a
 * click — see the button wiring in operationButton/activateOperation — and
 * the result is never persisted across a re-render caused by anything else
 * (a poll tick, another row's action), since grabPickers lives in page
 * state exactly like itemMessages and survives render() the same way. */
function renderGrabPicker(item: TriageSnapshotItem): HTMLElement | null {
  const grabID = item.grab?.grab_id;
  if (grabID === undefined) return null;
  const picker = state.grabPickers.get(item.id);
  if (picker === undefined) return null;
  const container = element("div");
  container.className = "grab-picker";

  if (picker.status === "loading") {
    const status = element("p", "Looking for a match…");
    status.className = "grab-picker-status";
    container.append(status);
    return container;
  }

  if (picker.confirmNotice !== undefined) {
    const notice = element("p", picker.confirmNotice.text);
    notice.className = "grab-picker-confirm-notice";
    notice.dataset.tone = picker.confirmNotice.tone;
    container.append(notice);
  }

  if (picker.outcome !== "ok") {
    const status = element("p", picker.detail ?? "papio could not compute suggestions for this file.");
    status.className = "grab-picker-status";
    status.dataset.tone = "error";
    container.append(status);
    return container;
  }

  if (picker.documentIdentifiers.length > 0) {
    const section = element("div");
    section.className = "grab-picker-identifiers";
    section.append(element("h4", "papio found in the file"));
    const list = element("ul");
    for (const identifier of picker.documentIdentifiers) {
      const row = element("li");
      const label = element("span", `${identifier.kind.toUpperCase()}: ${identifier.value}`);
      label.className = "grab-picker-identifier-value";
      const source = element("span", `found in ${identifier.source}`);
      source.className = "grab-picker-identifier-source";
      const command = element("code", `papio grabs identify ${grabID} --${identifier.kind} ${identifier.value}`);
      row.append(label, document.createTextNode(" — "), source, element("br"), command);
      list.append(row);
    }
    section.append(list);
    container.append(section);
  }

  if (picker.suggestions.length > 0) {
    const section = element("div");
    section.className = "grab-picker-suggestions";
    section.append(element("h4", "Which paper is this?"));
    const list = element("ul");
    for (const suggestion of picker.suggestions) {
      list.append(renderGrabSuggestionRow(item, grabID, suggestion, picker.confirmingJobID === suggestion.job_id));
    }
    section.append(list);
    if (picker.truncated) {
      const note = element("p", "More candidates exist than are shown here.");
      note.className = "grab-picker-truncated";
      section.append(note);
    }
    container.append(section);
  }

  if (picker.documentIdentifiers.length === 0 && picker.suggestions.length === 0) {
    const status = element("p", "No pending candidates matched this file, and papio found no identifier in it. Use papio grabs identify to file it manually.");
    status.className = "grab-picker-status";
    container.append(status);
  }

  return container;
}

// Supporting explanation and backend identifiers share one compact disclosure,
// preserving native button keyboard semantics and state.
function renderDebug(
  item: TriageSnapshotItem,
  blockedByChallenge: boolean,
): { toggle: HTMLButtonElement; list: HTMLDListElement } {
  const list = element("dl");
  list.className = "item-debug";
  const expanded = state.expandedItemIDs.has(item.id);
  list.hidden = !expanded;
  list.id = `backend-details-${item.id}`;

  const details = [factText(item, "Detail"), mechanismText(item, blockedByChallenge)]
    .filter((value): value is string => value !== null && value.trim() !== "");
  if (details.length > 0) {
    const field = element("div");
    field.className = "item-debug-field item-mechanism";
    const valueElement = element("dd");
    appendFactText(valueElement, details.join(" "));
    field.append(element("dt", "details"), valueElement);
    list.append(field);
  }

  const rows: Array<[string, string]> = [["item", item.id]];
  const job = factText(item, "Job");
  if (job !== null) rows.push(["job", job]);
  if (item.kind === "human_action" && typeof item.revision === "number") {
    rows.push(["revision", String(item.revision)]);
  }
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
  toggle.dataset.label = "More details";
  toggle.setAttribute("aria-controls", list.id);
  toggle.setAttribute("aria-expanded", String(expanded));
  toggle.setAttribute("aria-label", "More details");
  toggle.addEventListener("click", () => {
    const expanded = toggle.getAttribute("aria-expanded") === "true";
    toggle.setAttribute("aria-expanded", String(!expanded));
    list.hidden = expanded;
    if (expanded) state.expandedItemIDs.delete(item.id);
    else state.expandedItemIDs.add(item.id);
  });
  return { toggle, list };
}

function renderItem(item: TriageSnapshotItem, family: FamilyRender | null = null): HTMLElement {
  const card = element("article");
  card.className = "triage-item";
  card.dataset.triageItemId = item.id;
  const waiting = waitingSibling(item);
  const working = waiting || item.attention === "working";
  card.dataset.attention = working ? "working" : (item.attention ?? "");
  card.dataset.working = working ? "true" : "false";
  if (!working && item.attention === undefined) delete card.dataset.attention;
  const title = displayTitle(item);
  const citation = renderCitation(item, title.placeholder ? title.text : null);
  card.setAttribute("aria-label", title.text);
  card.addEventListener("focusin", () => selectItem(item.id, false));
  card.addEventListener("click", () => selectItem(item.id, false));

  const status = statusMeta(item, working, waiting);
  const badge = element("span", status.glyph);
  badge.className = "item-status";
  badge.dataset.status = status.key;
  badge.dataset.tone = status.tone;
  badge.dataset.label = status.label;
  badge.setAttribute("role", "img");
  badge.setAttribute("aria-label", status.label);
  card.append(badge);

  const body = element("div");
  body.className = "item-body";
  card.append(body);

  const headingText = element("h3", title.text);
  if (title.placeholder) headingText.classList.add("title-placeholder");
  const blockedByChallenge = challengeBlocked(item);
  const debug = renderDebug(item, blockedByChallenge);
  body.append(headingText);
  if (citation !== null) body.append(citation);
  // A row prints an instruction where no block speaks for it, and where its
  // own browser-local state — a sign-in open elsewhere, a challenge-blocked
  // tab — says something the block's copy cannot.
  const speaksForItself = family === null || waiting || blockedByChallenge;
  const instruction: HTMLElement | null = !speaksForItself
    ? null
    : waiting
      ? (() => {
        const waitingInstruction = element("p", "papio is continuing — waiting for the institution sign-in already open in another tab");
        waitingInstruction.className = "item-instruction item-guidance";
        waitingInstruction.id = itemGuidanceID(item);
        return waitingInstruction;
      })()
      : renderInstruction(item, blockedByChallenge, working);
  if (instruction !== null) body.append(instruction);
  // The hoisted instruction says what to do for the whole block; a row prints
  // its own reason only where that reason differs from its siblings', so the
  // block never degenerates back into one repeated sentence per row.
  const reasonText = family !== null && family.rowReasons && !working && !blockedByChallenge
    ? reasonSummary(item, family.instruction)
    : null;
  const reason = reasonText === null ? null : element("p", reasonText);
  if (reason !== null) {
    reason.className = "item-instruction item-reason";
    reason.id = `item-reason-${item.id.replace(/[^A-Za-z0-9_-]/g, "_")}`;
    body.append(reason);
  }
  const liveStatus = liveStatusChip(item);
  if (liveStatus !== null) body.append(liveStatus);
  const delivery = renderDeliveryDetail(item);
  if (delivery !== null) body.append(delivery);
  if (item.kind === "pdf_grab") {
    const picker = renderGrabPicker(item);
    if (picker !== null) body.append(picker);
  }

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

  if (instruction !== null) {
    instruction.append(debug.toggle);
  } else if (reason !== null) {
    reason.append(debug.toggle);
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
    body.append(result);
  }
  const controls = element("div");
  controls.className = "item-controls";
  controls.setAttribute("aria-label", `Actions for ${title.text}`);
  const preview = working ? null : previewButton(item);
  if (preview !== null) controls.append(preview);
  for (const operation of item.ops) {
    // A working row may expose only non-decision context (open/history).
    // Never present a mutation as if the researcher owns the next turn.
    if ((working && isMutation(operation)) || (waiting && operation === "open")) continue;
    const button = operationButton(item, operation);
    if (button !== null) controls.append(button);
  }
  if (controls.childElementCount > 0) {
    // Rule 13: the hoisted instruction, the block's platform route, and the
    // row's own reason are all part of what each control does, so each control
    // names every one of them that exists.
    const routeID = family?.platformRoute === null || family === null ? undefined : `${family.descriptionID}-route`;
    const describedBy = [family?.descriptionID, instruction?.id, routeID, reason?.id]
      .filter((id): id is string => id !== undefined).join(" ");
    if (describedBy !== "") {
      for (const control of Array.from(controls.querySelectorAll<HTMLButtonElement>("button"))) {
        control.setAttribute("aria-describedby", describedBy);
      }
    }
  }
  if (controls.childElementCount > 0) card.append(controls);

  return card;
}
function renderGroup(kind: TriageSnapshotItem["kind"], heading: string | null, items: TriageSnapshotItem[]): HTMLElement | null {
  if (items.length === 0) return null;
  const section = element("section");
  section.className = `triage-group triage-group-${kind}`;
  // A card is one block. A singleton block hoists its own heading, so keying
  // card edges on "the sibling is not a row" put a headingless row inside the
  // preceding block's card, where it visually inherited that block's heading
  // and count — a card headed "2 papers" held four rows of three different
  // kinds. Card edges therefore follow block identity, and a row with no
  // block is its own card.
  const families = familyBlocks(items);
  // The kind label names rows nothing else names. Where every row already sits
  // under a block header carrying its kind and its count, printing the kind
  // again would state one fact twice.
  if (heading !== null && families.some((family) => family === null)) {
    section.append(element("h2", `${heading} (${items.length})`));
  }
  const cardKeys = families.map((family, index) => family?.descriptionID ?? `standalone:${items[index]!.id}`);

  let previousBlock: string | undefined;
  for (const [index, item] of items.entries()) {
    const family = families[index]!;
    if (family !== null && family.descriptionID !== previousBlock) {
      // Kind, count and the single instruction share one header line: the
      // instruction is what the heading is for, and two stacked lines said it
      // twice as far as the eye is concerned.
      const header = element("div");
      header.className = "family-header";
      const familyHeading = element("h2", `${family.heading} · ${family.total} paper${family.total === 1 ? "" : "s"}`);
      familyHeading.className = "family-heading";
      familyHeading.id = `${family.descriptionID}-heading`;
      if (state.filterQuery.trim() !== "" && family.shown !== family.total) {
        familyHeading.append(element("span", ` (${family.shown} of ${family.total} shown)`));
      }
      header.append(familyHeading);
      const familyInstruction = element("p", family.instruction);
      familyInstruction.className = "item-instruction item-guidance family-guidance";
      familyInstruction.id = family.descriptionID;
      familyInstruction.setAttribute("aria-labelledby", familyHeading.id);
      header.append(familyInstruction);
      section.append(header);
      if (family.platformRoute !== null) {
        // Once per block, never per row, and never shortened: this is the only
        // place a Firefox operator learns a saved download cannot reach papio.
        const route = element("p", family.platformRoute);
        route.className = "item-instruction family-mechanism";
        route.id = `${family.descriptionID}-route`;
        section.append(route);
      }
      previousBlock = family.descriptionID;
    } else if (family === null) {
      previousBlock = undefined;
    }
    const row = renderItem(item, family);
    if (cardKeys[index] !== cardKeys[index - 1]) row.dataset.cardStart = "true";
    if (cardKeys[index] !== cardKeys[index + 1]) row.dataset.cardEnd = "true";
    section.append(row);
  }
  return section;
}


function pulseMovingCount(): number | undefined {
  if (!state.connected || state.pulse === undefined) return undefined;
  const display = derivePulseDisplay(state.pulse, "connected", Date.now(), 45_000);
  if (display.primary !== "Moving") return undefined;
  const { in_flight: inFlight, continuing } = state.pulse.pulse;
  if (
    typeof inFlight !== "number" ||
    !Number.isSafeInteger(inFlight) ||
    inFlight < 0 ||
    typeof continuing !== "number" ||
    !Number.isSafeInteger(continuing) ||
    continuing < 0
  ) return undefined;
  return inFlight + continuing;
}

function renderPulse(): void {
  if (elements === null) return;
  const connectionStatus = state.connected ? "connected" : "disconnected";
  const display = derivePulseDisplay(state.pulse, connectionStatus, Date.now(), 45_000);
  const detail = [display.buckets, display.next, display.capacity, display.batch].filter((part) => part !== "").join(" · ");
  elements.pulse.dataset.state = display.primary.toLowerCase().replaceAll(" ", "-");
  elements.pulse.title = detail;
  elements.pulse.hidden = false;

  if (!state.connected) {
    elements.pulse.hidden = true;
    return;
  }

  if (display.primary === "Moving") {
    const moving = pulseMovingCount();
    elements.pulse.textContent = moving === undefined
      ? "papio is working"
      : `papio is working on ${moving} paper${moving === 1 ? "" : "s"}`;
    return;
  }

  if (display.primary === "Waiting on you") {
    elements.pulse.hidden = true;
    return;
  }

  if (display.primary === "Scheduled") {
    elements.pulse.textContent = display.next !== "" ? display.next : display.primaryText;
    return;
  }

  if (display.primary === "Stalled") {
    elements.pulse.textContent = display.primaryText;
    return;
  }

  if (display.primary === "Idle") {
    if (display.primaryText.includes("last finished")) {
      elements.pulse.textContent = display.primaryText;
      return;
    }
    elements.pulse.hidden = true;
    return;
  }

  if (display.primary === "Unknown") {
    // Say which of the two it is: papio has not reported yet, or it reported at
    // a time worth naming. "Can't tell" conflated them.
    elements.pulse.textContent = pulseIsUnmeasured(display)
      ? "papio hasn't reported progress yet"
      : display.primaryText;
    return;
  }

  elements.pulse.hidden = true;
}


// The tab labels below are the inventory authority ("Actions (N)",
// "Watch hits (N)"). This line carries only the one fact they cannot: the
// daemon's effective count of researcher-owned turns. `pending_total` does not
// belong here: adding it beside those tabs repeats the inventory and makes the
// larger overlapping total look like a third category.
function renderCounts(): void {
  if (elements === null) return;
  const counts = state.counts ?? state.snapshot?.counts;
  if (counts === undefined || counts === null) {
    elements.counts.textContent = "Counts unavailable";
    return;
  }
  const required = counts.turns_required;
  if (typeof required !== "number" || !Number.isFinite(required)) {
    // Counts v3 was not negotiated, so the turn total is genuinely unknown.
    // Falling through to a zero would tell an operator with a full queue that
    // nothing needs them; the tabs still carry the inventory either way.
    elements.counts.textContent = "papio hasn't reported how many need you";
    return;
  }
  // turns_required stays exact when required_turns_complete is false: the
  // daemon drops the per-item projection and keeps the count ("Keep the exact
  // attention count" in internal/triage/triage.go). Only the badge suppresses
  // the number there, because a toolbar square cannot show a four-digit one.
  const turns = Math.max(0, Math.trunc(required));
  elements.counts.textContent =
    turns === 0 ? "Nothing needs you" : `${turns} need${turns === 1 ? "s" : ""} you`;
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
    if (item.action_kind === "unsafe_pdf") {
      elements.dialogMessage.textContent = `Reject ${item.title}? A manual download will be requested.`;
    } else {
      elements.dialogMessage.textContent = `Reject ${item.title}? It cancels the job.`;
    }
  } else {
    elements.dialogMessage.textContent = `Accept ${item.title}?`;
  }
  elements.dialogConfirm.textContent = confirmation.verdict === "accept" ? "Accept" : "Reject";
  elements.dialogConfirm.disabled = state.pending.has(item.id);
  elements.dialogCancel.disabled = state.pending.has(item.id);
  elements.dialog.hidden = false;
}

function render(): void {
  if (elements === null) return;
  if (!state.connectionKnown) {
    // Nothing has answered yet. The old code read `connected: false` as a
    // verdict, so every open flashed a red "Disconnected" before the first
    // snapshot returned.
    elements.connection.textContent = "Connecting to daemon…";
    delete elements.connection.dataset.state;
    elements.connection.hidden = !state.connectingVisible;
    elements.reconnect.hidden = true;
  } else {
    const isDisconnected = !state.connected;
    // A refused session is not lost connectivity: the daemon answered. Promising
    // an automatic recovery and pointing at `papio status` sent the researcher
    // looking for a broken daemon that was working the whole time.
    elements.connection.textContent = isDisconnected
      ? state.connectionSessionElsewhere
        ? `Not this browser: ${state.connectionMessage} The inbox reconnects by itself once it does.`
        : `Disconnected: ${state.connectionMessage} Reconnecting automatically — run papio status if this persists.`
      : state.connectionMessage;
    elements.connection.dataset.state = isDisconnected ? "disconnected" : "connected";
    elements.connection.hidden = !isDisconnected;
    elements.reconnect.hidden = !isDisconnected;
  }
  elements.refresh.disabled = state.loading;
  elements.reconnect.disabled = state.loading;
  renderCounts();
  renderPulse();
  renderTabs();
  renderActivity();

  const actionItems = itemsForTab("actions");
  const watchItems = itemsForTab("watch");
  const activeItems = itemsForTab(state.activeTab);
  if (state.selectedID !== null && !activeItems.some((item) => item.id === state.selectedID)) {
    state.selectedID = activeItems[0]?.id ?? null;
  }
  if (state.selectedID === null && activeItems.length > 0) state.selectedID = activeItems[0]?.id ?? null;

  elements.list.replaceChildren();
  const actionGroups = [
    renderGroup("retraction", "Retractions", actionItems.filter((item) => item.kind === "retraction")),
    renderGroup("human_action", null, actionItems.filter((item) => item.kind === "human_action")),
    renderGroup("pdf_grab", "PDF grabs", actionItems.filter((item) => item.kind === "pdf_grab")),
  ];
  for (const group of actionGroups) if (group !== null) elements.list.append(group);
  elements.watchList.replaceChildren();
  const watchGroup = renderGroup("watch_hit", null, watchItems);
  if (watchGroup !== null) elements.watchList.append(watchGroup);

  if (state.snapshot === null) {
    elements.list.append(element("p", "No snapshot is available yet. Reconnect to retrieve the inbox."));
  } else if (state.snapshot.items.length === 0) {
    elements.list.append(element("p", "Your inbox is clear."));
  } else if (actionItems.length === 0 && state.filterQuery.trim() !== "") {
    elements.list.append(element("p", `No items match "${state.filterQuery.trim()}".`));
  } else if (actionItems.length === 0) {
    // The pulse projection, not counts.jobs_working, decides whether an
    // honest liveness sentence is warranted in the empty Actions view.
    const moving = pulseMovingCount();
    elements.list.append(
      element(
        "p",
        moving !== undefined && moving > 0
          ? `No decisions waiting. papio is working through ${moving} papers — see Activity.`
          : "No decisions waiting.",
      ),
    );
  }
  if (state.snapshot === null) {
    elements.watchList.append(element("p", "No snapshot is available yet. Reconnect to retrieve the inbox."));
  } else {
    const snapshotWatchItems = state.snapshot.items.filter((item) => item.kind === "watch_hit");
    const daemonWatchTotal = state.counts?.watch_hits ?? snapshotWatchItems.length;
    if (snapshotWatchItems.length === 0 && daemonWatchTotal > 0) {
      // Hits exist daemon-side but the snapshot pages haven't streamed them
      // yet; autoLoadWatchHits pages forward on its own.
      elements.watchList.append(
        element(
          "p",
          `Loading ${daemonWatchTotal} watch hit${daemonWatchTotal === 1 ? "" : "s"}…`,
        ),
      );
    } else if (snapshotWatchItems.length === 0) {
      elements.watchList.append(element("p", "Your watch list is clear."));
    } else if (watchItems.length === 0) {
      elements.watchList.append(element("p", `No watch hits match "${state.filterQuery.trim()}".`));
    }
  }
  if (state.snapshot?.unsupported_items_count && state.snapshot.unsupported_items_count > 0) {
    elements.list.append(element("p", `${state.snapshot.unsupported_items_count} newer item(s) need a newer extension.`));
  }

  // Pagination is panel-scoped: the Activity tab pages itself, and a Watch
  // panel with nothing loaded suppresses Load more ONLY when the daemon
  // agrees there is nothing to fetch.
  const snapshotHasWatch = (state.snapshot?.items ?? []).some((item) => item.kind === "watch_hit");
  const watchPending = (state.counts?.watch_hits ?? 0) > 0;
  const loadMoreSuppressed =
    state.activeTab === "activity" ||
    (state.activeTab === "watch" && !snapshotHasWatch && !watchPending);
  elements.loadMore.hidden = state.snapshot?.has_more !== true || loadMoreSuppressed;
  elements.loadMore.disabled = state.loading || !state.connected;
  if (state.generatedAt === null) {
    elements.generatedAt.textContent = "generated at —";
    elements.generatedAt.removeAttribute("datetime");
  } else {
    elements.generatedAt.dateTime = state.generatedAt;
    elements.generatedAt.textContent = `generated at ${state.generatedAt}`;
  }

  renderDialog();
  renderFeedbackStrip();
  if (state.focusSelectionAfterRender) {
    state.focusSelectionAfterRender = false;
    if (state.selectedID !== null) rowForItem(state.selectedID)?.focus();
  }
}

function adjustCounts(item: TriageSnapshotItem, delta: number): void {
  const counts = state.counts;
  if (counts === null) return;
  const shift = (value: number): number => Math.max(0, value + delta);
  const next: TriageCounts = {
    ...counts,
    pending_total: shift(counts.pending_total),
    watch_hits: item.kind === "watch_hit" ? shift(counts.watch_hits) : counts.watch_hits,
    actions: item.kind === "human_action" ? shift(counts.actions) : counts.actions,
    retractions: item.kind === "retraction" ? shift(counts.retractions) : counts.retractions,
  };
  // The header line is turns_required alone, so it has to move with a local
  // removal the way the inventory counts already do. Only a row the daemon
  // marked "required" is a turn; "working" and "advisory" rows are not, and a
  // row with no attention at all leaves the total alone rather than guessing.
  const required = counts.turns_required;
  if (typeof required === "number" && item.attention === "required") {
    next.turns_required = shift(required);
  }
  state.counts = next;
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
  state.grabPickers.delete(itemID);
  adjustCounts(removed, -1);
  if (state.selectedID === itemID) {
    const next = items[index + 1] ?? items[index - 1] ?? null;
    state.selectedID = next?.id ?? null;
    state.focusSelectionAfterRender = true;
  }
}

// restoreItem puts an optimistically removed row back. orderedItems re-sorts
// by kind and rank, so appending restores its original position.
function restoreItem(item: TriageSnapshotItem): void {
  if (state.snapshot === null || state.snapshot.items.some((existing) => existing.id === item.id)) return;
  state.snapshot = { ...state.snapshot, items: [...state.snapshot.items, item] };
  adjustCounts(item, 1);
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
async function readWaitingSessionJobs(): Promise<Map<string, number>> {
  try {
    const response = await runtimeMessage("papio.triage.waiting", {});
    if (!isRecord(response) || response["ok"] !== true || !Array.isArray(response["waiting_jobs"])) {
      return new Map();
    }
    const jobs = new Map<string, number>();
    for (const value of response["waiting_jobs"]) {
      if (!isRecord(value) || typeof value["job_id"] !== "string" || typeof value["deadline"] !== "number") continue;
      if (value["deadline"] > Date.now()) jobs.set(value["job_id"], value["deadline"]);
    }
    return jobs;
  } catch {
    return new Map();
  }
}

async function refreshActivity(loadMore = false): Promise<void> {
  if (!loadMore && state.activitySeenThroughSeq === null) {
    state.activitySeenThroughSeq = await loadActivityWatermark();
  }
  const request: Record<string, unknown> = { limit: 50 };
  if (loadMore) {
    if (state.activityCursor === undefined) return;
    request["before_seq"] = state.activityCursor;
  } else if (state.activitySeenThroughSeq !== null && state.activitySeenThroughSeq > 0) {
    request["seen_through_seq"] = String(state.activitySeenThroughSeq);
  }
  const result = await runtimeMessage("papio.activity", request)
    .then(activityResponse)
    .catch(() => null);
  if (result === null) return;
  state.activityKnown = true;
  state.activityFeature = result.feature;
  state.activityLimited = result.feature && !result.paged;
  if (!result.feature) {
    state.activityEntries = [];
    state.activityHasMore = false;
    state.activityCursor = undefined;
    return;
  }
  const entries = loadMore
    ? [...state.activityEntries, ...result.entries.filter((entry) => !state.activityEntries.some((existing) => existing.seq === entry.seq))]
    : result.entries;
  state.activityEntries = entries.sort((left, right) => right.seq - left.seq);
  state.activityHasMore = result.hasMore;
  state.activityCursor = result.cursor;
  state.activityLatestSeq = Math.max(state.activityLatestSeq, result.latestSeq);
  state.activityGap = result.gap;
  if (result.paged && result.newCountSince !== undefined && !result.gap && !loadMore) {
    state.activityNewCount = Math.min(1_000_000, result.newCountSince);
  } else if (result.paged && result.gap) {
    state.activityNewCount = 0;
  }
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
    .catch((error: unknown) => ({ ok: false as const, code: "connection_lost", message: error instanceof Error ? error.message : "The daemon is unavailable." }));
  const countsPromise = append
    ? Promise.resolve({ ok: false as const, message: "Counts were not refreshed." })
    : runtimeMessage("papio.triage.counts", {})
      .then((response) => responseValue<TriageCounts>(response, "counts"))
      .catch((error: unknown) => ({ ok: false as const, message: error instanceof Error ? error.message : "The daemon is unavailable." }));
  const pulsePromise = append ? Promise.resolve(undefined) : requestWorkPulse();
  const waitingPromise = readWaitingSessionJobs();
  // Piggybacks on the same wave, not the lightweight counts poll: the picker
  // gate only needs to be as fresh as the row it renders on, and this read
  // already happens exactly when a full snapshot does.
  const daemonFeaturesPromise = append ? Promise.resolve() : loadDaemonFeatures();
  const [snapshotResult, countsResult, pulseResult, waitingResult] = await Promise.all([
    snapshotPromise,
    countsPromise,
    pulsePromise,
    waitingPromise,
    daemonFeaturesPromise,
  ]);
  state.loading = false;
  if (!append) state.pulse = pulseResult;

  if (snapshotResult.ok) {
    const snapshot = {
      ...snapshotResult.value,
      items: snapshotResult.value.items.map(normalizeSnapshotItem),
    };
    state.snapshot = append && state.snapshot !== null
      ? { ...snapshot, items: [...state.snapshot.items, ...snapshot.items] }
      : snapshot;
    state.counts = snapshot.counts;
    state.generatedAt = snapshot.generated_at;
    // The banner is hidden the moment the daemon answers, so a "connected"
    // sentence has nowhere to render; only the failure message is read back.
    setConnection(true, "");
  } else {
    setConnection(false, snapshotResult.message, snapshotResult.code);
  }
  if (countsResult.ok) state.counts = countsResult.value;
  if (waitingResult !== null) {
    state.waitingJobs = waitingResult;
    scheduleWaitingOverlayExpiry();
  }
  if (!append) await refreshActivity();
  render();
  autoLoadWatchHits();
}

/** Watch hits stream behind actions in the snapshot pages, so the daemon can
 * know hits exist while none are loaded yet. Nobody should have to click
 * Load more to see a list the tab already counts — page forward automatically,
 * bounded so a count/stream disagreement can never loop forever. */
const AUTO_WATCH_PAGE_LIMIT = 8;
let autoWatchPagesUsed = 0;

function autoLoadWatchHits(): void {
  const snapshot = state.snapshot;
  if (snapshot === null || state.loading || !state.connected) return;
  const wanted = (state.counts?.watch_hits ?? 0) > 0;
  const loaded = snapshot.items.some((item) => item.kind === "watch_hit");
  if (!wanted || loaded) {
    autoWatchPagesUsed = 0;
    return;
  }
  if (snapshot.has_more !== true || autoWatchPagesUsed >= AUTO_WATCH_PAGE_LIMIT) return;
  autoWatchPagesUsed += 1;
  void refreshInbox(true);
}

// An explicit refresh commits any queued dismissal first: the incoming
// snapshot still contains those rows, and resurrecting a row the user just
// dismissed would be worse than losing the undo window early.
function requestRefresh(): void {
  if (state.dismissalCommitInFlight) return;
  void commitDismissals().then(() => refreshInbox());
}
// Inbox freshness. The poll is visibility-gated and only refetches the full
// snapshot when the counts signature changes, keeping steady-state traffic to
// a lightweight counts request. refresh-on-return covers coming back to the
// tab. Auto-refresh (poll and return alike) is suppressed while the user is
// mid-action so it never reorders the list under a decision.
const COUNTS_POLL_INTERVAL_MS = 15000;
let countsPollTimer: number | Timer | undefined;
/** The document this page bootstrapped against; the poll below stops once the
 * live document is no longer it. The counts poll re-arms itself forever, so it
 * must stop when its own page is gone. In a browser the timer dies with the
 * page and this is always an identity match; a page module that outlives its
 * document would otherwise keep polling the daemon against whatever document
 * replaced it. */
let boundDocument: Document | undefined;

function countsSignature(counts: TriageCounts | null): string {
  if (counts === null) return "";
  return JSON.stringify([
    counts.pending_total,
    counts.watch_hits,
    counts.actions,
    counts.retractions,
    counts.jobs_working,
    counts.jobs_needs_review,
    counts.failure_groups_7d,
    counts.turns_required,
    counts.turns_working,
    counts.family_breakdown_complete,
    counts.family_runs,
    counts.required_turns_complete,
    counts.required_turns,
  ]);
}

const INBOX_PRESENCE_INSTANCE_ID = (() => {
  const source = typeof crypto !== "undefined" && typeof crypto.randomUUID === "function"
    ? crypto.randomUUID()
    : `${Math.random().toString(36).slice(2)}${Date.now().toString(36)}`;
  return source.replace(/-/g, "").slice(0, 64).padEnd(8, "0");
})();

function sendInboxPresence(focused: boolean): void {
  if (typeof chrome === "undefined" || !chrome.runtime?.sendMessage) return;
  void Promise.resolve(chrome.runtime.sendMessage({
    type: "papio.surface.presence",
    payload: {
      instance_id: INBOX_PRESENCE_INSTANCE_ID,
      surface: "inbox",
      focused,
      at: new Date().toISOString(),
    },
  })).catch(() => undefined);
}

function autoRefreshAllowed(): boolean {
  if (boundDocument !== undefined && globalThis.document !== boundDocument) return false;
  if (!state.connected || state.loading) return false;
  if (
    state.confirmation !== null ||
    state.pending.size > 0 ||
    state.dismissals.length > 0 ||
    state.dismissalCommitInFlight
  ) return false;
  return typeof document === "undefined" || document.visibilityState === "visible";
}

async function pollCounts(): Promise<void> {
  if (typeof document === "undefined" || document.visibilityState === "visible") sendInboxPresence(true);
  if (!autoRefreshAllowed()) return;
  const before = countsSignature(state.counts);
  const [result, pulse] = await Promise.all([
    runtimeMessage("papio.triage.counts", {})
      .then((response) => responseValue<TriageCounts>(response, "counts"))
      .catch(() => ({ ok: false as const, message: "" })),
    requestWorkPulse(),
  ]);
  state.pulse = pulse;
  // A failed counts poll means the port is healing; the background worker and
  // refreshInbox's own reconnect already own that recovery, so stay quiet.
  if (!result.ok) return;
  // The user may have started interacting during the await; re-check before
  // pulling a full snapshot out from under them.
  if (!autoRefreshAllowed()) return;
  if (countsSignature(result.value) !== before) {
    await refreshInbox();
  } else {
    const focused = captureFocusTarget();
    state.counts = result.value;
    await refreshActivity();
    render();
    restoreFocusTarget(focused);
  }
}

function scheduleCountsPoll(): void {
  clearTimeout(countsPollTimer);
  if (boundDocument !== undefined && globalThis.document !== boundDocument) return;
  countsPollTimer = setTimeout(() => {
    void pollCounts().finally(scheduleCountsPoll);
  }, COUNTS_POLL_INTERVAL_MS);
}

function refreshOnReturn(): void {
  if (typeof document !== "undefined") {
    if (document.visibilityState !== "visible") {
      sendInboxPresence(false);
      return;
    }
    sendInboxPresence(true);
  }
  if (
    state.loading ||
    state.confirmation !== null ||
    state.pending.size > 0 ||
    state.dismissals.length > 0 ||
    state.dismissalCommitInFlight
  ) return;
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

// failMutationOffline handles a thrown runtime call — the broker/worker was
// unreachable, which IS a connectivity problem.
function failMutationOffline(item: TriageSnapshotItem, error: unknown): void {
  state.pending.delete(item.id);
  const message = error instanceof Error ? error.message : "The daemon is unavailable.";
  setConnection(false, message);
  operationMessage(item.id, message, "offline");
  render();
}

async function finishMutation(item: TriageSnapshotItem, response: unknown): Promise<void> {
  const result = resultForMutation(response);
  state.pending.delete(item.id);
  if (!result.ok) {
    // A structured broker rejection proves the messaging path is alive —
    // render it on the row instead of faking a daemon disconnect. Genuine
    // transport failures go through failMutationOffline.
    operationMessage(item.id, result.message, "error");
    render();
    return;
  }
  switch (result.outcome) {
    case "applied":
    case "already_applied":
      announce(result.outcome === "applied" ? "Change applied." : "Change was already applied.");
      removeItem(item.id);
      if (state.successAckMode === "all") showFeedback(result.outcome === "applied" ? "Change applied." : "Change was already applied.");
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

// acquire drives the watch_hit-only triage-decide RPC. Dismissal of any kind
// goes through the deferred queue below instead.
async function acquire(item: TriageSnapshotItem): Promise<void> {
  if (!beginMutation(item)) return;
  try {
    const response = await runtimeMessage("papio.triage.decide", { item_id: item.id, op: "acquire" });
    await finishMutation(item, response);
  } catch (error) {
    failMutationOffline(item, error);
  }
}

// requestDeliveryReconcile drives Decision 4's confirm_request_exists/absent
// mutations. open_request_history is deliberately not here — it never
// mutates anything and is handled by expanding the item's own disclosure
// (activateOperation below).
async function requestDeliveryReconcile(
  item: TriageSnapshotItem,
  operation: "confirm_request_exists" | "confirm_request_absent",
  providerReference?: string,
): Promise<void> {
  if (item.job_id === undefined) return;
  if (!beginMutation(item)) return;
  try {
    const request: Record<string, unknown> = { job_id: item.job_id, operation };
    if (providerReference !== undefined) request["provider_reference"] = providerReference;
    const response = await runtimeMessage("papio.delivery.reconcile", request);
    await finishMutation(item, response);
  } catch (error) {
    failMutationOffline(item, error);
  }
}

// Dismissal is a deferred, undoable batch: the row leaves the list at once and
// the daemon call is held for UNDO_WINDOW_MS, so Undo is exact — nothing has
// happened yet. That ordering is what makes a confirmation dialog unnecessary.
// The daemon cannot reverse a dismissal (DismissHumanAction cancels the parked
// job, and Retry refuses a cancelled job), so the modal bought no recovery at
// all — only a second click on every single row.
const UNDO_WINDOW_MS = 6000;
const ACKNOWLEDGEMENT_WINDOW_MS = 4000;
const FEEDBACK_EXIT_MS = 140;
const FEEDBACK_ENTER_MS = 180;
let undoTimer: number | Timer | undefined;
let feedbackTimer: number | Timer | undefined;

function prefersReducedMotion(): boolean {
  return typeof globalThis.matchMedia === "function"
    && globalThis.matchMedia("(prefers-reduced-motion: reduce)").matches;
}

function feedbackExitDelay(): number {
  return prefersReducedMotion() ? 0 : FEEDBACK_EXIT_MS;
}

function enqueueSuccessFeedback(entry: QueuedFeedback): void {
  state.feedbackQueue.push(entry);
  if (state.feedbackQueue.length > 3) {
    const totalCount = state.feedbackQueue.reduce((sum, item) => sum + item.count, 0);
    state.feedbackQueue = [{ text: `${totalCount} actions completed.`, count: totalCount }];
  }
}

function scheduleFeedbackDismiss(): void {
  clearTimeout(feedbackTimer);
  const notice = state.feedbackNotice;
  if (notice === null) return;
  const remaining = notice.deadline - Date.now();
  feedbackTimer = setTimeout(() => {
    beginFeedbackExit();
  }, Math.max(0, remaining));
}

function beginFeedbackExit(): void {
  if (state.feedbackNotice === null) return;
  state.feedbackNotice.phase = "exit";
  renderFeedbackStrip();
  clearTimeout(feedbackTimer);
  feedbackTimer = setTimeout(() => {
    state.feedbackNotice = null;
    const next = state.feedbackQueue.shift();
    if (next !== undefined) presentSuccessFeedback(next);
    render();
  }, feedbackExitDelay());
}

function presentSuccessFeedback(entry: QueuedFeedback): void {
  if (state.successAckMode !== "all") return;
  const now = Date.now();
  state.feedbackNotice = {
    text: entry.text,
    count: entry.count,
    deadline: now + ACKNOWLEDGEMENT_WINDOW_MS,
    phase: prefersReducedMotion() ? "visible" : "enter",
  };
  clearTimeout(feedbackTimer);
  if (prefersReducedMotion()) {
    scheduleFeedbackDismiss();
  } else {
    feedbackTimer = setTimeout(() => {
      if (state.feedbackNotice !== null) state.feedbackNotice.phase = "visible";
      renderFeedbackStrip();
      scheduleFeedbackDismiss();
    }, FEEDBACK_ENTER_MS);
  }
  renderFeedbackStrip();
}

// Mirrors dismissalCancelsParkedJob in internal/job/job.go: a dismiss cancels
// work only when the job is parked on THIS action. Everything else — advisory
// openurl_available rows, actions left behind on a job that moved on, watch
// hits — just closes a dead row. The snapshot already carries both inputs, so
// the consequence is known client-side without a protocol change. A human
// action whose job_state is missing counts as destructive.
//
// from that case's list: the pending download is fine, only the Downloads
// folder grant is missing, so dismissing it must never cancel the job.
const DISMISS_DISPOSITION: Record<string, "cancels_parked_job" | "never_cancels"> = {
  "openurl_handoff": "cancels_parked_job",
  "manual_download": "cancels_parked_job",
  "openurl_available": "cancels_parked_job",
  "verify_identity": "cancels_parked_job",
  "unsafe_pdf": "cancels_parked_job",
  "document_delivery": "cancels_parked_job",
  "downloads_access_required": "never_cancels",
};

function dismissCancelsJob(item: TriageSnapshotItem): boolean {
  if (item.kind !== "human_action") return false;
  const disposition = DISMISS_DISPOSITION[item.action_kind ?? ""];
  switch (item.job_state) {
    case "awaiting_human":
      return disposition === "cancels_parked_job" &&
        (item.action_kind === "openurl_handoff" || item.action_kind === "manual_download" || item.action_kind === "openurl_available" || item.action_kind === "document_delivery");
    case "needs_review":
      return disposition === "cancels_parked_job" && (item.action_kind === "verify_identity" || item.action_kind === "unsafe_pdf");
    case undefined:
      return true;
    default:
      return false;
  }
}

function boundedTitle(item: TriageSnapshotItem): string {
  const text = displayTitle(item).text;
  return text.length <= 60 ? `“${text}”` : `“${text.slice(0, 59)}…”`;
}

function undoSummary(): string {
  const entries = state.dismissals;
  const first = entries[0];
  if (entries.length === 1 && first !== undefined) {
    const title = boundedTitle(first.item);
    return first.cancelsJob ? `Dismissed ${title} and cancelled its acquisition.` : `Dismissed ${title}.`;
  }
  const cancelled = entries.filter((entry) => entry.cancelsJob).length;
  if (cancelled === 0) return `Dismissed ${entries.length} items.`;
  return `Dismissed ${entries.length} items — ${cancelled} cancelled an acquisition.`;
}

function renderFeedbackStrip(): void {
  if (elements === null) return;
  const bar = elements.undoBar;
  const message = elements.undoMessage;
  const button = elements.undoButton;

  if (state.dismissals.length > 0) {
    bar.dataset.kind = "undo";
    bar.classList.remove("is-enter", "is-exit");
    message.textContent = undoSummary();
    button.textContent = "Undo";
    button.hidden = !state.undoUndoable;
    button.disabled = !state.undoUndoable;
    bar.hidden = false;
    return;
  }

  if (state.feedbackNotice !== null) {
    bar.dataset.kind = "success";
    message.textContent = state.feedbackNotice.text;
    button.hidden = true;
    button.disabled = false;
    bar.hidden = false;
    bar.classList.toggle("is-enter", state.feedbackNotice.phase === "enter");
    bar.classList.toggle("is-exit", state.feedbackNotice.phase === "exit");
    return;
  }

  bar.hidden = true;
  bar.removeAttribute("data-kind");
  bar.classList.remove("is-enter", "is-exit");
  button.hidden = false;
  button.disabled = false;
}

function showFeedback(text: string, count = 1): void {
  if (state.successAckMode !== "all") return;
  const entry: QueuedFeedback = { text, count };
  if (state.dismissals.length > 0 || state.dismissalCommitInFlight || state.feedbackNotice !== null) {
    enqueueSuccessFeedback(entry);
    renderFeedbackStrip();
    return;
  }
  presentSuccessFeedback(entry);
}

function scheduleUndoDeadline(): void {
  clearTimeout(undoTimer);
  if (boundDocument !== undefined && globalThis.document !== boundDocument) return;
  const remaining = (state.undoDeadline ?? 0) - Date.now();
  undoTimer = setTimeout(() => {
    state.undoUndoable = false;
    renderFeedbackStrip();
    void commitDismissals();
  }, Math.max(0, remaining));
}

function scheduleDismissal(item: TriageSnapshotItem): void {
  if (!state.connected) {
    operationMessage(item.id, "Daemon unavailable — reconnecting automatically.", "offline");
    render();
    return;
  }
  if (state.pending.has(item.id) || state.dismissals.some((entry) => entry.item.id === item.id)) return;
  if (item.kind === "human_action" && (typeof item.action_id !== "number" || typeof item.revision !== "number")) {
    operationMessage(item.id, "This action is missing its revision and cannot be changed.", "error");
    render();
    return;
  }
  state.dismissals.push({ item, cancelsJob: dismissCancelsJob(item) });
  removeItem(item.id);
  state.undoDeadline = Date.now() + UNDO_WINDOW_MS;
  state.undoUndoable = true;
  scheduleUndoDeadline();
  announce(`${undoSummary()} Press u to undo.`);
  render();
}

// takeDismissals empties the queue before anything can await, so the window
// timer, a refresh, and a page-hide flush can never commit or restore the same
// entry twice.
function takeDismissals(): PendingDismissal[] {
  const entries = state.dismissals;
  state.dismissals = [];
  state.undoDeadline = null;
  state.undoUndoable = false;
  clearTimeout(undoTimer);
  if (elements !== null && document.activeElement === elements.undoButton) state.focusSelectionAfterRender = true;
  renderFeedbackStrip();
  return entries;
}

function undoDismissals(): void {
  if (!state.undoUndoable || state.dismissals.length === 0) return;
  const entries = takeDismissals();
  const first = entries[0];
  if (first === undefined) return;
  for (const entry of entries) restoreItem(entry.item);
  state.selectedID = first.item.id;
  state.focusSelectionAfterRender = true;
  announce(entries.length === 1 ? "Dismissal undone." : `${entries.length} dismissals undone.`);
  render();
}

async function commitDismissals(): Promise<void> {
  const entries = takeDismissals();
  if (entries.length === 0) return;
  state.dismissalCommitInFlight = true;
  render();
  let applied = 0;
  let conflicted = false;
  try {
    for (const entry of entries) {
      const outcome = await sendDismissal(entry.item);
      if (outcome === "applied") applied += 1;
      if (outcome === "conflict") conflicted = true;
    }
    if (applied === entries.length) {
      const message = applied === 1 ? "Dismissal applied." : `${applied} dismissals applied.`;
      announce(message);
      showFeedback(message, applied);
    }
    if (conflicted) await refreshInbox();
  } finally {
    state.dismissalCommitInFlight = false;
    const next = state.feedbackQueue.shift();
    if (next !== undefined && state.feedbackNotice === null) presentSuccessFeedback(next);
    render();
  }
}

// A failed dismissal puts its row back rather than vanishing silently: the
// daemon still holds the item, so the inbox must too.
async function sendDismissal(item: TriageSnapshotItem): Promise<"applied" | "conflict" | "failed"> {
  try {
    const response = item.kind === "human_action"
      ? await runtimeMessage("papio.action.resolve", {
        action_id: item.action_id,
        verdict: "dismiss",
        expected_revision: item.revision,
      })
      : await runtimeMessage("papio.triage.decide", { item_id: item.id, op: "dismiss", watch_scope: "all" });
    const result = resultForMutation(response);
    if (!result.ok) {
      restoreItem(item);
      operationMessage(item.id, result.message, "error");
      return "failed";
    }
    switch (result.outcome) {
      case "applied":
      case "already_applied":
        return "applied";
      case "conflict":
        restoreItem(item);
        operationMessage(item.id, "changed elsewhere — refreshed", "info");
        return "conflict";
      case "error":
        restoreItem(item);
        operationMessage(item.id, result.detail ?? "The daemon could not dismiss this item.", "error");
        return "failed";
    }
  } catch (error) {
    restoreItem(item);
    failMutationOffline(item, error);
    return "failed";
  }
}

// A hidden or unloading page can be discarded before the window closes, and a
// queued dismissal is the user's expressed intent — commit it instead of
// dropping it. Leaving the page waives the undo, which is the exception.
function flushDismissals(): void {
  void commitDismissals();
}

function flushDismissalsWhenHidden(): void {
  if (typeof document !== "undefined" && document.visibilityState === "visible") return;
  flushDismissals();
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
    failMutationOffline(item, error);
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

/** Fetched fresh on every click, never cached: the daemon's own suggest RPC
 * recomputes the candidate pool on every call for exactly this reason (see
 * SuggestGrabCandidates in internal/browser/bridge.go), so a stored list
 * here would name a job the pool has since filed or abandoned. */
async function requestGrabSuggestions(item: TriageSnapshotItem): Promise<void> {
  const grabID = item.grab?.grab_id;
  if (grabID === undefined) return;
  if (!state.connected) {
    operationMessage(item.id, "Daemon unavailable — reconnecting automatically.", "offline");
    render();
    return;
  }
  if (state.grabPickers.get(item.id)?.status === "loading") return;
  state.grabPickers.set(item.id, {
    status: "loading",
    documentIdentifiers: [],
    suggestions: [],
    truncated: false,
  });
  render();
  try {
    const response = await runtimeMessage("papio.grab.suggest", { grab_id: grabID });
    if (!isRecord(response) || response["ok"] !== true || typeof response["outcome"] !== "string") {
      state.grabPickers.delete(item.id);
      const message = errorFromResponse(response);
      setConnection(false, message);
      operationMessage(item.id, message, "offline");
      render();
      return;
    }
    const documentIdentifiers = Array.isArray(response["document_identifiers"])
      ? response["document_identifiers"].map(toGrabDocumentIdentifier).filter((entry): entry is GrabDocumentIdentifier => entry !== null)
      : [];
    const suggestions = Array.isArray(response["suggestions"])
      ? response["suggestions"].map(toGrabSuggestionRow).filter((entry): entry is GrabSuggestionRow => entry !== null)
      : [];
    state.grabPickers.set(item.id, {
      status: "loaded",
      outcome: response["outcome"],
      ...(typeof response["detail"] === "string" ? { detail: response["detail"] } : {}),
      documentIdentifiers,
      suggestions,
      truncated: response["truncated"] === true,
    });
    render();
  } catch (error) {
    state.grabPickers.delete(item.id);
    const message = error instanceof Error ? error.message : "The daemon is unavailable.";
    setConnection(false, message);
    operationMessage(item.id, message, "offline");
    render();
  }
}

function clearGrabConfirming(itemID: string): void {
  const current = state.grabPickers.get(itemID);
  if (current === undefined) return;
  // The key is dropped rather than set to undefined: exactOptionalPropertyTypes
  // is on, so an explicit undefined is not assignable to an optional property.
  const { confirmingJobID: _settled, ...rest } = current;
  state.grabPickers.set(itemID, rest);
}

/** Binds the parked grab to the human's pick through the same fenced
 * operator_confirm path autonomous binding uses (ConfirmGrabCandidate). The
 * one outcome that must be unmistakable is refused_identity: the document's
 * own front matter names a different work than the pick, the bind was
 * refused, and nothing changed — extracted identity outranks a human pick,
 * unchanged from the autonomous path's own precedence. */
async function confirmGrabCandidate(item: TriageSnapshotItem, grabID: string, jobID: string): Promise<void> {
  const picker = state.grabPickers.get(item.id);
  if (picker === undefined || picker.status !== "loaded" || picker.confirmingJobID !== undefined) return;
  if (!beginMutation(item)) return;
  const { confirmNotice: _superseded, ...priorPicker } = picker;
  state.grabPickers.set(item.id, { ...priorPicker, confirmingJobID: jobID });
  render();
  try {
    const response = await runtimeMessage("papio.grab.confirm", { grab_id: grabID, job_id: jobID });
    state.pending.delete(item.id);
    if (!isRecord(response) || response["ok"] !== true || typeof response["outcome"] !== "string") {
      clearGrabConfirming(item.id);
      operationMessage(item.id, errorFromResponse(response), "error");
      render();
      return;
    }
    const outcome = response["outcome"];
    const detail = typeof response["detail"] === "string" ? response["detail"] : undefined;
    if (outcome === "job_created") {
      const matched = picker.suggestions.find((row) => row.job_id === jobID);
      const title = matched?.title !== undefined && matched.title !== "" ? matched.title : jobID;
      const message = `Filed as “${boundedProse(title, 60)}”.`;
      state.grabPickers.delete(item.id);
      announce(message);
      removeItem(item.id);
      if (state.successAckMode === "all") showFeedback(message);
      render();
      return;
    }
    if (outcome === "refused_identity") {
      clearGrabConfirming(item.id);
      const current = state.grabPickers.get(item.id);
      if (current !== undefined) {
        state.grabPickers.set(item.id, {
          ...current,
          confirmNotice: {
            text: `papio refused this pick — the file's own front matter names a different paper, so nothing was changed.${detail !== undefined ? ` (${detail})` : ""}`,
            tone: "error",
          },
        });
      }
      render();
      return;
    }
    if (outcome === "conflict") {
      clearGrabConfirming(item.id);
      operationMessage(item.id, "changed elsewhere — refreshed", "info");
      render();
      await refreshInbox();
      return;
    }
    clearGrabConfirming(item.id);
    operationMessage(item.id, detail ?? "The daemon could not confirm this pick.", "error");
    render();
  } catch (error) {
    clearGrabConfirming(item.id);
    failMutationOffline(item, error);
  }
}

async function requestHandoffOpen(item: TriageSnapshotItem): Promise<void> {
  const jobID = handoffJobID(item);
  if (jobID === null) {
    openFailure(item, { error: { message: "This browser handoff is missing its job identifier." } });
    return;
  }
  // Unlike daemon mutations, the broker open is local to the extension: the
  // background worker owns the native session, waits for hydration itself,
  // and returns a structured failure when it truly cannot open. Gating on the
  // inbox's (possibly lagging) connectivity view would block usable handoffs
  // during auto-reconnect backoff, so only deduplicate concurrent requests.
  if (state.pending.has(item.id)) return;
  state.pending.add(item.id);
  render();
  try {
    const response = await runtimeMessage("papio.handoff.open", { job_id: jobID });
    state.pending.delete(item.id);
    if (!isRecord(response) || response["ok"] !== true || response["opened"] !== true) {
      openFailure(item, response);
      return;
    }
    operationMessage(item.id, "Browser handoff opened.", "info");
    render();
  } catch (error) {
    state.pending.delete(item.id);
    const message = error instanceof Error ? error.message : "The daemon is unavailable.";
    setConnection(false, message);
    openFailure(item, { error: { message } }, "offline");
  }
}

async function requestManualDownloadOpen(item: TriageSnapshotItem): Promise<void> {
  const url = firstSafeLink(item);
  if (url === null) {
    openFailure(item, {
      error: { message: "This manual download has no safe link to open." },
    });
    return;
  }
  openNewTab(url);
  announce("Opened the manual-download link in a new tab.");
}


function activateOperation(item: TriageSnapshotItem, operation: TriageOperation): void {
  if (waitingSibling(item) && operation !== "dismiss") return;
  switch (operation) {
    case "acquire":
      void acquire(item);
      return;
    case "dismiss":
      scheduleDismissal(item);
      return;
    case "provide_identifier":
      if (item.kind === "pdf_grab" && grabPickerAvailable()) {
        void requestGrabSuggestions(item);
      } else {
        operationMessage(item.id, guidanceText(item, false) ?? "Provide an identifier with the papio grabs identify command.", "info");
        render();
      }
      return;
    case "accept":
    case "reject":
      requestConfirmation(item, operation);
      return;
    case "open": {
      if (item.kind === "human_action" && item.action_kind === "openurl_handoff") {
        void requestHandoffOpen(item);
        return;
      }
      if (item.kind === "human_action" && item.action_kind === "manual_download") {
        void requestManualDownloadOpen(item);
        return;
      }
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
    case "open_request_history": {
      const row = rowForItem(item.id);
      const toggle = row?.querySelector<HTMLButtonElement>(".item-debug-toggle") ?? null;
      if (toggle === null) {
        operationMessage(item.id, "No request history is available for this item.", "info");
        render();
        return;
      }
      toggle.click();
      announce("Showing the request's details.");
      return;
    }
    case "confirm_request_exists": {
      const providerReference = window.prompt(
        "Provider reference for this request (e.g. the ILLiad transaction number):",
        "",
      )?.trim();
      if (providerReference === undefined || providerReference === "") return;
      void requestDeliveryReconcile(item, "confirm_request_exists", providerReference);
      return;
    }
    case "confirm_request_absent":
      void requestDeliveryReconcile(item, "confirm_request_absent");
      return;
  }
}

function activatePrimary(item: TriageSnapshotItem): void {
  if (waitingSibling(item)) return;
  if (item.kind === "watch_hit" && hasOperation(item, "acquire")) {
    void acquire(item);
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
  const items = itemsForTab(state.activeTab);
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
      const target = items[current];
      if (current >= 0 && target !== undefined && hasOperation(target, "dismiss")) {
        event.preventDefault();
        scheduleDismissal(target);
      }
      return;
    }
    case "u":
      if (state.dismissals.length > 0 && state.undoUndoable) {
        event.preventDefault();
        undoDismissals();
      }
      return;
    case "o":
      if (current >= 0) {
        event.preventDefault();
        activateOperation(items[current]!, "open");
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
  const pulse = document.getElementById("inbox-pulse");
  const filterInput = document.getElementById("item-filter");
  const refresh = document.getElementById("refresh-inbox");
  const reconnect = document.getElementById("reconnect-daemon");
  const list = document.getElementById("item-list");
  const watchList = document.getElementById("watch-list");
  const actionsPanel = document.getElementById("actions-panel");
  const watchPanel = document.getElementById("watch-panel");
  const activityPanel = document.getElementById("activity-panel");
  const actionsTab = document.getElementById("actions-tab");
  const watchTab = document.getElementById("watch-tab");
  const activityTab = document.getElementById("activity-tab");
  const activityList = document.getElementById("activity-list");
  const activityNew = document.getElementById("activity-new");
  const activityShowMore = document.getElementById("activity-show-more");
  const operationStatus = document.getElementById("operation-status");
  const generatedAt = document.getElementById("generated-at");
  const loadMore = document.getElementById("load-more");
  const dialog = document.getElementById("confirm-dialog");
  const dialogMessage = document.getElementById("confirm-dialog-message");
  const dialogCancel = document.getElementById("confirm-cancel");
  const dialogConfirm = document.getElementById("confirm-submit");
  const citationStyle = document.getElementById("citation-style");
  const undoBar = document.getElementById("undo-bar");
  const undoMessage = document.getElementById("undo-message");
  const undoButton = document.getElementById("undo-dismiss");
  if (
    !(connection instanceof HTMLElement) ||
    !(pulse instanceof HTMLElement) ||
    !(counts instanceof HTMLElement) ||
    !(filterInput instanceof HTMLInputElement) ||
    !(refresh instanceof HTMLButtonElement) ||
    !(reconnect instanceof HTMLButtonElement) ||
    !(list instanceof HTMLElement) ||
    !(watchList instanceof HTMLElement) ||
    !(actionsPanel instanceof HTMLElement) ||
    !(watchPanel instanceof HTMLElement) ||
    !(activityPanel instanceof HTMLElement) ||
    !(actionsTab instanceof HTMLButtonElement) ||
    !(watchTab instanceof HTMLButtonElement) ||
    !(activityTab instanceof HTMLButtonElement) ||
    !(activityList instanceof HTMLElement) ||
    !(activityNew instanceof HTMLElement) ||
    !(activityShowMore instanceof HTMLButtonElement) ||
    !(operationStatus instanceof HTMLElement) ||
    !(generatedAt instanceof HTMLTimeElement) ||
    !(loadMore instanceof HTMLButtonElement) ||
    !(citationStyle instanceof HTMLSelectElement) ||
    !(dialog instanceof HTMLElement) ||
    !(dialogMessage instanceof HTMLElement) ||
    !(dialogCancel instanceof HTMLButtonElement) ||
    !(dialogConfirm instanceof HTMLButtonElement) ||
    !(undoBar instanceof HTMLElement) ||
    !(undoMessage instanceof HTMLElement) ||
    !(undoButton instanceof HTMLButtonElement)
  ) {
    return;
  }
  elements = {
    connection,
    counts,
    filterInput,
    pulse,
    refresh,
    reconnect,
    list,
    watchList,
    actionsPanel,
    watchPanel,
    activityPanel,
    actionsTab,
    watchTab,
    activityTab,
    activityList,
    activityNew,
    activityShowMore,
    operationStatus,
    generatedAt,
    loadMore,
    citationStyle,
    dialog,
    dialogMessage,
    dialogCancel,
    dialogConfirm,
    undoBar,
    undoMessage,
    undoButton,
  };
  refresh.addEventListener("click", requestRefresh);
  reconnect.addEventListener("click", requestRefresh);
  for (const tab of [actionsTab, watchTab, activityTab]) {
    if (!(tab instanceof HTMLButtonElement)) continue;
    tab.addEventListener("click", () => selectTab(tab.dataset.tab as InboxTab, false));
    tab.addEventListener("keydown", handleTabKeydown);
  }
  activityShowMore.addEventListener("click", () => {
    if (state.activityHasMore) void refreshActivity(true).then(render);
    else {
      state.activityExpanded = true;
      render();
    }
  });
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
    if (state.dismissalCommitInFlight) return;
    void commitDismissals().then(() => refreshInbox(true));
  });
  undoButton.addEventListener("click", undoDismissals);
  dialogCancel.addEventListener("click", () => closeDialog(true));
  dialogConfirm.addEventListener("click", () => {
    void resolveConfirmation();
  });
  document.addEventListener("keydown", handleKeyboard);
  document.addEventListener("keydown", trapDialogFocus);
  document.addEventListener("visibilitychange", refreshOnReturn);
  window.addEventListener("focus", refreshOnReturn);
  window.addEventListener("pagehide", flushDismissals);
  window.addEventListener("pagehide", () => sendInboxPresence(false));
  document.addEventListener("visibilitychange", flushDismissalsWhenHidden);
  boundDocument = document;
  void loadSuccessAckMode();
  sendInboxPresence(true);
  render();
  startConnectionGrace();
  void refreshInbox();
  scheduleCountsPoll();
}

if (typeof document !== "undefined") {
  if (document.getElementById("item-list") !== null) bootstrap();
  else document.addEventListener("DOMContentLoaded", bootstrap, { once: true });
}
