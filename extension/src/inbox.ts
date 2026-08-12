// Copyright 2026 OrgMentem. Licensed under MIT.

import { derivePulseDisplay, requestWorkPulse, type PopupPulseCache } from "./popup";
import { getSuccessAckMode, type SuccessAckMode } from "./state";
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

type FeedbackNotice = { text: string; deadline: number };


// One dismissal waiting out its undo window. The item is kept whole so an undo
// can put the exact row back without a round trip.
interface PendingDismissal {
  item: TriageSnapshotItem;
  cancelsJob: boolean;
}

interface PageState {
  snapshot: Snapshot | null;
  counts: TriageCounts | null;
  pulse: PopupPulseCache | undefined;
  successAckMode: SuccessAckMode;
  generatedAt: string | null;
  connected: boolean;
  connectionMessage: string;
  selectedID: string | null;
  pending: Set<string>;
  previewed: Set<string>;
  itemMessages: Map<string, { text: string; tone: "info" | "error" | "offline" }>;
  confirmation: Confirmation | null;
  dismissals: PendingDismissal[];
  undoDeadline: number | null;
  dismissalCommitInFlight: boolean;
  feedbackNotice: FeedbackNotice | null;
  feedbackQueue: string[];
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
  selectedID: null,
  pending: new Set(),
  previewed: new Set(),
  itemMessages: new Map(),
  confirmation: null,
  dismissals: [],
  undoDeadline: null,
  dismissalCommitInFlight: false,
  feedbackNotice: null,
  feedbackQueue: [],
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

function responseValue<T>(value: unknown, key: string): { ok: true; value: T } | { ok: false; message: string } {
  if (isRecord(value) && value["ok"] === true && key in value) {
    return { ok: true, value: value[key] as T };
  }
  return { ok: false, message: errorFromResponse(value) };
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
function announce(text: string): void {
  if (elements !== null) elements.operationStatus.textContent = text;
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

/** One-line bound for text papio did not author: collapse whitespace, cap the
 * length, and mark a truncation with an ellipsis. Every surface that prints
 * untrusted daemon or runtime prose goes through this. */
function boundedProse(text: string, limit: number): string {
  const message = text.replace(/\s+/g, " ").trim();
  return message.length <= limit ? message : `${message.slice(0, limit - 3)}…`;
}

function handoffFailure(item: TriageSnapshotItem, value: unknown, tone: "error" | "offline" = "error"): void {
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
  state.itemMessages.set(itemID, { text, tone });
  if (elements !== null) elements.operationStatus.textContent = text;
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

function operationButton(item: TriageSnapshotItem, operation: TriageOperation): HTMLButtonElement | null {
  // Retry is reserved for a newer inbox contract. Do not expose a dead,
  // permanently disabled placeholder when the daemon includes it.
  if (operation === "retry") return null;
  const label = operationLabel(operation);
  const title = displayTitle(item).text;
  const button = element("button", label);
  button.type = "button";
  button.dataset.operation = operation;
  button.dataset.label = label;
  button.setAttribute("aria-label", `${label} ${title}`);

  const needsPreview = operation === "accept" && item.action_kind === "verify_identity";
  const handoff = item.kind === "human_action" && item.action_kind === "openurl_handoff";
  const unavailable = operation === "open" && (handoff ? handoffJobID(item) === null : firstSafeLink(item) === null);
  button.disabled =
    state.pending.has(item.id) ||
    unavailable ||
    (isMutation(operation) && !state.connected) ||
    (needsPreview && !hasViewedPreview(item));
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
function statusMeta(item: TriageSnapshotItem): { key: string; glyph: string; label: string } {
  const key = item.kind === "human_action" && typeof item.action_kind === "string" ? item.action_kind : item.kind;
  // A rejected file and a wrong work are not "one more PDF to download"; they
  // get their own glyph and accessible name so a scanning eye cannot read them
  // as an ordinary manual download.
  const manual = manualDownloadCopy(item);
  if (manual !== null) return { key: item.guidance_variant ?? key, glyph: manual.glyph, label: manual.statusLabel };
  const meta = STATUS_META[key];
  if (meta !== undefined) return { key, glyph: meta.glyph, label: meta.label };
  return { key: "unknown", glyph: "•", label: key.replaceAll("_", " ") };
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
  /** Hoisted family heading; the block appends its exact paper count. */
  heading: string;
  /** Hoisted family instruction, printed once per block. */
  instruction: string;
  /** The same imperative for a row that hoists no heading. */
  row: string;
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
    row: "Download the PDF — papio takes it from there.",
    glyph: "↓",
    statusLabel: "Manual download needed",
  },
  manual_download_adapter_missing: {
    heading: "Manual downloads · no adapter yet",
    instruction: "papio has no adapter for these providers yet. Download each PDF — papio takes it from there.",
    row: "papio has no adapter for this provider yet. Download the PDF — papio takes it from there.",
    glyph: "↓",
    statusLabel: "Manual download needed — no adapter for this provider yet",
  },
  manual_download_page_undriveable: {
    heading: "Manual downloads · page changed",
    instruction: "papio could not drive these provider pages. Download each PDF — papio takes it from there.",
    row: "papio could not drive this provider page. Download the PDF — papio takes it from there.",
    glyph: "↓",
    statusLabel: "Manual download needed — papio could not drive the page",
  },
  manual_download_rejected_file: {
    heading: "Replace rejected files",
    instruction: "The file papio adopted was not the paper. Download a different PDF for each.",
    row: "The file papio adopted was not the paper. Download a different PDF.",
    glyph: "↺",
    statusLabel: "Rejected file — download a different PDF",
  },
  manual_download_wrong_work: {
    heading: "Wrong paper reached",
    instruction: "papio landed on a different work. Find and download the requested PDF.",
    row: "papio landed on a different work. Find and download the requested PDF.",
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
    case "manual_download": {
      const manual = manualDownloadCopy(item);
      if (manual !== null) return manual.row;
      // A daemon older than the closed manual-download variants ships no
      // structured discriminator at all; its detail prose is the only signal
      // there is. Never consulted once a variant is present.
      return missingAdapter(item) ? "No adapter yet - download this PDF manually" : "Download the PDF yourself - papio adopts it";
    }
    case "openurl_handoff":
      return item.requires_auth === true ? "Sign in to your institution" : "Open the page";
    case "verify_identity":
      return "Review the PDF, then accept or reject";
    case "document_delivery":
      return "Confirm what the library has on file";
    case "downloads_access_required":
      return `papio can't read ${actionDetail(item) || "your Downloads folder"} — grant access in System Settings → Privacy & Security`;
    default:
      return null;
  }
}
function locallyRenderedGuidance(item: TriageSnapshotItem): string | null {
  if (waitingSibling(item)) return "papio is continuing — waiting for the institution sign-in already open in another tab";
  const blocked = challengeBlocked(item);
  if (blocked) return renderInstruction(item, true, item.attention === "working")?.textContent ?? null;
  const family = familyCopy(item);
  if (family !== null) return family.instruction;
  const guidance = guidanceText(item, false);
  if (guidance === null || guidance.trim() === "") return null;
  return item.attention === "working" ? `papio is continuing — ${guidance.charAt(0).toLowerCase()}${guidance.slice(1)}` : guidance;
}

interface FamilyRender {
  heading: string;
  instruction: string;
  descriptionID: string;
  total: number;
  shown: number;
  /** The loaded rows of this run do not share one reason, so each carries its
   * own summary line. When they do share it, printing it per row would be the
   * exact repetition hoisting exists to remove. */
  varyingReasons: boolean;
  /** The honest download route on this browser, printed once per block where
   * the hoisted instruction alone would over-promise. Null where papio can
   * take the download itself. */
  platformRoute: string | null;
}

function familyCopy(item: TriageSnapshotItem): { heading: string; instruction: string } | null {
  const manual = manualDownloadCopy(item);
  if (manual !== null) return { heading: manual.heading, instruction: manual.instruction };
  switch (item.guidance_variant) {
    case "institution_sign_in": return { heading: "Institution sign-in", instruction: "Sign in to your institution once — papio continues the waiting papers." };
    case "open_page": return { heading: "Pages to open", instruction: "Open each source page so papio can continue." };
    case "verify_identity": return { heading: "PDF identity review", instruction: "Review each PDF, then accept or reject it." };
    case "document_delivery": return { heading: "Document delivery", instruction: "Confirm what the library has on file for each request." };
    case "downloads_access": return { heading: "Downloads access", instruction: "Grant Downloads access so papio can adopt the pending files." };
    case "terms_acceptance": return { heading: "Publisher terms", instruction: "Review and accept the publisher terms for each source." };
    case "security_challenge": return { heading: "Security checks", instruction: "Solve each security check in its tab." };
    case "pdf_identifier": return { heading: "PDF identifiers", instruction: "Provide an identifier for each captured PDF." };
    case "papio_continuing": return { heading: "papio continuing", instruction: "papio is continuing automatically — no decision is needed." };
    default: return null;
  }
}

function familyForItem(item: TriageSnapshotItem, items: readonly TriageSnapshotItem[]): FamilyRender | null {
  if (state.snapshot?.schema !== 5 || item.run_key === undefined || item.next_actor === undefined ||
      item.guidance_variant === undefined || item.operation_variant === undefined) return null;
  const copy = familyCopy(item);
  const run = state.counts?.family_runs?.find((candidate) =>
    candidate.run_key === item.run_key &&
    candidate.guidance_variant === item.guidance_variant &&
    candidate.operation_variant === item.operation_variant &&
    candidate.next_actor === item.next_actor);
  if (copy === null || state.counts?.family_breakdown_complete !== true || run === undefined || run.count < 2) return null;
  const runItems = items.filter((candidate) => candidate.run_key === item.run_key);
  if (runItems.length < 2) return null;
  const first = items.findIndex((candidate) => candidate.run_key === item.run_key);
  const last = items.length - 1 - [...items].reverse().findIndex((candidate) => candidate.run_key === item.run_key);
  if (first < 0 || last < first || items.slice(first, last + 1).some((candidate) => candidate.run_key !== item.run_key)) return null;
  const guidance = locallyRenderedGuidance(item);
  if (guidance === null || runItems.some((candidate) =>
    candidate.next_actor !== item.next_actor ||
    candidate.guidance_variant !== item.guidance_variant ||
    candidate.operation_variant !== item.operation_variant ||
    locallyRenderedGuidance(candidate) !== guidance
  )) return null;
  const descriptionID = `family-guidance-${item.run_key.replace(/[^A-Za-z0-9_-]/g, "_")}`;
  const ownReason = reasonSummary(item, copy.instruction);
  const varyingReasons = runItems.some((candidate) => reasonSummary(candidate, copy.instruction) !== ownReason);
  const platformRoute = manualDownloadCopy(item) !== null && !downloadSteeringAvailable() ? NO_STEERING_ROUTE : null;
  return { ...copy, descriptionID, total: run.count, shown: runItems.length, varyingReasons, platformRoute };
}

/** The row's own durable reason, reduced to the leading clause the family
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
  const compact = reason.toLowerCase().replace(/[^a-z0-9]+/g, " ").trim();
  const said = instruction.toLowerCase().replace(/[^a-z0-9]+/g, " ").trim();
  return compact === "" || said.includes(compact) || compact.includes(said) ? null : reason;
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

// Supporting explanation and backend identifiers share one compact disclosure,
// preserving native button keyboard semantics and state.
function renderDebug(
  item: TriageSnapshotItem,
  blockedByChallenge: boolean,
): { toggle: HTMLButtonElement; list: HTMLDListElement } {
  const list = element("dl");
  list.className = "item-debug";
  list.hidden = true;
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
  toggle.setAttribute("aria-expanded", "false");
  toggle.setAttribute("aria-label", "More details");
  toggle.addEventListener("click", () => {
    const expanded = toggle.getAttribute("aria-expanded") === "true";
    toggle.setAttribute("aria-expanded", String(!expanded));
    list.hidden = expanded;
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
  const blockedByChallenge = challengeBlocked(item);
  const debug = renderDebug(item, blockedByChallenge);
  body.append(headingText);
  if (citation !== null) body.append(citation);
  const instruction: HTMLElement | null = family === null
    ? waiting
      ? (() => {
        const waitingInstruction = element("p", "papio is continuing — waiting for the institution sign-in already open in another tab");
        waitingInstruction.className = "item-instruction item-guidance";
        waitingInstruction.id = itemGuidanceID(item);
        return waitingInstruction;
      })()
      : renderInstruction(item, blockedByChallenge, working)
    : null;
  if (instruction !== null) body.append(instruction);
  // The hoisted instruction says what to do for the whole block; a row prints
  // its own reason only where that reason differs from its siblings', so the
  // block never degenerates back into one repeated sentence per row.
  const reasonText = family !== null && family.varyingReasons && !working && !blockedByChallenge
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
    result.setAttribute("role", "status");
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
    const describedBy = [family?.descriptionID ?? instruction?.id, routeID, reason?.id]
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
  if (heading !== null) section.append(element("h2", `${heading} (${items.length})`));
  // A card is one family block. A singleton family hoists no heading (that needs
  // two adjacent rows), so keying card edges on "the sibling is not a row" put a
  // headingless row inside the preceding family's card, where it visually
  // inherited that family's heading and count — a card headed "2 papers" held
  // four rows of three different kinds. Card edges therefore follow family
  // identity, and a row with no family is its own card.
  const families = items.map((item) => familyForItem(item, items));
  const cardKeys = families.map((family, index) => family?.descriptionID ?? `standalone:${items[index]!.id}`);

  let previousRun: string | undefined;
  for (const [index, item] of items.entries()) {
    const family = families[index]!;
    if (family !== null && family.descriptionID !== previousRun) {
      const familyHeading = element("h2", `${family.heading} · ${family.total} paper${family.total === 1 ? "" : "s"}`);
      familyHeading.className = "family-heading";
      familyHeading.id = `${family.descriptionID}-heading`;
      if (state.filterQuery.trim() !== "" && family.shown !== family.total) {
        familyHeading.append(element("span", ` (${family.shown} of ${family.total} shown)`));
      }
      section.append(familyHeading);
      const familyInstruction = element("p", family.instruction);
      familyInstruction.className = "item-instruction item-guidance family-guidance";
      familyInstruction.id = family.descriptionID;
      familyInstruction.setAttribute("aria-labelledby", familyHeading.id);
      section.append(familyInstruction);
      if (family.platformRoute !== null) {
        // Once per block, never per row: the hoisted imperative is the same on
        // every browser, but how the file reaches papio is not.
        const route = element("p", family.platformRoute);
        route.className = "item-instruction family-mechanism";
        route.id = `${family.descriptionID}-route`;
        section.append(route);
      }
      previousRun = family.descriptionID;
    } else if (family === null) {
      previousRun = undefined;
    }
    const row = renderItem(item, family);
    if (cardKeys[index] !== cardKeys[index - 1]) row.dataset.cardStart = "true";
    if (cardKeys[index] !== cardKeys[index + 1]) row.dataset.cardEnd = "true";
    section.append(row);
  }
  return section;
}


function renderPulse(): void {
  if (elements === null) return;
  const display = derivePulseDisplay(state.pulse, state.connected ? "connected" : "disconnected", Date.now(), 45_000);
  elements.pulse.textContent = display.primaryText;
  elements.pulse.dataset.state = display.primary.toLowerCase().replaceAll(" ", "-");
  elements.pulse.title = [display.buckets, display.next, display.capacity, display.batch].filter((part) => part !== "").join(" · ");
}
function genuinelyMovingCount(): number | undefined {
  if (!state.connected || state.pulse === undefined) return undefined;
  const display = derivePulseDisplay(state.pulse, "connected", Date.now(), 45_000);
  if (display.primary === "Unknown") return undefined;
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

function renderCounts(): void {
  if (elements === null) return;
  const counts = state.counts ?? state.snapshot?.counts;
  if (counts === undefined || counts === null) {
    elements.counts.textContent = "Counts unavailable";
    return;
  }
  // Counts-v3 turns_required is the authority for decisions owed by the
  // researcher; it must not be replaced with pulse.waiting_required.
  const required = counts.turns_required;
  const needText = typeof required === "number" ? `${required} need you` : `${counts.pending_total} open`;
  const reference = counts.watch_hits + counts.retractions;
  const parts = [`${counts.pending_total} open`, needText];
  if (reference > 0) parts.push(`${reference} for reference`);
  // Pulse in_flight + continuing is the only honest liveness authority;
  // jobs_working also includes queued, awaiting-human, and retry-wait work.
  const moving = genuinelyMovingCount();
  if (moving !== undefined && moving > 0) parts.push(`papio is working on ${moving}`);
  elements.counts.textContent = [...new Set(parts)].join(" · ");
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
  const isDisconnected = !state.connected;
  elements.connection.textContent = isDisconnected
    ? `Disconnected: ${state.connectionMessage} Reconnecting automatically — run papio status if this persists.`
    : state.connectionMessage;
  elements.connection.dataset.state = isDisconnected ? "disconnected" : "connected";
  elements.reconnect.hidden = !isDisconnected;
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
    const moving = genuinelyMovingCount();
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
  renderUndoBar();
  if (state.focusSelectionAfterRender) {
    state.focusSelectionAfterRender = false;
    if (state.selectedID !== null) rowForItem(state.selectedID)?.focus();
  }
}

function adjustCounts(item: TriageSnapshotItem, delta: number): void {
  const counts = state.counts;
  if (counts === null) return;
  const shift = (value: number): number => Math.max(0, value + delta);
  state.counts = {
    ...counts,
    pending_total: shift(counts.pending_total),
    watch_hits: item.kind === "watch_hit" ? shift(counts.watch_hits) : counts.watch_hits,
    actions: item.kind === "human_action" ? shift(counts.actions) : counts.actions,
    retractions: item.kind === "retraction" ? shift(counts.retractions) : counts.retractions,
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
    .catch((error: unknown) => ({ ok: false as const, message: error instanceof Error ? error.message : "The daemon is unavailable." }));
  const countsPromise = append
    ? Promise.resolve({ ok: false as const, message: "Counts were not refreshed." })
    : runtimeMessage("papio.triage.counts", {})
      .then((response) => responseValue<TriageCounts>(response, "counts"))
      .catch((error: unknown) => ({ ok: false as const, message: error instanceof Error ? error.message : "The daemon is unavailable." }));
  const pulsePromise = append ? Promise.resolve(undefined) : requestWorkPulse();
  const waitingPromise = readWaitingSessionJobs();
  const [snapshotResult, countsResult, pulseResult, waitingResult] = await Promise.all([snapshotPromise, countsPromise, pulsePromise, waitingPromise]);
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
    setConnection(true, "Connected to daemon.");
  } else {
    setConnection(false, snapshotResult.message);
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
const UNDO_TICK_MS = 250;
let undoTimer: number | Timer | undefined;
let feedbackTimer: number | Timer | undefined;

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
      return disposition === "cancels_parked_job" && item.action_kind === "verify_identity";
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

function renderUndoBar(): void {
  if (elements === null) return;
  if (state.dismissals.length > 0) {
    const remaining = Math.max(0, Math.ceil(((state.undoDeadline ?? 0) - Date.now()) / 1000));
    elements.undoMessage.textContent = undoSummary();
    elements.undoButton.textContent = `Undo (${remaining})`;
    elements.undoButton.hidden = false;
    elements.undoBar.hidden = false;
    return;
  }
  if (state.feedbackNotice !== null) {
    elements.undoMessage.textContent = state.feedbackNotice.text;
    elements.undoButton.hidden = true;
    elements.undoBar.hidden = false;
    return;
  }
  elements.undoBar.hidden = true;
  elements.undoButton.hidden = false;
}

function showFeedback(text: string): void {
  if (state.dismissals.length > 0 || state.dismissalCommitInFlight || state.feedbackNotice !== null) {
    if (!state.feedbackQueue.includes(text)) state.feedbackQueue.push(text);
    if (state.feedbackQueue.length > 3) {
      state.feedbackQueue = [`${state.feedbackQueue.length} actions completed.`];
    }
    renderUndoBar();
    return;
  }
  state.feedbackNotice = { text, deadline: Date.now() + ACKNOWLEDGEMENT_WINDOW_MS };
  clearTimeout(feedbackTimer);
  feedbackTimer = setTimeout(() => {
    state.feedbackNotice = null;
    const next = state.feedbackQueue.shift();
    if (next !== undefined) showFeedback(next);
    render();
  }, ACKNOWLEDGEMENT_WINDOW_MS);
  renderUndoBar();
}

function scheduleUndoTick(): void {
  clearTimeout(undoTimer);
  if (boundDocument !== undefined && globalThis.document !== boundDocument) return;
  undoTimer = setTimeout(() => {
    if (state.dismissals.length === 0) return;
    if (state.undoDeadline !== null && Date.now() >= state.undoDeadline) {
      void commitDismissals();
      return;
    }
    renderUndoBar();
    scheduleUndoTick();
  }, UNDO_TICK_MS);
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
  scheduleUndoTick();
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
  clearTimeout(undoTimer);
  if (elements !== null && document.activeElement === elements.undoButton) state.focusSelectionAfterRender = true;
  renderUndoBar();
  return entries;
}

function undoDismissals(): void {
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
      showFeedback(message);
    }
    if (conflicted) await refreshInbox();
  } finally {
    state.dismissalCommitInFlight = false;
    const next = state.feedbackQueue.shift();
    if (next !== undefined && state.feedbackNotice === null) showFeedback(next);
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

async function requestHandoffOpen(item: TriageSnapshotItem): Promise<void> {
  const jobID = handoffJobID(item);
  if (jobID === null) {
    handoffFailure(item, { error: { message: "This browser handoff is missing its job identifier." } });
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
      handoffFailure(item, response);
      return;
    }
    operationMessage(item.id, "Browser handoff opened.", "info");
    render();
  } catch (error) {
    state.pending.delete(item.id);
    const message = error instanceof Error ? error.message : "The daemon is unavailable.";
    setConnection(false, message);
    handoffFailure(item, { error: { message } }, "offline");
  }
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
      operationMessage(item.id, guidanceText(item, false) ?? "Provide an identifier with the papio grabs identify command.", "info");
      render();
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
      if (state.dismissals.length > 0) {
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
  void refreshInbox();
  scheduleCountsPoll();
}

if (typeof document !== "undefined") {
  if (document.getElementById("item-list") !== null) bootstrap();
  else document.addEventListener("DOMContentLoaded", bootstrap, { once: true });
}
