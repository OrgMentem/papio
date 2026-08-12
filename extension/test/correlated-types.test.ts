import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

// `requestNative` THROWS an uncoded Error when its expectedType is absent from
// CORRELATED_RESULT_TYPES, before any connection or feature check. That throw is
// swallowed by respondToRuntimePromise, so the page shows a generic "could not
// complete that request" and NOTHING reaches the service worker console or the
// daemon — the failure is invisible from every surface an operator would check.
//
// That is exactly how the page-bulk bridge shipped broken: it landed after the
// guard (2026-08-07, ADR-0019 phase B) without registering its two reply types,
// and bulk acquisition never worked once. page_bulk_runs held six opened runs
// with zero submissions.
//
// So this parses the source rather than restating the set: a new correlated
// request cannot be added without registering its reply type.
test("every requestNative expectedType is registered in CORRELATED_RESULT_TYPES", () => {
  const source = readFileSync(new URL("../src/background.ts", import.meta.url), "utf8");

  const setBlock = /const CORRELATED_RESULT_TYPES[^=]*=\s*new Set\(\[([\s\S]*?)\]\)/.exec(source);
  expect(setBlock).not.toBeNull();
  const registered = new Set(
    Array.from(setBlock![1]!.matchAll(/"([a-z0-9_]+)"/g), (match) => match[1]!),
  );
  expect(registered.size).toBeGreaterThan(10);

  // Each call passes expectedType as the third positional argument, so take the
  // reply-shaped literal from the call's argument list.
  const expected = new Set<string>();
  for (const call of source.matchAll(/this\.requestNative\(([\s\S]{0,400}?)\)\s*;/g)) {
    for (const literal of call[1]!.matchAll(/"([a-z0-9_]+_(?:result|response|ack))"/g)) {
      expected.add(literal[1]!);
    }
  }
  expect(expected.size).toBeGreaterThan(10);

  const unregistered = [...expected].filter((type) => !registered.has(type)).sort();
  expect(unregistered).toEqual([]);
});
