// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";

import {
  isSurfaceBirthRecord,
  migrateTabLedger,
  originDigestOf,
  type SurfaceBirthRecord,
} from "../src/ledger";

function mintCounter(): () => string {
  let n = 0;
  return () => {
    n += 1;
    return `binding-${n}`;
  };
}

test("v1 entry with jobID migrates to a redacted, correctly mapped record with a fresh binding id", async () => {
  const raw = {
    "7": { openedAt: 1000, url: "https://Example.com/paper/123", jobID: "job-a" },
    "8": { openedAt: 2000, url: "https://other.test/x", jobID: "job-b" },
  };
  const { ledger, review } = await migrateTabLedger(raw, mintCounter(), () => 9999);

  expect(review).toEqual([]);
  expect(Object.keys(ledger)).toEqual(["7", "8"]);

  const first = ledger["7"];
  const second = ledger["8"];
  expect(first).toBeDefined();
  expect(second).toBeDefined();
  expect(first?.binding_id).not.toBe(second?.binding_id);
  expect(first).toEqual({
    binding_id: "binding-1",
    tab_hint: 7,
    purpose: "legacy",
    browser_epoch: "pre-v2",
    extension_generation: "pre-v2",
    created_at: 1000,
    origin_digest: "100680ad546ce6a577f42f52df33b4cfdca756859e664b8d7de329b150d09ce9",
    job_id: "job-a",
  });
  expect(first?.legacy).toBeUndefined();
  expect(first?.origin_digest).toBeDefined();

  const serialized = JSON.stringify(ledger);
  expect(serialized).not.toContain("http");
  expect(serialized).not.toContain("example.com");
  expect(serialized).not.toContain("other.test");
});

test("origin digest is deterministic per origin and matches an independently computed SHA-256 hex", async () => {
  const expectedHex = async (origin: string) => {
    const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(origin));
    return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join(
      "",
    );
  };

  const a1 = await originDigestOf("https://example.com/one/path?x=1");
  const a2 = await originDigestOf("https://example.com/two");
  const b = await originDigestOf("https://different.test/");

  expect(a1).toBe(a2);
  expect(a1).not.toBe(b);
  expect(a1).toBe(await expectedHex("https://example.com"));
  expect(b).toBe(await expectedHex("https://different.test"));
});

test("origin digest lowercases mixed-case hosts and fails closed on unparseable URLs", async () => {
  const lower = await originDigestOf("https://Example.COM/path");
  const upper = await originDigestOf("https://example.com/other");
  expect(lower).toBe(upper);

  expect(await originDigestOf("not a url")).toBeUndefined();
  expect(await originDigestOf("")).toBeUndefined();
});

test("jobless v1 entry is retained as legacy and listed for review", async () => {
  const raw = {
    "42": { openedAt: 500, url: "https://provider.test/handoff" },
  };
  const { ledger, review } = await migrateTabLedger(raw, mintCounter(), () => 0);

  expect(review).toEqual(["42"]);
  expect(ledger["42"]?.legacy).toBe(true);
  expect(ledger["42"]?.job_id).toBeUndefined();
  expect(ledger["42"]?.tab_hint).toBe(42);
});

test("migrating already-migrated output is idempotent: unchanged records, no new review entries", async () => {
  const raw = {
    "1": { openedAt: 10, url: "https://a.test/x", jobID: "job-1" },
    "2": { openedAt: 20, url: "https://b.test/y" },
  };
  const first = await migrateTabLedger(raw, mintCounter(), () => 30);
  expect(first.review).toEqual(["2"]);

  const second = await migrateTabLedger(first.ledger, mintCounter(), () => 30);
  expect(second.review).toEqual([]);
  expect(second.ledger).toEqual(first.ledger);
  expect(second.ledger["1"]?.binding_id).toBe(first.ledger["1"]?.binding_id);
  expect(second.ledger["2"]?.binding_id).toBe(first.ledger["2"]?.binding_id);
});

test("malformed entries are dropped silently and whole-input garbage yields an empty ledger", async () => {
  const raw = {
    a: "not-an-object",
    b: null,
    c: { jobID: "job-only-no-openedAt-no-url" },
    d: { openedAt: 5, url: 123 },
    "9": { openedAt: 5, url: "https://ok.test/" },
  };
  const { ledger, review } = await migrateTabLedger(raw, mintCounter(), () => 0);

  expect(Object.keys(ledger)).toEqual(["9"]);
  expect(review).toEqual(["9"]);

  for (const garbage of ["garbage", 42, null, undefined, [], true]) {
    const result = await migrateTabLedger(garbage, mintCounter(), () => 0);
    expect(result.ledger).toEqual({});
    expect(result.review).toEqual([]);
  }
});

test("isSurfaceBirthRecord rejects a url-carrying record and accepts a minimal valid record", () => {
  const withURL = {
    binding_id: "b1",
    tab_hint: 1,
    purpose: "legacy",
    browser_epoch: "pre-v2",
    extension_generation: "pre-v2",
    created_at: 0,
    url: "https://example.com",
  };
  expect(isSurfaceBirthRecord(withURL)).toBe(false);

  const minimal: SurfaceBirthRecord = {
    binding_id: "b1",
    tab_hint: 1,
    purpose: "legacy",
    browser_epoch: "pre-v2",
    extension_generation: "pre-v2",
    created_at: 0,
  };
  expect(isSurfaceBirthRecord(minimal)).toBe(true);

  expect(isSurfaceBirthRecord(null)).toBe(false);
  expect(isSurfaceBirthRecord("string")).toBe(false);
  expect(isSurfaceBirthRecord({ ...minimal, tab_hint: "1" })).toBe(false);
  expect(isSurfaceBirthRecord({ ...minimal, legacy: false })).toBe(false);
  expect(isSurfaceBirthRecord({ ...minimal, legacy: true })).toBe(true);
  expect(
    isSurfaceBirthRecord({
      ...minimal,
      pending_close: {
        authorization_id: "auth",
        nonce: "nonce",
        holder_generation: 1,
        recorded_at: 0,
      },
    }),
  ).toBe(true);
  expect(
    isSurfaceBirthRecord({ ...minimal, pending_close: { authorization_id: "auth" } }),
  ).toBe(false);
});
