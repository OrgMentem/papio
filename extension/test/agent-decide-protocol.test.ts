// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { parseBrowserMessage, parseBrowserMessageBytes, ProtocolError } from "../src/protocol";

const root = join(import.meta.dir, "..", "..", "testdata", "protocol");
interface AgentCase {
  name: string;
  base: "request" | "result" | "unavailable" | "stale" | "exhausted";
  path: string[];
  value?: unknown;
  remove?: boolean;
  valid: boolean;
}
const cases: AgentCase[] = JSON.parse(readFileSync(join(root, "agent-decide-cases.json"), "utf8"));
for (const entry of cases) {
  test(`agent decision v1: ${entry.name}`, () => {
    const name = entry.base === "request"
      ? "browser-agent-decide-request-v1.json"
      : `browser-agent-decide-result-${entry.base === "result" ? "decision" : entry.base}-v1.json`;
    const frame = JSON.parse(readFileSync(join(root, "valid", name), "utf8"));
    let target = frame;
    for (const key of entry.path.slice(0, -1)) target = target[key];
    const key = entry.path.at(-1)!;
    if (entry.remove) delete target[key];
    // defineProperty preserves __proto__ as an own JSON field.
    else Object.defineProperty(target, key, { value: entry.value, enumerable: true, configurable: true, writable: true });
    const raw = JSON.stringify(frame);
    if (entry.valid) {
      expect(parseBrowserMessage(frame)).toEqual(frame);
      expect(parseBrowserMessageBytes(raw)).toEqual(frame);
    } else {
      expect(() => parseBrowserMessage(frame)).toThrow(ProtocolError);
      expect(() => parseBrowserMessageBytes(raw)).toThrow(ProtocolError);
    }
  });
}
