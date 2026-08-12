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
// The Go counterpart is internal/browser/dispatch_exhaustive_test.go, which
// proves the same class on the daemon's inbound dispatch.
//
// Two traps this guard must avoid, both found the hard way:
//   1. Asserting a type name appears ANYWHERE in the module proves nothing — it
//      must be scoped to the registry the sender actually consults.
//   2. Extracting call arguments with a fixed-width window silently SKIPS long
//      calls. A 400-character window missed both page_bulk_submit_v2_request
//      chunk submissions, so the guard was blind to the newest path. Argument
//      spans are therefore paren-balanced, and the parsed call count is asserted
//      against the raw occurrence count so an unparseable call fails loudly
//      instead of vanishing.
test("every requestNative expectedType is registered in CORRELATED_RESULT_TYPES", () => {
  const source = readFileSync(new URL("../src/background.ts", import.meta.url), "utf8");

  const setBlock = /const CORRELATED_RESULT_TYPES[^=]*=\s*new Set\(\[([\s\S]*?)\]\)/.exec(source);
  expect(setBlock).not.toBeNull();
  const registered = new Set(
    Array.from(setBlock![1]!.matchAll(/"([a-z0-9_]+)"/g), (match) => match[1]!),
  );
  expect(registered.size).toBeGreaterThan(10);

  const marker = "this.requestNative(";
  const expectedTypes = new Set<string>();
  let parsedCalls = 0;
  for (let at = source.indexOf(marker); at !== -1; at = source.indexOf(marker, at + 1)) {
    let depth = 0;
    let end = -1;
    for (let i = at + marker.length - 1; i < source.length; i += 1) {
      const ch = source[i];
      if (ch === "(") depth += 1;
      else if (ch === ")") {
        depth -= 1;
        if (depth === 0) {
          end = i;
          break;
        }
      }
    }
    expect(end).toBeGreaterThan(at);
    const args = source.slice(at + marker.length, end);
    const literals = Array.from(
      args.matchAll(/"([a-z0-9_]+_(?:result|response|ack))"/g),
      (match) => match[1]!,
    );
    // Exactly one reply-shaped literal per call: the expectedType argument. More
    // than one means the extraction is ambiguous and the guard cannot be trusted.
    expect(literals).toHaveLength(1);
    expectedTypes.add(literals[0]!);
    parsedCalls += 1;
  }

  const occurrences = source.split(marker).length - 1;
  expect(parsedCalls).toBe(occurrences);
  expect(parsedCalls).toBeGreaterThan(15);

  const unregistered = [...expectedTypes].filter((type) => !registered.has(type)).sort();
  expect(unregistered).toEqual([]);
});
