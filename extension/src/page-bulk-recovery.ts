// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Durable browser-local recovery for page_bulk_cohort_v2. This module owns
// exactly one chrome.storage.local key; it must never share the managed state
// backend because the cohort manifest has a different restart/privacy boundary.

import { isBareLowercaseHTTPSOrigin, isCanonicalKey, isDetectorText } from "./protocol";

export const PAGE_BULK_COHORT_RECOVERY_KEY = "page_bulk_cohort_recovery_v1" as const;
const STORE_VERSION = 1;
const CHUNK_SIZE = 50;
const MAX_AGE_MS = 24 * 60 * 60 * 1000;
const FUTURE_SKEW_MS = 5 * 60 * 1000;
const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/u;
const ID = /^[\x21-\x7e]{1,64}$/u;
const REQUEST_ID = ID;
const DIGEST = /^[0-9a-f]{64}$/u;

export interface PageBulkRecoverySource {
  kind: "browser_page";
  origin: string;
  detector: string;
}

export interface PageBulkRecoveryUnresolved {
  request_id: string;
  chunk_index: number;
  payload_digest: string;
}

export interface PageBulkRecoveryCohort {
  cohort_id: string;
  scan_id: string;
  source: PageBulkRecoverySource;
  cohort_total: number;
  canonical_keys: string[];
  next_chunk: number;
  unresolved?: PageBulkRecoveryUnresolved;
  updated_at: string;
}

export interface PageBulkCohortRecoveryStoreV1 {
  version: 1;
  cohorts: Record<string, PageBulkRecoveryCohort>;
}

export interface PageBulkRecoveryStorage {
  get(key: string): Promise<Record<string, unknown>>;
  set(value: Record<string, unknown>): Promise<void>;
}

function defaultStorage(): PageBulkRecoveryStorage {
  const chromeValue = (globalThis as { chrome?: { storage?: { local?: PageBulkRecoveryStorage } } }).chrome;
  const local = chromeValue?.storage?.local;
  if (local === undefined || typeof local.get !== "function" || typeof local.set !== "function") {
    // A bridge constructed in a protocol-only test has no browser global. The
    // absence is treated as an empty recovery area; production always has it.
    return {
      async get() { return {}; },
      async set() { /* no browser storage in this host */ },
    };
  }
  return local;
}

function ownKeys(value: Record<string, unknown>, keys: string[]): boolean {
  const allowed = new Set(keys);
  return Object.keys(value).every((key) => allowed.has(key));
}
function isInteger(value: unknown, min: number, max: number): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= min && value <= max;
}
function isTimestamp(value: unknown): value is string {
  return typeof value === "string" && value.length <= 64 && RFC3339.test(value) && Number.isFinite(Date.parse(value));
}
function chunkCount(total: number): number { return Math.ceil(total / CHUNK_SIZE); }
function emptyCohorts(): Record<string, PageBulkRecoveryCohort> {
  return Object.create(null) as Record<string, PageBulkRecoveryCohort>;
}
// The manifest is populated from canonical_key values in a validated daemon
// response. isCanonicalKey only checks bounded NUL-free text; it does not
// establish the no-URL privacy provenance promised by this store.
function validManifest(keys: unknown, total: number): keys is string[] {
  if (!Array.isArray(keys) || keys.length !== total || keys.length < 1 || keys.length > 200) return false;
  const seen = new Set<string>();
  for (const key of keys) {
    if (typeof key !== "string" || !isCanonicalKey(key) || seen.has(key)) return false;
    seen.add(key);
  }
  return true;
}

/** Structural validation is deliberately exported for tests and for callers
 * that validate a replacement before entering the serialized storage lock. */
export function isValidPageBulkRecoveryCohort(value: unknown, now = Date.now()): value is PageBulkRecoveryCohort {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return false;
  const record = value as Record<string, unknown>;
  if (!ownKeys(record, ["cohort_id", "scan_id", "source", "cohort_total", "canonical_keys", "next_chunk", "unresolved", "updated_at"])) return false;
  if (typeof record.cohort_id !== "string" || !ID.test(record.cohort_id)) return false;
  if (typeof record.scan_id !== "string" || !ID.test(record.scan_id)) return false;
  if (!isInteger(record.cohort_total, 1, 200) || !validManifest(record.canonical_keys, record.cohort_total)) return false;
  const totalChunks = chunkCount(record.cohort_total);
  if (!isInteger(record.next_chunk, 0, totalChunks)) return false;
  if (!isTimestamp(record.updated_at)) return false;
  const updatedAt = Date.parse(record.updated_at);
  if (updatedAt > now + FUTURE_SKEW_MS || now - updatedAt > MAX_AGE_MS) return false;
  const source = record.source;
  if (typeof source !== "object" || source === null || Array.isArray(source)) return false;
  const sourceRecord = source as Record<string, unknown>;
  if (!ownKeys(sourceRecord, ["kind", "origin", "detector"]) || sourceRecord.kind !== "browser_page") return false;
  if (typeof sourceRecord.origin !== "string" || sourceRecord.origin.length > 300 || !isBareLowercaseHTTPSOrigin(sourceRecord.origin)) return false;
  if (typeof sourceRecord.detector !== "string" || sourceRecord.detector.length > 128 || !isDetectorText(sourceRecord.detector)) return false;
  if (record.unresolved !== undefined) {
    const unresolved = record.unresolved;
    if (typeof unresolved !== "object" || unresolved === null || Array.isArray(unresolved)) return false;
    const u = unresolved as Record<string, unknown>;
    if (!ownKeys(u, ["request_id", "chunk_index", "payload_digest"])) return false;
    if (typeof u.request_id !== "string" || !REQUEST_ID.test(u.request_id) || !isInteger(u.chunk_index, 0, totalChunks - 1) || typeof u.payload_digest !== "string" || !DIGEST.test(u.payload_digest)) return false;
    if (u.chunk_index !== record.next_chunk) return false;
  }
  return true;
}

async function isValidForWrite(value: unknown, now: number): Promise<boolean> {
  if (!isValidPageBulkRecoveryCohort(value, now)) return false;
  if (value.unresolved === undefined) return true;
  const index = value.unresolved.chunk_index;
  const digest = await pageBulkPayloadDigest({
    scan_id: value.scan_id,
    cohort_id: value.cohort_id,
    source: value.source,
    cohort_total: value.cohort_total,
    chunk_index: index,
    final_chunk: index === chunkCount(value.cohort_total) - 1,
    canonical_keys: chunkKeysFor(value, index),
  });
  return digest === value.unresolved.payload_digest;
}
export function chunkKeysFor(cohort: Pick<PageBulkRecoveryCohort, "canonical_keys">, chunkIndex: number): string[] {
  return cohort.canonical_keys.slice(chunkIndex * CHUNK_SIZE, (chunkIndex + 1) * CHUNK_SIZE);
}

export async function pageBulkPayloadDigest(args: {
  scan_id: string;
  cohort_id: string;
  source: PageBulkRecoverySource;
  cohort_total: number;
  chunk_index: number;
  final_chunk: boolean;
  canonical_keys: string[];
}): Promise<string> {
  const canonical = JSON.stringify([
    args.scan_id, args.cohort_id, args.source.kind, args.source.origin, args.source.detector,
    args.cohort_total, args.chunk_index, args.final_chunk, args.canonical_keys,
  ]);
  const digest = await globalThis.crypto.subtle.digest("SHA-256", new TextEncoder().encode(canonical));
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("");
}

export class PageBulkCohortRecovery {
  private writeChain: Promise<void> = Promise.resolve();
  constructor(
    private readonly storage: PageBulkRecoveryStorage = defaultStorage(),
    private readonly now: () => number = () => Date.now(),
    private readonly log: (message: string) => void = (message) => console.error(message),
  ) {}

  private async readRaw(): Promise<PageBulkCohortRecoveryStoreV1> {
    const raw = await this.storage.get(PAGE_BULK_COHORT_RECOVERY_KEY);
    const candidate = raw[PAGE_BULK_COHORT_RECOVERY_KEY];
    if (typeof candidate !== "object" || candidate === null || Array.isArray(candidate)) return { version: 1, cohorts: emptyCohorts() };
    const root = candidate as Record<string, unknown>;
    if (!ownKeys(root, ["version", "cohorts"]) || root.version !== STORE_VERSION || typeof root.cohorts !== "object" || root.cohorts === null || Array.isArray(root.cohorts)) {
      this.log("papio: discarded invalid page-bulk recovery record");
      return { version: 1, cohorts: emptyCohorts() };
    }
    const cohorts = emptyCohorts();
    for (const [id, value] of Object.entries(root.cohorts as Record<string, unknown>)) {
      if (!isValidPageBulkRecoveryCohort(value, this.now()) || value.cohort_id !== id) {
        this.log("papio: discarded invalid page-bulk recovery record");
        continue;
      }
      const valid = value as PageBulkRecoveryCohort;
      if (valid.unresolved !== undefined) {
        const chunk = chunkKeysFor(valid, valid.unresolved.chunk_index);
        const finalChunk = valid.unresolved.chunk_index === chunkCount(valid.cohort_total) - 1;
        const digest = await pageBulkPayloadDigest({
          scan_id: valid.scan_id,
          cohort_id: valid.cohort_id,
          source: valid.source,
          cohort_total: valid.cohort_total,
          chunk_index: valid.unresolved.chunk_index,
          final_chunk: finalChunk,
          canonical_keys: chunk,
        });
        if (digest !== valid.unresolved.payload_digest) {
          this.log("papio: discarded invalid page-bulk recovery record");
          continue;
        }
      }
      cohorts[id] = valid;
    }
    return { version: 1, cohorts };
  }

  private enqueue<T>(operation: () => Promise<T>): Promise<T> {
    const run = this.writeChain.then(operation, operation);
    this.writeChain = run.then(() => undefined, () => undefined);
    return run;
  }

  async load(): Promise<PageBulkCohortRecoveryStoreV1> { return this.enqueue(() => this.readRaw()); }

  async replace(store: PageBulkCohortRecoveryStoreV1): Promise<void> {
    return this.enqueue(async () => {
      if (store.version !== 1 || typeof store.cohorts !== "object" || store.cohorts === null) {
        throw new Error("invalid page-bulk recovery replacement");
      }
      const cohorts = emptyCohorts();
      for (const [id, cohort] of Object.entries(store.cohorts)) {
        if (cohort.cohort_id !== id || !(await isValidForWrite(cohort, this.now()))) {
          throw new Error("invalid page-bulk recovery replacement");
        }
        cohorts[id] = cohort;
      }
      await this.storage.set({ [PAGE_BULK_COHORT_RECOVERY_KEY]: { version: 1, cohorts } });
    });
  }

  async put(cohort: PageBulkRecoveryCohort): Promise<void> {
    return this.enqueue(async () => {
      if (!(await isValidForWrite(cohort, this.now()))) throw new Error("invalid page-bulk recovery cohort");
      const current = await this.readRaw();
      current.cohorts[cohort.cohort_id] = cohort;
      await this.storage.set({ [PAGE_BULK_COHORT_RECOVERY_KEY]: current });
    });
  }

  async update(cohortID: string, mutate: (cohort: PageBulkRecoveryCohort) => PageBulkRecoveryCohort | undefined): Promise<boolean> {
    return this.enqueue(async () => {
      const current = await this.readRaw();
      const existing = current.cohorts[cohortID];
      if (existing === undefined) return false;
      const next = mutate(existing);
      if (next === undefined) {
        delete current.cohorts[cohortID];
      } else {
        if (!(await isValidForWrite(next, this.now()))) throw new Error("invalid page-bulk recovery cohort");
        current.cohorts[cohortID] = next;
      }
      await this.storage.set({ [PAGE_BULK_COHORT_RECOVERY_KEY]: current });
      return true;
    });
  }
}
export type PageBulkRecoveryStoreV1 = PageBulkCohortRecoveryStoreV1;
export const PageBulkRecoveryStore = PageBulkCohortRecovery;

export const PageBulkCohortRecoveryStore = PageBulkCohortRecovery;
