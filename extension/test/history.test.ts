// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { Window } from "happy-dom";

interface RuntimeRequest {
  type: string;
  request: Record<string, unknown>;
}

let importSerial = 0;

async function settle(): Promise<void> {
  for (let iteration = 0; iteration < 12; iteration += 1) await Promise.resolve();
}

async function historyDocument(
  reply: (message: RuntimeRequest) => unknown | Promise<unknown>,
): Promise<{ document: Document; requests: RuntimeRequest[] }> {
  const window = new Window();
  window.document.write(readFileSync(new URL("../src/history.html", import.meta.url), "utf8"));
  const requests: RuntimeRequest[] = [];
  Object.assign(globalThis, {
    window,
    document: window.document,
    Event: window.Event,
    HTMLElement: window.HTMLElement,
    HTMLButtonElement: window.HTMLButtonElement,
    HTMLTimeElement: window.HTMLTimeElement,
    chrome: {
      runtime: {
        sendMessage: async (message: RuntimeRequest) => {
          requests.push(message);
          return reply(message);
        },
      },
    },
  });
  importSerial += 1;
  // Each fixture needs a fresh page module because its UI state is intentionally module-local.
  await import(`../src/history.ts?history-test=${importSerial}`);
  await settle();
  return {
    document: window.document as unknown as Document,
    requests,
  };
}

/** Twelve ISO-week buckets, oldest first: Mondays 2026-05-04 … 2026-07-20. */
function weeklySeries(values: number[]): Array<{ period_start: string; acquired: number }> {
  const firstMonday = Date.UTC(2026, 4, 4);
  return values.map((acquired, index) => ({
    period_start: new Date(firstMonday + index * 7 * 24 * 60 * 60 * 1000).toISOString(),
    acquired,
  }));
}

function stats(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    generated_at: "2026-07-25T08:00:00Z",
    acquired_total: 42,
    failed_total: 14,
    handoffs_required: 9,
    access: { open_access: 18, institutional: 20, licensed_api: 3, other: 1 },
    series: weeklySeries([0, 1, 3, 0, 5, 4, 2, 6, 8, 4, 3, 6]),
    ...overrides,
  };
}

test("renders every impact metric from a daemon stats reply", async () => {
  const { document, requests } = await historyDocument(() => ({ ok: true, stats: stats() }));

  expect(requests.map((request) => request.type)).toEqual(["papio.stats"]);
  expect(requests[0]?.request).toEqual({});
  expect(document.getElementById("connection-status")?.getAttribute("data-state")).toBe("connected");
  expect(document.getElementById("stats-main")?.hidden).toBe(false);
  expect(document.getElementById("stats-unavailable")?.hidden).toBe(true);
  expect(document.getElementById("reconnect-daemon")?.hidden).toBe(true);

  // 42 acquired × 20 min ≈ 14 h; 42 of 56 finished jobs succeeded.
  expect(document.getElementById("stat-acquired")?.textContent).toBe("42");
  expect(document.getElementById("stat-time-saved")?.textContent).toBe("14 h");
  expect(document.getElementById("stat-time-note")?.textContent).toContain("20 minutes");
  expect(document.getElementById("stat-success-rate")?.textContent).toBe("75%");
  expect(document.getElementById("stat-success-detail")?.textContent).toBe("42 acquired · 14 failed");

  const bars = Array.from(document.querySelectorAll("#weekly-chart .chart-bar"));
  expect(bars).toHaveLength(12);
  expect((bars[8] as HTMLElement).style.height).toBe("100%");
  expect(document.getElementById("chart-peak")?.hidden).toBe(false);
  expect(document.getElementById("chart-peak")?.textContent).toBe("peak 8 / week");
  expect(document.getElementById("chart-empty")?.hidden).toBe(true);
  expect(document.getElementById("chart-start")?.textContent).toBe("May 4");
  expect(document.getElementById("chart-end")?.textContent).toBe("Jul 20");

  const labels = Array.from(document.querySelectorAll("#access-list .access-label")).map((el) => el.textContent);
  expect(labels).toEqual(["Open access", "Institutional access", "Licensed API", "Other routes"]);
  const counts = Array.from(document.querySelectorAll("#access-list .access-count")).map((el) => el.textContent);
  expect(counts).toEqual(["18 · 43%", "20 · 48%", "3 · 7%", "1 · 2%"]);

  // 9 of 42 acquired papers needed the user.
  expect(document.getElementById("stat-handoff-rate")?.textContent).toBe("21%");
  expect(document.getElementById("stat-handoff-detail")?.textContent).toContain("9 of 42 acquired papers");
  expect(document.getElementById("generated-at")?.textContent).toBe("generated at 2026-07-25T08:00:00Z");

  // The Refresh control issues a fresh stats request.
  document.getElementById("refresh-history")?.click();
  await settle();
  expect(requests).toHaveLength(2);
  expect(requests[1]?.type).toBe("papio.stats");
});

test("renders the muted unavailable state when the daemon cannot serve stats", async () => {
  const { document } = await historyDocument(() => ({
    ok: false,
    error: { code: "feature_unavailable", message: "This daemon does not support the requested inbox feature" },
  }));

  expect(document.getElementById("stats-main")?.hidden).toBe(true);
  const unavailable = document.getElementById("stats-unavailable");
  expect(unavailable?.hidden).toBe(false);
  expect(unavailable?.textContent).toContain("update papio");
  expect(document.getElementById("connection-status")?.getAttribute("data-state")).toBe("disconnected");
  expect(document.getElementById("connection-status")?.textContent).toContain(
    "This daemon does not support the requested inbox feature",
  );
  expect(document.getElementById("reconnect-daemon")?.hidden).toBe(false);

  // A dead transport (no broker at all) lands in the same muted state.
  const offline = await historyDocument(() => {
    throw new Error("Could not establish connection.");
  });
  expect(offline.document.getElementById("stats-main")?.hidden).toBe(true);
  expect(offline.document.getElementById("stats-unavailable")?.hidden).toBe(false);
  expect(offline.document.getElementById("stats-unavailable")?.textContent).toContain("connect the papio daemon");
  expect(offline.document.getElementById("connection-status")?.textContent).toContain("Could not establish connection.");
});

test("keeps the weekly chart stable on an all-zero series", async () => {
  const { document } = await historyDocument(() => ({
    ok: true,
    stats: stats({
      acquired_total: 0,
      failed_total: 0,
      handoffs_required: 0,
      access: { open_access: 0, institutional: 0, licensed_api: 0, other: 0 },
      series: weeklySeries(new Array<number>(12).fill(0)),
    }),
  }));

  const bars = Array.from(document.querySelectorAll("#weekly-chart .chart-bar"));
  expect(bars).toHaveLength(12);
  for (const bar of bars) expect((bar as HTMLElement).style.height).toBe("0%");
  expect(document.getElementById("chart-empty")?.hidden).toBe(false);
  expect(document.getElementById("chart-peak")?.hidden).toBe(true);
  expect(document.getElementById("chart-start")?.textContent).toBe("May 4");
  expect(document.getElementById("chart-end")?.textContent).toBe("Jul 20");

  // Zero totals degrade to em dashes, never NaN.
  expect(document.getElementById("stat-acquired")?.textContent).toBe("0");
  expect(document.getElementById("stat-time-saved")?.textContent).toBe("0 h");
  expect(document.getElementById("stat-success-rate")?.textContent).toBe("—");
  expect(document.getElementById("stat-success-detail")?.textContent).toBe("No finished acquisitions yet.");
  expect(document.getElementById("stat-handoff-rate")?.textContent).toBe("—");
  expect(document.getElementById("stat-handoff-detail")?.textContent).toBe("No acquired papers yet.");
  const counts = Array.from(document.querySelectorAll("#access-list .access-count")).map((el) => el.textContent);
  expect(counts).toEqual(["0 · —", "0 · —", "0 · —", "0 · —"]);
});

// This page has no poll and no change subscription: it refetches on open and
// whenever the tab becomes visible again, which is refresh-on-return below.
test("refetches stats when the tab becomes visible again", async () => {
  const page = await historyDocument(() => ({ ok: true, stats: stats() }));
  const before = page.requests.filter((request) => request.type === "papio.stats").length;
  page.document.dispatchEvent(new Event("visibilitychange", { bubbles: true }));
  await settle();
  expect(page.requests.filter((request) => request.type === "papio.stats")).toHaveLength(before + 1);
});
