// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";

import { formatShare, parseStatsReply, type AcquisitionStats } from "../src/stats";

const validStats: AcquisitionStats = {
  generated_at: "2026-07-25T08:00:00Z",
  acquired_total: 42,
  failed_total: 14,
  handoffs_required: 9,
  access: { open_access: 18, institutional: 20, licensed_api: 3, other: 1 },
  series: [{ period_start: "2026-07-20T00:00:00Z", acquired: 6 }],
};

function stats(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return { ...validStats, ...overrides };
}

test("parses a fully valid stats reply", () => {
  const payload = validStats;
  const reply = parseStatsReply({ ok: true, stats: payload });

  expect(reply.ok).toBe(true);
  if (!reply.ok) throw new Error(`valid stats reply was rejected as ${reply.code}`);
  expect(reply.stats).toEqual(payload);
});

const invalidStatsCases: Array<[string, Record<string, unknown>]> = [
  ["missing generated_at", stats({ generated_at: undefined })],
  ["negative acquired_total", stats({ acquired_total: -1 })],
  ["NaN failed_total", stats({ failed_total: Number.NaN })],
  ["non-number handoffs_required", stats({ handoffs_required: "9" })],
  ["non-record access", stats({ access: null })],
  [
    "non-number access.open_access",
    stats({ access: { open_access: "18", institutional: 20, licensed_api: 3, other: 1 } }),
  ],
  [
    "negative access.institutional",
    stats({ access: { open_access: 18, institutional: -1, licensed_api: 3, other: 1 } }),
  ],
  [
    "access missing licensed_api",
    stats({ access: { open_access: 18, institutional: 20, other: 1 } }),
  ],
  [
    "infinite access.other",
    stats({ access: { open_access: 18, institutional: 20, licensed_api: 3, other: Number.POSITIVE_INFINITY } }),
  ],
  ["non-array series", stats({ series: {} })],
  ["non-record series bucket", stats({ series: [null] })],
  ["series bucket with non-string period_start", stats({ series: [{ period_start: 0, acquired: 6 }] })],
  [
    "series bucket with infinite acquired count",
    stats({ series: [{ period_start: "2026-07-20T00:00:00Z", acquired: Number.POSITIVE_INFINITY }] }),
  ],
];

test.each(invalidStatsCases)("rejects malformed stats: %s", (caseName, payload) => {
  const reply = parseStatsReply({ ok: true, stats: payload });

  expect(reply.ok).toBe(false);
  if (reply.ok) throw new Error(`${caseName} unexpectedly parsed as valid stats`);
  expect(reply.code).toBe("invalid_reply");
});

test.each([
  ["undefined", undefined],
  ["null", null],
  ["string", "not a reply"],
] as const)("uses the unavailable default for %s input", (_caseName, value) => {
  const reply = parseStatsReply(value);

  expect(reply.ok).toBe(false);
  if (reply.ok) throw new Error("non-record input unexpectedly parsed as valid stats");
  expect(reply.code).toBe("unavailable");
});

test.each([
  [0, 0, "—"],
  [2, 3, "67%"],
] as const)("formatShare(%s, %s) returns %s", (numerator, denominator, formatted) => {
  expect(formatShare(numerator, denominator)).toBe(formatted);
});
