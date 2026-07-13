// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Pure-function tests for the job/tab correlation store — no chrome required.

import { expect, test } from "bun:test";

import {
  emptyStore,
  findByJob,
  findByTab,
  patchJob,
  removeJob,
  upsertJob,
  type ActiveJob,
} from "../src/state";

function job(overrides: Partial<ActiveJob> = {}): ActiveJob {
  return {
    job_id: "job_00000001",
    tab_id: 100,
    offered_at: 1,
    expires_at: 2,
    status: "accepted",
    provider_hosts: ["www.jstor.org"],
    ...overrides,
  };
}

test("upsert inserts then replaces by job_id, never duplicating", () => {
  let store = emptyStore();
  store = upsertJob(store, job());
  store = upsertJob(store, job({ status: "auth_pending" }));
  expect(store.activeJobs.length).toBe(1);
  expect(findByJob(store, "job_00000001")?.status).toBe("auth_pending");
});

test("find by tab and by job resolve the same record", () => {
  const store = upsertJob(emptyStore(), job());
  expect(findByTab(store, 100)?.job_id).toBe("job_00000001");
  expect(findByTab(store, 999)).toBeUndefined();
});

test("patchJob returns a new store and only touches the named job", () => {
  let store = upsertJob(emptyStore(), job());
  store = upsertJob(store, job({ job_id: "job_00000002", tab_id: 200 }));
  store = patchJob(store, "job_00000002", { status: "awaiting_download" });
  expect(findByJob(store, "job_00000001")?.status).toBe("accepted");
  expect(findByJob(store, "job_00000002")?.status).toBe("awaiting_download");
});

test("removeJob drops exactly one record", () => {
  let store = upsertJob(emptyStore(), job());
  store = removeJob(store, "job_00000001");
  expect(store.activeJobs.length).toBe(0);
});
