// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Pure-function tests for the job/tab correlation store — no chrome required.

import { expect, test } from "bun:test";

import {
  MANAGED_STATE_VERSION,
  chromeBackend,
  clearPendingDelivery,
  emptyStore,
  findByJob,
  findByTab,
  migrateManagedState,
  patchJob,
  removeJob,
  startPendingDelivery,
  updatePendingDelivery,
  upsertJob,
  type ActiveJob,
  type StoreShape,
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
 
const migrationJob = (overrides: Record<string, unknown> = {}): Record<string, unknown> => ({
  job_id: "job_migrate_0001",
  tab_id: 407,
  offered_at: 100,
  expires_at: 200,
  status: "accepted",
  provider_hosts: ["provider.example.edu"],
  ...overrides,
});

function storageWith(raw: unknown): { storage: typeof chrome.storage; data: Record<string, unknown> } {
  const data: Record<string, unknown> = { papio_state_v1: raw };
  const area = {
    async get(): Promise<Record<string, unknown>> {
      return { ...data };
    },
    async set(values: Record<string, unknown>): Promise<void> {
      Object.assign(data, values);
    },
  };
  return { storage: { session: area, local: area } as unknown as typeof chrome.storage, data };
}

test("migration accepts a clean current state and writes an explicit version on save", async () => {
  const raw = { version: MANAGED_STATE_VERSION, activeJobs: [migrationJob()] };
  const fixture = storageWith(raw);
  const backend = chromeBackend(fixture.storage);
  const loaded = await backend.load();
  expect(loaded.activeJobs[0]).toMatchObject({ job_id: "job_migrate_0001", tab_id: 407, status: "accepted" });
  await backend.save({
    ...loaded,
    pendingDelivery: { job_id: "job_migrate_0001", url: "https://secret.example/runtime.pdf", initiated_at: 1 },
    offerURLs: { "job_migrate_0001": "https://secret.example/offer" },
  });
  expect(JSON.stringify(fixture.data.papio_state_v1)).not.toContain("https://secret.example");
  expect(fixture.data.papio_state_v1).toMatchObject({ version: MANAGED_STATE_VERSION });
});

test("migration scrubs every legacy URL, claim hash, and global terms authority", () => {
  const sentinel = "https://secret.example.edu/private?token=sentinel#fragment";
  const migrated = migrateManagedState({
    activeJobs: [migrationJob({
      institution_claim_key: "sha256:legacy-claim",
      waiting_for_session_key: "v2:legacy-claim",
      nested: { provider_url: sentinel, freshURL: sentinel },
      direct_envelope: {
        allowed_origin: "https://provider.example.edu",
        path_family: "/download/{id}",
        expected_identifier: "doi:10.1000/xyz",
      },
    })],
    pendingDelivery: {
      job_id: "job_migrate_0001",
      url: sentinel,
      initiated_at: 101,
      nested: { url: sentinel, providerUrl: sentinel },
    },
    offerURLs: { "job_migrate_0001": sentinel },
    federatedLoginOwners: { "v2:legacy-claim": { jobID: "job_migrate_0001", tabID: 407, phase: "auth" } },
    termsConsent: "accept",
    global_terms_accept_authority: "legacy",
  });
  const serialized = JSON.stringify(migrated);
  expect(serialized).not.toContain(sentinel);
  expect(serialized).not.toContain("legacy-claim");
  expect(serialized).not.toContain("termsConsent");
  expect(migrated.offerURLs).toBeUndefined();
  expect(migrated.federatedLoginOwners).toBeUndefined();
  expect(migrated.pendingDelivery?.url).toBeUndefined();
  expect(migrated.activeJobs[0]?.institution_claim_key).toBeUndefined();
  expect(migrated.activeJobs[0]?.waiting_for_session_key).toBeUndefined();
  expect(migrated.activeJobs[0]?.direct_envelope?.allowed_origin).toBe("https://provider.example.edu");
});
test("legacy federated wait authority is scrubbed without stranding the job or its tab", () => {
  const migrated = migrateManagedState({
    version: 1,
    activeJobs: [
      migrationJob({
        status: "auth_pending",
        waiting_for_session: true,
        waiting_for_session_key: "v2:legacy-owner",
        waiting_since: 120,
        waiting_deadline: 999,
        waiting_reason: "federated_claim",
        parked_with_tab: true,
        parked_at: 121,
      }),
      migrationJob({
        job_id: "job_migrate_0002",
        status: "auth_pending",
        parked_with_tab: true,
        parked_at: 122,
      }),
      migrationJob({
        job_id: "job_migrate_0003",
        status: "auth_pending",
        waiting_for_session: true,
        parked_with_tab: true,
        parked_at: 123,
      }),
    ],
    federatedLoginOwners: {
      "v2:legacy-owner": { jobID: "job_migrate_0001", tabID: 407, phase: "auth" },
    },
  });
  const stranded = migrated.activeJobs.find((candidate) => candidate.job_id === "job_migrate_0001");
  expect(stranded).toMatchObject({
    job_id: "job_migrate_0001",
    tab_id: 407,
    status: "auth_pending",
    provider_hosts: ["provider.example.edu"],
  });
  const strandedRecord = stranded as Record<string, unknown> | undefined;
  expect(strandedRecord?.waiting_for_session).toBeUndefined();
  expect(strandedRecord?.waiting_for_session_key).toBeUndefined();
  expect(strandedRecord?.waiting_since).toBeUndefined();
  expect(strandedRecord?.waiting_deadline).toBeUndefined();
  expect(strandedRecord?.waiting_reason).toBeUndefined();
  expect(strandedRecord?.parked_with_tab).toBeUndefined();
  expect(strandedRecord?.parked_at).toBeUndefined();
  const ordinaryPark = migrated.activeJobs.find((candidate) => candidate.job_id === "job_migrate_0002");
  expect(ordinaryPark).toMatchObject({ tab_id: 407, parked_with_tab: true, parked_at: 122 });
  const orphan = migrated.activeJobs.find((candidate) => candidate.job_id === "job_migrate_0003");
  expect(orphan).toMatchObject({ tab_id: 407, status: "auth_pending" });
  const orphanRecord = orphan as Record<string, unknown> | undefined;
  expect(orphanRecord?.waiting_since).toBeUndefined();
  expect(orphanRecord?.waiting_deadline).toBeUndefined();
  expect(orphanRecord?.waiting_for_session).toBeUndefined();
  expect(orphanRecord?.parked_with_tab).toBeUndefined();
  expect(orphanRecord?.parked_at).toBeUndefined();
});

test("migration preserves safe identity, download, lease, daemon, origin, and UI state", () => {
  const migrated = migrateManagedState({
    version: 1,
    activeJobs: [migrationJob({
      status: "awaiting_download",
      tab_id: 999,
      needs_terms_consent: true,
      drive_epoch: { drive_attempt_id: "attempt-1", ordinal: 2, strategy: "direct", attempt_count: 1 },
      direct_envelope: { allowed_origin: "https://provider.example.edu", path_family: "/pdf/{id}", expected_identifier: "doi:10/x" },
    })],
    pendingDelivery: { job_id: "job_migrate_0001", initiated_at: 9, status: "downloaded", page_host: "provider.example.edu" },
    providerDrainLeases: { "provider.example.edu": { providerKey: "provider.example.edu", expiresAt: 999, parkedReason: "challenge" } },
    challengeCooldowns: { "provider.example.edu": 888 },
    daemonFeatures: ["institutional_materialization_v1"],
    resolverOrigins: ["https://resolver.example.edu"],
    connectionStatus: "connected",
    daemonVersion: "2.6.0",
    daemonUpdateHint: true,
    workWindowID: 77,
    handoffGroupID: 88,
  });
  expect(migrated.activeJobs[0]).toMatchObject({ job_id: "job_migrate_0001", tab_id: 999, status: "awaiting_download" });
  expect(migrated.activeJobs[0]?.needs_terms_consent).toBe(true);
  expect(migrated.pendingDelivery).toMatchObject({ job_id: "job_migrate_0001", initiated_at: 9, status: "downloaded" });
  expect(migrated.providerDrainLeases?.["provider.example.edu"]?.expiresAt).toBe(999);
  expect(migrated.challengeCooldowns?.["provider.example.edu"]).toBe(888);
  expect(migrated.daemonFeatures).toEqual(["institutional_materialization_v1"]);
  expect(migrated.resolverOrigins).toEqual(["https://resolver.example.edu"]);
  expect(migrated.connectionStatus).toBe("connected");
  expect(migrated.workWindowID).toBe(77);
  expect(migrated.handoffGroupID).toBe(88);
});

test("malformed and unknown future versions fail closed without browser-side effects", () => {
  expect(migrateManagedState(null)).toEqual(emptyStore());
  expect(migrateManagedState({ version: MANAGED_STATE_VERSION + 1, activeJobs: [migrationJob()] })).toEqual(emptyStore());
  expect(migrateManagedState({ version: "future", activeJobs: [migrationJob()] })).toEqual(emptyStore());
  expect(migrateManagedState({ activeJobs: [{ job_id: "broken" }] })).toEqual(emptyStore());
});

test("migration is deterministic and idempotent", () => {
  const raw = {
    activeJobs: [migrationJob({ nested: { provider_url: "https://secret.example/" } })],
    pendingDelivery: { job_id: "job_migrate_0001", url: "https://secret.example/pdf", initiated_at: 1 },
    resolverOrigins: ["https://resolver.example.edu/path", "https://resolver.example.edu"],
  };
  const first = migrateManagedState(raw);
  const second = migrateManagedState(first);
  expect(second).toEqual(first);
  expect(JSON.stringify(second)).not.toContain("https://secret.example");
});