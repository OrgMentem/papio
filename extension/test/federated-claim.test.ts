// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";

import {
  bindFederatedClaim,
  promoteFederatedClaim,
  releaseFederatedClaim,
  reserveFederatedClaim,
  rollbackFederatedClaim,
} from "../src/federated-claim";

test("reserve creates an unbound engaging owner and rejects another job", () => {
  const reserved = reserveFederatedClaim(undefined, "v2:claim", "job-a");
  expect(reserved?.["v2:claim"]).toEqual({ jobID: "job-a", tabID: -1, phase: "engaging" });
  expect(reserveFederatedClaim(reserved, "v2:claim", "job-b")).toBeUndefined();
});

test("same-owner no-op preserves identity", () => {
  const owners = reserveFederatedClaim(undefined, "v2:claim", "job-a");
  expect(owners).toBeDefined();
  expect(reserveFederatedClaim(owners, "v2:claim", "job-a")).toBe(owners);
  expect(bindFederatedClaim(owners, "missing", "job-a", 10)).toBeUndefined();
  expect(promoteFederatedClaim(owners, "missing", "job-a")).toBeUndefined();
});

test("bind requires the owning engaging job and preserves phase", () => {
  const owners = reserveFederatedClaim(undefined, "v2:claim", "job-a");
  expect(bindFederatedClaim(owners, "v2:claim", "job-b", 42)).toBeUndefined();
  const bound = bindFederatedClaim(owners, "v2:claim", "job-a", 42);
  expect(bound?.["v2:claim"]).toEqual({ jobID: "job-a", tabID: 42, phase: "engaging" });
  expect(bindFederatedClaim(bound, "v2:claim", "job-a", 42)).toBe(bound);
  expect(bindFederatedClaim(bound, "v2:claim", "job-a", 43)).toBeUndefined();
  const promoted = promoteFederatedClaim(bound, "v2:claim", "job-a");
  expect(bindFederatedClaim(promoted, "v2:claim", "job-a", 42)).toBe(promoted);
});

test("promote changes engaging to auth for the same owner", () => {
  const owners = reserveFederatedClaim(undefined, "v2:claim", "job-a");
  const bound = bindFederatedClaim(owners, "v2:claim", "job-a", 42);
  const promoted = promoteFederatedClaim(bound, "v2:claim", "job-a");
  expect(promoted?.["v2:claim"]).toEqual({ jobID: "job-a", tabID: 42, phase: "auth" });
  expect(promoteFederatedClaim(promoted, "v2:claim", "job-a")).toBe(promoted);
  expect(promoteFederatedClaim(promoted, "v2:claim", "job-b")).toBeUndefined();
});

test("rollback and release cannot remove another owner's claim", () => {
  const owners = reserveFederatedClaim(undefined, "v2:claim", "job-a");
  expect(releaseFederatedClaim(owners, "v2:claim", "job-b")).toBe(owners);
  expect(rollbackFederatedClaim(owners, "v2:claim", "job-b")).toBe(owners);
  const released = rollbackFederatedClaim(owners, "v2:claim", "job-a");
  expect(released).not.toBe(owners);
  expect(released?.["v2:claim"]).toBeUndefined();
});
