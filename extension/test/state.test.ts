// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Pure-function tests for the job/tab correlation store — no chrome required.

import { expect, test } from "bun:test";

import {
  clearPendingDelivery,
  emptyStore,
  findByJob,
  findByTab,
  patchJob,
  removeJob,
  startPendingDelivery,
  updatePendingDelivery,
  upsertJob,
  type ActiveJob,
} from "../src/state";
import { federatedLoginClaimKey } from "../src/background";

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


test("pending delivery reducers replace, patch, and clear only the matching job", () => {
  const delivery = { job_id: "job_00000001", url: "https://papers.example/paper.pdf", initiated_at: 7 };
  let store = startPendingDelivery(emptyStore(), delivery);
  expect(store.pendingDelivery).toMatchObject({ ...delivery, status: "sending" });
  store = updatePendingDelivery(store, "other-job", { status: "downloaded" });
  expect(store.pendingDelivery?.status).toBe("sending");
  store = updatePendingDelivery(store, delivery.job_id, { status: "downloaded" });
  expect(store.pendingDelivery?.status).toBe("downloaded");
  store = clearPendingDelivery(store, "other-job");
  expect(store.pendingDelivery).toBeDefined();
  store = clearPendingDelivery(store, delivery.job_id);
  expect(store.pendingDelivery).toBeUndefined();
});
test("waiting-for-session persistence stores only the opaque claim digest", async () => {
  const origin = "https://login.idp.example.edu";
  const entityID = "https://idp.example.edu/entity";
  const digest = await federatedLoginClaimKey(entityID);
  const store = upsertJob(
    {
      ...emptyStore(),
      federatedLoginOwners: { [digest]: { jobID: "job_00000001", tabID: 100, phase: "auth" } },
    },
    job({ waiting_for_session: true, waiting_for_session_key: digest }),
  );
  const persisted = JSON.stringify(store);
  expect(persisted).toContain(digest);
  expect(persisted).not.toContain(origin);
  expect(persisted).not.toContain(entityID);
});

test("institution claim key is versioned and origin-independent", async () => {
  const entityID = "https://idp.example.edu/entity";
  const first = await federatedLoginClaimKey(entityID);
  const second = await federatedLoginClaimKey(entityID);
  expect(first).toBe(second);
  expect(first).toMatch(/^v2:[0-9a-f]{64}$/);
});