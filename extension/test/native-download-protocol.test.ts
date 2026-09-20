// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { parseBrowserMessage, parseBrowserMessageBytes, ProtocolError } from "../src/protocol";

const root = join(import.meta.dir, "..", "..", "testdata", "protocol");
const cases: { name: string; base: string; path: string[]; value?: unknown; remove?: boolean; valid: boolean }[] = JSON.parse(readFileSync(join(root, "native-download-cases.json"), "utf8"));
for (const entry of cases) test(`native download contract: ${entry.name}`, () => {
  const frame = JSON.parse(readFileSync(join(root, "valid", `browser-native-download-${entry.base}.json`), "utf8"));
  let target = frame;
  for (const key of entry.path.slice(0, -1)) target = target[key];
  const key = entry.path.at(-1)!;
  if (entry.remove) delete target[key];
  else Object.defineProperty(target, key, { value: entry.value, enumerable: true, configurable: true, writable: true });
  if (entry.valid) {
    expect(parseBrowserMessage(frame)).toEqual(frame);
    expect(parseBrowserMessageBytes(JSON.stringify(frame))).toEqual(frame);
  } else {
    expect(() => parseBrowserMessage(frame)).toThrow(ProtocolError);
    expect(() => parseBrowserMessageBytes(JSON.stringify(frame))).toThrow(ProtocolError);
  }
});
