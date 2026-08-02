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
