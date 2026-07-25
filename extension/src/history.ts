// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// History: a full-tab read-only page showing what papio has delivered — papers
// acquired, estimated time saved, success rate, access routes, and how often a
// human handoff was needed. All data arrives through one papio.stats runtime
// request to the background broker; the page never talks to the native host.
// Refresh happens on page open and whenever the tab becomes visible again.

import type { StatsAccess, StatsBucket } from "./protocol";
import { renderPapio } from "./dom";
import {
  EST_MINUTES_SAVED_PER_PAPER,
  formatHoursSaved,
  formatShare,
  parseStatsReply,
  type AcquisitionStats,
  type StatsReply,
} from "./stats";

interface PageElements {
  connection: HTMLElement;
  refresh: HTMLButtonElement;
  reconnect: HTMLButtonElement;
  main: HTMLElement;
  unavailable: HTMLElement;
  acquired: HTMLElement;
  timeSaved: HTMLElement;
  timeNote: HTMLElement;
  successRate: HTMLElement;
  successDetail: HTMLElement;
  chart: HTMLElement;
  chartPeak: HTMLElement;
  chartStart: HTMLElement;
  chartEnd: HTMLElement;
  chartEmpty: HTMLElement;
  accessList: HTMLElement;
  handoffRate: HTMLElement;
  handoffDetail: HTMLElement;
  generatedAt: HTMLTimeElement;
}

interface PageState {
  stats: AcquisitionStats | null;
  unavailableCode: string | null;
  connected: boolean;
  connectionMessage: string;
  loading: boolean;
}

const state: PageState = {
  stats: null,
  unavailableCode: null,
  connected: false,
  connectionMessage: "Connecting to daemon…",
  loading: false,
};

let elements: PageElements | null = null;

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
    void refreshStats();
  }, delay);
}

function setConnection(connected: boolean, message: string): void {
  state.connected = connected;
  state.connectionMessage = message;
  if (connected) cancelAutoReconnect();
  else scheduleAutoReconnect();
}

function element<K extends keyof HTMLElementTagNameMap>(tag: K, text?: string): HTMLElementTagNameMap[K] {
  const el = document.createElement(tag);
  if (text !== undefined) el.textContent = text;
  return el;
}

const WEEK_LABEL = new Intl.DateTimeFormat("en-US", { month: "short", day: "numeric", timeZone: "UTC" });

// period_start is RFC3339 UTC midnight of the bucket's Monday; formatting in
// UTC keeps that Monday from drifting a day in negative-offset timezones.
function weekLabel(periodStart: string): string {
  const parsed = new Date(periodStart);
  return Number.isNaN(parsed.getTime()) ? periodStart : WEEK_LABEL.format(parsed);
}

function renderChart(series: StatsBucket[]): void {
  if (elements === null) return;
  const peak = series.reduce((max, bucket) => Math.max(max, bucket.acquired), 0);
  const total = series.reduce((sum, bucket) => sum + bucket.acquired, 0);

  const columns = series.map((bucket) => {
    const column = element("div");
    column.className = "chart-col";
    column.title = `Week of ${weekLabel(bucket.period_start)} — ${bucket.acquired} paper${bucket.acquired === 1 ? "" : "s"}`;
    const bar = element("div");
    bar.className = "chart-bar";
    bar.style.height = peak === 0 ? "0%" : `${(bucket.acquired / peak) * 100}%`;
    // A one-paper week must stay visible even under a tall peak.
    if (bucket.acquired > 0) bar.style.minHeight = "2px";
    column.append(bar);
    return column;
  });
  elements.chart.replaceChildren(...columns);
  elements.chart.setAttribute(
    "aria-label",
    `Weekly acquisitions over the last ${series.length} weeks: ${total} total, peaking at ${peak} in a week.`,
  );

  const first = series.at(0);
  const last = series.at(-1);
  elements.chartStart.textContent = first === undefined ? "—" : weekLabel(first.period_start);
  elements.chartEnd.textContent = last === undefined ? "—" : weekLabel(last.period_start);
  elements.chartPeak.hidden = peak === 0;
  elements.chartPeak.textContent = `peak ${peak} / week`;
  elements.chartEmpty.hidden = total > 0;
}

const ACCESS_ROUTES: ReadonlyArray<{ key: keyof StatsAccess; label: string }> = [
  { key: "open_access", label: "Open access" },
  { key: "institutional", label: "Institutional access" },
  { key: "licensed_api", label: "Licensed API" },
  { key: "other", label: "Other routes" },
];

function renderAccess(stats: AcquisitionStats): void {
  if (elements === null) return;
  const rows = ACCESS_ROUTES.map(({ key, label }) => {
    const count = stats.access[key];
    const row = element("li");
    row.className = "access-row";
    const meter = element("span");
    meter.className = "access-meter";
    const fill = element("span");
    fill.className = "access-fill";
    fill.style.width = stats.acquired_total === 0 ? "0%" : `${(count / stats.acquired_total) * 100}%`;
    meter.append(fill);
    const labelEl = element("span", label);
    labelEl.className = "access-label";
    const countEl = element("span", `${count} · ${formatShare(count, stats.acquired_total)}`);
    countEl.className = "access-count";
    row.append(labelEl, meter, countEl);
    return row;
  });
  elements.accessList.replaceChildren(...rows);
}

function render(): void {
  if (elements === null) return;
  const disconnected = !state.connected;
  elements.connection.textContent = disconnected
    ? `Disconnected: ${state.connectionMessage} Reconnecting automatically — run papio status if this persists.`
    : state.connectionMessage;
  elements.connection.dataset.state = disconnected ? "disconnected" : "connected";
  elements.reconnect.hidden = !disconnected;
  elements.refresh.disabled = state.loading;
  elements.reconnect.disabled = state.loading;

  const stats = state.stats;
  elements.main.hidden = stats === null;
  elements.unavailable.hidden = stats !== null;
  if (stats === null) {
    renderPapio(
      elements.unavailable,
      state.unavailableCode === "feature_unavailable"
        ? "Your papio daemon doesn't publish stats yet — update papio to unlock your acquisition history."
        : "Stats are unavailable — connect the papio daemon to see your acquisition history.",
    );
    elements.generatedAt.textContent = "generated at —";
    elements.generatedAt.removeAttribute("datetime");
    return;
  }

  const finished = stats.acquired_total + stats.failed_total;
  elements.acquired.textContent = String(stats.acquired_total);
  elements.timeSaved.textContent = formatHoursSaved(stats.acquired_total * EST_MINUTES_SAVED_PER_PAPER);
  elements.timeNote.textContent = `Assumes ~${EST_MINUTES_SAVED_PER_PAPER} minutes of manual chasing per paper.`;
  elements.successRate.textContent = formatShare(stats.acquired_total, finished);
  elements.successDetail.textContent =
    finished === 0 ? "No finished acquisitions yet." : `${stats.acquired_total} acquired · ${stats.failed_total} failed`;

  renderChart(stats.series);
  renderAccess(stats);

  elements.handoffRate.textContent = formatShare(stats.handoffs_required, stats.acquired_total);
  if (stats.acquired_total === 0) {
    elements.handoffDetail.textContent = "No acquired papers yet.";
  } else {
    renderPapio(
      elements.handoffDetail,
      `${stats.handoffs_required} of ${stats.acquired_total} acquired papers needed a browser handoff — papio carried the rest end to end.`,
    );
  }

  elements.generatedAt.dateTime = stats.generated_at;
  elements.generatedAt.textContent = `generated at ${stats.generated_at}`;
}

async function refreshStats(): Promise<void> {
  state.loading = true;
  render();
  let reply: StatsReply;
  try {
    if (typeof chrome === "undefined" || !chrome.runtime?.sendMessage) {
      throw new Error("The extension runtime is unavailable.");
    }
    reply = parseStatsReply(await chrome.runtime.sendMessage({ type: "papio.stats", request: {} }));
  } catch (error) {
    reply = {
      ok: false,
      code: "transport_error",
      message: error instanceof Error ? error.message : "The daemon is unavailable.",
    };
  }
  state.loading = false;
  if (reply.ok) {
    state.stats = reply.stats;
    state.unavailableCode = null;
    setConnection(true, "Connected to daemon.");
  } else {
    state.unavailableCode = reply.code;
    setConnection(false, reply.message);
  }
  render();
}

// History freshness. The page refetches on bootstrap and again whenever the
// tab becomes visible after being hidden, which is how a history view is
// actually used: opened, left in a background tab, and returned to.

function refreshOnReturn(): void {
  if (typeof document !== "undefined" && document.visibilityState !== "visible") return;
  if (state.loading) return;
  void refreshStats();
}

function bootstrap(): void {
  const connection = document.getElementById("connection-status");
  const refresh = document.getElementById("refresh-history");
  const reconnect = document.getElementById("reconnect-daemon");
  const main = document.getElementById("stats-main");
  const unavailable = document.getElementById("stats-unavailable");
  const acquired = document.getElementById("stat-acquired");
  const timeSaved = document.getElementById("stat-time-saved");
  const timeNote = document.getElementById("stat-time-note");
  const successRate = document.getElementById("stat-success-rate");
  const successDetail = document.getElementById("stat-success-detail");
  const chart = document.getElementById("weekly-chart");
  const chartPeak = document.getElementById("chart-peak");
  const chartStart = document.getElementById("chart-start");
  const chartEnd = document.getElementById("chart-end");
  const chartEmpty = document.getElementById("chart-empty");
  const accessList = document.getElementById("access-list");
  const handoffRate = document.getElementById("stat-handoff-rate");
  const handoffDetail = document.getElementById("stat-handoff-detail");
  const generatedAt = document.getElementById("generated-at");
  if (
    !(connection instanceof HTMLElement) ||
    !(refresh instanceof HTMLButtonElement) ||
    !(reconnect instanceof HTMLButtonElement) ||
    !(main instanceof HTMLElement) ||
    !(unavailable instanceof HTMLElement) ||
    !(acquired instanceof HTMLElement) ||
    !(timeSaved instanceof HTMLElement) ||
    !(timeNote instanceof HTMLElement) ||
    !(successRate instanceof HTMLElement) ||
    !(successDetail instanceof HTMLElement) ||
    !(chart instanceof HTMLElement) ||
    !(chartPeak instanceof HTMLElement) ||
    !(chartStart instanceof HTMLElement) ||
    !(chartEnd instanceof HTMLElement) ||
    !(chartEmpty instanceof HTMLElement) ||
    !(accessList instanceof HTMLElement) ||
    !(handoffRate instanceof HTMLElement) ||
    !(handoffDetail instanceof HTMLElement) ||
    !(generatedAt instanceof HTMLTimeElement)
  ) {
    return;
  }
  elements = {
    connection,
    refresh,
    reconnect,
    main,
    unavailable,
    acquired,
    timeSaved,
    timeNote,
    successRate,
    successDetail,
    chart,
    chartPeak,
    chartStart,
    chartEnd,
    chartEmpty,
    accessList,
    handoffRate,
    handoffDetail,
    generatedAt,
  };
  refresh.addEventListener("click", () => {
    void refreshStats();
  });
  reconnect.addEventListener("click", () => {
    void refreshStats();
  });
  document.addEventListener("visibilitychange", refreshOnReturn);
  window.addEventListener("focus", refreshOnReturn);
  render();
  void refreshStats();
}

if (typeof document !== "undefined") {
  if (document.getElementById("stats-main") !== null) bootstrap();
  else document.addEventListener("DOMContentLoaded", bootstrap, { once: true });
}
