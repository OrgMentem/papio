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
  reduceMaterialization,
  removeJob,
  startPendingDelivery,
  updatePendingDelivery,
  upsertJob,
  type ActiveJob,
  type MaterializationCorrelation,
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

test("a daemon upsert cannot revoke an operator-selected manual delivery target", () => {
  const selected = job({
    tab_id: -1,
    status: "awaiting_download",
    provider_hosts: [],
    manual_delivery_target: true,
  });
  const store = upsertJob(
    { ...emptyStore(), activeJobs: [selected] },
    job({ tab_id: 222, status: "accepted", access_mode: "delegated" }),
  );
  expect(store.activeJobs).toEqual([selected]);
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
  const delivery = {
    job_id: "job_00000001",
    url: "https://papers.example/paper.pdf",
    initiated_at: 7,
  };
  let store = startPendingDelivery(emptyStore(), delivery);
  expect(store.pendingDelivery).toMatchObject({
    ...delivery,
    status: "sending",
  });
  store = updatePendingDelivery(store, "other-job", { status: "downloaded" });
  expect(store.pendingDelivery?.status).toBe("sending");
  store = updatePendingDelivery(store, delivery.job_id, {
    status: "downloaded",
  });
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
      federatedLoginOwners: {
        [digest]: { jobID: "job_00000001", tabID: 100, phase: "auth" },
      },
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

const migrationJob = (
  overrides: Record<string, unknown> = {},
): Record<string, unknown> => ({
  job_id: "job_migrate_0001",
  tab_id: 407,
  offered_at: 100,
  expires_at: 200,
  status: "accepted",
  provider_hosts: ["provider.example.edu"],
  ...overrides,
});

function storageWith(raw: unknown): {
  storage: typeof chrome.storage;
  data: Record<string, unknown>;
} {
  const data: Record<string, unknown> = { papio_state_v1: raw };
  const area = {
    async get(): Promise<Record<string, unknown>> {
      return { ...data };
    },
    async set(values: Record<string, unknown>): Promise<void> {
      Object.assign(data, values);
    },
  };
  return {
    storage: { session: area, local: area } as unknown as typeof chrome.storage,
    data,
  };
}

test("migration accepts a clean current state and writes an explicit version on save", async () => {
  const raw = { version: MANAGED_STATE_VERSION, activeJobs: [migrationJob()] };
  const fixture = storageWith(raw);
  const backend = chromeBackend(fixture.storage);
  const loaded = await backend.load();
  expect(loaded.activeJobs[0]).toMatchObject({
    job_id: "job_migrate_0001",
    tab_id: 407,
    status: "accepted",
  });
  await backend.save({
    ...loaded,
    pendingDelivery: {
      job_id: "job_migrate_0001",
      url: "https://secret.example/runtime.pdf",
      initiated_at: 1,
    },
    offerURLs: { job_migrate_0001: "https://secret.example/offer" },
  });
  expect(JSON.stringify(fixture.data.papio_state_v1)).not.toContain(
    "https://secret.example",
  );
  expect(fixture.data.papio_state_v1).toMatchObject({
    version: MANAGED_STATE_VERSION,
  });
});

test("version 3 upgrades and the unique manual-delivery target survives restart", () => {
  const legacy = migrateManagedState({
    version: 3,
    activeJobs: [migrationJob()],
  });
  expect(legacy.activeJobs[0]?.job_id).toBe("job_migrate_0001");

  const selected = migrationJob({
    job_id: "job_manual_selected",
    tab_id: -1,
    status: "awaiting_download",
    manual_delivery_target: true,
  });
  expect(
    migrateManagedState({
      version: MANAGED_STATE_VERSION,
      activeJobs: [selected],
    }).activeJobs[0]?.manual_delivery_target,
  ).toBe(true);
  expect(
    migrateManagedState({
      version: MANAGED_STATE_VERSION,
      activeJobs: [
        migrationJob({
          job_id: "job_manual_open_tab",
          tab_id: 88,
          status: "awaiting_download",
          access_mode: "delegated",
          manual_delivery_target: true,
        }),
      ],
    }).activeJobs[0]?.manual_delivery_target,
  ).toBe(true);

  const ambiguous = migrateManagedState({
    version: MANAGED_STATE_VERSION,
    activeJobs: [
      selected,
      migrationJob({
        job_id: "job_manual_other",
        tab_id: -1,
        status: "awaiting_download",
        manual_delivery_target: true,
      }),
    ],
  });
  expect(
    ambiguous.activeJobs.every((job) => job.manual_delivery_target !== true),
  ).toBe(true);
  expect(
    migrateManagedState({
      version: MANAGED_STATE_VERSION,
      activeJobs: [migrationJob({ manual_delivery_target: true })],
    }).activeJobs[0]?.manual_delivery_target,
  ).toBeUndefined();
});

test("materialization migration preserves only validated institutional effect identity", () => {
  const valid = {
    job_id: "job_migrate_0001",
    candidate_id: "candidate_0001",
    materialization_kind: "browser_tab",
    candidate_expires_at: "2030-01-01T00:00:00Z",
    claim_id: "claim_000001",
    binding_id: "binding_0001",
    browser_holder_generation: 1,
    lease_until: "2030-01-01T00:05:00Z",
    phase: "navigated",
    tab_id: 407,
    institutional_request_id: "request_000001",
    expected_effect_ordinal: 6,
    effect_ordinal: 7,
  };
  const migrated = migrateManagedState({
    activeJobs: [migrationJob()],
    materializations: { job_migrate_0001: valid },
  });
  expect(migrated.materializations?.["job_migrate_0001"]).toMatchObject({
    institutional_request_id: "request_000001",
    expected_effect_ordinal: 6,
    effect_ordinal: 7,
  });

  for (const [field, value] of [
    ["institutional_request_id", "https://secret.example/request"],
    ["expected_effect_ordinal", -1],
    ["effect_ordinal", 0],
  ] as const) {
    const malformed = migrateManagedState({
      activeJobs: [migrationJob()],
      materializations: {
        job_migrate_0001: { ...valid, [field]: value },
      },
    });
    expect(malformed.materializations).toBeUndefined();
  }
});
test("migration scrubs every legacy URL, claim hash, and global terms authority", () => {
  const sentinel = "https://secret.example.edu/private?token=sentinel#fragment";
  const migrated = migrateManagedState({
    activeJobs: [
      migrationJob({
        institution_claim_key: "sha256:legacy-claim",
        waiting_for_session_key: "v2:legacy-claim",
        nested: { provider_url: sentinel, freshURL: sentinel },
        direct_envelope: {
          allowed_origin: "https://provider.example.edu",
          path_family: "/download/{id}",
          expected_identifier: "doi:10.1000/xyz",
        },
      }),
    ],
    pendingDelivery: {
      job_id: "job_migrate_0001",
      url: sentinel,
      initiated_at: 101,
      nested: { url: sentinel, providerUrl: sentinel },
    },
    offerURLs: { job_migrate_0001: sentinel },
    federatedLoginOwners: {
      "v2:legacy-claim": {
        jobID: "job_migrate_0001",
        tabID: 407,
        phase: "auth",
      },
    },
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
  expect(migrated.activeJobs[0]?.direct_envelope?.allowed_origin).toBe(
    "https://provider.example.edu",
  );
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
      "v2:legacy-owner": {
        jobID: "job_migrate_0001",
        tabID: 407,
        phase: "auth",
      },
    },
  });
  const stranded = migrated.activeJobs.find(
    (candidate) => candidate.job_id === "job_migrate_0001",
  );
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
  const ordinaryPark = migrated.activeJobs.find(
    (candidate) => candidate.job_id === "job_migrate_0002",
  );
  expect(ordinaryPark).toMatchObject({
    tab_id: 407,
    parked_with_tab: true,
    parked_at: 122,
  });
  const orphan = migrated.activeJobs.find(
    (candidate) => candidate.job_id === "job_migrate_0003",
  );
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
    activeJobs: [
      migrationJob({
        status: "awaiting_download",
        tab_id: 999,
        needs_terms_consent: true,
        drive_epoch: {
          drive_attempt_id: "attempt-1",
          ordinal: 2,
          strategy: "direct",
          attempt_count: 1,
        },
        direct_envelope: {
          allowed_origin: "https://provider.example.edu",
          path_family: "/pdf/{id}",
          expected_identifier: "doi:10/x",
        },
      }),
    ],
    pendingDelivery: {
      job_id: "job_migrate_0001",
      initiated_at: 9,
      status: "downloaded",
      page_host: "provider.example.edu",
    },
    providerDrainLeases: {
      "provider.example.edu": {
        providerKey: "provider.example.edu",
        expiresAt: 999,
        parkedReason: "challenge",
      },
    },
    challengeCooldowns: { "provider.example.edu": 888 },
    daemonFeatures: ["institutional_materialization_v1"],
    resolverOrigins: ["https://resolver.example.edu"],
    connectionStatus: "connected",
    daemonVersion: "2.6.0",
    daemonUpdateHint: true,
    workWindowID: 77,
    handoffGroupID: 88,
  });
  expect(migrated.activeJobs[0]).toMatchObject({
    job_id: "job_migrate_0001",
    tab_id: 999,
    status: "awaiting_download",
  });
  expect(migrated.activeJobs[0]?.needs_terms_consent).toBe(true);
  expect(migrated.pendingDelivery).toMatchObject({
    job_id: "job_migrate_0001",
    initiated_at: 9,
    status: "downloaded",
  });
  expect(
    migrated.providerDrainLeases?.["provider.example.edu"]?.expiresAt,
  ).toBe(999);
  expect(migrated.challengeCooldowns?.["provider.example.edu"]).toBe(888);
  expect(migrated.daemonFeatures).toEqual(["institutional_materialization_v1"]);
  expect(migrated.resolverOrigins).toEqual(["https://resolver.example.edu"]);
  expect(migrated.connectionStatus).toBe("connected");
  expect(migrated.workWindowID).toBe(77);
  expect(migrated.handoffGroupID).toBe(88);
});

test("malformed and unknown future versions fail closed without browser-side effects", () => {
  expect(migrateManagedState(null)).toEqual(emptyStore());
  expect(
    migrateManagedState({
      version: MANAGED_STATE_VERSION + 1,
      activeJobs: [migrationJob()],
    }),
  ).toEqual(emptyStore());
  expect(
    migrateManagedState({ version: "future", activeJobs: [migrationJob()] }),
  ).toEqual(emptyStore());
  expect(migrateManagedState({ activeJobs: [{ job_id: "broken" }] })).toEqual(
    emptyStore(),
  );
});

test("migration is deterministic and idempotent", () => {
  const raw = {
    activeJobs: [
      migrationJob({ nested: { provider_url: "https://secret.example/" } }),
    ],
    pendingDelivery: {
      job_id: "job_migrate_0001",
      url: "https://secret.example/pdf",
      initiated_at: 1,
    },
    resolverOrigins: [
      "https://resolver.example.edu/path",
      "https://resolver.example.edu",
    ],
  };
  const first = migrateManagedState(raw);
  const second = migrateManagedState(first);
  expect(second).toEqual(first);
  expect(JSON.stringify(second)).not.toContain("https://secret.example");
});
test("materialization reducer keeps URL-free closed transitions and rejects stale callbacks", () => {
  const correlation: MaterializationCorrelation = {
    job_id: "job_mat_0001",
    candidate_id: "cand_0001",
    materialization_kind: "browser_tab",
    candidate_expires_at: "2030-01-01T00:00:00Z",
    phase: "offered",
    tab_id: -1,
  };
  let store = upsertJob(
    emptyStore(),
    job({
      job_id: correlation.job_id,
      tab_id: -1,
      provider_hosts: ["www.jstor.org"],
    }),
  );
  store = reduceMaterialization(store, correlation.job_id, {
    type: "offer",
    correlation,
  });
  store = reduceMaterialization(store, correlation.job_id, { type: "bound" });
  expect(store.materializations?.[correlation.job_id]?.phase).toBe("offered");
  store = reduceMaterialization(store, correlation.job_id, {
    type: "claiming",
  });
  store = reduceMaterialization(store, correlation.job_id, {
    type: "claimed",
    claim_id: "claim_001",
    binding_id: "bind_0001",
    browser_holder_generation: 3,
    lease_until: "2030-01-01T00:05:00Z",
  });
  store = reduceMaterialization(store, correlation.job_id, {
    type: "scaffolded",
    tab_id: 501,
  });
  expect(findByTab(store, 501)?.job_id).toBe(correlation.job_id);
  store = reduceMaterialization(store, correlation.job_id, { type: "bound" });
  store = reduceMaterialization(store, correlation.job_id, {
    type: "route_prepared",
    expected_effect_ordinal: 0,
    institutional_request_id: "inst_req_001",
  });
  store = reduceMaterialization(store, correlation.job_id, {
    type: "route_issued",
    route_issuance_ordinal: 9,
    effect_ordinal: 1,
    institutional_request_id: "inst_req_001",
  });
  store = reduceMaterialization(store, correlation.job_id, {
    type: "route_issued",
    route_issuance_ordinal: 8,
    effect_ordinal: 1,
    institutional_request_id: "inst_req_001",
  });
  expect(
    store.materializations?.[correlation.job_id]?.route_issuance_ordinal,
  ).toBe(9);
  const persisted = JSON.stringify(store);
  expect(persisted).not.toContain("https://");
  store = reduceMaterialization(store, correlation.job_id, { type: "clear" });
  expect(store.materializations).toBeUndefined();
  expect(
    store.activeJobs.find((active) => active.job_id === correlation.job_id)
      ?.tab_id,
  ).toBe(-1);
});
test("materialization reducer supersedes candidates and marks a lost scaffold without dropping binding", () => {
  const first: MaterializationCorrelation = {
    job_id: "job_mat_0002",
    candidate_id: "cand_0001",
    materialization_kind: "browser_tab",
    candidate_expires_at: "2030-01-01T00:00:00Z",
    claim_id: "claim_0001",
    binding_id: "bind_0001",
    browser_holder_generation: 1,
    lease_until: "2030-01-01T00:05:00Z",
    phase: "navigating",
    tab_id: 501,
  };
  let store = reduceMaterialization(emptyStore(), first.job_id, {
    type: "offer",
    correlation: { ...first, phase: "offered", tab_id: -1 },
  });
  store = reduceMaterialization(store, first.job_id, { type: "claiming" });
  store = reduceMaterialization(store, first.job_id, {
    type: "claimed",
    claim_id: first.claim_id!,
    binding_id: first.binding_id!,
    browser_holder_generation: first.browser_holder_generation!,
    lease_until: first.lease_until!,
  });
  store = reduceMaterialization(store, first.job_id, {
    type: "scaffolded",
    tab_id: first.tab_id,
  });
  store = reduceMaterialization(store, first.job_id, { type: "bound" });
  store = reduceMaterialization(store, first.job_id, {
    type: "route_prepared",
    expected_effect_ordinal: 0,
    institutional_request_id: "inst_req_002",
  });
  store = reduceMaterialization(store, first.job_id, {
    type: "route_issued",
    route_issuance_ordinal: 4,
    effect_ordinal: 1,
    institutional_request_id: "inst_req_002",
  });
  store = reduceMaterialization(store, first.job_id, { type: "navigating" });
  store = reduceMaterialization(store, first.job_id, { type: "scaffold_lost" });
  expect(store.materializations?.[first.job_id]).toMatchObject({
    candidate_id: first.candidate_id,
    claim_id: first.claim_id,
    route_issuance_ordinal: 4,
    tab_id: -1,
    phase: "claimed",
  });
  const second = {
    ...first,
    candidate_id: "cand_0002",
    phase: "offered" as const,
    tab_id: -1,
  };
  store = reduceMaterialization(store, first.job_id, {
    type: "offer",
    correlation: second,
  });
  expect(store.materializations?.[first.job_id]).toEqual(second);
});

test("materialization migration preserves only opaque correlation fields", () => {
  const migrated = migrateManagedState({
    activeJobs: [migrationJob()],
    materializations: {
      job_migrate_0001: {
        job_id: "job_migrate_0001",
        candidate_id: "cand_0001",
        materialization_kind: "browser_tab",
        candidate_expires_at: "2030-01-01T00:00:00Z",
        claim_id: "claim_001",
        binding_id: "bind_0001",
        browser_holder_generation: 4,
        lease_until: "2030-01-01T00:05:00Z",
        phase: "bound",
        tab_id: 502,
        route_issuance_ordinal: 1,
        fresh_route_url: "https://secret.example/one-use",
      },
    },
  });
  expect(migrated.materializations?.["job_migrate_0001"]).toMatchObject({
    candidate_id: "cand_0001",
    binding_id: "bind_0001",
    tab_id: 502,
    phase: "bound",
  });
  expect(JSON.stringify(migrated)).not.toContain("fresh_route_url");
  expect(JSON.stringify(migrated)).not.toContain("https://secret.example");
});
