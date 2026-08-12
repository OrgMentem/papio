import { expect, test } from "bun:test";
import {
  PAGE_BULK_COHORT_RECOVERY_KEY,
  PageBulkCohortRecovery,
  pageBulkPayloadDigest,
  type PageBulkRecoveryCohort,
  type PageBulkRecoveryStorage,
} from "../src/page-bulk-recovery";

const NOW = Date.parse("2026-08-12T12:00:00.000Z");
const source = { kind: "browser_page" as const, origin: "https://results.example.edu", detector: "generic-identifiers/1" };

function memoryStorage(initial: Record<string, unknown> = {}): PageBulkRecoveryStorage & { values: Record<string, unknown>; writes: Record<string, unknown>[] } {
  const values = { ...initial };
  const writes: Record<string, unknown>[] = [];
  return {
    values,
    writes,
    async get(key) { return key in values ? { [key]: values[key] } : {}; },
    async set(value) {
      writes.push(value);
      Object.assign(values, value);
    },
  };
}

async function cohort(total = 1, unresolved = true, cohortID = "cohort_1"): Promise<PageBulkRecoveryCohort> {
  const keys = Array.from({ length: total }, (_, index) => `work:${index}`);
  const record: PageBulkRecoveryCohort = {
    cohort_id: cohortID,
    scan_id: "scan_1",
    source,
    cohort_total: total,
    canonical_keys: keys,
    next_chunk: 0,
    updated_at: new Date(NOW).toISOString(),
  };
  if (unresolved) {
    const canonicalKeys = keys.slice(0, 50);
    record.unresolved = {
      request_id: "request_1",
      chunk_index: 0,
      payload_digest: await pageBulkPayloadDigest({
        scan_id: record.scan_id,
        cohort_id: record.cohort_id,
        source,
        cohort_total: total,
        chunk_index: 0,
        final_chunk: total <= 50,
        canonical_keys: canonicalKeys,
      }),
    };
  }
  return record;
}

test("persists only its dedicated key and survives a fresh store instance", async () => {
  const storage = memoryStorage();
  const first = new PageBulkCohortRecovery(storage, () => NOW);
  await first.put(await cohort());
  expect(storage.writes).toHaveLength(1);
  expect(Object.keys(storage.writes[0]!)).toEqual([PAGE_BULK_COHORT_RECOVERY_KEY]);
  expect(storage.values["papio_state_v1"]).toBeUndefined();

  const restarted = new PageBulkCohortRecovery(storage, () => NOW);
  const loaded = await restarted.load();
  expect(loaded.version).toBe(1);
  expect(loaded.cohorts["cohort_1"]?.unresolved?.request_id).toBe("request_1");
});

test("serializes concurrent read-modify-write operations without losing cohorts", async () => {
  const storage = memoryStorage();
  const store = new PageBulkCohortRecovery(storage, () => NOW);
  const second = { ...(await cohort()), cohort_id: "cohort_2", scan_id: "scan_2" };
  second.unresolved = {
    ...second.unresolved!,
    payload_digest: await pageBulkPayloadDigest({
      scan_id: second.scan_id,
      cohort_id: second.cohort_id,
      source,
      cohort_total: second.cohort_total,
      chunk_index: 0,
      final_chunk: true,
      canonical_keys: second.canonical_keys,
    }),
  };
  await Promise.all([store.put(await cohort()), store.put(second)]);
  expect(Object.keys((await store.load()).cohorts).sort()).toEqual(["cohort_1", "cohort_2"]);
});

test("discards malformed, expired, future-dated, and digest-mismatched records with bounded diagnostics", async () => {
  const valid = await cohort();
  const cases: unknown[] = [
    { version: 1, cohorts: { cohort_1: { ...valid, source: { ...source, origin: "https://bad.example.edu/path" } } } },
    { version: 1, cohorts: { cohort_1: { ...valid, updated_at: new Date(NOW - 25 * 60 * 60 * 1000).toISOString() } } },
    { version: 1, cohorts: { cohort_1: { ...valid, updated_at: new Date(NOW + 6 * 60 * 1000).toISOString() } } },
    { version: 1, cohorts: { cohort_1: { ...valid, unresolved: { ...valid.unresolved!, payload_digest: "0".repeat(64) } } } },
  ];
  for (const value of cases) {
    const storage = memoryStorage({ [PAGE_BULK_COHORT_RECOVERY_KEY]: value });
    const logs: string[] = [];
    const loaded = await new PageBulkCohortRecovery(storage, () => NOW, (message) => logs.push(message)).load();
    expect(Object.keys(loaded.cohorts)).toEqual([]);
    expect(logs).toHaveLength(1);
    expect(logs[0]).toBe("papio: discarded invalid page-bulk recovery record");
    expect(logs[0]).not.toContain("cohort_1");
    expect(logs[0]).not.toContain("results.example.edu");
  }
});

test("preserves a __proto__ cohort id as an own persisted property", async () => {
  const storage = memoryStorage();
  const store = new PageBulkCohortRecovery(storage, () => NOW);
  await store.put(await cohort(1, true, "__proto__"));

  const loaded = await new PageBulkCohortRecovery(storage, () => NOW).load();
  expect(Object.prototype.hasOwnProperty.call(loaded.cohorts, "__proto__")).toBe(true);
  expect(loaded.cohorts["__proto__"]?.cohort_id).toBe("__proto__");
  expect(Object.entries(loaded.cohorts).map(([id]) => id)).toEqual(["__proto__"]);
});