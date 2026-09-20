// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import type { NativeDownloadProducer } from "./protocol";
export { NATIVE_CLICK_ADOPTION_FEATURE } from "./protocol";

/** Recovery evidence only: no URL, source path, observation or action survives.
 * A new worker never spends this record as download or dispatch authority. */
export interface NativeDownloadRecovery {
  reservation_id: string;
  producer: NativeDownloadProducer;
  browser_epoch: string;
  document_id: string;
  expires_at_ms: number;
  dispatched_at_ms?: number;
  download_id?: number;
  started_at_ms?: number;
  phase: "armed" | "observed" | "importing" | "deferred" | "retired";
}

export interface NativeDownloadItem {
  id: number;
  url?: string | undefined;
  referrer?: string | undefined;
  startTime?: string | undefined;
  byExtensionId?: string | undefined;
  incognito?: boolean | undefined;
  cookieStoreId?: string | undefined;
  state?: string | undefined;
  exists?: boolean | undefined;
  filename?: string | undefined;
  fileSize?: number | undefined;
}

export interface NativeDownloadReceipt {
  id: number;
  startedAt: number;
  url: string;
  referrer: string;
  incognito: boolean;
  cookieStoreId: string;
}

export interface NativeDownloadBinding {
  articleURL: string;
  incognito: boolean;
  cookieStoreId: string;
  recovery: NativeDownloadRecovery;
}

/** Firefox supplies the ORIGINAL referrer. No host, DOI, tabId, finalUrl or
 * filename approximation is a substitute for this exact article binding. */
export function nativeDownloadReceipt(
  item: NativeDownloadItem, binding: NativeDownloadBinding, now: number,
): NativeDownloadReceipt | undefined {
  const dispatched = binding.recovery.dispatched_at_ms;
  const started = Date.parse(item.startTime ?? "");
  if (dispatched === undefined || !Number.isSafeInteger(item.id) || item.id <= 0 ||
    !Number.isSafeInteger(started) || started < dispatched || started > dispatched + 45_000 || started > now ||
    now >= binding.recovery.expires_at_ms || started >= binding.recovery.expires_at_ms ||
    item.referrer !== binding.articleURL || item.byExtensionId ||
    item.incognito !== binding.incognito ||
    (item.cookieStoreId ?? "firefox-default") !== binding.cookieStoreId) return undefined;
  try {
    const url = new URL(item.url ?? "");
    // Blob/data downloads have no equivalent provenance in this contract.
    if (!/^https?:$/.test(url.protocol) || url.username || url.password) return undefined;
  } catch { return undefined; }
  return { id: item.id, startedAt: started, url: item.url!, referrer: item.referrer,
    incognito: item.incognito, cookieStoreId: item.cookieStoreId ?? "firefox-default" };
}

export function sameNativeReceipt(a: NativeDownloadReceipt, b: NativeDownloadReceipt): boolean {
  return a.id === b.id && a.startedAt === b.startedAt && a.url === b.url &&
    a.referrer === b.referrer && a.incognito === b.incognito && a.cookieStoreId === b.cookieStoreId;
}

export function nativeCompletedFile(item: NativeDownloadItem): { source_path: string; size_bytes: number } | undefined {
  const path = item.filename;
  if (item.state !== "complete" || item.exists !== true ||
    !Number.isSafeInteger(item.fileSize) || item.fileSize! <= 0 || typeof path !== "string" ||
    new TextEncoder().encode(path).byteLength > 4096 || /[\u0000-\u001f\u007f]/.test(path) ||
    !/^(?:\/[^/]|[A-Za-z]:[\\/])/.test(path) || /[\\/]$/.test(path)) return undefined;
  return { source_path: path, size_bytes: item.fileSize! };
}

/** Read-only isolated-world injection: use the agent's DOM token, never the
 * Chromium-only webNavigation.documentId, and do not invalidate observations. */
export function nativeDownloadDocumentCurrent(documentID: string, articleURL: string): boolean {
  const state = (globalThis as typeof globalThis & {
    papioArticleAgent?: { node: Document; document: string };
  }).papioArticleAgent;
  return state?.node === document && state.document === documentID && location.href === articleURL;
}

/** Closed storage projection; reject malformed records rather than retaining
 * a source path hidden among unknown fields. */
export function migrateNativeDownloadRecovery(raw: unknown): NativeDownloadRecovery | undefined {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return undefined;
  const r = raw as Record<string, unknown>;
  const opaque = (v: unknown, min = 1, max = 128): v is string =>
    typeof v === "string" && v.length >= min && v.length <= max && /^[A-Za-z0-9_-]+$/.test(v);
  const integer = (v: unknown): v is number => typeof v === "number" && Number.isSafeInteger(v) && v >= 0;
  if (!opaque(r.reservation_id, 8, 64) || !opaque(r.browser_epoch) || !opaque(r.document_id) ||
    !integer(r.expires_at_ms) || !r.producer || typeof r.producer !== "object") return undefined;
  const p = r.producer as Record<string, unknown>;
  if (p.effect_kind !== "generic_drive" || p.strategy !== "generic" ||
    !opaque(p.drive_attempt_id, 8, 128) || !integer(p.ordinal) || !opaque(p.revision, 1, 128) ||
    !["armed", "observed", "importing", "deferred", "retired"].includes(String(r.phase))) return undefined;
  const result: NativeDownloadRecovery = { reservation_id: r.reservation_id, browser_epoch: r.browser_epoch,
    document_id: r.document_id, expires_at_ms: r.expires_at_ms, phase: r.phase as NativeDownloadRecovery["phase"],
    producer: { effect_kind: "generic_drive", strategy: "generic", drive_attempt_id: p.drive_attempt_id,
      ordinal: p.ordinal, revision: p.revision } };
  if (r.dispatched_at_ms !== undefined) {
    if (!integer(r.dispatched_at_ms) || r.dispatched_at_ms >= r.expires_at_ms) return undefined;
    result.dispatched_at_ms = r.dispatched_at_ms;
  }
  if (r.download_id !== undefined || r.started_at_ms !== undefined) {
    if (!integer(r.download_id) || r.download_id <= 0 || !integer(r.started_at_ms) || result.dispatched_at_ms === undefined ||
      r.started_at_ms < result.dispatched_at_ms || r.started_at_ms >= r.expires_at_ms) return undefined;
    result.download_id = r.download_id;
    result.started_at_ms = r.started_at_ms;
  }
  return result;
}
