// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";

import { computeBadge, type BadgeState } from "../src/background";

const base: BadgeState = {
  connectionStatus: "connected",
  reauthNeeded: false,
  authBlockers: 0,
  blockedHosts: [],
  ungrantedResolvers: 0,
  triageCount: undefined,
};

test("computeBadge follows the documented operator-signal precedence", () => {
  const cases: Array<[string, Partial<BadgeState>, { text: string; color: string }]> = [
    ["disconnected", { connectionStatus: "disconnected", reauthNeeded: true, authBlockers: 2 }, { text: "!", color: "#777777" }],
    ["session elsewhere", { connectionStatus: "session_elsewhere", reauthNeeded: true, authBlockers: 2 }, { text: "!", color: "#777777" }],
    ["reauth", { reauthNeeded: true, authBlockers: 2, blockedHosts: ["provider.example.edu"], triageCount: 4 }, { text: "!", color: "#b06000" }],
    ["sign-in blockers", { authBlockers: 2, blockedHosts: ["provider.example.edu"], triageCount: 4 }, { text: "2", color: "#b06000" }],
    ["blocked hosts", { blockedHosts: ["provider.example.edu"], ungrantedResolvers: 2, triageCount: 4 }, { text: "1", color: "#b06000" }],
    ["resolver grants", { ungrantedResolvers: 2, triageCount: 4 }, { text: "2", color: "#1a73e8" }],
    ["triage", { triageCount: 4 }, { text: "4", color: "#1a73e8" }],
    ["blank", {}, { text: "", color: "#1a73e8" }],
  ];

  for (const [label, patch, expected] of cases) {
    expect(computeBadge({ ...base, ...patch }), label).toMatchObject(expected);
  }
});

test("computeBadge separates a refused session from an unreachable daemon", () => {
  // Same grey "!" shape, different story: the tooltip is the only place the
  // researcher learns the daemon is fine and the session is in another browser.
  expect(computeBadge({ ...base, connectionStatus: "session_elsewhere" }).tooltip).toBe(
    "papio: another browser holds the papio session",
  );
  expect(computeBadge({ ...base, connectionStatus: "disconnected" }).tooltip).toBe(
    "papio: daemon disconnected",
  );
});

test("computeBadge uses required turns only for a complete counts v3 projection", () => {
  const legacy = computeBadge({ ...base, triageCount: 4 });
  expect(legacy).toMatchObject({ text: "4", color: "#1a73e8", tooltip: "papio: 4 pending items" });

  const completeV3 = computeBadge({
    ...base,
    triageCount: 9,
    countsSchemaV3: true,
    requiredTurnCount: 2,
    requiredTurnsComplete: true,
  });
  expect(completeV3).toMatchObject({ text: "2", color: "#1a73e8" });

  const incompleteV3 = computeBadge({
    ...base,
    triageCount: 9,
    countsSchemaV3: true,
    requiredTurnCount: undefined,
    requiredTurnsComplete: false,
  });
  expect(incompleteV3).toMatchObject({
    text: "",
    color: "#1a73e8",
    tooltip: "Many decisions waiting — open inbox",
  });
});
test("toolbar count modes select the configured numeric tier without masking blockers", () => {
  expect(computeBadge({ ...base, toolbarCountMode: "all", triageCount: 4, requiredTurnCount: 1, countsSchemaV3: true, requiredTurnsComplete: true }))
    .toMatchObject({ text: "4", tooltip: "papio: 4 pending items" });
  expect(computeBadge({ ...base, toolbarCountMode: "off", triageCount: 4, requiredTurnCount: 1, countsSchemaV3: true, requiredTurnsComplete: true }))
    .toMatchObject({ text: "" });
  expect(computeBadge({ ...base, toolbarCountMode: "off", connectionStatus: "disconnected", triageCount: 4 }))
    .toMatchObject({ text: "!" });
  expect(computeBadge({ ...base, toolbarCountMode: "off", reauthNeeded: true, triageCount: 4 }))
    .toMatchObject({ text: "!" });
});
