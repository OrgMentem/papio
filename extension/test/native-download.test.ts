// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { expect, test } from "bun:test";
import { nativeCompletedFile, nativeDownloadReceipt, migrateNativeDownloadRecovery, type NativeDownloadBinding } from "../src/native-download";
import { migrateManagedState, MANAGED_STATE_VERSION } from "../src/state";
const now = 1_700_000_000_000;
const binding: NativeDownloadBinding = { articleURL: "https://journal.example/article?id=one", incognito: false, cookieStoreId: "firefox-default",
  recovery: { reservation_id: "reserve-123456", browser_epoch: "worker_1", document_id: "doc_1", expires_at_ms: now + 120_000,
    dispatched_at_ms: now, phase: "armed", producer: { effect_kind: "generic_drive", strategy: "generic", drive_attempt_id: "attempt-123456", ordinal: 0, revision: "1" } } };

test("native receipt requires a started HTTP download within the dispatch grace", () => {
  const item = { id: 22, url: "https://journal.example/generated", referrer: binding.articleURL, startTime: new Date(now).toISOString(), incognito: false };
  expect(nativeDownloadReceipt(item, binding, now)?.id).toBe(22);
  expect(nativeDownloadReceipt({ ...item, id: 1 }, binding, now)?.id).toBe(1);
  for (const id of [0, -1, 1.2, Number.MAX_SAFE_INTEGER + 1])
    expect(nativeDownloadReceipt({ ...item, id }, binding, now)).toBeUndefined();
  expect(nativeDownloadReceipt({ ...item, startTime: new Date(now + 45_001).toISOString() }, binding, now + 45_001)).toBeUndefined();
  expect(nativeDownloadReceipt({ ...item, startTime: new Date(now - 1).toISOString() }, binding, now)).toBeUndefined();
  expect(nativeDownloadReceipt({ ...item, incognito: undefined }, binding, now)).toBeUndefined();
  const { dispatched_at_ms: _dispatch, ...unarmed } = binding.recovery;
  expect(nativeDownloadReceipt(item, { ...binding, recovery: unarmed }, now)).toBeUndefined();
});

for (const filename of ["/Users/Ellis/Downloads/paper (2).pdf", "C:\\Users\\Researcher\\Downloads\\paper (2).pdf", "C:/Downloads/paper.pdf"])
  test(`browser absolute path is passed through verbatim: ${filename}`, () => {
    expect(nativeCompletedFile({ id: 4, state: "complete", exists: true, fileSize: 12, filename })).toEqual({ source_path: filename, size_bytes: 12 });
  });
for (const filename of ["paper.pdf", "C:paper.pdf", "\\\\server\\share\\paper.pdf", "", "/Downloads/", "/Downloads/paper\n.pdf", "/" + "é".repeat(2048)])
  test(`invalid browser filename refuses: ${JSON.stringify(filename).slice(0, 70)}`, () => {
    expect(nativeCompletedFile({ id: 4, state: "complete", exists: true, fileSize: 12, filename })).toBeUndefined();
  });

test("source existence, completion and known size are all required", () => {
  const item = { id: 4, state: "complete", exists: true, fileSize: 12, filename: "/Downloads/paper.pdf" };
  for (const patch of [{ state: "in_progress" }, { exists: false }, { exists: undefined }, { fileSize: -1 }, { fileSize: undefined }, { fileSize: 0 }, { fileSize: 1.2 }, { fileSize: Infinity }])
    expect(nativeCompletedFile({ ...item, ...patch })).toBeUndefined();
});

test("managed native recovery projects a closed URL/path-free tuple and keeps v8 state", () => {
  const receipt = { ...binding.recovery, phase: "deferred", download_id: 22, started_at_ms: now,
    articleURL: binding.articleURL, source_path: "C:\\Users\\PRIVATE\\Downloads\\secret.pdf", arbitrary: { token: "PRIVATE" },
    producer: { ...binding.recovery.producer, other: "PRIVATE" } };
  const raw = { version: 8, activeJobs: [{ job_id: "job_123456", tab_id: 7, offered_at: now, expires_at: now + 3600_000,
    status: "accepted", provider_hosts: [], native_download: receipt }] };
  const migrated = migrateManagedState(raw);
  expect(migrated.activeJobs[0]?.native_download).toEqual({ ...binding.recovery, phase: "deferred", download_id: 22, started_at_ms: now });
  expect(JSON.stringify(migrated)).not.toContain("PRIVATE"); expect(JSON.stringify(migrated)).not.toContain("https:");
  expect(MANAGED_STATE_VERSION).toBe(9);
  for (const patch of [{ document_id: "bad/doc" }, { reservation_id: "short" }, { download_id: -1 }, { download_id: 0 }, { started_at_ms: now - 1 },
    { browser_epoch: null }, { phase: "unknown" }, { producer: { ...receipt.producer, strategy: "direct_get" } }]) {
    expect(migrateNativeDownloadRecovery({ ...receipt, ...patch })).toBeUndefined();
  }
});
