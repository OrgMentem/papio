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

// Papers queued behind an institution's one sign-in are papio's work, not the
// operator's. Counting them as blockers made the badge read "13 papers waiting
// on your institution sign-in" while exactly one could proceed (live
// 2026-08-20), so a queue looked like a human ask and a long internal stall
// looked like patience.
test("computeBadge never turns a queue into a human ask", () => {
  const queuedOnly = computeBadge({ ...base, queuedAuth: 12 });
  expect(queuedOnly.text, "a queue must not put a count on the badge").toBe("");
  expect(queuedOnly.color, "a queue must not raise the ask colour").toBe("#1a73e8");
  expect(queuedOnly.tooltip).toBe("papio: connected · 12 more queued for your library");

  // One paper genuinely at a login page, twelve behind it: the count is the
  // one the operator can act on, and the rest are named as papio's own work.
  const blocked = computeBadge({ ...base, authBlockers: 1, queuedAuth: 12 });
  expect(blocked).toMatchObject({ text: "1", color: "#b06000" });
  expect(blocked.tooltip).toBe(
    "papio: 1 paper needs your institution sign-in · 12 more queued for your library",
  );

  // Plural on the ask, and no clause at all when nothing is queued.
  expect(computeBadge({ ...base, authBlockers: 2 }).tooltip).toBe(
    "papio: 2 papers need your institution sign-in",
  );

  // A required-turn projection still reports the queue beside it.
  expect(
    computeBadge({
      ...base,
      countsSchemaV3: true,
      requiredTurnsComplete: true,
      requiredTurnCount: 0,
      queuedAuth: 3,
    }).tooltip,
  ).toBe(
    "papio: 0 need you · 0 watch hits · 0 retraction notices · 3 more queued for your library",
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
    tooltip: "papio: Many decisions waiting — open inbox",
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
