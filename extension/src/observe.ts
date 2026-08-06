// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Automatic, rate-limited capture of unknown provider pages. This is strictly
// development material: it is only reachable from a broker-owned handoff tab
// whose host passed the same offer-or-registry gate as classification.

import {
  capturePage,
  encodePageCapture,
  residualLeak,
  sanitizeFixture,
  type PageCapture,
} from "./capture";
import type { PageCapturePayload } from "./protocol";
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
const MAX_PER_SHAPE = 1;
const MAX_PER_DAY = 20;
// Bound retained digests per shape so a host that never stabilizes cannot
// grow storage without limit; 3 is enough to catch A/B page variants without
// needing to remember every sanitized snapshot ever seen for a shape.
const MAX_DIGESTS_PER_SHAPE = 3;

export interface ObserveChromeApi {
  scripting: {
    executeScript(injection: {
      target: { tabId: number };
      func: () => PageCapture;
    }): Promise<Array<{ result?: PageCapture | undefined }>>;
  };
  storage: {
    local: {
      get(key: string): Promise<Record<string, unknown>>;
      set(items: Record<string, unknown>): Promise<void>;
    };
  };
  sendPageCapture(payload: PageCapturePayload, jobID: string): boolean | Promise<boolean>;
}

/** Sources that already authorized classification of this broker-owned tab. */
export interface ObservationCaptureContext {
  verifiedHosts: readonly string[];
  adapterID?: string;
  adapterVersion?: string;
}

interface ObservationRateState {
  total: number[];
  byShape: Record<string, number[]>;
  digests: Record<string, string[]>;
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

/** The bucket a capture is rate-limited and deduped against: the same host
 * repeatedly matching-but-unclassified through the same adapter build is one
 * shape, but bumping adapterVersion on a still-broken host is a new attempt —
 * exactly when the diagnostic is worth having again. */
function observationShapeKey(hostKey: string, context: ObservationCaptureContext): string {
  return `${hostKey}|${context.adapterID ?? "-"}@${context.adapterVersion ?? "-"}`;
}

/** Dependency-free 32-bit FNV-1a hash, rendered as 8 hex digits. This is a
 * dedupe key for spotting a repeat sanitized page shape, not a security
 * primitive: a collision just costs one wasted capture slot, not a broken
 * guarantee. The extension has zero runtime deps, so no crypto import. */
function fnv1a(input: string): string {
  let hash = 0x811c9dc5;
  for (let index = 0; index < input.length; index += 1) {
    hash ^= input.charCodeAt(index);
    hash = Math.imul(hash, 0x01000193);
  }
  return (hash >>> 0).toString(16).padStart(8, "0");
}

function cleanRateState(raw: unknown, now: number): ObservationRateState {
  const empty: ObservationRateState = { total: [], byShape: {}, digests: {} };
  if (raw === null || typeof raw !== "object") return empty;
  const candidate = raw as Record<string, unknown>;
  // A pre-rename snapshot keyed captures on bare host ("byHost"). Reading its
  // entries as shapeKeys would silently let one failing host squat on every
  // adapter version's budget forever, so a legacy snapshot degrades to empty
  // rather than being reinterpreted.
  if ("byHost" in candidate && !("byShape" in candidate)) return empty;
  const sinceDay = now - DAY_MS;
  const timestamps = (value: unknown, since: number): number[] =>
    Array.isArray(value)
      ? value.filter((timestamp): timestamp is number => typeof timestamp === "number" && Number.isFinite(timestamp) && timestamp > since)
      : [];
  // Retained for the full day, not just the hourly gate window: the 1-per-
  // hour cap is enforced by sub-filtering this array at read time (see
  // observeUnknown), but the digest cache below needs a shape's entry to
  // survive across many hourly reset cycles within the day — otherwise a
  // host that fails the exact same way every hour would burn a fresh daily-
  // budget slot each time, defeating the point of the dedupe.
  const byShape: Record<string, number[]> = {};
  if (candidate["byShape"] !== null && typeof candidate["byShape"] === "object") {
    for (const [shapeKey, values] of Object.entries(candidate["byShape"] as Record<string, unknown>)) {
      const retained = timestamps(values, sinceDay);
      if (retained.length > 0) byShape[shapeKey] = retained;
    }
  }
  // A shape's digests only matter as long as the shape itself is still
  // within the daily budget's window; once its last timestamp ages out of
  // `byShape` above, drop its digests too instead of letting them accumulate
  // for a shape that hasn't been seen in a day.
  const digests: Record<string, string[]> = {};
  if (candidate["digests"] !== null && typeof candidate["digests"] === "object") {
    for (const [shapeKey, values] of Object.entries(candidate["digests"] as Record<string, unknown>)) {
      if (!(shapeKey in byShape) || !Array.isArray(values)) continue;
      const retained = values.filter((digest): digest is string => typeof digest === "string").slice(0, MAX_DIGESTS_PER_SHAPE);
      if (retained.length > 0) digests[shapeKey] = retained;
    }
  }
  return { total: timestamps(candidate["total"], sinceDay), byShape, digests };
}


let observationQueue: Promise<void> = Promise.resolve();

/**
 * Capture one unknown result from a tracked handoff tab. Calls are serialized
 * in the service worker so a burst of tab events cannot pass the persisted
 * quota concurrently. Every malformed snapshot, storage failure, changed
 * origin, oversized frame, or residual leak fails closed without emission.
 */
export function observeUnknown(
  api: ObserveChromeApi,
  job: ActiveJob | undefined,
  host: string,
  context: ObservationCaptureContext,
  now: () => Date = () => new Date(),
): Promise<boolean> {
  let captured = false;
  const run = observationQueue.then(async () => {
    if (!job || !hostMatches(host, context.verifiedHosts)) return;
    const hostKey = observedHostKey(host);
    if (!hostKey) return;
    const shapeKey = observationShapeKey(hostKey, context);

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
    // `rates.byShape` spans the full day (see cleanRateState); the 1-per-
    // hour gate is a trailing-hour sub-filter over that persisted history.
    const shapeCapturesThisHour = (rates.byShape[shapeKey] ?? []).filter((t) => t > timestamp - HOUR_MS).length;
    if (rates.total.length >= MAX_PER_DAY || shapeCapturesThisHour >= MAX_PER_SHAPE) return;

    let injected: { result?: PageCapture | undefined } | undefined;
    try {
      [injected] = await api.scripting.executeScript({ target: { tabId: job.tab_id }, func: capturePage });
    } catch (error) {
      console.warn("papio: observed page capture failed; skipping", error);
      return;
    }
    const page = injected?.result;
    if (!page || typeof page.html !== "string" || typeof page.origin !== "string" || typeof page.path !== "string") return;

    let pageHost: string;
    try {
      pageHost = new URL(page.origin).hostname;
    } catch {
      return;
    }
    if (!hostMatches(pageHost, context.verifiedHosts) || pageHost.toLowerCase() !== host.toLowerCase()) return;

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

    // The digest only exists once the page is captured and sanitized, so a
    // repeat of an already-seen shape still costs the executeScript above —
    // what it must not cost is a wasted daily-budget slot or a redundant
    // upload over the native-messaging bridge. Hash the body only: the
    // fixture header's `captured=` timestamp is different on every call by
    // construction, so including it would make every capture look unique
    // and the dedupe would never fire.
    const digest = fnv1a(sanitized.slice(sanitized.indexOf("\n") + 1));
    if (rates.digests[shapeKey]?.includes(digest)) return;

    const encoded = await encodePageCapture(sanitized, {
      host: pageHost,
      scenario: "observed",
      ...(context.adapterID === undefined ? {} : { adapterID: context.adapterID }),
      ...(context.adapterVersion === undefined ? {} : { adapterVersion: context.adapterVersion }),
      jobID: job.job_id,
    });
    if (!encoded.ok) {
      console.warn("papio: observed page capture could not be encoded; skipping", encoded.error);
      return;
    }

    // Reserve quota before bridge emission so a worker restart during native
    // transport cannot turn one observation into multiple captures.
    rates.total.push(timestamp);
    const shapeRates = rates.byShape[shapeKey] ?? [];
    shapeRates.push(timestamp);
    rates.byShape[shapeKey] = shapeRates;
    const shapeDigests = rates.digests[shapeKey] ?? [];
    rates.digests[shapeKey] = [digest, ...shapeDigests].slice(0, MAX_DIGESTS_PER_SHAPE);
    try {
      await api.storage.local.set({ [RATE_STORAGE_KEY]: rates });
    } catch (error) {
      console.warn("papio: observed capture rate storage unavailable; skipping", error);
      return;
    }

    try {
      if (await api.sendPageCapture(encoded.payload, job.job_id)) {
        captured = true;
      } else {
        console.warn("papio: observed page capture was not sent; skipping");
      }
    } catch (error) {
      console.warn("papio: observed page capture was not sent; skipping", error);
    }
  });
  observationQueue = run.catch(() => undefined);
  return run.then(() => captured);
}
