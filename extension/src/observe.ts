// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Automatic, rate-limited capture of unknown provider pages. This is strictly
// development material: it is only reachable from a broker-owned handoff tab
// after its current host has been verified against the original job offer.

import {
  capturePage,
  downloadFixture,
  MAX_CAPTURE_BYTES,
  residualLeak,
  sanitizeFixture,
  type PageCapture,
} from "./capture";
import type { ActiveJob } from "./state";

const RATE_STORAGE_KEY = "papio_observed_capture_rate_v1";
const HOUR_MS = 60 * 60 * 1000;

/** Common multi-label public suffixes used by institutional publishers. This
 * keeps `journal.example.co.uk` keyed as `example-co-uk`, while ordinary hosts
 * use their final two labels. */
const MULTI_LABEL_PUBLIC_SUFFIXES: Record<string, true> = {
  "ac.uk": true,
  "co.uk": true,
  "com.au": true,
  "com.br": true,
  "com.cn": true,
  "com.mx": true,
  "co.jp": true,
  "co.nz": true,
  "edu.au": true,
  "gov.au": true,
  "gov.uk": true,
  "govt.nz": true,
  "net.au": true,
  "ne.jp": true,
  "org.au": true,
  "org.nz": true,
  "or.jp": true,
};
const DAY_MS = 24 * HOUR_MS;
const MAX_PER_HOST = 1;
const MAX_PER_DAY = 5;

export interface ObserveChromeApi {
  scripting: {
    executeScript(injection: {
      target: { tabId: number };
      func: () => PageCapture;
    }): Promise<Array<{ result?: PageCapture | undefined }>>;
  };
  downloads: {
    download(options: {
      url: string;
      filename: string;
      conflictAction: "uniquify";
      saveAs: boolean;
    }): Promise<number>;
  };
  storage: {
    local: {
      get(key: string): Promise<Record<string, unknown>>;
      set(items: Record<string, unknown>): Promise<void>;
    };
  };
}

interface ObservationRateState {
  total: number[];
  byHost: Record<string, number[]>;
}

function hostMatches(host: string, providerHosts: readonly string[]): boolean {
  const normalized = host.toLowerCase();
  return providerHosts.some((providerHost) => {
    const expected = providerHost.toLowerCase();
    return normalized === expected || normalized.endsWith(`.${expected}`);
  });
}

/** Convert the verified provider host to a stable registrable-host key. Non-
 * domain values are rejected rather than guessed. */
export function observedHostKey(host: string): string | null {
  const labels = host.toLowerCase().split(".").filter(Boolean);
  if (labels.length < 2 || labels.some((label) => !/^[a-z0-9-]+$/.test(label))) return null;
  const suffix = labels.slice(-2).join(".");
  return labels.slice(MULTI_LABEL_PUBLIC_SUFFIXES[suffix] === true ? -3 : -2).join("-");
}

function cleanRateState(raw: unknown, now: number): ObservationRateState {
  const empty: ObservationRateState = { total: [], byHost: {} };
  if (raw === null || typeof raw !== "object") return empty;
  const candidate = raw as Record<string, unknown>;
  const sinceDay = now - DAY_MS;
  const timestamps = (value: unknown, since: number): number[] =>
    Array.isArray(value)
      ? value.filter((timestamp): timestamp is number => typeof timestamp === "number" && Number.isFinite(timestamp) && timestamp > since)
      : [];
  const byHost: Record<string, number[]> = {};
  if (candidate["byHost"] !== null && typeof candidate["byHost"] === "object") {
    for (const [host, values] of Object.entries(candidate["byHost"] as Record<string, unknown>)) {
      const retained = timestamps(values, now - HOUR_MS);
      if (retained.length > 0) byHost[host] = retained;
    }
  }
  return { total: timestamps(candidate["total"], sinceDay), byHost };
}


let observationQueue: Promise<void> = Promise.resolve();

/**
 * Capture one unknown result from a tracked handoff tab. Calls are serialized
 * in the service worker so a burst of tab events cannot pass the persisted
 * quota concurrently. Every malformed snapshot, storage failure, changed
 * origin, oversize document, or residual leak fails closed without download.
 */
export function observeUnknown(
  api: ObserveChromeApi,
  job: ActiveJob | undefined,
  host: string,
  now: () => Date = () => new Date(),
): Promise<void> {
  const run = observationQueue.then(async () => {
    if (!job || !hostMatches(host, job.provider_hosts)) return;
    const hostKey = observedHostKey(host);
    if (!hostKey) return;

    const capturedAt = now();
    const timestamp = capturedAt.getTime();
    if (!Number.isFinite(timestamp)) return;

    let stored: Record<string, unknown>;
    try {
      stored = await api.storage.local.get(RATE_STORAGE_KEY);
    } catch (error) {
      console.warn("papio: observed capture rate storage unavailable; skipping", error);
      return;
    }
    const rates = cleanRateState(stored[RATE_STORAGE_KEY], timestamp);
    if (rates.total.length >= MAX_PER_DAY || (rates.byHost[hostKey]?.length ?? 0) >= MAX_PER_HOST) return;

    let injected: { result?: PageCapture | undefined } | undefined;
    try {
      [injected] = await api.scripting.executeScript({ target: { tabId: job.tab_id }, func: capturePage });
    } catch (error) {
      console.warn("papio: observed page capture failed; skipping", error);
      return;
    }
    const page = injected?.result;
    if (!page || typeof page.html !== "string" || typeof page.origin !== "string" || typeof page.path !== "string") return;
    if (new TextEncoder().encode(page.html).length > MAX_CAPTURE_BYTES) return;

    let pageHost: string;
    try {
      pageHost = new URL(page.origin).hostname;
    } catch {
      return;
    }
    if (!hostMatches(pageHost, job.provider_hosts) || pageHost.toLowerCase() !== host.toLowerCase()) return;

    const sanitized = sanitizeFixture(page.html, {
      provider: hostKey,
      scenario: "observed",
      originNoQuery: `${page.origin}${page.path}`,
      capturedISO: capturedAt.toISOString(),
    });
    const leak = residualLeak(sanitized);
    if (leak) {
      console.warn(`papio: refusing observed capture with residual secret: ${leak}`);
      return;
    }

    // Reserve the quota before downloading. A service-worker restart between a
    // download starting and an afterward write must not permit a sixth capture.
    rates.total.push(timestamp);
    const hostRates = rates.byHost[hostKey] ?? [];
    hostRates.push(timestamp);
    rates.byHost[hostKey] = hostRates;
    try {
      await api.storage.local.set({ [RATE_STORAGE_KEY]: rates });
    } catch (error) {
      console.warn("papio: observed capture rate storage unavailable; skipping", error);
      return;
    }

    const filename = `papio-fixtures/observed/${hostKey}/${capturedAt.toISOString().replace(/:/g, "-")}.html`;
    try {
      await downloadFixture(api, filename, sanitized);
    } catch (error) {
      console.warn("papio: observed fixture download failed; skipping", error);
    }
  });
  observationQueue = run.catch(() => undefined);
  return run;
}
